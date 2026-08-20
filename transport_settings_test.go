package sdk

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

type testingTransportSettingsChangeListener struct {
	mutex          sync.Mutex
	clientValues   []*TransportSettings
	providerValues []*TransportSettings
	notify         chan struct{}
}

func newTestingTransportSettingsChangeListener() *testingTransportSettingsChangeListener {
	return &testingTransportSettingsChangeListener{
		notify: make(chan struct{}, 32),
	}
}

func (self *testingTransportSettingsChangeListener) TransportSettingsChanged(transportSettings *TransportSettings) {
	self.mutex.Lock()
	self.clientValues = append(self.clientValues, transportSettings)
	self.mutex.Unlock()
	select {
	case self.notify <- struct{}{}:
	default:
	}
}

func (self *testingTransportSettingsChangeListener) ProviderTransportSettingsChanged(transportSettings *TransportSettings) {
	self.mutex.Lock()
	self.providerValues = append(self.providerValues, transportSettings)
	self.mutex.Unlock()
	select {
	case self.notify <- struct{}{}:
	default:
	}
}

func (self *testingTransportSettingsChangeListener) values(provider bool) []*TransportSettings {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if provider {
		return slices.Clone(self.providerValues)
	}
	return slices.Clone(self.clientValues)
}

func (self *testingTransportSettingsChangeListener) waitForCount(
	t *testing.T,
	provider bool,
	want int,
) []*TransportSettings {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		values := self.values(provider)
		if len(values) >= want {
			return values
		}
		select {
		case <-self.notify:
		case <-deadline.C:
			t.Fatalf("transport settings listener count=%d, want at least %d", len(values), want)
		}
	}
}

func testingTransportSettings(
	mode TransportMode,
	priorities ...*TransportModePriority,
) *TransportSettings {
	list := NewTransportModePriorityList()
	list.addAll(priorities...)
	return &TransportSettings{
		Mode:               mode,
		AutoModePriorities: list,
	}
}

func assertTransportSettings(
	t *testing.T,
	got *TransportSettings,
	want *TransportSettings,
	provider bool,
) {
	t.Helper()
	got = normalizeTransportSettings(got, provider)
	want = normalizeTransportSettings(want, provider)
	if !transportSettingsEqual(got, want, provider) {
		t.Fatalf("transport settings = %#v, want %#v", got, want)
	}
}

func TestDefaultTransportSettingsContract(t *testing.T) {
	want := testingTransportSettings(
		TransportModeAuto,
		&TransportModePriority{Mode: TransportModeH1, Priority: 1},
		&TransportModePriority{Mode: TransportModeH3, Priority: 2},
		&TransportModePriority{Mode: TransportModeDns, Priority: 3},
		&TransportModePriority{Mode: TransportModeDnsPump, Priority: 4},
	)
	assertTransportSettings(t, DefaultTransportSettings(), want, false)
	assertTransportSettings(t, DefaultProviderTransportSettings(), want, true)

	mode, preferences := toConnectTransportPolicy(DefaultTransportSettings(), false)
	connect.AssertEqual(t, mode, connect.TransportModeAuto)
	connect.AssertEqual(t, preferences[connect.TransportModeH1], 1)
	connect.AssertEqual(t, preferences[connect.TransportModeH3], 2)
	connect.AssertEqual(t, preferences[connect.TransportModeH3Dns], 3)
	connect.AssertEqual(t, preferences[connect.TransportModeH3DnsPump], 4)

	// Auto priorities never rewrite an explicit carrier selection.
	explicitH3 := DefaultTransportSettings()
	explicitH3.Mode = TransportModeH3
	mode, preferences = toConnectTransportPolicy(explicitH3, false)
	connect.AssertEqual(t, mode, connect.TransportModeH3)
	if preferences != nil {
		t.Fatalf("explicit H3 preferences = %v, want nil", preferences)
	}
}

