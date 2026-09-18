package sdk

import (
	"context"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

func deviceRemoteLifecycleCounts(device *DeviceRemote) (connectLocationListeners int, windowMonitors int, activeWindowListeners int) {
	device.stateLock.Lock()
	defer device.stateLock.Unlock()

	for _, monitor := range device.windowMonitors {
		activeWindowListeners += len(monitor.listeners)
	}
	return len(device.connectLocationChangeListeners), len(device.windowMonitors), activeWindowListeners
}

func openedViewControllerCount(device *DeviceRemote) int {
	device.viewControllerManager.stateLock.Lock()
	defer device.viewControllerManager.stateLock.Unlock()
	return len(device.viewControllerManager.openedViewControllers)
}

// BrowserStateOnly deliberately keeps DeviceRemote.service nil so browser
// callbacks never make synchronous RPC calls on their own websocket event
// loop. A successful sync replays ConnectLocationChanged after the CVC has
// already created its logical window. Rebuilding that equivalent window
// removes and adds RPC listeners, and both operations request another sync;
// the next sync repeats the replay forever. Pin the exact live ordering while
// the grid is still DestinationSet/Connecting, not only after UI Connected.
func TestBrowserStateOnlyConnectLocationReplayRetainsActiveGrid(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings := defaultDeviceRpcSettings()
	settings.BrowserStateOnly = true
	settings.DisableLogging = true
	dialer := &testingBlockedDeviceRpcDialer{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	device, err := newDeviceRemoteWithOverrides(
		networkSpace,
		byJwt,
		NewId(),
		settings,
		connect.NewId(),
		dialer,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(device.Close)

	location := &ConnectLocation{
		ConnectLocationId: &ConnectLocationId{BestAvailable: true},
		Name:              "initial snapshot",
		ProviderCount:     1,
	}
	device.stateLock.Lock()
	device.lastKnownState.Location.Set(newDeviceRemoteConnectLocation(location))
	device.stateLock.Unlock()

	controller := device.OpenConnectViewController()
	if controller == nil || controller.GetGrid() == nil {
		t.Fatal("browser CVC did not create its initial logical window")
	}
	if controller.GetConnectionStatus() == Connected {
		t.Fatal("fixture skipped the initial-sync DestinationSet/Connecting ordering")
	}
	initialGrid := controller.GetGrid()
	initialLocationListeners, initialMonitors, initialWindowListeners := deviceRemoteLifecycleCounts(device)

	// Arm after initial controller/window registration. Any close/add churn in
	// the replay below closes this channel synchronously through Sync().
	unexpectedResync := device.reconnectMonitor.NotifyChannel()
	replayed := cloneConnectLocation(location)
	replayed.Name = "same transport, refreshed description"
	replayed.ProviderCount = 7
	device.connectLocationChanged(newDeviceRemoteConnectLocation(replayed))

	if got := controller.GetGrid(); got != initialGrid {
		t.Fatal("equivalent browser sync replay replaced the active logical window")
	}
	if got := controller.GetSelectedLocation(); got == nil || got.Name != replayed.Name {
		t.Fatalf("selected location did not retain replayed metadata: %+v", got)
	}
	locationListeners, monitors, windowListeners := deviceRemoteLifecycleCounts(device)
	if locationListeners != initialLocationListeners ||
		monitors != initialMonitors ||
		windowListeners != initialWindowListeners {
		t.Fatalf(
			"equivalent replay changed listener ownership: location/window/active=%d/%d/%d, want %d/%d/%d",
			locationListeners,
			monitors,
			windowListeners,
			initialLocationListeners,
			initialMonitors,
			initialWindowListeners,
		)
	}
	select {
	case <-unexpectedResync:
		t.Fatal("equivalent browser sync replay requested another device-RPC sync")
	default:
	}

	// An explicit reconnect advances the CVC generation before the same
	// location is echoed back. That makes the retained grid stale by design:
	// rebuild and register a fresh window rather than suppressing the gesture.
	controller.beginGeneration(DestinationSet)
	explicitReconnectSync := device.reconnectMonitor.NotifyChannel()
	device.connectLocationChanged(newDeviceRemoteConnectLocation(replayed))
	if got := controller.GetGrid(); got == nil || got == initialGrid {
		t.Fatal("explicit same-location reconnect retained its stale grid generation")
	}
	select {
	case <-explicitReconnectSync:
	default:
		t.Fatal("explicit same-location reconnect did not register its fresh remote window")
	}

	currentGrid := controller.GetGrid()
	changedLocationSync := device.reconnectMonitor.NotifyChannel()
	changedLocation := &ConnectLocation{
		ConnectLocationId: &ConnectLocationId{ClientId: NewId()},
		Name:              "different transport",
	}
	device.connectLocationChanged(newDeviceRemoteConnectLocation(changedLocation))
	if got := controller.GetGrid(); got == nil || got == currentGrid {
		t.Fatal("different browser transport location retained the previous grid")
	}
	select {
	case <-changedLocationSync:
	default:
		t.Fatal("different browser transport location did not register a fresh remote window")
	}
}

// A direct DeviceLocal does not have DeviceLocalRpc's logical-window rebind
// layer. Its explicit same-location callback must therefore continue to
// replace the grid rather than inheriting BrowserStateOnly idempotence.
func TestDirectDeviceConnectLocationReplayReplacesGrid(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	device, _ := testing_newRpcDeviceLocal(t, ctx)
	controller := newConnectViewController(ctx, device)
	defer controller.Close()
	location := &ConnectLocation{
		ConnectLocationId: &ConnectLocationId{BestAvailable: true},
	}

	controller.ConnectLocationChanged(location)
	initialGrid := controller.GetGrid()
	controller.ConnectLocationChanged(cloneConnectLocation(location))
	if got := controller.GetGrid(); got == nil || got == initialGrid {
		t.Fatal("direct same-location reconnect did not replace its grid")
	}
}

func TestConnectViewControllerLifecycleIsCycleBounded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, device := testing_newSyncedDeviceLocalRemote(t, ctx)
	baselineLocationListeners, baselineMonitors, baselineActiveListeners := deviceRemoteLifecycleCounts(device)

	controller := device.OpenConnectViewController()
	if controller == nil {
		t.Fatal("OpenConnectViewController returned nil")
	}
	if got := openedViewControllerCount(device); got != 1 {
		t.Fatalf("opened controllers=%d, want 1", got)
	}

	location := &ConnectLocation{
		ConnectLocationId: &ConnectLocationId{BestAvailable: true},
	}

	for cycle := 0; cycle < 100; cycle++ {
		// Drive the controller state directly. The lifecycle behavior under test
		// is its ConnectGrid/window-monitor ownership; no provider dial is needed.
		controller.ConnectLocationChanged(location)
		locationListeners, monitors, activeListeners := deviceRemoteLifecycleCounts(device)
		if locationListeners != baselineLocationListeners+1 {
			t.Fatalf("cycle %d connected location listeners=%d, want %d", cycle, locationListeners, baselineLocationListeners+1)
		}
		if monitors != baselineMonitors+1 || activeListeners != baselineActiveListeners+1 {
			t.Fatalf(
				"cycle %d connected monitors/active=%d/%d, want %d/%d",
				cycle,
				monitors,
				activeListeners,
				baselineMonitors+1,
				baselineActiveListeners+1,
			)
		}

		controller.ConnectLocationChanged(nil)
		locationListeners, monitors, activeListeners = deviceRemoteLifecycleCounts(device)
		if locationListeners != baselineLocationListeners+1 {
			t.Fatalf("cycle %d disconnected location listeners=%d, want %d", cycle, locationListeners, baselineLocationListeners+1)
		}
		if monitors != baselineMonitors || activeListeners != baselineActiveListeners {
			t.Fatalf(
				"cycle %d disconnected monitors/active=%d/%d, want baseline %d/%d",
				cycle,
				monitors,
				activeListeners,
				baselineMonitors,
				baselineActiveListeners,
			)
		}
	}

	// Closing while connected must transitively close the current grid, not
	// leave its callback alive until a later disconnect.
	controller.ConnectLocationChanged(location)
	controller.Close()
	locationListeners, monitors, activeListeners := deviceRemoteLifecycleCounts(device)
	if locationListeners != baselineLocationListeners {
		t.Fatalf("direct close location listeners=%d, want baseline %d", locationListeners, baselineLocationListeners)
	}
	if monitors != baselineMonitors || activeListeners != baselineActiveListeners {
		t.Fatalf(
			"direct close monitors/active=%d/%d, want baseline %d/%d",
			monitors,
			activeListeners,
			baselineMonitors,
			baselineActiveListeners,
		)
	}

	// Direct Close is intentionally idempotent. The manager call removes its
	// ownership entry without reactivating any resources.
	device.CloseViewController(controller)
	if got := openedViewControllerCount(device); got != 0 {
		t.Fatalf("opened controllers after manager close=%d, want 0", got)
	}
}

func TestDeviceRemoteCloseClosesManagedViewControllers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, device := testing_newSyncedDeviceLocalRemote(t, ctx)
	for range 16 {
		if controller := device.OpenConnectViewController(); controller == nil {
			t.Fatal("OpenConnectViewController returned nil")
		}
	}
	if got := openedViewControllerCount(device); got != 16 {
		t.Fatalf("opened controllers=%d, want 16", got)
	}

	device.Close()

	if got := openedViewControllerCount(device); got != 0 {
		t.Fatalf("opened controllers after device close=%d, want 0", got)
	}
	_, monitors, activeListeners := deviceRemoteLifecycleCounts(device)
	if monitors != 0 || activeListeners != 0 {
		t.Fatalf("device close retained monitors/active=%d/%d", monitors, activeListeners)
	}
}
