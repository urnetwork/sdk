import { test } from "node:test";
import assert from "node:assert/strict";
import { registerTsResolve } from "../ts-resolve.ts";
import type { LagLoadResult, LagSessionEvent } from "../../src/lag/client.ts";
import type { LagDownloadToPage, LagDownloadToWorker } from "../../src/lag/download.ts";

registerTsResolve();
const { openLagSession } = await import("../../src/lag/client.ts");
const { LAG_DOWNLOAD_BATCH_MS } = await import("../../src/lag/download.ts");
const { LAG_SPREAD_MAX_MS, LagIntervalSeries, nearestRank } = await import("../../src/lag/stats.ts");

// How a load's bytes are counted and reported (lag.dev/PLAN.md M4, M6, S5,
// amendment 14), on the session client's own code, with no network and no
// timing of its own: a stand-in for the download's Worker hands the client
// chunks at exact, synthetic arrival times, through the same messages the
// real Worker sends, and a scripted extender answers the control socket.

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

// a seeded generator, so a failure reproduces
const random = (seed: number) => () => {
  seed = (seed + 0x6d2b79f5) | 0;
  let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
  t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
  return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
};

const ofType = <T extends LagSessionEvent["type"]>(events: LagSessionEvent[], type: T) =>
  events.filter((event): event is Extract<LagSessionEvent, { type: T }> => event.type === type);

/** A chunk as the Worker times it: `at` is ms on the page's clock, performance.now(). */
interface Chunk {
  stream: number;
  at: number;
  bytes: number;
}

/** A download's streams: their chunks in order of arrival, and when any of them ended. */
interface Schedule {
  chunks: Chunk[];
  ends: Array<{ stream: number; at: number }>;
}

/**
 * The download's Worker, delivering a schedule: every LAG_DOWNLOAD_BATCH_MS,
 * and on a flush or a close, it hands over each chunk due by now with the
 * time the schedule gives it, says how far that covers (`through`), and
 * then reports each stream whose end is due. A schedule set in the past is
 * handed over whole in the first batch.
 */
class ScheduleWorker {
  /** the chunks handed over before the download was closed */
  readonly delivered: Chunk[] = [];
  /** performance.now() when the page closed the download */
  closedAt?: number;
  private readonly schedule: Schedule;
  private readonly listeners: Array<(event: { data: LagDownloadToPage }) => void> = [];
  private readonly runs = new Map<number, { next: number; ended: Set<number>; ticker: ReturnType<typeof setInterval> }>();

  constructor(schedule: Schedule) {
    this.schedule = schedule;
    setTimeout(() => this.post({ type: "ready" }), 0);
  }

  addEventListener(type: string, listener: (event: { data: LagDownloadToPage }) => void): void {
    if (type === "message") {
      this.listeners.push(listener);
    }
  }

  postMessage(message: LagDownloadToWorker): void {
    if (message.type === "close") {
      this.closedAt ??= performance.now();
    }
    setTimeout(() => this.receive(message), 0);
  }

  terminate(): void {
    for (const run of this.runs.values()) {
      clearInterval(run.ticker);
    }
    this.runs.clear();
  }

  private receive(message: LagDownloadToWorker): void {
    switch (message.type) {
      case "start": {
        const ticker = setInterval(() => this.send(message.id, "chunks"), LAG_DOWNLOAD_BATCH_MS);
        this.runs.set(message.id, { next: 0, ended: new Set(), ticker });
        this.send(message.id, "chunks");
        return;
      }
      case "flush":
        this.send(message.id, "flushed");
        return;
      case "close":
        this.send(message.id, "closed");
        clearInterval(this.runs.get(message.id)?.ticker);
        this.runs.delete(message.id);
        return;
    }
  }

