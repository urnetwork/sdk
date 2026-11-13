// Explicit equal-byte login and logout supersede prepared constructors even
// when neither a pointer comparison nor durable JWT-byte comparison changes.
package sdk

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Changes committed authority after preparation and before publication.
func testingPreparedConstructorExplicitMutation(t *testing.T, remote bool, mutation string) {
	t.Helper()
	fixture := testingAuthClientShapeSpace(t)
	fixture.initialJwt = testingStartupClientJwt(t, "prepared-explicit-boundary", nil)
	fixture.seedDistinctLogin(t)
	fixture.api.SetByJwt(fixture.initialJwt)
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	resume := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(resume)
	fixture.localState.testingAfterDeviceAuthPrepare = func(*deviceAuthPublicationGate) {
		close(entered)
		<-release
	}
	done := make(chan testingAuthConstructorResult, 1)
	go func() { done <- testingBuildReservedDevice(fixture, remote) }()
	testingAwaitAuthBoundary(t, entered)
	switch mutation {
	case "equal-admin":
		if err := fixture.localState.SetByJwt(fixture.adminJwt); err != nil {
			t.Fatal(err)
		}
	case "equal-client":
		if err := fixture.localState.SetByClientJwt(fixture.initialJwt); err != nil {
			t.Fatal(err)
		}
	case "equal-api":
		fixture.api.SetByJwt(fixture.initialJwt)
	case "logout":
		if err := fixture.localState.Logout(); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("unsupported explicit auth mutation")
	}
	committed, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	resume()
	var built testingAuthConstructorResult
	select {
	case built = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("prepared constructor did not finish after explicit auth mutation")
	}
	if built.local != nil {
		_ = built.local.CloseAndWait(context.Background())
		t.Error("superseded local constructor escaped")
	}
	if built.remote != nil {
		_ = built.remote.CloseAndWait(context.Background())
		t.Error("superseded remote constructor escaped")
	}
	after, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if built.err == nil || after != committed || fixture.api.GetByJwt() != fixture.initialJwt {
		t.Error("prepared constructor overwrote explicit login/logout authority")
	}
	if len(fixture.api.jwtRefreshListeners.Get()) != 0 || len(fixture.api.authLogoutListeners.Get()) != 0 {
		t.Error("superseded constructor retained API callbacks")
	}
}

// The initial store had no device owner; an equal admin write still matters.
func TestPreparedLocalConstructorCannotUndoEqualAdminLogin(t *testing.T) {
	testingPreparedConstructorExplicitMutation(t, false, "equal-admin")
}

// Completing an equal-byte client login also supersedes initial preparation.
func TestPreparedRemoteConstructorCannotUndoEqualClientLogin(t *testing.T) {
	testingPreparedConstructorExplicitMutation(t, true, "equal-client")
}

// API equality cannot bypass the constructor's version check.
func TestPreparedLocalConstructorCannotUndoEqualApiLogin(t *testing.T) {
	testingPreparedConstructorExplicitMutation(t, false, "equal-api")
}

// Remote request hooks must not be installed after an explicit API login.
func TestPreparedRemoteConstructorCannotUndoEqualApiLogin(t *testing.T) {
	testingPreparedConstructorExplicitMutation(t, true, "equal-api")
}

// Logout must not resurrect a prepared local credential or instance.
func TestPreparedLocalConstructorCannotUndoLogout(t *testing.T) {
	testingPreparedConstructorExplicitMutation(t, false, "logout")
}

// The remote constructor has the same destructive-boundary protection.
func TestPreparedRemoteConstructorCannotUndoLogout(t *testing.T) {
	testingPreparedConstructorExplicitMutation(t, true, "logout")
}

// Reading a provider credential must not expose an admin temporarily installed
// on its shared API by relogin.
func TestDeviceLocalClientJwtDoesNotReadAdminApiLogin(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.seedDistinctLogin(t)
	fixture.startLocal(t)
	fixture.api.SetByJwt(fixture.adminJwt)
	if fixture.localDevice.GetClientJwt() != fixture.initialJwt ||
		fixture.api.GetByJwt() != fixture.adminJwt {
		t.Error("device client getter confused provider and admin API credentials")
	}
}
