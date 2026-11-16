// Optional fixed-size native-loop observations, readable only after both owners join.
// No packet, route, address, or native descriptor is retained in the scalar result.

//go:build !windows

package sdk

import (
	"errors"
	"io"
	"time"
)

// A maximum is historical; its interval must overlap the owned workload before
// it can localize that workload. Shorter individual spans are not retained.
type ioLoopSpanObservation struct {
	Count                          uint64
	TotalNanoseconds               int64
	MaximumNanoseconds             int64
	MaximumStartUnixNanoseconds    int64
	MaximumEndUnixNanoseconds      int64
	MaximumStartElapsedNanoseconds int64
	MaximumEndElapsedNanoseconds   int64
}

// Records one completed call without allocations, payload retention, or a lock.
func (self *ioLoopSpanObservation) record(start, end, origin time.Time) {
	elapsed := end.Sub(start).Nanoseconds()
	self.Count++
	self.TotalNanoseconds += elapsed
	if self.MaximumNanoseconds < elapsed {
		self.MaximumNanoseconds = elapsed
		self.MaximumStartUnixNanoseconds = start.UnixNano()
		self.MaximumEndUnixNanoseconds = end.UnixNano()
		if !origin.IsZero() {
			self.MaximumStartElapsedNanoseconds = start.Sub(origin).Nanoseconds()
			self.MaximumEndElapsedNanoseconds = end.Sub(origin).Nanoseconds()
		}
	}
}

// The run worker alone writes read/send fields; the existing closer alone writes
// fileCloseError. Callers must receive both completion channels before reading.
// There is deliberately no live snapshot, polling worker, event ring, or logger.
type ioLoopObservation struct {
	now                  func() time.Time
	origin               time.Time
	runDone              chan struct{}
	fileClosed           chan struct{}
	read                 ioLoopSpanObservation
	send                 ioLoopSpanObservation
	primaryReadByteCount uint64
	readEofCount         uint64
	readErrorCount       uint64
	offeredPacketCount   uint64
	acceptedPacketCount  uint64
	refusedPacketCount   uint64
	invalidResultCount   uint64
	fileCloseError       bool
}

// Only an explicitly opted-in private caller allocates this fixed owner.
func newIoLoopObservation(now func() time.Time) *ioLoopObservation {
	if now == nil {
		now = time.Now
	}
	return &ioLoopObservation{
		now:        now,
		runDone:    make(chan struct{}),
		fileClosed: make(chan struct{}),
	}
}

// This measures the blocking os.File.Read call, not ready-drain syscalls, routing,
// a TCP syscall, or remote delivery. Cancellation errors stay distinct from EOF.
func (self *ioLoopObservation) recordRead(start time.Time, byteCount int, err error) {
	self.read.record(start, self.now(), self.origin)
	if byteCount > 0 {
		self.primaryReadByteCount += uint64(byteCount)
	}
	if errors.Is(err, io.EOF) {
		self.readEofCount++
	} else if err != nil {
		self.readErrorCount++
	}
}

// The batch API consumes every offered owner even when admission is partial.
// Invalid scalar results remain an explicit diagnostic error, never a success.
func (self *ioLoopObservation) recordSend(start time.Time, offered, accepted int) {
	self.send.record(start, self.now(), self.origin)
	self.offeredPacketCount += uint64(offered)
	if accepted < 0 || offered < accepted {
		self.invalidResultCount++
		return
	}
	self.acceptedPacketCount += uint64(accepted)
	self.refusedPacketCount += uint64(offered - accepted)
}
