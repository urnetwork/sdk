// The fit (lag.dev/DRIFTTEST.md V2, V5, V7), as connect's lagdrift computes it
// (fit.go): one position p for every run, a last-mile offset b_r ≥ 0 per run,
// and a route and place error uᵢ shared by every run that used extender i:
//
//   y_{r,i} = b_r + K · d(p, eᵢ) + uᵢ + ε_{r,i}
//
// uᵢ ~ N(0, τᵢ²) and ε_{r,i} ~ N(0, σ_meas,r,i² + σ₀²) (V3). Generalized least
// squares minimizes rᵀV⁻¹r, where V is block-diagonal by extender: each block
// is the diagonal of its runs' ε variances, D, plus τᵢ² everywhere. A block is
// D + t tᵀ with t_j the τ of its j-th row, so Sherman–Morrison inverts it in
// closed form, whatever its size:
//
//   V⁻¹ = D⁻¹ − D⁻¹ t tᵀ D⁻¹ / (1 + Q),  Q = tᵀ D⁻¹ t
//
// Huber weights (c = 2) apply per extender, to the standardized residual of
// what its runs share: z = tᵀD⁻¹r / √(Q (1 + Q)), which is N(0, 1) under the
// model for a block of any size and is r / σ for a single run (V5). A detoured
// path moves every run's residual the same way, so it is discounted in every
// run at once (V7).
//
// The solver is driftFit.ts's: Gauss–Newton on local east and north km and the
// offsets, with the diagonal damped by 1.001 (Levenberg–Marquardt), the
// position step capped at 400 km, warm-started from the last fit, and settled
// once a step moves the position less than 0.05 km and every offset less than
// 0.005 ms, or after 12 iterations from a warm start and 30 from a cold one.
// The variances depend on p, so they and the Huber weights are recomputed at
// every iteration. An offset at its bound of 0 whose step would go below it is
// held there for that step.
//
// The covariance of (east, north, b) is (JᵀWJ)⁻¹ × max(1, χ²/(n − 2 − R)),
// with W the Huber-weighted V⁻¹ at the solution and χ² = Σ h rᵀV⁻¹r. An offset
// at 0 is held fixed, and the position's covariance is the 2 × 2 of the rest.
//
// Sums run in the Go's order: blocks in the order their extenders first
// appear, and rows in a block in the observations' order.

import { azimuthalKm, bearingDeg, degPerRad, distanceKm, moveKm, pointValid, radPerDeg, type Point } from "./geo";
import { heldPlaceErrorKm, msPerKm, noiseVar, pathSigmaMs, type Model, type Observation } from "./model";

/** The Huber weights' c (V5). */
export const huberC = 2;
/** The largest position step, km (driftFit.ts). */
export const maxStepKm = 400;
/** Fewer extenders can't place anyone. A run's minimum of 6 is the caller's. */
export const minFitExtenders = 3;

const settledKm = 0.05;
const settledMs = 0.005;
const warmIterations = 12;
const coldIterations = 30;
const damping = 1.001;
// a cold start's offset, as a share of the run's lowest round trip
const coldLastMileShare = 0.7;
// a pivot this small is singular
const minPivot = 1e-20;

/** A 2 × 2 covariance of east and north, km². */
export type Cov2 = [[number, number], [number, number]];

/** The 1σ ellipse of a position covariance. */
export interface Ellipse {
  majorKm: number;
  minorKm: number;
  /** the major axis, degrees clockwise from north in [0, 180) */
  bearingDeg: number;
}

/** A fit of one or more runs (V5, V7). */
export interface Fit extends Ellipse {
  /** the fitted position */
  at: Point;
  /** the position's offset from the expected place, km, in the azimuthal equidistant projection centered on it */
  eastKm: number;
  northKm: number;
  /** b per run, ms, in the order of `runs` */
  lastMileMs: number[];
  /** the position's covariance: east and north at the fit, km² */
  cov: Cov2;
  /** χ² / (n − 2 − R) */
  chi2PerDof: number;
  /** distinct extenders in the fit */
  extenders: number;
  /** an extension of I3: the run numbers, ascending, that `lastMileMs` follows */
  runs: number[];
  /** an extension of I3: the observations in the fit */
  observations: number;
}

interface FitRow {
  at: Point;
  y: number;
  /** σ_meas² + σ₀² */
  noiseVar: number;
  /** s_ext, held */
  placeKm: number;
  /** the run's index in the fit's runs */
  run: number;
  /** the observation's run number, until the runs are indexed */
  runNumber: number;
}

