//go:build sdk_mobile_bind

package sdk

import (
	"context"
	"net"
	"net/rpc"
	"strings"
	"testing"

	"github.com/urnetwork/sdk/v2026/internal/subprotocolrpc"
)

// The binding tag hides the Go/JavaScript client types, not the companion RPC
// service embedded in a mobile DeviceLocal. Exercise the real net/rpc lookup so
// an unexported wire type or an accidentally excluded server method fails here.
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
	response := new(subprotocolrpc.Response)
	if err := client.Call("DeviceLocalRpc.Subprotocol", new(subprotocolrpc.Request), response); err != nil {
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
