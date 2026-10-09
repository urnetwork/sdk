import { test } from "node:test";
import assert from "node:assert/strict";
import { registerTsResolve } from "./ts-resolve.ts";

registerTsResolve();
const {
  createURNetworkApiClient,
  URNetworkApiClient,
  URNetworkApiError,
  URNetworkAPI,
  DEFAULT_API_BASE_URL,
} = await import("../src/client.ts");

// the generated client against a recording fetch — never the live api

interface Recorded {
  url: string;
  method: string;
  headers: Record<string, string>;
  body: unknown;
}

const json = (status: number, body: unknown, init: ResponseInit = {}) =>
  new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
    ...init,
  });

function recorder(responses: Array<Response | Error | (() => Response | Error)>) {
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
      body: init?.body,
    });
    const next = responses.shift();
    if (next === undefined) {
      throw new Error("unexpected fetch");
    }
    const value = typeof next === "function" ? next() : next;
    if (value instanceof Error) {
      throw value;
    }
    return value;
  };
  return { calls, fetchImpl };
}

const fastRetry = { retryMinTimeoutMillis: 0, retryMaxTimeoutMillis: 1 };

test("POST json: url, method, bearer, content type and body", async () => {
  const { calls, fetchImpl } = recorder([json(200, { api_key: "k", name: "n" })]);
  const client = createURNetworkApiClient({
    baseURL: "https://api.example.test/",
    token: "jwt-1",
    fetch: fetchImpl,
    headers: { "X-Client-Version": "1.0.0-test" },
  });
  const result = await client.accountCreateApiKey({ name: "n" });
  assert.deepEqual(result, { api_key: "k", name: "n" });
  assert.equal(calls.length, 1);
  assert.equal(calls[0].url, "https://api.example.test/account/api-key");
  assert.equal(calls[0].method, "POST");
  assert.equal(calls[0].headers["authorization"], "Bearer jwt-1");
  assert.equal(calls[0].headers["content-type"], "application/json");
  assert.equal(calls[0].headers["x-client-version"], "1.0.0-test");
  assert.equal(calls[0].body, JSON.stringify({ name: "n" }));
});

test("public operations send no token unless the call passes one", async () => {
  const { calls, fetchImpl } = recorder([json(200, {}), json(200, {})]);
  const client = createURNetworkApiClient({ token: "jwt-1", fetch: fetchImpl });
  assert.equal(client.baseURL, DEFAULT_API_BASE_URL);
  await client.authLogin({ user_auth: "a@b.c" });
  assert.equal(calls[0].url, "https://api.bringyour.com/auth/login");
  assert.equal(calls[0].headers["authorization"], undefined);
  await client.authLogin({ user_auth: "a@b.c" }, { token: "explicit" });
  assert.equal(calls[1].headers["authorization"], "Bearer explicit");
});

test("token function is read per request; setToken replaces it", async () => {
  const { calls, fetchImpl } = recorder([json(200, {}), json(200, {}), json(200, {})]);
  let current = "a";
  const client = new URNetworkApiClient({ token: async () => current, fetch: fetchImpl });
  await client.accountGetApiKeys();
  current = "b";
  await client.accountGetApiKeys();
  client.setToken(null);
  await client.accountGetApiKeys();
  assert.deepEqual(
    calls.map((c) => c.headers["authorization"]),
    ["Bearer a", "Bearer b", undefined],
  );
  // a GET carries no body and no content type
  assert.equal(calls[0].method, "GET");
  assert.equal(calls[0].body, undefined);
  assert.equal(calls[0].headers["content-type"], undefined);
});

test("path and query params are encoded; header params are sent", async () => {
  const { calls, fetchImpl } = recorder([
    json(200, { public_key: null }),
    json(200, {}),
    json(200, { ok: true }),
  ]);
  const client = createURNetworkApiClient({ fetch: fetchImpl });
  await client.getClientKey({ clientId: "a b/c" });
  assert.equal(calls[0].url, "https://api.bringyour.com/key/a%20b%2Fc");
  await client.snArtifactHistory({ deployment_id: "d&x", netuid: 7, limit: undefined, after: "a b" });
  const url = new URL(calls[1].url);
  assert.equal(url.pathname, "/sn/artifacts");
  assert.deepEqual([...url.searchParams], [["deployment_id", "d&x"], ["netuid", "7"], ["after", "a b"]]);
  await client.x402Purchase({ "X-PAYMENT": "signed" }, { sku_id: "s" });
  assert.equal(calls[2].headers["x-payment"], "signed");
});

