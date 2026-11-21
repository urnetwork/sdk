// The page Worker's operations on its own leg (sidecar §18.8, §18.10; SDK-M4-04): cmd/sidecarwasm's Go tests of its
// shell, run as wasm in Node on the fake IndexedDB. The published calls answer through promises with JSON whose ids
// are strings. A newly opened Worker can bind a marked browser client and waits for signed admission.
// An operation over 50 ms hands the thread to the event loop between its steps, so a poll's reply
// interleaves with it; and each network's Worker opens that network's own database, urnetwork-sidecar-v1-<network_id>,
// named from the id's canonical text, sharing nothing with another network's and finding its own state on reopen. A
// vault record opening against a consented fixture signer and a refresh with DPoP are outside this suite.
// The team console's directory_view, config_view
// and drop_states (SDK-M6-02, SDK-M10-05) answer before open and unenrolled as every read does, and their views build
// on this leg.
import {test} from "node:test";
import assert from "node:assert/strict";
import {runSidecarWasmGoTests} from "./sidecarwasm_go_tests.ts";

const names = [
  "TestPageWorkerBeforeOpen",
  "TestPageWorkerOpensUnenrolled",
  "TestBrowserBindsMarkedClientAndWaitsForAdmission",
  "TestPageWorkerOpensItsNetworksOwnDatabase",
  "TestTwoNetworksWorkersShareNothing",
  "TestReopenedWorkerFindsItsOwnNetwork",
  "TestPageServiceUrlsWholeOrTable",
  "TestPublishedCallsAnswerThroughPromises",
  "TestOperationYieldsToTheEventLoop",
  "TestConsoleOperationsBeforeOpenAndUnenrolled",
  "TestConsoleViewsAnswerOnThisLeg",
];

test("sidecar.wasm's page Worker passes its Go tests in Node", {timeout: 600000}, () => {
  const {status, output, passed} = runSidecarWasmGoTests(names);
  for (const [name, ok] of passed) {
    assert.ok(ok, `${name} did not pass:\n${output}`);
  }
  assert.equal(status, 0, output);
});
