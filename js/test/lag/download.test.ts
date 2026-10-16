import { test } from "node:test";
import assert from "node:assert/strict";
import { registerTsResolve } from "../ts-resolve.ts";
import { startMockExtender, type MockExtender } from "./client_mock_extender.ts";
import type { LagSession, LagSessionEvent } from "../../src/lag/client.ts";
import type { LagDownloadToPage, LagDownloadToWorker } from "../../src/lag/download.ts";

registerTsResolve();
const { openLagSession } = await import("../../src/lag/client.ts");
const { LagDownloadWorkerHost, LAG_DOWNLOAD_BATCH_MS } = await import("../../src/lag/download.ts");
const { LAG_SPREAD_MAX_MS: SPREAD_MAX_MS } = await import("../../src/lag/stats.ts");

// The download read in a Worker (lag.dev/PLAN.md S5): a load whose streams
// are read in the download Worker reports exactly what one read on the page
// does, chunks still on their way from the Worker land in the intervals they
// arrived in, and a Worker that cannot start or fails hands the download
// back to the page. Node has no Worker, so a FakeWorker runs the Worker's own
// host (download.ts) in this process, handing messages across with a delay
// as a busy page would see them.

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

const withMock = async (
  options: Parameters<typeof startMockExtender>[0],
  body: (mock: MockExtender) => Promise<void>,
) => {
  const mock = await startMockExtender(options);
  try {
    await body(mock);
  } finally {
    await mock.close();
  }
};

const expectedReceived = (intervalBytes: number[], intervalMs: number) => {
  let total = 0;
  return intervalBytes.map((bytes, i) => {
    total += bytes;
    return { bytes: total, ms: (i + 1) * intervalMs };
  });
};

const ofType = <T extends LagSessionEvent["type"]>(events: LagSessionEvent[], type: T) =>
  events.filter((event): event is Extract<LagSessionEvent, { type: T }> => event.type === type);

interface FakeWorkerOptions {
  /** how long each message to the page takes to arrive */
  deliveryDelayMs?: number;
  /** never say ready */
  silent?: boolean;
  /**
   * fail, in place of the next message, once this many messages carrying
   * chunks have reached the page: part way through a download, however many
   * batches went before its first chunk came
   */
  failAfterChunkMessages?: number;
}

type Listener = (event: { data: LagDownloadToPage }) => void;

// A dedicated Worker in this process: the download Worker's host behind the
// same messages and events a real one has.
class FakeWorker {
  readonly sent: LagDownloadToWorker[] = [];
  /** each flush the page asked for: when, and when its "flushed" reached the page (performance.now()) */
  readonly flushes: Array<{ askedAt: number; answeredAt?: number }> = [];
  readonly options: FakeWorkerOptions;
  private readonly messageListeners: Listener[] = [];
  private readonly errorListeners: Array<(event: unknown) => void> = [];
  private readonly host: InstanceType<typeof LagDownloadWorkerHost>;
  private readonly ids = new Set<number>();
  // messages carrying chunks that have reached the page
  private delivered = 0;
  terminated = false;

  constructor(options: FakeWorkerOptions = {}) {
    this.options = options;
    this.host = new LagDownloadWorkerHost((message) => this.toPage(message));
    if (!options.silent) {
      setTimeout(() => this.toPage({ type: "ready" }), 5);
    }
  }

  addEventListener(type: string, listener: (event: never) => void): void {
    if (type === "message") {
      this.messageListeners.push(listener as Listener);
    } else if (type === "error" || type === "messageerror") {
      this.errorListeners.push(listener as (event: unknown) => void);
    }
  }

  postMessage(message: LagDownloadToWorker): void {
    this.sent.push(message);
    if (message.type === "start") {
      this.ids.add(message.id);
    }
    if (message.type === "flush") {
      this.flushes.push({ askedAt: performance.now() });
    }
    setTimeout(() => {
      if (!this.terminated) {
        this.host.receive(message);
      }
    }, 0);
  }

