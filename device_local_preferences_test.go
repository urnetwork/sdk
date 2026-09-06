// Real separate stores, accepted DeviceLocal owners, and cold reconstruction
// exercise preference durability independently of any native persistence listener.
package sdk

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Reopens through the public manager and its actual per-space directory.
// Candidate enumeration is empty; no external API, VPN, or UI is required.
func testingPreferenceSpaceAt(t *testing.T, directory string) (*NetworkSpaceManager, *testingAuthClientShape) {
	t.Helper()
	manager := NewNetworkSpaceManager(directory)
	t.Cleanup(manager.Close)
	key := NewNetworkSpaceKey("preference-state.test", "test")
	space := manager.GetNetworkSpace(key)
	if space == nil {
		space = manager.UpdateNetworkSpaceValues(key, &NetworkSpaceValues{
			ApiUrl: "http://127.0.0.1:1", PlatformUrl: "ws://127.0.0.1:1",
		})
	}
	api := space.GetApi()
	api.tokenManager.Close()
	testingAwaitAuthBoundary(t, api.tokenManager.done)
	return manager, &testingAuthClientShape{
		networkSpace: space, localState: space.asyncLocalState.localState, api: api,
		initialJwt: testingRefreshableJwtWithMarker(t, "preference-owner"), instanceId: NewId(),
	}
}

// Uses stored auth, never a test-created replacement identity on cold reopen.
func testingPreferenceDevice(t *testing.T, fixture *testingAuthClientShape) *DeviceLocal {
	t.Helper()
	auth, err := fixture.networkSpace.GetAuthStateSnapshot()
	if err != nil || auth.GetByClientJwt() == "" || auth.GetInstanceId() == nil {
		t.Fatal("accepted stored provider identity is unavailable")
	}
	settings := DefaultDeviceLocalSettings()
	settings.AllowProvider = false
	settings.EnableRpc = false
	settings.DisableLogging = true
	settings.GeneratorFunc = func([]*connect.ProviderSpec) connect.MultiClientGenerator {
		return &testingDnsOwnerGenerator{}
	}
	device, err := newDeviceLocalWithOverrides(fixture.networkSpace, auth.GetByClientJwt(),
		"preference-test", "test", "0", auth.GetInstanceId(), settings, connect.NewId())
	if err != nil {
		t.Fatal("actual accepted device construction failed")
	}
	device.SetUpgradeMuxSettings(nil)
	t.Cleanup(func() { testingJoinPreferenceDevice(t, device) })
	return device
}

// Joins the real client/generator/API publication lifecycle before reopening.
func testingJoinPreferenceDevice(t *testing.T, device *DeviceLocal) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := device.CloseAndWait(ctx); err != nil {
		t.Fatal("preference device did not join")
	}
}

// An exact location id, not a renamed best-available selector.
func testingSpecificPreferenceLocation() *ConnectLocation {
	return &ConnectLocation{ConnectLocationId: &ConnectLocationId{LocationId: NewId()}, Name: "saved-specific"}
}

// Counts actual DeviceLocal location notifications, including equal replay.
type testingPreferenceLocationListener struct{ count atomic.Int64 }

func (self *testingPreferenceLocationListener) ConnectLocationChanged(*ConnectLocation) {
	self.count.Add(1)
}

// Captures the actual installed consumer identity, not a fabricated enabled bit.
func testingPreferenceConsumer(device *DeviceLocal) connect.UserNatClient {
	device.stateLock.Lock()
	defer device.stateLock.Unlock()
	return device.remoteUserNatClient
}

