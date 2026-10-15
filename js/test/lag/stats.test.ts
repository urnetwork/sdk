import { test } from "node:test";
import assert from "node:assert/strict";
import {
  bytesToMbps,
  classifyPing,
  LAG_SPREAD_JOIN_MS,
  LAG_SPREAD_MAX_MS,
  LagHistogram,
  LagIntervalSeries,
  LagLatencyStats,
  LagPingWindow,
  loadWindowAt,
  nearestRank,
  percentiles,
  splitPings,
} from "../../src/lag/stats.ts";

// a seeded generator, so a failure reproduces
const random = (seed: number) => () => {
  seed = (seed + 0x6d2b79f5) | 0;
  let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
  t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
  return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
};

test("nearest rank: four samples give the second smallest and the largest", () => {
  const samples = [23.4, 10.1, 40.2, 31.0];
  assert.equal(nearestRank(samples, 50), 23.4);
  assert.equal(nearestRank(samples, 95), 40.2);
  assert.deepEqual(percentiles(samples), { p50: 23.4, p95: 40.2 });
  assert.deepEqual(samples, [23.4, 10.1, 40.2, 31.0], "the input is not reordered");
});

test("nearest rank on round ranks and edges", () => {
  const hundred = Array.from({ length: 100 }, (_, i) => 100 - i);
  assert.deepEqual(percentiles(hundred), { p50: 50, p95: 95 });
  const twenty = Array.from({ length: 20 }, (_, i) => i + 1);
  assert.deepEqual(percentiles(twenty), { p50: 10, p95: 19 });
  assert.deepEqual(percentiles([7]), { p50: 7, p95: 7 });
  assert.equal(percentiles([]), undefined);
  assert.equal(nearestRank([], 50), undefined);
  assert.equal(nearestRank([1, 2, 3], 100), 3);
  assert.throws(() => nearestRank([1], 0), RangeError);
  assert.throws(() => nearestRank([], 101), RangeError);
});

test("bytesToMbps", () => {
  assert.equal(bytesToMbps(312_500, 250), 10);
  assert.equal(bytesToMbps(125_000_000, 1000), 1000);
  assert.equal(bytesToMbps(1, 0), 0);
});

test("histogram: exact percentiles while within exactLimit", () => {
  const next = random(1);
  const histogram = new LagHistogram({ exactLimit: 500 });
  const values: number[] = [];
  for (let i = 0; i < 500; i += 1) {
    const value = Math.round(next() * 100_000) / 100;
    values.push(value);
    assert.equal(histogram.add(value), true);
    if (i % 37 === 0) {
      assert.deepEqual(histogram.percentiles(), percentiles(values));
    }
  }
  assert.equal(histogram.exactPercentiles, true);
  assert.deepEqual(histogram.percentiles(), percentiles(values));
  assert.equal(histogram.count, 500);
  assert.equal(histogram.min, Math.min(...values));
  assert.equal(histogram.max, Math.max(...values));
  assert.ok(Math.abs(histogram.mean! - values.reduce((a, b) => a + b, 0) / 500) < 1e-9);
});

test("histogram: past exactLimit, percentiles stay within the relative accuracy", () => {
  const next = random(2);
  const histogram = new LagHistogram({ exactLimit: 64, relativeAccuracy: 0.01 });
  const values: number[] = [];
  for (let i = 0; i < 100_000; i += 1) {
    // a long-tailed latency: 20 ms typical, bursts to seconds
    const value = 20 * Math.exp(next() * 2) * (next() < 0.02 ? 50 : 1);
    values.push(value);
    histogram.add(value);
  }
  assert.equal(histogram.exactPercentiles, false);
  for (const p of [1, 25, 50, 90, 95, 99, 99.9]) {
    const exact = nearestRank(values, p)!;
    const estimate = histogram.percentile(p)!;
    assert.ok(
      Math.abs(estimate - exact) <= exact * 0.01 + 1e-9,
      `p${p}: ${estimate} vs ${exact}`,
    );
  }
  // the extremes are exact
  assert.equal(histogram.percentile(100), Math.max(...values));
  assert.equal(histogram.percentile(0.0001), Math.min(...values));
  // memory is bounded by the value range, not the count
  const buckets = (histogram as unknown as { buckets: Map<number, number> }).buckets;
  assert.ok(buckets.size < 1000, `${buckets.size} buckets`);
});

