//go:build sidecar_http || !js

// Search in the page Worker (§18.8's messages and storage rows, §18.4's browsers and caps rows; SDK-M10-04; R-SDK-13,
// D-SDK-21, D-SDK-24, I-19): five operations over the Sidecar's §5.4 search calls, whose engine the core runs.
//
//	search          starts a search and answers its first page's state: the hits found so far as display fields and
//	                states, never a file's content (D-SDK-24), and the search's id. A page settles once it holds its
//	                page size, the search is done, it waits (metered, offline, update), or SearchReplyTimeout passed
//	search_more     a state of the search the reply names: while its current page still fills, the page's next state,
//	                which replaces the last (same page number); once the page settled, the next page's first. After the
//	                last page, an empty one with done set
//	search_cancel   ends the search here: its stream ends in the core and nothing goes anywhere else, since a browser
//	                Worker never asks an indexer (§18.4's remote row). A search this Worker does not hold answers ok
//	search_status   a repo's search status, or every repo's together
//	search_policy   this browser's policy for all, for a repo or every repo, as SetPolicy (§18.5); "" restores the
//	                default. Kept in §5.9's search state, which eviction of the whole origin loses with everything else
//
// Every search runs in the Worker that holds the network's Web Lock, since worker.js relays every other Worker's
// requests to it, search_more and search_cancel among them, so a cancel reaches the Worker running its search. A
// search whose holder went (its tab closed) is gone: the next holder answers search_expired, and the page searches
// again. Only the holder opens the network, so only it writes the search cache, its own IndexedDB object store
// (sidecarwasm's store_search_indexeddb.go).
//
// js/wasm runs one thread (§18.10's threads row): the core bounds each pull by Search.PageCpu, which
// PageSearchSettings sets to the Worker's yield interval, and the Worker yields between pulls, so a search's working
// set stays within §18.4's 8 MiB (Search.MemoryByteCount) and it yields every 50 ms.
//
// The frame reports what its browser exposes (loader.js): unmetered where navigator.connection.type is wifi or
// ethernet, charging from getBattery(), and the tab's visibility as the foreground. Safari and Firefox expose neither
// condition and desktop browsers no network type, so those read metered and not charging, and a "search more" past
// §18.4's 25 MiB waits for allow_metered there. A query is never logged, stored or put in a reply's message (I-19):
// a refusal carries its code's fixed text.

package sidecarworker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"regexp"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/urnetwork/sidecar"
)

// The search operations, as requests name them.
const (
	OperationSearch       = "search"
	OperationSearchMore   = "search_more"
	OperationSearchCancel = "search_cancel"
	OperationSearchStatus = "search_status"
	OperationSearchPolicy = "search_policy"
)

// The scopes a search request names (§18.5's SidecarSearchQuery.Scope).
const (
	searchScopeCached = "cached"
	searchScopeLatest = "latest"
	searchScopeAll    = "all"
)

// A search id's format: 32 lowercase hex digits, as this Worker makes them.
var searchIdPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// The bytes a search id is made of.
const searchIdByteCount = 16

// The search a Worker runs on: on the page Worker, the leg's sidecar.Sidecar, which has each of these (CORE-M10-03).
type WorkerSearch interface {
	Search(ctx context.Context, request *sidecar.SearchRequest) (*sidecar.SearchStream, error)
	GetSearchStatus(repoId *sidecar.Id) (*sidecar.SearchStatus, error)
	SetSearchPolicy(repoId *sidecar.Id, mode string) error
	SetSearchConditions(unmetered bool, charging bool, lowPower bool)
}

// The core's foreground setter, which SEARCH-M10-33 adds to sidecar.Sidecar (plan-SEARCH-foreground-signal): a page
// Worker's tab visibility.
type workerSearchForeground interface {
	SetSearchForeground(foreground bool)
}

// What the key-origin frame reports of its browser (loader.js's browserConditions).
type WorkerConditions struct {
	Unmetered bool `json:"unmetered"`
	Charging  bool `json:"charging"`
	LowPower  bool `json:"low_power"`
	// Some tab of this network's pages is visible.
	Foreground bool `json:"foreground"`
}

