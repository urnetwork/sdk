import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { registerTsResolve } from "./ts-resolve.ts";
import type {
  CreateCryptoPaymentIntentArgs,
  CreateDataCryptoPaymentIntentArgs,
  CryptoPaymentIntentResult,
  CryptoPaymentStatusArgs,
  CryptoPaymentStatusResult,
} from "../src/generated/openapi.ts";
import type { CryptoInvoiceClient, CryptoInvoiceClock, CryptoInvoiceStorage } from "../src/utils/crypto_invoice.ts";

// The invoice state machine on a fake clock and a fake api: the same cases as
// sdk/crypto_invoice_view_controller_test.go, against the TypeScript twin.
// The clock runs the invoice's own timers at their due times, and lets each
// answer land before the next timer; nothing sleeps.

registerTsResolve();
const {
  CRYPTO_INVOICE_PHASE_FAILED,
  CRYPTO_INVOICE_PHASE_NONE,
  CRYPTO_INVOICE_PHASE_OPEN,
  CRYPTO_INVOICE_PHASE_PAID,
  CRYPTO_INVOICE_PHASE_STARTING,
  CryptoInvoice,
} = await import("../src/utils/crypto_invoice.ts");
const { createURNetworkApiClient } = await import("../src/client.ts");

const NETWORK_ID = "00000000-0000-0000-0000-00000000c0f1";
const OTHER_NETWORK_ID = "00000000-0000-0000-0000-00000000c0f2";
const INTENT_ID = "0b6f0b6f-0000-4000-8000-000000000001";
const STATUS_TOKEN = "kq3-synthetic-status-token";
const XMR_ADDRESS = "8BSyntheticXmrSubaddressFixture";
const XMR_URI = `monero:${XMR_ADDRESS}?tx_amount=0.072387&tx_description=URnetwork%20Pro%20yearly`;
const REQUOTED_URI = `monero:${XMR_ADDRESS}?tx_amount=0.071111&tx_description=URnetwork%20Pro%20yearly`;
const SECOND = 1000;
const MINUTE = 60 * SECOND;
const QUOTE_TTL = 30 * MINUTE;
const MAX_AGE = 7 * 24 * 60 * MINUTE;
const T0 = Date.parse("2026-10-20T18:00:00Z");

const rfc3339 = (ms: number) => new Date(ms).toISOString().replace(/\.\d{3}Z$/, "Z");

// The server's xmr yearly invoice, quoted at now.
function testIntent(now: number): CryptoPaymentIntentResult {
  const expiresAt = now + QUOTE_TTL;
  return {
    intent_id: INTENT_ID,
    status_token: STATUS_TOKEN,
    asset: "xmr",
    plan: "yearly",
    delivery: "account",
    tier: "standard",
    price_usd: 40,
    regular_price_usd: 40,
    rate_usd: 555.36,
    address: XMR_ADDRESS,
    amount: "0.072387",
    amount_atomic: "72387000000",
    uri: XMR_URI,
    quoted_at: rfc3339(now),
    expires_at: rfc3339(expiresAt),
    confirmations_required: 1,
    status: testStatus("pending", expiresAt),
  };
}

// The status of the invoice above.
function testStatus(status: CryptoPaymentStatusResult["status"], expiresAt: number): CryptoPaymentStatusResult {
  if (status === "unknown") return { status: "unknown" };
  const result: CryptoPaymentStatusResult = {
    intent_id: INTENT_ID,
    asset: "xmr",
    plan: "yearly",
    delivery: "account",
    status,
    amount: "0.072387",
    amount_atomic: "72387000000",
    received: "0",
    received_atomic: "0",
    remaining: "0.072387",
    remaining_atomic: "72387000000",
    confirmations: 0,
    confirmations_required: 1,
    expires_at: rfc3339(expiresAt),
  };
  switch (status) {
    case "seen":
      Object.assign(result, { received: "0.05", received_atomic: "50000000000", remaining: "0.022387", remaining_atomic: "22387000000" });
      break;
    case "confirming":
    case "paid":
      Object.assign(result, { received: "0.072387", received_atomic: "72387000000", remaining: "0", remaining_atomic: "0" });
      if (status === "paid") {
        Object.assign(result, { confirmations: 1, credited_at: rfc3339(expiresAt - QUOTE_TTL / 2) });
      }
      break;
    case "review":
      Object.assign(result, {
        review_reason: "underpaid",
        received: "0.05",
        received_atomic: "50000000000",
        remaining: "0.022387",
        remaining_atomic: "22387000000",
        error: { code: "underpaid", message: "Underpaid." },
      });
      break;
    case "expired":
      result.error = { code: "quote_expired", message: "The quote expired." };
      break;
  }
  return result;
}

function testRequote(now: number): CryptoPaymentIntentResult {
  const intent = testIntent(now);
  intent.amount = "0.071111";
  intent.amount_atomic = "71111000000";
  intent.rate_usd = 562.5;
  intent.uri = REQUOTED_URI;
  Object.assign(intent.status!, {
    amount: intent.amount,
    amount_atomic: intent.amount_atomic,
    remaining: intent.amount,
    remaining_atomic: intent.amount_atomic,
  });
  return intent;
}

const BALANCE_CODE = "SYNTHETICBALANCECODE0000000";
// the sdk's fixture key, never the merchant
const USDC_ADDRESS = "4wsaez5pzZPytskrQgV4TEc7mBqhT4WStJXfvMKRc2tQ";
const USDC_MEMO = "7cVfgArCheMR6Cs4t6vz5rfnqd56vZq4ndaBrY5xkxXy";
const OTHER_INTENT_ID = "0b6f0b6f-0000-4000-8000-0000000000ff";
const OTHER_STATUS_TOKEN = "kq3-synthetic-status-token-other";

// A 1 TiB data pack bought with USDC for a balance code, quoted at now. USDC
// reports only pending, paid and expired, so its status never says that money
// is on its way.
function testCodeIntent(intentId: string, statusToken: string, now: number): CryptoPaymentIntentResult {
  const expiresAt = now + QUOTE_TTL;
  return {
    intent_id: intentId,
    status_token: statusToken,
    asset: "usdc_solana",
    plan: "",
    item_id: "data_1tib",
    delivery: "code",
    price_usd: 40,
    regular_price_usd: 40,
    rate_usd: 1,
    address: USDC_ADDRESS,
    amount: "40.001234",
    amount_atomic: "40001234",
    memo: USDC_MEMO,
    uri:
      `solana:${USDC_ADDRESS}?amount=40.001234&spl-token=EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v` +
      `&reference=${USDC_MEMO}&label=URnetwork&message=URnetwork%201%20TiB%20data`,
    quoted_at: rfc3339(now),
    expires_at: rfc3339(expiresAt),
    confirmations_required: 1,
    status: testCodeStatus(intentId, "pending", expiresAt),
  };
}

// A 1 TiB data pack for the network "alice", bought without a sign-in and
// paid in Monero, quoted at now.
function testNetworkIntent(intentId: string, statusToken: string, now: number): CryptoPaymentIntentResult {
  const intent = testIntent(now);
  Object.assign(intent, {
    intent_id: intentId,
    status_token: statusToken,
    plan: "",
    item_id: "data_1tib",
    delivery: "network",
    network_name: "alice",
    status: testNetworkStatus(intentId, "pending", now + QUOTE_TTL),
  });
  return intent;
}

// The status of a network invoice above.
function testNetworkStatus(intentId: string, status: CryptoPaymentStatusResult["status"], expiresAt: number): CryptoPaymentStatusResult {
  const result = testStatus(status, expiresAt);
  if (status === "unknown") return result;
  return Object.assign(result, { intent_id: intentId, plan: "", item_id: "data_1tib", delivery: "network", network_name: "alice" });
}

// The status of a balance-code invoice above.
function testCodeStatus(intentId: string, status: CryptoPaymentStatusResult["status"], expiresAt: number): CryptoPaymentStatusResult {
  if (status === "unknown") return { status: "unknown" };
  const result: CryptoPaymentStatusResult = {
    intent_id: intentId,
    asset: "usdc_solana",
    plan: "",
    item_id: "data_1tib",
    delivery: "code",
    status,
    amount: "40.001234",
    amount_atomic: "40001234",
    received: "0",
    received_atomic: "0",
    remaining: "40.001234",
    remaining_atomic: "40001234",
    confirmations: 0,
    confirmations_required: 1,
    expires_at: rfc3339(expiresAt),
  };
  switch (status) {
    case "paid":
      Object.assign(result, {
        received: "40.001234",
        received_atomic: "40001234",
        remaining: "0",
        remaining_atomic: "0",
        confirmations: 1,
        balance_code: BALANCE_CODE,
      });
      break;
    case "seen":
    case "confirming":
    case "review":
      Object.assign(result, { received: "20", received_atomic: "20000000", remaining: "20.001234", remaining_atomic: "20001234" });
      break;
    case "expired":
      result.error = { code: "quote_expired", message: "The quote expired." };
      break;
  }
  return result;
}

// lets every settled answer land
const flush = () => new Promise<void>((resolve) => setImmediate(resolve));

class FakeClock implements CryptoInvoiceClock {
  nowMs: number;
  private seq = 0;
  private readonly timers = new Map<number, { at: number; callback: () => void }>();

  constructor(start: number) {
    this.nowMs = start;
  }

  now(): number {
    return this.nowMs;
  }