interface FitBlock {
  extenderId: string;
  rows: FitRow[];
}

function usableObservation(o: Observation): boolean {
  if (!pointValid(o.at)) {
    return false;
  }
  if (!(0 < o.roundTrip.ms) || o.roundTrip.ms === Infinity) {
    return false;
  }
  if (!(0 <= o.roundTrip.sigmaMs) || o.roundTrip.sigmaMs === Infinity) {
    return false;
  }
  return true;
}

/** The warm fit's offset for a run, if it has one. */
function warmLastMile(warm: Fit, run: number, index: number, runCount: number): number | undefined {
  const runs = warm.runs ?? [];
  if (runs.length === warm.lastMileMs.length && 0 < runs.length) {
    const i = runs.indexOf(run);
    return 0 <= i ? warm.lastMileMs[i] : undefined;
  }
  if (runs.length === 0 && warm.lastMileMs.length === runCount) {
    return warm.lastMileMs[index];
  }
  return undefined;
}

/**
 * Fits the position of the observations, one or more runs, from `expected`
 * (V2, V5, V7). An observation with a round trip that is not a positive
 * finite number, a negative or non-finite standard error, or an invalid
 * place is left out. It needs 3 distinct extenders and 3 + R observations for
 * R runs, else `null`; the 6-extender minimum of a run is the caller's. `null`
 * too for an invalid `expected` or a singular fit.
 *
 * `warm`, when given, starts the fit from its position and its offsets,
 * matched by run number through `warm.runs` (by index when it has no runs and
 * as many offsets as the observations have runs). A run it lacks starts, as a
 * cold fit's runs do, at 0.7 times the run's lowest round trip, and a cold
 * fit starts at `expected`.
 */
export function fitPosition(model: Model, expected: Point, observations: readonly Observation[], warm?: Fit | null): Fit | null {
  if (!pointValid(expected)) {
    return null;
  }
  const blocks: FitBlock[] = [];
  const extenderBlocks = new Map<string, FitBlock>();
  const runNumbers: number[] = [];
  for (const o of observations) {
    if (!usableObservation(o)) {
      continue;
    }
    let block = extenderBlocks.get(o.extenderId);
    if (block === undefined) {
      block = { extenderId: o.extenderId, rows: [] };
      extenderBlocks.set(o.extenderId, block);
      blocks.push(block);
    }
    block.rows.push({
      at: o.at,
      y: o.roundTrip.ms,
      noiseVar: noiseVar(model, o.roundTrip.sigmaMs),
      placeKm: heldPlaceErrorKm(o.placeErrorKm),
      run: 0,
      runNumber: o.run,
    });
    if (!runNumbers.includes(o.run)) {
      runNumbers.push(o.run);
    }
  }
  runNumbers.sort((a, b) => a - b);
  const runCount = runNumbers.length;
  let n = 0;
  for (const block of blocks) {
    for (const row of block.rows) {
      row.run = runNumbers.indexOf(row.runNumber);
    }
    n += block.rows.length;
  }
  if (blocks.length < minFitExtenders || n < 3 + runCount) {
    return null;
  }
  const paramCount = 2 + runCount;

  const coldLastMile = new Array<number>(runCount).fill(Infinity);
  for (const block of blocks) {
    for (const row of block.rows) {
      coldLastMile[row.run] = Math.min(coldLastMile[row.run], row.y);
    }
  }
  for (let r = 0; r < runCount; r++) {
    coldLastMile[r] *= coldLastMileShare;
  }

  let p: Point = expected;
  const b = coldLastMile.slice();
  let maxIterations = coldIterations;
  if (warm && pointValid(warm.at)) {
    p = warm.at;
    maxIterations = warmIterations;
    runNumbers.forEach((run, r) => {
      const lastMile = warmLastMile(warm, run, r, runCount);
      if (lastMile !== undefined) {
        b[r] = Math.max(0, lastMile);
      }
    });
  }

  let normal: Normal;
  let settled = false;
  for (let iteration = 0; ; iteration++) {
    normal = normalEquations(model, blocks, p, b, paramCount);
    if (settled || maxIterations <= iteration) {
      break;
    }
    const step = constrainedStep(normal.a, normal.g, b);
    if (step === null) {
      break;
    }
    const length = Math.hypot(step[0], step[1]);
    let scale = 1.0;
    if (maxStepKm < length) {
      scale = maxStepKm / length;
    }
    p = moveKm(p, step[0] * scale, step[1] * scale);
    settled = length * scale < settledKm;
    for (let r = 0; r < b.length; r++) {
      const next = Math.max(0, b[r] + step[2 + r] * scale);
      settled = settled && Math.abs(next - b[r]) < settledMs;
      b[r] = next;
    }
  }

  // an offset at its bound is held fixed: the covariance is the rest's
  const free = new Array<boolean>(paramCount).fill(false);
  free[0] = true;
  free[1] = true;
  for (let r = 0; r < b.length; r++) {
    free[2 + r] = 0 < b[r];
  }
  const inverse = invertFree(normal!.a, free);
  if (inverse === null) {
    return null;
  }
  const dof = n - 2 - runCount;
  const chi2PerDof = normal!.chi2 / Math.max(1, dof);
  const factor = Math.max(1, chi2PerDof);
  const cov: Cov2 = [
    [inverse[0][0] * factor, inverse[0][1] * factor],
    [inverse[1][0] * factor, inverse[1][1] * factor],
  ];
  for (const row of cov) {
    for (const v of row) {
      if (!Number.isFinite(v)) {
        return null;
      }
    }
  }
  if (!(0 < cov[0][0]) || !(0 < cov[1][1])) {
    return null;
  }
  const { eastKm, northKm } = azimuthalKm(expected, p);
  return {
    at: p,
    eastKm,
    northKm,
    lastMileMs: b,
    cov,
    chi2PerDof,
    extenders: blocks.length,
    ...covEllipse(cov),
    runs: runNumbers,
    observations: n,
  };
}

