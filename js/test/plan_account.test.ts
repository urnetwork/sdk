import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { registerTsResolve } from "./ts-resolve.ts";

// planAccountName replays test/plan_account_vectors.json, the vectors
// sdk/plan_account_test.go replays against the Go twin, so the apps and ur.io
// show the same account name with the plan.

registerTsResolve();
const { planAccountName } = await import("../src/utils/plan_account.ts");
const utils = await import("../src/utils/index.ts");

const vectors = JSON.parse(readFileSync(new URL("./plan_account_vectors.json", import.meta.url), "utf8"));

test("plan account name vectors", () => {
  assert.ok(vectors.length >= 6);
  for (const vector of vectors) {
    assert.equal(planAccountName(vector.network_name, vector.guest), vector.want, vector.name);
  }
});

test("plan account name is exported from utils", () => {
  assert.equal(utils.planAccountName, planAccountName);
});
