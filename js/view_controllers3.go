//go:build js

package main

import (
	"encoding/json"
	"syscall/js"

	"github.com/urnetwork/sdk/v2026"
)

// Account-plane and peer view-controller bindings (see view_controllers.go for
// the conventions). These are the controllers the android/apple account
// screens are built on, bound so the web renders the same state machines:
//   - PeerViewController                the connectable (provide-enabled) peers
//   - AccountPreferencesViewController  product-updates preference
//   - NetworkUserViewController         the profile: fetch + rename
//   - FeedbackViewController            send feedback
//   - ReferralCodeViewController        the referral code and its terms
//   - SubscriptionBalanceViewController balance, plan, purchase confirmation
// Opened from a DeviceRemote where one exists (peers need the device's live
// stream) or from an account host (account_host.go) over the api alone.

// jsJson hands an sdk result struct to JS as a plain object through its json
// tags, so the field names are the API's own (snake_case): the shapes the
// site's screens already read from the REST responses. Lists marshal as
// arrays; a nil result is null.
func jsJson(v any) js.Value {
	if v == nil {
		return js.Null()
	}
	b, err := json.Marshal(v)
	if err != nil {
		return js.Null()
	}
	return js.Global().Get("JSON").Call("parse", string(b))
}

func boolArg(args []js.Value, i int) bool {
	return i < len(args) && args[i].Truthy()
}

func stringArg(args []js.Value, i int) string {
	if i < len(args) && args[i].Type() == js.TypeString {
		return args[i].String()
	}
	return ""
}

func int64Arg(args []js.Value, i int) int64 {
	if i < len(args) && args[i].Type() == js.TypeNumber {
		return int64(args[i].Float())
	}
	return 0
}

func lifecycle(m map[string]any, start func(), stop func(), closeController func()) {
	m["close"] = jsViewControllerClose(closeController)
	m["start"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		start()
		return js.Null()
	})
	m["stop"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		stop()
		return js.Null()
	})
}

// ── PeerViewController ───────────────────────────────────────────────────────

// jsNetworkPeer is one of the network's peers, with the dot color every
// platform derives from its client id.
func jsNetworkPeer(peer *sdk.NetworkPeer) js.Value {
	if peer == nil {
		return js.Null()
	}
	roles := []any{}
	if peer.Roles != nil {
		for j := 0; j < peer.Roles.Len(); j += 1 {
			roles = append(roles, peer.Roles.Get(j))
		}
	}
	m := map[string]any{
		"provideEnabled": peer.ProvideEnabled,
		"principal":      peer.Principal,
		"deviceName":     peer.DeviceName,
		"deviceSpec":     peer.DeviceSpec,
		"roles":          roles,
		"colorHex":       peer.ColorHex(),
	}
	if peer.ClientId != nil {
		m["clientId"] = peer.ClientId.String()
	}
	return js.ValueOf(m)
}

func jsNetworkPeerList(list *sdk.NetworkPeerList) js.Value {
	out := []any{}
	if list != nil {
		for i := 0; i < list.Len(); i += 1 {
			out = append(out, jsNetworkPeer(list.Get(i)))
		}
	}
	return js.ValueOf(out)
}

// jsPeerViewController binds the connectable-peers list: ONLY the connected
// peers that provide (the sdk's filter, shared by every app), the count of
// all connected peers, and a listener that delivers the current list.
func jsPeerViewController(vc *sdk.PeerViewController, closeController func()) js.Value {
	if vc == nil {
		return js.Null()
	}
	m := map[string]any{}
	lifecycle(m, vc.Start, vc.Stop, closeController)

	m["getPeers"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsNetworkPeerList(vc.GetPeers())
	})
	m["getPeerCount"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(vc.GetPeerCount())
	})
	m["getConnectedCount"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(vc.GetConnectedCount())
	})
	m["addPeersListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(vc.AddPeersListener(&jsPeersListener{cb}))
	})
	return js.ValueOf(m)
}

type jsPeersListener struct{ cb js.Value }

