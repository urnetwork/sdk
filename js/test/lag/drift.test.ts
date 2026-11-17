import { test } from "node:test";
import assert from "node:assert/strict";
import { registerTsResolve } from "../ts-resolve.ts";

// The drift model (lag.dev/DRIFTTEST.md V1-V7, V10; T2), on synthetic
// geometries with known positions. Parity with the Go model is
// drift_vectors.test.ts.

registerTsResolve();
const drift = await import("../../src/lag/drift/index.ts");
const { autocorrelationTime } = await import("../../src/lag/drift/roundTrip.ts");
const { placeErrorKm, pathVariance } = await import("../../src/lag/drift/model.ts");
const { invert, invertSharedVariance, solve } = await import("../../src/lag/drift/linalg.ts");
const fx = await import("./drift_fixtures.ts");

type Fit = import("../../src/lag/drift/index.ts").Fit;
type Point = import("../../src/lag/drift/index.ts").Point;
type Cov2 = import("../../src/lag/drift/index.ts").Cov2;

const close = (actual: number, expected: number, tolerance: number, message?: string) =>
  assert.ok(Math.abs(actual - expected) <= tolerance, `${message ?? ""} ${actual} is not within ${tolerance} of ${expected}`);

/** a fit at a place with a covariance, for the geometry that reads only those */
function fitAt(at: Point, cov: Cov2, expected: Point = at): Fit {
  const offset = drift.toEastNorth(expected, at);
  return {
    at,
    eastKm: offset.eastKm,
    northKm: offset.northKm,
    lastMileMs: [5],
    runs: [1],
    cov,
    chi2PerDof: 1,
    extenders: 12,
    ...drift.ellipse(cov),
  };
}

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
});

test("the azimuthal equidistant projection keeps distance and bearing from its center, and inverts", () => {
  const rng = fx.random(3);
  for (let i = 0; i < 200; i++) {
    const origin = { lat: -80 + 160 * rng(), lon: -180 + 360 * rng() };
    const e = (rng() - 0.5) * 8000;
    const n = (rng() - 0.5) * 8000;
    const p = drift.fromEastNorth(origin, e, n);
    close(drift.distanceKm(origin, p), Math.hypot(e, n), 1e-6, "distance");
    const back = drift.toEastNorth(origin, p);
    close(back.eastKm, e, 1e-6, "east");
    close(back.northKm, n, 1e-6, "north");
    assert.ok(p.lon >= -180 && p.lon < 180, `lon ${p.lon} wrapped`);
  }
  assert.deepEqual(drift.toEastNorth(fx.CHICAGO, fx.CHICAGO), { eastKm: 0, northKm: 0 });
  assert.deepEqual(drift.fromEastNorth(fx.CHICAGO, 0, 0), fx.CHICAGO);
});

test("small dense solves, inverses and the shared-variance block inverse", () => {
  const A = [
    [4, 1, 0.5],
    [1, 3, 0.25],
    [0.5, 0.25, 2],
  ];
  const x = solve(A, [1, 2, 3])!;
  A.forEach((row, i) => close(row.reduce((s, a, j) => s + a * x[j], 0), [1, 2, 3][i], 1e-12));
  const inverse = invert(A)!;
  for (let i = 0; i < 3; i++) {
    for (let j = 0; j < 3; j++) {
      close(A[i].reduce((s, a, k) => s + a * inverse[k][j], 0), i === j ? 1 : 0, 1e-12);
    }
  }
  assert.equal(solve([[1, 2], [2, 4]], [1, 2]), null, "singular");
  // (D + τ² 11ᵀ)⁻¹ against a direct inverse
  const variances = [0.7, 1.3, 2.1];
  const tau2 = 0.9;
  const block = variances.map((v, j) => variances.map((_, k) => (j === k ? v : 0) + tau2));
  const direct = invert(block)!;
  const shared = invertSharedVariance(variances, tau2);
  direct.forEach((row, j) => row.forEach((v, k) => close(shared[j][k], v, 1e-12)));
});

// ---- V1

