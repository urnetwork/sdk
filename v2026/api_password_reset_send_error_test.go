package sdk

import (
	"encoding/json"
	"strings"
	"testing"
)

// A password reset code the server did not send must reach the apps. The
// server reports it as `error` on /auth/password-reset when the request sets
// result_errors, with the verify send error codes. Before, these types had no
// such fields, so the json decode dropped the server's answer and the apps said
// a reset code was sent.

func TestAuthPasswordResetDecodesError(t *testing.T) {
	serverBody := `{"user_auth":"a@example.com","error":` + verifySendErrorWire + `}`
	body := verifySendErrorRoundTrip(t, serverBody, &AuthPasswordResetResult{})
	if !strings.Contains(body, `"error":`+verifySendErrorWire) {
		t.Fatalf("password reset dropped error: %s", body)
	}
}

func TestAuthPasswordResetArgsAskForResultErrors(t *testing.T) {
	// without result_errors the server keeps the legacy 429 / 502 statuses
	var args map[string]any
	argsBody := verifySendErrorRoundTrip(t, `{"user_auth":"a@example.com","result_errors":true}`, &AuthPasswordResetArgs{})
	if err := json.Unmarshal([]byte(argsBody), &args); err != nil {
		t.Fatal(err)
	}
	if args["result_errors"] != true {
		t.Fatalf("password reset args dropped result_errors: %s", argsBody)
	}
}
