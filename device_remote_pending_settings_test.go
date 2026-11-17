// A setting write a DeviceRemote queues for the next sync is the remote's
// answer until a sync delivers it (device_remote_setting_state.go, the value
// settings): while the sync that took it is in flight, over the reply of a
// sync that predates it, over a live device's older answer, and over a report
// the device made before it had the write.
//
// The owner's report, 2026-11-17: on the latest TestFlight build post-quantum
// encryption showed off after an app update or a phone restart. The app seeds
// its saved connect options into a new remote and reads them back once
// (apple DeviceManager.initDevice, then setDevice's
// loadPerformanceProfileFromDevice). Opening into a tunnel the update or the
// restart had already started, the first sync that carried them was in flight
// at that read, and the remote answered the default profile.
//
// The orderings are barriers, never timings: a sync held on a channel, the run
// loop's own takeSyncRequest, the production reverse rpc handler, and a fake
// device service. Timers only bound a broken run.
package sdk

import (
	"context"
	"fmt"
	"net"
	"net/rpc"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Connects a remote to a device process over in-memory streams, serving a
// fresh device rpc for each dial. testingHeldSyncDialer
// (device_remote_connect_state_test.go) holds a dial's sync over it.
type testingPipeDeviceRpcDialer struct {
	serve func(serverConn net.Conn, serverReverseConn net.Conn) func()

	stateLock sync.Mutex
	closes    []func()
}

func newTestingPipeDeviceRpcDialer(
	t *testing.T,
	serve func(serverConn net.Conn, serverReverseConn net.Conn) func(),
) *testingPipeDeviceRpcDialer {
	dialer := &testingPipeDeviceRpcDialer{serve: serve}
	t.Cleanup(func() {
		dialer.stateLock.Lock()
		closes := dialer.closes
		dialer.closes = nil
		dialer.stateLock.Unlock()
		for _, close := range closes {
			close()
		}
	})
	return dialer
}

func (self *testingPipeDeviceRpcDialer) Dial(ctx context.Context) (net.Conn, net.Conn, error) {
	serverConn, clientConn := net.Pipe()
	serverReverseConn, clientReverseConn := net.Pipe()
	closeServer := self.serve(serverConn, serverReverseConn)

	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.closes = append(self.closes, func() {
		clientConn.Close()
		clientReverseConn.Close()
		closeServer()
	})
	return clientConn, clientReverseConn, nil
}

// Holds the sync of each of the first count dials over the dialer. A held
// sync's remote has taken its pending state (takeSyncRequest) and has no
// service yet: the sync is in flight until its release, which cleanup also
// runs.
func testingHoldSyncs(t *testing.T, dialer deviceRpcDialer, count int) (*testingHeldSyncDialer, []*testingHeldSyncConn, []func()) {
	helds := []*testingHeldSyncConn{}
	releases := []func(){}
	for range count {
		held := testingNewHeldSyncConn()
		release := sync.OnceFunc(func() { close(held.release) })
		t.Cleanup(release)
		helds = append(helds, held)
		releases = append(releases, release)
	}
	heldDialer := &testingHeldSyncDialer{
		dialer: dialer,
		helds:  append([]*testingHeldSyncConn{}, helds...),
	}
	return heldDialer, helds, releases
}

// Waits for the remote to take its pending state into the held sync.
func testingWaitHeldSync(t *testing.T, held *testingHeldSyncConn, what string) {
	t.Helper()
	select {
	case <-held.entered:
	case <-time.After(15 * time.Second):
		t.Fatalf("the remote never started %s", what)
	}
}

// Serves the production device rpc of a device process for each dial.
func testingServeDeviceLocalRpc(t *testing.T, deviceLocal *DeviceLocal) func(net.Conn, net.Conn) func() {
	return func(serverConn net.Conn, serverReverseConn net.Conn) func() {
		settings := defaultDeviceRpcSettings()
		settings.DisableLogging = true
		server := newDeviceLocalRpc(context.Background(), serverConn, serverReverseConn, deviceLocal, settings)
		return func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := server.CloseAndWait(ctx); err != nil {
				t.Error("the device rpc did not join")
			}
		}
	}
}

// Whether post-quantum encryption is on, as the apps read a profile: a nil
// profile is the default, with every option off.
func testingPostQuantumOn(performanceProfile *PerformanceProfile) bool {
	return performanceProfile != nil && performanceProfile.PostQuantumEncryption
}

