package sdk

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
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
