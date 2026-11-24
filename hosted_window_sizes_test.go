//go:build !ios

// The host's window sizes (DeviceLocalSettings.QualityWindowSize and
// SpeedWindowSize): a host may size a device's quality and speed windows in
// place of its memory profile. The proxy service sizes the hosted devices of
// networks on the free plan this way; apps set neither and keep their
// profile's sizes.
package sdk

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The sizes the proxy service gives a free hosted device: two quality exits
// and one speed exit.
func hostedWindowSizesTestQuality() connect.WindowSizeSettings {
	return connect.WindowSizeSettings{
		WindowSizeMin:            2,
		WindowSizeMax:            2,
		WindowSizeHardMax:        2,
		WindowSizeReconnectScale: 1,
	}
}

func hostedWindowSizesTestSpeed() connect.WindowSizeSettings {
	return connect.WindowSizeSettings{
		WindowSizeMin:            1,
		WindowSizeMax:            1,
		WindowSizeHardMax:        1,
		FixedWindowSize:          1,
		WindowSizeReconnectScale: 1,
	}
}

// A multi client's settings as a window build sizes them before the host's
// policy: the 32-MiB profile that iOS and the hosted proxies share.
func hostedWindowSizesTestProfileSettings() *connect.MultiClientSettings {
	settings := connect.DefaultMultiClientSettings()
	applyMobileLowMemoryMultiClientSettingsForPlatform(settings, LowMemoryDeviceTargetByteCount, true)
	return settings
}

// No default device setting sizes a window, so apps keep their profile's
// sizes. A host's sizes replace the profile's for the windows they name and
// leave the other window's, and they go into a map of the settings' own.
func TestDeviceLocalWindowClientSettingsCarryTheHostWindowSizes(t *testing.T) {
	profile := hostedWindowSizesTestProfileSettings().WindowSizes
	if profile[connect.WindowTypeQuality] == hostedWindowSizesTestQuality() {
		t.Fatal("the profile's quality window is already the host's size, so this pins nothing")
	}
	for _, deviceLocalSettings := range []*DeviceLocalSettings{DefaultDeviceLocalSettings(), DefaultLowMemoryDeviceLocalSettings()} {
		if deviceLocalSettings.QualityWindowSize != nil || deviceLocalSettings.SpeedWindowSize != nil {
			t.Fatal("default device settings size a window")
		}
		settings := hostedWindowSizesTestProfileSettings()
		applyDeviceLocalWindowClientSettings(settings, deviceLocalSettings)
		connect.AssertEqual(t, settings.WindowSizes, profile)
	}

	hosted := DefaultLowMemoryDeviceLocalSettings()
	quality := hostedWindowSizesTestQuality()
	hosted.QualityWindowSize = &quality
	settings := hostedWindowSizesTestProfileSettings()
	shared := settings.WindowSizes
	applyDeviceLocalWindowClientSettings(settings, hosted)
	connect.AssertEqual(t, settings.WindowSizes[connect.WindowTypeQuality], quality)
	connect.AssertEqual(t, settings.WindowSizes[connect.WindowTypeSpeed], profile[connect.WindowTypeSpeed])
	connect.AssertEqual(t, shared, profile)

	speed := hostedWindowSizesTestSpeed()
	hosted.SpeedWindowSize = &speed
	settings = hostedWindowSizesTestProfileSettings()
	applyDeviceLocalWindowClientSettings(settings, hosted)
	connect.AssertEqual(t, settings.WindowSizes[connect.WindowTypeQuality], quality)
	connect.AssertEqual(t, settings.WindowSizes[connect.WindowTypeSpeed], speed)
}

// A hosted device the host sized builds windows that ask find-providers2 for
// the host's targets, two quality exits and one speed exit, where its 32-MiB
// profile asks for five and one: the device sizes its windows after its
// profile, at every window build.
func TestHostedWindowSizesReachTheWindows(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	discovery := newHostedWindowSelectorsTestDiscovery()
	settings := DefaultLowMemoryDeviceLocalSettings()
	settings.AllowProvider = false
	settings.DisableLogging = true
	settings.ProviderDiscovery = discovery
	settings.NormalWindowSelectors = hostedWindowSelectorsTestSelectors()
	settings.DisableLimitedWindow = true
	quality := hostedWindowSizesTestQuality()
	speed := hostedWindowSizesTestSpeed()
	settings.QualityWindowSize = &quality
	settings.SpeedWindowSize = &speed
	device := newSettingsOwnershipDevice(t, fixture, settings, true)

	location := &ConnectLocation{ConnectLocationId: &ConnectLocationId{BestAvailable: true}}
	if err := device.SetConnectLocationChecked(location); err != nil {
		t.Fatal(err)
	}
	requireLimitedWindowTestMulti(t, device)

	watchdog, stop := context.WithTimeout(t.Context(), 45*time.Second)
	defer stop()
	targetCounts := map[string]int{}
	for {
		update := discovery.asked.NotifyChannel()
		requests, selects := discovery.recorded()
		for i, request := range requests {
			if _, ok := targetCounts[request.RankMode]; !ok {
				targetCounts[request.RankMode] = selects[i].TargetCount
			}
		}
		if _, ok := targetCounts["quality"]; ok {
			if _, ok := targetCounts["speed"]; ok {
				break
			}
		}
		select {
		case <-update:
		case <-watchdog.Done():
			t.Fatalf("the windows asked with rank modes %v, want quality and speed", targetCounts)
		}
	}
	connect.AssertEqual(t, targetCounts["quality"], 2)
	connect.AssertEqual(t, targetCounts["speed"], 1)
}
