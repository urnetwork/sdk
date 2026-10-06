// Callback completion and persisted loads preserve bounded queue ownership.
package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A bare queue owns only one explicitly joined flush and synthetic events.
type eventTrimFixture struct {
	queue    *ClientEventQueue
	original []*queuedClientEvent
	complete func(*ClientEventsSendResult, error)
	done     <-chan struct{}
}

// The send seam publishes a barrier only after the queue snapshots its batch.
func newEventTrimFixture(t *testing.T, attempts int, persist bool) *eventTrimFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	type sendCall struct {
		events   []*ClientEvent
		complete func(*ClientEventsSendResult, error)
	}
	ready := make(chan sendCall, 1)
	done := make(chan struct{})
	queue := &ClientEventQueue{
		ctx:     ctx,
		pending: make([]*queuedClientEvent, 0, clientEventQueueCapacity+1),
		hasJwt:  func() bool { return true },
		send: func(events []*ClientEvent, complete func(*ClientEventsSendResult, error)) {
			ready <- sendCall{events: append([]*ClientEvent(nil), events...), complete: complete}
		},
	}
	for i := 0; i < clientEventQueueCapacity; i++ {
		queue.Add(NewOnboardingStepShownEvent("seed", i, -1))
		if i < MaxClientEventsPerCall {
			queue.pending[i].Attempts = attempts
		}
	}
	original := append([]*queuedClientEvent(nil), queue.pending...)
	if persist {
		queue.statePath = filepath.Join(t.TempDir(), "events.json")
	}
	t.Cleanup(func() {
		cancel()
		<-done
	})
	go func() {
		defer close(done)
		queue.flushOnce()
	}()
	call := <-ready
	if len(call.events) != MaxClientEventsPerCall {
		t.Fatalf("sent batch=%d want=%d", len(call.events), MaxClientEventsPerCall)
	}
	for i, event := range call.events {
		if event != original[i].Event {
			t.Fatalf("sent owner %d differs from original head", i)
		}
	}
	return &eventTrimFixture{
		queue: queue, original: original, complete: call.complete, done: done,
	}
}

// Arrivals use the shipping cap policy while the callback is still withheld.
func (self *eventTrimFixture) addArrivals(count int) []*queuedClientEvent {
	arrivals := make([]*queuedClientEvent, 0, count)
	for i := 0; i < count; i++ {
		self.queue.Add(NewOnboardingStepShownEvent("arrival", i, -1))
		self.queue.mutex.Lock()
		arrivals = append(arrivals, self.queue.pending[len(self.queue.pending)-1])
		self.queue.mutex.Unlock()
	}
	return arrivals
}

// Completing the callback joins the only worker before assertions read owners.
func (self *eventTrimFixture) finish(err error) {
	self.complete(&ClientEventsSendResult{Accepted: MaxClientEventsPerCall}, err)
	<-self.done
}

// Pointer identity distinguishes separate admissions of the same event object.
func requireEventTrimOwners(t *testing.T, queue *ClientEventQueue, want []*queuedClientEvent) {
	t.Helper()
	queue.mutex.Lock()
	defer queue.mutex.Unlock()
	if len(queue.pending) != len(want) {
		t.Errorf("pending count=%d want=%d", len(queue.pending), len(want))
	}
	for i := 0; i < len(queue.pending) && i < len(want); i++ {
		if queue.pending[i] != want[i] {
			t.Errorf("pending owner %d props=%s want=%s", i,
				queue.pending[i].Event.PropsJson(), want[i].Event.PropsJson())
			return
		}
	}
	if len(queue.pending) > clientEventQueueCapacity {
		t.Errorf("pending count=%d exceeds unchanged cap=%d", len(queue.pending), clientEventQueueCapacity)
	}
}

// Success may remove only the admitted batch, not an unsent replacement head.
func TestClientEventQueueSuccessAfterHeadEviction(t *testing.T) {
	fixture := newEventTrimFixture(t, 0, false)
	arrivals := fixture.addArrivals(1)
	fixture.finish(nil)
	want := append(append([]*queuedClientEvent(nil), fixture.original[MaxClientEventsPerCall:]...), arrivals...)
	requireEventTrimOwners(t, fixture.queue, want)
}