// Signs the app's own space in to the extension's account. The app and its
// tunnel extension keep separate local states (apple: separate containers).
func testingSignInAppSpace(t *testing.T, app *testingAuthClientShape, extension *testingAuthClientShape) {
	t.Helper()
	if err := app.localState.SetByJwt(extension.adminJwt); err != nil {
		t.Fatal(err)
	}
	if err := app.localState.SetByClientJwtForInstance(extension.initialJwt, extension.instanceId); err != nil {
		t.Fatal(err)
	}
}

// The app's remote over its own space, reaching the device process through the
// dialer, as apple DeviceManager.initDevice builds it.
func testingAppRemote(t *testing.T, app *testingAuthClientShape, dialer deviceRpcDialer) *DeviceRemote {
	t.Helper()
	auth, err := app.localState.GetAuthStateSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	settings := defaultDeviceRpcSettings()
	settings.DisableLogging = true
	deviceRemote, err := newDeviceRemoteWithOverrides(
		app.networkSpace,
		auth.GetByClientJwt(),
		auth.GetInstanceId(),
		settings,
		connect.NewId(),
		dialer,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(deviceRemote.Close)
	return deviceRemote
}

// The owner's sequence. Before an update or a restart the user turned
// post-quantum encryption on: the app saved it in its own local state and the
// tunnel extension in its own. The update or the restart starts the extension
// first, on demand, which loads its saved profile. Then the app opens: it reads
// its saved profile, builds a remote that reaches the running extension, seeds
// the profile, and reads the profile back once while the first sync is in
// flight. The read must answer the saved profile, and a change of another
// connect option the app makes from what it read must keep post-quantum on in
// both stores.
func TestDeviceRemoteSavedConnectOptionsReadBackAfterAnUpdateOrRestart(t *testing.T) {
	extensionDirectory := t.TempDir()
	appDirectory := t.TempDir()

	// before the update or the restart
	extensionManager, extension := testingPreferenceSpaceAt(t, extensionDirectory)
	extension.seedDistinctLogin(t)
	appManager, app := testingPreferenceSpaceAt(t, appDirectory)
	testingSignInAppSpace(t, app, extension)
	saved := &PerformanceProfile{WindowType: WindowTypeAuto, PostQuantumEncryption: true}
	if err := app.localState.SetPerformanceProfile(saved); err != nil {
		t.Fatal(err)
	}
	before := testingPreferenceDevice(t, extension)
	if err := before.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	before.SetPerformanceProfile(saved)
	testingJoinPreferenceDevice(t, before)
	extensionManager.Close()
	appManager.Close()

	// the extension starts first and loads its own record (PacketTunnelProvider:
	// Load, then autosave, then the rpc)
	_, extensionAfter := testingPreferenceSpaceAt(t, extensionDirectory)
	running := testingPreferenceDevice(t, extensionAfter)
	if _, err := running.Load(); err != nil {
		t.Fatal(err)
	}
	if err := running.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	if !testingPostQuantumOn(running.GetPerformanceProfile()) {
		t.Fatal("the extension did not load its saved profile")
	}

	// the app opens (DeviceManager.initDevice)
	_, appAfter := testingPreferenceSpaceAt(t, appDirectory)
	appSaved := appAfter.localState.GetPerformanceProfile()
	if !testingPostQuantumOn(appSaved) {
		t.Fatal("the app did not read its saved profile")
	}
	dialer, helds, releases := testingHoldSyncs(
		t,
		newTestingPipeDeviceRpcDialer(t, testingServeDeviceLocalRpc(t, running)),
		1,
	)
	deviceRemote := testingAppRemote(t, appAfter, dialer)
	deviceRemote.SetPerformanceProfile(appSaved)

	// setDevice reads the profile once, with the first sync in flight
	testingWaitHeldSync(t, helds[0], "its first sync")
	shown := deviceRemote.GetPerformanceProfile()
	if !testingPostQuantumOn(shown) {
		t.Errorf("with the first sync in flight the app read %+v, not its saved profile", shown)
	}
	releases[0]()
	if !deviceRemote.waitForSync(15 * time.Second) {
		t.Fatal("the first sync did not complete")
	}

	// the user changes the connection mode: the app builds the profile from
	// the options it shows (DeviceManager.createPerformanceProfile)
	changed := &PerformanceProfile{
		WindowType: WindowTypeQuality,
		WindowSize: &WindowSizeSettings{WindowSizeMin: 2, WindowSizeMax: 4},
	}
	if shown != nil {
		changed.AllowDirect = shown.AllowDirect
		changed.PostQuantumEncryption = shown.PostQuantumEncryption
	}
	if err := appAfter.localState.SetPerformanceProfile(changed); err != nil {
		t.Fatal(err)
	}
	deviceRemote.SetPerformanceProfile(changed)

	if !testingPostQuantumOn(appAfter.localState.GetPerformanceProfile()) {
		t.Error("changing the connection mode saved post-quantum off in the app's local state")
	}
	if !testingPostQuantumOn(running.GetPerformanceProfile()) {
		t.Error("changing the connection mode turned post-quantum off in the running extension")
	}
	if !testingPostQuantumOn(extensionAfter.localState.GetPerformanceProfile()) {
		t.Error("changing the connection mode saved post-quantum off in the extension's local state")
	}
	if got := deviceRemote.GetPerformanceProfile(); got == nil || got.WindowType != WindowTypeQuality {
		t.Errorf("the remote reads %+v, not the changed connection mode", got)
	}
}

// A profile the app writes while a sync is in flight waits for the next sync.
// That sync's reply predates it, so it stays the remote's answer over the
// reply and while the next sync that takes it is in flight.
func TestDeviceRemoteLaterProfileWriteWaitsForTheNextSync(t *testing.T) {
	_, extension := testingPreferenceSpaceAt(t, t.TempDir())
	extension.seedDistinctLogin(t)
	_, app := testingPreferenceSpaceAt(t, t.TempDir())
	testingSignInAppSpace(t, app, extension)
	running := testingPreferenceDevice(t, extension)
	if err := running.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}

	dialer, helds, releases := testingHoldSyncs(
		t,
		newTestingPipeDeviceRpcDialer(t, testingServeDeviceLocalRpc(t, running)),
		2,
	)
	deviceRemote := testingAppRemote(t, app, dialer)

	speed := &PerformanceProfile{WindowType: WindowTypeSpeed, WindowSize: &WindowSizeSettings{WindowSizeMin: 2, WindowSizeMax: 4}}
	deviceRemote.SetPerformanceProfile(speed)
	testingWaitHeldSync(t, helds[0], "its first sync")

	// written while the first sync is in flight: it waits for the next one
	postQuantum := &PerformanceProfile{WindowType: WindowTypeAuto, PostQuantumEncryption: true}
	deviceRemote.SetPerformanceProfile(postQuantum)
	releases[0]()

	// the first sync's reply lands (speed), and the next sync takes the write
	testingWaitHeldSync(t, helds[1], "the sync that takes the later write")
	if got := deviceRemote.GetPerformanceProfile(); !testingPostQuantumOn(got) || got.WindowType != WindowTypeAuto {
		t.Errorf("with the later write's sync in flight the remote reads %+v, not the later write", got)
	}
	releases[1]()
	if !deviceRemote.waitForSync(15 * time.Second) {
		t.Fatal("the sync did not complete")
	}
	if got := running.GetPerformanceProfile(); !testingPostQuantumOn(got) || got.WindowType != WindowTypeAuto {
		t.Errorf("the device holds %+v, not the later write", got)
	}
}

