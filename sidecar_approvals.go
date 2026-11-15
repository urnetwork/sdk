//go:build !ios_extension && !js

// SidecarApprovals (§18.5, D-SDK-9, D-SDK-30, R-SDK-12, I-4, I-8; SDK-M8-05): an app's decisions on what waits for its
// device and its revocation of another of its member's devices, each a thin mapping onto the core's calls on the local
// backend, or onto ur daemon's control methods on the remote one. The signing, the cascade and every rule of who may
// sign what are the core's (§5.6, §7.9, §16.6): nothing here decides a kind, an eligibility or whether an act needs
// presence, and every check the core makes stands behind each call.
//
//	pending   GetPending: every kind the core lists, each with the decisions this device may make on it now. The local
//	          backend reads the core's authority items (Sidecar.PendingApprovals: admissions, admission alarms, recovery
//	          windows, recovery_rebind, recovery_unseen and the cleanup, shown as admissions_near_cap with the list a
//	          revoke signs, which reading it shows here, PreviewCleanup), §16.8's requests (ApprovalApprover, where the
//	          record is bound) and §16.11's access requests this device decides (DirectoryClient, in a team network).
//	          The remote backend reads approvals.list, approvals.cleanup_list and approvals.requests; §5.8 has no
//	          method for access requests yet (RECORD-M6-23), so a remote-mode app lists none. A revocation proposal
//	          appears once the core lists it as an item (CORE-M6-04's open kind); until then the proposer hands its id
//	          to `ur approve`
//	decide    Decide: one decision per item, a person's act in the app's UI, never MCP (D-CORE-13), so it goes to the
//	          core, and over the daemon's Confirm methods, as confirmed. An item is read afresh and must still offer the
//	          decision; this Sidecar then refuses a second Decide of it, also while the first runs (§16.8's single
//	          use), since a request stays listed until its decision is read back. A failure that signed nothing frees
//	          the item for another try. Admissions and OwnerConfirms follow the safety number the item carries
//	devices   GetDevices, PreviewRevokeDevice and RevokeDevice wrap §5.4's calls (D-SDK-30): the plan a person reads,
//	          then RevokeDevice of that plan id, with the plan's key items the person left selected. Refusals carry
//	          §5.6's REVOKE_ codes, FAILED_PRECONDITION while locked or unenrolled, and UNAVAILABLE when nothing could
//	          be reached
//	presence  wherever the log covers an act, the app's SidecarPresenceSigner signs its countersigned bytes before any
//	          statement is signed (sidecar_presence.go): in process on the local backend, and on the remote one by §5.8's
//	          two calls for RevokeDevice. A refusal, or no signer where the log covers the act, answers REVOKE_CANCELLED
//	          with nothing signed. A remote decision runs under the daemon's own presence prompt until the daemon
//	          serves Decide's two calls (CORE-M8-03), so an act covered by a key only the app holds is not signed there
//	listen    AddPendingListener hears GetPending's list again after each event of a kind that may change it, after
//	          each Decide, and every PendingRefreshInterval while a listener is added, for what no event announces
//
// The page Worker and the extension's hosts never reach these calls: they link no sdk, or answer REVOKE_NOT_CAPABLE
// on js (§18.4, §18.8). Each call answers once, on a Go thread, through its callback, or for the synchronous GetPending
// and GetDevices within ReadTimeout on the caller's, nil when nothing could be read, so an unknown never reads as none.
// A panic in the core reaches the callback as its error, never the app. Nothing here logs an item, a device or a plan,
// and no credential crosses (I-7, I-13). Safe for concurrent use.

package sdk

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
)

// What an app's approvals calls are held to.
type sidecarApprovalsSettings struct {
	// The bound of one call that answers through a callback: 2 min, a person's presence check and the act's posts
	// included.
	CallTimeout time.Duration
	// The bound of GetPending and GetDevices, which answer on the caller's thread: 10 s.
	ReadTimeout time.Duration
	// How often the pending list is read again while a listener is added, for a change no event announces, such as a
	// request filed in the approval log: 60 s. 0 reads it at events and decisions alone.
	PendingRefreshInterval time.Duration
	// How many decided approval ids a SidecarApprovals remembers, to refuse a second Decide while the core may still
	// list the item: 1,024, the oldest forgotten first.
	DecidedMaxCount int
}

// The defaults, a fresh value each call.
func defaultSidecarApprovalsSettings() *sidecarApprovalsSettings {
	return &sidecarApprovalsSettings{
		CallTimeout:            2 * time.Minute,
		ReadTimeout:            10 * time.Second,
		PendingRefreshInterval: 60 * time.Second,
		DecidedMaxCount:        1024,
	}
}

// The prefix of a result's Value while other AdminSet devices' cosigns are due (§18.5), before the proposal's id in
// hex, as `ur approve <proposal_id>` takes it. A format value: apps read it.
const sidecarAuthorityPendingPrefix = "authority_pending:"

// The event kinds after which the pending list may have changed (§5.4's kinds).
var sidecarApprovalEventKinds = []string{
	string(sidecar.SidecarEventState),
	string(sidecar.SidecarEventApprovalPending),
	string(sidecar.SidecarEventAdmissionAlarm),
	string(sidecar.SidecarEventRecoveryRebind),
	string(sidecar.SidecarEventRecoveryWindow),
	string(sidecar.SidecarEventRecoveryUnseen),
	string(sidecar.SidecarEventAdmissionsNearCap),
	string(sidecar.SidecarEventAccessRequest),
	string(sidecar.SidecarEventAccessDecided),
}

var (
	// The Sidecar closed, or the call's context ended, before an answer.
	errSidecarApprovalsUnavailable = errors.New("approvals are unavailable: the sidecar closed or the call was cancelled")
	// The call named no item, device, plan or reason it takes.
	errSidecarApprovalsArgument = errors.New("the call names nothing it can act on")
	// This Sidecar decided the item, or is deciding it.
	errSidecarApprovalDecided = errors.New("this item was decided here already")
	// No item of that id waits for this device.
	errSidecarApprovalUnknown = errors.New("no such item waits for this device's decision")
	// The item does not offer that decision to this device now.
	errSidecarApprovalDecision = errors.New("the item does not take that decision from this device")
)

// One item that waits for this device's decision (§18.5's GetPending), each decided once with Decide. A field is set
// only for the kinds its comment names. json in the C ABI and js.
type SidecarApprovalItem struct {
	// What Decide takes; nil for a request this device shows refused, which takes no decision.
	ApprovalId *Id `json:"approval_id,omitempty"`
	// One of the SidecarApprovalKind values, or a kind a later core adds, which an app shows as it shows a kind it
	// does not know.
	Kind string `json:"kind"`
	// The device it is about: the admitted or alarmed device, a rebind's signer, a recovered device, a request's
	// requester.
	SidecarId *Id `json:"sidecar_id,omitempty"`
	// The member it is about.
	MemberId *Id `json:"member_id,omitempty"`
	// recovery_window and revocation_proposal: §7.9's 32-byte proposal_id.
	ProposalId []byte `json:"proposal_id,omitempty"`
	// The entry that opened it: where an alarm's revoke answer revokes from, which PreviewRevokeDevice previews as a
	// compromise from this sinceN.
	Position int64 `json:"position,omitempty"`
	// When it was raised, in unix milliseconds; 0 for none known.
	RaisedMillis int64 `json:"raised_millis,omitempty"`
	// admission and admission_alarm: the 60-digit safety number the person compares with the other device's, shown
	// as 12 groups of 5 in three lines of 4 groups (QD-039); empty where none is known.
	SafetyNumber string `json:"safety_number,omitempty"`
	// request: the action's kind as record.proto names it, such as dns_set or mail_send.
	Action string `json:"action,omitempty"`
	// request: a multi-use template, each use's action matching its fields.
	Template bool `json:"template,omitempty"`
	// request: how this device signs it, device or presence (this sidecar's own request); empty where it signs none.
	Channel string `json:"channel,omitempty"`
	// request: the action as the core built it, with the agent's own words under their untrusted heading (§16.6),
	// printable, for a person. Never a field to act on.
	Text string `json:"text,omitempty"`
	// request: why this device shows it refused and takes no approval of it; empty for none.
	Refused string `json:"refused,omitempty"`
	// access_request: the request, its reason in its untrusted text.
	AccessRequest *SidecarAccessRequest `json:"access_request,omitempty"`
	// admissions_near_cap: the list a revoke signs, as this device showed it.
	Cleanup *SidecarCleanupList `json:"cleanup,omitempty"`
	// What this device may answer: approve, deny, revoke.
	Decisions *StringList `json:"decisions"`

	// where the backend found it, which Decide dispatches by; never crosses
	source sidecarApprovalSource
}

