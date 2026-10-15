import { test } from "node:test";
import assert from "node:assert/strict";
import { registerTsResolve } from "../ts-resolve.ts";
import { startMockExtender } from "./client_mock_extender.ts";
import type { LagSessionEvent } from "../../src/lag/client.ts";

registerTsResolve();
const { openLagSession, LagSessionError } = await import("../../src/lag/client.ts");

// A full extender closes its oldest session whose load has ended and whose
// report has gone out, with 4410 "reclaimed", to admit a new one (M3). The
// session finished: its load's result stands, the close names why, and no
// error is reported and nothing reconnects.

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

test("a session the extender reclaims after its load ends as reclaimed, not as a failure", { timeout: 30_000 }, async () => {
  const mock = await startMockExtender({ reclaimAfterLoadMs: 300, uploadBytesPerSecond: 4 << 20 });
  try {
    const events: LagSessionEvent[] = [];
    const session = openLagSession({ host: mock.origin, ticket: "ticket-reclaimed", onEvent: (event) => events.push(event) });
    await session.ready;
    const result = await session.startLoad();
    assert.equal(result.endReason, "complete");
    assert.ok(0 < result.download.bytes && 0 < result.upload.bytes);

    const closed = await session.closed;
    assert.equal(closed.code, 4410);
    assert.equal(closed.byClient, false);
    assert.ok(closed.error instanceof LagSessionError);
    assert.equal(closed.error.code, "reclaimed");
    assert.equal(closed.error.closeCode, 4410);
    assert.equal(session.state, "closed");
    // a finished session reports no error, and its load's events stand
    assert.deepEqual(events.filter((event) => event.type === "error"), []);
    assert.deepEqual(
      events.filter((event) => event.type === "load").map((event) => event.type === "load" && event.phase),
      ["download", "upload", "done"],
    );
    assert.equal(events.filter((event) => event.type === "loadResult").length, 1);
    // nothing reconnects it
    await sleep(1000);
    assert.equal(mock.sessions.length, 1);
    assert.equal(mock.sessions[0].closeCode, 4410);
    assert.equal(mock.sessions[0].closedBy, "extender");
  } finally {
    await mock.close();
  }
});

test("a close with another code is still a failure", { timeout: 30_000 }, async () => {
  // 4408, the extender's word that the page stopped answering pings
  const mock = await startMockExtender({ ignorePongs: true, noPongCloseMs: 500 });
  try {
    const events: LagSessionEvent[] = [];
    const session = openLagSession({ host: mock.origin, ticket: "ticket-dropped", onEvent: (event) => events.push(event) });
    await session.ready;
    const closed = await session.closed;
    assert.equal(closed.code, 4408);
    assert.equal(closed.error?.code, "dropped");
    assert.deepEqual(
      events.filter((event) => event.type === "error").map((event) => event.type === "error" && [event.error.code, event.fatal]),
      [["dropped", true]],
    );
  } finally {
    await mock.close();
  }
});
