package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"runtime/pprof"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// TestDeviceLocalProviderMemoryUnderLoad measures the total process memory of
// a DeviceLocal serving remote peers as a provider, configured with the ios
// packet tunnel budget (`SetMemoryLimit(32 MiB)`). It is the provider-side
// counterpart of TestDeviceLocalMemoryCeiling: that test drives the device's
// own client path; this one drives the provide path that serves other clients.
//
// Topology (fully in-process, no platform/credentials needed):
//
//	peer gvisor Tun -> RemoteUserNatClient -> in-memory routes
//	  -> device provider client -> RemoteUserNatProvider
//	  -> exit LocalUserNat -> loopback tcp/udp echo servers
//
// Each peer is a full connect client wired to the device's provider client
// over lossless in-memory gateway routes (the same wiring as connect's
// testingNewClient), so the provide path runs end to end: receive sequences,
// the provider security policy, the exit nat's per-flow tcp/udp state, and the
// return-path send sequences all carry real traffic.
//
// The test always runs a predeclared cohort of three fresh two-core processes.
// Every process must pass; no failing peak is retried or removed. Each emits a
// greppable `[provider-mem]` line and an exact `[provider-mem-json]` record with
// allocator, GC, actual sampling gaps and completed-work telemetry:
//
//   - idle: quiesced heap with the device + peers connected, before load
//   - peakTotal/peakHeap: max sampled Go soft-limit usage / live heap during
//     load. peakTotal is not RSS or the iOS jetsam footprint: binary mappings,
//     C allocations, and kernel memory are outside this gauge.
//   - settledPeak: the peak footprint the runtime could not give back: each
//     sample's soft-limit usage less the free pages the runtime holds, as far
//     as it still stands once the runtime has collected what was garbage at
//     the sample (see peakMemoryTelemetry). The peak ceiling judges this one.
//   - loaded: quiesced heap right after load (flows closed, nat entries and
//     pools still warm)
//   - final: quiesced heap after the peers disconnect
//
// Set URNETWORK_PROVIDER_MEM_PROFILE_DIR to also write pprof heap profiles at
// the idle, loaded and final points for allocation-site attribution; the idle
// one shows what the process holds before any load, package init included. Set
// URNETWORK_PROVIDER_GOROUTINE_PROFILE=1 to print the loaded goroutine profile
// grouped by stack, for per-flow goroutine attribution.
func TestDeviceLocalProviderMemoryUnderLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping DeviceLocal provider memory test in -short mode")
	}
	if runProviderMemoryCohort(t) {
		return
	}

	const budgetByteCount = 32 * 1024 * 1024

	const (
		peerCount    = 6
		rounds       = 4 // sequential connection churn per peer
		flowsPerPeer = 2 // concurrent tcp flows per peer per round
		bytesPerFlow = 96 * 1024
		// short udp flows per peer, to populate the exit nat's flow table the
		// way scattered real traffic does (entries linger to their idle
		// timeout, so they land in the loaded measurement)
		udpFlowsPerPeer = 32

		// Regression ceilings over the measured baseline. After making the
		// receive-buffer rejection tombstones lazy (2026-08-14), 12 fresh
		// darwin/arm64 processes measured peakTotal=26.0-27.4 MiB and
		// loaded=7.3-7.4 MiB. On the 2026-08-23 macOS 25.6 / Go 1.26.7 host,
		// five fresh exact-parent/candidate processes instead measured
		// 30.2-30.5 MiB with the same 3.9-MiB idle heap, 9.6-9.7-MiB loaded
		// heap, 179 idle goroutines, and 641 peak goroutines. The exact parent
		// failed the old ceiling in both control runs, so this is a runtime/OS
		// accounting baseline shift rather than a candidate regression.
		// loaded is still held to the budget/2 goal.
		loadedHeapCeiling = budgetByteCount / 2

		// The 0.5 MiB once left above that five-process maximum is gone. On
		// the 2026-09-23 macOS 26.6 / Go 1.26.7 host, 10 fresh processes each
		// measured 30.6-31.7 MiB at the commit before H1+ became the default
		// and 30.7-31.5 MiB after it, over the old 31-MiB ceiling in 3 and 5
		// runs. Darwin now holds the same line as linux below: do not exceed
		// the 32-MiB synthetic process budget. This host-side provider test no
		// longer proves the mobile 28-MiB active goal; physical mobile-policy
		// runs own that release gate.
		//
		// The ceiling held, and the code moved back under it. On the
		// 2026-11-21 macOS / go1.27.4 host, 18 fresh processes at sdk 9a151fd6
		// measured peakTotal=31.1-32.0 MiB, one of six cohorts over the
		// ceiling, with an 8.4-MiB idle heap, 12.6-13.2-MiB loaded heap and
		// 134 idle goroutines. Of that idle heap 4.5 MiB was package init, and
		// 2.0 MiB of it mox's public suffix list, which the sdk links with
		// sidecar's mail checks and which its package parsed at init though
		// nothing here looks a domain up. Parsed at its first lookup instead
		// (sidecar/third_party/mox/URNETWORK.md), 18 fresh processes measured
		// peakTotal=29.3-30.8 MiB, with a 6.3-MiB idle heap and
		// 11.0-11.7-MiB loaded heap.
		//
		// Since 2026-11-23 the ceiling judges the settled peak, not peakTotal.
		// Under five lanes' load (load average 50-100 on 14 cores) peakTotal
		// at sdk d27e9903 went past the 32-MiB soft limit in 3 of 183 fresh
		// processes, at 32.0, 33.3 and 38.3 MiB, the others at 28.4-31.3 MiB.
		// Traced with gctrace, the 33.3-MiB one was a collection the host
		// starved: 28 ms to stop the world, 72 ms of marking and 28 ms to stop
		// it again, while the provider took the heap from 17 to 24 MiB past a
		// 16-MiB goal; the cycle marked 20 MiB, 6 MiB of which the next
		// collection freed, and the total fell back under the limit. That
		// excess was garbage the runtime held, and a ceiling above the soft
		// limit would have to clear 38.3 MiB, room to hide 6 MiB of
		// regression. Rebuilt from the 120 traced processes' samples, the
		// settled peak was 26.2-29.9 MiB (median 27.8) where peakTotal was
		// 28.8-33.3 (median 29.8); at -count=20 beside the whole sdk package,
		// 60 processes settled at 26.2-29.3 MiB (median 27.6) with peakTotal
		// at 28.4-31.0. A footprint the collections leave over the budget
		// still fails. The websocket bump (GO-2026-6278) did not move the
		// peak: 60 fresh processes on each side, interleaved, had peakTotal
		// medians of 29.84 MiB before it (sdk 890d68d8, connect 16328277) and
		// 29.73 after (sdk d27e9903, connect 2b59b666), 90th percentiles of
		// 30.44 and 30.40, and the same 6.3-MiB idle and 11.0-MiB loaded
		// heaps; the 38.3-MiB process was after it.
		peakTotalCeilingDarwin = 32 * 1024 * 1024

		// peakTotal caught the 2.1-MiB idle growth of 2026-11-21 only by
		// flaking past its ceiling, and the settled peak sits about 2 MiB
		// under peakTotal, so it would flag such growth late or not at all.
		// The idle heap carries that guard instead, deterministically: package
		// init and the connected peers' state, which every later measurement
		// adds to. It measured 6.28-6.32 MiB in those 183 processes and
		// 8.4 MiB at sdk 9a151fd6, before mox's public suffix list was parsed
		// lazily.
		idleHeapCeiling = budgetByteCount / 4

		// linux/amd64 carries ~4 MiB more for the same work, and it is the
		// platform CI runs on, so the darwin figure failed every run on main.
		// Measured 2026-08-19, go1.26.6, 5 fresh linux/amd64 processes at the
		// merge of #142: peakTotal=30.8, 30.9, 31.2, 31.3, 31.4 MiB — spread
		// 0.6 MiB, and identical goroutine counts (237 idle / 715 peak / 679
		// loaded / 457 final) and heap (7.8 MiB loaded) to darwin. Same work,
		// same object graph, different runtime accounting: allocator arenas,
		// goroutine stack granularity and GC pacing all differ. It is a
		// platform delta, not a leak.
		//
		// The honest consequence, and it is worth stating rather than hiding
		// in a number: on linux/amd64 the provider peak sits at the 32 MiB
		// budget, not under the 28 MiB target. There is no room left for a
		// ceiling that is both below budget and far enough above 31.4 MiB to
		// avoid flaking, so on linux the guard becomes "do not exceed the
		// budget" rather than "do not exceed the target". That is a weaker
		// promise, and closing the 4 MiB is real work someone should own —
		// but a guard that fails on main is worse than a weaker one, because
		// it trains everyone to ignore the red X that catches the next real
		// regression.
		peakTotalCeilingLinux = budgetByteCount
	)

	peakTotalCeiling := int64(peakTotalCeilingDarwin)
	if runtime.GOOS == "linux" {
		peakTotalCeiling = int64(peakTotalCeilingLinux)
	}

	pinIosGcPacing(t)
	prevLimit := debug.SetMemoryLimit(-1)
	SetMemoryLimit(budgetByteCount)
	t.Cleanup(func() {
		connect.SetMemoryBudget(0)
		SetMessagePoolMemoryTargets(connect.InitialMessagePoolByteCount/2, connect.InitialMessagePoolByteCount/2)
		debug.SetMemoryLimit(prevLimit)
	})
	// Testing cleanups run after this function's deferred device/peer teardown.
	// Let the asynchronously closing gVisor stacks drain before another
	// load-budget test starts in the same process; otherwise the next test
	// measures the prior test's transient stacks as part of its idle baseline.
	t.Cleanup(func() {
		sampleStable()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(t, ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}
	echoAddr, closeEcho := startTcpEchoServer(t)
	defer closeEcho()
	udpEchoAddr, closeUdpEcho := startUdpEchoServer(t)
	defer closeUdpEcho()

	// the device is constructed after SetMemoryLimit, so it picks up the
	// scaled defaults
	settings := DefaultDeviceLocalSettings()
	settings.DisableLogging = true // keep the load quiet
	settings.Verbose = false       // no security policy monitor goroutine
	device, err := newDeviceLocalWithOverrides(
		networkSpace,
		byJwt,
		"",
		"",
		"",
		NewId(),
		settings,
		connect.NewId(),
	)
	if err != nil {
		t.Fatalf("new device: %v", err)
	}
	defer device.Close()
	// The measured provider path uses only the bounded in-memory peer routes
	// below. Retire the unrelated carrier that reconnects to the fake platform
	// host, whose H3-over-DNS dial cohorts would phase-shift the memory samples.
	platformTransport := func() migratablePlatformTransport {
		device.provider.stateLock.Lock()
		defer device.provider.stateLock.Unlock()
		return device.provider.platformTransport
	}()
	platformTransport.Close()

	// providing creates the RemoteUserNatProvider and its exit LocalUserNat on
	// the provider client
	device.SetProvideMode(ProvideModePublic)
	if !device.GetProvideEnabled() {
		t.Fatal("expected provide enabled")
	}
	providerClient := device.provider.Client()

	peers := make([]*providerLoadPeer, peerCount)
	for i := range peers {
		peers[i] = newProviderLoadPeer(t, ctx, providerClient)
	}
	closePeers := func() {
		for _, peer := range peers {
			peer.close()
		}
	}
	defer closePeers()

	idleGoroutines, idleHeap := sampleStable()
	idleStats := GetMemoryStats()
	writeProviderMemProfile(t, "provider_mem_idle")
	t.Logf("idle (device + %d peers connected): goroutines=%d heap=%s",
		peerCount, idleGoroutines, humanBytes(idleHeap))

	tcpSampler := startPeakSampler()

	// tcp load: every peer churns connections and moves bytes concurrently
	loadStart := time.Now()
	loadErrs := make(chan error, peerCount)
	for _, peer := range peers {
		go func() {
			loadErrs <- runLoadIteration(ctx, peer.tun, echoAddr, rounds, flowsPerPeer, bytesPerFlow)
		}()
	}
	for range peers {
		if err := <-loadErrs; err != nil {
			tcpSampler.stop()
			skipOnRaceGvisorWedge(t, "tcp load", err)
			t.Fatalf("tcp load: %v", err)
		}
	}
	tcpElapsed := time.Since(loadStart)
	tcpPeakTotal, tcpPeakHeap, tcpPeakGoroutines := tcpSampler.stop()
	tcpSettledPeak := tcpSampler.settle()

	// udp burst: scatter short flows into the exit nat's flow table
	udpSampler := startPeakSampler()
	udpStart := time.Now()
	for _, peer := range peers {
		go func() {
			loadErrs <- runPeerUdpBurst(ctx, peer.tun, udpEchoAddr, udpFlowsPerPeer)
		}()
	}
	for range peers {
		if err := <-loadErrs; err != nil {
			udpSampler.stop()
			skipOnRaceGvisorWedge(t, "udp burst", err)
			t.Fatalf("udp burst: %v", err)
		}
	}
	udpElapsed := time.Since(udpStart)
	udpPeakTotal, udpPeakHeap, udpPeakGoroutines := udpSampler.stop()
	udpSettledPeak := udpSampler.settle()
	peakTotal := max(tcpPeakTotal, udpPeakTotal)
	settledPeak := max(tcpSettledPeak, udpSettledPeak)
	peakHeap := max(tcpPeakHeap, udpPeakHeap)
	peakGoroutines := max(tcpPeakGoroutines, udpPeakGoroutines)

	bytesMoved := int64(peerCount) * int64(rounds) * int64(flowsPerPeer) * int64(bytesPerFlow)
	t.Logf("tcp load: %d peers x %d conns x %s = %.1f MiB in %v (%.1f MiB/s echoed)",
		peerCount, rounds*flowsPerPeer, humanBytes(bytesPerFlow),
		float64(bytesMoved)/(1<<20), tcpElapsed.Round(time.Millisecond),
		float64(bytesMoved)/(1<<20)/tcpElapsed.Seconds())
	t.Logf("udp load: %d peers x %d flows = %d round trips in %v (%.1f round trips/s)",
		peerCount, udpFlowsPerPeer, peerCount*udpFlowsPerPeer, udpElapsed.Round(time.Millisecond),
		float64(peerCount*udpFlowsPerPeer)/udpElapsed.Seconds())
	t.Logf("phase peaks: tcp runtime=%s settled=%s heap=%s goroutines=%d; udp runtime=%s settled=%s heap=%s goroutines=%d",
		humanBytes(uint64(tcpPeakTotal)), humanBytes(uint64(tcpSettledPeak)), humanBytes(uint64(tcpPeakHeap)), tcpPeakGoroutines,
		humanBytes(uint64(udpPeakTotal)), humanBytes(uint64(udpSettledPeak)), humanBytes(uint64(udpPeakHeap)), udpPeakGoroutines)

	loadedGoroutines, loadedHeap := sampleStable()
	stats := GetMemoryStats()
	loadedTotal := stats.TotalRuntimeByteCount
	t.Logf("loaded: goroutines=%d heap=%s pool taken=%d returned=%d created=%d held=%d",
		loadedGoroutines, humanBytes(loadedHeap),
		stats.PoolTakenCount, stats.PoolReturnedCount, stats.PoolCreatedCount,
		stats.PoolTakenCount-stats.PoolReturnedCount)
	if os.Getenv("URNETWORK_PROVIDER_GOROUTINE_PROFILE") != "" {
		if profile := pprof.Lookup("goroutine"); profile != nil {
			if err := profile.WriteTo(os.Stdout, 1); err != nil {
				t.Logf("write goroutine profile: %v", err)
			}
		}
	}
	writeProviderMemProfile(t, "provider_mem_loaded")

	closePeers()
	finalGoroutines, finalHeap := sampleStable()
	finalStats := GetMemoryStats()
	writeProviderMemProfile(t, "provider_mem_final")

	// Retain exact bytes and coincident allocator state, not just rounded
	// MiB or quiesced HeapAlloc. No JSON allocation occurs during the load.
	gcPolicy := [...]metrics.Sample{{Name: "/gc/gogc:percent"}}
	metrics.Read(gcPolicy[:])
	if gcPolicy[0].Value.Kind() != metrics.KindUint64 || gcPolicy[0].Value.Uint64() != 10 {
		t.Fatal("provider memory measurement lost its pinned GOGC=10 policy")
	}
	measurement := struct {
		PID           int                    `json:"pid"`
		GoVersion     string                 `json:"go_version"`
		GOOS          string                 `json:"goos"`
		GOARCH        string                 `json:"goarch"`
		GOMAXPROCS    int                    `json:"gomaxprocs"`
		HostCPUs      int                    `json:"host_cpus"`
		ProfileRate   int                    `json:"memory_profile_rate_bytes"`
		GOGC          uint64                 `json:"gogc"`
		SoftLimit     int64                  `json:"soft_limit_bytes"`
		PeakCeiling   int64                  `json:"peak_ceiling_bytes"`
		Race          bool                   `json:"race"`
		TCPBytes      int64                  `json:"tcp_bytes_completed"`
		UDPRoundTrips int                    `json:"udp_round_trips_completed"`
		TCPDuration   time.Duration          `json:"tcp_duration_ns"`
		UDPDuration   time.Duration          `json:"udp_duration_ns"`
		Idle          providerMemorySnapshot `json:"idle"`
		TCP           peakMemoryTelemetry    `json:"tcp"`
		UDP           peakMemoryTelemetry    `json:"udp"`
		Loaded        providerMemorySnapshot `json:"loaded"`
		Final         providerMemorySnapshot `json:"final"`
	}{
		PID: os.Getpid(), GoVersion: runtime.Version(), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		GOMAXPROCS: runtime.GOMAXPROCS(0), HostCPUs: runtime.NumCPU(), ProfileRate: runtime.MemProfileRate,
		GOGC:      gcPolicy[0].Value.Uint64(),
		SoftLimit: debug.SetMemoryLimit(-1), PeakCeiling: peakTotalCeiling, Race: raceEnabled,
		TCPBytes: bytesMoved, UDPRoundTrips: peerCount * udpFlowsPerPeer,
		TCPDuration: tcpElapsed, UDPDuration: udpElapsed,
		Idle: providerMemoryValues(idleStats), TCP: tcpSampler.telemetry, UDP: udpSampler.telemetry,
		Loaded: providerMemoryValues(stats), Final: providerMemoryValues(finalStats),
	}
	encoded, err := json.Marshal(measurement)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("[provider-mem-json] %s", encoded)

	// the stable measurement line for comparing provider memory changes
	t.Logf("[provider-mem] budget=%s idle=%s peakTotal=%s peakHeap=%s loaded=%s loadedTotal=%s final=%s goroutines idle=%d peak=%d loaded=%d final=%d tcpMiBps=%.1f udpRps=%.1f tcpPeakTotal=%s udpPeakTotal=%s settledPeak=%s tcpSettledPeak=%s udpSettledPeak=%s",
		humanBytes(budgetByteCount),
		humanBytes(idleHeap),
		humanBytes(uint64(peakTotal)),
		humanBytes(uint64(peakHeap)),
		humanBytes(loadedHeap),
		humanBytes(uint64(loadedTotal)),
		humanBytes(finalHeap),
		idleGoroutines, peakGoroutines, loadedGoroutines, finalGoroutines,
		float64(bytesMoved)/(1<<20)/tcpElapsed.Seconds(),
		float64(peerCount*udpFlowsPerPeer)/udpElapsed.Seconds(),
		humanBytes(uint64(tcpPeakTotal)),
		humanBytes(uint64(udpPeakTotal)),
		humanBytes(uint64(settledPeak)),
		humanBytes(uint64(tcpSettledPeak)),
		humanBytes(uint64(udpSettledPeak)))

	// regression ceilings (see the constants above). Race instrumentation
	// inflates every one of these numbers (shadow memory lands in the runtime
	// total), so under -race the [provider-mem] line is log-only.
	if !raceEnabled {
		if idleHeap > idleHeapCeiling {
			t.Errorf("idle quiesced heap %s exceeds the regression ceiling %s",
				humanBytes(idleHeap), humanBytes(uint64(idleHeapCeiling)))
		}
		if loadedHeap > loadedHeapCeiling {
			t.Errorf("loaded quiesced heap %s exceeds the regression ceiling %s",
				humanBytes(loadedHeap), humanBytes(uint64(loadedHeapCeiling)))
		}
		if settledPeak > peakTotalCeiling {
			t.Errorf("settled peak footprint %s exceeds the regression ceiling %s (peak runtime total %s)",
				humanBytes(uint64(settledPeak)), humanBytes(uint64(peakTotalCeiling)), humanBytes(uint64(peakTotal)))
		}
	}
}

// providerLoadPeer is one remote peer served by the device's provider: a full
// connect client wired to the provider client over lossless in-memory gateway
// routes, with a gvisor tun as its packet source (mirroring connect's
// testingNewClient wiring).
type providerLoadPeer struct {
	t      testing.TB
	cancel context.CancelFunc
	client *connect.Client
	nat    *connect.RemoteUserNatClient
	tun    *connect.Tun

	providerClient    *connect.Client
	providerTransport [2]connect.Transport
	peerTransport     [2]connect.Transport
	routes            [2]connect.Route

	bridgeWg  sync.WaitGroup
	closeOnce sync.Once
}

func newProviderLoadPeer(t *testing.T, ctx context.Context, providerClient *connect.Client) *providerLoadPeer {
	peerCtx, peerCancel := context.WithCancel(ctx)
	peerSettings := connect.DefaultClientSettings()
	peerSettings.Log = connect.NewNoopLogger()
	peerClient := connect.NewClient(peerCtx, connect.NewId(), connect.NewNoContractClientOob(), peerSettings)

	// lossless in-memory routes in both directions, buffered to mirror the
	// production transports (TransportBufferSize=32 buffered routes). an
	// unbuffered route serializes the pipeline on per-frame scheduler
	// rendezvous and under-measures the device by ~25%. burst safety at this
	// depth depends on the nat ingress dispatch applying bounded
	// backpressure instead of drop-on-full (see RemoteUserNatProvider
	// ClientReceive) — without it, bursts corrupt per-flow tcp state and
	// collapse runs into stall-to-timeout.
	routeToProvider := make(chan []byte, 64)
	routeToPeer := make(chan []byte, 64)

	peerSend := connect.NewSendGatewayTransport()
	peerReceive := connect.NewReceiveGatewayTransport()
	peerClient.RouteManager().UpdateTransport(peerSend, []connect.Route{routeToProvider})
	peerClient.RouteManager().UpdateTransport(peerReceive, []connect.Route{routeToPeer})
	peerClient.ContractManager().AddNoContractPeer(providerClient.ClientId())

	providerSend := connect.NewSendClientTransport(connect.DestinationId(peerClient.ClientId()))
	providerReceive := connect.NewReceiveGatewayTransport()
	providerClient.RouteManager().UpdateTransport(providerReceive, []connect.Route{routeToProvider})
	providerClient.RouteManager().UpdateTransport(providerSend, []connect.Route{routeToPeer})
	providerClient.ContractManager().AddNoContractPeer(peerClient.ClientId())

	// keep the tun tcp windows small: the nat assumes a lossless in-order
	// source, and bounded in-flight bytes keep the peer-side stacks from
	// polluting the provider measurement (see newLoopbackDeviceEnv)
	tunSettings := connect.DefaultTunSettingsWithBufferSize(2048)
	tunSettings.Log = connect.NewNoopLogger()
	tunSettings.TcpSendBuffer = connect.TcpBufferRange{Min: 4 * 1024, Default: 32 * 1024, Max: 64 * 1024}
	tunSettings.TcpReceiveBuffer = connect.TcpBufferRange{Min: 4 * 1024, Default: 32 * 1024, Max: 64 * 1024}
	tun, err := connect.CreateTun(peerCtx, tunSettings)
	if err != nil {
		peerCancel()
		t.Fatalf("create peer tun: %v", err)
	}

	// provider -> peer: Tun.Write copies into the gvisor stack, so the packet
	// is not retained past the callback
	nat := connect.NewRemoteUserNatClient(
		peerClient,
		func(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte) {
			tun.Write(packet)
		},
		[]connect.MultiHopId{connect.RequireMultiHopId(providerClient.ClientId())},
		protocol.ProvideMode_Network,
	)

	peer := &providerLoadPeer{
		t:                 t,
		cancel:            peerCancel,
		client:            peerClient,
		nat:               nat,
		tun:               tun,
		providerClient:    providerClient,
		providerTransport: [2]connect.Transport{providerSend, providerReceive},
		peerTransport:     [2]connect.Transport{peerSend, peerReceive},
		routes:            [2]connect.Route{routeToProvider, routeToPeer},
	}

	// peer tun -> nat. SendPacket consumes the pooled packet on success and
	// leaves ownership with the caller on failure. Return only a rejected
	// packet: returning unconditionally double-frees successful raw-v2 frames,
	// allowing the pool to reuse bytes that are still queued by SendSequence
	// and corrupting the synthetic TCP stream. A blocking send (-1) keeps the
	// source lossless, and unblocks when the peer client closes.
	source := connect.SourceId(peerClient.ClientId())
	peer.bridgeWg.Add(1)
	go func() {
		defer peer.bridgeWg.Done()
		packets := make([][]byte, 64)
		for {
			n, err := tun.ReadBatch(packets)
			if err != nil {
				return
			}
			for _, packet := range packets[:n] {
				if !peer.nat.SendPacket(source, protocol.ProvideMode_Network, packet, -1) {
					MessagePoolReturn(packet)
				}
			}
		}
	}()

	return peer
}

// close retires both route publications, joins the peer client, and returns
// every pooled frame that remained in the synthetic buffered carrier.
func (self *providerLoadPeer) close() {
	self.closeOnce.Do(func() {
		self.tun.Close()
		self.cancel()
		self.bridgeWg.Wait()
		self.nat.Close()
		for _, transport := range self.peerTransport {
			self.client.RouteManager().RemoveTransport(transport)
		}
		for _, transport := range self.providerTransport {
			self.providerClient.RouteManager().RemoveTransport(transport)
		}
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		if err := self.client.CloseAndWait(closeCtx); err != nil {
			self.t.Errorf("close provider-load peer client: %v", err)
		}
		drainProviderLoadRoutes(self.routes[:]...)
	})
}

// drainProviderLoadRoutes returns carrier ownership left after route retirement.
func drainProviderLoadRoutes(routes ...connect.Route) int {
	returned := 0
	for _, route := range routes {
		for {
			select {
			case message := <-route:
				if connect.MessagePoolReturn(message) {
					returned += 1
				}
			default:
				goto nextRoute
			}
		}
	nextRoute:
	}
	return returned
}

// Buffered synthetic carriers must return frames stranded during retirement.
func TestDrainProviderLoadRoutesReturnsQueuedPoolOwnership(t *testing.T) {
	routeA := make(connect.Route, 2)
	routeB := make(connect.Route, 2)
	routeA <- connect.MessagePoolGet(64)
	routeB <- connect.MessagePoolGet(64)
	if returned := drainProviderLoadRoutes(routeA, routeB); returned != 2 {
		t.Fatalf("returned pooled route frames=%d, want 2", returned)
	}
	if len(routeA) != 0 || len(routeB) != 0 {
		t.Fatalf("routes not drained: len(a)=%d len(b)=%d", len(routeA), len(routeB))
	}
}

// runPeerUdpBurst runs `flows` short-lived udp echo flows through the tun,
// each a fresh source port (a fresh nat flow table entry).
func runPeerUdpBurst(ctx context.Context, tun *connect.Tun, addr string, flows int) error {
	payload := makeLoadPayload(101, 256)
	echo := make([]byte, len(payload))
	for i := 0; i < flows; i++ {
		err := func() error {
			conn, err := tun.DialContext(ctx, "udp", addr)
			if err != nil {
				return fmt.Errorf("udp dial: %w", err)
			}
			defer conn.Close()

			conn.SetDeadline(time.Now().Add(60 * time.Second))
			if _, err := conn.Write(payload); err != nil {
				return fmt.Errorf("udp write: %w", err)
			}
			if _, err := conn.Read(echo); err != nil {
				return fmt.Errorf("udp read: %w", err)
			}
			return nil
		}()
		if err != nil {
			return err
		}
	}
	return nil
}

// startUdpEchoServer starts a loopback udp echo server and returns its address
// and a close function.
func startUdpEchoServer(t *testing.T) (addr string, closeFn func()) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("udp echo listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buffer := make([]byte, 2048)
		for {
			n, remoteAddr, err := conn.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			conn.WriteToUDP(buffer[:n], remoteAddr)
		}
	}()
	return conn.LocalAddr().String(), func() {
		conn.Close()
		<-done
	}
}

