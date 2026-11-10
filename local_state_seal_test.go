package sdk

// .auth_state sealed by the app's keystore (local_state_seal.go): the record holds no credential, opens only under
// its own key, is refused with no sealer or a broken byte, and a locked or failed sealer signs nobody out and
// deletes nothing. The sealer is an in-memory fake that answers statuses as an app's would.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// An app's keystore in memory: keys by name, a lock the test turns, and every call it took.
type testLocalStateSealer struct {
	stateLock sync.Mutex
	keys      map[string]string
	locked    bool
	// answers this instead of the store's own, when set: a panic, nil or an unknown status
	misbehave string
	puts      []string
	deletes   []string
}

func newTestLocalStateSealer() *testLocalStateSealer {
	return &testLocalStateSealer{keys: map[string]string{}}
}

func (self *testLocalStateSealer) PutSealKey(name string, keyBase64 string) int32 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.puts = append(self.puts, name)
	switch {
	case self.misbehave == "panic":
		panic("the keystore panicked")
	case self.misbehave != "":
		return 7
	case self.locked:
		return LocalStateSealerLocked
	}
	self.keys[name] = keyBase64
	return LocalStateSealerOk
}

func (self *testLocalStateSealer) GetSealKey(name string) *LocalStateSealKey {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	switch self.misbehave {
	case "panic":
		panic("the keystore panicked")
	case "nil":
		return nil
	case "status":
		return &LocalStateSealKey{Status: 7}
	case "short":
		return &LocalStateSealKey{Status: LocalStateSealKeyPresent, KeyBase64: base64.StdEncoding.EncodeToString([]byte("short"))}
	}
	if self.locked {
		return &LocalStateSealKey{Status: LocalStateSealKeyLocked}
	}
	key, ok := self.keys[name]
	if !ok {
		return &LocalStateSealKey{Status: LocalStateSealKeyAbsent}
	}
	return &LocalStateSealKey{Status: LocalStateSealKeyPresent, KeyBase64: key}
}

func (self *testLocalStateSealer) DeleteSealKey(name string) int32 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.deletes = append(self.deletes, name)
	if self.locked {
		return LocalStateSealerLocked
	}
	delete(self.keys, name)
	return LocalStateSealerOk
}

func (self *testLocalStateSealer) setLocked(locked bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.locked = locked
}

func (self *testLocalStateSealer) setMisbehave(misbehave string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.misbehave = misbehave
}

// The names it holds, the puts and the deletes so far.
func (self *testLocalStateSealer) calls() (keyNames []string, puts []string, deletes []string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for name := range self.keys {
		keyNames = append(keyNames, name)
	}
	return keyNames, append([]string{}, self.puts...), append([]string{}, self.deletes...)
}

// A local state at home whose .auth_state the sealer seals, nil for none.
func newTestSealedLocalState(t *testing.T, home string, sealer LocalStateSealer, sessionOnly bool) *LocalState {
	t.Helper()
	localState := newLocalStateWithAuthSettings(context.Background(), home, newLocalAuthSettings(sessionOnly, sealer))
	t.Cleanup(localState.Close)
	return localState
}

// The bytes of home's .auth_state.
func testAuthStateFile(t *testing.T, home string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".by", localAuthStateFileName))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// The file holds none of the secrets.
func requireNoSecretInAuthStateFile(t *testing.T, data []byte, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if secret != "" && bytes.Contains(data, []byte(secret)) {
			t.Fatal("the auth state file holds a credential in the clear")
		}
	}
}

