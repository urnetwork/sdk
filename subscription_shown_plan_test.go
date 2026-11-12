package sdk

import (
	"errors"
	"testing"

	"github.com/urnetwork/connect"
)

// The plan an app shows when the balance read fails (support inbox 4074). In
// December 2025 the balance read (/subscription/balance) failed under database
// load, and the apps of the time showed the failed read as Free: nine networks
// that had paid wrote in, again and again, while their paid windows ran on
// Main. A balance read that failed or has not come back yet now shows Pro by
// the jwt's claim, or unknown, which the apps show as a loading placeholder or
// a retry: never Free, and never an upgrade prompt. Only the server says Free.

// The plan the apps showed before this change: Pro from the decision, and Free
// whenever it was not Pro.
func shownPlanBefore(isPro bool) string {
	if isPro {
		return ShownPlanPro
	}
	return ShownPlanFree
}

// A network that just paid, whose stored claim still says Free, while its
// balance reads fail: unknown, never Free, until the server answers.
func TestSubscriptionProTrackerAFailedReadIsNeverFree(t *testing.T) {
	// a network that just paid: the stored jwt was minted before the
	// purchase, so its claim says Free
	tracker := NewSubscriptionProTracker()
	tracker.SetJwt(testNetworkJwt(t, testTrackerNetworkA, false, 1))

	// the first read is on its way
	connect.AssertEqual(t, tracker.GetShownPlan(), ShownPlanUnknown)
	connect.AssertEqual(t, tracker.GetLastReadFailed(), false)

	// every read fails, as on 2025-12-04
	connect.AssertEqual(t, tracker.BalanceFailed(), true)
	for i := 0; i < 5; i += 1 {
		connect.AssertEqual(t, tracker.BalanceFailed(), false)
	}
	// control: the rule before this change showed the paying network Free
	connect.AssertEqual(t, shownPlanBefore(tracker.GetIsPro()), ShownPlanFree)
	if plan := tracker.GetShownPlan(); plan != ShownPlanUnknown {
		t.Fatalf("plan %q after failed reads, want %q: a failed read shows a paying network Free", plan, ShownPlanUnknown)
	}
	connect.AssertEqual(t, tracker.GetLastReadFailed(), true)

	// the read comes back: the server's answer, and the one jwt refresh the
	// failures did not use up
	connect.AssertEqual(t, tracker.BalanceLoaded(true), true)
	connect.AssertEqual(t, tracker.GetShownPlan(), ShownPlanPro)
	connect.AssertEqual(t, tracker.GetLastReadFailed(), false)

	// a read that fails after the server answered keeps the answer
	connect.AssertEqual(t, tracker.BalanceFailed(), true)
	connect.AssertEqual(t, tracker.GetShownPlan(), ShownPlanPro)
	connect.AssertEqual(t, tracker.GetIsPro(), true)
	connect.AssertEqual(t, tracker.BalanceLoaded(true), false)
}

// Free comes from the server's answer alone; a Pro claim stays Pro until the
// server answers, also through a failed read.
func TestSubscriptionProTrackerOnlyTheServerSaysFree(t *testing.T) {
	// no jwt yet: unknown, also after a failed read
	tracker := NewSubscriptionProTracker()
	connect.AssertEqual(t, tracker.GetShownPlan(), ShownPlanUnknown)
	tracker.BalanceFailed()
	connect.AssertEqual(t, tracker.GetShownPlan(), ShownPlanUnknown)

	// a Free claim is unknown until the server answers
	tracker = NewSubscriptionProTracker()
	tracker.SetJwt(testNetworkJwt(t, testTrackerNetworkA, false, 1))
	connect.AssertEqual(t, tracker.GetShownPlan(), ShownPlanUnknown)
	connect.AssertEqual(t, tracker.GetIsPro(), false)
	tracker.BalanceLoaded(false)
	connect.AssertEqual(t, tracker.GetShownPlan(), ShownPlanFree)
	// the server's Free stands through a failed read, as Pro does
	tracker.BalanceFailed()
	connect.AssertEqual(t, tracker.GetShownPlan(), ShownPlanFree)
	// and a jwt of the same network that arrives later does not move it
	connect.AssertEqual(t, tracker.SetJwt(testNetworkJwt(t, testTrackerNetworkA, true, 2)), false)
	connect.AssertEqual(t, tracker.GetShownPlan(), ShownPlanFree)

	// a Pro claim is Pro until the server answers, also through a failed
	// read: it was the server's answer when the jwt was issued
	tracker = NewSubscriptionProTracker()
	connect.AssertEqual(t, tracker.SetJwt(testNetworkJwt(t, testTrackerNetworkA, true, 1)), true)
	connect.AssertEqual(t, tracker.GetShownPlan(), ShownPlanPro)
	tracker.BalanceFailed()
	connect.AssertEqual(t, tracker.GetShownPlan(), ShownPlanPro)
	connect.AssertEqual(t, tracker.GetIsPro(), true)
	// a lapse is Free once the server says so
	tracker.BalanceLoaded(false)
	connect.AssertEqual(t, tracker.GetShownPlan(), ShownPlanFree)
	connect.AssertEqual(t, tracker.GetIsPro(), false)

	// a renewed jwt whose claim drops to Free before any answer: unknown
	tracker = NewSubscriptionProTracker()
	tracker.SetJwt(testNetworkJwt(t, testTrackerNetworkA, true, 1))
	connect.AssertEqual(t, tracker.SetJwt(testNetworkJwt(t, testTrackerNetworkA, false, 2)), true)
	connect.AssertEqual(t, tracker.GetShownPlan(), ShownPlanUnknown)
}

