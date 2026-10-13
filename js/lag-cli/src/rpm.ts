// Responsiveness in round trips per minute (RPM), with the bands of the IETF
// draft "Responsiveness under Working Conditions",
// draft-ietf-ippm-responsiveness-09 (July 2026, not yet an RFC):
//
//   poor       below 300 RPM      (a round trip over 200 ms)
//   fair       300 to 1,000
//   good       1,000 to 6,000
//   excellent  6,000 or more      (10 ms or less)
//
// The draft measures RPM under working conditions, while the link is loaded.
// The CLI also gives an idle RPM, from the idle p50, and says which it is.

export type RpmBand = "poor" | "fair" | "good" | "excellent";

export interface Rpm {
  /** round trips per minute, 60,000 / p50 in ms, rounded */
  value: number;
  band: RpmBand;
}

/** Round trips per minute for a round trip of `ms` milliseconds. */
export function rpmOf(ms: number): number {
  if (!(Number.isFinite(ms) && 0 < ms)) {
    throw new RangeError(`a round trip must be a positive number of ms, got ${ms}`);
  }
  return 60_000 / ms;
}

/** The draft's band for an RPM value. */
export function rpmBand(rpm: number): RpmBand {
  if (rpm < 300) {
    return "poor";
  }
  if (rpm < 1000) {
    return "fair";
  }
  if (rpm < 6000) {
    return "good";
  }
  return "excellent";
}

/** RPM and its band for a p50 round trip, or null without one. */
export function rpmFor(p50Ms: number | null | undefined): Rpm | null {
  if (p50Ms === null || p50Ms === undefined || !(0 < p50Ms)) {
    return null;
  }
  const value = rpmOf(p50Ms);
  return { value: Math.round(value), band: rpmBand(value) };
}
