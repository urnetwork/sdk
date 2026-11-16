// Runs a js/wasm Go test binary of cmd/sidecarwasm in Node with the fake IndexedDB installed, the way the page Worker
// runs sidecar.wasm with the browser's: node test/sidecarwasm_harness.mjs <test.wasm> <wasm_exec.js> [go test args].
// Exits with the binary's code. Node's own crypto.subtle is WebCrypto, so the key store runs on the real thing.
//
// The binary gets the environment the page Worker's loader gives sidecar.wasm, GODEBUG=gcstoptheworld=1 (mmm's
// sidecar.ur.io/v1/worker.js, QD-909), so its collector runs here as it does there: each GC cycle marks to its end
// before the program goes on, where by default a long CPU-bound test, which never blocks on one thread, leaves a cycle
// open while linear memory grows. Once the binary ends, the harness reports what linear memory grew to, which a
// program never gives back, so a run can be held to a bound (sidecarwasm_boot.test.ts holds a planted program's).
import {readFileSync} from "node:fs";
import {createRequire} from "node:module";
import {installFakeIndexedDb} from "./fake_indexeddb.mjs";

const [wasmPath, wasmExecPath, ...args] = process.argv.slice(2);
installFakeIndexedDb(globalThis);
createRequire(import.meta.url)(wasmExecPath);
const go = new globalThis.Go();
go.argv = [wasmPath, ...args];
go.env = {GODEBUG: "gcstoptheworld=1"};
let exitCode = null;
go.exit = (code) => {
  exitCode = code;
};
const {instance} = await WebAssembly.instantiate(readFileSync(wasmPath), go.importObject);
await go.run(instance);
// a binary that stopped without exiting deadlocked: every goroutine waits on something the loop will never bring
if (exitCode === null) {
  console.error("sidecarwasm harness: the program stopped without exiting");
}
console.error(`sidecarwasm harness: wasm memory ${instance.exports.mem.buffer.byteLength} bytes`);
// A binary that exited may leave the timers of goroutines it never joined, an opened Sidecar's among them, which
// would hold Node's loop open until each fires and then throw into an exited program: once both streams have
// flushed what the binary wrote, the harness exits with its code.
const flushed = (stream) => new Promise((resolve) => stream.write("", resolve));
await flushed(process.stdout);
await flushed(process.stderr);
process.exit(exitCode ?? 3);
