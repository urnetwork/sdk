package sdk

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// deviceTokenManager owns the logout decision, and it must be conservative:
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
) *deviceTokenManager {
	cancelCtx, cancel := context.WithCancel(ctx)
	clientStrategy := connect.NewClientStrategy(cancelCtx, connect.DefaultClientStrategySettings())
	api := newApi(cancelCtx, clientStrategy, apiUrl)
	api.SetByJwt("test-jwt")
	// construct directly (no run goroutine) so each case drives refreshToken
	// deterministically
	return &deviceTokenManager{
		ctx:              cancelCtx,
		cancel:           cancel,
		log:              connect.DefaultLogger(),
		api:              api,
		refreshMonitor:   connect.NewMonitor(),
		onTokenRefreshed: onTokenRefreshed,
		logout:           logout,
	}
}

func TestDeviceTokenManagerRefreshSemantics(t *testing.T) {
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
			manager := testingNewTokenManager(
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

			loggedOut, err := manager.refreshToken()

			connect.AssertEqual(t, loggedOut, c.expectLogout)
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
func TestDeviceTokenManagerRefreshOffline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// grab a port with nothing listening on it
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadUrl := ts.URL
	ts.Close()

	logoutCount := 0
	manager := testingNewTokenManager(
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

	loggedOut, err := manager.refreshToken()
	connect.AssertEqual(t, loggedOut, false)
	connect.AssertNotEqual(t, err, nil)
	connect.AssertEqual(t, logoutCount, 0)
}

// the run loop validates the stored jwt immediately at start, and a confirmed
// rejection logs out exactly once and stops the loop (no hot loop of
// refresh->logout against an invalid jwt)
func TestDeviceTokenManagerRunLogsOutOnceAtStart(t *testing.T) {
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
	api.SetByJwt("test-jwt")

	var logoutCount atomic.Int64
	manager := newDeviceTokenManager(
		cancelCtx,
		connect.DefaultLogger(),
		api,
		func(jwt string) {},
		func() error {
			logoutCount.Add(1)
			return nil
		},
	)
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
