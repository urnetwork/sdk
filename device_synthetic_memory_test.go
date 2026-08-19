package sdk

import (
	"context"
	"errors"
	"fmt"
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
)

// TestDeviceLocalSyntheticDeviceRemoteMemorySoak exercises the complete mobile
// DeviceLocal, DeviceRemote, and RPC shape in one hermetic process:
//
//	paired gVisor TUN <-> compact packet-batch ABI <-> DeviceLocal
//	  -> UpgradeMux + client security -> RemoteUserNatMultiClient
//	  -> bounded in-memory connect transports -> production provider security
//	  -> provider LocalUserNat -> loopback HTTP, HTTPS, SMTP/465 and SMTP/587
//
// At the same time, a second paired client drives DeviceLocal's provider side,
// and DeviceRemote configures and observes DeviceLocal over the production RPC
// protocol and deviceRpcMux. The only synthetic layers are the transport wires
// and endpoint routing; packet, policy, NAT, TLS, SMTP, window, and RPC code are
// production code.
//
// The default one-minute soak is intentionally useful in ordinary regression
// runs. Set URNETWORK_DEVICE_LOCAL_MEMORY_SOAK_DURATION=30m (or longer) for a
// long-horizon run. Set URNETWORK_DEVICE_LOCAL_SYNTHETIC_PROFILE_DIR to capture
// end-of-soak heap and cumulative-allocation profiles for attribution.
func TestDeviceLocalSyntheticDeviceRemoteMemorySoak(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping synthetic DeviceLocal/DeviceRemote memory soak in -short mode")
	}
	if runIsolatedLoadTest(t) {
		return
	}

	const processBudgetByteCount = 32 * 1024 * 1024
	soakDuration := syntheticMemorySoakDuration(t)

	pinIosGcPacing(t)
	previousLimit := debug.SetMemoryLimit(-1)
	SetMemoryLimit(processBudgetByteCount)
	t.Cleanup(func() {
		connect.SetMemoryBudget(0)
		SetMessagePoolMemoryTargets(
			connect.InitialMessagePoolByteCount/2,
			connect.InitialMessagePoolByteCount/2,
		)
		debug.SetMemoryLimit(previousLimit)
	})

	rootCtx, rootCancel := context.WithTimeout(
		context.Background(),
		soakDuration+3*time.Minute,
	)
	defer rootCancel()

	endpoints := newSyntheticEndpointServers(t)
	defer endpoints.Close()
	providerTcpAddr, closeProviderTcp := startTcpEchoServer(t)
	defer closeProviderTcp()
	providerUdpAddr, closeProviderUdp := startUdpEchoServer(t)
	defer closeProviderUdp()

	// Warm every production subsystem—including DeviceRemote and both halves of
	// DeviceLocal—with a separate environment. This prevents lazy global pool,
	// TLS, gob, and gVisor initialization from looking like retained workload.
	func() {
		env := newSyntheticMemoryEnvironment(
			t,
			rootCtx,
			endpoints,
			providerTcpAddr,
			providerUdpAddr,
		)
		defer env.Close()
		if err := env.runWorkloadCycle(0); err != nil {
			skipOnRaceGvisorWedge(t, "synthetic warm-up", err)
			t.Fatalf("synthetic warm-up: %v", err)
		}
	}()

	baseGoroutines, baseHeap := sampleStable()
	baseRuntime := GetMemoryStats().TotalRuntimeByteCount
	baseStacks := captureGoroutineStacks()
	baseFds := openFdCount()
	basePool := poolOutstanding()
	t.Logf(
		"[synthetic-device-mem] process-baseline heap=%s runtime=%s goroutines=%d fds=%d pool=%d",
		humanBytes(baseHeap),
		humanBytes(uint64(baseRuntime)),
		baseGoroutines,
		baseFds,
		basePool,
	)

	env := newSyntheticMemoryEnvironment(
		t,
		rootCtx,
		endpoints,
		providerTcpAddr,
		providerUdpAddr,
	)
	idleGoroutines, idleHeap := sampleStable()
	idleRuntime := GetMemoryStats().TotalRuntimeByteCount
	t.Logf(
		"[synthetic-device-mem] connected-idle heap=%s runtime=%s goroutines=%d duration=%s",
		humanBytes(idleHeap),
		humanBytes(uint64(idleRuntime)),
		idleGoroutines,
		soakDuration,
	)

	var recoverySamples []uint64
	var naturalSamples []uint64
	var maximumPeak syntheticMemoryPeak
	deadline := time.Now().Add(soakDuration)
	cycle := 0
	for cycle < 3 || time.Now().Before(deadline) {
		cycle++
		peakSampler := startSyntheticMemoryPeakSampler(env.device)
		started := time.Now()
		err := env.runWorkloadCycle(cycle)
		peak := peakSampler.Stop()
		if err != nil {
			env.Close()
			skipOnRaceGvisorWedge(t, fmt.Sprintf("synthetic cycle %d", cycle), err)
			t.Fatalf("synthetic cycle %d: %v", cycle, err)
		}
		maximumPeak.Max(peak)

		// First observe natural post-burst behavior. The forced sample that
		// follows measures retained live memory independently of GC timing.
		time.Sleep(150 * time.Millisecond)
		natural := syntheticMemoryPoint(env.device)
		naturalSamples = append(naturalSamples, natural.heapAlloc)
		recoveredGoroutines, recoveredHeap := sampleStable()
		recoverySamples = append(recoverySamples, recoveredHeap)
		recoveredRuntime := GetMemoryStats().TotalRuntimeByteCount

		t.Logf(
			"[synthetic-device-mem] cycle=%d elapsed=%s peak-heap=%s peak-inuse=%s peak-runtime=%s peak-tracked=%s natural-heap=%s recovered-heap=%s recovered-runtime=%s goroutines=%d",
			cycle,
			time.Since(started).Round(time.Millisecond),
			humanBytes(uint64(peak.heapAlloc)),
			humanBytes(uint64(peak.heapInuse)),
			humanBytes(uint64(peak.runtimeTotal)),
			humanBytes(uint64(peak.deviceTracked)),
			humanBytes(natural.heapAlloc),
			humanBytes(recoveredHeap),
			humanBytes(uint64(recoveredRuntime)),
			recoveredGoroutines,
		)
	}

	writeSyntheticMemoryProfiles(t)
	env.assertTraffic(t)
	assertSyntheticMemoryBehavior(
		t,
		env.device,
		idleHeap,
		idleRuntime,
		maximumPeak,
		naturalSamples,
		recoverySamples,
	)
	env.Close()

	finalGoroutines, finalHeap := sampleStable()
	finalRuntime := GetMemoryStats().TotalRuntimeByteCount
	finalFds := openFdCount()
	t.Logf(
		"[synthetic-device-mem] post-teardown cycles=%d heap=%s runtime=%s goroutines=%d fds=%d pool=%d",
		cycle,
		humanBytes(finalHeap),
		humanBytes(uint64(finalRuntime)),
		finalGoroutines,
		finalFds,
		poolOutstanding(),
	)

	const (
		goroutineTolerance = 18
		stackTolerance     = 6
		fdTolerance        = 12
		heapTolerance      = 20 * 1024 * 1024
	)
	if finalGoroutines > baseGoroutines+goroutineTolerance {
		t.Errorf(
			"goroutines did not return to process baseline: final=%d baseline=%d tolerance=+%d",
			finalGoroutines,
			baseGoroutines,
			goroutineTolerance,
		)
	}
	reportGoroutineLeaks(t, baseStacks, captureGoroutineStacks(), stackTolerance)
	if 0 <= baseFds && finalFds > baseFds+fdTolerance {
		t.Errorf(
			"file descriptors did not return to process baseline: final=%d baseline=%d tolerance=+%d",
			finalFds,
			baseFds,
			fdTolerance,
		)
	}
	if finalHeap > baseHeap+heapTolerance {
		t.Errorf(
			"heap did not return to process baseline: final=%s baseline=%s tolerance=+%s",
			humanBytes(finalHeap),
			humanBytes(baseHeap),
			humanBytes(heapTolerance),
		)
	}
	reportPoolLeaks(t, basePool, 16)
}

