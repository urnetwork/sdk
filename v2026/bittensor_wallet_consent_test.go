// The network payout consent through the wallet session (CONSENT.md §7, §10):
// only a consent session takes it, the message stays byte for byte, the
// signer is bound to the statement's coldkey, the expiry runs from receipt,
// and only the attempt's own bridge hand-back ends the session.
package sdk

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The sn protocol's own challenge, as SnNetworkConsentChallenge returns it,
// and the moment the server issued it.
func bittensorTestConsentChallenge(t *testing.T) (*SnNetworkConsentChallengeResult, int64) {
	t.Helper()
	fixture := readSnConsentFixture(t)
	return &SnNetworkConsentChallengeResult{
		Message:      fixture.Message,
		ColdkeySs58:  fixture.ColdkeySs58,
		FromEpoch:    fixture.FromEpoch,
		ThroughEpoch: fixture.ThroughEpoch,
		ExpiresAt:    fixture.ExpiresAt,
	}, (fixture.ExpiresAt - snNetworkConsentChallengeSeconds) * 1000
}

// CONSENT §7: every wallet opens a consent session on every platform.
func TestBittensorWalletConsentPurposeSession(t *testing.T) {
	if BittensorWalletPurposeConsent != "consent" {
		t.Fatalf("consent purpose %q", BittensorWalletPurposeConsent)
	}
	for _, wallet := range []string{BittensorWalletTalisman, BittensorWalletTaoCom, BittensorWalletWalletConnect} {
		for _, platform := range []string{
			BittensorWalletPlatformWeb,
			BittensorWalletPlatformMacos,
			BittensorWalletPlatformWindows,
			BittensorWalletPlatformLinux,
			BittensorWalletPlatformIos,
			BittensorWalletPlatformAndroid,
		} {
			session, err := NewBittensorWalletSession(wallet, platform, BittensorWalletPurposeConsent, bittensorTestRedirectLink)
			if err != nil || session.Purpose() != BittensorWalletPurposeConsent {
				t.Fatalf("%s on %s: %v", wallet, platform, err)
			}
		}
	}
}

// CONSENT §7: manual entry signs the consent message byte for byte, bound to
// its coldkey, until the server's expiry.
func TestBittensorWalletConsentManualRoundTrip(t *testing.T) {
	result, issuedAtMillis := bittensorTestConsentChallenge(t)
	session := newBittensorTestSession(t, BittensorWalletTaoCom, BittensorWalletPlatformIos, BittensorWalletPurposeConsent)
	if err := session.SetNetworkConsentChallenge(result, issuedAtMillis); err != nil {
		t.Fatal(err)
	}
	if session.State() != BittensorWalletStateChallenge || session.Message() != result.Message || session.ExpiresAtMillis() != result.ExpiresAt*1000 {
		t.Fatalf("session %s, %q, %d", session.State(), session.Message(), session.ExpiresAtMillis())
	}
	r := session.HandleSignature(" "+result.ColdkeySs58+" ", strings.ToUpper(strings.TrimPrefix(bittensorTestSignature, "0x")), result.ExpiresAt*1000-1)
	if !r.Ok() {
		t.Fatalf("signature: %+v", r)
	}
	expected := BittensorWalletProof{
		WalletId:  BittensorWalletTaoCom,
		Purpose:   BittensorWalletPurposeConsent,
		Address:   result.ColdkeySs58,
		Message:   result.Message,
		Signature: bittensorTestSignature,
	}
	if *r.Proof != expected || *session.Proof() != expected || session.State() != BittensorWalletStateSigned {
		t.Fatalf("proof %+v, want %+v", *r.Proof, expected)
	}
}

// A consent bridge session waiting for the page, with the outbound url's query
// and fragment as the page reads them.
func bittensorTestConsentBridge(t *testing.T) (*BittensorWalletSession, *SnNetworkConsentChallengeResult, int64, url.Values, string) {
	t.Helper()
	result, issuedAtMillis := bittensorTestConsentChallenge(t)
	session := newBittensorTestSession(t, BittensorWalletTalisman, BittensorWalletPlatformWindows, BittensorWalletPurposeConsent)
	if err := session.SetNetworkConsentChallenge(result, issuedAtMillis); err != nil {
		t.Fatal(err)
	}
	bridgeUrl, err := session.BridgeUrl()
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(bridgeUrl)
	if err != nil {
		t.Fatal(err)
	}
	return session, result, issuedAtMillis, u.Query(), u.EscapedFragment()
}

