// Runs sdk.wasm in Node as the extension's host and drives sdk.Sidecar's js adapter as an app would, printing what it
// saw as one JSON line: node test/sidecar_adapter_host.mjs <sdk.wasm> <wasm_exec.js>. Each call that answers through a
// callback is counted, a callback that throws is given to every such call, and the program is asked a question after
// each throw, which a program that panicked could not answer. The port keeps what is posted and answers nothing.
// Exits once the report is printed, since the program's own timers would hold Node's loop open after it.
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";

const [wasmPath, wasmExecPath] = process.argv.slice(2);
createRequire(import.meta.url)(wasmExecPath);
const runtime = new globalThis.Go();
const { instance } = await WebAssembly.instantiate(readFileSync(wasmPath), runtime.importObject);
// run returns once main first blocks, after it set its globals
runtime.run(instance);

const port = {
  postMessage() {},
  disconnect() {},
  onMessage: { addListener() {} },
  onDisconnect: { addListener() {} },
};
const turn = () => new Promise((resolve) => setImmediate(resolve));
// a callback counting its calls and resolving at the first
const counted = () => {
  let resolveFirst;
  const first = new Promise((resolve) => {
    resolveFirst = resolve;
  });
  const callback = (value) => {
    callback.calls.push(value);
    resolveFirst(value);
  };
  callback.calls = [];
  callback.first = first;
  return callback;
};
// a callback that throws, counting its calls and resolving at the first
const throwing = () => {
  const callback = counted();
  const throwingCallback = (value) => {
    callback(value);
    throw new Error("an app's callback bug");
  };
  throwingCallback.calls = callback.calls;
  throwingCallback.first = callback.first;
  return throwingCallback;
};
const within = (promise) => Promise.race([
  promise,
  new Promise((_, reject) => setTimeout(() => reject(new Error("no answer")), 30000)),
]);
const report = {};

// the key store: a local sidecar answers {error} on js, and no method of a store handed to it is called
const storeCalls = [];
const keyStore = {
  putSecret() {
    storeCalls.push("put");
    throw new Error("a store bug");
  },
  getSecret() {
    storeCalls.push("get");
    throw new Error("a store bug");
  },
  deleteSecret() {
    storeCalls.push("delete");
    throw new Error("a store bug");
  },
};
report.local = globalThis.URnetworkNewSidecarLocal(null, keyStore, null, { instance: "default" });
report.storeCalls = storeCalls;

const sidecar = globalThis.URnetworkNewSidecarRemote(port, null, { instance: "default" });
report.sidecarError = sidecar?.error ?? null;
const approvals = sidecar.getApprovals();
const id = "018f2d5e-7b1a-7c3d-9e4f-0a1b2c3d4e60";

// one answer each, through the callback the call took
const plan = counted();
const revoked = counted();
const decided = counted();
const unlocked = counted();
approvals.previewRevokeDevice(id, "lost", 0, 0, plan);
approvals.revokeDevice(id, [], revoked);
approvals.decide(id, "approve", decided);
sidecar.unlock("", unlocked);
await within(Promise.all([plan.first, revoked.first, decided.first, unlocked.first]));

// a callback that throws ends nothing: the program answers after each
const throwers = { plan: throwing(), revoked: throwing(), decided: throwing(), unlocked: throwing() };
report.statesAfterThrows = [];
approvals.previewRevokeDevice(id, "lost", 0, 0, throwers.plan);
report.statesAfterThrows.push(sidecar.getState());
approvals.revokeDevice(id, [], throwers.revoked);
report.statesAfterThrows.push(sidecar.getState());
approvals.decide(id, "approve", throwers.decided);
report.statesAfterThrows.push(sidecar.getState());
sidecar.unlock("", throwers.unlocked);
await within(throwers.unlocked.first);
await turn();
report.statesAfterThrows.push(sidecar.getState());
report.pendingAfterThrows = approvals.getPending();

sidecar.close();
await turn();
await turn();
const summary = (callback) => ({ count: callback.calls.length, first: callback.calls[0] ?? null });
report.answers = { plan: summary(plan), revoked: summary(revoked), decided: summary(decided), unlocked: summary(unlocked) };
report.thrown = Object.fromEntries(Object.entries(throwers).map(([name, callback]) => [name, callback.calls.length]));
const flushed = (stream) => new Promise((resolve) => stream.write("", resolve));
process.stdout.write(JSON.stringify(report) + "\n");
await flushed(process.stdout);
process.exit(0);
