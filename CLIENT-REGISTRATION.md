# Durable client registration

`RegisterNetworkClientSyncWithContext` uses only `/network/register-client-v1` and exact caller-owned request bytes. Its caller must persist the opaque request and stable scope before the first request. The matching server migration and route must be deployed first; unsupported endpoints never fall back to legacy creation. Physical unknown replies can replay only that operation. Completed null, ambiguous, partial or changed-identity responses remain errors.

The shared refresh decoder validates bounded complete JSON before synchronous startup, callback users or the background token manager consume it. A mixed error never becomes confirmed revocation. The existing refreshed-client validator and auth generation still control token publication. `AddClientRefreshIntegrityListener` only reports an invalid completed refresh for the captured token/generation, without publishing, logging credentials or inventing logout authority. Callbacks run outside auth locks and may use nonjoining `Api.Close` to stop dependent API work.

Source qualification is pending. New registration, refresh and integrity-observer roots need normal/race runs alongside the existing token-manager and callback ownership tests. Compile-only author checks do not qualify behavior. No real client allocation or API deployment was performed.
