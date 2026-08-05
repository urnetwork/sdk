package sdk

import (
	"context"
	"reflect"
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
// NOTE for CI: the sdk test binary does not build on Windows (pre-existing
// unix-only test file), so these must run in the Linux sdk CI.

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

	// disconnected multi client: zeros and empty, never nil, on both ends
	localSettings := deviceLocal.GetReliabilitySettings()
	connect.AssertNotEqual(t, localSettings, nil)
	connect.AssertEqual(t, localSettings.HeartbeatIntervalMillis, int64(0))

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
	connect.AssertNotEqual(t, defaultSettings.HeartbeatIntervalMillis, int64(0))

	// the remote reads the same effective settings through the bridge
	testingReliabilityWaitFor(t, "remote reads the local effective settings", func() bool {
		return deviceRemote.GetReliabilitySettings().HeartbeatIntervalMillis == defaultSettings.HeartbeatIntervalMillis
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

	// the local appears with a live multi client; the queued override must
	// land on it via the sync. The forced resync makes the test independent
	// of whether the remote managed to sync before the multi client existed
	// (a sync-time apply against a multi-less local is a no-op).
	deviceLocal := testingNewReliabilityDeviceLocal(t, networkSpace, byJwt, instanceId, clientId)
	firstLocalClosed := false
	defer func() {
		if !firstLocalClosed {
			deviceLocal.Close()
		}
	}()
	deviceLocal.SetConnectLocation(testingReliabilityConnectLocation("reliability restart 1"))
	testingForceDeviceRpcResync(deviceRemote)
	testingReliabilityWaitFor(t, "queued override lands on the first local", func() bool {
		return deviceLocal.GetReliabilitySettings().HeartbeatIntervalMillis == int64(123456)
	})

	// the extension restart: tear the local down and recreate it. The
	// override is runtime-only state -- nothing on the local persists it --
	// so only the remote's sync-state re-apply can restore it.
	deviceLocal.Close()
	firstLocalClosed = true

	deviceLocal2 := testingNewReliabilityDeviceLocal(t, networkSpace, byJwt, instanceId, clientId)
	defer deviceLocal2.Close()
	deviceLocal2.SetConnectLocation(testingReliabilityConnectLocation("reliability restart 2"))
	// the fresh local starts from the shipped defaults
	connect.AssertNotEqual(t, deviceLocal2.GetReliabilitySettings().HeartbeatIntervalMillis, int64(123456))
	testingForceDeviceRpcResync(deviceRemote)
	testingReliabilityWaitFor(t, "override re-applied after the local restart", func() bool {
		return deviceLocal2.GetReliabilitySettings().HeartbeatIntervalMillis == int64(123456)
	})

	// reset clears the override on the local AND clears the queued sync
	// state, so it is no longer re-applied
	deviceRemote.ResetReliabilitySettings()
	testingReliabilityWaitFor(t, "reset restores the second local", func() bool {
		return deviceLocal2.GetReliabilitySettings().HeartbeatIntervalMillis != int64(123456)
	})
	queued := func() bool {
		deviceRemote.stateLock.Lock()
		defer deviceRemote.stateLock.Unlock()
		return deviceRemote.state.ReliabilitySettings.IsSet
	}()
	connect.AssertEqual(t, queued, false)
}
