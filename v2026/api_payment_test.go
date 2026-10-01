package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// newTestPaymentApi builds an Api against a local test server.
func newTestPaymentApi(t *testing.T, handler http.HandlerFunc) *Api {
	t.Helper()
	_, api := newTestApi(t, handler)
	api.SetByJwt("test-network-jwt")
	return api
}

func awaitApiResult[R any](t *testing.T, c chan connect.ApiCallbackResult[R], message string) connect.ApiCallbackResult[R] {
	t.Helper()
	select {
	case r := <-c:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal(message)
		panic("unreachable")
	}
}

func TestSubscriptionBalanceDecode(t *testing.T) {
	var path atomic.Value
	api := newTestPaymentApi(t, func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"start_balance_byte_count": 1000,
			"balance_byte_count": 600,
			"open_transfer_byte_count": 100,
			"current_subscription": {
				"subscription_id": "00000000-0000-0000-0000-000000000021",
				"store": "stripe",
				"plan": "supporter"
			},
			"subscriptions": [
				{"subscription_id": "00000000-0000-0000-0000-000000000021", "store": "stripe", "plan": "supporter"},
				{"subscription_id": "00000000-0000-0000-0000-000000000022", "store": "play_store", "plan": "supporter"}
			],
			"active_transfer_balances": [
				{
					"balance_id": "00000000-0000-0000-0000-000000000031",
					"network_id": "00000000-0000-0000-0000-000000000011",
					"start_time": "2026-08-07T00:00:00Z",
					"end_time": "2026-09-07T00:00:00Z",
					"start_balance_byte_count": 1000,
					"net_revenue_nano_cents": 5000000000,
					"balance_byte_count": 600
				}
			],
			"pending_payout_usd_nano_cents": 123,
			"update_time": "2026-08-07T12:00:00Z"
		}`)
	})

	callback, c := connect.NewBlockingApiCallback[*SubscriptionBalanceResult](context.Background())
	api.SubscriptionBalance(callback)
	r := awaitApiResult(t, c, "SubscriptionBalance never returned")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if got := path.Load(); got != "/subscription/balance" {
		t.Errorf("path = %v", got)
	}
	result := r.Result
	if result.StartBalanceByteCount != 1000 || result.BalanceByteCount != 600 || result.OpenTransferByteCount != 100 {
		t.Errorf("byte counts = %d/%d/%d", result.StartBalanceByteCount, result.BalanceByteCount, result.OpenTransferByteCount)
	}
	if result.CurrentSubscription == nil ||
		result.CurrentSubscription.Plan != SubscriptionPlanSupporter ||
		result.CurrentSubscription.Store != "stripe" {
		t.Errorf("current subscription = %+v", result.CurrentSubscription)
	}
	if result.Subscriptions == nil || result.Subscriptions.Len() != 2 {
		t.Fatalf("subscriptions = %+v", result.Subscriptions)
	}
	if got := ClassifySubscriptionStore(result.Subscriptions.Get(1).Store); got != SubscriptionStoreGoogle {
		t.Errorf("second store classified as %q", got)
	}
	if result.ActiveTransferBalances == nil || result.ActiveTransferBalances.Len() != 1 {
		t.Fatalf("active transfer balances = %+v", result.ActiveTransferBalances)
	}
	// the wire name is net_revenue_nano_cents; an earlier `net_revenue` tag
	// left this 0 in every app
	if got := result.ActiveTransferBalances.Get(0).NetRevenue; got != 5000000000 {
		t.Errorf("net revenue = %d, want 5000000000", got)
	}
	if result.PendingPayoutUsdNanoCents != 123 {
		t.Errorf("pending payout = %d", result.PendingPayoutUsdNanoCents)
	}
}

func TestCheckBalanceCode(t *testing.T) {
	var path, body atomic.Value
	api := newTestPaymentApi(t, func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.URL.Path)
		b, _ := io.ReadAll(r.Body)
		body.Store(string(b))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"balance": {
				"start_time": "2026-08-07T00:00:00Z",
				"end_time": "2027-08-07T00:00:00Z",
				"balance_byte_count": 1099511627776
			}
		}`)
	})

	callback, c := connect.NewBlockingApiCallback[*CheckBalanceCodeResult](context.Background())
	api.CheckBalanceCode(&CheckBalanceCodeArgs{Secret: "ABCDEFGHIJKLMNOPQRSTUVWXYZ"}, callback)
	r := awaitApiResult(t, c, "CheckBalanceCode never returned")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if got := path.Load(); got != "/subscription/check-balance-code" {
		t.Errorf("path = %v", got)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(body.Load().(string)), &sent); err != nil {
		t.Fatal(err)
	}
	if sent["secret"] != "ABCDEFGHIJKLMNOPQRSTUVWXYZ" {
		t.Errorf("sent secret = %v", sent["secret"])
	}
	if r.Result.Balance == nil || r.Result.Balance.BalanceByteCount != 1099511627776 {
		t.Errorf("balance = %+v", r.Result.Balance)
	}
	if r.Result.Balance.StartTime == nil || r.Result.Balance.EndTime == nil {
		t.Error("balance times did not decode")
	}
	if r.Result.Error != nil {
		t.Errorf("unexpected error: %+v", r.Result.Error)
	}
}

