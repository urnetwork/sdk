// V5 and V7: where the round trips place the visitor (lag.dev/DRIFTTEST.md).
//
// One position p for every run, one last-mile offset b_r ≥ 0 per run, and a
// route and place error uᵢ ~ N(0, τᵢ²) shared by every run that used extender
// i, so
//
//   y_{r,i} = b_r + K · d(p, eᵢ) + uᵢ + ε_{r,i},  ε_{r,i} ~ N(0, σ_meas,r,i² + σ₀²).
//
// Generalized least squares minimizes rᵀV⁻¹r, with V block-diagonal by
// extender: each block is the diagonal of its runs' ε variances plus τᵢ²
// everywhere. Huber weights (c = 2) apply per extender, on its block's
// standardized residual, so a detoured path is discounted in every run at
// once. A single run is the same fit with one b and blocks of one.
//
// The solver is Gauss–Newton on east and north km about the current position
// (its azimuthal equidistant projection) and the b's, with a little
// Levenberg–Marquardt damping, warm starts and a step cap (PLAN.md L18). The
// variances depend on p through dᵢ, so they are recomputed at every
// iteration.

import { DEG, fromEastNorth, toEastNorth, bearingRad, distanceKm, type Point } from "./geo";
import { invert, invertSharedVariance, solve } from "./linalg";
import { measurementVariance, msPerKm, pathVariance, type Model, type Observation } from "./model";

/** No fit is made from fewer extenders (PLAN.md L18, DRIFTTEST.md E5). */
export const minFitExtenders = 6;

/** V5: Huber's c, on the normalized residuals. */
const HUBER_C = 2;
/** a step moves the position at most this far (L18), km */
const MAX_STEP_KM = 400;
/** the fit has settled once a step moves the position less than this, km ... */
const SETTLED_KM = 1e-4;
/** ... and every b less than this, ms */
const SETTLED_MS = 1e-5;
const WARM_ITERATIONS = 60;
const COLD_ITERATIONS = 100;
/** Levenberg–Marquardt: the diagonal is scaled by 1 + this */
const DAMPING = 1e-3;

/** A 2 × 2 covariance of east and north, km². */
export type Cov2 = [[number, number], [number, number]];

/** The 1σ ellipse of a position covariance. */
export interface Ellipse {
  majorKm: number;
  minorKm: number;
  /** the major axis, degrees clockwise from north, in [0, 180) */
  bearingDeg: number;
}

/** V5, V7: one fit over one or more runs. */
export interface Fit extends Ellipse {
  /** the fitted position */
  at: Point;
  /** the fitted position from the expected place, km (azimuthal equidistant) */
  eastKm: number;
  northKm: number;
  /** b per run, ms, in the order of `runs` */
  lastMileMs: number[];
  /** the runs in the fit, ascending */
  runs: number[];
  /** the position's covariance, east and north at `at`, km² */
  cov: Cov2;
  chi2PerDof: number;
  /** distinct extenders in the fit */
  extenders: number;
}

/**
 * The 1σ ellipse of a position covariance: the square roots of its
 * eigenvalues, and its major eigenvector's bearing.
 */
export function ellipse(cov: Cov2): Ellipse {
  const a = cov[0][0];
  const b = cov[0][1];
  const c = cov[1][1];
  const mean = (a + c) / 2;
  const r = Math.hypot((a - c) / 2, b);
  // the major axis from east, counterclockwise
  const theta = Math.atan2(2 * b, a - c) / 2;
  let bearing = 90 - theta * DEG;
  bearing = ((bearing % 180) + 180) % 180;
  return { majorKm: Math.sqrt(Math.max(0, mean + r)), minorKm: Math.sqrt(Math.max(0, mean - r)), bearingDeg: bearing };
}

interface Block {
  at: Point;
  placeErrorKm: number;
  /** the run index of each of its observations */
  runs: number[];
  ys: number[];
  /** σ_meas² + σ₀² of each */
  variances: number[];
}

