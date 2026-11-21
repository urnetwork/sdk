//go:build !ios_extension

// The backend seam behind sdk.Sidecar (D-SDK-2; SDK-M8-02) and the remote backend, which forwards to `ur daemon`'s
// control mode (§5.8) and holds no key, token or state of its own.
//
//	channel   one control session: JSON-RPC 2.0 calls, one at a time, as the daemon answers them. On a desktop it is
//	          the root's DaemonClient on the instance's socket or a sandboxed app's app-group socket
//	          (sidecar_remote_dial.go); on js a host-supplied `ur native-host` port (sidecar_remote_js.go). This file
//	          links no sidecar package, so sdk.wasm shares it and stays under its gate (plan-SDK-small-readings #7)
//	events    a second session holds one long-poll of the daemon's events method open (QD-821), so every event kind of
//	          the core reaches the app in the daemon's order, state among them. The first call takes the ring's cursor,
//	          and status is read after it (subscribe, then read); a gap reads status and emits state, since events
//	          between were lost; a session that ends reads status at once and is dialed again after a backoff that
//	          doubles up to its cap. While the poll answers, its state events move the backend's state and the status
//	          poll stops; while it does not, the poll reads status every StatusPollInterval as before. On js the second
//	          session is the port itself, whose calls take turns
//	state     the daemon's, read by its status method or from its state events and never written here: locked,
//	          unenrolled, offline, online or revoked with its reason, as the daemon's core decided them (D-SDK-14).
//	          With no daemon answering the backend reads offline and starts nothing (R-SDK-9,
//	          plan-SDK-small-readings #9)
//	network   the one the pairing bound (pair's reply), else the daemon's only network, else its first; events of any
//	          other network go nowhere, and one naming a network the last status did not list reads status first
//	generation every status and event names its generation, and one older than the newest this backend took for the
//	          same sidecar_id is dropped, so an answer that crossed a re-pair cannot end the new pairing (D-SDK-14)
//	replies   only the fields this file names are read; nothing else of a reply reaches an app. A refusal's text that
//	          repeats the child's token is replaced before it is surfaced (I-7)
//
// The method names are their owners' (ur daemon's status, CORE-M8-02's pair, BRIDGE-M8-06's forwarding methods,
// QD-821's events), spelled here because the js leg cannot import them; sidecar_remote_test.go holds them equal.

package sdk

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// What a Sidecar's calls reach when its core runs in another process: the remote backend. The local backend's calls
// still read the core directly (sidecar.go); folding both behind this one interface is the review phase's.
type sidecarBackend interface {
	// The daemon's sidecar_id for this Sidecar's network, nil before it has one.
	sidecarId() *Id
	// The daemon's state and its reason (§18.5's State), as GetState names them; offline while no daemon answers.
	state() (state string, reason string)
	// The client generation the state belongs to.
	generation() int64
	// Reads the daemon's status now.
	refresh(ctx context.Context) error
	// §5.8's pairing: gives the daemon the child's token, and answers the network and generation it bound.
	pair(ctx context.Context, childJwt string) (*sidecarControlPairReply, error)
	// One control call for a wrapper, the paired network named where the method takes one. confirmed says a person
	// confirmed it in the app's UI (D-CORE-13).
	call(ctx context.Context, method string, params any, confirmed bool, result any) error
	// Ends the control session.
	close()
}

// The control methods the remote backend calls, as their owners named them.
const (
	sidecarControlMethodStatus            = "status"
	sidecarControlMethodPair              = "pair"
	sidecarControlMethodForwardingSetup   = "bridge.forwarding.setup"
	sidecarControlMethodForwardingWitness = "bridge.forwarding.witness"
	sidecarControlMethodEvents            = "events"
)

// The kind of the event that carries a state (§5.4's state kind).
const sidecarControlEventKindState = "state"

// §18.5's states, as the daemon names them.
const (
	sidecarRemoteStateLocked     = "locked"
	sidecarRemoteStateUnenrolled = "unenrolled"
	sidecarRemoteStateOffline    = "offline"
	sidecarRemoteStateOnline     = "online"
	sidecarRemoteStateRevoked    = "revoked"
)

// The one reason a revoked state carries: a SidecarRevoke the daemon's core verified (D-SDK-14).
const sidecarRemoteReasonSigned = "signed"

// What the backend falls to when the daemon answers revoked without its verified reason.
const sidecarRemoteReasonRefused = "refused"

// status's reply, as far as the backend reads it.
type sidecarControlStatus struct {
	State      string                        `json:"state"`
	Reason     string                        `json:"reason"`
	Generation int64                         `json:"generation"`
	Networks   []sidecarControlNetworkStatus `json:"networks"`
}

// One hosted network in status.
type sidecarControlNetworkStatus struct {
	NetworkId  *Id    `json:"network_id"`
	SidecarId  *Id    `json:"sidecar_id"`
	State      string `json:"state"`
	Reason     string `json:"reason"`
	Generation int64  `json:"generation"`
}

