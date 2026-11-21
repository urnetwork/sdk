//go:build !ios_extension && !js

// SidecarEvs (§18.5, D-SDK-2; SDK-M8-03, SDK-M10-05): an app's calls on its network's evs repos, each a thin mapping
// onto the core's EvsClient: CreateRepo with its private flag (owner decision 181), ReadHead, ReadVersion, History with
// format 2's states, Write of at most 1 MiB on a file-mode ref with its versioning setting, AddRepoChangeListener and
// Erasures, and the drop calls (sidecar_evs_drop.go). Tokens are derived and decrypted in the core, so nothing here
// touches a key, and every check the core makes stands behind each call: nothing here skips one, and beside its own
// bounds this layer adds one, D-SDK-29's, before the drop calls reach the core.
//
// Every call answers once, on a Go thread, through its callback: Ok when it did what it asked, else a refusal with
// §18.5's code where one exists and a message for a person, whether the core refused, the Sidecar closed under it,
// its context ended, or the call never left the device. A panic in the core reaches the callback as its error, never
// the app (trace). The codes:
//
//	EVS_GIT_REF             a write to a git-mode ref, which takes writes only through the local tier (§11.7)
//	EVS_TOO_LARGE           a write past WriteMaxByteCount (§18.5's 1 MiB), or a read past ReadMaxByteCount
//	EVS_VERSIONING          a write's versioning argument other than "", "on" or "off", refused before the core; or
//	                        evs's 409 VERSIONING, a caller's error, never retried
//	EVS_NOT_FOUND           a path the head does not hold
//	EVS_ERASED              a committed erasure destroyed a key needed to open the version
//	EVS_DROPPED             ReadVersion of a version a drop took: from its drop's not_before by this device's clock,
//	                        unless the drop is cancelled or refused, nothing fetched (§11.7's states row)
//	EVS_COLLECTED           ReadVersion of a replaced versionless version, whose content evs no longer keeps (§11.9)
//	EVS_FORMAT_UNSUPPORTED  a format 2 construct the core refuses, versioning "off" outside the network's format 2 gate
//	                        among them
//	EVS_INVALID             a call the core cannot run: a path that splits into no names, a kind apps do not create,
//	                        a version that is no number
//	EVS_READ_STALE          a read whose head another call of the core raised the device's pin past on each of the
//	                        core's tries, which nothing placed on the pinned history (the core's ErrEvsReadStale): no
//	                        alarm was raised, and the call may go again
//	EVS_DROP_*              the drop calls' own refusals (sidecar_evs_drop.go)
//	EVS_<refusal>           evs's own answer, its refusal code's name less EVS_REFUSAL_: EVS_PATH_CONFLICT, a
//	                        versionless path's 409, surfaced and never retried by rebasing (§11.7); EVS_SCOPE; ...
//	EVS_UNAVAILABLE         the Sidecar closed, or the call's context ended, before an answer
//
// EVS_PREFIX_RULE, the core's ErrEvsPrefixRule with its path and detail (DESIGN-DETAILS 16.7), is EVS1-M6-05's to add
// beside these once the core has it.
//
// A change listener hears each commit the core verified on any ref of its repo once, in order, on a Go thread of its
// own. Its repo is leased (EvsRepo.Subscribe) at the first listener and stays leased while the Sidecar runs, as MCP's
// resources keep theirs, so hints, and on phones wakes, follow the repo. A listener that falls its queue behind hears
// one gap for the repo in place of what it missed, and reads again.
//
// Paths are the person's own and go only between the app and its core on this device: nothing here logs one (I-13).
// Safe for concurrent use.

package sdk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
)

func init() {
	registerSidecarComponentFactory(SidecarComponentEvs, newSidecarEvsComponent)
}

// What an app's evs calls are held to.
type sidecarEvsSettings struct {
	// The bytes one Write takes: §18.5's 1 MiB. A larger one answers EVS_TOO_LARGE before anything is sent.
	WriteMaxByteCount int
	// The bytes one ReadHead answers with: 4 MiB, under a phone's working set (§18.4's 16 and 32 MiB budgets), since
	// the answer is held whole and copied across the binding once more. A larger file answers EVS_TOO_LARGE, its
	// bytes read no further than the bound.
	ReadMaxByteCount int
	// Changes waiting for one listener's goroutine: 256. Past it a change is not queued, and the listener hears a gap
	// for its repo once its queue has drained.
	ChangeQueueCount int
}

// The defaults, a fresh value each call.
func defaultSidecarEvsSettings() *sidecarEvsSettings {
	return &sidecarEvsSettings{
		WriteMaxByteCount: 1024 * 1024,
		ReadMaxByteCount:  4 * 1024 * 1024,
		ChangeQueueCount:  256,
	}
}

// The refusal codes this file answers itself (the file's comment). A format value: apps read them.
const (
	sidecarEvsCodeGitRef            = "EVS_GIT_REF"
	sidecarEvsCodeTooLarge          = "EVS_TOO_LARGE"
	sidecarEvsCodeVersioning        = "EVS_VERSIONING"
	sidecarEvsCodeNotFound          = "EVS_NOT_FOUND"
	sidecarEvsCodeDropped           = "EVS_DROPPED"
	sidecarEvsCodeCollected         = "EVS_COLLECTED"
	sidecarEvsCodeErased            = "EVS_ERASED"
	sidecarEvsCodeFormatUnsupported = "EVS_FORMAT_UNSUPPORTED"
	sidecarEvsCodeInvalid           = "EVS_INVALID"
	sidecarEvsCodeUnavailable       = "EVS_UNAVAILABLE"
	sidecarEvsCodePathConflict      = "EVS_PATH_CONFLICT"
	sidecarEvsCodeReadStale         = "EVS_READ_STALE"
	// The repo's prefix write rules hold the path (DESIGN-DETAILS 16.2.7, 16.2.5, 8.2.8; §18.5's {path, detail}): the
	// message carries the detail, one of refused, pending, update, depth, unavailable, base and tag, and the result's
	// Value the path the rules hold, the first file outside the device's prefixes for base and tag, empty where there
	// is none to name. The path is the caller's own and goes into no log (I-13).
	sidecarEvsCodePrefixRule = "EVS_PREFIX_RULE"
)

