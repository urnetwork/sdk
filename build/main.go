//go:build js

package main

import (
	"context"
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
	return js.ValueOf(map[string]any{

		// "getDevice": js.FuncOf(func(this js.Value, args []js.Value) any {

		// }),

		// "getProxyConfigResult": js.FuncOf(func(this js.Value, args []js.Value) any {

		// }),

		// "cancel": js.FuncOf(func(this js.Value, args []js.Value) any {

		// }),

		// "close": js.FuncOf(func(this js.Value, args []js.Value) any {

		// }),

		// "isDone": js.FuncOf(func(this js.Value, args []js.Value) any {

		// }),

	})
}

func jsProxyConfigResult(proxyConfigResult *sdk.ProxyConfigResult) js.Value {
	if proxyConfigResult == nil {
		return js.Null()
	}

	return js.ValueOf(map[string]any{
		"expirationTime":   js.ValueOf(proxyConfigResult.ExpirationTime),
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
	if jsProxyConfig.IsUndefined() {
		return nil
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
	if jsSetupDeviceCallback.IsUndefined() {
		return nil
	}

	return &simpleSetupDeviceCallback{
		setupNewDevice: func(device sdk.Device, proxyConfigResult *sdk.ProxyConfigResult) bool {
			v := jsSetupDeviceCallback.Call(
				"apply",
				js.Global(),
				jsDevice(device),
				jsProxyConfigResult(proxyConfigResult),
			)
			if v.IsUndefined() {
				return true
			}
			return v.Bool()
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
	proxyConfig := sdk.DefaultProxyConfig()
	var setupNewDeviceCallback sdk.SetupNewDeviceCallback

	if 0 < len(args) {
		proxyConfig = parseProxyConfig(args[0])

		if 1 < len(args) {
			setupNewDeviceCallback = parseSetupDeviceCallback(args[1])
		}
	}

	proxyDevice := sdk.NewProxyDeviceWithDefaults(proxyConfig, setupNewDeviceCallback)

	return jsProxyDevice(proxyDevice)
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())

	js.Global().Set("URnetworkClose", js.FuncOf(func(this js.Value, args []js.Value) any {
		cancel()
		return js.Null()
	}))

	js.Global().Set("URnetworkNewProxyDeviceWithDefaults", js.FuncOf(NewProxyDeviceWithDefaults))

	select {
	case <-ctx.Done():
	}
}
