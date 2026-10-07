// Checked key reads preserve optional storage semantics while making failure
// observable before a constructor or key-preserving cleanup can discard identity.
package sdk

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// All filesystem fixtures live under the test directory, never app storage.
func testingCheckedKeyLoadState(t *testing.T) (*LocalState, string) {
	t.Helper()
	state := newLocalState(context.Background(), t.TempDir())
	t.Cleanup(state.Close)
	return state, filepath.Join(state.localStorageDir, ".device_local_key_material")
}

// A missing leaf under the existing LocalState directory is a fresh identity.
func TestCheckedDeviceKeyLoadAbsent(t *testing.T) {
	state, path := testingCheckedKeyLoadState(t)
	material, err := state.LoadDeviceLocalKeyMaterial()
	if err != nil || material != nil {
		t.Fatal("checked load rejected a genuinely absent identity")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("checked absent load created a key file")
	}
}

// Successful empty JSON stays compatible; raw empty bytes are tested as errors.
func TestCheckedDeviceKeyLoadLegacyEmpty(t *testing.T) {
	cases := []struct {
		name string
		data string
	}{
		{name: "null", data: "null"},
		{name: "object", data: "{}"},
		{name: "empty-byte-fields", data: `{"client_key_seed":"","provide_tls_certificate_pem":"","provide_tls_private_key_pem":""}`},
		{name: "null-byte-fields", data: `{"client_key_seed":null,"provide_tls_certificate_pem":null,"provide_tls_private_key_pem":null}`},
		{name: "unknown-only", data: `{"future_metadata":1}`},
	}
	for _, test := range cases {
		state, path := testingCheckedKeyLoadState(t)
		data := []byte(test.data)
		if err := os.WriteFile(path, data, LocalStorageFilePermissions); err != nil {
			t.Fatal("could not write a legacy-empty key fixture")
		}
		material, err := state.LoadDeviceLocalKeyMaterial()
		if err != nil || material != nil || state.GetDeviceLocalKeyMaterial() != nil {
			t.Errorf("checked load changed successful legacy-empty semantics: %s", test.name)
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(after, data) {
			t.Errorf("checked load rewrote legacy-empty storage: %s", test.name)
		}
	}
}

// Every independently optional field survives; this is storage decoding, not
// validation of seed length, PEM shape, or TLS pairing.
func TestCheckedDeviceKeyLoadOptionalMaterial(t *testing.T) {
	cases := []struct {
		name     string
		material *DeviceLocalKeyMaterial
	}{
		{name: "seed-only", material: NewDeviceLocalKeyMaterial([]byte{1}, nil, nil)},
		{name: "certificate-only", material: NewDeviceLocalKeyMaterial(nil, []byte{2}, nil)},
		{name: "private-key-only", material: NewDeviceLocalKeyMaterial(nil, nil, []byte{3})},
		{name: "tls-pair", material: NewDeviceLocalKeyMaterial(nil, []byte{2}, []byte{3})},
		{name: "all-fields", material: NewDeviceLocalKeyMaterial([]byte{1}, []byte{2}, []byte{3})},
	}
	for _, test := range cases {
		state, path := testingCheckedKeyLoadState(t)
		if err := state.SetDeviceLocalKeyMaterial(test.material); err != nil {
			t.Fatal("could not write optional key fields")
		}
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal("could not read optional key fixture")
		}
		material, err := state.LoadDeviceLocalKeyMaterial()
		if err != nil || material == nil || !testingDeviceKeyLoadEqual(material, test.material) ||
			!testingDeviceKeyLoadEqual(material, state.GetDeviceLocalKeyMaterial()) {
			t.Errorf("checked load changed an optional field: %s", test.name)
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Errorf("checked load rewrote optional key fields: %s", test.name)
		}
	}
}

// Unknown fields retain json.Unmarshal compatibility without a new format.
func TestCheckedDeviceKeyLoadUnknownFields(t *testing.T) {
	state, path := testingCheckedKeyLoadState(t)
	data := []byte(`{"client_key_seed":"AQID","future_metadata":{"version":2}}`)
	if err := os.WriteFile(path, data, LocalStorageFilePermissions); err != nil {
		t.Fatal("could not write unknown-field key fixture")
	}
	material, err := state.LoadDeviceLocalKeyMaterial()
	if err != nil || material == nil || !bytes.Equal(material.GetClientKeySeed(), []byte{1, 2, 3}) {
		t.Fatal("checked load rejected compatible unknown fields")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, data) {
		t.Fatal("checked load changed unknown-field storage")
	}
}

