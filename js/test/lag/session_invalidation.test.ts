// Guest invalidation across storage and renewal awaits, using explicit promises
// and the session's injected clock instead of timers or real credentials.
import { test } from "node:test";
import assert from "node:assert/strict";
import { setImmediate } from "node:timers/promises";
import { registerTsResolve } from "../ts-resolve.ts";

registerTsResolve();
const { LagGuestSession, LAG_GUEST_STORAGE_KEY, memoryLagStorage } = await import("../../src/lag/session.ts");

const MINUTE = 60_000;
const T0 = Date.parse("2026-10-12T12:00:00Z");

const guest = (n: number, expiresAt = T0 + 24 * 60 * MINUTE) => ({
  by_client_jwt: `synthetic-jwt-${n}`,
  network_id: `synthetic-network-${n}`,
  client_id: `synthetic-client-${n}`,
  expires_at: new Date(expiresAt).toISOString(),
});

// The caller owns exactly when a storage read or renewal completes.
const deferred = <T>() => {
  let resolve!: (value: T) => void;
  let reject!: (reason: unknown) => void;
  const promise = new Promise<T>((a, b) => { resolve = a; reject = b; });
  return { promise, resolve, reject };
};

test("a rejected renewal never returns its invalidated fallback to any waiter", async () => {
  // Both an in-memory guest and one first found in storage are fallbacks.
  for (const storedOnly of [false, true]) {
    const storage = memoryLagStorage();
    const renewing = deferred<void>();
    const renewal = deferred<ReturnType<typeof guest>>();
    let now = T0;
    let calls = 0;
    const original = guest(1);
    if (storedOnly) {
      storage.set(LAG_GUEST_STORAGE_KEY, JSON.stringify(original));
    }
    const session = new LagGuestSession({
      storage,
      now: () => now,
      api: { lagSession: async () => {
        calls += 1;
        if (!storedOnly && calls === 1) return original;
        if (calls === (storedOnly ? 1 : 2)) {
          renewing.resolve();
          return renewal.promise;
        }
        return guest(2, now + 24 * 60 * MINUTE);
      } },
    });
    if (!storedOnly) assert.equal(await session.token(), original.by_client_jwt);
    now += 24 * 60 * MINUTE - 5 * MINUTE;
    const waiting = [session.token(), session.token()];
    const settled = Promise.allSettled(waiting);
    await renewing.promise;
    await session.invalidate(original.by_client_jwt);
    assert.equal(storage.get(LAG_GUEST_STORAGE_KEY), null);
    const afterInvalidation = session.token();
    const afterSettled = Promise.allSettled([afterInvalidation]);
    const failure = new Error("synthetic renewal failure");
    renewal.reject(failure);
    for (const outcome of [...await settled, ...await afterSettled]) {
      assert.equal(outcome.status, "rejected", `storedOnly=${storedOnly}`);
      if (outcome.status === "rejected") assert.equal(outcome.reason, failure);
    }
    assert.equal(await session.token(), "synthetic-jwt-2", "replacement starts without the old cooldown");
    assert.equal(calls, storedOnly ? 2 : 3);
    assert.equal(JSON.parse(storage.get(LAG_GUEST_STORAGE_KEY) as string).by_client_jwt, "synthetic-jwt-2");
  }
});

test("invalidation fences an older storage read before it can publish the refused guest", async () => {
  const stored = memoryLagStorage();
  stored.set(LAG_GUEST_STORAGE_KEY, JSON.stringify(guest(1)));
  const reading = deferred<void>();
  const read = deferred<string>();
  let reads = 0;
  let calls = 0;
  const session = new LagGuestSession({
    now: () => T0,
    storage: {
      ...stored,
      get: (key) => {
        reads += 1;
        if (reads === 1) {
          reading.resolve();
          return read.promise;
        }
        return stored.get(key);
      },
    },
    api: { lagSession: async () => { calls += 1; return guest(2); } },
  });
  const waiting = session.token();
  await reading.promise;
  await session.invalidate("synthetic-jwt-1");
  read.resolve(JSON.stringify(guest(1)));
  assert.equal(await waiting, "synthetic-jwt-2");
  assert.equal(await session.token(), "synthetic-jwt-2");
  assert.equal(calls, 1);
});

test("rejecting an older jwt preserves a newer guest and its failed-renewal fallback", async () => {
  const storage = memoryLagStorage();
  const renewing = deferred<void>();
  const renewal = deferred<ReturnType<typeof guest>>();
  let now = T0;
  let calls = 0;
  const session = new LagGuestSession({
    storage,
    now: () => now,
    api: { lagSession: async () => {
      calls += 1;
      if (calls === 1) return guest(2);
      renewing.resolve();
      return renewal.promise;
    } },
  });
  assert.equal(await session.token(), "synthetic-jwt-2");
  now += 24 * 60 * MINUTE - 5 * MINUTE;
  const waiting = session.token();
  await renewing.promise;
  await session.invalidate("synthetic-jwt-1");
  renewal.reject(new Error("synthetic renewal failure"));
  assert.equal(await waiting, "synthetic-jwt-2");
  assert.equal(await session.token(), "synthetic-jwt-2", "a valid fallback still has its renewal cooldown");
  assert.equal(JSON.parse(storage.get(LAG_GUEST_STORAGE_KEY) as string).by_client_jwt, "synthetic-jwt-2");
  assert.equal(calls, 2);
});

