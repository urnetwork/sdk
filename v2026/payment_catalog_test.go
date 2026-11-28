package sdk

import (
	"net/url"
	"regexp"
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
		// the one-time crypto markets: nothing renews, nothing to cancel
		{"solana", SubscriptionStoreCrypto},
		{"Solana", SubscriptionStoreCrypto},
		{"monero", SubscriptionStoreCrypto},
		{"zcash", SubscriptionStoreCrypto},
		// TAO, Bitcoin and ETH's markets (SOL's is solana), "other" before
		// 2026-11-10
		{"bittensor", SubscriptionStoreCrypto},
		{"bitcoin", SubscriptionStoreCrypto},
		{"Bitcoin", SubscriptionStoreCrypto},
		{"ethereum", SubscriptionStoreCrypto},
		// a redeemed gift code's year of Pro: exactly "gift" in any case,
		// "other" before gift codes
		{"gift", SubscriptionStoreGift},
		{"Gift", SubscriptionStoreGift},
		{"GIFT", SubscriptionStoreGift},
		// a store id that only contains "gift" keeps its family
		{"stripe_gift", SubscriptionStoreStripe},
		{"gift_play", SubscriptionStoreGoogle},
		{"gift-solana", SubscriptionStoreCrypto},
		{"gifts", SubscriptionStoreOther},
		{"giftcard", SubscriptionStoreOther},
		{" gift", SubscriptionStoreOther},
		// unknown but non-empty
		{"coinbase", SubscriptionStoreOther},
		{"manual", SubscriptionStoreOther},
		{"x402", SubscriptionStoreOther},
		// empty
		{"", ""},
	}
	for _, c := range cases {
		if got := ClassifySubscriptionStore(c.store); got != c.expected {
			t.Errorf("ClassifySubscriptionStore(%q) = %q, want %q", c.store, got, c.expected)
		}
	}
}

// The gift store is a family of its own: none of the substring families
// matches it, so the exact check that comes first takes no store from them.
func TestClassifySubscriptionStoreGiftIsNoOtherFamilys(t *testing.T) {
	families := map[string]*regexp.Regexp{
		SubscriptionStoreStripe: subscriptionStoreStripeRe,
		SubscriptionStoreApple:  subscriptionStoreAppleRe,
		SubscriptionStoreGoogle: subscriptionStoreGoogleRe,
		SubscriptionStoreCrypto: subscriptionStoreCryptoRe,
	}
	for family, re := range families {
		if re.MatchString(SubscriptionStoreGift) {
			t.Errorf("the %s family matches the store %q", family, SubscriptionStoreGift)
		}
	}
}

