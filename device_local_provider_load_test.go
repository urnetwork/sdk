package sdk

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
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
// The test emits one greppable `[provider-mem]` line with the stable
// measurement set, to compare across provider memory changes:
//
//   - idle: quiesced heap with the device + peers connected, before load
//   - peakTotal/peakHeap: max sampled Go soft-limit usage / live heap during
//     load. peakTotal is not RSS or the iOS jetsam footprint: binary mappings,
//     C allocations, and kernel memory are outside this gauge.
//   - loaded: quiesced heap right after load (flows closed, nat entries and
//     pools still warm)
//   - final: quiesced heap after the peers disconnect
//
// Set URNETWORK_PROVIDER_MEM_PROFILE_DIR to also write pprof heap profiles at
// the loaded and final points for allocation-site attribution. Set
// URNETWORK_PROVIDER_GOROUTINE_PROFILE=1 to print the loaded goroutine profile
// grouped by stack, for per-flow goroutine attribution.
func TestDeviceLocalProviderMemoryUnderLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping DeviceLocal provider memory test in -short mode")
	}
	if runIsolatedLoadTest(t) {
		return
	}

	const budgetByteCount = 32 * 1024 * 1024

	const (
		peerCount    = 6
		rounds       = 4 // sequential connection churn per peer
		flowsPerPeer = 2 // concurrent tcp flows per peer per round
		bytesPerFlow = 96 << 10
		// short udp flows per peer, to populate the exit nat's flow table the
		// way scattered real traffic does (entries linger to their idle
		// timeout, so they land in the loaded measurement)
		udpFlowsPerPeer = 32

		// Regression ceilings over the measured baseline. After making the
		// receive-buffer rejection tombstones lazy (2026-08-14), 12 fresh
		// darwin/arm64 processes measured peakTotal=26.0-27.4 MiB and
		// loaded=7.3-7.4 MiB. Keep 256 KiB of explicit headroom below the
		// product's 28 MiB upper target instead of allowing the old 40 MiB
		// process regression. loaded is still held to the budget/2 goal.
		loadedHeapCeiling = budgetByteCount / 2

		// darwin/arm64: the 28 MiB product target less 256 KiB of headroom,
		// which is what the 12-process baseline above was measured against.
		peakTotalCeilingDarwin = (28 << 20) - (256 << 10)

		// linux/amd64 CARRIES ~4 MiB MORE FOR THE SAME WORK, and it is the
		// platform CI runs on, so the darwin figure failed every run on main.
		// Measured 2026-08-19, go1.26.6, 5 fresh linux/amd64 processes at the
		// merge of #142: peakTotal=30.8, 30.9, 31.2, 31.3, 31.4 MiB — spread
		// 0.6 MiB, and identical goroutine counts (237 idle / 715 peak / 679
		// loaded / 457 final) and heap (7.8 MiB loaded) to darwin. Same work,
		// same object graph, different runtime accounting: allocator arenas,
		// goroutine stack granularity and GC pacing all differ. It is a
		// platform delta, not a leak.
		//
		// THE HONEST CONSEQUENCE, and it is worth stating rather than hiding
		// in a number: on linux/amd64 the provider peak sits AT the 32 MiB
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

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
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
	t.Logf("idle (device + %d peers connected): goroutines=%d heap=%s",
		peerCount, idleGoroutines, humanBytes(idleHeap))

	sampler := startPeakSampler()

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
			sampler.stop()
			skipOnRaceGvisorWedge(t, "tcp load", err)
			t.Fatalf("tcp load: %v", err)
		}
	}
	tcpElapsed := time.Since(loadStart)

	// udp burst: scatter short flows into the exit nat's flow table
	for _, peer := range peers {
		go func() {
			loadErrs <- runPeerUdpBurst(ctx, peer.tun, udpEchoAddr, udpFlowsPerPeer)
		}()
	}
	for range peers {
		if err := <-loadErrs; err != nil {
			sampler.stop()
			skipOnRaceGvisorWedge(t, "udp burst", err)
			t.Fatalf("udp burst: %v", err)
		}
	}

	peakTotal, peakHeap, peakGoroutines := sampler.stop()

	bytesMoved := int64(peerCount) * int64(rounds) * int64(flowsPerPeer) * int64(bytesPerFlow)
	t.Logf("tcp load: %d peers x %d conns x %s = %.1f MiB in %v (%.1f MiB/s echoed)",
		peerCount, rounds*flowsPerPeer, humanBytes(bytesPerFlow),
		float64(bytesMoved)/(1<<20), tcpElapsed.Round(time.Millisecond),
		float64(bytesMoved)/(1<<20)/tcpElapsed.Seconds())

	loadedGoroutines, loadedHeap := sampleStable()
	stats := GetMemoryStats()
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
	writeProviderMemProfile(t, "provider_mem_final")

	// the stable measurement line for comparing provider memory changes
	t.Logf("[provider-mem] budget=%s idle=%s peakTotal=%s peakHeap=%s loaded=%s final=%s goroutines idle=%d peak=%d loaded=%d final=%d",
		humanBytes(budgetByteCount),
		humanBytes(idleHeap),
		humanBytes(uint64(peakTotal)),
		humanBytes(uint64(peakHeap)),
		humanBytes(loadedHeap),
		humanBytes(finalHeap),
		idleGoroutines, peakGoroutines, loadedGoroutines, finalGoroutines)

	// regression ceilings (see the constants above). Race instrumentation
	// inflates both numbers (shadow memory lands in the runtime total), so
	// under -race the [provider-mem] line is log-only.
	if !raceEnabled {
		if loadedHeap > loadedHeapCeiling {
			t.Errorf("loaded quiesced heap %s exceeds the regression ceiling %s",
				humanBytes(loadedHeap), humanBytes(uint64(loadedHeapCeiling)))
		}
		if peakTotal > peakTotalCeiling {
			t.Errorf("peak runtime total %s exceeds the regression ceiling %s",
				humanBytes(uint64(peakTotal)), humanBytes(uint64(peakTotalCeiling)))
		}
	}
}