// One value setting of a DeviceRemote, for the orderings every one runs
// through. A value is spelled as a string; the device process holds the old
// value, and the test writes the new one, which no default spells.
type testingValueSetting struct {
	name     string
	oldValue string
	newValue string
	write    func(deviceRemote *DeviceRemote, value string)
	get      func(deviceRemote *DeviceRemote) string
	// sets the device's value in a sync reply
	reply func(state *DeviceRemoteState, value string)
	// the device's report through the production reverse rpc handler, or nil
	// for a setting the device never reports
	report func(deviceRemoteRpc *DeviceRemoteRpc, value string) error
}

func testingValueSettingProfile(value string) *PerformanceProfile {
	return &PerformanceProfile{WindowType: WindowTypeAuto, PostQuantumEncryption: value == "pq"}
}

func testingValueSettingLocation(value string) *ConnectLocation {
	return &ConnectLocation{
		ConnectLocationId: &ConnectLocationId{LocationId: NewId()},
		Name:              value,
	}
}

func testingValueSettingSplitRules(value string) *BlockActionOverrideList {
	return newSavedSettingsTestSplitRules(newSavedSettingsTestSplitRule(value, true))
}

func testingValueSettingSplitRulesValue(overrides *BlockActionOverrideList) string {
	if overrides == nil {
		return "none"
	}
	hosts := []string{}
	for _, override := range overrides.getAll() {
		hosts = append(hosts, stringsFromStringList(override.Hosts)...)
	}
	return strings.Join(hosts, ",")
}

func testingValueSettingDns(value string) *DnsResolverSettings {
	dnsResolverSettings := GetDefaultDnsResolverSettings()
	dnsResolverSettings.EnableFallback = value == "fallback"
	return dnsResolverSettings
}

