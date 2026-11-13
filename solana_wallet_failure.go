// Why a Solana wallet sign-in failed, read the same way on every app.
//
// Signing in with a Solana wallet, or adding one as a sign-in method, reaches
// the wallet over a different transport on each platform: the ur.io wallet
// bridge on macOS, Windows and Linux (solana_wallet_bridge.go), Phantom's or
// Solflare's own deep links on iOS, Mobile Wallet Adapter on Android, and a
// browser wallet (an injected Phantom or Solflare, or WalletConnect) on ur.io.
// Each hands a failure back with codes of its own. SolanaWalletFailureFor reads
// a code in its transport's scheme and answers one of the SolanaWalletFailure*
// failures, with the localizations store key of the message every app shows
// for it and the wallet name that message takes. So the same failure reads the
// same on every app, and an app only displays the key.
//
// js/src/utils/solana_wallet_failure.ts is the TypeScript twin ur.io imports,
// and js/test/solana_wallet_failure_vectors.json holds the vectors both test
// suites replay, so the two cannot drift apart.
package sdk

import (
	"strings"
)

// Where a failure's code comes from: the source argument of
// SolanaWalletFailureFor, which reads the code in that scheme.
const (
	// the ur.io wallet bridge's hand-back: one of SolanaWalletBridgeError*, or
	// the -1 of pages before the codes
	SolanaWalletFailureSourceBridge = "bridge"
	// Phantom's or Solflare's own deep link hand-back on iOS: the wallet's
	// numeric error code
	SolanaWalletFailureSourceWalletApp = "wallet_app"
	// Mobile Wallet Adapter on Android: the wallet's JSON-RPC error code
	SolanaWalletFailureSourceMobileWalletAdapter = "mobile_wallet_adapter"
	// a browser wallet on ur.io: an injected provider's error code, or a
	// WalletConnect error code
	SolanaWalletFailureSourceBrowser = "browser"
	// a failure the app found itself, named by one of SolanaWalletFailure*
	SolanaWalletFailureSourceApp = "app"
)

// The failures, in SolanaWalletFailure.Failure.
const (
	// the user declined the connection or the signature in the wallet
	SolanaWalletFailureUserRejected = "user_rejected"
	// no Solana wallet answered on this device or in this browser
	SolanaWalletFailureNoWallet = "no_wallet"
	// the chosen wallet app is not installed, or would not open
	SolanaWalletFailureWalletAppNotFound = "wallet_app_not_found"
	// the chosen wallet's browser extension is not in the browser
	SolanaWalletFailureExtensionNotFound = "extension_not_found"
	// the wallet connected without an account it can sign with
	SolanaWalletFailureNoAccount = "no_account"
	// the sign step found no connection from the connect step in the browser
	// (another browser, or its storage was cleared), so the wallet must
	// connect again
	SolanaWalletFailureSessionNotFound = "session_not_found"
	// any other failure, including a code this build does not know
	SolanaWalletFailureWallet = "wallet_error"
)

// The messages, in SolanaWalletFailure.MessageKey: localizations store keys,
// tagged for every app. The keys named bittensor_* are the wallet bridges'
// shared words, which read the same for a Solana wallet.
const (
	// takes no argument
	SolanaWalletFailureMessageUserRejected = "bittensor_error_user_rejected"
	// takes no argument
	SolanaWalletFailureMessageNoWallet = "no_wallets_found_alert_content"
	// takes no argument
	SolanaWalletFailureMessageWalletAppNotFound = "couldnt_open_wallet"
	// takes the wallet name
	SolanaWalletFailureMessageExtensionNotFound = "bittensor_error_extension_not_found"
	// takes the wallet name
	SolanaWalletFailureMessageNoAccount = "bittensor_error_no_account"
	// takes no argument
	SolanaWalletFailureMessageSessionNotFound = "solana_wallet_error_session_not_found"
	// takes the wallet name
	SolanaWalletFailureMessageWallet = "bittensor_error_wallet_failed"
)

// The wallets' own codes this reads.
const (
	// a declined request: Phantom's and Solflare's code in their deep links
	// and their injected providers (EIP-1193's 4001)
	solanaWalletCodeUserRejected = "4001"
	// WalletConnect's USER_REJECTED: the user rejected the session or the
	// request (5001 to 5003 are a wallet refusing a chain, method or event,
	// which is not the user's choice)
	solanaWalletCodeWalletConnectUserRejected = "5000"
	// Mobile Wallet Adapter's ERROR_AUTHORIZATION_FAILED: the user declined to
	// connect the wallet
	solanaWalletCodeMwaAuthorizationFailed = "-1"
	// Mobile Wallet Adapter's ERROR_NOT_SIGNED: the user declined to sign
	solanaWalletCodeMwaNotSigned = "-3"
	// the wallet a message names when the app does not know which wallet
	// answered (Mobile Wallet Adapter does not say)
	solanaWalletGenericName = "Solana"
)

