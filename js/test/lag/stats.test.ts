import { test } from "node:test";
import assert from "node:assert/strict";
import {
  bytesToMbps,
  classifyPing,
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
  // bytes reported with no time passing close the interval ending there
  const same = new LagIntervalSeries();
  same.addCumulative(0, 10);
  same.addCumulative(250, 10);
  assert.deepEqual(same.bytes, [10]);
  // a report far past maxIntervals stays bounded
  const far = new LagIntervalSeries({ maxIntervals: 2 });
  far.addCumulative(1e12, 1e6);
  assert.equal(far.count, 2);
  assert.equal(far.totalBytes, 1e6);
});
