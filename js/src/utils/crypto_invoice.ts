/**
 * The crypto invoice's state machine, for the web.
 *
 * This is the TypeScript twin of `sdk/crypto_invoice_view_controller.go`
 * (mmm/ZCASH-CONTRACT.md §4.4), with the same behavior and the same tests on a fake
 * clock and a fake api (test/crypto_invoice.test.ts mirrors
 * crypto_invoice_view_controller_test.go, and both replay the invoice vectors
 * of test/crypto_pay_vectors.json). It owns the quote, the countdown, the
 * requote, the status and the saved invoice; a page renders getState() and
 * calls the actions.
 *
 * Active (setActive(true)) means the invoice is on screen: it polls the
 * invoice's status (every 5 s while a payment may be on its way, every 30 s
 * once the quote expired, every 60 s in review, never once paid or unknown)
 * and ticks secondsLeft once a second. Inactive, nothing runs: there is no
 * background check. An answer already in flight still lands; after close()
 * nothing lands and nothing is written.
 *
 * An account invoice is saved in `storage` (localStorage by default) under
 * `urnCryptoInvoice.<networkId>`, for the signed-in network only. An invoice
 * bought without a sign-in (startDataInvoice: delivery network or code)
 * belongs to this browser, not to a network, and is saved under
 * `urnCryptoInvoice.intent.<intentId>`, whoever is signed in. Either is kept
 * from the moment the server quotes it until it is paid and that was shown
 * once, the server no longer knows it, or it is 7 days old. A dismiss with
 * nothing received drops an account or network invoice; once money arrived it
 * only puts it away (an account invoice comes back at the next resume, one
 * bought without a sign-in is hidden). A balance-code invoice is never dropped
 * while it might still pay: only by a dismiss after its code was shown, and
 * any other dismiss only hides it. Closing a panel is never a dismiss: call
 * close().
 */

import type {
  CreateCryptoPaymentIntentArgs,
  CreateDataCryptoPaymentIntentArgs,
  CryptoAsset,
  CryptoPaymentIntentResult,
  CryptoPaymentStatusArgs,
  CryptoPaymentStatusResult,
} from "../generated/openapi";
import {
  CRYPTO_PAYMENT_DELIVERY_CODE,
  CRYPTO_PAYMENT_DELIVERY_NETWORK,
  CRYPTO_PAYMENT_STATUS_CONFIRMING,
  CRYPTO_PAYMENT_STATUS_EXPIRED,
  CRYPTO_PAYMENT_STATUS_PAID,
  CRYPTO_PAYMENT_STATUS_PENDING,
  CRYPTO_PAYMENT_STATUS_REVIEW,
  CRYPTO_PAYMENT_STATUS_SEEN,
  CRYPTO_PAYMENT_STATUS_UNKNOWN,
  PAYMENT_METHOD_CARD,
  checkCryptoPaymentUri,
  cryptoAssetTicker,
} from "./crypto_pay";
import { parseRfc3339 } from "./rfc3339";

// The invoice's phases (`CryptoInvoiceState.phase`).

/** no invoice on screen: none was started, or it was dismissed */
export const CRYPTO_INVOICE_PHASE_NONE = "none";
/** waiting for the server's quote */
export const CRYPTO_INVOICE_PHASE_STARTING = "starting";
/** an invoice that is not paid yet: `status` says whether it waits, confirms, expired or is in review */
export const CRYPTO_INVOICE_PHASE_OPEN = "open";
/** credited */
export const CRYPTO_INVOICE_PHASE_PAID = "paid";
/** it could not be started (`errorCode`), or the server no longer knows it (`status` unknown) */
export const CRYPTO_INVOICE_PHASE_FAILED = "failed";

/**
 * The storage key prefixes: a network's invoice is `<prefix>.<networkId>`, an
 * invoice bought without a sign-in (delivery network or code)
 * `<prefix>.intent.<intentId>`, the payment method `<prefix>.<networkId|anon>`.
 * A sign-out may clear the network's keys; it must not clear the
 * `<prefix>.intent.` ones, which belong to whoever holds the browser (for a
 * code, they are the only way back to it).
 */
export const CRYPTO_INVOICE_STORAGE_KEY = "urnCryptoInvoice";
export const CRYPTO_PAYMENT_METHOD_STORAGE_KEY = "urnPaymentMethod";

const OWNED_KEY_PREFIX = `${CRYPTO_INVOICE_STORAGE_KEY}.intent.`;
const MAX_AGE_MS = 7 * 24 * 60 * 60 * 1000;
const TICK_MS = 1000;
const ACTIVE_POLL_MS = 5000;
const EXPIRED_POLL_MS = 30000;
const REVIEW_POLL_MS = 60000;

/** What an invoice panel renders. Amounts are exact decimal strings in the coin; show them as they are, beside `ticker`. */
export interface CryptoInvoiceState {
  /** one of the CRYPTO_INVOICE_PHASE_* values */
  readonly phase: string;
  readonly intentId: string;
  readonly asset: string;
  readonly plan: string;
  readonly itemId: string;
  readonly delivery: string;
  readonly networkName: string;
  /** the invoice's own receive address */
  readonly address: string;
  readonly amount: string;
  readonly amountAtomic: string;
  readonly ticker: string;
  /**
   * The server's payment uri, for the QR code and "Open in wallet". Empty when
   * it does not pass checkCryptoPaymentUri against `address`, or when the quote
   * changed elsewhere and the uri no longer pays the amount: show the address
   * and the amount to send by hand then.
   */
  readonly uri: string;
  /** usdc_solana only: the memo for a transfer sent by hand */
  readonly memo: string;
  readonly priceUsd: number;
  readonly rateUsd: number;
  /** when the quote ends, RFC 3339 in the server's clock */
  readonly expiresAt: string;
  /**
   * the quote's seconds left, rounded up; 0 once it expired. It counts the
   * quote's own duration from when the quote arrived, so a device clock that
   * is off does not shorten or lengthen it.
   */
  readonly secondsLeft: number;
  /** the server's status; "" for a resumed invoice until its first poll answers */
  readonly status: string;
  readonly reviewReason: string;
  readonly received: string;
  /** how much more to send, while `status` is seen */
  readonly remaining: string;
  readonly confirmations: number;
  readonly confirmationsRequired: number;
  /** the balance code a paid invoice bought, for delivery "code" */
  readonly balanceCode: string;
  /** a requote is in flight */
  readonly refreshing: boolean;
  /** the last start or requote failure: a purchase error code, "" for a transport failure or an answer that could not be read */
  readonly errorCode: string;
  readonly errorMessage: string;
}