// pair's params: the child's token, which the app minted and gives up.
type sidecarControlPairParams struct {
	ChildJwt string `json:"child_jwt"`
}

// pair's reply: no token.
type sidecarControlPairReply struct {
	NetworkId  *Id   `json:"network_id"`
	Generation int64 `json:"generation"`
}

// bridge.forwarding.setup's params: the component's bridge.ForwardingKeys, encoded, public halves only.
type sidecarControlForwardingSetupParams struct {
	Network                string `json:"network,omitempty"`
	Keys                   []byte `json:"keys"`
	ConfirmAfterRevocation bool   `json:"confirm_after_revocation,omitempty"`
}

// bridge.forwarding.setup's reply: the bridge.ForwardingHandOff, encoded, for the app's VPN component alone.
type sidecarControlForwardingSetupReply struct {
	HandOff []byte `json:"hand_off"`
}

// A network-scoped call's params with nothing else.
type sidecarControlNetworkParams struct {
	Network string `json:"network,omitempty"`
}

// bridge.forwarding.witness's reply.
type sidecarControlForwardingWitnessReply struct {
	// The bridge.AuthPin, encoded.
	Pin       []byte `json:"pin"`
	Witnessed bool   `json:"witnessed"`
	Fork      bool   `json:"fork"`
	// Why a setup is due, empty when none is.
	SetupDue string `json:"setup_due,omitempty"`
}

// events's params (QD-821).
type sidecarControlEventsParams struct {
	// The cursor of the last reply taken, empty for none, which answers the current cursor at once.
	After string `json:"after,omitempty"`
	// How long the daemon waits for an event past After, in milliseconds; it caps the wait at its own bound.
	WaitMs int64 `json:"wait_ms,omitempty"`
}

// events's reply.
type sidecarControlEventsReply struct {
	Events []*sidecarControlEvent `json:"events"`
	// Where the next call starts.
	Cursor string `json:"cursor"`
	// The events past After are gone: status says where things stand.
	Gap bool `json:"gap,omitempty"`
}

// One event of the daemon's ring: the root's DaemonEvent, its kind, generation and what §5.4's SidecarEvent carries
// for its kind, with the network and sidecar that raised it. Times are unix milliseconds, zero for none.
type sidecarControlEvent struct {
	LoggedOut         bool                        `json:"logged_out,omitempty"`
	NetworkId         *Id                         `json:"network_id,omitempty"`
	OriginSidecarId   *Id                         `json:"origin_sidecar_id,omitempty"`
	Kind              string                      `json:"kind"`
	Generation        int64                       `json:"generation"`
	State             string                      `json:"state,omitempty"`
	Reason            string                      `json:"reason,omitempty"`
	GroupId           *Id                         `json:"group_id,omitempty"`
	SidecarId         *Id                         `json:"sidecar_id,omitempty"`
	RepoId            *Id                         `json:"repo_id,omitempty"`
	RefId             *Id                         `json:"ref_id,omitempty"`
	Path              string                      `json:"path,omitempty"`
	DropId            *Id                         `json:"drop_id,omitempty"`
	DropState         string                      `json:"drop_state,omitempty"`
	NotBeforeMs       int64                       `json:"not_before_ms,omitempty"`
	SearchAlarm       string                      `json:"search_alarm,omitempty"`
	RequestId         *Id                         `json:"request_id,omitempty"`
	Outcome           string                      `json:"outcome,omitempty"`
	SendId            *Id                         `json:"send_id,omitempty"`
	ApprovalId        *Id                         `json:"approval_id,omitempty"`
	HeadAlarm         string                      `json:"head_alarm,omitempty"`
	FirstEvidence     []byte                      `json:"first_evidence,omitempty"`
	SecondEvidence    []byte                      `json:"second_evidence,omitempty"`
	Erasure           *sidecarControlEventErasure `json:"erasure,omitempty"`
	EvsAlarm          string                      `json:"evs_alarm,omitempty"`
	EvsAlarmDetail    string                      `json:"evs_alarm_detail,omitempty"`
	Version           uint64                      `json:"version,omitempty"`
	PushId            *Id                         `json:"push_id,omitempty"`
	Count             int                         `json:"count,omitempty"`
	VerifiedSinceMs   int64                       `json:"verified_since_ms,omitempty"`
	RecordAlarm       string                      `json:"record_alarm,omitempty"`
	BridgeAlarm       string                      `json:"bridge_alarm,omitempty"`
	DnsAlarm          string                      `json:"dns_alarm,omitempty"`
	CtAlarm           string                      `json:"ct_alarm,omitempty"`
	MailReceiverAlarm string                      `json:"mail_receiver_alarm,omitempty"`
}

// An event's erasure: the root's DaemonEventErasure.
type sidecarControlEventErasure struct {
	ErasureId   *Id    `json:"erasure_id"`
	State       string `json:"state"`
	ObjectsLeft int64  `json:"objects_left,omitempty"`
	BytesLeft   int64  `json:"bytes_left,omitempty"`
	NextTryMs   int64  `json:"next_try_ms,omitempty"`
	FinishMs    int64  `json:"finish_ms,omitempty"`
}

