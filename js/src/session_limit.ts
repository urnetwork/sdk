// The server's refusal of a new session for a network that holds its cap of
// active sessions (the Go sdk's api_session_limit.go, server owner decisions
// 6 and 7 of 2026-11-18): a sign-in, a code sign-in or a new client whose
// session would go over the cap is answered HTTP 409 with the body
// {"code":"session_limit_reached","error":"..."}. Nothing is signed out to
// make room. The user frees a session by signing out the ones they no longer
// use in Account > Sessions, on a device that is signed in.
//
// The generated client rejects that answer with a URNetworkApiError; a
// screen asks isSessionLimitReachedError and says there are too many active
// sessions in its own words. A 409 with any other code is not this refusal:
// session_retention_limit_reached, the cap of revoked sessions the server
// still remembers, is not freed by signing sessions out.

import { isURNetworkApiError } from "./api_client";

/** The code of the refusal (Go `SessionErrorCodeLimitReached`). */
export const SESSION_LIMIT_REACHED_CODE = "session_limit_reached";

/**
 * Whether a failure is the session cap refusal: an `http` URNetworkApiError
 * with status 409 whose body carries the code, at the top level as the
 * server's session errors have it, or in an `error` object as a result's
 * error has it. A result-shaped value (`{ error: { code } }`) that a legacy
 * method resolved with counts too.
 */
export function isSessionLimitReachedError(error: unknown): boolean {
  const hasCode = (body: unknown): boolean => {
    if (typeof body !== "object" || body === null) {
      return false;
    }
    if ((body as { code?: unknown }).code === SESSION_LIMIT_REACHED_CODE) {
      return true;
    }
    const nested = (body as { error?: unknown }).error;
    return typeof nested === "object" && nested !== null &&
      (nested as { code?: unknown }).code === SESSION_LIMIT_REACHED_CODE;
  };
  if (isURNetworkApiError(error)) {
    return error.kind === "http" && error.status === 409 && hasCode(error.body);
  }
  return !(error instanceof Error) && hasCode(error);
}