// Where an item comes from, inside a backend.
type sidecarApprovalSource int

const (
	// the core's authority items (Sidecar.PendingApprovals), the cleanup among them
	sidecarApprovalSourceAuthority sidecarApprovalSource = iota + 1
	// §16.8's requests (ApprovalApprover)
	sidecarApprovalSourceRequest
	// §16.11's access requests (DirectoryClient)
	sidecarApprovalSourceAccess
)

// Items, in the order GetPending lists them: the core's authority items, then requests, then access requests.
type SidecarApprovalList struct {
	exportedList[*SidecarApprovalItem]
}

// An empty list.
func NewSidecarApprovalList() *SidecarApprovalList {
	return &SidecarApprovalList{
		exportedList: *newExportedList[*SidecarApprovalItem](),
	}
}

// App-implemented: called on a Go thread with what GetPending answers whenever it may have changed, an item raised or
// decided, here or on another device. Calls come one at a time.
type SidecarApprovalListener interface {
	ApprovalsChanged(pending *SidecarApprovalList)
}

// One page Worker on an admin's cleanup list (§7.10's cleanup row). json in the C ABI and js.
type SidecarCleanupWorker struct {
	SidecarId *Id `json:"sidecar_id"`
	// Its member.
	MemberId *Id `json:"member_id"`
	// The entry its admission took effect at, and that entry's time in unix milliseconds, 0 for none known.
	AdmittedPosition int64 `json:"admitted_position,omitempty"`
	AdmittedMillis   int64 `json:"admitted_millis,omitempty"`
	// Its leaf's last change this device shows, in unix milliseconds; 0 for none seen.
	LastChangeMillis int64 `json:"last_change_millis,omitempty"`
	// The counted signatures its revocation needs: 1, or k for an AdminSet member's Worker, whose revocation goes as a
	// proposal other AdminSet devices cosign.
	Signatures int32 `json:"signatures"`
}

// Workers, by member and then oldest admission first.
type SidecarCleanupWorkerList struct {
	exportedList[*SidecarCleanupWorker]
}

// An empty list.
func NewSidecarCleanupWorkerList() *SidecarCleanupWorkerList {
	return &SidecarCleanupWorkerList{
		exportedList: *newExportedList[*SidecarCleanupWorker](),
	}
}

// §7.10's cleanup list as a revoke answer signs it, the one this device showed. json in the C ABI and js.
type SidecarCleanupList struct {
	// What config_admissions counts now, and the cap: 0 for a tier that does not include admissions, -1 for none.
	AdmissionCount int32 `json:"admission_count"`
	AdmissionCap   int32 `json:"admission_cap"`
	// Each member's page Workers but its newest.
	Workers *SidecarCleanupWorkerList `json:"workers"`
	// The revoke runs SignWithPresence.
	Presence bool `json:"presence,omitempty"`
}

// The item's decisions as a bound list.
func newSidecarDecisionList(decisions []string) *StringList {
	list := NewStringList()
	list.addAll(decisions...)
	return list
}

// What a revocation did, as a result names it.
type sidecarRevokeOutcome struct {
	// The SidecarRevoke's position; 0 while a proposal pends or a post's answer is lost.
	position uint64
	// §7.9's proposal_id while k > 1 pends.
	proposalId []byte
	// The statements are signed and kept, and a post's answer was lost: they go again with the same bytes.
	pending bool
}

// What SidecarApprovals asks of its backend: the core (sidecarApprovalsCoreBackend), ur daemon's control methods
// (sidecarApprovalsRemoteBackend), or a test's.
type sidecarApprovalsBackend interface {
	// The items waiting for this device's decision. preview shows each cleanup's list as well, which this device then
	// keeps as the one a revoke signs; Decide's own read never does, so it signs what the person was shown.
	pending(ctx context.Context, preview bool) ([]*SidecarApprovalItem, error)
	// Decides item, which pending answered: the result's Value, empty for none.
	decide(ctx context.Context, item *SidecarApprovalItem, decision string, signer SidecarPresenceSigner) (string, error)
	devices(ctx context.Context) ([]*SidecarDevice, error)
	// The plan, or a refusal in it.
	previewRevoke(ctx context.Context, sidecarId sidecar.Id, reason string, lostAt time.Time, sinceN uint64) (*SidecarRevokePlan, error)
	revoke(ctx context.Context, planId sidecar.Id, revokeKeyIds [][]byte, signer SidecarPresenceSigner) (*sidecarRevokeOutcome, error)
}

// The approvals of one Sidecar. Safe for concurrent use.
type SidecarApprovals struct {
	ctx      context.Context
	cancel   context.CancelFunc
	backend  sidecarApprovalsBackend
	settings *sidecarApprovalsSettings
	signer   SidecarPresenceSigner

	listeners *connect.CallbackList[SidecarApprovalListener]
	// wakes the listeners' reader; never blocks a sender
	wake chan struct{}

	stateLock sync.Mutex
	// the ids this Sidecar is deciding, and those it decided, oldest first
	deciding map[sidecar.Id]bool
	decided  []sidecar.Id
	// whether the listeners' reader runs, and what stops the Sidecar's events from waking it
	watching     bool
	removeEvents func()
}

// SidecarApprovals on backend until ctx ends or close.
func newSidecarApprovals(ctx context.Context, backend sidecarApprovalsBackend, settings *sidecarApprovalsSettings, signer SidecarPresenceSigner) *SidecarApprovals {
	cancelCtx, cancel := context.WithCancel(ctx)
	return &SidecarApprovals{
		ctx:       cancelCtx,
		cancel:    cancel,
		backend:   backend,
		settings:  settings,
		signer:    signer,
		listeners: connect.NewCallbackList[SidecarApprovalListener](),
		wake:      make(chan struct{}, 1),
		deciding:  map[sidecar.Id]bool{},
	}
}

// Ends every call in flight, which answers UNAVAILABLE, and the listeners' reader. Safe to call again.
func (self *SidecarApprovals) close() {
	self.cancel()
	self.stateLock.Lock()
	removeEvents := self.removeEvents
	self.removeEvents = nil
	self.stateLock.Unlock()
	if removeEvents != nil {
		removeEvents()
	}
}