func TestCheckBalanceCodeUnknownCode(t *testing.T) {
	api := newTestPaymentApi(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// the server's exact payload (model.CheckBalanceCode), sent both for a
		// nonexistent code and one already redeemed
		fmt.Fprint(w, `{"error":{"message":"Unknown balance code."}}`)
	})

	callback, c := connect.NewBlockingApiCallback[*CheckBalanceCodeResult](context.Background())
	api.CheckBalanceCode(&CheckBalanceCodeArgs{Secret: "ABCDEFGHIJKLMNOPQRSTUVWXYZ"}, callback)
	r := awaitApiResult(t, c, "CheckBalanceCode never returned")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if r.Result.Error == nil || r.Result.Error.Message != "Unknown balance code." {
		t.Errorf("error = %+v", r.Result.Error)
	}
}

func TestRedeemBalanceCodeDecodeAndClassify(t *testing.T) {
	var path atomic.Value
	api := newTestPaymentApi(t, func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"error":{"message":"Unknown balance code."}}`)
	})

	secret := "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	callback, c := connect.NewBlockingApiCallback[*RedeemBalanceCodeResult](context.Background())
	api.RedeemBalanceCode(&RedeemBalanceCodeArgs{Secret: secret}, callback)
	r := awaitApiResult(t, c, "RedeemBalanceCode never returned")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if got := path.Load(); got != "/subscription/redeem-balance-code" {
		t.Errorf("path = %v", got)
	}
	// without the redeemed-code list the payload can only classify as invalid
	if got := ClassifyBalanceCodeRedeem(r.Result, nil, secret); got != BalanceCodeRedeemOutcomeInvalid {
		t.Errorf("classified %q", got)
	}
	// with the list showing our own earlier redeem, the same payload becomes
	// already_redeemed
	redeemed := NewRedeemedBalanceCodeList()
	redeemed.Add(&RedeemedBalanceCode{Secret: secret})
	if got := ClassifyBalanceCodeRedeem(r.Result, redeemed, secret); got != BalanceCodeRedeemOutcomeAlreadyRedeemed {
		t.Errorf("classified %q", got)
	}
}

func TestStripeCreateCheckoutSession(t *testing.T) {
	var path, body atomic.Value
	api := newTestPaymentApi(t, func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.URL.Path)
		b, _ := io.ReadAll(r.Body)
		body.Store(string(b))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"ui_mode": "embedded",
			"client_secret": "cs_test_secret",
			"publishable_key": "pk_test",
			"session_id": "cs_test_123"
		}`)
	})

	callback, c := connect.NewBlockingApiCallback[*StripeCreateCheckoutSessionResult](context.Background())
	api.CreateStripeCheckoutSession(
		&StripeCreateCheckoutSessionArgs{
			ItemId:               StripeItemProYearly,
			UiMode:               StripeUiModeEmbedded,
			RedirectOnCompletion: StripeRedirectOnCompletionNever,
		},
		callback,
	)
	r := awaitApiResult(t, c, "CreateStripeCheckoutSession never returned")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if got := path.Load(); got != "/stripe/create-checkout-session" {
		t.Errorf("path = %v", got)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(body.Load().(string)), &sent); err != nil {
		t.Fatal(err)
	}
	if sent["item_id"] != "pro_yearly" || sent["ui_mode"] != "embedded" || sent["redirect_on_completion"] != "never" {
		t.Errorf("sent args = %v", sent)
	}
	if r.Result.ClientSecret != "cs_test_secret" || r.Result.SessionId != "cs_test_123" {
		t.Errorf("result = %+v", r.Result)
	}
	// the embedded result feeds straight into the shared envelope
	bridge := BuildCheckoutBridgeUrl(r.Result.ClientSecret)
	if !strings.Contains(bridge, "client_secret=cs_test_secret") {
		t.Errorf("bridge url = %q", bridge)
	}
}

