// These controls use real temporary NetworkSpace storage, API commits, and
// constructor-installed publishers. Channels order the disputed interleavings.
package sdk

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Stop only the optional timer worker, not the API's ownership context. API
// mutation methods and actual local/remote constructor listeners remain live.
func testingPairedAuthSpaceAt(t *testing.T, home string) *testingAuthClientShape {
	t.Helper()
	space := newNetworkSpace(context.Background(), *NewNetworkSpaceKey("paired-auth.test", "test"),
		NetworkSpaceValues{ApiUrl: "http://127.0.0.1:1", PlatformUrl: "ws://127.0.0.1:1"}, home)
	t.Cleanup(space.close)
	api := space.GetApi()
	api.tokenManager.Close()
	testingAwaitAuthBoundary(t, api.tokenManager.done)
	return &testingAuthClientShape{
		networkSpace: space,
		localState:   space.asyncLocalState.localState,
		api:          api,
		initialJwt:   testingRefreshableJwtWithMarker(t, "paired-initial"),
		instanceId:   NewId(),
	}
}

func testingPairedAuthSpace(t *testing.T) *testingAuthClientShape {
	t.Helper()
	return testingPairedAuthSpaceAt(t, t.TempDir())
}

func testingPairedAuthSnapshot(t *testing.T, fixture *testingAuthClientShape) *LocalAuthStateSnapshot {
	t.Helper()
	snapshot, err := fixture.networkSpace.GetAuthStateSnapshot()
	if err != nil || snapshot == nil {
		t.Fatal("could not capture the real auth pair")
	}
	return snapshot
}

// One helper observes complete durable/API values, never returns a decision
// that substitutes for the production reset. Fixture credentials are synthetic.
func testingRequireAuthUnchanged(t *testing.T, fixture *testingAuthClientShape, before persistedLocalAuthState, apiJwt string) {
	t.Helper()
	after, err := fixture.localState.loadAuthState()
	if err != nil || after != before || fixture.api.GetByJwt() != apiJwt {
		t.Fatal("observation or refused reset changed the actual auth pair")
	}
}

func testingRequireSupersededReset(t *testing.T, fixture *testingAuthClientShape, snapshot *LocalAuthStateSnapshot) {
	t.Helper()
	before, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal("could not observe retained auth")
	}
	apiJwt := fixture.api.GetByJwt()
	result, err := fixture.networkSpace.ResetLocalStateIfCurrent(snapshot)
	if err != nil || result == nil || result.GetReset() || result.GetDeviceLocalKeyMaterial() != nil {
		t.Fatal("superseded reset was not a distinct non-destructive result")
	}
	testingRequireAuthUnchanged(t, fixture, before, apiJwt)
}

// Healthy keys and both routing records are created through public persistence.
func testingSeedPreservedState(t *testing.T, fixture *testingAuthClientShape) []byte {
	t.Helper()
	if err := fixture.localState.SetDeviceLocalKeyMaterial(NewDeviceLocalKeyMaterial([]byte{1, 2, 3}, []byte("synthetic-cert"), []byte("synthetic-private-key"))); err != nil {
		t.Fatal("could not save test identity")
	}
	if err := fixture.localState.SetConnectLocation(testingStoredLocation("old-private-peer")); err != nil {
		t.Fatal("could not save destination")
	}
	if err := fixture.localState.SetDefaultLocation(testingStoredLocation("old-default")); err != nil {
		t.Fatal("could not save default")
	}
	data, err := os.ReadFile(filepath.Join(fixture.localState.localStorageDir, ".device_local_key_material"))
	if err != nil {
		t.Fatal("could not observe original identity bytes")
	}
	return data
}

