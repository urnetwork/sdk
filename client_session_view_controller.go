//go:build !ios_extension

// Shared session inventory/action state for native, web, and API-only hosts.
//
// A network's list can be long: every hosted proxy, every client an API key
// created and every paired device holds its own session (server owner
// decision 1 of 2026-11-18), so a Pro network may list thousands. A reload
// reads the whole list over its pages (GET /network/sessions, then ?cursor=
// with each page's next_cursor), within the settings' bounds, and the
// controller presents it the way the Sessions screen shows it:
//   - Sessions: the device sign-ins (sign-ins and code sign-ins), the current
//     session first, a page at a time (SessionsHasMore, ShowMoreSessions with
//     ClientSessionGroupSignIns);
//   - Groups: then one group per kind of client session, in a fixed order,
//     each collapsed to its count until the screen opens it
//     (SetGroupExpanded), and an open group shows its rows a page at a time.
//
// The whole list stays in the controller. A snapshot carries only the rows on
// screen, so a long list is not copied for every listener and publication.
//
// The session's kind decides its place. A server that names it (kind signin,
// code or client, and client_kind) is read as it says; an older server's
// kind is the way the session began, which says the same thing
// (sessionCategory, sessionClientKind).
package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// Tunables of a session controller (connect/CODESTYLE.md, tunables live in
// settings). Tests change them before Start.
type clientSessionViewControllerSettings struct {
	// How often a visible screen reloads the list. 30 s keeps the last-use
	// lines current without a reload a second for each open screen.
	// Non-positive never polls.
	pollInterval time.Duration
	// How often a pending action asks for its operation's outcome. 2 s.
	// Must be positive.
	recoveryInterval time.Duration
	// The least time from one reload to the next that a session change hint
	// starts. A network that mints sessions for its hosted proxies changes its
	// list many times a minute, with a hint for each, and the reload of a long
	// list is many pages. 10 s. Non-positive reloads at each hint.
	hintRefreshInterval time.Duration
	// The most sessions one reload reads over its pages. Past it the list is
	// truncated (ClientSessionSnapshot.Truncated) and its counts are lower
	// bounds. 10,000: twice the network session cap, which a network reaches
	// only with provider clients, which the cap does not count.
	maxSessions int
	// The most pages one reload reads. It also ends a server that repeats a
	// page. 200.
	maxPages int
	// The rows of a section shown at first, and the rows each Show more adds.
	// 50. Must be positive.
	pageSize int
}

func defaultClientSessionViewControllerSettings() *clientSessionViewControllerSettings {
	return &clientSessionViewControllerSettings{
		pollInterval:        30 * time.Second,
		recoveryInterval:    2 * time.Second,
		hintRefreshInterval: 10 * time.Second,
		maxSessions:         10000,
		maxPages:            200,
		pageSize:            50,
	}
}

const (
	// Keep these untyped so gomobile exports them on every app platform.

	// The device sign-ins: sign-ins and code sign-ins, the current session
	// among them (ClientSessionSnapshot.Sessions). The value ShowMoreSessions
	// takes for them; never a group of Groups.
	ClientSessionGroupSignIns = "sign_ins"
	// hosted proxies' own sessions
	ClientSessionGroupHostedProxies = "hosted_proxies"
	// the own sessions of the clients that API keys created
	ClientSessionGroupApiClients = "api_clients"
	// paired (adopted) devices' own sessions
	ClientSessionGroupPairedDevices = "paired_devices"
	// any other client's own session: a client's legacy sign-in lineage, or a
	// kind of client this sdk does not know
	ClientSessionGroupOtherClients = "other_clients"
)

// One section of the Sessions screen below the device sign-ins: the sessions
// of one kind of client, collapsed to its count until the screen opens it.
type ClientSessionGroup struct {
	// ClientSessionGroupHostedProxies, ClientSessionGroupApiClients,
	// ClientSessionGroupPairedDevices or ClientSessionGroupOtherClients
	Kind string `json:"kind"`
	// the group's sessions, a lower bound while the snapshot is Truncated
	Count int `json:"count"`
	// opened with SetGroupExpanded
	Expanded bool `json:"expanded"`
	// while Expanded, the rows to show in display order; empty while collapsed
	Sessions *NetworkSessionInfoList `json:"sessions"`
	// Expanded, with rows past Sessions: offer Show more (ShowMoreSessions)
	HasMore bool `json:"has_more"`
}
type ClientSessionGroupList struct {
	exportedList[*ClientSessionGroup]
}

