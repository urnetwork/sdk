package sdk

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func waitForPauseDetectorClients(t *testing.T, detector *connect.SchedulerPauseDetector, want int, message string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for detector.ClientCount() != want {
		if deadline.Before(time.Now()) {
			t.Fatalf("%s: the detector judges for %d clients, want %d", message, detector.ClientCount(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

// A device given a host's shared pause detector builds multi clients that
// join it instead of running their own, and leave it when they close.
func TestDeviceLocalMultiClientsShareThePauseDetector(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}
	detector := connect.NewSchedulerPauseDetector(ctx)
	defer detector.Close()

	newDevice := func(detector *connect.SchedulerPauseDetector) *DeviceLocal {
		settings := DefaultDeviceLocalSettings()
		settings.DisableLogging = true
		settings.AllowProvider = false
		settings.SchedulerPauseDetector = detector
		settings.GeneratorFunc = func(specs []*connect.ProviderSpec) connect.MultiClientGenerator {
			return &rpcLeakTestGenerator{}
		}
		device, err := newDeviceLocalWithOverrides(networkSpace, byJwt, "", "", "", NewId(), settings, connect.NewId())
		if err != nil {
			t.Fatalf("new device: %v", err)
		}
		upgradeMuxSettings := connect.DefaultUpgradeMuxSettings()
		upgradeMuxSettings.Dns = nil
		device.SetUpgradeMuxSettings(upgradeMuxSettings)
		device.SetConnectLocation(&ConnectLocation{
			ConnectLocationId: &ConnectLocationId{BestAvailable: true},
			Name:              "best available",
		})
		return device
	}

	sharing := newDevice(detector)
	waitForPauseDetectorClients(t, detector, 1, "the connected device did not join the shared detector")

	// a device without one runs its own detector and joins nothing
	alone := newDevice(nil)
	defer alone.Close()
	time.Sleep(20 * time.Millisecond)
	if count := detector.ClientCount(); count != 1 {
		t.Fatalf("a device without the shared detector joined it: %d clients", count)
	}

	sharing.Close()
	waitForPauseDetectorClients(t, detector, 0, "the closed device's multi client did not leave the shared detector")
}