// How a page Worker's, or a quick burner's, core search runs (§18.4's Search block: the browsers, caps and targets
// rows), as app hosts' classes set theirs (sdk's sidecar_host_class.go). DefaultPageSearchSettings and
// DefaultQuickBurnerSearchSettings are the only defaults.
type PageSearchSettings struct {
	// §18.4's mode for all: wifi_charging while a tab is open, plus on_demand at "search more"; the quick burner's never.
	AllPolicy string
	// Search.MemoryByteCount: one search's 8 MiB working set (§18.4's targets) beside the process's 256 KiB of
	// version-table pages (§5.9).
	MemoryByteCount int64
	// Search.DaemonSearchCount: 1, so a search fits MemoryByteCount.
	SearchCount int
	// Search.CacheByteCount: the page Worker keeps 128 MiB a network in its own IndexedDB store (D-SDK-24); the quick
	// burner 0, which keeps its buckets in memory.
	CacheByteCount int64
	// Search.AllCacheByteCount: the most of all the page Worker keeps, 128 MiB a network (§18.4's caps row); the quick
	// burner none.
	AllCacheByteCount int64
	// Search.UnusedAllAge: all shards no search used for 30 days are evicted in browsers.
	UnusedAllAge time.Duration
	// Search.SyncRoundCount: 1 in page Workers (DESIGN-DETAILS 7.2.1).
	SyncRoundCount int
	// Search.PageCpu: a pull's bound, the Worker's 50 ms yield interval (§18.10's threads row), so a page computes in
	// steps the Worker yields between.
	PageCpu time.Duration
}

// The page Worker's.
func DefaultPageSearchSettings() *PageSearchSettings {
	return &PageSearchSettings{
		AllPolicy:         sidecar.SearchPolicyWifiCharging,
		MemoryByteCount:   8*1024*1024 + 256*1024,
		SearchCount:       1,
		CacheByteCount:    128 * 1024 * 1024,
		AllCacheByteCount: 128 * 1024 * 1024,
		UnusedAllAge:      30 * 24 * time.Hour,
		SyncRoundCount:    1,
		PageCpu:           50 * time.Millisecond,
	}
}

// The quick burner's: buckets in memory, never all (§18.8's Quick burner).
func DefaultQuickBurnerSearchSettings() *PageSearchSettings {
	settings := DefaultPageSearchSettings()
	settings.AllPolicy = sidecar.SearchPolicyNever
	settings.CacheByteCount = 0
	settings.AllCacheByteCount = 0
	settings.UnusedAllAge = 0
	return settings
}

// Sets these over sidecarSettings' search: a browser Worker never asks an indexer and never serves (§18.4's remote
// row), so its requester side is off and an unreported condition reads metered.
func (self *PageSearchSettings) Apply(sidecarSettings *sidecar.SidecarSettings) {
	search := sidecarSettings.Search
	search.AllPolicy = self.AllPolicy
	search.MemoryByteCount = self.MemoryByteCount
	search.DaemonSearchCount = self.SearchCount
	search.CacheByteCount = self.CacheByteCount
	search.AllCacheByteCount = self.AllCacheByteCount
	search.UnusedAllAge = self.UnusedAllAge
	search.SyncRoundCount = self.SyncRoundCount
	search.PageCpu = self.PageCpu
	search.RemoteAsk = false
	search.RemoteServe = false
}

// A search request's fields, checked (ops.go's requestShape.fields).
type searchFields struct {
	scope        sidecar.SearchScope
	scopeAll     bool
	since        time.Time
	until        time.Time
	fuzzy        bool
	allowMetered bool
	// 0: the Worker's default
	pageSize int
	searchId string
	policy   string
}

