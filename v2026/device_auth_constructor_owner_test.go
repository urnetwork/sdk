// Construction can finish out of order; a successful earlier claim is not
// permission to publish after a replacement has claimed and installed the API.
package sdk

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// A partial constructor never escapes through a successful return.
type testingAuthConstructorResult struct {
	local  *DeviceLocal
	remote *DeviceRemote
	err    error
}

// Holds the first actual constructor after its non-mutating preparation, lets the second
// publish fully, then resumes the first. No provider or external RPC is opened.
func testingAuthConstructorPublicationOrder(t *testing.T, remote bool, apiLogin bool) {
	t.Helper()
	fixture := testingAuthClientShapeSpace(t)
	fixture.initialJwt = testingStartupClientJwt(t, "first-constructor", nil)
	fixture.seedDistinctLogin(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	resume := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(resume)
	var claims atomic.Int32
	fixture.localState.testingAfterDeviceAuthPrepare = func(*deviceAuthPublicationGate) {
		if claims.Add(1) == 1 {
			close(entered)
			<-release
		}
	}
	firstDone := make(chan testingAuthConstructorResult, 1)
	go func() {
		if remote {
			settings := defaultDeviceRpcSettings()
			settings.DisableLogging = true
			device, err := newDeviceRemoteWithOverrides(
				fixture.networkSpace, fixture.initialJwt, fixture.instanceId,
				settings, connect.NewId(), alwaysOfflineDeviceRpcDialer{},
			)
			firstDone <- testingAuthConstructorResult{remote: device, err: err}
		} else {
			settings := DefaultDeviceLocalSettings()
			settings.AllowProvider = false
			settings.DisableLogging = true
			device, err := newDeviceLocalWithOverrides(
				fixture.networkSpace, fixture.initialJwt, "held-constructor", "test", "0.0.0",
				fixture.instanceId, settings, connect.NewId(),
			)
			firstDone <- testingAuthConstructorResult{local: device, err: err}
		}
	}()
	testingAwaitAuthBoundary(t, entered)
	var replacement *testingAuthClientShape
	if apiLogin {
		fixture.api.SetByJwt(fixture.adminJwt)
	} else {
		replacement = &testingAuthClientShape{
			networkSpace: fixture.networkSpace, localState: fixture.localState,
			initialJwt: testingStartupClientJwt(t, "replacement-constructor", map[string]any{
				"iat": testingClientStartupNow - 10,
			}),
			adminJwt: fixture.adminJwt, instanceId: fixture.instanceId, api: fixture.api,
		}
		if remote {
			replacement.startRemote(t)
		} else {
			replacement.startLocal(t)
		}
	}
	resume()
	var first testingAuthConstructorResult
	select {
	case first = <-firstDone:
	case <-time.After(5 * time.Second):
		t.Fatal("overtaken constructor did not finish its cleanup")
	}
	if first.local != nil {
		_ = first.local.CloseAndWait(context.Background())
		t.Error("overtaken local constructor returned a live device")
	}
	if first.remote != nil {
		_ = first.remote.CloseAndWait(context.Background())
		t.Error("overtaken remote constructor returned a live device")
	}
	if first.err == nil {
		t.Error("overtaken constructor did not report failed publication")
	}
	state, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if apiLogin {
		if fixture.api.GetByJwt() != fixture.adminJwt || state.ByJwt != fixture.adminJwt ||
			state.ByClientJwt != fixture.initialJwt || len(fixture.api.jwtRefreshListeners.Get()) != 0 ||
			len(fixture.api.authLogoutListeners.Get()) != 0 {
			t.Error("overtaken constructor overrode explicit admin API login or retained listeners")
		}
		return
	}
	if state.ByJwt != fixture.adminJwt || state.ByClientJwt != replacement.initialJwt ||
		state.InstanceId != fixture.instanceId.String() ||
		fixture.api.GetByJwt() != replacement.initialJwt || replacement.deviceJwt() != replacement.initialJwt {
		t.Error("late first-constructor publication split or replaced the new auth owner")
	}
	if len(fixture.api.jwtRefreshListeners.Get()) != 1 || len(fixture.api.authLogoutListeners.Get()) != 1 {
		t.Error("overtaken constructor did not detach only its own API listeners")
	}
	if remote {
		fixture.api.mutex.Lock()
		postOwner := fixture.api.httpPostRawOwner
		getOwner := fixture.api.httpGetRawOwner
		fixture.api.mutex.Unlock()
		if postOwner != replacement.remoteDevice.authPublication ||
			getOwner != replacement.remoteDevice.authPublication {
			t.Error("overtaken remote replaced or cleared the winner's HTTP bindings")
		}
	}
}

// The older Local constructor cannot reclaim API publication after a new claim.
func TestDeviceLocalOvertakenConstructorCannotPublishOldAuth(t *testing.T) {
	testingAuthConstructorPublicationOrder(t, false, false)
}

// The older Remote constructor also cannot reclaim any of the HTTP bindings.
func TestDeviceRemoteOvertakenConstructorCannotPublishOldAuth(t *testing.T) {
	testingAuthConstructorPublicationOrder(t, true, false)
}

// Explicit API login also supersedes a held local constructor before the next
// device exists; storage-only owner checks cannot establish this boundary.
func TestDeviceLocalHeldConstructorCannotReplaceAdminApiLogin(t *testing.T) {
	testingAuthConstructorPublicationOrder(t, false, true)
}

// Remote construction must not replace the new admin or reinstall RPC hooks.
func TestDeviceRemoteHeldConstructorCannotReplaceAdminApiLogin(t *testing.T) {
	testingAuthConstructorPublicationOrder(t, true, true)
}
