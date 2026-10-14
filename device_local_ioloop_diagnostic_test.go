// Session wiring tests distinguish scalar custody from physical pause causation.
// Native cases retain the real read/send/close path and reconcile every root.
//go:build darwin || linux || android || ios

package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect"
)

// A real callback publishes completion without retaining any native packet.
type ioLoopDiagnosticDone func()

func (self ioLoopDiagnosticDone) IoLoopDone() { self() }

// Fixed native owners are kept by the test only, never by the diagnostic session.
type ioLoopDiagnosticNativeOwner struct {
	loop         *IoLoop
	peer         io.ReadWriteCloser
	callbackDone <-chan struct{}
}
type ioLoopDiagnosticNativeFixture struct {
	device     *DeviceLocal
	route      *ioLoopWitnessRoute
	cancel     context.CancelFunc
	owners     []ioLoopDiagnosticNativeOwner
	closeOnce  sync.Once
	poolCount  uint64
	poolBytes  connect.ByteCount
	beforeDone func()
}

// Reuse the qualified actual route and message-pool contract. Stream EOF is used
// only to join an unobserved closer; those cases offer exactly one synthetic byte.
func newIoLoopDiagnosticNativeFixture(t *testing.T) *ioLoopDiagnosticNativeFixture {
	t.Helper()
	connect.MessagePoolReturn(connect.MessagePoolGet(60))
	previous := connect.SetMessagePoolViolationHandler(func(err error, stack []byte) {
		t.Errorf("diagnostic native pool violation: %v\n%s", err, stack)
	})
	t.Cleanup(func() { connect.SetMessagePoolViolationHandler(previous) })
	ctx, cancel := context.WithCancel(context.Background())
	route := &ioLoopWitnessRoute{ctx: ctx, entered: make(chan ioLoopWitnessBatch, 2), responses: make(chan int)}
	device := newPacketBatchTestDevice(route)
	device.ctx, device.cancel, device.log = ctx, cancel, connect.NewNoopLogger()
	fixture := &ioLoopDiagnosticNativeFixture{device: device, route: route, cancel: cancel,
		poolCount: connect.MessagePoolOutstandingCount(), poolBytes: connect.MessagePoolOutstandingByteCount()}
	t.Cleanup(func() {
		fixture.close(t)
		if got := connect.MessagePoolOutstandingCount(); got != fixture.poolCount {
			t.Errorf("roots=%d want=%d", got, fixture.poolCount)
		}
		if got := connect.MessagePoolOutstandingByteCount(); got != fixture.poolBytes {
			t.Errorf("root bytes=%d want=%d", got, fixture.poolBytes)
		}
	})
	return fixture
}

// Each descriptor is detached exactly once; native run owns it after construction.
func (self *ioLoopDiagnosticNativeFixture) add(t *testing.T, session *IoLoopDiagnosticSession, observed bool, packet byte) *IoLoop {
	t.Helper()
	var fd int32
	var peer io.ReadWriteCloser
	if observed {
		fd, peer = testingIoLoopSocketPair(t)
	} else {
		pair, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
		if err != nil {
			t.Fatal(err)
		}
		fd, peer = int32(pair[0]), os.NewFile(uintptr(pair[1]), "synthetic-unobserved-peer")
	}
	callbackDone := make(chan struct{})
	beforeDone := self.beforeDone
	done := ioLoopDiagnosticDone(func() {
		if beforeDone != nil {
			beforeDone()
		}
		close(callbackDone)
	})
	var loop *IoLoop
	if session == nil {
		loop = NewIoLoop(self.device, fd, done)
	} else {
		loop = session.NewIoLoop(self.device, fd, done)
	}
	self.owners = append(self.owners, ioLoopDiagnosticNativeOwner{loop: loop, peer: peer, callbackDone: callbackDone})
	if (loop.observation != nil) != observed {
		t.Fatal("actual constructor selected wrong observation branch")
	}
	if n, err := peer.Write([]byte{packet}); err != nil || n != 1 {
		t.Fatalf("synthetic packet write n=%d err=%v", n, err)
	}
	call := <-self.route.entered
	if len(call.packets) != 1 || !bytes.Equal(call.packets[0], []byte{packet}) || call.rootByteCount != 256 || call.timeout != 5*time.Second {
		t.Fatalf("actual send changed payload/ownership/budget: %+v", call)
	}
	self.route.responses <- 1
	return loop
}

