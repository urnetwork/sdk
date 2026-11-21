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
// What a Worker does: it reads (list, read_view, record_view, and for RS2's team console directory_view, config_view
// and drop_states) and it signs itself out (sign_out). A page Worker is admitted read_only (§7.9) and its client
// admin_refused (§18.8's Identity bullet), so beyond its sign-out SidecarRevoke, its own key upkeep and read receipts,
// which the root signs where they belong, it signs nothing: the seams it holds have no signing call (WorkerSidecar,
// WorkerDirectory), and it files no approval, request or decision and offers no revocation (§18.5's js row, §16.11's
// console row). enroll and admission_status exist as operations and answer unavailable: the owner chose the ur.io
// session's one-time code as the key origin's credential (plan-SDK-key-origin-credential, option 2), and until the root
// can mint a top-level client from it the interim holds, so no page Worker enrolls.
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
	// A request's field bounds, the format the frame's loader.js checks first (its requestIdPattern and textMaxLength):
	// a correlation id's bytes, 64; a path's, as text or as the bytes its base64 names, 4096; a ref's name, 255;
	// enroll's one-time code, 512; directory_view's query, 512, a search query's bound (§11.14), which the Worker
	// filters by locally and sends nowhere (§16.11's tools row, I-19).
	RequestIdMaxByteCount    int
	RequestPathMaxByteCount  int
	RequestRefMaxByteCount   int
	RequestCodeMaxByteCount  int
	RequestQueryMaxByteCount int
	// The most items one directory_view returns; past it the reply names where the next page starts. 256: about 512
	// KiB of typical items' views, and at most about 5 MiB at an item's caps (four claims' text and targets), inside
	// §18.4's 64 MiB above wasm data.
	DirectoryMaxItemCount int
	// A directory_view syncs the directory's copy first unless a sync of it succeeded within this, as a desktop syncs
	// at each view (§16.11's desktops row): the root's Directory.ViewCoalesce, 30 s, which the shell passes from the
	// network's own settings. Non-positive syncs at every view.
	DirectoryViewCoalesce time.Duration
	// The most rows each list of a config_view holds, members, sidecars, names and rules each; past it the list stops
	// and the view is marked truncated. 16,384, past a Team's 5,000 config_admissions (§20.4) and its stress case's
	// 12,500 (§16.11's sizes row).
	ConfigMaxRowCount int
	// The most bytes of the policy document a config_view carries: §8.8's 4 MiB source bound, which no head a base
	// names passes. A longer one is left out and marked.
	ConfigPolicySourceMaxByteCount int
	// Search's (ops_search.go, SDK-M10-04): the searches one Worker holds open, the least recently used ending past it,
	// 4; how long one goes unused before it ends, the core's Search.CursorTimeout, 10 min; how long a search or
	// search_more computes before it answers what landed, §16.10's wait_ms default, 2 s; how many pulls in a row may
	// bring nothing before a reply answers what it has, 4; a page's hits when none is named, §18.5's 50, and at most,
	// §16.10's limit, 100; and a hit view's bounds, §16.10's 256 B a field and 200 B a snippet.
	SearchSessionMaxCount     int
	SearchIdleTimeout         time.Duration
	SearchReplyTimeout        time.Duration
	SearchEmptyPullCount      int
	SearchPageHitCount        int
	SearchPageHitMaxCount     int
	SearchFieldMaxByteCount   int
	SearchSnippetMaxByteCount int
	// The most drops one drop_states returns (ops_drops.go, SDK-M10-05); past it the view is marked truncated. 256:
	// each names at most the core's DropNamesCount paths, 1,024, so the view stays well inside §18.4's 64 MiB above
	// wasm data.
	DropStatesMaxDropCount int
}

