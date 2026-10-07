// This barrier is after the old callback's API owner check, so only the
// atomic LocalState predicate can reject it after the new constructor commits.
package sdk

import (
	"sync"
	"testing"
)

// A successful replacement is constructed while an admitted old rejection
// retains the previously checked token, then the old destructive write resumes.
func testingPrecheckedLogoutAfterConstructor(t *testing.T, remote bool) {
	t.Helper()
	fixture := testingAuthClientShapeSpace(t)
	fixture.initialJwt = testingStartupClientJwt(t, "prechecked-old", nil)
	fixture.seedDistinctLogin(t)
	var gate *deviceAuthPublicationGate
	if remote {
		fixture.startRemote(t)
		gate = fixture.remoteDevice.authPublication
	} else {
		fixture.startLocal(t)
		gate = fixture.localDevice.authPublication
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	resume := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(resume)
	gate.testingBeforePersistence = func() {
		close(entered)
		<-release
	}
	done := make(chan struct{})
	rejected := false
	go func() {
		defer close(done)
		rejected = fixture.api.rejectByJwt(fixture.initialJwt)
	}()
	testingAwaitAuthBoundary(t, entered)
	replacement := &testingAuthClientShape{
		networkSpace: fixture.networkSpace, localState: fixture.localState,
		api: fixture.api, adminJwt: fixture.adminJwt, instanceId: NewId(),
		initialJwt: testingStartupClientJwt(t, "prechecked-replacement", map[string]any{
			"iat": testingClientStartupNow - 50,
		}),
	}
	// A different instance is an explicit replacement proposal; it may commit
	// even though the prior instance's current client was just rejected.
	if remote {
		replacement.startRemote(t)
	} else {
		replacement.startLocal(t)
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
	if !rejected || after != before || fixture.deviceJwt() != fixture.initialJwt ||
		replacement.deviceJwt() != replacement.initialJwt ||
		fixture.api.GetByJwt() != replacement.initialJwt {
		t.Error("prechecked old logout crossed the committed constructor boundary")
	}
	nextJwt := testingStartupClientJwt(t, "prechecked-replacement-refresh", map[string]any{
		"iat": testingClientStartupNow - 10,
	})
	if !fixture.api.setRefreshedByJwt(replacement.initialJwt, nextJwt) {
		t.Fatal("replacement API did not accept its own refresh")
	}
	next, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if next.ByClientJwt != nextJwt || replacement.deviceJwt() != nextJwt ||
		next.ByJwt != fixture.adminJwt || next.InstanceId != replacement.instanceId.String() {
		t.Error("new constructor lost future refresh authority after stale logout returned")
	}
}

// Local destructive cleanup cannot use a prechecked old owner after commit.
func TestPrecheckedLocalLogoutCannotEraseConstructedReplacement(t *testing.T) {
	testingPrecheckedLogoutAfterConstructor(t, false)
}

// The native remote shares the same LocalState transaction predicate.
func TestPrecheckedRemoteLogoutCannotEraseConstructedReplacement(t *testing.T) {
	testingPrecheckedLogoutAfterConstructor(t, true)
}
