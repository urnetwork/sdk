// Percentiles, pings and throughput for lag.dev (lag.dev/PLAN.md S5).
//
// A session pings for as long as the page is open, so every structure here
// keeps bounded memory however long its stream gets. Percentiles are nearest
// rank: exact over a stream of at most `exactLimit` samples or over a time
// range of the one-minute ping window, otherwise read from a log-bucket
// histogram whose relative error is at most `relativeAccuracy`.

export interface LagPercentiles {
  p50: number;
  p95: number;
}

// The nearest rank of percentile p among n values, 1-based. p * n is an
// integer for integer p, so the division only rounds when the true rank is
// fractional, and ceil then lands on the right integer.
const rankOf = (p: number, n: number): number => {
  if (!(0 < p && p <= 100)) {
    throw new RangeError(`percentile must be in (0, 100], got ${p}`);
  }
  return Math.min(n, Math.max(1, Math.ceil((p * n) / 100)));
};

/**
 * Nearest rank: the smallest value such that at least p% of the values are at
 * most it. With 4 samples, p50 is the second smallest and p95 the largest.
 * `undefined` for no values.
 */
export function nearestRank(values: ArrayLike<number>, p: number): number | undefined {
  if (values.length === 0) {
    rankOf(p, 1);
    return undefined;
  }
  const sorted = Array.from(values).sort((a, b) => a - b);
  return sorted[rankOf(p, sorted.length) - 1];
}

/** p50 and p95 by nearest rank, or `undefined` for no values. */
export function percentiles(values: ArrayLike<number>): LagPercentiles | undefined {
  if (values.length === 0) {
    return undefined;
  }
  const sorted = Array.from(values).sort((a, b) => a - b);
  return {
    p50: sorted[rankOf(50, sorted.length) - 1],
    p95: sorted[rankOf(95, sorted.length) - 1],
  };
}

/** Megabits per second for `bytes` moved in `ms` milliseconds. */
export function bytesToMbps(bytes: number, ms: number): number {
  return ms <= 0 ? 0 : (bytes * 8) / (ms * 1000);
}

export interface LagHistogramOptions {
  /**
   * The largest relative error of a percentile read from buckets
   * (default 0.005). Exact values never have an error.
   */
  relativeAccuracy?: number;
  /** values kept exactly before the histogram switches to buckets (default 1024) */
  exactLimit?: number;
}

const DEFAULT_RELATIVE_ACCURACY = 0.005;
const DEFAULT_EXACT_LIMIT = 1024;
// Values below MIN_TRACKED share the zero bucket and values above MAX_TRACKED
// share the top one, so the bucket count is bounded (about 3,500 at 0.5%)
// whatever arrives. The tracked min and max stay exact.
const MIN_TRACKED = 1e-6;
const MAX_TRACKED = 1e9;

/**
 * A stream of non-negative samples (milliseconds, Mbps) with nearest-rank
 * percentiles in bounded memory.
 *
 * The first `exactLimit` samples are kept sorted, so a short stream has exact
 * percentiles. Past that the samples move into logarithmic buckets: bucket i
 * holds (γ^(i-1), γ^i] with γ = (1+α)/(1-α), and a percentile is the bucket's
 * midpoint 2γ^i/(γ+1), within α of every value in it. A rank that falls on
 * the smallest or largest sample answers it exactly.
 */
export class LagHistogram {
  readonly relativeAccuracy: number;
  readonly exactLimit: number;
  private readonly logGamma: number;
  private readonly midpointScale: number;
  // ascending; null once the stream outgrew exactLimit
  private exact: number[] | null = [];
  private readonly buckets = new Map<number, number>();
  private sortedKeys: number[] | null = null;
  private zeros = 0;
  private n = 0;
  private lo = Infinity;
  private hi = -Infinity;
  private total = 0;

  constructor(options: LagHistogramOptions = {}) {
    const alpha = options.relativeAccuracy ?? DEFAULT_RELATIVE_ACCURACY;
    if (!(0 < alpha && alpha < 1)) {
      throw new RangeError(`relativeAccuracy must be in (0, 1), got ${alpha}`);
    }
    const exactLimit = options.exactLimit ?? DEFAULT_EXACT_LIMIT;
    if (!(Number.isInteger(exactLimit) && 0 <= exactLimit)) {
      throw new RangeError(`exactLimit must be a non-negative integer, got ${exactLimit}`);
    }
    this.relativeAccuracy = alpha;
    this.exactLimit = exactLimit;
    const gamma = (1 + alpha) / (1 - alpha);
    this.logGamma = Math.log(gamma);
    this.midpointScale = 2 / (gamma + 1);
  }

