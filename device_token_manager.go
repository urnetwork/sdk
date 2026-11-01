package sdk

import (
	"context"
	"errors"
	"fmt"
	mathrand "math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect"
)

// apiTokenManager is owned by Api, rather than by DeviceLocal/DeviceRemote.
// Api is the single owner of the mutable bearer token, so headless users such
// as the subnet miner and validator get exactly the same scheduling, retry,
// rotation, and rejection behavior as the apps without constructing a device.
//
// It refreshes on the schedule of apiTokenRefreshSettings: each token at a
// point drawn from its refresh window, never sooner than the floor after the
// previous attempt, and after a failed attempt, again after a uniform wait.
type apiTokenManager struct {
	ctx    context.Context
	cancel context.CancelFunc
	api    *Api
	done   chan struct{}

	refreshMonitor *connect.Monitor
	active         atomic.Bool

	// A DeviceRemote can make a new API transport usable while an older direct
	// request is still failing. The generation closes that race: a failure can
	// tell that transport availability changed during its request and retry
	// immediately. The pending bits keep later availability notifications
	// level-triggered without turning every ordinary reconnect into a refresh.
	stateLock             sync.Mutex
	transportGeneration   uint64
	activeRefreshAttempt  *apiTokenRefreshAttempt
	failedRefreshPending  bool
	transportRetryPending bool

	// Immutable after the worker starts. Tests install a controlled clock, a
	// deterministic window draw, and a deterministic barrier for the retry
	// wait.
	settings     *apiTokenRefreshSettings
	clock        apiRefreshClock
	refreshDraw  func() float64
	retryTimeout func() time.Duration

	// Owned by run: the token the schedule was drawn for and its refresh time,
	// and when the last refresh attempt began (zero before the first).
	scheduledByJwt         string
	scheduledRefreshTime   time.Time
	lastRefreshAttemptTime time.Time

	// refreshPending makes a refresh request LEVEL-triggered instead of edge-triggered.
	//
	// The monitor is a pure edge: NotifyAll closes the current channel and swaps in a
	// fresh one. The run loop captures that channel at the TOP of each iteration -- so a
	// RefreshToken() landing while the loop is inside the /auth/refresh http call closes
	// a channel nobody is listening to any more. The request would be silently DROPPED,
	// and the next scheduled refresh is up to half a token lifetime away. The flag survives
	// across iterations, so a request made at any moment is honored on the next pass.
	refreshPending atomic.Bool
}

// apiTokenRefreshAttempt owns one cancellable request generation. Superseded
// is guarded by its manager's stateLock.
type apiTokenRefreshAttempt struct {
	ctx                 context.Context
	cancel              context.CancelFunc
	transportGeneration uint64
	superseded          bool
}

// apiTokenRefreshOutcome keeps retryable request errors unlogged until the
// owner knows whether a transport transition deliberately canceled them.
type apiTokenRefreshOutcome struct {
	loggedOut       bool
	stale           bool
	logRequestError bool
	requestCanceled bool
	err             error
}

type clientJwtIdentity struct {
	clientId string
	deviceId string
}

func parseClientJwtIdentity(byJwt string) (clientJwtIdentity, bool) {
	claims := gojwt.MapClaims{}
	if _, _, err := gojwt.NewParser().ParseUnverified(byJwt, claims); err != nil {
		return clientJwtIdentity{}, false
	}

	clientId, clientOk := claims["client_id"].(string)
	deviceId, deviceOk := claims["device_id"].(string)
	if !clientOk || clientId == "" || !deviceOk || deviceId == "" {
		return clientJwtIdentity{}, false
	}

	return clientJwtIdentity{
		clientId: clientId,
		deviceId: deviceId,
	}, true
}

// validateRefreshedClientJwt prevents a refresh response from silently
// replacing the live device identity. A genuine login/device change must build
// a new Device and rotate its instance through LocalState's login setters.
// Invalid legacy/test bearer strings remain accepted here for compatibility;
// production client JWTs always have the identity claims because jwtCanRefresh
// requires them before the refresh loop runs.
func validateRefreshedClientJwt(previousByJwt string, refreshedByJwt string) error {
	previousIdentity, previousOk := parseClientJwtIdentity(previousByJwt)
	if !previousOk {
		return nil
	}
	refreshedIdentity, refreshedOk := parseClientJwtIdentity(refreshedByJwt)
	if !refreshedOk {
		return errors.New("refreshed JWT is missing the client device identity")
	}
	if previousIdentity != refreshedIdentity {
		return errors.New("refreshed JWT changed the client device identity")
	}
	return nil
}

