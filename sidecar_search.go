//go:build !ios_extension && !js

// SidecarSearch (§18.5, §18.4's Search block; SDK-M10-01; R-SDK-13, D-SDK-22, I-13, I-19): an app's search of its
// network's repos, a thin mapping onto §5.4's calls. On the local backend it wraps the core's Sidecar.Search and its
// siblings (CORE-M10-03), whose engine the host installs; on the remote one it forwards to `ur daemon`'s control
// mode (sidecar_search_remote.go). Apps only render (D-SDK-22): assembly, masking, confirmation, ranking, indexer
// choice and merging stay in the core, and this layer maps fields.
//
//	stream   one Search streams pages to its listener on a Go thread of its own: each call replaces the last page
//	         given, as the core's pages land, until the page holds PageSize hits, the stream is done, or it waits
//	         (metered, offline, update); More(pageSize) starts the next page, from hits already ranked first. A
//	         page with done set is the last: nothing follows it
//	cancel   local: it ends the stream here and in the core, whose cursors and remote legs end with it, and nothing
//	         goes to an indexer, whose cursor a newer search supersedes (§18.5). After Cancel returns no listener
//	         call starts
//	refusal  a search that cannot run, or stops short, still answers: one page with done set, the hits found before,
//	         and progress' Code with that code's fixed message, never an error's text
//	nil      GetSearch is nil unless the core's evs component is attached on the local backend; on the remote one the
//	         daemon's search answers, unavailable where it has none
//
// A query is never logged, stored or put in a crash report here (I-19): no file of this wrapper logs, its messages are
// fixed per code, and the query lives only in the call and the stream that carries it. Safe for concurrent use.

package sdk

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/sidecar"
)

// What an app's searches are held to.
type sidecarSearchSettings struct {
	// Hits a page when a query names none: §18.5's 50.
	PageHitCount int
	// The most hits a page may ask for: 500, a reading, since a page is held whole and copied across the binding, about
	// 1 KiB a hit's display fields, inside a phone's 4 MiB working set (§18.4's targets). A larger PageSize is refused.
	PageHitMaxCount int
	// Pulls in a row that bring no hit and change nothing before the stream pauses: 4, a reading, so a stream whose
	// engine yields empty pages while a phase computes never spins.
	EmptyPullCount int
	// That pause: 250 ms, a reading.
	EmptyPullInterval time.Duration
	// The remote backend: how long each control call asks the daemon to wait for hits, §16.10's wait_ms, 2 s, its
	// MCP default.
	RemoteWaitTimeout time.Duration
	// The remote backend: the most hits one control call asks for, §16.10's limit's bound, 100.
	RemoteLimitMaxCount int
	// The remote backend: a search's own control session's bound for one call beyond RemoteWaitTimeout: 30 s, the
	// remote backend's CallTimeout.
	RemoteCallTimeout time.Duration
	// How long GetStatus waits for its answer on the caller's thread: 30 s, the remote backend's CallTimeout.
	StatusTimeout time.Duration
}

// The defaults, a fresh value each call.
func defaultSidecarSearchSettings() *sidecarSearchSettings {
	return &sidecarSearchSettings{
		PageHitCount:        50,
		PageHitMaxCount:     500,
		EmptyPullCount:      4,
		EmptyPullInterval:   250 * time.Millisecond,
		RemoteWaitTimeout:   2 * time.Second,
		RemoteLimitMaxCount: 100,
		RemoteCallTimeout:   30 * time.Second,
		StatusTimeout:       30 * time.Second,
	}
}

// §11.14's field numbers a hit's display fields carry: identifiers of the format.
const (
	sidecarSearchFieldSubject    = 1
	sidecarSearchFieldFrom       = 2
	sidecarSearchFieldTitle      = 4
	sidecarSearchFieldAgentLabel = 9
)

var (
	// The Sidecar closed, or the call's context ended, before an answer.
	errSidecarSearchUnavailable = errors.New("search is unavailable: the sidecar closed or the call was cancelled")
	// A query this wrapper does not take.
	errSidecarSearchQuery = errors.New("the search query is not one this device takes")
)

