// The lag.dev guest session (lag.dev/PLAN.md S4, A1, L11). One guest per
// browser: created on first use, kept through an injected storage adapter so
// page loads reuse it, and replaced once it is within 10 minutes of
// expires_at. Creation is rate-limited per /24 (/48), so concurrent callers
// share one creation, and a guest that is due for renewal but not yet expired
// keeps working while renewal fails.
//
// expires_at is on the api's clock, and a device's clock can be hours off.
// Judged by the device's clock, a guest on a device a day ahead would be due
// the moment it arrived, and every call would make another, spending A1's
// limit for everyone on that /24. So a guest's lifetime is read on the api's
// clock, from its jwt's `iat` to expires_at, and counted on the device's
// clock from when the guest arrived.

import type { RequestOptions } from "../api_client";
import { parseJWTClaims } from "../utils/jwt";
import { createLagApiClient, type LagApiRefusal, type LagSessionResult } from "./api";

/** Where the guest is kept between page loads; any method may be async. */
export interface LagStorage {
  get(key: string): string | null | undefined | Promise<string | null | undefined>;
  set(key: string, value: string): void | Promise<void>;
  remove(key: string): void | Promise<void>;
}

/** The storage key lag.dev keeps its guest under (L11). */
export const LAG_GUEST_STORAGE_KEY = "lagdev.guest";
/** A guest this close to expires_at is replaced (S4). */
export const LAG_GUEST_RENEW_WITHIN_MS = 10 * 60 * 1000;
// how long a failed renewal waits before the next, at least
const LAG_GUEST_RENEW_RETRY_MS = 60 * 1000;
// A1: the api keeps a guest 24 h. A guest whose jwt says nothing of when it
// was made is taken to live that long when the device's clock can't be
// believed.
const LAG_GUEST_TTL_MS = 24 * 60 * 60 * 1000;

/** A usable guest: the A1 result with every field present. */
export interface LagGuest {
  by_client_jwt: string;
  network_id: string;
  client_id: string;
  /** RFC 3339 */
  expires_at: string;
}

/** The api refused to create a guest, or answered without one. */
export class LagGuestSessionError extends Error {
  /** the refusal's code, e.g. "limit", when the api gave one */
  readonly code?: string;
  readonly retryAfterMs?: number;

  constructor(message: string, refusal?: LagApiRefusal) {
    super(message);
    this.name = "LagGuestSessionError";
    this.code = refusal?.code;
    this.retryAfterMs = refusal?.retry_after_ms;
  }
}

export interface LagGuestSessionOptions {
  /** where the guest is kept between page loads, e.g. localLagStorage() */
  storage: LagStorage;
  /** the api to create guests with (default createLagApiClient({ baseURL })) */
  api?: { lagSession(options?: RequestOptions): Promise<LagSessionResult> };
  /** the api origin when `api` is not given (default https://api.bringyour.com) */
  baseURL?: string;
  /** default LAG_GUEST_STORAGE_KEY */
  key?: string;
  /** default LAG_GUEST_RENEW_WITHIN_MS */
  renewWithinMs?: number;
  /** epoch milliseconds (default Date.now) */
  now?: () => number;
}

const expiryOf = (guest: LagGuest): number => Date.parse(guest.expires_at);

// A guest as this device keeps it: the A1 result, and on this device's clock
// (epoch ms) when it is due for renewal and when it expires. They are stored
// beside A1's fields, so every tab of the browser reads the same times.
interface KeptGuest {
  guest: LagGuest;
  renewAtMs: number;
  expiresAtMs: number;
}

// When the api signed a jwt, by its clock: its `iat`, in seconds
const issuedAtOf = (jwt: string): number | undefined => {
  try {
    const issuedAt = parseJWTClaims(jwt).iat;
    return typeof issuedAt === "number" && Number.isFinite(issuedAt) && 0 < issuedAt ? issuedAt * 1000 : undefined;
  } catch {
    return undefined;
  }
};

const asGuest = (value: unknown): LagGuest | null => {
  if (typeof value !== "object" || value === null) {
    return null;
  }
  const { by_client_jwt, network_id, client_id, expires_at } = value as Record<string, unknown>;
  if (
    typeof by_client_jwt !== "string" ||
    by_client_jwt === "" ||
    typeof network_id !== "string" ||
    typeof client_id !== "string" ||
    typeof expires_at !== "string" ||
    Number.isNaN(Date.parse(expires_at))
  ) {
    return null;
  }
  return { by_client_jwt, network_id, client_id, expires_at };
};

