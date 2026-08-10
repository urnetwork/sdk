package sdk

import (
	"context"
	"sync"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect"
)

// subscriptionBalanceSettings holds the view controller's tunables. The
// exported accessors bind durations as int64 millis.
type subscriptionBalanceSettings struct {
	// BackgroundPollInterval keeps the balance fresh while nothing special is
	// happening (the usage bar, the plan badge).
	BackgroundPollInterval time.Duration
	// ConfirmationPollInterval is the post-purchase poll rate, bridging the
	// gap until the server's payment webhook lands.
	ConfirmationPollInterval time.Duration
	// ConfirmationBudget is how much POLLING time a confirmation may consume
	// before giving up. It is a budget, not a wall-clock deadline: time spent
	// paused (app backgrounded, window hidden -- SetForeground(false)) does
	// not count, so a user who spends minutes in their browser typing card
	// details resumes with the budget intact.
	ConfirmationBudget time.Duration
}

func defaultSubscriptionBalanceSettings() *subscriptionBalanceSettings {
	return &subscriptionBalanceSettings{
		BackgroundPollInterval:   30 * time.Second,
		ConfirmationPollInterval: 5 * time.Second,
		ConfirmationBudget:       120 * time.Second,
	}
}

// SubscriptionBalanceChangeListener fires whenever the published balance or
// plan state changes. The app re-reads the Get* accessors.
type SubscriptionBalanceChangeListener interface {
	SubscriptionBalanceChanged()
}

// SubscriptionJwtOutOfSyncListener fires when the jwt's `pro` claim and the
// server's plan state disagree (in EITHER direction: an upgrade and a lapse
// both go stale in the token). The PLATFORM owns the actual refresh -- it
// calls Api.RefreshJwt (or its own auth flow), installs the new jwt, and then
// calls JwtRefreshed() on the controller so the claim tracking advances. The
// controller only owns the detection, and fires once per disagreement (again
// after JwtRefreshed if the refreshed claim still disagrees).
type SubscriptionJwtOutOfSyncListener interface {
	// serverIsPro is the server's side of the disagreement (the source of
	// truth): true on an upgrade the jwt missed, false on a lapse.
	SubscriptionJwtOutOfSync(serverIsPro bool)
}

// The purchase-confirmation lifecycle, delivered via
// PurchaseConfirmationListener and readable from
// GetPurchaseConfirmationState. Confirmed and ConfirmationGaveUp are TERMINAL
// states a UI must consume (show, then ClearPurchaseConfirmation) -- they are
// pushed through the listener precisely so no platform can leave a timeout
// flag unread.
const (
	// No confirmation in progress.
	PurchaseConfirmationStateIdle = "idle"
	// WaitingForConfirmation: the purchase was handed to the store/browser
	// and the controller is polling for the server to reflect it.
	PurchaseConfirmationStateWaitingForConfirmation = "waiting_for_confirmation"
	// Confirmed: the server reflects the purchase (see StartPurchaseConfirmation
	// for the exact rule). Confirmation polling has stopped.
	PurchaseConfirmationStateConfirmed = "confirmed"
	// ConfirmationGaveUp: the polling budget ran out without the server ever
	// reflecting the purchase (a lost or slow payment webhook). The money was
	// likely taken; the purchase usually still lands later -- the background
	// poll and the next launch pick it up. The UI must say so honestly
	// instead of spinning or silently staying Free.
	PurchaseConfirmationStateConfirmationGaveUp = "confirmation_gave_up"
)

// PurchaseConfirmationListener receives every confirmation state transition
// (one of the PurchaseConfirmationState* values).
type PurchaseConfirmationListener interface {
	PurchaseConfirmationStateChanged(state string)
}

// confirmationBudgetTracker meters how much running time a confirmation has
// consumed against its budget. The clock advances ONLY between resume and
// pause -- pausing (app backgrounded) freezes both the poll timer and the
// budget, so a paused confirmation can never expire (the fix for the
// windows/linux false "timed out" after a normal hosted checkout).
type confirmationBudgetTracker struct {
	budget   time.Duration
	consumed time.Duration
	// zero when paused
	runningSince time.Time
}

