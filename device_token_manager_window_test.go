package sdk

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The refresh window (apiTokenRefreshSettings): each client token refreshes at
// its point from 40% to 50% of its lifetime, a hash of its id that is uniform
// across tokens, never later than its half-life, never sooner than the floor
// after the previous attempt, and a failed refresh retries after a uniform
// wait under 15 minutes. At exactly the half-life, clients that refreshed
// together kept refreshing in lockstep every cycle (owner decision,
// 2026-11-01). Tests install their own points through refreshDraw.

const testingWindowDay = 24 * time.Hour

// A refreshable client token issued at `issued`. The marker keeps tokens
// minted in the same second distinct, as the server's `jti` does.
func testingWindowClientJwt(issued time.Time, lifetime time.Duration, marker string) string {
	return testingJwt(map[string]any{
		"iat":       issued.Unix(),
		"exp":       issued.Add(lifetime).Unix(),
		"user_id":   "11111111-1111-1111-1111-111111111111",
		"client_id": "33333333-3333-3333-3333-333333333333",
		"device_id": "44444444-4444-4444-4444-444444444444",
		"marker":    marker,
	})
}

// Compares instants: token claims parse into the local zone.
func requireTimeEqual(t *testing.T, got time.Time, want time.Time, message string) {
	t.Helper()
	if !got.Equal(want) {
		t.Fatalf("%s: got %s, want %s", message, got, want)
	}
}

// Allows the float rounding of a fraction of a lifetime.
func requireDurationNear(t *testing.T, got time.Duration, want time.Duration, message string) {
	t.Helper()
	if diff := got - want; diff < -time.Microsecond || time.Microsecond < diff {
		t.Fatalf("%s: got %s, want %s", message, got, want)
	}
}

// The window is the owner's decision: 40% to 50% of the lifetime, a 5-minute
// floor, the weekly no-expiration fallback, and the unchanged 15-minute retry.
func TestApiTokenRefreshSettingsDefaults(t *testing.T) {
	settings := defaultApiTokenRefreshSettings()
	connect.AssertEqual(t, settings.RefreshWindowStart, 0.4)
	connect.AssertEqual(t, settings.RefreshWindowEnd, 0.5)
	connect.AssertEqual(t, settings.MinRefreshTimeout, 5*time.Minute)
	connect.AssertEqual(t, settings.NoExpirationRefreshTimeout, 7*24*time.Hour)
	connect.AssertEqual(t, settings.RefreshRetryTimeout, 15*time.Minute)

	// every API's workers read them
	_, api := newTestApiForURL(t, "https://window.example")
	connect.AssertEqual(t, *api.tokenManager.settings, *settings)
	connect.AssertEqual(t, *api.networkRenewer.settings, *settings)
}

// A draw maps linearly onto the window, from 40% of the token's lifetime at
// draw 0 to 50% at draw 1, measured from `iat`; a draw out of range is clamped
// into the window rather than past it.
func TestJwtRefreshTimeDrawsWithinTheWindow(t *testing.T) {
	now := time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC)
	settings := defaultApiTokenRefreshSettings()
	lifetimes := []time.Duration{30 * testingWindowDay, testingWindowDay, 2 * time.Hour, 10 * time.Minute, time.Minute}
	for _, lifetime := range lifetimes {
		byJwt := testingWindowClientJwt(now, lifetime, "window")
		windowStart := now.Add(time.Duration(0.4 * float64(lifetime)))
		halfLife := now.Add(lifetime / 2)
		for _, draw := range []float64{0, 0.1, 0.25, 0.5, 0.75, 0.999999, 1} {
			got := jwtRefreshTime(byJwt, now, draw, settings)
			want := time.Duration((0.4 + 0.1*draw) * float64(lifetime))
			requireDurationNear(t, got.Sub(now), want, fmt.Sprintf("lifetime %s draw %v", lifetime, draw))
			if got.Before(windowStart) || halfLife.Before(got) {
				t.Fatalf("lifetime %s draw %v: refresh at %s, outside [%s, %s]", lifetime, draw, got, windowStart, halfLife)
			}
		}
		for _, draw := range []float64{-1, math.Inf(-1), math.NaN()} {
			requireTimeEqual(t, jwtRefreshTime(byJwt, now, draw, settings), windowStart, fmt.Sprintf("lifetime %s draw %v", lifetime, draw))
		}
		for _, draw := range []float64{1.5, math.Inf(1)} {
			requireTimeEqual(t, jwtRefreshTime(byJwt, now, draw, settings), halfLife, fmt.Sprintf("lifetime %s draw %v", lifetime, draw))
		}
	}
}

