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
 *
 * The coins sit in the payment widget's tabs (cryptoPaymentTabs): USDC on
 * Solana, Zcash and Monero each have a tab of their own, and TAO, Bitcoin, ETH
 * and SOL share the "More coins" tab (mmm/BTCPAY.md §17.6). Every coin's facts
 * (its ticker, label, mark, decimals, market and uri scheme) are in one table,
 * which every function here reads.
 */

import type { CryptoPaymentAsset, CryptoTerm } from "../generated/openapi";
import { parseRfc3339 } from "./rfc3339";

/** The coins (the `asset` of every crypto payment route). Each is also its coin's payment method. */
export const CRYPTO_ASSET_USDC_SOLANA = "usdc_solana";
export const CRYPTO_ASSET_ZEC = "zec";
export const CRYPTO_ASSET_XMR = "xmr";
/** TAO, on Bittensor */
export const CRYPTO_ASSET_TAO = "tao";
export const CRYPTO_ASSET_BTC = "btc";
export const CRYPTO_ASSET_ETH = "eth";
/** native SOL */
export const CRYPTO_ASSET_SOL = "sol";

/** The fiat method: Stripe on every surface outside an app store. */
export const PAYMENT_METHOD_CARD = "card";

/** The payment widget's tab of several coins: TAO, Bitcoin, ETH and SOL. A tab of one coin has that coin's asset as its id. */
export const CRYPTO_PAYMENT_TAB_MORE_COINS = "more_coins";

/**
 * The coins' marks (`iconKey`): each project's official mark in a single
 * color, which every app bundles under this name (coin_btc.svg on the web, an
 * Android drawable coin_btc). USDC on Solana shows the USDC mark, and SOL the
 * Solana mark.
 */
export const CRYPTO_ICON_KEY_USDC = "coin_usdc";
export const CRYPTO_ICON_KEY_ZEC = "coin_zec";
export const CRYPTO_ICON_KEY_XMR = "coin_xmr";
export const CRYPTO_ICON_KEY_TAO = "coin_tao";
export const CRYPTO_ICON_KEY_BTC = "coin_btc";
export const CRYPTO_ICON_KEY_ETH = "coin_eth";
export const CRYPTO_ICON_KEY_SOL = "coin_sol";

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

/** The stores a crypto purchase's events name (purchase.*, offer.cta.tapped): its coin's market (cryptoAssetMarket). */
export const EVENT_STORE_SOLANA = "solana";
export const EVENT_STORE_ZCASH = "zcash";
export const EVENT_STORE_MONERO = "monero";
export const EVENT_STORE_BITTENSOR = "bittensor";
export const EVENT_STORE_BITCOIN = "bitcoin";
export const EVENT_STORE_ETHEREUM = "ethereum";

/** the USDC mint on Solana mainnet, the only token a usdc_solana uri may name */
const SOLANA_USDC_MINT = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";

/** One coin's facts, the same on every surface. */
interface CryptoCoin {
  /** shown beside an amount ("0.072387 XMR") */
  readonly ticker: string;
  /** the label in English, and its string id in the shared localizations (localizations/keys) */
  readonly label: string;
  readonly labelKey: string;
  /** the coin's mark (CRYPTO_ICON_KEY_*) */
  readonly iconKey: string;
  /** the atomic unit's decimals: amount_atomic is the amount times 10^decimals */
  readonly decimals: number;
  /** the decimals the server quotes an amount to (it rounds amounts up) */
  readonly quoteDecimals: number;
  /** the subscription market a credit in the coin renews on, which is also the store its purchase events name */
  readonly market: string;
  /** the scheme of its payment uri, "" for a coin without one: its invoice shows the address, the amount and a QR code of the address alone */
  readonly uriScheme: string;
}