type syntheticMemoryEnvironment struct {
	ctx    context.Context
	cancel context.CancelFunc

	device    *DeviceLocal
	remote    *DeviceRemote
	exit      *syntheticExitProvider
	bridge    *syntheticTunnelBridge
	peer      *providerLoadPeer
	packetSub Sub

	endpoints       *syntheticEndpointServers
	providerTcpAddr string
	providerUdpAddr string
	packetEvents    atomic.Int64

	closeOnce sync.Once
}

type syntheticPacketStatsListener struct {
	events *atomic.Int64
}

func (self *syntheticPacketStatsListener) PacketStatsChanged(*PacketStats) {
	self.events.Add(1)
}

func newSyntheticMemoryEnvironment(
	t *testing.T,
	parentCtx context.Context,
	endpoints *syntheticEndpointServers,
	providerTcpAddr string,
	providerUdpAddr string,
) *syntheticMemoryEnvironment {
	t.Helper()
	ctx, cancel := context.WithCancel(parentCtx)
	env := &syntheticMemoryEnvironment{
		ctx:             ctx,
		cancel:          cancel,
		endpoints:       endpoints,
		providerTcpAddr: providerTcpAddr,
		providerUdpAddr: providerUdpAddr,
	}

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		cancel()
		t.Fatalf("synthetic network space: %v", err)
	}
	settings := DefaultDeviceLocalSettings()
	settings.DisableLogging = true
	settings.Verbose = false
	settings.AllowProvider = true
	settings.ContractStatsEpoch = 50 * time.Millisecond
	settings.NetworkPeersEpoch = 50 * time.Millisecond

	env.exit = newSyntheticExitProvider(ctx, settings, endpoints.Targets())
	settings.GeneratorFunc = func([]*connect.ProviderSpec) connect.MultiClientGenerator {
		return env.exit.generator
	}

	instanceId := NewId()
	clientId := connect.NewId()
	device, err := newDeviceLocalWithOverrides(
		networkSpace,
		byJwt,
		"synthetic-memory-device",
		"ios-network-extension",
		"synthetic",
		instanceId,
		settings,
		clientId,
	)
	if err != nil {
		env.exit.Close()
		cancel()
		t.Fatalf("new synthetic DeviceLocal: %v", err)
	}
	env.device = device

	upgradeMuxSettings := connect.DefaultUpgradeMuxSettings()
	upgradeMuxSettings.Dns = nil
	device.SetUpgradeMuxSettings(upgradeMuxSettings)
	env.bridge = newSyntheticTunnelBridge(t, device)

	rpcSettings := defaultDeviceRpcSettings()
	rpcSettings.DisableLogging = true
	rpcSettings.Verbose = false
	rpcSettings.KeepAliveTimeout = 0
	rpcSettings.RpcReconnectTimeout = 10 * time.Millisecond
	rpcSettings.RpcConnectTimeout = 5 * time.Second
	rpcSettings.RpcCallTimeout = 15 * time.Second
	rpcTransport := newSyntheticDeviceRpcTransport(device.Ctx(), rpcSettings)
	rpcManager := newDeviceLocalRpcManager(device.Ctx(), device, rpcSettings, rpcTransport)
	device.stateLock.Lock()
	device.deviceLocalRpcManager = rpcManager
	device.stateLock.Unlock()

	remote, err := newDeviceRemoteWithOverrides(
		networkSpace,
		byJwt,
		instanceId,
		rpcSettings,
		clientId,
		rpcTransport,
	)
	if err != nil {
		env.Close()
		t.Fatalf("new synthetic DeviceRemote: %v", err)
	}
	env.remote = remote
	remote.Sync()
	if !remote.waitForSync(10 * time.Second) {
		syncError := remote.GetSyncError()
		env.Close()
		t.Fatalf("synthetic DeviceRemote did not sync (error=%q)", syncError)
	}

	env.packetSub = remote.AddPacketStatsChangeListener(
		&syntheticPacketStatsListener{events: &env.packetEvents},
	)
	// These calls intentionally go through DeviceRemote: the test covers the
	// same app-to-extension state synchronization as an iOS process launch.
	remote.SetOffline(false)
	remote.SetTunnelStarted(true)
	remote.SetRouteLocal(false)
	remote.SetProvideMode(ProvideModePublic)
	providerSpecs := NewProviderSpecList()
	providerSpecs.Add(&ProviderSpec{ClientId: newId(env.exit.client.ClientId())})
	remote.SetDestination(
		&ConnectLocation{
			ConnectLocationId: &ConnectLocationId{BestAvailable: true},
			Name:              "synthetic memory exit",
		},
		providerSpecs,
	)

	windowDeadline := time.Now().Add(15 * time.Second)
	for {
		status := remote.GetWindowStatus()
		if status != nil && 0 < status.ProviderStateAdded {
			break
		}
		if syncError := remote.GetSyncError(); syncError != "" {
			env.Close()
			t.Fatalf("synthetic DeviceRemote sync rejected: %s", syncError)
		}
		if time.Now().After(windowDeadline) {
			env.Close()
			t.Fatalf("synthetic provider window did not become ready: %+v", status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !device.GetProvideEnabled() {
		env.Close()
		t.Fatal("DeviceRemote did not enable DeviceLocal provider side")
	}

	env.peer = newProviderLoadPeer(t, ctx, device.provider.Client())
	return env
}

func (self *syntheticMemoryEnvironment) Close() {
	self.closeOnce.Do(func() {
		if self.packetSub != nil {
			self.packetSub.Close()
		}
		if self.peer != nil {
			self.peer.close()
		}
		if self.bridge != nil {
			self.bridge.Close()
		}
		if self.remote != nil {
			self.remote.Close()
		}
		if self.device != nil {
			self.device.Close()
		}
		if self.exit != nil {
			self.exit.Close()
		}
		self.cancel()
	})
}

func (self *syntheticMemoryEnvironment) runWorkloadCycle(cycle int) error {
	cycleCtx, cancel := context.WithTimeout(self.ctx, 45*time.Second)
	if raceEnabled {
		cycleCtx, cancel = context.WithTimeout(self.ctx, 3*time.Minute)
	}
	defer cancel()

	body := syntheticMailBody(8 * 1024)
	type namedJob struct {
		name string
		run  func() error
	}
	jobs := []namedJob{
		{
			name: "http",
			run: func() error {
				return runSyntheticWebRequest(
					cycleCtx,
					self.bridge.tun,
					false,
					fmt.Sprintf("/memory?cycle=%d", cycle),
				)
			},
		},
		{
			name: "https-1",
			run: func() error {
				return runSyntheticWebRequest(
					cycleCtx,
					self.bridge.tun,
					true,
					fmt.Sprintf("/memory?cycle=%d&a=1", cycle),
				)
			},
		},
		{
			name: "https-2",
			run: func() error {
				return runSyntheticWebRequest(
					cycleCtx,
					self.bridge.tun,
					true,
					fmt.Sprintf("/memory?cycle=%d&a=2", cycle),
				)
			},
		},
		{
			name: "smtp-465",
			run: func() error {
				return runSyntheticSubmission(cycleCtx, self.bridge.tun, 465, false, body)
			},
		},
		{
			name: "smtp-587",
			run: func() error {
				return runSyntheticSubmission(cycleCtx, self.bridge.tun, 587, true, body)
			},
		},
		{
			name: "blocked-dht",
			run: func() error {
				return runSyntheticBlockedTraffic(cycleCtx, self.bridge.tun, 4)
			},
		},
		{
			name: "provider-tcp",
			run: func() error {
				return runLoadIteration(cycleCtx, self.peer.tun, self.providerTcpAddr, 1, 2, 64*1024)
			},
		},
		{
			name: "provider-udp",
			run: func() error {
				return runPeerUdpBurst(cycleCtx, self.peer.tun, self.providerUdpAddr, 2)
			},
		},
	}

	errs := make(chan error, len(jobs))
	var wg sync.WaitGroup
	for _, job := range jobs {
		job := job
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := job.run(); err != nil {
				errs <- fmt.Errorf("%s: %w", job.name, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	var result error
	for err := range errs {
		result = errors.Join(result, err)
	}
	if result != nil {
		return fmt.Errorf(
			"%w (web requests=%d bytes=%d smtp465 accepted=%d data=%d submitted=%d smtp587 accepted=%d data=%d submitted=%d bridge send=%d receive=%d dropped=%d exit dials 80=%d 443=%d 465=%d 587=%d unexpected=%d client-stats=%+v exit-stats=%+v exit-congestion=%+v)",
			result,
			self.endpoints.webRequests.Load(),
			self.endpoints.webBytes.Load(),
			self.endpoints.smtp465.accepted.Load(),
			self.endpoints.smtp465.dataBytes.Load(),
			self.endpoints.smtp465.submissions.Load(),
			self.endpoints.smtp587.accepted.Load(),
			self.endpoints.smtp587.dataBytes.Load(),
			self.endpoints.smtp587.submissions.Load(),
			self.bridge.sendPacketCount.Load(),
			self.bridge.receivePacketCount.Load(),
			self.bridge.droppedPacketCount.Load(),
			self.exit.router.Dials(80),
			self.exit.router.Dials(443),
			self.exit.router.Dials(465),
			self.exit.router.Dials(587),
			self.exit.router.UnexpectedDials(),
			self.device.GetPacketStats(),
			self.exit.provider.PacketStats(),
			self.exit.provider.CongestionDropStats(),
		)
	}

	// Exercise forward RPC reads every cycle. Reverse RPC is exercised by the
	// registered packet listener and asserted after the soak.
	if !self.remote.GetRemoteConnected() {
		return fmt.Errorf("DeviceRemote disconnected")
	}
	if status := self.remote.GetWindowStatus(); status == nil || status.ProviderStateAdded == 0 {
		return fmt.Errorf("DeviceRemote window lost provider: %+v", status)
	}
	if stats := self.remote.GetPacketStats(); stats == nil {
		return fmt.Errorf("DeviceRemote packet stats unavailable")
	}
	if stats := self.remote.GetProviderPacketStats(); stats == nil {
		return fmt.Errorf("DeviceRemote provider packet stats unavailable")
	}
	_ = self.remote.GetBlockStats()
	_ = self.remote.GetBlockActions()
	return nil
}

func (self *syntheticMemoryEnvironment) assertTraffic(t *testing.T) {
	t.Helper()
	if err := self.bridge.Error(); err != nil {
		t.Errorf("synthetic packet bridge: %v", err)
	}
	if self.bridge.sendBatchCount.Load() == 0 || self.bridge.receiveBatchCount.Load() == 0 {
		t.Errorf(
			"compact packet bridge was not bidirectional: send batches=%d receive batches=%d",
			self.bridge.sendBatchCount.Load(),
			self.bridge.receiveBatchCount.Load(),
		)
	}
	// SendPacketBatch reports route acceptance, so deliberate policy rejects
	// (the DHT probes) appear here too. A small second-order tail is expected
	// from teardown packets on reset flows; bound it relative to the security
	// counter instead of misclassifying every rejection as bridge packet loss.
	clientStats := self.remote.GetPacketStats()
	rejected := self.bridge.droppedPacketCount.Load()
	blocked := int64(0)
	if clientStats != nil {
		blocked = clientStats.BlockEgressPacketCount
	}
	if rejected < blocked || blocked*3+8 < rejected {
		t.Errorf(
			"compact packet route rejections are not explained by policy: rejected=%d blocked=%d",
			rejected,
			blocked,
		)
	}
	for _, port := range []int{80, 443, 465, 587} {
		if dials := self.exit.router.Dials(port); dials == 0 {
			t.Errorf("synthetic exit received no TCP/%d dials", port)
		}
	}
	if dials := self.exit.router.UnexpectedDials(); dials != 0 {
		t.Errorf("blocked/unmapped traffic escaped to synthetic exit %d times", dials)
	}
	if self.endpoints.webRequests.Load() == 0 || self.endpoints.webBytes.Load() == 0 {
		t.Error("synthetic web endpoints received no response traffic")
	}
	if self.endpoints.smtp465.submissions.Load() == 0 || self.endpoints.smtp587.submissions.Load() == 0 {
		t.Errorf(
			"encrypted SMTP did not complete: 465=%d 587=%d",
			self.endpoints.smtp465.submissions.Load(),
			self.endpoints.smtp587.submissions.Load(),
		)
	}
	if events := self.packetEvents.Load(); events == 0 {
		t.Error("DeviceRemote received no reverse-RPC packet-stat events")
	}
	if created := self.exit.generator.createdClientCount.Load(); created == 0 {
		t.Error("DeviceLocal provider window created no synthetic client")
	}

	stats := clientStats
	if stats == nil || stats.RemoteEgressPacketCount == 0 || stats.RemoteIngressPacketCount == 0 {
		t.Errorf("DeviceRemote client traffic counters do not show bidirectional traffic: %+v", stats)
	}
	if stats == nil || stats.BlockEgressPacketCount == 0 {
		t.Errorf("DeviceRemote client traffic counters do not show blocked traffic: %+v", stats)
	}
	providerStats := self.remote.GetProviderPacketStats()
	if providerStats == nil || providerStats.RemoteIngressPacketCount == 0 || providerStats.RemoteEgressPacketCount == 0 {
		t.Errorf("DeviceRemote provider counters do not show bidirectional traffic: %+v", providerStats)
	}
	securityStats := self.remote.egressSecurityPolicyStats(false)
	assertSecurityPathFired(
		t,
		securityStats,
		connect.SecurityPolicyResultIncident,
		connect.IpProtocolUdp,
		51415,
		"synthetic blocked DHT",
	)
}

type syntheticMemoryPointValue struct {
	heapAlloc     uint64
	heapInuse     uint64
	runtimeTotal  int64
	deviceTracked int64
	goroutines    int64
}

func syntheticMemoryPoint(device *DeviceLocal) syntheticMemoryPointValue {
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	stats := GetMemoryStats()
	tracked := int64(0)
	if device != nil {
		tracked = int64(device.MemoryUsed().TotalByteCount)
	}
	return syntheticMemoryPointValue{
		heapAlloc:     memory.HeapAlloc,
		heapInuse:     memory.HeapInuse,
		runtimeTotal:  int64(stats.TotalRuntimeByteCount),
		deviceTracked: tracked,
		goroutines:    int64(stats.GoroutineCount),
	}
}

type syntheticMemoryPeak struct {
	heapAlloc     int64
	heapInuse     int64
	runtimeTotal  int64
	deviceTracked int64
	goroutines    int64
}

func (self *syntheticMemoryPeak) Max(other syntheticMemoryPeak) {
	self.heapAlloc = max(self.heapAlloc, other.heapAlloc)
	self.heapInuse = max(self.heapInuse, other.heapInuse)
	self.runtimeTotal = max(self.runtimeTotal, other.runtimeTotal)
	self.deviceTracked = max(self.deviceTracked, other.deviceTracked)
	self.goroutines = max(self.goroutines, other.goroutines)
}

type syntheticMemoryPeakSampler struct {
	device *DeviceLocal
	stop   chan struct{}
	done   chan struct{}
	once   sync.Once

	heapAlloc     atomic.Int64
	heapInuse     atomic.Int64
	runtimeTotal  atomic.Int64
	deviceTracked atomic.Int64
	goroutines    atomic.Int64
}

func startSyntheticMemoryPeakSampler(device *DeviceLocal) *syntheticMemoryPeakSampler {
	self := &syntheticMemoryPeakSampler{
		device: device,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	go func() {
		defer close(self.done)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			self.sample()
			select {
			case <-self.stop:
				return
			case <-ticker.C:
			}
		}
	}()
	return self
}

func atomicMax(value *atomic.Int64, candidate int64) {
	for current := value.Load(); current < candidate; current = value.Load() {
		if value.CompareAndSwap(current, candidate) {
			return
		}
	}
}

func (self *syntheticMemoryPeakSampler) sample() {
	point := syntheticMemoryPoint(self.device)
	atomicMax(&self.heapAlloc, int64(point.heapAlloc))
	atomicMax(&self.heapInuse, int64(point.heapInuse))
	atomicMax(&self.runtimeTotal, point.runtimeTotal)
	atomicMax(&self.deviceTracked, point.deviceTracked)
	atomicMax(&self.goroutines, point.goroutines)
}

func (self *syntheticMemoryPeakSampler) Stop() syntheticMemoryPeak {
	self.once.Do(func() { close(self.stop) })
	<-self.done
	self.sample()
	return syntheticMemoryPeak{
		heapAlloc:     self.heapAlloc.Load(),
		heapInuse:     self.heapInuse.Load(),
		runtimeTotal:  self.runtimeTotal.Load(),
		deviceTracked: self.deviceTracked.Load(),
		goroutines:    self.goroutines.Load(),
	}
}

func syntheticMemorySoakDuration(t *testing.T) time.Duration {
	t.Helper()
	const defaultDuration = time.Minute
	value := os.Getenv("URNETWORK_DEVICE_LOCAL_MEMORY_SOAK_DURATION")
	if value == "" {
		return defaultDuration
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		t.Fatalf(
			"invalid URNETWORK_DEVICE_LOCAL_MEMORY_SOAK_DURATION %q: %v",
			value,
			err,
		)
	}
	return duration
}

func assertSyntheticMemoryBehavior(
	t *testing.T,
	device *DeviceLocal,
	idleHeap uint64,
	idleRuntime ByteCount,
	peak syntheticMemoryPeak,
	naturalSamples []uint64,
	recoverySamples []uint64,
) {
	t.Helper()
	if len(recoverySamples) < 3 {
		t.Fatalf("memory soak produced %d recovery samples, want at least 3", len(recoverySamples))
	}

	warmFrom := min(2, len(recoverySamples)-1)
	minimumRecovered := recoverySamples[warmFrom]
	maximumRecovered := recoverySamples[warmFrom]
	for _, recovered := range recoverySamples[warmFrom:] {
		minimumRecovered = min(minimumRecovered, recovered)
		maximumRecovered = max(maximumRecovered, recovered)
	}
	const recoverySpreadLimit = 10 * 1024 * 1024
	if maximumRecovered > minimumRecovered+recoverySpreadLimit {
		t.Errorf(
			"recovered heap drifted during soak: min=%s max=%s spread=%s limit=%s",
			humanBytes(minimumRecovered),
			humanBytes(maximumRecovered),
			humanBytes(maximumRecovered-minimumRecovered),
			humanBytes(recoverySpreadLimit),
		)
	}
	const endGrowthLimit = 6 * 1024 * 1024
	firstRecovered := recoverySamples[warmFrom]
	lastRecovered := recoverySamples[len(recoverySamples)-1]
	if lastRecovered > firstRecovered+endGrowthLimit {
		t.Errorf(
			"recovered heap ended above its warm value: first=%s last=%s limit=+%s",
			humanBytes(firstRecovered),
			humanBytes(lastRecovered),
			humanBytes(endGrowthLimit),
		)
	}

	const recoveredIdleAllowance = 16 * 1024 * 1024
	if lastRecovered > idleHeap+recoveredIdleAllowance {
		t.Errorf(
			"recovered heap remained too far above connected idle: idle=%s last=%s allowance=+%s",
			humanBytes(idleHeap),
			humanBytes(lastRecovered),
			humanBytes(recoveredIdleAllowance),
		)
	}

	// This is a full three-party simulation in one process, while the real app,
	// extension, and remote provider are separate. Guard a baseline-relative
	// regression rather than pretending the process total is extension RSS.
	const runtimeSpikeAllowance = 64 * 1024 * 1024
	if peak.runtimeTotal > int64(idleRuntime)+runtimeSpikeAllowance {
		t.Errorf(
			"runtime memory spike exceeded connected-idle allowance: idle=%s peak=%s allowance=+%s",
			humanBytes(uint64(idleRuntime)),
			humanBytes(uint64(peak.runtimeTotal)),
			humanBytes(runtimeSpikeAllowance),
		)
	}

	usage := device.MemoryUsed()
	if peak.deviceTracked > int64(usage.TargetByteCount) {
		t.Errorf(
			"DeviceLocal tracked memory exceeded target: peak=%s target=%s",
			humanBytes(uint64(peak.deviceTracked)),
			humanBytes(uint64(usage.TargetByteCount)),
		)
	}

	recoveredFromSpike := false
	for index, recovered := range recoverySamples {
		if index < len(naturalSamples) && recovered+256*1024 < naturalSamples[index] {
			recoveredFromSpike = true
			break
		}
	}
	if !recoveredFromSpike {
		t.Error("no cycle demonstrated post-burst heap recovery of at least 256 KiB")
	}
	t.Logf(
		"[synthetic-device-mem] summary samples=%d peak-heap=%s peak-runtime=%s peak-tracked=%s recovered-min=%s recovered-max=%s recovered-last=%s",
		len(recoverySamples),
		humanBytes(uint64(peak.heapAlloc)),
		humanBytes(uint64(peak.runtimeTotal)),
		humanBytes(uint64(peak.deviceTracked)),
		humanBytes(minimumRecovered),
		humanBytes(maximumRecovered),
		humanBytes(lastRecovered),
	)
}

func writeSyntheticMemoryProfiles(t *testing.T) {
	t.Helper()
	dir := os.Getenv("URNETWORK_DEVICE_LOCAL_SYNTHETIC_PROFILE_DIR")
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Errorf("create synthetic memory profile directory: %v", err)
		return
	}
	runtime.GC()
	profiles := []struct {
		name  string
		write func(*os.File) error
	}{
		{
			name: "heap.pprof",
			write: func(file *os.File) error {
				return pprof.WriteHeapProfile(file)
			},
		},
		{
			name: "allocs.pprof",
			write: func(file *os.File) error {
				return pprof.Lookup("allocs").WriteTo(file, 0)
			},
		},
		{
			name: "goroutine.pprof",
			write: func(file *os.File) error {
				return pprof.Lookup("goroutine").WriteTo(file, 0)
			},
		},
	}
	for _, profile := range profiles {
		path := filepath.Join(dir, profile.name)
		file, err := os.Create(path)
		if err != nil {
			t.Errorf("create %s: %v", path, err)
			continue
		}
		err = profile.write(file)
		closeErr := file.Close()
		if err != nil {
			t.Errorf("write %s: %v", path, err)
			continue
		}
		if closeErr != nil {
			t.Errorf("close %s: %v", path, closeErr)
			continue
		}
		t.Logf("wrote synthetic memory profile %s", path)
	}
}
