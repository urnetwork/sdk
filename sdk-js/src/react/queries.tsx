/**
 * TODO - deprecate in favor of query-options
 */

// import { useMutation, useQuery } from "@tanstack/react-query";
// import { useAPI, useAuth } from "./context";
// import { useEffect, useState } from "react";

/**
 * Mutation hook to verify a user's authentication code.
 *
 * This is used after a user signs up with an email or phone number.
 * The backend sends them a verification code (via email or SMS).
 * This hook takes the code and verifies the user's authentication.
 *
 * Requires an admin JWT (from context) to authorize the verification request.
 *
 * @returns
 *   - mutate / mutateAsync: Call with `{ userAuth, code }` to verify the user
 *   - isPending: Boolean indicating if the mutation is in progress
 *   - error: Any error encountered during verification
 *
 * @example
 * ```tsx
 * const { mutateAsync: verifyUserAuth, isPending, error } = useVerifyUserAuth();
 * // ...
 * await verifyUserAuth({ userAuth: "user@email.com", code: "123456" });
 * ```
 */
// export function useVerifyUserAuth() {
//   const api = useAPI();
//   const { token } = useAuth();

//   return useMutation({
//     mutationFn: async ({
//       userAuth,
//       code,
//     }: {
//       userAuth: string;
//       code: string;
//     }) => {
//       if (!token) {
//         throw new Error("Admin JWT is required for verification");
//       }
//       return api.verifyUserAuth(
//         { user_auth: userAuth, verify_code: code },
//         token,
//       );
//     },
//   });
// }

/**
 * Fetch provider locations with optional search query.
 * Includes automatic debouncing to prevent excessive API calls.
 *
 * @param search - Search query string
 * @param options - Configuration options
 * @param options.debounceMs - Debounce delay in milliseconds (default: 300)
 * @param options.enabled - Enable/disable the query (default: true)
 *
 * @example
 * ```tsx
 * const [query, setQuery] = useState("");
 * const { data, isLoading, error } = useProviderLocations(query);
 *
 * return <input value={query} onChange={(e) => setQuery(e.target.value)} />
 * ```
 */
// export function useProviderLocations(
//   search?: string,
//   options?: {
//     debounceMs?: number;
//     enabled?: boolean;
//   },
// ) {
//   const api = useAPI();
//   const [debouncedSearch, setDebouncedSearch] = useState(search);

//   const debounceMs = options?.debounceMs ?? 300;
//   const enabled = options?.enabled ?? true;

//   // Debounce the search input
//   useEffect(() => {
//     const handler = setTimeout(() => {
//       setDebouncedSearch(search);
//     }, debounceMs);

//     return () => clearTimeout(handler);
//   }, [search, debounceMs]);

//   return useQuery({
//     queryKey: ["provider-locations", debouncedSearch || ""],
//     queryFn: async () => {
//       if (!debouncedSearch || debouncedSearch.length === 0) {
//         return api.networkProviderLocations();
//       }
//       return api.searchProviderLocations({ query: debouncedSearch });
//     },
//     enabled,
//     staleTime: 1 * 60 * 1000,
//     gcTime: 5 * 60 * 1000,
//     retry: 1,
//   });
// }
