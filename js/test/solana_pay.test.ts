import { test } from "node:test";
import assert from "node:assert/strict";
import {
  SOLANA_PAY_REFERENCE_BYTES,
  buildSolanaPaymentUrl,
  createPaymentReference,
  decodeBase58,
  encodeBase58,
  isValidPaymentReference,
} from "../src/utils/solana_pay.ts";

// These pin the same rules as sdk/solana_pay_test.go. Every bug they cover was
// shipped, and none of them threw at the time -- a Solana Pay payment that cannot
// be credited looks exactly like a successful one from the client's side.

const USDC_MINT = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";
const RECIPIENT = "4Fj9RCwJqHLdLNK28DwWHunHqWapxKbbzeYZLmreSYCM";

const validArgs = () => ({
  recipient: RECIPIENT,
  amountUsd: 5,
  splTokenMint: USDC_MINT,
  reference: createPaymentReference(),
  label: "URnetwork",
  message: "UR Pro - Monthly",
});

// base58 must agree byte for byte with sdk/base58.go, because the reference this
// produces is matched against what the chain reports.
test("base58 round trips, including leading zeros", () => {
  const cases: Uint8Array[] = [
    new Uint8Array([0]),
    new Uint8Array([0, 0, 0, 1]),
    new Uint8Array(32),
    Uint8Array.from({ length: 32 }, (_, i) => i),
    Uint8Array.from({ length: 32 }, () => 255),
  ];
  for (const bytes of cases) {
    const encoded = encodeBase58(bytes);
    const decoded = decodeBase58(encoded);
    assert.ok(decoded, `decode failed for ${encoded}`);
    assert.deepEqual([...decoded!], [...bytes]);
  }
});

test("base58 known vector matches the go implementation", () => {
  // 32 zero bytes encode to 32 leading '1' characters.
  assert.equal(encodeBase58(new Uint8Array(32)), "1".repeat(32));
  assert.equal(decodeBase58("")!.length, 0);
  // characters outside the alphabet are rejected, not coerced
  assert.equal(decodeBase58("0OIl"), null);
});

// THE BUG: the web app used crypto.randomUUID() with the dashes stripped, a
// 32-character hex string, where a base58 32-byte public key is required.
test("createPaymentReference is a solana public key", () => {
  for (let i = 0; i < 200; i++) {
    const ref = createPaymentReference();
    const decoded = decodeBase58(ref);
    assert.ok(decoded, `not base58: ${ref}`);
    assert.equal(decoded!.length, SOLANA_PAY_REFERENCE_BYTES, `wrong length: ${ref}`);
    assert.ok(ref.length >= 43 && ref.length <= 44, `${ref} is ${ref.length} chars`);
    assert.ok(!/[0OIl]/.test(ref), `${ref} has a non-base58 character`);
    assert.ok(isValidPaymentReference(ref));
  }
});

test("the old hex uuid reference is rejected", () => {
  for (const ref of [
    "708c762b9f6c44138861a59cdafdfb37",
    "00000000000000000000000000000000",
    "ffffffffffffffffffffffffffffffff",
  ]) {
    assert.equal(isValidPaymentReference(ref), false, `accepted ${ref}`);
    assert.throws(() => buildSolanaPaymentUrl({ ...validArgs(), reference: ref }));
  }
});

test("references do not collide", () => {
  const seen = new Set<string>();
  for (let i = 0; i < 5000; i++) {
    const ref = createPaymentReference();
    assert.ok(!seen.has(ref), `duplicate after ${i}`);
    seen.add(ref);
  }
});

test("malformed references are rejected", () => {
  for (const ref of [
    "",
    "abc",
    encodeBase58(new Uint8Array(31)),
    encodeBase58(new Uint8Array(33)),
    "0Fj9RCwJqHLdLNK28DwWHunHqWapxKbbzeYZLmreSYCM",
    "OFj9RCwJqHLdLNK28DwWHunHqWapxKbbzeYZLmreSYCM",
    " " + RECIPIENT,
  ]) {
    assert.equal(isValidPaymentReference(ref), false, `accepted ${JSON.stringify(ref)}`);
  }
});