// One search's request, checked: what both backends map from.
type sidecarSearchRequest struct {
	text         string
	repoIds      []sidecar.Id
	scope        string
	since        time.Time
	until        time.Time
	fuzzy        bool
	allowMetered bool
	remote       string
	pageSize     int
}

// What a search's pages come from: the core's SearchStream, or the daemon's search method.
type sidecarSearchSource interface {
	// The next hits, which follow the last ones, with the progress now; done once no later call holds a hit.
	next(ctx context.Context) (hits []*SidecarSearchHit, progress *SidecarSearchProgress, done bool, err error)
	// Ends the search where it runs. Safe to call again.
	close()
}

// What SidecarSearch asks of its core: the local core, or the daemon.
type sidecarSearchBackend interface {
	search(ctx context.Context, request *sidecarSearchRequest) (sidecarSearchSource, error)
	status(ctx context.Context, repoId *sidecar.Id) (*SidecarSearchStatus, error)
	dropCache(ctx context.Context, repoId *sidecar.Id) error
	// SDK-M10-02's host calls (sidecar_search_host.go, sidecar_search_conditions.go).
	setPolicy(ctx context.Context, repoId *sidecar.Id, policy string) error
	setConditions(unmetered bool, charging bool, lowPower bool)
	setForeground(foreground bool)
	runBackgroundSync(ctx context.Context) error
	// SDK-M10-03's requester seams (sidecar_search_remote_host.go): none listed, and both calls refused, where the host
	// asks no indexer.
	indexers(ctx context.Context) ([]*SidecarIndexer, error)
	setRemote(ctx context.Context, mode string) error
	acknowledgeIndexer(ctx context.Context, memberId *sidecar.Id, allow bool) error
}

// One network's search for an app. Safe for concurrent use.
type SidecarSearch struct {
	ctx      context.Context
	cancel   context.CancelFunc
	backend  sidecarSearchBackend
	settings *sidecarSearchSettings
	// the Sidecar's host class and its foreground, which SDK-M10-02's calls report; nil for a test's
	host *sidecarSearchHost

	stateLock sync.Mutex
	// each open search's handle, ended at close
	handles map[*SidecarSearchHandle]bool
	closed  bool
}

// A SidecarSearch on backend until ctx ends or close.
func newSidecarSearch(ctx context.Context, backend sidecarSearchBackend, settings *sidecarSearchSettings, host *sidecarSearchHost) *SidecarSearch {
	cancelCtx, cancel := context.WithCancel(ctx)
	return &SidecarSearch{
		ctx:      cancelCtx,
		cancel:   cancel,
		backend:  backend,
		settings: settings,
		host:     host,
		handles:  map[*SidecarSearchHandle]bool{},
	}
}

// Ends every open search: no page reaches a listener after it returns, and a call in flight answers
// SEARCH_UNAVAILABLE. Safe to call again.
func (self *SidecarSearch) close() {
	handles := func() []*SidecarSearchHandle {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return nil
		}
		self.closed = true
		handles := make([]*SidecarSearchHandle, 0, len(self.handles))
		for handle := range self.handles {
			handles = append(handles, handle)
		}
		clear(self.handles)
		return handles
	}()
	for _, handle := range handles {
		handle.end()
	}
	self.cancel()
}

// This Sidecar's search (§18.5): nil unless the core's evs component is attached on the local backend, whose search
// reads evs's repos; on the remote backend the daemon's, which answers unavailable where it runs none.
func (self *Sidecar) GetSearch() *SidecarSearch {
	search, err := traceWithReturn(func() *SidecarSearch {
		local, remote := self.backends()
		existing, settings, backend, closed := func() (*SidecarSearch, *sidecarSearchSettings, sidecarSearchBackend, bool) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			return self.search, self.searchSettings, self.searchBackend, self.closed
		}()
		if closed {
			return nil
		}
		if existing != nil {
			return existing
		}
		if settings == nil {
			settings = defaultSidecarSearchSettings()
		}
		if backend == nil {
			switch {
			case local != nil:
				if component, ok := self.component(SidecarComponentEvs).(*sidecar.EvsComponent); !ok || component == nil {
					return nil
				}
				backend = &sidecarSearchCoreBackend{local: local}
			case remote != nil:
				backend = &sidecarSearchRemoteBackend{remote: remote, settings: settings}
			default:
				return nil
			}
		}
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return nil
		}
		if self.search == nil {
			self.search = newSidecarSearch(self.ctx, backend, settings, self.searchHost)
		}
		return self.search
	})
	if err != nil {
		return nil
	}
	return search
}

