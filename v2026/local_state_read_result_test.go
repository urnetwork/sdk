// Exercise actual optional storage through native-safe success envelopes.
// Native Swift tests separately reproduce the nil-on-error importer boundary.
package sdk

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Both checked local and paired observations must retain the same value.
func testingLocationResultReaders(state *LocalState, snapshot *LocalAuthStateSnapshot, name string) []func() (*LocalStateLocationReadResult, error) {
	if name == localConnectLocationFileName {
		return []func() (*LocalStateLocationReadResult, error){state.ReadConnectLocation, snapshot.ReadConnectLocation}
	}
	return []func() (*LocalStateLocationReadResult, error){state.ReadDefaultLocation, snapshot.ReadDefaultLocation}
}

// The success envelope exists even for first install or explicit disconnect.
func TestLocalStateLocationReadResultAbsent(t *testing.T) {
	for _, name := range []string{localConnectLocationFileName, localDefaultLocationFileName} {
		fixture := testingPairedAuthSpace(t)
		fixture.seedDistinctLogin(t)
		snapshot := testingPairedAuthSnapshot(t, fixture)
		set, load, _ := testingLocationAccess(fixture.localState, name)
		for _, disconnected := range []bool{false, true} {
			if disconnected {
				if err := set(testingStoredLocation("saved")); err != nil {
					t.Fatal("could not save disconnect control")
				}
				if err := set(nil); err != nil {
					t.Fatal("could not persist explicit disconnect")
				}
			}
			if location, err := load(); err != nil || location != nil {
				t.Fatal("compatibility loader did not report genuine absence")
			}
			for _, read := range testingLocationResultReaders(fixture.localState, snapshot, name) {
				result, err := read()
				if err != nil || result == nil || result.GetLocation() != nil {
					t.Fatal("native result lost successful absent location")
				}
			}
			if _, err := os.Lstat(filepath.Join(fixture.localState.localStorageDir, name)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("absent read created or repaired a location")
			}
		}
	}
}

// A returned object's mutation and a later write cannot change an observation.
func TestLocalStateLocationReadResultRetainsCapturedCopy(t *testing.T) {
	for _, name := range []string{localConnectLocationFileName, localDefaultLocationFileName} {
		fixture := testingPairedAuthSpace(t)
		fixture.seedDistinctLogin(t)
		snapshot := testingPairedAuthSnapshot(t, fixture)
		set, _, _ := testingLocationAccess(fixture.localState, name)
		for _, read := range testingLocationResultReaders(fixture.localState, snapshot, name) {
			original := testingStoredLocation("captured")
			if err := set(original); err != nil {
				t.Fatal("could not save healthy location")
			}
			result, err := read()
			if err != nil || result == nil || !connectLocationValuesEqual(result.GetLocation(), original) {
				t.Fatal("native read lost present location")
			}
			returned := result.GetLocation()
			returned.Name = "caller-change"
			returned.ConnectLocationId.BestAvailable = false
			if err := set(testingStoredLocation("later")); err != nil {
				t.Fatal("could not save later location")
			}
			if !connectLocationValuesEqual(result.GetLocation(), original) {
				t.Fatal("native result changed after caller mutation or later storage write")
			}
		}
	}
}

// Malformed records and non-file entries must not become successful absence.
func TestLocalStateLocationReadResultPreservesStorageErrors(t *testing.T) {
	for _, name := range []string{localConnectLocationFileName, localDefaultLocationFileName} {
		for _, directory := range []bool{false, true} {
			fixture := testingPairedAuthSpace(t)
			fixture.seedDistinctLogin(t)
			snapshot := testingPairedAuthSnapshot(t, fixture)
			path := filepath.Join(fixture.localState.localStorageDir, name)
			data := []byte(`{"connect_location_id":{"best_available":"private-marker"}}`)
			if directory {
				if err := os.Mkdir(path, LocalStorageDirectoryPermissions); err != nil {
					t.Fatal("could not create non-file location")
				}
			} else if err := os.WriteFile(path, data, LocalStorageFilePermissions); err != nil {
				t.Fatal("could not write malformed location")
			}
			_, load, _ := testingLocationAccess(fixture.localState, name)
			_, expectedErr := load()
			if expectedErr == nil {
				t.Fatal("fixture did not fail through the checked loader")
			}
			for _, read := range testingLocationResultReaders(fixture.localState, snapshot, name) {
				result, err := read()
				if err == nil || result != nil || err.Error() != expectedErr.Error() || strings.Contains(err.Error(), "private-marker") {
					t.Fatal("native location result lost or exposed a storage failure")
				}
			}
			if directory {
				if info, err := os.Stat(path); err != nil || !info.IsDir() {
					t.Fatal("failed read changed a non-file location")
				}
			} else if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, data) {
				t.Fatal("failed read changed malformed location")
			}
		}
	}
}