  private send(id: number, type: "chunks" | "flushed" | "closed"): void {
    const run = this.runs.get(id);
    const now = performance.now();
    const items: number[] = [];
    const ended: number[] = [];
    if (run !== undefined) {
      const { chunks, ends } = this.schedule;
      while (run.next < chunks.length && chunks[run.next].at <= now) {
        const chunk = chunks[run.next];
        run.next += 1;
        items.push(chunk.stream, performance.timeOrigin + chunk.at, chunk.bytes);
        if (this.closedAt === undefined) {
          this.delivered.push(chunk);
        }
      }
      for (const end of ends) {
        if (end.at <= now && !run.ended.has(end.stream)) {
          run.ended.add(end.stream);
          ended.push(end.stream);
        }
      }
    }
    this.post({ type, id, items, through: performance.timeOrigin + now });
    for (const stream of ended) {
      this.post({ type: "ended", id, stream });
    }
  }

  private post(message: LagDownloadToPage): void {
    setTimeout(() => {
      for (const listener of this.listeners) {
        listener({ data: message });
      }
    }, 0);
  }
}

interface ScriptedHello {
  intervalMs: number;
  downloadMs: number;
  uploadMs: number;
  maxStreams: number;
}

interface Script {
  hello: ScriptedHello;
  /** `uploaded` reports, sent once the upload's first message arrives, `gapMs` apart */
  uploaded: Array<Record<string, unknown>>;
  gapMs?: number;
  /** the loadResult: after done, or after the last report when `early` */
  loadResult: Record<string, unknown>;
  early?: boolean;
}

type Listener = (event: Record<string, unknown>) => void;

/**
 * An extender as a WebSocket class: the control socket says hello, takes
 * the browser's messages, sends the script's `uploaded` reports once the
 * upload has begun and answers done with its loadResult; an upload socket
 * swallows what it is sent.
 */
const scriptedExtender = (script: Script) => {
  const sent: Array<Record<string, unknown>> = [];
  const received: Array<{ bytes: number; ms: number; when: number }> = [];
  let reportsSent = 0;
  let control: FakeSocket | undefined;
  let uploading = false;

  class FakeSocket {
    binaryType = "blob";
    readyState = 0;
    bufferedAmount = 0;
    readonly url: string;
    private readonly listeners = new Map<string, Set<Listener>>();

    constructor(url: string) {
      this.url = url;
      const isControl = url.includes("/lag/v1/session?");
      if (isControl) {
        control = this;
      }
      setTimeout(() => {
        if (this.readyState !== 0) {
          return;
        }
        this.readyState = 1;
        this.dispatch("open", {});
        if (isControl) {
          this.message({ type: "hello", session: "c2Vzc2lvbg", uploadKey: "AAAAAAAAAAAAAAAAAAAAAA", pingIntervalMs: 250, ...script.hello });
        }
      }, 1);
    }

    addEventListener(type: string, listener: Listener): void {
      if (!this.listeners.has(type)) {
        this.listeners.set(type, new Set());
      }
      this.listeners.get(type)!.add(listener);
    }

    removeEventListener(type: string, listener: Listener): void {
      this.listeners.get(type)?.delete(listener);
    }

    send(data: string | Uint8Array): void {
      if (typeof data !== "string") {
        if (!uploading) {
          uploading = true;
          void reportUpload();
        }
        return;
      }
      const message = JSON.parse(data) as Record<string, unknown>;
      sent.push(message);
      if (message.type === "received") {
        received.push({ bytes: message.bytes as number, ms: message.ms as number, when: performance.now() });
      }
      if (message.type === "load" && message.phase === "done" && !script.early) {
        setTimeout(() => this.message({ type: "loadResult", ...script.loadResult }), 1);
      }
    }

    close(code = 1000): void {
      if (this.readyState === 3) {
        return;
      }
      this.readyState = 3;
      setTimeout(() => this.dispatch("close", { code, reason: "" }), 1);
    }

    message(value: Record<string, unknown>): void {
      if (this.readyState === 1) {
        this.dispatch("message", { data: JSON.stringify(value) });
      }
    }

    private dispatch(type: string, event: Record<string, unknown>): void {
      for (const listener of [...(this.listeners.get(type) ?? [])]) {
        listener(event);
      }
    }
  }

  const reportUpload = async () => {
    for (const report of script.uploaded) {
      await sleep(script.gapMs ?? 1);
      reportsSent += 1;
      control?.message({ type: "uploaded", ...report });
    }
    if (script.early) {
      await sleep(script.gapMs ?? 1);
      control?.message({ type: "loadResult", ...script.loadResult });
    }
  };

  return {
    WebSocket: FakeSocket as unknown as typeof WebSocket,
    /** the browser's control messages */
    sent,
    /** its `received` reports, with performance.now() when each was sent */
    received,
    /** the `uploaded` reports sent so far */
    reportsSent: () => reportsSent,
  };
};