// A client token with an id (`jti`), as the server mints each token.
func testingWindowClientJwtWithId(issued time.Time, lifetime time.Duration, id string) string {
	return testingJwt(map[string]any{
		"iat":       issued.Unix(),
		"exp":       issued.Add(lifetime).Unix(),
		"user_id":   "11111111-1111-1111-1111-111111111111",
		"client_id": "33333333-3333-3333-3333-333333333333",
		"device_id": "44444444-4444-4444-4444-444444444444",
		"jti":       id,
	})
}

// A synthetic token id, the n-th of a run.
func testingWindowTokenId(n int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", n)
}

// The production draw, with no hook installed, takes each token's point from
// its id: a cohort that refreshed together, 2000 tokens issued in the same
// second, spreads over the whole window, every point in [40%, 50%) of the
// lifetime.
func TestJwtRefreshDefaultDrawStaysWithinTheWindow(t *testing.T) {
	now := time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC)
	lifetime := 30 * testingWindowDay
	manager := &apiTokenManager{settings: defaultApiTokenRefreshSettings()}
	windowStart := now.Add(time.Duration(0.4 * float64(lifetime)))
	halfLife := now.Add(lifetime / 2)
	earliest := halfLife
	latest := windowStart
	for i := 0; i < 2000; i += 1 {
		byJwt := testingWindowClientJwtWithId(now, lifetime, testingWindowTokenId(i))
		draw := manager.drawRefreshPoint(byJwt)
		if draw < 0 || 1 <= draw {
			t.Fatalf("draw %v is outside [0, 1)", draw)
		}
		refreshTime := jwtRefreshTime(byJwt, now, draw, manager.settings)
		if refreshTime.Before(windowStart) || !refreshTime.Before(halfLife) {
			t.Fatalf("refresh at %s, outside [%s, %s)", refreshTime, windowStart, halfLife)
		}
		if refreshTime.Before(earliest) {
			earliest = refreshTime
		}
		if latest.Before(refreshTime) {
			latest = refreshTime
		}
	}
	// at exactly the half-life every client fell on the same instant
	if latest.Sub(earliest) < 2*testingWindowDay+23*time.Hour {
		t.Fatalf("2000 tokens spread over only %s of the 3-day window", latest.Sub(earliest))
	}
}

// A token's point is a function of the token: any process, and the same one
// after a restart, takes the same point for it, so a restart neither renews a
// token early nor makes a burst. The id decides: tokens with other ids take
// other points, and claims beside the id do not move it. A token without an
// id takes its point from its signature, and one that does not parse from its
// whole text.
func TestJwtRefreshDrawIsTheTokens(t *testing.T) {
	issued := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	lifetime := 30 * testingWindowDay
	byJwt := testingWindowClientJwtWithId(issued, lifetime, testingWindowTokenId(1))
	first := &apiTokenManager{settings: defaultApiTokenRefreshSettings()}
	restarted := &apiTokenManager{settings: defaultApiTokenRefreshSettings()}
	connect.AssertEqual(t, restarted.drawRefreshPoint(byJwt), first.drawRefreshPoint(byJwt))
	connect.AssertEqual(t, jwtRefreshDraw(byJwt), jwtRefreshDraw(byJwt))

	other := testingWindowClientJwtWithId(issued, lifetime, testingWindowTokenId(2))
	if jwtRefreshDraw(other) == jwtRefreshDraw(byJwt) {
		t.Fatal("tokens with different ids took the same point")
	}
	// the id alone decides
	reissued := testingWindowClientJwtWithId(issued.Add(time.Hour), lifetime, testingWindowTokenId(1))
	connect.AssertEqual(t, jwtRefreshDraw(reissued), jwtRefreshDraw(byJwt))

	// without an id, the signature
	header, body, _ := strings.Cut(testingJwt(map[string]any{"exp": issued.Add(lifetime).Unix()}), ".")
	body, _, _ = strings.Cut(body, ".")
	signedA := header + "." + body + ".c2lnbmF0dXJlLWE"
	signedB := header + "." + body + ".c2lnbmF0dXJlLWI"
	if jwtRefreshDraw(signedA) == jwtRefreshDraw(signedB) {
		t.Fatal("tokens with different signatures and no id took the same point")
	}
	connect.AssertEqual(t, jwtRefreshDraw(signedA), jwtRefreshDraw(signedA))
	// not a jwt: its text
	for _, text := range []string{"", "not-a-jwt", "a.b.c"} {
		draw := jwtRefreshDraw(text)
		if draw < 0 || 1 <= draw {
			t.Fatalf("%q: draw %v is outside [0, 1)", text, draw)
		}
		connect.AssertEqual(t, jwtRefreshDraw(text), draw)
	}
}

