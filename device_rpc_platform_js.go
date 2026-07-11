//go:build js

package sdk

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"syscall/js"
	"time"
)

// dialDeviceRpcWs opens the proxy-host device-rpc websocket with the browser
// WebSocket api. Auth is the signed proxy id carried in the url (the `proxy`
// query parameter), which is exactly why the browser can authenticate: it
// cannot set request headers but can set query params. There is no auth frame,
// so the mux starts immediately. proxyUrl may be a bare host, a host:port, or a
// ws/wss url; a bare host defaults to wss.
func dialDeviceRpcWs(ctx context.Context, proxyUrl string, signedProxyId string, settings *deviceRpcSettings) (deviceRpcWs, error) {
	u, err := deviceRpcUrl(proxyUrl, signedProxyId)
	if err != nil {
		return nil, err
	}

	ws, err := newBrowserWs(u)
	if err != nil {
		return nil, err
	}

	success := false
	defer func() {
		if !success {
			ws.Close()
		}
	}()

	connectTimeout := settings.RpcConnectTimeout
	if connectTimeout <= 0 {
		connectTimeout = 30 * time.Second
	}

	// wait for open
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(connectTimeout):
		return nil, fmt.Errorf("device rpc websocket open timeout")
	case err := <-ws.opened:
		if err != nil {
			return nil, err
		}
	}

	success = true
	return ws, nil
}

// browserWs adapts the browser WebSocket object to DeviceRpcWs. Each received
// binary message is delivered whole on the receive channel; NextReader returns
// a reader over the next message (the mux calls MessagePoolReadAll, which reads
// it fully). Browsers cannot send protocol pings, so WriteControl(ping) sends a
// zero-length binary message instead; the peer's mux read loop discards
// zero-length messages.
type browserWs struct {
	ws js.Value

	opened chan error
	// received binary messages as []byte, in order
	receive chan []byte
	// closed when the socket closes or errors
	done      chan struct{}
	closeOnce sync.Once

	openOnce  sync.Once
	openFunc  js.Func
	msgFunc   js.Func
	errFunc   js.Func
	closeFunc js.Func
}

var _ deviceRpcWs = (*browserWs)(nil)

func newBrowserWs(url string) (*browserWs, error) {
	wsClass := js.Global().Get("WebSocket")
	if !wsClass.Truthy() {
		return nil, fmt.Errorf("WebSocket is not available")
	}
	ws := wsClass.New(url)
	ws.Set("binaryType", "arraybuffer")

	self := &browserWs{
		ws:      ws,
		opened:  make(chan error, 1),
		receive: make(chan []byte, 256),
		done:    make(chan struct{}),
	}

	self.openFunc = js.FuncOf(func(this js.Value, args []js.Value) any {
		self.openOnce.Do(func() { self.opened <- nil })
		return nil
	})
	self.msgFunc = js.FuncOf(func(this js.Value, args []js.Value) any {
		data := args[0].Get("data")
		// arraybuffer -> []byte
		uint8Array := js.Global().Get("Uint8Array").New(data)
		n := uint8Array.Get("length").Int()
		b := make([]byte, n)
		js.CopyBytesToGo(b, uint8Array)
		select {
		case self.receive <- b:
		case <-self.done:
		}
		return nil
	})
	self.errFunc = js.FuncOf(func(this js.Value, args []js.Value) any {
		self.openOnce.Do(func() { self.opened <- fmt.Errorf("device rpc websocket error") })
		self.closeInternal()
		return nil
	})
	self.closeFunc = js.FuncOf(func(this js.Value, args []js.Value) any {
		self.openOnce.Do(func() { self.opened <- fmt.Errorf("device rpc websocket closed before open") })
		self.closeInternal()
		return nil
	})

	ws.Call("addEventListener", "open", self.openFunc)
	ws.Call("addEventListener", "message", self.msgFunc)
	ws.Call("addEventListener", "error", self.errFunc)
	ws.Call("addEventListener", "close", self.closeFunc)

	return self, nil
}

func (self *browserWs) closeInternal() {
	self.closeOnce.Do(func() {
		close(self.done)
		self.ws.Call("close")
		self.openFunc.Release()
		self.msgFunc.Release()
		self.errFunc.Release()
		self.closeFunc.Release()
	})
}

func (self *browserWs) WriteMessage(messageType int, data []byte) error {
	select {
	case <-self.done:
		return io.ErrClosedPipe
	default:
	}
	// copy into a js Uint8Array and send
	array := js.Global().Get("Uint8Array").New(len(data))
	js.CopyBytesToJS(array, data)
	self.ws.Call("send", array.Get("buffer"))
	return nil
}

func (self *browserWs) WriteControl(messageType int, data []byte, deadline time.Time) error {
	// browsers cannot send protocol pings; the mux uses a ping as keepalive, so
	// send a zero-length binary message the peer's read loop discards.
	if messageType == DeviceRpcWsPing {
		return self.WriteMessage(DeviceRpcWsBinary, nil)
	}
	return nil
}

func (self *browserWs) NextReader() (int, io.Reader, error) {
	select {
	case <-self.done:
		return 0, nil, io.EOF
	case b := <-self.receive:
		return DeviceRpcWsBinary, strings.NewReader(string(b)), nil
	}
}

func (self *browserWs) SetReadDeadline(t time.Time) error  { return nil }
func (self *browserWs) SetWriteDeadline(t time.Time) error { return nil }
func (self *browserWs) SetPongHandler(h func(string) error) {
	// browser answers pings automatically; no pong callback available
}
func (self *browserWs) Close() error {
	self.closeInternal()
	return nil
}

// browserWsAddr is a stub net.Addr for the browser websocket.
type browserWsAddr struct{}

func (browserWsAddr) Network() string { return "websocket" }
func (browserWsAddr) String() string  { return "browser" }

func (self *browserWs) LocalAddr() net.Addr  { return browserWsAddr{} }
func (self *browserWs) RemoteAddr() net.Addr { return browserWsAddr{} }
