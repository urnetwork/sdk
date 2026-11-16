// sdk.Sidecar's js adapter (SDK-M8-15; sidecar §22.5's §18 row, §18.5's js row, D-SDK-15, FS-98's js row): sdk.wasm,
// run in Node as the extension's host (sidecar_adapter_host.mjs), answers every call that takes a callback exactly once,
// a revocation REVOKE_NOT_CAPABLE, a decision APPROVAL_NOT_ELIGIBLE and an unlock refused, its daemon's to unlock; a
// callback that throws ends nothing, the program answering after each; and a local sidecar, with any key store handed
// to it, answers an error with no call on the store, since sdk.wasm links no sidecar root (plan-SDK-small-readings #7).
// The test builds its own sdk.wasm, so it never reads a stale wasm/. GO names the go command, go by default.
import { test } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const jsDirectory = fileURLToPath(new URL("..", import.meta.url));
const goCommand = process.env.GO ?? "go";

test("the sidecar adapter answers once a call and survives a throwing callback", { timeout: 900000 }, () => {
  const directory = mkdtempSync(join(tmpdir(), "sidecar-adapter-"));
  try {
    const wasmPath = join(directory, "sdk.wasm");
    execFileSync(goCommand, ["build", "-trimpath", "-o", wasmPath, "."], {
      cwd: jsDirectory, env: { ...process.env, GOOS: "js", GOARCH: "wasm", CGO_ENABLED: "0" }, stdio: ["ignore", "inherit", "inherit"],
    });
    const goroot = execFileSync(goCommand, ["env", "GOROOT"], { cwd: jsDirectory }).toString().trim();
    const output = execFileSync(process.execPath, [
      fileURLToPath(new URL("./sidecar_adapter_host.mjs", import.meta.url)), wasmPath, join(goroot, "lib", "wasm", "wasm_exec.js"),
    ], { cwd: jsDirectory, timeout: 120000 }).toString();
    const report = JSON.parse(output.trim().split("\n").pop() || "{}");

    // the key store: no local sidecar on js, and the store is never called
    assert.equal(typeof report.local?.error, "string", "a local sidecar opened on js");
    assert.deepEqual(report.storeCalls, []);

    // one terminal result per call
    assert.equal(report.sidecarError, null);
    for (const [name, answer] of Object.entries(report.answers) as [string, { count: number; first: Record<string, unknown> | null }][]) {
      assert.equal(answer.count, 1, `${name} answered ${answer.count} times`);
    }
    assert.equal(report.answers.plan.first.code, "REVOKE_NOT_CAPABLE");
    assert.equal(report.answers.revoked.first.ok, false);
    assert.equal(report.answers.revoked.first.code, "REVOKE_NOT_CAPABLE");
    assert.equal(report.answers.decided.first.ok, false);
    assert.equal(report.answers.decided.first.code, "APPROVAL_NOT_ELIGIBLE");
    assert.equal(report.answers.unlocked.first.ok, false);

    // a throwing callback was called once and ended nothing
    for (const [name, count] of Object.entries(report.thrown)) {
      assert.equal(count, 1, `the throwing ${name} callback was called ${count} times`);
    }
    assert.equal(report.statesAfterThrows.length, 4);
    for (const state of report.statesAfterThrows) {
      assert.equal(typeof state, "string");
      assert.ok(state.length > 0, "the program answered no state after a throw");
    }
    assert.deepEqual(report.pendingAfterThrows, []);
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});
