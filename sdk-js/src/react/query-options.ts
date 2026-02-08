// NEW FILE - query-options.ts
import type { URNetworkAPI } from "../api";

/**
 * Query options for checking network name availability.
 * Use with TanStack Query's useQuery, useSuspenseQuery, or prefetching.
 *
 * @example
 * ```tsx
 * import { useQuery } from '@tanstack/react-query';
 * import { useAPI } from '@urnetwork/sdk-js/react';
 * import { networkCheckQueryOptions } from '@urnetwork/sdk-js/react/query-options';
 *
 * const api = useAPI();
 * const query = useQuery(networkCheckQueryOptions(api, "mynetwork"));
 * ```
 */
export function networkCheckQueryOptions(
  api: URNetworkAPI,
  networkName: string,
) {
  return {
    queryKey: ["urnetwork", "network-check", networkName] as const,
    queryFn: () => api.networkCheck({ network_name: networkName }),
    enabled: networkName.length >= 6,
    staleTime: 30 * 1000, // 30 seconds
    retry: 1,
  };
}

/**
 * Query options for fetching provider locations with optional search.
 *
 * @example
 * ```tsx
 * import { useQuery } from '@tanstack/react-query';
 * import { providerLocationsQueryOptions } from '@urnetwork/sdk-js/react/query-options';
 *
 * const api = useAPI();
 * const query = useQuery(providerLocationsQueryOptions(api, "New York"));
 * ```
 */
export function providerLocationsQueryOptions(
  api: URNetworkAPI,
  search?: string,
) {
  return {
    queryKey: ["urnetwork", "provider-locations", search || ""] as const,
    queryFn: async () => {
      if (!search || search.length === 0) {
        return api.networkProviderLocations();
      }
      return api.searchProviderLocations({ query: search });
    },
    staleTime: 5 * 60 * 1000, // 5 minutes
    gcTime: 10 * 60 * 1000, // 10 minutes
    retry: 1,
  };
}

/**
 * Mutation options for verifying user authentication code.
 *
 * @example
 * ```tsx
 * import { useMutation } from '@tanstack/react-query';
 * import { verifyUserAuthMutationOptions } from '@urnetwork/sdk-js/react/query-options';
 *
 * const api = useAPI();
 * const { token } = useAuth();
 * const mutation = useMutation(verifyUserAuthMutationOptions(api, token));
 *
 * await mutation.mutateAsync({ userAuth: "user@email.com", code: "123456" });
 * ```
 */
export function verifyUserAuthMutationOptions(
  api: URNetworkAPI,
  token: string,
) {
  return {
    mutationFn: async ({
      userAuth,
      code,
    }: {
      userAuth: string;
      code: string;
    }) => {
      if (!token) {
        throw new Error("Admin JWT is required for verification");
      }
      return api.verifyUserAuth(
        { user_auth: userAuth, verify_code: code },
        token,
      );
    },
  };
}
