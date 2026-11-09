// The page Worker's key store (sidecar §18.8, D-SDK-15): cmd/sidecarwasm's Go tests of the non-extractable WebCrypto
// root key, run as wasm in Node on the fake IndexedDB and Node's own WebCrypto. §5.9's status table row by row,
// corrupt bytes Failed, exportKey on the root key rejected, one root key for racing first puts, the instance's answers
// to each status, and the quick burner writing nothing.
import {test} from "node:test";
import assert from "node:assert/strict";
import {runSidecarWasmGoTests} from "./sidecarwasm_go_tests.ts";

const names = [
  "TestWebCryptoKeyStoreStatusTable",
  "TestWebCryptoRootKeyNeverLeaves",
  "TestWebCryptoKeyStoreFailures",
  "TestWebCryptoFirstPutsMakeOneRootKey",
  "TestKeychainFollowsTheKeyStoreTable",
  "TestBurnerWritesNothing",
];

test("sidecar.wasm's WebCrypto key store passes its Go tests in Node", {timeout: 600000}, () => {
  const {status, output, passed} = runSidecarWasmGoTests(names);
  for (const [name, ok] of passed) {
    assert.ok(ok, `${name} did not pass:\n${output}`);
  }
  assert.equal(status, 0, output);
});