func (self *confirmationBudgetTracker) running() bool {
	return !self.runningSince.IsZero()
}

func (self *confirmationBudgetTracker) resume(now time.Time) {
	if self.runningSince.IsZero() {
		self.runningSince = now
	}
}

func (self *confirmationBudgetTracker) pause(now time.Time) {
	if !self.runningSince.IsZero() {
		if d := now.Sub(self.runningSince); d > 0 {
			self.consumed += d
		}
		self.runningSince = time.Time{}
	}
}

func (self *confirmationBudgetTracker) consumedAt(now time.Time) time.Duration {
	consumed := self.consumed
	if !self.runningSince.IsZero() {
		if d := now.Sub(self.runningSince); d > 0 {
			consumed += d
		}
	}
	return consumed
}

func (self *confirmationBudgetTracker) remainingAt(now time.Time) time.Duration {
	return self.budget - self.consumedAt(now)
}

func (self *confirmationBudgetTracker) expiredAt(now time.Time) bool {
	return self.remainingAt(now) <= 0
}

// SubscriptionBalanceViewController is the shared subscription-balance state
// machine for every app: one implementation of the plan/balance snapshot, the
// jwt `pro`-claim reconciliation, and the polling policy that every platform
// previously hand-rolled (apple/android SubscriptionBalanceViewModel, windows
// + linux SubscriptionBalance, web AuthContext/CheckoutReturn).
//
// It owns:
//
//   - The latest SubscriptionBalanceResult and its derivations: IsPro
//     (current_subscription != nil, the server's source of truth), the used
//     balance (start - available - pending, clamped at 0), the multi-store
//     Subscriptions list, and the normalized store family of the current
//     subscription (ClassifySubscriptionStore).
//
//   - Jwt reconciliation: the jwt's `pro` claim is baked in at issue time and
//     goes stale on BOTH an upgrade and a lapse. When it disagrees with the
//     server, the controller fires SubscriptionJwtOutOfSyncListener; the
//     platform refreshes the jwt and reports back via JwtRefreshed.
//
//   - The polling state machine: idle / background (default 30 s, stops once
//     the network is a supporter with a positive balance) / confirmation
//     (default 5 s, with a 120 s polling BUDGET). SetForeground(false)
//     pauses the poll timer AND the budget clock together, so time the user
//     spends off in a browser typing card details does not burn the budget;
//     SetForeground(true) resumes with an immediate poll. Confirmation ends
//     in a terminal state (Confirmed / ConfirmationGaveUp) that is pushed to
//     the app -- never a silently-set flag.
//
// The controller does NOT perform the jwt refresh, open any checkout, or
// finish any store transaction. Those stay platform-owned.
type SubscriptionBalanceViewController struct {
	ctx    context.Context
	cancel context.CancelFunc
	api    *Api

	wake chan struct{}

	stateLock sync.Mutex

	settings *subscriptionBalanceSettings

	started    bool
	foreground bool
	// invalidates in-flight fetches across Stop/Start (logout/login)
	generation int

	fetchInFlight bool
	// next scheduled poll; the zero time means "due now"
	nextPollAt time.Time
	// force one poll regardless of the background stop rule (Refresh, Start,
	// foreground resume, StartPurchaseConfirmation)
	forcePoll bool

	// confirmation
	confirming        bool
	budget            confirmationBudgetTracker
	baselineLoaded    bool
	baselineIsPro     bool
	baselineAvailable ByteCount
	confirmationState string

	// snapshot (server state; seeded from the jwt claims before first load)
	loaded              bool
	startBalance        ByteCount
	available           ByteCount
	pending             ByteCount
	serverIsPro         bool
	currentSubscription *Subscription
	subscriptions       *SubscriptionList

	// jwt claim tracking
	jwtPro              bool
	jwtGuest            bool
	jwtRefreshRequested bool

	balanceListeners      *connect.CallbackList[SubscriptionBalanceChangeListener]
	jwtListeners          *connect.CallbackList[SubscriptionJwtOutOfSyncListener]
	confirmationListeners *connect.CallbackList[PurchaseConfirmationListener]

	// test seams (unexported; not bound)
	nowFunc   func() time.Time
	fetchFunc func(callback SubscriptionBalanceCallback)
}

