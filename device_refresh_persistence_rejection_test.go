// Real refresh responses and constructor-installed callbacks exercise a
// storage failure followed by rejection, without running network workers.
package sdk

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/connect"
)

// The API advances while the actual device and durable envelope retain the
// predecessor. Only the independent scheduled workers are stopped.
type testingRefreshPersistenceRejection struct {
	*testingAuthClientShape
	owner        *deviceAuthPublicationGate
	refreshedJwt string
	logouts      atomic.Int64
}

// Forces a real read or write failure at the device callback, restores the
// same storage, and requires the exact split state that previously lost logout.
func testingFailedDeviceRefreshPersistence(t *testing.T, remote bool, failure string) *testingRefreshPersistenceRejection {
	t.Helper()
	shape := testingAuthClientShapeSpace(t)
	// No admin has been installed, so this worker has not started. Cancel its
	// independent test context before construction can arm scheduled renewal.
	renewCtx, cancelRenew := context.WithCancel(shape.api.ctx)
	cancelRenew()
	shape.api.networkRenewer.ctx = renewCtx
	shape.seedDistinctLogin(t)
	fixture := &testingRefreshPersistenceRejection{
		testingAuthClientShape: shape,
		refreshedJwt:           testingRefreshableJwtWithMarker(t, "synthetic-unpersisted-refresh"),
	}
	var logoutSub Sub
	if remote {
		shape.startRemote(t)
		fixture.owner = shape.remoteDevice.authPublication
		logoutSub = shape.remoteDevice.AddAuthLogoutListener(authLogoutListenerFunc(func() { fixture.logouts.Add(1) }))
	} else {
		shape.startLocal(t)
		fixture.owner = shape.localDevice.authPublication
		logoutSub = shape.localDevice.AddAuthLogoutListener(authLogoutListenerFunc(func() { fixture.logouts.Add(1) }))
	}
	t.Cleanup(logoutSub.Close)
	testingAwaitAuthBoundary(t, shape.api.networkRenewer.doneChannel())
	if err := shape.localState.SetBlockerEnabled(true); err != nil {
		t.Fatal(err)
	}
	before, err := shape.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	var published atomic.Int64
	refreshSub := shape.addRefresh(jwtRefreshListenerFunc(func(string) { published.Add(1) }))
	t.Cleanup(refreshSub.Close)
	raw, err := json.Marshal(&RefreshJwtResult{ByJwt: fixture.refreshedJwt})
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	shape.api.setHttpGetRaw(func(ctx context.Context, requestUrl string, byJwt string) ([]byte, error) {
		requests += 1
		if ctx.Err() != nil || !strings.HasSuffix(requestUrl, "/auth/refresh") || byJwt != shape.initialJwt {
			t.Error("refresh did not use the live request context, route, and original credential")
		}
		return raw, nil
	})

	var restore func()
	switch failure {
	case "read":
		path := shape.localState.authStatePath()
		original, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, LocalStorageDirectoryPermissions); err != nil {
			t.Fatal(err)
		}
		restore = func() {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, original, LocalStorageFilePermissions); err != nil {
				t.Fatal(err)
			}
		}
	case "write":
		if err := os.Chmod(shape.localState.localStorageDir, 0500); err != nil {
			t.Fatal(err)
		}
		restore = func() {
			if err := os.Chmod(shape.localState.localStorageDir, LocalStorageDirectoryPermissions); err != nil {
				t.Fatal(err)
			}
		}
	default:
		t.Fatal("unsupported persistence failure")
	}
	var restoreOnce sync.Once
	restoreStorage := func() { restoreOnce.Do(restore) }
	t.Cleanup(restoreStorage)
	outcome := shape.api.tokenManager.refreshTokenWithContext(shape.api.ctx, shape.initialJwt)
	restoreStorage()
	if requests != 1 || outcome.err != nil || outcome.loggedOut || outcome.stale {
		t.Fatal("valid real refresh did not install its API result")
	}
	after, err := shape.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if after != before || shape.api.GetByJwt() != fixture.refreshedJwt ||
		shape.deviceJwt() != shape.initialJwt || published.Load() != 0 || fixture.logouts.Load() != 0 {
		t.Fatal("storage failure did not retain the original device and durable credential beside the refreshed API")
	}
	return fixture
}

