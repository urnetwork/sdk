package sdk

// Test-owned process observation outlives device cancellation, never its finite
// deadline. Only this observer's goroutine samples or writes its append-once
// ring. Timeout exporters may read its atomically published immutable prefix.
import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
)

const (
	mobileMemoryTeardownLifetime = 150 * time.Second
	// One periodic read per existing interval, plus begin/cancel/join/terminal
	// and two spare failure-boundary reads. Overflow is still explicit failure.
	mobileMemoryTeardownCapacity = int((mobileMemoryTeardownLifetime+mobileMemorySampleInterval-1)/mobileMemorySampleInterval) + 6
)

// No device/controller/budget pointers or gomobile graphs are retained here.
type mobileMemoryTeardownSample struct {
	Sequence                 int64  `json:"sequence"`
	Stage                    string `json:"stage"`
	TimeUnixMillis           int64  `json:"timeUnixMs"`
	ElapsedNanos             int64  `json:"elapsedNanos"`
	GoRuntimeByteCount       int64  `json:"goRuntimeBytes"`
	GoMemoryLimitByteCount   int64  `json:"goMemoryLimitBytes"`
	GoMemoryProfileRateBytes int64  `json:"goMemoryProfileRateBytes"`
}

// The result becomes immutable before done closes. Repeated export is the same
// receipt, not another terminal observation or another independent sample set.
type mobileMemoryTeardownResult struct {
	Type                  string                       `json:"type"`
	SchemaVersion         int                          `json:"schemaVersion"`
	ObserverId            string                       `json:"observerId"`
	DeviceTargetByteCount int64                        `json:"deviceTargetBytes"`
	State                 string                       `json:"state"`
	Failure               string                       `json:"failure"`
	IntervalNanos         int64                        `json:"intervalNanos"`
	Capacity              int                          `json:"capacity"`
	Produced              int64                        `json:"produced"`
	Drained               int64                        `json:"drained"`
	Dropped               int64                        `json:"dropped"`
	CancelSequence        int64                        `json:"cancelSequence"`
	JoinSequence          int64                        `json:"joinSequence"`
	TerminalSequence      int64                        `json:"terminalSequence"`
	ObserverJoined        bool                         `json:"observerJoined"`
	Samples               []mobileMemoryTeardownSample `json:"samples"`
}

// Safe for concurrent exported calls. Cancellation and lifecycle channels do
// not retain their originating DeviceLocal; no operation closes that device.
// Blocking waits belong only to an external test/instrumentation owner.
type MemoryTeardownObservation struct {
	observerId      string
	targetByteCount int64
	deviceCancelled <-chan struct{}
	deviceJoined    <-chan struct{}
	ctx             context.Context
	cancel          context.CancelFunc
	requests        chan string
	ready           chan struct{}
	done            chan struct{}
	partial         atomic.Pointer[mobileMemoryTeardownResult]
	result          mobileMemoryTeardownResult
}

// Copies channel identities and target only. Normal asynchronous Close and the
// existing device-context sampler remain unchanged; neither joins this owner.
func (self *DeviceLocal) BeginMemoryTeardownObservation() (*MemoryTeardownObservation, error) {
	if self == nil || self.ctx == nil || self.lifecycleDone == nil || self.settings == nil {
		return nil, errors.New("device memory teardown owner unavailable")
	}
	reader := &mobileMemoryRuntimeReader{}
	return newMobileMemoryTeardownObservation(context.Background(), self.ctx.Done(), self.lifecycleDone,
		self.settings.MemoryTargetByteCount, func(snapshot *mobileMemoryRuntimeSnapshot) {
			reader.read(snapshot, nil)
		}), nil
}

// The callback captures only a process reader in production. Tests supply
// controlled readings; there is no production hook or new global policy.
func newMobileMemoryTeardownObservation(
	parent context.Context,
	deviceCancelled <-chan struct{},
	deviceJoined <-chan struct{},
	targetByteCount int64,
	read func(*mobileMemoryRuntimeSnapshot),
) *MemoryTeardownObservation {
	ctx, cancel := context.WithTimeout(parent, mobileMemoryTeardownLifetime)
	self := &MemoryTeardownObservation{
		observerId: connect.NewId().String(), targetByteCount: targetByteCount,
		deviceCancelled: deviceCancelled, deviceJoined: deviceJoined,
		ctx: ctx, cancel: cancel, requests: make(chan string),
		ready: make(chan struct{}), done: make(chan struct{}),
	}
	go self.run(read)
	<-self.ready
	return self
}

