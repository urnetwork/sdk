// The drift model's constants and its variance (lag.dev/DRIFTTEST.md V2, V3).
// Each extender's round trip is yᵢ ≈ b + K · d(p, eᵢ), with K = 2 / v, and
// its term in the fit has the variance
//
//   σᵢ² = σ_meas,i² + σ₀² + τᵢ²,  τᵢ² = (κ · K · dᵢ)² + (K · s_ext,i)²
//
// where τᵢ belongs to the path and is shared by every run that used it (V7).

import type { Point } from "./geo";
import type { RoundTrip } from "./roundTrip";

/** v: how far a round trip's signal goes, one way, per ms (V2), km/ms. */
export const speedKmPerMs = 133.24;

/** The speed of light in a vacuum (V5), km/ms. */
export const vacuumKmPerMs = 299.792458;

/** K = 2 / v: ms of round trip per km of distance (V2). */
export const msPerKm = 2 / speedKmPerMs;

/** V6: the confidence of the region a verdict judges, unless a token asks for another. */
export const defaultConfidence = 0.99;

/** V10: the k of the region a `clear` stop judges. */
export const earlyK = 4.0;

/** V3: the place error of an extender whose place has no accuracy of its own (a derived place, A4), km. */
export const derivedPlaceErrorKm = 50;

/** V3: the bounds an extender's place error is held to, km. */
export const minPlaceErrorKm = 10;
export const maxPlaceErrorKm = 100;

/** V3: the model's calibrated constants (V4). */
export interface Model {
  /** route error as a share of the distance */
  kappa: number;
  /** a floor for the stack's own timing, ms */
  sigma0Ms: number;
}

/** V3: κ = 0.25 and σ₀ = 1 ms, until V4 calibrates them. */
export function defaultModel(): Model {
  return { kappa: 0.25, sigma0Ms: 1 };
}

/** One extender's round trip in one run (V5, V7). */
export interface Observation {
  extenderId: string;
  /** the extender's place */
  at: Point;
  /** s_ext: the extender's place error, km (V3) */
  placeErrorKm: number;
  /** the run it was measured in */
  run: number;
  roundTrip: RoundTrip;
}

/**
 * s_ext,i (V3): GeoLite2's accuracy radius for the extender's address, held
 * to [10, 100] km; 50 km when it has none.
 */
export function placeErrorKm(km: number): number {
  if (!(Number.isFinite(km) && km > 0)) {
    return derivedPlaceErrorKm;
  }
  return Math.min(maxPlaceErrorKm, Math.max(minPlaceErrorKm, km));
}

/** τᵢ² (V3): the path's route and place error, ms², for an extender `distanceKm` away. */
export function pathVariance(model: Model, distanceKm: number, extenderPlaceErrorKm: number): number {
  const route = model.kappa * msPerKm * distanceKm;
  const place = msPerKm * placeErrorKm(extenderPlaceErrorKm);
  return route * route + place * place;
}

/** σ_meas² + σ₀² (V3, V7): one run's own error for one extender, ms². */
export function measurementVariance(model: Model, roundTrip: RoundTrip): number {
  return roundTrip.sigmaMs * roundTrip.sigmaMs + model.sigma0Ms * model.sigma0Ms;
}
