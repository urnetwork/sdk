# Solana Wallet Challenge Login Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make Solana wallet login/signup work against the beta server's new challenge-based auth flow (`urnetwork/server` PR #402) across the Go SDK, Android, and iOS.

**Architecture:** Add a thin `Api.AuthWalletChallenge` passthrough to the Go SDK (mirrors every other `Api` method). On Android, switch from Mobile Wallet Adapter's "Sign In With Solana" convenience API (which wraps the message and breaks the server's strict parser) to its raw `signMessagesDetached` API, wrapped in one new shared Kotlin helper used by all 4 product flavors. On iOS, source the message signed via Phantom/Solflare deep links from the new challenge endpoint instead of a static string. On both platforms, because the server marks a challenge used the instant it's checked (success or failure), fetch and sign a brand-new challenge before *every* server call — including the second call when a brand-new wallet falls through from `/auth/login` to `/auth/network-create`.

**Tech Stack:** Go 1.26 + gomobile (`urnetwork-sdk`), Kotlin + Jetpack Compose + Mobile Wallet Adapter `clientlib-ktx:2.1.0` (`urnetwork-android`), Swift + SwiftUI (`urnetwork-ios`).

**Spec:** `docs/superpowers/specs/2026-07-03-solana-wallet-challenge-login-design.md` (this repo)

## Global Constraints