test("histogram: zeros, a huge range, and what it ignores", () => {
  const histogram = new LagHistogram({ exactLimit: 0 });
  for (const value of [0, 0, 0, 1e-9, 5, 1e15]) {
    assert.equal(histogram.add(value), true);
  }
  for (const value of [NaN, -1, Infinity, -Infinity]) {
    assert.equal(histogram.add(value), false);
  }
  assert.equal(histogram.count, 6);
  assert.equal(histogram.percentile(50), 0);
  assert.equal(histogram.percentile(100), 1e15);
  const p83 = histogram.percentile(83)!;
  assert.ok(Math.abs(p83 - 5) <= 5 * 0.005, `${p83}`);
  histogram.reset();
  assert.equal(histogram.count, 0);
  assert.equal(histogram.percentiles(), undefined);
  assert.equal(histogram.min, undefined);
});

test("histogram: merge combines exact and bucketed histograms", () => {
  const next = random(3);
  const values: number[] = [];
  const a = new LagHistogram({ exactLimit: 100 });
  const b = new LagHistogram({ exactLimit: 100 });
  const c = new LagHistogram({ exactLimit: 100 });
  for (let i = 0; i < 50; i += 1) {
    const value = next() * 100;
    values.push(value);
    a.add(value);
  }
  for (let i = 0; i < 40; i += 1) {
    const value = next() * 100;
    values.push(value);
    b.add(value);
  }
  a.merge(b);
  assert.equal(a.exactPercentiles, true);
  assert.deepEqual(a.percentiles(), percentiles(values));
  for (let i = 0; i < 1000; i += 1) {
    const value = next() * 100;
    values.push(value);
    c.add(value);
  }
  a.merge(c);
  assert.equal(a.count, values.length);
  assert.equal(a.exactPercentiles, false);
  const exact = percentiles(values)!;
  const merged = a.percentiles()!;
  assert.ok(Math.abs(merged.p50 - exact.p50) <= exact.p50 * 0.005 + 1e-9);
  assert.ok(Math.abs(merged.p95 - exact.p95) <= exact.p95 * 0.005 + 1e-9);
  assert.throws(() => a.merge(new LagHistogram({ relativeAccuracy: 0.02 })), RangeError);
});

test("ping window: keeps the last minute, bounded, with exact ranges", () => {
  const window = new LagPingWindow();
  // four a second for two minutes, every 10th lost
  for (let i = 0; i < 480; i += 1) {
    window.add({ t: i * 250, ms: i % 10 === 9 ? null : 20 + (i % 7) });
  }
  assert.equal(window.newest, 479 * 250);
  assert.equal(window.oldest, 479 * 250 - 60_000);
  assert.equal(window.size, 241);
  const last10s = window.pings(window.newest! - 10_000 + 1);
  assert.equal(last10s.length, 40);
  assert.ok(last10s.every((ping, i) => i === 0 || last10s[i - 1].t < ping.t));
  // exact: the same as ranking the range's round trips directly
  const from = 90_000;
  const to = 100_000;
  const rtts = [];
  for (let i = 0; i < 480; i += 1) {
    if (from <= i * 250 && i * 250 < to && i % 10 !== 9) {
      rtts.push(20 + (i % 7));
    }
  }
  assert.deepEqual(window.roundTrips(from, to), rtts);
  assert.deepEqual(window.percentiles(from, to), percentiles(rtts));
  assert.equal(window.lost(from, to), 4);
  assert.equal(window.lost(), 25, "pings 239, 249, ..., 479");
  assert.equal(window.percentiles(1e9), undefined);

  const small = new LagPingWindow({ capacity: 10 });
  for (let i = 0; i < 100; i += 1) {
    small.add({ t: i, ms: i });
  }
  assert.equal(small.size, 10);
  assert.deepEqual(small.roundTrips(), [90, 91, 92, 93, 94, 95, 96, 97, 98, 99]);
});