const finiteNumber = (value: unknown): number | undefined =>
  typeof value === "number" && Number.isFinite(value) ? value : undefined;

// A kept guest from storage. One kept before its device times were (or by
// another writer) has none, and is judged by expires_at on this device's
// clock, as before.
const parseStored = (text: string | null | undefined, renewWithinMs: number): KeptGuest | null => {
  if (!text) {
    return null;
  }
  try {
    const value: unknown = JSON.parse(text);
    const guest = asGuest(value);
    if (guest === null) {
      return null;
    }
    const { local_renew_ms, local_expires_ms } = value as Record<string, unknown>;
    const expiresAtMs = finiteNumber(local_expires_ms) ?? expiryOf(guest);
    const renewAtMs = finiteNumber(local_renew_ms) ?? expiresAtMs - renewWithinMs;
    return { guest, renewAtMs: Math.min(renewAtMs, expiresAtMs), expiresAtMs };
  } catch {
    return null;
  }
};

const storedText = (kept: KeptGuest): string =>
  JSON.stringify({ ...kept.guest, local_renew_ms: kept.renewAtMs, local_expires_ms: kept.expiresAtMs });

const raceAbort = <T>(promise: Promise<T>, signal: AbortSignal): Promise<T> =>
  new Promise<T>((resolve, reject) => {
    if (signal.aborted) {
      reject(signal.reason);
      return;
    }
    const onAbort = () => reject(signal.reason);
    signal.addEventListener("abort", onAbort, { once: true });
    promise.then(
      (value) => {
        signal.removeEventListener("abort", onAbort);
        resolve(value);
      },
      (error) => {
        signal.removeEventListener("abort", onAbort);
        reject(error);
      },
    );
  });

/**
 * The guest jwt for the lag api routes. get() answers from memory while the
 * guest is fresh, otherwise reads storage (another tab may have renewed it),
 * otherwise creates one. Storage failures (a private window, blocked site
 * data) only cost persistence: the guest still works for this page.
 */
export class LagGuestSession {
  private readonly storage: LagStorage;
  private readonly api: { lagSession(options?: RequestOptions): Promise<LagSessionResult> };
  private readonly key: string;
  private readonly renewWithinMs: number;
  private readonly now: () => number;
  private current: KeptGuest | null = null;
  private loading: Promise<LagGuest> | null = null;
  // after a failed renewal, the still-valid guest is used without asking
  // the api again until then
  private renewAfter = 0;

  constructor(options: LagGuestSessionOptions) {
    this.storage = options.storage;
    this.api = options.api ?? createLagApiClient({ baseURL: options.baseURL });
    this.key = options.key ?? LAG_GUEST_STORAGE_KEY;
    this.renewWithinMs = options.renewWithinMs ?? LAG_GUEST_RENEW_WITHIN_MS;
    this.now = options.now ?? Date.now;
  }

  /**
   * The current guest, created or renewed as needed. Concurrent calls share
   * one load; a caller's `signal` abandons only its own wait.
   */
  get(options: { signal?: AbortSignal } = {}): Promise<LagGuest> {
    const { signal } = options;
    if (signal?.aborted) {
      return Promise.reject(signal.reason);
    }
    if (this.current !== null && this.isFresh(this.current)) {
      return Promise.resolve(this.current.guest);
    }
    if (this.loading === null) {
      this.loading = this.load().finally(() => {
        this.loading = null;
      });
    }
    return signal ? raceAbort(this.loading, signal) : this.loading;
  }

  /** The guest's client jwt, for the api client's `token`. */
  async token(options: { signal?: AbortSignal } = {}): Promise<string> {
    return (await this.get(options)).by_client_jwt;
  }

  /**
   * Forget the guest, e.g. after the api refused its jwt, so the next get()
   * creates one. Given the refused `jwt`, it forgets only that guest: a
   * guest another caller has already replaced it with stays.
   */
  async invalidate(jwt?: string): Promise<void> {
    if (jwt === undefined || this.current?.guest.by_client_jwt === jwt) {
      this.current = null;
    }
    const stored = parseStored(await this.read(), this.renewWithinMs);
    if (stored !== null && jwt !== undefined && stored.guest.by_client_jwt !== jwt) {
      return;
    }
    await this.attempt(() => this.storage.remove(this.key));
  }

