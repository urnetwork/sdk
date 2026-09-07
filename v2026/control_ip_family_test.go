package sdk

import (
	"context"
	"encoding/json"
	"net"
	"net/rpc"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

func TestControlIpFamilyPolicyRoundTrips(t *testing.T) {
	defer SetControlIpFamilyPolicy(IpFamilyPolicyAuto)
	tests := []struct {
		name string
		set  int
		want int
	}{
		{"auto", IpFamilyPolicyAuto, IpFamilyPolicyAuto},
		{"force4", IpFamilyPolicyForce4, IpFamilyPolicyForce4},
		{"force6", IpFamilyPolicyForce6, IpFamilyPolicyForce6},
		{"above range clamps to auto", 7, IpFamilyPolicyAuto},
		{"below range clamps to auto", -1, IpFamilyPolicyAuto},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			SetControlIpFamilyPolicy(test.set)
			if got := GetControlIpFamilyPolicy(); got != test.want {
				t.Fatalf("got %d, want %d", got, test.want)
			}
		})
	}
}

// TestClampIpFamilyPolicy exercises the sdk layer's own clampIpFamilyPolicy
// directly, independent of connect.SetControlIpFamilyPolicy's own fallback to
// Auto for an unrecognized value. TestControlIpFamilyPolicyRoundTrips above
// cannot distinguish sdk.go's clamp from connect's: both converge on the same
// Auto result for an out-of-range input, so that round trip would still pass
// even if clampIpFamilyPolicy were skipped entirely. Calling the unexported
// function in this same-package test is the only way to pin the sdk-layer
// clamp's own return value in isolation.
func TestClampIpFamilyPolicy(t *testing.T) {
	tests := []struct {
		name   string
		policy int
		want   int
	}{
		{"auto", IpFamilyPolicyAuto, IpFamilyPolicyAuto},
		{"force4", IpFamilyPolicyForce4, IpFamilyPolicyForce4},
		{"force6", IpFamilyPolicyForce6, IpFamilyPolicyForce6},
		{"above range clamps to auto", 7, IpFamilyPolicyAuto},
		{"below range clamps to auto", -1, IpFamilyPolicyAuto},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := clampIpFamilyPolicy(test.policy); got != test.want {
				t.Fatalf("got %d, want %d", got, test.want)
			}
		})
	}
}

// seedPersistedControlIpFamilyPolicy writes a policy into the local storage of
// one network space key, without going through a NetworkSpace: the restore
// under test runs while the manager is being built, so anything that
// constructs a space would run it before the seed was in place.
func seedPersistedControlIpFamilyPolicy(t *testing.T, ctx context.Context, storagePath string, key *NetworkSpaceKey, policy int) {
	t.Helper()
	// envStoragePath owns the host/env directory layout (and creates it), so
	// the seed lands where the manager's own space will look for it
	seedManager := newNetworkSpaceManagerWithContext(ctx, storagePath)
	envStoragePath := seedManager.envStoragePath(key)
	seedManager.Close()
	if err := newLocalState(ctx, envStoragePath).SetControlIpFamilyPolicy(policy); err != nil {
		t.Fatal(err)
	}
}