func TestTransportSettingsMigratesLegacyDefaultAutoPolicy(t *testing.T) {
	legacy := testingTransportSettings(
		TransportModeH1,
		&TransportModePriority{Mode: TransportModeH3, Priority: 1},
		&TransportModePriority{Mode: TransportModeH1, Priority: 1},
		&TransportModePriority{Mode: TransportModeDns, Priority: 2},
		&TransportModePriority{Mode: TransportModeDnsPump, Priority: 3},
	)
	want := DefaultTransportSettings()
	want.Mode = TransportModeH1
	assertTransportSettings(t, legacy, want, false)

	// The migrated policy is retained under an explicit mode and becomes the
	// effective H1-first ladder when the user later switches back to Auto.
	legacy.Mode = TransportModeAuto
	mode, preferences := toConnectTransportPolicy(legacy, false)
	connect.AssertEqual(t, mode, connect.TransportModeAuto)
	if preferences[connect.TransportModeH1] >= preferences[connect.TransportModeH3] {
		t.Fatalf("migrated Auto preferences = %v, want H1 strictly ahead of H3", preferences)
	}

	// Partial policies were produced by disabling modes in the UI. Migrate the
	// retained H3 priority too, or re-enabling H1 at its new default would tie it.
	partial := normalizeTransportSettings(testingTransportSettings(
		TransportModeAuto,
		&TransportModePriority{Mode: TransportModeH3, Priority: 1},
	), false)
	partial.AutoModePriorities.Add(&TransportModePriority{Mode: TransportModeH1, Priority: 1})
	_, preferences = toConnectTransportPolicy(partial, false)
	if preferences[connect.TransportModeH1] >= preferences[connect.TransportModeH3] {
		t.Fatalf("re-enabled partial Auto preferences = %v, want H1 strictly ahead of H3", preferences)
	}
}

func TestTransportSettingsNormalizeCanonicalAndDetached(t *testing.T) {
	input := testingTransportSettings(
		"unknown",
		&TransportModePriority{Mode: TransportModeH1, Priority: 10},
		&TransportModePriority{Mode: TransportModeH3, Priority: 5},
		&TransportModePriority{Mode: TransportModeDns, Priority: 0},
		&TransportModePriority{Mode: TransportModeAuto, Priority: 1},
		&TransportModePriority{Mode: TransportModeH3, Priority: 10},
		nil,
	)
	got := normalizeTransportSettings(input, false)
	want := testingTransportSettings(
		TransportModeAuto,
		&TransportModePriority{Mode: TransportModeH1, Priority: 10},
		&TransportModePriority{Mode: TransportModeH3, Priority: 10},
	)
	assertTransportSettings(t, got, want, false)

	// The final duplicate wins, equal priorities use the stable product order,
	// and neither later input nor returned-value mutation aliases stored state.
	input.AutoModePriorities.Get(4).Priority = 99
	input.AutoModePriorities.Add(&TransportModePriority{Mode: TransportModeDnsPump, Priority: 1})
	assertTransportSettings(t, got, want, false)

	fallback := normalizeTransportSettings(
		testingTransportSettings(
			TransportModeDns,
			&TransportModePriority{Mode: "bad", Priority: 1},
		),
		false,
	)
	connect.AssertEqual(t, fallback.Mode, TransportModeDns)
	assertTransportSettings(
		t,
		&TransportSettings{Mode: TransportModeAuto, AutoModePriorities: fallback.AutoModePriorities},
		DefaultTransportSettings(),
		false,
	)
}

