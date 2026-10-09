package sdk

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect"
)

// The SDK's API call map (AUTHZ-CLIENT.md): every call, the route it requests,
// and the credential the request carries while a device holds its client token
// beside the network credential. A route of the App admin or Network only class
// (server api/route_authz.go) carries the network credential. Every other route
// carries the API's current credential, the device's client token, unless the
// call sends none or names its own.

type sentCredential int

const (
	// the API's current credential: the device's client token
	sendsCurrent sentCredential = iota
	// the network credential
	sendsNetwork
	// no credential
	sendsNone
	// the credential the caller passed (credentialTestExplicitByJwt)
	sendsExplicit
)

const (
	credentialTestNetworkId      = "00000000-0000-0000-0000-00000000a001"
	credentialTestOtherNetworkId = "00000000-0000-0000-0000-00000000a002"
	credentialTestUserId         = "00000000-0000-0000-0000-00000000b001"
	credentialTestClientId       = "00000000-0000-0000-0000-00000000c001"
	credentialTestDeviceId       = "00000000-0000-0000-0000-00000000d001"
	credentialTestKeyClientId    = "00000000-0000-0000-0000-00000000e001"
	credentialTestExplicitByJwt  = "explicit-owner-credential"
)

type apiCredentialCase struct {
	name string
	// the route as route_authz.go keys it, with the path as the SDK formats it
	route string
	// the path of the request the call sends
	path  string
	sends sentCredential
	call  func(ctx context.Context, api *Api) error
}

// Runs one asynchronous API call and returns its callback error.
func awaitApiCall[R any](start func(callback connect.ApiCallback[R])) error {
	done := make(chan error, 1)
	start(connect.NewApiCallback[R](func(result R, err error) {
		done <- err
	}))
	select {
	case err := <-done:
		return err
	case <-time.After(15 * time.Second):
		return errors.New("the api call did not complete")
	}
}

func credentialTestRegistrationArgs() *RegisterNetworkClientArgs {
	return &RegisterNetworkClientArgs{
		Schema:            NetworkClientRegistrationSchema,
		RegistrationId:    strings.Repeat("1", 64),
		ScopeSha256:       strings.Repeat("2", 64),
		DeviceDescription: "credential test",
	}
}