test("ping window: a late ping is placed by its time; a stale or invalid one is dropped", () => {
  const window = new LagPingWindow({ windowMs: 1000 });
  for (const t of [0, 250, 500, 1000]) {
    window.add({ t, ms: 10 });
  }
  assert.equal(window.add({ t: 750, ms: 30 }), true);
  assert.equal(window.add({ t: 500, ms: 31 }), true);
  assert.deepEqual(
    window.pings().map((ping) => [ping.t, ping.ms]),
    [[0, 10], [250, 10], [500, 10], [500, 31], [750, 30], [1000, 10]],
  );
  assert.equal(window.add({ t: -1, ms: 1 }), false, "older than the window");
  assert.equal(window.add({ t: NaN, ms: 1 }), false);
  assert.equal(window.add({ t: 2000, ms: -3 }), false);
  window.add({ t: 1600, ms: 12 });
  assert.equal(window.oldest, 750);
  window.clear();
  assert.equal(window.size, 0);
  assert.equal(window.newest, undefined);
});

test("ping window: shift moves every kept ping on the time axis", () => {
  const window = new LagPingWindow({ windowMs: 1000 });
  for (const t of [0, 250, 500, 750]) {
    window.add({ t, ms: t / 10 });
  }
  window.shift(-400);
  assert.deepEqual(
    window.pings().map((ping) => [ping.t, ping.ms]),
    [[-400, 0], [-150, 25], [100, 50], [350, 75]],
  );
  assert.equal(window.newest, 350);
  assert.deepEqual(window.roundTrips(-200, 200), [25, 50]);
  // later pings line up with the moved ones
  window.add({ t: 600, ms: 1 });
  assert.equal(window.oldest, -400);
  assert.throws(() => window.shift(NaN), RangeError);
});

test("ping window: memory stays bounded over a long session", () => {
  const window = new LagPingWindow();
  for (let i = 0; i < 200_000; i += 1) {
    window.add({ t: i * 250, ms: 20 });
  }
  assert.equal(window.size, 241);
  const internal = window as unknown as { times: number[] };
  assert.ok(internal.times.length <= 2 * 241 + 64, `${internal.times.length}`);
});

test("pings split into idle, loaded and own by the track's load windows", () => {
  const windows = [
    { start: 5000, end: 15_000, index: 0 },
    { start: 20_000, end: 30_000, index: 1 },
    { start: 35_000, end: null, index: 2 },
  ];
  assert.equal(classifyPing(4999, windows, 1), "idle");
  assert.equal(classifyPing(5000, windows, 1), "loaded");
  assert.equal(classifyPing(15_000, windows, 1), "idle", "end is exclusive");
  assert.equal(classifyPing(25_000, windows, 1), "own");
  assert.equal(classifyPing(25_000, windows), "loaded");
  assert.equal(classifyPing(1e9, windows, 2), "own", "a running load has no end yet");
  assert.equal(loadWindowAt(25_000, windows)?.index, 1);
  assert.equal(loadWindowAt(17_000, windows), undefined);

  const pings = [
    { t: 1000, ms: 20 },
    { t: 6000, ms: 80 },
    { t: 7000, ms: null },
    { t: 21_000, ms: 150 },
    { t: 22_000, ms: 160 },
    { t: 31_000, ms: 22 },
  ];
  assert.deepEqual(splitPings(pings, windows, 1), {
    idle: [20, 22],
    loaded: [80, 150, 160],
    own: [150, 160],
    lost: 1,
  });

  const stats = new LagLatencyStats();
  for (const ping of pings) {
    stats.add(ping, windows, 1);
  }
  assert.deepEqual(stats.idle.percentiles(), { p50: 20, p95: 22 });
  assert.deepEqual(stats.own.percentiles(), { p50: 150, p95: 160 });
  assert.equal(stats.loaded.count, 3);
  assert.equal(stats.lost, 1);
  assert.equal(stats.loadIncreaseMs, 130);
  assert.equal(new LagLatencyStats().loadIncreaseMs, undefined);

  // a ping the caller saw near a load is neither idle nor loaded; one in a
  // window is classified as before
  const settling = new LagLatencyStats();
  assert.equal(settling.add({ t: 15_500, ms: 90 }, windows, 1, true), "settling");
  assert.equal(settling.add({ t: 21_000, ms: 150 }, windows, 1, true), "own");
  assert.equal(settling.add({ t: 31_000, ms: 22 }, windows, 1, false), "idle");
  assert.deepEqual(settling.idle.percentiles(), { p50: 22, p95: 22 });
  assert.equal(settling.loaded.count, 1);
});

