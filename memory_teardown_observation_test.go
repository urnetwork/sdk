package sdk

// Deterministic native lifecycle tests use synthetic runtime readings and
// virtual time; none of those readings claim a measured physical overshoot.
import (
	"encoding/json"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// The same result decoder is used after the observer's explicit join.
func memoryTeardownResultForTest(t *testing.T, encoded string) mobileMemoryTeardownResult {
	t.Helper()
	var result mobileMemoryTeardownResult
	if err := json.Unmarshal([]byte(encoded), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

// Cancellation precedes a withheld join; the observer must retain the high
// intermediate reading even when both the joined and terminal readings fall.
func TestMobileMemoryTeardownRecordsWithheldCloseOvershoot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cancelled, joined := make(chan struct{}), make(chan struct{})
		var runtimeBytes atomic.Int64
		runtimeBytes.Store(20 * 1024 * 1024)
		observer := newMobileMemoryTeardownObservation(t.Context(), cancelled, joined, 32*1024*1024,
			func(snapshot *mobileMemoryRuntimeSnapshot) {
				*snapshot = mobileMemoryRuntimeSnapshot{totalByteCount: runtimeBytes.Load(), limitByteCount: 32 * 1024 * 1024}
			})
		runtimeBytes.Store(32*1024*1024 + 1)
		close(cancelled)
		synctest.Wait()
		select {
		case <-observer.done:
			t.Fatal("device cancellation stopped process observation")
		default:
		}
		time.Sleep(mobileMemorySampleInterval)
		synctest.Wait()
		runtimeBytes.Store(19 * 1024 * 1024)
		close(joined)
		if !observer.WaitForDeviceClose(1000) {
			t.Fatal("released native join was not observed")
		}
		result := memoryTeardownResultForTest(t, observer.FinishAndTakeJson(1000))
		breaches, periodic := 0, 0
		for _, sample := range result.Samples {
			if sample.GoRuntimeByteCount > 32*1024*1024 {
				breaches++
			}
			if sample.Stage == "periodic" {
				periodic++
			}
		}
		if result.State != "complete" || !result.ObserverJoined || result.CancelSequence >= result.JoinSequence ||
			result.JoinSequence >= result.TerminalSequence || breaches != 2 || periodic != 1 || result.Dropped != 0 {
			t.Fatalf("post-cancel samples/join/terminal lost: %+v", result)
		}
	})
}

// Closing before the first periodic tick still forces begin/cancel/join and a
// fourth, new terminal runtime read, even at the same wall-clock millisecond.
func TestMobileMemoryTeardownForcesTerminalBeforeFirstPeriodicTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cancelled, joined := make(chan struct{}), make(chan struct{})
		var reads atomic.Int64
		observer := newMobileMemoryTeardownObservation(t.Context(), cancelled, joined, 32*1024*1024,
			func(snapshot *mobileMemoryRuntimeSnapshot) {
				*snapshot = mobileMemoryRuntimeSnapshot{totalByteCount: reads.Add(1), limitByteCount: 32 * 1024 * 1024}
			})
		close(cancelled)
		close(joined)
		result := memoryTeardownResultForTest(t, observer.FinishAndTakeJson(1000))
		if result.State != "complete" || result.Produced != 4 || result.Drained != 4 || result.TerminalSequence != 4 ||
			result.Samples[3].GoRuntimeByteCount != 4 || result.Samples[0].TimeUnixMillis != result.Samples[3].TimeUnixMillis {
			t.Fatalf("terminal reused an earlier reading: %+v", result)
		}
	})
}

// An already-cancelled owner has no observed pre-cancellation boundary.
func TestMobileMemoryTeardownRejectsAlreadyCancelledOwner(t *testing.T) {
	cancelled, joined := make(chan struct{}), make(chan struct{})
	close(cancelled)
	observer := newMobileMemoryTeardownObservation(t.Context(), cancelled, joined, 32*1024*1024,
		func(snapshot *mobileMemoryRuntimeSnapshot) { snapshot.totalByteCount = 1 })
	result := memoryTeardownResultForTest(t, observer.FinishAndTakeJson(1000))
	if result.State != "failed" || result.Failure != "observer-start-after-cancel" || result.TerminalSequence != 0 || len(result.Samples) != 1 {
		t.Fatalf("late start qualified teardown: %+v", result)
	}
}

