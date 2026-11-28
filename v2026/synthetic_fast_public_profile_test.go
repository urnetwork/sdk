//go:build !ios

package sdk

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// The external Fast fixture composes public factories; this SDK-owned control
// compares their actual devices and destination generations to platform policy.
func TestSyntheticFastPublicClient32FactoryParity(t *testing.T) {
	assertSyntheticFastPublicClientFactoryParity(t, 32*1024*1024, "ios", 6)
}

// A target-only desktop override would retain its smaller receive queue cap.
// The complete public Connect factory must precede SDK ownership attachment.
func TestSyntheticFastPublicClient64FactoryParity(t *testing.T) {
	assertSyntheticFastPublicClientFactoryParity(t, 64*1024*1024, "android", 12)
}

func assertSyntheticFastPublicClientFactoryParity(t *testing.T, target ByteCount, platform string, windowSize int) {
	t.Helper()
	if connect.MemoryBudget() != 0 {
		t.Fatal("profile parity requires an unscaled host process")
	}
	publicSettings := DefaultLowMemoryDeviceLocalSettings()
	if target != LowMemoryDeviceTargetByteCount {
		publicSettings = DefaultDeviceLocalSettings()
		publicSettings.MemoryTargetByteCount = target
		publicSettings.LowMemoryProfile = true
		publicSettings.ClientSettings = *connect.DefaultClientSettingsWithBufferSizeAndMemoryTarget(publicSettings.SequenceBufferSize, target)
		publicSettings.WebRtcSettings.UseEgressOnlyIceInterfaces = true
	}
	platformSettings := defaultDeviceLocalSettingsWithMemoryTargetForPlatform(target, platform)
	publicDevice := newSyntheticFastParityDevice(t, publicSettings)
	platformDevice := newSyntheticFastParityDevice(t, platformSettings)
	devices := []*DeviceLocal{publicDevice, platformDevice}
	expectedLimitedFraction := 1.0
	if target == LowMemoryDeviceTargetByteCount {
		expectedLimitedFraction = 0.5
	}
	if !reflect.DeepEqual(lowMemorySettingsSnapshot(publicDevice.settings), lowMemorySettingsSnapshot(platformDevice.settings)) ||
		!reflect.DeepEqual(publicDevice.GetPerformanceProfile(), platformDevice.GetPerformanceProfile()) {
		t.Fatal("public and platform constructors disagree on device policy")
	}
	assertSyntheticFastClientSettingsEqual(t, &publicDevice.settings.ClientSettings, &platformDevice.settings.ClientSettings)
	assertSyntheticFastCarrierSettingsEqual(t, publicDevice, platformDevice)
	assertSyntheticFastProviderOwnershipEqual(t, publicDevice, platformDevice)
	for _, device := range devices {
		assertSyntheticFastBudgetTopology(t, device, &device.settings.ClientSettings)
		if device.settings.DisableLimitedWindow || device.settings.NormalWindowSelectors != nil ||
			device.GetLimitedWindowSizeFraction() != expectedLimitedFraction {
			t.Fatal("public app defaults changed the normal selectors or limited-window fraction")
		}
	}
	if publicDevice.transferMemory.root == platformDevice.transferMemory.root ||
		publicDevice.settings.ReceiveBufferSettings.PackQueueBudget == platformDevice.settings.ReceiveBufferSettings.PackQueueBudget {
		t.Fatal("independent devices share an ownership root")
	}

	// Discovery remains held before identity or carrier creation. The existing
	// control still drives the real window resize and reconnect.
	var previous [2]*connect.ApiMultiClientGenerator
	for range 2 {
		var generated [2]*connect.ClientSettings
		for index, device := range devices {
			if err := device.SetConnectLocationChecked(&ConnectLocation{ConnectLocationId: &ConnectLocationId{BestAvailable: true}}); err != nil {
				t.Fatal(err)
			}
			multi := requireH3PolicyTarget(t, device, windowSize)
			if multi.LimitedWindowSizeFraction() != expectedLimitedFraction || device.GetLimitedWindowSizeFraction() != expectedLimitedFraction || multi.LimitedWindowSize() != 0 {
				t.Fatal("actual destination lost the current limited-window policy or bypassed held discovery")
			}
			device.stateLock.Lock()
			generator := device.apiMultiClientGenerator
			device.stateLock.Unlock()
			if generator == nil || generator == previous[index] {
				t.Fatal("public destination construction reused a retired generator")
			}
			generated[index] = generator.NewClientSettings()
			assertSyntheticFastBudgetTopology(t, device, generated[index])
			previous[index] = generator
			if err := device.SetConnectLocationChecked(nil); err != nil {
				t.Fatal(err)
			}
			join, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			err := multi.CloseAndWait(join)
			cancel()
			if err != nil {
				t.Fatal(err)
			}
		}
		assertSyntheticFastClientSettingsEqual(t, generated[0], generated[1])
	}
	for _, device := range devices {
		join, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		err := device.CloseAndWait(join)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		stats := device.transferMemory.root.Stats()
		if stats.UsedByteCount != 0 || stats.QuotaByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
			t.Fatalf("joined device retained transfer ownership: %+v", stats)
		}
		carrier := device.platformTransportBudget.Stats()
		if carrier.UsedByteCount != 0 || carrier.UsedTransportCount != 0 || carrier.ReservedByteCount != carrier.ReleasedByteCount {
			t.Fatalf("joined device retained carrier ownership: %+v", carrier)
		}
	}
	if connect.MemoryBudget() != 0 {
		t.Fatal("per-device profile changed the process memory target")
	}
}

