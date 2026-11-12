// Partial pool reads must reach the server as requested, so an unavailable
// pool does not hide the balances the server can still answer.
import { test } from "node:test";
import assert from "node:assert/strict";
import { registerTsResolve } from "./ts-resolve.ts";

registerTsResolve();
const { createURNetworkApiClient } = await import("../src/client.ts");
const coldkey = `0x${"1".repeat(64)}`;
const unavailablePool = {
  vault: `0x${"2".repeat(40)}`, no_id: 4294967395,
  kind: "ctf", label: "Synthetic flag pool",
};

function recordingClient(answer: unknown) {
  const calls: Array<{ url: string; authorization: string | null }> = [];
  const client = createURNetworkApiClient({
    baseURL: "https://api.example", token: "synthetic-network-credential", retry: false,
    fetch: async (input, init) => {
      calls.push({ url: String(input), authorization: new Headers(init?.headers).get("authorization") });
      return Response.json(answer);
    },
  });
  return { client, calls };
}

test("ledger reads preserve the partial choice and unavailable pools", async () => {
  const answer = { coldkey, accounts: [], unavailable_pools: [unavailablePool] };
  for (const partial of [true, false, undefined]) {
    const { client, calls } = recordingClient(answer);
    assert.deepEqual(await client.snLedgerAccount({ coldkey, partial }), answer);
    const query = new URLSearchParams({ coldkey });
    if (partial !== undefined) query.set("partial", String(partial));
    assert.deepEqual(calls, [{ url: `https://api.example/sn/ledger/account?${query}`, authorization: null }]);
  }
});

test("relay reads preserve the partial choice and unavailable pools", async () => {
  const answer = {
    state: "ledger_unavailable", automatic_threshold_tao_rao: 0, transfer_floor_tao_rao: 0,
    automatic_threshold_rao: 0, transfer_floor_rao: 0, claim_now_period_days: 30,
    accounts: [], unavailable_pools: [unavailablePool],
  };
  for (const partial of [true, false, undefined]) {
    const { client, calls } = recordingClient(answer);
    assert.deepEqual(await client.snClaimRelay({ partial }), answer);
    const query = partial === undefined ? "" : `?partial=${partial}`;
    assert.deepEqual(calls, [{
      url: `https://api.example/sn/claim-relay${query}`, authorization: "Bearer synthetic-network-credential",
    }]);
  }
});