// The request query asks for, checked against settings, or why not.
func sidecarSearchRequestOf(query *SidecarSearchQuery, settings *sidecarSearchSettings) (*sidecarSearchRequest, error) {
	if query == nil {
		return nil, errSidecarSearchQuery
	}
	request := &sidecarSearchRequest{
		text:         query.Text,
		fuzzy:        query.Fuzzy,
		allowMetered: query.AllowMetered,
		pageSize:     int(query.PageSize),
	}
	switch query.Scope {
	case "", SidecarSearchScopeCached:
		request.scope = SidecarSearchScopeCached
	case SidecarSearchScopeLatest, SidecarSearchScopeAll:
		request.scope = query.Scope
	default:
		return nil, errSidecarSearchQuery
	}
	switch query.Remote {
	case "", SidecarSearchRemoteOff:
		request.remote = query.Remote
	default:
		return nil, errSidecarSearchQuery
	}
	if query.SinceMillis < 0 || query.UntilMillis < 0 || (0 < query.UntilMillis && query.UntilMillis < query.SinceMillis) {
		return nil, errSidecarSearchQuery
	}
	if 0 < query.SinceMillis {
		request.since = time.UnixMilli(query.SinceMillis).UTC()
	}
	if 0 < query.UntilMillis {
		request.until = time.UnixMilli(query.UntilMillis).UTC()
	}
	switch {
	case query.PageSize < 0 || settings.PageHitMaxCount < int(query.PageSize):
		return nil, errSidecarSearchQuery
	case query.PageSize == 0:
		request.pageSize = settings.PageHitCount
	}
	if query.RepoIds != nil {
		for _, repoId := range query.RepoIds.getAll() {
			if repoId == nil {
				return nil, errSidecarSearchQuery
			}
			request.repoIds = append(request.repoIds, sidecar.Id(repoId.id))
		}
	}
	return request, nil
}

// The code err stands for: "" for none. Its text goes nowhere.
func sidecarSearchCodeOf(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errSidecarSearchUnavailable), errors.Is(err, sidecar.ErrSidecarClosed),
		errors.Is(err, sidecar.ErrSidecarSearchUnavailable), errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded), errors.Is(err, errSidecarRemoteOffline),
		errors.Is(err, errSidecarRemoteClosed):
		return SidecarSearchCodeUnavailable
	case errors.Is(err, errSidecarSearchQuery), errors.Is(err, sidecar.ErrSearchRequest),
		errors.Is(err, sidecar.ErrSearchQueryTooLong), errors.Is(err, sidecar.ErrSearchStateArgs),
		errors.Is(err, sidecar.ErrSearchCursorInvalid):
		return SidecarSearchCodeInvalid
	case errors.Is(err, sidecar.ErrSearchBusy):
		return SidecarSearchCodeBusy
	case errors.Is(err, sidecar.ErrSearchCursorExpired):
		return SidecarSearchCodeExpired
	case errors.Is(err, sidecar.ErrSidecarSearchRefused), errors.Is(err, errSidecarSearchRemoteRefused):
		return SidecarSearchCodeRefused
	}
	var refusal *sidecarControlError
	if errors.As(err, &refusal) {
		return sidecarSearchControlCodeOf(refusal)
	}
	return SidecarSearchCodeFailed
}

// A call's result from err: Ok, or its code with that code's fixed message.
func sidecarSearchResultOf(err error) *SidecarResult {
	if err == nil {
		return &SidecarResult{Ok: true}
	}
	code := sidecarSearchCodeOf(err)
	return &SidecarResult{Code: code, Message: sidecarSearchCodeMessages[code]}
}

// The call's context: the SidecarSearch's, which close ends.
func (self *SidecarSearch) callCtx() (context.Context, error) {
	if err := self.ctx.Err(); err != nil {
		return nil, errSidecarSearchUnavailable
	}
	return self.ctx, nil
}

