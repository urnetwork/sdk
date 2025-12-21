package sdk

import (
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"
)

type deviceTokenManager struct {
	ClientJwt       string `json:"client_jwt"`
	AdminJwt        string `json:"admin_jwt"`
	api             *Api
	jwtRefreshTimer *time.Timer
}

func newDeviceTokenManager(
	clientJwt string,
	adminJwt string,
	api *Api,
	onTokenRefreshed func(newToken string),
	logout func() error,
) *deviceTokenManager {
	manager := &deviceTokenManager{
		ClientJwt: clientJwt,
		AdminJwt:  adminJwt,
		api:       api,
	}

	manager.initRefreshJwtTimer(clientJwt, onTokenRefreshed, logout)
	return manager
}

func (self *deviceTokenManager) initRefreshJwtTimer(
	jwt string,
	onSuccess func(newToken string),
	logout func() error,
) {
	token, _, err := gojwt.NewParser().ParseUnverified(jwt, gojwt.MapClaims{})
	if err != nil {
		glog.Errorf("Failed to parse JWT for refresh timer: %v", err)
		return
	}

	if claims, ok := token.Claims.(gojwt.MapClaims); ok {

		glog.Infof("JWT claims: %+v", claims)

		if exp, ok := claims["exp"].(float64); ok {
			glog.Infof("Setting up JWT refresh timer")
			expirationTime := time.Unix(int64(exp), 0)

			// jwts currently last 30 days on the server, so start attempting to refresh 14 days before expiration
			refreshTime := expirationTime.Add(-14 * 24 * time.Hour)
			durationUntilRefresh := time.Until(refreshTime)
			if durationUntilRefresh <= 0 {
				glog.Infof("JWT is expiring soon, should refresh now")
				self.RefreshToken(0, onSuccess, logout)
				return
			}
			glog.Infof("Scheduling JWT refresh in %v", durationUntilRefresh)

			// if previous one exists, close it out
			if self.jwtRefreshTimer != nil {
				self.jwtRefreshTimer.Stop()
			}

			self.jwtRefreshTimer = time.AfterFunc(durationUntilRefresh, func() {
				glog.Infof("JWT refresh timer triggered")
				self.RefreshToken(0, onSuccess, logout)
			})
		} else {
			glog.Errorf("Failed to parse JWT exp claim for refresh timer")
		}
	} else {
		glog.Errorf("Failed to parse JWT claims for refresh timer")
	}
}

func (self *deviceTokenManager) RefreshToken(
	attempt int,
	onSuccess func(newToken string),
	logout func() error,
) (returnErr error) {

	glog.Infof("Refreshing JWT")

	// api := self.GetApi()

	callback := RefreshJwtCallback(connect.NewApiCallback[*RefreshJwtResult](
		func(result *RefreshJwtResult, err error) {

			if err != nil {
				/*
				 *  potentially API failed, try again
				 */

				glog.Errorf("Failed to refresh JWT: %v", err)

				if attempt < 5 {
					backoffDuration := time.Duration((attempt+1)*1) * time.Minute
					glog.Infof("Scheduling JWT refresh retry in %v", backoffDuration)
					time.AfterFunc(backoffDuration, func() {
						self.RefreshToken(attempt+1, onSuccess, logout)
					})
				}

				returnErr = err

				return
			}

			if result.Error != nil {
				/**
				 * not a API error, but a token refresh error
				 * for example, client no longer exists
				 */

				glog.Errorf("Failed to refresh JWT: %v", result.Error.Message)

				// logout user?
				logout()
				return
			}

			if result.ByJwt == "" {
				glog.Errorf("Failed to refresh JWT: empty JWT returned")

				// logout?
				logout()
				return
			}

			glog.Infof("Successfully refreshed JWT")

			onSuccess(result.ByJwt)

			self.initRefreshJwtTimer(result.ByJwt, onSuccess, logout)

		},
	))

	self.api.RefreshJwt(callback)

	// todo
	return
}

func (self *deviceTokenManager) Close() {
	if self.jwtRefreshTimer != nil {
		self.jwtRefreshTimer.Stop()
	}
}
