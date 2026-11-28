// The Microsoft browser sign-in attempt: the state and start url the server
// reads, and the checks that make a return usable. Pure, with an injected
// clock.
package sdk

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// An unsigned identity token with the claims, as the server's callback hands
// it back (the signature is the server's to check).
func microsoftTestIdToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".c2lnbmF0dXJl"
}

// The return url the server's callback builds for the attempt.
func microsoftTestReturnUrl(scheme string, values url.Values) string {
	return scheme + "://oauth/microsoft?" + values.Encode()
}

type microsoftTestClock struct {
	now time.Time
}

func (self *microsoftTestClock) Now() time.Time {
	return self.now
}

func newMicrosoftTestAttempt(t *testing.T, platform string) (*MicrosoftSignInAttempt, *microsoftTestClock) {
	t.Helper()
	clock := &microsoftTestClock{now: time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)}
	attempt, err := newMicrosoftSignInAttempt(platform, defaultMicrosoftSignInSettings(), clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	return attempt, clock
}

// The state is base64url JSON whose platform claim the server maps to the
// app's scheme; the token makes it unique, and the nonce is its own random
// value. The start url carries both to the api's start endpoint.
func TestMicrosoftSignInAttemptStateAndStartUrl(t *testing.T) {
	for _, c := range []struct {
		platform string
		scheme   string
	}{
		{OAuthPlatformAndroid, "ur"},
		{OAuthPlatformIos, "urnetwork"},
		{OAuthPlatformMacos, "urnetwork"},
		{OAuthPlatformWindows, "urnetwork"},
		{" Linux ", "urnetwork"},
	} {
		attempt, _ := newMicrosoftTestAttempt(t, c.platform)
		connect.AssertEqual(t, attempt.GetReturnScheme(), c.scheme)

		decoded, err := base64.RawURLEncoding.DecodeString(attempt.GetState())
		if err != nil {
			t.Fatalf("%s: the state is not base64url: %v", c.platform, err)
		}
		claims := map[string]string{}
		if err := json.Unmarshal(decoded, &claims); err != nil {
			t.Fatalf("%s: the state is not json: %v", c.platform, err)
		}
		connect.AssertEqual(t, claims["platform"], strings.ToLower(strings.TrimSpace(c.platform)))
		if len(claims["token"]) < 40 {
			t.Fatalf("%s: the state token %q is short", c.platform, claims["token"])
		}
		if len(attempt.GetNonce()) < 40 || attempt.GetNonce() == claims["token"] {
			t.Fatalf("%s: the nonce %q is short or reuses the state token", c.platform, attempt.GetNonce())
		}

		startUrl, err := url.Parse(attempt.StartUrl("https://api.example.com/"))
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, startUrl.Scheme+"://"+startUrl.Host+startUrl.Path, "https://api.example.com/auth/microsoft/start")
		connect.AssertEqual(t, startUrl.Query().Get("state"), attempt.GetState())
		connect.AssertEqual(t, startUrl.Query().Get("nonce"), attempt.GetNonce())
	}

	other, _ := newMicrosoftTestAttempt(t, OAuthPlatformAndroid)
	again, _ := newMicrosoftTestAttempt(t, OAuthPlatformAndroid)
	if other.GetState() == again.GetState() || other.GetNonce() == again.GetNonce() {
		t.Fatal("two attempts share a state or a nonce")
	}

	if _, err := NewMicrosoftSignInAttempt("web"); err == nil {
		t.Fatal("an attempt for a platform without an app scheme was made")
	}
	attempt, _ := newMicrosoftTestAttempt(t, OAuthPlatformWindows)
	connect.AssertEqual(t, attempt.StartUrl(""), "")
	connect.AssertEqual(t, attempt.StartUrl("api.example.com"), "")
}

// A return with the attempt's state and a token minted for its nonce gives the
// token; the query's "+" spaces read back.
func TestMicrosoftSignInAttemptFinishes(t *testing.T) {
	attempt, _ := newMicrosoftTestAttempt(t, OAuthPlatformWindows)
	idToken := microsoftTestIdToken(t, map[string]any{"nonce": attempt.GetNonce(), "email": "person@example.com"})
	result, err := attempt.Finish(microsoftTestReturnUrl("urnetwork", url.Values{"state": {attempt.GetState()}, "id_token": {idToken}}))
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, result.IdToken, idToken)
	connect.AssertEqual(t, IsMicrosoftSignInReturnUrl("URNETWORK://oauth/microsoft/?state=x"), true)
}