func TestStripeCreateCheckoutSessionOmitsEmptyRedirect(t *testing.T) {
	var body atomic.Value
	api := newTestPaymentApi(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body.Store(string(b))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ui_mode":"hosted","checkout_url":"https://checkout.stripe.com/x","session_id":"cs_1"}`)
	})

	callback, c := connect.NewBlockingApiCallback[*StripeCreateCheckoutSessionResult](context.Background())
	api.CreateStripeCheckoutSession(
		&StripeCreateCheckoutSessionArgs{ItemId: StripeItemProMonthly},
		callback,
	)
	r := awaitApiResult(t, c, "CreateStripeCheckoutSession never returned")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	// existing hosted callers must keep sending exactly what they sent before:
	// no redirect_on_completion key at all when unset
	if strings.Contains(body.Load().(string), "redirect_on_completion") {
		t.Errorf("empty redirect_on_completion was serialized: %s", body.Load())
	}
	if r.Result.CheckoutUrl == "" {
		t.Error("hosted checkout_url did not decode")
	}
}

func TestStripePaymentIntentAndCustomerPortal(t *testing.T) {
	paths := make(chan string, 2)
	api := newTestPaymentApi(t, func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/stripe/payment-intent":
			fmt.Fprint(w, `{
				"payment_intents": [{}],
				"ephemeral_key": "ek_test",
				"customer_id": "cus_test",
				"publishable_key": "pk_test"
			}`)
		case "/stripe/customer-portal":
			fmt.Fprint(w, `{"url": "https://billing.stripe.com/session/x"}`)
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	})

	intentCallback, intentC := connect.NewBlockingApiCallback[*StripeCreatePaymentIntentResult](context.Background())
	api.CreateStripePaymentIntent(&StripeCreatePaymentIntentArgs{}, intentCallback)
	intent := awaitApiResult(t, intentC, "CreateStripePaymentIntent never returned")
	if intent.Error != nil {
		t.Fatal(intent.Error)
	}
	if intent.Result.EphemeralKey != "ek_test" || intent.Result.CustomerId != "cus_test" {
		t.Errorf("payment intent result = %+v", intent.Result)
	}

	portalCallback, portalC := connect.NewBlockingApiCallback[*StripeCreateCustomerPortalResult](context.Background())
	api.StripeCreateCustomerPortal(&StripeCreateCustomerPortalArgs{}, portalCallback)
	portal := awaitApiResult(t, portalC, "StripeCreateCustomerPortal never returned")
	if portal.Error != nil {
		t.Fatal(portal.Error)
	}
	if portal.Result.Url == "" {
		t.Error("customer portal url did not decode")
	}

	seen := map[string]bool{<-paths: true, <-paths: true}
	if !seen["/stripe/payment-intent"] || !seen["/stripe/customer-portal"] {
		t.Errorf("paths = %v", seen)
	}
}

