package sdk

import (
	"net/url"
	"strings"
	"testing"
)

// The catalog constants must match the strings the apps and the server use
// today (verified against the app repos; see the doc comments in
// payment_catalog.go for the hardcode sites). A drift here would silently
// break checkout item resolution, so the strings are pinned.
func TestPaymentCatalogConstants(t *testing.T) {
	pins := map[string]string{
		"SubscriptionPlanSupporter":       SubscriptionPlanSupporter,
		"StripeItemProMonthly":            StripeItemProMonthly,
		"StripeItemProYearly":             StripeItemProYearly,
		"StripeItemData1Tib":              StripeItemData1Tib,
		"StripeItemData10Tib":             StripeItemData10Tib,
		"StripeUiModeHosted":              StripeUiModeHosted,
		"StripeUiModeEmbedded":            StripeUiModeEmbedded,
		"StripeRedirectOnCompletionNever": StripeRedirectOnCompletionNever,
		"CheckoutBridgeUrl":               CheckoutBridgeUrl,
		"CheckoutRedirectLink":            CheckoutRedirectLink,
	}
	expected := map[string]string{
		"SubscriptionPlanSupporter":       "supporter",
		"StripeItemProMonthly":            "pro_monthly",
		"StripeItemProYearly":             "pro_yearly",
		"StripeItemData1Tib":              "data_1tib",
		"StripeItemData10Tib":             "data_10tib",
		"StripeUiModeHosted":              "hosted",
		"StripeUiModeEmbedded":            "embedded",
		"StripeRedirectOnCompletionNever": "never",
		"CheckoutBridgeUrl":               "https://ur.io/checkout",
		"CheckoutRedirectLink":            "urnetwork://checkout",
	}
	for name, value := range pins {
		if value != expected[name] {
			t.Errorf("%s = %q, want %q", name, value, expected[name])
		}
	}
}

func TestClassifySubscriptionStore(t *testing.T) {
	cases := []struct {
		store    string
		expected string
	}{
		// stripe family
		{"stripe", SubscriptionStoreStripe},
		{"Stripe", SubscriptionStoreStripe},
		{"stripe_subscription", SubscriptionStoreStripe},
		// apple family (the web regex: apple|itunes|ios|app.?store)
		{"apple", SubscriptionStoreApple},
		{"Apple", SubscriptionStoreApple},
		{"itunes", SubscriptionStoreApple},
		{"iOS", SubscriptionStoreApple},
		{"app store", SubscriptionStoreApple},
		{"appstore", SubscriptionStoreApple},
		{"app_store", SubscriptionStoreApple},
		{"App Store", SubscriptionStoreApple},
		// google family (google|play|android)
		{"google", SubscriptionStoreGoogle},
		{"Google Play", SubscriptionStoreGoogle},
		{"play", SubscriptionStoreGoogle},
		{"android", SubscriptionStoreGoogle},
		// stripe wins over the others when both match (web switch order)
		{"stripe-apple-bridge", SubscriptionStoreStripe},
		// unknown but non-empty
		{"solana", SubscriptionStoreOther},
		{"coinbase", SubscriptionStoreOther},
		// empty
		{"", ""},
	}
	for _, c := range cases {
		if got := ClassifySubscriptionStore(c.store); got != c.expected {
			t.Errorf("ClassifySubscriptionStore(%q) = %q, want %q", c.store, got, c.expected)
		}
	}
}

func TestBuildCheckoutBridgeUrlRoundTrip(t *testing.T) {
	// nasty characters that must survive percent-encoding: url metacharacters,
	// spaces, plus signs, percent, non-ascii
	secrets := []string{
		"cs_test_a1B2c3",
		"cs_test_a&b=c?d#e/f",
		"cs with spaces",
		"cs+plus%25percent",
		"cs_ünïcode✓",
	}
	for _, secret := range secrets {
		built := BuildCheckoutBridgeUrl(secret)
		if !strings.HasPrefix(built, CheckoutBridgeUrl+"?") {
			t.Fatalf("built url %q does not start with the bridge page", built)
		}
		u, err := url.Parse(built)
		if err != nil {
			t.Fatalf("built url %q does not parse: %v", built, err)
		}
		// the bridge page reads the query with URLSearchParams
		// (form-urlencoding); url.ParseQuery matches those semantics
		values, err := url.ParseQuery(u.RawQuery)
		if err != nil {
			t.Fatalf("built query %q does not parse: %v", u.RawQuery, err)
		}
		if got := values.Get("client_secret"); got != secret {
			t.Errorf("client_secret round trip: got %q, want %q", got, secret)
		}
		if got := values.Get("redirect_link"); got != CheckoutRedirectLink {
			t.Errorf("redirect_link = %q, want %q", got, CheckoutRedirectLink)
		}
	}
}

func TestBuildCheckoutBridgeUrlWithRedirect(t *testing.T) {
	built := BuildCheckoutBridgeUrlWithRedirect("cs_1", "urnetwork://checkout?src=test")
	u, _ := url.Parse(built)
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		t.Fatal(err)
	}
	if got := values.Get("redirect_link"); got != "urnetwork://checkout?src=test" {
		t.Errorf("custom redirect_link = %q", got)
	}
}

