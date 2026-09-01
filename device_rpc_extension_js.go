//go:build js

package sdk

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"syscall/js"
	"time"

	"github.com/urnetwork/connect"
)

// extensionDeviceRpcDialer keeps the SDK's ordinary DeviceRemote and
// two-stream rpc mux intact while delegating the websocket-shaped byte
// transport to JavaScript. ur.io supplies an extension-backed transport; it
// never supplies the device-rpc URL or credential to this constructor.
type extensionDeviceRpcDialer struct {
	transport js.Value
	settings  *deviceRpcSettings
	log       connect.Logger
}

var _ deviceRpcDialer = (*extensionDeviceRpcDialer)(nil)

func (self *extensionDeviceRpcDialer) Dial(ctx context.Context) (net.Conn, net.Conn, error) {
	ws, err := dialExtensionDeviceRpcWs(ctx, self.transport, self.settings)
	if err != nil {
		self.log.Infof("[dr]extension device rpc dial err = %s", err)
		return nil, nil, err
	}
	self.log.Infof("[dr]extension device rpc dial connected")
	mux := newDeviceRpcMux(ctx, ws, self.settings)
	return mux.conns[deviceRpcStreamForward], mux.conns[deviceRpcStreamReverse], nil
}

// NewExtensionDeviceRemote creates the real SDK DeviceRemote used by ur.io,
// but routes its opaque device-rpc frames through the supplied extension
// transport. API operations installed on the NetworkSpace are remote-only in
// this mode: losing the extension connection can never fall back to a page
// fetch or a page-owned control connection.
func NewExtensionDeviceRemote(
	networkSpace *NetworkSpace,
	byJwt string,
	instanceId *Id,
	transport js.Value,
) (*DeviceRemote, error) {
	if !transport.Truthy() || (transport.Type() != js.TypeObject && transport.Type() != js.TypeFunction) {
		return nil, fmt.Errorf("extension device rpc transport is required")
	}
	clientId, err := parseByJwtClientId(byJwt)
	if err != nil {
		if err != errByJwtNoClientId {
			return nil, err
		}
		clientId = connect.Id{}
	}
	settings := defaultDeviceRpcSettings()
	settings.DisableHostedIncompatible = true
	settings.BrowserStateOnly = true
	settings.RequireRemoteApi = true
	dialer := &extensionDeviceRpcDialer{
		transport: transport,
		settings:  settings,
		log:       settings.logger(),
	}
	return newDeviceRemoteWithOverrides(networkSpace, byJwt, instanceId, settings, clientId, dialer)
}

func dialExtensionDeviceRpcWs(
	ctx context.Context,
	transport js.Value,
	settings *deviceRpcSettings,
) (deviceRpcWs, error) {
	ws, err := newExtensionBridgeWs(transport, settings)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			_ = ws.Close()
		}
	}()

	connectTimeout := settings.RpcConnectTimeout
	if connectTimeout <= 0 {
		connectTimeout = 30 * time.Second
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(connectTimeout):
		return nil, fmt.Errorf("extension device rpc open timeout")
	case err := <-ws.opened:
		if err != nil {
			return nil, err
		}
	}
	success = true
	return ws, nil
}

// extensionBridgeWs is deliberately the same small websocket-shaped surface
// as browserWs. JavaScript owns connection policy and the actual socket; Go
// owns framing, net/rpc, state synchronization, and every Device method.
type extensionBridgeWs struct {
	transport  js.Value
	connection js.Value

	opened  chan error
	receive chan []byte
	done    chan struct{}

	openOnce  sync.Once
	closeOnce sync.Once
	openFunc  js.Func
	msgFunc   js.Func
	closeFunc js.Func

	connectionMu    sync.Mutex
	receiveMu       sync.Mutex
	readLimit       int64
	maxReceiveBytes int64
	receiveBytes    int64
}

var _ deviceRpcWs = (*extensionBridgeWs)(nil)

func newExtensionBridgeWs(transport js.Value, settings *deviceRpcSettings) (*extensionBridgeWs, error) {
	self := &extensionBridgeWs{
		transport:       transport,
		opened:          make(chan error, 1),
		receive:         make(chan []byte, max(1, settings.MuxReceiveBufferSize)),
		done:            make(chan struct{}),
		readLimit:       settings.maxFrameBytes(),
		maxReceiveBytes: max(settings.maxQueuedBytes(), settings.maxFrameBytes()),
	}

	self.openFunc = js.FuncOf(func(this js.Value, args []js.Value) any {
		self.openOnce.Do(func() { self.opened <- nil })
		return nil
	})
	self.msgFunc = js.FuncOf(func(this js.Value, args []js.Value) any {
		if len(args) < 1 || !args[0].Truthy() {
			self.closeInternal(fmt.Errorf("extension device rpc delivered an invalid frame"))
			return nil
		}
		array, err := extensionUint8Array(args[0])
		if err != nil {
			self.closeInternal(err)
			return nil
		}
		n := array.Get("byteLength").Int()
		if !self.reserveReceive(n) {
			self.closeInternal(fmt.Errorf("extension device rpc receive limit exceeded"))
			return nil
		}
		message := make([]byte, n)
		js.CopyBytesToGo(message, array)
		if !self.offerReceive(message) {
			self.releaseReceive(n)
			self.closeInternal(io.ErrClosedPipe)
		}
		return nil
	})
	self.closeFunc = js.FuncOf(func(this js.Value, args []js.Value) any {
		reason := "extension device rpc closed"
		if len(args) > 0 && args[0].Type() == js.TypeString && args[0].String() != "" {
			reason += ": " + args[0].String()
		}
		self.closeInternal(fmt.Errorf("%s", reason))
		return nil
	})

	callbacks := js.Global().Get("Object").New()
	callbacks.Set("opened", self.openFunc)
	callbacks.Set("message", self.msgFunc)
	callbacks.Set("closed", self.closeFunc)
	connection, err := callExtensionMethod(transport, "open", callbacks)
	if err != nil {
		self.closeInternal(err)
		return nil, err
	}
	if !connection.Truthy() || (connection.Type() != js.TypeObject && connection.Type() != js.TypeFunction) {
		err := fmt.Errorf("extension device rpc transport.open returned no connection")
		self.closeInternal(err)
		return nil, err
	}
	self.connectionMu.Lock()
	self.connection = connection
	self.connectionMu.Unlock()
	select {
	case <-self.done:
		_, _ = callExtensionMethod(connection, "close")
	default:
	}
	return self, nil
}

