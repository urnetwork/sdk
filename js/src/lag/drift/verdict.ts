// The verdict over a fit (lag.dev/DRIFTTEST.md V6, V5), as connect's lagdrift
// decides it (verdict.go): the part of the verdict that is geometry, shared by
// the api and the demo token's local verdict (W8). The api decides the
// run-level reasons before it (ip_mismatch, too_few_extenders,
// ip_family_mismatch, reports_missing), since they need no fit.

import type { Fit } from "./fit";
import { judge, type Judgement } from "./judge";
import type { Observation } from "./model";
import { physicallyPossible } from "./physical";
import { hasBoundary, type Country } from "./polygon";

/** P4: a verdict. */
export type DriftVerdict = "agree" | "disagree" | "inconclusive";

/** P4: why. */
export type DriftVerdictReason =
  | "inside_ip_country"
  | "region_overlaps"
  | "region_outside"
  | "inconsistent"
  | "too_few_extenders"
  | "low_precision"
  | "ip_mismatch"
  | "ip_family_mismatch"
  | "ip_country_unknown"
  | "reports_missing"
  | "stopped_early";

/**
 * The default of lag.yml's max_sigma_km: a fit whose σ_major is larger is too imprecise to reject the visitor (V6),
 * km. It never stops an agree: a wide region that touches the IP country agrees however wide it is (2026-11-20).
 */
export const defaultMaxSigmaKm = 1500;

/** `decide`'s answer. */
export interface Decision {
  verdict: DriftVerdict;
  reason: DriftVerdictReason;
  /** null for an unknown country or no fit */
  judgement: Judgement | null;
}

/**
 * The verdict of a fit against the IP country (V6) at the region's k (V11),
 * with the fit's observations for the physical check (V5):
 *
 *  1. the country unknown (null, or without a ring with a point) →
 *     inconclusive, ip_country_unknown;
 *  2. no fit → inconclusive, low_precision;
 *  3. the region touches the country but it is physically impossible →
 *     inconclusive, inconsistent. The check never rejects on its own (V5);
 *  4. p̂ in the country → agree, inside_ip_country;
 *  5. the region touches it → agree, region_overlaps;
 *  6. the region is outside it but σ_major is above maxSigmaKm →
 *     inconclusive, low_precision: bounds rejection only, never acceptance;
 *  7. otherwise → disagree, region_outside.
 */
export function decide(
  fit: Fit | null,
  country: Country | null,
  observations: readonly Observation[],
  k: number,
  maxSigmaKm: number = defaultMaxSigmaKm,
): Decision {
  if (country === null || !hasBoundary(country)) {
    return { verdict: "inconclusive", reason: "ip_country_unknown", judgement: null };
  }
  if (fit === null) {
    return { verdict: "inconclusive", reason: "low_precision", judgement: null };
  }
  const j = judge(fit, country, k);
  if (j.overlaps && !physicallyPossible(observations, country)) {
    return { verdict: "inconclusive", reason: "inconsistent", judgement: j };
  }
  if (j.inside) {
    return { verdict: "agree", reason: "inside_ip_country", judgement: j };
  }
  if (j.overlaps) {
    return { verdict: "agree", reason: "region_overlaps", judgement: j };
  }
  if (!(fit.majorKm <= maxSigmaKm)) {
    return { verdict: "inconclusive", reason: "low_precision", judgement: j };
  }
  return { verdict: "disagree", reason: "region_outside", judgement: j };
}
