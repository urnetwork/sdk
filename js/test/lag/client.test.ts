import { test } from "node:test";
import assert from "node:assert/strict";
import { randomBytes } from "node:crypto";
import { createServer } from "node:net";
import { registerTsResolve } from "../ts-resolve.ts";
import { startMockExtender, type MockExtender } from "./client_mock_extender.ts";
import type { LagSession, LagSessionEvent } from "../../src/lag/client.ts";

registerTsResolve();
const { openLagSession, LagSessionError } = await import("../../src/lag/client.ts");

// The session client (lag.dev/PLAN.md S5) against a local mock extender
// (M3-M6) whose loads last 1 s each way. The mock and the client share one
// event loop on a machine that may be busy, so timing is asserted only where
// the protocol bounds it.

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

const waitFor = async (predicate: () => boolean, what: string, timeoutMs = 5000) => {
  const deadline = performance.now() + timeoutMs;
  while (!predicate()) {
    if (deadline < performance.now()) {
      throw new Error(`timed out waiting for ${what}`);
    }
    await sleep(10);
  }
};

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

const open = (mock: MockExtender, ticket: string, extra: Partial<Parameters<typeof openLagSession>[0]> = {}) => {
  const events: LagSessionEvent[] = [];
  const session = openLagSession({
    host: mock.origin,
    ticket,
    onEvent: (event) => events.push(event),
    ...extra,
  });
  return { session, events };
};

const ofType = <T extends LagSessionEvent["type"]>(events: LagSessionEvent[], type: T) =>
  events.filter((event): event is Extract<LagSessionEvent, { type: T }> => event.type === type);

test("a session pings from hello until close()", { timeout: 20_000 }, async () => {
  await withMock({}, async (mock) => {
    const { session, events } = open(mock, "ticket-ping");
    assert.equal(session.state, "connecting");
    const hello = await session.ready;
    assert.equal(session.state, "open");
    assert.deepEqual(
      [hello.pingIntervalMs, hello.intervalMs, hello.downloadMs, hello.uploadMs, hello.maxStreams],
      [250, 250, 1000, 1000, 4],
    );
    assert.equal(ofType(events, "open")[0].hello, hello);
    await waitFor(() => ofType(events, "rtt").length >= 5, "five pings");
    const rtts = ofType(events, "rtt");
    for (const [i, rtt] of rtts.entries()) {
      assert.equal(rtt.lost, false);
      assert.ok(typeof rtt.ms === "number" && 0 <= rtt.ms, `${rtt.ms}`);
      assert.equal(rtt.at, session.openedAt! + rtt.t);
      if (0 < i) {
        // one in flight, the next no sooner than 250 ms after the last send
        // (t is rounded to the millisecond, and a timer may fire 1 ms early)
        const gap = rtt.t - rtts[i - 1].t;
        assert.ok(247 <= gap, `gap ${gap}`);
        assert.ok(rtts[i - 1].ms! <= gap + 2, "the next ping waits for the pong");
      }
    }
    assert.ok(5 <= session.pings.size);
    assert.equal(session.pings.lost(), 0);
    assert.ok(0 <= session.pings.percentiles()!.p50);

    session.close();
    assert.equal(session.state, "closed");
    const closed = await session.closed;
    assert.equal(closed.byClient, true);
    assert.equal(closed.code, 1000);
    assert.equal(closed.error, undefined);
    assert.equal(ofType(events, "closed").length, 1);
    await waitFor(() => mock.sessions[0].closeCode !== undefined, "the extender to see the close");
    assert.equal(mock.sessions[0].closeCode, 1000);
    assert.equal(mock.sessions[0].closedBy, "browser");
    const count = events.length;
    await sleep(400);
    assert.equal(events.length, count, "no events after closed");
  });
});

