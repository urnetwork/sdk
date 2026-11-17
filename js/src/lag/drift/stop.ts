// V10: stopping early (lag.dev/DRIFTTEST.md V10). The ellipse is set by how
// many paths there are, not by how many round trips: once each extender has
// a few, its route and place error dominates and doesn't average away. So a
// run stops as soon as more pinging can't change the answer. The frame
// evaluates the rules on its own fit after every ping round (250 ms), from
// 3 s after the sessions open; the server checks the stop it reports against
// the signed reports, with a wider tolerance.

import type { Fit } from "./fit";
import { judge } from "./judge";
import { earlyK } from "./model";
import type { Country } from "./polygon";

/** One look at a run: its fit then, and each extender's round trips so far. */
export interface Snapshot {
  atMs: number;
  fit: Fit | null;
  /** round trips so far, per extender */
  roundTrips: number[];
}

/** V10, and `finish`'s `stop` (P5). */
export type StopReason = "converged" | "clear" | "interrupted" | "cap";

/** How far the area σ_major · σ_minor may stray over the last 2 s, as max / min. */
export interface StopTolerance {
  areaRatio: number;
}

/** The browser's tolerance: ±5%, 1.05 / 0.95. */
export const browserStopTolerance: Readonly<StopTolerance> = Object.freeze({ areaRatio: 1.105 });
/** The server's, for the browser's and the extender's clocks seeing the pings slightly apart: ±8%. */
export const serverStopTolerance: Readonly<StopTolerance> = Object.freeze({ areaRatio: 1.17 });

/** No stop is decided before the sessions have been open this long, ms. */
export const stopFromMs = 3000;
/** `cap`: a run ends this long after its sessions open, ms. */
export const stopCapMs = 12000;
/** `interrupted`: no pong from any extender for this long, ms. */
export const stopSilenceMs = 4000;
/** `converged`: the window the ellipse must hold steady over, ms. */
export const stopSteadyMs = 2000;
/** both stops need this many extenders ... */
export const stopMinExtenders = 8;
/** ... with this many round trips each */
export const stopMinRoundTrips = 12;

function enoughRoundTrips(snapshot: Snapshot): boolean {
  let extenders = 0;
  for (const count of snapshot.roundTrips) {
    if (count >= stopMinRoundTrips) {
      extenders++;
    }
  }
  return extenders >= stopMinExtenders;
}

/**
 * Over the last 2 s, from the last look at or before its start, every look
 * has a fit, the area σ_major · σ_minor stays within the tolerance, and the
 * position stays within a quarter of the latest σ_minor of the latest.
 */
function steady(history: readonly Snapshot[], latest: number, nowMs: number, tol: StopTolerance): boolean {
  const from = nowMs - stopSteadyMs;
  let start = -1;
  for (let i = latest; i >= 0; i--) {
    if (history[i].atMs <= from) {
      start = i;
      break;
    }
  }
  if (start < 0) {
    return false;
  }
  const last = history[latest].fit!;
  const reach = 0.25 * last.minorKm;
  let low = Infinity;
  let high = 0;
  for (let i = start; i <= latest; i++) {
    const fit = history[i].fit;
    if (fit === null) {
      return false;
    }
    const area = fit.majorKm * fit.minorKm;
    low = Math.min(low, area);
    high = Math.max(high, area);
    if (Math.hypot(fit.eastKm - last.eastKm, fit.northKm - last.northKm) >= reach) {
      return false;
    }
  }
  return low > 0 && high / low <= tol.areaRatio;
}

/**
 * V10: whether a run stops now, and why. `history` is the run's looks in
 * time order, the last at or before `nowMs`; `openedMs` is when its sessions
 * opened and `lastPongMs` the last pong from any extender (any value at or
 * before `openedMs` for none). `country` is the IP country, for `clear`.
 * `null` while the run goes on.
 */
export function evaluateStop(
  history: readonly Snapshot[],
  openedMs: number,
  nowMs: number,
  lastPongMs: number,
  country: Country | null,
  tol: StopTolerance = browserStopTolerance,
): StopReason | null {
  const elapsed = nowMs - openedMs;
  if (elapsed < stopFromMs) {
    return null;
  }
  if (nowMs - Math.max(lastPongMs, openedMs) >= stopSilenceMs) {
    return "interrupted";
  }
  let latest = history.length - 1;
  while (latest >= 0 && history[latest].atMs > nowMs) {
    latest--;
  }
  const snapshot = latest >= 0 ? history[latest] : null;
  if (snapshot !== null && snapshot.fit !== null && enoughRoundTrips(snapshot)) {
    // out: the whole k_early region is outside the country; in: the fit is
    // inside and so is its whole k_early region. Either way, D ≥ k_early.
    if (country !== null && judge(snapshot.fit, country).d >= earlyK) {
      return "clear";
    }
    if (steady(history, latest, nowMs, tol)) {
      return "converged";
    }
  }
  if (elapsed >= stopCapMs) {
    return "cap";
  }
  return null;
}