// NewSubscriptionBalanceViewController creates a standalone controller on an
// Api. Platforms with a Device should prefer
// OpenSubscriptionBalanceViewController, which ties the controller into the
// device's view-controller lifecycle; this constructor serves platforms that
// only hold an Api (the web app, pre-login flows). The caller owns Close.
func NewSubscriptionBalanceViewController(api *Api) *SubscriptionBalanceViewController {
	return newSubscriptionBalanceViewController(api.ctx, api)
}

func newSubscriptionBalanceViewController(ctx context.Context, api *Api) *SubscriptionBalanceViewController {
	cancelCtx, cancel := context.WithCancel(ctx)
	vc := &SubscriptionBalanceViewController{
		ctx:                   cancelCtx,
		cancel:                cancel,
		api:                   api,
		wake:                  make(chan struct{}, 1),
		settings:              defaultSubscriptionBalanceSettings(),
		foreground:            true,
		confirmationState:     PurchaseConfirmationStateIdle,
		subscriptions:         NewSubscriptionList(),
		balanceListeners:      connect.NewCallbackList[SubscriptionBalanceChangeListener](),
		jwtListeners:          connect.NewCallbackList[SubscriptionJwtOutOfSyncListener](),
		confirmationListeners: connect.NewCallbackList[PurchaseConfirmationListener](),
		nowFunc:               time.Now,
	}
	vc.fetchFunc = func(callback SubscriptionBalanceCallback) {
		api.SubscriptionBalance(callback)
	}
	go vc.run()
	return vc
}

// ---- lifecycle ---------------------------------------------------------------

// Start begins polling: it seeds the plan state offline from the stored jwt's
// claims (readable without any network call), fetches once immediately, and
// then keeps the balance fresh per the polling policy.
func (self *SubscriptionBalanceViewController) Start() {
	self.stateLock.Lock()
	if self.started {
		self.stateLock.Unlock()
		return
	}
	self.started = true
	self.applyJwtClaimsLocked()
	self.forcePoll = true
	self.nextPollAt = time.Time{}
	if self.confirming && self.foreground {
		self.budget.resume(self.nowFunc())
	}
	self.stateLock.Unlock()

	self.balanceChanged()
	self.scheduleWake()
}

// Stop halts all polling and clears the snapshot and any in-progress
// confirmation (logout).
func (self *SubscriptionBalanceViewController) Stop() {
	var confirmationChanged bool
	self.stateLock.Lock()
	if !self.started {
		self.stateLock.Unlock()
		return
	}
	self.started = false
	self.generation += 1
	self.fetchInFlight = false
	self.forcePoll = false
	self.nextPollAt = time.Time{}
	self.confirming = false
	self.budget = confirmationBudgetTracker{}
	if self.confirmationState != PurchaseConfirmationStateIdle {
		self.confirmationState = PurchaseConfirmationStateIdle
		confirmationChanged = true
	}
	self.loaded = false
	self.startBalance = 0
	self.available = 0
	self.pending = 0
	self.serverIsPro = false
	self.currentSubscription = nil
	self.subscriptions = NewSubscriptionList()
	self.jwtPro = false
	self.jwtGuest = false
	self.jwtRefreshRequested = false
	self.stateLock.Unlock()

	if confirmationChanged {
		self.confirmationStateChanged(PurchaseConfirmationStateIdle)
	}
	self.balanceChanged()
	self.scheduleWake()
}

func (self *SubscriptionBalanceViewController) Close() {
	self.cancel()
}

// SetForeground reports whether the app can usefully poll (window visible,
// app foregrounded). Backgrounding pauses the poll timers AND the
// confirmation budget clock together -- a confirmation can never expire while
// paused, so a user who spends minutes off in their browser resumes with the
// budget intact instead of a false "timed out". Foregrounding fires an
// immediate poll.
func (self *SubscriptionBalanceViewController) SetForeground(foreground bool) {
	self.stateLock.Lock()
	if self.foreground == foreground {
		self.stateLock.Unlock()
		return
	}
	self.foreground = foreground
	now := self.nowFunc()
	if foreground {
		if self.started {
			if self.confirming {
				self.budget.resume(now)
			}
			self.forcePoll = true
			self.nextPollAt = time.Time{}
		}
	} else {
		self.budget.pause(now)
	}
	self.stateLock.Unlock()

	self.scheduleWake()
}

