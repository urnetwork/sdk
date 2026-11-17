import { test } from "node:test";
import assert from "node:assert/strict";
import { registerTsResolve } from "../ts-resolve.ts";

// The drift model (lag.dev/DRIFTTEST.md V1-V7, V10; T2), on synthetic
// geometries with known positions. Parity with connect's lagdrift, the
// authoritative Go model, is drift_vectors.test.ts.

registerTsResolve();
const drift = await import("../../src/lag/drift/index.ts");
const { roundTripTau } = await import("../../src/lag/drift/roundTrip.ts");
const { pathSigmaMs, noiseVar } = await import("../../src/lag/drift/model.ts");
const { gaussSolve, smoothedDistanceKm } = await import("../../src/lag/drift/fit.ts");
const judgeModule = await import("../../src/lag/drift/judge.ts");
const { splitEdge } = judgeModule;
const fx = await import("./drift_fixtures.ts");

type Fit = import("../../src/lag/drift/index.ts").Fit;
type Point = import("../../src/lag/drift/index.ts").Point;
type Cov2 = import("../../src/lag/drift/index.ts").Cov2;

const close = (actual: number, expected: number, tolerance: number, message?: string) =>
  assert.ok(Math.abs(actual - expected) <= tolerance, `${message ?? ""} ${actual} is not within ${tolerance} of ${expected}`);

/** a fit at a place with a covariance, for the geometry that reads only those */
function fitAt(at: Point, cov: Cov2, expected: Point = at): Fit {
  const offset = drift.azimuthalKm(expected, at);
  return {
    at,
    eastKm: offset.eastKm,
    northKm: offset.northKm,
    lastMileMs: [5],
    runs: [1],
    cov,
    chi2PerDof: 1,
    extenders: 12,
    observations: 12,
    ...drift.covEllipse(cov),
  };
}

// the textbook k at 95%, 99% and 99.9% (V6); V11 calibrates them per continent
const K95 = drift.kForConfidence(0.95);
const K99 = drift.kForConfidence(0.99);
const K999 = drift.kForConfidence(0.999);

const iso = (sigmaKm: number): Cov2 => [
  [sigmaKm * sigmaKm, 0],
  [0, sigmaKm * sigmaKm],
];

// ---- geometry

test("distances and bearings on the sphere", () => {
  const london = { lat: 51.5074, lon: -0.1278 };
  const paris = { lat: 48.8566, lon: 2.3522 };
  close(drift.distanceKm(london, paris), 343.6, 1, "London to Paris");
  close(drift.distanceKm({ lat: 0, lon: 0 }, { lat: 90, lon: 0 }), (Math.PI / 2) * drift.earthRadiusKm, 1e-9);
  close(drift.distanceKm({ lat: 0, lon: 179.5 }, { lat: 0, lon: -179.5 }), (Math.PI / 180) * drift.earthRadiusKm, 1e-6, "across the antimeridian");
  assert.equal(drift.distanceKm(paris, paris), 0);
  const origin = { lat: 0, lon: 0 };
  close(drift.bearingDeg(origin, { lat: 10, lon: 0 }), 0, 1e-12);
  close(drift.bearingDeg(origin, { lat: 0, lon: 10 }), 90, 1e-12);
  close(drift.bearingDeg(origin, { lat: -10, lon: 0 }), 180, 1e-12);
  close(drift.bearingDeg(origin, { lat: 0, lon: -10 }), 270, 1e-12);
  assert.ok(drift.bearingDeg(paris, london) >= 0 && drift.bearingDeg(paris, london) < 360);
  assert.equal(drift.pointValid({ lat: 90, lon: -180 }), true);
  assert.equal(drift.pointValid({ lat: 90.1, lon: 0 }), false);
  assert.equal(drift.pointValid({ lat: Number.NaN, lon: 0 }), false);
});

test("the fit's step: east and north km on the local flat earth, as driftFit.ts moves", () => {
  assert.deepEqual(drift.moveKm({ lat: 10, lon: 20 }, 0, 110.574), { lat: 11, lon: 20 });
  close(drift.moveKm({ lat: 0, lon: 20 }, 111.32, 0).lon, 21, 1e-12);
  close(drift.moveKm({ lat: 60, lon: 20 }, 111.32, 0).lon, 22, 1e-9, "a degree of longitude is half as long at 60°");
  assert.equal(drift.moveKm({ lat: 89.8, lon: 0 }, 0, 500).lat, 89.9, "held short of the pole");
  close(drift.moveKm({ lat: 0, lon: 179.5 }, 111.32, 0).lon, -179.5, 1e-9, "across the antimeridian");
});

test("the azimuthal equidistant projection keeps distance and bearing from its center, and inverts", () => {
  const rng = fx.random(3);
  for (let i = 0; i < 200; i++) {
    const origin = { lat: -80 + 160 * rng(), lon: -180 + 360 * rng() };
    const e = (rng() - 0.5) * 8000;
    const n = (rng() - 0.5) * 8000;
    const p = drift.fromAzimuthalKm(origin, e, n);
    close(drift.distanceKm(origin, p), Math.hypot(e, n), 1e-6, "distance");
    const back = drift.azimuthalKm(origin, p);
    close(back.eastKm, e, 1e-6, "east");
    close(back.northKm, n, 1e-6, "north");
    assert.ok(p.lon >= -180 && p.lon < 180, `lon ${p.lon} wrapped`);
  }
  assert.deepEqual(drift.azimuthalKm(fx.CHICAGO, fx.CHICAGO), { eastKm: 0, northKm: 0 });
  assert.deepEqual(drift.fromAzimuthalKm(fx.CHICAGO, 0, 0), fx.CHICAGO);
});

test("Gauss–Jordan elimination with partial pivoting, and a singular system", () => {
  const A = [
    [4, 1, 0.5],
    [1, 3, 0.25],
    [0.5, 0.25, 2],
  ];
  const x = gaussSolve(A.map((row, i) => [...row, [1, 2, 3][i]]))!;
  A.forEach((row, i) => close(row.reduce((s, a, j) => s + a * x[j], 0), [1, 2, 3][i], 1e-12));
  assert.deepEqual(gaussSolve([[0, 2, 4], [3, 0, 3]]), [1, 2], "a zero pivot is swapped away");
  assert.equal(gaussSolve([[1, 2, 1], [2, 4, 2]]), null, "singular");
});

// ---- V1

test("V1: the 10th percentile by nearest rank; below 4 samples none; lost pings left out", () => {
  assert.equal(drift.estimateRoundTrip([]), null);
  assert.equal(drift.estimateRoundTrip([10, 11, 12]), null);
  assert.equal(drift.estimateRoundTrip([10, Number.NaN, 11, 12]), null, "a lost ping is no sample");
  assert.equal(drift.estimateRoundTrip([10, 0, -3, 11, 12, Infinity]), null, "nor is a round trip that isn't positive and finite");
  const shuffle = (n: number) => Array.from({ length: n }, (_, i) => ((i * 7) % n) + 1);
  assert.equal(drift.estimateRoundTrip(shuffle(20))!.ms, 2);
  assert.equal(drift.estimateRoundTrip(shuffle(10))!.ms, 1);
  assert.equal(drift.estimateRoundTrip(shuffle(11))!.ms, 2);
  assert.equal(drift.estimateRoundTrip(shuffle(30))!.ms, 3, "q n exactly whole stays on its rank");
  assert.equal(drift.estimateRoundTrip([5, 6, 7, 8])!.ms, 5);
  const counted = drift.estimateRoundTrip([12, Number.NaN, 9, 30, 15])!;
  assert.equal(counted.count, 4);
  assert.equal(counted.minMs, 9, "the lowest sample, for the physical check");
  const samples = [30, 10, 20, 40];
  drift.estimateRoundTrip(samples);
  assert.deepEqual(samples, [30, 10, 20, 40], "the input is not reordered");
  assert.equal(drift.minRoundTripSamples, 4);
});

