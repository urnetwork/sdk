//go:build js

package main

import (
	"context"
	"encoding/json"
	"syscall/js"

	"github.com/urnetwork/sdk"
)

// js conversion notes:
// - `time.Time` is converted as int unix epoch milliseconds
// - `time.Duration` is converted as int milliseconds

func jsProxyDevice(proxyDevice *sdk.ProxyDevice) js.Value {
	return js.ValueOf(map[string]any{
		"getDevice": js.FuncOf(func(this js.Value, args []js.Value) any {
			return jsDevice(proxyDevice.GetDevice())
		}),

		"getProxyConfigResult": js.FuncOf(func(this js.Value, args []js.Value) any {
			return jsProxyConfigResult(proxyDevice.GetProxyConfigResult())
		}),

		"cancel": js.FuncOf(func(this js.Value, args []js.Value) any {
			proxyDevice.Cancel()
			return js.Null()
		}),

		"close": js.FuncOf(func(this js.Value, args []js.Value) any {
			proxyDevice.Close()
			return js.Null()
		}),

		"isDone": js.FuncOf(func(this js.Value, args []js.Value) any {
			return js.ValueOf(proxyDevice.GetDone())
		}),
	})
}

func jsDevice(device sdk.Device) js.Value {
	if device == nil {
		return js.Null()
	}
	// Device methods can be added here as needed
	return js.ValueOf(map[string]any{})
}

func jsProxyConfigResult(proxyConfigResult *sdk.ProxyConfigResult) js.Value {
	if proxyConfigResult == nil {
		return js.Null()
	}

	return js.ValueOf(map[string]any{
		// `time.Time` is converted as int unix epoch milliseconds (see the
		// conversion notes at the top of this file). `js.ValueOf` on the
		// raw `time.Time` struct would panic at runtime.
		"expirationTime":   js.ValueOf(proxyConfigResult.ExpirationTime.UnixMilli()),
		"keepaliveSeconds": js.ValueOf(proxyConfigResult.KeepaliveSeconds),
		"httpProxyUrl":     js.ValueOf(proxyConfigResult.HttpProxyUrl),
		"socksProxyUrl":    js.ValueOf(proxyConfigResult.SocksProxyUrl),
		"httpProxyAuth":    jsProxyAuthResult(proxyConfigResult.HttpProxyAuth),
		"socksProxyAuth":   jsProxyAuthResult(proxyConfigResult.SocksProxyAuth),
	})
}

func jsProxyAuthResult(proxyAuthResult *sdk.ProxyAuthResult) js.Value {
	if proxyAuthResult == nil {
		return js.Null()
	}

	return js.ValueOf(map[string]any{
		"username": js.ValueOf(proxyAuthResult.Username),
		"password": js.ValueOf(proxyAuthResult.Password),
	})
}

func parseProxyConfig(jsProxyConfig js.Value) *sdk.ProxyConfig {
	if jsProxyConfig.IsUndefined() || jsProxyConfig.IsNull() {
		return sdk.DefaultProxyConfig()
	}

	proxyConfig := &sdk.ProxyConfig{}

	if v := jsProxyConfig.Get("lockCallerIp"); !v.IsUndefined() {
		proxyConfig.LockCallerIp = v.Bool()
	}
	if v := jsProxyConfig.Get("lockIpList"); !v.IsUndefined() {
		for i := 0; i < v.Length(); i += 1 {
			proxyConfig.LockIpList = append(proxyConfig.LockIpList, v.Index(i).String())
		}
	}
	if v := jsProxyConfig.Get("enableSocks"); !v.IsUndefined() {
		proxyConfig.EnableSocks = v.Bool()
	}
	if v := jsProxyConfig.Get("enableHttp"); !v.IsUndefined() {
		proxyConfig.EnableHttp = v.Bool()
	}
	if v := jsProxyConfig.Get("httpRequireAuth"); !v.IsUndefined() {
		proxyConfig.HttpRequireAuth = v.Bool()
	}

	return proxyConfig
}

func parseSetupDeviceCallback(jsSetupDeviceCallback js.Value) sdk.SetupNewDeviceCallback {
	if jsSetupDeviceCallback.IsUndefined() || jsSetupDeviceCallback.IsNull() {
		return nil
	}

	return &simpleSetupDeviceCallback{
		setupNewDevice: func(device sdk.Device, proxyConfigResult *sdk.ProxyConfigResult) bool {
			result := jsSetupDeviceCallback.Invoke(
				jsDevice(device),
				jsProxyConfigResult(proxyConfigResult),
			)
			if result.IsUndefined() || result.IsNull() {
				return true
			}
			return result.Bool()
		},
	}
}

type simpleSetupDeviceCallback struct {
	setupNewDevice func(device sdk.Device, proxyConfigResult *sdk.ProxyConfigResult) bool
}

func (self *simpleSetupDeviceCallback) SetupNewDevice(device sdk.Device, proxyConfigResult *sdk.ProxyConfigResult) bool {
	return self.setupNewDevice(device, proxyConfigResult)
}

func NewProxyDeviceWithDefaults(this js.Value, args []js.Value) any {
	var proxyConfig *sdk.ProxyConfig
	var setupNewDeviceCallback sdk.SetupNewDeviceCallback

	if len(args) > 0 && !args[0].IsNull() && !args[0].IsUndefined() {
		proxyConfig = parseProxyConfig(args[0])
	} else {
		proxyConfig = sdk.DefaultProxyConfig()
	}

	if len(args) > 1 && !args[1].IsNull() && !args[1].IsUndefined() {
		setupNewDeviceCallback = parseSetupDeviceCallback(args[1])
	}

	proxyDevice := sdk.NewProxyDeviceWithDefaults(proxyConfig, setupNewDeviceCallback)

	return jsProxyDevice(proxyDevice)
}

// FilteredLocationsFromResult groups and orders a raw
// /network/find-provider-locations (or /network/provider-locations) response
// the way every app's location chooser renders it: best matches, promoted,
// then countries / regions / cities / devices, each ordered by provider count
// descending and then by name.
//
// This is the SAME sdk function the native apps run their own api results
// through (apple's UrApiService calls GetFilteredLocationsFromResult), exposed
// so a caller that has a result but no device — the web chooser before the
// device plane attaches — orders it identically instead of rendering whatever
// order the server replied in.
//
// args: (resultJson string, filter string). Returns null when the json cannot
// be read, so the caller can fall back.
func FilteredLocationsFromResult(this js.Value, args []js.Value) any {
	if len(args) < 1 || args[0].Type() != js.TypeString {
		return js.Null()
	}
	filter := ""
	if 1 < len(args) && args[1].Type() == js.TypeString {
		filter = args[1].String()
	}
	var result sdk.FindLocationsResult
	if err := json.Unmarshal([]byte(args[0].String()), &result); err != nil {
		return js.Null()
	}
	return jsFilteredLocations(sdk.GetFilteredLocationsFromResult(&result, filter))
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())

	js.Global().Set("URnetworkClose", js.FuncOf(func(this js.Value, args []js.Value) any {
		cancel()
		return js.Null()
	}))

	js.Global().Set("URnetworkNewProxyDeviceWithDefaults", js.FuncOf(NewProxyDeviceWithDefaults))
	js.Global().Set("URnetworkNewPlatformDeviceRemote", js.FuncOf(NewPlatformDeviceRemote))
	js.Global().Set("URnetworkFilteredLocationsFromResult", js.FuncOf(FilteredLocationsFromResult))

	select {
	case <-ctx.Done():
	}
}
