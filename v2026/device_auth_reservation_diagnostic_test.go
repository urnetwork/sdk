// Additional constructor-reservation diagnostics are kept separate from the
// frozen v3 pair. They use actual installed callbacks and no external service.
package sdk

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// Constructs through the actual Local or Remote body without fatal assertions
// on a worker goroutine, so held-constructor cleanup remains observable.
func testingBuildReservedDevice(fixture *testingAuthClientShape, remote bool) testingAuthConstructorResult {
	if remote {
		settings := defaultDeviceRpcSettings()
		settings.DisableLogging = true
		device, err := newDeviceRemoteWithOverrides(
			fixture.networkSpace, fixture.initialJwt, fixture.instanceId, settings,
			connect.NewId(), alwaysOfflineDeviceRpcDialer{},
		)
		return testingAuthConstructorResult{remote: device, err: err}
	}
	settings := DefaultDeviceLocalSettings()
	settings.AllowProvider = false
	settings.DisableLogging = true
	device, err := newDeviceLocalWithOverrides(
		fixture.networkSpace, fixture.initialJwt, "reserved-constructor", "test", "0.0.0",
		fixture.instanceId, settings, connect.NewId(),
	)
	return testingAuthConstructorResult{local: device, err: err}
}

// Without supersession, a held read-only preparation still constructs a
// consistent replacement. The old strict-success refresh comparator remains
// frozen separately; V4's safe-abort tests below name its changed contract.
func testingHeldConstructorWithoutRefresh(t *testing.T, remote bool) {
	t.Helper()
	fixture := testingAuthClientShapeSpace(t)
	fixture.initialJwt = testingStartupClientJwt(t, "serving-original", nil)
	fixture.seedDistinctLogin(t)
	if remote {
		fixture.startRemote(t)
	} else {
		fixture.startLocal(t)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	resume := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(resume)
	fixture.localState.testingAfterDeviceAuthPrepare = func(*deviceAuthPublicationGate) {
		close(entered)
		<-release
	}
	result := make(chan testingAuthConstructorResult, 1)
	go func() { result <- testingBuildReservedDevice(fixture, remote) }()
	testingAwaitAuthBoundary(t, entered)
	expectedJwt := fixture.initialJwt

	resume()
	var built testingAuthConstructorResult
	select {
	case built = <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("reserved constructor did not finish")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if built.local != nil {
			if err := built.local.CloseAndWait(ctx); err != nil {
				t.Error("new local device did not join")
			}
		}
		if built.remote != nil {
			if err := built.remote.CloseAndWait(ctx); err != nil {
				t.Error("new remote did not join")
			}
		}
	})
	if built.err != nil {
		t.Fatal("reserved constructor failed unexpectedly")
	}
	var deviceJwt string
	if built.local != nil {
		built.local.stateLock.Lock()
		deviceJwt = built.local.byJwt
		built.local.stateLock.Unlock()
	} else if built.remote != nil {
		built.remote.stateLock.Lock()
		deviceJwt = built.remote.byJwt
		built.remote.stateLock.Unlock()
	} else {
		t.Fatal("reserved constructor returned neither a device nor an error")
	}
	state, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if fixture.api.GetByJwt() != expectedJwt {
		t.Error("held constructor rolled the API back over its already-committed refresh")
	}
	if state.ByClientJwt != expectedJwt || deviceJwt != expectedJwt {
		t.Error("held constructor lost the newer client generation between API, storage and device")
	}
	if state.ByJwt != fixture.adminJwt || state.InstanceId != fixture.instanceId.String() {
		t.Error("held constructor changed the separate admin or stable instance")
	}
}