test("V1: the error from order statistics at the effective sample size", () => {
  // alternating high and low: negative lag-1 autocorrelation, so τ = 1
  const alternating = (n: number) => {
    const low = Array.from({ length: Math.ceil(n / 2) }, (_, i) => i + 1);
    const high = Array.from({ length: Math.floor(n / 2) }, (_, i) => n - i);
    return low.flatMap((l, i) => (i < high.length ? [l, high[i]] : [l]));
  };
  assert.equal(roundTripTau(alternating(100)), 1);
  // n = 100: rank 10, half-width 1.96 √(100 · 0.09) = 5.88, so ranks 4 and 16
  const hundred = drift.estimateRoundTrip(alternating(100))!;
  assert.equal(hundred.ms, 10);
  close(hundred.sigmaMs, (16 - 4) / (2 * 1.96), 1e-12);
  // n = 20: the lower rank falls below the first, so the upper half is doubled:
  // rank 2, upper ceil(2 + 1.96 √1.8) = 5
  const twenty = drift.estimateRoundTrip(alternating(20))!;
  assert.equal(twenty.ms, 2);
  close(twenty.sigmaMs, (2 * (5 - 2)) / (2 * 1.96), 1e-12);
  // the same values in a slow drift are correlated: a wider interval
  const rampSamples = Array.from({ length: 100 }, (_, i) => i + 1);
  const ramp = drift.estimateRoundTrip(rampSamples)!;
  assert.equal(roundTripTau(rampSamples), 8, "held to 8");
  assert.equal(ramp.ms, 10);
  assert.ok(ramp.sigmaMs > hundred.sigmaMs * 2, `${ramp.sigmaMs} vs ${hundred.sigmaMs}`);
  // no spread, no error
  assert.deepEqual(drift.estimateRoundTrip([7, 7, 7, 7, 7, 7]), { ms: 7, sigmaMs: 0, count: 6, minMs: 7 });
  assert.equal(roundTripTau([7, 7, 7, 7]), 1);
});

// ---- V2, V3, V6 constants

test("V2, V3, V6, V10: the model's constants", () => {
  assert.deepEqual(drift.defaultModel(), { kappa: 0.25, sigma0Ms: 1 });
  assert.equal(drift.speedKmPerMs, 133.24);
  assert.equal(drift.vacuumKmPerMs, 299.792458);
  assert.equal(drift.msPerKm, 2 / 133.24);
  assert.equal(drift.roundTripQuantile, 0.1);
  assert.equal(drift.defaultConfidence, 0.99);
  assert.equal(drift.earlyK, 4);
  assert.equal(drift.huberC, 2);
  assert.equal(drift.maxStepKm, 400);
  assert.equal(drift.minFitExtenders, 3);
  assert.equal(drift.defaultMaxSigmaKm, 1500);
  close(drift.kForConfidence(0.95), 2.448, 1e-3);
  close(drift.kForConfidence(0.99), 3.035, 1e-3);
  close(drift.kForConfidence(0.999), 3.717, 1e-3);
  assert.equal(drift.kForConfidence(0.99), Math.sqrt(-2 * Math.log(0.01)));
  assert.equal(drift.kForConfidence(0), 0);
  assert.equal(drift.kForConfidence(-1), 0);
  assert.equal(drift.kForConfidence(1), Infinity);
  // s_ext is held to [10, 100] km, and an unknown one is a derived place's 50 km
  assert.equal(drift.heldPlaceErrorKm(3), 10);
  assert.equal(drift.heldPlaceErrorKm(40), 40);
  assert.equal(drift.heldPlaceErrorKm(500), 100);
  assert.equal(drift.heldPlaceErrorKm(0), 50);
  assert.equal(drift.heldPlaceErrorKm(-5), 50);
  assert.equal(drift.heldPlaceErrorKm(Number.NaN), 50);
  assert.equal(drift.heldPlaceErrorKm(Infinity), 50);
  // τ = √((κ K d)² + (K s)²), and σ_meas² + σ₀²
  close(pathSigmaMs(drift.defaultModel(), 1000, 25), Math.sqrt((0.25 * fx.K * 1000) ** 2 + (fx.K * 25) ** 2), 1e-15);
  assert.equal(noiseVar(drift.defaultModel(), 0.5), 1.25);
});

// ---- V5, V7

const truthNearChicago = { lat: 41.862, lon: -87.842 };
const exactChicago = (extra: Partial<Parameters<typeof fx.exactObservations>[0]> = {}) =>
  fx.exactObservations({ truth: truthNearChicago, extenders: fx.CHICAGO_EXTENDERS, lastMileMs: 7.5, ...extra });

test("V5: a single run's fit finds the place exact round trips come from", () => {
  const fit = drift.fitPosition(drift.defaultModel(), fx.CHICAGO, exactChicago(), null)!;
  assert.ok(fit);
  close(drift.distanceKm(fit.at, truthNearChicago), 0, 0.05, "position, km");
  close(fit.lastMileMs[0], 7.5, 0.005, "b");
  assert.deepEqual(fit.runs, [1]);
  assert.equal(fit.extenders, 12);
  assert.equal(fit.observations, 12);
  close(fit.chi2PerDof, 0, 1e-3);
  const offset = drift.azimuthalKm(fx.CHICAGO, fit.at);
  assert.equal(fit.eastKm, offset.eastKm);
  assert.equal(fit.northKm, offset.northKm);
  assert.ok(fit.majorKm >= fit.minorKm && fit.minorKm > 0);
  assert.ok(fit.bearingDeg >= 0 && fit.bearingDeg < 180);
  assert.deepEqual({ majorKm: fit.majorKm, minorKm: fit.minorKm, bearingDeg: fit.bearingDeg }, drift.covEllipse(fit.cov));
});