  setTimeout(callback: () => void, ms: number): unknown {
    this.seq += 1;
    this.timers.set(this.seq, { at: this.nowMs + ms, callback });
    return this.seq;
  }

  clearTimeout(handle: unknown): void {
    this.timers.delete(handle as number);
  }

  armed(): number {
    return this.timers.size;
  }

  // runs every timer due by now + ms at its own time, in order
  async advance(ms: number): Promise<void> {
    const target = this.nowMs + ms;
    for (;;) {
      await flush();
      let nextId = -1;
      let nextAt = Infinity;
      for (const [id, timer] of this.timers) {
        if (timer.at <= target && timer.at < nextAt) {
          nextId = id;
          nextAt = timer.at;
        }
      }
      if (nextId < 0) break;
      const timer = this.timers.get(nextId)!;
      this.timers.delete(nextId);
      this.nowMs = Math.max(this.nowMs, timer.at);
      timer.callback();
    }
    this.nowMs = target;
    await flush();
  }
}

type Answer<R> = { result?: R; error?: unknown };
type Held<R> = { resolve: (result: R) => void; reject: (error: unknown) => void };

// The api calls the invoice made, answered from the fields, or kept for the
// test to answer later while hold is set.
class FakeClient implements CryptoInvoiceClient {
  intentCalls: CreateCryptoPaymentIntentArgs[] = [];
  dataIntentCalls: CreateDataCryptoPaymentIntentArgs[] = [];
  statusCalls: CryptoPaymentStatusArgs[] = [];
  requoteCalls: CryptoPaymentStatusArgs[] = [];

  intent: Answer<CryptoPaymentIntentResult> = {};
  status: Answer<CryptoPaymentStatusResult> = {};
  requote: Answer<CryptoPaymentIntentResult> = {};

  holdIntent = false;
  holdStatus = false;
  holdRequote = false;
  heldIntent: Held<CryptoPaymentIntentResult>[] = [];
  heldStatus: Held<CryptoPaymentStatusResult>[] = [];
  heldRequote: Held<CryptoPaymentIntentResult>[] = [];

  cryptoPaymentIntent(body: CreateCryptoPaymentIntentArgs): Promise<CryptoPaymentIntentResult> {
    this.intentCalls.push(body);
    return this.answer(this.intent, this.holdIntent, this.heldIntent);
  }

  payDataCryptoIntent(body: CreateDataCryptoPaymentIntentArgs): Promise<CryptoPaymentIntentResult> {
    this.dataIntentCalls.push(body);
    return this.answer(this.intent, this.holdIntent, this.heldIntent);
  }

  cryptoPaymentStatus(body: CryptoPaymentStatusArgs): Promise<CryptoPaymentStatusResult> {
    this.statusCalls.push(body);
    return this.answer(this.status, this.holdStatus, this.heldStatus);
  }

  cryptoPaymentRequote(body: CryptoPaymentStatusArgs): Promise<CryptoPaymentIntentResult> {
    this.requoteCalls.push(body);
    return this.answer(this.requote, this.holdRequote, this.heldRequote);
  }

  private answer<R>(answer: Answer<R>, hold: boolean, held: Held<R>[]): Promise<R> {
    if (hold) {
      return new Promise<R>((resolve, reject) => {
        held.push({ resolve, reject });
      });
    }
    if (answer.error !== undefined) return Promise.reject(answer.error);
    return Promise.resolve(structuredClone(answer.result) as R);
  }
}

class MemoryStorage implements CryptoInvoiceStorage {
  readonly items = new Map<string, string>();

  get length(): number {
    return this.items.size;
  }

  key(index: number): string | null {
    return [...this.items.keys()][index] ?? null;
  }

  getItem(key: string): string | null {
    return this.items.get(key) ?? null;
  }

  setItem(key: string, value: string): void {
    this.items.set(key, value);
  }

  removeItem(key: string): void {
    this.items.delete(key);
  }
}

class Harness {
  readonly clock = new FakeClock(T0);
  readonly client = new FakeClient();
  readonly storage: MemoryStorage | null;
  networkId: string | null = NETWORK_ID;
  changes = 0;
  readonly invoice: InstanceType<typeof CryptoInvoice>;

  constructor(storage: MemoryStorage | null = new MemoryStorage()) {
    this.storage = storage;
    this.invoice = this.newInvoice();
  }

  // another invoice on the same storage, clock and api, as after a reload
  newInvoice(): InstanceType<typeof CryptoInvoice> {
    const invoice = new CryptoInvoice({
      client: this.client,
      storage: this.storage,
      clock: this.clock,
      networkId: () => this.networkId,
    });
    invoice.subscribe(() => {
      this.changes += 1;
    });
    return invoice;
  }

  // active, with the xmr yearly invoice quoted now and its pending status
  // answering every poll
  async startOpenInvoice(): Promise<void> {
    this.client.intent = { result: testIntent(this.clock.now()) };
    this.client.status = { result: testStatus("pending", this.clock.now() + QUOTE_TTL) };
    this.invoice.setActive(true);
    await this.invoice.startInvoice({ asset: "xmr", plan: "yearly", storefrontCountry: "US" });
    assert.equal(this.invoice.getState().phase, CRYPTO_INVOICE_PHASE_OPEN);
  }

  // starts a network invoice without a sign-in, answered at once; returns the
  // quote's end in the server's clock
  async startNetworkInvoice(invoice: InstanceType<typeof CryptoInvoice>, intentId: string, statusToken: string): Promise<number> {
    const intent = testNetworkIntent(intentId, statusToken, this.clock.now());
    this.client.intent = { result: intent };
    await invoice.startDataInvoice({ asset: "xmr", itemId: "data_1tib", networkName: "alice" });
    assert.equal(invoice.getState().phase, CRYPTO_INVOICE_PHASE_OPEN);
    return Date.parse(intent.expires_at!);
  }

  // starts a balance-code invoice without a sign-in, answered at once; returns
  // the quote's end in the server's clock
  async startCodeInvoice(invoice: InstanceType<typeof CryptoInvoice>, intentId: string, statusToken: string): Promise<number> {
    const intent = testCodeIntent(intentId, statusToken, this.clock.now());
    this.client.intent = { result: intent };
    await invoice.startDataInvoice({ asset: "usdc_solana", itemId: "data_1tib" });
    assert.equal(invoice.getState().phase, CRYPTO_INVOICE_PHASE_OPEN);
    return Date.parse(intent.expires_at!);
  }

  statusCount(): number {
    return this.client.statusCalls.length;
  }

  // the invoice as saved for the test network; null when there is none
  saved(networkId: string = NETWORK_ID): Record<string, unknown> | null {
    const text = this.storage!.getItem(`urnCryptoInvoice.${networkId}`);
    return text === null ? null : JSON.parse(text);
  }

  // an invoice bought without a sign-in as saved; null when there is none
  savedOwned(intentId: string): Record<string, unknown> | null {
    const text = this.storage!.getItem(`urnCryptoInvoice.intent.${intentId}`);
    return text === null ? null : JSON.parse(text);
  }
}

// ---- start -------------------------------------------------------------------

test("start shows the quote and saves it", async () => {
  const harness = new Harness();
  const { invoice, client } = harness;
  assert.equal(invoice.getState().phase, CRYPTO_INVOICE_PHASE_NONE);

  client.holdIntent = true;
  invoice.setActive(true);
  const started = invoice.startInvoice({ asset: "xmr", plan: "yearly", storefrontCountry: "US" });
  let state = invoice.getState();
  assert.equal(state.phase, CRYPTO_INVOICE_PHASE_STARTING);
  assert.equal(state.asset, "xmr");
  assert.equal(state.ticker, "XMR");
  assert.deepEqual(client.intentCalls, [{ asset: "xmr", plan: "yearly", storefront_country: "US" }]);
  assert.equal(harness.saved(), null, "an invoice was saved before the server quoted it");

  harness.clock.nowMs += 300;
  client.heldIntent[0].resolve(testIntent(harness.clock.now()));
  await started;
  state = invoice.getState();
  assert.equal(state.phase, CRYPTO_INVOICE_PHASE_OPEN);
  assert.equal(state.intentId, INTENT_ID);
  assert.equal(state.plan, "yearly");
  assert.equal(state.delivery, "account");
  assert.equal(state.address, XMR_ADDRESS);
  assert.equal(state.amount, "0.072387");
  assert.equal(state.amountAtomic, "72387000000");
  assert.equal(state.uri, XMR_URI);
  assert.equal(state.memo, "");
  assert.equal(state.priceUsd, 40);
  assert.equal(state.rateUsd, 555.36);
  assert.equal(state.secondsLeft, 1800);
  assert.equal(state.status, "pending");
  assert.equal(state.remaining, "0.072387");
  assert.equal(state.confirmationsRequired, 1);
  assert.equal(state.refreshing, false);
  assert.equal(state.errorCode, "");

  // the status token is the invoice's secret: saved on the device, never
  // handed to the page
  assert.ok(!JSON.stringify(state).includes(STATUS_TOKEN), "the state shows the status token");
  assert.deepEqual(harness.saved(), {
    network_id: NETWORK_ID,
    intent_id: INTENT_ID,
    status_token: STATUS_TOKEN,
    asset: "xmr",
    plan: "yearly",
    item_id: "",
    created_at: "2026-10-20T18:00:00Z",
    delivery: "account",
    network_name: "",
    address: XMR_ADDRESS,
    amount: "0.072387",
    amount_atomic: "72387000000",
    uri: XMR_URI,
    memo: "",
    price_usd: 40,
    rate_usd: 555.36,
    // the quote in the server's clock, and when it arrived here
    quoted_at: "2026-10-20T18:00:00Z",
    expires_at: "2026-10-20T18:30:00Z",
    received_at: "2026-10-20T18:00:00.300Z",
    status: "",
    balance_code: "",
    hidden: false,
  });
});

