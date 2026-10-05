import { after, before, test } from "node:test";
import assert from "node:assert/strict";
import { copyFileSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

// The release uploads build/URnetworkSdkJs.zip and npm publishes wasm/, so a
// build recipe that fails must stop there: nothing stale or partial may be left
// standing as this build's output. These run the real Makefile on a private
// copy, with a go that compiles nothing and a zip that only marks the archive.
const jsDir = fileURLToPath(new URL("..", import.meta.url));
let root = "";

before(() => { root = mkdtempSync(join(tmpdir(), "urnetwork-js-build-recipes-")); });
after(() => { if (root) rmSync(root, { recursive: true, force: true }); });

function fixture(compile: "fails" | "succeeds", files: Record<string, string>) {
  const dir = mkdtempSync(join(root, "case-"));
  copyFileSync(join(jsDir, "Makefile"), join(dir, "Makefile"));
  mkdirSync(join(dir, "goroot/lib/wasm"), { recursive: true });
  writeFileSync(join(dir, "goroot/lib/wasm/wasm_exec.js"), "// toolchain glue\n");
  for (const [path, content] of Object.entries(files)) {
    mkdirSync(dirname(join(dir, path)), { recursive: true });
    writeFileSync(join(dir, path), content);
  }
  const quote = (value: string) => "'" + value.replaceAll("'", "'\\''") + "'";
  const build = compile === "fails"
    ? `echo "compile failed" >&2; exit 1`
    : `while [ $# -gt 0 ]; do if [ "$1" = -o ]; then printf 'fresh wasm\\n' > "$2"; exit 0; fi; shift; done; exit 2`;
  mkdirSync(join(dir, "bin"));
  writeFileSync(join(dir, "bin/go"), `#!/bin/sh
case "$1" in
  build) ${build} ;;
  env) echo ${quote(join(dir, "goroot"))} ;;
  version) echo "go version go0.0-fixture js/wasm" ;;
  *) echo "unexpected-go $*" >&2; exit 91 ;;
esac
`, { mode: 0o700 });
  writeFileSync(join(dir, "bin/zip"), "#!/bin/sh\n: > \"$2\"\n", { mode: 0o700 });
  writeFileSync(join(dir, "bin/npm"), "#!/bin/sh\necho unexpected-npm >&2\nexit 92\n", { mode: 0o700 });
  return dir;
}

function make(dir: string, target: string) {
  return spawnSync("make", [target], {
    cwd: dir, env: { ...process.env, PATH: join(dir, "bin") + ":" + process.env.PATH },
    encoding: "utf8", timeout: 10000,
  });
}

// The pair an earlier build left behind.
const earlierBuild = { "wasm/sdk.wasm": "stale wasm\n", "wasm/wasm_exec.js": "// toolchain glue\n" };
const bundleSources = { "index.js": "// loader\n", "index.html": "<!doctype html>\n" };

test("a failed wasm compile fails build_wasm and leaves no wasm to claim", () => {
  const dir = fixture("fails", earlierBuild);
  const result = make(dir, "build_wasm");
  assert.notEqual(result.status, 0, "build_wasm succeeded although its compile failed");
  assert.match(result.stderr, /compile failed/);
  assert.doesNotMatch(result.stdout, /wasm_exec\.js paired/, "check_wasm vouched for a failed build");
  assert.equal(existsSync(join(dir, "wasm/sdk.wasm")), false, "the earlier build's sdk.wasm is still standing");
  assert.equal(existsSync(join(dir, "wasm/wasm_exec.js")), false, "glue was left without a wasm built beside it");
});

test("build_bundle packages nothing when the wasm compile fails", () => {
  const dir = fixture("fails", { ...earlierBuild, ...bundleSources });
  const result = make(dir, "build_bundle");
  assert.notEqual(result.status, 0, "build_bundle succeeded although the wasm compile failed");
  assert.equal(existsSync(join(dir, "build/URnetworkSdkJs.zip")), false, "a stale wasm was bundled");
});

test("build_bundle zips nothing when a bundle file fails to copy", () => {
  const dir = fixture("succeeds", { "index.js": "// loader\n" });
  const result = make(dir, "build_bundle");
  assert.notEqual(result.status, 0, "build_bundle succeeded although a copy failed");
  assert.match(result.stderr, /index\.html/);
  assert.equal(existsSync(join(dir, "build/URnetworkSdkJs.zip")), false, "a partial bundle was zipped");
});

test("a successful build_bundle packages the freshly built wasm", () => {
  const dir = fixture("succeeds", { ...earlierBuild, ...bundleSources });
  const result = make(dir, "build_bundle");
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /wasm_exec\.js paired with go0\.0-fixture/);
  assert.equal(readFileSync(join(dir, "build/URnetworkSdkJs/sdk.wasm"), "utf8"), "fresh wasm\n");
  assert.ok(existsSync(join(dir, "build/URnetworkSdkJs.zip")));
});
