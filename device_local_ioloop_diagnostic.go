// Private diagnostic session binding: fixed per-loop scalars, no packet retention.
// The ordinary NewIoLoop constructor remains unchanged and unobserved.
//
//go:build !windows

package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

const ioLoopDiagnosticCapacity = 16

// One acceptance session owns all its normal tunnel replacements. Attachment
// and clock calls are concurrent-safe; the external owner fences construction
// before its single seal. The cold lock never guards packet observations.
type IoLoopDiagnosticSession struct {
	stateLock    sync.Mutex
	now          func() time.Time
	origin       time.Time
	slots        [ioLoopDiagnosticCapacity]ioLoopDiagnosticSlot
	count        int
	attemptCount uint64
	refusedCount uint64
	sealed       bool
	// Cold explicit requests only; ordinary packet observation never reads this.
	pacingSnapshotCount int
}

// A slot retains only scalar observation state and its two completion signals,
// never the IoLoop, DeviceLocal, descriptor, callback or packet roots.
type ioLoopDiagnosticSlot struct {
	observation        *ioLoopObservation
	startedNanoseconds int64
}

// Explicit private diagnostic opt-in; no default constructor calls this.
func NewIoLoopDiagnosticSession() *IoLoopDiagnosticSession {
	return newIoLoopDiagnosticSession(time.Now)
}

// The deterministic clock is confined to constructor/observation qualification.
func newIoLoopDiagnosticSession(now func() time.Time) *IoLoopDiagnosticSession {
	return &IoLoopDiagnosticSession{now: now, origin: now()}
}

// Same descriptor ownership as NewIoLoop. Excess or sealed attachments remain
// ordinary unobserved traffic and make this diagnostic inventory incomplete.
func (self *IoLoopDiagnosticSession) NewIoLoop(deviceLocal *DeviceLocal, fd int32, doneCallback IoLoopDoneCallback) *IoLoop {
	var observation *ioLoopObservation
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.attemptCount++
		if self.sealed || self.count == len(self.slots) {
			self.refusedCount++
			return
		}
		observation = newIoLoopObservation(self.now)
		observation.origin = self.origin
		self.slots[self.count] = ioLoopDiagnosticSlot{
			observation:        observation,
			startedNanoseconds: self.now().Sub(self.origin).Nanoseconds(),
		}
		self.count++
	}()
	return newIoLoopObserved(deviceLocal, fd, doneCallback, observation)
}

// One cold control-call timestamp for the existing owned request/reply channel.
// Elapsed nanoseconds share the exact origin used by every loop's maxima.
func (self *IoLoopDiagnosticSession) ClockAnchorJson() (string, error) {
	self.stateLock.Lock()
	sealed := self.sealed
	self.stateLock.Unlock()
	if sealed {
		return "", errors.New("native loop diagnostic is sealed")
	}
	now := self.now()
	value := struct {
		Schema                int   `json:"schema"`
		OriginUnixNanoseconds int64 `json:"origin_unix_ns,string"`
		ElapsedNanoseconds    int64 `json:"elapsed_ns,string"`
		WallUnixNanoseconds   int64 `json:"wall_unix_ns,string"`
	}{
		Schema:                1,
		OriginUnixNanoseconds: self.origin.UnixNano(),
		ElapsedNanoseconds:    now.Sub(self.origin).Nanoseconds(),
		WallUnixNanoseconds:   now.UnixNano(),
	}
	encoded, err := json.Marshal(value)
	return string(encoded), err
}

// Integer text preserves nanoseconds and counters beyond floating-point range.
type ioLoopDiagnosticSpan struct {
	Count                          uint64 `json:"count,string"`
	TotalNanoseconds               int64  `json:"total_ns,string"`
	MaximumNanoseconds             int64  `json:"max_ns,string"`
	MaximumStartElapsedNanoseconds int64  `json:"max_start_elapsed_ns,string"`
	MaximumEndElapsedNanoseconds   int64  `json:"max_end_elapsed_ns,string"`
}

// The zero-valued detailed counters are absent until both owners have settled.
type ioLoopDiagnosticRow struct {
	Ordinal            int                        `json:"ordinal"`
	StartedNanoseconds int64                      `json:"started_elapsed_ns,string"`
	RunJoined          bool                       `json:"run_joined"`
	FileClosed         bool                       `json:"fd_closed"`
	Joined             *ioLoopDiagnosticJoinedRow `json:"joined,omitempty"`
}