// TestStripePaymentSheet pins the inline pay sheet call the Stripe-billed
// apps (the non-Play Android flavors, Windows, Linux and the direct-download
// macOS build) make: the server's route, the wire args, and every field of
// the server's StripePaymentSheetResult, including the RFC 3339 trial end the
// binding carries as a string.
func TestStripePaymentSheet(t *testing.T) {
	var path, method, body atomic.Value
	api := newTestPaymentApi(t, func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.URL.Path)
		method.Store(r.Method)
		b, _ := io.ReadAll(r.Body)
		body.Store(string(b))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"customer_id": "cus_test",
			"ephemeral_key_secret": "ek_test_secret",
			"setup_intent_client_secret": "seti_test_secret",
			"intent_type": "setup",
			"subscription_id": "sub_test",
			"publishable_key": "pk_test",
			"tier": "regional",
			"currency": "USD",
			"plan": "yearly",
			"amount_first_period_usd": 3,
			"regular_period_usd": 4,
			"trial_days": 14,
			"trial_end_at": "2026-10-14T00:00:00Z",
			"offer_applied": true
		}`)
	})

	callback, c := connect.NewBlockingApiCallback[*StripePaymentSheetResult](context.Background())
	api.StripePaymentSheet(
		&StripePaymentSheetArgs{
			Plan:              PlanYearly,
			StorefrontCountry: "RU",
			StripeVersion:     "2024-06-20",
		},
		callback,
	)
	r := awaitApiResult(t, c, "StripePaymentSheet never returned")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if got := path.Load(); got != "/subscription/stripe/payment-sheet" {
		t.Errorf("path = %v", got)
	}
	if got := method.Load(); got != http.MethodPost {
		t.Errorf("method = %v", got)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(body.Load().(string)), &sent); err != nil {
		t.Fatal(err)
	}
	if sent["plan"] != "yearly" || sent["storefront_country"] != "RU" || sent["stripe_version"] != "2024-06-20" {
		t.Errorf("sent args = %v", sent)
	}
	result := r.Result
	if result.Error != nil {
		t.Fatalf("unexpected error: %+v", result.Error)
	}
	if result.CustomerId != "cus_test" ||
		result.EphemeralKeySecret != "ek_test_secret" ||
		result.SetupIntentClientSecret != "seti_test_secret" ||
		result.PaymentIntentClientSecret != "" ||
		result.IntentType != StripeIntentTypeSetup ||
		result.SubscriptionId != "sub_test" ||
		result.PublishableKey != "pk_test" ||
		result.Tier != "regional" ||
		result.Currency != "USD" ||
		result.Plan != PlanYearly {
		t.Errorf("result = %+v", result)
	}
	if result.AmountFirstPeriodUsd != 3 || result.RegularPeriodUsd != 4 || result.TrialDays != 14 || !result.OfferApplied {
		t.Errorf("offer fields = %+v", result)
	}
	if result.TrialEndAt != "2026-10-14T00:00:00Z" {
		t.Errorf("trial_end_at = %q", result.TrialEndAt)
	}
	if _, err := time.Parse(time.RFC3339, result.TrialEndAt); err != nil {
		t.Errorf("trial_end_at is not RFC 3339: %v", err)
	}
}

func TestStripePaymentSheetMonthlyOmitsOptionalArgs(t *testing.T) {
	var body atomic.Value
	api := newTestPaymentApi(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body.Store(string(b))
		w.Header().Set("Content-Type", "application/json")
		// the monthly plan has no trial: a PaymentIntent and no trial_end_at
		fmt.Fprint(w, `{
			"customer_id": "cus_test",
			"ephemeral_key_secret": "ek_test_secret",
			"payment_intent_client_secret": "pi_test_secret",
			"intent_type": "payment",
			"subscription_id": "sub_test",
			"publishable_key": "pk_test",
			"tier": "standard",
			"currency": "USD",
			"plan": "monthly",
			"amount_first_period_usd": 0.5,
			"regular_period_usd": 0.5,
			"trial_days": 0,
			"offer_applied": false
		}`)
	})

	callback, c := connect.NewBlockingApiCallback[*StripePaymentSheetResult](context.Background())
	api.StripePaymentSheet(&StripePaymentSheetArgs{Plan: PlanMonthly}, callback)
	r := awaitApiResult(t, c, "StripePaymentSheet never returned")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	// the server treats a missing storefront and stripe version as unknown;
	// neither key is sent when unset
	sentBody := body.Load().(string)
	if strings.Contains(sentBody, "storefront_country") || strings.Contains(sentBody, "stripe_version") {
		t.Errorf("empty optional args were serialized: %s", sentBody)
	}
	result := r.Result
	if result.IntentType != StripeIntentTypePayment ||
		result.PaymentIntentClientSecret != "pi_test_secret" ||
		result.SetupIntentClientSecret != "" {
		t.Errorf("intent = %+v", result)
	}
	if result.TrialDays != 0 || result.TrialEndAt != "" || result.OfferApplied {
		t.Errorf("trial fields = %+v", result)
	}
}

func TestStripePaymentSheetError(t *testing.T) {
	api := newTestPaymentApi(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// the server's exact payload (controller.stripePaymentSheetError)
		fmt.Fprint(w, `{"amount_first_period_usd":0,"regular_period_usd":0,"trial_days":0,"offer_applied":false,"error":{"message":"Unknown plan."}}`)
	})

	callback, c := connect.NewBlockingApiCallback[*StripePaymentSheetResult](context.Background())
	api.StripePaymentSheet(&StripePaymentSheetArgs{Plan: "weekly"}, callback)
	r := awaitApiResult(t, c, "StripePaymentSheet never returned")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if r.Result.Error == nil || r.Result.Error.Message != "Unknown plan." {
		t.Errorf("error = %+v", r.Result.Error)
	}
	if r.Result.CustomerId != "" || r.Result.SubscriptionId != "" {
		t.Errorf("error result carried ids: %+v", r.Result)
	}
}

// TestStripePrices pins the price lookup the Stripe-billed apps make before
// showing the pay sheet: a GET with the storefront as a query parameter
// (the server reads ?storefront_country), and every field of the server's
// StripePricesResult.
func TestStripePrices(t *testing.T) {
	var path, method, query atomic.Value
	api := newTestPaymentApi(t, func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.URL.Path)
		method.Store(r.Method)
		query.Store(r.URL.Query().Get("storefront_country"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"tier": "regional",
			"currency": "USD",
			"yearly_price_id": "price_yearly_test",
			"monthly_price_id": "price_monthly_test",
			"yearly_usd": 4,
			"monthly_usd": 0.5,
			"publishable_key": "pk_test",
			"onboarding_coupon_id": "onboarding25",
			"offer_eligible": true
		}`)
	})

	callback, c := connect.NewBlockingApiCallback[*StripePricesResult](context.Background())
	api.StripePrices("RU", callback)
	r := awaitApiResult(t, c, "StripePrices never returned")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if got := path.Load(); got != "/subscription/stripe/prices" {
		t.Errorf("path = %v", got)
	}
	if got := method.Load(); got != http.MethodGet {
		t.Errorf("method = %v", got)
	}
	if got := query.Load(); got != "RU" {
		t.Errorf("storefront_country query = %v", got)
	}
	result := r.Result
	if result.Error != nil {
		t.Fatalf("unexpected error: %+v", result.Error)
	}
	if result.Tier != "regional" ||
		result.Currency != "USD" ||
		result.YearlyPriceId != "price_yearly_test" ||
		result.MonthlyPriceId != "price_monthly_test" ||
		result.YearlyUsd != 4 ||
		result.MonthlyUsd != 0.5 ||
		result.PublishableKey != "pk_test" ||
		result.OnboardingCouponId != "onboarding25" ||
		!result.OfferEligible {
		t.Errorf("result = %+v", result)
	}
}

