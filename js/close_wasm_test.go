//go:build js

// Browser teardown must let the JavaScript event loop deliver the events that
// release owned workers. These barriers need no network or scheduling sleeps.
package main

import (
	"syscall/js"
	"testing"

	"github.com/urnetwork/sdk/v2026"
)

// Proves the completion contract without blocking a broken synchronous bridge.
func TestCloseWasmReturnsOneCompletionPromise(t *testing.T) {
	closed := 0
	closeController := jsViewControllerClose(func() { closed++ })
	defer closeController.Release()
	completion := closeController.Invoke()
	if completion.Type() != js.TypeObject || completion.Get("then").Type() != js.TypeFunction {
		t.Fatal("close ran synchronously instead of returning its completion promise")
	}
	if !completion.Equal(closeController.Invoke()) {
		t.Fatal("repeated close did not return the same completion")
	}
	if _, err := awaitSocketPromise(t, completion); err != nil {
		t.Fatal(err)
	}
	if closed != 1 {
		t.Fatalf("owner closed %d times", closed)
	}
}

// Forces teardown to wait for a JavaScript microtask, just as an aborted fetch
// or websocket close must return to the browser before its worker can finish.
func TestCloseWasmYieldsForBrowserCompletion(t *testing.T) {
	finished := false
	closeController := jsViewControllerClose(func() {
		event := make(chan struct{})
		callback := js.FuncOf(func(js.Value, []js.Value) any { close(event); return nil })
		defer callback.Release()
		js.Global().Get("Promise").Call("resolve").Call("then", callback)
		<-event
		finished = true
	})
	defer closeController.Release()
	if _, err := awaitSocketPromise(t, closeController.Invoke()); err != nil {
		t.Fatal(err)
	}
	if !finished {
		t.Fatal("completion resolved before the owned worker joined")
	}
}

// A non-remote device is an adjacent close entry point with the same ownership.
type closeWasmDevice struct {
	sdk.Device
	closeDevice func()
}

// Delegates to the test barrier rather than a real network device.
func (self *closeWasmDevice) Close() { self.closeDevice() }

// Ensures the plain-device wrapper does not bypass asynchronous teardown.
func TestDeviceCloseWasmYieldsForBrowserCompletion(t *testing.T) {
	event := make(chan struct{})
	device := jsDevice(&closeWasmDevice{closeDevice: func() { <-event }})
	callback := js.FuncOf(func(js.Value, []js.Value) any { close(event); return nil })
	defer callback.Release()
	js.Global().Get("Promise").Call("resolve").Call("then", callback)
	if _, err := awaitSocketPromise(t, device.Call("close")); err != nil {
		t.Fatal(err)
	}
}

// Teardown must not serialize a socket join ahead of the owner cancellation
// that releases it, and completion must include the child resources too.
func TestCloseWasmJoinsOwnerAndDependentHandles(t *testing.T) {
	ownerClosed := make(chan struct{})
	resourceStarted := make(chan struct{})
	resourceClosed := false
	closeController := jsViewControllerClose(func() {
		<-resourceStarted
		close(ownerClosed)
	}, func() {
		close(resourceStarted)
		<-ownerClosed
		resourceClosed = true
	})
	defer closeController.Release()
	if _, err := awaitSocketPromise(t, closeController.Invoke()); err != nil {
		t.Fatal(err)
	}
	if !resourceClosed {
		t.Fatal("completion escaped an owned resource")
	}
}