  get count(): number {
    return this.n;
  }

  get min(): number | undefined {
    return this.n === 0 ? undefined : this.lo;
  }

  get max(): number | undefined {
    return this.n === 0 ? undefined : this.hi;
  }

  get mean(): number | undefined {
    return this.n === 0 ? undefined : this.total / this.n;
  }

  /** Whether percentiles are still exact (the stream has not outgrown exactLimit). */
  get exactPercentiles(): boolean {
    return this.exact !== null;
  }

  /**
   * Add one sample. A value that is not a finite number at least 0 has no
   * place in the ranks and is ignored (returns false).
   */
  add(value: number): boolean {
    if (!(Number.isFinite(value) && 0 <= value)) {
      return false;
    }
    this.n += 1;
    this.total += value;
    if (value < this.lo) {
      this.lo = value;
    }
    if (this.hi < value) {
      this.hi = value;
    }
    const exact = this.exact;
    if (exact !== null) {
      if (exact.length < this.exactLimit) {
        exact.splice(upperBound(exact, 0, exact.length, value), 0, value);
        return true;
      }
      for (const v of exact) {
        this.addToBucket(v);
      }
      this.exact = null;
    }
    this.addToBucket(value);
    return true;
  }

  /** The nearest-rank percentile p in (0, 100], or `undefined` when empty. */
  percentile(p: number): number | undefined {
    if (this.n === 0) {
      rankOf(p, 1);
      return undefined;
    }
    const rank = rankOf(p, this.n);
    if (this.exact !== null) {
      return this.exact[rank - 1];
    }
    if (rank === 1) {
      return this.lo;
    }
    if (rank === this.n) {
      return this.hi;
    }
    let seen = this.zeros;
    if (rank <= seen) {
      return this.lo;
    }
    for (const key of this.keys()) {
      seen += this.buckets.get(key)!;
      if (rank <= seen) {
        const midpoint = Math.exp(key * this.logGamma) * this.midpointScale;
        return Math.min(this.hi, Math.max(this.lo, midpoint));
      }
    }
    return this.hi;
  }

  /** p50 and p95, or `undefined` when empty. */
  percentiles(): LagPercentiles | undefined {
    if (this.n === 0) {
      return undefined;
    }
    return { p50: this.percentile(50)!, p95: this.percentile(95)! };
  }

  /**
   * Add every sample of another histogram with the same relative accuracy,
   * e.g. to combine per-window histograms.
   */
  merge(other: LagHistogram): void {
    if (other.relativeAccuracy !== this.relativeAccuracy) {
      throw new RangeError("merge needs histograms with the same relativeAccuracy");
    }
    if (other.n === 0) {
      return;
    }
    if (other.exact !== null) {
      for (const v of other.exact) {
        this.add(v);
      }
      return;
    }
    if (this.exact !== null) {
      for (const v of this.exact) {
        this.addToBucket(v);
      }
      this.exact = null;
    }
    this.n += other.n;
    this.total += other.total;
    this.lo = Math.min(this.lo, other.lo);
    this.hi = Math.max(this.hi, other.hi);
    this.zeros += other.zeros;
    for (const [key, count] of other.buckets) {
      this.addBucketCount(key, count);
    }
  }

  reset(): void {
    this.exact = [];
    this.buckets.clear();
    this.sortedKeys = null;
    this.zeros = 0;
    this.n = 0;
    this.lo = Infinity;
    this.hi = -Infinity;
    this.total = 0;
  }

  private addToBucket(value: number): void {
    if (value < MIN_TRACKED) {
      this.zeros += 1;
      return;
    }
    const key = Math.ceil(Math.log(Math.min(value, MAX_TRACKED)) / this.logGamma);
    this.addBucketCount(key, 1);
  }

  private addBucketCount(key: number, count: number): void {
    const previous = this.buckets.get(key);
    if (previous === undefined) {
      this.sortedKeys = null;
    }
    this.buckets.set(key, (previous ?? 0) + count);
  }

  private keys(): number[] {
    if (this.sortedKeys === null) {
      this.sortedKeys = Array.from(this.buckets.keys()).sort((a, b) => a - b);
    }
    return this.sortedKeys;
  }
}

// The first index in sorted[from, to) whose value is above `value`: inserting
// there keeps the array ascending, after any equal values.
const upperBound = (sorted: readonly number[], from: number, to: number, value: number): number => {
  let lo = from;
  let hi = to;
  while (lo < hi) {
    const mid = (lo + hi) >>> 1;
    if (sorted[mid] <= value) {
      lo = mid + 1;
    } else {
      hi = mid;
    }
  }
  return lo;
};

