import { test } from "node:test";
import assert from "node:assert/strict";
import { randomBytes } from "node:crypto";
import { registerTsResolve } from "../ts-resolve.ts";
import { startMockExtender, type MockHello } from "./client_mock_extender.ts";

// A drift test's tickets are for latency alone (lag.dev/DRIFTTEST.md E3, E4):
// DownloadMs, UploadMs, MaxStreams and both byte caps are zero, and they may
// carry ReportPromptly (15) and NoReport (16). The extender admits no load on
// such a ticket, and its hello still names the default load, 5 s each way
// over 4 streams (connect 76acc8a1). The session holds a hello to its ticket
// as the extender reads it (PLAN.md S5), where a zero cap is unset and takes
// that default, so the hello passes and the session pings, while a hello
// asking for more than the defaults is still refused.

registerTsResolve();
const { openLagSession, LagSessionError } = await import("../../src/lag/client.ts");
const { decodeLagTicket, decodeLagTicketBody } = await import("../../src/lag/protobuf.ts");

interface Caps {
  downloadMs?: number;
  uploadMs?: number;
  maxStreams?: number;
  maxDownloadBytes?: number;
  maxUploadBytes?: number;
  reportPromptly?: boolean;
  noReport?: boolean;
}

// A LagTicket's text form (M0) with the given body fields, and a signature
// nobody checks here: the extender does.
const ticketText = (caps: Caps) => {
  const varint = (out: number[], value: number) => {
    while (0x80 <= value) {
      out.push((value % 0x80) | 0x80);
      value = Math.floor(value / 0x80);
    }
    out.push(value);
  };
  const bytesField = (out: number[], number: number, bytes: Iterable<number>) => {
    const value = [...bytes];
    varint(out, (number << 3) | 2);
    varint(out, value.length);
    out.push(...value);
  };
  const numberField = (out: number[], number: number, value: number | undefined) => {
    if (value) {
      varint(out, number << 3);
      varint(out, value);
    }
  };
  const body: number[] = [];
  bytesField(body, 1, randomBytes(16));
  numberField(body, 9, 250);
  numberField(body, 10, caps.downloadMs);
  numberField(body, 11, caps.uploadMs);
  numberField(body, 12, caps.maxStreams);
  numberField(body, 13, caps.maxDownloadBytes);
  numberField(body, 14, caps.maxUploadBytes);
  numberField(body, 15, caps.reportPromptly ? 1 : 0);
  numberField(body, 16, caps.noReport ? 1 : 0);
  const ticket: number[] = [];
  bytesField(ticket, 1, body);
  bytesField(ticket, 2, randomBytes(64));
  bytesField(ticket, 3, randomBytes(8));
  return Buffer.from(ticket).toString("base64url");
};

// what the extender's hello names on a latency-only ticket
const DEFAULT_LOAD: Partial<MockHello> = { downloadMs: 5000, uploadMs: 5000, maxStreams: 4 };

const openWith = async (hello: Partial<MockHello>, ticket: string, pongs = 0) => {
  const mock = await startMockExtender({ hello });
  try {
    const rtts: number[] = [];
    let gotRtts: () => void = () => {};
    const enough = new Promise<void>((resolve) => {
      gotRtts = resolve;
    });
    const session = openLagSession({
      host: mock.origin,
      ticket,
      onEvent: (event) => {
        if (event.type === "rtt" && typeof (event as { ms?: unknown }).ms === "number") {
          rtts.push((event as { ms: number }).ms);
          if (pongs <= rtts.length) {
            gotRtts();
          }
        }
      },
    });
    const outcome = await session.ready.then(
      (taken) => ({ hello: taken, error: undefined as unknown }),
      (error: unknown) => ({ hello: undefined, error }),
    );
    if (outcome.hello !== undefined && 0 < pongs) {
      await Promise.race([enough, new Promise((resolve) => setTimeout(resolve, 5000))]);
    }
    if (outcome.hello !== undefined) {
      session.close();
    }
    await session.closed;
    return { ...outcome, rtts, record: mock.sessions[0] };
  } finally {
    await mock.close();
  }
};

test("E3: a latency-only drift ticket reads as every cap unset", () => {
  const body = decodeLagTicketBody(decodeLagTicket(ticketText({ reportPromptly: true, noReport: true })).body);
  assert.equal(body.ticketId.length, 16);
  assert.deepEqual(
    [body.downloadMs, body.uploadMs, body.maxStreams, body.maxDownloadBytes, body.maxUploadBytes],
    [0, 0, 0, 0, 0],
  );
});

test("E3: a latency-only ticket with the extender's default hello opens and pings", { timeout: 30_000 }, async () => {
  for (const ticket of [ticketText({}), ticketText({ reportPromptly: true }), ticketText({ reportPromptly: true, noReport: true })]) {
    const outcome = await openWith(DEFAULT_LOAD, ticket, 3);
    assert.equal(outcome.error, undefined, String(outcome.error));
    assert.equal(outcome.hello?.downloadMs, 5000);
    assert.equal(outcome.hello?.maxStreams, 4);
    assert.ok(outcome.rtts.length >= 3, `only ${outcome.rtts.length} round trips`);
    assert.equal(outcome.record.loadRequests, 0, "no load is asked for");
  }
});

test("E3: on a latency-only ticket a hello asking for more than the defaults is still refused", { timeout: 30_000 }, async () => {
  const outcome = await openWith({ downloadMs: 8000, uploadMs: 5000, maxStreams: 16 }, ticketText({ reportPromptly: true }));
  assert.ok(outcome.error instanceof LagSessionError, "the session opened");
  assert.equal(outcome.error.code, "protocol");
  assert.match(outcome.error.message, /downloadMs 8000, the ticket allows 5000; maxStreams 16, the ticket allows 4/);
});

test("E3: an ordinary ticket still fails on a hello larger than its caps", { timeout: 30_000 }, async () => {
  const ordinary = ticketText({ downloadMs: 2000, uploadMs: 1000, maxStreams: 2 });
  const outcome = await openWith(DEFAULT_LOAD, ordinary);
  assert.ok(outcome.error instanceof LagSessionError, "the session opened");
  assert.equal(outcome.error.code, "protocol");
  assert.match(outcome.error.message, /downloadMs 5000, the ticket allows 2000; uploadMs 5000, the ticket allows 1000; maxStreams 4, the ticket allows 2/);
  assert.equal(outcome.record.loadRequests, 0);
});
