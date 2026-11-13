package sdk

// Tests for ConnectViewController.GetLocationMatch and its listener: the
// connect screen's "No providers in <city>; showing <region>" signal, derived
// from the window-level location match the connect window monitor publishes
// (connect location_match.go), over a local device and across the device rpc.

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// Records, for every LocationMatchChanged signal, what the view controller's
// GetLocationMatch reads then, as an app re-reads it.
type testingLocationMatchListener struct {
	vc              *ConnectViewController
	stateLock       sync.Mutex
	locationMatches []*LocationMatch
	notify          chan struct{}
}

func newTestingLocationMatchListener(vc *ConnectViewController) *testingLocationMatchListener {
	return &testingLocationMatchListener{
		vc:     vc,
		notify: make(chan struct{}, 1024),
	}
}

// LocationMatchListener
func (self *testingLocationMatchListener) LocationMatchChanged() {
	locationMatch := self.vc.GetLocationMatch()
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.locationMatches = append(self.locationMatches, locationMatch)
	}()
	self.notify <- struct{}{}
}

func (self *testingLocationMatchListener) received() []*LocationMatch {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]*LocationMatch{}, self.locationMatches...)
}

// waits until the listener has received count changes
func (self *testingLocationMatchListener) waitForCount(t *testing.T, count int) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for len(self.received()) < count {
		select {
		case <-self.notify:
		case <-deadline:
			t.Fatalf("listener received %d changes, want %d: %+v", len(self.received()), count, self.received())
		}
	}
}

// The exported form the tests expect.
func testingLocationMatch(match string, mixed bool, name string, locationId connect.Id) LocationMatch {
	locationMatch := LocationMatch{
		Match: match,
		Mixed: mixed,
		Name:  name,
	}
	if locationId != (connect.Id{}) {
		locationMatch.LocationId = newId(locationId)
	}
	return locationMatch
}

func assertLocationMatch(t *testing.T, got *LocationMatch, want LocationMatch) {
	t.Helper()
	if got == nil {
		t.Fatal("location match is nil")
	}
	if got.Match != want.Match || got.Mixed != want.Mixed || got.Name != want.Name {
		t.Fatalf("location match = %+v, want %+v", *got, want)
	}
	if (got.LocationId == nil) != (want.LocationId == nil) ||
		(got.LocationId != nil && got.LocationId.Cmp(want.LocationId) != 0) {
		t.Fatalf("location id = %v, want %v", got.LocationId, want.LocationId)
	}
}

// A synthetic provider location with every id set.
func testingProviderLocation(region string, country string) *connect.ProviderLocation {
	regionLocationId := connect.NewId()
	countryLocationId := connect.NewId()
	return &connect.ProviderLocation{
		Country:           country,
		CountryCode:       "zz",
		Region:            region,
		City:              "City " + region,
		RegionLocationId:  &regionLocationId,
		CountryLocationId: &countryLocationId,
	}
}

func TestNewLocationMatchMapsTheWindowMatch(t *testing.T) {
	assertLocationMatch(t, newLocationMatch(connect.WindowLocationMatch{}), LocationMatch{})

	locationId := connect.NewId()
	assertLocationMatch(
		t,
		newLocationMatch(connect.WindowLocationMatch{
			Match:      connect.LocationMatchCountry,
			Mixed:      true,
			Name:       "Country A",
			LocationId: locationId,
		}),
		testingLocationMatch(LocationMatchCountry, true, "Country A", locationId),
	)
	// no id from the server is a nil id, never the all-zero one
	assertLocationMatch(
		t,
		newLocationMatch(connect.WindowLocationMatch{Match: connect.LocationMatchRegion}),
		LocationMatch{Match: LocationMatchRegion},
	)
	// a level this build does not know reads as exact, name and all, whether
	// a newer device process sends its own name for it or connect's other level
	assertLocationMatch(
		t,
		newLocationMatch(connect.WindowLocationMatch{Match: "continent", Name: "Continent A", LocationId: locationId}),
		LocationMatch{},
	)
	assertLocationMatch(
		t,
		newLocationMatch(connect.WindowLocationMatch{Match: connect.LocationMatch("continent").Normalize(), Name: "Continent A"}),
		LocationMatch{},
	)
	// the exported levels are the connect levels
	connect.AssertEqual(t, LocationMatchExact, string(connect.LocationMatchExact))
	connect.AssertEqual(t, LocationMatchRegion, string(connect.LocationMatchRegion))
	connect.AssertEqual(t, LocationMatchCountry, string(connect.LocationMatchCountry))
	connect.AssertEqual(t, LocationMatchRegion, LocationTypeRegion)
	connect.AssertEqual(t, LocationMatchCountry, LocationTypeCountry)
}

