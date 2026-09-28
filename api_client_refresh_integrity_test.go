package sdk

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

type clientRefreshIntegrityTestListener func(string)

func (self clientRefreshIntegrityTestListener) ClientRefreshInvalid(original string) { self(original) }

// Both completed JSON and the existing identity validator report refusal,
// retaining the original token and never invoking successful publication.
func TestClientRefreshIntegrityReportsOriginalOwner(t *testing.T) {
	ctx, api := newTestApi(t, http.NotFoundHandler())
	original := testingRefreshableJwtWithMarker(t, "integrity-owner")
	api.SetByJwt(original)
	observed := []string{}
	listener := api.AddClientRefreshIntegrityListener(clientRefreshIntegrityTestListener(func(value string) { observed = append(observed, value) }))
	defer listener.Close()
	changed, err := json.Marshal(RefreshJwtResult{ByJwt: "synthetic invalid JWT"})
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range [][]byte{[]byte("null"), changed} {
		api.setHttpGetRaw(func(context.Context, string, string) ([]byte, error) { return raw, nil })
		outcome := api.tokenManager.refreshTokenWithContext(ctx, original)
		if outcome.err == nil || api.GetByJwt() != original {
			t.Fatal("invalid live refresh changed original ownership")
		}
	}
	if len(observed) != 2 || observed[0] != original || observed[1] != original {
		t.Fatal("completed invalid live refresh omitted its owned integrity notice")
	}
}

// A newer equal-byte login is a distinct owner. A late invalid reply cannot
// disable its dependent work merely because the bearer bytes happen to match.
func TestClientRefreshIntegrityRejectsStaleGeneration(t *testing.T) {
	ctx, api := newTestApi(t, http.NotFoundHandler())
	original := testingRefreshableJwtWithMarker(t, "stale-integrity")
	api.SetByJwt(original)
	observed := false
	listener := api.AddClientRefreshIntegrityListener(clientRefreshIntegrityTestListener(func(string) { observed = true }))
	defer listener.Close()
	api.setHttpGetRaw(func(context.Context, string, string) ([]byte, error) {
		api.SetByJwt(original)
		return []byte("null"), nil
	})
	outcome := api.tokenManager.refreshTokenWithContext(ctx, original)
	if outcome.err == nil || observed || api.GetByJwt() != original {
		t.Fatal("stale invalid refresh reported against a new login owner")
	}
}
