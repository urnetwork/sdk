package sdk

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestDeviceLocalAppliesApiRefreshAndLogout(t *testing.T) {
	initialJwt := testingRefreshableJwtWithMarker(t, "device-local-initial")
	refreshedJwt := testingRefreshableJwtWithMarker(t, "device-local-refreshed")
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	requests := make(chan string, 2)
	var requestCount atomic.Int64

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestNumber := requestCount.Add(1)
		requests <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		switch requestNumber {
		case 1:
			close(firstStarted)
			<-releaseFirst
			fmt.Fprintf(w, `{"by_jwt":%q}`, refreshedJwt)
		case 2:
			fmt.Fprint(w, `{"error":{"message":"client no longer exists"}}`)
		default:
			http.Error(w, "unexpected refresh", http.StatusInternalServerError)
		}
	}))
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
		NewId(),
		settings,
		connect.NewId(),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()

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
	if got := receiveStringWithin(t, requests, "DeviceLocal refresh request was not observed"); got != "Bearer "+initialJwt {
		t.Fatalf("DeviceLocal refresh authorization = %q, want initial JWT", got)
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
	localState := networkSpace.asyncLocalState.localState
	if got := localState.GetByJwt(); got != refreshedJwt {
		t.Fatalf("persisted network JWT = %q, want refreshed JWT", got)
	}
	if got := localState.GetByClientJwt(); got != refreshedJwt {
		t.Fatalf("persisted client JWT = %q, want refreshed JWT", got)
	}

	// A confirmed logical rejection is propagated through the same Api owner,
	// clears persisted state, and preserves DeviceLocal's existing app-facing
	// logout callback contract.
	if err := device.RefreshToken(0); err != nil {
		t.Fatal(err)
	}
	select {
	case <-deviceLoggedOut:
	case <-time.After(5 * time.Second):
		t.Fatal("DeviceLocal did not publish Api logout")
	}
	if got := receiveStringWithin(t, requests, "DeviceLocal rejection request was not observed"); got != "Bearer "+refreshedJwt {
		t.Fatalf("DeviceLocal rejection authorization = %q, want refreshed JWT", got)
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
	requests := make(chan string, 2)
	var requestCount atomic.Int64

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestNumber := requestCount.Add(1)
		requests <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		switch requestNumber {
		case 1:
			close(firstStarted)
			<-releaseFirst
			fmt.Fprintf(w, `{"by_jwt":%q}`, refreshedJwt)
		case 2:
			http.Error(w, "not authorized", http.StatusUnauthorized)
		default:
			http.Error(w, "unexpected refresh", http.StatusInternalServerError)
		}
	}))
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
	settings.InitialLockTimeout = 0
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
	if got := receiveStringWithin(t, requests, "DeviceRemote refresh request was not observed"); got != "Bearer "+initialJwt {
		t.Fatalf("DeviceRemote refresh authorization = %q, want initial JWT", got)
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
	case <-deviceLoggedOut:
	case <-time.After(5 * time.Second):
		t.Fatal("DeviceRemote did not publish Api logout")
	}
	if got := receiveStringWithin(t, requests, "DeviceRemote rejection request was not observed"); got != "Bearer "+refreshedJwt {
		t.Fatalf("DeviceRemote rejection authorization = %q, want refreshed JWT", got)
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
