// sidecar.wasm, the page Worker's main (sidecar §18.10, D-SDK-19), boots in Node and publishes the
// version the build gave sidecar.Version. The test builds its own binary with a known version, the
// way make build_sidecar_wasm does, so it never reads a stale wasm/ and the version it checks is the
// one it set. GO names the go command, go by default.
import {test} from "node:test";
import assert from "node:assert/strict";
import {execFileSync} from "node:child_process";
import {mkdtempSync, readFileSync, rmSync} from "node:fs";
import {createRequire} from "node:module";
import {tmpdir} from "node:os";
import {join} from "node:path";
import {fileURLToPath} from "node:url";

const jsDirectory = fileURLToPath(new URL("..", import.meta.url));
const goCommand = process.env.GO ?? "go";

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