// One load against a scripted extender, its download from a schedule.
const runLoad = async (
  script: Script,
  schedule: Schedule,
  onEvent?: (event: LagSessionEvent, extender: ReturnType<typeof scriptedExtender>) => void,
) => {
  const extender = scriptedExtender(script);
  const worker = new ScheduleWorker(schedule);
  const events: LagSessionEvent[] = [];
  const session = openLagSession({
    host: "extender.test",
    ticket: "ticket",
    WebSocket: extender.WebSocket,
    downloadWorker: () => worker as unknown as Worker,
    onEvent: (event) => {
      events.push(event);
      onEvent?.(event, extender);
    },
  });
  await session.ready;
  let result: LagLoadResult;
  try {
    result = await session.startLoad();
  } finally {
    session.close();
    worker.terminate();
  }
  return { result, events, extender, worker };
};

// The reports' per-interval values: each one's bytes less the one before's.
const reportedIntervals = (received: Array<{ bytes: number }>) =>
  received.map((report, i) => report.bytes - (i === 0 ? 0 : received[i - 1].bytes));

/**
 * A download as four streams deliver it, the spread spec's model: stream k
 * carries a share w_k / Σw of the rate, each w_k 1 ± 30%, in 16 KiB records.
 * Its first record completes at a random point in its first gap and the
 * rest a gap apart, each up to `jitterMs` late. Times are ms from `start`,
 * up to `untilMs`.
 */
const streamSchedule = (
  mbps: number,
  seed: number,
  { start, untilMs, jitterMs = 5, recordBytes = 16384 }: { start: number; untilMs: number; jitterMs?: number; recordBytes?: number },
): Schedule => {
  const next = random(seed);
  const bytesPerMs = (mbps * 1e6) / 8 / 1000;
  const weights = [0, 1, 2, 3].map(() => 1 + (next() * 2 - 1) * 0.3);
  const total = weights.reduce((sum, weight) => sum + weight, 0);
  const chunks: Chunk[] = [];
  weights.forEach((weight, stream) => {
    const gap = recordBytes / ((bytesPerMs * weight) / total);
    let last = Number.NEGATIVE_INFINITY;
    for (let t = next() * gap; t < untilMs; t += gap) {
      // jitter delays a record, but a stream's records stay in order
      last = Math.max(last, t + next() * jitterMs);
      chunks.push({ stream, at: start + last, bytes: recordBytes });
    }
  });
  chunks.sort((a, b) => a.at - b.at);
  return { chunks, ends: [] };
};

// Rule S over a schedule's chunks (lag.dev/PLAN.md M6), written out on its
// own: per stream, each chunk spread evenly since the stream's previous one,
// at most LAG_SPREAD_MAX_MS back and never before the window's first byte;
// nothing at or after the window's end.
const ruleS = (chunks: Chunk[], windowMs: number, intervalMs: number): number[] => {
  const sums: number[] = Array(Math.ceil(windowMs / intervalMs)).fill(0);
  const first = chunks[0].at;
  const last = new Map<number, number>();
  for (const chunk of chunks) {
    const t = chunk.at - first;
    if (windowMs <= t) {
      continue;
    }
    const from = Math.max(last.get(chunk.stream) ?? 0, t - LAG_SPREAD_MAX_MS);
    last.set(chunk.stream, t);
    if (from === t) {
      sums[Math.floor(t / intervalMs)] += chunk.bytes;
      continue;
    }
    for (let i = Math.floor(from / intervalMs); i * intervalMs < t; i += 1) {
      sums[i] += (chunk.bytes * (Math.min(t, (i + 1) * intervalMs) - Math.max(from, i * intervalMs))) / (t - from);
    }
  }
  return sums;
};

