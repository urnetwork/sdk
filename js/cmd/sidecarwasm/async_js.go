//go:build js && wasm && sidecar_http

// How Go waits on the browser in the page Worker. js/wasm runs one thread: a goroutine blocked on a channel hands the
// thread back to the event loop, so a promise or an IndexedDB request resolves while Go waits, but a js.FuncOf
// callback runs on the event loop itself, and one that blocked would hold the loop that has to resolve what it waits
// for, and the Worker would deadlock. So every callback here only sends, without waiting, on a channel with room for
// its one answer, and returns; and nothing here may be called from inside a js.FuncOf callback: a callback that needs
// the store or the key store starts a goroutine and returns.
//
// Each wait releases its callbacks once answered. Safe for concurrent use from goroutines.

package main

import (
	"errors"
	"fmt"
	"syscall/js"
)

// A browser call failed: a rejected promise, a request's error event or an aborted transaction. The browser's own
// error is kept as its name, a QuotaExceededError or an AbortError, never its message, which may carry a value.
var errBrowserCall = errors.New("browser call failed")

// One answer from a callback: the value it carried, or the failure.
type browserAnswer struct {
	value js.Value
	err   error
}

// The name of a DOMException or Error, or its type for anything else.
func browserErrorName(value js.Value) string {
	if value.Type() == js.TypeObject {
		if name := value.Get("name"); name.Type() == js.TypeString {
			return name.String()
		}
	}
	return value.Type().String()
}

// A failure carrying the browser's error name.
func browserError(value js.Value) error {
	return fmt.Errorf("%w: %s", errBrowserCall, browserErrorName(value))
}

// Waits for a promise to settle, from a goroutine, never a callback.
func awaitPromise(promise js.Value) (js.Value, error) {
	answers := make(chan browserAnswer, 1)
	var resolved, rejected js.Func
	answer := func(value browserAnswer) {
		select {
		case answers <- value:
		default:
		}
	}
	resolved = js.FuncOf(func(this js.Value, args []js.Value) any {
		value := js.Undefined()
		if 0 < len(args) {
			value = args[0]
		}
		answer(browserAnswer{value: value})
		return nil
	})
	rejected = js.FuncOf(func(this js.Value, args []js.Value) any {
		reason := js.Undefined()
		if 0 < len(args) {
			reason = args[0]
		}
		answer(browserAnswer{err: browserError(reason)})
		return nil
	})
	defer resolved.Release()
	defer rejected.Release()
	promise.Call("then", resolved, rejected)
	settled := <-answers
	return settled.value, settled.err
}

// Waits for an IDBRequest's success or error event and returns its result, from a goroutine, never a callback.
func awaitRequest(request js.Value) (js.Value, error) {
	answers := make(chan browserAnswer, 1)
	var succeeded, failed js.Func
	succeeded = js.FuncOf(func(this js.Value, args []js.Value) any {
		select {
		case answers <- browserAnswer{value: request.Get("result")}:
		default:
		}
		return nil
	})
	failed = js.FuncOf(func(this js.Value, args []js.Value) any {
		// the request's own error; its transaction aborts too
		select {
		case answers <- browserAnswer{err: browserError(request.Get("error"))}:
		default:
		}
		return nil
	})
	defer succeeded.Release()
	defer failed.Release()
	request.Set("onsuccess", succeeded)
	request.Set("onerror", failed)
	answer := <-answers
	request.Set("onsuccess", js.Null())
	request.Set("onerror", js.Null())
	return answer.value, answer.err
}

// Waits for an IDBTransaction to commit, which for a readwrite one with durability strict is when its writes are on
// the disk, or to fail or abort, which leaves none of them (§22.9's no torn writes). From a goroutine, never a
// callback.
func awaitTransaction(transaction js.Value) error {
	answers := make(chan browserAnswer, 1)
	var completed, failed js.Func
	completed = js.FuncOf(func(this js.Value, args []js.Value) any {
		select {
		case answers <- browserAnswer{}:
		default:
		}
		return nil
	})
	failed = js.FuncOf(func(this js.Value, args []js.Value) any {
		err := transaction.Get("error")
		answer := browserAnswer{err: fmt.Errorf("%w: AbortError", errBrowserCall)}
		if !err.IsNull() && !err.IsUndefined() {
			answer = browserAnswer{err: browserError(err)}
		}
		// an error event then an abort event: the first answer stands
		select {
		case answers <- answer:
		default:
		}
		return nil
	})
	defer completed.Release()
	defer failed.Release()
	transaction.Set("oncomplete", completed)
	transaction.Set("onerror", failed)
	transaction.Set("onabort", failed)
	answer := <-answers
	transaction.Set("oncomplete", js.Null())
	transaction.Set("onerror", js.Null())
	transaction.Set("onabort", js.Null())
	return answer.err
}

// A Go byte slice as a fresh Uint8Array.
func bytesToJs(value []byte) js.Value {
	array := js.Global().Get("Uint8Array").New(len(value))
	js.CopyBytesToJS(array, value)
	return array
}

// A Uint8Array's or ArrayBuffer's bytes, copied into Go, or false for any other value.
func bytesFromJs(value js.Value) ([]byte, bool) {
	if value.Type() != js.TypeObject {
		return nil, false
	}
	if value.InstanceOf(js.Global().Get("ArrayBuffer")) {
		value = js.Global().Get("Uint8Array").New(value)
	}
	if !value.InstanceOf(js.Global().Get("Uint8Array")) {
		return nil, false
	}
	copied := make([]byte, value.Get("byteLength").Int())
	js.CopyBytesToGo(copied, value)
	return copied, true
}
