// fetchWithGetRetry retries an idempotent GET whose response is a transient
// gateway status (502/503: the lb momentarily had no healthy upstream, e.g.
// the edge of a deploy) or whose fetch failed at the network level. One
// jittered retry by default. Non-GET methods pass through untouched: the
// client cannot know whether a failed POST executed, so it is never
// replayed.

export type FetchLike = (
  input: string | URL | Request,
  init?: RequestInit,
) => Promise<Response>;

export interface GetRetryOptions {
  /** additional attempts after the first (default 1) */
  retryCount?: number;
  /** response statuses that trigger a retry (default 502, 503) */
  retryStatusCodes?: number[];
  /** jittered pause before a retry, uniform in [min, max) milliseconds */
  retryMinTimeoutMillis?: number;
  retryMaxTimeoutMillis?: number;
  /** injection point for tests (default globalThis.fetch) */
  fetchImpl?: FetchLike;
}

// the rejection value fetch itself uses for an abort: the signal's reason,
// or an AbortError when the runtime recorded none
const abortError = (signal: AbortSignal): unknown => {
  if (signal.reason !== undefined) {
    return signal.reason;
  }
  if (typeof DOMException !== "undefined") {
    return new DOMException("This operation was aborted", "AbortError");
  }
  const error = new Error("This operation was aborted");
  error.name = "AbortError";
  return error;
};

// sleep that rejects promptly with the abort reason when `signal` aborts —
// the retry backoff must not outlive the caller's cancellation
const sleep = (millis: number, signal?: AbortSignal) =>
  new Promise<void>((resolve, reject) => {
    if (!signal) {
      setTimeout(resolve, millis);
      return;
    }
    if (signal.aborted) {
      reject(abortError(signal));
      return;
    }
    const onAbort = () => {
      clearTimeout(timer);
      reject(abortError(signal));
    };
    const timer = setTimeout(() => {
      signal.removeEventListener("abort", onAbort);
      resolve();
    }, millis);
    signal.addEventListener("abort", onAbort, { once: true });
  });

export async function fetchWithGetRetry(
  input: string | URL | Request,
  init?: RequestInit,
  options?: GetRetryOptions,
): Promise<Response> {
  const fetchImpl = options?.fetchImpl ?? globalThis.fetch;
  // resolve the method the way fetch itself does: an explicit init overrides
  // a Request-object input, which overrides the GET default — a
  // `new Request(url, {method: "POST"})` must never be treated as a
  // retryable GET
  const request =
    typeof Request !== "undefined" && input instanceof Request
      ? input
      : undefined;
  const method = (init?.method ?? request?.method ?? "GET").toUpperCase();
  if (method !== "GET") {
    return fetchImpl(input, init);
  }
  const signal = init?.signal ?? request?.signal ?? undefined;

  const retryCount = options?.retryCount ?? 1;
  const retryStatusCodes = options?.retryStatusCodes ?? [502, 503];
  const retryMinTimeoutMillis = options?.retryMinTimeoutMillis ?? 100;
  const retryMaxTimeoutMillis = options?.retryMaxTimeoutMillis ?? 1000;

  const maxAttempt = Math.max(0, retryCount);
  let lastResponse: Response | undefined;
  let lastError: unknown;
  for (let attempt = 0; attempt <= maxAttempt; attempt += 1) {
    if (0 < attempt) {
      const jitter =
        retryMinTimeoutMillis +
        Math.random() *
          Math.max(0, retryMaxTimeoutMillis - retryMinTimeoutMillis);
      // rejects promptly on abort: an aborted caller never waits out the
      // jitter and never issues the retry fetch
      await sleep(jitter, signal);
    }
    try {
      const response = await fetchImpl(input, init);
      if (!retryStatusCodes.includes(response.status)) {
        return response;
      }
      lastResponse = response;
      lastError = undefined;
      if (attempt < maxAttempt) {
        // this response is discarded by the retry; release its body so the
        // unread stream does not pin the pooled connection. Only discarded
        // responses are cancelled — the final attempt's response is returned
        // to the caller with its body intact
        try {
          await response.body?.cancel();
        } catch {
          // best-effort: an already-consumed or locked body needs no release
        }
      }
    } catch (error) {
      // a network-level failure; safe to retry a GET
      lastError = error;
    }
  }
  if (lastError !== undefined) {
    throw lastError;
  }
  // the loop ran at least once and either returned, threw, or set lastResponse
  return lastResponse as Response;
}