const ACCURACY_RATES = [0.5, 1, 2, 3, 4, 5, 10, 20, 50];
const ACCURACY_WINDOW_MS = 5000;

// A 5 s download of a schedule's chunks counted by rule S, as the client
// counts them (LagIntervalSeries.addRead): from the window's first byte, and
// nothing at or after its end. Its series in whole bytes.
const ruleSSeries = (chunks: Chunk[]) => {
  const series = new LagIntervalSeries({ intervalMs: 250, maxIntervals: ACCURACY_WINDOW_MS / 250 });
  const first = chunks[0].at;
  for (const chunk of chunks) {
    const t = chunk.at - first;
    if (t < ACCURACY_WINDOW_MS) {
      series.addRead(chunk.stream, t, chunk.bytes);
    }
  }
  series.extendTo(ACCURACY_WINDOW_MS);
  return series;
};

test("rule S: over 200 downloads at each rate, every p50 is within 2% of the true rate from 1 to 50 Mbps, and their median within 5% at 0.5", (t) => {
  // Four streams at rates up to 30% apart, with up to 5 ms of jitter, in
  // 16 KiB records, over a 5 s window of 250 ms intervals: the extender's
  // own test of rule S reads these too (connect, amendment 14). At 0.5 Mbps
  // a stream's records come about LAG_SPREAD_MAX_MS apart or more, so one
  // download's p50 can be several percent off; the median of the 200 is
  // within 5%. Credited whole to the interval each arrived in, as before,
  // the same records put the median 4.9% high from 0.5 to 5 Mbps, and single
  // downloads as much as 48% low at 1 Mbps: steps of one record an interval.
  const rows = ACCURACY_RATES.map((mbps) => {
    const errors: number[] = [];
    for (let seed = 0; seed < 200; seed += 1) {
      const p50 = ruleSSeries(streamSchedule(mbps, seed, { start: 0, untilMs: 8000 }).chunks).percentiles()!.p50;
      errors.push((100 * (p50 - mbps)) / mbps);
    }
    const rank = (p: number) => nearestRank(errors, p)!;
    return { mbps, median: rank(50), p10: rank(10), p90: rank(90), min: Math.min(...errors), max: Math.max(...errors) };
  });
  const percent = (value: number) => `${value < 0 ? "" : "+"}${value.toFixed(2)}%`;
  t.diagnostic(
    `p50 error over 200 downloads, Mbps: median [p10, p90] (min, max): ${rows
      .map((row) => `${row.mbps}: ${percent(row.median)} [${percent(row.p10)}, ${percent(row.p90)}] (${percent(row.min)}, ${percent(row.max)})`)
      .join("; ")}`,
  );
  for (const row of rows) {
    if (row.mbps < 1) {
      assert.ok(Math.abs(row.median) <= 5, `${row.mbps} Mbps: the median p50 is off by ${percent(row.median)}`);
    } else {
      assert.ok(-2 <= row.min && row.max <= 2, `${row.mbps} Mbps: p50s off by ${percent(row.min)} to ${percent(row.max)}`);
    }
  }
});

