package sdk

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// TestSubscriptionBalanceResultOnboardingFields pins the plan response decode:
// the tier, the offer and the experiments object (surface -> assignment) come
// through the gomobile-safe types, and an older server without them decodes to
// nil fields.
func TestSubscriptionBalanceResultOnboardingFields(t *testing.T) {
	body := `{
		"balance_byte_count": 10,
		"update_time": "2026-09-10T12:00:00Z",
		"price_tier": {"name": "regional", "yearly_usd": 4, "monthly_usd": 0.5, "currency": "USD", "source": "ip", "estimate": true},
		"onboarding_offer": {
			"issued_at": "2026-09-10T12:00:00Z", "expires_at": "2099-09-15T12:00:00Z",
			"percent_off": 25, "months_free": 3, "first_year_usd": 3, "regular_year_usd": 4,
			"tier": "regional", "currency": "USD", "state": "active",
			"apple_offer_code": "ABCD1234", "play_offer_tag": "onboarding25", "stripe_coupon_id": "onboarding25"
		},
		"experiments": {
			"offer.in_app": {"experiment_id": "offer_screen", "variant": "holdout"},
			"email.sequence": {"experiment_id": "email_sequence", "variant": "control"}
		}
	}`
	var result SubscriptionBalanceResult
	connect.AssertEqual(t, nil, json.Unmarshal([]byte(body), &result))
	connect.AssertEqual(t, "regional", result.PriceTier.Name)
	connect.AssertEqual(t, true, result.PriceTier.IsRegional())
	connect.AssertEqual(t, true, result.PriceTier.Estimate)
	connect.AssertEqual(t, PriceTierSourceIp, result.PriceTier.Source)
	connect.AssertEqual(t, true, result.OfferActive())
	connect.AssertEqual(t, 3.0, result.OnboardingOffer.FirstYearUsd)
	connect.AssertEqual(t, "ABCD1234", result.OnboardingOffer.AppleOfferCode)
	connect.AssertEqual(t, true, 0 < result.OnboardingOffer.SecondsUntilExpiry())
	connect.AssertEqual(t, true, 0 < result.OnboardingOffer.ExpiresAtUnixMillis())

	connect.AssertEqual(t, 2, result.Experiments.Len())
	// sorted by surface
	connect.AssertEqual(t, "email.sequence", result.Experiments.Get(0).Surface)
	connect.AssertEqual(t, "offer.in_app", result.Experiments.Get(1).Surface)
	connect.AssertEqual(t, "holdout", result.ExperimentVariant(ExperimentSurfaceOfferInApp))
	connect.AssertEqual(t, "control", result.ExperimentVariant(ExperimentSurfaceEmailSequence))
	connect.AssertEqual(t, "", result.ExperimentVariant(ExperimentSurfaceOfferAccount))
	connect.AssertEqual(t, true, result.IsHoldout(ExperimentSurfaceOfferInApp))
	// the intro step and the final screen follow the in-app assignment
	connect.AssertEqual(t, true, result.IsHoldout(ExperimentSurfaceOfferIntroStep))
	connect.AssertEqual(t, true, result.IsHoldout(ExperimentSurfaceOfferFinalScreen))
	connect.AssertEqual(t, false, result.IsHoldout(ExperimentSurfaceEmailSequence))
	connect.AssertEqual(t, false, result.IsHoldout(ExperimentSurfaceOfferAccount))
	connect.AssertEqual(t, true, result.Experiments.ForSurface(ExperimentSurfaceOfferInApp).IsHoldout())

	// the wire shape survives a round trip (the web reads the object)
	out, err := json.Marshal(&result)
	connect.AssertEqual(t, nil, err)
	connect.AssertEqual(t, true, strings.Contains(string(out), `"experiments":{"email.sequence":{"surface":"email.sequence","experiment_id":"email_sequence","variant":"control"}`))

	// an older server
	var old SubscriptionBalanceResult
	connect.AssertEqual(t, nil, json.Unmarshal([]byte(`{"balance_byte_count": 1, "onboarding_offer": null}`), &old))
	connect.AssertEqual(t, true, old.PriceTier == nil)
	connect.AssertEqual(t, true, old.OnboardingOffer == nil)
	connect.AssertEqual(t, true, old.Experiments == nil)
	connect.AssertEqual(t, false, old.OfferActive())
	connect.AssertEqual(t, "", old.ExperimentVariant(ExperimentSurfaceOfferInApp))
	connect.AssertEqual(t, false, old.IsHoldout(ExperimentSurfaceOfferInApp))

	// an expired offer
	expired := &OnboardingOffer{State: OnboardingOfferStateExpired, ExpiresAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}
	connect.AssertEqual(t, false, expired.IsActive())
	connect.AssertEqual(t, int64(0), expired.SecondsUntilExpiry())
	connect.AssertEqual(t, int64(0), (&OnboardingOffer{ExpiresAt: "garbage"}).ExpiresAtUnixMillis())
}