// A failed batch cannot resurrect an owner already evicted by the cap.
func TestClientEventQueueRetryAfterHeadEviction(t *testing.T) {
	fixture := newEventTrimFixture(t, 0, false)
	arrivals := fixture.addArrivals(1)
	fixture.finish(errors.New("synthetic send failure"))
	want := append(append([]*queuedClientEvent(nil), fixture.original[1:]...), arrivals...)
	requireEventTrimOwners(t, fixture.queue, want)
	for i, owner := range want {
		wantAttempts := 0
		if i < MaxClientEventsPerCall-1 {
			wantAttempts = 1
		}
		if owner.Attempts != wantAttempts {
			t.Errorf("live owner %d attempts=%d want=%d", i, owner.Attempts, wantAttempts)
		}
	}
}

// Once arrivals evict the entire batch, neither outcome can retire newer work.
func TestClientEventQueueCompletionAfterWholeBatchEviction(t *testing.T) {
	for _, sendErr := range []error{nil, errors.New("synthetic send failure")} {
		fixture := newEventTrimFixture(t, 0, false)
		arrivals := fixture.addArrivals(MaxClientEventsPerCall)
		fixture.finish(sendErr)
		want := append(append([]*queuedClientEvent(nil), fixture.original[MaxClientEventsPerCall:]...), arrivals...)
		requireEventTrimOwners(t, fixture.queue, want)
		for i, owner := range want {
			if owner.Attempts != 0 {
				t.Errorf("unsent owner %d acquired attempts=%d for error=%v", i, owner.Attempts, sendErr)
			}
		}
	}
}

// The fixed retry limit applies to surviving batch owners, never new arrivals.
func TestClientEventQueueLastAttemptAfterHeadEviction(t *testing.T) {
	fixture := newEventTrimFixture(t, ClientEventMaxAttempts-1, false)
	arrivals := fixture.addArrivals(1)
	fixture.finish(errors.New("synthetic send failure"))
	want := append(append([]*queuedClientEvent(nil), fixture.original[MaxClientEventsPerCall:]...), arrivals...)
	requireEventTrimOwners(t, fixture.queue, want)
}

// Persisted state must contain exactly the live unsent and retryable owners.
func TestClientEventQueuePersistenceAfterHeadEviction(t *testing.T) {
	for _, sendErr := range []error{nil, errors.New("synthetic send failure")} {
		fixture := newEventTrimFixture(t, 0, true)
		arrivals := fixture.addArrivals(1)
		fixture.finish(sendErr)
		first := MaxClientEventsPerCall
		if sendErr != nil {
			first = 1
		}
		want := append(append([]*queuedClientEvent(nil), fixture.original[first:]...), arrivals...)
		restored := &ClientEventQueue{statePath: fixture.queue.statePath}
		restored.load()
		if len(restored.pending) != len(want) {
			t.Errorf("restored count=%d want=%d", len(restored.pending), len(want))
		}
		for i := 0; i < len(restored.pending) && i < len(want); i++ {
			if restored.pending[i].Event.PropsJson() != want[i].Event.PropsJson() ||
				restored.pending[i].Attempts != want[i].Attempts {
				t.Errorf("restored owner %d props=%s attempts=%d want props=%s attempts=%d", i,
					restored.pending[i].Event.PropsJson(), restored.pending[i].Attempts,
					want[i].Event.PropsJson(), want[i].Attempts)
				break
			}
		}
	}
}

// Re-admitting the same payload creates a distinct logical queue owner.
func TestClientEventQueueSameEventNewOwnerSurvives(t *testing.T) {
	fixture := newEventTrimFixture(t, 0, false)
	fixture.queue.Add(fixture.original[0].Event)
	fixture.queue.mutex.Lock()
	arrival := fixture.queue.pending[len(fixture.queue.pending)-1]
	fixture.queue.mutex.Unlock()
	fixture.finish(nil)
	want := append(append([]*queuedClientEvent(nil), fixture.original[MaxClientEventsPerCall:]...), arrival)
	requireEventTrimOwners(t, fixture.queue, want)
}

