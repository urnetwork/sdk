package sdk

import (
	"encoding/json"
	"strings"
	"testing"
)

// A verification code the server did not send must reach the apps. The
// server reports it as verification_required.send_error (login with password,
// network create) and as `error` on /auth/verify-send when the request sets
// result_errors. Before, these types had no such fields, so the json decode
// dropped the server's answer and the apps said a code was sent.
//
// Pure json round trips against the server's wire shape: decode what the
// server sends, encode it again, and require the field to survive.

func verifySendErrorRoundTrip[T any](t *testing.T, serverBody string, result T) string {
	if err := json.Unmarshal([]byte(serverBody), result); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

const verifySendErrorWire = `{"code":"verify_rate_limited","message":"Too many recent sign-in attempts for this account from your network address. Please try again in 5 minutes.","retry_after_seconds":300}`

func TestAuthLoginWithPasswordDecodesVerifySendError(t *testing.T) {
	serverBody := `{"verification_required":{"user_auth":"a@example.com","send_error":` + verifySendErrorWire + `},"network":{"name":"a"}}`
	body := verifySendErrorRoundTrip(t, serverBody, &AuthLoginWithPasswordResult{})
	if !strings.Contains(body, `"send_error":`+verifySendErrorWire) {
		t.Fatalf("login with password dropped verification_required.send_error: %s", body)
	}
}

func TestNetworkCreateDecodesVerifySendError(t *testing.T) {
	serverBody := `{"network":{"network_name":"a"},"verification_required":{"user_auth":"a@example.com","send_error":` + verifySendErrorWire + `}}`
	body := verifySendErrorRoundTrip(t, serverBody, &NetworkCreateResult{})
	if !strings.Contains(body, `"send_error":`+verifySendErrorWire) {
		t.Fatalf("network create dropped verification_required.send_error: %s", body)
	}
}

func TestAuthVerifySendDecodesError(t *testing.T) {
	serverBody := `{"user_auth":"a@example.com","error":{"code":"verify_send_failed","message":"The verification code could not be sent. Please try again."}}`
	body := verifySendErrorRoundTrip(t, serverBody, &AuthVerifySendResult{})
	if !strings.Contains(body, `"error":{"code":"verify_send_failed"`) {
		t.Fatalf("verify send dropped error: %s", body)
	}
}

func TestAuthVerifySendArgsAskForResultErrors(t *testing.T) {
	// without result_errors the server keeps the legacy 429 / 502 statuses
	var args map[string]any
	argsBody := verifySendErrorRoundTrip(t, `{"user_auth":"a@example.com","result_errors":true}`, &AuthVerifySendArgs{})
	if err := json.Unmarshal([]byte(argsBody), &args); err != nil {
		t.Fatal(err)
	}
	if args["result_errors"] != true {
		t.Fatalf("verify send args dropped result_errors: %s", argsBody)
	}
}
