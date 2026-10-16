// Settled download reports must already include every held read that can reach
// them. The client regression drives real Worker messages and holds its flush.
import { test } from "node:test";
import assert from "node:assert/strict";
import { registerTsResolve } from "../ts-resolve.ts";
import type { LagSessionEvent } from "../../src/lag/client.ts";
import type { LagDownloadToPage, LagDownloadToWorker } from "../../src/lag/download.ts";

registerTsResolve();
const { LagIntervalSeries } = await import("../../src/lag/stats.ts");
const { openLagSession, LagSessionError } = await import("../../src/lag/client.ts");

// A lifecycle barrier the test advances without timers.
const deferred = <T>() => {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => { resolve = done; });
  return { promise, resolve };
};

test("a done stream's held first group keeps its intervals unsettled until counted", () => {
  for (const endStream of [false, true]) {
    const series = new LagIntervalSeries({ intervalMs: 250, maxIntervals: 4, windowMs: 1000 });
    series.addRead(0, 0, 100);
    series.addRead(0, 900, 900);
    series.addRead(0, 1100, 2000);
    series.addRead(1, 1300, 3000);
    assert.equal(series.streamDone(0, 1303), true);
    assert.equal(series.streamDone(1, 1303), true);
    assert.equal(series.reach(0, 1303), Number.POSITIVE_INFINITY);
    assert.equal(series.reach(1, 1303), 300, "the uncounted group still reaches [300, 1000]");
    assert.deepEqual(series.bytes, [250, 250, 250, 1150]);
    if (endStream) series.endStream(1);
    else series.finish();
    assert.equal(series.reach(1, 1303), Number.POSITIVE_INFINITY);
    assert.deepEqual(series.bytes, [250, 850, 1000, 1900]);
    series.finish();
    assert.deepEqual(series.bytes, [250, 850, 1000, 1900], "held bytes count exactly once");
  }
});

test("late held download bytes agree in received reports, progress and the finished client result", async (context) => {
  let now = 0;
  context.mock.method(performance, "now", () => now);
  context.mock.method(Date, "now", () => Date.parse("2026-10-12T12:00:00Z") + now);
  // Only delivered watermarks advance this run; no wall-clock timer decides it.
  context.mock.timers.enable({ apis: ["setTimeout", "setInterval"] });
  const started = deferred<number>();
  const flushing = deferred<number>();
  const received: Array<{ bytes: number; ms: number }> = [];
  const events: LagSessionEvent[] = [];
  let sourceClosed = false;

  class ManualWorker extends EventTarget {
    constructor() {
      super();
      queueMicrotask(() => this.deliver({ type: "ready" }));
    }
    postMessage(message: LagDownloadToWorker): void {
      if (message.type === "start") started.resolve(message.id);
      if (message.type === "flush") flushing.resolve(message.id);
      if (message.type === "close") sourceClosed = true;
    }
    deliver(message: LagDownloadToPage): void {
      this.dispatchEvent(new MessageEvent("message", { data: message }));
    }
    terminate(): void {}
  }

  class ControlSocket extends EventTarget {
    readyState = 0;
    bufferedAmount = 0;
    binaryType = "arraybuffer";
    constructor(url: string) {
      super();
      assert.match(url, /^wss:\/\/settlement\.example\/lag\/v1\/session\?/);
      queueMicrotask(() => {
        this.readyState = 1;
        this.dispatchEvent(new MessageEvent("message", { data: JSON.stringify({
          type: "hello", session: "c3ludGhldGlj", uploadKey: "AAAA",
          pingIntervalMs: 250, intervalMs: 250, downloadMs: 1000, uploadMs: 250, maxStreams: 2,
        }) }));
      });
    }
    send(data: string): void {
      const message = JSON.parse(data);
      if (message.type === "received") received.push({ bytes: message.bytes, ms: message.ms });
    }
    close(code = 1000): void {
      if (this.readyState === 3) return;
      this.readyState = 3;
      this.dispatchEvent(Object.assign(new Event("close"), { code, reason: "" }));
    }
  }

  let worker!: ManualWorker;
  const session = openLagSession({
    host: "settlement.example", ticket: "synthetic-ticket",
    WebSocket: ControlSocket as unknown as typeof WebSocket,
    downloadWorker: () => (worker = new ManualWorker()) as unknown as Worker,
    onEvent: (event) => events.push(event),
  });
  try {
    await session.ready;
    // The invalid synthetic upload key ends the run after the real download
    // has finished; the SDK's public partial result carries that download.
    const completed = session.startLoad().catch((error: unknown) => error);
    const id = await started.promise;
    const deliver = (at: number, chunks: number[]) => {
      now = at;
      worker.deliver({ type: "chunks", id, items: chunks, through: performance.timeOrigin + now });
    };
    for (const [stream, at, bytes] of [[0, 0, 100], [0, 900, 900], [0, 1100, 2000], [1, 1300, 3000]]) {
      deliver(at, [stream, performance.timeOrigin + at, bytes]);
    }
    deliver(1303, []);
    assert.equal(await flushing.promise, id, "all streams are done and finalization is waiting for flush");
    assert.equal(sourceClosed, false);
    assert.deepEqual(received, [{ bytes: 250, ms: 250 }], "no report passes the held group's reach before flush");
    const downloadProgress = () => events.filter(
      (event): event is Extract<LagSessionEvent, { type: "progress" }> => event.type === "progress" && event.direction === "download",
    );
    assert.deepEqual(downloadProgress().map((event) => event.bytes), [250]);
    worker.deliver({ type: "flushed", id, items: [], through: performance.timeOrigin + now });
    const error = await completed;
    assert.ok(error instanceof LagSessionError);
    assert.equal(error.code, "protocol");
    assert.ok(error.partial);
    assert.equal(sourceClosed, true);
    assert.deepEqual(error.partial.download.intervalBytes, [250, 850, 1000, 1900]);
    assert.equal(error.partial.download.bytes, 6000);
    assert.deepEqual(received, [
      { bytes: 250, ms: 250 }, { bytes: 1100, ms: 500 }, { bytes: 2100, ms: 750 }, { bytes: 4000, ms: 1000 },
    ]);
    assert.deepEqual(downloadProgress().map((event) => event.interval), [0, 1, 2, 3]);
    assert.deepEqual(downloadProgress().map((event) => event.bytes), received.map((report) => report.bytes));
    assert.deepEqual(downloadProgress().map((event) => event.bytesPerSecond / 4), error.partial.download.intervalBytes);
  } finally {
    session.close();
  }
});
