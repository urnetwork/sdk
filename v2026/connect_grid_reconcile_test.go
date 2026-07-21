package sdk

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// testing_gridWindowMonitor is a windowMonitor with a controllable retained
// event map, so tests can simulate events lost between the monitor and the grid.
type testing_gridWindowMonitor struct {
	mu                sync.Mutex
	windowExpandEvent connect.WindowExpandEvent
	providerEvents    map[connect.Id]*connect.ProviderEvent
	callbacks         map[int]connect.MonitorEventFunction
	nextCallbackId    int
	// simulates the rpc to a remote device being down
	unavailable bool
}

func newTestingGridWindowMonitor() *testing_gridWindowMonitor {
	return &testing_gridWindowMonitor{
		windowExpandEvent: connect.WindowExpandEvent{TargetSize: 2, MinSatisfied: false},
		providerEvents:    map[connect.Id]*connect.ProviderEvent{},
		callbacks:         map[int]connect.MonitorEventFunction{},
	}
}

func (self *testing_gridWindowMonitor) setUnavailable(unavailable bool) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.unavailable = unavailable
}

// windowMonitorWithAvailability
func (self *testing_gridWindowMonitor) EventsWithAvailability() (*connect.WindowExpandEvent, map[connect.Id]*connect.ProviderEvent, bool) {
	unavailable := func() bool {
		self.mu.Lock()
		defer self.mu.Unlock()
		return self.unavailable
	}()
	if unavailable {
		return &connect.WindowExpandEvent{}, map[connect.Id]*connect.ProviderEvent{}, false
	}
	windowExpandEvent, providerEvents := self.Events()
	return windowExpandEvent, providerEvents, true
}

func (self *testing_gridWindowMonitor) AddMonitorEventCallback(monitorEventCallback connect.MonitorEventFunction) func() {
	self.mu.Lock()
	defer self.mu.Unlock()
	callbackId := self.nextCallbackId
	self.nextCallbackId += 1
	self.callbacks[callbackId] = monitorEventCallback
	return func() {
		self.mu.Lock()
		defer self.mu.Unlock()
		delete(self.callbacks, callbackId)
	}
}

func (self *testing_gridWindowMonitor) Events() (*connect.WindowExpandEvent, map[connect.Id]*connect.ProviderEvent) {
	self.mu.Lock()
	defer self.mu.Unlock()
	windowExpandEvent := self.windowExpandEvent
	providerEvents := map[connect.Id]*connect.ProviderEvent{}
	for clientId, providerEvent := range self.providerEvents {
		providerEvents[clientId] = providerEvent
	}
	return &windowExpandEvent, providerEvents
}

// emit delivers a diff to subscribers and updates the retained map like the
// real monitor (terminal deletes, non-terminal upserts)
func (self *testing_gridWindowMonitor) emit(events map[connect.Id]*connect.ProviderEvent) {
	callbacks := []connect.MonitorEventFunction{}
	var windowExpandEvent connect.WindowExpandEvent
	func() {
		self.mu.Lock()
		defer self.mu.Unlock()
		for clientId, providerEvent := range events {
			if providerEvent.State.IsTerminal() {
				delete(self.providerEvents, clientId)
			} else {
				self.providerEvents[clientId] = providerEvent
			}
		}
		windowExpandEvent = self.windowExpandEvent
		for _, callback := range self.callbacks {
			callbacks = append(callbacks, callback)
		}
	}()
	for _, callback := range callbacks {
		callback(&windowExpandEvent, events, false)
	}
}

// setRetained force-sets the retained map WITHOUT delivering events, simulating
// events lost between the monitor and the grid (an rpc gap, a dropped callback)
func (self *testing_gridWindowMonitor) setRetained(events map[connect.Id]*connect.ProviderEvent) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.providerEvents = events
}