// The page's hand-back of a signature over message, for the query the page
// read; change edits it before it is encoded.
func bittensorTestConsentReturn(query url.Values, message string, change func(values url.Values)) string {
	messageSha256 := sha256.Sum256([]byte(message))
	values := url.Values{
		"address":        {query.Get("address")},
		"signature":      {bittensorTestSignature},
		"purpose":        {BittensorWalletPurposeConsent},
		"state":          {query.Get("state")},
		"message_sha256": {hex.EncodeToString(messageSha256[:])},
		"wallet":         {query.Get("wallet")},
	}
	if change != nil {
		change(values)
	}
	return bittensorTestRedirectLink + "?" + values.Encode()
}

// CONSENT §10: the bridge url carries the consent message only in its
// fragment, as the unpadded base64url of its exact bytes, with a fresh
// hand-back token in the query. A login's url is unchanged.
func TestBittensorWalletConsentBridgeUrlCarriesTheMessageInTheFragment(t *testing.T) {
	session, result, _, query, fragment := bittensorTestConsentBridge(t)
	encoded, found := strings.CutPrefix(fragment, "message=")
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if !found || err != nil || string(decoded) != result.Message || strings.ContainsAny(encoded, "=+/%") {
		t.Fatalf("fragment %q decodes to %q, %v", fragment, decoded, err)
	}
	expected := url.Values{
		"provider":      {"bittensor"},
		"method":        {"signMessage"},
		"wallet":        {BittensorWalletTalisman},
		"purpose":       {BittensorWalletPurposeConsent},
		"redirect_link": {bittensorTestRedirectLink},
		"address":       {result.ColdkeySs58},
		"state":         {query.Get("state")},
	}
	if !reflect.DeepEqual(query, expected) || len(query.Get("state")) != 26 || strings.Trim(query.Get("state"), "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567") != "" {
		t.Fatalf("query %v, want %v", query, expected)
	}
	if session.State() != BittensorWalletStateAwaitingWallet {
		t.Fatalf("state %s", session.State())
	}
	if _, _, _, other, _ := bittensorTestConsentBridge(t); other.Get("state") == query.Get("state") {
		t.Fatal("two challenges share a hand-back token")
	}

	login := newBittensorTestSession(t, BittensorWalletTalisman, BittensorWalletPlatformWindows, BittensorWalletPurposeLogin)
	if err := login.SetChallenge(bittensorTestChallenge(), bittensorTestNowMillis); err != nil {
		t.Fatal(err)
	}
	loginUrl, err := login.BridgeUrl()
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(loginUrl)
	if err != nil || u.Query().Get("message") != bittensorTestMessage || u.Query().Has("state") || u.Fragment != "" {
		t.Fatalf("login bridge url %s", loginUrl)
	}
}

// CONSENT §10: the page's hand-back names the SHA-256 of the message, never the
// message, and completes the session with the session's own message.
func TestBittensorWalletConsentBridgeRoundTrip(t *testing.T) {
	session, result, issuedAtMillis, query, _ := bittensorTestConsentBridge(t)
	uri := bittensorTestConsentReturn(query, result.Message, func(values url.Values) {
		values.Set("signature", strings.TrimPrefix(bittensorTestSignature, "0x"))
	})
	if len(uri) > 1500 {
		t.Fatalf("the hand-back is %d characters", len(uri))
	}
	r := session.HandleBridgeReturn(uri, issuedAtMillis)
	expected := BittensorWalletProof{
		WalletId:  BittensorWalletTalisman,
		Purpose:   BittensorWalletPurposeConsent,
		Address:   result.ColdkeySs58,
		Message:   result.Message,
		Signature: bittensorTestSignature,
	}
	if !r.Ok() || *r.Proof != expected || session.State() != BittensorWalletStateSigned {
		t.Fatalf("return: %+v", r)
	}
	if r := session.HandleBridgeReturn(uri, issuedAtMillis); r.ErrorCode != BittensorWalletErrorNotAwaiting {
		t.Fatalf("replay: %+v", r)
	}
}