// Over a local device the grid listens to connect's own merged multi client
// monitor (DeviceLocal.windowMonitor), so the view controller follows the
// providers in use: exact until a fallback provider is Added, mixed while the
// selected location also has one, exact again once the fallback is gone. The
// listener fires once per change.
func TestConnectViewControllerLocationMatchFollowsTheWindowMonitor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	vc := newTestingConnectViewController(ctx)
	listener := newTestingLocationMatchListener(vc)
	sub := vc.AddLocationMatchListener(listener)
	defer sub.Close()

	qualityMonitor := connect.NewRemoteUserNatMultiClientMonitorWithDefaults()
	speedMonitor := connect.NewRemoteUserNatMultiClientMonitorWithDefaults()
	monitor := connect.NewMergedMultiClientMonitor([]connect.MultiClientMonitor{qualityMonitor, speedMonitor})
	grid := newConnectGridWithDefaults(ctx, vc)
	defer grid.close()
	grid.generation = vc.generation
	grid.listenToWindow(monitor)

	assertLocationMatch(t, vc.GetLocationMatch(), LocationMatch{})

	location := testingProviderLocation("Region A", "Country A")
	regionClientId := connect.NewId()
	speedMonitor.AddProviderEventWithLocationMatch(regionClientId, connect.ProviderStateInEvaluation, regionClientId, location, connect.LocationMatchRegion, connect.IpFamilyLegacy, nil)
	speedMonitor.AddProviderEventWithLocationMatch(regionClientId, connect.ProviderStateAdded, regionClientId, location, connect.LocationMatchRegion, connect.IpFamilyLegacy, nil)
	listener.waitForCount(t, 1)
	region := testingLocationMatch(LocationMatchRegion, false, "Region A", *location.RegionLocationId)
	assertLocationMatch(t, listener.received()[0], region)
	assertLocationMatch(t, vc.GetLocationMatch(), region)

	exactClientId := connect.NewId()
	qualityMonitor.AddProviderEventWithLocationMatch(exactClientId, connect.ProviderStateAdded, exactClientId, testingProviderLocation("Region B", "Country A"), connect.LocationMatchExact, connect.IpFamilyLegacy, nil)
	listener.waitForCount(t, 2)
	mixedRegion := testingLocationMatch(LocationMatchRegion, true, "Region A", *location.RegionLocationId)
	assertLocationMatch(t, listener.received()[1], mixedRegion)
	assertLocationMatch(t, vc.GetLocationMatch(), mixedRegion)

	speedMonitor.AddProviderEventWithLocationMatch(regionClientId, connect.ProviderStateRemoved, regionClientId, location, connect.LocationMatchRegion, connect.IpFamilyLegacy, nil)
	listener.waitForCount(t, 3)
	assertLocationMatch(t, listener.received()[2], LocationMatch{})
	assertLocationMatch(t, vc.GetLocationMatch(), LocationMatch{})

	// an event that does not change the match does not fire the listener. The
	// grid handles the merged events one at a time, so a later event that
	// changes the status is the barrier: once its status change is seen, the
	// earlier event's callback, its location match write included, is done
	otherExactClientId := connect.NewId()
	qualityMonitor.AddProviderEventWithLocationMatch(otherExactClientId, connect.ProviderStateAdded, otherExactClientId, nil, connect.LocationMatchExact, connect.IpFamilyLegacy, nil)
	statusListener := newTestingConnectionStatusListener()
	statusSub := vc.AddConnectionStatusListener(statusListener)
	defer statusSub.Close()
	qualityMonitor.AddWindowExpandEvent(true, 2, false)
	statusListener.waitFor(t, vc, Connected)
	if grid.GetProviderGridPointByClientId(newId(otherExactClientId)) == nil {
		t.Fatal("the exact provider never reached the grid")
	}
	connect.AssertEqual(t, len(listener.received()), 3)
}

