# AUTHZ-CLIENT: which credential the SDK sends

The server refuses a client token on the routes that administer the network or
the account (`server/api/route_authz.go`, `server/AUTHZ1.md`). Until this
change, a device installed its client token (`by_client_jwt`) as the `Api`'s
only credential. After that, every SDK call, account administration included,
carried the client token. The apps passed only because the 27 App admin routes
still accept a client token from an ordinary network.

## What the SDK does now

- **Two credentials in one `Api`.** The current credential is `GetByJwt()`,
  which is the device's client token once a device starts. The network
  credential is the network's sign-in token (a `by_jwt` with no `client_id`)
  or an API key (`urn_…`). It is kept beside the client token
  (`api_network_credential.go`).
- **Where the network credential comes from.**
  - `SetByJwt(networkCredential)` at sign-in sets it.
  - After a relaunch, the device adopts the `by_jwt` that its LocalState
    pairs with the client, when the `Api` holds none.
  - The SDK never adopts a client token as the network credential. Shipped
    Apple extensions stored a client token as `by_jwt`, and that value is
    ignored.
  - It never keeps another network's credential.
- **What keeps it and what ends it.** It survives device start, client token
  refresh and device close, and a renewal replaces it with the renewed token.
  It ends when:
  - `SetByJwt` is called with any other value: sign-out (`""`), a client token
    (which replaces the session), or another network's token;
  - the server confirms that it rejects the client token, which signs the app
    out;
  - the server rejects the network token itself when it is renewed (401).
    That ends only the network credential, as described under Renewal.

  A hosted device's session `Api` never receives a network credential.
- **Renewal.** `/auth/refresh` refreshes only client tokens ("Client ID is
  required"), and the SDK refreshes the client token exactly as before. The
  network token renews at `POST /auth/network-refresh`
  (`api_network_credential_renewal.go`), a Network only route.
  - **Which `Api` renews.** An `Api` renews the network token it keeps beside
    a device's client token when the LocalState that device started from
    stores exactly that token. That is the apps' `Api`:
    - Android: the network space's `Api`, which its in-process DeviceLocal
      and every account screen use.
    - Apple: the app's network space `Api`, which its DeviceRemote and every
      account screen use. The network extension has its own LocalState and
      never stores a network token (shipped extensions kept a client token in
      `by_jwt`), so it never renews.

    Nothing else renews: ur.io's account host (ur.io keeps and renews its own
    token), a headless backend, and a hosted device's session `Api` have no
    LocalState behind a network token. One `Api` has one renewer.
  - **When.** At the token's half-life, and never sooner than 5 minutes. A
    token with no `exp` (a legacy token) or an expired one renews at once.
    That works while the server still accepts it (vault `auth.yml`
    `reject_missing_expiration` and `reject_expired` off), which is how the
    installed base moves to renewable tokens before those gates flip. An API
    key never renews: it does not expire, and the server refuses it.
  - **Persisted.** The renewal replaces `by_jwt` in LocalState with a
    compare-and-swap that keeps the client token, the instance and the device
    owner. A sign-out or a new sign-in that races it wins, and the renewal is
    discarded. A relaunch adopts the renewed token. When another `Api` sharing
    the LocalState renewed first, this one keeps that token instead.
  - **Outcomes.**
    - A 401 drops the network credential from the `Api` only:
      `HasNetworkCredential()` goes false, the device keeps its client token,
      LocalState keeps both tokens, and the app is not signed out. The `Api`
      does not adopt the rejected token again; a new sign-in starts over.
    - A refusal in the result, or a renewal that names another identity,
      stops renewal and keeps the token.
    - Transient failures back off: 10 seconds plus jitter that doubles from 30
      seconds to 15 minutes. A device transport that becomes usable ends the
      wait of a failed renewal.
    - Another 4xx (a server that predates the route answers 404) waits a day.
    - A renewal that answers a token already due (no `exp`, or a skewed clock)
      is spaced from 5 minutes, doubling to 7 days.
- **How a credential is chosen.** The choice is made at the request seam
  (`getHttpPostRaw`, `getHttpGetRaw`, `getHttpPostStreamRaw`), from the method
  and path. `apiAdminRouteAccess` mirrors the server's App admin and Network
  only classes.
  - On an admin route, the request carries the network credential.
  - With no network credential, the call fails with
    `ErrNetworkCredentialRequired`, and the message names the route. The
    request is not sent, and the SDK never falls back to the client token.
  - Every other route carries the current credential, unchanged.
  - The network credential is never sent to a URL outside the API.
- **New public API.**
  - `Api.HasNetworkCredential() bool`, on gomobile and in the C ABI as
    `urnet_api_has_network_credential`.
  - The Go error `ErrNetworkCredentialRequired`.

  Nothing existing changed signature.