test("start failures", async (t) => {
  await t.test("refused", async () => {
    const harness = new Harness();
    harness.client.intent = { result: { error: { code: "asset_unavailable", message: "Monero is temporarily unavailable." } } };
    harness.invoice.setActive(true);
    await harness.invoice.startInvoice({ asset: "xmr", plan: "yearly" });
    const state = harness.invoice.getState();
    assert.equal(state.phase, CRYPTO_INVOICE_PHASE_FAILED);
    assert.equal(state.errorCode, "asset_unavailable");
    assert.equal(state.errorMessage, "Monero is temporarily unavailable.");
    assert.equal(harness.saved(), null, "a refused invoice was saved");
    await harness.clock.advance(MINUTE);
    assert.equal(harness.statusCount(), 0);
  });
  await t.test("transport", async () => {
    const harness = new Harness();
    harness.client.intent = { error: new Error("synthetic network down") };
    await harness.invoice.startInvoice({ asset: "zec", itemId: "data_1tib" });
    const state = harness.invoice.getState();
    assert.equal(state.phase, CRYPTO_INVOICE_PHASE_FAILED);
    assert.equal(state.errorCode, "");
    assert.equal(state.errorMessage, "synthetic network down");
  });
  await t.test("no token", async () => {
    const harness = new Harness();
    const intent = testIntent(harness.clock.now());
    intent.status_token = "";
    harness.client.intent = { result: intent };
    await harness.invoice.startInvoice({ asset: "xmr", plan: "yearly" });
    assert.equal(harness.invoice.getState().phase, CRYPTO_INVOICE_PHASE_FAILED);
    assert.equal(harness.saved(), null, "an invoice without a token was saved");
  });
  await t.test("an older hidden invoice stays saved", async () => {
    const harness = new Harness();
    await harness.startOpenInvoice();
    harness.client.status = { result: testStatus("seen", harness.clock.now() + QUOTE_TTL) };
    await harness.clock.advance(5 * SECOND);
    harness.invoice.dismiss();

    harness.client.intent = { result: { error: { code: "asset_unavailable", message: "No." } } };
    await harness.invoice.startInvoice({ asset: "zec", plan: "yearly" });
    assert.equal(harness.invoice.getState().phase, CRYPTO_INVOICE_PHASE_FAILED);
    assert.equal(harness.invoice.resume(), true, "the hidden invoice was lost to a refused start");
    assert.equal(harness.invoice.getState().intentId, INTENT_ID);
  });
});

test("a uri that does not pay the address is never shown", async () => {
  const harness = new Harness();
  const intent = testIntent(harness.clock.now());
  intent.uri = "monero:8BSyntheticXmrSubaddressOther?tx_amount=0.072387";
  harness.client.intent = { result: intent };
  await harness.invoice.startInvoice({ asset: "xmr", plan: "yearly" });
  const state = harness.invoice.getState();
  assert.equal(state.phase, CRYPTO_INVOICE_PHASE_OPEN);
  assert.equal(state.address, XMR_ADDRESS);
  assert.equal(state.uri, "");
  assert.equal(harness.saved()!.uri, "");
});

// ---- polling -----------------------------------------------------------------

// 5 s while a payment may be on its way, 30 s once expired, 60 s in review,
// and never once paid.
test("poll cadence", async () => {
  const harness = new Harness();
  await harness.startOpenInvoice();
  const expiresAt = harness.clock.now() + QUOTE_TTL;
  assert.equal(harness.statusCount(), 0);

  await harness.clock.advance(4999);
  assert.equal(harness.statusCount(), 0);
  harness.client.status = { result: testStatus("seen", expiresAt) };
  await harness.clock.advance(1);
  assert.equal(harness.statusCount(), 1);
  assert.deepEqual(harness.client.statusCalls[0], { intent_id: INTENT_ID, status_token: STATUS_TOKEN });
  let state = harness.invoice.getState();
  assert.equal(state.status, "seen");
  assert.equal(state.received, "0.05");
  assert.equal(state.remaining, "0.022387");

  harness.client.status = { result: testStatus("confirming", expiresAt) };
  await harness.clock.advance(4999);
  assert.equal(harness.statusCount(), 1);
  await harness.clock.advance(1);
  assert.equal(harness.statusCount(), 2);
  assert.equal(harness.invoice.getState().status, "confirming");

  harness.client.status = { result: testStatus("expired", expiresAt) };
  await harness.clock.advance(5 * SECOND);
  assert.equal(harness.statusCount(), 3);
  assert.equal(harness.invoice.getState().status, "expired");

  harness.client.status = { result: testStatus("review", expiresAt) };
  await harness.clock.advance(29 * SECOND);
  assert.equal(harness.statusCount(), 3);
  await harness.clock.advance(SECOND);
  assert.equal(harness.statusCount(), 4);
  state = harness.invoice.getState();
  assert.equal(state.phase, CRYPTO_INVOICE_PHASE_OPEN);
  assert.equal(state.status, "review");
  assert.equal(state.reviewReason, "underpaid");

  harness.client.status = { result: testStatus("paid", expiresAt) };
  await harness.clock.advance(59 * SECOND);
  assert.equal(harness.statusCount(), 4);
  await harness.clock.advance(SECOND);
  assert.equal(harness.statusCount(), 5);
  assert.equal(harness.invoice.getState().phase, CRYPTO_INVOICE_PHASE_PAID);

  await harness.clock.advance(10 * MINUTE);
  assert.equal(harness.statusCount(), 5);
  assert.equal(harness.clock.armed(), 0);
});

test("a failed poll retries on its cadence", async () => {
  const harness = new Harness();
  await harness.startOpenInvoice();
  harness.client.status = { error: new Error("429 Too many status checks") };
  await harness.clock.advance(5 * SECOND);
  assert.equal(harness.statusCount(), 1);
  const state = harness.invoice.getState();
  assert.equal(state.phase, CRYPTO_INVOICE_PHASE_OPEN);
  assert.equal(state.status, "pending");
  assert.equal(state.errorCode, "");
  await harness.clock.advance(5 * SECOND);
  assert.equal(harness.statusCount(), 2);
});

// The countdown ticks once a second, announces each second once, and stops at 0.
test("ticks once a second", async () => {
  const harness = new Harness();
  await harness.startOpenInvoice();
  assert.equal(harness.invoice.getState().secondsLeft, 1800);

  let changes = harness.changes;
  await harness.clock.advance(SECOND);
  assert.equal(harness.invoice.getState().secondsLeft, 1799);
  assert.equal(harness.changes, changes + 1);
  await harness.clock.advance(500);
  assert.equal(harness.changes, changes + 1);
  // rounded up: 1798.5 s left shows as 1799
  assert.equal(harness.invoice.getState().secondsLeft, 1799);
  await harness.clock.advance(500);
  assert.equal(harness.invoice.getState().secondsLeft, 1798);
  assert.equal(harness.changes, changes + 2);

  await harness.clock.advance(Date.parse("2026-10-20T18:29:59Z") - harness.clock.now());
  assert.equal(harness.invoice.getState().secondsLeft, 1);
  await harness.clock.advance(SECOND);
  assert.equal(harness.invoice.getState().secondsLeft, 0);
  changes = harness.changes;
  // only the poll is armed now: the countdown is done
  assert.equal(harness.clock.armed(), 1);
  await harness.clock.advance(SECOND);
  assert.equal(harness.changes, changes);
  assert.equal(harness.invoice.getState().secondsLeft, 0);
});

// Hidden, nothing runs: no poll and no tick, however long. Visible again, it
// polls at once.
test("nothing runs while inactive", async () => {
  const harness = new Harness();
  await harness.startOpenInvoice();
  harness.invoice.setActive(false);
  assert.equal(harness.clock.armed(), 0, "an inactive invoice armed a timer");
  const changes = harness.changes;
  await harness.clock.advance(10 * MINUTE);
  assert.equal(harness.statusCount(), 0);
  assert.equal(harness.changes, changes);

  harness.invoice.setActive(true);
  assert.equal(harness.statusCount(), 1);
  assert.equal(harness.invoice.getState().secondsLeft, 1200);
  assert.notEqual(harness.changes, changes, "the countdown's new second was not announced");
});

// An answer that was in flight when the invoice went inactive still lands.
test("an answer in flight lands after setActive(false)", async () => {
  const harness = new Harness();
  await harness.startOpenInvoice();
  harness.client.holdStatus = true;
  await harness.clock.advance(5 * SECOND);
  assert.equal(harness.statusCount(), 1);
  harness.invoice.setActive(false);
  harness.client.heldStatus[0].resolve(testStatus("confirming", harness.clock.now() + MINUTE));
  await flush();
  assert.equal(harness.invoice.getState().status, "confirming");
  await harness.clock.advance(MINUTE);
  assert.equal(harness.statusCount(), 1);
});

// ---- endings -----------------------------------------------------------------