func TestSealedAuthStateRoundTrip(t *testing.T) {
	home := t.TempDir()
	sealer := newTestLocalStateSealer()
	networkJwt := credentialTestNetworkJwt(t, credentialTestNetworkId)
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "sealed")
	localState := newTestSealedLocalState(t, home, sealer, false)
	if err := localState.SetByJwt(networkJwt); err != nil {
		t.Fatal(err)
	}
	if err := localState.SetByClientJwt(clientJwt); err != nil {
		t.Fatal(err)
	}
	instanceId := localState.GetInstanceId()

	// the record: the magic, its key's id, and nothing readable
	sealed := testAuthStateFile(t, home)
	if !isSealedLocalAuthState(sealed) || json.Valid(sealed) {
		t.Fatal("the auth state was written in the clear")
	}
	requireNoSecretInAuthStateFile(t, sealed, networkJwt, clientJwt, instanceId.String())
	keyId := sealed[len(localAuthStateSealMagic) : len(localAuthStateSealMagic)+localAuthStateSealKeyIdLength]
	keyNames, puts, _ := sealer.calls()
	if len(keyNames) != 1 || keyNames[0] != "auth_state."+hex.EncodeToString(keyId) || len(puts) != 1 {
		t.Fatalf("the sealer holds %v after puts %v", keyNames, puts)
	}

	// a later write keeps the file's key under a fresh nonce
	refreshedJwt := credentialTestClientJwt(t, credentialTestNetworkId, "sealed-refreshed")
	if accepted, err := localState.replaceRefreshedByJwt(clientJwt, refreshedJwt, instanceId); err != nil || !accepted {
		t.Fatalf("refresh: %v %v", accepted, err)
	}
	resealed := testAuthStateFile(t, home)
	if !bytes.Equal(resealed[:localAuthStateSealHeaderLength-localAuthStateSealNonceLength], sealed[:localAuthStateSealHeaderLength-localAuthStateSealNonceLength]) {
		t.Fatal("a rewrite changed the file's key")
	}
	if bytes.Equal(resealed[localAuthStateSealHeaderLength-localAuthStateSealNonceLength:localAuthStateSealHeaderLength], sealed[localAuthStateSealHeaderLength-localAuthStateSealNonceLength:localAuthStateSealHeaderLength]) {
		t.Fatal("a rewrite reused the nonce")
	}
	if _, puts, _ := sealer.calls(); len(puts) != 1 {
		t.Fatalf("a rewrite made another key: %v", puts)
	}

	// the same sealer opens it from a new process
	reopened := newTestSealedLocalState(t, home, sealer, false)
	if reopened.GetByJwt() != networkJwt || reopened.GetByClientJwt() != refreshedJwt || reopened.GetInstanceId().String() != instanceId.String() {
		t.Fatal("the sealed auth state did not round trip")
	}

	// no sealer: refused, never read as empty or legacy, and left as it is
	if err := os.WriteFile(filepath.Join(home, ".by", legacyByJwtFileName), []byte(networkJwt), LocalStorageFilePermissions); err != nil {
		t.Fatal(err)
	}
	unsealed := newTestSealedLocalState(t, home, nil, false)
	if _, err := unsealed.GetAuthStateSnapshot(); !errors.Is(err, errLocalAuthStateSealedWithoutSealer) {
		t.Fatalf("a sealed file read with no sealer: %v", err)
	}
	if unsealed.GetByJwt() != "" || unsealed.GetByClientJwt() != "" {
		t.Fatal("a sealed file fell back to something else")
	}
	if err := unsealed.SetByJwt(networkJwt); err == nil {
		t.Fatal("a write went through over a file it could not read")
	}
	if !bytes.Equal(testAuthStateFile(t, home), resealed) {
		t.Fatal("the refused read changed the file")
	}
	os.Remove(filepath.Join(home, ".by", legacyByJwtFileName))

	// a flipped byte, a truncation and a key the store lost each open nothing
	for _, broken := range [][]byte{
		func() []byte { b := bytes.Clone(resealed); b[len(b)-1] ^= 1; return b }(),
		func() []byte { b := bytes.Clone(resealed); b[localAuthStateSealHeaderLength] ^= 1; return b }(),
		resealed[:len(resealed)-1],
		resealed[:localAuthStateSealHeaderLength-1],
	} {
		if err := os.WriteFile(filepath.Join(home, ".by", localAuthStateFileName), broken, LocalStorageFilePermissions); err != nil {
			t.Fatal(err)
		}
		tampered := newTestSealedLocalState(t, home, sealer, false)
		if _, err := tampered.GetAuthStateSnapshot(); !errors.Is(err, errLocalAuthStateSealBroken) {
			t.Fatalf("a broken record read: %v", err)
		}
		if !bytes.Equal(testAuthStateFile(t, home), broken) {
			t.Fatal("a broken record was rewritten")
		}
	}
	if err := os.WriteFile(filepath.Join(home, ".by", localAuthStateFileName), resealed, LocalStorageFilePermissions); err != nil {
		t.Fatal(err)
	}
	other := newTestSealedLocalState(t, home, newTestLocalStateSealer(), false)
	if _, err := other.GetAuthStateSnapshot(); !errors.Is(err, errLocalAuthStateSealKeyAbsent) {
		t.Fatalf("a record read under another store: %v", err)
	}
	if !bytes.Equal(testAuthStateFile(t, home), resealed) {
		t.Fatal("a record whose key is absent was rewritten")
	}

	// a sign-out removes the file, then its key
	signedIn := newTestSealedLocalState(t, home, sealer, false)
	if err := signedIn.Logout(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(home, ".by", localAuthStateFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the sign-out left the auth state")
	}
	if keyNames, _, deletes := sealer.calls(); len(keyNames) != 0 || len(deletes) != 1 {
		t.Fatalf("the sign-out left keys %v after deletes %v", keyNames, deletes)
	}
}