func TestDeviceLocalAutoSaveFirstSpecificDestinationColdLoadWithoutUi(t *testing.T) {
	extensionDirectory := t.TempDir()
	extensionManager, extension := testingPreferenceSpaceAt(t, extensionDirectory)
	extension.seedDistinctLogin(t)
	_, app := testingPreferenceSpaceAt(t, t.TempDir())
	if err := app.localState.SetByJwt(extension.adminJwt); err != nil {
		t.Fatal(err)
	}
	if err := app.localState.SetByClientJwtForInstance(extension.initialJwt, extension.instanceId); err != nil {
		t.Fatal(err)
	}
	appOnly := testingSpecificPreferenceLocation()
	if err := app.localState.SetConnectLocation(appOnly); err != nil {
		t.Fatal(err)
	}
	device := testingPreferenceDevice(t, extension)
	if device.GetAutoSave() {
		t.Fatal("constructor implicitly enabled autosave")
	}
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	target := testingSpecificPreferenceLocation()
	device.SetConnectLocation(target)
	stored, err := extension.localState.LoadConnectLocation()
	if err != nil || !connectLocationValuesEqual(stored, target) || stored.ConnectLocationId.BestAvailable || stored.ConnectLocationId.LocationId == nil {
		t.Fatal("first destination was not committed in the extension's own store")
	}
	result := device.GetLastLocalStateSaveResult()
	if result == nil || !result.GetSaved() || result.GetError() != "" {
		t.Fatal("actual first save did not report its durable result")
	}
	consumer := testingPreferenceConsumer(device)
	if consumer == nil || !device.GetConnectEnabled() {
		t.Fatal("first mutation did not create a real consumer")
	}
	late := &testingPreferenceLocationListener{}
	sub := device.AddConnectLocationChangeListener(late)
	t.Cleanup(sub.Close)
	if err := device.SetConnectLocationChecked(cloneConnectLocation(target)); err != nil {
		t.Fatal(err)
	}
	if late.count.Load() != 0 || testingPreferenceConsumer(device) != consumer {
		t.Fatal("equal replay emitted a false edge or rebuilt a healthy consumer")
	}
	locationPath := filepath.Join(extension.localState.localStorageDir, localConnectLocationFileName)
	committedBytes, err := os.ReadFile(locationPath)
	if err != nil {
		t.Fatal(err)
	}
	testingJoinPreferenceDevice(t, device)
	if afterClose, err := os.ReadFile(locationPath); err != nil || !bytes.Equal(afterClose, committedBytes) {
		t.Fatal("Close persisted a transient disconnect")
	}
	extensionManager.Close()
	_, fresh := testingPreferenceSpaceAt(t, extensionDirectory)
	auth, err := fresh.localState.GetAuthStateSnapshot()
	if err != nil || auth.GetByJwt() != extension.adminJwt || auth.GetByClientJwt() != extension.initialJwt ||
		auth.GetInstanceId().Cmp(extension.instanceId) != 0 {
		t.Fatal("cold reopen changed auth or stable identity")
	}
	restored := testingPreferenceDevice(t, fresh)
	if restored.GetConnectLocation() != nil || testingPreferenceConsumer(restored) != nil {
		t.Fatal("constructor implicitly restored routing")
	}
	load, err := restored.Load()
	if err != nil || load == nil || !load.GetLoaded() || !load.GetHasConnectLocation() || load.GetDefaultError() != "" {
		t.Fatal("explicit cold Load failed")
	}
	if restored.GetAutoSave() || !connectLocationValuesEqual(restored.GetConnectLocation(), target) ||
		!restored.GetConnectEnabled() || testingPreferenceConsumer(restored) == nil {
		t.Fatal("Load did not restore the exact saved destination and real consumer independently of autosave")
	}
	if afterLoad, err := os.ReadFile(locationPath); err != nil || !bytes.Equal(afterLoad, committedBytes) {
		t.Fatal("Load rewrote saved intent")
	}
	if stillAppOnly, err := app.localState.LoadConnectLocation(); err != nil || !connectLocationValuesEqual(stillAppOnly, appOnly) {
		t.Fatal("extension persistence crossed into the app's independent store")
	}
}

func TestDeviceLocalDefaultOffLateListenerAndEqualReplayDoNotPersist(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	target := testingSpecificPreferenceLocation()
	device.SetConnectLocation(target)
	consumer := testingPreferenceConsumer(device)
	late := &testingPreferenceLocationListener{}
	sub := device.AddConnectLocationChangeListener(late)
	defer sub.Close()
	device.SetConnectLocation(cloneConnectLocation(target))
	if late.count.Load() != 0 || consumer == nil || testingPreferenceConsumer(device) != consumer {
		t.Fatal("late subscriber/equal value did not reproduce the original missed-edge shape")
	}
	if stored, err := fixture.localState.LoadConnectLocation(); err != nil || stored != nil {
		t.Fatal("default-off device implicitly persisted current intent")
	}
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	if stored, err := fixture.localState.LoadConnectLocation(); err != nil || stored != nil {
		t.Fatal("enabling autosave unexpectedly saved a current snapshot")
	}
	device.SetConnectLocation(cloneConnectLocation(target))
	if stored, err := fixture.localState.LoadConnectLocation(); err != nil || !connectLocationValuesEqual(stored, target) {
		t.Fatal("equal explicit enabled mutation did not repair missing durable intent")
	}
	if late.count.Load() != 0 || testingPreferenceConsumer(device) != consumer {
		t.Fatal("durability repair required a fake event or transport rebuild")
	}
}