// Paid is delivered once and then forgotten: no saved invoice and no poll.
test("paid is forgotten once shown", async () => {
  const harness = new Harness();
  await harness.startOpenInvoice();
  harness.client.status = { result: testStatus("paid", harness.clock.now() + QUOTE_TTL) };
  const changes = harness.changes;
  await harness.clock.advance(5 * SECOND);
  const state = harness.invoice.getState();
  assert.equal(state.phase, CRYPTO_INVOICE_PHASE_PAID);
  assert.equal(state.status, "paid");
  assert.equal(state.secondsLeft, 0);
  assert.notEqual(harness.changes, changes, "paid was not announced");
  assert.equal(harness.saved(), null, "a paid invoice stayed saved");
  // on screen until dismissed
  assert.equal(harness.invoice.resume(), true);
  harness.invoice.dismiss();
  assert.equal(harness.invoice.getState().phase, CRYPTO_INVOICE_PHASE_NONE);
  assert.equal(harness.invoice.resume(), false);
});

// A paid balance code stays saved, under a key of its own, until it is
// dismissed after it was shown, so it can be shown again after a reload.
test("a balance code stays saved until dismissed", async () => {
  const harness = new Harness();
  harness.networkId = null;
  harness.invoice.setActive(true);
  const expiresAt = await harness.startCodeInvoice(harness.invoice, INTENT_ID, STATUS_TOKEN);
  assert.deepEqual(harness.client.dataIntentCalls, [
    { asset: "usdc_solana", item_id: "data_1tib", network_name: "", email: "" },
  ]);
  harness.client.status = { result: testCodeStatus(INTENT_ID, "paid", expiresAt) };
  await harness.clock.advance(5 * SECOND);
  const state = harness.invoice.getState();
  assert.equal(state.phase, CRYPTO_INVOICE_PHASE_PAID);
  assert.equal(state.balanceCode, BALANCE_CODE);
  const saved = harness.savedOwned(INTENT_ID);
  assert.notEqual(saved, null, "the paid code was forgotten before it was dismissed");
  assert.equal(saved!.status, "paid");
  assert.equal(saved!.balance_code, BALANCE_CODE);
  assert.equal(harness.saved("anon"), null, "a code invoice was saved as anonymous");

  // shown again at once after a reload, signed in or not, without a poll
  harness.networkId = NETWORK_ID;
  const reloaded = harness.newInvoice();
  assert.equal(reloaded.resume(), true);
  assert.equal(reloaded.getState().phase, CRYPTO_INVOICE_PHASE_PAID);
  assert.equal(reloaded.getState().balanceCode, BALANCE_CODE);
  assert.equal(harness.statusCount(), 1);
  reloaded.dismiss();
  assert.equal(harness.savedOwned(INTENT_ID), null, "the dismissed code stayed saved");
});

// The server no longer knows the invoice: it fails, and is forgotten.
test("unknown is forgotten", async () => {
  const harness = new Harness();
  await harness.startOpenInvoice();
  harness.client.status = { result: testStatus("unknown", 0) };
  await harness.clock.advance(5 * SECOND);
  const state = harness.invoice.getState();
  assert.equal(state.phase, CRYPTO_INVOICE_PHASE_FAILED);
  assert.equal(state.status, "unknown");
  assert.equal(harness.saved(), null, "an unknown invoice stayed saved");
  await harness.clock.advance(MINUTE);
  assert.equal(harness.statusCount(), 1);
});

// ---- dismiss and resume ------------------------------------------------------

for (const status of ["pending", "expired"] as const) {
  test(`dismiss with nothing received forgets it: ${status}`, async () => {
    const harness = new Harness();
    await harness.startOpenInvoice();
    harness.client.status = { result: testStatus(status, harness.clock.now() + QUOTE_TTL) };
    await harness.clock.advance(5 * SECOND);
    harness.invoice.dismiss();
    assert.equal(harness.invoice.getState().phase, CRYPTO_INVOICE_PHASE_NONE);
    assert.equal(harness.saved(), null, "a dismissed unpaid invoice stayed saved");
    assert.equal(harness.invoice.resume(), false);
    await harness.clock.advance(MINUTE);
    assert.equal(harness.statusCount(), 1);
  });
}

// Once money arrived, dismiss only hides the invoice: resume brings it back,
// in this instance or after a reload, and polls it at once.
for (const status of ["seen", "confirming", "review"] as const) {
  test(`dismiss after money arrived only hides it: ${status}`, async () => {
    const harness = new Harness();
    await harness.startOpenInvoice();
    harness.client.status = { result: testStatus(status, harness.clock.now() + QUOTE_TTL) };
    await harness.clock.advance(5 * SECOND);
    harness.invoice.dismiss();
    assert.equal(harness.invoice.getState().phase, CRYPTO_INVOICE_PHASE_NONE);
    assert.notEqual(harness.saved(), null, "dismissing an invoice money arrived for forgot it");
    await harness.clock.advance(MINUTE);
    assert.equal(harness.statusCount(), 1);

    assert.equal(harness.invoice.resume(), true);
    const state = harness.invoice.getState();
    assert.equal(state.phase, CRYPTO_INVOICE_PHASE_OPEN);
    assert.equal(state.status, "");
    assert.equal(state.address, XMR_ADDRESS);
    assert.equal(state.uri, XMR_URI);
    assert.equal(harness.statusCount(), 2);
    await flush();
    assert.equal(harness.invoice.getState().status, status);

    const reloaded = harness.newInvoice();
    assert.equal(reloaded.resume(), true);
    assert.equal(reloaded.getState().intentId, INTENT_ID);
    // a resumed invoice with no status yet may have money on it
    reloaded.dismiss();
    assert.notEqual(harness.saved(), null, "dismissing a resumed invoice before its first poll forgot it");
  });
}

test("resume reads only the signed-in network", async () => {
  const harness = new Harness();
  await harness.startOpenInvoice();

  harness.networkId = OTHER_NETWORK_ID;
  assert.equal(harness.newInvoice().resume(), false);
  harness.networkId = null;
  assert.equal(harness.newInvoice().resume(), false);
  harness.networkId = NETWORK_ID;
  const reloaded = harness.newInvoice();
  assert.equal(reloaded.resume(), true);
  const state = reloaded.getState();
  assert.equal(state.phase, CRYPTO_INVOICE_PHASE_OPEN);
  assert.equal(state.intentId, INTENT_ID);
  assert.equal(state.amount, "0.072387");
  assert.equal(state.ticker, "XMR");
  assert.equal(state.secondsLeft, 1800);
});

test("resume drops a week-old invoice", async () => {
  const harness = new Harness();
  await harness.startOpenInvoice();
  harness.invoice.setActive(false);

  harness.clock.nowMs += MAX_AGE;
  assert.equal(harness.newInvoice().resume(), true);
  harness.clock.nowMs += SECOND;
  assert.equal(harness.newInvoice().resume(), false);
  assert.equal(harness.saved(), null, "a week-old invoice stayed saved");
});

test("resume ignores an unreadable record", () => {
  const harness = new Harness();
  harness.storage!.setItem(`urnCryptoInvoice.${NETWORK_ID}`, "{not json");
  assert.equal(harness.invoice.resume(), false);
  harness.storage!.setItem(`urnCryptoInvoice.${NETWORK_ID}`, JSON.stringify({ intent_id: 7 }));
  assert.equal(harness.invoice.resume(), false);
  assert.equal(harness.invoice.getState().phase, CRYPTO_INVOICE_PHASE_NONE);
});

// The answer to a start that was dismissed is dropped.
test("dismiss while starting drops the answer", async () => {
  const harness = new Harness();
  harness.client.holdIntent = true;
  harness.invoice.setActive(true);
  const started = harness.invoice.startInvoice({ asset: "xmr", plan: "yearly" });
  harness.invoice.dismiss();
  harness.client.heldIntent[0].resolve(testIntent(harness.clock.now()));
  await started;
  assert.equal(harness.invoice.getState().phase, CRYPTO_INVOICE_PHASE_NONE);
  assert.equal(harness.saved(), null, "a dismissed start was saved");
  await harness.clock.advance(MINUTE);
  assert.equal(harness.statusCount(), 0);
});

// An answer that lands after a sign-out never writes the invoice back.
test("never saves for a network that is not signed in", async () => {
  const harness = new Harness();
  await harness.startOpenInvoice();
  // a sign-out clears the saved invoice and the credential
  harness.storage!.removeItem(`urnCryptoInvoice.${NETWORK_ID}`);
  harness.networkId = null;
  // an answer that changes what is saved: the quote changed elsewhere
  harness.client.status = { result: testRequote(harness.clock.now()).status! };
  await harness.clock.advance(5 * SECOND);
  assert.equal(harness.invoice.getState().amount, "0.071111");
  assert.equal(harness.saved(), null, "the invoice was written back after a sign-out");
  assert.equal(harness.saved("anon"), null, "the invoice was saved as anonymous after a sign-out");
});

// Without a storage the invoice lives in memory: resume still brings a hidden
// one back.
test("without storage keeps the invoice in memory", async () => {
  const harness = new Harness(null);
  await harness.startOpenInvoice();
  harness.client.status = { result: testStatus("seen", harness.clock.now() + QUOTE_TTL) };
  await harness.clock.advance(5 * SECOND);
  harness.invoice.dismiss();
  assert.equal(harness.invoice.resume(), true);
  assert.equal(harness.invoice.getState().intentId, INTENT_ID);
  // a new instance has a memory of its own
  assert.equal(harness.newInvoice().resume(), false);
});

