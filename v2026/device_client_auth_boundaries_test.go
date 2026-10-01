// Additional auth-contract diagnostics exercise actual device constructors
// without changing the frozen role-repair candidate or contacting a backend.
package sdk

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// A platform/member token cannot be refreshed by the server. The public
// result-only API preserves both credential roles and fires no device event.
func TestPlatformRemoteMemberRefreshRejectionPreservesAuth(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	memberJwt := testingJwt(map[string]any{
		"user_id":      "00000000-0000-0000-0000-000000000003",
		"network_id":   "00000000-0000-0000-0000-000000000004",
		"network_name": "platform-member-contract-test",
	})
	if err := fixture.localState.SetByJwt(memberJwt); err != nil {
		t.Fatal(err)
	}
	before, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	settings := defaultDeviceRpcSettings()
	settings.DisableHostedIncompatible = true
	settings.DisableLogging = true
	// This is the constructor body and mode used by NewPlatformDeviceRemote;
	// the injected dialer excludes network scheduling from the contract test.
	device, err := newDeviceRemoteWithOverrides(
		fixture.networkSpace, memberJwt, NewId(), settings,
		connect.Id{}, alwaysOfflineDeviceRpcDialer{},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := device.CloseAndWait(ctx); err != nil {
			t.Error("platform member fixture did not join")
		}
	})
	if len(fixture.api.jwtRefreshListeners.Get()) != 1 ||
		len(fixture.api.authLogoutListeners.Get()) != 1 {
		t.Fatal("platform constructor did not install the actual auth listeners")
	}
	if jwtCanRefresh(memberJwt) {
		t.Fatal("automatic token manager admits an admin-only member token")
	}
	observed := 0
	sub := device.AddJwtRefreshListener(jwtRefreshListenerFunc(func(string) { observed += 1 }))
	t.Cleanup(sub.Close)
	requests := 0
	fixture.api.setHttpGetRaw(func(_ context.Context, requestUrl string, byJwt string) ([]byte, error) {
		requests += 1
		if !strings.HasSuffix(requestUrl, "/auth/refresh") || byJwt != memberJwt {
			t.Error("manual platform refresh used the wrong path or credential")
		}
		// server/controller/auth_controller.go RefreshToken returns this
		// logical error before database access when client_id is absent.
		return []byte(`{"error":{"message":"Client ID is required for token refresh."}}`), nil
	})
	result, err := fixture.api.RefreshJwtSync()
	if err != nil {
		t.Fatal("manual member refresh did not return its logical rejection")
	}
	if requests != 1 || result == nil || result.Error == nil ||
		result.Error.Message != "Client ID is required for token refresh." || result.ByJwt != "" {
		t.Fatal("manual member refresh lost the server rejection contract")
	}
	device.stateLock.Lock()
	deviceJwt := device.byJwt
	device.stateLock.Unlock()
	after, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if fixture.api.GetByJwt() != memberJwt || deviceJwt != memberJwt || observed != 0 || after != before {
		t.Fatal("result-only member refresh mutated live or persisted credentials")
	}
}

// A stale supplied token and a later durable same-instance client token are
// intentionally different. Every constructor consumer must retain the winner.
func TestDeviceLocalStartupDoesNotRollBackNewerPersistedClient(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.seedDistinctLogin(t)
	newerJwt := testingRefreshableJwtWithMarker(t, "newer-persisted-client")
	if err := fixture.localState.SetByClientJwtForInstance(newerJwt, fixture.instanceId); err != nil {
		t.Fatal(err)
	}
	fixture.startLocal(t)
	after, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if after.ByClientJwt != newerJwt {
		t.Error("stale supplied startup client rolled back the later durable same-instance credential")
	}
	if fixture.api.GetByJwt() != newerJwt || fixture.deviceJwt() != newerJwt {
		t.Error("constructor selected durable auth but published a different live credential")
	}
	if after.ByJwt != fixture.adminJwt || after.InstanceId != fixture.instanceId.String() {
		t.Error("stale startup altered the separate admin or stable instance")
	}
}

// The native remote constructor shares the same supplied-token
// versus later persisted-client ordering boundary.
func TestDeviceRemoteStartupDoesNotRollBackNewerPersistedClient(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.seedDistinctLogin(t)
	newerJwt := testingRefreshableJwtWithMarker(t, "newer-persisted-client")
	if err := fixture.localState.SetByClientJwtForInstance(newerJwt, fixture.instanceId); err != nil {
		t.Fatal(err)
	}
	fixture.startRemote(t)
	after, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if after.ByClientJwt != newerJwt {
		t.Error("stale supplied startup client rolled back the later durable same-instance credential")
	}
	if fixture.api.GetByJwt() != newerJwt || fixture.deviceJwt() != newerJwt {
		t.Error("remote selected durable auth but published a different live credential")
	}
	if after.ByJwt != fixture.adminJwt || after.InstanceId != fixture.instanceId.String() {
		t.Error("stale startup altered the separate admin or stable instance")
	}
}

// Older refresh code overwrote the lost admin slot with a client token.
// Client repair cannot reconstruct that admin and must not erase routing state.
func TestDeviceRemoteLegacyCollapsedAuthRemainsRoutableWithoutInventingAdmin(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	legacyClientJwt := fixture.initialJwt
	if err := fixture.localState.SetByJwt(legacyClientJwt); err != nil {
		t.Fatal(err)
	}
	if err := fixture.localState.SetByClientJwt(legacyClientJwt); err != nil {
		t.Fatal(err)
	}
	fixture.instanceId = fixture.localState.GetInstanceId()
	if fixture.instanceId == nil {
		t.Fatal("legacy store did not retain its instance")
	}
	location := &ConnectLocation{
		Name:              "legacy-retained-route",
		ConnectLocationId: &ConnectLocationId{LocationId: NewId()},
	}
	if err := fixture.localState.SetConnectLocation(location); err != nil {
		t.Fatal(err)
	}
	fixture.startRemote(t)
	refreshedJwt := testingRefreshableJwtWithMarker(t, "legacy-client-refreshed")
	if !fixture.api.setRefreshedByJwt(legacyClientJwt, refreshedJwt) {
		t.Fatal("legacy provider client refresh was not accepted")
	}
	after, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if fixture.api.GetByJwt() != refreshedJwt || fixture.deviceJwt() != refreshedJwt ||
		after.ByClientJwt != refreshedJwt || after.InstanceId != fixture.instanceId.String() {
		t.Error("legacy client could not continue refreshing with its stable instance")
	}
	// This is an already-corrupted field, not a recovered admin token.
	// Leave it unchanged; reauthentication is the only way to recover lost A.
	if after.ByJwt != legacyClientJwt {
		t.Error("client repair rewrote the legacy admin slot without admin credentials")
	}
	retainedLocation := fixture.localState.GetConnectLocation()
	if retainedLocation == nil || !connectLocationValuesEqual(retainedLocation, location) {
		t.Error("legacy credential-role repair erased the saved routing state")
	}
}
