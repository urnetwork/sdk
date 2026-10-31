package sdk

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The window client counts every pass that wants to grow, so a forming window
// shows up as expansion passes within its first resize pass.
func windowClientStatsExpandPassCount(stats *connect.WindowClientStats) uint64 {
	total := uint64(0)
	stats.VisitExpandPasses(func(window string, state string, count uint64) {
		total += count
	})
	return total
}

// A device given a host's window client counters builds multi clients whose
// windows count into them; a device without counters counts nothing there.
func TestDeviceLocalMultiClientsCountIntoTheWindowClientStats(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}
	stats := connect.NewWindowClientStats()

	newDevice := func(stats *connect.WindowClientStats) *DeviceLocal {
		settings := DefaultDeviceLocalSettings()
		settings.DisableLogging = true
		settings.AllowProvider = false
		settings.WindowClientStats = stats
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

	// a device without counters forms its windows first; nothing reaches the
	// host's counters from it
	alone := newDevice(nil)
	defer alone.Close()
	time.Sleep(50 * time.Millisecond)
	if count := windowClientStatsExpandPassCount(stats); count != 0 {
		t.Fatalf("a device without counters counted %d expansion passes into them", count)
	}

	counted := newDevice(stats)
	defer counted.Close()
	deadline := time.Now().Add(5 * time.Second)
	for windowClientStatsExpandPassCount(stats) == 0 {
		if deadline.Before(time.Now()) {
			t.Fatal("the device's windows counted no expansion pass into the host's counters")
		}
		time.Sleep(time.Millisecond)
	}
}