test("V1: the 10th percentile by nearest rank, below 4 samples none, lost pings left out", () => {
  assert.equal(drift.estimateRoundTrip([]), null);
  assert.equal(drift.estimateRoundTrip([10, 11, 12]), null);
  assert.equal(drift.estimateRoundTrip([10, Number.NaN, 11, 12]), null, "a lost ping is no sample");
  const shuffle = (n: number) => Array.from({ length: n }, (_, i) => ((i * 7) % n) + 1);
  assert.equal(drift.estimateRoundTrip(shuffle(20))!.ms, 2);
  assert.equal(drift.estimateRoundTrip(shuffle(10))!.ms, 1);
  assert.equal(drift.estimateRoundTrip(shuffle(11))!.ms, 2);
  assert.equal(drift.estimateRoundTrip([5, 6, 7, 8])!.ms, 5);
  assert.equal(drift.estimateRoundTrip([12, Number.NaN, 9, 30, 15])!.count, 4);
  const samples = [30, 10, 20, 40];
  drift.estimateRoundTrip(samples);
  assert.deepEqual(samples, [30, 10, 20, 40], "the input is not reordered");
});

test("V1: the error from order statistics at the effective sample size", () => {
  // alternating high and low: negative lag-1 autocorrelation, so τ = 1
  const alternating = (n: number) => {
    const low = Array.from({ length: Math.ceil(n / 2) }, (_, i) => i + 1);
    const high = Array.from({ length: Math.floor(n / 2) }, (_, i) => n - i);
    return low.flatMap((l, i) => (i < high.length ? [l, high[i]] : [l]));
  };
  assert.equal(autocorrelationTime(alternating(100)), 1);
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
  const ramp = drift.estimateRoundTrip(Array.from({ length: 100 }, (_, i) => i + 1))!;
  assert.equal(autocorrelationTime(Array.from({ length: 100 }, (_, i) => i + 1)), 8, "held to 8");
  assert.equal(ramp.ms, 10);
  assert.ok(ramp.sigmaMs > hundred.sigmaMs * 2, `${ramp.sigmaMs} vs ${hundred.sigmaMs}`);
  // no spread, no error
  assert.deepEqual(drift.estimateRoundTrip([7, 7, 7, 7, 7, 7]), { ms: 7, sigmaMs: 0, count: 6 });
  assert.equal(autocorrelationTime([7, 7, 7, 7]), 1);
  assert.equal(autocorrelationTime([7]), 1);
});

// ---- V2, V3, V6 constants

test("V2, V3, V6, V10: the model's constants", () => {
  assert.deepEqual(drift.defaultModel(), { kappa: 0.25, sigma0Ms: 1 });
  assert.equal(drift.speedKmPerMs, 133.24);
  assert.equal(drift.vacuumKmPerMs, 299.792458);
  assert.equal(drift.roundTripQuantile, 0.1);
  assert.equal(drift.defaultConfidence, 0.99);
  assert.equal(drift.earlyK, 4);
  close(drift.kForConfidence(0.95), 2.448, 1e-3);
  close(drift.kForConfidence(0.99), 3.035, 1e-3);
  close(drift.kForConfidence(0.999), 3.717, 1e-3);
  assert.equal(drift.kForConfidence(0.99), Math.sqrt(-2 * Math.log(0.01)));
  // s_ext is held to [10, 100] km, and an unknown one is a derived place's 50 km
  assert.equal(placeErrorKm(3), 10);
  assert.equal(placeErrorKm(40), 40);
  assert.equal(placeErrorKm(500), 100);
  assert.equal(placeErrorKm(0), 50);
  assert.equal(placeErrorKm(Number.NaN), 50);
  // τ² = (κ K d)² + (K s)²
  close(pathVariance(drift.defaultModel(), 1000, 25), (0.25 * fx.K * 1000) ** 2 + (fx.K * 25) ** 2, 1e-15);
});

// ---- V5, V7

const exactChicago = (extra: Partial<Parameters<typeof fx.exactObservations>[0]> = {}) =>
  fx.exactObservations({ truth: { lat: 41.862, lon: -87.842 }, extenders: fx.CHICAGO_EXTENDERS, lastMileMs: 7.5, ...extra });