// TestSubscriptionBalanceResultWinbackOffer pins the win-back field's decode:
// each store's shape comes through next to the welcome offer, the deadline
// alone decides whether it is active, and a response without the field (no
// offer, or an older server) decodes to nil.
func TestSubscriptionBalanceResultWinbackOffer(t *testing.T) {
	cases := []struct {
		offer        string
		store        string
		playOfferTag string
		appleOfferId string
	}{
		{`{"expires_at": "2099-11-16T12:00:00Z", "percent_off": 50, "plan": "monthly", "store": "play", "play_offer_tag": "winback-monthly-50"}`, WinbackOfferStorePlay, "winback-monthly-50", ""},
		{`{"expires_at": "2099-11-16T12:00:00Z", "percent_off": 50, "plan": "monthly", "store": "stripe"}`, WinbackOfferStoreStripe, "", ""},
		{`{"expires_at": "2099-11-16T12:00:00Z", "percent_off": 50, "plan": "monthly", "store": "apple", "apple_offer_id": "winback_monthly_50"}`, WinbackOfferStoreApple, "", "winback_monthly_50"},
	}
	for _, c := range cases {
		body := `{"balance_byte_count": 1, "onboarding_offer": null, "winback_offer": ` + c.offer + `}`
		var result SubscriptionBalanceResult
		connect.AssertEqual(t, nil, json.Unmarshal([]byte(body), &result))
		connect.AssertEqual(t, true, result.OnboardingOffer == nil)
		offer := result.WinbackOffer
		connect.AssertEqual(t, true, offer != nil)
		connect.AssertEqual(t, c.store, offer.Store)
		connect.AssertEqual(t, 50, offer.PercentOff)
		connect.AssertEqual(t, PlanMonthly, offer.Plan)
		connect.AssertEqual(t, c.playOfferTag, offer.PlayOfferTag)
		connect.AssertEqual(t, c.appleOfferId, offer.AppleOfferId)
		connect.AssertEqual(t, time.Date(2099, 11, 16, 12, 0, 0, 0, time.UTC).UnixMilli(), offer.ExpiresAtUnixMillis())
		connect.AssertEqual(t, true, offer.IsActive())

		// the wire shape survives a round trip (the web reads the object)
		out, err := json.Marshal(&result)
		connect.AssertEqual(t, nil, err)
		var back SubscriptionBalanceResult
		connect.AssertEqual(t, nil, json.Unmarshal(out, &back))
		connect.AssertEqual(t, *offer, *back.WinbackOffer)
	}

	// the deadline decides: active until it, not at it or after it, and never
	// with an unreadable one or nothing off
	offer := &WinbackOffer{ExpiresAt: "2020-11-16T12:00:00Z", PercentOff: 50, Plan: PlanMonthly, Store: WinbackOfferStoreStripe}
	deadline := offer.ExpiresAtUnixMillis()
	connect.AssertEqual(t, true, offer.IsActiveAt(deadline-1))
	connect.AssertEqual(t, false, offer.IsActiveAt(deadline))
	connect.AssertEqual(t, false, offer.IsActiveAt(deadline+1))
	connect.AssertEqual(t, false, offer.IsActive())
	connect.AssertEqual(t, false, (&WinbackOffer{ExpiresAt: "garbage", PercentOff: 50}).IsActiveAt(0))
	connect.AssertEqual(t, int64(0), (&WinbackOffer{ExpiresAt: "garbage"}).ExpiresAtUnixMillis())
	connect.AssertEqual(t, false, (&WinbackOffer{ExpiresAt: "2099-11-16T12:00:00Z"}).IsActive())

	// no offer, and an older server
	for _, body := range []string{`{"balance_byte_count": 1, "winback_offer": null}`, `{"balance_byte_count": 1}`} {
		var old SubscriptionBalanceResult
		connect.AssertEqual(t, nil, json.Unmarshal([]byte(body), &old))
		connect.AssertEqual(t, true, old.WinbackOffer == nil)
		out, err := json.Marshal(&old)
		connect.AssertEqual(t, nil, err)
		connect.AssertEqual(t, false, strings.Contains(string(out), "winback_offer"))
	}
}

