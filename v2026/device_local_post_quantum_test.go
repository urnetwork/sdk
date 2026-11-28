package sdk

// The device's notice that the selected location has no post-quantum exits
// right now (device_local_post_quantum.go; owner, 2026-11-24: "Tell them, let
// them choose"). Before, only the connect view controller decided it, so
// every surface that runs while the connect screen is closed (a VPN
// notification, a quick settings tile, widgets, a tray, the ios extension)
// could only say connecting. These pin, through a real device with no connect
// view controller and the find-providers2 requests as the platform receives
// them: the device sets the notice by itself; it clears on a location change,
// post-quantum turned off, a connected connection and post-quantum exits
// returning; a failed check is no notice; and the check never holds up the
// connect path or the device's close.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// One find-providers2 request of the notice's check.
type testingPostQuantumSupplyRequest struct {
	locationId            string
	requireModernProvider bool
	// when the request arrived
	time time.Time
}

// Answers the notice's find-providers2 requests: modernCount providers for a
// request with require_modern_provider, count without it, or err. While hold
// is set, a request waits for it to close or for its context to end. Records
// each request, in order.
type testingPostQuantumSupply struct {
	stateLock   sync.Mutex
	modernCount int
	count       int
	err         error
	hold        chan struct{}
	requests    []testingPostQuantumSupplyRequest
	// signaled on each request, as it arrives
	requested chan struct{}
}

func newTestingPostQuantumSupply(modernCount int, count int) *testingPostQuantumSupply {
	return &testingPostQuantumSupply{
		modernCount: modernCount,
		count:       count,
		requested:   make(chan struct{}, 1024),
	}
}

func (self *testingPostQuantumSupply) set(modernCount int, count int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.modernCount = modernCount
	self.count = count
	self.err = nil
}

func (self *testingPostQuantumSupply) setErr(err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.err = err
}

// Holds every request from now until the returned release is called.
func (self *testingPostQuantumSupply) holdRequests() (release func()) {
	hold := make(chan struct{})
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.hold = hold
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			close(hold)
			if self.hold == hold {
				self.hold = nil
			}
		})
	}
}

// Waits until n requests have arrived since the last takeRequests, or fails.
func (self *testingPostQuantumSupply) awaitRequests(t *testing.T, ctx context.Context, n int, description string) {
	t.Helper()
	for {
		count := func() int {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			return len(self.requests)
		}()
		if n <= count {
			return
		}
		select {
		case <-self.requested:
		case <-ctx.Done():
			t.Fatalf("no %s", description)
		}
	}
}

// The requests so far, then forgets them.
func (self *testingPostQuantumSupply) takeRequests() []testingPostQuantumSupplyRequest {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	requests := self.requests
	self.requests = nil
	return requests
}