test("a successful renewal can replace the guest invalidated while it waited", async () => {
  const storage = memoryLagStorage();
  const renewing = deferred<void>();
  const renewal = deferred<ReturnType<typeof guest>>();
  let now = T0;
  let calls = 0;
  const session = new LagGuestSession({
    storage,
    now: () => now,
    api: { lagSession: async () => {
      calls += 1;
      if (calls === 1) return guest(1);
      renewing.resolve();
      return renewal.promise;
    } },
  });
  await session.token();
  now += 24 * 60 * MINUTE - 5 * MINUTE;
  const waiting = session.token();
  await renewing.promise;
  await session.invalidate("synthetic-jwt-1");
  renewal.resolve(guest(2, now + 24 * 60 * MINUTE));
  assert.equal(await waiting, "synthetic-jwt-2");
  await session.invalidate("synthetic-jwt-1");
  assert.equal(await session.token(), "synthetic-jwt-2");
  assert.equal(JSON.parse(storage.get(LAG_GUEST_STORAGE_KEY) as string).by_client_jwt, "synthetic-jwt-2");
  assert.equal(calls, 2);
});

test("a fallback that expires during renewal is not returned when renewal fails", async () => {
  const renewing = deferred<void>();
  const renewal = deferred<ReturnType<typeof guest>>();
  let now = T0;
  let calls = 0;
  const session = new LagGuestSession({
    storage: memoryLagStorage(),
    now: () => now,
    api: { lagSession: async () => {
      calls += 1;
      if (calls === 1) return guest(1);
      if (calls === 2) {
        renewing.resolve();
        return renewal.promise;
      }
      return guest(2, now + 24 * 60 * MINUTE);
    } },
  });
  await session.token();
  now += 24 * 60 * MINUTE - 5 * MINUTE;
  const rejected = assert.rejects(session.token(), /synthetic renewal failure/);
  await renewing.promise;
  now += 5 * MINUTE;
  renewal.reject(new Error("synthetic renewal failure"));
  await rejected;
  assert.equal(await session.token(), "synthetic-jwt-2");
  assert.equal(calls, 3);
});

test("an old invalidation cannot remove a newer guest saved by the same session", async () => {
  for (const heldOperation of ["read", "remove"] as const) {
    const stored = memoryLagStorage();
    const entered = deferred<void>();
    const release = deferred<void>();
    const renewing = deferred<void>();
    const renewal = deferred<ReturnType<typeof guest>>();
    let hold = false;
    let now = T0;
    let calls = 0;
    const session = new LagGuestSession({
      now: () => now,
      storage: {
        ...stored,
        get: async (key) => {
          const snapshot = await stored.get(key);
          if (hold && heldOperation === "read") {
            hold = false;
            entered.resolve();
            await release.promise;
          }
          return snapshot;
        },
        remove: async (key) => {
          if (hold && heldOperation === "remove") {
            hold = false;
            entered.resolve();
            await release.promise;
          }
          await stored.remove(key);
        },
      },
      api: { lagSession: async () => {
        calls += 1;
        if (calls === 1) return guest(1);
        renewing.resolve();
        return renewal.promise;
      } },
    });
    await session.token();
    now += 24 * 60 * MINUTE - 5 * MINUTE;
    const replacement = session.token();
    await renewing.promise;
    hold = true;
    const invalidated = session.invalidate("synthetic-jwt-1");
    await entered.promise;
    renewal.resolve(guest(2, now + 24 * 60 * MINUTE));
    // Drain the released API's promise reactions before releasing storage.
    // This is an event-loop barrier, with no elapsed-time assumption.
    await setImmediate();
    assert.equal(await session.token(), "synthetic-jwt-2", "the replacement is already current while persistence waits");
    release.resolve();
    await invalidated;
    assert.equal(await replacement, "synthetic-jwt-2");
    assert.equal(JSON.parse(stored.get(LAG_GUEST_STORAGE_KEY) as string).by_client_jwt, "synthetic-jwt-2", heldOperation);
    const reloaded = new LagGuestSession({ storage: stored, now: () => now, api: {
      lagSession: async () => { assert.fail("the persisted replacement must be reused"); },
    } });
    assert.equal(await reloaded.token(), "synthetic-jwt-2");
    assert.equal(calls, 2);
  }
});

