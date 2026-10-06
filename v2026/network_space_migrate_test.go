package sdk

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// the apps' bundled space before the operator decision: key host ur.network
// with every service resolved under the migration host bringyour.com
func testingMigrateFromKey() *NetworkSpaceKey {
	return NewNetworkSpaceKey("ur.network", "main")
}

func testingMigrateToKey() *NetworkSpaceKey {
	return NewNetworkSpaceKey("bringyour.com", "main")
}

func testingMigrateCustomKey() *NetworkSpaceKey {
	return NewNetworkSpaceKey("custom.example.com:8443", "main")
}

// a manager with the pre-migration bundled space active, a custom space, and
// a marker file inside the bundled space's storage
func testingMigrateManager(t *testing.T) (*NetworkSpaceManager, string) {
	t.Helper()
	storagePath := t.TempDir()

	networkSpaceManager := NewNetworkSpaceManager(storagePath)
	bundled := networkSpaceManager.updateNetworkSpace(
		testingMigrateFromKey(),
		func(values *NetworkSpaceValues) {
			values.Bundled = true
			values.EnvSecret = "bundled-secret"
			values.LinkHostName = "ur.io"
			values.MigrationHostName = "bringyour.com"
			values.Store = "store"
			values.Wallet = "wallet"
			values.SsoGoogle = true
		},
	)
	networkSpaceManager.updateNetworkSpace(
		testingMigrateCustomKey(),
		func(values *NetworkSpaceValues) {
			values.EnvSecret = "custom-secret"
		},
	)
	networkSpaceManager.SetActiveNetworkSpace(bundled)

	markerPath := filepath.Join(bundled.storagePath, "device_state_marker")
	connect.AssertEqual(t, os.WriteFile(markerPath, []byte("device state"), LocalStorageFilePermissions), nil)

	return networkSpaceManager, storagePath
}

func testingReadNetworkSpaceManagerState(t *testing.T, storagePath string) *networkSpaceManagerState {
	t.Helper()
	stateBytes, err := os.ReadFile(filepath.Join(storagePath, ".network_spaces"))
	connect.AssertEqual(t, err, nil)
	state := &networkSpaceManagerState{}
	connect.AssertEqual(t, json.Unmarshal(stateBytes, state), nil)
	return state
}

func testingStateKeys(state *networkSpaceManagerState) map[NetworkSpaceKey]NetworkSpaceValues {
	keys := map[NetworkSpaceKey]NetworkSpaceValues{}
	for _, networkSpaceState := range state.NetworkSpaces {
		keys[networkSpaceState.Key] = networkSpaceState.Values
	}
	return keys
}

type testingNetworkSpacesChangeCounter struct {
	count atomic.Int32
}

func (self *testingNetworkSpacesChangeCounter) NetworkSpacesChanged() {
	self.count.Add(1)
}

type testingActiveNetworkSpaceRecorder struct {
	count  atomic.Int32
	active atomic.Pointer[NetworkSpace]
}

func (self *testingActiveNetworkSpaceRecorder) ActiveNetworkSpaceChanged(networkSpace *NetworkSpace) {
	self.count.Add(1)
	self.active.Store(networkSpace)
}

