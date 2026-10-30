// The Protocol leaderboard in the generated client (connect api/bringyour.yml,
// GET /sn/top and GET /sn/top/me; server LEADERTOP.md §7): the board is public
// with optional auth, the caller's detail takes the bearer, and the nullable
// fields stay nullable in the types.
import { test } from "node:test";
import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import ts from "typescript";
import { registerTsResolve } from "./ts-resolve.ts";

registerTsResolve();
const { createURNetworkApiClient } = await import("../src/client.ts");

interface Recorded {
  url: string;
  method: string;
  authorization: string | null;
}

function recordingClient(body: unknown, token?: string) {
  const calls: Recorded[] = [];
  const client = createURNetworkApiClient({
    baseURL: "https://api.example",
    token,
    retry: false,
    fetch: async (input, init) => {
      calls.push({
        url: String(input),
        method: init?.method ?? "GET",
        authorization: new Headers(init?.headers).get("authorization"),
      });
      return new Response(JSON.stringify(body), { headers: { "Content-Type": "application/json" } });
    },
  });
  return { client, calls };
}

const board = {
  snapshot: {
    id: "snapshot-7", tempo_index: 19034, block: 6852240, computed_at: "2026-10-29T15:12:00Z",
    settlement_epoch: 88, total_ranked: 1, head_seats: 200, claimed_fleets: 0, open_seats: 200,
    projected_cutoff: 2.5, claimed_cutoff: null, binding_open: false,
    binding_note: "Top 200 binding isn't open on mainnet yet", measured_providers: 1450,
    cohort_capacity: 1500, validator_summary_at: null, incentive_at: null, precision_at_200: null,
    stale: false,
  },
  rows: [],
  me: null,
  next: null,
};

test("snTop signed out sends the set query parameters and no credential", async () => {
  const { client, calls } = recordingClient(board);
  assert.equal(typeof client.snTop, "function", "snTop is missing from the generated client");
  const result = await client.snTop({ limit: 2, cursor: "c/1+x", filter: "claimed", snapshot: "snapshot-7" });
  assert.deepEqual(result, board);
  assert.deepEqual(calls, [{
    url: "https://api.example/sn/top?limit=2&cursor=c%2F1%2Bx&filter=claimed&snapshot=snapshot-7",
    method: "GET",
    authorization: null,
  }]);
});

test("snTop signed in sends the bearer, and no query without parameters", async () => {
  const { client, calls } = recordingClient(board, "board-token");
  await client.snTop();
  assert.deepEqual(calls, [{ url: "https://api.example/sn/top", method: "GET", authorization: "Bearer board-token" }]);
});

test("snTopMe sends the bearer to the caller's detail", async () => {
  const detail = {
    snapshot: board.snapshot,
    me: {
      row: {
        position: 230, name: "", tag: "", projected_score: 2.2, raw_last_tempo: 2.4, margin: -0.12,
        trend: 5, prefixes: 4, shared_prefixes: 2, presence: 0.9, label: "estimate", measurable: true,
        verify_eligible: true, claim_prompt: false, is_me: true, fleets: [],
      },
      solo_score: 3.5, distinct_prefixes: 4, shared_prefixes: 2, shared_with_units: 5,
      not_measurable_reason: "", history: [{ day: "2026-10-28", rank: 230, score: 2.2 }],
    },
  };
  const { client, calls } = recordingClient(detail, "me-token");
  assert.equal(typeof client.snTopMe, "function", "snTopMe is missing from the generated client");
  assert.deepEqual(await client.snTopMe(), detail);
  assert.deepEqual(calls, [{ url: "https://api.example/sn/top/me", method: "GET", authorization: "Bearer me-token" }]);
});

test("the Protocol leaderboard types keep nulls, enums and lists", () => {
  const filename = fileURLToPath(new URL("sn_top.fixture.ts", import.meta.url));
  const source = `
    import type { URNetworkApiClient } from "../src/client";
    import type {
      Operations, SnTopFleet, SnTopHistoryPoint, SnTopMe, SnTopMeResult, SnTopResult, SnTopRow, SnTopSnapshot,
    } from "../src/generated/openapi";
    declare const client: URNetworkApiClient;
    declare const result: SnTopResult;
    declare const snapshot: SnTopSnapshot;
    declare const row: SnTopRow;
    declare const fleet: SnTopFleet;
    declare const me: SnTopMe;
    const page: Promise<SnTopResult> = client.snTop({ limit: 50, filter: "unclaimed" });
    const detail: Promise<SnTopMeResult> = client.snTopMe();
    const response: SnTopResult = {} as Operations["snTop"]["response"];
    const meRow: SnTopRow | null = result.me;
    const next: string | null = result.next;
    const rows: SnTopRow[] = result.rows;
    const claimedCutoff: number | null = snapshot.claimed_cutoff;
    const precision: number | null = snapshot.precision_at_200;
    const summaryAt: string | null = snapshot.validator_summary_at;
    const label: "estimate" | "likely" | "borderline" | "unlikely" | "not_measurable" = row.label;
    const fleets: SnTopFleet[] = row.fleets;
    const unavailableReplica: SnTopFleet["replica_score"] = null;
    // @ts-expect-error the replica is unavailable until its evidence can be verified
    const replicaMustBePresent: number = fleet.replica_score;
    const score: number | null = fleet.validator_score;
    const rank: number | null = fleet.validator_rank;
    const rankOf: number | null = fleet.validator_rank_of;
    const selected: boolean | null = fleet.selected;
    const incentive: number | null = fleet.incentive;
    const reason: "" | "not_in_cohort" | "shared_egress" | "not_providing" = me.not_measurable_reason;
    const history: SnTopHistoryPoint[] = me.history;
    // @ts-expect-error a filter outside the enum
    client.snTop({ filter: "everyone" });
    // @ts-expect-error the cutoff can be null
    const notNull: number = snapshot.claimed_cutoff;
  `;
  const options: ts.CompilerOptions = {
    strict: true, noEmit: true, skipLibCheck: true,
    target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext,
    moduleResolution: ts.ModuleResolutionKind.Bundler,
    noUnusedLocals: false,
  };
  const host = ts.createCompilerHost(options);
  const read = host.getSourceFile.bind(host);
  host.getSourceFile = (name, languageVersion, onError, shouldCreateNewSourceFile) =>
    name === filename ? ts.createSourceFile(name, source, languageVersion) :
      read(name, languageVersion, onError, shouldCreateNewSourceFile);
  const program = ts.createProgram([filename], options, host);
  const diagnostics = ts.getPreEmitDiagnostics(program);
  assert.deepEqual(diagnostics.map(d => ts.flattenDiagnosticMessageText(d.messageText, "\n")), []);
});
