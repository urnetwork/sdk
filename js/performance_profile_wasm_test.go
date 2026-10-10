//go:build js

// The connect options a page writes must reach the hosted device as the same
// profile the native apps send (android ConnectViewModel.updatePerformanceProfile,
// apple DeviceManager.createPerformanceProfile), or the web's Fixed IP toggle
// does nothing.
package main

import (
	"reflect"
	"syscall/js"
	"testing"

	"github.com/urnetwork/sdk/v2026"
)

// A remote over an extension transport that never opens, with its binding:
// the remote's rpc stays down, so a set is held as pending state for the next
// sync, as a browser remote does before its first sync and between syncs. The
// transport's callbacks are never released: the remote's run loop may dial
// again while it closes.
func newUnopenedExtensionDeviceRemote(t *testing.T) (*sdk.DeviceRemote, js.Value) {
	t.Helper()
	noop := js.FuncOf(func(this js.Value, args []js.Value) any { return nil })
	connection := js.Global().Get("Object").New()
	connection.Set("send", noop)
	connection.Set("close", noop)
	open := js.FuncOf(func(this js.Value, args []js.Value) any { return connection })
	transport := js.Global().Get("Object").New()
	transport.Set("open", open)

	networkSpace := sdk.NewUrlsNetworkSpace("https://api.invalid", "wss://connect.invalid")
	remote, err := sdk.NewExtensionDeviceRemote(networkSpace, "", sdk.NewId(), transport)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remote.Close)
	return remote, jsDeviceRemote(remote)
}

// The binding has the three methods, and a set queues the profile a page
// sends (the web's Fixed IP is a window of exactly one exit) unless the multi
// client would refuse it; null queues the auto profile.
func TestPerformanceProfileWasmSetQueuesTheFixedIpProfile(t *testing.T) {
	remote, device := newUnopenedExtensionDeviceRemote(t)
	for _, method := range []string{"getPerformanceProfile", "setPerformanceProfile", "addPerformanceProfileChangeListener"} {
		if device.Get(method).Type() != js.TypeFunction {
			t.Fatalf("the DeviceRemote binding has no %s", method)
		}
	}
	if !device.Call("getPerformanceProfile").IsNull() {
		t.Fatal("a fresh remote reported a profile")
	}

	// Web with Fixed IP: a window of exactly one exit
	device.Call("setPerformanceProfile", map[string]any{
		"windowType":            "quality",
		"windowSize":            map[string]any{"windowSizeMin": 1, "windowSizeMax": 1},
		"allowDirect":           false,
		"postQuantumEncryption": true,
	})
	want := &sdk.PerformanceProfile{
		WindowType:            sdk.WindowTypeQuality,
		WindowSize:            &sdk.WindowSizeSettings{WindowSizeMin: 1, WindowSizeMax: 1},
		PostQuantumEncryption: true,
	}
	if got := remote.GetPerformanceProfile(); !reflect.DeepEqual(got, want) {
		t.Fatalf("queued profile = %+v, want %+v", got, want)
	}
	got := device.Call("getPerformanceProfile")
	if got.Get("windowType").String() != "quality" ||
		got.Get("windowSize").Get("windowSizeMin").Int() != 1 ||
		got.Get("windowSize").Get("windowSizeMax").Int() != 1 ||
		got.Get("allowDirect").Bool() || !got.Get("postQuantumEncryption").Bool() {
		t.Fatal("getPerformanceProfile does not report the profile that was set")
	}

	// a window the multi client would refuse never replaces it
	device.Call("setPerformanceProfile", map[string]any{
		"windowType": "speed",
		"windowSize": map[string]any{"windowSizeMin": 4, "windowSizeMax": 2},
	})
	device.Call("setPerformanceProfile", "speed")
	if got := remote.GetPerformanceProfile(); !reflect.DeepEqual(got, want) {
		t.Fatalf("an invalid profile replaced the queued one: %+v", got)
	}

	// null is the sdk's auto profile
	device.Call("setPerformanceProfile", nil)
	if got := remote.GetPerformanceProfile(); got != nil {
		t.Fatalf("null queued %+v, want the nil (auto) profile", got)
	}

	listener := js.FuncOf(func(this js.Value, args []js.Value) any { return nil })
	defer listener.Release()
	unsubscribe := device.Call("addPerformanceProfileChangeListener", listener)
	if unsubscribe.Type() != js.TypeFunction {
		t.Fatal("addPerformanceProfileChangeListener did not return an unsubscribe function")
	}
	unsubscribe.Invoke()
}

// A profile rendered for a page parses back to the same profile, and nil
// renders as null.
func TestPerformanceProfileWasmRoundTrip(t *testing.T) {
	if !jsPerformanceProfile(nil).IsNull() {
		t.Fatal("a nil profile is not null")
	}
	auto := jsPerformanceProfile(&sdk.PerformanceProfile{WindowType: sdk.WindowTypeAuto, AllowDirect: true})
	if auto.Get("windowType").String() != "auto" || !auto.Get("windowSize").IsNull() || !auto.Get("allowDirect").Bool() {
		t.Fatal("an auto profile did not carry its flags without a window size")
	}

	for _, profile := range []*sdk.PerformanceProfile{
		{WindowType: sdk.WindowTypeAuto},
		{WindowType: sdk.WindowTypeSpeed, WindowSize: &sdk.WindowSizeSettings{WindowSizeMin: 2, WindowSizeMax: 4}},
		{
			WindowType: sdk.WindowTypeQuality,
			WindowSize: &sdk.WindowSizeSettings{
				WindowSizeMin:            1,
				WindowSizeMinP2pOnly:     1,
				WindowSizeMax:            1,
				WindowSizeHardMax:        3,
				WindowSizeReconnectScale: 0.5,
				KeepHealthiestCount:      1,
				Ulimit:                   64,
			},
			AllowDirect:           true,
			PostQuantumEncryption: true,
		},
	} {
		parsed, ok := parsePerformanceProfile(jsPerformanceProfile(profile))
		if !ok || !reflect.DeepEqual(parsed, profile) {
			t.Fatalf("round trip = %+v (ok %t), want %+v", parsed, ok, profile)
		}
	}
}
