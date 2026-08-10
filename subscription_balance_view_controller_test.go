package sdk

import (
	"context"
	"sync"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect"
)

// ---- confirmation budget (the D1 fix, as pure math) --------------------------

func TestConfirmationBudgetTracker(t *testing.T) {
	base := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return base.Add(d) }

	tracker := confirmationBudgetTracker{budget: 120 * time.Second}

	// paused: nothing is consumed, no matter how much wall time passes
	if tracker.running() {
		t.Fatal("fresh tracker must be paused")
	}
	if got := tracker.consumedAt(at(time.Hour)); got != 0 {
		t.Fatalf("paused tracker consumed %v", got)
	}

	// run 30s
	tracker.resume(at(0))
	if !tracker.running() {
		t.Fatal("resume did not run the tracker")
	}
	if got := tracker.consumedAt(at(30 * time.Second)); got != 30*time.Second {
		t.Fatalf("consumed = %v, want 30s", got)
	}

	// pause at 30s; a 10 minute browser detour consumes NOTHING
	tracker.pause(at(30 * time.Second))
	if got := tracker.consumedAt(at(10*time.Minute + 30*time.Second)); got != 30*time.Second {
		t.Fatalf("consumed across pause = %v, want 30s", got)
	}
	if tracker.expiredAt(at(24 * time.Hour)) {
		t.Fatal("a paused tracker must never expire")
	}

	// resume after the detour; the second segment adds to the first
	tracker.resume(at(20 * time.Minute))
	if got := tracker.consumedAt(at(20*time.Minute + 60*time.Second)); got != 90*time.Second {
		t.Fatalf("consumed after resume = %v, want 90s", got)
	}
	if tracker.expiredAt(at(20*time.Minute + 89*time.Second)) {
		t.Fatal("expired before the budget was consumed")
	}
	if !tracker.expiredAt(at(20*time.Minute + 90*time.Second)) {
		t.Fatal("not expired after the budget was consumed")
	}
	if got := tracker.remainingAt(at(20*time.Minute + 60*time.Second)); got != 30*time.Second {
		t.Fatalf("remaining = %v, want 30s", got)
	}

	// double resume / double pause are idempotent
	tracker = confirmationBudgetTracker{budget: time.Minute}
	tracker.resume(at(0))
	tracker.resume(at(10 * time.Second)) // ignored; still anchored at 0
	if got := tracker.consumedAt(at(20 * time.Second)); got != 20*time.Second {
		t.Fatalf("double resume consumed = %v, want 20s", got)
	}
	tracker.pause(at(20 * time.Second))
	tracker.pause(at(40 * time.Second))
	if got := tracker.consumedAt(at(time.Hour)); got != 20*time.Second {
		t.Fatalf("double pause consumed = %v, want 20s", got)
	}

	// a clock that steps backwards while running must not credit the budget
	tracker = confirmationBudgetTracker{budget: time.Minute}
	tracker.resume(at(10 * time.Second))
	tracker.pause(at(5 * time.Second))
	if got := tracker.consumedAt(at(time.Hour)); got != 0 {
		t.Fatalf("backwards clock consumed = %v, want 0", got)
	}
}

// ---- test harness ------------------------------------------------------------