func (self *testingPostQuantumSupply) install(t *testing.T, api *Api) {
	t.Helper()
	api.setHttpPostRaw(func(ctx context.Context, requestUrl string, body []byte, _ string) ([]byte, error) {
		if !strings.HasSuffix(requestUrl, "/network/find-providers2") {
			return nil, fmt.Errorf("unexpected request %s", filepath.Base(requestUrl))
		}
		var args struct {
			Specs []struct {
				LocationId      string `json:"location_id"`
				LocationGroupId string `json:"location_group_id"`
			} `json:"specs"`
			RequireModernProvider bool `json:"require_modern_provider"`
		}
		if err := json.Unmarshal(body, &args); err != nil {
			return nil, err
		}
		request := testingPostQuantumSupplyRequest{requireModernProvider: args.RequireModernProvider, time: time.Now()}
		if len(args.Specs) == 1 {
			request.locationId = args.Specs[0].LocationId + args.Specs[0].LocationGroupId
		}
		hold := func() chan struct{} {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.requests = append(self.requests, request)
			return self.hold
		}()
		select {
		case self.requested <- struct{}{}:
		default:
		}
		if hold != nil {
			select {
			case <-hold:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		count, err := func() (int, error) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if args.RequireModernProvider {
				return self.modernCount, self.err
			}
			return self.count, self.err
		}()
		if err != nil {
			return nil, err
		}
		providers := make([]map[string]any, 0, count)
		for range count {
			providers = append(providers, map[string]any{"client_id": NewId().String()})
		}
		return json.Marshal(map[string]any{"providers": providers})
	})
}

// Hears every change of a device's notice, in order.
type testingPostQuantumUnavailableChangeListener struct {
	notices chan *PostQuantumUnavailable
}

func newTestingPostQuantumUnavailableChangeListener() *testingPostQuantumUnavailableChangeListener {
	return &testingPostQuantumUnavailableChangeListener{
		notices: make(chan *PostQuantumUnavailable, 64),
	}
}

func (self *testingPostQuantumUnavailableChangeListener) PostQuantumUnavailableChanged(postQuantumUnavailable *PostQuantumUnavailable) {
	self.notices <- postQuantumUnavailable
}

// The next notice the listener hears, or fails.
func (self *testingPostQuantumUnavailableChangeListener) next(t *testing.T, ctx context.Context, description string) *PostQuantumUnavailable {
	t.Helper()
	select {
	case postQuantumUnavailable := <-self.notices:
		return postQuantumUnavailable
	case <-ctx.Done():
		t.Fatalf("no %s", description)
		return nil
	}
}

// A device of the preference fixture with the default profile (post-quantum
// on) and a window that discovers no providers, so a selected location stays
// connecting. Its notice's checks go to the returned supply, and checked is
// signaled after each check answers. configure adjusts the settings first.
func newTestingPostQuantumNoticeDevice(
	t *testing.T,
	configure func(settings *DeviceLocalSettings),
) (*DeviceLocal, *testingPostQuantumSupply, chan struct{}) {
	t.Helper()
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	auth, err := fixture.networkSpace.GetAuthStateSnapshot()
	if err != nil || auth.GetByClientJwt() == "" || auth.GetInstanceId() == nil {
		t.Fatal("accepted stored provider identity is unavailable")
	}
	checked := make(chan struct{}, 1024)
	settings := DefaultDeviceLocalSettings()
	settings.AllowProvider = false
	settings.EnableRpc = false
	settings.DisableLogging = true
	settings.GeneratorFunc = func([]*connect.ProviderSpec) connect.MultiClientGenerator {
		return &testingDnsOwnerGenerator{}
	}
	settings.testingPostQuantumSupplyChecked = func() {
		select {
		case checked <- struct{}{}:
		default:
		}
	}
	if configure != nil {
		configure(settings)
	}
	device, err := newDeviceLocalWithOverrides(fixture.networkSpace, auth.GetByClientJwt(),
		"post-quantum-notice", "test", "0", auth.GetInstanceId(), settings, connect.NewId())
	if err != nil {
		t.Fatal(err)
	}
	device.SetUpgradeMuxSettings(nil)
	t.Cleanup(func() { testingJoinPreferenceDevice(t, device) })
	supply := newTestingPostQuantumSupply(0, 3)
	supply.install(t, device.GetApi())
	return device, supply, checked
}

// A location target, as the location list offers one.
func testingSupplyLocation() *ConnectLocation {
	return &ConnectLocation{
		ConnectLocationId: &ConnectLocationId{LocationId: NewId()},
		Name:              "Synthetic country",
		LocationType:      LocationTypeCountry,
	}
}

// Fails unless the notice is for the location.
func assertPostQuantumUnavailableFor(t *testing.T, postQuantumUnavailable *PostQuantumUnavailable, location *ConnectLocation, description string) {
	t.Helper()
	if postQuantumUnavailable == nil || postQuantumUnavailable.Location == nil ||
		!connectLocationTransportEqual(postQuantumUnavailable.Location, location) {
		t.Fatalf("%s: the notice is %+v, want one for %s", description, postQuantumUnavailable, location.Name)
	}
}

// Waits for a check to answer, or fails.
func testingAwaitSignal(t *testing.T, ctx context.Context, signal chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatalf("no %s", description)
	}
}

