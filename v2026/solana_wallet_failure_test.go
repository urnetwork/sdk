package sdk

import (
	"encoding/json"
	"os"
	"testing"
)

// The Solana wallet sign-in failures replay js/test/solana_wallet_failure_vectors.json,
// the vectors js/test/solana_wallet_failure.test.ts replays against the
// TypeScript twin, so every app reads a failure the same way.
func TestSolanaWalletFailureVectors(t *testing.T) {
	data, err := os.ReadFile("js/test/solana_wallet_failure_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	vectors := struct {
		Cases []struct {
			Source            string `json:"source"`
			Code              string `json:"code"`
			WalletName        string `json:"wallet_name"`
			Failure           string `json:"failure"`
			MessageKey        string `json:"message_key"`
			MessageWalletName string `json:"message_wallet_name"`
		} `json:"cases"`
	}{}
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors.Cases) < 20 {
		t.Fatalf("only %d vectors", len(vectors.Cases))
	}
	for _, c := range vectors.Cases {
		got := SolanaWalletFailureFor(c.Source, c.Code, c.WalletName)
		want := SolanaWalletFailure{Failure: c.Failure, MessageKey: c.MessageKey, WalletName: c.MessageWalletName}
		if *got != want {
			t.Errorf("SolanaWalletFailureFor(%q, %q, %q) = %+v, want %+v", c.Source, c.Code, c.WalletName, *got, want)
		}
	}
}

// Every failure the bridge page names has its own message, the words the apps
// had for those codes. Before, only Windows and Linux showed them: Apple's
// sign-in showed a generic error, Android a fixed English line and ur.io the
// wallet's own English.
func TestSolanaWalletFailureBridgeCodesHaveTheirOwnMessages(t *testing.T) {
	for code, want := range map[string]string{
		SolanaWalletBridgeErrorExtensionNotFound: SolanaWalletFailureMessageExtensionNotFound,
		SolanaWalletBridgeErrorNoAccount:         SolanaWalletFailureMessageNoAccount,
		SolanaWalletBridgeErrorSessionNotFound:   SolanaWalletFailureMessageSessionNotFound,
		SolanaWalletBridgeErrorUserRejected:      SolanaWalletFailureMessageUserRejected,
		SolanaWalletBridgeErrorInvalidRequest:    SolanaWalletFailureMessageWallet,
		SolanaWalletBridgeErrorWallet:            SolanaWalletFailureMessageWallet,
	} {
		if got := SolanaWalletFailureFor(SolanaWalletFailureSourceBridge, code, "phantom").MessageKey; got != want {
			t.Errorf("bridge code %q shows %q, want %q", code, got, want)
		}
	}
}

// A message that takes the wallet's name always gets one, and a message that
// takes none never does, whatever the app passed: an app formats the key with
// WalletName exactly when it is set.
func TestSolanaWalletFailureMessagesTakeTheWalletNameExactlyWhenTheyNeedIt(t *testing.T) {
	takesName := map[string]bool{
		SolanaWalletFailureMessageUserRejected:      false,
		SolanaWalletFailureMessageNoWallet:          false,
		SolanaWalletFailureMessageWalletAppNotFound: false,
		SolanaWalletFailureMessageExtensionNotFound: true,
		SolanaWalletFailureMessageNoAccount:         true,
		SolanaWalletFailureMessageSessionNotFound:   false,
		SolanaWalletFailureMessageWallet:            true,
	}
	sources := []string{
		SolanaWalletFailureSourceBridge,
		SolanaWalletFailureSourceWalletApp,
		SolanaWalletFailureSourceMobileWalletAdapter,
		SolanaWalletFailureSourceBrowser,
		SolanaWalletFailureSourceApp,
		"",
	}
	codes := []string{
		SolanaWalletFailureUserRejected,
		SolanaWalletFailureNoWallet,
		SolanaWalletFailureWalletAppNotFound,
		SolanaWalletFailureExtensionNotFound,
		SolanaWalletFailureNoAccount,
		SolanaWalletFailureSessionNotFound,
		SolanaWalletFailureWallet,
		SolanaWalletBridgeErrorInvalidRequest,
		solanaWalletCodeUserRejected,
		solanaWalletCodeWalletConnectUserRejected,
		solanaWalletCodeMwaAuthorizationFailed,
		solanaWalletCodeMwaNotSigned,
		"-32603",
		"",
	}
	seen := map[string]bool{}
	for _, source := range sources {
		for _, code := range codes {
			for _, walletName := range []string{"", "phantom", "Solflare", "Backpack"} {
				failure := SolanaWalletFailureFor(source, code, walletName)
				needsName, known := takesName[failure.MessageKey]
				if !known {
					t.Fatalf("(%q, %q, %q) shows %q, which is not one of the messages", source, code, walletName, failure.MessageKey)
				}
				if needsName != (failure.WalletName != "") {
					t.Errorf("(%q, %q, %q) shows %q with wallet name %q", source, code, walletName, failure.MessageKey, failure.WalletName)
				}
				seen[failure.MessageKey] = true
			}
		}
	}
	// every message is reachable, so every app's display covers a key it can get
	for key := range takesName {
		if !seen[key] {
			t.Errorf("no failure shows %q", key)
		}
	}
}