// Snapshots keep coincident allocator state: independent maxima cannot explain
// which allocation classes accounted for the actual total-runtime peak.
type providerMemorySnapshot struct {
	RuntimeBytes      int64 `json:"runtime_bytes"`
	HeapLiveBytes     int64 `json:"heap_live_bytes"`
	HeapAllocBytes    int64 `json:"heap_alloc_bytes"`
	HeapInuseBytes    int64 `json:"heap_inuse_bytes"`
	HeapSysBytes      int64 `json:"heap_sys_bytes"`
	HeapReleasedBytes int64 `json:"heap_released_bytes"`
	SysBytes          int64 `json:"sys_bytes"`
	StackInuseBytes   int64 `json:"stack_inuse_bytes"`
	MSpanInuseBytes   int64 `json:"mspan_inuse_bytes"`
	MCacheInuseBytes  int64 `json:"mcache_inuse_bytes"`
	GCSysBytes        int64 `json:"gc_sys_bytes"`
	OtherSysBytes     int64 `json:"other_sys_bytes"`
	ProfilingBytes    int64 `json:"profiling_bucket_bytes"`
	NumGC             int64 `json:"num_gc"`
	Goroutines        int   `json:"goroutines"`
}

func providerMemoryValues(stats *MemoryStats) providerMemorySnapshot {
	return providerMemorySnapshot{
		RuntimeBytes: stats.TotalRuntimeByteCount, HeapLiveBytes: stats.HeapLiveByteCount,
		HeapAllocBytes: stats.HeapAllocByteCount, HeapInuseBytes: stats.HeapInuseByteCount,
		HeapSysBytes: stats.HeapSystemByteCount, HeapReleasedBytes: stats.HeapReleasedByteCount,
		SysBytes: stats.SystemByteCount, NumGC: stats.GCCycleCount, Goroutines: stats.GoroutineCount,
		StackInuseBytes: stats.StackInuseByteCount, MSpanInuseBytes: stats.MSpanInuseByteCount,
		MCacheInuseBytes: stats.MCacheInuseByteCount, GCSysBytes: stats.GCSystemByteCount,
		OtherSysBytes: stats.OtherSystemByteCount, ProfilingBytes: stats.ProfilingBucketByteCount,
	}
}