interface Normal {
  /** JᵀWJ and JᵀWr, over (east, north, b₀ … b_R−1) */
  A: number[][];
  g: number[];
  /** Σ hᵢ rᵢᵀVᵢ⁻¹rᵢ */
  chi2: number;
}

function normalEquations(model: Model, blocks: readonly Block[], p: Point, b: readonly number[]): Normal {
  const P = 2 + b.length;
  const A = Array.from({ length: P }, () => new Array<number>(P).fill(0));
  const g = new Array<number>(P).fill(0);
  let chi2 = 0;
  for (const block of blocks) {
    const d = distanceKm(p, block.at);
    const beta = bearingRad(p, block.at);
    // the slope of K · d(p, e) in east and north km
    const de = -Math.sin(beta) * msPerKm;
    const dn = -Math.cos(beta) * msPerKm;
    const Vinv = invertSharedVariance(block.variances, pathVariance(model, d, block.placeErrorKm));
    const r = block.ys.map((y, j) => y - (b[block.runs[j]] + msPerKm * d));
    let z2 = 0;
    for (let j = 0; j < r.length; j++) {
      for (let k = 0; k < r.length; k++) {
        z2 += r[j] * Vinv[j][k] * r[k];
      }
    }
    const z = Math.sqrt(z2);
    const h = z <= HUBER_C ? 1 : HUBER_C / z;
    chi2 += h * z2;
    for (let j = 0; j < r.length; j++) {
      for (let k = 0; k < r.length; k++) {
        const w = h * Vinv[j][k];
        if (w === 0) {
          continue;
        }
        // J_j = (de, dn, e_{run j}), and the same for k
        const rj = 2 + block.runs[j];
        const rk = 2 + block.runs[k];
        A[0][0] += w * de * de;
        A[0][1] += w * de * dn;
        A[1][0] += w * dn * de;
        A[1][1] += w * dn * dn;
        A[0][rk] += w * de;
        A[1][rk] += w * dn;
        A[rj][0] += w * de;
        A[rj][1] += w * dn;
        A[rj][rk] += w;
        g[0] += w * de * r[k];
        g[1] += w * dn * r[k];
        g[rj] += w * r[k];
      }
    }
  }
  return { A, g, chi2 };
}

/** the parameters a step may move: east, north and each b not held at its bound */
function solveStep(A: number[][], g: number[], free: readonly boolean[]): number[] | null {
  const index = free.map((f, i) => (f ? i : -1)).filter((i) => i >= 0);
  const Af = index.map((i) => index.map((j) => (i === j ? A[i][j] * (1 + DAMPING) : A[i][j])));
  const step = solve(
    Af,
    index.map((i) => g[i]),
  );
  if (step === null) {
    return null;
  }
  const full = new Array<number>(free.length).fill(0);
  index.forEach((i, k) => {
    full[i] = step[k];
  });
  return full;
}

/**
 * V5, V7: the position and per-run offsets the observations fit best, with
 * their covariance. `expected` is the place the IP address gives: the cold
 * start, and the origin of `eastKm` and `northKm`. `warm` is the last fit,
 * to start from. `null` with fewer than 6 extenders, or when the geometry
 * can't place the visitor at all.
 */