// Points are uniform over tokens: 10000 ids fill 20 equal bins of [0, 1)
// evenly (chi-square below 43.8, the 0.001 critical value for 19 degrees of
// freedom), and reach both ends.
func TestJwtRefreshDrawIsUniformOverTokens(t *testing.T) {
	issued := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	lifetime := 30 * testingWindowDay
	const tokenCount = 10000
	const binCount = 20
	bins := make([]int, binCount)
	low, high := 1.0, 0.0
	for i := 0; i < tokenCount; i += 1 {
		draw := jwtRefreshDraw(testingWindowClientJwtWithId(issued, lifetime, testingWindowTokenId(i)))
		if draw < 0 || 1 <= draw {
			t.Fatalf("draw %v is outside [0, 1)", draw)
		}
		bins[int(draw*binCount)] += 1
		low = min(low, draw)
		high = max(high, draw)
	}
	expected := float64(tokenCount) / binCount
	chiSquare := 0.0
	for _, count := range bins {
		chiSquare += (float64(count) - expected) * (float64(count) - expected) / expected
	}
	if 43.8 < chiSquare {
		t.Fatalf("points are not uniform over tokens: chi-square %.1f, bins %v", chiSquare, bins)
	}
	if 0.002 < low || high < 0.998 {
		t.Fatalf("points span only [%v, %v]", low, high)
	}
}

// The point is of the token's real lifetime, from `iat`, not of what remains:
// a 24h token read 6h in refreshes 9.6h to 12h after its `iat`. Without `iat`,
// the lifetime counts from now.
func TestJwtRefreshTimeIsOfTheTokensLifetime(t *testing.T) {
	issued := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	now := issued.Add(6 * time.Hour)
	settings := defaultApiTokenRefreshSettings()
	byJwt := testingWindowClientJwt(issued, testingWindowDay, "read-later")
	requireDurationNear(t, jwtRefreshTimeout(jwtRefreshTime(byJwt, now, 0, settings), time.Time{}, now, settings), 3*time.Hour+36*time.Minute, "draw 0")
	requireDurationNear(t, jwtRefreshTimeout(jwtRefreshTime(byJwt, now, 1, settings), time.Time{}, now, settings), 6*time.Hour, "draw 1")

	noIat := testingJwt(map[string]any{"exp": now.Add(10 * time.Hour).Unix(), "client_id": "c", "device_id": "d"})
	requireDurationNear(t, jwtRefreshTime(noIat, now, 0, settings).Sub(now), 4*time.Hour, "no iat, draw 0")
	requireDurationNear(t, jwtRefreshTime(noIat, now, 1, settings).Sub(now), 5*time.Hour, "no iat, draw 1")

	// no readable exp: the weekly fallback
	for _, byJwt := range []string{"", "not-a-jwt", testingJwt(map[string]any{"client_id": "c", "device_id": "d"})} {
		requireTimeEqual(t, jwtRefreshTime(byJwt, now, 0.5, settings), now.Add(settings.NoExpirationRefreshTimeout), byJwt)
	}
}