/** The coins this sdk knows, by asset (mmm/BTCPAY.md §17.7 for the four added on 2026-11-10). A coin missing here is never offered and never shown. */
const CRYPTO_ASSET_COINS: ReadonlyMap<string, CryptoCoin> = new Map<string, CryptoCoin>([
  [
    CRYPTO_ASSET_USDC_SOLANA,
    {
      ticker: "USDC",
      label: "USDC on Solana",
      labelKey: "pay_method_usdc_solana",
      iconKey: CRYPTO_ICON_KEY_USDC,
      decimals: 6,
      quoteDecimals: 6,
      market: "solana",
      uriScheme: "solana",
    },
  ],
  [
    CRYPTO_ASSET_ZEC,
    {
      ticker: "ZEC",
      label: "Zcash",
      labelKey: "pay_method_zcash",
      iconKey: CRYPTO_ICON_KEY_ZEC,
      decimals: 8,
      quoteDecimals: 6,
      market: "zcash",
      uriScheme: "zcash",
    },
  ],
  [
    CRYPTO_ASSET_XMR,
    {
      ticker: "XMR",
      label: "Monero",
      labelKey: "pay_method_monero",
      iconKey: CRYPTO_ICON_KEY_XMR,
      decimals: 12,
      quoteDecimals: 6,
      market: "monero",
      uriScheme: "monero",
    },
  ],
  [
    CRYPTO_ASSET_TAO,
    {
      ticker: "TAO",
      label: "TAO",
      labelKey: "pay_method_tao",
      iconKey: CRYPTO_ICON_KEY_TAO,
      decimals: 9,
      quoteDecimals: 6,
      market: "bittensor",
      // TAO has no payment uri standard
      uriScheme: "",
    },
  ],
  [
    CRYPTO_ASSET_BTC,
    {
      ticker: "BTC",
      label: "Bitcoin",
      labelKey: "pay_method_bitcoin",
      iconKey: CRYPTO_ICON_KEY_BTC,
      decimals: 8,
      quoteDecimals: 8,
      market: "bitcoin",
      uriScheme: "bitcoin",
    },
  ],
  [
    CRYPTO_ASSET_ETH,
    {
      ticker: "ETH",
      label: "ETH",
      labelKey: "pay_method_eth",
      iconKey: CRYPTO_ICON_KEY_ETH,
      decimals: 18,
      quoteDecimals: 6,
      market: "ethereum",
      uriScheme: "ethereum",
    },
  ],
  [
    CRYPTO_ASSET_SOL,
    {
      ticker: "SOL",
      label: "SOL",
      labelKey: "pay_method_sol",
      iconKey: CRYPTO_ICON_KEY_SOL,
      decimals: 9,
      quoteDecimals: 6,
      market: "solana",
      uriScheme: "solana",
    },
  ],
]);

/** One tab of the payment widget's coin selector. */
interface CryptoPaymentTabLayout {
  readonly id: string;
  readonly label: string;
  readonly labelKey: string;
  /** its coins, in the order its sub-selector shows them */
  readonly assets: readonly string[];
  /** shown only while the server lists one of its coins; a tab of one coin is shown whenever the selector is */
  readonly listedOnly: boolean;
}

const singleCoinTab = (asset: string): CryptoPaymentTabLayout => ({
  id: asset,
  label: CRYPTO_ASSET_COINS.get(asset)!.label,
  labelKey: CRYPTO_ASSET_COINS.get(asset)!.labelKey,
  assets: [asset],
  listedOnly: false,
});

/** The selector's tabs in order: the three main coins, then "More coins". */
const CRYPTO_PAYMENT_TAB_LAYOUTS: readonly CryptoPaymentTabLayout[] = [
  singleCoinTab(CRYPTO_ASSET_USDC_SOLANA),
  singleCoinTab(CRYPTO_ASSET_ZEC),
  singleCoinTab(CRYPTO_ASSET_XMR),
  {
    id: CRYPTO_PAYMENT_TAB_MORE_COINS,
    label: "More coins",
    labelKey: "pay_method_more_coins",
    assets: [CRYPTO_ASSET_TAO, CRYPTO_ASSET_BTC, CRYPTO_ASSET_ETH, CRYPTO_ASSET_SOL],
    listedOnly: true,
  },
];

/** The coins in the order every surface shows them, after Card: the tabs' coins in the tabs' order. The order of the server's list means nothing. */
const CRYPTO_ASSET_ORDER: readonly string[] = CRYPTO_PAYMENT_TAB_LAYOUTS.flatMap((layout) => layout.assets);

const PLAN_MONTHLY = "monthly";

function isCryptoAsset(asset: string): boolean {
  return CRYPTO_ASSET_COINS.has(asset);
}

type AssetList = readonly (CryptoPaymentAsset | null | undefined)[] | null | undefined;