func testingValueSettingDnsValue(dnsResolverSettings *DnsResolverSettings) string {
	if dnsResolverSettings != nil && dnsResolverSettings.EnableFallback {
		return "fallback"
	}
	return "tunnel"
}

func testingValueSettingTransportValue(transportSettings *TransportSettings) string {
	if transportSettings == nil {
		return "none"
	}
	return string(transportSettings.Mode)
}

// A setting whose value is a bool.
func testingBoolValueSetting(
	name string,
	oldValue bool,
	write func(*DeviceRemote, bool),
	get func(*DeviceRemote) bool,
	reply func(*DeviceRemoteState, bool),
	report func(*DeviceRemoteRpc, bool) error,
) *testingValueSetting {
	setting := &testingValueSetting{
		name:     name,
		oldValue: fmt.Sprintf("%t", oldValue),
		newValue: fmt.Sprintf("%t", !oldValue),
		write: func(deviceRemote *DeviceRemote, value string) {
			write(deviceRemote, value == "true")
		},
		get: func(deviceRemote *DeviceRemote) string {
			return fmt.Sprintf("%t", get(deviceRemote))
		},
		reply: func(state *DeviceRemoteState, value string) {
			reply(state, value == "true")
		},
	}
	if report != nil {
		setting.report = func(deviceRemoteRpc *DeviceRemoteRpc, value string) error {
			return report(deviceRemoteRpc, value == "true")
		}
	}
	return setting
}

