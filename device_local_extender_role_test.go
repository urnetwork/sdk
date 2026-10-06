//go:build !ios && !android && !js

package sdk

import (
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/gossip"
)

// The provider extender role states the activation tests never reach
// (EXTENDER.md G2, A8, L2): the app the user put in the feed-only gossip mode,
// which serves the feed without a node and refuses the gossip service, and the
// carrier binds -- tcp 443, the one port an extender requires, which another
// process holds, and an optional udp carrier that cannot bind.

// A role whose app is in the feed-only mode runs the server and the feed
// service without a node, and refuses the gossip service rather than accepting
// a stream nothing will read (G2, A8).
func TestDeviceLocalProviderExtenderFeedModeRunsNoNode(t *testing.T) {
	fixture := newTestProvideExtenderFixture(t, nil)
	fixture.waitPass()
	fixture.waitPost()
	fixture.waitPost()
	// the member default: the role swapped in a listening extender node, which
	// is what the feed mode below must not do
	fixture.waitNodeListenAddrs([]string{
		fmt.Sprintf("/ip4/%s/tcp/%d", testProvideExtenderIpv4, fixture.tcpPort),
		fmt.Sprintf("/ip6/%s/tcp/%d", testProvideExtenderIpv6, fixture.tcpPort),
	})

	// the mode is written straight to the local state, so the restart below
	// reads it rather than racing the space's asynchronous save
	if err := fixture.networkSpace.asyncLocalState.GetLocalState().SetExtenderGossipMode(
		ExtenderGossipModeFeed,
	); err != nil {
		t.Fatal(err)
	}
	if role := extenderRole(fixture.networkSpace.GetExtenderGossipMode()); role != ExtenderRoleFeed {
		t.Fatalf("app role = %q, expected feed", role)
	}
	// the setting is what restarts the role, which is how a mode change takes
	fixture.device.SetProvideExtender(false)
	fixture.device.SetProvideExtender(true)

	extender := fixture.extender()
	if extender == nil {
		t.Fatal("the feed mode stopped the role")
	}
	if extenderNode := fixture.networkSpace.getExtenderNode(); extenderNode != nil {
		t.Fatalf("the feed mode ran a node in role %q", extenderNode.role())
	}
	if extender.gossipNodeRuns() {
		t.Fatal("the role believes it has a node to hand gossip streams to")
	}
	serverSettings := extender.serverSettings(extender.gossipNodeRuns())
	if serverSettings.GossipConnHandler != nil {
		t.Fatal("an extender with no node did not refuse the gossip service")
	}
	if serverSettings.FeedConnHandler == nil {
		t.Fatal("the feed service was not served")
	}
	if extender.feedServer == nil {
		t.Fatal("the role built no feed server")
	}

	// and it still binds, activates and publishes its record
	fixture.waitPost()
	status := fixture.waitStatus("activated without a node", func(status *ExtenderProvideStatus) bool {
		return status.Enabled && status.Listening && status.ActivatedV4
	})
	if status.ListenError != "" {
		t.Fatalf("listen error = %q, expected every carrier to bind", status.ListenError)
	}
	// the activated addresses never build a node: there is none to advertise
	// them on in this mode
	if extenderNode := fixture.networkSpace.getExtenderNode(); extenderNode != nil {
		t.Fatalf("an activation built a node in the feed mode, role %q", extenderNode.role())
	}
}

// Holds a tcp port on loopback the way another process of this host would, so
// the role cannot bind it. Returns the release.
func testHoldTcpPort(t *testing.T, port int) func() {
	t.Helper()
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	return func() { listener.Close() }
}

// The retry seam of a test role: every wait the role asks for is recorded,
// and the retry fires only when the test sends on `retry`.
func testTcpRetry(settings *deviceLocalExtenderSettings, waits chan time.Duration, retry chan time.Time) {
	settings.TcpRetryAfter = func(wait time.Duration) <-chan time.Time {
		waits <- wait
		return retry
	}
}

// Another process holding tcp 443, the one port an extender requires, turns
// the role off (G2): it binds no udp carrier, activates nothing and runs no
// extender node, the status says why, and it waits to bind the port again
// after extenderProvideTcpRetryTimeout.
func TestDeviceLocalProviderExtenderWithTcpTakenStartsNoCarrier(t *testing.T) {
	tcpPort := testFreeTcpPort(t)
	testHoldTcpPort(t, tcpPort)
	udpBinds := make(chan int, 64)
	waits := make(chan time.Duration, 16)
	fixture := newTestProvideExtenderFixture(t, func(settings *deviceLocalExtenderSettings) {
		settings.TcpPort = tcpPort
		testRecordUdpBinds(settings, udpBinds)
		testTcpRetry(settings, waits, make(chan time.Time))
	})

	status := fixture.waitStatus("off for want of tcp", func(status *ExtenderProvideStatus) bool {
		return status.ErrorCase == ExtenderProvideErrorTcpUnavailable
	})
	if status.State != ExtenderProvideStateError || status.Reason == "" ||
		status.Reason != status.TcpUnavailableError {
		t.Fatalf("status = %+v, expected the error state with the tcp bind error as its reason", status)
	}
	if status.Enabled || status.Listening {
		t.Fatalf("status = %+v, expected the role off", status)
	}
	if !strings.HasPrefix(status.ListenError, connect.ExtenderCarrierTcp+": ") {
		t.Fatalf("listen error = %q, expected the tcp carrier named", status.ListenError)
	}
	if status.DnsPorts != "" || status.ActivatedV4 || status.ActivatedV6 {
		t.Fatalf("status = %+v, expected nothing bound or activated", status)
	}
	select {
	case wait := <-waits:
		connect.AssertEqual(t, wait, extenderProvideTcpRetryTimeout)
	case <-time.After(10 * time.Second):
		t.Fatal("the role does not try the tcp port again")
	}

	// the status is published before the role builds anything, so the barrier
	// above proves none of it exists rather than waiting for it not to appear
	if udpPorts := testDrainUdpBinds(udpBinds); 0 < len(udpPorts) {
		t.Fatalf("the role bound udp %v without tcp 443", udpPorts)
	}
	extender := fixture.extender()
	if extender == nil {
		t.Fatal("the role is not waiting for its tcp port")
	}
	if extender.currentServer() != nil || extender.currentActivator() != nil {
		t.Fatal("the role started its server or its activation without tcp 443")
	}
	if extenderNode := fixture.networkSpace.getExtenderNode(); extenderNode.role() == gossip.NodeRoleExtender {
		t.Fatal("the role became an extender node without tcp 443")
	}
	if stats := fixture.device.GetExtenderStats(); stats != nil {
		t.Fatalf("stats = %+v, expected none from a role that is off", stats)
	}
	if posts := fixture.operator.postCount(4) + fixture.operator.postCount(6); posts != 0 {
		t.Fatalf("activations = %d, expected none", posts)
	}
}