// Refresh fetches the balance now (navigating to the account screen, a
// redeem success), regardless of the background stop rule.
func (self *SubscriptionBalanceViewController) Refresh() {
	self.stateLock.Lock()
	self.forcePoll = true
	self.nextPollAt = time.Time{}
	self.stateLock.Unlock()

	self.scheduleWake()
}

// ---- purchase confirmation ---------------------------------------------------

// StartPurchaseConfirmation begins the post-purchase confirmation poll: after
// a checkout was handed to the store/browser (or a balance code redeemed),
// poll every ConfirmationPollInterval until the server reflects the purchase,
// giving up once the polling BUDGET (ConfirmationBudget, default 120 s of
// actual polling time -- paused time does not count) is consumed.
//
// The confirmation rule is anchored to a BASELINE captured from the last
// loaded snapshot, so a pre-existing balance can never instantly
// "confirm" a purchase (the web CheckoutReturn bug):
//
//   - upgraded to Pro (server pro flipped false -> true vs the baseline), or
//   - balance increased above the baseline (a data pack landing), or
//   - if no snapshot was ever loaded: pro with a positive balance (the
//     legacy "supporter with balance" rule; there is nothing to diff against).
//
// Calling this while a confirmation is already running restarts it (fresh
// budget, fresh baseline): each hand-back of a new purchase deserves a full
// window. Terminal states arrive via PurchaseConfirmationListener.
func (self *SubscriptionBalanceViewController) StartPurchaseConfirmation() {
	self.stateLock.Lock()
	now := self.nowFunc()
	self.confirming = true
	self.budget = confirmationBudgetTracker{budget: self.settings.ConfirmationBudget}
	if self.started && self.foreground {
		self.budget.resume(now)
	}
	self.baselineLoaded = self.loaded
	self.baselineIsPro = self.serverIsPro
	self.baselineAvailable = self.available
	self.confirmationState = PurchaseConfirmationStateWaitingForConfirmation
	self.forcePoll = true
	self.nextPollAt = time.Time{}
	self.stateLock.Unlock()

	self.confirmationStateChanged(PurchaseConfirmationStateWaitingForConfirmation)
	self.scheduleWake()
}

// ClearPurchaseConfirmation acknowledges a terminal confirmation state
// (Confirmed or ConfirmationGaveUp), returning to Idle. It does not stop a
// confirmation still in progress.
func (self *SubscriptionBalanceViewController) ClearPurchaseConfirmation() {
	self.stateLock.Lock()
	if self.confirming || self.confirmationState == PurchaseConfirmationStateIdle {
		self.stateLock.Unlock()
		return
	}
	self.confirmationState = PurchaseConfirmationStateIdle
	self.stateLock.Unlock()

	self.confirmationStateChanged(PurchaseConfirmationStateIdle)
}

// GetPurchaseConfirmationState returns the current
// PurchaseConfirmationState* value.
func (self *SubscriptionBalanceViewController) GetPurchaseConfirmationState() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.confirmationState
}

// GetConfirmationBudgetRemainingMillis returns how much polling budget the
// in-progress confirmation has left, for a UI countdown. 0 when no
// confirmation is running.
func (self *SubscriptionBalanceViewController) GetConfirmationBudgetRemainingMillis() int64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !self.confirming {
		return 0
	}
	remaining := self.budget.remainingAt(self.nowFunc())
	if remaining < 0 {
		return 0
	}
	return remaining.Milliseconds()
}

// ---- jwt reconciliation ------------------------------------------------------

// JwtRefreshed tells the controller the platform installed a (freshly
// refreshed) jwt on the Api, so the claim tracking advances from the real new
// token. If the refreshed claim STILL disagrees with the server, the next
// poll fires SubscriptionJwtOutOfSyncListener again.
func (self *SubscriptionBalanceViewController) JwtRefreshed() {
	self.stateLock.Lock()
	previousPro := self.jwtPro
	previousGuest := self.jwtGuest
	self.applyJwtClaimsLocked()
	self.jwtRefreshRequested = false
	changed := (!self.loaded && previousPro != self.jwtPro) || previousGuest != self.jwtGuest
	self.stateLock.Unlock()

	if changed {
		self.balanceChanged()
	}
}