// A genuine current stale owner is the positive control. Cleanup must clear
// the old account's routing/auth but retain the exact identity file in place.
func TestPairedResetCurrentOwnerPreservesExactKeysAndClearsRouting(t *testing.T) {
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	fixture.startLocal(t)
	original := testingSeedPreservedState(t, fixture)
	keyPath := filepath.Join(fixture.localState.localStorageDir, ".device_local_key_material")
	beforeInfo, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal("could not stat saved keys")
	}
	if err := os.WriteFile(keyPath+".old", []byte{4}, LocalStorageFilePermissions); err != nil {
		t.Fatal("could not save similarly named stale file")
	}
	snapshot := testingPairedAuthSnapshot(t, fixture)
	result, err := fixture.networkSpace.ResetLocalStateIfCurrent(snapshot)
	if err != nil || result == nil || !result.GetReset() {
		t.Fatal("genuinely current stale state did not reset")
	}
	afterInfo, statErr := os.Stat(keyPath)
	after, readErr := os.ReadFile(keyPath)
	if statErr != nil || readErr != nil || !os.SameFile(beforeInfo, afterInfo) || !bytes.Equal(original, after) {
		t.Fatal("reset deleted, rewrote, or changed checked identity bytes")
	}
	entries, err := os.ReadDir(fixture.localState.localStorageDir)
	if err != nil || len(entries) != 1 || entries[0].Name() != ".device_local_key_material" {
		t.Fatal("reset preserved state other than the exact key record")
	}
	if fixture.api.GetByJwt() != "" {
		t.Fatal("reset retained the stale API credential")
	}
	material := result.GetDeviceLocalKeyMaterial()
	if material == nil || !bytes.Equal(material.GetClientKeySeed(), []byte{1, 2, 3}) {
		t.Fatal("reset did not return the material actually preserved")
	}
	material.clientKeySeed[0] = 8
	if result.GetDeviceLocalKeyMaterial().GetClientKeySeed()[0] != 1 {
		t.Fatal("mobile mutation altered the accepted reset result")
	}
	fresh := newLocalState(context.Background(), filepath.Dir(fixture.localState.localStorageDir))
	t.Cleanup(fresh.Close)
	if keys, err := fresh.LoadDeviceLocalKeyMaterial(); err != nil || keys == nil || keys.GetClientKeySeed()[0] != 1 {
		t.Fatal("cold state lost preserved identity")
	}
	if location, err := fresh.LoadConnectLocation(); err != nil || location != nil {
		t.Fatal("cold state restored an old account destination")
	}
	if location, err := fresh.LoadDefaultLocation(); err != nil || location != nil {
		t.Fatal("cold state restored an old account default")
	}
	if !testingPairedAuthSnapshot(t, fixture).GetEmpty() {
		t.Fatal("reset retained old auth")
	}
}

func TestPairedResetMissingKeysIsSuccessfulAbsence(t *testing.T) {
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	fixture.api.SetByJwt(fixture.adminJwt)
	result, err := fixture.networkSpace.ResetLocalStateIfCurrent(testingPairedAuthSnapshot(t, fixture))
	if err != nil || result == nil || !result.GetReset() || result.GetDeviceLocalKeyMaterial() != nil {
		t.Fatal("a genuinely absent identity did not survive as absence")
	}
}

// A local-only/wrong pair is a caller error, never a credential comparison.
func TestPairedResetRejectsWrongLocalOnlyAndClosedOrigins(t *testing.T) {
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	paired := testingPairedAuthSnapshot(t, fixture)
	localOnly, err := fixture.localState.GetAuthStateSnapshot()
	if err != nil {
		t.Fatal("could not capture local observation")
	}
	other := testingPairedAuthSpace(t)
	for _, snapshot := range []*LocalAuthStateSnapshot{nil, localOnly, testingPairedAuthSnapshot(t, other)} {
		if result, err := fixture.networkSpace.ResetLocalStateIfCurrent(snapshot); err == nil || result != nil {
			t.Fatal("unpaired snapshot authorized cleanup")
		}
	}
	fixture.api.Close()
	if result, err := fixture.networkSpace.ResetLocalStateIfCurrent(paired); err == nil || result != nil {
		t.Fatal("closed API authorized cleanup")
	}
	if snapshot, err := fixture.networkSpace.GetAuthStateSnapshot(); err == nil || snapshot != nil {
		t.Fatal("closed API produced a usable paired snapshot")
	}
}

// Same bytes are not same ownership. These controls use public API/local
// operations; restoring an envelope on disk also exercises its full equality.
func TestPairedResetRejectsEqualTokenAndApiABA(t *testing.T) {
	for _, aba := range []bool{false, true} {
		fixture := testingPairedAuthSpace(t)
		fixture.seedDistinctLogin(t)
		fixture.api.SetByJwt(fixture.adminJwt)
		snapshot := testingPairedAuthSnapshot(t, fixture)
		if aba {
			fixture.api.SetByJwt(testingJwt(map[string]any{"marker": "intermediate"}))
		}
		fixture.api.SetByJwt(fixture.adminJwt)
		testingRequireSupersededReset(t, fixture, snapshot)
	}
}

