package sdk

import (
	"encoding/json"
	"testing"
)

// The referral terms ride along with the referral code (server pro.yml is the
// single source of truth); the display helpers turn bytes-per-period into the
// whole GiB/day the apps print, and treat missing terms as zero.
func TestReferralTermsDisplayHelpers(t *testing.T) {
	var result GetNetworkReferralCodeResult
	err := json.Unmarshal([]byte(`{
		"referral_code": "TZ1TJX",
		"total_referrals": 25,
		"max_referrals": 20,
		"bonus_per_referral_bytes": 3221225472,
		"referred_bonus_bytes": 3221225472,
		"bonus_period_seconds": 86400
	}`), &result)
	if err != nil {
		t.Fatal(err)
	}
	if result.MaxReferrals != 20 {
		t.Fatalf("max referrals: %d", result.MaxReferrals)
	}
	if gib := result.BonusGibPerDay(); gib != 3 {
		t.Fatalf("bonus gib/day: %d", gib)
	}
	if gib := result.ReferredBonusGibPerDay(); gib != 3 {
		t.Fatalf("referred bonus gib/day: %d", gib)
	}
	if paid := result.PaidReferrals(25); paid != 20 {
		t.Fatalf("paid referrals over the cap: %d", paid)
	}
	if paid := result.PaidReferrals(4); paid != 4 {
		t.Fatalf("paid referrals under the cap: %d", paid)
	}
	if paid := result.PaidReferrals(-1); paid != 0 {
		t.Fatalf("paid referrals negative: %d", paid)
	}

	// a 12h period doubles the daily figure
	result.BonusPeriodSeconds = 12 * 60 * 60
	if gib := result.BonusGibPerDay(); gib != 6 {
		t.Fatalf("bonus gib/day at 12h: %d", gib)
	}

	// no terms (server without pro.yml): zero, and no cap
	var none GetNetworkReferralCodeResult
	if err := json.Unmarshal([]byte(`{"referral_code": "x", "total_referrals": 3}`), &none); err != nil {
		t.Fatal(err)
	}
	if none.MaxReferrals != 0 || none.BonusGibPerDay() != 0 || none.ReferredBonusGibPerDay() != 0 {
		t.Fatalf("absent terms should be zero: %+v", none)
	}
	if paid := none.PaidReferrals(1000); paid != 1000 {
		t.Fatalf("no cap should pay every referral: %d", paid)
	}
}