func TestTransportSettingsLocalStateRoundTrip(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	localState := newLocalState(ctx, t.TempDir())
	defer localState.cancel()

	clientSettings := testingTransportSettings(
		TransportModeAuto,
		&TransportModePriority{Mode: TransportModeDns, Priority: 1},
		&TransportModePriority{Mode: TransportModeH3, Priority: 2},
	)
	providerSettings := testingTransportSettings(
		TransportModeDnsPump,
		&TransportModePriority{Mode: TransportModeH1, Priority: 4},
	)
	connect.AssertEqual(t, localState.SetTransportSettings(clientSettings), nil)
	connect.AssertEqual(t, localState.SetProviderTransportSettings(providerSettings), nil)
	assertTransportSettings(t, localState.GetTransportSettings(), clientSettings, false)
	assertTransportSettings(t, localState.GetProviderTransportSettings(), providerSettings, true)

	connect.AssertEqual(t, localState.SetTransportSettings(nil), nil)
	connect.AssertEqual(t, localState.GetTransportSettings(), (*TransportSettings)(nil))
	// Client and provider settings are independent files.
	assertTransportSettings(t, localState.GetProviderTransportSettings(), providerSettings, true)
}

func TestDeviceLocalTransportSettingsPersistRestoreAndClone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	connect.AssertEqual(t, err, nil)

	clientSettings := testingTransportSettings(
		TransportModeAuto,
		&TransportModePriority{Mode: TransportModeH3, Priority: 1},
		&TransportModePriority{Mode: TransportModeDns, Priority: 2},
	)
	providerSettings := testingTransportSettings(
		TransportModeH1,
		&TransportModePriority{Mode: TransportModeDnsPump, Priority: 7},
	)
	device := testing_newBlockDeviceWithNetworkSpace(t, networkSpace, byJwt, false)
	device.SetTransportSettings(clientSettings)
	device.SetProviderTransportSettings(providerSettings)
	assertTransportSettings(t, device.GetTransportSettings(), clientSettings, false)
	assertTransportSettings(t, device.GetProviderTransportSettings(), providerSettings, true)

	// Public setters and getters both detach their object graphs.
	clientSettings.AutoModePriorities.Get(0).Priority = 99
	got := device.GetTransportSettings()
	got.AutoModePriorities.Get(0).Priority = 88
	wantClientSettings := testingTransportSettings(
		TransportModeAuto,
		&TransportModePriority{Mode: TransportModeH3, Priority: 1},
		&TransportModePriority{Mode: TransportModeDns, Priority: 2},
	)
	assertTransportSettings(t, device.GetTransportSettings(), wantClientSettings, false)
	device.Close()

	restored := testing_newBlockDeviceWithNetworkSpace(t, networkSpace, byJwt, false)
	defer restored.Close()
	assertTransportSettings(t, restored.GetTransportSettings(), wantClientSettings, false)
	assertTransportSettings(t, restored.GetProviderTransportSettings(), providerSettings, true)
}

func TestDeviceLocalTransportSettingsChangeListeners(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	connect.AssertEqual(t, err, nil)

	device := testing_newBlockDeviceWithNetworkSpace(t, networkSpace, byJwt, false)
	defer device.Close()
	listener := newTestingTransportSettingsChangeListener()
	clientSub := device.AddTransportSettingsChangeListener(listener)
	providerSub := device.AddProviderTransportSettingsChangeListener(listener)

	clientSettings := testingTransportSettings(
		TransportModeH1,
		&TransportModePriority{Mode: TransportModeH3, Priority: 1},
		&TransportModePriority{Mode: TransportModeH1, Priority: 1},
	)
	providerSettings := testingTransportSettings(
		TransportModeDns,
		&TransportModePriority{Mode: TransportModeDns, Priority: 1},
	)
	device.SetTransportSettings(clientSettings)
	device.SetProviderTransportSettings(providerSettings)

	clientValues := listener.waitForCount(t, false, 1)
	providerValues := listener.waitForCount(t, true, 1)
	assertTransportSettings(t, clientValues[0], clientSettings, false)
	assertTransportSettings(t, providerValues[0], providerSettings, true)

	// Callers and listeners receive detached graphs. Mutating either cannot
	// rewrite the effective device policy.
	clientSettings.Mode = TransportModeDnsPump
	clientValues[0].Mode = TransportModeH3
	providerSettings.Mode = TransportModeH1
	providerValues[0].Mode = TransportModeH3
	connect.AssertEqual(t, device.GetTransportSettings().Mode, TransportModeH1)
	connect.AssertEqual(t, device.GetProviderTransportSettings().Mode, TransportModeDns)

	// Canonically equal writes are not changes and do not fire listeners.
	device.SetTransportSettings(device.GetTransportSettings().Clone())
	device.SetProviderTransportSettings(device.GetProviderTransportSettings().Clone())
	connect.AssertEqual(t, len(listener.values(false)), 1)
	connect.AssertEqual(t, len(listener.values(true)), 1)

	clientSub.Close()
	providerSub.Close()
	device.SetTransportSettings(testingTransportSettings(
		TransportModeH3,
		&TransportModePriority{Mode: TransportModeH3, Priority: 1},
	))
	device.SetProviderTransportSettings(testingTransportSettings(
		TransportModeH1,
		&TransportModePriority{Mode: TransportModeH1, Priority: 1},
	))
	connect.AssertEqual(t, len(listener.values(false)), 1)
	connect.AssertEqual(t, len(listener.values(true)), 1)
}