func TestStripePricesWithoutStorefront(t *testing.T) {
	var rawQuery atomic.Value
	api := newTestPaymentApi(t, func(w http.ResponseWriter, r *http.Request) {
		rawQuery.Store(r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		// the server's exact payload when prices are not configured
		fmt.Fprint(w, `{"tier":"","currency":"","yearly_price_id":"","monthly_price_id":"","yearly_usd":0,"monthly_usd":0,"publishable_key":"","offer_eligible":false,"error":{"message":"Prices are not configured."}}`)
	})

	callback, c := connect.NewBlockingApiCallback[*StripePricesResult](context.Background())
	// whitespace is an unknown storefront, not a query value
	api.StripePrices("  ", callback)
	r := awaitApiResult(t, c, "StripePrices never returned")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if got := rawQuery.Load(); got != "" {
		t.Errorf("query = %q, want none", got)
	}
	if r.Result.Error == nil || r.Result.Error.Message != "Prices are not configured." {
		t.Errorf("error = %+v", r.Result.Error)
	}
	if r.Result.OfferEligible || r.Result.YearlyPriceId != "" {
		t.Errorf("error result carried prices: %+v", r.Result)
	}
}

func TestVerifyPlayPurchase(t *testing.T) {
	var path, body atomic.Value
	api := newTestPaymentApi(t, func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.URL.Path)
		b, _ := io.ReadAll(r.Body)
		body.Store(string(b))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"credited","expiry_time":"2026-09-07T00:00:00Z"}`)
	})

	callback, c := connect.NewBlockingApiCallback[*VerifyStorePurchaseResult](context.Background())
	api.VerifyPlayPurchase(
		&VerifyPlayPurchaseArgs{
			ProductId:     "supporter",
			PurchaseToken: "play-token-1",
		},
		callback,
	)
	r := awaitApiResult(t, c, "VerifyPlayPurchase never returned")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if got := path.Load(); got != "/subscription/verify-play-purchase" {
		t.Errorf("path = %v", got)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(body.Load().(string)), &sent); err != nil {
		t.Fatal(err)
	}
	if sent["product_id"] != "supporter" || sent["purchase_token"] != "play-token-1" {
		t.Errorf("sent args = %v", sent)
	}
	// the optional package name must not be serialized when unset
	if _, exists := sent["package_name"]; exists {
		t.Errorf("empty package_name was serialized: %v", sent)
	}
	if r.Result.Status != PurchaseReportStatusCredited {
		t.Errorf("status = %q", r.Result.Status)
	}
	if !IsPurchaseReportTerminal(r.Result.Status) {
		t.Error("credited must be terminal")
	}
	if r.Result.ExpiryTime == nil || r.Result.ExpiryTimeMillis() == 0 {
		t.Errorf("expiry did not decode: %+v", r.Result)
	}
}

func TestVerifyAppleTransaction(t *testing.T) {
	var path, body atomic.Value
	api := newTestPaymentApi(t, func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.URL.Path)
		b, _ := io.ReadAll(r.Body)
		body.Store(string(b))
		w.Header().Set("Content-Type", "application/json")
		// no expiry on a non-credit answer
		fmt.Fprint(w, `{"status":"pending"}`)
	})

	callback, c := connect.NewBlockingApiCallback[*VerifyStorePurchaseResult](context.Background())
	api.VerifyAppleTransaction(
		&VerifyAppleTransactionArgs{SignedTransaction: "eyJhbGciOiJFUzI1NiJ9.x.y"},
		callback,
	)
	r := awaitApiResult(t, c, "VerifyAppleTransaction never returned")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if got := path.Load(); got != "/subscription/verify-apple-transaction" {
		t.Errorf("path = %v", got)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(body.Load().(string)), &sent); err != nil {
		t.Fatal(err)
	}
	if sent["signed_transaction"] != "eyJhbGciOiJFUzI1NiJ9.x.y" {
		t.Errorf("sent args = %v", sent)
	}
	if r.Result.Status != PurchaseReportStatusPending {
		t.Errorf("status = %q", r.Result.Status)
	}
	// pending is NOT terminal: the client keeps the proof and retries
	if IsPurchaseReportTerminal(r.Result.Status) {
		t.Error("pending must not be terminal")
	}
	if r.Result.ExpiryTime != nil || r.Result.ExpiryTimeMillis() != 0 {
		t.Errorf("unexpected expiry: %+v", r.Result)
	}
}

func TestPurchaseReportTerminalStatuses(t *testing.T) {
	terminal := map[string]bool{
		PurchaseReportStatusCredited:        true,
		PurchaseReportStatusAlreadyCredited: true,
		PurchaseReportStatusInvalid:         true,
		PurchaseReportStatusWrongNetwork:    true,
		PurchaseReportStatusPending:         false,
		// a transport failure has no status
		"": false,
	}
	for status, want := range terminal {
		if got := IsPurchaseReportTerminal(status); got != want {
			t.Errorf("IsPurchaseReportTerminal(%q) = %t, want %t", status, got, want)
		}
	}
}

func TestPurchaseReportBackoffMillis(t *testing.T) {
	expected := map[int32]int64{
		-1:      1_000, // clamp
		0:       1_000,
		1:       5_000,
		2:       30_000,
		3:       300_000,
		4:       300_000, // capped
		100_000: 300_000,
	}
	for attempt, want := range expected {
		if got := PurchaseReportBackoffMillis(attempt); got != want {
			t.Errorf("PurchaseReportBackoffMillis(%d) = %d, want %d", attempt, got, want)
		}
	}
}

// The guest-upgrade routes were deleted on the server (commit 340d828a); the
// deprecated SDK methods must fail immediately and clearly WITHOUT any HTTP
// round-trip (finding S6).
func TestUpgradeGuestFailsImmediatelyWithoutHttp(t *testing.T) {
	var requests atomic.Int64
	api := newTestPaymentApi(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.NotFound(w, r)
	})

	callback, c := connect.NewBlockingApiCallback[*UpgradeGuestResult](context.Background())
	api.UpgradeGuest(&UpgradeGuestArgs{NetworkName: "testnet"}, callback)
	r := awaitApiResult(t, c, "UpgradeGuest never returned")
	if r.Error == nil {
		t.Fatal("UpgradeGuest did not fail")
	}
	if !strings.Contains(r.Error.Error(), "no longer supported") {
		t.Errorf("error = %q, want a clear deprecation message", r.Error)
	}
	if r.Result != nil {
		t.Errorf("result = %+v, want nil", r.Result)
	}

	existingCallback, existingC := connect.NewBlockingApiCallback[*UpgradeGuestExistingResult](context.Background())
	api.UpgradeGuestExisting(&UpgradeGuestExistingArgs{UserAuth: "a@b.c"}, existingCallback)
	existing := awaitApiResult(t, existingC, "UpgradeGuestExisting never returned")
	if existing.Error == nil {
		t.Fatal("UpgradeGuestExisting did not fail")
	}
	if existing.Result != nil {
		t.Errorf("result = %+v, want nil", existing.Result)
	}

	if got := requests.Load(); got != 0 {
		t.Errorf("deprecated methods made %d HTTP requests, want 0", got)
	}
}

// TestSdkPaymentEndpointsMatchServerRoutes cross-checks the payment endpoints
// this SDK calls against the server's route table, so a removed server route
// fails loudly here instead of 404ing in production (the failure mode of
// finding S6). It reads the sibling server repo read-only and skips when the
// repo is not checked out (e.g. a standalone sdk CI).
func TestSdkPaymentEndpointsMatchServerRoutes(t *testing.T) {
	serverApiPath := filepath.Join("..", "server", "api", "api.go")
	routesSource, err := os.ReadFile(serverApiPath)
	if err != nil {
		t.Skipf("server repo not available (%v); skipping route conformance", err)
	}
	routeRe := regexp.MustCompile(`NewRoute\("(?:GET|POST|PUT|DELETE)",\s*"([^"]+)"`)
	routes := map[string]bool{}
	for _, m := range routeRe.FindAllStringSubmatch(string(routesSource), -1) {
		routes[m[1]] = true
	}
	if len(routes) == 0 {
		t.Fatal("no routes parsed from server api.go; the conformance regex is stale")
	}

	// every payment/upgrade endpoint the SDK calls must exist server-side
	required := []string{
		"/subscription/balance",
		"/subscription/check-balance-code",
		"/subscription/redeem-balance-code",
		"/subscription/create-payment-id",
		"/subscription/verify-play-purchase",
		"/subscription/verify-apple-transaction",
		"/stripe/payment-intent",
		"/stripe/customer-portal",
		"/stripe/create-checkout-session",
		"/account/balance-codes",
		"/auth/refresh",
	}
	for _, route := range required {
		if !routes[route] {
			t.Errorf("SDK calls %s but the server no longer routes it", route)
		}
	}

	// the deprecated guest-upgrade methods must STAY deprecated while the
	// routes are gone; if the server restores them, this fails to prompt
	// un-deprecating the SDK methods
	for _, route := range []string{"/auth/upgrade-guest", "/auth/upgrade-guest-existing"} {
		if routes[route] {
			t.Errorf("server restored %s; un-deprecate the SDK guest-upgrade method", route)
		}
	}
}
