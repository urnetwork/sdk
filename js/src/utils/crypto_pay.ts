/**
 * Crypto payment rules, shared by every app and by ur.io.
 *
 * This is the TypeScript twin of `sdk/crypto_pay.go`: the functions have the
 * Go names in camelCase, and the constants the Go names in UPPER_SNAKE_CASE,
 * as this package names its constants. The Go version is what the apps bind;
 * this is what the web imports. The two must stay in step, and
 * test/crypto_pay_vectors.json holds the vectors both test suites replay.
 *
 * The rule they encode is the Solana Pay one (solana_pay.ts): the client never
 * names its own price, address or payment uri. The server quotes the invoice
 * and builds its uri; the client only checks the uri against the address it
 * shows (checkCryptoPaymentUri) before it renders or opens it.
 */

import type { CryptoPaymentAsset, CryptoTerm } from "../generated/openapi";
import { parseRfc3339 } from "./rfc3339";

/** The coins (the `asset` of every crypto payment route). Each is also its coin's payment method. */
export const CRYPTO_ASSET_USDC_SOLANA = "usdc_solana";
export const CRYPTO_ASSET_ZEC = "zec";
export const CRYPTO_ASSET_XMR = "xmr";

/** The fiat method: Stripe on every surface outside an app store. */
export const PAYMENT_METHOD_CARD = "card";

/** An invoice's state (the status route's `status`), first match wins. */
export const CRYPTO_PAYMENT_STATUS_PENDING = "pending";
export const CRYPTO_PAYMENT_STATUS_SEEN = "seen";
export const CRYPTO_PAYMENT_STATUS_CONFIRMING = "confirming";
export const CRYPTO_PAYMENT_STATUS_PAID = "paid";
export const CRYPTO_PAYMENT_STATUS_EXPIRED = "expired";
export const CRYPTO_PAYMENT_STATUS_REVIEW = "review";
export const CRYPTO_PAYMENT_STATUS_UNKNOWN = "unknown";

/** Where a paid invoice goes. */
export const CRYPTO_PAYMENT_DELIVERY_ACCOUNT = "account";
export const CRYPTO_PAYMENT_DELIVERY_NETWORK = "network";
export const CRYPTO_PAYMENT_DELIVERY_CODE = "code";

/** The crypto refusals, beside the other purchase error codes. */
export const PURCHASE_ERROR_CODE_ASSET_UNAVAILABLE = "asset_unavailable";
export const PURCHASE_ERROR_CODE_QUOTE_EXPIRED = "quote_expired";
export const PURCHASE_ERROR_CODE_UNDERPAID = "underpaid";

/** The stores a crypto purchase's events name (purchase.*, offer.cta.tapped); USDC's is "solana". */
export const EVENT_STORE_ZCASH = "zcash";
export const EVENT_STORE_MONERO = "monero";

/** The coins in the order every surface shows them, after Card. The order of the server's list means nothing. */
const CRYPTO_ASSET_ORDER = [CRYPTO_ASSET_USDC_SOLANA, CRYPTO_ASSET_ZEC, CRYPTO_ASSET_XMR] as const;

const PLAN_MONTHLY = "monthly";

function isCryptoAsset(asset: string): boolean {
  return (CRYPTO_ASSET_ORDER as readonly string[]).includes(asset);
}

type AssetList = readonly (CryptoPaymentAsset | null | undefined)[] | null | undefined;

/**
 * The method row for a purchase surface: card, then each coin that sells at
 * least one of `items` (the plans or the packs the surface offers), always in
 * the order USDC, Zcash, Monero. A coin the server did not list, or one this
 * sdk does not know, is never offered.
 */
export function paymentMethods(assets: AssetList, items: readonly string[] | null | undefined): string[] {
  const methods: string[] = [PAYMENT_METHOD_CARD];
  if (!items) return methods;
  for (const asset of CRYPTO_ASSET_ORDER) {
    if (items.some((item) => cryptoAssetSells(assets, asset, item))) {
      methods.push(asset);
    }
  }
  return methods;
}

/**
 * Whether the server's list names `asset` as selling `item`, a plan (`yearly`,
 * `yearly_onboarding`) or a data pack (`data_1tib`, `data_10tib`). Monthly is
 * card only, whatever the list says.
 */
export function cryptoAssetSells(assets: AssetList, asset: string, item: string): boolean {
  if (!assets || !isCryptoAsset(asset) || !paymentMethodAllowsPlan(asset, item)) return false;
  return assets.some(
    (listed) =>
      !!listed &&
      listed.asset === asset &&
      ((listed.plans ?? []).includes(item) || (listed.items ?? []).includes(item)),
  );
}

/**
 * False only for a coin with `monthly`: no coin sells monthly, so picking a
 * coin greys the monthly plan out ("Monthly is card only").
 */
