package sdk

import (
	"encoding/json"
	"testing"
)

// decodeTestBalance decodes a /subscription/balance body the way the api does.
func decodeTestBalance(t *testing.T, body string) *SubscriptionBalanceResult {
	t.Helper()
	var result SubscriptionBalanceResult
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatal(err)
	}
	return &result
}

// A legacy guest network has no login method. Every token refresh signs the
// jwt without the guest_mode claim, so after one refresh the claim no longer
// marks the guest. The server reports it on the balance instead (`guest`),
// and IsGuest must follow it, or the apps sell a plan to a network nothing can
// sign back in to.
func TestRefreshedLegacyGuestIsStillAGuest(t *testing.T) {
	stub := newBalanceFetchStub(nil)
	// the refreshed jwt: guest_mode is false
	vc, closeVc := newTestSubscriptionBalanceVc(t, testSubscriptionJwt(t, false, false), stub)
	defer closeVc()
	vc.stateLock.Lock()
	vc.applyJwtClaimsLocked()
	generation := vc.generation
	vc.stateLock.Unlock()

	vc.fetchDone(generation, decodeTestBalance(t, `{
		"start_balance_byte_count": 0,
		"balance_byte_count": 0,
		"open_transfer_byte_count": 0,
		"pending_payout_usd_nano_cents": 0,
		"update_time": "2026-10-03T00:00:00Z",
		"onboarding_offer": null,
		"guest": true
	}`), nil)

	if !vc.GetIsLoaded() {
		t.Fatal("snapshot did not load")
	}
	if !vc.GetIsGuest() {
		t.Fatal("refreshed legacy guest (guest_mode=false, server guest=true) is not a guest")
	}

	// a login method was added: the server stops reporting a guest
	vc.fetchDone(generation, decodeTestBalance(t, `{"guest": false}`), nil)
	if vc.GetIsGuest() {
		t.Fatal("still a guest after the server reported a login method")
	}
}

// A guest whose jwt still carries guest_mode stays a guest until the jwt is
// re-signed, whatever the server says (an older server sends no `guest`).
func TestGuestClaimStillCountsWithoutServerGuest(t *testing.T) {
	stub := newBalanceFetchStub(nil)
	vc, closeVc := newTestSubscriptionBalanceVc(t, testSubscriptionJwt(t, false, true), stub)
	defer closeVc()
	vc.stateLock.Lock()
	vc.applyJwtClaimsLocked()
	generation := vc.generation
	vc.stateLock.Unlock()

	vc.fetchDone(generation, decodeTestBalance(t, `{"balance_byte_count": 0}`), nil)
	if !vc.GetIsGuest() {
		t.Fatal("guest_mode claim ignored once a snapshot without `guest` loaded")
	}
}

func TestSubscriptionBalanceResultDecodesGuest(t *testing.T) {
	if !decodeTestBalance(t, `{"guest": true}`).Guest {
		t.Fatal(`"guest": true did not decode`)
	}
	if decodeTestBalance(t, `{"balance_byte_count": 1}`).Guest {
		t.Fatal("an older server's balance (no guest) decoded as a guest")
	}
}
