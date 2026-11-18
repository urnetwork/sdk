import { test } from "node:test";
import assert from "node:assert/strict";
import { existsSync, readFileSync } from "node:fs";
import { registerTsResolve } from "../ts-resolve.ts";

// Parity with connect's lagdrift, the authoritative Go model
// (lag.dev/DRIFTTEST.md V9, I4): the vectors lagdrift's drift-vectors writes,
// matched to 10⁻⁶. They are generated without fused multiply-adds, as the
// api's amd64 servers compute, from the connect worktree:
//
//   GOCACHE=$(mktemp -d) GOARCH=amd64 go run ./lagdrift/cmd/drift-vectors \
//     -out <sdk>/js/test/lag/drift-vectors.json

registerTsResolve();
const drift = await import("../../src/lag/drift/index.ts");

type Judgement = import("../../src/lag/drift/index.ts").Judgement;
type StopReason = import("../../src/lag/drift/index.ts").StopReason;

const VECTORS = new URL("./drift-vectors.json", import.meta.url);
const GENERATE =
  "GOCACHE=$(mktemp -d) GOARCH=amd64 go run ./lagdrift/cmd/drift-vectors -out <sdk>/js/test/lag/drift-vectors.json (connect, lag.dev/DRIFTTEST.md I4)";

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

// the Go's zero Judgement, which its Decide returns where the TypeScript's decide returns null
const zeroJudgement: Judgement = {
  inside: false,
  overlaps: false,
  d: 0,
  distanceKm: 0,
  significanceKm: 0,
  marginKm: 0,
  k: 0,
};

const STOP_REASONS: StopReason[] = ["interrupted", "clear", "converged", "cap"];

if (!existsSync(VECTORS)) {
  test("parity with connect's lagdrift", { skip: `test/lag/drift-vectors.json is missing; generate it with ${GENERATE}` }, () => {});
} else {
  // the vectors are JSON of the Go's types, whose fields are the TypeScript's
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  type Case = any;
  const vectors = JSON.parse(readFileSync(VECTORS, "utf8")) as Record<string, Case>;

  const borders = new Map<string, InstanceType<typeof drift.Borders>>();
  for (const entry of (vectors.borders ?? []) as Case[]) {
    borders.set(entry.name, new drift.Borders(entry.name, entry.countries));
  }

  // each section the generator writes, and how the TypeScript answers its cases
  const sections: Record<string, (c: Case) => void> = {
    geometry: (c) => {
      assertParity(drift.distanceKm(c.a, c.b), c.distanceKm, "distanceKm");
      assertParity(drift.bearingDeg(c.a, c.b), c.bearingDeg, "bearingDeg");
      const { eastKm, northKm } = drift.azimuthalKm(c.a, c.b);
      assertParity([eastKm, northKm], c.azimuthalKm, "azimuthalKm");
    },
    moveKm: (c) => assertParity(drift.moveKm(c.p, c.eastKm, c.northKm), c.result, "moveKm"),
    heldPlaceErrorKm: (c) => assertParity(drift.heldPlaceErrorKm(c.placeErrorKm), c.result, "heldPlaceErrorKm"),
    estimateRoundTrip: (c) => {
      const roundTrip = drift.estimateRoundTrip(c.samplesMs);
      if (!c.ok) {
        assert.equal(roundTrip, null, "no estimate");
        return;
      }
      assertParity(roundTrip, c.roundTrip, "roundTrip");
    },
    kForConfidence: (c) => assertParity(drift.kForConfidence(c.confidence), c.k, "k"),
    covEllipse: (c) => assertParity(drift.covEllipse(c.cov), { majorKm: c.majorKm, minorKm: c.minorKm, bearingDeg: c.bearingDeg }, "ellipse"),
    fitPosition: (c) => {
      const fit = drift.fitPosition(c.model, c.expected, c.observations, c.warm);
      if (c.error) {
        assert.equal(fit, null, `the Go's ${c.error}`);
        return;
      }
      assertParity(fit, c.fit, "fit");
    },
    refitPosition: (c) => {
      const fit = drift.refitPosition(c.model, c.expected, c.observations, c.warm);
      if (c.error) {
        assert.equal(fit, null, `the Go's ${c.error}`);
        return;
      }
      assertParity(fit, c.fit, "fit");
    },
    judge: (c) => assertParity(drift.judge(c.fit, c.country, c.k), c.judgement, "judgement"),
    defaultK: (c) => assertParity(drift.defaultK(c.confidence, c.continent), c.k, "k"),
    kEarly: (c) => assertParity(drift.kEarly(c.k), c.kEarly, "kEarly"),
    physicallyPossible: (c) => assert.equal(drift.physicallyPossible(c.observations, c.country), c.possible),
    evaluateStop: (c) => {
      const stop = drift.evaluateStop(c.history, c.openedMs, c.nowMs, c.lastPongMs, c.country, c.tolerance);
      assert.equal(stop !== null, c.stop, `stops: ${stop}`);
      assert.equal(stop ?? "", c.reason);
      assert.deepEqual(Object.keys(c.holds).sort(), [...STOP_REASONS].sort(), "every rule");
      for (const reason of STOP_REASONS) {
        const holds = drift.stopHolds(reason, c.history, c.openedMs, c.nowMs, c.lastPongMs, c.country, c.tolerance);
        assert.equal(holds, c.holds[reason], `stopHolds(${reason})`);
      }
    },
    countryAt: (c) => {
      const named = borders.get(c.countries);
      assert.ok(named, `no borders named ${c.countries}`);
      assert.equal(named.countryAt(c.point, c.withinKm), c.code);
    },
    decide: (c) => {
      const decision = drift.decide(c.fit, c.country, c.observations, c.k, c.maxSigmaKm);
      assert.equal(decision.verdict, c.verdict);
      assert.equal(decision.reason, c.reason);
      assertParity(decision.judgement ?? zeroJudgement, c.judgement, "judgement");
    },
    borders: (c) => assert.deepEqual(new drift.Borders(c.name, c.countries).codes(), c.codes),
  };

  test("parity: the vectors are lagdrift's, at the contract's tolerance", () => {
    assert.equal(vectors.format, "lagdrift-vectors/1");
    assert.equal(vectors.tolerance, TOLERANCE);
  });

  test("parity: every section of the vectors is checked", () => {
    const unknown = Object.keys(vectors).filter(
      (name) => !(name in sections) && !["format", "generator", "tolerance", "constants"].includes(name),
    );
    assert.deepEqual(unknown, [], `sections the TypeScript doesn't check yet: regenerate the test against ${GENERATE}`);
  });

  test("parity: constants", () => {
    const ours: Record<string, unknown> = { ...drift, defaultModel: drift.defaultModel() };
    for (const [name, expected] of Object.entries(vectors.constants as Record<string, unknown>)) {
      assert.ok(name in ours, `no constant ${name}`);
      assertParity(ours[name], expected, name);
    }
  });

  for (const [name, check] of Object.entries(sections)) {
    const cases = vectors[name];
    test(`parity: ${name}`, () => {
      assert.ok(Array.isArray(cases) && cases.length > 0, `${name} has no cases`);
      (cases as Case[]).forEach((vectorCase, i) => {
        try {
          check(vectorCase);
        } catch (error) {
          throw new Error(`${name}[${i}]${vectorCase.name ? ` (${String(vectorCase.name)})` : ""}: ${(error as Error).message}`, { cause: error });
        }
      });
    });
  }
}
