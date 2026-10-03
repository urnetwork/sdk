package sdk

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// UPGRADE.md §4.1: a confirmation that gives up says why (the server never
// reflected the purchase, or it could not be reached), a failed fetch stays
// readable, and the fetch carries the storefront and keeps the plan fields the
// desktop apps need. These tests drive step() by hand on an injected clock
// against a synchronous fetch stub: no polling loop, no sleeps, no network.

type stepTestClock struct {
	now time.Time
}

func (self *stepTestClock) Now() time.Time {
	return self.now
}

func (self *stepTestClock) advance(d time.Duration) {
	self.now = self.now.Add(d)
}

func newStepTestSubscriptionBalanceVc(
	t *testing.T,
	stub *balanceFetchStub,
) (*SubscriptionBalanceViewController, *stepTestClock) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	clientStrategy := connect.NewClientStrategyWithDefaults(ctx)
	api := NewApi(ctx, clientStrategy, "http://127.0.0.1:0")
	api.SetByJwt(testSubscriptionJwt(t, false, false))
	clock := &stepTestClock{now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	vc := newSubscriptionBalanceViewControllerWithoutRun(ctx, api)
	vc.nowFunc = clock.Now
	vc.fetchFunc = stub.fetch
	vc.SetBackgroundPollIntervalMillis(30_000)
	vc.SetConfirmationPollIntervalMillis(5_000)
	vc.SetConfirmationBudgetMillis(120_000)
	t.Cleanup(func() {
		vc.Close()
		api.Close()
		cancel()
		_ = api.CloseAndWait(context.Background())
		clientStrategy.Close()
	})
	return vc, clock
}

// runConfirmation starts a confirmation, polls every confirmation interval
// with the stub's current answer, and steps past the budget.
func runConfirmation(vc *SubscriptionBalanceViewController, clock *stepTestClock, stub *balanceFetchStub, failLastFetch bool) {
	vc.StartPurchaseConfirmation()
	vc.step()
	for i := 0; i < 23; i += 1 {
		clock.advance(5 * time.Second)
		if failLastFetch && i == 22 {
			stub.set(nil, errors.New("synthetic network down"))
		}
		vc.step()
	}
	clock.advance(10 * time.Second)
	vc.step()
}

func TestConfirmationGiveUpSaysWhy(t *testing.T) {
	t.Run("not_reflected", func(t *testing.T) {
		// the server answers every poll, still free: the webhook never landed
		stub := newBalanceFetchStub(testBalanceResult(0, 0, 0, ""))
		vc, clock := newStepTestSubscriptionBalanceVc(t, stub)
		vc.Start()
		vc.step()

		runConfirmation(vc, clock, stub, false)
		connect.AssertEqual(t, vc.GetPurchaseConfirmationState(), PurchaseConfirmationStateConfirmationGaveUp)
		if reason := vc.GetPurchaseConfirmationGiveUpReason(); reason != PurchaseConfirmationGiveUpReasonNotReflected {
			t.Fatalf("give-up reason = %q, want %q: the ui cannot tell an unreflected payment from an unreachable server", reason, PurchaseConfirmationGiveUpReasonNotReflected)
		}
		connect.AssertEqual(t, vc.GetLastFetchError(), "")

		vc.ClearPurchaseConfirmation()
		connect.AssertEqual(t, vc.GetPurchaseConfirmationGiveUpReason(), "")
	})

	t.Run("unreachable", func(t *testing.T) {
		// every poll during the confirmation fails
		stub := newBalanceFetchStub(testBalanceResult(0, 0, 0, ""))
		vc, clock := newStepTestSubscriptionBalanceVc(t, stub)
		vc.Start()
		vc.step()
		stub.set(nil, errors.New("synthetic network down"))

		runConfirmation(vc, clock, stub, false)
		connect.AssertEqual(t, vc.GetPurchaseConfirmationState(), PurchaseConfirmationStateConfirmationGaveUp)
		if reason := vc.GetPurchaseConfirmationGiveUpReason(); reason != PurchaseConfirmationGiveUpReasonUnreachable {
			t.Fatalf("give-up reason = %q, want %q: the ui cannot tell an unreflected payment from an unreachable server", reason, PurchaseConfirmationGiveUpReasonUnreachable)
		}
		connect.AssertEqual(t, vc.GetLastFetchError(), "synthetic network down")
		// the snapshot from before the outage stays
		connect.AssertEqual(t, vc.GetIsLoaded(), true)
	})

	t.Run("last_fetch_failed", func(t *testing.T) {
		// the server answered, then went away before the budget ran out: the
		// purchase may have landed since the last answer
		stub := newBalanceFetchStub(testBalanceResult(0, 0, 0, ""))
		vc, clock := newStepTestSubscriptionBalanceVc(t, stub)
		vc.Start()
		vc.step()

		runConfirmation(vc, clock, stub, true)
		connect.AssertEqual(t, vc.GetPurchaseConfirmationState(), PurchaseConfirmationStateConfirmationGaveUp)
		connect.AssertEqual(t, vc.GetPurchaseConfirmationGiveUpReason(), PurchaseConfirmationGiveUpReasonUnreachable)

		// a successful fetch clears the error, and a new confirmation starts
		// without a reason
		stub.set(testBalanceResult(0, 0, 0, ""), nil)
		vc.Refresh()
		vc.step()
		connect.AssertEqual(t, vc.GetLastFetchError(), "")
		vc.StartPurchaseConfirmation()
		connect.AssertEqual(t, vc.GetPurchaseConfirmationGiveUpReason(), "")
	})
}

func TestSubscriptionBalanceFetchCarriesStorefrontAndPlanFields(t *testing.T) {
	result := testBalanceResult(100, 30, 20, "")
	result.PriceTier = &PriceTier{Name: "regional", YearlyUsd: 20, MonthlyUsd: 2, Currency: "USD"}
	result.OnboardingOffer = &OnboardingOffer{}
	stub := newBalanceFetchStub(result)
	vc, _ := newStepTestSubscriptionBalanceVc(t, stub)

	vc.Start()
	vc.step()
	// the desktop has no store: the server resolves the tier
	connect.AssertEqual(t, stub.storefrontCountries, []string{""})

	planResult := vc.GetSubscriptionBalanceResult()
	if planResult == nil || planResult.PriceTier == nil || planResult.OnboardingOffer == nil {
		t.Fatalf("the controller dropped the plan fields the desktop apps need: %+v", planResult)
	}
	connect.AssertEqual(t, planResult.PriceTier.Name, "regional")

	// a store's storefront is sent with the next fetch, which runs at once
	vc.SetStorefrontCountry(" RU ")
	vc.step()
	connect.AssertEqual(t, stub.storefrontCountries, []string{"", "RU"})
	// setting the same storefront again fetches nothing new
	vc.SetStorefrontCountry("RU")
	vc.step()
	connect.AssertEqual(t, len(stub.storefrontCountries), 2)

	vc.Stop()
	if vc.GetSubscriptionBalanceResult() != nil {
		t.Fatal("Stop kept the last plan result")
	}
}
