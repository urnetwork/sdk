package sdk

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The reliability bridge round trips. Wire fidelity follows the mirror-test
// convention (fill every field non-zero, cross the gob wire, compare), and
// the live tests run a DeviceRemote against a DeviceLocal whose multi client
// is built from the stub generator, so the settings round trip is observable
// end to end: remote -> rpc -> DeviceLocal -> connect multi client -> back.
//
// These are ordinary build-tag-free tests in the default compile set, and
// run on Windows as well as in the Linux sdk CI.

// gob wire fidelity for the whole-struct payloads

func TestRpcGobReliabilitySettingsComplete(t *testing.T) {
	seed := 0
	reliabilitySettings := &ReliabilitySettings{}
	fillNonZero(t, reflect.ValueOf(reliabilitySettings), &seed)

	wired := gobRoundTrip(t, &DeviceRemoteReliabilitySettingsRpc{
		ReliabilitySettings: reliabilitySettings,
	})
	connect.AssertEqual(t, wired.ReliabilitySettings, reliabilitySettings)
}

func TestRpcGobReliabilityMetricsComplete(t *testing.T) {
	seed := 0
	reliabilityMetrics := &ReliabilityMetrics{}
	fillNonZero(t, reflect.ValueOf(reliabilityMetrics), &seed)

	wired := gobRoundTrip(t, &DeviceRemoteReliabilityMetricsRpc{
		ReliabilityMetrics: reliabilityMetrics,
	})
	connect.AssertEqual(t, wired.ReliabilityMetrics, reliabilityMetrics)
}

// hand-mirror completeness (Exit and DestinationExit carry an *Id whose uuid
// bytes are unexported; a forgotten mirror field would be dropped silently)

func TestRpcMirrorExitComplete(t *testing.T) {
	seed := 0
	exit := &Exit{}
	fillNonZero(t, reflect.ValueOf(exit), &seed)

	wired := gobRoundTrip(t, newExitRpc(exit))
	connect.AssertEqual(t, wired.toExit(), exit)
}

func TestRpcMirrorDestinationExitComplete(t *testing.T) {
	seed := 0
	destinationExit := &DestinationExit{}
	fillNonZero(t, reflect.ValueOf(destinationExit), &seed)

	wired := gobRoundTrip(t, newDestinationExitRpc(destinationExit))
	connect.AssertEqual(t, wired.toDestinationExit(), destinationExit)
}

// populated list payloads across the wire (empty is covered live below)

func TestRpcMirrorExitListPopulated(t *testing.T) {
	seed := 0
	exits := NewExitList()
	sourceExits := []*Exit{}
	for range 3 {
		exit := &Exit{}
		fillNonZero(t, reflect.ValueOf(exit), &seed)
		exits.Add(exit)
		sourceExits = append(sourceExits, exit)
	}
	// a nil row must be skipped, not crash the encoder
	exits.Add(nil)

	wired := gobRoundTrip(t, &DeviceRemoteExitListRpc{
		Exits: newExitListRpc(exits),
	})
	exitList := toExitList(wired.Exits)
	connect.AssertEqual(t, exitList.Len(), 3)
	for i, exit := range sourceExits {
		connect.AssertEqual(t, exitList.Get(i), exit)
	}
}

func TestRpcMirrorDestinationExitListPopulated(t *testing.T) {
	seed := 0
	destinationExits := NewDestinationExitList()
	sourceDestinationExits := []*DestinationExit{}
	for range 3 {
		destinationExit := &DestinationExit{}
		fillNonZero(t, reflect.ValueOf(destinationExit), &seed)
		destinationExits.Add(destinationExit)
		sourceDestinationExits = append(sourceDestinationExits, destinationExit)
	}
	destinationExits.Add(nil)

	wired := gobRoundTrip(t, &DeviceRemoteDestinationExitListRpc{
		DestinationExits: newDestinationExitListRpc(destinationExits),
	})
	destinationExitList := toDestinationExitList(wired.DestinationExits)
	connect.AssertEqual(t, destinationExitList.Len(), 3)
	for i, destinationExit := range sourceDestinationExits {
		connect.AssertEqual(t, destinationExitList.Get(i), destinationExit)
	}
}

// the site-pin route override crosses the existing block-action bridge; the
// `Pin` mode must survive the payload conversion and the gob wire in both
// the single-override and the synced-list forms (the WP3 site pinning path)

