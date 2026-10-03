import { test } from "node:test";
import assert from "node:assert/strict";
import { registerTsResolve } from "./ts-resolve.ts";

registerTsResolve();
const { createURNetworkApiClient } = await import("../src/client.ts");

const intent = () => ({
  request_id: "00000000-0000-4000-8000-000000000001",
  to_address: "synthetic-destination",
  amount_usdc_nano_cents: 1_000_001_000,
  terms: true,
});

test("customer transfer refuses missing intent and unsafe money before token or HTTP", async () => {
  let effects = 0;
  const client = createURNetworkApiClient({
    token: () => { effects++; return "synthetic-token"; },
    fetch: async () => { effects++; return new Response("{}"); },
  });
  for (const amount of [0, -1000, 1, 1001, NaN, Infinity, Number.MAX_SAFE_INTEGER + 1, 9_007_199_254_742_000]) {
    await assert.rejects(client.walletCircleTransferOut({ ...intent(), amount_usdc_nano_cents: amount }), /exact positive safe integer/);
  }
  for (const request_id of [undefined, "", "00000000-0000-0000-0000-000000000000"]) {
    await assert.rejects(client.walletCircleTransferOut({ ...intent(), request_id } as any), /persisted request_id/);
  }
  assert.equal(effects, 0);
});

test("customer transfer persists request identity and freezes it before token await", async () => {
  let enter!: () => void;
  let release!: () => void;
  const entered = new Promise<void>(resolve => { enter = resolve; });
  const released = new Promise<void>(resolve => { release = resolve; });
  const bodies: string[] = [];
  const client = createURNetworkApiClient({
    token: async () => { enter(); await released; return "synthetic-token"; },
    fetch: async (_url, init) => {
      bodies.push(String(init?.body));
      return new Response(JSON.stringify({ challenge_id: "synthetic-challenge" }), { headers: { "Content-Type": "application/json" } });
    },
  });
  const original = intent();
  const saved = JSON.stringify(original);
  const pending = client.walletCircleTransferOut(original);
  await entered;
  original.request_id = "00000000-0000-4000-8000-000000000002";
  original.amount_usdc_nano_cents = 2000;
  release();
  await pending;
  await client.walletCircleTransferOut(JSON.parse(saved));
  await client.walletCircleTransferOut(original);
  assert.deepEqual(bodies, [saved, saved, JSON.stringify(original)]);
});

test("customer transfer preserves cancellation before any new challenge", async () => {
  let effects = 0;
  const client = createURNetworkApiClient({
    token: () => { effects++; return "synthetic-token"; },
    fetch: async () => { effects++; return new Response("{}"); },
  });
  const owner = new AbortController();
  const cause = new Error("synthetic caller cancellation");
  owner.abort(cause);
  await assert.rejects(client.walletCircleTransferOut(intent(), { signal: owner.signal }), error => error === cause);
  assert.equal(effects, 0);
});
