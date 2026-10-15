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
  // A1's fields, and on this device's clock when it is due and when it expires
  assert.deepEqual(JSON.parse(storage.get("lagdev.guest") as string), {
    ...guest,
    local_renew_ms: T0 + 24 * 60 * MINUTE - 10 * MINUTE,
    local_expires_ms: T0 + 24 * 60 * MINUTE,
  });
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

// A guest's lifetime is read on the api's clock and counted on the device's
// (S4, A1): a device clock that is off by hours neither makes a fresh guest
// due, which would make a new guest on every call and spend the /24's ten
// an hour, nor keeps one past its end.

// a by_client_jwt as the api signs it, with `iat` on the api's clock
const jwtIssuedAt = (n: number, issuedAtMs: number) =>
  [
    Buffer.from(JSON.stringify({ alg: "HS256", typ: "JWT" })).toString("base64url"),
    Buffer.from(JSON.stringify({ client_id: `client-${n}`, lag_guest: true, iat: Math.floor(issuedAtMs / 1000) })).toString("base64url"),
    "c2lnbmF0dXJl",
  ].join(".");

// An api on its own clock, `skewMs` behind the device's: a guest expires 24 h
// after it was made, in whole seconds, and its jwt carries `iat` unless
// `withIat` is false.
const skewedSetup = (skewMs: number, withIat = true) => {
  const clock = { now: T0 + skewMs };
  const apiNow = () => clock.now - skewMs;
  let created = 0;
  const api = {
    calls: 0,
    async lagSession() {
      api.calls += 1;
      created += 1;
      const issuedAt = apiNow();
      return {
        by_client_jwt: withIat ? jwtIssuedAt(created, issuedAt) : `jwt-${created}`,
        network_id: `network-${created}`,
        client_id: `client-${created}`,
        expires_at: new Date(Math.floor((issuedAt + 24 * 60 * MINUTE) / 1000) * 1000).toISOString(),
      };
    },
  };
  const storage = memoryLagStorage();
  const session = new LagGuestSession({ storage, api, now: () => clock.now });
  return { clock, api, storage, session };
};

test("a device clock a day ahead doesn't make every get create a guest (the reviewer's demo)", async () => {
  // the api's jwt here says nothing of when it was made
  const { api, session } = skewedSetup(24 * 60 * MINUTE + MINUTE, false);
  for (let i = 0; i < 5; i += 1) {
    await session.token();
  }
  assert.equal(api.calls, 1, "guests created for 5 token() calls");
});

for (const [name, skewMs] of [
  ["23 h 50 min ahead", 23 * 60 * MINUTE + 50 * MINUTE],
  ["a day ahead", 24 * 60 * MINUTE + MINUTE],
  ["two days ahead", 48 * 60 * MINUTE],
  ["a day behind", -24 * 60 * MINUTE],
  ["right", 0],
] as const) {
  test(`with the device clock ${name}, a guest lives its 24 h on the device's clock from when it arrived`, async () => {
    const { clock, api, storage, session } = skewedSetup(skewMs);
    const first = await session.token();
    for (let i = 0; i < 5; i += 1) {
      assert.equal(await session.token(), first);
    }
    assert.equal(api.calls, 1);
    // another tab on this device reads the same times
    const tab = new LagGuestSession({ storage, api, now: () => clock.now });
    assert.equal(await tab.token(), first);
    assert.equal(api.calls, 1);
    // due 10 minutes before its 24 h are up, by the device's clock
    clock.now += 24 * 60 * MINUTE - 11 * MINUTE;
    assert.equal(await session.token(), first, "11 minutes left is fresh");
    clock.now += 2 * MINUTE;
    assert.notEqual(await session.token(), first, "9 minutes left renews");
    assert.equal(api.calls, 2);
  });
}

test("a guest the api gave a short life is renewed halfway, never as it arrives", async () => {
  const clock = { now: T0 + 6 * 60 * MINUTE };
  const api = {
    calls: 0,
    async lagSession() {
      api.calls += 1;
      // made at T0 by the api's clock, for 15 minutes: less than twice the renewal window
      return {
        by_client_jwt: jwtIssuedAt(api.calls, T0),
        network_id: "n",
        client_id: "c",
        expires_at: new Date(T0 + 15 * MINUTE).toISOString(),
      };
    },
  };
  const session = new LagGuestSession({ storage: memoryLagStorage(), api, now: () => clock.now });
  const first = await session.token();
  assert.equal(await session.token(), first);
  clock.now += 7 * MINUTE;
  assert.equal(await session.token(), first, "fresh before halfway");
  assert.equal(api.calls, 1);
  clock.now += MINUTE;
  assert.notEqual(await session.token(), first, "renewed past halfway");
  assert.equal(api.calls, 2);
});

test("a guest stored without device times is judged by expires_at, as before", async () => {
  const { clock, storage, api, session } = setup();
  storage.set("lagdev.guest", JSON.stringify(guestExpiring(5, T0 + 60 * MINUTE)));
  assert.equal(await session.token(), "jwt-5");
  clock.now += 49 * MINUTE;
  assert.equal(await session.token(), "jwt-5");
  clock.now += 2 * MINUTE;
  assert.equal(await session.token(), "jwt-1");
  assert.equal(api.calls, 1);
});
