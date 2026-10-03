package sdk

import (
	"bytes"
	"context"
	"io"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect"
)

// These tests use the real mux producer, writer, reader and final drain with
// in-memory websocket boundaries. Each bubble owns its contexts and workers.
// A retained read-only frame reference proves the production owner's return
// without depending on pool counters changed by unrelated SDK workers.

func runDeviceRpcSendDrainSynctest(t *testing.T, f func(*testing.T)) {
	t.Helper()
	// Lazy message-pool initialization starts its process-lifetime stats
	// worker. Keep that worker outside the bubble while the mux workers remain
	// bubble-owned.
	connect.GetMessagePoolAggregateStats()
	synctest.Test(t, f)
}

func TestDeviceRpcByteBudgetRejectsCanceledAvailableCapacity(t *testing.T) {
	runDeviceRpcSendDrainSynctest(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		budget := newDeviceRpcByteBudget(8)
		if budget.acquire(ctx, 4) {
			t.Error("canceled producer acquired available bytes")
			budget.release(4)
		}
		if !budget.acquire(context.Background(), 4) {
			t.Fatal("healthy producer could not acquire available bytes")
		}
		budget.release(4)
		if budget.used != 0 {
			t.Errorf("healthy release left %d bytes", budget.used)
		}
	})
}

func TestDeviceRpcMuxFinalDrainJoinsHeldForwardProducer(t *testing.T) {
	testDeviceRpcMuxFinalDrainJoinsHeldProducer(t, deviceRpcStreamForward)
}

func TestDeviceRpcMuxFinalDrainJoinsHeldReverseProducer(t *testing.T) {
	testDeviceRpcMuxFinalDrainJoinsHeldProducer(t, deviceRpcStreamReverse)
}

func testDeviceRpcMuxFinalDrainJoinsHeldProducer(t *testing.T, tag uint8) {
	runDeviceRpcSendDrainSynctest(t, func(t *testing.T) {
		mux := newDeviceRpcSendDrainTestMux(t, newBoundedDeviceRpcTestWs(nil), nil)
		admitted := make(chan struct{})
		releaseProducer := make(chan struct{})
		result := make(chan deviceRpcSendDrainTestResult, 1)
		var observed []byte
		go func() {
			n, err := mux.conns[tag].write([]byte("abc"), func(frame []byte) {
				observed = connect.MessagePoolShareReadOnly(frame)
				close(admitted)
				<-releaseProducer
			})
			result <- deviceRpcSendDrainTestResult{n: n, err: err}
		}()
		<-admitted
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			mux.finishSend()
		}()

		// Both the producer barrier and the join are durably blocked here.
		// Removing the join makes finished close before this Wait returns,
		// regardless of which select arm the later producer eventually chooses.
		synctest.Wait()
		select {
		case <-finished:
			t.Error("final drain finished while an admitted producer still owned its frame")
		default:
		}
		if !deviceRpcSendDrainTestSendClosed(mux) || mux.ctx.Err() == nil {
			t.Error("teardown did not close admission and cancel before joining")
		}
		if mux.sendBytes.used != 4 || len(mux.send) != 0 {
			t.Errorf("held producer ownership = %d bytes, %d queued frames", mux.sendBytes.used, len(mux.send))
		}

		close(releaseProducer)
		synctest.Wait()
		<-finished
		got := <-result
		if !((got.n == 0 && got.err == io.ErrClosedPipe) || (got.n == 3 && got.err == nil)) {
			t.Errorf("canceled/ready handoff returned n=%d err=%v", got.n, got.err)
		}
		assertDeviceRpcSendDrained(t, mux)
		assertDeviceRpcObservedFrameReturned(t, observed)
		// A failing old-order run may leave its late frame queued. Cleanup is
		// after all ownership assertions and after the first final drain joined.
		mux.finishSend()
	})
}

func TestDeviceRpcMuxWriteAfterFinalDrainRejectsBeforeCopy(t *testing.T) {
	runDeviceRpcSendDrainSynctest(t, func(t *testing.T) {
		mux := newDeviceRpcSendDrainTestMux(t, newBoundedDeviceRpcTestWs(nil), nil)
		mux.finishSend()
		for _, tag := range []uint8{deviceRpcStreamForward, deviceRpcStreamReverse} {
			beforeSendCalled := false
			n, err := mux.conns[tag].write([]byte("abc"), func([]byte) {
				beforeSendCalled = true
			})
			if n != 0 || err != io.ErrClosedPipe || beforeSendCalled {
				t.Errorf("stream %d admitted after final drain: n=%d err=%v copied=%t", tag, n, err, beforeSendCalled)
			}
		}
		assertDeviceRpcSendDrained(t, mux)
	})
}

