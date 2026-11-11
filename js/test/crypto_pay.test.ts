import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { registerTsResolve } from "./ts-resolve.ts";

// The crypto payment rules replay test/crypto_pay_vectors.json, the vectors
// sdk/crypto_pay_test.go replays against the Go twin, so the two cannot drift
// apart.

registerTsResolve();
const {
  CRYPTO_ASSET_BTC,
  CRYPTO_ASSET_ETH,
  CRYPTO_ASSET_SOL,
  CRYPTO_ASSET_TAO,
  CRYPTO_ASSET_USDC_SOLANA,
  CRYPTO_ASSET_XMR,
  CRYPTO_ASSET_ZEC,
  CRYPTO_ICON_KEY_BTC,
  CRYPTO_ICON_KEY_TAO,
  CRYPTO_ICON_KEY_ETH,
  CRYPTO_ICON_KEY_XMR,
  CRYPTO_ICON_KEY_SOL,
  CRYPTO_ICON_KEY_USDC,
  CRYPTO_ICON_KEY_ZEC,
  CRYPTO_PAYMENT_DELIVERY_ACCOUNT,
  CRYPTO_PAYMENT_DELIVERY_CODE,
  CRYPTO_PAYMENT_DELIVERY_NETWORK,
  CRYPTO_PAYMENT_STATUS_CONFIRMING,
  CRYPTO_PAYMENT_STATUS_EXPIRED,
  CRYPTO_PAYMENT_STATUS_PAID,
  CRYPTO_PAYMENT_STATUS_PENDING,
  CRYPTO_PAYMENT_STATUS_REVIEW,
  CRYPTO_PAYMENT_STATUS_SEEN,
  CRYPTO_PAYMENT_STATUS_UNKNOWN,
  CRYPTO_PAYMENT_STEP_CURRENT,
  CRYPTO_PAYMENT_STEP_DONE,
  CRYPTO_PAYMENT_STEP_UPCOMING,
  CRYPTO_PAYMENT_TAB_MORE_COINS,
  EVENT_STORE_BITCOIN,
  EVENT_STORE_BITTENSOR,
  EVENT_STORE_ETHEREUM,
  EVENT_STORE_MONERO,
  EVENT_STORE_SOLANA,
  EVENT_STORE_ZCASH,
  PAYMENT_METHOD_CARD,
  PURCHASE_ERROR_CODE_ASSET_UNAVAILABLE,
  PURCHASE_ERROR_CODE_QUOTE_EXPIRED,
  PURCHASE_ERROR_CODE_UNDERPAID,
  checkCryptoPaymentUri,
  cryptoAssetDecimals,
  cryptoAssetIconKey,
  cryptoAssetLabel,
  cryptoAssetLabelKey,
  cryptoAssetLinesFor,
  cryptoAssetMarket,
  cryptoAssetQuoteDecimals,
  cryptoAssetSells,
  cryptoAssetTicker,
  cryptoPaidAsset,
  cryptoPaymentEventProduct,
  cryptoPaymentQrText,
  cryptoPaymentShowsMemo,
  cryptoPaymentSteps,
  cryptoPaymentTabCoin,
  cryptoPaymentTabId,
  cryptoPaymentTabs,
  cryptoPaymentUriScheme,
  cryptoTermNoticeDue,
  cryptoWalletSuggestions,
  paymentMethodAllowsPlan,
  paymentMethods,
} = await import("../src/utils/crypto_pay.ts");
const utils = await import("../src/utils/index.ts");

const vectors = JSON.parse(readFileSync(new URL("./crypto_pay_vectors.json", import.meta.url), "utf8"));