// JSON byte arrays are another successful encoding accepted by the legacy
// decoder, even though the existing writer emits base64 strings.
func TestCheckedDeviceKeyLoadByteArrayCompatibility(t *testing.T) {
	state, path := testingCheckedKeyLoadState(t)
	data := []byte(`{"client_key_seed":[1,2,3],"provide_tls_certificate_pem":[4],"provide_tls_private_key_pem":[5,6]}`)
	if err := os.WriteFile(path, data, LocalStorageFilePermissions); err != nil {
		t.Fatal("could not write byte-array key fixture")
	}
	material, err := state.LoadDeviceLocalKeyMaterial()
	expected := NewDeviceLocalKeyMaterial([]byte{1, 2, 3}, []byte{4}, []byte{5, 6})
	if err != nil || material == nil || !testingDeviceKeyLoadEqual(material, expected) ||
		!testingDeviceKeyLoadEqual(material, state.GetDeviceLocalKeyMaterial()) {
		t.Fatal("checked load changed successful byte-array decoding")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, data) {
		t.Fatal("checked load rewrote byte-array storage")
	}
}

// Decode errors must not masquerade as a successful empty identity. The old
// getter remains intentionally compatible and still returns nil for these.
func TestCheckedDeviceKeyLoadMalformedRecords(t *testing.T) {
	cases := []struct {
		name string
		data string
	}{
		{name: "zero-byte", data: ""},
		{name: "whitespace", data: " \n\t"},
		{name: "truncated", data: `{"client_key_seed":`},
		{name: "top-level-array", data: "[]"},
		{name: "wrong-field-type", data: `{"client_key_seed":{}}`},
		{name: "invalid-base64", data: `{"client_key_seed":"key-material-must-not-be-logged!"}`},
		{name: "partial-decode", data: `{"client_key_seed":"AQID","provide_tls_private_key_pem":{}}`},
	}
	for _, test := range cases {
		state, path := testingCheckedKeyLoadState(t)
		data := []byte(test.data)
		if err := os.WriteFile(path, data, LocalStorageFilePermissions); err != nil {
			t.Fatal("could not write malformed key fixture")
		}
		material, err := state.LoadDeviceLocalKeyMaterial()
		if err == nil || material != nil {
			t.Errorf("checked load accepted a malformed key record: %s", test.name)
		}
		if state.GetDeviceLocalKeyMaterial() != nil {
			t.Errorf("compatibility getter changed its malformed-record result: %s", test.name)
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(after, data) {
			t.Errorf("checked load rewrote a malformed key record: %s", test.name)
		}
	}
}

// A byte overflow makes encoding/json include the rejected numeric value in
// its error text. The checked public error must never carry that value.
func TestCheckedDeviceKeyLoadDecodeErrorDoesNotExposeMaterial(t *testing.T) {
	state, path := testingCheckedKeyLoadState(t)
	marker := "98765432109876543210"
	data := []byte(`{"client_key_seed":[` + marker + `]}`)
	if err := os.WriteFile(path, data, LocalStorageFilePermissions); err != nil {
		t.Fatal("could not write private-error key fixture")
	}
	material, err := state.LoadDeviceLocalKeyMaterial()
	if err == nil || material != nil {
		t.Fatal("private-error key fixture did not produce a decode error")
	}
	if strings.Contains(err.Error(), marker) || !strings.Contains(err.Error(), "decode device key material") {
		t.Fatal("checked key-load error exposed record contents or lost its stage")
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || !bytes.Equal(after, data) {
		t.Fatal("private-error key load changed stored bytes")
	}
}

// Existing readable file modes are not tightened or repaired by a load.
func TestCheckedDeviceKeyLoadDoesNotChangePermissions(t *testing.T) {
	state, path := testingCheckedKeyLoadState(t)
	if err := os.WriteFile(path, []byte(`{"client_key_seed":"AQID"}`), 0400); err != nil {
		t.Fatal("could not write read-only key fixture")
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal("could not stat read-only key fixture")
	}
	material, err := state.LoadDeviceLocalKeyMaterial()
	if err != nil || material == nil {
		t.Fatal("checked load rejected a readable key fixture")
	}
	after, err := os.Stat(path)
	if err != nil || before.Mode() != after.Mode() {
		t.Fatal("checked load changed existing key-file permissions")
	}
}

// A real denied read is tested only when this host enforces that permission.
// Directory/loop controls provide non-skipped deterministic filesystem failures.
func TestCheckedDeviceKeyLoadReadPermissionFailure(t *testing.T) {
	state, path := testingCheckedKeyLoadState(t)
	if err := os.WriteFile(path, []byte(`{"client_key_seed":"AQID"}`), LocalStorageFilePermissions); err != nil {
		t.Fatal("could not write denied-read key fixture")
	}
	if err := os.Chmod(path, 0000); err != nil {
		t.Fatal("could not deny reading the key fixture")
	}
	t.Cleanup(func() { _ = os.Chmod(path, LocalStorageFilePermissions) })
	if _, err := os.ReadFile(path); err == nil {
		t.Skip("this runner bypasses key-file read permissions")
	} else if !errors.Is(err, os.ErrPermission) {
		t.Fatal("denied-read fixture did not produce a permission error")
	}
	material, err := state.LoadDeviceLocalKeyMaterial()
	if material != nil || !errors.Is(err, os.ErrPermission) {
		t.Fatal("checked load lost the real key-file permission error")
	}
}

// A directory at the key path is an observation failure, never fresh storage.
func TestCheckedDeviceKeyLoadDirectoryEntry(t *testing.T) {
	state, path := testingCheckedKeyLoadState(t)
	if err := os.Mkdir(path, LocalStorageDirectoryPermissions); err != nil {
		t.Fatal("could not create non-file key fixture")
	}
	material, err := state.LoadDeviceLocalKeyMaterial()
	if err == nil || material != nil {
		t.Fatal("checked load accepted a directory as an absent key")
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		t.Fatal("checked load changed a non-file key entry")
	}
}

// Losing an already-created .by parent is distinct from a missing key leaf.
func TestCheckedDeviceKeyLoadMissingParent(t *testing.T) {
	state, path := testingCheckedKeyLoadState(t)
	if err := os.WriteFile(path, []byte(`{"client_key_seed":"AQID"}`), LocalStorageFilePermissions); err != nil {
		t.Fatal("could not write missing-parent key fixture")
	}
	moved := filepath.Join(filepath.Dir(state.localStorageDir), "held-key-directory")
	if err := os.Rename(state.localStorageDir, moved); err != nil {
		t.Fatal("could not hold the established key directory")
	}
	material, err := state.LoadDeviceLocalKeyMaterial()
	if err == nil || material != nil {
		t.Fatal("checked load called a missing established parent fresh storage")
	}
	if _, err := os.Lstat(state.localStorageDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("checked load recreated the missing parent")
	}
	if _, err := os.Stat(filepath.Join(moved, filepath.Base(path))); err != nil {
		t.Fatal("checked load changed the held key directory")
	}
}

// An inaccessible established parent cannot prove a missing leaf. This host
// must demonstrate real permission denial before the loader assertion runs.
func TestCheckedDeviceKeyLoadParentPermissionFailure(t *testing.T) {
	state, path := testingCheckedKeyLoadState(t)
	if err := os.Chmod(state.localStorageDir, 0000); err != nil {
		t.Fatal("could not deny traversal of the key directory")
	}
	t.Cleanup(func() { _ = os.Chmod(state.localStorageDir, LocalStorageDirectoryPermissions) })
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrPermission) {
		if errors.Is(err, os.ErrNotExist) {
			t.Skip("this runner bypasses key-directory traversal permissions")
		}
		t.Fatal("inaccessible-parent fixture did not produce a permission error")
	}
	material, err := state.LoadDeviceLocalKeyMaterial()
	if material != nil || !errors.Is(err, os.ErrPermission) {
		t.Fatal("checked load treated an inaccessible key directory as fresh storage")
	}
}

// Readable symlinks keep their existing meaning and are never replaced by load.
func TestCheckedDeviceKeyLoadReadableSymlink(t *testing.T) {
	state, path := testingCheckedKeyLoadState(t)
	target := filepath.Join(t.TempDir(), "identity.json")
	data := []byte(`{"client_key_seed":"AQID"}`)
	if err := os.WriteFile(target, data, LocalStorageFilePermissions); err != nil {
		t.Fatal("could not write symlink target fixture")
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal("could not create readable key symlink fixture")
	}
	material, err := state.LoadDeviceLocalKeyMaterial()
	if err != nil || material == nil || !testingDeviceKeyLoadEqual(material, state.GetDeviceLocalKeyMaterial()) {
		t.Fatal("checked load changed readable key symlink semantics")
	}
	link, err := os.Readlink(path)
	if err != nil || link != target {
		t.Fatal("checked load replaced the readable key symlink")
	}
	after, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(after, data) {
		t.Fatal("checked load changed the readable symlink target")
	}
}

// A readable symlinked storage directory also retains the existing read path.
func TestCheckedDeviceKeyLoadSymlinkedParent(t *testing.T) {
	state, path := testingCheckedKeyLoadState(t)
	if err := os.WriteFile(path, []byte(`{"client_key_seed":"AQID"}`), LocalStorageFilePermissions); err != nil {
		t.Fatal("could not write symlinked-parent fixture")
	}
	moved := filepath.Join(filepath.Dir(state.localStorageDir), "linked-key-directory")
	if err := os.Rename(state.localStorageDir, moved); err != nil {
		t.Fatal("could not move the symlinked-parent fixture")
	}
	if err := os.Symlink(moved, state.localStorageDir); err != nil {
		t.Fatal("could not link the established storage directory")
	}
	material, err := state.LoadDeviceLocalKeyMaterial()
	if err != nil || material == nil {
		t.Fatal("checked load rejected a readable symlinked parent")
	}
	link, err := os.Readlink(state.localStorageDir)
	if err != nil || link != moved {
		t.Fatal("checked load replaced its symlinked parent")
	}
}

// ENOENT from an existing dangling link must not admit fresh-key persistence.
func TestCheckedDeviceKeyLoadDanglingSymlink(t *testing.T) {
	state, path := testingCheckedKeyLoadState(t)
	target := filepath.Join(t.TempDir(), "missing-identity.json")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal("could not create dangling key symlink fixture")
	}
	material, err := state.LoadDeviceLocalKeyMaterial()
	if err == nil || material != nil {
		t.Fatal("checked load treated a dangling key link as fresh storage")
	}
	link, err := os.Readlink(path)
	if err != nil || link != target {
		t.Fatal("checked load replaced the dangling key link")
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("checked load created the missing key target")
	}
}