// caller must hold stateLock
func (self *SubscriptionBalanceViewController) applyJwtClaimsLocked() {
	pro, guest := parseJwtProGuestClaims(self.api.GetByJwt())
	self.jwtPro = pro
	self.jwtGuest = guest
}

func parseJwtProGuestClaims(byJwt string) (pro bool, guest bool) {
	if byJwt == "" {
		return false, false
	}
	claims := gojwt.MapClaims{}
	if _, _, err := gojwt.NewParser().ParseUnverified(byJwt, claims); err != nil {
		return false, false
	}
	if v, ok := claims["pro"].(bool); ok {
		pro = v
	}
	if v, ok := claims["guest_mode"].(bool); ok {
		guest = v
	}
	return
}

// ---- snapshot accessors ------------------------------------------------------

// GetIsPro is the plan state: the server's current_subscription != nil once a
// snapshot has loaded, seeded from the jwt's `pro` claim before that.
func (self *SubscriptionBalanceViewController) GetIsPro() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.isProLocked()
}

// caller must hold stateLock
func (self *SubscriptionBalanceViewController) isProLocked() bool {
	if self.loaded {
		return self.serverIsPro
	}
	return self.jwtPro
}

// GetIsGuest is the jwt's guest_mode claim.
func (self *SubscriptionBalanceViewController) GetIsGuest() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.jwtGuest
}

// GetIsLoaded reports whether at least one balance fetch succeeded this
// session. The byte-count accessors are meaningful only once loaded.
func (self *SubscriptionBalanceViewController) GetIsLoaded() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.loaded
}

// GetStartBalanceByteCount is the balance the network started the day with.
func (self *SubscriptionBalanceViewController) GetStartBalanceByteCount() ByteCount {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.startBalance
}

// GetAvailableByteCount is the remaining available balance.
func (self *SubscriptionBalanceViewController) GetAvailableByteCount() ByteCount {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.available
}

// GetPendingByteCount is the balance tied up in open transfers.
func (self *SubscriptionBalanceViewController) GetPendingByteCount() ByteCount {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.pending
}

// GetUsedBalanceByteCount is the derived used balance:
// start - available - pending, clamped at 0 (the server values are sampled
// independently, so the raw difference can transiently go negative).
func (self *SubscriptionBalanceViewController) GetUsedBalanceByteCount() ByteCount {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	used := self.startBalance - self.available - self.pending
	if used < 0 {
		used = 0
	}
	return used
}

// GetCurrentSubscription is ONE of the active subscriptions, or nil (the
// server's plan indicator). Use GetSubscriptions for the full per-store set.
func (self *SubscriptionBalanceViewController) GetCurrentSubscription() *Subscription {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.currentSubscription
}

// GetSubscriptions is every store currently billing this network, one entry
// per store, so each can be offered its own cancel path.
func (self *SubscriptionBalanceViewController) GetSubscriptions() *SubscriptionList {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.subscriptions
}

// GetCurrentStore is the normalized store family
// (ClassifySubscriptionStore) of the current subscription: one of the
// SubscriptionStore* values, or "" when there is no current subscription.
func (self *SubscriptionBalanceViewController) GetCurrentStore() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.currentSubscription == nil {
		return ""
	}
	return ClassifySubscriptionStore(self.currentSubscription.Store)
}

// ---- settings ----------------------------------------------------------------

func (self *SubscriptionBalanceViewController) GetBackgroundPollIntervalMillis() int64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.settings.BackgroundPollInterval.Milliseconds()
}

func (self *SubscriptionBalanceViewController) SetBackgroundPollIntervalMillis(millis int64) {
	self.stateLock.Lock()
	self.settings.BackgroundPollInterval = time.Duration(millis) * time.Millisecond
	self.stateLock.Unlock()
	self.scheduleWake()
}

func (self *SubscriptionBalanceViewController) GetConfirmationPollIntervalMillis() int64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.settings.ConfirmationPollInterval.Milliseconds()
}

