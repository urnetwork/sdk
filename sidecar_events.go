//go:build !ios_extension && !js

// §18.5's events for apps: one bound SidecarEvent per core event, of every §5.4 kind, carrying the repo, ref and path
// and whatever else its kind names, its times as milliseconds and its ids as *Id. Kind is the core's own string, so an
// app passes along a kind it does not know (dns_alarm, bridge_alarm). Every event names its client generation, and
// one of a generation older than one already delivered is dropped (D-SDK-14).
//
// The core's callback must not block, so it offers each event to a bounded queue with zero wait, and one goroutine per
// Sidecar delivers the queue to every listener in order, each call under trace: a listener that panics loses that one
// call, never the app. A full queue drops the event and counts it; an app reads GetState for where things stand.

package sdk

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
)

// One event. A field is set only for the kinds its comment names; the rest are zero. json in the C ABI and js.
type SidecarEvent struct {
	// The core's kind: state, approval_pending, member_added, member_removed, key_changed, refused, admission_alarm,
	// recovery_rebind, recovery_window, recovery_unseen, admissions_near_cap, head_alarm, index_mismatch,
	// index_malformed, search_alarm, history_drop, access_request, access_decided, directory_erasure, erasure,
	// mail_send, evs_conflict, evs_alarm, evs_gap, credential_store, record_alarm, bridge_alarm, dns_alarm, ct_alarm,
	// mail_receiver_alarm, upgrade_required, disclosure_changed, and any a later component adds.
	Kind string `json:"kind"`
	// The client generation it comes from (§18.5's State block).
	Generation int64 `json:"generation"`
	// state: locked, unenrolled, offline, online or revoked.
	State string `json:"state,omitempty"`
	// state: the reason of an unenrolled or revoked state.
	Reason string `json:"reason,omitempty"`
	// member_added, member_removed: the group.
	GroupId *Id `json:"group_id,omitempty"`
	// The sidecar its kind names: a group's member, an admitted, recovered or rebinding device, key_changed's sidecar,
	// a head alarm's peer, an index alarm's or drop's signer, a search alarm's indexer, an evs_alarm's or
	// record_alarm's signer where one is known, a ct_alarm's attesting identity, a mail_receiver_alarm's receiver or
	// record signer.
	SidecarId *Id `json:"sidecar_id,omitempty"`
	// Index alarms, search_alarm, history_drop, directory_erasure, erasure, the evs kinds, record_alarm's log and
	// mail_receiver_alarm.
	RepoId *Id `json:"repo_id,omitempty"`
	// Index alarms, history_drop and the evs kinds.
	RefId *Id `json:"ref_id,omitempty"`
	// Index alarms: the path; history_drop: its first listed path; evs_conflict: the local conflict ref; evs_alarm:
	// the path, where one is named. Local to this device: never log it (I-13).
	Path string `json:"path,omitempty"`
	// history_drop.
	DropId *Id `json:"drop_id,omitempty"`
	// history_drop: pending, cancelled, executing, done, refused or invalid.
	DropState string `json:"drop_state,omitempty"`
	// history_drop: its window's end, in unix milliseconds; 0 for none.
	NotBeforeMillis int64 `json:"not_before_millis,omitempty"`
	// search_alarm: mismatch or omission.
	SearchAlarm string `json:"search_alarm,omitempty"`
	// access_request, access_decided, directory_erasure.
	RequestId *Id `json:"request_id,omitempty"`
	// access_decided: approved, denied, cancelled or expired; directory_erasure: pending or done.
	Outcome string `json:"outcome,omitempty"`
	// mail_send: the send, whose outcomes GetSendResult reads.
	SendId *Id `json:"send_id,omitempty"`
	// approval_pending.
	ApprovalId *Id `json:"approval_id,omitempty"`
	// head_alarm: fork, withheld, unverified or unwitnessed (§6.4), or rollback, a push reset that presented a durable
	// topic below the version this device holds (§6.8; owner decision 300).
	HeadAlarm string `json:"head_alarm,omitempty"`
	// head_alarm: this device's pin, or for rollback the TopicVersion held; recovery_rebind at a void: the AuthPin
	// keychain.wrap was sealed at; key_changed: the SHA-256 of the foreign key gkm served, where it names one.
	FirstEvidence []byte `json:"first_evidence,omitempty"`
	// head_alarm: the peer's or api's pin, or for rollback the TopicVersion offered; recovery_rebind at a void: the
	// AuthPin it was re-sealed at.
	SecondEvidence []byte `json:"second_evidence,omitempty"`
	// erasure.
	Erasure *SidecarErasure `json:"erasure,omitempty"`
	// evs_alarm: missing_segment, tamper, version_rule, epoch_overdue or prefix_rule.
	EvsAlarm string `json:"evs_alarm,omitempty"`
	// evs_alarm of reason prefix_rule: ignored, refused, set_void or set_newer, what it stands for.
	EvsAlarmDetail string `json:"evs_alarm_detail,omitempty"`
	// evs_alarm of reason prefix_rule: the ref version of the first change it names, or a set's log position;
	// disclosure_changed: the disclosure's current version.
	Version int64 `json:"version,omitempty"`
	// evs_alarm of reason prefix_rule: the push of the first change it names; nil for a set's.
	PushId *Id `json:"push_id,omitempty"`
	// evs_alarm of reason prefix_rule: how many changes it stands for.
	Count int64 `json:"count,omitempty"`
	// evs_gap: the newest re-pin past the catch-up budget, in unix milliseconds; 0 for none.
	VerifiedSinceMillis int64 `json:"verified_since_millis,omitempty"`
	// record_alarm: what the record found, one of the core's closed set (chain_gap, chain_fork, bad_entry, ...).
	RecordAlarm string `json:"record_alarm,omitempty"`
	// bridge_alarm: what the bridge found (block_conflict, state_unverified, map_refused, ...).
	BridgeAlarm string `json:"bridge_alarm,omitempty"`
	// dns_alarm: what the zone watch found (split, unrequested, zone_mismatch, ...).
	DnsAlarm string `json:"dns_alarm,omitempty"`
	// ct_alarm: what the CT watch found (unattested, recent_identity).
	CtAlarm string `json:"ct_alarm,omitempty"`
	// mail_receiver_alarm: why a reader alarmed (D-MAIL-15): undesignated, internet mail taken by a receiver that is
	// none of the network's mail indexers; scope, a member scope's deposit from a receiver that is no consented
	// device of the scope's member; deposit, a deposit ingest passed over or wrote as evidence unavailable; record, a
	// mailcfg version a reader passed over.
	MailReceiverAlarm string `json:"mail_receiver_alarm,omitempty"`
	// upgrade_required: the authorization features the network turned on that this release lacks, by name, comma
	// separated (agent_repos, compute_policy, listeners, enclaves): the sidecar holds, applying nothing from that base,
	// until the update that ends it (DESIGN-COMPUTE §16.6's hold).
	AuthFeatures string `json:"auth_features,omitempty"`
	// disclosure_changed, on an Owner's or Admin's device (DESIGN-COMPUTE §2.8's new version row): the disclosure whose
	// acknowledgment for the network names an older version than its current one, Version. The app's notice to read the
	// new text and acknowledge it again for the network (AckNetworkDisclosure), before its grace ends.
	DisclosureId string `json:"disclosure_id,omitempty"`
}

