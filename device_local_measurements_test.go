package sdk

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/urnetwork/connect"
)

// The owner's lag.dev measurements setting (lag.dev/PLAN.md C4, decision 18):
// on by default, persisted per network space beside the provider extender
// setting and kept across a sign-out, held by the device on a space with no
// local state, and carried over the device rpc as the extender setting is.

// A space on storage the test names, so a later manager can open the same
// storage again: a restart of the app.
func testMeasurementsSpace(t *testing.T, storagePath string) (*NetworkSpaceManager, *NetworkSpace) {
	t.Helper()
	networkSpaceManager := NewNetworkSpaceManager(storagePath)
	networkSpace := networkSpaceManager.updateNetworkSpace(
		NewNetworkSpaceKey("space.example", "main"),
		func(values *NetworkSpaceValues) {},
	)
	return networkSpaceManager, networkSpace
}

// Only an explicit true or false is stored; anything else reads as on, and the
// stored value survives a reload of local state.
func TestLocalStateMeasurementsEnabled(t *testing.T) {
	storagePath := t.TempDir()
	localState := newLocalState(context.Background(), storagePath)
	t.Cleanup(localState.Close)
	if !localState.GetMeasurementsEnabled() {
		t.Fatal("measurements are off by default")
	}
	if err := localState.SetMeasurementsEnabled(false); err != nil {
		t.Fatal(err)
	}
	if localState.GetMeasurementsEnabled() {
		t.Fatal("the opt-out did not take")
	}
	reloaded := newLocalState(context.Background(), storagePath)
	t.Cleanup(reloaded.Close)
	if reloaded.GetMeasurementsEnabled() {
		t.Fatal("the opt-out did not survive a reload")
	}

	for _, content := range []string{"", "nonsense", "FALSE "} {
		if err := os.WriteFile(
			filepath.Join(localState.localStorageDir, measurementsEnabledFileName),
			[]byte(content),
			LocalStorageFilePermissions,
		); err != nil {
			t.Fatal(err)
		}
		reread := newLocalState(context.Background(), storagePath)
		if !reread.GetMeasurementsEnabled() {
			t.Errorf("%q read as an opt-out, expected on", content)
		}
		reread.Close()
	}
}

// A sign-out keeps the setting, on disk and in memory, with the rest of the
// extender state.
func TestLocalStateLogoutKeepsTheMeasurementsSetting(t *testing.T) {
	localState := newLocalState(context.Background(), t.TempDir())
	t.Cleanup(localState.Close)
	seeded := testingSeedExtenderState(t, localState)
	if err := localState.SetMeasurementsEnabled(false); err != nil {
		t.Fatal(err)
	}
	if err := localState.Logout(); err != nil {
		t.Fatal(err)
	}
	testingRequireExtenderStateKept(t, localState, seeded)
	if localState.GetMeasurementsEnabled() {
		t.Fatal("the sign-out reset the measurements setting")
	}
	cold := newLocalState(context.Background(), filepath.Dir(localState.localStorageDir))
	defer cold.Close()
	if cold.GetMeasurementsEnabled() {
		t.Fatal("the sign-out removed the measurements setting")
	}
}

// A device reads the setting on by default, stores it in its space, and a
// device of a later start reads what the last one set.
func TestDeviceLocalMeasurementsEnabledPersists(t *testing.T) {
	storagePath := t.TempDir()
	networkSpaceManager, networkSpace := testMeasurementsSpace(t, storagePath)
	deviceLocal := testingProvideExtenderDevice(t, networkSpace, true)
	if !deviceLocal.GetMeasurementsEnabled() {
		t.Fatal("measurements are off by default")
	}
	deviceLocal.SetMeasurementsEnabled(false)
	if deviceLocal.GetMeasurementsEnabled() {
		t.Fatal("the opt-out did not take")
	}
	if networkSpace.asyncLocalState.GetLocalState().GetMeasurementsEnabled() {
		t.Fatal("the opt-out was not stored in the space")
	}
	deviceLocal.Close()
	networkSpaceManager.Close()

	restartedManager, restartedSpace := testMeasurementsSpace(t, storagePath)
	t.Cleanup(restartedManager.Close)
	restartedDevice := testingProvideExtenderDevice(t, restartedSpace, true)
	if restartedDevice.GetMeasurementsEnabled() {
		t.Fatal("the opt-out did not survive a restart")
	}
	restartedDevice.SetMeasurementsEnabled(true)
	if !restartedDevice.GetMeasurementsEnabled() {
		t.Fatal("measurements did not go back on")
	}
}

