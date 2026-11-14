package sdk

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	gojwt "github.com/golang-jwt/jwt/v5"
)

func testingClientJwtForIdentity(t *testing.T, clientId string, deviceId string) string {
	t.Helper()
	token, err := gojwt.NewWithClaims(gojwt.SigningMethodNone, gojwt.MapClaims{
		"client_id": clientId,
		"device_id": deviceId,
	}).SignedString(gojwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestJwtRefreshPersistencePreservesInstanceId(t *testing.T) {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "jwtorder-fixed")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	localState := newLocalState(ctx, dir)

	initialJwt := testingRefreshableJwtWithMarker(t, "initial")
	refreshedJwt := testingRefreshableJwtWithMarker(t, "refreshed")
	if err := localState.SetByJwt(initialJwt); err != nil {
		t.Fatal(err)
	}
	if err := localState.SetByClientJwt(initialJwt); err != nil {
		t.Fatal(err)
	}
	initialInstanceId := localState.GetInstanceId()
	if initialInstanceId == nil {
		t.Fatal("after login: instance_id is nil")
	}

	accepted, err := localState.replaceRefreshedByJwt(
		initialJwt,
		refreshedJwt,
		initialInstanceId,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !accepted {
		t.Fatal("current JWT refresh was rejected")
	}
	if got := localState.GetByJwt(); got != initialJwt {
		t.Fatal("client refresh changed the separate stored login credential")
	}
	if got := localState.GetByClientJwt(); got != refreshedJwt {
		t.Fatalf("after refresh: by_client_jwt = %q, want refreshed JWT", got)
	}
	refreshedInstanceId := localState.GetInstanceId()
	if refreshedInstanceId == nil || refreshedInstanceId.Cmp(initialInstanceId) != 0 {
		t.Fatalf(
			"JWT refresh changed instance_id: got %v, want %v",
			refreshedInstanceId,
			initialInstanceId,
		)
	}
}

func TestJwtRefreshPersistenceRepairsMissingInstanceId(t *testing.T) {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "jwt-refresh-missing-instance")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	localState := newLocalState(ctx, dir)

	initialJwt := testingRefreshableJwtWithMarker(t, "initial")
	refreshedJwt := testingRefreshableJwtWithMarker(t, "refreshed")
	if err := localState.SetByJwt(initialJwt); err != nil {
		t.Fatal(err)
	}
	if err := localState.SetByClientJwt(initialJwt); err != nil {
		t.Fatal(err)
	}
	liveInstanceId := localState.GetInstanceId()
	if liveInstanceId == nil {
		t.Fatal("after login: instance_id is nil")
	}
	if err := localState.SetInstanceId(nil); err != nil {
		t.Fatal(err)
	}

	accepted, err := localState.replaceRefreshedByJwt(
		initialJwt,
		refreshedJwt,
		liveInstanceId,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !accepted {
		t.Fatal("refresh did not repair the missing instance")
	}
	repairedInstanceId := localState.GetInstanceId()
	if repairedInstanceId == nil || repairedInstanceId.Cmp(liveInstanceId) != 0 {
		t.Fatalf(
			"refresh repaired instance_id with %v, want live device %v",
			repairedInstanceId,
			liveInstanceId,
		)
	}
}

func TestNewDeviceLoginPersistenceRotatesDifferentIdentity(t *testing.T) {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "jwt-refresh-new-identity")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	localState := newLocalState(ctx, dir)

	initialJwt := testingClientJwtForIdentity(
		t,
		"00000000-0000-0000-0000-000000000001",
		"00000000-0000-0000-0000-000000000002",
	)
	replacementJwt := testingClientJwtForIdentity(
		t,
		"00000000-0000-0000-0000-000000000001",
		"00000000-0000-0000-0000-000000000003",
	)
	if err := localState.SetByJwt(initialJwt); err != nil {
		t.Fatal(err)
	}
	if err := localState.SetByClientJwt(initialJwt); err != nil {
		t.Fatal(err)
	}
	initialInstanceId := localState.GetInstanceId()
	if initialInstanceId == nil {
		t.Fatal("after login: instance_id is nil")
	}

	if err := localState.SetByJwt(replacementJwt); err != nil {
		t.Fatal(err)
	}
	if err := localState.SetByClientJwt(replacementJwt); err != nil {
		t.Fatal(err)
	}
	replacementInstanceId := localState.GetInstanceId()
	if replacementInstanceId == nil {
		t.Fatal("replacement identity has no instance_id")
	}
	if replacementInstanceId.Cmp(initialInstanceId) == 0 {
		t.Fatal("different device identity retained the previous instance_id")
	}
}

func TestSetByJwtForNewLoginStillClearsClientAndInstance(t *testing.T) {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "jwt-new-login")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	localState := newLocalState(ctx, dir)

	clientJwt := testingRefreshableJwtWithMarker(t, "initial")
	if err := localState.SetByJwt(clientJwt); err != nil {
		t.Fatal(err)
	}
	if err := localState.SetByClientJwt(clientJwt); err != nil {
		t.Fatal(err)
	}
	if localState.GetInstanceId() == nil {
		t.Fatal("after login: instance_id is nil")
	}

	if err := localState.SetByJwt("NETWORK_LOGIN_JWT"); err != nil {
		t.Fatal(err)
	}
	if got := localState.GetByClientJwt(); got != "" {
		t.Fatalf("new login retained by_client_jwt = %q", got)
	}
	if localState.GetInstanceId() != nil {
		t.Fatal("new login retained the previous instance_id")
	}
}

