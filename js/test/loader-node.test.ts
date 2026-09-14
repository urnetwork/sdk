import {test} from "node:test";
import assert from "node:assert/strict";
import {existsSync} from "node:fs";
import {initWasm, isWasmInitialized, getWasmGlobals} from "../src/loader.ts";

test("Node loads the real packaged WASM, waits for exports, shuts down and reopens", {
  skip: !existsSync(new URL("../wasm/sdk.wasm", import.meta.url)), timeout: 60000,
}, async () => {
  assert.equal(typeof (globalThis as any).window, "undefined");
  (globalThis as any).URnetworkUserValue = 7;
  for (let i = 0; i < 2; i++) {
    await initWasm();
    assert.equal(isWasmInitialized(), true);
    const exports = getWasmGlobals();
    assert.equal(typeof exports.URnetworkNewPlatformDeviceRemote, "function");
    exports.URnetworkClose();
    await new Promise(resolve => setTimeout(resolve, 30));
    assert.equal(isWasmInitialized(), false);
    assert.equal((globalThis as any).URnetworkUserValue, 7);
  }
  delete (globalThis as any).URnetworkUserValue;
});