// The wire strings are the server's and the other platforms'; a drift here
// sends a value the server refuses.
test("crypto pay constants", () => {
  assert.deepEqual(
    {
      CRYPTO_ASSET_USDC_SOLANA,
      CRYPTO_ASSET_ZEC,
      CRYPTO_ASSET_XMR,
      CRYPTO_ASSET_TAO,
      CRYPTO_ASSET_BTC,
      CRYPTO_ASSET_ETH,
      CRYPTO_ASSET_SOL,
      CRYPTO_PAYMENT_TAB_MORE_COINS,
      CRYPTO_ICON_KEY_USDC,
      CRYPTO_ICON_KEY_ZEC,
      CRYPTO_ICON_KEY_XMR,
      CRYPTO_ICON_KEY_TAO,
      CRYPTO_ICON_KEY_BTC,
      CRYPTO_ICON_KEY_ETH,
      CRYPTO_ICON_KEY_SOL,
      PAYMENT_METHOD_CARD,
      CRYPTO_PAYMENT_STATUS_PENDING,
      CRYPTO_PAYMENT_STATUS_SEEN,
      CRYPTO_PAYMENT_STATUS_CONFIRMING,
      CRYPTO_PAYMENT_STATUS_PAID,
      CRYPTO_PAYMENT_STATUS_EXPIRED,
      CRYPTO_PAYMENT_STATUS_REVIEW,
      CRYPTO_PAYMENT_STATUS_UNKNOWN,
      CRYPTO_PAYMENT_STEP_DONE,
      CRYPTO_PAYMENT_STEP_CURRENT,
      CRYPTO_PAYMENT_STEP_UPCOMING,
      CRYPTO_PAYMENT_DELIVERY_ACCOUNT,
      CRYPTO_PAYMENT_DELIVERY_NETWORK,
      CRYPTO_PAYMENT_DELIVERY_CODE,
      PURCHASE_ERROR_CODE_ASSET_UNAVAILABLE,
      PURCHASE_ERROR_CODE_QUOTE_EXPIRED,
      PURCHASE_ERROR_CODE_UNDERPAID,
      EVENT_STORE_SOLANA,
      EVENT_STORE_ZCASH,
      EVENT_STORE_MONERO,
      EVENT_STORE_BITTENSOR,
      EVENT_STORE_BITCOIN,
      EVENT_STORE_ETHEREUM,
    },
    {
      CRYPTO_ASSET_USDC_SOLANA: "usdc_solana",
      CRYPTO_ASSET_ZEC: "zec",
      CRYPTO_ASSET_XMR: "xmr",
      CRYPTO_ASSET_TAO: "tao",
      CRYPTO_ASSET_BTC: "btc",
      CRYPTO_ASSET_ETH: "eth",
      CRYPTO_ASSET_SOL: "sol",
      CRYPTO_PAYMENT_TAB_MORE_COINS: "more_coins",
      CRYPTO_ICON_KEY_USDC: "coin_usdc",
      CRYPTO_ICON_KEY_ZEC: "coin_zec",
      CRYPTO_ICON_KEY_XMR: "coin_xmr",
      CRYPTO_ICON_KEY_TAO: "coin_tao",
      CRYPTO_ICON_KEY_BTC: "coin_btc",
      CRYPTO_ICON_KEY_ETH: "coin_eth",
      CRYPTO_ICON_KEY_SOL: "coin_sol",
      PAYMENT_METHOD_CARD: "card",
      CRYPTO_PAYMENT_STATUS_PENDING: "pending",
      CRYPTO_PAYMENT_STATUS_SEEN: "seen",
      CRYPTO_PAYMENT_STATUS_CONFIRMING: "confirming",
      CRYPTO_PAYMENT_STATUS_PAID: "paid",
      CRYPTO_PAYMENT_STATUS_EXPIRED: "expired",
      CRYPTO_PAYMENT_STATUS_REVIEW: "review",
      CRYPTO_PAYMENT_STATUS_UNKNOWN: "unknown",
      CRYPTO_PAYMENT_STEP_DONE: "done",
      CRYPTO_PAYMENT_STEP_CURRENT: "current",
      CRYPTO_PAYMENT_STEP_UPCOMING: "upcoming",
      CRYPTO_PAYMENT_DELIVERY_ACCOUNT: "account",
      CRYPTO_PAYMENT_DELIVERY_NETWORK: "network",
      CRYPTO_PAYMENT_DELIVERY_CODE: "code",
      PURCHASE_ERROR_CODE_ASSET_UNAVAILABLE: "asset_unavailable",
      PURCHASE_ERROR_CODE_QUOTE_EXPIRED: "quote_expired",
      PURCHASE_ERROR_CODE_UNDERPAID: "underpaid",
      EVENT_STORE_SOLANA: "solana",
      EVENT_STORE_ZCASH: "zcash",
      EVENT_STORE_MONERO: "monero",
      EVENT_STORE_BITTENSOR: "bittensor",
      EVENT_STORE_BITCOIN: "bitcoin",
      EVENT_STORE_ETHEREUM: "ethereum",
    },
  );
});

