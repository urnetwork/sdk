//go:build js && wasm && sidecar_http

// The page Worker's boundary with its script, worker.js (§18.8, SDK-M4-05): one global, urnetworkSidecar, whose calls
// take and answer JSON strings, so no Go value reaches script and every id crosses as a string, never a JS number
// that would round an int64 (§6.8). Each call answers a Promise, since a js.FuncOf callback never waits (async_js.go):
// the callback starts a goroutine and returns, and the goroutine settles the promise.
//
//	version              the release version, empty in a development build
//	open(configJson)     opens this Worker's network (main.go's pageOpenConfig), answering its state's JSON; worker.js
//	                     calls it only once it holds the network's Web Lock
//	handle(requestJson)  answers one request with its reply's JSON (sidecarworker): a request that does not parse or
//	                     check is answered, never thrown
//	close()              ends the Sidecar and closes the instance
//
// A rejected promise carries an Error whose message is fixed per call, never a Go error's text, which could name a
// path or a value (I-13). Nothing here holds a key or a token: those stay in the root, behind the Worker's views.

package main

import (
	"context"
	"encoding/json"
	"syscall/js"

	"github.com/urnetwork/sidecar"
)

// Publishes worker's calls as the global worker.js reads.
func publishPageWorker(worker *pageWorker) {
	js.Global().Set(sidecarWasmGlobalName, js.ValueOf(map[string]any{
		"version": sidecar.Version,
		"open": js.FuncOf(func(this js.Value, args []js.Value) any {
			configJson, ok := stringArgument(args)
			return newBrowserPromise(func() (js.Value, error) {
				if !ok {
					return js.Undefined(), errPageCall("open takes its config as a JSON string")
				}
				config := &pageOpenConfig{}
				if err := json.Unmarshal([]byte(configJson), config); err != nil {
					return js.Undefined(), errPageCall("open's config does not parse")
				}
				status, err := worker.open(config)
				if err != nil {
					return js.Undefined(), errPageCall("the network did not open")
				}
				encoded, err := json.Marshal(status)
				if err != nil {
					return js.Undefined(), errPageCall("the network did not open")
				}
				return js.ValueOf(string(encoded)), nil
			})
		}),
		"handle": js.FuncOf(func(this js.Value, args []js.Value) any {
			requestJson, ok := stringArgument(args)
			return newBrowserPromise(func() (js.Value, error) {
				if !ok {
					// answered as any request that does not parse
					requestJson = ""
				}
				return js.ValueOf(string(worker.handle(worker.ctx, []byte(requestJson)))), nil
			})
		}),
		"close": js.FuncOf(func(this js.Value, args []js.Value) any {
			return newBrowserPromise(func() (js.Value, error) {
				if err := worker.close(context.Background()); err != nil {
					return js.Undefined(), errPageCall("the network did not close")
				}
				return js.Undefined(), nil
			})
		}),
	}))
}

// The first argument, when it is a string.
func stringArgument(args []js.Value) (string, bool) {
	if len(args) == 0 || args[0].Type() != js.TypeString {
		return "", false
	}
	return args[0].String(), true
}

// A rejection's fixed message.
type errPageCall string

func (self errPageCall) Error() string {
	return string(self)
}

// A Promise that run settles from a goroutine: resolved with its value, or rejected with an Error carrying the fixed
// message of an errPageCall and a generic one for anything else. The Promise constructor calls its executor at once,
// so the executor's callback is released as soon as the Promise exists.
func newBrowserPromise(run func() (js.Value, error)) js.Value {
	executor := js.FuncOf(func(this js.Value, args []js.Value) any {
		resolve, reject := args[0], args[1]
		go func() {
			value, err := run()
			if err != nil {
				message := "the page Worker's call failed"
				if call, ok := err.(errPageCall); ok {
					message = string(call)
				}
				reject.Invoke(js.Global().Get("Error").New(message))
				return
			}
			resolve.Invoke(value)
		}()
		return nil
	})
	defer executor.Release()
	return js.Global().Get("Promise").New(executor)
}

// Hands the thread to the browser's event loop until a zero timeout fires: what an operation's yield does on this leg
// (§18.10's threads row). A goroutine that only computes keeps the one thread, and runtime.Gosched hands it to other
// goroutines but never back to the loop, so a fetch's answer and a page's request wait until something blocks; a
// timer's promise blocks this goroutine on the loop itself, which then runs whatever is queued. From a goroutine,
// never a callback.
func yieldToEventLoop(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	executor := js.FuncOf(func(this js.Value, args []js.Value) any {
		js.Global().Call("setTimeout", args[0], 0)
		return nil
	})
	promise := js.Global().Get("Promise").New(executor)
	executor.Release()
	if _, err := awaitPromise(promise); err != nil {
		return err
	}
	return ctx.Err()
}