// Runs query and streams its pages to listener (the file's comment). A query that cannot run answers one page with
// done set and its code. The handle is never nil.
func (self *SidecarSearch) Search(query *SidecarSearchQuery, listener SidecarSearchListener) *SidecarSearchHandle {
	handle, err := traceWithReturn(func() *SidecarSearchHandle {
		return self.startSearch(query, listener)
	})
	if err != nil || handle == nil {
		// a panic in the wrapper: a handle that does nothing, and one refusal
		handle = newSidecarSearchHandle(context.Background(), self, listener, 0)
		handle.refuse(nil, errSidecarSearchUnavailable)
	}
	return handle
}

// Search's body: the request checked, the handle registered, the stream started on a goroutine of its own.
func (self *SidecarSearch) startSearch(query *SidecarSearchQuery, listener SidecarSearchListener) *SidecarSearchHandle {
	request, err := sidecarSearchRequestOf(query, self.settings)
	pageSize := self.settings.PageHitCount
	if request != nil {
		pageSize = request.pageSize
	}
	handle := newSidecarSearchHandle(self.ctx, self, listener, pageSize)
	if err != nil {
		handle.refuse(nil, err)
		return handle
	}
	registered := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return false
		}
		self.handles[handle] = true
		return true
	}()
	if !registered {
		handle.refuse(nil, errSidecarSearchUnavailable)
		return handle
	}
	go handle.run(request)
	return handle
}

// Takes handle off the open searches.
func (self *SidecarSearch) forget(handle *SidecarSearchHandle) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.handles, handle)
}

// A repo's status, or with a nil repoId every repo's together (§18.5's GetStatus): nil when there is none, as while no
// search engine runs, the Sidecar closed, or no daemon answers.
func (self *SidecarSearch) GetStatus(repoId *Id) *SidecarSearchStatus {
	status, err := traceWithReturn(func() *SidecarSearchStatus {
		ctx, err := self.callCtx()
		if err != nil {
			return nil
		}
		ctx, cancel := context.WithTimeout(ctx, self.settings.StatusTimeout)
		defer cancel()
		status, err := self.backend.status(ctx, sidecarSearchRepoIdOf(repoId))
		if err != nil {
			return nil
		}
		return status
	})
	if err != nil {
		return nil
	}
	return status
}

// Drops the search cache of repoId, or with a nil repoId of every repo (§18.5): its files and derived data. This
// device's overrides and notice answers stay (§5.9's state row). Answers callback once.
func (self *SidecarSearch) DropCache(repoId *Id, callback SidecarResultCallback) {
	sidecarSearchAnswer(func() *SidecarResult {
		ctx, err := self.callCtx()
		if err != nil {
			return sidecarSearchResultOf(err)
		}
		return sidecarSearchResultOf(self.backend.dropCache(ctx, sidecarSearchRepoIdOf(repoId)))
	}, callback)
}

// repoId as the core's, nil for nil.
func sidecarSearchRepoIdOf(repoId *Id) *sidecar.Id {
	if repoId == nil {
		return nil
	}
	id := sidecar.Id(repoId.id)
	return &id
}

// Runs call on a goroutine of its own and answers callback once with what it answers, or with call's panic as the
// error.
func sidecarSearchAnswer(call func() *SidecarResult, callback SidecarResultCallback) {
	go func() {
		result, err := traceWithReturnError(func() (*SidecarResult, error) {
			return call(), nil
		})
		if callback != nil {
			callback.Result(result, err)
		}
	}()
}

// One open search (§18.5): its pages go to its listener until the last page or Cancel.
type SidecarSearchHandle struct {
	ctx      context.Context
	cancel   context.CancelFunc
	search   *SidecarSearch
	listener SidecarSearchListener
	pages    *sidecarSearchDelivery
	// the page size the search started with, which a More of 0 or less keeps
	pageSize int
	// told when More asks for a page
	wake chan struct{}

	stateLock sync.Mutex
	// the page the stream fills, or gave last
	pageIndex int
	// More's next page and its size: the latest asked, in place of one the stream has not started
	morePending  bool
	morePageSize int
	// the stream gave its last page and ended
	finished bool
}