export function paymentMethodAllowsPlan(method: string, plan: string): boolean {
  return !(isCryptoAsset(method) && plan === PLAN_MONTHLY);
}

/** The ticker shown beside an amount ("0.072387 XMR"), "" for anything else. */
export function cryptoAssetTicker(asset: string): string {
  switch (asset) {
    case CRYPTO_ASSET_USDC_SOLANA:
      return "USDC";
    case CRYPTO_ASSET_ZEC:
      return "ZEC";
    case CRYPTO_ASSET_XMR:
      return "XMR";
    default:
      return "";
  }
}

/**
 * The wallets the invoice names as working for a coin ("Wallets that work:
 * …"), in the order to show them. Every Zcash one sends to a shielded address.
 */
export function cryptoWalletSuggestions(asset: string): string[] {
  switch (asset) {
    case CRYPTO_ASSET_XMR:
      return ["Cake", "Feather", "Monerujo", "Stack", "Unstoppable"];
    case CRYPTO_ASSET_ZEC:
      return ["Zodl", "Cake", "Zingo", "Zkool", "Vizor"];
    case CRYPTO_ASSET_USDC_SOLANA:
      return ["Phantom", "Solflare"];
    default:
      return [];
  }
}

function cryptoPaymentUriScheme(asset: string): string {
  switch (asset) {
    case CRYPTO_ASSET_USDC_SOLANA:
      return "solana";
    case CRYPTO_ASSET_ZEC:
      return "zcash";
    case CRYPTO_ASSET_XMR:
      return "monero";
    default:
      return "";
  }
}

const isHex = (code: number) =>
  (0x30 <= code && code <= 0x39) || (0x41 <= code && code <= 0x46) || (0x61 <= code && code <= 0x66);

/** %XX to its byte, as Go's url.PathUnescape does; null for a malformed escape. */
function percentDecode(value: string): string | null {
  let out = "";
  for (let i = 0; i < value.length; i++) {
    if (value[i] !== "%") {
      out += value[i];
      continue;
    }
    if (value.length <= i + 2 || !isHex(value.charCodeAt(i + 1)) || !isHex(value.charCodeAt(i + 2))) {
      return null;
    }
    out += String.fromCharCode(parseInt(value.slice(i + 1, i + 3), 16));
    i += 2;
  }
  return out;
}

/**
 * Whether `uri` is a payment uri of `asset`'s coin that pays exactly
 * `address`, the address the invoice shows. Refuse to render or open a uri
 * that fails it: the QR code and "Open in wallet" must never pay somewhere
 * other than the address on screen.
 *
 * The uri is the server's `<scheme>:<address>?<query>` (Solana Pay, ZIP 321
 * or the Monero uri scheme). The scheme must be the coin's, compared without
 * case; the address between the colon and the query must be `address`
 * exactly; and the uri must be plain printable ascii with no fragment, as the
 * server's percent-encoded uris are. A Zcash uri must not name a second
 * recipient (ZIP 321's `address.1=…`), which a wallet would pay too.
 */
export function checkCryptoPaymentUri(asset: string, uri: string, address: string): boolean {
  const scheme = cryptoPaymentUriScheme(asset);
  if (!scheme || !address || !uri) return false;
  for (let i = 0; i < uri.length; i++) {
    const code = uri.charCodeAt(i);
    if (code <= 0x20 || 0x7e < code || code === 0x23) return false;
  }
  const colon = uri.indexOf(":");
  if (colon < 0 || uri.slice(0, colon).toLowerCase() !== scheme) return false;
  const rest = uri.slice(colon + 1);
  const question = rest.indexOf("?");
  const uriAddress = question < 0 ? rest : rest.slice(0, question);
  const query = question < 0 ? "" : rest.slice(question + 1);
  if (uriAddress !== address) return false;
  if (asset === CRYPTO_ASSET_ZEC && query !== "") {
    for (const param of query.split("&")) {
      const equals = param.indexOf("=");
      const name = percentDecode(equals < 0 ? param : param.slice(0, equals));
      if (name === null) return false;
      const lower = name.toLowerCase();
      if (lower === "address" || lower.startsWith("address.")) return false;
    }
  }
  return true;
}

/**
 * Whether to show the renewal notice for a one-time crypto credit now: from
 * the end of its paid term until its grace ends, and never before. False for
 * no term, or for times that do not parse.
 */
export function cryptoTermNoticeDue(term: CryptoTerm | null | undefined, now: number = Date.now()): boolean {
  if (!term) return false;
  const termEnd = parseRfc3339(term.term_end_time);
  const graceEnd = parseRfc3339(term.grace_end_time);
  if (termEnd === null || graceEnd === null) return false;
  return termEnd <= now && now < graceEnd;
}