func TestDeviceLocalAutoSaveCommitFailureIsVisibleAndPreservesLiveAndDisk(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	target := testingSpecificPreferenceLocation()
	if err := device.SetConnectLocationChecked(target); err != nil {
		t.Fatal(err)
	}
	consumer := testingPreferenceConsumer(device)
	fixture.localState.testingBeforeLocationCommit = func(string) error { return errors.New("private test path must not escape") }
	replacement := testingSpecificPreferenceLocation()
	err := device.SetConnectLocationChecked(replacement)
	if err == nil || strings.Contains(err.Error(), "private") {
		t.Fatal("checked setter hid the failure or exposed the underlying path")
	}
	result := device.GetLastLocalStateSaveResult()
	if result == nil || result.GetSaved() || !result.GetAutoSaveEnabled() || result.GetError() != "save connect-location" {
		t.Fatal("failed write was reported as a durable success")
	}
	if stored, err := fixture.localState.LoadConnectLocation(); err != nil || !connectLocationValuesEqual(stored, target) ||
		!connectLocationValuesEqual(device.GetConnectLocation(), target) || testingPreferenceConsumer(device) != consumer {
		t.Fatal("failed write lost the original destination or replaced the healthy consumer")
	}
	device.SetConnectLocation(replacement)
	if result := device.GetLastLocalStateSaveResult(); result.GetSaved() || result.GetError() == "" {
		t.Fatal("compatibility void setter concealed its failed save")
	}
	fixture.localState.testingBeforeLocationCommit = nil
	if err := device.SetConnectLocationChecked(cloneConnectLocation(target)); err != nil || testingPreferenceConsumer(device) != consumer {
		t.Fatal("equal-value retry did not preserve the healthy consumer")
	}
}

func TestDeviceLocalLoadRequiredErrorDoesNotMeanAbsentOrResetAuth(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	path := filepath.Join(fixture.localState.localStorageDir, localConnectLocationFileName)
	if err := os.WriteFile(path, []byte("{"), LocalStorageFilePermissions); err != nil {
		t.Fatal(err)
	}
	if result, err := device.Load(); err == nil || result != nil || err.Error() != "load connect location" {
		t.Fatal("malformed required state was treated as absence or partial success")
	}
	if device.GetConnectLocation() != nil || testingPreferenceConsumer(device) != nil {
		t.Fatal("failed Load created a fallback consumer")
	}
	auth, err := fixture.localState.GetAuthStateSnapshot()
	if err != nil || auth.GetByJwt() != fixture.adminJwt || auth.GetByClientJwt() != fixture.initialJwt {
		t.Fatal("required preference read error reset authentication")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "{" {
		t.Fatal("failed Load rewrote or removed the unreadable intent")
	}
}

func TestDeviceLocalLoadOptionalDefaultFailurePreservesCurrentAndExplicitDisconnect(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	target := testingSpecificPreferenceLocation()
	if err := fixture.localState.SetConnectLocation(target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.localState.localStorageDir, localDefaultLocationFileName), []byte("{"), LocalStorageFilePermissions); err != nil {
		t.Fatal(err)
	}
	result, err := device.Load()
	if err != nil || result == nil || !result.GetHasConnectLocation() || result.GetHasDefaultLocation() ||
		result.GetDefaultError() != "load default location" || !connectLocationValuesEqual(device.GetConnectLocation(), target) {
		t.Fatal("unused bad default blocked current routing or appeared absent")
	}
	current, err := fixture.networkSpace.GetAuthStateSnapshot()
	if err != nil || current.SetConnectLocation(nil) != nil {
		t.Fatal("current explicit disconnect could not clear its own saved intent")
	}
	result, err = device.Load()
	if err != nil || result.GetHasConnectLocation() || result.GetDefaultError() == "" ||
		device.GetConnectEnabled() || testingPreferenceConsumer(device) != nil {
		t.Fatal("unused default blocked an explicit disconnect or revived old routing")
	}
}