/** The api calls the invoice makes; the generated URNetworkApiClient is one. */
export interface CryptoInvoiceClient {
  cryptoPaymentIntent(body: CreateCryptoPaymentIntentArgs): Promise<CryptoPaymentIntentResult>;
  payDataCryptoIntent(body: CreateDataCryptoPaymentIntentArgs): Promise<CryptoPaymentIntentResult>;
  cryptoPaymentStatus(body: CryptoPaymentStatusArgs): Promise<CryptoPaymentStatusResult>;
  cryptoPaymentRequote(body: CryptoPaymentStatusArgs): Promise<CryptoPaymentIntentResult>;
}

/**
 * Where the invoices and the payment method are kept: localStorage's shape,
 * including `length` and `key()`, which list the saved balance-code invoices.
 * Pass localStorage itself, or nothing.
 */
export interface CryptoInvoiceStorage {
  readonly length: number;
  key(index: number): string | null;
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
  removeItem(key: string): void;
}

/** The time source and timers; a test passes a fake one. */
export interface CryptoInvoiceClock {
  now(): number;
  setTimeout(callback: () => void, ms: number): unknown;
  clearTimeout(handle: unknown): void;
}

export interface CryptoInvoiceOptions {
  client: CryptoInvoiceClient;
  /** default: localStorage, when the page can reach it; null keeps everything in memory */
  storage?: CryptoInvoiceStorage | null;
  clock?: CryptoInvoiceClock;
  /**
   * The signed-in network's id, read live on every use, or null while signed
   * out. Without one, no account or network invoice is saved or resumed.
   */
  networkId?: () => string | null | undefined;
}

export interface StartCryptoInvoiceArgs {
  asset: CryptoAsset;
  /** `yearly`, or `yearly_onboarding` for the welcome offer; exactly one of plan and itemId */
  plan?: CreateCryptoPaymentIntentArgs["plan"];
  itemId?: CreateCryptoPaymentIntentArgs["item_id"];
  storefrontCountry?: string;
}

export interface StartDataCryptoInvoiceArgs {
  asset: CryptoAsset;
  itemId: CreateDataCryptoPaymentIntentArgs["item_id"];
  /** the network that receives the data; empty buys a balance code */
  networkName?: string;
  /** where to email the balance code too */
  email?: string;
}

/**
 * A saved invoice: the network it was started on, the invoice and its status
 * token, and the quote as last shown. The status answer carries no address
 * and no uri, so without the quote a resumed invoice could not be paid until
 * it was re-quoted. The same fields as the Go controller's records.
 */
interface CryptoInvoiceRecord {
  network_id: string;
  intent_id: string;
  status_token: string;
  asset: string;
  plan: string;
  item_id: string;
  /** the local time the invoice was first saved; the 7-day cap counts from here */
  created_at: string;
  delivery: string;
  network_name: string;
  address: string;
  amount: string;
  amount_atomic: string;
  uri: string;
  memo: string;
  price_usd: number;
  rate_usd: number;
  /** the quote's start and end in the server's clock, and the local time it arrived */
  quoted_at: string;
  expires_at: string;
  received_at: string;
  /** balance-code invoices only: the last status and the code once paid */
  status: string;
  balance_code: string;
  /** invoices bought without a sign-in only: a dismiss hid it while it might still pay */
  hidden: boolean;
}

type MutableState = { -readonly [K in keyof CryptoInvoiceState]: CryptoInvoiceState[K] };

function emptyState(phase: string): MutableState {
  return {
    phase,
    intentId: "",
    asset: "",
    plan: "",
    itemId: "",
    delivery: "",
    networkName: "",
    address: "",
    amount: "",
    amountAtomic: "",
    ticker: "",
    uri: "",
    memo: "",
    priceUsd: 0,
    rateUsd: 0,
    expiresAt: "",
    secondsLeft: 0,
    status: "",
    reviewReason: "",
    received: "",
    remaining: "",
    confirmations: 0,
    confirmationsRequired: 0,
    balanceCode: "",
    refreshing: false,
    errorCode: "",
    errorMessage: "",
  };
}

function emptyRecord(): CryptoInvoiceRecord {
  return {
    network_id: "",
    intent_id: "",
    status_token: "",
    asset: "",
    plan: "",
    item_id: "",
    created_at: "",
    delivery: "",
    network_name: "",
    address: "",
    amount: "",
    amount_atomic: "",
    uri: "",
    memo: "",
    price_usd: 0,
    rate_usd: 0,
    quoted_at: "",
    expires_at: "",
    received_at: "",
    status: "",
    balance_code: "",
    hidden: false,
  };
}

const RECORD_STRING_FIELDS = [
  "network_id",
  "intent_id",
  "status_token",
  "asset",
  "plan",
  "item_id",
  "created_at",
  "delivery",
  "network_name",
  "address",
  "amount",
  "amount_atomic",
  "uri",
  "memo",
  "quoted_at",
  "expires_at",
  "received_at",
  "status",
  "balance_code",
] as const;