// A handle whose stream runs under ctx.
func newSidecarSearchHandle(ctx context.Context, search *SidecarSearch, listener SidecarSearchListener, pageSize int) *SidecarSearchHandle {
	cancelCtx, cancel := context.WithCancel(ctx)
	return &SidecarSearchHandle{
		ctx:      cancelCtx,
		cancel:   cancel,
		search:   search,
		listener: listener,
		pages:    newSidecarSearchDelivery(listener),
		pageSize: pageSize,
		wake:     make(chan struct{}, 1),
	}
}

// Asks for the next page, of pageSize hits, or the search's own size for 0 or less (§18.5). The current page is given
// whole first; a More past the last page answers an empty page with done set.
func (self *SidecarSearchHandle) More(pageSize int32) {
	trace(func() {
		size := int(pageSize)
		if size <= 0 {
			size = self.pageSize
		}
		if settings := self.search.settings; settings.PageHitMaxCount < size {
			size = settings.PageHitMaxCount
		}
		pastLast, pageIndex := func() (bool, int) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if self.finished {
				self.pageIndex += 1
				return true, self.pageIndex
			}
			self.morePending = true
			self.morePageSize = size
			return false, 0
		}()
		if pastLast {
			self.pages.offer(pageIndex, nil, &SidecarSearchProgress{}, true)
			return
		}
		select {
		case self.wake <- struct{}{}:
		default:
		}
	})
}

// Ends the search on this device (§18.5): its stream ends here and in the core, no Cancel reaches an indexer, and no
// listener call starts after it returns. Safe to call again and from the listener.
func (self *SidecarSearchHandle) Cancel() {
	trace(func() {
		self.end()
		self.search.forget(self)
	})
}

// Ends delivery and the stream.
func (self *SidecarSearchHandle) end() {
	self.pages.close()
	self.cancel()
}

// The next page More asked for, and its size, taking the request.
func (self *SidecarSearchHandle) takeMore() (int, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !self.morePending {
		return 0, false
	}
	self.morePending = false
	self.pageIndex += 1
	return self.morePageSize, true
}

// The page the stream fills now.
func (self *SidecarSearchHandle) currentPage() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.pageIndex
}

// The stream gave its last page: a More from here on answers itself, and one asked before this answers now.
func (self *SidecarSearchHandle) finish() {
	pastLast, pageIndex := func() (bool, int) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.finished = true
		if !self.morePending {
			return false, 0
		}
		self.morePending = false
		self.pageIndex += 1
		return true, self.pageIndex
	}()
	if pastLast {
		self.pages.offer(pageIndex, nil, &SidecarSearchProgress{}, true)
	}
}

// One refusal, the stream's last page: done set, the hits found before, and err's code with its fixed message.
func (self *SidecarSearchHandle) refuse(hits []*SidecarSearchHit, err error) {
	code := sidecarSearchCodeOf(err)
	if code == "" {
		code = SidecarSearchCodeFailed
	}
	self.pages.offer(self.currentPage(), hits, &SidecarSearchProgress{Code: code, Message: sidecarSearchCodeMessages[code]}, true)
	self.finish()
}