// Every value setting. Each old value is the remote's default, so an answer
// that falls back to the default is the old value.
func testingValueSettings() []*testingValueSetting {
	return []*testingValueSetting{
		{
			name:     "PerformanceProfile",
			oldValue: "classical",
			newValue: "pq",
			write: func(deviceRemote *DeviceRemote, value string) {
				deviceRemote.SetPerformanceProfile(testingValueSettingProfile(value))
			},
			get: func(deviceRemote *DeviceRemote) string {
				if testingPostQuantumOn(deviceRemote.GetPerformanceProfile()) {
					return "pq"
				}
				return "classical"
			},
			reply: func(state *DeviceRemoteState, value string) {
				state.PerformanceProfile.Set(testingValueSettingProfile(value))
			},
			report: func(deviceRemoteRpc *DeviceRemoteRpc, value string) error {
				return deviceRemoteRpc.PerformanceProfileChanged(&DevicePerformanceProfile{
					PerformanceProfile: testingValueSettingProfile(value),
				}, nil)
			},
		},
		{
			name:     "ProvideNetworkMode",
			oldValue: ProvideNetworkModeWiFi,
			newValue: ProvideNetworkModeAll,
			write: func(deviceRemote *DeviceRemote, value string) {
				deviceRemote.SetProvideNetworkMode(value)
			},
			get: func(deviceRemote *DeviceRemote) string {
				return deviceRemote.GetProvideNetworkMode()
			},
			reply: func(state *DeviceRemoteState, value string) {
				state.ProvideNetworkMode.Set(value)
			},
			report: func(deviceRemoteRpc *DeviceRemoteRpc, value string) error {
				return deviceRemoteRpc.ProvideNetworkModeChanged(value, nil)
			},
		},
		testingBoolValueSetting(
			"CanShowRatingDialog",
			true,
			(*DeviceRemote).SetCanShowRatingDialog,
			(*DeviceRemote).GetCanShowRatingDialog,
			func(state *DeviceRemoteState, value bool) { state.CanShowRatingDialog.Set(value) },
			func(deviceRemoteRpc *DeviceRemoteRpc, value bool) error {
				return deviceRemoteRpc.CanShowRatingDialogChanged(value, nil)
			},
		),
		testingBoolValueSetting(
			"CanPromptIntroFunnel",
			true,
			(*DeviceRemote).SetCanPromptIntroFunnel,
			(*DeviceRemote).GetCanPromptIntroFunnel,
			func(state *DeviceRemoteState, value bool) { state.CanPromptIntroFunnel.Set(value) },
			func(deviceRemoteRpc *DeviceRemoteRpc, value bool) error {
				return deviceRemoteRpc.CanPromptIntroFunnelChanged(value, nil)
			},
		),
		testingBoolValueSetting(
			"CanRefer",
			false,
			(*DeviceRemote).SetCanRefer,
			(*DeviceRemote).GetCanRefer,
			func(state *DeviceRemoteState, value bool) { state.CanRefer.Set(value) },
			func(deviceRemoteRpc *DeviceRemoteRpc, value bool) error {
				return deviceRemoteRpc.CanReferChanged(value, nil)
			},
		),
		testingBoolValueSetting(
			"AllowForeground",
			false,
			(*DeviceRemote).SetAllowForeground,
			(*DeviceRemote).GetAllowForeground,
			func(state *DeviceRemoteState, value bool) { state.AllowForeground.Set(value) },
			func(deviceRemoteRpc *DeviceRemoteRpc, value bool) error {
				return deviceRemoteRpc.AllowForegroundChanged(value, nil)
			},
		),
		{
			name:     "DefaultLocation",
			oldValue: "none",
			newValue: "chosen-location",
			write: func(deviceRemote *DeviceRemote, value string) {
				deviceRemote.SetDefaultLocation(testingValueSettingLocation(value))
			},
			get: func(deviceRemote *DeviceRemote) string {
				if location := deviceRemote.GetDefaultLocation(); location != nil {
					return location.Name
				}
				return "none"
			},
			reply: func(state *DeviceRemoteState, value string) {
				if value == "none" {
					state.DefaultLocation.Set(newDeviceRemoteDefaultLocation(nil))
				} else {
					state.DefaultLocation.Set(newDeviceRemoteDefaultLocation(testingValueSettingLocation(value)))
				}
			},
			report: func(deviceRemoteRpc *DeviceRemoteRpc, value string) error {
				if value == "none" {
					return deviceRemoteRpc.DefaultLocationChanged(newDeviceRemoteDefaultLocation(nil), nil)
				}
				return deviceRemoteRpc.DefaultLocationChanged(newDeviceRemoteDefaultLocation(testingValueSettingLocation(value)), nil)
			},
		},
		testingBoolValueSetting(
			"ProvideExtender",
			true,
			(*DeviceRemote).SetProvideExtender,
			(*DeviceRemote).GetProvideExtender,
			func(state *DeviceRemoteState, value bool) { state.ProvideExtender.Set(value) },
			nil,
		),
		testingBoolValueSetting(
			"MeasurementsEnabled",
			true,
			(*DeviceRemote).SetMeasurementsEnabled,
			(*DeviceRemote).GetMeasurementsEnabled,
			func(state *DeviceRemoteState, value bool) { state.MeasurementsEnabled.Set(value) },
			nil,
		),
		testingBoolValueSetting(
			"ProvideExtenderPortMapping",
			false,
			(*DeviceRemote).SetProvideExtenderPortMapping,
			(*DeviceRemote).GetProvideExtenderPortMapping,
			func(state *DeviceRemoteState, value bool) { state.ProvideExtenderPortMapping.Set(value) },
			nil,
		),
		{
			name:     "TransportSettings",
			oldValue: string(TransportModeH1),
			newValue: string(TransportModeH3),
			write: func(deviceRemote *DeviceRemote, value string) {
				deviceRemote.SetTransportSettings(&TransportSettings{Mode: TransportMode(value)})
			},
			get: func(deviceRemote *DeviceRemote) string {
				return testingValueSettingTransportValue(deviceRemote.GetTransportSettings())
			},
			reply: func(state *DeviceRemoteState, value string) {
				state.TransportSettings.Set(newTransportSettingsRpc(&TransportSettings{Mode: TransportMode(value)}, false))
			},
			report: func(deviceRemoteRpc *DeviceRemoteRpc, value string) error {
				return deviceRemoteRpc.TransportSettingsChanged(&DeviceRemoteTransportSettingsRpc{
					TransportSettings: newTransportSettingsRpc(&TransportSettings{Mode: TransportMode(value)}, false),
				}, nil)
			},
		},
		{
			name:     "ProviderTransportSettings",
			oldValue: string(TransportModeH1),
			newValue: string(TransportModeH3),
			write: func(deviceRemote *DeviceRemote, value string) {
				deviceRemote.SetProviderTransportSettings(&TransportSettings{Mode: TransportMode(value)})
			},
			get: func(deviceRemote *DeviceRemote) string {
				return testingValueSettingTransportValue(deviceRemote.GetProviderTransportSettings())
			},
			reply: func(state *DeviceRemoteState, value string) {
				state.ProviderTransportSettings.Set(newTransportSettingsRpc(&TransportSettings{Mode: TransportMode(value)}, true))
			},
			report: func(deviceRemoteRpc *DeviceRemoteRpc, value string) error {
				return deviceRemoteRpc.ProviderTransportSettingsChanged(&DeviceRemoteTransportSettingsRpc{
					TransportSettings: newTransportSettingsRpc(&TransportSettings{Mode: TransportMode(value)}, true),
				}, nil)
			},
		},
		{
			name:     "BlockActionOverrides",
			oldValue: "old.example",
			newValue: "new.example",
			write: func(deviceRemote *DeviceRemote, value string) {
				deviceRemote.SetBlockActionOverrides(testingValueSettingSplitRules(value))
			},
			get: func(deviceRemote *DeviceRemote) string {
				return testingValueSettingSplitRulesValue(deviceRemote.GetBlockActionOverrides())
			},
			reply: func(state *DeviceRemoteState, value string) {
				state.BlockActionOverrides.Set(newBlockActionOverridesRpc(testingValueSettingSplitRules(value)))
			},
			report: func(deviceRemoteRpc *DeviceRemoteRpc, value string) error {
				return deviceRemoteRpc.BlockActionOverridesChanged(&DeviceRemoteBlockActionOverrides{
					BlockActionOverrides: newBlockActionOverridesRpc(testingValueSettingSplitRules(value)),
				}, nil)
			},
		},
		{
			name:     "DnsResolverSettings",
			oldValue: "tunnel",
			newValue: "fallback",
			write: func(deviceRemote *DeviceRemote, value string) {
				deviceRemote.SetDnsResolverSettings(testingValueSettingDns(value))
			},
			get: func(deviceRemote *DeviceRemote) string {
				return testingValueSettingDnsValue(deviceRemote.GetDnsResolverSettings())
			},
			reply: func(state *DeviceRemoteState, value string) {
				state.DnsResolverSettings.Set(newDnsResolverSettingsRpc(testingValueSettingDns(value)))
			},
			report: func(deviceRemoteRpc *DeviceRemoteRpc, value string) error {
				return deviceRemoteRpc.DnsResolverSettingsChanged(&DeviceRemoteDnsResolverSettings{
					DnsResolverSettings: newDnsResolverSettingsRpc(testingValueSettingDns(value)),
				}, nil)
			},
		},
	}
}