func extensionUint8Array(value js.Value) (array js.Value, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("invalid extension device rpc frame: %v", recovered)
		}
	}()
	array = js.Global().Get("Uint8Array").New(value)
	return array, nil
}

func callExtensionMethod(value js.Value, method string, args ...any) (result js.Value, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("extension device rpc %s failed: %v", method, recovered)
		}
	}()
	fn := value.Get(method)
	if fn.Type() != js.TypeFunction {
		return js.Undefined(), fmt.Errorf("extension device rpc transport has no %s method", method)
	}
	return value.Call(method, args...), nil
}

func (self *extensionBridgeWs) reserveReceive(byteCount int) bool {
	self.receiveMu.Lock()
	defer self.receiveMu.Unlock()
	if self.readLimit < int64(byteCount) || self.maxReceiveBytes < self.receiveBytes+int64(byteCount) {
		return false
	}
	self.receiveBytes += int64(byteCount)
	return true
}

func (self *extensionBridgeWs) releaseReceive(byteCount int) {
	self.receiveMu.Lock()
	self.receiveBytes -= int64(byteCount)
	self.receiveMu.Unlock()
}

func (self *extensionBridgeWs) offerReceive(message []byte) bool {
	self.receiveMu.Lock()
	defer self.receiveMu.Unlock()
	return offerDeviceRpcReceive(self.done, self.receive, message)
}

func (self *extensionBridgeWs) closeInternal(openErr error) {
	self.closeOnce.Do(func() {
		self.openOnce.Do(func() { self.opened <- openErr })
		close(self.done)

		self.connectionMu.Lock()
		connection := self.connection
		self.connection = js.Undefined()
		self.connectionMu.Unlock()
		if connection.Truthy() {
			_, _ = callExtensionMethod(connection, "close")
		}

		self.receiveMu.Lock()
	drainReceive:
		for {
			select {
			case message := <-self.receive:
				self.receiveBytes -= int64(len(message))
			default:
				break drainReceive
			}
		}
		self.receiveMu.Unlock()
		self.openFunc.Release()
		self.msgFunc.Release()
		self.closeFunc.Release()
	})
}

func (self *extensionBridgeWs) WriteMessage(messageType int, data []byte) error {
	select {
	case <-self.done:
		return io.ErrClosedPipe
	default:
	}
	array := js.Global().Get("Uint8Array").New(len(data))
	js.CopyBytesToJS(array, data)
	self.connectionMu.Lock()
	connection := self.connection
	self.connectionMu.Unlock()
	if !connection.Truthy() {
		return io.ErrClosedPipe
	}
	_, err := callExtensionMethod(connection, "send", array)
	if err != nil {
		self.closeInternal(err)
	}
	return err
}

func (self *extensionBridgeWs) WriteControl(messageType int, data []byte, deadline time.Time) error {
	if messageType == DeviceRpcWsPing {
		return self.WriteMessage(DeviceRpcWsBinary, nil)
	}
	return nil
}

func (self *extensionBridgeWs) NextReader() (int, io.Reader, error) {
	select {
	case <-self.done:
		return 0, nil, io.EOF
	case message := <-self.receive:
		self.releaseReceive(len(message))
		return DeviceRpcWsBinary, bytes.NewReader(message), nil
	}
}

func (self *extensionBridgeWs) SetReadLimit(limit int64) {
	self.receiveMu.Lock()
	self.readLimit = limit
	self.receiveMu.Unlock()
}

func (self *extensionBridgeWs) SetReadDeadline(time.Time) error   { return nil }
func (self *extensionBridgeWs) SetWriteDeadline(time.Time) error  { return nil }
func (self *extensionBridgeWs) SetPongHandler(func(string) error) {}
func (self *extensionBridgeWs) Close() error {
	self.closeInternal(io.ErrClosedPipe)
	return nil
}

type extensionBridgeWsAddr struct{}

func (extensionBridgeWsAddr) Network() string        { return "extension" }
func (extensionBridgeWsAddr) String() string         { return "extension-device-rpc" }
func (self *extensionBridgeWs) LocalAddr() net.Addr  { return extensionBridgeWsAddr{} }
func (self *extensionBridgeWs) RemoteAddr() net.Addr { return extensionBridgeWsAddr{} }
