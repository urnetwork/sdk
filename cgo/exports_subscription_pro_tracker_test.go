// The balance's Pro and jwt refresh decision (sdk SubscriptionProTracker)
// through the real c abi, as the desktop balance stores call it (support inbox
// 8211): a claim that disagrees with the server asks for one refresh, the same
// jwt again asks for none and never moves Pro back to its claim, and a renewed
// jwt that still disagrees asks once more.
package main

import (
	"encoding/base64"
	"fmt"
	"testing"
)

// A synthetic unsigned network credential with a `pro` claim. The issue time
// makes a renewal of the same claims another jwt.
func testingProTrackerJwt(pro bool, issuedAt int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(
		`{"network_id":"00000000-0000-0000-0000-000000000011","pro":%t,"iat":%d}`,
		pro,
		issuedAt,
	)))
	return fmt.Sprintf("%s.%s.", header, payload)
}

func TestSubscriptionProTrackerThroughTheAbi(t *testing.T) {
	tracker := socketABICall(urnet_new_subscription_pro_tracker)[0].Uint()
	if tracker == 0 {
		t.Fatal("no tracker handle")
	}
	defer handleRelease(tracker)

	setJwt := func(byJwt string) bool {
		t.Helper()
		byJwtC := cString(byJwt)
		defer cStringFree(byJwtC)
		return socketABICall(urnet_subscription_pro_tracker_set_jwt, tracker, byJwtC)[0].Bool()
	}
	balanceLoaded := func(serverIsPro bool) bool {
		t.Helper()
		return socketABICall(urnet_subscription_pro_tracker_balance_loaded, tracker, serverIsPro)[0].Bool()
	}
	isPro := func() bool {
		t.Helper()
		return socketABICall(urnet_subscription_pro_tracker_get_is_pro, tracker)[0].Bool()
	}

	// the stored jwt says free; before a load, Pro is its claim
	staleJwt := testingProTrackerJwt(false, 1)
	setJwt(staleJwt)
	if isPro() {
		t.Fatal("Pro before a load is not the jwt's claim")
	}

	// the server says Pro: one refresh, and Pro is the server's
	if !balanceLoaded(true) {
		t.Fatal("a claim that disagrees asked for no refresh")
	}
	for i := 0; i < 5; i += 1 {
		// the refresh renewed the client token: the stored jwt comes back
		if setJwt(staleJwt) {
			t.Fatalf("read %d: the same jwt changed Pro", i)
		}
		if balanceLoaded(true) {
			t.Fatalf("read %d: asked again for the jwt it already asked about", i)
		}
		if !isPro() {
			t.Fatalf("read %d: Pro flipped back to the stale claim", i)
		}
	}

	// a renewed jwt that still disagrees asks once more
	setJwt(testingProTrackerJwt(false, 2))
	if !balanceLoaded(true) || balanceLoaded(true) {
		t.Fatal("a renewed jwt that still disagrees must ask exactly once")
	}

	// a sign out forgets the server's answer
	socketABICall(urnet_subscription_pro_tracker_reset, tracker)
	if isPro() {
		t.Fatal("Pro after reset")
	}
}
