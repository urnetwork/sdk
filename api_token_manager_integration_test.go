package sdk

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func receiveStringWithin(t *testing.T, values <-chan string, message string) string {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal(message)
		return ""
	}
}

func requireNoStringWithin(t *testing.T, values <-chan string, duration time.Duration, message string) {
	t.Helper()
	select {
	case value := <-values:
		t.Fatalf("%s: %q", message, value)
	case <-time.After(duration):
	}
}

func TestApiRefreshStartsWhenClientJwtIsInstalled(t *testing.T) {
	initialJwt := testingRefreshableJwtWithMarker(t, "installed-after-start")
	refreshedJwt := testingRefreshableJwtWithMarker(t, "installed-after-start-refreshed")
	requests := make(chan string, 2)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"by_jwt":%q}`, refreshedJwt)
	}))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := NewApi(ctx, connect.NewClientStrategyWithDefaults(ctx), ts.URL)
	defer api.Close()
	refreshed := make(chan string, 1)
	sub := api.AddJwtRefreshListener(jwtRefreshListenerFunc(func(jwt string) {
		refreshed <- jwt
	}))
	defer sub.Close()

	// Constructing an Api and installing a network-login credential must not
	// create background traffic. Network JWTs do not carry client/device ids
	// and cannot be refreshed by /auth/refresh.
	api.SetByJwt("network-login-jwt")
	requireNoStringWithin(t, requests, 100*time.Millisecond, "refresh ran before StartJwtRefresh")
	api.StartJwtRefresh()
	requireNoStringWithin(t, requests, 100*time.Millisecond, "network-login JWT was sent to /auth/refresh")

	// A client JWT installed after Start wakes the already-owned worker. This
	// is the standalone Api flow used by the miner and validator bootstrap.
	api.SetByJwt(initialJwt)
	if got := receiveStringWithin(t, requests, "client JWT did not start refresh"); got != "Bearer "+initialJwt {
		t.Fatalf("refresh authorization = %q, want bearer for installed client JWT", got)
	}
	if got := receiveStringWithin(t, refreshed, "client JWT refresh callback did not run"); got != refreshedJwt {
		t.Fatalf("refreshed JWT = %q, want %q", got, refreshedJwt)
	}
	if got := api.GetByJwt(); got != refreshedJwt {
		t.Fatalf("Api JWT = %q, want refreshed JWT", got)
	}

	// DeviceLocal and DeviceRemote may both enable the same Api. Repeated
	// starts must not turn into repeated immediate network requests.
	api.StartJwtRefresh()
	requireNoStringWithin(t, requests, 200*time.Millisecond, "repeated StartJwtRefresh scheduled another refresh")
}

func TestApiRefreshRequestSurvivesInFlightRefresh(t *testing.T) {
	initialJwt := testingRefreshableJwtWithMarker(t, "in-flight-initial")
	firstJwt := testingRefreshableJwtWithMarker(t, "in-flight-first")
	secondJwt := testingRefreshableJwtWithMarker(t, "in-flight-second")
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	requests := make(chan string, 3)
	var requestCount atomic.Int64

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestNumber := requestCount.Add(1)
		requests <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		switch requestNumber {
		case 1:
			close(firstStarted)
			<-releaseFirst
			fmt.Fprintf(w, `{"by_jwt":%q}`, firstJwt)
		case 2:
			fmt.Fprintf(w, `{"by_jwt":%q}`, secondJwt)
		default:
			http.Error(w, "unexpected refresh", http.StatusInternalServerError)
		}
	}))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := NewApi(ctx, connect.NewClientStrategyWithDefaults(ctx), ts.URL)
	defer api.Close()
	refreshed := make(chan string, 3)
	sub := api.AddJwtRefreshListener(jwtRefreshListenerFunc(func(jwt string) {
		refreshed <- jwt
	}))
	defer sub.Close()
	api.SetByJwt(initialJwt)
	api.StartJwtRefresh()

	select {
	case <-firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("initial refresh did not start")
	}
	api.RequestJwtRefresh()
	close(releaseFirst)

	if got := receiveStringWithin(t, refreshed, "first refresh did not finish"); got != firstJwt {
		t.Fatalf("first refreshed JWT = %q, want %q", got, firstJwt)
	}
	if got := receiveStringWithin(t, refreshed, "refresh requested in flight was lost"); got != secondJwt {
		t.Fatalf("second refreshed JWT = %q, want %q", got, secondJwt)
	}

	if got := receiveStringWithin(t, requests, "first refresh request was not observed"); got != "Bearer "+initialJwt {
		t.Fatalf("first authorization = %q, want initial JWT", got)
	}
	if got := receiveStringWithin(t, requests, "second refresh request was not observed"); got != "Bearer "+firstJwt {
		t.Fatalf("second authorization = %q, want first refreshed JWT", got)
	}
	if got := api.GetByJwt(); got != secondJwt {
		t.Fatalf("Api JWT = %q, want second refreshed JWT", got)
	}
	requireNoStringWithin(t, requests, 200*time.Millisecond, "in-flight request produced more than two refreshes")
}

func TestApiDiscardsRejectionForReplacedJwt(t *testing.T) {
	oldJwt := testingRefreshableJwtWithMarker(t, "rejected-old")
	newJwt := testingRefreshableJwtWithMarker(t, "replacement-login")
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		<-releaseRequest
		http.Error(w, "not authorized", http.StatusUnauthorized)
	}))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := NewApi(ctx, connect.NewClientStrategyWithDefaults(ctx), ts.URL)
	defer api.Close()
	api.SetByJwt(oldJwt)
	var logoutCount atomic.Int64
	sub := api.AddAuthLogoutListener(authLogoutListenerFunc(func() {
		logoutCount.Add(1)
	}))
	defer sub.Close()

	type refreshOutcome struct {
		loggedOut bool
		stale     bool
		err       error
	}
	outcome := make(chan refreshOutcome, 1)
	go func() {
		loggedOut, stale, err := api.tokenManager.refreshToken(oldJwt)
		outcome <- refreshOutcome{loggedOut: loggedOut, stale: stale, err: err}
	}()
	select {
	case <-requestStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh request did not start")
	}
	api.SetByJwt(newJwt)
	close(releaseRequest)

	select {
	case got := <-outcome:
		if got.loggedOut || !got.stale || got.err != nil {
			t.Fatalf("refresh outcome = %+v, want stale rejection", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stale rejection did not finish")
	}
	if got := api.GetByJwt(); got != newJwt {
		t.Fatalf("stale rejection cleared replacement JWT: got %q", got)
	}
	if got := logoutCount.Load(); got != 0 {
		t.Fatalf("stale rejection fired %d logout callbacks, want 0", got)
	}
}

func TestApiRefreshAndLogoutSubscriptions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := NewApi(ctx, connect.NewClientStrategyWithDefaults(ctx), "http://unused.invalid")
	defer api.Close()

	api.SetByJwt("initial")
	var refreshCount atomic.Int64
	refreshSub := api.AddJwtRefreshListener(jwtRefreshListenerFunc(func(jwt string) {
		if got := api.GetByJwt(); got != jwt {
			t.Errorf("refresh callback observed Api JWT %q, want %q", got, jwt)
		}
		refreshCount.Add(1)
	}))
	if !api.setRefreshedByJwt("initial", "refreshed") {
		t.Fatal("current JWT refresh was rejected as stale")
	}
	if got := refreshCount.Load(); got != 1 {
		t.Fatalf("refresh callbacks = %d, want 1", got)
	}
	refreshSub.Close()
	if !api.setRefreshedByJwt("refreshed", "refreshed-again") {
		t.Fatal("second current JWT refresh was rejected as stale")
	}
	if got := refreshCount.Load(); got != 1 {
		t.Fatalf("closed refresh subscription received callback; count = %d", got)
	}

	var logoutCount atomic.Int64
	logoutSub := api.AddAuthLogoutListener(authLogoutListenerFunc(func() {
		if got := api.GetByJwt(); got != "" {
			t.Errorf("logout callback observed non-empty Api JWT %q", got)
		}
		logoutCount.Add(1)
	}))
	if !api.rejectByJwt("refreshed-again") {
		t.Fatal("current JWT rejection was treated as stale")
	}
	if got := logoutCount.Load(); got != 1 {
		t.Fatalf("logout callbacks = %d, want 1", got)
	}
	logoutSub.Close()
	api.SetByJwt("replacement")
	if !api.rejectByJwt("replacement") {
		t.Fatal("replacement JWT rejection was treated as stale")
	}
	if got := logoutCount.Load(); got != 1 {
		t.Fatalf("closed logout subscription received callback; count = %d", got)
	}
}