// Signals every ConnectionStatusChanged.
type testingConnectionStatusListener struct {
	notify chan struct{}
}

func newTestingConnectionStatusListener() *testingConnectionStatusListener {
	return &testingConnectionStatusListener{
		notify: make(chan struct{}, 1024),
	}
}

// ConnectionStatusListener
func (self *testingConnectionStatusListener) ConnectionStatusChanged() {
	self.notify <- struct{}{}
}

// waits until a signal finds the view controller's status at want
func (self *testingConnectionStatusListener) waitFor(t *testing.T, vc *ConnectViewController, want ConnectionStatus) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-self.notify:
			if vc.GetConnectionStatus() == want {
				return
			}
		case <-deadline:
			t.Fatalf("connection status never reached %s: %s", want, vc.GetConnectionStatus())
		}
	}
}

// The match belongs to one selection: a gesture returns it to exact at once,
// a late event of the outgoing connection cannot bring the fallback back, and
// the next connection's grid publishes its own.
func TestConnectViewControllerLocationMatchResetsOnEachGesture(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	vc := newTestingConnectViewController(ctx)
	listener := newTestingLocationMatchListener(vc)
	sub := vc.AddLocationMatchListener(listener)
	defer sub.Close()

	grid := newConnectGridWithDefaults(ctx, vc)
	defer grid.close()
	grid.generation = vc.generation
	monitor := newTestingGridWindowMonitor()
	grid.listenToWindow(monitor)

	locationId := connect.NewId()
	func() {
		monitor.mu.Lock()
		defer monitor.mu.Unlock()
		monitor.windowExpandEvent = connect.WindowExpandEvent{
			TargetSize:   2,
			MinSatisfied: true,
			LocationMatch: connect.WindowLocationMatch{
				Match:      connect.LocationMatchCountry,
				Name:       "Country A",
				LocationId: locationId,
			},
		}
	}()
	a := connect.NewId()
	monitor.emit(map[connect.Id]*connect.ProviderEvent{
		a: {ClientId: a, State: connect.ProviderStateAdded, LocationMatch: connect.LocationMatchCountry},
	})
	country := testingLocationMatch(LocationMatchCountry, false, "Country A", locationId)
	assertLocationMatch(t, vc.GetLocationMatch(), country)
	connect.AssertEqual(t, vc.GetConnectionStatus(), Connected)
	connect.AssertEqual(t, len(listener.received()), 1)

	// the user picks another location: exact before any device call returns
	stamped := vc.generation
	vc.beginGeneration(DestinationSet)
	assertLocationMatch(t, vc.GetLocationMatch(), LocationMatch{})
	connect.AssertEqual(t, len(listener.received()), 2)
	assertLocationMatch(t, listener.received()[1], LocationMatch{})

	// the outgoing connection's late event, through its grid and through a
	// write that passed the grid's entry gate before the gesture
	b := connect.NewId()
	monitor.emit(map[connect.Id]*connect.ProviderEvent{
		b: {ClientId: b, State: connect.ProviderStateAdded, LocationMatch: connect.LocationMatchCountry},
	})
	staleLocationMatch := func() connect.WindowLocationMatch {
		monitor.mu.Lock()
		defer monitor.mu.Unlock()
		return monitor.windowExpandEvent.LocationMatch
	}()
	vc.setConnectionForGeneration(Connected, staleLocationMatch, stamped)
	assertLocationMatch(t, vc.GetLocationMatch(), LocationMatch{})
	connect.AssertEqual(t, len(listener.received()), 2)

	// the new selection's grid publishes the window it listens to
	grid2 := newConnectGridWithDefaults(ctx, vc)
	defer grid2.close()
	grid2.generation = vc.generation
	grid2.listenToWindow(monitor)
	assertLocationMatch(t, vc.GetLocationMatch(), country)
	connect.AssertEqual(t, len(listener.received()), 3)

	// a disconnect returns it to exact as well
	vc.beginGeneration(Disconnected)
	assertLocationMatch(t, vc.GetLocationMatch(), LocationMatch{})
	connect.AssertEqual(t, len(listener.received()), 4)
}

