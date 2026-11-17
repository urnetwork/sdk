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
// path moves every run's residual the same way, which is z's direction, so it
// is discounted in every run at once (V7). The rest of the block's residual,
// ‖r⊥‖² = rᵀV⁻¹r − z², is how its runs differ, which is measurement noise, and
// keeps its full weight. So the fit minimizes
//
//   F = Σ_blocks 2ρ_H(z) + ‖r⊥‖²
//
// with ρ_H(z) = z²/2 within c and c|z| − c²/2 beyond. The predicted distance
// is smoothed within 10 km of an extender, so the model is differentiable at
// an extender's place.
//
// Starts and convergence (V5): the fit starts from the expected point, the
// places of the three fastest extenders and the warm fit, if any. It
// iterates each start until a step lowers F by less than 10⁻⁶ of F and the
// undamped step from there would move the position less than 0.1 km, or no
// step lowers F, within 100 trial steps, and keeps the lowest F. It leaves the
// warm fit's basin only for an F lower by more than 1.
//
// The variances, whose route error grows with dᵢ, are fixed per pass
// (feasible GLS in two steps), and the covariance measures them from the fit:
//
//  1. the first pass measures each dᵢ from the expected place and runs the
//     cold starts, the expected point and the three fastest extenders, so it
//     depends on the data alone;
//  2. the second measures each dᵢ from the first pass's fit and runs from that
//     fit and from the warm fit, which wins unless the other's F is lower by
//     more than 1.
//
// Each start is minimized by variable projection: the offsets enter the
// model linearly and F is convex in them, so at every position they are
// solved exactly, and Levenberg–Marquardt steps the position alone on the
// profile min_b F, with the Schur complement of Newton's matrix over the free
// offsets. Newton's matrix is JᵀWJ plus the position's curvature that
// Gauss–Newton leaves out, −K Σⱼ (Wr)ⱼ ∇²dⱼ. λ, which scales JᵀWJ's position
// diagonal into the step's matrix, follows Nielsen's rule from 0.001, and a
// step is taken only when it lowers F. The position step is capped at 400 km.
//
// The covariance of (east, north, b) is (JᵀWJ)⁻¹ × max(1, χ²/(n − 2 − R)) at
// the solution, with W the Huber-weighted block inverse and χ² = Σ rᵀWr. It
// is the full inverse, every offset free, even one the solver holds at 0
// (V5): conditioning on b = 0 would claim a certainty the fit doesn't have.
//
// Sums run in the Go's order: blocks in the order their extenders first
// appear, and rows in a block in the observations' order.

import { azimuthalKm, bearingDeg, degPerRad, distanceKm, hypot, moveKm, pointValid, radPerDeg, type Point } from "./geo";
import { heldPlaceErrorKm, minPlaceErrorKm, msPerKm, noiseVar, pathSigmaMs, type Model, type Observation } from "./model";

/** The Huber weights' c (V5). */
export const huberC = 2;
/** The largest position step, km (driftFit.ts). */
export const maxStepKm = 400;
/** Fewer extenders can't place anyone. A run's minimum of 6 is the caller's. */
export const minFitExtenders = 3;

// a start has converged when a step lowers F by less than this share of F
// and the undamped step from there would move the position less than this, km (V5)
const convergedKm = 0.1;
const convergedShare = 1e-6;
// a start's trial steps (V5)
const maxFitIterations = 100;
// the starts besides the warm fit and the expected point: the places of this many fastest extenders (V5)
const startExtenders = 3;
// the warm fit's basin is left only for an F lower by more than this (V5)
const warmBasinMargin = 1;
// Levenberg–Marquardt's λ, which scales JᵀWJ's diagonal into the step's
// matrix: it starts at, and never falls below, driftFit.ts's 0.001
const minDamping = 0.001;
const maxDamping = 1e9;
// a cold start's offset, as a share of the run's lowest round trip
const coldLastMileShare = 0.7;
// a pivot this small is singular
const minPivot = 1e-20;
// solving the offsets at a position: Newton's steps until they move less than
// this, ms, or this many, each halved at most this many times
const offsetSettledMs = 1e-9;
const maxOffsetIterations = 30;
const maxOffsetBacktracks = 20;
// the distance is smoothed this close to an extender, km
const smoothingKm = minPlaceErrorKm;

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
  /** τ, the route and place error, for the pass */
  tauMs: number;
}