test("the client counts each download chunk by rule S, reports that series, and reads within 2% of the true rate from 1 to 50 Mbps", { timeout: 60_000 }, async (t) => {
  // One download a rate, through the client: each schedule is set in the
  // past, so the Worker hands it over in one batch and the window closes at
  // once, and every interval is counted from the same chunks however busy
  // the machine is.
  const hello = { intervalMs: 250, downloadMs: ACCURACY_WINDOW_MS, uploadMs: 250, maxStreams: 4 };
  const rows = await Promise.all(
    ACCURACY_RATES.map(async (mbps, index) => {
      const schedule = streamSchedule(mbps, 1000 + index, { start: performance.now() - 10_000, untilMs: 8000 });
      const { result, extender } = await runLoad(
        { hello, uploaded: [{ bytes: 0, ms: 250, at: 0 }], loadResult: { uploadVerified: true, endReason: "complete" } },
        schedule,
      );
      const download = result.download;
      // its series is rule S's, give or take a byte for the chunks' times
      // on the page's clock
      const expected = ruleSSeries(schedule.chunks).bytes;
      assert.equal(download.intervalBytes.length, expected.length);
      download.intervalBytes.forEach((bytes, i) => {
        assert.ok(Math.abs(bytes - expected[i]) <= 1, `${mbps} Mbps, interval ${i}: ${bytes} bytes, rule S gives ${expected[i]}`);
      });
      // and what the extender signs is what the browser shows
      assert.deepEqual(reportedIntervals(extender.received), download.intervalBytes, `${mbps} Mbps`);
      const p50 = download.percentiles!.p50;
      return { mbps, p50, error: (100 * (p50 - mbps)) / mbps };
    }),
  );
  t.diagnostic(
    `p50 by rate: ${rows.map(({ mbps, p50, error }) => `${mbps} Mbps ${p50.toFixed(3)} (${error < 0 ? "" : "+"}${error.toFixed(2)}%)`).join(", ")}`,
  );
  for (const { mbps, p50, error } of rows) {
    if (1 <= mbps) {
      assert.ok(Math.abs(error) <= 2, `${mbps} Mbps read ${p50.toFixed(4)} (${error.toFixed(2)}%)`);
    }
  }
});

test("a download interval is reported once it has settled: with a slow stream, a stream that never opens, and through the window's end", { timeout: 30_000 }, async () => {
  // Streams 0 and 1 deliver a 16 KiB record about every 20 ms, stream 2 one
  // every 600 ms, and stream 3 never opens: it ends 300 ms in with nothing.
  // Each stream delivers past the 2 s window, live, from 100 ms after the
  // load starts. No record falls on an interval's end, where the clock's
  // rounding would decide its interval.
  const hello = { intervalMs: 250, downloadMs: 2000, uploadMs: 250, maxStreams: 4 };
  const start = performance.now() + 100;
  const chunks: Chunk[] = [];
  for (let t = 0; t < 3000; t += 20.3) {
    chunks.push({ stream: 0, at: start + t, bytes: 16384 }, { stream: 1, at: start + t + 7.1, bytes: 16384 });
  }
  for (let t = 450.7; t < 3000; t += 600) {
    chunks.push({ stream: 2, at: start + t, bytes: 16384 });
  }
  chunks.sort((a, b) => a.at - b.at);
  const schedule: Schedule = { chunks, ends: [{ stream: 3, at: start + 300 }] };
  const { result, events, extender, worker } = await runLoad(
    { hello, uploaded: [{ bytes: 0, ms: 250, at: 0 }], loadResult: { uploadVerified: true, endReason: "complete" } },
    schedule,
  );
  const download = result.download;
  assert.equal(download.streams, 3);
  assert.equal(download.intervalBytes.length, 8);
  // one report per interval, each with the interval's final value
  assert.deepEqual(extender.received.map((report) => report.ms), [250, 500, 750, 1000, 1250, 1500, 1750, 2000]);
  assert.deepEqual(reportedIntervals(extender.received), download.intervalBytes);
  // and that value is rule S's over what the Worker handed over
  const expected = ruleS(worker.delivered, hello.downloadMs, hello.intervalMs);
  download.intervalBytes.forEach((bytes, i) => {
    assert.ok(Math.abs(bytes - expected[i]) <= 1, `interval ${i}: ${bytes} bytes, rule S gives ${expected[i]}`);
  });

  // Each interval's progress event goes out with its report, carrying its
  // value, at the page's time since the first byte, which an interval
  // waiting to settle does not hold back (the slow stream holds some for
  // hundreds of ms): never ahead of the clock or past the window, never
  // going back, and the window's length once it has closed.
  const first = worker.delivered[0].at;
  const progress = ofType(events, "progress").filter((event) => event.direction === "download");
  assert.deepEqual(progress.map((event) => event.interval), [0, 1, 2, 3, 4, 5, 6, 7]);
  assert.deepEqual(progress.map((event) => event.bytes), extender.received.map((report) => report.bytes));
  progress.forEach((event, i) => {
    const elapsed = Math.min(hello.downloadMs, extender.received[i].when - first);
    assert.ok(elapsed - 100 < event.ms && event.ms <= elapsed + 0.01, `interval ${i}'s event says ${event.ms} ms, ${elapsed} ms in`);
    assert.ok(i === 0 || progress[i - 1].ms <= event.ms, "the progress went back");
  });
  assert.equal(progress.at(-1)!.ms, hello.downloadMs);

  // No interval went out before it settled: by then the clock had passed
  // its end, and each open stream had had a chunk at or after its end, or
  // 1 s had passed since. Only the reports at the window's close, once the
  // download had settled everything, may come sooner.
  const closedAt = worker.closedAt!;
  let early = 0;
  extender.received.forEach((report, i) => {
    if (closedAt <= report.when) {
      return;
    }
    early += 1;
    const end = first + (i + 1) * hello.intervalMs;
    assert.ok(end <= report.when, `interval ${i} was reported before its end`);
    for (let stream = 0; stream < 4; stream += 1) {
      const ended = schedule.ends.some((e) => e.stream === stream && e.at <= report.when);
      const chunkPast = worker.delivered.some((c) => c.stream === stream && end <= c.at && c.at <= report.when);
      const spreadOver = end + LAG_SPREAD_MAX_MS <= report.when;
      assert.ok(ended || chunkPast || spreadOver, `interval ${i} was reported while stream ${stream} could still spread into it`);
    }
  });
  // the slow stream held intervals back, but they settled during the window
  assert.ok(2 <= early, `${early} intervals reported before the window closed`);
});