// Observed loops expose both existing completion channels. For an off-path
// stream, peer EOF proves the real fd closer ran; the callback proves unsubscribe.
func (self *ioLoopDiagnosticNativeFixture) close(t *testing.T) {
	t.Helper()
	self.closeOnce.Do(func() {
		self.cancel()
		for _, owner := range self.owners {
			owner.loop.Close()
		}
		for _, owner := range self.owners {
			<-owner.callbackDone
			if owner.loop.observation != nil {
				<-owner.loop.observation.runDone
				<-owner.loop.observation.fileClosed
			} else {
				var b [1]byte
				n, err := owner.peer.Read(b[:])
				if n != 0 || err != io.EOF {
					t.Errorf("unobserved native closer n=%d err=%v", n, err)
				}
			}
			owner.peer.Close()
		}
	})
}

// Decode the actual public JSON with exact integer fields, never float64.
type ioLoopDiagnosticSnapshotForTest struct {
	Schema                int                   `json:"schema"`
	QualificationEligible bool                  `json:"qualification_eligible"`
	Origin                int64                 `json:"origin_unix_ns,string"`
	Capacity              int                   `json:"capacity"`
	Attempts              uint64                `json:"attempts,string"`
	Refused               uint64                `json:"refused,string"`
	Complete              bool                  `json:"complete"`
	Loops                 []ioLoopDiagnosticRow `json:"loops"`
}

func ioLoopDiagnosticTakeForTest(t *testing.T, session *IoLoopDiagnosticSession, remaining int64) (ioLoopDiagnosticSnapshotForTest, string) {
	t.Helper()
	raw, err := session.SealAndSnapshotJson(remaining)
	if err != nil {
		t.Fatal(err)
	}
	var result ioLoopDiagnosticSnapshotForTest
	if err = json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	if result.Schema != 1 || result.QualificationEligible || result.Capacity != 16 || len(raw) > 32*1024 {
		t.Fatal("diagnostic authority or bound changed")
	}
	return result, raw
}

// This scalar-only seam exercises actual seal/join publication without native
// I/O inside a synctest bubble. Real fd ownership is checked in the other cases.
func ioLoopDiagnosticScalarSlotForTest(session *IoLoopDiagnosticSession) *ioLoopObservation {
	observation := newIoLoopObservation(session.now)
	observation.origin = session.origin
	session.slots[session.count] = ioLoopDiagnosticSlot{observation: observation, startedNanoseconds: 0}
	session.count++
	session.attemptCount++
	return observation
}

func TestIoLoopDiagnosticDefaultConstructorRemainsOff(t *testing.T) {
	fixture := newIoLoopDiagnosticNativeFixture(t)
	loop := fixture.add(t, nil, false, 0x41)
	if loop.observation != nil {
		t.Fatal("public constructor enabled timing")
	}
	fixture.close(t)
}

func TestIoLoopDiagnosticSessionRetainsReplacementLoops(t *testing.T) {
	fixture := newIoLoopDiagnosticNativeFixture(t)
	session := NewIoLoopDiagnosticSession()
	first := fixture.add(t, session, true, 0x41)
	second := fixture.add(t, session, true, 0x52)
	second.Close()
	<-second.observation.runDone
	<-second.observation.fileClosed
	first.Close()
	<-first.observation.runDone
	<-first.observation.fileClosed
	snapshot, _ := ioLoopDiagnosticTakeForTest(t, session, 0)
	if !snapshot.Complete || snapshot.Attempts != 2 || snapshot.Refused != 0 || len(snapshot.Loops) != 2 {
		t.Fatalf("replacement inventory: %+v", snapshot)
	}
	for index, row := range snapshot.Loops {
		if row.Ordinal != index+1 || !row.RunJoined || !row.FileClosed || row.Joined == nil || row.Joined.Send.Count != 1 ||
			row.Joined.OfferedPacketCount != 1 || row.Joined.AcceptedPacketCount != 1 || row.Joined.RefusedPacketCount != 0 {
			t.Fatalf("replacement row lost its independent owner: %+v", row)
		}
	}
	fixture.close(t)
}

func TestIoLoopDiagnosticOverflowKeepsTrafficUnobserved(t *testing.T) {
	fixture := newIoLoopDiagnosticNativeFixture(t)
	session := NewIoLoopDiagnosticSession()
	for index := 0; index < ioLoopDiagnosticCapacity; index++ {
		fixture.add(t, session, true, byte(index+1))
	}
	excess := fixture.add(t, session, false, 0x71)
	if excess.observation != nil {
		t.Fatal("excess observer silently overwrote a slot")
	}
	fixture.close(t)
	snapshot, _ := ioLoopDiagnosticTakeForTest(t, session, 0)
	if snapshot.Complete || snapshot.Attempts != 17 || snapshot.Refused != 1 || len(snapshot.Loops) != 16 {
		t.Fatalf("overflow authority: %+v", snapshot)
	}
	for index, row := range snapshot.Loops {
		if row.Ordinal != index+1 || row.Joined == nil || row.Joined.AcceptedPacketCount != 1 {
			t.Fatalf("retained slot changed: %+v", row)
		}
	}
}

