package sdk

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// dmcaReclaimFlowTtl is the DMCA flow idle TTL for the mux/security test. It must
// exceed the reclaim-phase fill duration (even under -race, where 40k sends take a
// few seconds) so the flow table accumulates fully before the idle scan reclaims
// it. The scan interval is FlowTtl/4 clamped to a 5s minimum.
const dmcaReclaimFlowTtl = 15 * time.Second

// TestDeviceLocalMuxSecurityLoadStability drives packets through a real DeviceLocal
// over the MUX UPGRADE path (not routeLocal) so that the client-egress security
// policy runs, and it exercises BOTH the CFAA and DMCA inspection paths across
// progressively longer load. It then checks goroutines/heap stay flat and return
// to baseline after teardown -- covering the DMCA flow-table + idle-scan goroutine
// and the mux's own machinery.
//
// Coverage:
//   - CFAA: abused-port drop, system-port allow.
//   - DMCA classify branches: all five BitTorrent signatures (wire handshake, HTTP
//     tracker, DHT KRPC, UDP tracker, uTP), unsanctioned-encrypted drop for both
//     UDP (first datagram is the flow start) and TCP (SYN observed), the four
//     web-standard rescues (TLS/QUIC/DTLS/STUN), and plaintext HTTP allow. Each is
//     confirmed via the egress SecurityPolicyStats counters (a distinct
//     result+port per branch).
//   - DMCA idle-scan TTL reclaim: a short FlowTtl plus a large fill-then-idle phase
//     verifies the scan drains the flow table (heap drops) while the device stays
//     up; the capacity-LRU (MaxFlows) bounds the table under the fill.
//
// Topology (fully in-process, no platform/credentials, no real network egress):
//
//	crafted IP packet -> DeviceLocal.SendPacket
//	  -> upgradeMux (forwards non-DNS/HTTP)  -> RemoteUserNatMultiClient.SendPacket
//	  -> securityPolicy.InspectEgress (CFAA then DMCA)  -> Drop/Incident (counted)
//	                                                    \-> Allow -> in-process provider (echo)
//
// Why a separate test from TestDeviceLocalLoadStability: the security policy drops
// any non-public-unicast destination as an Incident BEFORE CFAA/DMCA run
// (ip_security.go isPublicUnicast gate), so a loopback echo can't be an allowed
// round-trip while the policy is active. This test therefore sends to a public
// TEST-NET-3 address (203.0.113.7, unrouteable so nothing leaves the host) and the
// probes are inspected/dropped at the client rather than echoed.
//
// The mux path is reached by:
//   - settings.GeneratorFunc -> an in-process MultiClientGenerator (the platform
//     API generator needs a JWT); this is what lets SetConnectLocation build a real
//     RemoteUserNatMultiClient in-process.
//   - the multi-client runs as ProvideMode_Public (device_local.go), so
//     minRelationship = max(Network, Public) = Public and the policy's
//     ProvideMode_Network bypass is NOT taken -- CFAA/DMCA run.
//   - routeLocal is off (AllowProvider=false), so SetLocalSecurityBypass is off and
//     dropped packets are really dropped (and counted) rather than routed locally.
func TestDeviceLocalMuxSecurityLoadStability(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping DeviceLocal mux/security load stability test in -short mode")
	}

	const (
		measureIterations = 6
		roundsStep        = 2 // probe-set rounds added per iteration (progressively longer)
		// distinct source ports per probe type. This is a FIXED, bounded set reused
		// every iteration, so the DMCA flow table reaches a steady size and the
		// footprint is invariant to iteration length -- any heap climb is a leak.
		srcPortFanout = 8
		warmupRounds  = measureIterations * roundsStep // heaviest load before baseline
		warmFromIndex = 1                              // iteration 2 (exclude first-touch)

		goroutineIterationDrift    = 10
		heapGrowthBandBytes        = 6 << 20 // 6 MiB
		goroutineBaselineTolerance = 10
		goroutineStackTolerance    = 5
		fdBaselineTolerance        = 8
		heapBaselineBandBytes      = 16 << 20 // 16 MiB
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}

	newEnv := func() (*DeviceLocal, func()) {
		// per-env in-process provider + generator so teardown releases everything
		// and there is no cross-cycle accumulation in a shared provider.
		providerCtx, providerCancel := context.WithCancel(ctx)
		providerClient, closeProvider := startEchoProviderClient(t, providerCtx)
		generator := newMuxSecurityGenerator(providerClient)

		settings := DefaultDeviceLocalSettings()
		settings.DisableLogging = true
		settings.Verbose = false
		// no device-side provider: the mux path uses the remote multi-client, and
		// excluding the provider avoids unrelated platform-transport goroutine churn.
		settings.AllowProvider = false
		// route packets through an in-process multi-client instead of the platform api
		settings.GeneratorFunc = func(specs []*connect.ProviderSpec) connect.MultiClientGenerator {
			return generator
		}

		device, err := newDeviceLocalWithOverrides(
			networkSpace, byJwt, "", "", "", NewId(), settings, connect.NewId(),
		)
		if err != nil {
			t.Fatalf("new device: %v", err)
		}

		// register a receive callback so the return (ingress) path is exercised
		unsub := device.AddReceivePacketCallback(func(
			source connect.TransferPath,
			provideMode protocol.ProvideMode,
			ipPath *connect.IpPath,
			packet []byte,
		) {
		})

		// Use a short DMCA FlowTtl so the idle-scan actually reclaims flows within
		// the test (the scan interval is FlowTtl/4 clamped to a 5s minimum). CFAA
		// and web-standard settings stay at their defaults, so the full chain runs.
		device.SetClientSecurityPolicyGenerator(func(policyCtx context.Context, stats *connect.SecurityPolicyStatsCollector) connect.SecurityPolicy {
			dmca := connect.DefaultDmcaSecurityPolicySettings()
			// Long enough that the whole reclaim fill (even under -race) completes
			// before any flow is idle past it, so the table accumulates fully; the
			// reclaim phase then idles past this to trigger the scan.
			dmca.FlowTtl = dmcaReclaimFlowTtl
			return connect.NewSecurityPolicy(
				policyCtx,
				connect.DefaultCfaaSecurityPolicySettings(),
				dmca,
				connect.DefaultWebStandardSettings(),
				stats,
			)
		})

		// build the mux + remote multi-client and publish route.upgradeMux
		device.SetConnectLocation(&ConnectLocation{
			ConnectLocationId: &ConnectLocationId{BestAvailable: true},
		})
		// keep the security policy active (no local-security bypass)
		device.SetRouteLocal(false)

		teardown := func() {
			unsub()
			device.Close()  // closes the mux + multi-client (cancels generated clients)
			closeProvider() // release the in-process provider's goroutines
			providerCancel()
		}
		return device, teardown
	}

	// warm up with the heaviest load so pools / mux / policy machinery reach steady
	// state before the baseline (so return-to-baseline is achievable).
	func() {
		device, teardown := newEnv()
		defer teardown()
		sendSecurityProbes(device, warmupRounds, srcPortFanout)
	}()

	baseGoroutines, baseHeap := sampleStable()
	baseStacks := captureGoroutineStacks()
	baseFds := openFdCount()
	t.Logf("baseline: goroutines=%d heap=%s fds=%d", baseGoroutines, humanBytes(baseHeap), baseFds)

	device, teardown := newEnv()

	gSamples := make([]int, measureIterations)
	hSamples := make([]uint64, measureIterations)
	for i := 0; i < measureIterations; i++ {
		rounds := (i + 1) * roundsStep
		start := time.Now()
		sendSecurityProbes(device, rounds, srcPortFanout)
		elapsed := time.Since(start)

		g, h := sampleStable()
		gSamples[i] = g
		hSamples[i] = h
		t.Logf("iteration %d: rounds=%d goroutines=%d heap=%s (%v)",
			i+1, rounds, g, humanBytes(h), elapsed.Round(time.Millisecond))
	}

	// confirm every security path was actually traversed. Ports are chosen so each
	// (result, port) can only be produced by one detector/branch:
	//   Drop@6881 -> CFAA abused-port drop
	//   Allow@123 -> CFAA system-port allow (DMCA skipped)
	//   Incident@51413 -> DMCA BitTorrent signature
	//   Drop@50000 -> DMCA encrypted (CFAA passes >=1024)
	//   Allow@8443 -> DMCA web-standard rescue of a TLS flow
	stats := device.egressSecurityPolicyStats(false)
	D, A, I := connect.SecurityPolicyResultDrop, connect.SecurityPolicyResultAllow, connect.SecurityPolicyResultIncident
	tcp, udp := connect.IpProtocolTcp, connect.IpProtocolUdp
	assertSecurityPathFired(t, stats, D, tcp, 6881, "CFAA abused-port drop")
	assertSecurityPathFired(t, stats, A, udp, 123, "CFAA system-port allow")
	assertSecurityPathFired(t, stats, I, tcp, 51413, "DMCA BitTorrent wire handshake")
	assertSecurityPathFired(t, stats, I, tcp, 51414, "DMCA BitTorrent HTTP tracker")
	assertSecurityPathFired(t, stats, I, udp, 51415, "DMCA BitTorrent DHT KRPC")
	assertSecurityPathFired(t, stats, I, udp, 51416, "DMCA BitTorrent UDP tracker")
	assertSecurityPathFired(t, stats, I, udp, 51417, "DMCA BitTorrent uTP")
	assertSecurityPathFired(t, stats, D, udp, 50000, "DMCA encrypted (UDP)")
	assertSecurityPathFired(t, stats, D, tcp, 50001, "DMCA encrypted (TCP)")
	assertSecurityPathFired(t, stats, A, tcp, 8443, "DMCA web-standard TLS")
	assertSecurityPathFired(t, stats, A, udp, 8444, "DMCA web-standard QUIC")
	assertSecurityPathFired(t, stats, A, udp, 8445, "DMCA web-standard DTLS")
	assertSecurityPathFired(t, stats, A, udp, 8446, "DMCA web-standard STUN")
	assertSecurityPathFired(t, stats, A, tcp, 8080, "DMCA plaintext HTTP")

	// goroutines are invariant to iteration length; they must stay flat.
	refG := gSamples[warmFromIndex]
	for i := warmFromIndex; i < measureIterations; i++ {
		if gSamples[i] > refG+goroutineIterationDrift {
			t.Errorf("iteration %d goroutines=%d drifted above iteration %d=%d (tol +%d)",
				i+1, gSamples[i], warmFromIndex+1, refG, goroutineIterationDrift)
		}
	}

	// heap must not climb as the iterations get longer (bounded flow set).
	minWarmHeap, maxWarmHeap := hSamples[warmFromIndex], hSamples[warmFromIndex]
	for i := warmFromIndex; i < measureIterations; i++ {
		if hSamples[i] < minWarmHeap {
			minWarmHeap = hSamples[i]
		}
		if hSamples[i] > maxWarmHeap {
			maxWarmHeap = hSamples[i]
		}
	}
	t.Logf("heap across warm iterations %d..%d: first=%s last=%s min=%s max=%s (growth band +%s)",
		warmFromIndex+1, measureIterations, humanBytes(hSamples[warmFromIndex]),
		humanBytes(hSamples[measureIterations-1]), humanBytes(minWarmHeap), humanBytes(maxWarmHeap),
		humanBytes(heapGrowthBandBytes))
	if maxWarmHeap > minWarmHeap+heapGrowthBandBytes {
		t.Errorf("heap climbed as iterations lengthened: max=%s min=%s spread=%s exceeds band +%s",
			humanBytes(maxWarmHeap), humanBytes(minWarmHeap),
			humanBytes(maxWarmHeap-minWarmHeap), humanBytes(heapGrowthBandBytes))
	}

	// --- exercise the real DMCA idle-scan TTL reclaim ---
	// Fill the flow table with many distinct fresh flows (BitTorrent handshakes ->
	// Incident, so each is tracked but dropped rather than egressed), then idle past
	// FlowTtl + the (>=5s clamped) scan interval and confirm the scan drains the
	// table: live heap drops back while the device stays up. A broken reclaim leaves
	// the flows (and heap) resident. The capacity-LRU (MaxFlows) is left at its
	// default, so reclaimFlows stays below it and this isolates the time-based scan.
	const (
		reclaimFlows        = 40000
		reclaimIdle         = dmcaReclaimFlowTtl + 7*time.Second // > FlowTtl + scan interval(>=5s)
		reclaimFillMinBytes = 3 << 20                            // the fill must build a substantial flow table
		reclaimMinDropBytes = 2 << 20                            // and the idle scan must reclaim most of it
	)
	fillStart := time.Now()
	fillDmcaFlows(device, reclaimFlows)
	fillElapsed := time.Since(fillStart)
	loadedHeap := sampleHeap()
	if fillElapsed >= dmcaReclaimFlowTtl {
		t.Errorf("reclaim fill (%v) exceeded FlowTtl (%v); flows evicted mid-fill -- raise dmcaReclaimFlowTtl", fillElapsed, dmcaReclaimFlowTtl)
	}
	if loadedHeap < baseHeap+reclaimFillMinBytes {
		t.Errorf("reclaim fill did not build a flow table: loaded=%s base=%s (want >= base + %s)",
			humanBytes(loadedHeap), humanBytes(baseHeap), humanBytes(reclaimFillMinBytes))
	}
	time.Sleep(reclaimIdle)
	reclaimedHeap := sampleHeap()
	t.Logf("DMCA TTL reclaim: filled %d flows in %v, heap %s -> after %v idle %s (base %s)",
		reclaimFlows, fillElapsed.Round(time.Millisecond), humanBytes(loadedHeap), reclaimIdle,
		humanBytes(reclaimedHeap), humanBytes(baseHeap))
	// Assert the idle-scan actually reclaimed the flow values only without -race,
	// where the live-heap delta is reliable. Under -race the delta is dominated by
	// allocator/GC noise, so the no-leak proof there is the exact return-to-baseline
	// after teardown (checked below) plus the fill-built-a-table check above.
	if !raceEnabled && int64(loadedHeap)-int64(reclaimedHeap) < reclaimMinDropBytes {
		t.Errorf("DMCA idle-scan did not reclaim the flow table: loaded=%s reclaimed=%s (want drop >= %s)",
			humanBytes(loadedHeap), humanBytes(reclaimedHeap), humanBytes(reclaimMinDropBytes))
	}

	// teardown and confirm return to baseline (the DMCA idle-scan goroutine, the
	// mux's internal tun/DoH goroutines, and the flow tables must all release).
	teardown()
	finalGoroutines, finalHeap := sampleStable()
	finalFds := openFdCount()
	t.Logf("post-teardown: goroutines=%d heap=%s fds=%d", finalGoroutines, humanBytes(finalHeap), finalFds)

	if finalGoroutines > baseGoroutines+goroutineBaselineTolerance {
		t.Errorf("goroutines did not return to baseline: final=%d baseline=%d (tol +%d)",
			finalGoroutines, baseGoroutines, goroutineBaselineTolerance)
	}
	reportGoroutineLeaks(t, baseStacks, captureGoroutineStacks(), goroutineStackTolerance)
	if baseFds >= 0 && finalFds > baseFds+fdBaselineTolerance {
		t.Errorf("file descriptors did not return to baseline: final=%d baseline=%d (tol +%d)",
			finalFds, baseFds, fdBaselineTolerance)
	}
	if finalHeap > baseHeap+heapBaselineBandBytes {
		t.Errorf("heap did not return to baseline: final=%s baseline=%s (tol +%s)",
			humanBytes(finalHeap), humanBytes(baseHeap), humanBytes(heapBaselineBandBytes))
	}
}