test("V5: a single run's fit finds the place exact round trips come from", () => {
  const truth = { lat: 41.862, lon: -87.842 };
  const fit = drift.fitPosition(drift.defaultModel(), fx.CHICAGO, exactChicago(), null)!;
  assert.ok(fit);
  close(drift.distanceKm(fit.at, truth), 0, 0.01, "position, km");
  close(fit.lastMileMs[0], 7.5, 1e-3, "b");
  assert.deepEqual(fit.runs, [1]);
  assert.equal(fit.extenders, 12);
  close(fit.chi2PerDof, 0, 1e-6);
  const offset = drift.toEastNorth(fx.CHICAGO, fit.at);
  close(fit.eastKm, offset.eastKm, 1e-9);
  close(fit.northKm, offset.northKm, 1e-9);
  assert.ok(fit.majorKm >= fit.minorKm && fit.minorKm > 0);
  assert.ok(fit.bearingDeg >= 0 && fit.bearingDeg < 180);
  assert.deepEqual({ majorKm: fit.majorKm, minorKm: fit.minorKm, bearingDeg: fit.bearingDeg }, drift.ellipse(fit.cov));
  assert.equal(fit.cov[0][1], fit.cov[1][0], "symmetric");
});

test("V5: no fit from fewer than 6 extenders", () => {
  const six = exactChicago().slice(0, 6);
  assert.ok(drift.fitPosition(drift.defaultModel(), fx.CHICAGO, six, null));
  assert.equal(drift.fitPosition(drift.defaultModel(), fx.CHICAGO, six.slice(0, 5), null), null);
  assert.equal(drift.fitPosition(drift.defaultModel(), fx.CHICAGO, [], null), null);
  // one extender measured twice is still one extender
  const repeated = six.slice(0, 5).concat([{ ...six[0], run: 2 }]);
  assert.equal(drift.fitPosition(drift.defaultModel(), fx.CHICAGO, repeated, null), null);
});

test("V5: a warm start ends where a cold one does", () => {
  const observations = fx.exactObservations({
    truth: { lat: 42.4, lon: -88.3 },
    extenders: fx.CHICAGO_EXTENDERS,
    lastMileMs: 9,
    stretch: [0.1, -0.05, 0.2, 0, 0.15, -0.08, 0.3, 0.05, 0.12, -0.02, 0.07, 0.2],
  });
  const cold = drift.fitPosition(drift.defaultModel(), fx.CHICAGO, observations, null)!;
  // the last look's fit, 100 km off, with another b
  const last = { ...fitAt(drift.fromEastNorth(cold.at, 80, -60), iso(10), fx.CHICAGO), lastMileMs: [4] };
  const warm = drift.fitPosition(drift.defaultModel(), fx.CHICAGO, observations, last)!;
  close(drift.distanceKm(cold.at, warm.at), 0, 1e-3, "km apart");
  close(cold.lastMileMs[0], warm.lastMileMs[0], 1e-4);
  close(cold.cov[0][0], warm.cov[0][0], cold.cov[0][0] * 1e-4);
  // a warm start from a fit over other runs takes its b only for the runs it shares
  const otherRuns = { ...last, runs: [7], lastMileMs: [4] };
  close(drift.distanceKm(drift.fitPosition(drift.defaultModel(), fx.CHICAGO, observations, otherRuns)!.at, cold.at), 0, 1e-3);
});

test("V7: one position and one last-mile offset per run", () => {
  const truth = { lat: 41.862, lon: -87.842 };
  const run = (run: number, lastMileMs: number) =>
    fx.exactObservations({ truth, extenders: fx.CHICAGO_EXTENDERS, lastMileMs, run });
  const fit = drift.fitPosition(drift.defaultModel(), fx.CHICAGO, [...run(3, 12), ...run(1, 5)], null)!;
  assert.deepEqual(fit.runs, [1, 3]);
  close(fit.lastMileMs[0], 5, 1e-3);
  close(fit.lastMileMs[1], 12, 1e-3);
  close(drift.distanceKm(fit.at, truth), 0, 0.01);
  assert.equal(fit.extenders, 12, "the same 12 paths twice");
});

test("V5: a b at its bound of 0 is held, and the covariance is the position's alone", () => {
  // round trips shorter than the distance allows push b below 0
  const fit = drift.fitPosition(drift.defaultModel(), fx.CHICAGO, exactChicago({ lastMileMs: -2 }), null)!;
  assert.ok(fit);
  assert.equal(fit.lastMileMs[0], 0);
  assert.ok(fit.cov.every((row) => row.every(Number.isFinite)));
  assert.ok(fit.minorKm > 0);
});

