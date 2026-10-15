import { test } from "node:test";
import assert from "node:assert/strict";
import { registerTsResolve } from "../ts-resolve.ts";

registerTsResolve();
const { lagUploadShortMessage, LAG_UPLOAD_SHORT_MIN_BYTES } = await import("../../src/lag/uploadEnd.ts");
const { LAG_UPLOAD_MESSAGE_BYTES } = await import("../../src/lag/keystream.ts");

// The upload's short, last message (lag.dev/PLAN.md M6, S5): when room for a
// whole message has run out near the window's end, the room left goes as one
// stream's last message, so a slow path's streams don't stop sending up to a
// second before the extender's window ends.

// 2 Mbps of counting, a 50 ms round trip: a whole message takes 262 ms to
// count, so the end phase is the last 312 ms
const steady = 250_000;
const endPhaseMs = (LAG_UPLOAD_MESSAGE_BYTES * 1000) / steady + 50;
const open = (bufferedAmount = 0) => ({ bufferedAmount, finished: false });

test("a short message takes the room left once no whole message fits, in the window's end phase", () => {
  assert.equal(LAG_UPLOAD_SHORT_MIN_BYTES, 16 * 1024);
  const state = { roomLeft: 40_000.7, timeLeftMs: 200, steadyBytesPerSecond: steady, baseRttMs: 50, streams: [open(), open(), open(), open()] };
  assert.deepEqual(lagUploadShortMessage(state), { stream: 0, bytes: 40_000 });
  // and past the window's estimated end
  assert.deepEqual(lagUploadShortMessage({ ...state, timeLeftMs: 0 }), { stream: 0, bytes: 40_000 });
  // up to a byte short of a whole message, and from one TLS record
  assert.deepEqual(lagUploadShortMessage({ ...state, roomLeft: LAG_UPLOAD_MESSAGE_BYTES - 1 }), { stream: 0, bytes: LAG_UPLOAD_MESSAGE_BYTES - 1 });
  assert.deepEqual(lagUploadShortMessage({ ...state, roomLeft: LAG_UPLOAD_SHORT_MIN_BYTES }), { stream: 0, bytes: LAG_UPLOAD_SHORT_MIN_BYTES });
});

test("no short message while a whole one fits, or with less than a TLS record of room", () => {
  const state = { roomLeft: 40_000, timeLeftMs: 100, steadyBytesPerSecond: steady, baseRttMs: 50, streams: [open(), open()] };
  assert.equal(lagUploadShortMessage({ ...state, roomLeft: LAG_UPLOAD_MESSAGE_BYTES }), undefined);
  assert.equal(lagUploadShortMessage({ ...state, roomLeft: 1 << 20 }), undefined);
  assert.equal(lagUploadShortMessage({ ...state, roomLeft: LAG_UPLOAD_SHORT_MIN_BYTES - 1 }), undefined);
  assert.equal(lagUploadShortMessage({ ...state, roomLeft: 0 }), undefined);
  assert.equal(lagUploadShortMessage({ ...state, roomLeft: -5000 }), undefined);
});

test("no stream finishes before the end phase: a room that is small for a moment earlier in the window sends nothing", () => {
  const state = { roomLeft: 40_000, steadyBytesPerSecond: steady, baseRttMs: 50, streams: [open(), open(), open(), open()] };
  for (const timeLeftMs of [endPhaseMs, endPhaseMs + 1, 1000, 2500, 4800]) {
    assert.equal(lagUploadShortMessage({ ...state, timeLeftMs }), undefined, `${timeLeftMs} ms before the end`);
  }
  assert.notEqual(lagUploadShortMessage({ ...state, timeLeftMs: endPhaseMs - 1 }), undefined);
  // the phase scales with the rate: at 20 Mbps a whole message counts in
  // 26 ms, so 100 ms before the end is not yet the end phase
  assert.equal(lagUploadShortMessage({ ...state, steadyBytesPerSecond: 2_500_000, timeLeftMs: 100 }), undefined);
  assert.notEqual(lagUploadShortMessage({ ...state, steadyBytesPerSecond: 2_500_000, timeLeftMs: 60 }), undefined);
  // no rate to judge by, no short message
  assert.equal(lagUploadShortMessage({ ...state, steadyBytesPerSecond: 0, timeLeftMs: 0 }), undefined);
});

test("the short message goes on the stream with the least buffered that has not sent its last", () => {
  const state = { roomLeft: 30_000, timeLeftMs: 50, steadyBytesPerSecond: steady, baseRttMs: 50 };
  assert.deepEqual(lagUploadShortMessage({ ...state, streams: [open(9000), open(200), open(4000)] })?.stream, 1);
  // a stream that has sent its last never sends again, however little it buffers
  const finished = { bufferedAmount: 0, finished: true };
  assert.deepEqual(lagUploadShortMessage({ ...state, streams: [open(9000), finished, open(4000)] })?.stream, 2);
  assert.equal(lagUploadShortMessage({ ...state, streams: [finished, finished] }), undefined);
  assert.equal(lagUploadShortMessage({ ...state, streams: [] }), undefined);
});
