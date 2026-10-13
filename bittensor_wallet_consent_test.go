// The network payout consent through the wallet session (CONSENT.md §7): only
// a consent session takes it, the message stays byte for byte, the signer is
// bound to the consent's coldkey, and the expiry is the server's own.
package sdk

import (
	"net/url"
	"strings"
	"testing"
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

// CONSENT §7: the bridge page gets the consent message and coldkey unchanged,
// and its echo of the exact message completes the session.
func TestBittensorWalletConsentBridgeRoundTrip(t *testing.T) {
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
	q := u.Query()
	if q.Get("message") != result.Message || q.Get("address") != result.ColdkeySs58 || q.Get("purpose") != BittensorWalletPurposeConsent {
		t.Fatalf("bridge url %s", bridgeUrl)
	}
	returnUri := func(message string) string {
		return bittensorTestRedirectLink + "?" + url.Values{
			"address":   {result.ColdkeySs58},
			"signature": {bittensorTestSignature},
			"message":   {message},
			"purpose":   {BittensorWalletPurposeConsent},
			"wallet":    {BittensorWalletTalisman},
		}.Encode()
	}
	// another message than the one issued is refused
	if r := session.HandleBridgeReturn(returnUri(result.Message+" "), issuedAtMillis); r.ErrorCode != BittensorWalletErrorMessageMismatch {
		t.Fatalf("changed message: %+v", r)
	}
	session = newBittensorTestSession(t, BittensorWalletTalisman, BittensorWalletPlatformWindows, BittensorWalletPurposeConsent)
	if err := session.SetNetworkConsentChallenge(result, issuedAtMillis); err != nil {
		t.Fatal(err)
	}
	if _, err := session.BridgeUrl(); err != nil {
		t.Fatal(err)
	}
	r := session.HandleBridgeReturn(returnUri(result.Message), issuedAtMillis)
	if !r.Ok() || r.Proof.Message != result.Message || r.Proof.Address != result.ColdkeySs58 || r.Proof.Purpose != BittensorWalletPurposeConsent {
		t.Fatalf("return: %+v", r)
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

// CONSENT §7: the expiry is the server's: a consent at or past it is refused
// with challenge_expired, and a signature after it too.
func TestBittensorWalletConsentSessionRefusesAnExpiredChallenge(t *testing.T) {
	result, issuedAtMillis := bittensorTestConsentChallenge(t)
	session := newBittensorTestSession(t, BittensorWalletTaoCom, BittensorWalletPlatformIos, BittensorWalletPurposeConsent)
	err := session.SetNetworkConsentChallenge(result, result.ExpiresAt*1000)
	if err == nil || !strings.HasPrefix(err.Error(), SnErrorCodeChallengeExpired+": ") || session.ErrorCode() != BittensorWalletErrorExpired || session.Message() != "" {
		t.Fatalf("expired consent: %v", err)
	}

	session = newBittensorTestSession(t, BittensorWalletTaoCom, BittensorWalletPlatformIos, BittensorWalletPurposeConsent)
	if err := session.SetNetworkConsentChallenge(result, result.ExpiresAt*1000-1); err != nil {
		t.Fatalf("consent a millisecond before its expiry: %v", err)
	}
	if r := session.HandleSignature(result.ColdkeySs58, bittensorTestSignature, result.ExpiresAt*1000); r.Ok() || r.ErrorCode != BittensorWalletErrorExpired {
		t.Fatalf("signature at the expiry: %+v", r)
	}
	if issuedAtMillis >= result.ExpiresAt*1000 {
		t.Fatal("the fixture's consent is issued after its expiry")
	}
}