test("V5: no fit from fewer than 3 extenders or 3 + R observations, an invalid place, or unusable round trips", () => {
  const all = exactChicago();
  assert.equal(drift.fitPosition(drift.defaultModel(), fx.CHICAGO, all.slice(0, 3), null), null, "3 observations for 3 parameters");
  assert.ok(drift.fitPosition(drift.defaultModel(), fx.CHICAGO, all.slice(0, 4), null));
  assert.equal(drift.fitPosition(drift.defaultModel(), fx.CHICAGO, [], null), null);
  // one extender measured in two runs is still one extender
  const repeated = [all[0], all[1], { ...all[0], run: 2 }, { ...all[1], run: 2 }, { ...all[0], run: 3 }];
  assert.equal(drift.fitPosition(drift.defaultModel(), fx.CHICAGO, repeated, null), null);
  assert.equal(drift.fitPosition(drift.defaultModel(), { lat: 91, lon: 0 }, all, null), null, "an invalid expected place");
  // an unusable observation is left out
  const broken = all.map((o, i) =>
    i < 9 ? { ...o, roundTrip: { ...o.roundTrip, ms: i % 3 === 0 ? 0 : i % 3 === 1 ? Number.NaN : o.roundTrip.ms, sigmaMs: i % 3 === 2 ? -1 : o.roundTrip.sigmaMs } } : o,
  );
  assert.equal(drift.fitPosition(drift.defaultModel(), fx.CHICAGO, broken, null), null, "3 usable");
  const placeless = all.map((o, i) => (i === 0 ? { ...o, at: { lat: 0, lon: 200 } } : o));
  assert.equal(drift.fitPosition(drift.defaultModel(), fx.CHICAGO, placeless, null)!.extenders, 11);
});

test("V5: a warm start settles where a cold one does", () => {
  const observations = fx.exactObservations({
    truth: { lat: 42.4, lon: -88.3 },
    extenders: fx.CHICAGO_EXTENDERS,
    lastMileMs: 9,
    stretch: [0.1, -0.05, 0.2, 0, 0.15, -0.08, 0.3, 0.05, 0.12, -0.02, 0.07, 0.2],
  });
  const cold = drift.fitPosition(drift.defaultModel(), fx.CHICAGO, observations, null)!;
  // the last look's fit, 100 km off, with another b
  const last = { ...fitAt(drift.fromAzimuthalKm(cold.at, 80, -60), iso(10), fx.CHICAGO), lastMileMs: [4] };
  const warm = drift.fitPosition(drift.defaultModel(), fx.CHICAGO, observations, last)!;
  // each start stops once a step barely lowers F and the whole step from there
  // is under 0.1 km, so the two settle near each other rather than on one point
  close(drift.distanceKm(cold.at, warm.at), 0, 0.5, "km apart");
  close(cold.lastMileMs[0], warm.lastMileMs[0], 0.05);
  // a warm start from a fit over other runs takes its b only for the runs it shares
  const otherRuns = { ...last, runs: [7], lastMileMs: [4] };
  close(drift.distanceKm(drift.fitPosition(drift.defaultModel(), fx.CHICAGO, observations, otherRuns)!.at, cold.at), 0, 0.5);
});

test("V10: a refit from the warm fit lands where the full fit does", () => {
  const observations = exactChicago({ stretch: [0.05, -0.05, 0.1, 0, 0.08, -0.04, 0.15, 0.02, 0.06, -0.01, 0.03, 0.1] });
  const full = drift.fitPosition(drift.defaultModel(), fx.CHICAGO, observations, null)!;
  const refit = drift.refitPosition(drift.defaultModel(), fx.CHICAGO, observations, full)!;
  assert.ok(drift.distanceKm(refit.at, full.at) < 0.1, `${drift.distanceKm(refit.at, full.at)} km apart`);
  close(refit.majorKm, full.majorKm, full.majorKm * 1e-3);
  assert.deepEqual(refit.runs, full.runs);
  // a tick later: a few more pings, the same basin
  const later = observations.map((o) => ({ ...o, roundTrip: { ...o.roundTrip, ms: o.roundTrip.ms + 0.05 } }));
  const fullLater = drift.fitPosition(drift.defaultModel(), fx.CHICAGO, later, null)!;
  const refitLater = drift.refitPosition(drift.defaultModel(), fx.CHICAGO, later, full)!;
  assert.ok(drift.distanceKm(refitLater.at, fullLater.at) < 0.1);
});

test("V10: without a warm fit a refit is the full fit, and a far warm fit still settles", () => {
  const observations = exactChicago();
  assert.deepEqual(
    drift.refitPosition(drift.defaultModel(), fx.CHICAGO, observations, null),
    drift.fitPosition(drift.defaultModel(), fx.CHICAGO, observations, null),
  );
  const invalid = { ...fitAt({ lat: 95, lon: 0 }, iso(10)) };
  assert.deepEqual(
    drift.refitPosition(drift.defaultModel(), fx.CHICAGO, observations, invalid),
    drift.fitPosition(drift.defaultModel(), fx.CHICAGO, observations, null),
  );
  const far = { ...fitAt(drift.fromAzimuthalKm(truthNearChicago, 300, -200), iso(10), fx.CHICAGO), lastMileMs: [2] };
  const refit = drift.refitPosition(drift.defaultModel(), fx.CHICAGO, observations, far)!;
  assert.ok(drift.distanceKm(refit.at, truthNearChicago) < 1, `${drift.distanceKm(refit.at, truthNearChicago)} km from the truth`);
  assert.equal(drift.refitPosition(drift.defaultModel(), fx.CHICAGO, observations.slice(0, 2), far), null, "two extenders");
});

test("V7: one position and one last-mile offset per run", () => {
  const run = (run: number, lastMileMs: number) =>
    fx.exactObservations({ truth: truthNearChicago, extenders: fx.CHICAGO_EXTENDERS, lastMileMs, run });
  const fit = drift.fitPosition(drift.defaultModel(), fx.CHICAGO, [...run(3, 12), ...run(1, 5)], null)!;
  assert.deepEqual(fit.runs, [1, 3]);
  close(fit.lastMileMs[0], 5, 0.01);
  close(fit.lastMileMs[1], 12, 0.01);
  close(drift.distanceKm(fit.at, truthNearChicago), 0, 0.05);
  assert.equal(fit.extenders, 12, "the same 12 paths twice");
  assert.equal(fit.observations, 24);
});

test("V2: the predicted distance is smoothed within 10 km of an extender, keeping its value and slope at 10 km", () => {
  assert.deepEqual(smoothedDistanceKm(250), [250, 1]);
  assert.deepEqual(smoothedDistanceKm(10), [10, 1]);
  assert.deepEqual(smoothedDistanceKm(0), [5, 0]);
  const [justInside, slope] = smoothedDistanceKm(10 - 1e-9);
  close(justInside, 10, 1e-8);
  close(slope, 1, 1e-8);
  close(smoothedDistanceKm(4)[0], (16 + 100) / 20, 1e-12);
});

test("V5: a visitor on an extender's own place settles there", () => {
  // the visitor is in Milwaukee, where an extender is: d's cone at its place is smoothed away
  const milwaukee = fx.CHICAGO_EXTENDERS[0];
  const observations = fx.exactObservations({ truth: milwaukee, extenders: fx.CHICAGO_EXTENDERS, lastMileMs: 6 });
  const fit = drift.fitPosition(drift.defaultModel(), fx.CHICAGO, observations, null)!;
  assert.ok(drift.distanceKm(fit.at, milwaukee) < 15, `${drift.distanceKm(fit.at, milwaukee)} km from Milwaukee`);
});