func newApiTokenManager(ctx context.Context, api *Api, settings *apiTokenRefreshSettings) *apiTokenManager {
	cancelCtx, cancel := context.WithCancel(ctx)
	manager := &apiTokenManager{
		ctx:            cancelCtx,
		cancel:         cancel,
		api:            api,
		done:           make(chan struct{}),
		refreshMonitor: connect.NewMonitor(),
		settings:       settings,
		clock:          systemApiRefreshClock{},
	}
	go func() {
		defer close(manager.done)
		connect.HandleError(manager.run)
	}()
	return manager
}

// Wait joins the API-owned refresh worker after its lifetime is canceled.
func (self *apiTokenManager) Wait() {
	<-self.done
}

// jwtCanRefresh is intentionally an unverified parse. It is only a local
// scheduling predicate; /auth/refresh performs the authoritative signature,
// claims, active-client, and network-ownership checks.
func jwtCanRefresh(byJwt string) bool {
	_, ok := parseClientJwtIdentity(byJwt)
	return ok
}

func jwtExpiration(byJwt string) time.Time {
	claims := gojwt.MapClaims{}
	if _, _, err := gojwt.NewParser().ParseUnverified(byJwt, claims); err != nil {
		return time.Time{}
	}
	expiration, err := claims.GetExpirationTime()
	if err != nil || expiration == nil {
		return time.Time{}
	}
	return expiration.Time
}

func jwtIssuedAt(byJwt string) time.Time {
	claims := gojwt.MapClaims{}
	if _, _, err := gojwt.NewParser().ParseUnverified(byJwt, claims); err != nil {
		return time.Time{}
	}
	issuedAt, err := claims.GetIssuedAt()
	if err != nil || issuedAt == nil {
		return time.Time{}
	}
	return issuedAt.Time
}

// The schedule of an API's credential refreshes: the client JWT refreshes of
// apiTokenManager and the network credential renewals of
// apiNetworkCredentialRenewer. Neither worker changes it.
//
// A scheduled refresh falls at a point of the token's own lifetime, from `iat`
// to `exp`, that each token draws uniformly from the refresh window, between
// RefreshWindowStart and RefreshWindowEnd of the lifetime (owner decision,
// 2026-11-01). The draw is what spreads clients that refreshed together --
// after a mass refusal, or when a process hosting many devices restarts --
// across the window. At exactly the half-life, such a group refreshed in
// lockstep every cycle, indefinitely. The window ends at the half-life, so a
// token keeps at least half its life for the retries after a failed refresh,
// and a token loaded past its point refreshes at once.
//
// The lifetime is server policy that the sdk cannot hardcode: it was 30 days,
// then 24h, then 30 days again. A schedule derived from each token is right for
// whatever lifetime the server picks next. The refresh point used to be a fixed
// 14-day lead before `exp`, written for 30-day tokens; with 24h tokens every
// refresh was due in the past, and since a successful refresh yields another
// such token, success re-armed the same condition: 593 refreshes in one
// observed 22-minute session. MinRefreshTimeout is what keeps the next lifetime
// change, a skewed clock or a malformed `exp` from doing that again.
type apiTokenRefreshSettings struct {
	// The start of the refresh window, as a fraction of the token's lifetime.
	// 0.4 spreads a group over the last tenth of the lifetime before the
	// half-life (3 days of a 30-day token), while the mean scheduled refresh
	// rate rises only from 1/0.5 to 1/0.45 per lifetime, about 11%.
	// Non-positive starts the window at `iat`.
	RefreshWindowStart float64
	// The end of the refresh window, as a fraction of the token's lifetime: the
	// half-life, 0.5, which no drawn point passes. At or below
	// RefreshWindowStart, every token refreshes at the window's start.
	RefreshWindowEnd float64
	// The least time from one refresh attempt to the next scheduled one. Any
	// schedule derived from a server-controlled value needs a floor, or it is
	// one config change away from a hot loop. The floor wins over the window:
	// a lifetime shorter than twice the floor refreshes after the floor, later
	// than its half-life. Immediate refreshes (Start, RefreshToken, a new token)
	// and the retries after a failure are not held by it. Non-positive
	// disables it.
	MinRefreshTimeout time.Duration
	// How long after it is first scheduled a token without a readable `exp`
	// refreshes, including an empty or unparseable one. A refreshable legacy
	// token without an expiration must not become permanent, so it is
	// revalidated weekly. Also the cap of the network renewer's anomaly
	// spacing (networkRenewalAnomalyTimeout). Non-positive refreshes such a
	// token as often as the floor allows.
	NoExpirationRefreshTimeout time.Duration
	// After a failed client JWT refresh, the next attempt waits a time drawn
	// uniformly from [0, RefreshRetryTimeout), which spreads clients that failed
	// together across the interval. Non-positive retries without a wait.
	RefreshRetryTimeout time.Duration

	// A failed network credential renewal waits at least this long, plus a
	// jitter drawn from an interval that starts at
	// NetworkRenewalRetryJitterBase and doubles with each further failure in
	// a row, up to NetworkRenewalMaxRetryJitter. The minimum keeps any jitter,
	// even none, from retrying in a loop; non-positive leaves only the
	// jitter, and a non-positive jitter interval adds none.
	NetworkRenewalMinRetryTimeout time.Duration
	NetworkRenewalRetryJitterBase time.Duration
	NetworkRenewalMaxRetryJitter  time.Duration
	// A renewal the server answers with another 4xx (a server that predates
	// the route answers 404) is tried again after this. Non-positive retries
	// at once.
	NetworkRenewalRefusedRetryTimeout time.Duration
}