func TestPairedResetRejectsEqualLocalLoginAndStorageABA(t *testing.T) {
	for _, aba := range []bool{false, true} {
		fixture := testingPairedAuthSpace(t)
		fixture.seedDistinctLogin(t)
		snapshot := testingPairedAuthSnapshot(t, fixture)
		path := fixture.localState.authStatePath()
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal("could not capture durable envelope")
		}
		if aba {
			if err := fixture.localState.SetByJwt(testingJwt(map[string]any{"marker": "intermediate"})); err != nil {
				t.Fatal("intermediate login failed")
			}
		}
		if err := fixture.localState.SetByJwt(fixture.adminJwt); err != nil {
			t.Fatal("equal login failed")
		}
		// Deliberately restore equal full bytes while retaining the real public
		// mutation's owner epoch. The epoch must still reject this ABA.
		if err := os.WriteFile(path, before, LocalStorageFilePermissions); err != nil {
			t.Fatal("could not restore equal envelope")
		}
		testingRequireSupersededReset(t, fixture, snapshot)
	}
}

func TestPairedResetRejectsFullEnvelopeChangeWithoutLocalEpoch(t *testing.T) {
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	snapshot := testingPairedAuthSnapshot(t, fixture)
	external := newLocalState(context.Background(), filepath.Dir(fixture.localState.localStorageDir))
	t.Cleanup(external.Close)
	if err := external.SetByJwt(testingJwt(map[string]any{"network_name": "different-account"})); err != nil {
		t.Fatal("could not write changed full envelope")
	}
	testingRequireSupersededReset(t, fixture, snapshot)
}

func TestPairedResetRejectsReplacementConstructorWithEqualCredentials(t *testing.T) {
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	fixture.startLocal(t)
	snapshot := testingPairedAuthSnapshot(t, fixture)
	// A remote constructor claims the SAME durable client/instance and API,
	// providing a different real owner without any token-byte discriminator.
	replacement := *fixture
	replacement.startRemote(t)
	testingRequireSupersededReset(t, fixture, snapshot)
}

// The API has already rotated, while the actual admitted device callback has
// not written storage. Both a prior snapshot and a newly captured one refuse.
func testingPairedResetApiAhead(t *testing.T, snapshotBefore bool, remote bool) {
	t.Helper()
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	if remote {
		fixture.startRemote(t)
	} else {
		fixture.startLocal(t)
	}
	testingSeedPreservedState(t, fixture)
	var snapshot *LocalAuthStateSnapshot
	if snapshotBefore {
		snapshot = testingPairedAuthSnapshot(t, fixture)
	}
	gate := fixture.localState.deviceAuthOwner
	entered, resume := testingHoldAuthPublication(t, gate)
	refreshed := testingRefreshableJwtWithMarker(t, "paired-ahead")
	done := make(chan struct{})
	accepted := false
	go func() { defer close(done); accepted = fixture.api.setRefreshedByJwt(fixture.initialJwt, refreshed) }()
	testingAwaitAuthBoundary(t, entered)
	if fixture.api.GetByJwt() != refreshed || fixture.localState.GetByClientJwt() != fixture.initialJwt {
		t.Fatal("test did not hold a real API-ahead-of-storage publication")
	}
	if !snapshotBefore {
		snapshot = testingPairedAuthSnapshot(t, fixture)
	}
	testingRequireSupersededReset(t, fixture, snapshot)
	if location, err := snapshot.LoadConnectLocation(); err == nil || location != nil {
		t.Fatal("unsettled snapshot restored a destination")
	}
	resume()
	testingAwaitAuthBoundary(t, done)
	if !accepted || fixture.localState.GetByClientJwt() != refreshed || fixture.deviceJwt() != refreshed {
		t.Fatal("refused cleanup prevented the real current refresh from finishing")
	}
	current := testingPairedAuthSnapshot(t, fixture)
	if location, err := current.LoadConnectLocation(); err != nil || location == nil || location.Name != "old-private-peer" {
		t.Fatal("same-owner renewal lost the authenticated destination")
	}
	if location, err := current.LoadDefaultLocation(); err != nil || location == nil || location.Name != "old-default" {
		t.Fatal("same-owner renewal lost the default")
	}
}

func TestPairedResetRefusesLocalRefreshAfterSnapshot(t *testing.T) {
	testingPairedResetApiAhead(t, true, false)
}
func TestPairedResetRefusesLocalRefreshBeforeSnapshot(t *testing.T) {
	testingPairedResetApiAhead(t, false, false)
}
func TestPairedResetRefusesRemoteRefreshAfterSnapshot(t *testing.T) {
	testingPairedResetApiAhead(t, true, true)
}
func TestPairedResetRefusesRemoteRefreshBeforeSnapshot(t *testing.T) {
	testingPairedResetApiAhead(t, false, true)
}