test("a load runs the download, then the upload, while the pings go on", { timeout: 30_000 }, async () => {
  await withMock({ uploadBytesPerSecond: 16 << 20 }, async (mock) => {
    const { session, events } = open(mock, "ticket-load");
    await session.ready;
    await waitFor(() => ofType(events, "rtt").length >= 2, "pings before the load");
    const result = await session.startLoad();
    const record = mock.sessions[0];

    // the phases begin when data moves, not when they are asked for
    const phases = ofType(events, "load");
    assert.deepEqual(phases.map((event) => event.phase), ["download", "upload", "done"]);
    assert.equal(phases[0].at, result.download.startedAt);
    assert.equal(phases[1].at, result.upload.startedAt);
    assert.ok(result.download.startedAt! + result.download.durationMs <= phases[1].at + 1);
    assert.deepEqual(
      record.messages.filter((m) => m.type === "load").map((m) => m.phase),
      ["download", "upload", "done"],
    );
    assert.equal(result.intervalMs, 250);
    assert.equal(result.uploadVerified, true);
    assert.equal(result.endReason, "complete");
    assert.deepEqual(ofType(events, "loadResult")[0].result, result);

    // download: four streams at once, every byte the extender wrote, per interval
    assert.equal(record.maxConcurrentDownloads, 4);
    assert.equal(result.download.streams, 4);
    assert.equal(result.download.bytes, record.downloadBytes);
    assert.equal(result.download.mbps.length, 4);
    assert.equal(result.download.intervalBytes.length, 4);
    const inIntervals = result.download.intervalBytes.reduce((a, b) => a + b, 0);
    assert.ok(0 < inIntervals && inIntervals <= result.download.bytes);
    assert.deepEqual(
      result.download.mbps,
      result.download.intervalBytes.map((bytes) => (bytes * 8) / (250 * 1000)),
    );
    const download = result.download.percentiles!;
    assert.ok(0 < download.p95 && download.p50 <= download.p95);
    // the browser's reports: every interval from the first byte, cumulative,
    // and a last one with the total
    assert.ok(2 <= record.received.length, `${record.received.length} reports`);
    for (let i = 1; i < record.received.length; i += 1) {
      assert.ok(record.received[i - 1].ms <= record.received[i].ms);
      assert.ok(record.received[i - 1].bytes <= record.received[i].bytes);
    }
    assert.equal(record.received.at(-1)!.bytes, result.download.bytes);
    assert.ok(249 <= record.received[0].ms, `first report at ${record.received[0].ms}`);
    const downloadProgress = ofType(events, "progress").filter((event) => event.direction === "download");
    assert.deepEqual(downloadProgress.map((event) => event.interval), [0, 1, 2, 3]);
    assert.ok(downloadProgress.every((event) => event.durationMs === 1000));

    // upload: four streams of whole 64 KiB messages, verified
    assert.equal(record.mismatch, false);
    assert.equal(record.uploadStreams, 4);
    assert.equal(result.upload.streams, 4);
    assert.deepEqual(record.oddUploadMessages, []);
    assert.ok(0 < record.uploadVerifiedBytes);
    assert.equal(result.upload.bytes, record.uploadReports.at(-1)!.bytes);
    assert.ok(1 <= result.upload.mbps.length && result.upload.mbps.length <= 4);
    assert.ok(0 < result.upload.percentiles!.p95);
    const uploadProgress = ofType(events, "progress").filter((event) => event.direction === "upload");
    assert.equal(uploadProgress.length, record.uploadReports.length);
    assert.deepEqual(
      uploadProgress.map((event) => event.bytes),
      record.uploadReports.map((report) => report.bytes),
    );

    // the pings went on through the load, and go on after it
    const loads = ofType(events, "load");
    const during = ofType(events, "rtt").filter((rtt) => loads[0].at <= rtt.at && rtt.at < loads[2].at);
    assert.ok(2 <= during.length, `${during.length} pings during the load`);
    const after = ofType(events, "rtt").length;
    await waitFor(() => after + 2 <= ofType(events, "rtt").length, "pings after the load");
    assert.equal(mock.activeStreams(), 0);

    // one load per session
    await assert.rejects(session.startLoad(), (error: unknown) => {
      assert.ok(error instanceof LagSessionError);
      assert.equal(error.code, "used");
      return true;
    });
    assert.equal(record.loadRequests, 1);
    session.close();
  });
});