// The bound event this ring event stands for, as the local backend's listener gets its core's: what the js leg
// delivers, and on the other legs the same values the dispatch makes from the core event (sidecar_remote.go).
func (self *sidecarControlEvent) boundEvent() *SidecarEvent {
	bound := &SidecarEvent{
		Kind:                self.Kind,
		Generation:          self.Generation,
		State:               self.State,
		Reason:              self.Reason,
		GroupId:             sidecarControlIdOf(self.GroupId),
		SidecarId:           sidecarControlIdOf(self.SidecarId),
		RepoId:              sidecarControlIdOf(self.RepoId),
		RefId:               sidecarControlIdOf(self.RefId),
		Path:                self.Path,
		DropId:              sidecarControlIdOf(self.DropId),
		DropState:           self.DropState,
		NotBeforeMillis:     self.NotBeforeMs,
		SearchAlarm:         self.SearchAlarm,
		RequestId:           sidecarControlIdOf(self.RequestId),
		Outcome:             self.Outcome,
		SendId:              sidecarControlIdOf(self.SendId),
		ApprovalId:          sidecarControlIdOf(self.ApprovalId),
		HeadAlarm:           self.HeadAlarm,
		FirstEvidence:       sidecarControlBytesOf(self.FirstEvidence),
		SecondEvidence:      sidecarControlBytesOf(self.SecondEvidence),
		EvsAlarm:            self.EvsAlarm,
		EvsAlarmDetail:      self.EvsAlarmDetail,
		Version:             int64(self.Version),
		PushId:              sidecarControlIdOf(self.PushId),
		Count:               int64(self.Count),
		VerifiedSinceMillis: self.VerifiedSinceMs,
		RecordAlarm:         self.RecordAlarm,
		BridgeAlarm:         self.BridgeAlarm,
		DnsAlarm:            self.DnsAlarm,
		CtAlarm:             self.CtAlarm,
		MailReceiverAlarm:   self.MailReceiverAlarm,
	}
	if erasure := self.Erasure; erasure != nil {
		bound.Erasure = &SidecarErasure{
			ErasureId:     sidecarControlIdOf(erasure.ErasureId),
			State:         erasure.State,
			ObjectsLeft:   erasure.ObjectsLeft,
			BytesLeft:     erasure.BytesLeft,
			NextTryMillis: erasure.NextTryMs,
			FinishMillis:  erasure.FinishMs,
		}
	}
	return bound
}

// A copy of id, nil for nil.
func sidecarControlIdOf(id *Id) *Id {
	if id == nil {
		return nil
	}
	return newId(id.id)
}

// A copy, nil for none.
func sidecarControlBytesOf(value []byte) []byte {
	if len(value) == 0 {
		return nil
	}
	return append([]byte{}, value...)
}

// Whether a and b name one id, nil naming none.
func sidecarControlSameId(a *Id, b *Id) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Cmp(b) == 0
}

// The daemon's code for a call that needs a person's confirmation and came without one (D-CORE-13).
const sidecarControlCodeConfirmationRequired = -32001

// The daemon's code for a method that ran past its request timeout. The root's DaemonRpcTimeout carries
// !ios && !android, so phones' builds, which link no daemon client, read the number here.
const sidecarControlCodeTimeout = -32003

// The daemon's refusal of one call: its JSON-RPC error.
type sidecarControlError struct {
	Code    int
	Message string
}

// The daemon's message, which it writes knowing a same-user agent may read it.
func (self *sidecarControlError) Error() string {
	return self.Message
}

var (
	// No daemon answers: the backend is offline and starts none (R-SDK-9).
	errSidecarRemoteOffline = errors.New("no ur daemon answers this instance's control socket")
	// The Sidecar closed.
	errSidecarRemoteClosed = errors.New("the remote-mode sidecar is closed")
	// A pairing the daemon answered without a network or a generation.
	errSidecarRemotePairReply = errors.New("the daemon answered the pairing with no binding")
	// A pairing's refusal whose text repeated the child's token.
	errSidecarRemotePairRefused = errors.New("the daemon refused the pairing")
)

// One control session.
type sidecarControlChannel interface {
	// Calls method and decodes its result into result when that is not nil. A daemon's refusal is a
	// *sidecarControlError; any other error means the session is gone.
	call(ctx context.Context, method string, params any, confirmed bool, result any) error
	close()
}

// Opens a control session, or fails when no daemon answers. It never starts one.
type sidecarControlDial func(ctx context.Context) (sidecarControlChannel, error)