// A failed read belongs to its session: another network's jwt and a sign out
// forget it.
func TestSubscriptionProTrackerAnotherSessionForgetsAFailedRead(t *testing.T) {
	tracker := NewSubscriptionProTracker()
	tracker.SetJwt(testNetworkJwt(t, testTrackerNetworkA, false, 1))
	tracker.BalanceLoaded(false)
	tracker.BalanceFailed()
	connect.AssertEqual(t, tracker.GetLastReadFailed(), true)

	// another network's jwt starts over: neither the server's Free nor the
	// failed read were its own; SetJwt reports the plan's change even though
	// Pro stays false
	connect.AssertEqual(t, tracker.SetJwt(testNetworkJwt(t, testTrackerNetworkB, false, 1)), true)
	connect.AssertEqual(t, tracker.GetShownPlan(), ShownPlanUnknown)
	connect.AssertEqual(t, tracker.GetLastReadFailed(), false)

	// a sign out forgets a failed read
	tracker.BalanceFailed()
	tracker.Reset()
	connect.AssertEqual(t, tracker.GetLastReadFailed(), false)
	connect.AssertEqual(t, tracker.GetShownPlan(), ShownPlanUnknown)
}

// The controller, driven by hand against a failing fetch, shows the same plan
// as the tracker: unknown until a fetch answers, the snapshot's plan after.
func TestBalanceControllerAFailedFetchIsNeverFree(t *testing.T) {
	// the network paid; its stored jwt still says Free; the balance read
	// fails, as on 2025-12-04
	stub := newBalanceFetchStub(nil)
	stub.set(nil, errors.New("synthetic database timeout"))
	vc, _ := newStepTestSubscriptionBalanceVc(t, stub)
	vc.api.SetByJwt(testSubscriptionJwtIssued(t, false, 1))

	vc.Start()
	// the first fetch is on its way: unknown, not Free
	connect.AssertEqual(t, vc.GetShownPlan(), ShownPlanUnknown)

	vc.step()
	connect.AssertEqual(t, stub.fetchCount(), 1)
	connect.AssertEqual(t, vc.GetIsLoaded(), false)
	connect.AssertEqual(t, vc.GetLastFetchError(), "synthetic database timeout")
	// control: the rule before this change showed the paying network Free
	connect.AssertEqual(t, shownPlanBefore(vc.GetIsPro()), ShownPlanFree)
	if plan := vc.GetShownPlan(); plan != ShownPlanUnknown {
		t.Fatalf("plan %q after a failed fetch, want %q: a failed read shows a paying network Free", plan, ShownPlanUnknown)
	}

	// the fetch comes back: the server's answer
	stub.set(testBalanceResult(100, 50, 0, "stripe"), nil)
	vc.Refresh()
	vc.step()
	connect.AssertEqual(t, vc.GetIsLoaded(), true)
	connect.AssertEqual(t, vc.GetShownPlan(), ShownPlanPro)

	// a fetch that fails after a load keeps the snapshot and its plan
	stub.set(nil, errors.New("synthetic database timeout"))
	vc.Refresh()
	vc.step()
	connect.AssertEqual(t, vc.GetShownPlan(), ShownPlanPro)

	// a sign out forgets it
	vc.Stop()
	connect.AssertEqual(t, vc.GetShownPlan(), ShownPlanUnknown)
}
