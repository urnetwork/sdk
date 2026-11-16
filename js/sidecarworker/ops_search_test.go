//go:build !sidecar_http && !js

package sidecarworker

// Search in the page Worker (SDK-M10-04), natively on fakes of the Sidecar's search calls: the first page and its
// replacing states, the next page from hits already ranked, the last page and past it, a cancel that ends the stream
// here, a search the Worker no longer holds, status and policy, the session bound, hit views that carry display fields
// and states and never content, the quick burner's buckets-only search, the conditions the frame reports, the search
// settings a page Worker and a burner run their core with, and a search that yields as it pulls. Every repo, query and
// time is synthetic.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
)

// One page a fake stream answers.
type testSearchPull struct {
	hits     []sidecar.SearchHit
	progress sidecar.SearchProgress
	done     bool
	err      error
}

// A stream's source that answers its pulls in order, then no hit and done.
type testWorkerSearchSource struct {
	stateLock sync.Mutex
	pulls     []testSearchPull
	pullCount int
	closed    bool
}

func (self *testWorkerSearchSource) Next() ([]sidecar.SearchHit, sidecar.SearchProgress, bool, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.pullCount += 1
	if len(self.pulls) == 0 {
		return nil, sidecar.SearchProgress{}, true, nil
	}
	pull := self.pulls[0]
	self.pulls = self.pulls[1:]
	return pull.hits, pull.progress, pull.done, pull.err
}

func (self *testWorkerSearchSource) Close() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.closed = true
}

func (self *testWorkerSearchSource) isClosed() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.closed
}

// A fake of the Sidecar's search calls: each search runs the next scripted source.
type testWorkerSearch struct {
	stateLock   sync.Mutex
	requests    []*sidecar.SearchRequest
	scripts     [][]testSearchPull
	sources     []*testWorkerSearchSource
	searchErr   error
	status      *sidecar.SearchStatus
	statusRepos []*sidecar.Id
	policies    []string
	conditions  [][3]bool
}

func (self *testWorkerSearch) Search(ctx context.Context, request *sidecar.SearchRequest) (*sidecar.SearchStream, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	copied := *request
	self.requests = append(self.requests, &copied)
	if self.searchErr != nil {
		return nil, self.searchErr
	}
	source := &testWorkerSearchSource{}
	if 0 < len(self.scripts) {
		source.pulls = self.scripts[0]
		self.scripts = self.scripts[1:]
	}
	self.sources = append(self.sources, source)
	return sidecar.NewSearchStream(source), nil
}

func (self *testWorkerSearch) GetSearchStatus(repoId *sidecar.Id) (*sidecar.SearchStatus, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.statusRepos = append(self.statusRepos, repoId)
	if self.status == nil {
		return nil, sidecar.ErrSidecarSearchUnavailable
	}
	return self.status, nil
}

func (self *testWorkerSearch) SetSearchPolicy(repoId *sidecar.Id, mode string) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	key := "*"
	if repoId != nil {
		key = repoId.String()
	}
	self.policies = append(self.policies, key+"="+mode)
	return nil
}

func (self *testWorkerSearch) SetSearchConditions(unmetered bool, charging bool, lowPower bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.conditions = append(self.conditions, [3]bool{unmetered, charging, lowPower})
}

// A fake whose core has the foreground setter SEARCH-M10-33 adds.
type testWorkerSearchForeground struct {
	*testWorkerSearch
	foregrounds []bool
}

func (self *testWorkerSearchForeground) SetSearchForeground(foreground bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.foregrounds = append(self.foregrounds, foreground)
}

// A hit of the synthetic repo, named by n.
func testWorkerSearchHit(repoId sidecar.Id, n int) sidecar.SearchHit {
	return sidecar.SearchHit{
		Repo: repoId, Path: "notes/" + string(rune('a'+n)) + ".md", Version: uint64(n), Time: time.UnixMilli(1_800_000_000_000 + int64(n)),
		State: sidecar.SearchConfirmed, Partition: sidecar.SearchPartitionLatest,
		Fields: []sidecar.SearchField{{Field: 1, Text: "subject " + string(rune('a'+n))}},
	}
}

// A Worker world whose Search is search.
func newTestSearchWorld(t *testing.T, search WorkerSearch, adjust func(args *WorkerArgs)) *testWorkerWorld {
	t.Helper()
	return newTestWorkerWorld(t, func(args *WorkerArgs) {
		args.Search = func() (WorkerSearch, error) {
			return search, nil
		}
		if adjust != nil {
			adjust(args)
		}
	})
}

