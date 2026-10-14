// The lag.dev guest session (lag.dev/PLAN.md S4, A1, L11). One guest per
// browser: created on first use, kept through an injected storage adapter so
// page loads reuse it, and replaced once it is within 10 minutes of
// expires_at. Creation is rate-limited per /24 (/48), so concurrent callers
// share one creation, and a guest that is due for renewal but not yet expired
// keeps working while renewal fails.

import type { RequestOptions } from "../api_client";
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

const parseStored = (text: string | null | undefined): LagGuest | null => {
  if (!text) {
    return null;
  }
  try {
    return asGuest(JSON.parse(text));
  } catch {
    return null;
  }
};

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
  private current: LagGuest | null = null;
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
      return Promise.resolve(this.current);
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
    if (jwt === undefined || this.current?.by_client_jwt === jwt) {
      this.current = null;
    }
    const stored = parseStored(await this.read());
    if (stored !== null && jwt !== undefined && stored.by_client_jwt !== jwt) {
      return;
    }
    await this.attempt(() => this.storage.remove(this.key));
  }

  private isFresh(guest: LagGuest): boolean {
    return this.now() < expiryOf(guest) - this.renewWithinMs;
  }

  private async load(): Promise<LagGuest> {
    const stored = parseStored(await this.read());
    let best = this.current;
    if (stored !== null && (best === null || expiryOf(best) < expiryOf(stored))) {
      best = stored;
    }
    const usable = best !== null && this.now() < expiryOf(best);
    if (best !== null && (this.isFresh(best) || (usable && this.now() < this.renewAfter))) {
      this.current = best;
      return best;
    }
    let created: LagGuest;
    try {
      created = await this.create();
    } catch (error) {
      // a guest due for renewal still works until it actually expires
      if (best !== null && usable) {
        const retryAfterMs = error instanceof LagGuestSessionError ? (error.retryAfterMs ?? 0) : 0;
        this.renewAfter = this.now() + Math.max(LAG_GUEST_RENEW_RETRY_MS, retryAfterMs);
        this.current = best;
        return best;
      }
      throw error;
    }
    this.current = created;
    await this.attempt(() => this.storage.set(this.key, JSON.stringify(created)));
    return created;
  }

  private async create(): Promise<LagGuest> {
    const result = await this.api.lagSession();
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
    return guest;
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