interface Normal {
  a: number[][];
  g: number[];
  chi2: number;
}

interface RowTerms {
  jEast: number;
  jNorth: number;
  run: number;
  r: number;
  /** the row's noise variance, D's entry */
  d: number;
  /** τ */
  t: number;
}

/**
 * JᵀWJ, JᵀWr and χ² = Σ h rᵀV⁻¹r at (p, b), with W the Huber-weighted block
 * inverse. The parameters are east km, north km, then b per run; J is the
 * model's slope, so a step solves (JᵀWJ) δ = JᵀWr.
 */
function normalEquations(model: Model, blocks: readonly FitBlock[], p: Point, b: readonly number[], paramCount: number): Normal {
  const a = Array.from({ length: paramCount }, () => new Array<number>(paramCount).fill(0));
  const g = new Array<number>(paramCount).fill(0);
  let chi2 = 0.0;
  const alpha = new Array<number>(paramCount).fill(0);
  for (const block of blocks) {
    const terms: RowTerms[] = [];
    alpha.fill(0);
    // Q = tᵀD⁻¹t, ρ = tᵀD⁻¹r, α = JᵀD⁻¹t
    let q = 0.0;
    let rho = 0.0;
    for (const row of block.rows) {
      const d = distanceKm(p, row.at);
      const beta = bearingDeg(p, row.at) * radPerDeg;
      const term: RowTerms = {
        jEast: -Math.sin(beta) * msPerKm,
        jNorth: -Math.cos(beta) * msPerKm,
        run: row.run,
        r: row.y - (b[row.run] + msPerKm * d),
        d: row.noiseVar,
        t: pathSigmaMs(model, d, row.placeKm),
      };
      terms.push(term);
      q += (term.t * term.t) / term.d;
      rho += (term.t * term.r) / term.d;
      alpha[0] += (term.jEast * term.t) / term.d;
      alpha[1] += (term.jNorth * term.t) / term.d;
      alpha[2 + term.run] += term.t / term.d;
    }
    let h = 1.0;
    if (0 < q) {
      const z = Math.abs(rho / Math.sqrt(q * (1 + q)));
      if (huberC < z) {
        h = huberC / z;
      }
    }
    for (const term of terms) {
      const index = [0, 1, 2 + term.run];
      const j = [term.jEast, term.jNorth, 1];
      for (let u = 0; u < 3; u++) {
        g[index[u]] += (h * j[u] * term.r) / term.d;
        for (let v = 0; v < 3; v++) {
          a[index[u]][index[v]] += (h * j[u] * j[v]) / term.d;
        }
      }
      chi2 += (h * term.r * term.r) / term.d;
    }
    const shrink = 1 / (1 + q);
    for (let u = 0; u < paramCount; u++) {
      if (alpha[u] === 0) {
        continue;
      }
      g[u] -= h * alpha[u] * rho * shrink;
      for (let v = 0; v < paramCount; v++) {
        a[u][v] -= h * alpha[u] * alpha[v] * shrink;
      }
    }
    chi2 -= h * rho * rho * shrink;
  }
  return { a, g, chi2 };
}