// No scheduled refresh falls after the half-life, at any draw, for a token
// loaded at any point of its life, unless the previous attempt was less than
// the floor ago. A token loaded past its half-life refreshes now.
func TestJwtRefreshNeverLaterThanHalfLife(t *testing.T) {
	issued := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	settings := defaultApiTokenRefreshSettings()
	for _, lifetime := range []time.Duration{30 * testingWindowDay, testingWindowDay, 2 * time.Hour, 20 * time.Minute} {
		byJwt := testingWindowClientJwt(issued, lifetime, "half-life")
		halfLife := issued.Add(lifetime / 2)
		for _, loadedAt := range []float64{0, 0.1, 0.39, 0.4, 0.42, 0.45, 0.49, 0.4999, 0.5, 0.6, 0.99, 1, 1.5} {
			now := issued.Add(time.Duration(loadedAt * float64(lifetime)))
			for _, draw := range []float64{0, 0.5, 0.999999, 1, 2} {
				refreshTime := jwtRefreshTime(byJwt, now, draw, settings)
				// no attempt yet, or one long before
				for _, previousAttemptTime := range []time.Time{{}, now.Add(-settings.MinRefreshTimeout)} {
					timeout := jwtRefreshTimeout(refreshTime, previousAttemptTime, now, settings)
					if timeout < 0 {
						t.Fatalf("lifetime %s loaded at %v draw %v: negative timeout %s", lifetime, loadedAt, draw, timeout)
					}
					due := now.Add(timeout)
					if now.After(halfLife) {
						if timeout != 0 {
							t.Fatalf("lifetime %s loaded past its half-life (%v) draw %v: waits %s, want now", lifetime, loadedAt, draw, timeout)
						}
					} else if due.After(halfLife) {
						t.Fatalf("lifetime %s loaded at %v draw %v: refresh at %s, after the half-life %s", lifetime, loadedAt, draw, due, halfLife)
					}
				}
			}
		}
	}
}

// A token loaded past its drawn point refreshes promptly: now when there was no
// attempt for at least the floor, else when the floor ends. It never waits for
// a later point of the window, and never spins.
func TestJwtRefreshTokenPastItsPointRefreshesPromptly(t *testing.T) {
	issued := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	lifetime := 30 * testingWindowDay
	settings := defaultApiTokenRefreshSettings()
	byJwt := testingWindowClientJwt(issued, lifetime, "past-its-point")
	// loaded at 45%, with its point at 40%
	now := issued.Add(time.Duration(0.45 * float64(lifetime)))
	refreshTime := jwtRefreshTime(byJwt, now, 0, settings)
	connect.AssertEqual(t, jwtRefreshTimeout(refreshTime, time.Time{}, now, settings), time.Duration(0))
	connect.AssertEqual(t, jwtRefreshTimeout(refreshTime, now.Add(-time.Hour), now, settings), time.Duration(0))
	connect.AssertEqual(t, jwtRefreshTimeout(refreshTime, now.Add(-time.Minute), now, settings), 4*time.Minute)
	// with its point at 48% it waits for the point, inside the window
	requireDurationNear(t, jwtRefreshTimeout(jwtRefreshTime(byJwt, now, 0.8, settings), time.Time{}, now, settings), time.Duration(0.03*float64(lifetime)), "draw 0.8")
}

// A lifetime shorter than twice the floor cannot refresh by its half-life
// without refreshing more often than the floor allows: the floor wins, so such
// tokens refresh every MinRefreshTimeout, promptly and never in a loop. A
// longer lifetime keeps its drawn point.
func TestJwtRefreshShortLifetimeIsHeldByTheFloor(t *testing.T) {
	now := time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC)
	settings := defaultApiTokenRefreshSettings()
	for _, lifetime := range []time.Duration{time.Second, time.Minute, 2 * time.Minute, 9 * time.Minute} {
		byJwt := testingWindowClientJwt(now, lifetime, "short")
		for _, draw := range []float64{0, 0.5, 1} {
			// just fetched by the attempt that began now
			got := jwtRefreshTimeout(jwtRefreshTime(byJwt, now, draw, settings), now, now, settings)
			connect.AssertEqual(t, got, settings.MinRefreshTimeout)
		}
	}
	byJwt := testingWindowClientJwt(now, 20*time.Minute, "longer")
	connect.AssertEqual(t, jwtRefreshTimeout(jwtRefreshTime(byJwt, now, 0, settings), now, now, settings), 8*time.Minute)
	connect.AssertEqual(t, jwtRefreshTimeout(jwtRefreshTime(byJwt, now, 1, settings), now, now, settings), 10*time.Minute)
}

// A failed refresh retries after a wait drawn uniformly from [0, 15 minutes),
// as before the window: the retry is not part of the schedule.
func TestTokenManagerRetryDrawIsUnchanged(t *testing.T) {
	manager := &apiTokenManager{settings: defaultApiTokenRefreshSettings()}
	latest := time.Duration(0)
	for i := 0; i < 10000; i += 1 {
		retryTimeout := manager.nextRetryTimeout()
		if retryTimeout < 0 || 15*time.Minute <= retryTimeout {
			t.Fatalf("retry timeout %s is outside [0, 15m)", retryTimeout)
		}
		latest = max(latest, retryTimeout)
	}
	if latest < 10*time.Minute {
		t.Fatalf("10000 retry draws reached only %s of the 15 minutes", latest)
	}
}