func TestTransportSettingsRpcWireRoundTrip(t *testing.T) {
	settings := testingTransportSettings(
		TransportModeAuto,
		&TransportModePriority{Mode: TransportModeH3, Priority: 1},
		&TransportModePriority{Mode: TransportModeH1, Priority: 1},
		&TransportModePriority{Mode: TransportModeDnsPump, Priority: 3},
	)
	wired := gobRoundTrip(t, &DeviceRemoteTransportSettingsRpc{
		TransportSettings: newTransportSettingsRpc(settings, false),
	})
	assertTransportSettings(t, wired.TransportSettings.toTransportSettings(false), settings, false)
}

func TestDeviceTransportSettingsHostedGuardsPinH1(t *testing.T) {
	h3 := testingTransportSettings(
		TransportModeH3,
		&TransportModePriority{Mode: TransportModeH3, Priority: 1},
	)
	dns := testingTransportSettings(
		TransportModeDns,
		&TransportModePriority{Mode: TransportModeDns, Priority: 1},
	)
	logger := connect.NewNoopLogger()
	hosted := &DeviceLocal{
		settings:                  &DeviceLocalSettings{HostedIncompatible: true},
		log:                       logger,
		transportSettings:         h3,
		providerTransportSettings: h3,
	}
	hosted.SetTransportSettings(dns)
	hosted.SetProviderTransportSettings(dns)
	connect.AssertEqual(t, hosted.GetTransportSettings().Mode, TransportModeH1)
	connect.AssertEqual(t, hosted.GetProviderTransportSettings().Mode, TransportModeH1)

	// Pin the final boundary used by the built-in ApiMultiClientGenerator, not
	// just the value exposed by the settings getters. A hosted proxy must pass
	// an explicit H1 target to Connect with no Auto preference map.
	for name, settings := range map[string]*TransportSettings{
		"client":   hosted.GetTransportSettings(),
		"provider": hosted.GetProviderTransportSettings(),
	} {
		mode, preferences := toConnectTransportPolicy(settings, name == "provider")
		if mode != connect.TransportModeH1 {
			t.Fatalf("hosted %s Connect mode = %q, expected explicit h1", name, mode)
		}
		if preferences != nil {
			t.Fatalf("hosted %s Connect preferences = %v, expected nil for explicit h1", name, preferences)
		}
	}

	// The RPC layer independently blocks the same setters, even if its local
	// object was not constructed with the DeviceLocal guard.
	rpcLocal := &DeviceLocal{
		settings:                  &DeviceLocalSettings{},
		log:                       logger,
		transportSettings:         cloneTransportSettings(h3),
		providerTransportSettings: cloneTransportSettings(h3),
	}
	localRpc := &DeviceLocalRpc{
		deviceLocal: rpcLocal,
		settings:    &deviceRpcSettings{DisableHostedIncompatible: true},
	}
	connect.AssertEqual(
		t,
		localRpc.SetTransportSettings(
			&DeviceRemoteTransportSettingsRpc{TransportSettings: newTransportSettingsRpc(dns, false)},
			nil,
		),
		nil,
	)
	connect.AssertEqual(
		t,
		localRpc.SetProviderTransportSettings(
			&DeviceRemoteTransportSettingsRpc{TransportSettings: newTransportSettingsRpc(dns, true)},
			nil,
		),
		nil,
	)
	connect.AssertEqual(t, rpcLocal.GetTransportSettings().Mode, TransportModeH3)
	connect.AssertEqual(t, rpcLocal.GetProviderTransportSettings().Mode, TransportModeH3)

	remote := &DeviceRemote{
		settings: &deviceRpcSettings{DisableHostedIncompatible: true},
		log:      logger,
	}
	remote.SetTransportSettings(dns)
	remote.SetProviderTransportSettings(dns)
	connect.AssertEqual(t, remote.GetTransportSettings().Mode, TransportModeH1)
	connect.AssertEqual(t, remote.GetProviderTransportSettings().Mode, TransportModeH1)
}