// An erasure as the device running it, or one that may finish it, sees it (§11.12, §11.13's wait). json in the C ABI
// and js.
type SidecarErasure struct {
	ErasureId *Id `json:"erasure_id,omitempty"`
	// fenced, rewriting, waiting, done or stalled.
	State string `json:"state"`
	// Rekey objects not yet re-keyed.
	ObjectsLeft int64 `json:"objects_left,omitempty"`
	// Their closure bytes.
	BytesLeft int64 `json:"bytes_left,omitempty"`
	// The last 429's retry, in unix milliseconds; 0 when none waits.
	NextTryMillis int64 `json:"next_try_millis,omitempty"`
	// The bound on its finish, in unix milliseconds; 0 below the limit.
	FinishMillis int64 `json:"finish_millis,omitempty"`
}

// App-implemented: called on a Go thread with each event, in order.
type SidecarEventListener interface {
	SidecarEventReceived(event *SidecarEvent)
}

// The bound event of a core event.
func newSidecarEventOf(event *sidecar.SidecarEvent) *SidecarEvent {
	bound := &SidecarEvent{
		Kind:                string(event.Kind),
		Generation:          event.Generation,
		State:               string(event.State),
		Reason:              string(event.Reason),
		GroupId:             sidecarIdOf(event.GroupId),
		SidecarId:           sidecarIdOf(event.SidecarId),
		RepoId:              sidecarIdOf(event.RepoId),
		RefId:               sidecarIdOf(event.RefId),
		Path:                event.Path,
		DropId:              sidecarIdOf(event.DropId),
		DropState:           string(event.DropState),
		NotBeforeMillis:     sidecarMillisOf(event.NotBefore),
		SearchAlarm:         string(event.SearchAlarm),
		RequestId:           sidecarIdOf(event.RequestId),
		Outcome:             event.Outcome,
		SendId:              sidecarIdOf(event.SendId),
		ApprovalId:          sidecarIdOf(event.ApprovalId),
		HeadAlarm:           string(event.HeadAlarm),
		FirstEvidence:       sidecarBytesOf(event.Evidence[0]),
		SecondEvidence:      sidecarBytesOf(event.Evidence[1]),
		EvsAlarm:            string(event.EvsAlarm),
		EvsAlarmDetail:      string(event.EvsAlarmDetail),
		Version:             int64(event.Version),
		PushId:              sidecarIdOf(event.PushId),
		Count:               int64(event.Count),
		VerifiedSinceMillis: sidecarMillisOf(event.VerifiedSince),
		RecordAlarm:         string(event.RecordAlarm),
		BridgeAlarm:         string(event.BridgeAlarm),
		DnsAlarm:            string(event.DnsAlarm),
		CtAlarm:             string(event.CtAlarm),
		MailReceiverAlarm:   string(event.MailReceiverAlarm),
		AuthFeatures:        event.AuthFeatures.String(),
		DisclosureId:        event.DisclosureId,
	}
	if erasure := event.Erasure; erasure != nil {
		bound.Erasure = sidecarErasureOf(erasure)
	}
	return bound
}

