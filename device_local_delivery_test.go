package sdk

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// TestDeviceLocalMuxSecurityDelivery closes the gap that the mux/security load test
// asserts policy verdict COUNTERS but never that allowed traffic is actually
// DELIVERED and dropped traffic is not. It drives probes through the real mux ->
// multi-client -> in-process echo provider, and asserts on the device's receive
// callback: allowed probes round-trip (echo received), dropped probes never do.
//
// The echo provider reverses each IpPacketToProvider it receives, so an echo can
// only arrive for a probe the client-egress policy ALLOWED (a dropped probe never
// reaches the provider). The returning packet's source port is the probe's original
// destination port, which identifies the probe. This is an end-to-end enforcement
// check, distinct from the egress stat counters.
func TestDeviceLocalMuxSecurityDelivery(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping mux/security delivery test in -short mode")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}

	providerCtx, providerCancel := context.WithCancel(ctx)
	defer providerCancel()
	providerClient, closeProvider := startEchoProviderClient(t, providerCtx)
	defer closeProvider()
	generator := newMuxSecurityGenerator(providerClient)

	settings := DefaultDeviceLocalSettings()
	settings.DisableLogging = true
	settings.Verbose = false
	settings.AllowProvider = false
	settings.GeneratorFunc = func(specs []*connect.ProviderSpec) connect.MultiClientGenerator {
		return generator
	}

	device, err := newDeviceLocalWithOverrides(networkSpace, byJwt, "", "", "", NewId(), settings, connect.NewId())
	if err != nil {
		t.Fatalf("new device: %v", err)
	}
	defer device.Close()

	// count echoes by the returning packet's SOURCE port (= the probe's dst port)
	var mu sync.Mutex
	echoesByPort := map[int]int{}
	unsub := device.AddReceivePacketCallback(func(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte) {
		p, err := connect.ParseIpPath(packet)
		if err != nil {
			return
		}
		mu.Lock()
		echoesByPort[p.SourcePort] += 1
		mu.Unlock()
	})
	defer unsub()

	device.SetConnectLocation(&ConnectLocation{ConnectLocationId: &ConnectLocationId{BestAvailable: true}})
	device.SetRouteLocal(false)

	srcIP := net.ParseIP("10.0.0.5")
	dstIP := net.ParseIP("203.0.113.7") // TEST-NET-3, unrouteable

	// allowed UDP probes: CFAA system port, and web-standard rescues (QUIC/STUN)
	allowed := []struct {
		dstPort int
		payload []byte
	}{
		{123, []byte("ntp-probe")},
		{8444, quicInitial()},
		{8446, stunBinding()},
	}
	// dropped UDP probes: DMCA bittorrent signatures. These are immediate incidents
	// (dropped on the first packet), so every packet must be blocked -- a clean
	// end-to-end assertion. The encrypted-heuristic drop is deliberately NOT used
	// here: it allows a flow's first few datagrams while inspecting (see
	// EncryptedDecisionPackets), which is a state-machine property covered by the
	// connect DMCA tests, not an every-packet delivery block.
	dropped := []struct {
		dstPort int
		payload []byte
	}{
		{51415, bittorrentDhtPing()},           // BEP 5 DHT KRPC
		{51416, bittorrentUdpTrackerConnect()}, // BEP 15 UDP tracker
	}

	// send several rounds so the multi-client flow is warm and echoes have time
	for round := 0; round < 8; round += 1 {
		srcPort := 40000 + round
		for _, a := range allowed {
			pkt := craftIpv4Packet(connect.IpProtocolUdp, srcIP, srcPort, dstIP, a.dstPort, false, a.payload)
			if pkt != nil {
				device.SendPacket(pkt, int32(len(pkt)))
			}
		}
		for _, d := range dropped {
			pkt := craftIpv4Packet(connect.IpProtocolUdp, srcIP, srcPort, dstIP, d.dstPort, false, d.payload)
			if pkt != nil {
				device.SendPacket(pkt, int32(len(pkt)))
			}
		}
	}

	// wait for echoes to settle
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := len(echoesByPort)
		mu.Unlock()
		if got >= len(allowed) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, a := range allowed {
		if echoesByPort[a.dstPort] == 0 {
			t.Errorf("allowed probe dst=%d was not delivered end-to-end (no echo received)", a.dstPort)
		} else {
			t.Logf("allowed probe dst=%d delivered: %d echoes", a.dstPort, echoesByPort[a.dstPort])
		}
	}
	for _, d := range dropped {
		if echoesByPort[d.dstPort] != 0 {
			t.Errorf("dropped probe dst=%d leaked through: %d echoes received (must be 0)", d.dstPort, echoesByPort[d.dstPort])
		}
	}
}