// request's search fields, each held to settings' bounds.
func searchFieldsOf(request *Request, settings *WorkerSettings) (*searchFields, error) {
	fields := &searchFields{fuzzy: request.Fuzzy, allowMetered: request.AllowMetered}
	switch request.Scope {
	case "", searchScopeCached:
		fields.scope = sidecar.SearchCached
	case searchScopeLatest:
		fields.scope = sidecar.SearchLatest
	case searchScopeAll:
		fields.scope = sidecar.SearchAll
		fields.scopeAll = true
	default:
		return nil, errors.New("a scope is cached, latest or all")
	}
	parseMillis := func(text string) (time.Time, error) {
		if text == "" {
			return time.Time{}, nil
		}
		millis, err := strconv.ParseInt(text, 10, 64)
		if err != nil || millis <= 0 {
			return time.Time{}, errors.New("a time is positive decimal milliseconds")
		}
		return time.UnixMilli(millis).UTC(), nil
	}
	var err error
	if fields.since, err = parseMillis(request.SinceMs); err != nil {
		return nil, err
	}
	if fields.until, err = parseMillis(request.UntilMs); err != nil {
		return nil, err
	}
	if !fields.since.IsZero() && !fields.until.IsZero() && fields.until.Before(fields.since) {
		return nil, errors.New("until is before since")
	}
	if request.PageSize != "" {
		pageSize, err := strconv.Atoi(request.PageSize)
		if err != nil || pageSize <= 0 || settings.SearchPageHitMaxCount < pageSize {
			return nil, errors.New("a page size is 1 to SearchPageHitMaxCount")
		}
		fields.pageSize = pageSize
	}
	if request.SearchId != "" && !searchIdPattern.MatchString(request.SearchId) {
		return nil, errors.New("a search id is 32 lowercase hex digits")
	}
	fields.searchId = request.SearchId
	switch request.Policy {
	case "", sidecar.SearchPolicyBackground, sidecar.SearchPolicyWifiCharging, sidecar.SearchPolicyOnDemand, sidecar.SearchPolicyNever:
		fields.policy = request.Policy
	default:
		return nil, errors.New("a policy is background, wifi_charging, on_demand or never")
	}
	return fields, nil
}

// search_more and search_cancel name their search.
func checkSearchIdField(fields *requestFields) error {
	if fields.search == nil || fields.search.searchId == "" {
		return errors.New("this operation names its search")
	}
	return nil
}

// One page's state of a search, as search and search_more answer it.
type SearchView struct {
	SearchId string `json:"search_id"`
	// The page this state is of, decimal from "0": a later state of the same page replaces it.
	Page string           `json:"page"`
	Hits []*SearchHitView `json:"hits"`
	// Where the search stands.
	Progress *SearchProgressView `json:"progress"`
	// The page is full, the search waits, or it is done: search_more asks for the next page.
	PageComplete bool `json:"page_complete"`
	// No later page will come.
	Done bool `json:"done"`
}

// One hit, as display fields and states only (D-SDK-24): never the file's content.
type SearchHitView struct {
	RepoId string   `json:"repo_id"`
	Path   NameView `json:"path"`
	// The path's version the hit rests on, decimal.
	Version    string `json:"version"`
	TimeMillis int64  `json:"time_millis,omitempty"`
	// The subject or title, and the sender or the writer's label, each at most SearchFieldMaxByteCount.
	Title string `json:"title,omitempty"`
	From  string `json:"from,omitempty"`
	// Around the match on a verified hit, i/'s snippet on mail, at most SearchSnippetMaxByteCount.
	Snippet string `json:"snippet,omitempty"`
	// verified, confirmed, possible, near or unmatched.
	State string `json:"state"`
	// latest or all.
	Partition  string `json:"partition"`
	Correction string `json:"correction,omitempty"`
	// Empty in a browser, which never asks an indexer.
	By string `json:"by,omitempty"`
}

