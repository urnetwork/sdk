// run with `npm test` (node --test; node >= 23.6 strips types natively)
import { test } from "node:test";
import assert from "node:assert/strict";

import { fetchWithGetRetry } from "../src/utils/fetch_retry.ts";

// fast jitter so tests do not sleep for real
const fastRetry = {
  retryMinTimeoutMillis: 1,
  retryMaxTimeoutMillis: 2,
};

// a fetch stub that pops one scripted result per call; a status number
// becomes a Response, an Error is thrown (network-level failure)
const scriptedFetch = (script: Array<number | Error>) => {
  const calls: Array<string> = [];
  const fetchImpl = async (
    input: string | URL | Request,
    init?: RequestInit,
  ): Promise<Response> => {
    calls.push(`${(init?.method ?? "GET").toUpperCase()} ${input}`);
    const next = script.shift();
    if (next === undefined) {
      throw new Error("script exhausted");
    }
    if (next instanceof Error) {
      throw next;
    }
    return new Response(`status ${next}`, { status: next });
  };
  return { calls, fetchImpl };
};

test("GET retries a transient gateway status", async () => {
  const { calls, fetchImpl } = scriptedFetch([502, 200]);
  const response = await fetchWithGetRetry(
    "https://api.test/thing",
    { method: "GET" },
    { ...fastRetry, fetchImpl },
  );
  assert.equal(response.status, 200);
  assert.equal(calls.length, 2);
});

test("GET retries a network-level failure", async () => {
  const { calls, fetchImpl } = scriptedFetch([
    new TypeError("fetch failed"),
    200,
  ]);
  const response = await fetchWithGetRetry(
    "https://api.test/thing",
    { method: "GET" },
    { ...fastRetry, fetchImpl },
  );
  assert.equal(response.status, 200);
  assert.equal(calls.length, 2);
});

test("GET does not retry a deterministic application error", async () => {
  const { calls, fetchImpl } = scriptedFetch([500]);
  const response = await fetchWithGetRetry(
    "https://api.test/thing",
    { method: "GET" },
    { ...fastRetry, fetchImpl },
  );
  assert.equal(response.status, 500);
  assert.equal(calls.length, 1);
});

test("GET surfaces the last gateway status when retries exhaust", async () => {
  const { calls, fetchImpl } = scriptedFetch([503, 503]);
  const response = await fetchWithGetRetry(
    "https://api.test/thing",
    { method: "GET" },
    { ...fastRetry, fetchImpl },
  );
  assert.equal(response.status, 503);
  assert.equal(calls.length, 2);
});

test("GET surfaces the last network failure when retries exhaust", async () => {
  const { calls, fetchImpl } = scriptedFetch([
    new TypeError("fetch failed"),
    new TypeError("fetch failed again"),
  ]);
  await assert.rejects(
    fetchWithGetRetry(
      "https://api.test/thing",
      { method: "GET" },
      { ...fastRetry, fetchImpl },
    ),
    /fetch failed again/,
  );
  assert.equal(calls.length, 2);
});

test("POST is never replayed", async () => {
  const { calls, fetchImpl } = scriptedFetch([502]);
  const response = await fetchWithGetRetry(
    "https://api.test/thing",
    { method: "POST", body: "{}" },
    { ...fastRetry, fetchImpl },
  );
  assert.equal(response.status, 502);
  assert.equal(calls.length, 1);
});

test("a missing method defaults to GET and retries", async () => {
  const { calls, fetchImpl } = scriptedFetch([502, 200]);
  const response = await fetchWithGetRetry("https://api.test/thing", undefined, {
    ...fastRetry,
    fetchImpl,
  });
  assert.equal(response.status, 200);
  assert.equal(calls.length, 2);
});

test("a Request-object POST is never replayed", async () => {
  const { calls, fetchImpl } = scriptedFetch([502]);
  const response = await fetchWithGetRetry(
    new Request("https://api.test/thing", { method: "POST" }),
    undefined,
    { ...fastRetry, fetchImpl },
  );
  assert.equal(response.status, 502);
  assert.equal(calls.length, 1);
});

test("a Request-object GET retries a transient gateway status", async () => {
  const { calls, fetchImpl } = scriptedFetch([503, 200]);
  const response = await fetchWithGetRetry(
    new Request("https://api.test/thing"),
    undefined,
    { ...fastRetry, fetchImpl },
  );
  assert.equal(response.status, 200);
  assert.equal(calls.length, 2);
});

test("abort during the backoff rejects promptly without a second fetch", async () => {
  const { calls, fetchImpl } = scriptedFetch([502, 200]);
  const abort = new AbortController();
  const pending = fetchWithGetRetry(
    "https://api.test/thing",
    { method: "GET", signal: abort.signal },
    {
      // a jitter long enough that an unobserved abort would hang the test
      retryMinTimeoutMillis: 60_000,
      retryMaxTimeoutMillis: 60_000,
      fetchImpl,
    },
  );
  // let the first fetch resolve and the backoff sleep begin
  await new Promise((resolve) => setImmediate(resolve));
  abort.abort();
  await assert.rejects(pending, (error: unknown) => {
    assert.equal((error as Error).name, "AbortError");
    return true;
  });
  // the retry fetch was never issued
  assert.equal(calls.length, 1);
});

test("the discarded gateway response body is cancelled before the retry", async () => {
  let cancelCount = 0;
  let served = 0;
  const fetchImpl = async (): Promise<Response> => {
    served += 1;
    if (served === 1) {
      const body = new ReadableStream({
        start(controller) {
          controller.enqueue(new TextEncoder().encode("status 502"));
        },
        cancel() {
          cancelCount += 1;
        },
      });
      return new Response(body, { status: 502 });
    }
    return new Response("status 200", { status: 200 });
  };
  const response = await fetchWithGetRetry(
    "https://api.test/thing",
    { method: "GET" },
    { ...fastRetry, fetchImpl },
  );
  assert.equal(response.status, 200);
  assert.equal(served, 2);
  // the 502's stream was released so the connection is not pinned
  assert.equal(cancelCount, 1);
  // the returned response's body is untouched and readable
  assert.equal(response.bodyUsed, false);
  assert.equal(await response.text(), "status 200");
});

test("the final gateway response keeps a readable body when retries exhaust", async () => {
  const { calls, fetchImpl } = scriptedFetch([503, 503]);
  const response = await fetchWithGetRetry(
    "https://api.test/thing",
    { method: "GET" },
    { ...fastRetry, fetchImpl },
  );
  assert.equal(response.status, 503);
  assert.equal(calls.length, 2);
  // only discarded responses are cancelled; the surfaced one stays readable
  assert.equal(response.bodyUsed, false);
  assert.equal(await response.text(), "status 503");
});
