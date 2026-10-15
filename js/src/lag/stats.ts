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

  /**
   * Move every kept ping by `deltaMs` on the time axis, e.g. when the
   * session's time origin is found to be earlier than first thought.
   */
  shift(deltaMs: number): void {
    if (!Number.isFinite(deltaMs)) {
      throw new RangeError(`deltaMs must be finite, got ${deltaMs}`);
    }
    for (let i = this.first; i < this.times.length; i += 1) {
      this.times[i] += deltaMs;
    }
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

  /**
   * Add a ping. `settling` marks one the caller saw near a load without it
   * being in the load's windows: in flight while the load set up, between its
   * download and upload, or while what it queued drained. Such a ping counts
   * as neither idle nor loaded, so a network still recovering from a load
   * does not read as idle.
   */
  add(
    ping: LagPing,
    windows: readonly LagLoadWindow[],
    ownIndex?: number,
    settling = false,
  ): LagPingKind | "settling" | "lost" {
    if (ping.ms === null) {
      this.lostCount += 1;
      return "lost";
    }
    const kind = classifyPing(ping.t, windows, ownIndex);
    if (kind === "idle" && settling) {
      return "settling";
    }
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
  /**
   * The window's length from its first byte, ms (default maxIntervals x
   * intervalMs). A read (addRead) counts only the part of its span inside
   * the window, and the window is final once no read can still reach back
   * before its end (lag.dev/PLAN.md M6, amendment 15).
   */
  windowMs?: number;
}

/**
 * Rule S's cap (lag.dev/PLAN.md M6): a read counts as having arrived evenly
 * since its stream's previous read, but over no more than this, so the bytes
 * after a genuine stall are not smeared far back into it. A stream's first
 * group reaches back no further either, and so no read that arrives more
 * than this after the window's end can count (amendment 15). The extender's
 * `lagSpreadMax`.
 */
export const LAG_SPREAD_MAX_MS = 1000;

/**
 * Reads that arrive back to back form one group (lag.dev/PLAN.md M6,
 * amendment 15): a read that arrives within this of the first read of its
 * stream's group joins the group, and its span starts where the group's
 * does. A burst released after a stall then spreads, all of it, over the
 * stall. It is measured from the group's first read, so a steady fast stream
 * is cut into groups this long and never smeared over a long span. The
 * extender's `lagSpreadJoin`.
 */
export const LAG_SPREAD_JOIN_MS = 2;

// One stream's reads, as addRead counts them (lag.dev/PLAN.md M6, amendment
// 15). Times are ms after the window's first byte.
interface LagStreamReads {
  // when its latest read arrived
  last: number;
  // its group: when the group's first read arrived, and where the group's
  // span starts
  groupFirst: number;
  groupFrom: number;
  // its first group, held uncounted until its second begins: its bytes, and
  // when its last read arrived; null once it has counted
  held: { bytes: number; last: number } | null;
  // it has had a read whose span starts at or after the window's end, or it
  // ended: nothing it reads later counts
  done: boolean;
}

// A span's pieces are cut by cumulative rounding to 1/SPREAD_GRAIN byte, as
// the extender cuts whole bytes (M6), so they add up to exactly the span's
// bytes. Every sum of whole bytes and such pieces is then exact (below
// 2^53 / SPREAD_GRAIN, some 8.8 TB), and a running sum's floor never falls a
// byte short from rounding.
const SPREAD_GRAIN = 1024;

/**
 * One direction of a load, as bytes per interval from the direction's first
 * byte (lag.dev/PLAN.md M4, M8). Feed it reads on their streams (addRead,
 * e.g. each download chunk, counted by rule S as amendment 15 extends it,
 * M6), bytes with the span they arrived over (addSpread), exact arrivals
 * (addAt) or cumulative counts (addCumulative, e.g. the extender's
 * `uploaded` reports), whose bytes spread evenly over the time since the
 * previous count. A span splits across the intervals it covers in
 * proportion to time.
 *
 * The series holds fractions, but reads out whole bytes: an interval's bytes
 * are the floor of the running sum through it less the floor through the one
 * before, which is what a `received` report sends (M4).
 *
 * An interval counts once at least half of it has passed by the last byte,
 * so a load cut short by a cap does not end on a part-empty interval.
 */
export class LagIntervalSeries {
  readonly intervalMs: number;
  readonly maxIntervals: number;
  readonly windowMs: number;
  private readonly sums: number[] = [];
  private total = 0;
  private endMs = 0;
  private cumulative = 0;
  private cumulativeMs = 0;
  // per stream, its reads so far (addRead)
  private readonly streams: Array<LagStreamReads | undefined> = [];
  private finishedValue = false;

  constructor(options: LagIntervalSeriesOptions = {}) {
    const intervalMs = options.intervalMs ?? 250;
    if (!(Number.isFinite(intervalMs) && 0 < intervalMs)) {
      throw new RangeError(`intervalMs must be positive, got ${intervalMs}`);
    }
    const maxIntervals = options.maxIntervals ?? 40;
    if (!(Number.isInteger(maxIntervals) && 0 <= maxIntervals)) {
      throw new RangeError(`maxIntervals must be a non-negative integer, got ${maxIntervals}`);
    }
    const windowMs = options.windowMs ?? maxIntervals * intervalMs;
    if (!(Number.isFinite(windowMs) && 0 <= windowMs)) {
      throw new RangeError(`windowMs must be a finite number at least 0, got ${windowMs}`);
    }
    this.intervalMs = intervalMs;
    this.maxIntervals = maxIntervals;
    this.windowMs = windowMs;
  }

  /**
   * Every byte added, including any past the kept intervals, and a read's
   * bytes that fell outside the window.
   */
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

  /** Whole bytes per counted interval: through each, they add up to wholeBytesThrough. */
  get bytes(): number[] {
    const out: number[] = [];
    let sum = 0;
    let before = 0;
    for (let i = 0; i < this.count; i += 1) {
      sum += this.sums[i] ?? 0;
      const through = Math.floor(sum);
      out.push(through - before);
      before = through;
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

  /** Bytes in interval `index`, counted or not yet, as held: a fraction where a span split. */
  intervalBytes(index: number): number {
    return this.sums[index] ?? 0;
  }

  /**
   * The bytes through interval `index`, counted or not yet, as a whole
   * number: the floor of the running sum. It never decreases from one
   * interval to the next, and once no more bytes can reach the intervals
   * through `index`, it is final.
   */
  wholeBytesThrough(index: number): number {
    let sum = 0;
    for (let i = 0; i <= index && i < this.sums.length; i += 1) {
      sum += this.sums[i];
    }
    return Math.floor(sum);
  }

  /**
   * Mark `ms` after the first byte as passed, with no more bytes: a window
   * that closed then counts its last intervals even when they stayed empty.
   */
  extendTo(ms: number): void {
    if (!Number.isFinite(ms)) {
      throw new RangeError(`ms must be finite, got ${ms}`);
    }
    this.endMs = Math.max(this.endMs, ms);
  }

  /** `bytes` that arrived `ms` after the first byte. */
  addAt(ms: number, bytes: number): void {
    this.addSpread(ms, ms, bytes);
  }

  /**
   * A read of `bytes` on stream `stream` that arrived `ms` after the window's
   * first byte, counted by rule S as amendment 15 extends it
   * (lag.dev/PLAN.md M6). Each stream's reads are counted on their own:
   * - A read counts as having arrived evenly over a span that ends when it
   *   arrived. A read that arrives within LAG_SPREAD_JOIN_MS of the first
   *   read of its stream's group joins the group, and its span starts where
   *   the group's does. Any other read starts a new group, whose span starts
   *   at the stream's previous read, at most LAG_SPREAD_MAX_MS back and
   *   never before the window's first byte.
   * - A stream's first group is held, uncounted, until its second begins.
   *   It then counts as having arrived at the rate of the second group's
   *   first read, n bytes at t2: its B bytes, the last of them at t1, over
   *   [max(t1 - B (t2 - t1) / n, t1 - LAG_SPREAD_MAX_MS), t1]. Only the part
   *   from the window's first byte on counts; the rest arrived before the
   *   window opened. A first group with no second counts by rule S (finish,
   *   endStream).
   * - Only the part of a span before the window's end counts, so a read
   *   that arrives after the end still counts what it carried before it. A
   *   stream is done once it has a read whose span starts at or after the
   *   end: its spans are contiguous, so nothing it reads later can count.
   *   A read that arrives more than LAG_SPREAD_MAX_MS after the end could
   *   not reach back before it, so it makes the window final (finish).
   * A read of no bytes changes nothing.
   */
  addRead(stream: number, ms: number, bytes: number): void {
    if (!(Number.isInteger(stream) && 0 <= stream)) {
      throw new RangeError(`stream must be an integer at least 0, got ${stream}`);
    }
    if (!Number.isFinite(ms)) {
      throw new RangeError(`ms must be finite, got ${ms}`);
    }
    if (!(Number.isFinite(bytes) && 0 <= bytes)) {
      throw new RangeError(`bytes must be a finite number at least 0, got ${bytes}`);
    }
    if (this.finishedValue) {
      return;
    }
    this.total += bytes;
    if (bytes === 0) {
      return;
    }
    const end = this.windowMs;
    if (end + LAG_SPREAD_MAX_MS < ms) {
      this.finish();
      return;
    }
    const reads = this.streams[stream];
    // a stream's reads arrive in order
    const t = Math.max(0, ms, reads?.last ?? 0);
    this.endMs = Math.max(this.endMs, Math.min(t, end));
    if (reads === undefined) {
      this.streams[stream] = { last: t, groupFirst: t, groupFrom: t, held: { bytes, last: t }, done: false };
      return;
    }
    if (reads.done) {
      return;
    }
    let from: number;
    if (t - reads.groupFirst <= LAG_SPREAD_JOIN_MS) {
      if (reads.held !== null) {
        reads.held.bytes += bytes;
        reads.held.last = t;
        reads.last = t;
        return;
      }
      from = reads.groupFrom;
    } else {
      if (reads.held !== null) {
        this.countFirstGroup(reads, t, bytes);
      }
      from = Math.max(reads.last, t - LAG_SPREAD_MAX_MS, 0);
      reads.groupFirst = t;
      reads.groupFrom = from;
    }
    reads.last = t;
    if (end <= from) {
      reads.done = true;
      return;
    }
    this.countSpan(from, t, bytes);
  }

  /** When stream `stream`'s latest read arrived (addRead), in ms after the first byte; undefined before its first. */
  lastRead(stream: number): number | undefined {
    return this.streams[stream]?.last;
  }

  /**
   * Stream `stream` has ended (lag.dev/PLAN.md M6, amendment 15): a first
   * group it still holds counts by rule S, its span starting at the later of
   * the window's first byte and LAG_SPREAD_MAX_MS before its last read, and
   * nothing it reads after counts.
   */
  endStream(stream: number): void {
    if (!(Number.isInteger(stream) && 0 <= stream)) {
      throw new RangeError(`stream must be an integer at least 0, got ${stream}`);
    }
    if (this.finishedValue) {
      return;
    }
    const reads = this.streams[stream];
    if (reads === undefined) {
      this.streams[stream] = { last: 0, groupFirst: Number.NEGATIVE_INFINITY, groupFrom: 0, held: null, done: true };
      return;
    }
    if (reads.held !== null) {
      this.countFirstGroup(reads);
    }
    reads.done = true;
  }

  /** The window is final: nothing more counts (finish). */
  get finished(): boolean {
    return this.finishedValue;
  }

  /**
   * The window is final (lag.dev/PLAN.md M6, amendment 15): every stream is
   * done, or no read can reach back before the end any more, or the
   * direction is over. A first group still held counts by rule S, as when
   * its stream ends (endStream), and no read counts after this.
   */
  finish(): void {
    if (this.finishedValue) {
      return;
    }
    for (const reads of this.streams) {
      if (reads !== undefined && reads.held !== null) {
        this.countFirstGroup(reads);
      }
    }
    this.finishedValue = true;
  }

  /**
   * Whether stream `stream` is done (lag.dev/PLAN.md M6, amendment 15): it
   * has had a read whose span starts at or after the window's end, or it
   * ended, so nothing it reads later counts.
   */
  streamDone(stream: number): boolean {
    return this.streams[stream]?.done ?? false;
  }

  /**
   * How far back stream `stream`'s later reads can still count, in ms after
   * the first byte, when every read of it that arrived by `seenMs` has been
   * added: an interval that ends at or before this is final as far as the
   * stream goes (M4's settled reports). Infinity when nothing it reads later
   * can count: it is done, its reach is past the window's end, or the window
   * is final.
   */
  reach(stream: number, seenMs: number): number {
    if (this.finishedValue) {
      return Number.POSITIVE_INFINITY;
    }
    const reads = this.streams[stream];
    let reach: number;
    if (reads === undefined) {
      // its first group, still to come, reaches back at most
      // LAG_SPREAD_MAX_MS from its last read
      reach = seenMs - LAG_SPREAD_MAX_MS;
    } else if (reads.done) {
      return Number.POSITIVE_INFINITY;
    } else if (reads.held !== null) {
      // and so does the first group it holds
      reach = reads.held.last - LAG_SPREAD_MAX_MS;
    } else if (seenMs <= reads.groupFirst + LAG_SPREAD_JOIN_MS) {
      // a read may still join its group, whose span starts where the
      // group's does
      reach = reads.groupFrom;
    } else {
      // a new group's span starts no earlier than its previous read, nor
      // more than LAG_SPREAD_MAX_MS before it arrives
      reach = Math.max(reads.last, seenMs - LAG_SPREAD_MAX_MS);
    }
    return this.windowMs <= reach ? Number.POSITIVE_INFINITY : reach;
  }

  // A stream's first group counts (addRead): at the rate of its second
  // group's first read, `bytes` at `t2`, or by rule S when there is none or
  // no time passed between them.
  private countFirstGroup(reads: LagStreamReads, t2?: number, bytes?: number): void {
    const held = reads.held!;
    reads.held = null;
    const t1 = held.last;
    const from =
      t2 !== undefined && bytes !== undefined && t1 < t2
        ? Math.max(t1 - (held.bytes * (t2 - t1)) / bytes, t1 - LAG_SPREAD_MAX_MS)
        : Math.max(0, t1 - LAG_SPREAD_MAX_MS);
    this.countSpan(from, t1, held.bytes);
  }

  // `bytes` that arrived evenly over [from, to] (addRead), of which only the
  // part inside the window counts: from its first byte to its end. A span of
  // no time counts its bytes at `to`, when that is inside the window. The
  // pieces are cut by cumulative rounding, as addSpread cuts them, so they
  // add up to exactly what the span counts.
  private countSpan(from: number, to: number, bytes: number): void {
    const interval = this.intervalMs;
    if (to <= from) {
      if (0 <= to && to < this.windowMs) {
        this.addToInterval(Math.floor(to / interval), bytes);
      }
      return;
    }
    const lo = Math.max(0, from);
    const hi = Math.min(to, this.windowMs);
    if (hi <= lo) {
      return;
    }
    const through = (cut: number) =>
      cut <= from
        ? 0
        : to <= cut
          ? bytes
          : Math.min(bytes, Math.round(((bytes * (cut - from)) / (to - from)) * SPREAD_GRAIN) / SPREAD_GRAIN);
    let placed = through(lo);
    for (let i = Math.floor(lo / interval); i * interval < hi && i < this.maxIntervals; i += 1) {
      const counted = through(Math.min(hi, (i + 1) * interval));
      this.addToInterval(i, counted - placed);
      placed = counted;
    }
  }

  /**
   * `bytes` that arrived evenly over [fromMs, toMs] after the first byte
   * (rule S, lag.dev/PLAN.md M6): each interval the span covers gets its
   * share by time. A span of no time counts its bytes at toMs, in the
   * interval that holds it. Nothing counts before the first byte.
   */
  addSpread(fromMs: number, toMs: number, bytes: number): void {
    if (!Number.isFinite(fromMs) || !Number.isFinite(toMs)) {
      throw new RangeError(`fromMs and toMs must be finite, got ${fromMs} and ${toMs}`);
    }
    if (!(Number.isFinite(bytes) && 0 <= bytes)) {
      throw new RangeError(`bytes must be a finite number at least 0, got ${bytes}`);
    }
    const to = Math.max(0, toMs);
    const from = Math.min(to, Math.max(0, fromMs));
    this.total += bytes;
    this.endMs = Math.max(this.endMs, to);
    if (bytes === 0) {
      return;
    }
    const interval = this.intervalMs;
    if (from === to) {
      this.addToInterval(Math.floor(to / interval), bytes);
      return;
    }
    // the bytes through each cut, so the pieces add up to exactly `bytes`
    let placed = 0;
    for (let i = Math.floor(from / interval); i * interval < to && i < this.maxIntervals; i += 1) {
      const cut = Math.min(to, (i + 1) * interval);
      const through =
        cut === to
          ? bytes
          : Math.min(bytes, Math.round(((bytes * (cut - from)) / (to - from)) * SPREAD_GRAIN) / SPREAD_GRAIN);
      this.addToInterval(i, through - placed);
      placed = through;
    }
  }

  /**
   * A cumulative byte count `ms` after the first byte; the count is 0 at
   * ms 0, and a count lower than an earlier one adds nothing. What it adds
   * spreads over the time since the previous count (addSpread).
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
    this.addSpread(from, to, bytes);
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