func TestIoLoopDiagnosticSealJoinsRunAndFd(t *testing.T) {
	fixture := newIoLoopDiagnosticNativeFixture(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseCallback := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseCallback()
	fixture.beforeDone = func() { close(entered); <-release }
	nativeSession := NewIoLoopDiagnosticSession()
	loop := fixture.add(t, nativeSession, true, 0x41)
	loop.Close()
	<-entered
	<-loop.observation.fileClosed
	pending, _ := ioLoopDiagnosticTakeForTest(t, nativeSession, 0)
	if pending.Complete || pending.Loops[0].RunJoined || !pending.Loops[0].FileClosed || pending.Loops[0].Joined != nil {
		t.Fatal("actual fd close published counters before the held done callback returned")
	}
	releaseCallback()
	<-loop.observation.runDone
	fixture.close(t)
	synctest.Test(t, func(t *testing.T) {
		for _, closeRunFirst := range []bool{true, false} {
			session := NewIoLoopDiagnosticSession()
			observation := ioLoopDiagnosticScalarSlotForTest(session)
			returned := make(chan string, 1)
			failure := make(chan error, 1)
			go func() { raw, err := session.SealAndSnapshotJson(1000); returned <- raw; failure <- err }()
			synctest.Wait()
			if closeRunFirst {
				close(observation.runDone)
			} else {
				close(observation.fileClosed)
			}
			synctest.Wait()
			select {
			case <-returned:
				t.Fatal("one owner published the other's unfinished counters")
			default:
			}
			// Writes happen before the final completion channel; seal may read only after it.
			if closeRunFirst {
				observation.fileCloseError = false
				close(observation.fileClosed)
			} else {
				observation.offeredPacketCount = 3
				observation.acceptedPacketCount = 2
				observation.refusedPacketCount = 1
				close(observation.runDone)
			}
			raw := <-returned
			if err := <-failure; err != nil {
				t.Fatal(err)
			}
			var snapshot ioLoopDiagnosticSnapshotForTest
			if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
				t.Fatal(err)
			}
			if !snapshot.Complete || len(snapshot.Loops) != 1 || snapshot.Loops[0].Joined == nil {
				t.Fatal("joined publication missing")
			}
			if !closeRunFirst && snapshot.Loops[0].Joined.RefusedPacketCount != 1 {
				t.Fatal("published before final producer writes")
			}
		}
	})
}

func TestIoLoopDiagnosticSharedDeadlineRetainsPendingUnknown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		session := NewIoLoopDiagnosticSession()
		first := ioLoopDiagnosticScalarSlotForTest(session)
		second := ioLoopDiagnosticScalarSlotForTest(session)
		close(first.runDone)
		close(second.fileClosed)
		started := time.Now()
		snapshot, _ := ioLoopDiagnosticTakeForTest(t, session, 10)
		if time.Since(started) != 10*time.Millisecond || snapshot.Complete || len(snapshot.Loops) != 2 {
			t.Fatal("join deadline renewed or incomplete inventory accepted")
		}
		for _, row := range snapshot.Loops {
			if row.Joined != nil {
				t.Fatal("pending owner invented zero-valued settled counters")
			}
		}
		if !snapshot.Loops[0].RunJoined || snapshot.Loops[0].FileClosed || snapshot.Loops[1].RunJoined || !snapshot.Loops[1].FileClosed {
			t.Fatal("completion identity changed")
		}
		close(first.fileClosed)
		close(second.runDone)
	})
}

func TestIoLoopDiagnosticClosedSessionRefusesObservation(t *testing.T) {
	fixture := newIoLoopDiagnosticNativeFixture(t)
	session := NewIoLoopDiagnosticSession()
	first := fixture.add(t, session, true, 0x41)
	first.Close()
	<-first.observation.runDone
	<-first.observation.fileClosed
	snapshot, _ := ioLoopDiagnosticTakeForTest(t, session, 0)
	if !snapshot.Complete {
		t.Fatal("pre-fence loop did not join")
	}
	late := fixture.add(t, session, false, 0x52)
	if late.observation != nil || session.attemptCount != 2 || session.refusedCount != 1 || session.count != 1 {
		t.Fatal("sealed observer admitted a late loop")
	}
	if _, err := session.ClockAnchorJson(); err == nil {
		t.Fatal("sealed anchor accepted")
	}
	if _, err := session.SealAndSnapshotJson(0); err == nil {
		t.Fatal("second seal accepted")
	}
	// No assertion claims that the already emitted snapshot attests this late loop.
	fixture.close(t)
}

