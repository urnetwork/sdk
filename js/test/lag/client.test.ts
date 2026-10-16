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

// what M4 says the browser reports for a download: one `received` per
// completed interval i, at ms (i+1) x interval, with the bytes through i
const expectedReceived = (intervalBytes: number[], intervalMs: number) => {
  let total = 0;
  return intervalBytes.map((bytes, i) => {
    total += bytes;
    return { bytes: total, ms: (i + 1) * intervalMs };
  });
};

const ofType = <T extends LagSessionEvent["type"]>(events: LagSessionEvent[], type: T) =>
  events.filter((event): event is Extract<LagSessionEvent, { type: T }> => event.type === type);

// A direction's progress events go out at the load's own time, within its
// window, and neither that time nor their bytes ever go back (S5)
const assertProgressTime = (progress: Array<{ ms: number; bytes: number }>, windowMs: number) => {
  progress.forEach((event, i) => {
    assert.ok(0 <= event.ms && event.ms <= windowMs, `progress at ${event.ms} ms`);
    if (0 < i) {
      assert.ok(progress[i - 1].ms <= event.ms, "the progress went back");
      assert.ok(progress[i - 1].bytes <= event.bytes, "the bytes went back");
    }
  });
};

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
    // each ping's time is the session's origin, as it stood then, plus t;
    // the origin starts at the hello and only ever moves earlier
    const origins = rtts.map((rtt) => rtt.at - rtt.t);
    assert.ok(origins[0] <= ofType(events, "open")[0].at + 0.001);
    assert.ok(Math.abs(origins.at(-1)! - session.openedAt!) < 0.001);
    for (const [i, rtt] of rtts.entries()) {
      assert.equal(rtt.lost, false);
      assert.ok(typeof rtt.ms === "number" && 0 <= rtt.ms, `${rtt.ms}`);
      if (0 < i) {
        assert.ok(origins[i] <= origins[i - 1] + 0.001, "the origin moved later");
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
    // when the page made its first upload socket, and sent the upload message
    let firstUploadSocketAt: number | undefined;
    let uploadMessageAt: number | undefined;
    class TimedWebSocket extends WebSocket {
      constructor(url: string | URL, protocols?: string | string[]) {
        super(url, protocols);
        if (String(url).includes("/lag/v1/up")) {
          firstUploadSocketAt ??= performance.now();
        }
      }
      send(data: string | ArrayBufferLike | Blob | ArrayBufferView) {
        if (typeof data === "string" && data.includes('"phase":"upload"')) {
          uploadMessageAt ??= performance.now();
        }
        super.send(data);
      }
    }
    const { session, events } = open(mock, "ticket-load", { WebSocket: TimedWebSocket });
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

    // download: four streams at once, what arrived inside the window, per
    // interval; the extender's last writes may land just past it
    assert.equal(record.maxConcurrentDownloads, 4);
    assert.equal(result.download.streams, 4);
    assert.ok(0 < result.download.bytes && result.download.bytes <= record.downloadBytes);
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
    // the browser's reports: one per interval of the window, from its series
    assert.deepEqual(record.received, expectedReceived(result.download.intervalBytes, 250));
    const downloadProgress = ofType(events, "progress").filter((event) => event.direction === "download");
    assert.deepEqual(downloadProgress.map((event) => event.interval), [0, 1, 2, 3]);
    assert.ok(downloadProgress.every((event) => event.durationMs === 1000));
    assertProgressTime(downloadProgress, 1000);

    // upload: four streams of whole 64 KiB messages, verified; its sockets
    // began to open shortly before the download's window ended, so before
    // the upload message, to wait for it at the extender (how punctually,
    // and how soon one gets there, is up to this machine)
    assert.ok(firstUploadSocketAt !== undefined && uploadMessageAt !== undefined && firstUploadSocketAt < uploadMessageAt, "no upload socket was opened before the upload message");
    assert.equal(record.mismatch, false);
    assert.equal(record.uploadStreams, 4);
    assert.equal(result.upload.streams, 4);
    assert.deepEqual(record.oddUploadMessages, []);
    assert.ok(0 < record.uploadVerifiedBytes);
    assert.equal(result.upload.bytes, record.uploadReports.at(-1)!.bytes);
    assert.ok(record.uploadReports.every((report) => report.at !== undefined && report.at <= report.ms));
    // its result is the extender's own series (M4 loadResult uploadBytes),
    // within the window its last report gives
    assert.deepEqual(record.loadResult?.uploadBytes, record.uploadBytes);
    assert.deepEqual(result.upload.intervalBytes, record.uploadBytes.slice(0, Math.ceil(record.uploadReports.at(-1)!.ms / 250)));
    assert.ok(1 <= result.upload.mbps.length && result.upload.mbps.length <= 4);
    assert.ok(0 < result.upload.percentiles!.p95);
    // while the live progress came once per interval of the series the
    // extender's reports built, at its time
    const uploadProgress = ofType(events, "progress").filter((event) => event.direction === "upload");
    assert.deepEqual(uploadProgress.map((event) => event.interval), [0, 1, 2, 3]);
    assertProgressTime(uploadProgress, 1000);
    assert.ok(uploadProgress.at(-1)!.bytes <= record.uploadVerifiedBytes);

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

test("the download ends once its window is final: streams still delivering are cut and the upload starts at once", { timeout: 30_000 }, async () => {
  // The extender keeps writing every stream 3 s past a 2 s window; a gentle
  // pace keeps the mock, which shares this event loop, from delaying
  // timers. Each stream writes every 20 ms, so each is done, 2 ms after its
  // first write past the window's end, within some 25 ms of it, and the
  // window is final then (M6, amendment 15), far sooner than the 1 s a
  // stream that stopped could hold it. A 500 ms interval leaves room for a
  // busy machine's scheduling in a bound of two intervals.
  const options = {
    hello: { intervalMs: 500, downloadMs: 2000 },
    downloadOverrunMs: 3000,
    downloadPauseMs: 20,
    uploadBytesPerSecond: 4 << 20,
  };
  await withMock(options, async (mock) => {
    // The window end as this process sees it: a timer due when the window
    // closes, set as the download's first byte arrives. A busy machine fires
    // it late exactly as it fires the client's own, so measuring from it
    // leaves only the client's delay.
    let observedEnd: number | undefined;
    const { session, events } = open(mock, "ticket-overrun", {
      onEvent: (event) => {
        events.push(event);
        if (event.type === "load" && event.phase === "download") {
          const due = event.at + session.hello!.downloadMs;
          setTimeout(() => {
            observedEnd = Date.now();
          }, due - Date.now());
        }
      },
    });
    const hello = await session.ready;
    const result = await session.startLoad();
    const record = mock.sessions[0];
    const phases = ofType(events, "load");
    assert.deepEqual(phases.map((event) => event.phase), ["download", "upload", "done"]);
    assert.equal(result.endReason, "complete");
    assert.equal(result.uploadVerified, true);

    // the upload's first message goes out within two intervals of the
    // download window's end, downloadMs from its first byte: the window is
    // final tens of ms after it, and the upload's sockets have had 250 ms
    // before it to open
    const windowEnd = result.download.startedAt! + hello.downloadMs;
    const gap = phases[1].at - windowEnd;
    assert.ok(observedEnd !== undefined);
    const reaction = phases[1].at - observedEnd;
    assert.ok(reaction <= 2 * hello.intervalMs, `the upload began ${reaction} ms after the window was seen to end (${gap} ms by the clock)`);
    // and nowhere near the 2 s a client that waited out trailing bytes would take
    assert.ok(-2 <= gap && gap < 1500, `the upload began ${gap} ms after the download window`);
    assert.equal(phases[1].at, result.upload.startedAt);

    // every stream was still delivering, and the browser cut each one
    assert.equal(record.downloadCutByBrowser, 4);
    assert.ok(result.download.bytes < record.downloadBytes);
    // the series holds the window's intervals and nothing past it, though
    // chunks past the end counted what they carried before it, and the
    // reports end with the window's last interval
    assert.equal(result.download.mbps.length, hello.downloadMs / hello.intervalMs);
    const inIntervals = result.download.intervalBytes.reduce((a, b) => a + b, 0);
    assert.ok(0 < inIntervals && inIntervals <= result.download.bytes);
    assert.deepEqual(record.received, expectedReceived(result.download.intervalBytes, hello.intervalMs));
    assert.deepEqual(record.received.at(-1), { bytes: inIntervals, ms: hello.downloadMs });
    session.close();
  });
});

test("a page that stalls during the download still reports each interval once, at its own time", { timeout: 30_000 }, async () => {
  await withMock({ uploadBytesPerSecond: 16 << 20 }, async (mock) => {
    let stalled = false;
    const { session, events } = open(mock, "ticket-stall", {
      onEvent: (event) => {
        events.push(event);
        // a long task on the page, as the first interval completes
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
    // the ticks after the stall caught up: still one report per interval,
    // at (i+1) x interval, with the bytes through it, and none past the window
    assert.deepEqual(record.received, expectedReceived(result.download.intervalBytes, hello.intervalMs));
    assert.equal(record.received.length, hello.downloadMs / hello.intervalMs);
    const progress = ofType(events, "progress").filter((event) => event.direction === "download");
    assert.deepEqual(progress.map((event) => event.interval), [0, 1, 2, 3]);
    assert.deepEqual(
      progress.map((event) => event.bytes),
      record.received.map((report) => report.bytes),
    );
    assertProgressTime(progress, hello.downloadMs);
    session.close();
  });
});

test("each `received` report carries its interval's final value: with a slow stream, a stream that never opens, and past the window", { timeout: 30_000 }, async () => {
  // Stream 2 writes 64 KiB every 400 ms where the others write every 20 ms,
  // so by rule S its bytes spread back over intervals the others have long
  // passed; stream 3 is refused every time it asks, so it never opens; and
  // every stream keeps writing 1 s past a 2 s window.
  const options = {
    hello: { downloadMs: 2000 },
    downloadPauseMs: 20,
    slowDownloadStream: { stream: 2, pauseMs: 400 },
    refuseDownloadStream: 3,
    downloadOverrunMs: 1000,
    uploadBytesPerSecond: 4 << 20,
  };
  await withMock(options, async (mock) => {
    const { session, events } = open(mock, "ticket-settled");
    const hello = await session.ready;
    const result = await session.startLoad();
    const record = mock.sessions[0];
    assert.equal(result.endReason, "complete");
    assert.equal(result.download.streams, 3);
    // the first try and three retries, all refused
    assert.equal(record.downloadRefused, 4);
    assert.equal(result.download.intervalBytes.length, hello.downloadMs / hello.intervalMs);
    // what the extender signs is what the browser shows, interval by interval
    assert.deepEqual(record.received, expectedReceived(result.download.intervalBytes, hello.intervalMs));
    const progress = ofType(events, "progress").filter((event) => event.direction === "download");
    assert.deepEqual(progress.map((event) => event.interval), [0, 1, 2, 3, 4, 5, 6, 7]);
    assert.deepEqual(
      progress.map((event) => event.bytes),
      record.received.map((report) => report.bytes),
    );
    assertProgressTime(progress, hello.downloadMs);
    session.close();
  });
});

test("from an extender without `at` and `uploadBytes`, the upload's result is the series its reports build", { timeout: 30_000 }, async () => {
  await withMock({ legacyUpload: true, uploadBytesPerSecond: 16 << 20 }, async (mock) => {
    const { session, events } = open(mock, "ticket-legacy");
    await session.ready;
    const result = await session.startLoad();
    const record = mock.sessions[0];
    assert.equal(result.uploadVerified, true);
    assert.ok(record.uploadReports.every((report) => report.at === undefined));
    assert.equal(record.loadResult && "uploadBytes" in record.loadResult, false);
    // the live series, from each report's (ms, bytes), one progress event
    // per interval, stands as the result
    const progress = ofType(events, "progress").filter((event) => event.direction === "upload");
    assert.deepEqual(progress.map((event) => event.interval), [0, 1, 2, 3]);
    assertProgressTime(progress, 1000);
    assert.deepEqual(
      result.upload.intervalBytes,
      progress.map((event, i) => event.bytes - (i === 0 ? 0 : progress[i - 1].bytes)),
    );
    assert.equal(result.upload.bytes, record.uploadReports.at(-1)!.bytes);
    assert.ok(0 < result.upload.percentiles!.p50);
    session.close();
  });
});

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

test("a stall while the hello is handled does not shift the session's pings", { timeout: 20_000 }, async () => {
  // the hello waits 400 ms to be handled, as on a page busy when it arrives
  await withMock({ stallAfterHelloMs: 400 }, async (mock) => {
    // when the page handled each round trip's report, on the clock its
    // times are in
    const handled: number[] = [];
    const { session, events } = open(mock, "ticket-hello-stall", {
      onEvent: (event) => {
        events.push(event);
        if (event.type === "rtt") {
          handled.push(Date.now());
        }
      },
    });
    await session.ready;
    const record = mock.sessions[0];
    const opened = record.openedEpoch!;
    const late = ofType(events, "open")[0].at - opened;
    assert.ok(300 <= late, `the hello was handled ${late} ms after the session opened`);
    // Each round trip bounds the session's opening: the extender had its
    // pong t + ms after it and reported it at once, so the session opened
    // no later than the report's handling less t + ms. How close a bound
    // comes is how soon the report was handled after it went, which a busy
    // machine makes later; so wait for one that places the opening well
    // before the late hello.
    const bound = (i: number) => {
      const rtt = ofType(events, "rtt")[i];
      return handled[i] - (rtt.t + rtt.ms!) - opened;
    };
    await waitFor(
      () => {
        const rtts = ofType(events, "rtt");
        return 4 <= rtts.length && rtts.some((rtt, i) => !rtt.lost && bound(i) < late / 2);
      },
      "a round trip that places the session's opening before the hello",
      15_000,
    );
    // the origin is the earliest bound so far, and each ping sits at the
    // origin as it stood then plus its own send time, so the pings are not
    // shifted by the stall (to 5 ms: t and Date.now() are whole ms, and the
    // wall clock may be slewed against the monotonic one)
    const rtts = ofType(events, "rtt");
    let origin = late;
    rtts.forEach((rtt, i) => {
      if (!rtt.lost) {
        // a bound is never before the opening, nor later than its report took
        const delivery = handled[i] - record.rttSentEpoch[i];
        assert.ok(-5 <= bound(i) && bound(i) <= delivery + 5, `ping ${i}'s bound is ${bound(i)} ms after the opening, its report took ${delivery} ms`);
        origin = Math.min(origin, bound(i));
      }
      const drift = rtt.at - record.pingSentEpoch[i];
      assert.ok(Math.abs(drift - origin) <= 5, `ping ${i} is ${drift} ms from its send, where the origin then was ${origin} ms after the opening`);
    });
    assert.ok(origin < late / 2);
    const off = session.openedAt! - opened;
    assert.ok(Math.abs(off - origin) <= 5, `the origin is ${off} ms from the session's opening, its earliest bound ${origin} ms`);
    // the window follows the origin as later round trips refine it
    assert.deepEqual(
      session.pings.pings().map((ping) => ping.t),
      rtts.map((rtt) => session.openedAt! + rtt.t),
    );
    session.close();
  });
});

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
    // M4: the extender closes a session without pongs with 4408, which the
    // browser counts as dropped
    assert.equal(closed.byClient, false);
    assert.equal(closed.code, 4408);
    assert.equal(closed.error?.code, "dropped");
    assert.equal(closed.error?.closeCode, 4408);
    assert.equal(mock.sessions[0].closeCode, 4408);
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
    // the streams that opened start once the 3 s the sockets get to open
    // are over: about 1 s of download, 3 s, 1 s of upload and its result
    assert.ok(performance.now() - started < 9000, `${performance.now() - started} ms`);
    assert.equal(result.upload.streams, 3);
    assert.equal(mock.sessions[0].uploadStreams, 3);
    assert.equal(result.uploadVerified, true);
    assert.equal(result.endReason, "complete");
    session.close();
  });
});

test("upload streams start together even when their sockets open one at a time", { timeout: 30_000 }, async () => {
  // A browser makes WebSocket handshakes to one host one at a time; the mock
  // answers the upgrades 300 ms apart, as that makes them complete.
  await withMock({ uploadUpgradeSpacingMs: 300, uploadBytesPerSecond: 16 << 20 }, async (mock) => {
    const { session, events } = open(mock, "ticket-serialized");
    await session.ready;
    const result = await session.startLoad();
    const record = mock.sessions[0];
    assert.equal(result.upload.streams, 4);
    assert.equal(result.uploadVerified, true);
    const opens = record.uploadOpenedAt;
    const firsts = record.uploadFirstMessageAt;
    assert.equal(opens.filter((at) => at !== undefined).length, 4);
    assert.equal(firsts.filter((at) => at !== undefined).length, 4);
    // the mock answered them one at a time, 300 ms apart, and none opened
    // before its turn (a timer fires up to a millisecond early); a busy
    // machine opens one late, which can bring the first nearer the last
    const turns = record.uploadAnswerAt.filter((at) => at !== undefined).sort((a, b) => a - b);
    assert.equal(turns.length, 4);
    turns.slice(1).forEach((at, i) => assert.ok(300 <= at - turns[i] + 0.001, `turns ${at - turns[i]} ms apart`));
    record.uploadAnswerAt.forEach((at, k) => assert.ok(at <= opens[k] + 2, `upload socket ${k} opened ${at - opens[k]} ms before its turn`));
    const lastOpen = Math.max(...opens);
    // no stream began before the last socket opened, and they began together
    const spread = Math.max(...firsts) - Math.min(...firsts);
    assert.ok(lastOpen <= Math.min(...firsts), "a stream began before every socket had opened");
    assert.ok(spread <= 150, `the streams began over ${spread} ms`);
    // the upload's window starts at that common start
    const phases = ofType(events, "load");
    assert.equal(phases[1].phase, "upload");
    assert.equal(phases[1].at, result.upload.startedAt);
    session.close();
  });
});

// Windows of a few seconds, as the extender's are, so the first interval's
// ramp and, at 128 KiB/s a stream (half a second a message), the message
// each stream can still hold when the window ends stay a small part of it.
for (const [perStream, uploadMs] of [
  [1 << 20, 3000],
  [1 << 17, 4000],
]) {
  test(`the upload sends little more than the extender verifies, at ${perStream / 1024} KiB/s a stream: its backlog follows the verified rate`, { timeout: 30_000 }, async () => {
    // each of the four streams read only this fast, so the kernel's buffers
    // between them could hold far more than the window takes
    await withMock({ uploadBytesPerSecond: perStream, hello: { uploadMs } }, async (mock) => {
      let sent = 0;
      let sentAfter = 0;
      let closedAt: number | undefined;
      class CountingWebSocket extends WebSocket {
        send(data: string | ArrayBufferLike | Blob | ArrayBufferView) {
          if (typeof data !== "string") {
            sent += (data as ArrayBufferView).byteLength;
            if (closedAt !== undefined) {
              sentAfter += (data as ArrayBufferView).byteLength;
            }
          }
          super.send(data);
        }
        close(code?: number, reason?: string) {
          if (this.url.includes("/lag/v1/up")) {
            closedAt ??= performance.now();
          }
          super.close(code, reason);
        }
      }
      const { session, events } = open(mock, "ticket-backlog", { WebSocket: CountingWebSocket });
      await session.ready;
      await waitFor(() => ofType(events, "rtt").length >= 2, "pings before the load");
      const result = await session.startLoad();
      const record = mock.sessions[0];
      assert.equal(result.uploadVerified, true);
      const verified = record.uploadVerifiedBytes;
      // the window was full: the four streams' rate for its length, give or
      // take the first interval and a message a stream
      const capacity = (4 * perStream * uploadMs) / 1000;
      assert.ok(capacity * 0.8 <= verified, `${verified} bytes verified of the ${capacity} the window could take`);
      const overshoot = (sent - verified) / verified;
      assert.ok(overshoot < 0.2, `sent ${sent} bytes for ${verified} verified (${(overshoot * 100).toFixed(0)}% more)`);
      // the streams closed at the window's end, and nothing went after
      assert.ok(closedAt !== undefined, "the upload's sockets never closed");
      assert.equal(sentAfter, 0);
      session.close();
    });
  });
}

test("the upload's streams stay open past the window's end until the extender says it is over, then close on it", { timeout: 30_000 }, async () => {
  // An extender of amendment 15 says its upload window is over only once
  // the window is final, which can be 1 s after its end (M4, M6); here it
  // waits 700 ms. The browser's streams send until that report, rather than
  // for uploadMs by its own clock, then close at once, and done follows.
  await withMock({ uploadBytesPerSecond: 1 << 20, uploadFinal: true, uploadWindowOverDelayMs: 700 }, async (mock) => {
    let sentPastEnd = 0;
    let closedAt: number | undefined;
    class CountingWebSocket extends WebSocket {
      send(data: string | ArrayBufferLike | Blob | ArrayBufferView) {
        const first = mock.sessions[0]?.uploadFirstAt;
        if (typeof data !== "string" && first !== undefined && first + 1000 < performance.now()) {
          sentPastEnd += (data as ArrayBufferView).byteLength;
        }
        super.send(data);
      }
      close(code?: number, reason?: string) {
        if (this.url.includes("/lag/v1/up")) {
          closedAt ??= performance.now();
        }
        super.close(code, reason);
      }
    }
    const { session } = open(mock, "ticket-late-over", { WebSocket: CountingWebSocket });
    await session.ready;
    const result = await session.startLoad();
    const record = mock.sessions[0];
    assert.equal(result.uploadVerified, true);
    assert.equal(result.endReason, "complete");
    const end = record.uploadFirstAt! + 1000;
    const over = record.uploadWindowOverAt!;
    assert.ok(700 <= over - end, `the window-over report went ${over - end} ms after the end`);
    // the streams sent on past the window's end, and closed on the report
    assert.ok(0 < sentPastEnd, "nothing was sent past the window's end");
    assert.ok(closedAt !== undefined && over <= closedAt, `the streams closed ${over - closedAt!} ms before the report`);
    assert.ok(closedAt! - over < 300, `the streams closed ${closedAt! - over} ms after the report`);
    // the report's bytes are the window's, and the result's series is the
    // extender's
    assert.equal(record.uploadReports.at(-1)!.ms, 1000);
    assert.equal(result.upload.bytes, record.uploadReports.at(-1)!.bytes);
    assert.deepEqual(result.upload.intervalBytes, record.uploadBytes.slice(0, 4));
    session.close();
  });
});

test("on a slow uplink, streams end on a short message of the room left, which the extender verifies, and send nothing after it", { timeout: 30_000 }, async () => {
  // 128 KiB/s a stream, about 4 Mbps in all, so the 1 MiB the streams may
  // send before the first report takes 2 s of the 3 s window, and an
  // extender of amendment 15 that says its window is over 600 ms after its
  // end: near the end, room for a whole message runs out, and what is left
  // goes as short, last messages (M6, S5)
  await withMock({ uploadBytesPerSecond: 128 << 10, hello: { uploadMs: 3000 }, uploadFinal: true, uploadWindowOverDelayMs: 600 }, async (mock) => {
    const { session } = open(mock, "ticket-short");
    await session.ready;
    const result = await session.startLoad();
    const record = mock.sessions[0];
    assert.equal(result.uploadVerified, true);
    assert.equal(result.endReason, "complete");
    assert.equal(record.mismatch, false);
    // at least one stream ended on a short message, each at most once, and
    // the extender checked it against the keystream like any other
    assert.ok(0 < record.uploadShortMessages.length, "no stream sent a short message");
    const streams = record.uploadShortMessages.map((message) => message.stream);
    assert.equal(new Set(streams).size, streams.length, `a stream sent two short messages: ${JSON.stringify(record.uploadShortMessages)}`);
    assert.ok(record.uploadShortMessages.every((message) => 16 * 1024 <= message.bytes && message.bytes < 65536));
    // and no stream sent a message after its short one
    assert.deepEqual(record.oddUploadMessages, []);
    session.close();
  });
});

test("the upload keeps bufferedAmount under its cap on a slow uplink", { timeout: 30_000 }, async () => {
  await withMock({ uploadBytesPerSecond: 2 << 20 }, async (mock) => {
    let largest = 0;
    let uncounted = 0;
    class MeasuredWebSocket extends WebSocket {
      send(data: string | ArrayBufferLike | Blob | ArrayBufferView) {
        if (typeof data === "string") {
          super.send(data);
          return;
        }
        largest = Math.max(largest, this.bufferedAmount);
        super.send(data);
        // a socket counts a message as soon as it is sent
        if (this.bufferedAmount < (data as ArrayBufferView).byteLength) {
          uncounted += 1;
        }
      }
    }
    const { session } = open(mock, "ticket-slow", { WebSocket: MeasuredWebSocket });
    await session.ready;
    const result = await session.startLoad();
    assert.equal(result.uploadVerified, true);
    assert.ok(0 < largest, "the upload buffered");
    // a stream holds no more than its share of the backlog in the page
    assert.ok(largest + 65536 <= 8 << 20, `bufferedAmount reached ${largest}`);
    assert.equal(uncounted, 0);
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

test("a load's streams keep the host's port, and a bufferedAmount that never counts cannot run away", { timeout: 30_000 }, async () => {
  const host = `${EXTENDER_ID}.extender.localhost:8443`;
  const hello = {
    type: "hello",
    session: randomBytes(16).toString("base64url"),
    uploadKey: randomBytes(16).toString("base64url"),
    pingIntervalMs: 250,
    intervalMs: 50,
    downloadMs: 100,
    uploadMs: 200,
    maxStreams: 2,
  };
  const socketUrls: string[] = [];
  const fetchUrls: string[] = [];
  let uploadedBytes = 0;

  // A WebSocket that goes nowhere: the control socket says hello and answers
  // done with a loadResult; upload sockets swallow what they are sent, and
  // bufferedAmount stays 0 whatever was sent, so it gives no backpressure.
  type Listener = (event: Record<string, unknown>) => void;
  class FakeWebSocket {
    binaryType = "blob";
    readyState = 0;
    bufferedAmount = 0;
    private readonly listeners = new Map<string, Set<Listener>>();
    private readonly control: boolean;
    constructor(url: string) {
      socketUrls.push(url);
      this.control = url.includes("/lag/v1/session?");
      setTimeout(() => {
        this.readyState = 1;
        this.dispatch("open", {});
        if (this.control) {
          this.dispatch("message", { data: JSON.stringify(hello) });
        }
      }, 1);
    }
    addEventListener(type: string, listener: Listener) {
      if (!this.listeners.has(type)) {
        this.listeners.set(type, new Set());
      }
      this.listeners.get(type)!.add(listener);
    }
    removeEventListener(type: string, listener: Listener) {
      this.listeners.get(type)?.delete(listener);
    }
    send(data: string | Uint8Array) {
      if (typeof data !== "string") {
        uploadedBytes += data.byteLength;
        return;
      }
      const message = JSON.parse(data);
      if (message.type === "load" && message.phase === "done") {
        setTimeout(() => {
          this.dispatch("message", {
            data: JSON.stringify({ type: "loadResult", uploadVerified: true, endReason: "complete" }),
          });
        }, 1);
      }
    }
    close(code = 1000) {
      if (this.readyState === 3) {
        return;
      }
      this.readyState = 3;
      setTimeout(() => this.dispatch("close", { code, reason: "" }), 1);
    }
    private dispatch(type: string, event: Record<string, unknown>) {
      for (const listener of [...(this.listeners.get(type) ?? [])]) {
        listener(event);
      }
    }
  }
  // a download stream of five chunks, all at once, then its end
  const fakeFetch = async (input: string | URL | Request) => {
    fetchUrls.push(String(input));
    const body = new ReadableStream<Uint8Array>({
      start(controller) {
        for (let i = 0; i < 5; i += 1) {
          controller.enqueue(new Uint8Array(1000));
        }
        controller.close();
      },
    });
    return new Response(body, { status: 200 });
  };

  const session = openLagSession({
    host,
    ticket: "ticket-port",
    WebSocket: FakeWebSocket as unknown as typeof WebSocket,
    fetch: fakeFetch as typeof fetch,
  });
  await session.ready;
  const started = performance.now();
  const result = await session.startLoad();
  session.close();

  const query = (k: number) => `session=${hello.session}&stream=${k}`;
  assert.deepEqual(fetchUrls.sort(), [0, 1].map((k) => `https://${host}/lag/v1/down?${query(k)}`));
  assert.deepEqual(socketUrls.sort(), [
    `wss://${host}/lag/v1/session?ticket=ticket-port`,
    ...[0, 1].map((k) => `wss://${host}/lag/v1/up?${query(k)}`),
  ]);
  assert.equal(result.download.bytes, 2 * 5 * 1000);
  assert.equal(result.endReason, "complete");
  // the upload ended on its 200 ms window: its timer ran, though nothing
  // ever held a stream back
  assert.ok(performance.now() - started < 5000, `${performance.now() - started} ms`);
  // and with no report from the extender, the streams sent no more than
  // they may before its first (sixteen messages)
  assert.ok(0 < uploadedBytes && uploadedBytes <= 16 * 65536, `${uploadedBytes} bytes sent`);
});