func NewClientSessionGroupList() *ClientSessionGroupList {
	return &ClientSessionGroupList{exportedList: *newExportedList[*ClientSessionGroup]()}
}

// The kinds of session (server R-kinds) and of client session.
const (
	sessionCategorySignIn = "signin"
	sessionCategoryCode   = "code"
	sessionCategoryClient = "client"

	sessionClientKindHostedProxy = "hosted_proxy"
	sessionClientKindApiKey      = "api_key"
	sessionClientKindDevice      = "device"
	sessionClientKindLegacy      = "legacy"
)

// signin, code or client: the kind a server names (session_kind, or kind
// from a server that puts it there), else the one the way the session began
// implies. A server before the kinds lists a code sign-in as auth_code and a
// client's own session as api_key_client, legacy_proxy or device_adopt;
// every other way is a sign-in.
func sessionCategory(info *NetworkSessionInfo) string {
	switch info.SessionKind {
	case sessionCategorySignIn, sessionCategoryCode, sessionCategoryClient:
		return info.SessionKind
	}
	switch info.Kind {
	case sessionCategorySignIn, sessionCategoryCode, sessionCategoryClient:
		return info.Kind
	case "auth_code":
		return sessionCategoryCode
	case "api_key_client", "hosted_proxy", "legacy_proxy", "device_adopt":
		return sessionCategoryClient
	}
	if info.ClientKind != "" {
		return sessionCategoryClient
	}
	return sessionCategorySignIn
}

// The kind of client a client session belongs to: the client_kind a server
// names, else the one the way the session began implies.
func sessionClientKind(info *NetworkSessionInfo) string {
	if info.ClientKind != "" {
		return info.ClientKind
	}
	switch info.Kind {
	case "hosted_proxy", "legacy_proxy":
		return sessionClientKindHostedProxy
	case "api_key_client":
		return sessionClientKindApiKey
	case "device_adopt":
		return sessionClientKindDevice
	}
	return sessionClientKindLegacy
}

// The section of a session: ClientSessionGroupSignIns for the device sign-ins
// and the current session, else its client group.
func sessionGroupKind(info *NetworkSessionInfo) string {
	if info.Current || sessionCategory(info) != sessionCategoryClient {
		return ClientSessionGroupSignIns
	}
	switch sessionClientKind(info) {
	case sessionClientKindHostedProxy:
		return ClientSessionGroupHostedProxies
	case sessionClientKindApiKey:
		return ClientSessionGroupApiClients
	case sessionClientKindDevice:
		return ClientSessionGroupPairedDevices
	default:
		return ClientSessionGroupOtherClients
	}
}

// The client groups in display order.
func clientSessionGroupKinds() []string {
	return []string{
		ClientSessionGroupHostedProxies,
		ClientSessionGroupApiClients,
		ClientSessionGroupPairedDevices,
		ClientSessionGroupOtherClients,
	}
}

// Whether RevokeOtherSessions signs a session out: every session but the
// current one and the hosted proxies, which a sign-out of other sessions, a
// sign-out of all and a password reset keep (server owner decision 4 of
// 2026-11-18). One place, so a change of what the server keeps is one change.
func sessionSignedOutByOthers(info *NetworkSessionInfo) bool {
	if info.Current {
		return false
	}
	return sessionCategory(info) != sessionCategoryClient || sessionClientKind(info) != sessionClientKindHostedProxy
}

type ClientSessionError struct {
	Message        string `json:"message"`
	Retryable      bool   `json:"retryable"`
	SignInRequired bool   `json:"sign_in_required"`
	// with SignInRequired: the server confirmed this sign-in session was
	// revoked from another device (AuthLogoutCauseSessionRevoked). Never set
	// for a generic rejection or a sign-out of this session made here.
	SessionRevoked bool `json:"session_revoked"`
	Unsupported    bool `json:"unsupported"`
}
type ClientSessionAction struct {
	Status      string `json:"status"`
	State       string `json:"state"`
	target      networkRenewalTarget
	SessionId   *Id                 `json:"session_id"`
	OperationId *Id                 `json:"operation_id"`
	Loading     bool                `json:"loading"`
	Pending     bool                `json:"pending"`
	Error       *ClientSessionError `json:"error"`
}
type ClientSessionActionList struct {
	exportedList[*ClientSessionAction]
}