// With no connect view controller open, a device whose selected location has
// no post-quantum exits, but has others, sets the notice for that location by
// itself, from a request with the requirement and one without, and a recheck
// clears it when post-quantum exits return. Before, only the connect view
// controller decided, so a device without one never had a notice.
func TestDeviceLocalNoticesALocationWithoutPostQuantumExitsWithNoController(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	device, supply, _ := newTestingPostQuantumNoticeDevice(t, func(settings *DeviceLocalSettings) {
		settings.postQuantumSupply.RecheckInterval = time.Millisecond
	})
	listener := newTestingPostQuantumUnavailableChangeListener()
	sub := device.AddPostQuantumUnavailableChangeListener(listener)
	defer sub.Close()

	location := testingSupplyLocation()
	device.SetConnectLocation(location)
	notice := listener.next(t, ctx, "notice for a location without post-quantum exits")
	assertPostQuantumUnavailableFor(t, notice, location, "the device's listener")
	assertPostQuantumUnavailableFor(t, device.GetPostQuantumUnavailable(), location, "the device's getter")
	if status := device.GetWindowStatus(); status.MinSatisfied {
		t.Fatal("a location without post-quantum exits reads connected")
	}
	// the post-quantum request found none, so the check asked without it
	requests := supply.takeRequests()
	if len(requests) < 2 || !requests[0].requireModernProvider || requests[1].requireModernProvider {
		t.Fatalf("the check asked %+v, want with and then without the requirement", requests)
	}
	if requests[0].locationId != location.ConnectLocationId.LocationId.String() {
		t.Fatalf("the check asked about %s, want the selected location %s", requests[0].locationId, location.ConnectLocationId.LocationId)
	}
	// the device keeps the user's choices: the location and the setting
	if !connectLocationTransportEqual(device.GetConnectLocation(), location) {
		t.Fatal("the notice changed the selected location")
	}
	assertPostQuantumEncryption(t, device.GetPerformanceProfile(), true, "the device with the notice")

	supply.set(2, 3)
	if notice := listener.next(t, ctx, "the recheck to clear the notice"); notice != nil {
		t.Fatalf("after post-quantum exits returned the notice is %+v", notice)
	}
	if device.GetPostQuantumUnavailable() != nil {
		t.Fatal("the getter still has the notice after post-quantum exits returned")
	}
}

// Selecting another location clears the notice at once, before the new
// location's check answers, and the new location is checked on its own.
func TestDeviceLocalPostQuantumNoticeClearsWhenTheLocationChanges(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	device, supply, checked := newTestingPostQuantumNoticeDevice(t, func(settings *DeviceLocalSettings) {
		settings.postQuantumSupply.RecheckInterval = time.Hour
	})
	listener := newTestingPostQuantumUnavailableChangeListener()
	sub := device.AddPostQuantumUnavailableChangeListener(listener)
	defer sub.Close()

	first := testingSupplyLocation()
	device.SetConnectLocation(first)
	assertPostQuantumUnavailableFor(t, listener.next(t, ctx, "notice for the first location"), first, "the first location")
	testingAwaitSignal(t, ctx, checked, "the first check")
	supply.takeRequests()

	// the second location's check is held, so only the change can clear
	release := supply.holdRequests()
	defer release()
	second := testingSupplyLocation()
	device.SetConnectLocation(second)
	if notice := listener.next(t, ctx, "the notice to clear on the location change"); notice != nil {
		t.Fatalf("after the location changed the notice is %+v", notice)
	}
	supply.awaitRequests(t, ctx, 1, "a check of the second location")
	requests := supply.takeRequests()
	if len(requests) != 1 || requests[0].locationId != second.ConnectLocationId.LocationId.String() {
		t.Fatalf("the requests after the change were %+v, want one about the second location", requests)
	}
	release()
	assertPostQuantumUnavailableFor(t, listener.next(t, ctx, "notice for the second location"), second, "the second location")
}

// Turning post-quantum encryption off, the notice's second action, clears it
// at once and stops the checks: the location is not asked about again.
func TestDeviceLocalPostQuantumNoticeClearsWhenPostQuantumTurnsOff(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	device, supply, checked := newTestingPostQuantumNoticeDevice(t, func(settings *DeviceLocalSettings) {
		settings.postQuantumSupply.RecheckInterval = time.Hour
	})
	listener := newTestingPostQuantumUnavailableChangeListener()
	sub := device.AddPostQuantumUnavailableChangeListener(listener)
	defer sub.Close()

	location := testingSupplyLocation()
	device.SetConnectLocation(location)
	assertPostQuantumUnavailableFor(t, listener.next(t, ctx, "notice"), location, "post-quantum on")
	testingAwaitSignal(t, ctx, checked, "the first check")
	supply.takeRequests()

	device.SetPerformanceProfile(&PerformanceProfile{WindowType: WindowTypeAuto})
	if notice := listener.next(t, ctx, "the notice to clear once post-quantum is off"); notice != nil {
		t.Fatalf("after post-quantum was turned off the notice is %+v", notice)
	}
	// another selection with post-quantum off makes no request: the next
	// request is the one after post-quantum is back on, about that selection
	other := testingSupplyLocation()
	device.SetConnectLocation(other)
	device.SetPerformanceProfile(DefaultPerformanceProfile())
	assertPostQuantumUnavailableFor(t, listener.next(t, ctx, "notice once post-quantum is back on"), other, "post-quantum back on")
	requests := supply.takeRequests()
	if len(requests) != 2 || requests[0].locationId != other.ConnectLocationId.LocationId.String() {
		t.Fatalf("the requests after post-quantum was off were %+v, want only the check once it was back on", requests)
	}
}

