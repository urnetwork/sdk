//go:build !ios_extension

package sdk

// The connect screen's notice that the selected location has no post-quantum
// exits right now (owner, 2026-11-24: "Tell them, let them choose"). These
// pin, through the real device and connect view controller and the
// find-providers2 requests as the platform receives them: a selected location
// with no post-quantum exits gives the notice instead of a connection that
// stays connecting, and it clears when exits return; turning post-quantum off,
// the notice's second action, clears it, discovers without the requirement
// and is saved; a location with post-quantum exits connects as before, with no
// notice.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Answers the controller's find-providers2 requests: modernCount providers
// for a request with require_modern_provider, count without it. Records each
// request's flag, in order.
type testingPostQuantumSupply struct {
	stateLock   sync.Mutex
	modernCount int
	count       int
	requests    []bool
}

func (self *testingPostQuantumSupply) set(modernCount int, count int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.modernCount = modernCount
	self.count = count
}

// The require_modern_provider flag of each request so far, then forgets them.
func (self *testingPostQuantumSupply) takeRequests() []bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	requests := self.requests
	self.requests = nil
	return requests
}

func (self *testingPostQuantumSupply) install(t *testing.T, api *Api) {
	t.Helper()
	api.setHttpPostRaw(func(_ context.Context, requestUrl string, body []byte, _ string) ([]byte, error) {
		if !strings.HasSuffix(requestUrl, "/network/find-providers2") {
			return nil, fmt.Errorf("unexpected request %s", filepath.Base(requestUrl))
		}
		fields := map[string]any{}
		if err := json.Unmarshal(body, &fields); err != nil {
			return nil, err
		}
		requireModernProvider, _ := fields["require_modern_provider"].(bool)
		count := func() int {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.requests = append(self.requests, requireModernProvider)
			if requireModernProvider {
				return self.modernCount
			}
			return self.count
		}()
		providers := make([]map[string]any, 0, count)
		for range count {
			providers = append(providers, map[string]any{"client_id": NewId().String()})
		}
		return json.Marshal(map[string]any{"providers": providers})
	})
}

// Signals each change of the notice.
type testingPostQuantumUnavailableListener struct {
	changed chan struct{}
}

func (self *testingPostQuantumUnavailableListener) PostQuantumUnavailableChanged() {
	select {
	case self.changed <- struct{}{}:
	default:
	}
}

// A connect view controller of a device with the default profile
// (post-quantum on) whose supply checks the fake answers, with its notice
// listener and a barrier after each check. Rechecks run at once, so a test
// drives them by changing the fake and waiting on the barrier.
func newTestingPostQuantumSupplyController(t *testing.T, device *DeviceLocal, supply *testingPostQuantumSupply) (*ConnectViewController, *testingPostQuantumUnavailableListener, chan struct{}) {
	t.Helper()
	supply.install(t, device.GetApi())
	vc := device.OpenConnectViewController()
	t.Cleanup(func() { device.CloseConnectViewController(vc) })
	checked := make(chan struct{}, 16)
	vc.postQuantumSupplySettings.RecheckInterval = time.Millisecond
	vc.testingPostQuantumSupplyChecked = func() {
		select {
		case checked <- struct{}{}:
		default:
		}
	}
	listener := &testingPostQuantumUnavailableListener{changed: make(chan struct{}, 16)}
	sub := vc.AddPostQuantumUnavailableListener(listener)
	t.Cleanup(sub.Close)
	return vc, listener, checked
}

// Waits on a signal or fails.
func testingAwaitSignal(t *testing.T, ctx context.Context, signal chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatalf("no %s", description)
	}
}

// A location target, as the location list offers one.
func testingSupplyLocation() *ConnectLocation {
	return &ConnectLocation{
		ConnectLocationId: &ConnectLocationId{LocationId: NewId()},
		Name:              "Synthetic country",
		LocationType:      LocationTypeCountry,
	}
}

