package sdk

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// TestDeviceLocalReconfigurationChurn toggles every reconfigurable subsystem of ONE
// long-lived device — destination (multi-client + upgrade mux + client security
// policy), provide mode (provider local NAT + RemoteUserNatProvider + provider
// security policy and its DMCA scan goroutine), route local, provide paused — and
// asserts goroutines/stacks/fds/heap/pool return to baseline.
//
// This is the coverage the whole-device churn test cannot give: there, Close cancels
// the device's client contexts, so anything scoped to a killed client dies with the
// device and a provider/multi-client-scoped leak is invisible. Here the device (and
// its clients) live across every cycle, so a subsystem torn down WITHOUT its
// context being killed must release everything itself — e.g. RemoteUserNatProvider.
// Close must cancel its security policy's scan goroutine; a regression there leaks
// one goroutine per provide-mode toggle and fails the stack diff below.
func TestDeviceLocalReconfigurationChurn(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping reconfiguration churn test in -short mode")
	}

	const (
		cycles                     = 20
		goroutineBaselineTolerance = 8
		goroutineStackTolerance    = 4 // a per-cycle leak shows as ~cycles
		fdBaselineTolerance        = 8
		heapBaselineBandBytes      = 16 << 20
		poolTolerance              = 16
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}

	// a FRESH echo provider client per destination build, closing the previous one.
	// The exit peer must not be long-lived here: a provider's transfer sequences for
	// a departed peer are reaped by IDLE TIMEOUT (send 300s / receive 120s), so a
	// shared echo client would accumulate ~3 parked goroutines and their queued pool
	// buffers per cycle for minutes — provider-side steady state by design, but it
	// would drown the device-scoped leak signal this test is after.
	var generatorLock sync.Mutex
	var closeLastProvider func()
	closeProviderNow := func() {
		generatorLock.Lock()
		defer generatorLock.Unlock()
		if closeLastProvider != nil {
			closeLastProvider()
			closeLastProvider = nil
		}
	}
	defer closeProviderNow()

	settings := DefaultDeviceLocalSettings()
	settings.DisableLogging = true
	settings.Verbose = false
	settings.GeneratorFunc = func(specs []*connect.ProviderSpec) connect.MultiClientGenerator {
		generatorLock.Lock()
		defer generatorLock.Unlock()
		if closeLastProvider != nil {
			closeLastProvider()
		}
		providerCtx, providerCancel := context.WithCancel(ctx)
		providerClient, closeProvider := startEchoProviderClient(t, providerCtx)
		closeLastProvider = func() {
			closeProvider()
			providerCancel()
		}
		return newMuxSecurityGenerator(providerClient)
	}

	prePool := poolOutstanding()

	device, err := newDeviceLocalWithOverrides(networkSpace, byJwt, "", "", "", NewId(), settings, connect.NewId())
	if err != nil {
		t.Fatalf("new device: %v", err)
	}
	defer device.Close()

	unsub := device.AddReceivePacketCallback(func(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte) {
	})
	defer unsub()

	location := &ConnectLocation{ConnectLocationId: &ConnectLocationId{BestAvailable: true}}
	probe := craftIpv4Packet(connect.IpProtocolTcp, testProbeSrcIp(), 34000, testProbeDstIp(), 8443, false, tlsClientHelloProbe())

	cycle := func() {
		device.SetConnectLocation(location) // builds mux + multi-client + client policy
		device.SendPacket(probe, int32(len(probe)))
		device.SetRouteLocal(false)
		device.SetProvideMode(ProvideModePublic) // builds provider NAT + provider + provider policy
		device.SetProvidePaused(true)
		device.SetProvidePaused(false)
		device.Shuffle()
		device.SendPacket(probe, int32(len(probe)))
		device.SetProvideMode(ProvideModeNone) // must release the provider AND its policy scan goroutine
		device.SetRouteLocal(true)
		device.RemoveDestination() // must release mux + multi-client + client policy
	}

	// warmup: initialize every lazy pool/global on all the paths before the baseline
	cycle()

	baseGoroutines, baseHeap := sampleStable()
	baseStacks := captureGoroutineStacks()
	baseFds := openFdCount()
	t.Logf("baseline: goroutines=%d heap=%s fds=%d pool-outstanding=%d", baseGoroutines, humanBytes(baseHeap), baseFds, poolOutstanding())

	for i := 0; i < cycles; i += 1 {
		cycle()
	}

	finalGoroutines, finalHeap := sampleStable()
	finalFds := openFdCount()
	// pool outstanding grows while the device is OPEN with the platform
	// unreachable: every provide toggle queues control frames (provide modes,
	// audits) toward ControlId, and with no transport they park in the client's
	// resend queue — bounded by ResendQueueMaxByteCount (2 MiB/sequence) and
	// released on client close. So while live it is logged, and the balance is
	// asserted after Close below.
	t.Logf("after %d reconfiguration cycles: goroutines=%d heap=%s fds=%d pool-outstanding=%d (parked control frames expected while open)",
		cycles, finalGoroutines, humanBytes(finalHeap), finalFds, poolOutstanding())

	if finalGoroutines > baseGoroutines+goroutineBaselineTolerance {
		t.Errorf("goroutines leaked across %d reconfiguration cycles: final=%d baseline=%d (tol +%d)",
			cycles, finalGoroutines, baseGoroutines, goroutineBaselineTolerance)
	}
	reportGoroutineLeaks(t, baseStacks, captureGoroutineStacks(), goroutineStackTolerance)
	if baseFds >= 0 && finalFds > baseFds+fdBaselineTolerance {
		t.Errorf("file descriptors leaked across %d cycles: final=%d baseline=%d (tol +%d)",
			cycles, finalFds, baseFds, fdBaselineTolerance)
	}
	if finalHeap > baseHeap+heapBaselineBandBytes {
		t.Errorf("heap leaked across %d cycles: final=%s baseline=%s (tol +%s)",
			cycles, humanBytes(finalHeap), humanBytes(baseHeap), humanBytes(heapBaselineBandBytes))
	}

	// every buffer parked while open must come back once the device (and the last
	// exit peer) is closed: growth here is a genuinely lost MessagePoolReturn.
	device.Close()
	closeProviderNow()
	sampleStable()
	reportPoolLeaks(t, prePool, poolTolerance)
}

