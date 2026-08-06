package sdk

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect"
)

// Api's token manager owns the logout decision, and it must be conservative:
// only a confirmed api response that rejects the credential logs out — a 200
// with an error result (e.g. "client no longer exists") or a 401 from the
// auth layer. Api outages (5xx, even when the body is a json error payload
// like the TemporarilyUnavailable wrapper emits), offline networks, and
// timeouts must retry without ever touching the auth state.

func testingNewTokenManager(
	ctx context.Context,
	apiUrl string,
	onTokenRefreshed func(string),
	logout func() error,
) (*apiTokenManager, *Api) {
	clientStrategy := connect.NewClientStrategy(ctx, connect.DefaultClientStrategySettings())
	api := newApi(ctx, clientStrategy, apiUrl)
	api.AddJwtRefreshListener(jwtRefreshListenerFunc(onTokenRefreshed))
	api.AddAuthLogoutListener(authLogoutListenerFunc(func() {
		_ = logout()
	}))
	api.SetByJwt("test-jwt")
	return api.tokenManager, api
}

func testingRefreshableJwt(t *testing.T) string {
	return testingRefreshableJwtWithMarker(t, "default")
}

func testingRefreshableJwtWithMarker(t *testing.T, marker string) string {
	t.Helper()
	token, err := gojwt.NewWithClaims(gojwt.SigningMethodNone, gojwt.MapClaims{
		"client_id": "00000000-0000-0000-0000-000000000001",
		"device_id": "00000000-0000-0000-0000-000000000002",
		"exp":       time.Now().Add(30 * 24 * time.Hour).Unix(),
		"marker":    marker,
	}).SignedString(gojwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestApiOwnsRefreshLifecycle(t *testing.T) {
	initialJwt := testingRefreshableJwtWithMarker(t, "initial")
	firstRefreshJwt := testingRefreshableJwtWithMarker(t, "first")
	secondRefreshJwt := testingRefreshableJwtWithMarker(t, "second")

	var requestCount atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := requestCount.Add(1)
		var expectedJwt string
		var responseJwt string
		switch request {
		case 1:
			expectedJwt = initialJwt
			responseJwt = firstRefreshJwt
		case 2:
			expectedJwt = firstRefreshJwt
			responseJwt = secondRefreshJwt
		default:
			t.Errorf("unexpected refresh request %d", request)
			http.Error(w, "unexpected", http.StatusInternalServerError)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+expectedJwt {
			t.Errorf("request %d authorization = %q, want refreshed bearer", request, got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"by_jwt":%q}`, responseJwt)
	}))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	strategy := connect.NewClientStrategyWithDefaults(ctx)
	api := NewApi(ctx, strategy, ts.URL)
	defer api.Close()
	refreshed := make(chan string, 2)
	api.AddJwtRefreshListener(jwtRefreshListenerFunc(func(jwt string) {
		refreshed <- jwt
	}))
	api.SetByJwt(initialJwt)
	api.StartJwtRefresh()

	select {
	case got := <-refreshed:
		connect.AssertEqual(t, got, firstRefreshJwt)
	case <-time.After(5 * time.Second):
		t.Fatal("API-owned startup refresh did not run")
	}
	connect.AssertEqual(t, api.GetByJwt(), firstRefreshJwt)

	api.RequestJwtRefresh()
	select {
	case got := <-refreshed:
		connect.AssertEqual(t, got, secondRefreshJwt)
	case <-time.After(5 * time.Second):
		t.Fatal("API-owned manual refresh did not run")
	}
	connect.AssertEqual(t, api.GetByJwt(), secondRefreshJwt)
	connect.AssertEqual(t, requestCount.Load(), int64(2))
}

func TestApiDiscardsRefreshForReplacedJwt(t *testing.T) {
	oldJwt := testingRefreshableJwtWithMarker(t, "old")
	newLoginJwt := testingRefreshableJwtWithMarker(t, "new-login")
	staleRefreshJwt := testingRefreshableJwtWithMarker(t, "stale-refresh")
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		<-releaseRequest
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"by_jwt":%q}`, staleRefreshJwt)
	}))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := NewApi(ctx, connect.NewClientStrategyWithDefaults(ctx), ts.URL)
	defer api.Close()
	api.SetByJwt(oldJwt)

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
	<-requestStarted
	api.SetByJwt(newLoginJwt)
	close(releaseRequest)
	got := <-outcome
	connect.AssertEqual(t, got.loggedOut, false)
	connect.AssertEqual(t, got.stale, true)
	connect.AssertEqual(t, got.err, nil)
	connect.AssertEqual(t, api.GetByJwt(), newLoginJwt)
}

