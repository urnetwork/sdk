//go:build !ios && !android && !js

package sdk

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
)

// The two provider extender role states the activation tests never reach
// (EXTENDER.md G2, A8, L2): the app the user put in the feed-only gossip mode,
// which serves the feed without a node and refuses the gossip service, and the
// host where no carrier bound at all.

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

// A host where every carrier bind failed has nothing to activate: the failure
// stands in the status, no activation is posted, and no mesh address is
// advertised (G2, G3, F3).
func TestDeviceLocalProviderExtenderWithNoCarriersNeverActivates(t *testing.T) {
	// hold all three carrier ports before the role reaches them
	tcpPort := testFreeTcpPort(t)
	tcpListener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(tcpPort)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tcpListener.Close() })
	udpPort := testFreeUdpPort(t)
	udpConn, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(udpPort)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { udpConn.Close() })
	dnsPort := testFreeUdpPort(t)
	dnsConn, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(dnsPort)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dnsConn.Close() })

	fixture := newTestProvideExtenderFixture(t, func(settings *deviceLocalExtenderSettings) {
		settings.TcpPort = tcpPort
		settings.UdpPort = udpPort
		settings.DnsPort = dnsPort
	})

	status := fixture.waitStatus("no carrier bound", func(status *ExtenderProvideStatus) bool {
		return status.Enabled && !status.Listening && status.ListenError != ""
	})
	for _, carrier := range []string{
		connect.ExtenderCarrierTcp,
		connect.ExtenderCarrierQuic,
		connect.ExtenderCarrierDns,
	} {
		if !strings.Contains(status.ListenError, carrier+": ") {
			t.Errorf("listen error = %q, expected the %s carrier named", status.ListenError, carrier)
		}
	}
	if status.DnsPorts != "" {
		t.Fatalf("dns ports = %q, expected none", status.DnsPorts)
	}
	if status.ActivatedV4 || status.ActivatedV6 {
		t.Fatalf("status = %+v, expected nothing activated", status)
	}

	// the status is published before the loop would start an activator, so the
	// barrier above proves there is none rather than waiting for one not to
	// appear
	if activator := fixture.extender().currentActivator(); activator != nil {
		t.Fatal("a host with no carrier started an activation loop")
	}
	if posts := fixture.operator.postCount(4) + fixture.operator.postCount(6); posts != 0 {
		t.Fatalf("activations = %d, expected none", posts)
	}
	// the node is the role's, and it advertises nothing: only the tcp carrier
	// carries the mesh (D2)
	extenderNode := fixture.networkSpace.getExtenderNode()
	if extenderNode == nil {
		t.Fatal("the role ran no node")
	}
	if listenAddrs := extenderNode.node.ListenAddrs(); 0 < len(listenAddrs) {
		t.Fatalf("mesh addresses = %v, expected none", listenAddrs)
	}

	// releasing the ports and restarting the role binds them, so the failure
	// above is the ports and not the fixture
	tcpListener.Close()
	udpConn.Close()
	dnsConn.Close()
	fixture.device.SetProvideExtender(false)
	fixture.device.SetProvideExtender(true)
	fixture.waitStatus("bound after the ports were released", func(status *ExtenderProvideStatus) bool {
		return status.Listening && status.ListenError == ""
	})
}
