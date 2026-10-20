// Discovery diagnostics exercise real callbacks with typed failures and owned barriers.
package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// The actual API's logger is captured without changing the process default.
type testingAuthDiscoveryLogger struct {
	connect.Logger
	stateLock   sync.Mutex
	lines       []string
	panicOnInfo bool
}

// Captures the invocation's diagnostic or forces a deterministic sink failure.
func (self *testingAuthDiscoveryLogger) Infof(format string, args ...any) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.panicOnInfo {
		panic("synthetic diagnostic sink failure")
	}
	self.lines = append(self.lines, fmt.Sprintf(format, args...))
}

// Returns an owned copy after callback arrival, including concurrent requests.
func (self *testingAuthDiscoveryLogger) snapshot() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string(nil), self.lines...)
}

// Callback arrival is the barrier; the timeout is only a deadlock guard.
func testingAuthDiscoveryCall(t *testing.T, log *testingAuthDiscoveryLogger, args *AuthLoginArgs, post connect.HttpPostRawFunction) (*AuthLoginResult, error) {
	t.Helper()
	api := &Api{ctx: t.Context(), log: log, httpPostRaw: post}
	type answer struct {
		result *AuthLoginResult
		err    error
	}
	answers := make(chan answer, 1)
	api.AuthLogin(args, connect.NewApiCallback(func(result *AuthLoginResult, err error) {
		answers <- answer{result: result, err: err}
	}))
	select {
	case value := <-answers:
		return value.result, value.err
	case <-time.After(10 * time.Second):
		t.Fatal("discovery callback did not arrive")
		return nil, nil
	}
}

// Typed failures retain their original callback identity and one fixed category.
func TestAuthLoginDiscoveryRecordsTypedFailureWithoutChangingCallback(t *testing.T) {
	const secret = "synthetic-secret-request-detail"
	cases := []struct {
		err      error
		category string
	}{
		{err: context.Canceled, category: "canceled"},
		{err: context.DeadlineExceeded, category: "timeout"},
		{err: &connect.HttpStatusError{StatusCode: http.StatusUnauthorized, Status: secret, Body: []byte(secret)}, category: "http_auth"},
		{err: &connect.HttpStatusError{StatusCode: http.StatusTooManyRequests, Status: secret, Body: []byte(secret)}, category: "http_rate"},
		{err: &connect.HttpStatusError{StatusCode: http.StatusServiceUnavailable, Status: secret, Body: []byte(secret)}, category: "http_server"},
		{err: &net.OpError{Op: secret, Net: "tcp", Err: errors.New(secret)}, category: "network"},
		{err: errors.New(secret), category: "request_unknown"},
	}
	for _, c := range cases {
		log := &testingAuthDiscoveryLogger{Logger: connect.NewNoopLogger()}
		result, err := testingAuthDiscoveryCall(t, log, &AuthLoginArgs{UserAuth: "synthetic-user@login.example"},
			func(context.Context, string, []byte, string) ([]byte, error) { return nil, c.err })
		if result != nil || err != c.err {
			t.Fatalf("callback identity changed for %s: result=%p error type=%T", c.category, result, err)
		}
		want := []string{"[auth-discovery]callback category=" + c.category + "\n"}
		if got := log.snapshot(); !reflect.DeepEqual(got, want) {
			t.Fatalf("missing bounded discovery callback category: got %q, want %q", got, want)
		}
	}
}

// Successful decoding preserves the full result without logging its private fields.
func TestAuthLoginDiscoveryRecordsResultWithoutRetainingItsFields(t *testing.T) {
	cases := []struct {
		body     string
		category string
	}{
		{body: `{"auth_allowed":["password"],"user_auth":"synthetic-user@login.example"}`, category: "password_allowed"},
		{body: `{"auth_allowed":["synthetic-private-auth-method"]}`, category: "password_not_allowed"},
		{body: `{"auth_allowed":[]}`, category: "password_not_allowed"},
		{body: `{"user_auth":"synthetic-user@login.example"}`, category: "no_auth_methods"},
		{body: `{"error":{"code":"synthetic-private-code","message":"synthetic-private-message"},"auth_allowed":["password"]}`, category: "api_refusal"},
		{body: `null`, category: "result_invalid"},
	}
	for _, c := range cases {
		log := &testingAuthDiscoveryLogger{Logger: connect.NewNoopLogger()}
		result, err := testingAuthDiscoveryCall(t, log, &AuthLoginArgs{UserAuth: "synthetic-user@login.example"},
			func(context.Context, string, []byte, string) ([]byte, error) { return []byte(c.body), nil })
		if err != nil {
			t.Fatalf("result callback changed for %s: error type=%T", c.category, err)
		}
		var expected *AuthLoginResult
		if err := json.Unmarshal([]byte(c.body), &expected); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(result, expected) {
			t.Fatalf("result callback fields changed for %s", c.category)
		}
		want := []string{"[auth-discovery]callback category=" + c.category + "\n"}
		if got := log.snapshot(); !reflect.DeepEqual(got, want) {
			t.Fatalf("result diagnostic = %q, want %q", got, want)
		}
	}
}