// The real filesystem loop error remains observable without permission tricks.
func TestCheckedDeviceKeyLoadSymlinkLoop(t *testing.T) {
	state, path := testingCheckedKeyLoadState(t)
	target := filepath.Base(path)
	if err := os.Symlink(target, path); err != nil {
		t.Fatal("could not create key symlink loop fixture")
	}
	material, err := state.LoadDeviceLocalKeyMaterial()
	if err == nil || material != nil {
		t.Fatal("checked load treated a key symlink loop as empty")
	}
	link, err := os.Readlink(path)
	if err != nil || link != target {
		t.Fatal("checked load changed the key symlink loop")
	}
}

// Explicit user logout is not conditional on being able to decode identity.
func TestCheckedDeviceKeyLoadExplicitLogoutStillClearsCorruption(t *testing.T) {
	state, path := testingCheckedKeyLoadState(t)
	if err := state.SetByJwt("logout-control-admin"); err != nil {
		t.Fatal("could not seed logout auth control")
	}
	if err := os.WriteFile(path, []byte("{"), LocalStorageFilePermissions); err != nil {
		t.Fatal("could not write corrupt logout key fixture")
	}
	if _, err := state.LoadDeviceLocalKeyMaterial(); err == nil {
		t.Fatal("logout control did not establish a key decode error")
	}
	if err := state.Logout(); err != nil {
		t.Fatal("explicit logout was blocked by key corruption")
	}
	material, err := state.LoadDeviceLocalKeyMaterial()
	if err != nil || material != nil || state.GetByJwt() != "" {
		t.Fatal("explicit logout retained corrupt key or auth state")
	}
}