interface FitBlock {
  extenderId: string;
  rows: FitRow[];
}

type Matrix2 = [[number, number], [number, number]];

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
 * `warm`, when given, is a start too, with its offsets matched by run number
 * through `warm.runs` (by index when it has no runs and as many offsets as the
 * observations have runs). The other starts, and a run it lacks, begin each
 * offset at 0.7 times the run's lowest round trip.
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
      tauMs: 0,
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

  // τ for each row, its distance measured from a place
  const routeErrorsFrom = (at: Point) => {
    for (const block of blocks) {
      for (const row of block.rows) {
        row.tauMs = pathSigmaMs(model, distanceKm(at, row.at), row.placeKm);
      }
    }
  };

  // pass 1: the route errors measured from the expected place, one F that
  // every cold start minimizes, from the expected point and the places of the
  // three fastest extenders; the lowest F wins, so this pass depends on the
  // data alone
  routeErrorsFrom(expected);
  const coldStarts = [expected, ...fastestPlaces(blocks, startExtenders)];
  let first: FitMinimum | null = null;
  for (const start of coldStarts) {
    const minimum = minimizeFit(blocks, start, coldLastMile, paramCount);
    if (first === null || minimum.objective < first.objective) {
      first = minimum;
    }
  }
  // pass 2: the route errors measured from the first pass's fit, from that
  // fit and from the warm fit, if any, whose basin is left only for an F
  // lower by more than 1
  routeErrorsFrom(first!.p);
  let final = minimizeFit(blocks, first!.p, first!.b, paramCount);
  if (warm && pointValid(warm.at)) {
    const warmB = coldLastMile.slice();
    runNumbers.forEach((run, r) => {
      const lastMile = warmLastMile(warm, run, r, runCount);
      if (lastMile !== undefined) {
        warmB[r] = Math.max(0, lastMile);
      }
    });
    const fromWarm = minimizeFit(blocks, warm.at, warmB, paramCount);
    if (!(final.objective < fromWarm.objective - warmBasinMargin)) {
      final = fromWarm;
    }
  }
  const p = final.p;
  const b = final.b;

  // the covariance and χ² with the route errors measured from the fit
  routeErrorsFrom(p);
  const { a, chi2 } = normalEquations(blocks, p, b, paramCount);

  // V5: an offset held at its bound in the solver is free here, so the
  // position keeps the freedom b would absorb
  const free = new Array<boolean>(paramCount).fill(true);
  const inverse = invertFree(a, free);
  if (inverse === null) {
    return null;
  }
  const dof = n - 2 - runCount;
  const chi2PerDof = chi2 / Math.max(1, dof);
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

/** Where one start's minimization of F ended. */
interface FitMinimum {
  p: Point;
  b: number[];
  objective: number;
  /** trial steps taken */
  iterations: number;
  /** it met the convergence rule, rather than the iteration cap */
  converged: boolean;
}

/**
 * Minimizes F, with the rows' τ as they are, from (p, b), by variable
 * projection: at every position the offsets are solved exactly
 * (solveOffsets), and Levenberg–Marquardt steps the position alone on the
 * profile φ(p) = min_b F(p, b), with the Schur complement of Newton's matrix
 * over the free offsets, damped by λ times JᵀWJ's position diagonal. A step
 * is taken only when it lowers φ. It converges once a step taken lowers φ by
 * less than 10⁻⁶ of φ and the undamped step from there would move the
 * position less than 0.1 km, or when no step lowers φ, within 100 trial steps.
 */