// Match the real consumer: its provider and fallback NAT remain constructed,
// while public providing is disabled and discovery is held before exit clients.
func newSyntheticFastParityDevice(t *testing.T, settings *DeviceLocalSettings) *DeviceLocal {
	t.Helper()
	fixture := testingAuthClientShapeSpace(t)
	settings.AllowProvider = true
	settings.DefaultProvideControlMode = ProvideControlModeNever
	settings.ProvideExtenderEnabled = false
	settings.DefaultProvideExtender = false
	settings.EnableRpc = false
	settings.DefaultRouteLocal = false
	settings.DefaultOffline = false
	settings.DefaultTunnelStarted = true
	settings.DisableLogging = true
	settings.ProviderDiscovery = &heldH3PolicyDiscovery{}
	device, err := NewDeviceLocal(fixture.networkSpace, fixture.initialJwt, "fast-profile-parity", "test", "0", fixture.instanceId, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		join, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := device.CloseAndWait(join); err != nil {
			t.Error(err)
		}
	})
	device.SetTransportSettings(&TransportSettings{Mode: TransportModeH1})
	device.SetProviderTransportSettings(&TransportSettings{Mode: TransportModeH1})
	device.SetProvideControlMode(ProvideControlModeNever)
	device.SetProvidePaused(true)
	device.SetRouteLocal(false)
	device.SetPerformanceProfile(&PerformanceProfile{WindowType: WindowTypeAuto, AllowDirect: false})
	profile := device.GetPerformanceProfile()
	if profile == nil || profile.WindowType != WindowTypeAuto || profile.WindowSize != nil || profile.AllowDirect || profile.PostQuantumEncryption {
		t.Fatal("public relay profile changed production windows or enabled a direct route")
	}
	upgrade := connect.DefaultUpgradeMuxSettings()
	upgrade.Dns = nil
	device.SetUpgradeMuxSettings(upgrade)
	return device
}

// Compare all send/receive policy fields, including min-capped queue limits;
// ownership is checked separately rather than equating independent pointers.
func assertSyntheticFastClientSettingsEqual(t *testing.T, actual, expected *connect.ClientSettings) {
	t.Helper()
	actualSend, expectedSend := *actual.SendBufferSettings, *expected.SendBufferSettings
	actualSend.ResendQueueBudget, expectedSend.ResendQueueBudget = nil, nil
	actualReceive, expectedReceive := *actual.ReceiveBufferSettings, *expected.ReceiveBufferSettings
	actualReceive.ReceiveQueueBudget, expectedReceive.ReceiveQueueBudget = nil, nil
	actualReceive.ReceiveHeadRecoveryBudget, expectedReceive.ReceiveHeadRecoveryBudget = nil, nil
	actualReceive.PackQueueBudget, expectedReceive.PackQueueBudget = nil, nil
	actualWebRtc, expectedWebRtc := *actual.WebRtcSettings, *expected.WebRtcSettings
	actualWebRtc.MemoryBudget, expectedWebRtc.MemoryBudget = nil, nil
	actualWebRtc.NetworkPeerMemoryBudget, expectedWebRtc.NetworkPeerMemoryBudget = nil, nil
	actualWebRtc.Log, expectedWebRtc.Log = nil, nil
	actualWebRtc.DataPlaneStats, expectedWebRtc.DataPlaneStats = nil, nil
	if !reflect.DeepEqual(actualSend, expectedSend) {
		t.Fatal("public profile diverged from platform send policy")
	}
	if !reflect.DeepEqual(actualReceive, expectedReceive) {
		t.Fatalf("public profile diverged from platform receive policy: receive maxima %d/%d", actualReceive.ReceiveQueueMaxByteCount, expectedReceive.ReceiveQueueMaxByteCount)
	}
	if !reflect.DeepEqual(actualWebRtc, expectedWebRtc) || actual.SendBufferSize != expected.SendBufferSize ||
		actual.ForwardBufferSize != expected.ForwardBufferSize ||
		!reflect.DeepEqual(actual.ForwardBufferSettings, expected.ForwardBufferSettings) ||
		!reflect.DeepEqual(actual.ContractManagerSettings, expected.ContractManagerSettings) {
		t.Fatal("public profile diverged from platform client, contract or P2P policy")
	}
	actualBudgets := []*connect.TransferMemoryBudget{actual.SendBufferSettings.ResendQueueBudget, actual.ReceiveBufferSettings.ReceiveQueueBudget,
		actual.ReceiveBufferSettings.ReceiveHeadRecoveryBudget, actual.ReceiveBufferSettings.PackQueueBudget,
		actual.WebRtcSettings.MemoryBudget, actual.WebRtcSettings.NetworkPeerMemoryBudget}
	expectedBudgets := []*connect.TransferMemoryBudget{expected.SendBufferSettings.ResendQueueBudget, expected.ReceiveBufferSettings.ReceiveQueueBudget,
		expected.ReceiveBufferSettings.ReceiveHeadRecoveryBudget, expected.ReceiveBufferSettings.PackQueueBudget,
		expected.WebRtcSettings.MemoryBudget, expected.WebRtcSettings.NetworkPeerMemoryBudget}
	for index, actualBudget := range actualBudgets {
		if index == len(actualBudgets)-1 && actualBudget == nil && expectedBudgets[index] == nil {
			continue // No selected network peer; both factories leave this pool absent.
		}
		if actualBudget == nil || expectedBudgets[index] == nil || actualBudget.TotalByteCount() != expectedBudgets[index].TotalByteCount() {
			t.Fatalf("public profile diverged from platform budget %d", index)
		}
	}
}