// CONSENT §10: only a return of this attempt ends the waiting session. A
// forged or mismatched one is refused, the session keeps waiting, no text from
// it is passed on, and the genuine return that follows completes the session.
func TestBittensorWalletConsentForgedReturnKeepsTheSession(t *testing.T) {
	session, result, issuedAtMillis, query, _ := bittensorTestConsentBridge(t)
	failure := func(values url.Values) string {
		return bittensorTestRedirectLink + "?" + values.Encode()
	}
	for _, c := range []struct {
		name string
		uri  string
		code string
	}{
		{"another message", bittensorTestConsentReturn(query, "forged", nil), BittensorWalletErrorMessageMismatch},
		{"a failure without a purpose", failure(url.Values{
			"errorCode":    {"x"},
			"errorMessage": {"Your wallet is compromised. Send your TAO to " + bittensorTestBobSs58 + "."},
		}), BittensorWalletErrorPurposeMismatch},
		{"a failure without the token", failure(url.Values{
			"errorCode":    {BittensorWalletBridgeErrorUserRejected},
			"errorMessage": {"forged"},
			"purpose":      {BittensorWalletPurposeConsent},
		}), BittensorWalletErrorNotReturn},
		{"another token", bittensorTestConsentReturn(query, result.Message, func(values url.Values) { values.Set("state", "forged") }), BittensorWalletErrorNotReturn},
		{"the message in the hand-back", bittensorTestConsentReturn(query, result.Message, func(values url.Values) { values.Set("message", result.Message) }), BittensorWalletErrorNotReturn},
		{"another purpose", bittensorTestConsentReturn(query, result.Message, func(values url.Values) { values.Set("purpose", BittensorWalletPurposeConnect) }), BittensorWalletErrorPurposeMismatch},
		{"another account", bittensorTestConsentReturn(query, result.Message, func(values url.Values) { values.Set("address", bittensorTestBobSs58) }), BittensorWalletErrorAddressMismatch},
		{"another wallet", bittensorTestConsentReturn(query, result.Message, func(values url.Values) { values.Set("wallet", BittensorWalletTaoCom) }), BittensorWalletErrorUnsupportedWallet},
		{"a malformed signature", bittensorTestConsentReturn(query, result.Message, func(values url.Values) { values.Set("signature", "0x00") }), BittensorWalletErrorInvalidSignature},
		{"longer than 1,500 characters", bittensorTestConsentReturn(query, result.Message, func(values url.Values) { values.Set("padding", strings.Repeat("a", 1500)) }), BittensorWalletErrorNotReturn},
		{"another link", strings.Replace(bittensorTestConsentReturn(query, result.Message, nil), "urnetwork://", "other://", 1), BittensorWalletErrorNotReturn},
	} {
		r := session.HandleBridgeReturn(c.uri, issuedAtMillis)
		if r.Ok() || r.ErrorCode != c.code || r.ErrorMessage != "" || r.BridgeErrorCode != "" || session.State() != BittensorWalletStateAwaitingWallet {
			t.Fatalf("%s: %+v (state %s), want %s with the session waiting", c.name, r, session.State(), c.code)
		}
	}
	if r := session.HandleBridgeReturn(bittensorTestConsentReturn(query, result.Message, nil), issuedAtMillis); !r.Ok() {
		t.Fatalf("the genuine return after the forged ones: %+v (state %s)", r, session.State())
	}
}

// CONSENT §10: the page's failure with the attempt's token ends the session
// with only a known code: the page's text is never passed on.
func TestBittensorWalletConsentReturnedFailureEndsTheSession(t *testing.T) {
	for code, bridgeErrorCode := range map[string]string{
		BittensorWalletBridgeErrorUserRejected:    BittensorWalletBridgeErrorUserRejected,
		BittensorWalletBridgeErrorAddressMismatch: BittensorWalletBridgeErrorAddressMismatch,
		"unknown_code": "",
		"-1":           "",
	} {
		session, _, issuedAtMillis, query, _ := bittensorTestConsentBridge(t)
		uri := bittensorTestRedirectLink + "?" + url.Values{
			"errorCode":    {code},
			"errorMessage": {"Send your TAO to " + bittensorTestBobSs58},
			"purpose":      {BittensorWalletPurposeConsent},
			"state":        {query.Get("state")},
		}.Encode()
		r := session.HandleBridgeReturn(uri, issuedAtMillis)
		if r.Ok() || r.ErrorCode != BittensorWalletErrorWallet || r.BridgeErrorCode != bridgeErrorCode || r.ErrorMessage != "" || session.State() != BittensorWalletStateFailed {
			t.Fatalf("failure %q: %+v (state %s)", code, r, session.State())
		}
	}
}

