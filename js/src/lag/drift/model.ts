// The drift model's constants and its variance (lag.dev/DRIFTTEST.md V2, V3),
// as connect's lagdrift has them (geo.go, model.go):
//
//   σᵢ² = σ_meas,i² + σ₀² + τᵢ²,  τᵢ² = (κ · K · dᵢ)² + (K · s_ext,i)²
//
// σ_meas,i is the round trip's standard error (V1) and σ₀ a floor for the
// stack's own timing. τᵢ is the path's route and place error: real paths stray
// from the great circle by a share κ of the distance, and the extender's place
// is off by s_ext,i km. τᵢ belongs to the path, not the run, so the runs of one
// extender share it (V7).

import type { Point } from "./geo";
import type { RoundTrip } from "./roundTrip";

/** v, the speed a round trip's distance travels at, km/ms (V2). */
export const speedKmPerMs = 133.24;
/** The speed of light in a vacuum, for the physical check, km/ms (V5). */
export const vacuumKmPerMs = 299.792458;
/** The verdict's confidence when none is chosen (V6). */
export const defaultConfidence = 0.99;
/** The region's k for a clear stop (V10). */
export const earlyK = 4.0;
/** K = 2 / v: ms of round trip per km of distance (V2). */
export const msPerKm = 2 / speedKmPerMs;

/** κ until V4 calibrates it. */
export const defaultKappa = 0.25;
/** σ₀, ms. */
export const defaultSigma0Ms = 1.0;
/**
 * An extender's place error is GeoLite2's accuracy radius held to [10, 100]
 * km, or 50 km for a place derived another way (V3, A4).
 */
export const minPlaceErrorKm = 10;
export const maxPlaceErrorKm = 100;
export const derivedPlaceErrorKm = 50;

/** The constants V4 calibrates (V3). */
export interface Model {
  /** the route error's share of the distance */
  kappa: number;
  /** the timing floor, ms */
  sigma0Ms: number;
}

/** κ = 0.25 and σ₀ = 1 ms, until V4 calibrates them. */
export function defaultModel(): Model {
  return { kappa: defaultKappa, sigma0Ms: defaultSigma0Ms };
}

/** One extender's round trip in one run (V5, V7). */
export interface Observation {
  /** the extender: every run to the same id shares its route error */
  extenderId: string;
  /** the extender's place */
  at: Point;
  /**
   * GeoLite2's accuracy radius for the extender's address, km. The model
   * holds it to [10, 100], and uses 50 when it is not positive (V3).
   */
  placeErrorKm: number;
  /** the run's number: one last-mile offset per distinct run */
  run: number;
  /** V1's estimate */
  roundTrip: RoundTrip;
}

/**
 * The place error the model uses for an extender, km: the given accuracy
 * radius held to [10, 100], or 50 when it is unknown (not a positive finite
 * number).
 */
export function heldPlaceErrorKm(placeErrorKm: number): number {
  if (!(0 < placeErrorKm) || placeErrorKm === Infinity) {
    return derivedPlaceErrorKm;
  }
  return Math.max(minPlaceErrorKm, Math.min(maxPlaceErrorKm, placeErrorKm));
}

/** τ, the path's route and place error, ms, for an extender d km away whose place is off by placeKm (already held). */
export function pathSigmaMs(model: Model, dKm: number, placeKm: number): number {
  const route = model.kappa * msPerKm * dKm;
  const place = msPerKm * placeKm;
  return Math.sqrt(route * route + place * place);
}

/** σ_meas² + σ₀², ms²: the variance a run's own measurement adds. */
export function noiseVar(model: Model, sigmaMs: number): number {
  return sigmaMs * sigmaMs + model.sigma0Ms * model.sigma0Ms;
}