// The kinds CreateRepo takes, as §11.12 names them: the ones an app makes for a person. Owner-bound kinds and the
// network's own repos are made by their components' flows.
var sidecarEvsCreateKinds = map[string]protocol.RepoKind{
	"general": protocol.RepoKind_REPO_KIND_GENERAL,
	"record":  protocol.RepoKind_REPO_KIND_RECORD,
}

// The Sidecar closed, or the call's context ended, before an answer.
var errSidecarEvsUnavailable = errors.New("evs is unavailable: the sidecar closed or the call was cancelled")

// What SidecarEvs asks of the core's evs client: *sidecar.EvsClient through sidecarEvsClientBackend, or a test's.
type sidecarEvsBackend interface {
	// sidecar.EvsClient.CreateRepo, answering the new repo's id.
	CreateRepo(ctx context.Context, args *sidecar.EvsCreateRepoArgs) (sidecar.Id, error)
	// sidecar.EvsRepo.ReadHead of repoId.
	ReadHead(ctx context.Context, repoId sidecar.Id, args *sidecar.EvsReadArgs, out io.Writer) (*sidecar.EvsFileInfo, error)
	// sidecar.EvsRepo.ReadVersion of repoId.
	ReadVersion(ctx context.Context, repoId sidecar.Id, args *sidecar.EvsReadArgs, out io.Writer) (*sidecar.EvsFileInfo, error)
	// sidecar.EvsRepo.History of repoId.
	History(ctx context.Context, repoId sidecar.Id, args *sidecar.EvsReadArgs) (*sidecar.EvsHistory, error)
	// sidecar.EvsRepo.Write of repoId.
	Write(ctx context.Context, repoId sidecar.Id, args *sidecar.EvsWriteArgs) (*sidecar.EvsWriteResult, error)
	// sidecar.EvsRepo.Erasures of repoId.
	Erasures(ctx context.Context, repoId sidecar.Id) ([]*sidecar.SidecarErasure, error)
	// Leases every ref of repoId whole (sidecar.EvsRepo.Subscribe), answering what ends the lease.
	Subscribe(ctx context.Context, repoId sidecar.Id) (func(), error)
	// sidecar.EvsClient.AddRepoChangeListener for repoId.
	AddRepoChangeListener(repoId sidecar.Id, listener sidecar.EvsRepoChangeFunction) func()
	// D-SDK-29's check on this device's newest verified state (sidecar_evs_drop.go): nil when this device may take act
	// on repoId now, else why not, an error that is sidecar.ErrEvsDropRefused.
	MayDrop(repoId sidecar.Id, act sidecarEvsDropAct) error
	// sidecar.EvsRepo.DropHistoryPreview of repoId.
	DropHistoryPreview(ctx context.Context, repoId sidecar.Id, args *sidecar.EvsDropPreviewArgs) (*sidecar.EvsDropPreview, error)
	// sidecar.EvsRepo.DropHistory of repoId.
	DropHistory(ctx context.Context, repoId sidecar.Id, args *sidecar.EvsDropHistoryArgs) (*sidecar.EvsDropHistoryResult, error)
	// sidecar.EvsRepo.CancelDrop of repoId.
	CancelDrop(ctx context.Context, repoId sidecar.Id, args *sidecar.EvsCancelDropArgs) (*sidecar.EvsCancelDropResult, error)
	// repoId's drops on every ref, each named against the paths this device names on main: sidecar.EvsRepo.KnownPaths,
	// then sidecar.EvsRepo.Drops.
	Drops(ctx context.Context, repoId sidecar.Id) ([]*sidecar.EvsDrop, error)
}

// sidecarEvsBackend over the core's client.
type sidecarEvsClientBackend struct {
	client *sidecar.EvsClient
	// D-SDK-29's check (sidecar_evs_drop.go), on the verified state the composition that built client gives it: nil
	// until a host composes one, and every preview, drop and cancel is refused meanwhile.
	dropGate *sidecarEvsDropGate
}

// See sidecarEvsBackend.
func (self *sidecarEvsClientBackend) CreateRepo(ctx context.Context, args *sidecar.EvsCreateRepoArgs) (sidecar.Id, error) {
	repo, err := self.client.CreateRepo(ctx, args)
	if err != nil {
		return sidecar.Id{}, err
	}
	return repo.RepoId(), nil
}

// See sidecarEvsBackend.
func (self *sidecarEvsClientBackend) ReadHead(ctx context.Context, repoId sidecar.Id, args *sidecar.EvsReadArgs, out io.Writer) (*sidecar.EvsFileInfo, error) {
	return self.client.Repo(repoId).ReadHead(ctx, args, out)
}

// See sidecarEvsBackend.
func (self *sidecarEvsClientBackend) ReadVersion(ctx context.Context, repoId sidecar.Id, args *sidecar.EvsReadArgs, out io.Writer) (*sidecar.EvsFileInfo, error) {
	return self.client.Repo(repoId).ReadVersion(ctx, args, out)
}