// Drives the API's own token manager on a controlled clock. Each /auth/refresh
// request is answered by answer, which gets the request's number from 1 and
// the clock's now; each window draw is the next of draws.
type testingWindowRefreshes struct {
	api       *Api
	clock     *renewalTestClock
	requests  atomic.Int64
	drawCount atomic.Int64
}

func newTestingWindowRefreshes(
	t *testing.T,
	initialByJwt string,
	draws []float64,
	retryTimeout func() time.Duration,
	answer func(request int64, now time.Time) ([]byte, error),
) *testingWindowRefreshes {
	t.Helper()
	_, api := newTestApiForURL(t, "https://window.example")
	self := &testingWindowRefreshes{
		api:   api,
		clock: newRenewalTestClock(),
	}
	var drawLock sync.Mutex
	api.tokenManager.clock = self.clock
	api.tokenManager.refreshDraw = func(string) float64 {
		drawLock.Lock()
		defer drawLock.Unlock()
		i := self.drawCount.Add(1) - 1
		if int64(len(draws)) <= i {
			t.Errorf("window draw %d, but the test has %d", i+1, len(draws))
			return 0
		}
		return draws[i]
	}
	api.tokenManager.retryTimeout = retryTimeout
	api.setHttpGetRaw(func(ctx context.Context, requestUrl string, byJwt string) ([]byte, error) {
		if requestUrl != "https://window.example/auth/refresh" {
			return nil, fmt.Errorf("unexpected request %q", requestUrl)
		}
		return answer(self.requests.Add(1), self.clock.Now())
	})
	api.SetByJwt(initialByJwt)
	api.StartJwtRefresh()
	return self
}

// answers a fresh client token of the lifetime, issued at the clock's now
func testingWindowAnswer(lifetime time.Duration) func(request int64, now time.Time) ([]byte, error) {
	return func(request int64, now time.Time) ([]byte, error) {
		return []byte(fmt.Sprintf(`{"by_jwt":%q}`, testingWindowClientJwt(now, lifetime, fmt.Sprintf("refresh-%d", request)))), nil
	}
}

// Every refresh installs a new token, which draws its own point: refreshes of
// 30-day tokens wait 12, 15, 13.5 and 12 days for draws 0, 1, 0.5 and 0.
// A wake that changes nothing keeps the point it has.
func TestTokenManagerRedrawsEachRefresh(t *testing.T) {
	lifetime := 30 * testingWindowDay
	initial := testingWindowClientJwt(renewalTestBaseTime.Add(-time.Hour), lifetime, "initial")
	r := newTestingWindowRefreshes(t, initial, []float64{0, 1, 0.5, 0}, nil, testingWindowAnswer(lifetime))

	// the immediate refresh at start, then the first token's point
	r.clock.requireArmed(t, 12*testingWindowDay)
	connect.AssertEqual(t, r.requests.Load(), int64(1))
	connect.AssertEqual(t, r.drawCount.Load(), int64(1))

	// a wake that changes nothing: the same point, no new draw
	r.clock.Advance(testingWindowDay)
	r.api.tokenManager.refreshMonitor.NotifyAll()
	r.clock.requireArmed(t, 11*testingWindowDay)
	connect.AssertEqual(t, r.drawCount.Load(), int64(1))
	connect.AssertEqual(t, r.requests.Load(), int64(1))

	r.clock.Advance(11 * testingWindowDay)
	r.clock.requireArmed(t, 15*testingWindowDay)
	connect.AssertEqual(t, r.requests.Load(), int64(2))
	connect.AssertEqual(t, r.drawCount.Load(), int64(2))

	r.clock.Advance(15 * testingWindowDay)
	r.clock.requireArmed(t, 13*testingWindowDay+12*time.Hour)
	connect.AssertEqual(t, r.requests.Load(), int64(3))
	connect.AssertEqual(t, r.drawCount.Load(), int64(3))

	r.clock.Advance(13*testingWindowDay + 12*time.Hour)
	r.clock.requireArmed(t, 12*testingWindowDay)
	connect.AssertEqual(t, r.requests.Load(), int64(4))
	connect.AssertEqual(t, r.drawCount.Load(), int64(4))
}

