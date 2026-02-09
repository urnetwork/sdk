import { useState, useEffect, useCallback, useRef } from "react";
import { URNetwork } from "../index";
import { useAPI, useAuth } from "./context";
import type { InitOptions } from "../types";
import {
  AuthCodeLoginResult,
  AuthNetworkClientArgs,
  AuthNetworkClientResult,
  AuthVerifyResult,
  ConnectLocation,
  FilteredLocations,
  FindLocationsArgs,
  FindLocationsResult,
  NetworkCheckResult,
} from "../generated";

const tokenRequiredError = "Admin JWT is required for verification";

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

export function useVerifyUserAuth() {
  const api = useAPI();
  const { token } = useAuth();
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<Error | null>(null);

  const verifyUserAuth = useCallback(
    async (userAuth: string, code: string): Promise<AuthVerifyResult> => {
      if (!token) {
        const err = new Error(tokenRequiredError);
        setError(err);
        return {
          error: {
            message: err.message,
          },
        };
      }

      setLoading(true);
      setError(null);
      try {
        const result = await api.verifyUserAuth(
          {
            user_auth: userAuth,
            verify_code: code,
          },
          token,
        );

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

export function useAuthCodeLogin() {
  const api = useAPI();
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<Error | null>(null);

  const authCodeLogin = useCallback(
    async (authCode: string): Promise<AuthCodeLoginResult> => {
      setLoading(true);
      setError(null);
      try {
        const result = await api.authCodeLogin({
          auth_code: authCode,
        });

        if (result.error) {
          const err = new Error(result.error.message);
          setError(err);
        }

        return result;
      } catch (err) {
        setError(err as Error);
        return {
          by_jwt: "",
          error: {
            message:
              err instanceof Error ? err.message : "Auth code login failed",
          },
        };
      } finally {
        setLoading(false);
      }
    },
    [api],
  );

  return { loading, error, authCodeLogin };
}

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

/**
 * provider list hook
 */

const initialFilteredLocations: FilteredLocations = {
  best_matches: [],
  promoted: [],
  countries: [],
  cities: [],
  regions: [],
  devices: [],
};

export function useProviderList() {
  const api = useAPI();
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<Error | null>(null);
  const [filteredLocations, setFilteredLocations] = useState<FilteredLocations>(
    initialFilteredLocations,
  );
  const [query, setQuery] = useState("");

  const debounceMs = 500;
  const timeoutRef = useRef<NodeJS.Timeout | null>(null);

  const filterLocations = (result: FindLocationsResult): FilteredLocations => {
    const bestMatch: ConnectLocation[] = [];
    const promoted: ConnectLocation[] = [];
    const countries: ConnectLocation[] = [];
    const cities: ConnectLocation[] = [];
    const regions: ConnectLocation[] = [];
    const devices: ConnectLocation[] = [];

    // Process groups
    if (result.groups) {
      for (const groupResult of result.groups) {
        const location: ConnectLocation = {
          connect_location_id: {
            location_group_id: groupResult.location_group_id,
          },
          name: groupResult.name,
          provider_count: groupResult.provider_count,
          promoted: groupResult.promoted,
          match_distance: groupResult.match_distance,
          stable: false,
          strong_privacy: false,
        };

        if (groupResult.match_distance === 0 && query !== "") {
          bestMatch.push(location);
        } else if (groupResult.promoted) {
          promoted.push(location);
        }
      }
    }

    // Process locations
    if (result.locations) {
      for (const locationResult of result.locations) {
        const location: ConnectLocation = {
          connect_location_id: {
            location_id: locationResult.location_id,
          },
          location_type: locationResult.location_type,
          name: locationResult.name,
          city: locationResult.city,
          region: locationResult.region,
          country: locationResult.country,
          country_code: locationResult.country_code,
          city_location_id: locationResult.city_location_id,
          region_location_id: locationResult.region_location_id,
          country_location_id: locationResult.country_location_id,
          provider_count: locationResult.provider_count,
          match_distance: locationResult.match_distance,
          stable: locationResult.stable,
          strong_privacy: locationResult.strong_privacy,
        };

        if (location.match_distance === 0 && query !== "") {
          bestMatch.push(location);
        } else {
          if (location.location_type === "country") {
            countries.push(location);
          }

          // only show cities when searching
          if (location.location_type === "city" && query !== "") {
            cities.push(location);
          }

          // only show regions when searching
          if (location.location_type === "region" && query !== "") {
            regions.push(location);
          }
        }
      }
    }

    // Process devices
    if (result.devices) {
      for (const locationDeviceResult of result.devices) {
        const location: ConnectLocation = {
          connect_location_id: {
            client_id: locationDeviceResult.client_id,
          },
          name: locationDeviceResult.device_name,
          stable: false,
          strong_privacy: false,
        };
        devices.push(location);
      }
    }

    // Comparison function for sorting
    const cmpConnectLocations = (
      a: ConnectLocation,
      b: ConnectLocation,
    ): number => {
      // Match distance priority (0 or 1 are considered close matches)
      const aCloseMatch = (a.match_distance ?? 0) <= 1;
      const bCloseMatch = (b.match_distance ?? 0) <= 1;
      if (aCloseMatch !== bCloseMatch) {
        return aCloseMatch ? -1 : 1;
      }

      // Provider count descending
      const aProviderCount = a.provider_count ?? 0;
      const bProviderCount = b.provider_count ?? 0;
      if (aProviderCount !== bProviderCount) {
        return bProviderCount - aProviderCount;
      }

      // Name ascending
      const aName = a.name ?? "";
      const bName = b.name ?? "";
      if (aName !== bName) {
        return aName.localeCompare(bName);
      }

      // Compare location IDs as a final tiebreaker
      const aId = JSON.stringify(a.connect_location_id);
      const bId = JSON.stringify(b.connect_location_id);
      return aId.localeCompare(bId);
    };

    // Sort all arrays
    bestMatch.sort(cmpConnectLocations);
    promoted.sort(cmpConnectLocations);
    countries.sort(cmpConnectLocations);
    cities.sort(cmpConnectLocations);
    regions.sort(cmpConnectLocations);

    return {
      best_matches: bestMatch,
      promoted: promoted,
      countries: countries,
      cities: cities,
      regions: regions,
      devices: devices,
    };
  };

  const getProviders = useCallback(
    async (search: string): Promise<void> => {
      setLoading(true);
      setError(null);

      try {
        let result;
        if (search.length <= 0) {
          result = await api.networkProviderLocations();
        } else {
          const params: FindLocationsArgs = {
            query: search,
          };
          result = await api.searchProviderLocations(params);
        }

        const filteredLocations = filterLocations(result);
        setFilteredLocations(filteredLocations);
      } catch (error) {
        setError(error as Error);
        setFilteredLocations(initialFilteredLocations);
      } finally {
        setLoading(false);
      }
    },
    [api],
  );

  // Trigger debounced search whenever query changes
  useEffect(() => {
    // Clear existing timeout
    if (timeoutRef.current) {
      clearTimeout(timeoutRef.current);
    }

    // Set new timeout
    timeoutRef.current = setTimeout(() => {
      getProviders(query);
    }, debounceMs);

    // Cleanup function
    return () => {
      if (timeoutRef.current) {
        clearTimeout(timeoutRef.current);
      }
    };
  }, [query, getProviders]);

  return {
    loading,
    error,
    filteredLocations,
    query,
    setQuery,
  };
}

export function useRemoveNetworkClient() {
  const api = useAPI();
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<Error | null>(null);
  const { token } = useAuth();

  const removeNetworkClient = useCallback(
    async (clientId: string) => {
      if (!token) {
        const err = new Error(tokenRequiredError);
        setError(err);
        return {
          error: {
            message: err.message,
          },
        };
      }

      setLoading(true);
      setError(null);
      try {
        const result = await api.removeNetworkClient(
          {
            client_id: clientId,
          },
          token,
        );

        return result;
      } catch (err) {
        setError(err as Error);
        return {
          error: {
            message:
              err instanceof Error
                ? err.message
                : "Remove network client failed",
          },
        };
      } finally {
        setLoading(false);
      }
    },
    [api],
  );

  return {
    loading,
    error,
    removeNetworkClient,
  };
}