func TestSetByClientJwtForInstanceCommitsOneCoherentGeneration(t *testing.T) {
	localState := newLocalState(context.Background(), t.TempDir())
	initialJwt := testingRefreshableJwtWithMarker(t, "existing-instance-initial")
	if err := localState.SetByJwt(initialJwt); err != nil {
		t.Fatal(err)
	}
	if err := localState.SetByClientJwt(initialJwt); err != nil {
		t.Fatal(err)
	}
	instanceId := localState.GetInstanceId()
	if instanceId == nil {
		t.Fatal("existing instance_id is nil")
	}
	before, err := localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}

	rotatedJwt := testingRefreshableJwtWithMarker(t, "existing-instance-rotated")
	if err := localState.SetByClientJwtForInstance(rotatedJwt, instanceId); err != nil {
		t.Fatal(err)
	}
	after, err := localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if after.ByJwt != before.ByJwt || after.ByClientJwt != rotatedJwt {
		t.Fatal("atomic client seed changed the admin credential or lost the client rotation")
	}
	if after.InstanceId != instanceId.String() {
		t.Fatalf("atomic seed instance_id = %q, want %q", after.InstanceId, instanceId)
	}
	if after.Generation != before.Generation+1 {
		t.Fatalf(
			"atomic seed generation = %d, want exactly one generation after %d",
			after.Generation,
			before.Generation,
		)
	}
}

func TestSetByClientJwtForInstanceRejectsIncompleteStateWithoutMutation(t *testing.T) {
	localState := newLocalState(context.Background(), t.TempDir())
	initialJwt := testingRefreshableJwtWithMarker(t, "missing-instance-initial")
	if err := localState.SetByJwt(initialJwt); err != nil {
		t.Fatal(err)
	}
	before, err := localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name       string
		byJwt      string
		instanceId *Id
	}{
		{name: "empty JWT", instanceId: NewId()},
		{name: "nil instance", byJwt: "replacement"},
	} {
		if err := localState.SetByClientJwtForInstance(test.byJwt, test.instanceId); err == nil {
			t.Fatalf("%s: atomic existing-instance seed accepted incomplete state", test.name)
		}
	}
	after, err := localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("rejected seed mutated auth state: before=%+v after=%+v", before, after)
	}
}

