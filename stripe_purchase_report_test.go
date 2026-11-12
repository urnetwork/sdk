package sdk

// ReportStripePurchase (stripe_purchase_report.go), support inbox 7247: a card
// payment is reported as soon as the checkout or the payment sheet finishes,
// and the report retries a pending answer until it is terminal, so the credit
// no longer waits only on Stripe's webhook. Deterministic: the server is a
// local test server and the waits are recorded, never slept.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The report posts the id the purchase started with to the verify route, and
// a terminal answer ends it at once.
func TestReportStripePurchasePostsTheSession(t *testing.T) {
	var lock sync.Mutex
	paths := []string{}
	bodies := []map[string]any{}
	api := newTestPaymentApi(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sent := map[string]any{}
		json.Unmarshal(b, &sent)
		lock.Lock()
		paths = append(paths, r.URL.Path)
		bodies = append(bodies, sent)
		lock.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"credited","expiry_time":"2026-12-12T00:00:00Z"}`)
	})

	callback, c := connect.NewBlockingApiCallback[*VerifyStorePurchaseResult](context.Background())
	api.ReportStripePurchase(&VerifyStripeCheckoutArgs{SessionId: " cs_test_report "}, callback)
	r := awaitApiResult(t, c, "ReportStripePurchase never called back")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if r.Result.Status != PurchaseReportStatusCredited || r.Result.ExpiryTimeMillis() == 0 {
		t.Fatalf("result = %+v", r.Result)
	}
	lock.Lock()
	defer lock.Unlock()
	if len(paths) != 1 || paths[0] != "/subscription/verify-stripe-checkout" {
		t.Fatalf("paths = %v, want one report to /subscription/verify-stripe-checkout", paths)
	}
	if bodies[0]["session_id"] != "cs_test_report" {
		t.Errorf("sent = %v, want the trimmed session id", bodies[0])
	}
	if _, ok := bodies[0]["subscription_id"]; ok {
		t.Errorf("sent an empty subscription_id: %v", bodies[0])
	}
}

// A pending answer and a transport failure are retried after the purchase
// report backoff, until the answer is terminal.
func TestReportStripePurchaseRetriesUntilTerminal(t *testing.T) {
	answers := []func() (*VerifyStorePurchaseResult, error){
		func() (*VerifyStorePurchaseResult, error) { return nil, errors.New("synthetic transport failure") },
		func() (*VerifyStorePurchaseResult, error) {
			return &VerifyStorePurchaseResult{Status: PurchaseReportStatusPending}, nil
		},
		func() (*VerifyStorePurchaseResult, error) {
			return &VerifyStorePurchaseResult{Status: PurchaseReportStatusCredited}, nil
		},
	}
	asked := []*VerifyStripeCheckoutArgs{}
	waits := []time.Duration{}
	result, err := reportStripePurchase(
		context.Background(),
		&VerifyStripeCheckoutArgs{SubscriptionId: "sub_sheet"},
		func(ctx context.Context, args *VerifyStripeCheckoutArgs) (*VerifyStorePurchaseResult, error) {
			asked = append(asked, args)
			return answers[len(asked)-1]()
		},
		func(ctx context.Context, d time.Duration) bool {
			waits = append(waits, d)
			return true
		},
	)
	if err != nil || result == nil || result.Status != PurchaseReportStatusCredited {
		t.Fatalf("result = %+v err = %v, want credited", result, err)
	}
	if len(asked) != 3 || asked[2].SubscriptionId != "sub_sheet" || asked[2].SessionId != "" {
		t.Fatalf("asked = %+v, want three reports of the subscription", asked)
	}
	if len(waits) != 2 || waits[0] != time.Second || waits[1] != 5*time.Second {
		t.Fatalf("waits = %v, want the purchase report backoff (1s, 5s)", waits)
	}
}

// A report still pending after every attempt answers the last pending answer;
// one never answered answers its last error.
func TestReportStripePurchaseGivesUpAfterItsAttempts(t *testing.T) {
	asked := 0
	result, err := reportStripePurchase(
		context.Background(),
		&VerifyStripeCheckoutArgs{SessionId: "cs_test_pending"},
		func(ctx context.Context, args *VerifyStripeCheckoutArgs) (*VerifyStorePurchaseResult, error) {
			asked += 1
			return &VerifyStorePurchaseResult{Status: PurchaseReportStatusPending}, nil
		},
		func(ctx context.Context, d time.Duration) bool { return true },
	)
	if err != nil || result == nil || result.Status != PurchaseReportStatusPending {
		t.Fatalf("result = %+v err = %v, want the last pending answer", result, err)
	}
	if asked != StripePurchaseReportMaxAttempts {
		t.Fatalf("asked %d times, want %d", asked, StripePurchaseReportMaxAttempts)
	}

	result, err = reportStripePurchase(
		context.Background(),
		&VerifyStripeCheckoutArgs{SessionId: "cs_test_down"},
		func(ctx context.Context, args *VerifyStripeCheckoutArgs) (*VerifyStorePurchaseResult, error) {
			return nil, errors.New("synthetic outage")
		},
		func(ctx context.Context, d time.Duration) bool { return true },
	)
	if result != nil || err == nil || err.Error() != "synthetic outage" {
		t.Fatalf("result = %+v err = %v, want the last error", result, err)
	}
}

// The Api's end stops the waits; an args naming neither id, or both, asks
// nothing.
func TestReportStripePurchaseStopsAndRefusesBadArgs(t *testing.T) {
	asked := 0
	verify := func(ctx context.Context, args *VerifyStripeCheckoutArgs) (*VerifyStorePurchaseResult, error) {
		asked += 1
		return &VerifyStorePurchaseResult{Status: PurchaseReportStatusPending}, nil
	}
	result, err := reportStripePurchase(
		context.Background(),
		&VerifyStripeCheckoutArgs{SessionId: "cs_test_stopped"},
		verify,
		func(ctx context.Context, d time.Duration) bool { return false },
	)
	if asked != 1 || err != nil || result == nil || result.Status != PurchaseReportStatusPending {
		t.Fatalf("asked %d, result = %+v err = %v, want one report and its answer", asked, result, err)
	}

	for _, args := range []*VerifyStripeCheckoutArgs{nil, {}, {SessionId: "cs_a", SubscriptionId: "sub_a"}, {SessionId: "  "}} {
		asked = 0
		result, err := reportStripePurchase(context.Background(), args, verify, func(ctx context.Context, d time.Duration) bool { return true })
		if result != nil || err == nil || asked != 0 {
			t.Fatalf("args %+v: result = %+v err = %v asked = %d, want a refusal that asks nothing", args, result, err, asked)
		}
	}
}