export function fitPosition(
  model: Model,
  expected: Point,
  observations: readonly Observation[],
  warm?: Fit | null,
): Fit | null {
  const usable = observations.filter(
    (o) =>
      Number.isFinite(o.roundTrip.ms) &&
      Number.isFinite(o.roundTrip.sigmaMs) &&
      Number.isFinite(o.at.lat) &&
      Number.isFinite(o.at.lon) &&
      Number.isFinite(o.run),
  );
  const runs = [...new Set(usable.map((o) => o.run))].sort((a, b) => a - b);
  const runIndex = new Map(runs.map((run, i) => [run, i]));
  const byExtender = new Map<string, Block>();
  for (const o of usable) {
    let block = byExtender.get(o.extenderId);
    if (block === undefined) {
      block = { at: o.at, placeErrorKm: o.placeErrorKm, runs: [], ys: [], variances: [] };
      byExtender.set(o.extenderId, block);
    }
    block.runs.push(runIndex.get(o.run)!);
    block.ys.push(o.roundTrip.ms);
    block.variances.push(measurementVariance(model, o.roundTrip));
  }
  const blocks = [...byExtender.values()];
  const n = usable.length;
  const R = runs.length;
  if (blocks.length < minFitExtenders || n < 3 + R) {
    return null;
  }

  // a cold start is at the expected place, with each b at 0.7 times its run's lowest round trip
  let p: Point = warm ? { lat: warm.at.lat, lon: warm.at.lon } : { lat: expected.lat, lon: expected.lon };
  const b = runs.map((run, r) => {
    const w = warm ? warm.lastMileMs[warm.runs.indexOf(run)] : undefined;
    if (w !== undefined && Number.isFinite(w)) {
      return Math.max(0, w);
    }
    let lowest = Infinity;
    for (const o of usable) {
      if (runIndex.get(o.run) === r) {
        lowest = Math.min(lowest, o.roundTrip.ms);
      }
    }
    return Math.max(0, 0.7 * lowest);
  });

  const maxIterations = warm ? WARM_ITERATIONS : COLD_ITERATIONS;
  let normal = normalEquations(model, blocks, p, b);
  for (let iteration = 0; iteration < maxIterations; iteration++) {
    // a b at its bound that the step would push below it is held there
    const free = [true, true, ...b.map(() => true)];
    let step = solveStep(normal.A, normal.g, free);
    for (let held = true; step !== null && held; ) {
      held = false;
      for (let r = 0; r < R; r++) {
        if (free[2 + r] && b[r] <= 0 && step[2 + r] < 0) {
          free[2 + r] = false;
          held = true;
        }
      }
      if (held) {
        step = solveStep(normal.A, normal.g, free);
      }
    }
    if (step === null) {
      return null;
    }
    const length = Math.hypot(step[0], step[1]);
    const scale = length > MAX_STEP_KM ? MAX_STEP_KM / length : 1;
    p = fromEastNorth(p, step[0] * scale, step[1] * scale);
    let moved = length * scale;
    let movedMs = 0;
    for (let r = 0; r < R; r++) {
      const next = Math.max(0, b[r] + step[2 + r] * scale);
      movedMs = Math.max(movedMs, Math.abs(next - b[r]));
      b[r] = next;
    }
    normal = normalEquations(model, blocks, p, b);
    if (moved < SETTLED_KM && movedMs < SETTLED_MS) {
      break;
    }
  }

  // V5: a b at its bound of 0 is held fixed, and the position covariance is
  // the 2 × 2 of the rest
  const free = [0, 1, ...b.map((value, r) => (value > 0 ? 2 + r : -1)).filter((i) => i >= 0)];
  const inverse = invert(free.map((i) => free.map((j) => normal.A[i][j])));
  if (inverse === null) {
    return null;
  }
  const dof = n - 2 - R;
  const chi2PerDof = normal.chi2 / Math.max(1, dof);
  const factor = Math.max(1, chi2PerDof);
  const cov: Cov2 = [
    [inverse[0][0] * factor, inverse[0][1] * factor],
    [inverse[1][0] * factor, inverse[1][1] * factor],
  ];
  if (!cov.every((row) => row.every(Number.isFinite)) || !(cov[0][0] > 0 && cov[1][1] > 0)) {
    return null;
  }
  // symmetric to the last bit, so the ellipse and the verdict see one matrix
  cov[0][1] = cov[1][0] = (cov[0][1] + cov[1][0]) / 2;
  const offset = toEastNorth(expected, p);
  return {
    at: p,
    eastKm: offset.eastKm,
    northKm: offset.northKm,
    lastMileMs: b.slice(),
    runs,
    cov,
    chi2PerDof,
    extenders: blocks.length,
    ...ellipse(cov),
  };
}