// The manager consumes a real confirmed refresh refusal. Returning from it is
// the positive completion boundary for all synchronous device callbacks.
func (self *testingRefreshPersistenceRejection) reject(t *testing.T) apiTokenRefreshOutcome {
	t.Helper()
	self.api.setHttpGetRaw(func(ctx context.Context, requestUrl string, byJwt string) ([]byte, error) {
		if ctx.Err() != nil || !strings.HasSuffix(requestUrl, "/auth/refresh") || byJwt != self.refreshedJwt {
			t.Error("rejection did not reach the currently refreshed credential")
		}
		return nil, &connect.HttpStatusError{StatusCode: http.StatusUnauthorized, Status: "401 Unauthorized"}
	})
	return self.api.tokenManager.refreshTokenWithContext(self.api.ctx, self.refreshedJwt)
}

// The API refusal must reach the app exactly once and remove this sign-in,
// including routing preferences, despite the earlier refresh storage failure.
func (self *testingRefreshPersistenceRejection) requireLogout(t *testing.T) {
	t.Helper()
	outcome := self.reject(t)
	if outcome.err != nil || outcome.stale || !outcome.loggedOut {
		t.Fatal("current refresh rejection was not committed")
	}
	state, err := self.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if self.api.GetByJwt() != "" || self.deviceJwt() != "" || self.logouts.Load() != 1 ||
		state.ByJwt != "" || state.ByClientJwt != "" || state.InstanceId != "" || self.localState.GetBlockerEnabled() {
		t.Error("current rejection after failed refresh persistence did not clear owned auth and notify the app once")
	}
}

// A temporary read error must not permanently suppress a later real refusal.
func TestLocalRefreshReadFailureDoesNotSuppressCurrentRejection(t *testing.T) {
	testingFailedDeviceRefreshPersistence(t, false, "read").requireLogout(t)
}

// The remote's constructor-installed listener has the same persistence gap.
func TestRemoteRefreshReadFailureDoesNotSuppressCurrentRejection(t *testing.T) {
	testingFailedDeviceRefreshPersistence(t, true, "read").requireLogout(t)
}

// A readable old envelope survives a failed atomic replacement, then logs out.
func TestLocalRefreshWriteFailureDoesNotSuppressCurrentRejection(t *testing.T) {
	testingFailedDeviceRefreshPersistence(t, false, "write").requireLogout(t)
}

// Write failure on a remote must also leave a later refusal with an app owner.
func TestRemoteRefreshWriteFailureDoesNotSuppressCurrentRejection(t *testing.T) {
	testingFailedDeviceRefreshPersistence(t, true, "write").requireLogout(t)
}

// The original rejected token remains eligible when storage has advanced
// through the real CAS before the device has published that refresh.
func TestRefreshRejectionAcceptsDurableTokenAheadOfDevice(t *testing.T) {
	for _, remote := range []bool{false, true} {
		fixture := testingFailedDeviceRefreshPersistence(t, remote, "read")
		accepted, err := fixture.localState.replaceOwnedClientJwt(
			fixture.initialJwt, fixture.refreshedJwt, fixture.instanceId, fixture.owner,
		)
		if err != nil || !accepted || fixture.deviceJwt() != fixture.initialJwt {
			t.Fatal("durable-ahead control did not retain the original device publication")
		}
		fixture.requireLogout(t)
	}
}

