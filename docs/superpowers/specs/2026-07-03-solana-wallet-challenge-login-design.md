# Solana Wallet Login: Server-Issued Challenge Support

Date: 2026-07-03
Repos affected: `urnetwork-sdk` (this repo), `urnetwork-android`, `urnetwork-ios`
Server reference: [`urnetwork/server` PR #402](https://github.com/urnetwork/server/pull/402) — already merged into the beta server (`Ryanmello07/server@beta/self-contained-env`)

## Background

`urnetwork/server` PR #402 replaces client-generated wallet sign-in messages with a
server-issued, single-use, expiring "challenge" to prevent replay attacks:

1. Client calls `POST /auth/wallet-challenge` with an optional
   `{wallet_address, blockchain}` and gets back:
   ```json
   { "challenge": "...", "timestamp": 1234567890, "expires_in": 300, "message_template": "Sign in to URnetwork\nChallenge: <challenge>\nTimestamp: <timestamp>" }
   ```
2. The client has the wallet sign `message_template` **exactly** (byte-for-byte).
3. The client submits the existing `wallet_auth` shape (`wallet_address`,
   `wallet_signature`, `wallet_message`, `blockchain`) to `/auth/login` or
   `/auth/network-create` as before. The server now additionally requires that
   `wallet_message` match a stored, unused, unexpired challenge, and it marks
   the challenge used the moment it's checked (valid or not).
4. There is no backward compatibility: the old client-generated
   `"Welcome to URnetwork"` (or `"Welcome to URnetwork - <timestamp>"`) message
   is rejected outright since it will never match a stored challenge.

All three client repos currently generate their own static/timestamp-based
message client-side and never call the new challenge endpoint, so every
Solana login/signup attempt against the beta server fails.

## Goals

- Add a Go SDK API to fetch a wallet-auth challenge from the server.
- Update Android and iOS to sign the exact server-issued `message_template`
  instead of a client-generated message.
- Correctly handle the challenge's single-use nature across the existing
  two-step "login, then create-network if no account exists" flow.

## Non-goals

- `ethos_dapp` flavor's Ethereum wallet path (`connectEthOSWallet` /
  `walletLoginEthereum`). The server now rejects non-Solana blockchains with
  `400` by design ("no ETH support by design" per the PR). This is an
  intentional server-side scope cut, not a bug — left untouched.
- The Settings "Claim Seeker Token" wallet-signing flow and "link an
  additional wallet to an already-logged-in account" (`addWalletAuth`
  server-side). Both still use plain signature verification
  (`VerifySolanaSignature`), not `UseWalletAuthChallenge` — PR #402 did not
  change their contract, so no client changes are needed.
- Any new automated tests for the wallet UI flows (see Testing section).

## Design

### 1. Go SDK: `Api.AuthWalletChallenge`

Add to `api.go`, following the exact existing pattern used by `AuthLogin` and
every other `Api` method (goroutine + `connect.HandleError` +
`connect.HttpPostWithRawFunction`):

```go
type AuthWalletChallengeCallback connect.ApiCallback[*AuthWalletChallengeResult]

type AuthWalletChallengeArgs struct {
    WalletAddress string `json:"wallet_address,omitempty"`
    Blockchain    string `json:"blockchain,omitempty"`
}

// `model.WalletAuthChallengeResult`
type AuthWalletChallengeResult struct {
    Challenge       string                          `json:"challenge,omitempty"`
    Timestamp       int64                           `json:"timestamp,omitempty"`
    ExpiresIn       int64                           `json:"expires_in,omitempty"`
    MessageTemplate string                          `json:"message_template,omitempty"`
    Error           *AuthWalletChallengeResultError `json:"error,omitempty"`
}

type AuthWalletChallengeResultError struct {
    Message string `json:"message"`
}

func (self *Api) AuthWalletChallenge(args *AuthWalletChallengeArgs, callback AuthWalletChallengeCallback) {
    go connect.HandleError(func() {
        connect.HttpPostWithRawFunction(
            self.ctx,
            self.getHttpPostRaw(),
            fmt.Sprintf("%s/auth/wallet-challenge", self.apiUrl),
            args,
            self.GetByJwt(),
            &AuthWalletChallengeResult{},
            callback,
        )
    })
}
```

No signing logic belongs in Go — signing has always happened natively (MWA on
Android, wallet deep links on iOS). This just exposes the new endpoint through
gomobile bindings like every other API call, matching the naming/shape of
`model.WalletAuthChallengeResult` on the server.

### 2. Signing mechanism change

The server's `parseWalletAuthChallengeMessage` strictly parses an exact
3-line format. Any wrapping breaks it.

- **Android** currently uses MWA's `signIn()` "Sign In With Solana" (SIWS)
  convenience API (`com.solana.mobilewalletadapter.common.signin.SignInWithSolana.Payload`).
  SIWS wraps the given "statement" into a large canonical multi-field message
  (domain, address, statement, URI, version, chain ID, nonce, issued-at...)
  and signs *that whole thing* — fundamentally incompatible with the server's
  strict parser.

  **Fix:** switch to MWA's raw `signMessagesDetached()` API (same
  already-present dependency, `mobile-wallet-adapter-clientlib-ktx:2.1.0`, no
  new library needed), which signs exactly the bytes given to it:
  ```kotlin
  val result = walletAdapter.transact(activityResultSender) { authResult ->
      signMessagesDetached(
          arrayOf(messageTemplate.toByteArray()),
          arrayOf(authResult.accounts.first().publicKey)
      )
  }
  // result.successPayload?.messages?.first()?.signatures?.first() -> ByteArray signature
  ```