  terminate(): void {
    if (this.terminated) {
      return;
    }
    this.terminated = true;
    // a real Worker's streams die with it
    for (const id of this.ids) {
      this.host.receive({ type: "close", id });
    }
  }

  private toPage(message: LagDownloadToPage): void {
    if (this.terminated) {
      return;
    }
    setTimeout(() => {
      if (this.terminated) {
        return;
      }
      const failAfter = this.options.failAfterChunkMessages;
      if (failAfter !== undefined && message.type !== "ready" && failAfter <= this.delivered) {
        for (const listener of this.errorListeners) {
          listener(new Error("the download worker failed"));
        }
        return;
      }
      if (message.type !== "ready" && message.type !== "ended" && 0 < message.items.length) {
        this.delivered += 1;
      }
      if (message.type === "flushed") {
        const flush = this.flushes.find((asked) => asked.answeredAt === undefined);
        if (flush !== undefined) {
          flush.answeredAt = performance.now();
        }
      }
      for (const listener of this.messageListeners) {
        listener({ data: message });
      }
    }, this.options.deliveryDelayMs ?? 0);
  }
}

const openWithWorker = (mock: MockExtender, ticket: string, options: FakeWorkerOptions = {}) => {
  const workers: FakeWorker[] = [];
  const factory = () => {
    const worker = new FakeWorker(options);
    workers.push(worker);
    return worker as unknown as Worker;
  };
  const events: LagSessionEvent[] = [];
  const session = openLagSession({ host: mock.origin, ticket, onEvent: (event) => events.push(event), downloadWorker: factory });
  return { session, events, workers, factory };
};

test("a load's download is read in the Worker, and reports what one read on the page would", { timeout: 30_000 }, async () => {
  await withMock({ uploadBytesPerSecond: 16 << 20 }, async (mock) => {
    const { session, events, workers } = openWithWorker(mock, "ticket-worker");
    await session.ready;
    const result = await session.startLoad();
    const record = mock.sessions[0];

    // the Worker read the four streams
    assert.equal(workers.length, 1);
    const start = workers[0].sent.find((message) => message.type === "start");
    assert.ok(start && start.type === "start");
    assert.equal(start.start.streams, 4);
    assert.match(start.start.url, /\/lag\/v1\/down\?session=/);
    assert.equal(record.maxConcurrentDownloads, 4);

    assert.deepEqual(ofType(events, "load").map((event) => event.phase), ["download", "upload", "done"]);
    assert.equal(result.endReason, "complete");
    assert.equal(result.download.streams, 4);
    assert.ok(0 < result.download.bytes && result.download.bytes <= record.downloadBytes);
    assert.equal(result.download.intervalBytes.length, 4);
    assert.deepEqual(record.received, expectedReceived(result.download.intervalBytes, 250));
    const progress = ofType(events, "progress").filter((event) => event.direction === "download");
    assert.deepEqual(progress.map((event) => event.interval), [0, 1, 2, 3]);
    assert.deepEqual(
      progress.map((event) => event.bytes),
      record.received.map((report) => report.bytes),
    );
    // at the page's time since the first byte, which never goes back
    progress.forEach((event, i) => {
      assert.ok(0 <= event.ms && event.ms <= 1000 && (i === 0 || progress[i - 1].ms <= event.ms), `progress at ${event.ms} ms`);
    });
    session.close();
  });
});

test("through the Worker, each `received` report carries its interval's final value: with a slow stream, a stream that never opens, and past the window", { timeout: 30_000 }, async () => {
  const options = {
    hello: { downloadMs: 2000 },
    downloadPauseMs: 20,
    slowDownloadStream: { stream: 2, pauseMs: 400 },
    refuseDownloadStream: 3,
    downloadOverrunMs: 1000,
    uploadBytesPerSecond: 4 << 20,
  };
  await withMock(options, async (mock) => {
    const { session, events } = openWithWorker(mock, "ticket-worker-settled", { deliveryDelayMs: 30 });
    const hello = await session.ready;
    const result = await session.startLoad();
    const record = mock.sessions[0];
    assert.equal(result.download.streams, 3);
    assert.equal(record.downloadRefused, 4);
    assert.equal(result.download.intervalBytes.length, hello.downloadMs / hello.intervalMs);
    assert.deepEqual(record.received, expectedReceived(result.download.intervalBytes, hello.intervalMs));
    const progress = ofType(events, "progress").filter((event) => event.direction === "download");
    assert.deepEqual(
      progress.map((event) => event.bytes),
      record.received.map((report) => report.bytes),
    );
    session.close();
  });
});