## Call → credential

The "before" column describes an app with a running device.

| Server class | SDK calls | Before | Now |
|---|---|---|---|
| App admin (27; the SDK calls all of them) | `NetworkDelete`, `AuthCodeCreate`, `AddAuth`, `RemoveAuth`, `GenerateSeedphrase`, `RegenerateSeedphrase`, `GetNetworkClients` (DevicesViewController), `GetNetworkUser` (NetworkUserViewController), `SetNetworkLeaderboardPublic`, `SetPointsLeaderboardPublic`, `SetEmojiTag`, `NetworkBlockLocation`, `NetworkUnblockLocation`, `AccountPreferencesUpdate` (AccountPreferencesViewController), `StripeCreateCustomerPortal`, `SetPayoutWallet`, `GetPayoutWallet`, `CreateAccountWallet`, `GetAccountWallets`, `RemoveWallet` (WalletViewController), `VerifySeekerHolder`, `GetAccountPayments`, `UnlinkReferralNetwork`, `SetNetworkReferral`, `ChangeNetworkName`, `ClaimNetworkName`, `GetNetworkRedeemedBalanceCodes` | client token | **network credential** |
| Network only (9 the SDK calls) | `WalletCircleInit`, `WalletBalance`, `WalletCircleTransferOut`, `NetworkUserUpdate` (NetworkUserViewController.UpdateNetworkUser), `CreateApiKey`, `ListApiKeys`, `DeleteApiKey`, `SnNetworkWalletMappingChallengeSync[WithContext]`, `RegisterNetworkClientSyncWithContext` | client token (server 403) | **network credential** |
| Network only (renewal) | the renewer's `POST /auth/network-refresh` (internal) | none (new) | **the network token it renews** |
| Own client payout | `SnWalletMappingChallengeSync[WithContext]`, `SnSetWallet[Sync[WithContext]]`, `SnGetWallet[SyncWithContext]` | client token | client token. These are the provider's own client. The server refuses them only for an Embed network's client token, and an Embed backend never sends one there |
| Own client | `AuthNetworkClient[SyncWithContext]`, `DeviceSetName`, `RemoveNetworkClient[Sync…]` | client token | client token. The server limits it to its own client and its children. `RemoveNetworkClientSyncWithContextAndJwt` sends the credential its caller passes |
| Client | `RefreshJwt` (`/auth/refresh`), `GetLeaderboard`, `WalletValidateAddress`, `SubscriptionBalance[ForStorefront]`, `SubscriptionCreatePaymentId`, `GetNetworkReferralCode`, `GetReferralNetwork`, `SendFeedback`, log upload, `AccountPreferencesGet`, `GetTransferStats`, `GetNetworkLeaderboardRanking`, `GetAccountPoints`, `GetNetworkBlockedLocations`, `GetNetworkReliability`, `GetProviderStatus`, `CreateSolanaPaymentIntent`, `CreateStripePaymentIntent`, `CreateStripeCheckoutSession`, `RedeemBalanceCode`, `CheckBalanceCode`, `VerifyPlayPurchase`, `VerifyAppleTransaction`, `OnboardingOfferIssue`, `StripePaymentSheet`, `StripePrices`, `ClientEventsSend`, `AccountEpochs`, `SnHead`, `SnPoolClaimSync` | client token | client token |
| Public | `AuthLogin`, `AuthWalletChallenge`, `AuthLoginWithPassword`, `AuthVerify`, `AuthPasswordReset`, `AuthVerifySend`, `NetworkCheck`, `NetworkCreate`, `AuthCodeLogin`, `GetProviderLocations`, `FindProviderLocations`, `FindProviders2`, `ValidateReferralCode`, `GetPointsLeaderboard`, `OnboardingClick`, `OnboardingFeedbackToken`, `SnEpoch`. These send none: `GetClientKeySync…`, `VerifyKeysSync…`, `SnValidateWallet` | current | current |

An `Api` that holds only a network credential sends it on every route, as
before. That covers the sign-in phase before a device starts, the ur.io
account host and remotes, and backends.

`api_network_credential_test.go` pins every row: 107 calls, each with its
route and credential. `TestApiCredentialCasesCoverEverySdkRoute` fails for
any SDK route without a row. `TestApiAdminRoutesMatchTheServer` compares the
SDK's table with `../server/api/route_authz.go`, class by class, and fails on
a server class that the SDK has not decided on.

## What the apps must change

**Nothing is required.** None of the App admin call sites in AUTHZ1 §7 builds
its own request. They all call SDK `Api` methods or SDK view controllers, so
the fix ships with the SDK when the apps rebuild against it. Checked:

- apple: `UrApiService.swift`, `UsdcWalletsClient.swift`,
  `StripeBillingClient.swift`, `PointsLeaderboardStore.swift`,
  `NetworkUserViewModel.swift`, `AccountPreferencesViewModel.swift`
- android: `SettingsViewModel.kt`, `SdkLegacyWalletSource.kt`,
  `NetworkPeersViewModel.kt`, `LeaderboardViewModel.kt`,
  `PointsLeaderboardViewModel.kt`, `BlockedRegionsViewModel.kt`,
  `EarningsViewModel.kt`, `UpdateReferralNetworkBottomSheetViewModel.kt`,
  `ProfileViewModel.kt`, `BalanceCodesViewModel.kt`, `AccountViewModel.kt`

The apps' only raw HTTP requests are unauthenticated: android
`MainApplication.kt:749` probes `/status`, and apple only loads web views.

**The fix depends on what the apps already do. Keep it:**

- Save the sign-in token in LocalState, then set it on the `Api` before
  starting the device:
  - apple: `DeviceManager.swift:1790` (`localState.setByJwt(jwt)`) and
    `:1802` (`api.setByJwt(jwt)`)
  - android: `MainApplication.kt:2112-2114`
- After a relaunch, the device adopts that LocalState token. Android already
  refuses to start without a valid `by_jwt` (`MainApplication.kt:1347`).
- Clear it at sign-out:
  - apple: `api?.setByJwt(nil)` at `DeviceManager.swift:990`, `:1879`,
    `:1934`
  - android: `api?.byJwt = null` at `MainApplication.kt:2244`
- Never call `api.setByJwt(clientJwt)`. That replaces the session and drops
  the network credential. Neither app does it.

**Optional, one line each.** An install whose LocalState has lost `by_jwt`,
or whose network token the server rejected on renewal, now gets
`ErrNetworkCredentialRequired` on account screens, where it used to send the
client token. Ask the user to sign in again when `api.hasNetworkCredential()`
is false:

- apple: in `UrApiService.requireApi()`, or before presenting account settings
- android: in `SettingsViewModel`, before account actions

**Embed apps** hold only a client token. Their admin calls fail locally and
nothing is sent. The customer backend administers with the root token or an
API key.

**JS.** The generated TS client (`createURNetworkApiClient`) sends whatever
token its caller configures. ur.io and the web manager configure the root
token. The wasm account host and the device remotes receive the network token
from ur.io, so they are unchanged. A caller that keeps a network token renews
it itself with the generated `authNetworkRefresh()`
(`POST /auth/network-refresh`); `authRefreshToken()` refuses a network token.

## Transition: making the 27 App admin routes Network only

Today the server refuses the App admin routes for a client token of an Embed
network, including one that was ever Embed. The flip refuses them for every
client token. It can happen when all of these hold:

1. **The apps ship this SDK.** Apple and Android releases built against it
   send the network credential on every admin call.
2. **Old builds are gone.** Add a counter to the router gate for client
   tokens that App admin routes let through, by route and by app version.
   Flip when the counter stays near zero for two weeks. The other way to meet
   this is to raise the minimum app version (`upgrade_required` from
   `/network/auth-client`) to the first build with this SDK.
3. **The network token outlives 30 days.** Addressed by renewal: the server
   renews a network token at `POST /auth/network-refresh`, which a client
   token can never call, and the SDK renews the kept one at its half-life
   (Renewal, above). What still holds:
   - Renewal runs only while the app runs. A token of an app that was not
     opened for 30 days expires. While `reject_expired` is off it renews on
     the next launch; once it is on, that user signs in again for the account
     screens.
   - A user whose token the server already rejected (a password reset, or an
     expiration it enforces) signs in again. The app can ask when
     `api.hasNetworkCredential()` is false.
   - Turn `reject_expired` on only once apps with this SDK are the installed
     base and the server's
     `urnetwork_auth_jwt_legacy_accepts_total{cause="expired",kind="network"}`
     stays near zero. `urnetwork_auth_network_refreshes_total` charts the
     renewals by outcome.

Order of the flip:

1. Move G5 and G6 first: `/auth/code-create`, `/auth/add-auth`, both
   seedphrase routes, `/auth/network-delete` and `/auth/remove-auth`.
2. Then move the rest.

For each route, change `routeAccessAppAdmin` to `routeAccessNetwork` in
`route_authz.go` and the class in `apiAdminRouteAccess`. The SDK's behavior
does not change, because it already sends the network credential for both
classes. `TestApiAdminRoutesMatchTheServer` keeps the two tables equal.

After the flip, an old build gets the server's 403 on its account screens.
Connecting and providing are unaffected.