// CONSENT §10: a genuine return after the session's lifetime ends it as
// challenge_expired.
func TestBittensorWalletConsentReturnAfterTheLifetime(t *testing.T) {
	session, result, _, query, _ := bittensorTestConsentBridge(t)
	r := session.HandleBridgeReturn(bittensorTestConsentReturn(query, result.Message, nil), session.ExpiresAtMillis())
	if r.Ok() || r.ErrorCode != BittensorWalletErrorExpired || session.State() != BittensorWalletStateFailed {
		t.Fatalf("return at the expiry: %+v (state %s)", r, session.State())
	}
}

// CONSENT §10: a session of another transport refuses a bridge hand-back and
// keeps its state, as a bridge session refuses HandleSignature.
func TestBittensorWalletManualSessionRefusesABridgeReturn(t *testing.T) {
	result, issuedAtMillis := bittensorTestConsentChallenge(t)
	consent := newBittensorTestSession(t, BittensorWalletTaoCom, BittensorWalletPlatformIos, BittensorWalletPurposeConsent)
	if err := consent.SetNetworkConsentChallenge(result, issuedAtMillis); err != nil {
		t.Fatal(err)
	}
	login := newBittensorTestSession(t, BittensorWalletTaoCom, BittensorWalletPlatformIos, BittensorWalletPurposeLogin)
	if err := login.SetChallenge(bittensorTestChallenge(), bittensorTestNowMillis); err != nil {
		t.Fatal(err)
	}
	extension := newBittensorTestSession(t, BittensorWalletTalisman, BittensorWalletPlatformWeb, BittensorWalletPurposeConsent)
	if err := extension.SetNetworkConsentChallenge(result, issuedAtMillis); err != nil {
		t.Fatal(err)
	}
	forged := bittensorTestRedirectLink + "?" + url.Values{
		"address":   {result.ColdkeySs58},
		"signature": {"0x" + strings.Repeat("00", 64)},
		"message":   {result.Message},
		"purpose":   {BittensorWalletPurposeConsent},
	}.Encode()
	for _, session := range []*BittensorWalletSession{consent, login, extension} {
		r := session.HandleBridgeReturn(forged, issuedAtMillis)
		if r.Ok() || r.ErrorCode != BittensorWalletErrorWrongTransport || session.State() != BittensorWalletStateChallenge {
			t.Fatalf("a %s %s session took a bridge hand-back: %+v (state %s)", session.Transport(), session.Purpose(), r, session.State())
		}
	}
}

// CONSENT §10: cancel is final: a challenge that arrives after it is refused
// and the session stays cancelled, for a consent and a login.
func TestBittensorWalletCancelledSessionTakesNoLateChallenge(t *testing.T) {
	result, issuedAtMillis := bittensorTestConsentChallenge(t)
	consent := newBittensorTestSession(t, BittensorWalletTalisman, BittensorWalletPlatformWindows, BittensorWalletPurposeConsent)
	consent.Cancel()
	if err := consent.SetNetworkConsentChallenge(result, issuedAtMillis); err == nil || !strings.HasPrefix(err.Error(), BittensorWalletErrorNotAwaiting+": ") {
		t.Fatalf("a cancelled consent session took a late challenge: %v", err)
	}
	if _, err := consent.BridgeUrl(); err == nil || consent.State() != BittensorWalletStateCancelled || consent.Message() != "" {
		t.Fatalf("a cancelled consent session opens the bridge (state %s)", consent.State())
	}
	login := newBittensorTestSession(t, BittensorWalletTalisman, BittensorWalletPlatformWindows, BittensorWalletPurposeLogin)
	login.Cancel()
	if err := login.SetChallenge(bittensorTestChallenge(), bittensorTestNowMillis); err == nil || login.State() != BittensorWalletStateCancelled || login.Message() != "" {
		t.Fatalf("a cancelled login session took a late challenge: %v (state %s)", err, login.State())
	}
}

