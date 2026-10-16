import { test } from "node:test";
import assert from "node:assert/strict";
import { registerTsResolve } from "../ts-resolve.ts";

registerTsResolve();
const { lagUploadStartBytes, lagUploadFirstRate, LAG_UPLOAD_START_MAX_BYTES } = await import("../../src/lag/uploadStart.ts");
const { LAG_UPLOAD_MESSAGE_BYTES } = await import("../../src/lag/keystream.ts");

// The upload's start (lag.dev/PLAN.md S5): what its streams may send before
// the extender's first report, sized from the download the session has just
// run, and how that first report's rate is read.

const bytesPerSecond = (mbps: number) => (mbps * 1e6) / 8;
const start = (downloadMbps: number, baseRttMs: number, streams = 4, intervalMs = 250) =>
  lagUploadStartBytes({ downloadBytesPerSecond: bytesPerSecond(downloadMbps), baseRttMs, intervalMs, streams });

test("the start is the download's rate for the lowest round trip and two intervals, between a message a stream and 1 MiB", () => {
  assert.equal(LAG_UPLOAD_START_MAX_BYTES, 16 * LAG_UPLOAD_MESSAGE_BYTES);
  // 4 Mbps for 50 + 500 ms, and 8 Mbps for 100 + 500 ms
  assert.equal(start(4, 50), 275_000);
  assert.equal(start(8, 100), 600_000);
  // the interval is the hello's
  assert.equal(start(4, 50, 4, 500), 525_000);
  // a slow download gives a whole message a stream, so every stream starts
  assert.equal(start(0.5, 20), 4 * LAG_UPLOAD_MESSAGE_BYTES);
  assert.equal(start(2, 250), 4 * LAG_UPLOAD_MESSAGE_BYTES);
  assert.equal(start(1, 50, 2), 2 * LAG_UPLOAD_MESSAGE_BYTES);
  assert.equal(start(1, 50, 3), 3 * LAG_UPLOAD_MESSAGE_BYTES);
  // a fast one the 1 MiB of before, and never more
  assert.equal(start(20, 20), LAG_UPLOAD_START_MAX_BYTES);
  assert.equal(start(50, 250), LAG_UPLOAD_START_MAX_BYTES);
  assert.equal(start(10_000, 250), LAG_UPLOAD_START_MAX_BYTES);
  // a message a stream wins over the cap, so more than 16 streams each start
  assert.equal(start(50, 250, 20), 20 * LAG_UPLOAD_MESSAGE_BYTES);
  // it grows with the round trip and with the download's rate
  assert.ok(start(4, 20) < start(4, 100) && start(4, 100) < start(4, 250));
  assert.ok(start(3, 50) < start(4, 50) && start(4, 50) < start(6, 50));
});

test("a start sized from the download fits a slow link's window and covers a link's first report as fast as the download", () => {
  // over the table's links, the upload as fast as the download: the start
  // lasts until the first report comes, a round trip and an interval after
  // the streams start, or is the 1 MiB of before; and on a link of 1.5 Mbps
  // or less it is less than the 5 s window counts, where 1 MiB was 1.1 to
  // 3.4 times as much
  for (const mbps of [0.5, 1, 1.5, 2, 4, 8, 20, 50]) {
    for (const rttMs of [20, 50, 100, 250]) {
      const bytes = start(mbps, rttMs);
      const window = bytesPerSecond(mbps) * 5;
      assert.ok(Math.min(LAG_UPLOAD_START_MAX_BYTES, (bytesPerSecond(mbps) * (rttMs + 250)) / 1000) <= bytes, `${mbps} Mbps, ${rttMs} ms: ${bytes}`);
      if (mbps <= 1.5) {
        assert.ok(bytes < window, `${mbps} Mbps, ${rttMs} ms: a start of ${bytes} for a window of ${window}`);
      }
    }
  }
});

test("without a download's rate or round trip to go by, the start is a message a stream", () => {
  for (const downloadBytesPerSecond of [0, -1, Number.NaN, Number.POSITIVE_INFINITY]) {
    assert.equal(lagUploadStartBytes({ downloadBytesPerSecond, baseRttMs: 50, intervalMs: 250, streams: 4 }), 4 * LAG_UPLOAD_MESSAGE_BYTES);
  }
  // a round trip it can't read counts as none
  assert.equal(lagUploadStartBytes({ downloadBytesPerSecond: 1_000_000, baseRttMs: Number.NaN, intervalMs: 250, streams: 4 }), 500_000);
  // and no streams as one
  assert.equal(lagUploadStartBytes({ downloadBytesPerSecond: 0, baseRttMs: 50, intervalMs: 250, streams: 0 }), LAG_UPLOAD_MESSAGE_BYTES);
});

test("the first report's rate is its bytes over the time they took to arrive, once the start has drained", () => {
  // a 50 Mbps link drained a 256 KiB start in 42 ms, and R2 left 40 KiB of
  // the four streams' first records out: over the report's 250 ms it would
  // read as 7 Mbps
  const start = { bytes: 4 * LAG_UPLOAD_MESSAGE_BYTES, streams: 4 };
  assert.equal(lagUploadFirstRate({ ms: 250, bytes: 221_184, at: 42 }, start), (221_184 * 1000) / 42);
  // all but a record a stream counted is drained; any less is not, so the
  // report's interval stands
  assert.equal(lagUploadFirstRate({ ms: 250, bytes: 196_608, at: 42 }, start), (196_608 * 1000) / 42);
  assert.equal(lagUploadFirstRate({ ms: 250, bytes: 196_607, at: 42 }, start), (196_607 * 1000) / 250);
  // a start still draining at the report: an extender that counted a whole
  // message a stream at once, as one without R2 does, while a 1 MiB start
  // queued behind them, says nothing of how fast the link is
  assert.equal(lagUploadFirstRate({ ms: 250, bytes: 262_144, at: 2 }, { bytes: 1 << 20, streams: 4 }), (262_144 * 1000) / 250);
  // a link busy to the report's time, or an extender that sends no `at`
  // and gives its ms in its place
  assert.equal(lagUploadFirstRate({ ms: 250, bytes: 250_000, at: 250 }, start), 1_000_000);
  // nothing counted, or nothing to go by
  assert.equal(lagUploadFirstRate({ ms: 250, bytes: 0, at: 0 }, start), 0);
  assert.equal(lagUploadFirstRate({ ms: 0, bytes: 221_184, at: 0 }, start), 0);
  assert.equal(lagUploadFirstRate({ ms: 250, bytes: 221_184, at: 0 }, start), (221_184 * 1000) / 250);
  assert.equal(lagUploadFirstRate({ ms: 250, bytes: Number.NaN, at: 10 }, start), 0);
  // never slower than over its interval
  for (const at of [1, 10, 100, 249, 250, 300]) {
    assert.ok((221_184 * 1000) / 250 <= lagUploadFirstRate({ ms: 250, bytes: 221_184, at }, start), `at ${at}`);
  }
});