// A cancellation while the initial read is withheld cannot masquerade as an
// observed pre-cancellation boundary just because Begin was called first.
func TestMobileMemoryTeardownRejectsCancellationBeforeInitialSample(t *testing.T) {
	cancelled, joined := make(chan struct{}), make(chan struct{})
	reading, release := make(chan struct{}), make(chan struct{})
	constructed := make(chan *MemoryTeardownObservation, 1)
	go func() {
		constructed <- newMobileMemoryTeardownObservation(t.Context(), cancelled, joined, 32*1024*1024,
			func(snapshot *mobileMemoryRuntimeSnapshot) {
				close(reading)
				<-release
				snapshot.totalByteCount = 123
			})
	}()
	<-reading
	close(cancelled)
	close(release)
	observer := <-constructed
	result := memoryTeardownResultForTest(t, observer.FinishAndTakeJson(1000))
	if result.State != "failed" || result.Failure != "observer-start-after-cancel" || result.TerminalSequence != 0 {
		t.Fatalf("cancel-before-read completion was accepted: %+v", result)
	}
}

// Copied admission target and observed runtime policy are independent values.
// A wrong live limit/rate must remain visible to the qualification consumer.
func TestMobileMemoryTeardownPreservesCopiedTargetAndLiveRuntimePolicy(t *testing.T) {
	cancelled, joined := make(chan struct{}), make(chan struct{})
	observer := newMobileMemoryTeardownObservation(t.Context(), cancelled, joined, 32*1024*1024,
		func(snapshot *mobileMemoryRuntimeSnapshot) {
			*snapshot = mobileMemoryRuntimeSnapshot{totalByteCount: 123, limitByteCount: 64 * 1024 * 1024, memoryProfileRateByteCount: 65536}
		})
	close(cancelled)
	close(joined)
	result := memoryTeardownResultForTest(t, observer.FinishAndTakeJson(1000))
	if result.DeviceTargetByteCount != 32*1024*1024 {
		t.Fatal("copied admission target changed")
	}
	for _, sample := range result.Samples {
		if sample.GoMemoryLimitByteCount != 64*1024*1024 || sample.GoMemoryProfileRateBytes != 65536 {
			t.Fatal("observer relabeled the live process policy")
		}
	}
}

// A finish request cannot manufacture the actual device lifecycle join.
func TestMobileMemoryTeardownCannotFinishBeforeLifecycleJoin(t *testing.T) {
	cancelled, joined := make(chan struct{}), make(chan struct{})
	observer := newMobileMemoryTeardownObservation(t.Context(), cancelled, joined, 32*1024*1024,
		func(snapshot *mobileMemoryRuntimeSnapshot) { snapshot.totalByteCount = 1 })
	close(cancelled)
	result := memoryTeardownResultForTest(t, observer.FinishAndTakeJson(1000))
	if result.State != "failed" || result.Failure != "device-join-incomplete" || result.JoinSequence != 0 || result.TerminalSequence != 0 {
		t.Fatalf("cancel-only owner passed native join: %+v", result)
	}
}

// Virtual timeout is an explicit state transition, not a short negative wait.
func TestMobileMemoryTeardownRejectsExpiredJoinEvenIfDeviceLaterCloses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cancelled, joined := make(chan struct{}), make(chan struct{})
		observer := newMobileMemoryTeardownObservation(t.Context(), cancelled, joined, 32*1024*1024,
			func(snapshot *mobileMemoryRuntimeSnapshot) { snapshot.totalByteCount = 1 })
		close(cancelled)
		if observer.WaitForDeviceClose(1) {
			t.Fatal("withheld lifecycle join passed")
		}
		close(joined)
		result := memoryTeardownResultForTest(t, observer.FinishAndTakeJson(1000))
		if result.State != "failed" || !result.ObserverJoined || result.TerminalSequence != 0 {
			t.Fatalf("late close erased failed join: %+v", result)
		}
	})
}

// Abort owns its native join and preserves every already produced primitive.
func TestMobileMemoryTeardownAbortDrainsAndJoins(t *testing.T) {
	observer := newMobileMemoryTeardownObservation(t.Context(), make(chan struct{}), make(chan struct{}), 32*1024*1024,
		func(snapshot *mobileMemoryRuntimeSnapshot) { snapshot.totalByteCount = 123 })
	result := memoryTeardownResultForTest(t, observer.AbortAndTakeJson(1000))
	if result.State != "failed" || !result.ObserverJoined || result.Drained != result.Produced || result.Drained < 2 || result.TerminalSequence != 0 {
		t.Fatalf("abort discarded or qualified partial observations: %+v", result)
	}
}

