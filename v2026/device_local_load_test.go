package sdk

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// TestDeviceLocalLoadStability drives real IP traffic through a real DeviceLocal
// and checks that goroutines and heap stay flat across repeated load, then
// return to baseline once the device is torn down.
//
// Topology (fully in-process, no platform/credentials needed):
//
//	net.Conn (from tun.DialContext)
//	  -> gvisor Tun  -> DeviceLocal.SendPacket
//	  -> provider LocalUserNat (routeLocal)  -> loopback TCP echo server
//	  -> DeviceLocal.receive  -> Tun.Write  -> net.Conn.Read
//
// The default settings already route locally through the provider's LocalUserNat
// (DefaultRouteLocal=true, AllowProvider=true), and neither the send nor the
// receive path is gated by offline/tunnelStarted, so the device moves real bytes
// on its defaults.
//
// Methodology notes (this is what keeps the test meaningful rather than flaky):
//   - A warm-up cycle runs the full load once BEFORE the baseline is captured, so
//     lazy process-global goroutines and message-pool buffers are already
//     initialized and counted in the baseline. Without this, "return to baseline"
//     is unachievable.
//   - Goroutine/heap samples are taken after GC + FreeOSMemory and a poll-to-settle
//     loop (workers exit asynchronously after their flows close) rather than a
//     fixed sleep.
//   - Heap is compared with a tolerance band, not equality: pooled buffers legit-
//     imately persist. The real leak signal is the absence of monotonic growth
//     across iterations, plus return to baseline after teardown. Goroutines are
//     checked more tightly.
//   - The measured device stays up across all iterations, so the check targets
//     intra-device leaks (contracts, sequences, per-flow buffers).
//   - Iterations get progressively longer (more sequential connections) at a
//     fixed concurrency. That makes the steady-state footprint invariant to
//     iteration length, so the quiesced heap must stay flat as the iterations
//     grow; any climb is a per-connection / per-transfer leak whose signal
//     compounds with the growing load.
func TestDeviceLocalLoadStability(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping DeviceLocal load stability test in -short mode")
	}

	const (
		measureIterations = 6
		// iteration i (1-based) runs i*roundsStep sequential rounds, so each
		// iteration is strictly longer than the last (more connections, more
		// bytes). Concurrency is fixed at flowsPerIteration, so the steady-state
		// footprint is invariant to iteration length -- any heap growth as the
		// iterations lengthen is a per-connection / per-transfer leak.
		roundsStep        = 4 // sequential rounds added per iteration
		flowsPerIteration = 2 // parallel flows per round (kept low so the
		//                              lossless NAT channel never overflows)
		bytesPerFlow = 128 << 10 // 128 KiB per flow
		// warm up with the heaviest load (== the longest measured iteration) so the
		// process-global message-pool high-water mark is fully established before the
		// baseline is captured. The measured iterations then never grow it, which
		// isolates any true length-proportional leak from benign pool growth.
		warmupRounds = measureIterations * roundsStep

		// warm region = iterations 2..N. Iteration 1 additionally pays device
		// first-touch allocation, so it is excluded from the growth check.
		warmFromIndex = 1 // 0-based -> iteration 2

		// goroutine count is invariant to iteration length (fixed concurrency),
		// so it must stay flat across the progressively longer iterations.
		goroutineIterationDrift = 8

		// heap growth budget across the warm region. Because the footprint is
		// length-invariant, the spread between the shortest and longest warm
		// iteration must stay within this band. Generous enough to absorb GC/race
		// noise (observed spread < 0.5 MiB) yet catch a work-proportional leak.
		heapGrowthBandBytes = 6 << 20 // 6 MiB

		// return-to-baseline tolerances after teardown.
		goroutineBaselineTolerance = 8
		goroutineStackTolerance    = 5        // per goroutine-stack signature
		fdBaselineTolerance        = 8        // open file descriptors
		heapBaselineBandBytes      = 16 << 20 // 16 MiB
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// scaffolding that stays up for the whole test (present in both the baseline
	// and the final sample, so it cancels out of the return-to-baseline check).
	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}
	echoAddr, closeEcho := startTcpEchoServer(t)
	defer closeEcho()

	// newEnv builds a real DeviceLocal, a gvisor Tun as the client packet source,
	// and the bidirectional bridge between them (mirroring device_local_ioloop.go).
	// It returns a teardown that closes everything and joins the bridge goroutine.
	newEnv := func() (device *DeviceLocal, tun *connect.Tun, teardown func()) {
		settings := DefaultDeviceLocalSettings()
		settings.DisableLogging = true // keep the load quiet
		settings.Verbose = false       // no security policy monitor goroutine
		// DefaultRouteLocal / AllowProvider stay true -> route through LocalUserNat

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
		device.SetRouteLocal(true) // explicit; already the default

		// The LocalUserNat assumes the source->NAT link is lossless and in-order
		// (ip.go: "do not implement any retransmit logic"), but DeviceLocal's
		// routeLocal send is non-blocking and drops when the NAT's send channel
		// (SequenceBufferSize=1024 packets) is full. A dropped packet corrupts the
		// NAT's per-flow tcp state and stalls the flow to its deadline. So keep the
		// tun tcp windows small: bounded in-flight bytes stay far below the channel
		// depth, and no packet is ever dropped. A larger channel buffer further
		// absorbs bursts.
		tunSettings := connect.DefaultTunSettingsWithBufferSize(2048)
		tunSettings.Log = connect.NewNoopLogger()
		tunSettings.TcpSendBuffer = connect.TcpBufferRange{Min: 4 * 1024, Default: 32 * 1024, Max: 64 * 1024}
		tunSettings.TcpReceiveBuffer = connect.TcpBufferRange{Min: 4 * 1024, Default: 32 * 1024, Max: 64 * 1024}
		tun, err = connect.CreateTun(ctx, tunSettings)
		if err != nil {
			device.Close()
			t.Fatalf("create tun: %v", err)
		}

		// device -> tun: Write copies into the gvisor stack, so `packet` is not
		// retained past the callback.
		unsub := device.AddReceivePacketCallback(func(
			source connect.TransferPath,
			provideMode protocol.ProvideMode,
			ipPath *connect.IpPath,
			packet []byte,
		) {
			tun.Write(packet)
		})

		// tun -> device: batch-drain the tun (the way ProxyDevice.Run drains it) so
		// a burst is one wakeup instead of one per packet.
		var retries atomic.Int64
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
					// The send path assumes a lossless, in-order source, but the
					// non-blocking send returns false when the downstream channel is
					// momentarily full (we keep ownership of the packet on false).
					// Retry instead of dropping so a transient full channel never
					// corrupts a flow; give up only once the device is closed. On
					// success the downstream owns the pooled packet.
					for !device.SendPacketNoCopy(packet, int32(len(packet))) {
						select {
						case <-device.Ctx().Done():
							MessagePoolReturn(packet)
							return
						default:
						}
						retries.Add(1)
						runtime.Gosched()
					}
				}
			}
		}()

		teardown = func() {
			unsub()
			device.Close() // cancels device ctx, closes provider + mux/nat
			tun.Close()    // unblocks tun.Read -> bridge goroutine exits
			wg.Wait()
			if r := retries.Load(); r > 0 {
				t.Logf("bridge retried %d sends (transient downstream backpressure; no packets dropped)", r)
			}
		}
		return device, tun, teardown
	}

	// warm-up: run the full load once so all global pools/goroutines reach steady
	// state before the baseline is captured. Uses its own device, then tears down.
	func() {
		_, tun, teardown := newEnv()
		defer teardown()
		if err := runLoadIteration(ctx, tun, echoAddr, warmupRounds, flowsPerIteration, bytesPerFlow); err != nil {
			t.Fatalf("warm-up load: %v", err)
		}
	}()

	baseGoroutines, baseHeap := sampleStable()
	baseStacks := captureGoroutineStacks()
	baseFds := openFdCount()
	t.Logf("baseline: goroutines=%d heap=%s fds=%d", baseGoroutines, humanBytes(baseHeap), baseFds)

	// the measured device stays up across every iteration.
	_, tun, teardown := newEnv()

	gSamples := make([]int, measureIterations)
	hSamples := make([]uint64, measureIterations)
	for i := 0; i < measureIterations; i++ {
		rounds := (i + 1) * roundsStep
		start := time.Now()
		if err := runLoadIteration(ctx, tun, echoAddr, rounds, flowsPerIteration, bytesPerFlow); err != nil {
			teardown()
			t.Fatalf("iteration %d load: %v", i+1, err)
		}
		elapsed := time.Since(start)

		g, h := sampleStable()
		gSamples[i] = g
		hSamples[i] = h

		conns := rounds * flowsPerIteration
		bytesMoved := int64(conns) * int64(bytesPerFlow)
		mibs := float64(bytesMoved) / (1 << 20) / elapsed.Seconds()
		t.Logf("iteration %d: rounds=%d conns=%d goroutines=%d heap=%s (%.1f MiB in %v = %.1f MiB/s)",
			i+1, rounds, conns, g, humanBytes(h), float64(bytesMoved)/(1<<20), elapsed.Round(time.Millisecond), mibs)
	}

	// goroutines are invariant to iteration length (fixed concurrency); they must
	// stay flat across the progressively longer iterations.
	refG := gSamples[warmFromIndex]
	for i := warmFromIndex; i < measureIterations; i++ {
		if gSamples[i] > refG+goroutineIterationDrift {
			t.Errorf("iteration %d goroutines=%d drifted above iteration %d=%d (tol +%d) despite fixed concurrency",
				i+1, gSamples[i], warmFromIndex+1, refG, goroutineIterationDrift)
		}
	}

	// heap must not climb as the iterations get longer. Across the warm region the
	// footprint is length-invariant, so the quiesced-heap spread between the
	// shortest and longest warm iteration must stay within the growth band; a
	// per-connection leak would make the longest (heaviest) iteration the largest.
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
		t.Errorf("heap climbed as iterations lengthened: max=%s min=%s spread=%s exceeds growth band +%s",
			humanBytes(maxWarmHeap), humanBytes(minWarmHeap),
			humanBytes(maxWarmHeap-minWarmHeap), humanBytes(heapGrowthBandBytes))
	}

	// teardown the measured environment and confirm return to baseline.
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