for (const deliveryDelayMs of [120, 400]) {
  test(`chunks the Worker hands over ${deliveryDelayMs} ms late land in their own intervals, and no interval is reported before its chunks`, { timeout: 30_000 }, async () => {
    await withMock({ uploadBytesPerSecond: 16 << 20 }, async (mock) => {
      const { session, events } = openWithWorker(mock, `ticket-late-${deliveryDelayMs}`, { deliveryDelayMs });
      const hello = await session.ready;
      const result = await session.startLoad();
      const record = mock.sessions[0];
      // every report holds the bytes through its interval as the series
      // finally has them: none went out before its last chunk came in
      assert.deepEqual(record.received, expectedReceived(result.download.intervalBytes, hello.intervalMs));
      assert.equal(record.received.length, hello.downloadMs / hello.intervalMs);
      assert.equal(record.received.at(-1)!.ms, hello.downloadMs);
      const progress = ofType(events, "progress").filter((event) => event.direction === "download");
      assert.deepEqual(progress.map((event) => event.interval), [0, 1, 2, 3]);
      assert.ok(result.download.intervalBytes.every((bytes) => 0 < bytes), "an interval of the window got no bytes");
      session.close();
    });
  });
}

test("chunks the Worker times past the window count only what they carried before its end, and the streams are cut once it is final", { timeout: 30_000 }, async () => {
  const options = {
    hello: { intervalMs: 500, downloadMs: 2000 },
    downloadOverrunMs: 3000,
    downloadPauseMs: 20,
    uploadBytesPerSecond: 4 << 20,
  };
  await withMock(options, async (mock) => {
    const { session, events } = openWithWorker(mock, "ticket-worker-overrun", { deliveryDelayMs: 60 });
    const hello = await session.ready;
    const result = await session.startLoad();
    const record = mock.sessions[0];
    assert.deepEqual(ofType(events, "load").map((event) => event.phase), ["download", "upload", "done"]);
    assert.equal(record.downloadCutByBrowser, 4);
    assert.ok(result.download.bytes < record.downloadBytes);
    assert.equal(result.download.mbps.length, hello.downloadMs / hello.intervalMs);
    // nothing past the window: every byte counted is in the window's
    // intervals, and the bytes read include what fell outside it, before
    // its first byte (M6, amendment 15) or after its end
    const inIntervals = result.download.intervalBytes.reduce((a, b) => a + b, 0);
    assert.ok(0 < inIntervals && inIntervals < result.download.bytes);
    assert.equal(result.download.durationMs, hello.downloadMs);
    assert.deepEqual(record.received, expectedReceived(result.download.intervalBytes, hello.intervalMs));
    assert.deepEqual(record.received.at(-1), { bytes: inIntervals, ms: hello.downloadMs });
    session.close();
  });
});