// The remote backend's tunables.
type sidecarRemoteSettings struct {
	// How often the backend reads the daemon's status while the Sidecar is open.
	StatusPollInterval time.Duration
	// The bound of one control call, its dial included.
	CallTimeout time.Duration
	// The bound of a pairing, which binds on the server.
	PairTimeout time.Duration
	// js: the longest reply joined from a native host's chunks, the bound the root's DaemonClient reads a line to.
	AnswerMaxByteCount int64
	// js: how many events wait for the listeners before one is dropped.
	EventQueueSize int
	// How long one events call asks the daemon to wait for an event: 20 s, under the daemon's own cap of 25 s
	// (SidecarLocalSettings.EventWaitTimeout), so a poll answers in the daemon's time. The call's bound is this and
	// CallTimeout together.
	EventWaitTimeout time.Duration
	// The first wait before the events session is dialed or called again after it ended or was refused: 250 ms,
	// doubling at each failure in a row up to EventReconnectMaxTimeout, and back to this once a call answers.
	EventReconnectMinTimeout time.Duration
	// The longest such wait: 10 s, the status poll reading the daemon's state meanwhile.
	EventReconnectMaxTimeout time.Duration
}

// The defaults: a status read every 2 s, 30 s a call, 60 s a pairing, a 64 MiB reply, 256 queued events, and events
// polled 20 s at a time and dialed again from 250 ms to 10 s apart.
func defaultSidecarRemoteSettings() *sidecarRemoteSettings {
	return &sidecarRemoteSettings{
		StatusPollInterval:       2 * time.Second,
		CallTimeout:              30 * time.Second,
		PairTimeout:              60 * time.Second,
		AnswerMaxByteCount:       64 * 1024 * 1024,
		EventQueueSize:           256,
		EventWaitTimeout:         20 * time.Second,
		EventReconnectMinTimeout: 250 * time.Millisecond,
		EventReconnectMaxTimeout: 10 * time.Second,
	}
}

// Where the daemon's sidecar stands, as the backend last read it.
type sidecarRemoteStatus struct {
	state      string
	reason     string
	generation int64
	sidecarId  *Id
}

// The remote backend over a control channel. Safe for concurrent use.
type sidecarRemoteBackend struct {
	ctx      context.Context
	cancel   context.CancelFunc
	settings *sidecarRemoteSettings
	dial     sidecarControlDial
	// called with each change of the state, its reason or its generation that a status read finds; it must not block.
	// fresh marks the first of a sidecar this backend did not follow before (takeFreshWithLock)
	stateChanged func(generation int64, state string, reason string, fresh bool)
	// called with each event of the followed network the events long-poll takes, in order, fresh as above; it must
	// not block
	eventReceived func(event *sidecarControlEvent, fresh bool)
	pollDone      chan struct{}
	// closed once followEvents has returned, its session closed
	eventsDone chan struct{}

	// one call at a time, as the daemon answers them; guards channel
	callLock sync.Mutex
	channel  sidecarControlChannel

	stateLock sync.Mutex
	status    sidecarRemoteStatus
	// the newest generation taken for status's sidecar_id, from a status or a pairing: nothing older is taken
	generationFloor int64
	// A status read that crosses a pairing or state event must not clear the resulting identity.
	identityRevision uint64
	// Events still queued at a reconnect can predate the logout already observed in status.
	loggedOutSidecarId *Id
	// the network the pairing bound, nil before one
	networkId *Id
	// the networks the last status named, and its first, which an unpaired backend follows; nil while it names none
	statusNetworkIds   []*Id
	statusFirstNetwork *Id
	// the events long-poll answers, so its state events move the state and status reads by anything else leave it
	eventsCarryState bool
	// the next state or event emitted is the first of a sidecar this backend did not follow before: a pairing moved
	// it to another network's
	freshPending bool
	closed       bool
}

var _ sidecarBackend = (*sidecarRemoteBackend)(nil)

// A backend that dials with dial, offline until its first status. stateChanged and eventReceived may be nil. The
// caller starts run and followEvents.
func newSidecarRemoteBackend(ctx context.Context, dial sidecarControlDial, settings *sidecarRemoteSettings,
	stateChanged func(generation int64, state string, reason string, fresh bool), eventReceived func(event *sidecarControlEvent, fresh bool)) *sidecarRemoteBackend {
	cancelCtx, cancel := context.WithCancel(ctx)
	if stateChanged == nil {
		stateChanged = func(generation int64, state string, reason string, fresh bool) {}
	}
	if eventReceived == nil {
		eventReceived = func(event *sidecarControlEvent, fresh bool) {}
	}
	return &sidecarRemoteBackend{
		ctx:           cancelCtx,
		cancel:        cancel,
		settings:      settings,
		dial:          dial,
		stateChanged:  stateChanged,
		eventReceived: eventReceived,
		pollDone:      make(chan struct{}),
		eventsDone:    make(chan struct{}),
		status:        sidecarRemoteStatus{state: sidecarRemoteStateOffline},
	}
}

// Reads the daemon's status at once and then every StatusPollInterval until the backend closes, a turn passing while
// the events long-poll carries the state (pollStatus). A read that fails leaves the backend offline, and the next one
// dials again.
func (self *sidecarRemoteBackend) run() {
	defer close(self.pollDone)
	self.refresh(self.ctx)
	ticker := time.NewTicker(self.settings.StatusPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-ticker.C:
			self.pollStatus(self.ctx)
		}
	}
}

