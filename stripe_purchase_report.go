package sdk

// Reporting a card payment (support inbox 7247): the Stripe half of the
// purchase reporting purchase_report.go describes for Play and the App Store.
//
// A card payment credited Pro only when Stripe's webhook arrived, or within
// the hour when the server's payment reconcile ran: no app told the server it
// had paid, so a late webhook left a paying customer on Free. Now, as soon as
// a checkout or the payment sheet finishes, the app hands ReportStripePurchase
// the id the server answered when the purchase started:
//
//   - the checkout session id, StripeCreateCheckoutSessionResult.SessionId (also
//     ParseCheckoutRedirect's SessionId, and the session_id of ur.io's
//     /checkout/success return url);
//   - the payment sheet's subscription id, StripePaymentSheetResult.SubscriptionId.
//
// The server (POST /subscription/verify-stripe-checkout) reads it back from
// Stripe, checks it is for the signed-in network, and credits it through the
// same code as the webhooks, once. ReportStripePurchase retries a pending
// answer or a transport failure with PurchaseReportBackoffMillis, up to
// StripePurchaseReportMaxAttempts, and calls back once with the last answer.
// The app keeps its own balance confirmation (the polling it already runs
// after a purchase); a credited answer means the next balance read shows Pro.
// Unlike a store purchase, nothing has to be finalized after the report: the
// checkout is already complete at Stripe.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/connect"
)

// How many times ReportStripePurchase asks before it gives up on a pending
// answer: with PurchaseReportBackoffMillis the waits are 1 s, 5 s, 30 s, then
// 5 min, so the last attempt comes about 21 minutes after the first. The
// webhooks and the hourly reconcile credit what is still pending then. The
// server's per-account verify budget (30 an hour, shared with Play and the App
// Store) is never at risk.
const StripePurchaseReportMaxAttempts = 8

type VerifyStripeCheckoutArgs struct {
	// the checkout session id (cs_...): StripeCreateCheckoutSessionResult.SessionId
	SessionId string `json:"session_id,omitempty"`
	// the payment sheet's subscription id (sub_...):
	// StripePaymentSheetResult.SubscriptionId
	SubscriptionId string `json:"subscription_id,omitempty"`
}

type VerifyStripeCheckoutCallback connect.ApiCallback[*VerifyStorePurchaseResult]

// VerifyStripeCheckout reports a finished card payment once (POST
// /subscription/verify-stripe-checkout). Apps call ReportStripePurchase, which
// retries it.
func (self *Api) VerifyStripeCheckout(args *VerifyStripeCheckoutArgs, callback VerifyStripeCheckoutCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/subscription/verify-stripe-checkout", self.apiUrl),
			args,
			self.GetByJwt(),
			&VerifyStorePurchaseResult{},
			callback,
		)
	})
}

// VerifyStripeCheckoutSyncWithContext is one report bounded by ctx.
//
//gomobile:noexport
func (self *Api) VerifyStripeCheckoutSyncWithContext(ctx context.Context, args *VerifyStripeCheckoutArgs) (*VerifyStorePurchaseResult, error) {
	return connect.HttpPostWithRawFunction(
		ctx,
		self.getHttpPostRaw(),
		fmt.Sprintf("%s/subscription/verify-stripe-checkout", self.apiUrl),
		args,
		self.GetByJwt(),
		&VerifyStorePurchaseResult{},
		connect.NewNoopApiCallback[*VerifyStorePurchaseResult](),
	)
}

// ReportStripePurchase reports a finished card payment and retries until the
// server answers a terminal status (IsPurchaseReportTerminal) or
// StripePurchaseReportMaxAttempts are spent, then calls back once with the
// last answer, or the last error when no attempt was answered. An args that
// names neither id, or both, calls back at once with an error and asks
// nothing. Returns at once; the attempts run in the background, bound to the
// Api's lifetime.
func (self *Api) ReportStripePurchase(args *VerifyStripeCheckoutArgs, callback VerifyStripeCheckoutCallback) {
	go connect.HandleError(func() {
		result, err := reportStripePurchase(
			self.ctx,
			args,
			self.VerifyStripeCheckoutSyncWithContext,
			stripePurchaseReportSleep,
		)
		if callback != nil {
			callback.Result(result, err)
		}
	})
}

// stripePurchaseReportSleep waits d unless ctx ends first. Replaceable only by
// tests.
var stripePurchaseReportSleep = func(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// reportStripePurchase is the report loop: verify, and on a pending answer or a
// transport failure wait PurchaseReportBackoffMillis(attempt) and ask again,
// until an answer is terminal, the attempts are spent, or ctx ends.
func reportStripePurchase(
	ctx context.Context,
	args *VerifyStripeCheckoutArgs,
	verify func(ctx context.Context, args *VerifyStripeCheckoutArgs) (*VerifyStorePurchaseResult, error),
	sleep func(ctx context.Context, d time.Duration) bool,
) (*VerifyStorePurchaseResult, error) {
	if args == nil {
		return nil, fmt.Errorf("no card payment to report")
	}
	sessionId := strings.TrimSpace(args.SessionId)
	subscriptionId := strings.TrimSpace(args.SubscriptionId)
	if (sessionId == "") == (subscriptionId == "") {
		return nil, fmt.Errorf("a card payment is reported by its checkout session id or its subscription id, one of them")
	}
	trimmed := &VerifyStripeCheckoutArgs{SessionId: sessionId, SubscriptionId: subscriptionId}

	var lastResult *VerifyStorePurchaseResult
	var lastErr error
	for attempt := 0; attempt < StripePurchaseReportMaxAttempts; attempt += 1 {
		if 0 < attempt {
			if !sleep(ctx, time.Duration(PurchaseReportBackoffMillis(int32(attempt-1)))*time.Millisecond) {
				break
			}
		}
		result, err := verify(ctx, trimmed)
		if err == nil && result != nil {
			lastResult = result
			lastErr = nil
			if IsPurchaseReportTerminal(result.Status) {
				return result, nil
			}
		} else if lastResult == nil {
			lastErr = err
			if lastErr == nil {
				lastErr = fmt.Errorf("no answer")
			}
		}
	}
	if lastResult != nil {
		return lastResult, nil
	}
	return nil, lastErr
}
