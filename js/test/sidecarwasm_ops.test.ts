// The page Worker's operations on its own leg (sidecar §18.8, §18.10; SDK-M4-04): cmd/sidecarwasm's Go tests of its
// shell, run as wasm in Node on the fake IndexedDB. The published calls answer through promises with JSON whose ids
// are strings; a Worker before open, and an unenrolled one, answer each operation's refusal; enroll answers
// unavailable; an operation over 50 ms hands the thread to the event loop between its steps, so a poll's reply
// interleaves with it; and each network's Worker opens that network's own database, urnetwork-sidecar-v1-<network_id>,
// named from the id's canonical text, sharing nothing with another network's and finding its own state on reopen. A
// vault record opening against a fake stack, a refresh with DPoP and a real poll interleaved with an operation wait
// for enrollment (plan-SDK-key-origin-credential) and are not run here.
import {test} from "node:test";
import assert from "node:assert/strict";
import {runSidecarWasmGoTests} from "./sidecarwasm_go_tests.ts";

const names = [
  "TestPageWorkerBeforeOpen",
  "TestPageWorkerOpensUnenrolled",
  "TestPageWorkerOpensItsNetworksOwnDatabase",
  "TestTwoNetworksWorkersShareNothing",
  "TestReopenedWorkerFindsItsOwnNetwork",
  "TestPageServiceUrlsWholeOrTable",
  "TestPublishedCallsAnswerThroughPromises",
  "TestOperationYieldsToTheEventLoop",
];

test("sidecar.wasm's page Worker passes its Go tests in Node", {timeout: 600000}, () => {
  const {status, output, passed} = runSidecarWasmGoTests(names);
  for (const [name, ok] of passed) {
    assert.ok(ok, `${name} did not pass:\n${output}`);
  }
  assert.equal(status, 0, output);
});
