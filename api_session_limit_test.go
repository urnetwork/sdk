package sdk

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The server's refusal of a new session at the network's session cap: 409
// with its structured session error body (server session.SessionError).
func sessionLimitTestRefusal() error {
	return &connect.HttpStatusError{
		StatusCode: http.StatusConflict,
		Status:     "409 Conflict",
		Body:       []byte(`{"error":"Too many active sessions.","code":"session_limit_reached"}` + "\n"),
	}
}

func awaitSessionLimitTestResult[R any](t *testing.T, results <-chan R) R {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("the request did not answer")
		var empty R
		return empty
	}
}

// Every route that creates a session answers the cap refusal with its result,
// the error carrying SessionErrorCodeLimitReached, so the apps can say there
// are too many active sessions. They got a raw transport error before, which
// they showed as "409 Conflict: {...}" or as a generic failure.
func TestSessionLimitRefusalReadsAsTheResultErrorOfEveryMintRoute(t *testing.T) {
	api := newApi(t.Context(), nil, "https://api.example")
	defer api.Close()
	var paths []string
	api.setHttpPostRaw(func(_ context.Context, requestUrl string, _ []byte, _ string) ([]byte, error) {
		paths = append(paths, strings.TrimPrefix(requestUrl, "https://api.example"))
		return nil, sessionLimitTestRefusal()
	})
	type answer struct {
		code    string
		message string
		err     error
	}
	check := func(route string, a answer) {
		t.Helper()
		if a.err != nil {
			t.Fatalf("%s: the refusal stayed an error: %v", route, a.err)
		}
		if a.code != SessionErrorCodeLimitReached || a.message == "" {
			t.Fatalf("%s: refusal not in the result: %+v", route, a)
		}
	}
	answers := make(chan answer, 1)

	api.AuthLogin(&AuthLoginArgs{AuthJwt: "jwt", AuthJwtType: "google"}, connect.NewApiCallback(func(result *AuthLoginResult, err error) {
		a := answer{err: err}
		if result != nil && result.Error != nil {
			a.code, a.message = result.Error.Code, result.Error.Message
		}
		answers <- a
	}))
	check("/auth/login", awaitSessionLimitTestResult(t, answers))

	api.AuthLoginWithPassword(&AuthLoginWithPasswordArgs{UserAuth: "user@example.com", Password: "password"}, connect.NewApiCallback(func(result *AuthLoginWithPasswordResult, err error) {
		a := answer{err: err}
		if result != nil && result.Error != nil {
			a.code, a.message = result.Error.Code, result.Error.Message
		}
		answers <- a
	}))
	check("/auth/login-with-password", awaitSessionLimitTestResult(t, answers))

	passwordResult, err := api.AuthLoginWithPasswordSyncWithContext(t.Context(), &AuthLoginWithPasswordArgs{UserAuth: "user@example.com", Password: "password"})
	if err != nil || passwordResult == nil || passwordResult.Error == nil {
		t.Fatalf("/auth/login-with-password (sync): %+v %v", passwordResult, err)
	}
	check("/auth/login-with-password (sync)", answer{code: passwordResult.Error.Code, message: passwordResult.Error.Message})

	api.AuthVerify(&AuthVerifyArgs{UserAuth: "user@example.com", VerifyCode: "123456"}, connect.NewApiCallback(func(result *AuthVerifyResult, err error) {
		a := answer{err: err}
		if result != nil && result.Error != nil {
			a.code, a.message = result.Error.Code, result.Error.Message
		}
		answers <- a
	}))
	check("/auth/verify", awaitSessionLimitTestResult(t, answers))

	for _, resultErrors := range []bool{false, true} {
		api.NetworkCreate(&NetworkCreateArgs{UserAuth: "user@example.com", Password: "password", NetworkName: "example", Terms: true, ResultErrors: resultErrors}, connect.NewApiCallback(func(result *NetworkCreateResult, err error) {
			a := answer{err: err}
			if result != nil && result.Error != nil {
				a.code, a.message = result.Error.Code, result.Error.Message
			}
			answers <- a
		}))
		check("/auth/network-create", awaitSessionLimitTestResult(t, answers))
	}

	api.AuthCodeLogin(&AuthCodeLoginArgs{AuthCode: "code"}, connect.NewApiCallback(func(result *AuthCodeLoginResult, err error) {
		a := answer{err: err}
		if result != nil && result.Error != nil {
			a.code, a.message = result.Error.Code, result.Error.Message
		}
		answers <- a
	}))
	check("/auth/code-login", awaitSessionLimitTestResult(t, answers))

	codeResult, err := api.AuthCodeLoginSyncWithContext(t.Context(), &AuthCodeLoginArgs{AuthCode: "code"})
	if err != nil || codeResult == nil || codeResult.Error == nil {
		t.Fatalf("/auth/code-login (sync): %+v %v", codeResult, err)
	}
	check("/auth/code-login (sync)", answer{code: codeResult.Error.Code, message: codeResult.Error.Message})

	api.AuthNetworkClient(&AuthNetworkClientArgs{DeviceDescription: "proxy", DeviceSpec: "spec"}, connect.NewApiCallback(func(result *AuthNetworkClientResult, err error) {
		a := answer{err: err}
		if result != nil && result.Error != nil {
			a.code, a.message = result.Error.Code, result.Error.Message
			// not a client limit: no upgrade and no device to remove frees it
			if result.Error.ClientLimitPrompt != ClientLimitPromptNone || result.Error.UpgradeRequired || result.Error.ClientLimitExceeded {
				a.code = "client limit"
			}
		}
		answers <- a
	}))
	check("/network/auth-client", awaitSessionLimitTestResult(t, answers))

	clientResult, err := api.AuthNetworkClientSyncWithContext(t.Context(), &AuthNetworkClientArgs{DeviceDescription: "proxy", DeviceSpec: "spec"})
	if err != nil || clientResult == nil || clientResult.Error == nil {
		t.Fatalf("/network/auth-client (sync): %+v %v", clientResult, err)
	}
	check("/network/auth-client (sync)", answer{code: clientResult.Error.Code, message: clientResult.Error.Message})

	expected := []string{
		"/auth/login",
		"/auth/login-with-password",
		"/auth/login-with-password",
		"/auth/verify",
		"/auth/network-create",
		"/auth/network-create",
		"/auth/code-login",
		"/auth/code-login",
		"/network/auth-client",
		"/network/auth-client",
	}
	if strings.Join(paths, " ") != strings.Join(expected, " ") {
		t.Fatalf("routes: %v", paths)
	}
}

