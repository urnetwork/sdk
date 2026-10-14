import { test } from "node:test";
import assert from "node:assert/strict";
import { registerTsResolve } from "../ts-resolve.ts";

registerTsResolve();
const {
  LagGuestSession,
  LagGuestSessionError,
  LAG_GUEST_STORAGE_KEY,
  memoryLagStorage,
} = await import("../../src/lag/session.ts");

const MINUTE = 60_000;
const T0 = Date.parse("2026-10-12T12:00:00Z");

const guestExpiring = (n: number, expiresAt: number) => ({
  by_client_jwt: `jwt-${n}`,
  network_id: `network-${n}`,
  client_id: `client-${n}`,
  expires_at: new Date(expiresAt).toISOString(),
});

// an api whose /lag/session answers from a queue, counting calls
function fakeApi(clock: { now: number }, answers: Array<"guest" | Error | object> = []) {
  let created = 0;
  const api = {
    calls: 0,
    async lagSession() {
      api.calls += 1;
      await new Promise((resolve) => setTimeout(resolve, 5));
      const answer = answers.shift() ?? "guest";
      if (answer instanceof Error) {
        throw answer;
      }
      if (answer !== "guest") {
        return answer;
      }
      created += 1;
      return guestExpiring(created, clock.now + 24 * 60 * MINUTE);
    },
  };
  return api;
}

const setup = (answers?: Array<"guest" | Error | object>) => {
  const clock = { now: T0 };
  const storage = memoryLagStorage();
  const api = fakeApi(clock, answers);
  const session = new LagGuestSession({ storage, api, now: () => clock.now });
  return { clock, storage, api, session };
};

test("the first get creates a guest and keeps it under lagdev.guest", async () => {
  const { storage, api, session } = setup();
  const guest = await session.get();
  assert.equal(guest.by_client_jwt, "jwt-1");
  assert.equal(api.calls, 1);
  assert.equal(LAG_GUEST_STORAGE_KEY, "lagdev.guest");
  assert.deepEqual(JSON.parse(storage.get("lagdev.guest") as string), guest);
  assert.equal(await session.token(), "jwt-1");
  assert.equal(api.calls, 1);
});

test("a stored guest is reused across page loads", async () => {
  const { clock, storage, api, session } = setup();
  await session.get();
  clock.now += 6 * 60 * MINUTE;
  const reloaded = new LagGuestSession({ storage, api, now: () => clock.now });
  assert.equal(await reloaded.token(), "jwt-1");
  assert.equal(api.calls, 1);
});

test("a guest within 10 minutes of expiry is renewed", async () => {
  const { clock, storage, api, session } = setup();
  await session.get();
  clock.now += 24 * 60 * MINUTE - 11 * MINUTE;
  assert.equal(await session.token(), "jwt-1", "11 minutes left is fresh");
  clock.now += 2 * MINUTE;
  assert.equal(await session.token(), "jwt-2", "9 minutes left renews");
  assert.equal(api.calls, 2);
  assert.equal(JSON.parse(storage.get("lagdev.guest") as string).by_client_jwt, "jwt-2");
});

test("a failed renewal keeps the unexpired guest and waits before asking again", async () => {
  const { clock, api, session } = setup(["guest", new Error("down"), new Error("down")]);
  await session.get();
  clock.now += 24 * 60 * MINUTE - 5 * MINUTE;
  assert.equal(await session.token(), "jwt-1");
  assert.equal(api.calls, 2);
  assert.equal(await session.token(), "jwt-1");
  assert.equal(api.calls, 2, "no retry within a minute");
  clock.now += MINUTE;
  assert.equal(await session.token(), "jwt-1");
  assert.equal(api.calls, 3);
  clock.now += MINUTE;
  assert.equal(await session.token(), "jwt-2");
  // once expired, a failure is the caller's
  const expired = setup(["guest", new Error("down")]);
  await expired.session.get();
  expired.clock.now += 24 * 60 * MINUTE;
  await assert.rejects(expired.session.get(), /down/);
});

