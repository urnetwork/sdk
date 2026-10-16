import { test } from "node:test";
import assert from "node:assert/strict";
import { registerTsResolve } from "../ts-resolve.ts";
import type { LagLoadResult, LagSessionEvent } from "../../src/lag/client.ts";
import type { LagDownloadToPage, LagDownloadToWorker } from "../../src/lag/download.ts";

registerTsResolve();
const { openLagSession, LagSessionError } = await import("../../src/lag/client.ts");
const { LAG_DOWNLOAD_BATCH_MS } = await import("../../src/lag/download.ts");
const stats = await import("../../src/lag/stats.ts");
const { LagIntervalSeries, nearestRank } = stats;

// How a load's bytes are counted and reported (lag.dev/PLAN.md M4, M6, S5,
// amendments 14 and 15), on the session client's own code, with no network
// and no timing of its own: a stand-in for the download's Worker hands the
// client chunks at exact, synthetic arrival times, through the same messages
// the real Worker sends, and a scripted extender answers the control socket.

// the rules' constants, as the spec gives them (lag.dev/PLAN.md M6,
// amendment 15), for the rules written out on their own below
const SPREAD_MAX_MS = 1000;
const SPREAD_JOIN_MS = 2;

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
  /** the chunks handed over before the download was closed, and when (performance.now()) */
  readonly delivered: Chunk[] = [];
  readonly deliveredAt: number[] = [];
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
          this.deliveredAt.push(now);
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
  /**
   * `uploaded` reports, sent once the upload's first message arrives: each
   * `afterMs` after that message, or else `gapMs` after the one before
   */
  uploaded: Array<Record<string, unknown> & { afterMs?: number }>;
  gapMs?: number;
  /** a report without `bytes` carries every upload byte the extender has been sent */
  countBytes?: boolean;
  /** round trips the extender reports right after its hello */
  rtts?: number[];
  /** the loadResult: after done, or after the last report when `early` */
  loadResult: Record<string, unknown>;
  early?: boolean;
  /**
   * The load ends after its download: the hello's upload key is not 16
   * bytes, so the upload fails at once and the load's error carries what the
   * download measured. No upload keystream is generated, which lets a test
   * run hundreds of downloads.
   */
  downloadOnly?: boolean;
}

type Listener = (event: Record<string, unknown>) => void;

/**
 * An extender as a WebSocket class: the control socket says hello, takes
 * the browser's messages, sends the script's `uploaded` reports once the
 * upload has begun and answers done with its loadResult; an upload socket
 * swallows what it is sent, and the extender notes when.
 */
