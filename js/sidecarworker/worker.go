//go:build sidecar_http || !js

// The page Worker's logic (§18.8, §18.10; D-SDK-18, D-SDK-19): one network's read_only sidecar answering the requests
// of the ur.io pages that frame its key origin, each naming one operation with a correlation id, with plaintext views
// only. It names no syscall/js, so it builds and tests natively on sidecartest as well as inside sidecar.wasm, whose
// main (sdk/js/cmd/sidecarwasm) is the thin shell that builds this leg's sidecar.Sidecar on IndexedDB and the
// WebCrypto key and hands its pieces in as seams.
//
// What a reply carries: a view, one of this package's own structs built field by field from what the root verified,
// never a root struct passed through (views.go), and for a refusal a code with that code's fixed message, never an
// error's text. So no reply path reaches a token, a key, a wrapped secret or a GetSecret string (§18.8's messages
// row; I-2, I-7). Ids cross as strings and versions as decimal strings, since a JS number holds an integer exactly
// only up to 2^53 (§6.8).
//
// What a Worker does: it reads (list, read_view, record_view) and it signs itself out (sign_out). A page Worker is
// admitted read_only (§7.9), so beyond its sign-out SidecarRevoke, its own key upkeep and read receipts, which the
// root signs where they belong, it signs nothing: the seams it holds have no signing call (WorkerSidecar), and it
// files no approval or request and offers no revocation (§18.5's js row). enroll and admission_status exist as
// operations and answer unavailable: the owner chose the ur.io session's one-time code as the key origin's credential
// (plan-SDK-key-origin-credential, option 2), and until the root can mint a top-level client from it the interim
// holds, so no page Worker enrolls.
//
// js/wasm runs one thread (§18.10's threads row): an operation yields between its steps once YieldInterval has
// passed since its last yield, through the host's Yield, which on the page Worker hands the thread to the browser's
// event loop, so the long-poll's fetch and other requests interleave with a long read.
//
// Safe for concurrent use: requests run at once, each on its caller's goroutine, and one sign_out at a time.

package sidecarworker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"time"

	"github.com/urnetwork/sidecar"
)

// What a Worker's operations are bounded by.
type WorkerSettings struct {
	// The longest an operation computes between two yields (§18.10's threads row): 50 ms, so a poll's reply or another
	// page's request waits at most that long behind a read. Non-positive yields at every step.
	YieldInterval time.Duration
	// The most bytes one request may hold. The frame refuses a larger one before it reaches the Worker; this is the
	// Worker's own bound.
	RequestMaxByteCount int
	// The most content bytes one read_view returns. A larger file answers too_large. 8 MiB, so the view and its
	// base64 stay well inside §18.4's 64 MiB above wasm data.
	ReadMaxByteCount int64
	// The most children one list returns; past it the listing is marked truncated.
	ListMaxEntryCount int
	// The most entries one record_view returns; past it the reply names where the next page starts.
	RecordMaxEntryCount int
}

// The defaults.
func DefaultWorkerSettings() *WorkerSettings {
	return &WorkerSettings{
		YieldInterval:       50 * time.Millisecond,
		RequestMaxByteCount: 64 * 1024,
		ReadMaxByteCount:    8 * 1024 * 1024,
		ListMaxEntryCount:   4096,
		RecordMaxEntryCount: 512,
	}
}

// The sidecar a Worker serves: on the page Worker this leg's sidecar.Sidecar, which has each of these. It has no
// signing call: sign-out's SidecarRevoke is Logout's own (§5.6), and everything else a read_only device signs, its key
// upkeep and read receipts, the root signs inside the reads that need them.
type WorkerSidecar interface {
	// The enrolled identity's ids and public keys, nil before enrollment.
	Identity() *sidecar.SidecarIdentity
	Status() sidecar.SidecarStatus
	// Signs SidecarRevoke{removed} for this sidecar, removes its client and wipes this network's state, in that
	// order, wiping nothing when a step before refuses (identity_logout.go).
	Logout(ctx context.Context) error
}

// One repo as a Worker reads it: sidecar.EvsRepo's verified reads (§11.7, §11.9).
type WorkerRepo interface {
	List(ctx context.Context, args *sidecar.EvsReadArgs) (*sidecar.EvsListing, error)
	ReadHead(ctx context.Context, args *sidecar.EvsReadArgs, out io.Writer) (*sidecar.EvsFileInfo, error)
	History(ctx context.Context, args *sidecar.EvsReadArgs) (*sidecar.EvsHistory, error)
}

// The record logs a Worker reads: sidecar.RecordReader (RECORD.md section 5.5), which verifies each entry before it
// holds it.
type WorkerRecords interface {
	Sync(ctx context.Context, logId sidecar.Id) error
	Entries(logId sidecar.Id) []*sidecar.RecordReadEntry
}