func apiCredentialCases() []apiCredentialCase {
	keyClientId, err := ParseId(credentialTestKeyClientId)
	if err != nil {
		panic(err)
	}
	return []apiCredentialCase{
		// sign-in and public routes
		{"AuthLogin", "POST /auth/login", "/auth/login", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*AuthLoginResult]) { api.AuthLogin(&AuthLoginArgs{}, cb) })
		}},
		{"AuthWalletChallenge", "POST /auth/wallet-challenge", "/auth/wallet-challenge", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*AuthWalletChallengeResult]) {
				api.AuthWalletChallenge(&AuthWalletChallengeArgs{}, cb)
			})
		}},
		{"AuthWalletChallengeSyncWithContext", "POST /auth/wallet-challenge", "/auth/wallet-challenge", sendsCurrent, func(ctx context.Context, api *Api) error {
			_, err := api.AuthWalletChallengeSyncWithContext(ctx, &AuthWalletChallengeArgs{})
			return err
		}},
		{"AuthLoginWithPassword", "POST /auth/login-with-password", "/auth/login-with-password", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*AuthLoginWithPasswordResult]) {
				api.AuthLoginWithPassword(&AuthLoginWithPasswordArgs{}, cb)
			})
		}},
		{"AuthLoginWithPasswordSyncWithContext", "POST /auth/login-with-password", "/auth/login-with-password", sendsCurrent, func(ctx context.Context, api *Api) error {
			_, err := api.AuthLoginWithPasswordSyncWithContext(ctx, &AuthLoginWithPasswordArgs{})
			return err
		}},
		{"AuthVerify", "POST /auth/verify", "/auth/verify", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*AuthVerifyResult]) { api.AuthVerify(&AuthVerifyArgs{}, cb) })
		}},
		{"AuthPasswordReset", "POST /auth/password-reset", "/auth/password-reset", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*AuthPasswordResetResult]) {
				api.AuthPasswordReset(&AuthPasswordResetArgs{}, cb)
			})
		}},
		{"AuthVerifySend", "POST /auth/verify-send", "/auth/verify-send", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*AuthVerifySendResult]) {
				api.AuthVerifySend(&AuthVerifySendArgs{}, cb)
			})
		}},
		{"NetworkCheck", "POST /auth/network-check", "/auth/network-check", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*NetworkCheckResult]) { api.NetworkCheck(&NetworkCheckArgs{}, cb) })
		}},
		{"NetworkCreate", "POST /auth/network-create", "/auth/network-create", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*NetworkCreateResult]) { api.NetworkCreate(&NetworkCreateArgs{}, cb) })
		}},
		{"AuthCodeLogin", "POST /auth/code-login", "/auth/code-login", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*AuthCodeLoginResult]) { api.AuthCodeLogin(&AuthCodeLoginArgs{}, cb) })
		}},
		{"AuthCodeLoginSyncWithContext", "POST /auth/code-login", "/auth/code-login", sendsCurrent, func(ctx context.Context, api *Api) error {
			_, err := api.AuthCodeLoginSyncWithContext(ctx, &AuthCodeLoginArgs{})
			return err
		}},
		{"GetProviderLocations", "GET /network/provider-locations", "/network/provider-locations", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*FindLocationsResult]) { api.GetProviderLocations(cb) })
		}},
		{"FindProviderLocations", "POST /network/find-provider-locations", "/network/find-provider-locations", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*FindLocationsResult]) {
				api.FindProviderLocations(&FindLocationsArgs{}, cb)
			})
		}},
		{"FindProviders2", "POST /network/find-providers2", "/network/find-providers2", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*FindProviders2Result]) {
				api.FindProviders2(&FindProviders2Args{}, cb)
			})
		}},
		{"FindProviders2SyncWithContext", "POST /network/find-providers2", "/network/find-providers2", sendsCurrent, func(ctx context.Context, api *Api) error {
			_, err := api.FindProviders2SyncWithContext(ctx, &FindProviders2Args{})
			return err
		}},
		{"ValidateReferralCode", "POST /referral-code/validate", "/referral-code/validate", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*ValidateReferralCodeResult]) {
				api.ValidateReferralCode(&ValidateReferralCodeArgs{}, cb)
			})
		}},
		{"GetPointsLeaderboard", "POST /stats/points-leaderboard", "/stats/points-leaderboard", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*PointsLeaderboardResult]) {
				api.GetPointsLeaderboard(&GetPointsLeaderboardArgs{}, cb)
			})
		}},
		{"OnboardingClick", "POST /onboarding/click", "/onboarding/click", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*OnboardingClickResult]) {
				api.OnboardingClick(&OnboardingClickArgs{}, cb)
			})
		}},
		{"OnboardingFeedbackToken", "GET /onboarding/feedback/%s", "/onboarding/feedback/token-1", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*OnboardingFeedbackTokenResult]) {
				api.OnboardingFeedbackToken("token-1", 0, "", cb)
			})
		}},
		{"SnEpoch", "GET /sn/epoch", "/sn/epoch", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*SnEpochResult]) { api.SnEpoch(cb) })
		}},
		{"SnEpochSyncWithContext", "GET /sn/epoch", "/sn/epoch", sendsCurrent, func(ctx context.Context, api *Api) error {
			_, err := api.SnEpochSyncWithContext(ctx)
			return err
		}},
		{"GetClientKeySyncWithContext", "GET /key/%s", "/key/" + credentialTestKeyClientId, sendsNone, func(ctx context.Context, api *Api) error {
			_, err := api.GetClientKeySyncWithContext(ctx, &GetClientKeyArgs{ClientId: keyClientId})
			return err
		}},
		{"VerifyKeysSyncWithContext", "GET /verify/keys", "/verify/keys", sendsNone, func(ctx context.Context, api *Api) error {
			_, err := api.VerifyKeysSyncWithContext(ctx)
			return err
		}},
		{"SnValidateWallet", "POST /sn/wallet/validate", "/sn/wallet/validate", sendsNone, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*SnValidateWalletResult]) { api.SnValidateWallet("coldkey", cb) })
		}},
		{"SnValidateWalletSyncWithContext", "POST /sn/wallet/validate", "/sn/wallet/validate", sendsNone, func(ctx context.Context, api *Api) error {
			_, err := api.SnValidateWalletSyncWithContext(ctx, "coldkey")
			return err
		}},

		// client routes: the caller's own client, what every installation
		// shows, or something the caller pays for
		{"RefreshJwt", "GET /auth/refresh", "/auth/refresh", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*RefreshJwtResult]) { api.RefreshJwt(cb) })
		}},
		{"GetLeaderboard", "POST /stats/leaderboard", "/stats/leaderboard", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*LeaderboardResult]) { api.GetLeaderboard(&GetLeaderboardArgs{}, cb) })
		}},
		{"WalletValidateAddress", "POST /wallet/validate-address", "/wallet/validate-address", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*WalletValidateAddressResult]) {
				api.WalletValidateAddress(&WalletValidateAddressArgs{}, cb)
			})
		}},
		{"SubscriptionBalance", "GET /subscription/balance", "/subscription/balance", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*SubscriptionBalanceResult]) { api.SubscriptionBalance(cb) })
		}},
		{"SubscriptionBalanceForStorefront", "GET /subscription/balance", "/subscription/balance", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*SubscriptionBalanceResult]) {
				api.SubscriptionBalanceForStorefront("US", cb)
			})
		}},
		{"SubscriptionCreatePaymentId", "POST /subscription/create-payment-id", "/subscription/create-payment-id", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*SubscriptionCreatePaymentIdResult]) {
				api.SubscriptionCreatePaymentId(&SubscriptionCreatePaymentIdArgs{}, cb)
			})
		}},
		{"GetNetworkReferralCode", "GET /account/referral-code", "/account/referral-code", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*GetNetworkReferralCodeResult]) { api.GetNetworkReferralCode(cb) })
		}},
		{"GetReferralNetwork", "GET /account/referral-network", "/account/referral-network", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*GetReferralNetworkResult]) { api.GetReferralNetwork(cb) })
		}},
		{"SendFeedback", "POST /feedback/send-feedback", "/feedback/send-feedback", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*FeedbackSendResult]) { api.SendFeedback(&FeedbackSendArgs{}, cb) })
		}},
		{"postLogsZip", "POST /log/%s/upload", "/log/feedback-1/upload", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*UploadLogsResult]) {
				api.postLogsZip("feedback-1", strings.NewReader("logs"), cb)
			})
		}},
		{"AccountPreferencesGet", "GET /preferences", "/preferences", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*AccountPreferencesGetResult]) { api.AccountPreferencesGet(cb) })
		}},
		{"GetTransferStats", "GET /transfer/stats", "/transfer/stats", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*TransferStatsResult]) { api.GetTransferStats(cb) })
		}},
		{"GetNetworkLeaderboardRanking", "GET /network/ranking", "/network/ranking", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*GetNetworkRankingResult]) { api.GetNetworkLeaderboardRanking(cb) })
		}},
		{"GetAccountPoints", "GET /account/points", "/account/points", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*AccountPointsResult]) { api.GetAccountPoints(cb) })
		}},
		{"GetNetworkBlockedLocations", "GET /network/blocked-locations", "/network/blocked-locations", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*GetNetworkBlockedLocationsResult]) {
				api.GetNetworkBlockedLocations(cb)
			})
		}},
		{"GetNetworkReliability", "GET /network/reliability", "/network/reliability", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*GetNetworkReliabilityResult]) { api.GetNetworkReliability(cb) })
		}},
		{"GetProviderStatus", "GET /network/provider-status", "/network/provider-status", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*GetProviderStatusResult]) { api.GetProviderStatus(cb) })
		}},
		{"CreateSolanaPaymentIntent", "POST /solana/payment-intent", "/solana/payment-intent", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*SolanaPaymentIntentResult]) {
				api.CreateSolanaPaymentIntent(&SolanaPaymentIntentArgs{}, cb)
			})
		}},
		{"CreateStripePaymentIntent", "POST /stripe/payment-intent", "/stripe/payment-intent", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*StripeCreatePaymentIntentResult]) {
				api.CreateStripePaymentIntent(&StripeCreatePaymentIntentArgs{}, cb)
			})
		}},
		{"CreateStripeCheckoutSession", "POST /stripe/create-checkout-session", "/stripe/create-checkout-session", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*StripeCreateCheckoutSessionResult]) {
				api.CreateStripeCheckoutSession(&StripeCreateCheckoutSessionArgs{}, cb)
			})
		}},
		{"RedeemBalanceCode", "POST /subscription/redeem-balance-code", "/subscription/redeem-balance-code", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*RedeemBalanceCodeResult]) {
				api.RedeemBalanceCode(&RedeemBalanceCodeArgs{}, cb)
			})
		}},
		{"CheckBalanceCode", "POST /subscription/check-balance-code", "/subscription/check-balance-code", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*CheckBalanceCodeResult]) {
				api.CheckBalanceCode(&CheckBalanceCodeArgs{}, cb)
			})
		}},
		{"VerifyPlayPurchase", "POST /subscription/verify-play-purchase", "/subscription/verify-play-purchase", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*VerifyStorePurchaseResult]) {
				api.VerifyPlayPurchase(&VerifyPlayPurchaseArgs{}, cb)
			})
		}},
		{"VerifyPlayPurchaseSyncWithContext", "POST /subscription/verify-play-purchase", "/subscription/verify-play-purchase", sendsCurrent, func(ctx context.Context, api *Api) error {
			_, err := api.VerifyPlayPurchaseSyncWithContext(ctx, &VerifyPlayPurchaseArgs{})
			return err
		}},
		{"VerifyAppleTransaction", "POST /subscription/verify-apple-transaction", "/subscription/verify-apple-transaction", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*VerifyStorePurchaseResult]) {
				api.VerifyAppleTransaction(&VerifyAppleTransactionArgs{}, cb)
			})
		}},
		{"VerifyAppleTransactionSyncWithContext", "POST /subscription/verify-apple-transaction", "/subscription/verify-apple-transaction", sendsCurrent, func(ctx context.Context, api *Api) error {
			_, err := api.VerifyAppleTransactionSyncWithContext(ctx, &VerifyAppleTransactionArgs{})
			return err
		}},
		{"OnboardingOfferIssue", "POST /onboarding/offer/issue", "/onboarding/offer/issue", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*OnboardingOfferIssueResult]) {
				api.OnboardingOfferIssue(&OnboardingOfferIssueArgs{}, cb)
			})
		}},
		{"StripePaymentSheet", "POST /subscription/stripe/payment-sheet", "/subscription/stripe/payment-sheet", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*StripePaymentSheetResult]) {
				api.StripePaymentSheet(&StripePaymentSheetArgs{}, cb)
			})
		}},
		{"StripePrices", "GET /subscription/stripe/prices", "/subscription/stripe/prices", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*StripePricesResult]) { api.StripePrices("", cb) })
		}},
		{"ClientEventsSend", "POST /client/events", "/client/events", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*ClientEventsSendResult]) {
				api.ClientEventsSend(&ClientEventsSendArgs{}, cb)
			})
		}},
		{"AccountEpochs", "GET /account/epochs", "/account/epochs", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*AccountEpochsResult]) { api.AccountEpochs(cb) })
		}},
		{"SnHead", "GET /sn/head", "/sn/head", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*SnHeadResult]) { api.SnHead(cb) })
		}},
		{"SnPoolClaimSyncWithContext", "GET /sn/pool/claim", "/sn/pool/claim", sendsCurrent, func(ctx context.Context, api *Api) error {
			_, err := api.SnPoolClaimSyncWithContext(ctx, &SnPoolClaimArgs{Epoch: 7})
			return err
		}},

		// own-client routes: the server limits a client token to its own
		// client and the clients it created
		{"AuthNetworkClient", "POST /network/auth-client", "/network/auth-client", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*AuthNetworkClientResult]) {
				api.AuthNetworkClient(&AuthNetworkClientArgs{}, cb)
			})
		}},
		{"AuthNetworkClientSyncWithContext", "POST /network/auth-client", "/network/auth-client", sendsCurrent, func(ctx context.Context, api *Api) error {
			_, err := api.AuthNetworkClientSyncWithContext(ctx, &AuthNetworkClientArgs{})
			return err
		}},
		{"DeviceSetName", "POST /device/set-name", "/device/set-name", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*DeviceSetNameResult]) { api.DeviceSetName(&DeviceSetNameArgs{}, cb) })
		}},
		{"RemoveNetworkClient", "POST /network/remove-client", "/network/remove-client", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*RemoveNetworkClientResult]) {
				api.RemoveNetworkClient(&RemoveNetworkClientArgs{}, cb)
			})
		}},
		{"RemoveNetworkClientSyncWithContext", "POST /network/remove-client", "/network/remove-client", sendsCurrent, func(ctx context.Context, api *Api) error {
			_, err := api.RemoveNetworkClientSyncWithContext(ctx, &RemoveNetworkClientArgs{})
			return err
		}},
		{"RemoveNetworkClientSyncWithContextAndJwt", "POST /network/remove-client", "/network/remove-client", sendsExplicit, func(ctx context.Context, api *Api) error {
			_, err := api.RemoveNetworkClientSyncWithContextAndJwt(ctx, &RemoveNetworkClientArgs{}, credentialTestExplicitByJwt)
			return err
		}},

		// own-client payout routes: a provider client's subnet wallet. The
		// server refuses a client token here only for an Embed network, whose
		// backend never sends one; everywhere else they take the provider's
		// own client token
		{"SnWalletMappingChallengeSyncWithContext", "POST /sn/wallet/consent", "/sn/wallet/consent", sendsCurrent, func(ctx context.Context, api *Api) error {
			_, err := api.SnWalletMappingChallengeSyncWithContext(ctx, &SnWalletMappingChallengeArgs{})
			return err
		}},
		{"SnSetWallet", "POST /sn/wallet", "/sn/wallet", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*SnSetWalletResult]) { api.SnSetWallet(&SnSetWalletArgs{}, cb) })
		}},
		{"SnSetWalletSyncWithContext", "POST /sn/wallet", "/sn/wallet", sendsCurrent, func(ctx context.Context, api *Api) error {
			_, err := api.SnSetWalletSyncWithContext(ctx, &SnSetWalletArgs{})
			return err
		}},
		{"SnGetWallet", "GET /sn/wallet", "/sn/wallet", sendsCurrent, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*SnGetWalletResult]) { api.SnGetWallet(cb) })
		}},
		{"SnGetWalletSyncWithContext", "GET /sn/wallet", "/sn/wallet", sendsCurrent, func(ctx context.Context, api *Api) error {
			_, err := api.SnGetWalletSyncWithContext(ctx)
			return err
		}},

		// App admin routes: the apps sent these with the device's client token
		{"NetworkDelete", "POST /auth/network-delete", "/auth/network-delete", sendsNetwork, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*NetworkDeleteResult]) { api.NetworkDelete(cb) })
		}},
		{"AuthCodeCreate", "POST /auth/code-create", "/auth/code-create", sendsNetwork, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*AuthCodeCreateResult]) { api.AuthCodeCreate(&AuthCodeCreateArgs{}, cb) })
		}},
		{"AddAuth", "POST /auth/add-auth", "/auth/add-auth", sendsNetwork, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*AddAuthResult]) { api.AddAuth(&AddAuthArgs{}, cb) })
		}},
		{"RemoveAuth", "POST /auth/remove-auth", "/auth/remove-auth", sendsNetwork, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*RemoveAuthResult]) { api.RemoveAuth(&RemoveAuthArgs{}, cb) })
		}},
		{"GenerateSeedphrase", "POST /auth/generate-seedphrase", "/auth/generate-seedphrase", sendsNetwork, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*GenerateSeedphraseResult]) {
				api.GenerateSeedphrase(&GenerateSeedphraseArgs{}, cb)
			})
		}},
		{"RegenerateSeedphrase", "POST /auth/regenerate-seedphrase", "/auth/regenerate-seedphrase", sendsNetwork, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*RegenerateSeedphraseResult]) {
				api.RegenerateSeedphrase(&RegenerateSeedphraseArgs{}, cb)
			})
		}},
		{"GetNetworkClients", "GET /network/clients", "/network/clients", sendsNetwork, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*NetworkClientsResult]) { api.GetNetworkClients(cb) })
		}},
		{"GetNetworkUser", "GET /network/user", "/network/user", sendsNetwork, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*GetNetworkUserResult]) { api.GetNetworkUser(cb) })
		}},
		{"SetNetworkLeaderboardPublic", "POST /network/ranking-visibility", "/network/ranking-visibility", sendsNetwork, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*SetNetworkRankingPublicResult]) {
				api.SetNetworkLeaderboardPublic(&SetNetworkRankingPublicArgs{}, cb)
			})
		}},
		{"SetPointsLeaderboardPublic", "POST /network/points-ranking-visibility", "/network/points-ranking-visibility", sendsNetwork, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*SetPointsLeaderboardPublicResult]) {
				api.SetPointsLeaderboardPublic(&SetPointsLeaderboardPublicArgs{}, cb)
			})
		}},
		{"SetEmojiTag", "POST /network/emoji", "/network/emoji", sendsNetwork, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*SetEmojiTagResult]) { api.SetEmojiTag(&SetEmojiTagArgs{}, cb) })
		}},
		{"NetworkBlockLocation", "POST /network/block-location", "/network/block-location", sendsNetwork, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*NetworkBlockLocationResult]) {
				api.NetworkBlockLocation(&NetworkBlockLocationArgs{}, cb)
			})
		}},
		{"NetworkUnblockLocation", "POST /network/unblock-location", "/network/unblock-location", sendsNetwork, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*NetworkUnblockLocationResult]) {
				api.NetworkUnblockLocation(&NetworkUnblockLocationArgs{}, cb)
			})
		}},
		{"AccountPreferencesUpdate", "POST /preferences/set-preferences", "/preferences/set-preferences", sendsNetwork, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*AccountPreferencesSetResult]) {
				api.AccountPreferencesUpdate(&AccountPreferencesSetArgs{}, cb)
			})
		}},
		{"StripeCreateCustomerPortal", "POST /stripe/customer-portal", "/stripe/customer-portal", sendsNetwork, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*StripeCreateCustomerPortalResult]) {
				api.StripeCreateCustomerPortal(&StripeCreateCustomerPortalArgs{}, cb)
			})
		}},
		{"SetPayoutWallet", "POST /account/payout-wallet", "/account/payout-wallet", sendsNetwork, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*SetPayoutWalletResult]) { api.SetPayoutWallet(&SetPayoutWalletArgs{}, cb) })
		}},
		{"GetPayoutWallet", "GET /account/payout-wallet", "/account/payout-wallet", sendsNetwork, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*GetPayoutWalletIdResult]) { api.GetPayoutWallet(cb) })
		}},
		{"CreateAccountWallet", "POST /account/wallet", "/account/wallet", sendsNetwork, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*CreateAccountWalletResult]) {
				api.CreateAccountWallet(&CreateAccountWalletArgs{}, cb)
			})
		}},
		{"GetAccountWallets", "GET /account/wallets", "/account/wallets", sendsNetwork, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*GetAccountWalletsResult]) { api.GetAccountWallets(cb) })
		}},
		{"RemoveWallet", "POST /account/wallets/remove", "/account/wallets/remove", sendsNetwork, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*RemoveWalletResult]) { api.RemoveWallet(&RemoveWalletArgs{}, cb) })
		}},
		{"VerifySeekerHolder", "POST /account/wallets/verify-seeker", "/account/wallets/verify-seeker", sendsNetwork, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*VerifySeekerNftHolderResult]) {
				api.VerifySeekerHolder(&VerifySeekerNftHolderArgs{}, cb)
			})
		}},
		{"GetAccountPayments", "GET /account/payments", "/account/payments", sendsNetwork, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*GetNetworkAccountPaymentsResult]) { api.GetAccountPayments(cb) })
		}},
		{"UnlinkReferralNetwork", "GET /account/unlink-referral-network", "/account/unlink-referral-network", sendsNetwork, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*UnlinkReferralNetworkResult]) { api.UnlinkReferralNetwork(cb) })
		}},
		{"SetNetworkReferral", "POST /account/set-referral", "/account/set-referral", sendsNetwork, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*SetNetworkReferralResult]) {
				api.SetNetworkReferral(&SetNetworkReferralArgs{}, cb)
			})
		}},
		{"ChangeNetworkName", "POST /account/change-name", "/account/change-name", sendsNetwork, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*ChangeNetworkNameResult]) {
				api.ChangeNetworkName(&ChangeNetworkNameArgs{}, cb)
			})
		}},
		{"ClaimNetworkName", "POST /account/claim-name", "/account/claim-name", sendsNetwork, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*ClaimNetworkNameResult]) {
				api.ClaimNetworkName(&ClaimNetworkNameArgs{}, cb)
			})
		}},
		{"GetNetworkRedeemedBalanceCodes", "GET /account/balance-codes", "/account/balance-codes", sendsNetwork, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*GetNetworkRedeemedBalanceCodesResult]) {
				api.GetNetworkRedeemedBalanceCodes(cb)
			})
		}},

		// Network only routes
		{"WalletCircleInit", "POST /wallet/circle-init", "/wallet/circle-init", sendsNetwork, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*WalletCircleInitResult]) { api.WalletCircleInit(cb) })
		}},
		{"WalletBalance", "GET /wallet/balance", "/wallet/balance", sendsNetwork, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*WalletBalanceResult]) { api.WalletBalance(cb) })
		}},
		{"WalletCircleTransferOut", "POST /wallet/circle-transfer-out", "/wallet/circle-transfer-out", sendsNetwork, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*WalletCircleTransferOutResult]) {
				api.WalletCircleTransferOut(NewWalletCircleTransferOutArgs("address", 1, true), cb)
			})
		}},
		{"NetworkUserUpdate", "POST /network/user/update", "/network/user/update", sendsNetwork, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*NetworkUserUpdateResult]) {
				api.NetworkUserUpdate(&NetworkUserUpdateArgs{}, cb)
			})
		}},
		{"CreateApiKey", "POST /account/api-key", "/account/api-key", sendsNetwork, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*CreateApiKeyResult]) { api.CreateApiKey(&CreateApiKeyArgs{}, cb) })
		}},
		{"ListApiKeys", "GET /account/api-keys", "/account/api-keys", sendsNetwork, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*ListApiKeysResult]) { api.ListApiKeys(cb) })
		}},
		{"DeleteApiKey", "POST /account/api-key/remove", "/account/api-key/remove", sendsNetwork, func(ctx context.Context, api *Api) error {
			return awaitApiCall(func(cb connect.ApiCallback[*DeleteApiKeyResult]) { api.DeleteApiKey(&DeleteApiKeyArgs{}, cb) })
		}},
		{"SnNetworkWalletMappingChallengeSyncWithContext", "POST /sn/wallet/network-consent", "/sn/wallet/network-consent", sendsNetwork, func(ctx context.Context, api *Api) error {
			_, err := api.SnNetworkWalletMappingChallengeSyncWithContext(ctx, &SnNetworkWalletMappingChallengeArgs{})
			return err
		}},
		{"RegisterNetworkClientSyncWithContext", "POST /network/register-client-v1", "/network/register-client-v1", sendsNetwork, func(ctx context.Context, api *Api) error {
			_, err := api.RegisterNetworkClientSyncWithContext(ctx, credentialTestRegistrationArgs())
			return err
		}},
	}
}

