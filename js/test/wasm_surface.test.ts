import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

const source = (relative: string): string =>
  readFileSync(new URL(relative, import.meta.url), "utf8");

// The WASM bindings are authored in Go while the public declarations are
// authored in TypeScript. Keep a small explicit baseline for the
// contract-details surface so a rename cannot compile on one side and become
// undefined at runtime on the other.
test("contract-details declarations match WASM runtime keys", () => {
  const declarations = source("../src/types.ts");
  const deviceRuntime = source("../device_remote.go");
  const controllerRuntime = source("../view_controllers.go");

  for (const method of [
    "openContractDetailsViewController",
    "openClientContractDetailsViewController",
    "openProviderContractDetailsViewController",
  ]) {
    assert.match(declarations, new RegExp(`\\b${method}\\s*\\(`));
    assert.match(deviceRuntime, new RegExp(`m\\[\"${method}\"\\]`));
  }

  for (const method of [
    "getContractRows",
    "setAtTop",
    "pendingCount",
    "getClientContractRows",
    "getProviderContractRows",
    "addContractRowsListener",
  ]) {
    assert.match(declarations, new RegExp(`\\b${method}\\s*\\(`));
    assert.match(controllerRuntime, new RegExp(`m\\[\"${method}\"\\]`));
  }

  for (const field of [
    "sendContracts",
    "receiveContracts",
    "sendByteCount",
    "receiveByteCount",
    "lastActivityMillis",
    "closing",
  ]) {
    assert.match(declarations, new RegExp(`\\b${field}\\s*:`));
    assert.match(controllerRuntime, new RegExp(`\"${field}\"\\s*:`));
  }
});

// Same guard for the block-action surface, whose runtime lives in
// view_controllers2.go.
test("block-action declarations match WASM runtime keys", () => {
  const declarations = source("../src/types.ts");
  const controllerRuntime = source("../view_controllers2.go");

  for (const method of [
    "getBlockStats",
    "getBlockActions",
    "getWindowDurationSeconds",
    "setWindowDurationSeconds",
    "getMaxBlockActions",
    "setMaxBlockActions",
    "addBlockActionsListener",
    "addBlockActionStatsListener",
  ]) {
    assert.match(declarations, new RegExp(`\\b${method}\\s*\\(`));
    assert.match(controllerRuntime, new RegExp(`m\\[\"${method}\"\\]`));
  }

  // BlockAction feed row fields (jsBlockAction) and the BlockStats counters
  // (jsBlockStats)
  for (const field of [
    "time",
    "block",
    "local",
    "ips",
    "hosts",
    "matchedIps",
    "matchedHosts",
    "allowedCount",
    "blockedCount",
  ]) {
    assert.match(declarations, new RegExp(`\\b${field}\\s*:`));
    assert.match(controllerRuntime, new RegExp(`\"${field}\"\\s*:`));
  }
});
