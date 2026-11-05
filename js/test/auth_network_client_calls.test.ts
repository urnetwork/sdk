import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync, readdirSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { registerTsResolve } from "./ts-resolve.ts";

registerTsResolve();
const { AuthNetworkClientCalls, authNetworkClientCancelledMessage } = await import(
  "../src/auth_network_client_calls.ts"
);
const { URNetworkAPI } = await import("../src/client.ts");

// A server that creates a client for every create it receives, whatever the
// caller does afterwards: an aborted request still creates it. `answer(i)`
// lets the i-th create answer; `active` is what the server still holds.
function fakeServer() {
  const active = new Set<string>();
  const pending: Array<() => void> = [];
  let next = 0;
  const create = () => {
    const clientId = `00000000-0000-4000-8000-${(next++).toString(16).padStart(12, "0")}`;
    active.add(clientId);
    return new Promise<{ by_client_jwt: string; client_id: string; proxy_config_result: null }>((resolve) => {
      pending.push(() =>
        resolve({ by_client_jwt: jwtFor(clientId), client_id: clientId, proxy_config_result: null }),
      );
    });
  };
  const release = async (clientId: string) => {
    active.delete(clientId);
    return {};
  };
  const answer = async (i: number) => {
    pending[i]();
    // let the call's continuation run
    await new Promise((r) => setTimeout(r, 0));
  };
  return { active, create, release, answer };
}

function jwtFor(clientId: string): string {
  const payload = Buffer.from(JSON.stringify({ client_id: clientId, network_id: "n" })).toString("base64url");
  return `h.${payload}.s`;
}

// The hook aborted the previous call on every new one: the server created the
// client anyway and its id was lost, so it stayed active with no holder. A
// superseded call now answers cancelled and its client is removed.
test("a superseded create answers cancelled and its client is removed", async () => {
  const server = fakeServer();
  const calls = new AuthNetworkClientCalls();
  const first = calls.run(server.create, server.release);
  const second = calls.run(server.create, server.release);
  await server.answer(0);
  await server.answer(1);
  const [firstOutcome, secondOutcome] = await Promise.all([first, second]);

  assert.equal(firstOutcome.current, false);
  assert.equal(firstOutcome.result.error?.message, authNetworkClientCancelledMessage);
  assert.equal(secondOutcome.current, true);
  assert.equal(secondOutcome.result.client_id, [...server.active][0]);
  // only the delivered client is left on the server
  assert.equal(server.active.size, 1);
});

// The hook aborted every call in flight on unmount, with the same leak.
test("a create in flight when the owner goes away is removed when it answers", async () => {
  const server = fakeServer();
  const calls = new AuthNetworkClientCalls();
  const inFlight = calls.run(server.create, server.release);
  calls.close();
  await server.answer(0);
  const outcome = await inFlight;
  assert.equal(outcome.current, false);
  assert.equal(server.active.size, 0);
});

test("a remounted owner (StrictMode) keeps its calls' answers", async () => {
  const server = fakeServer();
  const calls = new AuthNetworkClientCalls();
  const inFlight = calls.run(server.create, server.release);
  calls.close();
  calls.reopen();
  await server.answer(0);
  const outcome = await inFlight;
  assert.equal(outcome.current, true);
  assert.equal(server.active.size, 1);
});

test("sequential creates are all delivered", async () => {
  const server = fakeServer();
  const calls = new AuthNetworkClientCalls();
  for (let i = 0; i < 3; i++) {
    const call = calls.run(server.create, server.release);
    await server.answer(i);
    assert.equal((await call).current, true);
  }
  assert.equal(server.active.size, 3);
});