test("interval series: exact arrivals per 250 ms interval from the first byte", () => {
  const series = new LagIntervalSeries({ intervalMs: 250, maxIntervals: 20 });
  // 10 Mbps is 312,500 bytes per 250 ms: arrive in 4 chunks per interval
  for (let ms = 0; ms < 5000; ms += 62.5) {
    series.addAt(ms, 78_125);
  }
  // a spill past the window counts toward the total only
  series.addAt(5100, 1000);
  assert.equal(series.count, 20);
  assert.equal(series.totalBytes, 80 * 78_125 + 1000);
  assert.deepEqual(series.mbps, Array(20).fill(10));
  assert.deepEqual(series.percentiles(), { p50: 10, p95: 10 });
  assert.equal(series.intervalBytes(19), 312_500);
  assert.equal(series.intervalBytes(20), 0);
});

test("interval series: an interval counts once half of it has passed", () => {
  const series = new LagIntervalSeries();
  series.addAt(0, 100);
  assert.equal(series.count, 0);
  series.addAt(124, 100);
  assert.equal(series.count, 0);
  series.addAt(125, 100);
  assert.equal(series.count, 1);
  series.addAt(3100, 100);
  assert.equal(series.count, 12, "a load cut short at 3.1 s ends on a whole interval");
  series.addAt(3130, 100);
  assert.equal(series.count, 13);
  assert.equal(series.bytes.length, 13);
  assert.equal(new LagIntervalSeries().percentiles(), undefined);
});

test("interval series: cumulative reports spread over the time since the last", () => {
  const series = new LagIntervalSeries({ maxIntervals: 4 });
  series.addCumulative(250, 1000);
  series.addCumulative(500, 3000);
  // a late report: 260 ms covering [500, 760) splits 250:10
  series.addCumulative(760, 5600);
  series.addCumulative(760, 5600);
  // a count that goes backwards adds nothing
  series.addCumulative(1000, 5000);
  assert.deepEqual(series.bytes, [1000, 2000, 2500, 100]);
  assert.equal(series.totalBytes, 5600);
  assert.equal(series.durationMs, 1000);
  // bytes reported with no time passing count at that time, in the interval
  // that holds it
  const same = new LagIntervalSeries();
  same.addCumulative(0, 10);
  same.addCumulative(250, 10);
  assert.deepEqual(same.bytes, [10]);
  same.addCumulative(250, 30);
  same.addCumulative(500, 30);
  assert.deepEqual(same.bytes, [10, 20]);
  // a report far past maxIntervals stays bounded
  const far = new LagIntervalSeries({ maxIntervals: 2 });
  far.addCumulative(1e12, 1e6);
  assert.equal(far.count, 2);
  assert.equal(far.totalBytes, 1e6);
});

test("interval series: a span's bytes split across the intervals it covers, in proportion to time (rule S)", () => {
  const series = new LagIntervalSeries({ maxIntervals: 4 });
  // 1000 bytes over [100, 600): 150 ms, 250 ms and 100 ms of it
  series.addSpread(100, 600, 1000);
  assert.deepEqual([0, 1, 2].map((i) => series.intervalBytes(i)), [300, 500, 200]);
  // a span of no time counts at its time, in the interval that holds it,
  // and a span from before the first byte starts at it
  series.addSpread(750, 750, 10);
  series.addSpread(-500, 0, 7);
  assert.equal(series.intervalBytes(3), 10);
  assert.equal(series.intervalBytes(0), 307);
  assert.equal(series.count, 3);
  assert.equal(series.durationMs, 750);
  // a span past the kept intervals counts toward the total only
  series.addSpread(900, 1100, 400);
  assert.equal(series.intervalBytes(3), 10 + 200);
  assert.equal(series.totalBytes, 1000 + 10 + 7 + 400);
  assert.equal(series.count, 4);
  assert.throws(() => series.addSpread(0, Number.NaN, 1), RangeError);
  assert.throws(() => series.addSpread(0, 1, -1), RangeError);
});

