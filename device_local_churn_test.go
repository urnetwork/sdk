package sdk

import (
	"context"
	"runtime"
	"sync"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// TestDeviceLocalLifecycleChurn creates and closes many real DeviceLocals (each
// with a live data transfer) in a tight loop and asserts that goroutines, goroutine
// stack identities, open file descriptors, and heap all return to baseline. A
// per-lifecycle leak -- a Sub left open, a goroutine outliving Close, an egress
// socket not closed -- is invisible to a single long-lived-device test but
// accumulates here in proportion to the cycle count.
func TestDeviceLocalLifecycleChurn(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping DeviceLocal lifecycle churn test in -short mode")
	}

	const (
		cycles = 60

		// brief per-cycle load: enough to spin up the data-path goroutines/buffers
		// and egress sockets that Close must then release.
		cycleRounds       = 1
		cycleFlows        = 2
		cycleBytesPerFlow = 64 << 10

		goroutineBaselineTolerance = 8
		goroutineStackTolerance    = 5 // per-signature; a per-cycle leak would be ~cycles
		fdBaselineTolerance        = 8
		heapBaselineBandBytes      = 16 << 20
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}
	echoAddr, closeEcho := startTcpEchoServer(t)
	defer closeEcho()

	// warmup: one full cycle so all lazy process-global goroutines/pools/fds are
	// initialized and counted in the baseline.
	runRouteLocalDeviceCycle(t, ctx, networkSpace, byJwt, echoAddr, cycleRounds, cycleFlows, cycleBytesPerFlow)

	baseGoroutines, baseHeap := sampleStable()
	baseStacks := captureGoroutineStacks()
	baseFds := openFdCount()
	t.Logf("baseline: goroutines=%d heap=%s fds=%d", baseGoroutines, humanBytes(baseHeap), baseFds)

	logEvery := cycles / 6
	if logEvery < 1 {
		logEvery = 1
	}
	for i := 0; i < cycles; i++ {
		runRouteLocalDeviceCycle(t, ctx, networkSpace, byJwt, echoAddr, cycleRounds, cycleFlows, cycleBytesPerFlow)
		if (i+1)%logEvery == 0 {
			g, h := sampleStable()
			t.Logf("  after %d cycles: goroutines=%d heap=%s fds=%d", i+1, g, humanBytes(h), openFdCount())
		}
	}

	finalGoroutines, finalHeap := sampleStable()
	finalStacks := captureGoroutineStacks()
	finalFds := openFdCount()
	t.Logf("after %d create/close cycles: goroutines=%d heap=%s fds=%d",
		cycles, finalGoroutines, humanBytes(finalHeap), finalFds)

	if finalGoroutines > baseGoroutines+goroutineBaselineTolerance {
		t.Errorf("goroutines leaked across %d cycles: final=%d baseline=%d (tol +%d)",
			cycles, finalGoroutines, baseGoroutines, goroutineBaselineTolerance)
	}
	reportGoroutineLeaks(t, baseStacks, finalStacks, goroutineStackTolerance)
	if baseFds >= 0 && finalFds > baseFds+fdBaselineTolerance {
		t.Errorf("file descriptors leaked across %d cycles: final=%d baseline=%d (tol +%d)",
			cycles, finalFds, baseFds, fdBaselineTolerance)
	}
	if finalHeap > baseHeap+heapBaselineBandBytes {
		t.Errorf("heap leaked across %d cycles: final=%s baseline=%s (tol +%s)",
			cycles, humanBytes(finalHeap), humanBytes(baseHeap), humanBytes(heapBaselineBandBytes))
	}
}

// runRouteLocalDeviceCycle builds a real DeviceLocal routed locally through its
// provider's LocalUserNat to the loopback echo (via a gVisor tun bridge), runs a
// brief transfer, and tears everything down. It is one create+load+close lifecycle.
func runRouteLocalDeviceCycle(t *testing.T, ctx context.Context, networkSpace *NetworkSpace, byJwt string, echoAddr string, rounds, flows, bytesPerFlow int) {
	t.Helper()

	settings := DefaultDeviceLocalSettings()
	settings.DisableLogging = true
	settings.Verbose = false

	device, err := newDeviceLocalWithOverrides(networkSpace, byJwt, "", "", "", NewId(), settings, connect.NewId())
	if err != nil {
		t.Fatalf("new device: %v", err)
	}
	device.SetRouteLocal(true)

	tunSettings := connect.DefaultTunSettingsWithBufferSize(2048)
	tunSettings.Log = connect.NewNoopLogger()
	tunSettings.TcpSendBuffer = connect.TcpBufferRange{Min: 4 * 1024, Default: 32 * 1024, Max: 64 * 1024}
	tunSettings.TcpReceiveBuffer = connect.TcpBufferRange{Min: 4 * 1024, Default: 32 * 1024, Max: 64 * 1024}
	// The gvisor Tun stands in for the host OS TUN; Tun.Close() destroys its whole
	// stack (fully released on close), so this exercises only DeviceLocal-lifecycle
	// leaks, not the tun stack itself.
	tun, err := connect.CreateTun(ctx, tunSettings)
	if err != nil {
		device.Close()
		t.Fatalf("create tun: %v", err)
	}

	unsub := device.AddReceivePacketCallback(func(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte) {
		tun.Write(packet)
	})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		packets := make([][]byte, 64)
		for {
			n, err := tun.ReadBatch(packets)
			if err != nil {
				return
			}
			for _, packet := range packets[:n] {
				for !device.SendPacketNoCopy(packet, int32(len(packet))) {
					select {
					case <-device.Ctx().Done():
						MessagePoolReturn(packet)
						return
					default:
					}
					runtime.Gosched()
				}
			}
		}
	}()

	// run the brief load; a flow error here is not the subject of this test (the
	// stability/security tests assert transfer correctness), so ignore it.
	_ = runLoadIteration(ctx, tun, echoAddr, rounds, flows, bytesPerFlow)

	unsub()
	device.Close()
	tun.Close()
	wg.Wait()
}