func TestParseCheckoutRedirect(t *testing.T) {
	// success hand-back, exactly as the bridge page emits it
	redirect, err := ParseCheckoutRedirect("urnetwork://checkout?status=complete&session_id=cs_test_123")
	if err != nil {
		t.Fatal(err)
	}
	if !redirect.Complete {
		t.Error("status=complete did not set Complete")
	}
	if redirect.SessionId != "cs_test_123" {
		t.Errorf("SessionId = %q", redirect.SessionId)
	}
	if redirect.ErrorCode != "" || redirect.ErrorMessage != "" {
		t.Errorf("unexpected error fields: %q %q", redirect.ErrorCode, redirect.ErrorMessage)
	}

	// error hand-back: the bridge assembles the query with URLSearchParams,
	// which form-encodes spaces as "+" -- the parser must decode them back
	redirect, err = ParseCheckoutRedirect("urnetwork://checkout?errorCode=-1&errorMessage=Payment+failed%3A+card+declined+%26+retried")
	if err != nil {
		t.Fatal(err)
	}
	if redirect.Complete {
		t.Error("error redirect must not be Complete")
	}
	if redirect.ErrorCode != "-1" {
		t.Errorf("ErrorCode = %q, want -1", redirect.ErrorCode)
	}
	if redirect.ErrorMessage != "Payment failed: card declined & retried" {
		t.Errorf("ErrorMessage = %q", redirect.ErrorMessage)
	}

	// scheme/host are matched case-insensitively
	if _, err := ParseCheckoutRedirect("URNETWORK://Checkout?status=complete"); err != nil {
		t.Errorf("case-insensitive scheme/host rejected: %v", err)
	}

	// non-checkout uris are rejected
	for _, uri := range []string{
		"https://ur.io/checkout?status=complete",
		"urnetwork://wallet?status=complete",
		"not a uri at all\x7f://",
	} {
		if _, err := ParseCheckoutRedirect(uri); err == nil {
			t.Errorf("ParseCheckoutRedirect(%q) unexpectedly succeeded", uri)
		}
		if IsCheckoutRedirect(uri) {
			t.Errorf("IsCheckoutRedirect(%q) = true", uri)
		}
	}
	if !IsCheckoutRedirect("urnetwork://checkout?status=complete") {
		t.Error("IsCheckoutRedirect rejected a checkout redirect")
	}
}

func TestIsBalanceCodeFormatValid(t *testing.T) {
	valid26 := "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	if len(valid26) != BalanceCodeLength {
		t.Fatal("test fixture is not 26 chars")
	}
	cases := []struct {
		secret   string
		expected bool
	}{
		{valid26, true},
		{"  " + valid26 + "\n", true}, // surrounding whitespace is trimmed
		{valid26[:25], false},
		{valid26 + "A", false},
		{"", false},
		{"   ", false},
	}
	for _, c := range cases {
		if got := IsBalanceCodeFormatValid(c.secret); got != c.expected {
			t.Errorf("IsBalanceCodeFormatValid(%q) = %v, want %v", c.secret, got, c.expected)
		}
	}
}

func TestClassifyBalanceCodeRedeem(t *testing.T) {
	secret := "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	success := &RedeemBalanceCodeResult{
		TransferBalance: &RedeemBalanceCodeTransferBalance{
			BalanceByteCount: 1024,
		},
	}
	// the server's single error payload, sent for BOTH a nonexistent code and
	// a code already redeemed (by anyone)
	unknownCode := &RedeemBalanceCodeResult{
		Error: &RedeemBalanceCodeError{
			Message: "Unknown balance code.",
		},
	}
	redeemedByUs := NewRedeemedBalanceCodeList()
	redeemedByUs.Add(&RedeemedBalanceCode{Secret: secret})
	redeemedOther := NewRedeemedBalanceCodeList()
	redeemedOther.Add(&RedeemedBalanceCode{Secret: "ZZZZZZZZZZZZZZZZZZZZZZZZZZ"})

	cases := []struct {
		name     string
		result   *RedeemBalanceCodeResult
		codes    *RedeemedBalanceCodeList
		expected string
	}{
		{"success", success, nil, BalanceCodeRedeemOutcomeRedeemed},
		{"success ignores list", success, redeemedByUs, BalanceCodeRedeemOutcomeRedeemed},
		{"server error, no list", unknownCode, nil, BalanceCodeRedeemOutcomeInvalid},
		{"server error, not ours", unknownCode, redeemedOther, BalanceCodeRedeemOutcomeInvalid},
		// the D2/N7 fix: the code is in OUR redeemed list, so "Unknown balance
		// code." means we already have the data -- never "invalid"
		{"server error, already ours", unknownCode, redeemedByUs, BalanceCodeRedeemOutcomeAlreadyRedeemed},
		// transport failure: outcome unknown unless the list proves credit
		{"transport error, no list", nil, nil, BalanceCodeRedeemOutcomeUnknown},
		{"transport error, not ours", nil, redeemedOther, BalanceCodeRedeemOutcomeUnknown},
		// network blip AFTER the server committed: the list shows the credit
		{"transport error, already ours", nil, redeemedByUs, BalanceCodeRedeemOutcomeAlreadyRedeemed},
		{"empty result", &RedeemBalanceCodeResult{}, nil, BalanceCodeRedeemOutcomeUnknown},
	}
	for _, c := range cases {
		if got := ClassifyBalanceCodeRedeem(c.result, c.codes, secret); got != c.expected {
			t.Errorf("%s: got %q, want %q", c.name, got, c.expected)
		}
	}

	// secrets compare trimmed and case-insensitively (users retype codes)
	if got := ClassifyBalanceCodeRedeem(unknownCode, redeemedByUs, "  "+strings.ToLower(secret)+" "); got != BalanceCodeRedeemOutcomeAlreadyRedeemed {
		t.Errorf("lenient secret compare: got %q", got)
	}
}
