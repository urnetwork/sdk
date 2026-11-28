package hotspot

// credentials.go — the user name and password the shared devices sign in
// with. They are generated on the phone, shown in the app, and checked on every
// request before anything is dialed: Proxy-Authorization Basic for HTTP and
// the SOCKS5 user/password method (RFC 1929), never "no authentication".

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"math/big"
	"net/http"
	"strings"
)

// The user name prefix, so a shared device's settings name the app.
const hotspotProxyUsernamePrefix = "ur-"

// The alphabets of the generated credentials. Letters and digits only, so the
// credentials sit in a proxy URL without escaping and type easily on a laptop.
const (
	hotspotProxyUsernameAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	hotspotProxyPasswordAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
)

var errHotspotProxyUnauthenticated = errors.New("proxy credentials required")

// A random string of the alphabet from crypto/rand, each character uniform.
func randomHotspotProxyString(alphabet string, length int) (string, error) {
	if length <= 0 {
		return "", errors.New("credential length must be positive")
	}
	var b strings.Builder
	alphabetSize := big.NewInt(int64(len(alphabet)))
	for range length {
		i, err := rand.Int(rand.Reader, alphabetSize)
		if err != nil {
			return "", err
		}
		b.WriteByte(alphabet[i.Int64()])
	}
	return b.String(), nil
}

// A new user name and password.
func newHotspotProxyCredentials(settings *hotspotProxySettings) (username string, password string, err error) {
	usernameRandom, err := randomHotspotProxyString(hotspotProxyUsernameAlphabet, settings.usernameLength)
	if err != nil {
		return "", "", err
	}
	password, err = randomHotspotProxyString(hotspotProxyPasswordAlphabet, settings.passwordLength)
	if err != nil {
		return "", "", err
	}
	return hotspotProxyUsernamePrefix + usernameRandom, password, nil
}

// Whether the offered credentials are the expected ones. Both parts are always
// compared, in constant time for equal lengths. Empty expected credentials
// match nothing.
func hotspotProxyCredentialsMatch(
	username string,
	password string,
	expectedUsername string,
	expectedPassword string,
) bool {
	if expectedUsername == "" || expectedPassword == "" {
		return false
	}
	usernameMatch := subtle.ConstantTimeCompare([]byte(username), []byte(expectedUsername))
	passwordMatch := subtle.ConstantTimeCompare([]byte(password), []byte(expectedPassword))
	return usernameMatch&passwordMatch == 1
}

// The credentials of a request's Proxy-Authorization Basic header. ok is false
// when it has none or the header does not parse.
func httpProxyRequestCredentials(r *http.Request) (username string, password string, ok bool) {
	scheme, encoded, found := strings.Cut(r.Header.Get("Proxy-Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "Basic") {
		return "", "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return "", "", false
	}
	return strings.Cut(string(decoded), ":")
}
