// Runs a js/wasm Go test binary of cmd/sidecarwasm in Node with the fake IndexedDB installed, the way the page Worker
// runs sidecar.wasm with the browser's: node test/sidecarwasm_harness.mjs <test.wasm> <wasm_exec.js> [go test args].
// Exits with the binary's code. Node's own crypto.subtle is WebCrypto, so the key store runs on the real thing.
import {readFileSync} from "node:fs";
import {createRequire} from "node:module";
import {installFakeIndexedDb} from "./fake_indexeddb.mjs";

const [wasmPath, wasmExecPath, ...args] = process.argv.slice(2);
installFakeIndexedDb(globalThis);
createRequire(import.meta.url)(wasmExecPath);
const go = new globalThis.Go();
go.argv = [wasmPath, ...args];
go.env = {};
let exitCode = null;
go.exit = (code) => {
  exitCode = code;
};
const {instance} = await WebAssembly.instantiate(readFileSync(wasmPath), go.importObject);
await go.run(instance);
// a binary that stopped without exiting deadlocked: every goroutine waits on something the loop will never bring
process.exitCode = exitCode ?? 3;
if (exitCode === null) {
  console.error("sidecarwasm harness: the program stopped without exiting");
}
