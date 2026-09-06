// Actual device location listeners are paused before persistence; no detached
// boolean predicate substitutes for the SDK callback or durable write.
package sdk

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

type testingLocationOwnerListener func(*ConnectLocation)

func (self testingLocationOwnerListener) ConnectLocationChanged(location *ConnectLocation) {
	self(location)
}
func (self testingLocationOwnerListener) DefaultLocationChanged(location *ConnectLocation) {
	self(location)
}

func testingLateLocationOwnerCallback(t *testing.T, defaultLocation bool) {
	t.Helper()
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	fixture.startLocal(t)
	testingSeedPreservedState(t, fixture)
	snapshot := testingPairedAuthSnapshot(t, fixture)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	resume := func() { once.Do(func() { close(release) }) }
	t.Cleanup(resume)
	var writeErr error
	listener := testingLocationOwnerListener(func(location *ConnectLocation) {
		close(entered)
		<-release
		if defaultLocation {
			writeErr = snapshot.SetDefaultLocation(location)
		} else {
			writeErr = snapshot.SetConnectLocation(location)
		}
	})
	var subscription Sub
	if defaultLocation {
		subscription = fixture.localDevice.AddDefaultLocationChangeListener(listener)
	} else {
		subscription = fixture.localDevice.AddConnectLocationChangeListener(listener)
	}
	t.Cleanup(subscription.Close)
	done := make(chan struct{})
	go func() {
		defer close(done)
		location := testingStoredLocation("late-old-account-callback")
		if defaultLocation {
			fixture.localDevice.SetDefaultLocation(location)
		} else {
			// Explicit nil specs exercises the actual destination notification
			// without asking the network to construct provider candidates.
			fixture.localDevice.SetDestination(location, nil)
		}
	}()
	testingAwaitAuthBoundary(t, entered)
	result, err := fixture.networkSpace.ResetLocalStateIfCurrent(snapshot)
	if err != nil || result == nil || !result.GetReset() {
		t.Fatal("current old account failed to clean up")
	}
	// Replace the account before releasing the old native-style listener.
	if err := fixture.localState.SetByJwt(testingJwt(map[string]any{"network_name": "new-account"})); err != nil {
		t.Fatal("replacement admin could not persist")
	}
	fixture.initialJwt = testingJwt(map[string]any{
		"client_id": "00000000-0000-0000-0000-000000000021", "device_id": "00000000-0000-0000-0000-000000000022",
	})
	if err := fixture.localState.SetByClientJwt(fixture.initialJwt); err != nil {
		t.Fatal("replacement client could not persist")
	}
	fixture.api.SetByJwt(fixture.initialJwt)
	current := testingPairedAuthSnapshot(t, fixture)
	if err := current.SetConnectLocation(testingStoredLocation("new-account-destination")); err != nil {
		t.Fatal("current destination could not persist")
	}
	if err := current.SetDefaultLocation(testingStoredLocation("new-account-default")); err != nil {
		t.Fatal("current default could not persist")
	}
	resume()
	testingAwaitAuthBoundary(t, done)
	if writeErr == nil || writeErr.Error() != localAuthSnapshotSupersededMessage {
		t.Fatal("late old-owner callback was not explicitly superseded")
	}
	fresh := newLocalState(context.Background(), filepath.Dir(fixture.localState.localStorageDir))
	t.Cleanup(fresh.Close)
	if location, err := fresh.LoadConnectLocation(); err != nil || location == nil || location.Name != "new-account-destination" {
		t.Fatal("old callback replaced new account's durable destination")
	}
	if location, err := fresh.LoadDefaultLocation(); err != nil || location == nil || location.Name != "new-account-default" {
		t.Fatal("old callback replaced new account's durable default")
	}
}

func TestSnapshotLocationWriteRejectsPausedOldDeviceCallbackAfterReset(t *testing.T) {
	testingLateLocationOwnerCallback(t, false)
}
func TestSnapshotDefaultWriteRejectsPausedOldDeviceCallbackAfterReset(t *testing.T) {
	testingLateLocationOwnerCallback(t, true)
}