// See sidecarEvsBackend.
func (self *sidecarEvsClientBackend) History(ctx context.Context, repoId sidecar.Id, args *sidecar.EvsReadArgs) (*sidecar.EvsHistory, error) {
	return self.client.Repo(repoId).History(ctx, args)
}

// See sidecarEvsBackend.
func (self *sidecarEvsClientBackend) Write(ctx context.Context, repoId sidecar.Id, args *sidecar.EvsWriteArgs) (*sidecar.EvsWriteResult, error) {
	return self.client.Repo(repoId).Write(ctx, args)
}

// See sidecarEvsBackend.
func (self *sidecarEvsClientBackend) Erasures(ctx context.Context, repoId sidecar.Id) ([]*sidecar.SidecarErasure, error) {
	return self.client.Repo(repoId).Erasures(ctx)
}

// See sidecarEvsBackend.
func (self *sidecarEvsClientBackend) Subscribe(ctx context.Context, repoId sidecar.Id) (func(), error) {
	subscription, err := self.client.Repo(repoId).Subscribe(ctx, &sidecar.EvsSubscribeArgs{})
	if err != nil {
		return nil, err
	}
	return subscription.Close, nil
}

// See sidecarEvsBackend.
func (self *sidecarEvsClientBackend) AddRepoChangeListener(repoId sidecar.Id, listener sidecar.EvsRepoChangeFunction) func() {
	return self.client.AddRepoChangeListener(&repoId, listener)
}

// See sidecarEvsBackend.
func (self *sidecarEvsClientBackend) MayDrop(repoId sidecar.Id, act sidecarEvsDropAct) error {
	return self.dropGate.check(repoId, act)
}

// See sidecarEvsBackend.
func (self *sidecarEvsClientBackend) DropHistoryPreview(ctx context.Context, repoId sidecar.Id, args *sidecar.EvsDropPreviewArgs) (*sidecar.EvsDropPreview, error) {
	return self.client.Repo(repoId).DropHistoryPreview(ctx, args)
}

// See sidecarEvsBackend.
func (self *sidecarEvsClientBackend) DropHistory(ctx context.Context, repoId sidecar.Id, args *sidecar.EvsDropHistoryArgs) (*sidecar.EvsDropHistoryResult, error) {
	return self.client.Repo(repoId).DropHistory(ctx, args)
}

// See sidecarEvsBackend.
func (self *sidecarEvsClientBackend) CancelDrop(ctx context.Context, repoId sidecar.Id, args *sidecar.EvsCancelDropArgs) (*sidecar.EvsCancelDropResult, error) {
	return self.client.Repo(repoId).CancelDrop(ctx, args)
}

// See sidecarEvsBackend.
func (self *sidecarEvsClientBackend) Drops(ctx context.Context, repoId sidecar.Id) ([]*sidecar.EvsDrop, error) {
	repo := self.client.Repo(repoId)
	known, err := repo.KnownPaths(ctx, nil)
	if err != nil {
		return nil, err
	}
	return repo.Drops(ctx, &sidecar.EvsDropsArgs{KnownPaths: known})
}

// Uses the enrolled network's authenticated replica, deferring until the core has adopted its identity.
func newSidecarEvsComponent(ctx context.Context, local *sidecar.Sidecar, settings *SidecarSettings) (sidecar.SidecarComponent, error) {
	replica, err := local.OpenMemberReplica(ctx)
	if errors.Is(err, sidecar.ErrSidecarNoIdentity) {
		return nil, fmt.Errorf("%w: %w", errSidecarComponentNotEnrolled, err)
	}
	if err != nil {
		return nil, err
	}
	return replica.EvsComponent(), nil
}

// One read's terminal answer: the file's bytes and the version they are, or why not. json in the C ABI and js.
type SidecarBytes struct {
	// The read answered the file.
	Ok bool `json:"ok"`
	// Its content.
	Data []byte `json:"data,omitempty"`
	// The path's version, and the ref version its push committed at.
	Version       int64 `json:"version,omitempty"`
	CommitVersion int64 `json:"commit_version,omitempty"`
	// When its push was signed, in unix milliseconds, and by which sidecar.
	TimeMillis      int64 `json:"time_millis,omitempty"`
	SignerSidecarId *Id   `json:"signer_sidecar_id,omitempty"`
	// An EVS_ code, for a refusal that has one.
	Code string `json:"code,omitempty"`
	// What went wrong, for a person. Never a token or a key (I-7).
	Message string `json:"message,omitempty"`
}

// Answers a read once.
type SidecarBytesCallback connect.ApiCallback[*SidecarBytes]

// One version of a path (§11.7's history row). json in the C ABI and js.
type SidecarHistoryVersion struct {
	Version       int64 `json:"version"`
	CommitVersion int64 `json:"commit_version"`
	// When its push was signed, in unix milliseconds, and by which sidecar.
	TimeMillis      int64 `json:"time_millis,omitempty"`
	SignerSidecarId *Id   `json:"signer_sidecar_id,omitempty"`
	// The removal that ended the path at this version.
	Removed bool `json:"removed,omitempty"`
	// on (every version kept), off (versionless) or index (the search index's own, §11.14); format 1's are all on.
	Versioning string `json:"versioning"`
	// kept; collected, a replaced versionless version evs no longer keeps; drop_pending, a version a drop lists,
	// readable until DropNotBeforeMillis; dropped, from then on, nothing of it fetched (§11.7's states row, §11.9). A
	// drop never takes the head, and a drop a hold defers reads dropped like any other (owner decision 184).
	State string `json:"state"`
	// For drop_pending and dropped: the drop that lists the version, and its window's end in unix milliseconds, which
	// an app shows as "drop pending until <time>" and CancelDrop names.
	DropId              *Id   `json:"drop_id,omitempty"`
	DropNotBeforeMillis int64 `json:"drop_not_before_millis,omitempty"`
}