func TestRpcBlockActionOverridePinRouteOverride(t *testing.T) {
	override := &BlockActionOverride{
		OverrideId:    NewId(),
		Hosts:         stringListFromStrings([]string{"example.com"}),
		BlockOverride: &BlockOverride{Block: false},
		RouteOverride: &RouteOverride{Local: false, Pin: true},
	}

	wired := gobRoundTrip(t, newBlockActionOverrideRpc(override))
	roundTripped := wired.toBlockActionOverride()
	connect.AssertEqual(t, roundTripped.RouteOverride.Pin, true)
	connect.AssertEqual(t, roundTripped.RouteOverride.Local, false)

	// the sync-state list form (what a reconnect re-applies)
	overrides := NewBlockActionOverrideList()
	overrides.Add(override)
	wiredList := gobRoundTrip(t, newBlockActionOverridesRpc(overrides))
	roundTrippedList := toBlockActionOverrideList(wiredList)
	connect.AssertEqual(t, roundTrippedList.Len(), 1)
	connect.AssertEqual(t, roundTrippedList.Get(0).RouteOverride.Pin, true)
}

// TestReliabilitySettingsNilWithoutMultiClient pins the zero-value-off trap
// shut at every layer of the settings path.
//
// Settings is a read-modify-WRITE surface: a caller reads the effective
// settings, changes one field, and writes the WHOLE struct back. If any
// layer answers a disconnected read with a zero struct, that loop turns one
// toggle into an override with every reliability fix off -- and on ios the
// override then outlives the disconnected moment, because the DeviceRemote
// exists from login and its queued override is re-applied when the extension
// comes up. nil is what makes the disconnected read unusable by construction.
//
// The nil-able contract is settings-only and deliberately asymmetric:
// metrics and the list readouts still degrade, since zeros and empty lists
// are honest answers there and are not written back to anything.
func TestReliabilitySettingsNilWithoutMultiClient(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	connect.AssertEqual(t, err, nil)

	// the DeviceLocal layer (shared with android): no multi client, no
	// effective config to report
	settings := DefaultDeviceLocalSettings()
	settings.DisableLogging = true
	settings.AllowProvider = false
	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace,
		byJwt,
		"",
		"",
		"",
		NewId(),
		settings,
		connect.NewId(),
	)
	connect.AssertEqual(t, err, nil)
	defer deviceLocal.Close()

	connect.AssertEqual(t, deviceLocal.GetReliabilitySettings(), nil)
	// the writes a disconnected caller might still make are safe no-ops
	deviceLocal.SetReliabilitySettings(&ReliabilitySettings{})
	deviceLocal.ResetReliabilitySettings()
	connect.AssertEqual(t, deviceLocal.GetReliabilitySettings(), nil)

	// the readouts on the same disconnected device still degrade, never nil
	connect.AssertNotEqual(t, deviceLocal.GetReliabilityMetrics(), nil)
	connect.AssertEqual(t, deviceLocal.GetExits().Len(), 0)
	connect.AssertEqual(t, deviceLocal.GetDestinationExits().Len(), 0)

	// the DeviceRemote layer with no service at all (the ios cold launch:
	// the remote exists from login, the extension is not up)
	deviceRemote := &DeviceRemote{}
	connect.AssertEqual(t, deviceRemote.GetReliabilitySettings(), nil)

	// a queued RESET carries no values, so it must not read back as a struct
	deviceRemote.state.ReliabilitySettings.Set(nil)
	connect.AssertEqual(t, deviceRemote.GetReliabilitySettings(), nil)

	// last known state from a local that had no multi client is nil too: the
	// fallback must not resurrect settings the local stopped reporting
	deviceRemote.state.ReliabilitySettings.Unset()
	deviceRemote.lastKnownState.ReliabilitySettings.Set(nil)
	connect.AssertEqual(t, deviceRemote.GetReliabilitySettings(), nil)

	// and a nil argument can never become a queued override
	deviceRemote.SetReliabilitySettings(nil)
	queued := deviceRemote.state.ReliabilitySettings
	connect.AssertEqual(t, queued.IsSet, false)
	connect.AssertEqual(t, deviceRemote.GetReliabilitySettings(), nil)

	// the remote readouts still degrade with no service, never nil
	connect.AssertNotEqual(t, deviceRemote.GetReliabilityMetrics(), nil)
	connect.AssertEqual(t, deviceRemote.GetExits().Len(), 0)
	connect.AssertEqual(t, deviceRemote.GetDestinationExits().Len(), 0)
}