// A device process that holds the old value of every value setting: it
// answers the getters and every sync with them, as a device whose state
// predates the remote's writes does.
type testingValueSettingsFakeDevice struct {
	settings []*testingValueSetting
}

func (self *testingValueSettingsFakeDevice) oldState() DeviceRemoteState {
	state := DeviceRemoteState{}
	for _, setting := range self.settings {
		setting.reply(&state, setting.oldValue)
	}
	return state
}

func (self *testingValueSettingsFakeDevice) Sync(syncRequest *DeviceRemoteSyncRequest, syncResponse **DeviceRemoteSyncResponse) error {
	*syncResponse = &DeviceRemoteSyncResponse{State: self.oldState()}
	return nil
}

func (self *testingValueSettingsFakeDevice) SyncReverse(_ RpcNoArg, _ RpcVoid) error {
	return nil
}

func (self *testingValueSettingsFakeDevice) GetPerformanceProfile(_ RpcNoArg, devicePerformanceProfile **DevicePerformanceProfile) error {
	state := self.oldState()
	*devicePerformanceProfile = &DevicePerformanceProfile{PerformanceProfile: state.PerformanceProfile.Value}
	return nil
}

func (self *testingValueSettingsFakeDevice) GetProvideNetworkMode(_ RpcNoArg, provideNetworkMode *ProvideNetworkMode) error {
	*provideNetworkMode = self.oldState().ProvideNetworkMode.Value
	return nil
}

func (self *testingValueSettingsFakeDevice) GetCanShowRatingDialog(_ RpcNoArg, canShowRatingDialog *bool) error {
	*canShowRatingDialog = self.oldState().CanShowRatingDialog.Value
	return nil
}

func (self *testingValueSettingsFakeDevice) GetCanPromptIntroFunnel(_ RpcNoArg, canPromptIntroFunnel *bool) error {
	*canPromptIntroFunnel = self.oldState().CanPromptIntroFunnel.Value
	return nil
}

func (self *testingValueSettingsFakeDevice) GetCanRefer(_ RpcNoArg, canRefer *bool) error {
	*canRefer = self.oldState().CanRefer.Value
	return nil
}

func (self *testingValueSettingsFakeDevice) GetAllowForeground(_ RpcNoArg, allowForeground *bool) error {
	*allowForeground = self.oldState().AllowForeground.Value
	return nil
}

func (self *testingValueSettingsFakeDevice) GetDefaultLocation(_ RpcNoArg, location **DeviceRemoteConnectLocation) error {
	*location = self.oldState().DefaultLocation.Value
	return nil
}

func (self *testingValueSettingsFakeDevice) GetProvideExtender(_ RpcNoArg, provideExtender *bool) error {
	*provideExtender = self.oldState().ProvideExtender.Value
	return nil
}

