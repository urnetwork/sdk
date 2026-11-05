import { useCallback, useEffect, useRef, useState } from "react";
import { useAPI, useAuth } from "../context";
import {
  AuthNetworkClientArgs,
  AuthNetworkClientResult,
} from "../../generated";
import { AuthNetworkClientCalls } from "../../auth_network_client_calls";
import { tokenRequiredError } from "./constants";

// A newer call supersedes an older one still in flight, and unmounting
// orphans the calls in flight; neither aborts the request, because the server
// creates the client either way. A superseded or orphaned call's client is
// removed when it answers, and the call answers "Request was cancelled", as it
// did (auth_network_client_calls.ts).
export function useAuthNetworkClient() {
  const api = useAPI();
  const { token } = useAuth();
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<Error | null>(null);
  const callsRef = useRef<AuthNetworkClientCalls | null>(null);
  if (callsRef.current === null) {
    callsRef.current = new AuthNetworkClientCalls();
  }

  const authNetworkClient = useCallback(
    async (params: AuthNetworkClientArgs): Promise<AuthNetworkClientResult> => {
      if (!token) {
        const err = new Error(tokenRequiredError);
        setError(err);
        return {
          proxy_config_result: null,
          error: {
            message: err.message,
            client_limit_exceeded: false,
          },
        };
      }

      setLoading(true);
      setError(null);

      try {
        const outcome = await callsRef.current!.run(
          () => api.authNetworkClient(params, token),
          (clientId) => api.removeNetworkClient({ client_id: clientId }, token),
        );
        if (outcome.current) {
          setLoading(false);
        }
        return outcome.result;
      } catch (err) {
        setLoading(false);
        setError(err as Error);
        return {
          proxy_config_result: null,
          error: {
            message:
              err instanceof Error ? err.message : "Auth network client failed",
            client_limit_exceeded: false,
          },
        };
      }
    },
    [api, token],
  );

  // the owner is present while mounted; a remount (StrictMode) reopens it
  useEffect(() => {
    const calls = callsRef.current!;
    calls.reopen();
    return () => calls.close();
  }, []);

  return {
    loading,
    error,
    authNetworkClient,
  };
}