/**
 * The method row for a purchase surface: card, then each coin that sells at
 * least one of `items` (the plans or the packs the surface offers), always in
 * the order USDC, Zcash, Monero, TAO, Bitcoin, ETH, SOL. A coin the server did
 * not list, or one this sdk does not know, is never offered. The server lists
 * only the coins it would quote now, so a coin whose quotes it refuses for now
 * (its wallet scanner lagging) is not offered either: that one is in the
 * route's `unavailable_assets`, which cryptoPaymentTabs shows greyed out and
 * which is never passed here as `assets`.
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

/** A coin in the payment widget. */
export interface CryptoPaymentCoin {
  /** one of the CRYPTO_ASSET_* values */
  readonly asset: string;
  /** "USDC on Solana", "Zcash", "Monero", "TAO", "Bitcoin", "ETH" or "SOL", in English (cryptoAssetLabel) */
  readonly label: string;
  /** the label's string id in the shared localizations (cryptoAssetLabelKey) */
  readonly labelKey: string;
  /**
   * The coin's mark, one of the CRYPTO_ICON_KEY_* values. A mark is never
   * dimmed (Solana's and Bittensor's brand rules allow theirs only in black or
   * white, at full contrast), whatever `available` says.
   */
  readonly iconKey: string;
  /**
   * The server lists the coin as payable now, for one of the surface's items.
   * An unavailable coin stays in its place and cannot be chosen: its label is
   * greyed, with "Temporarily unavailable" (pay_method_unavailable) beside it,
   * and its mark is unchanged.
   */
  readonly available: boolean;
}

/** A tab of the payment widget's coin selector: one coin, or the "More coins" tab, which opens a sub-selector of its coins. */
export interface CryptoPaymentTab {
  /** the asset of a tab with one coin, or CRYPTO_PAYMENT_TAB_MORE_COINS */
  readonly id: string;
  /** in English: the coin's label, or "More coins" */
  readonly label: string;
  /** the label's string id in the shared localizations */
  readonly labelKey: string;
  /**
   * The marks the tab shows, in order, each at full contrast: its coin's, or
   * the four of "More coins" as separate marks with space between them, never
   * merged into one badge.
   */
  readonly iconKeys: readonly string[];
  /** its coins, in order: one, or TAO, Bitcoin, ETH and SOL. Each says whether it can be paid with now. */
  readonly coins: readonly CryptoPaymentCoin[];
  /** at least one of its coins is available; a tab with none cannot be chosen, and its label is greyed as an unavailable coin's is */
  readonly available: boolean;
}

/**
 * The payment widget's coin selector for a purchase surface that sells
 * `items` (as for paymentMethods), in order: USDC on Solana, Zcash, Monero,
 * then "More coins" with TAO, Bitcoin, ETH and SOL. Card stays the method
 * row's own first choice (paymentMethods).
 *
 * `assets` are the coins the server would quote now: the balance's
 * `crypto_assets`, or GET /pay/crypto/assets' `assets`. `unavailableAssets`
 * are the coins that are on but cannot be quoted now: the route's
 * `unavailable_assets`, or null where the surface reads the balance, which
 * lists only the coins that can be paid with now. A coin is available when
 * `assets` names it as selling one of `items`. The coins' per-coin
 * `available` flags say the same thing, by the server's contract, and are not
 * read.
 *
 * - With no coin of either list selling one of the items, there is nothing to
 *   choose: the list is empty, and the surface shows card alone, as it did
 *   before crypto.
 * - Otherwise the three main tabs are always there, and "More coins" is there
 *   while either list names one of its coins (a server that predates them
 *   names none).
 * - Every coin of a tab is shown in its place, never hidden. A coin that
 *   cannot be paid with now, for these items, has `available` false: its label
 *   is greyed and says "Temporarily unavailable", and its mark, on its tab and
 *   in the sub-selector, is unchanged.
 */
export function cryptoPaymentTabs(
  assets: AssetList,
  unavailableAssets: AssetList,
  items: readonly string[] | null | undefined,
): CryptoPaymentTab[] {
  if (!items) return [];
  const sells = (list: AssetList, asset: string) => items.some((item) => cryptoAssetSells(list, asset, item));
  const listed = (asset: string) => sells(assets, asset) || sells(unavailableAssets, asset);
  if (!CRYPTO_ASSET_ORDER.some(listed)) return [];
  const tabs: CryptoPaymentTab[] = [];
  for (const layout of CRYPTO_PAYMENT_TAB_LAYOUTS) {
    if (layout.listedOnly && !layout.assets.some(listed)) continue;
    const coins: CryptoPaymentCoin[] = [];
    const iconKeys: string[] = [];
    for (const asset of layout.assets) {
      const coin = CRYPTO_ASSET_COINS.get(asset)!;
      coins.push({ asset, label: coin.label, labelKey: coin.labelKey, iconKey: coin.iconKey, available: sells(assets, asset) });
      iconKeys.push(coin.iconKey);
    }
    tabs.push({
      id: layout.id,
      label: layout.label,
      labelKey: layout.labelKey,
      iconKeys,
      coins,
      available: coins.some((coin) => coin.available),
    });
  }
  return tabs;
}

