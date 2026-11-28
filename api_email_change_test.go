// The account email change calls through the actual transport against a local
// fake server: the network credential they need, what they send, and how the
// server's answers and refusals read.
package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/urnetwork/connect"
)

// One request the fake account server saw.
type accountTestRequest struct {
	// "METHOD /path"
	route string
	byJwt string
	body  string
}

// A fake server for the account calls: each route answers what answers
// returns for it, and every request is recorded.
type accountTestServer struct {
	t       *testing.T
	answers map[string]func(body string) (int, string)

	mutex    sync.Mutex
	requests []accountTestRequest
}

func newAccountTestServer(t *testing.T, answers map[string]func(body string) (int, string)) *accountTestServer {
	return &accountTestServer{t: t, answers: answers}
}

func (self *accountTestServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		self.t.Errorf("read %s %s: %v", r.Method, r.URL, err)
	}
	route := r.Method + " " + r.URL.Path
	func() {
		self.mutex.Lock()
		defer self.mutex.Unlock()
		self.requests = append(self.requests, accountTestRequest{
			route: route,
			byJwt: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "),
			body:  string(body),
		})
	}()
	answer, ok := self.answers[route]
	if !ok {
		self.t.Errorf("unexpected request %s", route)
		http.Error(w, "no such route", http.StatusNotFound)
		return
	}
	status, response := answer(string(body))
	if status != http.StatusOK {
		http.Error(w, response, status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, response)
}

func (self *accountTestServer) takeRequests() []accountTestRequest {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	requests := self.requests
	self.requests = nil
	return requests
}

func accountAnswer(body string) func(string) (int, string) {
	return func(string) (int, string) {
		return http.StatusOK, body
	}
}

// An api as the apps hold it once their device starts: the network credential
// beside the device's client token, which is the current credential.
func newAccountTestApi(t *testing.T, server *accountTestServer) (context.Context, *Api, string, string) {
	t.Helper()
	ctx, api := newTestApi(t, server)
	networkJwt := credentialTestNetworkJwt(t, credentialTestNetworkId)
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "device")
	api.SetByJwt(networkJwt)
	installTestDeviceByJwt(t, api, nil, clientJwt, nil)
	connect.AssertEqual(t, api.GetByJwt(), clientJwt)
	return ctx, api, networkJwt, clientJwt
}

// Runs one asynchronous account call and returns its callback's result.
func awaitAccountCall[R any](t *testing.T, start func(callback connect.ApiCallback[R])) (R, error) {
	t.Helper()
	return awaitSnConsent[R](t, start)
}

