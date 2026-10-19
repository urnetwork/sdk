package sdk

import (
	"reflect"
	"runtime/metrics"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// Freeze capacity/policy values, never pointer identities or asynchronously
// sampled usage, for exact platform-profile parity.
func lowMemorySettingsSnapshot(settings *DeviceLocalSettings) []int64 {
	send, receive, webRtc := settings.SendBufferSettings, settings.ReceiveBufferSettings, settings.WebRtcSettings
	return []int64{
		settings.MemoryTargetByteCount, int64(settings.SequenceBufferSize),
		int64(send.SequenceBufferSize), int64(send.AckBufferSize), send.ResendQueueMaxByteCount,
		send.ResendQueueBudget.TotalByteCount(), int64(receive.SequenceBufferSize),
		int64(receive.H1SequenceBufferSize), receive.SequenceBufferByteCount, receive.H1SequenceBufferByteCount,
		receive.ReceiveQueueBudget.TotalByteCount(), receive.PackQueueBudget.TotalByteCount(),
		webRtc.ReceiveBufferSize, webRtc.MemoryBudget.TotalByteCount(),
	}
}

// iOS and explicit hosted low settings have one target; Android's default and
// explicit 64-MiB override preserve their existing calibration.
func TestLowMemoryProfileDefaultsAndAndroid64RemainDistinct(t *testing.T) {
	ios := defaultDeviceLocalSettingsForPlatform("ios")
	hosted := DefaultLowMemoryDeviceLocalSettings()
	if LowMemoryDeviceTargetByteCount != 32*1024*1024 ||
		!ios.LowMemoryProfile || !hosted.LowMemoryProfile ||
		!reflect.DeepEqual(lowMemorySettingsSnapshot(ios), lowMemorySettingsSnapshot(hosted)) {
		t.Fatal("iOS and hosted low defaults diverged")
	}
	android := defaultDeviceLocalSettingsForPlatform("android")
	if android.MemoryTargetByteCount != mobileSteadyMemoryTargetByteCount || !android.LowMemoryProfile {
		t.Fatal("Android default/calibration changed")
	}
	android.MemoryTargetByteCount = 64 * 1024 * 1024
	for _, goos := range []string{"linux", "darwin", "windows"} {
		legacy := defaultDeviceLocalSettingsForPlatform(goos)
		if legacy.LowMemoryProfile || legacy.MemoryTargetByteCount != defaultDeviceLocalMemoryTargetByteCount {
			t.Fatalf("%s desktop default silently opted into low policy", goos)
		}
	}
	fixture := testingAuthClientShapeSpace(t)
	for _, settings := range []*DeviceLocalSettings{ios, hosted, android} {
		settings.HostedIncompatible = true
		settings.AllowProvider = false
		settings.DisableLogging = true
	}
	iosDevice := newSettingsOwnershipDevice(t, fixture, ios, true)
	hostedDevice := newSettingsOwnershipDevice(t, fixture, hosted, false)
	androidDevice := newSettingsOwnershipDevice(t, fixture, android, true)
	if !reflect.DeepEqual(lowMemorySettingsSnapshot(iosDevice.settings), lowMemorySettingsSnapshot(hostedDevice.settings)) {
		t.Fatal("actual iOS and hosted device queue policies diverged")
	}
	for _, device := range []*DeviceLocal{iosDevice, hostedDevice, androidDevice} {
		memory := device.transferMemory
		_, clientShare, _, providerShare := deviceMemoryShares(device.settings)
		if memory == nil || memory.root.TotalByteCount() != clientShare+providerShare ||
			memory.client.Parent() != memory.root || memory.provider.Parent() != memory.root ||
			memory.nat.Parent() != memory.root ||
			device.settings.ReceiveBufferSettings.PackQueueBudget.Parent() != memory.client ||
			!device.settings.SendBufferSettings.ResendQueueRetainedByteAccounting ||
			!device.settings.ReceiveBufferSettings.ReceiveQueueRetainedByteAccounting {
			t.Fatal("profile lost the shared retained-byte ownership topology")
		}
		if providerShare != 0 || memory.client.TotalByteCount() != memory.root.TotalByteCount() {
			t.Fatal("provider-disabled capacity did not fold into the client")
		}
		if got := device.platformTransportBudget.Stats().TotalByteCount; got != device.settings.MemoryTargetByteCount/4 {
			t.Fatalf("carrier budget = %d", got)
		}
	}
	if iosDevice.memorySampler == nil || androidDevice.memorySampler == nil ||
		hostedDevice.memorySampler != nil || hostedDevice.mobilePacketPressure != nil {
		t.Fatal("per-device profile changed actual-platform sampling/packet pressure selection")
	}
	if androidDevice.settings.ReceiveBufferSettings.H1SequenceBufferSize != 170 ||
		hostedDevice.settings.ReceiveBufferSettings.H1SequenceBufferSize != 85 ||
		androidDevice.settings.ReceiveBufferSettings.PackQueueBudget.TotalByteCount() <=
			hostedDevice.settings.ReceiveBufferSettings.PackQueueBudget.TotalByteCount() {
		t.Fatal("Android64 lost its existing larger scaled handoff capacity")
	}
}

// The actual destination/reconnect factory must use the selected owner policy,
// not runtime.GOOS. Caller-selected performance profiles still reach the owner.
func TestLowMemoryProfileDestinationGenerationsAndCustomProfile(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	settings := DefaultLowMemoryDeviceLocalSettings()
	settings.HostedIncompatible = true
	settings.AllowProvider = false
	settings.DisableLogging = true
	device := newSettingsOwnershipDevice(t, fixture, settings, false)
	device.SetTransportSettings(&TransportSettings{Mode: TransportModeH1})
	profile := &PerformanceProfile{
		WindowType:  WindowTypeSpeed,
		WindowSize:  &WindowSizeSettings{WindowSizeMin: 2, WindowSizeMax: 2, WindowSizeHardMax: 2, WindowSizeReconnectScale: 1},
		AllowDirect: true,
	}
	device.SetPerformanceProfile(profile)
	want := clonePerformanceProfile(profile)
	want.AllowDirect = false
	if !reflect.DeepEqual(device.GetPerformanceProfile(), want) || !profile.AllowDirect {
		t.Fatal("low profile replaced caller performance settings or hosted direct restriction")
	}
	upgrade := connect.DefaultUpgradeMuxSettings()
	upgrade.Dns = nil
	device.SetUpgradeMuxSettings(upgrade)
	var previous *connect.ApiMultiClientGenerator
	for range 2 {
		device.SetConnectLocation(&ConnectLocation{ConnectLocationId: &ConnectLocationId{BestAvailable: true}})
		device.stateLock.Lock()
		generator := device.apiMultiClientGenerator
		device.stateLock.Unlock()
		if generator == nil || generator == previous {
			t.Fatal("real destination factory did not create a fresh generation")
		}
		client := generator.NewClientSettings()
		if client.ReceiveBufferSettings.PackQueueBudget != device.settings.ReceiveBufferSettings.PackQueueBudget ||
			client.SendBufferSettings.ResendQueueBudget != device.settings.SendBufferSettings.ResendQueueBudget ||
			!client.SendBufferSettings.ResendQueueRetainedByteAccounting ||
			!client.ReceiveBufferSettings.ReceiveQueueRetainedByteAccounting ||
			client.ReceiveBufferSettings.H1SequenceBufferSize != 85 ||
			client.SendBufferSettings.LogicalDataLaneCount != 10 {
			t.Fatal("reconnect factory escaped its low-profile owner policy")
		}
		if !reflect.DeepEqual(device.GetPerformanceProfile(), want) {
			t.Fatal("reconnect discarded the custom profile")
		}
		previous = generator
		device.SetConnectLocation(nil)
	}
}

// Provider carriers use the same profile as destination carriers, including
// on a non-mobile host; this leaves the current H3 window policy unchanged.
func TestLowMemoryProfileProviderCarrierAndWindowParity(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	settings := DefaultLowMemoryDeviceLocalSettings()
	settings.DisableLogging = true
	device := newSettingsOwnershipDevice(t, fixture, settings, false)
	if device.provider == nil {
		t.Fatal("provider-capable fixture did not create its actual provider")
	}
	device.provider.platformTransport.Close()
	provider := device.provider.platformTransportSettings
	want := newDeviceLocalPlatformTransportSettingsForProfile(settings.MemoryTargetByteCount,
		device.platformTransportBudget, nil, "", "", true)
	if provider.PlatformTransportBudget != device.platformTransportBudget ||
		provider.H1AckPriorityBufferSize != want.H1AckPriorityBufferSize ||
		provider.H3BudgetByteCount != want.H3BudgetByteCount ||
		provider.H3MaxStreamReceiveWindowByteCount != want.H3MaxStreamReceiveWindowByteCount ||
		provider.H3MaxConnectionReceiveWindowByteCount != want.H3MaxConnectionReceiveWindowByteCount {
		t.Fatal("provider carrier used a different low-memory policy")
	}
	iosWindow, serverWindow := connect.DefaultMultiClientSettings(), connect.DefaultMultiClientSettings()
	applyMobileLowMemoryMultiClientSettingsForPlatform(iosWindow, LowMemoryDeviceTargetByteCount, true)
	applyMobileLowMemoryMultiClientSettingsForPlatform(serverWindow, device.settings.MemoryTargetByteCount, device.settings.LowMemoryProfile)
	if !reflect.DeepEqual(iosWindow.WindowSizes, serverWindow.WindowSizes) ||
		serverWindow.WindowSizes[connect.WindowTypeQuality].WindowSizeMin != 5 ||
		serverWindow.WindowSizes[connect.WindowTypeSpeed].WindowSizeMin != 1 ||
		!serverWindow.StrictWindowSizeHardMax || serverWindow.StandingReserve ||
		serverWindow.PacketGroupMaxPacketCount != 16 || serverWindow.PacketGroupMaxByteCount != 24*1024 {
		t.Fatal("shared profile changed the existing calibrated topology")
	}
}

// Host low devices own only admission budgets; process Go pacing and all
// allocator free-list capacities must survive construction and joined close.
func TestLowMemoryProfileLeavesHostProcessLimitsUnchanged(t *testing.T) {
	readRuntime := func() []uint64 {
		samples := []metrics.Sample{{Name: "/gc/gomemlimit:bytes"}, {Name: "/gc/gogc:percent"}}
		metrics.Read(samples)
		return []uint64{samples[0].Value.Uint64(), samples[1].Value.Uint64()}
	}
	poolCapacities := func() map[int]int {
		values := map[int]int{}
		for _, stats := range connect.GetMessagePoolClassStats() {
			values[stats.Size] = stats.Capacity
		}
		return values
	}
	beforeRuntime, beforePools, beforeBudget := readRuntime(), poolCapacities(), connect.MemoryBudget()
	settings := DefaultLowMemoryDeviceLocalSettings()
	assertDeviceLocalSettingsOwnerIndependence(t, settings, false)
	if !reflect.DeepEqual(beforeRuntime, readRuntime()) ||
		!reflect.DeepEqual(beforePools, poolCapacities()) || beforeBudget != connect.MemoryBudget() {
		t.Fatal("per-device low profile changed process-owned limits")
	}
}

// Cloning is deliberately not defaulting/auth installation. Explicit lower
// caller caps and callbacks retain their identity until the existing client
// boundary applies its own documented defaults.
func TestLowMemoryProfileCloneAndConfiguredTargetCompatibility(t *testing.T) {
	partial := &connect.ClientSettings{}
	cloned := cloneDeviceClientSettings(partial)
	if !reflect.DeepEqual(partial, cloned) || cloneDeviceClientSettings(nil) != nil {
		t.Fatal("clone-only boundary installed defaults or callbacks")
	}
	fixture := testingAuthClientShapeSpace(t)
	for _, target := range []int64{0, 48 * 1024 * 1024} {
		settings := DefaultLowMemoryDeviceLocalSettings()
		settings.MemoryTargetByteCount = target
		settings.HostedIncompatible = true
		settings.AllowProvider = false
		settings.DisableLogging = true
		settings.SendBufferSettings.SequenceBufferSize = 3
		device := newSettingsOwnershipDevice(t, fixture, settings, false)
		if device.MemoryUsed().TargetByteCount != target || device.settings.SendBufferSettings.SequenceBufferSize != 3 {
			t.Fatal("low constructor discarded a configured target or smaller caller cap")
		}
		if (device.transferMemory == nil) != (target == 0) {
			t.Fatal("zero target no longer preserves legacy opt-out")
		}
	}
}