/**
 * The id of the tab a coin sits in: the asset itself for USDC on Solana,
 * Zcash and Monero, CRYPTO_PAYMENT_TAB_MORE_COINS for TAO, Bitcoin, ETH and
 * SOL, and "" for anything else. A remembered method selects this tab.
 */
export function cryptoPaymentTabId(asset: string): string {
  return CRYPTO_PAYMENT_TAB_LAYOUTS.find((layout) => layout.assets.includes(asset))?.id ?? "";
}

/** The ticker shown beside an amount ("0.072387 XMR"), "" for anything else. */
export function cryptoAssetTicker(asset: string): string {
  return CRYPTO_ASSET_COINS.get(asset)?.ticker ?? "";
}

/** The coin's label in English ("USDC on Solana", "Bitcoin"), "" for anything else. The label of USDC always names its chain. */
export function cryptoAssetLabel(asset: string): string {
  return CRYPTO_ASSET_COINS.get(asset)?.label ?? "";
}

/** The string id of the coin's label in the shared localizations (localizations/keys), "" for anything else. */
export function cryptoAssetLabelKey(asset: string): string {
  return CRYPTO_ASSET_COINS.get(asset)?.labelKey ?? "";
}

/** The coin's mark, one of the CRYPTO_ICON_KEY_* values, "" for anything else. */
export function cryptoAssetIconKey(asset: string): string {
  return CRYPTO_ASSET_COINS.get(asset)?.iconKey ?? "";
}

/**
 * The decimals of the coin's atomic unit (`amount_atomic` is the amount times
 * 10^decimals): 6 for USDC, 8 for Zcash and Bitcoin, 12 for Monero, 9 for TAO
 * and SOL, 18 for ETH. 0 for anything else.
 */
export function cryptoAssetDecimals(asset: string): number {
  return CRYPTO_ASSET_COINS.get(asset)?.decimals ?? 0;
}

/** The decimals the server quotes the coin's amounts to, rounding up: 8 for Bitcoin, 6 for every other coin. 0 for anything else. */
export function cryptoAssetQuoteDecimals(asset: string): number {
  return CRYPTO_ASSET_COINS.get(asset)?.quoteDecimals ?? 0;
}

/**
 * The subscription market a credit in the coin renews on (`crypto_term.market`,
 * a subscription's store), which is also the store its purchase events name:
 * "solana" for USDC and SOL, "zcash", "monero", "bittensor" for TAO, "bitcoin"
 * and "ethereum". "" for anything else.
 */
export function cryptoAssetMarket(asset: string): string {
  return CRYPTO_ASSET_COINS.get(asset)?.market ?? "";
}

/**
 * The scheme of the coin's payment uri: "solana" (Solana Pay) for USDC and
 * SOL, "zcash" (ZIP 321), "monero", "bitcoin" (BIP 21) and "ethereum" (EIP
 * 681). "" for TAO, which has no payment uri, and for anything else.
 */
export function cryptoPaymentUriScheme(asset: string): string {
  return CRYPTO_ASSET_COINS.get(asset)?.uriScheme ?? "";
}

/**
 * The wallets the invoice names as working for a coin ("Wallets that work:
 * …"), in the order to show them. Every Zcash one sends to a shielded address;
 * the TAO ones are Bittensor's own list. Empty for anything else, and for
 * Bitcoin and ETH, which most wallets send.
 */
