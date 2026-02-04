import { useState, useEffect, useCallback, useRef } from "react";
import { URNetwork } from "../index";
import { useAPI } from "./context";
import type {
  // ApiResult,
  // ProxyDevice,
  // ProxyConfig,
  // ProxyConfigResult,
  // SetupDeviceCallback,
  InitOptions,
} from "../types";
import {
  AuthVerifyResult,
  // AuthLoginArgs,
  // AuthLoginWithPasswordArgs,
  NetworkCheckResult,
  // NetworkCreateArgs,
} from "../generated";

/**
 * Hook to initialize the URnetwork SDK
 */
export function useURNetwork(options?: InitOptions) {
  const [sdk, setSdk] = useState<URNetwork | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<Error | null>(null);

  useEffect(() => {
    let mounted = true;

    const initSdk = async () => {
      try {
        const instance = await URNetwork.init(options);
        if (mounted) {
          setSdk(instance);
          setLoading(false);
        }
      } catch (err) {
        if (mounted) {
          setError(err as Error);
          setLoading(false);
        }
      }
    };

    initSdk();

    return () => {
      mounted = false;
    };
  }, []);

  return { sdk, loading, error };
}

/**
 * Hook to create and manage a proxy device
 */
// export function useProxyDevice(options: UseProxyDeviceOptions = {}) {
//   // ... existing implementation ...
// }

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

      // This is the correct place for setLoading(true)
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

      // REMOVED: setLoading(true);
      // Removing this prevents the input from disabling (and losing focus) while the user is still typing.

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

export function useVerifyUserAuth() {
  const api = useAPI();
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<Error | null>(null);

  const verifyUserAuth = useCallback(
    async (userAuth: string, code: string): Promise<AuthVerifyResult> => {
      setLoading(true);
      setError(null);
      try {
        const result = await api.verifyUserAuth({
          user_auth: userAuth,
          verify_code: code,
        });

        return result;
      } catch (err) {
        setError(err as Error);
        return {
          error: {
            message: err instanceof Error ? err.message : "Verification failed",
          },
        };
      } finally {
        setLoading(false);
      }
    },
    [api],
  );

  return {
    verifyUserAuth,
    loading,
    error,
  };
}