// A path's versions, newest first.
type SidecarHistoryVersionList struct {
	exportedList[*SidecarHistoryVersion]
}

// An empty list.
func NewSidecarHistoryVersionList() *SidecarHistoryVersionList {
	return &SidecarHistoryVersionList{
		exportedList: *newExportedList[*SidecarHistoryVersion](),
	}
}

// One History call's terminal answer. json in the C ABI and js.
type SidecarHistory struct {
	Ok       bool                       `json:"ok"`
	Versions *SidecarHistoryVersionList `json:"versions,omitempty"`
	// The ref's head the walk verified.
	HeadVersion int64  `json:"head_version,omitempty"`
	Code        string `json:"code,omitempty"`
	Message     string `json:"message,omitempty"`
}

// Answers a History call once.
type SidecarHistoryCallback connect.ApiCallback[*SidecarHistory]

// A repo's erasures.
type SidecarErasureList struct {
	exportedList[*SidecarErasure]
}

// An empty list.
func NewSidecarErasureList() *SidecarErasureList {
	return &SidecarErasureList{
		exportedList: *newExportedList[*SidecarErasure](),
	}
}

// One Erasures call's terminal answer. json in the C ABI and js.
type SidecarErasures struct {
	Ok       bool                `json:"ok"`
	Erasures *SidecarErasureList `json:"erasures,omitempty"`
	Code     string              `json:"code,omitempty"`
	Message  string              `json:"message,omitempty"`
}

// Answers an Erasures call once.
type SidecarErasuresCallback connect.ApiCallback[*SidecarErasures]

// One commit a repo saw, or a gap. json in the C ABI and js.
type SidecarRepoChange struct {
	RepoId *Id `json:"repo_id"`
	// The ref, nil for a listener's own gap.
	RefId *Id `json:"ref_id,omitempty"`
	// The ref version the commit landed at; on the core's gap, the head's.
	Version         int64 `json:"version,omitempty"`
	PushId          *Id   `json:"push_id,omitempty"`
	SignerSidecarId *Id   `json:"signer_sidecar_id,omitempty"`
	// Which commits came between is not known: read the repo again.
	Gap bool `json:"gap,omitempty"`
	// The push moved the ref's path epoch.
	PathEpoch bool `json:"path_epoch,omitempty"`
}

// App-implemented: called on a Go thread with each change, in order.
type SidecarRepoChangeListener interface {
	RepoChanged(change *SidecarRepoChange)
}

// One network's evs for an app. Safe for concurrent use.
type SidecarEvs struct {
	ctx      context.Context
	cancel   context.CancelFunc
	backend  sidecarEvsBackend
	settings *sidecarEvsSettings
	// The stock component only serves the enrolled member after its anchor has verified.
	core *sidecar.Sidecar

	stateLock sync.Mutex
	// each repo's lease, taken at its first listener and ended at close
	repoIdLeases map[sidecar.Id]func()
	// repos whose lease is being taken
	leasingRepoIds map[sidecar.Id]bool
	// each listener's delivery, ended at close
	dispatches map[*sidecarRepoChangeDispatch]bool
	closed     bool
	// told once a lease's goroutine has kept or dropped the lease: a test's hook, nil otherwise
	leaseSettled func(repoId sidecar.Id, kept bool)
}

// A SidecarEvs on backend until ctx ends or close.
func newSidecarEvs(ctx context.Context, backend sidecarEvsBackend, settings *sidecarEvsSettings) *SidecarEvs {
	cancelCtx, cancel := context.WithCancel(ctx)
	return &SidecarEvs{
		ctx:            cancelCtx,
		cancel:         cancel,
		backend:        backend,
		settings:       settings,
		repoIdLeases:   map[sidecar.Id]func(){},
		leasingRepoIds: map[sidecar.Id]bool{},
		dispatches:     map[*sidecarRepoChangeDispatch]bool{},
	}
}

// Ends every lease and listener: no change reaches a listener after it returns, and a call in flight answers
// EVS_UNAVAILABLE. Safe to call again.
func (self *SidecarEvs) close() {
	leases, dispatches := func() ([]func(), []*sidecarRepoChangeDispatch) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return nil, nil
		}
		self.closed = true
		leases := []func(){}
		for _, endLease := range self.repoIdLeases {
			leases = append(leases, endLease)
		}
		dispatches := []*sidecarRepoChangeDispatch{}
		for dispatch := range self.dispatches {
			dispatches = append(dispatches, dispatch)
		}
		clear(self.repoIdLeases)
		clear(self.dispatches)
		return leases, dispatches
	}()
	for _, dispatch := range dispatches {
		dispatch.close()
	}
	self.cancel()
	for _, endLease := range leases {
		endLease()
	}
}

// The attached EVS client, with drop operations judged against the live verified config and identity.
func (self *Sidecar) GetEvs() *SidecarEvs {
	evs, err := traceWithReturn(func() *SidecarEvs {
		component, ok := self.component(SidecarComponentEvs).(*sidecar.EvsComponent)
		if !ok || component == nil {
			return nil
		}
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return nil
		}
		if self.evs == nil {
			backend := &sidecarEvsClientBackend{client: component.Client()}
			if local := self.local; local != nil {
				backend.dropGate = newSidecarEvsDropGate(func() sidecar.GkmAuthView {
					if follower := local.ConfigFollower(); follower != nil {
						if config := follower.Client(); config != nil {
							if state := config.State(); state != nil {
								return state
							}
						}
					}
					return nil
				}, local.Identity, time.Now)
			}
			self.evs = newSidecarEvs(self.ctx, backend, self.evsSettings)
			self.evs.core = self.local
		}
		return self.evs
	})
	if err != nil {
		return nil
	}
	return evs
}

