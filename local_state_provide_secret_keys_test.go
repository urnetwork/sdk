// Checked secret records are observations, not permission to generate a new
// provider identity. These controls use the real loader/constructor/save chain.
package sdk

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalStateProvideSecretKeysCheckedLegacyValuesAndReadableSymlink(t *testing.T) {
	state := newLocalState(context.Background(), t.TempDir())
	t.Cleanup(state.Close)
	path := filepath.Join(state.localStorageDir, ".provide_secret_keys")
	if secrets, err := state.LoadProvideSecretKeys(); err != nil || secrets != nil {
		t.Fatal("genuine missing provider secrets were not healthy absence")
	}
	for _, data := range []string{"null", "[]", "[{}]", "[{\"provide_mode\":2,\"provide_secret_key\":\"synthetic-secret\"}]"} {
		if err := os.WriteFile(path, []byte(data), LocalStorageFilePermissions); err != nil {
			t.Fatal("could not seed a legacy provider-secret value")
		}
		checked, err := state.LoadProvideSecretKeys()
		legacy := state.GetProvideSecretKeys()
		if err != nil || checked == nil || legacy == nil || checked.Len() != legacy.Len() {
			t.Fatal("checked provider-secret read narrowed a successful legacy value")
		}
		for index := 0; index < checked.Len(); index += 1 {
			if *checked.Get(index) != *legacy.Get(index) {
				t.Fatal("checked provider-secret read changed a legacy optional field")
			}
		}
	}
	target := path + ".retained-test"
	if os.Rename(path, target) != nil || os.Symlink(target, path) != nil {
		t.Fatal("could not create a readable provider-secret symlink")
	}
	if secrets, err := state.LoadProvideSecretKeys(); err != nil || secrets == nil || secrets.Len() != 1 {
		t.Fatal("readable legacy provider-secret symlink was rejected")
	}
}

func TestLocalStateProvideSecretKeysCheckedMalformedTypeAndUnavailableStore(t *testing.T) {
	state := newLocalState(context.Background(), t.TempDir())
	t.Cleanup(state.Close)
	path := filepath.Join(state.localStorageDir, ".provide_secret_keys")
	for _, data := range []string{"", "{broken-private-marker", "{}", "[null]", "[{\"provide_secret_key\":17}]"} {
		if os.WriteFile(path, []byte(data), LocalStorageFilePermissions) != nil {
			t.Fatal("could not seed a failed secret observation")
		}
		secrets, err := state.LoadProvideSecretKeys()
		if err == nil || secrets != nil || err.Error() != "decode provide secret keys" {
			t.Fatal("malformed provider secrets became absence or exposed their value")
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(after, []byte(data)) {
			t.Fatal("checked secret read repaired or removed a failed observation")
		}
	}
	if os.Remove(path) != nil || os.Mkdir(path, LocalStorageDirectoryPermissions) != nil {
		t.Fatal("could not install the non-regular secret record")
	}
	if secrets, err := state.LoadProvideSecretKeys(); err == nil || secrets != nil {
		t.Fatal("non-regular provider secret record became absence")
	}
	if os.Remove(path) != nil || os.Symlink(path+".missing-test", path) != nil {
		t.Fatal("could not install the dangling secret symlink")
	}
	if secrets, err := state.LoadProvideSecretKeys(); err == nil || secrets != nil {
		t.Fatal("dangling provider secret symlink became absence")
	}
	if os.Remove(path) != nil || os.Rename(state.localStorageDir, state.localStorageDir+".retained-test") != nil {
		t.Fatal("could not hold the unavailable store")
	}
	if secrets, err := state.LoadProvideSecretKeys(); err == nil || secrets != nil {
		t.Fatal("unavailable provider secret directory became absence")
	}
}

func TestCheckedProvideSecretReadFailureStopsActualConstructorAndPersistence(t *testing.T) {
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	material := NewDeviceLocalKeyMaterial(bytes.Repeat([]byte{67}, 32), nil, nil)
	if fixture.localState.SetDeviceLocalKeyMaterial(material) != nil {
		t.Fatal("could not seed the retained provider identity")
	}
	path := filepath.Join(fixture.localState.localStorageDir, ".provide_secret_keys")
	original := []byte("{unreadable-retained-provider-secrets")
	if os.WriteFile(path, original, LocalStorageFilePermissions) != nil {
		t.Fatal("could not seed the failed required secret observation")
	}
	secrets, readErr := fixture.localState.LoadProvideSecretKeys()
	if readErr == nil {
		// This is the actual constructor/persistence continuation that the old
		// null-on-error getter permits. A counterfactual runs it, not a mock.
		device := testingDeviceKeyLoadConstructor(t, fixture, material)
		if secrets == nil {
			device.InitProvideSecretKeys()
		} else {
			device.LoadProvideSecretKeys(secrets)
		}
		if device.SaveProvideSecretKeys() != nil || device.SaveKeyMaterial() != nil {
			t.Fatal("counterfactual constructor continuation failed before its semantic assertion")
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, original) {
		t.Fatal("failed secret observation regenerated and overwrote retained provider secrets")
	}
	if readErr == nil || secrets != nil {
		t.Fatal("failed secret observation did not stop startup")
	}
	stored, err := fixture.localState.LoadDeviceLocalKeyMaterial()
	if err != nil || stored == nil || !testingDeviceKeyLoadEqual(stored, material) ||
		fixture.localState.GetByJwt() != fixture.adminJwt {
		t.Fatal("failed secret observation changed retained identity or separate admin auth")
	}
}

func TestCheckedProvideSecretHealthyAbsenceConstructsAndPersistsActualProvider(t *testing.T) {
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	secrets, err := fixture.localState.LoadProvideSecretKeys()
	if err != nil || secrets != nil {
		t.Fatal("fresh provider secrets were not genuinely absent")
	}
	device := testingDeviceKeyLoadConstructor(t, fixture, nil)
	device.InitProvideSecretKeys()
	if device.SaveProvideSecretKeys() != nil || device.SaveKeyMaterial() != nil {
		t.Fatal("healthy fresh provider could not explicitly save")
	}
	testingRequireProviderKeyState(t, fixture.localState, device.GetKeyMaterial(), device.GetProvideSecretKeys())
}
