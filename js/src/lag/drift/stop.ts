// Stopping a run early (lag.dev/DRIFTTEST.md V10), as connect's lagdrift
// rules it (stop.go). The ellipse is set by how many paths there are, not how
// many round trips, so a run stops once more pinging can't change the answer.
// The frame evaluates the rules on its own fit after every ping round
// (250 ms), and the server evaluates the same rules on the signed reports at
// the moment the sessions closed, with wider tolerances.
//
//   - converged: at least 8 extenders have 12 round trips each, and over the
//     trailing 2 s the ellipse's area (σ_major · σ_minor) has stayed within
//     the tolerance's ratio and the position has moved less than a quarter of
//     σ_minor;
//   - clear: the same 12-round-trip minimum, and the whole region at earlyK
//     lies outside the IP country, or the fit is inside it and the whole
//     region lies inside it. Both are D ≥ earlyK (V6);
//   - interrupted: no pong from any extender for 4 s;
//   - cap: 12 s after the sessions opened.
//
// Nothing stops a run in its first 3 s.

import type { Fit } from "./fit";
import { hypot } from "./geo";
import { judgeK } from "./judge";
import { earlyK } from "./model";
import type { Country } from "./polygon";

/** No rule stops a run before this, ms after the sessions opened. */
export const stopEarliestMs = 3000;
/** converged looks back this far, ms. */
export const stopWindowMs = 2000;
/** interrupted: no pong for this long, ms. */
export const stopInterruptedMs = 4000;
/** cap: this long after the sessions opened, ms. */
export const stopCapMs = 12000;
/** converged and clear need this many extenders with `stopRoundTrips` each. */
export const stopExtenders = 8;
export const stopRoundTrips = 12;

/** The run's state after a ping round. */
export interface Snapshot {
  /** when, ms, on the clock of openedMs and nowMs */
  atMs: number;
  /** the fit then, or null before the first */
  fit: Fit | null;
  /** round trips so far, per extender */
  roundTrips: number[];
}

/** Why a run stopped (V10, the `stop` of P5). */
export type StopReason = "converged" | "clear" | "interrupted" | "cap";

/** How far converged lets the ellipse's area move: the largest area over the window divided by the smallest. */
export interface StopTolerance {
  areaRatio: number;
}

/** ±5%: 1.05 / 0.95. */
export const browserStopTolerance: Readonly<StopTolerance> = Object.freeze({ areaRatio: 1.105 });
/** ±8%, since the browser's and the extenders' clocks see the pings slightly apart (V10). */
export const serverStopTolerance: Readonly<StopTolerance> = Object.freeze({ areaRatio: 1.17 });

const stopReasons: readonly StopReason[] = ["interrupted", "clear", "converged", "cap"];

/**
 * The rule that stops the run at nowMs, if one does (V10); `null` while it
 * goes on. `history` is the run's snapshots in time order; those after nowMs
 * are ignored. `lastPongMs` is the last pong from any extender, before which
 * openedMs stands in. `country` is the IP country, null when unknown, which
 * rules out clear. When several rules hold, the first of interrupted, clear,
 * converged and cap is the reason.
 */
export function evaluateStop(
  history: readonly Snapshot[],
  openedMs: number,
  nowMs: number,
  lastPongMs: number,
  country: Country | null,
  tol: StopTolerance = browserStopTolerance,
): StopReason | null {
  for (const reason of stopReasons) {
    if (stopHolds(reason, history, openedMs, nowMs, lastPongMs, country, tol)) {
      return reason;
    }
  }
  return null;
}

/** The rule holds at nowMs, as `evaluateStop` evaluates it: an extension of I3, for checking a reported stop (E5). */
export function stopHolds(
  reason: StopReason,
  history: readonly Snapshot[],
  openedMs: number,
  nowMs: number,
  lastPongMs: number,
  country: Country | null,
  tol: StopTolerance = browserStopTolerance,
): boolean {
  if (nowMs < openedMs + stopEarliestMs) {
    return false;
  }
  // as a Go nil slice: none is empty
  history = history ?? [];
  switch (reason) {
    case "interrupted":
      return Math.max(lastPongMs, openedMs) + stopInterruptedMs <= nowMs;
    case "cap":
      return openedMs + stopCapMs <= nowMs;
    case "clear": {
      const latest = latestSnapshot(history, nowMs);
      if (latest === null || latest.fit === null || country === null || !enoughRoundTrips(latest.roundTrips)) {
        return false;
      }
      return earlyK <= judgeK(latest.fit, country, earlyK).d;
    }
    case "converged": {
      const latest = latestSnapshot(history, nowMs);
      if (latest === null || latest.fit === null || !enoughRoundTrips(latest.roundTrips)) {
        return false;
      }
      return steady(history, nowMs, latest.fit, tol);
    }
    default:
      return false;
  }
}

function latestSnapshot(history: readonly Snapshot[], nowMs: number): Snapshot | null {
  for (let i = history.length - 1; 0 <= i; i -= 1) {
    if (history[i].atMs <= nowMs) {
      return history[i];
    }
  }
  return null;
}

/** at least 8 extenders have 12 round trips each */
function enoughRoundTrips(roundTrips: readonly number[] | null): boolean {
  let count = 0;
  for (const n of roundTrips ?? []) {
    if (stopRoundTrips <= n) {
      count += 1;
    }
  }
  return stopExtenders <= count;
}

/**
 * Over the trailing 2 s, every snapshot has a fit, the largest area over the
 * smallest is within the tolerance, and every position is less than a
 * quarter of the latest σ_minor from the latest. The window reaches back to
 * the last snapshot at or before nowMs − 2 s, so a history shorter than 2 s
 * is never steady.
 */
function steady(history: readonly Snapshot[], nowMs: number, latest: Fit, tol: StopTolerance): boolean {
  let start = -1;
  history.forEach((snapshot, i) => {
    if (snapshot.atMs <= nowMs - stopWindowMs) {
      start = i;
    }
  });
  if (start < 0) {
    return false;
  }
  let minArea = Infinity;
  let maxArea = 0.0;
  const reachKm = latest.minorKm / 4;
  for (let i = start; i < history.length; i++) {
    const snapshot = history[i];
    if (nowMs < snapshot.atMs) {
      break;
    }
    if (snapshot.fit === null) {
      return false;
    }
    const area = snapshot.fit.majorKm * snapshot.fit.minorKm;
    minArea = Math.min(minArea, area);
    maxArea = Math.max(maxArea, area);
    if (!(hypot(snapshot.fit.eastKm - latest.eastKm, snapshot.fit.northKm - latest.northKm) < reachKm)) {
      return false;
    }
  }
  return 0 < minArea && maxArea / minArea <= tol.areaRatio;
}