// securityProbe is one packet template aimed at a specific policy branch.
type securityProbe struct {
	proto   connect.IpProtocol
	dstPort int
	syn     bool
	payload []byte
	repeat  int // packets per flow (the encrypted UDP verdict needs a few)
}

func securityProbes() []securityProbe {
	return []securityProbe{
		// CFAA: static endpoint reputation (stateless)
		{connect.IpProtocolTcp, 6881, true, nil, 1},                 // abused port -> Drop
		{connect.IpProtocolUdp, 123, false, []byte("ntp-probe"), 1}, // system port -> Allow
		// DMCA BitTorrent signatures -> Incident (each a distinct wire form)
		{connect.IpProtocolTcp, 51413, false, bittorrentHandshake(), 1},         // BEP 3 wire handshake
		{connect.IpProtocolTcp, 51414, false, bittorrentHttpTracker(), 1},       // BEP 3 HTTP tracker GET
		{connect.IpProtocolUdp, 51415, false, bittorrentDhtPing(), 1},           // BEP 5 DHT KRPC
		{connect.IpProtocolUdp, 51416, false, bittorrentUdpTrackerConnect(), 1}, // BEP 15 UDP tracker
		{connect.IpProtocolUdp, 51417, false, bittorrentUtpHandshake(), 1},      // BEP 29 uTP + handshake
		// DMCA unsanctioned-encrypted -> Drop
		{connect.IpProtocolUdp, 50000, false, encryptedLikePayload(512), 4}, // UDP: first datagram is the start
		{connect.IpProtocolTcp, 50001, true, encryptedLikePayload(512), 4},  // TCP: SYN observed (all SYN)
		// DMCA web-standard rescue of an encrypted-looking opener -> Allow
		{connect.IpProtocolTcp, 8443, false, tlsClientHello(), 1},  // TLS
		{connect.IpProtocolUdp, 8444, false, quicInitial(), 1},     // QUIC
		{connect.IpProtocolUdp, 8445, false, dtlsClientHello(), 1}, // DTLS
		{connect.IpProtocolUdp, 8446, false, stunBinding(), 1},     // STUN
		// DMCA plaintext HTTP request -> Allow
		{connect.IpProtocolTcp, 8080, false, httpPlaintextRequest(), 1},
	}
}

