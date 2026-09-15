//go:build js

package main

import (
	"context"
	"io"
	"net"
	"syscall/js"
	"testing"
	"time"

	"github.com/urnetwork/sdk"
)

type socketWasmDevice struct{ sdk.Device }

func (*socketWasmDevice) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if address == "wait:1" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	a, b := net.Pipe()
	go func() { defer b.Close(); _, _ = io.Copy(b, b) }()
	return a, nil
}
func awaitSocketPromise(t *testing.T, p js.Value) (js.Value, error) {
	t.Helper()
	type result struct {
		value js.Value
		err   error
	}
	done := make(chan result, 1)
	resolve := js.FuncOf(func(_ js.Value, a []js.Value) any { done <- result{value: a[0]}; return nil })
	reject := js.FuncOf(func(_ js.Value, a []js.Value) any {
		done <- result{err: &net.OpError{Op: "js", Err: socketWasmError(a[0].Get("message").String())}}
		return nil
	})
	defer resolve.Release()
	defer reject.Release()
	p.Call("then", resolve, reject)
	select {
	case r := <-done:
		return r.value, r.err
	case <-time.After(2 * time.Second):
		t.Fatal("WASM Promise blocked event loop")
		return js.Undefined(), nil
	}
}

type socketWasmError string

func (e socketWasmError) Error() string { return string(e) }
func TestSocketWasmDispatcher(t *testing.T) {
	m := map[string]any{}
	h := jsBindSocketDevice(&socketWasmDevice{}, m)
	defer h.close()
	fn := m["socketOperation"].(js.Func)
	defer fn.Release()
	handle, err := awaitSocketPromise(t, fn.Invoke("dial", 0, map[string]any{"network": "tcp", "address": "echo:1"}))
	if err != nil {
		t.Fatal(err)
	}
	id := handle.Get("id").Int()
	read := fn.Invoke("read", id, 3)
	data := js.Global().Get("Uint8Array").New(3)
	js.CopyBytesToJS(data, []byte{1, 2, 3})
	write := fn.Invoke("write", id, data)
	data.SetIndex(0, 9)
	got, err := awaitSocketPromise(t, read)
	if err != nil {
		t.Fatal(err)
	}
	p := make([]byte, 3)
	js.CopyBytesToGo(p, got.Get("data"))
	if p[0] != 1 || got.Get("eof").Bool() {
		t.Fatalf("WASM bytes %v", p)
	}
	size, err := awaitSocketPromise(t, write)
	if err != nil || size.Int() != 3 {
		t.Fatal(err)
	}
	if _, err = awaitSocketPromise(t, fn.Invoke("release", id, js.Null())); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	count := len(h.values)
	h.mu.Unlock()
	if count != 0 {
		t.Fatal("socket handle leaked")
	}
	if _, err = awaitSocketPromise(t, fn.Invoke("read", id, 1)); err == nil {
		t.Fatal("stale handle accepted")
	}
}
func TestSocketWasmDialTimeout(t *testing.T) {
	m := map[string]any{}
	h := jsBindSocketDevice(&socketWasmDevice{}, m)
	defer h.close()
	fn := m["socketOperation"].(js.Func)
	defer fn.Release()
	_, err := awaitSocketPromise(t, fn.Invoke("dial", 0, map[string]any{"network": "tcp", "address": "wait:1", "timeoutMillis": 10}))
	if err == nil {
		t.Fatal("dial ignored timeout")
	}
}

func TestSocketWasmDeviceClosureNotifiesIdleSocketAndRejectsLateDial(t *testing.T) {
	m := map[string]any{}
	h := jsBindSocketDevice(&socketWasmDevice{}, m)
	defer h.close()
	fn := m["socketOperation"].(js.Func)
	defer fn.Release()
	a, b := net.Pipe()
	defer b.Close()
	handle, err := h.add(a)
	if err != nil {
		t.Fatal(err)
	}
	id := handle.(map[string]any)["id"].(int)
	closed := fn.Invoke("socketClosed", id, js.Null())
	h.close()
	_, _ = awaitSocketPromise(t, closed) // Resolution or rejection both notify JS.
	h.mu.Lock()
	count := len(h.values)
	h.mu.Unlock()
	if count != 0 {
		t.Fatal("socket handle leaked after Device closure")
	}
	late, peer := net.Pipe()
	defer peer.Close()
	if _, err = h.add(late); err == nil {
		t.Fatal("late dial attached to closed Device")
	}
	if _, err = late.Write([]byte{1}); err == nil {
		t.Fatal("late connection was not closed")
	}
}
