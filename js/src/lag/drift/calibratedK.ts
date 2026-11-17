// Calibrated k (lag.dev/DRIFTTEST.md V11, the user's decision of 2026-11-17:
// "drop 99.9%, calibrate"), as connect's lagdrift has it (calibrated_k.go).
// T3 found the textbook region, kForConfidence, too narrow in a thin tail of
// fits: about right at 99% outside Oceania, far too narrow around New Zealand,
// and wrong 5–10 times too often at 99.9%, which the user dropped. A run's k is
// the larger of the textbook k and a calibrated k, the empirical quantile V4
// measures per continent from lag.dev tracks; a continent without enough
// tracks uses defaultK, T3's per-region k. The clear stop's k, kEarly, scales
// with k, so a clear stop is never easier than the verdict.

import { kForConfidence } from "./judge";
import { earlyK } from "./model";

/** k_early's scale: earlyK over the textbook 99% k, 4.0 / 3.035 ≈ 1.32 (V11). */
export const kEarlyScale = 4.0 / 3.035;

// GeoLite2's continent codes
const continentCodes: readonly string[] = ["AF", "AN", "AS", "EU", "NA", "OC", "SA"];

// V11's defaults where T3 found the textbook k too narrow, by continent code,
// "" for an unknown continent. A continent not listed keeps the textbook k.
const defaultK95: Readonly<Record<string, number>> = { AF: 2.5, OC: 2.79 };
const defaultK99: Readonly<Record<string, number>> = { NA: 3.13, SA: 3.5, AF: 3.71, OC: 7.88, AN: 3.49, "": 3.49 };

/**
 * V11's default k at a confidence for a continent, never below the textbook
 * kForConfidence. The continent is GeoLite2's two-letter code (AF, AN, AS,
 * EU, NA, OC, SA) in either case; anything else is unknown.
 *
 *   - 95%: AF 2.50, OC 2.79, every other continent textbook (2.448);
 *   - 99%: NA 3.13, SA 3.50, AF 3.71, OC 7.88, AN and unknown 3.49, EU and AS
 *     textbook (3.035).
 *
 * V11 allows only 95% and 99%; any other confidence gets the textbook k.
 */
export function defaultK(confidence: number, continent: string): number {
  const k = kForConfidence(confidence);
  let defaults: Readonly<Record<string, number>>;
  if (confidence === 0.95) {
    defaults = defaultK95;
  } else if (confidence === 0.99) {
    defaults = defaultK99;
  } else {
    return k;
  }
  let code = continent.toUpperCase();
  if (!continentCodes.includes(code)) {
    code = "";
  }
  return Math.max(k, defaults[code] ?? 0);
}

/**
 * The clear stop's k for a run's k (V11): max(earlyK, 1.32 × k), with 1.32 =
 * kEarlyScale, so a clear stop is never easier than the verdict. At the
 * textbook 99% k it is earlyK.
 */
export function kEarly(k: number): number {
  return Math.max(earlyK, kEarlyScale * k);
}
