// The network payout consent's wallet session through the real c abi
// (CONSENT.md §7, §9): a desktop app hands the challenge over as json with the
// sdk's snake_case names, the session keeps the message byte for byte, and a
// refusal reaches out_error in the sdk's "code: message" form.
package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/urnetwork/sdk"
)

func TestNetworkConsentChallengeJsonThroughTheAbi(t *testing.T) {
	const message = "Approve URnetwork network wallet mapping\n{\"synthetic\":\"statement\"}"
	const expiresAt = int64(1791245100)
	resultJson, err := json.Marshal(map[string]any{
		"message":       message,
		"coldkey_ss58":  "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY",
		"from_epoch":    43,
		"through_epoch": 43 + sdk.SnNetworkConsentEpochSpan,
		"expires_at":    expiresAt,
	})
	if err != nil {
		t.Fatal(err)
	}
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
		ok := socketABICall(urnet_bittensor_wallet_session_set_network_consent_challenge, session, resultC, (expiresAt-300)*1000, outError)[0].Bool()
		return ok, socketABIError(outError)
	}

	consent := newSession(sdk.BittensorWalletPurposeConsent)
	if ok, failure := setChallenge(consent); !ok || failure != "" {
		t.Fatalf("consent session refused the challenge: %q", failure)
	}
	messagePointer := socketABICall(urnet_bittensor_wallet_session_message, consent)[0]
	got := goStringAt(messagePointer.UnsafePointer())
	socketABICall(urnet_free_string, messagePointer)
	if got != message {
		t.Fatalf("session message = %q, want the challenge's bytes %q", got, message)
	}
	if millis := socketABICall(urnet_bittensor_wallet_session_expires_at_millis, consent)[0].Int(); millis != expiresAt*1000 {
		t.Fatalf("session expiry = %d, want %d", millis, expiresAt*1000)
	}

	login := newSession(sdk.BittensorWalletPurposeLogin)
	if ok, failure := setChallenge(login); ok || !strings.HasPrefix(failure, sdk.BittensorWalletErrorPurposeMismatch+": ") {
		t.Fatalf("login session took the network consent: %t, %q", ok, failure)
	}
}
