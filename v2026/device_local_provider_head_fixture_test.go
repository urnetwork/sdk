// Provider role controls reuse production constructors with independent owners.
// This package-local fixture exists only in the SDK test binary.
package sdk

import (
	"context"
	"errors"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// One provider owns every budget and its standing pin store/fallback NAT.
// The caller creates the Client and active provider NAT from these settings,
// joins them, and only then checks closure. No limit is reconstructed from
// a numeric receipt and no budget is shared with another fixture provider.
type testMobileProviderFixture struct {
	clientSettings   *connect.ClientSettings
	localNatSettings *connect.LocalUserNatSettings
	remoteSettings   *connect.RemoteUserNatProviderSettings
	platformSettings *connect.PlatformTransportSettings
	fallbackNat      *connect.LocalUserNat
	targetByteCount  ByteCount
	providerShare    ByteCount

	memory    *deviceLocalTransferMemory
	transport *connect.PlatformTransportBudget
	settings  *DeviceLocalSettings
	state     *LocalState
	pins      *boundedPeerClientKeyPinStore
	device    *DeviceLocal
	closed    bool
}

// The caller captures DefaultLowMemoryDeviceLocalSettings while process sizing
// equals target, before any fixture worker starts, then restores host sizing.
// Capturing defaults here would change global policy under existing workers.
func newTestMobileProviderFixture(
	ctx context.Context,
	captured *DeviceLocalSettings,
	apiUrl string,
	strategy *connect.ClientStrategy,
	localStorageHome string,
) (*testMobileProviderFixture, error) {
	if ctx == nil || captured == nil || !captured.LowMemoryProfile || !captured.AllowProvider ||
		(captured.MemoryTargetByteCount != 32*1024*1024 && captured.MemoryTargetByteCount != 64*1024*1024) ||
		connect.MemoryBudget() != 0 || localStorageHome == "" {
		return nil, errors.New("mobile provider fixture requires captured low32/64 defaults, process0 and an owned local-state directory")
	}
	settingsCopy := *captured
	settingsCopy.ClientSettings = *cloneDeviceClientSettings(&captured.ClientSettings)
	settings := &settingsCopy
	settings.ClientSettings.Log = connect.NewNoopLogger()
	memory := newDeviceLocalTransferMemoryForPlatform(settings, true)
	configureDeviceLocalClientMemoryForPlatform(settings, memory, true)
	if memory == nil {
		return nil, errors.New("mobile provider fixture lost its real device owner tree")
	}
	_, _, _, providerShare := deviceMemoryShares(settings)
	self := &testMobileProviderFixture{
		targetByteCount: settings.MemoryTargetByteCount, providerShare: providerShare,
		memory: memory, transport: connect.NewPlatformTransportBudgetForMemoryTarget(settings.MemoryTargetByteCount),
		settings: settings, state: newLocalState(ctx, localStorageHome),
	}
	var err error
	self.pins, err = prepareBoundedPeerClientKeyPinStore(self.state, nil, memory.peerKeyPins)
	if err != nil {
		self.state.Close()
		return nil, err
	}
	if err := self.pins.activate(); err != nil {
		self.pins.Close()
		self.state.Close()
		return nil, err
	}
	if settings.EncryptionSettings == nil {
		settings.EncryptionSettings = connect.DefaultEncryptionSettings()
	}
	settings.EncryptionSettings.PeerClientKeyPinStore = self.pins
	client := newDeviceClientSettings(&settings.ClientSettings, apiUrl, strategy)
	applyMobileH1PerformanceClientSettingsForPlatform(client, settings.MemoryTargetByteCount, true, true)
	client.EncryptionSettings.Mode = connect.EncryptionModeOpportunistic
	client.ProviderStreamPolicy = true
	send, receive := configureDeviceLocalProviderMemory(client, providerShare, memory.provider)
	memory.configureProviderHeadRecovery(client)
	self.clientSettings = client
	self.device = &DeviceLocal{
		settings: settings, transferMemory: memory,
		provider: &deviceLocalProvider{resendQueueBudget: send, receiveQueueBudget: receive},
	}
	// This is the actual role transition, including any configured bounded
	// provider send loan. Its repeated application must never compound a loan.
	self.device.applyProvideMemorySharesWithLock(true)
	self.localNatSettings = providerLocalUserNatSettingsForProfile(providerShare, settings.MemoryTargetByteCount, settings.LowMemoryProfile, client.Log)
	self.localNatSettings.MemoryBudget = memory.nat
	self.remoteSettings = connect.DefaultRemoteUserNatProviderSettingsWithMemoryTarget(providerShare)
	self.platformSettings = newDeviceLocalPlatformTransportSettingsForProfile(
		settings.MemoryTargetByteCount, self.transport, nil, "", "", true, nil,
	)
	self.platformSettings.PlatformTransportBudgetPriority = connect.PlatformTransportBudgetPriorityBackground
	// Device construction keeps a separate fallback NAT alive even while the
	// active provider NAT serves traffic. Its fixed owner must consume this root.
	fallback := providerLocalUserNatSettingsForProfile(providerShare, settings.MemoryTargetByteCount, settings.LowMemoryProfile, client.Log)
	fallback.MemoryBudget = memory.nat
	self.fallbackNat, err = connect.TryNewLocalUserNat(ctx, "private mobile fallback", fallback)
	if err != nil {
		self.pins.Close()
		self.state.Close()
		return nil, err
	}
	return self, nil
}

// A single root lock covers overlapping leaves; values are physical ownership,
// while total bytes are permission ceilings and must not be summed as usage.
type testMobileProviderSnapshot struct {
	targetByteCount, providerShare                                                 ByteCount
	root, client, provider, nat, pins, send, receive, head, pack, p2p, networkPeer connect.TransferMemoryBudgetStats
	transport                                                                      connect.PlatformTransportBudgetStats
	closed                                                                         bool
}

// Samples the exact constructor pointers without repairing or reapplying policy.
func (self *testMobileProviderFixture) snapshot() testMobileProviderSnapshot {
	client := self.clientSettings
	rows := self.memory.root.StatsWithDescendants(
		self.memory.client, self.memory.provider, self.memory.nat, self.memory.peerKeyPins,
		client.SendBufferSettings.ResendQueueBudget, client.ReceiveBufferSettings.ReceiveQueueBudget,
		client.ReceiveBufferSettings.PackQueueBudget, client.WebRtcSettings.MemoryBudget,
		client.WebRtcSettings.NetworkPeerMemoryBudget,
		self.memory.providerHeadRecovery,
	)
	return testMobileProviderSnapshot{
		targetByteCount: self.targetByteCount, providerShare: self.providerShare,
		root: rows[0], client: rows[1], provider: rows[2], nat: rows[3], pins: rows[4],
		send: rows[5], receive: rows[6], pack: rows[7], p2p: rows[8], networkPeer: rows[9],
		head:      rows[10],
		transport: self.transport.Stats(), closed: self.closed,
	}
}

// Called after the active Client, provider, NAT and carrier have all joined.
// Standing owners close last so leaked traffic ownership remains observable.
func (self *testMobileProviderFixture) closeAndCheck(ctx context.Context) error {
	if !self.closed {
		if err := self.fallbackNat.CloseAndWait(ctx); err != nil {
			return err
		}
		self.pins.Close()
		self.state.Close()
		self.closed = true
	}
	value := self.snapshot()
	for _, row := range []connect.TransferMemoryBudgetStats{
		value.root, value.client, value.provider, value.nat, value.pins,
		value.send, value.receive, value.head, value.pack, value.p2p, value.networkPeer,
	} {
		if row.UsedByteCount != 0 || row.QuotaByteCount != 0 || row.ReservedByteCount != row.ReleasedByteCount {
			return errors.New("mobile provider fixture retained an owner or quota after join")
		}
	}
	if value.transport.UsedByteCount != 0 || value.transport.ReservedByteCount != value.transport.ReleasedByteCount {
		return errors.New("mobile provider fixture retained carrier ownership after join")
	}
	return nil
}

// Capture process-derived defaults before any fixture worker starts.
func testMobileProviderCapturedDefaults(t *testing.T, target ByteCount) *DeviceLocalSettings {
	t.Helper()
	if connect.MemoryBudget() != 0 {
		t.Fatal("mobile provider fixture controls require a fresh process0")
	}
	connect.SetMemoryBudget(target)
	defer connect.SetMemoryBudget(0)
	settings := DefaultLowMemoryDeviceLocalSettings()
	settings.MemoryTargetByteCount = target
	settings.DisableLogging = true
	settings.ProvideExtenderEnabled, settings.EnableRpc = false, false
	settings.ProviderDiscovery = &heldH3PolicyDiscovery{}
	return settings
}
