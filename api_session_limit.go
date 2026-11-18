package sdk

// api_session_limit.go -- the server's refusal of a new session for a network
// that holds its cap of active sessions (server session/, owner decisions 6
// and 7 of 2026-11-18).
//
// Once the cap is enforced, a sign-in, a code sign-in or a new client whose
// session would go over the cap is refused with HTTP 409 and the server's
// structured body {"code":"session_limit_reached","error":"..."}. Nothing is
// signed out to make room. The user frees a session by signing out the ones
// they no longer use in Account > Sessions, on a device that is signed in.
//
// Every route that can create a session reads that refusal as its result,
// with Error.Code SessionErrorCodeLimitReached and an English Error.Message,
// the same way auth-client reads a 402 as its upgrade refusal: each app
// already shows a result's error where it made the call, so it can say there
// are too many active sessions in its own words and point to
// Account > Sessions. The routes: /auth/login (SSO, wallet and recovery
// phrase sign-ins), /auth/login-with-password, /auth/verify,
// /auth/network-create, /auth/code-login and /network/auth-client.
//
// A 409 with any other code is not this refusal. In particular
// session_retention_limit_reached, the cap of revoked sessions the server
// still remembers, is not freed by signing sessions out, so it stays an error.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/urnetwork/connect"
)

const (
	// Keep this untyped so gomobile exports it on every app platform.

	// The Error.Code of a refused sign-in, code sign-in or new client: the
	// network holds its cap of active sessions. Show that there are too many
	// active sessions and point to Account > Sessions, where the user signs
	// out the sessions they no longer use (on a device that is signed in,
	// when the refused call was a sign-in).
	SessionErrorCodeLimitReached = "session_limit_reached"
)

// The Message of the refusal in a result, for a caller that shows a result's
// message as it is. Apps word the refusal from its code.
const sessionLimitReachedMessage = "Too many active sessions. Sign out sessions you no longer use in Account > Sessions, then try again."

// Whether err is the server's session cap refusal: a 409 whose JSON body has
// the code session_limit_reached, at the top level as the server's session
// errors have it, or in an error object as a result's error has it. A
// malformed typed-nil error is no refusal and is left as it is (errors.As
// would call its methods on the nil pointer).
func isSessionLimitRefusal(err error) bool {
	if clientControlErrorIsNil(err) {
		return false
	}
	var status *connect.HttpStatusError
	if !errors.As(err, &status) || status == nil || status.StatusCode != http.StatusConflict {
		return false
	}
	var refusal struct {
		Code  string          `json:"code"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(status.Body, &refusal) != nil {
		return false
	}
	if refusal.Code == SessionErrorCodeLimitReached {
		return true
	}
	var resultError struct {
		Code string `json:"code"`
	}
	return json.Unmarshal(refusal.Error, &resultError) == nil && resultError.Code == SessionErrorCodeLimitReached
}

// The body that a result reads the refusal from: every result of a route
// that creates a session has its error at "error", with "code" and
// "message".
func sessionLimitRefusalResultBody() []byte {
	body, _ := json.Marshal(map[string]any{
		"error": map[string]string{
			"code":    SessionErrorCodeLimitReached,
			"message": sessionLimitReachedMessage,
		},
	})
	return body
}

// Wraps the raw POST of a route that can create a session so that the
// server's session cap refusal answers with the route's result, its error
// carrying the code (the file header). Every other answer passes through.
func sessionLimitRefusalHttpPostRaw(httpPostRaw connect.HttpPostRawFunction) connect.HttpPostRawFunction {
	return func(ctx context.Context, requestUrl string, requestBodyBytes []byte, byJwt string) ([]byte, error) {
		bodyBytes, err := httpPostRaw(ctx, requestUrl, requestBodyBytes, byJwt)
		if isSessionLimitRefusal(err) {
			return sessionLimitRefusalResultBody(), nil
		}
		return bodyBytes, err
	}
}