// The paths of a view's hits, in order.
func testSearchViewPaths(view *SearchView) []string {
	paths := []string{}
	for _, hit := range view.Hits {
		paths = append(paths, hit.Path.Text)
	}
	return paths
}

// search answers its first page's state; search_more the page's next state while it fills, then the next page from
// hits already ranked, the last page with done, and past it an empty page; the request maps the query's fields and
// never asks an indexer.
func TestSearchOperations(t *testing.T) {
	repoId := sidecar.NewId()
	search := &testWorkerSearch{scripts: [][]testSearchPull{{
		{hits: []sidecar.SearchHit{testWorkerSearchHit(repoId, 0)}, progress: sidecar.SearchProgress{Searched: 1, Total: 2}},
		{progress: sidecar.SearchProgress{Searched: 1, Total: 2, Waiting: "metered", FetchBytes: 30 << 20}},
		{hits: []sidecar.SearchHit{testWorkerSearchHit(repoId, 1), testWorkerSearchHit(repoId, 2), testWorkerSearchHit(repoId, 3)}, progress: sidecar.SearchProgress{Searched: 2, Total: 2}},
		{progress: sidecar.SearchProgress{Searched: 2, Total: 2}, done: true},
	}}}
	world := newTestSearchWorld(t, search, nil)
	request := &Request{Id: "s1", Operation: OperationSearch, Query: "quarterly", RepoId: repoId.String(), Scope: "all", SinceMs: "1700000000000",
		Fuzzy: true, AllowMetered: false, PageSize: "2"}
	reply := testReplyOf(t, world.worker, request)
	first := reply.Search
	if !reply.Ok || first == nil || first.Page != "0" || !slices.Equal(testSearchViewPaths(first), []string{"notes/a.md"}) || first.Done {
		t.Fatalf("the first state: %+v %+v", reply, first)
	}
	// the page waits on the metered cap: settled, with what it has
	if !first.PageComplete || first.Progress.Waiting != "metered" || first.Progress.FetchByteCount != 30<<20 {
		t.Errorf("the waiting state: %+v %+v", first, first.Progress)
	}
	coreRequest := search.requests[0]
	if coreRequest.Text != "quarterly" || coreRequest.Scope != sidecar.SearchAll || !coreRequest.Fuzzy || coreRequest.Remote != sidecar.SearchRemoteOff ||
		coreRequest.PageSize != 2 || len(coreRequest.Repos) != 1 || coreRequest.Repos[0] != repoId || coreRequest.Since.UnixMilli() != 1700000000000 {
		t.Errorf("the core's request: %+v", coreRequest)
	}
	more := testReplyOf(t, world.worker, &Request{Id: "m1", Operation: OperationSearchMore, SearchId: first.SearchId})
	if !more.Ok || more.Search.Page != "1" || !slices.Equal(testSearchViewPaths(more.Search), []string{"notes/b.md", "notes/c.md"}) || more.Search.Done || !more.Search.PageComplete {
		t.Fatalf("the next page: %+v", more.Search)
	}
	last := testReplyOf(t, world.worker, &Request{Id: "m2", Operation: OperationSearchMore, SearchId: first.SearchId})
	if !last.Ok || last.Search.Page != "2" || !slices.Equal(testSearchViewPaths(last.Search), []string{"notes/d.md"}) || !last.Search.Done {
		t.Fatalf("the last page: %+v", last.Search)
	}
	past := testReplyOf(t, world.worker, &Request{Id: "m3", Operation: OperationSearchMore, SearchId: first.SearchId})
	if !past.Ok || past.Search.Page != "3" || len(past.Search.Hits) != 0 || !past.Search.Done {
		t.Errorf("past the last page: %+v", past.Search)
	}

	// a page that fills over two pulls: the second state replaces the first under the same page number
	search.scripts = [][]testSearchPull{{
		{hits: []sidecar.SearchHit{testWorkerSearchHit(repoId, 0)}},
		{}, {}, {}, {},
		{hits: []sidecar.SearchHit{testWorkerSearchHit(repoId, 1)}},
	}}
	filling := testReplyOf(t, world.worker, &Request{Id: "s2", Operation: OperationSearch, Query: "q", PageSize: "5"})
	if filling.Search.Page != "0" || len(filling.Search.Hits) != 1 || filling.Search.PageComplete {
		t.Fatalf("a page whose pulls brought nothing answers what it has: %+v", filling.Search)
	}
	replacing := testReplyOf(t, world.worker, &Request{Id: "m4", Operation: OperationSearchMore, SearchId: filling.Search.SearchId})
	if replacing.Search.Page != "0" || !slices.Equal(testSearchViewPaths(replacing.Search), []string{"notes/a.md", "notes/b.md"}) || !replacing.Search.Done {
		t.Errorf("the replacing state: %+v", replacing.Search)
	}

	// search_cancel ends the stream here; the search is then gone, and a cancel of it again answers ok
	search.scripts = [][]testSearchPull{{{hits: []sidecar.SearchHit{testWorkerSearchHit(repoId, 0)}}, {hits: []sidecar.SearchHit{testWorkerSearchHit(repoId, 1)}}}}
	cancelled := testReplyOf(t, world.worker, &Request{Id: "s3", Operation: OperationSearch, Query: "q", PageSize: "1"})
	source := search.sources[len(search.sources)-1]
	if reply := testReplyOf(t, world.worker, &Request{Id: "c1", Operation: OperationSearchCancel, SearchId: cancelled.Search.SearchId}); !reply.Ok {
		t.Errorf("search_cancel: %+v", reply)
	}
	if !source.isClosed() {
		t.Error("search_cancel did not end the stream")
	}
	if reply := testReplyOf(t, world.worker, &Request{Id: "m5", Operation: OperationSearchMore, SearchId: cancelled.Search.SearchId}); reply.Ok || reply.Code != CodeSearchExpired {
		t.Errorf("search_more after a cancel: %+v", reply)
	}
	if reply := testReplyOf(t, world.worker, &Request{Id: "c2", Operation: OperationSearchCancel, SearchId: cancelled.Search.SearchId}); !reply.Ok {
		t.Errorf("a second cancel: %+v", reply)
	}

	// status and policy
	search.status = &sidecar.SearchStatus{IndexOn: true, FormatSupported: true, Policy: sidecar.SearchPolicyWifiCharging, Remote: sidecar.SearchRemoteOff,
		LatestFrom: time.UnixMilli(1_790_000_000_000), AllMissingByteCount: 42}
	status := testReplyOf(t, world.worker, &Request{Id: "st", Operation: OperationSearchStatus, RepoId: repoId.String()})
	if !status.Ok || status.SearchStatus == nil || status.SearchStatus.Policy != sidecar.SearchPolicyWifiCharging || status.SearchStatus.LatestFromMillis != 1_790_000_000_000 ||
		status.SearchStatus.AllMissingByteCount != 42 || search.statusRepos[0] == nil || *search.statusRepos[0] != repoId {
		t.Errorf("search_status: %+v %+v", status, status.SearchStatus)
	}
	for _, policy := range []string{sidecar.SearchPolicyNever, ""} {
		if reply := testReplyOf(t, world.worker, &Request{Id: "p", Operation: OperationSearchPolicy, Policy: policy}); !reply.Ok {
			t.Errorf("search_policy %q: %+v", policy, reply)
		}
	}
	if !slices.Equal(search.policies, []string{"*=never", "*="}) {
		t.Errorf("the policies set: %v", search.policies)
	}

	// refusals: fields an operation does not take, a malformed id, a bad scope, policy or page size
	for _, refused := range []*Request{
		{Id: "x1", Operation: OperationSearch, Scope: "everything"},
		{Id: "x2", Operation: OperationSearch, PageSize: "0"},
		{Id: "x3", Operation: OperationSearch, PageSize: "101"},
		{Id: "x4", Operation: OperationSearch, SinceMs: "5", UntilMs: "4"},
		{Id: "x5", Operation: OperationSearch, SearchId: strings.Repeat("a", 32)},
		{Id: "x6", Operation: OperationSearchMore},
		{Id: "x7", Operation: OperationSearchMore, SearchId: "NOT-HEX"},
		{Id: "x8", Operation: OperationSearchCancel, SearchId: strings.Repeat("a", 32), PageSize: "2"},
		{Id: "x9", Operation: OperationSearchPolicy, Policy: "sometimes"},
		{Id: "x10", Operation: OperationList, RepoId: repoId.String(), Scope: "all"},
		{Id: "x11", Operation: OperationSearchStatus, Query: "q"},
	} {
		if reply := testReplyOf(t, world.worker, refused); reply.Ok || reply.Code != CodeInvalid {
			t.Errorf("%s: %+v", refused.Id, reply)
		}
	}
	if reply := testReplyOf(t, world.worker, &Request{Id: "m6", Operation: OperationSearchMore, SearchId: strings.Repeat("b", 32)}); reply.Code != CodeSearchExpired {
		t.Errorf("an unknown search: %+v", reply)
	}

	// a Worker with no search, and a core with no engine, answer unavailable
	bare := newTestWorkerWorld(t, nil)
	if reply := testReplyOf(t, bare.worker, &Request{Id: "u1", Operation: OperationSearch, Query: "q"}); reply.Code != CodeUnavailable {
		t.Errorf("no search: %+v", reply)
	}
	search.searchErr = sidecar.ErrSidecarSearchUnavailable
	if reply := testReplyOf(t, world.worker, &Request{Id: "u2", Operation: OperationSearch, Query: "q"}); reply.Code != CodeUnavailable {
		t.Errorf("no engine: %+v", reply)
	}
	search.searchErr = nil
	// an engine error mid-search ends it with its code alone, its text nowhere
	search.scripts = [][]testSearchPull{{{err: errors.New("an engine that repeats query-canary: " + sidecar.ErrSearchCursorExpired.Error())}}}
	if reply := testReplyOf(t, world.worker, &Request{Id: "e1", Operation: OperationSearch, Query: "query-canary"}); reply.Ok || strings.Contains(reply.Message, "canary") {
		t.Errorf("an engine error: %+v", reply)
	}
}