// A legitimate same-owner renewal supersedes an observation, not the saved
// intent. A bounded recapture by the still-current native owner can persist it.
func TestSnapshotLocationWriteSameOwnerRenewalAndExplicitDisconnectHealthy(t *testing.T) {
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	fixture.startLocal(t)
	testingSeedPreservedState(t, fixture)
	old := testingPairedAuthSnapshot(t, fixture)
	refreshed := testingRefreshableJwtWithMarker(t, "location-renewal")
	if !fixture.api.setRefreshedByJwt(fixture.initialJwt, refreshed) {
		t.Fatal("same-owner renewal failed")
	}
	if err := old.SetConnectLocation(nil); err == nil || err.Error() != localAuthSnapshotSupersededMessage {
		t.Fatal("stale observation removed the renewed owner's destination")
	}
	current := testingPairedAuthSnapshot(t, fixture)
	if current.GetByJwt() != fixture.adminJwt || current.GetByClientJwt() != refreshed || current.GetInstanceId().Cmp(fixture.instanceId) != 0 {
		t.Fatal("same-owner renewal changed admin or stable instance")
	}
	if location, err := current.LoadConnectLocation(); err != nil || location == nil || location.Name != "old-private-peer" {
		t.Fatal("renewal lost saved intent")
	}
	if err := current.SetConnectLocation(testingStoredLocation("accepted-current-choice")); err != nil {
		t.Fatal("current recapture could not persist selected destination")
	}
	if err := current.SetDefaultLocation(testingStoredLocation("accepted-current-default")); err != nil {
		t.Fatal("current recapture could not persist default")
	}
	if err := current.SetConnectLocation(nil); err != nil {
		t.Fatal("current explicit disconnect failed")
	}
	if location, err := current.LoadConnectLocation(); err != nil || location != nil {
		t.Fatal("explicit disconnect did not survive checked read")
	}
	if location, err := current.LoadDefaultLocation(); err != nil || location == nil || location.Name != "accepted-current-default" {
		t.Fatal("disconnect erased independent default selection")
	}
}

// Hold the actual disk commit while a reset queues. Positive lock probes prove
// neither auth writer can overtake the accepted location transaction.
func TestSnapshotLocationCommitSerializesWithPairedReset(t *testing.T) {
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	snapshot := testingPairedAuthSnapshot(t, fixture)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	resume := func() { once.Do(func() { close(release) }) }
	t.Cleanup(resume)
	fixture.localState.testingBeforeLocationCommit = func(string) error { close(entered); <-release; return nil }
	writeDone := make(chan struct{})
	var writeErr error
	go func() {
		defer close(writeDone)
		writeErr = snapshot.SetConnectLocation(testingStoredLocation("serialized-old-owner"))
	}()
	testingAwaitAuthBoundary(t, entered)
	if fixture.api.authMutationLock.TryLock() {
		fixture.api.authMutationLock.Unlock()
		t.Fatal("conditional disk writer released API ownership")
	}
	if fixture.localState.authStateLock.TryLock() {
		fixture.localState.authStateLock.Unlock()
		t.Fatal("conditional disk writer released local ownership")
	}
	if !fixture.api.mutex.TryLock() {
		t.Fatal("location disk commit held short API mutex")
	}
	fixture.api.mutex.Unlock()
	resetDone := make(chan struct{})
	var result *LocalStateResetResult
	var resetErr error
	go func() {
		defer close(resetDone)
		result, resetErr = fixture.networkSpace.ResetLocalStateIfCurrent(snapshot)
	}()
	resume()
	testingAwaitAuthBoundary(t, writeDone)
	testingAwaitAuthBoundary(t, resetDone)
	if writeErr != nil || resetErr != nil || result == nil || !result.GetReset() {
		t.Fatal("serialized write/reset failed")
	}
	if location, err := fixture.localState.LoadConnectLocation(); err != nil || location != nil {
		t.Fatal("ordered reset retained old owner's accepted write")
	}
}
