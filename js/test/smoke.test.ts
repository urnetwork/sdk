import {test} from "node:test";
import assert from "node:assert/strict";
import {existsSync} from "node:fs";
import {initWasm, isWasmInitialized, getWasmGlobals} from "../src/loader.ts";

test("JS SDK smoke loads the WASM runtime and closes it", {
  skip: !existsSync(new URL("../wasm/sdk.wasm", import.meta.url)),
  timeout: 60000,
}, async () => {
  await initWasm();
  assert.equal(isWasmInitialized(), true);
  const exports = getWasmGlobals();
  assert.equal(typeof exports.URnetworkNewPlatformDeviceRemote, "function");
  assert.equal(typeof exports.URnetworkClose, "function");
  exports.URnetworkClose();
  assert.equal(isWasmInitialized(), false);
});