// The first index in sorted[from, to) whose value is at least `value`.
const lowerBound = (sorted: readonly number[], from: number, to: number, value: number): number => {
  let lo = from;
  let hi = to;
  while (lo < hi) {
    const mid = (lo + hi) >>> 1;
    if (sorted[mid] < value) {
      lo = mid + 1;
    } else {
      hi = mid;
    }
  }
  return lo;
};


/**
 * One ping: `t` is when it was sent, in milliseconds on the caller's time
 * axis, and `ms` its round trip, or null when it was lost.
 */
export interface LagPing {
  t: number;
  ms: number | null;
}

/**
 * A load on the pings' time axis, covering [start, end); `end` is null while
 * it runs. `index` names the loading dot, so a dot can tell its own load
 * from its neighbours'.
 */
export interface LagLoadWindow {
  start: number;
  end: number | null;
  index?: number;
}

/**
 * What a ping saw (lag.dev/PLAN.md L4): `idle` when no load in the track was
 * running, `loaded` when one was, and `own` when it was the dot's own.
 */
export type LagPingKind = "idle" | "loaded" | "own";

const covers = (window: LagLoadWindow, t: number): boolean =>
  window.start <= t && (window.end === null || t < window.end);

/** The load running at `t`, or undefined when the track was idle. */
export function loadWindowAt(
  t: number,
  windows: readonly LagLoadWindow[],
): LagLoadWindow | undefined {
  return windows.find((window) => covers(window, t));
}

/** Classify a ping sent at `t` against the track's loads, for the dot `ownIndex`. */
export function classifyPing(
  t: number,
  windows: readonly LagLoadWindow[],
  ownIndex?: number,
): LagPingKind {
  let kind: LagPingKind = "idle";
  for (const window of windows) {
    if (covers(window, t)) {
      if (ownIndex !== undefined && window.index === ownIndex) {
        return "own";
      }
      kind = "loaded";
    }
  }
  return kind;
}

export interface LagPingSplit {
  /** round trips while no load ran */
  idle: number[];
  /** round trips while any load ran, the dot's own included */
  loaded: number[];
  /** round trips during the dot's own load */
  own: number[];
  /** lost pings, of every kind */
  lost: number;
}

/** Split pings into idle and loaded round trips by the track's load windows. */
export function splitPings(
  pings: Iterable<LagPing>,
  windows: readonly LagLoadWindow[],
  ownIndex?: number,
): LagPingSplit {
  const split: LagPingSplit = { idle: [], loaded: [], own: [], lost: 0 };
  for (const ping of pings) {
    if (ping.ms === null) {
      split.lost += 1;
      continue;
    }
    const kind = classifyPing(ping.t, windows, ownIndex);
    if (kind === "idle") {
      split.idle.push(ping.ms);
    } else {
      split.loaded.push(ping.ms);
      if (kind === "own") {
        split.own.push(ping.ms);
      }
    }
  }
  return split;
}

export interface LagPingWindowOptions {
  /** history kept back from the newest ping, in milliseconds (default 60 s) */
  windowMs?: number;
  /**
   * The most pings kept, oldest dropped first (default 1024). Four a second
   * for a minute is 240.
   */
  capacity?: number;
}


/**
 * The last minute of a session's pings, ordered by send time, in bounded
 * memory. Ranges are [from, to) on the pings' time axis, and their
 * percentiles are exact. The window follows the newest ping, so a session
 * that stops keeps its last minute as it ended.
 */
export class LagPingWindow {
  readonly windowMs: number;
  readonly capacity: number;
  // parallel arrays ascending by send time; live from `first`
  private times: number[] = [];
  private rtts: (number | null)[] = [];
  private first = 0;

  constructor(options: LagPingWindowOptions = {}) {
    const windowMs = options.windowMs ?? 60_000;
    if (!(0 < windowMs)) {
      throw new RangeError(`windowMs must be positive, got ${windowMs}`);
    }
    const capacity = options.capacity ?? 1024;
    if (!(Number.isInteger(capacity) && 1 <= capacity)) {
      throw new RangeError(`capacity must be a positive integer, got ${capacity}`);
    }
    this.windowMs = windowMs;
    this.capacity = capacity;
  }

  get size(): number {
    return this.times.length - this.first;
  }

  /** The newest ping's send time, or undefined when empty. */
  get newest(): number | undefined {
    return this.size === 0 ? undefined : this.times[this.times.length - 1];
  }

  /** The oldest kept ping's send time, or undefined when empty. */
  get oldest(): number | undefined {
    return this.size === 0 ? undefined : this.times[this.first];
  }