test("V5: the fit depends on the data, not on where a warm start left off, once it settles", () => {
  const observations = exactChicago({ stretch: [0.05, -0.05, 0.1, 0, 0.08, -0.04, 0.15, 0.02, 0.06, -0.01, 0.03, 0.1] });
  const cold = drift.fitPosition(drift.defaultModel(), fx.CHICAGO, observations, null)!;
  for (const [e, n] of [[30, 0], [0, -40], [-25, 25]]) {
    const warm = drift.fitPosition(drift.defaultModel(), fx.CHICAGO, observations, { ...cold, at: drift.fromAzimuthalKm(cold.at, e, n) })!;
    assert.ok(drift.distanceKm(cold.at, warm.at) < 1, `${drift.distanceKm(cold.at, warm.at)} km apart`);
  }
});

test("V5: a b at its bound of 0 is held, and the covariance still leaves it free", () => {
  // round trips shorter than the distance allows push b below 0
  const fit = drift.fitPosition(drift.defaultModel(), fx.CHICAGO, exactChicago({ lastMileMs: -2 }), null)!;
  assert.ok(fit);
  assert.equal(fit.lastMileMs[0], 0);
  assert.ok(fit.cov.every((row) => row.every(Number.isFinite)));
  assert.ok(fit.minorKm > 0);
  // the fit slid away until b reached 0; conditioning on b = 0 would shorten
  // the long axis to about 82 km here, and the full inverse keeps that freedom
  const interior = drift.fitPosition(drift.defaultModel(), fx.CHICAGO, exactChicago({ lastMileMs: 5 }), null)!;
  assert.ok(interior.lastMileMs[0] > 0);
  assert.ok(fit.majorKm >= interior.majorKm, `at the bound ${fit.majorKm} km, inside ${interior.majorKm} km`);
});

test("V5: Huber weights keep one detoured path from dragging the position", () => {
  const clean = drift.fitPosition(drift.defaultModel(), fx.CHICAGO, exactChicago(), null)!;
  const detoured = exactChicago();
  // Atlanta's path detours by 40 ms, about 2,700 km of fiber; unweighted, it
  // would pull the fit over 300 km
  detoured[5] = { ...detoured[5], roundTrip: { ...detoured[5].roundTrip, ms: detoured[5].roundTrip.ms + 40 } };
  const fit = drift.fitPosition(drift.defaultModel(), fx.CHICAGO, detoured, null)!;
  const shift = drift.distanceKm(fit.at, truthNearChicago);
  assert.ok(shift < 60, `the fit moved ${shift} km`);
  assert.ok(drift.distanceKm(clean.at, truthNearChicago) < 0.05);
  // the residuals are larger than the model expects, so the ellipse widens
  assert.ok(fit.chi2PerDof > 1);
  assert.ok(fit.majorKm * fit.minorKm > clean.majorKm * clean.minorKm);
});

test("V5: the covariance widens with the residuals and with the measurement error", () => {
  const rng = fx.random(11);
  const noisy = exactChicago().map((o) => ({ ...o, roundTrip: { ...o.roundTrip, ms: o.roundTrip.ms + fx.gaussian(rng) * 6 } }));
  const clean = drift.fitPosition(drift.defaultModel(), fx.CHICAGO, exactChicago(), null)!;
  const fit = drift.fitPosition(drift.defaultModel(), fx.CHICAGO, noisy, null)!;
  assert.ok(fit.chi2PerDof > 1, `χ²/dof ${fit.chi2PerDof}`);
  assert.ok(fit.majorKm > clean.majorKm);
  const loose = drift.fitPosition(drift.defaultModel(), fx.CHICAGO, exactChicago({ sigmaMs: 5 }), null)!;
  assert.ok(loose.majorKm > clean.majorKm);
});

test("T2: a repeated path's route error isn't counted twice", () => {
  // three runs on the same 12 paths, and three runs on 36 distinct paths of the same geometry
  const shifted = (k: number) => fx.CHICAGO_EXTENDERS.map((p) => drift.fromAzimuthalKm(p, 40 * k, -30 * k));
  const same = [1, 2, 3].flatMap((run) =>
    fx.exactObservations({ truth: truthNearChicago, extenders: fx.CHICAGO_EXTENDERS, lastMileMs: 7, run }),
  );
  const distinct = [1, 2, 3].flatMap((run) =>
    fx.exactObservations({
      truth: truthNearChicago,
      extenders: shifted(run - 2),
      lastMileMs: 7,
      run,
      ids: fx.CHICAGO_EXTENDERS.map((_, i) => `r${run}e${i}`),
    }),
  );
  const one = drift.fitPosition(drift.defaultModel(), fx.CHICAGO, same.filter((o) => o.run === 1), null)!;
  const repeated = drift.fitPosition(drift.defaultModel(), fx.CHICAGO, same, null)!;
  const spread = drift.fitPosition(drift.defaultModel(), fx.CHICAGO, distinct, null)!;
  assert.equal(repeated.extenders, 12);
  assert.equal(spread.extenders, 36);
  const area = (f: Fit) => f.majorKm * f.minorKm;
  assert.ok(area(repeated) > area(spread) * 1.5, `repeated ${area(repeated)}, distinct ${area(spread)}`);
  // a repeat shrinks only the measurement error, never the shared route error:
  // three runs are narrower than one, but by less than three runs' worth
  assert.ok(area(repeated) < area(one) && area(repeated) > area(one) / 3, `repeated ${area(repeated)}, one ${area(one)}`);
});

test("the 1σ ellipse of a covariance: its axes and the major axis' bearing", () => {
  assert.deepEqual(drift.covEllipse([[400, 0], [0, 100]]), { majorKm: 20, minorKm: 10, bearingDeg: 90 });
  assert.deepEqual(drift.covEllipse([[100, 0], [0, 400]]), { majorKm: 20, minorKm: 10, bearingDeg: 0 });
  const ne = drift.covEllipse([[250, 150], [150, 250]]);
  close(ne.majorKm, 20, 1e-12);
  close(ne.minorKm, 10, 1e-12);
  close(ne.bearingDeg, 45, 1e-12);
  close(drift.covEllipse([[250, -150], [-150, 250]]).bearingDeg, 135, 1e-12);
  // the off-diagonal is the mean of the two
  close(drift.covEllipse([[250, 140], [160, 250]]).bearingDeg, 45, 1e-12);
});

// ---- V6

test("V6: inside the country agrees, with the region's radius toward the nearest border", () => {
  const country = fx.squareCountry("aa", fx.CHICAGO, 200);
  const j = drift.judge(fitAt(fx.CHICAGO, iso(20)), country, K99);
  assert.equal(j.inside, true);
  assert.equal(j.overlaps, true);
  assert.equal(j.distanceKm, 0);
  close(j.d, 200 / 20, 0.05);
  close(j.significanceKm, K99 * 20, 1e-6, "isotropic: k σ every way");
  assert.equal(j.marginKm, -j.significanceKm);
  assert.equal(j.k, K99);
});

