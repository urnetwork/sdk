package sdk

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

func TestDeviceLocalClientSeedFailsBeforeApiPublication(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.api.SetByJwt("previous-api-owner")
	if err := os.WriteFile(fixture.localState.authStatePath(), []byte("{"), LocalStorageFilePermissions); err != nil {
		t.Fatal(err)
	}
	settings := DefaultDeviceLocalSettings()
	settings.AllowProvider = false
	settings.DisableLogging = true
	device, err := newDeviceLocalWithOverrides(
		fixture.networkSpace, fixture.initialJwt, "client-seed-test", "test", "0.0.0",
		fixture.instanceId, settings, connect.NewId(),
	)
	if device != nil {
		device.Close()
		t.Fatal("device started with an unreadable auth envelope")
	}
	if err == nil {
		t.Fatal("client seed failure was not returned")
	}
	if fixture.api.GetByJwt() != "previous-api-owner" ||
		len(fixture.api.jwtRefreshListeners.Get()) != 0 ||
		len(fixture.api.authLogoutListeners.Get()) != 0 {
		t.Fatal("failed client seed published a new API credential or listener")
	}
}

func TestDeviceRemoteClientSeedFailsBeforeApiPublication(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.api.SetByJwt("previous-api-owner")
	if err := os.WriteFile(fixture.localState.authStatePath(), []byte("{"), LocalStorageFilePermissions); err != nil {
		t.Fatal(err)
	}
	settings := defaultDeviceRpcSettings()
	settings.DisableLogging = true
	device, err := newDeviceRemoteWithOverrides(
		fixture.networkSpace, fixture.initialJwt, fixture.instanceId, settings,
		connect.NewId(), alwaysOfflineDeviceRpcDialer{},
	)
	if device != nil {
		device.Close()
		t.Fatal("remote started with an unreadable auth envelope")
	}
	if err == nil {
		t.Fatal("remote client seed failure was not returned")
	}
	fixture.api.mutex.Lock()
	postInstalled := fixture.api.httpPostRaw != nil
	getInstalled := fixture.api.httpGetRaw != nil
	fixture.api.mutex.Unlock()
	if fixture.api.GetByJwt() != "previous-api-owner" || postInstalled || getInstalled ||
		len(fixture.api.jwtRefreshListeners.Get()) != 0 ||
		len(fixture.api.authLogoutListeners.Get()) != 0 {
		t.Fatal("failed remote seed changed API credential, transports or listeners")
	}
}

func TestHostedDeviceClientAuthNeverMutatesSharedHostCredentials(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.seedDistinctLogin(t)
	before, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	// An inherited in-memory API transport prevents any public network request
	// while the hosted constructor enables its independent refresh worker.
	fixture.api.setHttpPostRaw(func(ctx context.Context, _ string, _ []byte, _ string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	tenantJwt := testingRefreshableJwtWithMarker(t, "hosted-tenant-initial")
	settings := DefaultDeviceLocalSettings()
	settings.AllowProvider = false
	settings.HostedIncompatible = true
	settings.DisableLogging = true
	device, err := newDeviceLocalWithOverrides(
		fixture.networkSpace, tenantJwt, "hosted-auth-test", "test", "0.0.0",
		NewId(), settings, connect.NewId(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := device.CloseAndWait(ctx); err != nil {
			t.Error("hosted auth fixture did not join")
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tenantApi := device.GetApi()
	if tenantApi == fixture.api || !device.ownsApi {
		t.Fatal("hosted constructor did not isolate its API")
	}
	if err := tenantApi.CloseAndWait(ctx); err != nil {
		t.Fatal("hosted automatic worker did not join")
	}
	refreshedJwt := testingRefreshableJwtWithMarker(t, "hosted-tenant-refreshed")
	if !tenantApi.setRefreshedByJwt(tenantJwt, refreshedJwt) {
		t.Fatal("hosted client refresh was rejected")
	}
	device.stateLock.Lock()
	deviceJwt := device.byJwt
	device.stateLock.Unlock()
	if deviceJwt != refreshedJwt {
		t.Fatal("hosted client refresh did not reach its own device")
	}
	afterRefresh, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if afterRefresh != before {
		t.Error("hosted construction or refresh changed shared host auth")
	}
	if !tenantApi.rejectByJwt(refreshedJwt) {
		t.Fatal("hosted current client rejection was not accepted")
	}
	afterLogout, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if afterLogout != before {
		t.Error("hosted client logout erased shared host auth")
	}
}

func TestPlatformRemoteDoesNotSeedMemberTokenAsProviderClient(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.seedDistinctLogin(t)
	before, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	settings := defaultDeviceRpcSettings()
	settings.DisableHostedIncompatible = true
	settings.DisableLogging = true
	device, err := newDeviceRemoteWithOverrides(
		fixture.networkSpace, fixture.adminJwt, NewId(), settings,
		connect.Id{}, alwaysOfflineDeviceRpcDialer{},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := device.CloseAndWait(ctx); err != nil {
			t.Error("platform remote fixture did not join")
		}
	})
	after, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Error("platform remote mislabeled its member token as a provider client")
	}
}