// Where a search stands (§18.5's SidecarSearchProgress, its remote fields aside: a browser never asks).
type SearchProgressView struct {
	Searched int `json:"searched"`
	Total    int `json:"total"`
	// Scope all: bytes still to fetch, and fetched so far.
	FetchByteCount   int64 `json:"fetch_byte_count,omitempty"`
	FetchedByteCount int64 `json:"fetched_byte_count,omitempty"`
	Uncovered        int   `json:"uncovered,omitempty"`
	// "", metered, offline or update: metered waits for a search with allow_metered.
	Waiting           string `json:"waiting,omitempty"`
	CoveredFromMillis int64  `json:"covered_from_millis,omitempty"`
	TooBroad          bool   `json:"too_broad,omitempty"`
}

// A repo's search status (§18.5's SidecarSearchStatus).
type SearchStatusView struct {
	IndexOn                bool   `json:"index_on"`
	FormatSupported        bool   `json:"format_supported"`
	Policy                 string `json:"policy"`
	Remote                 string `json:"remote"`
	LatestFromMillis       int64  `json:"latest_from_millis,omitempty"`
	AllCachedThroughMillis int64  `json:"all_cached_through_millis,omitempty"`
	AllCachedByteCount     int64  `json:"all_cached_byte_count,omitempty"`
	AllMissingByteCount    int64  `json:"all_missing_byte_count,omitempty"`
	Uncovered              int    `json:"uncovered,omitempty"`
}

// §11.14's field numbers a hit's display fields carry: identifiers of the format.
const (
	searchViewFieldSubject    = 1
	searchViewFieldFrom       = 2
	searchViewFieldTitle      = 4
	searchViewFieldAgentLabel = 9
)

// text cut to at most maxByteCount bytes on a rune boundary, invalid bytes made U+FFFD.
func searchViewText(text string, maxByteCount int) string {
	if !utf8.ValidString(text) {
		text = string([]rune(text))
	}
	if len(text) <= maxByteCount {
		return text
	}
	cut := maxByteCount
	for 0 < cut && !utf8.RuneStart(text[cut]) {
		cut -= 1
	}
	return text[:cut]
}

// hit's view: its display fields and state, field by field (D-SDK-24).
func searchHitViewOf(hit *sidecar.SearchHit, settings *WorkerSettings) *SearchHitView {
	view := &SearchHitView{
		RepoId:     hit.Repo.String(),
		Path:       nameViewOf([]byte(hit.Path)),
		Version:    strconv.FormatUint(hit.Version, 10),
		Snippet:    searchViewText(hit.Snippet, settings.SearchSnippetMaxByteCount),
		Correction: searchViewText(hit.Correction, settings.SearchFieldMaxByteCount),
		By:         searchViewText(hit.By, settings.SearchFieldMaxByteCount),
		Partition:  searchScopeLatest,
	}
	if !hit.Time.IsZero() {
		view.TimeMillis = hit.Time.UnixMilli()
	}
	if hit.Partition == sidecar.SearchPartitionAll {
		view.Partition = searchScopeAll
	}
	switch hit.State {
	case sidecar.SearchVerified:
		view.State = "verified"
	case sidecar.SearchConfirmed:
		view.State = "confirmed"
	case sidecar.SearchNear:
		view.State = "near"
	case sidecar.SearchUnmatched:
		view.State = "unmatched"
	default:
		view.State = "possible"
	}
	fields := map[int]string{}
	for _, field := range hit.Fields {
		if _, seen := fields[field.Field]; !seen {
			fields[field.Field] = field.Text
		}
	}
	title := fields[searchViewFieldSubject]
	if title == "" {
		title = fields[searchViewFieldTitle]
	}
	from := fields[searchViewFieldFrom]
	if from == "" {
		from = fields[searchViewFieldAgentLabel]
	}
	view.Title = searchViewText(title, settings.SearchFieldMaxByteCount)
	view.From = searchViewText(from, settings.SearchFieldMaxByteCount)
	return view
}

