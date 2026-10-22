// Native descriptor-loop witnesses: these observe current synchronous ownership,
// not a new flow-isolation promise or a reproduction of a physical phone stall.

//go:build darwin || linux || android || ios

package sdk

import (
	"bytes"
	"context"
	"io"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// The route borrows the outer batch and takes every pooled packet on entry.
// Only synthetic copied bytes and scalar root charges cross the test barrier.
type ioLoopWitnessBatch struct {
	packets       [][]byte
	rootByteCount connect.ByteCount
	timeout       time.Duration
}

// One real route call stays held until the explicit response or device cancel.
// No second worker, packet queue, or retransmit policy is inserted into IoLoop.
type ioLoopWitnessRoute struct {
	packetBatchTestUserNat
	ctx       context.Context
	entered   chan ioLoopWitnessBatch
	responses chan int
}

// Takes every packet, including a partially refused batch, as the batch API does.
func (self *ioLoopWitnessRoute) SendPacketBatch(_ connect.TransferPath, _ protocol.ProvideMode, packets [][]byte, timeout time.Duration) int {
	call := ioLoopWitnessBatch{timeout: timeout}
	for _, packet := range packets {
		call.packets = append(call.packets, bytes.Clone(packet))
		call.rootByteCount += connect.MessagePoolRootByteCount(packet)
	}
	defer func() {
		for _, packet := range packets {
			connect.MessagePoolReturn(packet)
		}
	}()
	self.entered <- call
	select {
	case accepted := <-self.responses:
		return accepted
	case <-self.ctx.Done():
		return 0
	}
}

// Cold construction is shared; the test owns the opposite descriptor and device
// cancellation. Both production owners must join before any final observation.
type ioLoopWitnessFixture struct {
	loop        *IoLoop
	observation *ioLoopObservation
	route       *ioLoopWitnessRoute
	cancel      context.CancelFunc
	peer        io.ReadWriteCloser
	fd          int32
	poolCount   uint64
	poolBytes   connect.ByteCount
}

// The real public constructor path delegates to the same observed constructor;
// the production os.File/read/drain/send/unsubscribe/close bodies are unchanged.
func newIoLoopWitnessFixture(t *testing.T, preload [][]byte, now func() time.Time) *ioLoopWitnessFixture {
	t.Helper()
	for _, size := range []int{60, 1100} {
		connect.MessagePoolReturn(connect.MessagePoolGet(size))
	}
	previous := connect.SetMessagePoolViolationHandler(func(err error, stack []byte) {
		t.Errorf("native-loop pool violation: %v\n%s", err, stack)
	})
	t.Cleanup(func() { connect.SetMessagePoolViolationHandler(previous) })
	ctx, cancel := context.WithCancel(context.Background())
	route := &ioLoopWitnessRoute{
		ctx:       ctx,
		entered:   make(chan ioLoopWitnessBatch, 2),
		responses: make(chan int),
	}
	device := newPacketBatchTestDevice(route)
	device.ctx, device.cancel = ctx, cancel
	device.log = connect.NewNoopLogger()
	fd, peer := testingIoLoopSocketPair(t)
	for _, packet := range preload {
		if count, err := peer.Write(packet); err != nil || count != len(packet) {
			peer.Close()
			syscall.Close(int(fd))
			cancel()
			t.Fatalf("preload native datagram: count=%d err=%v", count, err)
		}
	}
	observation := newIoLoopObservation(now)
	fixture := &ioLoopWitnessFixture{
		observation: observation,
		route:       route,
		cancel:      cancel,
		peer:        peer,
		fd:          fd,
		poolCount:   connect.MessagePoolOutstandingCount(),
		poolBytes:   connect.MessagePoolOutstandingByteCount(),
	}
	t.Cleanup(func() {
		cancel()
		fixture.loop.Close()
		<-observation.runDone
		<-observation.fileClosed
		peer.Close()
		if got := connect.MessagePoolOutstandingCount(); got != fixture.poolCount {
			t.Errorf("joined native loop roots=%d want=%d", got, fixture.poolCount)
		}
		if got := connect.MessagePoolOutstandingByteCount(); got != fixture.poolBytes {
			t.Errorf("joined native loop root bytes=%d want=%d", got, fixture.poolBytes)
		}
	})
	fixture.loop = newIoLoopObserved(device, fd, nil, observation)
	return fixture
}

// Native datagrams are visibly synthetic and exercise both packet pool classes.
func ioLoopWitnessPacket(size int, marker byte) []byte {
	packet := make([]byte, size)
	for index := range packet {
		packet[index] = marker
	}
	return packet
}

// An already-read owned batch delays the next native read by current design.
// MSG_PEEK proves the next datagram is still kernel-owned at the held barrier;
// no sleep, queue-length polling, or assumed absence of a callback is the proof.
func TestIoLoopHeldSendLeavesNextDatagramNativeOwned(t *testing.T) {
	first := ioLoopWitnessPacket(60, 0x41)
	second := ioLoopWitnessPacket(1100, 0x52)
	var clock atomic.Int64
	fixture := newIoLoopWitnessFixture(t, [][]byte{first}, func() time.Time {
		return time.Unix(100, clock.Load())
	})
	call := <-fixture.route.entered
	if len(call.packets) != 1 || !bytes.Equal(call.packets[0], first) ||
		call.rootByteCount != 256 || call.timeout != 5*time.Second {
		t.Fatal("first actual native batch changed payload, root class, or send budget")
	}
	if count, err := fixture.peer.Write(second); err != nil || count != len(second) {
		t.Fatalf("next native datagram: count=%d err=%v", count, err)
	}
	var peek [2048]byte
	count, _, err := syscall.Recvfrom(int(fixture.fd), peek[:], syscall.MSG_PEEK|syscall.MSG_DONTWAIT)
	if err != nil || count != len(second) || !bytes.Equal(peek[:count], second) {
		t.Fatalf("held send did not leave exact next datagram in the native queue: count=%d err=%v", count, err)
	}
	if connect.MessagePoolOutstandingCount() != fixture.poolCount+1 ||
		connect.MessagePoolOutstandingByteCount() != fixture.poolBytes+256 {
		t.Fatal("unread native datagram acquired a pooled owner or first owner was lost")
	}
	clock.Store(int64(5 * time.Second))
	fixture.route.responses <- 1
	next := <-fixture.route.entered
	if len(next.packets) != 1 || !bytes.Equal(next.packets[0], second) || next.rootByteCount != 2048 {
		t.Fatal("release did not preserve the next native packet and its exact pool class")
	}
	fixture.cancel()
	<-fixture.observation.runDone
	<-fixture.observation.fileClosed
	got := fixture.observation
	if got.send.Count != 2 || got.offeredPacketCount != 2 || got.acceptedPacketCount != 1 ||
		got.refusedPacketCount != 1 || got.invalidResultCount != 0 ||
		got.send.MaximumNanoseconds != int64(5*time.Second) ||
		got.send.MaximumStartUnixNanoseconds != time.Unix(100, 0).UnixNano() ||
		got.send.MaximumEndUnixNanoseconds != time.Unix(105, 0).UnixNano() {
		t.Fatalf("joined send scalar observation did not preserve exact held interval: %+v", got)
	}
}

// The staging buffer is reused before forwarding, so independent copies must
// survive a complete 3-packet batch. Every result still consumes all 3 owners.
func TestIoLoopPartialBatchDispositionReturnsAllRoots(t *testing.T) {
	for _, accepted := range []int{0, 1, 3} {
		packets := [][]byte{
			ioLoopWitnessPacket(60, 0x41),
			ioLoopWitnessPacket(1100, 0x52),
			ioLoopWitnessPacket(60, 0x63),
		}
		nextRead := make(chan struct{})
		var clocks atomic.Int64
		fixture := newIoLoopWitnessFixture(t, packets, func() time.Time {
			// Primary read entry/end, send entry/end, then next read entry.
			if clocks.Add(1) == 5 {
				close(nextRead)
			}
			return time.Unix(100, 0)
		})
		call := <-fixture.route.entered
		if len(call.packets) != len(packets) || call.rootByteCount != 256+2048+256 {
			t.Fatalf("accepted=%d native ready batch/copy root mismatch", accepted)
		}
		for index, packet := range packets {
			if !bytes.Equal(call.packets[index], packet) {
				t.Fatalf("accepted=%d staging reuse changed packet %d", accepted, index)
			}
		}
		fixture.route.responses <- accepted
		<-nextRead
		fixture.loop.Close()
		<-fixture.observation.runDone
		<-fixture.observation.fileClosed
		got := fixture.observation
		if got.send.Count != 1 || got.offeredPacketCount != 3 ||
			got.acceptedPacketCount != uint64(accepted) || got.refusedPacketCount != uint64(3-accepted) ||
			got.invalidResultCount != 0 || got.fileCloseError {
			t.Fatalf("accepted=%d changed batch disposition or close: %+v", accepted, got)
		}
		fixture.cancel()
		fixture.peer.Close()
	}
}

// Loop Close owns the descriptor cancellation, not the selected route's device
// context. Keep that distinction observable without inventing an immediate join.
func TestIoLoopDeviceCancellationJoinsHeldSendAndFdClose(t *testing.T) {
	fixture := newIoLoopWitnessFixture(t, [][]byte{ioLoopWitnessPacket(60, 0x71)}, time.Now)
	<-fixture.route.entered
	fixture.loop.Close()
	<-fixture.observation.fileClosed
	if connect.MessagePoolOutstandingCount() != fixture.poolCount+1 ||
		connect.MessagePoolOutstandingByteCount() != fixture.poolBytes+256 {
		t.Fatal("descriptor close stole the still-held route's packet owner")
	}
	fixture.cancel()
	<-fixture.observation.runDone
	got := fixture.observation
	if got.send.Count != 1 || got.acceptedPacketCount != 0 || got.refusedPacketCount != 1 || got.fileCloseError {
		t.Fatalf("device cancellation did not settle actual batch ownership: %+v", got)
	}
}

// An invalid native descriptor exercises real os.File.Read failure and closes
// both existing owners without a packet or route invocation.
func TestIoLoopReadErrorJoinsWithoutPacketOwnership(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	route := &ioLoopWitnessRoute{ctx: ctx, entered: make(chan ioLoopWitnessBatch, 1), responses: make(chan int)}
	device := newPacketBatchTestDevice(route)
	device.ctx, device.cancel, device.log = ctx, cancel, connect.NewNoopLogger()
	beforeCount, beforeBytes := connect.MessagePoolOutstandingCount(), connect.MessagePoolOutstandingByteCount()
	observation := newIoLoopObservation(time.Now)
	loop := newIoLoopObserved(device, -1, nil, observation)
	defer loop.Close()
	<-observation.runDone
	<-observation.fileClosed
	if observation.read.Count != 1 || observation.readErrorCount != 1 || observation.readEofCount != 0 ||
		observation.send.Count != 0 || observation.offeredPacketCount != 0 || !observation.fileCloseError {
		t.Fatalf("invalid descriptor observation: %+v", observation)
	}
	if connect.MessagePoolOutstandingCount() != beforeCount || connect.MessagePoolOutstandingByteCount() != beforeBytes {
		t.Fatal("native read error changed pooled ownership")
	}
}

// A closed pipe writer is a real EOF control, separate from datagram framing.
// It invokes the same production blocking-read/terminal cleanup path.
func TestIoLoopNativeEofJoinsWithoutSend(t *testing.T) {
	beforeCount, beforeBytes := connect.MessagePoolOutstandingCount(), connect.MessagePoolOutstandingByteCount()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	fd, err := syscall.Dup(int(reader.Fd()))
	if err != nil {
		reader.Close()
		writer.Close()
		t.Fatal(err)
	}
	reader.Close()
	writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	route := &ioLoopWitnessRoute{ctx: ctx, entered: make(chan ioLoopWitnessBatch, 1), responses: make(chan int)}
	device := newPacketBatchTestDevice(route)
	device.ctx, device.cancel, device.log = ctx, cancel, connect.NewNoopLogger()
	observation := newIoLoopObservation(time.Now)
	loop := newIoLoopObserved(device, int32(fd), nil, observation)
	defer loop.Close()
	<-observation.runDone
	<-observation.fileClosed
	if observation.read.Count != 1 || observation.readEofCount != 1 || observation.readErrorCount != 0 ||
		observation.send.Count != 0 || observation.fileCloseError {
		t.Fatalf("native EOF observation: %+v", observation)
	}
	if connect.MessagePoolOutstandingCount() != beforeCount || connect.MessagePoolOutstandingByteCount() != beforeBytes {
		t.Fatal("native EOF changed pooled ownership")
	}
}

// Fixed scalar bookkeeping remains allocation-free and reports invalid returned
// counts rather than silently treating an impossible partial result as success.
func TestIoLoopScalarObservationIsBoundedAndFailClosed(t *testing.T) {
	start := time.Unix(100, 0)
	observation := newIoLoopObservation(func() time.Time { return start.Add(time.Second) })
	if allocations := testing.AllocsPerRun(100, func() {
		observation.recordRead(start, 60, nil)
		observation.recordSend(start, 3, 1)
	}); allocations != 0 {
		t.Fatalf("scalar observation allocations=%v", allocations)
	}
	offered := observation.offeredPacketCount
	accepted := observation.acceptedPacketCount
	refused := observation.refusedPacketCount
	observation.recordSend(start, 3, -1)
	observation.recordSend(start, 3, 4)
	if observation.invalidResultCount != 2 || observation.offeredPacketCount != offered+6 ||
		observation.acceptedPacketCount != accepted || observation.refusedPacketCount != refused {
		t.Fatal("invalid route results were laundered into successful admission")
	}
}