// Runs call on a goroutine of its own and answers callback once with what it answers, or with call's panic as the
// error.
func sidecarEvsAnswer[T any](call func() T, callback connect.ApiCallback[T]) {
	go func() {
		result, err := traceWithReturnError(func() (T, error) {
			return call(), nil
		})
		if callback != nil {
			callback.Result(result, err)
		}
	}()
}

// The call's context: the SidecarEvs's, which close ends.
func (self *SidecarEvs) callCtx() (context.Context, error) {
	if err := self.ctx.Err(); err != nil {
		return nil, errSidecarEvsUnavailable
	}
	if self.core != nil {
		replica := self.core.MemberReplica()
		if replica == nil || replica.Evs() == nil {
			return nil, sidecar.ErrEvsNoConfig
		}
	}
	return self.ctx, nil
}

// §18.5's code and the message for err (the file's comment): "" for an error with no code. The core's own refusals
// come before evs's answer they may wrap: a drop that went stale on a 409 is EVS_DROP_STALE, not the 409.
func sidecarEvsCodeOf(err error) string {
	var conflict *sidecar.EvsPathConflictError
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errSidecarEvsUnavailable), errors.Is(err, sidecar.ErrSidecarClosed),
		errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return sidecarEvsCodeUnavailable
	}
	if code := sidecarEvsFormat2CodeOf(err); code != "" {
		return code
	}
	switch {
	case errors.Is(err, sidecar.ErrEvsGitRef):
		return sidecarEvsCodeGitRef
	case errors.Is(err, sidecar.ErrEvsErased):
		return sidecarEvsCodeErased
	case errors.Is(err, sidecar.ErrEvsPathAbsent):
		return sidecarEvsCodeNotFound
	case errors.Is(err, sidecar.ErrEvsFormatUnsupported):
		return sidecarEvsCodeFormatUnsupported
	case errors.Is(err, sidecar.ErrEvsReadStale):
		return sidecarEvsCodeReadStale
	case errors.As(err, &conflict):
		return sidecarEvsCodePathConflict
	case errors.Is(err, sidecar.ErrEvsPrefixRule), errors.Is(err, sidecar.ErrAclPrefixRule):
		// the planner's and a reader's, and publish's with its details base and tag (QD-801)
		return sidecarEvsCodePrefixRule
	}
	if refusal, ok := sidecar.EvsErrorOf(err); ok && refusal.Code != protocol.EvsRefusalCode_EVS_REFUSAL_CODE_UNSPECIFIED {
		return "EVS_" + strings.TrimPrefix(refusal.Code.String(), "EVS_REFUSAL_")
	}
	if errors.Is(err, sidecar.ErrEvsWriteInvalid) {
		return sidecarEvsCodeInvalid
	}
	return ""
}

// A refusal's message, for a person.
func sidecarEvsMessageOf(err error) string {
	if errors.Is(err, sidecar.ErrEvsErased) {
		return "this version was erased"
	}
	return err.Error()
}

// The repo an *Id names, or an error for none.
func sidecarEvsRepoIdOf(repoId *Id) (sidecar.Id, error) {
	if repoId == nil {
		return sidecar.Id{}, fmt.Errorf("%w: no repo", sidecar.ErrEvsWriteInvalid)
	}
	return sidecar.Id(repoId.id), nil
}

// Makes a repo of kind, general or record, named displayName for its directory, and answers its repo_id in the
// result's Value (§11.12). In a team network the repo is announced at creation unless private, which writes the
// directory's private marker instead (owner decision 181; the core's RepoCreated and PrivateCreate).
func (self *SidecarEvs) CreateRepo(kind string, displayName string, private bool, callback SidecarResultCallback) {
	trace(func() {
		sidecarEvsAnswer(func() *SidecarResult {
			ctx, err := self.callCtx()
			if err != nil {
				return sidecarEvsResultOf("", err)
			}
			repoKind, ok := sidecarEvsCreateKinds[kind]
			if !ok {
				return &SidecarResult{Code: sidecarEvsCodeInvalid, Message: fmt.Sprintf("an app makes general or record repos, not %q", kind)}
			}
			repoId, err := self.backend.CreateRepo(ctx, &sidecar.EvsCreateRepoArgs{
				Kind:        repoKind,
				DisplayName: displayName,
				Private:     private,
			})
			if err != nil {
				return sidecarEvsResultOf("", err)
			}
			return sidecarEvsResultOf(repoId.String(), nil)
		}, connect.ApiCallback[*SidecarResult](callback))
	})
}

// A SidecarResult for a call that answered value or err: sidecarResultOf's, with §18.5's EVS_ code where err has one,
// and for EVS_PREFIX_RULE the path the rules hold in Value, what the refusal names.
func sidecarEvsResultOf(value string, err error) *SidecarResult {
	result := sidecarResultOf(value, err)
	if err != nil && result.Code == "" {
		result.Code = sidecarEvsCodeOf(err)
	}
	if result.Code == sidecarEvsCodeErased {
		result.Message = sidecarEvsMessageOf(err)
	}
	if err != nil && result.Code == sidecarEvsCodePrefixRule {
		result.Value = sidecarEvsPrefixRulePath(err)
	}
	return result
}