// Only the cap's own code is the refusal. The retained-session cap is not
// freed by signing sessions out, and other statuses and bodies keep their
// errors.
func TestSessionLimitRefusalIsOnlyTheCapCode(t *testing.T) {
	for _, c := range []struct {
		err     error
		refusal bool
	}{
		{err: sessionLimitTestRefusal(), refusal: true},
		{err: &connect.HttpStatusError{StatusCode: http.StatusConflict, Body: []byte(`{"error":{"code":"session_limit_reached","message":"m"}}`)}, refusal: true},
		{err: &connect.HttpStatusError{StatusCode: http.StatusConflict, Body: []byte(`{"error":"Session request could not be completed.","code":"session_retention_limit_reached"}`)}},
		{err: &connect.HttpStatusError{StatusCode: http.StatusConflict, Body: []byte(`{"error":"Session request could not be completed.","code":"client_session_changed"}`)}},
		{err: &connect.HttpStatusError{StatusCode: http.StatusBadRequest, Body: []byte(`{"code":"session_limit_reached"}`)}},
		{err: &connect.HttpStatusError{StatusCode: http.StatusConflict, Body: []byte(`session_limit_reached`)}},
		{err: &connect.HttpStatusError{StatusCode: http.StatusConflict}},
		{err: context.DeadlineExceeded},
		{err: nil},
		// malformed typed-nil errors pass through untouched
		{err: (*connect.HttpStatusError)(nil)},
		{err: (*net.OpError)(nil)},
	} {
		if isSessionLimitRefusal(c.err) != c.refusal {
			t.Errorf("%v: refusal %v", c.err, !c.refusal)
		}
	}

	api := newApi(t.Context(), nil, "https://api.example")
	defer api.Close()
	retention := &connect.HttpStatusError{StatusCode: http.StatusConflict, Body: []byte(`{"error":"Session request could not be completed.","code":"session_retention_limit_reached"}`)}
	api.setHttpPostRaw(func(context.Context, string, []byte, string) ([]byte, error) {
		return nil, retention
	})
	result, err := api.AuthLoginWithPasswordSyncWithContext(t.Context(), &AuthLoginWithPasswordArgs{UserAuth: "user@example.com", Password: "password"})
	if err == nil || result != nil {
		t.Fatalf("the retention refusal read as a result: %+v %v", result, err)
	}
}
