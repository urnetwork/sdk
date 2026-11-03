//go:build !ios && !android && !js

package sdk

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// A provider run with a proxy file gives one of its proxy slots the extender
// role (the sn miner's providerExtenderSlot), so the space's strategy can name a
// proxy. The relay's forward dial still leaves from this host: the operator
// proved this host's address, a proxy can't keep the client's family (A7, G2),
// and the slot's proxy is for what the slot provides.
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

	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	reached := make(chan struct{}, 1)
	go func() {
		conn, err := target.Accept()
		if err != nil {
			return
		}
		conn.Close()
		reached <- struct{}{}
	}()

	networkSpace := newTestProvideExtenderUrlSpace(ctx)
	defer networkSpace.Close()
	device, err := newDeviceLocalWithOverrides(
		networkSpace, "", "", "", "", NewId(), testExtenderStatusDeviceSettings(), connect.NewId())
	if err != nil {
		t.Fatal(err)
	}
	defer device.CloseAndWait(context.Background())

	// the settings a proxy slot's space holds; a test seam that answers dials
	// itself would take the place of the proxy, so none stays
	strategySettings := *device.provider.clientStrategySettings
	strategySettings.ProxySettings = &connect.ProxySettings{
		Network: "tcp",
		Address: proxyListener.Addr().String(),
	}
	strategySettings.DialContextSettings = nil
	device.provider.clientStrategySettings = &strategySettings
	device.provider.dialContextSettings = nil

	extenderSettings, err := device.provider.extenderSettings()
	if err != nil {
		t.Fatal(err)
	}
	dialCtx, dialCancel := context.WithTimeout(ctx, 30*time.Second)
	defer dialCancel()
	conn, err := extenderSettings.DialContext(dialCtx, "tcp4", target.Addr().String())
	if err != nil {
		t.Fatalf("the forward dial did not reach its destination: %s", err)
	}
	defer conn.Close()
	select {
	case <-reached:
	case <-dialCtx.Done():
		t.Fatal("the forward dial connected, but not to its destination")
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
