package sdk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect"
)

// DPoP proofs of a sidecar's ClientKey (§7.4, Q-CORE-4, RFC 9449). A sidecar's
// token, a strict_exp client token, renews on GET /auth/refresh and mints on
// POST /network/auth-client only with a proof that it holds its ClientKey, so a
// copy of the token alone does neither. An sdk host keeps such a token in an Api
// session (§18.7), and sdk/sidecar.go installs the core's signer with
// SetDpopSigner before D-CORE-8's core refresher is switched off on any host.
//
// Without a signer, such a request is not sent at all: the token is unenrolled,
// not proofless, and its refresh fails with ErrDpopSignerRequired, which signs
// nothing out, so the refresh tries again once a signer is installed. Every
// other token, and every other route, is untouched.
//
// connect's raw functions add no header, so a request with a proof goes through
// a transport of its own: the strategy's dial and TLS settings under net/http,
// as the sidecar core's own api client does, never net/http's default transport,
// whose unbound sockets would follow a tunnel's route back into this process.
// Tests install another with setHttpDpopRaw.

// Signs one proof: the compact JWS of §7.4's header {typ dpop+jwt, alg EdDSA,
// jwk} and claims {jti, htm = method, htu = url without query or fragment, iat,
// ath}, by the ClientKey. ath is SHA-256 of the token, base64url.
type dpopSignerFunc func(method string, url string, ath string) (string, error)

// A request a sidecar's token needs a proof for, and no signer is installed. It
// is not a rejection of the token: nothing signs out.
var ErrDpopSignerRequired = errors.New("a sidecar token renews and mints only with a proof of its client key, and no signer is installed")

// The DPoP signer refused, or returned no proof. The request was not sent.
var ErrDpopProofFailed = errors.New("the dpop signer did not sign")

// The transport of a request that carries a proof: method to requestUrl with
// requestBodyBytes (nil for none), the bearer byJwt and the DPoP header proof.
// A status other than 200 is a *connect.HttpStatusError, as connect's raw
// functions answer, so the callers' status handling holds.
type apiHttpDpopRawFunction func(ctx context.Context, method string, requestUrl string, requestBodyBytes []byte, byJwt string, proof string) ([]byte, error)

// Installs the signer of this Api's proofs, replacing any. nil removes it, and
// then a sidecar's token is neither refreshed nor used to mint. Go only: the
// signer holds the core's ClientKey.
//
//gomobile:noexport
func (self *Api) SetDpopSigner(signer func(method string, url string, ath string) (string, error)) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if signer == nil {
		self.dpopSigner = nil
		return
	}
	self.dpopSigner = dpopSignerFunc(signer)
}

// The installed signer, or nil.
func (self *Api) getDpopSigner() dpopSignerFunc {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.dpopSigner
}

// Whether byJwt claims to be a sidecar's token. Unverified: the server decides
// what the token is, and this only decides whether to send a proof with it.
func byJwtStrictExpUnverified(byJwt string) bool {
	claims := gojwt.MapClaims{}
	if _, _, err := gojwt.NewParser().ParseUnverified(byJwt, claims); err != nil {
		return false
	}
	strictExp, _ := claims["strict_exp"].(bool)
	return strictExp
}

// ath for token: the unpadded base64url of SHA-256 over its bytes (RFC 9449
// §4.2).
func dpopAth(token string) string {
	hash := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(hash[:])
}

// Whether method to requestUrl is a route §7.4 wants a sidecar token's proof on:
// GET /auth/refresh and POST /network/auth-client of this Api's api. The token
// exchange is the core's own, on its own client.
func (self *Api) dpopRoute(method string, requestUrl string) bool {
	switch {
	case method == http.MethodGet && requestUrl == self.apiUrl+"/auth/refresh":
		return true
	case method == http.MethodPost && requestUrl == self.apiUrl+"/network/auth-client":
		return true
	default:
		return false
	}
}

