//go:build sidecar_http || !js

// The quick burner (§18.8's Quick burner, Q-SDK-4, R-SDK-8, D-SDK-6, D-SDK-17, I-15; SDK-M8-14): an ephemeral sidecar
// in a ur.io page, labeled ephemeral to it, that reads its member's mailbox and nothing else.
//
//	keychain   memory only, on an instance on the core's memory backend (sidecarwasm's newBurnerInstance): no Web Lock,
//	           nothing in IndexedDB or any storage, so what it holds dies with the Worker, and it is never a genesis
//	           signer (an ephemeral client, §7.7)
//	enroll     quick_burner with the ur.io session's one-time code (plan-SDK-key-origin-credential, option 2) mints the
//	           ephemeral client and enrolls the keychain on it, once in a burner's life; the mint's close capability
//	           goes straight to the key-origin document by HandClose, which beacons it on pagehide, and the burner
//	           keeps no copy and puts it in no reply, so no page script ever holds it (D-SDK-17)
//	admission  a consented device of its member admits it and signs the EvsShare that opens the mailbox to it by
//	           ReaderWraps (§5.6, §8.8, §15.7); until one does the burner answers waiting and the page waits
//	reap       30 to 36 min after its last poll the client is reaped (§7.10's ephemeral job, the lease); a resumed tab's
//	           poll then gets 401 and the core reads unenrolled, which quick_burner answers reaped with reenroll set:
//	           a memory-only keychain cannot rejoin, so the page offers a new burner
//	reads      list and read_view on the shared mailbox, as the page Worker's; it never fetches all, and at M10 searches
//	           the mailbox's two buckets in memory only (SDK-M10-04)
//
// The root has no top-level mint from the one-time code yet (the page Worker's enroll waits on it too), so Enroll is
// nil on every host today and quick_burner answers unavailable before an enrollment, failing closed. Every other
// operation is the page Worker's, which a burner holds. Safe for concurrent use.

package sidecarworker

import (
	"context"
	"errors"
	"sync"

	"github.com/urnetwork/sidecar"
)

// The quick burner's own operation, as requests name it.
const OperationQuickBurner = "quick_burner"

// The states a quick_burner view names. Format values: the page reads them.
const (
	// Enrolled, and no consented device has admitted it and shared the mailbox yet: the page waits.
	QuickBurnerStateWaiting = "waiting"
	// The mailbox's EvsShare opens it: reads answer.
	QuickBurnerStateAdmitted = "admitted"
	// The client was reaped or revoked: the memory-only keychain cannot rejoin, so the page offers a new burner.
	QuickBurnerStateReaped = "reaped"
)

// What a quick_burner reply carries: the burner's state, always labeled ephemeral. Never the close capability, a
// token or a key.
type QuickBurnerView struct {
	// Always true: the page labels the burner ephemeral (§18.8).
	Ephemeral bool `json:"ephemeral"`
	// waiting, admitted or reaped.
	State string `json:"state"`
	// The page offers re-enrollment, a new burner.
	Reenroll bool `json:"reenroll,omitempty"`
	// The mailbox the share opens, once admitted.
	MailboxRepoId string `json:"mailbox_repo_id,omitempty"`
}

// What a quick burner runs on beside its page Worker's args.
type QuickBurnerArgs struct {
	// Mints the ephemeral client from the ur.io session's one-time code and enrolls the memory-only keychain on it,
	// answering the mint's close capability. Nil answers unavailable: the root has no such mint yet (the file's
	// comment).
	Enroll func(ctx context.Context, code string) (closeCapability string, err error)
	// Hands the close capability to the key-origin document, never to the page. Required.
	HandClose func(closeCapability string)
	// The mailbox the admitting device's EvsShare opens to this burner, false while none does. Nil reads none.
	SharedMailbox func() (sidecar.Id, bool)
}

// A quick burner: a page Worker in ephemeral mode with quick_burner beside its operations.
type QuickBurner struct {
	worker *Worker
	args   *QuickBurnerArgs

	// one enrollment at a time
	enrollLock sync.Mutex

	stateLock sync.Mutex
	// set once this burner enrolled, so a later unenrolled state reads reaped
	enrolled bool
}