// sendSecurityProbes injects the probe set `rounds` times over a fixed set of
// `srcPortFanout` source ports per probe (a bounded flow set). All probes target a
// public TEST-NET-3 destination so CFAA/DMCA run; nothing leaves the host.
func sendSecurityProbes(device *DeviceLocal, rounds int, srcPortFanout int) {
	srcIP := net.ParseIP("10.0.0.1")
	dstIP := net.ParseIP("203.0.113.7") // TEST-NET-3: public unicast, unrouteable
	probes := securityProbes()
	for r := 0; r < rounds; r++ {
		for f := 0; f < srcPortFanout; f++ {
			for pi, probe := range probes {
				// distinct, stable source port per (probe, fanout) so flows are
				// distinct across probes but reused across rounds/iterations
				srcPort := 20000 + pi*1000 + f
				for k := 0; k < probe.repeat; k++ {
					pkt := craftIpv4Packet(probe.proto, srcIP, srcPort, dstIP, probe.dstPort, probe.syn, probe.payload)
					if pkt == nil {
						continue
					}
					device.SendPacket(pkt, int32(len(pkt)))
				}
			}
		}
	}
}

func assertSecurityPathFired(t *testing.T, stats connect.SecurityPolicyStats, result connect.SecurityPolicyResult, proto connect.IpProtocol, port int, label string) {
	t.Helper()
	key := connect.SecurityDestination{Version: 4, Protocol: proto, Ip: "", Port: port}
	if n := stats[result][key]; n == 0 {
		t.Errorf("security path %q not exercised: no %v at port %d", label, result, port)
	} else {
		t.Logf("security path %q: %v@%d count=%d", label, result, port, n)
	}
}

