// sidecar.wasm, the page Worker's main (sidecar §18.10, D-SDK-19), boots in Node and publishes the
// version the build gave sidecar.Version. The test builds its own binary with a known version, the
// way make build_sidecar_wasm does, so it never reads a stale wasm/ and the version it checks is the
// one it set. GO names the go command, go by default.
//
// Go runs here as the page Worker runs it, with the one environment variable its loader gives
// sidecar.wasm, GODEBUG=gcstoptheworld=1 (mmm's sidecar.ur.io/v1/worker.js, QD-909). A planted
// program, test/testdata/gcloop, shows what that does: a 4 MiB heap of pointers, then 256 MiB
// allocated in 64 KiB blocks by a loop that never blocks, which stays under a bound on linear memory
// with the setting and goes past it without, and stays under it in sidecarwasm_harness.mjs, which
// runs sdk's js/wasm Go tests.
import {test} from "node:test";
import assert from "node:assert/strict";
import {execFileSync, spawnSync} from "node:child_process";
import {mkdtempSync, readFileSync, rmSync} from "node:fs";
import {createRequire} from "node:module";
import {tmpdir} from "node:os";
import {join} from "node:path";
import {fileURLToPath} from "node:url";

const jsDirectory = fileURLToPath(new URL("..", import.meta.url));
const goCommand = process.env.GO ?? "go";

// What the page Worker's loader sets as go.env before it runs sidecar.wasm.
const workerGoEnv = {GODEBUG: "gcstoptheworld=1"};

// The bound gcloop's linear memory is held to: under the Worker's environment it needs about 12 MB,
// its heap of pointers and a little garbage, and with none over 200 MB, nearly all the garbage it made.
const gcLoopMaxMemoryByteCount = 64 * 1024 * 1024;

test("sidecar.wasm boots in Node and publishes sidecar.Version", {timeout: 600000}, async () => {
  const directory = mkdtempSync(join(tmpdir(), "sidecarwasm-boot-"));
  try {
    const version = "2026.11.8-boot";
    const wasmPath = join(directory, "sidecar.wasm");
    execFileSync(goCommand, [
      "build", "-tags", "sidecar_http", "-trimpath",
      "-ldflags", `-X github.com/urnetwork/sidecar.Version=${version}`,
      "-o", wasmPath, "./cmd/sidecarwasm",
    ], {cwd: jsDirectory, env: {...process.env, GOOS: "js", GOARCH: "wasm", CGO_ENABLED: "0"}, stdio: ["ignore", "inherit", "inherit"]});

    const goroot = execFileSync(goCommand, ["env", "GOROOT"], {cwd: jsDirectory}).toString().trim();
    createRequire(import.meta.url)(join(goroot, "lib", "wasm", "wasm_exec.js"));
    const runtime = new (globalThis as any).Go();
    runtime.env = {...workerGoEnv};
    const {instance} = await WebAssembly.instantiate(readFileSync(wasmPath), runtime.importObject);
    // run returns once main first blocks, which is after it set its global; main never returns
    runtime.run(instance);
    const published = (globalThis as any).urnetworkSidecar;
    assert.equal(typeof published, "object");
    assert.equal(published.version, version);
    assert.equal(runtime.exited, false);
  } finally {
    rmSync(directory, {recursive: true, force: true});
  }
});

test("under the page Worker's environment each GC cycle ends before a planted allocation loop goes on", {timeout: 600000}, async () => {
  const directory = mkdtempSync(join(tmpdir(), "sidecarwasm-gcloop-"));
  try {
    const wasmPath = join(directory, "gcloop.wasm");
    execFileSync(goCommand, ["build", "-trimpath", "-o", wasmPath, "./test/testdata/gcloop"], {
      cwd: jsDirectory,
      env: {...process.env, GOOS: "js", GOARCH: "wasm", CGO_ENABLED: "0"},
      stdio: ["ignore", "inherit", "inherit"],
    });
    const goroot = execFileSync(goCommand, ["env", "GOROOT"], {cwd: jsDirectory}).toString().trim();
    const wasmExecPath = join(goroot, "lib", "wasm", "wasm_exec.js");
    createRequire(import.meta.url)(wasmExecPath);
    const wasm = readFileSync(wasmPath);

    // Runs gcloop to its end with env as its environment, answering what its linear memory grew to,
    // which a program never gives back.
    const memoryByteCount = async (env: Record<string, string>): Promise<number> => {
      const runtime = new (globalThis as any).Go();
      runtime.env = env;
      let exitCode: number | null = null;
      runtime.exit = (code: number) => {
        exitCode = code;
      };
      const {instance} = await WebAssembly.instantiate(wasm, runtime.importObject);
      await runtime.run(instance);
      assert.equal(exitCode, 0, `gcloop under ${JSON.stringify(env)} exited ${exitCode}`);
      return (instance.exports.mem as WebAssembly.Memory).buffer.byteLength;
    };

    const workerByteCount = await memoryByteCount({...workerGoEnv});
    assert.ok(workerByteCount < gcLoopMaxMemoryByteCount, `under the Worker's environment gcloop grew wasm memory to ${workerByteCount} bytes: a cycle let it run on with its garbage`);
    // with no environment the loop runs ahead of every cycle, and each block a cycle saw allocated is marked live
    const emptyByteCount = await memoryByteCount({});
    assert.ok(gcLoopMaxMemoryByteCount < emptyByteCount, `with no environment gcloop grew wasm memory to only ${emptyByteCount} bytes, so this no longer shows what the setting does`);

    // the harness sdk's js/wasm Go tests run in gives them the Worker's environment too
    const run = spawnSync(process.execPath, [join(jsDirectory, "test", "sidecarwasm_harness.mjs"), wasmPath, wasmExecPath], {
      cwd: jsDirectory,
      encoding: "utf8",
      timeout: 600000,
    });
    const output = `${run.stdout}${run.stderr}`;
    assert.equal(run.status, 0, output);
    const reported = /sidecarwasm harness: wasm memory ([0-9]+) bytes/.exec(output);
    assert.notEqual(reported, null, `no memory report in\n${output}`);
    const harnessByteCount = Number(reported![1]);
    assert.ok(harnessByteCount < gcLoopMaxMemoryByteCount, `under sidecarwasm_harness.mjs gcloop grew wasm memory to ${harnessByteCount} bytes: the harness no longer runs Go as the Worker does`);
  } finally {
    rmSync(directory, {recursive: true, force: true});
  }
});
