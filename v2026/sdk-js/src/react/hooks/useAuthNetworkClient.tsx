import { useCallback, useEffect, useRef, useState } from "react";
import { useAPI, useAuth } from "../context";
import {
  AuthNetworkClientArgs,
  AuthNetworkClientResult,
} from "../../generated";
import { tokenRequiredError } from "./constants";

export function useAuthNetworkClient() {
  const api = useAPI();
  const { token } = useAuth();
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<Error | null>(null);
  const abortControllerRef = useRef<AbortController | null>(null);

  const authNetworkClient = useCallback(
    async (params: AuthNetworkClientArgs): Promise<AuthNetworkClientResult> => {
      // Cancel previous request if it exists
      if (abortControllerRef.current) {
        abortControllerRef.current.abort();
      }

      // Create new abort controller for this request
      const abortController = new AbortController();
      abortControllerRef.current = abortController;

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
        const result = await api.authNetworkClient(
          params,
          token,
          abortController.signal,
        );

        // Check if this request was aborted
        if (abortController.signal.aborted) {
          return {
            proxy_config_result: null,
            error: {
              message: "Request was cancelled",
              client_limit_exceeded: false,
            },
          };
        }

        return result;
      } catch (err) {
        // Ignore abort errors
        if (err instanceof Error && err.name === "AbortError") {
          return {
            proxy_config_result: null,
            error: {
              message: "Request was cancelled",
              client_limit_exceeded: false,
            },
          };
        }

        setError(err as Error);
        return {
          proxy_config_result: null,
          error: {
            message:
              err instanceof Error ? err.message : "Auth code login failed",
            client_limit_exceeded: false,
          },
        };
      } finally {
        // Only set loading to false if this controller wasn't aborted
        if (!abortController.signal.aborted) {
          setLoading(false);
        }
      }
    },
    [api, token],
  );

  // Cleanup on unmount
  useEffect(() => {
    return () => {
      if (abortControllerRef.current) {
        abortControllerRef.current.abort();
      }
    };
  }, []);

  return {
    loading,
    error,
    authNetworkClient,
  };
}
