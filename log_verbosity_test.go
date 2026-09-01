package sdk

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"
)

// restoreTestingLogVerbosity puts the process verbosity back after a test.
// The -v flag is process-global state that init() sets to 0, so a test that
// raises it and leaves it raised turns every later test's log output to noise.
func restoreTestingLogVerbosity(t *testing.T) {
	t.Helper()
	level := GetLogVerbosity()
	t.Cleanup(func() {
		setLogVerbosityFlag(level)
	})
}

// TestLogVerbosityTakesEffectAtRuntime pins the claim the whole feature rests
// on: setting the -v flag after init, with no restart and no flag.Parse,
// changes what V() reports on the very next call.
//
// glog registers -v as a flag.Value over its Level type and V() re-reads it
// per call, so this holds -- but it is an implementation detail of the glog
// fork, and if a future glog snapshots the level at parse time instead, the
// sdk's verbosity control silently becomes a no-op with nothing else to catch
// it.
func TestLogVerbosityTakesEffectAtRuntime(t *testing.T) {
	restoreTestingLogVerbosity(t)

	// the logger `connect` actually logs through, resolved the same way its
	// components resolve it, so this covers the real path and not just
	// glog.V's own bookkeeping
	log := connect.NewGlogLogger()

	for _, testCase := range []struct {
		level  int
		wantV1 bool
		wantV2 bool
	}{
		{LogVerbosityDefault, false, false},
		{LogVerbosityVerbose, true, false},
		{LogVerbosityTrace, true, true},
		// and back down again: raising verbosity for a repro must be
		// reversible in the same process
		{LogVerbosityDefault, false, false},
	} {
		if err := SetLogVerbosity(testCase.level); err != nil {
			t.Fatalf("SetLogVerbosity(%d) = %v, want nil", testCase.level, err)
		}

		connect.AssertEqual(t, GetLogVerbosity(), testCase.level)
		connect.AssertEqual(t, bool(glog.V(glog.Level(1))), testCase.wantV1)
		connect.AssertEqual(t, bool(glog.V(glog.Level(2))), testCase.wantV2)
		connect.AssertEqual(t, log.V(1).Enabled(), testCase.wantV1)
		connect.AssertEqual(t, log.V(2).Enabled(), testCase.wantV2)
	}
}

// TestLogVerbosityClampsOutOfRange: a level outside 0..2 is clamped, not
// rejected. `connect` only gates on V(1) and V(2), so a 7 would be an
// unbounded promise the sdk cannot keep, and a negative level is nonsense --
// but neither is worth failing a support workflow over.
func TestLogVerbosityClampsOutOfRange(t *testing.T) {
	restoreTestingLogVerbosity(t)

	if err := SetLogVerbosity(7); err != nil {
		t.Fatalf("SetLogVerbosity(7) = %v, want nil", err)
	}
	connect.AssertEqual(t, GetLogVerbosity(), LogVerbosityTrace)
	// clamped, not merely reported as clamped: V(7) must be off, or the
	// process is logging at a level nothing in `connect` writes at while
	// every V() call site pays for the check
	connect.AssertEqual(t, bool(glog.V(glog.Level(7))), false)
	connect.AssertEqual(t, bool(glog.V(glog.Level(2))), true)

	if err := SetLogVerbosity(-3); err != nil {
		t.Fatalf("SetLogVerbosity(-3) = %v, want nil", err)
	}
	connect.AssertEqual(t, GetLogVerbosity(), LogVerbosityDefault)
	connect.AssertEqual(t, bool(glog.V(glog.Level(1))), false)
}