test("a download that takes its whole extra second to be final still has its upload's sockets accepted, none held into a refusal", { timeout: 30_000 }, async () => {
  // Stream 2 writes 64 KiB and then nothing for 5 s, so it is never done:
  // the window is final 1 s after its 2 s end, and the upload's phase opens
  // only then (M6, amendment 15). The extender holds an upload
  // socket that comes before its phase for 2 s and then refuses it (M4),
  // and the Worker hands chunks over 100 ms late, as to a busy page. Opened
  // 1 s before the window's end, the first socket would wait over 2 s and be
  // refused; it reaches the extender about 250 ms before the end instead.
  //
  // A socket's wait also holds what this machine adds when it is busy: the
  // timer that finds the window final firing late, the Worker's hand-overs
  // coming late, and the extender getting to the upload message late. So
  // the mock holds the sockets for as long as the page takes, and each
  // wait, less those delays as measured, must be under the 2 s.
  const deliveryDelayMs = 100;
  const options = {
    hello: { downloadMs: 2000 },
    downloadPauseMs: 20,
    slowDownloadStream: { stream: 2, pauseMs: 5000 },
    downloadOverrunMs: 2000,
    uploadBytesPerSecond: 4 << 20,
    streamWaitMs: 30_000,
  };
  await withMock(options, async (mock) => {
    // when the page made its first upload socket, and sent the upload message
    let firstUploadSocketAt: number | undefined;
    let uploadSentAt: number | undefined;
    class TimedWebSocket extends WebSocket {
      constructor(url: string | URL, protocols?: string | string[]) {
        super(url, protocols);
        if (String(url).includes("/lag/v1/up")) {
          firstUploadSocketAt ??= performance.now();
        }
      }
      send(data: string | ArrayBufferLike | Blob | ArrayBufferView) {
        if (typeof data === "string" && data.includes('"phase":"upload"')) {
          uploadSentAt ??= performance.now();
        }
        super.send(data);
      }
    }
    // the window's end on this clock, and the session's lowest round trip
    // as the page timed its sockets by it, both as of the first byte
    let windowEnd: number | undefined;
    let rttMs: number | undefined;
    const workers: FakeWorker[] = [];
    const events: LagSessionEvent[] = [];
    let session!: LagSession;
    session = openLagSession({
      host: mock.origin,
      ticket: "ticket-worker-slow-final",
      WebSocket: TimedWebSocket,
      downloadWorker: () => {
        const worker = new FakeWorker({ deliveryDelayMs });
        workers.push(worker);
        return worker as unknown as Worker;
      },
      onEvent: (event) => {
        events.push(event);
        if (event.type === "load" && event.phase === "download") {
          windowEnd = performance.now() - (Date.now() - event.at) + session.hello!.downloadMs;
          rttMs = Math.min(...session.pings.roundTrips());
        }
      },
    });
    const hello = await session.ready;
    // round trips to time the sockets by
    while (session.pings.roundTrips().length < 2) {
      await sleep(10);
    }
    const result = await session.startLoad();
    const record = mock.sessions[0];
    assert.equal(result.endReason, "complete");
    assert.equal(record.uploadRefused, 0, "an upload socket was refused");
    assert.equal(record.uploadStreams, 4);
    assert.equal(result.upload.streams, 4);
    // the window was final 1 s after its end, when the upload began
    const upload = ofType(events, "load").find((event) => event.phase === "upload")!;
    const end = result.download.startedAt! + hello.downloadMs;
    assert.ok(SPREAD_MAX_MS - 50 <= upload.at - end, `the upload began ${upload.at - end} ms after the download's window ended`);
    // the page made its first upload socket 250 ms and two round trips
    // before the window's end, not 1 s before it (a busy machine only makes
    // it later)
    assert.ok(firstUploadSocketAt !== undefined && windowEnd !== undefined && rttMs !== undefined);
    const lead = windowEnd - firstUploadSocketAt;
    assert.ok(lead <= 250 + 2 * rttMs + 5, `the first upload socket was made ${lead} ms before the window's end, the round trip ${rttMs} ms`);
    // and every socket that came before its phase waited, less this
    // machine's delays, under the 2 s the extender allows
    const flush = workers[0].flushes[0];
    assert.ok(flush?.answeredAt !== undefined && uploadSentAt !== undefined && record.uploadPhaseAt !== undefined);
    const delays = {
      final: Math.max(0, flush.askedAt - (windowEnd + SPREAD_MAX_MS)),
      handOver: Math.max(0, Math.min(flush.answeredAt, uploadSentAt) - flush.askedAt - deliveryDelayMs),
      extender: Math.max(0, record.uploadPhaseAt - uploadSentAt),
    };
    const late = delays.final + delays.handOver + delays.extender;
    assert.equal(record.uploadRequestedAt.filter((at) => at !== undefined).length, 4);
    record.uploadOpenedAt.forEach((openedAt, k) => {
      const waited = openedAt - record.uploadRequestedAt[k];
      assert.ok(waited - late < 2000, `upload stream ${k} waited ${waited} ms, of it this machine's delays ${JSON.stringify(delays)}`);
    });
    session.close();
  });
});

