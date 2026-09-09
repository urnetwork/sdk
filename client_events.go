package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// Client events (POST /client/events): the CLOSED product-event schema and the
// queue that batches, persists and retries them.
//
// An app never names an event or a prop itself: it calls one of the New*Event
// constructors below, which are the only way to make a ClientEvent, so the
// binding cannot send a name or a prop key the server would refuse. The queue
// fills platform / app version / locale / session, batches every 30 s or on
// Flush (at most 200 per call), retries a failed call three times, then drops
// the events, and persists the pending events to the local state dir so a
// killed app loses nothing.

// Event names.
const (
	EventOnboardingStepShown     = "onboarding.step.shown"
	EventOnboardingStepCompleted = "onboarding.step.completed"
	EventOnboardingStepSkipped   = "onboarding.step.skipped"
	EventOfferScreenShown        = "offer.screen.shown"
	EventOfferCardTapped         = "offer.card.tapped"
	EventOfferCtaTapped          = "offer.cta.tapped"
	EventOfferDeclined           = "offer.declined"
	EventPurchaseStarted         = "purchase.started"
	EventPurchaseCompleted       = "purchase.completed"
	EventPurchaseCancelled       = "purchase.cancelled"
	EventPurchaseFailed          = "purchase.failed"
	EventConnectFirst            = "connect.first"
	EventWidgetAdded             = "widget.added"
	EventFeedbackSubmitted       = "feedback.submitted"
	EventSignupOptoutChanged     = "signup.optout_changed"
)

// Platforms (ClientEventQueue platform).
const (
	EventPlatformIos     = "ios"
	EventPlatformMacos   = "macos"
	EventPlatformAndroid = "android"
	EventPlatformWeb     = "web"
	EventPlatformWindows = "windows"
	EventPlatformLinux   = "linux"
)

// Stores (purchase.* store, offer.cta.tapped store).
const (
	EventStoreApple  = "apple"
	EventStorePlay   = "play"
	EventStoreStripe = "stripe"
	EventStoreSolana = "solana"
)

// Decline controls (offer.declined control).
const (
	OfferDeclineControlFreePlanLink  = "free_plan_link"
	OfferDeclineControlBack          = "back"
	OfferDeclineControlSystemDismiss = "system_dismiss"
)

// clientEventPropKeys is the closed prop set per event name, mirrored from the
// server schema (server/model/onboarding_event_schema.go). ParseClientEventsJson
// checks incoming json against it; the constructors cannot stray from it.
var clientEventPropKeys = map[string][]string{
	EventOnboardingStepShown:     {"step", "index", "elapsed_ms"},
	EventOnboardingStepCompleted: {"step", "index", "elapsed_ms"},
	EventOnboardingStepSkipped:   {"step", "index", "elapsed_ms"},
	EventOfferScreenShown:        {"surface", "experiment", "variant", "tier", "price_shown", "currency", "expires_in_s"},
	EventOfferCardTapped:         {"plan"},
	EventOfferCtaTapped:          {"plan", "store"},
	EventOfferDeclined:           {"control", "elapsed_ms"},
	EventPurchaseStarted:         {"store", "product", "plan", "trial", "price", "currency", "error_class"},
	EventPurchaseCompleted:       {"store", "product", "plan", "trial", "price", "currency", "error_class"},
	EventPurchaseCancelled:       {"store", "product", "plan", "trial", "price", "currency", "error_class"},
	EventPurchaseFailed:          {"store", "product", "plan", "trial", "price", "currency", "error_class"},
	EventConnectFirst:            {},
	EventWidgetAdded:             {"kind"},
	EventFeedbackSubmitted:       {"rating", "reason", "has_text", "text"},
	EventSignupOptoutChanged:     {"product_updates"},
}

// ClientEventNames lists the event names a client may send, sorted.
func ClientEventNames() *StringList {
	names := make([]string, 0, len(clientEventPropKeys))
	for name := range clientEventPropKeys {
		names = append(names, name)
	}
	sort.Strings(names)
	list := NewStringList()
	list.addAll(names...)
	return list
}