export function cryptoWalletSuggestions(asset: string): string[] {
  switch (asset) {
    case CRYPTO_ASSET_XMR:
      return ["Cake", "Feather", "Monerujo", "Stack", "Unstoppable"];
    case CRYPTO_ASSET_ZEC:
      return ["Zodl", "Cake", "Zingo", "Zkool", "Vizor"];
    case CRYPTO_ASSET_USDC_SOLANA:
    case CRYPTO_ASSET_SOL:
      return ["Phantom", "Solflare"];
    case CRYPTO_ASSET_TAO:
      return ["btcli", "Talisman", "Nova", "SubWallet", "Polkadot.js", "Ledger", "Tangem"];
    default:
      return [];
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

/** A uri query's parameter, its name percent-decoded. */
interface CryptoPaymentUriParam {
  readonly name: string;
  /** the name with ASCII letters lowercased, and nothing else changed, so both twins compare names the same way */
  readonly lowerName: string;
  readonly rawValue: string;
}

/**
 * Whether `uri` is a payment uri of `asset`'s coin that pays exactly
 * `address`, the address the invoice shows. Refuse to render or open a uri
 * that fails it: the QR code and "Open in wallet" must never pay somewhere
 * other than the address on screen, nor in another coin.
 *
 * The uri is the server's `<scheme>:<address>?<query>`, with the coin's scheme
 * (cryptoPaymentUriScheme). The scheme must be the coin's, compared without
 * case; the address between the colon and the query must be `address`
 * exactly; and the uri must be plain printable ascii with no fragment, as the
 * server's percent-encoded uris are. TAO has no payment uri, so a TAO uri
 * always fails. Monero puts a second recipient in the path, which the exact
 * address match already refuses, and its query is not read. For every other
 * coin the query's parameter names are percent-decoded (a malformed escape
 * fails the uri) and compare with their ASCII letters lowercased:
 *
 * - Zcash (ZIP 321) must not name a second recipient (`address.1=…`), which a
 *   wallet would pay too.
 * - USDC on Solana (Solana Pay) must name the USDC mint as its one
 *   `spl-token`: without it, a wallet would send the amount in SOL.
 * - SOL (Solana Pay) must name no `spl-token`, which would send a token
 *   instead. Its `reference` keys, like USDC's, are not funds and are not
 *   checked here.
 * - Bitcoin (BIP 21) may carry `amount` (in BTC), `label` and `message`, each
 *   once, and nothing else: a `lightning` invoice, another address (BIP 321's
 *   `bc`, `sp`, `lno`) or a `req-` parameter would pay something the invoice
 *   does not show.
 * - ETH (EIP 681) names Ethereum mainnet after the address (`<address>@1`),
 *   with no function call, and may carry `value` (in wei) once and nothing
 *   else.
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
  const uriTarget = question < 0 ? rest : rest.slice(0, question);
  const query = question < 0 ? "" : rest.slice(question + 1);
  // an ethereum uri names its chain id after the address, 1 for mainnet
  const target = asset === CRYPTO_ASSET_ETH ? `${address}@1` : address;
  if (uriTarget !== target) return false;
  if (asset === CRYPTO_ASSET_XMR) return true;
  // the query split at `&`, each parameter at its first `=`
  const params: CryptoPaymentUriParam[] = [];
  if (query !== "") {
    for (const param of query.split("&")) {
      const equals = param.indexOf("=");
      const name = percentDecode(equals < 0 ? param : param.slice(0, equals));
      if (name === null) return false;
      params.push({
        name,
        lowerName: name.replace(/[A-Z]/g, (c) => c.toLowerCase()),
        rawValue: equals < 0 ? "" : param.slice(equals + 1),
      });
    }
  }
  // every parameter's name is one of `names`, and none comes twice
  const onlyOnce = (...names: string[]) => {
    const seen = new Set<string>();
    for (const param of params) {
      if (!names.includes(param.lowerName) || seen.has(param.lowerName)) return false;
      seen.add(param.lowerName);
    }
    return true;
  };
  const splTokens = () => params.filter((param) => param.lowerName === "spl-token");
  switch (asset) {
    case CRYPTO_ASSET_ZEC:
      return !params.some((param) => param.lowerName === "address" || param.lowerName.startsWith("address."));
    case CRYPTO_ASSET_USDC_SOLANA: {
      const matches = splTokens();
      return matches.length === 1 && matches[0].name === "spl-token" && percentDecode(matches[0].rawValue) === SOLANA_USDC_MINT;
    }
    case CRYPTO_ASSET_SOL:
      return splTokens().length === 0;
    case CRYPTO_ASSET_BTC:
      return onlyOnce("amount", "label", "message");
    case CRYPTO_ASSET_ETH:
      return onlyOnce("value");
    default:
      return true;
  }
}

/**
 * The text an invoice's QR code encodes: `uri` when it passes
 * checkCryptoPaymentUri, the bare `address` for a coin without a payment uri
 * (TAO, whose wallets scan an address alone), and "" for anything else, when
 * the invoice shows no QR code and the address and the amount are sent by
 * hand. An address that is not plain printable ascii gets no QR code either.
 */
export function cryptoPaymentQrText(asset: string, uri: string, address: string): string {
  if (checkCryptoPaymentUri(asset, uri, address)) return uri;
  if (!isCryptoAsset(asset) || cryptoPaymentUriScheme(asset) !== "" || !address) return "";
  for (let i = 0; i < address.length; i++) {
    const code = address.charCodeAt(i);
    if (code <= 0x20 || 0x7e < code) return "";
  }
  return address;
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
