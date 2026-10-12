import { test } from "node:test";
import assert from "node:assert/strict";
import { registerTsResolve } from "../ts-resolve.ts";

registerTsResolve();
const { createLagApiClient, LagApiClient } = await import("../../src/lag/api.ts");
const { URNetworkApiError } = await import("../../src/api_client.ts");

// the lag wrappers against a recording fetch — never the live api

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

const guest = {
  by_client_jwt: "jwt-1",
  network_id: "n-1",
  client_id: "c-1",
  expires_at: "2026-10-13T20:00:00Z",
};

test("lagSession posts {} to api.bringyour.com without a bearer", async () => {
  const { calls, fetchImpl } = recorder([json(200, guest)]);
  const api = createLagApiClient({ fetch: fetchImpl, token: "should-not-be-sent" });
  assert.ok(api instanceof LagApiClient);
  assert.deepEqual(await api.lagSession(), guest);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].url, "https://api.bringyour.com/lag/session");
  assert.equal(calls[0].method, "POST");
  assert.deepEqual(calls[0].body, {});
  assert.equal(calls[0].headers["content-type"], "application/json");
  assert.equal(calls[0].headers["authorization"], undefined);
});

test("a configured base url is used, without its trailing slash", async () => {
  const { calls, fetchImpl } = recorder([json(200, { extenders: [] })]);
  const api = createLagApiClient({ fetch: fetchImpl, baseURL: "http://127.0.0.1:8080/", token: "t" });
  await api.lagExtenders();
  assert.equal(calls[0].url, "http://127.0.0.1:8080/lag/extenders");
});

test("lagExtenders sends the guest jwt from a token function", async () => {
  const extender = {
    extender_id: "e-1",
    public_key: "ab".repeat(32),
    ip: "203.0.113.7",
    host: "203-0-113-7.extender.bringyour.com",
    location: { city: "Frankfurt", country_code: "de", country_name: "Germany", latitude: 50.1, longitude: 8.7 },
  };
  const { calls, fetchImpl } = recorder([json(200, { extenders: [extender] }), json(200, {})]);
  const api = createLagApiClient({ fetch: fetchImpl, token: async () => "jwt-2" });
  assert.deepEqual(await api.lagExtenders(), { extenders: [extender] });
  assert.equal(calls[0].method, "GET");
  assert.equal(calls[0].headers["authorization"], "Bearer jwt-2");
  // a body without the list still gives one
  assert.deepEqual(await api.lagExtenders(), { extenders: [] });
});

test("lagTrack posts the track's extenders and returns its tickets", async () => {
  const tickets = {
    track_id: "t-1",
    tickets: [
      { extender_id: "e-1", ip: "203.0.113.7", host: "203-0-113-7.extender.bringyour.com", position: 0, ticket: "AAAA" },
    ],
    expires_at: "2026-10-12T20:02:00Z",
  };
  const { calls, fetchImpl } = recorder([json(200, tickets)]);
  const api = createLagApiClient({ fetch: fetchImpl, token: "jwt-3" });
  const args = { extenders: [{ extender_id: "e-1", ip: "203.0.113.7" }, { extender_id: "e-2", ip: "2001:db8::1" }] };
  assert.deepEqual(await api.lagTrack(args), tickets);
  assert.equal(calls[0].url, "https://api.bringyour.com/lag/track");
  assert.equal(calls[0].method, "POST");
  assert.deepEqual(calls[0].body, args);
  assert.equal(calls[0].headers["authorization"], "Bearer jwt-3");
});

test("a refusal is a result whether the api answers it with 200 or an error status", async () => {
  const refusal = { error: { code: "limit", retry_after_ms: 60000 } };
  const { fetchImpl } = recorder([json(200, refusal), json(429, refusal), json(503, { error: { code: "unavailable" } })]);
  const api = createLagApiClient({ fetch: fetchImpl, token: "t", retry: false });
  const args = { extenders: [{ extender_id: "e-1", ip: "203.0.113.7" }] };
  assert.deepEqual(await api.lagTrack(args), { ...refusal, tickets: [] });
  assert.deepEqual(await api.lagTrack(args), { ...refusal, tickets: [] });
  assert.deepEqual(await api.lagSession(), { error: { code: "unavailable" } });
});

test("an error without a refusal code still throws", async () => {
  const { fetchImpl } = recorder([json(401, { error: { message: "unauthorized" } }), json(500, "oops")]);
  const api = createLagApiClient({ fetch: fetchImpl, token: "t", retry: false });
  await assert.rejects(api.lagTrack({ extenders: [] }), (error: unknown) => {
    assert.ok(error instanceof URNetworkApiError);
    assert.equal(error.status, 401);
    assert.equal(error.message, "unauthorized");
    return true;
  });
  await assert.rejects(api.lagExtenders(), URNetworkApiError);
});

test("myIpInfo carries the network (A3) and sends no bearer", async () => {
  const info = {
    info: {
      ip: "198.51.100.4",
      location: { city: "Austin", country: { code: "us", name: "United States" }, coordinates: { lat: 30.3, lon: -97.7 } },
      asn: { number: 64500, organization: "Example" },
    },
    connected_to_network: false,
  };
  const { calls, fetchImpl } = recorder([json(200, info)]);
  const api = createLagApiClient({ fetch: fetchImpl, token: "t" });
  const result = await api.myIpInfo();
  assert.equal(result.info.asn?.organization, "Example");
  assert.equal(result.info.asn?.number, 64500);
  assert.equal(calls[0].url, "https://api.bringyour.com/my-ip-info");
  assert.equal(calls[0].headers["authorization"], undefined);
});
