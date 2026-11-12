// The page Worker's store over IndexedDB (sidecar §18.10): cmd/sidecarwasm's Go tests of its backend and host, run as
// wasm in Node on the fake IndexedDB. A keychain opens and reopens, a write killed mid-transaction leaves the old
// state, a quota error is Failed and never Absent, one network's lost database starts that network fresh and leaves
// the other's while an eviction clears every network's, two networks' instances each keep their own record (owner
// decision 217, one database per network), the instance takes the leg's Argon2id cost, and a Go call holding a
// promise never blocks the event loop.
import {test} from "node:test";
import assert from "node:assert/strict";
import {runSidecarWasmGoTests} from "./sidecarwasm_go_tests.ts";

const names = [
  "TestIndexedDbBackendWritesReadsListsAndRemoves",
  "TestIndexedDbAbsentIsNeverAFault",
  "TestIndexedDbKilledWriteLeavesTheOldRecord",
  "TestIndexedDbQuotaIsFailed",
  "TestIndexedDbAppendSyncsWhole",
  "TestKeychainOpensAndReopens",
  "TestEvictedStoreStartsFresh",
  "TestNetworkDatabasesKeepTheirOwnInstances",
  "TestPageWorkerSettingsCarryTheWasmPassCost",
  "TestAwaitPromiseLeavesTheEventLoopRunning",
];

test("sidecar.wasm's IndexedDB store passes its Go tests in Node", {timeout: 600000}, () => {
  const {status, output, passed} = runSidecarWasmGoTests(names);
  for (const [name, ok] of passed) {
    assert.ok(ok, `${name} did not pass:\n${output}`);
  }
  assert.equal(status, 0, output);
});
