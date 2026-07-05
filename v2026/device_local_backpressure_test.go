package sdk

import (
	"context"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// TestDeviceLocalRouteLocalOverloadBackpressure drives the routeLocal path in the
// burst regime the steady-state load test deliberately avoids (that test keeps its
// windows small so "no packet is ever dropped"): many parallel flows with large
// gvisor buffers burst into the LocalUserNat's shared 1024-packet send channel while
// a throttled echo server stalls the per-flow socket writes.
//
// Mechanism note (why "many flows" and not "one big flow"): the NAT advertises at
// most a 64 KiB window per TCP flow (no window scaling), so a single flow can never
// fill its own 1024-slot sequence queue, and the shared channel only fills from
// AGGREGATE burst arrival outpacing the single dispatcher — which is also exactly
// how the original field failure manifested (worst under -race, where dispatch runs
// ~30x slower). The bridge uses the PRODUCTION IoLoop semantics
// (device_local_ioloop.go): when SendPacketNoCopy returns false the packet is
// dropped, not retried.
//
// The contract under test is the sendPacket fix: the routeLocal send blocks up to
// SendTimeout (backpressure) instead of dropping, and LocalUserNat assumes a
// lossless in-order source — so across the whole run there must be NO drops and NO
// client-side TCP retransmits (a retransmit means a segment was lost after gvisor
// emitted it). With the non-blocking send (timeout 0) this guard is what fails
// whenever a burst finds the channel momentarily full.
func TestDeviceLocalRouteLocalOverloadBackpressure(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping overload backpressure test in -short mode")
	}

	const (
		flows        = 16
		bytesPerFlow = 256 << 10
		// throttle: the echo server reads the first slowBytes of each connection in
		// small delayed chunks. Each chunk delay stalls the NAT's per-flow socket
		// writes so the flows' window-refill bursts synchronize instead of smearing
		// out. The stalls are far below every stage's timeout (device SendTimeout 5s,
		// NAT WriteTimeout 15s), so a correct blocking chain absorbs them with zero
		// drops and zero retransmits.
		slowBytes  = 64 << 10
		slowChunk  = 8 << 10
		slowDelay  = 5 * time.Millisecond
		fastRounds = 1 // a fast warm-up round before the throttled overload round
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}

	settings := DefaultDeviceLocalSettings()
	settings.DisableLogging = true
	settings.Verbose = false
	device, err := newDeviceLocalWithOverrides(networkSpace, byJwt, "", "", "", NewId(), settings, connect.NewId())
	if err != nil {
		t.Fatalf("new device: %v", err)
	}
	device.SetRouteLocal(true)

	// LARGE windows: this is the point of the test. In-flight bytes per flow may reach
	// ~1 MiB, so the aggregate in-flight far exceeds the NAT's central channel
	// (1024 packets ~ 1.5 MiB) and the per-flow sequence buffers.
	tunSettings := connect.DefaultTunSettingsWithBufferSize(2048)
	tunSettings.Log = connect.NewNoopLogger()
	tunSettings.TcpSendBuffer = connect.TcpBufferRange{Min: 64 << 10, Default: 512 << 10, Max: 1 << 20}
	tunSettings.TcpReceiveBuffer = connect.TcpBufferRange{Min: 64 << 10, Default: 512 << 10, Max: 1 << 20}
	tun, err := connect.CreateTun(ctx, tunSettings)
	if err != nil {
		device.Close()
		t.Fatalf("create tun: %v", err)
	}

	unsub := device.AddReceivePacketCallback(func(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte) {
		tun.Write(packet)
	})

	// production-semantics bridge: drop (return to pool) when the device send fails.
	var drops atomic.Int64
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
				if !device.SendPacketNoCopy(packet, int32(len(packet))) {
					drops.Add(1)
					MessagePoolReturn(packet)
				}
			}
		}
	}()

	teardown := func() {
		unsub()
		device.Close()
		tun.Close()
		wg.Wait()
	}

	echoAddr, closeEcho := startThrottledTcpEchoServer(t, slowBytes, slowChunk, slowDelay)
	defer closeEcho()

	// fast warm-up (throttle applies per connection; these small flows barely feel it)
	for i := 0; i < fastRounds; i += 1 {
		if err := runLoadIteration(ctx, tun, echoAddr, 1, 2, 32<<10); err != nil {
			teardown()
			t.Fatalf("warm-up: %v", err)
		}
	}

	basePool := poolOutstanding()
	baseStacks := captureGoroutineStacks()

	// the overload round: all flows in parallel against the throttled server
	start := time.Now()
	errs := make(chan error, flows)
	for f := 0; f < flows; f += 1 {
		payload := makeLoadPayload(100+f, bytesPerFlow)
		go func() {
			errs <- runEchoFlow(ctx, tun, echoAddr, payload)
		}()
	}
	for f := 0; f < flows; f += 1 {
		if err := <-errs; err != nil {
			t.Errorf("overload flow: %v", err)
		}
	}
	elapsed := time.Since(start)

	stats := tun.Stats()
	retransmits := stats.TCP.Retransmits.Value()
	rtoTimeouts := stats.TCP.Timeouts.Value()
	t.Logf("overload: %d flows x %s in %v, bridge drops=%d, tcp retransmits=%d rto timeouts=%d",
		flows, humanBytes(bytesPerFlow), elapsed.Round(time.Millisecond), drops.Load(), retransmits, rtoTimeouts)

	// The core backpressure contract, asserted in every build: the routeLocal send
	// must block (apply backpressure) rather than drop, so the bridge -- which uses
	// production IoLoop drop semantics -- never drops a packet. This is the exact
	// regression guard for the sendPacket SendTimeout fix (with a non-blocking send
	// this count is non-zero whenever a burst finds the NAT channel momentarily full).
	if d := drops.Load(); d != 0 {
		t.Errorf("device send dropped %d packets under overload; the routeLocal send must block (backpressure), not drop", d)
	}

	// Zero client-side retransmits is a stronger check that no segment was lost
	// anywhere, but it only holds at full speed. Under -race gvisor runs ~30x
	// slower, so the throttled round-trip routinely exceeds gvisor's 200ms minimum
	// RTO and it fires SPURIOUS retransmits with no actual loss (drops==0 above
	// proves nothing was dropped). So assert it only without -race, matching the
	// raceEnabled gating used elsewhere in this package.
	if !raceEnabled && (retransmits != 0 || rtoTimeouts != 0) {
		t.Errorf("client tcp saw loss under overload: retransmits=%d rto timeouts=%d, want 0/0 (the source->NAT link must be lossless)",
			retransmits, rtoTimeouts)
	}

	teardown()
	// settle before the leak checks: tun.Close() releases the gvisor stack's
	// goroutines asynchronously (it deliberately does not Wait), so poll to a stable
	// goroutine count before diffing, the same way the churn/load tests do.
	sampleStable()
	reportGoroutineLeaks(t, baseStacks, captureGoroutineStacks(), 5)
	reportPoolLeaks(t, basePool, 8)
}

