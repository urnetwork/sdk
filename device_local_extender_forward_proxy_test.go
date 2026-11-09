//go:build !ios && !android && !js

package sdk

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// A provider run with a proxy file gives one of its proxy slots the extender
// role (the sn miner's providerExtenderSlot), so the space's strategy can name a
// proxy. The relay's forward dial still leaves from this host: the operator
// proved this host's address, a proxy can't keep the client's family (A7, G2),
// and the slot's proxy is for what the slot provides.
//
// The destination is a public name, since the forward refuses loopback
// (device_local_extender_forward_guard_test.go): the strategy's control
// records the socket the forward opens and refuses it. A socket to the
// destination's own address is this host's dial; a dial through the proxy
// would have opened one to the proxy instead.
func TestExtenderForwardDialLeavesFromThisHostWhateverProxyTheStrategyNames(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// a proxy that records the connection and goes no further
	proxyListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxyListener.Close()
	proxied := make(chan struct{}, 1)
	go func() {
		conn, err := proxyListener.Accept()
		if err != nil {
			return
		}
		conn.Close()
		proxied <- struct{}{}
	}()

	networkSpace := newTestProvideExtenderUrlSpace(ctx)
	defer networkSpace.Close()
	device, err := newDeviceLocalWithOverrides(
		networkSpace, "", "", "", "", NewId(), testExtenderStatusDeviceSettings(), connect.NewId())
	if err != nil {
		t.Fatal(err)
	}
	defer device.CloseAndWait(context.Background())

	destination := "public." + testProvideExtenderHost
	sockets := &testExtenderForwardSockets{}
	testExtenderForwardEgress(device, newTestExtenderForwardResolver(t, map[string][]netip.Addr{
		destination: {netip.MustParseAddr("198.51.100.7")},
	}), sockets)
	// the settings a proxy slot's space holds
	strategySettings := *device.provider.clientStrategySettings
	strategySettings.ProxySettings = &connect.ProxySettings{
		Network: "tcp",
		Address: proxyListener.Addr().String(),
	}
	device.provider.clientStrategySettings = &strategySettings

	extenderSettings, err := device.provider.extenderSettings()
	if err != nil {
		t.Fatal(err)
	}
	dialCtx, dialCancel := context.WithTimeout(ctx, 30*time.Second)
	defer dialCancel()
	conn, err := extenderSettings.DialContext(dialCtx, "tcp4", net.JoinHostPort(destination, "443"))
	if conn != nil {
		conn.Close()
	}
	if !errors.Is(err, errTestExtenderForwardSocket) {
		t.Fatalf("the forward dial did not open a socket of this host's: %v", err)
	}
	if connected := sockets.connected(); !slices.Equal(connected, []string{"198.51.100.7:443"}) {
		t.Fatalf("the forward dial opened sockets to %v, want only its destination", connected)
	}
	select {
	case <-proxied:
		t.Fatal("the forward dial went through the proxy")
	default:
	}
	if strategySettings.ProxySettings == nil {
		t.Fatal("building the role's settings changed the space's proxy")
	}
}