// A role off for want of tcp 443 starts once the port is free: the next retry
// binds it, then the udp carriers, and the role activates as any other. This
// is how the role moves to another provider process of the host when the one
// that held the port exits (G2).
func TestDeviceLocalProviderExtenderStartsWhenTheTcpPortFrees(t *testing.T) {
	tcpPort := testFreeTcpPort(t)
	releaseTcpPort := testHoldTcpPort(t, tcpPort)
	udpBinds := make(chan int, 64)
	waits := make(chan time.Duration, 16)
	retry := make(chan time.Time)
	fixture := newTestProvideExtenderFixture(t, func(settings *deviceLocalExtenderSettings) {
		settings.TcpPort = tcpPort
		testRecordUdpBinds(settings, udpBinds)
		testTcpRetry(settings, waits, retry)
	})

	fixture.waitStatus("off for want of tcp", func(status *ExtenderProvideStatus) bool {
		return status.ErrorCase == ExtenderProvideErrorTcpUnavailable
	})
	<-waits
	if udpPorts := testDrainUdpBinds(udpBinds); 0 < len(udpPorts) {
		t.Fatalf("the role bound udp %v without tcp 443", udpPorts)
	}

	// the holder exits, and the next retry takes the port
	releaseTcpPort()
	retry <- time.Time{}

	fixture.waitPass()
	expectedCarriers := []string{
		connect.ExtenderCarrierTcp,
		connect.ExtenderCarrierQuic,
		connect.ExtenderCarrierDns,
	}
	for range 2 {
		post := fixture.waitPost()
		if !slices.Equal(post.args.Carriers, expectedCarriers) {
			t.Fatalf("v%d carriers = %v, expected %v", post.ipVersion, post.args.Carriers, expectedCarriers)
		}
		if post.args.TcpPort != tcpPort {
			t.Fatalf("v%d tcp port = %d, expected %d", post.ipVersion, post.args.TcpPort, tcpPort)
		}
	}
	status := fixture.waitStatus("running on the freed port", func(status *ExtenderProvideStatus) bool {
		return status.Enabled && status.Listening && status.ActivatedV4
	})
	if status.TcpUnavailableError != "" || status.ListenError != "" || status.ErrorCase != "" {
		t.Fatalf("status = %+v, expected the tcp failure cleared", status)
	}
	udpPorts := testDrainUdpBinds(udpBinds)
	for _, port := range []int{fixture.udpPort, fixture.dnsPort} {
		if !slices.Contains(udpPorts, port) {
			t.Fatalf("udp binds = %v, expected %d once the role held tcp 443", udpPorts, port)
		}
	}
}

// With tcp 443 held, an optional carrier that cannot bind is left out and the
// role runs without it (G2): udp 4053 held by another process leaves the
// extender on tcp and udp 443, advertising no dns carrier.
func TestDeviceLocalProviderExtenderRunsWithoutATakenDnsPort(t *testing.T) {
	dnsPort := testFreeUdpPort(t)
	dnsConn, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(dnsPort)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dnsConn.Close() })
	fixture := newTestProvideExtenderFixture(t, func(settings *deviceLocalExtenderSettings) {
		settings.DnsPort = dnsPort
	})

	fixture.waitPass()
	expectedCarriers := []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierQuic}
	for range 2 {
		post := fixture.waitPost()
		if !slices.Equal(post.args.Carriers, expectedCarriers) {
			t.Fatalf("v%d carriers = %v, expected %v", post.ipVersion, post.args.Carriers, expectedCarriers)
		}
		if 0 < len(post.args.DnsPorts) {
			t.Fatalf("v%d dns ports = %v, expected none", post.ipVersion, post.args.DnsPorts)
		}
	}
	status := fixture.waitStatus("running without the dns carrier", func(status *ExtenderProvideStatus) bool {
		return status.Enabled && status.Listening && status.ActivatedV4
	})
	if !strings.Contains(status.ListenError, connect.ExtenderCarrierDns+": ") {
		t.Fatalf("listen error = %q, expected the dns carrier named", status.ListenError)
	}
	if status.DnsPorts != "" || status.TcpUnavailableError != "" {
		t.Fatalf("status = %+v, expected no dns port and the tcp port held", status)
	}
}
