import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, readdir, readFile, rm, stat } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { registerTsResolve } from "../../test/ts-resolve.ts";
import { startMockApi, VISITOR_IP, type MockApi, type MockApiOptions } from "./mock_api.ts";

registerTsResolve();
const { createContext } = await import("../src/context.ts");
const { measureLatency, measureBufferbloat, MeasureError } = await import("../src/measure.ts");

// The measurement against the SDK's mock extenders (1 s loads each way)
// behind a mock api, over the CLI's own network layer (undici, with lag.dev's
// origin on measurement requests). Everything stays on 127.0.0.1.

const withApi = async (options: MockApiOptions, body: (api: MockApi) => Promise<void>) => {
  const api = await startMockApi(options);
  try {
    await body(api);
  } finally {
    await api.close();
  }
};

const contextFor = (api: MockApi, extra: Parameters<typeof createContext>[0] = {}) =>
  createContext({ apiURL: api.url, save: false, ...extra });

test("latency: every server is pinged, with no load and nothing about the address kept", async () => {
  await withApi({}, async (api) => {
    const local = contextFor(api);
    try {
      const result = await measureLatency(local.context, { durationMs: 1500, servers: 3 });
      assert.equal(result.mode, "latency");
      assert.equal(result.servers.length, 3);
      for (const server of result.servers) {
        assert.equal(server.error, undefined);
        assert.ok(server.pings >= 3, `${server.city}: ${server.pings} pings`);
        assert.equal(server.lost, 0);
        assert.ok(server.idle && server.idle.p50Ms > 0 && server.idle.p95Ms >= server.idle.p50Ms);
        assert.ok(server.rpm && server.rpm.value > 0);
        assert.equal(server.load, undefined);
      }
      // nearest first: Houston, then Dallas, then London
      assert.deepEqual(result.servers.map((server) => server.city), ["Houston", "Dallas", "London"]);
      assert.ok(result.overall.best && result.overall.rpm);
      assert.equal(result.bytes.total, 0);
      assert.equal(result.stoppedEarly, null);
      // in Node this process answers the pings, not the network stack
      assert.match(result.notes[0], /This process answers them on its main thread, which also reads the downloads/);
      assert.ok(!result.notes.some((note) => /network stack answers them\./.test(note)));
      assert.equal(result.visitor?.city, "Austin");
      assert.deepEqual(result.visitor?.asn, { number: 64500, organization: "Example Networks" });
      assert.ok(!JSON.stringify(result).includes(VISITOR_IP), "the visitor's address is never in the result");
      // the api's requests carry no origin; the extenders' are lag.dev's (checked in net.test.ts)
      assert.ok(api.requests.every((request) => request.origin === undefined));
    } finally {
      await local.close();
    }
  });
});

test("bufferbloat: each server loads in turn and the result says what the loads added", async () => {
  await withApi({ places: ["houston", "london"] }, async (api) => {
    const local = contextFor(api);
    try {
      const result = await measureBufferbloat(local.context, {
        maxBytes: 20e9,
        maxMs: 60_000,
        servers: 2,
        idleMs: 600,
        settleMs: 200,
      });
      assert.equal(result.mode, "bufferbloat");
      assert.equal(result.stoppedEarly, null);
      for (const server of result.servers) {
        const load = server.load!;
        assert.equal(load.status, "complete", `${server.city}: ${load.status} ${load.reason}`);
        assert.ok(load.download && load.download.p50Mbps! > 0 && load.download.bytes > 0);
        assert.ok(load.upload && load.upload.p50Mbps! > 0 && load.upload.bytes > 0);
        assert.equal(load.upload.verified, true);
        for (const direction of [load.download, load.upload]) {
          assert.ok(direction.increaseMs === null || Number.isFinite(direction.increaseMs));
        }
      }
      assert.equal(result.bytes.total, result.bytes.download + result.bytes.upload);
      assert.ok(result.bytes.total > 0);
      assert.equal(result.bytes.maxBytes, 20e9);
      assert.ok(result.overall.downloadMbps?.best && result.overall.uploadMbps?.best);
      assert.ok(result.overall.loadedIncreaseMs);
      // one load per server, each from its own session
      for (const extender of api.extenders.values()) {
        assert.equal(extender.sessions.filter((session) => session.loadRequests > 0).length, 1);
      }
    } finally {
      await local.close();
    }
  });
});

