package sdk

// The card payment report (stripe_purchase_report.go), support inbox 7247: a card
// payment is reported as soon as the checkout or the payment sheet finishes,
// and the report retries a pending answer until it is terminal, so the credit
// no longer waits only on Stripe's webhook. A refused trial card ends the
// report at once and reaches the app with its code and message.
// Deterministic: the server is a local test server and the waits are
// recorded, never slept.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
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
		defaultStripePurchaseReportSettings(),
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
	settings := defaultStripePurchaseReportSettings()
	result, err := reportStripePurchase(
		context.Background(),
		settings,
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
	if asked != settings.MaxAttempts {
		t.Fatalf("asked %d times, want %d", asked, settings.MaxAttempts)
	}

	result, err = reportStripePurchase(
		context.Background(),
		settings,
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
		defaultStripePurchaseReportSettings(),
		&VerifyStripeCheckoutArgs{SessionId: "cs_test_stopped"},
		verify,
		func(ctx context.Context, d time.Duration) bool { return false },
	)
	if asked != 1 || err != nil || result == nil || result.Status != PurchaseReportStatusPending {
		t.Fatalf("asked %d, result = %+v err = %v, want one report and its answer", asked, result, err)
	}

	for _, args := range []*VerifyStripeCheckoutArgs{nil, {}, {SessionId: "cs_a", SubscriptionId: "sub_a"}, {SessionId: "  "}} {
		asked = 0
		result, err := reportStripePurchase(context.Background(), defaultStripePurchaseReportSettings(), args, verify, func(ctx context.Context, d time.Duration) bool { return true })
		if result != nil || err == nil || asked != 0 {
			t.Fatalf("args %+v: result = %+v err = %v asked = %d, want a refusal that asks nothing", args, result, err, asked)
		}
	}
}

// The server's words for a yearly trial whose card failed the CVC check when
// it was attached (the trial card check, owner 2026-11-16).
const testIncorrectCvcMessage = "Your card's security code (CVC) didn't match. Check the card details and try again."

// The verify answer keeps a refusal: an answer with an error decodes its code
// and message. One without (every Play and App Store answer, and an older
// server's) leaves Error nil, and encodes again with no error, which is how
// the answer crosses to the desktop apps.
func TestVerifyStorePurchaseResultDecodesTheRefusal(t *testing.T) {
	var refused VerifyStorePurchaseResult
	if err := json.Unmarshal([]byte(`{"status":"invalid","error":{"code":"incorrect_cvc","message":"`+testIncorrectCvcMessage+`"}}`), &refused); err != nil {
		t.Fatal(err)
	}
	if refused.Status != PurchaseReportStatusInvalid || refused.Error == nil {
		t.Fatalf("refused = %+v, want invalid with the error", refused)
	}
	if refused.Error.Code != PurchaseErrorCodeIncorrectCvc || refused.Error.Message != testIncorrectCvcMessage {
		t.Fatalf("error = %+v, want incorrect_cvc and the server's message", refused.Error)
	}
	if refused.ExpiryTimeMillis() != 0 {
		t.Errorf("a refusal decoded an expiry: %+v", refused)
	}

	for _, answer := range []string{
		`{"status":"credited","expiry_time":"2026-12-12T00:00:00Z"}`,
		`{"status":"invalid"}`,
		`{"status":"pending","error":null}`,
	} {
		var result VerifyStorePurchaseResult
		if err := json.Unmarshal([]byte(answer), &result); err != nil {
			t.Fatal(err)
		}
		if result.Error != nil {
			t.Errorf("%s decoded an error: %+v", answer, result.Error)
		}
		encoded, err := json.Marshal(&result)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), `"error"`) {
			t.Errorf("%s encoded again as %s, want no error", answer, encoded)
		}
	}
}

// A refused trial card reaches the app through ReportStripePurchase: the server
// answers invalid with an error naming it, the report asks once, and the
// callback carries the code and the message.
func TestReportStripePurchaseCallsBackTheRefusal(t *testing.T) {
	var asked atomic.Int32
	api := newTestPaymentApi(t, func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"invalid","error":{"code":"incorrect_cvc","message":"`+testIncorrectCvcMessage+`"}}`)
	})

	callback, c := connect.NewBlockingApiCallback[*VerifyStorePurchaseResult](context.Background())
	api.ReportStripePurchase(&VerifyStripeCheckoutArgs{SubscriptionId: "sub_trial_refused"}, callback)
	// the callback comes after the loop ended; a retry would have waited out
	// the backoff first
	r := awaitApiResult(t, c, "ReportStripePurchase never called back")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if r.Result == nil || r.Result.Status != PurchaseReportStatusInvalid || r.Result.Error == nil {
		t.Fatalf("result = %+v, want invalid with the refusal", r.Result)
	}
	if r.Result.Error.Code != PurchaseErrorCodeIncorrectCvc || r.Result.Error.Message != testIncorrectCvcMessage {
		t.Fatalf("error = %+v, want incorrect_cvc and the server's message", r.Result.Error)
	}
	if n := asked.Load(); n != 1 {
		t.Fatalf("asked %d times, want one report", n)
	}
}

// A refusal ends the report whatever status it came with, also after a
// transport failure and a pending answer: a server that paired the error with
// a status this client does not count as terminal is not asked again for 21
// minutes.
func TestReportStripePurchaseStopsAtTheRefusal(t *testing.T) {
	refusal := &OnboardingError{Code: PurchaseErrorCodeIncorrectCvc, Message: testIncorrectCvcMessage}
	for _, status := range []string{PurchaseReportStatusInvalid, PurchaseReportStatusPending, "refused", ""} {
		answers := []func() (*VerifyStorePurchaseResult, error){
			func() (*VerifyStorePurchaseResult, error) { return nil, errors.New("synthetic transport failure") },
			func() (*VerifyStorePurchaseResult, error) {
				return &VerifyStorePurchaseResult{Status: PurchaseReportStatusPending}, nil
			},
			func() (*VerifyStorePurchaseResult, error) {
				return &VerifyStorePurchaseResult{Status: status, Error: refusal}, nil
			},
			// never asked: the refusal was the last word
			func() (*VerifyStorePurchaseResult, error) {
				return &VerifyStorePurchaseResult{Status: PurchaseReportStatusCredited}, nil
			},
		}
		asked := 0
		waits := []time.Duration{}
		result, err := reportStripePurchase(
			context.Background(),
			defaultStripePurchaseReportSettings(),
			&VerifyStripeCheckoutArgs{SessionId: "cs_test_refused"},
			func(ctx context.Context, args *VerifyStripeCheckoutArgs) (*VerifyStorePurchaseResult, error) {
				asked += 1
				return answers[asked-1]()
			},
			func(ctx context.Context, d time.Duration) bool {
				waits = append(waits, d)
				return true
			},
		)
		if err != nil || result == nil || result.Error != refusal || result.Status != status {
			t.Fatalf("status %q: result = %+v err = %v, want the refusal", status, result, err)
		}
		if asked != 3 || len(waits) != 2 {
			t.Fatalf("status %q: asked %d times after waits %v, want the refusal to end the third report", status, asked, waits)
		}
	}
}