test("a GET is retried once on 503; a POST is never replayed", async () => {
  const { calls, fetchImpl } = recorder([
    new Response("busy", { status: 503 }),
    json(200, { locations: [] }),
  ]);
  const client = createURNetworkApiClient({ fetch: fetchImpl, retry: fastRetry });
  const result = await client.networkProviderLocations();
  assert.deepEqual(result, { locations: [] });
  assert.equal(calls.length, 2);

  const post = recorder([new Response("busy", { status: 503 })]);
  const postClient = createURNetworkApiClient({ fetch: post.fetchImpl, retry: fastRetry });
  await assert.rejects(postClient.networkFindProviderLocations({ query: "de" }), (error: unknown) => {
    assert.ok(error instanceof URNetworkApiError);
    assert.equal(error.kind, "http");
    assert.equal(error.status, 503);
    return true;
  });
  assert.equal(post.calls.length, 1);
});

test("retry: false disables the GET retry", async () => {
  const { calls, fetchImpl } = recorder([new Response("", { status: 502 })]);
  const client = createURNetworkApiClient({ fetch: fetchImpl, retry: false });
  await assert.rejects(client.networkProviderLocations(), URNetworkApiError);
  assert.equal(calls.length, 1);
});

test("a non-2xx maps to URNetworkApiError with the server's message and body", async () => {
  const { fetchImpl } = recorder([json(400, { error: { message: "bad name" } }, { statusText: "Bad Request" })]);
  const client = createURNetworkApiClient({ fetch: fetchImpl });
  await assert.rejects(client.accountCreateApiKey({ name: "" }), (error: unknown) => {
    assert.ok(error instanceof URNetworkApiError);
    assert.equal(error.kind, "http");
    assert.equal(error.status, 400);
    assert.equal(error.statusText, "Bad Request");
    assert.equal(error.message, "bad name");
    assert.deepEqual(error.body, { error: { message: "bad name" } });
    assert.equal(error.operationId, "accountCreateApiKey");
    assert.equal(error.method, "POST");
    assert.equal(error.url, "https://api.bringyour.com/account/api-key");
    return true;
  });
});

test("an html gateway page and a network failure are URNetworkApiErrors too", async () => {
  const { fetchImpl } = recorder([
    new Response("<html>bad gateway</html>", { status: 502 }),
    new Response("<html>bad gateway</html>", { status: 502 }),
    new TypeError("fetch failed"),
    new TypeError("fetch failed"),
  ]);
  const client = createURNetworkApiClient({ fetch: fetchImpl, retry: fastRetry });
  await assert.rejects(client.accountGetApiKeys(), (error: unknown) => {
    assert.ok(error instanceof URNetworkApiError);
    assert.equal(error.kind, "http");
    assert.equal(error.body, "<html>bad gateway</html>");
    assert.match(error.message, /GET \/account\/api-keys failed: HTTP 502/);
    return true;
  });
  await assert.rejects(client.accountGetApiKeys(), (error: unknown) => {
    assert.ok(error instanceof URNetworkApiError);
    assert.equal(error.kind, "network");
    assert.equal(error.status, 0);
    assert.ok(error.cause instanceof TypeError);
    return true;
  });
});

test("a 2xx that is not json is a parse error; an empty 2xx is {}", async () => {
  const { fetchImpl } = recorder([
    new Response("<html>portal</html>", { status: 200 }),
    new Response(null, { status: 200 }),
  ]);
  const client = createURNetworkApiClient({ fetch: fetchImpl });
  await assert.rejects(client.accountGetApiKeys(), (error: unknown) => {
    assert.ok(error instanceof URNetworkApiError);
    assert.equal(error.kind, "parse");
    return true;
  });
  assert.deepEqual(await client.accountGetApiKeys(), {});
});

test("timeoutMs aborts the request and rejects with kind timeout", async () => {
  const fetchImpl = (_input: string | URL | Request, init?: RequestInit) =>
    new Promise<Response>((_resolve, reject) => {
      init?.signal?.addEventListener("abort", () => {
        const error = new Error("aborted");
        error.name = "AbortError";
        reject(error);
      });
    });
  const client = createURNetworkApiClient({ fetch: fetchImpl, timeoutMs: 20 });
  await assert.rejects(client.authLogin({ user_auth: "x" }), (error: unknown) => {
    assert.ok(error instanceof URNetworkApiError);
    assert.equal(error.kind, "timeout");
    assert.equal(error.isTimeout, true);
    return true;
  });
});