// Eviction releases the retired backing slot while retaining the live suffix.
func TestClientEventQueueCapacityEvictionReleasesRetiredSlot(t *testing.T) {
	queue := &ClientEventQueue{pending: make([]*queuedClientEvent, 0, clientEventQueueCapacity+1)}
	for i := 0; i < clientEventQueueCapacity; i++ {
		queue.Add(NewOnboardingStepShownEvent("seed", i, -1))
	}
	backing := queue.pending[:cap(queue.pending)]
	queue.Add(NewOnboardingStepShownEvent("arrival", 0, -1))
	if backing[0] != nil {
		t.Error("capacity-evicted owner remains rooted by the retained backing array")
	}
	if len(queue.pending) != clientEventQueueCapacity || queue.pending[0] != backing[1] ||
		queue.pending[len(queue.pending)-1] != backing[len(backing)-1] {
		t.Error("capacity eviction changed the live suffix or its bound")
	}
}

// Retiring a batch releases its backing references, not just its visible count.
func TestClientEventQueueSuccessfulFlushReleasesRetiredSlots(t *testing.T) {
	fixture := newEventTrimFixture(t, 0, false)
	backing := fixture.queue.pending
	retired := map[*queuedClientEvent]bool{}
	for _, owner := range fixture.original[:MaxClientEventsPerCall] {
		retired[owner] = true
	}
	fixture.finish(nil)
	for i, owner := range backing {
		if retired[owner] {
			t.Errorf("completed owner remains rooted in backing slot %d", i)
			break
		}
	}
	requireEventTrimOwners(t, fixture.queue, fixture.original[MaxClientEventsPerCall:])
}

// Synthetic valid rows are interspersed with rows load already rejects.
func newEventLoadFixture(t *testing.T, count int) (*ClientEventQueue, []*queuedClientEvent) {
	t.Helper()
	valid := make([]*queuedClientEvent, 0, count)
	rows := []*queuedClientEvent{nil, {Event: nil}, {Event: &ClientEvent{Name: ""}}}
	for i := 0; i < count; i += 1 {
		owner := &queuedClientEvent{
			Event:    NewOnboardingStepShownEvent("persisted", i, -1),
			Attempts: i % ClientEventMaxAttempts,
		}
		valid = append(valid, owner)
		rows = append(rows, owner)
		if i == 1 {
			rows = append(rows, nil, &queuedClientEvent{Event: nil})
		}
	}
	rows = append(rows, nil, &queuedClientEvent{Event: &ClientEvent{Name: ""}})
	data, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(t.TempDir(), "events.json")
	if err := os.WriteFile(statePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	return &ClientEventQueue{statePath: statePath}, valid
}

// Serialization changes pointers, so compare every surviving payload and attempt.
func requireEventLoadOwners(t *testing.T, got, want []*queuedClientEvent) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("loaded count=%d want=%d", len(got), len(want))
	}
	for i := 0; i < len(got) && i < len(want); i += 1 {
		if got[i].Event.PropsJson() != want[i].Event.PropsJson() || got[i].Attempts != want[i].Attempts {
			t.Errorf("loaded owner %d props=%s attempts=%d want props=%s attempts=%d", i,
				got[i].Event.PropsJson(), got[i].Attempts, want[i].Event.PropsJson(), want[i].Attempts)
			return
		}
	}
}

// An oversized valid snapshot retains the newest capacity entries across save.
func TestClientEventQueueLoadTrimsToNewestCapacity(t *testing.T) {
	queue, valid := newEventLoadFixture(t, clientEventQueueCapacity+3)
	queue.load()
	want := valid[len(valid)-clientEventQueueCapacity:]
	requireEventLoadOwners(t, queue.pending, want)

	queue.mutex.Lock()
	queue.saveLocked()
	queue.mutex.Unlock()
	restored := &ClientEventQueue{statePath: queue.statePath}
	restored.load()
	requireEventLoadOwners(t, restored.pending, want)

	queue.Add(NewOnboardingStepShownEvent("arrival", 0, -1))
	arrival := queue.pending[len(queue.pending)-1]
	want = append(append([]*queuedClientEvent(nil), want[1:]...), arrival)
	requireEventLoadOwners(t, queue.pending, want)
}

// Invalid disk rows do not evict valid entries below or exactly at capacity.
func TestClientEventQueueLoadPreservesAdmittedCapacity(t *testing.T) {
	for _, count := range []int{0, 1, clientEventQueueCapacity} {
		queue, valid := newEventLoadFixture(t, count)
		queue.load()
		requireEventLoadOwners(t, queue.pending, valid)
	}
}
