// Reachability observation preserves transport ownership without inspecting errors.
package sdk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// A synchronous raw seam provides a deterministic observation boundary.
type testingCantReachObservationSeam struct {
	name    string
	request func(*Api) ([]byte, error)
}

// Every installed raw transport reports through the same observer.
func testingCantReachObservationSeams(t *testing.T) []testingCantReachObservationSeam {
	t.Helper()
	const requestUrl = "https://reachability.example/observation"
	return []testingCantReachObservationSeam{
		{name: "post", request: func(api *Api) ([]byte, error) {
			return api.getHttpPostRaw()(t.Context(), requestUrl, []byte("{}"), api.GetByJwt())
		}},
		{name: "get", request: func(api *Api) ([]byte, error) {
			return api.getHttpGetRaw()(t.Context(), requestUrl, api.GetByJwt())
		}},
		{name: "stream", request: func(api *Api) ([]byte, error) {
			return api.getHttpPostStreamRaw()(t.Context(), requestUrl, strings.NewReader("{}"), api.GetByJwt())
		}},
	}
}

// Foreign error methods must never run for passive reachability observation.
type testingCantReachInspectionError struct {
	calls atomic.Int32
}

// Formatting would inspect private text and must fail the regression.
func (self *testingCantReachInspectionError) Error() string {
	self.calls.Add(1)
	panic("synthetic private error formatter")
}

// Typed matching is also foreign code, unnecessary for a concrete response.
func (self *testingCantReachInspectionError) Is(error) bool {
	self.calls.Add(1)
	panic("synthetic foreign error matcher")
}

// Foreign assertions cannot supply response evidence to the observer.
func (self *testingCantReachInspectionError) As(any) bool {
	self.calls.Add(1)
	panic("synthetic foreign error assertion")
}

// Traversal must not become a new side effect of passive observation.
func (self *testingCantReachInspectionError) Unwrap() error {
	self.calls.Add(1)
	panic("synthetic foreign error unwrapper")
}

// Success and real typed responses end an outage; text or partial evidence does not.
func TestApiRequestSeamsClassifyOnlyConcreteHttpAnswers(t *testing.T) {
	processCantReach.apiAnswer()
	defer processCantReach.apiAnswer()
	statusErr := &connect.HttpStatusError{StatusCode: http.StatusServiceUnavailable, Status: "503 Service Unavailable", Body: []byte("synthetic private response")}
	inspectionErr := &testingCantReachInspectionError{}
	for _, c := range []struct {
		name   string
		err    error
		answer bool
	}{
		{name: "success", answer: true},
		{name: "http response", err: statusErr, answer: true},
		{name: "status text", err: errors.New("503 Service Unavailable: synthetic private response")},
		{name: "wrapped response", err: fmt.Errorf("synthetic wrapper: %w", statusErr)},
		{name: "response and cancellation", err: errors.Join(statusErr, context.Canceled)},
		{name: "cancellation and response", err: errors.Join(context.Canceled, statusErr)},
		{name: "typed nil response", err: (*connect.HttpStatusError)(nil)},
		{name: "typed nil transport", err: (*net.OpError)(nil)},
		{name: "foreign inspection", err: inspectionErr},
	} {
		for _, seam := range testingCantReachObservationSeams(t) {
			processCantReach.apiAnswer()
			t0 := time.Unix(1_000_000, 0)
			processCantReach.apiFailure(cantReachTestTimeout, t0)
			response := []byte("synthetic owned response")
			requests := 0
			post := func(context.Context, string, []byte, string) ([]byte, error) {
				requests++
				return response, c.err
			}
			api := &Api{
				ctx: t.Context(), httpPostRaw: post,
				httpGetRaw: func(context.Context, string, string) ([]byte, error) { return post(nil, "", nil, "") },
				httpPostStreamRaw: func(context.Context, string, io.Reader, string) ([]byte, error) {
					return post(nil, "", nil, "")
				},
			}
			var result []byte
			var err error
			returned := false
			func() {
				defer func() { _ = recover() }()
				result, err = seam.request(api)
				returned = true
			}()
			if !returned || len(result) != len(response) || &result[0] != &response[0] || err != c.err || requests != 1 {
				t.Errorf("%s/%s: observation changed transport result, error identity, return, or request count", seam.name, c.name)
			}
			raised := processCantReach.apiFailure(cantReachTestTimeout, t0.Add(25*time.Second))
			if raised == c.answer {
				t.Errorf("%s/%s: outage raised=%v after response, want %v", seam.name, c.name, raised, !c.answer)
			}
		}
	}
	if inspectionErr.calls.Load() != 0 {
		t.Error("reachability observation invoked a foreign error method")
	}
}