- No local Go, Kotlin, or Swift toolchain is available in this environment. Verification is done by pushing to the `beta/custom-server` branch of each repo and checking the existing `beta-build.yml` GitHub Actions workflow (`gh run watch` / `gh run view`), the same mechanism used earlier to fix the CI builds.
- The signed message must be **exactly** the server's `message_template` string, byte-for-byte, never modified or reconstructed client-side. Format (for reference only, never hardcode it): `"Sign in to URnetwork\nChallenge: <challenge>\nTimestamp: <timestamp>"`.
- Every wallet-signing attempt (login OR create-network) must fetch a **brand-new** challenge. Never reuse a previously-signed message/signature across two server calls — the server invalidates a challenge the instant it's checked, whether the check succeeds or fails.
- `ethos_dapp` flavor's Ethereum wallet path (`connectEthOSWallet`, `walletLoginEthereum`, and the Ethereum branch of `onCreateNetworkWallet`) is out of scope — do not modify it. The server rejects non-Solana blockchains by design; that's not a bug to route around here.
- iOS's "Claim Seeker Token" wallet-signing flow (`SettingsView.swift`, `connectWalletProviderViewModel.claimSeekerTokenMessage`) and "link an additional wallet to an existing account" are out of scope — the server left these on the old plain-signature check. Do not modify them.
- Do not add new automated test frameworks/harnesses. None of the three repos have existing unit tests for this kind of code (verified: no `api_test.go`-style per-endpoint tests in the SDK, no `*Test.kt` files in the Android app, and iOS's `networkTests.swift`/`networkUITests.swift` are empty Xcode-generated stubs). Follow that existing convention — verify via compilation/CI, not new test suites.

---

### Task 1: SDK — add `Api.AuthWalletChallenge`

**Files:**
- Modify: `api.go` (repo: `urnetwork-sdk`, insert near `AuthLogin`, e.g. after line 169)

**Interfaces:**
- Consumes: existing `Api` struct/pattern (`self.ctx`, `self.getHttpPostRaw()`, `self.GetByJwt()`, `connect.HandleError`, `connect.HttpPostWithRawFunction`) — all already present in `api.go`.
- Produces (for later tasks):
  - Go: `func (self *Api) AuthWalletChallenge(args *AuthWalletChallengeArgs, callback AuthWalletChallengeCallback)`
  - Go types: `AuthWalletChallengeArgs{WalletAddress, Blockchain string}`, `AuthWalletChallengeResult{Challenge string, Timestamp int64, ExpiresIn int64, MessageTemplate string, Error *AuthWalletChallengeResultError}`, `AuthWalletChallengeResultError{Message string}`
  - Kotlin (gomobile binding, used by Task 2): `com.bringyour.sdk.Api.authWalletChallenge(args: AuthWalletChallengeArgs, callback: (AuthWalletChallengeResult?, Exception?) -> Unit)`, `com.bringyour.sdk.AuthWalletChallengeArgs` (settable `walletAddress`/`blockchain` properties), result has `messageTemplate: String` and `error: AuthWalletChallengeResultError?` properties.
  - Swift (gomobile binding, used by Task 8): `api.authWalletChallenge(_ args: SdkAuthWalletChallengeArgs, callback: AuthWalletChallengeCallback)`, `SdkAuthWalletChallengeArgs`, `SdkAuthWalletChallengeResult` (with `.messageTemplate: String`, `.error: SdkAuthWalletChallengeResultError?`).

- [ ] **Step 1: Add the new types and method to `api.go`**

Open `api.go` and find the end of the existing `AuthLogin` method (currently ends around line 169 with the closing braces after `connect.HttpPostWithRawFunction(...)`). Insert this immediately after it:

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

- [ ] **Step 2: Sanity-check the edit**

Read back the modified section of `api.go` and confirm:
- The new code sits at the top level of the file (not nested inside another function — check the brace immediately before your insertion point is the closing `}` of `AuthLogin`).
- `fmt` is already imported at the top of `api.go` (it is — used throughout the file already).
- No other type in the file is already named `AuthWalletChallengeArgs`, `AuthWalletChallengeResult`, `AuthWalletChallengeResultError`, or `AuthWalletChallengeCallback` (run: `grep -n "AuthWalletChallenge" api.go` — expect exactly the block you just added, once each).

- [ ] **Step 3: Commit**

```bash
git add api.go
git commit -m "feat(sdk): add AuthWalletChallenge API for server-issued wallet sign-in challenges"
```

- [ ] **Step 4: Push and verify via CI**

```bash
git push origin beta/custom-server
gh workflow run beta-build.yml --repo Ryanmello07/urnetwork-sdk --ref beta/custom-server
```

Poll with `gh run list --repo Ryanmello07/urnetwork-sdk --limit 1` until the triggered run shows `in_progress`, then `gh run view <run-id> --repo Ryanmello07/urnetwork-sdk` every ~90s until it's `completed`.

Expected: run completes with `success` (green check on job "Build SDK (Android + iOS)"). If it fails, run `gh run view <run-id> --repo Ryanmello07/urnetwork-sdk --log-failed` and fix the reported Go compile error in `api.go` before proceeding — the most likely mistakes are a misplaced brace or a duplicate type name.

---

### Task 2: Android — shared `SolanaWalletAuth.kt` helper

**Files:**
- Create: `app/app/src/main/java/com/bringyour/network/ui/login/SolanaWalletAuth.kt` (repo: `urnetwork-android`)

**Interfaces:**
- Consumes: `com.bringyour.sdk.Api.authWalletChallenge(...)` and `com.bringyour.sdk.AuthWalletChallengeArgs` from Task 1; `com.solana.mobilewalletadapter.clientlib.*` and `com.solana.publickey.SolanaPublicKey` (already a dependency: `mobile-wallet-adapter-clientlib-ktx:2.1.0` in `app/app/build.gradle:517`).
- Produces (for Tasks 3–6):
  - `data class SolanaSignedChallenge(val publicKey: String, val message: String, val signature: String)`
  - `sealed class SolanaChallengeSignResult { data class Success(val signed: SolanaSignedChallenge); object NoWalletFound; data class Failure(val error: Throwable) }`
  - `suspend fun requestAndSignSolanaChallenge(activityResultSender: ActivityResultSender, api: Api): SolanaChallengeSignResult`

- [ ] **Step 1: Create the file**

```kotlin
package com.bringyour.network.ui.login

import android.net.Uri
import android.util.Base64
import com.bringyour.sdk.Api
import com.bringyour.sdk.AuthWalletChallengeArgs
import com.solana.mobilewalletadapter.clientlib.ActivityResultSender
import com.solana.mobilewalletadapter.clientlib.ConnectionIdentity
import com.solana.mobilewalletadapter.clientlib.MobileWalletAdapter
import com.solana.mobilewalletadapter.clientlib.Solana
import com.solana.mobilewalletadapter.clientlib.TransactionResult
import com.solana.publickey.SolanaPublicKey
import kotlinx.coroutines.suspendCancellableCoroutine
import kotlin.coroutines.resume

data class SolanaSignedChallenge(
    val publicKey: String,
    val message: String,
    val signature: String
)

sealed class SolanaChallengeSignResult {
    data class Success(val signed: SolanaSignedChallenge) : SolanaChallengeSignResult()
    object NoWalletFound : SolanaChallengeSignResult()
    data class Failure(val error: Throwable) : SolanaChallengeSignResult()
}

/**
 * Fetches a fresh, server-issued wallet-auth challenge and has the user's
 * Solana wallet sign it via Mobile Wallet Adapter's raw message-signing API
 * (`signMessagesDetached`), NOT the "Sign In With Solana" convenience API —
 * SIWS wraps the message in a multi-field canonical format that the
 * server's strict challenge parser does not accept.
 *
 * IMPORTANT: call this fresh for every login or create-network attempt.
 * The server marks a challenge used the moment it is checked, whether
 * the check succeeds or fails, so a signed message/signature pair must
 * never be reused across two server calls.
 */
suspend fun requestAndSignSolanaChallenge(
    activityResultSender: ActivityResultSender,
    api: Api,
): SolanaChallengeSignResult {

    val challengeArgs = AuthWalletChallengeArgs()
    challengeArgs.blockchain = "solana"

    val messageTemplate = suspendCancellableCoroutine<String?> { cont ->
        api.authWalletChallenge(challengeArgs) { result, err ->
            if (err != null || result == null || result.error != null) {
                cont.resume(null)
            } else {
                cont.resume(result.messageTemplate)
            }
        }
    } ?: return SolanaChallengeSignResult.Failure(
        Exception("Could not fetch a wallet sign-in challenge from the server")
    )

    val solanaUri = Uri.parse("https://ur.io")
    val iconUri = Uri.parse("favicon.ico")
    val identityName = "URnetwork"

    val walletAdapter = MobileWalletAdapter(
        connectionIdentity = ConnectionIdentity(
            identityUri = solanaUri,
            iconUri = iconUri,
            identityName = identityName,
        ),
    )
    walletAdapter.blockchain = Solana.Mainnet

    val result = walletAdapter.transact(activityResultSender) { authResult ->
        signMessagesDetached(
            arrayOf(messageTemplate.toByteArray()),
            arrayOf(authResult.accounts.first().publicKey)
        )
    }

    return when (result) {
        is TransactionResult.Success -> {
            val signatureBytes = result.successPayload?.messages?.first()?.signatures?.first()
            if (signatureBytes == null) {
                SolanaChallengeSignResult.Failure(Exception("Wallet did not return a signature"))
            } else {
                val publicKey = SolanaPublicKey(result.authResult.accounts.first().publicKey).base58()
                val signatureBase64 = Base64.encodeToString(signatureBytes, Base64.NO_WRAP)

                SolanaChallengeSignResult.Success(
                    SolanaSignedChallenge(
                        publicKey = publicKey,
                        message = messageTemplate,
                        signature = signatureBase64
                    )
                )
            }
        }
        is TransactionResult.NoWalletFound -> SolanaChallengeSignResult.NoWalletFound
        is TransactionResult.Failure -> SolanaChallengeSignResult.Failure(result.e)
    }
}
```

Note: the `Uri` import is unused in this file (kept out) — remove it if your editor flags it. Double check: this file does NOT import `Uri`; it's listed here only because Task 3-6 need it in `LoginInitial.kt`. (Re-verify the import block above does not include `android.net.Uri` — it doesn't.)

- [ ] **Step 2: Sanity-check the file**

```bash
grep -n "^package\|^import\|^suspend fun\|^data class\|^sealed class" app/app/src/main/java/com/bringyour/network/ui/login/SolanaWalletAuth.kt
```

Expect: one `package` line, the import block, `data class SolanaSignedChallenge`, `sealed class SolanaChallengeSignResult`, and `suspend fun requestAndSignSolanaChallenge`. Confirm braces are balanced (count `{` vs `}` — should match).

- [ ] **Step 3: Commit**

```bash
git add app/app/src/main/java/com/bringyour/network/ui/login/SolanaWalletAuth.kt
git commit -m "feat(login): add shared Solana wallet-challenge fetch+sign helper"
```