func TestNetworkSpaceManagerMigrateNetworkSpaceMovesStorageStateAndActive(t *testing.T) {
	networkSpaceManager, storagePath := testingMigrateManager(t)
	defer networkSpaceManager.Close()

	fromKey := testingMigrateFromKey()
	toKey := testingMigrateToKey()
	fromStoragePath := filepath.Join(storagePath, "network_spaces", "ur.network", "main")
	toStoragePath := filepath.Join(storagePath, "network_spaces", "bringyour.com", "main")

	spacesListener := &testingNetworkSpacesChangeCounter{}
	activeListener := &testingActiveNetworkSpaceRecorder{}
	spacesSub := networkSpaceManager.AddNetworkSpacesChangeListener(spacesListener)
	defer spacesSub.Close()
	activeSub := networkSpaceManager.AddActiveNetworkSpaceChangeListener(activeListener)
	defer activeSub.Close()

	connect.AssertEqual(t, networkSpaceManager.MigrateNetworkSpace(fromKey, toKey), true)

	// the storage directory moved with its contents
	if _, err := os.Stat(fromStoragePath); !os.IsNotExist(err) {
		t.Fatalf("expected the fromKey storage directory to be moved, not left behind")
	}
	markerContents, err := os.ReadFile(filepath.Join(toStoragePath, "device_state_marker"))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, string(markerContents), "device state")

	// the manager now holds the space under toKey with the same values, the
	// completed migration host cleared
	connect.AssertEqual(t, networkSpaceManager.GetNetworkSpace(fromKey), nil)
	migrated := networkSpaceManager.GetNetworkSpace(toKey)
	if migrated == nil {
		t.Fatalf("expected a space under toKey after migration")
	}
	connect.AssertEqual(t, migrated.storagePath, toStoragePath)
	connect.AssertEqual(t, migrated.GetHostName(), "bringyour.com")
	connect.AssertEqual(t, migrated.GetEnvName(), "main")
	connect.AssertEqual(t, migrated.GetBundled(), true)
	connect.AssertEqual(t, migrated.GetEnvSecret(), "bundled-secret")
	connect.AssertEqual(t, migrated.GetLinkHostName(), "ur.io")
	connect.AssertEqual(t, migrated.GetMigrationHostName(), "")
	connect.AssertEqual(t, migrated.GetStore(), "store")
	connect.AssertEqual(t, migrated.GetWallet(), "wallet")
	connect.AssertEqual(t, migrated.GetSsoGoogle(), true)
	// the service urls resolve under the new host (the env secret is the path)
	connect.AssertEqual(t, migrated.GetApiUrl(), "https://api.bringyour.com/bundled-secret")
	connect.AssertEqual(t, networkSpaceManager.GetNetworkSpaces().Len(), 2)

	// the active pointer followed, and the listeners were told
	connect.AssertEqual(t, networkSpaceManager.GetActiveNetworkSpace() == migrated, true)
	connect.AssertEqual(t, spacesListener.count.Load(), int32(1))
	connect.AssertEqual(t, activeListener.count.Load(), int32(1))
	connect.AssertEqual(t, activeListener.active.Load() == migrated, true)

	// the persisted state file carries the toKey entry and the active pointer
	state := testingReadNetworkSpaceManagerState(t, storagePath)
	stateValues := testingStateKeys(state)
	connect.AssertEqual(t, len(stateValues), 2)
	if _, ok := stateValues[*fromKey]; ok {
		t.Fatalf("expected the fromKey entry to be removed from the persisted state")
	}
	migratedValues, ok := stateValues[*toKey]
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, migratedValues.Bundled, true)
	connect.AssertEqual(t, migratedValues.EnvSecret, "bundled-secret")
	connect.AssertEqual(t, migratedValues.MigrationHostName, "")
	connect.AssertEqual(t, *state.Active, *toKey)

	// the custom space is untouched, in memory and on disk
	custom := networkSpaceManager.GetNetworkSpace(testingMigrateCustomKey())
	if custom == nil {
		t.Fatalf("expected the custom space to survive the migration")
	}
	connect.AssertEqual(t, custom.GetEnvSecret(), "custom-secret")
	connect.AssertEqual(t, stateValues[*testingMigrateCustomKey()].EnvSecret, "custom-secret")
	if _, err := os.Stat(filepath.Join(storagePath, "network_spaces", "custom.example.com_8443", "main")); err != nil {
		t.Fatalf("expected the custom space storage directory to remain: %v", err)
	}

	// a second call on the same launch is a no-op
	connect.AssertEqual(t, networkSpaceManager.MigrateNetworkSpace(fromKey, toKey), false)
	connect.AssertEqual(t, networkSpaceManager.GetActiveNetworkSpace() == migrated, true)
	connect.AssertEqual(t, spacesListener.count.Load(), int32(1))
}