test("bufferbloat stops at the byte cap, cutting the running load and skipping the rest", async () => {
  // the mock downloads as fast as loopback carries it, gigabits a second, so a
  // whole 1 s download would move hundreds of MB
  await withApi({ places: ["houston", "london"] }, async (api) => {
    const local = contextFor(api);
    try {
      const result = await measureBufferbloat(local.context, {
        maxBytes: 2e6,
        maxMs: 60_000,
        servers: 2,
        idleMs: 300,
        settleMs: 100,
      });
      assert.equal(result.stoppedEarly, "max_bytes");
      assert.equal(result.servers[0].load?.reason, "max_bytes");
      assert.notEqual(result.servers[0].load?.status, "complete");
      assert.equal(result.servers[1].load?.status, "skipped");
      // the meter cuts it within some 20 ms of the cap, not a whole interval
      assert.ok(result.bytes.total < 60e6, `moved ${result.bytes.total}`);
    } finally {
      await local.close();
    }
  });
});

test("bufferbloat stops before a load the time cap can't fit", async () => {
  await withApi({ places: ["houston", "london"] }, async (api) => {
    const local = contextFor(api);
    try {
      // a load is 1 s each way plus 2 s of setup; 2.5 s leaves no room for one
      const result = await measureBufferbloat(local.context, { maxBytes: 20e9, maxMs: 2500, servers: 2, idleMs: 100 });
      assert.equal(result.stoppedEarly, "max_seconds");
      assert.ok(result.servers.every((server) => server.load?.status === "skipped"));
      assert.equal(result.bytes.total, 0);
    } finally {
      await local.close();
    }
  });
});

test("the guest is kept between runs in a file only its user can read", async () => {
  const dir = await mkdtemp(join(tmpdir(), "lagdev-test-"));
  try {
    await withApi({ places: ["houston"] }, async (api) => {
      for (let run = 0; run < 2; run += 1) {
        const local = createContext({ apiURL: api.url, configDir: dir });
        try {
          await measureLatency(local.context, { durationMs: 1000, servers: 1 });
        } finally {
          await local.close();
        }
      }
      assert.equal(api.sessionCreates, 1, "the second run reused the guest");
      const [file] = await readdir(dir);
      assert.match(file, /^guest-127\.0\.0\.1_\d+\.json$/);
      assert.equal((await stat(join(dir, file))).mode & 0o777, 0o600);
      const stored = JSON.parse(await readFile(join(dir, file), "utf8"));
      assert.ok(!JSON.stringify(stored).includes(VISITOR_IP));
    });
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("a guest the api no longer takes is replaced once", async () => {
  await withApi({ places: ["houston"], rejectJwt: "jwt-1" }, async (api) => {
    const local = contextFor(api);
    try {
      const result = await measureLatency(local.context, { durationMs: 1000, servers: 1 });
      assert.equal(result.servers[0].error, undefined);
      assert.equal(api.sessionCreates, 2);
    } finally {
      await local.close();
    }
  });
});

test("an api that takes no guest at all is an error, not a loop", async () => {
  await withApi({ places: ["houston"], rejectJwt: /^jwt-/ }, async (api) => {
    const local = contextFor(api);
    try {
      await assert.rejects(measureLatency(local.context, { durationMs: 1000, servers: 1 }), /unauthorized|401/);
      assert.equal(api.sessionCreates, 2, "one replacement, then it gives up");
    } finally {
      await local.close();
    }
  });
});

test("the api's refusal of a guest says when to try again", async () => {
  await withApi(
    { places: ["houston"], refuseSession: { code: "limit", message: "too many guests from this network", retry_after_ms: 60_000 } },
    async (api) => {
      const local = contextFor(api);
      try {
        await assert.rejects(measureLatency(local.context, { durationMs: 1000, servers: 1 }), (error: unknown) => {
          assert.ok(error instanceof MeasureError);
          assert.equal(error.code, "limit");
          assert.equal(error.retryAfterMs, 60_000);
          assert.match(error.message, /too many guests.*try again in 60 s/);
          return true;
        });
      } finally {
        await local.close();
      }
    },
  );
});

test("a server whose session won't open is replaced by a spare", async () => {
  await withApi({ places: ["houston", "dallas", "london"], deadFirstTicket: true }, async (api) => {
    const local = contextFor(api);
    try {
      const result = await measureLatency(local.context, { durationMs: 1000, servers: 1 });
      assert.equal(result.servers.length, 1);
      assert.equal(result.servers[0].error, undefined);
      assert.notEqual(result.servers[0].city, "Houston", "Houston's ticket pointed nowhere");
      assert.ok(result.servers[0].pings > 0);
      // the track asked for the server and its spares in one call
      assert.equal(api.trackRequests.length, 1);
      assert.equal(api.trackRequests[0].extenders.length, 3);
    } finally {
      await local.close();
    }
  });
});