type recordedApiRequest struct {
	method string
	path   string
	// the bearer, or "" for none
	byJwt string
}

// An API on a local server that records the credential of every request.
func newCredentialRecordingApi(t *testing.T) (context.Context, *Api, chan recordedApiRequest) {
	t.Helper()
	requests := make(chan recordedApiRequest, 64)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- recordedApiRequest{
			method: r.Method,
			path:   r.URL.Path,
			byJwt:  strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "),
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	})
	ctx, api := newTestApi(t, handler)
	return ctx, api, requests
}

// The request a completed call sent. The handler records a request before it
// answers, so a completed call's request is already in the channel.
func takeRecordedRequest(t *testing.T, requests chan recordedApiRequest) (recordedApiRequest, bool) {
	t.Helper()
	select {
	case request := <-requests:
		return request, true
	default:
		return recordedApiRequest{}, false
	}
}

func credentialTestJwt(t *testing.T, claims gojwt.MapClaims) string {
	t.Helper()
	now := time.Now()
	claims["iat"] = now.Unix()
	claims["exp"] = now.Add(30 * 24 * time.Hour).Unix()
	token, err := gojwt.NewWithClaims(gojwt.SigningMethodNone, claims).SignedString(gojwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// the network's sign-in token: no client_id
func credentialTestNetworkJwt(t *testing.T, networkId string) string {
	return credentialTestJwt(t, gojwt.MapClaims{
		"network_id":   networkId,
		"user_id":      credentialTestUserId,
		"network_name": "credential-test",
	})
}

// a device's client token. The marker tells refreshed tokens apart.
func credentialTestClientJwt(t *testing.T, networkId string, marker string) string {
	return credentialTestJwt(t, gojwt.MapClaims{
		"network_id":   networkId,
		"user_id":      credentialTestUserId,
		"network_name": "credential-test",
		"client_id":    credentialTestClientId,
		"device_id":    credentialTestDeviceId,
		"marker":       marker,
	})
}

// Installs a device's client token the way DeviceLocal and DeviceRemote do:
// prepare under the auth locks, then publish the device as the owner.
func installTestDeviceByJwt(t *testing.T, api *Api, localState *LocalState, clientJwt string, instanceId *Id) *deviceAuthPublicationGate {
	t.Helper()
	owner := newDeviceAuthPublicationGate()
	prepared, err := api.prepareDeviceAuth(localState, clientJwt, instanceId, time.Now(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := api.setDeviceByJwt(prepared, owner, connect.DefaultLogger()); err != nil {
		t.Fatal(err)
	}
	return owner
}

func (self sentCredential) byJwt(networkJwt string, currentJwt string) string {
	switch self {
	case sendsNetwork:
		return networkJwt
	case sendsCurrent:
		return currentJwt
	case sendsExplicit:
		return credentialTestExplicitByJwt
	default:
		return ""
	}
}

// Every SDK call on an API that holds both credentials, as the apps' does once
// their device starts: the admin routes carry the network credential and
// never the client token; every other route carries the client token and never
// the network credential.
func TestEveryApiCallSendsTheCredentialItsRouteNeeds(t *testing.T) {
	ctx, api, requests := newCredentialRecordingApi(t)
	networkJwt := credentialTestNetworkJwt(t, credentialTestNetworkId)
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "device")
	api.SetByJwt(networkJwt)
	installTestDeviceByJwt(t, api, nil, clientJwt, nil)
	connect.AssertEqual(t, api.GetByJwt(), clientJwt)
	connect.AssertEqual(t, api.HasNetworkCredential(), true)

	for _, c := range apiCredentialCases() {
		t.Run(c.name, func(t *testing.T) {
			method, _, _ := strings.Cut(c.route, " ")
			admin := apiRouteAccessFor(method, c.path) != apiRouteAccessClient
			if admin != (c.sends == sendsNetwork) {
				t.Fatalf("%s %s: the route table says admin=%t, the case sends %d", method, c.path, admin, c.sends)
			}

			err := c.call(ctx, api)
			if errors.Is(err, ErrNetworkCredentialRequired) {
				t.Fatalf("refused while the network credential is held: %v", err)
			}
			request, ok := takeRecordedRequest(t, requests)
			if !ok {
				t.Fatalf("no request was sent (err = %v)", err)
			}
			connect.AssertEqual(t, request.method, method)
			connect.AssertEqual(t, request.path, c.path)
			connect.AssertEqual(t, request.byJwt, c.sends.byJwt(networkJwt, clientJwt))
			if admin && request.byJwt == clientJwt {
				t.Fatal("an admin route carried the client token")
			}
			if !admin && request.byJwt == networkJwt {
				t.Fatal("a client route carried the network credential")
			}
			if extra, ok := takeRecordedRequest(t, requests); ok {
				t.Fatalf("unexpected extra request %+v", extra)
			}
		})
	}
}

// A device that an embed backend provisioned holds only its client token. No
// admin call is sent at all: each fails with ErrNetworkCredentialRequired and
// never falls back to the client token. Every other call works as before.
func TestAdminCallsAreRefusedWithoutTheNetworkCredential(t *testing.T) {
	ctx, api, requests := newCredentialRecordingApi(t)
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "embed")
	installTestDeviceByJwt(t, api, nil, clientJwt, nil)
	connect.AssertEqual(t, api.HasNetworkCredential(), false)

	for _, c := range apiCredentialCases() {
		t.Run(c.name, func(t *testing.T) {
			method, _, _ := strings.Cut(c.route, " ")
			err := c.call(ctx, api)
			request, sent := takeRecordedRequest(t, requests)
			if c.sends == sendsNetwork {
				if !errors.Is(err, ErrNetworkCredentialRequired) {
					t.Fatalf("err = %v, want ErrNetworkCredentialRequired", err)
				}
				if !strings.Contains(err.Error(), method+" "+c.path) {
					t.Fatalf("the refusal does not name the route: %v", err)
				}
				if sent {
					t.Fatalf("an admin request was sent without the network credential: %+v", request)
				}
				return
			}
			if !sent {
				t.Fatalf("no request was sent (err = %v)", err)
			}
			connect.AssertEqual(t, request.path, c.path)
			connect.AssertEqual(t, request.byJwt, c.sends.byJwt("", clientJwt))
		})
	}
}

// With no credential at all an admin call is refused too, rather than sent
// without one.
func TestAdminCallsAreRefusedWithoutAnyCredential(t *testing.T) {
	_, api, requests := newCredentialRecordingApi(t)
	connect.AssertEqual(t, api.HasNetworkCredential(), false)
	err := awaitApiCall(func(cb connect.ApiCallback[*NetworkDeleteResult]) { api.NetworkDelete(cb) })
	if !errors.Is(err, ErrNetworkCredentialRequired) {
		t.Fatalf("err = %v, want ErrNetworkCredentialRequired", err)
	}
	if request, sent := takeRecordedRequest(t, requests); sent {
		t.Fatalf("an admin request was sent with no credential: %+v", request)
	}
}

// An API that holds only the network credential (a sign-in before the device
// starts, an account page, a backend) sends it on every route, as before.
func TestNetworkCredentialAloneIsSentOnEveryRoute(t *testing.T) {
	ctx, api, requests := newCredentialRecordingApi(t)
	networkJwt := credentialTestNetworkJwt(t, credentialTestNetworkId)
	api.SetByJwt(networkJwt)
	for _, call := range []func() error{
		func() error {
			return awaitApiCall(func(cb connect.ApiCallback[*NetworkDeleteResult]) { api.NetworkDelete(cb) })
		},
		func() error {
			return awaitApiCall(func(cb connect.ApiCallback[*SubscriptionBalanceResult]) { api.SubscriptionBalance(cb) })
		},
		func() error {
			_, err := api.AuthNetworkClientSyncWithContext(ctx, &AuthNetworkClientArgs{})
			return err
		},
	} {
		_ = call()
		request, ok := takeRecordedRequest(t, requests)
		if !ok {
			t.Fatal("no request was sent")
		}
		connect.AssertEqual(t, request.byJwt, networkJwt)
	}
}

// An API key is a network credential: kept beside the device's client token
// and sent on the admin routes.
func TestApiKeyIsKeptAsTheNetworkCredential(t *testing.T) {
	_, api, requests := newCredentialRecordingApi(t)
	apiKey := apiKeyPrefix + "credential-test-key"
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "device")
	api.SetByJwt(apiKey)
	installTestDeviceByJwt(t, api, nil, clientJwt, nil)

	_ = awaitApiCall(func(cb connect.ApiCallback[*NetworkClientsResult]) { api.GetNetworkClients(cb) })
	request, ok := takeRecordedRequest(t, requests)
	if !ok {
		t.Fatal("no request was sent")
	}
	connect.AssertEqual(t, request.byJwt, apiKey)
}
