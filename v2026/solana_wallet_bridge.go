package sdk

// The ur.io Solana wallet bridge (mmm/ur.io react WalletConnect.jsx, at
// https://ur.io/wallet-connect). The desktop apps (macOS, Windows, Linux) open
// it in the system browser, where it drives the Phantom or Solflare extension
// and hands control back on the app's urnetwork://<provider>-connect or
// urnetwork://<provider>-sign-message link, in the envelope of the wallets'
// own deep links (the NaCl box of GenerateSharedSecret, EncryptData and
// DecryptData). The apps read the hand-back themselves.
//
// A failure comes back as ?errorCode=<code>&errorMessage=<the page's English
// text>[&purpose=...], with one of SolanaWalletBridgeError* as the code. An
// app shows its own words for a code it knows and errorMessage for any other,
// as it does for the -1 of pages before these codes. The wallet apps' own deep
// links (Phantom and Solflare on iOS) send the wallets' numeric codes instead,
// 4001 for a declined request.

// The bridge page's codes for a failure it hands back.
const (
	// the page was opened without the parameters it needs, or with a sign
	// request it cannot read
	SolanaWalletBridgeErrorInvalidRequest = "invalid_request"
	// the wallet's extension (Phantom or Solflare) is not in the browser
	SolanaWalletBridgeErrorExtensionNotFound = "extension_not_found"
	// the wallet connected without sharing an account
	SolanaWalletBridgeErrorNoAccount = "no_account"
	// the sign step found no key from the connect step in the browser (another
	// browser, or its storage was cleared), so the wallet must connect again
	SolanaWalletBridgeErrorSessionNotFound = "session_not_found"
	// the user declined the connection or the signature in the wallet
	SolanaWalletBridgeErrorUserRejected = "user_rejected"
	// any other failure; the page's text says what
	SolanaWalletBridgeErrorWallet = "wallet_error"
)