test("a refusal from the api is a LagGuestSessionError with its code", async () => {
  const { session } = setup([{ error: { code: "limit", retry_after_ms: 120_000 } }]);
  await assert.rejects(session.get(), (error: unknown) => {
    assert.ok(error instanceof LagGuestSessionError);
    assert.equal(error.code, "limit");
    assert.equal(error.retryAfterMs, 120_000);
    return true;
  });
  const malformed = setup([{ by_client_jwt: "", network_id: "n", client_id: "c", expires_at: "soon" }]);
  await assert.rejects(malformed.session.get(), LagGuestSessionError);
});

test("concurrent gets share one creation", async () => {
  const { api, session } = setup();
  const tokens = await Promise.all([session.token(), session.token(), session.get(), session.token()]);
  assert.equal(api.calls, 1);
  assert.deepEqual(tokens.map((t) => (typeof t === "string" ? t : t.by_client_jwt)), ["jwt-1", "jwt-1", "jwt-1", "jwt-1"]);
});

test("a corrupt or foreign stored value is replaced", async () => {
  for (const stored of ["{not json", "null", '{"by_client_jwt":"x"}', JSON.stringify({ ...guestExpiring(9, T0), expires_at: "never" })]) {
    const { storage, api, session } = setup();
    storage.set("lagdev.guest", stored);
    assert.equal(await session.token(), "jwt-1", stored);
    assert.equal(api.calls, 1);
  }
});

test("another tab's renewal in storage is picked up", async () => {
  const { clock, storage, api, session } = setup();
  await session.get();
  clock.now += 24 * 60 * MINUTE - 5 * MINUTE;
  storage.set("lagdev.guest", JSON.stringify(guestExpiring(7, clock.now + 24 * 60 * MINUTE)));
  assert.equal(await session.token(), "jwt-7");
  assert.equal(api.calls, 1);
});

test("invalidate forgets only the refused jwt", async () => {
  const { storage, api, session } = setup();
  await session.get();
  await session.invalidate("jwt-other");
  assert.equal(await session.token(), "jwt-1", "a different jwt leaves the guest");
  await session.invalidate("jwt-1");
  assert.equal(storage.get("lagdev.guest"), null);
  assert.equal(await session.token(), "jwt-2");
  // a stale caller invalidating the old jwt does not drop the new one
  await session.invalidate("jwt-1");
  assert.equal(await session.token(), "jwt-2");
  assert.equal(api.calls, 2);
  await session.invalidate();
  assert.equal(await session.token(), "jwt-3");
});

test("storage that throws costs only persistence", async () => {
  const clock = { now: T0 };
  const api = fakeApi(clock);
  const broken = {
    get: () => {
      throw new Error("SecurityError");
    },
    set: () => {
      throw new Error("QuotaExceededError");
    },
    remove: async () => {
      throw new Error("SecurityError");
    },
  };
  const session = new LagGuestSession({ storage: broken, api, now: () => clock.now });
  assert.equal(await session.token(), "jwt-1");
  assert.equal(await session.token(), "jwt-1");
  assert.equal(api.calls, 1);
  await session.invalidate();
  assert.equal(await session.token(), "jwt-2");
});

test("async storage works too", async () => {
  const clock = { now: T0 };
  const api = fakeApi(clock);
  const values = new Map<string, string>();
  const storage = {
    get: async (key: string) => values.get(key),
    set: async (key: string, value: string) => {
      values.set(key, value);
    },
    remove: async (key: string) => {
      values.delete(key);
    },
  };
  await new LagGuestSession({ storage, api, now: () => clock.now }).get();
  assert.equal(await new LagGuestSession({ storage, api, now: () => clock.now }).token(), "jwt-1");
  assert.equal(api.calls, 1);
});

test("aborting a get abandons only that wait; the guest is still kept", async () => {
  const { storage, api, session } = setup();
  const controller = new AbortController();
  const waiting = session.get({ signal: controller.signal });
  controller.abort(new Error("page hidden"));
  await assert.rejects(waiting, /page hidden/);
  assert.equal(await session.token(), "jwt-1");
  assert.equal(api.calls, 1);
  assert.ok(storage.get("lagdev.guest"));
  await assert.rejects(session.get({ signal: AbortSignal.abort(new Error("already")) }), /already/);
});
