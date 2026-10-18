// Bounded explicit requests cannot alter native-loop observation or ownership.
//go:build !windows

package sdk

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Disabled diagnostics do not inspect the device, allocate or read a clock.
func TestIoLoopPacingDiagnosticDisabledDoesNotAllocate(t *testing.T) {
	var session *IoLoopDiagnosticSession
	allocations := testing.AllocsPerRun(100, func() {
		if raw, err := session.PacingSnapshotJson(nil); raw != "" || err != nil {
			panic("disabled diagnostic ran")
		}
	})
	if allocations != 0 {
		t.Fatalf("disabled allocations=%g", allocations)
	}
}

// The cold session lock is not held while entering another owner. Clock
// brackets and count refusal are deterministic, with no timer or packet worker.
func TestIoLoopPacingDiagnosticOwnsThreeClockBoundedRequests(t *testing.T) {
	clock := time.Unix(100, 0)
	session := newIoLoopDiagnosticSession(func() time.Time { clock = clock.Add(time.Nanosecond); return clock })
	calls := 0
	capture := func(origin time.Time) connect.TransferPacingDiagnosticSnapshot {
		calls++
		if !session.stateLock.TryLock() {
			t.Fatal("session lock crossed snapshot boundary")
		}
		session.stateLock.Unlock()
		if origin != session.origin {
			t.Fatal("clock origin changed")
		}
		return connect.TransferPacingDiagnosticSnapshot{ClientCapacity: 16, SequenceCapacity: 16, Truncated: true}
	}
	for ordinal := 1; ordinal <= 3; ordinal++ {
		raw, err := session.pacingSnapshotJson(capture)
		if err != nil {
			t.Fatal(err)
		}
		var row ioLoopPacingDiagnosticSnapshot
		if err := json.Unmarshal([]byte(raw), &row); err != nil {
			t.Fatal(err)
		}
		if row.Ordinal != ordinal || row.Schema != 1 || row.QualificationEligible || !row.Snapshot.Truncated ||
			row.OriginUnixNanoseconds != session.origin.UnixNano() || row.StartElapsedNanoseconds >= row.EndElapsedNanoseconds {
			t.Fatalf("invalid bound envelope: %+v", row)
		}
	}
	if raw, err := session.pacingSnapshotJson(capture); raw != "" || err == nil || calls != 3 {
		t.Fatal("fourth request captured")
	}
	if session.count != 0 || session.attemptCount != 0 || session.refusedCount != 0 {
		t.Fatal("pacing request changed loop inventory")
	}
	sealed := newIoLoopDiagnosticSession(time.Now)
	sealed.sealed = true
	if _, err := sealed.pacingSnapshotJson(capture); err == nil || calls != 3 {
		t.Fatal("sealed request captured")
	}
}

// The JSON boundary fails closed even if a future source expands a scalar.
func TestIoLoopPacingDiagnosticRefusesOversizedSerializedEvidence(t *testing.T) {
	makeRow := func(padding int) ioLoopPacingDiagnosticSnapshot {
		var row ioLoopPacingDiagnosticSnapshot
		// Unmarshal synthetic reason text into the otherwise private row.
		raw := `{"snapshot":{"sequences":[{"estimate":{"reason":"` + strings.Repeat("x", padding) + `"}}]}}`
		if err := json.Unmarshal([]byte(raw), &row); err != nil {
			t.Fatal(err)
		}
		return row
	}
	base, err := encodeIoLoopPacingSnapshot(makeRow(0))
	if err != nil {
		t.Fatal(err)
	}
	for _, delta := range []int{-1, 0, 1} {
		result, err := encodeIoLoopPacingSnapshot(makeRow(ioLoopPacingDiagnosticByteLimit - len(base) + delta))
		if delta <= 0 {
			if err != nil || len(result) != ioLoopPacingDiagnosticByteLimit+delta {
				t.Fatalf("boundary=%d bytes=%d error=%v", delta, len(result), err)
			}
		} else if result != "" || err == nil {
			t.Fatal("oversized evidence escaped")
		}
	}
}

// Sealing cannot deadlock with an already-admitted cold read or retain its device.
func TestIoLoopPacingDiagnosticConcurrentSealReleasesOwnerLock(t *testing.T) {
	session := newIoLoopDiagnosticSession(time.Now)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var captureError error
	go func() {
		defer close(done)
		_, captureError = session.pacingSnapshotJson(func(time.Time) connect.TransferPacingDiagnosticSnapshot {
			close(entered)
			<-release
			return connect.TransferPacingDiagnosticSnapshot{}
		})
	}()
	<-entered
	_, sealError := session.SealAndSnapshotJson(0)
	close(release)
	<-done
	if sealError != nil || captureError != nil {
		t.Fatalf("seal=%v capture=%v", sealError, captureError)
	}
	if _, err := session.PacingSnapshotJson(&DeviceLocal{}); err == nil {
		t.Fatal("sealed owner admitted another capture")
	}
}