func (self *testingValueSettingsFakeDevice) GetMeasurementsEnabled(_ RpcNoArg, measurementsEnabled *bool) error {
	*measurementsEnabled = self.oldState().MeasurementsEnabled.Value
	return nil
}

func (self *testingValueSettingsFakeDevice) GetProvideExtenderPortMapping(_ RpcNoArg, portMapping *bool) error {
	*portMapping = self.oldState().ProvideExtenderPortMapping.Value
	return nil
}

func (self *testingValueSettingsFakeDevice) GetTransportSettings(_ RpcNoArg, deviceSettings **DeviceRemoteTransportSettingsRpc) error {
	*deviceSettings = &DeviceRemoteTransportSettingsRpc{TransportSettings: self.oldState().TransportSettings.Value}
	return nil
}

func (self *testingValueSettingsFakeDevice) GetProviderTransportSettings(_ RpcNoArg, deviceSettings **DeviceRemoteTransportSettingsRpc) error {
	*deviceSettings = &DeviceRemoteTransportSettingsRpc{TransportSettings: self.oldState().ProviderTransportSettings.Value}
	return nil
}

func (self *testingValueSettingsFakeDevice) GetBlockActionOverrides(_ RpcNoArg, deviceOverrides **DeviceRemoteBlockActionOverrides) error {
	*deviceOverrides = &DeviceRemoteBlockActionOverrides{BlockActionOverrides: self.oldState().BlockActionOverrides.Value}
	return nil
}

func (self *testingValueSettingsFakeDevice) GetDnsResolverSettings(_ RpcNoArg, deviceSettings **DeviceRemoteDnsResolverSettings) error {
	*deviceSettings = &DeviceRemoteDnsResolverSettings{DnsResolverSettings: self.oldState().DnsResolverSettings.Value}
	return nil
}

// Serves the fake device as the device rpc for each dial.
func testingServeValueSettingsFakeDevice(t *testing.T, device *testingValueSettingsFakeDevice) func(net.Conn, net.Conn) func() {
	return func(serverConn net.Conn, serverReverseConn net.Conn) func() {
		rpcServer := rpc.NewServer()
		if err := rpcServer.RegisterName("DeviceLocalRpc", device); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			rpcServer.ServeConn(serverConn)
		}()
		return func() {
			serverConn.Close()
			serverReverseConn.Close()
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				t.Error("the fake device rpc did not join")
			}
		}
	}
}

// Reports whether the getter answers the value, and fails the test with the
// setting's name otherwise.
func testingValueSettingGets(t *testing.T, setting *testingValueSetting, deviceRemote *DeviceRemote, value string, when string) bool {
	t.Helper()
	if got := setting.get(deviceRemote); got != value {
		t.Errorf("%s: %s the getter answers %s, want %s", setting.name, when, got, value)
		return false
	}
	return true
}

// A write the remote queued for the first sync is its answer while that sync
// is in flight: the run loop took it (takeSyncRequest), and the remote has no
// device value yet.
func TestDeviceRemoteQueuedWriteIsTheAnswerWhileItsSyncIsInFlight(t *testing.T) {
	for _, setting := range testingValueSettings() {
		deviceRemote := testingConnectStateRemoteWithoutListeners(t)
		setting.write(deviceRemote, setting.newValue)
		if !testingValueSettingGets(t, setting, deviceRemote, setting.newValue, "before the sync") {
			continue
		}
		syncRequest := deviceRemote.takeSyncRequest()
		if !syncRequest.State.hasPendingSyncState() {
			t.Errorf("%s: the sync took no write", setting.name)
			continue
		}
		testingValueSettingGets(t, setting, deviceRemote, setting.newValue, "with the sync that took the write in flight")
	}
}