// The stream (the file's comment): pulls the source while the current page wants hits, gives each state of the page,
// and waits for More once the page is full or the source waits. It ends once it has given the last page, or at
// Cancel or the Sidecar's close.
func (self *SidecarSearchHandle) run(request *sidecarSearchRequest) {
	defer self.search.forget(self)
	settings := self.search.settings
	source, err := self.search.backend.search(self.ctx, request)
	if err != nil {
		if self.ctx.Err() == nil {
			self.refuse(nil, err)
		}
		return
	}
	defer source.close()

	pageSize := request.pageSize
	page := []*SidecarSearchHit{}
	held := []*SidecarSearchHit{}
	progress := &SidecarSearchProgress{}
	sourceDone := false
	// the source waits on the person or the host: only More pulls it again
	waiting := false
	emptyPullCount := 0
	// whether the page changed since it was last given
	changed := true
	give := func() {
		self.pages.offer(self.currentPage(), page, progress, sourceDone && len(held) == 0)
		changed = false
	}
	nextPage := func(size int) {
		pageSize = size
		page = []*SidecarSearchHit{}
		waiting = false
		emptyPullCount = 0
		changed = true
	}
	for {
		for len(page) < pageSize && 0 < len(held) {
			page = append(page, held[0])
			held = held[1:]
			changed = true
		}
		if sourceDone && len(held) == 0 {
			if changed {
				give()
			}
			self.finish()
			return
		}
		if pageSize <= len(page) || waiting {
			if changed {
				give()
			}
			if size, ok := self.takeMore(); ok {
				nextPage(size)
				continue
			}
			select {
			case <-self.ctx.Done():
				return
			case <-self.wake:
			}
			continue
		}
		if size, ok := self.takeMore(); ok {
			// the app asked for the next page before this one filled: this one stands as given
			if changed {
				give()
			}
			nextPage(size)
			continue
		}
		hits, pulled, done, err := source.next(self.ctx)
		if self.ctx.Err() != nil {
			return
		}
		if err != nil {
			self.refuse(page, err)
			return
		}
		if pulled == nil {
			pulled = &SidecarSearchProgress{}
		}
		if !sidecarSearchProgressEqual(pulled, progress) {
			changed = true
		}
		progress = pulled
		sourceDone = done
		for _, hit := range hits {
			if len(page) < pageSize {
				page = append(page, hit)
			} else {
				held = append(held, hit)
			}
			changed = true
		}
		if changed && !(sourceDone && len(held) == 0) {
			give()
		}
		waiting = progress.Waiting != "" && len(hits) == 0 && !done
		if len(hits) == 0 && !done && !waiting {
			emptyPullCount += 1
			if settings.EmptyPullCount <= emptyPullCount {
				emptyPullCount = 0
				pause := time.NewTimer(settings.EmptyPullInterval)
				select {
				case <-self.ctx.Done():
					pause.Stop()
					return
				case <-pause.C:
				}
			}
		} else {
			emptyPullCount = 0
		}
	}
}

// Whether a and b say the same, the indexer compared by its id.
func sidecarSearchProgressEqual(a *SidecarSearchProgress, b *SidecarSearchProgress) bool {
	aRemoteBy, bRemoteBy := a.RemoteBy, b.RemoteBy
	aCopy, bCopy := *a, *b
	aCopy.RemoteBy, bCopy.RemoteBy = nil, nil
	if aCopy != bCopy {
		return false
	}
	if aRemoteBy == nil || bRemoteBy == nil {
		return aRemoteBy == nil && bRemoteBy == nil
	}
	return sidecarControlSameId(aRemoteBy.SidecarId, bRemoteBy.SidecarId)
}

// One state of a page, waiting for the listener.
type sidecarSearchPageState struct {
	pageIndex int
	hits      []*SidecarSearchHit
	progress  *SidecarSearchProgress
	done      bool
}

// A search's pages to its listener, in order, on a Go thread: one goroutine at a time gives what waits and ends once
// nothing does, so a search that ended leaves none behind. A state replaces one of the same page the listener has not
// taken, since it holds everything that one did; a later page's never replaces an earlier one's, nor a last one.
type sidecarSearchDelivery struct {
	listener SidecarSearchListener
	closed   atomic.Bool

	stateLock sync.Mutex
	pending   []*sidecarSearchPageState
	running   bool
}

// A delivery to listener, which may be nil.
func newSidecarSearchDelivery(listener SidecarSearchListener) *sidecarSearchDelivery {
	return &sidecarSearchDelivery{listener: listener}
}

// Leaves a copy of a page's state for the listener.
func (self *sidecarSearchDelivery) offer(pageIndex int, hits []*SidecarSearchHit, progress *SidecarSearchProgress, done bool) {
	if self.listener == nil || self.closed.Load() {
		return
	}
	state := &sidecarSearchPageState{pageIndex: pageIndex, hits: append([]*SidecarSearchHit{}, hits...), done: done}
	copied := *progress
	state.progress = &copied
	start := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if n := len(self.pending); 0 < n && self.pending[n-1].pageIndex == pageIndex && !self.pending[n-1].done {
			self.pending[n-1] = state
		} else {
			self.pending = append(self.pending, state)
		}
		if self.running {
			return false
		}
		self.running = true
		return true
	}()
	if start {
		go self.run()
	}
}

