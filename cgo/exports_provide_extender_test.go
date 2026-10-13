// The provider extender's device controls through the real c abi (EXTENDER.md
// G1, F3): a third-party provider builds its device with the host-facing
// constructor, on a space that keeps no local state as the headless examples
// do, and reads and writes the setting through the device exports.
package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/sdk/v2026"
)

// A synthetic unsigned client credential naming one new client of one new
// network. Construction reads only the client id from it.
func testingProvideExtenderClientJwt() string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(
		`{"client_id":"%s","network_id":"%s"}`,
		connect.NewId(),
		connect.NewId(),
	)))
	return fmt.Sprintf("%s.%s.", header, payload)
}

// The constructor carries the device default to the device: on reads on, off
// reads off until the setting is set, and on a space with no local state the
// value set is what the device then reads.
func TestProvideExtenderDeviceControlsThroughTheAbi(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	strategySettings := connect.DefaultClientStrategySettings()
	strategySettings.Log = connect.NewNoopLogger()
	// an ip literal derives no extender network, so the space starts no
	// extender client or node in this process
	networkSpace := sdk.NewNetworkSpaceWithUrls(ctx, "https://192.0.2.1", "wss://192.0.2.1", strategySettings)
	defer networkSpace.Close()
	// the devices never reach an api: the test is the construction and the
	// setting
	if err := networkSpace.GetApi().CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	spaceHandle := newHandle(networkSpace)
	defer handleRelease(spaceHandle)

	newDevice := func(provideExtenderEnabled bool, defaultProvideExtender bool) uint64 {
		t.Helper()
		byJwt := cString(testingProvideExtenderClientJwt())
		defer cStringFree(byJwt)
		empty := cString("")
		defer cStringFree(empty)
		instanceId := cString(sdk.NewId().String())
		defer cStringFree(instanceId)
		outError := socketABIOut(urnet_new_device_local_with_provide_extender, 10)
		deviceHandle := socketABICall(
			urnet_new_device_local_with_provide_extender,
			spaceHandle,
			byJwt,
			empty,
			empty,
			empty,
			instanceId,
			false,
			uint64(0),
			provideExtenderEnabled,
			defaultProvideExtender,
			outError,
		)[0].Uint()
		if err := socketABIError(outError); err != "" || deviceHandle == 0 {
			t.Fatalf("the device was not built: %q", err)
		}
		t.Cleanup(func() {
			socketABICall(urnet_device_close, deviceHandle)
			handleRelease(deviceHandle)
		})
		return deviceHandle
	}
	getProvideExtender := func(deviceHandle uint64) bool {
		return socketABICall(urnet_device_get_provide_extender, deviceHandle)[0].Bool()
	}
	setProvideExtender := func(deviceHandle uint64, provideExtender bool) {
		socketABICall(urnet_device_set_provide_extender, deviceHandle, provideExtender)
	}

	onDevice := newDevice(true, true)
	if !getProvideExtender(onDevice) {
		t.Fatal("the setting read off with the device default on")
	}
	setProvideExtender(onDevice, false)
	if getProvideExtender(onDevice) {
		t.Fatal("turning the setting off did not take on a space with no local state")
	}

	offDevice := newDevice(true, false)
	if getProvideExtender(offDevice) {
		t.Fatal("the setting read on with the device default off")
	}
	setProvideExtender(offDevice, true)
	if !getProvideExtender(offDevice) {
		t.Fatal("turning the setting on did not take on a space with no local state")
	}
}
