// The POST /network/auth-client calls of one owner (a useAuthNetworkClient
// instance), where a newer call supersedes the older ones and the owner may go
// away while a call is in flight.
//
// A create is not idempotent, and aborting its request does not stop it: the
// server still creates the client (and, for a proxy config, the hosted proxy),
// and its id is lost with the response. The hook used to abort the previous
// call on every new one and every call on unmount, which left that client
// active with no one holding it: a hosted proxy kept a device on a proxy host
// and counted against the network's active proxy cap until the idle reap. So a
// superseded or orphaned call is never aborted; when it answers, the client it
// created is removed, and its caller gets the cancelled answer it got before.
//
// Not safe for concurrent use from more than one thread of control; the hook
// calls it from React's single thread.

import type { AuthNetworkClientResult } from "./generated";
import { parseByJwtClientId } from "./utils/jwt";

export const authNetworkClientCancelledMessage = "Request was cancelled";

export interface AuthNetworkClientCallOutcome {
  /** false: superseded or orphaned; the result is the cancelled answer */
  current: boolean;
  result: AuthNetworkClientResult;
}

export class AuthNetworkClientCalls {
  private generation = 0;
  private open = true;

  /**
   * Runs one create. `create` performs the request, `release` removes a
   * client by id. A call that is still the newest, on an open owner, when it
   * answers is current and gets the result; any other call's created client is
   * released (best effort) and the call gets the cancelled answer. A current
   * call's failure rejects; a superseded one's is the cancelled answer.
   */
  async run(
    create: () => Promise<AuthNetworkClientResult>,
    release: (clientId: string) => Promise<unknown>,
  ): Promise<AuthNetworkClientCallOutcome> {
    const generation = ++this.generation;
    let result: AuthNetworkClientResult;
    try {
      result = await create();
    } catch (error) {
      if (this.isCurrent(generation)) throw error;
      return { current: false, result: cancelledResult() };
    }
    if (this.isCurrent(generation)) {
      return { current: true, result };
    }
    const clientId = createdClientId(result);
    if (clientId) {
      try {
        await release(clientId);
      } catch (error) {
        console.warn("Could not remove a superseded auth-client client:", error);
      }
    }
    return { current: false, result: cancelledResult() };
  }

  /** the owner is (again) present: answers from now on reach their callers */
  reopen(): void {
    this.open = true;
  }

  /** the owner went away: calls in flight release what they create */
  close(): void {
    this.open = false;
  }

  private isCurrent(generation: number): boolean {
    return this.open && generation === this.generation;
  }
}

export function cancelledResult(): AuthNetworkClientResult {
  return {
    proxy_config_result: null,
    error: {
      message: authNetworkClientCancelledMessage,
      client_limit_exceeded: false,
    },
  };
}

// The client a result created: its client_id, else its by_client_jwt's
// client_id claim, or null for a refusal or an answer without one.
function createdClientId(result: AuthNetworkClientResult): string | null {
  if (result.error) return null;
  if (typeof result.client_id === "string" && result.client_id !== "") return result.client_id;
  if (!result.by_client_jwt) return null;
  try {
    return parseByJwtClientId(result.by_client_jwt);
  } catch {
    return null;
  }
}
