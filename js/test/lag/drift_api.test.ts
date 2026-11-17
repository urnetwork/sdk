import { test } from "node:test";
import assert from "node:assert/strict";
import { registerTsResolve } from "../ts-resolve.ts";

// The drift test's widget routes (lag.dev/DRIFTTEST.md P5, W8) against a
// recording fetch, never the live api.

registerTsResolve();
const { createLagApiClient, DRIFT_TEST_DEMO_TOKEN } = await import("../../src/lag/api.ts");
const { URNetworkApiError } = await import("../../src/api_client.ts");

interface Recorded {
  url: string;
  method: string;
  headers: Record<string, string>;
  body: unknown;
}

const json = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

function recorder(responses: Response[]) {
  const calls: Recorded[] = [];
  const fetchImpl = async (input: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const headers: Record<string, string> = {};
    new Headers(init?.headers).forEach((v, k) => {
      headers[k] = v;
    });
    calls.push({
      url: String(input),
      method: init?.method ?? "GET",
      headers,
      body: typeof init?.body === "string" ? JSON.parse(init.body) : init?.body,
    });
    const next = responses.shift();
    if (next === undefined) {
      throw new Error("unexpected fetch");
    }
    return next;
  };
  return { calls, fetchImpl };
}

const TOKEN = "dt1_" + "A".repeat(43);
const ID = "0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f";
const ticket = {
  extender_id: ID,
  host: `${ID}.extender.bringyour.com`,
  position: 0,
  ticket: "t-1",
  location: { city: "Frankfurt", country_code: "de", country_name: "Germany", latitude: 50.11, longitude: 8.68 },
  place_error_km: 20,
};
const started = {
  drift_test_id: "0193aaaa-0000-7000-8000-000000000001",
  run: 1,
  runs_left: 5,
  track_id: "0193aaaa-0000-7000-8000-000000000002",
  tickets: [ticket],
  ip: { country_code: "de", country_name: "Germany", region: "Hesse", city: "Frankfurt", latitude: 50.11, longitude: 8.68 },
  measure_ms: 12000,
  expires_at: "2026-11-17T18:05:00Z",
};

test("driftTestStart posts the token and parent origin in the body, never a bearer", async () => {
  const { calls, fetchImpl } = recorder([json(200, started)]);
  const api = createLagApiClient({ fetch: fetchImpl, token: "guest-jwt" });
  const result = await api.driftTestStart({ token: TOKEN, parentOrigin: "https://example.com" });
  assert.deepEqual(result, started);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].url, "https://api.bringyour.com/lag/drift-test/start");
  assert.equal(calls[0].method, "POST");
  assert.deepEqual(calls[0].body, { token: TOKEN, parent_origin: "https://example.com" });
  assert.equal(calls[0].headers.authorization, undefined, "the token is the only credential");
  assert.ok(!calls[0].url.includes(TOKEN), "never in the url");
});

test("driftTestStart without a known parent origin sends none", async () => {
  const { calls, fetchImpl } = recorder([json(200, started), json(200, started)]);
  const api = createLagApiClient({ fetch: fetchImpl });
  await api.driftTestStart({ token: TOKEN });
  await api.driftTestStart({ token: TOKEN, parentOrigin: null });
  assert.deepEqual(calls[0].body, { token: TOKEN });
  assert.deepEqual(calls[1].body, { token: TOKEN });
});

test("W8: the demo token's start carries demo and no drift test id", async () => {
  const demo = { ...started, drift_test_id: undefined, demo: true };
  const { calls, fetchImpl } = recorder([json(200, demo)]);
  const api = createLagApiClient({ fetch: fetchImpl });
  assert.equal(DRIFT_TEST_DEMO_TOKEN, "demo");
  const result = await api.driftTestStart({ token: DRIFT_TEST_DEMO_TOKEN, parentOrigin: "https://example.com" });
  assert.equal(result.demo, true);
  assert.equal(result.drift_test_id, undefined);
  assert.equal(result.tickets.length, 1);
  assert.deepEqual(calls[0].body, { token: "demo", parent_origin: "https://example.com" });
});