func (self *jsPeersListener) PeersChanged(peers *sdk.NetworkPeerList) {
	self.cb.Invoke(jsNetworkPeerList(peers))
}

// ── AccountPreferencesViewController ─────────────────────────────────────────

func jsAccountPreferencesViewController(
	vc *sdk.AccountPreferencesViewController,
	closeController func(),
) js.Value {
	if vc == nil {
		return js.Null()
	}
	m := map[string]any{}
	lifecycle(m, vc.Start, vc.Stop, closeController)

	m["getAllowProductUpdates"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(vc.GetAllowProductUpdates())
	})
	m["updateAllowProductUpdates"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc.UpdateAllowProductUpdates(boolArg(args, 0))
		return js.Null()
	})
	m["addAllowProductUpdatesListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(vc.AddAllowProductUpdatesListener(&jsBoolStateListener{cb}))
	})
	return js.ValueOf(m)
}

// jsBoolStateListener serves every `StateChanged(bool)` listener interface.
type jsBoolStateListener struct{ cb js.Value }

func (self *jsBoolStateListener) StateChanged(state bool) {
	self.cb.Invoke(state)
}

// ── NetworkUserViewController ────────────────────────────────────────────────

func jsNetworkUserViewController(
	vc *sdk.NetworkUserViewController,
	closeController func(),
) js.Value {
	if vc == nil {
		return js.Null()
	}
	m := map[string]any{}
	lifecycle(m, vc.Start, vc.Stop, closeController)

	m["fetchNetworkUser"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc.FetchNetworkUser()
		return js.Null()
	})
	// the cached NetworkUser through its json tags (userId, user_name,
	// user_auth, verified, auth_type, network_name, wallet_address,
	// auth_types), null until fetched
	m["getNetworkUser"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsJson(vc.GetNetworkUser())
	})
	m["updateNetworkUser"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc.UpdateNetworkUser(stringArg(args, 0))
		return js.Null()
	})
	m["addNetworkUserListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(vc.AddNetworkUserListener(&jsSignalStateListener{cb}))
	})
	m["addIsLoadingListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(vc.AddIsLoadingListener(&jsBoolStateListener{cb}))
	})
	m["addNetworkUserUpdateErrorListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(vc.AddNetworkUserUpdateErrorListener(&jsMessageListener{cb}))
	})
	m["addNetworkUserUpdateSuccessListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(vc.AddNetworkUserUpdateSuccessListener(&jsSuccessListener{cb}))
	})
	m["addIsUpdatingListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(vc.AddIsUpdatingListener(&jsBoolStateListener{cb}))
	})
	return js.ValueOf(m)
}

// jsSignalStateListener serves the argument-less `StateChanged()` interfaces.
type jsSignalStateListener struct{ cb js.Value }

func (self *jsSignalStateListener) StateChanged() {
	self.cb.Invoke()
}

type jsMessageListener struct{ cb js.Value }

func (self *jsMessageListener) Message(message string) {
	self.cb.Invoke(message)
}

type jsSuccessListener struct{ cb js.Value }

func (self *jsSuccessListener) Success() {
	self.cb.Invoke()
}

// ── FeedbackViewController ───────────────────────────────────────────────────

func jsFeedbackViewController(vc *sdk.FeedbackViewController, closeController func()) js.Value {
	if vc == nil {
		return js.Null()
	}
	m := map[string]any{}
	lifecycle(m, vc.Start, vc.Stop, closeController)

	// sendFeedback(message, starCount)
	m["sendFeedback"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc.SendFeedback(stringArg(args, 0), int(int64Arg(args, 1)))
		return js.Null()
	})
	m["addIsSendingFeedbackListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(vc.AddIsSendingFeedbackListener(&jsBoolStateListener{cb}))
	})
	return js.ValueOf(m)
}

// ── ReferralCodeViewController ───────────────────────────────────────────────

