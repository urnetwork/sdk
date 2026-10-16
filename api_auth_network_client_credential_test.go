package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Holds the actual public method's request before any transport work, without
// changing scheduling for another Api or depending on goroutine timing.
type heldAuthNetworkClient struct {
	api          *Api
	requests     []func()
	credentials  []string
	transportErr error
}

func newHeldAuthNetworkClient(t *testing.T) *heldAuthNetworkClient {
	t.Helper()
	api := newApi(t.Context(), nil, "https://registration.example")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := api.CloseAndWait(ctx); err != nil {
			t.Errorf("close registration API: %v", err)
		}
	})
	held := &heldAuthNetworkClient{api: api}
	api.authNetworkClientLaunch = func(request func()) {
		held.requests = append(held.requests, request)
	}
	api.setHttpPostRaw(func(_ context.Context, requestUrl string, body []byte, byJwt string) ([]byte, error) {
		if requestUrl != "https://registration.example/network/auth-client" {
			t.Errorf("registration route = %q", requestUrl)
		}
		var args AuthNetworkClientArgs
		if err := json.Unmarshal(body, &args); err != nil {
			t.Errorf("decode registration arguments: %v", err)
		}
		held.credentials = append(held.credentials, byJwt)
		if held.transportErr != nil {
			return nil, held.transportErr
		}
		return []byte(`{"by_client_jwt":"synthetic-client"}`), nil
	})
	return held
}

func (self *heldAuthNetworkClient) release(t *testing.T, index int) {
	t.Helper()
	if len(self.requests) <= index || self.requests[index] == nil {
		t.Fatalf("no held registration request at %d", index)
	}
	request := self.requests[index]
	self.requests[index] = nil
	request()
}

// Exercises replacement and logout at the public method's scheduling boundary.
func checkAuthNetworkClientInvocationCredential(t *testing.T, currentByJwt string, transportErr error) {
	t.Helper()
	held := newHeldAuthNetworkClient(t)
	held.transportErr = transportErr
	held.api.SetByJwt("synthetic-network-A")
	callbackCount := 0
	var callbackResult *AuthNetworkClientResult
	var callbackErr error
	held.api.AuthNetworkClient(&AuthNetworkClientArgs{}, connect.NewApiCallback(func(result *AuthNetworkClientResult, err error) {
		callbackCount += 1
		callbackResult, callbackErr = result, err
	}))
	if len(held.requests) != 1 || len(held.credentials) != 0 || callbackCount != 0 {
		t.Fatalf("request did not remain queued: requests=%d transport=%d callbacks=%d", len(held.requests), len(held.credentials), callbackCount)
	}
	// SetByJwt("") is the local credential-clear step used by host logout.
	held.api.SetByJwt(currentByJwt)
	held.release(t, 0)
	if len(held.credentials) != 1 || held.credentials[0] != "synthetic-network-A" {
		t.Errorf("registration credentials = %q, want the invocation credential", held.credentials)
	}
	if callbackCount != 1 {
		t.Errorf("callback count = %d, want 1", callbackCount)
	}
	if transportErr != nil {
		if callbackResult != nil || callbackErr != transportErr {
			t.Errorf("failure callback = (%v, %v), want (nil, original transport error)", callbackResult, callbackErr)
		}
	} else if callbackErr != nil || callbackResult == nil || callbackResult.ByClientJwt != "synthetic-client" {
		t.Errorf("success callback = (%v, %v), want the decoded registration result", callbackResult, callbackErr)
	}
	if current := held.api.GetByJwt(); current != currentByJwt {
		t.Errorf("request changed current credential to %q, want %q", current, currentByJwt)
	}
}

func TestAuthNetworkClientKeepsInvocationCredentialAfterLogin(t *testing.T) {
	checkAuthNetworkClientInvocationCredential(t, "synthetic-network-B", nil)
}

func TestAuthNetworkClientKeepsInvocationCredentialAfterLogout(t *testing.T) {
	checkAuthNetworkClientInvocationCredential(t, "", nil)
}

func TestAuthNetworkClientTransportFailureKeepsInvocationCredential(t *testing.T) {
	checkAuthNetworkClientInvocationCredential(t, "synthetic-network-B", errors.New("synthetic registration transport failure"))
}