// A selected location with no post-quantum exits, but with exits without
// post-quantum encryption, gives the notice for that location while the
// connection is not connected, instead of a connection that only stays
// connecting. When post-quantum exits return, a recheck clears it.
func TestConnectViewControllerNoticesALocationWithoutPostQuantumExits(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	supply := &testingPostQuantumSupply{modernCount: 0, count: 3}
	vc, listener, checked := newTestingPostQuantumSupplyController(t, device, supply)

	location := testingSupplyLocation()
	vc.Connect(location)
	testingAwaitSignal(t, ctx, listener.changed, "notice for a location without post-quantum exits")
	notice := vc.GetPostQuantumUnavailable()
	if notice == nil || notice.Location == nil || notice.Location.ConnectLocationId.LocationId.Cmp(location.ConnectLocationId.LocationId) != 0 {
		t.Fatalf("the notice is %+v, want the selected location", notice)
	}
	if status := vc.GetConnectionStatus(); status == Connected {
		t.Fatal("a location without post-quantum exits reads connected")
	}
	// the post-quantum request found none, so the check asked without it
	if requests := supply.takeRequests(); len(requests) < 2 || !requests[0] || requests[1] {
		t.Fatalf("the check asked %v, want with and then without the requirement", requests)
	}

	supply.set(2, 3)
	for vc.GetPostQuantumUnavailable() != nil {
		testingAwaitSignal(t, ctx, checked, "recheck after post-quantum exits returned")
	}
}

// Turning post-quantum encryption off, the notice's second action, as every
// app's switch does: the notice clears, the device's discovery asks without
// the requirement, and the choice is saved, so the next load keeps it off.
func TestConnectViewControllerTurningPostQuantumOffConnectsClassicallyAndPersists(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	directory := t.TempDir()
	manager, fixture := testingPreferenceSpaceAt(t, directory)
	fixture.seedDistinctLogin(t)
	auth, err := fixture.networkSpace.GetAuthStateSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	discovery := newTestingPostQuantumDiscovery()
	settings := DefaultDeviceLocalSettings()
	settings.AllowProvider = false
	settings.EnableRpc = false
	settings.DisableLogging = true
	settings.ProviderDiscovery = discovery
	device, err := newDeviceLocalWithOverrides(fixture.networkSpace, auth.GetByClientJwt(),
		"post-quantum-supply", "test", "0", auth.GetInstanceId(), settings, connect.NewId())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testingJoinPreferenceDevice(t, device) })
	upgrade := connect.DefaultUpgradeMuxSettings()
	upgrade.Dns = nil
	device.SetUpgradeMuxSettings(upgrade)
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	supply := &testingPostQuantumSupply{modernCount: 0, count: 3}
	vc, listener, _ := newTestingPostQuantumSupplyController(t, device, supply)

	vc.Connect(testingSupplyLocation())
	testingAwaitSignal(t, ctx, listener.changed, "notice for a location without post-quantum exits")
	if vc.GetPostQuantumUnavailable() == nil {
		t.Fatal("no notice for a location without post-quantum exits")
	}
	for _, body := range discovery.windowDiscovered(t, ctx) {
		assertModernDiscoveryRequest(t, body, "a post-quantum window")
	}

	device.SetPerformanceProfile(&PerformanceProfile{WindowType: WindowTypeAuto})
	for vc.GetPostQuantumUnavailable() != nil {
		testingAwaitSignal(t, ctx, listener.changed, "the notice to clear once post-quantum is off")
	}
	// the window the change rebuilt asks without the requirement
	for {
		bodies := discovery.windowDiscovered(t, ctx)
		if _, present := testingDiscoveryRequestFlag(t, bodies[len(bodies)-1], "require_modern_provider"); !present {
			assertCapabilityBlindDiscoveryRequest(t, bodies[len(bodies)-1], "the window after post-quantum was turned off")
			break
		}
	}

	path := filepath.Join(fixture.localState.localStorageDir, ".performance_profile")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if requested, _ := testingDiscoveryRequestFlag(t, string(data), "post_quantum_encryption"); requested {
		t.Fatalf("the saved profile keeps post-quantum on: %s", data)
	}
	testingJoinPreferenceDevice(t, device)
	manager.Close()
	_, fresh := testingPreferenceSpaceAt(t, directory)
	restored := testingPreferenceDevice(t, fresh)
	if _, err := restored.Load(); err != nil {
		t.Fatal(err)
	}
	assertPostQuantumEncryption(t, restored.GetPerformanceProfile(), false, "the device after turning post-quantum off from the notice")
}

// A location with post-quantum exits connects as before: the check finds them
// with its first request and no notice is given.
func TestConnectViewControllerLocationWithPostQuantumExitsHasNoNotice(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	supply := &testingPostQuantumSupply{modernCount: 2, count: 3}
	vc, _, checked := newTestingPostQuantumSupplyController(t, device, supply)

	vc.Connect(testingSupplyLocation())
	testingAwaitSignal(t, ctx, checked, "a check of a location with post-quantum exits")
	if notice := vc.GetPostQuantumUnavailable(); notice != nil {
		t.Fatalf("a location with post-quantum exits has the notice %+v", notice)
	}
	if requests := supply.takeRequests(); len(requests) != 1 || !requests[0] {
		t.Fatalf("the check asked %v, want only the post-quantum request", requests)
	}
}