for (const code of ["busy", "budget"] as const) {
  test(`a load refused as ${code} rejects, the session goes on, and the load can be asked for again`, { timeout: 30_000 }, async () => {
    await withMock({ refuseLoads: [{ code, retryAfterMs: 5000 }] }, async (mock) => {
      const { session, events } = open(mock, `ticket-${code}`);
      await session.ready;
      await assert.rejects(session.startLoad(), (error: unknown) => {
        assert.ok(error instanceof LagSessionError);
        assert.equal(error.code, code);
        assert.equal(error.retryAfterMs, 5000);
        assert.equal(error.partial, undefined);
        return true;
      });
      const refusal = ofType(events, "error")[0];
      assert.equal(refusal.fatal, false);
      assert.equal(refusal.error.code, code);
      assert.equal(ofType(events, "load").length, 0, "a refused load never started");
      assert.equal(session.state, "open");
      assert.equal(mock.activeStreams(), 0);
      const before = ofType(events, "rtt").length;
      await waitFor(() => before + 2 <= ofType(events, "rtt").length, "pings after the refusal");

      const result = await session.startLoad();
      assert.equal(result.endReason, "complete");
      assert.equal(result.uploadVerified, true);
      assert.deepEqual(ofType(events, "load").map((event) => event.phase), ["download", "upload", "done"]);
      assert.equal(mock.sessions[0].loadRequests, 2);
      assert.equal(mock.sessions[0].refused, 1);
      assert.equal(ofType(events, "closed").length, 0);
      session.close();
    });
  });
}

test("an upload mismatch ends the load unverified, and the session goes on", { timeout: 30_000 }, async () => {
  await withMock({ verifyKey: randomBytes(16) }, async (mock) => {
    const { session, events } = open(mock, "ticket-mismatch");
    await session.ready;
    const result = await session.startLoad();
    assert.equal(mock.sessions[0].mismatch, true);
    assert.equal(result.uploadVerified, false);
    assert.equal(result.endReason, "upload_mismatch");
    assert.ok(0 < result.download.bytes);
    assert.equal(result.upload.bytes, 0);
    assert.deepEqual(ofType(events, "load").map((event) => event.phase), ["download", "upload", "done"]);
    assert.equal(ofType(events, "loadResult").length, 1);
    assert.equal(session.state, "open");
    const before = ofType(events, "rtt").length;
    await waitFor(() => before + 2 <= ofType(events, "rtt").length, "pings after the mismatch");
    await waitFor(() => mock.activeStreams() === 0, "the upload sockets to close");
    session.close();
  });
});

for (const direction of ["download", "upload"] as const) {
  test(`close() during the ${direction} cuts the load and every connection`, { timeout: 30_000 }, async () => {
    await withMock({ uploadBytesPerSecond: 16 << 20 }, async (mock) => {
      let session!: LagSession;
      const events: LagSessionEvent[] = [];
      session = openLagSession({
        host: mock.origin,
        ticket: `ticket-close-${direction}`,
        onEvent: (event) => {
          events.push(event);
          if (event.type === "progress" && event.direction === direction && 1 <= event.interval) {
            session.close();
          }
        },
      });
      await session.ready;
      await assert.rejects(session.startLoad(), (error: unknown) => {
        assert.ok(error instanceof LagSessionError);
        assert.equal(error.code, "closed");
        assert.ok(error.partial !== undefined && 0 < error.partial.download.bytes);
        return true;
      });
      const closed = await session.closed;
      assert.equal(closed.byClient, true);
      // the load's done comes before the session's closed
      const order = events.filter((event) => event.type === "load" || event.type === "closed");
      assert.deepEqual(
        order.map((event) => (event.type === "load" ? event.phase : event.type)),
        direction === "download" ? ["download", "done", "closed"] : ["download", "upload", "done", "closed"],
      );
      await waitFor(
        () => mock.activeStreams() === 0 && mock.openSessions() === 0,
        "the extender to see every connection close",
      );
      assert.equal(mock.sessions[0].closedBy, "browser");
      assert.equal(mock.sessions[0].closeCode, 1000);
      assert.equal(ofType(events, "loadResult").length, 0);
      await assert.rejects(session.startLoad(), (error: unknown) => (error as InstanceType<typeof LagSessionError>).code === "closed");
    });
  });
}

