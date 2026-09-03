//go:build js

package main

import (
	"context"
	"syscall/js"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
)

// NewAccountHost(apiUrl, platformUrl, byJwt) is the sdk for a signed-in page
// that has no device: the network space api plus the api-only view
// controllers the account screens are built on, so the web renders the same
// controllers as android/apple with or without the extension. The host owns
// one network space; close() releases it (and every controller it opened
// should be closed first, as usual).
//
// Openers return the SAME objects a DeviceRemote's openers return. The api
// methods return Promises of the sdk result through its json tags, i.e. the
// API's own snake_case field names, and reject with an Error carrying the sdk
// error message.
func NewAccountHost(this js.Value, args []js.Value) any {
	if len(args) < 3 {
		return js.ValueOf(map[string]any{
			"error": "apiUrl, platformUrl and byJwt are required",
		})
	}
	apiUrl := args[0].String()
	platformUrl := args[1].String()
	byJwt := args[2].String()

	networkSpace := sdk.NewUrlsNetworkSpace(apiUrl, platformUrl)
	api := networkSpace.GetApi()
	api.SetByJwt(byJwt)
	ctx, cancel := context.WithCancel(context.Background())

	m := map[string]any{}

	m["setByJwt"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		api.SetByJwt(stringArg(args, 0))
		return js.Null()
	})
	m["getByJwt"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(api.GetByJwt())
	})
	m["close"] = jsViewControllerClose(func() {
		cancel()
		networkSpace.Close()
	})

	// ── api-only view controllers ────────────────────────────────────────────
	m["openLocationsViewController"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc := sdk.NewLocationsViewControllerWithApi(ctx, api)
		return jsLocationsViewController(vc, vc.Close)
	})
	m["openDevicesViewController"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc := sdk.NewDevicesViewControllerWithApi(ctx, api)
		return jsDevicesViewController(vc, vc.Close)
	})
	m["openAccountPreferencesViewController"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc := sdk.NewAccountPreferencesViewControllerWithApi(ctx, api)
		return jsAccountPreferencesViewController(vc, vc.Close)
	})
	m["openNetworkUserViewController"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc := sdk.NewNetworkUserViewControllerWithApi(ctx, api)
		return jsNetworkUserViewController(vc, vc.Close)
	})
	m["openFeedbackViewController"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc := sdk.NewFeedbackViewControllerWithApi(ctx, api)
		return jsFeedbackViewController(vc, vc.Close)
	})
	m["openReferralCodeViewController"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc := sdk.NewReferralCodeViewControllerWithApi(ctx, api)
		return jsReferralCodeViewController(vc, vc.Close)
	})
	m["openPointsLeaderboardViewController"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc := sdk.NewPointsLeaderboardViewControllerWithApi(ctx, api)
		return jsPointsLeaderboardViewController(vc, vc.Close)
	})
	m["openSubscriptionBalanceViewController"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc := sdk.NewSubscriptionBalanceViewController(api)
		return jsSubscriptionBalanceViewController(vc, vc.Close)
	})

	// ── api methods (Promise<result json>) ───────────────────────────────────
	m["getNetworkClients"] = promiseMethod(func(args []js.Value) js.Value {
		return apiPromise(func(cb connect.ApiCallback[*sdk.NetworkClientsResult]) {
			api.GetNetworkClients(sdk.GetNetworkClientsCallback(cb))
		})
	})
	m["removeNetworkClient"] = promiseMethod(func(args []js.Value) js.Value {
		clientId, err := sdk.ParseId(stringArg(args, 0))
		if err != nil {
			return jsRejected(err)
		}
		return apiPromise(func(cb connect.ApiCallback[*sdk.RemoveNetworkClientResult]) {
			api.RemoveNetworkClient(&sdk.RemoveNetworkClientArgs{ClientId: clientId}, sdk.RemoveNetworkClientCallback(cb))
		})
	})
	// getPointsLeaderboard(sort, cursor?, limit?): one page of the all-time
	// points leaderboard (public; the jwt only adds `me`)
	m["getPointsLeaderboard"] = promiseMethod(func(args []js.Value) js.Value {
		sort := stringArg(args, 0)
		cursor := stringArg(args, 1)
		limit := int(int64Arg(args, 2))
		return apiPromise(func(cb connect.ApiCallback[*sdk.PointsLeaderboardResult]) {
			api.GetPointsLeaderboard(&sdk.GetPointsLeaderboardArgs{Sort: sort, Cursor: cursor, Limit: limit}, sdk.GetPointsLeaderboardCallback(cb))
		})
	})
	m["setPointsLeaderboardPublic"] = promiseMethod(func(args []js.Value) js.Value {
		public := boolArg(args, 0)
		return apiPromise(func(cb connect.ApiCallback[*sdk.SetPointsLeaderboardPublicResult]) {
			api.SetPointsLeaderboardPublic(&sdk.SetPointsLeaderboardPublicArgs{Public: public}, sdk.SetPointsLeaderboardPublicCallback(cb))
		})
	})
	// setEmojiTag(tag): validate with validateEmojiTag first and send `normalized`
	m["setEmojiTag"] = promiseMethod(func(args []js.Value) js.Value {
		tag := stringArg(args, 0)
		return apiPromise(func(cb connect.ApiCallback[*sdk.SetEmojiTagResult]) {
			api.SetEmojiTag(&sdk.SetEmojiTagArgs{EmojiTag: tag}, sdk.SetEmojiTagCallback(cb))
		})
	})
	// validateEmojiTag(tag): synchronous; the sdk's EmojiTagValidation through
	// its json tags (ok, count, normalized, reason, message)
	m["validateEmojiTag"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsJson(sdk.ValidateEmojiTag(stringArg(args, 0)))
	})
	// suggestEmojiTag(count): synchronous; a random tag of 1–3 distinct emoji
	// to prefill the editor with (count 0 or omitted picks the length at random)
	m["suggestEmojiTag"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(sdk.SuggestEmojiTag(int(int64Arg(args, 0))))
	})
	m["getNetworkReferralCode"] = promiseMethod(func(args []js.Value) js.Value {
		return apiPromise(func(cb connect.ApiCallback[*sdk.GetNetworkReferralCodeResult]) {
			api.GetNetworkReferralCode(sdk.GetNetworkReferralCodeCallback(cb))
		})
	})
	m["validateReferralCode"] = promiseMethod(func(args []js.Value) js.Value {
		code := stringArg(args, 0)
		return apiPromise(func(cb connect.ApiCallback[*sdk.ValidateReferralCodeResult]) {
			api.ValidateReferralCode(&sdk.ValidateReferralCodeArgs{ReferralCode: code}, sdk.ValidateReferralCodeCallback(cb))
		})
	})
	m["setNetworkReferral"] = promiseMethod(func(args []js.Value) js.Value {
		code := stringArg(args, 0)
		return apiPromise(func(cb connect.ApiCallback[*sdk.SetNetworkReferralResult]) {
			api.SetNetworkReferral(&sdk.SetNetworkReferralArgs{ReferralCode: code}, sdk.SetNetworkReferralCallback(cb))
		})
	})
	m["getReferralNetwork"] = promiseMethod(func(args []js.Value) js.Value {
		return apiPromise(func(cb connect.ApiCallback[*sdk.GetReferralNetworkResult]) {
			api.GetReferralNetwork(sdk.GetReferralNetworkCallback(cb))
		})
	})
	m["unlinkReferralNetwork"] = promiseMethod(func(args []js.Value) js.Value {
		return apiPromise(func(cb connect.ApiCallback[*sdk.UnlinkReferralNetworkResult]) {
			api.UnlinkReferralNetwork(sdk.UnlinkReferralNetworkCallback(cb))
		})
	})
	// authCodeCreate(uses, durationMinutes)
	m["authCodeCreate"] = promiseMethod(func(args []js.Value) js.Value {
		uses := int(int64Arg(args, 0))
		durationMinutes := 0.0
		if 1 < len(args) && args[1].Type() == js.TypeNumber {
			durationMinutes = args[1].Float()
		}
		return apiPromise(func(cb connect.ApiCallback[*sdk.AuthCodeCreateResult]) {
			api.AuthCodeCreate(&sdk.AuthCodeCreateArgs{Uses: uses, DurationMinutes: durationMinutes}, sdk.AuthCodeCreateCallback(cb))
		})
	})
	m["networkDelete"] = promiseMethod(func(args []js.Value) js.Value {
		return apiPromise(func(cb connect.ApiCallback[*sdk.NetworkDeleteResult]) {
			api.NetworkDelete(sdk.NetworkDeleteCallback(cb))
		})
	})
	m["getLeaderboard"] = promiseMethod(func(args []js.Value) js.Value {
		return apiPromise(func(cb connect.ApiCallback[*sdk.LeaderboardResult]) {
			api.GetLeaderboard(&sdk.GetLeaderboardArgs{}, sdk.GetLeaderboardCallback(cb))
		})
	})
	m["getNetworkLeaderboardRanking"] = promiseMethod(func(args []js.Value) js.Value {
		return apiPromise(func(cb connect.ApiCallback[*sdk.GetNetworkRankingResult]) {
			api.GetNetworkLeaderboardRanking(sdk.GetNetworkLeaderboardRankingCallback(cb))
		})
	})
	m["setNetworkLeaderboardPublic"] = promiseMethod(func(args []js.Value) js.Value {
		isPublic := boolArg(args, 0)
		return apiPromise(func(cb connect.ApiCallback[*sdk.SetNetworkRankingPublicResult]) {
			api.SetNetworkLeaderboardPublic(&sdk.SetNetworkRankingPublicArgs{IsPublic: isPublic}, sdk.SetNetworkLeaderboardPublicCallback(cb))
		})
	})
	m["getNetworkReliability"] = promiseMethod(func(args []js.Value) js.Value {
		return apiPromise(func(cb connect.ApiCallback[*sdk.GetNetworkReliabilityResult]) {
			api.GetNetworkReliability(sdk.GetNetworkReliabilityCallback(cb))
		})
	})
	m["getNetworkRedeemedBalanceCodes"] = promiseMethod(func(args []js.Value) js.Value {
		return apiPromise(func(cb connect.ApiCallback[*sdk.GetNetworkRedeemedBalanceCodesResult]) {
			api.GetNetworkRedeemedBalanceCodes(sdk.GetNetworkRedeemedBalanceCodesCallback(cb))
		})
	})
	m["redeemBalanceCode"] = promiseMethod(func(args []js.Value) js.Value {
		secret := stringArg(args, 0)
		return apiPromise(func(cb connect.ApiCallback[*sdk.RedeemBalanceCodeResult]) {
			api.RedeemBalanceCode(&sdk.RedeemBalanceCodeArgs{Secret: secret}, sdk.RedeemBalanceCodeCallback(cb))
		})
	})
	m["checkBalanceCode"] = promiseMethod(func(args []js.Value) js.Value {
		secret := stringArg(args, 0)
		return apiPromise(func(cb connect.ApiCallback[*sdk.CheckBalanceCodeResult]) {
			api.CheckBalanceCode(&sdk.CheckBalanceCodeArgs{Secret: secret}, sdk.CheckBalanceCodeCallback(cb))
		})
	})
	m["subscriptionBalance"] = promiseMethod(func(args []js.Value) js.Value {
		return apiPromise(func(cb connect.ApiCallback[*sdk.SubscriptionBalanceResult]) {
			api.SubscriptionBalance(sdk.SubscriptionBalanceCallback(cb))
		})
	})
	m["getNetworkUser"] = promiseMethod(func(args []js.Value) js.Value {
		return apiPromise(func(cb connect.ApiCallback[*sdk.GetNetworkUserResult]) {
			api.GetNetworkUser(sdk.GetNetworkUserCallback(cb))
		})
	})

	return js.ValueOf(m)
}

func promiseMethod(run func(args []js.Value) js.Value) js.Func {
	return js.FuncOf(func(this js.Value, args []js.Value) any {
		return run(args)
	})
}

// apiPromise runs one sdk Api call and resolves with the result as json
// (jsJson), or rejects with the sdk error.
func apiPromise[R any](run func(callback connect.ApiCallback[R])) js.Value {
	return jsPromise(func(resolve func(any), reject func(error)) {
		run(connect.NewApiCallback[R](func(result R, err error) {
			if err != nil {
				reject(err)
				return
			}
			resolve(jsJson(result))
		}))
	})
}

func jsRejected(err error) js.Value {
	return js.Global().Get("Promise").Call("reject", js.Global().Get("Error").New(err.Error()))
}