// A Worker holds at most SearchSessionMaxCount searches, the least recently used ending past it, and one unused past
// SearchIdleTimeout ends at the next search.
func TestSearchSessionsAreBounded(t *testing.T) {
	now := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := &now
	search := &testWorkerSearch{}
	world := newTestSearchWorld(t, search, func(args *WorkerArgs) {
		args.Now = func() time.Time { return *clock }
		args.Settings.SearchSessionMaxCount = 2
	})
	searchIds := []string{}
	for i := 0; i < 3; i++ {
		search.scripts = [][]testSearchPull{{{hits: []sidecar.SearchHit{testWorkerSearchHit(sidecar.NewId(), 0)}}, {}}}
		reply := testReplyOf(t, world.worker, &Request{Id: "s", Operation: OperationSearch, Query: "q", PageSize: "1"})
		searchIds = append(searchIds, reply.Search.SearchId)
		*clock = clock.Add(time.Second)
	}
	if count := world.worker.searches.count(); count != 2 || !search.sources[0].isClosed() {
		t.Fatalf("%d searches open, the oldest closed %v", count, search.sources[0].isClosed())
	}
	if reply := testReplyOf(t, world.worker, &Request{Id: "m", Operation: OperationSearchMore, SearchId: searchIds[0]}); reply.Code != CodeSearchExpired {
		t.Errorf("the ended search: %+v", reply)
	}
	*clock = clock.Add(world.worker.settings.SearchIdleTimeout)
	search.scripts = [][]testSearchPull{{{hits: []sidecar.SearchHit{testWorkerSearchHit(sidecar.NewId(), 0)}}}}
	testReplyOf(t, world.worker, &Request{Id: "s", Operation: OperationSearch, Query: "q"})
	if count := world.worker.searches.count(); count != 1 || !search.sources[1].isClosed() || !search.sources[2].isClosed() {
		t.Errorf("idle searches: %d open", count)
	}
}