// The proof a request this Api sends with byJwt must carry, "" for none: only a
// sidecar's token on a proof route needs one. With no signer installed that
// request is refused here, before anything is sent.
func (self *Api) dpopProof(method string, requestUrl string, byJwt string) (string, error) {
	if !self.dpopRoute(method, requestUrl) || !byJwtStrictExpUnverified(byJwt) {
		return "", nil
	}
	signer := self.getDpopSigner()
	if signer == nil {
		return "", ErrDpopSignerRequired
	}
	proof, err := signer(method, requestUrl, dpopAth(byJwt))
	if err != nil || proof == "" {
		// the signer's error may name what it signs over, so it stays out
		return "", ErrDpopProofFailed
	}
	return proof, nil
}

// Installs the transport of requests with a proof, for tests.
func (self *Api) setHttpDpopRaw(httpDpopRaw apiHttpDpopRawFunction) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.httpDpopRaw = httpDpopRaw
}

// The installed transport of requests with a proof, else one over this Api's
// client strategy, made once and kept for the Api's life.
func (self *Api) transportHttpDpopRaw() apiHttpDpopRawFunction {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.httpDpopRaw != nil {
		return self.httpDpopRaw
	}
	if self.dpopTransport == nil {
		self.dpopTransport = newApiDpopTransport(self.ctx, self.clientStrategy)
	}
	transport := self.dpopTransport
	return func(ctx context.Context, method string, requestUrl string, requestBodyBytes []byte, byJwt string, proof string) ([]byte, error) {
		return sendApiDpopRequest(ctx, transport, method, requestUrl, requestBodyBytes, byJwt, proof)
	}
}

// Refuses every request, closing its body as http.RoundTripper requires: an Api
// with no client strategy has no dial to send a proof's request on.
type noApiDpopTransport struct{}

func (self noApiDpopTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Body != nil {
		request.Body.Close()
	}
	return nil, errors.New("this api has no transport for a request with a proof")
}

// A transport on strategy's dial and TLS settings until ctx ends, dropping its
// idle connections when the network changes or memory runs short, as the
// strategy's own pools do.
func newApiDpopTransport(ctx context.Context, strategy *connect.ClientStrategy) http.RoundTripper {
	if strategy == nil {
		return noApiDpopTransport{}
	}
	connectSettings := strategy.ConnectSettings()
	transport := &http.Transport{
		// nil: no proxy from the environment, which would skip the egress binding
		Proxy:               nil,
		DialContext:         connectSettings.DialContext,
		TLSHandshakeTimeout: connectSettings.TlsTimeout,
		IdleConnTimeout:     connectSettings.IdleConnTimeout,
		ForceAttemptHTTP2:   true,
	}
	if connectSettings.TlsConfig != nil {
		transport.TLSClientConfig = connectSettings.TlsConfig.Clone()
	}
	unsubNetworkChange := connect.AddNetworkChangeListener(transport.CloseIdleConnections)
	unsubMemoryShed := connect.AddMemoryShedder(transport.CloseIdleConnections)
	go connect.HandleError(func() {
		<-ctx.Done()
		unsubNetworkChange()
		unsubMemoryShed()
		transport.CloseIdleConnections()
	})
	return transport
}

// Sends one request with its proof, as connect's raw functions send theirs:
// text/json, the client info header from ctx, the bearer, and a
// *connect.HttpStatusError for any status but 200.
func sendApiDpopRequest(ctx context.Context, transport http.RoundTripper, method string, requestUrl string, requestBodyBytes []byte, byJwt string, proof string) ([]byte, error) {
	var body io.Reader
	if requestBodyBytes != nil {
		body = bytes.NewReader(requestBodyBytes)
	}
	request, err := http.NewRequestWithContext(ctx, method, requestUrl, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "text/json")
	request.Header.Set(connect.ClientInfoHeader, connect.ClientInfoFromContext(ctx).Json())
	if byJwt != "" {
		request.Header.Set("Authorization", fmt.Sprintf("Bearer %s", byJwt))
	}
	request.Header.Set("DPoP", proof)
	client := &http.Client{
		Transport: transport,
		// a redirect would send the proof's request elsewhere
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, &connect.HttpStatusError{
			StatusCode: response.StatusCode,
			Status:     response.Status,
			Body:       responseBody,
		}
	}
	return responseBody, nil
}
