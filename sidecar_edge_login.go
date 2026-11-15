//go:build !ios_extension && !js

// SignEdgeLogin (§18.5, §13.7's members-device audience, R-SDK-12, I-4; SDK-M6-03): the member's device vouches for
// one edge login. A members-device host's front, the member's ID token checked, issues a nonce at its origin and asks
// for the device's statement; the app or extension hands both here, the core signs §6's SIGNED_TLS_EDGE_LOGIN over
// exactly that origin and nonce under the device's identity key (Sidecar.SignTlsEdgeLogin, TLS-M6-06), and the result
// is the statement for the visitor's browser to post back.
//
//	binds      the statement names the origin and the nonce and nothing else, so it completes that one login at that
//	           one origin: a front refuses it at every other origin and for every other nonce
//	origin     the caller passes the origin of the page that asked, as the browser or the OS reports it, never one the
//	           page names for itself; it goes to the core as given, which signs only https://<a canonical host>, with
//	           a port other than 443 named
//	consent    only a member's consented device signs: the core refuses on a tagged, ephemeral, parked or read_only
//	           sidecar, before enrollment, and while it holds no verified state of its network, and nothing here
//	           signs in its place
//	remote     a remote-mode Sidecar's device is its daemon, and §5.8 has no method that asks it for this statement
//	           yet, so the call is refused there with nothing signed
//
// The nonce and the statement are one login's secrets: nothing here logs either, and a refusal's message repeats
// neither (I-7). Safe for concurrent use.

package sdk

import (
	"encoding/base64"
	"errors"
	"strings"
)

// What SignEdgeLogin holds its arguments to.
type sidecarEdgeLoginSettings struct {
	// The longest nonce text read: 128 B, several times a front's 32-byte nonce in base64. A longer one is refused
	// before it is decoded.
	NonceTextMaxByteCount int
}

// The defaults, a fresh value each call.
func defaultSidecarEdgeLoginSettings() *sidecarEdgeLoginSettings {
	return &sidecarEdgeLoginSettings{
		NonceTextMaxByteCount: 128,
	}
}

var (
	// The nonce is not base64, or is longer than any a front issues.
	errSidecarEdgeLoginNonce = errors.New("an edge login's nonce is the front's, in base64")
	// A remote-mode Sidecar: its daemon holds the device's key, and §5.8 has no method for this statement yet.
	errSidecarEdgeLoginRemote = errors.New("a remote-mode sidecar signs no edge login: the daemon's control mode has no method for it yet")
	// The Sidecar closed.
	errSidecarEdgeLoginClosed = errors.New("the sidecar is closed")
)

// The bytes text names: base64url as a front's challenge writes its nonce, padded or not, or standard base64.
func sidecarEdgeLoginNonceOf(text string, settings *sidecarEdgeLoginSettings) ([]byte, error) {
	if text == "" || settings.NonceTextMaxByteCount < len(text) {
		return nil, errSidecarEdgeLoginNonce
	}
	unpadded := strings.TrimRight(text, "=")
	for _, encoding := range []*base64.Encoding{base64.RawURLEncoding, base64.RawStdEncoding} {
		if nonce, err := encoding.Strict().DecodeString(unpadded); err == nil {
			return nonce, nil
		}
	}
	return nil, errSidecarEdgeLoginNonce
}

// Signs this device's statement for the members-device login a front issued the nonce nonceBase64 for at origin
// (§13.7). The result's Value is the statement, the base64url of the encoded Signed, which the visitor's browser
// posts as the device step's s. origin is the origin of the page that asked, never one the page names for itself.
// Only a member's consented device signs; every other answers a refusal, with nothing signed.
func (self *Sidecar) SignEdgeLogin(origin string, nonceBase64 string, callback SidecarResultCallback) {
	go func() {
		result, err := traceWithReturnError(func() (*SidecarResult, error) {
			statement, err := self.signEdgeLogin(origin, nonceBase64)
			return sidecarResultOf(statement, err), nil
		})
		if callback != nil {
			callback.Result(result, err)
		}
	}()
}

// SignEdgeLogin's statement, or why none was signed.
func (self *Sidecar) signEdgeLogin(origin string, nonceBase64 string) (string, error) {
	local, _ := self.backends()
	settings, closed := func() (*sidecarEdgeLoginSettings, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.edgeLoginSettings, self.closed
	}()
	if closed {
		return "", errSidecarEdgeLoginClosed
	}
	if local == nil {
		return "", errSidecarEdgeLoginRemote
	}
	if settings == nil {
		settings = defaultSidecarEdgeLoginSettings()
	}
	nonce, err := sidecarEdgeLoginNonceOf(nonceBase64, settings)
	if err != nil {
		return "", err
	}
	// the core checks the origin and the nonce's length, and that this device is its member's consented one
	signed, err := local.SignTlsEdgeLogin(origin, nonce)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(signed), nil
}