// This Sidecar's approvals (§18.5), on either backend; nil once the Sidecar closed.
func (self *Sidecar) GetApprovals() *SidecarApprovals {
	approvals, err := traceWithReturn(func() *SidecarApprovals {
		local, remote := self.backends()
		existing, settings, backend, closed := func() (*SidecarApprovals, *sidecarApprovalsSettings, sidecarApprovalsBackend, bool) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			return self.approvals, self.approvalsSettings, self.approvalsBackend, self.closed
		}()
		if closed {
			return nil
		}
		if existing != nil {
			return existing
		}
		if settings == nil {
			settings = defaultSidecarApprovalsSettings()
		}
		if backend == nil {
			switch {
			case local != nil:
				backend = &sidecarApprovalsCoreBackend{local: local}
			case remote != nil:
				backend = &sidecarApprovalsRemoteBackend{remote: remote}
			default:
				return nil
			}
		}
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return nil
		}
		if self.approvals == nil {
			approvals := newSidecarApprovals(self.ctx, backend, settings, self.presenceSigner)
			// the events that may change what waits wake its listeners
			approvals.removeEvents = self.events.addListener(&sidecarApprovalsEventListener{approvals: approvals})
			self.approvals = approvals
		}
		return self.approvals
	})
	if err != nil {
		return nil
	}
	return approvals
}

// The Sidecar's events, as the approvals hear them.
type sidecarApprovalsEventListener struct {
	approvals *SidecarApprovals
}

// See SidecarEventListener: an event of a kind that may change what waits wakes the listeners' reader.
func (self *sidecarApprovalsEventListener) SidecarEventReceived(event *SidecarEvent) {
	if event != nil && slices.Contains(sidecarApprovalEventKinds, event.Kind) {
		self.approvals.wakeListeners()
	}
}

// The call's context ended or the approvals closed.
func (self *SidecarApprovals) callErr() error {
	if self.ctx.Err() != nil {
		return errSidecarApprovalsUnavailable
	}
	return nil
}

// Runs call on a goroutine of its own within CallTimeout and answers callback once with what it answers, or with
// call's panic as the error.
func sidecarApprovalsAnswer[T any](self *SidecarApprovals, call func(ctx context.Context) T, callback connect.ApiCallback[T]) {
	go func() {
		result, err := traceWithReturnError(func() (T, error) {
			ctx, cancel := context.WithTimeout(self.ctx, self.settings.CallTimeout)
			defer cancel()
			return call(ctx), nil
		})
		if callback != nil {
			callback.Result(result, err)
		}
	}()
}

// The items that wait for this device's decision now (the file's pending row), each with the decisions this device may
// make on it, read within ReadTimeout on the caller's thread. Nil when they could not be read, so an answer that is not
// known never reads as none.
func (self *SidecarApprovals) GetPending() *SidecarApprovalList {
	list, err := traceWithReturn(func() *SidecarApprovalList {
		return self.readPending()
	})
	if err != nil {
		return nil
	}
	return list
}

// GetPending's read: the backend's items with their cleanup lists shown, nil when they could not be read.
func (self *SidecarApprovals) readPending() *SidecarApprovalList {
	if self.callErr() != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(self.ctx, self.settings.ReadTimeout)
	defer cancel()
	items, err := self.backend.pending(ctx, true)
	if err != nil {
		return nil
	}
	list := NewSidecarApprovalList()
	for _, item := range items {
		list.Add(item)
	}
	return list
}

// Calls listener with GetPending's list whenever it may have changed (the file's listen row), on a Go thread, until
// the answer closes. Nothing is read while no listener is added.
func (self *SidecarApprovals) AddPendingListener(listener SidecarApprovalListener) Sub {
	sub, err := traceWithReturn(func() Sub {
		if listener == nil || self.callErr() != nil {
			return newSub(func() {})
		}
		listenerId := self.listeners.Add(listener)
		start := func() bool {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if self.watching {
				return false
			}
			self.watching = true
			return true
		}()
		if start {
			go self.watchPending()
		}
		// the new listener hears where things stand
		self.wakeListeners()
		var once sync.Once
		return newSub(func() {
			once.Do(func() {
				self.listeners.Remove(listenerId)
			})
		})
	})
	if err != nil {
		return newSub(func() {})
	}
	return sub
}

// Wakes the listeners' reader without waiting.
func (self *SidecarApprovals) wakeListeners() {
	select {
	case self.wake <- struct{}{}:
	default:
	}
}

// The listeners' reader: at each wake, and every PendingRefreshInterval, the pending list read once and handed to each
// listener added then, each call under trace, until the approvals close.
func (self *SidecarApprovals) watchPending() {
	var refresh <-chan time.Time
	if 0 < self.settings.PendingRefreshInterval {
		ticker := time.NewTicker(self.settings.PendingRefreshInterval)
		defer ticker.Stop()
		refresh = ticker.C
	}
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-self.wake:
		case <-refresh:
		}
		listeners := self.listeners.Get()
		if len(listeners) == 0 {
			continue
		}
		items := func() []*SidecarApprovalItem {
			ctx, cancel := context.WithTimeout(self.ctx, self.settings.ReadTimeout)
			defer cancel()
			items, err := self.backend.pending(ctx, true)
			if err != nil {
				return nil
			}
			return items
		}()
		if items == nil {
			// nothing could be read: the listeners keep the list they have
			continue
		}
		for _, listener := range listeners {
			if self.ctx.Err() != nil {
				return
			}
			// each listener its own list, so one that edits it changes nothing another sees
			list := NewSidecarApprovalList()
			for _, item := range items {
				copied := *item
				list.Add(&copied)
			}
			trace(func() {
				listener.ApprovalsChanged(list)
			})
		}
	}
}

// Takes id for one Decide: false when this Sidecar decided it or is deciding it.
func (self *SidecarApprovals) claim(id sidecar.Id) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.deciding[id] || slices.Contains(self.decided, id) {
		return false
	}
	self.deciding[id] = true
	return true
}

// Ends id's Decide: decided keeps it from another, the oldest decided id going past DecidedMaxCount; else another may
// try again.
func (self *SidecarApprovals) release(id sidecar.Id, decided bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.deciding, id)
	if !decided {
		return
	}
	self.decided = append(self.decided, id)
	if overflow := len(self.decided) - max(self.settings.DecidedMaxCount, 1); 0 < overflow {
		self.decided = slices.Delete(self.decided, 0, overflow)
	}
}

// Decides one item: decision is approve, deny or revoke, one the item offers this device (the file's decide row). A
// person's act in the app's UI. The result is Ok once the decision is made, its statements signed and kept even where a
// post's answer was lost, which then goes again on its own; a refusal carries one of the SidecarCode values.
func (self *SidecarApprovals) Decide(approvalId *Id, decision string, callback SidecarResultCallback) {
	trace(func() {
		var id sidecar.Id
		named := approvalId != nil
		if named {
			id = sidecar.Id(approvalId.id)
		}
		sidecarApprovalsAnswer(self, func(ctx context.Context) *SidecarResult {
			if err := self.callErr(); err != nil {
				return sidecarApprovalsResultOf("", err)
			}
			if !named {
				return sidecarApprovalsResultOf("", errSidecarApprovalsArgument)
			}
			if !self.claim(id) {
				return sidecarApprovalsResultOf("", errSidecarApprovalDecided)
			}
			decided := false
			defer func() {
				self.release(id, decided)
				self.wakeListeners()
			}()
			items, err := self.backend.pending(ctx, false)
			if err != nil {
				return sidecarApprovalsResultOf("", err)
			}
			var item *SidecarApprovalItem
			for _, each := range items {
				if each.ApprovalId != nil && sidecar.Id(each.ApprovalId.id) == id {
					item = each
					break
				}
			}
			if item == nil {
				return sidecarApprovalsResultOf("", errSidecarApprovalUnknown)
			}
			if item.Decisions == nil || !item.Decisions.Contains(decision) {
				return sidecarApprovalsResultOf("", errSidecarApprovalDecision)
			}
			value, err := self.backend.decide(ctx, item, decision, self.signer)
			// a statement signed and kept is a decision made, whatever its post's answer
			decided = err == nil || errors.Is(err, sidecar.ErrSidecarActPending)
			return sidecarApprovalsResultOf(value, err)
		}, connect.ApiCallback[*SidecarResult](callback))
	})
}