func (self *SubscriptionBalanceViewController) SetConfirmationPollIntervalMillis(millis int64) {
	self.stateLock.Lock()
	self.settings.ConfirmationPollInterval = time.Duration(millis) * time.Millisecond
	self.stateLock.Unlock()
	self.scheduleWake()
}

func (self *SubscriptionBalanceViewController) GetConfirmationBudgetMillis() int64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.settings.ConfirmationBudget.Milliseconds()
}

// SetConfirmationBudgetMillis applies to the NEXT StartPurchaseConfirmation;
// an in-progress confirmation keeps the budget it started with.
func (self *SubscriptionBalanceViewController) SetConfirmationBudgetMillis(millis int64) {
	self.stateLock.Lock()
	self.settings.ConfirmationBudget = time.Duration(millis) * time.Millisecond
	self.stateLock.Unlock()
}

// ---- listeners ---------------------------------------------------------------

func (self *SubscriptionBalanceViewController) AddSubscriptionBalanceChangeListener(listener SubscriptionBalanceChangeListener) Sub {
	callbackId := self.balanceListeners.Add(listener)
	return newSub(func() {
		self.balanceListeners.Remove(callbackId)
	})
}

func (self *SubscriptionBalanceViewController) AddSubscriptionJwtOutOfSyncListener(listener SubscriptionJwtOutOfSyncListener) Sub {
	callbackId := self.jwtListeners.Add(listener)
	return newSub(func() {
		self.jwtListeners.Remove(callbackId)
	})
}

func (self *SubscriptionBalanceViewController) AddPurchaseConfirmationListener(listener PurchaseConfirmationListener) Sub {
	callbackId := self.confirmationListeners.Add(listener)
	return newSub(func() {
		self.confirmationListeners.Remove(callbackId)
	})
}

func (self *SubscriptionBalanceViewController) balanceChanged() {
	for _, listener := range self.balanceListeners.Get() {
		connect.HandleError(func() {
			listener.SubscriptionBalanceChanged()
		})
	}
}

func (self *SubscriptionBalanceViewController) jwtOutOfSync(serverIsPro bool) {
	for _, listener := range self.jwtListeners.Get() {
		connect.HandleError(func() {
			listener.SubscriptionJwtOutOfSync(serverIsPro)
		})
	}
}

func (self *SubscriptionBalanceViewController) confirmationStateChanged(state string) {
	for _, listener := range self.confirmationListeners.Get() {
		connect.HandleError(func() {
			listener.PurchaseConfirmationStateChanged(state)
		})
	}
}

// ---- polling machine ---------------------------------------------------------

func (self *SubscriptionBalanceViewController) scheduleWake() {
	select {
	case self.wake <- struct{}{}:
	default:
	}
}

// run is the polling loop. Each step fires a due poll and/or retires an
// expired confirmation budget, then arms a timer for the earliest next
// deadline (next poll, budget expiry). Paused (background) or fully stopped
// states arm no timer at all.
func (self *SubscriptionBalanceViewController) run() {
	var timer *time.Timer
	var timerC <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	armTimer := func(delay time.Duration, arm bool) {
		if timer != nil && !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		if !arm {
			timerC = nil
			return
		}
		if delay < 0 {
			delay = 0
		}
		if timer == nil {
			timer = time.NewTimer(delay)
		} else {
			timer.Reset(delay)
		}
		timerC = timer.C
	}

	for {
		select {
		case <-self.ctx.Done():
			return
		case <-self.wake:
		case <-timerC:
		}
		delay, arm := self.step()
		armTimer(delay, arm)
	}
}