// A connected connection clears the notice and is not checked; leaving
// connected checks again, spaced by MinCheckInterval from the last check of
// the same location.
func TestDeviceLocalPostQuantumNoticeClearsWhenConnected(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var windowStatus atomic.Pointer[WindowStatus]
	windowStatus.Store(&WindowStatus{})
	device, supply, checked := newTestingPostQuantumNoticeDevice(t, func(settings *DeviceLocalSettings) {
		settings.postQuantumSupply.RecheckInterval = time.Hour
		settings.postQuantumSupply.MinCheckInterval = 0
		settings.testingPostQuantumWindowStatus = windowStatus.Load
	})
	listener := newTestingPostQuantumUnavailableChangeListener()
	sub := device.AddPostQuantumUnavailableChangeListener(listener)
	defer sub.Close()

	location := testingSupplyLocation()
	device.SetConnectLocation(location)
	assertPostQuantumUnavailableFor(t, listener.next(t, ctx, "notice"), location, "not connected")
	testingAwaitSignal(t, ctx, checked, "the first check")
	supply.takeRequests()

	// the window reaches its minimum, as the device's window status says
	connected := &WindowStatus{MinSatisfied: true, ProviderStateAdded: 4}
	windowStatus.Store(connected)
	device.windowStatusChanged(connected)
	if notice := listener.next(t, ctx, "the notice to clear once connected"); notice != nil {
		t.Fatalf("over a connected connection the notice is %+v", notice)
	}
	// more window changes while connected check nothing: the next request is
	// the one after the connection drops
	device.windowStatusChanged(connected)
	device.windowStatusChanged(connected)
	notConnected := &WindowStatus{ProviderStateAdded: 1}
	windowStatus.Store(notConnected)
	device.windowStatusChanged(notConnected)
	assertPostQuantumUnavailableFor(t, listener.next(t, ctx, "notice after the connection dropped"), location, "dropped")
	if requests := supply.takeRequests(); len(requests) != 2 {
		t.Fatalf("the requests over and after the connection were %+v, want one check after it dropped", requests)
	}
}

// A connection that drops in and out of connected checks its location at
// most once per MinCheckInterval: the drops right after a check wait out the
// interval, then the location is checked once, however many drops there were.
// The interval is a lower bound on the second check's start, so the test
// cannot fail on a slow machine.
func TestDeviceLocalPostQuantumNoticeSpacesChecksOfAFlappingConnection(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	minCheckInterval := 500 * time.Millisecond
	var windowStatus atomic.Pointer[WindowStatus]
	windowStatus.Store(&WindowStatus{})
	device, supply, checked := newTestingPostQuantumNoticeDevice(t, func(settings *DeviceLocalSettings) {
		settings.postQuantumSupply.RecheckInterval = time.Hour
		settings.postQuantumSupply.MinCheckInterval = minCheckInterval
		settings.testingPostQuantumWindowStatus = windowStatus.Load
	})
	listener := newTestingPostQuantumUnavailableChangeListener()
	sub := device.AddPostQuantumUnavailableChangeListener(listener)
	defer sub.Close()

	location := testingSupplyLocation()
	device.SetConnectLocation(location)
	assertPostQuantumUnavailableFor(t, listener.next(t, ctx, "notice"), location, "not connected")
	testingAwaitSignal(t, ctx, checked, "the first check")
	firstCheckTime := supply.takeRequests()[0].time

	connected := &WindowStatus{MinSatisfied: true, ProviderStateAdded: 4}
	notConnected := &WindowStatus{ProviderStateAdded: 1}
	windowStatus.Store(connected)
	device.windowStatusChanged(connected)
	if notice := listener.next(t, ctx, "the notice to clear once connected"); notice != nil {
		t.Fatalf("over a connected connection the notice is %+v", notice)
	}
	for range 3 {
		windowStatus.Store(notConnected)
		device.windowStatusChanged(notConnected)
		windowStatus.Store(connected)
		device.windowStatusChanged(connected)
	}
	windowStatus.Store(notConnected)
	device.windowStatusChanged(notConnected)

	assertPostQuantumUnavailableFor(t, listener.next(t, ctx, "notice after the drops"), location, "after the drops")
	requests := supply.takeRequests()
	if len(requests) != 2 {
		t.Fatalf("the drops asked %+v, want one check", requests)
	}
	if spacing := requests[0].time.Sub(firstCheckTime); spacing < minCheckInterval {
		t.Fatalf("the location was checked again %s after its last check, want at least %s", spacing, minCheckInterval)
	}
}