func defaultApiTokenRefreshSettings() *apiTokenRefreshSettings {
	return &apiTokenRefreshSettings{
		RefreshWindowStart:                0.4,
		RefreshWindowEnd:                  0.5,
		MinRefreshTimeout:                 5 * time.Minute,
		NoExpirationRefreshTimeout:        7 * 24 * time.Hour,
		RefreshRetryTimeout:               15 * time.Minute,
		NetworkRenewalMinRetryTimeout:     10 * time.Second,
		NetworkRenewalRetryJitterBase:     30 * time.Second,
		NetworkRenewalMaxRetryJitter:      15 * time.Minute,
		NetworkRenewalRefusedRetryTimeout: 24 * time.Hour,
	}
}

// The point of a token's lifetime that a window draw selects: draw 0 is the
// window's start and 1 its end. A draw outside [0, 1] is clamped, so no draw
// falls after the window's end.
func (self *apiTokenRefreshSettings) refreshFraction(draw float64) float64 {
	start := max(self.RefreshWindowStart, 0)
	end := max(self.RefreshWindowEnd, start)
	if !(0 <= draw) {
		// negative, or not a number
		draw = 0
	} else if 1 < draw {
		draw = 1
	}
	return min(start+(end-start)*draw, end)
}

// The time source of an API's refresh workers. Tests install a controlled
// one.
type apiRefreshClock interface {
	Now() time.Time
	// After starts a timer of d; stop releases it.
	After(d time.Duration) (c <-chan time.Time, stop func())
}

type systemApiRefreshClock struct{}

func (systemApiRefreshClock) Now() time.Time {
	return time.Now()
}

func (systemApiRefreshClock) After(d time.Duration) (<-chan time.Time, func()) {
	timer := time.NewTimer(d)
	return timer.C, func() {
		stopApiTokenTimer(timer)
	}
}

