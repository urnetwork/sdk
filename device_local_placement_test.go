package sdk

import (
	"net/netip"
	"testing"

	"github.com/urnetwork/connect"
)

// A placement that offers nothing; the tests compare its identity only.
type testDevicePlatformPlacement struct{}

func (self *testDevicePlatformPlacement) PlatformPlacementTargets(host string) []netip.Addr {
	return nil
}

func (self *testDevicePlatformPlacement) PlatformPlacementFailed(host string, targetIps []netip.Addr) {
}

// Every window transport of a device the platform hosts carries the device's
// placement (connect platform_placement.go), so a hosted proxy's windows dial
// the edge the platform's load model chooses; a device without one resolves
// the platform host.
func TestDeviceLocalWindowTransportsCarryThePlatformPlacement(t *testing.T) {
	placement := &testDevicePlatformPlacement{}
	settings := DefaultDeviceLocalSettings()
	settings.DisableLogging = true
	settings.PlatformPlacement = placement
	device := newSettingsOwnershipDevice(t, testingAuthClientShapeSpace(t), settings, false)
	for i := 0; i < 2; i += 1 {
		windowSettings := device.windowPlatformTransportSettings()
		if windowSettings.Placement != connect.PlatformPlacement(placement) {
			t.Fatalf("window %d settings placement = %v, want the device's", i, windowSettings.Placement)
		}
	}

	unplacedSettings := DefaultDeviceLocalSettings()
	unplacedSettings.DisableLogging = true
	unplacedDevice := newSettingsOwnershipDevice(t, testingAuthClientShapeSpace(t), unplacedSettings, false)
	if placement := unplacedDevice.windowPlatformTransportSettings().Placement; placement != nil {
		t.Fatalf("a device without a placement places its windows with %v", placement)
	}
}