// craftIpv4Packet builds a valid IPv4 TCP/UDP packet with the given 5-tuple, SYN
// flag, and payload, using gopacket (checksums + lengths filled).
func craftIpv4Packet(proto connect.IpProtocol, srcIP net.IP, srcPort int, dstIP net.IP, dstPort int, syn bool, payload []byte) []byte {
	ip := &layers.IPv4{
		Version: 4,
		TTL:     64,
		SrcIP:   srcIP.To4(),
		DstIP:   dstIP.To4(),
	}
	var transport gopacket.SerializableLayer
	switch proto {
	case connect.IpProtocolTcp:
		ip.Protocol = layers.IPProtocolTCP
		tcp := &layers.TCP{
			SrcPort: layers.TCPPort(srcPort),
			DstPort: layers.TCPPort(dstPort),
			SYN:     syn,
			Seq:     1,
			Window:  65535,
		}
		tcp.SetNetworkLayerForChecksum(ip)
		transport = tcp
	case connect.IpProtocolUdp:
		ip.Protocol = layers.IPProtocolUDP
		udp := &layers.UDP{
			SrcPort: layers.UDPPort(srcPort),
			DstPort: layers.UDPPort(dstPort),
		}
		udp.SetNetworkLayerForChecksum(ip)
		transport = udp
	default:
		return nil
	}
	buf := gopacket.NewSerializeBuffer()
	if err := gopacket.SerializeLayers(
		buf,
		gopacket.SerializeOptions{ComputeChecksums: true, FixLengths: true},
		ip, transport, gopacket.Payload(payload),
	); err != nil {
		return nil
	}
	return append([]byte(nil), buf.Bytes()...)
}