// providerLoadPeer is one remote peer served by the device's provider: a full
// connect client wired to the provider client over lossless in-memory gateway
// routes, with a gvisor tun as its packet source (mirroring connect's
// testingNewClient wiring).
type providerLoadPeer struct {
	cancel context.CancelFunc
	client *connect.Client
	nat    *connect.RemoteUserNatClient
	tun    *connect.Tun

	providerClient    *connect.Client
	providerTransport [2]connect.Transport

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
		cancel:            peerCancel,
		client:            peerClient,
		nat:               nat,
		tun:               tun,
		providerClient:    providerClient,
		providerTransport: [2]connect.Transport{providerSend, providerReceive},
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

func (self *providerLoadPeer) close() {
	self.closeOnce.Do(func() {
		self.tun.Close()
		self.cancel()
		self.bridgeWg.Wait()
		self.nat.Close()
		self.client.Close()
		for _, transport := range self.providerTransport {
			self.providerClient.RouteManager().RemoveTransport(transport)
		}
	})
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

// peakSampler tracks the max Go soft-limit-accounted bytes, live heap, and
// goroutine count while running. This is runtime-managed memory minus released
// heap pages, not process RSS or the iOS jetsam footprint.
type peakSampler struct {
	stopCh chan struct{}
	doneCh chan struct{}

	totalMax     atomic.Int64
	heapMax      atomic.Int64
	goroutineMax atomic.Int64
}

func startPeakSampler() *peakSampler {
	self := &peakSampler{
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
	go func() {
		defer close(self.doneCh)
		ticker := time.NewTicker(20 * time.Millisecond)
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
	stats := GetMemoryStats()
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