func NewClientSessionActionList() *ClientSessionActionList {
	return &ClientSessionActionList{exportedList: *newExportedList[*ClientSessionAction]()}
}

type ClientSessionSnapshot struct {
	// The device sign-ins to show, in display order: the current session
	// first, then the most recent use. Every session a client owns is in
	// Groups instead.
	Sessions *NetworkSessionInfoList `json:"sessions"`
	// All the device sign-ins, Sessions and the rest; a lower bound while
	// Truncated.
	SessionCount int `json:"session_count"`
	// More device sign-ins than Sessions: offer Show more
	// (ShowMoreSessions(ClientSessionGroupSignIns)).
	SessionsHasMore bool `json:"sessions_has_more"`
	// The client sessions, one group per kind of client in display order. A
	// kind without sessions has no group.
	Groups *ClientSessionGroupList `json:"groups"`
	// The sessions RevokeOtherSessions signs out: every listed session but the
	// current one and the hosted proxies, which it keeps. Offer the action
	// while there is a current session and this is positive. A lower bound
	// while Truncated.
	OtherSessionCount int `json:"other_session_count"`
	// The list was longer than a reload reads (the controller's settings), so
	// the counts are lower bounds.
	Truncated        bool                     `json:"truncated"`
	CurrentSessionId *Id                      `json:"current_session_id"`
	LegacyCoverage   string                   `json:"legacy_coverage"`
	Generation       string                   `json:"generation"`
	EventId          int64                    `json:"event_id"`
	Loaded           bool                     `json:"loaded"`
	Loading          bool                     `json:"loading"`
	Refreshing       bool                     `json:"refreshing"`
	Supported        bool                     `json:"supported"`
	BulkAction       *ClientSessionAction     `json:"bulk_action"`
	Actions          *ClientSessionActionList `json:"actions"`
	Error            *ClientSessionError      `json:"error"`
}
type ClientSessionListener interface {
	ClientSessionsChanged(snapshot *ClientSessionSnapshot)
}

// Snapshot copies own their nested typed metadata; callers cannot mutate state.
func cloneClientSessionSnapshot(value *ClientSessionSnapshot) *ClientSessionSnapshot {
	encoded, _ := json.Marshal(value)
	copy := &ClientSessionSnapshot{}
	_ = json.Unmarshal(encoded, copy)
	return copy
}

type ClientSessionViewController struct {
	ctx                      context.Context
	cancel                   context.CancelFunc
	api                      *Api
	device                   Device
	settings                 *clientSessionViewControllerSettings
	stateLock                sync.Mutex
	state                    *ClientSessionSnapshot
	listeners                *connect.CallbackList[ClientSessionListener]
	started, closed, visible bool
	credentialGeneration     uint64
	sequence                 uint64
	mutation                 uint64
	// the whole list of the last reload, in the server's order
	sessions []*NetworkSessionInfo
	// the last reload stopped at the settings' bounds
	truncated bool
	// the rows each section shows, by its kind; a client group is open while
	// it has an entry
	shownSessionCounts map[string]int
	// when the last reload started, for the hint spacing
	lastRefreshTime     time.Time
	requests            chan struct{}
	hints               chan struct{}
	lifecycle           chan struct{}
	subscription        Sub
	testingBeforeSubmit func()
	testingAfterAction  func()
}

func newClientSessionViewController(ctx context.Context, device Device) *ClientSessionViewController {
	return NewClientSessionViewControllerWithDevice(ctx, device)
}
func NewClientSessionViewControllerWithDevice(ctx context.Context, device Device) *ClientSessionViewController {
	vc := NewClientSessionViewControllerWithApi(ctx, device.GetApi())
	vc.device = device
	return vc
}
func NewClientSessionViewControllerWithApi(ctx context.Context, api *Api) *ClientSessionViewController {
	ctx, cancel := context.WithCancel(ctx)
	return &ClientSessionViewController{
		ctx:                  ctx,
		cancel:               cancel,
		api:                  api,
		settings:             defaultClientSessionViewControllerSettings(),
		credentialGeneration: api.sessionCredentialGeneration(),
		state:                emptyClientSessionSnapshot(),
		shownSessionCounts:   map[string]int{},
		listeners:            connect.NewCallbackList[ClientSessionListener](),
		requests:             make(chan struct{}, 1),
		hints:                make(chan struct{}, 1),
		lifecycle:            make(chan struct{}, 1),
	}
}
func emptyClientSessionSnapshot() *ClientSessionSnapshot {
	return &ClientSessionSnapshot{Sessions: NewNetworkSessionInfoList(), Groups: NewClientSessionGroupList(), Actions: NewClientSessionActionList(), LegacyCoverage: "partial", Supported: true}
}