test("a superseded refusal releases nothing, and a failed release does not throw", async () => {
  const calls = new AuthNetworkClientCalls();
  let released = 0;
  let resolveRefusal: (v: unknown) => void = () => {};
  const refused = calls.run(
    () => new Promise((r) => { resolveRefusal = r; }) as never,
    async () => { released++; },
  );
  const newer = calls.run(
    async () => ({ proxy_config_result: null, client_id: "x" }),
    async () => { throw new Error("remove failed"); },
  );
  resolveRefusal({ proxy_config_result: null, error: { message: "limit", client_limit_exceeded: true } });
  assert.equal((await refused).current, false);
  assert.equal((await newer).current, true);
  assert.equal(released, 0);

  // a superseded create whose release fails still answers cancelled
  let resolveCreate: (v: unknown) => void = () => {};
  const superseded = calls.run(
    () => new Promise((r) => { resolveCreate = r; }) as never,
    async () => { throw new Error("remove failed"); },
  );
  void calls.run(async () => ({ proxy_config_result: null }), async () => {});
  resolveCreate({ proxy_config_result: null, client_id: "y" });
  const outcome = await superseded;
  assert.equal(outcome.current, false);
  assert.equal(outcome.result.error?.message, authNetworkClientCancelledMessage);
});

const json = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

// With x402 on, a plan limit refuses auth-client with HTTP 402 and the
// server's message in `error`; it read as "HTTP error! status: 402" with
// client_limit_exceeded false, so callers could neither tell it was a limit
// nor say which one.
test("a 402 from auth-client is a plan-limit refusal with the server's message", async () => {
  const message = "Your plan's limit of 5 active proxies is reached. Remove a proxy to create another, or upgrade to UR Pro for more.";
  const responses = [
    json(402, { x402Version: 1, error: message, accepts: [] }),
    json(200, { proxy_config_result: null, error: { message, client_limit_exceeded: true, upgrade_required: true } }),
    json(500, { error: { message: "boom" } }),
  ];
  const fetchImpl = async () => responses.shift()!;
  const api = new URNetworkAPI({
    baseURL: "https://api.example.test",
    fetch: fetchImpl,
    retry: { retryMinTimeoutMillis: 0, retryMaxTimeoutMillis: 1, maxRetries: 0 },
  });
  const params = { description: "d", device_spec: "s", proxy_config: null } as never;

  assert.deepEqual(await api.authNetworkClient(params, "t"), {
    proxy_config_result: null,
    error: { message, client_limit_exceeded: true, upgrade_required: true },
  });
  // the 200 answer is passed through as before
  assert.deepEqual((await api.authNetworkClient(params, "t")).error, {
    message,
    client_limit_exceeded: true,
    upgrade_required: true,
  });
  // any other failure keeps its legacy message
  const failed = await api.authNetworkClient(params, "t");
  assert.equal(failed.error?.client_limit_exceeded, false);
  assert.match(failed.error?.message ?? "", /HTTP error! status: 500/);
});

// A hook callback memoized without the auth token it reads kept the token of
// the render that made it: useRemoveNetworkClient's, made before sign-in
// finished, refused every remove with "token required", so the extension's
// provisioned proxies were never released.
test("every sdk hook callback that reads the token lists it as a dependency", () => {
  const dir = fileURLToPath(new URL("../src/react/hooks/", import.meta.url));
  const offenders: string[] = [];
  for (const name of readdirSync(dir).filter((n) => n.endsWith(".tsx") || n.endsWith(".ts"))) {
    const source = readFileSync(dir + name, "utf8");
    for (const call of useCallbackCalls(source)) {
      if (/\btoken\b/.test(call.body) && !/\btoken\b/.test(call.deps)) {
        offenders.push(`${name}: deps ${call.deps}`);
      }
    }
  }
  assert.deepEqual(offenders, []);
});

// The body and the dependency array of each `useCallback(` call in `source`,
// found by bracket matching (strings and comments are not special-cased; the
// hooks keep brackets balanced inside them).
function useCallbackCalls(source: string): Array<{ body: string; deps: string }> {
  const calls: Array<{ body: string; deps: string }> = [];
  let from = 0;
  for (;;) {
    const start = source.indexOf("useCallback(", from);
    if (start < 0) return calls;
    let depth = 0;
    let end = start + "useCallback".length;
    for (; end < source.length; end++) {
      const c = source[end];
      if (c === "(" || c === "[" || c === "{") depth++;
      else if (c === ")" || c === "]" || c === "}") {
        depth--;
        if (depth === 0) break;
      }
    }
    const inner = source.slice(start + "useCallback(".length, end);
    const depsStart = inner.lastIndexOf("[");
    calls.push({ body: inner.slice(0, depsStart), deps: inner.slice(depsStart) });
    from = end;
  }
}