test("the twins are exported from the package's utils", () => {
  for (const name of [
    "paymentMethods",
    "cryptoAssetSells",
    "paymentMethodAllowsPlan",
    "cryptoAssetTicker",
    "cryptoAssetLabel",
    "cryptoAssetLabelKey",
    "cryptoAssetIconKey",
    "cryptoAssetDecimals",
    "cryptoAssetQuoteDecimals",
    "cryptoAssetMarket",
    "cryptoPaymentUriScheme",
    "cryptoPaymentTabs",
    "cryptoPaymentTabId",
    "cryptoPaymentTabCoin",
    "cryptoAssetLinesFor",
    "cryptoPaymentQrText",
    "cryptoPaymentShowsMemo",
    "cryptoPaymentSteps",
    "cryptoPaymentEventProduct",
    "cryptoPaidAsset",
    "cryptoWalletSuggestions",
    "checkCryptoPaymentUri",
    "cryptoTermNoticeDue",
    "CryptoInvoice",
  ]) {
    assert.equal(typeof (utils as Record<string, unknown>)[name], "function", `${name} is not exported`);
  }
  // the strict time parser stays internal
  assert.equal((utils as Record<string, unknown>).parseRfc3339, undefined);
});

for (const vector of vectors.paymentMethods) {
  test(`paymentMethods: ${vector.name}`, () => {
    assert.deepEqual(paymentMethods(vector.assets, vector.items), vector.expected);
  });
}

test("cryptoAssetSells", () => {
  for (const vector of vectors.cryptoAssetSells) {
    assert.equal(
      cryptoAssetSells(vector.assets, vector.asset, vector.item),
      vector.expected,
      `cryptoAssetSells(${vector.asset}, ${JSON.stringify(vector.item)})`,
    );
  }
});

test("paymentMethodAllowsPlan", () => {
  for (const vector of vectors.paymentMethodAllowsPlan) {
    assert.equal(
      paymentMethodAllowsPlan(vector.method, vector.plan),
      vector.expected,
      `paymentMethodAllowsPlan(${JSON.stringify(vector.method)}, ${JSON.stringify(vector.plan)})`,
    );
  }
});

test("cryptoAssetTicker", () => {
  for (const vector of vectors.cryptoAssetTicker) {
    assert.equal(cryptoAssetTicker(vector.asset), vector.expected, `cryptoAssetTicker(${JSON.stringify(vector.asset)})`);
  }
});

test("cryptoWalletSuggestions", () => {
  for (const vector of vectors.cryptoWalletSuggestions) {
    assert.deepEqual(cryptoWalletSuggestions(vector.asset), vector.expected, `cryptoWalletSuggestions(${JSON.stringify(vector.asset)})`);
  }
});

// Only TAO's invoice names the wallets that work. The owner, 2026-11-11: "we
// do not need to suggest btc or eth wallets. for other coins besides tao we
// don't need to suggest wallets". Every other coin (USDC on Solana, Zcash,
// Monero, SOL, Bitcoin and ETH) names none, so its invoice shows no wallets
// line, and neither does anything else. The vectors pin every coin.
test("only TAO names its wallets", () => {
  const coins = [CRYPTO_ASSET_USDC_SOLANA, CRYPTO_ASSET_ZEC, CRYPTO_ASSET_XMR, CRYPTO_ASSET_TAO, CRYPTO_ASSET_BTC, CRYPTO_ASSET_ETH, CRYPTO_ASSET_SOL];
  const named = vectors.cryptoAssets.map((vector: { asset: string }) => vector.asset);
  for (const asset of new Set([...coins, ...named])) {
    const expected = asset === CRYPTO_ASSET_TAO ? ["btcli", "Talisman", "Nova", "SubWallet", "Polkadot.js", "Ledger", "Tangem"] : [];
    assert.deepEqual(cryptoWalletSuggestions(asset), expected, `cryptoWalletSuggestions(${JSON.stringify(asset)})`);
  }
  const pinned = new Set(vectors.cryptoWalletSuggestions.map((vector: { asset: string }) => vector.asset));
  for (const asset of coins) assert.ok(pinned.has(asset), `no cryptoWalletSuggestions vector pins ${asset}`);
});

for (const vector of vectors.checkCryptoPaymentUri) {
  test(`checkCryptoPaymentUri: ${vector.name}`, () => {
    assert.equal(checkCryptoPaymentUri(vector.asset, vector.uri, vector.address), vector.expected);
  });
}

