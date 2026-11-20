//go:build !ios && !android && !js

package sdk

// The relay's forward reaches no address of this host or its LAN, whatever a
// destination name resolves to (connect docs/IP_SECURITY.md §2.2.4). A client
// names the destination, any name under the operator's wildcard, and the name
// resolves on this host. Before the provider dial control, a name that
// resolved to a private address, to loopback or to this host's observed public
// address was dialed: the extender reached the provider host's own services
// and its LAN for any client.
//
// These tests drive the role's own forward dials, the ones
// deviceLocalProvider.extenderSettings builds, with the strategy's resolver
// answering for the names. The strategy's own dial control records every
// socket a forward would connect and refuses it, so nothing leaves this host,
// and the tests assert what it recorded and what the role logged. They run
// unchanged against the code before the change; the datagram relay's dial,
// which the provider builds since the change, is tested beside them
// (device_local_extender_forward_guard_datagram_test.go).

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/urnetwork/connect/v2026"
)

// newTestExtenderForwardResolver answers each name with its addresses from a
// local wire responder, and every other name with NXDOMAIN, so a test never
// depends on the host's resolver.
func newTestExtenderForwardResolver(t *testing.T, nameAddrs map[string][]netip.Addr) *net.Resolver {
	t.Helper()
	packetConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		packetConn.Close()
	})

	responsePayload := func(queryPayload []byte) ([]byte, bool) {
		var query dnsmessage.Message
		if err := query.Unpack(queryPayload); err != nil || len(query.Questions) != 1 {
			return nil, false
		}
		question := query.Questions[0]
		response := dnsmessage.Message{
			Header: dnsmessage.Header{
				ID:                 query.Header.ID,
				Response:           true,
				RecursionDesired:   query.Header.RecursionDesired,
				RecursionAvailable: true,
			},
			Questions: query.Questions,
		}
		addrs, ok := nameAddrs[strings.ToLower(strings.TrimSuffix(question.Name.String(), "."))]
		if !ok {
			response.Header.RCode = dnsmessage.RCodeNameError
		}
		for _, addr := range addrs {
			header := dnsmessage.ResourceHeader{
				Name:  question.Name,
				Class: dnsmessage.ClassINET,
				TTL:   60,
			}
			switch {
			case question.Type == dnsmessage.TypeA && addr.Is4():
				header.Type = dnsmessage.TypeA
				response.Answers = append(response.Answers, dnsmessage.Resource{
					Header: header,
					Body:   &dnsmessage.AResource{A: addr.As4()},
				})
			case question.Type == dnsmessage.TypeAAAA && addr.Is6():
				header.Type = dnsmessage.TypeAAAA
				response.Answers = append(response.Answers, dnsmessage.Resource{
					Header: header,
					Body:   &dnsmessage.AAAAResource{AAAA: addr.As16()},
				})
			}
		}
		payload, err := response.Pack()
		return payload, err == nil
	}

	go func() {
		buffer := make([]byte, 2048)
		for {
			count, sourceAddr, err := packetConn.ReadFrom(buffer)
			if err != nil {
				return
			}
			if payload, ok := responsePayload(buffer[:count]); ok {
				if _, err := packetConn.WriteTo(payload, sourceAddr); err != nil {
					return
				}
			}
		}
	}()

	dialer := &net.Dialer{}
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _ string, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "udp4", packetConn.LocalAddr().String())
		},
	}
}

var errTestExtenderForwardSocket = errors.New("the test refuses every forward socket")

// testExtenderForwardSockets is the strategy's dial control in these tests:
// it records the address of every socket a forward would connect, and
// refuses the socket.
type testExtenderForwardSockets struct {
	stateLock sync.Mutex
	addresses []string
}

func (self *testExtenderForwardSockets) control(network string, address string, c syscall.RawConn) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addresses = append(self.addresses, address)
	return errTestExtenderForwardSocket
}

func (self *testExtenderForwardSockets) connected() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.addresses)
}

// testExtenderForwardEgress puts the resolver and the recording control on
// the device's provider egress, the settings the role's forward dials are
// built from.
func testExtenderForwardEgress(device *DeviceLocal, resolver *net.Resolver, sockets *testExtenderForwardSockets) {
	strategySettings := *device.provider.clientStrategySettings
	strategySettings.ConnectSettings.Resolver = resolver
	strategySettings.ConnectSettings.DialControl = sockets.control
	strategySettings.ConnectSettings.DialContextSettings = nil
	strategySettings.ProxySettings = nil
	device.provider.clientStrategySettings = &strategySettings
	device.provider.dialContextSettings = nil
}

// The address the platform observes this host connecting from, set for the
// test's life. The provider-local set is the process's, as on a provider.
func testExtenderForwardObservedAddress(t *testing.T) netip.Addr {
	observed := netip.MustParseAddr("203.0.113.77")
	connect.SetProviderObservedAddresses([]netip.Addr{observed})
	t.Cleanup(func() {
		connect.SetProviderObservedAddresses(nil)
	})
	return observed
}