function minimizeFit(blocks: readonly FitBlock[], p: Point, b: readonly number[], paramCount: number): FitMinimum {
  let eq = solveOffsets(blocks, p, b, paramCount);
  let lambda = minDamping;
  let nu = 2.0;
  const minimum: FitMinimum = { p, b: [], objective: 0, iterations: 0, converged: false };
  while (minimum.iterations < maxFitIterations) {
    minimum.iterations += 1;
    const profile = eq.profile();
    let step: [number, number] | null = null;
    if (profile !== null) {
      const [s, gr] = profile;
      step = solve2(
        [
          [s[0][0] + lambda * eq.a[0][0], s[0][1]],
          [s[1][0], s[1][1] + lambda * eq.a[1][1]],
        ],
        gr,
      );
      if (step !== null) {
        const length = hypot(step[0], step[1]);
        if (maxStepKm < length) {
          step[0] *= maxStepKm / length;
          step[1] *= maxStepKm / length;
        }
        const next = solveOffsets(blocks, moveKm(eq.p, step[0], step[1]), eq.b, paramCount);
        if (next.objective <= eq.objective) {
          // the gain over the quadratic model's 2δᵀg − δᵀSδ
          const predicted =
            2 * (step[0] * gr[0] + step[1] * gr[1]) -
            (step[0] * (s[0][0] * step[0] + s[0][1] * step[1]) + step[1] * (s[1][0] * step[0] + s[1][1] * step[1]));
          let gain = 0.0;
          if (0 < predicted) {
            gain = (eq.objective - next.objective) / predicted;
          }
          const drop = eq.objective - next.objective;
          eq = next;
          const c = 2 * gain - 1;
          lambda = Math.max(minDamping, lambda * Math.max(1.0 / 3, 1 - c * c * c));
          nu = 2;
          // converged when the whole step from here is short and φ barely
          // moved: a damped step can be short far from the minimum
          if (drop < convergedShare * eq.objective) {
            const there = eq.profile();
            if (there !== null) {
              const whole = solve2(there[0], there[1]);
              if (whole !== null && hypot(whole[0], whole[1]) < convergedKm) {
                minimum.converged = true;
                break;
              }
            }
          }
          continue;
        }
      }
    }
    // no step lowers φ at this damping: damp more, unless no step can, which is a minimum
    lambda *= nu;
    nu *= 2;
    if (maxDamping < lambda) {
      minimum.converged = true;
      break;
    }
  }
  minimum.p = eq.p;
  minimum.b = eq.b;
  minimum.objective = eq.objective;
  return minimum;
}

/** A position, its offsets, and the normal equations there. */
class FitEquations {
  p: Point;
  b: number[];
  a: number[][];
  g: number[];
  objective: number;
  curvature: Matrix2;

  constructor(p: Point, b: number[], normal: Normal) {
    this.p = p;
    this.b = b;
    this.a = normal.a;
    this.g = normal.g;
    this.objective = normal.objective;
    this.curvature = normal.curvature;
  }