// One turn of the status poll: a read, unless the events long-poll answers, whose state events stand for it (QD-821).
func (self *sidecarRemoteBackend) pollStatus(ctx context.Context) {
	if self.eventsCarry() {
		return
	}
	self.refresh(ctx)
}

// Whether the events long-poll answers, so its state events carry the state.
func (self *sidecarRemoteBackend) eventsCarry() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.eventsCarryState
}

// Notes whether the events long-poll answers.
func (self *sidecarRemoteBackend) setEventsCarry(carry bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.eventsCarryState = carry
}

// Keeps one events long-poll open on a session of its own until the backend closes (QD-821; see the file's comment):
// takes the cursor and reads status, then hands each event of the followed network to eventReceived in order; reads
// status and emits state after a gap; and after a session that ended or a refusal, reads status at once and calls again
// after a backoff, dialing again where the session ended.
func (self *sidecarRemoteBackend) followEvents() {
	defer close(self.eventsDone)
	var channel sidecarControlChannel
	defer func() {
		if channel != nil {
			channel.close()
		}
		self.setEventsCarry(false)
	}()
	cursor := ""
	failureCount := 0
	for self.ctx.Err() == nil {
		if 0 < failureCount {
			backoff := self.settings.EventReconnectMinTimeout
			for i := 1; i < failureCount && backoff < self.settings.EventReconnectMaxTimeout; i += 1 {
				backoff *= 2
			}
			reconnect := connect.NewPacedReconnect(min(backoff, self.settings.EventReconnectMaxTimeout))
			select {
			case <-self.ctx.Done():
				return
			case <-reconnect.After():
			}
		}
		if channel == nil {
			dialCtx, cancel := context.WithTimeout(self.ctx, self.settings.CallTimeout)
			opened, err := self.dial(dialCtx)
			cancel()
			if err != nil {
				failureCount += 1
				continue
			}
			channel = opened
		}
		params := &sidecarControlEventsParams{After: cursor}
		if cursor != "" {
			params.WaitMs = self.settings.EventWaitTimeout.Milliseconds()
		}
		// a reply with no event comes at once only for the cursor, a gap or a daemon that does not wait: the next call
		// waits for this, so none of them turns the loop hot
		paced := connect.NewPacedReconnect(self.settings.EventReconnectMinTimeout)
		reply := &sidecarControlEventsReply{}
		callCtx, cancel := context.WithTimeout(self.ctx, self.settings.EventWaitTimeout+self.settings.CallTimeout)
		err := channel.call(callCtx, sidecarControlMethodEvents, params, false, reply)
		cancel()
		if err == nil && reply.Cursor == "" {
			// no place to go on from: a broken answer, kept apart from the session
			err = &sidecarControlError{Message: "the daemon's events answer named no cursor"}
		}
		if err != nil {
			var refusal *sidecarControlError
			if !errors.As(err, &refusal) {
				// the session is gone, and with it whatever its answer would have said
				channel.close()
				channel = nil
			}
			if self.ctx.Err() != nil {
				return
			}
			// the poll reads status again, and this reads it now, offline among what it may find
			self.setEventsCarry(false)
			self.refreshFromEvents(self.ctx, false)
			failureCount += 1
			continue
		}
		failureCount = 0
		switch {
		case cursor == "" || reply.Gap:
			// subscribed, or anew past a gap: status says where things stand, and every event from the cursor on
			// follows it
			gap := cursor != ""
			cursor = reply.Cursor
			self.setEventsCarry(true)
			self.refreshFromEvents(self.ctx, gap)
		default:
			cursor = reply.Cursor
			self.setEventsCarry(true)
			self.applyEvents(reply.Events)
		}
		if len(reply.Events) == 0 {
			select {
			case <-self.ctx.Done():
				return
			case <-paced.After():
			}
		}
	}
}

// Takes a reply's events in order. Each of the followed network reaches eventReceived, a state one first moving the
// backend's state, and one of an older generation of the same sidecar is dropped (D-SDK-14). One naming a network the
// last status did not list reads status first, once a reply, so the network an enrollment or a pairing just added is
// followed from its first event.
func (self *sidecarRemoteBackend) applyEvents(events []*sidecarControlEvent) {
	refreshed := false
	for _, event := range events {
		if event == nil {
			continue
		}
		if !refreshed && !self.knowsNetwork(event.NetworkId) {
			// for the networks: the state stays the events', unless the read moves the network followed
			refreshed = true
			self.refresh(self.ctx)
		}
		if taken, fresh := self.takeEvent(event); taken {
			self.eventReceived(event, fresh)
		}
	}
}