func testSubscriptionJwt(t *testing.T, pro bool, guest bool) string {
	t.Helper()
	token, err := gojwt.NewWithClaims(gojwt.SigningMethodNone, gojwt.MapClaims{
		"network_id": "00000000-0000-0000-0000-000000000011",
		"pro":        pro,
		"guest_mode": guest,
	}).SignedString(gojwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func testBalanceResult(start ByteCount, available ByteCount, pending ByteCount, store string) *SubscriptionBalanceResult {
	result := &SubscriptionBalanceResult{
		StartBalanceByteCount: start,
		BalanceByteCount:      available,
		OpenTransferByteCount: pending,
	}
	if store != "" {
		result.CurrentSubscription = &Subscription{
			Store: store,
			Plan:  SubscriptionPlanSupporter,
		}
		result.Subscriptions = NewSubscriptionList()
		result.Subscriptions.Add(result.CurrentSubscription)
	}
	return result
}

// balanceFetchStub serves a settable SubscriptionBalanceResult and counts
// fetches.
type balanceFetchStub struct {
	mutex   sync.Mutex
	result  *SubscriptionBalanceResult
	err     error
	count   int
	fetched chan struct{}
}

func newBalanceFetchStub(result *SubscriptionBalanceResult) *balanceFetchStub {
	return &balanceFetchStub{
		result:  result,
		fetched: make(chan struct{}, 128),
	}
}

func (self *balanceFetchStub) set(result *SubscriptionBalanceResult, err error) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.result = result
	self.err = err
}

func (self *balanceFetchStub) fetchCount() int {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.count
}

func (self *balanceFetchStub) fetch(callback SubscriptionBalanceCallback) {
	self.mutex.Lock()
	self.count += 1
	result := self.result
	err := self.err
	self.mutex.Unlock()
	select {
	case self.fetched <- struct{}{}:
	default:
	}
	callback.Result(result, err)
}

func (self *balanceFetchStub) awaitFetch(t *testing.T, message string) {
	t.Helper()
	select {
	case <-self.fetched:
	case <-time.After(5 * time.Second):
		t.Fatal(message)
	}
}

type confirmationStateRecorder struct {
	states chan string
}

func newConfirmationStateRecorder() *confirmationStateRecorder {
	return &confirmationStateRecorder{states: make(chan string, 128)}
}

func (self *confirmationStateRecorder) PurchaseConfirmationStateChanged(state string) {
	self.states <- state
}

func (self *confirmationStateRecorder) await(t *testing.T, expected string, message string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case state := <-self.states:
			if state == expected {
				return
			}
			// skip intermediate states (e.g. waiting before confirmed)
		case <-deadline:
			t.Fatalf("%s (never saw %q)", message, expected)
		}
	}
}

func (self *confirmationStateRecorder) requireNone(t *testing.T, wait time.Duration, message string) {
	t.Helper()
	select {
	case state := <-self.states:
		t.Fatalf("%s: got %q", message, state)
	case <-time.After(wait):
	}
}

type jwtOutOfSyncRecorder struct {
	signals chan bool
}

func newJwtOutOfSyncRecorder() *jwtOutOfSyncRecorder {
	return &jwtOutOfSyncRecorder{signals: make(chan bool, 128)}
}

func (self *jwtOutOfSyncRecorder) SubscriptionJwtOutOfSync(serverIsPro bool) {
	self.signals <- serverIsPro
}

// newTestSubscriptionBalanceVc builds a controller on a stub fetch with fast
// test intervals. No HTTP is performed.
func newTestSubscriptionBalanceVc(
	t *testing.T,
	byJwt string,
	stub *balanceFetchStub,
) (*SubscriptionBalanceViewController, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	api := NewApi(ctx, connect.NewClientStrategyWithDefaults(ctx), "http://127.0.0.1:0")
	api.SetByJwt(byJwt)
	vc := newSubscriptionBalanceViewController(ctx, api)
	vc.fetchFunc = stub.fetch
	vc.SetBackgroundPollIntervalMillis(40)
	vc.SetConfirmationPollIntervalMillis(15)
	vc.SetConfirmationBudgetMillis(60_000) // effectively unlimited unless a test shrinks it
	closeFunc := func() {
		vc.Close()
		api.Close()
		cancel()
	}
	return vc, closeFunc
}

func awaitCondition(t *testing.T, message string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(message)
}

// ---- balance math ------------------------------------------------------------