// TestDeviceLocalSetLogVerbosity: the device-level setter raises the process
// it runs in, which on ios is the network extension -- the process that writes
// the contract and transport lines the level is raised for.
func TestDeviceLocalSetLogVerbosity(t *testing.T) {
	restoreTestingLogVerbosity(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	device, _ := testing_newBlockDevice(ctx, t, false)
	defer device.Close()

	connect.AssertEqual(t, device.GetLogVerbosity(), LogVerbosityDefault)

	device.SetLogVerbosity(LogVerbosityTrace)
	connect.AssertEqual(t, device.GetLogVerbosity(), LogVerbosityTrace)
	connect.AssertEqual(t, GetLogVerbosity(), LogVerbosityTrace)
	connect.AssertEqual(t, connect.NewGlogLogger().V(2).Enabled(), true)
}

// A hosted device shares one process with unrelated customers' devices, and
// the verbosity flag is process-global. One tenant raising it would put every
// other tenant's traffic into the host's logs at V(2), which is both a volume
// and a disclosure problem.
func TestDeviceLocalHostedSetLogVerbosityIsIgnored(t *testing.T) {
	restoreTestingLogVerbosity(t)

	if err := SetLogVerbosity(LogVerbosityDefault); err != nil {
		t.Fatalf("SetLogVerbosity: %v", err)
	}

	hosted := &DeviceLocal{
		settings: &DeviceLocalSettings{HostedIncompatible: true},
		log:      connect.NewNoopLogger(),
	}
	hosted.SetLogVerbosity(LogVerbosityTrace)

	connect.AssertEqual(t, GetLogVerbosity(), LogVerbosityDefault)
}

// testing_newSyncedDeviceLocalRemoteSeparateSpaces is
// testing_newSyncedDeviceLocalRemote with one difference that the log
// verbosity tests depend on: the local and the remote get their OWN network
// space, and therefore their own local storage directory.
//
// That is what the two processes actually look like on ios -- the app writes
// <Application/uuid>/.by and the extension writes <PluginKitPlugin/uuid>/.by,
// distinct containers -- and it is what makes a crossing observable in one
// test process. With a shared space (the default helper) both devices persist
// to the same file, so a level found there proves nothing about which of them
// wrote it. With separate spaces, only DeviceLocal.SetLogVerbosity can put a
// level in the DEVICE's file, so finding one there means the rpc carried it.
//
// The remote is returned already synced. Both are closed via t.Cleanup.
func testing_newSyncedDeviceLocalRemoteSeparateSpaces(
	t *testing.T,
	ctx context.Context,
) (deviceLocal *DeviceLocal, deviceRemote *DeviceRemote, localSpaceState *LocalState, remoteSpaceState *LocalState) {
	t.Helper()

	localSpace, localByJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	remoteSpace, remoteByJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if localSpace.GetAsyncLocalState().GetLocalState().localStorageDir ==
		remoteSpace.GetAsyncLocalState().GetLocalState().localStorageDir {
		t.Fatal("the two spaces share a storage dir, so a crossing would be unobservable")
	}

	clientId := connect.NewId()
	instanceId := NewId()
	settings := defaultDeviceRpcSettings()

	deviceLocal, err = newDeviceLocalWithOverrides(
		localSpace, localByJwt, "", "", "", instanceId, testDeviceLocalSettingsRpc(), clientId,
	)
	if err != nil {
		t.Fatal(err)
	}

	deviceRemote, err = newDeviceRemoteWithOverrides(
		remoteSpace, remoteByJwt, instanceId, settings, clientId, testing_deviceRpcDialer(settings),
	)
	if err != nil {
		deviceLocal.Close()
		t.Fatal(err)
	}

	t.Cleanup(func() {
		deviceRemote.Close()
		deviceLocal.Close()
	})

	deviceRemote.Sync()
	if !deviceRemote.waitForSync(15 * time.Second) {
		t.Fatal("device remote did not sync")
	}

	return deviceLocal, deviceRemote,
		localSpace.GetAsyncLocalState().GetLocalState(),
		remoteSpace.GetAsyncLocalState().GetLocalState()
}

// testing_awaitPersistedLogVerbosity waits for a level to land in one local
// state. The device persists asynchronously (serialAsync), so the write
// trails the call that caused it.
func testing_awaitPersistedLogVerbosity(t *testing.T, localState *LocalState, want int) bool {
	t.Helper()
	for i := 0; i < 500; i += 1 {
		if localState.GetLogVerbosity() == want {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// TestDeviceRemoteSetLogVerbosityCrossesToTheDeviceProcess is the reason the
// rpc bridge exists: on ios `connect` runs in the network extension, a
// separate process with its own glog state, so a level set in the app reaches
// the logs that matter only if it crosses the rpc.
//
// This drives DeviceRemote.SetLogVerbosity -- the call the app actually makes
// -- rather than the rpc method under it, so the dispatch is what is pinned.
// The two devices share this test process, so the process-global level proves
// nothing; the two devices do NOT share a network space, so the level landing
// in the DEVICE's local state can only have come across the rpc.
//
// It also pins that the crossing happened NOW, over the live connection, and
// was not merely queued for some later sync: nothing in the sdk triggers a
// resync on a queued write, so a level that is still queued here is a level
// the extension would not be logging at while the user reproduces their bug.
func TestDeviceRemoteSetLogVerbosityCrossesToTheDeviceProcess(t *testing.T) {
	restoreTestingLogVerbosity(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	deviceLocal, deviceRemote, localSpaceState, _ := testing_newSyncedDeviceLocalRemoteSeparateSpaces(t, ctx)

	if err := setLogVerbosityFlag(LogVerbosityDefault); err != nil {
		t.Fatalf("setLogVerbosityFlag: %v", err)
	}
	connect.AssertEqual(t, localSpaceState.GetLogVerbosity(), LogVerbosityDefault)

	deviceRemote.SetLogVerbosity(LogVerbosityTrace)

	// delivered over the live rpc, not left for a later sync
	deviceRemote.stateLock.Lock()
	pending := deviceRemote.state.LogVerbosity.IsSet
	deviceRemote.stateLock.Unlock()
	if pending {
		t.Fatal("the level is queued for a later sync, so the connected device never received it")
	}

	connect.AssertEqual(t, deviceLocal.GetLogVerbosity(), LogVerbosityTrace)
	// the device recorded it in ITS OWN storage, which only the device side
	// writes -- the crossing, observed from the far end
	if !testing_awaitPersistedLogVerbosity(t, localSpaceState, LogVerbosityTrace) {
		t.Fatal("the device process never recorded the level, so nothing crossed the rpc")
	}
}

// TestDeviceRemoteLogVerbosityQueuedWhileDownCrossesOnConnect is the user's
// real order of operations: raise the level while disconnected, then connect
// and reproduce.
//
// Persisting in the app cannot cover this. On ios the app and the extension
// each read their own Documents container -- only the app group is shared, and
// this repo uses it for logs alone -- so the level the app wrote is a file the
// extension never opens, and the tunnel comes up at 0 and captures none of the
// V(1) contract and transport lines the raise exists for. The queued sync
// state is what carries it, so this test starts the device AFTER the set and
// checks the DEVICE's own storage.
func TestDeviceRemoteLogVerbosityQueuedWhileDownCrossesOnConnect(t *testing.T) {
	restoreTestingLogVerbosity(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	localSpace, localByJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}
	remoteSpace, remoteByJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}
	localSpaceState := localSpace.GetAsyncLocalState().GetLocalState()

	clientId := connect.NewId()
	instanceId := NewId()
	settings := defaultDeviceRpcSettings()

	// the tunnel is down: nothing is listening on the rpc address yet
	deviceRemote, err := newDeviceRemoteWithOverrides(
		remoteSpace, remoteByJwt, instanceId, settings, clientId, testing_deviceRpcDialer(settings),
	)
	if err != nil {
		t.Fatalf("device remote: %v", err)
	}
	defer deviceRemote.Close()
	connect.AssertEqual(t, deviceRemote.GetRemoteConnected(), false)

	deviceRemote.SetLogVerbosity(LogVerbosityVerbose)

	// with no service to take the call the level is queued, which is the only
	// thing that will reach the device process
	deviceRemote.stateLock.Lock()
	queued := deviceRemote.state.LogVerbosity
	deviceRemote.stateLock.Unlock()
	connect.AssertEqual(t, queued.IsSet, true)
	connect.AssertEqual(t, queued.Value, LogVerbosityVerbose)

	// stand in for the tunnel process starting: a fresh device, whose own
	// storage has never held a level, in a process reset to 0
	if err := setLogVerbosityFlag(LogVerbosityDefault); err != nil {
		t.Fatalf("setLogVerbosityFlag: %v", err)
	}
	deviceLocal, err := newDeviceLocalWithOverrides(
		localSpace, localByJwt, "", "", "", instanceId, testDeviceLocalSettingsRpc(), clientId,
	)
	if err != nil {
		t.Fatalf("device local: %v", err)
	}
	defer deviceLocal.Close()
	connect.AssertEqual(t, deviceLocal.GetLogVerbosity(), LogVerbosityDefault)

	deviceRemote.Sync()
	if !deviceRemote.waitForSync(15 * time.Second) {
		t.Fatal("device remote did not sync after the device came up")
	}

	if !testing_awaitPersistedLogVerbosity(t, localSpaceState, LogVerbosityVerbose) {
		t.Fatal("the tunnel came up at the default level, so the session being reproduced captures nothing")
	}
	connect.AssertEqual(t, deviceLocal.GetLogVerbosity(), LogVerbosityVerbose)
}

// A level the app restored from its own storage is re-queued for the device
// process at construction. The app's copy and the extension's are separate
// files in separate containers on ios, so a reinstall, a cleared extension
// container, or simply a level chosen against one tunnel and carried across an
// app relaunch would otherwise leave the extension at 0 while the app reports
// the level the user chose.
func TestDeviceRemoteRestoredLogVerbosityIsQueuedForTheDevice(t *testing.T) {
	restoreTestingLogVerbosity(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}

	// the level a previous app session chose
	if err := networkSpace.GetAsyncLocalState().GetLocalState().SetLogVerbosity(LogVerbosityTrace); err != nil {
		t.Fatalf("SetLogVerbosity: %v", err)
	}
	if err := setLogVerbosityFlag(LogVerbosityDefault); err != nil {
		t.Fatalf("setLogVerbosityFlag: %v", err)
	}

	settings := defaultDeviceRpcSettings()
	settings.Address = requireRemoteAddress(testing_freeHostPort())
	deviceRemote, err := newDeviceRemoteWithOverrides(
		networkSpace,
		byJwt,
		NewId(),
		settings,
		connect.NewId(),
		NewWebsocketDeviceRpcDialer(settings.Address, "", "", settings),
	)
	if err != nil {
		t.Fatalf("device remote: %v", err)
	}
	defer deviceRemote.Close()

	connect.AssertEqual(t, deviceRemote.GetLogVerbosity(), LogVerbosityTrace)

	deviceRemote.stateLock.Lock()
	queued := deviceRemote.state.LogVerbosity
	deviceRemote.stateLock.Unlock()
	connect.AssertEqual(t, queued.IsSet, true)
	connect.AssertEqual(t, queued.Value, LogVerbosityTrace)
}

// SetLogVerbosity is exported to gomobile and to the C ABI, and inside the sdk
// it is reached from DeviceRemote.SetLogVerbosity outside stateLock and from
// whatever goroutine constructs a device. Nothing serializes those callers, so
// the setter has to.
//
// The write it performs lands in flag.CommandLine.actual, an unsynchronized
// map: concurrent writes are not only a race the suite would flag under
// -race, they can be an unrecoverable concurrent-map-write fatal error that no
// recover can catch.
func TestSetLogVerbosityIsSafeForConcurrentUse(t *testing.T) {
	restoreTestingLogVerbosity(t)

	levels := []int{LogVerbosityDefault, LogVerbosityVerbose, LogVerbosityTrace}

	var wg sync.WaitGroup
	for i := 0; i < 8; i += 1 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 500; j += 1 {
				if err := SetLogVerbosity(levels[(i+j)%len(levels)]); err != nil {
					t.Errorf("SetLogVerbosity: %v", err)
					return
				}
				// a concurrent reader, since the read path is deliberately
				// unlocked: it must stay a read of immutable or atomic state
				GetLogVerbosity()
			}
		}(i)
	}
	wg.Wait()

	// still a coherent level, and still the one last written
	if err := SetLogVerbosity(LogVerbosityVerbose); err != nil {
		t.Fatalf("SetLogVerbosity: %v", err)
	}
	connect.AssertEqual(t, GetLogVerbosity(), LogVerbosityVerbose)
}

// TestLocalStateLogVerbosityRoundTrip: the level survives the process, which
// is the whole point of persisting it -- the bug the user is capturing is
// normally reproduced by reconnecting, and the tunnel process re-runs
// initGlog on the way up.
func TestLocalStateLogVerbosityRoundTrip(t *testing.T) {
	localState := newLocalState(context.Background(), t.TempDir())

	// unset reads as the level a process starts at anyway
	connect.AssertEqual(t, localState.GetLogVerbosity(), LogVerbosityDefault)

	if err := localState.SetLogVerbosity(LogVerbosityTrace); err != nil {
		t.Fatalf("SetLogVerbosity: %v", err)
	}
	connect.AssertEqual(t, localState.GetLogVerbosity(), LogVerbosityTrace)

	// a level from a future build with a wider range must not read back as a
	// level this build does not honor
	if err := localState.SetLogVerbosity(9); err != nil {
		t.Fatalf("SetLogVerbosity: %v", err)
	}
	connect.AssertEqual(t, localState.GetLogVerbosity(), LogVerbosityTrace)

	// a corrupt file is not a reason to start a process logging at an unknown
	// level
	path := filepath.Join(localState.localStorageDir, ".log_verbosity")
	if err := os.WriteFile(path, []byte("loud"), LocalStorageFilePermissions); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	connect.AssertEqual(t, localState.GetLogVerbosity(), LogVerbosityDefault)
}

// TestDeviceLocalLogVerbosityPersistRestore is the user's actual workflow:
// raise the level, reproduce the bug -- which means reconnecting -- then
// upload the logs. The reconnect starts a new tunnel process whose initGlog
// resets the level to 0, so without the restore the session being captured is
// the one session that is not captured.
func TestDeviceLocalLogVerbosityPersistRestore(t *testing.T) {
	restoreTestingLogVerbosity(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}
	localState := networkSpace.GetAsyncLocalState().GetLocalState()

	device := testing_newBlockDeviceWithNetworkSpace(t, networkSpace, byJwt, false)
	connect.AssertEqual(t, localState.GetLogVerbosity(), LogVerbosityDefault)

	// the set persists asynchronously to local state
	device.SetLogVerbosity(LogVerbosityTrace)
	persisted := false
	for i := 0; i < 100; i += 1 {
		if localState.GetLogVerbosity() == LogVerbosityTrace {
			persisted = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	connect.AssertEqual(t, persisted, true)
	device.Close()

	// stand in for the tunnel restart: a new process runs initGlog, which sets
	// the level back to 0 before any device exists
	if err := setLogVerbosityFlag(LogVerbosityDefault); err != nil {
		t.Fatalf("setLogVerbosityFlag: %v", err)
	}

	restored := testing_newBlockDeviceWithNetworkSpace(t, networkSpace, byJwt, false)
	defer restored.Close()
	connect.AssertEqual(t, restored.GetLogVerbosity(), LogVerbosityTrace)
}

// TestDeviceRemoteSetLogVerbosityPersistsWithTheTunnelDown covers what the APP
// process keeps for itself while the tunnel is down: the level it restores at
// its own next launch, and the one it reports in the meantime.
//
// It is deliberately not the crossing. This local state is the app's own
// container on ios, and the extension never reads it -- what puts the level in
// the tunnel process is the queued sync state, covered by
// TestDeviceRemoteLogVerbosityQueuedWhileDownCrossesOnConnect. Both devices
// share one network space here, so this test could not tell the two apart.
func TestDeviceRemoteSetLogVerbosityPersistsWithTheTunnelDown(t *testing.T) {
	restoreTestingLogVerbosity(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}
	localState := networkSpace.GetAsyncLocalState().GetLocalState()

	// a level chosen in an earlier session, and an app process that has just
	// restarted: initGlog has reset this process to 0
	if err := localState.SetLogVerbosity(LogVerbosityTrace); err != nil {
		t.Fatalf("SetLogVerbosity: %v", err)
	}
	if err := setLogVerbosityFlag(LogVerbosityDefault); err != nil {
		t.Fatalf("setLogVerbosityFlag: %v", err)
	}

	// nothing is listening on this address, so the remote never has a service:
	// the tunnel is down
	settings := defaultDeviceRpcSettings()
	settings.Address = requireRemoteAddress(testing_freeHostPort())
	deviceRemote, err := newDeviceRemoteWithOverrides(
		networkSpace,
		byJwt,
		NewId(),
		settings,
		connect.NewId(),
		NewWebsocketDeviceRpcDialer(settings.Address, "", "", settings),
	)
	if err != nil {
		t.Fatalf("device remote: %v", err)
	}
	defer deviceRemote.Close()
	connect.AssertEqual(t, deviceRemote.GetRemoteConnected(), false)

	// the remote restores the persisted level into the app process too, so
	// what the app reports is what the extension it is about to start will be
	// logging at, rather than the 0 this process was reset to
	connect.AssertEqual(t, deviceRemote.GetLogVerbosity(), LogVerbosityTrace)

	deviceRemote.SetLogVerbosity(LogVerbosityVerbose)

	// the app process is raised immediately, so its own lines match the level
	// it reports
	connect.AssertEqual(t, deviceRemote.GetLogVerbosity(), LogVerbosityVerbose)

	persisted := false
	for i := 0; i < 100; i += 1 {
		if localState.GetLogVerbosity() == LogVerbosityVerbose {
			persisted = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	connect.AssertEqual(t, persisted, true)
}

// A hosted DeviceRemote is the platform client -- a browser or wasm process,
// one per user -- driving a DeviceLocal that shares its process with unrelated
// tenants. What the hosted guard protects is that shared process: the level
// must never be sent to it, nor left queued for the next sync to send.
//
// This process is a different matter. It is the client's own, its level is
// already raised on the spot, and it is the level it reports -- so it is
// recorded and restored like anywhere else. Guarding the record too would
// leave the platform client showing a level it silently forgets at the next
// reload.
func TestDeviceRemoteHostedSetLogVerbosityStopsAtThisProcess(t *testing.T) {
	restoreTestingLogVerbosity(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}
	localState := networkSpace.GetAsyncLocalState().GetLocalState()

	if err := setLogVerbosityFlag(LogVerbosityDefault); err != nil {
		t.Fatalf("setLogVerbosityFlag: %v", err)
	}

	// the hosted rpc client: hosted-incompatible mutations are dropped, and
	// nothing is listening on the address either way
	settings := defaultDeviceRpcSettings()
	settings.DisableHostedIncompatible = true
	settings.Address = requireRemoteAddress(testing_freeHostPort())
	newHostedRemote := func() *DeviceRemote {
		t.Helper()
		deviceRemote, err := newDeviceRemoteWithOverrides(
			networkSpace,
			byJwt,
			NewId(),
			settings,
			connect.NewId(),
			NewWebsocketDeviceRpcDialer(settings.Address, "", "", settings),
		)
		if err != nil {
			t.Fatalf("device remote: %v", err)
		}
		t.Cleanup(deviceRemote.Close)
		return deviceRemote
	}
	queuedForTheDevice := func(deviceRemote *DeviceRemote) bool {
		deviceRemote.stateLock.Lock()
		defer deviceRemote.stateLock.Unlock()
		return deviceRemote.state.LogVerbosity.IsSet
	}

	deviceRemote := newHostedRemote()
	deviceRemote.SetLogVerbosity(LogVerbosityTrace)

	connect.AssertEqual(t, deviceRemote.GetLogVerbosity(), LogVerbosityTrace)
	if !testing_awaitPersistedLogVerbosity(t, localState, LogVerbosityTrace) {
		t.Fatal("the client did not record its own level, so it reports one it does not keep")
	}
	if queuedForTheDevice(deviceRemote) {
		t.Fatal("the level is queued for a hosted device, whose process is shared with unrelated tenants")
	}

	// and at the client's next launch, with this process reset the way
	// initGlog resets it
	if err := setLogVerbosityFlag(LogVerbosityDefault); err != nil {
		t.Fatalf("setLogVerbosityFlag: %v", err)
	}
	relaunched := newHostedRemote()
	connect.AssertEqual(t, relaunched.GetLogVerbosity(), LogVerbosityTrace)
	if queuedForTheDevice(relaunched) {
		t.Fatal("the restored level is queued for a hosted device, which the guard exists to prevent")
	}
}

// Restoring is for a level the user chose. With nothing persisted there is no
// instruction to apply, and applying the default anyway would clear a level an
// embedder set another way -- a server that passed -v on its command line
// would lose it to the first device it constructed.
func TestDeviceLocalRestoreLeavesAnUnsetVerbosityAlone(t *testing.T) {
	restoreTestingLogVerbosity(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}

	// the embedder's own choice, made before any device exists
	if err := SetLogVerbosity(LogVerbosityTrace); err != nil {
		t.Fatalf("SetLogVerbosity: %v", err)
	}

	device := testing_newBlockDeviceWithNetworkSpace(t, networkSpace, byJwt, false)
	defer device.Close()

	connect.AssertEqual(t, device.GetLogVerbosity(), LogVerbosityTrace)
}