// Pages receive hit views only (D-SDK-24): each hit carries its display fields and state, cut to their bounds, a name
// that is not UTF-8 as text and base64, and nothing of the file's content.
func TestHitsCarryNoRawFile(t *testing.T) {
	repoId := sidecar.NewId()
	long := strings.Repeat("é", 300)
	search := &testWorkerSearch{scripts: [][]testSearchPull{{{hits: []sidecar.SearchHit{{
		Repo: repoId, Path: "mail/" + string([]byte{0xff}) + "x", Version: 1 << 60, Time: time.UnixMilli(1_800_000_000_000),
		State: sidecar.SearchVerified, Partition: sidecar.SearchPartitionAll, Snippet: long, Correction: "plan",
		Fields: []sidecar.SearchField{{Field: 2, Text: long}, {Field: 4, Text: "Run card"}, {Field: 9, Text: "limiter-fix"}, {Field: 5, Text: "the body text"}},
	}}, done: true}}}}
	world := newTestSearchWorld(t, search, nil)
	encoded := world.worker.HandleJson(context.Background(), []byte(`{"id":"s","op":"search","query":"plan"}`))
	reply := &struct {
		Search struct {
			Hits []map[string]any `json:"hits"`
		} `json:"search"`
	}{}
	if err := json.Unmarshal(encoded, reply); err != nil || len(reply.Search.Hits) != 1 {
		t.Fatalf("the reply: %s %v", encoded, err)
	}
	allowed := map[string]bool{"repo_id": true, "path": true, "version": true, "time_millis": true, "title": true, "from": true, "snippet": true,
		"state": true, "partition": true, "correction": true, "by": true}
	for key := range reply.Search.Hits[0] {
		if !allowed[key] {
			t.Errorf("a hit view carries %s", key)
		}
	}
	if strings.Contains(string(encoded), "the body text") || strings.Contains(string(encoded), "content") {
		t.Errorf("a hit view carries the body: %s", encoded)
	}
	search.scripts = [][]testSearchPull{{{hits: []sidecar.SearchHit{{
		Repo: repoId, Path: "mail/" + string([]byte{0xff}) + "x", Version: 1 << 60, Snippet: long, State: sidecar.SearchVerified,
		Fields: []sidecar.SearchField{{Field: 2, Text: long}, {Field: 4, Text: "Run card"}},
	}}, done: true}}}
	hit := testReplyOf(t, world.worker, &Request{Id: "s3", Operation: OperationSearch, Query: "plan"}).Search.Hits[0]
	settings := DefaultWorkerSettings()
	if len(hit.Snippet) > settings.SearchSnippetMaxByteCount || len(hit.From) > settings.SearchFieldMaxByteCount || !strings.HasPrefix(long, hit.Snippet) {
		t.Errorf("the bounds: snippet %d, from %d", len(hit.Snippet), len(hit.From))
	}
	if hit.Path.Base64 == "" || hit.Version != "1152921504606846976" || hit.State != "verified" || hit.Title != "Run card" {
		t.Errorf("the hit: %+v", hit)
	}
}