func TestSubscriptionBalanceMath(t *testing.T) {
	stub := newBalanceFetchStub(testBalanceResult(100, 30, 20, ""))
	vc, closeVc := newTestSubscriptionBalanceVc(t, testSubscriptionJwt(t, false, false), stub)
	defer closeVc()

	vc.Start()
	awaitCondition(t, "first snapshot never loaded", vc.GetIsLoaded)

	if got := vc.GetStartBalanceByteCount(); got != 100 {
		t.Errorf("start = %d", got)
	}
	if got := vc.GetAvailableByteCount(); got != 30 {
		t.Errorf("available = %d", got)
	}
	if got := vc.GetPendingByteCount(); got != 20 {
		t.Errorf("pending = %d", got)
	}
	if got := vc.GetUsedBalanceByteCount(); got != 50 {
		t.Errorf("used = %d, want 50", got)
	}
	if vc.GetIsPro() {
		t.Error("free network reported pro")
	}

	// available + pending exceeding start (independently sampled server
	// values) must clamp used at 0, never go negative
	stub.set(testBalanceResult(100, 80, 40, ""), nil)
	awaitCondition(t, "clamped snapshot never applied", func() bool {
		return vc.GetAvailableByteCount() == 80
	})
	if got := vc.GetUsedBalanceByteCount(); got != 0 {
		t.Errorf("used = %d, want 0 (clamped)", got)
	}

	// zero everything: used stays 0
	stub.set(testBalanceResult(0, 0, 0, ""), nil)
	awaitCondition(t, "zero snapshot never applied", func() bool {
		return vc.GetStartBalanceByteCount() == 0
	})
	if got := vc.GetUsedBalanceByteCount(); got != 0 {
		t.Errorf("used = %d, want 0", got)
	}
}

func TestSubscriptionBalanceStoreClassification(t *testing.T) {
	stub := newBalanceFetchStub(testBalanceResult(100, 50, 0, "Google Play"))
	vc, closeVc := newTestSubscriptionBalanceVc(t, testSubscriptionJwt(t, true, false), stub)
	defer closeVc()

	vc.Start()
	awaitCondition(t, "snapshot never loaded", vc.GetIsLoaded)
	if got := vc.GetCurrentStore(); got != SubscriptionStoreGoogle {
		t.Errorf("GetCurrentStore = %q, want google", got)
	}
	if !vc.GetIsPro() {
		t.Error("subscription present but not pro")
	}
	if got := vc.GetSubscriptions().Len(); got != 1 {
		t.Errorf("subscriptions len = %d", got)
	}
	if vc.GetCurrentSubscription() == nil {
		t.Error("current subscription nil")
	}
}

// ---- jwt reconciliation ------------------------------------------------------

