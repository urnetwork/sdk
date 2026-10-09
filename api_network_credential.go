package sdk

// Which credential an API request carries (AUTHZ-CLIENT.md).
//
// The network credential is the network's sign-in token (a by_jwt without a
// client_id) or an API key. A client token is the by_jwt of one client. A
// device installs its client token as the API's credential when it starts,
// and a device that an embed backend provisions holds nothing else. The server
// refuses a client token on the routes that administer the network or the
// account (server api/route_authz.go, AUTHZ1.md). The API keeps the network
// credential beside the device's client token and sends it on those routes
// only; every other request carries the API's current credential. Without a
// network credential an admin request fails before it is sent. It never falls
// back to the client token.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect"
)

// ErrNetworkCredentialRequired is the error of an API call that administers
// the network or the account when the API holds no network credential. The
// request is not sent.
var ErrNetworkCredentialRequired = errors.New("this call administers the network or the account and needs the network credential (the network's sign-in token or an API key); the api holds none")

type apiRouteAccess int

const (
	// any credential: the public, client and own-client routes. On an
	// own-client route the server limits a client token to its own client. The
	// own-client payout routes (a provider client's subnet wallet) belong here
	// too: the provider's client token is their credential, and the server
	// refuses it only for an Embed network, whose backend never sends it there.
	apiRouteAccessClient apiRouteAccess = iota
	// network or account administration that the URnetwork apps sent with the
	// device's client token before this sdk. The server refuses a client token
	// here for an Embed network, and for every network once those app builds
	// are gone (AUTHZ-CLIENT.md)
	apiRouteAccessAppAdmin
	// network or account administration: the server refuses every client token
	apiRouteAccessNetwork
)