// TestNetworkCreateArgsProductUpdates pins the sign-up opt-out wire field: the
// zero value sends product_updates true, the opt-out sends false.
func TestNetworkCreateArgsProductUpdates(t *testing.T) {
	out, err := json.Marshal(&NetworkCreateArgs{UserName: "a", NetworkName: "n", Terms: true})
	connect.AssertEqual(t, nil, err)
	var wire map[string]any
	connect.AssertEqual(t, nil, json.Unmarshal(out, &wire))
	connect.AssertEqual(t, true, wire["product_updates"])
	connect.AssertEqual(t, "n", wire["network_name"])
	connect.AssertEqual(t, true, wire["terms"])

	out, err = json.Marshal(NetworkCreateArgs{UserName: "a", ProductUpdatesOptOut: true, ReferralCode: "code"})
	connect.AssertEqual(t, nil, err)
	connect.AssertEqual(t, nil, json.Unmarshal(out, &wire))
	connect.AssertEqual(t, false, wire["product_updates"])
	connect.AssertEqual(t, "code", wire["referral_code"])
	_, hasOptOut := wire["ProductUpdatesOptOut"]
	connect.AssertEqual(t, false, hasOptOut)
}

// TestNetworkCreateArgsProductUpdatesRoundTrip pins the json boundary the C
// ABI uses: marshal then unmarshal preserves the opt-out both ways, and an
// absent product_updates field means opted in.
func TestNetworkCreateArgsProductUpdatesRoundTrip(t *testing.T) {
	for _, optOut := range []bool{false, true} {
		out, err := json.Marshal(&NetworkCreateArgs{UserName: "a", NetworkName: "n", Terms: true, ProductUpdatesOptOut: optOut})
		connect.AssertEqual(t, nil, err)
		var back NetworkCreateArgs
		connect.AssertEqual(t, nil, json.Unmarshal(out, &back))
		connect.AssertEqual(t, optOut, back.ProductUpdatesOptOut)
		connect.AssertEqual(t, "n", back.NetworkName)
		connect.AssertEqual(t, true, back.Terms)
	}

	var absent NetworkCreateArgs
	connect.AssertEqual(t, nil, json.Unmarshal([]byte(`{"user_name":"a","terms":true}`), &absent))
	connect.AssertEqual(t, false, absent.ProductUpdatesOptOut)

	var explicitTrue NetworkCreateArgs
	connect.AssertEqual(t, nil, json.Unmarshal([]byte(`{"user_name":"a","product_updates":true}`), &explicitTrue))
	connect.AssertEqual(t, false, explicitTrue.ProductUpdatesOptOut)

	var explicitFalse NetworkCreateArgs
	connect.AssertEqual(t, nil, json.Unmarshal([]byte(`{"user_name":"a","product_updates":false}`), &explicitFalse))
	connect.AssertEqual(t, true, explicitFalse.ProductUpdatesOptOut)
}
