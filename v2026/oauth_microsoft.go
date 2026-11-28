package sdk

// Sign in with Microsoft through the browser, shared by every app (server GET
// /auth/microsoft/start and GET /auth/microsoft/callback). No app carries the
// Microsoft client id: the app opens the start url in the system browser (a
// Custom Tab on Android, an authentication session on iOS and macOS, the
// default browser on Windows and Linux), the server sends the browser on to
// Microsoft with its own client, exchanges the authorization code with its
// client secret, and hands the identity token back to the app's own scheme:
//
//	<scheme>://oauth/microsoft?state=<state>&id_token=<identity token>
//	<scheme>://oauth/microsoft?state=<state>&error=<message>
//
// The server picks the scheme from the `platform` claim inside the state
// (base64url JSON {"platform":<platform>,"token":<random>}, server
// oauthSchemes): `ur` on Android, `urnetwork` on iOS, macOS, Windows and
// Linux. Two checks make a return usable, as for the Apple and Google browser
// flows: the echoed state must be this attempt's (a stray or replayed return
// never starts a sign-in), and the identity token's `nonce` claim must be the
// attempt's (the token was issued for this attempt, not lifted from another
// sign-in). The token's signature, audience and email are verified by the
// server when the app signs in with it (POST /auth/login, auth_jwt_type
// AuthJwtTypeMicrosoft) or adds it (POST /auth/add-auth).
//
// An attempt is safe for concurrent use; its values never change after it is
// made.

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// The auth_jwt_type of a Microsoft identity token (AuthLoginArgs.AuthJwtType,
// AddAuthArgs.AuthJwtType, NetworkCreateArgs.AuthJwtType).
const AuthJwtTypeMicrosoft = "microsoft"

// The platform claims of an attempt's state (server oauthSchemes).
const (
	OAuthPlatformAndroid = "android"
	OAuthPlatformIos     = "ios"
	OAuthPlatformMacos   = "macos"
	OAuthPlatformWindows = "windows"
	OAuthPlatformLinux   = "linux"
)

// The return url's host and path: <scheme>://oauth/microsoft
const (
	microsoftSignInReturnHost = "oauth"
	microsoftSignInReturnPath = "/microsoft"
)

// The codes of a Microsoft sign-in return that cannot be used, the start of
// the error's message ("<code>: <message>"). An app words each itself.
const (
	// the person backed out on Microsoft's page (access_denied)
	MicrosoftSignInErrorCodeCancelled = "cancelled"
	// the server has no Microsoft sign-in configured yet
	MicrosoftSignInErrorCodeNotConfigured = "not_configured"
	// Microsoft or the server refused the sign-in; the message says why
	MicrosoftSignInErrorCodeFailed = "failed"
	// the return carries no identity token
	MicrosoftSignInErrorCodeNoToken = "no_token"
	// the identity token was not issued for this attempt
	MicrosoftSignInErrorCodeNonceMismatch = "nonce_mismatch"
	// the attempt waited longer than its timeout
	MicrosoftSignInErrorCodeExpired = "expired"
	// the url is not this attempt's return (a stray or replayed return): the
	// app ignores it and keeps waiting
	MicrosoftSignInErrorCodeNotThisAttempt = "not_this_attempt"
)

// Kept off the bound api: no app sets it, and gobind skips a time.Duration
// field, which the Android build's mobileexports check rejects.
type microsoftSignInSettings struct {
	// how long an attempt waits for its return before it is stale. The
	// server's authorization code and Microsoft's page outlive it; five
	// minutes is the Apple and Google browser flows' timeout.
	AttemptTimeout time.Duration
}

func defaultMicrosoftSignInSettings() *microsoftSignInSettings {
	return &microsoftSignInSettings{
		AttemptTimeout: 5 * time.Minute,
	}
}

// One Microsoft sign-in attempt: its state and nonce, and where it returns.
type MicrosoftSignInAttempt struct {
	platform   string
	state      string
	nonce      string
	createTime time.Time
	settings   *microsoftSignInSettings
	now        func() time.Time
}

// The attempt's persisted form (Encode): what an app keeps when it may be
// stopped while the browser is open (Android), to finish the attempt when the
// return wakes it.
type microsoftSignInAttemptJson struct {
	Platform       string `json:"platform"`
	State          string `json:"state"`
	Nonce          string `json:"nonce"`
	CreateTimeUnix int64  `json:"create_time_unix"`
}

// The identity token of a usable return, for POST /auth/login or POST
// /auth/add-auth with AuthJwtTypeMicrosoft.
type MicrosoftSignInResult struct {
	IdToken string
}