// apiAdminRouteAccess is the App admin and Network only classes of server
// api/route_authz.go, keyed the same way ("METHOD pattern"). Every other route
// takes any credential. TestApiAdminRoutesMatchTheServer compares the two
// tables when the server checkout is beside the sdk.
var apiAdminRouteAccess = map[string]apiRouteAccess{
	"POST /auth/network-delete":               apiRouteAccessAppAdmin,
	"POST /auth/code-create":                  apiRouteAccessAppAdmin,
	"POST /auth/add-auth":                     apiRouteAccessAppAdmin,
	"POST /auth/remove-auth":                  apiRouteAccessAppAdmin,
	"POST /auth/regenerate-seedphrase":        apiRouteAccessAppAdmin,
	"POST /auth/generate-seedphrase":          apiRouteAccessAppAdmin,
	"GET /network/clients":                    apiRouteAccessAppAdmin,
	"GET /network/user":                       apiRouteAccessAppAdmin,
	"POST /network/ranking-visibility":        apiRouteAccessAppAdmin,
	"POST /network/points-ranking-visibility": apiRouteAccessAppAdmin,
	"POST /network/emoji":                     apiRouteAccessAppAdmin,
	"POST /network/block-location":            apiRouteAccessAppAdmin,
	"POST /network/unblock-location":          apiRouteAccessAppAdmin,
	"POST /preferences/set-preferences":       apiRouteAccessAppAdmin,
	"POST /stripe/customer-portal":            apiRouteAccessAppAdmin,
	"POST /account/payout-wallet":             apiRouteAccessAppAdmin,
	"GET /account/payout-wallet":              apiRouteAccessAppAdmin,
	"POST /account/wallet":                    apiRouteAccessAppAdmin,
	"GET /account/wallets":                    apiRouteAccessAppAdmin,
	"POST /account/wallets/remove":            apiRouteAccessAppAdmin,
	"POST /account/wallets/verify-seeker":     apiRouteAccessAppAdmin,
	"GET /account/payments":                   apiRouteAccessAppAdmin,
	"GET /account/unlink-referral-network":    apiRouteAccessAppAdmin,
	"POST /account/set-referral":              apiRouteAccessAppAdmin,
	"POST /account/change-name":               apiRouteAccessAppAdmin,
	"POST /account/claim-name":                apiRouteAccessAppAdmin,
	"GET /account/balance-codes":              apiRouteAccessAppAdmin,

	"POST /auth/network-refresh":            apiRouteAccessNetwork,
	"GET /stats/providers":                  apiRouteAccessNetwork,
	"POST /stats/providers-last-n":          apiRouteAccessNetwork,
	"POST /stats/provider-last-n":           apiRouteAccessNetwork,
	"POST /stats/providers-overview-last-n": apiRouteAccessNetwork,
	"GET /stats/providers-overview-last-90": apiRouteAccessNetwork,
	"POST /stats/provider-last-90":          apiRouteAccessNetwork,
	"POST /network/register-client-v1":      apiRouteAccessNetwork,
	"POST /network/remove-clients":          apiRouteAccessNetwork,
	"GET /network/proxies":                  apiRouteAccessNetwork,
	"POST /network/user/update":             apiRouteAccessNetwork,
	"POST /network/client-data-cap":         apiRouteAccessNetwork,
	"GET /network/client-data-caps":         apiRouteAccessNetwork,
	"POST /network/client-acl-group":        apiRouteAccessNetwork,
	"GET /network/embed":                    apiRouteAccessNetwork,
	"GET /wallet/balance":                   apiRouteAccessNetwork,
	"POST /wallet/circle-init":              apiRouteAccessNetwork,
	"POST /wallet/circle-transfer-out":      apiRouteAccessNetwork,
	"POST /test/balance-drain":              apiRouteAccessNetwork,
	"POST /test/balance-restore":            apiRouteAccessNetwork,
	"GET /subscription/details":             apiRouteAccessNetwork,
	"POST /subscription/cancel":             apiRouteAccessNetwork,
	"POST /subscription/resume":             apiRouteAccessNetwork,
	"POST /device/add":                      apiRouteAccessNetwork,
	"POST /device/create-share-code":        apiRouteAccessNetwork,
	"GET /device/share-code/([^/]+)/qr.png": apiRouteAccessNetwork,
	"POST /device/share-status":             apiRouteAccessNetwork,
	"POST /device/confirm-share":            apiRouteAccessNetwork,
	"GET /device/associations":              apiRouteAccessNetwork,
	"POST /device/remove-association":       apiRouteAccessNetwork,
	"POST /device/set-association-name":     apiRouteAccessNetwork,
	"POST /sn/wallet/network-consent":       apiRouteAccessNetwork,
	"POST /sn/wallet/hotkey-consent":        apiRouteAccessNetwork,
	"POST /sn/wallet/hotkey-delegation":     apiRouteAccessNetwork,
	"POST /account/api-key":                 apiRouteAccessNetwork,
	"POST /account/api-key/remove":          apiRouteAccessNetwork,
	"GET /account/api-keys":                 apiRouteAccessNetwork,
	"POST /oauth/authorize":                 apiRouteAccessNetwork,
	"POST /oauth/consent":                   apiRouteAccessNetwork,
}

type apiAdminRoutePattern struct {
	method  string
	pattern *regexp.Regexp
	access  apiRouteAccess
}

// the admin routes with a path parameter, matched the way the server's router
// matches a route (an anchored regexp)
var apiAdminRoutePatterns = func() []apiAdminRoutePattern {
	patterns := []apiAdminRoutePattern{}
	for route, access := range apiAdminRouteAccess {
		method, pattern, _ := strings.Cut(route, " ")
		if regexp.QuoteMeta(pattern) == pattern {
			continue
		}
		patterns = append(patterns, apiAdminRoutePattern{
			method:  method,
			pattern: regexp.MustCompile("^" + pattern + "$"),
			access:  access,
		})
	}
	return patterns
}()

func apiRouteAccessFor(method string, path string) apiRouteAccess {
	if access, ok := apiAdminRouteAccess[method+" "+path]; ok {
		return access
	}
	for _, route := range apiAdminRoutePatterns {
		if route.method == method && route.pattern.MatchString(path) {
			return route.access
		}
	}
	return apiRouteAccessClient
}

// every API key starts with this. The server authenticates an API key as the
// network without reading it as a JWT.
const apiKeyPrefix = "urn_"

