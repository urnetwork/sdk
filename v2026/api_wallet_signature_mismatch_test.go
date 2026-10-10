package sdk

// Wallet sign-in, network create and add-auth against a scripted api: what
// reaches the apps when the server refuses a wallet signature made with another
// account than the address entered (TAO.com manual entry). For a request that
// sets result_errors the server answers it with error.code signature_mismatch:
// sign-in in the result with a 200, network create in the body of the refusal's
// 401 (the signup monitor counts every 2xx create as a created network), and
// add-auth always in its result.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

const walletSignatureMismatchMessage = "The signature does not match this wallet address. Sign the challenge with this address."

// The coded refusal as the server writes it.
var walletSignatureMismatchBody = fmt.Sprintf(`{"error":{"code":"signature_mismatch","message":%q}}`, walletSignatureMismatchMessage)

// A scripted api that answers `path` with `status`, `contentType` and `body`,
// and hands the request body it read to the test.
func newWalletSignatureMismatchApi(t *testing.T, path string, status int, contentType string, body string) (*Api, chan map[string]any) {
	t.Helper()
	requestBodies := make(chan map[string]any, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			http.Error(w, "unexpected route", http.StatusNotFound)
			return
		}
		raw, err := io.ReadAll(r.Body)
		requestBody := map[string]any{}
		if err == nil {
			err = json.Unmarshal(raw, &requestBody)
		}
		if err != nil {
			http.Error(w, "unreadable body", http.StatusBadRequest)
			return
		}
		requestBodies <- requestBody
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		io.WriteString(w, body)
	})
	_, api := newTestApi(t, handler)
	return api, requestBodies
}

// Waits for an async api call's one callback.
func awaitWalletApiAnswer[R any](t *testing.T, call func(connect.ApiCallback[R])) (R, error) {
	t.Helper()
	type answer struct {
		result R
		err    error
	}
	answers := make(chan answer, 1)
	call(connect.NewApiCallback(func(result R, err error) {
		answers <- answer{result: result, err: err}
	}))
	select {
	case a := <-answers:
		return a.result, a.err
	case <-time.After(30 * time.Second):
		t.Fatal("the api call never answered")
	}
	var empty R
	return empty, nil
}

// Waits for an async api call's one callback, which must be a result.
func awaitWalletApiResult[R any](t *testing.T, call func(connect.ApiCallback[R])) R {
	t.Helper()
	result, err := awaitWalletApiAnswer(t, call)
	if err != nil {
		t.Fatalf("api error: %v", err)
	}
	return result
}

// A Bittensor wallet proof with synthetic values; the scripted api reads none of
// them.
func walletSignatureMismatchAuth() *WalletAuthArgs {
	return &WalletAuthArgs{
		PublicKey:  "synthetic-coldkey",
		Signature:  "0x" + fmt.Sprintf("%0128x", 1),
		Message:    "Sign in to URnetwork\nChallenge: c3ludGhldGlj\nTimestamp: 1",
		Blockchain: string(TAO),
	}
}

// A wallet sign-in asks for result_errors and keeps the refusal's code.
func TestAuthLoginKeepsTheSignatureMismatchCode(t *testing.T) {
	api, requestBodies := newWalletSignatureMismatchApi(t, "/auth/login", http.StatusOK, "application/json", walletSignatureMismatchBody)
	result := awaitWalletApiResult(t, func(callback connect.ApiCallback[*AuthLoginResult]) {
		api.AuthLogin(&AuthLoginArgs{WalletAuth: walletSignatureMismatchAuth(), ResultErrors: true}, callback)
	})
	if requestBody := <-requestBodies; requestBody["result_errors"] != true {
		t.Fatalf("sign-in did not ask for result_errors: %v", requestBody)
	}
	if result == nil || result.Error == nil || result.Network != nil {
		t.Fatalf("sign-in = %+v, want only the refusal", result)
	}
	connect.AssertEqual(t, result.Error.Code, WalletAuthErrorCodeSignatureMismatch)
	connect.AssertEqual(t, result.Error.Message, walletSignatureMismatchMessage)
	// the subnet wallet's code for the same refusal
	connect.AssertEqual(t, WalletAuthErrorCodeSignatureMismatch, SnErrorCodeSignatureMismatch)
}