func TestRefreshedJwtCannotRecreateLoggedOutAuthState(t *testing.T) {
	localState := newLocalState(context.Background(), t.TempDir())
	initialJwt := testingRefreshableJwtWithMarker(t, "logout-race-initial")
	refreshedJwt := testingRefreshableJwtWithMarker(t, "logout-race-refreshed")
	if err := localState.SetByJwt(initialJwt); err != nil {
		t.Fatal(err)
	}
	if err := localState.SetByClientJwt(initialJwt); err != nil {
		t.Fatal(err)
	}
	instanceId := localState.GetInstanceId()
	if instanceId == nil {
		t.Fatal("initial instance_id is nil")
	}
	if err := localState.Logout(); err != nil {
		t.Fatal(err)
	}

	accepted, err := localState.replaceRefreshedByJwt(
		initialJwt,
		refreshedJwt,
		instanceId,
	)
	if err != nil {
		t.Fatal(err)
	}
	if accepted {
		t.Fatal("retired refresh recreated logged-out auth state")
	}
	state, err := localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if state.ByJwt != "" || state.ByClientJwt != "" || state.InstanceId != "" {
		t.Fatalf("retired refresh recreated auth: %+v", state)
	}
}

func TestOlderSessionRefreshCannotReplaceNewerSameInstanceSeed(t *testing.T) {
	localState := newLocalState(context.Background(), t.TempDir())
	oldJwt := testingRefreshableJwtWithMarker(t, "old-session")
	oldRefreshJwt := testingRefreshableJwtWithMarker(t, "old-session-refresh")
	newJwt := testingRefreshableJwtWithMarker(t, "new-session")
	if err := localState.SetByJwt(oldJwt); err != nil {
		t.Fatal(err)
	}
	if err := localState.SetByClientJwt(oldJwt); err != nil {
		t.Fatal(err)
	}
	instanceId := localState.GetInstanceId()
	if instanceId == nil {
		t.Fatal("old session instance_id is nil")
	}
	if err := localState.SetByClientJwtForInstance(newJwt, instanceId); err != nil {
		t.Fatal(err)
	}

	accepted, err := localState.replaceRefreshedByJwt(oldJwt, oldRefreshJwt, instanceId)
	if err != nil {
		t.Fatal(err)
	}
	if accepted {
		t.Fatal("old session refresh replaced a newer same-instance seed")
	}
	state, err := localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if state.ByJwt != oldJwt || state.ByClientJwt != newJwt || state.InstanceId != instanceId.String() {
		t.Fatal("old refresh changed the admin credential or newer same-instance client auth")
	}
}

func TestSetByClientJwtRepairsMissingPairedInstance(t *testing.T) {
	localState := newLocalState(context.Background(), t.TempDir())
	clientJwt := testingRefreshableJwtWithMarker(t, "repair-client-instance")
	if err := localState.SetByJwt(clientJwt); err != nil {
		t.Fatal(err)
	}
	if err := localState.SetByClientJwt(clientJwt); err != nil {
		t.Fatal(err)
	}
	previous := localState.GetInstanceId()
	if previous == nil {
		t.Fatal("initial client JWT has no paired instance")
	}
	if err := localState.SetInstanceId(nil); err != nil {
		t.Fatal(err)
	}

	if err := localState.SetByClientJwt(clientJwt); err != nil {
		t.Fatal(err)
	}
	repaired := localState.GetInstanceId()
	if repaired == nil {
		t.Fatal("equal client JWT did not repair its missing instance")
	}
	if repaired.Cmp(previous) == 0 {
		t.Fatal("repair reused the lost instance instead of creating a new device session")
	}
}