// The quick burner searches its buckets in memory and never fetches all: its core keeps no cache on any store and
// keeps no all, and a search of scope all is refused before the core sees it.
func TestQuickBurnerBucketsInMemoryOnly(t *testing.T) {
	settings := sidecar.DefaultSidecarSettings()
	DefaultQuickBurnerSearchSettings().Apply(settings)
	if settings.Search.CacheByteCount != 0 || settings.Search.AllCacheByteCount != 0 || settings.Search.AllPolicy != sidecar.SearchPolicyNever ||
		settings.Search.RemoteAsk || settings.Search.RemoteServe {
		t.Errorf("the burner's search settings: %+v", settings.Search)
	}
	search := &testWorkerSearch{scripts: [][]testSearchPull{{{hits: []sidecar.SearchHit{testWorkerSearchHit(sidecar.NewId(), 0)}, done: true}}}}
	world := newTestSearchWorld(t, search, nil)
	burner, err := NewQuickBurner(&WorkerArgs{
		Sidecar: world.sidecar,
		Repo:    func(repoId sidecar.Id) (WorkerRepo, error) { return world.repo, nil },
		Records: func() (WorkerRecords, error) { return world.records, nil },
		Search:  func() (WorkerSearch, error) { return search, nil },
	}, &QuickBurnerArgs{HandClose: func(string) {}})
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"all"} {
		encoded := burner.HandleJson(context.Background(), []byte(`{"id":"b","op":"search","query":"code","scope":"`+scope+`"}`))
		reply := &Reply{}
		if err := json.Unmarshal(encoded, reply); err != nil || reply.Ok || reply.Code != CodeUnsupported {
			t.Errorf("a burner's scope %s: %s", scope, encoded)
		}
	}
	if len(search.requests) != 0 {
		t.Fatal("a burner's scope all reached the core")
	}
	encoded := burner.HandleJson(context.Background(), []byte(`{"id":"b","op":"search","query":"code"}`))
	reply := &Reply{}
	if err := json.Unmarshal(encoded, reply); err != nil || !reply.Ok || reply.Search == nil || len(reply.Search.Hits) != 1 {
		t.Errorf("a burner's bucket search: %s", encoded)
	}
	if len(search.requests) != 1 || search.requests[0].Scope != sidecar.SearchCached {
		t.Errorf("the burner's request: %+v", search.requests)
	}
}

