//go:build !ios_extension

package sdk

// The connect screen's notice that the selected location has no post-quantum
// exits right now (owner, 2026-11-24: "Tell them, let them choose"). The
// device decides it (device_local_post_quantum_test.go pins that); these pin
// that the connect view controller shows the device's notice, through the
// real device and controller and the find-providers2 requests as the platform
// receives them: a selected location with no post-quantum exits gives the
// notice instead of a connection that stays connecting, and it clears when
// exits return; turning post-quantum off, the notice's second action, clears
// it, discovers without the requirement and is saved; a location with
// post-quantum exits connects as before, with no notice; and a controller
// opened later shows the device's standing notice at once, making no check of
// its own.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// Signals each change of the controller's notice.
type testingPostQuantumUnavailableListener struct {
	changed chan struct{}
}

func (self *testingPostQuantumUnavailableListener) PostQuantumUnavailableChanged() {
	select {
	case self.changed <- struct{}{}:
	default:
	}
}

// Opens a connect view controller of the device, with its notice listener.
func newTestingPostQuantumSupplyController(t *testing.T, device *DeviceLocal) (*ConnectViewController, *testingPostQuantumUnavailableListener) {
	t.Helper()
	vc := device.OpenConnectViewController()
	t.Cleanup(func() { device.CloseConnectViewController(vc) })
	listener := &testingPostQuantumUnavailableListener{changed: make(chan struct{}, 16)}
	sub := vc.AddPostQuantumUnavailableListener(listener)
	t.Cleanup(sub.Close)
	return vc, listener
}

// A selected location with no post-quantum exits, but with exits without
// post-quantum encryption, gives the notice for that location while the
// connection is not connected, instead of a connection that only stays
// connecting. When post-quantum exits return, a recheck clears it.
func TestConnectViewControllerNoticesALocationWithoutPostQuantumExits(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	device, supply, _ := newTestingPostQuantumNoticeDevice(t, func(settings *DeviceLocalSettings) {
		settings.postQuantumSupply.RecheckInterval = time.Millisecond
	})
	vc, listener := newTestingPostQuantumSupplyController(t, device)

	location := testingSupplyLocation()
	vc.Connect(location)
	for vc.GetPostQuantumUnavailable() == nil {
		testingAwaitSignal(t, ctx, listener.changed, "notice for a location without post-quantum exits")
	}
	assertPostQuantumUnavailableFor(t, vc.GetPostQuantumUnavailable(), location, "the controller")
	if status := vc.GetConnectionStatus(); status == Connected {
		t.Fatal("a location without post-quantum exits reads connected")
	}
	// the post-quantum request found none, so the check asked without it
	if requests := supply.takeRequests(); len(requests) < 2 || !requests[0].requireModernProvider || requests[1].requireModernProvider {
		t.Fatalf("the check asked %+v, want with and then without the requirement", requests)
	}

	supply.set(2, 3)
	for vc.GetPostQuantumUnavailable() != nil {
		testingAwaitSignal(t, ctx, listener.changed, "the notice to clear after post-quantum exits returned")
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
	settings.postQuantumSupply.RecheckInterval = time.Millisecond
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
	supply := newTestingPostQuantumSupply(0, 3)
	supply.install(t, device.GetApi())
	vc, listener := newTestingPostQuantumSupplyController(t, device)

	vc.Connect(testingSupplyLocation())
	for vc.GetPostQuantumUnavailable() == nil {
		testingAwaitSignal(t, ctx, listener.changed, "notice for a location without post-quantum exits")
	}
	for _, body := range discovery.windowDiscovered(t, ctx) {
		assertModernDiscoveryRequest(t, body, "a post-quantum window")
	}

	device.SetPerformanceProfile(&PerformanceProfile{WindowType: WindowTypeAuto})
	if vc.GetPostQuantumUnavailable() != nil {
		t.Fatal("the controller still shows the notice once post-quantum is off")
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

	device, supply, checked := newTestingPostQuantumNoticeDevice(t, nil)
	supply.set(2, 3)
	vc, _ := newTestingPostQuantumSupplyController(t, device)

	vc.Connect(testingSupplyLocation())
	testingAwaitSignal(t, ctx, checked, "a check of a location with post-quantum exits")
	if notice := vc.GetPostQuantumUnavailable(); notice != nil {
		t.Fatalf("a location with post-quantum exits has the notice %+v", notice)
	}
	if requests := supply.takeRequests(); len(requests) != 1 || !requests[0].requireModernProvider {
		t.Fatalf("the check asked %+v, want only the post-quantum request", requests)
	}
}

// A controller opened over a device that already has the notice shows it at
// once, with no check of its own: one source of truth, the device's. Choosing
// another location in the controller hides it at once, before the device's
// own clear arrives. Before, a controller decided on its own, with its own
// requests, and showed nothing until they answered.
func TestConnectViewControllerShowsTheDevicesNotice(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	device, supply, checked := newTestingPostQuantumNoticeDevice(t, func(settings *DeviceLocalSettings) {
		settings.postQuantumSupply.RecheckInterval = time.Hour
	})
	deviceListener := newTestingPostQuantumUnavailableChangeListener()
	deviceSub := device.AddPostQuantumUnavailableChangeListener(deviceListener)
	defer deviceSub.Close()

	location := testingSupplyLocation()
	device.SetConnectLocation(location)
	assertPostQuantumUnavailableFor(t, deviceListener.next(t, ctx, "the device's notice"), location, "the device")
	testingAwaitSignal(t, ctx, checked, "the device's check")
	supply.takeRequests()

	vc, _ := newTestingPostQuantumSupplyController(t, device)
	assertPostQuantumUnavailableFor(t, vc.GetPostQuantumUnavailable(), location, "a controller opened over the notice")

	// another selection hides it before the device clears its own; the device's
	// check of the new location is the only request
	release := supply.holdRequests()
	defer release()
	other := testingSupplyLocation()
	vc.Connect(other)
	if notice := vc.GetPostQuantumUnavailable(); notice != nil {
		t.Fatalf("after choosing another location the controller shows %+v", notice)
	}
	supply.awaitRequests(t, ctx, 1, "the device's check of the new location")
	if requests := supply.takeRequests(); len(requests) != 1 || requests[0].locationId != other.ConnectLocationId.LocationId.String() {
		t.Fatalf("the requests after the controller opened were %+v, want only the device's check of the new location", requests)
	}
}