func TestDeviceLocalLoadRejectsResetAfterReadBeforeAdmission(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	if err := fixture.localState.SetConnectLocation(testingSpecificPreferenceLocation()); err != nil {
		t.Fatal(err)
	}
	read := make(chan struct{})
	resume := make(chan struct{})
	device.testingBeforePreferenceApply = func() {
		close(read)
		<-resume
	}
	done := make(chan error, 1)
	go func() {
		result, err := device.Load()
		if result != nil {
			done <- errors.New("superseded Load returned a result")
			return
		}
		done <- err
	}()
	testingAwaitAuthBoundary(t, read)
	current, err := fixture.networkSpace.GetAuthStateSnapshot()
	if err != nil {
		close(resume)
		t.Fatal(err)
	}
	reset, err := fixture.networkSpace.ResetLocalStateIfCurrent(current)
	if err != nil || reset == nil || !reset.GetReset() {
		close(resume)
		t.Fatal("actual current owner reset failed")
	}
	newLocation := testingSpecificPreferenceLocation()
	if err := fixture.localState.SetByJwt(testingJwt(map[string]any{"network_name": "new-owner"})); err != nil {
		close(resume)
		t.Fatal(err)
	}
	if err := fixture.localState.SetByClientJwt(testingRefreshableJwtWithMarker(t, "new-owner")); err != nil {
		close(resume)
		t.Fatal(err)
	}
	if err := fixture.localState.SetConnectLocation(newLocation); err != nil {
		close(resume)
		t.Fatal(err)
	}
	close(resume)
	select {
	case err := <-done:
		if err == nil || err.Error() != localAuthSnapshotSupersededMessage {
			t.Fatal("old checked Load adopted after genuine owner replacement")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("held Load did not complete")
	}
	if testingPreferenceConsumer(device) != nil {
		t.Fatal("superseded observation constructed an old-account consumer")
	}
	if stored, err := fixture.localState.LoadConnectLocation(); err != nil || !connectLocationValuesEqual(stored, newLocation) {
		t.Fatal("superseded Load disturbed newer account intent")
	}
}

func TestDeviceLocalAutoSaveRejectsPausedOldMutationAfterReset(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	oldTarget := testingSpecificPreferenceLocation()
	if err := device.SetConnectLocationChecked(oldTarget); err != nil {
		t.Fatal(err)
	}
	prepared := make(chan struct{})
	resume := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(prepared)
		<-resume
		done <- device.SetConnectLocationChecked(oldTarget)
	}()
	testingAwaitAuthBoundary(t, prepared)
	current, err := fixture.networkSpace.GetAuthStateSnapshot()
	if err != nil {
		close(resume)
		t.Fatal(err)
	}
	reset, err := fixture.networkSpace.ResetLocalStateIfCurrent(current)
	if err != nil || reset == nil || !reset.GetReset() {
		close(resume)
		t.Fatal("genuine reset failed")
	}
	close(resume)
	select {
	case err := <-done:
		if err == nil || err.Error() != localAuthSnapshotSupersededMessage {
			t.Fatal("paused old callback reclaimed persistence after owner reset")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("old setter did not finish")
	}
	if stored, err := fixture.localState.LoadConnectLocation(); err != nil || stored != nil {
		t.Fatal("old callback recreated the cleared private destination")
	}
}

func TestDeviceLocalSameOwnerRenewalKeepsAutoSaveAndExactDestination(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	target := testingSpecificPreferenceLocation()
	if err := device.SetConnectLocationChecked(target); err != nil {
		t.Fatal(err)
	}
	consumer := testingPreferenceConsumer(device)
	renewed := testingRefreshableJwtWithMarker(t, "preference-renewed")
	if !fixture.api.setRefreshedByJwt(fixture.initialJwt, renewed) {
		t.Fatal("actual same-owner renewal was not accepted")
	}
	if err := device.SetConnectLocationChecked(cloneConnectLocation(target)); err != nil || testingPreferenceConsumer(device) != consumer {
		t.Fatal("same-owner renewal blocked save or rebuilt healthy routing")
	}
	if stored, err := fixture.localState.LoadConnectLocation(); err != nil || !connectLocationValuesEqual(stored, target) {
		t.Fatal("renewal lost the exact intended location")
	}
	auth, err := fixture.localState.GetAuthStateSnapshot()
	if err != nil || auth.GetByJwt() != fixture.adminJwt || auth.GetByClientJwt() != renewed || auth.GetInstanceId().Cmp(fixture.instanceId) != 0 {
		t.Fatal("preference persistence merged auth roles or changed stable instance")
	}
}

func TestDeviceLocalCloseJoinsHeldLoadWithoutSavingDisconnect(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	target := testingSpecificPreferenceLocation()
	if err := fixture.localState.SetConnectLocation(target); err != nil {
		t.Fatal(err)
	}
	read := make(chan struct{})
	resume := make(chan struct{})
	device.testingBeforePreferenceApply = func() {
		close(read)
		<-resume
	}
	done := make(chan error, 1)
	go func() { _, err := device.Load(); done <- err }()
	testingAwaitAuthBoundary(t, read)
	device.Close()
	device.authPublication.stateLock.Lock()
	activeOperations := device.authPublication.inFlightCount
	device.authPublication.stateLock.Unlock()
	if activeOperations == 0 {
		close(resume)
		t.Fatal("Close's real lifecycle gate does not own the held preference operation")
	}
	close(resume)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("closed device adopted the held Load")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("held Load failed to unwind")
	}
	testingJoinPreferenceDevice(t, device)
	if stored, err := fixture.localState.LoadConnectLocation(); err != nil || !connectLocationValuesEqual(stored, target) {
		t.Fatal("joined teardown persisted a transient nil destination")
	}
}