func TestDeviceRemoteTransportSettingsOfflineSyncAndConnectedRpc(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deviceLocal, rpcSettings := testing_newRpcDeviceLocal(t, ctx)
	deviceRemote := testing_newRpcDeviceRemote(
		t,
		deviceLocal,
		rpcSettings,
		deviceLocal.GetInstanceId(),
		DeviceRpcVersion,
	)

	clientSettings := testingTransportSettings(
		TransportModeAuto,
		&TransportModePriority{Mode: TransportModeH3, Priority: 1},
		&TransportModePriority{Mode: TransportModeH1, Priority: 1},
		&TransportModePriority{Mode: TransportModeDns, Priority: 2},
	)
	providerSettings := testingTransportSettings(
		TransportModeDnsPump,
		&TransportModePriority{Mode: TransportModeH1, Priority: 1},
	)
	deviceRemote.SetTransportSettings(clientSettings)
	deviceRemote.SetProviderTransportSettings(providerSettings)
	assertTransportSettings(t, deviceRemote.GetTransportSettings(), clientSettings, false)
	assertTransportSettings(t, deviceRemote.GetProviderTransportSettings(), providerSettings, true)

	deviceRemote.Sync()
	connect.AssertEqual(t, deviceRemote.waitForSync(10*time.Second), true)
	assertTransportSettings(t, deviceLocal.GetTransportSettings(), clientSettings, false)
	assertTransportSettings(t, deviceLocal.GetProviderTransportSettings(), providerSettings, true)
	assertTransportSettings(t, deviceRemote.GetTransportSettings(), clientSettings, false)
	assertTransportSettings(t, deviceRemote.GetProviderTransportSettings(), providerSettings, true)

	// Once connected, setters take the direct RPC path and getters read the
	// authoritative local value rather than a stale queued copy.
	connectedClientSettings := testingTransportSettings(
		TransportModeH1,
		&TransportModePriority{Mode: TransportModeDns, Priority: 4},
	)
	deviceRemote.SetTransportSettings(connectedClientSettings)
	assertTransportSettings(t, deviceLocal.GetTransportSettings(), connectedClientSettings, false)
	assertTransportSettings(t, deviceRemote.GetTransportSettings(), connectedClientSettings, false)

	localProviderSettings := testingTransportSettings(
		TransportModeH3,
		&TransportModePriority{Mode: TransportModeH3, Priority: 1},
	)
	deviceLocal.SetProviderTransportSettings(localProviderSettings)
	assertTransportSettings(t, deviceRemote.GetProviderTransportSettings(), localProviderSettings, true)
}