// startThrottledTcpEchoServer starts a loopback echo server that reads each
// connection's first slowBytes in chunk-sized reads separated by delay, then echoes
// at full speed. The slow phase stalls the NAT's egress socket writes, which is what
// pushes the backpressure all the way up the chain to DeviceLocal.sendPacket.
func startThrottledTcpEchoServer(t *testing.T, slowBytes int, chunk int, delay time.Duration) (addr string, closeFn func()) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("throttled echo listen: %v", err)
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
				buf := make([]byte, chunk)
				total := 0
				for total < slowBytes {
					n, err := conn.Read(buf)
					if 0 < n {
						if _, werr := conn.Write(buf[:n]); werr != nil {
							return
						}
						total += n
					}
					if err != nil {
						return
					}
					time.Sleep(delay)
				}
				io.Copy(conn, conn)
			}()
		}
	}()
	return ln.Addr().String(), func() {
		ln.Close()
		wg.Wait()
	}
}

// TestDeviceLocalIoLoopEndToEnd drives the REAL production io loop
// (device_local_ioloop.go) instead of a test bridge: packets flow tun <-> unix
// datagram socketpair <-> IoLoop <-> DeviceLocal, exercising the loop's pool
// ownership (MessagePoolGet / SendPacketNoCopy / MessagePoolReturn), its receive
// write path, and its shutdown. The test then closes the IoLoop on a QUIET link and
// requires its goroutine and fd to be released promptly — a reader parked in f.Read
// with no traffic must be unblocked by Close, or every stop/start of the platform
// tunnel leaks a goroutine and a file descriptor.
func TestDeviceLocalIoLoopEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping io loop end-to-end test in -short mode")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}
	echoAddr, closeEcho := startTcpEchoServer(t)
	defer closeEcho()

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
	tun, err := connect.CreateTun(ctx, tunSettings)
	if err != nil {
		device.Close()
		t.Fatalf("create tun: %v", err)
	}

	// a unix datagram socketpair stands in for the platform TUN fd: datagram
	// framing preserves packet boundaries, one datagram = one IP packet.
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	for _, fd := range fds {
		syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_SNDBUF, 512<<10)
		syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, 512<<10)
	}
	// the io loop contract requires a non-blocking fd (it is handed to os.NewFile,
	// which then uses the runtime poller)
	syscall.SetNonblock(fds[0], true)
	syscall.SetNonblock(fds[1], true)
	testSide := os.NewFile(uintptr(fds[1]), "test-tun")
	defer testSide.Close()

	// test side of the socketpair <-> gvisor tun. The two directions get separate
	// wait groups so the teardown can quiesce the tun->fd direction first (see below)
	// while the fd->tun reader stays up until the fd closes.
	var writerWg sync.WaitGroup
	var readerWg sync.WaitGroup
	writerWg.Add(1)
	go func() {
		defer writerWg.Done()
		packets := make([][]byte, 64)
		for {
			n, err := tun.ReadBatch(packets)
			if err != nil {
				return
			}
			for _, packet := range packets[:n] {
				testSide.Write(packet)
				MessagePoolReturn(packet)
			}
		}
	}()
	readerWg.Add(1)
	go func() {
		defer readerWg.Done()
		buf := make([]byte, 2048)
		for {
			n, err := testSide.Read(buf)
			if 0 < n {
				tun.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	baseStacks := captureGoroutineStacks()
	basePool := poolOutstanding()
	baseFds := openFdCount()

	ioLoop := NewIoLoop(device, int32(fds[0]), nil)

	// real traffic through the production loop
	if err := runLoadIteration(ctx, tun, echoAddr, 4, 2, 64<<10); err != nil {
		t.Errorf("io loop load: %v", err)
	}
	// the loop plus its ctx watcher; the exact count is an implementation detail,
	// what matters is presence here and absence after Close
	if n := goroutinesWithFrame("sdk.(*IoLoop).run"); n == 0 {
		t.Error("expected IoLoop.run goroutines during traffic, found none")
	}

	// make the link DEFINITIVELY quiet before Close: closing the tun stops the
	// gvisor stack and the tun->fd writer, so nothing can arrive to wake a reader
	// parked in f.Read. Only then does Close's own unblocking behavior get tested —
	// with traffic still flowing, any stray packet would mask a parked-reader leak.
	tun.Close()
	writerWg.Wait()
	time.Sleep(200 * time.Millisecond)

	ioLoop.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if n := goroutinesWithFrame("sdk.(*IoLoop).run"); n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("IoLoop.run goroutine still parked in f.Read %v after Close on a quiet link (Close must unblock the fd read; every tunnel stop/start leaks a goroutine and an fd)", 5*time.Second)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	device.Close()
	testSide.Close()
	readerWg.Wait()

	sampleStable()
	reportGoroutineLeaks(t, baseStacks, captureGoroutineStacks(), 2)
	reportPoolLeaks(t, basePool, 8)
	if fds := openFdCount(); baseFds >= 0 && fds > baseFds {
		t.Errorf("file descriptors leaked across io loop lifecycle: %d -> %d", baseFds, fds)
	}
}

// goroutinesWithFrame counts live goroutines whose raw stack contains substr. It
// works on the raw dump (not captureGoroutineStacks signatures, which truncate each
// frame at the first paren and so mangle pointer-receiver method names like
// "(*IoLoop).run").
func goroutinesWithFrame(substr string) int {
	var buf []byte
	for size := 1 << 20; ; size *= 2 {
		buf = make([]byte, size)
		if n := runtime.Stack(buf, true); n < size {
			buf = buf[:n]
			break
		}
	}
	n := 0
	for _, block := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(block, substr) {
			n += 1
		}
	}
	return n
}
