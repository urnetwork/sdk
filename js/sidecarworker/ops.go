// The page Worker's requests and its operations (§18.8's messages row): a request names one operation and its
// fields, every one of them checked here before anything is read, so a request the frame let through that this
// Worker would not take still answers invalid and reaches no seam.
//
//	list              repo_id, [ref], [path | path_base64]          a directory's children, as EvsRepo.List verifies them
//	read_view         repo_id, path | path_base64, [ref], [version],  a file's content and what its push signed, and
//	                  [history]                                     with history its versions (EvsRepo.ReadHead, History)
//	record_view       log_id, [after]                               a record log's verified entries (RecordReader)
//	sign_out          nothing                                       SidecarRevoke{removed}, the client removed, the
//	                                                                network's state wiped (Sidecar.Logout)
//	enroll            [code]                                        unavailable until plan-SDK-key-origin-credential's
//	admission_status  nothing                                       answer is built; neither reaches a seam
//
// A field an operation does not take must be empty. ids are uuid strings, versions decimal strings, names UTF-8 text
// or, for bytes that are not UTF-8, base64 (plan-EVS1-ref-name-bytes).

package sidecarworker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"unicode/utf8"

	"github.com/urnetwork/sidecar"
)

// The operations, as requests name them.
const (
	OperationList            = "list"
	OperationReadView        = "read_view"
	OperationRecordView      = "record_view"
	OperationSignOut         = "sign_out"
	OperationEnroll          = "enroll"
	OperationAdmissionStatus = "admission_status"
)

// Format bounds on a request's fields, which change nothing a server reads.
const (
	requestIdMaxByteCount   = 64
	requestPathMaxByteCount = 4096
	requestRefMaxByteCount  = 255
	requestCodeMaxByteCount = 512
)

// One request, as the frame passes it: an operation, the page's correlation id, and the operation's fields.
type Request struct {
	// The page's correlation id, 1 to 64 of [A-Za-z0-9_-], which the reply repeats.
	Id        string `json:"id"`
	Operation string `json:"op"`
	// A repo's or a record log's id.
	RepoId string `json:"repo_id,omitempty"`
	LogId  string `json:"log_id,omitempty"`
	// A ref's name as UTF-8; empty reads main.
	Ref string `json:"ref,omitempty"`
	// A path, raw names joined by /: as UTF-8 text, or as base64 of its bytes when they are not UTF-8. Not both.
	Path       string `json:"path,omitempty"`
	PathBase64 string `json:"path_base64,omitempty"`
	// read_view: an older version of the path, decimal; empty or 0 reads the newest.
	Version string `json:"version,omitempty"`
	// read_view: also the path's versions.
	History bool `json:"history,omitempty"`
	// record_view: the index of the first entry to return, decimal, as a reply's next named it; empty is 0.
	After string `json:"after,omitempty"`
	// enroll: the ur.io session's one-time code (plan-SDK-key-origin-credential, option 2).
	Code string `json:"code,omitempty"`
}

// A request from its JSON, refusing a field this package does not know.
func ParseRequest(requestJson []byte) (*Request, error) {
	decoder := json.NewDecoder(bytes.NewReader(requestJson))
	decoder.DisallowUnknownFields()
	request := &Request{}
	if err := decoder.Decode(request); err != nil {
		return nil, err
	}
	if decoder.More() {
		return nil, errors.New("a request is one JSON object")
	}
	return request, nil
}

// The id of a request that did not parse, where it is one a reply may repeat, else empty.
func readableRequestId(requestJson []byte) string {
	partial := &struct {
		Id string `json:"id"`
	}{}
	if json.Unmarshal(requestJson, partial) != nil || !validRequestId(partial.Id) {
		return ""
	}
	return partial.Id
}

