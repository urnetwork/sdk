import { test } from "node:test";
import assert from "node:assert/strict";
import { randomBytes } from "node:crypto";
import { readFileSync } from "node:fs";
import { registerTsResolve } from "../ts-resolve.ts";
import { startMockExtender, type MockHello } from "./client_mock_extender.ts";

registerTsResolve();
const { openLagSession, LagSessionError } = await import("../../src/lag/client.ts");

// The extender's hello names the load's windows and streams (M4), and anyone
// can run an extender. So the session holds the hello to what its ticket
// allows (A6), as an honest extender holds the ticket to its own bounds, and
// fails a hello that asks for more before any load runs.

const vectors = JSON.parse(readFileSync(new URL("./vectors.json", import.meta.url), "utf8"));

// A LagTicket's text form (M0) with the given caps in its body, and a
// signature nobody checks here: the extender does.
const ticketText = (caps: { downloadMs?: number; uploadMs?: number; maxStreams?: number; ticketId?: Uint8Array }) => {
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
  bytesField(body, 1, caps.ticketId ?? randomBytes(16));
  numberField(body, 9, 250);
  numberField(body, 10, caps.downloadMs);
  numberField(body, 11, caps.uploadMs);
  numberField(body, 12, caps.maxStreams);
  const ticket: number[] = [];
  bytesField(ticket, 1, body);
  bytesField(ticket, 2, randomBytes(64));
  bytesField(ticket, 3, randomBytes(8));
  return Buffer.from(ticket).toString("base64url");
};

// Opens a session on a mock extender whose hello is `hello`, with `ticket`;
// what the session made of it, and what reached the extender.
const openWith = async (hello: Partial<MockHello>, ticket: string) => {
  const mock = await startMockExtender({ hello });
  try {
    const session = openLagSession({ host: mock.origin, ticket });
    const outcome = await session.ready.then(
      (taken) => ({ hello: taken, error: undefined }),
      (error: unknown) => ({ hello: undefined, error }),
    );
    if (outcome.hello !== undefined) {
      session.close();
    }
    await session.closed;
    return { ...outcome, record: mock.sessions[0] };
  } finally {
    await mock.close();
  }
};

const assertRefused = (outcome: Awaited<ReturnType<typeof openWith>>, beyond: RegExp) => {
  assert.ok(outcome.error instanceof LagSessionError, `the session opened with ${JSON.stringify(outcome.hello)}`);
  assert.equal(outcome.error.code, "protocol");
  assert.match(outcome.error.message, beyond);
  assert.equal(outcome.record.loadRequests, 0);
  assert.equal(outcome.record.downloadRequests, 0);
};

test("a hello that asks for longer windows and more streams than any ticket fails before a load (the reviewer's hostile hello)", { timeout: 30_000 }, async () => {
  const mock = await startMockExtender({ hello: { downloadMs: 12_000, uploadMs: 12_000, maxStreams: 16 }, uploadBytesPerSecond: 4 << 20 });
  try {
    const events: string[] = [];
    const session = openLagSession({ host: mock.origin, ticket: "ticket-hostile", onEvent: (event) => events.push(event.type) });
    await assert.rejects(session.ready, (error: unknown) => {
      assert.ok(error instanceof LagSessionError);
      assert.equal(error.code, "protocol");
      assert.match(error.message, /downloadMs 12000, the ticket allows 8000; uploadMs 12000, the ticket allows 8000; maxStreams 16, the ticket allows 4/);
      return true;
    });
    const closed = await session.closed;
    assert.equal(closed.byClient, false);
    assert.equal(closed.error?.code, "protocol");
    assert.equal(session.hello, null);
    assert.ok(!events.includes("open"), "no open event for a hello that was refused");
    // the load can't be asked for, and nothing reached the extender for one
    await assert.rejects(session.startLoad(), (error: unknown) => error instanceof LagSessionError && error.code === "closed");
    const record = mock.sessions[0];
    assert.equal(record.loadRequests, 0);
    assert.equal(record.downloadRequests, 0);
    assert.equal(record.uploadStreams, 0);
    // the browser closed it: the close reaches the extender a moment later
    const deadline = performance.now() + 5000;
    while (record.closedBy === undefined && performance.now() < deadline) {
      await new Promise((resolve) => setTimeout(resolve, 10));
    }
    assert.equal(record.closedBy, "browser");
  } finally {
    await mock.close();
  }
});

