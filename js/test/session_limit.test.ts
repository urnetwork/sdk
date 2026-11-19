import { test } from "node:test";
import assert from "node:assert/strict";
import { registerTsResolve } from "./ts-resolve.ts";

registerTsResolve();
const {
  createURNetworkApiClient,
  isSessionLimitReachedError,
  SESSION_LIMIT_REACHED_CODE,
} = await import("../src/client.ts");

const json = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

// The refusal the server sends at the cap reaches a screen as the rejection
// of a generated client call, and only its code is the refusal.
test("a 409 session_limit_reached rejection is the session cap refusal", async () => {
  const answers = [
    json(409, { error: "Too many active sessions.", code: "session_limit_reached" }),
    json(409, { error: { code: "session_limit_reached", message: "m" } }),
    json(409, { error: "Session request could not be completed.", code: "session_retention_limit_reached" }),
    json(400, { code: "session_limit_reached" }),
    new Response("session_limit_reached", { status: 409 }),
  ];
  const fetchImpl = async () => answers.shift() as Response;
  const client = createURNetworkApiClient({ fetch: fetchImpl, retry: false });
  const refusals: boolean[] = [];
  for (let i = 0; i < 5; i++) {
    try {
      await client.authCodeLogin({ auth_code: "code" });
      refusals.push(false);
    } catch (error) {
      refusals.push(isSessionLimitReachedError(error));
    }
  }
  assert.deepEqual(refusals, [true, true, false, false, false]);
  assert.equal(SESSION_LIMIT_REACHED_CODE, "session_limit_reached");
  // a legacy method's resolved result, and nothing else
  assert.equal(isSessionLimitReachedError({ error: { code: "session_limit_reached", message: "m" } }), true);
  assert.equal(isSessionLimitReachedError(new Error("session_limit_reached")), false);
  assert.equal(isSessionLimitReachedError(null), false);
});