const uploadHello = { intervalMs: 250, downloadMs: 250, uploadMs: 1000, maxStreams: 2 };

// a download that is over at once: two streams of one record each
const briefDownload = (): Schedule => {
  const start = performance.now() - 2000;
  return {
    chunks: [
      { stream: 0, at: start, bytes: 16384 },
      { stream: 1, at: start + 10, bytes: 16384 },
    ],
    ends: [
      { stream: 0, at: start + 20 },
      { stream: 1, at: start + 20 },
    ],
  };
};

// The extender's reports over a 1 s upload window, one each interval, the
// second a repeat of the first; `at` left out for an older extender.
const uploadReports = (withAt: boolean) =>
  [
    { bytes: 100_000, ms: 250, at: 240 },
    { bytes: 100_000, ms: 250, at: 240 },
    { bytes: 250_000, ms: 500, at: 495 },
    { bytes: 400_000, ms: 750, at: 745 },
    { bytes: 520_000, ms: 1000, at: 990 },
  ].map(({ at, ...report }) => (withAt ? { ...report, at } : report));

// The live series those reports make (S5), written out on its own: each
// report's added bytes spread evenly since the point before, the point
// being `at`, or `ms` without it.
const liveSeries = (reports: Array<{ bytes: number; ms: number; at?: number }>, intervals: number, intervalMs: number) => {
  const sums: number[] = Array(intervals).fill(0);
  let before = { at: 0, bytes: 0 };
  for (const report of reports) {
    const point = { at: Math.max(before.at, report.at ?? report.ms), bytes: Math.max(before.bytes, report.bytes) };
    const added = point.bytes - before.bytes;
    for (let i = 0; i < intervals && 0 < added; i += 1) {
      const overlap = Math.min(point.at, (i + 1) * intervalMs) - Math.max(before.at, i * intervalMs);
      if (0 < overlap) {
        sums[i] += (added * overlap) / (point.at - before.at);
      }
    }
    before = point;
  }
  return sums;
};