// CONSENT §10: the session binds the coldkey its statement names: a result
// whose coldkey is another, or whose message is not a statement after the
// prefix, is refused.
func TestBittensorWalletSetNetworkConsentChallengeBindsTheStatementColdkey(t *testing.T) {
	result, issuedAtMillis := bittensorTestConsentChallenge(t)
	for name, challenge := range map[string]*SnNetworkConsentChallengeResult{
		"another coldkey than the statement's": {Message: result.Message, ColdkeySs58: bittensorTestBobSs58, ExpiresAt: result.ExpiresAt},
		"no statement after the prefix":        {Message: snNetworkConsentMessagePrefix + "pay this network's earnings to " + bittensorTestBobSs58, ColdkeySs58: result.ColdkeySs58, ExpiresAt: result.ExpiresAt},
	} {
		session := newBittensorTestSession(t, BittensorWalletTaoCom, BittensorWalletPlatformIos, BittensorWalletPurposeConsent)
		err := session.SetNetworkConsentChallenge(challenge, issuedAtMillis)
		if err == nil || !strings.HasPrefix(err.Error(), BittensorWalletErrorInvalidChallenge+": ") || session.Message() != "" {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// CONSENT §10: the expiry counts from receipt for the statement's lifetime, so
// a consent issued a moment ago signs on a device whose clock runs ten minutes
// ahead of the operator's, and a shorter lifetime is kept.
func TestBittensorWalletConsentExpiryCountsFromReceipt(t *testing.T) {
	for _, lifetime := range []int64{snNetworkConsentChallengeSeconds, 120} {
		server := newSnConsentServer(t)
		// the operator's clock is ten minutes behind the device's
		serverNow := time.Now().Unix() - 600
		server.challenge = func(args *SnNetworkWalletMappingChallengeArgs) (int, string) {
			return http.StatusOK, snConsentChallengeBody(snConsentTestMessage(t, args.ColdkeySs58, args.FromEpoch, args.ThroughEpoch, func(statement *snNetworkConsentStatement) {
				statement.IssuedAt, statement.ExpiresAt = serverNow, serverNow+lifetime
			}))
		}
		ctx, api, _ := newSnConsentOwnerApi(t, server)
		result, err := api.SnNetworkConsentChallengeSyncWithContext(ctx, bittensorTestAliceSs58)
		if err != nil {
			t.Fatal(err)
		}
		session := newBittensorTestSession(t, BittensorWalletTaoCom, BittensorWalletPlatformIos, BittensorWalletPurposeConsent)
		deviceNowMillis := time.Now().UnixMilli()
		if err := session.SetNetworkConsentChallenge(result, deviceNowMillis); err != nil {
			t.Fatalf("a consent issued a moment ago, on a device clock ten minutes ahead: %v", err)
		}
		if session.ExpiresAtMillis() != deviceNowMillis+lifetime*1000 {
			t.Fatalf("expiry %d, want receipt + %d s", session.ExpiresAtMillis(), lifetime)
		}
		if r := session.HandleSignature(bittensorTestAliceSs58, bittensorTestSignature, deviceNowMillis+lifetime*1000-1); !r.Ok() {
			t.Fatalf("signed within the %d s lifetime: %+v", lifetime, r)
		}
	}
}

// CONSENT §7: a consent session refuses the login challenge.
func TestBittensorWalletConsentSessionRefusesTheLoginChallenge(t *testing.T) {
	session := newBittensorTestSession(t, BittensorWalletTaoCom, BittensorWalletPlatformIos, BittensorWalletPurposeConsent)
	err := session.SetChallenge(bittensorTestChallenge(), bittensorTestNowMillis)
	if err == nil || !strings.HasPrefix(err.Error(), BittensorWalletErrorPurposeMismatch+": ") {
		t.Fatalf("login challenge on a consent session: %v", err)
	}
	if session.State() != BittensorWalletStateFailed || session.ErrorCode() != BittensorWalletErrorPurposeMismatch || session.Message() != "" {
		t.Fatalf("session %s, %s, %q", session.State(), session.ErrorCode(), session.Message())
	}
}

// CONSENT §7: no session of another purpose takes a network consent.
func TestBittensorWalletSetNetworkConsentChallengeRefusesTheWrongPurpose(t *testing.T) {
	result, issuedAtMillis := bittensorTestConsentChallenge(t)
	for _, purpose := range []string{BittensorWalletPurposeLogin, BittensorWalletPurposeCreate, BittensorWalletPurposeConnect, BittensorWalletPurposeAdd} {
		session := newBittensorTestSession(t, BittensorWalletTaoCom, BittensorWalletPlatformIos, purpose)
		err := session.SetNetworkConsentChallenge(result, issuedAtMillis)
		if err == nil || !strings.HasPrefix(err.Error(), BittensorWalletErrorPurposeMismatch+": ") {
			t.Fatalf("network consent on a %s session: %v", purpose, err)
		}
		if session.State() != BittensorWalletStateFailed || session.Message() != "" {
			t.Fatalf("%s session %s, %q", purpose, session.State(), session.Message())
		}
	}
}

// CONSENT §7: the message must be a network consent: none, or one without the
// exact first line, is refused.
func TestBittensorWalletSetNetworkConsentChallengeRefusesAMissingPrefix(t *testing.T) {
	result, issuedAtMillis := bittensorTestConsentChallenge(t)
	for _, challenge := range []*SnNetworkConsentChallengeResult{
		nil,
		{Message: bittensorTestMessage, ColdkeySs58: result.ColdkeySs58, ExpiresAt: result.ExpiresAt},
		{Message: strings.TrimPrefix(result.Message, snNetworkConsentMessagePrefix), ColdkeySs58: result.ColdkeySs58, ExpiresAt: result.ExpiresAt},
		{Message: strings.Replace(result.Message, "\n", "\r\n", 1), ColdkeySs58: result.ColdkeySs58, ExpiresAt: result.ExpiresAt},
	} {
		session := newBittensorTestSession(t, BittensorWalletTaoCom, BittensorWalletPlatformIos, BittensorWalletPurposeConsent)
		err := session.SetNetworkConsentChallenge(challenge, issuedAtMillis)
		if err == nil || !strings.HasPrefix(err.Error(), BittensorWalletErrorInvalidChallenge+": ") || session.ErrorCode() != BittensorWalletErrorInvalidChallenge {
			t.Fatalf("challenge %+v: %v", challenge, err)
		}
	}
}

// CONSENT §7: the signer is bound to a valid coldkey, so a consent without
// one is refused rather than left unbound.
func TestBittensorWalletSetNetworkConsentChallengeRefusesAnInvalidAddress(t *testing.T) {
	result, issuedAtMillis := bittensorTestConsentChallenge(t)
	for _, address := range []string{"", "not-an-address", bittensorTestAlicePolkadot} {
		session := newBittensorTestSession(t, BittensorWalletTaoCom, BittensorWalletPlatformIos, BittensorWalletPurposeConsent)
		challenge := *result
		challenge.ColdkeySs58 = address
		err := session.SetNetworkConsentChallenge(&challenge, issuedAtMillis)
		if err == nil || !strings.HasPrefix(err.Error(), BittensorWalletErrorInvalidAddress+": ") || session.ErrorCode() != BittensorWalletErrorInvalidAddress {
			t.Fatalf("coldkey %q: %v", address, err)
		}
	}
}

// CONSENT §7: a signature from another account than the consent's coldkey is
// refused.
func TestBittensorWalletConsentSessionRefusesAnAddressMismatch(t *testing.T) {
	result, issuedAtMillis := bittensorTestConsentChallenge(t)
	session := newBittensorTestSession(t, BittensorWalletTaoCom, BittensorWalletPlatformIos, BittensorWalletPurposeConsent)
	if err := session.SetNetworkConsentChallenge(result, issuedAtMillis); err != nil {
		t.Fatal(err)
	}
	if r := session.HandleSignature(bittensorTestAliceSs58, bittensorTestSignature, issuedAtMillis); r.Ok() || r.ErrorCode != BittensorWalletErrorAddressMismatch {
		t.Fatalf("another account: %+v", r)
	}
}

// CONSENT §7, §10: the expiry is the statement's lifetime after receipt, on the
// device's own clock however far it runs from the operator's: a signature at
// it is refused with challenge_expired, one a millisecond before is taken.
func TestBittensorWalletConsentSessionRefusesASignatureAfterTheLifetime(t *testing.T) {
	result, _ := bittensorTestConsentChallenge(t)
	// a device clock a day past the statement's own expiry
	receivedMillis := result.ExpiresAt*1000 + 24*3600*1000
	for _, c := range []struct {
		signedMillis int64
		ok           bool
	}{
		{receivedMillis + snNetworkConsentChallengeSeconds*1000 - 1, true},
		{receivedMillis + snNetworkConsentChallengeSeconds*1000, false},
	} {
		session := newBittensorTestSession(t, BittensorWalletTaoCom, BittensorWalletPlatformIos, BittensorWalletPurposeConsent)
		if err := session.SetNetworkConsentChallenge(result, receivedMillis); err != nil {
			t.Fatalf("consent received on a clock past its expires_at: %v", err)
		}
		r := session.HandleSignature(result.ColdkeySs58, bittensorTestSignature, c.signedMillis)
		if r.Ok() != c.ok || !c.ok && (r.ErrorCode != SnErrorCodeChallengeExpired || session.State() != BittensorWalletStateFailed) {
			t.Fatalf("signature %d ms after receipt: %+v", c.signedMillis-receivedMillis, r)
		}
	}
}