test("lost pings are reported, and 30 s without a pong ends the session", { timeout: 20_000 }, async () => {
  await withMock({ ignorePongs: true, lostAfterMs: 150, noPongCloseMs: 1500 }, async (mock) => {
    const { session, events } = open(mock, "ticket-lost");
    await session.ready;
    const closed = await session.closed;
    const rtts = ofType(events, "rtt");
    assert.ok(2 <= rtts.length, `${rtts.length} lost pings`);
    assert.ok(rtts.every((rtt) => rtt.lost && rtt.ms === null));
    assert.ok(2 <= session.pings.lost());
    assert.equal(session.pings.percentiles(), undefined);
    assert.equal(closed.byClient, false);
    assert.equal(closed.code, 1001);
    assert.equal(closed.error?.code, "dropped");
    assert.equal(ofType(events, "error").at(-1)?.fatal, true);
    assert.equal(mock.sessions[0].closedBy, "extender");
    await assert.rejects(session.startLoad(), (error: unknown) => (error as InstanceType<typeof LagSessionError>).code === "closed");
  });
});

test("admission: a bad ticket, a reused ticket and too many sessions end the session", { timeout: 20_000 }, async () => {
  await withMock({ maxSessions: 1 }, async (mock) => {
    const bad = open(mock, "bad-ticket");
    await assert.rejects(bad.session.ready, (error: unknown) => {
      assert.ok(error instanceof LagSessionError);
      assert.equal(error.code, "ticket");
      assert.equal(error.closeCode, 4403);
      return true;
    });
    const closed = await bad.session.closed;
    assert.equal(closed.code, 4403);
    assert.equal(closed.error?.code, "ticket");
    assert.deepEqual(ofType(bad.events, "error").map((event) => [event.error.code, event.fatal]), [["ticket", true]]);

    const first = open(mock, "ticket-a");
    await first.session.ready;
    const reused = open(mock, "ticket-a");
    await assert.rejects(reused.session.ready, (error: unknown) => (error as InstanceType<typeof LagSessionError>).code === "reused");
    const third = open(mock, "ticket-b");
    await assert.rejects(third.session.ready, (error: unknown) => {
      const lagError = error as InstanceType<typeof LagSessionError>;
      return lagError.code === "sessions" && lagError.closeCode === 4429;
    });
    assert.equal(first.session.state, "open");
    first.session.close();
  });
});

test("a session that goes silent is closed; an unreachable host fails to connect", { timeout: 20_000 }, async () => {
  await withMock({ silent: true }, async (mock) => {
    const { session } = open(mock, "ticket-silent", { silenceMs: 300 });
    await session.ready;
    const closed = await session.closed;
    assert.equal(closed.error?.code, "silent");
    assert.equal(closed.byClient, false);
    await waitFor(() => mock.sessions[0].closeCode !== undefined, "the extender to see the close");
    assert.equal(mock.sessions[0].closedBy, "browser");
  });

  // a port with nothing listening
  const server = createServer();
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const { port } = server.address() as { port: number };
  await new Promise((resolve) => server.close(resolve));
  const unreachable = openLagSession({ host: `http://127.0.0.1:${port}`, ticket: "ticket-x" });
  await assert.rejects(unreachable.ready, (error: unknown) => (error as InstanceType<typeof LagSessionError>).code === "connect");
  assert.equal((await unreachable.closed).error?.code, "connect");
});