func unverifiedCredentialClaims(byJwt string) (gojwt.MapClaims, bool) {
	if byJwt == "" || strings.HasPrefix(byJwt, apiKeyPrefix) {
		return nil, false
	}
	claims := gojwt.MapClaims{}
	if _, _, err := gojwt.NewParser().ParseUnverified(byJwt, claims); err != nil {
		return nil, false
	}
	return claims, true
}

// credentialNamesClient reports whether a credential is a client token: a JWT
// whose claims, read without verifying the signature, name a client. The
// server's gate reads a bearer the same way (jwt.ByJwtNamesClientUnverified).
func credentialNamesClient(byJwt string) bool {
	claims, ok := unverifiedCredentialClaims(byJwt)
	if !ok {
		return false
	}
	clientId, present := claims["client_id"]
	return present && clientId != nil
}

// isNetworkCredential reports whether the server would take a credential as
// the network's own rather than a client's: a network token, an API key, or
// anything else that does not name a client
func isNetworkCredential(byJwt string) bool {
	return byJwt != "" && !credentialNamesClient(byJwt)
}

// credentialNetworkId is the network a JWT names, read unverified. Only the
// server knows the network of an API key.
func credentialNetworkId(byJwt string) (connect.Id, bool) {
	claims, ok := unverifiedCredentialClaims(byJwt)
	if !ok {
		return connect.Id{}, false
	}
	value, ok := claims["network_id"].(string)
	if !ok {
		return connect.Id{}, false
	}
	networkId, err := connect.ParseId(value)
	if err != nil || networkId == (connect.Id{}) {
		return connect.Id{}, false
	}
	return networkId, true
}

// sameNetworkOrUnknown is false only when both credentials name a network and
// the networks differ
func sameNetworkOrUnknown(a string, b string) bool {
	aNetworkId, aOk := credentialNetworkId(a)
	bNetworkId, bOk := credentialNetworkId(b)
	return !aOk || !bOk || aNetworkId == bNetworkId
}

// An explicit login replaces the whole session, so it also replaces the
// network credential: the login itself when it is one, else none. A client
// token that a caller sets with SetByJwt never inherits an earlier login's.
// No LocalState backs the login until a device starts from one, so renewal
// waits for that. Callers hold authMutationLock and mutex.
func (self *Api) setLoginNetworkByJwtWithLock(byJwt string) {
	if isNetworkCredential(byJwt) {
		self.networkByJwt = byJwt
	} else {
		self.networkByJwt = ""
	}
	self.networkByJwtStore = nil
	self.networkByJwtRejected = ""
	self.networkCredentialChangedWithLock()
}

// Keeps the network credential beside the client token a device installs.
// A device started after a relaunch adopts the sign-in token its LocalState
// pairs with the client (the apps save both at sign-in), unless the API
// already holds the login's. A network credential of another network than the
// device's client is dropped rather than sent for it, and one the server
// rejected is not adopted again. The LocalState backs the kept credential, and
// renews it, when it stores exactly that token beside the device's client
// token (api_network_credential_renewal.go). Reports whether the kept
// credential or its LocalState changed. Callers hold authMutationLock, the
// LocalState auth lock and mutex.
func (self *Api) keepNetworkByJwtForDeviceWithLock(prepared *deviceAuthStartup, deviceByJwt string) bool {
	networkByJwt := self.networkByJwt
	networkByJwtStore := self.networkByJwtStore
	if !sameNetworkOrUnknown(self.networkByJwt, deviceByJwt) {
		self.networkByJwt = ""
	}
	if self.networkByJwt == "" && prepared.localState != nil {
		storedByJwt := prepared.state.ByJwt
		// shipped Apple extensions stored their client token as by_jwt
		// (selectStartupClientJwt); a client token is never adopted
		if isNetworkCredential(storedByJwt) && sameNetworkOrUnknown(storedByJwt, deviceByJwt) &&
			storedByJwt != self.networkByJwtRejected {
			self.networkByJwt = storedByJwt
		}
	}
	self.networkByJwtStore = nil
	if prepared.localState != nil && self.networkByJwt != "" &&
		prepared.state.ByJwt == self.networkByJwt && credentialNamesClient(deviceByJwt) {
		self.networkByJwtStore = prepared.localState
	}
	if self.networkByJwt == networkByJwt && self.networkByJwtStore == networkByJwtStore {
		return false
	}
	self.networkCredentialChangedWithLock()
	return true
}

