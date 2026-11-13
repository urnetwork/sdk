# Durable client registration

`RegisterNetworkClientSyncWithContext` uses only `/network/register-client-v1` and exact caller-owned request bytes. Its caller must persist the opaque request and stable scope before the first request. The matching server migration and route must be deployed first; unsupported endpoints never fall back to legacy creation. Physical unknown replies can replay only that operation. Completed null, ambiguous, partial or changed-identity responses remain errors.

The shared refresh decoder validates bounded complete JSON before synchronous startup, callback users or the background token manager consume it. A mixed error never becomes confirmed revocation. The existing refreshed-client validator and auth generation still control token publication. `AddClientRefreshIntegrityListener` only reports an invalid completed refresh for the captured token/generation, without publishing, logging credentials or inventing logout authority. Callbacks run outside auth locks. The immutable notice exposes `CloseApiIfCurrent`, which atomically checks the original token/generation and requests nonjoining cancellation under the existing auth owner. A replacement between callback selection and delivery, including an equal-byte login, cannot be closed by the stale notice.

The current-main integration was qualified on 2026-10-01 with 100 top-level SDK registration, refresh, integrity-observer, token-manager and callback/auth-ownership tests in normal and race modes against the merged Connect source. `go vet ./...` also passed. These checks use synthetic local fixtures; no real client allocation or API deployment was performed.

Known ownership/verdict JSON tags require exact spelling, including nested error fields. Case-fold aliases cannot overwrite those fields through encoding/json; truly unknown refresh extensions stay compatible. Raw registration and refresh errors are classified across every leaf before wrapping unavailability or unsupported capability. The cumulative Connect dependency preserves actual exhausted request causes, so physical EOF/deadline can stay unavailable while a joined hard cause remains hard.

The endpoint-pinning successor selects Connect's request-scoped redirect refusal
only for versioned registration. A completed redirect is a typed API integrity
refusal, never a retry at another path/origin or a legacy allocation. Direct 3xx
statuses are classified without selecting one leaf from a mixed error tree;
joined local/physical failures retain their original causes. Unrelated clients
keep their redirect behavior. Canonical encoded request bytes must also fit the
server's 16 KiB physical body bound: decoded description/spec limits alone are
insufficient when JSON escaping expands those fields. Oversize is rejected before
HTTP, while an exactly bounded payload remains valid.

The endpoint-pinning and encoded-request-bound roots are included in that focused
normal/race qualification on the cumulative Connect dependency. The remaining
SDK-to-production-route, controller and database integration fixture is a separate
rollout requirement; local SDK HTTP tests and model/controller tests do not claim
that complete seam.