// This member's admitted devices, this one first (§5.4's Devices), read within ReadTimeout on the caller's thread. Nil
// when they could not be read: locked, unenrolled, no verified state yet, or no daemon answering.
func (self *SidecarApprovals) GetDevices() *SidecarDeviceList {
	list, err := traceWithReturn(func() *SidecarDeviceList {
		if self.callErr() != nil {
			return nil
		}
		ctx, cancel := context.WithTimeout(self.ctx, self.settings.ReadTimeout)
		defer cancel()
		devices, err := self.backend.devices(ctx)
		if err != nil {
			return nil
		}
		list := NewSidecarDeviceList()
		for _, device := range devices {
			list.Add(device)
		}
		return list
	})
	if err != nil {
		return nil
	}
	return list
}

// §5.6's plan of revoking sidecarId, another of this member's devices: reason lost or compromise; for compromise,
// lostAtMillis the loss time the person gave, whose first entry at or after it is the proposed since_n, and sinceN a
// position the person moved it to, 0 for the proposed one; lost ignores both. Signs nothing. A refusal answers a plan
// whose Code names it.
func (self *SidecarApprovals) PreviewRevokeDevice(sidecarId *Id, reason string, lostAtMillis int64, sinceN int64, callback SidecarRevokePlanCallback) {
	trace(func() {
		var id sidecar.Id
		named := sidecarId != nil
		if named {
			id = sidecar.Id(sidecarId.id)
		}
		sidecarApprovalsAnswer(self, func(ctx context.Context) *SidecarRevokePlan {
			if err := self.callErr(); err != nil {
				return sidecarRevokeRefusalOf(err)
			}
			if !named || sinceN < 0 || lostAtMillis < 0 || (reason != SidecarRevokeLost && reason != SidecarRevokeCompromise) {
				return sidecarRevokeRefusalOf(errSidecarApprovalsArgument)
			}
			lostAt := time.Time{}
			if lostAtMillis != 0 {
				lostAt = time.UnixMilli(lostAtMillis)
			}
			plan, err := self.backend.previewRevoke(ctx, id, reason, lostAt, uint64(sinceN))
			if err != nil {
				return sidecarRevokeRefusalOf(err)
			}
			return plan
		}, connect.ApiCallback[*SidecarRevokePlan](callback))
	})
}

// Signs the plan planId names, which PreviewRevokeDevice answered and the person confirmed after the plan's copy
// (D-SDK-30): SignWithPresence over the act's bytes where the plan's Presence is set, then §5.6's statements, each with
// its countersignature. revokeKeys are the plan's key items the person left selected. The result names the
// SidecarRevoke's entry, its position in decimal, or authority_pending:<proposal_id> while other AdminSet devices'
// cosigns are due; Ok with no Value is a revocation signed whose post goes again. Refusals carry §5.6's REVOKE_ codes.
func (self *SidecarApprovals) RevokeDevice(planId *Id, revokeKeys *SidecarRevokeItemList, callback SidecarResultCallback) {
	trace(func() {
		var id sidecar.Id
		named := planId != nil
		if named {
			id = sidecar.Id(planId.id)
		}
		keyIds := [][]byte{}
		if revokeKeys != nil {
			for _, item := range revokeKeys.getAll() {
				if item != nil && 0 < len(item.Id) {
					keyIds = append(keyIds, slices.Clone(item.Id))
				}
			}
		}
		sidecarApprovalsAnswer(self, func(ctx context.Context) *SidecarResult {
			if err := self.callErr(); err != nil {
				return sidecarApprovalsResultOf("", err)
			}
			if !named {
				return sidecarApprovalsResultOf("", errSidecarApprovalsArgument)
			}
			outcome, err := self.backend.revoke(ctx, id, keyIds, self.signer)
			value := ""
			if outcome != nil {
				switch {
				case 0 < len(outcome.proposalId):
					value = sidecarAuthorityPendingPrefix + hex.EncodeToString(outcome.proposalId)
				case outcome.position != 0:
					value = strconv.FormatUint(outcome.position, 10)
				}
				if err == nil && outcome.pending {
					err = sidecar.ErrSidecarActPending
				}
			}
			self.wakeListeners()
			return sidecarApprovalsResultOf(value, err)
		}, connect.ApiCallback[*SidecarResult](callback))
	})
}

// The code a refusal carries (the SidecarCode values), "" for none.
func sidecarApprovalsCodeOf(err error) string {
	var revokeErr *sidecar.DeviceRevokeError
	var refusal *sidecarControlError
	var deviceRefusal *sidecarDeviceRefusalError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &deviceRefusal):
		return deviceRefusal.code
	case errors.As(err, &revokeErr):
		return revokeErr.Code
	case errors.Is(err, errSidecarApprovalsUnavailable), errors.Is(err, sidecar.ErrSidecarClosed),
		errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, errSidecarRemoteOffline), errors.Is(err, errSidecarRemoteClosed),
		errors.Is(err, sidecar.ErrSidecarActNoAuthority), errors.Is(err, sidecar.ErrSidecarNoIdentity):
		return SidecarCodeUnavailable
	case errors.Is(err, sidecar.ErrSidecarDevicesPrecondition):
		return SidecarCodeFailedPrecondition
	case errors.Is(err, errSidecarApprovalsArgument), errors.Is(err, sidecar.ErrSidecarRevokeReason),
		errors.Is(err, sidecar.ErrSidecarRevokeKey):
		return SidecarCodeInvalidArgument
	case errors.Is(err, sidecar.ErrSidecarActPresence), errors.Is(err, errSidecarPresenceNoSigner),
		errors.Is(err, errSidecarPresenceRefused), errors.Is(err, errSidecarPresenceEvidence):
		return SidecarCodeRevokeCancelled
	case errors.Is(err, sidecar.ErrSidecarActRefused):
		return SidecarCodeRevokeRefused
	case errors.Is(err, errSidecarApprovalDecided):
		return SidecarCodeApprovalDecided
	case errors.Is(err, errSidecarApprovalUnknown), errors.Is(err, sidecar.ErrSidecarApprovalUnknown),
		errors.Is(err, sidecar.ErrDirectoryRequestDecided), errors.Is(err, sidecar.ErrSidecarRevokeProposalUnknown):
		return SidecarCodeApprovalUnknown
	case errors.Is(err, errSidecarApprovalDecision), errors.Is(err, sidecar.ErrSidecarApprovalDecision),
		errors.Is(err, sidecar.ErrSidecarApprovalNotEligible), errors.Is(err, sidecar.ErrApprovalOwnRequest),
		errors.Is(err, sidecar.ErrDirectoryDecideNotEligible), errors.Is(err, sidecar.ErrDirectoryReadOnly):
		return SidecarCodeApprovalNotEligible
	case errors.Is(err, sidecar.ErrSidecarApprovalAuthority):
		return SidecarCodeApprovalAuthority
	case errors.As(err, &refusal):
		return sidecarApprovalsRemoteCodeOf(refusal)
	}
	return SidecarCodeApprovalRefused
}