/** The damped Gauss–Newton step, with each offset at its bound of 0 whose step would go below it held there. */
function constrainedStep(a: number[][], g: number[], b: readonly number[]): number[] | null {
  const free = new Array<boolean>(g.length).fill(true);
  for (;;) {
    const step = solveFree(a, g, free, damping);
    if (step === null) {
      return null;
    }
    let held = false;
    for (let r = 0; r < b.length; r++) {
      if (free[2 + r] && b[r] <= 0 && step[2 + r] < 0) {
        free[2 + r] = false;
        held = true;
      }
    }
    if (!held) {
      return step;
    }
  }
}

/**
 * Solves the system over the free parameters, with their diagonal multiplied
 * by `diagonalFactor`; a held parameter's step is 0.
 */
function solveFree(a: number[][], g: number[], free: readonly boolean[], diagonalFactor: number): number[] | null {
  const indexes: number[] = [];
  free.forEach((isFree, i) => {
    if (isFree) {
      indexes.push(i);
    }
  });
  const k = indexes.length;
  const augmented = indexes.map((i) => {
    const row = new Array<number>(k + 1);
    indexes.forEach((j, col) => {
      let v = a[i][j];
      if (i === j) {
        v *= diagonalFactor;
      }
      row[col] = v;
    });
    row[k] = g[i];
    return row;
  });
  const solution = gaussSolve(augmented);
  if (solution === null) {
    return null;
  }
  const step = new Array<number>(g.length).fill(0);
  indexes.forEach((i, row) => {
    step[i] = solution[row];
  });
  return step;
}

/** Solves the augmented k × (k + 1) system in place, by Gauss–Jordan elimination with partial pivoting. */
export function gaussSolve(m: number[][]): number[] | null {
  const k = m.length;
  for (let c = 0; c < k; c++) {
    let p = c;
    for (let r = c + 1; r < k; r++) {
      if (Math.abs(m[p][c]) < Math.abs(m[r][c])) {
        p = r;
      }
    }
    if (!(minPivot < Math.abs(m[p][c]))) {
      return null;
    }
    const swap = m[c];
    m[c] = m[p];
    m[p] = swap;
    for (let r = 0; r < k; r++) {
      if (r === c) {
        continue;
      }
      const f = m[r][c] / m[c][c];
      for (let j = c; j <= k; j++) {
        m[r][j] -= f * m[c][j];
      }
    }
  }
  const solution = new Array<number>(k);
  for (let i = 0; i < k; i++) {
    solution[i] = m[i][k] / m[i][i];
  }
  return solution;
}

/** The inverse of a over the free parameters, as a full-size matrix whose held rows and columns are 0. */
function invertFree(a: number[][], free: readonly boolean[]): number[][] | null {
  const paramCount = a.length;
  const inverse = Array.from({ length: paramCount }, () => new Array<number>(paramCount).fill(0));
  for (let j = 0; j < paramCount; j++) {
    if (!free[j]) {
      continue;
    }
    const unit = new Array<number>(paramCount).fill(0);
    unit[j] = 1;
    const column = solveFree(a, unit, free, 1);
    if (column === null) {
      return null;
    }
    for (let i = 0; i < paramCount; i++) {
      inverse[i][j] = column[i];
    }
  }
  return inverse;
}

/**
 * The 1σ ellipse of a 2 × 2 east and north covariance: the square roots of
 * its eigenvalues, km, and the major axis's bearing, degrees clockwise from
 * north in [0, 180).
 */
export function covEllipse(cov: Cov2): Ellipse {
  const a = cov[0][0];
  const b = (cov[0][1] + cov[1][0]) / 2;
  const c = cov[1][1];
  const mid = (a + c) / 2;
  const disc = Math.hypot((a - c) / 2, b);
  const majorKm = Math.sqrt(Math.max(0, mid + disc));
  const minorKm = Math.sqrt(Math.max(0, mid - disc));
  // the major axis's angle from east, counterclockwise
  const theta = Math.atan2(2 * b, a - c) / 2;
  const bearing = (((90 - theta * degPerRad) % 180) + 180) % 180;
  return { majorKm, minorKm, bearingDeg: bearing };
}
