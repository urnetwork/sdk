package sdk

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect"
)

// A sidecar token's DPoP proofs through the sdk's Api (api_dpop.go; §7.4,
// §18.7). A local server stands in for api and reads each request's proof;
// the signer is a test's, over a fixed key, as sdk/sidecar.go installs the
// core's.

// A sidecar's client token as the server mints one: it names a client and
// carries strict_exp.
func dpopTestStrictToken(t *testing.T) string {
	t.Helper()
	return credentialTestJwt(t, gojwt.MapClaims{
		"network_id": credentialTestNetworkId,
		"user_id":    credentialTestUserId,
		"client_id":  credentialTestClientId,
		"device_id":  credentialTestDeviceId,
		"strict_exp": true,
	})
}

// An ordinary client token: it names a client and has no strict_exp.
func dpopTestClientToken(t *testing.T) string {
	t.Helper()
	return credentialTestJwt(t, gojwt.MapClaims{
		"network_id": credentialTestNetworkId,
		"user_id":    credentialTestUserId,
		"client_id":  credentialTestClientId,
		"device_id":  credentialTestDeviceId,
	})
}

// The test's signer, over a fixed seed: §7.4's header and claims, as the core's
// signer makes them.
func dpopTestSigner(t *testing.T) (dpopSignerFunc, ed25519.PublicKey) {
	t.Helper()
	privateKey := ed25519.NewKeyFromSeed([]byte(strings.Repeat("d", ed25519.SeedSize)))
	publicKey := privateKey.Public().(ed25519.PublicKey)
	jti := atomic.Int64{}
	signer := func(method string, url string, ath string) (string, error) {
		claims := gojwt.MapClaims{
			"jti": base64.RawURLEncoding.EncodeToString([]byte{byte(jti.Add(1))}),
			"htm": method,
			"htu": strings.SplitN(strings.SplitN(url, "?", 2)[0], "#", 2)[0],
			"iat": time.Now().Unix(),
			"ath": ath,
		}
		token := gojwt.NewWithClaims(gojwt.SigningMethodEdDSA, claims)
		token.Header["typ"] = "dpop+jwt"
		token.Header["jwk"] = map[string]any{"kty": "OKP", "crv": "Ed25519", "x": base64.RawURLEncoding.EncodeToString(publicKey)}
		return token.SignedString(privateKey)
	}
	return signer, publicKey
}

// What the server saw of each request.
type dpopTestRequest struct {
	method        string
	path          string
	authorization string
	proof         string
}

type dpopTestServer struct {
	stateLock sync.Mutex
	requests  []dpopTestRequest
	// the answer to each request, by path
	answer func(w http.ResponseWriter, r *http.Request)
}

func (self *dpopTestServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.requests = append(self.requests, dpopTestRequest{
			method:        r.Method,
			path:          r.URL.Path,
			authorization: r.Header.Get("Authorization"),
			proof:         r.Header.Get("DPoP"),
		})
	}()
	self.answer(w, r)
}

func (self *dpopTestServer) seen() []dpopTestRequest {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]dpopTestRequest{}, self.requests...)
}

// Checks a proof the way api does, short of the replay store: the signer's key,
// the request's method and uri, and the token's hash.
func requireDpopTestProof(t *testing.T, proof string, publicKey ed25519.PublicKey, method string, url string, token string) {
	t.Helper()
	claims := gojwt.MapClaims{}
	parsed, err := gojwt.NewParser(gojwt.WithValidMethods([]string{"EdDSA"})).ParseWithClaims(proof, claims, func(token *gojwt.Token) (any, error) {
		return publicKey, nil
	})
	if err != nil || !parsed.Valid {
		t.Fatalf("the proof does not verify: %v", err)
	}
	if parsed.Header["typ"] != "dpop+jwt" {
		t.Fatalf("typ = %v", parsed.Header["typ"])
	}
	hash := sha256.Sum256([]byte(token))
	if claims["htm"] != method || claims["htu"] != url || claims["ath"] != base64.RawURLEncoding.EncodeToString(hash[:]) || claims["jti"] == "" {
		t.Fatalf("claims = %v, want %s %s over the token", claims, method, url)
	}
}

// A strict_exp refresh without a signer is not sent at all: the request never
// leaves, the error says why, and the token is kept, unenrolled rather than
// proofless. An ordinary client token refreshes as it always did, with no proof.
func TestStrictExpRefreshIsNotSentWithoutASigner(t *testing.T) {
	server := &dpopTestServer{answer: func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"by_jwt":"` + dpopTestClientToken(t) + `"}`))
	}}
	_, api := newTestApi(t, server)
	strict := dpopTestStrictToken(t)
	api.SetByJwt(strict)

	_, err := api.RefreshJwtSyncWithContext(t.Context())
	if !errors.Is(err, ErrDpopSignerRequired) {
		t.Fatalf("err = %v, want the signer required", err)
	}
	if len(server.seen()) != 0 {
		t.Fatalf("sent %v without a signer", server.seen())
	}
	if api.GetByJwt() != strict {
		t.Fatal("the token was dropped")
	}

	ordinary := dpopTestClientToken(t)
	api.SetByJwt(ordinary)
	if _, err := api.RefreshJwtSyncWithContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	seen := server.seen()
	if len(seen) != 1 || seen[0].proof != "" || seen[0].authorization != "Bearer "+ordinary {
		t.Fatalf("an ordinary refresh sent %v", seen)
	}
}

// With a signer, a strict_exp refresh carries one proof by its key, over the
// request's method and url and the token it presents, through the strategy's
// dial.
func TestStrictExpRefreshCarriesAProof(t *testing.T) {
	server := &dpopTestServer{answer: func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"by_jwt":"` + dpopTestStrictToken(t) + `"}`))
	}}
	_, api := newTestApi(t, server)
	signer, publicKey := dpopTestSigner(t)
	api.SetDpopSigner(signer)
	strict := dpopTestStrictToken(t)
	api.SetByJwt(strict)

	if _, err := api.RefreshJwtSyncWithContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	seen := server.seen()
	if len(seen) != 1 || seen[0].method != http.MethodGet || seen[0].path != "/auth/refresh" || seen[0].authorization != "Bearer "+strict {
		t.Fatalf("sent %v", seen)
	}
	requireDpopTestProof(t, seen[0].proof, publicKey, http.MethodGet, api.apiUrl+"/auth/refresh", strict)

	// removing the signer stops the next refresh before it is sent
	api.SetDpopSigner(nil)
	if _, err := api.RefreshJwtSyncWithContext(t.Context()); !errors.Is(err, ErrDpopSignerRequired) {
		t.Fatalf("err = %v, want the signer required", err)
	}
	if len(server.seen()) != 1 {
		t.Fatal("a refresh was sent after the signer was removed")
	}
}

