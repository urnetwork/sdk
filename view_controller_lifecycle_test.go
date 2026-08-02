package sdk

import (
	"context"
	"testing"
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
