// Loaded latency and RPM use the actual measured directions, independently of
// connection setup, failed uploads and result-settlement waits. No network runs.
import { test } from "node:test";
import assert from "node:assert/strict";
import { registerTsResolve } from "../../test/ts-resolve.ts";
import type { MeasureContext } from "../src/measure.ts";
import type { LagHello, LagLoadResult, LagSessionEvent } from "../../src/lag/client.ts";

registerTsResolve();
const { measureBufferbloat } = await import("../src/measure.ts");
const { LagSessionError } = await import("../../src/lag/client.ts");
const { LagPingWindow } = await import("../../src/lag/stats.ts");

const T0 = Date.parse("2026-10-12T12:00:00Z");

// The session advances one controllable clock and emits exact RTT/phase events
// through MeasureContext, the same entry point used by CLI and MCP runs.
const measureWindow = async (
  clock: { now: number },
  options: { gapMs?: number; gapPings?: boolean; failUpload?: boolean; resultWaitMs?: number; phaseStartsOnly?: boolean } = {},
) => {
  clock.now = T0;
  const hello: LagHello = {
    session: "synthetic-session", uploadKey: "synthetic-key", pingIntervalMs: 250,
    intervalMs: 250, downloadMs: 4000, uploadMs: 1000, maxStreams: 1,
  };
  let closed = false;
  let onEvent!: (event: LagSessionEvent) => void;
  const ping = (at: number, ms: number) => onEvent({ type: "rtt", at, t: at - T0, ms, lost: false });
  const context: MeasureContext = {
    apiOrigin: "https://api.measurement.example",
    guest: { token: async () => "synthetic-guest", invalidate: async () => undefined },
    api: {
      myIpInfo: async () => ({ info: { ip: "192.0.2.1" } }),
      lagExtenders: async () => ({ extenders: [{
        extender_id: "synthetic-extender", public_key: "00", host: "latency.example", ips: [],
      }] }),
      lagTrack: async () => ({ tickets: [{
        extender_id: "synthetic-extender", host: "latency.example", position: 0, ticket: "synthetic-ticket",
      }] }),
    },
    openSession: (optionsForSession) => {
      onEvent = optionsForSession.onEvent;
      ping(clock.now, 10);
      clock.now += 1000;
      const closedEvent: Extract<LagSessionEvent, { type: "closed" }> = {
        type: "closed", code: 1000, reason: "", byClient: true, at: clock.now,
      };
      return {
        host: optionsForSession.host,
        get state() { return closed ? "closed" : "open"; },
        hello, openedAt: T0, pings: new LagPingWindow(),
        ready: Promise.resolve(hello), closed: Promise.resolve(closedEvent),
        close: () => { closed = true; },
        startLoad: async () => {
          const downloadAt = clock.now;
          const downloadEnd = downloadAt + 1000;
          const uploadAt = downloadEnd + (options.gapMs ?? 0);
          onEvent({ type: "load", phase: "download", at: downloadAt });
          for (let at = downloadAt; at < downloadEnd; at += 250) ping(at, 100);
          if (options.gapPings) {
            for (let at = downloadEnd; at < uploadAt; at += 250) ping(at, 10);
          }
          const result: LagLoadResult = {
            intervalMs: 250,
            download: {
              intervalBytes: [250, 250, 250, 250], mbps: [0.008, 0.008, 0.008, 0.008],
              percentiles: { p50: 0.008, p95: 0.008 }, bytes: 1000, streams: 1,
              startedAt: options.phaseStartsOnly ? undefined : downloadAt, durationMs: 1000,
            },
            upload: { intervalBytes: [], mbps: [], bytes: 0, streams: 0, durationMs: 0 },
            uploadVerified: false, endReason: options.failUpload ? null : "complete",
          };
          clock.now = uploadAt;
          if (!options.failUpload) {
            onEvent({ type: "load", phase: "upload", at: uploadAt });
            const uploadEnd = uploadAt + 1000;
            for (let at = uploadAt; at < uploadEnd; at += 250) ping(at, 200);
            result.upload = {
              intervalBytes: [500, 500, 500, 500], mbps: [0.016, 0.016, 0.016, 0.016],
              percentiles: { p50: 0.016, p95: 0.016 }, bytes: 2000, streams: 1,
              startedAt: options.phaseStartsOnly ? undefined : uploadAt, durationMs: 1000,
            };
            result.uploadVerified = true;
            clock.now = uploadEnd + (options.resultWaitMs ?? 0);
            for (let at = uploadEnd; at < clock.now; at += 250) ping(at, 10);
          }
          onEvent({ type: "load", phase: "done", at: clock.now });
          if (options.failUpload) throw new LagSessionError("upload", "synthetic upload setup failure", { partial: result });
          return result;
        },
      };
    },
  };
  const result = await measureBufferbloat(context, { maxBytes: 1_000_000, maxMs: 60_000, servers: 1, idleMs: 0, settleMs: 0 });
  assert.equal(closed, true);
  assert.equal(result.stoppedEarly, null);
  assert.deepEqual(result.servers[0].idle, { p50Ms: 10, p95Ms: 10 });
  return result;
};

