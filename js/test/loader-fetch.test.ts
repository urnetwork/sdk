// No live requests: prove the browser loader preserves fetch failures and
// reuses one response when MIME/streaming support requires byte compilation.
import assert from "node:assert/strict";
import test from "node:test";
import { instantiateWasm } from "../src/loader.ts";

test("aborted WASM fetch is not replayed during document teardown", async () => {
  const originalFetch = globalThis.fetch;
  let calls = 0;
  const failure = new TypeError("synthetic document navigation aborted fetch");
  globalThis.fetch = async () => { calls++; throw failure; };
  try {
    await assert.rejects(instantiateWasm("https://application.example/sdk.wasm", { importObject: {} }), error => error === failure);
    assert.equal(calls, 1, "loader issued a second fetch after the document stopped loading");
  } finally { globalThis.fetch = originalFetch; }
});

test("streaming MIME fallback instantiates the original response without refetching", async () => {
  const originalFetch = globalThis.fetch;
  const originalWarn = console.warn;
  let calls = 0;
  const wasm = new Uint8Array([0, 97, 115, 109, 1, 0, 0, 0]);
  globalThis.fetch = async () => { calls++; return new Response(wasm, { headers: { "content-type": "application/octet-stream" } }); };
  console.warn = () => {};
  try {
    assert.ok(await instantiateWasm("https://application.example/sdk.wasm", { importObject: {} }) instanceof WebAssembly.Instance);
    assert.equal(calls, 1);
  } finally { globalThis.fetch = originalFetch; console.warn = originalWarn; }
});

test("HTTP failure is reported instead of compiling an error response", async () => {
  const originalFetch = globalThis.fetch;
  let calls = 0;
  globalThis.fetch = async () => { calls++; return new Response("synthetic failure", { status: 503 }); };
  try {
    await assert.rejects(instantiateWasm("https://application.example/sdk.wasm", { importObject: {} }), /HTTP 503/);
    assert.equal(calls, 1);
  } finally { globalThis.fetch = originalFetch; }
});