// A new attempt for the platform, one of the OAuthPlatform* values.
func NewMicrosoftSignInAttempt(platform string) (*MicrosoftSignInAttempt, error) {
	return newMicrosoftSignInAttempt(platform, defaultMicrosoftSignInSettings(), time.Now)
}

func newMicrosoftSignInAttempt(platform string, settings *microsoftSignInSettings, now func() time.Time) (*MicrosoftSignInAttempt, error) {
	platform = strings.ToLower(strings.TrimSpace(platform))
	if microsoftSignInReturnScheme(platform) == "" {
		return nil, fmt.Errorf("unknown sign-in platform %q", platform)
	}
	token, err := microsoftSignInRandomToken()
	if err != nil {
		return nil, err
	}
	nonce, err := microsoftSignInRandomToken()
	if err != nil {
		return nil, err
	}
	stateJson, err := json.Marshal(struct {
		// the key order is fixed: the server reads only the platform
		Platform string `json:"platform"`
		Token    string `json:"token"`
	}{
		Platform: platform,
		Token:    token,
	})
	if err != nil {
		return nil, err
	}
	return &MicrosoftSignInAttempt{
		platform:   platform,
		state:      base64.RawURLEncoding.EncodeToString(stateJson),
		nonce:      nonce,
		createTime: now(),
		settings:   settings,
		now:        now,
	}, nil
}

// 32 random bytes, base64url without padding
func microsoftSignInRandomToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

// The scheme the server returns to for a platform claim, "" for an unknown
// platform.
func microsoftSignInReturnScheme(platform string) string {
	switch platform {
	case OAuthPlatformAndroid:
		return "ur"
	case OAuthPlatformIos, OAuthPlatformMacos, OAuthPlatformWindows, OAuthPlatformLinux:
		return "urnetwork"
	default:
		return ""
	}
}

// The attempt in its persisted form (DecodeMicrosoftSignInAttempt).
func (self *MicrosoftSignInAttempt) Encode() string {
	encoded, err := json.Marshal(&microsoftSignInAttemptJson{
		Platform:       self.platform,
		State:          self.state,
		Nonce:          self.nonce,
		CreateTimeUnix: self.createTime.Unix(),
	})
	if err != nil {
		return ""
	}
	return string(encoded)
}

// The attempt an app persisted with Encode.
func DecodeMicrosoftSignInAttempt(encoded string) (*MicrosoftSignInAttempt, error) {
	return decodeMicrosoftSignInAttempt(encoded, defaultMicrosoftSignInSettings(), time.Now)
}

func decodeMicrosoftSignInAttempt(encoded string, settings *microsoftSignInSettings, now func() time.Time) (*MicrosoftSignInAttempt, error) {
	var decoded microsoftSignInAttemptJson
	if err := json.Unmarshal([]byte(encoded), &decoded); err != nil {
		return nil, err
	}
	if microsoftSignInReturnScheme(decoded.Platform) == "" || decoded.State == "" || decoded.Nonce == "" {
		return nil, errors.New("not a Microsoft sign-in attempt")
	}
	return &MicrosoftSignInAttempt{
		platform:   decoded.Platform,
		state:      decoded.State,
		nonce:      decoded.Nonce,
		createTime: time.Unix(decoded.CreateTimeUnix, 0),
		settings:   settings,
		now:        now,
	}, nil
}

func (self *MicrosoftSignInAttempt) GetState() string {
	return self.state
}

func (self *MicrosoftSignInAttempt) GetNonce() string {
	return self.nonce
}

// The scheme the attempt returns to ("ur" or "urnetwork"), for the apps that
// tell their browser session which scheme ends it.
func (self *MicrosoftSignInAttempt) GetReturnScheme() string {
	return microsoftSignInReturnScheme(self.platform)
}

// The url the app opens in the browser: the api's start endpoint with the
// attempt's state and nonce. "" without an api url.
func (self *MicrosoftSignInAttempt) StartUrl(apiUrl string) string {
	apiUrl = strings.TrimRight(strings.TrimSpace(apiUrl), "/")
	if !strings.HasPrefix(apiUrl, "https://") && !strings.HasPrefix(apiUrl, "http://") {
		return ""
	}
	query := url.Values{}
	query.Set("state", self.state)
	query.Set("nonce", self.nonce)
	return fmt.Sprintf("%s/auth/microsoft/start?%s", apiUrl, query.Encode())
}