function parseRecord(text: string | null): CryptoInvoiceRecord | null {
  if (text === null) return null;
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch {
    return null;
  }
  if (typeof value !== "object" || value === null) return null;
  const raw = value as Record<string, unknown>;
  const record = emptyRecord();
  for (const field of RECORD_STRING_FIELDS) {
    const v = raw[field];
    if (v !== undefined && typeof v !== "string") return null;
    record[field] = v ?? "";
  }
  for (const field of ["price_usd", "rate_usd"] as const) {
    const v = raw[field];
    if (v !== undefined && typeof v !== "number") return null;
    record[field] = v ?? 0;
  }
  if (raw.hidden !== undefined && typeof raw.hidden !== "boolean") return null;
  record.hidden = raw.hidden === true;
  return record;
}

function recordsEqual(a: CryptoInvoiceRecord, b: CryptoInvoiceRecord): boolean {
  return (
    RECORD_STRING_FIELDS.every((field) => a[field] === b[field]) &&
    a.price_usd === b.price_usd &&
    a.rate_usd === b.rate_usd &&
    a.hidden === b.hidden
  );
}

const isCode = (record: CryptoInvoiceRecord) => record.delivery === CRYPTO_PAYMENT_DELIVERY_CODE;

/**
 * Bought without a sign-in (startDataInvoice): the data goes to a named
 * network or to a balance code, so the invoice belongs to this browser, not to
 * the signed-in network.
 */
const ownedByBrowser = (record: CryptoInvoiceRecord) =>
  record.delivery === CRYPTO_PAYMENT_DELIVERY_NETWORK || record.delivery === CRYPTO_PAYMENT_DELIVERY_CODE;

/** The server's intent ids are uuids. Anything else never names a key. */
const validIntentId = (intentId: string) => /^[0-9A-Za-z-]{1,64}$/.test(intentId);

function tooOld(record: CryptoInvoiceRecord, now: number): boolean {
  const createdAt = parseRfc3339(record.created_at);
  return createdAt === null || MAX_AGE_MS < now - createdAt;
}

/** The local time minus the server's, from a quote's server start and the local time it arrived; 0 when either is missing. */
function quoteOffsetMs(quotedAt: string, receivedAt: string): number {
  const quoted = parseRfc3339(quotedAt);
  const received = parseRfc3339(receivedAt);
  return quoted === null || received === null ? 0 : received - quoted;
}

/** A local time as the records keep it: RFC 3339 to the millisecond. */
const localTime = (ms: number) => new Date(ms).toISOString();

/** The quote's seconds left at now, rounded up, so it is 0 exactly once the quote expired. */
function secondsLeftAt(expiresAt: number | null, now: number): number {
  if (expiresAt === null) return 0;
  const remaining = expiresAt - now;
  if (remaining <= 0) return 0;
  return Math.ceil(remaining / 1000);
}

/**
 * How long until the next status poll, or null for a status never polled
 * again. A resumed invoice with no status yet polls like one waiting for a
 * payment; a status this sdk does not know polls at the slowest rate.
 */
function pollIntervalMs(status: string): number | null {
  switch (status) {
    case "":
    case CRYPTO_PAYMENT_STATUS_PENDING:
    case CRYPTO_PAYMENT_STATUS_SEEN:
    case CRYPTO_PAYMENT_STATUS_CONFIRMING:
      return ACTIVE_POLL_MS;
    case CRYPTO_PAYMENT_STATUS_EXPIRED:
      return EXPIRED_POLL_MS;
    case CRYPTO_PAYMENT_STATUS_PAID:
    case CRYPTO_PAYMENT_STATUS_UNKNOWN:
      return null;
    default:
      return REVIEW_POLL_MS;
  }
}

/** Money may have arrived: the status says so, or a resumed invoice has no status yet. */
function mayHaveReceived(status: string): boolean {
  return (
    status === "" ||
    status === CRYPTO_PAYMENT_STATUS_SEEN ||
    status === CRYPTO_PAYMENT_STATUS_CONFIRMING ||
    status === CRYPTO_PAYMENT_STATUS_REVIEW
  );
}