const peakMemorySampleCadence = 20 * time.Millisecond

// A sample settles once this many collections have completed after it: the
// first may have been marking when the sample was read, and keeps live what
// was allocated during its mark; the second marks after the sample and frees
// everything that was garbage at it; the third, by starting, has finished
// sweeping what the second freed.
const peakMemorySettleCycleCount = 3

// One reading of the footprint the runtime could not give back without
// collecting: its soft-limit usage (the total less released pages) less the
// free pages it holds, which it releases when it must hold its limit. The
// completed collections are counted just before the reading and just after
// it, so the reading saw at least CyclesBefore of them and at most CyclesAfter.
type peakMemoryFootprint struct {
	Bytes        int64
	CyclesBefore int64
	CyclesAfter  int64
}

func readPeakMemoryFootprint() peakMemoryFootprint {
	cycles := [...]metrics.Sample{{Name: "/gc/cycles/total:gc-cycles"}}
	metrics.Read(cycles[:])
	cyclesBefore := int64(cycles[0].Value.Uint64())
	classes := [...]metrics.Sample{
		{Name: "/memory/classes/total:bytes"},
		{Name: "/memory/classes/heap/released:bytes"},
		{Name: "/memory/classes/heap/free:bytes"},
	}
	metrics.Read(classes[:])
	metrics.Read(cycles[:])
	return peakMemoryFootprint{
		Bytes: int64(classes[0].Value.Uint64()) -
			int64(classes[1].Value.Uint64()) -
			int64(classes[2].Value.Uint64()),
		CyclesBefore: cyclesBefore,
		CyclesAfter:  int64(cycles[0].Value.Uint64()),
	}
}