// progress's view.
func searchProgressViewOf(progress *sidecar.SearchProgress) *SearchProgressView {
	view := &SearchProgressView{
		Searched:         progress.Searched,
		Total:            progress.Total,
		FetchByteCount:   progress.FetchBytes,
		FetchedByteCount: progress.FetchedBytes,
		Uncovered:        progress.Uncovered,
		Waiting:          progress.Waiting,
		TooBroad:         progress.TooBroad,
	}
	if !progress.CoveredFrom.IsZero() {
		view.CoveredFromMillis = progress.CoveredFrom.UnixMilli()
	}
	return view
}

// status's view.
func searchStatusViewOf(status *sidecar.SearchStatus) *SearchStatusView {
	view := &SearchStatusView{
		IndexOn:             status.IndexOn,
		FormatSupported:     status.FormatSupported,
		Policy:              status.Policy,
		Remote:              status.Remote,
		AllCachedByteCount:  status.AllCachedByteCount,
		AllMissingByteCount: status.AllMissingByteCount,
		Uncovered:           status.Uncovered,
	}
	if !status.LatestFrom.IsZero() {
		view.LatestFromMillis = status.LatestFrom.UnixMilli()
	}
	if !status.AllCachedThrough.IsZero() {
		view.AllCachedThroughMillis = status.AllCachedThrough.UnixMilli()
	}
	return view
}

// The refusal a search error stands for, by its class alone.
func searchCodeOf(err error) string {
	switch {
	case errors.Is(err, sidecar.ErrSidecarSearchUnavailable):
		return CodeUnavailable
	case errors.Is(err, sidecar.ErrSearchBusy):
		return CodeUnavailable
	case errors.Is(err, sidecar.ErrSearchCursorExpired):
		return CodeSearchExpired
	case errors.Is(err, sidecar.ErrSearchRequest), errors.Is(err, sidecar.ErrSearchQueryTooLong),
		errors.Is(err, sidecar.ErrSearchStateArgs), errors.Is(err, sidecar.ErrSearchCursorInvalid):
		return CodeInvalid
	case errors.Is(err, sidecar.ErrSidecarSearchRefused):
		return CodeRefused
	}
	return codeOf(err)
}

// The searches one Worker holds open.
type workerSearches struct {
	stateLock sync.Mutex
	sessions  map[string]*workerSearchSession
}

func newWorkerSearches() *workerSearches {
	return &workerSearches{sessions: map[string]*workerSearchSession{}}
}

// One open search and its current page.
type workerSearchSession struct {
	id     string
	stream *sidecar.SearchStream

	// one operation of the search at a time
	pageLock   sync.Mutex
	pageIndex  int
	pageSize   int
	page       []*SearchHitView
	held       []*SearchHitView
	progress   *SearchProgressView
	sourceDone bool
	waiting    bool
	// the last page was given with done set
	finished bool
	// guarded by workerSearches' lock
	lastUsed time.Time
	ended    bool
}

// Ends sessions idle past SearchIdleTimeout, and the least recently used past SearchSessionMaxCount once one more is
// added, then adds session. Each ended session's stream ends outside the lock.
func (self *workerSearches) add(session *workerSearchSession, now time.Time, settings *WorkerSettings) {
	ended := func() []*workerSearchSession {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		ended := []*workerSearchSession{}
		for id, open := range self.sessions {
			if settings.SearchIdleTimeout <= now.Sub(open.lastUsed) {
				open.ended = true
				ended = append(ended, open)
				delete(self.sessions, id)
			}
		}
		for settings.SearchSessionMaxCount <= len(self.sessions) {
			var oldest *workerSearchSession
			for _, open := range self.sessions {
				if oldest == nil || open.lastUsed.Before(oldest.lastUsed) {
					oldest = open
				}
			}
			oldest.ended = true
			ended = append(ended, oldest)
			delete(self.sessions, oldest.id)
		}
		session.lastUsed = now
		self.sessions[session.id] = session
		return ended
	}()
	for _, session := range ended {
		session.stream.Close()
	}
}

// The open session id names, marked used, or nil.
func (self *workerSearches) get(id string, now time.Time) *workerSearchSession {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	session := self.sessions[id]
	if session != nil {
		session.lastUsed = now
	}
	return session
}