// step advances the state machine once and reports the delay until the next
// deadline (arm == false when there is none).
func (self *SubscriptionBalanceViewController) step() (delay time.Duration, arm bool) {
	var launchFetch bool
	var generation int
	var gaveUp bool

	self.stateLock.Lock()
	now := self.nowFunc()

	if !self.started || !self.foreground {
		// fully paused: no polls, and the confirmation budget is frozen too
		self.stateLock.Unlock()
		return 0, false
	}

	// retire an exhausted confirmation budget
	if self.confirming && self.budget.expiredAt(now) {
		self.confirming = false
		self.budget = confirmationBudgetTracker{}
		self.confirmationState = PurchaseConfirmationStateConfirmationGaveUp
		gaveUp = true
	}

	// the background poll stops once the network is a supporter with a
	// positive balance: there is nothing left to poll for. Confirmation and
	// forced (Refresh) polls always run.
	pollEligible := self.confirming || self.forcePoll || !self.supporterWithBalanceLocked()

	pollDue := self.forcePoll || !now.Before(self.nextPollAt)
	if pollEligible && pollDue && !self.fetchInFlight {
		self.fetchInFlight = true
		self.forcePoll = false
		interval := self.settings.BackgroundPollInterval
		if self.confirming {
			interval = self.settings.ConfirmationPollInterval
		}
		self.nextPollAt = now.Add(interval)
		generation = self.generation
		launchFetch = true
	}

	// earliest next deadline
	if pollEligible && !self.fetchInFlight {
		delay = self.nextPollAt.Sub(now)
		arm = true
	}
	if self.confirming && self.budget.running() {
		budgetDelay := self.budget.remainingAt(now)
		if !arm || budgetDelay < delay {
			delay = budgetDelay
		}
		arm = true
	}
	self.stateLock.Unlock()

	if gaveUp {
		self.confirmationStateChanged(PurchaseConfirmationStateConfirmationGaveUp)
	}
	if launchFetch {
		callback := connect.NewApiCallback[*SubscriptionBalanceResult](
			func(result *SubscriptionBalanceResult, err error) {
				self.fetchDone(generation, result, err)
			},
		)
		self.fetchFunc(callback)
	}
	return
}

// caller must hold stateLock
func (self *SubscriptionBalanceViewController) supporterWithBalanceLocked() bool {
	return self.loaded && self.serverIsPro && self.available > 0
}

func (self *SubscriptionBalanceViewController) fetchDone(generation int, result *SubscriptionBalanceResult, err error) {
	var confirmed bool
	var jwtDisagrees bool
	var serverIsPro bool

	self.stateLock.Lock()
	if generation != self.generation {
		// a Stop (logout) superseded this fetch
		self.stateLock.Unlock()
		return
	}
	self.fetchInFlight = false
	if err != nil || result == nil {
		// keep the last snapshot; the poll retries
		self.stateLock.Unlock()
		self.scheduleWake()
		return
	}

	self.loaded = true
	self.startBalance = result.StartBalanceByteCount
	self.available = result.BalanceByteCount
	self.pending = result.OpenTransferByteCount
	self.currentSubscription = result.CurrentSubscription
	if result.Subscriptions != nil {
		self.subscriptions = result.Subscriptions
	} else {
		self.subscriptions = NewSubscriptionList()
	}
	// The server is the source of truth for Pro: current_subscription is set
	// exactly when the network is Pro.
	serverIsPro = result.CurrentSubscription != nil
	self.serverIsPro = serverIsPro

	// jwt reconciliation, both directions; fire once per disagreement (the
	// request flag clears when the platform reports JwtRefreshed)
	if serverIsPro != self.jwtPro && !self.jwtRefreshRequested {
		self.jwtRefreshRequested = true
		jwtDisagrees = true
	}

	if self.confirming && self.purchaseConfirmedLocked() {
		self.confirming = false
		self.budget = confirmationBudgetTracker{}
		self.confirmationState = PurchaseConfirmationStateConfirmed
		confirmed = true
	}
	self.stateLock.Unlock()

	if jwtDisagrees {
		self.jwtOutOfSync(serverIsPro)
	}
	if confirmed {
		self.confirmationStateChanged(PurchaseConfirmationStateConfirmed)
	}
	self.balanceChanged()
	// re-arm: the mode may have changed (confirmation ended, supporter with
	// balance stops the background poll)
	self.scheduleWake()
}

// caller must hold stateLock; see StartPurchaseConfirmation for the rule
func (self *SubscriptionBalanceViewController) purchaseConfirmedLocked() bool {
	if self.baselineLoaded {
		return (self.serverIsPro && !self.baselineIsPro) ||
			self.available > self.baselineAvailable
	}
	return self.serverIsPro && self.available > 0
}
