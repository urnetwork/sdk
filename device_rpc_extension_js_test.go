//go:build js

package sdk

import (
	"context"
	"io"
	"reflect"
	"sync"
	"syscall/js"
	"testing"
	"time"
)

type testingExtensionTransport struct {
	transport  js.Value
	callbacks  js.Value
	sent       [][]byte
	closeCount int
	mu         sync.Mutex
	funcs      []js.Func
}

func newTestingExtensionTransport(t *testing.T) *testingExtensionTransport {
	t.Helper()
	self := &testingExtensionTransport{transport: js.Global().Get("Object").New()}
	connection := js.Global().Get("Object").New()
	send := js.FuncOf(func(this js.Value, args []js.Value) any {
		array := js.Global().Get("Uint8Array").New(args[0])
		message := make([]byte, array.Get("byteLength").Int())
		js.CopyBytesToGo(message, array)
		self.mu.Lock()
		self.sent = append(self.sent, message)
		self.mu.Unlock()
		return nil
	})
	closeConnection := js.FuncOf(func(this js.Value, args []js.Value) any {
		self.mu.Lock()
		self.closeCount += 1
		self.mu.Unlock()
		return nil
	})
	open := js.FuncOf(func(this js.Value, args []js.Value) any {
		self.callbacks = args[0]
		return connection
	})
	connection.Set("send", send)
	connection.Set("close", closeConnection)
	self.transport.Set("open", open)
	self.funcs = []js.Func{send, closeConnection, open}
	t.Cleanup(func() {
		for _, fn := range self.funcs {
			fn.Release()
		}
	})
	return self
}

func (self *testingExtensionTransport) callback(name string, args ...any) {
	self.callbacks.Get(name).Invoke(args...)
}

func TestExtensionBridgeWsTransportsBinaryFrames(t *testing.T) {
	transport := newTestingExtensionTransport(t)
	settings := defaultDeviceRpcSettings()
	ws, err := newExtensionBridgeWs(transport.transport, settings)
	if err != nil {
		t.Fatal(err)
	}
	transport.callback("opened")
	select {
	case err := <-ws.opened:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("extension transport did not open")
	}

	outbound := []byte{0, 1, 2, 254, 255}
	if err := ws.WriteMessage(DeviceRpcWsBinary, outbound); err != nil {
		t.Fatal(err)
	}
	transport.mu.Lock()
	if !reflect.DeepEqual(transport.sent, [][]byte{outbound}) {
		t.Fatalf("sent = %v", transport.sent)
	}
	transport.mu.Unlock()

	inbound := []byte{9, 8, 7}
	array := js.Global().Get("Uint8Array").New(len(inbound))
	js.CopyBytesToJS(array, inbound)
	transport.callback("message", array)
	messageType, reader, err := ws.NextReader()
	if err != nil {
		t.Fatal(err)
	}
	actual, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if messageType != DeviceRpcWsBinary || !reflect.DeepEqual(actual, inbound) {
		t.Fatalf("received type=%d frame=%v", messageType, actual)
	}

	if err := ws.WriteControl(DeviceRpcWsPing, nil, time.Time{}); err != nil {
		t.Fatal(err)
	}
	transport.mu.Lock()
	if len(transport.sent) != 2 || len(transport.sent[1]) != 0 {
		t.Fatalf("keepalive was not preserved: %v", transport.sent)
	}
	transport.mu.Unlock()

	transport.callback("closed", "extension disconnected")
	if _, _, err := ws.NextReader(); err != io.EOF {
		t.Fatalf("closed NextReader error = %v", err)
	}
}

func TestExtensionBridgeWsRejectsOversizedInboundFrame(t *testing.T) {
	transport := newTestingExtensionTransport(t)
	settings := defaultDeviceRpcSettings()
	settings.MuxMaxFrameBytes = 2
	ws, err := newExtensionBridgeWs(transport.transport, settings)
	if err != nil {
		t.Fatal(err)
	}
	array := js.Global().Get("Uint8Array").New(3)
	transport.callback("message", array)
	if _, _, err := ws.NextReader(); err != io.EOF {
		t.Fatalf("oversized frame did not close transport: %v", err)
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if transport.closeCount != 1 {
		t.Fatalf("connection close count = %d", transport.closeCount)
	}
}

func TestNewExtensionDeviceRemoteIsRemoteOnly(t *testing.T) {
	transport := newTestingExtensionTransport(t)
	networkSpace := NewUrlsNetworkSpace("https://api.invalid", "wss://connect.invalid")
	remote, err := NewExtensionDeviceRemote(networkSpace, "", NewId(), transport.transport)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()

	if !remote.settings.BrowserStateOnly || !remote.settings.DisableHostedIncompatible || !remote.settings.RequireRemoteApi {
		t.Fatalf("extension settings are not hosted/remote-only: %+v", remote.settings)
	}
	if _, ok := remote.dialer.(*extensionDeviceRpcDialer); !ok {
		t.Fatalf("dialer = %T", remote.dialer)
	}
	api := networkSpace.GetApi()
	if api.httpGetRaw == nil || api.httpPostRaw == nil || api.httpPostStreamRaw == nil {
		t.Fatal("not every API request seam was installed on the DeviceRemote")
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := remote.httpGetRaw(ctx, "https://api.invalid/test", ""); err == nil {
		t.Fatal("remote-only API call fell back locally")
	}
}