// Whether networkId is one the last status named or the pairing bound; nil, a sidecar's before enrollment, always is.
func (self *sidecarRemoteBackend) knowsNetwork(networkId *Id) bool {
	if networkId == nil {
		return true
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if sidecarControlSameId(self.networkId, networkId) {
		return true
	}
	for _, statusNetworkId := range self.statusNetworkIds {
		if sidecarControlSameId(statusNetworkId, networkId) {
			return true
		}
	}
	return false
}

// Whether event is the followed network's and not of an older generation of its sidecar, moving the floor, the
// sidecar_id and, for a state event, the state, which it makes read as a status's does, and whether it is the first of
// a sidecar this backend did not follow before. The event's own state is rewritten to match, so a listener and
// GetState agree.
func (self *sidecarRemoteBackend) takeEvent(event *sidecarControlEvent) (bool, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return false, false
	}
	if !sidecarControlSameId(event.NetworkId, self.followedWithLock()) {
		return false, false
	}
	if event.OriginSidecarId != nil && sidecarControlSameId(event.OriginSidecarId, self.loggedOutSidecarId) {
		return false, false
	}
	current := self.status
	sameSidecar := current.sidecarId == nil || event.OriginSidecarId == nil || current.sidecarId.Cmp(event.OriginSidecarId) == 0
	if event.LoggedOut && event.Kind == sidecarControlEventKindState && event.State == sidecarRemoteStateUnenrolled {
		if !sameSidecar || event.Generation < self.generationFloor {
			return false, false
		}
		self.loggedOutSidecarId = sidecarControlIdOf(event.OriginSidecarId)
		if self.loggedOutSidecarId == nil {
			self.loggedOutSidecarId = current.sidecarId
		}
		self.status = sidecarRemoteStatus{state: sidecarRemoteStateUnenrolled}
		self.generationFloor = 0
		self.identityRevision++
		event.Generation = 0
		return true, self.takeFreshWithLock(true)
	}
	if sameSidecar && event.Generation < self.generationFloor {
		return false, false
	}
	if !sameSidecar || current.sidecarId == nil {
		self.status.sidecarId = sidecarControlIdOf(event.OriginSidecarId)
	}
	if !sameSidecar || self.generationFloor < event.Generation {
		self.generationFloor = event.Generation
	}
	if event.Kind == sidecarControlEventKindState {
		self.identityRevision++
		event.State, event.Reason = sidecarRemoteStateOf(event.State, event.Reason)
		self.status.state, self.status.reason, self.status.generation = event.State, event.Reason, event.Generation
	}
	return true, self.takeFreshWithLock(!sameSidecar)
}

// The network this backend follows: the pairing's, else the first the last status named, nil for none.
func (self *sidecarRemoteBackend) followedWithLock() *Id {
	if self.networkId != nil {
		return self.networkId
	}
	return self.statusFirstNetwork
}

// Whether what is emitted now is the first of a sidecar this backend did not follow before: another sidecar_id than
// the one it followed (changed), or the first since a pairing moved it to another network's. Taking it clears the
// pairing's mark.
func (self *sidecarRemoteBackend) takeFreshWithLock(changed bool) bool {
	fresh := changed || self.freshPending
	self.freshPending = false
	return fresh
}

// A state and reason as this sdk reads them: a state it does not know reads offline, a daemon that runs no sidecar
// among them, and revoked without the reason only a verified SidecarRevoke carries reads unenrolled(refused), since
// nothing but signed means revoked (D-SDK-14).
func sidecarRemoteStateOf(state string, reason string) (string, string) {
	switch state {
	case sidecarRemoteStateLocked, sidecarRemoteStateUnenrolled, sidecarRemoteStateOffline, sidecarRemoteStateOnline:
		return state, reason
	case sidecarRemoteStateRevoked:
		if reason != sidecarRemoteReasonSigned {
			return sidecarRemoteStateUnenrolled, sidecarRemoteReasonRefused
		}
		return state, reason
	}
	return sidecarRemoteStateOffline, ""
}

// See sidecarBackend.
func (self *sidecarRemoteBackend) sidecarId() *Id {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.status.sidecarId
}

// See sidecarBackend.
func (self *sidecarRemoteBackend) state() (string, string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.status.state, self.status.reason
}

// See sidecarBackend.
func (self *sidecarRemoteBackend) generation() int64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.status.generation
}

// The network the pairing bound, as a control call's network argument: empty before a pairing, so the daemon picks by
// §5.8's rule.
func (self *sidecarRemoteBackend) network() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.networkId == nil {
		return ""
	}
	return self.networkId.String()
}

// See sidecarBackend. While the events long-poll carries the state, a read from here moves the sidecar_id and the
// networks and leaves the state to the events, so an answer that crossed an event cannot undo it.
func (self *sidecarRemoteBackend) refresh(ctx context.Context) error {
	revision := self.revision()
	reply := &sidecarControlStatus{}
	if err := self.callBounded(ctx, self.settings.CallTimeout, sidecarControlMethodStatus, nil, false, reply); err != nil {
		return err
	}
	self.applyStatusAt(reply, false, false, revision)
	return nil
}

