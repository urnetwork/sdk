// Startup transaction diagnostics extend, rather than replace, the frozen
// reservation reproductions. All barriers use real constructors and callbacks.
package sdk

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// A proposed startup credential is not a completed login. Force the actual
// allocation failure after selection and require the serving session to remain
// intact, including its next real API-to-storage refresh.
func testingFailedConstructorProposedAuth(t *testing.T, replaceInstance bool) {
	t.Helper()
	fixture := testingAuthClientShapeSpace(t)
	fixture.initialJwt = testingStartupClientJwt(t, "serving-before-proposal", nil)
	fixture.seedDistinctLogin(t)
	fixture.startLocal(t)
	if err := fixture.localState.SetBlockerEnabled(true); err != nil {
		t.Fatal(err)
	}
	before, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	proposedJwt := testingStartupClientJwt(t, "failed-proposal", map[string]any{
		"iat": testingClientStartupNow - 50,
	})
	proposedInstance := fixture.instanceId
	if replaceInstance {
		proposedInstance = NewId()
	}
	settings := DefaultDeviceLocalSettings()
	settings.AllowProvider = false
	settings.DisableLogging = true
	settings.UseExperimentalTunnelAddress = false
	settings.testingTakeLocalAddress = func() (netip.Addr, bool) { return netip.Addr{}, false }
	device, err := newDeviceLocalWithOverrides(
		fixture.networkSpace, proposedJwt, "failed-proposal", "test", "0.0.0",
		proposedInstance, settings, connect.NewId(),
	)
	if device != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = device.CloseAndWait(ctx)
		t.Fatal("allocation failure returned a replacement device")
	}
	if err == nil {
		t.Fatal("allocation failure did not reach the real constructor failure path")
	}
	after, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Error("failed constructor committed its proposed auth before device publication")
	}
	if fixture.api.GetByJwt() != fixture.initialJwt || fixture.deviceJwt() != fixture.initialJwt ||
		!fixture.localState.GetBlockerEnabled() {
		t.Error("failed constructor disturbed the serving device or routing preference")
	}
	nextJwt := testingStartupClientJwt(t, "serving-after-proposal-failure", map[string]any{
		"iat": testingClientStartupNow - 10,
	})
	if !fixture.api.setRefreshedByJwt(fixture.initialJwt, nextJwt) {
		t.Fatal("serving API did not accept its next refresh")
	}
	afterRefresh, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if afterRefresh.ByClientJwt != nextJwt || fixture.deviceJwt() != nextJwt {
		t.Error("failed proposal stranded the serving device's future refresh publication")
	}
	if afterRefresh.ByJwt != fixture.adminJwt || afterRefresh.InstanceId != fixture.instanceId.String() {
		t.Error("failed proposal replaced the serving admin or stable instance")
	}
}

// A newer same-identity token must not commit when construction itself fails.
func TestFailedLocalConstructorDoesNotCommitProposedClient(t *testing.T) {
	testingFailedConstructorProposedAuth(t, false)
}

// A proposed instance is also speculative until the constructor can publish;
// this does not undo a new login already committed by an explicit login setter.
func TestFailedLocalConstructorDoesNotCommitProposedInstance(t *testing.T) {
	testingFailedConstructorProposedAuth(t, true)
}