// The role's stream forward dial, as the provider builds it: a name that
// resolves to a private address, to IPv4 or IPv6 loopback, to this host's
// observed address or to port control anycast gets no socket, and a name that
// resolves to a public address still does.
func TestExtenderForwardDialRefusesNamesThatResolveToThisHostOrItsLan(t *testing.T) {
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
	}{
		{name: "lan." + testProvideExtenderHost, network: "tcp4", addr: netip.MustParseAddr("10.0.0.1")},
		{name: "loopback." + testProvideExtenderHost, network: "tcp6", addr: netip.MustParseAddr("::1")},
		{name: "loopback4." + testProvideExtenderHost, network: "tcp4", addr: netip.MustParseAddr("127.0.0.1")},
		{name: "observed." + testProvideExtenderHost, network: "tcp4", addr: observed},
		{name: "pcp." + testProvideExtenderHost, network: "tcp4", addr: netip.MustParseAddr("192.0.0.9")},
	}
	public := netip.MustParseAddr("198.51.100.7")
	nameAddrs := map[string][]netip.Addr{
		"public." + testProvideExtenderHost: {public},
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
	if !slices.Contains(extenderSettings.AllowedHosts, "*."+testProvideExtenderHost) {
		t.Fatalf("the role does not forward to names under %s: %v", testProvideExtenderHost, extenderSettings.AllowedHosts)
	}
	dialCtx, dialCancel := context.WithTimeout(ctx, 30*time.Second)
	defer dialCancel()
	for _, c := range refused {
		conn, err := extenderSettings.DialContext(dialCtx, c.network, net.JoinHostPort(c.name, "443"))
		if conn != nil {
			conn.Close()
		}
		if err == nil {
			t.Errorf("%s (%s): the forward connected", c.name, c.addr)
		}
	}
	conn, err := extenderSettings.DialContext(dialCtx, "tcp4", net.JoinHostPort("public."+testProvideExtenderHost, "443"))
	if conn != nil {
		conn.Close()
	}
	if !errors.Is(err, errTestExtenderForwardSocket) {
		t.Errorf("the public name: err = %v, want the test's refusal of its socket", err)
	}

	connected := sockets.connected()
	for _, c := range refused {
		address := net.JoinHostPort(c.addr.String(), "443")
		if slices.Contains(connected, address) {
			t.Errorf("%s: the forward opened a socket to %s", c.name, address)
		}
	}
	if !slices.Contains(connected, net.JoinHostPort(public.String(), "443")) {
		t.Errorf("the public name's socket never reached the strategy's control: %v", connected)
	}
}

// The composed path: clients of the running role ask it to forward streams
// and datagrams to names that resolve to a private address, to loopback and
// to this host's observed address. The role refuses each forward before a
// socket connects and logs why, and none of the addresses gets a socket.
func TestDeviceLocalProviderExtenderRefusesForwardsToThisHostOrItsLan(t *testing.T) {
	observed := testExtenderForwardObservedAddress(t)
	refused := []struct {
		name   string
		addr   netip.Addr
		reason string
	}{
		{name: "lan." + testProvideExtenderHost, addr: netip.MustParseAddr("10.0.0.1"), reason: "not-public"},
		{name: "loopback." + testProvideExtenderHost, addr: netip.MustParseAddr("127.0.0.1"), reason: "not-public"},
		{name: "observed." + testProvideExtenderHost, addr: observed, reason: "provider-local"},
	}
	nameAddrs := map[string][]netip.Addr{}
	for _, c := range refused {
		nameAddrs[c.name] = []netip.Addr{c.addr}
	}
	resolver := newTestExtenderForwardResolver(t, nameAddrs)
	sockets := &testExtenderForwardSockets{}
	log := newTestingForwardDialLogger()
	fixture := newTestProvideExtenderFixtureWithOptions(t, &testProvideExtenderFixtureOptions{
		configure: func(settings *deviceLocalExtenderSettings) {
			settings.Log = log
			settings.ForwardDialErrorLogInterval = -1
		},
		beforeProvide: func(device *DeviceLocal) {
			testExtenderForwardEgress(device, resolver, sockets)
		},
	})
	fixture.waitStatus("listening", func(status *ExtenderProvideStatus) bool {
		return status.Enabled && status.Listening
	})

	forward := func(name string, datagram bool) {
		t.Helper()
		connectSettings := connect.DefaultConnectSettings()
		connectSettings.ConnectTimeout = 10 * time.Second
		connectSettings.TlsTimeout = 10 * time.Second
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		conn, _, err := connect.DialExtender(
			ctx,
			connectSettings,
			&connect.ExtenderConfig{
				Profile: connect.ExtenderProfile{
					ConnectMode: connect.ExtenderConnectModeTcpTls,
					ServerName:  "front.example",
					Port:        fixture.tcpPort,
				},
				Ip:        netip.MustParseAddr("127.0.0.1"),
				PublicKey: fixture.extender().publicKey,
			},
			&connect.ExtenderDial{
				DestinationHost: name,
				DestinationPort: 443,
				Datagram:        datagram,
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		// the role closes the stream of a forward it did not dial
		if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadAll(conn); err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				t.Fatalf("%s: the role kept the stream of a forward it did not dial", name)
			}
		}
	}
	// the dialer's error names the address, the control's the reason
	awaitLine := func(stage string, address string, reason string) {
		t.Helper()
		prefix := "[extender]provide " + stage + " err = "
		suffix := address + ": refused by the provider security policy: " + reason + "\n"
		logged := func() bool {
			return slices.ContainsFunc(log.forwardDialLines(), func(line string) bool {
				return strings.HasPrefix(line, prefix) && strings.HasSuffix(line, suffix)
			})
		}
		deadline := time.After(10 * time.Second)
		for !logged() {
			select {
			case <-log.logged:
			case <-deadline:
				t.Fatalf("the role did not log a %s refusal of %s as %s; logged %q", stage, address, reason, log.forwardDialLines())
			}
		}
	}
	for _, c := range refused {
		address := net.JoinHostPort(c.addr.String(), strconv.Itoa(443))
		forward(c.name, false)
		awaitLine("forward dial", address, c.reason)
		forward(c.name, true)
		awaitLine("datagram forward dial", address, c.reason)
	}
	connected := sockets.connected()
	for _, c := range refused {
		address := net.JoinHostPort(c.addr.String(), "443")
		if slices.Contains(connected, address) {
			t.Errorf("%s: a forward opened a socket to %s", c.name, address)
		}
	}
}
