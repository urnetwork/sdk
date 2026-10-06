//go:build js

package main

import (
	"context"
	"errors"
	"net"
	"syscall/js"

	"github.com/urnetwork/sdk/v2026"
)

type jsSubprotocol interface {
	Receive(context.Context) (*sdk.Id, []byte, error)
	Send(context.Context, *sdk.Id, []byte) (bool, error)
	Query(context.Context, *sdk.Id, int64) ([]int32, bool, error)
	Close() error
}

type jsOpenSubprotocol func(context.Context, int32) (jsSubprotocol, error)

// One dispatcher and the existing bounded handle registry per device. Copies
// happen synchronously on entry/exit; Go never retains a caller's typed array.
func jsBindSubprotocolDevice(parent context.Context, open jsOpenSubprotocol, m map[string]any) *jsSocketHandles {
	ctx, cancel := context.WithCancel(parent)
	h := &jsSocketHandles{values: make(map[int]any), done: make(map[int]chan struct{}), ctx: ctx, cancel: cancel}
	go func() { <-ctx.Done(); h.close() }()
	m["subprotocolOperation"] = js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) != 3 || args[0].Type() != js.TypeString || args[1].Type() != js.TypeNumber {
			return jsRejected(errors.New("subprotocolOperation requires operation, handle, and argument"))
		}
		op, handle, arg := args[0].String(), args[1].Int(), args[2]
		var protocol int32
		var timeout int64
		var destination *sdk.Id
		var data []byte
		if op == "open" {
			if arg.Type() != js.TypeNumber || arg.Float() != float64(arg.Int()) || arg.Int() < int(sdk.SubprotocolReservedLimit) || arg.Int() > 65535 {
				return jsRejected(errors.New("invalid application subprotocol id"))
			}
			protocol = int32(arg.Int())
		}
		if op == "send" || op == "query" {
			if arg.Type() != js.TypeObject || arg.IsNull() || arg.Get("destinationClientId").Type() != js.TypeString {
				return jsRejected(errors.New("destinationClientId is required"))
			}
			var err error
			destination, err = sdk.ParseId(arg.Get("destinationClientId").String())
			if err != nil {
				return jsRejected(err)
			}
			if op == "send" {
				value := arg.Get("bytes")
				if !value.InstanceOf(js.Global().Get("Uint8Array")) || value.Length() > 65535 {
					return jsRejected(errors.New("expected Uint8Array of at most 65535 bytes"))
				}
				data = make([]byte, value.Length())
				js.CopyBytesToGo(data, value)
			} else {
				value := arg.Get("timeoutMillis")
				if value.Type() != js.TypeNumber || value.Float() != float64(value.Int()) || value.Int() < 1 || value.Int() > 60000 {
					return jsRejected(errors.New("timeoutMillis must be between 1 and 60000"))
				}
				timeout = int64(value.Int())
			}
		}
		return jsPromise(func(resolve func(any), reject func(error)) {
			var result any
			var err error
			if op == "open" {
				var channel jsSubprotocol
				channel, err = open(ctx, protocol)
				if err == nil {
					result, err = h.add(channel)
				}
			} else {
				h.mu.Lock()
				resource := h.values[handle]
				if op == "release" {
					delete(h.values, handle)
					if done := h.done[handle]; done != nil {
						close(done)
						delete(h.done, handle)
					}
				}
				h.mu.Unlock()
				channel, _ := resource.(jsSubprotocol)
				if op == "release" {
					if channel != nil {
						err = channel.Close()
					}
				} else if channel == nil {
					err = net.ErrClosed
				} else {
					switch op {
					case "receive":
						var source *sdk.Id
						var frame []byte
						source, frame, err = channel.Receive(ctx)
						if err == nil {
							bytes := js.Global().Get("Uint8Array").New(len(frame))
							js.CopyBytesToJS(bytes, frame)
							result = map[string]any{"sourceClientId": source.String(), "bytes": bytes}
						}
					case "send":
						result, err = channel.Send(ctx, destination, data)
					case "query":
						var ids []int32
						var ok bool
						ids, ok, err = channel.Query(ctx, destination, timeout)
						if err == nil && ok {
							out := make([]any, len(ids))
							for i, id := range ids {
								out[i] = id
							}
							result = out
						}
					default:
						err = errors.New("unknown subprotocol operation")
					}
				}
			}
			if err != nil {
				reject(err)
			} else {
				resolve(result)
			}
		})
	})
	return h
}
