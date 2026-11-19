// A sign-in handed from one app of the account to another, such as ur.io to
// the browser extension, by a sign-in code (server owner decision 2 of
// 2026-11-18): the giver creates a one-use code with its own sign-in
// (createSignInCode, POST /auth/code-create), and the receiver redeems it
// (redeemSignInCode, POST /auth/code-login) for a sign-in of its own, an
// independent session that the account's Sessions list shows and signs out
// on its own. Handing over the giver's token instead shares the giver's
// session: the giver's sign-out signed the receiver out too.
//
// The receiver then renews its own sign-in (renewNetworkToken, POST
// /auth/network-refresh) when its token reaches the point of its lifetime
// networkTokenRenewalTime names (the Go sdk's refresh window), since no giver
// pushes it a fresh token any more.

import type { URNetworkApiClient } from "./generated/client";
import { isURNetworkApiError } from "./api_client";
import { isSessionLimitReachedError } from "./session_limit";

/**
 * How long a handed-over sign-in code lives, in minutes: long enough for the
 * receiver to redeem it, short enough that a code that leaks is soon useless.
 */
export const SIGN_IN_CODE_HANDOFF_MINUTES = 5;

export type SignInCodeCreation =
  | { kind: "created"; authCode: string }
  /** the account holds its most sign-in codes; one ends when it expires */
  | { kind: "limit_exceeded"; message: string }
  /** any other refusal, in the server's words */
  | { kind: "refused"; message: string };

/**
 * Creates a one-use sign-in code with the client's sign-in (or `token`), for
 * another app of the account to redeem. A transport failure rejects with the
 * client's URNetworkApiError.
 */
export async function createSignInCode(
  client: URNetworkApiClient,
  options: { durationMinutes?: number; token?: string } = {},
): Promise<SignInCodeCreation> {
  const result = await client.authCodeCreate(
    { duration_minutes: options.durationMinutes ?? SIGN_IN_CODE_HANDOFF_MINUTES, uses: 1 },
    options.token ? { token: options.token } : undefined,
  );
  if (result.error) {
    const message = result.error.message ?? "";
    return result.error.auth_code_limit_exceeded
      ? { kind: "limit_exceeded", message }
      : { kind: "refused", message };
  }
  if (!result.auth_code) {
    return { kind: "refused", message: "" };
  }
  return { kind: "created", authCode: result.auth_code };
}

export type SignInCodeRedemption =
  | { kind: "signed_in"; byJwt: string }
  /** expired, used already, or not a code: a fresh code may work */
  | { kind: "refused" }
  /** the account holds its cap of active sessions (session_limit.ts) */
  | { kind: "session_limit_reached" };

/**
 * Redeems a sign-in code for a sign-in of this app's own. `requestId` makes
 * the redemption idempotent on the server: send the same id again only to
 * retry the same code after a lost answer, and a new one for each code. A
 * transport failure rejects with the client's URNetworkApiError, and the code
 * may still be good.
 */
export async function redeemSignInCode(
  client: URNetworkApiClient,
  authCode: string,
  requestId: string,
): Promise<SignInCodeRedemption> {
  let result;
  try {
    result = await client.authCodeLogin({ auth_code: authCode, request_id: requestId });
  } catch (error) {
    if (isSessionLimitReachedError(error)) {
      return { kind: "session_limit_reached" };
    }
    throw error;
  }
  if (result.error || !result.by_jwt) {
    return { kind: "refused" };
  }
  return { kind: "signed_in", byJwt: result.by_jwt };
}

export type NetworkTokenRenewal =
  | { kind: "renewed"; byJwt: string }
  /** the server ended this sign-in (revoked, signed out, expired): sign out */
  | { kind: "rejected" };

/**
 * Renews a network sign-in token (POST /auth/network-refresh): a fresh token
 * of the same network, user and session. A 401 is the sign-in's end. Any other
 * failure rejects, and the token stays as it is for a later try.
 */
export async function renewNetworkToken(
  client: URNetworkApiClient,
  byJwt: string,
): Promise<NetworkTokenRenewal> {
  let result;
  try {
    result = await client.authNetworkRefresh({ token: byJwt });
  } catch (error) {
    if (isURNetworkApiError(error) && error.kind === "http" && error.status === 401) {
      return { kind: "rejected" };
    }
    throw error;
  }
  if (result.error || !result.by_jwt) {
    // a refusal in the body is no proof the sign-in ended: try again later
    throw new Error(result.error?.message || "the network token was not renewed");
  }
  return { kind: "renewed", byJwt: result.by_jwt };
}

/** A token's numeric claim, in seconds, or undefined when it has none. */
function numericClaim(byJwt: string, name: string): number | undefined {
  const parts = byJwt.split(".");
  if (parts.length !== 3) {
    return undefined;
  }
  try {
    const base64 = parts[1].replace(/-/g, "+").replace(/_/g, "/");
    const payload = JSON.parse(atob(base64.padEnd(base64.length + ((4 - (base64.length % 4)) % 4), "="))) as Record<string, unknown>;
    const value = payload[name];
    return typeof value === "number" && Number.isFinite(value) ? value : undefined;
  } catch {
    return undefined;
  }
}

/**
 * When to renew a network sign-in token, in epoch milliseconds: a point
 * between 40% and 50% of its lifetime, from `iat` (from now without a sensible
 * one) to `exp`, picked by `draw` in [0, 1), as the Go sdk renews; now for a
 * token without a readable expiration or past it.
 */
export function networkTokenRenewalTime(byJwt: string, nowMs: number, draw: number): number {
  const exp = numericClaim(byJwt, "exp");
  if (exp === undefined || nowMs >= exp * 1000) {
    return nowMs;
  }
  const iat = numericClaim(byJwt, "iat");
  const start = iat !== undefined && iat < exp ? iat * 1000 : nowMs;
  const fraction = 0.4 + 0.1 * Math.min(Math.max(draw, 0), 1);
  return start + fraction * (exp * 1000 - start);
}