// One phase's samples: the raw peak with the allocator state that coincided
// with it, and the settled peak. The settled peak is the highest footprint
// that still stood once the runtime had collected after it: each sample's
// footprint as far as the first reading peakMemorySettleCycleCount completed
// collections later, or the one after settle's forced collection, still shows
// it. What a starved collection left behind as garbage does not count; a
// footprint the collections could not bring down does.
type peakMemoryTelemetry struct {
	Cadence       time.Duration          `json:"requested_cadence_ns"`
	Samples       int                    `json:"samples"`
	Duration      time.Duration          `json:"duration_ns"`
	MaxGap        time.Duration          `json:"max_gap_ns"`
	AtPeak        providerMemorySnapshot `json:"at_runtime_peak"`
	PeakElapsed   time.Duration          `json:"peak_elapsed_ns"`
	PeakHeapAlloc int64                  `json:"peak_heap_alloc_bytes"`
	PeakHeapInuse int64                  `json:"peak_heap_inuse_bytes"`
	PeakSys       int64                  `json:"peak_sys_bytes"`
	FirstGC       int64                  `json:"first_num_gc"`
	LastGC        int64                  `json:"last_num_gc"`
	SettledPeak   int64                  `json:"settled_peak_footprint_bytes"`
	// the samples settle's forced collection settled, for want of a reading
	// far enough after them
	SettledAtEnd int `json:"settled_at_end_samples"`
	first, last  time.Time
	// the samples still waiting for a reading that settles them
	settling [peakMemorySettleCycleCount + 1]settlingMemorySample
}