// Returns the immutable, process-local observation identity for finish binding.
func (self *MemoryTeardownObservation) GetObserverId() string {
	return self.observerId
}

// Only the actual lifecycle channel constitutes a device join. A timeout
// aborts observation permanently; a late join cannot turn that timeout green.
func (self *MemoryTeardownObservation) WaitForDeviceClose(timeoutMilliseconds int64) bool {
	select {
	case <-self.done:
		return self.result.State == "complete"
	default:
	}
	if timeoutMilliseconds <= 0 {
		self.cancel()
		return false
	}
	ctx, cancel := context.WithTimeout(self.ctx, time.Duration(min(timeoutMilliseconds, int64(mobileMemoryTeardownLifetime/time.Millisecond)))*time.Millisecond)
	defer cancel()
	select {
	case <-self.done:
		return self.result.State == "complete"
	case <-ctx.Done():
		self.cancel()
		return false
	case <-self.deviceJoined:
		if ctx.Err() != nil {
			self.cancel()
			return false
		}
		select {
		case <-self.done:
			return self.result.State == "complete"
		default:
			return true
		}
	}
}

// Requests a fresh terminal read after native join. Call only after the old
// drainer has joined, its ring was drained, and scoped device holders released.
func (self *MemoryTeardownObservation) FinishAndTakeJson(timeoutMilliseconds int64) string {
	return self.finishJson("finish", timeoutMilliseconds)
}

// Preserves partial evidence without qualifying teardown. Always use on an
// adapter failure; a later device join does not erase the failed observation.
func (self *MemoryTeardownObservation) AbortAndTakeJson(timeoutMilliseconds int64) string {
	self.cancel()
	return self.finishJson("abort", timeoutMilliseconds)
}

// Public callers send requests; only run reads runtime metrics or the ring.
// On a caller timeout no mutable partial ring is read concurrently.
func (self *MemoryTeardownObservation) finishJson(request string, timeoutMilliseconds int64) string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(max(int64(0), min(timeoutMilliseconds, int64(mobileMemoryTeardownLifetime/time.Millisecond))))*time.Millisecond)
	defer cancel()
	select {
	case <-self.done:
	case self.requests <- request:
	case <-ctx.Done():
		self.cancel()
		return self.unjoinedJson()
	}
	select {
	case <-self.done:
		encoded, err := json.Marshal(self.result)
		if err != nil {
			return self.unjoinedJson()
		}
		return string(encoded)
	case <-ctx.Done():
		self.cancel()
		return self.unjoinedJson()
	}
}

// The sole owner publishes only completed, never-rewritten ring prefixes.
// Timeout readers copy the immutable header, never the changing owner state.
func (self *MemoryTeardownObservation) unjoinedJson() string {
	result := mobileMemoryTeardownResult{Type: "device-memory-teardown", SchemaVersion: 1,
		ObserverId: self.observerId, DeviceTargetByteCount: self.targetByteCount,
		Samples: []mobileMemoryTeardownSample{}}
	if partial := self.partial.Load(); partial != nil {
		result = *partial
	}
	result.State, result.Failure, result.ObserverJoined = "failed", "observer-join-incomplete", false
	encoded, _ := json.Marshal(result)
	return string(encoded)
}