// A storage that throws on every access (a blocked or full localStorage) costs
// the saving, never the invoice.
test("a storage that throws is survived", async () => {
  const throwing: CryptoInvoiceStorage = {
    getItem: () => {
      throw new Error("SecurityError");
    },
    setItem: () => {
      throw new Error("QuotaExceededError");
    },
    removeItem: () => {
      throw new Error("SecurityError");
    },
  };
  const harness = new Harness();
  const invoice = new CryptoInvoice({ client: harness.client, storage: throwing, clock: harness.clock, networkId: () => NETWORK_ID });
  harness.client.intent = { result: testIntent(harness.clock.now()) };
  harness.client.status = { result: testStatus("paid", harness.clock.now() + QUOTE_TTL) };
  invoice.setActive(true);
  await invoice.startInvoice({ asset: "xmr", plan: "yearly" });
  assert.equal(invoice.getState().phase, CRYPTO_INVOICE_PHASE_OPEN);
  await harness.clock.advance(5 * SECOND);
  assert.equal(invoice.getState().phase, CRYPTO_INVOICE_PHASE_PAID);
  invoice.dismiss();
  assert.equal(invoice.resume(), false);
  invoice.setPaymentMethod("xmr");
  assert.equal(invoice.getPaymentMethod(), "");
});

// ---- requote -----------------------------------------------------------------

for (const status of ["pending", "expired"] as const) {
  test(`requote: ${status}`, async () => {
    const harness = new Harness();
    await harness.startOpenInvoice();
    harness.client.status = { result: testStatus(status, harness.clock.now() + QUOTE_TTL) };
    await harness.clock.advance(31 * MINUTE);
    assert.equal(harness.invoice.getState().secondsLeft, 0);

    harness.client.holdRequote = true;
    const changes = harness.changes;
    const requoted = harness.invoice.requote();
    assert.equal(harness.invoice.getState().refreshing, true);
    assert.equal(harness.changes, changes + 1);
    assert.deepEqual(harness.client.requoteCalls, [{ intent_id: INTENT_ID, status_token: STATUS_TOKEN }]);
    // one at a time, and no poll while it is in flight
    await harness.invoice.requote();
    assert.equal(harness.client.requoteCalls.length, 1);
    const polls = harness.statusCount();
    await harness.clock.advance(MINUTE);
    assert.equal(harness.statusCount(), polls);

    const requote = testRequote(harness.clock.now());
    harness.client.status = { result: requote.status! };
    harness.client.heldRequote[0].resolve(requote);
    await requoted;
    const state = harness.invoice.getState();
    assert.equal(state.refreshing, false);
    assert.equal(state.status, "pending");
    assert.equal(state.address, XMR_ADDRESS);
    assert.equal(state.amount, "0.071111");
    assert.equal(state.rateUsd, 562.5);
    assert.equal(state.secondsLeft, 1800);
    assert.equal(state.uri, REQUOTED_URI);
    const saved = harness.saved()!;
    assert.equal(saved.amount, "0.071111");
    assert.equal(saved.expires_at, rfc3339(harness.clock.now() + QUOTE_TTL));
    // the invoice is as old as it was
    assert.equal(saved.created_at, "2026-10-20T18:00:00Z");

    // the new quote is polled at once, and the countdown and the polls start
    // over from it
    assert.equal(harness.statusCount(), polls + 1);
    await flush();
    assert.equal(harness.invoice.getState().status, "pending");
    await harness.clock.advance(SECOND);
    assert.equal(harness.invoice.getState().secondsLeft, 1799);
    await harness.clock.advance(4 * SECOND);
    assert.equal(harness.statusCount(), polls + 2);
  });
}

test("requote only with nothing received", async () => {
  for (const status of ["seen", "confirming", "review"] as const) {
    const harness = new Harness();
    await harness.startOpenInvoice();
    harness.client.status = { result: testStatus(status, harness.clock.now() + QUOTE_TTL) };
    await harness.clock.advance(5 * SECOND);
    await harness.invoice.requote();
    assert.equal(harness.client.requoteCalls.length, 0);
    assert.equal(harness.invoice.getState().refreshing, false);
  }
});

test("a requote failure keeps the invoice", async (t) => {
  await t.test("refused", async () => {
    const harness = new Harness();
    await harness.startOpenInvoice();
    harness.client.requote = { result: { error: { code: "asset_unavailable", message: "Monero is temporarily unavailable." } } };
    await harness.invoice.requote();
    let state = harness.invoice.getState();
    assert.equal(state.phase, CRYPTO_INVOICE_PHASE_OPEN);
    assert.equal(state.refreshing, false);
    assert.equal(state.errorCode, "asset_unavailable");
    assert.equal(state.errorMessage, "Monero is temporarily unavailable.");
    assert.equal(state.amount, "0.072387");
    assert.equal(state.uri, XMR_URI);

    // the next try clears the error
    const requote = testRequote(harness.clock.now());
    harness.client.requote = { result: requote };
    harness.client.status = { result: requote.status! };
    await harness.invoice.requote();
    state = harness.invoice.getState();
    assert.equal(state.errorCode, "");
    assert.equal(state.amount, "0.071111");
  });
  await t.test("transport", async () => {
    const harness = new Harness();
    await harness.startOpenInvoice();
    harness.client.requote = { error: new Error("synthetic network down") };
    await harness.invoice.requote();
    const state = harness.invoice.getState();
    assert.equal(state.refreshing, false);
    assert.equal(state.errorCode, "");
    assert.equal(state.errorMessage, "synthetic network down");
    assert.equal(state.amount, "0.072387");
  });
});

// A poll sent for the old quote cannot undo the new one.
test("requote drops a stale poll", async () => {
  const harness = new Harness();
  await harness.startOpenInvoice();
  const staleStatus = testStatus("pending", harness.clock.now() + QUOTE_TTL);
  harness.client.holdStatus = true;
  await harness.clock.advance(5 * SECOND);
  assert.equal(harness.statusCount(), 1);

  harness.client.requote = { result: testRequote(harness.clock.now()) };
  await harness.invoice.requote();
  assert.equal(harness.invoice.getState().amount, "0.071111");
  harness.client.heldStatus[0].resolve(staleStatus);
  await flush();
  const state = harness.invoice.getState();
  assert.equal(state.amount, "0.071111");
  assert.equal(state.uri, REQUOTED_URI);
});

// Another device requoted this network's invoice: the uri asks for the old
// amount, so it is dropped and the new amount shown.
test("a quote changed elsewhere drops the uri", async () => {
  const harness = new Harness();
  await harness.startOpenInvoice();
  const requoted = testRequote(harness.clock.now() + 2 * MINUTE).status!;
  harness.client.status = { result: requoted };
  await harness.clock.advance(5 * SECOND);
  const state = harness.invoice.getState();
  assert.equal(state.amount, "0.071111");
  assert.equal(state.amountAtomic, "71111000000");
  assert.equal(state.uri, "");
  assert.equal(state.address, XMR_ADDRESS);
  assert.equal(state.expiresAt, requoted.expires_at);
  assert.equal(state.secondsLeft, 1915);
  const saved = harness.saved()!;
  assert.equal(saved.amount, "0.071111");
  assert.equal(saved.uri, "");
});

// ---- payment method ------------------------------------------------------------

test("the payment method is remembered", () => {
  const harness = new Harness();
  assert.equal(harness.invoice.getPaymentMethod(), "");
  harness.invoice.setPaymentMethod("xmr");
  assert.equal(harness.invoice.getPaymentMethod(), "xmr");
  assert.equal(harness.storage!.getItem(`urnPaymentMethod.${NETWORK_ID}`), '"xmr"');

  // read again after a reload; a method the sdk does not know is ignored
  const reloaded = harness.newInvoice();
  assert.equal(reloaded.getPaymentMethod(), "xmr");
  reloaded.setPaymentMethod("btc");
  assert.equal(reloaded.getPaymentMethod(), "xmr");
  reloaded.setPaymentMethod("card");
  assert.equal(harness.invoice.getPaymentMethod(), "card");
  reloaded.setPaymentMethod("");
  assert.equal(reloaded.getPaymentMethod(), "");
  assert.equal(harness.storage!.getItem(`urnPaymentMethod.${NETWORK_ID}`), null);

  // a value written by something else that is not a method reads as none
  harness.storage!.setItem(`urnPaymentMethod.${NETWORK_ID}`, '"btc"');
  assert.equal(reloaded.getPaymentMethod(), "");

  const memory = new Harness(null);
  memory.invoice.setPaymentMethod("zec");
  assert.equal(memory.invoice.getPaymentMethod(), "zec");
});

// ---- lifecycle -----------------------------------------------------------------

// A closed invoice runs nothing and drops the answer still in flight.
test("close drops answers in flight", async () => {
  const harness = new Harness();
  await harness.startOpenInvoice();
  harness.client.holdStatus = true;
  await harness.clock.advance(5 * SECOND);
  assert.equal(harness.statusCount(), 1);
  harness.invoice.close();
  assert.equal(harness.clock.armed(), 0);
  const changes = harness.changes;
  harness.client.heldStatus[0].resolve(testStatus("paid", harness.clock.now() + QUOTE_TTL));
  await flush();
  assert.equal(harness.invoice.getState().status, "pending");
  assert.equal(harness.changes, changes);
  assert.notEqual(harness.saved(), null, "close dropped the saved invoice");
});