// A failed refresh waits the retry draw, not the schedule and not the floor,
// then the refreshed token draws its point.
func TestTokenManagerFailedRefreshRetryIsUnchanged(t *testing.T) {
	lifetime := 30 * testingWindowDay
	initial := testingWindowClientJwt(renewalTestBaseTime.Add(-time.Hour), lifetime, "initial")
	retryTimeouts := []time.Duration{7 * time.Minute, time.Second}
	var retries atomic.Int64
	retryTimeout := func() time.Duration {
		return retryTimeouts[retries.Add(1)-1]
	}
	succeed := testingWindowAnswer(lifetime)
	r := newTestingWindowRefreshes(t, initial, []float64{0}, retryTimeout, func(request int64, now time.Time) ([]byte, error) {
		if request <= 2 {
			return nil, errors.New("refresh transport failed")
		}
		return succeed(request, now)
	})

	r.clock.requireArmed(t, 7*time.Minute)
	r.clock.Advance(7 * time.Minute)
	// a retry sooner than the floor: retries are not held by it
	r.clock.requireArmed(t, time.Second)
	r.clock.Advance(time.Second)
	r.clock.requireArmed(t, 12*testingWindowDay)
	connect.AssertEqual(t, r.requests.Load(), int64(3))
	connect.AssertEqual(t, retries.Load(), int64(2))
	connect.AssertEqual(t, r.drawCount.Load(), int64(1))
}

// Short lifetimes, and tokens already past their point when they arrive (a
// clock far ahead of the server's), refresh every MinRefreshTimeout: promptly,
// and never in a loop.
func TestTokenManagerShortLifetimeRefreshesAreSpaced(t *testing.T) {
	for _, c := range []struct {
		lifetime time.Duration
		// how long before the clock's now the server issues each token
		age time.Duration
	}{
		{lifetime: 2 * time.Minute},
		{lifetime: time.Second},
		{lifetime: 30 * testingWindowDay, age: 16 * testingWindowDay},
		{lifetime: 30 * testingWindowDay, age: 40 * testingWindowDay},
	} {
		initial := testingWindowClientJwt(renewalTestBaseTime, c.lifetime, "initial")
		r := newTestingWindowRefreshes(t, initial, []float64{0, 0.5, 1, 0}, nil, func(request int64, now time.Time) ([]byte, error) {
			byJwt := testingWindowClientJwt(now.Add(-c.age), c.lifetime, fmt.Sprintf("refresh-%d", request))
			return []byte(fmt.Sprintf(`{"by_jwt":%q}`, byJwt)), nil
		})
		for i := int64(1); i <= 4; i += 1 {
			r.clock.requireArmed(t, 5*time.Minute)
			connect.AssertEqual(t, r.requests.Load(), i)
			if i < 4 {
				r.clock.Advance(5 * time.Minute)
			}
		}
	}
}

// The draw's upper end is the half-life, and a draw out of range does not pass
// it: a fresh 30-day token never waits more than 15 days.
func TestTokenManagerNeverSchedulesPastHalfLife(t *testing.T) {
	lifetime := 30 * testingWindowDay
	initial := testingWindowClientJwt(renewalTestBaseTime.Add(-time.Hour), lifetime, "initial")
	r := newTestingWindowRefreshes(t, initial, []float64{1, 2, math.Inf(1), 1.5}, nil, testingWindowAnswer(lifetime))
	for i := 0; i < 3; i += 1 {
		r.clock.requireArmed(t, 15*testingWindowDay)
		r.clock.Advance(15 * testingWindowDay)
	}
	r.clock.requireArmed(t, 15*testingWindowDay)
	connect.AssertEqual(t, r.requests.Load(), int64(4))
}

// A second manager over the API, inactive and with no worker yet, so the test
// installs its hooks before the worker reads them and then runs the worker.
// The API's own manager stays dormant: these tests never start it.
func testingInactiveTokenManager(t *testing.T, api *Api, clock *renewalTestClock) (*apiTokenManager, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(api.ctx)
	manager := &apiTokenManager{
		ctx:            ctx,
		cancel:         cancel,
		api:            api,
		done:           make(chan struct{}),
		refreshMonitor: connect.NewMonitor(),
		settings:       defaultApiTokenRefreshSettings(),
		clock:          clock,
	}
	run := func() {
		go func() {
			defer close(manager.done)
			manager.run()
		}()
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-manager.done:
		case <-time.After(renewalTestStepLimit):
			t.Error("the worker did not stop")
		}
	})
	return manager, run
}

