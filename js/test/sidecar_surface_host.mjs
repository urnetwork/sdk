// Runs sdk.wasm in Node as the alarms-only extension's host over a fake port to `ur native-host`, and prints what its
// sidecar answered as one JSON line: node test/sidecar_surface_host.mjs <sdk.wasm> <wasm_exec.js>. The port keeps every
// message posted and answers none. Exits once the answers are printed, since the program's own timers, a status call
// waiting on a port that never answers among them, would hold Node's loop open after it.
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";

const [wasmPath, wasmExecPath] = process.argv.slice(2);
createRequire(import.meta.url)(wasmExecPath);
const runtime = new globalThis.Go();
const { instance } = await WebAssembly.instantiate(readFileSync(wasmPath), runtime.importObject);
// run returns once main first blocks, after it set its globals
runtime.run(instance);

const posted = [];
const port = {
  postMessage(message) {
    posted.push(message);
  },
  disconnect() {},
  onMessage: { addListener() {} },
  onDisconnect: { addListener() {} },
};
const answered = (call) => new Promise((resolve, reject) => {
  const timer = setTimeout(() => reject(new Error("no answer")), 30000);
  call((value) => {
    clearTimeout(timer);
    resolve(value);
  });
});
const report = {};
report.local = globalThis.URnetworkNewSidecarLocal(null, null, null, null);
const sidecar = globalThis.URnetworkNewSidecarRemote(port, null, { instance: "default" });
report.sidecarError = sidecar?.error ?? null;
const approvals = sidecar.getApprovals();
report.plan = await answered((callback) => approvals.previewRevokeDevice("018f2d5e-7b1a-7c3d-9e4f-0a1b2c3d4e60", "lost", 0, 0, callback));
report.revoked = await answered((callback) => approvals.revokeDevice("018f2d5e-7b1a-7c3d-9e4f-0a1b2c3d4e61", [], callback));
report.decided = await answered((callback) => approvals.decide("018f2d5e-7b1a-7c3d-9e4f-0a1b2c3d4e62", "approve", callback));
report.pending = approvals.getPending();
report.devices = approvals.getDevices();
report.posted = posted.map((message) => {
  try {
    const value = typeof message === "string" ? JSON.parse(message) : message;
    return typeof value?.method === "string" ? value.method : "";
  } catch {
    return "";
  }
});
sidecar.close();
const flushed = (stream) => new Promise((resolve) => stream.write("", resolve));
process.stdout.write(JSON.stringify(report) + "\n");
await flushed(process.stdout);
process.exit(0);