func TestDeviceRpcMuxCloseDoesNotJoinHeldProducer(t *testing.T) {
	runDeviceRpcSendDrainSynctest(t, func(t *testing.T) {
		mux := newDeviceRpcSendDrainTestMux(t, newBoundedDeviceRpcTestWs(nil), nil)
		admitted := make(chan struct{})
		releaseProducer := make(chan struct{})
		result := make(chan deviceRpcSendDrainTestResult, 1)
		var observed []byte
		go func() {
			n, err := mux.conns[deviceRpcStreamReverse].write([]byte("abc"), func(frame []byte) {
				observed = connect.MessagePoolShareReadOnly(frame)
				close(admitted)
				<-releaseProducer
			})
			result <- deviceRpcSendDrainTestResult{n: n, err: err}
		}()
		<-admitted
		closed := make(chan struct{})
		go func() {
			defer close(closed)
			mux.conns[deviceRpcStreamForward].Close()
		}()
		synctest.Wait()
		select {
		case <-closed:
		default:
			t.Error("Close joined an admitted producer")
		}
		if mux.ctx.Err() == nil {
			t.Error("Close did not cancel the whole mux")
		}
		close(releaseProducer)
		synctest.Wait()
		<-closed
		got := <-result
		if !((got.n == 0 && got.err == io.ErrClosedPipe) || (got.n == 3 && got.err == nil)) {
			t.Errorf("write concurrent with Close: n=%d err=%v", got.n, got.err)
		}
		mux.finishSend()
		assertDeviceRpcSendDrained(t, mux)
		assertDeviceRpcObservedFrameReturned(t, observed)
	})
}

func TestDeviceRpcMuxSendByteBackpressureResumes(t *testing.T) {
	testDeviceRpcMuxSendBackpressureResumes(t, true)
}

func TestDeviceRpcMuxSendQueueBackpressureResumes(t *testing.T) {
	testDeviceRpcMuxSendBackpressureResumes(t, false)
}

func testDeviceRpcMuxSendBackpressureResumes(t *testing.T, byteBlocked bool) {
	runDeviceRpcSendDrainSynctest(t, func(t *testing.T) {
		settings := deviceRpcSendDrainTestSettings()
		if byteBlocked {
			settings.MuxMaxFrameBytes = 4
			settings.MuxMaxQueuedBytes = 4
			settings.MuxSendBufferSize = 2
		}
		frames := make(chan []byte, 2)
		ws := &deviceRpcSendDrainTestWs{
			boundedDeviceRpcTestWs: newBoundedDeviceRpcTestWs(nil),
			write: func(frame []byte) error {
				frames <- bytes.Clone(frame)
				return nil
			},
		}
		mux := newDeviceRpcSendDrainTestMux(t, ws, settings)
		var firstObserved, secondObserved []byte
		n, err := mux.conns[deviceRpcStreamForward].write([]byte("one"), func(frame []byte) {
			firstObserved = connect.MessagePoolShareReadOnly(frame)
		})
		if n != 3 || err != nil {
			t.Fatalf("first write: n=%d err=%v", n, err)
		}
		result := make(chan deviceRpcSendDrainTestResult, 1)
		go func() {
			n, err := mux.conns[deviceRpcStreamReverse].write([]byte("two"), func(frame []byte) {
				secondObserved = connect.MessagePoolShareReadOnly(frame)
			})
			result <- deviceRpcSendDrainTestResult{n: n, err: err}
		}()
		synctest.Wait()
		var got deviceRpcSendDrainTestResult
		returnedEarly := false
		select {
		case got = <-result:
			returnedEarly = true
			t.Errorf("blocked send returned before capacity: %+v", got)
		default:
		}
		wantBytes, wantWaiters := int64(8), 0
		if byteBlocked {
			wantBytes, wantWaiters = 4, 1
		}
		if mux.sendBytes.used != wantBytes || mux.sendBytes.waiters != wantWaiters || len(mux.send) != 1 {
			t.Errorf("backpressure state: bytes=%d waiters=%d queued=%d", mux.sendBytes.used, mux.sendBytes.waiters, len(mux.send))
		}

		finished := startDeviceRpcSendDrainTestWriter(mux)
		synctest.Wait()
		if !returnedEarly {
			got = <-result
		}
		if got.n != 3 || got.err != nil {
			t.Errorf("resumed write: n=%d err=%v", got.n, got.err)
		}
		for _, want := range [][]byte{{deviceRpcStreamForward, 'o', 'n', 'e'}, {deviceRpcStreamReverse, 't', 'w', 'o'}} {
			select {
			case frame := <-frames:
				if !bytes.Equal(frame, want) {
					t.Errorf("wire frame=%v want=%v", frame, want)
				}
			default:
				t.Errorf("missing wire frame %v after writer quiescence", want)
			}
		}
		mux.close()
		synctest.Wait()
		<-finished
		assertDeviceRpcSendDrained(t, mux)
		assertDeviceRpcObservedFrameReturned(t, firstObserved)
		assertDeviceRpcObservedFrameReturned(t, secondObserved)
	})
}