func TestNetworkSpaceManagerMigrateNetworkSpaceSurvivesRelaunch(t *testing.T) {
	networkSpaceManager, storagePath := testingMigrateManager(t)
	fromKey := testingMigrateFromKey()
	toKey := testingMigrateToKey()

	connect.AssertEqual(t, networkSpaceManager.MigrateNetworkSpace(fromKey, toKey), true)
	networkSpaceManager.Close()

	// the next launch loads the migrated space as active, and the migrate
	// call the app makes on every launch is a no-op
	networkSpaceManager2 := NewNetworkSpaceManager(storagePath)
	defer networkSpaceManager2.Close()
	connect.AssertEqual(t, networkSpaceManager2.GetNetworkSpaces().Len(), 2)
	connect.AssertEqual(t, networkSpaceManager2.GetNetworkSpace(fromKey), nil)
	migrated := networkSpaceManager2.GetNetworkSpace(toKey)
	if migrated == nil {
		t.Fatalf("expected the migrated space to be loaded under toKey")
	}
	connect.AssertEqual(t, networkSpaceManager2.GetActiveNetworkSpace() == migrated, true)
	connect.AssertEqual(t, migrated.GetEnvSecret(), "bundled-secret")
	connect.AssertEqual(t, migrated.GetMigrationHostName(), "")
	connect.AssertEqual(t, networkSpaceManager2.MigrateNetworkSpace(fromKey, toKey), false)

	markerContents, err := os.ReadFile(filepath.Join(migrated.storagePath, "device_state_marker"))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, string(markerContents), "device state")
}

func TestNetworkSpaceManagerMigrateNetworkSpaceToKeyExistsIsNoop(t *testing.T) {
	networkSpaceManager, storagePath := testingMigrateManager(t)
	defer networkSpaceManager.Close()
	fromKey := testingMigrateFromKey()
	toKey := testingMigrateToKey()

	existing := networkSpaceManager.updateNetworkSpace(
		toKey,
		func(values *NetworkSpaceValues) {
			values.EnvSecret = "existing-secret"
		},
	)
	existingMarkerPath := filepath.Join(existing.storagePath, "existing_marker")
	connect.AssertEqual(t, os.WriteFile(existingMarkerPath, []byte("existing"), LocalStorageFilePermissions), nil)
	spacesListener := &testingNetworkSpacesChangeCounter{}
	spacesSub := networkSpaceManager.AddNetworkSpacesChangeListener(spacesListener)
	defer spacesSub.Close()

	connect.AssertEqual(t, networkSpaceManager.MigrateNetworkSpace(fromKey, toKey), false)

	// nothing moved: both spaces and both directories are as they were
	from := networkSpaceManager.GetNetworkSpace(fromKey)
	connect.AssertEqual(t, from != nil, true)
	connect.AssertEqual(t, from.GetEnvSecret(), "bundled-secret")
	connect.AssertEqual(t, from.GetMigrationHostName(), "bringyour.com")
	connect.AssertEqual(t, networkSpaceManager.GetNetworkSpace(toKey) == existing, true)
	connect.AssertEqual(t, networkSpaceManager.GetActiveNetworkSpace() == from, true)
	connect.AssertEqual(t, networkSpaceManager.GetNetworkSpaces().Len(), 3)
	connect.AssertEqual(t, spacesListener.count.Load(), int32(0))

	fromMarker, err := os.ReadFile(filepath.Join(storagePath, "network_spaces", "ur.network", "main", "device_state_marker"))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, string(fromMarker), "device state")
	existingMarker, err := os.ReadFile(existingMarkerPath)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, string(existingMarker), "existing")

	state := testingReadNetworkSpaceManagerState(t, storagePath)
	stateValues := testingStateKeys(state)
	connect.AssertEqual(t, stateValues[*fromKey].EnvSecret, "bundled-secret")
	connect.AssertEqual(t, stateValues[*toKey].EnvSecret, "existing-secret")
	connect.AssertEqual(t, *state.Active, *fromKey)
}

