/**
 * Why a Solana wallet sign-in failed, read the same way on every app.
 *
 * This is the TypeScript twin of `sdk/solana_wallet_failure.go`: the function
 * has the Go name in camelCase, and the constants the Go names in
 * UPPER_SNAKE_CASE, as this package names its constants. The Go version is
 * what the apps bind; this is what ur.io imports. The two must stay in step,
 * and test/solana_wallet_failure_vectors.json holds the vectors both test
 * suites replay.
 *
 * Signing in with a Solana wallet, or adding one as a sign-in method, reaches
 * the wallet over a different transport on each platform (the ur.io wallet
 * bridge on macOS, Windows and Linux, the wallets' deep links on iOS, Mobile
 * Wallet Adapter on Android, a browser wallet on ur.io), and each hands a
 * failure back with codes of its own. solanaWalletFailureFor reads a code in
 * its transport's scheme and answers the failure, with the localizations store
 * key of the message every app shows for it and the wallet name that message
 * takes. An app only displays the key.
 */

/** Where a failure's code comes from: the bridge page's hand-back. */
export const SOLANA_WALLET_FAILURE_SOURCE_BRIDGE = "bridge";
/** Phantom's or Solflare's own deep link hand-back on iOS. */
export const SOLANA_WALLET_FAILURE_SOURCE_WALLET_APP = "wallet_app";
/** Mobile Wallet Adapter on Android: the wallet's JSON-RPC error code. */
export const SOLANA_WALLET_FAILURE_SOURCE_MOBILE_WALLET_ADAPTER = "mobile_wallet_adapter";
/** A browser wallet on ur.io: an injected provider's or WalletConnect's error code. */
export const SOLANA_WALLET_FAILURE_SOURCE_BROWSER = "browser";
/** A failure the app found itself, named by one of SOLANA_WALLET_FAILURE_*. */
export const SOLANA_WALLET_FAILURE_SOURCE_APP = "app";

/** The user declined the connection or the signature in the wallet. */
export const SOLANA_WALLET_FAILURE_USER_REJECTED = "user_rejected";
/** No Solana wallet answered on this device or in this browser. */
export const SOLANA_WALLET_FAILURE_NO_WALLET = "no_wallet";
/** The chosen wallet app is not installed, or would not open. */
export const SOLANA_WALLET_FAILURE_WALLET_APP_NOT_FOUND = "wallet_app_not_found";
/** The chosen wallet's browser extension is not in the browser. */
export const SOLANA_WALLET_FAILURE_EXTENSION_NOT_FOUND = "extension_not_found";
/** The wallet connected without an account it can sign with. */
export const SOLANA_WALLET_FAILURE_NO_ACCOUNT = "no_account";
/** The sign step found no connection from the connect step in the browser. */
export const SOLANA_WALLET_FAILURE_SESSION_NOT_FOUND = "session_not_found";
/** Any other failure, including a code this build does not know. */
export const SOLANA_WALLET_FAILURE_WALLET = "wallet_error";

/** The messages: localizations store keys. Takes no argument. */
export const SOLANA_WALLET_FAILURE_MESSAGE_USER_REJECTED = "bittensor_error_user_rejected";
/** Takes no argument. */
export const SOLANA_WALLET_FAILURE_MESSAGE_NO_WALLET = "no_wallets_found_alert_content";
/** Takes no argument. */
export const SOLANA_WALLET_FAILURE_MESSAGE_WALLET_APP_NOT_FOUND = "couldnt_open_wallet";
/** Takes the wallet name. */
export const SOLANA_WALLET_FAILURE_MESSAGE_EXTENSION_NOT_FOUND = "bittensor_error_extension_not_found";
/** Takes the wallet name. */
export const SOLANA_WALLET_FAILURE_MESSAGE_NO_ACCOUNT = "bittensor_error_no_account";
/** Takes no argument. */
export const SOLANA_WALLET_FAILURE_MESSAGE_SESSION_NOT_FOUND = "solana_wallet_error_session_not_found";
/** Takes the wallet name. */
export const SOLANA_WALLET_FAILURE_MESSAGE_WALLET = "bittensor_error_wallet_failed";

/** Every message key, for a display that must cover them all. */
export const SOLANA_WALLET_FAILURE_MESSAGE_KEYS = [
  SOLANA_WALLET_FAILURE_MESSAGE_USER_REJECTED,
  SOLANA_WALLET_FAILURE_MESSAGE_NO_WALLET,
  SOLANA_WALLET_FAILURE_MESSAGE_WALLET_APP_NOT_FOUND,
  SOLANA_WALLET_FAILURE_MESSAGE_EXTENSION_NOT_FOUND,
  SOLANA_WALLET_FAILURE_MESSAGE_NO_ACCOUNT,
  SOLANA_WALLET_FAILURE_MESSAGE_SESSION_NOT_FOUND,
  SOLANA_WALLET_FAILURE_MESSAGE_WALLET,
] as const;

// the bridge page's codes (sdk solana_wallet_bridge.go)
const BRIDGE_USER_REJECTED = "user_rejected";
const BRIDGE_EXTENSION_NOT_FOUND = "extension_not_found";
const BRIDGE_NO_ACCOUNT = "no_account";
const BRIDGE_SESSION_NOT_FOUND = "session_not_found";

// a declined request: Phantom's and Solflare's code (EIP-1193's 4001)
const CODE_USER_REJECTED = "4001";
// WalletConnect's USER_REJECTED
const CODE_WALLET_CONNECT_USER_REJECTED = "5000";
// Mobile Wallet Adapter's ERROR_AUTHORIZATION_FAILED and ERROR_NOT_SIGNED
const CODE_MWA_AUTHORIZATION_FAILED = "-1";
const CODE_MWA_NOT_SIGNED = "-3";
// the wallet a message names when the app does not know which wallet answered
const GENERIC_WALLET_NAME = "Solana";