test("a caller abort rejects with the AbortError itself", async () => {
  const fetchImpl = (_input: string | URL | Request, init?: RequestInit) =>
    new Promise<Response>((_resolve, reject) => {
      init?.signal?.addEventListener("abort", () => reject(init.signal!.reason));
    });
  const client = createURNetworkApiClient({ fetch: fetchImpl, timeoutMs: 5000 });
  const controller = new AbortController();
  const pending = client.authLogin({ user_auth: "x" }, { signal: controller.signal });
  controller.abort();
  await assert.rejects(pending, (error: unknown) => {
    assert.ok(!(error instanceof URNetworkApiError));
    assert.equal((error as Error).name, "AbortError");
    return true;
  });
});

test("form bodies are url-encoded; binary responses are blobs", async () => {
  const { calls, fetchImpl } = recorder([
    json(200, { access_token: "t" }),
    new Response(new Uint8Array([137, 80, 78, 71]), { status: 200, headers: { "Content-Type": "image/png" } }),
  ]);
  const client = createURNetworkApiClient({ fetch: fetchImpl });
  await client.oauthToken({ grant_type: "authorization_code", client_id: "c", code: "x y" });
  assert.ok(calls[0].body instanceof URLSearchParams);
  assert.equal(String(calls[0].body), "grant_type=authorization_code&client_id=c&code=x+y");
  const png = await client.deviceShareCodeQr({ code: "abc" });
  assert.ok(png instanceof Blob);
  assert.equal(png.size, 4);
  assert.equal(calls[1].url, "https://api.bringyour.com/device/share-code/abc/qr.png");
});

test("request() reaches a route by hand with the same behavior", async () => {
  const { calls, fetchImpl } = recorder([json(200, { ok: 1 })]);
  const client = createURNetworkApiClient({ token: "t", fetch: fetchImpl });
  const result = await client.request<{ ok: number }>({
    method: "GET",
    path: "/things/{id}",
    params: { id: "a/b", page: 2 },
  });
  assert.deepEqual(result, { ok: 1 });
  assert.equal(calls[0].url, "https://api.bringyour.com/things/a%2Fb?page=2");
  assert.equal(calls[0].headers["authorization"], "Bearer t");
});

// the legacy wrapper keeps its result-shaped contract on top of the client
test("URNetworkAPI keeps its legacy contract", async () => {
  const errors = console.error;
  const logs = console.log;
  console.error = () => {};
  console.log = () => {};
  try {
    const { calls, fetchImpl } = recorder([
      json(500, { error: { message: "boom" } }),
      new Response("bad code", { status: 400 }),
      json(200, { network: { by_jwt: "j" } }),
      new Response("", { status: 503 }),
      new Response("", { status: 503 }),
      json(200, { api_keys: [] }),
    ]);
    const api = new URNetworkAPI({ baseURL: "https://api.example.test", fetch: fetchImpl, retry: fastRetry });
    assert.ok(api.client instanceof URNetworkApiClient);

    assert.deepEqual(await api.authLogin({ user_auth: "a" }), {
      error: { message: "HTTP error! status: 500" },
    });
    assert.deepEqual(await api.authCodeLogin({ auth_code: "c" }), {
      by_jwt: "",
      error: { message: "bad code" },
    });

    // networkCreate posts to the route the api serves
    const created = await api.networkCreate({ terms: true, guest_mode: true, user_auth: "u", password: "p" });
    assert.deepEqual(created, { network: { by_jwt: "j" } });
    assert.equal(calls[2].url, "https://api.example.test/auth/network-create");
    assert.deepEqual(JSON.parse(String(calls[2].body)), {
      terms: true,
      guest_mode: false,
      user_auth: "u",
      password: "p",
    });

    await assert.rejects(api.networkProviderLocations(), /Failed to fetch provider locations: 503/);
    assert.equal(calls.length, 5); // the GET was retried once

    assert.deepEqual(await api.listApiKeys("tok"), { api_keys: [] });
    assert.equal(calls[5].headers["authorization"], "Bearer tok");

    assert.deepEqual(await api.networkCreate({ terms: false, guest_mode: false }), {
      error: { message: "Terms must be accepted to create a network." },
    });
    assert.equal(calls.length, 6);
  } finally {
    console.error = errors;
    console.log = logs;
  }
});