func TestApiTokenManagerRefreshSemantics(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type serverCase struct {
		name         string
		handler      http.HandlerFunc
		expectLogout bool
		expectErr    bool
		expectJwt    string
	}

	cases := []serverCase{
		{
			// successful refresh rotates the token
			name: "success",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"by_jwt":"refreshed-jwt"}`))
			},
			expectJwt: "refreshed-jwt",
		},
		{
			// confirmed rejection in the result payload (e.g. the client was
			// removed): the one true logout path
			name: "result error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"error":{"message":"Client does not exist"}}`))
			},
			expectLogout: true,
		},
		{
			// the auth layer rejecting the jwt itself (expired/unparseable)
			// is also confirmed invalid
			name: "401",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "Not authorized.", http.StatusUnauthorized)
			},
			expectLogout: true,
		},
		{
			// an outage body in the exact json error shape of the api must
			// NOT be mistaken for a refresh rejection
			name: "503 with json error body",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				w.Write([]byte(`{"error":{"message":"This api is temporarily unavailable."}}`))
			},
			expectErr: true,
		},
		{
			// plain api failure
			name: "500",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "boom", http.StatusInternalServerError)
			},
			expectErr: true,
		},
		{
			// waf/proxy blocks use 403; ambiguous, so never a logout
			name: "403",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "blocked", http.StatusForbidden)
			},
			expectErr: true,
		},
		{
			// a 200 that is not the api (e.g. captive portal html) is a
			// parse failure, not a logout
			name: "non-json 200",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte("<html>welcome to the lobby wifi</html>"))
			},
			expectErr: true,
		},
	}

	for _, c := range cases {
		func() {
			ts := httptest.NewServer(c.handler)
			defer ts.Close()

			logoutCount := 0
			refreshedJwt := ""
			manager, api := testingNewTokenManager(
				ctx,
				ts.URL,
				func(jwt string) {
					refreshedJwt = jwt
				},
				func() error {
					logoutCount += 1
					return nil
				},
			)
			defer manager.Close()

			loggedOut, stale, err := manager.refreshToken(api.GetByJwt())

			connect.AssertEqual(t, loggedOut, c.expectLogout)
			connect.AssertEqual(t, stale, false)
			if c.expectLogout {
				connect.AssertEqual(t, logoutCount, 1)
				connect.AssertEqual(t, err, nil)
			} else {
				connect.AssertEqual(t, logoutCount, 0)
			}
			if c.expectErr {
				connect.AssertNotEqual(t, err, nil)
			}
			connect.AssertEqual(t, refreshedJwt, c.expectJwt)
			if c.expectJwt != "" {
				connect.AssertEqual(t, err, nil)
			}
		}()
	}
}

// an offline network (nothing listening) is a transient error: retry, no
// logout, and the attempt respects the manager ctx so it cannot hang past
// close
func TestApiTokenManagerRefreshOffline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// grab a port with nothing listening on it
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadUrl := ts.URL
	ts.Close()

	logoutCount := 0
	manager, api := testingNewTokenManager(
		ctx,
		deadUrl,
		func(jwt string) {},
		func() error {
			logoutCount += 1
			return nil
		},
	)
	defer manager.Close()

	// bound the attempt so the test does not wait out the full strategy
	// timeouts against the dead endpoint
	go func() {
		select {
		case <-time.After(5 * time.Second):
			manager.cancel()
		case <-manager.ctx.Done():
		}
	}()

	loggedOut, stale, err := manager.refreshToken(api.GetByJwt())
	connect.AssertEqual(t, loggedOut, false)
	connect.AssertEqual(t, stale, false)
	connect.AssertNotEqual(t, err, nil)
	connect.AssertEqual(t, logoutCount, 0)
}

// the run loop validates the stored jwt immediately at start, and a confirmed
// rejection logs out exactly once and stops the loop (no hot loop of
// refresh->logout against an invalid jwt)
func TestApiTokenManagerRunLogsOutOnceAtStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var requestCount atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"error":{"message":"Client does not exist"}}`))
	}))
	defer ts.Close()

	cancelCtx, managerCancel := context.WithCancel(ctx)
	defer managerCancel()
	clientStrategy := connect.NewClientStrategy(cancelCtx, connect.DefaultClientStrategySettings())
	api := newApi(cancelCtx, clientStrategy, ts.URL)

	var logoutCount atomic.Int64
	api.AddAuthLogoutListener(authLogoutListenerFunc(func() {
		logoutCount.Add(1)
	}))
	api.SetByJwt(testingRefreshableJwt(t))
	api.StartJwtRefresh()
	manager := api.tokenManager
	defer manager.Close()

	// the startup refresh fires without waiting for the expiration window
	endTime := time.Now().Add(5 * time.Second)
	for logoutCount.Load() == 0 && time.Now().Before(endTime) {
		time.Sleep(10 * time.Millisecond)
	}
	connect.AssertEqual(t, logoutCount.Load(), int64(1))

	// the loop stopped: no further refresh attempts
	settledRequestCount := requestCount.Load()
	time.Sleep(300 * time.Millisecond)
	connect.AssertEqual(t, requestCount.Load(), settledRequestCount)
	connect.AssertEqual(t, logoutCount.Load(), int64(1))
}
