//go:build js

package main

import (
	"context"
	"net"
	"slices"
	"sync"
	"syscall/js"
	"testing"

	"github.com/urnetwork/sdk"
)

type subprotocolWasmChannel struct {
	mu       sync.Mutex
	closed   chan struct{}
	once     sync.Once
	received chan []byte
	sent     []byte
	queryOK  bool
}

func (c *subprotocolWasmChannel) Receive(ctx context.Context) (*sdk.Id, []byte, error) {
	select {
	case data := <-c.received:
		return sdk.NewId(), data, nil
	case <-c.closed:
		return nil, nil, net.ErrClosed
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}
func (c *subprotocolWasmChannel) Send(_ context.Context, _ *sdk.Id, data []byte) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = data
	return true, nil
}
func (c *subprotocolWasmChannel) Query(context.Context, *sdk.Id, int64) ([]int32, bool, error) {
	return []int32{4096, 4097}, c.queryOK, nil
}
func (c *subprotocolWasmChannel) Close() error { c.once.Do(func() { close(c.closed) }); return nil }

func TestSubprotocolWasmCopiesQueriesAndUnsubscribe(t *testing.T) {
	c := &subprotocolWasmChannel{closed: make(chan struct{}), received: make(chan []byte, 1), queryOK: true}
	m := map[string]any{}
	h := jsBindSubprotocolDevice(t.Context(), func(context.Context, int32) (jsSubprotocol, error) { return c, nil }, m)
	defer h.close()
	fn := m["subprotocolOperation"].(js.Func)
	defer fn.Release()
	handle, err := awaitSocketPromise(t, fn.Invoke("open", 0, 4096))
	if err != nil {
		t.Fatal(err)
	}
	id := handle.Get("id").Int()
	value := js.Global().Get("Uint8Array").New(3)
	js.CopyBytesToJS(value, []byte{0, 255, 7})
	send := fn.Invoke("send", id, map[string]any{"destinationClientId": sdk.NewId().String(), "bytes": value})
	value.SetIndex(1, 0)
	if ok, err := awaitSocketPromise(t, send); err != nil || !ok.Bool() {
		t.Fatal(err)
	}
	c.mu.Lock()
	if !slices.Equal(c.sent, []byte{0, 255, 7}) {
		t.Fatal("outgoing WASM data was not copied synchronously")
	}
	c.mu.Unlock()
	original := []byte{8, 0, 255}
	c.received <- original
	message, err := awaitSocketPromise(t, fn.Invoke("receive", id, js.Null()))
	if err != nil {
		t.Fatal(err)
	}
	clear(original)
	got := make([]byte, 3)
	js.CopyBytesToGo(got, message.Get("bytes"))
	if !slices.Equal(got, []byte{8, 0, 255}) {
		t.Fatal("incoming WASM data aliases Go memory")
	}
	queryArg := map[string]any{"destinationClientId": sdk.NewId().String(), "timeoutMillis": 1000}
	ids, err := awaitSocketPromise(t, fn.Invoke("query", id, queryArg))
	if err != nil || ids.Length() != 2 || ids.Index(0).Int() != 4096 {
		t.Fatal("query lost supported protocols", err)
	}
	c.queryOK = false
	ids, err = awaitSocketPromise(t, fn.Invoke("query", id, queryArg))
	if err != nil || !ids.IsNull() {
		t.Fatal("unanswered query must be null", err)
	}
	read := fn.Invoke("receive", id, js.Null())
	if _, err := awaitSocketPromise(t, fn.Invoke("release", id, js.Null())); err != nil {
		t.Fatal(err)
	}
	if _, err := awaitSocketPromise(t, read); err == nil {
		t.Fatal("unsubscribe did not unblock receive")
	}
	if _, err := awaitSocketPromise(t, fn.Invoke("receive", id, js.Null())); err == nil {
		t.Fatal("stale handle accepted")
	}
	if len(h.values) != 0 {
		t.Fatal("subscription handle leaked")
	}
}

func TestSubprotocolWasmDeviceClosureRejectsLateOpen(t *testing.T) {
	m := map[string]any{}
	c := &subprotocolWasmChannel{closed: make(chan struct{}), received: make(chan []byte, 1)}
	h := jsBindSubprotocolDevice(t.Context(), func(context.Context, int32) (jsSubprotocol, error) { return c, nil }, m)
	fn := m["subprotocolOperation"].(js.Func)
	defer fn.Release()
	h.close()
	if _, err := awaitSocketPromise(t, fn.Invoke("open", 0, 4096)); err == nil {
		t.Fatal("late subscription accepted")
	}
	select {
	case <-c.closed:
	default:
		t.Fatal("late native subscription leaked")
	}
}
