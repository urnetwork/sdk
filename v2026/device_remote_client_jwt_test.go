// Native getter tests use actual constructed remotes with an offline test
// dialer; no shared API/admin field is a provider-client fallback.
package sdk

import (
	"context"
	"testing"
	"time"
)

// Durable selection can supersede the supplied constructor token. Later
// admin installation on the shared API must not alter the device getter.
func TestRemoteClientJwtUsesConstructedSelectionNotAdminApi(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.initialJwt = testingStartupClientJwt(t, "remote-supplied-old", nil)
	fixture.seedDistinctLogin(t)
	selectedJwt := testingStartupClientJwt(t, "remote-selected-new", map[string]any{
		"iat": testingClientStartupNow - 10,
	})
	if err := fixture.localState.SetByClientJwt(selectedJwt); err != nil {
		t.Fatal(err)
	}
	if err := fixture.localState.SetInstanceId(fixture.instanceId); err != nil {
		t.Fatal(err)
	}
	fixture.startRemote(t)
	if fixture.remoteDevice.GetClientJwt() != selectedJwt {
		t.Fatal("remote getter returned stale constructor input")
	}
	fixture.api.SetByJwt(fixture.adminJwt)
	if fixture.remoteDevice.GetClientJwt() != selectedJwt ||
		fixture.api.GetByJwt() != fixture.adminJwt ||
		fixture.localState.GetByJwt() != fixture.adminJwt {
		t.Error("remote client getter exposed or altered the separate admin")
	}
}

// The value follows the actual installed refresh/logout callback, not a
// constructor-only copy that remains stale for later profile reconciliation.
func TestRemoteClientJwtTracksPublishedRefreshAndLogout(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.seedDistinctLogin(t)
	fixture.startRemote(t)
	nextJwt := testingRefreshableJwtWithMarker(t, "remote-getter-refresh")
	if !fixture.api.setRefreshedByJwt(fixture.initialJwt, nextJwt) {
		t.Fatal("actual refresh did not commit")
	}
	if fixture.remoteDevice.GetClientJwt() != nextJwt {
		t.Error("remote getter missed its published refresh")
	}
	if !fixture.api.rejectByJwt(nextJwt) {
		t.Fatal("actual rejection did not commit")
	}
	if fixture.remoteDevice.GetClientJwt() != "" {
		t.Error("remote getter retained a client after published logout")
	}
}

// The actual platform constructor accepts member auth for control; it must
// never relabel that admin/member token as a provider credential.
func TestPlatformRemoteClientJwtNeverReturnsMemberCredential(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.seedDistinctLogin(t)
	before, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	memberJwt := testingJwt(map[string]any{
		"network_id": "00000000-0000-0000-0000-000000000004",
		"user_id":    "00000000-0000-0000-0000-000000000003",
	})
	device, err := NewPlatformDeviceRemote(
		fixture.networkSpace, memberJwt, "127.0.0.1:1",
		"synthetic-platform-auth", NewId(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := device.CloseAndWait(ctx); err != nil {
			t.Error("platform remote getter fixture did not join")
		}
	})
	after, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if device.GetClientJwt() != "" || fixture.api.GetByJwt() != memberJwt || after != before {
		t.Error("platform getter invented provider auth from member control credentials")
	}
}
