import { test } from "node:test";
import assert from "node:assert/strict";
import { existsSync, readFileSync } from "node:fs";
import { registerTsResolve } from "../ts-resolve.ts";

// Parity with connect's lagdrift, the authoritative Go model
// (lag.dev/DRIFTTEST.md V9, I4): the vectors `go run ./lagdrift/cmd/drift-vectors`
// writes from the connect worktree to this directory, matched to 10⁻⁶.

registerTsResolve();
const drift = await import("../../src/lag/drift/index.ts");

const VECTORS = new URL("./drift-vectors.json", import.meta.url);
const GENERATE = "go run ./lagdrift/cmd/drift-vectors (connect, lag.dev/DRIFTTEST.md I4)";

// 10⁻⁶, relative to the value once it is past 1
const TOLERANCE = 1e-6;

/** Every field the Go wrote is the TypeScript's, numbers to 10⁻⁶. */
function assertParity(actual: unknown, expected: unknown, path: string): void {
  if (typeof expected === "number") {
    assert.equal(typeof actual, "number", `${path}: ${String(actual)} is not a number`);
    const a = actual as number;
    if (!Number.isFinite(expected)) {
      assert.equal(a, expected, path);
      return;
    }
    assert.ok(Math.abs(a - expected) <= TOLERANCE * Math.max(1, Math.abs(expected)), `${path}: ${a} vs the Go's ${expected}`);
    return;
  }
  // encoding/json has no infinities: a generator may write them as text
  if (typeof expected === "string" && typeof actual === "number" && ["+Inf", "-Inf", "NaN", "Infinity", "-Infinity"].includes(expected)) {
    const want = expected === "NaN" ? Number.NaN : expected.startsWith("-") ? -Infinity : Infinity;
    assert.ok(Object.is(actual, want), `${path}: ${actual} vs the Go's ${expected}`);
    return;
  }
  if (Array.isArray(expected)) {
    assert.ok(Array.isArray(actual), `${path}: not a list`);
    assert.equal((actual as unknown[]).length, expected.length, `${path}: length`);
    expected.forEach((e, i) => assertParity((actual as unknown[])[i], e, `${path}[${i}]`));
    return;
  }
  if (expected !== null && typeof expected === "object") {
    assert.ok(actual !== null && typeof actual === "object", `${path}: ${String(actual)} is not an object`);
    for (const [key, e] of Object.entries(expected)) {
      assertParity((actual as Record<string, unknown>)[key], e, `${path}.${key}`);
    }
    return;
  }
  assert.equal(actual, expected, path);
}

test("parity helpers compare to 10⁻⁶, relative past 1", () => {
  assertParity({ a: [1.0000004, 2e6 + 1] }, { a: [1, 2e6] }, "x");
  assert.throws(() => assertParity({ a: 1.00001 }, { a: 1 }, "x"));
  assert.throws(() => assertParity([1, 2], [1], "x"));
  assertParity(Infinity, "+Inf", "x");
});

if (!existsSync(VECTORS)) {
  test("parity with connect's lagdrift", { skip: `test/lag/drift-vectors.json is missing; generate it with ${GENERATE}` }, () => {});
} else {
  const vectors = JSON.parse(readFileSync(VECTORS, "utf8")) as Record<string, unknown>;

  // each section the generator writes, and how the TypeScript answers its cases
  const sections: Record<string, (vectorCase: Record<string, unknown>) => void> = {};

  test("parity: every section of the vectors is checked", () => {
    const unknown = Object.keys(vectors).filter((name) => !(name in sections) && !["format", "version", "generator", "note"].includes(name));
    assert.deepEqual(unknown, [], `sections the TypeScript doesn't check yet: regenerate the test against ${GENERATE}`);
  });

  for (const [name, check] of Object.entries(sections)) {
    const cases = vectors[name];
    test(`parity: ${name}`, () => {
      assert.ok(Array.isArray(cases) && cases.length > 0, `${name} has no cases`);
      (cases as Record<string, unknown>[]).forEach((vectorCase, i) => {
        try {
          check(vectorCase);
        } catch (error) {
          throw new Error(`${name}[${i}]${vectorCase.name ? ` (${String(vectorCase.name)})` : ""}: ${(error as Error).message}`, { cause: error });
        }
      });
    });
  }
  void drift;
}