func TestDeviceRpcMuxFinalDrainReleasesBudgetBlockedProducer(t *testing.T) {
	testDeviceRpcMuxFinalDrainReleasesBlockedProducer(t, true)
}

func TestDeviceRpcMuxFinalDrainReleasesQueueBlockedProducer(t *testing.T) {
	testDeviceRpcMuxFinalDrainReleasesBlockedProducer(t, false)
}

func testDeviceRpcMuxFinalDrainReleasesBlockedProducer(t *testing.T, byteBlocked bool) {
	runDeviceRpcSendDrainSynctest(t, func(t *testing.T) {
		settings := deviceRpcSendDrainTestSettings()
		if byteBlocked {
			settings.MuxMaxFrameBytes = 4
			settings.MuxMaxQueuedBytes = 4
		}
		mux := newDeviceRpcSendDrainTestMux(t, newBoundedDeviceRpcTestWs(nil), settings)
		var firstObserved, secondObserved []byte
		n, err := mux.conns[deviceRpcStreamForward].write([]byte("one"), func(frame []byte) {
			firstObserved = connect.MessagePoolShareReadOnly(frame)
		})
		if n != 3 || err != nil {
			t.Fatalf("first write: n=%d err=%v", n, err)
		}
		result := make(chan deviceRpcSendDrainTestResult, 1)
		go func() {
			n, err := mux.conns[deviceRpcStreamReverse].write([]byte("two"), func(frame []byte) {
				secondObserved = connect.MessagePoolShareReadOnly(frame)
			})
			result <- deviceRpcSendDrainTestResult{n: n, err: err}
		}()
		synctest.Wait()
		var got deviceRpcSendDrainTestResult
		returnedEarly := false
		select {
		case got = <-result:
			returnedEarly = true
			t.Errorf("producer did not block before teardown: %+v", got)
		default:
		}
		if byteBlocked && mux.sendBytes.waiters != 1 {
			t.Errorf("budget waiters=%d want=1", mux.sendBytes.waiters)
		}
		if !byteBlocked && mux.sendBytes.used != 8 {
			t.Errorf("queue-blocked ownership=%d want=8", mux.sendBytes.used)
		}
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			mux.finishSend()
		}()
		synctest.Wait()
		<-finished
		if !returnedEarly {
			got = <-result
		}
		if got.n != 0 || got.err != io.ErrClosedPipe {
			t.Errorf("blocked producer cancellation: n=%d err=%v", got.n, got.err)
		}
		assertDeviceRpcSendDrained(t, mux)
		assertDeviceRpcObservedFrameReturned(t, firstObserved)
		if byteBlocked {
			if secondObserved != nil {
				t.Error("budget-blocked producer copied a frame after cancellation")
				assertDeviceRpcObservedFrameReturned(t, secondObserved)
			}
		} else {
			assertDeviceRpcObservedFrameReturned(t, secondObserved)
		}
	})
}