// React's useSyncExternalStore needs the same snapshot until something changes.
test("getState returns the same object until a change", async () => {
  const harness = new Harness();
  await harness.startOpenInvoice();
  const state = harness.invoice.getState();
  assert.equal(harness.invoice.getState(), state);
  assert.ok(Object.isFrozen(state));
  await harness.clock.advance(SECOND);
  assert.notEqual(harness.invoice.getState(), state);
});

// The generated api client is an invoice client.
test("the generated client serves the invoice", async () => {
  const calls: Array<{ url: string; authorization: string | null; body: unknown }> = [];
  const client = createURNetworkApiClient({
    baseURL: "https://api.example",
    token: "synthetic-token",
    retry: false,
    fetch: async (input, init) => {
      calls.push({
        url: String(input),
        authorization: new Headers(init?.headers).get("authorization"),
        body: init?.body ? JSON.parse(String(init.body)) : undefined,
      });
      return new Response(JSON.stringify(testIntent(T0)), { headers: { "Content-Type": "application/json" } });
    },
  });
  const invoice = new CryptoInvoice({ client, storage: null, clock: new FakeClock(T0) });
  await invoice.startInvoice({ asset: "xmr", plan: "yearly" });
  assert.equal(invoice.getState().phase, CRYPTO_INVOICE_PHASE_OPEN);
  await invoice.startDataInvoice({ asset: "zec", itemId: "data_1tib", networkName: "alice" });
  assert.deepEqual(calls, [
    { url: "https://api.example/pay/crypto/intent", authorization: "Bearer synthetic-token", body: { asset: "xmr", plan: "yearly" } },
    // the buy-data invoice needs no sign-in, and is sent without one
    {
      url: "https://api.example/pay/data/crypto-intent",
      authorization: null,
      body: { asset: "zec", item_id: "data_1tib", network_name: "alice", email: "" },
    },
  ]);
});

// ---- the pre-merge review (mmm/ZCASH-CONTRACT.md §4.4.2) --------------------------------

// the Dismiss and countdown vectors sdk/crypto_invoice_view_controller_test.go
// replays against the Go twin
const vectors = JSON.parse(readFileSync(new URL("./crypto_pay_vectors.json", import.meta.url), "utf8"));

test("the invoice vectors are read", () => {
  assert.ok(0 < vectors.invoiceDismiss.length);
  assert.ok(0 < vectors.invoiceCountdown.length);
});

// §4.4.2.1: dismiss drops an account or network invoice with nothing
// received, and a code invoice only once it is paid and its code was shown.
// Otherwise an account invoice is kept for the next resume, and one bought
// without a sign-in (network or code) is hidden.
for (const vector of vectors.invoiceDismiss) {
  test(`dismiss: ${vector.name}`, async () => {
    const harness = new Harness();
    harness.invoice.setActive(true);
    let saved: () => Record<string, unknown> | null;
    if (vector.delivery === "code") {
      const expiresAt = await harness.startCodeInvoice(harness.invoice, INTENT_ID, STATUS_TOKEN);
      harness.client.status = { result: testCodeStatus(INTENT_ID, vector.status, expiresAt) };
      saved = () => harness.savedOwned(INTENT_ID);
    } else if (vector.delivery === "network") {
      const expiresAt = await harness.startNetworkInvoice(harness.invoice, INTENT_ID, STATUS_TOKEN);
      harness.client.status = { result: testNetworkStatus(INTENT_ID, vector.status, expiresAt) };
      saved = () => harness.savedOwned(INTENT_ID);
    } else {
      harness.client.intent = { result: testIntent(harness.clock.now()) };
      await harness.invoice.startInvoice({ asset: "xmr", plan: "yearly" });
      harness.client.status = { result: testStatus(vector.status, harness.clock.now() + QUOTE_TTL) };
      saved = () => harness.saved();
    }
    await harness.clock.advance(5 * SECOND);
    assert.equal(harness.invoice.getState().status, vector.status);
    assert.notEqual(saved(), null, "the invoice was not saved before the dismiss");

    harness.invoice.dismiss();
    assert.equal(harness.invoice.getState().phase, CRYPTO_INVOICE_PHASE_NONE);
    const record = saved();
    if (vector.expected === "drop") {
      assert.equal(record, null, "the dismissed invoice stayed saved");
    } else if (vector.expected === "keep") {
      assert.notEqual(record, null, "the dismissed account invoice was not kept");
      assert.equal(record!.hidden, false);
      assert.equal(harness.newInvoice().resume(), true);
    } else {
      assert.notEqual(record, null, "the dismissed invoice was dropped; it may still pay");
      assert.equal(record!.hidden, true);
    }
  });
}

// §4.4.2.1: a code invoice dismissed while its status still read pending (the
// buyer had sent USDC; USDC goes from pending straight to paid) is polled once
// at the next resume, and shown with its code when it turns out paid.
for (const answer of ["paid", "pending", "unknown"] as const) {
  test(`a hidden code invoice is checked once and shown when paid: ${answer}`, async () => {
    const harness = new Harness();
    harness.networkId = null;
    harness.invoice.setActive(true);
    const expiresAt = await harness.startCodeInvoice(harness.invoice, INTENT_ID, STATUS_TOKEN);
    harness.client.status = { result: testCodeStatus(INTENT_ID, "pending", expiresAt) };
    await harness.clock.advance(5 * SECOND);
    harness.invoice.dismiss();
    assert.equal(harness.savedOwned(INTENT_ID)!.hidden, true);

    harness.client.status = { result: testCodeStatus(INTENT_ID, answer, expiresAt) };
    const reloaded = harness.newInvoice();
    const polls = harness.statusCount();
    assert.equal(reloaded.resume(), false);
    assert.equal(harness.statusCount(), polls + 1);
    assert.deepEqual(harness.client.statusCalls[polls], { intent_id: INTENT_ID, status_token: STATUS_TOKEN });
    await flush();
    const state = reloaded.getState();
    const saved = harness.savedOwned(INTENT_ID);
    if (answer === "paid") {
      assert.equal(state.phase, CRYPTO_INVOICE_PHASE_PAID);
      assert.equal(state.balanceCode, BALANCE_CODE);
      assert.equal(saved!.status, "paid");
      assert.equal(saved!.balance_code, BALANCE_CODE);
      assert.equal(saved!.hidden, false);
    } else if (answer === "pending") {
      // still hidden, still saved: it may yet pay
      assert.equal(state.phase, CRYPTO_INVOICE_PHASE_NONE);
      assert.equal(saved!.hidden, true);
    } else {
      assert.equal(saved, null, "an invoice the server no longer knows stayed saved");
    }
    // once per resume, not on a cadence
    reloaded.setActive(true);
    await harness.clock.advance(MINUTE);
    assert.equal(harness.statusCount(), polls + 1);
  });
}

// §4.4.2.2: each code invoice has a record of its own, so two tabs never
// overwrite each other's, and resume shows a paid one first.
test("code invoices are saved per intent", async () => {
  const harness = new Harness();
  harness.networkId = null;
  const tab1 = harness.invoice;
  tab1.setActive(true);
  const expiresAt = await harness.startCodeInvoice(tab1, INTENT_ID, STATUS_TOKEN);
  const tab2 = harness.newInvoice();
  await harness.startCodeInvoice(tab2, OTHER_INTENT_ID, OTHER_STATUS_TOKEN);
  assert.notEqual(harness.savedOwned(INTENT_ID), null);
  assert.notEqual(harness.savedOwned(OTHER_INTENT_ID), null);

  harness.client.status = { result: testCodeStatus(INTENT_ID, "paid", expiresAt) };
  await harness.clock.advance(5 * SECOND);
  assert.equal(tab1.getState().balanceCode, BALANCE_CODE);
  assert.equal(harness.savedOwned(INTENT_ID)!.status, "paid");
  assert.equal(harness.savedOwned(OTHER_INTENT_ID)!.status, "pending");

  // a reload shows the paid one first, and checks the other once
  harness.client.status = { result: testCodeStatus(OTHER_INTENT_ID, "pending", expiresAt) };
  const reloaded = harness.newInvoice();
  const polls = harness.statusCount();
  assert.equal(reloaded.resume(), true);
  let state = reloaded.getState();
  assert.equal(state.intentId, INTENT_ID);
  assert.equal(state.phase, CRYPTO_INVOICE_PHASE_PAID);
  assert.equal(state.balanceCode, BALANCE_CODE);
  assert.equal(harness.statusCount(), polls + 1);
  assert.equal(harness.client.statusCalls[polls].intent_id, OTHER_INTENT_ID);
  await flush();

  // the shown code is dismissed and dropped; the other comes next
  reloaded.dismiss();
  assert.equal(harness.savedOwned(INTENT_ID), null, "the dismissed code stayed saved");
  assert.equal(reloaded.resume(), true);
  state = reloaded.getState();
  assert.equal(state.intentId, OTHER_INTENT_ID);
  assert.equal(state.phase, CRYPTO_INVOICE_PHASE_OPEN);
  assert.equal(state.status, "");
});

