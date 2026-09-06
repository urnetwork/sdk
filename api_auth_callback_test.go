package sdk

import (
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
)

func TestRunApiRequestDeliversPreCallbackPanic(t *testing.T) {
	const panicSecret = "forced-secret-auth-request-panic"
	callCount := 0
	var callbackError error
	callback := connect.NewApiCallback(func(_ *AuthLoginResult, err error) {
		callCount += 1
		callbackError = err
	})

	runApiRequest(callback, func(connect.ApiCallback[*AuthLoginResult]) {
		panic(panicSecret)
	})

	if callCount != 1 {
		t.Fatalf("callback count = %d, want 1", callCount)
	}
	if !errors.Is(callbackError, errApiRequestFailed) {
		t.Fatalf("error = %v, want static request failure", callbackError)
	}
	if strings.Contains(callbackError.Error(), panicSecret) {
		t.Fatalf("error exposed panic value: %v", callbackError)
	}
}

func TestRunApiRequestDoesNotReplaceResultAfterRequestPanic(t *testing.T) {
	expected := &AuthLoginResult{UserName: "expected"}
	callCount := 0
	var observed *AuthLoginResult
	var callbackError error
	callback := connect.NewApiCallback(func(result *AuthLoginResult, err error) {
		callCount += 1
		observed = result
		callbackError = err
	})

	runApiRequest(callback, func(callback connect.ApiCallback[*AuthLoginResult]) {
		callback.Result(expected, nil)
		panic("panic after callback")
	})

	if callCount != 1 {
		t.Fatalf("callback count = %d, want 1", callCount)
	}
	if observed != expected || callbackError != nil {
		t.Fatalf("callback = (%p, %v), want (%p, nil)", observed, callbackError, expected)
	}
}

func TestRunApiRequestRejectsSilentReturn(t *testing.T) {
	callCount := 0
	var callbackError error
	callback := connect.NewApiCallback(func(_ *AuthLoginResult, err error) {
		callCount += 1
		callbackError = err
	})

	runApiRequest(callback, func(connect.ApiCallback[*AuthLoginResult]) {})

	if callCount != 1 {
		t.Fatalf("callback count = %d, want 1", callCount)
	}
	if !errors.Is(callbackError, errApiRequestReturnedWithoutCallback) {
		t.Fatalf("error = %v, want silent-return error", callbackError)
	}
}

func TestRunApiRequestDoesNotRedeliverAfterDelegatePanic(t *testing.T) {
	callCount := 0
	callback := connect.NewApiCallback(func(_ *AuthLoginResult, _ error) {
		callCount += 1
		panic("foreign callback panic")
	})

	runApiRequest(callback, func(callback connect.ApiCallback[*AuthLoginResult]) {
		callback.Result(&AuthLoginResult{}, nil)
	})

	if callCount != 1 {
		t.Fatalf("callback count = %d, want 1", callCount)
	}
}