// Postjoin fields only; this is not a live/read-and-clear stats API.
type ioLoopDiagnosticJoinedRow struct {
	Read                 ioLoopDiagnosticSpan `json:"primary_read"`
	Send                 ioLoopDiagnosticSpan `json:"batch_send"`
	PrimaryReadByteCount uint64               `json:"primary_read_bytes,string"`
	ReadEofCount         uint64               `json:"read_eof_count,string"`
	ReadErrorCount       uint64               `json:"read_error_count,string"`
	OfferedPacketCount   uint64               `json:"offered_packets,string"`
	AcceptedPacketCount  uint64               `json:"accepted_packets,string"`
	RefusedPacketCount   uint64               `json:"refused_packets,string"`
	InvalidResultCount   uint64               `json:"invalid_result_count,string"`
	FileCloseError       bool                 `json:"fd_close_error"`
}

// Seal only after the external Android owner has stopped future construction
// and joined the device. The remaining existing device-join budget bounds one
// shared wait for every native-loop owner; this never cancels packet handling.
// A pending loop emits its completion flags only and invalidates coverage.
func (self *IoLoopDiagnosticSession) SealAndSnapshotJson(remainingJoinMillis int64) (string, error) {
	if remainingJoinMillis < 0 || 120_000 < remainingJoinMillis {
		return "", errors.New("invalid remaining native-loop join budget")
	}
	var rows [ioLoopDiagnosticCapacity]ioLoopDiagnosticRow
	snapshot := struct {
		Schema                int                   `json:"schema"`
		QualificationEligible bool                  `json:"qualification_eligible"`
		OriginUnixNanoseconds int64                 `json:"origin_unix_ns,string"`
		Capacity              int                   `json:"capacity"`
		Attempts              uint64                `json:"attempts,string"`
		Refused               uint64                `json:"refused,string"`
		Complete              bool                  `json:"complete"`
		Loops                 []ioLoopDiagnosticRow `json:"loops"`
	}{Schema: 1, OriginUnixNanoseconds: self.origin.UnixNano(), Capacity: ioLoopDiagnosticCapacity}
	self.stateLock.Lock()
	if self.sealed {
		self.stateLock.Unlock()
		return "", errors.New("native loop diagnostic already sealed")
	}
	self.sealed = true
	snapshot.Attempts, snapshot.Refused = self.attemptCount, self.refusedCount
	snapshot.Complete = self.count > 0 && self.refusedCount == 0
	count, slots := self.count, self.slots
	self.stateLock.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(remainingJoinMillis)*time.Millisecond)
	defer cancel()
	joined := func(done <-chan struct{}) bool {
		select {
		case <-done:
			return true
		default:
		}
		select {
		case <-done:
			return true
		case <-ctx.Done():
			return false
		}
	}
	for index, slot := range slots[:count] {
		row := &rows[index]
		row.Ordinal, row.StartedNanoseconds = index+1, slot.startedNanoseconds
		row.RunJoined = joined(slot.observation.runDone)
		row.FileClosed = joined(slot.observation.fileClosed)
		if !row.RunJoined || !row.FileClosed {
			snapshot.Complete = false
			continue
		}
		observation := slot.observation
		span := func(value ioLoopSpanObservation) ioLoopDiagnosticSpan {
			return ioLoopDiagnosticSpan{
				Count: value.Count, TotalNanoseconds: value.TotalNanoseconds,
				MaximumNanoseconds:             value.MaximumNanoseconds,
				MaximumStartElapsedNanoseconds: value.MaximumStartElapsedNanoseconds,
				MaximumEndElapsedNanoseconds:   value.MaximumEndElapsedNanoseconds,
			}
		}
		row.Joined = &ioLoopDiagnosticJoinedRow{
			Read: span(observation.read), Send: span(observation.send),
			PrimaryReadByteCount: observation.primaryReadByteCount,
			ReadEofCount:         observation.readEofCount, ReadErrorCount: observation.readErrorCount,
			OfferedPacketCount: observation.offeredPacketCount, AcceptedPacketCount: observation.acceptedPacketCount,
			RefusedPacketCount: observation.refusedPacketCount, InvalidResultCount: observation.invalidResultCount,
			FileCloseError: observation.fileCloseError,
		}
		if observation.invalidResultCount != 0 || observation.fileCloseError {
			snapshot.Complete = false
		}
	}
	snapshot.Loops = rows[:count]
	encoded, err := json.Marshal(snapshot)
	if len(encoded) > 32*1024 {
		return "", errors.New("native-loop diagnostic output exceeds bound")
	}
	return string(encoded), err
}