// bittorrentHandshake is the 68-byte BitTorrent handshake ("\x13BitTorrent protocol"
// + reserved/infohash/peerid), which the DMCA detector reports as an incident.
func bittorrentHandshake() []byte {
	const pstr = "BitTorrent protocol"
	b := make([]byte, 68)
	b[0] = byte(len(pstr))
	copy(b[1:], pstr)
	return b
}

// encryptedLikePayload returns bytes that satisfy the DMCA "looks encrypted"
// heuristic (uniform byte distribution, ~0.5 popcount ratio, low printable
// fraction, high entropy).
func encryptedLikePayload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 256)
	}
	return b
}

// tlsClientHello is a minimal TLS ClientHello record header the web-standard
// detector recognizes, so an otherwise encrypted-looking flow is allowed.
func tlsClientHello() []byte {
	return []byte{0x16, 0x03, 0x01, 0x00, 0x2a, 0x01, 0x00, 0x00, 0x26}
}

// bittorrentHttpTracker is a BEP 3 HTTP-tracker GET (info_hash + /announce).
func bittorrentHttpTracker() []byte {
	return []byte("GET /announce?info_hash=%01%02%03&peer_id=x HTTP/1.1\r\nHost: t\r\n\r\n")
}

// bittorrentDhtPing is a BEP 5 DHT KRPC ping (bencode).
func bittorrentDhtPing() []byte {
	return []byte("d1:ad2:id20:abcdefghij0123456789e1:q4:ping1:t2:aa1:y1:qe")
}