// The path an EVS_PREFIX_RULE names, the planner's or a reader's and publish's alike, empty for none.
func sidecarEvsPrefixRulePath(err error) string {
	var evsErr *sidecar.EvsPrefixRuleError
	var aclErr *sidecar.AclPrefixRuleError
	switch {
	case errors.As(err, &evsErr):
		return evsErr.Path
	case errors.As(err, &aclErr):
		return aclErr.Path
	}
	return ""
}

// A writer that keeps at most maxByteCount bytes and refuses the write past them, so a read stops at the bound.
type sidecarEvsBoundedBuffer struct {
	buffer       bytes.Buffer
	maxByteCount int
	over         bool
}

func (self *sidecarEvsBoundedBuffer) Write(p []byte) (int, error) {
	if self.maxByteCount-self.buffer.Len() < len(p) {
		self.over = true
		return 0, fmt.Errorf("a file past %d bytes", self.maxByteCount)
	}
	return self.buffer.Write(p)
}

// Reads path's newest version on repoId's main ref (§11.9, §18.5): its bytes, verified against their signed statement
// in the core, which derives the tokens and decrypts. A versionless file replaced mid-read is read again from the new
// head there (§11.7). A file past ReadMaxByteCount answers EVS_TOO_LARGE.
func (self *SidecarEvs) ReadHead(repoId *Id, path string, callback SidecarBytesCallback) {
	trace(func() {
		sidecarEvsAnswer(func() *SidecarBytes {
			return self.read(repoId, &sidecar.EvsReadArgs{Path: path}, self.backend.ReadHead)
		}, connect.ApiCallback[*SidecarBytes](callback))
	})
}

// Reads an older version of path on repoId's main ref by its number (§18.5, §11.9), verified and reconstructed in the
// core as ReadHead's is. A version a drop lists reads as any other until its drop's not_before by this device's
// clock, History naming that time, and from then on answers EVS_DROPPED with nothing fetched, unless the drop is
// cancelled or evs reports it refused; the head is never one (§11.7's states row). A replaced versionless version
// answers EVS_COLLECTED, after the core's fresh read shows its replacement. A version below 1 answers EVS_INVALID.
func (self *SidecarEvs) ReadVersion(repoId *Id, path string, version int64, callback SidecarBytesCallback) {
	trace(func() {
		sidecarEvsAnswer(func() *SidecarBytes {
			if version < 1 {
				return &SidecarBytes{Code: sidecarEvsCodeInvalid, Message: fmt.Sprintf("version %d names no version: the first is 1", version)}
			}
			return self.read(repoId, &sidecar.EvsReadArgs{Path: path, Version: uint64(version)}, self.backend.ReadVersion)
		}, connect.ApiCallback[*SidecarBytes](callback))
	})
}

// One read's answer through read, its bytes held to ReadMaxByteCount.
func (self *SidecarEvs) read(
	repoId *Id,
	args *sidecar.EvsReadArgs,
	read func(ctx context.Context, repoId sidecar.Id, args *sidecar.EvsReadArgs, out io.Writer) (*sidecar.EvsFileInfo, error),
) *SidecarBytes {
	refuse := func(err error) *SidecarBytes {
		return &SidecarBytes{Code: sidecarEvsCodeOf(err), Message: sidecarEvsMessageOf(err)}
	}
	ctx, err := self.callCtx()
	if err != nil {
		return refuse(err)
	}
	coreRepoId, err := sidecarEvsRepoIdOf(repoId)
	if err != nil {
		return refuse(err)
	}
	out := &sidecarEvsBoundedBuffer{maxByteCount: self.settings.ReadMaxByteCount}
	info, err := read(ctx, coreRepoId, args, out)
	if out.over {
		return &SidecarBytes{Code: sidecarEvsCodeTooLarge, Message: fmt.Sprintf("the file is larger than the %d bytes an app reads at once", self.settings.ReadMaxByteCount)}
	}
	if err != nil {
		return refuse(err)
	}
	return &SidecarBytes{
		Ok:              true,
		Data:            out.buffer.Bytes(),
		Version:         int64(info.Version),
		CommitVersion:   int64(info.CommitVersion),
		TimeMillis:      sidecarMillisOf(info.Time),
		SignerSidecarId: newId([16]byte(info.SignerSidecarId)),
	}
}

// Each version of path on repoId's main ref, newest first (§11.7, §11.9): its time, signer, versioning and state,
// kept, collected, drop_pending or dropped, the core judging a listed version against its drop's not_before on this
// device's clock; a dropped version keeps its row, its content gone.
func (self *SidecarEvs) History(repoId *Id, path string, callback SidecarHistoryCallback) {
	trace(func() {
		sidecarEvsAnswer(func() *SidecarHistory {
			refuse := func(err error) *SidecarHistory {
				return &SidecarHistory{Code: sidecarEvsCodeOf(err), Message: sidecarEvsMessageOf(err)}
			}
			ctx, err := self.callCtx()
			if err != nil {
				return refuse(err)
			}
			coreRepoId, err := sidecarEvsRepoIdOf(repoId)
			if err != nil {
				return refuse(err)
			}
			history, err := self.backend.History(ctx, coreRepoId, &sidecar.EvsReadArgs{Path: path})
			if err != nil {
				return refuse(err)
			}
			versions := NewSidecarHistoryVersionList()
			for _, version := range history.Versions {
				versions.Add(sidecarHistoryVersionOf(version))
			}
			return &SidecarHistory{Ok: true, Versions: versions, HeadVersion: int64(history.HeadVersion)}
		}, connect.ApiCallback[*SidecarHistory](callback))
	})
}

