import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { registerTsResolve } from "./ts-resolve.ts";

// The Pro features in use notice replays test/pro_features_vectors.json, the
// vectors sdk/pro_features_status_test.go replays against the Go twin, so the
// two cannot drift apart.

registerTsResolve();
const {
  PRO_FEATURE_SOCKS_PROXY,
  PRO_FEATURE_WIREGUARD_PROXY,
  PRO_FEATURES_NOTICE_NONE,
  PRO_FEATURES_NOTICE_POST,
  PRO_FEATURES_NOTICE_REMOVE,
  PRO_FEATURES_NOTICE_UPDATE,
  PRO_FEATURES_PROMPT_NONE,
  PRO_FEATURES_PROMPT_STOPPED,
  PRO_FEATURES_PROMPT_UPGRADE,
  ProFeaturesNoticeTracker,
  proFeaturesStatusFor,
} = await import("../src/utils/pro_features.ts");
const utils = await import("../src/utils/index.ts");

const vectors = JSON.parse(readFileSync(new URL("./pro_features_vectors.json", import.meta.url), "utf8"));

// The wire strings are the server's (pro.yml's feature names) and the Go
// sdk's.
test("pro features constants", () => {
  assert.deepEqual(
    {
      PRO_FEATURE_SOCKS_PROXY,
      PRO_FEATURE_WIREGUARD_PROXY,
      PRO_FEATURES_PROMPT_NONE,
      PRO_FEATURES_PROMPT_UPGRADE,
      PRO_FEATURES_PROMPT_STOPPED,
      PRO_FEATURES_NOTICE_NONE,
      PRO_FEATURES_NOTICE_POST,
      PRO_FEATURES_NOTICE_UPDATE,
      PRO_FEATURES_NOTICE_REMOVE,
    },
    {
      PRO_FEATURE_SOCKS_PROXY: "socks_proxy",
      PRO_FEATURE_WIREGUARD_PROXY: "wireguard_proxy",
      PRO_FEATURES_PROMPT_NONE: "",
      PRO_FEATURES_PROMPT_UPGRADE: "upgrade",
      PRO_FEATURES_PROMPT_STOPPED: "stopped",
      PRO_FEATURES_NOTICE_NONE: "",
      PRO_FEATURES_NOTICE_POST: "post",
      PRO_FEATURES_NOTICE_UPDATE: "update",
      PRO_FEATURES_NOTICE_REMOVE: "remove",
    },
  );
});

test("the utils index exports the pro features notice", () => {
  assert.equal(utils.proFeaturesStatusFor, proFeaturesStatusFor);
  assert.equal(utils.ProFeaturesNoticeTracker, ProFeaturesNoticeTracker);
});

test("pro features status vectors", () => {
  assert.ok(vectors.status.length > 0);
  for (const vector of vectors.status) {
    const status = proFeaturesStatusFor(vector.result);
    assert.deepEqual(
      {
        prompt: status.prompt,
        socks_proxy_count: status.socksProxyCount,
        wireguard_proxy_count: status.wireguardProxyCount,
        plan_known: status.planKnown,
        pro: status.pro,
        pro_features_enforced: status.proFeaturesEnforced,
      },
      vector.want,
      vector.name,
    );
  }
});

test("pro features notice tracker vectors", () => {
  assert.ok(vectors.tracker.length > 0);
  for (const vector of vectors.tracker) {
    const tracker = new ProFeaturesNoticeTracker();
    vector.steps.forEach((step: { op: string; prompt?: string; action: string; shown: string }, i: number) => {
      let action: string;
      if (step.op === "update") action = tracker.update({ prompt: step.prompt as never });
      else if (step.op === "dismiss") action = tracker.dismiss();
      else if (step.op === "reset") action = tracker.reset();
      else throw new Error(`${vector.name}: step ${i}: unknown op ${step.op}`);
      assert.equal(action, step.action, `${vector.name}: step ${i} (${step.op} ${step.prompt ?? ""}) action`);
      assert.equal(tracker.shownPromptNow, step.shown, `${vector.name}: step ${i} (${step.op} ${step.prompt ?? ""}) shown`);
    });
  }
});

// A missing status is no prompt: the notice goes.
test("pro features notice tracker withdraws on a missing status", () => {
  const tracker = new ProFeaturesNoticeTracker();
  assert.equal(tracker.update(null), PRO_FEATURES_NOTICE_NONE);
  assert.equal(tracker.update({ prompt: PRO_FEATURES_PROMPT_UPGRADE }), PRO_FEATURES_NOTICE_POST);
  assert.equal(tracker.update(undefined), PRO_FEATURES_NOTICE_REMOVE);
  assert.equal(tracker.shownPromptNow, PRO_FEATURES_PROMPT_NONE);
});

// A list with a client of null, or pro_features of null, reads as holding
// nothing: the web hands over whatever the server sent.
test("pro features status tolerates null rows", () => {
  const status = proFeaturesStatusFor({
    clients: [null, { pro_features: null }, { pro_features: [PRO_FEATURE_WIREGUARD_PROXY] }],
    plan: { pro: false, pro_features_enforced: true },
  });
  assert.equal(status.prompt, PRO_FEATURES_PROMPT_STOPPED);
  assert.equal(status.socksProxyCount, 0);
  assert.equal(status.wireguardProxyCount, 1);
});