// bittorrentUdpTrackerConnect is a BEP 15 UDP-tracker connect request
// (protocol-id magic + action 0 + transaction id).
func bittorrentUdpTrackerConnect() []byte {
	magic := []byte{0x00, 0x00, 0x04, 0x17, 0x27, 0x10, 0x19, 0x80}
	return append(magic, 0, 0, 0, 0, 0x12, 0x34, 0x56, 0x78)
}

// bittorrentUtpHandshake is a BEP 29 uTP v1 header carrying a plaintext handshake.
func bittorrentUtpHandshake() []byte {
	utp := append([]byte{0x01, 0x00}, make([]byte, 18)...)
	return append(utp, bittorrentHandshake()...)
}

// quicInitial is a QUIC v1 long-header Initial (header-form + fixed bit + version).
func quicInitial() []byte {
	return []byte{0xc0, 0x00, 0x00, 0x00, 0x01, 0x08, 0xde, 0xad, 0xbe, 0xef}
}

// dtlsClientHello is a DTLS 1.2 record header carrying a ClientHello.
func dtlsClientHello() []byte {
	return []byte{0x16, 0xfe, 0xfd, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x02, 0x01}
}

// stunBinding is a STUN binding request (type + zero length + magic cookie).
func stunBinding() []byte {
	b := make([]byte, 20)
	b[0], b[1] = 0x00, 0x01
	b[4], b[5], b[6], b[7] = 0x21, 0x12, 0xa4, 0x42
	return b
}

