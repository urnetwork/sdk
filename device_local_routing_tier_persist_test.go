package sdk

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/urnetwork/connect"
)

// Autosave commits before return. A cold accepted owner remains at the
// constructor default until explicit Load, without rewriting the saved record.
func TestDeviceLocalRoutingTierPersistRestore(t *testing.T) {
	directory := t.TempDir()
	manager, fixture := testingPreferenceSpaceAt(t, directory)
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	connect.AssertEqual(t, RoutingTierOff, testingDeviceRoutingTier(device))
	if device.GetAutoSave() {
		t.Fatal("constructor implicitly enabled routing-tier persistence")
	}
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	device.SetRoutingTier(RoutingTierFull)
	result := device.GetLastLocalStateSaveResult()
	if result == nil || result.GetPreference() != "routing-tier" ||
		!result.GetAutoSaveEnabled() || !result.GetSaved() || result.GetError() != "" {
		t.Fatal("routing tier did not report a completed durable save")
	}
	connect.AssertEqual(t, RoutingTierFull, testingDeviceRoutingTier(device))
	connect.AssertEqual(t, RoutingTierFull, fixture.localState.GetRoutingTier())
	path := filepath.Join(fixture.localState.localStorageDir, ".routing_tier")
	committed, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(committed, []byte("2")) {
		t.Fatal("routing-tier setter returned before committing its JSON record")
	}
	testingJoinPreferenceDevice(t, device)
	manager.Close()

	_, fresh := testingPreferenceSpaceAt(t, directory)
	restored := testingPreferenceDevice(t, fresh)
	connect.AssertEqual(t, RoutingTierOff, testingDeviceRoutingTier(restored))
	loaded, err := restored.Load()
	if err != nil || loaded == nil || !loaded.GetLoaded() ||
		!loaded.GetHasPreference("routing-tier") || loaded.GetPreferenceError("routing-tier") != "" {
		t.Fatal("checked routing-tier restoration failed")
	}
	connect.AssertEqual(t, RoutingTierFull, testingDeviceRoutingTier(restored))
	if restored.GetAutoSave() || restored.GetLastLocalStateSaveResult() != nil || restored.GetConnectEnabled() {
		t.Fatal("Load enabled persistence, replayed a save, or invented a consumer")
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, committed) {
		t.Fatal("close or cold Load changed the saved routing-tier record")
	}
}

// Opting in is not a snapshot save. An explicit equal mutation must still
// commit a tier that was previously only live with persistence disabled.
func TestDeviceLocalRoutingTierDefaultOffRequiresExplicitEqualSave(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	path := filepath.Join(fixture.localState.localStorageDir, ".routing_tier")
	device.SetRoutingTier(RoutingTierFull)
	connect.AssertEqual(t, RoutingTierFull, testingDeviceRoutingTier(device))
	result := device.GetLastLocalStateSaveResult()
	if result == nil || result.GetPreference() != "routing-tier" ||
		result.GetAutoSaveEnabled() || result.GetSaved() || result.GetError() != "" {
		t.Fatal("default-off mutation did not report live-only success")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("default-off routing-tier mutation wrote a file")
	}
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("enabling autosave implicitly saved the routing tier")
	}
	device.SetRoutingTier(RoutingTierFull)
	saved := device.GetLastLocalStateSaveResult()
	if saved == nil || !saved.GetAutoSaveEnabled() || !saved.GetSaved() || saved.GetError() != "" ||
		saved.GetSequence() <= result.GetSequence() {
		t.Fatal("explicit equal routing-tier mutation did not commit")
	}
	connect.AssertEqual(t, RoutingTierFull, fixture.localState.GetRoutingTier())
}

// Observe the live field under its production lock; no exported tier getter
// is needed solely for this test.
func testingDeviceRoutingTier(device *DeviceLocal) int {
	device.stateLock.Lock()
	defer device.stateLock.Unlock()
	return device.routingTier
}