test("a real ticket's own caps hold: 5 s each way and 4 streams, though 8 s would pass without one", { timeout: 30_000 }, async () => {
  const ticket: string = vectors.ticket.text;
  assert.equal(vectors.ticket.body.downloadMs, 5000);
  assert.equal(vectors.ticket.body.uploadMs, 5000);
  assert.equal(vectors.ticket.body.maxStreams, 4);

  const honest = await openWith({ downloadMs: 5000, uploadMs: 5000, maxStreams: 4 }, ticket);
  assert.equal(honest.error, undefined);
  assert.deepEqual(
    { downloadMs: honest.hello?.downloadMs, uploadMs: honest.hello?.uploadMs, maxStreams: honest.hello?.maxStreams },
    { downloadMs: 5000, uploadMs: 5000, maxStreams: 4 },
  );
  // asking for less is allowed: the ticket is a cap
  assert.equal((await openWith({ downloadMs: 1000, uploadMs: 1000, maxStreams: 2 }, ticket)).error, undefined);

  assertRefused(await openWith({ downloadMs: 8000 }, ticket), /^the extender's hello asks for more than its ticket allows: downloadMs 8000, the ticket allows 5000$/);
  assertRefused(await openWith({ uploadMs: 5001 }, ticket), /uploadMs 5001, the ticket allows 5000/);
  assertRefused(await openWith({ maxStreams: 5 }, ticket), /maxStreams 5, the ticket allows 4/);
});

test("the caps follow the ticket as the extender reads it: its values, the defaults when unset, and 250 ms at least", { timeout: 60_000 }, async () => {
  // a ticket for less than A6 issues
  const small = ticketText({ downloadMs: 2000, uploadMs: 1000, maxStreams: 2 });
  assert.equal((await openWith({ downloadMs: 2000, uploadMs: 1000, maxStreams: 2 }, small)).error, undefined);
  assertRefused(await openWith({ downloadMs: 2000, uploadMs: 1000, maxStreams: 3 }, small), /maxStreams 3, the ticket allows 2/);
  assertRefused(await openWith({ downloadMs: 2250, uploadMs: 1000, maxStreams: 2 }, small), /downloadMs 2250, the ticket allows 2000/);

  // unset caps are the extender's defaults: 5 s and 4 streams
  const unset = ticketText({});
  assert.equal((await openWith({ downloadMs: 5000, uploadMs: 5000, maxStreams: 4 }, unset)).error, undefined);
  assertRefused(await openWith({ downloadMs: 6000, uploadMs: 5000, maxStreams: 4 }, unset), /downloadMs 6000, the ticket allows 5000/);

  // the extender raises a window to one interval, and lowers it to 8 s and the streams to 16
  assert.equal((await openWith({ downloadMs: 250, uploadMs: 250 }, ticketText({ downloadMs: 100, uploadMs: 1 }))).error, undefined);
  const large = ticketText({ downloadMs: 20_000, uploadMs: 20_000, maxStreams: 32 });
  assert.equal((await openWith({ downloadMs: 8000, uploadMs: 8000, maxStreams: 16 }, large)).error, undefined);
  assertRefused(await openWith({ downloadMs: 8250, uploadMs: 8000, maxStreams: 16 }, large), /downloadMs 8250, the ticket allows 8000/);
});

test("a ticket this can't read allows 8 s windows and 4 streams", { timeout: 30_000 }, async () => {
  for (const ticket of ["ticket-1", "t", "AAAA", ticketText({ ticketId: new Uint8Array(8), downloadMs: 20_000 })]) {
    const outcome = await openWith({ downloadMs: 8000, uploadMs: 8000, maxStreams: 4 }, ticket);
    assert.equal(outcome.error, undefined, ticket);
    assert.equal(outcome.hello?.downloadMs, 8000, ticket);
    assert.equal(outcome.record.ticket, ticket);
  }
  assertRefused(await openWith({ downloadMs: 8001 }, "ticket-2"), /downloadMs 8001, the ticket allows 8000/);
  assertRefused(await openWith({ maxStreams: 5 }, "ticket-3"), /maxStreams 5, the ticket allows 4/);
});
