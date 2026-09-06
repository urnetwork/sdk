// Deterministic admission barriers exercise the real constructor-installed
// callback and the same auth-lock claim used by replacement constructors.
package sdk

import (
	"sync"
	"testing"
	"time"
)

// Holds a registered lease outside all state locks, without scheduler polling.
func testingHoldAuthPublication(t *testing.T, gate *deviceAuthPublicationGate) (<-chan struct{}, func()) {
	t.Helper()
	entered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce sync.Once
	var releaseOnce sync.Once
	resume := func() { releaseOnce.Do(func() { close(release) }) }
	gate.testingAfterAdmission = func() {
		enterOnce.Do(func() { close(entered) })
		<-release
	}
	t.Cleanup(resume)
	return entered, resume
}

// Both the blocked event and completion have positive synchronization edges;
// the timeout is only a deadlock bound.
func testingAwaitAuthBoundary(t *testing.T, boundary <-chan struct{}) {
	t.Helper()
	select {
	case <-boundary:
	case <-time.After(5 * time.Second):
		t.Fatal("auth ownership boundary did not complete")
	}
}

// The mutation runs while the old real callback owns an admitted lease. The
// API still belongs to the old device, isolating the LocalState owner check.
func testingAdmittedLogoutOwner(t *testing.T, remote bool, mutation string) {
	t.Helper()
	fixture := testingAuthClientShapeSpace(t)
	fixture.seedDistinctLogin(t)
	var gate *deviceAuthPublicationGate
	if remote {
		fixture.startRemote(t)
		gate = fixture.remoteDevice.authPublication
	} else {
		fixture.startLocal(t)
		gate = fixture.localDevice.authPublication
	}
	if err := fixture.localState.SetBlockerEnabled(true); err != nil {
		t.Fatal(err)
	}
	entered, resume := testingHoldAuthPublication(t, gate)
	done := make(chan struct{})
	accepted := false
	go func() {
		defer close(done)
		accepted = fixture.api.rejectByJwt(fixture.initialJwt)
	}()
	testingAwaitAuthBoundary(t, entered)
	before, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	switch mutation {
	case "replacement":
		testingClaimDurableAuthOwner(fixture.localState, newDeviceAuthPublicationGate())
	case "equal-admin-login":
		if err := fixture.localState.SetByJwt(fixture.adminJwt); err != nil {
			t.Fatal(err)
		}
	case "equal-client-login":
		if err := fixture.localState.SetByClientJwt(fixture.initialJwt); err != nil {
			t.Fatal(err)
		}
	case "current":
	default:
		t.Fatal("unsupported test ownership transition")
	}
	resume()
	testingAwaitAuthBoundary(t, done)
	if !accepted {
		t.Fatal("real API rejection did not reach its admitted callback")
	}
	after, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if mutation == "current" {
		if after.ByJwt != "" || after.ByClientJwt != "" || after.InstanceId != "" ||
			fixture.localState.GetBlockerEnabled() {
			t.Error("current owner's confirmed rejection did not clear its auth and routing state")
		}
		if fixture.deviceJwt() != "" {
			t.Error("current device did not publish logout")
		}
	} else {
		if after != before || !fixture.localState.GetBlockerEnabled() {
			t.Error("old admitted logout erased the replacement or explicit-login state")
		}
		if fixture.deviceJwt() != fixture.initialJwt {
			t.Error("superseded logout still published a device event")
		}
	}
}

// A constructor claim with identical bytes is still a new publication owner.
func TestAdmittedLocalLogoutCannotEraseReplacementAuthClaim(t *testing.T) {
	testingAdmittedLogoutOwner(t, false, "replacement")
}

// The remote's installed callback has the same persisted ownership boundary.
func TestAdmittedRemoteLogoutCannotEraseReplacementAuthClaim(t *testing.T) {
	testingAdmittedLogoutOwner(t, true, "replacement")
}

// Current-owner rejection remains destructive as required by the existing API.
func TestAdmittedCurrentLocalLogoutClearsItsOwnedAuth(t *testing.T) {
	testingAdmittedLogoutOwner(t, false, "current")
}

// Remote current-owner rejection retains its existing logout behavior.
func TestAdmittedCurrentRemoteLogoutClearsItsOwnedAuth(t *testing.T) {
	testingAdmittedLogoutOwner(t, true, "current")
}

// An explicit login retires old callbacks even if the admin bytes are equal.
func TestAdmittedLogoutCannotUndoEqualAdminLogin(t *testing.T) {
	testingAdmittedLogoutOwner(t, false, "equal-admin-login")
}

// Completing client login retires old callbacks even on a durable no-op.
func TestAdmittedLogoutCannotUndoEqualClientLogin(t *testing.T) {
	testingAdmittedLogoutOwner(t, true, "equal-client-login")
}

// Hold a real refresh after admission, then transfer the same durable client
// to another owner before the old callback can enter its compare-and-swap.
func TestAdmittedClientRefreshCannotOverwriteReplacementAuthClaim(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.seedDistinctLogin(t)
	fixture.startLocal(t)
	entered, resume := testingHoldAuthPublication(t, fixture.localDevice.authPublication)
	done := make(chan struct{})
	nextJwt := testingRefreshableJwtWithMarker(t, "admitted-old-owner-refresh")
	go func() {
		defer close(done)
		fixture.api.setRefreshedByJwt(fixture.initialJwt, nextJwt)
	}()
	testingAwaitAuthBoundary(t, entered)
	before, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	testingClaimDurableAuthOwner(fixture.localState, newDeviceAuthPublicationGate())
	resume()
	testingAwaitAuthBoundary(t, done)
	after, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if after != before || fixture.deviceJwt() != fixture.initialJwt {
		t.Error("admitted old refresh published through the new durable owner")
	}
}

// Explicit client writes revoke an old refresh even if no bytes changed.
func TestAdmittedClientRefreshCannotUndoEqualClientLogin(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.seedDistinctLogin(t)
	fixture.startRemote(t)
	entered, resume := testingHoldAuthPublication(t, fixture.remoteDevice.authPublication)
	done := make(chan struct{})
	nextJwt := testingRefreshableJwtWithMarker(t, "admitted-old-remote-refresh")
	go func() {
		defer close(done)
		fixture.api.setRefreshedByJwt(fixture.initialJwt, nextJwt)
	}()
	testingAwaitAuthBoundary(t, entered)
	if err := fixture.localState.SetByClientJwt(fixture.initialJwt); err != nil {
		t.Fatal(err)
	}
	before, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	resume()
	testingAwaitAuthBoundary(t, done)
	after, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if after != before || fixture.deviceJwt() != fixture.initialJwt {
		t.Error("admitted refresh overrode an equality-no-op explicit client login")
	}
}

// Isolates the final transaction's LocalState owner check from the API check.
// Actual constructor publication order is covered independently; this helper
// does not pretend a speculative constructor is entitled to claim the store.
func testingClaimDurableAuthOwner(localState *LocalState, owner *deviceAuthPublicationGate) {
	localState.authStateLock.Lock()
	defer localState.authStateLock.Unlock()
	localState.deviceAuthOwner = owner
	localState.deviceAuthGeneration += 1
}