for (const withAt of [true, false]) {
  test(`upload progress comes once per completed interval of the live series, ${withAt ? "built from `at`" : "built from `ms` when the extender sends no `at`"}`, { timeout: 30_000 }, async () => {
    const reports = uploadReports(withAt);
    const progress: Array<{ interval: number; report: number; mbps: number; bytes: number; ms: number }> = [];
    const { result } = await runLoad(
      { hello: uploadHello, uploaded: reports, gapMs: 20, loadResult: { uploadVerified: true, endReason: "complete" } },
      briefDownload(),
      (event, extender) => {
        if (event.type === "progress" && event.direction === "upload") {
          progress.push({ interval: event.interval, report: extender.reportsSent(), mbps: event.mbps, bytes: event.bytes, ms: event.ms });
        }
      },
    );
    // Interval i is complete once the series' latest point is at or past
    // its end, or the window is over: from `at`, interval 0 waits for the
    // third report (at 495), and the window's end completes the last two;
    // from `ms`, each report completes the interval it ends. The repeated
    // report adds nothing and sends nothing.
    assert.deepEqual(progress.map((event) => event.interval), [0, 1, 2, 3]);
    assert.deepEqual(progress.map((event) => event.report), withAt ? [3, 4, 5, 5] : [1, 3, 4, 5]);
    // and goes out at the extender's time, its latest report's ms, which
    // the page's arc follows
    assert.deepEqual(progress.map((event) => event.ms), withAt ? [500, 750, 1000, 1000] : [250, 500, 750, 1000]);
    // each carries its interval's value in the live series, in whole bytes
    const sums = liveSeries(reports, 4, 250);
    let through = 0;
    progress.forEach((event, i) => {
      through += sums[i];
      assert.ok(through - 1.01 < event.bytes && event.bytes <= through + 0.01, `interval ${i}: ${event.bytes} bytes through it, ${through} in the series`);
      const wholeBytes = event.bytes - (i === 0 ? 0 : progress[i - 1].bytes);
      assert.equal(event.mbps, (wholeBytes * 8) / (250 * 1000));
    });
    // without uploadBytes in the loadResult, the live series is the result
    assert.deepEqual(
      result.upload.intervalBytes,
      progress.map((event, i) => event.bytes - (i === 0 ? 0 : progress[i - 1].bytes)),
    );
    assert.equal(result.upload.bytes, 520_000);
    assert.equal(result.upload.durationMs, 1000);
  });
}

// The API's derivation of a stored series' speeds (A5; server/controller/
// lag_report_controller.go): lagLoadIntervals keeps the first
// ceil(loadMs / intervalMs) intervals, lagIntervalMbps turns each into Mbps,
// and lagNearestRank picks the p50 and p95 by nearest rank, as float32.
const serverSpeeds = (byteCounts: number[], loadMs: number, intervalMs: number) => {
  const kept = loadMs === 0 ? [] : byteCounts.slice(0, Math.min(Math.floor((loadMs + intervalMs - 1) / intervalMs), byteCounts.length));
  const mbps = kept.map((bytes) => (bytes * 8) / (intervalMs * 1000));
  const nearestRank = (p: number) => {
    if (mbps.length === 0) {
      return undefined;
    }
    const sorted = [...mbps].sort((a, b) => a - b);
    const rank = Math.ceil((p / 100) * sorted.length);
    return Math.fround(sorted[Math.min(Math.max(rank - 1, 0), sorted.length - 1)]);
  };
  return { kept, p50: nearestRank(50), p95: nearestRank(95) };
};