// The bound id of a core id, nil for nil.
func sidecarIdOf(id *sidecar.Id) *Id {
	if id == nil {
		return nil
	}
	return newId([16]byte(*id))
}

// Unix milliseconds, 0 for the zero time.
func sidecarMillisOf(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// A copy, nil for none.
func sidecarBytesOf(value []byte) []byte {
	if len(value) == 0 {
		return nil
	}
	return append([]byte{}, value...)
}

// The tunables of a Sidecar's event delivery.
type sidecarEventSettings struct {
	// Events waiting for the delivering goroutine: 256, a burst of a generation's worth of alarms and state moves.
	// Past it an event is dropped and counted, never waited for, since the core's callback must not block.
	QueueEventCount int
}

// The defaults, a fresh value each call.
func defaultSidecarEventSettings() *sidecarEventSettings {
	return &sidecarEventSettings{
		QueueEventCount: 256,
	}
}

// One queued event.
type sidecarDispatchItem struct {
	event *sidecar.SidecarEvent
	// the first event of a sidecar the remote backend did not follow before, such as one a logout and a new enrollment
	// made in the same network or another network's after a pairing: its generations start anew, so the floor starts
	// again at its own (D-SDK-14)
	fresh bool
}

// One Sidecar's delivery of its core's events to its app listeners. Safe for concurrent use.
type sidecarEventDispatch struct {
	ctx       context.Context
	cancel    context.CancelFunc
	events    chan sidecarDispatchItem
	listeners *connect.CallbackList[SidecarEventListener]
	// the newest generation an event was delivered for, read and written by the delivering goroutine
	newestGeneration int64
	// events dropped at a full queue
	droppedCount atomic.Int64
	// set by close: no listener call starts after it
	closed atomic.Bool
	// closed once the delivering goroutine has returned
	done chan struct{}
}

// A dispatch running until ctx ends or close.
func newSidecarEventDispatch(ctx context.Context, settings *sidecarEventSettings) *sidecarEventDispatch {
	cancelCtx, cancel := context.WithCancel(ctx)
	self := &sidecarEventDispatch{
		ctx:       cancelCtx,
		cancel:    cancel,
		events:    make(chan sidecarDispatchItem, settings.QueueEventCount),
		listeners: connect.NewCallbackList[SidecarEventListener](),
		done:      make(chan struct{}),
	}
	go self.run()
	return self
}

// Ends delivery without waiting, so a listener may call it: no listener call starts after it returns, though one
// already running finishes.
func (self *sidecarEventDispatch) close() {
	self.closed.Store(true)
	self.cancel()
}

// The core's callback: a copy of event offered with zero wait, dropped and counted when the queue is full.
func (self *sidecarEventDispatch) offer(event sidecar.SidecarEvent) {
	self.offerItem(sidecarDispatchItem{event: &event})
}

// offer of the first event of a sidecar the remote backend did not follow before (sidecarDispatchItem.fresh).
func (self *sidecarEventDispatch) offerFresh(event sidecar.SidecarEvent) {
	self.offerItem(sidecarDispatchItem{event: &event, fresh: true})
}

// Queues item with zero wait, dropping and counting it when the queue is full.
func (self *sidecarEventDispatch) offerItem(item sidecarDispatchItem) {
	select {
	case self.events <- item:
	default:
		self.droppedCount.Add(1)
	}
}

// Delivers queued events in order until ctx ends.
func (self *sidecarEventDispatch) run() {
	defer close(self.done)
	for {
		select {
		case <-self.ctx.Done():
			return
		case item := <-self.events:
			self.deliver(item.event, item.fresh)
		}
	}
}

// Delivers one event to every listener, unless a newer generation's was delivered first and event is not a fresh
// sidecar's (D-SDK-14).
func (self *sidecarEventDispatch) deliver(event *sidecar.SidecarEvent, fresh bool) {
	if !fresh && event.Generation < self.newestGeneration {
		return
	}
	self.newestGeneration = event.Generation
	for _, listener := range self.listeners.Get() {
		if self.closed.Load() {
			return
		}
		// each listener its own copy, so one that edits it changes nothing another sees; and a listener's panic loses
		// its own call, never the app or the other listeners
		bound := newSidecarEventOf(event)
		trace(func() {
			listener.SidecarEventReceived(bound)
		})
	}
}

// Adds listener, until the answer is called.
func (self *sidecarEventDispatch) addListener(listener SidecarEventListener) func() {
	listenerId := self.listeners.Add(listener)
	return func() {
		self.listeners.Remove(listenerId)
	}
}
