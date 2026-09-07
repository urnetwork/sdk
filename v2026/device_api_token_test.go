package sdk

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

func TestDeviceLocalAppliesApiRefreshAndLogout(t *testing.T) {
	initialJwt := testingRefreshableJwtWithMarker(t, "device-local-initial")
	refreshedJwt := testingRefreshableJwtWithMarker(t, "device-local-refreshed")
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	refreshedRequestStarted := make(chan struct{})
	windowAuth := make(chan string, 1)
	var firstStartedOnce sync.Once
	var refreshedStartedOnce sync.Once

	ts := httptest.NewServer(testApiHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/network/auth-client" {
			windowAuth <- r.Header.Get("Authorization")
			fmt.Fprintf(w, `{"by_client_jwt":%q}`, refreshedJwt)
			return
		}
		switch r.Header.Get("Authorization") {
		case "Bearer " + initialJwt:
			firstStartedOnce.Do(func() {
				close(firstStarted)
			})
			<-releaseFirst
			fmt.Fprintf(w, `{"by_jwt":%q}`, refreshedJwt)
		case "Bearer " + refreshedJwt:
			refreshedStartedOnce.Do(func() {
				close(refreshedRequestStarted)
			})
			fmt.Fprint(w, `{"error":{"message":"client no longer exists"}}`)
		default:
			http.Error(w, "unexpected authorization", http.StatusBadRequest)
		}
	})))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	storagePath := t.TempDir()
	networkSpace := newNetworkSpace(
		ctx,
		*NewNetworkSpaceKey("refresh.test", "test"),
		NetworkSpaceValues{
			ApiUrl:                   ts.URL,
			PlatformUrl:              "ws://127.0.0.1:1",
			NetExposeServerIps:       true,
			NetExposeServerHostNames: true,
		},
		storagePath,
	)
	defer networkSpace.close()
	defer networkSpace.asyncLocalState.Close()
	localState := networkSpace.asyncLocalState.localState
	if err := localState.SetByJwt(initialJwt); err != nil {
		t.Fatal(err)
	}
	if err := localState.SetByClientJwt(initialJwt); err != nil {
		t.Fatal(err)
	}
	liveInstanceId := localState.GetInstanceId()
	if liveInstanceId == nil {
		t.Fatal("seeded DeviceLocal has no persisted instance_id")
	}

	settings := DefaultDeviceLocalSettings()
	settings.AllowProvider = false
	settings.Verbose = false
	settings.DisableLogging = true
	device, err := newDeviceLocalWithOverrides(
		networkSpace,
		initialJwt,
		"refresh-test",
		"test",
		"0.0.0",
		liveInstanceId,
		settings,
		connect.NewId(),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()
	windowGenerator := connect.NewApiMultiClientGenerator(
		ctx,
		nil,
		networkSpace.clientStrategy,
		nil,
		ts.URL,
		initialJwt,
		"ws://127.0.0.1:1",
		"refresh-test",
		"test",
		"0.0.0",
		nil,
		connect.DefaultClientSettings,
		connect.DefaultApiMultiClientGeneratorSettings(),
	)
	device.stateLock.Lock()
	device.apiMultiClientGenerator = windowGenerator
	device.stateLock.Unlock()

	select {
	case <-firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("DeviceLocal did not start the Api-owned refresh")
	}
	deviceRefreshed := make(chan string, 1)
	refreshSub := device.AddJwtRefreshListener(jwtRefreshListenerFunc(func(jwt string) {
		deviceRefreshed <- jwt
	}))
	defer refreshSub.Close()
	deviceLoggedOut := make(chan struct{}, 1)
	logoutSub := device.AddAuthLogoutListener(authLogoutListenerFunc(func() {
		deviceLoggedOut <- struct{}{}
	}))
	defer logoutSub.Close()
	close(releaseFirst)

	if got := receiveStringWithin(t, deviceRefreshed, "DeviceLocal did not publish the Api refresh"); got != refreshedJwt {
		t.Fatalf("DeviceLocal refreshed JWT = %q, want %q", got, refreshedJwt)
	}
	if got := networkSpace.GetApi().GetByJwt(); got != refreshedJwt {
		t.Fatalf("Api JWT = %q, want DeviceLocal refreshed JWT", got)
	}
	device.stateLock.Lock()
	deviceJwt := device.byJwt
	device.stateLock.Unlock()
	if deviceJwt != refreshedJwt {
		t.Fatalf("DeviceLocal JWT = %q, want refreshed JWT", deviceJwt)
	}
	if got := localState.GetByJwt(); got != refreshedJwt {
		t.Fatalf("persisted network JWT = %q, want refreshed JWT", got)
	}
	if got := localState.GetByClientJwt(); got != refreshedJwt {
		t.Fatalf("persisted client JWT = %q, want refreshed JWT", got)
	}
	if got := localState.GetInstanceId(); got == nil || got.Cmp(liveInstanceId) != 0 {
		t.Fatalf("persisted instance_id after refresh = %v, want live %v", got, liveInstanceId)
	}
	if _, err := windowGenerator.NewClientArgsContext(ctx); err != nil {
		t.Fatalf("mint client after device JWT refresh: %v", err)
	}
	select {
	case authorization := <-windowAuth:
		if authorization != "Bearer "+refreshedJwt {
			t.Fatalf("window generator authorization did not rotate with device JWT")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("window generator did not authenticate a later client")
	}

	// A confirmed logical rejection is propagated through the same Api owner,
	// clears persisted state, and preserves DeviceLocal's existing app-facing
	// logout callback contract.
	if err := device.RefreshToken(0); err != nil {
		t.Fatal(err)
	}
	select {
	case <-refreshedRequestStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("DeviceLocal rejection request was not observed")
	}
	select {
	case <-deviceLoggedOut:
	case <-time.After(5 * time.Second):
		t.Fatal("DeviceLocal did not publish Api logout")
	}
	if got := networkSpace.GetApi().GetByJwt(); got != "" {
		t.Fatalf("Api JWT after rejection = %q, want empty", got)
	}
	device.stateLock.Lock()
	deviceJwt = device.byJwt
	device.stateLock.Unlock()
	if deviceJwt != "" {
		t.Fatalf("DeviceLocal JWT after rejection = %q, want empty", deviceJwt)
	}
	if got := localState.GetByJwt(); got != "" {
		t.Fatalf("persisted network JWT after rejection = %q, want empty", got)
	}
	if got := localState.GetByClientJwt(); got != "" {
		t.Fatalf("persisted client JWT after rejection = %q, want empty", got)
	}
	if got := localState.GetInstanceId(); got != nil {
		t.Fatalf("persisted instance_id after rejection = %v, want nil", got)
	}
}

type alwaysOfflineDeviceRpcDialer struct{}

func (alwaysOfflineDeviceRpcDialer) Dial(ctx context.Context) (net.Conn, net.Conn, error) {
	select {
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	default:
		return nil, nil, errors.New("device rpc offline")
	}
}

func TestDeviceRemoteAppliesStandaloneApiRefreshAndLogout(t *testing.T) {
	initialJwt := testingRefreshableJwtWithMarker(t, "device-remote-initial")
	refreshedJwt := testingRefreshableJwtWithMarker(t, "device-remote-refreshed")
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	refreshedRequestStarted := make(chan struct{})
	var firstStartedOnce sync.Once
	var refreshedStartedOnce sync.Once

	ts := httptest.NewServer(testApiHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Header.Get("Authorization") {
		case "Bearer " + initialJwt:
			firstStartedOnce.Do(func() {
				close(firstStarted)
			})
			<-releaseFirst
			fmt.Fprintf(w, `{"by_jwt":%q}`, refreshedJwt)
		case "Bearer " + refreshedJwt:
			refreshedStartedOnce.Do(func() {
				close(refreshedRequestStarted)
			})
			http.Error(w, "not authorized", http.StatusUnauthorized)
		default:
			http.Error(w, "unexpected authorization", http.StatusBadRequest)
		}
	})))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	networkSpace := NewNetworkSpaceWithUrls(
		ctx,
		ts.URL,
		"ws://127.0.0.1:1",
		connect.DefaultClientStrategySettings(),
	)
	defer networkSpace.close()

	settings := defaultDeviceRpcSettings()
	settings.RpcReconnectTimeout = 10 * time.Millisecond
	settings.DisableLogging = true
	device, err := newDeviceRemoteWithOverrides(
		networkSpace,
		initialJwt,
		NewId(),
		settings,
		connect.NewId(),
		alwaysOfflineDeviceRpcDialer{},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()

	select {
	case <-firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("DeviceRemote did not start the standalone Api refresh")
	}
	deviceRefreshed := make(chan string, 1)
	refreshSub := device.AddJwtRefreshListener(jwtRefreshListenerFunc(func(jwt string) {
		deviceRefreshed <- jwt
	}))
	defer refreshSub.Close()
	deviceLoggedOut := make(chan struct{}, 1)
	logoutSub := device.AddAuthLogoutListener(authLogoutListenerFunc(func() {
		deviceLoggedOut <- struct{}{}
	}))
	defer logoutSub.Close()
	close(releaseFirst)

	if got := receiveStringWithin(t, deviceRefreshed, "DeviceRemote did not publish the Api refresh"); got != refreshedJwt {
		t.Fatalf("DeviceRemote refreshed JWT = %q, want %q", got, refreshedJwt)
	}
	if got := networkSpace.GetApi().GetByJwt(); got != refreshedJwt {
		t.Fatalf("Api JWT = %q, want DeviceRemote refreshed JWT", got)
	}
	device.stateLock.Lock()
	deviceJwt := device.byJwt
	device.stateLock.Unlock()
	if deviceJwt != refreshedJwt {
		t.Fatalf("DeviceRemote JWT = %q, want refreshed JWT", deviceJwt)
	}

	if err := device.RefreshToken(0); err != nil {
		t.Fatal(err)
	}
	select {
	case <-refreshedRequestStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("DeviceRemote rejection request was not observed")
	}
	select {
	case <-deviceLoggedOut:
	case <-time.After(5 * time.Second):
		t.Fatal("DeviceRemote did not publish Api logout")
	}
	if got := networkSpace.GetApi().GetByJwt(); got != "" {
		t.Fatalf("Api JWT after rejection = %q, want empty", got)
	}
	device.stateLock.Lock()
	deviceJwt = device.byJwt
	device.stateLock.Unlock()
	if deviceJwt != "" {
		t.Fatalf("DeviceRemote JWT after rejection = %q, want empty", deviceJwt)
	}
}