  /**
   * The position's profile at these offsets: the Schur complement of Newton's
   * matrix over the free offsets, S = H_pp − H_pf H_ff⁻¹ H_fp, and the reduced
   * gradient g_p − H_pf H_ff⁻¹ g_f. An offset at 0 is held, not free. `null`
   * when H_ff is singular.
   */
  profile(): [Matrix2, [number, number]] | null {
    const h = newtonMatrix(this.a, this.curvature);
    const free: number[] = [];
    for (let r = 0; r < this.b.length; r++) {
      if (0 < this.b[r]) {
        free.push(2 + r);
      }
    }
    const s: Matrix2 = [
      [h[0][0], h[0][1]],
      [h[1][0], h[1][1]],
    ];
    const gr: [number, number] = [this.g[0], this.g[1]];
    if (0 < free.length) {
      // H_ff⁻¹ [H_fp | g_f], column by column
      const k = free.length;
      for (let col = 0; col < 3; col++) {
        const augmented = free.map((i) => {
          const row = new Array<number>(k + 1);
          free.forEach((j, c) => {
            row[c] = h[i][j];
          });
          row[k] = col < 2 ? h[i][col] : this.g[i];
          return row;
        });
        const x = gaussSolve(augmented);
        if (x === null) {
          return null;
        }
        for (let u = 0; u < 2; u++) {
          let sum = 0.0;
          free.forEach((i, row) => {
            sum += h[u][i] * x[row];
          });
          if (col < 2) {
            s[u][col] -= sum;
          } else {
            gr[u] -= sum;
          }
        }
      }
    }
    return [s, gr];
  }
}

/**
 * The offsets that minimize F at the position p, from b: Newton's steps on
 * the offsets alone, which F is convex in, each offset at its bound of 0 whose
 * step would go below it held there, a step halved while it raises F, until
 * the offsets move less than 10⁻⁹ ms or 30 steps.
 */
function solveOffsets(blocks: readonly FitBlock[], p: Point, b: readonly number[], paramCount: number): FitEquations {
  let eq = new FitEquations(p, b.slice(), normalEquations(blocks, p, b, paramCount));
  const free = new Array<boolean>(paramCount).fill(true);
  free[0] = false;
  free[1] = false;
  for (let iteration = 0; iteration < maxOffsetIterations; iteration++) {
    const step = constrainedStepOver(eq.a, eq.g, eq.b, free);
    if (step === null) {
      break;
    }
    let largest = 0.0;
    for (let r = 0; r < eq.b.length; r++) {
      largest = Math.max(largest, Math.abs(step[2 + r]));
    }
    if (largest < offsetSettledMs) {
      break;
    }
    let t = 1.0;
    let next: FitEquations | null = null;
    for (let tries = 0; tries < maxOffsetBacktracks; tries++) {
      const nextB = eq.b.map((value, r) => Math.max(0, value + step[2 + r] * t));
      const candidate = new FitEquations(p, nextB, normalEquations(blocks, p, nextB, paramCount));
      if (candidate.objective <= eq.objective) {
        next = candidate;
        break;
      }
      t /= 2;
    }
    if (next === null) {
      break;
    }
    eq = next;
  }
  return eq;
}

/** The 2 × 2 system's solution; `null` when it is singular. */
function solve2(m: Matrix2, v: readonly [number, number]): [number, number] | null {
  const solution = gaussSolve([
    [m[0][0], m[0][1], v[0]],
    [m[1][0], m[1][1], v[1]],
  ]);
  return solution === null ? null : [solution[0], solution[1]];
}

/** JᵀWJ with the position block's curvature added: half of F's Hessian. */
function newtonMatrix(a: readonly (readonly number[])[], curvature: Matrix2): number[][] {
  const h = a.map((row) => row.slice());
  for (let u = 0; u < 2; u++) {
    for (let v = 0; v < 2; v++) {
      h[u][v] += curvature[u][v];
    }
  }
  return h;
}

/**
 * The places of the n extenders with the lowest round trips, each at the
 * place of its lowest, in the order of their round trips (ties in the blocks'
 * order).
 */
function fastestPlaces(blocks: readonly FitBlock[], n: number): Point[] {
  const extenders = blocks.map((block) => {
    let f = { at: block.rows[0].at, y: block.rows[0].y };
    for (const row of block.rows.slice(1)) {
      if (row.y < f.y) {
        f = { at: row.at, y: row.y };
      }
    }
    return f;
  });
  // Array.prototype.sort is stable, as Go's SortStableFunc
  extenders.sort((a, b) => (a.y < b.y ? -1 : b.y < a.y ? 1 : 0));
  return extenders.slice(0, Math.min(n, extenders.length)).map((f) => f.at);
}