// An exact window, a window of an older device process (no field), and a level
// this build does not know never produce a notice or a listener call.
func TestConnectViewControllerLocationMatchIsExactWithoutAKnownFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	vc := newTestingConnectViewController(ctx)
	listener := newTestingLocationMatchListener(vc)
	sub := vc.AddLocationMatchListener(listener)
	defer sub.Close()

	grid := newConnectGridWithDefaults(ctx, vc)
	defer grid.close()
	grid.generation = vc.generation
	monitor := newTestingGridWindowMonitor()
	grid.listenToWindow(monitor)

	a := connect.NewId()
	func() {
		monitor.mu.Lock()
		defer monitor.mu.Unlock()
		monitor.windowExpandEvent = connect.WindowExpandEvent{TargetSize: 2, MinSatisfied: true}
	}()
	monitor.emit(map[connect.Id]*connect.ProviderEvent{
		a: {ClientId: a, State: connect.ProviderStateAdded},
	})
	connect.AssertEqual(t, vc.GetConnectionStatus(), Connected)
	assertLocationMatch(t, vc.GetLocationMatch(), LocationMatch{})

	func() {
		monitor.mu.Lock()
		defer monitor.mu.Unlock()
		monitor.windowExpandEvent.LocationMatch = connect.WindowLocationMatch{
			Match:      "continent",
			Name:       "Continent A",
			LocationId: connect.NewId(),
		}
	}()
	b := connect.NewId()
	monitor.emit(map[connect.Id]*connect.ProviderEvent{
		b: {ClientId: b, State: connect.ProviderStateAdded, LocationMatch: "continent"},
	})
	assertLocationMatch(t, vc.GetLocationMatch(), LocationMatch{})
	connect.AssertEqual(t, len(listener.received()), 0)
}

// Sets a grid test monitor's window to a satisfied one whose providers are in
// a region, and returns the match the view controller should read.
func testingWindowMonitorWithRegion(monitor *testing_gridWindowMonitor) LocationMatch {
	locationId := connect.NewId()
	func() {
		monitor.mu.Lock()
		defer monitor.mu.Unlock()
		monitor.windowExpandEvent = connect.WindowExpandEvent{
			TargetSize:   2,
			MinSatisfied: true,
			LocationMatch: connect.WindowLocationMatch{
				Match:      connect.LocationMatchRegion,
				Name:       "Region A",
				LocationId: locationId,
			},
		}
	}()
	return testingLocationMatch(LocationMatchRegion, false, "Region A", locationId)
}

// Through a device's view controller and its public Connect and Disconnect:
// the connection's grid publishes its window's match, and the disconnect
// returns it to exact, one signal each.
func TestConnectViewControllerLocationMatchFollowsPublicConnectAndDisconnect(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	controller := testingPreferenceController(t, device)
	region := testingWindowMonitorWithRegion(controller.testingWindowMonitor.(*testing_gridWindowMonitor))
	listener := newTestingLocationMatchListener(controller)
	sub := controller.AddLocationMatchListener(listener)
	defer sub.Close()

	controller.Connect(testingSpecificPreferenceLocation())
	connect.AssertEqual(t, controller.GetConnectionStatus(), Connected)
	assertLocationMatch(t, controller.GetLocationMatch(), region)

	controller.Disconnect()
	connect.AssertEqual(t, controller.GetConnectionStatus(), Disconnected)
	assertLocationMatch(t, controller.GetLocationMatch(), LocationMatch{})

	received := listener.received()
	if len(received) != 2 {
		t.Fatalf("listener received %d changes, want 2: %+v", len(received), received)
	}
	assertLocationMatch(t, received[0], region)
	assertLocationMatch(t, received[1], LocationMatch{})
}

