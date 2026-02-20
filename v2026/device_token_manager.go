package sdk

import (
	"context"
	"fmt"
	mathrand "math/rand"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/glog/v2026"
)

type deviceTokenManager struct {
	ctx              context.Context
	cancel           context.CancelFunc
	api              *Api
	refreshMonitor   *connect.Monitor
	onTokenRefreshed func(newToken string)
	logout           func() error
}

func newDeviceTokenManager(
	ctx context.Context,
	api *Api,
	onTokenRefreshed func(newToken string),
	logout func() error,
) *deviceTokenManager {
	cancelCtx, cancel := context.WithCancel(ctx)

	manager := &deviceTokenManager{
		ctx:              cancelCtx,
		cancel:           cancel,
		api:              api,
		refreshMonitor:   connect.NewMonitor(),
		onTokenRefreshed: onTokenRefreshed,
		logout:           logout,
	}

	go connect.HandleError(manager.run)

	return manager
}

func (self *deviceTokenManager) run() {
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
				glog.V(1).Infof("[dtm]JWT claims: %+v", claims)

				if exp, ok := claims["exp"].(float64); ok {
					expirationTime = time.Unix(int64(exp), 0)
					return
				}
			}
		}()

		now := time.Now()
		var refreshTimeout time.Duration
		if expirationTime.IsZero() {
			// jwt has no expiration, refresh at an arbitrary interval of 7 days
			refreshTimeout = 7 * 27 * time.Hour
		} else {
			// jwts currently last 30 days on the server, so start attempting to refresh 14 days before expiration
			refreshTime := expirationTime.Add(-14 * 24 * time.Hour)
			refreshTimeout = refreshTime.Sub(now)
		}

		if 0 < refreshTimeout {
			glog.Infof(
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

		func() {
			for {
				glog.Infof("[dtm]refreshing the jwt now")
				err := self.refreshToken()
				if err == nil {
					return
				}

				randomTimeout := time.Duration(mathrand.Int63n(int64(15 * time.Minute)))

				glog.Infof(
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
	}
}

func (self *deviceTokenManager) refreshToken() error {
	result, err := self.api.RefreshJwtSync()

	if err != nil {
		/*
		 *  potentially API failed, try again
		 */

		glog.Errorf("[dtm]failed to refresh JWT: %v", err)

		return err
	}

	if result.Error != nil {
		/**
		 * not a API error, but a token refresh error
		 * for example, client no longer exists
		 */

		glog.Errorf("[dtm]failed to refresh JWT: %v", result.Error.Message)

		self.logout()
		return nil
	}

	// guard against api logic errors that could mess up the client state
	if result.ByJwt == "" {
		return fmt.Errorf("Failed to refresh JWT: empty JWT returned")
	}

	glog.Infof("[dtm]successfully refreshed JWT")

	self.onTokenRefreshed(result.ByJwt)

	return nil
}

// refreshes the token immediately
func (self *deviceTokenManager) RefreshToken() {
	self.refreshMonitor.NotifyAll()
}

func (self *deviceTokenManager) Close() {
	self.cancel()
}