// A caller deadline while the terminal reader is withheld cannot become a
// successful native receipt when that read eventually returns.
func TestMobileMemoryTeardownRejectsTerminalReadAfterCallerTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cancelled, joined := make(chan struct{}), make(chan struct{})
		terminalReading, release := make(chan struct{}), make(chan struct{})
		var reads atomic.Int64
		observer := newMobileMemoryTeardownObservation(t.Context(), cancelled, joined, 32*1024*1024,
			func(snapshot *mobileMemoryRuntimeSnapshot) {
				snapshot.totalByteCount = reads.Add(1)
				if snapshot.totalByteCount == 4 {
					close(terminalReading)
					<-release
				}
			})
		close(cancelled)
		close(joined)
		encoded := make(chan string, 1)
		go func() { encoded <- observer.FinishAndTakeJson(1) }()
		<-terminalReading
		incomplete := memoryTeardownResultForTest(t, <-encoded)
		if incomplete.State != "failed" || incomplete.ObserverJoined {
			t.Fatalf("withheld native reader qualified caller timeout: %+v", incomplete)
		}
		close(release)
		<-observer.done
		result := memoryTeardownResultForTest(t, observer.FinishAndTakeJson(1000))
		if result.State != "failed" || !result.ObserverJoined || result.Failure != "observer-aborted-or-expired" {
			t.Fatalf("late terminal read erased timeout: %+v", result)
		}
	})
}

// Concurrent bridge callers cannot race sample reads or create two terminals.
func TestMobileMemoryTeardownTerminalIsFreshUniqueAndLast(t *testing.T) {
	cancelled, joined := make(chan struct{}), make(chan struct{})
	observer := newMobileMemoryTeardownObservation(t.Context(), cancelled, joined, 32*1024*1024,
		func(snapshot *mobileMemoryRuntimeSnapshot) { snapshot.totalByteCount = 1 })
	close(cancelled)
	close(joined)
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			result := memoryTeardownResultForTest(t, observer.FinishAndTakeJson(1000))
			terminalCount := 0
			for _, sample := range result.Samples {
				if sample.Stage == "terminal" {
					terminalCount++
				}
			}
			if result.State != "complete" || terminalCount != 1 || result.TerminalSequence != result.Produced {
				t.Errorf("concurrent terminal mutation: %+v", result)
			}
		}()
	}
	workers.Wait()
}

// The observer keeps copied channels/target, not an entire cancelled device.
func TestMobileMemoryTeardownRecorderHasBoundedPrimitiveOwnership(t *testing.T) {
	if mobileMemoryTeardownCapacity < int(mobileMemoryTeardownLifetime/mobileMemorySampleInterval)+6 {
		t.Fatal("finite lifetime and lifecycle events exceed retained ring")
	}
	deviceType := reflect.TypeOf((*DeviceLocal)(nil))
	observerType := reflect.TypeOf(MemoryTeardownObservation{})
	for i := 0; i < observerType.NumField(); i++ {
		field := observerType.Field(i)
		if field.Type == deviceType || field.Type.Kind() == reflect.Func && field.Name != "cancel" {
			t.Fatalf("observer stores device/closure field %s", field.Name)
		}
	}
}