const scriptedExtender = (script: Script) => {
  const sent: Array<Record<string, unknown>> = [];
  const received: Array<{ bytes: number; ms: number; when: number }> = [];
  // the upload's messages, the reports sent, all at performance.now()
  const uploadSends: Array<{ at: number; bytes: number }> = [];
  const reports: Array<{ at: number; ms: number; bytes: number }> = [];
  const times: { uploadFirst?: number; uploadClosed?: number; done?: number } = {};
  let uploadBytes = 0;
  let reportsSent = 0;
  let control: FakeSocket | undefined;

  class FakeSocket {
    binaryType = "blob";
    readyState = 0;
    bufferedAmount = 0;
    readonly url: string;
    private readonly upload: boolean;
    private readonly listeners = new Map<string, Set<Listener>>();

    constructor(url: string) {
      this.url = url;
      const isControl = url.includes("/lag/v1/session?");
      this.upload = url.includes("/lag/v1/up?");
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
          const uploadKey = script.downloadOnly ? "AAAA" : "AAAAAAAAAAAAAAAAAAAAAA";
          this.message({ type: "hello", session: "c2Vzc2lvbg", uploadKey, pingIntervalMs: 250, ...script.hello });
          for (const ms of script.rtts ?? []) {
            this.message({ type: "rtt", t: 0, ms });
          }
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
        const now = performance.now();
        uploadSends.push({ at: now, bytes: data.byteLength });
        uploadBytes += data.byteLength;
        if (times.uploadFirst === undefined) {
          times.uploadFirst = now;
          void reportUpload();
        }
        return;
      }
      const message = JSON.parse(data) as Record<string, unknown>;
      sent.push(message);
      if (message.type === "received") {
        received.push({ bytes: message.bytes as number, ms: message.ms as number, when: performance.now() });
      }
      if (message.type === "load" && message.phase === "done") {
        times.done ??= performance.now();
        if (!script.early) {
          setTimeout(() => this.message({ type: "loadResult", ...script.loadResult }), 1);
        }
      }
    }

    close(code = 1000): void {
      if (this.readyState === 3) {
        return;
      }
      if (this.upload) {
        times.uploadClosed ??= performance.now();
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
    for (const { afterMs, ...scripted } of script.uploaded) {
      await sleep(afterMs === undefined ? (script.gapMs ?? 1) : Math.max(0, times.uploadFirst! + afterMs - performance.now()));
      const report = script.countBytes && scripted.bytes === undefined ? { ...scripted, bytes: uploadBytes } : scripted;
      reportsSent += 1;
      reports.push({ at: performance.now(), ms: report.ms as number, bytes: report.bytes as number });
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
    /** the upload's messages as they were sent, and the reports the extender sent */
    uploadSends,
    reports,
    /** when the first upload message came, an upload socket was first closed, and done came */
    times,
    /** the `uploaded` reports sent so far */
    reportsSent: () => reportsSent,
  };
};

// One load against a scripted extender, its download from a schedule. A
// download-only load's result is what its error says the download measured.
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
    assert.ok(!script.downloadOnly, "a download-only load ran its upload");
  } catch (error) {
    if (!(script.downloadOnly && error instanceof LagSessionError && error.code === "protocol" && error.partial !== undefined)) {
      throw error;
    }
    result = error.partial;
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
 * A download as four streams deliver it, the spread spec's model (amendment
 * 14): stream k carries a share w_k / Σw of the rate, each w_k 1 ± 30%, in
 * 16 KiB records. Its first record completes at a random point in its first
 * gap and the rest a gap apart, each up to `jitterMs` late. Times are ms
 * from `start`, up to `untilMs`.
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

/**
 * A download at the window's edges, as amendment 15's simulation models it: four
 * streams start sending together, stream k at a share w_k / Σw of the rate
 * with each w_k 1 ± 30%, in 16 KiB records, and keep sending well past the
 * window's end. A stream's first record completes one record's time after
 * the start, and each record comes up to 3 ms late. With `stallChance`,
 * each record may begin a stall of 0.2 to 0.6 s, as TCP recovering a lost
 * packet holds back what came after it, and the records it held come back
 * to back once it ends, 10 µs apart. Times are ms from `start`, which is the
 * window's first byte: the first record to arrive on any stream.
 */
const edgesSchedule = (mbps: number, seed: number, stallChance: number, start: number): Schedule => {
  const next = random(seed);
  const bytesPerMs = (mbps * 1e6) / 8 / 1000;
  const weights = [0, 1, 2, 3].map(() => 1 + (next() * 2 - 1) * 0.3);
  const total = weights.reduce((sum, weight) => sum + weight, 0);
  const chunks: Chunk[] = [];
  weights.forEach((weight, stream) => {
    const gap = 16384 / ((bytesPerMs * weight) / total);
    let held = 0;
    let stalledUntil = Number.NEGATIVE_INFINITY;
    for (let t = gap; t < 9500; t += gap) {
      if (0 < stallChance && stalledUntil < t && next() < stallChance) {
        stalledUntil = t + 200 + next() * 400;
      }
      if (t < stalledUntil) {
        held += 1;
        continue;
      }
      for (let k = 0; k < held; k += 1) {
        chunks.push({ stream, at: t + k * 0.01, bytes: 16384 });
      }
      held = 0;
      chunks.push({ stream, at: t + next() * 3, bytes: 16384 });
    }
  });
  chunks.sort((a, b) => a.at - b.at);
  const first = chunks[0].at;
  return { chunks: chunks.map((chunk) => ({ ...chunk, at: start + chunk.at - first })), ends: [] };
};

/**
 * The counting rules written out on their own, as amendment 15's simulation
 * writes them (lag.dev/PLAN.md M6, amendments 14 and 15), to hold the client
 * to. Per
 * stream, from the window's first byte, the first chunk at time 0:
 * - a chunk that comes within SPREAD_JOIN_MS of its group's first chunk
 *   shares the group's span; any other starts a group, whose span starts at
 *   the stream's previous chunk, at most SPREAD_MAX_MS back;
 * - a stream's first group waits for its second, then counts at the rate of
 *   the second's first chunk, at most SPREAD_MAX_MS back, and only from the
 *   window's first byte on; with no second, by rule S, when its stream ends
 *   or the window is final;
 * - only the part of a span before the window's end counts; a stream is
 *   done SPREAD_JOIN_MS after the first chunk of its first group to begin at
 *   or after the end, and nothing it reads after counts or places a first
 *   group it still holds; and nothing that arrives more than SPREAD_MAX_MS
 *   after the end counts.
 * Bytes per interval, as fractions.
 */
const edgeRules = (schedule: Schedule, windowMs: number, intervalMs: number): number[] => {
  const sums: number[] = Array(Math.ceil(windowMs / intervalMs)).fill(0);
  const add = (from: number, to: number, bytes: number) => {
    if (to <= from) {
      if (0 <= to && to < windowMs) {
        sums[Math.floor(to / intervalMs)] += bytes;
      }
      return;
    }
    for (let i = 0; i < sums.length; i += 1) {
      const overlap = Math.min(to, windowMs, (i + 1) * intervalMs) - Math.max(from, 0, i * intervalMs);
      if (0 < overlap) {
        sums[i] += (bytes * overlap) / (to - from);
      }
    }
  };
  interface Stream {
    last: number;
    groupFirst: number;
    groupFrom: number;
    held: { bytes: number; last: number } | null;
    ended: boolean;
    doneAt: number;
  }
  const streams = new Map<number, Stream>();
  const placeHeld = (stream: Stream, t2?: number, bytes?: number) => {
    const { bytes: held, last: t1 } = stream.held!;
    stream.held = null;
    if (t2 !== undefined && bytes !== undefined) {
      add(Math.max(t1 - (held * (t2 - t1)) / bytes, t1 - SPREAD_MAX_MS), t1, held);
    } else {
      add(Math.max(0, t1 - SPREAD_MAX_MS), t1, held);
    }
  };
  const first = schedule.chunks[0].at;
  const events = [
    ...schedule.chunks.map((chunk) => ({ ...chunk, end: false })),
    ...schedule.ends.map((end) => ({ ...end, bytes: 0, end: true })),
  ].sort((a, b) => a.at - b.at || Number(a.end) - Number(b.end));
  for (const event of events) {
    const t = event.at - first;
    if (windowMs + SPREAD_MAX_MS < t) {
      break;
    }
    const stream = streams.get(event.stream);
    if (event.end) {
      if (stream?.held) {
        placeHeld(stream);
      }
      streams.set(event.stream, { ...(stream ?? { last: 0, groupFirst: 0, groupFrom: 0, held: null, doneAt: Infinity }), ended: true });
      continue;
    }
    if (stream === undefined) {
      const doneAt = windowMs <= t ? t + SPREAD_JOIN_MS : Infinity;
      streams.set(event.stream, { last: t, groupFirst: t, groupFrom: t, held: { bytes: event.bytes, last: t }, ended: false, doneAt });
      continue;
    }
    if (stream.ended || stream.doneAt < t) {
      continue;
    }
    let from: number;
    if (t - stream.groupFirst <= SPREAD_JOIN_MS) {
      if (stream.held) {
        stream.held.bytes += event.bytes;
        stream.held.last = t;
        stream.last = t;
        continue;
      }
      from = stream.groupFrom;
    } else {
      if (stream.held) {
        placeHeld(stream, t, event.bytes);
      }
      from = Math.max(stream.last, t - SPREAD_MAX_MS, 0);
      stream.groupFirst = t;
      stream.groupFrom = from;
      if (windowMs <= t && stream.doneAt === Infinity) {
        stream.doneAt = t + SPREAD_JOIN_MS;
      }
    }
    stream.last = t;
    add(from, t, event.bytes);
  }
  for (const stream of streams.values()) {
    if (stream.held) {
      placeHeld(stream);
    }
  }
  return sums;
};

// A schedule's chunks counted by the series as the client counts them
// (LagIntervalSeries.addRead): from the window's first byte, each read
// past the end counting what it carried before it, until the window is
// final. Its series in whole bytes.
const seriesOf = (chunks: Chunk[], windowMs = 5000, intervalMs = 250) => {
  const series = new LagIntervalSeries({ intervalMs, maxIntervals: Math.ceil(windowMs / intervalMs), windowMs });
  const first = chunks[0].at;
  for (const chunk of chunks) {
    series.addRead(chunk.stream, chunk.at - first, chunk.bytes);
  }
  series.finish();
  series.extendTo(windowMs);
  return series;
};

// whole bytes per interval, the floors of the running sum, from fractions
const wholeBytes = (sums: number[]) => {
  let total = 0;
  let before = 0;
  return sums.map((bytes) => {
    total += bytes;
    const through = Math.floor(total + 1e-6);
    const out = through - before;
    before = through;
    return out;
  });
};

test("the rules' constants are the spec's", () => {
  assert.equal(stats.LAG_SPREAD_MAX_MS, SPREAD_MAX_MS);
  assert.equal(stats.LAG_SPREAD_JOIN_MS, SPREAD_JOIN_MS);
});

const ACCURACY_RATES = [0.5, 1, 2, 3, 4, 5, 10, 20, 50];
const ACCURACY_WINDOW_MS = 5000;

test("the counting: over 200 downloads at each rate, every p50 is within 2% of the true rate from 1 to 50 Mbps, and their median within 5% at 0.5", (t) => {
  // Four streams at rates up to 30% apart, with up to 5 ms of jitter, in
  // 16 KiB records, over a 5 s window of 250 ms intervals (amendment 14's
  // model: each stream's first record completes at a random point in its
  // first gap): the extender's own test of the counting reads these too. At
  // 0.5 Mbps a stream's records come about LAG_SPREAD_MAX_MS apart or more,
  // so one download's p50 can be several percent off; the median of the 200
  // is within 5%. Credited whole to the interval each arrived in, as before
  // amendment 14, the same records put the median 4.9% high from 0.5 to
  // 5 Mbps, and single downloads as much as 48% low at 1 Mbps: steps of one
  // record an interval.
  const rows = ACCURACY_RATES.map((mbps) => {
    const errors: number[] = [];
    for (let seed = 0; seed < 200; seed += 1) {
      const p50 = seriesOf(streamSchedule(mbps, seed, { start: 0, untilMs: 8000 }).chunks).percentiles()!.p50;
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

test("the client counts each download chunk by the rules, reports that series, and reads within 2% of the true rate from 1 to 50 Mbps", { timeout: 60_000 }, async (t) => {
  // One download a rate, through the client: each schedule is set in the
  // past, so the Worker hands it over in one batch and the window is final
  // at once, and every interval is counted from the same chunks however
  // busy the machine is.
  const hello = { intervalMs: 250, downloadMs: ACCURACY_WINDOW_MS, uploadMs: 250, maxStreams: 4 };
  const rows = await Promise.all(
    ACCURACY_RATES.map(async (mbps, index) => {
      const schedule = streamSchedule(mbps, 1000 + index, { start: performance.now() - 10_000, untilMs: 8000 });
      const { result, extender } = await runLoad(
        { hello, uploaded: [{ bytes: 0, ms: 250, at: 0 }], loadResult: { uploadVerified: true, endReason: "complete" } },
        schedule,
      );
      const download = result.download;
      // its series is the rules', as the series counts them and as they are
      // written out on their own, give or take a byte for the chunks' times
      // on the page's clock
      const expected = seriesOf(schedule.chunks).bytes;
      const written = wholeBytes(edgeRules(schedule, ACCURACY_WINDOW_MS, 250));
      assert.equal(download.intervalBytes.length, expected.length);
      download.intervalBytes.forEach((bytes, i) => {
        assert.ok(Math.abs(bytes - expected[i]) <= 1, `${mbps} Mbps, interval ${i}: ${bytes} bytes, the series gives ${expected[i]}`);
        assert.ok(Math.abs(bytes - written[i]) <= 1, `${mbps} Mbps, interval ${i}: ${bytes} bytes, the rules give ${written[i]}`);
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

// Amendment 15's acceptance (lag.dev/PLAN.md M6): over 100 downloads a rate,
// the medians of the p50, the p95 and the first and last intervals, as a
// share of the true rate, within these. 0.5 Mbps has wider marks: a
// stream's records come a second or more apart there.
const EDGES_RATES = [0.5, 1, 2, 3, 5, 10, 20];
const EDGES_MARKS = { p50: [5, 2], p95: [12, 6], first: [15, 10], last: [15, 10] } as const;

for (const stallChance of [0, 0.03]) {
  test(`at the window's edges and in bursts, ${stallChance === 0 ? "without stalls" : "with stalls released back to back"}: over 100 downloads at each rate, the median p50, p95 and first and last intervals are the true rate's`, { timeout: 600_000 }, async (t) => {
    // that simulation's downloads through the client (amendment 15): four streams
    // start together, so most of the record that opens the window, and of
    // the first records the others have on their way, arrived before it;
    // they send on past the window's end, so a record straddles it; and
    // with stalls, a stream holds its records back for 0.2 to 0.6 s and
    // releases them back to back. Each load ends after its download, so a
    // hundred of them run at once.
    const hello = { intervalMs: 250, downloadMs: 5000, uploadMs: 250, maxStreams: 4 };
    const rows: Array<{ mbps: number; p50: number; p95: number; first: number; last: number }> = [];
    for (const mbps of EDGES_RATES) {
      const runs = await Promise.all(
        Array.from({ length: 100 }, async (_, seed) => {
          const schedule = edgesSchedule(mbps, 7919 * seed + Math.round(mbps * 1000) + (stallChance === 0 ? 0 : 1), stallChance, performance.now() - 12_000);
          const { result, extender } = await runLoad(
            { hello, uploaded: [], loadResult: {}, downloadOnly: true },
            schedule,
          );
          const { mbps: series, intervalBytes } = result.download;
          assert.equal(series.length, 20);
          // what the extender signs is what the browser shows
          assert.deepEqual(reportedIntervals(extender.received), intervalBytes);
          const share = (value: number) => (100 * value) / mbps;
          return {
            p50: share(nearestRank(series, 50)!),
            p95: share(nearestRank(series, 95)!),
            first: share(series[0]),
            last: share(series[series.length - 1]),
          };
        }),
      );
      const median = (key: "p50" | "p95" | "first" | "last") => nearestRank(runs.map((run) => run[key]), 50)!;
      rows.push({ mbps, p50: median("p50"), p95: median("p95"), first: median("first"), last: median("last") });
    }
    t.diagnostic(
      `median over 100 downloads, % of the true rate (p50 p95 first last): ${rows
        .map((row) => `${row.mbps} Mbps ${[row.p50, row.p95, row.first, row.last].map((value) => value.toFixed(1)).join(" ")}`)
        .join("; ")}`,
    );
    for (const row of rows) {
      for (const key of ["p50", "p95", "first", "last"] as const) {
        const mark = EDGES_MARKS[key][row.mbps < 1 ? 0 : 1];
        assert.ok(Math.abs(row[key] - 100) <= mark, `${row.mbps} Mbps: the median ${key} is ${row[key].toFixed(1)}% of the rate, outside ±${mark}%`);
      }
    }
  });
}

test("a download interval is reported once it has settled: with a slow stream, a stream that never opens, held first groups, and past the window's end until it is final", { timeout: 30_000 }, async () => {
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
  // and that value is the rules' over what the Worker handed over
  const expected = edgeRules({ chunks: worker.delivered, ends: schedule.ends }, hello.downloadMs, hello.intervalMs);
  download.intervalBytes.forEach((bytes, i) => {
    assert.ok(Math.abs(bytes - expected[i]) <= 1, `interval ${i}: ${bytes} bytes, the rules give ${expected[i]}`);
  });
  // The window was final once streams 0, 1 and 2 were each done, 2 ms after
  // the first record of their first group to begin after the end: stream
  // 2's at 2250.7 ms, which straddles the end and counted the part of its
  // span before it. Done only on a record whose span starts after the end,
  // it would have waited for the next, at 2850.7.
  const first = worker.delivered[0].at;
  assert.equal(first, start);
  assert.ok(start + 2250.7 + SPREAD_JOIN_MS < worker.closedAt!, `the download closed ${worker.closedAt! - first} ms in, before its window was final`);
  assert.ok(worker.closedAt! < first + 2850, `the download closed ${worker.closedAt! - first} ms in`);

  // Each interval's progress event goes out with its report, carrying its
  // value, at the page's time since the first byte, which an interval
  // waiting to settle does not hold back (the slow stream holds some for
  // hundreds of ms): never ahead of the clock or past the window, never
  // going back, and the window's length once it has closed.
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
  // 1 s had passed since; and a stream whose first group was still held,
  // waiting for its second, reached no further back than 1 s before the
  // group's last chunk. Only the reports at the window's close, once the
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
      const seen = worker.delivered.filter((c, k) => c.stream === stream && worker.deliveredAt[k] <= report.when);
      const chunkPast = seen.some((c) => end <= c.at);
      const spreadOver = end + SPREAD_MAX_MS <= report.when;
      assert.ok(ended || chunkPast || spreadOver, `interval ${i} was reported while stream ${stream} could still spread into it`);
      const held = 0 < seen.length && seen.every((c) => c.at - seen[0].at <= SPREAD_JOIN_MS);
      if (held && !ended) {
        assert.ok(end <= seen.at(-1)!.at - SPREAD_MAX_MS, `interval ${i} was reported while stream ${stream}'s first group could reach it`);
      }
    }
  });
  // the slow stream held intervals back, but they settled during the window
  assert.ok(2 <= early, `${early} intervals reported before the window closed`);
});

test("a stream's first group waits for its second and counts at its rate, and the intervals it can reach wait with it", { timeout: 30_000 }, async () => {
  // Streams 0 and 1 deliver a 16 KiB record every 25 ms from the first
  // byte. Stream 2's first record comes 900 ms in and its second 1700 ms
  // in, so its first counts as having arrived at the second's rate, over
  // the 800 ms before it: [100, 900]. Until the second comes, that record
  // could reach back to 1 s before it, past the window's first byte, so no
  // interval settles. Stream 3 ends at once. The schedule runs live.
  const hello = { intervalMs: 250, downloadMs: 2000, uploadMs: 250, maxStreams: 4 };
  const start = performance.now() + 100;
  const chunks: Chunk[] = [];
  for (let t = 0; t < 3100; t += 25) {
    chunks.push({ stream: 0, at: start + t, bytes: 16384 }, { stream: 1, at: start + t + 12.5, bytes: 16384 });
  }
  for (const t of [900, 1700, 2500, 3300]) {
    chunks.push({ stream: 2, at: start + t, bytes: 16384 });
  }
  chunks.sort((a, b) => a.at - b.at);
  const schedule: Schedule = { chunks, ends: [{ stream: 3, at: start + 50 }] };
  const { result, extender, worker } = await runLoad(
    { hello, uploaded: [{ bytes: 0, ms: 250, at: 0 }], loadResult: { uploadVerified: true, endReason: "complete" } },
    schedule,
  );
  const secondAt = worker.deliveredAt[worker.delivered.findIndex((c) => c.stream === 2 && c.at === start + 1700)];
  assert.ok(extender.received[0].when >= secondAt, `interval 0 went out ${secondAt - extender.received[0].when} ms before stream 2's second record came`);
  // the series is the rules': stream 2's first record over [100, 900]
  const expected = edgeRules({ chunks: worker.delivered, ends: schedule.ends }, hello.downloadMs, hello.intervalMs);
  result.download.intervalBytes.forEach((bytes, i) => {
    assert.ok(Math.abs(bytes - expected[i]) <= 1, `interval ${i}: ${bytes} bytes, the rules give ${expected[i]}`);
  });
  // Each stream's first record arrived before the window opened, at the
  // rate of its second, and counts only from the first byte: none of
  // stream 0's, which opened the window, and half of stream 1's (12.5 of its
  // 25 ms). So streams 0 and 1 each count their true rate from the first
  // interval on, 250 ms of a record per 25 ms, and stream 2 its first
  // record's share of each interval in [100, 900].
  const steady = (16384 * 250) / 25;
  assert.ok(Math.abs(expected[0] - (2 * steady + (16384 * 150) / 800)) < 1e-6, `interval 0: ${expected[0]}`);
  assert.ok(Math.abs(expected[1] - (2 * steady + (16384 * 250) / 800)) < 1e-6, `interval 1: ${expected[1]}`);
  assert.deepEqual(reportedIntervals(extender.received), result.download.intervalBytes);
});

test("the download's window is final once every open stream is done, 2 ms after its first group past the end, or 1 s after the end", { timeout: 30_000 }, async () => {
  // Stream k delivers a record every 200 ms from 25 + 50k ms, so each has
  // one record that straddles the 1 s window's end, at 1025 + 50k ms, and is
  // done 2 ms after it: the window is final at 1177 ms. Done only on a
  // record whose span starts after the end, each would wait for its next,
  // until 1375 ms. Then, the same but with stream 1 stalled from 800 to
  // 2600 ms: it has no record past the end that could be its last, so it
  // holds the window until 1 s past the end.
  const hello = { intervalMs: 250, downloadMs: 1000, uploadMs: 250, maxStreams: 4 };
  const run = async (stall: boolean) => {
    const start = performance.now() + 100;
    const chunks: Chunk[] = [];
    for (let stream = 0; stream < 4; stream += 1) {
      for (let t = 25 + 50 * stream; t < 3000; t += 200) {
        if (!(stall && stream === 1 && 800 < t && t < 2600)) {
          chunks.push({ stream, at: start + t, bytes: 16384 });
        }
      }
    }
    chunks.sort((a, b) => a.at - b.at);
    const schedule: Schedule = { chunks, ends: [] };
    const { result, extender, worker } = await runLoad(
      { hello, uploaded: [{ bytes: 0, ms: 250, at: 0 }], loadResult: { uploadVerified: true, endReason: "complete" } },
      schedule,
    );
    const expected = edgeRules({ chunks: worker.delivered, ends: [] }, hello.downloadMs, hello.intervalMs);
    result.download.intervalBytes.forEach((bytes, i) => {
      assert.ok(Math.abs(bytes - expected[i]) <= 1, `interval ${i}: ${bytes} bytes, the rules give ${expected[i]}`);
    });
    assert.deepEqual(reportedIntervals(extender.received), result.download.intervalBytes);
    return { result, closed: worker.closedAt! - start };
  };
  const steady = await run(false);
  // the close follows the last stream's done time, a Worker batch or so on
  assert.ok(1177 < steady.closed && steady.closed < 1360, `the download closed ${steady.closed} ms in`);
  // the records that straddle the end counted their parts before it, so the
  // last interval is whole: 250 ms at 4 x 16 KiB per 200 ms
  const full = (4 * 16384 * 250) / 200;
  assert.ok(Math.abs(steady.result.download.intervalBytes[3] - full) < 2, `the last interval holds ${steady.result.download.intervalBytes[3]} of ${full}`);
  const stalled = await run(true);
  assert.ok(1000 + SPREAD_MAX_MS <= stalled.closed && stalled.closed < 1000 + SPREAD_MAX_MS + 500, `the download closed ${stalled.closed} ms in`);
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

// The upload's window end on the page's clock as the extender's reports
// place it (lag.dev/PLAN.md M4, S5, amendment 15): the least, over the
// reports, of when one arrived less its ms, less half the session's lowest
// round trip, plus uploadMs. The scripted extender's reports reach the page
// as they are sent.
const estimatedEnd = (reports: Array<{ at: number; ms: number }>, rttMs: number, uploadMs: number) =>
  Math.min(...reports.map((report) => report.at - report.ms)) - rttMs / 2 + uploadMs;

test("the upload sends past its own uploadMs until the extender's report says the window is over, and closes on that report", { timeout: 30_000 }, async () => {
  // The extender's window opens 320 ms after the first message goes, as
  // far as its reports show, and is final 400 ms after its end: its
  // reports come 320 ms after their ms, and the one that says the window
  // is over 1720 ms after the first message. Each report counts every byte
  // sent, so flow control never holds the streams back for long.
  const { result, extender } = await runLoad(
    {
      hello: uploadHello,
      rtts: [40],
      countBytes: true,
      uploaded: [
        { ms: 250, at: 245, afterMs: 570 },
        { ms: 500, at: 495, afterMs: 820 },
        { ms: 750, at: 745, afterMs: 1070 },
        { ms: 1000, at: 1000, afterMs: 1720 },
      ],
      loadResult: { uploadVerified: true, endReason: "complete" },
    },
    briefDownload(),
  );
  const { times, uploadSends, reports } = extender;
  const first = times.uploadFirst!;
  const over = reports.at(-1)!.at;
  // the streams sent on past uploadMs by their own clock, up to the report
  const late = uploadSends.filter((send) => first + uploadHello.uploadMs + 50 < send.at).reduce((sum, send) => sum + send.bytes, 0);
  assert.ok(0 < late, "nothing was sent after uploadMs by the page's clock");
  // they closed on the report, and nothing went after
  assert.ok(times.uploadClosed !== undefined && over <= times.uploadClosed, `the streams closed ${over - times.uploadClosed!} ms before the report`);
  assert.ok(times.uploadClosed - over < 100, `the streams closed ${times.uploadClosed - over} ms after the report`);
  assert.ok(uploadSends.every((send) => send.at <= times.uploadClosed!));
  // done follows, and the load is whole
  assert.ok(times.done !== undefined && over <= times.done);
  assert.equal(result.endReason, "complete");
  assert.equal(result.upload.durationMs, 1000);
});

test("with no report that the window is over, the upload closes 2 s after its end as the reports place it: the earliest arrival less ms, less half the lowest round trip, plus uploadMs", { timeout: 30_000 }, async () => {
  // Reports at ms 250, 500 and 750, the first 600 ms after its time, the
  // others 1400 and 1200 ms after theirs, and the session's lowest round
  // trip 100 ms: the end is placed by the first, 600 - 50 + 1000 ms after
  // the first message, and the streams close 2 s after that. Placed by the
  // latest report, they would close 600 ms later; by the page's own start,
  // 550 ms sooner.
  const { result, extender } = await runLoad(
    {
      hello: uploadHello,
      rtts: [180, 100, 140],
      uploaded: [
        { bytes: 100_000, ms: 250, at: 250, afterMs: 850 },
        { bytes: 200_000, ms: 500, at: 500, afterMs: 1900 },
        { bytes: 300_000, ms: 750, at: 750, afterMs: 1950 },
      ],
      loadResult: { uploadVerified: true, endReason: "complete" },
    },
    briefDownload(),
  );
  const { times, reports, uploadSends } = extender;
  const close = estimatedEnd(reports, 100, 1000) + 2000;
  assert.ok(close - 2 <= times.uploadClosed!, `the streams closed ${close - times.uploadClosed!} ms before the end's estimate allowed`);
  assert.ok(times.uploadClosed! < close + 550, `the streams closed ${times.uploadClosed! - close} ms after the end's estimate allowed`);
  assert.ok(times.done !== undefined && close - 2 <= times.done);
  assert.ok(uploadSends.every((send) => send.at <= times.uploadClosed!));
  assert.equal(result.endReason, "complete");
});

test("with no report at all, the upload closes 2 s after its own start plus uploadMs, having sent what it may before the first", { timeout: 30_000 }, async () => {
  const { result, extender } = await runLoad(
    { hello: uploadHello, uploaded: [], loadResult: { uploadVerified: true, endReason: "complete" } },
    briefDownload(),
  );
  const { times, uploadSends } = extender;
  const first = times.uploadFirst!;
  // the page's start is a little before its first message went
  assert.ok(first + 1000 + 2000 - 100 <= times.uploadClosed!, `the streams closed ${times.uploadClosed! - first} ms after the first message`);
  assert.ok(times.uploadClosed! < first + 1000 + 2000 + 550, `the streams closed ${times.uploadClosed! - first} ms after the first message`);
  // before the first report, the start: a message a stream, since the
  // download ran at about 1 Mbps (S5, uploadStart.ts)
  assert.equal(uploadSends.reduce((sum, send) => sum + send.bytes, 0), 2 * 65536);
  assert.equal(result.endReason, "complete");
});

// How long the extender waits for the browser's own close after it closes
// an upload stream at its window's end, before it closes the connection
// (connect's lagApiCloseWait, RFC 6455 7.1.1)
const CLOSE_WAIT_MS = 2000;

/**
 * A link and an extender of amendment 15, for the upload (lag.dev/PLAN.md
 * M4, M6): the page's upload sockets queue what they are sent; a bottleneck
 * of `mbps` drains them in 16 KiB records, one from each stream with bytes
 * queued in turn; each record reaches the extender `oneWayMs` later, as its
 * messages reach the page. The extender counts each record by rule S on its
 * stream, one within 2 ms of its group's first sharing the group's span,
 * and past the window's end only the part before it. A stream's first
 * group waits for its second and counts at its rate, from the window's
 * first byte on (R2), so the bytes counted, and the reports' `bytes` and
 * `at`, leave out what arrived before the window opened: about 2.5 records
 * a load, which flow control sees as in flight for the rest of the window.
 * With `holdFirst` false, as the model was before the start was measured, a
 * stream's first record counts at once, spread back to the window's first
 * byte, and the first interval reads high on slow links. It reports every
 * interval of its window but the last half one, and says the window is over
 * once every stream is done, 2 ms after the first record of its first group
 * to begin at or after the end, or 1 s after the end. A socket's
 * bufferedAmount is what it has queued past a 128 KiB send buffer.
 *
 * What leaves the browser (`wire`) is what the bottleneck drains. A socket
 * the page closes still drains what it had queued, its close frame behind
 * it, and is then over; the extender gives up waiting for that close
 * CLOSE_WAIT_MS after its window's end and closes the connection, and once
 * that reaches the browser, a one-way delay later, nothing more of it leaves.
 * The extender's own close of each stream at its window's end isn't
 * modelled: the page sends on until the report that the window is over.
 *
 * The hello's downloadMs is `downloadMs`, 250 by default: the window of the
 * download the load runs first.
 */
const linkedExtender = ({ mbps, oneWayMs, uploadMs, downloadMs = 250, holdFirst = true }: { mbps: number; oneWayMs: number; uploadMs: number; downloadMs?: number; holdFirst?: boolean }) => {
  const intervalMs = 250;
  const rate = (mbps * 1e6) / 8 / 1000;
  const timers: Array<ReturnType<typeof setTimeout>> = [];
  const later = (ms: number, action: () => void) => {
    timers.push(setTimeout(action, Math.max(0, ms)));
  };
  let control: LinkedSocket | undefined;
  const uploads: LinkedSocket[] = [];
  const arrivals: Array<{ at: number; stream: number; bytes: number }> = [];
  let linkFree = 0;
  let turn = 0;
  const series: number[] = Array(Math.ceil(uploadMs / intervalMs)).fill(0);
  const streams = new Map<number, { last: number; groupFirst: number; groupFrom: number; doneAt: number; held: { bytes: number; last: number } | null }>();
  const stats = {
    sent: 0,
    counted: 0,
    // what left the browser: the bytes the bottleneck drained, and when the
    // last of them did (performance.now())
    wire: 0,
    lastDrain: undefined as number | undefined,
    series,
    first: undefined as number | undefined,
    windowOverAt: undefined as number | undefined,
    closedAt: undefined as number | undefined,
    doneAt: undefined as number | undefined,
    // the short messages, as they went (performance.now()), and any message
    // a stream sent after its short one
    shorts: [] as Array<{ at: number; stream: number; bytes: number }>,
    afterShort: 0,
    // the upload's messages as they went (performance.now()), and the
    // reports as they reached the page
    sends: [] as Array<{ at: number; bytes: number }>,
    reports: [] as Array<{ at: number; ms: number; bytes: number }>,
  };
  let lastCounted = 0;

  type Listener = (event: Record<string, unknown>) => void;
  class LinkedSocket {
    binaryType = "blob";
    readyState = 0;
    queued = 0;
    // once the page has closed it: what it still drains before its close
    // frame; and whether the connection is over, so nothing more leaves
    closing?: number;
    over = false;
    readonly url: string;
    readonly stream: number;
    private readonly listeners = new Map<string, Set<Listener>>();

    constructor(url: string) {
      this.url = url;
      const isControl = url.includes("/lag/v1/session?");
      this.stream = url.includes("/lag/v1/up?") ? Number(/stream=(\d+)/.exec(url)![1]) : -1;
      if (isControl) {
        control = this;
      } else if (0 <= this.stream) {
        uploads.push(this);
      }
      setTimeout(() => {
        if (this.readyState !== 0) {
          return;
        }
        this.readyState = 1;
        this.dispatch("open", {});
        if (isControl) {
          this.message({ type: "hello", session: "c2Vzc2lvbg", uploadKey: "AAAAAAAAAAAAAAAAAAAAAA", pingIntervalMs: 250, intervalMs, downloadMs, uploadMs, maxStreams: 4 });
          this.message({ type: "rtt", t: 0, ms: 2 * oneWayMs });
        }
      }, 1);
    }

    get bufferedAmount(): number {
      return Math.max(0, this.queued - 131_072);
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
        if (stats.shorts.some((short) => short.stream === this.stream)) {
          stats.afterShort += 1;
        }
        if (data.byteLength < 65536) {
          stats.shorts.push({ at: performance.now(), stream: this.stream, bytes: data.byteLength });
        }
        this.queued += data.byteLength;
        stats.sent += data.byteLength;
        stats.sends.push({ at: performance.now(), bytes: data.byteLength });
        return;
      }
      const message = JSON.parse(data) as Record<string, unknown>;
      if (message.type === "load" && message.phase === "done") {
        stats.doneAt ??= performance.now();
        const uploadBytes = series.map((bytes) => Math.round(bytes));
        later(oneWayMs, () => control?.message({ type: "loadResult", uploadVerified: true, endReason: "complete", uploadBytes }));
      }
    }

    close(): void {
      if (this.readyState === 3) {
        return;
      }
      if (0 <= this.stream) {
        stats.closedAt ??= performance.now();
        // what it has queued still goes, then its close frame
        this.closing = this.queued;
        this.over ||= this.queued === 0;
      }
      this.readyState = 3;
      setTimeout(() => this.dispatch("close", { code: 1000, reason: "" }), 1);
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

  const report = (ms: number) => {
    const message = { type: "uploaded", bytes: Math.floor(stats.counted), ms, at: Math.min(ms, lastCounted) };
    later(oneWayMs, () => {
      stats.reports.push({ at: performance.now(), ms, bytes: message.bytes });
      control?.message(message);
    });
  };
  const windowOver = () => {
    if (stats.windowOverAt === undefined) {
      // the window is final: a first group still held counts by rule S
      for (const stream of streams.values()) {
        if (stream.held !== null) {
          lastCounted = Math.max(lastCounted, Math.min(stream.held.last, uploadMs));
          placeHeld(stream);
        }
      }
      stats.windowOverAt = performance.now() + oneWayMs;
      report(uploadMs);
    }
  };
  const count = (from: number, to: number, bytes: number) => {
    if (to <= from) {
      if (0 <= to && to < uploadMs) {
        series[Math.floor(to / intervalMs)] += bytes;
        stats.counted += bytes;
      }
      return;
    }
    series.forEach((_, i) => {
      const overlap = Math.min(to, uploadMs, (i + 1) * intervalMs) - Math.max(from, i * intervalMs);
      if (0 < overlap) {
        series[i] += (bytes * overlap) / (to - from);
        stats.counted += (bytes * overlap) / (to - from);
      }
    });
  };
  // R2: a stream's first group counts once its second begins, at the rate
  // of that group's first record, and only from the window's first byte on;
  // without a second, by rule S
  const placeHeld = (stream: { held: { bytes: number; last: number } | null }, t2?: number, bytes?: number) => {
    const { bytes: held, last: t1 } = stream.held!;
    stream.held = null;
    if (t2 !== undefined && bytes !== undefined && t1 < t2) {
      count(Math.max(t1 - (held * (t2 - t1)) / bytes, t1 - SPREAD_MAX_MS), t1, held);
    } else {
      count(Math.max(0, t1 - SPREAD_MAX_MS), t1, held);
    }
  };
  const receive = (arrival: number, k: number, bytes: number) => {
    if (stats.first === undefined) {
      stats.first = arrival;
      for (let ms = intervalMs; ms < uploadMs - intervalMs / 2; ms += intervalMs) {
        later(arrival + ms - performance.now(), () => report(ms));
      }
      later(arrival + uploadMs + SPREAD_MAX_MS - performance.now(), windowOver);
      // the extender gives up on the browser's close and closes the
      // connections; once that reaches the browser, nothing more leaves
      later(arrival + uploadMs + CLOSE_WAIT_MS + oneWayMs - performance.now(), () => {
        for (const upload of uploads) {
          upload.over = true;
        }
      });
    }
    const t = arrival - stats.first;
    const stream = streams.get(k);
    if (stats.windowOverAt !== undefined || (stream !== undefined && stream.doneAt < t)) {
      return;
    }
    if (stream === undefined && holdFirst) {
      // its first group, held (R2)
      streams.set(k, { last: t, groupFirst: t, groupFrom: t, doneAt: uploadMs <= t ? t + SPREAD_JOIN_MS : Infinity, held: { bytes, last: t } });
      return;
    }
    if (stream === undefined) {
      const from = Math.max(0, t - SPREAD_MAX_MS);
      streams.set(k, { last: t, groupFirst: t, groupFrom: from, doneAt: uploadMs <= t ? t + SPREAD_JOIN_MS : Infinity, held: null });
      count(from, t, bytes);
      lastCounted = Math.min(t, uploadMs);
      return;
    }
    let from: number;
    if (t - stream.groupFirst <= SPREAD_JOIN_MS) {
      if (stream.held !== null) {
        stream.held.bytes += bytes;
        stream.held.last = t;
        stream.last = t;
        return;
      }
      from = stream.groupFrom;
    } else {
      if (stream.held !== null) {
        placeHeld(stream, t, bytes);
      }
      from = Math.max(stream.last, t - SPREAD_MAX_MS, 0);
      stream.groupFirst = t;
      stream.groupFrom = from;
      if (uploadMs <= t && stream.doneAt === Infinity) {
        stream.doneAt = t + SPREAD_JOIN_MS;
      }
    }
    stream.last = t;
    count(from, t, bytes);
    lastCounted = Math.min(t, uploadMs);
  };
  // every 2 ms the link drains what it could have since the last time, and
  // the records due by now arrive; while it has nothing queued it idles
  const ticker = setInterval(() => {
    const now = performance.now();
    while (linkFree <= now) {
      const queued = uploads.filter((upload) => 0 < upload.queued && !upload.over);
      if (queued.length === 0) {
        linkFree = now;
        break;
      }
      const upload = queued[turn % queued.length];
      turn += 1;
      const bytes = Math.min(16384, upload.queued);
      upload.queued -= bytes;
      linkFree += bytes / rate;
      stats.wire += bytes;
      stats.lastDrain = linkFree;
      arrivals.push({ at: linkFree + oneWayMs, stream: upload.stream, bytes });
      if (upload.closing !== undefined) {
        upload.closing -= bytes;
        upload.over ||= upload.closing <= 0;
      }
    }
    arrivals.sort((a, b) => a.at - b.at);
    while (0 < arrivals.length && arrivals[0].at <= now) {
      const arrival = arrivals.shift()!;
      receive(arrival.at, arrival.stream, arrival.bytes);
    }
    // the window is final once every stream is done
    const elapsed = stats.first === undefined ? 0 : now - stats.first;
    if (uploadMs <= elapsed && uploads.every((upload) => (streams.get(upload.stream)?.doneAt ?? Infinity) < elapsed)) {
      windowOver();
    }
  }, 2);
  const stop = () => {
    clearInterval(ticker);
    for (const timer of timers) {
      clearTimeout(timer);
    }
  };
  // resolves once nothing more can leave the browser: every upload socket's
  // connection is over
  const settled = async () => {
    while (!(0 < uploads.length && uploads.every((upload) => upload.over))) {
      await sleep(10);
    }
  };
  return { WebSocket: LinkedSocket as unknown as typeof WebSocket, stats, stop, settled };
};

/**
 * One load over a linked extender (above): a download first, then the
 * upload over `mbps` with a round trip of 2 x `oneWayMs`. The download is
 * briefDownload's unless `downloadMbps` is given: then a 5 s download at
 * that rate, set in the past so that it is over at once, as a link whose
 * download runs at that rate would leave the session. What the run
 * measured, as shares of the link: the bytes counted, the p50, the first,
 * second and last intervals, and the overshoot, as bytes handed to the
 * sockets (`sent`) and leaving the browser (`wire`) over those counted; and
 * when the link was full (`fullMs`): the start of the first interval from
 * which every one but the window's last holds at least 90% of the link.
 * Also the bytes handed to the sockets before the first report reached the
 * page (`start`), those sent and not counted (`wasted`), and how long the
 * link went on carrying the upload after the page's own window, uploadMs
 * from its first message (`tailMs`): what the site's idle span after an
 * upload has to cover (L4).
 */
const linkedLoad = async ({
  mbps,
  oneWayMs,
  uploadMs = 5000,
  downloadMbps,
  holdFirst = true,
}: {
  mbps: number;
  oneWayMs: number;
  uploadMs?: number;
  downloadMbps?: number;
  holdFirst?: boolean;
}) => {
  const downloadMs = downloadMbps === undefined ? 250 : 5000;
  const link = linkedExtender({ mbps, oneWayMs, uploadMs, downloadMs, holdFirst });
  const download =
    downloadMbps === undefined
      ? briefDownload()
      : streamSchedule(downloadMbps, Math.round(downloadMbps * 1000) + oneWayMs, { start: performance.now() - 20_000, untilMs: downloadMs + 1500 });
  const worker = new ScheduleWorker(download);
  const session = openLagSession({ host: "extender.test", ticket: "ticket", WebSocket: link.WebSocket, downloadWorker: () => worker as unknown as Worker });
  let result: LagLoadResult;
  try {
    await session.ready;
    result = await session.startLoad();
    await link.settled();
  } finally {
    session.close();
    worker.terminate();
    link.stop();
  }
  const { stats } = link;
  const capacity = (mbps * 1e6 * uploadMs) / 8 / 1000;
  const perInterval = capacity / (uploadMs / 250);
  const share = (bytes: number) => bytes / perInterval;
  const { series } = stats;
  let full = series.length - 1;
  while (0 < full && 0.9 <= share(series[full - 1])) {
    full -= 1;
  }
  const firstReport = stats.reports[0]?.at ?? Infinity;
  return {
    result,
    stats,
    end: stats.first! + uploadMs,
    start: stats.sends.filter((send) => send.at < firstReport).reduce((sum, send) => sum + send.bytes, 0),
    wasted: stats.sent - stats.counted,
    tailMs: stats.lastDrain! - (stats.sends[0].at + uploadMs),
    counted: stats.counted / capacity,
    p50: result.upload.percentiles!.p50 / mbps,
    p95: result.upload.percentiles!.p95 / mbps,
    first: share(series[0]),
    second: share(series[1]),
    last: share(series.at(-1)!),
    fullMs: full * 250,
    sent: stats.sent / stats.counted - 1,
    wire: stats.wire / stats.counted - 1,
    download: result.download.percentiles?.p50,
  };
};

for (const mbps of [20, 4, 2]) {
  test(`over a ${mbps} Mbps link, the upload fills the extender's window to its end, ends its streams on short messages, and sends past it only until the window is final`, { timeout: 60_000 }, async (t) => {
    // A 5 s window over a path of 50 ms, the download at the link's rate.
    // The streams send on past the window's end until the extender's report
    // says it is over, so that each stream has a record that straddles the
    // end and fills it. Near the end the room for a whole message runs out,
    // and what is left goes as a stream's short, last message (M6, S5). By
    // this model, amendment 14's client, which stopped at its own uploadMs,
    // left the last interval at 86%, 65-73% and 41% of the link at 20, 4 and
    // 2 Mbps. Sending whole messages until the window was over, without
    // short ones, filled it to 98%, 79-87% and 58%, and sent 2-4%, 9-12% and
    // 19% more than was counted; with them, 4 and 2 Mbps fill it to about
    // 95-110% and 85-100%. Each stream's first record counts at once here
    // (holdFirst false): held as R2 holds it, the 2.5 records or so that R2
    // never counts stay in flight for flow control to the end, and the last
    // interval on these slow paths ends short of room, as on the stack (Q8),
    // which is not what this tests.
    const oneWayMs = 25;
    const run = await linkedLoad({ mbps, oneWayMs, downloadMbps: mbps, holdFirst: false });
    const { result, stats, end, last } = run;
    t.diagnostic(
      `counted ${(run.counted * 100).toFixed(1)}% of the link; p50 ${result.upload.percentiles!.p50.toFixed(3)} Mbps; last interval ${(last * 100).toFixed(0)}%; ` +
        `sent ${(run.sent * 100).toFixed(1)}% more than counted; window over ${Math.round(stats.windowOverAt! - end)} ms after its end; ` +
        `short messages ${JSON.stringify(stats.shorts.map((short) => [Math.round(short.at - end), short.stream, short.bytes]))}`,
    );
    // the link was full through the window, and its last interval too: the
    // p50 says so however busy the machine is, the bytes counted unless the
    // machine starved the streams for a while
    assert.ok(Math.abs(result.upload.percentiles!.p50 - mbps) <= mbps * 0.03, `p50 ${result.upload.percentiles!.p50}`);
    assert.ok(0.9 <= run.counted, `counted ${(run.counted * 100).toFixed(1)}% of the link`);
    assert.ok(0.8 <= last, `the last interval holds ${(last * 100).toFixed(0)}% of the link`);
    // each short message was a stream's last, of one record to just under a
    // whole message, and none went before the end phase: before the window's
    // estimated end less a whole message's time and the round trip
    assert.equal(stats.afterShort, 0, "a stream sent a message after its short one");
    const endPhaseMs = (65536 * 1000) / ((mbps * 1e6) / 8) + 2 * oneWayMs;
    stats.shorts.forEach((short) => {
      assert.ok(16 * 1024 <= short.bytes && short.bytes < 65536, `a short message of ${short.bytes} bytes`);
      assert.ok(end - short.at < endPhaseMs + 100, `a short message went ${end - short.at} ms before the end`);
    });
    if (mbps < 10) {
      assert.ok(0 < stats.shorts.length, "no stream ended on a short message");
    }
    // the streams closed on the report that the window was over, within 1 s
    // of its end (a stream whose short message all came before the end has
    // no record past it, so it holds the window that long), and done
    // followed
    assert.ok(stats.windowOverAt! - end <= SPREAD_MAX_MS + 300, `the window was over ${stats.windowOverAt! - end} ms after its end`);
    assert.ok(stats.windowOverAt! - 5 <= stats.closedAt! && stats.closedAt! - stats.windowOverAt! < 100, `the streams closed ${stats.closedAt! - stats.windowOverAt!} ms after the report`);
    assert.ok(stats.closedAt! <= stats.doneAt!);
    assert.ok(run.sent <= 0.08, `sent ${(run.sent * 100).toFixed(1)}% more than was counted`);
  });
}

// The link model's table (lag.dev/PLAN.md S5): over each link, by rate and
// round trip, one load, the download at the upload's rate; and links whose
// upload is far slower or far faster than their download, at 50 and 250 ms.
const LINK_RATES = [0.5, 1, 1.5, 2, 4, 20, 50];
const LINK_RTTS = [20, 50, 100, 250];
const LINK_ASYMMETRIC: Array<{ mbps: number; downloadMbps: number }> = [
  { mbps: 0.5, downloadMbps: 20 },
  { mbps: 1, downloadMbps: 20 },
  { mbps: 1.5, downloadMbps: 20 },
  { mbps: 20, downloadMbps: 2 },
  { mbps: 50, downloadMbps: 2 },
];

// runs `jobs` with at most `width` at once, in order
const pooled = async <T,>(jobs: Array<() => Promise<T>>, width: number): Promise<T[]> => {
  const out: T[] = [];
  let next = 0;
  await Promise.all(
    Array.from({ length: Math.min(width, jobs.length) }, async () => {
      while (next < jobs.length) {
        const i = next;
        next += 1;
        out[i] = await jobs[i]();
      }
    }),
  );
  return out;
};

test("the link model's table: from 0.5 to 50 Mbps and 20 to 250 ms, the upload's start fits the window, it sends little past what is counted, and it fills the link as before", { timeout: 900_000 }, async (t) => {
  // The start is what the streams may send before the extender's first
  // report: the download's rate for the lowest round trip and two
  // intervals, a message a stream at least and 1 MiB at most (S5,
  // uploadStart.ts). With the 1 MiB start of before, a 0.5 Mbps link handed
  // its sockets 3.4 times what its window counted, and 1.5 times at 1 Mbps,
  // and what its window could not count drained after it, until the
  // extender closed the connections 2 s after its end. Four loads at a time
  // (LAG_LINK_WIDTH).
  const { lagUploadStartBytes, LAG_UPLOAD_START_MAX_BYTES } = await import("../../src/lag/uploadStart.ts");
  const width = Number(process.env.LAG_LINK_WIDTH ?? 4);
  const cases = [
    ...LINK_RATES.flatMap((mbps) => LINK_RTTS.map((rttMs) => ({ mbps, downloadMbps: mbps, rttMs }))),
    ...LINK_ASYMMETRIC.flatMap((link) => [50, 250].map((rttMs) => ({ ...link, rttMs }))),
  ];
  const rows = await pooled(
    cases.map((link) => async () => ({ ...link, ...(await linkedLoad({ mbps: link.mbps, oneWayMs: link.rttMs / 2, downloadMbps: link.downloadMbps })) })),
    width,
  );
  const percent = (value: number) => `${(100 * value).toFixed(0)}%`;
  t.diagnostic(
    "up/down Mbps, rtt ms: start KiB | sent% wire% over counted, KB not counted | p50 p95 | counted | first second last interval | full from ms | tail ms\n" +
      rows
        .map(
          (row) =>
            `${row.mbps}/${row.downloadMbps}, ${row.rttMs}: ${Math.round(row.start / 1024)} | ${(100 * row.sent).toFixed(1)} ${(100 * row.wire).toFixed(1)}, ${Math.round(row.wasted / 1000)} | ` +
            `${percent(row.p50)} ${percent(row.p95)} | ${percent(row.counted)} | ${percent(row.first)} ${percent(row.second)} ${percent(row.last)} | ${row.fullMs} | ${Math.round(row.tailMs)}`,
        )
        .join("\n"),
  );
  for (const row of rows) {
    const name = `${row.mbps} Mbps up, ${row.downloadMbps} down, ${row.rttMs} ms`;
    // The p50 is the link's, and the link was full through the window but
    // for its ramp, however fast the upload is next to the download. At
    // 2 Mbps and below a start that fits the window leaves the four streams
    // about a message each in flight after it (the backlog's 256 KiB), so a
    // stream can run dry between messages and the extender's count of it,
    // stream by stream (rule S), reads a few percent off: 96-100% at 1 to
    // 2 Mbps, where the 1 MiB start kept every stream busy to the end and
    // read 100%.
    const p50Mark = row.mbps <= 2 ? 0.05 : 0.03;
    assert.ok(Math.abs(row.p50 - 1) <= p50Mark, `${name}: p50 ${percent(row.p50)} of the link`);
    assert.ok(0.85 <= row.counted, `${name}: counted ${percent(row.counted)} of the link`);
    // before the first report the streams sent no more than the start
    const start = lagUploadStartBytes({
      downloadBytesPerSecond: ((row.download ?? 0) * 1e6) / 8,
      baseRttMs: row.rttMs,
      intervalMs: 250,
      streams: 4,
    });
    assert.ok(row.start <= start, `${name}: ${row.start} bytes before the first report, the start is ${start}`);
    assert.ok(row.wire <= row.sent + 1e-9, `${name}: more left the browser than it sent`);
    if (row.mbps === row.downloadMbps && 20 <= row.mbps) {
      // a fast link's start is the 1 MiB it was, so it fills as quickly
      assert.equal(start, LAG_UPLOAD_START_MAX_BYTES, `${name}: the start`);
    }
    if (row.mbps === row.downloadMbps && row.mbps < 2) {
      // a slow link leaves uncounted about what a faster one does, 60 to
      // 90 KB: the records R2 never counts, and the end's (M6). The 1 MiB
      // start left 736 KB at 0.5 Mbps, 424 KB at 1 and 111 KB at 1.5.
      assert.ok(row.wasted <= 160_000, `${name}: ${Math.round(row.wasted)} bytes sent and not counted`);
    }
  }
});