test("interval series: whole bytes are the floors of the running sum, and spans add up exactly", () => {
  // a third of 1000 bytes in each interval
  const thirds = new LagIntervalSeries({ maxIntervals: 3 });
  thirds.addSpread(0, 750, 1000);
  assert.deepEqual(thirds.bytes, [333, 333, 334]);
  assert.deepEqual([0, 1, 2].map((i) => thirds.wholeBytesThrough(i)), [333, 666, 1000]);

  // Thousands of chunks on four streams, spread over spans that split them
  // unevenly: the whole bytes through each interval never decrease, add up
  // interval by interval, and end at exactly the bytes that came. Pieces
  // in plain floating point lose a byte to rounding on most of these.
  for (let seed = 1; seed <= 40; seed += 1) {
    const next = random(seed);
    const series = new LagIntervalSeries({ maxIntervals: 20 });
    const last = [0, 0, 0, 0];
    let total = 0;
    for (let n = 0; n < 2000; n += 1) {
      const stream = Math.floor(next() * 4);
      const t = Math.min(4999.999, last[stream] + next() * 40 + 0.001);
      const bytes = 1 + Math.floor(next() * 65536);
      series.addSpread(Math.max(last[stream], t - 1000), t, bytes);
      last[stream] = t;
      total += bytes;
    }
    series.extendTo(5000);
    let through = 0;
    series.bytes.forEach((bytes, i) => {
      assert.ok(Number.isInteger(bytes) && 0 <= bytes, `seed ${seed}, interval ${i}: ${bytes}`);
      through += bytes;
      assert.equal(series.wholeBytesThrough(i), through);
    });
    assert.equal(through, total, `seed ${seed}`);
  }
});

// The share of `bytes`, spread evenly over [from, to], that falls in [lo, hi)
const share = (from: number, to: number, bytes: number, lo: number, hi: number) =>
  (bytes * Math.max(0, Math.min(to, hi) - Math.max(from, lo))) / (to - from);

const intervals = (series: LagIntervalSeries, count: number) => Array.from({ length: count }, (_, i) => series.intervalBytes(i));

// within the 1/1024 byte the pieces are cut to
const assertBytes = (actual: number[], expected: number[], what: string) => {
  assert.equal(actual.length, expected.length);
  actual.forEach((bytes, i) => assert.ok(Math.abs(bytes - expected[i]) < 0.01, `${what}, interval ${i}: ${bytes}, expected ${expected[i]}`));
};

test("interval series: reads that arrive back to back join their group and share its span, so a burst after a stall spreads over it", () => {
  assert.equal(LAG_SPREAD_JOIN_MS, 2);
  const series = new LagIntervalSeries({ maxIntervals: 4, windowMs: 1000 });
  // the stream's first group, at the rate of its second: [-100, 0], before
  // the window, so it counts nothing; the second spans [0, 100]
  series.addRead(0, 0, 1000);
  series.addRead(0, 100, 1000);
  // a stall to 600 ms, then four reads, the last 2 ms after the first: one
  // group, each read spread from the stream's read before the stall
  for (const t of [600, 600.5, 601, 602]) {
    series.addRead(0, t, 1000);
  }
  // 2.5 ms after the group's first read, a new group, from the read before
  series.addRead(0, 602.5, 1000);
  const expected = [0, 1, 2, 3].map((i) => {
    const [lo, hi] = [i * 250, (i + 1) * 250];
    return (
      share(0, 100, 1000, lo, hi) +
      [600, 600.5, 601, 602].reduce((sum, t) => sum + share(100, t, 1000, lo, hi), 0) +
      share(602, 602.5, 1000, lo, hi)
    );
  });
  assertBytes(intervals(series, 4), expected, "burst");
  // by rule S alone, the stall's middle interval would hold only the first
  // read's share, 500 bytes
  assert.ok(1990 < series.intervalBytes(1), `${series.intervalBytes(1)}`);
});