// Every return the attempt must not use: another attempt's or another app's
// return (ignored, the attempt keeps waiting), a token issued for another
// nonce or none, the provider's refusals, and a return after the timeout.
func TestMicrosoftSignInAttemptRefusesUnusableReturns(t *testing.T) {
	attempt, clock := newMicrosoftTestAttempt(t, OAuthPlatformAndroid)
	state := attempt.GetState()
	ownToken := microsoftTestIdToken(t, map[string]any{"nonce": attempt.GetNonce()})
	for _, c := range []struct {
		name      string
		returnUrl string
		code      string
	}{
		{"not a return", "ur://oauth/apple?" + url.Values{"state": {state}, "id_token": {ownToken}}.Encode(), MicrosoftSignInErrorCodeNotThisAttempt},
		{"another scheme", microsoftTestReturnUrl("urnetwork", url.Values{"state": {state}, "id_token": {ownToken}}), MicrosoftSignInErrorCodeNotThisAttempt},
		{"another attempt", microsoftTestReturnUrl("ur", url.Values{"state": {state + "x"}, "id_token": {ownToken}}), MicrosoftSignInErrorCodeNotThisAttempt},
		{"no state", microsoftTestReturnUrl("ur", url.Values{"id_token": {ownToken}}), MicrosoftSignInErrorCodeNotThisAttempt},
		{"cancelled", microsoftTestReturnUrl("ur", url.Values{"state": {state}, "error": {"access_denied"}}), MicrosoftSignInErrorCodeCancelled},
		{"not configured", microsoftTestReturnUrl("ur", url.Values{"state": {state}, "error": {"not_configured"}}), MicrosoftSignInErrorCodeNotConfigured},
		{"refused", microsoftTestReturnUrl("ur", url.Values{"state": {state}, "error": {"Microsoft rejected the sign-in: invalid_grant."}}), MicrosoftSignInErrorCodeFailed},
		{"no token", microsoftTestReturnUrl("ur", url.Values{"state": {state}}), MicrosoftSignInErrorCodeNoToken},
		{"another nonce", microsoftTestReturnUrl("ur", url.Values{"state": {state}, "id_token": {microsoftTestIdToken(t, map[string]any{"nonce": "lifted-from-another-sign-in"})}}), MicrosoftSignInErrorCodeNonceMismatch},
		{"no nonce", microsoftTestReturnUrl("ur", url.Values{"state": {state}, "id_token": {microsoftTestIdToken(t, map[string]any{"email": "person@example.com"})}}), MicrosoftSignInErrorCodeNonceMismatch},
		{"not a token", microsoftTestReturnUrl("ur", url.Values{"state": {state}, "id_token": {"not-a-jwt"}}), MicrosoftSignInErrorCodeNonceMismatch},
	} {
		result, err := attempt.Finish(c.returnUrl)
		if err == nil || result != nil {
			t.Errorf("%s: the return was used", c.name)
			continue
		}
		if code := MicrosoftSignInErrorCodeOf(err.Error()); code != c.code {
			t.Errorf("%s: code %q (%v), want %q", c.name, code, err, c.code)
		}
	}

	clock.now = clock.now.Add(5*time.Minute + time.Second)
	_, err := attempt.Finish(microsoftTestReturnUrl("ur", url.Values{"state": {state}, "id_token": {ownToken}}))
	if err == nil || MicrosoftSignInErrorCodeOf(err.Error()) != MicrosoftSignInErrorCodeExpired {
		t.Fatalf("a return after the timeout: %v", err)
	}
	connect.AssertEqual(t, MicrosoftSignInErrorCodeOf("something else"), "")
}

// An attempt an app persisted while the browser was open finishes after it is
// read back, with the same state, nonce and age.
func TestMicrosoftSignInAttemptEncodeDecode(t *testing.T) {
	attempt, clock := newMicrosoftTestAttempt(t, OAuthPlatformAndroid)
	decoded, err := decodeMicrosoftSignInAttempt(attempt.Encode(), defaultMicrosoftSignInSettings(), clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, decoded.GetState(), attempt.GetState())
	connect.AssertEqual(t, decoded.GetNonce(), attempt.GetNonce())
	connect.AssertEqual(t, decoded.GetReturnScheme(), "ur")
	idToken := microsoftTestIdToken(t, map[string]any{"nonce": attempt.GetNonce()})
	if _, err := decoded.Finish(microsoftTestReturnUrl("ur", url.Values{"state": {attempt.GetState()}, "id_token": {idToken}})); err != nil {
		t.Fatal(err)
	}

	clock.now = clock.now.Add(10 * time.Minute)
	stale, err := decodeMicrosoftSignInAttempt(attempt.Encode(), defaultMicrosoftSignInSettings(), clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stale.Finish(microsoftTestReturnUrl("ur", url.Values{"state": {attempt.GetState()}, "id_token": {idToken}})); err == nil {
		t.Fatal("a decoded attempt outlived its timeout")
	}

	for _, encoded := range []string{"", "{}", `{"platform":"web","state":"s","nonce":"n"}`, `{"platform":"android","state":"","nonce":"n"}`} {
		if _, err := DecodeMicrosoftSignInAttempt(encoded); err == nil {
			t.Errorf("%q decoded as an attempt", encoded)
		}
	}
}
