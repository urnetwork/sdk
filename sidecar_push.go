//go:build !ios_extension && !js

// SetPushToken (§18.5, §18.7's wake pushes; D-SDK-16): apps call it at every launch with their vendor's token, and
// the core posts it to POST /sidecar/push-token on the sidecar's own client token, so the server's row and its wake
// pushes are this client's. "" deletes the token. The token is the vendor registration's credential: it goes in the
// request body alone, and no result or error here names it (I-7).
//
// The local backend posts through the core (Sidecar.SetPushToken), which holds the generation's token whether or not
// this process minted it; the remote backend's daemon registers no app token (SDK-M8-02).

package sdk

import (
	"errors"

	"github.com/urnetwork/sidecar"
)

// The platforms SetPushToken takes, as §18.5 names them.
const (
	SidecarPushPlatformApns = sidecar.PushTokenPlatformApns
	SidecarPushPlatformFcm  = sidecar.PushTokenPlatformFcm
)

// The remote backend registers no app's wake token.
var errSidecarRemotePushToken = errors.New("a remote-mode sidecar's daemon takes no app wake token")

// A platform or token the route does not take, refused before anything is sent.
var errSidecarPushTokenInvalid = errors.New("the push token's platform is apns or fcm and its token printable ASCII")

// Sets this sidecar's wake token for platform, apns or fcm, or deletes it with an empty token.
func (self *Sidecar) SetPushToken(platform string, token string, callback SidecarResultCallback) {
	trace(func() {
		local, remote := self.backends()
		go func() {
			result, err := traceWithReturnError(func() (*SidecarResult, error) {
				switch {
				case remote != nil:
					return sidecarResultOf("", errSidecarRemotePushToken), nil
				case local == nil:
					return sidecarResultOf("", sidecar.ErrSidecarClosed), nil
				}
				if token == "" && !sidecar.ValidPushTokenDelete(platform) || token != "" && !sidecar.ValidPushToken(platform, token) {
					return sidecarResultOf("", errSidecarPushTokenInvalid), nil
				}
				if err := local.SetPushToken(self.ctx, platform, token); err != nil {
					// the route's refusal names the route alone, never the token
					return sidecarResultOf("", err), nil
				}
				return sidecarResultOf("", nil), nil
			})
			if callback != nil {
				callback.Result(result, err)
			}
		}()
	})
}
