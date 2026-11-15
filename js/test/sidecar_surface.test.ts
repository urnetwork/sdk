// sdk.Sidecar on js (sidecar §18.5's js row, §18.4's alarms-only extension, D-SDK-30; SDK-M8-06): the declarations in
// src/types.ts name every method sidecar.go binds, and sdk.wasm, run in Node as the extension's host over a fake port
// to `ur native-host`, answers REVOKE_NOT_CAPABLE to a device revocation and APPROVAL_NOT_ELIGIBLE to a decision, lists
// nothing to decide and no device, and posts none of those calls on the port. The test builds its own sdk.wasm, so it
// never reads a stale wasm/. GO names the go command, go by default.
import { test } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const jsDirectory = fileURLToPath(new URL("..", import.meta.url));
const goCommand = process.env.GO ?? "go";
const source = (relative: string): string => readFileSync(new URL(relative, import.meta.url), "utf8");

const sidecarMethods = ["getState", "getStateReason", "getSidecarId", "addEventListener", "unlock", "setPushToken", "getApprovals", "close"];
const approvalsMethods = ["getPending", "addPendingListener", "decide", "getDevices", "previewRevokeDevice", "revokeDevice"];

test("the sidecar declarations match the wasm runtime's keys", () => {
  const declarations = source("../src/types.ts");
  const runtime = source("../sidecar.go");
  for (const [name, methods] of [["Sidecar", sidecarMethods], ["SidecarApprovals", approvalsMethods]] as const) {
    const body = declarations.match(new RegExp(`export interface ${name} \\{[\\s\\S]*?\\n}`))?.[0] || "";
    for (const method of methods) {
      assert.match(body, new RegExp(`\\b${method}\\s*\\(`), `${name}.${method} is not declared`);
      assert.match(runtime, new RegExp(`m\\["${method}"\\]`), `${name}.${method} is not bound`);
    }
  }
  const main = source("../main.go");
  const loader = source("../src/loader.ts");
  for (const global of ["URnetworkNewSidecarRemote", "URnetworkNewSidecarLocal"]) {
    assert.match(main, new RegExp(`Set\\("${global}"`), `${global} is not registered`);
    assert.match(loader, new RegExp(`${global}: runtimeGlobal\\.${global}`), `${global} is not loaded`);
  }
  const generated = source("../src/generated/types.ts");
  for (const type of ["SidecarSettings", "SidecarResult", "SidecarEvent", "SidecarDevice", "SidecarRevokePlan", "SidecarRevokeItem"]) {
    assert.match(generated, new RegExp(`export interface ${type} \\{`), `${type} is not generated`);
  }
});

test("sdk.wasm as the alarms-only extension decides and revokes nothing", { timeout: 900000 }, () => {
  const directory = mkdtempSync(join(tmpdir(), "sidecar-surface-"));
  try {
    const wasmPath = join(directory, "sdk.wasm");
    execFileSync(goCommand, ["build", "-trimpath", "-o", wasmPath, "."], {
      cwd: jsDirectory, env: { ...process.env, GOOS: "js", GOARCH: "wasm", CGO_ENABLED: "0" }, stdio: ["ignore", "inherit", "inherit"],
    });
    const goroot = execFileSync(goCommand, ["env", "GOROOT"], { cwd: jsDirectory }).toString().trim();
    // a process of its own, which exits once it printed its answers (sidecar_surface_host.mjs)
    const output = execFileSync(process.execPath, [
      fileURLToPath(new URL("./sidecar_surface_host.mjs", import.meta.url)), wasmPath, join(goroot, "lib", "wasm", "wasm_exec.js"),
    ], { cwd: jsDirectory, timeout: 120000 }).toString();
    const report = JSON.parse(output.trim().split("\n").pop() || "{}");
    assert.equal(typeof report.local?.error, "string", "a local sidecar opened on js");
    assert.equal(report.sidecarError, null);
    assert.equal(report.plan.code, "REVOKE_NOT_CAPABLE");
    assert.equal(report.plan.plan_id ?? null, null);
    assert.equal(report.revoked.ok, false);
    assert.equal(report.revoked.code, "REVOKE_NOT_CAPABLE");
    assert.equal(report.decided.ok, false);
    assert.equal(report.decided.code, "APPROVAL_NOT_ELIGIBLE");
    assert.deepEqual(report.pending, []);
    assert.equal(report.devices, null);
    // none of those calls reached the port: what crossed is the status and events reads alone
    for (const method of report.posted) {
      assert.ok(["", "status", "events"].includes(method), `the port carried ${method}`);
    }
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});
