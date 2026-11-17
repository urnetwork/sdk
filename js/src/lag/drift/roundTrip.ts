// V1: one extender's round trip in a run (lag.dev/DRIFTTEST.md V1), as
// connect's lagdrift computes it (roundtrip.go). Queueing only ever adds to a
// ping's round trip, so the fit takes each extender's 10th percentile, not its
// median, with a distribution-free error from order statistics.
// Deterministic, with no resampling, so the Go and this agree exactly (T2).

/** V1: the quantile of an extender's round trips the fit uses. */
export const roundTripQuantile = 0.1;

/** Fewer samples have no estimate. */
export const minRoundTripSamples = 4;

// the round trip's 95% interval is ±1.96 standard errors
const roundTripZ = 1.96;
// the autocorrelation's inflation of the variance is held to [1, 8]
const maxRoundTripTau = 8;

/** One extender's round trip over a run (V1). */
export interface RoundTrip {
  /** the 10th percentile, by nearest rank, ms */
  ms: number;
  /** its standard error, ms */
  sigmaMs: number;
  /** the samples it is from */
  count: number;
  /**
   * the lowest sample, ms, for the physical check (V5): an extension of I3,
   * 0 when unknown, and the check then uses `ms`
   */
  minMs: number;
}

/**
 * The 10th percentile of a run's round trips to one extender and its
 * distribution-free standard error (V1). `samplesMs` are in arrival order,
 * which the autocorrelation needs. A sample that is not a positive finite
 * number (a lost ping) is left out, and `null` means fewer than 4 remain.
 *
 * The estimate is the sample of rank ⌈0.1 n⌉. Its 95% interval is
 * [x₍ₗ₎, x₍ᵤ₎] with l = ⌊0.1 n − h⌋ and u = ⌈0.1 n + h⌉, where
 * h = 1.96 √(n · 0.1 · 0.9 · τ): the binomial's ranks at the effective sample
 * size n / τ, mapped back onto the n samples. τ = (1 + ρ) / (1 − ρ), from the
 * lag-1 autocorrelation ρ, held to [1, 8]. The standard error is
 * (x₍ᵤ₎ − x₍ₗ₎) / (2 · 1.96), with u held to n. When l falls below the first
 * rank, the interval's upper half is doubled: (2 (x₍ᵤ₎ − x₍ₖ₎)) / (2 · 1.96).
 */
export function estimateRoundTrip(samplesMs: readonly number[]): RoundTrip | null {
  const xs: number[] = [];
  for (const x of samplesMs) {
    if (0 < x && x !== Infinity) {
      xs.push(x);
    }
  }
  const n = xs.length;
  if (n < minRoundTripSamples) {
    return null;
  }
  const tau = roundTripTau(xs);

  const sorted = xs.slice().sort((a, b) => a - b);
  const at = (rank: number) => sorted[Math.min(Math.max(rank, 1), n) - 1];
  const center = n * roundTripQuantile;
  // q n is an integer for every n a multiple of 10; the guard keeps its
  // rounding from moving the rank up
  const rank = Math.max(Math.ceil(center - 1e-9), 1);
  const y = at(rank);
  const half = roundTripZ * Math.sqrt(n * roundTripQuantile * (1 - roundTripQuantile) * tau);
  const lo = Math.floor(center - half);
  const hi = Math.ceil(center + half);
  const width = 1 <= lo ? at(hi) - at(lo) : 2 * (at(hi) - y);
  return { ms: y, sigmaMs: width / (2 * roundTripZ), count: n, minMs: sorted[0] };
}

/**
 * τ = (1 + ρ) / (1 − ρ) from the lag-1 autocorrelation ρ of the samples in
 * order, held to [1, 8]. Constant samples have ρ = 0. Pings 250 ms apart are
 * correlated, and counting them as independent would understate the error.
 */
export function roundTripTau(xs: readonly number[]): number {
  const n = xs.length;
  let sum = 0;
  for (const x of xs) {
    sum += x;
  }
  const mean = sum / n;
  let num = 0;
  let den = 0;
  for (let i = 0; i < n; i++) {
    const d = xs[i] - mean;
    den += d * d;
    if (i + 1 < n) {
      num += d * (xs[i + 1] - mean);
    }
  }
  if (!(0 < den)) {
    return 1;
  }
  const rho = num / den;
  if (1 <= rho) {
    return maxRoundTripTau;
  }
  const tau = (1 + rho) / (1 - rho);
  return Math.max(1, Math.min(maxRoundTripTau, tau));
}