test("a download stream refused before the extender has the load is retried", { timeout: 30_000 }, async () => {
  await withMock({ firstDownloadStatus: 409 }, async (mock) => {
    const { session } = open(mock, "ticket-retry");
    await session.ready;
    const result = await session.startLoad();
    assert.equal(mock.sessions[0].downloadRefused, 4);
    assert.equal(mock.sessions[0].downloadStreams, 4);
    assert.equal(result.download.streams, 4);
    assert.equal(result.download.mbps.length, 4);
    session.close();
  });
});

test("an upload stream that never connects does not hold up the load", { timeout: 30_000 }, async () => {
  await withMock({ holdUploadStream: 3, uploadBytesPerSecond: 16 << 20 }, async (mock) => {
    const { session } = open(mock, "ticket-held");
    await session.ready;
    const started = performance.now();
    const result = await session.startLoad();
    // well inside the 10 s a stream gets to connect
    assert.ok(performance.now() - started < 8000, `${performance.now() - started} ms`);
    assert.equal(result.upload.streams, 3);
    assert.equal(mock.sessions[0].uploadStreams, 3);
    assert.equal(result.uploadVerified, true);
    assert.equal(result.endReason, "complete");
    session.close();
  });
});

test("the upload keeps bufferedAmount under its cap on a slow uplink", { timeout: 30_000 }, async () => {
  await withMock({ uploadBytesPerSecond: 2 << 20 }, async (mock) => {
    let largest = 0;
    class MeasuredWebSocket extends WebSocket {
      send(data: string | ArrayBufferLike | Blob | ArrayBufferView) {
        if (typeof data !== "string") {
          largest = Math.max(largest, this.bufferedAmount);
        }
        super.send(data);
      }
    }
    const { session } = open(mock, "ticket-slow", { WebSocket: MeasuredWebSocket });
    await session.ready;
    const result = await session.startLoad();
    assert.equal(result.uploadVerified, true);
    assert.ok(0 < largest, "the upload buffered");
    assert.ok(largest + 65536 <= 8 << 20, `bufferedAmount reached ${largest}`);
    session.close();
  });
});

// an extender id and the measurement name it gives (N1)
const EXTENDER_ID = "0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f";

test("a host is an extender's name, with any port it carries, over https and wss", () => {
  const urls: string[] = [];
  class RecordingWebSocket {
    binaryType = "blob";
    readyState = 0;
    constructor(url: string) {
      urls.push(url);
    }
    addEventListener() {}
    close() {}
  }
  const WebSocketImpl = RecordingWebSocket as unknown as typeof WebSocket;
  const hosts = [
    `${EXTENDER_ID}.extender.bringyour.com`,
    // a local test stack's host carries a port (A4, A6)
    `${EXTENDER_ID}.extender.localhost:8443`,
    "127.0.0.1:9443",
    "[::1]:8443",
  ];
  for (const host of hosts) {
    openLagSession({ host, ticket: "a b+c", WebSocket: WebSocketImpl }).close();
  }
  assert.deepEqual(
    urls,
    hosts.map((host) => `wss://${host}/lag/v1/session?ticket=a%20b%2Bc`),
  );
  // an origin is used as given, without its path
  urls.length = 0;
  openLagSession({ host: "https://127.0.0.1:8443/ignored", ticket: "t", WebSocket: WebSocketImpl }).close();
  openLagSession({ host: "http://127.0.0.1:8080", ticket: "t", WebSocket: WebSocketImpl }).close();
  assert.deepEqual(urls, [
    "wss://127.0.0.1:8443/lag/v1/session?ticket=t",
    "ws://127.0.0.1:8080/lag/v1/session?ticket=t",
  ]);
  for (const host of ["evil.example/path", "a b", "x?y", "h#f", "user@host", "host:99999", "::1", ""]) {
    assert.throws(() => openLagSession({ host, ticket: "t", WebSocket: WebSocketImpl }), TypeError, host);
  }
});