// The bound row of one of the core's history versions: its state as the core judged it on this device's clock, and
// for a version a drop lists, that drop and its window's end.
func sidecarHistoryVersionOf(version *sidecar.EvsHistoryVersion) *SidecarHistoryVersion {
	row := &SidecarHistoryVersion{
		Version:         int64(version.Version),
		CommitVersion:   int64(version.CommitVersion),
		TimeMillis:      sidecarMillisOf(version.Time),
		SignerSidecarId: newId([16]byte(version.SignerSidecarId)),
		Removed:         version.Removed,
		Versioning:      sidecarEvsVersioningOf(version.Versioning),
		State:           version.State,
	}
	if version.State == sidecar.EvsVersionStateDropPending || version.State == sidecar.EvsVersionStateDropped {
		if version.DropId != (sidecar.Id{}) {
			row.DropId = newId([16]byte(version.DropId))
		}
		row.DropNotBeforeMillis = sidecarMillisOf(version.DropNotBefore)
	}
	return row
}

// The versioning settings a version carries (§11.7), as §18.5 names them.
const (
	sidecarEvsVersioningOn    = "on"
	sidecarEvsVersioningOff   = "off"
	sidecarEvsVersioningIndex = "index"
)

// A version's versioning as §18.5 names it: the core's 0 is on, 1 off and 2 the search index's own; a value no format
// gives reads as off, which claims no version is kept.
func sidecarEvsVersioningOf(versioning uint32) string {
	switch versioning {
	case 0:
		return sidecarEvsVersioningOn
	case 2:
		return sidecarEvsVersioningIndex
	}
	return sidecarEvsVersioningOff
}

// Writes data to path on repoId's main ref, a file-mode ref (§11.7, §18.5): at most WriteMaxByteCount bytes, in one
// push, the result naming the path's new version in Value. versioning is "", "on" or "off": "" keeps the path's and
// gives a new path its rules' setting, "on" keeps every version and "off" makes the path versionless (owner decision
// 167), which the core refuses EVS_FORMAT_UNSUPPORTED outside the network's format 2 gate; any other value answers
// EVS_VERSIONING before the core sees it. A git-mode ref answers EVS_GIT_REF, and a versionless path's 409 comes back
// as EVS_PATH_CONFLICT, never retried by rebasing: the loser's content would replace the winner's with no history to
// recover it (§11.7's writers row).
func (self *SidecarEvs) Write(repoId *Id, path string, data []byte, versioning string, callback SidecarResultCallback) {
	trace(func() {
		// the bytes are the caller's to reuse once Write returns
		dataCopy := bytes.Clone(data)
		sidecarEvsAnswer(func() *SidecarResult {
			ctx, err := self.callCtx()
			if err != nil {
				return sidecarEvsResultOf("", err)
			}
			switch versioning {
			case sidecar.EvsVersioningKeepSetting, sidecar.EvsVersioningOn, sidecar.EvsVersioningOff:
			default:
				return &SidecarResult{Code: sidecarEvsCodeVersioning, Message: fmt.Sprintf("a write's versioning is \"\", \"on\" or \"off\", not %q", versioning)}
			}
			if self.settings.WriteMaxByteCount < len(dataCopy) {
				return &SidecarResult{Code: sidecarEvsCodeTooLarge, Message: fmt.Sprintf("an app writes at most %d bytes at once", self.settings.WriteMaxByteCount)}
			}
			coreRepoId, err := sidecarEvsRepoIdOf(repoId)
			if err != nil {
				return sidecarEvsResultOf("", err)
			}
			result, err := self.backend.Write(ctx, coreRepoId, &sidecar.EvsWriteArgs{
				Path:       path,
				Content:    bytes.NewReader(dataCopy),
				Size:       int64(len(dataCopy)),
				Versioning: versioning,
			})
			if err != nil {
				return sidecarEvsResultOf("", err)
			}
			return sidecarEvsResultOf(strconv.FormatUint(result.PathVersion, 10), nil)
		}, connect.ApiCallback[*SidecarResult](callback))
	})
}

// Each erasure repoId's erasure sets list or this device runs (§5.4's SidecarErasure, §11.12, §11.13's wait row).
func (self *SidecarEvs) Erasures(repoId *Id, callback SidecarErasuresCallback) {
	trace(func() {
		sidecarEvsAnswer(func() *SidecarErasures {
			refuse := func(err error) *SidecarErasures {
				return &SidecarErasures{Code: sidecarEvsCodeOf(err), Message: sidecarEvsMessageOf(err)}
			}
			ctx, err := self.callCtx()
			if err != nil {
				return refuse(err)
			}
			coreRepoId, err := sidecarEvsRepoIdOf(repoId)
			if err != nil {
				return refuse(err)
			}
			erasures, err := self.backend.Erasures(ctx, coreRepoId)
			if err != nil {
				return refuse(err)
			}
			list := NewSidecarErasureList()
			for _, erasure := range erasures {
				list.Add(sidecarErasureOf(erasure))
			}
			return &SidecarErasures{Ok: true, Erasures: list}
		}, connect.ApiCallback[*SidecarErasures](callback))
	})
}

// The bound erasure of the core's.
func sidecarErasureOf(erasure *sidecar.SidecarErasure) *SidecarErasure {
	return &SidecarErasure{
		ErasureId:     newId([16]byte(erasure.ErasureId)),
		State:         erasure.State,
		ObjectsLeft:   erasure.ObjectsLeft,
		BytesLeft:     erasure.BytesLeft,
		NextTryMillis: sidecarMillisOf(erasure.NextTry),
		FinishMillis:  sidecarMillisOf(erasure.Finish),
	}
}

