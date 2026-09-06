//go:build !ios_extension

// The shared local controller must not bypass its device's store ownership or
// advertise a command outcome before the checked mutation actually commits.
package sdk

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/urnetwork/connect"
)

// A real controller over an actual accepted device, without native UI savers.
func testingPreferenceController(t *testing.T, device *DeviceLocal) *ConnectViewController {
	t.Helper()
	controller := newConnectViewController(device.Ctx(), device)
	controller.testingWindowMonitor = newTestingGridWindowMonitor()
	t.Cleanup(controller.Close)
	return controller
}

func TestConnectViewControllerAutoSaveFirstWriteFailureDoesNotClaimConnect(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	controller := testingPreferenceController(t, device)
	fixture.localState.testingBeforeLocationCommit = func(name string) error {
		if name == localConnectLocationFileName {
			return errors.New("private write failure")
		}
		return nil
	}
	controller.Connect(testingSpecificPreferenceLocation())
	if controller.GetConnectionStatus() != Disconnected || controller.GetConnected() ||
		testingPreferenceConsumer(device) != nil || device.GetConnectLocation() != nil {
		t.Fatal("failed current commit was displayed as a successful connection command")
	}
	if stored, err := fixture.localState.LoadConnectLocation(); err != nil || stored != nil {
		t.Fatal("failed current write created intent")
	}
	if stored, err := fixture.localState.LoadDefaultLocation(); err != nil || stored != nil || device.GetDefaultLocation() != nil {
		t.Fatal("failed current command continued into default persistence")
	}
	if result := device.GetLastLocalStateSaveResult(); result == nil || result.GetSaved() || result.GetError() != "save connect-location" {
		t.Fatal("controller lost its checked operation failure")
	}
}

func TestConnectViewControllerAutoSaveRetiredOwnerCannotWriteAfterReset(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	oldDevice := testingPreferenceDevice(t, fixture)
	if err := oldDevice.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	oldController := testingPreferenceController(t, oldDevice)
	oldLocation := testingSpecificPreferenceLocation()
	oldController.Connect(oldLocation)
	snapshot, err := fixture.networkSpace.GetAuthStateSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	reset, err := fixture.networkSpace.ResetLocalStateIfCurrent(snapshot)
	if err != nil || reset == nil || !reset.GetReset() {
		t.Fatal("actual paired owner reset failed")
	}
	fixture.seedDistinctLogin(t)
	newDevice := testingPreferenceDevice(t, fixture)
	if err := newDevice.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	newController := testingPreferenceController(t, newDevice)
	newLocation := testingSpecificPreferenceLocation()
	newController.Connect(newLocation)
	newConsumer := testingPreferenceConsumer(newDevice)
	// Delayed old callbacks cannot bypass the checked owner through LocalState.
	oldController.Connect(oldLocation)
	oldController.Disconnect()
	if stored, err := fixture.localState.LoadConnectLocation(); err != nil || !connectLocationValuesEqual(stored, newLocation) {
		t.Fatal("old controller bypassed owner validation and overwrote newer current routing")
	}
	if stored, err := fixture.localState.LoadDefaultLocation(); err != nil || !connectLocationValuesEqual(stored, newLocation) {
		t.Fatal("old controller bypassed owner validation and overwrote newer default routing")
	}
	if newConsumer == nil || testingPreferenceConsumer(newDevice) != newConsumer || !connectLocationValuesEqual(newDevice.GetConnectLocation(), newLocation) {
		t.Fatal("old callback disturbed the new accepted consumer")
	}
}