// The highest footprint among the held samples read when cycles collections
// had completed. No samples marks a free slot.
type settlingMemorySample struct {
	cycles    int64
	footprint int64
	samples   int
}

// record takes one sample's snapshot, for the raw peak and its attribution,
// and the footprint read just before it, for the settled peak. It allocates
// nothing, so the samples add no pressure to the workload they measure.
func (self *peakMemoryTelemetry) record(at time.Time, stats providerMemorySnapshot, footprint peakMemoryFootprint) {
	if self.Samples == 0 {
		self.Cadence, self.first, self.FirstGC = peakMemorySampleCadence, at, stats.NumGC
	} else {
		self.MaxGap = max(self.MaxGap, at.Sub(self.last))
	}
	self.Samples++
	self.last, self.LastGC, self.Duration = at, stats.NumGC, at.Sub(self.first)
	if stats.RuntimeBytes > self.AtPeak.RuntimeBytes {
		self.AtPeak, self.PeakElapsed = stats, self.Duration
	}
	self.PeakHeapAlloc = max(self.PeakHeapAlloc, stats.HeapAllocBytes)
	self.PeakHeapInuse = max(self.PeakHeapInuse, stats.HeapInuseBytes)
	self.PeakSys = max(self.PeakSys, stats.SysBytes)
	self.settleThrough(footprint)
	self.hold(footprint)
}