// What an app shows for a failed Solana wallet sign-in. Crosses the c abi as
// json.
type SolanaWalletFailure struct {
	// one of SolanaWalletFailure*
	Failure string `json:"failure"`
	// one of SolanaWalletFailureMessage*, the store key of the message to show
	MessageKey string `json:"message_key"`
	// the message's argument, a wallet product name; "" when the message
	// takes none
	WalletName string `json:"wallet_name,omitempty"`
}

// The failure for `code` read in the scheme of `source` (one of
// SolanaWalletFailureSource*), and the message to show for it. `walletName`
// is the chosen wallet, by id (phantom, solflare) or by name, or "" when the
// app does not know which wallet answered.
//
// A code this build does not know, from any source, and any code from an
// unknown source, is wallet_error: its message names the wallet and says to
// check it, never the wallet's own English. A message that names the wallet
// names "Solana" when the wallet is unknown, except an extension that is not
// found, which then reads as no wallet at all.
func SolanaWalletFailureFor(source string, code string, walletName string) *SolanaWalletFailure {
	code = strings.TrimSpace(code)
	failure := func() string {
		switch source {
		case SolanaWalletFailureSourceBridge:
			switch code {
			case SolanaWalletBridgeErrorUserRejected:
				return SolanaWalletFailureUserRejected
			case SolanaWalletBridgeErrorExtensionNotFound:
				return SolanaWalletFailureExtensionNotFound
			case SolanaWalletBridgeErrorNoAccount:
				return SolanaWalletFailureNoAccount
			case SolanaWalletBridgeErrorSessionNotFound:
				return SolanaWalletFailureSessionNotFound
			}
		case SolanaWalletFailureSourceWalletApp:
			if code == solanaWalletCodeUserRejected {
				return SolanaWalletFailureUserRejected
			}
		case SolanaWalletFailureSourceBrowser:
			switch code {
			case solanaWalletCodeUserRejected, solanaWalletCodeWalletConnectUserRejected:
				return SolanaWalletFailureUserRejected
			}
		case SolanaWalletFailureSourceMobileWalletAdapter:
			switch code {
			case solanaWalletCodeMwaAuthorizationFailed, solanaWalletCodeMwaNotSigned:
				return SolanaWalletFailureUserRejected
			}
		case SolanaWalletFailureSourceApp:
			switch code {
			case SolanaWalletFailureUserRejected,
				SolanaWalletFailureNoWallet,
				SolanaWalletFailureWalletAppNotFound,
				SolanaWalletFailureExtensionNotFound,
				SolanaWalletFailureNoAccount,
				SolanaWalletFailureSessionNotFound,
				SolanaWalletFailureWallet:
				return code
			}
		}
		return SolanaWalletFailureWallet
	}()

	// Phantom and Solflare by id or by name; any other name as given
	name := strings.TrimSpace(walletName)
	switch strings.ToLower(name) {
	case "phantom":
		name = "Phantom"
	case "solflare":
		name = "Solflare"
	}
	nameOrGeneric := name
	if nameOrGeneric == "" {
		nameOrGeneric = solanaWalletGenericName
	}

	switch failure {
	case SolanaWalletFailureUserRejected:
		return &SolanaWalletFailure{Failure: failure, MessageKey: SolanaWalletFailureMessageUserRejected}
	case SolanaWalletFailureNoWallet:
		return &SolanaWalletFailure{Failure: failure, MessageKey: SolanaWalletFailureMessageNoWallet}
	case SolanaWalletFailureWalletAppNotFound:
		return &SolanaWalletFailure{Failure: failure, MessageKey: SolanaWalletFailureMessageWalletAppNotFound}
	case SolanaWalletFailureExtensionNotFound:
		// "the Solana extension" would name a product that does not exist
		if name == "" {
			return &SolanaWalletFailure{Failure: failure, MessageKey: SolanaWalletFailureMessageNoWallet}
		}
		return &SolanaWalletFailure{Failure: failure, MessageKey: SolanaWalletFailureMessageExtensionNotFound, WalletName: name}
	case SolanaWalletFailureNoAccount:
		return &SolanaWalletFailure{Failure: failure, MessageKey: SolanaWalletFailureMessageNoAccount, WalletName: nameOrGeneric}
	case SolanaWalletFailureSessionNotFound:
		return &SolanaWalletFailure{Failure: failure, MessageKey: SolanaWalletFailureMessageSessionNotFound}
	default:
		return &SolanaWalletFailure{Failure: SolanaWalletFailureWallet, MessageKey: SolanaWalletFailureMessageWallet, WalletName: nameOrGeneric}
	}
}