func TestNetworkSpaceManagerMigrateNetworkSpaceFromKeyMissingIsNoop(t *testing.T) {
	storagePath := t.TempDir()
	networkSpaceManager := NewNetworkSpaceManager(storagePath)
	defer networkSpaceManager.Close()
	toKey := testingMigrateToKey()

	custom := networkSpaceManager.updateNetworkSpace(
		testingMigrateCustomKey(),
		func(values *NetworkSpaceValues) {
			values.EnvSecret = "custom-secret"
		},
	)
	networkSpaceManager.SetActiveNetworkSpace(custom)
	spacesListener := &testingNetworkSpacesChangeCounter{}
	spacesSub := networkSpaceManager.AddNetworkSpacesChangeListener(spacesListener)
	defer spacesSub.Close()

	// a fresh install, or one already migrated: there is nothing under fromKey
	connect.AssertEqual(t, networkSpaceManager.MigrateNetworkSpace(testingMigrateFromKey(), toKey), false)
	connect.AssertEqual(t, networkSpaceManager.GetNetworkSpace(toKey), nil)
	connect.AssertEqual(t, networkSpaceManager.GetNetworkSpace(testingMigrateCustomKey()) == custom, true)
	connect.AssertEqual(t, networkSpaceManager.GetActiveNetworkSpace() == custom, true)
	connect.AssertEqual(t, networkSpaceManager.GetNetworkSpaces().Len(), 1)
	connect.AssertEqual(t, spacesListener.count.Load(), int32(0))
	if _, err := os.Stat(filepath.Join(storagePath, "network_spaces", "bringyour.com")); !os.IsNotExist(err) {
		t.Fatalf("expected no toKey storage to be created by a no-op migration")
	}

	// degenerate keys are no-ops too
	connect.AssertEqual(t, networkSpaceManager.MigrateNetworkSpace(nil, toKey), false)
	connect.AssertEqual(t, networkSpaceManager.MigrateNetworkSpace(testingMigrateCustomKey(), nil), false)
	connect.AssertEqual(t, networkSpaceManager.MigrateNetworkSpace(testingMigrateCustomKey(), testingMigrateCustomKey()), false)
	connect.AssertEqual(t, networkSpaceManager.GetNetworkSpace(testingMigrateCustomKey()) == custom, true)
}

func TestNetworkSpaceManagerMigrateNetworkSpaceKeepsOtherMigrationHost(t *testing.T) {
	storagePath := t.TempDir()
	networkSpaceManager := NewNetworkSpaceManager(storagePath)
	defer networkSpaceManager.Close()
	fromKey := testingMigrateFromKey()
	toKey := testingMigrateToKey()

	// a migration host that is NOT the new key's host is still in progress
	// and is carried over unchanged
	networkSpaceManager.updateNetworkSpace(
		fromKey,
		func(values *NetworkSpaceValues) {
			values.MigrationHostName = "other.example"
		},
	)

	connect.AssertEqual(t, networkSpaceManager.MigrateNetworkSpace(fromKey, toKey), true)
	migrated := networkSpaceManager.GetNetworkSpace(toKey)
	if migrated == nil {
		t.Fatalf("expected a space under toKey after migration")
	}
	connect.AssertEqual(t, migrated.GetMigrationHostName(), "other.example")
	// the space was not active, so the manager still has no active space
	connect.AssertEqual(t, networkSpaceManager.GetActiveNetworkSpace(), nil)
	state := testingReadNetworkSpaceManagerState(t, storagePath)
	connect.AssertEqual(t, state.Active, nil)
	connect.AssertEqual(t, testingStateKeys(state)[*toKey].MigrationHostName, "other.example")
}