// httpPlaintextRequest is a raw HTTP request line, which the DMCA detector allows
// definitively (media/radio streaming relies on plaintext HTTP).
func httpPlaintextRequest() []byte {
	return []byte("GET / HTTP/1.1\r\nHost: example.com\r\nUser-Agent: test\r\n\r\n")
}

// fillDmcaFlows creates n distinct tracked DMCA flows by sending one BitTorrent
// handshake per fresh source port. BitTorrent -> Incident, so each flow is tracked
// but dropped (not egressed), and stays resident until the idle scan reclaims it.
func fillDmcaFlows(device *DeviceLocal, n int) {
	srcIP := net.ParseIP("10.0.0.2")
	dstIP := net.ParseIP("203.0.113.7")
	payload := bittorrentHandshake()
	for i := 0; i < n; i++ {
		srcPort := 1024 + i
		pkt := craftIpv4Packet(connect.IpProtocolTcp, srcIP, srcPort, dstIP, 51413, false, payload)
		if pkt != nil {
			device.SendPacket(pkt, int32(len(pkt)))
		}
	}
}

// startEchoProviderClient builds an in-process provider Client that echoes each
// IpPacketToProvider back with the path reversed (mirrors the connect mux
// integration test). Allowed flows round-trip through it.
func startEchoProviderClient(t *testing.T, ctx context.Context) (*connect.Client, func()) {
	clientSettings := connect.DefaultClientSettings()
	clientSettings.SendBufferSettings.SequenceBufferSize = 0
	clientSettings.SendBufferSettings.AckBufferSize = 0
	clientSettings.ReceiveBufferSettings.SequenceBufferSize = 0
	clientSettings.ForwardBufferSettings.SequenceBufferSize = 0

	providerClient := connect.NewClient(ctx, connect.NewId(), connect.NewNoContractClientOob(), clientSettings)
	providerClient.AddReceiveCallback(func(src connect.TransferPath, frames []*protocol.Frame, provideMode protocol.ProvideMode) {
		for _, frame := range frames {
			msg, err := connect.FromFrame(frame)
			if err != nil {
				continue
			}
			toProvider, ok := msg.(*protocol.IpPacketToProvider)
			if !ok {
				continue
			}
			ipPath, payload, err := connect.ParseIpPathWithPayload(toProvider.IpPacket.PacketBytes)
			if err != nil {
				continue
			}
			reversed := craftIpv4Packet(ipPath.Protocol, ipPath.DestinationIp, ipPath.DestinationPort, ipPath.SourceIp, ipPath.SourcePort, false, payload)
			if reversed == nil {
				continue
			}
			fromProvider := &protocol.IpPacketFromProvider{
				IpPacket: &protocol.IpPacket{PacketBytes: reversed},
			}
			echoFrame, err := connect.ToFrame(fromProvider, connect.DefaultProtocolVersion)
			if err != nil {
				continue
			}
			providerClient.SendWithTimeout(echoFrame, src.Reverse(), func(err error) {}, -1)
		}
	})
	return providerClient, func() { providerClient.Cancel() }
}

// muxSecurityGenerator is an in-process connect.MultiClientGenerator that
// terminates every client on a single shared provider Client via in-memory
// gateway transports (mirrors connect's TestMultiClientGenerator, which is not
// reachable from package sdk).
type muxSecurityGenerator struct {
	providerClient *connect.Client
	mu             sync.Mutex
	unsubs         map[*connect.Client]func()
}