// The code of a daemon's refusal, which carries its handler's text and the daemon's own codes: FAILED_PRECONDITION by
// code or by the core's text, a REVOKE_ code or the core's approval errors by their text, else refused.
func sidecarApprovalsRemoteCodeOf(refusal *sidecarControlError) string {
	message := refusal.Message
	if refusal.Code == sidecarControlCodeFailedPrecondition || strings.HasPrefix(message, SidecarCodeFailedPrecondition) {
		return SidecarCodeFailedPrecondition
	}
	for _, code := range sidecarRevokeCodes {
		if strings.Contains(message, code) {
			return code
		}
	}
	texts := []struct {
		err  error
		code string
	}{
		{sidecar.ErrSidecarApprovalUnknown, SidecarCodeApprovalUnknown},
		{sidecar.ErrSidecarApprovalDecision, SidecarCodeApprovalNotEligible},
		{sidecar.ErrSidecarApprovalNotEligible, SidecarCodeApprovalNotEligible},
		{sidecar.ErrSidecarApprovalAuthority, SidecarCodeApprovalAuthority},
		{sidecar.ErrSidecarActPresence, SidecarCodeRevokeCancelled},
		{sidecar.ErrSidecarActRefused, SidecarCodeRevokeRefused},
		{sidecar.ErrSidecarActNoAuthority, SidecarCodeUnavailable},
	}
	for _, text := range texts {
		if strings.Contains(message, text.err.Error()) {
			return text.code
		}
	}
	return SidecarCodeApprovalRefused
}

// §5.6's REVOKE_ codes, as a daemon's text may name one.
var sidecarRevokeCodes = []string{
	SidecarCodeRevokeSelf,
	SidecarCodeRevokeNotOwn,
	SidecarCodeRevokeNotCapable,
	SidecarCodeRevokeCancelled,
	SidecarCodeRevokeSince,
	SidecarCodeRevokeParksSelf,
	SidecarCodeRevokeStale,
	SidecarCodeRevokeRefused,
}

// The daemon's FAILED_PRECONDITION, §5.8's network-scoped call with no network to act on.
const sidecarControlCodeFailedPrecondition = -32004

// A result for a call that answered value or err: Ok on success, and on statements signed and kept whose post goes
// again (ErrSidecarActPending), which is a decision made; else a refusal with its code. approval_pending:<id> is
// sidecarResultOf's.
func sidecarApprovalsResultOf(value string, err error) *SidecarResult {
	if errors.Is(err, sidecar.ErrSidecarActPending) {
		return &SidecarResult{Ok: true, Value: value, Message: "signed; its post goes again until it lands"}
	}
	result := sidecarResultOf(value, err)
	if err != nil && !strings.HasPrefix(result.Value, sidecarApprovalPendingPrefix) {
		result.Value = ""
		result.Code = sidecarApprovalsCodeOf(err)
	}
	return result
}

// A plan carrying a refusal.
func sidecarRevokeRefusalOf(err error) *SidecarRevokePlan {
	plan := &SidecarRevokePlan{Code: sidecarApprovalsCodeOf(err), Message: err.Error()}
	var revokeErr *sidecar.DeviceRevokeError
	var deviceRefusal *sidecarDeviceRefusalError
	switch {
	case errors.As(err, &deviceRefusal):
		plan.ParksSelfPosition = int64(deviceRefusal.position)
	case errors.As(err, &revokeErr):
		plan.ParksSelfPosition = int64(revokeErr.Position)
	}
	if plan.Code != SidecarCodeRevokeParksSelf {
		plan.ParksSelfPosition = 0
	}
	return plan
}

// §5.6's refusal as a daemon's reply carried it.
type sidecarDeviceRefusalError struct {
	code     string
	position uint64
}

func (self *sidecarDeviceRefusalError) Error() string {
	return self.code
}

// The error of a daemon's refusal, nil for none.
func sidecarDeviceRefusalErrOf(refusal *sidecarControlDeviceRefusal) error {
	if refusal == nil {
		return nil
	}
	return &sidecarDeviceRefusalError{code: refusal.Code, position: refusal.Position}
}

// sidecarApprovalsBackend over the core: its approvals, its approver of requests, its directory and its device calls,
// each asked for at each call, since the core's record and directory come after the Sidecar opens.
type sidecarApprovalsCoreBackend struct {
	local *sidecar.Sidecar
}

// The core's kind for an app's: the cleanup is §18.5's admissions_near_cap.
func sidecarApprovalKindOfCore(kind string) string {
	if kind == sidecar.SidecarApprovalCleanup {
		return SidecarApprovalKindAdmissionsNearCap
	}
	return kind
}

// See sidecarApprovalsBackend: the core's authority items first, then its requests where the record is bound, then
// the access requests this device decides where the network has a directory. The core's own refusal of the authority
// items is the answer; a record or a directory that is not there, or that cannot be read now, lists nothing of its
// kind, so the authority items still reach the person.
func (self *sidecarApprovalsCoreBackend) pending(ctx context.Context, preview bool) ([]*SidecarApprovalItem, error) {
	coreItems, err := self.local.PendingApprovals()
	if err != nil {
		return nil, err
	}
	items := []*SidecarApprovalItem{}
	for _, coreItem := range coreItems {
		item := &SidecarApprovalItem{
			ApprovalId:   newId([16]byte(coreItem.ApprovalId)),
			Kind:         sidecarApprovalKindOfCore(coreItem.Kind),
			SidecarId:    sidecarIdOf(coreItem.SidecarId),
			MemberId:     sidecarIdOf(coreItem.MemberId),
			ProposalId:   sidecarBytesOf(coreItem.ProposalId),
			Position:     int64(coreItem.Position),
			RaisedMillis: sidecarMillisOf(coreItem.RaisedAt),
			SafetyNumber: coreItem.SafetyNumber,
			Decisions:    newSidecarDecisionList(coreItem.Decisions),
			source:       sidecarApprovalSourceAuthority,
		}
		if preview && coreItem.Kind == sidecar.SidecarApprovalCleanup {
			// the list a revoke signs, kept by the core as the one shown; a list that cannot be read now leaves the
			// item without one, and its revoke refuses until it is shown
			if list, err := self.local.PreviewCleanup(ctx, coreItem.ApprovalId); err == nil {
				item.Cleanup = sidecarCleanupListOf(list)
			}
		}
		items = append(items, item)
	}
	if approver, err := self.local.ApprovalApprover(); err == nil {
		if requests, err := approver.Pending(ctx); err == nil {
			for _, request := range requests {
				items = append(items, sidecarRequestItemOf(request))
			}
		}
	}
	directory := &sidecarDirectoryCoreBackend{local: self.local}
	if directory.standing() {
		if stubs, err := directory.requests(ctx, &sidecar.DirectoryRequestFilter{Decidable: true}); err == nil {
			for _, stub := range stubs {
				if item := sidecarAccessItemOf(stub); item != nil {
					items = append(items, item)
				}
			}
		}
	}
	return items, nil
}

// The item of one of §16.11's access requests this device decides now, approve or deny; nil for one it does not
// decide.
func sidecarAccessItemOf(stub *sidecar.DirectoryRequestStub) *SidecarApprovalItem {
	if stub == nil || !stub.Decidable {
		return nil
	}
	requestId, err := sidecar.ParseId(stub.RequestId)
	if err != nil {
		return nil
	}
	return &SidecarApprovalItem{
		ApprovalId:    newId([16]byte(requestId)),
		Kind:          SidecarApprovalKindAccessRequest,
		SidecarId:     sidecarDirectoryIdOfText(stub.RequesterSidecarId),
		AccessRequest: sidecarAccessRequestOf(stub),
		Decisions:     newSidecarDecisionList([]string{SidecarDecisionApprove, SidecarDecisionDeny}),
		source:        sidecarApprovalSourceAccess,
	}
}

// The item of one of §16.8's requests: an approval id, its render's text, the channel this device signs it on, and the
// decisions it offers; or, shown refused, why, with none.
func sidecarRequestItemOf(request *sidecar.SidecarRequestItem) *SidecarApprovalItem {
	item := &SidecarApprovalItem{
		Kind:      SidecarApprovalKindRequest,
		Refused:   request.Refused,
		Channel:   sidecarApprovalChannelName(request.Channel),
		Decisions: newSidecarDecisionList(request.Decisions),
		source:    sidecarApprovalSourceRequest,
	}
	if request.Render != nil {
		item.ApprovalId = newId([16]byte(request.ApprovalId))
		item.Action = request.Render.KindName
		item.Template = request.Render.Template
		item.Text = request.Render.Text()
		item.SidecarId = sidecarIdOfValue(request.Render.RequesterSidecarId)
	}
	return item
}

