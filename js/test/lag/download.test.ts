import { test } from "node:test";
import assert from "node:assert/strict";
import { registerTsResolve } from "../ts-resolve.ts";
import { startMockExtender, type MockExtender } from "./client_mock_extender.ts";
import type { LagSessionEvent } from "../../src/lag/client.ts";
import type { LagDownloadToPage, LagDownloadToWorker } from "../../src/lag/download.ts";

registerTsResolve();
const { openLagSession } = await import("../../src/lag/client.ts");
const { LagDownloadWorkerHost, LAG_DOWNLOAD_BATCH_MS } = await import("../../src/lag/download.ts");

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
  /** fail once this many messages after "ready" have reached the page */
  failAfterMessages?: number;
}

type Listener = (event: { data: LagDownloadToPage }) => void;

// A dedicated Worker in this process: the download Worker's host behind the
// same messages and events a real one has.
class FakeWorker {
  readonly sent: LagDownloadToWorker[] = [];
  readonly options: FakeWorkerOptions;
  private readonly messageListeners: Listener[] = [];
  private readonly errorListeners: Array<(event: unknown) => void> = [];
  private readonly host: InstanceType<typeof LagDownloadWorkerHost>;
  private readonly ids = new Set<number>();
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
    if (message.type !== "ready") {
      this.delivered += 1;
    }
    const n = this.delivered;
    setTimeout(() => {
      if (this.terminated) {
        return;
      }
      const failAfter = this.options.failAfterMessages;
      if (failAfter !== undefined && message.type !== "ready" && failAfter < n) {
        for (const listener of this.errorListeners) {
          listener(new Error("the download worker failed"));
        }
        return;
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
      progress.map((event) => [event.ms, event.bytes]),
      record.received.map((report) => [report.ms, report.bytes]),
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

test("chunks the Worker times past the window do not count, and the streams are cut at it", { timeout: 30_000 }, async () => {
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
    // nothing past the window: every byte counted is in the window's intervals
    const inIntervals = result.download.intervalBytes.reduce((a, b) => a + b, 0);
    assert.equal(inIntervals, result.download.bytes);
    assert.equal(result.download.durationMs, hello.downloadMs);
    assert.deepEqual(record.received, expectedReceived(result.download.intervalBytes, hello.intervalMs));
    assert.deepEqual(record.received.at(-1), { bytes: inIntervals, ms: hello.downloadMs });
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
      const worker = new FakeWorker({ failAfterMessages: 3 });
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

test("the Worker hands over a download's first chunk at once and the rest in batches; a flush hands over the rest; a close ends it", { timeout: 10_000 }, async () => {
  // one stream of ten chunks 40 ms apart, from a fetch of its own
  const fetchImpl = (async () =>
    new Response(
      new ReadableStream<Uint8Array>({
        async start(controller) {
          for (let i = 0; i < 10; i += 1) {
            controller.enqueue(new Uint8Array(1000));
            await sleep(40);
          }
          controller.close();
        },
      }),
    )) as typeof fetch;
  const posted: Array<{ at: number; message: LagDownloadToPage }> = [];
  const host = new LagDownloadWorkerHost((message) => posted.push({ at: performance.now(), message }), fetchImpl);
  const begun = performance.now();
  host.receive({ type: "start", id: 7, start: { url: "https://extender.test/lag/v1/down?session=s", streams: 1, downloadMs: 5000, openRetries: 0 } });
  await sleep(20);
  // the first chunk, alone and at once
  const firstWithItems = posted.find((post) => post.message.type === "chunks" && post.message.items.length > 0);
  assert.ok(firstWithItems && firstWithItems.message.type === "chunks");
  assert.deepEqual(firstWithItems.message.items.length, 3);
  assert.ok(firstWithItems.at - begun < LAG_DOWNLOAD_BATCH_MS + 10);
  await sleep(150);
  // batches on the ticker, each saying how far it covers
  const batches = posted.filter((post) => post.message.type === "chunks");
  assert.ok(4 <= batches.length, `${batches.length} batches in 170 ms`);
  const throughs = batches.map((post) => (post.message.type === "chunks" ? post.message.through : 0));
  assert.deepEqual([...throughs].sort((a, b) => a - b), throughs);
  host.receive({ type: "flush", id: 7 });
  const flushed = posted.at(-1)!.message;
  assert.equal(flushed.type, "flushed");
  host.receive({ type: "close", id: 7 });
  assert.equal(posted.at(-1)!.message.type, "closed");
  const count = posted.length;
  await sleep(150);
  // nothing after the close: no chunk, no end, no ticker
  assert.equal(posted.length, count);
  const chunks = posted.flatMap((post) => (post.message.type === "chunks" || post.message.type === "flushed" || post.message.type === "closed" ? post.message.items : []));
  assert.ok(chunks.length / 3 < 10, "the close let the stream run on");
});