// Use actual DeviceLocal.Close and its withheld worker, rather than treating a
// caller boolean or cancelled context as device shutdown evidence.
func TestDeviceMemoryTeardownUsesActualAsyncCloseAndJoinedLifecycle(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	device := newPeerPinTestDevice(t, fixture, peerPinDeviceSettings(32*1024*1024), "")
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	device.lifecycleWorkers.Add(1)
	go func() { defer device.lifecycleWorkers.Done(); <-release }()
	observer, err := device.BeginMemoryTeardownObservation()
	if err != nil {
		t.Fatal(err)
	}
	device.Close()
	select {
	case <-device.ctx.Done():
	default:
		t.Fatal("asynchronous Close did not cancel the device")
	}
	select {
	case <-device.lifecycleDone:
		t.Fatal("Close joined a deliberately withheld lifecycle worker")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	if !observer.WaitForDeviceClose(5000) {
		t.Fatal("actual device lifecycle did not join")
	}
	device.TakeMemorySamplesJson()
	result := memoryTeardownResultForTest(t, observer.FinishAndTakeJson(5000))
	if result.State != "complete" || result.DeviceTargetByteCount != 32*1024*1024 || result.CancelSequence == 0 || result.JoinSequence == 0 {
		t.Fatalf("actual native lifecycle was not bound: %+v", result)
	}
}

// Against the preserved v2 observer this fails because timeout discards the
// already-completed high prefix. No delayed read or fake terminal is counted.
func TestMobileMemoryTeardownTimeoutRetainsImmutableCompletedPrefix(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cancelled, joined := make(chan struct{}), make(chan struct{})
		terminalReading, release := make(chan struct{}), make(chan struct{})
		var reads atomic.Int64
		observer := newMobileMemoryTeardownObservation(t.Context(), cancelled, joined, 32*1024*1024,
			func(snapshot *mobileMemoryRuntimeSnapshot) {
				sequence := reads.Add(1)
				*snapshot = mobileMemoryRuntimeSnapshot{totalByteCount: 20 * 1024 * 1024, limitByteCount: 32 * 1024 * 1024}
				if sequence == 2 {
					snapshot.totalByteCount = 32*1024*1024 + 1
				}
				if sequence == 4 {
					close(terminalReading)
					<-release
				}
			})
		close(cancelled)
		close(joined)
		encoded := make(chan string, 1)
		go func() { encoded <- observer.FinishAndTakeJson(1) }()
		<-terminalReading
		partial := memoryTeardownResultForTest(t, <-encoded)
		// Always release/join before asserting, including the expected red.
		close(release)
		<-observer.done
		if partial.State != "failed" || partial.ObserverJoined || partial.Produced != 3 || partial.Drained != 3 ||
			partial.CancelSequence != 2 || partial.JoinSequence != 3 || partial.TerminalSequence != 0 || len(partial.Samples) != 3 {
			t.Fatalf("timeout discarded completed native prefix: %+v", partial)
		}
		if partial.Samples[1].GoRuntimeByteCount != 32*1024*1024+1 {
			t.Fatalf("known cancelled-stage high was erased: %+v", partial)
		}
		final := memoryTeardownResultForTest(t, observer.FinishAndTakeJson(1000))
		if final.State != "failed" || final.Failure != "observer-aborted-or-expired" || final.Produced != 4 ||
			len(partial.Samples) != 3 || partial.TerminalSequence != 0 {
			t.Fatal("late completion mutated the published timeout prefix or rescued failure")
		}
	})
}

// A failed later native reader still publishes all prior completed evidence.
func TestMobileMemoryTeardownReaderFailurePreservesPriorHigh(t *testing.T) {
	cancelled, joined := make(chan struct{}), make(chan struct{})
	var reads atomic.Int64
	observer := newMobileMemoryTeardownObservation(t.Context(), cancelled, joined, 32*1024*1024,
		func(snapshot *mobileMemoryRuntimeSnapshot) {
			if reads.Add(1) > 1 {
				panic("synthetic reader failure")
			}
			*snapshot = mobileMemoryRuntimeSnapshot{totalByteCount: 32*1024*1024 + 1, limitByteCount: 32 * 1024 * 1024}
		})
	close(cancelled)
	result := memoryTeardownResultForTest(t, observer.FinishAndTakeJson(1000))
	if result.State != "failed" || result.Failure != "native-sample-failed" || !result.ObserverJoined ||
		result.Produced != 1 || len(result.Samples) != 1 || result.Samples[0].GoRuntimeByteCount != 32*1024*1024+1 {
		t.Fatalf("reader failure erased retained evidence: %+v", result)
	}
}

// Unlike the public-boundary red above, this is a new-feature ownership control:
// the previous immutable publication cannot grow with a later ring append.
func TestMobileMemoryTeardownPublishedPrefixDoesNotGrowOrMutate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cancelled, joined := make(chan struct{}), make(chan struct{})
		terminalReading, release := make(chan struct{}), make(chan struct{})
		var reads atomic.Int64
		observer := newMobileMemoryTeardownObservation(t.Context(), cancelled, joined, 32*1024*1024,
			func(snapshot *mobileMemoryRuntimeSnapshot) {
				snapshot.totalByteCount = reads.Add(1)
				if snapshot.totalByteCount == 4 {
					close(terminalReading)
					<-release
				}
			})
		close(cancelled)
		close(joined)
		encoded := make(chan string, 1)
		go func() { encoded <- observer.FinishAndTakeJson(1) }()
		<-terminalReading
		published := observer.partial.Load()
		<-encoded
		close(release)
		<-observer.done
		if published == nil || published.Produced != 3 || published.Drained != 3 || published.TerminalSequence != 0 ||
			published.ObserverJoined || len(published.Samples) != 3 || cap(published.Samples) != 3 ||
			published.Samples[2].Stage != "joined" || published.Samples[2].GoRuntimeByteCount != 3 {
			t.Fatalf("completed-prefix publication was mutable: %+v", published)
		}
		if observer.partial.Load() == published || observer.result.State != "failed" {
			t.Fatal("later record reused publication or rescued timeout")
		}
	})
}