// §4.4.2.2: the network's slot is compare-and-delete. One tab's answer for its
// invoice never deletes the invoice another tab saved there since.
for (const ending of ["unknown", "dismiss"] as const) {
  test(`the network's slot is compare-and-delete: ${ending}`, async () => {
    const harness = new Harness();
    await harness.startOpenInvoice();
    const tab1 = harness.invoice;

    const tab2 = harness.newInvoice();
    const other = testIntent(harness.clock.now());
    other.intent_id = OTHER_INTENT_ID;
    other.status_token = OTHER_STATUS_TOKEN;
    other.status!.intent_id = OTHER_INTENT_ID;
    harness.client.intent = { result: other };
    await tab2.startInvoice({ asset: "xmr", plan: "yearly" });
    assert.equal(harness.saved()!.intent_id, OTHER_INTENT_ID);

    if (ending === "unknown") {
      harness.client.status = { result: testStatus("unknown", 0) };
      await harness.clock.advance(5 * SECOND);
      assert.equal(tab1.getState().phase, CRYPTO_INVOICE_PHASE_FAILED);
    } else {
      // nothing received on tab 1's invoice: dismiss drops it, but only from
      // where it still is
      tab1.dismiss();
    }
    assert.equal(harness.saved()?.intent_id, OTHER_INTENT_ID, "tab 1's ending deleted tab 2's invoice");

    // tab 2's invoice receives money and stays saved for a reload
    const seen = testStatus("seen", harness.clock.now() + QUOTE_TTL);
    seen.intent_id = OTHER_INTENT_ID;
    harness.client.status = { result: seen };
    tab2.setActive(true);
    await flush();
    assert.equal(tab2.getState().status, "seen");
    const reloaded = harness.newInvoice();
    assert.equal(reloaded.resume(), true);
    assert.equal(reloaded.getState().intentId, OTHER_INTENT_ID);
  });
}

// §4.4.2.1: a saved code invoice is dropped at the 7-day cap, unpolled.
test("a code invoice is dropped at the 7-day cap", async () => {
  const harness = new Harness();
  await harness.startCodeInvoice(harness.invoice, INTENT_ID, STATUS_TOKEN);
  harness.invoice.dismiss();
  assert.equal(harness.savedOwned(INTENT_ID)!.hidden, true);

  harness.clock.nowMs += MAX_AGE + SECOND;
  const polls = harness.statusCount();
  assert.equal(harness.newInvoice().resume(), false);
  assert.equal(harness.savedOwned(INTENT_ID), null, "a week-old code invoice stayed saved");
  assert.equal(harness.statusCount(), polls);
});

// §4.4.2.3: the countdown runs the quote's own duration from when the quote
// arrived, whatever the device clock says about the server's.
for (const vector of vectors.invoiceCountdown) {
  test(`countdown: ${vector.name}`, async () => {
    const harness = new Harness();
    harness.clock.nowMs = Date.parse(vector.received_at);
    const intent = testIntent(harness.clock.now());
    intent.quoted_at = vector.quoted_at;
    intent.expires_at = vector.expires_at;
    intent.status!.expires_at = vector.expires_at;
    harness.client.intent = { result: intent };
    harness.invoice.setActive(true);
    await harness.invoice.startInvoice({ asset: "xmr", plan: "yearly" });
    harness.client.status = { result: intent.status! };
    await harness.clock.advance(vector.after_ms);
    assert.equal(harness.invoice.getState().secondsLeft, vector.expected);
    const saved = harness.saved()!;
    assert.equal(saved.quoted_at, vector.quoted_at);
    assert.equal(saved.received_at, new Date(Date.parse(vector.received_at)).toISOString());
  });
}

// §4.4.2.3: the arrival time is saved, so a resumed countdown keeps the
// quote's duration, and a quote moved elsewhere moves by the server's own
// difference.
test("a resumed countdown keeps the quote's duration", async () => {
  const harness = new Harness();
  // the device clock runs 31 minutes fast
  const serverNow = harness.clock.now();
  harness.clock.nowMs = serverNow + 31 * MINUTE;
  harness.client.intent = { result: testIntent(serverNow) };
  await harness.invoice.startInvoice({ asset: "xmr", plan: "yearly" });
  assert.equal(harness.invoice.getState().secondsLeft, 1800);

  harness.clock.nowMs += 10 * MINUTE;
  const reloaded = harness.newInvoice();
  assert.equal(reloaded.resume(), true);
  assert.equal(reloaded.getState().secondsLeft, 1200);

  // requoted on another device: two minutes later in the server's clock
  harness.client.status = { result: testRequote(serverNow + 2 * MINUTE).status! };
  reloaded.setActive(true);
  await flush();
  assert.equal(reloaded.getState().secondsLeft, 1320);
});

// The server's expired ends the countdown, whatever is left of it here: an
// open invoice handed out again keeps its first quote time, so its count from
// arrival runs long.
test("the server's expired ends the countdown", async () => {
  const harness = new Harness();
  harness.client.intent = { result: testIntent(harness.clock.now() - 10 * MINUTE) };
  harness.client.status = { result: testStatus("pending", harness.clock.now() + 20 * MINUTE) };
  harness.invoice.setActive(true);
  await harness.invoice.startInvoice({ asset: "xmr", plan: "yearly" });
  assert.equal(harness.invoice.getState().secondsLeft, 1800);

  await harness.clock.advance(20 * MINUTE - 5 * SECOND);
  assert.equal(harness.invoice.getState().secondsLeft, 605);
  harness.client.status = { result: testStatus("expired", harness.clock.now() + 5 * SECOND) };
  await harness.clock.advance(5 * SECOND);
  const state = harness.invoice.getState();
  assert.equal(state.status, "expired");
  assert.equal(state.secondsLeft, 0);
  await harness.clock.advance(SECOND);
  assert.equal(harness.invoice.getState().secondsLeft, 0);
});

// §4.4.2.4: a poll in flight when a requote starts answers for the old quote
// and is dropped; the new quote is polled as soon as it lands.
test("a requote drops the poll in flight", async () => {
  const harness = new Harness();
  await harness.startOpenInvoice();
  harness.client.holdStatus = true;
  await harness.clock.advance(5 * SECOND);
  assert.equal(harness.client.heldStatus.length, 1);

  harness.client.holdRequote = true;
  const requoted = harness.invoice.requote();
  // the poll answers first, for the old quote: dropped
  harness.client.heldStatus[0].resolve(testStatus("seen", harness.clock.now() + QUOTE_TTL));
  await flush();
  let state = harness.invoice.getState();
  assert.equal(state.status, "pending");
  assert.equal(state.refreshing, true);

  // the requote lands, and its quote is polled at once
  const requote = testRequote(harness.clock.now());
  const seen = testStatus("seen", harness.clock.now() + QUOTE_TTL);
  Object.assign(seen, { amount: requote.amount, amount_atomic: requote.amount_atomic });
  harness.client.holdStatus = false;
  harness.client.status = { result: seen };
  const polls = harness.statusCount();
  harness.client.heldRequote[0].resolve(requote);
  await requoted;
  assert.equal(harness.statusCount(), polls + 1);
  await flush();
  state = harness.invoice.getState();
  assert.equal(state.amount, "0.071111");
  assert.equal(state.status, "seen");
});

// §4.4.2.4: a requote's answer never moves a status past pending or expired
// back to pending.
test("a requote never makes received money payable", async () => {
  const harness = new Harness();
  await harness.startOpenInvoice();
  harness.client.holdRequote = true;
  const requoted = harness.invoice.requote();
  // money seen by now, however that reached the state
  (harness.invoice as unknown as { state: { status: string } }).state.status = "seen";
  harness.client.holdStatus = true;
  harness.client.heldRequote[0].resolve(testRequote(harness.clock.now()));
  await requoted;
  const state = harness.invoice.getState();
  assert.equal(state.refreshing, false);
  assert.equal(state.status, "seen");
});

// §4.4.2.5: nothing is written after close(), so a sign-out that closes the
// invoice and then clears the keys leaves them clear.
test("nothing is written after close", async () => {
  const harness = new Harness();
  await harness.startOpenInvoice();
  harness.client.holdStatus = true;
  await harness.clock.advance(5 * SECOND);
  harness.invoice.close();
  // the sign-out clears the keys once the invoice is closed
  harness.storage!.items.clear();
  harness.client.heldStatus[0].resolve(testRequote(harness.clock.now()).status!);
  await flush();
  harness.invoice.dismiss();
  harness.invoice.setPaymentMethod("xmr");
  await harness.invoice.startInvoice({ asset: "xmr", plan: "yearly" });
  await harness.invoice.requote();
  assert.equal(harness.invoice.resume(), false);
  assert.equal(harness.storage!.items.size, 0, "something was written after close");
});

// §4.4.2.6: an empty network id is signed out: an invoice is neither saved
// for it nor resumed.
test("an empty network id neither saves nor resumes", async () => {
  const harness = new Harness();
  harness.networkId = null;
  await harness.startOpenInvoice();
  assert.equal(harness.storage!.items.size, 0, "an invoice was saved for no network");
  assert.equal(harness.newInvoice().resume(), false);

  // nor is a leftover saved under an empty id resumed
  harness.storage!.setItem(
    "urnCryptoInvoice.",
    JSON.stringify({ network_id: "", intent_id: INTENT_ID, status_token: STATUS_TOKEN, asset: "xmr", created_at: rfc3339(harness.clock.now()), delivery: "account" }),
  );
  harness.storage!.setItem(
    "urnCryptoInvoice.anon",
    JSON.stringify({ network_id: "", intent_id: INTENT_ID, status_token: STATUS_TOKEN, asset: "xmr", created_at: rfc3339(harness.clock.now()), delivery: "account" }),
  );
  assert.equal(harness.newInvoice().resume(), false);
});