// Writes made while a sync is in flight wait for the next sync. The reply of
// the sync in flight predates them, so they stay the remote's answer over it
// and while the next sync takes them. Drives the remote's own run loop
// against a device whose every reply holds the old values.
func TestDeviceRemoteWritesQueuedDuringASyncStayTheAnswerOverItsReply(t *testing.T) {
	settings := testingValueSettings()
	device := &testingValueSettingsFakeDevice{settings: settings}
	dialer, helds, releases := testingHoldSyncs(
		t,
		newTestingPipeDeviceRpcDialer(t, testingServeValueSettingsFakeDevice(t, device)),
		2,
	)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rpcSettings := defaultDeviceRpcSettings()
	rpcSettings.DisableLogging = true
	deviceRemote, err := newDeviceRemoteWithOverrides(networkSpace, byJwt, NewId(), rpcSettings, connect.NewId(), dialer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(deviceRemote.Close)

	testingWaitHeldSync(t, helds[0], "its first sync")
	for _, setting := range settings {
		setting.write(deviceRemote, setting.newValue)
	}
	releases[0]()

	// the first reply (the old values) has landed and the next sync took the
	// writes
	testingWaitHeldSync(t, helds[1], "the sync that takes the writes")
	for _, setting := range settings {
		testingValueSettingGets(t, setting, deviceRemote, setting.newValue, "over an older reply, with the sync that took the write in flight")
	}
}

// A pending write is the remote's answer before the device's: a live device
// that still holds the old value answers with a state the write replaces at
// the next sync.
func TestDeviceRemotePendingWriteIsTheAnswerOverALiveDevice(t *testing.T) {
	settings := testingValueSettings()
	for _, setting := range settings {
		deviceRemote := testingConnectStateRemoteWithoutListeners(t)
		setting.write(deviceRemote, setting.newValue)
		// a sync that started before the write completed: the device is live
		// and holds the old value, and the write is still pending
		testingInstallFakeDeviceService(t, deviceRemote, &testingValueSettingsFakeDevice{settings: settings})
		testingValueSettingGets(t, setting, deviceRemote, setting.newValue, "with a live device holding the old value")
	}
}

// Delivers a device report through the production reverse rpc handler and
// returns once the remote has handled it.
func testingReportValueSetting(t *testing.T, deviceRemote *DeviceRemote, setting *testingValueSetting, value string) {
	t.Helper()
	deviceRemoteRpc := newDeviceRemoteRpc(context.Background(), deviceRemote)
	defer deviceRemoteRpc.Close()

	if err := setting.report(deviceRemoteRpc, value); err != nil {
		t.Fatal(err)
	}
	handled := make(chan struct{})
	deviceRemoteRpc.dispatch(func() {
		close(handled)
	})
	select {
	case <-handled:
	case <-time.After(15 * time.Second):
		t.Fatalf("%s: the reverse report was not handled", setting.name)
	}
}

// A report the device made before it had a pending write, as its replay to a
// new connection is, predates the write: the next sync applies the write over
// it. It does not replace the write as the remote's answer, also once the next
// sync took the write.
func TestDeviceRemoteDropsADeviceReportThatPredatesAPendingWrite(t *testing.T) {
	for _, setting := range testingValueSettings() {
		if setting.report == nil {
			continue
		}
		deviceRemote := testingConnectStateRemoteWithoutListeners(t)
		setting.write(deviceRemote, setting.newValue)
		testingReportValueSetting(t, deviceRemote, setting, setting.oldValue)
		if !testingValueSettingGets(t, setting, deviceRemote, setting.newValue, "after a report that predates the write") {
			continue
		}
		deviceRemote.takeSyncRequest()
		testingValueSettingGets(t, setting, deviceRemote, setting.newValue, "after a report that predates the write, with the sync that took it in flight")
	}
}

// Records the profiles its listeners hear.
type testingProfileRecorder struct {
	stateLock sync.Mutex
	profiles  []string
}

func (self *testingProfileRecorder) PerformanceProfileChanged(performanceProfile *PerformanceProfile) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if testingPostQuantumOn(performanceProfile) {
		self.profiles = append(self.profiles, "pq")
	} else {
		self.profiles = append(self.profiles, "classical")
	}
}

func (self *testingProfileRecorder) get() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string{}, self.profiles...)
}

// A profile listener (the linux app redraws its connect options from one) is
// not told a report that predates a pending write, and is told the device's
// own change once no write is pending.
func TestDeviceRemoteProfileListenerSkipsAReportThatPredatesAPendingWrite(t *testing.T) {
	setting := testingValueSettings()[0]
	if setting.name != "PerformanceProfile" {
		t.Fatalf("the first value setting is %s", setting.name)
	}
	deviceRemote := testingConnectStateRemoteWithoutListeners(t)
	recorder := &testingProfileRecorder{}
	sub := deviceRemote.AddPerformanceProfileChangeListener(recorder)
	t.Cleanup(sub.Close)

	deviceRemote.SetPerformanceProfile(testingValueSettingProfile("pq"))
	testingReportValueSetting(t, deviceRemote, setting, "classical")
	if heard := recorder.get(); len(heard) != 0 {
		t.Errorf("the listener heard %v, a report that predates the pending write", heard)
	}

	// the sync delivered the write; the device's later change is its own
	deviceRemote.takeSyncRequest()
	testingReportValueSetting(t, deviceRemote, setting, "classical")
	if heard := recorder.get(); len(heard) != 1 || heard[0] != "classical" {
		t.Errorf("the listener heard %v, want the device's own change", heard)
	}
}