// The envelope must not bypass the paired loader's ownership discriminator.
func TestLocalStateLocationReadResultRejectsSupersededAndUnpairedSnapshots(t *testing.T) {
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	paired := testingPairedAuthSnapshot(t, fixture)
	localOnly, err := fixture.localState.GetAuthStateSnapshot()
	if err != nil {
		t.Fatal("could not read local-only auth observation")
	}
	fixture.api.SetByJwt("synthetic-new-owner")
	for _, snapshot := range []*LocalAuthStateSnapshot{paired, localOnly} {
		for _, read := range []func() (*LocalStateLocationReadResult, error){snapshot.ReadConnectLocation, snapshot.ReadDefaultLocation} {
			result, err := read()
			if err == nil || result != nil {
				t.Fatal("unowned native observation was accepted as an absent location")
			}
			if snapshot == paired && err.Error() != localAuthSnapshotSupersededMessage {
				t.Fatal("native result lost the exact supersession discriminator")
			}
		}
	}
}

// First install has no key record and must not throw through the native bridge.
func TestLocalStateKeyMaterialReadResultAbsent(t *testing.T) {
	state, path := testingCheckedKeyLoadState(t)
	result, err := state.ReadDeviceLocalKeyMaterial()
	if err != nil || result == nil || result.GetKeyMaterial() != nil {
		t.Fatal("native key result lost successful absence")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("absent key read created a file")
	}
}

// Legacy empty records remain successful reads without rewriting their bytes.
func TestLocalStateKeyMaterialReadResultLegacyEmpty(t *testing.T) {
	for _, data := range []string{"null", "{}", `{"client_key_seed":""}`} {
		state, path := testingCheckedKeyLoadState(t)
		if err := os.WriteFile(path, []byte(data), LocalStorageFilePermissions); err != nil {
			t.Fatal("could not save legacy-empty material")
		}
		result, err := state.ReadDeviceLocalKeyMaterial()
		if err != nil || result == nil || result.GetKeyMaterial() != nil {
			t.Fatal("native result rejected readable legacy-empty material")
		}
		if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, []byte(data)) {
			t.Fatal("legacy-empty read changed the record")
		}
	}
}

// Optional binary fields survive and no mutable returned bytes own the result.
func TestLocalStateKeyMaterialReadResultRetainsCapturedCopy(t *testing.T) {
	state, path := testingCheckedKeyLoadState(t)
	original := NewDeviceLocalKeyMaterial([]byte{0, 128, 255}, nil, []byte{1, 0, 254})
	if err := state.SetDeviceLocalKeyMaterial(original); err != nil {
		t.Fatal("could not save key material")
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("could not inspect saved material")
	}
	result, err := state.ReadDeviceLocalKeyMaterial()
	if err != nil || result == nil || !testingDeviceKeyLoadEqual(result.GetKeyMaterial(), original) {
		t.Fatal("native result changed optional binary material")
	}
	returned := result.GetKeyMaterial()
	returned.clientKeySeed[0] ^= 255
	returned.provideTlsPrivateKeyPem[0] ^= 255
	if !testingDeviceKeyLoadEqual(result.GetKeyMaterial(), original) {
		t.Fatal("returned material mutated the captured result")
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
		t.Fatal("native key observation rewrote storage")
	}
	if err := state.SetDeviceLocalKeyMaterial(NewDeviceLocalKeyMaterial([]byte{7}, nil, nil)); err != nil {
		t.Fatal("could not save later identity")
	}
	if !testingDeviceKeyLoadEqual(result.GetKeyMaterial(), original) {
		t.Fatal("native key result reread a later identity")
	}
}

// A failed read has no usable envelope and retains the exact sanitized stage.
func TestLocalStateKeyMaterialReadResultPreservesStorageErrors(t *testing.T) {
	for _, directory := range []bool{false, true} {
		state, path := testingCheckedKeyLoadState(t)
		data := []byte(`{"client_key_seed":"private-marker!"}`)
		if directory {
			if err := os.Mkdir(path, LocalStorageDirectoryPermissions); err != nil {
				t.Fatal("could not create non-file key record")
			}
		} else if err := os.WriteFile(path, data, LocalStorageFilePermissions); err != nil {
			t.Fatal("could not save malformed key record")
		}
		_, expectedErr := state.LoadDeviceLocalKeyMaterial()
		if expectedErr == nil {
			t.Fatal("fixture did not fail the checked key read")
		}
		result, err := state.ReadDeviceLocalKeyMaterial()
		if err == nil || result != nil || err.Error() != expectedErr.Error() || strings.Contains(err.Error(), "private-marker") {
			t.Fatal("native key result lost or exposed a checked failure")
		}
		if directory {
			if info, err := os.Stat(path); err != nil || !info.IsDir() {
				t.Fatal("failed native read changed non-file key storage")
			}
		} else if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, data) {
			t.Fatal("failed native read changed malformed key material")
		}
	}
}