func TestDeviceRpcMuxWriteFailureDrainsForwardAndReverseOnce(t *testing.T) {
	runDeviceRpcSendDrainSynctest(t, func(t *testing.T) {
		settings := deviceRpcSendDrainTestSettings()
		settings.MuxSendBufferSize = 2
		var attempted [][]byte
		ws := &deviceRpcSendDrainTestWs{
			boundedDeviceRpcTestWs: newBoundedDeviceRpcTestWs(nil),
			write: func(frame []byte) error {
				attempted = append(attempted, bytes.Clone(frame))
				return io.ErrClosedPipe
			},
		}
		mux := newDeviceRpcSendDrainTestMux(t, ws, settings)
		observed := [][]byte{}
		for _, tag := range []uint8{deviceRpcStreamForward, deviceRpcStreamReverse} {
			n, err := mux.conns[tag].write([]byte("abc"), func(frame []byte) {
				observed = append(observed, connect.MessagePoolShareReadOnly(frame))
			})
			if n != 3 || err != nil {
				t.Fatalf("stream %d enqueue: n=%d err=%v", tag, n, err)
			}
		}
		mux.writeLoop()
		if len(attempted) != 1 || !bytes.Equal(attempted[0], []byte{deviceRpcStreamForward, 'a', 'b', 'c'}) {
			t.Errorf("write failure attempted frames=%v", attempted)
		}
		assertDeviceRpcSendDrained(t, mux)
		for _, frame := range observed {
			assertDeviceRpcObservedFrameReturned(t, frame)
		}
	})
}

func TestDeviceRpcMuxReceiveHealthyForwardAndReverse(t *testing.T) {
	runDeviceRpcSendDrainSynctest(t, func(t *testing.T) {
		forward := newDeviceRpcSendDrainObservedReader([]byte{deviceRpcStreamForward, 'o', 'n', 'e'})
		reverse := newDeviceRpcSendDrainObservedReader([]byte{deviceRpcStreamReverse, 't', 'w', 'o'})
		ws := newScriptedDeviceRpcTestWs([]io.Reader{forward, reverse})
		mux := newDeviceRpcSendDrainTestMux(t, ws, nil)
		finished := startDeviceRpcSendDrainTestReader(mux)
		synctest.Wait()
		for _, tag := range []uint8{deviceRpcStreamForward, deviceRpcStreamReverse} {
			data := make([]byte, 3)
			n, err := io.ReadFull(mux.conns[tag], data)
			want := "one"
			if tag == deviceRpcStreamReverse {
				want = "two"
			}
			if n != 3 || err != nil || string(data) != want {
				t.Errorf("stream %d read: n=%d err=%v data=%q", tag, n, err, data)
			}
		}
		if mux.receiveBytes.used != 0 {
			t.Errorf("fully consumed frames retained %d bytes", mux.receiveBytes.used)
		}
		assertDeviceRpcObservedFrameReturned(t, forward.observed)
		assertDeviceRpcObservedFrameReturned(t, reverse.observed)
		mux.close()
		synctest.Wait()
		<-finished
		mux.finishSend()
		assertDeviceRpcSendDrained(t, mux)
	})
}

func TestDeviceRpcMuxReceiveCloseReleasesPartialAndQueuedFrames(t *testing.T) {
	runDeviceRpcSendDrainSynctest(t, func(t *testing.T) {
		settings := deviceRpcSendDrainTestSettings()
		settings.MuxReceiveBufferSize = 2
		forward := newDeviceRpcSendDrainObservedReader([]byte{deviceRpcStreamForward, 'o', 'n', 'e'})
		reverse := newDeviceRpcSendDrainObservedReader([]byte{deviceRpcStreamReverse, 't', 'w', 'o'})
		reverseNext := newDeviceRpcSendDrainObservedReader([]byte{deviceRpcStreamReverse, 'e', 'n', 'd'})
		ws := newScriptedDeviceRpcTestWs([]io.Reader{forward, reverse, reverseNext})
		mux := newDeviceRpcSendDrainTestMux(t, ws, settings)
		finished := startDeviceRpcSendDrainTestReader(mux)
		synctest.Wait()
		firstByte := make([]byte, 1)
		n, err := mux.conns[deviceRpcStreamForward].Read(firstByte)
		if n != 1 || err != nil || firstByte[0] != 'o' || mux.receiveBytes.used != 12 {
			t.Errorf("partial read: n=%d err=%v data=%v bytes=%d", n, err, firstByte, mux.receiveBytes.used)
		}
		mux.close()
		synctest.Wait()
		<-finished
		if mux.receiveBytes.used != 0 {
			t.Errorf("receive teardown retained %d bytes", mux.receiveBytes.used)
		}
		for _, conn := range mux.conns {
			if conn.readMsg != nil || len(conn.receive) != 0 {
				t.Error("receive teardown retained a current or queued frame")
			}
		}
		for _, reader := range []*deviceRpcSendDrainObservedReader{forward, reverse, reverseNext} {
			assertDeviceRpcObservedFrameReturned(t, reader.observed)
		}
		mux.finishSend()
		assertDeviceRpcSendDrained(t, mux)
	})
}