// On a space with no local state the value set is the device's own, for its
// life: another device on the space reads the default.
func TestDeviceLocalMeasurementsEnabledWithoutLocalStateIsTheDevicesOwn(t *testing.T) {
	networkSpace := testingProvideExtenderUrlSpace(t)
	deviceLocal := testingProvideExtenderDevice(t, networkSpace, true)
	deviceLocal.SetMeasurementsEnabled(false)
	if deviceLocal.GetMeasurementsEnabled() {
		t.Fatal("the opt-out did not take on a space with no local state")
	}
	otherDeviceLocal := testingProvideExtenderDevice(t, networkSpace, true)
	if !otherDeviceLocal.GetMeasurementsEnabled() {
		t.Fatal("another device on the space took the value set on the first")
	}
}

// The setting reads and writes through the rpc, in both directions.
func TestDeviceRemoteMeasurementsEnabledSetting(t *testing.T) {
	_, networkSpace := testExtenderStatusSpace(t)
	deviceLocal, deviceRemote := testExtenderStatusSyncedDeviceLocalRemote(t, networkSpace)

	connect.AssertEqual(t, deviceRemote.GetMeasurementsEnabled(), true)

	deviceRemote.SetMeasurementsEnabled(false)
	// the device process persisted it
	connect.AssertEqual(t, deviceLocal.GetMeasurementsEnabled(), false)
	connect.AssertEqual(t, deviceRemote.GetMeasurementsEnabled(), false)

	deviceLocal.SetMeasurementsEnabled(true)
	connect.AssertEqual(t, deviceRemote.GetMeasurementsEnabled(), true)
}

// Turned off while the daemon is down, the setting reads back off at once and
// reaches the device at the next sync.
func TestDeviceRemoteQueuesTheMeasurementsSettingWhileUnreachable(t *testing.T) {
	_, networkSpace := testExtenderStatusSpace(t)
	deviceRemote, bringUp := testExtenderProvideRemoteBeforeLocal(t, networkSpace)

	deviceRemote.SetMeasurementsEnabled(false)
	connect.AssertEqual(t, deviceRemote.GetMeasurementsEnabled(), false)
	if queued := testQueuedMeasurementsEnabled(deviceRemote); !queued.IsSet || queued.Value {
		t.Fatalf("queued = %+v, expected off queued for replay", queued)
	}

	deviceLocal := bringUp()
	connect.AssertEqual(t, deviceLocal.GetMeasurementsEnabled(), false)
	if queued := testQueuedMeasurementsEnabled(deviceRemote); queued.IsSet {
		t.Fatalf("queued = %+v after the sync, expected nothing left to replay", queued)
	}
	connect.AssertEqual(t, deviceRemote.GetMeasurementsEnabled(), false)
}

// The value the sync seeds is what a remote that never read or wrote the
// setting answers once the device process is gone.
func TestDeviceRemoteMeasurementsEnabledSeededBySync(t *testing.T) {
	_, networkSpace := testExtenderStatusSpace(t)
	if err := networkSpace.asyncLocalState.GetLocalState().SetMeasurementsEnabled(false); err != nil {
		t.Fatal(err)
	}
	deviceLocal, deviceRemote := testExtenderStatusSyncedDeviceLocalRemote(t, networkSpace)
	connect.AssertEqual(t, deviceLocal.GetMeasurementsEnabled(), false)

	deviceLocal.Close()
	testWaitRemoteDisconnected(t, deviceRemote)

	if queued := testQueuedMeasurementsEnabled(deviceRemote); queued.IsSet {
		t.Fatalf("queued = %+v, expected nothing queued", queued)
	}
	connect.AssertEqual(t, deviceRemote.GetMeasurementsEnabled(), false)
}

// The value a remote has queued for the device process.
func testQueuedMeasurementsEnabled(deviceRemote *DeviceRemote) deviceRemoteValue[bool] {
	deviceRemote.stateLock.Lock()
	defer deviceRemote.stateLock.Unlock()
	return deviceRemote.state.MeasurementsEnabled
}
