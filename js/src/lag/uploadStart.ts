// The upload's start (lag.dev/PLAN.md S5): what its streams may send before
// the extender's first report, and how that report's rate is read.
//
// Until the first report nothing says how fast the uplink is, and whatever
// the streams hand their sockets by then goes out whether the window can
// count it or not: what a socket has been given can't be taken back. The
// start was 1 MiB, which fills the window of a 1.7 Mbps uplink. A slower
// one drained the rest after its window had ended, until the extender
// closed the connection 2 s later: on the local stack the bytes leaving the
// browser were 1.67, 1.54 and 1.13 times those counted at 0.5, 1 and
// 1.5 Mbps.
//
// So the start covers the first report's round trip, and an interval more,
// at the rate the session's download has just run. The report comes about a
// round trip and an interval after the streams start, so an uplink no faster
// than its download, as most are, sends until that report as it did with
// the larger start, and a slow one sends little more than its window can
// count. The download's rate says nothing more about the uplink: one far
// slower than its download still gets a start sized for the download, as
// before, and one faster drains its start before the first report, whose
// rate is then read over the time its bytes took to arrive.
//
// The start is at least one whole message a stream, so every stream starts
// at once (S5), and at most 1 MiB, the start before, so no uplink sends more
// before the first report than it did.

import { LAG_UPLOAD_MESSAGE_BYTES } from "./keystream";

/** The most the streams may send before the first report: 16 messages, 1 MiB. */
export const LAG_UPLOAD_START_MAX_BYTES = 16 * LAG_UPLOAD_MESSAGE_BYTES;

/** What the session knows as its upload starts. */
export interface LagUploadStartState {
  /** the session's download, the p50 of its intervals: bytes per second */
  downloadBytesPerSecond: number;
  /** the session's lowest round trip: ms */
  baseRttMs: number;
  /** the extender's report interval, the hello's intervalMs: ms */
  intervalMs: number;
  /** the upload's streams that opened */
  streams: number;
}

const atLeastZero = (value: number) => (Number.isFinite(value) && 0 < value ? value : 0);

/**
 * What the streams may send before the extender's first report, in bytes:
 * the download's rate for the lowest round trip and two intervals, at least
 * one whole message a stream and at most LAG_UPLOAD_START_MAX_BYTES.
 */
export function lagUploadStartBytes(state: LagUploadStartState): number {
  const streams = Math.max(1, Math.floor(atLeastZero(state.streams)));
  const coverMs = atLeastZero(state.baseRttMs) + 2 * atLeastZero(state.intervalMs);
  const cover = (atLeastZero(state.downloadBytesPerSecond) * coverMs) / 1000;
  return Math.max(streams * LAG_UPLOAD_MESSAGE_BYTES, Math.min(LAG_UPLOAD_START_MAX_BYTES, cover));
}

// A TLS record: what a read brings the extender, and the most of each
// stream's first group that R2 can leave out of the count (M6)
const RECORD_BYTES = 16 * 1024;

/**
 * The rate the extender's first `uploaded` report shows, bytes per second:
 * its bytes over its interval (`ms`), or over the time they took to arrive
 * (`at`, the latest arrival counted) when the streams' start had drained
 * before it: the report counted all they had sent by then but a record a
 * stream, which R2 may leave out (M6). An uplink faster than its download
 * drains its start well before the first report, and over the report's
 * interval that reads as the start's rate rather than the link's. Every
 * byte the report counts arrived after the window's first byte, so over
 * `at` they are never more than the link carried them at. An extender that
 * sends no `at` gives `ms` in its place.
 */
export function lagUploadFirstRate(
  report: { ms: number; bytes: number; at: number },
  sent: { bytes: number; streams: number },
): number {
  const bytes = atLeastZero(report.bytes);
  const ms = atLeastZero(report.ms);
  let rate = ms === 0 ? 0 : (bytes * 1000) / ms;
  const at = atLeastZero(report.at);
  const drained = 0 < bytes && atLeastZero(sent.bytes) - atLeastZero(sent.streams) * RECORD_BYTES <= bytes;
  if (drained && 0 < at && at < ms) {
    rate = Math.max(rate, (bytes * 1000) / at);
  }
  return rate;
}