// runLoadIteration runs `rounds` sequential rounds of `flows` parallel tcp echo
// flows through the tun, each transferring `bytesPerFlow`. Sequential rounds
// churn connections (open/close) while parallel flows within a round create
// concurrent load. Returns the first flow error, if any.
func runLoadIteration(ctx context.Context, tun *connect.Tun, addr string, rounds int, flows int, bytesPerFlow int) error {
	for round := 0; round < rounds; round++ {
		errs := make(chan error, flows)
		for f := 0; f < flows; f++ {
			payload := makeLoadPayload(round*flows+f, bytesPerFlow)
			go func() {
				errs <- runEchoFlow(ctx, tun, addr, payload)
			}()
		}
		for f := 0; f < flows; f++ {
			if err := <-errs; err != nil {
				return err
			}
		}
	}
	return nil
}

// runEchoFlow dials the loopback echo server through the tun, writes the payload
// while concurrently reading the echo back, and verifies the round trip.
func runEchoFlow(ctx context.Context, tun *connect.Tun, addr string, payload []byte) error {
	conn, err := tun.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	// generous deadline: under -race the gvisor stack runs ~30x slower, so this
	// is a stall detector, not a throughput target.
	const flowDeadline = 120 * time.Second

	readErr := make(chan error, 1)
	go func() {
		echo := make([]byte, len(payload))
		conn.SetReadDeadline(time.Now().Add(flowDeadline))
		if _, err := io.ReadFull(conn, echo); err != nil {
			readErr <- fmt.Errorf("read: %w", err)
			return
		}
		if !bytes.Equal(payload, echo) {
			readErr <- fmt.Errorf("echo mismatch (%d bytes)", len(payload))
			return
		}
		readErr <- nil
	}()

	conn.SetWriteDeadline(time.Now().Add(flowDeadline))
	if _, err := conn.Write(payload); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return <-readErr
}

