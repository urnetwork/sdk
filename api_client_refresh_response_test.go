package sdk

// The same completed-response decoder protects synchronous startup and the
// already-running token manager before either can dereference or publish it.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/urnetwork/connect"
)

func TestClientRefreshRejectsNullMixedAndDuplicateResponses(t *testing.T) {
	ctx, api := newTestApi(t, http.NotFoundHandler())
	original := testingRefreshableJwtWithMarker(t, "original-control")
	refreshed := testingRefreshableJwtWithMarker(t, "renewed-control")
	api.SetByJwt(original)
	var publications, logouts atomic.Int64
	refresh := api.AddJwtRefreshListener(jwtRefreshListenerFunc(func(string) { publications.Add(1) }))
	logout := api.AddAuthLogoutListener(authLogoutListenerFunc(func() { logouts.Add(1) }))
	defer refresh.Close()
	defer logout.Close()
	encoded, err := json.Marshal(refreshed)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		"null", "{}", "{",
		"{\"by_jwt\":" + string(encoded) + ",\"error\":{\"message\":\"synthetic conflicting refusal\"}}",
		"{\"by_jwt\":\"discarded\",\"by_jwt\":" + string(encoded) + "}",
		"{\"error\":{\"message\":\"first\",\"message\":\"second\"}}",
	} {
		api.setHttpGetRaw(func(context.Context, string, string) ([]byte, error) { return []byte(raw), nil })
		if result, err := api.RefreshJwtSyncWithContext(ctx); err == nil || result != nil {
			t.Fatal("completed ambiguous refresh reached startup as success")
		}
		outcome := api.tokenManager.refreshTokenWithContext(ctx, original)
		if outcome.err == nil || outcome.loggedOut || outcome.stale || api.GetByJwt() != original || publications.Load() != 0 || logouts.Load() != 0 {
			t.Fatal("completed ambiguous refresh changed live ownership or panicked")
		}
	}
}

func TestClientRefreshRetainsExistingPublicationIdentityCheck(t *testing.T) {
	ctx, api := newTestApi(t, http.NotFoundHandler())
	original := testingRefreshableJwtWithMarker(t, "original-control")
	claims := gojwt.MapClaims{}
	if _, _, err := gojwt.NewParser().ParseUnverified(original, claims); err != nil {
		t.Fatal(err)
	}
	claims["iat"] = claims["exp"].(float64) - 30*24*60*60
	claims["exp"] = claims["exp"].(float64) + 3600
	refreshed, err := gojwt.NewWithClaims(gojwt.SigningMethodNone, claims).SignedString(gojwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	api.SetByJwt(original)
	raw, err := json.Marshal(RefreshJwtResult{ByJwt: refreshed})
	if err != nil {
		t.Fatal(err)
	}
	api.setHttpGetRaw(func(context.Context, string, string) ([]byte, error) { return raw, nil })
	outcome := api.tokenManager.refreshTokenWithContext(ctx, original)
	if outcome.err != nil || outcome.loggedOut || outcome.stale || api.GetByJwt() != refreshed {
		t.Fatalf("legitimate renewed token was rejected: %v", outcome.err)
	}
}

// The already-running consumer reaches the shared decoder without a startup
// caller first refusing the response. The old null response panicked here.
func TestClientRefreshBackgroundRejectsNull(t *testing.T) {
	defer func() {
		if recover() != nil {
			t.Fatal("background refresh dereferenced a null completed response")
		}
	}()
	ctx, api := newTestApi(t, http.NotFoundHandler())
	original := testingRefreshableJwtWithMarker(t, "background-null")
	api.SetByJwt(original)
	api.setHttpGetRaw(func(context.Context, string, string) ([]byte, error) { return []byte("null"), nil })
	outcome := api.tokenManager.refreshTokenWithContext(ctx, original)
	if outcome.err == nil || outcome.loggedOut || api.GetByJwt() != original {
		t.Fatal("background null response changed live credential ownership")
	}
}

// A401 leaf cannot erase credentials when the complete transport outcome also
// carries a timeout or bad response. The existing generation owner stays live.
func TestClientRefreshMixedRejectionRetainsCredential(t *testing.T) {
	ctx, api := newTestApi(t, http.NotFoundHandler())
	original := testingRefreshableJwtWithMarker(t, "original-mixed")
	api.SetByJwt(original)
	for _, cause := range []error{context.DeadlineExceeded, &ClientControlResponseError{detail: "synthetic mixed response"}} {
		err := errors.Join(&connect.HttpStatusError{StatusCode: http.StatusUnauthorized}, cause)
		api.setHttpGetRaw(func(context.Context, string, string) ([]byte, error) { return nil, err })
		outcome := api.tokenManager.refreshTokenWithContext(ctx, original)
		if outcome.err == nil || outcome.loggedOut || outcome.stale || api.GetByJwt() != original {
			t.Fatal("mixed refresh failure erased original credential")
		}
	}
}