// writeNetworkSpaceIndex writes the manager's stored index directly, in a
// GIVEN order.
//
// `store` serializes the spaces out of a map, so the on-disk order is random.
// A two-space test that relied on it would only catch the per-space restore
// half the time. Writing the index by hand puts the ACTIVE space first and the
// other last, which is the worst case for a restore that fires from every
// constructed space: the last one built is the one that wins.
func writeNetworkSpaceIndex(t *testing.T, storagePath string, keys []NetworkSpaceKey, active *NetworkSpaceKey) {
	t.Helper()
	networkSpaceStates := []*networkSpaceState{}
	for _, key := range keys {
		networkSpaceStates = append(networkSpaceStates, &networkSpaceState{
			Key:    key,
			Values: NetworkSpaceValues{},
		})
	}
	stateBytes, err := json.Marshal(&networkSpaceManagerState{
		NetworkSpaces: networkSpaceStates,
		Active:        active,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(storagePath, ".network_spaces"),
		stateBytes,
		LocalStorageFilePermissions,
	); err != nil {
		t.Fatal(err)
	}
}

// THE departure from the log-verbosity template, and the reason for it.
//
// A user who forces IPv4, kills the app and relaunches hits the LOGIN api call
// before any Device exists. That is precisely the call they are stuck on. The
// log verbosity is restored from the two Device constructors, which would
// leave this setting inert during the one request that matters -- while the
// developer menu read back the correct value the whole time.
//
// The assertion is made the instant the manager constructor returns: no
// listener can have been registered yet, no Device exists, and the api token
// manager's refresh worker parks until a Device calls StartJwtRefresh, so
// nothing has been able to issue a request.
func TestPolicyIsInForceAfterNetworkSpaceManagerConstructionWithNoDevice(t *testing.T) {
	defer SetControlIpFamilyPolicy(IpFamilyPolicyAuto)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	storagePath := t.TempDir()
	key := *NewNetworkSpaceKey("example.test", "main")
	seedPersistedControlIpFamilyPolicy(t, ctx, storagePath, &key, IpFamilyPolicyForce4)
	writeNetworkSpaceIndex(t, storagePath, []NetworkSpaceKey{key}, &key)

	SetControlIpFamilyPolicy(IpFamilyPolicyAuto)
	networkSpaceManager := newNetworkSpaceManagerWithContext(ctx, storagePath)
	defer networkSpaceManager.Close()

	if got := GetControlIpFamilyPolicy(); got != IpFamilyPolicyForce4 {
		t.Fatalf("policy is %d after constructing the network space manager, want force4 -- "+
			"the restore did not happen before the first api call could be made", got)
	}
}

// The fresh-install and corrupt-index paths: the manager comes up with no
// spaces at all, and the app creates the bundled space and activates it before
// it reads a stored jwt. The restore still has to be in force by then, because
// that is still before any Device exists.
func TestPolicyIsInForceOnTheFirstSpaceWithNoStoredIndex(t *testing.T) {
	defer SetControlIpFamilyPolicy(IpFamilyPolicyAuto)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	storagePath := t.TempDir()
	key := *NewNetworkSpaceKey("example.test", "main")
	seedPersistedControlIpFamilyPolicy(t, ctx, storagePath, &key, IpFamilyPolicyForce6)

	SetControlIpFamilyPolicy(IpFamilyPolicyAuto)
	networkSpaceManager := newNetworkSpaceManagerWithContext(ctx, storagePath)
	defer networkSpaceManager.Close()
	if got := GetControlIpFamilyPolicy(); got != IpFamilyPolicyAuto {
		t.Fatalf("policy is %d with no stored index, want auto -- nothing was bound yet", got)
	}

	networkSpace := networkSpaceManager.UpdateNetworkSpaceValues(&key, &NetworkSpaceValues{})
	if got := GetControlIpFamilyPolicy(); got != IpFamilyPolicyForce6 {
		t.Fatalf("policy is %d after the bundled space was created, want force6", got)
	}
	networkSpaceManager.SetActiveNetworkSpace(networkSpace)
	if got := GetControlIpFamilyPolicy(); got != IpFamilyPolicyForce6 {
		t.Fatalf("policy is %d after activating the space, want force6", got)
	}
}

// The ACTIVE space's policy wins, not whichever space the manager happened to
// construct last.
//
// This is the normal case on this branch, not an edge case: a custom api host
// exists alongside the production one precisely so both are configured. The
// manager builds EVERY stored space before it selects the active one, so a
// restore that ran per space handed the process the last entry in the stored
// slice -- here, deliberately, the space the user is not on.
func TestTheActiveSpacesPersistedPolicyWins(t *testing.T) {
	defer SetControlIpFamilyPolicy(IpFamilyPolicyAuto)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	storagePath := t.TempDir()
	activeKey := *NewNetworkSpaceKey("custom.example", "main")
	otherKey := *NewNetworkSpaceKey("bringyour.com", "main")

	seedPersistedControlIpFamilyPolicy(t, ctx, storagePath, &activeKey, IpFamilyPolicyForce4)
	seedPersistedControlIpFamilyPolicy(t, ctx, storagePath, &otherKey, IpFamilyPolicyForce6)
	writeNetworkSpaceIndex(t, storagePath, []NetworkSpaceKey{activeKey, otherKey}, &activeKey)

	SetControlIpFamilyPolicy(IpFamilyPolicyAuto)
	networkSpaceManager := newNetworkSpaceManagerWithContext(ctx, storagePath)
	defer networkSpaceManager.Close()

	if networkSpaceManager.GetActiveNetworkSpace() == nil {
		t.Fatal("no active network space was selected, so the test proves nothing")
	}
	if got := networkSpaceManager.GetActiveNetworkSpace().GetHostName(); got != activeKey.HostName {
		t.Fatalf("active space is %s, want %s", got, activeKey.HostName)
	}
	if got := GetControlIpFamilyPolicy(); got != IpFamilyPolicyForce4 {
		t.Fatalf("policy is %d, want force4 -- the INACTIVE space's persisted force6 won", got)
	}
}

// A policy set at runtime survives everything the manager does afterwards.
//
// `updateNetworkSpace` rebuilds a space, and ios calls it at boot for the
// bundled space and again on every custom-server import. A restore that ran
// per constructed space re-imposed the persisted value over one an embedder
// had just set, with nothing in the logs to explain it.
func TestManagerDoesNotReimposeAPersistedPolicyOverARuntimeSet(t *testing.T) {
	defer SetControlIpFamilyPolicy(IpFamilyPolicyAuto)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	storagePath := t.TempDir()
	activeKey := *NewNetworkSpaceKey("custom.example", "main")
	otherKey := *NewNetworkSpaceKey("bringyour.com", "main")

	seedPersistedControlIpFamilyPolicy(t, ctx, storagePath, &activeKey, IpFamilyPolicyForce4)
	seedPersistedControlIpFamilyPolicy(t, ctx, storagePath, &otherKey, IpFamilyPolicyForce6)
	writeNetworkSpaceIndex(t, storagePath, []NetworkSpaceKey{activeKey, otherKey}, &activeKey)

	networkSpaceManager := newNetworkSpaceManagerWithContext(ctx, storagePath)
	defer networkSpaceManager.Close()

	// whatever the construction restore did, the embedder now turns the force
	// back off at runtime without persisting it. Asserted from here rather
	// than from the restored value, so this test pins the re-imposition on its
	// own -- TestTheActiveSpacesPersistedPolicyWins owns the restore itself.
	SetControlIpFamilyPolicy(IpFamilyPolicyAuto)

	// the boot refresh of the bundled (inactive) space, and a custom-server
	// import of the active one
	networkSpaceManager.UpdateNetworkSpaceValues(&otherKey, &NetworkSpaceValues{})
	if got := GetControlIpFamilyPolicy(); got != IpFamilyPolicyAuto {
		t.Fatalf("policy is %d after updating the inactive space, want auto -- its persisted force6 was re-imposed", got)
	}
	networkSpaceManager.UpdateNetworkSpaceValues(&activeKey, &NetworkSpaceValues{})
	if got := GetControlIpFamilyPolicy(); got != IpFamilyPolicyAuto {
		t.Fatalf("policy is %d after updating the active space, want auto -- its persisted force4 was re-imposed", got)
	}

	// and re-selecting a space is not a restore point either
	networkSpaceManager.SetActiveNetworkSpace(networkSpaceManager.GetNetworkSpace(&otherKey))
	if got := GetControlIpFamilyPolicy(); got != IpFamilyPolicyAuto {
		t.Fatalf("policy is %d after re-selecting a space, want auto", got)
	}
}

func TestNetworkSpaceSetControlIpFamilyPolicyPersists(t *testing.T) {
	defer SetControlIpFamilyPolicy(IpFamilyPolicyAuto)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	storagePath := t.TempDir()
	networkSpace := newNetworkSpace(
		ctx,
		*NewNetworkSpaceKey("example.test", "main"),
		NetworkSpaceValues{
			NetExposeServerIps:       true,
			NetExposeServerHostNames: true,
		},
		storagePath,
	)
	defer networkSpace.close()
	defer networkSpace.asyncLocalState.Close()

	networkSpace.SetControlIpFamilyPolicy(IpFamilyPolicyForce6)
	// the PROCESS is set synchronously -- that half is not deferred
	if got := GetControlIpFamilyPolicy(); got != IpFamilyPolicyForce6 {
		t.Fatalf("process policy is %d, want force6", got)
	}

	// the FILE is not. NetworkSpace.SetControlIpFamilyPolicy hands the write to
	// asyncLocalState.serialAsync, which runs it on a worker goroutine, so
	// reading the file on the next line races the write and fails most of the
	// time. Poll, bounded -- the same shape as log_verbosity_test.go:556-565.
	localState := newLocalState(ctx, storagePath)
	persisted := false
	for i := 0; i < 100; i += 1 {
		if got, ok := localState.controlIpFamilyPolicyIfSet(); ok && got == IpFamilyPolicyForce6 {
			persisted = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !persisted {
		t.Fatal("force6 was never persisted, so a relaunch comes back up under auto")
	}
}

func TestUnsetPolicyDoesNotOverrideTheProcessValue(t *testing.T) {
	defer SetControlIpFamilyPolicy(IpFamilyPolicyAuto)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	localState := newLocalState(ctx, t.TempDir())
	if _, ok := localState.controlIpFamilyPolicyIfSet(); ok {
		t.Fatal("a fresh local state reports a policy it was never given")
	}
	SetControlIpFamilyPolicy(IpFamilyPolicyForce4)
	if _, applied := applyPersistedControlIpFamilyPolicy(localState, nil); applied {
		t.Fatal("an unset policy must not be applied")
	}
	if got := GetControlIpFamilyPolicy(); got != IpFamilyPolicyForce4 {
		t.Fatalf("policy is %d, want the process value left alone", got)
	}
}

// newTestDeviceRemoteWithNoService builds a DeviceRemote whose rpc address has
// nothing listening on it: the ios regime where the app process is up and the
// tunnel process is down. It composes the same pieces the log verbosity tests
// construct inline -- testing_newNetworkSpace, defaultDeviceRpcSettings moved
// onto a free port (never the fixed production default, which collides with a
// concurrent suite run), and testing_deviceRpcDialer -- and ties the context
// and the device to t.
func newTestDeviceRemoteWithNoService(t *testing.T) *DeviceRemote {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}
	return newTestDeviceRemoteWithNoServiceInSpace(t, networkSpace, byJwt)
}

// newTestDeviceRemoteWithNoServiceInSpace is newTestDeviceRemoteWithNoService
// over a caller-supplied space, for the tests that have to seed that space's
// local state before the device remote is constructed.
func newTestDeviceRemoteWithNoServiceInSpace(t *testing.T, networkSpace *NetworkSpace, byJwt string) *DeviceRemote {
	t.Helper()

	settings := defaultDeviceRpcSettings()
	settings.Address = requireRemoteAddress(testing_freeHostPort())

	deviceRemote, err := newDeviceRemoteWithOverrides(
		networkSpace,
		byJwt,
		NewId(),
		settings,
		connect.NewId(),
		testing_deviceRpcDialer(settings),
	)
	if err != nil {
		t.Fatalf("device remote: %v", err)
	}
	t.Cleanup(deviceRemote.Close)

	if deviceRemote.GetRemoteConnected() {
		t.Fatal("the test device remote found a service, so the tunnel-down path is not the one under test")
	}
	return deviceRemote
}

// Both Device implementations are compile-time asserted, so a missing method
// is a build failure -- but the QUEUE behavior is not, and it is what covers
// the ios regime where the tunnel is down. A policy set with no rpc service
// must be replayed to the device when one appears, or the extension keeps
// dialing under the old policy while the menu reads back the new one.
func TestDeviceRemoteQueuesThePolicyWhenTheTunnelIsDown(t *testing.T) {
	defer SetControlIpFamilyPolicy(IpFamilyPolicyAuto)
	deviceRemote := newTestDeviceRemoteWithNoService(t)

	deviceRemote.SetControlIpFamilyPolicy(IpFamilyPolicyForce4)

	if got := GetControlIpFamilyPolicy(); got != IpFamilyPolicyForce4 {
		t.Fatalf("this process is at %d, want force4 -- the app process dials while the tunnel is down", got)
	}
	// FIELDS, not methods. deviceRemoteValue[T] (device_rpc.go:5806) is
	//   struct { Value T; IsSet bool }
	// with exactly one accessor, Get(defaultValue T) T -- so `.IsSet()` does
	// not compile and `.Get()` is missing its argument. Read under stateLock,
	// copying the pattern at log_verbosity_test.go:303-307 (and :243).
	deviceRemote.stateLock.Lock()
	queued := deviceRemote.state.ControlIpFamilyPolicy
	deviceRemote.stateLock.Unlock()
	if !queued.IsSet {
		t.Fatal("the policy was not queued for replay")
	}
	if queued.Value != IpFamilyPolicyForce4 {
		t.Fatalf("queued %d, want force4", queued.Value)
	}
}

// TestDeviceRemoteControlIpFamilyPolicyCrossesToTheDeviceProcess is the test
// the queue test above cannot be: the rpc handler's argument shape is a
// RUNTIME contract, not a compile-time one. RpcVoid is already `*any`
// (device_rpc.go:7556), so a handler written `_ *RpcVoid` builds cleanly, is
// registered by net/rpc, and simply fails every call -- and the tunnel-down
// test still passes, because it never reaches a live service at all.
//
// This drives DeviceRemote.SetControlIpFamilyPolicy over a LIVE rpc, the call
// the app actually makes, and looks for the policy in the DEVICE's own
// storage. The two devices share this test process, so the process-global
// policy proves nothing; they do NOT share a network space, so a policy in the
// device's file can only have come across the rpc.
func TestDeviceRemoteControlIpFamilyPolicyCrossesToTheDeviceProcess(t *testing.T) {
	defer SetControlIpFamilyPolicy(IpFamilyPolicyAuto)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, deviceRemote, localSpaceState, _ := testing_newSyncedDeviceLocalRemoteSeparateSpaces(t, ctx)

	deviceRemote.SetControlIpFamilyPolicy(IpFamilyPolicyForce6)

	// delivered now, over the live connection, not left for a later sync
	deviceRemote.stateLock.Lock()
	pending := deviceRemote.state.ControlIpFamilyPolicy.IsSet
	deviceRemote.stateLock.Unlock()
	if pending {
		t.Fatal("the policy is queued for a later sync, so the connected device never took the call")
	}

	if !awaitPersistedControlIpFamilyPolicy(t, localSpaceState, IpFamilyPolicyForce6) {
		t.Fatal("the device process never recorded the policy, so nothing crossed the rpc")
	}
}

// The user's real order of operations on ios: force a family from the
// developer menu with the tunnel down, then start the tunnel. The extension
// reads its own Documents container, so the file the app wrote is one it never
// opens -- the queued sync state is the only thing that carries the policy,
// and this covers the apply on the device side of the sync
// (`DeviceLocalRpc.syncState`), which the live-rpc test above bypasses.
func TestDeviceRemoteQueuedPolicyCrossesWhenTheTunnelComesUp(t *testing.T) {
	defer SetControlIpFamilyPolicy(IpFamilyPolicyAuto)

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

	deviceRemote.SetControlIpFamilyPolicy(IpFamilyPolicyForce6)

	// stand in for the tunnel process starting: a fresh device whose own
	// storage has never held a policy
	deviceLocal, err := newDeviceLocalWithOverrides(
		localSpace, localByJwt, "", "", "", instanceId, testDeviceLocalSettingsRpc(), clientId,
	)
	if err != nil {
		t.Fatalf("device local: %v", err)
	}
	defer deviceLocal.Close()
	if _, ok := localSpaceState.controlIpFamilyPolicyIfSet(); ok {
		t.Fatal("the device storage already holds a policy, so a crossing would be unobservable")
	}

	deviceRemote.Sync()
	if !deviceRemote.waitForSync(15 * time.Second) {
		t.Fatal("device remote did not sync after the device came up")
	}

	if !awaitPersistedControlIpFamilyPolicy(t, localSpaceState, IpFamilyPolicyForce6) {
		t.Fatal("the tunnel came up under the old policy, so the extension keeps dialing the stuck family")
	}
}

// A policy this process restored from its own storage is re-queued for the
// device process at construction: on ios the app's copy and the extension's
// are separate files in separate containers, so a reinstall or a cleared
// extension container would otherwise leave the extension on auto while the
// app reported the family the user forced.
func TestDeviceRemoteRestoredPolicyIsQueuedForTheDevice(t *testing.T) {
	defer SetControlIpFamilyPolicy(IpFamilyPolicyAuto)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}

	// the policy a previous app session chose
	if err := networkSpace.GetAsyncLocalState().GetLocalState().SetControlIpFamilyPolicy(IpFamilyPolicyForce4); err != nil {
		t.Fatalf("SetControlIpFamilyPolicy: %v", err)
	}
	SetControlIpFamilyPolicy(IpFamilyPolicyAuto)

	deviceRemote := newTestDeviceRemoteWithNoServiceInSpace(t, networkSpace, byJwt)

	if got := deviceRemote.GetControlIpFamilyPolicy(); got != IpFamilyPolicyForce4 {
		t.Fatalf("this process is at %d after constructing the device remote, want force4", got)
	}
	deviceRemote.stateLock.Lock()
	queued := deviceRemote.state.ControlIpFamilyPolicy
	deviceRemote.stateLock.Unlock()
	if !queued.IsSet || queued.Value != IpFamilyPolicyForce4 {
		t.Fatalf("queued %+v, want force4 set -- the extension comes up on auto", queued)
	}
}

// awaitPersistedControlIpFamilyPolicy waits for a policy to land in one local
// state. The device persists asynchronously (serialAsync), so the write trails
// the call that caused it.
func awaitPersistedControlIpFamilyPolicy(t *testing.T, localState *LocalState, want int) bool {
	t.Helper()
	for i := 0; i < 500; i += 1 {
		if got, ok := localState.controlIpFamilyPolicyIfSet(); ok && got == want {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// A space with nothing persisted must not spend the manager's one restore.
//
// Two configured spaces is the normal case on this branch -- a custom api host
// alongside the production one -- and only one of them need ever have had a
// policy written. If the guard were spent by the first space the restore was
// offered, regardless of whether it applied anything, the space that DOES hold
// the user's force would never get to restore it: the process would keep
// dialing that space's api host under whatever the other space's silence left
// in place, until the next launch.
func TestASpaceWithNoPersistedPolicyDoesNotSpendTheRestore(t *testing.T) {
	defer SetControlIpFamilyPolicy(IpFamilyPolicyAuto)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	storagePath := t.TempDir()
	// the bundled space, active at boot, with no policy ever written
	bundledKey := *NewNetworkSpaceKey("bringyour.com", "main")
	// the custom space, holding the force from an earlier session
	customKey := *NewNetworkSpaceKey("custom.example", "main")

	seedPersistedControlIpFamilyPolicy(t, ctx, storagePath, &customKey, IpFamilyPolicyForce6)
	writeNetworkSpaceIndex(t, storagePath, []NetworkSpaceKey{bundledKey, customKey}, &bundledKey)

	SetControlIpFamilyPolicy(IpFamilyPolicyAuto)
	networkSpaceManager := newNetworkSpaceManagerWithContext(ctx, storagePath)
	defer networkSpaceManager.Close()

	// the active space has nothing to say, so nothing changes -- and nothing
	// is spent either
	if got := GetControlIpFamilyPolicy(); got != IpFamilyPolicyAuto {
		t.Fatalf("policy is %d after construction, want auto -- the bundled space has no policy to restore", got)
	}

	// the in-session switch: android NetworkServerSelector, ios
	// setActiveNetworkSpace
	networkSpaceManager.SetActiveNetworkSpace(networkSpaceManager.GetNetworkSpace(&customKey))
	if got := GetControlIpFamilyPolicy(); got != IpFamilyPolicyForce6 {
		t.Fatalf("policy is %d after switching to the custom space, want force6 -- "+
			"the space with no persisted policy spent the restore", got)
	}

	// and now that a policy HAS been applied the guard is closed: the bug the
	// guard exists for must not come back with it
	SetControlIpFamilyPolicy(IpFamilyPolicyAuto)
	networkSpaceManager.SetActiveNetworkSpace(networkSpaceManager.GetNetworkSpace(&bundledKey))
	networkSpaceManager.SetActiveNetworkSpace(networkSpaceManager.GetNetworkSpace(&customKey))
	if got := GetControlIpFamilyPolicy(); got != IpFamilyPolicyAuto {
		t.Fatalf("policy is %d after re-selecting the custom space, want auto -- "+
			"a restore re-fired over a value set at runtime", got)
	}
}

// The status is the one half of the family pair that DeviceRemote cannot
// answer from its own process, and this pins that it asks the device.
//
// The assertion is on the SESSION, not on the value. Both devices share this
// test process, so they share connect's demotion ledger and the two answers
// are equal whether the rpc was used or not -- but the rpc handler's argument
// shape is a RUNTIME contract, not a compile-time one. RpcNoArg is `int`
// (device_rpc.go), so a handler written with the wrong argument or result
// shape still compiles, is still registered by net/rpc, and simply fails every
// call -- and `rpcCallNoArg` hands that failure to `closeService`, which drops
// the whole rpc session. The fallback then returns this process's own status
// and the caller sees a plausible answer over a torn-down tunnel rpc.
func TestDeviceRemoteControlIpFamilyStatusAsksTheDeviceProcess(t *testing.T) {
	defer SetControlIpFamilyPolicy(IpFamilyPolicyAuto)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	deviceLocal, deviceRemote, _, _ := testing_newSyncedDeviceLocalRemoteSeparateSpaces(t, ctx)

	status := deviceRemote.GetControlIpFamilyStatus()

	if !deviceRemote.GetRemoteConnected() {
		t.Fatal("the status call tore the rpc session down, so the device's ledger is unreachable " +
			"and every later call falls back to the app process's own")
	}
	if status != deviceLocal.GetControlIpFamilyStatus() {
		t.Fatalf("status is %q, want the device process's %q", status, deviceLocal.GetControlIpFamilyStatus())
	}
}

// With no device process to ask, the answer is this process's own ledger --
// which is the correct one, not a degraded one: the tunnel is down, so this
// process is the one dialing the control plane.
func TestDeviceRemoteControlIpFamilyStatusFallsBackToThisProcess(t *testing.T) {
	defer SetControlIpFamilyPolicy(IpFamilyPolicyAuto)
	deviceRemote := newTestDeviceRemoteWithNoService(t)

	if got := deviceRemote.GetControlIpFamilyStatus(); got != GetControlIpFamilyStatus() {
		t.Fatalf("status is %q with no service, want this process's %q", got, GetControlIpFamilyStatus())
	}
}

// testing_controlIpFamilyStatusRpc stands in for the DEVICE process's
// DeviceLocalRpc. It answers the one method under test with a string this
// process's own ledger cannot produce, and counts the calls.
type testing_controlIpFamilyStatusRpc struct {
	stateLock sync.Mutex
	calls     int
	answer    string
}

func (self *testing_controlIpFamilyStatusRpc) GetControlIpFamilyStatus(
	_ RpcNoArg,
	status *string,
) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.calls += 1
	*status = self.answer
	return nil
}

func (self *testing_controlIpFamilyStatusRpc) callCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.calls
}

// The value DeviceRemote.GetControlIpFamilyStatus returns must be the one the
// DEVICE process computed, not one this process answered locally. Replacing
// the method's whole body with `return GetControlIpFamilyStatus()` -- the
// local-answer implementation the finding was filed about -- is the mutation
// this pins.
//
// TestDeviceRemoteControlIpFamilyStatusAsksTheDeviceProcess above cannot pin
// it, and the reason is structural rather than an oversight: both devices live
// in this test process and therefore share connect's process-global demotion
// ledger, so the local answer and the crossed answer are the same string no
// matter which path ran. The session assertion there catches a handler whose
// runtime argument shape is wrong -- a real hazard, since RpcNoArg is `int`
// and a mis-shaped handler still compiles and registers -- but a body that
// never calls the rpc at all tears nothing down and is invisible to it.
//
// So the two ledgers are made to differ, which is what they do in production
// and the only thing this process cannot arrange with two real devices: the
// rpc peer here is a stub registered under the production method name, over
// the same net.Pipe + net/rpc pairing the rest of this package's rpc tests
// use, and it answers with a sentinel. The sentinel is a demotion string of
// the shape controlFamilyStatus emits, so nothing about the value itself
// makes the crossing detectable -- only its provenance does. This process's
// own status is asserted empty first, so the local answer is a distinct value
// rather than a coincidence.
//
// The call count is asserted too. It fails the mutation for a second,
// independent reason (a locally answered call invokes no handler), and it
// pins that the answer is fetched per call rather than cached -- a demotion
// expires on a timer, so a cached string goes wrong in the direction that
// matters.
//
// The two tests are complements, not duplicates: this one pins the crossing
// against a stub server, that one pins that the REAL DeviceLocalRpc answers
// the same method over a real session.
func TestDeviceRemoteControlIpFamilyStatusIsTheDeviceProcessAnswer(t *testing.T) {
	defer SetControlIpFamilyPolicy(IpFamilyPolicyAuto)

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	deviceProcess := &testing_controlIpFamilyStatusRpc{
		answer: "IPv6 demoted for 5m0s (2 strikes)",
	}
	server := rpc.NewServer()
	if err := server.RegisterName("DeviceLocalRpc", deviceProcess); err != nil {
		t.Fatal(err)
	}
	go server.ServeConn(serverConn)

	settings := defaultDeviceRpcSettings()
	service := &rpcClientWithTimeout{
		ctx:         context.Background(),
		log:         settings.logger(),
		timeout:     settings.RpcCallTimeout,
		closeClient: clientConn.Close,
		client:      rpc.NewClient(clientConn),
	}
	defer service.Close()

	// this process has nothing demoted, so the local answer is the empty
	// string and cannot be mistaken for the device process's
	if local := GetControlIpFamilyStatus(); local != "" {
		t.Fatalf(
			"this process's own status is %q, want empty -- the local and the "+
				"crossed answer must be distinguishable for this test to mean anything",
			local)
	}

	deviceRemote := newTestDeviceRemoteWithNoService(t)
	func() {
		deviceRemote.stateLock.Lock()
		defer deviceRemote.stateLock.Unlock()
		deviceRemote.service = service
		deviceRemote.remoteConnected = true
	}()

	status := deviceRemote.GetControlIpFamilyStatus()

	if status != deviceProcess.answer {
		t.Fatalf(
			"status is %q, want the device process's %q -- the app process "+
				"answered from its own ledger, which on ios is empty for the "+
				"whole time the tunnel is up and the extension is the one dialing",
			status, deviceProcess.answer)
	}
	if calls := deviceProcess.callCount(); calls != 1 {
		t.Fatalf("the device process's handler ran %d times, want exactly 1", calls)
	}
	if !deviceRemote.GetRemoteConnected() {
		t.Fatal("the status call tore the rpc session down, so the device's ledger is " +
			"unreachable and every later call falls back to the app process's own")
	}

	// fetched per call, not cached: the second call reaches the handler too
	if status := deviceRemote.GetControlIpFamilyStatus(); status != deviceProcess.answer {
		t.Fatalf("second status is %q, want %q", status, deviceProcess.answer)
	}
	if calls := deviceProcess.callCount(); calls != 2 {
		t.Fatalf("the device process's handler ran %d times over two calls, want 2 -- "+
			"a cached status goes stale when a demotion expires", calls)
	}
}