test("download loaded latency and RPM exclude upload setup gap pings", async (context) => {
  const clock = { now: T0 };
  context.mock.method(Date, "now", () => clock.now);
  context.mock.timers.enable({ apis: ["setTimeout", "setInterval"] });
  const ordinary = await measureWindow(clock);
  const quietGap = await measureWindow(clock, { gapMs: 3000 });
  const pingedGap = await measureWindow(clock, { gapMs: 3000, gapPings: true });
  for (const result of [ordinary, quietGap, pingedGap]) {
    const load = result.servers[0].load!;
    assert.equal(load.status, "complete");
    assert.equal(load.download!.latencyP50Ms, 100);
    assert.equal(load.download!.increaseMs, 90);
    assert.equal(load.upload!.latencyP50Ms, 200);
    assert.equal(load.upload!.increaseMs, 190);
    assert.deepEqual(load.rpm, { value: 600, band: "fair" });
    assert.deepEqual(result.overall.loadedRpm, { value: 600, band: "fair" });
    assert.deepEqual(result.overall.loadedIncreaseMs, { download: 90, upload: 190 });
    assert.deepEqual(result.bytes, { download: 1000, upload: 2000, total: 3000, maxBytes: 1_000_000 });
  }
  assert.deepEqual(pingedGap.servers[0].load, ordinary.servers[0].load);
});

test("a failed upload's setup timeout cannot dilute the partial download's latency or RPM", async (context) => {
  const clock = { now: T0 };
  context.mock.method(Date, "now", () => clock.now);
  context.mock.timers.enable({ apis: ["setTimeout", "setInterval"] });
  const result = await measureWindow(clock, { gapMs: 3000, gapPings: true, failUpload: true });
  const load = result.servers[0].load!;
  assert.equal(load.status, "partial");
  assert.equal(load.reason, "upload");
  assert.equal(load.download!.latencyP50Ms, 100);
  assert.equal(load.download!.increaseMs, 90);
  assert.equal(load.upload!.latencyP50Ms, null);
  assert.equal(load.upload!.increaseMs, null);
  assert.deepEqual(load.rpm, { value: 600, band: "fair" });
  assert.deepEqual(result.overall.loadedRpm, { value: 600, band: "fair" });
});

test("upload loaded metrics exclude the wait after its measured window and accept phase start times", async (context) => {
  const clock = { now: T0 };
  context.mock.method(Date, "now", () => clock.now);
  context.mock.timers.enable({ apis: ["setTimeout", "setInterval"] });
  const result = await measureWindow(clock, { resultWaitMs: 3000, phaseStartsOnly: true });
  const load = result.servers[0].load!;
  assert.equal(load.download!.latencyP50Ms, 100);
  assert.equal(load.upload!.latencyP50Ms, 200);
  assert.equal(load.upload!.increaseMs, 190);
  assert.deepEqual(load.rpm, { value: 600, band: "fair" });
  assert.deepEqual(result.overall.loadedRpm, { value: 600, band: "fair" });
});
