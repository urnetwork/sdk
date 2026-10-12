import { test } from "node:test";
import assert from "node:assert/strict";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, relative } from "node:path";
import { fileURLToPath } from "node:url";
import { registerTsResolve } from "../ts-resolve.ts";

// lag.dev/PLAN.md S, decision 13: `@urnetwork/sdk/lag` is plain TypeScript
// built like ./client, and never loads the ~66 MB wasm
const jsDir = fileURLToPath(new URL("../..", import.meta.url));
const src = join(jsDir, "src");

// every source file the entry reaches through relative imports
function reachable(entry: string): string[] {
  const seen = new Set<string>();
  const visit = (file: string) => {
    if (seen.has(file)) return;
    seen.add(file);
    const source = readFileSync(file, "utf8");
    for (const match of source.matchAll(/(?:import|export)\s[^;]*?from\s+["'](\.[^"']+)["']|import\(\s*["'](\.[^"']+)["']\s*\)/g)) {
      const specifier = match[1] ?? match[2];
      const base = join(dirname(file), specifier);
      const resolved = [base, `${base}.ts`, `${base}.tsx`, join(base, "index.ts")].find((candidate) => existsSync(candidate) && candidate.match(/\.tsx?$/));
      assert.ok(resolved, `${relative(src, file)} imports ${specifier}, which does not resolve`);
      visit(resolved);
    }
  };
  visit(entry);
  return [...seen].map((file) => relative(src, file));
}

test("the lag entry never reaches the wasm loader", () => {
  const files = reachable(join(src, "lag/index.ts"));
  assert.ok(files.includes("lag/feed.ts"));
  for (const wasm of ["index.ts", "loader.ts", "socket.ts", "subprotocol.ts"]) {
    assert.ok(!files.includes(wasm), `the lag entry reaches ${wasm}: ${files.join(", ")}`);
  }
  for (const file of files) {
    assert.doesNotMatch(readFileSync(join(src, file), "utf8"), /wasm_exec|sdk\.wasm|getWasmGlobals/, file);
  }
});

test("the package exports ./lag as it exports ./client", () => {
  const pkg = JSON.parse(readFileSync(join(jsDir, "package.json"), "utf8"));
  assert.deepEqual(pkg.exports["./lag"], {
    import: "./dist/lag/index.js",
    require: "./dist/lag/index.cjs",
    types: "./dist/lag/index.d.ts",
  });
  assert.match(pkg.scripts.test, /test\/lag\/\*\.test\.ts/);
  const rollup = readFileSync(join(jsDir, "rollup.config.js"), "utf8");
  assert.match(rollup, /input: "src\/lag\/index\.ts"/);
});

test("the lag entry exports S1-S3", async () => {
  registerTsResolve();
  const lag = await import("../../src/lag/index.ts");
  for (const name of [
    "countryColorHex",
    "extenderColorHex",
    "extenderHue",
    "extenderHueColorHex",
    "canonicalIp",
    "extenderMeasurementHost",
    "parseExtenderMeasurementHost",
    "decodeExtenderFeedFrame",
    "decodeExtenderRecordBody",
    "decodeLagTicket",
    "encodeExtenderFeedRequest",
    "verifyEd25519",
    "ExtenderDirectory",
    "ExtenderFeedClient",
  ]) {
    assert.equal(typeof (lag as Record<string, unknown>)[name], "function", name);
  }
  assert.equal(Object.keys(lag.COUNTRY_COLOR_HEXES).length, 250);
  assert.equal(lag.extenderMeasurementHost("203.0.113.7"), "203-0-113-7.extender.bringyour.com");
});