// Today's plaintext envelope is sealed at its first read once a sealer is installed, and keeps every value.
func TestPlaintextAuthStateIsSealedAtItsFirstRead(t *testing.T) {
	home := t.TempDir()
	networkJwt := credentialTestNetworkJwt(t, credentialTestNetworkId)
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "plaintext")
	plain := newTestSealedLocalState(t, home, nil, false)
	if err := plain.SetByJwt(networkJwt); err != nil {
		t.Fatal(err)
	}
	if err := plain.SetByClientJwt(clientJwt); err != nil {
		t.Fatal(err)
	}
	before, err := plain.loadAuthState()
	if err != nil || !json.Valid(testAuthStateFile(t, home)) {
		t.Fatal("the plaintext fixture was not written")
	}

	sealer := newTestLocalStateSealer()
	sealed := newTestSealedLocalState(t, home, sealer, false)
	data := testAuthStateFile(t, home)
	if !isSealedLocalAuthState(data) {
		t.Fatal("the first read with a sealer left the file in the clear")
	}
	requireNoSecretInAuthStateFile(t, data, networkJwt, clientJwt)
	after, err := sealed.loadAuthState()
	if err != nil || after != before {
		t.Fatalf("sealing changed the state: %v", err)
	}
}

// A sealer that panics, answers nil, a status nobody knows or a key of the wrong size is Failed: reads fail, and
// nothing is written or deleted.
func TestMisbehavingSealerIsFailed(t *testing.T) {
	for _, misbehave := range []string{"panic", "nil", "status", "short"} {
		home := t.TempDir()
		sealer := newTestLocalStateSealer()
		localState := newTestSealedLocalState(t, home, sealer, false)
		if err := localState.SetByJwt(credentialTestNetworkJwt(t, credentialTestNetworkId)); err != nil {
			t.Fatal(err)
		}
		before := testAuthStateFile(t, home)
		sealer.setMisbehave(misbehave)
		reopened := newTestSealedLocalState(t, home, sealer, false)
		if _, err := reopened.GetAuthStateSnapshot(); !errors.Is(err, ErrLocalStateSealerFailed) {
			t.Fatalf("%s: a read answered %v", misbehave, err)
		}
		if err := reopened.SetByJwt(credentialTestNetworkJwt(t, credentialTestOtherNetworkId)); err == nil {
			t.Fatalf("%s: a write went through", misbehave)
		}
		if !bytes.Equal(testAuthStateFile(t, home), before) {
			t.Fatalf("%s: the file changed", misbehave)
		}
		if _, _, deletes := sealer.calls(); len(deletes) != 0 {
			t.Fatalf("%s: a key was deleted", misbehave)
		}
	}
}