// networkCredentialChangedWithLock starts a new generation of the kept network
// credential: a renewal that read the old one is discarded, and renewal of a
// new credential starts over. Callers hold mutex.
func (self *Api) networkCredentialChangedWithLock() {
	self.networkByJwtGeneration += 1
	self.networkRenewalHalted = false
}

// networkCredential is the API's current credential when that is a network
// credential, else the one it kept beside a device's client token
func (self *Api) networkCredential() string {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if isNetworkCredential(self.byJwt) {
		return self.byJwt
	}
	return self.networkByJwt
}

// HasNetworkCredential reports whether the API can make the calls that
// administer the network or the account. Without a network credential they
// fail with ErrNetworkCredentialRequired, for example on a device that an
// embed backend provisioned with only a client token.
func (self *Api) HasNetworkCredential() bool {
	return self.networkCredential() != ""
}

// apiRequestPath is the route path of a request to this API, without its query,
// or false for a url outside this API
func (self *Api) apiRequestPath(requestUrl string) (string, bool) {
	baseUrl := strings.TrimRight(self.apiUrl, "/")
	if baseUrl == "" || !strings.HasPrefix(requestUrl, baseUrl+"/") {
		return "", false
	}
	path := requestUrl[len(baseUrl):]
	if i := strings.IndexAny(path, "?#"); 0 <= i {
		path = path[:i]
	}
	return path, true
}

// requestByJwt is the credential a request carries. A route that administers
// the network carries the network credential, even when the caller read the
// device's client token; every other request carries byJwt unchanged.
func (self *Api) requestByJwt(method string, requestUrl string, byJwt string) (string, error) {
	path, ok := self.apiRequestPath(requestUrl)
	if !ok || apiRouteAccessFor(method, path) == apiRouteAccessClient {
		return byJwt, nil
	}
	if isNetworkCredential(byJwt) {
		return byJwt, nil
	}
	if networkByJwt := self.networkCredential(); networkByJwt != "" {
		return networkByJwt, nil
	}
	return "", fmt.Errorf("%w: %s %s", ErrNetworkCredentialRequired, method, path)
}

// The request seams every API call goes through. Each sends the credential
// requestByJwt selects for its route over the installed transport.

func (self *Api) getHttpPostRaw() connect.HttpPostRawFunction {
	httpPostRaw := self.transportHttpPostRaw()
	return func(ctx context.Context, requestUrl string, requestBodyBytes []byte, byJwt string) ([]byte, error) {
		byJwt, err := self.requestByJwt(http.MethodPost, requestUrl, byJwt)
		if err != nil {
			return nil, err
		}
		return httpPostRaw(ctx, requestUrl, requestBodyBytes, byJwt)
	}
}

func (self *Api) getHttpGetRaw() connect.HttpGetRawFunction {
	httpGetRaw := self.transportHttpGetRaw()
	return func(ctx context.Context, requestUrl string, byJwt string) ([]byte, error) {
		byJwt, err := self.requestByJwt(http.MethodGet, requestUrl, byJwt)
		if err != nil {
			return nil, err
		}
		return httpGetRaw(ctx, requestUrl, byJwt)
	}
}

func (self *Api) getHttpPostStreamRaw() connect.HttpPostStreamRawFunction {
	httpPostStreamRaw := self.transportHttpPostStreamRaw()
	return func(ctx context.Context, requestUrl string, body io.Reader, byJwt string) ([]byte, error) {
		byJwt, err := self.requestByJwt(http.MethodPost, requestUrl, byJwt)
		if err != nil {
			return nil, err
		}
		return httpPostStreamRaw(ctx, requestUrl, body, byJwt)
	}
}