// When the scheduled refresh of `byJwt` falls, for a window draw in [0, 1]
// (refreshFraction). A token without a readable `exp` refreshes
// NoExpirationRefreshTimeout from now; any other at its drawn point, which is
// in the past for a token loaded late in its life.
//
// Pure and total, so the schedule can be pinned against real tokens without a
// network, a clock or a running device. jwtRefreshTimeout spaces the result
// from the previous attempt.
func jwtRefreshTime(byJwt string, now time.Time, draw float64, settings *apiTokenRefreshSettings) time.Time {
	expirationTime := jwtExpiration(byJwt)
	if expirationTime.IsZero() {
		return now.Add(settings.NoExpirationRefreshTimeout)
	}
	// The point is of the token's real lifetime, from `iat`, rather than of
	// whatever remains now: a fraction of the remainder on every pass would
	// collapse geometrically toward the floor. Without a sensible `iat` the
	// lifetime counts from now. That fallback is deliberately not a fixed lead
	// before `exp`, the shape of the 14-day lead that caused the storm, which
	// lands in the past for any token shorter than the lead.
	lifetimeStart := jwtIssuedAt(byJwt)
	if lifetimeStart.IsZero() || !lifetimeStart.Before(expirationTime) {
		lifetimeStart = now
	}
	lifetime := float64(expirationTime.Sub(lifetimeStart))
	return lifetimeStart.Add(time.Duration(settings.refreshFraction(draw) * lifetime))
}

// The wait from now until a scheduled refresh at refreshTime, held to at
// least MinRefreshTimeout after the previous refresh attempt (zero when there
// was none). It is 0 when the refresh is due: a token
// loaded past its point refreshes promptly, and the floor, rather than the
// schedule, keeps one scheduled refresh from following another in a loop.
func jwtRefreshTimeout(refreshTime time.Time, previousAttemptTime time.Time, now time.Time, settings *apiTokenRefreshSettings) time.Duration {
	if !previousAttemptTime.IsZero() {
		if earliest := previousAttemptTime.Add(settings.MinRefreshTimeout); refreshTime.Before(earliest) {
			refreshTime = earliest
		}
	}
	return max(refreshTime.Sub(now), 0)
}

func stopApiTokenTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func (self *apiTokenManager) Start() {
	if self.active.CompareAndSwap(false, true) {
		// the first refresh runs immediately: an app start with a stored jwt must
		// find out right away when the jwt's client no longer exists on the server
		// (see refreshToken), instead of silently running against a dead client
		// until the scheduled refresh window
		self.RefreshToken()
	}
}

func (self *apiTokenManager) TokenChanged() {
	if self.active.Load() {
		self.RefreshToken()
		return
	}
	// Wake the dormant loop as well. This matters when a network JWT is
	// replaced with a client JWT immediately before Start.
	self.refreshMonitor.NotifyAll()
}

// transportAvailable cancels an attempt that captured an older API path, or
// wakes an outstanding failed refresh. /auth/refresh is an idempotent GET that
// mints a JWT without invalidating the authenticated JWT, so retrying after an
// ambiguous cancellation cannot strand the client on a rotated credential.
func (self *apiTokenManager) transportAvailable() {
	var attemptCancel context.CancelFunc
	self.stateLock.Lock()
	self.transportGeneration += 1
	if self.activeRefreshAttempt != nil {
		self.activeRefreshAttempt.superseded = true
		attemptCancel = self.activeRefreshAttempt.cancel
	} else if self.failedRefreshPending {
		self.transportRetryPending = true
		self.refreshMonitor.NotifyAll()
	}
	self.stateLock.Unlock()

	// Context cancellation is external work; never invoke it under stateLock.
	if attemptCancel != nil {
		attemptCancel()
	}
}

// beginRefreshAttempt registers cancellation ownership before the HTTP path is
// selected. A transport publication is therefore ordered either wholly before
// this attempt (which uses the new path) or after it (which cancels this path).
func (self *apiTokenManager) beginRefreshAttempt() *apiTokenRefreshAttempt {
	attemptCtx, attemptCancel := context.WithCancel(self.ctx)
	attempt := &apiTokenRefreshAttempt{
		ctx:    attemptCtx,
		cancel: attemptCancel,
	}

	self.stateLock.Lock()
	self.failedRefreshPending = false
	self.transportRetryPending = false
	attempt.transportGeneration = self.transportGeneration
	self.activeRefreshAttempt = attempt
	self.stateLock.Unlock()
	return attempt
}