// Hold the actual refusal after its API snapshot, then replace the state the
// delayed callback would otherwise erase using the added predecessor match.
func testingRefreshPersistenceRejectionReplacement(t *testing.T, remote bool, replacement string) {
	t.Helper()
	fixture := testingFailedDeviceRefreshPersistence(t, remote, "read")
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	resume := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(resume)
	fixture.owner.testingBeforePersistence = func() {
		close(entered)
		<-release
	}
	done := make(chan struct{})
	var outcome apiTokenRefreshOutcome
	go func() {
		defer close(done)
		outcome = fixture.reject(t)
	}()
	testingAwaitAuthBoundary(t, entered)
	expectedDeviceJwt := fixture.initialJwt
	var replacementDevice *testingAuthClientShape
	switch replacement {
	case "equal-admin-login":
		if err := fixture.localState.SetByJwt(fixture.adminJwt); err != nil {
			t.Fatal(err)
		}
	case "equal-client-login":
		if err := fixture.localState.SetByClientJwt(fixture.initialJwt); err != nil {
			t.Fatal(err)
		}
	case "instance":
		if err := fixture.localState.SetInstanceId(NewId()); err != nil {
			t.Fatal(err)
		}
	case "missing-instance":
		// Clearing only the instance keeps the owner: isolate the instance CAS.
		if err := fixture.localState.SetInstanceId(nil); err != nil {
			t.Fatal(err)
		}
	case "owner":
		testingClaimDurableAuthOwner(fixture.localState, newDeviceAuthPublicationGate())
	case "constructor":
		replacementDevice = &testingAuthClientShape{
			networkSpace: fixture.networkSpace, localState: fixture.localState,
			api: fixture.api, adminJwt: fixture.adminJwt, instanceId: NewId(),
			initialJwt: testingRefreshableJwtWithMarker(t, "synthetic-replacement-device"),
		}
		if remote {
			replacementDevice.startRemote(t)
		} else {
			replacementDevice.startLocal(t)
		}
	case "device-original", "device-rejected", "device-new":
		if remote {
			t.Fatal("explicit client setter belongs to the local device")
		}
		nextJwt := fixture.initialJwt
		if replacement == "device-rejected" {
			nextJwt = fixture.refreshedJwt
		} else if replacement == "device-new" {
			nextJwt = testingRefreshableJwtWithMarker(t, "synthetic-replacement-setter")
		}
		fixture.localDevice.SetByJwt(nextJwt)
		expectedDeviceJwt = nextJwt
		if fixture.api.GetByJwt() != nextJwt || fixture.deviceJwt() != nextJwt {
			t.Fatal("explicit device setter did not publish its replacement")
		}
	case "api-login":
		fixture.api.SetByJwt(fixture.refreshedJwt)
	default:
		t.Fatal("unsupported replacement control")
	}
	before, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	apiBefore := fixture.api.GetByJwt()
	resume()
	testingAwaitAuthBoundary(t, done)
	after, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if outcome.err != nil || outcome.stale || !outcome.loggedOut {
		t.Fatal("control did not reach the old real API rejection")
	}
	if after != before || fixture.deviceJwt() != expectedDeviceJwt || fixture.api.GetByJwt() != apiBefore ||
		fixture.logouts.Load() != 0 || !fixture.localState.GetBlockerEnabled() {
		t.Errorf("delayed rejection crossed replacement boundary %q (remote=%t)", replacement, remote)
	}
	if replacementDevice != nil && replacementDevice.deviceJwt() != replacementDevice.initialJwt {
		t.Error("old callback altered the constructed replacement")
	}
}

// The fallback cannot erase a newer durable login, instance, or constructor.
func TestLocalUnpersistedRefreshRejectionPreservesReplacements(t *testing.T) {
	for _, replacement := range []string{"equal-admin-login", "equal-client-login", "instance", "missing-instance", "owner", "constructor", "api-login"} {
		testingRefreshPersistenceRejectionReplacement(t, false, replacement)
	}
}

// The same replacement controls cover the remote's real callback wiring.
func TestRemoteUnpersistedRefreshRejectionPreservesReplacements(t *testing.T) {
	for _, replacement := range []string{"equal-admin-login", "equal-client-login", "instance", "missing-instance", "owner", "constructor", "api-login"} {
		testingRefreshPersistenceRejectionReplacement(t, true, replacement)
	}
}

// An explicit setter keeps the device owner, so the API generation fence is
// required even when it installs either of the two eligible token byte strings.
func TestUnpersistedRefreshRejectionPreservesExplicitDeviceSetter(t *testing.T) {
	for _, replacement := range []string{"device-original", "device-rejected", "device-new"} {
		testingRefreshPersistenceRejectionReplacement(t, false, replacement)
	}
}
