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

	"github.com/urnetwork/connect/v2026"
)

type deviceTokenManager struct {
	ctx              context.Context
	cancel           context.CancelFunc
	log              connect.Logger
	api              *Api
	refreshMonitor   *connect.Monitor
	onTokenRefreshed func(newToken string)
	logout           func() error

	// refreshPending makes a refresh request LEVEL-triggered instead of edge-triggered.
	//
	// The monitor is a pure edge: NotifyAll closes the current channel and swaps in a
	// fresh one. The run loop captures that channel at the TOP of each iteration — so a
	// RefreshToken() landing while the loop is inside the /auth/refresh http call closes
	// a channel nobody is listening to any more. The request is silently DROPPED, and
	// the next scheduled refresh is ~16 days away.
	//
	// That is a real failure, not a theoretical one: an app asking for a refresh right
	// after an upgrade (to pick up the new `pro` claim) would simply never get one, and
	// nothing would say so. The flag survives across iterations, so a request made at any
	// moment is honored on the next pass.
	refreshPending atomic.Bool
}

func newDeviceTokenManager(
	ctx context.Context,
	log connect.Logger,
	api *Api,
	onTokenRefreshed func(newToken string),
	logout func() error,
) *deviceTokenManager {
	cancelCtx, cancel := context.WithCancel(ctx)

	manager := &deviceTokenManager{
		ctx:              cancelCtx,
		cancel:           cancel,
		log:              log,
		api:              api,
		refreshMonitor:   connect.NewMonitor(),
		onTokenRefreshed: onTokenRefreshed,
		logout:           logout,
	}

	go connect.HandleError(manager.run)

	return manager
}

func (self *deviceTokenManager) run() {
	// the first refresh runs immediately: an app start with a stored jwt
	// must find out right away when the jwt's client no longer exists on the
	// server (see refreshToken), instead of silently running against a dead
	// client until the scheduled refresh window
	first := true
	for {
		refreshNotify := self.refreshMonitor.NotifyChannel()

		var expirationTime time.Time
		func() {
			byJwt := self.api.GetByJwt()
			token, _, err := gojwt.NewParser().ParseUnverified(byJwt, gojwt.MapClaims{})
			if err != nil {
				return
			}

			if claims, ok := token.Claims.(gojwt.MapClaims); ok {
				if self.log.V(1).Enabled() {
					self.log.Infof("[dtm]JWT claims: %+v", claims)
				}

				if exp, ok := claims["exp"].(float64); ok {
					expirationTime = time.Unix(int64(exp), 0)
					return
				}
			}
		}()

		now := time.Now()
		var refreshTimeout time.Duration
		if first {
			first = false
			refreshTimeout = 0
		} else if expirationTime.IsZero() {
			// jwt has no expiration, refresh at an arbitrary interval of 7 days
			refreshTimeout = 7 * 27 * time.Hour
		} else {
			// jwts currently last 30 days on the server, so start attempting to refresh 14 days before expiration
			refreshTime := expirationTime.Add(-14 * 24 * time.Hour)
			refreshTimeout = refreshTime.Sub(now)
		}

		// A refresh was requested while we were busy (or while computing the timeout
		// above), so the monitor's edge went to a channel we are no longer listening to.
		// Honor the request instead of sleeping for two weeks.
		if self.refreshPending.Load() {
			refreshTimeout = 0
		}

		if 0 < refreshTimeout {
			self.log.Infof(
				"[dtm]waiting %.2fs to refresh the jwt",
				float64(refreshTimeout/time.Millisecond)/1000.0,
			)
			select {
			case <-self.ctx.Done():
				return
			case <-refreshNotify:
			case <-time.After(refreshTimeout):
			}
		}

		// Consume the request BEFORE doing the work, not after. A RefreshToken() that
		// lands *during* the refresh below must set the flag again and be serviced by the
		// next iteration — clearing it afterwards would swallow exactly that request.
		self.refreshPending.Store(false)

		loggedOut := false
		func() {
			for {
				self.log.Infof("[dtm]refreshing the jwt now")
				var err error
				loggedOut, err = self.refreshToken()
				if err == nil {
					return
				}

				randomTimeout := time.Duration(mathrand.Int63n(int64(15 * time.Minute)))

				self.log.Infof(
					"[dtm]jwt refresh failed. Will retry in %.2fs. err = %s",
					float64(randomTimeout/time.Millisecond)/1000.0,
					err,
				)

				select {
				case <-self.ctx.Done():
					return
				case <-refreshNotify:
				case <-time.After(randomTimeout):
				}
			}
		}()
		if loggedOut {
			// the local auth state is cleared; there is nothing left to
			// refresh. Without stopping here, an already-expired stored jwt
			// would hot loop refresh->logout.
			return
		}
	}
}

// refreshToken refreshes the jwt once. `loggedOut` is true when the api
// confirmed the credential is invalid (an error result such as "client no
// longer exists", or a 401) and the logout callback ran; `err` is a transient
// failure the caller should retry.
//
// The logout decision is deliberately conservative: only a confirmed api
// response that rejects the jwt logs out. Transport failures (offline
// network), timeouts, and non-401 statuses (5xx outages, proxy/waf blocks)
// retry forever without touching the auth state. Non-2xx responses surface as
// a typed `connect.HttpStatusError` from the http layer, so an outage page
// body can never be mistaken for a refresh result.
func (self *deviceTokenManager) refreshToken() (loggedOut bool, returnErr error) {
	// bound the request to the manager ctx so a closed device does not leave
	// the refresh (and its dialer evals) running to their own timeouts
	result, err := self.api.refreshJwtSyncWithContext(self.ctx)

	if err != nil {
		// a 401 over the api connection is the auth layer rejecting the jwt
		// itself (expired or unparseable): confirmed invalid
		var statusErr *connect.HttpStatusError
		if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusUnauthorized {
			self.log.Errorf("[dtm]jwt rejected by the api (%d): logging out", statusErr.StatusCode)

			self.logout()
			loggedOut = true
			return
		}

		/*
		 *  potentially API failed, try again
		 */

		self.log.Errorf("[dtm]failed to refresh JWT: %v", err)

		returnErr = err
		return
	}

	if result.Error != nil {
		/**
		 * not a API error, but a token refresh error
		 * for example, client no longer exists
		 */

		self.log.Errorf("[dtm]failed to refresh JWT: %v", result.Error.Message)

		self.logout()
		loggedOut = true
		return
	}

	// guard against api logic errors that could mess up the client state
	if result.ByJwt == "" {
		returnErr = fmt.Errorf("Failed to refresh JWT: empty JWT returned")
		return
	}

	self.log.Infof("[dtm]successfully refreshed JWT")

	self.onTokenRefreshed(result.ByJwt)

	return
}

// refreshes the token immediately
func (self *deviceTokenManager) RefreshToken() {
	// Record the request FIRST. The notify below is only a wake-up; the flag is what
	// makes the request survive being made while the loop is busy refreshing.
	self.refreshPending.Store(true)
	self.refreshMonitor.NotifyAll()
}

func (self *deviceTokenManager) Close() {
	self.cancel()
}