// Unlike the frozen v3 strict-success comparator, a constructor overtaken
// during preparation deliberately reports supersession. The serving device
// must retain both this refresh and the next one, without any partial device.
func testingPreparedConstructorRefreshSupersession(t *testing.T, remote bool) {
	t.Helper()
	fixture := testingAuthClientShapeSpace(t)
	fixture.initialJwt = testingStartupClientJwt(t, "before-preparation", nil)
	fixture.seedDistinctLogin(t)
	if remote {
		fixture.startRemote(t)
	} else {
		fixture.startLocal(t)
	}
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
	refreshedJwt := testingStartupClientJwt(t, "during-preparation", map[string]any{
		"iat": testingClientStartupNow - 50,
	})
	if !fixture.api.setRefreshedByJwt(fixture.initialJwt, refreshedJwt) {
		t.Fatal("serving API did not commit its refresh")
	}
	resume()
	var built testingAuthConstructorResult
	select {
	case built = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("superseded constructor did not finish")
	}
	if built.local != nil {
		_ = built.local.CloseAndWait(context.Background())
		t.Error("superseded constructor exposed a local device")
	}
	if built.remote != nil {
		_ = built.remote.CloseAndWait(context.Background())
		t.Error("superseded constructor exposed a remote device")
	}
	if built.err == nil {
		t.Error("superseded constructor did not return an error")
	}
	state, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if state.ByClientJwt != refreshedJwt || fixture.api.GetByJwt() != refreshedJwt ||
		fixture.deviceJwt() != refreshedJwt || state.ByJwt != fixture.adminJwt ||
		state.InstanceId != fixture.instanceId.String() {
		t.Error("superseded startup lost the serving owner's committed auth")
	}
	nextJwt := testingStartupClientJwt(t, "after-supersession", map[string]any{
		"iat": testingClientStartupNow - 10,
	})
	if !fixture.api.setRefreshedByJwt(refreshedJwt, nextJwt) {
		t.Fatal("serving API could not commit its next refresh")
	}
	after, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if after.ByClientJwt != nextJwt || fixture.deviceJwt() != nextJwt ||
		after.ByJwt != fixture.adminJwt || after.InstanceId != fixture.instanceId.String() {
		t.Error("superseded constructor stranded future serving-owner publication")
	}
}

// The safe-abort contract does not require replaying a constructor or callback.
func TestPreparedLocalConstructorSupersededByRefreshKeepsServingOwner(t *testing.T) {
	testingPreparedConstructorRefreshSupersession(t, false)
}

// Remote supersession also retains the live API and its current HTTP owner.
func TestPreparedRemoteConstructorSupersededByRefreshKeepsServingOwner(t *testing.T) {
	testingPreparedConstructorRefreshSupersession(t, true)
}

// Healthy control: admission itself does not alter a token that never rotated.
func TestHeldLocalConstructorWithoutRefreshKeepsConsistentAuth(t *testing.T) {
	testingHeldConstructorWithoutRefresh(t, false)
}

// The remote control excludes a generic constructor/fixture failure.
func TestHeldRemoteConstructorWithoutRefreshKeepsConsistentAuth(t *testing.T) {
	testingHeldConstructorWithoutRefresh(t, true)
}

// Force the existing non-default address-allocation error after claiming auth.
// No pool is exhausted and no OS address, route, interface or device is changed.
func TestFailedLocalConstructorDoesNotRetireServingAuthOwner(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.initialJwt = testingStartupClientJwt(t, "serving-before-failure", nil)
	fixture.seedDistinctLogin(t)
	fixture.startLocal(t)
	settings := DefaultDeviceLocalSettings()
	settings.AllowProvider = false
	settings.DisableLogging = true
	settings.UseExperimentalTunnelAddress = false
	settings.testingTakeLocalAddress = func() (netip.Addr, bool) { return netip.Addr{}, false }
	device, err := newDeviceLocalWithOverrides(
		fixture.networkSpace, fixture.initialJwt, "failing-constructor", "test", "0.0.0",
		fixture.instanceId, settings, connect.NewId(),
	)
	if device != nil {
		device.Close()
		t.Fatal("allocator failure unexpectedly produced a replacement device")
	}
	if err == nil {
		t.Fatal("injected allocator failure did not reach the real failure return")
	}
	if fixture.api.GetByJwt() != fixture.initialJwt ||
		!fixture.api.deviceOwnsAuth(fixture.localDevice.authPublication) {
		t.Fatal("failed constructor had already replaced the serving API owner")
	}
	nextJwt := testingStartupClientJwt(t, "serving-after-failure", map[string]any{
		"iat": testingClientStartupNow - 10,
	})
	if !fixture.api.setRefreshedByJwt(fixture.initialJwt, nextJwt) {
		t.Fatal("serving API could not commit its later valid refresh")
	}
	state, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if state.ByClientJwt != nextJwt || fixture.deviceJwt() != nextJwt {
		t.Error("failed constructor permanently stripped the serving device's persistence authority")
	}
	if state.ByJwt != fixture.adminJwt || state.InstanceId != fixture.instanceId.String() {
		t.Error("constructor failure changed the separate admin or stable instance")
	}
}