test("URNetworkAPI.authNetworkClient rethrows an abort", async () => {
  const logs = console.log;
  console.log = () => {};
  try {
    const fetchImpl = (_input: string | URL | Request, init?: RequestInit) =>
      new Promise<Response>((_resolve, reject) => {
        init?.signal?.addEventListener("abort", () => reject(init.signal!.reason));
      });
    const api = new URNetworkAPI({ fetch: fetchImpl });
    const controller = new AbortController();
    const pending = api.authNetworkClient({ description: "d", device_spec: "s" }, "tok", controller.signal);
    controller.abort();
    await assert.rejects(pending, { name: "AbortError" });
  } finally {
    console.log = logs;
  }
});

test("optional-auth operations work anonymously and send the token when there is one", async () => {
  const { calls, fetchImpl } = recorder([json(200, {}), json(200, {})]);
  const anonymous = createURNetworkApiClient({ fetch: fetchImpl });
  await anonymous.statsPointsLeaderboard({ sort: "points" } as never);
  assert.equal(calls[0].url, "https://api.bringyour.com/stats/points-leaderboard");
  assert.equal(calls[0].headers["authorization"], undefined);
  const signedIn = createURNetworkApiClient({ token: "jwt-1", fetch: fetchImpl });
  await signedIn.statsPointsLeaderboard({ sort: "points" } as never);
  assert.equal(calls[1].headers["authorization"], "Bearer jwt-1");
});

// The operations the embed work added to the spec (data caps, ACL groups, the
// Services inquiry) and the older routes it documented. A spec edit that
// changes any of these wire shapes fails here.
type Client = ReturnType<typeof createURNetworkApiClient>;
const generatedOperations: Array<{
  name: string;
  call: (client: Client) => Promise<unknown>;
  method: "GET" | "POST";
  path: string;
  query?: Array<[string, string]>;
  bearer: boolean;
  body?: unknown;
}> = [
  { name: "getNetworkClientDataCap", call: c => c.getNetworkClientDataCap({ client_id: "c 1" }),
    method: "GET", path: "/network/client-data-cap", query: [["client_id", "c 1"]], bearer: true },
  { name: "setNetworkClientDataCap", call: c => c.setNetworkClientDataCap({ client_id: "c1", monthly_byte_limit: 10000000000 }),
    method: "POST", path: "/network/client-data-cap", bearer: true, body: { client_id: "c1", monthly_byte_limit: 10000000000 } },
  { name: "listNetworkClientDataCaps", call: c => c.listNetworkClientDataCaps({ cursor: "next/1", limit: 50 }),
    method: "GET", path: "/network/client-data-caps", query: [["cursor", "next/1"], ["limit", "50"]], bearer: true },
  { name: "getNetworkClientAclGroup", call: c => c.getNetworkClientAclGroup({ client_id: "c1" }),
    method: "GET", path: "/network/client-acl-group", query: [["client_id", "c1"]], bearer: true },
  { name: "setNetworkClientAclGroup", call: c => c.setNetworkClientAclGroup({ client_id: "c1", acl_group: "isolated" }),
    method: "POST", path: "/network/client-acl-group", bearer: true, body: { client_id: "c1", acl_group: "isolated" } },
  { name: "servicesContactSales",
    call: c => c.servicesContactSales({ name: "N", email: "n@example.com", company: "C", monthly_active_users: 5000, monthly_data_budget_byte_count: 10995116277760, message: "embed" }),
    method: "POST", path: "/services/contact-sales", bearer: false,
    body: { name: "N", email: "n@example.com", company: "C", monthly_active_users: 5000, monthly_data_budget_byte_count: 10995116277760, message: "embed" } },
  { name: "getProviderWorkOwners", call: c => c.getProviderWorkOwners({ domain: "d", client: "c", key: "k", generation: 3 } as never),
    method: "GET", path: "/provider-work/v1/owners", query: [["domain", "d"], ["client", "c"], ["key", "k"], ["generation", "3"]], bearer: false },
  { name: "listProviderWorkRequests", call: c => c.listProviderWorkRequests({ domain: "d", client: "c", generation: 3, key: "k" } as never),
    method: "GET", path: "/provider-work/v1/requests", query: [["domain", "d"], ["client", "c"], ["generation", "3"], ["key", "k"]], bearer: false },
  { name: "getProviderWorkRequest", call: c => c.getProviderWorkRequest({ requestHash: "ab/cd" } as never),
    method: "GET", path: "/provider-work/v1/requests/ab%2Fcd", bearer: false },
  { name: "getProviderWorkCut", call: c => c.getProviderWorkCut({ cutHash: "ef" } as never),
    method: "GET", path: "/provider-work/v1/cuts/ef", bearer: false },
  { name: "getProviderWorkWindow", call: c => c.getProviderWorkWindow({ domain: "d", epoch: 7, artifact: "a", authority: "o" } as never),
    method: "GET", path: "/provider-work/v1/windows", query: [["domain", "d"], ["epoch", "7"], ["artifact", "a"], ["authority", "o"]], bearer: false },
  { name: "getVerifyOriginalRequest", call: c => c.getVerifyOriginalRequest({ request_id: "r" } as never),
    method: "POST", path: "/verify/original", bearer: false, body: { request_id: "r" } },
  { name: "registerNetworkClient", call: c => c.registerNetworkClient({ registration_id: "g" } as never),
    method: "POST", path: "/network/register-client-v1", bearer: true, body: { registration_id: "g" } },
  { name: "extenderRelease", call: c => c.extenderRelease({} as never),
    method: "POST", path: "/network/extender-release", bearer: true, body: {} },
  { name: "extenderBlockReport", call: c => c.extenderBlockReport({ extender_id: "x" } as never),
    method: "POST", path: "/network/extender-block-report", bearer: true, body: { extender_id: "x" } },
  { name: "testBalanceDrain", call: c => c.testBalanceDrain({ minutes: 5 } as never),
    method: "POST", path: "/test/balance-drain", bearer: true, body: { minutes: 5 } },
  { name: "testBalanceRestore", call: c => c.testBalanceRestore({} as never),
    method: "POST", path: "/test/balance-restore", bearer: true, body: {} },
];