// Lays the whole list out as the screen shows it (the file header): the shown
// device sign-ins, the client groups with their counts and open rows, and the
// counts the actions need. Callers hold stateLock.
func (self *ClientSessionViewController) presentWithLock() {
	sectionSessions := map[string][]*NetworkSessionInfo{}
	others := 0
	for _, info := range self.sessions {
		kind := sessionGroupKind(info)
		sectionSessions[kind] = append(sectionSessions[kind], info)
		if sessionSignedOutByOthers(info) {
			others++
		}
	}
	shown := func(kind string) int {
		if count, ok := self.shownSessionCounts[kind]; ok {
			return count
		}
		return self.settings.pageSize
	}
	signIns := sectionSessions[ClientSessionGroupSignIns]
	signInsShown := min(shown(ClientSessionGroupSignIns), len(signIns))
	self.state.Sessions = NewNetworkSessionInfoList()
	self.state.Sessions.addAll(signIns[:signInsShown]...)
	self.state.SessionCount = len(signIns)
	self.state.SessionsHasMore = signInsShown < len(signIns)
	self.state.Groups = NewClientSessionGroupList()
	for _, kind := range clientSessionGroupKinds() {
		sessions := sectionSessions[kind]
		if len(sessions) == 0 {
			continue
		}
		group := &ClientSessionGroup{Kind: kind, Count: len(sessions), Sessions: NewNetworkSessionInfoList()}
		if count, ok := self.shownSessionCounts[kind]; ok {
			group.Expanded = true
			count = min(count, len(sessions))
			group.Sessions.addAll(sessions[:count]...)
			group.HasMore = count < len(sessions)
		}
		self.state.Groups.Add(group)
	}
	self.state.OtherSessionCount = others
	self.state.Truncated = self.truncated
}

// Opens a client group to its first page of rows, or closes it. Other kinds,
// ClientSessionGroupSignIns among them, are ignored. Presentation only: the
// list is not read again.
func (self *ClientSessionViewController) SetGroupExpanded(kind string, expanded bool) {
	if kind == ClientSessionGroupSignIns || !isClientSessionGroupKind(kind) {
		return
	}
	changed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return false
		}
		_, open := self.shownSessionCounts[kind]
		if open == expanded {
			return false
		}
		if expanded {
			self.shownSessionCounts[kind] = self.settings.pageSize
		} else {
			delete(self.shownSessionCounts, kind)
		}
		self.presentWithLock()
		return true
	}()
	if changed {
		self.publish()
	}
}

// Shows another page of a section's rows: the device sign-ins
// (ClientSessionGroupSignIns) or an open client group. Presentation only.
func (self *ClientSessionViewController) ShowMoreSessions(kind string) {
	if kind != ClientSessionGroupSignIns && !isClientSessionGroupKind(kind) {
		return
	}
	changed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return false
		}
		count, ok := self.shownSessionCounts[kind]
		if !ok {
			if kind != ClientSessionGroupSignIns {
				// a closed group opens with SetGroupExpanded
				return false
			}
			count = self.settings.pageSize
		}
		self.shownSessionCounts[kind] = count + self.settings.pageSize
		self.presentWithLock()
		return true
	}()
	if changed {
		self.publish()
	}
}