// Gives each state in order until none waits, or until closed.
func (self *sidecarSearchDelivery) run() {
	for {
		state := func() *sidecarSearchPageState {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if len(self.pending) == 0 || self.closed.Load() {
				self.pending = nil
				self.running = false
				return nil
			}
			state := self.pending[0]
			self.pending = self.pending[1:]
			return state
		}()
		if state == nil {
			return
		}
		list := NewSidecarSearchHitList()
		list.addAll(state.hits...)
		trace(func() {
			self.listener.SearchPage(list, state.progress, state.done)
		})
	}
}

// Ends delivery without waiting, so a listener may call it: no listener call starts after it returns.
func (self *sidecarSearchDelivery) close() {
	self.closed.Store(true)
}

// sidecarSearchBackend over the local core (CORE-M10-03's calls).
type sidecarSearchCoreBackend struct {
	local *sidecar.Sidecar
}

// See sidecarSearchBackend.
func (self *sidecarSearchCoreBackend) search(ctx context.Context, request *sidecarSearchRequest) (sidecarSearchSource, error) {
	coreRequest := &sidecar.SearchRequest{
		Text:         request.text,
		Repos:        request.repoIds,
		Since:        request.since,
		Until:        request.until,
		Fuzzy:        request.fuzzy,
		AllowMetered: request.allowMetered,
		Remote:       request.remote,
		PageSize:     request.pageSize,
	}
	switch request.scope {
	case SidecarSearchScopeLatest:
		coreRequest.Scope = sidecar.SearchLatest
	case SidecarSearchScopeAll:
		coreRequest.Scope = sidecar.SearchAll
	default:
		coreRequest.Scope = sidecar.SearchCached
	}
	stream, err := self.local.Search(ctx, coreRequest)
	if err != nil {
		return nil, err
	}
	return &sidecarSearchCoreSource{local: self.local, stream: stream}, nil
}

// See sidecarSearchBackend.
func (self *sidecarSearchCoreBackend) status(ctx context.Context, repoId *sidecar.Id) (*SidecarSearchStatus, error) {
	status, err := self.local.GetSearchStatus(repoId)
	if err != nil {
		return nil, err
	}
	return sidecarSearchStatusOf(status), nil
}

// See sidecarSearchBackend.
func (self *sidecarSearchCoreBackend) dropCache(ctx context.Context, repoId *sidecar.Id) error {
	return self.local.DropSearchCache(repoId)
}

// A search's pages from the core's SearchStream.
type sidecarSearchCoreSource struct {
	local  *sidecar.Sidecar
	stream *sidecar.SearchStream
}

// See sidecarSearchSource. The core's stream bounds a page by Search.PageCpu, so a pull computes no longer than that.
func (self *sidecarSearchCoreSource) next(ctx context.Context) ([]*SidecarSearchHit, *SidecarSearchProgress, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, false, err
	}
	coreHits, coreProgress, done, err := self.stream.Next()
	if err != nil {
		return nil, nil, false, err
	}
	hits := make([]*SidecarSearchHit, 0, len(coreHits))
	for i := range coreHits {
		hits = append(hits, sidecarSearchHitOf(&coreHits[i]))
	}
	return hits, sidecarSearchProgressOf(&coreProgress, self.indexerOf), done, nil
}

// See sidecarSearchSource.
func (self *sidecarSearchCoreSource) close() {
	self.stream.Close()
}

// The indexer of sidecarId as GetSearchIndexers lists it, or one naming the id alone.
func (self *sidecarSearchCoreSource) indexerOf(sidecarId sidecar.Id) *SidecarIndexer {
	for _, indexer := range self.local.GetSearchIndexers() {
		if indexer.SidecarId == sidecarId {
			return sidecarIndexerOf(&indexer)
		}
	}
	return &SidecarIndexer{SidecarId: newId([16]byte(sidecarId))}
}