// Calls listener with each commit the core verifies on any ref of repoId, once each, in order, on a Go thread, until
// the answer closes (the file's comment). The repo is leased at its first listener.
func (self *SidecarEvs) AddRepoChangeListener(repoId *Id, listener SidecarRepoChangeListener) Sub {
	sub, err := traceWithReturn(func() Sub {
		if repoId == nil || listener == nil {
			return newSub(func() {})
		}
		coreRepoId := sidecar.Id(repoId.id)
		dispatch := newSidecarRepoChangeDispatch(self.ctx, coreRepoId, listener, self.settings.ChangeQueueCount)
		added := func() bool {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if self.closed {
				return false
			}
			self.dispatches[dispatch] = true
			return true
		}()
		if !added {
			dispatch.close()
			return newSub(func() {})
		}
		removeListener := self.backend.AddRepoChangeListener(coreRepoId, func(change *sidecar.EvsRepoChange) {
			if _, err := self.callCtx(); err == nil {
				dispatch.offer(change)
			}
		})
		self.lease(coreRepoId)
		var once sync.Once
		return newSub(func() {
			once.Do(func() {
				removeListener()
				dispatch.close()
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				delete(self.dispatches, dispatch)
			})
		})
	})
	if err != nil {
		return newSub(func() {})
	}
	return sub
}

// Takes repoId's lease once, on a goroutine of its own, since a lease answers once evs did: kept until close. A lease
// that fails is taken again at the repo's next listener.
func (self *SidecarEvs) lease(repoId sidecar.Id) {
	leasing := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed || self.leasingRepoIds[repoId] || self.repoIdLeases[repoId] != nil {
			return false
		}
		self.leasingRepoIds[repoId] = true
		return true
	}()
	if !leasing {
		return
	}
	go func() {
		endLease, err := traceWithReturnError(func() (func(), error) {
			if self.core != nil {
				follower := self.core.ConfigFollower()
				if follower == nil {
					return nil, sidecar.ErrEvsNoConfig
				}
				if _, err := follower.WaitStanding(self.ctx); err != nil {
					return nil, err
				}
			}
			ctx, err := self.callCtx()
			if err != nil {
				return nil, err
			}
			return self.backend.Subscribe(ctx, repoId)
		})
		kept, settled := func() (bool, func(sidecar.Id, bool)) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			delete(self.leasingRepoIds, repoId)
			if err != nil || endLease == nil || self.closed {
				return false, self.leaseSettled
			}
			self.repoIdLeases[repoId] = endLease
			return true, self.leaseSettled
		}()
		if !kept && err == nil && endLease != nil {
			endLease()
		}
		if settled != nil {
			settled(repoId, kept)
		}
	}()
}

// One listener's delivery: the core's callback offers each change with zero wait, and one goroutine calls the
// listener in order, under trace. Safe for concurrent use.
type sidecarRepoChangeDispatch struct {
	ctx      context.Context
	cancel   context.CancelFunc
	repoId   sidecar.Id
	listener SidecarRepoChangeListener
	changes  chan *SidecarRepoChange
	// changes not queued, owed as one gap once the queue drains
	missed atomic.Bool
	// set by close: no listener call starts after it
	closed atomic.Bool
}

// A delivery running until ctx ends or close.
func newSidecarRepoChangeDispatch(ctx context.Context, repoId sidecar.Id, listener SidecarRepoChangeListener, queueCount int) *sidecarRepoChangeDispatch {
	cancelCtx, cancel := context.WithCancel(ctx)
	self := &sidecarRepoChangeDispatch{
		ctx:      cancelCtx,
		cancel:   cancel,
		repoId:   repoId,
		listener: listener,
		changes:  make(chan *SidecarRepoChange, max(1, queueCount)),
	}
	go self.run()
	return self
}

// The core's callback, on its subscriber's goroutine: a bound copy of change offered with zero wait, or a gap owed
// when the queue is full.
func (self *sidecarRepoChangeDispatch) offer(change *sidecar.EvsRepoChange) {
	if self.closed.Load() || change.RepoId != self.repoId {
		return
	}
	bound := &SidecarRepoChange{
		RepoId:    newId([16]byte(change.RepoId)),
		RefId:     newId([16]byte(change.RefId)),
		Version:   int64(change.Version),
		Gap:       change.Gap,
		PathEpoch: change.PathEpoch,
	}
	if !change.Gap {
		bound.PushId = newId([16]byte(change.PushId))
		bound.SignerSidecarId = newId([16]byte(change.SignerSidecarId))
	}
	select {
	case self.changes <- bound:
	default:
		self.missed.Store(true)
	}
}

// Delivers queued changes in order until ctx ends, then the gap a full queue owed, once the queue has drained.
func (self *sidecarRepoChangeDispatch) run() {
	for {
		select {
		case <-self.ctx.Done():
			return
		case change := <-self.changes:
			self.deliver(change)
		}
		if len(self.changes) == 0 && self.missed.Swap(false) {
			self.deliver(&SidecarRepoChange{RepoId: newId([16]byte(self.repoId)), Gap: true})
		}
	}
}

// Calls the listener with change, unless closed.
func (self *sidecarRepoChangeDispatch) deliver(change *SidecarRepoChange) {
	if self.closed.Load() {
		return
	}
	trace(func() {
		self.listener.RepoChanged(change)
	})
}

// Ends delivery without waiting, so a listener may call it: no listener call starts after it returns.
func (self *sidecarRepoChangeDispatch) close() {
	self.closed.Store(true)
	self.cancel()
}