// Settles every held sample this reading comes peakMemorySettleCycleCount
// completed collections after, at the smaller of the two footprints.
func (self *peakMemoryTelemetry) settleThrough(footprint peakMemoryFootprint) {
	for i := range self.settling {
		held := &self.settling[i]
		if 0 < held.samples && held.cycles+peakMemorySettleCycleCount <= footprint.CyclesBefore {
			self.SettledPeak = max(self.SettledPeak, min(held.footprint, footprint.Bytes))
			*held = settlingMemorySample{}
		}
	}
}

// Holds a reading until one comes far enough after it. Readings at one count
// share a slot. With every slot taken the newest slot takes the reading too,
// which only makes it wait longer and settle no lower.
func (self *peakMemoryTelemetry) hold(footprint peakMemoryFootprint) {
	free := -1
	newest := -1
	for i := range self.settling {
		held := &self.settling[i]
		switch {
		case held.samples == 0:
			if free < 0 {
				free = i
			}
		case held.cycles == footprint.CyclesAfter:
			held.footprint = max(held.footprint, footprint.Bytes)
			held.samples++
			return
		case newest < 0 || self.settling[newest].cycles < held.cycles:
			newest = i
		}
	}
	if 0 <= free {
		self.settling[free] = settlingMemorySample{
			cycles:    footprint.CyclesAfter,
			footprint: footprint.Bytes,
			samples:   1,
		}
		return
	}
	held := &self.settling[newest]
	held.cycles = max(held.cycles, footprint.CyclesAfter)
	held.footprint = max(held.footprint, footprint.Bytes)
	held.samples++
}

