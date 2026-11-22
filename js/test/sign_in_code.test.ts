import { test } from "node:test";
import assert from "node:assert/strict";
import { registerTsResolve } from "./ts-resolve.ts";

registerTsResolve();
const {
  createURNetworkApiClient,
  createSignInCode,
  redeemSignInCode,
  renewNetworkToken,
  networkTokenRenewalTime,
  SIGN_IN_CODE_HANDOFF_MINUTES,
} = await import("../src/client.ts");

const json = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

function recording(answers: Response[]) {
  const calls: { url: string; body: unknown; authorization: string | null }[] = [];
  const fetchImpl = async (input: RequestInfo | URL, init?: RequestInit) => {
    const headers = new Headers(init?.headers);
    calls.push({
      url: String(input),
      body: init?.body ? JSON.parse(String(init.body)) : undefined,
      authorization: headers.get("Authorization"),
    });
    return answers.shift() as Response;
  };
  return { calls, client: createURNetworkApiClient({ fetch: fetchImpl, retry: false }) };
}

// The giver's one-use code: its own sign-in creates it, and the refusals come
// back typed, never thrown.
test("a handed-over sign-in code is one use, short-lived, and its refusals are typed", async () => {
  const { calls, client } = recording([
    json(200, { auth_code: "code-1", duration_minutes: 5, uses: 1 }),
    json(200, { error: { auth_code_limit_exceeded: true, message: "too many codes" } }),
    json(200, { error: { message: "no" } }),
  ]);
  assert.deepEqual(await createSignInCode(client, { token: "giver-jwt" }), { kind: "created", authCode: "code-1" });
  assert.equal(calls[0].url.endsWith("/auth/code-create"), true);
  assert.deepEqual(calls[0].body, { duration_minutes: SIGN_IN_CODE_HANDOFF_MINUTES, uses: 1 });
  assert.equal(calls[0].authorization, "Bearer giver-jwt");
  assert.deepEqual(await createSignInCode(client, { token: "giver-jwt" }), { kind: "limit_exceeded", message: "too many codes" });
  assert.deepEqual(await createSignInCode(client, { token: "giver-jwt" }), { kind: "refused", message: "no" });
});

// The receiver's own sign-in: the code's redemption, with its request id for
// a retry of a lost answer, a refused or expired code and the session cap
// told apart, and transport failures thrown so the caller may retry the
// same code.
test("redeeming a code signs the receiver in on its own, and tells a refused code from the session cap", async () => {
  const { calls, client } = recording([
    json(200, { by_jwt: "receiver-jwt" }),
    json(200, { by_jwt: "", error: { message: "Invalid auth code." } }),
    json(409, { error: "Too many active sessions.", code: "session_limit_reached" }),
    json(503, { error: "Authentication temporarily unavailable." }),
  ]);
  assert.deepEqual(await redeemSignInCode(client, "code-1", "request-1"), { kind: "signed_in", byJwt: "receiver-jwt" });
  assert.equal(calls[0].url.endsWith("/auth/code-login"), true);
  assert.deepEqual(calls[0].body, { auth_code: "code-1", request_id: "request-1" });
  assert.deepEqual(await redeemSignInCode(client, "code-2", "request-2"), { kind: "refused" });
  assert.deepEqual(await redeemSignInCode(client, "code-3", "request-3"), { kind: "session_limit_reached" });
  await assert.rejects(redeemSignInCode(client, "code-4", "request-4"));
});

// The receiver renews its own token: a 401 ends the sign-in, anything else
// leaves the token for a later try.
test("renewing a network token ends the sign-in only on a 401", async () => {
  const { calls, client } = recording([
    json(200, { by_jwt: "renewed-jwt" }),
    json(401, { error: "Session request could not be completed.", code: "session_revoked" }),
    json(503, { error: "Authentication temporarily unavailable." }),
    json(200, { error: { message: "not now" } }),
  ]);
  assert.deepEqual(await renewNetworkToken(client, "old-jwt"), { kind: "renewed", byJwt: "renewed-jwt" });
  assert.equal(calls[0].url.endsWith("/auth/network-refresh"), true);
  assert.equal(calls[0].authorization, "Bearer old-jwt");
  assert.deepEqual(await renewNetworkToken(client, "old-jwt"), { kind: "rejected" });
  await assert.rejects(renewNetworkToken(client, "old-jwt"));
  await assert.rejects(renewNetworkToken(client, "old-jwt"));
});

function token(claims: Record<string, unknown>): string {
  const encode = (value: unknown) =>
    btoa(JSON.stringify(value)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  return `${encode({ alg: "none" })}.${encode(claims)}.signature`;
}

// The Go sdk's window: between 40% and 50% of the lifetime from iat, at once
// without an expiration or past it.
test("a network token renews between 40% and 50% of its lifetime", () => {
  const day = 24 * 3600;
  const iat = 1_800_000_000;
  const jwt = token({ iat, exp: iat + 30 * day });
  const now = (iat + day) * 1000;
  assert.equal(networkTokenRenewalTime(jwt, now, 0), (iat + 12 * day) * 1000);
  assert.equal(networkTokenRenewalTime(jwt, now, 1), (iat + 15 * day) * 1000);
  assert.equal(networkTokenRenewalTime(token({ iat }), now, 0.5), now, "no expiration: at once");
  assert.equal(networkTokenRenewalTime(jwt, (iat + 31 * day) * 1000, 0.5), (iat + 31 * day) * 1000, "expired: at once");
  assert.equal(networkTokenRenewalTime("not a token", now, 0.5), now);
  const noIat = token({ exp: iat + 30 * day });
  assert.equal(networkTokenRenewalTime(noIat, now, 0), now + 0.4 * ((iat + 30 * day) * 1000 - now));
});