interface Normal {
  /** JᵀWJ */
  a: number[][];
  /** JᵀWr */
  g: number[];
  /** Σ rᵀWr */
  chi2: number;
  /** F = Σ 2ρ_H(z) + ‖r⊥‖² */
  objective: number;
  /** the position block's curvature JᵀWJ leaves out, −K Σⱼ (Wr)ⱼ ∇²dⱼ */
  curvature: Matrix2;
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
  /** ∇²d, the smoothed distance's curvature in east and north */
  curvature: Matrix2;
}

/**
 * The normal equations at (p, b): JᵀWJ, JᵀWr, χ² = Σ rᵀWr, the objective
 * F = Σ 2ρ_H(z) + ‖r⊥‖², with τ the rows' own, and the position block's
 * curvature. Per block, the shared component of r, z = tᵀD⁻¹r / √(Q (1 + Q)),
 * is Huber-weighted by h, and the rest, ‖r⊥‖² = rᵀD⁻¹r − (tᵀD⁻¹r)² / Q, is
 * not:
 *
 *   W = D⁻¹ − ω D⁻¹ t tᵀ D⁻¹,  ω = (1 + Q − h) / (Q (1 + Q))
 *
 * so rᵀWr = ‖r⊥‖² + h z², and W = V⁻¹ when h = 1. The parameters are east
 * km, north km, then b per run; J is the model's slope, and JᵀWr is half of
 * −∂F/∂δ. Half of ∂²F/∂δ² is JᵀWJ plus the curvature, with W held: Newton's
 * matrix.
 */
function normalEquations(blocks: readonly FitBlock[], p: Point, b: readonly number[], paramCount: number): Normal {
  const a = Array.from({ length: paramCount }, () => new Array<number>(paramCount).fill(0));
  const g = new Array<number>(paramCount).fill(0);
  let chi2 = 0.0;
  let objective = 0.0;
  const curvature: Matrix2 = [
    [0, 0],
    [0, 0],
  ];
  const alpha = new Array<number>(paramCount).fill(0);
  for (const block of blocks) {
    const terms: RowTerms[] = [];
    alpha.fill(0);
    // Q = tᵀD⁻¹t, ρ = tᵀD⁻¹r, rᵀD⁻¹r and α = JᵀD⁻¹t
    let q = 0.0;
    let rho = 0.0;
    let rr = 0.0;
    for (const row of block.rows) {
      const d = distanceKm(p, row.at);
      const beta = bearingDeg(p, row.at) * radPerDeg;
      const [smooth, slope] = smoothedDistanceKm(d);
      const sinBeta = Math.sin(beta);
      const cosBeta = Math.cos(beta);
      const term: RowTerms = {
        jEast: -sinBeta * msPerKm * slope,
        jNorth: -cosBeta * msPerKm * slope,
        run: row.run,
        r: row.y - (b[row.run] + msPerKm * smooth),
        d: row.noiseVar,
        t: row.tauMs,
        curvature: distanceCurvature(d, sinBeta, cosBeta),
      };
      terms.push(term);
      const t = row.tauMs;
      q += (t * t) / term.d;
      rho += (t * term.r) / term.d;
      rr += (term.r * term.r) / term.d;
      alpha[0] += (term.jEast * t) / term.d;
      alpha[1] += (term.jNorth * t) / term.d;
      alpha[2 + term.run] += t / term.d;
    }
    let omega = 0.0;
    if (0 < q) {
      const z = Math.abs(rho / Math.sqrt(q * (1 + q)));
      let h = 1.0;
      let huber = z * z;
      if (huberC < z) {
        h = huberC / z;
        huber = 2 * huberC * z - huberC * huberC;
      }
      omega = (1 + q - h) / (q * (1 + q));
      const perp = rr - (rho * rho) / q;
      chi2 += perp + h * z * z;
      objective += perp + huber;
    } else {
      chi2 += rr;
      objective += rr;
    }
    for (const term of terms) {
      const index = [0, 1, 2 + term.run];
      const j = [term.jEast, term.jNorth, 1];
      for (let u = 0; u < 3; u++) {
        g[index[u]] += (j[u] * term.r) / term.d;
        for (let v = 0; v < 3; v++) {
          a[index[u]][index[v]] += (j[u] * j[v]) / term.d;
        }
      }
      // −K (Wr)ⱼ ∇²dⱼ, with (Wr)ⱼ = (rⱼ − ω tⱼ ρ) / Dⱼ
      const wr = (term.r - omega * term.t * rho) / term.d;
      for (let u = 0; u < 2; u++) {
        for (let v = 0; v < 2; v++) {
          curvature[u][v] -= msPerKm * wr * term.curvature[u][v];
        }
      }
    }
    for (let u = 0; u < paramCount; u++) {
      if (alpha[u] === 0) {
        continue;
      }
      g[u] -= omega * alpha[u] * rho;
      for (let v = 0; v < paramCount; v++) {
        a[u][v] -= omega * alpha[u] * alpha[v];
      }
    }
  }
  return { a, g, chi2, objective, curvature };
}

