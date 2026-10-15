// The upload's short, last messages (lag.dev/PLAN.md M6, S5). Near the
// window's end, flow control's room shrinks below a whole 64 KiB message
// while some of it is still left, so on a slow path the streams would stop
// sending up to a second before the extender's window ends and leave its
// last intervals short. A stream may then send one shorter message of the
// room left. M6 lets only a stream's last message be shorter, and the
// extender ends the load as a mismatch on any message after a short one, so
// that stream sends nothing more in the load.

import { LAG_UPLOAD_MESSAGE_BYTES } from "./keystream";

/** The least room left worth a short message: one TLS record. */
export const LAG_UPLOAD_SHORT_MIN_BYTES = 16 * 1024;

/** What the upload's streams look like as a message is due. */
export interface LagUploadEndState {
  /** what flow control lets the streams hold unverified, less what they hold: bytes */
  roomLeft: number;
  /** until the extender's window ends, as its reports place it: ms, 0 once past */
  timeLeftMs: number;
  /** the extender's counting rate over its last second of reports: bytes per second */
  steadyBytesPerSecond: number;
  /** the session's lowest round trip: ms */
  baseRttMs: number;
  /** each live stream: what its socket still buffers, and whether it has sent its last message */
  streams: ReadonlyArray<{ bufferedAmount: number; finished: boolean }>;
}

/**
 * The stream that sends a short, last message, and its length, or undefined
 * when none should. One does only when a whole message no longer fits the
 * room left, at least LAG_UPLOAD_SHORT_MIN_BYTES of it is, and the window is
 * in its end phase: a whole message could no longer be counted by its end,
 * at the steady rate and a round trip later. A room that is small for a
 * moment earlier in the window never ends a stream. The message takes all
 * the room left, on the stream with the least buffered of those that have
 * not sent their last.
 */
export function lagUploadShortMessage(state: LagUploadEndState): { stream: number; bytes: number } | undefined {
  const { roomLeft, timeLeftMs, steadyBytesPerSecond: steady, baseRttMs, streams } = state;
  if (!(LAG_UPLOAD_SHORT_MIN_BYTES <= roomLeft && roomLeft < LAG_UPLOAD_MESSAGE_BYTES)) {
    return undefined;
  }
  if (!(0 < steady && timeLeftMs < (LAG_UPLOAD_MESSAGE_BYTES * 1000) / steady + baseRttMs)) {
    return undefined;
  }
  let chosen: number | undefined;
  streams.forEach((stream, k) => {
    if (!stream.finished && (chosen === undefined || stream.bufferedAmount < streams[chosen].bufferedAmount)) {
      chosen = k;
    }
  });
  return chosen === undefined ? undefined : { stream: chosen, bytes: Math.floor(roomLeft) };
}
