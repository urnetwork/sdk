package sdk

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// testingJwt builds an unsigned-but-parseable jwt with the given claims. The sdk
// only ever ParseUnverified's tokens, so a real signature is not needed to pin
// the scheduling behavior.
func testingJwt(claims map[string]any) string {
	encode := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			panic(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	header := encode(map[string]any{"alg": "HS256", "typ": "JWT"})
	body := encode(claims)
	return fmt.Sprintf("%s.%s.%s", header, body, base64.RawURLEncoding.EncodeToString([]byte("sig")))
}

// testingRefreshableScheduleJwt is a token the run loop will actually schedule
// against: `jwtCanRefresh` gates the loop on client_id AND device_id, so a jwt
// carrying only iat/exp parks the manager in its dormant branch and the
// scheduling tests below would pass for the wrong reason.
func testingRefreshableScheduleJwt(issued time.Time, lifetime time.Duration) string {
	return testingJwt(map[string]any{
		"iat":       issued.Unix(),
		"exp":       issued.Add(lifetime).Unix(),
		"user_id":   "11111111-1111-1111-1111-111111111111",
		"client_id": "33333333-3333-3333-3333-333333333333",
		"device_id": "44444444-4444-4444-4444-444444444444",
	})
}

// testingRunnableTokenManager builds a SECOND manager over the same Api, whose
// run() the test drives on its own goroutine so cancellation can be observed by
// joining it. The Api-owned manager stays dormant because these tests never
// call StartJwtRefresh, so it never competes for refreshes or inflates the
// counts below.
//
// `active` and `refreshPending` are set the way `Start` sets them: active, with
// the immediate first refresh armed. That is the zero-timeout path the
// cancellation half of this fix guards.
func testingRunnableTokenManager(ctx context.Context, api *Api) *apiTokenManager {
	cancelCtx, cancel := context.WithCancel(ctx)
	manager := &apiTokenManager{
		ctx:            cancelCtx,
		cancel:         cancel,
		api:            api,
		refreshMonitor: connect.NewMonitor(),
	}
	manager.active.Store(true)
	manager.refreshPending.Store(true)
	return manager
}

// TestJwtRefreshTimeoutNeverHotLoops is the regression pin for the refresh storm.
//
// The schedule used to be a FIXED 14-day lead subtracted from `exp`, calibrated
// to a 30-day server token. When the server shortened its lifetime to 24h, the
// lead exceeded the token's entire life and every computed timeout landed ~13
// days in the PAST. A non-positive timeout means no sleep, and because a
// successful refresh yields another 24h token, SUCCESS re-armed the same
// condition. One observed 22-minute service session logged 593 refreshes at a
// median 6.4ms apart.
//
// The invariant that matters is not the exact interval, it is that NO token the
// server can hand us -- of any lifetime, at any age, with any clock skew -- can
// produce a timeout that lets the loop spin.
func TestJwtRefreshTimeoutNeverHotLoops(t *testing.T) {
	now := time.Date(2026, 8, 11, 0, 7, 44, 0, time.UTC)

	cases := []struct {
		name   string
		claims map[string]any
	}{
		{
			// the exact shape that caused the storm: the server's current 24h token
			"24h token, freshly issued",
			map[string]any{"iat": now.Unix(), "exp": now.Add(24 * time.Hour).Unix()},
		},
		{
			// the shape the old 14-day lead was written for
			"30d token, freshly issued",
			map[string]any{"iat": now.Unix(), "exp": now.Add(30 * 24 * time.Hour).Unix()},
		},
		{"1h token", map[string]any{"iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}},
		{"1m token", map[string]any{"iat": now.Unix(), "exp": now.Add(time.Minute).Unix()}},
		{"token already past its half-life", map[string]any{"iat": now.Add(-20 * time.Hour).Unix(), "exp": now.Add(4 * time.Hour).Unix()}},
		{"already expired token", map[string]any{"iat": now.Add(-48 * time.Hour).Unix(), "exp": now.Add(-24 * time.Hour).Unix()}},
		{"no iat (pre-audit server)", map[string]any{"exp": now.Add(24 * time.Hour).Unix()}},
		{"iat in the future (skewed clock)", map[string]any{"iat": now.Add(time.Hour).Unix(), "exp": now.Add(24 * time.Hour).Unix()}},
		{"iat after exp (nonsense)", map[string]any{"iat": now.Add(48 * time.Hour).Unix(), "exp": now.Add(24 * time.Hour).Unix()}},
		{"exp at the epoch", map[string]any{"exp": 0}},
		{"no exp at all", map[string]any{"user_id": "u"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := jwtRefreshTimeout(testingJwt(c.claims), now)
			if got < minRefreshTimeout {
				t.Fatalf("timeout = %s, which is below the %s floor -- the loop can spin", got, minRefreshTimeout)
			}
		})
	}

	// the degenerate inputs must not spin either
	for _, jwt := range []string{"", "not-a-jwt", "a.b.c", "test-jwt"} {
		if got := jwtRefreshTimeout(jwt, now); got < minRefreshTimeout {
			t.Fatalf("timeout for %q = %s, below the %s floor", jwt, got, minRefreshTimeout)
		}
	}
}

// TestJwtRefreshTimeoutIsHalfLife pins the schedule itself: half of the token's
// own lifetime, measured from `iat`, which is what the server documents for sdk
// clients and is correct for whatever lifetime the server picks next.
func TestJwtRefreshTimeoutIsHalfLife(t *testing.T) {
	now := time.Date(2026, 8, 11, 0, 7, 44, 0, time.UTC)

	cases := []struct {
		lifetime time.Duration
		want     time.Duration
	}{
		{24 * time.Hour, 12 * time.Hour},
		{30 * 24 * time.Hour, 15 * 24 * time.Hour},
		{2 * time.Hour, time.Hour},
	}
	for _, c := range cases {
		jwt := testingJwt(map[string]any{"iat": now.Unix(), "exp": now.Add(c.lifetime).Unix()})
		got := jwtRefreshTimeout(jwt, now)
		if got != c.want {
			t.Fatalf("lifetime %s: timeout = %s, want %s", c.lifetime, got, c.want)
		}
	}

	// Halving must be of the token's REAL lifetime, not of the remaining time --
	// otherwise repeated passes collapse the interval geometrically toward the
	// floor. A 24h token read 6h in still refreshes at its 12h mark, i.e. in 6h.
	jwt := testingJwt(map[string]any{"iat": now.Unix(), "exp": now.Add(24 * time.Hour).Unix()})
	if got := jwtRefreshTimeout(jwt, now.Add(6*time.Hour)); got != 6*time.Hour {
		t.Fatalf("6h into a 24h token: timeout = %s, want 6h", got)
	}
}

// TestTokenManagerRunStopsOnCancel pins the second half of the storm: a CLOSED
// device must stop refreshing.
//
// The retry backoff was computed correctly and then discarded. `select { case
// <-ctx.Done(): return }` sat inside an anonymous func, so its `return` exited
// only the CLOSURE; `loggedOut` was false, so run() fell through to the outer
// loop, where the non-positive timeout skipped the only other ctx check and the
// refresh fired again. The tester's log shows this verbatim -- "Will retry in
// 128.69s" followed by the next attempt in the SAME millisecond, 297 times in
// 497ms against an already-cancelled client.
func TestTokenManagerRunStopsOnCancel(t *testing.T) {
	// always fails, so the manager stays on the retry path where the bug lived
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"message":"nope"}}`)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log := &countingLogger{}
	attempts := &log.refreshes
	_, api := testingNewTokenManager(ctx, server.URL, func(string) {}, func() error { return nil })
	api.setLog(log)
	// a real, current token: the schedule must not be what stops the loop
	api.SetByJwt(testingRefreshableScheduleJwt(time.Now(), 24*time.Hour))

	manager := testingRunnableTokenManager(ctx, api)

	done := make(chan struct{})
	go func() {
		defer close(done)
		manager.run()
	}()

	// let it get into the retry path
	time.Sleep(300 * time.Millisecond)
	manager.cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run() did not return within 5s of cancellation -- it is spinning on a closed device")
	}

	// and it must not have burned through attempts on the way out
	settled := attempts.Load()
	time.Sleep(200 * time.Millisecond)
	if after := attempts.Load(); after != settled {
		t.Fatalf("refresh attempts kept arriving after run() returned: %d -> %d", settled, after)
	}
	if 20 < settled {
		t.Fatalf("%d refresh attempts in ~300ms -- the backoff is being discarded", settled)
	}
}

// countingLogger counts the "refreshing the jwt now" line -- the exact
// quantity the tester's service log reports (593 in one 22-minute session), so
// the tests below measure the same thing the production evidence does. Counting
// http hits does NOT work: a cancelled ClientStrategy short-circuits before the
// request is ever sent, so a spinning loop is invisible at the server.
type countingLogger struct {
	refreshes atomic.Int64
}

func (self *countingLogger) Info(args ...any) {}
func (self *countingLogger) Infof(format string, args ...any) {
	if strings.Contains(format, "refreshing the jwt now") {
		self.refreshes.Add(1)
	}
}
func (self *countingLogger) Warningf(format string, args ...any) {}
func (self *countingLogger) Errorf(format string, args ...any)   {}
func (self *countingLogger) V(level int32) connect.Verbose       { return noopVerbose{} }

type noopVerbose struct{}

func (noopVerbose) Enabled() bool                    { return false }
func (noopVerbose) Info(args ...any)                 {}
func (noopVerbose) Infof(format string, args ...any) {}

// TestTokenManagerClosedDeviceDoesNotRefresh pins the second, independent half of
// the cancellation fix: the outer loop must observe cancellation on the
// ZERO-timeout path too.
//
// That path stays legitimately reachable after the scheduling fix -- the
// immediate first refresh takes it, and so does an explicit RefreshToken -- so
// the guarantee must not depend on the computed timeout being positive. Without
// the unconditional check, a device closed before run() got going still entered
// the refresh; with a non-positive schedule (the bug that shipped) it entered it
// without bound.
func TestTokenManagerClosedDeviceDoesNotRefresh(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log := &countingLogger{}
	_, api := testingNewTokenManager(ctx, server.URL, func(string) {}, func() error { return nil })
	api.setLog(log)
	api.SetByJwt(testingRefreshableScheduleJwt(time.Now(), 24*time.Hour))

	manager := testingRunnableTokenManager(ctx, api)

	// the device is closed BEFORE the loop starts: nothing it does can succeed,
	// so it must do nothing at all
	manager.cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		manager.run()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run() did not return on an already-cancelled ctx")
	}

	if got := log.refreshes.Load(); got != 0 {
		t.Fatalf("a closed device entered the refresh %d time(s); the zero-timeout path is not checking ctx", got)
	}
}

// TestTokenManagerRunSchedulesAfterSuccess pins the closed cycle itself: a
// SUCCESSFUL refresh must put the loop to sleep, not immediately re-arm it.
// Success was the trigger -- every completed refresh manufactured the exact
// condition that forced the next one.
func TestTokenManagerRunSchedulesAfterSuccess(t *testing.T) {
	var refreshes atomic.Int64

	// mints a fresh 24h token every time, exactly like the live server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		// a fresh 24h token every time, exactly like the live server: still
		// refreshable, so only the SCHEDULE can stop the loop
		jwt := testingRefreshableScheduleJwt(time.Now(), 24*time.Hour)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"by_jwt":%q}`, jwt)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, api := testingNewTokenManager(ctx, server.URL, func(jwt string) {}, func() error { return nil })
	api.SetByJwt(testingRefreshableScheduleJwt(time.Now(), 24*time.Hour))

	manager := testingRunnableTokenManager(ctx, api)

	done := make(chan struct{})
	go func() {
		defer close(done)
		manager.run()
	}()

	time.Sleep(1500 * time.Millisecond)
	got := refreshes.Load()
	manager.cancel()
	<-done

	// exactly one: the immediate first refresh. The next is 12h away.
	if got != 1 {
		t.Fatalf("%d refreshes in 1.5s, want 1 (the immediate first) -- success is re-arming the loop", got)
	}
}