for (const op of generatedOperations) {
  test(`generated ${op.name}: ${op.method} ${op.path}, ${op.bearer ? "bearer" : "no token"}`, async () => {
    const { calls, fetchImpl } = recorder([json(200, {})]);
    const client = createURNetworkApiClient({ baseURL: "https://api.example.test", token: "jwt-1", fetch: fetchImpl });
    assert.equal(typeof (client as unknown as Record<string, unknown>)[op.name], "function");
    await op.call(client);
    assert.equal(calls.length, 1);
    const url = new URL(calls[0].url);
    assert.equal(calls[0].method, op.method);
    assert.equal(url.origin + url.pathname, `https://api.example.test${op.path}`);
    assert.deepEqual([...url.searchParams], op.query ?? []);
    assert.equal(calls[0].headers["authorization"], op.bearer ? "Bearer jwt-1" : undefined);
    if (op.body === undefined) {
      assert.equal(calls[0].body, undefined);
    } else {
      assert.equal(calls[0].headers["content-type"], "application/json");
      assert.equal(calls[0].body, JSON.stringify(op.body));
    }
  });
}

test("setNetworkClientDataCap sends merge semantics as written: omitted stays absent, null and 0 are sent", async () => {
  const { calls, fetchImpl } = recorder([json(200, {}), json(200, {}), json(200, {})]);
  const client = createURNetworkApiClient({ token: "jwt-1", fetch: fetchImpl });
  await client.setNetworkClientDataCap({ client_id: "c1", monthly_byte_limit: 5000000000 });
  await client.setNetworkClientDataCap({ client_id: "c1", total_byte_limit: null });
  await client.setNetworkClientDataCap({ client_id: "c1", monthly_byte_limit: 0, reset_total: true });
  const bodies = calls.map(call => JSON.parse(String(call.body)));
  assert.deepEqual(bodies[0], { client_id: "c1", monthly_byte_limit: 5000000000 });
  assert.equal("total_byte_limit" in bodies[0], false);
  assert.deepEqual(bodies[1], { client_id: "c1", total_byte_limit: null });
  assert.deepEqual(bodies[2], { client_id: "c1", monthly_byte_limit: 0, reset_total: true });
});