// Live generators must reuse each device's own transfer tree, including NAT
// and peer-key siblings, rather than assembling independently funded pools.
func assertSyntheticFastBudgetTopology(t *testing.T, device *DeviceLocal, client *connect.ClientSettings) {
	t.Helper()
	memory := device.transferMemory
	if memory == nil || memory.client.Parent() != memory.root || memory.provider.Parent() != memory.root ||
		memory.nat.Parent() != memory.root || memory.peerKeyPins.Parent() != memory.client || memory.headRecovery.Parent() != memory.client ||
		memory.providerHeadRecovery.Parent() != memory.provider ||
		client.SendBufferSettings.ResendQueueBudget != device.settings.SendBufferSettings.ResendQueueBudget ||
		client.ReceiveBufferSettings.ReceiveQueueBudget != device.settings.ReceiveBufferSettings.ReceiveQueueBudget ||
		client.ReceiveBufferSettings.ReceiveHeadRecoveryBudget != memory.headRecovery ||
		client.ReceiveBufferSettings.PackQueueBudget != device.settings.ReceiveBufferSettings.PackQueueBudget ||
		client.WebRtcSettings.MemoryBudget != device.settings.WebRtcSettings.MemoryBudget {
		t.Fatal("destination escaped its device ownership topology")
	}
	for _, budget := range []*connect.TransferMemoryBudget{client.SendBufferSettings.ResendQueueBudget,
		client.ReceiveBufferSettings.ReceiveQueueBudget, client.ReceiveBufferSettings.PackQueueBudget,
		client.WebRtcSettings.MemoryBudget} {
		if budget == nil || budget.Parent() != memory.client {
			t.Fatal("destination budget has a different client owner")
		}
	}
	// A public destination uses the shared public P2P pool. Its unused
	// network-peer defaults are not a live selected-network owner.
	if client == &device.settings.ClientSettings &&
		client.WebRtcSettings.NetworkPeerMemoryBudget != nil && client.WebRtcSettings.NetworkPeerMemoryBudget.Parent() != memory.client {
		t.Fatal("device network-peer pool has a different client owner")
	}
	if !client.SendBufferSettings.ResendQueueRetainedByteAccounting || !client.ReceiveBufferSettings.ReceiveQueueRetainedByteAccounting ||
		!client.WebRtcSettings.UseEgressOnlyIceInterfaces {
		t.Fatal("public factory dropped retained ownership or SDK interface policy")
	}
}

