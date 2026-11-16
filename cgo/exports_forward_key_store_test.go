// The VPN component's forwarding key store through the real c abi (QD-844; sidecar §18.6's Setup, owner decision 197,
// SDK-M8-16): a host on the c abi, whose device settings cross as json, hands its store to
// urnet_new_device_local_with_forward_key_store as three C functions and a user_data, and the library calls them
// through the adapter the export builds. The C side is abitest's store, written as a C host writes one.
//
// The device role is dark behind DeviceLocalSettings.EnableBridge, which is Go only and has no json, so a device
// built through the abi runs no forwarding role yet and its store takes no call: the first test is the callbacks'
// round trip through the adapter, the second the export. What a device with the gate open does with its store is
// the sdk's test (device_sidecar_forward_keystore_test.go).
package main

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"github.com/urnetwork/sdk"
	"github.com/urnetwork/sdk/cgo/abitest"
)

// pointer as the C function pointer fn takes at index.
func testingAbiFunction(fn any, index int, pointer unsafe.Pointer) reflect.Value {
	functionType := reflect.TypeOf(fn).In(index)
	return reflect.NewAt(functionType.Elem(), pointer).Convert(functionType)
}

// Each of the store's three callbacks is called with what the sdk passed and answers what the C function returned:
// a put's name and value reach the host and its status comes back, a get's json answer is decoded and its buffer
// freed by the library, and a delete removes. A get that answers NULL, or text that is no json, reads as no answer,
// which the device takes as Failed, never as Absent.
func TestForwardKeyStoreCallbacksRoundTripThroughTheAbi(t *testing.T) {
	store := abitest.NewForwardKeyStore()
	defer store.Free()
	// the adapter the export builds over the host's three functions and its user_data, field for field
	adapter := &cAdapterSidecarForwardKeyStore{userData: store.UserData()}
	fields := reflect.ValueOf(adapter).Elem()
	for name, function := range map[string]unsafe.Pointer{
		"cbPutSecret":    store.PutSecretFunction(),
		"cbGetSecret":    store.GetSecretFunction(),
		"cbDeleteSecret": store.DeleteSecretFunction(),
	} {
		*(*unsafe.Pointer)(unsafe.Pointer(fields.FieldByName(name).UnsafeAddr())) = function
	}
	var keyStore sdk.SidecarForwardKeyStore = adapter

	if status := keyStore.PutSecret("forward.root", "cm9vdCBrZXk="); status != sdk.SidecarForwardKeyStoreOk {
		t.Fatalf("put answered %d", status)
	}
	if value, ok := store.Value("forward.root"); !ok || value != "cm9vdCBrZXk=" {
		t.Fatalf("the host holds %q (%t) under the name put", value, ok)
	}
	secret := keyStore.GetSecret("forward.root")
	if secret == nil || secret.Status != sdk.SidecarForwardSecretPresent || secret.ValueBase64 != "cm9vdCBrZXk=" {
		t.Fatalf("get answered %+v", secret)
	}
	if absent := keyStore.GetSecret("forward.other"); absent == nil || absent.Status != sdk.SidecarForwardSecretAbsent || absent.ValueBase64 != "" {
		t.Errorf("get of a name never put answered %+v", absent)
	}

	// a status the host answers is the status the sdk reads: this store refuses a value it has no room for
	if status := keyStore.PutSecret("forward.large", strings.Repeat("A", 2048)); status != sdk.SidecarForwardKeyStoreFailed {
		t.Errorf("a put the host refused answered %d", status)
	}

	if status := keyStore.DeleteSecret("forward.root"); status != sdk.SidecarForwardKeyStoreOk {
		t.Errorf("delete answered %d", status)
	}
	if deleted := keyStore.GetSecret("forward.root"); deleted == nil || deleted.Status != sdk.SidecarForwardSecretAbsent {
		t.Errorf("get after the delete answered %+v", deleted)
	}

	store.SetGetAnswersNull(true)
	if secret := keyStore.GetSecret("forward.root"); secret != nil {
		t.Errorf("a NULL answer read as %+v", secret)
	}
	store.SetGetAnswersNull(false)
	store.SetGetAnswersGarbage(true)
	if secret := keyStore.GetSecret("forward.root"); secret != nil {
		t.Errorf("an answer that is no json read as %+v", secret)
	}

	putCount, getCount, deleteCount := store.CallCounts()
	if putCount != 2 || getCount != 5 || deleteCount != 1 {
		t.Errorf("the host took %d puts, %d gets and %d deletes", putCount, getCount, deleteCount)
	}
}