// TestRpcGobReliabilitySettingsNilCrossesWire covers the two directions a
// nil settings value travels: the payload wrapper (the local reporting "no
// multi client") and the sync state (the remote queueing a reset). gob omits
// nil pointer fields, so both must decode back to nil rather than a zero
// struct -- a zero struct arriving as an override is the trap by another
// route.
func TestRpcGobReliabilitySettingsNilCrossesWire(t *testing.T) {
	wired := gobRoundTrip(t, &DeviceRemoteReliabilitySettingsRpc{ReliabilitySettings: nil})
	connect.AssertEqual(t, wired.ReliabilitySettings, nil)

	state := DeviceRemoteState{}
	state.ReliabilitySettings.Set(nil)
	wiredState := gobRoundTrip(t, state)
	connect.AssertEqual(t, wiredState.ReliabilitySettings.IsSet, true)
	connect.AssertEqual(t, wiredState.ReliabilitySettings.Value, nil)
}

// live rpc helpers

func testingReliabilityWaitFor(t *testing.T, description string, cond func() bool) {
	endTime := time.Now().Add(30 * time.Second)
	for !cond() {
		if endTime.Before(time.Now()) {
			t.Fatalf("timeout waiting for %s", description)
		}
		select {
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// testingNewReliabilityDeviceLocal builds an rpc-enabled DeviceLocal whose
// multi client comes from the stub generator, so `SetConnectLocation` gives
// it a real `connect.RemoteUserNatMultiClient` without any network. Retries
// briefly so a restart test does not race the previous local releasing the
// rpc port.
func testingNewReliabilityDeviceLocal(
	t *testing.T,
	networkSpace *NetworkSpace,
	byJwt string,
	instanceId *Id,
	clientId connect.Id,
) *DeviceLocal {
	endTime := time.Now().Add(15 * time.Second)
	for {
		settings := testDeviceLocalSettingsRpc()
		settings.DisableLogging = true
		settings.AllowProvider = false
		settings.GeneratorFunc = func(specs []*connect.ProviderSpec) connect.MultiClientGenerator {
			return &rpcLeakTestGenerator{}
		}
		deviceLocal, err := newDeviceLocalWithOverrides(
			networkSpace,
			byJwt,
			"",
			"",
			"",
			instanceId,
			settings,
			clientId,
		)
		if err == nil {
			upgradeMuxSettings := connect.DefaultUpgradeMuxSettings()
			upgradeMuxSettings.Dns = nil
			deviceLocal.SetUpgradeMuxSettings(upgradeMuxSettings)
			return deviceLocal
		}
		if endTime.Before(time.Now()) {
			t.Fatalf("new device local: %v", err)
		}
		select {
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// testingReliabilityHeartbeatMillis reads the local's effective heartbeat,
// reporting -1 when there is no effective config (nil settings). Settings
// are nil-able on this path, so no assertion may dereference them blindly.
func testingReliabilityHeartbeatMillis(deviceLocal *DeviceLocal) int64 {
	settings := deviceLocal.GetReliabilitySettings()
	if settings == nil {
		return -1
	}
	return settings.HeartbeatIntervalMillis
}

func testingReliabilityConnectLocation(name string) *ConnectLocation {
	return &ConnectLocation{
		ConnectLocationId: &ConnectLocationId{BestAvailable: true},
		Name:              name,
	}
}

// testingForceDeviceRpcResync drops the live rpc connection (if any) and
// wakes the reconnect loop, so the next sync happens promptly and against
// the local's CURRENT state (e.g. after its multi client was built).
func testingForceDeviceRpcResync(deviceRemote *DeviceRemote) {
	func() {
		deviceRemote.stateLock.Lock()
		defer deviceRemote.stateLock.Unlock()
		deviceRemote.closeService()
	}()
	deviceRemote.Sync()
}

// TestDeviceRemoteReliabilityBridge covers the connected surface end to end:
// the getters read through to the local device (settings equality across the
// bridge, empty exits and destination exits, zero metrics), a settings
// override set on the remote lands on the local's multi client (set -> get
// across the bridge), and ResetReliabilitySettings -- the action assertion
// -- observably runs on the DeviceLocal side, restoring the defaults.
func TestDeviceRemoteReliabilityBridge(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	connect.AssertEqual(t, err, nil)

	clientId := connect.NewId()
	instanceId := NewId()

	deviceLocal := testingNewReliabilityDeviceLocal(t, networkSpace, byJwt, instanceId, clientId)
	defer deviceLocal.Close()

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

	// no multi client: settings are nil on BOTH ends (never a zero struct --
	// see TestReliabilitySettingsNilWithoutMultiClient), while the readouts
	// degrade to zeros and empty
	connect.AssertEqual(t, deviceLocal.GetReliabilitySettings(), nil)
	testingReliabilityWaitFor(t, "remote reports nil settings with no multi client", func() bool {
		return deviceRemote.GetReliabilitySettings() == nil
	})

	remoteExits := deviceRemote.GetExits()
	connect.AssertNotEqual(t, remoteExits, nil)
	connect.AssertEqual(t, remoteExits.Len(), 0)
	remoteDestinationExits := deviceRemote.GetDestinationExits()
	connect.AssertNotEqual(t, remoteDestinationExits, nil)
	connect.AssertEqual(t, remoteDestinationExits.Len(), 0)
	remoteMetrics := deviceRemote.GetReliabilityMetrics()
	connect.AssertNotEqual(t, remoteMetrics, nil)
	connect.AssertEqual(t, remoteMetrics.FlowsOpened, int64(0))

	// bring up the multi client on the local; the effective settings become
	// the shipped defaults
	deviceLocal.SetConnectLocation(testingReliabilityConnectLocation("reliability bridge"))
	defaultSettings := deviceLocal.GetReliabilitySettings()
	connect.AssertNotEqual(t, defaultSettings, nil)
	connect.AssertNotEqual(t, defaultSettings.HeartbeatIntervalMillis, int64(0))

	// the remote reads the same effective settings through the bridge
	testingReliabilityWaitFor(t, "remote reads the local effective settings", func() bool {
		remoteSettings := deviceRemote.GetReliabilitySettings()
		return remoteSettings != nil && remoteSettings.HeartbeatIntervalMillis == defaultSettings.HeartbeatIntervalMillis
	})
	connect.AssertEqual(t, deviceRemote.GetReliabilitySettings(), defaultSettings)

	// read-modify-write from the effective settings (the ui pattern): the
	// override lands on the local multi client through the bridge
	override := deviceRemote.GetReliabilitySettings()
	override.HeartbeatIntervalMillis = 123456
	deviceRemote.SetReliabilitySettings(override)
	testingReliabilityWaitFor(t, "override lands on the local", func() bool {
		return deviceLocal.GetReliabilitySettings().HeartbeatIntervalMillis == int64(123456)
	})

	// set -> get across the bridge, full struct fidelity through
	// remote -> gob -> DeviceLocal -> connect -> back
	connect.AssertEqual(t, deviceRemote.GetReliabilitySettings(), override)
	connect.AssertEqual(t, deviceLocal.GetReliabilitySettings(), override)

	// exits stay a live readout over the bridge (the stub window has no
	// admitted clients)
	connect.AssertNotEqual(t, deviceRemote.GetExits(), nil)
	connect.AssertNotEqual(t, deviceRemote.GetDestinationExits(), nil)

	// the action assertion: ResetReliabilitySettings invoked on the remote
	// observably runs on the DeviceLocal side (the local's effective
	// settings return to the shipped defaults)
	deviceRemote.ResetReliabilitySettings()
	testingReliabilityWaitFor(t, "reset restores the local defaults", func() bool {
		return deviceLocal.GetReliabilitySettings().HeartbeatIntervalMillis == defaultSettings.HeartbeatIntervalMillis
	})
	connect.AssertEqual(t, deviceLocal.GetReliabilitySettings(), defaultSettings)

	// dev action smokes: safe no-ops against the stub window, and a
	// malformed exit id is a client-side no-op
	deviceRemote.ProbeAllExits()
	deviceRemote.SimulateNetworkChange()
	deviceRemote.MigrateExit(NewId().IdStr)
	deviceRemote.MigrateExit("not-an-id")
	deviceRemote.ResetReliabilityMetrics()
	connect.AssertNotEqual(t, deviceRemote.GetReliabilityMetrics(), nil)
}

// testingReliabilityActionLogger captures the connect-side `[rel]` action
// lines. Every dev action logs one (see `RemoteUserNatMultiClient.logAction`),
// which is the only observable the action bridges have: the DeviceLocal
// methods return nothing through the bridge, and against a stub window they
// change no state.
type testingReliabilityActionLogger struct {
	lock  sync.Mutex
	lines []string
}

func (self *testingReliabilityActionLogger) record(line string) {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.lines = append(self.lines, line)
}

// count reports how many captured lines contain every fragment
func (self *testingReliabilityActionLogger) count(fragments ...string) int {
	self.lock.Lock()
	defer self.lock.Unlock()
	matches := 0
	for _, line := range self.lines {
		matched := true
		for _, fragment := range fragments {
			if !strings.Contains(line, fragment) {
				matched = false
				break
			}
		}
		if matched {
			matches += 1
		}
	}
	return matches
}

func (self *testingReliabilityActionLogger) contains(fragments ...string) bool {
	return 0 < self.count(fragments...)
}

func (self *testingReliabilityActionLogger) Info(args ...any) {
	self.record(fmt.Sprint(args...))
}

func (self *testingReliabilityActionLogger) Infof(format string, args ...any) {
	self.record(fmt.Sprintf(format, args...))
}

func (self *testingReliabilityActionLogger) Warningf(format string, args ...any) {
	self.record(fmt.Sprintf(format, args...))
}

func (self *testingReliabilityActionLogger) Errorf(format string, args ...any) {
	self.record(fmt.Sprintf(format, args...))
}

func (self *testingReliabilityActionLogger) V(level int32) connect.Verbose {
	return connect.NewNoopLogger().V(level)
}

// TestDeviceRemoteReliabilityActionsReachTheLocal asserts each action bridge
// actually reached the DeviceLocal side, rather than merely returning without
// error. No-oping any of the three DeviceLocalRpc handlers must fail this.
//
// The observable is the connect-side action log every dev action emits: the
// bridge drops the DeviceLocal return values by contract, and against a stub
// window with no admitted clients the actions change no readable state. For
// MigrateExit the log also carries the exit id, which is what pins the
// ARGUMENT crossing the wire (string -> connect.Id -> *Id -> connect).
func TestDeviceRemoteReliabilityActionsReachTheLocal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	connect.AssertEqual(t, err, nil)

	clientId := connect.NewId()
	instanceId := NewId()

	actionLogger := &testingReliabilityActionLogger{}
	settings := testDeviceLocalSettingsRpc()
	// the capturing logger only reaches connect when logging is enabled
	settings.DisableLogging = false
	settings.ClientSettings.Log = actionLogger
	settings.AllowProvider = false
	settings.GeneratorFunc = func(specs []*connect.ProviderSpec) connect.MultiClientGenerator {
		return &rpcLeakTestGenerator{}
	}
	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace,
		byJwt,
		"",
		"",
		"",
		instanceId,
		settings,
		clientId,
	)
	connect.AssertEqual(t, err, nil)
	defer deviceLocal.Close()

	upgradeMuxSettings := connect.DefaultUpgradeMuxSettings()
	upgradeMuxSettings.Dns = nil
	deviceLocal.SetUpgradeMuxSettings(upgradeMuxSettings)

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

	// the actions need a multi client to act on
	deviceLocal.SetConnectLocation(testingReliabilityConnectLocation("reliability actions"))
	testingReliabilityWaitFor(t, "the remote reaches the local", func() bool {
		return deviceRemote.GetReliabilitySettings() != nil
	})

	// MigrateExit: the action ran AND the exit id arrived intact. The `[rel]`
	// grammar renders a client id as its 8-char tail, so that tail is what
	// pins the argument crossing string -> connect.Id -> *Id -> connect
	exitClientId := NewId()
	exitIdTail := exitClientId.IdStr[len(exitClientId.IdStr)-8:]
	deviceRemote.MigrateExit(exitClientId.IdStr)
	testingReliabilityWaitFor(t, "migrate_exit reached the local with its exit id", func() bool {
		return actionLogger.contains("migrate_exit", "exit="+exitIdTail)
	})

	deviceRemote.ProbeAllExits()
	testingReliabilityWaitFor(t, "probe_all reached the local", func() bool {
		return actionLogger.contains("probe_all")
	})

	deviceRemote.SimulateNetworkChange()
	testingReliabilityWaitFor(t, "network_change reached the local", func() bool {
		return actionLogger.contains("network_change")
	})

	// a malformed exit id is rejected client-side and never reaches the local
	migrateCount := actionLogger.count("migrate_exit")
	deviceRemote.MigrateExit("not-an-id")
	select {
	case <-time.After(500 * time.Millisecond):
	}
	connect.AssertEqual(t, actionLogger.count("migrate_exit"), migrateCount)
}

// TestDeviceRemoteReliabilityConnectedOverrideSurvivesRestart pins the
// deliberate deviation from the ordinary setter convention.
//
// Every other DeviceRemote setter unsets its queued state once the call
// succeeds, because DeviceLocal persists the value. The reliability override
// is runtime-only on the local, so it stays queued even on success and is
// re-applied by every later sync. The distinguishing case is setting while
// CONNECTED (the success path) and then losing the local: with the ordinary
// Unset()-on-success convention the queue would be empty and the override
// would never come back.
func TestDeviceRemoteReliabilityConnectedOverrideSurvivesRestart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	connect.AssertEqual(t, err, nil)

	clientId := connect.NewId()
	instanceId := NewId()

	deviceLocal := testingNewReliabilityDeviceLocal(t, networkSpace, byJwt, instanceId, clientId)
	firstLocalClosed := false
	defer func() {
		if !firstLocalClosed {
			deviceLocal.Close()
		}
	}()
	deviceLocal.SetConnectLocation(testingReliabilityConnectLocation("connected override"))

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

	// set while CONNECTED: the rpc call succeeds, so an ordinary setter
	// would clear its queued state here
	testingReliabilityWaitFor(t, "the remote reaches the local", func() bool {
		return deviceRemote.GetReliabilitySettings() != nil
	})
	override := deviceRemote.GetReliabilitySettings()
	override.HeartbeatIntervalMillis = 654321
	deviceRemote.SetReliabilitySettings(override)
	testingReliabilityWaitFor(t, "the override lands while connected", func() bool {
		return testingReliabilityHeartbeatMillis(deviceLocal) == int64(654321)
	})

	// the extension restarts: a fresh local with no memory of the override
	deviceLocal.Close()
	firstLocalClosed = true

	deviceLocal2 := testingNewReliabilityDeviceLocal(t, networkSpace, byJwt, instanceId, clientId)
	defer deviceLocal2.Close()
	testingForceDeviceRpcResync(deviceRemote)
	testingReliabilityWaitFor(t, "the remote syncs to the restarted local", func() bool {
		return deviceRemote.GetRemoteConnected()
	})
	deviceLocal2.SetConnectLocation(testingReliabilityConnectLocation("connected override restart"))

	// only the retained queue can put it back
	testingReliabilityWaitFor(t, "the connected-set override is re-applied", func() bool {
		return testingReliabilityHeartbeatMillis(deviceLocal2) == int64(654321)
	})
}

// TestDeviceRemoteReliabilityOfflineQueueAndRestartReapply covers the two
// sync-state traps by name:
//   - an override set while the rpc is DOWN is queued and applied on the
//     first sync (offline -> connected), and
//   - an override set while CONNECTED survives the local being torn down and
//     recreated (the extension restart), because the remote keeps it queued
//     and re-applies it on every sync instead of unsetting it after success.
func TestDeviceRemoteReliabilityOfflineQueueAndRestartReapply(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	connect.AssertEqual(t, err, nil)

	clientId := connect.NewId()
	instanceId := NewId()

	// the remote starts with NO local: the override queues
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

	// no service and nothing queued: nil, so a caller has nothing to
	// read-modify-write (see TestReliabilitySettingsNilWithoutMultiClient)
	connect.AssertEqual(t, deviceRemote.GetReliabilitySettings(), nil)

	override := &ReliabilitySettings{
		UdpTeardownSignal:       true,
		DialFailureRerace:       true,
		HeartbeatIntervalMillis: 123456,
	}
	deviceRemote.SetReliabilitySettings(override)

	// the offline getter reports the queued override, and the caller's
	// struct was copied (mutating it must not corrupt the queue)
	connect.AssertEqual(t, deviceRemote.GetReliabilitySettings(), override)
	override.HeartbeatIntervalMillis = 999
	connect.AssertEqual(t, deviceRemote.GetReliabilitySettings().HeartbeatIntervalMillis, int64(123456))
	override.HeartbeatIntervalMillis = 123456

	// offline readouts degrade to empty/zero, never nil
	connect.AssertEqual(t, deviceRemote.GetExits().Len(), 0)
	connect.AssertEqual(t, deviceRemote.GetDestinationExits().Len(), 0)
	connect.AssertEqual(t, deviceRemote.GetReliabilityMetrics().FlowsOpened, int64(0))

	// the local appears WITHOUT a multi client -- the real ios ordering: the
	// extension starts, the rpc syncs, and only later does the tunnel come
	// up. The sync delivers the override to a device that has no multi
	// client to apply it to, so it must be held on the device and applied
	// when the window is built. Syncing FIRST and connecting SECOND is the
	// ordering that catches a bridge which only writes through to the
	// current multi client.
	deviceLocal := testingNewReliabilityDeviceLocal(t, networkSpace, byJwt, instanceId, clientId)
	firstLocalClosed := false
	defer func() {
		if !firstLocalClosed {
			deviceLocal.Close()
		}
	}()
	testingForceDeviceRpcResync(deviceRemote)
	testingReliabilityWaitFor(t, "the remote syncs to the multi-less local", func() bool {
		return deviceRemote.GetRemoteConnected()
	})
	// nothing is in force yet: there is no multi client
	connect.AssertEqual(t, deviceLocal.GetReliabilitySettings(), nil)

	// the tunnel comes up; the held override is applied to the new window
	deviceLocal.SetConnectLocation(testingReliabilityConnectLocation("reliability restart 1"))
	testingReliabilityWaitFor(t, "queued override lands on the first local", func() bool {
		return testingReliabilityHeartbeatMillis(deviceLocal) == int64(123456)
	})

	// a reconnect rebuilds the multi client; the override must survive it
	deviceLocal.SetConnectLocation(testingReliabilityConnectLocation("reliability reconnect"))
	testingReliabilityWaitFor(t, "override survives the multi client rebuild", func() bool {
		return testingReliabilityHeartbeatMillis(deviceLocal) == int64(123456)
	})

	// the extension restart: tear the local down and recreate it. The
	// override is runtime-only state -- nothing on the local persists it --
	// so only the remote's sync-state re-apply can restore it. Again the
	// sync lands before the tunnel is up.
	deviceLocal.Close()
	firstLocalClosed = true

	deviceLocal2 := testingNewReliabilityDeviceLocal(t, networkSpace, byJwt, instanceId, clientId)
	defer deviceLocal2.Close()
	testingForceDeviceRpcResync(deviceRemote)
	testingReliabilityWaitFor(t, "the remote syncs to the restarted local", func() bool {
		return deviceRemote.GetRemoteConnected()
	})
	deviceLocal2.SetConnectLocation(testingReliabilityConnectLocation("reliability restart 2"))
	testingReliabilityWaitFor(t, "override re-applied after the local restart", func() bool {
		return testingReliabilityHeartbeatMillis(deviceLocal2) == int64(123456)
	})

	// reset with the rpc DOWN: the reset queues as the nil sentinel, the next
	// sync delivers it, and -- unlike an override -- it is NOT carried across
	// the sync, so it stops being re-applied. Both the direct and the queued
	// path converge on the same end state, so this does not race the
	// reconnect loop.
	func() {
		deviceRemote.stateLock.Lock()
		defer deviceRemote.stateLock.Unlock()
		deviceRemote.closeService()
	}()
	deviceRemote.ResetReliabilitySettings()
	deviceRemote.Sync()
	testingReliabilityWaitFor(t, "reset restores the second local", func() bool {
		heartbeat := testingReliabilityHeartbeatMillis(deviceLocal2)
		return heartbeat != int64(-1) && heartbeat != int64(123456)
	})
	testingReliabilityWaitFor(t, "the queued reset is cleared by the sync", func() bool {
		deviceRemote.stateLock.Lock()
		defer deviceRemote.stateLock.Unlock()
		return !deviceRemote.state.ReliabilitySettings.IsSet
	})

	// and the override does not come back on a later reconnect
	testingForceDeviceRpcResync(deviceRemote)
	testingReliabilityWaitFor(t, "remote reconnects after the reset", func() bool {
		return deviceRemote.GetReliabilitySettings() != nil
	})
	connect.AssertNotEqual(t, testingReliabilityHeartbeatMillis(deviceLocal2), int64(123456))
}