// Compare actual carrier policies and the same NAT factory inputs passed to
// each constructed fallback owner. No packet or egress flow is submitted.
func assertSyntheticFastCarrierSettingsEqual(t *testing.T, actual, expected *DeviceLocal) {
	t.Helper()
	carrierValues := func(device *DeviceLocal) []int64 {
		settings := device.windowPlatformTransportSettings()
		if settings.PlatformTransportBudget != device.platformTransportBudget {
			t.Fatal("destination carrier replaced its device owner")
		}
		return []int64{device.platformTransportBudget.Stats().TotalByteCount, int64(settings.TransportBufferSize),
			int64(settings.H1AckPriorityBufferSize), settings.H1MaxMessageByteCount, settings.H1BudgetByteCount, settings.H3BudgetByteCount,
			settings.H3SocketReadBufferByteCount, settings.H3SocketWriteBufferByteCount,
			settings.H3InitialStreamReceiveWindowByteCount, settings.H3MaxStreamReceiveWindowByteCount,
			settings.H3InitialConnectionReceiveWindowByteCount, settings.H3MaxConnectionReceiveWindowByteCount,
			int64(settings.H3StreamWriteBatchMaxMessageCount), int64(settings.H3StreamWriteBatchMaxByteCount),
			int64(settings.WriteTimeout), int64(settings.ReadTimeout)}
	}
	if !reflect.DeepEqual(carrierValues(actual), carrierValues(expected)) {
		t.Fatal("public profile diverged from platform carrier policy")
	}
	natValues := func(device *DeviceLocal) []int64 {
		_, _, _, providerShare := deviceMemoryShares(device.settings)
		settings := providerLocalUserNatSettingsForProfile(providerShare, device.settings.MemoryTargetByteCount, device.settings.LowMemoryProfile, connect.NewNoopLogger())
		return []int64{device.transferMemory.nat.TotalByteCount(), int64(settings.SequenceBufferSize), int64(settings.SendShardCount),
			int64(settings.UdpBufferSettings.UserLimit), int64(settings.UdpBufferSettings.GlobalLimit),
			int64(settings.TcpBufferSettings.UserLimit), int64(settings.TcpBufferSettings.GlobalLimit),
			int64(settings.IcmpBufferSettings.UserLimit), int64(settings.IcmpBufferSettings.GlobalLimit),
			int64(settings.UdpBufferSettings.IdleTimeout)}
	}
	if !reflect.DeepEqual(natValues(actual), natValues(expected)) {
		t.Fatal("public profile diverged from platform NAT factory inputs")
	}
}

// Never/paused is an off role, not an absent provider. Inspect the actual
// constructor owners, including the NAT's standing charge, before any traffic.
func assertSyntheticFastProviderOwnershipEqual(t *testing.T, actual, expected *DeviceLocal) {
	t.Helper()
	providerValues := func(device *DeviceLocal) []int64 {
		device.stateLock.Lock()
		provider, memory := device.provider, device.transferMemory
		activeNat := device.remoteUserNatProvider != nil || device.remoteUserNatProviderLocalUserNat != nil
		device.stateLock.Unlock()
		if !device.settings.AllowProvider || provider == nil || provider.client == nil || provider.localUserNat == nil ||
			memory == nil || device.GetProvideEnabled() ||
			device.GetProvideControlMode() != ProvideControlModeNever || device.GetProvideMode() != ProvideModeNone || !device.GetProvidePaused() {
			t.Fatal("actual consumer lacks its constructed, paused off-role provider")
		}
		provider.stateLock.Lock()
		carrier := provider.platformTransportSettings
		hasTransport := provider.platformTransport != nil
		provider.stateLock.Unlock()
		send, receive := provider.transferBudgets()
		if send == nil || receive == nil || send.Parent() != memory.provider || receive.Parent() != memory.provider ||
			memory.providerHeadRecovery.Parent() != memory.provider || memory.nat.Parent() != memory.root ||
			!hasTransport || carrier == nil || carrier.PlatformTransportBudget != device.platformTransportBudget || activeNat {
			t.Fatal("actual provider or fallback NAT escaped its off-role owner tree")
		}
		nat := memory.nat.Stats()
		if nat.UsedByteCount <= 0 || nat.ReservedByteCount < nat.UsedByteCount {
			t.Fatal("constructed fallback NAT has no retained ownership charge")
		}
		return []int64{memory.root.TotalByteCount(), memory.client.TotalByteCount(), memory.provider.TotalByteCount(),
			send.TotalByteCount(), receive.TotalByteCount(), memory.providerHeadRecovery.TotalByteCount(),
			nat.TotalByteCount, nat.UsedByteCount, int64(carrier.TransportBufferSize), int64(carrier.H1AckPriorityBufferSize),
			carrier.H1BudgetByteCount, carrier.H3BudgetByteCount, carrier.H3MaxStreamReceiveWindowByteCount,
			carrier.H3MaxConnectionReceiveWindowByteCount}
	}
	if !reflect.DeepEqual(providerValues(actual), providerValues(expected)) {
		t.Fatal("public profile diverged from actual platform provider/NAT ownership")
	}
	if actual.provider.client == expected.provider.client || actual.provider.localUserNat == expected.provider.localUserNat {
		t.Fatal("independent devices share an actual provider or fallback NAT")
	}
}