func newMuxSecurityGenerator(providerClient *connect.Client) *muxSecurityGenerator {
	return &muxSecurityGenerator{
		providerClient: providerClient,
		unsubs:         map[*connect.Client]func(){},
	}
}

func (g *muxSecurityGenerator) NextDestinations(count int, excludeDestinations []connect.MultiHopId, rankMode string) (map[connect.MultiHopId]connect.DestinationStats, error) {
	next := map[connect.MultiHopId]connect.DestinationStats{}
	for _, d := range excludeDestinations {
		if 0 < d.Len() && d.Tail() == g.providerClient.ClientId() {
			return next, nil
		}
	}
	next[connect.RequireMultiHopId(g.providerClient.ClientId())] = connect.DestinationStats{
		EstimatedBytesPerSecond: connect.ByteCount(0),
		Tier:                    0,
	}
	return next, nil
}

func (g *muxSecurityGenerator) NewClientArgs() (*connect.MultiClientGeneratorClientArgs, error) {
	return &connect.MultiClientGeneratorClientArgs{ClientId: connect.NewId(), ClientAuth: nil}, nil
}

func (g *muxSecurityGenerator) RemoveClientArgs(args *connect.MultiClientGeneratorClientArgs) {}

func (g *muxSecurityGenerator) RemoveClientWithArgs(client *connect.Client, args *connect.MultiClientGeneratorClientArgs) {
	g.mu.Lock()
	unsub, ok := g.unsubs[client]
	if ok {
		delete(g.unsubs, client)
	}
	g.mu.Unlock()
	if ok {
		unsub()
	}
}

func (g *muxSecurityGenerator) NewClientSettings() *connect.ClientSettings {
	settings := connect.DefaultClientSettings()
	settings.SendBufferSettings.SequenceBufferSize = 0
	settings.SendBufferSettings.AckBufferSize = 0
	settings.ReceiveBufferSettings.SequenceBufferSize = 0
	settings.ForwardBufferSettings.SequenceBufferSize = 0
	return settings
}

func (g *muxSecurityGenerator) NewClient(ctx context.Context, args *connect.MultiClientGeneratorClientArgs, clientSettings *connect.ClientSettings) (*connect.Client, error) {
	client := connect.NewClient(ctx, args.ClientId, connect.NewNoContractClientOob(), clientSettings)

	routeSend := make(chan []byte)
	routeReceive := make(chan []byte)

	transportSend := connect.NewSendGatewayTransport()
	transportReceive := connect.NewReceiveGatewayTransport()
	client.RouteManager().UpdateTransport(transportSend, []connect.Route{routeSend})
	client.RouteManager().UpdateTransport(transportReceive, []connect.Route{routeReceive})
	client.ContractManager().AddNoContractPeer(g.providerClient.ClientId())

	providerTransportSend := connect.NewSendClientTransport(connect.DestinationId(args.ClientId))
	providerTransportReceive := connect.NewReceiveGatewayTransport()
	g.providerClient.RouteManager().UpdateTransport(providerTransportReceive, []connect.Route{routeSend})
	g.providerClient.RouteManager().UpdateTransport(providerTransportSend, []connect.Route{routeReceive})
	g.providerClient.ContractManager().AddNoContractPeer(client.ClientId())

	unsub := func() {
		client.RouteManager().RemoveTransport(transportSend)
		client.RouteManager().RemoveTransport(transportReceive)
		g.providerClient.RouteManager().RemoveTransport(providerTransportReceive)
		g.providerClient.RouteManager().RemoveTransport(providerTransportSend)
		// release the generated client's goroutines so teardown returns to baseline
		client.Cancel()
	}
	g.mu.Lock()
	g.unsubs[client] = unsub
	g.mu.Unlock()

	return client, nil
}

func (g *muxSecurityGenerator) FixedDestinationSize() (int, bool) { return 1, true }
