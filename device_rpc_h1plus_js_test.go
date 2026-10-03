//go:build js

package sdk

import (
	"net/url"
	"syscall/js"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The native H1+ setting, on by default, must never send the browser down an
// unsupported raw-HTTP-upgrade path. Drive the actual browser dial function
// with the browser WebSocket boundary instrumented, including its normal URL
// authorization.
func TestDeviceRpcH1PlusBrowserSkipsCustomUpgrade(t *testing.T) {
	if connect.H1PlusAvailable() {
		t.Fatal("browser advertised a raw HTTP upgrade capability")
	}
	previous := js.Global().Get("WebSocket")
	var openedUrl string
	var constructorArgs, closes int
	addListener := js.FuncOf(func(_ js.Value, args []js.Value) any {
		if args[0].String() == "open" {
			// Deliver open after all production event handlers are registered.
			js.Global().Call("setTimeout", args[1], 0)
		}
		return nil
	})
	closeSocket := js.FuncOf(func(_ js.Value, _ []js.Value) any { closes++; return nil })
	constructor := js.FuncOf(func(_ js.Value, args []js.Value) any {
		constructorArgs = len(args)
		openedUrl = args[0].String()
		object := js.Global().Get("Object").New()
		object.Set("addEventListener", addListener)
		object.Set("close", closeSocket)
		return object
	})
	js.Global().Set("WebSocket", constructor)
	defer func() {
		js.Global().Set("WebSocket", previous)
		constructor.Release()
		closeSocket.Release()
		addListener.Release()
	}()
	s := defaultDeviceRpcSettings()
	s.EnableH1Plus = true
	s.H1PlusStats = &connect.H1PlusStats{}
	s.RpcConnectTimeout = time.Second
	const credential = "browser-test-only+/="
	ws, err := dialDeviceRpcWs(t.Context(), "wss://rpc.example.invalid", credential, s)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ws.(*browserWs); !ok {
		ws.Close()
		t.Fatalf("browser selected %T instead of its WebSocket API", ws)
	}
	ws.Close()
	u, err := url.Parse(openedUrl)
	if err != nil || u.Scheme != "wss" || u.Path != "/device-rpc" || u.Query().Get("proxy") != credential {
		t.Fatalf("browser WebSocket URL did not preserve authorization: %q %v", openedUrl, err)
	}
	if constructorArgs != 1 || closes != 1 {
		t.Fatalf("unexpected browser handshake/close behavior: constructor args=%d closes=%d", constructorArgs, closes)
	}
	if stats := s.H1PlusStats.Snapshot(); stats.Attempts != 0 || stats.Accepted != 0 || stats.Fallbacks != 0 {
		t.Fatalf("browser attempted native custom upgrade: %+v", stats)
	}
}
