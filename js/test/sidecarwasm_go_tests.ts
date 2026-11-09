// Builds cmd/sidecarwasm's js/wasm Go tests and runs some of them in Node under sidecarwasm_harness.mjs, which
// installs the fake IndexedDB, returning their output and whether each named test passed. GO names the go command.
import {execFileSync, spawnSync} from "node:child_process";
import {mkdtempSync, rmSync} from "node:fs";
import {tmpdir} from "node:os";
import {join} from "node:path";
import {fileURLToPath} from "node:url";

const jsDirectory = fileURLToPath(new URL("..", import.meta.url));
const goCommand = process.env.GO ?? "go";

export function runSidecarWasmGoTests(names: string[]): {status: number | null, output: string, passed: Map<string, boolean>} {
  const directory = mkdtempSync(join(tmpdir(), "sidecarwasm-go-tests-"));
  try {
    const wasmPath = join(directory, "sidecarwasm.test.wasm");
    execFileSync(goCommand, ["test", "-c", "-tags", "sidecar_http", "-o", wasmPath, "./cmd/sidecarwasm"], {
      cwd: jsDirectory,
      env: {...process.env, GOOS: "js", GOARCH: "wasm", CGO_ENABLED: "0"},
      stdio: ["ignore", "inherit", "inherit"],
    });
    const goroot = execFileSync(goCommand, ["env", "GOROOT"], {cwd: jsDirectory}).toString().trim();
    const run = spawnSync(process.execPath, [
      join(jsDirectory, "test", "sidecarwasm_harness.mjs"), wasmPath, join(goroot, "lib", "wasm", "wasm_exec.js"),
      "-test.v", "-test.count=1", "-test.run", `^(${names.join("|")})$`,
    ], {cwd: jsDirectory, encoding: "utf8", timeout: 600000});
    const output = `${run.stdout}${run.stderr}`;
    const passed = new Map(names.map((name) => [name, output.includes(`--- PASS: ${name} `)]));
    return {status: run.status, output, passed};
  } finally {
    rmSync(directory, {recursive: true, force: true});
  }
}