test("V5: Huber weights keep one detoured path from dragging the position", () => {
  const truth = { lat: 41.862, lon: -87.842 };
  const clean = drift.fitPosition(drift.defaultModel(), fx.CHICAGO, exactChicago(), null)!;
  const detoured = exactChicago();
  // Atlanta's path detours by 40 ms, about 2,700 km of fiber; unweighted, it
  // would pull the fit about 345 km
  detoured[5] = { ...detoured[5], roundTrip: { ...detoured[5].roundTrip, ms: detoured[5].roundTrip.ms + 40 } };
  const fit = drift.fitPosition(drift.defaultModel(), fx.CHICAGO, detoured, null)!;
  const shift = drift.distanceKm(fit.at, truth);
  assert.ok(shift < 60, `the fit moved ${shift} km`);
  assert.ok(drift.distanceKm(clean.at, truth) < 0.01);
  // the residuals are larger than the model expects, so the ellipse widens
  assert.ok(fit.chi2PerDof > 1);
  assert.ok(fit.majorKm * fit.minorKm > clean.majorKm * clean.minorKm);
});

test("V5: the covariance widens with the residuals and never narrows below the model's", () => {
  const rng = fx.random(11);
  const noisy = exactChicago().map((o) => ({ ...o, roundTrip: { ...o.roundTrip, ms: o.roundTrip.ms + fx.gaussian(rng) * 6 } }));
  const clean = drift.fitPosition(drift.defaultModel(), fx.CHICAGO, exactChicago(), null)!;
  const fit = drift.fitPosition(drift.defaultModel(), fx.CHICAGO, noisy, null)!;
  assert.ok(fit.chi2PerDof > 1, `χ²/dof ${fit.chi2PerDof}`);
  assert.ok(fit.majorKm > clean.majorKm);
  // a larger measurement error alone widens the ellipse too
  const loose = drift.fitPosition(drift.defaultModel(), fx.CHICAGO, exactChicago({ sigmaMs: 5 }), null)!;
  assert.ok(loose.majorKm > clean.majorKm);
});

