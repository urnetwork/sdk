package sdk

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// packetFloodBackpressureUserNat is a bounded, deliberately slow route. The
// queue owns admitted packet batches; callers remain synchronously blocked
// when it is full, exactly like DeviceLocal's production UserNat boundary.
type packetFloodBackpressureUserNat struct {
	queue   chan [][]byte
	start   chan struct{}
	started sync.Once
	done    chan struct{}

	activeCalls    atomic.Int64
	maxActiveCalls atomic.Int64
	batchCount     atomic.Int64
	packetCount    atomic.Int64
	closeOnce      sync.Once
}

func newPacketFloodBackpressureUserNat(queueSize int) *packetFloodBackpressureUserNat {
	self := &packetFloodBackpressureUserNat{
		queue: make(chan [][]byte, queueSize),
		start: make(chan struct{}),
		done:  make(chan struct{}),
	}
	go func() {
		defer close(self.done)
		<-self.start
		for packets := range self.queue {
			// Keep the queue saturated long enough for every flood stream to
			// exercise the blocking edge. No lock is held during this delay.
			time.Sleep(50 * time.Microsecond)
			for _, packet := range packets {
				connect.MessagePoolReturn(packet)
			}
		}
	}()
	return self
}

func (self *packetFloodBackpressureUserNat) release() {
	self.started.Do(func() { close(self.start) })
}

func (self *packetFloodBackpressureUserNat) updateMaxActive(candidate int64) {
	for current := self.maxActiveCalls.Load(); current < candidate; current = self.maxActiveCalls.Load() {
		if self.maxActiveCalls.CompareAndSwap(current, candidate) {
			return
		}
	}
}

func (self *packetFloodBackpressureUserNat) SendPacket(
	source connect.TransferPath,
	provideMode protocol.ProvideMode,
	packet []byte,
	timeout time.Duration,
) bool {
	return self.SendPacketBatch(
		source,
		provideMode,
		[][]byte{packet},
		timeout,
	) == 1
}

func (self *packetFloodBackpressureUserNat) SendPacketBatch(
	_ connect.TransferPath,
	_ protocol.ProvideMode,
	packets [][]byte,
	_ time.Duration,
) int {
	active := self.activeCalls.Add(1)
	self.updateMaxActive(active)
	self.queue <- packets
	self.activeCalls.Add(-1)
	self.batchCount.Add(1)
	self.packetCount.Add(int64(len(packets)))
	return len(packets)
}

func (self *packetFloodBackpressureUserNat) Close() {
	self.closeOnce.Do(func() {
		self.release()
		close(self.queue)
		<-self.done
	})
}

func (*packetFloodBackpressureUserNat) Shuffle() {}

func (*packetFloodBackpressureUserNat) SecurityPolicyStats(bool) connect.SecurityPolicyStats {
	return connect.SecurityPolicyStats{}
}

func (*packetFloodBackpressureUserNat) SetLocalSecurityBypass(bool) {}

