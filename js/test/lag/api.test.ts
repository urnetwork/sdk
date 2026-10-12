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

// extender ids, and the measurement names they give (N1)
const ID_A = "0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f";
const ID_B = "0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e10";
const hostOf = (id: string) => `${id}.extender.bringyour.com`;

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

test("lagExtenders sends the guest jwt and returns one entry per extender", async () => {
  const extender = {
    extender_id: ID_A,
    public_key: "ab".repeat(32),
    host: hostOf(ID_A),
    ips: ["203.0.113.7", "2001:db8::1"],
    location: { city: "Frankfurt", country_code: "de", country_name: "Germany", latitude: 50.1, longitude: 8.7 },
  };
  const { calls, fetchImpl } = recorder([json(200, { extenders: [extender] }), json(200, {})]);
  const api = createLagApiClient({ fetch: fetchImpl, token: async () => "jwt-2" });
  const result = await api.lagExtenders();
  assert.deepEqual(result, { extenders: [extender] });
  // both families under the one name, IPv4 first, and no single `ip`
  assert.deepEqual(result.extenders[0].ips, ["203.0.113.7", "2001:db8::1"]);
  assert.equal("ip" in result.extenders[0], false);
  assert.equal(calls[0].method, "GET");
  assert.equal(calls[0].headers["authorization"], "Bearer jwt-2");
  // a body without the list still gives one
  assert.deepEqual(await api.lagExtenders(), { extenders: [] });
});

test("lagExtenders gives every entry a list of ips, and keeps a host's port", async () => {
  // a local test stack's host carries a port; an entry may come without ips
  const local = { extender_id: ID_A, public_key: "cd".repeat(32), host: `${ID_A}.extender.localhost:8443`, ips: ["127.0.0.1"] };
  const bare = { extender_id: ID_B, public_key: "ef".repeat(32), host: hostOf(ID_B) };
  const noIps = { extender_id: ID_B, public_key: "ef".repeat(32), host: hostOf(ID_B), ips: null };
  const { fetchImpl } = recorder([json(200, { extenders: [local, bare, noIps] })]);
  const api = createLagApiClient({ fetch: fetchImpl, token: "t" });
  const { extenders } = await api.lagExtenders();
  assert.equal(extenders[0].host, `${ID_A}.extender.localhost:8443`);
  assert.deepEqual(extenders.map((extender) => extender.ips), [["127.0.0.1"], [], []]);
});

test("lagTrack posts the track's extender ids and returns its tickets", async () => {
  const tickets = {
    track_id: "t-1",
    tickets: [
      { extender_id: ID_A, host: hostOf(ID_A), position: 0, ticket: "AAAA" },
      { extender_id: ID_B, host: `${ID_B}.extender.localhost:8443`, position: 1, ticket: "BBBB" },
    ],
    expires_at: "2026-10-12T20:02:00Z",
  };
  const { calls, fetchImpl } = recorder([json(200, tickets), json(200, tickets)]);
  const api = createLagApiClient({ fetch: fetchImpl, token: "jwt-3" });
  const args = { extenders: [{ extender_id: ID_A }, { extender_id: ID_B }] };
  const result = await api.lagTrack(args);
  assert.deepEqual(result, tickets);
  assert.deepEqual(Object.keys(result.tickets[0]).sort(), ["extender_id", "host", "position", "ticket"]);
  assert.equal(result.tickets[1].host, `${ID_B}.extender.localhost:8443`);
  assert.equal(calls[0].url, "https://api.bringyour.com/lag/track");
  assert.equal(calls[0].method, "POST");
  assert.deepEqual(calls[0].body, args);
  assert.equal(calls[0].headers["authorization"], "Bearer jwt-3");

  // the request names each extender by its id alone, whatever else the
  // caller's objects carry
  const stale = { extenders: [{ extender_id: ID_A, ip: "203.0.113.7" }, { extender_id: ID_B, host: hostOf(ID_B) }] };
  await api.lagTrack(stale);
  assert.deepEqual(calls[1].body, args);
});