// The email change routes carry the network credential, never the device's
// client token: the server refuses a client token there. An api without the
// network credential sends nothing.
func TestAuthEmailChangeCallsSendTheNetworkCredential(t *testing.T) {
	server := newAccountTestServer(t, map[string]func(string) (int, string){
		"POST /auth/email-change":         accountAnswer(`{"current_email":"j…@example.com","new_email":"new@example.com","expires_at_millis":1791244800000}`),
		"GET /auth/email-change":          accountAnswer(`{"pending":false}`),
		"POST /auth/email-change/confirm": accountAnswer(`{"current_confirmed":true,"new_confirmed":false,"complete":false}`),
		"POST /auth/email-change/cancel":  accountAnswer(`{}`),
	})
	ctx, api, networkJwt, _ := newAccountTestApi(t, server)

	if _, err := api.AuthEmailChangeSyncWithContext(ctx, &AuthEmailChangeArgs{NewEmail: "new@example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := awaitAccountCall(t, func(cb connect.ApiCallback[*AuthEmailChangeStatusResult]) { api.AuthEmailChangeStatus(cb) }); err != nil {
		t.Fatal(err)
	}
	if _, err := api.AuthEmailChangeConfirmSyncWithContext(ctx, &AuthEmailChangeConfirmArgs{CurrentCode: "123456"}); err != nil {
		t.Fatal(err)
	}
	if _, err := awaitAccountCall(t, func(cb connect.ApiCallback[*AuthEmailChangeCancelResult]) { api.AuthEmailChangeCancel(cb) }); err != nil {
		t.Fatal(err)
	}
	requests := server.takeRequests()
	connect.AssertEqual(t, len(requests), 4)
	for _, request := range requests {
		if request.byJwt != networkJwt {
			t.Fatalf("%s carried %q, want the network credential", request.route, request.byJwt)
		}
	}

	// a device that holds only its client token
	_, clientOnly := newTestApi(t, server)
	clientOnly.SetByJwt(credentialTestClientJwt(t, credentialTestNetworkId, "device"))
	if _, err := clientOnly.AuthEmailChangeSyncWithContext(ctx, &AuthEmailChangeArgs{NewEmail: "new@example.com"}); !errors.Is(err, ErrNetworkCredentialRequired) {
		t.Fatalf("err = %v, want ErrNetworkCredentialRequired", err)
	}
	connect.AssertEqual(t, len(server.takeRequests()), 0)
}

// The start sends the new address trimmed, and the confirm sends the codes as
// the server reads them: the spaces and dashes of a pasted or grouped code
// dropped. A code left empty is not sent.
func TestAuthEmailChangeSendsTrimmedAddressAndNormalizedCodes(t *testing.T) {
	server := newAccountTestServer(t, map[string]func(string) (int, string){
		"POST /auth/email-change":         accountAnswer(`{}`),
		"POST /auth/email-change/confirm": accountAnswer(`{"current_confirmed":true,"new_confirmed":true,"complete":true,"email":"new@example.com","other_sessions_signed_out":true}`),
	})
	ctx, api, _, _ := newAccountTestApi(t, server)

	if _, err := api.AuthEmailChangeSyncWithContext(ctx, &AuthEmailChangeArgs{NewEmail: "  new@example.com \n"}); err != nil {
		t.Fatal(err)
	}
	result, err := api.AuthEmailChangeConfirmSyncWithContext(ctx, &AuthEmailChangeConfirmArgs{CurrentCode: " 123 456 ", NewCode: "654-321"})
	if err != nil {
		t.Fatal(err)
	}
	requests := server.takeRequests()
	connect.AssertEqual(t, len(requests), 2)

	start := map[string]string{}
	if err := json.Unmarshal([]byte(requests[0].body), &start); err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, start, map[string]string{"new_email": "new@example.com"})

	confirm := map[string]string{}
	if err := json.Unmarshal([]byte(requests[1].body), &confirm); err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, confirm, map[string]string{"current_code": "123456", "new_code": "654321"})

	connect.AssertEqual(t, result.Complete, true)
	connect.AssertEqual(t, result.Email, "new@example.com")
	connect.AssertEqual(t, result.OtherSessionsSignedOut, true)
	connect.AssertEqual(t, result.NeedsCurrentCode(), false)
	connect.AssertEqual(t, result.NeedsNewCode(), false)

	only, err := api.AuthEmailChangeConfirmSyncWithContext(ctx, &AuthEmailChangeConfirmArgs{NewCode: "111111"})
	if err != nil || only == nil {
		t.Fatalf("result = %+v, err = %v", only, err)
	}
	requests = server.takeRequests()
	confirm = map[string]string{}
	if err := json.Unmarshal([]byte(requests[0].body), &confirm); err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, confirm, map[string]string{"new_code": "111111"})
}