test("interval series: a steady fast stream is cut into groups of 2 ms, so no read spreads far", () => {
  // A read at 0 and one at 500 ms, then from 1000 ms a steady run of 500
  // bytes every 0.4 ms, 1250 bytes a ms. Groups are 2 ms from their first
  // read, not from the read before, so only the run's first group, six
  // reads, spreads back over the gap since 500 ms, and every later read
  // over at most 2.4 ms: each interval of the run holds its 312,500 bytes to
  // within a group's 3000. Joined read to read, the run would be one group,
  // every read spread back to 500 ms: some 340 kB in [750, 1000) alone.
  const series = new LagIntervalSeries({ maxIntervals: 8, windowMs: 2000 });
  series.addRead(0, 0, 500);
  series.addRead(0, 500, 500);
  for (let k = 0; 1000 + k * 0.4 < 2005; k += 1) {
    series.addRead(0, 1000 + k * 0.4, 500);
  }
  series.finish();
  assert.ok(series.intervalBytes(3) < 3000, `${series.intervalBytes(3)} bytes in [750, 1000)`);
  [4, 5, 6, 7].forEach((i) => {
    assert.ok(Math.abs(series.intervalBytes(i) - 312_500) <= 3000, `interval ${i}: ${series.intervalBytes(i)}`);
  });
});

test("interval series: a stream's first group waits for its second, counts at its rate, and only from the window's first byte", () => {
  const series = new LagIntervalSeries({ maxIntervals: 4, windowMs: 1000 });
  series.addRead(0, 0, 16384);
  series.addRead(1, 100, 16384);
  // both first groups are held, uncounted
  assert.deepEqual(intervals(series, 4), [0, 0, 0, 0]);
  assert.equal(series.totalBytes, 2 * 16384);
  // stream 0's second: its first arrived over [-200, 0], before the window
  series.addRead(0, 200, 16384);
  // stream 1's second, 8192 bytes in 200 ms: its first, twice that, over
  // [-300, 100], a quarter of it from the first byte on
  series.addRead(1, 300, 8192);
  assertBytes(intervals(series, 4), [16384 + 4096 + 6144, 2048, 0, 0], "first groups");

  // at most LAG_SPREAD_MAX_MS back: 64 KiB then 1 KiB 100 ms later would
  // put the first 6.4 s back
  const capped = new LagIntervalSeries({ maxIntervals: 8, windowMs: 2000 });
  capped.addRead(0, 0, 100);
  capped.addRead(0, 50, 100);
  capped.addRead(1, 1500, 65536);
  capped.addRead(1, 1600, 1024);
  assert.equal(LAG_SPREAD_MAX_MS, 1000);
  assertBytes(intervals(capped, 8), [100, 0, 16384, 16384, 16384, 16384, 1024, 0], "capped");
});

test("interval series: a first group with no second counts by rule S when its stream ends or the window is final", () => {
  const series = new LagIntervalSeries({ maxIntervals: 8, windowMs: 2000 });
  series.addRead(0, 0, 1000);
  series.addRead(0, 100, 1000);
  // a stream that ends after one read: from 1 s before it, but not before
  // the window's first byte
  series.addRead(1, 400, 2000);
  series.endStream(1);
  assert.equal(series.streamDone(1), true);
  // nothing it reads after its end counts
  series.addRead(1, 450, 2000);
  // a stream still holding its first group when the window is final
  series.addRead(2, 1700, 3000);
  assert.equal(series.finished, false);
  series.finish();
  assert.equal(series.finished, true);
  // and nothing counts after that
  series.addRead(0, 800, 5000);
  const expected = [0, 1, 2, 3, 4, 5, 6, 7].map((i) => {
    const [lo, hi] = [i * 250, (i + 1) * 250];
    return share(0, 100, 1000, lo, hi) + share(0, 400, 2000, lo, hi) + share(700, 1700, 3000, lo, hi);
  });
  assertBytes(intervals(series, 8), expected, "fallbacks");
  assert.equal(series.totalBytes, 1000 + 1000 + 2000 + 2000 + 3000);

  // the window's only read, at its first byte, counts there whole
  const single = new LagIntervalSeries({ maxIntervals: 4, windowMs: 1000 });
  single.addRead(0, 0, 777);
  single.endStream(0);
  assert.deepEqual(intervals(single, 4), [777, 0, 0, 0]);
});