const APP_FAILURES = new Set([
  SOLANA_WALLET_FAILURE_USER_REJECTED,
  SOLANA_WALLET_FAILURE_NO_WALLET,
  SOLANA_WALLET_FAILURE_WALLET_APP_NOT_FOUND,
  SOLANA_WALLET_FAILURE_EXTENSION_NOT_FOUND,
  SOLANA_WALLET_FAILURE_NO_ACCOUNT,
  SOLANA_WALLET_FAILURE_SESSION_NOT_FOUND,
  SOLANA_WALLET_FAILURE_WALLET,
]);

/** What an app shows for a failed Solana wallet sign-in. */
export interface SolanaWalletFailure {
  /** one of SOLANA_WALLET_FAILURE_* */
  failure: string;
  /** one of SOLANA_WALLET_FAILURE_MESSAGE_*, the store key of the message to show */
  messageKey: string;
  /** the message's argument, a wallet product name; "" when the message takes none */
  walletName: string;
}

function failureOf(source: string, code: string): string {
  switch (source) {
    case SOLANA_WALLET_FAILURE_SOURCE_BRIDGE:
      switch (code) {
        case BRIDGE_USER_REJECTED:
          return SOLANA_WALLET_FAILURE_USER_REJECTED;
        case BRIDGE_EXTENSION_NOT_FOUND:
          return SOLANA_WALLET_FAILURE_EXTENSION_NOT_FOUND;
        case BRIDGE_NO_ACCOUNT:
          return SOLANA_WALLET_FAILURE_NO_ACCOUNT;
        case BRIDGE_SESSION_NOT_FOUND:
          return SOLANA_WALLET_FAILURE_SESSION_NOT_FOUND;
      }
      break;
    case SOLANA_WALLET_FAILURE_SOURCE_WALLET_APP:
      if (code === CODE_USER_REJECTED) return SOLANA_WALLET_FAILURE_USER_REJECTED;
      break;
    case SOLANA_WALLET_FAILURE_SOURCE_BROWSER:
      if (code === CODE_USER_REJECTED || code === CODE_WALLET_CONNECT_USER_REJECTED) return SOLANA_WALLET_FAILURE_USER_REJECTED;
      break;
    case SOLANA_WALLET_FAILURE_SOURCE_MOBILE_WALLET_ADAPTER:
      if (code === CODE_MWA_AUTHORIZATION_FAILED || code === CODE_MWA_NOT_SIGNED) return SOLANA_WALLET_FAILURE_USER_REJECTED;
      break;
    case SOLANA_WALLET_FAILURE_SOURCE_APP:
      if (APP_FAILURES.has(code)) return code;
      break;
  }
  return SOLANA_WALLET_FAILURE_WALLET;
}

/**
 * The failure for `code` read in the scheme of `source` (one of
 * SOLANA_WALLET_FAILURE_SOURCE_*), and the message to show for it.
 * `walletName` is the chosen wallet, by id (phantom, solflare) or by name, or
 * "" when the app does not know which wallet answered. A numeric code may be
 * passed as a number.
 *
 * A code this build does not know, from any source, and any code from an
 * unknown source, is wallet_error: its message names the wallet and says to
 * check it, never the wallet's own English. A message that names the wallet
 * names "Solana" when the wallet is unknown, except an extension that is not
 * found, which then reads as no wallet at all.
 */
export function solanaWalletFailureFor(source: string, code: string | number | null | undefined, walletName?: string | null): SolanaWalletFailure {
  const failure = failureOf(source, String(code ?? "").trim());

  // Phantom and Solflare by id or by name; any other name as given
  let name = String(walletName ?? "").trim();
  switch (name.toLowerCase()) {
    case "phantom":
      name = "Phantom";
      break;
    case "solflare":
      name = "Solflare";
      break;
  }
  const nameOrGeneric = name || GENERIC_WALLET_NAME;

  switch (failure) {
    case SOLANA_WALLET_FAILURE_USER_REJECTED:
      return { failure, messageKey: SOLANA_WALLET_FAILURE_MESSAGE_USER_REJECTED, walletName: "" };
    case SOLANA_WALLET_FAILURE_NO_WALLET:
      return { failure, messageKey: SOLANA_WALLET_FAILURE_MESSAGE_NO_WALLET, walletName: "" };
    case SOLANA_WALLET_FAILURE_WALLET_APP_NOT_FOUND:
      return { failure, messageKey: SOLANA_WALLET_FAILURE_MESSAGE_WALLET_APP_NOT_FOUND, walletName: "" };
    case SOLANA_WALLET_FAILURE_EXTENSION_NOT_FOUND:
      // "the Solana extension" would name a product that does not exist
      return name
        ? { failure, messageKey: SOLANA_WALLET_FAILURE_MESSAGE_EXTENSION_NOT_FOUND, walletName: name }
        : { failure, messageKey: SOLANA_WALLET_FAILURE_MESSAGE_NO_WALLET, walletName: "" };
    case SOLANA_WALLET_FAILURE_NO_ACCOUNT:
      return { failure, messageKey: SOLANA_WALLET_FAILURE_MESSAGE_NO_ACCOUNT, walletName: nameOrGeneric };
    case SOLANA_WALLET_FAILURE_SESSION_NOT_FOUND:
      return { failure, messageKey: SOLANA_WALLET_FAILURE_MESSAGE_SESSION_NOT_FOUND, walletName: "" };
    default:
      return { failure: SOLANA_WALLET_FAILURE_WALLET, messageKey: SOLANA_WALLET_FAILURE_MESSAGE_WALLET, walletName: nameOrGeneric };
  }
}
