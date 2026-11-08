package sdk

// api_auth_network_client_prompt_test.go -- what an app reads from a refused
// auth-client (client_limit_status.go): the prompt every refusal carries, and
// the server's 402 read as the refusal it stands for.

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// An Api whose auth-client calls run at once on the caller's goroutine and are
// answered by `answer`.
func newClientLimitPromptApi(t *testing.T, answer func() ([]byte, error)) *Api {
	t.Helper()
	api := newApi(t.Context(), nil, "https://client-limit.example")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := api.CloseAndWait(ctx); err != nil {
			t.Errorf("close client limit API: %v", err)
		}
	})
	api.authNetworkClientLaunch = func(request func()) {
		request()
	}
	api.setHttpPostRaw(func(_ context.Context, requestUrl string, _ []byte, _ string) ([]byte, error) {
		if requestUrl != "https://client-limit.example/network/auth-client" {
			t.Errorf("auth-client route = %q", requestUrl)
		}
		return answer()
	})
	return api
}

// The public method's one callback, which the launch above runs before
// AuthNetworkClient returns.
func authNetworkClientNow(t *testing.T, api *Api) (*AuthNetworkClientResult, error) {
	t.Helper()
	called := false
	var result *AuthNetworkClientResult
	var err error
	api.AuthNetworkClient(&AuthNetworkClientArgs{}, connect.NewApiCallback(func(r *AuthNetworkClientResult, e error) {
		called = true
		result, err = r, e
	}))
	if !called {
		t.Fatal("the auth-client callback did not run")
	}
	return result, err
}

const clientLimitPromptTerms = `{"x402Version":1,"error":"Your plan's concurrent client limit is reached. Upgrade to UR Pro to connect more clients.","accepts":[{"scheme":"exact","network":"base"}]}`

// x402 has been on in production since 2026-10-27, and auth-client answered a
// free network at its concurrent client limit with 402 and payment terms. The
// sdk passed the 402 on as a failed request, so the apps showed the raw terms
// or a generic error, never their upgrade prompt. The 402 now reads as the
// refusal with upgrade_required and the terms' message.
func TestAuthNetworkClientReadsPaymentRequiredAsTheUpgradeRefusal(t *testing.T) {
	api := newClientLimitPromptApi(t, func() ([]byte, error) {
		return nil, &connect.HttpStatusError{
			StatusCode: http.StatusPaymentRequired,
			Status:     "402 Payment Required",
			Body:       []byte(clientLimitPromptTerms),
		}
	})

	result, err := authNetworkClientNow(t, api)
	if err != nil {
		t.Fatalf("the 402 reached the app as a failed request: %v", err)
	}
	if result == nil || result.Error == nil {
		t.Fatalf("result = %+v, want the refusal", result)
	}
	connect.AssertEqual(t, result.Error.UpgradeRequired, true)
	connect.AssertEqual(t, result.Error.Message, "Your plan's concurrent client limit is reached. Upgrade to UR Pro to connect more clients.")
	connect.AssertEqual(t, result.Error.ClientLimitPrompt, ClientLimitPromptUpgrade)
	connect.AssertEqual(t, result.ByClientJwt, "")
	connect.AssertEqual(t, result.ClientId == nil, true)

	// the headless form reads it the same
	result, err = api.AuthNetworkClientSync(&AuthNetworkClientArgs{})
	if err != nil {
		t.Fatalf("the 402 reached the headless caller as a failed request: %v", err)
	}
	connect.AssertEqual(t, result.Error.UpgradeRequired, true)
	connect.AssertEqual(t, result.Error.ClientLimitPrompt, ClientLimitPromptUpgrade)

	// terms that do not parse still mean an upgrade lifts the refusal
	api = newClientLimitPromptApi(t, func() ([]byte, error) {
		return nil, &connect.HttpStatusError{StatusCode: http.StatusPaymentRequired, Status: "402 Payment Required", Body: []byte("payment required")}
	})
	result, err = authNetworkClientNow(t, api)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Error.ClientLimitPrompt, ClientLimitPromptUpgrade)
}

// A 402 is the only status that stands for a refusal: every other failure
// reaches the caller as the same error.
func TestAuthNetworkClientKeepsEveryOtherFailure(t *testing.T) {
	for _, failure := range []error{
		&connect.HttpStatusError{StatusCode: http.StatusInternalServerError, Status: "500 Internal Server Error", Body: []byte("synthetic")},
		&connect.HttpStatusError{StatusCode: http.StatusUnauthorized, Status: "401 Unauthorized", Body: []byte("Not authorized.")},
		errors.New("synthetic transport failure"),
	} {
		api := newClientLimitPromptApi(t, func() ([]byte, error) {
			return nil, failure
		})
		result, err := authNetworkClientNow(t, api)
		if result != nil || err != failure {
			t.Errorf("failure %v reached the caller as (%+v, %v)", failure, result, err)
		}
	}
}

// The server's refusals carry the prompt the apps show: the upgrade when an
// upgrade lifts the refusal, a freed slot for a client limit no upgrade lifts,
// and nothing for a refusal that is no limit. A success carries no refusal.
func TestAuthNetworkClientRefusalCarriesItsPrompt(t *testing.T) {
	cases := []struct {
		body   string
		prompt string
	}{
		{body: `{"error":{"client_limit_exceeded":true,"upgrade_required":true,"message":"m"}}`, prompt: ClientLimitPromptUpgrade},
		{body: `{"error":{"client_limit_exceeded":true,"message":"m"}}`, prompt: ClientLimitPromptRemoveClient},
		{body: `{"error":{"client_limit_exceeded":false,"message":"Invalid location"}}`, prompt: ClientLimitPromptNone},
	}
	for _, c := range cases {
		api := newClientLimitPromptApi(t, func() ([]byte, error) {
			return []byte(c.body), nil
		})
		result, err := authNetworkClientNow(t, api)
		if err != nil || result == nil || result.Error == nil {
			t.Fatalf("%s: (%+v, %v), want the refusal", c.body, result, err)
		}
		if result.Error.ClientLimitPrompt != c.prompt {
			t.Errorf("%s: prompt = %q, want %q", c.body, result.Error.ClientLimitPrompt, c.prompt)
		}
	}

	api := newClientLimitPromptApi(t, func() ([]byte, error) {
		return []byte(`{"by_client_jwt":"synthetic-client"}`), nil
	})
	result, err := authNetworkClientNow(t, api)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Error, (*AuthNetworkClientError)(nil))
	connect.AssertEqual(t, result.ByClientJwt, "synthetic-client")
}

// Pure: the prompt of a refusal, nil included.
func TestClientLimitPromptForRefusal(t *testing.T) {
	cases := []struct {
		refusal *AuthNetworkClientError
		prompt  string
	}{
		{refusal: nil, prompt: ClientLimitPromptNone},
		{refusal: &AuthNetworkClientError{ClientLimitExceeded: true, UpgradeRequired: true}, prompt: ClientLimitPromptUpgrade},
		// a Pro-only feature: the same upgrade flow lifts it
		{refusal: &AuthNetworkClientError{UpgradeRequired: true}, prompt: ClientLimitPromptUpgrade},
		{refusal: &AuthNetworkClientError{ClientLimitExceeded: true}, prompt: ClientLimitPromptRemoveClient},
		{refusal: &AuthNetworkClientError{Message: "Invalid location"}, prompt: ClientLimitPromptNone},
	}
	for _, c := range cases {
		if prompt := clientLimitPromptForRefusal(c.refusal); prompt != c.prompt {
			t.Errorf("%+v: prompt = %q, want %q", c.refusal, prompt, c.prompt)
		}
	}
}