test("driftTestStart answers a refusal as a result, and always lists tickets", async () => {
  const refusal = { error: { code: "measuring", message: "another run is measuring" } };
  const { fetchImpl } = recorder([json(409, refusal), json(200, { error: { code: "origin" } })]);
  const api = createLagApiClient({ fetch: fetchImpl });
  assert.deepEqual(await api.driftTestStart({ token: TOKEN }), { ...refusal, tickets: [] });
  const origin = await api.driftTestStart({ token: TOKEN });
  assert.equal(origin.error?.code, "origin");
  assert.deepEqual(origin.tickets, []);
});

test("driftTestFinish posts the run and its stop reason", async () => {
  const { calls, fetchImpl } = recorder([json(200, { run: 2, run_state: "processing" })]);
  const api = createLagApiClient({ fetch: fetchImpl, token: "guest-jwt" });
  assert.deepEqual(await api.driftTestFinish({ token: TOKEN, run: 2, stop: "converged" }), { run: 2, run_state: "processing" });
  assert.equal(calls[0].url, "https://api.bringyour.com/lag/drift-test/finish");
  assert.equal(calls[0].method, "POST");
  assert.deepEqual(calls[0].body, { token: TOKEN, run: 2, stop: "converged" });
  assert.equal(calls[0].headers.authorization, undefined);
});

test("driftTestFinish refuses a bad run, stop or token before any request", async () => {
  const { calls, fetchImpl } = recorder([]);
  const api = createLagApiClient({ fetch: fetchImpl });
  assert.throws(() => api.driftTestFinish({ token: TOKEN, run: 0, stop: "cap" }), TypeError);
  assert.throws(() => api.driftTestFinish({ token: TOKEN, run: 1.5, stop: "cap" }), TypeError);
  assert.throws(() => api.driftTestFinish({ token: TOKEN, run: 1, stop: "bored" as never }), TypeError);
  assert.throws(() => api.driftTestFinish({ token: "", run: 1, stop: "cap" }), TypeError);
  await assert.rejects(api.driftTestStart({ token: "" }), TypeError);
  await assert.rejects(api.driftTestResult({ token: undefined as never }), TypeError);
  assert.equal(calls.length, 0);
});

test("driftTestResult posts the token and reads only what the widget shows", async () => {
  const shown = {
    state: "active",
    run: 2,
    run_state: "complete",
    verdict: "agree",
    ip_country_name: "Germany",
    runs_in_verdict: [1, 2],
    retest_allowed: true,
    runs_left: 4,
  };
  const { calls, fetchImpl } = recorder([json(200, shown), json(200, { state: "created", run: 0, verdict: null })]);
  const api = createLagApiClient({ fetch: fetchImpl });
  assert.deepEqual(await api.driftTestResult({ token: TOKEN }), shown);
  assert.equal(calls[0].url, "https://api.bringyour.com/lag/drift-test/result");
  assert.deepEqual(calls[0].body, { token: TOKEN });
  // before any run completes: no verdict, and runs_in_verdict is still a list
  assert.deepEqual(await api.driftTestResult({ token: TOKEN }), { state: "created", run: 0, verdict: null, runs_in_verdict: [] });
});

test("W8: finish and result answer the demo token's refusal as a result, and other errors throw", async () => {
  const refusal = { error: { code: "demo", message: "the demo token stores nothing" } };
  const { fetchImpl } = recorder([json(400, refusal), json(400, refusal), json(400, { message: "bad request" })]);
  const api = createLagApiClient({ fetch: fetchImpl });
  assert.deepEqual(await api.driftTestFinish({ token: "demo", run: 1, stop: "clear" }), refusal);
  assert.deepEqual(await api.driftTestResult({ token: "demo" }), { ...refusal, runs_in_verdict: [] });
  await assert.rejects(api.driftTestResult({ token: "demo" }), (error: unknown) => error instanceof URNetworkApiError && error.status === 400);
});
