// The network payout consent's wallet session through the real c abi
// (CONSENT.md §7, §9, §10): a desktop app hands the challenge over as json
// with the sdk's snake_case names, the session keeps the message byte for
// byte with its lifetime from receipt, and a refusal reaches out_error in the
// sdk's "code: message" form.
package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/urnetwork/sdk/v2026"
)

func TestNetworkConsentChallengeJsonThroughTheAbi(t *testing.T) {
	// the sn protocol's own challenge (sdk/sn/protocol/testdata)
	raw, err := os.ReadFile("../sn/protocol/testdata/network-consent-challenge-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	fixture := struct {
		ColdkeySs58  string `json:"coldkey_ss58"`
		FromEpoch    int64  `json:"from_epoch"`
		ThroughEpoch int64  `json:"through_epoch"`
		ExpiresAt    int64  `json:"expires_at"`
		Message      string `json:"message"`
	}{}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	resultJson, err := json.Marshal(map[string]any{
		"message":       fixture.Message,
		"coldkey_ss58":  fixture.ColdkeySs58,
		"from_epoch":    fixture.FromEpoch,
		"through_epoch": fixture.ThroughEpoch,
		"expires_at":    fixture.ExpiresAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	const receivedMillis = int64(1_800_000_000_000)
	newSession := func(purpose string) uint64 {
		t.Helper()
		walletId := cString(sdk.BittensorWalletTaoCom)
		defer cStringFree(walletId)
		platform := cString(sdk.BittensorWalletPlatformIos)
		defer cStringFree(platform)
		purposeC := cString(purpose)
		defer cStringFree(purposeC)
		redirectLink := cString("urnetwork://bittensor-sign-message")
		defer cStringFree(redirectLink)
		outError := socketABIOut(urnet_new_bittensor_wallet_session, 4)
		handle := socketABICall(urnet_new_bittensor_wallet_session, walletId, platform, purposeC, redirectLink, outError)[0].Uint()
		if failure := socketABIError(outError); failure != "" || handle == 0 {
			t.Fatalf("new %s session: %q", purpose, failure)
		}
		t.Cleanup(func() { handleRelease(handle) })
		return handle
	}
	setChallenge := func(session uint64) (bool, string) {
		t.Helper()
		resultC := cString(string(resultJson))
		defer cStringFree(resultC)
		outError := socketABIOut(urnet_bittensor_wallet_session_set_network_consent_challenge, 3)
		ok := socketABICall(urnet_bittensor_wallet_session_set_network_consent_challenge, session, resultC, receivedMillis, outError)[0].Bool()
		return ok, socketABIError(outError)
	}

	consent := newSession(sdk.BittensorWalletPurposeConsent)
	if ok, failure := setChallenge(consent); !ok || failure != "" {
		t.Fatalf("consent session refused the challenge: %q", failure)
	}
	messagePointer := socketABICall(urnet_bittensor_wallet_session_message, consent)[0]
	got := goStringAt(messagePointer.UnsafePointer())
	socketABICall(urnet_free_string, messagePointer)
	if got != fixture.Message {
		t.Fatalf("session message = %q, want the challenge's bytes %q", got, fixture.Message)
	}
	if millis := socketABICall(urnet_bittensor_wallet_session_expires_at_millis, consent)[0].Int(); millis != receivedMillis+300_000 {
		t.Fatalf("session expiry = %d, want receipt + 300 s = %d", millis, receivedMillis+300_000)
	}

	login := newSession(sdk.BittensorWalletPurposeLogin)
	if ok, failure := setChallenge(login); ok || !strings.HasPrefix(failure, sdk.BittensorWalletErrorPurposeMismatch+": ") {
		t.Fatalf("login session took the network consent: %t, %q", ok, failure)
	}
}