// A quick burner over workerArgs and args. HandClose is required.
func NewQuickBurner(workerArgs *WorkerArgs, args *QuickBurnerArgs) (*QuickBurner, error) {
	if args == nil || args.HandClose == nil {
		return nil, errors.New("a quick burner hands its close capability to the document")
	}
	if workerArgs == nil {
		return nil, errors.New("a quick burner needs its Worker's args")
	}
	// memory only: its search reads the buckets alone (SDK-M10-04)
	ephemeral := *workerArgs
	ephemeral.Ephemeral = true
	worker, err := NewWorker(&ephemeral)
	if err != nil {
		return nil, err
	}
	return &QuickBurner{worker: worker, args: args}, nil
}

// Reports the frame's conditions to the burner's search, as the page Worker's are (ops_search.go).
func (self *QuickBurner) SetConditions(conditions *WorkerConditions) {
	self.worker.SetConditions(conditions)
}

// Answers request: quick_burner here, every other operation its Worker's.
func (self *QuickBurner) Handle(ctx context.Context, request *Request) *Reply {
	if request == nil || request.Operation != OperationQuickBurner {
		return self.worker.Handle(ctx, request)
	}
	if err := checkRequest(request, self.worker.settings); err != nil {
		return refusalReply("", CodeInvalid)
	}
	if request.RepoId != "" || request.LogId != "" || request.Ref != "" || request.Path != "" || request.PathBase64 != "" ||
		request.Version != "" || request.History || request.After != "" || self.worker.settings.RequestCodeMaxByteCount < len(request.Code) {
		return refusalReply(request.Id, CodeInvalid)
	}
	reply := self.quickBurner(ctx, request.Code)
	reply.Id = request.Id
	return reply
}

// Handle on a request's JSON, as the frame passes it.
func (self *QuickBurner) HandleJson(ctx context.Context, requestJson []byte) []byte {
	var reply *Reply
	if self.worker.settings.RequestMaxByteCount < len(requestJson) {
		reply = refusalReply("", CodeTooLarge)
	} else if request, err := ParseRequest(requestJson); err != nil {
		reply = refusalReply(readableRequestId(requestJson, self.worker.settings), CodeInvalid)
	} else {
		reply = self.Handle(ctx, request)
	}
	return reply.json()
}

// quick_burner (the file's comment): enrolls once with code where nothing is enrolled, then answers the state.
func (self *QuickBurner) quickBurner(ctx context.Context, code string) *Reply {
	if self.worker.args.Sidecar.Identity() == nil && !self.wasEnrolled() {
		if code == "" {
			return refusalReply("", CodeInvalid)
		}
		if err := self.enroll(ctx, code); errors.Is(err, errQuickBurnerNoMint) {
			return refusalReply("", CodeUnavailable)
		} else if err != nil {
			return errorReply(err)
		}
	}
	return &Reply{Ok: true, QuickBurner: self.view()}
}

// Enrolls once: the close capability goes to the document and nowhere else, and is dropped here.
func (self *QuickBurner) enroll(ctx context.Context, code string) error {
	self.enrollLock.Lock()
	defer self.enrollLock.Unlock()
	if self.wasEnrolled() {
		return nil
	}
	if self.args.Enroll == nil {
		return errQuickBurnerNoMint
	}
	closeCapability, err := self.args.Enroll(ctx, code)
	if err != nil {
		return err
	}
	if closeCapability != "" {
		self.args.HandClose(closeCapability)
	}
	self.stateLock.Lock()
	self.enrolled = true
	self.stateLock.Unlock()
	return nil
}

// Whether this burner enrolled in its life.
func (self *QuickBurner) wasEnrolled() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.enrolled
}

// The burner's state as the page sees it.
func (self *QuickBurner) view() *QuickBurnerView {
	view := &QuickBurnerView{Ephemeral: true, State: QuickBurnerStateWaiting}
	status := self.worker.args.Sidecar.Status()
	switch {
	case status.State == sidecar.SidecarStateUnenrolled || status.State == sidecar.SidecarStateRevoked:
		// a reaped or revoked ephemeral client: its memory-only keychain cannot rejoin
		view.State = QuickBurnerStateReaped
		view.Reenroll = true
		return view
	case self.args.SharedMailbox == nil:
		return view
	}
	if mailboxId, ok := self.args.SharedMailbox(); ok {
		view.State = QuickBurnerStateAdmitted
		view.MailboxRepoId = mailboxId.String()
	}
	return view
}

// No host can mint the burner's ephemeral client from the one-time code yet (the file's comment).
var errQuickBurnerNoMint = errors.New("quick burner: no ephemeral mint from the session's one-time code yet")