func TestJwtReconciliationUpgradeDirection(t *testing.T) {
	// jwt says free; server says pro -> refresh needed, serverIsPro = true
	stub := newBalanceFetchStub(testBalanceResult(100, 50, 0, "stripe"))
	vc, closeVc := newTestSubscriptionBalanceVc(t, testSubscriptionJwt(t, false, false), stub)
	defer closeVc()

	recorder := newJwtOutOfSyncRecorder()
	sub := vc.AddSubscriptionJwtOutOfSyncListener(recorder)
	defer sub.Close()

	vc.Start()
	select {
	case serverIsPro := <-recorder.signals:
		if !serverIsPro {
			t.Error("upgrade direction must report serverIsPro = true")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("jwt out-of-sync never fired for the upgrade direction")
	}

	// no re-fire while the request is outstanding, even across more polls.
	// (background polling stopped here -- supporter with balance -- so force
	// polls to prove the point)
	vc.Refresh()
	stub.awaitFetch(t, "forced poll did not run")
	select {
	case <-recorder.signals:
		t.Fatal("out-of-sync re-fired before JwtRefreshed")
	case <-time.After(150 * time.Millisecond):
	}

	// platform installs a refreshed jwt whose claim agrees; no more signals
	vc.api.SetByJwt(testSubscriptionJwt(t, true, false))
	vc.JwtRefreshed()
	vc.Refresh()
	stub.awaitFetch(t, "post-refresh poll did not run")
	select {
	case <-recorder.signals:
		t.Fatal("out-of-sync fired after the claim was reconciled")
	case <-time.After(150 * time.Millisecond):
	}
}

func TestJwtReconciliationLapseDirection(t *testing.T) {
	// jwt says pro; server says free -> refresh needed, serverIsPro = false
	stub := newBalanceFetchStub(testBalanceResult(100, 50, 0, ""))
	vc, closeVc := newTestSubscriptionBalanceVc(t, testSubscriptionJwt(t, true, false), stub)
	defer closeVc()

	recorder := newJwtOutOfSyncRecorder()
	sub := vc.AddSubscriptionJwtOutOfSyncListener(recorder)
	defer sub.Close()

	// before any load, pro is seeded from the jwt claim
	vc.Start()
	select {
	case serverIsPro := <-recorder.signals:
		if serverIsPro {
			t.Error("lapse direction must report serverIsPro = false")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("jwt out-of-sync never fired for the lapse direction")
	}
	// after the load, the server wins the pro derivation
	if vc.GetIsPro() {
		t.Error("server-free network still reports pro after load")
	}

	// a refresh that still carries the stale claim re-arms detection: the
	// next poll fires again
	vc.JwtRefreshed() // jwt unchanged: still pro=true
	select {
	case serverIsPro := <-recorder.signals:
		if serverIsPro {
			t.Error("re-fired signal must report serverIsPro = false")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("out-of-sync did not re-fire after an ineffective refresh")
	}
}

func TestJwtSeedsProAndGuestBeforeLoad(t *testing.T) {
	stub := newBalanceFetchStub(nil)
	stub.set(nil, context.DeadlineExceeded) // fetches fail; nothing loads
	vc, closeVc := newTestSubscriptionBalanceVc(t, testSubscriptionJwt(t, true, true), stub)
	defer closeVc()

	vc.Start()
	if !vc.GetIsPro() {
		t.Error("pro claim did not seed IsPro before load")
	}
	if !vc.GetIsGuest() {
		t.Error("guest_mode claim did not seed IsGuest")
	}
	if vc.GetIsLoaded() {
		t.Error("loaded with failing fetches")
	}
}

// ---- polling policy ----------------------------------------------------------

func TestBackgroundPollingStopsOnSupporterWithBalance(t *testing.T) {
	stub := newBalanceFetchStub(testBalanceResult(100, 50, 0, "stripe"))
	vc, closeVc := newTestSubscriptionBalanceVc(t, testSubscriptionJwt(t, true, false), stub)
	defer closeVc()

	vc.Start()
	awaitCondition(t, "snapshot never loaded", vc.GetIsLoaded)

	// supporter with balance: polling stops entirely
	stable := stub.fetchCount()
	time.Sleep(250 * time.Millisecond) // > 6 background intervals
	if got := stub.fetchCount(); got != stable {
		t.Errorf("supporter-with-balance still polled: %d -> %d", stable, got)
	}

	// Refresh still forces a poll
	vc.Refresh()
	stub.awaitFetch(t, "Refresh did not poll")

	// a lapse (server no longer pro) resumes the background poll
	stub.set(testBalanceResult(100, 50, 0, ""), nil)
	vc.Refresh()
	awaitCondition(t, "lapse snapshot never applied", func() bool { return !vc.GetIsPro() })
	before := stub.fetchCount()
	awaitCondition(t, "background polling did not resume after lapse", func() bool {
		return stub.fetchCount() > before
	})
}

func TestBackgroundPollingContinuesForFreeNetwork(t *testing.T) {
	stub := newBalanceFetchStub(testBalanceResult(100, 50, 0, ""))
	vc, closeVc := newTestSubscriptionBalanceVc(t, testSubscriptionJwt(t, false, false), stub)
	defer closeVc()

	vc.Start()
	stub.awaitFetch(t, "no initial poll")
	stub.awaitFetch(t, "no second background poll")
	stub.awaitFetch(t, "no third background poll")
}

func TestStopHaltsPollingAndClearsSnapshot(t *testing.T) {
	stub := newBalanceFetchStub(testBalanceResult(100, 50, 0, ""))
	vc, closeVc := newTestSubscriptionBalanceVc(t, testSubscriptionJwt(t, false, false), stub)
	defer closeVc()

	vc.Start()
	awaitCondition(t, "snapshot never loaded", vc.GetIsLoaded)
	vc.Stop()
	if vc.GetIsLoaded() {
		t.Error("Stop did not clear the snapshot")
	}
	// drain any in-flight fetch signal, then require silence
	time.Sleep(100 * time.Millisecond)
	for len(stub.fetched) > 0 {
		<-stub.fetched
	}
	select {
	case <-stub.fetched:
		t.Error("polling continued after Stop")
	case <-time.After(150 * time.Millisecond):
	}
}

// ---- purchase confirmation ---------------------------------------------------

func TestConfirmationConfirmedStopsPolling(t *testing.T) {
	// baseline: loaded, free, with a PRE-EXISTING balance. The pre-existing
	// balance alone must not confirm (the web CheckoutReturn false-confirm).
	stub := newBalanceFetchStub(testBalanceResult(100, 60, 0, ""))
	vc, closeVc := newTestSubscriptionBalanceVc(t, testSubscriptionJwt(t, false, false), stub)
	defer closeVc()

	recorder := newConfirmationStateRecorder()
	sub := vc.AddPurchaseConfirmationListener(recorder)
	defer sub.Close()

	vc.Start()
	awaitCondition(t, "baseline never loaded", vc.GetIsLoaded)

	vc.StartPurchaseConfirmation()
	recorder.await(t, PurchaseConfirmationStateWaitingForConfirmation, "no waiting state")
	if got := vc.GetPurchaseConfirmationState(); got != PurchaseConfirmationStateWaitingForConfirmation {
		t.Fatalf("state = %q", got)
	}

	// several polls of the unchanged pre-existing balance: still waiting
	stub.awaitFetch(t, "confirmation did not poll")
	stub.awaitFetch(t, "confirmation did not keep polling")
	if got := vc.GetPurchaseConfirmationState(); got != PurchaseConfirmationStateWaitingForConfirmation {
		t.Fatalf("pre-existing balance falsely confirmed: %q", got)
	}

	// the webhook lands: server flips to pro
	stub.set(testBalanceResult(100, 60, 0, "stripe"), nil)
	recorder.await(t, PurchaseConfirmationStateConfirmed, "purchase never confirmed")
	if got := vc.GetPurchaseConfirmationState(); got != PurchaseConfirmationStateConfirmed {
		t.Fatalf("state = %q, want confirmed", got)
	}

	// confirmed AND supporter-with-balance: all polling stops
	time.Sleep(100 * time.Millisecond)
	stable := stub.fetchCount()
	time.Sleep(200 * time.Millisecond)
	if got := stub.fetchCount(); got != stable {
		t.Errorf("polling continued after confirmation: %d -> %d", stable, got)
	}

	// the terminal state is acknowledged explicitly
	vc.ClearPurchaseConfirmation()
	recorder.await(t, PurchaseConfirmationStateIdle, "clear did not return to idle")
}

func TestConfirmationDataPackForExistingSupporter(t *testing.T) {
	// A pro user with balance buys a data pack. The legacy "supporter with
	// balance" rule would insta-confirm; the baseline rule requires the
	// balance to actually increase.
	stub := newBalanceFetchStub(testBalanceResult(1000, 500, 0, "stripe"))
	vc, closeVc := newTestSubscriptionBalanceVc(t, testSubscriptionJwt(t, true, false), stub)
	defer closeVc()

	recorder := newConfirmationStateRecorder()
	sub := vc.AddPurchaseConfirmationListener(recorder)
	defer sub.Close()

	vc.Start()
	awaitCondition(t, "baseline never loaded", vc.GetIsLoaded)

	vc.StartPurchaseConfirmation()
	stub.awaitFetch(t, "confirmation did not poll")
	stub.awaitFetch(t, "confirmation did not keep polling")
	if got := vc.GetPurchaseConfirmationState(); got != PurchaseConfirmationStateWaitingForConfirmation {
		t.Fatalf("unchanged balance falsely confirmed for an existing supporter: %q", got)
	}

	// the pack lands
	stub.set(testBalanceResult(1000, 500+1024, 0, "stripe"), nil)
	recorder.await(t, PurchaseConfirmationStateConfirmed, "data pack never confirmed")
}

func TestConfirmationGiveUp(t *testing.T) {
	stub := newBalanceFetchStub(testBalanceResult(100, 50, 0, ""))
	vc, closeVc := newTestSubscriptionBalanceVc(t, testSubscriptionJwt(t, false, false), stub)
	defer closeVc()

	recorder := newConfirmationStateRecorder()
	sub := vc.AddPurchaseConfirmationListener(recorder)
	defer sub.Close()

	vc.Start()
	awaitCondition(t, "baseline never loaded", vc.GetIsLoaded)

	vc.SetConfirmationBudgetMillis(120)
	vc.StartPurchaseConfirmation()
	recorder.await(t, PurchaseConfirmationStateWaitingForConfirmation, "no waiting state")
	// the webhook never lands: the budget runs out and the give-up is PUSHED
	// (a UI cannot miss it the way apple's unread purchaseConfirmationTimedOut
	// flag was missed)
	recorder.await(t, PurchaseConfirmationStateConfirmationGaveUp, "confirmation never gave up")
	if got := vc.GetPurchaseConfirmationState(); got != PurchaseConfirmationStateConfirmationGaveUp {
		t.Fatalf("state = %q, want gave up", got)
	}

	// after giving up, the background poll resumes (the entitlement can still
	// land later)
	before := stub.fetchCount()
	awaitCondition(t, "background polling did not resume after give-up", func() bool {
		return stub.fetchCount() > before
	})
}

func TestConfirmationBudgetPausesInBackground(t *testing.T) {
	// THE key fix (finding D1): the budget counts only time while polling is
	// actually running. A user off in the browser for far longer than the
	// budget must resume with budget intact -- not land on a false timeout.
	stub := newBalanceFetchStub(testBalanceResult(100, 50, 0, ""))
	vc, closeVc := newTestSubscriptionBalanceVc(t, testSubscriptionJwt(t, false, false), stub)
	defer closeVc()

	recorder := newConfirmationStateRecorder()
	sub := vc.AddPurchaseConfirmationListener(recorder)
	defer sub.Close()

	vc.Start()
	awaitCondition(t, "baseline never loaded", vc.GetIsLoaded)

	vc.SetConfirmationBudgetMillis(600)
	vc.StartPurchaseConfirmation()
	recorder.await(t, PurchaseConfirmationStateWaitingForConfirmation, "no waiting state")
	stub.awaitFetch(t, "confirmation did not poll")

	// user goes off to the browser to type card details
	vc.SetForeground(false)
	time.Sleep(50 * time.Millisecond) // let any in-flight tick settle
	pausedCount := stub.fetchCount()

	// >3x the whole budget passes in wall-clock time while paused
	time.Sleep(2 * time.Second)

	if got := stub.fetchCount(); got != pausedCount {
		t.Errorf("polled while backgrounded: %d -> %d", pausedCount, got)
	}
	recorder.requireNone(t, 10*time.Millisecond, "state changed while paused")
	if got := vc.GetPurchaseConfirmationState(); got != PurchaseConfirmationStateWaitingForConfirmation {
		t.Fatalf("false timeout while paused: %q", got)
	}

	// back to the app: an immediate poll fires and the budget resumes where
	// it left off
	for len(stub.fetched) > 0 {
		<-stub.fetched
	}
	vc.SetForeground(true)
	stub.awaitFetch(t, "no immediate poll on foreground resume")
	if got := vc.GetPurchaseConfirmationState(); got != PurchaseConfirmationStateWaitingForConfirmation {
		t.Fatalf("resumed into %q, want still waiting", got)
	}

	// the webhook lands after the resume -- long after the wall-clock budget
	// expired -- and the purchase still confirms
	stub.set(testBalanceResult(100, 50, 0, "stripe"), nil)
	recorder.await(t, PurchaseConfirmationStateConfirmed, "purchase never confirmed after resume")
}

func TestConfirmationGivesUpAfterResumeWhenBudgetConsumed(t *testing.T) {
	stub := newBalanceFetchStub(testBalanceResult(100, 50, 0, ""))
	vc, closeVc := newTestSubscriptionBalanceVc(t, testSubscriptionJwt(t, false, false), stub)
	defer closeVc()

	recorder := newConfirmationStateRecorder()
	sub := vc.AddPurchaseConfirmationListener(recorder)
	defer sub.Close()

	vc.Start()
	awaitCondition(t, "baseline never loaded", vc.GetIsLoaded)

	vc.SetConfirmationBudgetMillis(200)
	vc.StartPurchaseConfirmation()
	recorder.await(t, PurchaseConfirmationStateWaitingForConfirmation, "no waiting state")

	// pause partway through, then resume: the remaining budget is consumed
	// and the confirmation honestly gives up
	time.Sleep(60 * time.Millisecond)
	vc.SetForeground(false)
	time.Sleep(300 * time.Millisecond)
	vc.SetForeground(true)
	recorder.await(t, PurchaseConfirmationStateConfirmationGaveUp, "no give-up after budget consumed across a pause")
}

func TestConfirmationImmediatePollOnStart(t *testing.T) {
	stub := newBalanceFetchStub(testBalanceResult(100, 50, 0, "stripe"))
	vc, closeVc := newTestSubscriptionBalanceVc(t, testSubscriptionJwt(t, true, false), stub)
	defer closeVc()

	vc.Start()
	awaitCondition(t, "snapshot never loaded", vc.GetIsLoaded)
	// supporter with balance: background polling is stopped
	time.Sleep(100 * time.Millisecond)
	for len(stub.fetched) > 0 {
		<-stub.fetched
	}
	before := stub.fetchCount()
	// starting a confirmation must poll immediately, not wait an interval
	vc.StartPurchaseConfirmation()
	stub.awaitFetch(t, "StartPurchaseConfirmation did not poll immediately")
	if got := stub.fetchCount(); got <= before {
		t.Errorf("fetch count did not advance: %d -> %d", before, got)
	}
}

func TestConfirmationBudgetRemainingMillis(t *testing.T) {
	stub := newBalanceFetchStub(testBalanceResult(100, 50, 0, ""))
	vc, closeVc := newTestSubscriptionBalanceVc(t, testSubscriptionJwt(t, false, false), stub)
	defer closeVc()

	if got := vc.GetConfirmationBudgetRemainingMillis(); got != 0 {
		t.Errorf("remaining with no confirmation = %d", got)
	}
	vc.Start()
	vc.SetConfirmationBudgetMillis(60_000)
	vc.StartPurchaseConfirmation()
	remaining := vc.GetConfirmationBudgetRemainingMillis()
	if remaining <= 0 || remaining > 60_000 {
		t.Errorf("remaining = %d, want (0, 60000]", remaining)
	}
}

func TestFetchErrorKeepsLastSnapshot(t *testing.T) {
	stub := newBalanceFetchStub(testBalanceResult(100, 30, 20, ""))
	vc, closeVc := newTestSubscriptionBalanceVc(t, testSubscriptionJwt(t, false, false), stub)
	defer closeVc()

	vc.Start()
	awaitCondition(t, "snapshot never loaded", vc.GetIsLoaded)

	// the api starts failing: the last snapshot stays published and polling
	// keeps retrying
	stub.set(nil, context.DeadlineExceeded)
	stub.awaitFetch(t, "no retry after error")
	stub.awaitFetch(t, "no second retry after error")
	if !vc.GetIsLoaded() || vc.GetAvailableByteCount() != 30 {
		t.Error("error dropped the last snapshot")
	}
}