test("lagTrack without a trackId sends ids only, even when positions are given", async () => {
  const { calls, fetchImpl } = recorder([json(200, { track_id: "t-1", tickets: [] })]);
  const api = createLagApiClient({ fetch: fetchImpl, token: "jwt-3" });
  await api.lagTrack({ extenders: [{ extender_id: ID_A, position: 5 }, { extender_id: ID_B }] });
  assert.deepEqual(calls[0].body, { extenders: [{ extender_id: ID_A }, { extender_id: ID_B }] });
  assert.equal("track_id" in (calls[0].body as object), false);
});

test("lagTrack with a trackId reissues tickets at their positions in that track", async () => {
  const reissued = {
    track_id: "t-1",
    tickets: [
      { extender_id: ID_B, host: hostOf(ID_B), position: 3, ticket: "CCCC" },
      { extender_id: ID_A, host: hostOf(ID_A), position: 9, ticket: "DDDD" },
    ],
    expires_at: "2026-10-12T20:12:00Z",
  };
  const { calls, fetchImpl } = recorder([json(200, reissued)]);
  const api = createLagApiClient({ fetch: fetchImpl, token: "jwt-4" });
  const result = await api.lagTrack({
    trackId: "t-1",
    extenders: [
      { extender_id: ID_B, position: 3 },
      // what else a caller's object carries stays out of the request
      { extender_id: ID_A, position: 9, ip: "203.0.113.7" } as { extender_id: string; position: number },
    ],
  });
  // the response shape is unchanged
  assert.deepEqual(result, reissued);
  assert.equal(calls[0].url, "https://api.bringyour.com/lag/track");
  assert.equal(calls[0].method, "POST");
  assert.equal(calls[0].headers["authorization"], "Bearer jwt-4");
  assert.deepEqual(calls[0].body, {
    track_id: "t-1",
    extenders: [
      { extender_id: ID_B, position: 3 },
      { extender_id: ID_A, position: 9 },
    ],
  });
  // on the wire: {"track_id":"…","extenders":[{"extender_id":"…","position":3}, …]}
  assert.deepEqual(Object.keys(calls[0].body as object), ["track_id", "extenders"]);
});

test("a reissue without each extender's position is refused before any request", async () => {
  const { calls, fetchImpl } = recorder([]);
  const api = createLagApiClient({ fetch: fetchImpl, token: "t" });
  for (const position of [undefined, -1, 1.5, NaN, "3" as unknown as number]) {
    await assert.rejects(
      api.lagTrack({ trackId: "t-1", extenders: [{ extender_id: ID_A, position: 0 }, { extender_id: ID_B, position }] }),
      TypeError,
      String(position),
    );
  }
  await assert.rejects(api.lagTrack({ trackId: "", extenders: [{ extender_id: ID_A, position: 0 }] }), TypeError);
  assert.equal(calls.length, 0);
});

test("a reissue's refusal is a result too", async () => {
  const refusal = { error: { code: "limit", retry_after_ms: 0 } };
  const { fetchImpl } = recorder([json(429, refusal)]);
  const api = createLagApiClient({ fetch: fetchImpl, token: "t", retry: false });
  assert.deepEqual(
    await api.lagTrack({ trackId: "t-1", extenders: [{ extender_id: ID_A, position: 0 }] }),
    { ...refusal, tickets: [] },
  );
});

test("a refusal is a result whether the api answers it with 200 or an error status", async () => {
  const refusal = { error: { code: "limit", retry_after_ms: 60000 } };
  const { fetchImpl } = recorder([json(200, refusal), json(429, refusal), json(503, { error: { code: "unavailable" } })]);
  const api = createLagApiClient({ fetch: fetchImpl, token: "t", retry: false });
  const args = { extenders: [{ extender_id: ID_A }] };
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
