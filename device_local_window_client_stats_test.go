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

// Every default a host builds its device from turns the strict window handover
// on: the platform defaults of iOS, Android and the c abi's desktop hosts, and
// the low-memory profile. Where the profile makes the windows strict (iOS,
// Android, the low-memory settings), the handover engages; a host that turns
// it off keeps its windows as before. The host's counters are carried either
// way.
func TestDeviceLocalWindowClientSettingsTurnTheHandoverOnUnlessTheHostTurnsItOff(t *testing.T) {
	stats := connect.NewWindowClientStats()
	for name, deviceLocalSettings := range map[string]*DeviceLocalSettings{
		"default":    DefaultDeviceLocalSettings(),
		"low memory": DefaultLowMemoryDeviceLocalSettings(),
		"android":    defaultDeviceLocalSettingsForPlatform("android"),
		"ios":        defaultDeviceLocalSettingsForPlatform("ios"),
		"windows":    defaultDeviceLocalSettingsForPlatform("windows"),
		"linux":      defaultDeviceLocalSettingsForPlatform("linux"),
		"darwin":     defaultDeviceLocalSettingsForPlatform("darwin"),
	} {
		if !deviceLocalSettings.StrictWindowHandover {
			t.Errorf("%s: the default settings leave the handover off", name)
		}
		deviceLocalSettings.WindowClientStats = stats
		settings := connect.DefaultMultiClientSettings()
		applyMobileLowMemoryMultiClientSettingsForPlatform(settings, deviceLocalSettings.MemoryTargetByteCount, deviceLocalSettings.LowMemoryProfile)
		applyDeviceLocalWindowClientSettings(settings, deviceLocalSettings)
		if !settings.StrictWindowHandover || settings.WindowClientStats != stats {
			t.Errorf("%s: the device's multi clients do not hand strict window slots over, or lost the host's counters", name)
		}
		// the strict windows the handover acts on
		if deviceLocalSettings.LowMemoryProfile && !settings.StrictWindowSizeHardMax {
			t.Errorf("%s: the low-memory profile gives the device no strict windows", name)
		}

		// the host's emergency off switch
		deviceLocalSettings.StrictWindowHandover = false
		settings = connect.DefaultMultiClientSettings()
		applyMobileLowMemoryMultiClientSettingsForPlatform(settings, deviceLocalSettings.MemoryTargetByteCount, deviceLocalSettings.LowMemoryProfile)
		applyDeviceLocalWindowClientSettings(settings, deviceLocalSettings)
		if settings.StrictWindowHandover || settings.WindowClientStats != stats {
			t.Errorf("%s: the host turned the handover off and its windows still hand slots over, or lost its counters", name)
		}
	}
	// the iOS and Android defaults are the profiles that give windows a strict
	// hard max
	for _, goos := range []string{"ios", "android"} {
		if !defaultDeviceLocalSettingsForPlatform(goos).LowMemoryProfile {
			t.Errorf("%s: the platform default is not the low-memory profile", goos)
		}
	}
}
