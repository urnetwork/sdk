package sdk

import (
	"testing"
)

// The Solana bridge page's failure codes are a contract with ur.io's
// WalletConnect.jsx, which sends them, and the desktop apps, which show their
// own words for the ones they know.
func TestSolanaWalletBridgeErrorCodes(t *testing.T) {
	for code, want := range map[string]string{
		SolanaWalletBridgeErrorInvalidRequest:    "invalid_request",
		SolanaWalletBridgeErrorExtensionNotFound: "extension_not_found",
		SolanaWalletBridgeErrorNoAccount:         "no_account",
		SolanaWalletBridgeErrorSessionNotFound:   "session_not_found",
		SolanaWalletBridgeErrorUserRejected:      "user_rejected",
		SolanaWalletBridgeErrorWallet:            "wallet_error",
	} {
		if code != want {
			t.Errorf("solana wallet bridge error code %q, want %q", code, want)
		}
	}
	// the codes both wallet bridge pages send for the same failure are the same
	for solanaCode, bittensorCode := range map[string]string{
		SolanaWalletBridgeErrorInvalidRequest:    BittensorWalletBridgeErrorInvalidRequest,
		SolanaWalletBridgeErrorExtensionNotFound: BittensorWalletBridgeErrorExtensionNotFound,
		SolanaWalletBridgeErrorNoAccount:         BittensorWalletBridgeErrorNoAccount,
		SolanaWalletBridgeErrorUserRejected:      BittensorWalletBridgeErrorUserRejected,
		SolanaWalletBridgeErrorWallet:            BittensorWalletBridgeErrorWallet,
	} {
		if solanaCode != bittensorCode {
			t.Errorf("solana wallet bridge error code %q, bittensor %q", solanaCode, bittensorCode)
		}
	}
}