for (const vector of vectors.cryptoTermNoticeDue) {
  test(`cryptoTermNoticeDue: ${vector.name}`, () => {
    assert.equal(cryptoTermNoticeDue(vector.term, Date.parse(vector.now)), vector.expected);
  });
}

test("cryptoTermNoticeDue reads the clock by default", () => {
  const hour = 60 * 60 * 1000;
  const iso = (ms: number) => new Date(ms).toISOString();
  assert.equal(
    cryptoTermNoticeDue({ market: "monero", term_end_time: iso(Date.now() - hour), grace_end_time: iso(Date.now() + hour) }),
    true,
  );
});

// Every coin's facts, read through each function the site calls.
test("cryptoAssets", () => {
  for (const vector of vectors.cryptoAssets) {
    const asset = vector.asset;
    const lines = cryptoAssetLinesFor(asset);
    assert.deepEqual(
      {
        asset,
        ticker: cryptoAssetTicker(asset),
        label: cryptoAssetLabel(asset),
        label_key: cryptoAssetLabelKey(asset),
        icon_key: cryptoAssetIconKey(asset),
        decimals: cryptoAssetDecimals(asset),
        quote_decimals: cryptoAssetQuoteDecimals(asset),
        market: cryptoAssetMarket(asset),
        uri_scheme: cryptoPaymentUriScheme(asset),
        tab_id: cryptoPaymentTabId(asset),
        network_line: lines.networkLine,
        network_line_key: lines.networkLineKey,
        network_warning: lines.networkWarning,
        network_warning_key: lines.networkWarningKey,
        no_payment_link_note: lines.noPaymentLinkNote,
        no_payment_link_note_key: lines.noPaymentLinkNoteKey,
        event_product: cryptoPaymentEventProduct(asset),
      },
      vector,
      `the facts of ${JSON.stringify(asset)}`,
    );
  }
});

test("cryptoAssetLinesFor hands out a copy, all six lines", () => {
  const lines = cryptoAssetLinesFor(CRYPTO_ASSET_TAO);
  assert.deepEqual(Object.keys(lines).sort(), [
    "networkLine",
    "networkLineKey",
    "networkWarning",
    "networkWarningKey",
    "noPaymentLinkNote",
    "noPaymentLinkNoteKey",
  ]);
  (lines as { networkLine: string }).networkLine = "changed";
  assert.equal(cryptoAssetLinesFor(CRYPTO_ASSET_TAO).networkLine, "Bittensor network");
  assert.deepEqual(Object.values(cryptoAssetLinesFor("ltc")), ["", "", "", "", "", ""]);
});

// The coin a tap on a tab chooses: the coin last chosen in the tab, else the
// remembered method's, else the tab's first that can be paid with.
for (const vector of vectors.cryptoPaymentTabCoin) {
  test(`cryptoPaymentTabCoin: ${vector.name}`, () => {
    assert.equal(cryptoPaymentTabCoin(vector.tab, vector.last_chosen, vector.remembered), vector.expected);
  });
}

// Over the tabs cryptoPaymentTabs lays out, a surface that keeps the coin last
// chosen per tab gets that coin back on More coins after choosing another tab
// in between; one that kept only the last coin chosen anywhere fell back to TAO.
test("cryptoPaymentTabCoin keeps the coin chosen in the tab", () => {
  const assets = [CRYPTO_ASSET_USDC_SOLANA, CRYPTO_ASSET_TAO, CRYPTO_ASSET_BTC, CRYPTO_ASSET_ETH, CRYPTO_ASSET_SOL].map((asset) => ({
    asset,
    plans: ["yearly"],
  }));
  const tabs = cryptoPaymentTabs(assets, null, ["yearly"]);
  const tab = (id: string) => tabs.find((candidate) => candidate.id === id)!;
  const lastChosen = new Map<string, string>();
  let remembered = CRYPTO_ASSET_USDC_SOLANA;
  const choose = (asset: string) => {
    lastChosen.set(cryptoPaymentTabId(asset), asset);
    remembered = asset;
  };
  const tap = (id: string) => {
    const asset = cryptoPaymentTabCoin(tab(id), lastChosen.get(id) ?? "", remembered);
    if (asset) choose(asset);
    return asset;
  };
  assert.equal(tap(CRYPTO_PAYMENT_TAB_MORE_COINS), CRYPTO_ASSET_TAO);
  choose(CRYPTO_ASSET_BTC);
  assert.equal(tap(CRYPTO_ASSET_USDC_SOLANA), CRYPTO_ASSET_USDC_SOLANA);
  assert.equal(tap(CRYPTO_PAYMENT_TAB_MORE_COINS), CRYPTO_ASSET_BTC);
  assert.equal(cryptoPaymentTabCoin(tab(CRYPTO_PAYMENT_TAB_MORE_COINS), CRYPTO_ASSET_USDC_SOLANA, CRYPTO_ASSET_USDC_SOLANA), CRYPTO_ASSET_TAO);
});