// Malformed wire responses keep their concrete decoder errors and no response text.
func TestAuthLoginDiscoveryPreservesActualDecodeFailure(t *testing.T) {
	for _, body := range []string{`synthetic-private-not-json`, `{"auth_allowed":123}`} {
		log := &testingAuthDiscoveryLogger{Logger: connect.NewNoopLogger()}
		result, err := testingAuthDiscoveryCall(t, log, &AuthLoginArgs{UserAuth: "synthetic-user@login.example"},
			func(context.Context, string, []byte, string) ([]byte, error) { return []byte(body), nil })
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		if result != nil || !(errors.As(err, &syntaxErr) || errors.As(err, &typeErr)) {
			t.Fatalf("original decode error not preserved: result=%p error type=%T", result, err)
		}
		want := []string{"[auth-discovery]callback category=decode\n"}
		if got := log.snapshot(); !reflect.DeepEqual(got, want) {
			t.Fatalf("decode diagnostic = %q, want %q", got, want)
		}
	}
}

// A pre-callback panic retains the existing static terminal error and one event.
func TestAuthLoginDiscoveryRequestPanicKeepsOneSafeTerminalCallback(t *testing.T) {
	log := &testingAuthDiscoveryLogger{Logger: connect.NewNoopLogger()}
	result, err := testingAuthDiscoveryCall(t, log, &AuthLoginArgs{UserAuth: "synthetic-user@login.example"},
		func(context.Context, string, []byte, string) ([]byte, error) {
			panic("synthetic-private-request-panic")
		})
	if result != nil || err != errApiRequestFailed {
		t.Fatalf("request panic terminal changed: result=%p error type=%T", result, err)
	}
	if got, want := log.snapshot(), []string{"[auth-discovery]callback category=request_panic\n"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("request panic diagnostic = %q, want %q", got, want)
	}
}

// Token, wallet, seedphrase and absent-user calls remain outside discovery observation.
func TestAuthLoginDiscoveryDoesNotObserveOtherAuthenticationMechanisms(t *testing.T) {
	for _, args := range []*AuthLoginArgs{nil, {}, {Seedphrase: "synthetic-private-seed"}, {AuthJwt: "synthetic-private-token"},
		{WalletAuth: &WalletAuthArgs{PublicKey: "synthetic-private-key"}}} {
		log := &testingAuthDiscoveryLogger{Logger: connect.NewNoopLogger()}
		expected := errors.New("synthetic-private-error")
		_, err := testingAuthDiscoveryCall(t, log, args,
			func(context.Context, string, []byte, string) ([]byte, error) { return nil, expected })
		if err != expected || len(log.snapshot()) != 0 {
			t.Fatal("non-discovery authentication behavior or diagnostics changed")
		}
	}
}

// A failing diagnostic sink cannot replace the caller's error or suppress delivery.
func TestAuthLoginDiscoveryDiagnosticFailureCannotReplaceCallback(t *testing.T) {
	log := &testingAuthDiscoveryLogger{Logger: connect.NewNoopLogger(), panicOnInfo: true}
	expected := errors.New("synthetic-private-error")
	result, err := testingAuthDiscoveryCall(t, log, &AuthLoginArgs{UserAuth: "synthetic-user@login.example"},
		func(context.Context, string, []byte, string) ([]byte, error) { return nil, expected })
	if result != nil || err != expected {
		t.Fatalf("diagnostic panic replaced callback: result=%p error type=%T", result, err)
	}
}

// Only typed causes select categories; matching error text remains unknown.
func TestAuthLoginDiscoveryTypedCategoriesIgnoreTextLookalikes(t *testing.T) {
	for _, message := range []string{"context canceled", "context deadline exceeded", "i/o timeout", "503 Service Unavailable",
		"invalid character in JSON", "password not allowed", "api request failed", "api request returned without a callback"} {
		if got := authDiscoveryResultCategory(nil, errors.New(message)); got != "request_unknown" {
			t.Fatalf("untyped text became %q", got)
		}
	}
	for _, c := range []struct {
		err      error
		category string
	}{
		{err: context.Canceled, category: "canceled"},
		{err: context.DeadlineExceeded, category: "timeout"},
		{err: &connect.HttpStatusError{StatusCode: 400}, category: "http_client"},
		{err: &connect.HttpStatusError{StatusCode: 302}, category: "http_other"},
		{err: errApiRequestReturnedWithoutCallback, category: "request_no_callback"},
	} {
		if got := authDiscoveryResultCategory(nil, fmt.Errorf("synthetic wrapper: %w", c.err)); got != c.category {
			t.Fatalf("typed wrapper category = %q, want %q", got, c.category)
		}
	}
}

