package sdk

import (
	"context"
	"errors"
	"fmt"
	mathrand "math/rand"
	"net/http"
	"sync/atomic"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect"
)

// apiTokenManager is owned by Api, rather than by DeviceLocal/DeviceRemote.
// Api is the single owner of the mutable bearer token, so headless users such
// as the subnet miner and validator get exactly the same scheduling, retry,
// rotation, and rejection behavior as the apps without constructing a device.
type apiTokenManager struct {
	ctx    context.Context
	cancel context.CancelFunc
	api    *Api

	refreshMonitor *connect.Monitor
	active         atomic.Bool

	// refreshPending makes a refresh request LEVEL-triggered instead of edge-triggered.
	//
	// The monitor is a pure edge: NotifyAll closes the current channel and swaps in a
	// fresh one. The run loop captures that channel at the TOP of each iteration -- so a
	// RefreshToken() landing while the loop is inside the /auth/refresh http call closes
	// a channel nobody is listening to any more. The request would be silently DROPPED,
	// and the next scheduled refresh is half a token lifetime away. The flag survives
	// across iterations, so a request made at any moment is honored on the next pass.
	refreshPending atomic.Bool
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

func newApiTokenManager(ctx context.Context, api *Api) *apiTokenManager {
	cancelCtx, cancel := context.WithCancel(ctx)
	manager := &apiTokenManager{
		ctx:            cancelCtx,
		cancel:         cancel,
		api:            api,
		refreshMonitor: connect.NewMonitor(),
	}
	go connect.HandleError(manager.run)
	return manager
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

// minRefreshTimeout is the floor on the scheduled refresh interval, and it is
// the load-bearing half of the schedule.
//
// The refresh point used to be a FIXED lead subtracted from the token's `exp`
// (14 days, calibrated to a 30-day server lifetime). The server later shortened
// its lifetime to 24h, which made the lead exceed the token's entire life, so
// every computed timeout was ~13 days in the PAST. A non-positive timeout means
// no sleep, and since a successful refresh yields another 24h token, success
// re-armed the same condition: an unbounded refresh loop at ~30ms per pass, 593
// refreshes in one observed 22-minute session.
//
// The half-life schedule below is the correctness fix; this floor is what makes
// the failure mode non-catastrophic the NEXT time the server changes its
// lifetime, or a clock is skewed, or an `exp` is malformed. Any interval derived
// from a server-controlled value needs a floor, or it is one config change away
// from a hot loop.
const minRefreshTimeout = 5 * time.Minute

// noExpirationRefreshTimeout is the fallback when the stored jwt has no usable
// `exp` -- including when it is empty or unparseable. A refreshable legacy token
// without an expiration must not become permanent, so it is revalidated on a
// conservative weekly cadence.
const noExpirationRefreshTimeout = 7 * 24 * time.Hour

// jwtRefreshTimeout is the delay until the next SCHEDULED refresh of `byJwt`.
//
// Pure and total by design: it is the entire schedule, so it can be pinned
// against real tokens without a network, a clock, or a running device. It never
// returns less than minRefreshTimeout -- callers that want an immediate refresh
// (an explicit RefreshToken request, or the one Start arms at launch) override
// it deliberately.
func jwtRefreshTimeout(byJwt string, now time.Time) time.Duration {
	issuedTime := jwtIssuedAt(byJwt)
	expirationTime := jwtExpiration(byJwt)

	var refreshTimeout time.Duration
	if expirationTime.IsZero() {
		// no expiration, or none we can read (including an empty jwt): refresh at
		// an arbitrary long interval
		refreshTimeout = noExpirationRefreshTimeout
	} else {
		// Refresh at the token's HALF-LIFE, derived from the token itself.
		//
		// The server's token lifetime is not a constant this sdk can hardcode: it
		// was 30 days, then became 24h, and the hardcoded 14-day lead this
		// replaces silently became a hot loop when it did (see minRefreshTimeout).
		// A half-life is correct for whatever lifetime the server chooses, and it
		// is the cadence the server documents for sdk clients.
		//
		// Prefer `iat` so the half-life is of the token's REAL lifetime rather than
		// of whatever happens to remain right now -- halving the remainder on every
		// pass would make the interval collapse geometrically toward the floor.
		// Fall back to now when `iat` is absent or nonsensical. (The no-`iat`
		// fallback is deliberately NOT a fixed `exp - 12h` lead: that is the same
		// shape as the 14-day lead that caused the storm, and it lands in the past
		// for any token shorter than 12h.)
		lifetimeStart := issuedTime
		if lifetimeStart.IsZero() || !lifetimeStart.Before(expirationTime) {
			lifetimeStart = now
		}
		refreshTime := lifetimeStart.Add(expirationTime.Sub(lifetimeStart) / 2)
		refreshTimeout = refreshTime.Sub(now)
	}

	// A token already past its half-life -- a device resumed after being offline,
	// a skewed clock, a server that shortened its lifetime again -- is refreshed
	// after the floor rather than in a tight loop.
	if refreshTimeout < minRefreshTimeout {
		refreshTimeout = minRefreshTimeout
	}
	return refreshTimeout
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
			select {
			case <-self.ctx.Done():
				return
			case <-refreshNotify:
				continue
			}
		}

		refreshTimeout := time.Duration(0)
		if !self.refreshPending.Load() {
			// A refresh requested while we were busy (or while computing the
			// timeout) sent the monitor's edge to a channel we are no longer
			// listening to. The level-triggered flag above is what honors that
			// request now instead of sleeping out the schedule.
			refreshTimeout = jwtRefreshTimeout(byJwt, time.Now())
		}

		if 0 < refreshTimeout {
			self.api.logger().Infof(
				"[api-token]waiting %.2fs to refresh the jwt",
				float64(refreshTimeout/time.Millisecond)/1000.0,
			)
			timer := time.NewTimer(refreshTimeout)
			select {
			case <-self.ctx.Done():
				stopApiTokenTimer(timer)
				return
			case <-refreshNotify:
				stopApiTokenTimer(timer)
				continue
			case <-timer.C:
			}
		} else {
			// The wait above is the ONLY place this branch of the outer loop would
			// observe cancellation, and it is skipped whenever the timeout is
			// non-positive. A closed device otherwise still enters refreshToken --
			// 297 attempts in 497ms against a cancelled ClientStrategy in one
			// observed teardown. The zero-timeout path stays legitimately reachable
			// (the refresh Start arms, an explicit RefreshToken), so the guarantee
			// must not depend on the schedule above being positive.
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
			self.api.logger().Infof("[api-token]refreshing the jwt now")
			loggedOut, stale, err := self.refreshToken(byJwt)
			if loggedOut || stale || err == nil {
				break
			}

			// A request arriving during the failed HTTP call must not sleep
			// behind retry jitter. Return to the outer loop immediately.
			if self.refreshPending.Load() {
				break
			}

			randomTimeout := time.Duration(mathrand.Int63n(int64(15 * time.Minute)))
			self.api.logger().Infof(
				"[api-token]jwt refresh failed. Will retry in %.2fs. err = %s",
				float64(randomTimeout/time.Millisecond)/1000.0,
				err,
			)

			// re-capture per wait: see the note at the outer capture. A NotifyAll
			// during this iteration would otherwise leave the arm below
			// permanently ready and defeat the backoff entirely.
			retryNotify := self.refreshMonitor.NotifyChannel()
			timer := time.NewTimer(randomTimeout)
			select {
			case <-self.ctx.Done():
				// Cancellation has to stop run(), not just the retry. This return
				// used to sit inside an anonymous func, so it exited only the
				// CLOSURE; the outer loop then immediately re-entered the refresh,
				// discarding a correctly-computed backoff ("Will retry in
				// 128.69s") in the same millisecond, 297 times.
				stopApiTokenTimer(timer)
				return
			case <-retryNotify:
				stopApiTokenTimer(timer)
				// The token or refresh request changed; recalculate from the
				// API's current state rather than retrying the captured JWT.
			case <-timer.C:
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
	// bound the request to the manager ctx so a closed device does not leave
	// the refresh (and its dialer evals) running to their own timeouts
	result, err := self.api.refreshJwtSyncWithContextAndJwt(self.ctx, byJwt)
	if err != nil {
		// a 401 over the api connection is the auth layer rejecting the jwt
		// itself (expired or unparseable): confirmed invalid
		var statusErr *connect.HttpStatusError
		if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusUnauthorized {
			self.api.logger().Errorf("[api-token]jwt rejected by the api (%d): logging out", statusErr.StatusCode)
			if self.api.rejectByJwt(byJwt) {
				loggedOut = true
			} else {
				stale = true
			}
			return
		}

		self.api.logger().Errorf("[api-token]failed to refresh JWT: %v", err)
		returnErr = err
		return
	}

	if result.Error != nil {
		// not an api error, but a token refresh error -- for example, the client
		// no longer exists
		self.api.logger().Errorf("[api-token]failed to refresh JWT: %v", result.Error.Message)
		if self.api.rejectByJwt(byJwt) {
			loggedOut = true
		} else {
			stale = true
		}
		return
	}

	// guard against api logic errors that could mess up the client state
	if result.ByJwt == "" {
		returnErr = fmt.Errorf("failed to refresh JWT: empty JWT returned")
		return
	}
	if err := validateRefreshedClientJwt(byJwt, result.ByJwt); err != nil {
		returnErr = fmt.Errorf("failed to refresh JWT: %w", err)
		return
	}

	if !self.api.setRefreshedByJwt(byJwt, result.ByJwt) {
		stale = true
		return
	}
	self.api.logger().Infof("[api-token]successfully refreshed JWT")
	return
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