// With autosave the gesture (beginLocalPreferenceGesture) changes neither the
// status nor the match; the grid change that follows does. A reconnect clears
// the match before its new grid publishes its window's, and a disconnect
// clears it for good.
func TestConnectViewControllerLocationMatchResetsOnTheAutoSaveGridChange(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	controller := testingPreferenceController(t, device)
	region := testingWindowMonitorWithRegion(controller.testingWindowMonitor.(*testing_gridWindowMonitor))
	listener := newTestingLocationMatchListener(controller)
	sub := controller.AddLocationMatchListener(listener)
	defer sub.Close()

	location := testingSpecificPreferenceLocation()
	controller.Connect(location)
	assertLocationMatch(t, controller.GetLocationMatch(), region)

	controller.Connect(cloneConnectLocation(location))
	assertLocationMatch(t, controller.GetLocationMatch(), region)

	controller.Disconnect()
	connect.AssertEqual(t, controller.GetConnectionStatus(), Disconnected)
	assertLocationMatch(t, controller.GetLocationMatch(), LocationMatch{})

	received := listener.received()
	want := []LocationMatch{region, {}, region, {}}
	if len(received) != len(want) {
		t.Fatalf("listener received %d changes, want %d: %+v", len(received), len(want), received)
	}
	for i := range want {
		assertLocationMatch(t, received[i], want[i])
	}
}

// A disconnect the device makes itself, from another process or a sign-out,
// reaches the view controller as ConnectLocationChanged(nil) with no gesture:
// the grid goes, and the match returns to exact with it.
func TestConnectViewControllerLocationMatchResetsWhenTheDeviceDisconnects(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	vc := newTestingConnectViewController(ctx)
	monitor := newTestingGridWindowMonitor()
	vc.testingWindowMonitor = monitor
	region := testingWindowMonitorWithRegion(monitor)
	listener := newTestingLocationMatchListener(vc)
	sub := vc.AddLocationMatchListener(listener)
	defer sub.Close()

	vc.ConnectLocationChanged(&ConnectLocation{
		ConnectLocationId: &ConnectLocationId{LocationId: NewId()},
		Name:              "City A",
	})
	connect.AssertEqual(t, vc.GetConnectionStatus(), Connected)
	assertLocationMatch(t, vc.GetLocationMatch(), region)

	generation := vc.generation
	vc.ConnectLocationChanged(nil)
	connect.AssertEqual(t, vc.generation, generation)
	connect.AssertEqual(t, vc.GetConnectionStatus(), Disconnected)
	assertLocationMatch(t, vc.GetLocationMatch(), LocationMatch{})

	received := listener.received()
	if len(received) != 2 {
		t.Fatalf("listener received %d changes, want 2: %+v", len(received), received)
	}
	assertLocationMatch(t, received[0], region)
	assertLocationMatch(t, received[1], LocationMatch{})
}

