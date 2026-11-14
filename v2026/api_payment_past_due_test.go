package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// A renewal whose payment did not go through reaches the apps as the
// balance's payment_past_due, which they show as the past-due notice with an
// "Update payment method" action. Before the field existed the sdk dropped it
// while decoding, so no app could show the notice.
func TestSubscriptionBalanceDecodesPaymentPastDue(t *testing.T) {
	api := newTestPaymentApi(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"start_balance_byte_count": 1000,
			"balance_byte_count": 600,
			"open_transfer_byte_count": 0,
			"pending_payout_usd_nano_cents": 0,
			"update_time": "2026-10-26T12:00:00Z",
			"payment_past_due": {
				"store": "stripe",
				"fail_time": "2026-10-26T15:56:00Z",
				"update_payment_url": "https://ur.io/app/subscription"
			}
		}`)
	})
	callback, c := connect.NewBlockingApiCallback[*SubscriptionBalanceResult](context.Background())
	api.SubscriptionBalance(callback)
	r := awaitApiResult(t, c, "SubscriptionBalance never returned")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	pastDue := r.Result.PaymentPastDue
	if pastDue == nil {
		t.Fatal("payment_past_due did not decode")
	}
	if pastDue.Store != "stripe" || pastDue.FailTime != "2026-10-26T15:56:00Z" || pastDue.UpdatePaymentUrl != "https://ur.io/app/subscription" {
		t.Errorf("payment past due = %+v", pastDue)
	}
}

// No failing renewal, or an older server: no past-due notice.
func TestSubscriptionBalanceWithoutPaymentPastDue(t *testing.T) {
	api := newTestPaymentApi(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"start_balance_byte_count": 1, "balance_byte_count": 1, "open_transfer_byte_count": 0, "pending_payout_usd_nano_cents": 0, "update_time": "2026-10-26T12:00:00Z"}`)
	})
	callback, c := connect.NewBlockingApiCallback[*SubscriptionBalanceResult](context.Background())
	api.SubscriptionBalance(callback)
	r := awaitApiResult(t, c, "SubscriptionBalance never returned")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if r.Result.PaymentPastDue != nil {
		t.Errorf("payment past due = %+v, want nil", r.Result.PaymentPastDue)
	}
}

// The portal's payment method flow is sent as the server's `flow`, and the
// portal home sends no flow at all, so an older server reads the same empty
// args as before.
func TestStripeCustomerPortalSendsTheFlow(t *testing.T) {
	var body atomic.Value
	api := newTestPaymentApi(t, func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		body.Store(string(bodyBytes))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"url": "https://billing.stripe.example/p/session/synthetic"}`)
	})

	for _, c := range []struct {
		args *StripeCreateCustomerPortalArgs
		flow any
	}{
		{&StripeCreateCustomerPortalArgs{Flow: StripeCustomerPortalFlowPaymentMethodUpdate}, "payment_method_update"},
		{&StripeCreateCustomerPortalArgs{}, nil},
	} {
		callback, resultC := connect.NewBlockingApiCallback[*StripeCreateCustomerPortalResult](context.Background())
		api.StripeCreateCustomerPortal(c.args, callback)
		r := awaitApiResult(t, resultC, "StripeCreateCustomerPortal never returned")
		if r.Error != nil || r.Result.Url == "" {
			t.Fatalf("portal: %+v", r)
		}
		var sent map[string]any
		if err := json.Unmarshal([]byte(body.Load().(string)), &sent); err != nil {
			t.Fatal(err)
		}
		if sent["flow"] != c.flow {
			t.Errorf("flow sent = %v, want %v (body %s)", sent["flow"], c.flow, body.Load())
		}
	}
}