func isClientSessionGroupKind(kind string) bool {
	for _, groupKind := range clientSessionGroupKinds() {
		if groupKind == kind {
			return true
		}
	}
	return false
}
func (self *ClientSessionViewController) Start() {
	self.stateLock.Lock()
	if self.closed || self.started {
		self.stateLock.Unlock()
		return
	}
	self.started = true
	self.subscription = self.api.AddNetworkSessionsChangeListener(self)
	self.stateLock.Unlock()
	go self.run()
	self.Refresh()
}
func (self *ClientSessionViewController) Stop() { self.SetVisible(false) }
func (self *ClientSessionViewController) Close() {
	self.stateLock.Lock()
	if self.closed {
		self.stateLock.Unlock()
		return
	}
	self.closed = true
	self.sequence++
	subscription := self.subscription
	self.subscription = nil
	self.stateLock.Unlock()
	self.cancel()
	if subscription != nil {
		subscription.Close()
	}
}
func (self *ClientSessionViewController) SetVisible(visible bool) {
	self.stateLock.Lock()
	changed := self.visible != visible
	self.visible = visible
	self.stateLock.Unlock()
	select {
	case self.lifecycle <- struct{}{}:
	default:
	}
	if visible && changed {
		self.Refresh()
	}
}
func (self *ClientSessionViewController) SetForeground(foreground bool) {
	if foreground {
		self.Refresh()
	}
}
func (self *ClientSessionViewController) Refresh() {
	self.stateLock.Lock()
	closed := self.closed
	self.stateLock.Unlock()
	if closed {
		return
	}
	select {
	case self.requests <- struct{}{}:
	default:
	}
}
func (self *ClientSessionViewController) GetSnapshot() *ClientSessionSnapshot {
	generation := self.api.sessionCredentialGeneration()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if generation != self.credentialGeneration {
		return emptyClientSessionSnapshot()
	}
	return cloneClientSessionSnapshot(self.state)
}
func (self *ClientSessionViewController) AddClientSessionListener(listener ClientSessionListener) Sub {
	id := self.listeners.Add(listener)
	return newSub(func() { self.listeners.Remove(id) })
}
func (self *ClientSessionViewController) publish() {
	snapshot := self.GetSnapshot()
	for _, listener := range self.listeners.Get() {
		copy := cloneClientSessionSnapshot(snapshot)
		connect.HandleError(func() { listener.ClientSessionsChanged(copy) })
	}
}

// A hint of a newer list reloads it while the screen is visible, no sooner
// than the settings' hint spacing after the last reload (hintRefreshDelay). A
// hint while the screen is hidden is dropped: a controller kept open in the
// background would otherwise read a long list again at every change, and
// SetVisible(true) reloads it when the screen shows again.
func (self *ClientSessionViewController) NetworkSessionsChanged(revision *NetworkSessionsRevision) {
	self.stateLock.Lock()
	refresh := !self.closed && self.visible && (revision == nil || revision.Generation == "" || revision.Generation != self.state.Generation || revision.EventId > self.state.EventId)
	self.stateLock.Unlock()
	if refresh {
		select {
		case self.hints <- struct{}{}:
		default:
		}
	}
}

// How long a hint waits for its reload: until the settings' hint spacing has
// passed since the last reload started, and not at all when none has.
func (self *ClientSessionViewController) hintRefreshDelay(now time.Time) time.Duration {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.lastRefreshTime.IsZero() || self.settings.hintRefreshInterval <= 0 {
		return 0
	}
	return max(0, self.lastRefreshTime.Add(self.settings.hintRefreshInterval).Sub(now))
}
func (self *ClientSessionViewController) run() {
	recovery := time.NewTicker(self.settings.recoveryInterval)
	defer recovery.Stop()
	var timer *time.Timer
	var tick <-chan time.Time
	reset := func() {
		if timer != nil {
			timer.Stop()
		}
		self.stateLock.Lock()
		visible := self.visible
		self.stateLock.Unlock()
		tick = nil
		if visible && 0 < self.settings.pollInterval {
			timer = time.NewTimer(self.settings.pollInterval)
			tick = timer.C
		}
	}
	reset()
	// a hint waiting for its reload (hintRefreshDelay)
	var hintTimer *time.Timer
	var hintTick <-chan time.Time
	stopHint := func() {
		if hintTimer != nil {
			hintTimer.Stop()
		}
		hintTimer = nil
		hintTick = nil
	}
	defer func() {
		if timer != nil {
			timer.Stop()
		}
		stopHint()
	}()
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-self.hints:
			if hintTick != nil {
				// already waiting: that reload reads this change too
				continue
			}
			if delay := self.hintRefreshDelay(time.Now()); 0 < delay {
				hintTimer = time.NewTimer(delay)
				hintTick = hintTimer.C
				continue
			}
			self.refresh()
			self.recoverPending()
		case <-hintTick:
			stopHint()
			self.stateLock.Lock()
			visible := self.visible
			self.stateLock.Unlock()
			if !visible {
				// hidden while it waited: SetVisible(true) reloads
				continue
			}
			self.refresh()
			self.recoverPending()
		case <-recovery.C:
			self.recoverPending()
		case <-self.lifecycle:
			reset()
		case <-tick:
			self.Refresh()
			reset()
		case <-self.requests:
			// a waiting hint came before this reload, which reads its change
			stopHint()
			self.refresh()
			self.recoverPending()
		}
	}
}