// makeLoadPayload builds a deterministic payload so a corrupted, reordered, or
// cross-flow echo is detected.
func makeLoadPayload(seed int, size int) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(31*seed + i)
	}
	return b
}

// startTcpEchoServer starts a loopback tcp echo server and returns its address
// and a close function that stops accepting and joins in-flight handlers.
func startTcpEchoServer(t *testing.T) (addr string, closeFn func()) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	var wg sync.WaitGroup
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				io.Copy(conn, conn)
			}()
		}
	}()
	return ln.Addr().String(), func() {
		ln.Close()
		wg.Wait()
	}
}

// sampleStable forces a collection, waits for the goroutine count to settle
// (workers wind down asynchronously after their flows close), and returns the
// settled goroutine count and live heap. It polls with a cap instead of sleeping
// a fixed duration.
func sampleStable() (goroutines int, heapBytes uint64) {
	const (
		settleInterval = 50 * time.Millisecond
		settleChecks   = 8 // consecutive equal samples => settled
		settleTimeout  = 20 * time.Second
	)

	runtime.GC()
	runtime.GC()
	debug.FreeOSMemory()

	deadline := time.Now().Add(settleTimeout)
	prev := runtime.NumGoroutine()
	stable := 0
	for {
		time.Sleep(settleInterval)
		runtime.GC()
		n := runtime.NumGoroutine()
		if n == prev {
			stable++
			if stable >= settleChecks {
				break
			}
		} else {
			stable = 0
			prev = n
		}
		if time.Now().After(deadline) {
			break
		}
	}

	runtime.GC()
	debug.FreeOSMemory()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return runtime.NumGoroutine(), m.HeapAlloc
}

func humanBytes(b uint64) string {
	return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
}

// sampleHeap returns live heap after a full collection, without the goroutine
// settle loop. Use it when only the heap delta matters (e.g. before/after an idle
// period) and goroutine counts are not the signal.
func sampleHeap() uint64 {
	runtime.GC()
	runtime.GC()
	debug.FreeOSMemory()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}
