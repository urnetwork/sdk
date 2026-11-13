//go:build !ios && !android && !js

package sdk

// The datagram relay's dial, which the provider builds with the stream
// forward's control and resolver (device_local_extender_forward_guard_test.go).
// Before the change the relay dialed with a bare dialer through the host's
// resolver, so this reads a setting that did not exist then.

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// The role's datagram relay dial, as the provider builds it: the same names
// get no udp socket, and the public one does.
func TestExtenderDatagramForwardDialRefusesNamesThatResolveToThisHostOrItsLan(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	observed := testExtenderForwardObservedAddress(t)

	networkSpace := newTestProvideExtenderUrlSpace(ctx)
	defer networkSpace.Close()
	device, err := newDeviceLocalWithOverrides(
		networkSpace, "", "", "", "", NewId(), testExtenderStatusDeviceSettings(), connect.NewId())
	if err != nil {
		t.Fatal(err)
	}
	defer device.CloseAndWait(context.Background())

	refused := []struct {
		name    string
		network string
		addr    netip.Addr
		reason  connect.SecurityPolicyReason
	}{
		{name: "lan." + testProvideExtenderHost, network: "udp4", addr: netip.MustParseAddr("10.0.0.1"), reason: connect.SecurityPolicyReasonNotPublic},
		{name: "loopback." + testProvideExtenderHost, network: "udp6", addr: netip.MustParseAddr("::1"), reason: connect.SecurityPolicyReasonNotPublic},
		{name: "observed." + testProvideExtenderHost, network: "udp4", addr: observed, reason: connect.SecurityPolicyReasonProviderLocal},
		{name: "pcp." + testProvideExtenderHost, network: "udp4", addr: netip.MustParseAddr("192.0.0.9"), reason: connect.SecurityPolicyReasonProviderLocal},
	}
	nameAddrs := map[string][]netip.Addr{
		"public." + testProvideExtenderHost: {netip.MustParseAddr("198.51.100.7")},
	}
	for _, c := range refused {
		nameAddrs[c.name] = []netip.Addr{c.addr}
	}
	sockets := &testExtenderForwardSockets{}
	testExtenderForwardEgress(device, newTestExtenderForwardResolver(t, nameAddrs), sockets)

	extenderSettings, err := device.provider.extenderSettings()
	if err != nil {
		t.Fatal(err)
	}
	dialCtx, dialCancel := context.WithTimeout(ctx, 30*time.Second)
	defer dialCancel()
	for _, c := range refused {
		conn, err := extenderSettings.DialPacketContext(dialCtx, c.network, net.JoinHostPort(c.name, "53"))
		if conn != nil {
			conn.Close()
			t.Errorf("%s (%s): the datagram relay got a socket", c.name, c.addr)
			continue
		}
		var refusal *connect.ProviderDialRefusedError
		if !errors.As(err, &refusal) || refusal.Reason != c.reason {
			t.Errorf("%s (%s): err = %v, want a %s refusal", c.name, c.addr, err, c.reason)
		}
	}
	conn, err := extenderSettings.DialPacketContext(dialCtx, "udp4", net.JoinHostPort("public."+testProvideExtenderHost, "53"))
	if conn != nil {
		conn.Close()
	}
	if !errors.Is(err, errTestExtenderForwardSocket) {
		t.Errorf("the public name: err = %v, want the strategy's control to see its socket", err)
	}
	if connected := sockets.connected(); !slices.Equal(connected, []string{"198.51.100.7:53"}) {
		t.Errorf("udp sockets that reached the strategy's control = %v, want only the public one", connected)
	}
}