func jsReferralCodeViewController(
	vc *sdk.ReferralCodeViewController,
	closeController func(),
) js.Value {
	if vc == nil {
		return js.Null()
	}
	m := map[string]any{}
	lifecycle(m, vc.Start, vc.Stop, closeController)

	// the last fetched result through its json tags (referral_code,
	// total_referrals, max_referrals, bonus_per_referral_bytes,
	// referred_bonus_bytes, bonus_period_seconds, ...), null until fetched
	m["getReferralCode"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsJson(vc.GetReferralCodeResult())
	})
	m["addReferralCodeListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(vc.AddReferralCodeListener(&jsReferralCodeListener{cb}))
	})
	return js.ValueOf(m)
}

type jsReferralCodeListener struct{ cb js.Value }

func (self *jsReferralCodeListener) ReferralCodeUpdated(code string) {
	self.cb.Invoke(code)
}

// ── SubscriptionBalanceViewController ────────────────────────────────────────

func jsSubscriptionList(list *sdk.SubscriptionList) js.Value {
	out := []any{}
	if list != nil {
		for i := 0; i < list.Len(); i += 1 {
			out = append(out, jsJson(list.Get(i)))
		}
	}
	return js.ValueOf(out)
}

// jsSubscriptionBalanceViewController binds the balance / plan / purchase
// confirmation state machine (see the Go controller's doc): byte counts are
// numbers, the current subscription is its json (subscription_id, store, plan)
// or null, the confirmation state is one of idle |
// waiting_for_confirmation | confirmed | confirmation_gave_up.
func jsSubscriptionBalanceViewController(
	vc *sdk.SubscriptionBalanceViewController,
	closeController func(),
) js.Value {
	if vc == nil {
		return js.Null()
	}
	m := map[string]any{}
	lifecycle(m, vc.Start, vc.Stop, closeController)

	// state
	m["getIsPro"] = js.FuncOf(func(this js.Value, args []js.Value) any { return js.ValueOf(vc.GetIsPro()) })
	m["getIsGuest"] = js.FuncOf(func(this js.Value, args []js.Value) any { return js.ValueOf(vc.GetIsGuest()) })
	m["getIsLoaded"] = js.FuncOf(func(this js.Value, args []js.Value) any { return js.ValueOf(vc.GetIsLoaded()) })
	m["getStartBalanceByteCount"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(vc.GetStartBalanceByteCount())
	})
	m["getAvailableByteCount"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(vc.GetAvailableByteCount())
	})
	m["getPendingByteCount"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(vc.GetPendingByteCount())
	})
	m["getUsedBalanceByteCount"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(vc.GetUsedBalanceByteCount())
	})
	m["getCurrentSubscription"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsJson(vc.GetCurrentSubscription())
	})
	m["getSubscriptions"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsSubscriptionList(vc.GetSubscriptions())
	})
	m["getCurrentStore"] = js.FuncOf(func(this js.Value, args []js.Value) any { return js.ValueOf(vc.GetCurrentStore()) })
	m["getPurchaseConfirmationState"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(vc.GetPurchaseConfirmationState())
	})
	m["getConfirmationBudgetRemainingMillis"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(vc.GetConfirmationBudgetRemainingMillis())
	})

	// actions
	m["refresh"] = js.FuncOf(func(this js.Value, args []js.Value) any { vc.Refresh(); return js.Null() })
	m["setForeground"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc.SetForeground(boolArg(args, 0))
		return js.Null()
	})
	m["startPurchaseConfirmation"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc.StartPurchaseConfirmation()
		return js.Null()
	})
	m["clearPurchaseConfirmation"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc.ClearPurchaseConfirmation()
		return js.Null()
	})
	m["jwtRefreshed"] = js.FuncOf(func(this js.Value, args []js.Value) any { vc.JwtRefreshed(); return js.Null() })

	// tunables (millis)
	m["getBackgroundPollIntervalMillis"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(vc.GetBackgroundPollIntervalMillis())
	})
	m["setBackgroundPollIntervalMillis"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc.SetBackgroundPollIntervalMillis(int64Arg(args, 0))
		return js.Null()
	})
	m["getConfirmationPollIntervalMillis"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(vc.GetConfirmationPollIntervalMillis())
	})
	m["setConfirmationPollIntervalMillis"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc.SetConfirmationPollIntervalMillis(int64Arg(args, 0))
		return js.Null()
	})
	m["getConfirmationBudgetMillis"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(vc.GetConfirmationBudgetMillis())
	})
	m["setConfirmationBudgetMillis"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc.SetConfirmationBudgetMillis(int64Arg(args, 0))
		return js.Null()
	})

	// listeners
	m["addSubscriptionBalanceChangeListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(vc.AddSubscriptionBalanceChangeListener(&jsSubscriptionBalanceChangeListener{cb}))
	})
	m["addSubscriptionJwtOutOfSyncListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(vc.AddSubscriptionJwtOutOfSyncListener(&jsSubscriptionJwtOutOfSyncListener{cb}))
	})
	m["addPurchaseConfirmationListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(vc.AddPurchaseConfirmationListener(&jsPurchaseConfirmationListener{cb}))
	})
	return js.ValueOf(m)
}