// The memo shows only when the server sent one, whatever the coin.
for (const vector of vectors.cryptoPaymentShowsMemo) {
  test(`cryptoPaymentShowsMemo: ${vector.name}`, () => {
    assert.equal(cryptoPaymentShowsMemo(vector.asset, vector.memo), vector.expected);
  });
}

// Each coin's status steps at each status, a skipped step drawn done. The
// vectors spell the steps as the Go twin's json does.
for (const vector of vectors.cryptoPaymentSteps) {
  test(`cryptoPaymentSteps: ${vector.name}`, () => {
    assert.deepEqual(
      cryptoPaymentSteps(vector.asset, vector.status, vector.confirmations, vector.confirmations_required),
      vector.expected.map((step: { status: string; progress: string; confirmations: number; confirmations_required: number }) => ({
        status: step.status,
        progress: step.progress,
        confirmations: step.confirmations,
        confirmationsRequired: step.confirmations_required,
      })),
    );
  });
}

test("cryptoPaymentSteps draws a skipped step done", () => {
  assert.deepEqual(
    cryptoPaymentSteps(CRYPTO_ASSET_SOL, CRYPTO_PAYMENT_STATUS_PAID, 1, 1).map((step) => step.progress),
    [CRYPTO_PAYMENT_STEP_DONE, CRYPTO_PAYMENT_STEP_DONE, CRYPTO_PAYMENT_STEP_DONE, CRYPTO_PAYMENT_STEP_DONE],
  );
  assert.deepEqual(
    cryptoPaymentSteps(CRYPTO_ASSET_BTC, CRYPTO_PAYMENT_STATUS_SEEN, 0, 1).map((step) => step.progress),
    [CRYPTO_PAYMENT_STEP_DONE, CRYPTO_PAYMENT_STEP_CURRENT, CRYPTO_PAYMENT_STEP_UPCOMING, CRYPTO_PAYMENT_STEP_UPCOMING],
  );
});

// The coin a paid year is named by: the server's asset, never its market.
for (const vector of vectors.cryptoPaidAsset) {
  test(`cryptoPaidAsset: ${vector.name}`, () => {
    assert.equal(cryptoPaidAsset(vector.market, vector.asset), vector.expected);
  });
}

// The vectors and the sdk name the same coins: every coin the method row can
// offer has its facts in the vectors, in the row's order.
test("the cryptoAssets vectors name every coin", () => {
  const known = vectors.cryptoAssets.filter((vector: { ticker: string }) => vector.ticker !== "").map((vector: { asset: string }) => vector.asset);
  const everyCoin = known.map((asset: string) => ({ asset, plans: ["yearly"] }));
  assert.deepEqual(paymentMethods(everyCoin, ["yearly"]), [PAYMENT_METHOD_CARD, ...known]);
});

for (const vector of vectors.cryptoPaymentQrText) {
  test(`cryptoPaymentQrText: ${vector.name}`, () => {
    assert.equal(cryptoPaymentQrText(vector.asset, vector.uri, vector.address), vector.expected);
  });
}

type TabLayout = {
  id: string;
  label: string;
  label_key: string;
  icon_keys: string[];
  coins: { asset: string; label: string; label_key: string; icon_key: string }[];
};

test("the tab layout is the method order", () => {
  const layoutAssets = (vectors.cryptoPaymentTabLayout as TabLayout[]).flatMap((layout) => layout.coins.map((coin) => coin.asset));
  const everyCoin = layoutAssets.map((asset) => ({ asset, plans: ["yearly"] }));
  assert.deepEqual(paymentMethods(everyCoin, ["yearly"]), [PAYMENT_METHOD_CARD, ...layoutAssets]);
});

