import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { registerTsResolve } from "./ts-resolve.ts";

// The crypto payment rules replay test/crypto_pay_vectors.json, the vectors
// sdk/crypto_pay_test.go replays against the Go twin, so the two cannot drift
// apart.

registerTsResolve();
const {
  CRYPTO_ASSET_USDC_SOLANA,
  CRYPTO_ASSET_XMR,
  CRYPTO_ASSET_ZEC,
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
  EVENT_STORE_MONERO,
  EVENT_STORE_ZCASH,
  PAYMENT_METHOD_CARD,
  PURCHASE_ERROR_CODE_ASSET_UNAVAILABLE,
  PURCHASE_ERROR_CODE_QUOTE_EXPIRED,
  PURCHASE_ERROR_CODE_UNDERPAID,
  checkCryptoPaymentUri,
  cryptoAssetSells,
  cryptoAssetTicker,
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
      PAYMENT_METHOD_CARD,
      CRYPTO_PAYMENT_STATUS_PENDING,
      CRYPTO_PAYMENT_STATUS_SEEN,
      CRYPTO_PAYMENT_STATUS_CONFIRMING,
      CRYPTO_PAYMENT_STATUS_PAID,
      CRYPTO_PAYMENT_STATUS_EXPIRED,
      CRYPTO_PAYMENT_STATUS_REVIEW,
      CRYPTO_PAYMENT_STATUS_UNKNOWN,
      CRYPTO_PAYMENT_DELIVERY_ACCOUNT,
      CRYPTO_PAYMENT_DELIVERY_NETWORK,
      CRYPTO_PAYMENT_DELIVERY_CODE,
      PURCHASE_ERROR_CODE_ASSET_UNAVAILABLE,
      PURCHASE_ERROR_CODE_QUOTE_EXPIRED,
      PURCHASE_ERROR_CODE_UNDERPAID,
      EVENT_STORE_ZCASH,
      EVENT_STORE_MONERO,
    },
    {
      CRYPTO_ASSET_USDC_SOLANA: "usdc_solana",
      CRYPTO_ASSET_ZEC: "zec",
      CRYPTO_ASSET_XMR: "xmr",
      PAYMENT_METHOD_CARD: "card",
      CRYPTO_PAYMENT_STATUS_PENDING: "pending",
      CRYPTO_PAYMENT_STATUS_SEEN: "seen",
      CRYPTO_PAYMENT_STATUS_CONFIRMING: "confirming",
      CRYPTO_PAYMENT_STATUS_PAID: "paid",
      CRYPTO_PAYMENT_STATUS_EXPIRED: "expired",
      CRYPTO_PAYMENT_STATUS_REVIEW: "review",
      CRYPTO_PAYMENT_STATUS_UNKNOWN: "unknown",
      CRYPTO_PAYMENT_DELIVERY_ACCOUNT: "account",
      CRYPTO_PAYMENT_DELIVERY_NETWORK: "network",
      CRYPTO_PAYMENT_DELIVERY_CODE: "code",
      PURCHASE_ERROR_CODE_ASSET_UNAVAILABLE: "asset_unavailable",
      PURCHASE_ERROR_CODE_QUOTE_EXPIRED: "quote_expired",
      PURCHASE_ERROR_CODE_UNDERPAID: "underpaid",
      EVENT_STORE_ZCASH: "zcash",
      EVENT_STORE_MONERO: "monero",
    },
  );
});

test("the twins are exported from the package's utils", () => {
  for (const name of [
    "paymentMethods",
    "cryptoAssetSells",
    "paymentMethodAllowsPlan",
    "cryptoAssetTicker",
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
  ]) {
    assert.ok(Array.isArray(vectors[name]) && 0 < vectors[name].length, `no ${name} vectors`);
  }
});