/** Past pending or expired: something arrived, or the invoice is done. */
function pastPending(status: string): boolean {
  return (
    status === CRYPTO_PAYMENT_STATUS_SEEN ||
    status === CRYPTO_PAYMENT_STATUS_CONFIRMING ||
    status === CRYPTO_PAYMENT_STATUS_REVIEW ||
    status === CRYPTO_PAYMENT_STATUS_PAID
  );
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function defaultStorage(): CryptoInvoiceStorage | null {
  try {
    return (globalThis as { localStorage?: CryptoInvoiceStorage }).localStorage ?? null;
  } catch {
    return null;
  }
}

const defaultClock: CryptoInvoiceClock = {
  now: () => Date.now(),
  setTimeout: (callback, ms) => setTimeout(callback, ms),
  clearTimeout: (handle) => clearTimeout(handle as ReturnType<typeof setTimeout>),
};

export class CryptoInvoice {
  private readonly client: CryptoInvoiceClient;
  private readonly storage: CryptoInvoiceStorage | null;
  private readonly memory = new Map<string, string>();
  private readonly clock: CryptoInvoiceClock;
  private readonly networkIdSource: () => string | null | undefined;
  private readonly listeners = new Set<() => void>();

  private active = false;
  private closed = false;
  // bumped whenever the invoice on screen changes (start, resume, dismiss)
  // and when a requote starts, so the answer to a call made for an earlier
  // invoice or quote is dropped
  private generation = 0;

  private state: MutableState = emptyState(CRYPTO_INVOICE_PHASE_NONE);
  private snapshot: CryptoInvoiceState;
  // the network a start was called on, while its answer is awaited, and
  // whether it is startDataInvoice's, which belongs to this browser
  private startNetworkId = "";
  private startOwnedByBrowser = false;
  // the invoice's saved form; null while there is none on screen
  private record: CryptoInvoiceRecord | null = null;
  // what storage holds for it, as this instance last wrote or read it
  private savedRecord: CryptoInvoiceRecord | null = null;
  // the quote's end in the local clock: the server's expiry moved by quoteOffset
  private expiresAtMs: number | null = null;
  // the local time minus the server's, as of the quote's arrival
  private quoteOffset = 0;

  private pollInFlight = false;
  // poll at once, whatever nextPollAt says (setActive(true), resume, a new quote)
  private forcePoll = false;
  private nextPollAt = 0;
  private nextTickAt = 0;
  // the secondsLeft the last tick published; -1 before the first tick of a
  // quote, and the tick stops once it published 0
  private tickSecondsLeft = -1;
  private timer: unknown = null;

  constructor(options: CryptoInvoiceOptions) {
    this.client = options.client;
    this.storage = options.storage === undefined ? defaultStorage() : options.storage;
    this.clock = options.clock ?? defaultClock;
    this.networkIdSource = options.networkId ?? (() => null);
    this.snapshot = this.buildSnapshot();
  }

  // ---- lifecycle -------------------------------------------------------------

  /** true: the invoice is on screen, poll and tick; false: hidden, nothing runs. */
  setActive(active: boolean): void {
    if (this.closed || this.active === active) return;
    this.active = active;
    if (active) {
      this.forcePoll = true;
      this.nextTickAt = 0;
    }
    this.step();
  }

  /**
   * Ends the invoice's timers. Nothing lands and nothing is written after it;
   * the saved invoice stays saved. Call it when the panel closes, at sign-out
   * and on unmount; a close is never a dismiss.
   */
  close(): void {
    this.closed = true;
    this.active = false;
    this.clearTimer();
    this.listeners.clear();
  }

  /** Calls `listener` after every change; returns the unsubscribe. */
  subscribe(listener: () => void): () => void {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  }

  /** The invoice as of its last change. The same object until the next change, as React's useSyncExternalStore needs. */
  getState(): CryptoInvoiceState {
    return this.snapshot;
  }

  // ---- actions ---------------------------------------------------------------

  /** Starts an invoice for the signed-in network; the server returns its open invoice for the same asset and item, if any. */
  startInvoice(args: StartCryptoInvoiceArgs): Promise<void> {
    const body: CreateCryptoPaymentIntentArgs = { asset: args.asset };
    if (args.plan) body.plan = args.plan;
    if (args.itemId) body.item_id = args.itemId;
    if (args.storefrontCountry) body.storefront_country = args.storefrontCountry;
    return this.start(args.asset, args.plan ?? "", args.itemId ?? "", false, () => this.client.cryptoPaymentIntent(body));
  }

  /** Starts a data-pack invoice without a sign-in, for a named network or a balance code. */
  startDataInvoice(args: StartDataCryptoInvoiceArgs): Promise<void> {
    const body: CreateDataCryptoPaymentIntentArgs = {
      asset: args.asset,
      item_id: args.itemId,
      network_name: args.networkName ?? "",
      email: args.email ?? "",
    };
    return this.start(args.asset, "", args.itemId, true, () => this.client.payDataCryptoIntent(body));
  }

  /**
   * Shows a saved invoice, and polls it at once while active. In order: a
   * balance-code invoice that is paid and not yet dismissed after its code was
   * shown; an invoice bought without a sign-in that was on screen; the
   * signed-in network's own invoice. Every other invoice bought without a
   * sign-in is polled once: a balance code that turns out paid is shown while
   * nothing else is on screen, and an invoice the server no longer knows, or a
   * network's that was paid, is dropped. An invoice already on screen stays,
   * unless it is an account invoice of another network than the signed-in
   * one, which puts it away. True when an invoice is on screen after the call;
   * a paid code found by the polls shows later.
   */
  resume(): boolean {
    if (this.closed) return false;
    const networkId = this.currentNetworkId();
    let changed = false;
    switch (this.state.phase) {
      case CRYPTO_INVOICE_PHASE_STARTING:
      case CRYPTO_INVOICE_PHASE_OPEN:
      case CRYPTO_INVOICE_PHASE_PAID:
        if (this.showingOwnedByBrowser() || this.showingNetworkId() === networkId) {
          return true;
        }
        // another network's invoice, or one from before a sign-out
        this.reset();
        this.state = emptyState(CRYPTO_INVOICE_PHASE_NONE);
        changed = true;
    }

    const now = this.clock.now();
    const owned: CryptoInvoiceRecord[] = [];
    for (const record of this.loadAllOwned()) {
      if (!record.status_token || !ownedByBrowser(record) || tooOld(record, now)) {
        this.removeItem(OWNED_KEY_PREFIX + record.intent_id);
        continue;
      }
      owned.push(record);
    }
    // newest first
    owned.sort((a, b) => (a.created_at < b.created_at ? 1 : a.created_at > b.created_at ? -1 : 0));
    let show: CryptoInvoiceRecord | null =
      owned.find((record) => isCode(record) && record.status === CRYPTO_PAYMENT_STATUS_PAID) ??
      owned.find((record) => !record.hidden && record.status !== CRYPTO_PAYMENT_STATUS_PAID) ??
      null;
    if (!show && networkId) {
      const record = parseRecord(this.getItem(this.slotKey(networkId)));
      if (record && record.intent_id && record.status_token && record.network_id === networkId && !ownedByBrowser(record)) {
        if (tooOld(record, now)) {
          this.removeItem(this.slotKey(networkId));
        } else {
          show = record;
        }
      }
    }
    const checks = owned.filter((record) => record !== show);
    if (show) {
      this.showRecord(show);
      changed = true;
    }
    if (changed) {
      this.notify();
      this.step();
    }
    for (const record of checks) {
      const intentId = record.intent_id;
      this.call(() => this.client.cryptoPaymentStatus({ intent_id: intentId, status_token: record.status_token })).then(
        (result) => this.ownedChecked(intentId, result, null),
        (error: unknown) => this.ownedChecked(intentId, null, error),
      );
    }
    return show !== null;
  }

  /**
   * Asks for a new quote for an invoice nothing has been paid to (status
   * pending or expired). A poll in flight is dropped, since it answers for the
   * old quote, and the new quote is polled at once. `refreshing` is true while
   * it is in flight; a failure leaves the invoice as it was, with `errorCode`
   * set. Anything else is ignored.
   */
  requote(): Promise<void> {
    const record = this.record;
    if (
      this.closed ||
      this.state.phase !== CRYPTO_INVOICE_PHASE_OPEN ||
      !record ||
      this.state.refreshing ||
      (this.state.status !== CRYPTO_PAYMENT_STATUS_PENDING && this.state.status !== CRYPTO_PAYMENT_STATUS_EXPIRED)
    ) {
      return Promise.resolve();
    }
    this.state.refreshing = true;
    this.state.errorCode = "";
    this.state.errorMessage = "";
    this.generation += 1;
    this.pollInFlight = false;
    const generation = this.generation;
    this.notify();
    this.step();
    return this.call(() =>
      this.client.cryptoPaymentRequote({ intent_id: record.intent_id, status_token: record.status_token }),
    ).then(
      (result) => this.requoteDone(generation, result, null),
      (error: unknown) => this.requoteDone(generation, null, error),
    );
  }

  /**
   * Closes the invoice ("Pay another way"); closing the panel is close(),
   * never this. An account or network invoice with nothing received is
   * dropped: nothing is lost, the data goes to the network anyway. Once money
   * arrived it is only put away: an account invoice comes back at the next
   * resume(), and one bought without a sign-in is hidden. A balance-code
   * invoice is dropped only when it is paid and its code was shown; before
   * that it is only hidden, since its status lags the payment (USDC goes from
   * pending straight to paid, and a late payment credits while it reads
   * expired) and a dropped code invoice would lose a code nobody else can read.
   */
  dismiss(): void {
    if (this.closed || this.state.phase === CRYPTO_INVOICE_PHASE_NONE) return;
    if (this.record && this.state.status !== CRYPTO_PAYMENT_STATUS_UNKNOWN) {
      if (isCode(this.record)) {
        if (this.state.phase === CRYPTO_INVOICE_PHASE_PAID && this.state.balanceCode) {
          this.clearRecord();
        } else {
          this.record.hidden = true;
          this.saveRecord();
        }
      } else if (!mayHaveReceived(this.state.status)) {
        this.clearRecord();
      } else if (ownedByBrowser(this.record)) {
        this.record.hidden = true;
        this.saveRecord();
      }
    }
    this.reset();
    this.state = emptyState(CRYPTO_INVOICE_PHASE_NONE);
    this.notify();
    this.step();
  }

  /** The payment method last chosen here ("card" or a coin), "" when none was. */
  getPaymentMethod(): string {
    let method: unknown = null;
    try {
      method = JSON.parse(this.getItem(this.paymentMethodKey()) ?? "null");
    } catch {
      return "";
    }
    return typeof method === "string" && (method === PAYMENT_METHOD_CARD || cryptoAssetTicker(method) !== "")
      ? method
      : "";
  }

  /** Remembers the payment method: "card" or a coin, or "" to forget it. Anything else is ignored. */
  setPaymentMethod(method: string): void {
    if (this.closed) return;
    if (method === "") {
      this.removeItem(this.paymentMethodKey());
    } else if (method === PAYMENT_METHOD_CARD || cryptoAssetTicker(method) !== "") {
      this.setItem(this.paymentMethodKey(), JSON.stringify(method));
    }
  }

  // ---- answers ---------------------------------------------------------------

  private start(
    asset: string,
    plan: string,
    itemId: string,
    ownedByBrowser: boolean,
    request: () => Promise<CryptoPaymentIntentResult>,
  ): Promise<void> {
    if (this.closed) return Promise.resolve();
    this.reset();
    this.state = {
      ...emptyState(CRYPTO_INVOICE_PHASE_STARTING),
      asset,
      plan,
      itemId,
      ticker: cryptoAssetTicker(asset),
    };
    const generation = this.generation;
    const networkId = this.currentNetworkId();
    this.startNetworkId = networkId;
    this.startOwnedByBrowser = ownedByBrowser;
    this.notify();
    this.step();
    return this.call(request).then(
      (result) => this.intentStarted(generation, networkId, result, null),
      (error: unknown) => this.intentStarted(generation, networkId, null, error),
    );
  }

  private call<R>(request: () => Promise<R>): Promise<R> {
    try {
      return Promise.resolve(request());
    } catch (error) {
      return Promise.reject(error);
    }
  }

  private intentStarted(
    generation: number,
    networkId: string,
    result: CryptoPaymentIntentResult | null,
    error: unknown,
  ): void {
    if (this.closed || generation !== this.generation) {
      // dismissed, replaced by another invoice or closed while in flight
      return;
    }
    if (error !== null) {
      this.fail("", errorMessage(error));
    } else if (!result) {
      this.fail("", "no invoice");
    } else if (result.error) {
      this.fail(result.error.code ?? "", result.error.message ?? "");
    } else if (!result.intent_id || !result.status_token || !result.address || !result.amount_atomic) {
      // an answer that cannot be paid is no answer
      this.fail("", "no invoice");
    } else {
      this.record = {
        ...emptyRecord(),
        network_id: networkId,
        intent_id: result.intent_id,
        status_token: result.status_token,
        created_at: new Date(this.clock.now()).toISOString().replace(/\.\d{3}Z$/, "Z"),
      };
      this.applyQuote(result, false);
    }
    this.notify();
    this.step();
  }

  private requoteDone(generation: number, result: CryptoPaymentIntentResult | null, error: unknown): void {
    const record = this.record;
    if (this.closed || generation !== this.generation || !record) return;
    this.state.refreshing = false;
    if (error !== null) {
      this.state.errorCode = "";
      this.state.errorMessage = errorMessage(error);
    } else if (!result) {
      this.state.errorCode = "";
      this.state.errorMessage = "no quote";
    } else if (result.error) {
      this.state.errorCode = result.error.code ?? "";
      this.state.errorMessage = result.error.message ?? "";
    } else if (result.intent_id !== record.intent_id || !result.address || !result.amount_atomic) {
      // another invoice's quote, or one that cannot be paid
      this.state.errorCode = "";
      this.state.errorMessage = "no quote";
    } else {
      if (result.status_token) record.status_token = result.status_token;
      this.applyQuote(result, true);
    }
    this.notify();
    this.step();
  }

  private pollDone(generation: number, result: CryptoPaymentStatusResult | null, error: unknown): void {
    const record = this.record;
    if (this.closed || generation !== this.generation || !record) return;
    this.pollInFlight = false;
    this.forcePoll = false;
    const now = this.clock.now();
    if (error !== null || !result || (result.intent_id && result.intent_id !== record.intent_id)) {
      // keep the invoice as it is and ask again on its cadence
      const interval = pollIntervalMs(this.state.status);
      if (interval !== null) this.nextPollAt = now + interval;
      this.step();
      return;
    }
    this.applyStatus(result);
    const interval = pollIntervalMs(this.state.status);
    if (interval !== null) this.nextPollAt = now + interval;
    this.notify();
    this.step();
  }

  /**
   * The answer to resume()'s single poll of an invoice bought without a
   * sign-in that is not on screen. Unknown drops it. Paid saves a balance
   * code, and shows it when nothing else is on screen; a network's paid
   * invoice is done, its data on the network, and is dropped.
   */
  private ownedChecked(intentId: string, result: CryptoPaymentStatusResult | null, error: unknown): void {
    if (this.closed || error !== null || !result || (result.intent_id && result.intent_id !== intentId)) return;
    if (this.record && this.record.intent_id === intentId) {
      // on screen by now, polled as such
      return;
    }
    const record = parseRecord(this.getItem(OWNED_KEY_PREFIX + intentId));
    if (!record || record.intent_id !== intentId) return;
    switch (result.status) {
      case CRYPTO_PAYMENT_STATUS_UNKNOWN:
        this.removeItem(OWNED_KEY_PREFIX + intentId);
        break;
      case CRYPTO_PAYMENT_STATUS_PAID:
        if (!isCode(record)) {
          this.removeItem(OWNED_KEY_PREFIX + intentId);
          break;
        }
        record.status = CRYPTO_PAYMENT_STATUS_PAID;
        record.balance_code = result.balance_code ?? "";
        record.hidden = false;
        this.setItem(OWNED_KEY_PREFIX + intentId, JSON.stringify(record));
        if (this.state.phase === CRYPTO_INVOICE_PHASE_NONE) {
          this.showRecord(record);
          this.notify();
          this.step();
        }
        break;
    }
  }

  // ---- state -----------------------------------------------------------------

  /** Forgets the invoice on screen and drops every answer in flight. The caller sets the new state. */
  private reset(): void {
    this.generation += 1;
    this.startNetworkId = "";
    this.startOwnedByBrowser = false;
    this.record = null;
    this.savedRecord = null;
    this.expiresAtMs = null;
    this.quoteOffset = 0;
    this.pollInFlight = false;
    this.forcePoll = false;
    this.nextPollAt = 0;
    this.nextTickAt = 0;
    this.tickSecondsLeft = -1;
  }

  private fail(code: string, message: string): void {
    this.state.phase = CRYPTO_INVOICE_PHASE_FAILED;
    this.state.errorCode = code;
    this.state.errorMessage = message;
  }

  /** The network the invoice on screen was started on. */
  private showingNetworkId(): string {
    return this.record ? this.record.network_id : this.startNetworkId;
  }

  /** The invoice on screen was bought without a sign-in, so it belongs to no network. */
  private showingOwnedByBrowser(): boolean {
    return this.record ? ownedByBrowser(this.record) : this.startOwnedByBrowser;
  }

  /** Shows a saved invoice: open and not yet polled, or paid when its record says so (a balance-code invoice). */
  private showRecord(saved: CryptoInvoiceRecord): void {
    this.reset();
    // as storage holds it; on screen it is no longer hidden
    this.savedRecord = { ...saved };
    const record = { ...saved, hidden: false };
    this.record = record;
    this.state = {
      ...emptyState(CRYPTO_INVOICE_PHASE_OPEN),
      intentId: record.intent_id,
      asset: record.asset,
      plan: record.plan,
      itemId: record.item_id,
      delivery: record.delivery,
      networkName: record.network_name,
      address: record.address,
      amount: record.amount,
      amountAtomic: record.amount_atomic,
      ticker: cryptoAssetTicker(record.asset),
      memo: record.memo,
      priceUsd: record.price_usd,
      rateUsd: record.rate_usd,
    };
    if (isCode(record) && record.status === CRYPTO_PAYMENT_STATUS_PAID) {
      this.state.phase = CRYPTO_INVOICE_PHASE_PAID;
      this.state.status = CRYPTO_PAYMENT_STATUS_PAID;
      this.state.balanceCode = record.balance_code;
    }
    this.setUri(record.uri);
    this.quoteOffset = quoteOffsetMs(record.quoted_at, record.received_at);
    this.setExpiresAt(record.expires_at);
    this.forcePoll = true;
  }

  /**
   * Shows a quote from the server (a new invoice or a requote) and saves it.
   * The quote's own fields come from the answer as they are; the invoice's
   * names keep what the invoice already shows when the answer leaves them out.
   * A requote never moves a status past pending or expired back to pending,
   * and its quote is polled at once.
   */
  private applyQuote(result: CryptoPaymentIntentResult, requote: boolean): void {
    const record = this.record!;
    const now = this.clock.now();
    const keep = (value: string | undefined, current: string) => (value ? value : current);
    this.state.phase = CRYPTO_INVOICE_PHASE_OPEN;
    this.state.intentId = record.intent_id;
    this.state.asset = keep(result.asset, this.state.asset);
    this.state.plan = keep(result.plan, this.state.plan);
    this.state.itemId = keep(result.item_id, this.state.itemId);
    this.state.delivery = keep(result.delivery, this.state.delivery);
    this.state.networkName = keep(result.network_name, this.state.networkName);
    this.state.address = result.address ?? "";
    this.state.amount = result.amount ?? "";
    this.state.amountAtomic = result.amount_atomic ?? "";
    this.state.ticker = cryptoAssetTicker(this.state.asset);
    this.state.memo = result.memo ?? "";
    this.state.priceUsd = result.price_usd ?? 0;
    this.state.rateUsd = result.rate_usd ?? 0;
    if (result.confirmations_required && 0 < result.confirmations_required) {
      this.state.confirmationsRequired = result.confirmations_required;
    }
    this.state.errorCode = "";
    this.state.errorMessage = "";
    this.setUri(result.uri ?? "");

    // the countdown runs the quote's own duration from now, whatever the
    // device clock says about the server's
    const receivedAt = localTime(now);
    this.quoteOffset = quoteOffsetMs(result.quoted_at ?? "", receivedAt);
    this.expiresAtMs = null;
    this.setExpiresAt(result.expires_at ?? "");

    record.asset = this.state.asset;
    record.plan = this.state.plan;
    record.item_id = this.state.itemId;
    record.delivery = this.state.delivery;
    record.quoted_at = result.quoted_at ?? "";
    record.received_at = receivedAt;

    if (requote && pastPending(this.state.status)) {
      // money arrived: the requote's pending never makes it payable again
      this.saveRecord();
    } else {
      // both a new invoice and a requote have nothing received yet
      this.applyStatus(result.status ?? { status: CRYPTO_PAYMENT_STATUS_PENDING });
    }
    if (requote) {
      this.forcePoll = true;
    } else {
      this.forcePoll = false;
      const interval = pollIntervalMs(this.state.status);
      if (interval !== null) this.nextPollAt = now + interval;
    }
  }

  /**
   * Shows a status from the server and keeps the saved invoice in step: saved
   * while open, dropped once paid (a balance-code invoice's is kept, with its
   * code, until it is dismissed) or unknown. A missing status reads as "".
   */
  private applyStatus(status: CryptoPaymentStatusResult): void {
    const record = this.record!;
    const value: string = status.status ?? "";
    if (value === CRYPTO_PAYMENT_STATUS_UNKNOWN) {
      this.state.status = CRYPTO_PAYMENT_STATUS_UNKNOWN;
      this.state.phase = CRYPTO_INVOICE_PHASE_FAILED;
      this.clearRecord();
      return;
    }
    this.state.status = value;
    this.state.reviewReason = status.review_reason ?? "";
    this.state.received = status.received ?? "";
    this.state.remaining = status.remaining ?? "";
    this.state.confirmations = status.confirmations ?? 0;
    if (status.confirmations_required && 0 < status.confirmations_required) {
      this.state.confirmationsRequired = status.confirmations_required;
    }
    this.state.balanceCode = status.balance_code ?? "";
    if (status.delivery && !this.state.delivery) {
      this.state.delivery = status.delivery;
      record.delivery = status.delivery;
    }
    if (status.network_name) this.state.networkName = status.network_name;
    if (status.amount_atomic && status.amount_atomic !== this.state.amountAtomic) {
      // quoted again elsewhere (another device requoted this network's
      // invoice): the uri still asks for the old amount, so it goes
      this.state.amount = status.amount ?? "";
      this.state.amountAtomic = status.amount_atomic;
      this.state.uri = "";
    }
    if (status.expires_at) this.setExpiresAt(status.expires_at);
    if (value === CRYPTO_PAYMENT_STATUS_EXPIRED) {
      // the server's word ends the countdown, whatever is left of it here (an
      // open invoice handed out again keeps its first quote time)
      const now = this.clock.now();
      if (this.expiresAtMs !== null && now < this.expiresAtMs) {
        this.expiresAtMs = now;
        this.tickSecondsLeft = 0;
      }
    }

    if (value === CRYPTO_PAYMENT_STATUS_PAID) {
      this.state.phase = CRYPTO_INVOICE_PHASE_PAID;
      if (!isCode(record)) {
        // this answer is the delivery: the listeners hear it next
        this.clearRecord();
        return;
      }
    } else {
      this.state.phase = CRYPTO_INVOICE_PHASE_OPEN;
    }
    this.saveRecord();
  }

  /**
   * Saves the invoice as it shows now, when that changed. Nothing is written
   * after close(). An account invoice is only ever saved for the network that
   * is signed in, never for none; an invoice bought without a sign-in is saved
   * whoever is signed in, since it belongs to no network.
   */
  private saveRecord(): void {
    const record: CryptoInvoiceRecord = {
      ...this.record!,
      delivery: this.state.delivery,
      network_name: this.state.networkName,
      address: this.state.address,
      amount: this.state.amount,
      amount_atomic: this.state.amountAtomic,
      uri: this.state.uri,
      memo: this.state.memo,
      price_usd: this.state.priceUsd,
      rate_usd: this.state.rateUsd,
      expires_at: this.state.expiresAt,
    };
    if (isCode(record)) {
      record.status = this.state.status;
      record.balance_code = this.state.balanceCode;
    }
    this.record = record;
    if (this.savedRecord && recordsEqual(record, this.savedRecord)) return;
    if (this.closed) return;
    if (ownedByBrowser(record)) {
      if (!validIntentId(record.intent_id)) return;
      this.setItem(OWNED_KEY_PREFIX + record.intent_id, JSON.stringify(record));
    } else {
      const networkId = this.currentNetworkId();
      if (!record.network_id || record.network_id !== networkId) return;
      this.setItem(this.slotKey(networkId), JSON.stringify(record));
    }
    this.savedRecord = { ...record };
  }

  /**
   * Drops the saved invoice, from its own place only. The network's slot is
   * compare-and-delete: it is left alone when it holds another invoice by
   * now, which another tab saved there.
   */
  private clearRecord(): void {
    const record = this.record;
    this.savedRecord = null;
    if (!record || this.closed) return;
    if (ownedByBrowser(record)) {
      if (validIntentId(record.intent_id)) this.removeItem(OWNED_KEY_PREFIX + record.intent_id);
      return;
    }
    if (!record.network_id) return;
    const key = this.slotKey(record.network_id);
    const slot = parseRecord(this.getItem(key));
    if (slot && slot.intent_id === record.intent_id) this.removeItem(key);
  }

  private setUri(uri: string): void {
    this.state.uri = checkCryptoPaymentUri(this.state.asset, uri, this.state.address) ? uri : "";
  }

  /**
   * Moves the quote's end, a server time, into the local clock by the quote's
   * offset. A new end restarts the countdown from the second it shows now; the
   * next tick is a second away.
   */
  private setExpiresAt(expiresAt: string): void {
    this.state.expiresAt = expiresAt;
    const parsed = parseRfc3339(expiresAt);
    const local = parsed === null ? null : parsed + this.quoteOffset;
    if (local === this.expiresAtMs) return;
    this.expiresAtMs = local;
    const now = this.clock.now();
    this.tickSecondsLeft = secondsLeftAt(local, now);
    this.nextTickAt = now + TICK_MS;
  }

  // ---- loop ------------------------------------------------------------------

  /**
   * Launches a due status poll, publishes a due tick, and arms a timer for the
   * next of either; none while inactive or closed, with no open invoice, or
   * with a poll or requote in flight and the countdown done.
   */
  private step(): void {
    this.clearTimer();
    const record = this.record;
    if (this.closed || !this.active || this.state.phase !== CRYPTO_INVOICE_PHASE_OPEN || !record) return;
    const now = this.clock.now();

    let next: number | null = null;
    let pollArgs: CryptoPaymentStatusArgs | null = null;
    const generation = this.generation;
    const interval = pollIntervalMs(this.state.status);
    if (interval !== null && !this.pollInFlight && !this.state.refreshing) {
      if (this.forcePoll || this.nextPollAt <= now) {
        this.pollInFlight = true;
        this.forcePoll = false;
        // the answer sets the next poll from the status it brings
        this.nextPollAt = now + interval;
        pollArgs = { intent_id: record.intent_id, status_token: record.status_token };
      } else {
        next = this.nextPollAt;
      }
    }

    let ticked = false;
    if (this.expiresAtMs !== null && this.tickSecondsLeft !== 0) {
      if (this.nextTickAt <= now) {
        const secondsLeft = secondsLeftAt(this.expiresAtMs, now);
        if (secondsLeft !== this.tickSecondsLeft) {
          this.tickSecondsLeft = secondsLeft;
          ticked = true;
        }
        this.nextTickAt = now + TICK_MS;
      }
      if (this.tickSecondsLeft !== 0 && (next === null || this.nextTickAt < next)) {
        next = this.nextTickAt;
      }
    }

    if (next !== null) {
      this.timer = this.clock.setTimeout(() => {
        this.timer = null;
        this.step();
      }, Math.max(next - now, 0));
    }
    if (ticked) this.notify();
    if (pollArgs) {
      const args = pollArgs;
      this.call(() => this.client.cryptoPaymentStatus(args)).then(
        (result) => this.pollDone(generation, result, null),
        (error: unknown) => this.pollDone(generation, null, error),
      );
    }
  }

  private clearTimer(): void {
    if (this.timer !== null) {
      this.clock.clearTimeout(this.timer);
      this.timer = null;
    }
  }

  private buildSnapshot(): CryptoInvoiceState {
    const secondsLeft =
      this.state.phase === CRYPTO_INVOICE_PHASE_OPEN ? secondsLeftAt(this.expiresAtMs, this.clock.now()) : 0;
    return Object.freeze({ ...this.state, secondsLeft });
  }

  private notify(): void {
    this.snapshot = this.buildSnapshot();
    for (const listener of [...this.listeners]) {
      try {
        listener();
      } catch {
        // a listener's failure is its own; the others still hear the change
      }
    }
  }

  // ---- storage ---------------------------------------------------------------

  private currentNetworkId(): string {
    try {
      return this.networkIdSource() ?? "";
    } catch {
      return "";
    }
  }

  private slotKey(networkId: string): string {
    return `${CRYPTO_INVOICE_STORAGE_KEY}.${networkId}`;
  }

  private paymentMethodKey(): string {
    return `${CRYPTO_PAYMENT_METHOD_STORAGE_KEY}.${this.currentNetworkId() || "anon"}`;
  }

  /** Every invoice of this browser's own saved, the ones bought without a sign-in. */
  private loadAllOwned(): CryptoInvoiceRecord[] {
    const keys: string[] = [];
    if (!this.storage) {
      keys.push(...this.memory.keys());
    } else {
      try {
        for (let i = 0; i < this.storage.length; i++) {
          const key = this.storage.key(i);
          if (key !== null) keys.push(key);
        }
      } catch {
        // a blocked storage lists nothing
      }
    }
    const records: CryptoInvoiceRecord[] = [];
    for (const key of keys) {
      if (!key.startsWith(OWNED_KEY_PREFIX)) continue;
      const intentId = key.slice(OWNED_KEY_PREFIX.length);
      if (!validIntentId(intentId)) continue;
      const record = parseRecord(this.getItem(key));
      if (record && record.intent_id === intentId) records.push(record);
    }
    return records;
  }

  private getItem(key: string): string | null {
    if (!this.storage) return this.memory.get(key) ?? null;
    try {
      return this.storage.getItem(key);
    } catch {
      return null;
    }
  }

  private setItem(key: string, value: string): void {
    if (this.closed) return;
    if (!this.storage) {
      this.memory.set(key, value);
      return;
    }
    try {
      this.storage.setItem(key, value);
    } catch {
      // a full or blocked storage: the invoice stays on screen, unsaved
    }
  }

  private removeItem(key: string): void {
    if (this.closed) return;
    if (!this.storage) {
      this.memory.delete(key);
      return;
    }
    try {
      this.storage.removeItem(key);
    } catch {
      // nothing to do: a blocked storage holds nothing of ours either
    }
  }
}