func TestDeviceRemoteTransportSettingsChangeListenersOfflineAndRpc(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	connect.AssertEqual(t, err, nil)
	clientId := connect.NewId()
	instanceId := NewId()

	// Start the remote before a service exists so listener registration and
	// setter notification exercise the real offline queue.
	deviceRemote, err := newDeviceRemoteWithOverrides(
		networkSpace,
		byJwt,
		instanceId,
		defaultDeviceRpcSettings(),
		clientId,
		testing_deviceRpcDialerDefault(),
	)
	connect.AssertEqual(t, err, nil)
	defer deviceRemote.Close()

	listener := newTestingTransportSettingsChangeListener()
	clientSub := deviceRemote.AddTransportSettingsChangeListener(listener)
	defer clientSub.Close()
	providerSub := deviceRemote.AddProviderTransportSettingsChangeListener(listener)
	defer providerSub.Close()

	clientOffline := testingTransportSettings(
		TransportModeH1,
		&TransportModePriority{Mode: TransportModeH1, Priority: 1},
	)
	providerOffline := testingTransportSettings(
		TransportModeDns,
		&TransportModePriority{Mode: TransportModeDns, Priority: 1},
	)
	deviceRemote.SetTransportSettings(clientOffline)
	deviceRemote.SetProviderTransportSettings(providerOffline)
	assertTransportSettings(t, listener.waitForCount(t, false, 1)[0], clientOffline, false)
	assertTransportSettings(t, listener.waitForCount(t, true, 1)[0], providerOffline, true)

	// A repeated offline write still stays queued for synchronization but does
	// not report a setting change.
	deviceRemote.SetTransportSettings(clientOffline.Clone())
	deviceRemote.SetProviderTransportSettings(providerOffline.Clone())
	connect.AssertEqual(t, len(listener.values(false)), 1)
	connect.AssertEqual(t, len(listener.values(true)), 1)

	localSettings := testDeviceLocalSettingsRpc()
	localSettings.DisableLogging = true
	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace,
		byJwt,
		"",
		"",
		"",
		instanceId,
		localSettings,
		clientId,
	)
	connect.AssertEqual(t, err, nil)
	defer deviceLocal.Close()

	deviceRemote.Sync()
	connect.AssertEqual(t, deviceRemote.waitForSync(10*time.Second), true)
	// SyncReverse publishes one level event for every registered listener so a
	// cold app process immediately sees the extension's authoritative state.
	assertTransportSettings(t, listener.waitForCount(t, false, 2)[1], clientOffline, false)
	assertTransportSettings(t, listener.waitForCount(t, true, 2)[1], providerOffline, true)

	clientFromLocal := testingTransportSettings(
		TransportModeH3,
		&TransportModePriority{Mode: TransportModeH3, Priority: 1},
	)
	providerFromLocal := testingTransportSettings(
		TransportModeDnsPump,
		&TransportModePriority{Mode: TransportModeDnsPump, Priority: 1},
	)
	deviceLocal.SetTransportSettings(clientFromLocal)
	deviceLocal.SetProviderTransportSettings(providerFromLocal)
	assertTransportSettings(t, listener.waitForCount(t, false, 3)[2], clientFromLocal, false)
	assertTransportSettings(t, listener.waitForCount(t, true, 3)[2], providerFromLocal, true)

	clientFromRemote := DefaultTransportSettings()
	providerFromRemote := testingTransportSettings(
		TransportModeH1,
		&TransportModePriority{Mode: TransportModeH1, Priority: 1},
	)
	deviceRemote.SetTransportSettings(clientFromRemote)
	deviceRemote.SetProviderTransportSettings(providerFromRemote)
	clientValues := listener.waitForCount(t, false, 4)
	providerValues := listener.waitForCount(t, true, 4)
	assertTransportSettings(t, clientValues[3], clientFromRemote, false)
	assertTransportSettings(t, providerValues[3], providerFromRemote, true)

	// Reverse RPC payloads are detached from DeviceRemote's cached state.
	clientValues[3].Mode = TransportModeDns
	providerValues[3].Mode = TransportModeDnsPump
	assertTransportSettings(t, deviceRemote.GetTransportSettings(), clientFromRemote, false)
	assertTransportSettings(t, deviceRemote.GetProviderTransportSettings(), providerFromRemote, true)
}
