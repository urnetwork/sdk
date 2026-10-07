// The device settings json through the real c abi: a c host reads the
// defaults, edits the provider extender controls (EXTENDER.md G1, F3) and
// builds a device from the edited json with urnet_new_device_local, on a space
// that keeps no local state. A json that names only some fields, or none,
// keeps the defaults for the rest.
package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
)

func TestDeviceLocalSettingsJsonThroughTheAbi(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	strategySettings := connect.DefaultClientStrategySettings()
	strategySettings.Log = connect.NewNoopLogger()
	// an ip literal derives no extender network, so the space starts no
	// extender client or node, and a role asked to run there fails to start
	// before it binds anything
	networkSpace := sdk.NewNetworkSpaceWithUrls(ctx, "https://192.0.2.1", "wss://192.0.2.1", strategySettings)
	defer networkSpace.Close()
	// the devices never reach an api: the test is the settings
	if err := networkSpace.GetApi().CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	spaceHandle := newHandle(networkSpace)
	defer handleRelease(spaceHandle)

	// the defaults as a c host reads them
	defaultsPointer := socketABICall(urnet_default_device_local_settings)[0]
	if defaultsPointer.IsNil() {
		t.Fatal("urnet_default_device_local_settings answered NULL")
	}
	defaultsJson := goStringAt(defaultsPointer.UnsafePointer())
	socketABICall(urnet_free_string, defaultsPointer)
	readDefaults := func() map[string]any {
		values := map[string]any{}
		if err := json.Unmarshal([]byte(defaultsJson), &values); err != nil {
			t.Fatalf("the default settings json does not decode: %v", err)
		}
		return values
	}
	defaults := readDefaults()
	for _, key := range []string{"ProvideExtenderEnabled", "DefaultProvideExtender", "AllowProvider"} {
		if defaults[key] != true {
			t.Fatalf("the default settings json has %s = %v, expected true", key, defaults[key])
		}
	}
	// every app binds the extender's dns carrier on 4053 alone (L2)
	if value, ok := defaults["ProvideExtenderDnsPrivilegedPort"]; !ok || value != false {
		t.Fatalf("the default settings json has ProvideExtenderDnsPrivilegedPort = %v (present %t), expected false",
			value, ok)
	}

	// builds a device from settings json, nil for a NULL json
	newDevice := func(settingsJson *string) uint64 {
		t.Helper()
		byJwt := cString(testingProvideExtenderClientJwt())
		defer cStringFree(byJwt)
		empty := cString("")
		defer cStringFree(empty)
		instanceId := cString(sdk.NewId().String())
		defer cStringFree(instanceId)
		var settings any
		if settingsJson != nil {
			settingsC := cString(*settingsJson)
			defer cStringFree(settingsC)
			settings = settingsC
		}
		outError := socketABIOut(urnet_new_device_local, 7)
		deviceHandle := socketABICall(
			urnet_new_device_local,
			spaceHandle,
			byJwt,
			empty,
			empty,
			empty,
			instanceId,
			settings,
			outError,
		)[0].Uint()
		if err := socketABIError(outError); err != "" || deviceHandle == 0 {
			t.Fatalf("the device was not built from %v: %q", settingsJson, err)
		}
		t.Cleanup(func() {
			socketABICall(urnet_device_close, deviceHandle)
			handleRelease(deviceHandle)
		})
		return deviceHandle
	}
	edit := func(values map[string]any) *string {
		t.Helper()
		settingsBytes, err := json.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}
		settingsJson := string(settingsBytes)
		return &settingsJson
	}
	getProvideExtender := func(deviceHandle uint64) bool {
		return socketABICall(urnet_device_get_provide_extender, deviceHandle)[0].Bool()
	}
	// the state the device reports for the role once it provides
	providingState := func(deviceHandle uint64) (string, string) {
		t.Helper()
		socketABICall(urnet_device_set_provide_mode, deviceHandle, int64(sdk.ProvideModePublic))
		statusPointer := socketABICall(urnet_device_get_extender_provide_status, deviceHandle)[0]
		if statusPointer.IsNil() {
			t.Fatal("the device answered no provider extender status")
		}
		statusJson := goStringAt(statusPointer.UnsafePointer())
		socketABICall(urnet_free_string, statusPointer)
		status := &sdk.ExtenderProvideStatus{}
		if err := json.Unmarshal([]byte(statusJson), status); err != nil {
			t.Fatal(err)
		}
		return status.State, status.ErrorCase
	}

	// the defaults unchanged: the role is asked to run once providing, and on
	// this space it can only report that it could not start
	defaultDevice := newDevice(&defaultsJson)
	if !getProvideExtender(defaultDevice) {
		t.Fatal("the setting read off from the default settings json")
	}
	if state, errorCase := providingState(defaultDevice); state != sdk.ExtenderProvideStateError ||
		errorCase != sdk.ExtenderProvideErrorStart {
		t.Fatalf("state = %q, %q from the default settings json, expected the start error", state, errorCase)
	}

	// the device default edited off
	defaultOff := readDefaults()
	defaultOff["DefaultProvideExtender"] = false
	defaultOffDevice := newDevice(edit(defaultOff))
	if getProvideExtender(defaultOffDevice) {
		t.Fatal("the setting read on with the device default edited off")
	}

	// the hard switch edited off: the setting stays on, and the role is not
	// asked for while the device provides
	switchOff := readDefaults()
	switchOff["ProvideExtenderEnabled"] = false
	switchOffDevice := newDevice(edit(switchOff))
	if !getProvideExtender(switchOffDevice) {
		t.Fatal("the setting read off with only the hard switch edited off")
	}
	if state, _ := providingState(switchOffDevice); state != sdk.ExtenderProvideStateNotProviding {
		t.Fatalf("state = %q with the hard switch edited off, expected not providing", state)
	}

	// the 53 opt-in edited on, as a host that runs where it can take 53
	// would: the device builds, and the setting stays independent of the
	// role's other controls
	dnsPrivilegedPort := readDefaults()
	dnsPrivilegedPort["ProvideExtenderDnsPrivilegedPort"] = true
	dnsPrivilegedPortDevice := newDevice(edit(dnsPrivilegedPort))
	if !getProvideExtender(dnsPrivilegedPortDevice) {
		t.Fatal("the setting read off with only the 53 opt-in edited on")
	}

	// a json that names one field keeps the defaults for the rest
	partialDevice := newDevice(edit(map[string]any{"DefaultProvideExtender": false}))
	if getProvideExtender(partialDevice) {
		t.Fatal("the setting read on from a partial json that turned the default off")
	}

	// and a NULL json is the defaults
	nullDevice := newDevice(nil)
	if !getProvideExtender(nullDevice) {
		t.Fatal("the setting read off from a NULL settings json")
	}
}