// A status read by the events long-poll, between its calls, so no event of its own crosses it: it moves the state as
// a read with no long-poll does, and with always it emits the state even unchanged, as after a gap, whose lost events
// may have moved it and back.
func (self *sidecarRemoteBackend) refreshFromEvents(ctx context.Context, always bool) error {
	revision := self.revision()
	reply := &sidecarControlStatus{}
	if err := self.callBounded(ctx, self.settings.CallTimeout, sidecarControlMethodStatus, nil, false, reply); err != nil {
		return err
	}
	self.applyStatusAt(reply, true, always, revision)
	return nil
}

// See sidecarBackend.
func (self *sidecarRemoteBackend) call(ctx context.Context, method string, params any, confirmed bool, result any) error {
	return self.callBounded(ctx, self.settings.CallTimeout, method, params, confirmed, result)
}

// One call within timeout, dialing a session when none is open. A session that fails is dropped and the backend reads
// offline, so the next call dials again; a daemon's refusal keeps the session.
func (self *sidecarRemoteBackend) callBounded(ctx context.Context, timeout time.Duration, method string, params any, confirmed bool, result any) error {
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// a close ends a call in flight
	stop := context.AfterFunc(self.ctx, cancel)
	defer stop()

	self.callLock.Lock()
	defer self.callLock.Unlock()
	if self.ctx.Err() != nil {
		return errSidecarRemoteClosed
	}
	if self.channel == nil {
		channel, err := self.dial(callCtx)
		if err != nil {
			self.applyOffline()
			return errors.Join(errSidecarRemoteOffline, err)
		}
		self.channel = channel
	}
	err := self.channel.call(callCtx, method, params, confirmed, result)
	if err == nil {
		return nil
	}
	var refusal *sidecarControlError
	if errors.As(err, &refusal) {
		return refusal
	}
	self.channel.close()
	self.channel = nil
	if self.ctx.Err() != nil {
		return errSidecarRemoteClosed
	}
	self.applyOffline()
	return errors.Join(errSidecarRemoteOffline, err)
}

// See sidecarBackend. The token is in the request alone: the reply names the binding, and a refusal that repeats the
// token is replaced.
func (self *sidecarRemoteBackend) pair(ctx context.Context, childJwt string) (*sidecarControlPairReply, error) {
	if childJwt == "" {
		return nil, errors.New("a pairing needs the child's token")
	}
	reply := &sidecarControlPairReply{}
	err := self.callBounded(ctx, self.settings.PairTimeout, sidecarControlMethodPair, &sidecarControlPairParams{ChildJwt: childJwt}, false, reply)
	if err != nil {
		if strings.Contains(err.Error(), childJwt) {
			return nil, errSidecarRemotePairRefused
		}
		return nil, err
	}
	if reply.NetworkId == nil || reply.Generation <= 0 {
		return nil, errSidecarRemotePairReply
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.networkId == nil || self.networkId.Cmp(reply.NetworkId) != 0 {
			self.identityRevision++
			// the daemon's sidecar of this network, which the next status names: its own id and generations, which its
			// first event delivers past whatever another network's left
			self.networkId = reply.NetworkId
			self.status.sidecarId = nil
			self.generationFloor = reply.Generation
			self.freshPending = true
			return
		}
		// the same sidecar's next generation: nothing older ends it from here on
		if self.generationFloor < reply.Generation {
			self.identityRevision++
			self.generationFloor = reply.Generation
		}
	}()
	return reply, nil
}

// Relays forwarding's setup for the app's VPN component (§18.6; BRIDGE-M8-06): the component's public keys out and
// the hand-off back, both as their encoded messages. confirmAfterRevocation carries the person's confirmation in the
// app's UI of setting forwarding up again after another signer's revocation.
func (self *sidecarRemoteBackend) forwardingSetup(ctx context.Context, keys []byte, confirmAfterRevocation bool) ([]byte, error) {
	reply := &sidecarControlForwardingSetupReply{}
	params := &sidecarControlForwardingSetupParams{Network: self.network(), Keys: keys, ConfirmAfterRevocation: confirmAfterRevocation}
	if err := self.call(ctx, sidecarControlMethodForwardingSetup, params, confirmAfterRevocation, reply); err != nil {
		return nil, err
	}
	if len(reply.HandOff) == 0 {
		return nil, errors.New("the daemon answered forwarding's setup with no hand-off")
	}
	return reply.HandOff, nil
}

// Relays forwarding's witness hint (§18.6): the daemon's pin, whether it is the witnessed one, a fork, and why a setup
// is due.
func (self *sidecarRemoteBackend) forwardingWitness(ctx context.Context) (*sidecarControlForwardingWitnessReply, error) {
	reply := &sidecarControlForwardingWitnessReply{}
	if err := self.call(ctx, sidecarControlMethodForwardingWitness, &sidecarControlNetworkParams{Network: self.network()}, false, reply); err != nil {
		return nil, err
	}
	return reply, nil
}

// See sidecarBackend. A call in flight ends, the session closes and the status reads stop.
func (self *sidecarRemoteBackend) close() {
	alreadyClosed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return true
		}
		self.closed = true
		return false
	}()
	if alreadyClosed {
		return
	}
	self.cancel()
	self.callLock.Lock()
	defer self.callLock.Unlock()
	if self.channel != nil {
		self.channel.close()
		self.channel = nil
	}
}