test("a get begun during invalidation cannot reload the still-stored refused guest", async () => {
  const stored = memoryLagStorage();
  const removing = deferred<void>();
  const remove = deferred<void>();
  let calls = 0;
  const session = new LagGuestSession({
    now: () => T0,
    storage: {
      ...stored,
      remove: async (key) => {
        removing.resolve();
        await remove.promise;
        await stored.remove(key);
      },
    },
    api: { lagSession: async () => { calls += 1; return guest(calls); } },
  });
  await session.token();
  const invalidated = session.invalidate("synthetic-jwt-1");
  await removing.promise;
  const waiting = session.token();
  await setImmediate();
  remove.resolve();
  await invalidated;
  assert.equal(await waiting, "synthetic-jwt-2");
  assert.equal(await session.token(), "synthetic-jwt-2");
  assert.equal(calls, 2);
});

test("a guest invalidated during its storage write is not returned or left persisted", async () => {
  for (const refused of ["synthetic-jwt-2", undefined]) {
    const stored = memoryLagStorage();
    const saving = deferred<void>();
    const write = deferred<void>();
    let now = T0;
    let calls = 0;
    const session = new LagGuestSession({
      now: () => now,
      storage: {
        ...stored,
        set: async (key, value) => {
          if (JSON.parse(value).by_client_jwt === "synthetic-jwt-2") {
            saving.resolve();
            await write.promise;
          }
          await stored.set(key, value);
        },
      },
      api: { lagSession: async () => { calls += 1; return guest(calls, now + 24 * 60 * MINUTE); } },
    });
    await session.token();
    now += 24 * 60 * MINUTE - 5 * MINUTE;
    const waiting = session.token();
    const rejected = assert.rejects(waiting, /invalidated/);
    await saving.promise;
    const invalidated = session.invalidate(refused);
    write.resolve();
    await invalidated;
    await rejected;
    assert.equal(stored.get(LAG_GUEST_STORAGE_KEY), null);
    assert.equal(await session.token(), "synthetic-jwt-3");
    assert.equal(calls, 3);
  }
});

test("failed storage removal and repeated invalidations never restore a refused stored guest", async () => {
  const stored = memoryLagStorage();
  let now = T0;
  let calls = 0;
  const session = new LagGuestSession({
    now: () => now,
    storage: { ...stored, remove: () => { throw new Error("synthetic removal failure"); } },
    api: { lagSession: async () => {
      calls += 1;
      if (calls === 2) throw new Error("synthetic replacement failure");
      return guest(calls, now + 24 * 60 * MINUTE);
    } },
  });
  assert.equal(await session.token(), "synthetic-jwt-1");
  await session.invalidate("synthetic-jwt-1");
  await session.invalidate("synthetic-jwt-1");
  await session.invalidate("synthetic-unrelated-jwt");
  assert.equal(JSON.parse(stored.get(LAG_GUEST_STORAGE_KEY) as string).by_client_jwt, "synthetic-jwt-1");
  await assert.rejects(session.token(), /synthetic replacement failure/);
  assert.equal(await session.token(), "synthetic-jwt-3");
  assert.equal(calls, 3);
  // A confirmed fresh write restores storage reuse, including another tab's
  // subsequent renewal. No history of rejected tokens is retained.
  now += 24 * 60 * MINUTE - 5 * MINUTE;
  stored.set(LAG_GUEST_STORAGE_KEY, JSON.stringify(guest(4, now + 24 * 60 * MINUTE)));
  assert.equal(await session.token(), "synthetic-jwt-4");
  assert.equal(calls, 3);
});

test("failed old-guest cleanup preserves an independently known newer guest", async () => {
  const stored = memoryLagStorage();
  let now = T0;
  let calls = 0;
  const session = new LagGuestSession({
    now: () => now,
    storage: {
      ...stored,
      set: (key, value) => {
        if (JSON.parse(value).by_client_jwt !== "synthetic-jwt-1") throw new Error("synthetic write failure");
        return stored.set(key, value);
      },
      remove: () => { throw new Error("synthetic removal failure"); },
    },
    api: { lagSession: async () => {
      calls += 1;
      if (calls === 3) throw new Error("synthetic renewal failure");
      return guest(calls, now + 24 * 60 * MINUTE);
    } },
  });
  await session.token();
  now += 24 * 60 * MINUTE - 5 * MINUTE;
  assert.equal(await session.token(), "synthetic-jwt-2");
  await session.invalidate("synthetic-jwt-1");
  assert.equal(await session.token(), "synthetic-jwt-2");
  now += 24 * 60 * MINUTE - 5 * MINUTE;
  assert.equal(await session.token(), "synthetic-jwt-2", "the trusted in-memory fallback survives storage failure");
  assert.equal(calls, 3);
});

test("successful removal restores storage reuse after invalidation", async () => {
  const stored = memoryLagStorage();
  let calls = 0;
  const session = new LagGuestSession({
    now: () => T0, storage: stored,
    api: { lagSession: async () => { calls += 1; return guest(1); } },
  });
  await session.token();
  await session.invalidate("synthetic-jwt-1");
  stored.set(LAG_GUEST_STORAGE_KEY, JSON.stringify(guest(2)));
  assert.equal(await session.token(), "synthetic-jwt-2");
  assert.equal(calls, 1);
});
