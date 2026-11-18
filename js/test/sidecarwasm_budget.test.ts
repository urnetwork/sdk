// T4's page Worker cell and the index vectors on the page Worker's leg, in wasm under Node (SDK-M10-06; sidecar §22.4's
// T4, §18.4's targets row, §22.9's Index determinism row, D-SDK-25, D-TEST-15): cmd/sidecarwasm's Go tests of a page
// Worker's search held to §18.4's 8 MiB working set beside the version-table pages by FS-109's measure, with a
// deliberate per-search key collection failing the cell, and of testdata/index's vectors reproduced byte for byte in
// the wasm the page Worker's module graph builds. The vectors run in a process of their own, since together with the
// other tests they pass wasm's 4 GiB on this leg (SEARCH-M10-41's finding). Search targets in Safari's wasm engine,
// and a page search over a real index in wasm, are external to this file (budget_search_js_test.go says why).
import {test} from "node:test";
import assert from "node:assert/strict";
import {runSidecarWasmGoTests} from "./sidecarwasm_go_tests.ts";

test("a page Worker's search holds its working set in wasm", {timeout: 600000}, () => {
  const {status, output, passed} = runSidecarWasmGoTests([
    "TestPageWorkerSearchWithinItsWorkingSet",
    "TestPageWorkerSearchBudgetCatchesAPerSearchKeyCollection",
  ]);
  for (const [name, ok] of passed) {
    assert.ok(ok, `${name} did not pass:\n${output}`);
  }
  assert.equal(status, 0, output);
});

test("the index vectors reproduce on the page Worker's leg", {timeout: 600000}, () => {
  const {status, output, passed} = runSidecarWasmGoTests(["TestPageWorkerReproducesTheIndexVectors"], ["sidecar_testhooks"]);
  for (const [name, ok] of passed) {
    assert.ok(ok, `${name} did not pass:\n${output}`);
  }
  assert.equal(status, 0, output);
});