// §4.4.2.6: resume checks the network of the invoice it already shows: one
// from before a sign-out, or of another account, is put away; a code invoice
// belongs to no network and stays.
test("resume puts away another network's invoice", async () => {
  const harness = new Harness();
  await harness.startOpenInvoice();
  harness.networkId = OTHER_NETWORK_ID;
  const changes = harness.changes;
  assert.equal(harness.invoice.resume(), false);
  assert.equal(harness.invoice.getState().phase, CRYPTO_INVOICE_PHASE_NONE);
  assert.notEqual(harness.changes, changes, "putting the invoice away was not announced");
  // still saved for its own network
  harness.networkId = NETWORK_ID;
  assert.equal(harness.invoice.resume(), true);
  assert.equal(harness.invoice.getState().intentId, INTENT_ID);

  await harness.startCodeInvoice(harness.invoice, OTHER_INTENT_ID, OTHER_STATUS_TOKEN);
  harness.networkId = null;
  assert.equal(harness.invoice.resume(), true);
  assert.equal(harness.invoice.getState().intentId, OTHER_INTENT_ID);
});

// §4.4.2.7: an answer that cannot be read is no answer. A requote without an
// intent id, an address or an amount leaves the invoice as it was; a start
// without them fails; a status without one reads as "" (not yet known).
for (const name of ["empty", "no intent id", "another intent", "no address", "no amount"] as const) {
  test(`malformed answers: requote ${name}`, async () => {
    const harness = new Harness();
    await harness.startOpenInvoice();
    const before = harness.saved();
    let requote: CryptoPaymentIntentResult = testRequote(harness.clock.now());
    if (name === "empty") requote = {};
    if (name === "no intent id") delete requote.intent_id;
    if (name === "another intent") requote.intent_id = OTHER_INTENT_ID;
    if (name === "no address") delete requote.address;
    if (name === "no amount") delete requote.amount_atomic;
    harness.client.requote = { result: requote };
    await harness.invoice.requote();
    const state = harness.invoice.getState();
    assert.equal(state.refreshing, false);
    assert.equal(state.errorCode, "");
    assert.equal(state.errorMessage, "no quote");
    assert.equal(state.address, XMR_ADDRESS);
    assert.equal(state.amount, "0.072387");
    assert.equal(state.uri, XMR_URI);
    assert.deepEqual(harness.saved(), before);
  });
}

test("malformed answers: a start without an address", async () => {
  const harness = new Harness();
  const intent = testIntent(harness.clock.now());
  delete intent.address;
  harness.client.intent = { result: intent };
  await harness.invoice.startInvoice({ asset: "xmr", plan: "yearly" });
  assert.equal(harness.invoice.getState().phase, CRYPTO_INVOICE_PHASE_FAILED);
  assert.equal(harness.invoice.getState().errorMessage, "no invoice");
  assert.equal(harness.saved(), null, "an invoice that cannot be paid was saved");
});

test("malformed answers: a status without a status", async () => {
  const harness = new Harness();
  await harness.startOpenInvoice();
  harness.client.status = { result: {} as CryptoPaymentStatusResult };
  await harness.clock.advance(5 * SECOND);
  assert.equal(harness.invoice.getState().status, "");
  // not yet known counts as money that may have arrived
  harness.invoice.dismiss();
  assert.notEqual(harness.saved(), null, "an invoice of unknown status was dropped");
});

// An invoice for a named network bought without a sign-in (a gift, or an
// owner not signed in in this browser) belongs to the browser: it is saved
// whoever is signed in, and a reload brings it back to see it through.
test("a network invoice bought without a sign-in survives a reload", async () => {
  const harness = new Harness();
  harness.networkId = null;
  harness.invoice.setActive(true);
  const expiresAt = await harness.startNetworkInvoice(harness.invoice, INTENT_ID, STATUS_TOKEN);
  assert.deepEqual(harness.client.dataIntentCalls, [{ asset: "xmr", item_id: "data_1tib", network_name: "alice", email: "" }]);
  const saved = harness.savedOwned(INTENT_ID);
  assert.notEqual(saved, null, "the network invoice bought without a sign-in was not saved");
  assert.equal(saved!.delivery, "network");
  assert.equal(saved!.network_name, "alice");

  // a reload, signed in to another network by now
  harness.networkId = OTHER_NETWORK_ID;
  const reloaded = harness.newInvoice();
  assert.equal(reloaded.resume(), true);
  const state = reloaded.getState();
  assert.equal(state.intentId, INTENT_ID);
  assert.equal(state.networkName, "alice");
  assert.equal(state.status, "");
  assert.equal(harness.saved(OTHER_NETWORK_ID), null, "a network invoice was saved in the signed-in network's slot");
  // it stays on screen whoever signs in or out
  harness.networkId = null;
  assert.equal(reloaded.resume(), true);

  // paid: shown once, then done, its data on the network
  harness.client.status = { result: testNetworkStatus(INTENT_ID, "paid", expiresAt) };
  reloaded.setActive(true);
  await flush();
  assert.equal(reloaded.getState().phase, CRYPTO_INVOICE_PHASE_PAID);
  assert.equal(harness.savedOwned(INTENT_ID), null, "a paid network invoice stayed saved");
});

// A start made without a sign-in stays on screen across a sign-in while it
// waits for its answer.
test("a start without a sign-in survives a sign-in in flight", async () => {
  const harness = new Harness();
  harness.networkId = null;
  harness.client.holdIntent = true;
  const started = harness.invoice.startDataInvoice({ asset: "xmr", itemId: "data_1tib", networkName: "alice" });
  harness.networkId = NETWORK_ID;
  assert.equal(harness.invoice.resume(), true);
  harness.client.heldIntent[0].resolve(testNetworkIntent(INTENT_ID, STATUS_TOKEN, harness.clock.now()));
  await started;
  assert.equal(harness.invoice.getState().phase, CRYPTO_INVOICE_PHASE_OPEN);
  assert.notEqual(harness.savedOwned(INTENT_ID), null);
});

// A hidden network invoice is polled once at the next resume: paid, its data
// is on the network and it is dropped, as it is when the server no longer
// knows it; otherwise it stays hidden.
for (const answer of ["paid", "confirming", "unknown"] as const) {
  test(`a hidden network invoice is checked once: ${answer}`, async () => {
    const harness = new Harness();
    harness.networkId = null;
    harness.invoice.setActive(true);
    const expiresAt = await harness.startNetworkInvoice(harness.invoice, INTENT_ID, STATUS_TOKEN);
    harness.client.status = { result: testNetworkStatus(INTENT_ID, "seen", expiresAt) };
    await harness.clock.advance(5 * SECOND);
    harness.invoice.dismiss();
    assert.equal(harness.savedOwned(INTENT_ID)!.hidden, true);

    harness.client.status = { result: testNetworkStatus(INTENT_ID, answer, expiresAt) };
    const reloaded = harness.newInvoice();
    const polls = harness.statusCount();
    assert.equal(reloaded.resume(), false);
    assert.equal(harness.statusCount(), polls + 1);
    await flush();
    assert.equal(reloaded.getState().phase, CRYPTO_INVOICE_PHASE_NONE);
    const saved = harness.savedOwned(INTENT_ID);
    if (answer === "confirming") {
      assert.equal(saved?.hidden, true, "the hidden network invoice was not kept hidden");
    } else {
      assert.equal(saved, null, `a ${answer} network invoice stayed saved`);
    }
  });
}

// resume's order: a paid code not yet dismissed, then an invoice bought
// without a sign-in that was on screen, then the signed-in network's own.
test("resume order", async () => {
  const harness = new Harness();
  await harness.startOpenInvoice();
  harness.clock.nowMs += SECOND;

  const network = harness.newInvoice();
  await harness.startNetworkInvoice(network, OTHER_INTENT_ID, OTHER_STATUS_TOKEN);
  harness.clock.nowMs += SECOND;

  const codeIntentId = "0b6f0b6f-0000-4000-8000-0000000000c0";
  const code = harness.newInvoice();
  const codeExpiresAt = await harness.startCodeInvoice(code, codeIntentId, "kq3-synthetic-status-token-code");
  harness.client.status = { result: testCodeStatus(codeIntentId, "paid", codeExpiresAt) };
  code.setActive(true);
  await flush();
  assert.equal(code.getState().phase, CRYPTO_INVOICE_PHASE_PAID);
  code.close();

  harness.client.status = { result: testNetworkStatus(OTHER_INTENT_ID, "pending", codeExpiresAt) };
  const reloaded = harness.newInvoice();
  assert.equal(reloaded.resume(), true);
  assert.equal(reloaded.getState().intentId, codeIntentId);
  assert.equal(reloaded.getState().balanceCode, BALANCE_CODE);
  await flush();

  reloaded.dismiss();
  assert.equal(reloaded.resume(), true);
  assert.equal(reloaded.getState().intentId, OTHER_INTENT_ID);

  // nothing received: the network invoice is dropped, and the account's own
  // comes next
  reloaded.setActive(true);
  await flush();
  assert.equal(reloaded.getState().status, "pending");
  reloaded.dismiss();
  assert.equal(harness.savedOwned(OTHER_INTENT_ID), null, "a dismissed network invoice with nothing received stayed saved");
  assert.equal(reloaded.resume(), true);
  assert.equal(reloaded.getState().intentId, INTENT_ID);
});
