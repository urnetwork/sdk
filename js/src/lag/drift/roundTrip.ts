// V1: one extender's round trip in a run (lag.dev/DRIFTTEST.md V1). Queueing
// only ever adds to a ping's round trip, so the fit takes each extender's
// 10th percentile rather than its median, with a distribution-free error from
// order statistics. Deterministic, with no resampling, so the Go model and
// this one agree exactly (T2).

/** V1: the quantile of an extender's round trips the fit uses. */
export const roundTripQuantile = 0.1;

/** The fewest round trips an estimate is made from. */
export const minRoundTripSamples = 4;

/** The z of a two-sided 95% interval. */
const Z95 = 1.96;

/** The bounds the autocorrelation time τ is held to (V1). */
const MIN_TAU = 1;
const MAX_TAU = 8;

/** One extender's round trip in one run. */
export interface RoundTrip {
  /** the 10th percentile of the run's round trips, ms */
  ms: number;
  /** its standard error, ms (σ_meas, V1) */
  sigmaMs: number;
  /** the round trips it was estimated from */
  count: number;
}

/**
 * The autocorrelation time τ = (1 + ρ) / (1 − ρ) of the pings in the order
 * they were sent, from their lag-1 autocorrelation ρ, held to [1, 8]. Pings
 * 250 ms apart are correlated, and counting them as independent would
 * understate the error: the effective sample size is n / τ.
 */
export function autocorrelationTime(samplesMs: readonly number[]): number {
  const n = samplesMs.length;
  if (n < 2) {
    return MIN_TAU;
  }
  let sum = 0;
  for (const x of samplesMs) {
    sum += x;
  }
  const mean = sum / n;
  let num = 0;
  let den = 0;
  for (let i = 0; i < n; i++) {
    const d = samplesMs[i] - mean;
    den += d * d;
    if (i + 1 < n) {
      num += d * (samplesMs[i + 1] - mean);
    }
  }
  if (!(den > 0)) {
    return MIN_TAU;
  }
  const rho = num / den;
  if (!(rho < 1)) {
    return MAX_TAU;
  }
  const tau = (1 + rho) / (1 - rho);
  return Math.min(MAX_TAU, Math.max(MIN_TAU, tau));
}

/**
 * V1: the 10th percentile of a run's round trips to one extender, by nearest
 * rank, and its standard error. A 95% interval for the quantile is
 * [x₍ₗ₎, x₍ᵤ₎], with ranks from the binomial at the effective sample size
 * n / τ, so σ = (x₍ᵤ₎ − x₍ₗ₎) / (2 × 1.96). With few round trips the
 * interval's lower rank falls below the first, leaving only its upper half,
 * which is doubled.
 *
 * `samplesMs` are the round trips in the order they were sent, lost pings
 * left out. `null` below 4 of them.
 */
export function estimateRoundTrip(samplesMs: readonly number[]): RoundTrip | null {
  const samples = samplesMs.filter((x) => Number.isFinite(x));
  const n = samples.length;
  if (n < minRoundTripSamples) {
    return null;
  }
  const tau = autocorrelationTime(samples);
  const sorted = samples.slice().sort((a, b) => a - b);
  const at = (rank: number) => sorted[Math.min(n, Math.max(1, rank)) - 1];
  const center = roundTripQuantile * n;
  const ms = at(Math.ceil(center));
  const half = Z95 * Math.sqrt(n * roundTripQuantile * (1 - roundTripQuantile) * tau);
  const lower = Math.floor(center - half);
  const upper = Math.ceil(center + half);
  const width = lower >= 1 ? at(upper) - at(lower) : 2 * (at(upper) - ms);
  return { ms, sigmaMs: width / (2 * Z95), count: n };
}
