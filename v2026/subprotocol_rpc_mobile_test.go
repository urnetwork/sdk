//go:build sdk_mobile_bind

package sdk

import (
	"context"
	"net"
	"net/rpc"
	"strings"
	"testing"
)

// The binding tag hides the Go/JavaScript client types, not the companion RPC
// service embedded in a mobile DeviceLocal. Exercise the real net/rpc lookup so
// an unexported named wire type or an excluded server method fails here.
func TestMobileSubprotocolRpcServerRemainsRegistered(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	settings := DefaultDeviceLocalSettings()
	settings.AllowProvider = false
	local := &DeviceLocalRpc{
		ctx: ctx,
		deviceLocal: &DeviceLocal{
			settings: settings,
		},
	}
	server := rpc.NewServer()
	if err := server.RegisterName("DeviceLocalRpc", local); err != nil {
		t.Fatal(err)
	}
	clientConnection, serverConnection := net.Pipe()
	done := make(chan struct{})
	go func() {
		server.ServeConn(serverConnection)
		close(done)
	}()
	client := rpc.NewClient(clientConnection)
	response := new(subprotocolRpcResponse)
	if err := client.Call("DeviceLocalRpc.Subprotocol", new(subprotocolRpcRequest), response); err != nil {
		t.Fatalf("mobile companion subprotocol method is unavailable: %v", err)
	}
	if !strings.Contains(response.Error, "provider-capable") {
		t.Fatalf("mobile companion subprotocol method returned %q", response.Error)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	<-done
}