// The bound id of a core id, nil for the zero id.
func sidecarIdOfValue(id sidecar.Id) *Id {
	if id == (sidecar.Id{}) {
		return nil
	}
	return newId([16]byte(id))
}

// A cleanup list as an item carries it.
func sidecarCleanupListOf(list *sidecar.SidecarCleanupList) *SidecarCleanupList {
	bound := &SidecarCleanupList{
		AdmissionCount: int32(list.AdmissionCount),
		AdmissionCap:   int32(list.AdmissionCap),
		Workers:        NewSidecarCleanupWorkerList(),
		Presence:       list.Presence,
	}
	for _, member := range list.Members {
		for _, worker := range member.Workers {
			bound.Workers.Add(&SidecarCleanupWorker{
				SidecarId:        newId([16]byte(worker.SidecarId)),
				MemberId:         newId([16]byte(member.MemberId)),
				AdmittedPosition: int64(worker.AdmittedAt),
				AdmittedMillis:   sidecarMillisOf(worker.AdmittedTime),
				LastChangeMillis: sidecarMillisOf(worker.LastChange),
				Signatures:       int32(worker.Signatures),
			})
		}
	}
	return bound
}

// See sidecarApprovalsBackend: the item's own path, as the person's confirmation, under the app's presence check where
// the log covers what the decision signs.
func (self *sidecarApprovalsCoreBackend) decide(ctx context.Context, item *SidecarApprovalItem, decision string, signer SidecarPresenceSigner) (string, error) {
	id := sidecar.Id(item.ApprovalId.id)
	check := sidecarPresenceCheckOf(signer)
	switch item.source {
	case sidecarApprovalSourceAuthority:
		return "", self.local.DecideApproval(ctx, id, decision, sidecar.SidecarActConfirmed, check)
	case sidecarApprovalSourceRequest:
		approver, err := self.local.ApprovalApprover()
		if err != nil {
			return "", err
		}
		return "", approver.Decide(ctx, id, &sidecar.SidecarRequestDecision{Decision: decision}, sidecar.SidecarActConfirmed, check)
	case sidecarApprovalSourceAccess:
		client := self.local.Directory()
		if client == nil {
			return "", errSidecarApprovalUnknown
		}
		_, err := client.Decide(ctx, &sidecar.DirectoryDecideArgs{RequestId: id, Approve: decision == SidecarDecisionApprove})
		return "", err
	}
	return "", errSidecarApprovalUnknown
}

// See sidecarApprovalsBackend.
func (self *sidecarApprovalsCoreBackend) devices(ctx context.Context) ([]*SidecarDevice, error) {
	memberDevices, err := self.local.Devices()
	if err != nil {
		return nil, err
	}
	devices := []*SidecarDevice{}
	for _, memberDevice := range memberDevices {
		devices = append(devices, &SidecarDevice{
			SidecarId:        newId([16]byte(memberDevice.SidecarId)),
			Name:             memberDevice.Name,
			This:             memberDevice.This,
			ReadOnly:         memberDevice.ReadOnly,
			Ephemeral:        memberDevice.Ephemeral,
			State:            memberDevice.State,
			AdmittedMillis:   sidecarMillisOf(memberDevice.AdmittedAt),
			LastChangeMillis: sidecarMillisOf(memberDevice.LastChange),
		})
	}
	return devices, nil
}

// See sidecarApprovalsBackend.
func (self *sidecarApprovalsCoreBackend) previewRevoke(ctx context.Context, sidecarId sidecar.Id, reason string, lostAt time.Time, sinceN uint64) (*SidecarRevokePlan, error) {
	plan, err := self.local.PreviewDeviceRevoke(ctx, sidecarId, reason, lostAt, sinceN)
	if err != nil {
		return nil, err
	}
	bound := &SidecarRevokePlan{
		PlanId:      newId([16]byte(plan.PlanId)),
		SidecarId:   newId([16]byte(plan.SidecarId)),
		Name:        plan.Name,
		Reason:      plan.Reason,
		SinceN:      int64(plan.SinceN),
		SinceMillis: sidecarMillisOf(plan.SinceTime),
		Items:       NewSidecarRevokeItemList(),
		Signatures:  int32(plan.Signatures),
		Presence:    plan.Presence,
	}
	for _, item := range plan.Items {
		bound.Items.Add(&SidecarRevokeItem{
			Effect:   item.Effect,
			Kind:     item.Kind,
			Name:     item.Name,
			Id:       sidecarBytesOf(item.Id),
			Position: int64(item.Position),
		})
	}
	return bound, nil
}

// See sidecarApprovalsBackend: the core signs, as the person's confirmation, under the app's presence check.
func (self *sidecarApprovalsCoreBackend) revoke(ctx context.Context, planId sidecar.Id, revokeKeyIds [][]byte, signer SidecarPresenceSigner) (*sidecarRevokeOutcome, error) {
	revocation, err := self.local.RevokeDevicePlan(ctx, planId, revokeKeyIds, sidecar.SidecarActConfirmed, sidecarPresenceCheckOf(signer))
	if revocation == nil {
		return nil, err
	}
	outcome := &sidecarRevokeOutcome{proposalId: sidecarBytesOf(revocation.ProposalId), pending: revocation.Pending}
	if revocation.Entry != nil {
		outcome.position = revocation.Entry.GetVersion()
	}
	return outcome, err
}

// The control methods the remote backend calls, as their owners named them: cli's ur approve, and the root's devices
// methods with CORE-M8-03's two calls of a revocation (sidecar_presence.go). The approvals methods take no network:
// the daemon decides on its only network's sidecar.
const (
	sidecarControlMethodApprovalsList        = "approvals.list"
	sidecarControlMethodApprovalsDecide      = "approvals.decide"
	sidecarControlMethodApprovalsCleanupList = "approvals.cleanup_list"
	sidecarControlMethodApprovalsRequests    = "approvals.requests"
	sidecarControlMethodApprovalsRequest     = "approvals.request"
	sidecarControlMethodDevicesList          = "devices.list"
	sidecarControlMethodDevicesPreview       = "devices.preview"
	sidecarControlMethodDevicesRevoke        = "devices.revoke"
	sidecarControlMethodDevicesRevokeBegin   = "devices.revoke.begin"
	sidecarControlMethodDevicesRevokeFinish  = "devices.revoke.finish"
)

// JSON-RPC's method not found: a daemon from before a method.
const sidecarControlCodeMethodNotFound = -32601

// approvals.list's item, as cli's ur approve answers it.
type sidecarControlApprovalItem struct {
	ApprovalId   *Id      `json:"approval_id"`
	Kind         string   `json:"kind"`
	SidecarId    *Id      `json:"sidecar_id,omitempty"`
	MemberId     *Id      `json:"member_id,omitempty"`
	Position     uint64   `json:"position,omitempty"`
	SafetyNumber string   `json:"safety_number,omitempty"`
	Decisions    []string `json:"decisions"`
}

// approvals.list's reply.
type sidecarControlApprovalsReply struct {
	Items []*sidecarControlApprovalItem `json:"items"`
}

// approvals.decide's params, and approvals.cleanup_list's with no decision.
type sidecarControlDecideParams struct {
	ApprovalId *Id    `json:"approval_id"`
	Decision   string `json:"decision,omitempty"`
}