  /**
   * Add a ping. Pings normally arrive in send order; one that arrives late
   * is placed by its time, and one already older than the window is
   * dropped, as is a ping without a finite time or a valid round trip.
   */
  add(ping: LagPing): boolean {
    const { t, ms } = ping;
    if (!Number.isFinite(t) || !(ms === null || (Number.isFinite(ms) && 0 <= ms))) {
      return false;
    }
    const newest = this.newest;
    if (newest === undefined || newest <= t) {
      this.times.push(t);
      this.rtts.push(ms);
    } else {
      if (t < newest - this.windowMs) {
        return false;
      }
      const index = upperBound(this.times, this.first, this.times.length, t);
      this.times.splice(index, 0, t);
      this.rtts.splice(index, 0, ms);
    }
    const horizon = this.times[this.times.length - 1] - this.windowMs;
    while (this.first < this.times.length && this.times[this.first] < horizon) {
      this.first += 1;
    }
    if (this.capacity < this.size) {
      this.first = this.times.length - this.capacity;
    }
    // compact once the dead prefix is as long as the live part
    if (64 <= this.first && this.size <= this.first) {
      this.times = this.times.slice(this.first);
      this.rtts = this.rtts.slice(this.first);
      this.first = 0;
    }
    return true;
  }

  /** The pings sent in [from, to), oldest first (a copy). */
  pings(from = -Infinity, to = Infinity): LagPing[] {
    const [start, end] = this.range(from, to);
    const out: LagPing[] = [];
    for (let i = start; i < end; i += 1) {
      out.push({ t: this.times[i], ms: this.rtts[i] });
    }
    return out;
  }

  /** The round trips of the pings sent in [from, to), lost pings left out. */
  roundTrips(from = -Infinity, to = Infinity): number[] {
    const [start, end] = this.range(from, to);
    const out: number[] = [];
    for (let i = start; i < end; i += 1) {
      const ms = this.rtts[i];
      if (ms !== null) {
        out.push(ms);
      }
    }
    return out;
  }

  /** p50 and p95 by nearest rank over [from, to), or undefined without a round trip. */
  percentiles(from = -Infinity, to = Infinity): LagPercentiles | undefined {
    return percentiles(this.roundTrips(from, to));
  }

  /** Lost pings sent in [from, to). */
  lost(from = -Infinity, to = Infinity): number {
    const [start, end] = this.range(from, to);
    let lost = 0;
    for (let i = start; i < end; i += 1) {
      if (this.rtts[i] === null) {
        lost += 1;
      }
    }
    return lost;
  }

  clear(): void {
    this.times = [];
    this.rtts = [];
    this.first = 0;
  }

  private range(from: number, to: number): [number, number] {
    const start = lowerBound(this.times, this.first, this.times.length, from);
    const end = lowerBound(this.times, start, this.times.length, to);
    return [start, end];
  }
}

/**
 * A session's round trips over its whole life, by kind, in bounded memory.
 * Classify each ping as it arrives against the load windows known by then:
 * a load's window opens before any ping sent during it can be reported.
 */
export class LagLatencyStats {
  /** pings while no load in the track ran */
  readonly idle: LagHistogram;
  /** pings while any load ran, the dot's own included */
  readonly loaded: LagHistogram;
  /** pings during the dot's own load */
  readonly own: LagHistogram;
  private lostCount = 0;

  constructor(options?: LagHistogramOptions) {
    this.idle = new LagHistogram(options);
    this.loaded = new LagHistogram(options);
    this.own = new LagHistogram(options);
  }

  /** Lost pings so far. */
  get lost(): number {
    return this.lostCount;
  }

  /** Own-load p50 less idle p50, once both have a sample. */
  get loadIncreaseMs(): number | undefined {
    const own = this.own.percentile(50);
    const idle = this.idle.percentile(50);
    return own === undefined || idle === undefined ? undefined : own - idle;
  }

  add(ping: LagPing, windows: readonly LagLoadWindow[], ownIndex?: number): LagPingKind | "lost" {
    if (ping.ms === null) {
      this.lostCount += 1;
      return "lost";
    }
    const kind = classifyPing(ping.t, windows, ownIndex);
    if (kind === "idle") {
      this.idle.add(ping.ms);
    } else {
      this.loaded.add(ping.ms);
      if (kind === "own") {
        this.own.add(ping.ms);
      }
    }
    return kind;
  }
}

export interface LagIntervalSeriesOptions {
  /** default 250 */
  intervalMs?: number;
  /**
   * Intervals kept (default 40, the most A7 accepts per direction). Bytes
   * past them count toward the total only.
   */
  maxIntervals?: number;
}