test("the upload's result is the extender's series from loadResult, cut to its window as the API cuts it", { timeout: 30_000 }, async () => {
  // a full window: the extender's series as it came, whatever the live one
  // made of the reports
  const full = [120_000, 150_000, 140_000, 110_000];
  const complete = await runLoad(
    {
      hello: uploadHello,
      uploaded: uploadReports(true),
      loadResult: { uploadVerified: true, endReason: "complete", uploadBytes: full },
    },
    briefDownload(),
  );
  const server = serverSpeeds(full, 1000, 250);
  assert.deepEqual(complete.result.upload.intervalBytes, full);
  assert.deepEqual(complete.result.upload.mbps, [3.84, 4.8, 4.48, 3.52]);
  assert.equal(Math.fround(complete.result.upload.percentiles!.p50), server.p50);
  assert.equal(Math.fround(complete.result.upload.percentiles!.p95), server.p95);
  assert.equal(complete.result.upload.bytes, 520_000);
  // the live progress still came from the reports
  const live = ofType(complete.events, "progress").filter((event) => event.direction === "upload");
  assert.deepEqual(live.map((event) => event.interval), [0, 1, 2, 3]);

  // the extender leaves out trailing empty intervals, and the API counts
  // only those it sent: the browser never pads them back
  const short = [130_000, 160_000, 230_000];
  const trimmed = await runLoad(
    {
      hello: uploadHello,
      uploaded: uploadReports(true),
      loadResult: { uploadVerified: true, endReason: "complete", uploadBytes: short },
    },
    briefDownload(),
  );
  const shortServer = serverSpeeds(short, 1000, 250);
  assert.deepEqual(shortServer.kept, short);
  assert.deepEqual(trimmed.result.upload.intervalBytes, short);
  assert.equal(Math.fround(trimmed.result.upload.percentiles!.p50), shortServer.p50);
  assert.equal(Math.fround(trimmed.result.upload.percentiles!.p95), shortServer.p95);
  // padded with a zero, its p50 would have been 4.16 Mbps, not 5.12
  assert.equal(shortServer.p50, Math.fround(5.12));

  // a load the extender cut short at 610 ms, as a cap does: its last report
  // says so, and only the first ceil(610 / 250) = 3 intervals count, as the
  // API counts them; a byte counted just past that stays out
  const cut = [100_000, 100_000, 60_000, 5];
  const capped = await runLoad(
    {
      hello: uploadHello,
      uploaded: [
        { bytes: 100_000, ms: 250, at: 240 },
        { bytes: 200_000, ms: 500, at: 498 },
        { bytes: 260_005, ms: 610, at: 610 },
      ],
      loadResult: { uploadVerified: true, endReason: "cap", uploadBytes: cut },
      early: true,
    },
    briefDownload(),
  );
  const cutServer = serverSpeeds(cut, 610, 250);
  assert.equal(capped.result.endReason, "cap");
  assert.deepEqual(cutServer.kept, [100_000, 100_000, 60_000]);
  assert.deepEqual(capped.result.upload.intervalBytes, cutServer.kept);
  assert.equal(Math.fround(capped.result.upload.percentiles!.p50), cutServer.p50);
  assert.equal(Math.fround(capped.result.upload.percentiles!.p95), cutServer.p95);
  assert.equal(capped.result.upload.durationMs, 610);

  // an extender that counted nothing sends an empty series: no speeds, as
  // the API derives none
  const empty = await runLoad(
    {
      hello: uploadHello,
      uploaded: [{ bytes: 0, ms: 1000, at: 0 }],
      loadResult: { uploadVerified: true, endReason: "complete", uploadBytes: [] },
    },
    briefDownload(),
  );
  assert.deepEqual(empty.result.upload.intervalBytes, []);
  assert.equal(empty.result.upload.percentiles, undefined);
});

test("an upload series the extender sends malformed is set aside for the browser's own", { timeout: 30_000 }, async () => {
  const run = (uploadBytes: unknown) =>
    runLoad(
      {
        hello: uploadHello,
        uploaded: uploadReports(true),
        loadResult: { uploadVerified: true, endReason: "complete", ...(uploadBytes === undefined ? {} : { uploadBytes }) },
      },
      briefDownload(),
    );
  const own = (await run(undefined)).result.upload;
  assert.equal(own.intervalBytes.length, 4);
  const malformed: unknown[] = [
    // a negative count, a string, a number JSON cannot carry (NaN goes as null)
    [100, -1, 100, 100],
    [100, "100", 100, 100],
    [100, Number.NaN, 100, 100],
    // more intervals than the window holds
    [100, 100, 100, 100, 100],
    "100,100,100,100",
    { 0: 100 },
    null,
  ];
  const results = await Promise.all(malformed.map((value) => run(value)));
  results.forEach(({ result }, i) => {
    assert.deepEqual({ ...result.upload, startedAt: undefined }, { ...own, startedAt: undefined }, `uploadBytes ${JSON.stringify(malformed[i])}`);
  });
});