// Each case's tabs in order, each tab laid out as cryptoPaymentTabLayout says,
// its marks the same whatever is available, with the case's coins available
// and every other coin not.
for (const vector of vectors.cryptoPaymentTabs) {
  test(`cryptoPaymentTabs: ${vector.name}`, () => {
    const tabs = cryptoPaymentTabs(vector.assets, vector.unavailable_assets, vector.items);
    const layouts = vectors.cryptoPaymentTabLayout as TabLayout[];
    assert.deepEqual(
      tabs,
      vector.expected.map((expected: { id: string; available: boolean; available_coins: string[] }) => {
        const layout = layouts.find((candidate) => candidate.id === expected.id)!;
        return {
          id: layout.id,
          label: layout.label,
          labelKey: layout.label_key,
          iconKeys: layout.icon_keys,
          coins: layout.coins.map((coin) => ({
            asset: coin.asset,
            label: coin.label,
            labelKey: coin.label_key,
            iconKey: coin.icon_key,
            available: expected.available_coins.includes(coin.asset),
          })),
          available: expected.available,
        };
      }),
    );
  });
}

// The shape a page renders, for the balance of a server that lists Monero
// and SOL. Nothing in it says to dim a mark: an unavailable coin greys its
// label only.
test("cryptoPaymentTabs, whole", () => {
  assert.deepEqual(
    cryptoPaymentTabs(
      [
        { asset: CRYPTO_ASSET_XMR, plans: ["yearly"], confirmations_required: 1, quote_ttl_seconds: 1800 },
        { asset: CRYPTO_ASSET_SOL, plans: ["yearly"], confirmations_required: 1, quote_ttl_seconds: 1800 },
      ],
      null,
      ["yearly"],
    ),
    [
      {
        id: "usdc_solana",
        label: "USDC on Solana",
        labelKey: "pay_method_usdc_solana",
        iconKeys: ["coin_usdc"],
        coins: [{ asset: "usdc_solana", label: "USDC on Solana", labelKey: "pay_method_usdc_solana", iconKey: "coin_usdc", available: false }],
        available: false,
      },
      {
        id: "zec",
        label: "Zcash",
        labelKey: "pay_method_zcash",
        iconKeys: ["coin_zec"],
        coins: [{ asset: "zec", label: "Zcash", labelKey: "pay_method_zcash", iconKey: "coin_zec", available: false }],
        available: false,
      },
      {
        id: "xmr",
        label: "Monero",
        labelKey: "pay_method_monero",
        iconKeys: ["coin_xmr"],
        coins: [{ asset: "xmr", label: "Monero", labelKey: "pay_method_monero", iconKey: "coin_xmr", available: true }],
        available: true,
      },
      {
        id: "more_coins",
        label: "More coins",
        labelKey: "pay_method_more_coins",
        iconKeys: ["coin_tao", "coin_btc", "coin_eth", "coin_sol"],
        coins: [
          { asset: "tao", label: "TAO", labelKey: "pay_method_tao", iconKey: "coin_tao", available: false },
          { asset: "btc", label: "Bitcoin", labelKey: "pay_method_bitcoin", iconKey: "coin_btc", available: false },
          { asset: "eth", label: "ETH", labelKey: "pay_method_eth", iconKey: "coin_eth", available: false },
          { asset: "sol", label: "SOL", labelKey: "pay_method_sol", iconKey: "coin_sol", available: true },
        ],
        available: true,
      },
    ],
  );
  assert.deepEqual(cryptoPaymentTabs(null, null, ["yearly"]), []);
});

// Every vector set is replayed: an empty one means the file and this test no
// longer agree on its shape.
test("every vector set is read", () => {
  for (const name of [
    "paymentMethods",
    "cryptoAssetSells",
    "paymentMethodAllowsPlan",
    "cryptoAssetTicker",
    "cryptoWalletSuggestions",
    "checkCryptoPaymentUri",
    "cryptoTermNoticeDue",
    "cryptoAssets",
    "cryptoPaymentQrText",
    "cryptoPaymentTabLayout",
    "cryptoPaymentTabs",
    "cryptoPaymentTabCoin",
    "cryptoPaymentShowsMemo",
    "cryptoPaymentSteps",
    "cryptoPaidAsset",
  ]) {
    assert.ok(Array.isArray(vectors[name]) && 0 < vectors[name].length, `no ${name} vectors`);
  }
});
