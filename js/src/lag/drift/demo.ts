// W8: the demo token's verdict, computed in the frame (lag.dev/DRIFTTEST.md
// W8, V6, V7). A real token's verdict is the server's, from the extenders'
// signed reports and the 1:10m borders; the demo has no reports and stores
// nothing, so the frame pools its own last 3 counting runs, fits them with
// the same model and judges them against the simplified map (G2) by V6's
// rule. It also returns the figures the demo shows and a real token never
// shows the visitor: the drift country, the distance and margin to the IP
// country, and the ±.

import type { Borders } from "./borders";
import { driftCountryWithinKm } from "./borders";
import { fitPosition, type Fit } from "./fit";
import type { Point } from "./geo";
import { judge, regionRadiusKm, type Judgement } from "./judge";
import { defaultConfidence, defaultModel, type Model, type Observation } from "./model";
import { physicallyPossible } from "./physical";
import { estimateRoundTrip } from "./roundTrip";

/** V6, P4: a verdict. */
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

/** E5: an extender is usable in a run with this many round trips. */
export const driftUsableRoundTrips = 12;
/** E5, V7: a run counts with this many usable extenders. */
export const driftMinRunExtenders = 6;
/** V7: the verdict pools this many of the last counting runs. */
export const driftWindowRuns = 3;
/** V6: above this σ_major the geometry can't place the visitor, km. */
export const driftMaxSigmaKm = 1500;

/** One extender's round trips in one run the frame measured. */
export interface DriftRunExtender {
  extenderId: string;
  /** the extender's place, from `start` */
  at: Point;
  /** s_ext (V3), km; the derived place's 50 km when unknown */
  placeErrorKm?: number;
  /** the run's round trips to it, in the order sent, lost pings left out */
  roundTripsMs: readonly number[];
}

/** One run the frame measured. */
export interface DriftRun {
  /** its number, from `start` */
  run: number;
  extenders: readonly DriftRunExtender[];
}

/** E5: a run's observations, one per extender with 12 round trips or more. */
export function driftRunObservations(run: DriftRun): Observation[] {
  const observations: Observation[] = [];
  for (const extender of run.extenders) {
    const samples = extender.roundTripsMs.filter((ms) => Number.isFinite(ms));
    if (samples.length < driftUsableRoundTrips) {
      continue;
    }
    const roundTrip = estimateRoundTrip(samples);
    if (roundTrip !== null) {
      observations.push({
        extenderId: extender.extenderId,
        at: extender.at,
        placeErrorKm: extender.placeErrorKm ?? 0,
        run: run.run,
        roundTrip,
      });
    }
  }
  return observations;
}

/** V7: whether a run counts toward a verdict: 6 usable extenders. */
export function driftRunCounts(run: DriftRun): boolean {
  return driftRunObservations(run).length >= driftMinRunExtenders;
}

export interface LocalDriftVerdictOptions {
  /** the simplified map (`loadDriftWorld()`) */
  borders: Borders;
  /** the IP country's code, from `start`'s IP place */
  ipCountryCode: string;
  /** the IP place, from `start`: the fit's cold start */
  expected: Point;
  /** the frame's runs, oldest first */
  runs: readonly DriftRun[];
  /** default 0.99 */
  confidence?: number;
  model?: Model;
}

/** W8: the demo's verdict and figures. */
export interface LocalDriftVerdict {
  verdict: DriftVerdict;
  reason: DriftVerdictReason;
  /** the runs pooled, oldest first */
  runsInVerdict: number[];
  confidence: number;
  ipCountryCode: string;
  /** the country containing the fit, else the nearest within 300 km, else "" at sea (V6) */
  driftCountryCode: string;
  /** V6's figures toward the IP country's nearest border point, km; null without a fit and a country */
  distanceKm: number | null;
  significanceKm: number | null;
  marginKm: number | null;
  /** V8's ±: the radius of a circle with the region's area, km */
  plusMinusKm: number | null;
  fit: Fit | null;
  judgement: Judgement | null;
}

/**
 * W8: pools the last 3 of the frame's runs that count (V7), fits them, and
 * judges the fit against the simplified map by V6's rule. The physical check
 * (V5) turns an agree into `inconclusive`/`inconsistent`; it never rejects.
 */
export function localDriftVerdict(options: LocalDriftVerdictOptions): LocalDriftVerdict {
  const confidence = options.confidence ?? defaultConfidence;
  const model = options.model ?? defaultModel();
  const ipCountryCode = options.ipCountryCode.toLowerCase();
  const counting = options.runs
    .map((run) => ({ run: run.run, observations: driftRunObservations(run) }))
    .filter(({ observations }) => observations.length >= driftMinRunExtenders);
  const window = counting.slice(-driftWindowRuns);
  const result: LocalDriftVerdict = {
    verdict: "inconclusive",
    reason: "too_few_extenders",
    runsInVerdict: window.map(({ run }) => run),
    confidence,
    ipCountryCode,
    driftCountryCode: "",
    distanceKm: null,
    significanceKm: null,
    marginKm: null,
    plusMinusKm: null,
    fit: null,
    judgement: null,
  };
  if (window.length === 0) {
    return result;
  }
  const observations = window.flatMap(({ observations }) => observations);
  const fit = fitPosition(model, options.expected, observations, null);
  if (fit === null) {
    return { ...result, reason: "low_precision" };
  }
  result.fit = fit;
  result.driftCountryCode = options.borders.countryAt(fit.at, driftCountryWithinKm);
  result.plusMinusKm = regionRadiusKm(fit, confidence);
  const country = options.borders.country(ipCountryCode);
  if (country !== null) {
    const judgement = judge(fit, country, confidence);
    result.judgement = judgement;
    result.distanceKm = judgement.distanceKm;
    result.significanceKm = judgement.significanceKm;
    result.marginKm = judgement.marginKm;
  }
  if (fit.majorKm > driftMaxSigmaKm) {
    return { ...result, reason: "low_precision" };
  }
  if (country === null || result.judgement === null) {
    return { ...result, reason: "ip_country_unknown" };
  }
  const { inside, overlaps } = result.judgement;
  if (!inside && !overlaps) {
    return { ...result, verdict: "disagree", reason: "region_outside" };
  }
  if (!physicallyPossible(observations, country)) {
    return { ...result, reason: "inconsistent" };
  }
  return { ...result, verdict: "agree", reason: inside ? "inside_ip_country" : "region_overlaps" };
}