func TestLockedSealerDeletesNothing(t *testing.T) {
	home := t.TempDir()
	sealer := newTestLocalStateSealer()
	networkJwt := credentialTestNetworkJwt(t, credentialTestNetworkId)
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "locked")
	localState := newTestSealedLocalState(t, home, sealer, false)
	if err := localState.SetByJwt(networkJwt); err != nil {
		t.Fatal(err)
	}
	if err := localState.SetByClientJwt(clientJwt); err != nil {
		t.Fatal(err)
	}
	instanceId := localState.GetInstanceId()
	before := testAuthStateFile(t, home)
	keyNames, puts, _ := sealer.calls()

	// locked: a new process reads nothing, writes nothing, and stays signed in
	sealer.setLocked(true)
	locked := newTestSealedLocalState(t, home, sealer, false)
	if _, err := locked.GetAuthStateSnapshot(); !errors.Is(err, ErrLocalStateSealerLocked) {
		t.Fatalf("a locked read answered %v", err)
	}
	if !locked.currentSignIn().signedIn {
		t.Fatal("a locked keystore signed the space out")
	}
	if err := locked.SetByJwt(credentialTestNetworkJwt(t, credentialTestOtherNetworkId)); !errors.Is(err, ErrLocalStateSealerLocked) {
		t.Fatalf("a locked write answered %v", err)
	}
	if err := locked.SetByClientJwt(""); !errors.Is(err, ErrLocalStateSealerLocked) {
		t.Fatalf("a locked client removal answered %v", err)
	}
	if _, err := newApi(t.Context(), nil, "https://locked.example").prepareDeviceAuth(locked, clientJwt, instanceId, renewalTestBaseTime, newDeviceAuthPublicationGate()); !errors.Is(err, ErrLocalStateSealerLocked) {
		t.Fatalf("a device started over a locked keystore: %v", err)
	}
	if !locked.currentSignIn().signedIn {
		t.Fatal("a refused write signed the space out")
	}
	if !bytes.Equal(testAuthStateFile(t, home), before) {
		t.Fatal("a locked keystore changed the file")
	}
	if names, lockedPuts, deletes := sealer.calls(); len(names) != len(keyNames) || len(lockedPuts) != len(puts) || len(deletes) != 0 {
		t.Fatalf("a locked keystore was written: keys %v puts %v deletes %v", names, lockedPuts, deletes)
	}

	// unlocked: the state reads as it was
	sealer.setLocked(false)
	if locked.GetByJwt() != networkJwt || locked.GetByClientJwt() != clientJwt || locked.GetInstanceId().String() != instanceId.String() {
		t.Fatal("the state did not read back once the keystore unlocked")
	}
}

// A locked keystore keeps a plaintext envelope readable as it is, and seals it once it unlocks.
func TestLockedSealerLeavesPlaintextUntilItUnlocks(t *testing.T) {
	home := t.TempDir()
	networkJwt := credentialTestNetworkJwt(t, credentialTestNetworkId)
	plain := newTestSealedLocalState(t, home, nil, false)
	if err := plain.SetByJwt(networkJwt); err != nil {
		t.Fatal(err)
	}
	before := testAuthStateFile(t, home)
	sealer := newTestLocalStateSealer()
	sealer.setLocked(true)
	localState := newTestSealedLocalState(t, home, sealer, false)
	if localState.GetByJwt() != networkJwt || !bytes.Equal(testAuthStateFile(t, home), before) {
		t.Fatal("a locked keystore changed a plaintext envelope")
	}
	sealer.setLocked(false)
	if localState.GetByJwt() != networkJwt || !isSealedLocalAuthState(testAuthStateFile(t, home)) {
		t.Fatal("the envelope was not sealed once the keystore unlocked")
	}
}

// A space's manager hands its sealer to the space's local state, with the space's credential rules.
func TestNetworkSpaceManagerSealsItsSpaces(t *testing.T) {
	home := t.TempDir()
	sealer := newTestLocalStateSealer()
	space := newNetworkSpaceWithConnectSettingsAndApiAlt(
		context.Background(),
		*NewNetworkSpaceKey("sealed-auth.test", "test"),
		NetworkSpaceValues{ApiUrl: "http://127.0.0.1:1", PlatformUrl: "ws://127.0.0.1:1", SessionOnlyNetworkCredential: true},
		home,
		newNetworkSpaceConnectSettings(),
		false,
		true,
		sealer,
	)
	t.Cleanup(space.close)
	localState := space.GetAsyncLocalState().GetLocalState()
	if !localState.sessionOnlyNetworkCredential() || localState.authSettings.sealer == nil {
		t.Fatal("the space's local state does not follow its values and its manager's sealer")
	}
	if err := localState.SetByJwt(credentialTestNetworkJwt(t, credentialTestNetworkId)); err != nil {
		t.Fatal(err)
	}
	if !isSealedLocalAuthState(testAuthStateFile(t, home)) {
		t.Fatal("the space wrote its auth state in the clear")
	}
}