// The real SDK boundary is exercised here. It models the caller's load,
// constructor and persistence sequence; it does not qualify native wiring.
func TestCheckedDeviceKeyLoadStopsConstructorAndPersistence(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.seedDistinctLogin(t)
	fixture.api.SetByJwt(fixture.adminJwt)
	path := filepath.Join(fixture.localState.localStorageDir, ".device_local_key_material")
	data := []byte(`{"client_key_seed":`)
	if err := os.WriteFile(path, data, LocalStorageFilePermissions); err != nil {
		t.Fatal("could not write constructor-admission key fixture")
	}
	before, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal("could not snapshot constructor-admission auth")
	}
	constructed := false
	persisted := false
	material, loadErr := fixture.localState.LoadDeviceLocalKeyMaterial()
	if loadErr == nil {
		constructed = true
		device := testingDeviceKeyLoadConstructor(t, fixture, material)
		if err := fixture.localState.SetDeviceLocalKeyMaterial(device.GetKeyMaterial()); err != nil {
			t.Fatal("constructor-admission control could not persist its result")
		}
		persisted = true
	}
	after, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal("could not read constructor-admission auth afterward")
	}
	afterBytes, readErr := os.ReadFile(path)
	if loadErr == nil || material != nil || constructed || persisted ||
		readErr != nil || !bytes.Equal(afterBytes, data) || after != before ||
		fixture.api.GetByJwt() != fixture.adminJwt {
		t.Fatal("checked key-load admission constructed or persisted after observation failure")
	}
}