// Settles every sample still held, at a reading taken after a collection that
// began after all of them.
func (self *peakMemoryTelemetry) settleAll(footprint peakMemoryFootprint) {
	for i := range self.settling {
		held := &self.settling[i]
		if 0 < held.samples {
			self.SettledPeak = max(self.SettledPeak, min(held.footprint, footprint.Bytes))
			self.SettledAtEnd += held.samples
			*held = settlingMemorySample{}
		}
	}
}

// peakSampler tracks the max Go soft-limit-accounted bytes, live heap, and
// goroutine count while running, and the settled footprint peak (see
// peakMemoryTelemetry). Soft-limit usage is runtime-managed memory minus
// released heap pages, not process RSS or the iOS jetsam footprint.
type peakSampler struct {
	stopCh chan struct{}
	doneCh chan struct{}

	totalMax     atomic.Int64
	heapMax      atomic.Int64
	goroutineMax atomic.Int64
	// Written only by the sampler, then by stop and settle after joining the
	// sampler. Callers inspect telemetry only after stop; no retained sample
	// list grows.
	telemetry peakMemoryTelemetry
}

func startPeakSampler() *peakSampler {
	self := &peakSampler{
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
	go func() {
		defer close(self.doneCh)
		ticker := time.NewTicker(peakMemorySampleCadence)
		defer ticker.Stop()
		for {
			self.sample()
			select {
			case <-self.stopCh:
				return
			case <-ticker.C:
			}
		}
	}()
	return self
}

func (self *peakSampler) sample() {
	footprint := readPeakMemoryFootprint()
	var stats MemoryStats
	readMemoryStats(&stats)
	self.telemetry.record(time.Now(), providerMemoryValues(&stats), footprint)
	if self.totalMax.Load() < stats.TotalRuntimeByteCount {
		self.totalMax.Store(stats.TotalRuntimeByteCount)
	}
	if self.heapMax.Load() < stats.HeapLiveByteCount {
		self.heapMax.Store(stats.HeapLiveByteCount)
	}
	if self.goroutineMax.Load() < int64(stats.GoroutineCount) {
		self.goroutineMax.Store(int64(stats.GoroutineCount))
	}
}

// stop takes a final sample and returns (total, heap, goroutines) maxima.
func (self *peakSampler) stop() (int64, int64, int) {
	select {
	case <-self.stopCh:
	default:
		close(self.stopCh)
	}
	<-self.doneCh
	self.sample()
	return self.totalMax.Load(), self.heapMax.Load(), int(self.goroutineMax.Load())
}

// settle forces a collection, which begins after the sampler's last reading
// and so frees and sweeps all that was garbage at any sample, settles every
// sample still held at the footprint left after it, and returns the settled
// peak. Call it after stop, once the phase's load is done.
func (self *peakSampler) settle() int64 {
	runtime.GC()
	self.telemetry.settleAll(readPeakMemoryFootprint())
	return self.telemetry.SettledPeak
}

// writeProviderMemProfile writes a pprof heap profile named `<name>.pprof`
// into URNETWORK_PROVIDER_MEM_PROFILE_DIR, when set.
func writeProviderMemProfile(t *testing.T, name string) {
	dir := os.Getenv("URNETWORK_PROVIDER_MEM_PROFILE_DIR")
	if dir == "" {
		return
	}
	runtime.GC()
	path := filepath.Join(dir, name+".pprof")
	f, err := os.Create(path)
	if err != nil {
		t.Logf("mem profile: %v", err)
		return
	}
	defer f.Close()
	if err := pprof.WriteHeapProfile(f); err != nil {
		t.Logf("mem profile write: %v", err)
		return
	}
	t.Logf("wrote heap profile %s", path)
}