// The finite ring is allocated before the first runtime read; its real cost is
// in the observed process, never subtracted. Cadence is the native constant.
func (self *MemoryTeardownObservation) run(read func(*mobileMemoryRuntimeSnapshot)) {
	var samples [mobileMemoryTeardownCapacity]mobileMemoryTeardownSample
	result := mobileMemoryTeardownResult{Type: "device-memory-teardown", SchemaVersion: 1,
		ObserverId: self.observerId, DeviceTargetByteCount: self.targetByteCount,
		State: "failed", IntervalNanos: int64(mobileMemorySampleInterval), Capacity: len(samples)}
	start := time.Now()
	ready := false
	defer func() {
		if recover() != nil {
			result.State, result.Failure = "failed", "native-sample-failed"
		}
		if !ready {
			close(self.ready)
		}
		result.Drained = min(result.Produced, int64(len(samples)))
		result.Samples = samples[:result.Drained:result.Drained]
		result.ObserverJoined = true
		self.result = result
		self.cancel()
		close(self.done)
	}()
	publishPartial := func() {
		partial := result
		partial.State, partial.Failure, partial.ObserverJoined = "failed", "observer-join-incomplete", false
		partial.Drained = min(partial.Produced, int64(len(samples)))
		// Completed slots are assigned exactly once. Capping the slice length
		// and capacity excludes every future slot without another sample ring.
		partial.Samples = samples[:partial.Drained:partial.Drained]
		self.partial.Store(&partial)
	}
	record := func(stage string) int64 {
		var snapshot mobileMemoryRuntimeSnapshot
		read(&snapshot)
		now := time.Now()
		result.Produced++
		if result.Produced > int64(len(samples)) {
			result.Dropped++
			result.Failure = "native-sample-overflow"
			publishPartial()
			return result.Produced
		}
		samples[result.Produced-1] = mobileMemoryTeardownSample{
			Sequence: result.Produced, Stage: stage, TimeUnixMillis: now.UnixMilli(), ElapsedNanos: now.Sub(start).Nanoseconds(),
			GoRuntimeByteCount: snapshot.totalByteCount, GoMemoryLimitByteCount: snapshot.limitByteCount,
			GoMemoryProfileRateBytes: snapshot.memoryProfileRateByteCount,
		}
		switch stage {
		case "cancelled":
			result.CancelSequence = result.Produced
		case "joined":
			result.JoinSequence = result.Produced
		case "terminal":
			result.TerminalSequence = result.Produced
		}
		publishPartial()
		return result.Produced
	}
	closed := func(channel <-chan struct{}) bool {
		select {
		case <-channel:
			return true
		default:
			return false
		}
	}
	lateStart := closed(self.deviceCancelled) || closed(self.deviceJoined)
	record("begin")
	// The initial reader may overlap an external cancellation. A read that
	// only completed afterward cannot establish a pre-cancel boundary.
	lateStart = lateStart || closed(self.deviceCancelled) || closed(self.deviceJoined)
	close(self.ready)
	ready = true
	if lateStart {
		result.Failure = "observer-start-after-cancel"
		return
	}
	deviceCancelled, deviceJoined := self.deviceCancelled, self.deviceJoined
	observeLifecycle := func() {
		if result.CancelSequence == 0 && closed(self.deviceCancelled) {
			result.CancelSequence = record("cancelled")
			deviceCancelled = nil
		}
		if result.JoinSequence == 0 && closed(self.deviceJoined) {
			if result.CancelSequence == 0 {
				result.Failure = "device-join-without-cancel"
			}
			result.JoinSequence = record("joined")
			deviceJoined = nil
		}
	}
	ticker := time.NewTicker(mobileMemorySampleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-self.ctx.Done():
			observeLifecycle()
			record("aborted")
			result.Failure = "observer-aborted-or-expired"
			return
		case <-deviceCancelled:
			observeLifecycle()
		case <-deviceJoined:
			observeLifecycle()
		case <-ticker.C:
			observeLifecycle()
			record("periodic")
		case request := <-self.requests:
			observeLifecycle()
			if request != "finish" || self.ctx.Err() != nil {
				record("aborted")
				result.Failure = "observer-aborted-or-expired"
				return
			}
			if result.CancelSequence == 0 || result.JoinSequence == 0 {
				record("incomplete")
				result.Failure = "device-join-incomplete"
				return
			}
			result.TerminalSequence = record("terminal")
			if self.ctx.Err() != nil {
				result.Failure = "observer-aborted-or-expired"
			}
			if result.Failure == "" && result.Dropped == 0 {
				result.State = "complete"
			}
			return
		}
	}
}