// finishRefreshAttempt releases request ownership and reports whether a newer
// transport canceled this exact attempt. Pointer identity prevents a late
// completion from clearing a later attempt if the lifecycle changes.
func (self *apiTokenManager) finishRefreshAttempt(attempt *apiTokenRefreshAttempt) bool {
	attempt.cancel()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	superseded := attempt.superseded
	if self.activeRefreshAttempt == attempt {
		self.activeRefreshAttempt = nil
	}
	return superseded
}

// markRefreshFailed records the retryable failure and reports whether a newer
// transport became available while that request was in flight.
func (self *apiTokenManager) markRefreshFailed(transportGeneration uint64) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.failedRefreshPending = true
	if self.transportGeneration != transportGeneration {
		self.transportRetryPending = true
	}
	return self.transportRetryPending
}

// transportRetryIsPending reads the level paired with refreshMonitor.
func (self *apiTokenManager) transportRetryIsPending() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.transportRetryPending
}

// The randomized outage backoff, uniform over [0, RefreshRetryTimeout). Tests
// replace it with a barrier so transport/wait races are forced rather than
// scheduled by luck.
func (self *apiTokenManager) nextRetryTimeout() time.Duration {
	if self.retryTimeout != nil {
		return self.retryTimeout()
	}
	if self.settings.RefreshRetryTimeout <= 0 {
		return 0
	}
	return time.Duration(mathrand.Int63n(int64(self.settings.RefreshRetryTimeout)))
}

// A token's point in its refresh window, drawn uniformly from [0, 1).
func (self *apiTokenManager) drawRefreshPoint() float64 {
	if self.refreshDraw != nil {
		return self.refreshDraw()
	}
	return mathrand.Float64()
}

// The wait until the scheduled refresh of `byJwt`. A token draws its point
// once, when it is first scheduled: a refresh installs a new token, which
// draws again, and a wake that changes nothing keeps the point it has. A token
// without `iat` keeps its lifetime start the same way, rather than receding on
// every pass. Owned by run.
func (self *apiTokenManager) scheduledRefreshTimeout(byJwt string) time.Duration {
	now := self.clock.Now()
	if byJwt != self.scheduledByJwt {
		self.scheduledByJwt = byJwt
		self.scheduledRefreshTime = jwtRefreshTime(byJwt, now, self.drawRefreshPoint(), self.settings)
	}
	return jwtRefreshTimeout(self.scheduledRefreshTime, self.lastRefreshAttemptTime, now, self.settings)
}