test("url carries every parameter the wallet needs", () => {
  const args = validArgs();
  const raw = buildSolanaPaymentUrl(args);
  assert.ok(raw.startsWith(`solana:${RECIPIENT}?`), raw);

  const q = new URLSearchParams(raw.split("?")[1]);
  assert.equal(q.get("amount"), "5");
  assert.equal(q.get("spl-token"), USDC_MINT);
  assert.equal(q.get("reference"), args.reference);
  assert.equal(q.get("label"), "URnetwork");
  assert.equal(q.get("message"), "UR Pro - Monthly");
});

// THE BUG: Android hardcoded amount=40, so the monthly plan could not be sold and
// the price no longer had to agree with the server's quote. The webhook checks the
// payment against the intent, so a disagreement means the money is never credited.
test("amount comes from the caller, in decimal notation", () => {
  for (const amount of [5, 40, 4.5, 0.01, 12.34]) {
    const raw = buildSolanaPaymentUrl({ ...validArgs(), amountUsd: amount });
    const q = new URLSearchParams(raw.split("?")[1]);
    assert.equal(q.get("amount"), String(amount));
  }
});

test("amount is never scientific notation", () => {
  for (const amount of [0.000001, 1e21, 0.0000005]) {
    let raw: string;
    try {
      raw = buildSolanaPaymentUrl({ ...validArgs(), amountUsd: amount });
    } catch {
      continue; // rejecting outright is also acceptable
    }
    const q = new URLSearchParams(raw.split("?")[1]);
    assert.ok(!/[eE]/.test(q.get("amount")!), `${amount} became ${q.get("amount")}`);
  }
});

// A zero price is the most dangerous input here: the webhook's check is
// `amount >= price - tolerance`, satisfied at zero by any payment, including none.
test("zero, negative and non-finite amounts are rejected", () => {
  for (const amount of [0, -1, -0.01, NaN, Infinity]) {
    assert.throws(
      () => buildSolanaPaymentUrl({ ...validArgs(), amountUsd: amount }),
      `amount ${amount} accepted`,
    );
  }
});

test("malformed addresses are rejected", () => {
  const bad = ["", "not-an-address", "0OIl", "a".repeat(44), RECIPIENT + "x"];
  for (const v of bad) {
    assert.throws(() => buildSolanaPaymentUrl({ ...validArgs(), recipient: v }), `recipient ${v}`);
    assert.throws(() => buildSolanaPaymentUrl({ ...validArgs(), splTokenMint: v }), `mint ${v}`);
  }
});

// An unescaped '&' or '#' in a plan name would truncate the query and drop the
// reference -- the same silent failure as a malformed reference.
test("label and message are escaped without losing the reference", () => {
  const args = { ...validArgs(), label: "UR & Co #1", message: "UR Pro — Yearly (100% off?)" };
  const raw = buildSolanaPaymentUrl(args);
  const q = new URLSearchParams(raw.split("?")[1]);
  assert.equal(q.get("label"), args.label);
  assert.equal(q.get("message"), args.message);
  assert.equal(q.get("reference"), args.reference);
});

test("empty label and message are omitted rather than sent blank", () => {
  const raw = buildSolanaPaymentUrl({ ...validArgs(), label: "", message: "" });
  const q = new URLSearchParams(raw.split("?")[1]);
  assert.equal(q.has("label"), false);
  assert.equal(q.has("message"), false);
});

// The end-to-end shape an app must follow: quote from the server, then build the
// url from that quote. A refused quote must not produce a url.
test("a refused quote cannot produce a payment url", () => {
  const refused = { error: { message: "Unknown plan." } } as { amount_usd?: number };
  assert.throws(() =>
    buildSolanaPaymentUrl({ ...validArgs(), amountUsd: refused.amount_usd ?? 0 }),
  );
});