test("a page that stalls while the Worker reads still has every chunk in the interval it arrived in", { timeout: 30_000 }, async () => {
  await withMock({ uploadBytesPerSecond: 16 << 20 }, async (mock) => {
    let stalled = false;
    const workers: FakeWorker[] = [];
    const events: LagSessionEvent[] = [];
    const session = openLagSession({
      host: mock.origin,
      ticket: "ticket-worker-stall",
      downloadWorker: () => {
        const worker = new FakeWorker();
        workers.push(worker);
        return worker as unknown as Worker;
      },
      onEvent: (event) => {
        events.push(event);
        if (!stalled && event.type === "progress" && event.direction === "download") {
          stalled = true;
          const until = performance.now() + 450;
          while (performance.now() < until) {
            // busy
          }
        }
      },
    });
    const hello = await session.ready;
    const result = await session.startLoad();
    assert.ok(stalled);
    const record = mock.sessions[0];
    assert.deepEqual(record.received, expectedReceived(result.download.intervalBytes, hello.intervalMs));
    assert.equal(record.received.length, hello.downloadMs / hello.intervalMs);
    session.close();
  });
});

test("a Worker that never says it is ready is given up, and the page reads the download itself", { timeout: 30_000 }, async () => {
  await withMock({ uploadBytesPerSecond: 16 << 20 }, async (mock) => {
    const { session, workers } = openWithWorker(mock, "ticket-worker-silent", { silent: true });
    const hello = await session.ready;
    const result = await session.startLoad();
    const record = mock.sessions[0];
    assert.equal(workers.length, 1);
    assert.ok(workers[0].terminated, "the silent Worker was kept");
    assert.equal(workers[0].sent.length, 0);
    assert.equal(result.endReason, "complete");
    assert.ok(0 < result.download.bytes);
    assert.deepEqual(record.received, expectedReceived(result.download.intervalBytes, hello.intervalMs));
    session.close();
  });
});

test("a Worker that fails during the download ends it with what it had, and later downloads are read on the page", { timeout: 40_000 }, async () => {
  await withMock({ uploadBytesPerSecond: 16 << 20 }, async (mock) => {
    const workers: FakeWorker[] = [];
    const factory = () => {
      const worker = new FakeWorker({ failAfterChunkMessages: 3 });
      workers.push(worker);
      return worker as unknown as Worker;
    };
    const first = openLagSession({ host: mock.origin, ticket: "ticket-worker-fail-1", downloadWorker: factory });
    await first.ready;
    const firstResult = await first.startLoad();
    assert.ok(workers[0].terminated, "the failed Worker was kept");
    assert.ok(0 < firstResult.download.bytes, "the download kept nothing it had");
    assert.equal(firstResult.endReason, "complete");
    first.close();

    // the same page's next load: the given-up Worker is not made again
    const second = openLagSession({ host: mock.origin, ticket: "ticket-worker-fail-2", downloadWorker: factory });
    await second.ready;
    const secondResult = await second.startLoad();
    assert.equal(workers.length, 1);
    assert.ok(0 < secondResult.download.bytes);
    assert.equal(secondResult.download.intervalBytes.length, 4);
    second.close();
  });
});

test("a page that brings its own fetch reads the download itself", { timeout: 30_000 }, async () => {
  await withMock({ uploadBytesPerSecond: 16 << 20 }, async (mock) => {
    let made = 0;
    let fetched = 0;
    const session = openLagSession({
      host: mock.origin,
      ticket: "ticket-own-fetch",
      fetch: (input, init) => {
        fetched += 1;
        return fetch(input, init);
      },
      downloadWorker: () => {
        made += 1;
        return new FakeWorker() as unknown as Worker;
      },
    });
    await session.ready;
    const result = await session.startLoad();
    assert.equal(made, 0);
    assert.ok(4 <= fetched);
    assert.ok(0 < result.download.bytes);
    session.close();
  });
});