(No CI push yet — this file alone doesn't compile against anything until Task 3 uses it. Verification happens at the end of Task 7.)

---

### Task 3: Android — wire `solana_dapp` flavor to the challenge flow

**Files:**
- Modify: `app/app/src/solana_dapp/java/com/bringyour/network/ui/login/LoginInitial.kt`

**Interfaces:**
- Consumes: `requestAndSignSolanaChallenge`, `SolanaChallengeSignResult`, `SolanaSignedChallenge` from Task 2 (same package, no import needed). Existing (unmodified) `loginViewModel.walletLogin(context, api, publicKey, signedMessage, signature, onLogin, onCreateNetwork)` from `LoginViewModel.kt` — signature unchanged.

- [ ] **Step 1: Remove the now-unused SIWS import**

In the import block near the top of the file, delete this line (no longer used after this task):
```kotlin
import com.solana.mobilewalletadapter.common.signin.SignInWithSolana
```

- [ ] **Step 2: Replace `connectSolanaWallet`**

Find the current `connectSolanaWallet` lambda (starts with `val connectSolanaWallet = {`, currently spans from the `Uri.parse("https://ur.io")` line through the closing `}` after the `is TransactionResult.Failure ->` branch). Replace the entire lambda body with:

```kotlin
    val connectSolanaWallet = {
        scope.launch {
            activityResultSender?.let { sender ->
                val api = application?.api
                if (api == null) {
                    loginViewModel.setLoginError("Error connecting to wallet")
                    return@launch
                }

                when (val result = requestAndSignSolanaChallenge(sender, api)) {
                    is SolanaChallengeSignResult.Success -> {
                        loginViewModel.walletLogin(
                            context,
                            api,
                            result.signed.publicKey,
                            result.signed.message,
                            result.signed.signature,
                            { loginResult -> onLogin(loginResult.network.byJwt) },
                            onCreateNetworkSolana
                        )
                    }
                    is SolanaChallengeSignResult.NoWalletFound -> {
                        noSolanaWalletsFound = true
                        Log.i("LoginInitial", "No MWA compatible wallet app found on device.")
                    }
                    is SolanaChallengeSignResult.Failure -> {
                        loginViewModel.setLoginError("Error connecting to wallet")
                        Log.i("LoginInitial", "Error connecting to wallet: ${result.error}")
                    }
                }
            }
        }
    }
```

- [ ] **Step 3: Replace `onCreateNetworkSolana` to re-sign with a fresh challenge**

Find the current `onCreateNetworkSolana` lambda (the one that immediately URI-encodes `publicKey`/`signedMessage`/`signature` and calls `navController.navigate(...)`). Replace it with:

```kotlin
    val onCreateNetworkSolana: (
        blockchain: String,
        publicKey: String,
        signedMessage: String,
        signature: String
            ) -> Unit = { blockchain, _, _, _ ->

        scope.launch {
            activityResultSender?.let { sender ->
                val api = application?.api
                if (api == null) {
                    loginViewModel.setLoginError("Error connecting to wallet")
                    return@launch
                }

                // the message/signature this callback was invoked with were
                // already consumed by the just-completed /auth/login call —
                // fetch and sign a brand-new challenge before creating the network
                when (val result = requestAndSignSolanaChallenge(sender, api)) {
                    is SolanaChallengeSignResult.Success -> {
                        val encodedPublicKey = Uri.encode(result.signed.publicKey)
                        val encodedSignedMessage = Uri.encode(result.signed.message)
                        val encodedSignature = Uri.encode(result.signed.signature)

                        navController.navigate("create-network/${blockchain}/${encodedPublicKey}/${encodedSignedMessage}/${encodedSignature}")
                    }
                    is SolanaChallengeSignResult.NoWalletFound -> {
                        noSolanaWalletsFound = true
                    }
                    is SolanaChallengeSignResult.Failure -> {
                        loginViewModel.setLoginError("Error connecting to wallet")
                    }
                }
            }
        }
    }
```

Note the parameter names `blockchain, _, _, _` — the incoming `publicKey`/`signedMessage`/`signature` are intentionally discarded (stale); only `blockchain` is reused.

- [ ] **Step 4: Sanity-check the file**

```bash
grep -n "SignInWithSolana\|requestAndSignSolanaChallenge\|onCreateNetworkSolana\|connectSolanaWallet" app/app/src/solana_dapp/java/com/bringyour/network/ui/login/LoginInitial.kt
```

Expect: no `SignInWithSolana` match; `requestAndSignSolanaChallenge` called twice (once in each lambda); `onCreateNetworkSolana` and `connectSolanaWallet` each defined once and referenced consistently with the rest of the file (the `LoginInitial(...)` composable call further down still passes `solanaLogin = { connectSolanaWallet() }` — do not change that wiring).

- [ ] **Step 5: Commit**

```bash
git add app/app/src/solana_dapp/java/com/bringyour/network/ui/login/LoginInitial.kt
git commit -m "fix(login): sign server-issued challenge for Solana login (solana_dapp)"
```

---

### Task 4: Android — wire `google` flavor to the challenge flow

**Files:**
- Modify: `app/app/src/google/java/com/bringyour/network/ui/login/LoginInitial.kt`

**Interfaces:** identical to Task 3.

This flavor's `connectSolanaWallet`/`onCreateNetworkSolana` are structurally identical to `solana_dapp`'s (verified: `diff` between the two files shows only unrelated whitespace/Google-Sign-In differences, no differences in the Solana wallet logic). The edits below are byte-identical to Task 3's, applied to this flavor's copy of the file.

- [ ] **Step 1: Remove the now-unused SIWS import**

Delete this line from the import block:
```kotlin
import com.solana.mobilewalletadapter.common.signin.SignInWithSolana
```

- [ ] **Step 2: Replace `connectSolanaWallet`**

Find the current `connectSolanaWallet` lambda (starts with `val connectSolanaWallet = {`, currently spans from the `Uri.parse("https://ur.io")` line through the closing `}` after the `is TransactionResult.Failure ->` branch). Replace the entire lambda body with:

```kotlin
    val connectSolanaWallet = {
        scope.launch {
            activityResultSender?.let { sender ->
                val api = application?.api
                if (api == null) {
                    loginViewModel.setLoginError("Error connecting to wallet")
                    return@launch
                }

                when (val result = requestAndSignSolanaChallenge(sender, api)) {
                    is SolanaChallengeSignResult.Success -> {
                        loginViewModel.walletLogin(
                            context,
                            api,
                            result.signed.publicKey,
                            result.signed.message,
                            result.signed.signature,
                            { loginResult -> onLogin(loginResult.network.byJwt) },
                            onCreateNetworkSolana
                        )
                    }
                    is SolanaChallengeSignResult.NoWalletFound -> {
                        noSolanaWalletsFound = true
                        Log.i("LoginInitial", "No MWA compatible wallet app found on device.")
                    }
                    is SolanaChallengeSignResult.Failure -> {
                        loginViewModel.setLoginError("Error connecting to wallet")
                        Log.i("LoginInitial", "Error connecting to wallet: ${result.error}")
                    }
                }
            }
        }
    }
```

- [ ] **Step 3: Replace `onCreateNetworkSolana` to re-sign with a fresh challenge**

Find the current `onCreateNetworkSolana` lambda (the one that immediately URI-encodes `publicKey`/`signedMessage`/`signature` and calls `navController.navigate(...)`). Replace it with:

```kotlin
    val onCreateNetworkSolana: (
        blockchain: String,
        publicKey: String,
        signedMessage: String,
        signature: String
            ) -> Unit = { blockchain, _, _, _ ->

        scope.launch {
            activityResultSender?.let { sender ->
                val api = application?.api
                if (api == null) {
                    loginViewModel.setLoginError("Error connecting to wallet")
                    return@launch
                }

                // the message/signature this callback was invoked with were
                // already consumed by the just-completed /auth/login call —
                // fetch and sign a brand-new challenge before creating the network
                when (val result = requestAndSignSolanaChallenge(sender, api)) {
                    is SolanaChallengeSignResult.Success -> {
                        val encodedPublicKey = Uri.encode(result.signed.publicKey)
                        val encodedSignedMessage = Uri.encode(result.signed.message)
                        val encodedSignature = Uri.encode(result.signed.signature)

                        navController.navigate("create-network/${blockchain}/${encodedPublicKey}/${encodedSignedMessage}/${encodedSignature}")
                    }
                    is SolanaChallengeSignResult.NoWalletFound -> {
                        noSolanaWalletsFound = true
                    }
                    is SolanaChallengeSignResult.Failure -> {
                        loginViewModel.setLoginError("Error connecting to wallet")
                    }
                }
            }
        }
    }
```

Note the parameter names `blockchain, _, _, _` — the incoming `publicKey`/`signedMessage`/`signature` are intentionally discarded (stale); only `blockchain` is reused.

- [ ] **Step 4: Sanity-check the file**

```bash
grep -n "SignInWithSolana\|requestAndSignSolanaChallenge\|onCreateNetworkSolana\|connectSolanaWallet" app/app/src/google/java/com/bringyour/network/ui/login/LoginInitial.kt
```

Expect: no `SignInWithSolana` match; `requestAndSignSolanaChallenge` called twice (once in each lambda); `onCreateNetworkSolana` and `connectSolanaWallet` each defined once and referenced consistently with the rest of the file (the `LoginInitial(...)` composable call further down still passes `solanaLogin = { connectSolanaWallet() }` — do not change that wiring).

- [ ] **Step 5: Commit**

```bash
git add app/app/src/google/java/com/bringyour/network/ui/login/LoginInitial.kt
git commit -m "fix(login): sign server-issued challenge for Solana login (google)"
```

---

### Task 5: Android — wire `ungoogle` flavor to the challenge flow

**Files:**
- Modify: `app/app/src/ungoogle/java/com/bringyour/network/ui/login/LoginInitial.kt`

**Interfaces:** identical to Task 3.

Same verification: this flavor's Solana logic is structurally identical to `solana_dapp`'s (only Google-Sign-In-related lines differ). The edits below are byte-identical to Task 3's, applied to this flavor's copy of the file.

- [ ] **Step 1: Remove the now-unused SIWS import**

Delete this line from the import block:
```kotlin
import com.solana.mobilewalletadapter.common.signin.SignInWithSolana
```

- [ ] **Step 2: Replace `connectSolanaWallet`**

Find the current `connectSolanaWallet` lambda (starts with `val connectSolanaWallet = {`, currently spans from the `Uri.parse("https://ur.io")` line through the closing `}` after the `is TransactionResult.Failure ->` branch). Replace the entire lambda body with:

```kotlin
    val connectSolanaWallet = {
        scope.launch {
            activityResultSender?.let { sender ->
                val api = application?.api
                if (api == null) {
                    loginViewModel.setLoginError("Error connecting to wallet")
                    return@launch
                }

                when (val result = requestAndSignSolanaChallenge(sender, api)) {
                    is SolanaChallengeSignResult.Success -> {
                        loginViewModel.walletLogin(
                            context,
                            api,
                            result.signed.publicKey,
                            result.signed.message,
                            result.signed.signature,
                            { loginResult -> onLogin(loginResult.network.byJwt) },
                            onCreateNetworkSolana
                        )
                    }
                    is SolanaChallengeSignResult.NoWalletFound -> {
                        noSolanaWalletsFound = true
                        Log.i("LoginInitial", "No MWA compatible wallet app found on device.")
                    }
                    is SolanaChallengeSignResult.Failure -> {
                        loginViewModel.setLoginError("Error connecting to wallet")
                        Log.i("LoginInitial", "Error connecting to wallet: ${result.error}")
                    }
                }
            }
        }
    }
```

- [ ] **Step 3: Replace `onCreateNetworkSolana` to re-sign with a fresh challenge**

Find the current `onCreateNetworkSolana` lambda (the one that immediately URI-encodes `publicKey`/`signedMessage`/`signature` and calls `navController.navigate(...)`). Replace it with:

```kotlin
    val onCreateNetworkSolana: (
        blockchain: String,
        publicKey: String,
        signedMessage: String,
        signature: String
            ) -> Unit = { blockchain, _, _, _ ->

        scope.launch {
            activityResultSender?.let { sender ->
                val api = application?.api
                if (api == null) {
                    loginViewModel.setLoginError("Error connecting to wallet")
                    return@launch
                }

                // the message/signature this callback was invoked with were
                // already consumed by the just-completed /auth/login call —
                // fetch and sign a brand-new challenge before creating the network
                when (val result = requestAndSignSolanaChallenge(sender, api)) {
                    is SolanaChallengeSignResult.Success -> {
                        val encodedPublicKey = Uri.encode(result.signed.publicKey)
                        val encodedSignedMessage = Uri.encode(result.signed.message)
                        val encodedSignature = Uri.encode(result.signed.signature)

                        navController.navigate("create-network/${blockchain}/${encodedPublicKey}/${encodedSignedMessage}/${encodedSignature}")
                    }
                    is SolanaChallengeSignResult.NoWalletFound -> {
                        noSolanaWalletsFound = true
                    }
                    is SolanaChallengeSignResult.Failure -> {
                        loginViewModel.setLoginError("Error connecting to wallet")
                    }
                }
            }
        }
    }
```

Note the parameter names `blockchain, _, _, _` — the incoming `publicKey`/`signedMessage`/`signature` are intentionally discarded (stale); only `blockchain` is reused.

- [ ] **Step 4: Sanity-check the file**

```bash
grep -n "SignInWithSolana\|requestAndSignSolanaChallenge\|onCreateNetworkSolana\|connectSolanaWallet" app/app/src/ungoogle/java/com/bringyour/network/ui/login/LoginInitial.kt
```

Expect: no `SignInWithSolana` match; `requestAndSignSolanaChallenge` called twice (once in each lambda); `onCreateNetworkSolana` and `connectSolanaWallet` each defined once and referenced consistently with the rest of the file (the `LoginInitial(...)` composable call further down still passes `solanaLogin = { connectSolanaWallet() }` — do not change that wiring).

- [ ] **Step 5: Commit**

```bash
git add app/app/src/ungoogle/java/com/bringyour/network/ui/login/LoginInitial.kt
git commit -m "fix(login): sign server-issued challenge for Solana login (ungoogle)"
```

---

### Task 6: Android — wire `ethos_dapp` flavor to the challenge flow (Solana path only)

**Files:**
- Modify: `app/app/src/ethos_dapp/java/com/bringyour/network/ui/login/LoginInitial.kt`

**Interfaces:** identical `requestAndSignSolanaChallenge`/`SolanaChallengeSignResult` consumption as Task 3. This flavor's callback names differ (`walletLoginSolana`, `onCreateNetworkWallet` — the latter is **shared** between the Solana and out-of-scope Ethereum paths).

- [ ] **Step 1: Remove the now-unused SIWS import**

Delete:
```kotlin
import com.solana.mobilewalletadapter.common.signin.SignInWithSolana
```

- [ ] **Step 2: Replace the Solana `connectSolanaWallet` lambda**

Find `val connectSolanaWallet = { ... }` (the one calling `walletAdapter.signIn(...)` and `loginViewModel.walletLoginSolana(...)`). Replace its body with:

```kotlin
    val connectSolanaWallet = {
        scope.launch {
            activityResultSender?.let { sender ->
                val api = application?.api
                if (api == null) {
                    loginViewModel.setLoginError("Error connecting to wallet")
                    return@launch
                }

                when (val result = requestAndSignSolanaChallenge(sender, api)) {
                    is SolanaChallengeSignResult.Success -> {
                        loginViewModel.walletLoginSolana(
                            context,
                            api,
                            result.signed.publicKey,
                            result.signed.message,
                            result.signed.signature,
                            { loginResult -> onLogin(loginResult.network.byJwt) },
                            onCreateNetworkWallet
                        )
                    }
                    is SolanaChallengeSignResult.NoWalletFound -> {
                        noSolanaWalletsFound = true
                        Log.i("LoginInitial", "No MWA compatible wallet app found on device.")
                    }
                    is SolanaChallengeSignResult.Failure -> {
                        loginViewModel.setLoginError("Error connecting to wallet")
                        Log.i("LoginInitial", "Error connecting to wallet: ${result.error}")
                    }
                }
            }
        }
    }
```

Leave `connectEthOSWallet` (the separate Ethereum-wallet lambda, using `WalletSDK`/`wallet.signMessage`/`walletLoginEthereum`) completely untouched.

- [ ] **Step 3: Branch `onCreateNetworkWallet` by blockchain**

Find the current `onCreateNetworkWallet` (takes `blockchain, publicKey, signedMessage, signature` and unconditionally URI-encodes + navigates). Replace it with:

```kotlin
    val onCreateNetworkWallet: (
        blockchain: String,
        publicKey: String,
        signedMessage: String,
        signature: String
            ) -> Unit = { blockchain, pk, signedMessage, signature ->

        if (blockchain == "solana") {
            scope.launch {
                activityResultSender?.let { sender ->
                    val api = application?.api
                    if (api == null) {
                        loginViewModel.setLoginError("Error connecting to wallet")
                        return@launch
                    }

                    // the message/signature this callback was invoked with were
                    // already consumed by the just-completed /auth/login call —
                    // fetch and sign a brand-new challenge before creating the network
                    when (val result = requestAndSignSolanaChallenge(sender, api)) {
                        is SolanaChallengeSignResult.Success -> {
                            val encodedPublicKey = Uri.encode(result.signed.publicKey)
                            val encodedSignedMessage = Uri.encode(result.signed.message)
                            val encodedSignature = Uri.encode(result.signed.signature)

                            navController.navigate("create-network/solana/${encodedPublicKey}/${encodedSignedMessage}/${encodedSignature}")
                        }
                        is SolanaChallengeSignResult.NoWalletFound -> {
                            noSolanaWalletsFound = true
                        }
                        is SolanaChallengeSignResult.Failure -> {
                            loginViewModel.setLoginError("Error connecting to wallet")
                        }
                    }
                }
            }
        } else {
            // Ethereum path (out of scope): server does not require a wallet
            // challenge for this blockchain today — keep prior behavior as-is.
            val encodedPublicKey = Uri.encode(pk)
            val encodedSignedMessage = Uri.encode(signedMessage)
            val encodedSignature = Uri.encode(signature)
            val encodedBlockchain = Uri.encode(blockchain)

            navController.navigate("create-network/${encodedBlockchain}/${encodedPublicKey}/${encodedSignedMessage}/${encodedSignature}")
        }
    }
```

- [ ] **Step 4: Sanity-check the file**

```bash
grep -n "SignInWithSolana\|requestAndSignSolanaChallenge\|onCreateNetworkWallet\|connectSolanaWallet\|connectEthOSWallet\|walletLoginEthereum" app/app/src/ethos_dapp/java/com/bringyour/network/ui/login/LoginInitial.kt
```

Expect: no `SignInWithSolana`; `requestAndSignSolanaChallenge` appears twice; `connectEthOSWallet` and `walletLoginEthereum` still present and unchanged (grep won't show *content*, so also manually re-read those two blocks with the Read tool and confirm they're byte-identical to before this task).

- [ ] **Step 5: Commit**

```bash
git add app/app/src/ethos_dapp/java/com/bringyour/network/ui/login/LoginInitial.kt
git commit -m "fix(login): sign server-issued challenge for Solana login (ethos_dapp, Solana path only)"
```

---

### Task 7: Android — push and verify all 4 flavors via CI

**Files:** none (verification only)

- [ ] **Step 1: Push**

```bash
git push origin beta/custom-server
```

(This pushes Tasks 2–6's commits together — the `beta-build.yml` workflow only builds one flavor's config per the existing CI setup, so a single CI run exercises the shared code path; see Step 3 for how to confirm the other 3 flavors also compile.)

- [ ] **Step 2: Trigger and watch the existing CI workflow**

```bash
gh workflow run beta-build.yml --repo Ryanmello07/urnetwork-android --ref beta/custom-server
```

Poll `gh run list --repo Ryanmello07/urnetwork-android --limit 1` then `gh run view <run-id> --repo Ryanmello07/urnetwork-android` every ~90s until `completed`.

Expected: `success`. If it fails on `Build githubDebug APK` (the flavor CI actually builds — confirm which via `gh run view <run-id> --repo Ryanmello07/urnetwork-android --log-failed`), fix the reported Kotlin compile error in that flavor's `LoginInitial.kt` or in `SolanaWalletAuth.kt` before proceeding.

- [ ] **Step 3: Confirm the other 3 flavors compile too**

The CI workflow only assembles one flavor. Since all 4 flavors' `LoginInitial.kt` edits are near-identical text (Tasks 3–5) plus one deliberately-adapted variant (Task 6), the highest-risk file for flavor-specific breakage is `SolanaWalletAuth.kt` (shared) — already proven by Step 2's build. For the other 3 flavors, re-run each Task's Step 4 grep sanity-check (already done) and re-read each modified `connectSolanaWallet`/`onCreateNetworkSolana`/`onCreateNetworkWallet` block once more end-to-end for brace balance and correct variable references (`activityResultSender`, `noSolanaWalletsFound`, `application`, `context`, `scope`, `navController` must all be in scope in each file — they are, as parameters/locals of the same enclosing `LoginInitial` composable in every flavor).

If the project's CI is later extended to build multiple flavors, re-run Step 2 for each; for now, this manual cross-check plus the one real compiled flavor is the available verification.

---

### Task 8: iOS — add `authWalletChallenge` to `UrApiService`

**Files:**
- Modify: `app/network/Shared/Services/UrApiService/UrApiServiceProtocol.swift`
- Modify: `app/network/Shared/Services/UrApiService/UrApiService.swift`
- Modify: `app/network/Shared/Services/UrApiService/MockUrApiService.swift`

**Interfaces:**
- Consumes: `SdkAuthWalletChallengeArgs`, `SdkAuthWalletChallengeResult`, `AuthWalletChallengeCallback` (gomobile Swift bindings from Task 1).
- Produces (for Task 9): `UrApiServiceProtocol.authWalletChallenge(_ args: SdkAuthWalletChallengeArgs) async throws -> SdkAuthWalletChallengeResult`, implemented identically on `UrApiService` and `MockUrApiService`.

- [ ] **Step 1: Add to the protocol**

In `UrApiServiceProtocol.swift`, in the `/** * Authentication */` section (after `func authCodeLogin(...)` on line 39), add:

```swift
    func authWalletChallenge(_ args: SdkAuthWalletChallengeArgs) async throws -> SdkAuthWalletChallengeResult
```

- [ ] **Step 2: Implement on `UrApiService`**

In `UrApiService.swift`, inside the `// MARK - authentication` extension (after the `createNetwork` method, which currently ends around line 402), add:

```swift
    func authWalletChallenge(_ args: SdkAuthWalletChallengeArgs) async throws -> SdkAuthWalletChallengeResult {
        return try await withCheckedThrowingContinuation { continuation in

            let callback = AuthWalletChallengeCallback { result, err in

                if let err = err {
                    continuation.resume(throwing: err)
                    return
                }

                guard let result = result else {
                    continuation.resume(throwing: NSError(domain: "UrApiService", code: 0, userInfo: [NSLocalizedDescriptionKey: "No result found in callback"]))
                    return
                }

                if let resultError = result.error {
                    continuation.resume(throwing: NSError(domain: "UrApiService", code: 0, userInfo: [NSLocalizedDescriptionKey: resultError.message]))
                    return
                }

                continuation.resume(returning: result)

            }

            api.authWalletChallenge(args, callback: callback)

        }
    }
```

- [ ] **Step 3: Implement on `MockUrApiService`**

In `MockUrApiService.swift`, after the `createNetwork` mock (currently ends around line 42), add:

```swift
    func authWalletChallenge(_ args: SdkAuthWalletChallengeArgs) async throws -> SdkAuthWalletChallengeResult {
        return SdkAuthWalletChallengeResult()
    }
```

- [ ] **Step 4: Sanity-check all three files**

```bash
grep -n "authWalletChallenge" app/network/Shared/Services/UrApiService/UrApiServiceProtocol.swift app/network/Shared/Services/UrApiService/UrApiService.swift app/network/Shared/Services/UrApiService/MockUrApiService.swift
```

Expect exactly one match per file.

- [ ] **Step 5: Commit**

```bash
git add app/network/Shared/Services/UrApiService/UrApiServiceProtocol.swift app/network/Shared/Services/UrApiService/UrApiService.swift app/network/Shared/Services/UrApiService/MockUrApiService.swift
git commit -m "feat(sdk-service): add authWalletChallenge to UrApiService"
```

---

### Task 9: iOS — add challenge state + fetch to `LoginInitialViewModel`

**Files:**
- Modify: `app/network/Authenticate/LoginInitial/LoginInitialViewModel.swift`

**Interfaces:**
- Consumes: `urApiService.authWalletChallenge(_:)` from Task 8.
- Produces (for Task 10): `@Published private(set) var solanaChallengeMessage: String?` and `func prepareSolanaChallenge() async -> Bool` on `LoginInitialView.ViewModel`.

- [ ] **Step 1: Add the published property and fetch function**

In the `/** * Solana */` section of `LoginInitialViewModel.swift` (currently lines 101–114, ending after `func setIsSigningMessage`), add:

```swift
        @Published private(set) var solanaChallengeMessage: String?

        /// Fetches a fresh, server-issued wallet-auth challenge and stores its
        /// message template for the wallet to sign. Must be called again for
        /// every sign attempt — the server invalidates a challenge the moment
        /// it is checked, whether the check succeeds or fails.
        func prepareSolanaChallenge() async -> Bool {
            let args = SdkAuthWalletChallengeArgs()
            args.blockchain = "solana"

            do {
                let result = try await urApiService.authWalletChallenge(args)
                solanaChallengeMessage = result.messageTemplate
                return true
            } catch {
                solanaChallengeMessage = nil
                setLoginErrorMessage("There was an error connecting to your wallet")
                return false
            }
        }
```

- [ ] **Step 2: Sanity-check the file**

```bash
grep -n "solanaChallengeMessage\|prepareSolanaChallenge" app/network/Authenticate/LoginInitial/LoginInitialViewModel.swift
```

Expect: the `@Published` declaration once, and `prepareSolanaChallenge` defined once (referenced 0 times yet — that's Task 10).

- [ ] **Step 3: Commit**

```bash
git add app/network/Authenticate/LoginInitial/LoginInitialViewModel.swift
git commit -m "feat(login): add Solana wallet-challenge fetch to LoginInitialViewModel"
```

---

### Task 10: iOS — sign the fetched challenge for initial Solana login

**Files:**
- Modify: `app/network/Authenticate/LoginInitial/LoginInitialView.swift`

**Interfaces:**
- Consumes: `viewModel.prepareSolanaChallenge()`, `viewModel.solanaChallengeMessage` from Task 9.

- [ ] **Step 1: Fetch the challenge before presenting the sign sheet**

There are two `presentSignInWithSolanaSheet: { viewModel.setPresentSigninWithSolanaSheet(true) }` closures in this file (around lines 76 and 107 — one for the guest-device-exists path, one for the normal path). Replace **both** occurrences of:

```swift
                            presentSignInWithSolanaSheet: {
                                viewModel.setPresentSigninWithSolanaSheet(true)
                            },
```

with:

```swift
                            presentSignInWithSolanaSheet: {
                                Task {
                                    let ok = await viewModel.prepareSolanaChallenge()
                                    if ok {
                                        viewModel.setPresentSigninWithSolanaSheet(true)
                                    }
                                }
                            },
```

- [ ] **Step 2: Source the sheet's message from the fetched challenge**

Both `SolanaSignMessageSheet(...)` call sites in this file currently pass `message: connectWalletProviderViewModel.welcomeMessage` (lines 132 and 237). Change **only the one inside the `.sheet(isPresented: $viewModel.presentSigninWithSolanaSheet)` block** (around line 132, the login sign-in sheet — NOT the "Claim Seeker Token" one, which doesn't exist in this file, so there is exactly one to change here) from:

```swift
                    message: connectWalletProviderViewModel.welcomeMessage,
```

to:

```swift
                    message: viewModel.solanaChallengeMessage ?? "",
```

- [ ] **Step 3: Sign the fetched challenge message, not the static one**

Find `handleSolanaWalletResult` (currently starts around line 253) and its call site inside the `onOpenURL`/`handleDeepLink` `onSignature` closure (currently around lines 235-241):

```swift
                        Task {
                            await handleSolanaWalletResult(
                                message: connectWalletProviderViewModel.welcomeMessage,
                                signature: signature,
                                publicKey: pk
                            )
                        }
```

Change `message: connectWalletProviderViewModel.welcomeMessage` to `message: viewModel.solanaChallengeMessage ?? ""`:

```swift
                        Task {
                            await handleSolanaWalletResult(
                                message: viewModel.solanaChallengeMessage ?? "",
                                signature: signature,
                                publicKey: pk
                            )
                        }
```

Leave `handleSolanaWalletResult`'s own body (the function definition) unchanged — it already takes `message` as a parameter and forwards it into `createSolanaAuthLoginArgs`.

- [ ] **Step 4: Sanity-check the file**

```bash
grep -n "connectWalletProviderViewModel.welcomeMessage\|viewModel.solanaChallengeMessage\|prepareSolanaChallenge" app/network/Authenticate/LoginInitial/LoginInitialView.swift
```

Expect: `connectWalletProviderViewModel.welcomeMessage` no longer appears in this file at all (both prior usages replaced); `viewModel.solanaChallengeMessage` appears twice (sheet's `message:` and `handleSolanaWalletResult`'s call); `prepareSolanaChallenge` appears twice (once per `presentSignInWithSolanaSheet` closure).

- [ ] **Step 5: Commit**

```bash
git add app/network/Authenticate/LoginInitial/LoginInitialView.swift
git commit -m "fix(login): sign server-issued challenge for Solana login (iOS)"
```

---

### Task 11: iOS — re-sign a fresh challenge before navigating to create-network

**Files:**
- Modify: `app/network/Authenticate/LoginInitial/LoginInitialView.swift`

**Interfaces:**
- Consumes: `viewModel.prepareSolanaChallenge()`, `viewModel.solanaChallengeMessage` (Task 9); `connectWalletProviderViewModel.signMessagePhantom`/`signMessageSolflare` (existing); `navigate(.createNetwork(authLoginArgs))` (existing, `SdkAuthLoginArgs` with `.walletAuth` — unchanged shape).

**Context:** `handleAuthLoginResult`'s `.create(let authLoginArgs)` case currently navigates straight to create-network reusing `authLoginArgs.walletAuth` (the message/signature that `/auth/login` just consumed). Per the design, this needs a fresh challenge signed again before navigating. This requires re-presenting the wallet sign sheet, so it needs a small piece of new state to distinguish "signing to log in" from "signing to create a network."

- [ ] **Step 1: Add state to distinguish a login-sign from a create-network-sign**

In `LoginInitialViewModel.swift`, in the same `/** * Solana */` section from Task 9, add (after `prepareSolanaChallenge`):

```swift
        @Published var isSigningForCreateNetwork: Bool = false
```

- [ ] **Step 2: Change `.create` handling to re-trigger a fresh sign**

In `LoginInitialView.swift`'s `handleAuthLoginResult`, change:

```swift
        case .create(let authLoginArgs):
            viewModel.setIsCheckingUserAuth(false)
            navigate(.createNetwork(authLoginArgs))
            break
```

to:

```swift
        case .create(let authLoginArgs):
            viewModel.setIsCheckingUserAuth(false)

            if authLoginArgs.walletAuth != nil {
                // the wallet challenge behind authLoginArgs was already
                // consumed by the /auth/login call that just returned this
                // .create case — fetch and sign a brand-new one before
                // creating the network
                viewModel.isSigningForCreateNetwork = true
                let ok = await viewModel.prepareSolanaChallenge()
                if ok {
                    viewModel.setPresentSigninWithSolanaSheet(true)
                } else {
                    viewModel.isSigningForCreateNetwork = false
                }
            } else {
                navigate(.createNetwork(authLoginArgs))
            }
            break
```

Note: `handleAuthLoginResult` is already an `async` function (called via `await self.handleAuthLoginResult(result)` at its call sites), so `await viewModel.prepareSolanaChallenge()` is valid directly inside it — no extra `Task {}` wrapper needed here (unlike Task 10 Step 1, which is inside a synchronous button-action closure).

- [ ] **Step 3: Route the re-signed result to create-network instead of login**

Find `handleSolanaWalletResult` (modified in Task 10). Change its body so that when `viewModel.isSigningForCreateNetwork` is true, it builds a fresh `SdkAuthLoginArgs` with the new wallet auth and navigates to create-network instead of calling `authLogin`:

```swift
    private func handleSolanaWalletResult(message: String, signature: String, publicKey: String) async {
        print("handleSolanaWalletResult")

        if viewModel.isSigningForCreateNetwork {
            viewModel.isSigningForCreateNetwork = false
            viewModel.setIsSigningMessage(false)
            viewModel.presentSigninWithSolanaSheet = false

            let createArgsResult = viewModel.createSolanaAuthLoginArgs(message: message, signature: signature, publicKey: publicKey)
            switch createArgsResult {
            case .success(let args):
                navigate(.createNetwork(args))
            case .failure(let error):
                print("error create args result: \(error.localizedDescription)")
                viewModel.setLoginErrorMessage("There was an error logging in")
            }
            return
        }

        guard viewModel.beginLoginAction(.solana) else {
            return
        }

        defer {
            viewModel.endLoginAction(.solana)
        }

        let createArgsResult = viewModel.createSolanaAuthLoginArgs(message: message, signature: signature, publicKey: publicKey)
        switch createArgsResult {
        case .success(let args):

            if deviceManager.device != nil {

                let upgradeArgs = self.createUpgradeSolanaWalletArgs(args)

                let result = await guestUpgradeViewModel.linkGuestToExistingLogin(args: upgradeArgs)

                await self.handleAuthLoginResult(result)
                viewModel.presentSigninWithSolanaSheet = false
                viewModel.setIsSigningMessage(false)


            } else {
                let result = await viewModel.authLogin(args: args)
                await self.handleAuthLoginResult(result)
                viewModel.presentSigninWithSolanaSheet = false
                viewModel.setIsSigningMessage(false)
            }

        case .failure(let error):
            print("error create args result: \(error.localizedDescription)")
            viewModel.setIsSigningMessage(false)
            viewModel.setLoginErrorMessage("There was an error logging in")
        }
    }
```

(Only the new `if viewModel.isSigningForCreateNetwork { ... return }` block at the top is new; everything below it is the existing body from Task 10, unchanged, shown in full here per the plan's no-placeholders rule.)

- [ ] **Step 4: Sanity-check the file**

```bash
grep -n "isSigningForCreateNetwork" app/network/Authenticate/LoginInitial/LoginInitialViewModel.swift app/network/Authenticate/LoginInitial/LoginInitialView.swift
```

Expect: declared once in the ViewModel; referenced in `LoginInitialView.swift` in the `.create` case (set true, and false in `prepareSolanaChallenge` failure path) and in `handleSolanaWalletResult` (checked, then reset to false).

- [ ] **Step 5: Commit**

```bash
git add app/network/Authenticate/LoginInitial/LoginInitialViewModel.swift app/network/Authenticate/LoginInitial/LoginInitialView.swift
git commit -m "fix(login): re-sign a fresh challenge before navigating to create-network (iOS)"
```

---

### Task 12: iOS — push and verify via CI

**Files:** none (verification only)

- [ ] **Step 1: Push**

```bash
git push origin beta/custom-server
```

- [ ] **Step 2: Trigger and watch the existing CI workflow**

```bash
gh workflow run beta-build.yml --repo Ryanmello07/urnetwork-ios --ref beta/custom-server
```

Poll `gh run list --repo Ryanmello07/urnetwork-ios --limit 1` then `gh run view <run-id> --repo Ryanmello07/urnetwork-ios` every ~90s until `completed`.

Expected: `success`. If it fails at `Build SDK for iOS` or `Resolve Swift packages` or `Archive unsigned iOS app`, run `gh run view <run-id> --repo Ryanmello07/urnetwork-ios --log-failed` and fix the reported Swift compile error — most likely a mismatched brace from Task 11's `handleSolanaWalletResult` rewrite, or a stray reference to `connectWalletProviderViewModel.welcomeMessage` left over from Task 10.

---

### Task 13: Cross-repo final check

**Files:** none (verification only)

- [ ] **Step 1: Confirm all three CI runs are green**

```bash
gh run list --repo Ryanmello07/urnetwork-sdk --limit 1
gh run list --repo Ryanmello07/urnetwork-android --limit 1
gh run list --repo Ryanmello07/urnetwork-ios --limit 1
```

All three should show `completed` / `success` for the most recent `beta/custom-server` run.

- [ ] **Step 2: Report to the user**

Summarize what changed per repo and remind the user that the actual wallet-signing flow (real Phantom/Solflare app + on-chain keypair) must be tested on a physical device against the beta server — this cannot be exercised in CI or in this environment.