  private isFresh(kept: KeptGuest): boolean {
    return this.now() < kept.renewAtMs;
  }

  private async load(): Promise<LagGuest> {
    const stored = parseStored(await this.read(), this.renewWithinMs);
    let best = this.current;
    // the guest the api made last is the one that lives longest
    if (stored !== null && (best === null || expiryOf(best.guest) < expiryOf(stored.guest))) {
      best = stored;
    }
    const usable = best !== null && this.now() < best.expiresAtMs;
    if (best !== null && (this.isFresh(best) || (usable && this.now() < this.renewAfter))) {
      this.current = best;
      return best.guest;
    }
    let created: KeptGuest;
    try {
      created = await this.create();
    } catch (error) {
      // a guest due for renewal still works until it actually expires
      if (best !== null && usable) {
        const retryAfterMs = error instanceof LagGuestSessionError ? (error.retryAfterMs ?? 0) : 0;
        this.renewAfter = this.now() + Math.max(LAG_GUEST_RENEW_RETRY_MS, retryAfterMs);
        this.current = best;
        return best.guest;
      }
      throw error;
    }
    this.current = created;
    await this.attempt(() => this.storage.set(this.key, storedText(created)));
    return created.guest;
  }

  private async create(): Promise<KeptGuest> {
    const result = await this.api.lagSession();
    const arrivedAt = this.now();
    if (result.error) {
      throw new LagGuestSessionError(
        result.error.message ?? `the api refused a guest session (${result.error.code ?? "unknown"})`,
        result.error,
      );
    }
    const guest = asGuest(result);
    if (guest === null) {
      throw new LagGuestSessionError("the api answered /lag/session without a usable guest");
    }
    const lifetimeMs = this.lifetimeOf(guest, arrivedAt);
    return {
      guest,
      // never due as it arrives: a lifetime shorter than twice the renewal
      // window is renewed halfway through
      renewAtMs: arrivedAt + Math.max(lifetimeMs - this.renewWithinMs, lifetimeMs / 2),
      expiresAtMs: arrivedAt + lifetimeMs,
    };
  }

  // How long a guest that has just arrived has to live: on the api's clock,
  // from when the api signed its jwt (`iat`) to expires_at. A jwt that says
  // nothing of it leaves the device's clock to decide, unless that clock says
  // a guest made a moment ago is already due, or will live longer than A1's
  // 24 h: then it is off, and the guest has A1's 24 h.
  private lifetimeOf(guest: LagGuest, arrivedAt: number): number {
    const expiresAt = expiryOf(guest);
    const issuedAt = issuedAtOf(guest.by_client_jwt);
    if (issuedAt !== undefined && issuedAt < expiresAt) {
      return expiresAt - issuedAt;
    }
    const leftMs = expiresAt - arrivedAt;
    return this.renewWithinMs < leftMs && leftMs <= LAG_GUEST_TTL_MS ? leftMs : LAG_GUEST_TTL_MS;
  }

  private async read(): Promise<string | null | undefined> {
    let text: string | null | undefined;
    await this.attempt(async () => {
      text = await this.storage.get(this.key);
    });
    return text;
  }

  private async attempt(action: () => unknown): Promise<void> {
    try {
      await action();
    } catch {
      // storage is a convenience: without it the guest lasts this page
    }
  }
}

/** A LagStorage over a Map, for tests and pages that keep nothing. */
export function memoryLagStorage(): LagStorage {
  const values = new Map<string, string>();
  return {
    get: (key) => values.get(key) ?? null,
    set: (key, value) => {
      values.set(key, value);
    },
    remove: (key) => {
      values.delete(key);
    },
  };
}

/**
 * A LagStorage over `localStorage`, looked up on each call: touching it can
 * throw (blocked site data), which LagGuestSession treats as no storage.
 */
export function localLagStorage(): LagStorage {
  const storage = () => globalThis.localStorage;
  return {
    get: (key) => storage().getItem(key),
    set: (key, value) => storage().setItem(key, value),
    remove: (key) => storage().removeItem(key),
  };
}
