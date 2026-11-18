// W8: the demo token's verdict, computed in the frame (lag.dev/DRIFTTEST.md
// W8, V6, V7). A real token's verdict is the server's, from the extenders'
// signed reports and the 1:10m borders; the demo has no reports and stores
// nothing, so the frame pools its own last 3 counting runs, fits them with
// the same model and decides them against the simplified map (G2) as the api
// does (lagdrift's Decide). It also returns the figures the demo shows and a
// real token never shows the visitor: the drift country, the distance and
// margin to the IP country, and the ±.

import { driftCountryWithinKm, type Borders } from "./borders";
import { fitPosition, type Fit } from "./fit";
import type { Point } from "./geo";
import { judge, regionRadiusKm, type Judgement } from "./judge";
import { defaultModel, type Model, type Observation } from "./model";
import { countryContains, hasBoundary } from "./polygon";
import { estimateRoundTrip } from "./roundTrip";
import { decide, defaultMaxSigmaKm, type DriftVerdict, type DriftVerdictReason } from "./verdict";

/** E5: an extender is usable in a run with this many round trips, 2 s of pings (12 before 2026-11-17). */
export const driftUsableRoundTrips = 8;
/** E5, V7: a run counts with this many usable extenders. */
export const driftMinRunExtenders = 6;
/** V7: the verdict pools this many of the last counting runs. */
export const driftWindowRuns = 3;

/** One extender's round trips in one run the frame measured. */
export interface DriftRunExtender {
  extenderId: string;
  /** the extender's place, from `start` */
  at: Point;
  /** s_ext (V3), km; the model takes a derived place's 50 km when unknown */
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

/** E5: a run's observations, one per extender with 8 round trips or more. */
export function driftRunObservations(run: DriftRun): Observation[] {
  const observations: Observation[] = [];
  for (const extender of run.extenders) {
    const roundTrip = estimateRoundTrip(extender.roundTripsMs);
    if (roundTrip !== null && driftUsableRoundTrips <= roundTrip.count) {
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
  /**
   * V11: the region's k, the demo run's `k` from `start`, or, from a start
   * that lacks it, defaultK(confidence, continentForCountry(the IP country))
   */
  k: number;
  model?: Model;
  /** σ_major beyond which the geometry can't place the visitor, km; default 1,500 */
  maxSigmaKm?: number;
}

/** W8: the demo's verdict and figures. */
export interface LocalDriftVerdict {
  verdict: DriftVerdict;
  reason: DriftVerdictReason;
  /** the runs pooled, oldest first */
  runsInVerdict: number[];
  /** the region's k judged at (V11) */
  k: number;
  ipCountryCode: string;
  /**
   * V6: the country containing the fit (the IP country when it is one of
   * several, as in a disputed area), else the nearest within 300 km, else ""
   * at sea
   */
  driftCountryCode: string;
  /** V6's figures toward the IP country's nearest border point, km; null without a fit and the country */
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
 * decides the fit against the simplified map as the api decides a real
 * token's (V6, V5). The physical check turns an agree into
 * `inconclusive`/`inconsistent`; it never rejects.
 */
export function localDriftVerdict(options: LocalDriftVerdictOptions): LocalDriftVerdict {
  const k = options.k;
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
    k,
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
  const country = ipCountryCode === "" ? null : options.borders.country(ipCountryCode);
  const decision = decide(fit, country, observations, k, options.maxSigmaKm ?? defaultMaxSigmaKm);
  result.verdict = decision.verdict;
  result.reason = decision.reason;
  if (fit !== null) {
    result.fit = fit;
    // V6: a disputed area belongs to every claimant, so where the fit lies in
    // several countries the drift country is the IP country if it is one of
    // them, else the first by code, as countryAt gives
    result.driftCountryCode =
      country !== null && countryContains(country, fit.at)
        ? country.code
        : options.borders.countryAt(fit.at, driftCountryWithinKm);
    result.plusMinusKm = regionRadiusKm(fit, k);
    // the figures, even where the verdict needed none
    const judgement = decision.judgement ?? (country !== null && hasBoundary(country) ? judge(fit, country, k) : null);
    if (judgement !== null) {
      result.judgement = judgement;
      result.distanceKm = judgement.distanceKm;
      result.significanceKm = judgement.significanceKm;
      result.marginKm = judgement.marginKm;
    }
  }
  return result;
}