- **iOS** already signs the raw message directly via Phantom/Solflare's
  `signMessage` universal-link method (base58-encoded UTF-8 bytes, signed
  as-is). No mechanism change needed — just source the `message` parameter
  from a freshly-fetched `message_template` instead of the static
  `welcomeMessage`.

### 3. Single-use challenge across the login → create-network handoff

**Confirmed conflict:** `handleLoginWallet` (used by `/auth/login`) calls
`UseWalletAuthChallenge` — which marks the challenge used — even when it
subsequently reports "no account found for this wallet." Today, both apps
take that already-consumed signed message/signature and forward it (via
Android nav args / iOS enum-associated-values) to a "create network" screen,
which calls `/auth/network-create` with the same values. That second call
will now get `403 challenge already used`.

Traced and confirmed unaffected: the guest-upgrade paths
(`UpgradeGuest`/`UpgradeGuestExisting` on iOS's `linkGuestToExistingLogin`)
each sign and submit exactly once per user action — no reuse of an
already-consumed challenge occurs there.

**Fix:** when login reports "no account found," immediately fetch a *second*
fresh challenge and re-prompt the wallet to sign it (one extra, transparent
wallet-approval popup for brand-new wallets only — returning users still sign
once), then proceed to create-network with that fresh signature. The
create-network screens/viewmodels themselves need no changes — they already
just consume whatever signed values they're handed at navigation time.

### 4. Android code organization: shared helper

`solana_dapp`, `google`, and `ungoogle` flavors have near-identical
`connectSolanaWallet` logic today (`ethos_dapp` differs only by additionally
offering the out-of-scope Ethereum path). Extract one shared suspend function
instead of duplicating the challenge-fetch/sign/retry orchestration four
times:

```kotlin
// app/app/src/main/java/com/bringyour/network/ui/login/SolanaWalletAuth.kt
data class SolanaSignedChallenge(val publicKey: String, val message: String, val signature: String)

sealed class SolanaChallengeSignResult {
    data class Success(val signed: SolanaSignedChallenge) : SolanaChallengeSignResult()
    object NoWalletFound : SolanaChallengeSignResult()
    data class Failure(val error: Throwable) : SolanaChallengeSignResult()
}

suspend fun requestAndSignSolanaChallenge(
    activityResultSender: ActivityResultSender,
    api: Api,
): SolanaChallengeSignResult
```

This bridges the SDK's callback-style `api.authWalletChallenge(...)` into a
suspend call (via `suspendCancellableCoroutine`), then drives
`walletAdapter.transact` + `signMessagesDetached` as above. Each flavor's
`connectSolanaWallet` calls this once for normal login, and calls it again
(fresh challenge, fresh popup) right before navigating to create-network,
replacing the current stale-value passthrough. The nav-argument shape stays
the same, so `LoginCreateNetworkViewModel`/`LoginNavHost`/the create-network
screen require no changes.

Files expected to change (Android, all 4 flavors + shared):
- New: `app/app/src/main/java/com/bringyour/network/ui/login/SolanaWalletAuth.kt`
- `app/app/src/{solana_dapp,google,ungoogle,ethos_dapp}/java/com/bringyour/network/ui/login/LoginInitial.kt`
  (Solana portion only; `ethos_dapp`'s Ethereum path untouched)
- `app/app/src/{solana_dapp,google,ungoogle,ethos_dapp}/java/com/bringyour/network/ui/login/LoginViewModel.kt`
  (`walletLogin`/`walletLoginSolana`, unchanged signature/shape — still just
  takes `publicKey`/`signedMessage`/`signature` — callers now pass
  challenge-sourced values)

Files expected to change (iOS):
- `app/network/Shared/ViewModels/ConnectWalletProviderViewModel.swift`
  (replace static `welcomeMessage` usage for login/signup — `claimSeekerTokenMessage`
  stays untouched, out of scope)
- `app/network/Authenticate/LoginInitial/LoginInitialView.swift` /
  `LoginInitialViewModel.swift` (fetch challenge before presenting the sign
  sheet; re-fetch + re-sign before navigating to create-network)
- `app/network/Authenticate/CreateNetwork/CreateNetworkViewModel.swift`
  (no shape changes expected — verify during implementation)

Files expected to change (SDK):
- `api.go` (new `AuthWalletChallenge` API, per section 1)

### 5. Error handling

Server errors already arrive as descriptive strings (e.g. `"403 challenge
already used"`, `"400 challenge timestamp too old"`, `"429 ..."` for
rate-limiting). Pass these through to the existing `setLoginError`/snackbar
display as-is — no new error-mapping layer. The functional fix (always
fetching a fresh challenge per attempt) makes expired/used-challenge errors
self-resolving on retry.

### 6. Testing / verification

No Go toolchain, Android emulator, or iOS simulator is available in this
environment, and wallet signing fundamentally requires a real Phantom/Solflare
app and an on-chain keypair — the actual sign-in flow can't be executed here.
Verification will be:
- Push changes and run the existing CI workflows (`beta-build.yml` in each
  repo) to confirm the Go SDK, Kotlin, and Swift all compile cleanly.
- Manual on-device testing against the beta server is required afterward and
  is out of scope for this agent session.