// A credit in any coin this sdk knows is on its coin's market, which is one
// time: a coin added without its market in the crypto family would offer a
// manage link for a payment that never renews.
func TestClassifySubscriptionStoreKnowsEveryCoinsMarket(t *testing.T) {
	for _, asset := range cryptoAssetOrder {
		market := CryptoAssetMarket(asset)
		if got := ClassifySubscriptionStore(market); got != SubscriptionStoreCrypto {
			t.Errorf("the market %q of %q classifies as %q, want %q", market, asset, got, SubscriptionStoreCrypto)
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

// A session created with redirect_on_completion "never" completes through
// Stripe's onComplete callback only; its bridge url must say so, or the bridge
// never hands control back to the app.
func TestBuildInlineCheckoutBridgeUrl(t *testing.T) {
	secrets := []string{
		"cs_test_a1B2c3_secret_z9",
		"cs_test_a&b=c?d#e/f",
		"cs+plus%25percent",
	}
	for _, secret := range secrets {
		built := BuildInlineCheckoutBridgeUrl(secret)
		if !strings.HasPrefix(built, CheckoutBridgeUrl+"?") {
			t.Fatalf("built url %q does not start with the bridge page", built)
		}
		u, err := url.Parse(built)
		if err != nil {
			t.Fatalf("built url %q does not parse: %v", built, err)
		}
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
		if got := values.Get("redirect_on_completion"); got != StripeRedirectOnCompletionNever {
			t.Errorf("redirect_on_completion = %q, want %q", got, StripeRedirectOnCompletionNever)
		}
	}
	// the return_url bridge url is unchanged for older clients
	values, _ := url.ParseQuery(strings.SplitN(BuildCheckoutBridgeUrl("cs_1"), "?", 2)[1])
	if _, ok := values["redirect_on_completion"]; ok {
		t.Errorf("BuildCheckoutBridgeUrl carries redirect_on_completion: %v", values)
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

// The bridge page hands a failure back with its own code next to its English
// text, and the parsed hand-back carries both as sent: an app shows its own
// words for a code it knows and the text for any other, which includes the -1
// of pages before the codes.
func TestParseCheckoutRedirectPassesThePageErrorCode(t *testing.T) {
	// the codes are a contract with ur.io's EmbeddedCheckout.jsx
	for code, want := range map[string]string{
		CheckoutBridgeErrorInvalidRequest:    "invalid_request",
		CheckoutBridgeErrorUnavailable:       "checkout_unavailable",
		CheckoutBridgeErrorStripeUnavailable: "stripe_unavailable",
		CheckoutBridgeErrorCheckout:          "checkout_error",
	} {
		if code != want {
			t.Errorf("checkout bridge error code %q, want %q", code, want)
		}
	}

	for _, c := range []struct {
		errorCode    string
		errorMessage string
	}{
		{errorCode: CheckoutBridgeErrorInvalidRequest, errorMessage: "This page was opened without a checkout session."},
		{errorCode: CheckoutBridgeErrorUnavailable, errorMessage: "Checkout is not configured."},
		{errorCode: CheckoutBridgeErrorStripeUnavailable, errorMessage: "Could not load Stripe."},
		{errorCode: CheckoutBridgeErrorCheckout, errorMessage: "The checkout session cs_test_a1B2 is not valid."},
		{errorCode: "checkout_paused", errorMessage: "Checkout is paused."},
		{errorCode: "-1", errorMessage: "Could not start checkout. Please try again."},
	} {
		values := url.Values{"errorCode": {c.errorCode}, "errorMessage": {c.errorMessage}}
		redirect, err := ParseCheckoutRedirect(CheckoutRedirectLink + "?" + values.Encode())
		if err != nil {
			t.Fatalf("%s: %v", c.errorCode, err)
		}
		if redirect.Complete || redirect.SessionId != "" || redirect.ErrorCode != c.errorCode || redirect.ErrorMessage != c.errorMessage {
			t.Errorf("%s: got %+v, want the code and %q", c.errorCode, redirect, c.errorMessage)
		}
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
		// as a gift card prints it, and as people type that
		{"ABCD EFGH IJKL MNOP QRST UVWX YZ", true},
		{"ABCD-EFGH-IJKL-MNOP-QRST-UVWX-YZ", true},
		{"abcd\u2013efgh\u2013ijkl\u2013mnop\u2013qrst\u2013uvwx\u2013yz", true},
		{"ABCD.EFGH.IJKL.MNOP.QRST.UVWX.YZ", true},
		{"ABCD EFGH IJKL MNOP QRST UVWX", false},
		{"ABCD EFGH IJKL MNOP QRST UVWX YZA", false},
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

	// a code typed as a gift card prints it, whose answer was lost after the
	// server committed: the list still proves the credit, as the server's
	// fallback reads the typed code
	giftSecret := "K7Q4M2XDOIBA2345673ABCDEFG"
	redeemedGift := NewRedeemedBalanceCodeList()
	redeemedGift.Add(&RedeemedBalanceCode{Secret: giftSecret, Kind: BalanceCodeKindProGift, ProDays: 365})
	for _, typed := range []string{
		"K7Q4 M2XD OIBA 2345 673A BCDE FG",
		"K7Q4-M2XD-OIBA-2345-673A-BCDE-FG",
		"k7q4 m2xd oiba 2345 673a bcde fg",
		"K7Q4\u2014M2XD\u2014OIBA\u20142345\u2014673A\u2014BCDE\u2014FG",
		"K7Q4.M2XD.OIBA.2345.673A.BCDE.FG",
		// 0, 1 and 8 typed for the O, I and B they look like on the card
		"K7Q4 M2XD 0I8A 2345 673A BCDE FG",
		"K7Q4 M2XD O18A 2345 673A BCDE FG",
	} {
		if got := ClassifyBalanceCodeRedeem(nil, redeemedGift, typed); got != BalanceCodeRedeemOutcomeAlreadyRedeemed {
			t.Errorf("lost answer, typed %q: got %q, want %q", typed, got, BalanceCodeRedeemOutcomeAlreadyRedeemed)
		}
		if got := ClassifyBalanceCodeRedeem(unknownCode, redeemedGift, typed); got != BalanceCodeRedeemOutcomeAlreadyRedeemed {
			t.Errorf("retry after commit, typed %q: got %q, want %q", typed, got, BalanceCodeRedeemOutcomeAlreadyRedeemed)
		}
	}
	// another code, however typed, is not ours
	for _, typed := range []string{"K7Q4 M2XD OIBA 2345 673A BCDE FH", "K7Q4-M2XD-OIBA-2345-673A-BCDE"} {
		if got := ClassifyBalanceCodeRedeem(nil, redeemedGift, typed); got != BalanceCodeRedeemOutcomeUnknown {
			t.Errorf("lost answer, other code %q: got %q, want %q", typed, got, BalanceCodeRedeemOutcomeUnknown)
		}
	}
	// a legacy secret with digits still matches itself, however grouped
	legacy := NewRedeemedBalanceCodeList()
	legacy.Add(&RedeemedBalanceCode{Secret: "legacy-code-1080"})
	if got := ClassifyBalanceCodeRedeem(nil, legacy, "LEGACY CODE 1080"); got != BalanceCodeRedeemOutcomeAlreadyRedeemed {
		t.Errorf("legacy secret: got %q", got)
	}
	// nothing typed is never an earlier redeem, even of an empty secret
	empty := NewRedeemedBalanceCodeList()
	empty.Add(&RedeemedBalanceCode{Secret: ""})
	if got := ClassifyBalanceCodeRedeem(nil, empty, " - "); got != BalanceCodeRedeemOutcomeUnknown {
		t.Errorf("empty secret: got %q", got)
	}
}

// A typed code reads as the server's fallback lookup reads it (server model
// normalizeBalanceCodeSecret and unconfuseBalanceCodeSecret): whitespace,
// dashes and dots removed, upper case, then 0, 1 and 8 as O, I and B.
func TestNormalizeBalanceCodeSecret(t *testing.T) {
	for typed, want := range map[string]string{
		"K7Q4M2XDOIBA2345673ABCDEFG":                             "K7Q4M2XDOIBA2345673ABCDEFG",
		" k7q4 m2xd\toiba\n2345 ":                                "K7Q4M2XDOIBA2345",
		"K7Q4-M2XD\u2010OIBA\u2011A\u2012B\u2013C\u2014D\u2015E": "K7Q4M2XDOIBAABCDE",
		"K7Q4.M2XD":            "K7Q4M2XD",
		"0123456789":           "OI234567B9",
		"\u00a0K7Q4\u3000M2XD": "K7Q4M2XD",
		"":                     "",
	} {
		if got := normalizeBalanceCodeSecret(typed); got != want {
			t.Errorf("normalizeBalanceCodeSecret(%q) = %q, want %q", typed, got, want)
		}
	}
}