test("V6: outside, the region overlapping the border agrees and one clear of it disagrees", () => {
  const country = fx.squareCountry("aa", fx.CHICAGO, 200);
  const at = drift.fromAzimuthalKm(fx.CHICAGO, 300, 0);
  const clear = drift.judge(fitAt(at, iso(20)), country, K99);
  assert.equal(clear.inside, false);
  assert.equal(clear.overlaps, false);
  close(clear.distanceKm, 100, 0.5);
  close(clear.d, 100 / 20, 0.05);
  close(clear.significanceKm, 3.0349 * 20, 1e-3);
  assert.ok(clear.marginKm > 0);
  const touching = drift.judge(fitAt(at, iso(50)), country, K99);
  assert.equal(touching.inside, false);
  assert.equal(touching.overlaps, true);
  assert.ok(touching.marginKm < 0);
  // a smaller k draws a smaller region, and a calibrated one a larger
  assert.equal(drift.judge(fitAt(at, iso(38)), country, K95).overlaps, false);
  assert.equal(drift.judge(fitAt(at, iso(38)), country, K999).overlaps, true);
  assert.equal(drift.judge(fitAt(at, iso(20)), country, drift.defaultK(0.99, "OC")).overlaps, true, "Oceania's 7.88");
});

test("V6: margin > 0 exactly when D > k, and the radius is along the line to q, for tilted ellipses", () => {
  const rng = fx.random(5);
  const country = fx.squareCountry("aa", fx.MADRID, 250);
  for (let i = 0; i < 300; i++) {
    const at = drift.fromAzimuthalKm(fx.MADRID, (rng() - 0.5) * 1400, (rng() - 0.5) * 1400);
    const a = 5 + rng() * 120;
    const b = 5 + rng() * 120;
    const theta = rng() * Math.PI;
    const c = Math.cos(theta);
    const s = Math.sin(theta);
    const cov: Cov2 = [
      [a * a * c * c + b * b * s * s, (a * a - b * b) * c * s],
      [(a * a - b * b) * c * s, a * a * s * s + b * b * c * c],
    ];
    const k = [K95, K99, 3.13, 3.71, 7.88][i % 5];
    const j = drift.judge(fitAt(at, cov), country, k);
    if (!j.inside) {
      assert.equal(j.marginKm > 0, j.d > k, `case ${i}: margin ${j.marginKm}, D ${j.d}, k ${k}`);
      assert.equal(j.k, k);
      assert.equal(j.overlaps, j.d <= k);
      close(j.marginKm, j.distanceKm - j.significanceKm, 1e-9 * j.distanceKm);
      // the radius toward q from the region's own equation: k / √(uᵀΣ⁻¹u)
      const q = drift.azimuthalKm(at, at);
      assert.deepEqual(q, { eastKm: 0, northKm: 0 });
      close(j.significanceKm, (k * j.distanceKm) / j.d, 1e-9 * j.distanceKm);
    } else {
      assert.equal(j.overlaps, true);
      assert.ok(j.marginKm < 0);
    }
  }
});

test("V6: a concave border is judged exactly", () => {
  // a U open to the north: the fit sits in the notch, outside the country
  const u = fx.densify(
    fx.ringAround(fx.MADRID, [
      [-300, -300],
      [300, -300],
      [300, 300],
      [100, 300],
      [100, -100],
      [-100, -100],
      [-100, 300],
      [-300, 300],
    ]),
    10,
  );
  const country = { code: "aa", polygons: [[u]] };
  const notch = drift.fromAzimuthalKm(fx.MADRID, 0, 150);
  const narrow = drift.judge(fitAt(notch, iso(15)), country, K99);
  assert.equal(narrow.inside, false);
  // the arms are 100 km away: D = 100 / 15 > k
  close(narrow.distanceKm, 100, 1);
  assert.equal(narrow.overlaps, false, "inside the hull but clear of the border");
  assert.equal(drift.judge(fitAt(notch, iso(40)), country, K99).overlaps, true);
});

test("V6: an island inside the region counts as touching, and a hole is outside", () => {
  const far = fx.squareCountry("aa", fx.MADRID, 100);
  const at = drift.fromAzimuthalKm(fx.MADRID, 900, 0);
  const island = fx.ringAround(drift.fromAzimuthalKm(at, 0, 80), [
    [-3, -3],
    [3, -3],
    [3, 3],
    [-3, 3],
  ]);
  const withIsland = { code: "aa", polygons: [...far.polygons, [island]] };
  assert.equal(drift.judge(fitAt(at, iso(40)), far, K99).overlaps, false);
  const j = drift.judge(fitAt(at, iso(40)), withIsland, K99);
  assert.equal(j.overlaps, true);
  close(j.distanceKm, 77, 1);
  // a lake: the fit in the hole is outside the country, 50 km from its shore
  const outer = fx.densify(fx.ringAround(fx.MADRID, [[-400, -400], [400, -400], [400, 400], [-400, 400]]), 20);
  const hole = fx.densify(fx.ringAround(fx.MADRID, [[-50, -50], [50, -50], [50, 50], [-50, 50]]), 5);
  const lake = { code: "aa", polygons: [[outer, hole]] };
  const inHole = drift.judge(fitAt(fx.MADRID, iso(5)), lake, K99);
  assert.equal(inHole.inside, false);
  close(inHole.distanceKm, 50, 1);
  assert.equal(inHole.overlaps, false);
  assert.equal(drift.judge(fitAt(drift.fromAzimuthalKm(fx.MADRID, 200, 0), iso(5)), lake, K99).inside, true);
});

test("V6: polygons at the antimeridian and around a pole", () => {
  // a country split at ±180, as Natural Earth draws Fiji
  const east = [
    { lon: 175, lat: -20 },
    { lon: 180, lat: -20 },
    { lon: 180, lat: -15 },
    { lon: 175, lat: -15 },
  ];
  const west = [
    { lon: -180, lat: -20 },
    { lon: -175, lat: -20 },
    { lon: -175, lat: -15 },
    { lon: -180, lat: -15 },
  ];
  const fiji = { code: "fj", polygons: [[east], [west]] };
  assert.equal(drift.judge(fitAt({ lat: -17, lon: 179.9 }, iso(10)), fiji, K99).inside, true);
  assert.equal(drift.judge(fitAt({ lat: -17, lon: -179.5 }, iso(10)), fiji, K99).inside, true);
  const off = drift.judge(fitAt({ lat: -17.5, lon: 170 }, iso(10)), fiji, K99);
  assert.equal(off.inside, false);
  close(off.distanceKm, drift.distanceKm({ lat: -17.5, lon: 170 }, { lat: -17.5, lon: 175 }), 2);
  // a polar cap closed along the pole, as Natural Earth draws Antarctica
  const cap = [
    { lon: -180, lat: -80 },
    { lon: -90, lat: -80 },
    { lon: 0, lat: -80 },
    { lon: 90, lat: -80 },
    { lon: 180, lat: -80 },
    { lon: 180, lat: -90 },
    { lon: -180, lat: -90 },
  ];
  const aq = { code: "aq", polygons: [[cap]] };
  assert.equal(drift.judge(fitAt({ lat: -85, lon: 30 }, iso(10)), aq, K99).inside, true);
  assert.equal(drift.judge(fitAt({ lat: -89.5, lon: -120 }, iso(10)), aq, K99).inside, true);
  const north = drift.judge(fitAt({ lat: -75, lon: 0 }, iso(10)), aq, K99);
  assert.equal(north.inside, false);
  close(north.distanceKm, drift.distanceKm({ lat: -75, lon: 0 }, { lat: -80, lon: 0 }), 1);
});