// Force an API refresh before startup reads its ownership generation but keep
// the installed old callback from writing storage. A constructor may adopt the
// committed token or fail while retaining the serving owner; it may not restore
// the older disk/profile token or strand the next refresh.
func testingConstructorDuringPendingRefreshPersistence(t *testing.T, remote bool) {
	t.Helper()
	fixture := testingAuthClientShapeSpace(t)
	fixture.initialJwt = testingStartupClientJwt(t, "before-pending-persistence", nil)
	fixture.seedDistinctLogin(t)
	var owner *deviceAuthPublicationGate
	if remote {
		fixture.startRemote(t)
		owner = fixture.remoteDevice.authPublication
	} else {
		fixture.startLocal(t)
		owner = fixture.localDevice.authPublication
	}
	entered, resume := testingHoldAuthPublication(t, owner)
	refreshedJwt := testingStartupClientJwt(t, "pending-persistence", map[string]any{
		"iat": testingClientStartupNow - 50,
	})
	refreshDone := make(chan struct{})
	refreshAccepted := false
	go func() {
		defer close(refreshDone)
		refreshAccepted = fixture.api.setRefreshedByJwt(fixture.initialJwt, refreshedJwt)
	}()
	testingAwaitAuthBoundary(t, entered)
	if fixture.api.GetByJwt() != refreshedJwt {
		t.Fatal("refresh did not commit before the constructor began")
	}
	before, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if before.ByClientJwt != fixture.initialJwt {
		t.Fatal("pending-persistence boundary did not retain the older durable token")
	}
	constructorDone := make(chan testingAuthConstructorResult, 1)
	go func() { constructorDone <- testingBuildReservedDevice(fixture, remote) }()
	var built testingAuthConstructorResult
	select {
	case built = <-constructorDone:
	case <-time.After(5 * time.Second):
		resume()
		t.Error("constructor joined another device's parked callback")
		select {
		case built = <-constructorDone:
		case <-time.After(5 * time.Second):
			t.Fatal("constructor did not finish after releasing the parked callback")
		}
	}
	t.Cleanup(func() {
		resume()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if built.local != nil {
			if err := built.local.CloseAndWait(ctx); err != nil {
				t.Error("replacement local device did not join")
			}
		}
		if built.remote != nil {
			if err := built.remote.CloseAndWait(ctx); err != nil {
				t.Error("replacement remote device did not join")
			}
		}
	})
	currentDeviceJwt := fixture.deviceJwt
	if built.err == nil {
		if built.local != nil {
			currentDeviceJwt = func() string {
				built.local.stateLock.Lock()
				defer built.local.stateLock.Unlock()
				return built.local.byJwt
			}
		} else if built.remote != nil {
			currentDeviceJwt = func() string {
				built.remote.stateLock.Lock()
				defer built.remote.stateLock.Unlock()
				return built.remote.byJwt
			}
		} else {
			t.Fatal("constructor returned neither a device nor an error")
		}
	} else if built.local != nil || built.remote != nil {
		t.Error("failed constructor exposed a partial device")
	}
	resume()
	testingAwaitAuthBoundary(t, refreshDone)
	if !refreshAccepted {
		t.Fatal("original refresh was not committed")
	}
	after, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if fixture.api.GetByJwt() != refreshedJwt || after.ByClientJwt != refreshedJwt ||
		currentDeviceJwt() != refreshedJwt {
		t.Error("startup lost the refresh committed before its ownership snapshot")
	}
	if after.ByJwt != fixture.adminJwt || after.InstanceId != fixture.instanceId.String() {
		t.Error("startup changed the admin credential or stable instance")
	}
	nextJwt := testingStartupClientJwt(t, "after-pending-persistence", map[string]any{
		"iat": testingClientStartupNow - 10,
	})
	if !fixture.api.setRefreshedByJwt(refreshedJwt, nextJwt) {
		t.Error("post-startup API could not accept the next serving refresh")
		return
	}
	afterNext, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if afterNext.ByClientJwt != nextJwt || currentDeviceJwt() != nextJwt {
		t.Error("startup left the serving device without durable refresh ownership")
	}
}

// Shared local construction must account for already-committed API refreshes.
func TestLocalConstructorPreservesRefreshAwaitingPersistence(t *testing.T) {
	testingConstructorDuringPendingRefreshPersistence(t, false)
}

// Native remotes share this boundary even though their data plane is remote.
func TestRemoteConstructorPreservesRefreshAwaitingPersistence(t *testing.T) {
	testingConstructorDuringPendingRefreshPersistence(t, true)
}