// The page Worker's core search runs as §18.4's browsers and caps rows say: wifi_charging, one search in an 8 MiB
// working set beside the version-table pages, 128 MiB a network, 30 days unused evicted, no requester side, and each
// pull bounded by the Worker's yield interval.
func TestPageSearchSettings(t *testing.T) {
	settings := sidecar.DefaultSidecarSettings()
	DefaultPageSearchSettings().Apply(settings)
	search := settings.Search
	if search.AllPolicy != sidecar.SearchPolicyWifiCharging || search.MemoryByteCount != 8*1024*1024+256*1024 || search.DaemonSearchCount != 1 ||
		search.CacheByteCount != 128*1024*1024 || search.AllCacheByteCount != 128*1024*1024 || search.UnusedAllAge != 30*24*time.Hour ||
		search.RemoteAsk || search.RemoteServe || search.SyncRoundCount != 1 || search.PageCpu != DefaultWorkerSettings().YieldInterval {
		t.Errorf("the page Worker's search settings: %+v", search)
	}
	if search.MemoryByteCount < search.VersionCacheByteCount {
		t.Error("the version-table pages do not fit the search memory")
	}
}

// The frame's report reaches the Sidecar's search: the three conditions as SetSearchConditions, and the tab's
// visibility as the core's foreground where the core has the setter.
func TestConditionsReachTheSearch(t *testing.T) {
	search := &testWorkerSearch{}
	world := newTestSearchWorld(t, search, nil)
	world.worker.SetConditions(&WorkerConditions{Unmetered: true, Charging: false, LowPower: true, Foreground: true})
	if !slices.Equal(search.conditions, [][3]bool{{true, false, true}}) {
		t.Errorf("the conditions: %v", search.conditions)
	}
	foreground := &testWorkerSearchForeground{testWorkerSearch: &testWorkerSearch{}}
	withForeground := newTestSearchWorld(t, foreground, nil)
	withForeground.worker.SetConditions(&WorkerConditions{Foreground: true})
	withForeground.worker.SetConditions(&WorkerConditions{Foreground: false})
	if !slices.Equal(foreground.foregrounds, []bool{true, false}) {
		t.Errorf("the foreground: %v", foreground.foregrounds)
	}
	// a Worker with no search tells nothing, and no report panics
	newTestWorkerWorld(t, nil).worker.SetConditions(&WorkerConditions{Unmetered: true})
	world.worker.SetConditions(nil)
}

// A search that pulls page after page yields between pulls once the yield interval passed, so a long-poll's fetch and
// other requests interleave on js/wasm's one thread.
func TestSearchYieldsBetweenPulls(t *testing.T) {
	now := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	stepLock := sync.Mutex{}
	yieldCount := 0
	pulls := []testSearchPull{}
	for i := 0; i < 8; i++ {
		pulls = append(pulls, testSearchPull{hits: []sidecar.SearchHit{testWorkerSearchHit(sidecar.NewId(), i)}})
	}
	search := &testWorkerSearch{scripts: [][]testSearchPull{pulls}}
	world := newTestSearchWorld(t, search, func(args *WorkerArgs) {
		args.Now = func() time.Time {
			stepLock.Lock()
			defer stepLock.Unlock()
			// each reading moves the clock past the interval, as a 50 ms pull would
			now = now.Add(60 * time.Millisecond)
			return now
		}
		args.Yield = func(ctx context.Context) error {
			stepLock.Lock()
			defer stepLock.Unlock()
			yieldCount += 1
			return nil
		}
		args.Settings.SearchReplyTimeout = time.Hour
	})
	reply := testReplyOf(t, world.worker, &Request{Id: "y", Operation: OperationSearch, Query: "q", PageSize: "8"})
	if !reply.Ok || len(reply.Search.Hits) != 8 {
		t.Fatalf("the search: %+v", reply)
	}
	if yieldCount < 7 {
		t.Errorf("the search yielded %d times over 8 pulls", yieldCount)
	}
}