type deviceRpcSendDrainTestResult struct {
	n   int
	err error
}

// Only mux fields participate; no DeviceLocal, client or SDK worker is created.
func deviceRpcSendDrainTestSettings() *deviceRpcSettings {
	return &deviceRpcSettings{
		MuxMaxFrameBytes:     16,
		MuxMaxQueuedBytes:    32,
		MuxSendBufferSize:    1,
		MuxReceiveBufferSize: 1,
		DeviceLocalSettings:  DeviceLocalSettings{DisableLogging: true},
	}
}

// Constructs only the mux state; each test explicitly owns the real loops it
// starts, so completion means their final drains returned, not merely Close.
func newDeviceRpcSendDrainTestMux(t *testing.T, ws deviceRpcWs, settings *deviceRpcSettings) *deviceRpcMux {
	t.Helper()
	if settings == nil {
		settings = deviceRpcSendDrainTestSettings()
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mux := &deviceRpcMux{
		ctx:           ctx,
		cancel:        cancel,
		log:           settings.logger(),
		ws:            ws,
		maxFrameBytes: settings.maxFrameBytes(),
		sendBytes:     newDeviceRpcByteBudget(max(settings.maxQueuedBytes(), settings.maxFrameBytes())),
		receiveBytes:  newDeviceRpcByteBudget(max(settings.maxQueuedBytes(), settings.maxFrameBytes())),
		send:          make(chan []byte, settings.MuxSendBufferSize),
	}
	for tag := range mux.conns {
		mux.conns[tag] = newDeviceRpcMuxConn(mux, uint8(tag), settings)
	}
	return mux
}

func startDeviceRpcSendDrainTestWriter(mux *deviceRpcMux) <-chan struct{} {
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		mux.writeLoop()
	}()
	return finished
}

func startDeviceRpcSendDrainTestReader(mux *deviceRpcMux) <-chan struct{} {
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		mux.readLoop()
	}()
	return finished
}

func assertDeviceRpcSendDrained(t *testing.T, mux *deviceRpcMux) {
	t.Helper()
	closed := deviceRpcSendDrainTestSendClosed(mux)
	if !closed || mux.sendBytes.used != 0 || mux.sendBytes.waiters != 0 || len(mux.send) != 0 {
		t.Errorf("final send state: closed=%t bytes=%d waiters=%d queued=%d", closed, mux.sendBytes.used, mux.sendBytes.waiters, len(mux.send))
	}
}

func deviceRpcSendDrainTestSendClosed(mux *deviceRpcMux) bool {
	mux.stateLock.Lock()
	defer mux.stateLock.Unlock()
	return mux.sendClosed
}

func assertDeviceRpcObservedFrameReturned(t *testing.T, frame []byte) {
	t.Helper()
	if frame == nil {
		t.Error("actual pooled frame was not observed")
		return
	}
	if !connect.MessagePoolReturn(frame) {
		t.Error("observer was not the final pool owner after production completion")
	}
}

type deviceRpcSendDrainTestWs struct {
	*boundedDeviceRpcTestWs
	write func([]byte) error
}

func (self *deviceRpcSendDrainTestWs) WriteMessage(_ int, frame []byte) error {
	return self.write(frame)
}

// The first read borrows the actual root allocated by MessagePoolReadAllLimit.
// The observer takes its own reference before readLoop receives that root.
type deviceRpcSendDrainObservedReader struct {
	reader   *bytes.Reader
	observed []byte
}

func newDeviceRpcSendDrainObservedReader(frame []byte) *deviceRpcSendDrainObservedReader {
	return &deviceRpcSendDrainObservedReader{reader: bytes.NewReader(frame)}
}

func (self *deviceRpcSendDrainObservedReader) Read(p []byte) (int, error) {
	n, err := self.reader.Read(p)
	if 0 < n && self.observed == nil {
		self.observed = connect.MessagePoolShareReadOnly(p[:n])
	}
	return n, err
}
