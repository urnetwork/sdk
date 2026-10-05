//go:build js

// The page applies its own device settings again when the device behind its
// remote may not hold them (the first connect, a recreated device). It learns
// that only through this binding.
package main

import (
	"syscall/js"
	"testing"
)

func TestDeviceConfigurationChangedWasmBinding(t *testing.T) {
	_, device := newUnopenedExtensionDeviceRemote(t)
	if device.Get("addDeviceConfigurationChangedListener").Type() != js.TypeFunction {
		t.Fatal("the DeviceRemote binding has no addDeviceConfigurationChangedListener")
	}
	if !device.Call("addDeviceConfigurationChangedListener", "not a function").IsNull() {
		t.Fatal("a non-function listener was accepted")
	}

	callCount := 0
	argumentCount := -1
	listener := js.FuncOf(func(this js.Value, args []js.Value) any {
		callCount += 1
		argumentCount = len(args)
		return nil
	})
	defer listener.Release()
	unsubscribe := device.Call("addDeviceConfigurationChangedListener", listener)
	if unsubscribe.Type() != js.TypeFunction {
		t.Fatal("addDeviceConfigurationChangedListener did not return an unsubscribe function")
	}
	defer unsubscribe.Invoke()

	// the remote's event reaches the page's callback as a bare signal
	adapter := &jsDeviceConfigurationChangedListener{cb: listener.Value}
	adapter.DeviceConfigurationChanged()
	if callCount != 1 || argumentCount != 0 {
		t.Fatalf("the callback ran %d times with %d arguments, want once with none", callCount, argumentCount)
	}
}