test("V6: Natural Earth's cuts along the antimeridian and a pole are no border", () => {
  const { isCut } = judgeModule;
  assert.equal(isCut({ lat: -20, lon: 180 }, { lat: -15, lon: 180 }), true);
  assert.equal(isCut({ lat: -20, lon: -180 }, { lat: -15, lon: -180 }), true);
  assert.equal(isCut({ lat: -90, lon: 180 }, { lat: -90, lon: -180 }), true);
  assert.equal(isCut({ lat: -20, lon: 180 }, { lat: -15, lon: -180 }), false, "across, not along");
  assert.equal(isCut({ lat: -20, lon: 179.999 }, { lat: -15, lon: 179.999 }), false);
  // a fit beside the cut in a country split there is as far from the border as from its real edges, 2.5° of latitude
  const fiji = {
    code: "fj",
    polygons: [
      [[{ lon: 175, lat: -20 }, { lon: 180, lat: -20 }, { lon: 180, lat: -15 }, { lon: 175, lat: -15 }]],
      [[{ lon: -180, lat: -20 }, { lon: -175, lat: -20 }, { lon: -175, lat: -15 }, { lon: -180, lat: -15 }]],
    ],
  };
  const beside = drift.judge(fitAt({ lat: -17.5, lon: 179.9 }, iso(10)), fiji, K99);
  assert.equal(beside.inside, true);
  close(beside.d, drift.distanceKm({ lat: -17.5, lon: 179.9 }, { lat: -15, lon: 179.9 }) / 10, 0.05);
  // near the pole, the pole is no border: the coast at 80°S is
  const coast = Array.from({ length: 73 }, (_, i) => ({ lon: -180 + 5 * i, lat: -80 }));
  const aq = { code: "aq", polygons: [[[...coast, { lon: 180, lat: -90 }, { lon: -180, lat: -90 }]]] };
  const nearPole = drift.judge(fitAt({ lat: -89.5, lon: -120 }, iso(10)), aq, K99);
  assert.equal(nearPole.inside, true);
  close(nearPole.d, drift.distanceKm({ lat: -89.5, lon: -120 }, { lat: -80, lon: -120 }) / 10, 0.5);
});

test("V6: the antipode of a country doesn't put the fit in it, nor near it", () => {
  // Buenos Aires' antipode is in eastern China
  const buenosAires = { lat: -34.6, lon: -58.4 };
  const china = fx.squareCountry("cn", { lat: 34.6, lon: 121.6 }, 500);
  const j = drift.judge(fitAt(buenosAires, iso(50)), china, K99);
  assert.equal(j.inside, false);
  assert.equal(j.overlaps, false);
  assert.ok(j.distanceKm > 15000, `${j.distanceKm}`);
});

test("V6: no country, or one with no border, is never touched", () => {
  for (const country of [null, { code: "zz", polygons: [] }]) {
    const j = drift.judge(fitAt(fx.MADRID, iso(10)), country, K99);
    assert.deepEqual(j, { inside: false, overlaps: false, d: Infinity, distanceKm: Infinity, significanceKm: 0, marginKm: Infinity, k: K99 });
  }
  assert.equal(drift.judge(null, fx.squareCountry("aa", fx.MADRID, 100), K99).d, Infinity);
});

test("V6: an edge is split every 0.25° before it is projected", () => {
  const points: Point[] = [];
  splitEdge({ lat: 0, lon: 0 }, { lat: 1, lon: 0.1 }, (p) => points.push(p));
  assert.equal(points.length, 4);
  assert.deepEqual(points[3], { lat: 1, lon: 0.1 });
  close(points[0].lat, 0.25, 1e-15);
  const one: Point[] = [];
  splitEdge({ lat: 0, lon: 0 }, { lat: 0, lon: 0 }, (p) => one.push(p));
  assert.deepEqual(one, [{ lat: 0, lon: 0 }]);
});

test("V8: the region's ± and its outline at the region's k", () => {
  const cov: Cov2 = [
    [900, 300],
    [300, 400],
  ];
  const fit = fitAt(fx.MADRID, cov);
  close(drift.regionRadiusKm(fit, K99), K99 * Math.sqrt(fit.majorKm * fit.minorKm), 1e-12);
  close(drift.regionRadiusKm(fit, 7.88), 7.88 * Math.sqrt(fit.majorKm * fit.minorKm), 1e-12);
  const outline = drift.regionOutline(fit, K99, 48);
  assert.equal(outline.length, 48);
  const det = cov[0][0] * cov[1][1] - cov[0][1] * cov[1][0];
  for (const p of outline) {
    const { eastKm: e, northKm: n } = drift.azimuthalKm(fit.at, p);
    const m2 = (e * e * cov[1][1] - 2 * e * n * cov[0][1] + n * n * cov[0][0]) / det;
    close(Math.sqrt(m2), K99, 1e-6);
  }
});

// ---- V5's physical check

test("V5: the physical check passes a consistent run and fails a country out of light's reach", () => {
  const observations = exactChicago();
  assert.equal(drift.physicallyPossible(observations, fx.squareCountry("aa", fx.CHICAGO, 200)), true);
  assert.equal(drift.physicallyPossible(observations, fx.squareCountry("bb", fx.MADRID, 200)), false);
  assert.equal(drift.physicallyPossible(observations, fx.squareCountry("cc", { lat: 20, lon: -155 }, 100)), false);
  // nothing rules a country out without two discs, and no country is never impossible
  assert.equal(drift.physicallyPossible([], fx.squareCountry("bb", fx.MADRID, 200)), true);
  assert.equal(drift.physicallyPossible(observations.slice(0, 1), fx.squareCountry("bb", fx.MADRID, 200)), true);
  assert.equal(drift.physicallyPossible(observations, { code: "zz", polygons: [] }), true);
  assert.equal(drift.physicallyPossible(observations, null), true);
});

test("V5: one misplaced extender can't make a country impossible, two can", () => {
  const observations = exactChicago();
  // an extender that claims Tokyo but answers in 10 ms
  const misplaced = (o: (typeof observations)[number], at: Point) => ({ ...o, at, roundTrip: { ...o.roundTrip, ms: 10 } });
  const one = observations.map((o, i) => (i === 0 ? misplaced(o, { lat: 35.68, lon: 139.69 }) : o));
  const two = one.map((o, i) => (i === 1 ? misplaced(o, { lat: -33.87, lon: 151.21 }) : o));
  const country = fx.squareCountry("aa", fx.CHICAGO, 200);
  assert.equal(drift.physicallyPossible(one, country), true);
  assert.equal(drift.physicallyPossible(two, country), false);
});

test("V5: a disc reaches as far as the extender's lowest round trip in any run", () => {
  const observations = exactChicago();
  const country = fx.squareCountry("cc", { lat: 20, lon: -155 }, 100);
  assert.equal(drift.physicallyPossible(observations, country), false);
  // round trips of 100 ms reach about 15,000 km from every extender, London's and São Paulo's too
  const far = observations.map((o) => ({ ...o, roundTrip: { ...o.roundTrip, ms: 100, minMs: 0 } }));
  assert.equal(drift.physicallyPossible(far, country), true);
  // minMs, when known and below ms, is the one used
  const lowered = far.map((o) => ({ ...o, roundTrip: { ...o.roundTrip, minMs: 5 } }));
  assert.equal(drift.physicallyPossible(lowered, country), false);
});