// Across the device rpc the window-level match rides the expand event the
// device process forwards, so a remote device's view controller reads the
// same match, name and id. An older device process sends no field and reads
// as exact; an older app decodes a newer event without error.
func TestConnectViewControllerLocationMatchCrossesTheDeviceRpc(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	location := testingProviderLocation("Region A", "Country A")
	clientId := connect.NewId()
	sent := &DeviceRemoteWindowMonitorEvent{
		WindowIds: map[connect.Id]bool{connect.NewId(): true},
		WindowExpandEvent: &connect.WindowExpandEvent{
			TargetSize:   1,
			MinSatisfied: true,
			LocationMatch: connect.WindowLocationMatch{
				Match:      connect.LocationMatchRegion,
				Name:       location.Region,
				LocationId: *location.RegionLocationId,
			},
		},
		ProviderEvents: map[connect.Id]*connect.ProviderEvent{
			clientId: {
				ClientId:      clientId,
				State:         connect.ProviderStateAdded,
				Location:      location,
				LocationMatch: connect.LocationMatchRegion,
			},
		},
		Reset: true,
	}
	received := gobRoundTrip(t, sent)
	connect.AssertEqual(t, received.WindowExpandEvent.LocationMatch, sent.WindowExpandEvent.LocationMatch)
	connect.AssertEqual(t, received.ProviderEvents[clientId].LocationMatch, connect.LocationMatchRegion)

	deviceRemote := newTestDeviceRemoteWithNoService(t)
	vc := newTestingConnectViewController(ctx)
	grid := newConnectGridWithDefaults(ctx, vc)
	defer grid.close()
	grid.generation = vc.generation
	windowMonitor := deviceRemote.windowMonitor()
	remoteWindowMonitor, ok := windowMonitor.(*deviceRemoteWindowMonitor)
	if !ok {
		t.Fatalf("window monitor = %T, expected the remote one", windowMonitor)
	}
	grid.listenToWindow(windowMonitor)
	deliver := func(event *DeviceRemoteWindowMonitorEvent) {
		deviceRemote.windowMonitorEvent(
			map[connect.Id]bool{remoteWindowMonitor.windowId: true},
			event.WindowExpandEvent,
			event.ProviderEvents,
			event.Reset,
		)
	}
	deliver(received)
	assertLocationMatch(t, vc.GetLocationMatch(), testingLocationMatch(LocationMatchRegion, false, "Region A", *location.RegionLocationId))

	// an older device process: the expand and provider events have no field
	type olderWindowExpandEvent struct {
		TargetSize   int
		MinSatisfied bool
	}
	type olderProviderEvent struct {
		ClientId connect.Id
		State    connect.ProviderState
	}
	type olderWindowMonitorEvent struct {
		WindowIds         map[connect.Id]bool
		WindowExpandEvent *olderWindowExpandEvent
		ProviderEvents    map[connect.Id]*olderProviderEvent
		Reset             bool
	}
	older := &olderWindowMonitorEvent{
		WindowIds:         map[connect.Id]bool{connect.NewId(): true},
		WindowExpandEvent: &olderWindowExpandEvent{TargetSize: 1, MinSatisfied: true},
		ProviderEvents: map[connect.Id]*olderProviderEvent{
			clientId: {ClientId: clientId, State: connect.ProviderStateAdded},
		},
		Reset: true,
	}
	fromOlder := gobRoundTripAs[DeviceRemoteWindowMonitorEvent](t, older)
	connect.AssertEqual(t, fromOlder.WindowExpandEvent.LocationMatch, connect.WindowLocationMatch{})
	connect.AssertEqual(t, fromOlder.ProviderEvents[clientId].LocationMatch, connect.LocationMatchExact)
	deliver(&fromOlder)
	assertLocationMatch(t, vc.GetLocationMatch(), LocationMatch{})
	connect.AssertEqual(t, vc.GetConnectionStatus(), Connected)

	// an older app reading a newer device process ignores the fields
	toOlder := gobRoundTripAs[olderWindowMonitorEvent](t, sent)
	connect.AssertEqual(t, toOlder.WindowExpandEvent.TargetSize, 1)
	connect.AssertEqual(t, toOlder.ProviderEvents[clientId].State, connect.ProviderStateAdded)
}

// Every field of the window monitor event crosses the rpc, the location
// match's included, so a field added later that gob cannot carry fails here.
func TestRpcGobWindowMonitorEventComplete(t *testing.T) {
	seed := 0
	event := &DeviceRemoteWindowMonitorEvent{}
	fillNonZero(t, reflect.ValueOf(event), &seed)
	if event.WindowExpandEvent.LocationMatch == (connect.WindowLocationMatch{}) {
		t.Fatal("the fixture did not fill the location match")
	}
	received := gobRoundTrip(t, event)
	if !reflect.DeepEqual(received, event) {
		t.Fatalf("window monitor event changed in the gob round trip:\nsent %+v\ngot  %+v", event, received)
	}
}