// A sidecar token mints a child on POST /network/auth-client with its proof, and
// sends none on a route that wants none.
func TestStrictExpMintCarriesAProof(t *testing.T) {
	server := &dpopTestServer{answer: func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"client_id":"` + credentialTestKeyClientId + `","by_client_jwt":"child"}`))
	}}
	_, api := newTestApi(t, server)
	signer, publicKey := dpopTestSigner(t)
	strict := dpopTestStrictToken(t)
	api.SetByJwt(strict)

	// without a signer nothing is sent
	if _, err := api.AuthNetworkClientSyncWithContext(t.Context(), &AuthNetworkClientArgs{DeviceDescription: "child"}); !errors.Is(err, ErrDpopSignerRequired) {
		t.Fatalf("err = %v, want the signer required", err)
	}
	if len(server.seen()) != 0 {
		t.Fatal("a mint was sent without a signer")
	}

	api.SetDpopSigner(signer)
	if _, err := api.AuthNetworkClientSyncWithContext(t.Context(), &AuthNetworkClientArgs{DeviceDescription: "child"}); err != nil {
		t.Fatal(err)
	}
	seen := server.seen()
	if len(seen) != 1 || seen[0].method != http.MethodPost || seen[0].path != "/network/auth-client" {
		t.Fatalf("sent %v", seen)
	}
	requireDpopTestProof(t, seen[0].proof, publicKey, http.MethodPost, api.apiUrl+"/network/auth-client", strict)

	// a route outside §7.4's proof routes carries none
	if _, err := api.getHttpPostRaw()(t.Context(), api.apiUrl+"/connect/control", []byte(`{}`), strict); err != nil {
		t.Fatal(err)
	}
	seen = server.seen()
	if len(seen) != 2 || seen[1].path != "/connect/control" || seen[1].proof != "" {
		t.Fatalf("sent %v", seen)
	}
}

// A signer that fails sends nothing, and its error does not leave the api.
func TestFailedDpopSignerSendsNothing(t *testing.T) {
	server := &dpopTestServer{answer: func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}}
	_, api := newTestApi(t, server)
	api.SetByJwt(dpopTestStrictToken(t))
	api.SetDpopSigner(func(method string, url string, ath string) (string, error) {
		return "", errors.New("the key " + url + " is locked")
	})
	_, err := api.RefreshJwtSyncWithContext(t.Context())
	if !errors.Is(err, ErrDpopProofFailed) || strings.Contains(err.Error(), "locked") {
		t.Fatalf("err = %v", err)
	}
	api.SetDpopSigner(func(method string, url string, ath string) (string, error) {
		return "", nil
	})
	if _, err := api.RefreshJwtSyncWithContext(t.Context()); !errors.Is(err, ErrDpopProofFailed) {
		t.Fatalf("err = %v", err)
	}
	if len(server.seen()) != 0 {
		t.Fatalf("sent %v", server.seen())
	}
}

// The proof's request reaches whatever transport is installed, which tests
// replace; it answers with connect's status error, so the callers' status
// handling holds.
func TestDpopRequestUsesTheInstalledTransport(t *testing.T) {
	api := newApi(t.Context(), nil, "https://api.test")
	defer api.Close()
	signer, _ := dpopTestSigner(t)
	api.SetDpopSigner(signer)
	api.SetByJwt(dpopTestStrictToken(t))

	// with no strategy there is no dial, and nothing is sent
	if _, err := api.RefreshJwtSyncWithContext(t.Context()); err == nil {
		t.Fatal("a refresh with no strategy succeeded")
	}

	calls := 0
	api.setHttpDpopRaw(func(ctx context.Context, method string, requestUrl string, requestBodyBytes []byte, byJwt string, proof string) ([]byte, error) {
		calls += 1
		if method != http.MethodGet || requestUrl != "https://api.test/auth/refresh" || proof == "" || requestBodyBytes != nil {
			t.Fatalf("%s %s proof %q", method, requestUrl, proof)
		}
		return nil, &connect.HttpStatusError{StatusCode: http.StatusForbidden, Status: "403 Forbidden", Body: []byte("A sidecar token needs a proof of its client key on this route.")}
	})
	_, err := api.RefreshJwtSyncWithContext(t.Context())
	var status *connect.HttpStatusError
	if calls != 1 || !errors.As(err, &status) || status.StatusCode != http.StatusForbidden {
		t.Fatalf("calls = %d, err = %v", calls, err)
	}
}