test("V5: the check finds a country holding the discs' intersection without touching a border", () => {
  assert.equal(drift.physicallyPossible(exactChicago(), fx.squareCountry("aa", fx.CHICAGO, 3000)), true);
});

// ---- V6's verdict over a fit

test("V6: the verdict over a fit, as the api decides it", () => {
  const observations = exactChicago();
  const fit = drift.fitPosition(drift.defaultModel(), fx.CHICAGO, observations, null)!;
  const home = fx.squareCountry("aa", fx.CHICAGO, 200);
  assert.deepEqual(
    { ...drift.decide(fit, home, observations, K99), judgement: null },
    { verdict: "agree", reason: "inside_ip_country", judgement: null },
  );
  const near = fx.squareCountry("bb", drift.fromAzimuthalKm(truthNearChicago, 260, 0), 100);
  const nearDecision = drift.decide(fit, near, observations, K99);
  assert.equal(nearDecision.reason, nearDecision.judgement!.overlaps ? "region_overlaps" : "region_outside");
  const far = fx.squareCountry("cc", fx.MADRID, 200);
  const farDecision = drift.decide(fit, far, observations, K99);
  assert.equal(farDecision.verdict, "disagree");
  assert.equal(farDecision.reason, "region_outside");
  assert.deepEqual(drift.decide(fit, null, observations, K99), { verdict: "inconclusive", reason: "ip_country_unknown", judgement: null });
  // a country with no ring that has a point is unknown too, before the precision check (review F-04)
  for (const empty of [{ code: "zz", polygons: [] }, { code: "zz", polygons: [[[]]] }, { code: "zz", polygons: [[]] }]) {
    assert.deepEqual(drift.decide(fit, empty, observations, K99), { verdict: "inconclusive", reason: "ip_country_unknown", judgement: null });
    assert.deepEqual(drift.decide(null, empty, observations, K99).reason, "ip_country_unknown");
  }
  assert.deepEqual(drift.decide(null, home, observations, K99), { verdict: "inconclusive", reason: "low_precision", judgement: null });
  assert.equal(drift.decide(fit, home, observations, K99, fit.majorKm - 1).reason, "low_precision");
  // a region wide enough to touch Spain, while the light discs keep Spain out of reach
  const wide = fitAt(fx.CHICAGO, iso(2400));
  const inconsistent = drift.decide(wide, far, observations, K99, 1e9);
  assert.equal(inconsistent.judgement!.overlaps, true);
  assert.equal(inconsistent.verdict, "inconclusive");
  assert.equal(inconsistent.reason, "inconsistent");
});

// ---- V10

const stopFit = (areaKm2: number, eastKm = 0, northKm = 0): Fit => {
  const sigma = Math.sqrt(areaKm2);
  return fitAt(drift.fromAzimuthalKm(fx.CHICAGO, eastKm, northKm), iso(sigma), fx.CHICAGO);
};
const ready = Array.from({ length: 12 }, () => 14);
/** looks every 250 ms from the sessions' opening until `untilMs` */
function looks(untilMs: number, at: (t: number) => Fit | null, roundTrips: (t: number) => number[] = () => ready) {
  const history = [];
  for (let t = 0; t <= untilMs; t += 250) {
    history.push({ atMs: 1_000_000 + t, fit: at(t), roundTrips: roundTrips(t) });
  }
  return history;
}
const OPENED = 1_000_000;
const small = fx.squareCountry("aa", fx.CHICAGO, 60);

test("V10: nothing stops a run in its first 3 s", () => {
  const history = looks(2750, () => stopFit(2500));
  assert.equal(drift.evaluateStop(history, OPENED, OPENED + 2750, OPENED + 2700, small, drift.browserStopTolerance), null);
  assert.equal(drift.stopEarliestMs, 3000);
});

test("V10: converged once the ellipse holds for 2 s with 8 extenders at 12 round trips", () => {
  const history = looks(3000, () => stopFit(2500));
  assert.equal(drift.evaluateStop(history, OPENED, OPENED + 3000, OPENED + 2990, small, drift.browserStopTolerance), "converged");
  assert.equal(drift.stopHolds("converged", history, OPENED, OPENED + 3000, OPENED + 2990, small, drift.browserStopTolerance), true);
  // too few round trips: no stop before the cap
  const sparse = looks(3000, () => stopFit(2500), () => [12, 12, 12, 12, 12, 12, 12, 11, 11, 11, 11, 11]);
  assert.equal(drift.evaluateStop(sparse, OPENED, OPENED + 3000, OPENED + 2990, small, drift.browserStopTolerance), null);
  // a fit for less than 2 s isn't steady yet
  const late = looks(3000, (t) => (t >= 1250 ? stopFit(2500) : null));
  assert.equal(drift.evaluateStop(late, OPENED, OPENED + 3000, OPENED + 2990, small, drift.browserStopTolerance), null);
  // looks after now are ignored
  const ahead = [...looks(3000, () => stopFit(2500)), { atMs: OPENED + 3250, fit: null, roundTrips: [] }];
  assert.equal(drift.evaluateStop(ahead, OPENED, OPENED + 3000, OPENED + 2990, small, drift.browserStopTolerance), "converged");
});

test("V10: the area must stay within the tolerance, the browser's ±5% and the server's ±8%", () => {
  // 13% apart over the window
  const wobbly = looks(4000, (t) => stopFit(t % 500 === 0 ? 2500 : 2825));
  assert.equal(drift.evaluateStop(wobbly, OPENED, OPENED + 4000, OPENED + 3990, small, drift.browserStopTolerance), null);
  assert.equal(drift.evaluateStop(wobbly, OPENED, OPENED + 4000, OPENED + 3990, small, drift.serverStopTolerance), "converged");
  assert.deepEqual(drift.browserStopTolerance, { areaRatio: 1.105, kEarly: 0 });
  assert.deepEqual(drift.serverStopTolerance, { areaRatio: 1.17, kEarly: 0 });
  // and the position must move less than a quarter of σ_minor (here 50 km)
  const drifting = looks(4000, (t) => stopFit(2500, t / 100));
  assert.equal(drift.evaluateStop(drifting, OPENED, OPENED + 4000, OPENED + 3990, small, drift.browserStopTolerance), null);
  const settled = looks(4000, (t) => stopFit(2500, t / 1000));
  assert.equal(drift.evaluateStop(settled, OPENED, OPENED + 4000, OPENED + 3990, small, drift.browserStopTolerance), "converged");
});

