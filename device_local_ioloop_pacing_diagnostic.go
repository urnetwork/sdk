// Cold pacing observations share the explicit native-loop diagnostic owner.
//go:build !windows

package sdk

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/urnetwork/connect"
)

const ioLoopPacingDiagnosticCapacity = 3
const ioLoopPacingDiagnosticByteLimit = 64 * 1024

// This envelope binds independently bracketed components to the loop clock.
// The Android command receipt supplies session, build, process and command ids.
type ioLoopPacingDiagnosticSnapshot struct {
	Schema                  int                                      `json:"schema"`
	QualificationEligible   bool                                     `json:"qualification_eligible"`
	Ordinal                 int                                      `json:"ordinal"`
	OriginUnixNanoseconds   int64                                    `json:"origin_unix_ns,string"`
	StartElapsedNanoseconds int64                                    `json:"start_elapsed_ns,string"`
	EndElapsedNanoseconds   int64                                    `json:"end_elapsed_ns,string"`
	Snapshot                connect.TransferPacingDiagnosticSnapshot `json:"snapshot"`
}

// Explicitly requested only alongside the existing three stack commands. A nil
// diagnostic is an allocation-free no-op; no device pointer is retained. Owner
// and device locks are released before reading any client/controller state.
func (self *IoLoopDiagnosticSession) PacingSnapshotJson(deviceLocal *DeviceLocal) (string, error) {
	if self == nil {
		return "", nil
	}
	if deviceLocal == nil {
		return "", errors.New("pacing diagnostic requires the owned device")
	}
	return self.pacingSnapshotJson(func(origin time.Time) connect.TransferPacingDiagnosticSnapshot {
		deviceLocal.stateLock.Lock()
		multi, _ := deviceLocal.remoteUserNatClient.(*connect.RemoteUserNatMultiClient)
		deviceLocal.stateLock.Unlock()
		return multi.PacingDiagnosticSnapshot(origin)
	})
}

// The capture seam permits deterministic lock/cap/clock tests, never packet work.
// An admitted synchronous call may finish after sealing; it owns no lifecycle.
func (self *IoLoopDiagnosticSession) pacingSnapshotJson(capture func(time.Time) connect.TransferPacingDiagnosticSnapshot) (string, error) {
	self.stateLock.Lock()
	if self.sealed || self.pacingSnapshotCount >= ioLoopPacingDiagnosticCapacity {
		self.stateLock.Unlock()
		return "", errors.New("pacing diagnostic is sealed or exhausted")
	}
	self.pacingSnapshotCount++
	ordinal := self.pacingSnapshotCount
	self.stateLock.Unlock()
	row := ioLoopPacingDiagnosticSnapshot{
		Schema: 1, Ordinal: ordinal, OriginUnixNanoseconds: self.origin.UnixNano(),
		StartElapsedNanoseconds: self.now().Sub(self.origin).Nanoseconds(),
	}
	row.Snapshot = capture(self.origin)
	row.EndElapsedNanoseconds = self.now().Sub(self.origin).Nanoseconds()
	return encodeIoLoopPacingSnapshot(row)
}

// Refuse oversized serialized evidence rather than emitting a partial JSON row.
// Inventory truncation is separately explicit in the native bounded snapshot.
func encodeIoLoopPacingSnapshot(row ioLoopPacingDiagnosticSnapshot) (string, error) {
	encoded, err := json.Marshal(row)
	if err != nil {
		return "", err
	}
	if len(encoded) > ioLoopPacingDiagnosticByteLimit {
		return "", errors.New("pacing diagnostic output exceeds bound")
	}
	return string(encoded), nil
}