// approvals.cleanup_list's reply.
type sidecarControlCleanupListReply struct {
	AdmissionCount int                            `json:"admission_count"`
	AdmissionCap   int                            `json:"admission_cap"`
	Members        []*sidecarControlCleanupMember `json:"members"`
	Presence       bool                           `json:"presence,omitempty"`
}

// One member's part of the cleanup list.
type sidecarControlCleanupMember struct {
	MemberId *Id                            `json:"member_id"`
	Workers  []*sidecarControlCleanupWorker `json:"workers"`
}

// One page Worker of the cleanup list.
type sidecarControlCleanupWorker struct {
	SidecarId      *Id    `json:"sidecar_id"`
	AdmittedAt     uint64 `json:"admitted_at"`
	AdmittedTimeMs int64  `json:"admitted_time_ms,omitempty"`
	LastChangeMs   int64  `json:"last_change_ms,omitempty"`
	Signatures     int    `json:"signatures"`
}

// approvals.requests' item.
type sidecarControlRequestItem struct {
	ApprovalId *Id      `json:"approval_id,omitempty"`
	EntryId    *Id      `json:"entry_id"`
	Kind       string   `json:"kind,omitempty"`
	Template   bool     `json:"template,omitempty"`
	Text       string   `json:"text,omitempty"`
	Refused    string   `json:"refused,omitempty"`
	Channel    string   `json:"channel,omitempty"`
	Decisions  []string `json:"decisions"`
}

// approvals.requests' reply.
type sidecarControlRequestsReply struct {
	Items []*sidecarControlRequestItem `json:"items"`
}

// approvals.request's params: one item, one decision.
type sidecarControlRequestDecideParams struct {
	ApprovalId *Id    `json:"approval_id"`
	Decision   string `json:"decision"`
}

// §5.6's refusal as the devices methods carry it.
type sidecarControlDeviceRefusal struct {
	Code     string `json:"code"`
	Position uint64 `json:"position,omitempty"`
}

// devices.list's params, and every devices method's network.
type sidecarControlDevicesParams struct {
	Network string `json:"network,omitempty"`
}

// One device as devices.list answers it.
type sidecarControlDevice struct {
	SidecarId    *Id    `json:"sidecar_id"`
	Name         string `json:"name,omitempty"`
	This         bool   `json:"this,omitempty"`
	ReadOnly     bool   `json:"read_only,omitempty"`
	Ephemeral    bool   `json:"ephemeral,omitempty"`
	State        string `json:"state"`
	AdmittedAtMs int64  `json:"admitted_at_ms,omitempty"`
	LastChangeMs int64  `json:"last_change_ms,omitempty"`
}

// devices.list's reply.
type sidecarControlDevicesListReply struct {
	Devices []*sidecarControlDevice `json:"devices"`
}

// devices.preview's params.
type sidecarControlDevicesPreviewParams struct {
	Network  string `json:"network,omitempty"`
	Device   string `json:"device"`
	Reason   string `json:"reason"`
	LostAtMs int64  `json:"lost_at_ms,omitempty"`
	SinceN   uint64 `json:"since_n,omitempty"`
}

// One row of a plan as a reply carries it, its id in hex.
type sidecarControlDeviceRevokeItem struct {
	Effect   string `json:"effect"`
	Kind     string `json:"kind"`
	Id       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Position uint64 `json:"position,omitempty"`
}

// A plan as devices.preview answers it.
type sidecarControlDeviceRevokePlan struct {
	PlanId      *Id                               `json:"plan_id"`
	SidecarId   *Id                               `json:"sidecar_id"`
	Name        string                            `json:"name,omitempty"`
	Reason      string                            `json:"reason"`
	SinceN      uint64                            `json:"since_n,omitempty"`
	SinceTimeMs int64                             `json:"since_time_ms,omitempty"`
	Items       []*sidecarControlDeviceRevokeItem `json:"items"`
	Signatures  int                               `json:"signatures"`
	Presence    bool                              `json:"presence,omitempty"`
}

// devices.preview's reply.
type sidecarControlDevicesPreviewReply struct {
	Plan    *sidecarControlDeviceRevokePlan `json:"plan,omitempty"`
	Refusal *sidecarControlDeviceRefusal    `json:"refusal,omitempty"`
}

// devices.revoke's params, and devices.revoke.begin's.
type sidecarControlDevicesRevokeParams struct {
	Network      string   `json:"network,omitempty"`
	PlanId       *Id      `json:"plan_id"`
	RevokeKeyIds []string `json:"revoke_key_ids,omitempty"`
}

// devices.revoke's reply, and devices.revoke.finish's.
type sidecarControlDevicesRevokeReply struct {
	Position      uint64                            `json:"position,omitempty"`
	ProposalId    string                            `json:"proposal_id,omitempty"`
	Joined        []*sidecarControlDeviceRevokeItem `json:"joined,omitempty"`
	RevokedKeyIds []string                          `json:"revoked_key_ids,omitempty"`
	Pending       bool                              `json:"pending,omitempty"`
	Refusal       *sidecarControlDeviceRefusal      `json:"refusal,omitempty"`
}

// sidecarApprovalsBackend over ur daemon's control methods (sidecar_backend.go). Reads go unconfirmed; a decision and
// a revocation, the person's act in the app's UI, go confirmed (D-CORE-13).
type sidecarApprovalsRemoteBackend struct {
	remote sidecarBackend
}

// The network the pairing bound, as a devices method's network argument.
func (self *sidecarApprovalsRemoteBackend) network() string {
	if named, ok := self.remote.(interface{ network() string }); ok {
		return named.network()
	}
	return ""
}

// See sidecarApprovalsBackend: approvals.list, each cleanup's list where preview asks for it, and approvals.requests.
func (self *sidecarApprovalsRemoteBackend) pending(ctx context.Context, preview bool) ([]*SidecarApprovalItem, error) {
	reply := &sidecarControlApprovalsReply{}
	if err := self.remote.call(ctx, sidecarControlMethodApprovalsList, nil, false, reply); err != nil {
		return nil, err
	}
	items := []*SidecarApprovalItem{}
	for _, replyItem := range reply.Items {
		if replyItem == nil || replyItem.ApprovalId == nil {
			continue
		}
		item := &SidecarApprovalItem{
			ApprovalId:   sidecarControlIdOf(replyItem.ApprovalId),
			Kind:         sidecarApprovalKindOfCore(replyItem.Kind),
			SidecarId:    sidecarControlIdOf(replyItem.SidecarId),
			MemberId:     sidecarControlIdOf(replyItem.MemberId),
			Position:     int64(replyItem.Position),
			SafetyNumber: replyItem.SafetyNumber,
			Decisions:    newSidecarDecisionList(replyItem.Decisions),
			source:       sidecarApprovalSourceAuthority,
		}
		if preview && replyItem.Kind == sidecar.SidecarApprovalCleanup {
			list := &sidecarControlCleanupListReply{}
			params := &sidecarControlDecideParams{ApprovalId: replyItem.ApprovalId}
			if err := self.remote.call(ctx, sidecarControlMethodApprovalsCleanupList, params, false, list); err == nil {
				item.Cleanup = sidecarCleanupListOfControl(list)
			}
		}
		items = append(items, item)
	}
	requests := &sidecarControlRequestsReply{}
	if err := self.remote.call(ctx, sidecarControlMethodApprovalsRequests, nil, false, requests); err != nil {
		var refusal *sidecarControlError
		// a daemon with no record serves no requests: it lists the authority items alone
		if !errors.As(err, &refusal) {
			return nil, err
		}
	}
	for _, request := range requests.Items {
		if request == nil {
			continue
		}
		items = append(items, &SidecarApprovalItem{
			ApprovalId: sidecarControlIdOf(request.ApprovalId),
			Kind:       SidecarApprovalKindRequest,
			Action:     request.Kind,
			Template:   request.Template,
			Channel:    request.Channel,
			Text:       request.Text,
			Refused:    request.Refused,
			Decisions:  newSidecarDecisionList(request.Decisions),
			source:     sidecarApprovalSourceRequest,
		})
	}
	return items, nil
}

