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
  refresh and device close. It ends when:
  - `SetByJwt` is called with any other value: sign-out (`""`), a client token
    (which replaces the session), or another network's token;
  - the server confirms that it rejects the client token, which signs the app
    out.

  A hosted device's session `Api` never receives a network credential.
- **No refresh.** The server refreshes only client tokens
  (`/auth/refresh`: "Client ID is required"). The SDK refreshes the client
  token exactly as before. The refresh request carries the client token, and
  the network credential is left as it is.
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
| Own client payout | `SnWalletMappingChallengeSync[WithContext]`, `SnSetWallet[Sync[WithContext]]`, `SnGetWallet[SyncWithContext]` | client token | client token. These are the provider's own client. The server refuses them only for an Embed network's client token, and an Embed backend never sends one there |
| Own client | `AuthNetworkClient[SyncWithContext]`, `DeviceSetName`, `RemoveNetworkClient[Sync…]` | client token | client token. The server limits it to its own client and its children. `RemoveNetworkClientSyncWithContextAndJwt` sends the credential its caller passes |
| Client | `RefreshJwt` (`/auth/refresh`), `GetLeaderboard`, `WalletValidateAddress`, `SubscriptionBalance[ForStorefront]`, `SubscriptionCreatePaymentId`, `GetNetworkReferralCode`, `GetReferralNetwork`, `SendFeedback`, log upload, `AccountPreferencesGet`, `GetTransferStats`, `GetNetworkLeaderboardRanking`, `GetAccountPoints`, `GetNetworkBlockedLocations`, `GetNetworkReliability`, `GetProviderStatus`, `CreateSolanaPaymentIntent`, `CreateStripePaymentIntent`, `CreateStripeCheckoutSession`, `RedeemBalanceCode`, `CheckBalanceCode`, `VerifyPlayPurchase`, `VerifyAppleTransaction`, `OnboardingOfferIssue`, `StripePaymentSheet`, `StripePrices`, `ClientEventsSend`, `AccountEpochs`, `SnHead`, `SnPoolClaimSync` | client token | client token |
| Public | `AuthLogin`, `AuthWalletChallenge`, `AuthLoginWithPassword`, `AuthVerify`, `AuthPasswordReset`, `AuthVerifySend`, `NetworkCheck`, `NetworkCreate`, `AuthCodeLogin`, `GetProviderLocations`, `FindProviderLocations`, `FindProviders2`, `ValidateReferralCode`, `GetPointsLeaderboard`, `OnboardingClick`, `OnboardingFeedbackToken`, `SnEpoch`. These send none: `GetClientKeySync…`, `VerifyKeysSync…`, `SnValidateWallet` | current | current |

An `Api` that holds only a network credential sends it on every route, as
before. That covers the sign-in phase before a device starts, the ur.io
account host and remotes, and backends.

`api_network_credential_test.go` pins every row: 106 calls, each with its
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

**Optional, one line each.** An install whose LocalState has lost `by_jwt`
now gets `ErrNetworkCredentialRequired` on account screens, where it used to
send the client token. Ask the user to sign in again when
`api.hasNetworkCredential()` is false:

- apple: in `UrApiService.requireApi()`, or before presenting account settings
- android: in `SettingsViewModel`, before account actions

**Embed apps** hold only a client token. Their admin calls fail locally and
nothing is sent. The customer backend administers with the root token or an
API key.

**JS.** The generated TS client (`createURNetworkApiClient`) sends whatever
token its caller configures. ur.io and the web manager configure the root
token. The wasm account host and the device remotes receive the network token
from ur.io, so they are unchanged.

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
3. **The network token can outlive 30 days.** Sign-in tokens expire after 30
   days and cannot be refreshed. `reject_expired: false` on main keeps
   expired tokens working. Before `reject_expired` is turned on, do one of
   these:
   - let a network token renew itself on the server, which is not an
     escalation, and have the SDK refresh the kept network credential on its
     half-life;
   - have the apps sign in again when an admin call returns 401.

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
