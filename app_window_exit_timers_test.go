//go:build !ios

// The exit timers of an app's windows (app_window_exit_timers.go), against
// the owner's iOS logs of 2026-11-13: 15 exits that never acked held their
// flows for the whole 30 s send-ack lifetime, and four exits held a hole in
// their return stream for the whole 60 s gap timeout. Connect pins what the
// timers do (TestNeverAckingExitIsEvictedAtTheWindowAckTimeoutAndItsFlowsRehome,
// TestGapWedgedExitIsEvictedAtTheGapTimeoutAndItsFlowsRehome); these pin the
// values every app platform builds its windows with, and that a hosted device
// keeps connect's.
package sdk

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The platforms whose apps build DeviceLocal windows.
var appWindowExitTimersTestPlatforms = []string{"ios", "android", "darwin", "windows", "linux"}

// Root cause 1. Every app platform built its windows with connect's 30 s
// send-ack lifetime, so a never-acking exit held its flows for 30 s. Each
// now builds them with the app's 12 s.
func TestAppWindowsRunTheShortSendAckLifetime(t *testing.T) {
	for _, platform := range appWindowExitTimersTestPlatforms {
		settings := connect.DefaultMultiClientSettings()
		applyDeviceLocalWindowClientSettings(settings, defaultDeviceLocalSettingsForPlatform(platform))
		if settings.AckTimeout != 12*time.Second {
			t.Errorf("%s windows run a %s send-ack lifetime, want 12s", platform, settings.AckTimeout)
		}
	}
}

// Guard: a hosted device (the proxy service) keeps connect's send-ack
// lifetime. Passes before and after the change.
func TestHostedWindowsKeepTheConnectSendAckLifetime(t *testing.T) {
	hosted := DefaultLowMemoryDeviceLocalSettings()
	hosted.HostedIncompatible = true
	settings := connect.DefaultMultiClientSettings()
	applyDeviceLocalWindowClientSettings(settings, hosted)
	connect.AssertEqual(t, settings.AckTimeout, connect.DefaultMultiClientSettings().AckTimeout)
}

// The receive gap timeout of the window clients of a connected device, as its
// window generator builds them.
func appWindowExitTimersTestGapTimeout(t *testing.T, hosted bool) (window time.Duration, provider time.Duration) {
	t.Helper()
	fixture := testingAuthClientShapeSpace(t)
	settings := DefaultLowMemoryDeviceLocalSettings()
	settings.AllowProvider = false
	settings.DisableLogging = true
	settings.ProviderDiscovery = newHostedWindowSelectorsTestDiscovery()
	settings.HostedIncompatible = hosted
	device := newSettingsOwnershipDevice(t, fixture, settings, true)
	location := &ConnectLocation{ConnectLocationId: &ConnectLocationId{BestAvailable: true}}
	if err := device.SetConnectLocationChecked(location); err != nil {
		t.Fatal(err)
	}
	requireLimitedWindowTestMulti(t, device)
	var generator *connect.ApiMultiClientGenerator
	func() {
		device.stateLock.Lock()
		defer device.stateLock.Unlock()
		generator = device.apiMultiClientGenerator
	}()
	if generator == nil {
		t.Fatal("the device built no window generator")
	}
	return generator.NewClientSettings().ReceiveBufferSettings.GapTimeout,
		device.settings.ClientSettings.ReceiveBufferSettings.GapTimeout
}

// Root cause 2. An app's window clients held a hole in an exit's return
// stream for connect's 60 s gap timeout. They now give up at 15 s, which
// evicts the exit; the device's own provider client keeps connect's.
func TestAppWindowClientsRunTheShortGapTimeout(t *testing.T) {
	window, provider := appWindowExitTimersTestGapTimeout(t, false)
	if window != 15*time.Second {
		t.Errorf("app window clients run a %s gap timeout, want 15s", window)
	}
	connect.AssertEqual(t, provider, connect.DefaultReceiveBufferSettings().GapTimeout)
}

// Guard: a hosted device's window clients keep connect's gap timeout. Passes
// before and after the change.
func TestHostedWindowClientsKeepTheConnectGapTimeout(t *testing.T) {
	window, _ := appWindowExitTimersTestGapTimeout(t, true)
	connect.AssertEqual(t, window, connect.DefaultReceiveBufferSettings().GapTimeout)
}
