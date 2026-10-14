import { test } from "node:test";
import assert from "node:assert/strict";
import { existsSync, mkdtempSync, rmSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

// lag.dev/PLAN.md S1, T3: the committed country colors and vectors are what
// Go generates now. The other lag tests replay the vectors; this ties them to
// the Go sdk and connect checked out beside sdk.
const jsDir = fileURLToPath(new URL("../..", import.meta.url));
const connectDir = fileURLToPath(new URL("../../../../connect/lag_names.go", import.meta.url));
const sourceAvailable = existsSync(connectDir) && existsSync(join(jsDir, "gen_lag.go"));
const skip = sourceAvailable ? false : "connect with lag_names.go is not checked out beside sdk";

test("gen_lag.go output is current", { skip, timeout: 600_000 }, () => {
  const root = mkdtempSync(join(tmpdir(), "urnetwork-js-gen-lag-"));
  try {
    const binary = join(root, "gen-lag");
    const env = { ...process.env, GOWORK: "off", GOMAXPROCS: "2" };
    const build = spawnSync("go", ["build", "-mod=readonly", "-o", binary, "gen_lag.go"], {
      cwd: jsDir, env, encoding: "utf8", timeout: 540_000,
    });
    assert.equal(build.status, 0, `gen_lag.go: ${build.error || ""}${build.stderr}`);
    const check = spawnSync(binary, ["-check"], { cwd: jsDir, env, encoding: "utf8", timeout: 60_000 });
    assert.equal(check.status, 0, `${check.stdout}${check.stderr}`);
    assert.match(check.stdout, /up to date/);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