// Any error is no notice: a failing check gives none, and a recheck that
// fails clears a standing one.
func TestDeviceLocalPostQuantumNoticeIsNoneWhenTheCheckFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	device, supply, checked := newTestingPostQuantumNoticeDevice(t, func(settings *DeviceLocalSettings) {
		settings.postQuantumSupply.RecheckInterval = time.Millisecond
	})
	listener := newTestingPostQuantumUnavailableChangeListener()
	sub := device.AddPostQuantumUnavailableChangeListener(listener)
	defer sub.Close()

	supply.setErr(errors.New("synthetic discovery failure"))
	location := testingSupplyLocation()
	device.SetConnectLocation(location)
	for range 3 {
		testingAwaitSignal(t, ctx, checked, "a failing check")
	}
	if notice := device.GetPostQuantumUnavailable(); notice != nil {
		t.Fatalf("failing checks gave the notice %+v", notice)
	}

	supply.set(0, 3)
	assertPostQuantumUnavailableFor(t, listener.next(t, ctx, "notice once the checks answer"), location, "answering checks")
	supply.setErr(errors.New("synthetic discovery failure"))
	if notice := listener.next(t, ctx, "a failing recheck to clear the notice"); notice != nil {
		t.Fatalf("after a failing recheck the notice is %+v", notice)
	}
}

// The check never holds up the connect path: while discovery for the notice
// is stalled, selecting a location returns, the device installs it and builds
// its window, a second selection replaces it, and the device still closes, its
// check canceled. Discovery that errors leaves the device's location and
// setting as they were, with no notice.
func TestDeviceLocalPostQuantumCheckLeavesTheConnectPathAlone(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	device, supply, checked := newTestingPostQuantumNoticeDevice(t, func(settings *DeviceLocalSettings) {
		settings.postQuantumSupply.RecheckInterval = time.Hour
		// longer than the test, so only cancellation ends the stalled check
		settings.postQuantumSupply.CheckTimeout = time.Hour
	})
	windowStatuses := make(chan *WindowStatus, 64)
	windowSub := device.AddWindowStatusChangeListener(windowStatusListenerFunc(func(windowStatus *WindowStatus) {
		select {
		case windowStatuses <- windowStatus:
		default:
		}
	}))
	defer windowSub.Close()

	release := supply.holdRequests()
	defer release()
	first := testingSupplyLocation()
	selected := make(chan struct{})
	go func() {
		defer close(selected)
		device.SetConnectLocation(first)
	}()
	supply.awaitRequests(t, ctx, 1, "the stalled check of the first location")
	testingAwaitSignal(t, ctx, selected, "the selection to return while its check is stalled")
	if !connectLocationTransportEqual(device.GetConnectLocation(), first) {
		t.Fatal("the stalled check held back the selection")
	}
	generation := device.GetWindowStatus().ConnectionGeneration

	second := testingSupplyLocation()
	device.SetConnectLocation(second)
	for {
		var windowStatus *WindowStatus
		select {
		case windowStatus = <-windowStatuses:
		case <-ctx.Done():
			t.Fatal("no window of the second selection while the check was stalled")
		}
		if generation < windowStatus.ConnectionGeneration {
			break
		}
	}
	if device.GetPostQuantumUnavailable() != nil {
		t.Fatal("a stalled check gave a notice")
	}

	// discovery that errors changes nothing on the device: the second
	// location's stalled check answers with an error
	supply.setErr(errors.New("synthetic discovery failure"))
	release()
	testingAwaitSignal(t, ctx, checked, "the second location's failing check")
	if device.GetPostQuantumUnavailable() != nil {
		t.Fatal("a failing check gave a notice")
	}
	if !connectLocationTransportEqual(device.GetConnectLocation(), second) {
		t.Fatal("a failing check changed the selected location")
	}
	assertPostQuantumEncryption(t, device.GetPerformanceProfile(), true, "the device after a failing check")

	// a stalled check is canceled by the device's close, which joins it
	supply.takeRequests()
	supply.holdRequests()
	third := testingSupplyLocation()
	device.SetConnectLocation(third)
	supply.awaitRequests(t, ctx, 1, "the stalled check of the third location")
	closeCtx, closeCancel := context.WithTimeout(ctx, 10*time.Second)
	defer closeCancel()
	if err := device.CloseAndWait(closeCtx); err != nil {
		t.Fatalf("the device did not close while its check was stalled: %v", err)
	}
}