// ClientEvent is one product event. Make one with a New*Event constructor; the
// props never cross the binding.
type ClientEvent struct {
	Name string `json:"name"`
	// At is the event time (RFC 3339); the constructors stamp the device clock
	At string `json:"at,omitempty"`
	// Platform, AppVersion, Locale and Session are filled by the queue
	Platform   string `json:"platform,omitempty"`
	AppVersion string `json:"app_version,omitempty"`
	Locale     string `json:"locale,omitempty"`
	Session    string `json:"session,omitempty"`

	props map[string]any
}

// PropsJson is the props as json, for logging and tests.
func (self *ClientEvent) PropsJson() string {
	if len(self.props) == 0 {
		return "{}"
	}
	b, err := json.Marshal(self.props)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func (self *ClientEvent) MarshalJSON() ([]byte, error) {
	type wire struct {
		Name       string         `json:"name"`
		At         string         `json:"at,omitempty"`
		Platform   string         `json:"platform,omitempty"`
		AppVersion string         `json:"app_version,omitempty"`
		Locale     string         `json:"locale,omitempty"`
		Session    string         `json:"session,omitempty"`
		Props      map[string]any `json:"props,omitempty"`
	}
	return json.Marshal(&wire{
		Name:       self.Name,
		At:         self.At,
		Platform:   self.Platform,
		AppVersion: self.AppVersion,
		Locale:     self.Locale,
		Session:    self.Session,
		Props:      self.props,
	})
}

func (self *ClientEvent) UnmarshalJSON(b []byte) error {
	var wire struct {
		Name       string         `json:"name"`
		At         string         `json:"at,omitempty"`
		Platform   string         `json:"platform,omitempty"`
		AppVersion string         `json:"app_version,omitempty"`
		Locale     string         `json:"locale,omitempty"`
		Session    string         `json:"session,omitempty"`
		Props      map[string]any `json:"props,omitempty"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}
	self.Name = wire.Name
	self.At = wire.At
	self.Platform = wire.Platform
	self.AppVersion = wire.AppVersion
	self.Locale = wire.Locale
	self.Session = wire.Session
	self.props = wire.Props
	return nil
}

func newClientEvent(name string) *ClientEvent {
	return &ClientEvent{
		Name:  name,
		At:    time.Now().UTC().Format(time.RFC3339Nano),
		props: map[string]any{},
	}
}

func (self *ClientEvent) setString(key string, value string) *ClientEvent {
	if value = strings.TrimSpace(value); value != "" {
		self.props[key] = value
	}
	return self
}

// setInt sets a non-negative integer prop; a negative value means "not given"
func (self *ClientEvent) setInt(key string, value int64) *ClientEvent {
	if 0 <= value {
		self.props[key] = value
	}
	return self
}

// setNumber sets a non-negative number prop; a negative value means "not given"
func (self *ClientEvent) setNumber(key string, value float64) *ClientEvent {
	if 0 <= value {
		self.props[key] = value
	}
	return self
}

func (self *ClientEvent) setBool(key string, value bool) *ClientEvent {
	self.props[key] = value
	return self
}

func newStepEvent(name string, step string, index int, elapsedMs int64) *ClientEvent {
	return newClientEvent(name).setString("step", step).setInt("index", int64(index)).setInt("elapsed_ms", elapsedMs)
}

// NewOnboardingStepShownEvent: an onboarding step rendered. index and
// elapsedMs may be -1 when unknown.
func NewOnboardingStepShownEvent(step string, index int, elapsedMs int64) *ClientEvent {
	return newStepEvent(EventOnboardingStepShown, step, index, elapsedMs)
}

// NewOnboardingStepCompletedEvent: the user finished a step.
func NewOnboardingStepCompletedEvent(step string, index int, elapsedMs int64) *ClientEvent {
	return newStepEvent(EventOnboardingStepCompleted, step, index, elapsedMs)
}

// NewOnboardingStepSkippedEvent: the user skipped a step.
func NewOnboardingStepSkippedEvent(step string, index int, elapsedMs int64) *ClientEvent {
	return newStepEvent(EventOnboardingStepSkipped, step, index, elapsedMs)
}

// NewOfferScreenShownEvent: an offer surface rendered. surface is one of the
// OfferSurface* values; experiment/variant come from the plan response's
// assignment for the surface; tier is the price tier name; priceShown is the
// figure displayed (-1 when none); expiresInS the remaining validity.
func NewOfferScreenShownEvent(surface string, experiment string, variant string, tier string, priceShown float64, currency string, expiresInS int64) *ClientEvent {
	return newClientEvent(EventOfferScreenShown).
		setString("surface", surface).
		setString("experiment", experiment).
		setString("variant", variant).
		setString("tier", tier).
		setNumber("price_shown", priceShown).
		setString("currency", currency).
		setInt("expires_in_s", expiresInS)
}

// NewOfferCardTappedEvent: a plan card was tapped (PlanYearly or PlanMonthly).
func NewOfferCardTappedEvent(plan string) *ClientEvent {
	return newClientEvent(EventOfferCardTapped).setString("plan", plan)
}

// NewOfferCtaTappedEvent: the purchase button was tapped for a plan on a store
// (EventStore*).
func NewOfferCtaTappedEvent(plan string, store string) *ClientEvent {
	return newClientEvent(EventOfferCtaTapped).setString("plan", plan).setString("store", store)
}

// NewOfferDeclinedEvent: the offer was passed on through a control
// (OfferDeclineControl*); elapsedMs may be -1.
func NewOfferDeclinedEvent(control string, elapsedMs int64) *ClientEvent {
	return newClientEvent(EventOfferDeclined).setString("control", control).setInt("elapsed_ms", elapsedMs)
}

func newPurchaseEvent(name string, store string, product string, plan string, trial bool, price float64, currency string, errorClass string) *ClientEvent {
	return newClientEvent(name).
		setString("store", store).
		setString("product", product).
		setString("plan", plan).
		setBool("trial", trial).
		setNumber("price", price).
		setString("currency", currency).
		setString("error_class", errorClass)
}

// NewPurchaseStartedEvent: a store purchase flow began. price may be -1.
func NewPurchaseStartedEvent(store string, product string, plan string, trial bool, price float64, currency string) *ClientEvent {
	return newPurchaseEvent(EventPurchaseStarted, store, product, plan, trial, price, currency, "")
}

// NewPurchaseCompletedEvent: the store confirmed the purchase.
func NewPurchaseCompletedEvent(store string, product string, plan string, trial bool, price float64, currency string) *ClientEvent {
	return newPurchaseEvent(EventPurchaseCompleted, store, product, plan, trial, price, currency, "")
}

// NewPurchaseCancelledEvent: the user backed out of the store flow.
func NewPurchaseCancelledEvent(store string, product string, plan string, trial bool, price float64, currency string) *ClientEvent {
	return newPurchaseEvent(EventPurchaseCancelled, store, product, plan, trial, price, currency, "")
}

// NewPurchaseFailedEvent: the store flow failed; errorClass is a short token
// (e.g. card_declined, network, store_unavailable), never a message.
func NewPurchaseFailedEvent(store string, product string, plan string, trial bool, price float64, currency string, errorClass string) *ClientEvent {
	return newPurchaseEvent(EventPurchaseFailed, store, product, plan, trial, price, currency, errorClass)
}

// NewConnectFirstEvent: the network's first connection (once per network; the
// server deduplicates resends).
func NewConnectFirstEvent() *ClientEvent {
	return newClientEvent(EventConnectFirst)
}

// NewWidgetAddedEvent: a home/lock screen widget was added (kind is a short
// token: dashboard, globe, contracts, quick_connect, ...).
func NewWidgetAddedEvent(kind string) *ClientEvent {
	return newClientEvent(EventWidgetAdded).setString("kind", kind)
}

// NewFeedbackSubmittedEvent: feedback was sent. rating 1-5 (0 = none), reason
// a short token (or ""), text the free text (or ""; redacted server-side; the
// has_text flag is derived).
func NewFeedbackSubmittedEvent(rating int, reason string, text string) *ClientEvent {
	event := newClientEvent(EventFeedbackSubmitted).setString("reason", reason)
	if 1 <= rating && rating <= 5 {
		event.setInt("rating", int64(rating))
	}
	text = strings.TrimSpace(text)
	event.setBool("has_text", text != "")
	if text != "" {
		event.props["text"] = text
	}
	return event
}

// NewSignupOptoutChangedEvent: the sign-up form's product-updates line changed.
func NewSignupOptoutChangedEvent(productUpdates bool) *ClientEvent {
	return newClientEvent(EventSignupOptoutChanged).setBool("product_updates", productUpdates)
}

// ClientEventList is a gomobile-safe list of events.
type ClientEventList struct {
	exportedList[*ClientEvent]
}

func NewClientEventList() *ClientEventList {
	return &ClientEventList{
		exportedList: *newExportedList[*ClientEvent](),
	}
}

// ParseClientEventsJson parses a json array of events (`[{name, at?, props?}]`,
// the web's path) and checks every name and prop key against the closed schema,
// so a page cannot send what the constructors cannot make. The whole batch is
// refused on the first bad event.
func ParseClientEventsJson(eventsJson string) (*ClientEventList, error) {
	var events []*ClientEvent
	if err := json.Unmarshal([]byte(eventsJson), &events); err != nil {
		return nil, err
	}
	list := NewClientEventList()
	for i, event := range events {
		if event == nil {
			return nil, fmt.Errorf("event %d: missing", i)
		}
		if err := checkClientEvent(event); err != nil {
			return nil, fmt.Errorf("event %d: %w", i, err)
		}
		if event.At == "" {
			event.At = time.Now().UTC().Format(time.RFC3339Nano)
		}
		list.Add(event)
	}
	return list, nil
}

func checkClientEvent(event *ClientEvent) error {
	allowed, ok := clientEventPropKeys[event.Name]
	if !ok {
		return fmt.Errorf("unknown event name %q", event.Name)
	}
	for key := range event.props {
		found := false
		for _, a := range allowed {
			if a == key {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("event %q: unknown prop %q", event.Name, key)
		}
	}
	return nil
}

// ----- the endpoint -----

type ClientEventsSendArgs struct {
	Events *ClientEventList `json:"events"`
}

type ClientEventRejection struct {
	Index   int    `json:"index"`
	Message string `json:"message"`
}

type ClientEventRejectionList struct {
	exportedList[*ClientEventRejection]
}

func NewClientEventRejectionList() *ClientEventRejectionList {
	return &ClientEventRejectionList{
		exportedList: *newExportedList[*ClientEventRejection](),
	}
}

type ClientEventsSendResult struct {
	Accepted int `json:"accepted"`
	// schema refusals by batch index; final, never resent
	Rejected *ClientEventRejectionList `json:"rejected,omitempty"`
}

type ClientEventsSendCallback connect.ApiCallback[*ClientEventsSendResult]

// MaxClientEventsPerCall is the server's batch bound.
const MaxClientEventsPerCall = 200

// ClientEventsSend posts one batch (at most MaxClientEventsPerCall events).
// Apps use a ClientEventQueue instead of calling this directly.
func (self *Api) ClientEventsSend(args *ClientEventsSendArgs, callback ClientEventsSendCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/client/events", self.apiUrl),
			args,
			self.GetByJwt(),
			&ClientEventsSendResult{},
			callback,
		)
	})
}

// ----- the queue -----

const (
	// ClientEventFlushIntervalMillis is how often the queue sends on its own.
	ClientEventFlushIntervalMillis = 30000
	clientEventFlushInterval       = ClientEventFlushIntervalMillis * time.Millisecond
	// ClientEventMaxAttempts is how many failed calls an event survives.
	ClientEventMaxAttempts = 3
	// clientEventQueueCapacity bounds the pending events; the oldest are dropped
	// beyond it (a signed-out app can accumulate for a long time).
	clientEventQueueCapacity = 2000
	clientEventQueueFile     = ".client_events.json"
)

type queuedClientEvent struct {
	Event    *ClientEvent `json:"event"`
	Attempts int          `json:"attempts"`
}

// clientEventSender is the queue's send seam (the Api in production).
type clientEventSender func(events []*ClientEvent, callback func(*ClientEventsSendResult, error))

// ClientEventQueue collects events and sends them in batches. One per app
// process: `NewClientEventQueue(networkSpace, platform, appVersion, locale)`,
// then Add from anywhere, Flush when the app goes to the background, Close on
// exit. Events are held (persisted) while there is no signed-in jwt.
type ClientEventQueue struct {
	ctx    context.Context
	cancel context.CancelFunc

	send      clientEventSender
	hasJwt    func() bool
	statePath string

	mutex      sync.Mutex
	pending    []*queuedClientEvent
	platform   string
	appVersion string
	locale     string
	session    string
	sending    bool

	flushTrigger chan struct{}
	done         chan struct{}
}

// NewClientEventQueue makes the queue for a network space (its Api sends, its
// local state dir persists). platform is one of the EventPlatform* values.
func NewClientEventQueue(networkSpace *NetworkSpace, platform string, appVersion string, locale string) *ClientEventQueue {
	api := networkSpace.GetApi()
	statePath := ""
	if asyncLocalState := networkSpace.GetAsyncLocalState(); asyncLocalState != nil {
		if localState := asyncLocalState.GetLocalState(); localState != nil {
			statePath = filepath.Join(localState.localStorageDir, clientEventQueueFile)
		}
	}
	return newClientEventQueue(
		networkSpace.ctx,
		func(events []*ClientEvent, callback func(*ClientEventsSendResult, error)) {
			list := NewClientEventList()
			list.addAll(events...)
			api.ClientEventsSend(&ClientEventsSendArgs{Events: list}, connect.NewApiCallback[*ClientEventsSendResult](callback))
		},
		func() bool { return api.GetByJwt() != "" },
		statePath,
		platform,
		appVersion,
		locale,
		clientEventFlushInterval,
	)
}

func newClientEventQueue(
	ctx context.Context,
	send clientEventSender,
	hasJwt func() bool,
	statePath string,
	platform string,
	appVersion string,
	locale string,
	flushInterval time.Duration,
) *ClientEventQueue {
	cancelCtx, cancel := context.WithCancel(ctx)
	q := &ClientEventQueue{
		ctx:          cancelCtx,
		cancel:       cancel,
		send:         send,
		hasJwt:       hasJwt,
		statePath:    statePath,
		platform:     platform,
		appVersion:   appVersion,
		locale:       locale,
		session:      newEventSessionId(),
		flushTrigger: make(chan struct{}, 1),
		done:         make(chan struct{}),
	}
	q.load()
	go connect.HandleError(func() {
		q.run(flushInterval)
	})
	return q
}

// newEventSessionId is a short random session id. Ids are time-prefixed
// (the leading hex is the millisecond clock), so the tail is what varies:
// two sessions rotated within the same millisecond must not collide.
func newEventSessionId() string {
	hex := strings.ReplaceAll(NewId().String(), "-", "")
	return "s_" + hex[len(hex)-16:]
}

// SetLocale updates the locale stamped on new events.
func (self *ClientEventQueue) SetLocale(locale string) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.locale = locale
}

// SetAppVersion updates the app version stamped on new events.
func (self *ClientEventQueue) SetAppVersion(appVersion string) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.appVersion = appVersion
}

// NewSession starts a new app session id (call when the app returns to the
// foreground after a long pause).
func (self *ClientEventQueue) NewSession() {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.session = newEventSessionId()
}

// GetSession is the current app session id stamped on events.
func (self *ClientEventQueue) GetSession() string {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.session
}

// Add queues one event (made by a New*Event constructor).
func (self *ClientEventQueue) Add(event *ClientEvent) {
	if event == nil || event.Name == "" {
		return
	}
	self.mutex.Lock()
	if event.Platform == "" {
		event.Platform = self.platform
	}
	if event.AppVersion == "" {
		event.AppVersion = self.appVersion
	}
	if event.Locale == "" {
		event.Locale = self.locale
	}
	if event.Session == "" {
		event.Session = self.session
	}
	if event.At == "" {
		event.At = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if event.props == nil {
		event.props = map[string]any{}
	}
	self.pending = append(self.pending, &queuedClientEvent{Event: event})
	if clientEventQueueCapacity < len(self.pending) {
		self.pending = self.pending[len(self.pending)-clientEventQueueCapacity:]
	}
	self.saveLocked()
	self.mutex.Unlock()
}

// AddAll queues every event in the list.
func (self *ClientEventQueue) AddAll(events *ClientEventList) {
	if events == nil {
		return
	}
	for _, event := range events.getAll() {
		self.Add(event)
	}
}

// PendingCount is the number of events waiting to be sent.
func (self *ClientEventQueue) PendingCount() int {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return len(self.pending)
}

// Flush sends the pending events now (asynchronously). Call it when the app
// goes to the background.
func (self *ClientEventQueue) Flush() {
	select {
	case self.flushTrigger <- struct{}{}:
	default:
	}
}

// FlushAndWait sends the pending events and waits (bounded by timeoutMillis)
// for the send to finish; for tests and app shutdown.
func (self *ClientEventQueue) FlushAndWait(timeoutMillis int64) {
	deadline := time.Now().Add(time.Duration(timeoutMillis) * time.Millisecond)
	for {
		self.flushOnce()
		self.mutex.Lock()
		remaining := len(self.pending)
		self.mutex.Unlock()
		if remaining == 0 || !time.Now().Before(deadline) {
			return
		}
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Close stops the queue after a final flush. The pending events stay on disk.
func (self *ClientEventQueue) Close() {
	self.flushOnce()
	self.cancel()
	<-self.done
}

func (self *ClientEventQueue) run(flushInterval time.Duration) {
	defer close(self.done)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-ticker.C:
			self.flushOnce()
		case <-self.flushTrigger:
			self.flushOnce()
		}
	}
}

// flushOnce sends one batch synchronously (at most MaxClientEventsPerCall) and
// applies the outcome: success removes the batch; a call failure counts an
// attempt on every event of the batch and drops the ones past the limit.
func (self *ClientEventQueue) flushOnce() {
	self.mutex.Lock()
	if self.sending || len(self.pending) == 0 || self.hasJwt == nil || !self.hasJwt() {
		self.mutex.Unlock()
		return
	}
	n := len(self.pending)
	if MaxClientEventsPerCall < n {
		n = MaxClientEventsPerCall
	}
	batch := make([]*queuedClientEvent, n)
	copy(batch, self.pending[:n])
	events := make([]*ClientEvent, n)
	for i, q := range batch {
		events[i] = q.Event
	}
	self.sending = true
	self.mutex.Unlock()

	resultChan := make(chan error, 1)
	self.send(events, func(result *ClientEventsSendResult, err error) {
		if err == nil && result == nil {
			err = errors.New("no result")
		}
		resultChan <- err
	})
	var err error
	select {
	case err = <-resultChan:
	case <-self.ctx.Done():
		err = self.ctx.Err()
	case <-time.After(60 * time.Second):
		err = errors.New("timeout")
	}

	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.sending = false
	if err == nil {
		// the batch is the head of pending, unchanged while sending
		self.pending = self.pending[len(batch):]
	} else {
		kept := self.pending[len(batch):]
		retry := make([]*queuedClientEvent, 0, len(batch))
		for _, q := range batch {
			q.Attempts += 1
			if q.Attempts < ClientEventMaxAttempts {
				retry = append(retry, q)
			}
		}
		self.pending = append(retry, kept...)
	}
	self.saveLocked()
}

// load reads the persisted queue; a corrupt file is discarded.
func (self *ClientEventQueue) load() {
	if self.statePath == "" {
		return
	}
	data, err := os.ReadFile(self.statePath)
	if err != nil {
		return
	}
	var pending []*queuedClientEvent
	if err := json.Unmarshal(data, &pending); err != nil {
		return
	}
	self.mutex.Lock()
	defer self.mutex.Unlock()
	for _, q := range pending {
		if q != nil && q.Event != nil && q.Event.Name != "" {
			self.pending = append(self.pending, q)
		}
	}
}

func (self *ClientEventQueue) saveLocked() {
	if self.statePath == "" {
		return
	}
	if len(self.pending) == 0 {
		os.Remove(self.statePath)
		return
	}
	data, err := json.Marshal(self.pending)
	if err != nil {
		return
	}
	_ = os.WriteFile(self.statePath, data, LocalStorageFilePermissions)
}