func TestConnectViewControllerAutoSaveFailedDisconnectRebindsLiveGrid(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	controller := testingPreferenceController(t, device)
	location := testingSpecificPreferenceLocation()
	controller.Connect(location)
	consumer := testingPreferenceConsumer(device)
	oldGrid := controller.GetGrid()
	path := filepath.Join(fixture.localState.localStorageDir, localConnectLocationFileName)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".preserved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, LocalStorageDirectoryPermissions); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "blocked"), []byte("occupied"), LocalStorageFilePermissions); err != nil {
		t.Fatal(err)
	}
	controller.Disconnect()
	grid := controller.GetGrid()
	if grid == nil || grid == oldGrid || !controller.generationCurrent(grid.generation) || !controller.GetConnected() ||
		controller.GetConnectionStatus() == Disconnected || testingPreferenceConsumer(device) != consumer {
		t.Fatal("failed disconnect froze or discarded the retained live connection's grid")
	}
	if result := device.GetLastLocalStateSaveResult(); result == nil || result.GetSaved() || result.GetError() != "save connect-location" {
		t.Fatal("failed disconnect was recorded as durable success")
	}
	if data, err := os.ReadFile(path + ".preserved"); err != nil || !bytes.Equal(data, original) {
		t.Fatal("failed disconnect destroyed previous destination bytes")
	}
	// Exercise the actual rebuilt grid's monitor callback after the failed
	// command: keeping only the old text status would leave this event gated.
	providerId := connect.NewId()
	monitor := controller.testingWindowMonitor.(*testing_gridWindowMonitor)
	monitor.mu.Lock()
	monitor.windowExpandEvent = connect.WindowExpandEvent{TargetSize: 1, MinSatisfied: true}
	monitor.mu.Unlock()
	monitor.emit(map[connect.Id]*connect.ProviderEvent{providerId: {ClientId: providerId, State: connect.ProviderStateAdded}})
	if controller.GetConnectionStatus() != Connected || grid.GetProviderGridPointList().Len() != 1 {
		t.Fatal("retained grid stopped accepting current provider truth")
	}
	monitor.mu.Lock()
	monitor.windowExpandEvent = connect.WindowExpandEvent{TargetSize: 1, MinSatisfied: false, Failed: true}
	monitor.mu.Unlock()
	monitor.emit(map[connect.Id]*connect.ProviderEvent{providerId: {ClientId: providerId, State: connect.ProviderStateRemoved}})
	if controller.GetConnectionStatus() != ConnectFailed {
		t.Fatal("retained grid did not accept a later current failure")
	}
}

func TestConnectViewControllerAutoSaveHealthyConnectReconnectDisconnect(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	controller := testingPreferenceController(t, device)
	location := testingSpecificPreferenceLocation()
	controller.Connect(location)
	consumer := testingPreferenceConsumer(device)
	if consumer == nil {
		t.Fatal("healthy explicit controller connect did not construct a consumer")
	}
	controller.Connect(cloneConnectLocation(location))
	if next := testingPreferenceConsumer(device); next == nil || next == consumer {
		t.Fatal("explicit equal connect lost its deliberate reconnect behavior")
	}
	if stored, err := fixture.localState.LoadConnectLocation(); err != nil || !connectLocationValuesEqual(stored, location) {
		t.Fatal("healthy current choice was not saved")
	}
	controller.Disconnect()
	if controller.GetConnectionStatus() != Disconnected || controller.GetConnected() || testingPreferenceConsumer(device) != nil {
		t.Fatal("healthy explicit disconnect did not stop the consumer")
	}
	if stored, err := fixture.localState.LoadConnectLocation(); err != nil || stored != nil {
		t.Fatal("healthy explicit disconnect was not durable")
	}
	if stored, err := fixture.localState.LoadDefaultLocation(); err != nil || !connectLocationValuesEqual(stored, location) {
		t.Fatal("disconnect erased the separately selected default")
	}
}

func TestConnectViewControllerAutoSaveDefaultFailureKeepsCommittedCurrent(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	controller := testingPreferenceController(t, device)
	fixture.localState.testingBeforeLocationCommit = func(name string) error {
		if name == localDefaultLocationFileName {
			return errors.New("default interruption")
		}
		return nil
	}
	location := testingSpecificPreferenceLocation()
	controller.Connect(location)
	if stored, err := fixture.localState.LoadConnectLocation(); err != nil || !connectLocationValuesEqual(stored, location) ||
		!connectLocationValuesEqual(device.GetConnectLocation(), location) || testingPreferenceConsumer(device) == nil {
		t.Fatal("default failure rolled back the durably accepted current route")
	}
	if stored, err := fixture.localState.LoadDefaultLocation(); err != nil || stored != nil {
		t.Fatal("failed default was incorrectly committed")
	}
	if result := device.GetLastLocalStateSaveResult(); result == nil || result.GetSaved() || result.GetError() != "save default-location" {
		t.Fatal("partial current/default operation hid the default failure")
	}
}