func TestDeviceLocalLoadAndAutoSaveRefuseUnownedHostedStorage(t *testing.T) {
	for _, device := range []*DeviceLocal{
		{settings: &DeviceLocalSettings{HostedIncompatible: true}},
		{settings: &DeviceLocalSettings{}},
	} {
		if err := device.SetAutoSave(true); err == nil || err.Error() != localPreferencesUnsupportedMessage {
			t.Fatal("unsupported device acquired persistence authority")
		}
		if result, err := device.Load(); err == nil || result != nil || err.Error() != localPreferencesUnsupportedMessage {
			t.Fatal("unsupported device loaded a shared or nonexistent tenant store")
		}
		if device.GetAutoSave() {
			t.Fatal("failed enable left autosave active")
		}
	}
}

func TestDeviceLocalLoadWhileAutoSaveEnabledDoesNotWriteOrToggleMode(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	target := testingSpecificPreferenceLocation()
	defaultLocation := testingSpecificPreferenceLocation()
	if err := fixture.localState.SetConnectLocation(target); err != nil {
		t.Fatal(err)
	}
	if err := fixture.localState.SetDefaultLocation(defaultLocation); err != nil {
		t.Fatal(err)
	}
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	var writes atomic.Int64
	fixture.localState.testingBeforeLocationCommit = func(string) error {
		writes.Add(1)
		return errors.New("Load must not write this existing preference")
	}
	result, err := device.Load()
	if err != nil || result == nil || !result.GetHasConnectLocation() || !result.GetHasDefaultLocation() ||
		result.GetDefaultError() != "" || !device.GetAutoSave() || writes.Load() != 0 {
		t.Fatal("enabled Load attempted an autosave or changed persistence mode")
	}
	if device.GetLastLocalStateSaveResult() != nil || !connectLocationValuesEqual(device.GetConnectLocation(), target) ||
		!connectLocationValuesEqual(device.GetDefaultLocation(), defaultLocation) || testingPreferenceConsumer(device) == nil {
		t.Fatal("Load did not directly apply the checked values without a save operation")
	}
}

func TestDeviceLocalDisablingAutoSavePreservesLastDurableSelection(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	durable := testingSpecificPreferenceLocation()
	if err := device.SetConnectLocationChecked(durable); err != nil {
		t.Fatal(err)
	}
	if err := device.SetAutoSave(false); err != nil {
		t.Fatal(err)
	}
	liveOnly := testingSpecificPreferenceLocation()
	if err := device.SetConnectLocationChecked(liveOnly); err != nil {
		t.Fatal(err)
	}
	result := device.GetLastLocalStateSaveResult()
	if device.GetAutoSave() || result == nil || result.GetAutoSaveEnabled() || result.GetSaved() || result.GetError() != "" ||
		!connectLocationValuesEqual(device.GetConnectLocation(), liveOnly) || testingPreferenceConsumer(device) == nil {
		t.Fatal("disabled mode did not distinguish a successful live-only mutation")
	}
	testingJoinPreferenceDevice(t, device)
	if stored, err := fixture.localState.LoadConnectLocation(); err != nil || !connectLocationValuesEqual(stored, durable) {
		t.Fatal("live-only mutation or Close replaced the last durable selection")
	}
}