/**
 * One direction of a load, as bytes per interval from the direction's first
 * byte (lag.dev/PLAN.md M4, M8). Feed it exact arrivals (addAt, e.g. each
 * download chunk as it is read) or cumulative counts (addCumulative, e.g. the
 * extender's `uploaded` reports), whose bytes spread evenly over the time
 * since the previous count, so a late report splits across intervals in
 * proportion to time.
 *
 * An interval counts once at least half of it has passed by the last byte,
 * so a load cut short by a cap does not end on a part-empty interval.
 */
export class LagIntervalSeries {
  readonly intervalMs: number;
  readonly maxIntervals: number;
  private readonly sums: number[] = [];
  private total = 0;
  private endMs = 0;
  private cumulative = 0;
  private cumulativeMs = 0;

  constructor(options: LagIntervalSeriesOptions = {}) {
    const intervalMs = options.intervalMs ?? 250;
    if (!(Number.isFinite(intervalMs) && 0 < intervalMs)) {
      throw new RangeError(`intervalMs must be positive, got ${intervalMs}`);
    }
    const maxIntervals = options.maxIntervals ?? 40;
    if (!(Number.isInteger(maxIntervals) && 0 <= maxIntervals)) {
      throw new RangeError(`maxIntervals must be a non-negative integer, got ${maxIntervals}`);
    }
    this.intervalMs = intervalMs;
    this.maxIntervals = maxIntervals;
  }

  /** Every byte added, including any past the kept intervals. */
  get totalBytes(): number {
    return this.total;
  }

  /** When the last byte came, in ms after the first. */
  get durationMs(): number {
    return this.endMs;
  }

  /** The intervals that count so far. */
  get count(): number {
    return Math.min(this.maxIntervals, Math.round(this.endMs / this.intervalMs));
  }

  /** Bytes per counted interval. */
  get bytes(): number[] {
    const out: number[] = [];
    for (let i = 0; i < this.count; i += 1) {
      out.push(this.sums[i] ?? 0);
    }
    return out;
  }

  /** Mbps per counted interval. */
  get mbps(): number[] {
    return this.bytes.map((bytes) => bytesToMbps(bytes, this.intervalMs));
  }

  /** p50 and p95 of the counted intervals' Mbps, or undefined before the first. */
  percentiles(): LagPercentiles | undefined {
    return percentiles(this.mbps);
  }

  /** Bytes in interval `index`, counted or not yet. */
  intervalBytes(index: number): number {
    return this.sums[index] ?? 0;
  }

  /** `bytes` that arrived `ms` after the first byte. */
  addAt(ms: number, bytes: number): void {
    if (!Number.isFinite(ms)) {
      throw new RangeError(`ms must be finite, got ${ms}`);
    }
    if (!(Number.isFinite(bytes) && 0 <= bytes)) {
      throw new RangeError(`bytes must be a finite number at least 0, got ${bytes}`);
    }
    const at = Math.max(0, ms);
    this.total += bytes;
    this.endMs = Math.max(this.endMs, at);
    this.addToInterval(Math.floor(at / this.intervalMs), bytes);
  }

  /**
   * A cumulative byte count `ms` after the first byte; the count is 0 at
   * ms 0, and a count lower than an earlier one adds nothing.
   */
  addCumulative(ms: number, totalBytes: number): void {
    if (!Number.isFinite(ms) || !Number.isFinite(totalBytes)) {
      throw new RangeError(`ms and totalBytes must be finite, got ${ms} and ${totalBytes}`);
    }
    const bytes = Math.max(0, totalBytes - this.cumulative);
    this.cumulative = Math.max(this.cumulative, totalBytes);
    const from = this.cumulativeMs;
    const to = Math.max(from, ms);
    this.cumulativeMs = to;
    this.total += bytes;
    this.endMs = Math.max(this.endMs, to);
    if (bytes === 0) {
      return;
    }
    const interval = this.intervalMs;
    if (to === from) {
      // no time passed: the bytes close the interval that ends at `to`
      this.addToInterval(Math.max(0, Math.ceil(to / interval) - 1), bytes);
      return;
    }
    for (let i = Math.floor(from / interval); i * interval < to && i < this.maxIntervals; i += 1) {
      const overlap = Math.min(to, (i + 1) * interval) - Math.max(from, i * interval);
      if (0 < overlap) {
        this.addToInterval(i, (bytes * overlap) / (to - from));
      }
    }
  }

  private addToInterval(index: number, bytes: number): void {
    if (index < 0 || this.maxIntervals <= index) {
      return;
    }
    while (this.sums.length <= index) {
      this.sums.push(0);
    }
    this.sums[index] += bytes;
  }
}