type jsSubscriptionBalanceChangeListener struct{ cb js.Value }

func (self *jsSubscriptionBalanceChangeListener) SubscriptionBalanceChanged() {
	self.cb.Invoke()
}

type jsSubscriptionJwtOutOfSyncListener struct{ cb js.Value }

func (self *jsSubscriptionJwtOutOfSyncListener) SubscriptionJwtOutOfSync(serverIsPro bool) {
	self.cb.Invoke(serverIsPro)
}

type jsPurchaseConfirmationListener struct{ cb js.Value }

func (self *jsPurchaseConfirmationListener) PurchaseConfirmationStateChanged(state string) {
	self.cb.Invoke(state)
}

// ── PointsLeaderboardViewController ──────────────────────────────────────────

// jsPointsLeaderboardViewController is the all-time points leaderboard
// (android/POINTSLEADERBOARD.md): sort, pages, load-more and the caller's own
// row. Rows and `me` cross as json through their tags, with the sdk's
// preformatted text fields (display_name, total_points_text, rank_*_text).
func jsPointsLeaderboardViewController(
	vc *sdk.PointsLeaderboardViewController,
	closeController func(),
) js.Value {
	if vc == nil {
		return js.Null()
	}
	m := map[string]any{}
	lifecycle(m, vc.Start, vc.Stop, closeController)

	m["getSort"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(vc.GetSort())
	})
	m["setSort"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc.SetSort(stringArg(args, 0))
		return js.Null()
	})
	m["loadMore"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc.LoadMore()
		return js.Null()
	})
	m["refresh"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		vc.Refresh()
		return js.Null()
	})
	m["getRows"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return jsJson(vc.GetRows())
	})
	m["getRowCount"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(vc.GetRowCount())
	})
	m["isLoading"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(vc.IsLoading())
	})
	m["isEndReached"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(vc.IsEndReached())
	})
	m["getMe"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		me := vc.GetMe()
		if me == nil {
			return js.Null()
		}
		return jsJson(me)
	})
	m["getErrorMessage"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(vc.GetErrorMessage())
	})
	m["getTotalRanked"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(float64(vc.GetTotalRanked()))
	})
	m["getLatestEpoch"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		return js.ValueOf(float64(vc.GetLatestEpoch()))
	})
	m["getSnapshotTime"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		t := vc.GetSnapshotTime()
		if t == nil {
			return js.Null()
		}
		return jsJson(t)
	})
	m["addPointsLeaderboardListener"] = js.FuncOf(func(this js.Value, args []js.Value) any {
		cb, ok := funcArg(args)
		if !ok {
			return js.Null()
		}
		return jsSub(vc.AddPointsLeaderboardListener(&jsPointsLeaderboardListener{cb}))
	})
	return js.ValueOf(m)
}

type jsPointsLeaderboardListener struct{ cb js.Value }

func (self *jsPointsLeaderboardListener) PointsLeaderboardChanged() {
	self.cb.Invoke()
}