// A cleanup list as approvals.cleanup_list answered it.
func sidecarCleanupListOfControl(list *sidecarControlCleanupListReply) *SidecarCleanupList {
	bound := &SidecarCleanupList{
		AdmissionCount: int32(list.AdmissionCount),
		AdmissionCap:   int32(list.AdmissionCap),
		Workers:        NewSidecarCleanupWorkerList(),
		Presence:       list.Presence,
	}
	for _, member := range list.Members {
		if member == nil {
			continue
		}
		for _, worker := range member.Workers {
			if worker == nil {
				continue
			}
			bound.Workers.Add(&SidecarCleanupWorker{
				SidecarId:        sidecarControlIdOf(worker.SidecarId),
				MemberId:         sidecarControlIdOf(member.MemberId),
				AdmittedPosition: int64(worker.AdmittedAt),
				AdmittedMillis:   worker.AdmittedTimeMs,
				LastChangeMillis: worker.LastChangeMs,
				Signatures:       int32(worker.Signatures),
			})
		}
	}
	return bound
}

// See sidecarApprovalsBackend: the daemon's Confirm method of the item's kind, confirmed, under the daemon's own
// presence prompt (the file's presence row).
func (self *sidecarApprovalsRemoteBackend) decide(ctx context.Context, item *SidecarApprovalItem, decision string, signer SidecarPresenceSigner) (string, error) {
	switch item.source {
	case sidecarApprovalSourceAuthority:
		params := &sidecarControlDecideParams{ApprovalId: item.ApprovalId, Decision: decision}
		return "", self.remote.call(ctx, sidecarControlMethodApprovalsDecide, params, true, nil)
	case sidecarApprovalSourceRequest:
		params := &sidecarControlRequestDecideParams{ApprovalId: item.ApprovalId, Decision: decision}
		return "", self.remote.call(ctx, sidecarControlMethodApprovalsRequest, params, true, nil)
	}
	return "", errSidecarApprovalUnknown
}

// See sidecarApprovalsBackend.
func (self *sidecarApprovalsRemoteBackend) devices(ctx context.Context) ([]*SidecarDevice, error) {
	reply := &sidecarControlDevicesListReply{}
	if err := self.remote.call(ctx, sidecarControlMethodDevicesList, &sidecarControlDevicesParams{Network: self.network()}, false, reply); err != nil {
		return nil, err
	}
	devices := []*SidecarDevice{}
	for _, device := range reply.Devices {
		if device == nil || device.SidecarId == nil {
			continue
		}
		devices = append(devices, &SidecarDevice{
			SidecarId:        sidecarControlIdOf(device.SidecarId),
			Name:             device.Name,
			This:             device.This,
			ReadOnly:         device.ReadOnly,
			Ephemeral:        device.Ephemeral,
			State:            device.State,
			AdmittedMillis:   device.AdmittedAtMs,
			LastChangeMillis: device.LastChangeMs,
		})
	}
	return devices, nil
}

// See sidecarApprovalsBackend.
func (self *sidecarApprovalsRemoteBackend) previewRevoke(ctx context.Context, sidecarId sidecar.Id, reason string, lostAt time.Time, sinceN uint64) (*SidecarRevokePlan, error) {
	params := &sidecarControlDevicesPreviewParams{
		Network:  self.network(),
		Device:   sidecarId.String(),
		Reason:   reason,
		LostAtMs: sidecarMillisOf(lostAt),
		SinceN:   sinceN,
	}
	reply := &sidecarControlDevicesPreviewReply{}
	if err := self.remote.call(ctx, sidecarControlMethodDevicesPreview, params, false, reply); err != nil {
		return nil, err
	}
	if err := sidecarDeviceRefusalErrOf(reply.Refusal); err != nil {
		return nil, err
	}
	plan := reply.Plan
	if plan == nil || plan.PlanId == nil {
		return nil, errors.New("the daemon answered the preview with no plan")
	}
	bound := &SidecarRevokePlan{
		PlanId:      sidecarControlIdOf(plan.PlanId),
		SidecarId:   sidecarControlIdOf(plan.SidecarId),
		Name:        plan.Name,
		Reason:      plan.Reason,
		SinceN:      int64(plan.SinceN),
		SinceMillis: plan.SinceTimeMs,
		Items:       NewSidecarRevokeItemList(),
		Signatures:  int32(plan.Signatures),
		Presence:    plan.Presence,
	}
	for _, item := range plan.Items {
		if item == nil {
			continue
		}
		id, err := hex.DecodeString(item.Id)
		if err != nil {
			return nil, fmt.Errorf("the daemon's plan names a row id that is no hex")
		}
		bound.Items.Add(&SidecarRevokeItem{Effect: item.Effect, Kind: item.Kind, Name: item.Name, Id: id, Position: int64(item.Position)})
	}
	return bound, nil
}

// See sidecarApprovalsBackend: §5.8's two calls, devices.revoke.begin and devices.revoke.finish, the app signing the
// act's countersigned bytes between them (sidecar_presence.go); a daemon from before them, which does not know the
// first, takes the one confirmed devices.revoke, under its own presence prompt.
func (self *sidecarApprovalsRemoteBackend) revoke(ctx context.Context, planId sidecar.Id, revokeKeyIds [][]byte, signer SidecarPresenceSigner) (*sidecarRevokeOutcome, error) {
	params := &sidecarControlDevicesRevokeParams{Network: self.network(), PlanId: newId([16]byte(planId))}
	for _, keyId := range revokeKeyIds {
		params.RevokeKeyIds = append(params.RevokeKeyIds, hex.EncodeToString(keyId))
	}
	reply := &sidecarControlDevicesRevokeReply{}
	refusal, err := sidecarRemotePresenceAct(ctx, self.remote, params.Network, sidecarControlMethodDevicesRevokeBegin, params,
		sidecarControlMethodDevicesRevokeFinish, signer, reply)
	var unknown *sidecarControlError
	if errors.As(err, &unknown) && unknown.Code == sidecarControlCodeMethodNotFound {
		reply = &sidecarControlDevicesRevokeReply{}
		err = self.remote.call(ctx, sidecarControlMethodDevicesRevoke, params, true, reply)
		refusal = nil
	}
	if err != nil {
		return nil, err
	}
	if refusal == nil {
		refusal = reply.Refusal
	}
	outcome := &sidecarRevokeOutcome{position: reply.Position, pending: reply.Pending}
	if reply.ProposalId != "" {
		proposalId, err := hex.DecodeString(reply.ProposalId)
		if err != nil {
			return nil, errors.New("the daemon's answer names a proposal id that is no hex")
		}
		outcome.proposalId = proposalId
	}
	return outcome, sidecarDeviceRefusalErrOf(refusal)
}

// The channel's name as ur approve prints it, empty where this device signs none.
func sidecarApprovalChannelName(channel protocol.ApprovalChannel) string {
	switch channel {
	case protocol.ApprovalChannel_APPROVAL_CHANNEL_DEVICE:
		return "device"
	case protocol.ApprovalChannel_APPROVAL_CHANNEL_PRESENCE:
		return "presence"
	}
	return ""
}

var (
	_ sidecarApprovalsBackend = (*sidecarApprovalsCoreBackend)(nil)
	_ sidecarApprovalsBackend = (*sidecarApprovalsRemoteBackend)(nil)
)