// TestConnectGridReconcile verifies the grid self-heals from lost events: a
// non-terminal point whose terminal event was lost is reclaimed on reconcile,
// a point whose add was lost appears, and a live point is untouched.
func TestConnectGridReconcile(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	vc := &ConnectViewController{
		ctx:                       ctx,
		device:                    nil,
		selectedLocationListeners: connect.NewCallbackList[SelectedLocationListener](),
		connectionStatusListeners: connect.NewCallbackList[ConnectionStatusListener](),
		gridListeners:             connect.NewCallbackList[GridListener](),
	}

	settings := &connectGridSettings{
		MinSideLength:    16,
		ExpandFraction:   1.5,
		RemoveTimeout:    100 * time.Millisecond,
		ReconcileTimeout: 300 * time.Millisecond,
	}
	grid := newConnectGrid(ctx, vc, settings)
	defer grid.close()

	monitor := newTestingGridWindowMonitor()
	grid.listenToWindow(monitor)

	a := connect.NewId()
	b := connect.NewId()
	c := connect.NewId()

	// a and b in evaluation, delivered through the normal event path
	monitor.emit(map[connect.Id]*connect.ProviderEvent{
		a: {ClientId: a, State: connect.ProviderStateInEvaluation},
		b: {ClientId: b, State: connect.ProviderStateInEvaluation},
	})

	getPoint := func(clientId connect.Id) *ProviderGridPoint {
		return grid.GetProviderGridPointByClientId(newId(clientId))
	}

	if n := grid.GetProviderGridPointList().Len(); n != 2 {
		t.Fatalf("expected 2 points, got %d", n)
	}

	// simulate lost events: a's terminal event and c's add never reached the
	// grid. the monitor's retained state is now {b: InEvaluation, c: Added}
	monitor.setRetained(map[connect.Id]*connect.ProviderEvent{
		b: {ClientId: b, State: connect.ProviderStateInEvaluation},
		c: {ClientId: c, State: connect.ProviderStateAdded},
	})

	// within a reconcile period + remove timeout: a is reclaimed, c appears,
	// b remains live
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if getPoint(a) == nil && getPoint(c) != nil && getPoint(b) != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if p := getPoint(a); p != nil {
		t.Errorf("expected a's point reclaimed after reconcile, still present state=%s endTime=%v", p.State, p.EndTime)
	}
	if p := getPoint(c); p == nil {
		t.Errorf("expected c's point added by reconcile")
	} else if p.State != ProviderStateAdded {
		t.Errorf("expected c Added, got %s", p.State)
	}
	if p := getPoint(b); p == nil {
		t.Errorf("expected b's point kept by reconcile")
	} else {
		if p.State != ProviderStateInEvaluation {
			t.Errorf("expected b InEvaluation, got %s", p.State)
		}
		if p.EndTime != nil {
			t.Errorf("expected b not scheduled for removal")
		}
	}
}

// TestConnectGridReconcileFreezeWhenUnavailable verifies the grid freezes rather
// than drains when the monitor state is unreadable (e.g. the rpc to the remote
// device is down): reconcile skips, the dots stay, and once the monitor is
// readable again the reconcile resumes healing.
func TestConnectGridReconcileFreezeWhenUnavailable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	vc := &ConnectViewController{
		ctx:                       ctx,
		device:                    nil,
		selectedLocationListeners: connect.NewCallbackList[SelectedLocationListener](),
		connectionStatusListeners: connect.NewCallbackList[ConnectionStatusListener](),
		gridListeners:             connect.NewCallbackList[GridListener](),
	}

	settings := &connectGridSettings{
		MinSideLength:    16,
		ExpandFraction:   1.5,
		RemoveTimeout:    100 * time.Millisecond,
		ReconcileTimeout: 200 * time.Millisecond,
	}
	grid := newConnectGrid(ctx, vc, settings)
	defer grid.close()

	monitor := newTestingGridWindowMonitor()
	grid.listenToWindow(monitor)

	a := connect.NewId()
	b := connect.NewId()
	monitor.emit(map[connect.Id]*connect.ProviderEvent{
		a: {ClientId: a, State: connect.ProviderStateInEvaluation},
		b: {ClientId: b, State: connect.ProviderStateAdded},
	})
	if n := grid.GetProviderGridPointList().Len(); n != 2 {
		t.Fatalf("expected 2 points, got %d", n)
	}

	// the monitor becomes unreadable (rpc down) and its retained state is
	// cleared. the grid must FREEZE: reconcile skips, both dots stay.
	monitor.setUnavailable(true)
	monitor.setRetained(map[connect.Id]*connect.ProviderEvent{})

	time.Sleep(1 * time.Second) // several reconcile + remove periods
	if n := grid.GetProviderGridPointList().Len(); n != 2 {
		t.Fatalf("expected the grid frozen at 2 points while unavailable, got %d", n)
	}

	// the monitor is readable again: reconcile resumes, and with an empty
	// retained state both dots are reclaimed
	monitor.setUnavailable(false)
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if grid.GetProviderGridPointList().Len() == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n := grid.GetProviderGridPointList().Len(); n != 0 {
		t.Errorf("expected the grid reclaimed after the monitor became readable, got %d points", n)
	}
}