// The row of reply this backend follows: the paired network's, unenrolled when the daemon hosts it no longer; before
// a pairing the daemon's only network, else its first, which the reply's own fields name.
func (self *sidecarRemoteBackend) statusOf(reply *sidecarControlStatus, networkId *Id) (sidecarRemoteStatus, bool) {
	rowStatus := func(row *sidecarControlNetworkStatus) sidecarRemoteStatus {
		return sidecarRemoteStatus{state: row.State, reason: row.Reason, generation: row.Generation, sidecarId: row.SidecarId}
	}
	if networkId != nil {
		for i := range reply.Networks {
			if row := &reply.Networks[i]; row.NetworkId != nil && row.NetworkId.Cmp(networkId) == 0 {
				return rowStatus(row), true
			}
		}
		return sidecarRemoteStatus{state: sidecarRemoteStateUnenrolled}, false
	}
	if len(reply.Networks) == 1 {
		return rowStatus(&reply.Networks[0]), true
	}
	return sidecarRemoteStatus{state: reply.State, reason: reply.Reason, generation: reply.Generation}, true
}

// Which pairing or state event was current when a status request started.
func (self *sidecarRemoteBackend) revision() uint64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.identityRevision
}

// Applies an authoritative response for the same selection that issued the request.
func (self *sidecarRemoteBackend) applyStatusAt(reply *sidecarControlStatus, fromEvents bool, always bool, revision uint64) {
	fresh := false
	emitted, next := func() (bool, sidecarRemoteStatus) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed || revision != self.identityRevision {
			return false, self.status
		}
		next, hosted := self.statusOf(reply, self.networkId)
		// A status row can still name the removed identity while the daemon finishes detaching its sidecar.
		if next.sidecarId != nil && sidecarControlSameId(next.sidecarId, self.loggedOutSidecarId) {
			return false, self.status
		}
		// After a paired logout, the daemon may still list the old generation without its sidecar_id.
		if self.networkId != nil && self.generationFloor == 0 && next.sidecarId == nil && next.generation > 0 {
			return false, self.status
		}
		followedBefore := self.followedWithLock()
		self.statusNetworkIds = nil
		self.statusFirstNetwork = nil
		for i := range reply.Networks {
			if networkId := reply.Networks[i].NetworkId; networkId != nil {
				self.statusNetworkIds = append(self.statusNetworkIds, networkId)
			}
		}
		if 0 < len(reply.Networks) {
			self.statusFirstNetwork = reply.Networks[0].NetworkId
		}
		next.state, next.reason = sidecarRemoteStateOf(next.state, next.reason)
		current := self.status
		if !hosted {
			if current.sidecarId != nil {
				self.loggedOutSidecarId = current.sidecarId
			}
			self.generationFloor = 0
			self.status = next
			changed := current.sidecarId != nil || current.state != next.state || current.generation != 0 || current.reason != ""
			if changed {
				self.identityRevision++
			}
			fresh = self.takeFreshWithLock(changed)
			return changed || always, next
		}
		sameSidecar := current.sidecarId == nil || next.sidecarId == nil || current.sidecarId.Cmp(next.sidecarId) == 0
		if sameSidecar && next.generation < self.generationFloor {
			// an answer from before a re-pair
			return false, current
		}
		if sameSidecar && next.sidecarId == nil {
			next.sidecarId = current.sidecarId
		}
		if self.eventsCarryState && !fromEvents && sidecarControlSameId(followedBefore, self.followedWithLock()) {
			// the events move the state; this read names the sidecar alone
			if !sidecarControlSameId(current.sidecarId, next.sidecarId) {
				self.identityRevision++
			}
			self.status.sidecarId = next.sidecarId
			return false, self.status
		}
		self.generationFloor = next.generation
		self.status = next
		changed := next.state != current.state || next.reason != current.reason || next.generation != current.generation || !sidecarControlSameId(current.sidecarId, next.sidecarId)
		if changed {
			self.identityRevision++
		}
		emitted := always || changed
		if emitted {
			fresh = self.takeFreshWithLock(!sameSidecar)
		}
		return emitted, next
	}()
	if emitted {
		self.stateChanged(next.generation, next.state, next.reason, fresh)
	}
}

// No daemon answers: offline, the sidecar_id and generation kept for when one does. While the events long-poll
// answers, the daemon does, and a call that failed on the other session leaves the state as the events have it.
func (self *sidecarRemoteBackend) applyOffline() {
	fresh := false
	changed, next := func() (bool, sidecarRemoteStatus) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed || self.eventsCarryState || self.status.state == sidecarRemoteStateOffline {
			return false, self.status
		}
		self.status.state, self.status.reason = sidecarRemoteStateOffline, ""
		fresh = self.takeFreshWithLock(false)
		return true, self.status
	}()
	if changed {
		self.stateChanged(next.generation, next.state, next.reason, fresh)
	}
}