/**
 * ∇²d of the smoothed distance in east and north km at a distance d, β the
 * bearing from the fit to the extender: (I − u uᵀ) / d with u the unit vector
 * from the extender, and I / ε within ε of it.
 */
function distanceCurvature(d: number, sinBeta: number, cosBeta: number): Matrix2 {
  if (d < smoothingKm) {
    return [
      [1 / smoothingKm, 0],
      [0, 1 / smoothingKm],
    ];
  }
  return [
    [(cosBeta * cosBeta) / d, (-sinBeta * cosBeta) / d],
    [(-sinBeta * cosBeta) / d, (sinBeta * sinBeta) / d],
  ];
}

/**
 * The distance the model predicts a round trip from (V2), and its slope in
 * the true distance d: d itself, except within 10 km of the extender, below
 * its place error's floor (V3), where it is (d² + ε²) / (2ε), ε = 10 km. That
 * is d's value and slope at ε, and smooth at the extender's place.
 */
export function smoothedDistanceKm(d: number): [number, number] {
  if (smoothingKm <= d) {
    return [d, 1];
  }
  return [(d * d + smoothingKm * smoothingKm) / (2 * smoothingKm), d / smoothingKm];
}

/**
 * The step solving a δ = g over the free parameters, the rest held at a step
 * of 0, and each offset at its bound of 0 whose step would go below it held
 * there too. `null` when the system is singular.
 */
function constrainedStepOver(a: number[][], g: number[], b: readonly number[], free: readonly boolean[]): number[] | null {
  const over = free.slice();
  for (;;) {
    const step = solveFree(a, g, over);
    if (step === null) {
      return null;
    }
    let held = false;
    for (let r = 0; r < b.length; r++) {
      if (over[2 + r] && b[r] <= 0 && step[2 + r] < 0) {
        over[2 + r] = false;
        held = true;
      }
    }
    if (!held) {
      return step;
    }
  }
}

/** Solves the system over the free parameters; a held parameter's step is 0. */
function solveFree(a: number[][], g: number[], free: readonly boolean[]): number[] | null {
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
      row[col] = a[i][j];
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
    const column = solveFree(a, unit, free);
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
  const disc = hypot((a - c) / 2, b);
  const majorKm = Math.sqrt(Math.max(0, mid + disc));
  const minorKm = Math.sqrt(Math.max(0, mid - disc));
  // the major axis's angle from east, counterclockwise
  const theta = Math.atan2(2 * b, a - c) / 2;
  const bearing = (((90 - theta * degPerRad) % 180) + 180) % 180;
  return { majorKm, minorKm, bearingDeg: bearing };
}