test("T2: a repeated path's route error isn't counted twice", () => {
  const truth = { lat: 41.862, lon: -87.842 };
  // three runs on the same 12 paths, and three runs on 36 distinct paths of the same geometry
  const shifted = (k: number) => fx.CHICAGO_EXTENDERS.map((p) => drift.fromEastNorth(p, 40 * k, -30 * k));
  const same = [1, 2, 3].flatMap((run) =>
    fx.exactObservations({ truth, extenders: fx.CHICAGO_EXTENDERS, lastMileMs: 7, run }),
  );
  const distinct = [1, 2, 3].flatMap((run) =>
    fx.exactObservations({
      truth,
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
  assert.deepEqual(drift.ellipse([[400, 0], [0, 100]]), { majorKm: 20, minorKm: 10, bearingDeg: 90 });
  assert.deepEqual(drift.ellipse([[100, 0], [0, 400]]), { majorKm: 20, minorKm: 10, bearingDeg: 0 });
  const ne = drift.ellipse([[250, 150], [150, 250]]);
  close(ne.majorKm, 20, 1e-12);
  close(ne.minorKm, 10, 1e-12);
  close(ne.bearingDeg, 45, 1e-12);
  close(drift.ellipse([[250, -150], [-150, 250]]).bearingDeg, 135, 1e-12);
});

// ---- V6

test("V6: inside the country agrees, with the region's radius toward the nearest border", () => {
  const country = fx.squareCountry("aa", fx.CHICAGO, 200);
  const j = drift.judge(fitAt(fx.CHICAGO, iso(20)), country, 0.99);
  assert.equal(j.inside, true);
  assert.equal(j.overlaps, true);
  assert.equal(j.distanceKm, 0);
  close(j.d, 200 / 20, 0.05);
  close(j.significanceKm, drift.kForConfidence(0.99) * 20, 1e-6, "isotropic: k σ every way");
  assert.equal(j.marginKm, -j.significanceKm);
});

test("V6: outside, the region overlapping the border agrees and one clear of it disagrees", () => {
  const country = fx.squareCountry("aa", fx.CHICAGO, 200);
  const at = drift.fromEastNorth(fx.CHICAGO, 300, 0);
  const clear = drift.judge(fitAt(at, iso(20)), country);
  assert.equal(clear.inside, false);
  assert.equal(clear.overlaps, false);
  close(clear.distanceKm, 100, 0.5);
  close(clear.d, 100 / 20, 0.05);
  close(clear.significanceKm, 3.0349 * 20, 1e-3);
  assert.ok(clear.marginKm > 0);
  const touching = drift.judge(fitAt(at, iso(50)), country);
  assert.equal(touching.inside, false);
  assert.equal(touching.overlaps, true);
  assert.ok(touching.marginKm < 0);
  // a lower confidence draws a smaller region
  const looser = drift.judge(fitAt(at, iso(38)), country, 0.95);
  const stricter = drift.judge(fitAt(at, iso(38)), country, 0.999);
  assert.equal(looser.overlaps, false);
  assert.equal(stricter.overlaps, true);
});

test("V6: margin > 0 exactly when D > k, and the radius is along the line to q, for tilted ellipses", () => {
  const rng = fx.random(5);
  const country = fx.squareCountry("aa", fx.MADRID, 250);
  for (let i = 0; i < 300; i++) {
    const at = drift.fromEastNorth(fx.MADRID, (rng() - 0.5) * 1400, (rng() - 0.5) * 1400);
    const a = 5 + rng() * 120;
    const b = 5 + rng() * 120;
    const theta = rng() * Math.PI;
    const c = Math.cos(theta);
    const s = Math.sin(theta);
    const cov: Cov2 = [
      [a * a * c * c + b * b * s * s, (a * a - b * b) * c * s],
      [(a * a - b * b) * c * s, a * a * s * s + b * b * c * c],
    ];
    const confidence = [0.95, 0.99, 0.999][i % 3];
    const k = drift.kForConfidence(confidence);
    const j = drift.judge(fitAt(at, cov), country, confidence);
    if (!j.inside) {
      assert.equal(j.marginKm > 0, j.d > k, `case ${i}: margin ${j.marginKm}, D ${j.d}, k ${k}`);
      assert.equal(j.overlaps, j.d <= k);
      close(j.marginKm, j.distanceKm - j.significanceKm, 1e-9);
      // D = ‖q − p̂‖ √(uᵀΣ⁻¹u), so the radius toward q is k ‖q − p̂‖ / D
      close(j.significanceKm, (k * j.distanceKm) / j.d, 1e-6 * j.distanceKm);
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
  const notch = drift.fromEastNorth(fx.MADRID, 0, 150);
  const narrow = drift.judge(fitAt(notch, iso(15)), country);
  assert.equal(narrow.inside, false);
  // the arms are 100 km away: D = 100 / 15 > k
  close(narrow.distanceKm, 100, 1);
  assert.equal(narrow.overlaps, false, "inside the hull but clear of the border");
  assert.equal(drift.judge(fitAt(notch, iso(40)), country).overlaps, true);
});

test("V6: an island inside the region counts as touching, and a hole is outside", () => {
  const far = fx.squareCountry("aa", fx.MADRID, 100);
  const at = drift.fromEastNorth(fx.MADRID, 900, 0);
  const island = fx.ringAround(drift.fromEastNorth(at, 0, 80), [
    [-3, -3],
    [3, -3],
    [3, 3],
    [-3, 3],
  ]);
  const withIsland = { code: "aa", polygons: [...far.polygons, [island]] };
  assert.equal(drift.judge(fitAt(at, iso(40)), far).overlaps, false);
  const j = drift.judge(fitAt(at, iso(40)), withIsland);
  assert.equal(j.overlaps, true);
  close(j.distanceKm, 77, 1);
  // a lake: the fit in the hole is outside the country, 50 km from its shore
  const outer = fx.densify(fx.ringAround(fx.MADRID, [[-400, -400], [400, -400], [400, 400], [-400, 400]]), 20);
  const hole = fx.densify(fx.ringAround(fx.MADRID, [[-50, -50], [50, -50], [50, 50], [-50, 50]]), 5);
  const lake = { code: "aa", polygons: [[outer, hole]] };
  const inHole = drift.judge(fitAt(fx.MADRID, iso(5)), lake);
  assert.equal(inHole.inside, false);
  close(inHole.distanceKm, 50, 1);
  assert.equal(inHole.overlaps, false);
  assert.equal(drift.judge(fitAt(drift.fromEastNorth(fx.MADRID, 200, 0), iso(5)), lake).inside, true);
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
  assert.equal(drift.judge(fitAt({ lat: -17, lon: 179.9 }, iso(10)), fiji).inside, true);
  assert.equal(drift.judge(fitAt({ lat: -17, lon: -179.5 }, iso(10)), fiji).inside, true);
  const off = drift.judge(fitAt({ lat: -17.5, lon: 170 }, iso(10)), fiji);
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
  assert.equal(drift.judge(fitAt({ lat: -85, lon: 30 }, iso(10)), aq).inside, true);
  assert.equal(drift.judge(fitAt({ lat: -89.5, lon: -120 }, iso(10)), aq).inside, true);
  const north = drift.judge(fitAt({ lat: -75, lon: 0 }, iso(10)), aq);
  assert.equal(north.inside, false);
  close(north.distanceKm, drift.distanceKm({ lat: -75, lon: 0 }, { lat: -80, lon: 0 }), 1);
});

test("V6: the antipode of a country doesn't put the fit in it", () => {
  // Buenos Aires' antipode is in eastern China; the fit is not in a country around it
  const buenosAires = { lat: -34.6, lon: -58.4 };
  const antipode = { lat: 34.6, lon: 121.6 };
  const china = fx.squareCountry("cn", antipode, 500);
  const j = drift.judge(fitAt(buenosAires, iso(50)), china);
  assert.equal(j.inside, false);
  assert.equal(j.overlaps, false);
  assert.ok(j.distanceKm > 15000);
});

test("V8: the region's ± and its outline at the confidence", () => {
  const cov: Cov2 = [
    [900, 300],
    [300, 400],
  ];
  const fit = fitAt(fx.MADRID, cov);
  close(drift.regionRadiusKm(fit, 0.99), drift.kForConfidence(0.99) * Math.sqrt(fit.majorKm * fit.minorKm), 1e-12);
  const outline = drift.regionOutline(fit, 0.99, 48);
  assert.equal(outline.length, 48);
  const det = cov[0][0] * cov[1][1] - cov[0][1] * cov[1][0];
  for (const p of outline) {
    const { eastKm: e, northKm: n } = drift.toEastNorth(fit.at, p);
    const m2 = (e * e * cov[1][1] - 2 * e * n * cov[0][1] + n * n * cov[0][0]) / det;
    close(Math.sqrt(m2), drift.kForConfidence(0.99), 1e-6);
  }
});

// ---- V5's physical check

test("V5: the physical check passes a consistent run and fails a country out of light's reach", () => {
  const observations = exactChicago();
  assert.equal(drift.physicallyPossible(observations, fx.squareCountry("aa", fx.CHICAGO, 200)), true);
  assert.equal(drift.physicallyPossible(observations, fx.squareCountry("bb", fx.MADRID, 200)), false);
  // a country just beyond the near discs' reach
  assert.equal(drift.physicallyPossible(observations, fx.squareCountry("cc", { lat: 20, lon: -155 }, 100)), false);
  assert.equal(drift.physicallyPossible([], fx.squareCountry("aa", fx.CHICAGO, 200)), true, "nothing rules it out");
  assert.equal(drift.physicallyPossible(observations, { code: "zz", polygons: [] }), false);
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

test("V5: the check finds a country inside the discs' intersection without touching a border", () => {
  // a large country holding every disc's crossing: no border edge is in reach of the near discs
  const observations = exactChicago();
  const big = fx.squareCountry("aa", fx.CHICAGO, 3000);
  assert.equal(drift.physicallyPossible(observations, big), true);
});

// ---- V10

const stopFit = (areaKm2: number, eastKm = 0, northKm = 0): Fit => {
  const sigma = Math.sqrt(areaKm2);
  return fitAt(drift.fromEastNorth(fx.CHICAGO, eastKm, northKm), iso(sigma), fx.CHICAGO);
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
const big = fx.squareCountry("aa", fx.CHICAGO, 60);

test("V10: nothing stops a run in its first 3 s", () => {
  const history = looks(2750, () => stopFit(2500));
  assert.equal(drift.evaluateStop(history, OPENED, OPENED + 2750, OPENED + 2700, big, drift.browserStopTolerance), null);
});

test("V10: converged once the ellipse holds for 2 s with 8 extenders at 12 round trips", () => {
  const history = looks(3000, () => stopFit(2500));
  assert.equal(drift.evaluateStop(history, OPENED, OPENED + 3000, OPENED + 2990, big), "converged");
  // too few round trips: no stop before the cap
  const sparse = looks(3000, () => stopFit(2500), () => [12, 12, 12, 12, 12, 12, 12, 11, 11, 11, 11, 11]);
  assert.equal(drift.evaluateStop(sparse, OPENED, OPENED + 3000, OPENED + 2990, big), null);
  // a fit for less than 2 s isn't steady yet
  const late = looks(3000, (t) => (t >= 1250 ? stopFit(2500) : null));
  assert.equal(drift.evaluateStop(late, OPENED, OPENED + 3000, OPENED + 2990, big), null);
});

test("V10: the area must stay within the tolerance, the browser's ±5% and the server's ±8%", () => {
  // 13% apart over the window
  const wobbly = looks(4000, (t) => stopFit(t % 500 === 0 ? 2500 : 2825));
  assert.equal(drift.evaluateStop(wobbly, OPENED, OPENED + 4000, OPENED + 3990, big, drift.browserStopTolerance), null);
  assert.equal(drift.evaluateStop(wobbly, OPENED, OPENED + 4000, OPENED + 3990, big, drift.serverStopTolerance), "converged");
  assert.deepEqual(drift.browserStopTolerance, { areaRatio: 1.105 });
  assert.deepEqual(drift.serverStopTolerance, { areaRatio: 1.17 });
  // and the position must move less than a quarter of σ_minor (here 50 km)
  const drifting = looks(4000, (t) => stopFit(2500, t / 100));
  assert.equal(drift.evaluateStop(drifting, OPENED, OPENED + 4000, OPENED + 3990, big), null);
  const settled = looks(4000, (t) => stopFit(2500, t / 1000));
  assert.equal(drift.evaluateStop(settled, OPENED, OPENED + 4000, OPENED + 3990, big), "converged");
});

test("V10: clear when the k_early region is wholly out of the country, or wholly in it", () => {
  // shrinking, so never steady
  const outside = looks(3000, (t) => stopFit(400 + t, 600, 0));
  assert.equal(drift.evaluateStop(outside, OPENED, OPENED + 3000, OPENED + 2990, big), "clear");
  const inside = looks(3000, (t) => stopFit(100 + t / 10, 0, 0));
  assert.equal(drift.evaluateStop(inside, OPENED, OPENED + 3000, OPENED + 2990, fx.squareCountry("aa", fx.CHICAGO, 400)), "clear");
  // near the border it isn't clear
  const near = looks(3000, (t) => stopFit(400 + t, 75, 0));
  assert.equal(drift.evaluateStop(near, OPENED, OPENED + 3000, OPENED + 2990, big), null);
  // without the IP country's borders, never clear
  assert.equal(drift.evaluateStop(outside, OPENED, OPENED + 3000, OPENED + 2990, null), null);
});

test("V10: interrupted after 4 s without a pong, and the cap at 12 s", () => {
  const history = looks(5000, () => null, () => []);
  assert.equal(drift.evaluateStop(history, OPENED, OPENED + 5000, OPENED + 1000, big), "interrupted");
  assert.equal(drift.evaluateStop(history, OPENED, OPENED + 4000, 0, big), "interrupted", "no pong at all");
  assert.equal(drift.evaluateStop(history, OPENED, OPENED + 3500, 0, big), null);
  const slow = looks(12000, (t) => stopFit(400 + t), () => [20, 20, 20, 20, 20, 20, 20, 3, 3, 3, 3, 3]);
  assert.equal(drift.evaluateStop(slow, OPENED, OPENED + 11750, OPENED + 11700, big), null);
  assert.equal(drift.evaluateStop(slow, OPENED, OPENED + 12000, OPENED + 11990, big), "cap");
  assert.equal(drift.stopCapMs, 12000);
});