// The key-preserving cleanup sequence must stop before the real LocalState
// wipe when the loader cannot observe the existing identity.
func TestCheckedDeviceKeyLoadStopsStaleCleanup(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.seedDistinctLogin(t)
	path := filepath.Join(fixture.localState.localStorageDir, ".device_local_key_material")
	data := []byte("{")
	if err := os.WriteFile(path, data, LocalStorageFilePermissions); err != nil {
		t.Fatal("could not write stale-cleanup key fixture")
	}
	before, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal("could not snapshot stale-cleanup auth")
	}
	cleared := false
	material, loadErr := fixture.localState.LoadDeviceLocalKeyMaterial()
	if loadErr == nil {
		if err := fixture.localState.Logout(); err != nil {
			t.Fatal("stale-cleanup control could not reach the real state wipe")
		}
		cleared = true
		if material != nil {
			if err := fixture.localState.SetDeviceLocalKeyMaterial(material); err != nil {
				t.Fatal("stale-cleanup control could not restore loaded material")
			}
		}
	}
	after, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal("could not read stale-cleanup auth afterward")
	}
	afterBytes, readErr := os.ReadFile(path)
	if loadErr == nil || material != nil || cleared || after != before ||
		readErr != nil || !bytes.Equal(afterBytes, data) {
		t.Fatal("checked key-load admission cleared state after observation failure")
	}
}

// A successful checked read still permits the existing LocalState cleanup
// sequence to restore all known material after deliberately clearing auth.
// This models that storage sequence, not native request/API ownership.
func TestCheckedDeviceKeyLoadStaleCleanupPreservesKnownMaterial(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.seedDistinctLogin(t)
	device := testingDeviceKeyLoadConstructor(t, fixture, nil)
	original := device.GetKeyMaterial()
	testingDeviceKeyLoadJoin(t, device)
	if err := fixture.localState.SetDeviceLocalKeyMaterial(original); err != nil {
		t.Fatal("could not write healthy stale-cleanup material")
	}
	path := filepath.Join(fixture.localState.localStorageDir, ".device_local_key_material")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("could not read healthy stale-cleanup material")
	}
	material, err := fixture.localState.LoadDeviceLocalKeyMaterial()
	if err != nil || material == nil || !testingDeviceKeyLoadEqual(material, original) {
		t.Fatal("healthy stale cleanup did not load known material")
	}
	if err := fixture.localState.Logout(); err != nil {
		t.Fatal("healthy stale cleanup could not clear local auth")
	}
	if err := fixture.localState.SetDeviceLocalKeyMaterial(material); err != nil {
		t.Fatal("healthy stale cleanup could not restore known material")
	}
	restored, err := fixture.localState.LoadDeviceLocalKeyMaterial()
	if err != nil || !testingDeviceKeyLoadEqual(restored, original) {
		t.Fatal("healthy stale cleanup changed known material")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatal("healthy stale cleanup did not restore exact stored material")
	}
	snapshot, err := fixture.localState.GetAuthStateSnapshot()
	if err != nil || snapshot == nil || !snapshot.GetEmpty() {
		t.Fatal("healthy stale cleanup did not clear local auth")
	}
}