// TestDeviceLocalConcurrentLifecycleChaos runs live traffic, every reconfiguration
// mutator, and receive-callback churn CONCURRENTLY against one device, then closes
// the device while all of them are still hammering. It asserts no worker panics
// (a panic fails the process), no deadlock (every worker must observe the close and
// exit within a bound — a Set* caller wedged on the device state lock trips this),
// and that the post-close state returns to baseline. Run it under -race for the
// interleaving coverage; the sequential churn test above keeps the leak signal
// clean, this one exists to make lifecycle races fail loudly.
func TestDeviceLocalConcurrentLifecycleChaos(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping concurrent lifecycle chaos test in -short mode")
	}

	const (
		chaosDuration = 1500 * time.Millisecond
		exitBound     = 15 * time.Second
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}

	baseStacks := captureGoroutineStacks()

	// fresh exit peer per destination build, closing the previous one (see the
	// reconfiguration churn test for why the exit peer must not be long-lived)
	var generatorLock sync.Mutex
	var closeLastProvider func()
	closeProviderNow := func() {
		generatorLock.Lock()
		defer generatorLock.Unlock()
		if closeLastProvider != nil {
			closeLastProvider()
			closeLastProvider = nil
		}
	}
	defer closeProviderNow()

	settings := DefaultDeviceLocalSettings()
	settings.DisableLogging = true
	settings.Verbose = false
	settings.GeneratorFunc = func(specs []*connect.ProviderSpec) connect.MultiClientGenerator {
		generatorLock.Lock()
		defer generatorLock.Unlock()
		if closeLastProvider != nil {
			closeLastProvider()
		}
		providerCtx, providerCancel := context.WithCancel(ctx)
		providerClient, closeProvider := startEchoProviderClient(t, providerCtx)
		closeLastProvider = func() {
			closeProvider()
			providerCancel()
		}
		return newMuxSecurityGenerator(providerClient)
	}
	device, err := newDeviceLocalWithOverrides(networkSpace, byJwt, "", "", "", NewId(), settings, connect.NewId())
	if err != nil {
		t.Fatalf("new device: %v", err)
	}

	location := &ConnectLocation{ConnectLocationId: &ConnectLocationId{BestAvailable: true}}
	probes := [][]byte{
		craftIpv4Packet(connect.IpProtocolTcp, testProbeSrcIp(), 35000, testProbeDstIp(), 8443, false, tlsClientHelloProbe()),
		craftIpv4Packet(connect.IpProtocolUdp, testProbeSrcIp(), 35001, testProbeDstIp(), 8446, false, stunBindingProbe()),
		craftIpv4Packet(connect.IpProtocolTcp, testProbeSrcIp(), 35002, testProbeDstIp(), 51413, false, bittorrentHandshake()),
	}

	var wg sync.WaitGroup
	var sends atomic.Int64
	stop := make(chan struct{})
	worker := func(body func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if device.GetDone() {
					return
				}
				body()
			}
		}()
	}

	worker(func() { // packet spam across all policy branches
		for _, probe := range probes {
			if device.SendPacket(probe, int32(len(probe))) {
				sends.Add(1)
			}
		}
	})
	worker(func() { // destination churn: multi-client + mux build/teardown
		device.SetConnectLocation(location)
		device.RemoveDestination()
	})
	worker(func() { // provider churn: provider + its policy build/teardown
		device.SetProvideMode(ProvideModePublic)
		device.SetProvideMode(ProvideModeNone)
	})
	worker(func() { // route + shuffle + paused churn
		device.SetRouteLocal(false)
		device.Shuffle()
		device.SetProvidePaused(true)
		device.SetRouteLocal(true)
		device.SetProvidePaused(false)
	})
	worker(func() { // receive-callback churn against live delivery
		unsub := device.AddReceivePacketCallback(func(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte) {
		})
		unsub()
	})

	time.Sleep(chaosDuration)

	// close the device while every worker is still hammering it
	device.Close()

	close(stop)
	if !waitTimeout(&wg, exitBound) {
		t.Fatalf("workers did not exit within %v of device close — a mutator or send path deadlocked", exitBound)
	}
	t.Logf("chaos: %d packets sent across the reconfiguration storm", sends.Load())

	// everything device-scoped must be gone; the generator's clients were released
	// through RemoveClientWithArgs and the multi-client close path, and the last
	// exit peer through closeProviderNow
	closeProviderNow()
	sampleStable()
	reportGoroutineLeaks(t, baseStacks, captureGoroutineStacks(), 4)

	// NOTE: this test deliberately does NOT assert message-pool balance. Closing the
	// device mid-storm drops whatever buffers are in flight across the multi-client ->
	// mux -> provider pipeline at the cancellation points; that count scales with the
	// in-flight window at the instant of Close (observed 0..several thousand out of
	// tens of millions sent), is GC-collected, and does not accumulate. Steady-state
	// pool balance is asserted by TestDeviceLocalReconfigurationChurn, which
	// reconfigures sequentially with no concurrent close-during-send. This test's job
	// is race / deadlock / panic detection under concurrent lifecycle churn.
}

// probe helpers shared by the reconfiguration tests. Distinct names from the mux
// security test's payload builders (same package) while reusing the same wire forms.
func testProbeSrcIp() net.IP { return net.ParseIP("10.0.0.3") }
func testProbeDstIp() net.IP { return net.ParseIP("203.0.113.7") }
func tlsClientHelloProbe() []byte {
	return []byte{0x16, 0x03, 0x01, 0x00, 0x2a, 0x01, 0x00, 0x00, 0x26}
}
func stunBindingProbe() []byte {
	b := make([]byte, 20)
	b[0], b[1] = 0x00, 0x01
	b[4], b[5], b[6], b[7] = 0x21, 0x12, 0xa4, 0x42
	return b
}