func TestAuthNetworkClientKeepsEachInvocationCredential(t *testing.T) {
	held := newHeldAuthNetworkClient(t)
	callbackCounts := [2]int{}
	for i, byJwt := range []string{"synthetic-network-A", "synthetic-network-B"} {
		held.api.SetByJwt(byJwt)
		held.api.AuthNetworkClient(&AuthNetworkClientArgs{}, connect.NewApiCallback(func(_ *AuthNetworkClientResult, err error) {
			callbackCounts[i] += 1
			if err != nil {
				t.Errorf("registration callback %d: %v", i, err)
			}
		}))
	}
	held.api.SetByJwt("")
	held.release(t, 1)
	held.release(t, 0)
	if len(held.credentials) != 2 || held.credentials[0] != "synthetic-network-B" || held.credentials[1] != "synthetic-network-A" {
		t.Errorf("reverse execution credentials = %q, want [B A]", held.credentials)
	}
	if callbackCounts != [2]int{1, 1} {
		t.Errorf("callback counts = %v, want [1 1]", callbackCounts)
	}
}

func TestAuthNetworkClientHeldRequestDoesNotDelayOtherApi(t *testing.T) {
	first := newHeldAuthNetworkClient(t)
	first.api.SetByJwt("synthetic-network-A")
	first.api.AuthNetworkClient(&AuthNetworkClientArgs{}, connect.NewNoopApiCallback[*AuthNetworkClientResult]())

	second := newHeldAuthNetworkClient(t)
	// Use the normal goroutine launcher for this independent API instance.
	second.api.authNetworkClientLaunch = nil
	second.api.SetByJwt("synthetic-network-B")
	if err := awaitApiCall(func(callback connect.ApiCallback[*AuthNetworkClientResult]) {
		second.api.AuthNetworkClient(&AuthNetworkClientArgs{}, callback)
	}); err != nil {
		t.Fatalf("independent API registration: %v", err)
	}
	if len(second.credentials) != 1 || second.credentials[0] != "synthetic-network-B" {
		t.Errorf("independent API credentials = %q, want [B]", second.credentials)
	}
	if len(first.requests) != 1 || len(first.credentials) != 0 {
		t.Fatalf("first API no longer held: requests=%d transport=%d", len(first.requests), len(first.credentials))
	}
	first.release(t, 0)
	if len(first.credentials) != 1 || first.credentials[0] != "synthetic-network-A" {
		t.Errorf("held API credentials = %q, want [A]", first.credentials)
	}
}

func TestAuthNetworkClientDoesNotRedeliverAfterCallbackPanic(t *testing.T) {
	held := newHeldAuthNetworkClient(t)
	callbackCount := 0
	held.api.AuthNetworkClient(&AuthNetworkClientArgs{}, connect.NewApiCallback(func(result *AuthNetworkClientResult, err error) {
		callbackCount += 1
		if err != nil || result == nil || result.ByClientJwt != "synthetic-client" {
			t.Errorf("callback = (%v, %v), want a successful registration", result, err)
		}
		panic("synthetic foreign callback panic")
	}))
	held.release(t, 0)
	if callbackCount != 1 || len(held.credentials) != 1 {
		t.Errorf("callbacks=%d transport=%d, want one of each", callbackCount, len(held.credentials))
	}
}

func TestAuthNetworkClientDeliversTransportPanicOnce(t *testing.T) {
	held := newHeldAuthNetworkClient(t)
	const panicSecret = "synthetic private registration credential"
	held.api.setHttpPostRaw(func(context.Context, string, []byte, string) ([]byte, error) {
		panic(panicSecret)
	})
	callbackCount := 0
	held.api.AuthNetworkClient(&AuthNetworkClientArgs{}, connect.NewApiCallback(func(result *AuthNetworkClientResult, err error) {
		callbackCount += 1
		if result != nil || !errors.Is(err, errApiRequestFailed) {
			t.Errorf("panic callback = (%v, %v), want static request failure", result, err)
		}
		if err != nil && strings.Contains(err.Error(), panicSecret) {
			t.Errorf("callback exposed private panic value: %v", err)
		}
	}))
	held.release(t, 0)
	if callbackCount != 1 {
		t.Errorf("callback count = %d, want 1", callbackCount)
	}
}
