// Search in the page Worker on its own leg (sidecar §18.8's storage and messages rows; SDK-M10-04): cmd/sidecarwasm's Go
// tests of the search cache's own IndexedDB store and of the Worker's search operations, run as wasm in Node on the fake
// IndexedDB. The index cache's files land in the search store and every other file in files, a listing reaching both
// merges them, a version-1 database gains the store at its next open, the page Worker and the quick burner run their
// core's search as §18.4's browsers row says, and the operations answer through JSON on this leg.
import {test} from "node:test";
import assert from "node:assert/strict";
import {runSidecarWasmGoTests} from "./sidecarwasm_go_tests.ts";

const names = [
  "TestSearchCacheHasItsOwnStore",
  "TestSearchStoreJoinsAVersionOneDatabase",
  "TestPageWorkerSearchOnThisLeg",
];

test("sidecar.wasm's search passes its Go tests in Node", {timeout: 600000}, () => {
  const {status, output, passed} = runSidecarWasmGoTests(names);
  for (const [name, ok] of passed) {
    assert.ok(ok, `${name} did not pass:\n${output}`);
  }
  assert.equal(status, 0, output);
});