// What a Worker runs on.
type WorkerArgs struct {
	Sidecar WorkerSidecar
	// The repo of repoId: on the page Worker, its EvsClient's. An error is what the reply reports.
	Repo func(repoId sidecar.Id) (WorkerRepo, error)
	// The record reader: on the page Worker, the Sidecar's record's. An error is what the reply reports.
	Records func() (WorkerRecords, error)
	// Hands the thread to the event loop: the shell's wait on a zero timeout in the browser. Nil yields with
	// runtime.Gosched, which on a native leg is a cheap hand-off.
	Yield func(ctx context.Context) error
	// The clock YieldInterval is read on. Nil is real time.
	Now func() time.Time
	// Called once sign_out has signed out and wiped. Nil calls nothing.
	SignedOut func()
	// Nil is the defaults.
	Settings *WorkerSettings
}

// One network's page Worker.
type Worker struct {
	args     *WorkerArgs
	settings *WorkerSettings
	now      func() time.Time
	yield    func(ctx context.Context) error

	// one sign_out at a time
	signOutLock sync.Mutex

	stateLock sync.Mutex
	// set once a sign_out has wiped, after which every request answers signed_out
	signedOut bool
}

// A Worker on args. Refuses args without the sidecar, the repo seam or the record seam.
func NewWorker(args *WorkerArgs) (*Worker, error) {
	if args == nil || args.Sidecar == nil || args.Repo == nil || args.Records == nil {
		return nil, errors.New("a page Worker needs its sidecar, its repos and its records")
	}
	settings := args.Settings
	if settings == nil {
		settings = DefaultWorkerSettings()
	}
	if settings.RequestMaxByteCount <= 0 || settings.ReadMaxByteCount <= 0 || settings.ListMaxEntryCount <= 0 || settings.RecordMaxEntryCount <= 0 {
		return nil, fmt.Errorf("a page Worker's bounds must be positive")
	}
	now := args.Now
	if now == nil {
		now = time.Now
	}
	yield := args.Yield
	if yield == nil {
		yield = func(ctx context.Context) error {
			runtime.Gosched()
			return ctx.Err()
		}
	}
	return &Worker{
		args:     args,
		settings: settings,
		now:      now,
		yield:    yield,
	}, nil
}

// Answers request: its operation's view, or a refusal with a code. Never nil, and its Id is the request's.
func (self *Worker) Handle(ctx context.Context, request *Request) *Reply {
	if request == nil {
		return refusalReply("", CodeInvalid)
	}
	if err := checkRequest(request); err != nil {
		// an id that is not one is never repeated
		return refusalReply("", CodeInvalid)
	}
	operation, ok := workerOperations[request.Operation]
	if !ok {
		return refusalReply(request.Id, CodeUnknownOperation)
	}
	fields, err := operation.fields(request)
	if err != nil {
		return refusalReply(request.Id, CodeInvalid)
	}
	if self.isSignedOut() {
		return refusalReply(request.Id, CodeSignedOut)
	}
	reply := operation.run(self, ctx, request, fields, self.newStep())
	reply.Id = request.Id
	return reply
}

// Handle on a request as the frame passes it, JSON, answering the reply as JSON. A request that does not parse, or
// is over RequestMaxByteCount, answers invalid, under its id where one was readable.
func (self *Worker) HandleJson(ctx context.Context, requestJson []byte) []byte {
	var reply *Reply
	if self.settings.RequestMaxByteCount < len(requestJson) {
		reply = refusalReply("", CodeTooLarge)
	} else if request, err := ParseRequest(requestJson); err != nil {
		reply = refusalReply(readableRequestId(requestJson), CodeInvalid)
	} else {
		reply = self.Handle(ctx, request)
	}
	return reply.json()
}

// Whether a sign_out has wiped this Worker's network.
func (self *Worker) isSignedOut() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.signedOut
}

// One operation's yields, started now.
func (self *Worker) newStep() *workerStep {
	return &workerStep{worker: self, last: self.now()}
}

// The yields of one operation. Not safe for concurrent use: each operation has its own.
type workerStep struct {
	worker *Worker
	last   time.Time
	// how many times this operation yielded
	yieldCount int
}

// Called between two steps of an operation: yields once YieldInterval has passed since the last yield or the start,
// and answers ctx's error once it ends.
func (self *workerStep) step(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if self.worker.now().Sub(self.last) < self.worker.settings.YieldInterval {
		return nil
	}
	err := self.worker.yield(ctx)
	self.yieldCount += 1
	self.last = self.worker.now()
	return err
}