test("interval series: past the window's end, a read counts its part before it, a stream whose span starts after it is done, and a read 1 s past it makes the window final", () => {
  const series = new LagIntervalSeries({ maxIntervals: 4, windowMs: 1000 });
  series.addRead(0, 0, 100);
  series.addRead(0, 900, 900);
  // straddling the end: half of [900, 1100] is before it
  series.addRead(0, 1100, 2000);
  assert.equal(series.streamDone(0), false);
  // joining that group, from 900: 100 of its 201 ms are before the end
  series.addRead(0, 1101, 500);
  // a new group from 1101: after the end, so the stream is done
  series.addRead(0, 1200, 700);
  assert.equal(series.streamDone(0), true);
  series.addRead(0, 1300, 700);
  // a first group held after the end, which may still reach back before it
  series.addRead(1, 1500, 4000);
  assert.equal(series.finished, false);
  // a read more than 1 s past the end could not: the window is final, and
  // the held group counts by rule S, over [500, 1500]
  series.addRead(2, 2000.5, 100);
  assert.equal(series.finished, true);
  const expected = [0, 1, 2, 3].map((i) => {
    const [lo, hi] = [i * 250, (i + 1) * 250];
    return (
      share(0, 900, 900, lo, hi) +
      share(900, 1100, 2000, lo, Math.min(hi, 1000)) +
      share(900, 1101, 500, lo, Math.min(hi, 1000)) +
      share(500, 1500, 4000, lo, Math.min(hi, 1000))
    );
  });
  assertBytes(intervals(series, 4), expected, "end");
  assert.equal(series.durationMs, 1000);
  assert.equal(series.count, 4);
});

test("interval series: reach says how far back a stream's later reads can still count", () => {
  const series = new LagIntervalSeries({ maxIntervals: 20, windowMs: 5000 });
  // no read yet: its first group can reach 1 s before whatever comes
  assert.equal(series.reach(0, 300), -700);
  // a held first group reaches 1 s before its last read
  series.addRead(0, 0, 100);
  assert.equal(series.reach(0, 10), -1000);
  series.addRead(0, 1.5, 100);
  assert.equal(series.reach(0, 10), -998.5);
  // the second group spans from the first's last read: while a read may
  // still join it (within 2 ms of its first), the reach is there; then the
  // last read, or 1 s back, whichever is later
  series.addRead(0, 400, 100);
  assert.equal(series.reach(0, 401), 1.5);
  assert.equal(series.reach(0, 403), 400);
  assert.equal(series.reach(0, 2000), 1000);
  // a read that straddles the end still reaches back before it, until its
  // group closes; the next span starts after it, so nothing more can count
  series.addRead(0, 5100, 100);
  assert.equal(series.reach(0, 5101), 4100);
  assert.equal(series.reach(0, 5103), Number.POSITIVE_INFINITY);
  series.addRead(0, 5200, 100);
  assert.equal(series.streamDone(0), true);
  assert.equal(series.reach(0, 5300), Number.POSITIVE_INFINITY);
  // an ended stream, and every stream of a final window, reach nothing
  series.endStream(1);
  assert.equal(series.reach(1, 0), Number.POSITIVE_INFINITY);
  series.finish();
  assert.equal(series.reach(2, 0), Number.POSITIVE_INFINITY);
});

test("interval series: counted by the rules, whole bytes are still the floors of the running sum and add up exactly", () => {
  // Four streams of reads with stalls, bursts, first groups before the
  // window and reads past its end: the whole bytes through each interval
  // never decrease and add up interval by interval, whatever was cut away.
  for (let seed = 1; seed <= 40; seed += 1) {
    const next = random(seed);
    const series = new LagIntervalSeries({ maxIntervals: 20, windowMs: 5000 });
    const at = [0, 0, 0, 0].map(() => next() * 50);
    for (let n = 0; n < 2000; n += 1) {
      const stream = Math.floor(next() * 4);
      at[stream] += next() < 0.1 ? next() * 2 : next() * 40;
      series.addRead(stream, at[stream], 1 + Math.floor(next() * 65536));
    }
    series.finish();
    series.extendTo(5000);
    let through = 0;
    series.bytes.forEach((bytes, i) => {
      assert.ok(Number.isInteger(bytes) && 0 <= bytes, `seed ${seed}, interval ${i}: ${bytes}`);
      through += bytes;
      assert.equal(series.wholeBytesThrough(i), through);
    });
    assert.ok(through <= series.totalBytes);
  }
});