// The defaults.
func DefaultWorkerSettings() *WorkerSettings {
	return &WorkerSettings{
		YieldInterval:                  50 * time.Millisecond,
		RequestMaxByteCount:            64 * 1024,
		ReadMaxByteCount:               8 * 1024 * 1024,
		ListMaxEntryCount:              4096,
		RecordMaxEntryCount:            512,
		RequestIdMaxByteCount:          64,
		RequestPathMaxByteCount:        4096,
		RequestRefMaxByteCount:         255,
		RequestCodeMaxByteCount:        512,
		RequestQueryMaxByteCount:       512,
		DirectoryMaxItemCount:          256,
		DirectoryViewCoalesce:          sidecar.DefaultSidecarSettings().Directory.ViewCoalesce,
		ConfigMaxRowCount:              16 * 1024,
		ConfigPolicySourceMaxByteCount: 4 * 1024 * 1024,
		SearchSessionMaxCount:          4,
		SearchIdleTimeout:              10 * time.Minute,
		SearchReplyTimeout:             2 * time.Second,
		SearchEmptyPullCount:           4,
		SearchPageHitCount:             50,
		SearchPageHitMaxCount:          100,
		SearchFieldMaxByteCount:        256,
		SearchSnippetMaxByteCount:      200,
		DropStatesMaxDropCount:         256,
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

// The team directory a Worker reads (§16.11): on the page Worker, the Sidecar's DirectoryClient, which the
// sidecar_http leg builds read_only, so its writes refuse whatever is asked of it. Every method here reads: a sync
// fetches and verifies, and List and Get filter the folded copy locally, so nothing of a filter or a query reaches evs
// (I-19).
type WorkerDirectory interface {
	// The network's directory, false where it has none: a personal network, or a team's before its directory exists.
	DirectoryId() (sidecar.Id, bool)
	// One sync of the copy: the cards ref, the moved streams, each entry verified before it is kept.
	Sync(ctx context.Context) error
	List(filter *sidecar.DirectoryFilter) ([]*sidecar.DirectoryItem, error)
	Get(announcementId sidecar.Id, writerSidecarId *sidecar.Id) (*sidecar.DirectoryItem, error)
}

// The network's config as a Worker shows it (§18.8's Identity bullet: the config's names, tags, groups and policy):
// one verified state of §7.9's log and what the config head that state stands on names. A Worker reads it and changes
// none of it.
type WorkerConfigState struct {
	// The newest verified state.
	State *sidecar.ConfigState
	// The config head State's newest base names, nil where this device could not open it.
	Head *sidecar.ConfigHead
	// The policy compiled from Head (AclLocalPolicy): its rules, its requests rules and the names behind their atoms;
	// nil where none compiled.
	Local *sidecar.AclLocalPolicy
}

// What a Worker runs on.
type WorkerArgs struct {
	Sidecar WorkerSidecar
	// The key-origin document redeems the one-time code and hands its Worker only a marked client.
	Enroll func(context.Context, string) error
	// The local binding and verified admission of this browser.
	Admission func() *AdmissionView
	// The repo of repoId: on the page Worker, its EvsClient's. An error is what the reply reports.
	Repo func(repoId sidecar.Id) (WorkerRepo, error)
	// The record reader: on the page Worker, the Sidecar's record's. An error is what the reply reports.
	Records func() (WorkerRecords, error)
	// The team directory: on the page Worker, the Sidecar's read_only DirectoryClient once its evs and authorization
	// state are bound. An error is what the reply reports; nil answers unavailable.
	Directory func() (WorkerDirectory, error)
	// The verified config: on the page Worker, its config state, head and compiled policy once this leg verifies
	// them. An error is what the reply reports; nil answers unavailable.
	Config func() (*WorkerConfigState, error)
	// The search (ops_search.go): on the page Worker, the Sidecar's §5.4 search calls, which answer unavailable until
	// a host installs its engine. An error is what the reply reports; nil answers unavailable.
	Search func() (WorkerSearch, error)
	// The drops of repoId, for the console's drop_states (ops_drops.go): on the page Worker, its EvsClient's repo, read
	// through WorkerDrops' reads alone. An error is what the reply reports; nil answers unavailable.
	Drops func(repoId sidecar.Id) (WorkerDrops, error)
	// A quick burner's Worker: memory only, its search reads the buckets alone and never fetches all (§18.8's Quick
	// burner).
	Ephemeral bool
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
	// the directory whose copy last synced without error, and when that sync started, which DirectoryViewCoalesce
	// reads; zero before any
	syncedDirectoryId sidecar.Id
	syncedAt          time.Time

	// the searches this Worker holds open (ops_search.go)
	searches *workerSearches
}

// A Worker on args. Refuses args without the sidecar, the repo seam or the record seam, or with a bound that is not
// positive.
func NewWorker(args *WorkerArgs) (*Worker, error) {
	if args == nil || args.Sidecar == nil || args.Repo == nil || args.Records == nil {
		return nil, errors.New("a page Worker needs its sidecar, its repos and its records")
	}
	settings := args.Settings
	if settings == nil {
		settings = DefaultWorkerSettings()
	}
	for _, bound := range []int64{
		int64(settings.RequestMaxByteCount), settings.ReadMaxByteCount, int64(settings.ListMaxEntryCount), int64(settings.RecordMaxEntryCount),
		int64(settings.RequestIdMaxByteCount), int64(settings.RequestPathMaxByteCount), int64(settings.RequestRefMaxByteCount),
		int64(settings.RequestCodeMaxByteCount), int64(settings.RequestQueryMaxByteCount), int64(settings.DirectoryMaxItemCount),
		int64(settings.ConfigMaxRowCount), int64(settings.ConfigPolicySourceMaxByteCount),
		int64(settings.SearchSessionMaxCount), int64(settings.SearchIdleTimeout), int64(settings.SearchReplyTimeout),
		int64(settings.SearchEmptyPullCount), int64(settings.SearchPageHitCount), int64(settings.SearchPageHitMaxCount),
		int64(settings.SearchFieldMaxByteCount), int64(settings.SearchSnippetMaxByteCount), int64(settings.DropStatesMaxDropCount),
	} {
		if bound <= 0 {
			return nil, fmt.Errorf("a page Worker's bounds must be positive")
		}
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
		searches: newWorkerSearches(),
	}, nil
}

// Answers request: its operation's view, or a refusal with a code. Never nil, and its Id is the request's.
func (self *Worker) Handle(ctx context.Context, request *Request) *Reply {
	if request == nil {
		return refusalReply("", CodeInvalid)
	}
	if err := checkRequest(request, self.settings); err != nil {
		// an id that is not one is never repeated
		return refusalReply("", CodeInvalid)
	}
	operation, ok := workerOperations[request.Operation]
	if !ok {
		return refusalReply(request.Id, CodeUnknownOperation)
	}
	fields, err := operation.fields(request, self.settings)
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
		reply = refusalReply(readableRequestId(requestJson, self.settings), CodeInvalid)
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