// Calling Error itself is forbidden; the diagnostic needs only typed causes.
type testingAuthDiscoveryOpaqueError struct {
	calls atomic.Int32
}

// Makes accidental formatting fail deterministically instead of exposing test data.
func (self *testingAuthDiscoveryOpaqueError) Error() string {
	self.calls.Add(1)
	panic("synthetic secret error formatter must not run")
}

// An opaque error reaches its caller without invoking its forbidden formatter.
func TestAuthLoginDiscoveryNeverFormatsOpaqueErrors(t *testing.T) {
	log := &testingAuthDiscoveryLogger{Logger: connect.NewNoopLogger()}
	expected := &testingAuthDiscoveryOpaqueError{}
	_, err := testingAuthDiscoveryCall(t, log, &AuthLoginArgs{UserAuth: "synthetic-user@login.example"},
		func(context.Context, string, []byte, string) ([]byte, error) { return nil, expected })
	if err != expected || expected.calls.Load() != 0 {
		t.Fatal("diagnostic touched private error text or changed callback identity")
	}
	if got, want := log.snapshot(), []string{"[auth-discovery]callback category=request_unknown\n"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("opaque error diagnostic = %q, want %q", got, want)
	}
}

// Explicit request barriers reverse completion order without crossing callback owners.
func TestAuthLoginDiscoveryConcurrentRequestsKeepTheirOwnOutcomes(t *testing.T) {
	log := &testingAuthDiscoveryLogger{Logger: connect.NewNoopLogger()}
	arrived := make(chan string, 2)
	releaseA, releaseB := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-releaseA:
		default:
			close(releaseA)
		}
		select {
		case <-releaseB:
		default:
			close(releaseB)
		}
	}()
	errA, errB := context.Canceled, errors.New("synthetic-private-error")
	api := &Api{ctx: t.Context(), log: log, httpPostRaw: func(_ context.Context, _ string, body []byte, _ string) ([]byte, error) {
		if strings.Contains(string(body), "request-a@login.example") {
			arrived <- "a"
			<-releaseA
			return nil, errA
		}
		arrived <- "b"
		<-releaseB
		return nil, errB
	}}
	type answer struct {
		name string
		err  error
	}
	answers := make(chan answer, 2)
	for _, name := range []string{"a", "b"} {
		api.AuthLogin(&AuthLoginArgs{UserAuth: "request-" + name + "@login.example"}, connect.NewApiCallback(func(_ *AuthLoginResult, err error) {
			answers <- answer{name: name, err: err}
		}))
	}
	for range 2 {
		select {
		case <-arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("request did not reach barrier")
		}
	}
	close(releaseB)
	var b, a answer
	select {
	case b = <-answers:
	case <-time.After(10 * time.Second):
		t.Fatal("second request callback missing")
	}
	if b.name != "b" || b.err != errB {
		t.Fatal("second request outcome crossed owners")
	}
	close(releaseA)
	select {
	case a = <-answers:
	case <-time.After(10 * time.Second):
		t.Fatal("first request callback missing")
	}
	if a.name != "a" || a.err != errA {
		t.Fatal("first request outcome crossed owners")
	}
	want := []string{"[auth-discovery]callback category=request_unknown\n", "[auth-discovery]callback category=canceled\n"}
	if got := log.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("request-local categories = %q, want %q", got, want)
	}
}

// Malformed typed-nil errors can discard diagnostics, never alter or retry the request.
func TestAuthLoginDiscoveryTypedNilErrorCannotReplaceCallback(t *testing.T) {
	for _, expected := range []error{(*connect.HttpStatusError)(nil), (*net.OpError)(nil)} {
		log := &testingAuthDiscoveryLogger{Logger: connect.NewNoopLogger()}
		var requests atomic.Int32
		result, err := testingAuthDiscoveryCall(t, log, &AuthLoginArgs{UserAuth: "synthetic-user@login.example"},
			func(context.Context, string, []byte, string) ([]byte, error) {
				requests.Add(1)
				return nil, expected
			})
		if result != nil || err != expected || requests.Load() != 1 {
			t.Fatalf("malformed error changed callback or retried request: result=%p error type=%T requests=%d", result, err, requests.Load())
		}
		if len(log.snapshot()) != 0 {
			t.Fatal("malformed typed-nil error should drop the failed diagnostic")
		}
	}
}

// A nil callback retains its pre-existing recovered terminal exit without continuation.
func TestRunApiRequestNilCallbackPreservesRecoveredTerminalExit(t *testing.T) {
	requests := 0
	continuedAfterCallback := false
	runApiRequest[*AuthLoginResult](nil, func(callback connect.ApiCallback[*AuthLoginResult]) {
		requests++
		callback.Result(&AuthLoginResult{}, nil)
		continuedAfterCallback = true
	})
	if requests != 1 || continuedAfterCallback {
		t.Fatal("nil callback changed its recovered terminal exit")
	}
}