// A c host builds a device with its forwarding key store: the settings json, with the component's directory in it,
// then the three functions and their user_data. The device builds with the store, with no store (NULL functions),
// and from a NULL settings json; json that does not decode builds none. The device role's gate has no json, so no
// device built here runs a forwarding role, and the store takes no call.
func TestNewDeviceLocalWithForwardKeyStoreThroughTheAbi(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	strategySettings := testingOfflineClientStrategySettings()
	networkSpace := sdk.NewNetworkSpaceWithUrls(ctx, "https://192.0.2.1", "wss://192.0.2.1", strategySettings)
	defer networkSpace.Close()
	// the devices never reach an api: the test is the construction
	if err := networkSpace.GetApi().CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	spaceHandle := newHandle(networkSpace)
	defer handleRelease(spaceHandle)

	store := abitest.NewForwardKeyStore()
	// freed after the devices' cleanups, which run first
	t.Cleanup(store.Free)

	settingsBytes, err := json.Marshal(map[string]any{"ForwardStateDir": t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	settingsJson := string(settingsBytes)

	// the device's handle and the error the export set
	build := func(settingsJson *string, withStore bool) (uint64, string) {
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
		var deleteSecret, getSecret, putSecret, userData any
		if withStore {
			deleteSecret = testingAbiFunction(urnet_new_device_local_with_forward_key_store, 7, store.DeleteSecretFunction())
			getSecret = testingAbiFunction(urnet_new_device_local_with_forward_key_store, 8, store.GetSecretFunction())
			putSecret = testingAbiFunction(urnet_new_device_local_with_forward_key_store, 9, store.PutSecretFunction())
			userData = store.UserData()
		}
		outError := socketABIOut(urnet_new_device_local_with_forward_key_store, 11)
		deviceHandle := socketABICall(
			urnet_new_device_local_with_forward_key_store,
			spaceHandle,
			byJwt,
			empty,
			empty,
			empty,
			instanceId,
			settings,
			deleteSecret,
			getSecret,
			putSecret,
			userData,
			outError,
		)[0].Uint()
		if deviceHandle != 0 {
			t.Cleanup(func() {
				socketABICall(urnet_device_close, deviceHandle)
				handleRelease(deviceHandle)
			})
		}
		return deviceHandle, socketABIError(outError)
	}

	for name, arguments := range map[string]struct {
		settingsJson *string
		withStore    bool
	}{
		"the store and the component's directory": {&settingsJson, true},
		"the store and a NULL settings json":      {nil, true},
		"no store":                                {&settingsJson, false},
	} {
		deviceHandle, buildError := build(arguments.settingsJson, arguments.withStore)
		if deviceHandle == 0 || buildError != "" {
			t.Errorf("%s: no device was built: %q", name, buildError)
			continue
		}
		if _, ok := resolveHandle[*sdk.DeviceLocal](deviceHandle, "the test"); !ok {
			t.Errorf("%s: the handle is no local device", name)
		}
	}

	notJson := "{"
	if deviceHandle, _ := build(&notJson, true); deviceHandle != 0 {
		t.Error("settings json that does not decode built a device")
	}

	if putCount, getCount, deleteCount := store.CallCounts(); putCount != 0 || getCount != 0 || deleteCount != 0 {
		t.Errorf("behind the device role's gate the store took %d puts, %d gets and %d deletes", putCount, getCount, deleteCount)
	}
}