// A refusal reads as the server sends it: its code, message and retry hint,
// and whether it ended the waiting request.
func TestAuthEmailChangeRefusalsRead(t *testing.T) {
	server := newAccountTestServer(t, map[string]func(string) (int, string){
		"POST /auth/email-change":         accountAnswer(`{"error":{"code":"rate_limited","message":"Too many email changes.","retry_after_seconds":3600}}`),
		"POST /auth/email-change/confirm": accountAnswer(`{"current_confirmed":false,"new_confirmed":true,"complete":false,"error":{"code":"code_invalid","message":"The code is wrong."}}`),
	})
	ctx, api, _, _ := newAccountTestApi(t, server)

	started, err := api.AuthEmailChangeSyncWithContext(ctx, &AuthEmailChangeArgs{NewEmail: "new@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, started.Error.Code, AuthEmailChangeErrorCodeRateLimited)
	connect.AssertEqual(t, started.Error.RetryAfterSeconds, int64(3600))
	connect.AssertEqual(t, started.Error.EndsRequest(), false)

	confirmed, err := api.AuthEmailChangeConfirmSyncWithContext(ctx, &AuthEmailChangeConfirmArgs{CurrentCode: "000000"})
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, confirmed.Error.Code, AuthEmailChangeErrorCodeCodeInvalid)
	connect.AssertEqual(t, confirmed.NeedsCurrentCode(), true)
	connect.AssertEqual(t, confirmed.NeedsNewCode(), false)
	connect.AssertEqual(t, confirmed.Error.EndsRequest(), false)
}

func TestAuthEmailChangeErrorEndsRequest(t *testing.T) {
	for _, c := range []struct {
		code string
		ends bool
	}{
		{AuthEmailChangeErrorCodeNoChange, true},
		{AuthEmailChangeErrorCodeTooManyTries, true},
		{AuthEmailChangeErrorCodeEmailInUse, true},
		{AuthEmailChangeErrorCodeCodeInvalid, false},
		{AuthEmailChangeErrorCodeRateLimited, false},
		{AuthEmailChangeErrorCodeSendFailed, false},
		{AuthEmailChangeErrorCodeOwnerOnly, false},
		{"a_newer_code", false},
	} {
		if ends := (&AuthEmailChangeError{Code: c.code}).EndsRequest(); ends != c.ends {
			t.Errorf("%s: EndsRequest = %t, want %t", c.code, ends, c.ends)
		}
	}
	var none *AuthEmailChangeError
	connect.AssertEqual(t, none.EndsRequest(), false)
}

func TestValidateEmailChangeAddress(t *testing.T) {
	for _, c := range []struct {
		email string
		code  string
	}{
		{"new@example.com", ""},
		{"  new.name+tag@sub.example.co  ", ""},
		{"", AuthEmailChangeErrorCodeInvalidEmail},
		{"new", AuthEmailChangeErrorCodeInvalidEmail},
		{"@example.com", AuthEmailChangeErrorCodeInvalidEmail},
		{"new@", AuthEmailChangeErrorCodeInvalidEmail},
		{"new@example", AuthEmailChangeErrorCodeInvalidEmail},
		{"new@.example.com", AuthEmailChangeErrorCodeInvalidEmail},
		{"new@example.com.", AuthEmailChangeErrorCodeInvalidEmail},
		{"new@example..com", AuthEmailChangeErrorCodeInvalidEmail},
		{"new@exa@mple.com", AuthEmailChangeErrorCodeInvalidEmail},
		{"new name@example.com", AuthEmailChangeErrorCodeInvalidEmail},
		{"new@exam\u0000ple.com", AuthEmailChangeErrorCodeInvalidEmail},
		{strings.Repeat("a", 250) + "@example.com", AuthEmailChangeErrorCodeInvalidEmail},
	} {
		if code := ValidateEmailChangeAddress(c.email); code != c.code {
			t.Errorf("%q: %q, want %q", c.email, code, c.code)
		}
	}
}

func TestNormalizeEmailChangeCode(t *testing.T) {
	connect.AssertEqual(t, NormalizeEmailChangeCode(" 12 34-56\t"), "123456")
	connect.AssertEqual(t, NormalizeEmailChangeCode(""), "")
}

// The address an email change replaces is the email sign-in, never the phone
// sign-in or the identity the account was created with.
func TestNetworkUserEmailSignIn(t *testing.T) {
	user := &NetworkUser{}
	connect.AssertEqual(t, user.EmailSignIn(), "")
	connect.AssertEqual(t, user.EmailChangeAvailable(), false)

	user = &NetworkUser{}
	if err := json.Unmarshal([]byte(`{"user_auth":"created@example.com","user_auths":[{"user_auth":"+15555550100","auth_type":"phone"}]}`), user); err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, user.EmailSignIn(), "")
	connect.AssertEqual(t, user.EmailChangeAvailable(), false)

	user = &NetworkUser{}
	if err := json.Unmarshal([]byte(`{"user_auths":[{"user_auth":"+15555550100","auth_type":"phone"},{"user_auth":"current@example.com","auth_type":"email"}]}`), user); err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, user.EmailSignIn(), "current@example.com")
	connect.AssertEqual(t, user.EmailChangeAvailable(), true)

	var none *NetworkUser
	connect.AssertEqual(t, none.EmailSignIn(), "")
}