// A checked seed-only restore must feed the stored seed into the actual
// constructor, then preserve the exported complete identity on restart.
func TestCheckedDeviceKeyLoadSeedOnlyRestart(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.seedDistinctLogin(t)
	seed := make([]byte, 32)
	for index := range seed {
		seed[index] = byte(index + 1)
	}
	if err := fixture.localState.SetDeviceLocalKeyMaterial(NewDeviceLocalKeyMaterial(seed, nil, nil)); err != nil {
		t.Fatal("could not write checked seed-only material")
	}
	loaded, err := fixture.localState.LoadDeviceLocalKeyMaterial()
	if err != nil || loaded == nil || !bytes.Equal(loaded.GetClientKeySeed(), seed) {
		t.Fatal("checked seed-only fixture did not load")
	}
	first := testingDeviceKeyLoadConstructor(t, fixture, loaded)
	identity := first.GetPublicIdentityKey()
	material := first.GetKeyMaterial()
	if !bytes.Equal(material.GetClientKeySeed(), seed) {
		t.Fatal("checked seed-only constructor changed the stored seed")
	}
	if err := fixture.localState.SetDeviceLocalKeyMaterial(material); err != nil {
		t.Fatal("could not persist the checked seed-only constructor result")
	}
	testingDeviceKeyLoadJoin(t, first)
	reloaded, err := fixture.localState.LoadDeviceLocalKeyMaterial()
	if err != nil || reloaded == nil || !testingDeviceKeyLoadEqual(reloaded, material) {
		t.Fatal("checked seed-only persistence changed optional key fields")
	}
	second := testingDeviceKeyLoadConstructor(t, fixture, reloaded)
	if !bytes.Equal(second.GetPublicIdentityKey(), identity) ||
		!bytes.Equal(second.GetProvideTlsCertificatePem(), material.GetProvideTlsCertificatePem()) {
		t.Fatal("checked seed-only restart changed provider identity")
	}
	instance := fixture.localState.GetInstanceId()
	if fixture.localState.GetByJwt() != fixture.adminJwt ||
		fixture.localState.GetByClientJwt() != fixture.initialJwt ||
		instance == nil || instance.String() != fixture.instanceId.String() {
		t.Fatal("checked seed-only restart changed distinct auth roles or instance")
	}
}

// Actual empty startup may create and persist once; a checked reload restores
// the same provider identity and keeps distinct admin/client auth unchanged.
func TestCheckedDeviceKeyLoadFreshStartAndRestart(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.seedDistinctLogin(t)
	loaded, err := fixture.localState.LoadDeviceLocalKeyMaterial()
	if err != nil || loaded != nil {
		t.Fatal("fresh checked constructor fixture was not empty")
	}
	first := testingDeviceKeyLoadConstructor(t, fixture, loaded)
	identity := first.GetPublicIdentityKey()
	material := first.GetKeyMaterial()
	if err := fixture.localState.SetDeviceLocalKeyMaterial(material); err != nil {
		t.Fatal("could not persist the fresh checked device identity")
	}
	testingDeviceKeyLoadJoin(t, first)
	reloaded, err := fixture.localState.LoadDeviceLocalKeyMaterial()
	if err != nil || reloaded == nil || !testingDeviceKeyLoadEqual(reloaded, material) {
		t.Fatal("checked reload changed the persisted identity material")
	}
	second := testingDeviceKeyLoadConstructor(t, fixture, reloaded)
	if !bytes.Equal(second.GetPublicIdentityKey(), identity) ||
		!bytes.Equal(second.GetProvideTlsCertificatePem(), material.GetProvideTlsCertificatePem()) {
		t.Fatal("checked restart changed the published provider identity")
	}
	instance := fixture.localState.GetInstanceId()
	if fixture.localState.GetByJwt() != fixture.adminJwt ||
		fixture.localState.GetByClientJwt() != fixture.initialJwt ||
		instance == nil || instance.String() != fixture.instanceId.String() {
		t.Fatal("checked key restart changed distinct auth roles or instance")
	}
}