func TestAuthStateMigratesLegacyFilesIntoAtomicEnvelope(t *testing.T) {
	home := t.TempDir()
	legacyDir := filepath.Join(home, ".by")
	if err := os.MkdirAll(legacyDir, LocalStorageDirectoryPermissions); err != nil {
		t.Fatal(err)
	}
	byJwt := testingRefreshableJwtWithMarker(t, "legacy-network")
	clientJwt := testingRefreshableJwtWithMarker(t, "legacy-client")
	instanceId := NewId()
	legacy := map[string][]byte{
		legacyByJwtFileName:     []byte(byJwt),
		legacyClientJwtFileName: []byte(clientJwt),
		legacyInstanceFileName:  instanceId.Bytes(),
	}
	for name, data := range legacy {
		if err := os.WriteFile(
			filepath.Join(legacyDir, name),
			data,
			LocalStorageFilePermissions,
		); err != nil {
			t.Fatal(err)
		}
	}

	localState := newLocalState(context.Background(), home)
	if got := localState.GetByJwt(); got != byJwt {
		t.Fatalf("migrated by_jwt = %q, want %q", got, byJwt)
	}
	if got := localState.GetByClientJwt(); got != clientJwt {
		t.Fatalf("migrated by_client_jwt = %q, want %q", got, clientJwt)
	}
	if got := localState.GetInstanceId(); got == nil || got.Cmp(instanceId) != 0 {
		t.Fatalf("migrated instance_id = %v, want %v", got, instanceId)
	}
	info, err := os.Stat(filepath.Join(legacyDir, localAuthStateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != LocalStorageFilePermissions {
		t.Fatalf("auth envelope permissions = %#o, want %#o", got, LocalStorageFilePermissions)
	}
	for name := range legacy {
		if _, err := os.Stat(filepath.Join(legacyDir, name)); !os.IsNotExist(err) {
			t.Fatalf("legacy auth file %s remains after migration: %v", name, err)
		}
	}
}

func TestAuthStateSnapshotIsOneImmutableGeneration(t *testing.T) {
	localState := newLocalState(context.Background(), t.TempDir())
	firstInstance := NewId()
	secondInstance := NewId()
	if err := localState.SetByClientJwtForInstance("first-jwt", firstInstance); err != nil {
		t.Fatal(err)
	}

	snapshot, err := localState.GetAuthStateSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.GetEmpty() {
		t.Fatal("seeded auth snapshot reported empty")
	}
	if got := snapshot.GetInstanceId(); got == nil || got.Cmp(firstInstance) != 0 {
		t.Fatal("snapshot instance does not match first generation")
	}

	if err := localState.SetByClientJwtForInstance("second-jwt", secondInstance); err != nil {
		t.Fatal(err)
	}
	if got := snapshot.GetInstanceId(); got == nil || got.Cmp(firstInstance) != 0 {
		t.Fatal("later auth mutation tore the prior snapshot instance")
	}
}

// Snapshot consumers cannot mutate the next reader's identity through Id.
func TestAuthStateSnapshotDoesNotExposeMutableIdentity(t *testing.T) {
	localState := newLocalState(context.Background(), t.TempDir())
	instanceId := NewId()
	if err := localState.SetByClientJwtForInstance("snapshot-jwt", instanceId); err != nil {
		t.Fatal(err)
	}
	snapshot, err := localState.GetAuthStateSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	exposed := snapshot.GetInstanceId()
	exposed.Bytes()[0] ^= 0xff
	exposed.IdStr = "changed"
	if got := snapshot.GetInstanceId(); got.Cmp(instanceId) != 0 || got.String() != instanceId.String() {
		t.Fatal("a caller mutated the snapshot's stable identity")
	}
}

// Absence is distinct from a partial login and a coherent old-build envelope.
func TestAuthStateSnapshotDistinguishesAbsentAndIncompleteIdentity(t *testing.T) {
	localState := newLocalState(context.Background(), t.TempDir())
	snapshot, err := localState.GetAuthStateSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.GetEmpty() || snapshot.GetInstanceId() != nil {
		t.Fatal("absent auth was not reported as an empty snapshot")
	}
	if err := localState.SetByJwt("partial-login-jwt"); err != nil {
		t.Fatal(err)
	}
	snapshot, err = localState.GetAuthStateSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.GetEmpty() || snapshot.GetInstanceId() != nil {
		t.Fatal("partial auth was reported as empty or attributed to an instance")
	}
	instanceId := NewId()
	if err := localState.SetInstanceId(instanceId); err != nil {
		t.Fatal(err)
	}
	snapshot, err = localState.GetAuthStateSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.GetEmpty() || snapshot.GetInstanceId().Cmp(instanceId) != 0 {
		t.Fatal("the pre-envelope startup pair lost its stable identity")
	}
}

func TestAuthStateSnapshotReportsCorruptionWithoutMutation(t *testing.T) {
	localState := newLocalState(context.Background(), t.TempDir())
	path := filepath.Join(localState.localStorageDir, localAuthStateFileName)
	const corrupt = "not-json"
	if err := os.WriteFile(path, []byte(corrupt), LocalStorageFilePermissions); err != nil {
		t.Fatal(err)
	}

	if _, err := localState.GetAuthStateSnapshot(); err == nil {
		t.Fatal("corrupt auth envelope was reported as empty state")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != corrupt {
		t.Fatal("failed snapshot read mutated the auth envelope")
	}
}

func TestAuthStateSnapshotRejectsUnavailableEnvelopeWithoutMutation(t *testing.T) {
	localState := newLocalState(context.Background(), t.TempDir())
	target := filepath.Join(t.TempDir(), "other-auth-state")
	const original = "external-state"
	if err := os.WriteFile(target, []byte(original), LocalStorageFilePermissions); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(localState.localStorageDir, localAuthStateFileName)
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}

	if _, err := localState.GetAuthStateSnapshot(); err == nil {
		t.Fatal("non-regular auth envelope was reported as absent state")
	}
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != original {
		t.Fatal("failed snapshot read mutated the referenced state")
	}
}

func TestAtomicAuthCommitFailurePreservesPreviousGeneration(t *testing.T) {
	localState := newLocalState(context.Background(), t.TempDir())
	initialJwt := testingRefreshableJwtWithMarker(t, "atomic-initial")
	if err := localState.SetByJwt(initialJwt); err != nil {
		t.Fatal(err)
	}
	if err := localState.SetByClientJwt(initialJwt); err != nil {
		t.Fatal(err)
	}
	instanceId := localState.GetInstanceId()
	before, err := localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(localState.localStorageDir, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(localState.localStorageDir, LocalStorageDirectoryPermissions)
	refreshedJwt := testingRefreshableJwtWithMarker(t, "atomic-new")
	if err := localState.SetByClientJwtForInstance(refreshedJwt, instanceId); err == nil {
		t.Fatal("refresh unexpectedly succeeded in a read-only auth directory")
	}
	if err := os.Chmod(localState.localStorageDir, LocalStorageDirectoryPermissions); err != nil {
		t.Fatal(err)
	}

	after, err := localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("failed commit changed auth state: before=%+v after=%+v", before, after)
	}
}

func TestCorruptAuthEnvelopeNeverFallsBackToStaleLegacyCredentials(t *testing.T) {
	home := t.TempDir()
	stateDir := filepath.Join(home, ".by")
	if err := os.MkdirAll(stateDir, LocalStorageDirectoryPermissions); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(stateDir, localAuthStateFileName),
		[]byte("{not-json"),
		LocalStorageFilePermissions,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(stateDir, legacyByJwtFileName),
		[]byte("stale-legacy-jwt"),
		LocalStorageFilePermissions,
	); err != nil {
		t.Fatal(err)
	}

	localState := newLocalState(context.Background(), home)
	if got := localState.GetByJwt(); got != "" {
		t.Fatalf("corrupt authoritative envelope fell back to legacy jwt %q", got)
	}
	if err := localState.SetByJwt("replacement"); err == nil {
		t.Fatal("a normal mutation silently overwrote corrupt auth state")
	}
	if err := localState.Logout(); err != nil {
		t.Fatal(err)
	}
	if err := localState.SetByJwt("replacement"); err != nil {
		t.Fatalf("explicit logout did not recover corrupt auth state: %v", err)
	}
}

func TestInterruptedAuthTempCannotOverrideCommittedEnvelope(t *testing.T) {
	home := t.TempDir()
	localState := newLocalState(context.Background(), home)
	committedJwt := testingRefreshableJwtWithMarker(t, "committed")
	if err := localState.SetByJwt(committedJwt); err != nil {
		t.Fatal(err)
	}
	interrupted := persistedLocalAuthState{
		Version:    localAuthStateVersion,
		Generation: 999,
		ByJwt:      "interrupted-newer-looking-token",
	}
	data, err := json.Marshal(interrupted)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(localState.localStorageDir, localAuthStateFileName+".tmp-crash"),
		data,
		LocalStorageFilePermissions,
	); err != nil {
		t.Fatal(err)
	}

	relaunched := newLocalState(context.Background(), home)
	if got := relaunched.GetByJwt(); got != committedJwt {
		t.Fatalf("interrupted temp won over committed envelope: got %q", got)
	}
}