func (self *apiTokenManager) run() {
	for {
		// Capture the notify channel BEFORE reading the token and refreshPending,
		// and use it for exactly ONE wait (the inner retry loop re-captures its
		// own). Subscribe then check, never the reverse: a RefreshToken landing
		// between the check and the capture would close a channel we had not taken
		// yet, and we would sleep the full interval with the request still pending.
		//
		// `Monitor.NotifyAll` CLOSES the current channel and swaps in a fresh one,
		// so a captured channel that outlives its wait is a permanently-ready
		// select arm -- a zero-backoff spin for the rest of the iteration.
		refreshNotify := self.refreshMonitor.NotifyChannel()
		byJwt := self.api.GetByJwt()

		if !self.active.Load() || !jwtCanRefresh(byJwt) {
			// keep no copy of a credential that is no longer scheduled, such as
			// one signed out
			self.scheduledByJwt = ""
			self.scheduledRefreshTime = time.Time{}
			select {
			case <-self.ctx.Done():
				return
			case <-refreshNotify:
				continue
			}
		}

		refreshTimeout := time.Duration(0)
		if !self.refreshPending.Load() && !self.transportRetryIsPending() {
			// A refresh requested while we were busy (or while computing the
			// timeout) sent the monitor's edge to a channel we are no longer
			// listening to. The level-triggered flag above is what honors that
			// request now instead of sleeping out the schedule.
			refreshTimeout = self.scheduledRefreshTimeout(byJwt)
		}

		if 0 < refreshTimeout {
			self.api.logger().Infof(
				"[api-token]waiting %.2fs to refresh the jwt",
				float64(refreshTimeout/time.Millisecond)/1000.0,
			)
			timeout, stopTimeout := self.clock.After(refreshTimeout)
			select {
			case <-self.ctx.Done():
				stopTimeout()
				return
			case <-refreshNotify:
				stopTimeout()
				continue
			case <-timeout:
			}
		} else {
			// The wait above is the ONLY place this branch of the outer loop would
			// observe cancellation, and it is skipped whenever the timeout is
			// non-positive. A closed device otherwise still enters refreshToken --
			// 297 attempts in 497ms against a cancelled ClientStrategy in one
			// observed teardown. The zero-timeout path stays legitimately reachable
			// (the refresh Start arms, an explicit RefreshToken, a token loaded past
			// its refresh point), so the guarantee must not depend on the schedule
			// above being positive.
			select {
			case <-self.ctx.Done():
				return
			default:
			}
		}

		// Consume before the request. A manual request or new JWT installed
		// while this HTTP call is in flight sets the flag again and is handled
		// on the next outer iteration.
		self.refreshPending.Store(false)
		byJwt = self.api.GetByJwt()
		if !jwtCanRefresh(byJwt) {
			continue
		}

		for {
			// the floor of the next scheduled refresh counts from here
			self.lastRefreshAttemptTime = self.clock.Now()
			attempt := self.beginRefreshAttempt()
			self.api.logger().Infof("[api-token]refreshing the jwt now")
			outcome := self.refreshTokenWithContext(attempt.ctx, byJwt)
			superseded := self.finishRefreshAttempt(attempt)
			if outcome.loggedOut || outcome.stale || outcome.err == nil {
				break
			}
			if self.ctx.Err() != nil {
				return
			}

			if superseded && outcome.requestCanceled {
				// An explicit refresh or token replacement retains its stronger
				// level-triggered ownership. Let the outer loop consume it (and
				// re-read the JWT) rather than performing a transport retry first.
				if self.refreshPending.Load() {
					break
				}
				// The obsolete-path error is expected cancellation, not an outage.
				// Retry through the now-published path without emitting a false
				// failure or entering randomized backoff.
				continue
			}

			if outcome.logRequestError {
				self.api.logger().Errorf("[api-token]failed to refresh JWT: %v", outcome.err)
			}
			transportRetryPending := self.markRefreshFailed(attempt.transportGeneration)

			// A request arriving during the failed HTTP call must not sleep
			// behind retry jitter. Return to the outer loop immediately.
			if self.refreshPending.Load() {
				break
			}
			// RPC transport availability is separate from an explicit refresh:
			// it retries this same captured JWT only when the failed request
			// overlapped the transition.
			if transportRetryPending {
				continue
			}

			// Subscribe immediately before reading both pending levels. A wake
			// between an earlier read and this capture would otherwise be lost
			// behind the full retry jitter.
			retryNotify := self.refreshMonitor.NotifyChannel()
			if self.refreshPending.Load() {
				break
			}
			if self.transportRetryIsPending() {
				continue
			}

			randomTimeout := self.nextRetryTimeout()
			self.api.logger().Infof(
				"[api-token]jwt refresh failed. Will retry in %.2fs. err = %s",
				float64(randomTimeout/time.Millisecond)/1000.0,
				outcome.err,
			)

			retryWait, stopRetryWait := self.clock.After(randomTimeout)
			select {
			case <-self.ctx.Done():
				// Cancellation has to stop run(), not just the retry. This return
				// used to sit inside an anonymous func, so it exited only the
				// CLOSURE; the outer loop then immediately re-entered the refresh,
				// discarding a correctly-computed backoff ("Will retry in
				// 128.69s") in the same millisecond, 297 times.
				stopRetryWait()
				return
			case <-retryNotify:
				stopRetryWait()
				if !self.refreshPending.Load() && self.transportRetryIsPending() {
					continue
				}
				// The token or an explicit refresh request changed; recalculate
				// from the API's current state rather than retrying the captured
				// JWT.
			case <-retryWait:
				continue
			}
			break
		}
	}
}