// Ends the session id names, if this Worker holds it.
func (self *workerSearches) end(id string) {
	session := func() *workerSearchSession {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		session := self.sessions[id]
		if session != nil {
			session.ended = true
			delete(self.sessions, id)
		}
		return session
	}()
	if session != nil {
		session.stream.Close()
	}
}

// How many searches are open.
func (self *workerSearches) count() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.sessions)
}

// The Worker's search, or why there is none.
func (self *Worker) search() (WorkerSearch, error) {
	if self.args.Search == nil {
		return nil, sidecar.ErrSidecarSearchUnavailable
	}
	search, err := self.args.Search()
	if err != nil {
		return nil, err
	}
	if search == nil {
		return nil, sidecar.ErrSidecarSearchUnavailable
	}
	return search, nil
}

// Reports the frame's conditions to the Sidecar's search (§18.7's conditions and foreground rows): unmetered, charging
// and low power as SetSearchConditions, the tab's visibility as the core's foreground once it has the setter. A Worker
// without a search tells nothing.
func (self *Worker) SetConditions(conditions *WorkerConditions) {
	if conditions == nil {
		return
	}
	search, err := self.search()
	if err != nil {
		return
	}
	search.SetSearchConditions(conditions.Unmetered, conditions.Charging, conditions.LowPower)
	if foreground, ok := search.(workerSearchForeground); ok {
		foreground.SetSearchForeground(conditions.Foreground)
	}
}

// A new search id: 128 random bits as hex.
func newSearchId() (string, error) {
	id := make([]byte, searchIdByteCount)
	if _, err := rand.Read(id); err != nil {
		return "", err
	}
	return hex.EncodeToString(id), nil
}

// search: starts the search and answers its first page's state.
func runSearch(worker *Worker, ctx context.Context, request *Request, fields *requestFields, step *workerStep) *Reply {
	search, err := worker.search()
	if err != nil {
		return refusalReply("", searchCodeOf(err))
	}
	searchFields := fields.search
	if worker.args.Ephemeral && searchFields.scopeAll {
		// the quick burner searches its buckets in memory and never fetches all (§18.8)
		return refusalReply("", CodeUnsupported)
	}
	pageSize := searchFields.pageSize
	if pageSize == 0 {
		pageSize = worker.settings.SearchPageHitCount
	}
	coreRequest := &sidecar.SearchRequest{
		Text:         fields.query,
		Scope:        searchFields.scope,
		Since:        searchFields.since,
		Until:        searchFields.until,
		Fuzzy:        searchFields.fuzzy,
		AllowMetered: searchFields.allowMetered,
		// a browser Worker never asks an indexer (§18.4's remote row)
		Remote:   sidecar.SearchRemoteOff,
		PageSize: pageSize,
	}
	if fields.repoIdFilter != nil {
		coreRequest.Repos = []sidecar.Id{*fields.repoIdFilter}
	}
	id, err := newSearchId()
	if err != nil {
		return refusalReply("", CodeFailed)
	}
	stream, err := search.Search(ctx, coreRequest)
	if err != nil {
		return refusalReply("", searchCodeOf(err))
	}
	session := &workerSearchSession{id: id, stream: stream, pageSize: pageSize, page: []*SearchHitView{}, progress: &SearchProgressView{}}
	worker.searches.add(session, worker.now(), worker.settings)
	session.pageLock.Lock()
	defer session.pageLock.Unlock()
	return session.fill(worker, ctx, step)
}

// search_more: the search's next state, or its next page once the current one settled.
func runSearchMore(worker *Worker, ctx context.Context, request *Request, fields *requestFields, step *workerStep) *Reply {
	session := worker.searches.get(fields.search.searchId, worker.now())
	if session == nil {
		return refusalReply("", CodeSearchExpired)
	}
	session.pageLock.Lock()
	defer session.pageLock.Unlock()
	if session.finished {
		// past the last page: an empty one, done
		session.pageIndex += 1
		return &Reply{Ok: true, Search: &SearchView{
			SearchId: session.id, Page: strconv.Itoa(session.pageIndex), Hits: []*SearchHitView{}, Progress: session.progress,
			PageComplete: true, Done: true,
		}}
	}
	if session.complete() {
		session.pageIndex += 1
		session.page = []*SearchHitView{}
		session.waiting = false
		if fields.search.pageSize != 0 {
			session.pageSize = fields.search.pageSize
		}
	}
	return session.fill(worker, ctx, step)
}