// Start records its refresh before it activates the worker. A wake between the
// two steps (setDeviceByJwt's TokenChanged lands just before StartJwtRefresh)
// must find the worker still dormant. In the other order it found an active
// worker with nothing requested and scheduled the stored token: one already
// past its point was refreshed at once, then again by Start's request, and
// the window tests saw their first draw taken by the stored token.
func TestTokenManagerStartRequestsBeforeActivating(t *testing.T) {
	lifetime := 30 * testingWindowDay
	// 45% into its life; draw 0 puts its point at 40%, already past
	stored := testingWindowClientJwt(renewalTestBaseTime.Add(-time.Duration(0.45*float64(lifetime))), lifetime, "stored")
	_, api := newTestApiForURL(t, "https://window.example")
	clock := newRenewalTestClock()
	manager, run := testingInactiveTokenManager(t, api, clock)
	var draws atomic.Int64
	manager.refreshDraw = func(string) float64 {
		draws.Add(1)
		return 0
	}
	var requests atomic.Int64
	api.setHttpGetRaw(func(ctx context.Context, requestUrl string, byJwt string) ([]byte, error) {
		n := requests.Add(1)
		return []byte(fmt.Sprintf(`{"by_jwt":%q}`, testingWindowClientJwt(clock.Now(), lifetime, fmt.Sprintf("refresh-%d", n)))), nil
	})
	dormant := make(chan struct{}, 16)
	manager.testingDormant = func() {
		dormant <- struct{}{}
	}
	manager.testingBetweenStartSteps = func() {
		// the wake, and the worker's answer to it: parked dormant, or a
		// schedule of the stored token
		manager.refreshMonitor.NotifyAll()
		select {
		case <-dormant:
		case wait := <-clock.armed:
			t.Errorf("between Start's steps the worker scheduled the stored token and armed %s", wait)
		case <-time.After(renewalTestStepLimit):
			t.Error("the worker did not answer the wake between Start's steps")
		}
	}
	api.SetByJwt(stored)
	run()
	select {
	case <-dormant:
	case <-time.After(renewalTestStepLimit):
		t.Fatal("the inactive worker did not park")
	}

	manager.Start()
	// one refresh, Start's, then the refreshed token's point
	clock.requireArmed(t, 12*testingWindowDay)
	connect.AssertEqual(t, requests.Load(), int64(1))
	connect.AssertEqual(t, draws.Load(), int64(1))
	clock.requireNotArmed(t)
}

// A worker with nothing to schedule keeps no copy of the token it scheduled
// last: after a sign-out it holds no credential.
func TestTokenManagerDormantDropsTheScheduledToken(t *testing.T) {
	lifetime := 30 * testingWindowDay
	initial := testingWindowClientJwt(renewalTestBaseTime.Add(-time.Hour), lifetime, "initial")
	_, api := newTestApiForURL(t, "https://window.example")
	clock := newRenewalTestClock()
	manager, run := testingInactiveTokenManager(t, api, clock)
	manager.refreshDraw = func(string) float64 { return 0 }
	api.setHttpGetRaw(func(ctx context.Context, requestUrl string, byJwt string) ([]byte, error) {
		return []byte(fmt.Sprintf(`{"by_jwt":%q}`, testingWindowClientJwt(clock.Now(), lifetime, "refreshed"))), nil
	})
	dormant := make(chan struct{}, 16)
	manager.testingDormant = func() {
		dormant <- struct{}{}
	}
	api.SetByJwt(initial)
	run()
	<-dormant
	manager.Start()
	clock.requireArmed(t, 12*testingWindowDay)

	// signed out: the worker wakes and parks dormant
	api.SetByJwt("")
	manager.TokenChanged()
	select {
	case <-dormant:
	case <-time.After(renewalTestStepLimit):
		t.Fatal("the signed-out worker did not park")
	}
	// parked in its select, the worker writes nothing until the next wake
	connect.AssertEqual(t, manager.scheduledByJwt, "")
	connect.AssertEqual(t, manager.scheduledRefreshTime.IsZero(), true)
}