// refreshToken refreshes one captured JWT. loggedOut means that exact token
// was authoritatively rejected and cleared. stale means a concurrent login or
// token replacement won the compare-and-set, so this result was discarded.
//
// The logout decision is deliberately conservative: only a confirmed api
// response that rejects the jwt logs out. Transport failures (offline
// network), timeouts, and non-401 statuses (5xx outages, proxy/waf blocks)
// retry forever without touching the auth state. Non-2xx responses surface as
// a typed `connect.HttpStatusError` from the http layer, so an outage page
// body can never be mistaken for a refresh result.
func (self *apiTokenManager) refreshToken(byJwt string) (loggedOut bool, stale bool, returnErr error) {
	outcome := self.refreshTokenWithContext(self.ctx, byJwt)
	if outcome.logRequestError {
		self.api.logger().Errorf("[api-token]failed to refresh JWT: %v", outcome.err)
	}
	return outcome.loggedOut, outcome.stale, outcome.err
}

// refreshTokenWithContext leaves retryable request-error logging to its owner,
// which can distinguish a real failure from deliberate transport supersession.
func (self *apiTokenManager) refreshTokenWithContext(ctx context.Context, byJwt string) apiTokenRefreshOutcome {
	currentJwt, authGeneration := self.api.authCredentialSnapshot()
	if currentJwt != byJwt {
		return apiTokenRefreshOutcome{stale: true}
	}
	// Bound the request to the attempt ctx so a transport transition can release
	// an obsolete path immediately and manager close still joins every dialer.
	result, err := self.api.refreshJwtSyncWithContextAndJwt(ctx, byJwt)
	if err != nil {
		// A 401 over the api connection is the auth layer rejecting the jwt
		// itself (expired, unparseable or a revoked session): confirmed invalid.
		// Its structured code is the logout's cause.
		var statusErr *connect.HttpStatusError
		if ConfirmedClientRefreshRejection(err) {
			self.api.logger().Errorf("[api-token]jwt rejected by the api (%d): logging out", http.StatusUnauthorized)
			if self.api.rejectByJwt(byJwt, confirmedRejectionCause(err), authGeneration) {
				return apiTokenRefreshOutcome{loggedOut: true}
			} else {
				return apiTokenRefreshOutcome{stale: true}
			}
		}
		var invalid *ClientControlResponseError
		if errors.As(err, &invalid) {
			self.api.reportClientRefreshIntegrity(byJwt, authGeneration)
		}

		return apiTokenRefreshOutcome{
			logRequestError: true,
			// Preserve every HTTP response, including retryable 5xx responses,
			// even if transport publication raced with it. Only an untyped
			// request failure from the canceled attempt is expected supersession.
			requestCanceled: ctx.Err() != nil && !errors.As(err, &statusErr),
			err:             err,
		}
	}

	if result.Error != nil {
		// not an api error, but a token refresh error -- for example, the client
		// no longer exists. A refusal in a result carries no cause.
		self.api.logger().Errorf("[api-token]failed to refresh JWT: %v", result.Error.Message)
		if self.api.rejectByJwt(byJwt, "", authGeneration) {
			return apiTokenRefreshOutcome{loggedOut: true}
		} else {
			return apiTokenRefreshOutcome{stale: true}
		}
	}

	// guard against api logic errors that could mess up the client state
	if result.ByJwt == "" {
		return apiTokenRefreshOutcome{err: fmt.Errorf("failed to refresh JWT: empty JWT returned")}
	}
	if err := validateRefreshedClientJwt(byJwt, result.ByJwt); err != nil {
		self.api.reportClientRefreshIntegrity(byJwt, authGeneration)
		return apiTokenRefreshOutcome{err: fmt.Errorf("failed to refresh JWT: %w", err)}
	}

	if !self.api.setRefreshedByJwt(byJwt, result.ByJwt, authGeneration) {
		return apiTokenRefreshOutcome{stale: true}
	}
	self.api.logger().Infof("[api-token]successfully refreshed JWT")
	return apiTokenRefreshOutcome{}
}

// RefreshToken records a level-triggered immediate refresh request.
func (self *apiTokenManager) RefreshToken() {
	// Record the request FIRST. The notify below is only a wake-up; the flag is
	// what makes the request survive being made while the loop is busy refreshing.
	self.refreshPending.Store(true)
	self.refreshMonitor.NotifyAll()
}

func (self *apiTokenManager) Close() {
	self.cancel()
}