// Whether a url the app was opened with is a Microsoft sign-in return, of any
// attempt. An app routes such a url to its attempt (Finish), and ignores it
// when none is waiting.
func IsMicrosoftSignInReturnUrl(returnUrl string) bool {
	parsed, err := url.Parse(strings.TrimSpace(returnUrl))
	if err != nil {
		return false
	}
	scheme := strings.ToLower(parsed.Scheme)
	return (scheme == "ur" || scheme == "urnetwork") &&
		strings.EqualFold(parsed.Host, microsoftSignInReturnHost) &&
		strings.EqualFold(strings.TrimRight(parsed.Path, "/"), microsoftSignInReturnPath)
}

// Finishes the attempt with the url the browser returned. A usable return
// gives the identity token. Otherwise the error's message starts with one of
// the MicrosoftSignInErrorCode* codes; for MicrosoftSignInErrorCodeNotThisAttempt
// the app keeps waiting for its own return.
func (self *MicrosoftSignInAttempt) Finish(returnUrl string) (*MicrosoftSignInResult, error) {
	if !IsMicrosoftSignInReturnUrl(returnUrl) {
		return nil, microsoftSignInError(MicrosoftSignInErrorCodeNotThisAttempt, "not a Microsoft sign-in return")
	}
	parsed, err := url.Parse(strings.TrimSpace(returnUrl))
	if err != nil {
		return nil, microsoftSignInError(MicrosoftSignInErrorCodeNotThisAttempt, "not a Microsoft sign-in return")
	}
	if !strings.EqualFold(parsed.Scheme, self.GetReturnScheme()) {
		return nil, microsoftSignInError(MicrosoftSignInErrorCodeNotThisAttempt, "the return is for another app")
	}
	// the server builds the query with Go's url.Values.Encode, which writes a
	// space as "+"; ParseQuery reads it back
	params, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return nil, microsoftSignInError(MicrosoftSignInErrorCodeNotThisAttempt, "the return query is malformed")
	}
	if subtle.ConstantTimeCompare([]byte(params.Get("state")), []byte(self.state)) != 1 {
		return nil, microsoftSignInError(MicrosoftSignInErrorCodeNotThisAttempt, "the return is for another attempt")
	}
	if self.settings != nil && 0 < self.settings.AttemptTimeout && self.settings.AttemptTimeout < self.now().Sub(self.createTime) {
		return nil, microsoftSignInError(MicrosoftSignInErrorCodeExpired, "the sign-in took too long; try again")
	}
	if providerError := strings.TrimSpace(params.Get("error")); providerError != "" {
		switch providerError {
		case "access_denied", "user_cancelled", "cancelled":
			return nil, microsoftSignInError(MicrosoftSignInErrorCodeCancelled, "the sign-in was cancelled")
		case "not_configured":
			return nil, microsoftSignInError(MicrosoftSignInErrorCodeNotConfigured, "Sign in with Microsoft is not available yet")
		default:
			return nil, microsoftSignInError(MicrosoftSignInErrorCodeFailed, providerError)
		}
	}
	idToken := strings.TrimSpace(params.Get("id_token"))
	if idToken == "" {
		return nil, microsoftSignInError(MicrosoftSignInErrorCodeNoToken, "Microsoft did not return an identity token")
	}
	nonce, ok := microsoftSignInTokenNonce(idToken)
	if !ok || subtle.ConstantTimeCompare([]byte(nonce), []byte(self.nonce)) != 1 {
		return nil, microsoftSignInError(MicrosoftSignInErrorCodeNonceMismatch, "the identity token was not issued for this sign-in")
	}
	return &MicrosoftSignInResult{
		IdToken: idToken,
	}, nil
}

// The `nonce` claim of an identity token, read without verifying it (the
// server verifies the token).
func microsoftSignInTokenNonce(idToken string) (string, bool) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return "", false
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	var claims map[string]any
	if err := decoder.Decode(&claims); err != nil {
		return "", false
	}
	nonce, ok := claims["nonce"].(string)
	return nonce, ok && nonce != ""
}

func microsoftSignInError(code string, message string) error {
	return fmt.Errorf("%s: %s", code, message)
}

// The MicrosoftSignInErrorCode* code the message of an error of Finish starts
// with, else "". The apps get the message from the bound error.
func MicrosoftSignInErrorCodeOf(errorMessage string) string {
	code, _, found := strings.Cut(errorMessage, ": ")
	if !found {
		return ""
	}
	switch code {
	case MicrosoftSignInErrorCodeCancelled,
		MicrosoftSignInErrorCodeNotConfigured,
		MicrosoftSignInErrorCodeFailed,
		MicrosoftSignInErrorCodeNoToken,
		MicrosoftSignInErrorCodeNonceMismatch,
		MicrosoftSignInErrorCodeExpired,
		MicrosoftSignInErrorCodeNotThisAttempt:
		return code
	default:
		return ""
	}
}