test("V10: clear when the k_early region is wholly out of the country, or wholly in it", () => {
  // at the textbook k_early, 4.0, the run's when its k is the textbook 99% k
  const textbook = { ...drift.browserStopTolerance, kEarly: drift.kEarly(K99) };
  // shrinking, so never steady
  const outside = looks(3000, (t) => stopFit(400 + t, 600, 0));
  assert.equal(drift.evaluateStop(outside, OPENED, OPENED + 3000, OPENED + 2990, small, textbook), "clear");
  const inside = looks(3000, (t) => stopFit(100 + t / 10, 0, 0));
  assert.equal(drift.evaluateStop(inside, OPENED, OPENED + 3000, OPENED + 2990, fx.squareCountry("aa", fx.CHICAGO, 400), textbook), "clear");
  // near the border it isn't clear
  const near = looks(3000, (t) => stopFit(400 + t, 75, 0));
  assert.equal(drift.evaluateStop(near, OPENED, OPENED + 3000, OPENED + 2990, small, textbook), null);
  // without the IP country's borders, never clear, nor against a country with none (review F-04)
  assert.equal(drift.evaluateStop(outside, OPENED, OPENED + 3000, OPENED + 2990, null, textbook), null);
  assert.equal(drift.evaluateStop(outside, OPENED, OPENED + 3000, OPENED + 2990, { code: "zz", polygons: [] }, textbook), null);
  assert.equal(drift.stopHolds("clear", outside, OPENED, OPENED + 3000, OPENED + 2990, { code: "zz", polygons: [[[]]] }, textbook), false);
  assert.equal(drift.stopHolds("clear", outside, OPENED, OPENED + 3000, OPENED + 2990, small, textbook), true);
  assert.equal(drift.stopHolds("converged", outside, OPENED, OPENED + 3000, OPENED + 2990, small, textbook), false);
  // the bare tolerances carry no k_early, the run's, so they allow no clear stop (review V-4)
  assert.equal(drift.evaluateStop(outside, OPENED, OPENED + 3000, OPENED + 2990, small, drift.browserStopTolerance), null);
  assert.equal(drift.stopHolds("clear", outside, OPENED, OPENED + 3000, OPENED + 2990, small, drift.serverStopTolerance), false);
});

test("V11: the calibrated k per confidence and continent, never below the textbook k", () => {
  assert.equal(drift.defaultK(0.99, "EU"), K99);
  assert.equal(drift.defaultK(0.99, "as"), K99);
  assert.equal(drift.defaultK(0.99, "NA"), 3.13);
  assert.equal(drift.defaultK(0.99, "sa"), 3.5);
  assert.equal(drift.defaultK(0.99, "AF"), 3.71);
  assert.equal(drift.defaultK(0.99, "Oc"), 7.88);
  assert.equal(drift.defaultK(0.99, "AN"), 3.49);
  assert.equal(drift.defaultK(0.99, ""), 3.49, "unknown");
  assert.equal(drift.defaultK(0.99, "XX"), 3.49, "unknown");
  assert.equal(drift.defaultK(0.99, " EU"), 3.49, "no trimming");
  assert.equal(drift.defaultK(0.95, "AF"), 2.5);
  assert.equal(drift.defaultK(0.95, "OC"), 2.79);
  assert.equal(drift.defaultK(0.95, "NA"), K95);
  assert.equal(drift.defaultK(0.95, ""), K95);
  // V11 allows 95% and 99% only; anything else is the textbook k
  assert.equal(drift.defaultK(0.999, "OC"), K999);
  assert.equal(drift.defaultK(0.9, "OC"), drift.kForConfidence(0.9));
});

test("V11: k_early scales with the run's k, and is earlyK at the textbook 99% k", () => {
  assert.equal(drift.kEarlyScale, 4.0 / 3.035);
  assert.equal(drift.kEarly(K99), drift.earlyK);
  assert.equal(drift.kEarly(2), drift.earlyK);
  close(drift.kEarly(7.88), (4.0 / 3.035) * 7.88, 1e-12);
  close(drift.kEarly(3.71), (4.0 / 3.035) * 3.71, 1e-12);
});

test("V10, V11: the clear stop judges at the run's k_early, and never without one", () => {
  // D about 5 at the last look (300 km at σ 58 km), shrinking, so never steady:
  // clear at the textbook k_early of 4.0, not at Oceania's of 10.4
  const outside = looks(3000, (t) => stopFit(400 + t, 360, 0));
  const textbook = { ...drift.browserStopTolerance, kEarly: drift.kEarly(K99) };
  const oceania = { ...drift.browserStopTolerance, kEarly: drift.kEarly(drift.defaultK(0.99, "OC")) };
  assert.equal(drift.evaluateStop(outside, OPENED, OPENED + 3000, OPENED + 2990, small, textbook), "clear");
  assert.equal(drift.evaluateStop(outside, OPENED, OPENED + 3000, OPENED + 2990, small, oceania), null);
  // without the run's k_early, no clear stop: earlyK would be easier than a
  // verdict at a k above 3.035 (review V-4)
  for (const kEarly of [undefined, 0, -1, Number.NaN]) {
    const tol = { areaRatio: 1.105, kEarly };
    assert.equal(drift.stopHolds("clear", outside, OPENED, OPENED + 3000, OPENED + 2990, small, tol), false, `k_early ${kEarly}`);
    assert.equal(drift.evaluateStop(outside, OPENED, OPENED + 3000, OPENED + 2990, small, tol), null);
  }
  // the other rules don't need one
  const steadyLooks = looks(3000, () => stopFit(2500));
  assert.equal(drift.evaluateStop(steadyLooks, OPENED, OPENED + 3000, OPENED + 2990, small, drift.browserStopTolerance), "converged");
  const unready = looks(12000, (t) => stopFit(400 + t, 360, 0), () => [20, 20, 20, 20, 20, 20, 20, 3, 3, 3, 3, 3]);
  assert.equal(drift.evaluateStop(unready, OPENED, OPENED + 12000, OPENED + 11990, small, drift.browserStopTolerance), "cap");
  assert.equal(drift.evaluateStop(unready, OPENED, OPENED + 5000, OPENED + 900, small, drift.browserStopTolerance), "interrupted");
});

test("V10: interrupted after 4 s without a pong, and the cap at 12 s", () => {
  const history = looks(5000, () => null, () => []);
  assert.equal(drift.evaluateStop(history, OPENED, OPENED + 5000, OPENED + 1000, small, drift.browserStopTolerance), "interrupted");
  assert.equal(drift.evaluateStop(history, OPENED, OPENED + 4000, 0, small, drift.browserStopTolerance), "interrupted", "no pong at all");
  assert.equal(drift.evaluateStop(history, OPENED, OPENED + 3500, 0, small, drift.browserStopTolerance), null);
  const slow = looks(12000, (t) => stopFit(400 + t), () => [20, 20, 20, 20, 20, 20, 20, 3, 3, 3, 3, 3]);
  assert.equal(drift.evaluateStop(slow, OPENED, OPENED + 11750, OPENED + 11700, small, drift.browserStopTolerance), null);
  assert.equal(drift.evaluateStop(slow, OPENED, OPENED + 12000, OPENED + 11990, small, drift.browserStopTolerance), "cap");
  assert.equal(drift.stopHolds("interrupted", slow, OPENED, OPENED + 12000, OPENED + 11990, small, drift.browserStopTolerance), false);
  assert.deepEqual(
    [drift.stopCapMs, drift.stopInterruptedMs, drift.stopWindowMs, drift.stopExtenders, drift.stopRoundTrips],
    [12000, 4000, 2000, 8, 12],
  );
});
