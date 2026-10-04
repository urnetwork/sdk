import { after, before, test } from "node:test";
import assert from "node:assert/strict";
import { copyFileSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const jsDir = fileURLToPath(new URL("..", import.meta.url));
const spec = fileURLToPath(new URL("../../../connect/api/bringyour.yml", import.meta.url));
const outputs = ["types.ts", "openapi.ts", "client.ts"];
const generated = join(jsDir, "src/generated");
const sourceAvailable = existsSync(spec) && existsSync(join(jsDir, "gen_types.go"));
const skip = sourceAvailable ? false : "generation sources are not checked out beside sdk";
const env = { ...process.env, GOWORK: "off", GOMAXPROCS: "2" };
let privateRoot = "";
let typesGenerator = "";
let apiGenerator = "";

before(() => {
  if (!sourceAvailable) return;
  privateRoot = mkdtempSync(join(tmpdir(), "urnetwork-js-generated-"));
  typesGenerator = join(privateRoot, "gen-types");
  apiGenerator = join(privateRoot, "gen-openapi");
  for (const [source, binary] of [["gen_types.go", typesGenerator], ["gen_openapi.go", apiGenerator]]) {
    const result = spawnSync("go", ["build", "-mod=readonly", "-o", binary, source], {
      cwd: jsDir, env, encoding: "utf8", timeout: 180000,
    });
    assert.equal(result.status, 0, `${source}: ${result.error || ""}${result.stderr}`);
  }
});
after(() => { if (privateRoot) rmSync(privateRoot, { recursive: true, force: true }); });

function fixture() {
  const dir = mkdtempSync(join(privateRoot, "case-"));
  mkdirSync(join(dir, "src/generated"), { recursive: true });
  for (const file of outputs) copyFileSync(join(generated, file), join(dir, "src/generated", file));
  return dir;
}

function snapshot(dir: string) {
  return outputs.map(file => {
    const path = join(dir, "src/generated", file);
    if (!existsSync(path)) return null;
    return { bytes: readFileSync(path), mtime: statSync(path, { bigint: true }).mtimeNs };
  });
}

function check(dir: string, file: string) {
  return spawnSync(file === "types.ts" ? typesGenerator : apiGenerator,
    file === "types.ts" ? ["-check"] : ["-spec", spec, "-check"], {
      cwd: dir, env, encoding: "utf8", timeout: 10000,
    });
}

test("generated verification errors have a complete exported shape", () => {
  const types = readFileSync(join(generated, "types.ts"), "utf8");
  const body = types.match(/export interface AuthVerifySendError \{([\s\S]*?)\n}/)?.[1] || "";
  assert.match(body, /\bcode: string;/);
  assert.match(body, /\bmessage: string;/);
  assert.match(body, /\bretry_after_seconds\?: number;/);
  assert.equal((types.match(/send_error\?: AuthVerifySendError \| null;/g) || []).length, 2);
});

test("every named reference in the generated Go interface grammar is declared", () => {
  const types = readFileSync(join(generated, "types.ts"), "utf8");
  const names = new Set([...types.matchAll(/^export interface (\w+) \{/gm)].map(match => match[1]));
  // This generator emits one-line field types and exported Go names. Record
  // is its only generic built-in; inline object field labels are not types.
  names.add("Record");
  for (const field of types.matchAll(/^  [\w]+\??: (.*);$/gm)) {
    const expression = field[1].replace(/\b\w+\??\s*:/g, "");
    for (const reference of expression.matchAll(/\b[A-Z]\w*\b/g)) {
      assert.ok(names.has(reference[0]), `undeclared generated type ${reference[0]} in ${field[0]}`);
    }
  }
});

test("both committed generator outputs are current without any write", { skip }, () => {
  const dir = fixture();
  const before = snapshot(dir);
  for (const file of ["types.ts", "openapi.ts"]) {
    const result = check(dir, file);
    assert.equal(result.status, 0, `${result.error || ""}${result.stderr}`);
  }
  assert.deepEqual(snapshot(dir), before, "a freshness check rewrote even identical output");
});

for (const file of outputs) {
  for (const mode of ["stale", "missing"]) {
    test(`${file} ${mode} check fails without repairing tracked-like output`, { skip }, () => {
      const dir = fixture();
      const path = join(dir, "src/generated", file);
      if (mode === "stale") writeFileSync(path, "// stale synthetic output\n");
      else rmSync(path);
      const before = snapshot(dir);
      const result = check(dir, file);
      assert.notEqual(result.status, 0);
      assert.match(result.stderr, /stale/);
      assert.deepEqual(snapshot(dir), before);
    });
  }

  test(`smoke refuses stale ${file} before generation, build or npm`, { skip }, () => {
    const dir = fixture();
    writeFileSync(join(dir, "src/generated", file), "// stale synthetic output\n");
    copyFileSync(join(jsDir, "Makefile"), join(dir, "Makefile"));
    mkdirSync(join(dir, "bin"));
    const quote = (value: string) => "'" + value.replaceAll("'", "'\\''") + "'";
    // Execute the real generators, but never a WASM build, npm, or network.
    writeFileSync(join(dir, "bin/go"), `#!/bin/sh
case "$1:$2" in
  run:gen_types.go) shift 2; exec ${quote(typesGenerator)} "$@" ;;
  run:gen_openapi.go) shift 2; exec ${quote(apiGenerator)} "$@" ;;
  *) echo unexpected-build >&2; exit 91 ;;
esac
`, { mode: 0o700 });
    writeFileSync(join(dir, "bin/npm"), "#!/bin/sh\necho unexpected-npm >&2\nexit 92\n", { mode: 0o700 });
    const before = snapshot(dir);
    const result = spawnSync("make", ["smoke", `OPENAPI_SPEC=${spec}`], {
      cwd: dir, env: { ...env, PATH: join(dir, "bin") + ":" + process.env.PATH },
      encoding: "utf8", timeout: 10000,
    });
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /stale/);
    assert.doesNotMatch(result.stderr, /unexpected-build|unexpected-npm/);
    assert.deepEqual(snapshot(dir), before);
  });
}

test("developer build still explicitly generates while smoke only checks", { skip }, () => {
  for (const target of ["build", "smoke"]) {
    const result = spawnSync("make", ["-n", target], { cwd: jsDir, env, encoding: "utf8", timeout: 10000 });
    assert.equal(result.status, 0, result.stderr);
    const commands = result.stdout.split("\n").filter(line => /go run gen_(types|openapi)\.go/.test(line));
    assert.equal(commands.length, 2, "both generated surfaces must be covered");
    for (const command of commands) assert.equal(command.includes("-check"), target === "smoke", command);
  }
});