// A WindowStatusChangeListener from a function.
type windowStatusListenerFunc func(windowStatus *WindowStatus)

func (self windowStatusListenerFunc) WindowStatusChanged(windowStatus *WindowStatus) {
	self(windowStatus)
}

// The notice crosses the device rpc: a remote's listener and getter report
// the notice the device process decides for the location the remote selected,
// its clear when the remote turns post-quantum off, and the getter keeps the
// last notice while the device process is gone. Before, a remote had no
// notice to read, so an app whose connect screen was closed could only say
// connecting.
func TestDeviceRemoteMirrorsThePostQuantumNotice(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	strategySettings := testOfflineClientStrategySettings()
	strategySettings.Log = connect.NewNoopLogger()
	networkSpace := NewNetworkSpaceWithUrls(ctx, "https://api.post-quantum.example", "wss://platform.post-quantum.example", strategySettings)
	defer networkSpace.Close()
	deviceLocal, deviceRemote := testExtenderStatusSyncedDeviceLocalRemoteWithSettings(
		t,
		networkSpace,
		func(settings *DeviceLocalSettings) {
			settings.AllowProvider = false
			settings.GeneratorFunc = func([]*connect.ProviderSpec) connect.MultiClientGenerator {
				return &testingDnsOwnerGenerator{}
			}
			settings.postQuantumSupply.RecheckInterval = time.Hour
		},
	)
	supply := newTestingPostQuantumSupply(0, 3)
	supply.install(t, deviceLocal.GetApi())
	listener := newTestingPostQuantumUnavailableChangeListener()
	sub := deviceRemote.AddPostQuantumUnavailableChangeListener(listener)
	defer sub.Close()
	if notice := deviceRemote.GetPostQuantumUnavailable(); notice != nil {
		t.Fatalf("a remote with nothing selected reads the notice %+v", notice)
	}

	// the app selects through the remote; the device process decides
	location := testingSupplyLocation()
	deviceRemote.SetConnectLocation(location)
	var notice *PostQuantumUnavailable
	for notice == nil {
		notice = listener.next(t, ctx, "the device process's notice on the remote")
	}
	assertPostQuantumUnavailableFor(t, notice, location, "the remote's listener")
	assertPostQuantumUnavailableFor(t, deviceLocal.GetPostQuantumUnavailable(), location, "the device process")
	assertPostQuantumUnavailableFor(t, deviceRemote.GetPostQuantumUnavailable(), location, "the remote's getter")

	// the notice's second action, from the app
	deviceRemote.SetPerformanceProfile(&PerformanceProfile{WindowType: WindowTypeAuto})
	for notice != nil {
		notice = listener.next(t, ctx, "the clear on the remote once post-quantum is off")
	}
	if notice := deviceRemote.GetPostQuantumUnavailable(); notice != nil {
		t.Fatalf("the remote's getter reads %+v once post-quantum is off", notice)
	}

	deviceRemote.SetPerformanceProfile(DefaultPerformanceProfile())
	for notice == nil {
		notice = listener.next(t, ctx, "the notice on the remote once post-quantum is back on")
	}
	assertPostQuantumUnavailableFor(t, notice, location, "post-quantum back on")

	deviceLocal.Close()
	testWaitRemoteDisconnected(t, deviceRemote)
	assertPostQuantumUnavailableFor(t, deviceRemote.GetPostQuantumUnavailable(), location, "the remote's getter while the device process is gone")
}