func TestIoLoopDiagnosticMonotonicAnchorPreservesIntegerSpans(t *testing.T) {
	var elapsed atomic.Int64
	origin := time.Unix(100, 0)
	session := newIoLoopDiagnosticSession(func() time.Time { return origin.Add(time.Duration(elapsed.Load())) })
	observation := ioLoopDiagnosticScalarSlotForTest(session)
	start := origin.Add(3 * time.Second)
	elapsed.Store(int64(8 * time.Second))
	observation.recordSend(start, 3, 2)
	observation.offeredPacketCount = 9007199254740993
	observation.acceptedPacketCount = 9007199254740992
	observation.refusedPacketCount = 1
	raw, err := session.ClockAnchorJson()
	if err != nil {
		t.Fatal(err)
	}
	var anchor struct {
		Origin  int64 `json:"origin_unix_ns,string"`
		Elapsed int64 `json:"elapsed_ns,string"`
		Wall    int64 `json:"wall_unix_ns,string"`
	}
	if err = json.Unmarshal([]byte(raw), &anchor); err != nil || anchor.Origin != origin.UnixNano() || anchor.Elapsed != int64(8*time.Second) || anchor.Wall != origin.Add(8*time.Second).UnixNano() {
		t.Fatalf("anchor: %s err=%v", raw, err)
	}
	for _, budget := range []int64{-1, 120001} {
		if _, err = session.SealAndSnapshotJson(budget); err == nil {
			t.Fatal("invalid budget accepted")
		}
	}
	close(observation.runDone)
	close(observation.fileClosed)
	snapshot, raw := ioLoopDiagnosticTakeForTest(t, session, 0)
	row := snapshot.Loops[0].Joined
	if !snapshot.Complete || row.OfferedPacketCount != 9007199254740993 || row.AcceptedPacketCount != 9007199254740992 || row.RefusedPacketCount != 1 ||
		row.Send.MaximumNanoseconds != int64(5*time.Second) || row.Send.MaximumStartElapsedNanoseconds != int64(3*time.Second) || row.Send.MaximumEndElapsedNanoseconds != int64(8*time.Second) ||
		!bytes.Contains([]byte(raw), []byte(`"offered_packets":"9007199254740993"`)) {
		t.Fatalf("integer/interval custody lost: %s", raw)
	}
}

func TestIoLoopDiagnosticPartialFailureDoesNotInventSuccess(t *testing.T) {
	for _, testCase := range []struct {
		accepted     int
		closeError   bool
		wantComplete bool
	}{
		{accepted: 0, wantComplete: true}, {accepted: 1, wantComplete: true}, {accepted: 3, wantComplete: true},
		{accepted: -1, wantComplete: false}, {accepted: 4, wantComplete: false}, {accepted: 1, closeError: true, wantComplete: false},
	} {
		session := NewIoLoopDiagnosticSession()
		observation := ioLoopDiagnosticScalarSlotForTest(session)
		observation.recordSend(session.origin, 3, testCase.accepted)
		observation.fileCloseError = testCase.closeError
		close(observation.runDone)
		close(observation.fileClosed)
		snapshot, _ := ioLoopDiagnosticTakeForTest(t, session, 0)
		row := snapshot.Loops[0].Joined
		if snapshot.Complete != testCase.wantComplete || row == nil || row.OfferedPacketCount != 3 || row.FileCloseError != testCase.closeError {
			t.Fatalf("partial/error custody: %+v", snapshot)
		}
		if 0 <= testCase.accepted && testCase.accepted <= 3 {
			if row.AcceptedPacketCount != uint64(testCase.accepted) || row.RefusedPacketCount != uint64(3-testCase.accepted) || row.InvalidResultCount != 0 {
				t.Fatalf("valid partial disposition: %+v", row)
			}
		} else if row.InvalidResultCount != 1 || row.AcceptedPacketCount != 0 || row.RefusedPacketCount != 0 {
			t.Fatalf("invalid count invented admission: %+v", row)
		}
	}
	// Actual native partial ownership/error/EOF remain covered by the unchanged
	// qualified six-test backing, including 0/1/3 results and exact pooled roots.
}