// The error of a request that sent the credential sentByJwt. Once a confirmed
// rejection has cleared the network credential, the controller's requests
// fail before they are sent; they report that rejection as sign-in required,
// with its trusted cause, until a new login.
func (self *ClientSessionViewController) clientSessionError(err error, listing bool, sentByJwt string) *ClientSessionError {
	if err == nil {
		return nil
	}
	result := &ClientSessionError{Message: "Session request could not be completed.", Retryable: true}
	var status *connect.HttpStatusError
	if errors.As(err, &status) {
		result.SignInRequired = status.StatusCode == 401
		result.SessionRevoked = result.SignInRequired && self.api.trustedRejectionCause(sentByJwt, err) == AuthLogoutCauseSessionRevoked
		result.Unsupported = listing && status.StatusCode == 404
		result.Retryable = status.StatusCode == 503 || status.StatusCode == 429 || status.StatusCode == 408 || status.StatusCode == 409
	} else if errors.Is(err, ErrNetworkCredentialRequired) {
		if rejected, cause := self.api.sessionSignInRejection(); rejected {
			result.SignInRequired = true
			result.SessionRevoked = cause == AuthLogoutCauseSessionRevoked
			result.Retryable = false
		}
	}
	if result.SessionRevoked {
		result.Message = "This session was signed out from another device."
	} else if result.SignInRequired {
		result.Message = "Sign-in required."
	}
	return result
}

// One reload's read of the list: every page's rows in the server's order, the
// first page's current session, coverage and revision, and whether the read
// stopped at the settings' bounds.
type clientSessionListing struct {
	first     *NetworkSessionsResult
	sessions  []*NetworkSessionInfo
	truncated bool
}

// Reads the list over its pages, following each page's next_cursor, until
// the last page or the settings' bounds. A row a later page repeats is read
// once. Also returns the index of the page that failed, so only the first
// page's 404 reads as a server without the list.
func (self *ClientSessionViewController) loadSessions(target networkRenewalTarget) (*clientSessionListing, int, error) {
	listing := &clientSessionListing{}
	seenSessionIds := map[string]bool{}
	cursor := ""
	for page := 0; ; page++ {
		result, err := self.api.networkSessionsPageFor(self.ctx, target, cursor)
		if err != nil {
			return nil, page, err
		}
		if page == 0 {
			listing.first = result
		}
		for _, info := range result.Sessions.values {
			if info == nil {
				continue
			}
			if info.SessionId != nil {
				sessionId := info.SessionId.String()
				if seenSessionIds[sessionId] {
					continue
				}
				seenSessionIds[sessionId] = true
			}
			listing.sessions = append(listing.sessions, info)
		}
		if result.NextCursor == "" {
			return listing, page, nil
		}
		if result.NextCursor == cursor || self.settings.maxPages <= page+1 || self.settings.maxSessions <= len(listing.sessions) {
			listing.truncated = true
			return listing, page, nil
		}
		cursor = result.NextCursor
	}
}
func (self *ClientSessionViewController) refresh() {
	target := self.api.captureNetworkTarget()
	generation := target.generation
	self.stateLock.Lock()
	if self.closed {
		self.stateLock.Unlock()
		return
	}
	if generation != self.credentialGeneration {
		self.state = emptyClientSessionSnapshot()
		self.sessions = nil
		self.truncated = false
		self.shownSessionCounts = map[string]int{}
		self.credentialGeneration = generation
		self.mutation++
	}
	self.sequence++
	sequence, mutation := self.sequence, self.mutation
	self.lastRefreshTime = time.Now()
	self.state.Loading = !self.state.Loaded
	self.state.Refreshing = self.state.Loaded
	self.stateLock.Unlock()
	self.publish()
	listing, failedPage, err := self.loadSessions(target)
	requestError := self.clientSessionError(err, failedPage == 0, target.byJwt)
	current := self.api.sessionCredentialGeneration()
	self.stateLock.Lock()
	if self.closed || sequence != self.sequence || mutation != self.mutation || generation != current {
		closed := self.closed
		self.stateLock.Unlock()
		if !closed {
			self.Refresh()
		}
		return
	}
	self.state.Loading = false
	self.state.Refreshing = false
	self.state.Error = requestError
	if err == nil {
		self.sessions = listing.sessions
		self.truncated = listing.truncated
		self.state.CurrentSessionId = listing.first.CurrentSessionId
		self.state.LegacyCoverage = listing.first.LegacyCoverage
		self.state.Generation = listing.first.Generation
		self.state.EventId = listing.first.EventId
		self.state.Loaded = true
		self.state.Supported = true
		self.presentWithLock()
	} else if self.state.Error.Unsupported {
		self.state.Supported = false
	}
	self.stateLock.Unlock()
	self.publish()
}
func (self *ClientSessionViewController) RevokeSession(sessionId *Id) {
	if sessionId == nil {
		return
	}
	self.revoke(sessionId, false)
}
func (self *ClientSessionViewController) RevokeOtherSessions() { self.revoke(nil, true) }