// A wallet network create asks for result_errors and reads the coded refusal
// from the body of its 401 as the result. A refusal without a code, a 401 for a
// request without result_errors and a server error stay errors; a 200 result
// reads as before.
func TestNetworkCreateKeepsTheSignatureMismatchCode(t *testing.T) {
	uncodedBody := fmt.Sprintf(`{"error":{"message":%q}}`, walletSignatureMismatchMessage)
	tests := []struct {
		resultErrors bool
		status       int
		contentType  string
		body         string
		// the code the result carries; "" when the create must fail
		code string
	}{
		{resultErrors: true, status: http.StatusUnauthorized, contentType: "application/json", body: walletSignatureMismatchBody, code: WalletAuthErrorCodeSignatureMismatch},
		{resultErrors: true, status: http.StatusOK, contentType: "application/json", body: walletSignatureMismatchBody, code: WalletAuthErrorCodeSignatureMismatch},
		{resultErrors: true, status: http.StatusUnauthorized, contentType: "text/plain; charset=utf-8", body: walletSignatureMismatchMessage + "\n"},
		{resultErrors: true, status: http.StatusUnauthorized, contentType: "application/json", body: uncodedBody},
		{resultErrors: false, status: http.StatusUnauthorized, contentType: "application/json", body: walletSignatureMismatchBody},
		{resultErrors: true, status: http.StatusInternalServerError, contentType: "application/json", body: walletSignatureMismatchBody},
	}
	for _, test := range tests {
		api, requestBodies := newWalletSignatureMismatchApi(t, "/auth/network-create", test.status, test.contentType, test.body)
		result, err := awaitWalletApiAnswer(t, func(callback connect.ApiCallback[*NetworkCreateResult]) {
			api.NetworkCreate(&NetworkCreateArgs{
				NetworkName:  "synthetic-network",
				Terms:        true,
				WalletAuth:   walletSignatureMismatchAuth(),
				ResultErrors: test.resultErrors,
			}, callback)
		})
		requestBody := <-requestBodies
		if (requestBody["result_errors"] == true) != test.resultErrors {
			t.Errorf("status %d: result_errors in the request = %v, want %t", test.status, requestBody["result_errors"], test.resultErrors)
		}
		// the custom marshaling keeps the product updates line
		connect.AssertEqual(t, requestBody["product_updates"], true)
		if test.code == "" {
			var statusErr *connect.HttpStatusError
			if err == nil || !errors.As(err, &statusErr) || statusErr.StatusCode != test.status {
				t.Errorf("status %d %s (result_errors %t): = %+v, %v, want the HTTP error", test.status, test.body, test.resultErrors, result, err)
			}
			continue
		}
		if err != nil || result == nil || result.Error == nil || result.Network != nil {
			t.Errorf("status %d (result_errors %t): = %+v, %v, want only the refusal", test.status, test.resultErrors, result, err)
			continue
		}
		connect.AssertEqual(t, result.Error.Code, test.code)
		connect.AssertEqual(t, result.Error.Message, walletSignatureMismatchMessage)
	}
}

// An added wallet sign-in method keeps the refusal's code (add-auth always
// answers in its result).
func TestAddAuthKeepsTheSignatureMismatchCode(t *testing.T) {
	api, requestBodies := newWalletSignatureMismatchApi(t, "/auth/add-auth", http.StatusOK, "application/json", walletSignatureMismatchBody)
	api.SetByJwt("synthetic-bearer")
	result := awaitWalletApiResult(t, func(callback connect.ApiCallback[*AddAuthResult]) {
		api.AddAuth(&AddAuthArgs{WalletAuth: walletSignatureMismatchAuth()}, callback)
	})
	<-requestBodies
	if result == nil || result.Error == nil {
		t.Fatalf("add-auth = %+v, want the refusal", result)
	}
	connect.AssertEqual(t, result.Error.Code, WalletAuthErrorCodeSignatureMismatch)
	connect.AssertEqual(t, result.Error.Message, walletSignatureMismatchMessage)
}