// A tracked credential must not turn absent error evidence into a request panic.
func TestAuthLoginDiscoveryTypedNilErrorsKeepTrackedCredential(t *testing.T) {
	const credential = "synthetic-network-credential"
	for _, expected := range []error{(*connect.HttpStatusError)(nil), (*net.OpError)(nil)} {
		log := &testingAuthDiscoveryLogger{Logger: connect.NewNoopLogger()}
		var requests atomic.Int32
		api := &Api{
			ctx: t.Context(), log: log, byJwt: credential, networkByJwt: credential,
			httpPostRaw: func(context.Context, string, []byte, string) ([]byte, error) {
				requests.Add(1)
				return nil, expected
			},
		}
		type answer struct {
			result *AuthLoginResult
			err    error
		}
		answers := make(chan answer, 1)
		api.AuthLogin(&AuthLoginArgs{UserAuth: "synthetic-user@login.example"}, connect.NewApiCallback(func(result *AuthLoginResult, err error) {
			answers <- answer{result: result, err: err}
		}))
		select {
		case value := <-answers:
			if value.result != nil || value.err != expected || requests.Load() != 1 {
				t.Errorf("tracked typed-nil error changed callback identity or request count: error type=%T", value.err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("tracked typed-nil discovery callback missing")
		}
		if api.GetByJwt() != credential || api.networkCredential() != credential {
			t.Error("typed-nil error revoked a tracked credential")
		}
		if len(log.snapshot()) != 0 {
			t.Error("typed-nil error should discard its failed diagnostic")
		}
	}
}

// A malformed error cannot select another registration outcome or repeat its request.
func TestNetworkClientRegistrationTypedNilPreservesOriginalOutcome(t *testing.T) {
	const credential = "synthetic-network-credential"
	const apiUrl = "https://registration.example"
	args := networkClientRegistrationTestArgs()
	wantBody, err := EncodeNetworkClientRegistration(args)
	if err != nil {
		t.Fatal("synthetic registration request is invalid")
	}
	for _, expected := range []error{(*connect.HttpStatusError)(nil), (*net.OpError)(nil)} {
		requests := 0
		api := &Api{
			ctx: t.Context(), apiUrl: apiUrl, byJwt: credential, networkByJwt: credential,
			httpPostRaw: func(_ context.Context, requestUrl string, body []byte, byJwt string) ([]byte, error) {
				requests++
				if requestUrl != apiUrl+"/network/register-client-v1" || !bytes.Equal(body, wantBody) || byJwt != credential {
					t.Error("malformed error changed the original registration request")
				}
				return nil, expected
			},
		}
		var result *RegisterNetworkClientResult
		var err error
		returned := false
		func() {
			defer func() { _ = recover() }()
			result, err = api.RegisterNetworkClientSyncWithContext(t.Context(), args)
			returned = true
		}()
		if !returned || result != nil || err != expected || requests != 1 {
			t.Errorf("typed-nil %T changed the registration result, error identity, return, or request count", expected)
		}
		if api.GetByJwt() != credential || api.networkCredential() != credential {
			t.Error("typed-nil registration error revoked a tracked credential")
		}
	}
}

// Every control predicate treats typed-nil causes as unknown response evidence.
func TestClientControlPredicatesTreatTypedNilAsUnknown(t *testing.T) {
	var statusErr *connect.HttpStatusError
	var networkErr *net.OpError
	for _, err := range []error{
		statusErr,
		networkErr,
		&net.OpError{Err: statusErr},
		&net.OpError{Err: networkErr},
		errors.Join(&connect.HttpStatusError{StatusCode: http.StatusUnauthorized, Body: []byte(`{"code":"session_revoked"}`)}, statusErr),
		errors.Join(&connect.HttpStatusError{StatusCode: http.StatusNotFound}, networkErr),
		errors.Join(io.EOF, networkErr),
	} {
		for _, predicate := range []struct {
			name    string
			inspect func(error) bool
		}{
			{name: "confirmed rejection", inspect: ConfirmedClientRefreshRejection},
			{name: "trusted cause", inspect: func(err error) bool { return confirmedRejectionCause(err) != "" }},
			{name: "transient request", inspect: transientClientControlRequestError},
			{name: "unsupported response", inspect: func(err error) bool {
				status, valid := clientControlOnlyUnsupportedStatus(err)
				return status != 0 || valid
			}},
		} {
			classified, returned := false, false
			func() {
				defer func() { _ = recover() }()
				classified = predicate.inspect(err)
				returned = true
			}()
			if !returned || classified {
				t.Errorf("%s: typed-nil %T became response evidence or a panic", predicate.name, err)
			}
		}
	}
}