// Both legacy status-only replies and operation-state replies confirm success.
func clientSessionOperationEnforced(status, state string) bool {
	return status == "revoked" || status == "already_revoked" || state == "enforced" || state == "complete"
}

// A known outcome ends an operation; a transport failure or missing receipt does not.
func clientSessionOperationTerminal(status, state string) bool {
	return clientSessionOperationEnforced(status, state) || state == "cancelled" || state == "failed"
}

// Detached actions cannot publish into a replacement action or credential generation.
func (self *ClientSessionViewController) actionCurrentWithLock(action *ClientSessionAction) bool {
	if self.state.BulkAction == action {
		return true
	}
	for _, current := range self.state.Actions.values {
		if current == action {
			return true
		}
	}
	return false
}

func (self *ClientSessionViewController) revoke(sessionId *Id, bulk bool) {
	target := self.api.captureNetworkTarget()
	generation := target.generation
	tracked := target.byJwt != ""
	self.stateLock.Lock()
	if self.closed || generation != self.credentialGeneration || !tracked {
		self.stateLock.Unlock()
		self.Refresh()
		return
	}
	var action *ClientSessionAction
	actionIndex := -1
	if bulk {
		action = self.state.BulkAction
	} else {
		for i, value := range self.state.Actions.values {
			if value.SessionId != nil && value.SessionId.Cmp(sessionId) == 0 {
				action = value
				actionIndex = i
				break
			}
		}
	}
	if action != nil && (action.Loading || action.Pending) {
		self.stateLock.Unlock()
		return
	}
	if action == nil || clientSessionOperationTerminal(action.Status, action.State) {
		// Retries keep an uncertain operation's identity. A new action after a
		// known outcome must not replay the server's cached receipt.
		action = &ClientSessionAction{SessionId: sessionId, OperationId: newId(connect.NewId()), target: target}
		if bulk {
			self.state.BulkAction = action
		} else if 0 <= actionIndex {
			self.state.Actions.values[actionIndex] = action
		} else {
			self.state.Actions.Add(action)
		}
	}
	action.Loading = true
	action.Error = nil
	self.mutation++
	operationId := action.OperationId
	currentSession := self.state.CurrentSessionId
	self.stateLock.Unlock()
	self.publish()
	go func() {
		if self.testingAfterAction != nil {
			defer self.testingAfterAction()
		}
		if self.testingBeforeSubmit != nil {
			self.testingBeforeSubmit()
		}
		var result *SessionOperationResult
		var err error
		if bulk {
			result, err = self.api.sessionOperationFor(self.ctx, "/network/revoke-other-sessions", &RevokeOtherNetworkSessionsArgs{OperationId: operationId}, target)
		} else {
			result, err = self.api.sessionOperationFor(self.ctx, "/network/revoke-session", &RevokeNetworkSessionArgs{OperationId: operationId, SessionId: sessionId}, target)
		}
		requestError := self.clientSessionError(err, false, target.byJwt)
		current := self.api.sessionCredentialGeneration()
		self.stateLock.Lock()
		if self.closed || generation != current || generation != self.credentialGeneration || !self.actionCurrentWithLock(action) {
			self.stateLock.Unlock()
			return
		}
		action.Loading = false
		action.Error = requestError
		self.mutation++
		enforced := err == nil && clientSessionOperationEnforced(result.Status, result.State)
		terminalFailure := err == nil && (result.State == "cancelled" || result.State == "failed")
		if result != nil {
			action.Status = result.Status
			action.State = result.State
		}
		if terminalFailure {
			action.Error = &ClientSessionError{Message: "Session revocation was not applied.", Retryable: false}
		}
		action.Pending = !enforced && !terminalFailure && (err == nil || action.Error.Retryable)
		if enforced {
			// until the reload below: a sign-out of other sessions keeps the
			// current session and the hosted proxies
			remaining := []*NetworkSessionInfo{}
			for _, item := range self.sessions {
				if bulk && !sessionSignedOutByOthers(item) || !bulk && (item.SessionId == nil || item.SessionId.Cmp(sessionId) != 0) {
					remaining = append(remaining, item)
				}
			}
			self.sessions = remaining
			self.presentWithLock()
		}
		self.stateLock.Unlock()
		self.publish()
		if enforced && !bulk && tracked && currentSession != nil && currentSession.Cmp(sessionId) == 0 {
			if current, ok := self.api.currentNetworkTarget(target); ok {
				// this session was signed out here: no cause, and the
				// whole app signs out, as the confirmation said
				self.api.signOutNetworkCredential(current)
			}
			return
		}
		self.Refresh()
	}()
}
func (self *ClientSessionViewController) recoverPending() {
	self.stateLock.Lock()
	actions := append([]*ClientSessionAction{}, self.state.Actions.values...)
	if self.state.BulkAction != nil {
		actions = append(actions, self.state.BulkAction)
	}
	generation := self.credentialGeneration
	self.stateLock.Unlock()
	for _, action := range actions {
		self.stateLock.Lock()
		pending := !self.closed && generation == self.credentialGeneration && self.actionCurrentWithLock(action) && action.Pending
		self.stateLock.Unlock()
		if !pending {
			continue
		}
		result, err := self.api.sessionOperationFor(self.ctx, "/network/session-operations/"+action.OperationId.String(), nil, action.target)
		requestError := self.clientSessionError(err, false, action.target.byJwt)
		current := self.api.sessionCredentialGeneration()
		self.stateLock.Lock()
		if self.closed || generation != current || generation != self.credentialGeneration {
			self.stateLock.Unlock()
			return
		}
		if !self.actionCurrentWithLock(action) || !action.Pending {
			self.stateLock.Unlock()
			continue
		}
		previousError := action.Error
		previousStatus, previousState, previousPending := action.Status, action.State, action.Pending
		action.Error = requestError
		if result != nil {
			action.Status = result.Status
			action.State = result.State
		}
		if err == nil && (result.State == "cancelled" || result.State == "failed") {
			action.Error = &ClientSessionError{Message: "Session revocation was not applied."}
		}
		var status *connect.HttpStatusError
		notFound := errors.As(err, &status) && status.StatusCode == 404
		if notFound {
			action.Error.Retryable = true
		}
		terminal := err == nil && clientSessionOperationTerminal(result.Status, result.State)
		if terminal || notFound {
			action.Pending = false
		}
		errorChanged := (previousError == nil) != (action.Error == nil)
		if previousError != nil && action.Error != nil {
			errorChanged = *previousError != *action.Error
		}
		changed := errorChanged || previousStatus != action.Status || previousState != action.State || previousPending != action.Pending
		if changed {
			self.mutation++
			self.stateLock.Unlock()
			if terminal {
				self.Refresh()
			}
			self.publish()
		} else {
			self.stateLock.Unlock()
		}
	}
}

// API-only binding entry point; the API owns cancellation and credentials.
func (self *Api) OpenClientSessionViewController() *ClientSessionViewController {
	return NewClientSessionViewControllerWithApi(self.ctx, self)
}