// Whether the current page settled: full, waiting, or the search done with nothing held.
func (self *workerSearchSession) complete() bool {
	return self.pageSize <= len(self.page) || self.waiting || (self.sourceDone && len(self.held) == 0)
}

// Pulls the stream into the current page until it settles, SearchEmptyPullCount pulls bring nothing, or
// SearchReplyTimeout passed, yielding between pulls, and answers the page's state.
func (self *workerSearchSession) fill(worker *Worker, ctx context.Context, step *workerStep) *Reply {
	deadline := worker.now().Add(worker.settings.SearchReplyTimeout)
	emptyPullCount := 0
	for {
		for len(self.page) < self.pageSize && 0 < len(self.held) {
			self.page = append(self.page, self.held[0])
			self.held = self.held[1:]
		}
		if self.complete() || worker.settings.SearchEmptyPullCount <= emptyPullCount || !worker.now().Before(deadline) {
			return self.reply()
		}
		hits, progress, done, err := self.stream.Next()
		if err != nil {
			worker.searches.end(self.id)
			return refusalReply("", searchCodeOf(err))
		}
		self.progress = searchProgressViewOf(&progress)
		self.sourceDone = done
		for i := range hits {
			view := searchHitViewOf(&hits[i], worker.settings)
			if len(self.page) < self.pageSize {
				self.page = append(self.page, view)
			} else {
				self.held = append(self.held, view)
			}
		}
		self.waiting = progress.Waiting != "" && len(hits) == 0 && !done
		if len(hits) == 0 && !done {
			emptyPullCount += 1
		} else {
			emptyPullCount = 0
		}
		if err := step.step(ctx); err != nil {
			return refusalReply("", searchCodeOf(err))
		}
	}
}

// The current page's state. A page with done set is the search's last.
func (self *workerSearchSession) reply() *Reply {
	done := self.sourceDone && len(self.held) == 0
	if done {
		self.finished = true
	}
	return &Reply{Ok: true, Search: &SearchView{
		SearchId:     self.id,
		Page:         strconv.Itoa(self.pageIndex),
		Hits:         append([]*SearchHitView{}, self.page...),
		Progress:     self.progress,
		PageComplete: self.complete(),
		Done:         done,
	}}
}

// search_cancel: ends the search here, and nowhere else.
func runSearchCancel(worker *Worker, ctx context.Context, request *Request, fields *requestFields, step *workerStep) *Reply {
	worker.searches.end(fields.search.searchId)
	return &Reply{Ok: true}
}

// search_status: a repo's status, or every repo's.
func runSearchStatus(worker *Worker, ctx context.Context, request *Request, fields *requestFields, step *workerStep) *Reply {
	search, err := worker.search()
	if err != nil {
		return refusalReply("", searchCodeOf(err))
	}
	status, err := search.GetSearchStatus(fields.repoIdFilter)
	if err != nil {
		return refusalReply("", searchCodeOf(err))
	}
	return &Reply{Ok: true, SearchStatus: searchStatusViewOf(status)}
}

// search_policy: this browser's policy for all, for a repo or every repo (SetPolicy).
func runSearchPolicy(worker *Worker, ctx context.Context, request *Request, fields *requestFields, step *workerStep) *Reply {
	search, err := worker.search()
	if err != nil {
		return refusalReply("", searchCodeOf(err))
	}
	if err := search.SetSearchPolicy(fields.repoIdFilter, fields.search.policy); err != nil {
		return refusalReply("", searchCodeOf(err))
	}
	return &Reply{Ok: true}
}