test("the Worker hands over a download's first chunk at once and the rest in batches; a flush hands over the rest; a close ends it", { timeout: 10_000 }, async (t) => {
  // On the test's own clock (its timers and Date), so how busy this machine
  // is changes nothing: the Worker's ticker and the stream's chunks run only
  // as the test moves time, and every hand-over is timed exactly.
  t.mock.timers.enable({ apis: ["setTimeout", "setInterval", "Date"] });
  // the fetch, its reads and their hand-overs are promises only: they run
  // without time passing
  const settle = () => new Promise((resolve) => setImmediate(resolve));
  const advance = async (ms: number) => {
    for (let i = 0; i < ms; i += 1) {
      t.mock.timers.tick(1);
      await settle();
    }
  };
  // one stream of ten chunks 25 ms apart, from a fetch of its own; none
  // falls on a batch's tick, so each belongs to one batch
  const fetchImpl = (async () =>
    new Response(
      new ReadableStream<Uint8Array>({
        async start(controller) {
          for (let i = 0; i < 10; i += 1) {
            controller.enqueue(new Uint8Array(1000));
            await sleep(25);
          }
          controller.close();
        },
      }),
    )) as typeof fetch;
  const posted: LagDownloadToPage[] = [];
  const host = new LagDownloadWorkerHost((message) => posted.push(message), fetchImpl, () => Date.now());
  const begun = Date.now();
  let closed = false;
  try {
    host.receive({ type: "start", id: 7, start: { url: "https://extender.test/lag/v1/down?session=s", streams: 1, downloadMs: 5000, openRetries: 0 } });
    await settle();
    // the first chunk, alone and at once: handed over as it was read, before
    // the ticker's first batch and with no time gone by
    assert.deepEqual(posted, [{ type: "chunks", id: 7, items: [0, begun, 1000], through: begun }]);
    // then batches on the ticker, every LAG_DOWNLOAD_BATCH_MS, each with the
    // chunks read since the one before and saying how far it covers
    await advance(150);
    const batches = posted.slice(1);
    assert.deepEqual(
      batches.map((post) => [post.type, post.type === "chunks" ? post.through : undefined]),
      Array.from({ length: Math.floor(150 / LAG_DOWNLOAD_BATCH_MS) }, (_, i) => ["chunks", begun + (i + 1) * LAG_DOWNLOAD_BATCH_MS]),
    );
    let covered = begun;
    for (const batch of batches) {
      assert.ok(batch.type === "chunks");
      for (let i = 0; i < batch.items.length; i += 3) {
        const at = batch.items[i + 1];
        assert.ok(covered < at && at <= batch.through, `a chunk read at ${at} came in the batch through ${batch.through}`);
      }
      covered = batch.through;
    }
    assert.deepEqual(
      batches.flatMap((batch) => (batch.type === "chunks" ? batch.items : [])),
      [25, 50, 75, 100, 125].flatMap((ms) => [0, begun + ms, 1000]),
    );
    // a flush hands over what came since the last batch, the chunk at 150 ms
    host.receive({ type: "flush", id: 7 });
    assert.deepEqual(posted.at(-1), { type: "flushed", id: 7, items: [0, begun + 150, 1000], through: begun + 150 });
    host.receive({ type: "close", id: 7 });
    closed = true;
    assert.deepEqual(posted.at(-1), { type: "closed", id: 7, items: [], through: begun + 150 });
    const count = posted.length;
    await advance(150);
    // nothing after the close: no chunk, no end, no ticker, though the
    // stream had four chunks left to send
    assert.equal(posted.length, count);
  } finally {
    if (!closed) {
      host.receive({ type: "close", id: 7 });
    }
  }
});