// Whether id is 1 to 64 of [A-Za-z0-9_-].
func validRequestId(id string) bool {
	if id == "" || requestIdMaxByteCount < len(id) {
		return false
	}
	for _, c := range []byte(id) {
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// The checks every request passes, whatever its operation.
func checkRequest(request *Request) error {
	if !validRequestId(request.Id) {
		return errors.New("a request id is 1 to 64 of [A-Za-z0-9_-]")
	}
	return nil
}

// The request's fields an operation reads, checked.
type requestFields struct {
	repoId  sidecar.Id
	logId   sidecar.Id
	refName []byte
	path    string
	version uint64
	after   int
}

// Which fields an operation takes, beyond Id and Operation.
type requestShape struct {
	repoId  bool
	logId   bool
	ref     bool
	path    bool
	version bool
	history bool
	after   bool
	code    bool
}

// request's fields under shape: each field shape does not take is empty, and each it takes parses.
func (self requestShape) fields(request *Request) (*requestFields, error) {
	fields := &requestFields{}
	refuse := func(set bool, takes bool) bool {
		return set && !takes
	}
	if refuse(request.RepoId != "", self.repoId) || refuse(request.LogId != "", self.logId) || refuse(request.Ref != "", self.ref) ||
		refuse(request.Path != "" || request.PathBase64 != "", self.path) || refuse(request.Version != "", self.version) ||
		refuse(request.History, self.history) || refuse(request.After != "", self.after) || refuse(request.Code != "", self.code) {
		return nil, errors.New("a field this operation does not take")
	}
	if self.repoId {
		repoId, err := sidecar.ParseId(request.RepoId)
		if err != nil {
			return nil, err
		}
		fields.repoId = repoId
	}
	if self.logId {
		logId, err := sidecar.ParseId(request.LogId)
		if err != nil {
			return nil, err
		}
		fields.logId = logId
	}
	if request.Ref != "" {
		if requestRefMaxByteCount < len(request.Ref) || !utf8.ValidString(request.Ref) {
			return nil, errors.New("a ref is up to 255 bytes of UTF-8")
		}
		fields.refName = []byte(request.Ref)
	}
	switch {
	case request.Path != "" && request.PathBase64 != "":
		return nil, errors.New("a path is text or base64, not both")
	case request.PathBase64 != "":
		path, err := base64.StdEncoding.Strict().DecodeString(request.PathBase64)
		if err != nil {
			return nil, err
		}
		fields.path = string(path)
	default:
		if !utf8.ValidString(request.Path) {
			return nil, errors.New("a text path is UTF-8")
		}
		fields.path = request.Path
	}
	if requestPathMaxByteCount < len(fields.path) {
		return nil, errors.New("a path is up to 4096 bytes")
	}
	if request.Version != "" {
		version, err := strconv.ParseUint(request.Version, 10, 64)
		if err != nil {
			return nil, err
		}
		fields.version = version
	}
	if request.After != "" {
		after, err := strconv.Atoi(request.After)
		if err != nil || after < 0 {
			return nil, errors.New("after is a decimal index")
		}
		fields.after = after
	}
	if request.Code != "" && (requestCodeMaxByteCount < len(request.Code) || !printableAscii(request.Code)) {
		return nil, errors.New("a code is up to 512 printable ASCII bytes")
	}
	return fields, nil
}

// Whether s is printable ASCII, with no space.
func printableAscii(s string) bool {
	for _, c := range []byte(s) {
		if c <= ' ' || '~' < c {
			return false
		}
	}
	return true
}

// One operation: the fields it takes, and how it answers a request that passed them.
type workerOperation struct {
	shape requestShape
	// a path is required
	needsPath bool
	run       func(worker *Worker, ctx context.Context, request *Request, fields *requestFields, step *workerStep) *Reply
}

// The request's fields under the operation's shape, checked.
func (self *workerOperation) fields(request *Request) (*requestFields, error) {
	fields, err := self.shape.fields(request)
	if err != nil {
		return nil, err
	}
	if self.needsPath && fields.path == "" {
		return nil, errors.New("this operation reads a path")
	}
	return fields, nil
}

// Every operation by name. Nothing here signs but sign_out, through Logout.
var workerOperations = map[string]*workerOperation{
	OperationList: {
		shape: requestShape{repoId: true, ref: true, path: true},
		run:   runList,
	},
	OperationReadView: {
		shape:     requestShape{repoId: true, ref: true, path: true, version: true, history: true},
		needsPath: true,
		run:       runReadView,
	},
	OperationRecordView: {
		shape: requestShape{logId: true, after: true},
		run:   runRecordView,
	},
	OperationSignOut: {
		run: runSignOut,
	},
	OperationEnroll: {
		shape: requestShape{code: true},
		run:   runUnavailable,
	},
	OperationAdmissionStatus: {
		run: runUnavailable,
	},
}

// A directory's children (§11.7's name row), each name checked against its token by List.
func runList(worker *Worker, ctx context.Context, request *Request, fields *requestFields, step *workerStep) *Reply {
	repo, err := worker.args.Repo(fields.repoId)
	if err != nil {
		return errorReply(err)
	}
	listing, err := repo.List(ctx, &sidecar.EvsReadArgs{RefName: fields.refName, Path: fields.path})
	if err != nil {
		return errorReply(err)
	}
	view := &ListingView{
		RepoId:      fields.repoId.String(),
		RefId:       listing.RefId.String(),
		Path:        nameViewOf([]byte(listing.Path)),
		HeadVersion: strconv.FormatUint(listing.HeadVersion, 10),
		Entries:     []*ListingEntryView{},
	}
	for _, entry := range listing.Entries {
		if err := step.step(ctx); err != nil {
			return errorReply(err)
		}
		if worker.settings.ListMaxEntryCount <= len(view.Entries) {
			view.Truncated = true
			break
		}
		view.Entries = append(view.Entries, &ListingEntryView{Name: nameViewOf(entry.Name), Kind: evsEntryKindName(entry.Kind)})
	}
	return &Reply{Ok: true, Listing: view}
}

// A file's content at its head or at an older version, verified against its signed statement (§11.9), and with
// history its versions.
func runReadView(worker *Worker, ctx context.Context, request *Request, fields *requestFields, step *workerStep) *Reply {
	repo, err := worker.args.Repo(fields.repoId)
	if err != nil {
		return errorReply(err)
	}
	content := &workerContent{ctx: ctx, maxByteCount: worker.settings.ReadMaxByteCount, step: step}
	info, err := repo.ReadHead(ctx, &sidecar.EvsReadArgs{RefName: fields.refName, Path: fields.path, Version: fields.version}, content)
	if content.overflowed {
		return refusalReply("", CodeTooLarge)
	}
	if err != nil {
		return errorReply(err)
	}
	view := fileViewOf(fields.repoId, info, content.buffer.Bytes())
	if request.History {
		if err := step.step(ctx); err != nil {
			return errorReply(err)
		}
		history, err := repo.History(ctx, &sidecar.EvsReadArgs{RefName: fields.refName, Path: fields.path})
		if err != nil {
			return errorReply(err)
		}
		view.Versions = []*VersionView{}
		for _, version := range history.Versions {
			if err := step.step(ctx); err != nil {
				return errorReply(err)
			}
			view.Versions = append(view.Versions, versionViewOf(version))
		}
	}
	return &Reply{Ok: true, File: view}
}

// Where ReadHead writes a file's content: kept up to the bound, and a step between writes, so a long read yields.
type workerContent struct {
	ctx          context.Context
	maxByteCount int64
	step         *workerStep
	buffer       bytes.Buffer
	overflowed   bool
}

// Keeps p, or refuses once the content passes the bound, which ends the read.
func (self *workerContent) Write(p []byte) (int, error) {
	if self.maxByteCount < int64(self.buffer.Len())+int64(len(p)) {
		self.overflowed = true
		return 0, errWorkerTooLarge
	}
	self.buffer.Write(p)
	if err := self.step.step(self.ctx); err != nil {
		return len(p), err
	}
	return len(p), nil
}

// A record log's verified entries (RECORD.md section 5.5), a page at a time.
func runRecordView(worker *Worker, ctx context.Context, request *Request, fields *requestFields, step *workerStep) *Reply {
	records, err := worker.args.Records()
	if err != nil {
		return errorReply(err)
	}
	if err := records.Sync(ctx, fields.logId); err != nil {
		return errorReply(err)
	}
	if err := step.step(ctx); err != nil {
		return errorReply(err)
	}
	entries := records.Entries(fields.logId)
	view := &RecordView{
		LogId:      fields.logId.String(),
		EntryCount: len(entries),
		Entries:    []*RecordEntryView{},
	}
	for i := fields.after; i < len(entries); i++ {
		if err := step.step(ctx); err != nil {
			return errorReply(err)
		}
		if worker.settings.RecordMaxEntryCount <= len(view.Entries) {
			view.Next = strconv.Itoa(i)
			break
		}
		view.Entries = append(view.Entries, recordEntryViewOf(entries[i]))
	}
	return &Reply{Ok: true, Record: view}
}

// Signs this Worker out (§18.8's sign-out, §5.6): Logout signs SidecarRevoke{removed} for its own sidecar_id, removes
// its client and wipes the network's state, and only then is the Worker done. A sign-out that cannot land wipes
// nothing and the Worker keeps serving, so the page may ask again with the same request, which posts the same
// statement. Nothing enrolled answers ok, since there is nothing to sign out, so a relayed sign_out repeated after a
// hand-over answers as the first did.
func runSignOut(worker *Worker, ctx context.Context, request *Request, fields *requestFields, step *workerStep) *Reply {
	worker.signOutLock.Lock()
	defer worker.signOutLock.Unlock()
	if worker.isSignedOut() {
		return refusalReply("", CodeSignedOut)
	}
	view := &SignOutView{}
	if identity := worker.args.Sidecar.Identity(); identity != nil {
		view.SidecarId = identity.SidecarId.String()
	}
	if err := worker.args.Sidecar.Logout(ctx); err != nil && !errors.Is(err, sidecar.ErrSidecarNoIdentity) {
		return errorReply(err)
	}
	func() {
		worker.stateLock.Lock()
		defer worker.stateLock.Unlock()
		worker.signedOut = true
	}()
	if worker.args.SignedOut != nil {
		worker.args.SignedOut()
	}
	return &Reply{Ok: true, SignOut: view}
}

// enroll and admission_status: unavailable until the key origin's credential is built (plan-SDK-key-origin-credential).
func runUnavailable(worker *Worker, ctx context.Context, request *Request, fields *requestFields, step *workerStep) *Reply {
	return refusalReply("", CodeUnavailable)
}