func TestNetworkSpaceManagerMigrateNetworkSpaceDoesNotOverwriteDestinationState(t *testing.T) {
	networkSpaceManager, storagePath := testingMigrateManager(t)
	defer networkSpaceManager.Close()
	fromKey := testingMigrateFromKey()
	toKey := testingMigrateToKey()

	// state on disk under the toKey path with no space record: left alone
	toStoragePath := filepath.Join(storagePath, "network_spaces", "bringyour.com", "main")
	connect.AssertEqual(t, os.MkdirAll(toStoragePath, LocalStorageDirectoryPermissions), nil)
	strayMarkerPath := filepath.Join(toStoragePath, "stray_marker")
	connect.AssertEqual(t, os.WriteFile(strayMarkerPath, []byte("stray"), LocalStorageFilePermissions), nil)

	connect.AssertEqual(t, networkSpaceManager.MigrateNetworkSpace(fromKey, toKey), false)

	from := networkSpaceManager.GetNetworkSpace(fromKey)
	connect.AssertEqual(t, from != nil, true)
	connect.AssertEqual(t, networkSpaceManager.GetActiveNetworkSpace() == from, true)
	connect.AssertEqual(t, networkSpaceManager.GetNetworkSpace(toKey), nil)
	fromMarker, err := os.ReadFile(filepath.Join(from.storagePath, "device_state_marker"))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, string(fromMarker), "device state")
	strayMarker, err := os.ReadFile(strayMarkerPath)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, string(strayMarker), "stray")
	state := testingReadNetworkSpaceManagerState(t, storagePath)
	connect.AssertEqual(t, *state.Active, *fromKey)
	if _, ok := testingStateKeys(state)[*toKey]; ok {
		t.Fatalf("expected no toKey entry in the persisted state")
	}
}

func TestNetworkSpaceManagerMigrateNetworkSpaceReplacesEmptyDestinationDirectory(t *testing.T) {
	networkSpaceManager, storagePath := testingMigrateManager(t)
	defer networkSpaceManager.Close()
	fromKey := testingMigrateFromKey()
	toKey := testingMigrateToKey()

	// an empty toKey directory is just a materialized path, not state
	toStoragePath := filepath.Join(storagePath, "network_spaces", "bringyour.com", "main")
	connect.AssertEqual(t, os.MkdirAll(toStoragePath, LocalStorageDirectoryPermissions), nil)

	connect.AssertEqual(t, networkSpaceManager.MigrateNetworkSpace(fromKey, toKey), true)
	migrated := networkSpaceManager.GetNetworkSpace(toKey)
	if migrated == nil {
		t.Fatalf("expected a space under toKey after migration")
	}
	markerContents, err := os.ReadFile(filepath.Join(toStoragePath, "device_state_marker"))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, string(markerContents), "device state")
}

func TestNetworkSpaceManagerMigrateNetworkSpaceNoStorage(t *testing.T) {
	networkSpaceManager := NewNetworkSpaceManagerNoStorage()
	defer networkSpaceManager.Close()
	fromKey := testingMigrateFromKey()
	toKey := testingMigrateToKey()

	from := networkSpaceManager.updateNetworkSpace(
		fromKey,
		func(values *NetworkSpaceValues) {
			values.Bundled = true
			values.MigrationHostName = "bringyour.com"
		},
	)
	networkSpaceManager.SetActiveNetworkSpace(from)

	connect.AssertEqual(t, networkSpaceManager.MigrateNetworkSpace(fromKey, toKey), true)
	migrated := networkSpaceManager.GetNetworkSpace(toKey)
	if migrated == nil {
		t.Fatalf("expected a space under toKey after migration")
	}
	connect.AssertEqual(t, migrated.GetBundled(), true)
	connect.AssertEqual(t, migrated.GetMigrationHostName(), "")
	connect.AssertEqual(t, networkSpaceManager.GetActiveNetworkSpace() == migrated, true)
	connect.AssertEqual(t, networkSpaceManager.GetNetworkSpace(fromKey), nil)
	connect.AssertEqual(t, networkSpaceManager.MigrateNetworkSpace(fromKey, toKey), false)
}