// The bound hit of the core's.
func sidecarSearchHitOf(hit *sidecar.SearchHit) *SidecarSearchHit {
	bound := &SidecarSearchHit{
		RepoId:     newId([16]byte(hit.Repo)),
		Path:       hit.Path,
		Version:    int64(hit.Version),
		Snippet:    hit.Snippet,
		Correction: hit.Correction,
		By:         hit.By,
		Partition:  SidecarSearchPartitionLatest,
	}
	if !hit.Time.IsZero() {
		bound.TimeMillis = hit.Time.UnixMilli()
	}
	if hit.Partition == sidecar.SearchPartitionAll {
		bound.Partition = SidecarSearchPartitionAll
	}
	switch hit.State {
	case sidecar.SearchVerified:
		bound.State = SidecarSearchHitVerified
	case sidecar.SearchConfirmed:
		bound.State = SidecarSearchHitConfirmed
	case sidecar.SearchNear:
		bound.State = SidecarSearchHitNear
	case sidecar.SearchUnmatched:
		bound.State = SidecarSearchHitUnmatched
	default:
		bound.State = SidecarSearchHitPossible
	}
	fields := map[int]string{}
	for _, field := range hit.Fields {
		if _, seen := fields[field.Field]; !seen {
			fields[field.Field] = field.Text
		}
	}
	bound.Title = fields[sidecarSearchFieldSubject]
	if bound.Title == "" {
		bound.Title = fields[sidecarSearchFieldTitle]
	}
	bound.From = fields[sidecarSearchFieldFrom]
	if bound.From == "" {
		bound.From = fields[sidecarSearchFieldAgentLabel]
	}
	return bound
}

// The bound progress of the core's, its RemoteBy named by indexerOf, and its remote state and fallback reason mapped
// one for one (sidecar_search_remote_host.go).
func sidecarSearchProgressOf(progress *sidecar.SearchProgress, indexerOf func(sidecarId sidecar.Id) *SidecarIndexer) *SidecarSearchProgress {
	bound := &SidecarSearchProgress{
		Searched:         int32(progress.Searched),
		Total:            int32(progress.Total),
		FetchByteCount:   progress.FetchBytes,
		FetchedByteCount: progress.FetchedBytes,
		Uncovered:        int32(progress.Uncovered),
		Waiting:          progress.Waiting,
		Remote:           sidecarSearchRemoteStateOf(progress.Remote),
		FallbackReason:   sidecarSearchFallbackReasonOf(progress.FallbackReason),
	}
	if !progress.CoveredFrom.IsZero() {
		bound.CoveredFromMillis = progress.CoveredFrom.UnixMilli()
	}
	if progress.RemoteBy != nil && indexerOf != nil {
		bound.RemoteBy = indexerOf(*progress.RemoteBy)
	}
	return bound
}

// The bound status of the core's.
func sidecarSearchStatusOf(status *sidecar.SearchStatus) *SidecarSearchStatus {
	bound := &SidecarSearchStatus{
		IndexOn:             status.IndexOn,
		FormatSupported:     status.FormatSupported,
		Policy:              status.Policy,
		Remote:              status.Remote,
		AllCachedByteCount:  status.AllCachedByteCount,
		AllMissingByteCount: status.AllMissingByteCount,
		Uncovered:           int32(status.Uncovered),
	}
	if !status.LatestFrom.IsZero() {
		bound.LatestFromMillis = status.LatestFrom.UnixMilli()
	}
	if !status.AllCachedThrough.IsZero() {
		bound.AllCachedThroughMillis = status.AllCachedThrough.UnixMilli()
	}
	return bound
}

// The bound indexer of the core's, its state mapped one for one (sidecar_search_remote_host.go).
func sidecarIndexerOf(indexer *sidecar.SearchIndexer) *SidecarIndexer {
	bound := &SidecarIndexer{
		SidecarId:     newId([16]byte(indexer.SidecarId)),
		Name:          indexer.Name,
		Member:        indexer.Member,
		Own:           indexer.Own,
		State:         sidecarIndexerStateOf(indexer.State),
		Acknowledged:  indexer.Acknowledged,
		ServedRepoIds: NewIdList(),
	}
	if indexer.MemberId != nil {
		bound.MemberId = newId([16]byte(*indexer.MemberId))
	}
	for _, repoId := range indexer.ServedRepoIds {
		bound.ServedRepoIds.Add(newId([16]byte(repoId)))
	}
	return bound
}