// Taking a snapshot does not allow a writer between envelope read and owner
// capture: both existing auth locks are positively observed held at the seam.
func TestPairedAuthSnapshotSerializesReadWithApiAndLocalWriters(t *testing.T) {
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	fixture.api.SetByJwt(fixture.adminJwt)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	resume := func() { once.Do(func() { close(release) }) }
	t.Cleanup(resume)
	fixture.localState.testingAfterAuthSnapshotRead = func() { close(entered); <-release }
	snapshotDone := make(chan struct{})
	var snapshot *LocalAuthStateSnapshot
	var snapshotErr error
	go func() { defer close(snapshotDone); snapshot, snapshotErr = fixture.networkSpace.GetAuthStateSnapshot() }()
	testingAwaitAuthBoundary(t, entered)
	if fixture.api.authMutationLock.TryLock() {
		fixture.api.authMutationLock.Unlock()
		t.Fatal("snapshot released API ownership during its storage read")
	}
	if fixture.localState.authStateLock.TryLock() {
		fixture.localState.authStateLock.Unlock()
		t.Fatal("snapshot released local ownership before capturing its envelope")
	}
	// The short API mutex, unlike the two owner locks, must not cover I/O.
	if !fixture.api.mutex.TryLock() {
		t.Fatal("snapshot held short API mutex over storage I/O")
	}
	fixture.api.mutex.Unlock()
	writerDone := make(chan struct{})
	go func() { defer close(writerDone); fixture.api.SetByJwt(fixture.adminJwt) }()
	resume()
	testingAwaitAuthBoundary(t, snapshotDone)
	testingAwaitAuthBoundary(t, writerDone)
	fixture.localState.testingAfterAuthSnapshotRead = nil
	if snapshotErr != nil || snapshot == nil {
		t.Fatal("coherent snapshot failed")
	}
	testingRequireSupersededReset(t, fixture, snapshot)
}

func TestPairedSnapshotRejectsClosedStorageAndNetworkSpace(t *testing.T) {
	for _, closeStorage := range []bool{false, true} {
		fixture := testingPairedAuthSpace(t)
		fixture.seedDistinctLogin(t)
		snapshot := testingPairedAuthSnapshot(t, fixture)
		if closeStorage {
			fixture.localState.Close()
		} else {
			fixture.networkSpace.cancel()
		}
		if result, err := fixture.networkSpace.ResetLocalStateIfCurrent(snapshot); err == nil || result != nil {
			t.Fatal("closed origin authorized reset")
		}
		if location, err := snapshot.LoadConnectLocation(); err == nil || location != nil || err.Error() == localAuthSnapshotSupersededMessage {
			t.Fatal("closed origin became a retryable location observation")
		}
		if err := snapshot.SetDefaultLocation(testingStoredLocation("closed")); err == nil || err.Error() == localAuthSnapshotSupersededMessage {
			t.Fatal("closed origin authorized a location write")
		}
	}
}

// Pending rejection is not an ordinary blank/unowned API. The real captured
// callback, not the cleanup caller, still owns its policy and durable action.
func TestPairedResetRefusesInFlightApiRejection(t *testing.T) {
	for _, snapshotBefore := range []bool{false, true} {
		fixture := testingPairedAuthSpace(t)
		fixture.seedDistinctLogin(t)
		fixture.startLocal(t)
		testingSeedPreservedState(t, fixture)
		var snapshot *LocalAuthStateSnapshot
		if snapshotBefore {
			snapshot = testingPairedAuthSnapshot(t, fixture)
		}
		entered, resume := testingHoldAuthPublication(t, fixture.localDevice.authPublication)
		done := make(chan struct{})
		accepted := false
		go func() { defer close(done); accepted = fixture.api.rejectByJwt(fixture.initialJwt) }()
		testingAwaitAuthBoundary(t, entered)
		if !snapshotBefore {
			snapshot = testingPairedAuthSnapshot(t, fixture)
		}
		testingRequireSupersededReset(t, fixture, snapshot)
		resume()
		testingAwaitAuthBoundary(t, done)
		if !accepted {
			t.Fatal("existing current rejection policy failed to finish")
		}
	}
}