// TestDeviceLocalParallelPacketFloodMemoryBounded locks the native packet
// tunnel's most important pressure contract: SendPacketBatch is synchronous,
// so a full downstream queue can retain at most one copied batch per calling
// stream plus the route's explicit queue. It must not spawn packet workers,
// accumulate a hidden queue, or hold DeviceLocal.stateLock while blocked.
func TestDeviceLocalParallelPacketFloodMemoryBounded(t *testing.T) {
	const (
		floodStreams     = 8
		batchesPerStream = 128
		packetsPerBatch  = devicePacketBatchMaxPacketCount
		packetByteCount  = 1380
		routeQueueSize   = 2
	)

	packets := make([][]byte, packetsPerBatch)
	for packetIndex := range packets {
		packet := make([]byte, packetByteCount)
		packet[0] = 0x45
		packet[1] = byte(packetIndex)
		packets[packetIndex] = packet
	}
	encodedBatch := packetBatchTestEncode(packets)
	if len(encodedBatch) > devicePacketBatchMaxByteCount {
		t.Fatalf("flood fixture is %d bytes, bridge limit %d", len(encodedBatch), devicePacketBatchMaxByteCount)
	}

	sink := newPacketFloodBackpressureUserNat(routeQueueSize)
	device := newPacketBatchTestDevice(sink)
	// Populate the mutable routing fields too, so the concurrent publication
	// below rebuilds an equivalent snapshot instead of changing the route.
	device.stateLock.Lock()
	device.remoteUserNatClient = sink
	device.updateSendRouteWithLock()
	device.stateLock.Unlock()

	basePool := poolOutstanding()
	baseGoroutines := runtime.NumGoroutine()
	baseHeap := sampleHeap()

	var peakHeap atomic.Uint64
	var peakGoroutines atomic.Int64
	sample := func() {
		var memory runtime.MemStats
		runtime.ReadMemStats(&memory)
		for current := peakHeap.Load(); current < memory.HeapAlloc; current = peakHeap.Load() {
			if peakHeap.CompareAndSwap(current, memory.HeapAlloc) {
				break
			}
		}
		goroutines := int64(runtime.NumGoroutine())
		for current := peakGoroutines.Load(); current < goroutines; current = peakGoroutines.Load() {
			if peakGoroutines.CompareAndSwap(current, goroutines) {
				break
			}
		}
	}
	sample()
	samplerStop := make(chan struct{})
	samplerDone := make(chan struct{})
	go func() {
		defer close(samplerDone)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			sample()
			select {
			case <-samplerStop:
				return
			case <-ticker.C:
			}
		}
	}()

	var senders sync.WaitGroup
	senders.Add(floodStreams)
	var acceptedPackets atomic.Int64
	for range floodStreams {
		go func() {
			defer senders.Done()
			for range batchesPerStream {
				acceptedPackets.Add(int64(device.SendPacketBatch(encodedBatch)))
			}
		}()
	}

	// With the consumer paused, two batches reside in its bounded queue and
	// exactly one additional batch per stream can be blocked in the synchronous
	// call. This is the maximum live native-to-Go packet wave.
	saturationDeadline := time.Now().Add(5 * time.Second)
	for sink.activeCalls.Load() != floodStreams && time.Now().Before(saturationDeadline) {
		runtime.Gosched()
	}
	if active := sink.activeCalls.Load(); active != floodStreams {
		t.Errorf("packet flood did not saturate all streams: active=%d want=%d", active, floodStreams)
	}
	wantOutstanding := int64((floodStreams + routeQueueSize) * packetsPerBatch)
	if outstanding := poolOutstanding() - basePool; outstanding != wantOutstanding {
		t.Errorf(
			"saturated packet ownership=%d buffers, want bounded stream+queue wave %d",
			outstanding,
			wantOutstanding,
		)
	}

	// A route publication must not wait behind downstream packet pressure. If
	// SendPacketBatch held stateLock across the blocking call, this would time
	// out deterministically before the sink is released.
	reconfigured := make(chan struct{})
	go func() {
		device.stateLock.Lock()
		device.updateSendRouteWithLock()
		device.stateLock.Unlock()
		close(reconfigured)
	}()
	select {
	case <-reconfigured:
	case <-time.After(time.Second):
		t.Error("route publication blocked behind packet backpressure")
	}

	sink.release()
	if !waitTimeout(&senders, 30*time.Second) {
		t.Error("parallel packet flood did not drain; possible backpressure deadlock")
	}
	sink.Close()
	close(samplerStop)
	<-samplerDone
	sample()

	wantPackets := int64(floodStreams * batchesPerStream * packetsPerBatch)
	if accepted := acceptedPackets.Load(); accepted != wantPackets {
		t.Errorf("accepted packet count=%d, want=%d", accepted, wantPackets)
	}
	if sink.packetCount.Load() != wantPackets ||
		sink.batchCount.Load() != floodStreams*batchesPerStream {
		t.Errorf(
			"sink packets/batches=%d/%d, want=%d/%d",
			sink.packetCount.Load(),
			sink.batchCount.Load(),
			wantPackets,
			floodStreams*batchesPerStream,
		)
	}
	if maxActive := sink.maxActiveCalls.Load(); maxActive > floodStreams {
		t.Errorf("route observed %d concurrent calls, stream bound %d", maxActive, floodStreams)
	}
	reportPoolLeaks(t, basePool, 0)

	const (
		peakHeapAllowance      = 12 * 1024 * 1024
		recoveredHeapAllowance = 4 * 1024 * 1024
	)
	if peak := peakHeap.Load(); peak > baseHeap+peakHeapAllowance {
		t.Errorf(
			"parallel flood heap spike exceeded bound: baseline=%s peak=%s allowance=+%s",
			humanBytes(baseHeap),
			humanBytes(peak),
			humanBytes(peakHeapAllowance),
		)
	}
	recoveredHeap := sampleHeap()
	if recoveredHeap > baseHeap+recoveredHeapAllowance {
		t.Errorf(
			"parallel flood heap did not recover: baseline=%s recovered=%s allowance=+%s",
			humanBytes(baseHeap),
			humanBytes(recoveredHeap),
			humanBytes(recoveredHeapAllowance),
		)
	}
	if peak := peakGoroutines.Load(); peak > int64(baseGoroutines+floodStreams+4) {
		t.Errorf(
			"parallel flood created unbounded workers: baseline=%d peak=%d stream allowance=+%d",
			baseGoroutines,
			peak,
			floodStreams+4,
		)
	}
	t.Logf(
		"packet flood: streams=%d packets=%d baseline=%s peak=%s recovered=%s max-active=%d",
		floodStreams,
		wantPackets,
		humanBytes(baseHeap),
		humanBytes(peakHeap.Load()),
		humanBytes(recoveredHeap),
		sink.maxActiveCalls.Load(),
	)
}
