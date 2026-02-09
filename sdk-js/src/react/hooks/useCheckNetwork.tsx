import { useCallback, useEffect, useRef, useState } from "react";
import { useAPI } from "../context";
import { NetworkCheckResult } from "../../generated";

/**
 * Check network name availability with debounce
 */
export function useCheckNetwork(options?: {
  debounceMs?: number;
  minLength?: number;
}) {
  const api = useAPI();
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<Error | null>(null);
  const [result, setResult] = useState<NetworkCheckResult | null>(null);

  const debounceMs = options?.debounceMs ?? 300;
  const minLength = options?.minLength ?? 6;

  const timeoutRef = useRef<NodeJS.Timeout | null>(null);

  const checkNetwork = useCallback(
    async (params: { network_name: string }) => {
      if (params.network_name.length < minLength) {
        setResult(null);
        setError(null);
        setLoading(false);
        return null;
      }

      setLoading(true);
      setError(null);
      try {
        const result = await api.networkCheck(params);
        if (!result) {
          const err = new Error("Network check failed");
          setError(err);
          setResult(null);
          return null;
        }
        setResult(result);
        return result;
      } catch (err) {
        setError(err as Error);
        throw err;
      } finally {
        setLoading(false);
      }
    },
    [api, minLength],
  );

  const debouncedCheckNetwork = useCallback(
    (params: { network_name: string }) => {
      if (timeoutRef.current) {
        clearTimeout(timeoutRef.current);
      }

      if (params.network_name.length < minLength) {
        setResult(null);
        setError(null);
        setLoading(false);
        return;
      }

      timeoutRef.current = setTimeout(() => {
        checkNetwork(params);
      }, debounceMs);
    },
    [checkNetwork, debounceMs, minLength],
  );

  useEffect(() => {
    return () => {
      if (timeoutRef.current) {
        clearTimeout(timeoutRef.current);
      }
    };
  }, []);

  return {
    checkNetwork: debouncedCheckNetwork,
    loading,
    error,
    result,
  };
}
