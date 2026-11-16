//go:build !ios_extension && !js

package sdk

// SidecarSearch (SDK-M10-01): pages that replace the last as hits land, More's next page from hits already ranked,
// Cancel ending the stream here and nowhere else, GetSearch nil without evs, the remote backend streaming through the
// daemon's control mode on a fake daemon, and a query canary that reaches no message or log. Every repo and query is
// synthetic, and every wait is on a channel.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"
)

// One pull a test's source answers.
type testSearchStep struct {
	hits     []*SidecarSearchHit
	progress *SidecarSearchProgress
	done     bool
	err      error
}

// A source whose pulls each wait for the test's next step.
type testSearchSource struct {
	steps  chan *testSearchStep
	closed chan struct{}
	once   sync.Once
	pulls  chan struct{}
}

func newTestSearchSource() *testSearchSource {
	return &testSearchSource{steps: make(chan *testSearchStep), closed: make(chan struct{}), pulls: make(chan struct{}, 64)}
}

func (self *testSearchSource) next(ctx context.Context) ([]*SidecarSearchHit, *SidecarSearchProgress, bool, error) {
	self.pulls <- struct{}{}
	select {
	case <-ctx.Done():
		return nil, nil, false, ctx.Err()
	case step := <-self.steps:
		return step.hits, step.progress, step.done, step.err
	}
}

func (self *testSearchSource) close() {
	self.once.Do(func() { close(self.closed) })
}

// Sends one step, once the stream pulls.
func (self *testSearchSource) step(t *testing.T, step *testSearchStep) {
	t.Helper()
	select {
	case self.steps <- step:
	case <-time.After(testSidecarTimeout):
		t.Fatal("the stream did not pull")
	}
}

// A backend whose searches run on the test's sources, recording what it was asked.
type testSearchBackend struct {
	stateLock   sync.Mutex
	requests    []*sidecarSearchRequest
	sources     chan *testSearchSource
	searchErr   error
	statuses    map[string]*SidecarSearchStatus
	policies    map[string]string
	dropped     []string
	conditions  [][3]bool
	foregrounds []bool
	windows     []time.Time
	windowErr   error
	// called inside each background window, nil for nothing
	windowHook func()
}

func newTestSearchBackend() *testSearchBackend {
	return &testSearchBackend{
		sources:  make(chan *testSearchSource, 16),
		statuses: map[string]*SidecarSearchStatus{},
		policies: map[string]string{},
	}
}

// The key a repo id, or every repo, is kept under.
func testSearchRepoKey(repoId *sidecar.Id) string {
	if repoId == nil {
		return "*"
	}
	return repoId.String()
}

func (self *testSearchBackend) search(ctx context.Context, request *sidecarSearchRequest) (sidecarSearchSource, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.requests = append(self.requests, request)
	if self.searchErr != nil {
		return nil, self.searchErr
	}
	source := newTestSearchSource()
	self.sources <- source
	return source, nil
}

func (self *testSearchBackend) status(ctx context.Context, repoId *sidecar.Id) (*SidecarSearchStatus, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	status, ok := self.statuses[testSearchRepoKey(repoId)]
	if !ok {
		return nil, sidecar.ErrSidecarSearchUnavailable
	}
	copied := *status
	if policy, ok := self.policies[testSearchRepoKey(repoId)]; ok && policy != "" {
		copied.Policy = policy
	}
	return &copied, nil
}

func (self *testSearchBackend) dropCache(ctx context.Context, repoId *sidecar.Id) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.dropped = append(self.dropped, testSearchRepoKey(repoId))
	return nil
}

func (self *testSearchBackend) setPolicy(ctx context.Context, repoId *sidecar.Id, policy string) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.policies[testSearchRepoKey(repoId)] = policy
	return nil
}

func (self *testSearchBackend) setConditions(unmetered bool, charging bool, lowPower bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.conditions = append(self.conditions, [3]bool{unmetered, charging, lowPower})
}

func (self *testSearchBackend) setForeground(foreground bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.foregrounds = append(self.foregrounds, foreground)
}

func (self *testSearchBackend) runBackgroundSync(ctx context.Context) error {
	deadline, ok := ctx.Deadline()
	self.stateLock.Lock()
	hook := self.windowHook
	self.stateLock.Unlock()
	if hook != nil {
		hook()
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !ok {
		return errors.New("a window without a deadline")
	}
	self.windows = append(self.windows, deadline)
	return self.windowErr
}

// The next search's source.
func (self *testSearchBackend) nextSource(t *testing.T) *testSearchSource {
	t.Helper()
	select {
	case source := <-self.sources:
		return source
	case <-time.After(testSidecarTimeout):
		t.Fatal("no search started")
		return nil
	}
}

// One page a listener got.
type testSearchPage struct {
	hits     []*SidecarSearchHit
	progress *SidecarSearchProgress
	done     bool
}

// A listener recording each page it gets, in order.
type testSearchListener struct {
	pages chan *testSearchPage
}

func newTestSearchListener() *testSearchListener {
	return &testSearchListener{pages: make(chan *testSearchPage, 64)}
}

func (self *testSearchListener) SearchPage(hits *SidecarSearchHitList, progress *SidecarSearchProgress, done bool) {
	self.pages <- &testSearchPage{hits: hits.getAll(), progress: progress, done: done}
}

// The next page.
func (self *testSearchListener) next(t *testing.T) *testSearchPage {
	t.Helper()
	select {
	case page := <-self.pages:
		return page
	case <-time.After(testSidecarTimeout):
		t.Fatal("no page")
		return nil
	}
}

// Fails if a page arrives before the delivery could have given one: the stream's goroutines have nothing left to give
// once quiet is closed.
func (self *testSearchListener) requireNone(t *testing.T, quiet func()) {
	t.Helper()
	quiet()
	select {
	case page := <-self.pages:
		t.Fatalf("a page arrived: %d hits, done %v", len(page.hits), page.done)
	default:
	}
}

// A hit of the synthetic repo, named by n.
func testSearchHit(n int) *SidecarSearchHit {
	return &SidecarSearchHit{
		RepoId:     newId([16]byte{0x5e, byte(n)}),
		Path:       fmt.Sprintf("notes/%d.md", n),
		Version:    int64(n),
		TimeMillis: 1_800_000_000_000 + int64(n),
		State:      SidecarSearchHitConfirmed,
		Partition:  SidecarSearchPartitionLatest,
	}
}

// The paths of hits, in order.
func testSearchPaths(hits []*SidecarSearchHit) []string {
	paths := []string{}
	for _, hit := range hits {
		paths = append(paths, hit.Path)
	}
	return paths
}

// A SidecarSearch on backend, closed at the test's end.
func newTestSidecarSearch(t *testing.T, backend sidecarSearchBackend, host *sidecarSearchHost) *SidecarSearch {
	t.Helper()
	search := newSidecarSearch(context.Background(), backend, defaultSidecarSearchSettings(), host)
	t.Cleanup(search.close)
	return search
}

// Each page replaces the last: as hits land the listener gets the page so far, each holding every hit before, until
// the stream is done.
func TestPagesReplace(t *testing.T) {
	backend := newTestSearchBackend()
	search := newTestSidecarSearch(t, backend, nil)
	listener := newTestSearchListener()
	handle := search.Search(&SidecarSearchQuery{Text: "quarterly numbers"}, listener)
	defer handle.Cancel()
	source := backend.nextSource(t)

	source.step(t, &testSearchStep{hits: []*SidecarSearchHit{testSearchHit(1), testSearchHit(2)}, progress: &SidecarSearchProgress{Searched: 1, Total: 3}})
	first := listener.next(t)
	if !slices.Equal(testSearchPaths(first.hits), []string{"notes/1.md", "notes/2.md"}) || first.done || first.progress.Searched != 1 {
		t.Fatalf("the first page: %v done %v progress %+v", testSearchPaths(first.hits), first.done, first.progress)
	}
	source.step(t, &testSearchStep{hits: []*SidecarSearchHit{testSearchHit(3)}, progress: &SidecarSearchProgress{Searched: 2, Total: 3}})
	second := listener.next(t)
	if !slices.Equal(testSearchPaths(second.hits), []string{"notes/1.md", "notes/2.md", "notes/3.md"}) || second.done || second.progress.Searched != 2 {
		t.Fatalf("the replacing page: %v done %v", testSearchPaths(second.hits), second.done)
	}
	// a pull that changes nothing gives nothing
	source.step(t, &testSearchStep{progress: &SidecarSearchProgress{Searched: 2, Total: 3}})
	source.step(t, &testSearchStep{progress: &SidecarSearchProgress{Searched: 3, Total: 3}, done: true})
	last := listener.next(t)
	if !slices.Equal(testSearchPaths(last.hits), []string{"notes/1.md", "notes/2.md", "notes/3.md"}) || !last.done || last.progress.Searched != 3 {
		t.Fatalf("the last page: %v done %v progress %+v", testSearchPaths(last.hits), last.done, last.progress)
	}
	if last.progress.Code != "" {
		t.Errorf("a search that ended well carries %q", last.progress.Code)
	}
	select {
	case <-source.closed:
	case <-time.After(testSidecarTimeout):
		t.Fatal("the stream did not end its source after its last page")
	}
	if request := backend.requests[0]; request.text != "quarterly numbers" || request.scope != SidecarSearchScopeCached || request.pageSize != 50 {
		t.Errorf("the request: %+v", request)
	}
}

// More starts the next page from hits already ranked, and past the last page answers an empty one with done set.
func TestMoreNextPage(t *testing.T) {
	backend := newTestSearchBackend()
	search := newTestSidecarSearch(t, backend, nil)
	listener := newTestSearchListener()
	handle := search.Search(&SidecarSearchQuery{Text: "minutes", PageSize: 2}, listener)
	defer handle.Cancel()
	source := backend.nextSource(t)

	source.step(t, &testSearchStep{hits: []*SidecarSearchHit{testSearchHit(1), testSearchHit(2), testSearchHit(3)}, progress: &SidecarSearchProgress{}})
	first := listener.next(t)
	if !slices.Equal(testSearchPaths(first.hits), []string{"notes/1.md", "notes/2.md"}) || first.done {
		t.Fatalf("page 0: %v done %v", testSearchPaths(first.hits), first.done)
	}
	// a full page asks nothing more of the core until More
	select {
	case <-source.pulls:
	default:
		t.Fatal("the first pull was not seen")
	}
	select {
	case <-source.pulls:
		t.Fatal("a full page pulled again before More")
	default:
	}

	handle.More(2)
	source.step(t, &testSearchStep{hits: []*SidecarSearchHit{testSearchHit(4)}, progress: &SidecarSearchProgress{}, done: true})
	second := listener.next(t)
	if !slices.Equal(testSearchPaths(second.hits), []string{"notes/3.md", "notes/4.md"}) || !second.done {
		t.Fatalf("page 1: %v done %v", testSearchPaths(second.hits), second.done)
	}
	handle.More(2)
	past := listener.next(t)
	if len(past.hits) != 0 || !past.done {
		t.Fatalf("a More past the last page: %v done %v", testSearchPaths(past.hits), past.done)
	}

	// a page size past the bound is refused, with no search started
	refused := newTestSearchListener()
	search.Search(&SidecarSearchQuery{Text: "minutes", PageSize: int32(defaultSidecarSearchSettings().PageHitMaxCount + 1)}, refused)
	if page := refused.next(t); !page.done || page.progress.Code != SidecarSearchCodeInvalid {
		t.Fatalf("an oversized page: %+v", page.progress)
	}
	for _, query := range []*SidecarSearchQuery{nil, {Text: "a", Scope: "everything"}, {Text: "a", Remote: "on"}, {Text: "a", SinceMillis: 5, UntilMillis: 4}} {
		invalid := newTestSearchListener()
		search.Search(query, invalid)
		if page := invalid.next(t); !page.done || page.progress.Code != SidecarSearchCodeInvalid {
			t.Errorf("query %+v: %+v", query, page.progress)
		}
	}
	if len(backend.requests) != 1 {
		t.Errorf("a refused query reached the backend: %d requests", len(backend.requests))
	}
}

// Cancel is local: the stream ends here and its source with it, so the core's cursors and remote legs end, and no page
// starts after Cancel returns. On the remote backend no call goes to the daemon after it, and none names a cancel.
func TestCancelStopsLocallyOnly(t *testing.T) {
	backend := newTestSearchBackend()
	search := newTestSidecarSearch(t, backend, nil)
	listener := newTestSearchListener()
	handle := search.Search(&SidecarSearchQuery{Text: "budget"}, listener)
	source := backend.nextSource(t)
	source.step(t, &testSearchStep{hits: []*SidecarSearchHit{testSearchHit(1)}, progress: &SidecarSearchProgress{}})
	listener.next(t)
	handle.Cancel()
	select {
	case <-source.closed:
	case <-time.After(testSidecarTimeout):
		t.Fatal("Cancel did not end the source")
	}
	listener.requireNone(t, func() {})
	// a More or a second Cancel after it gives nothing
	handle.More(10)
	handle.Cancel()
	listener.requireNone(t, func() {})

	// remote: the daemon answers a first page with a cursor; after Cancel nothing more is asked of it
	socketPath := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, socketPath)
	searched := make(chan struct{}, 8)
	daemon.answer = func(request testRemoteDaemonRequest) (any, *sidecar.DaemonRpcError, bool) {
		if request.Method != sidecarControlMethodSearch {
			return nil, nil, false
		}
		searched <- struct{}{}
		return map[string]any{
			"hits":   []map[string]any{{"repo_id": sidecar.Id{0x5e}.String(), "version": 1, "time": "2027-01-15T08:00:00Z", "match": "possible", "partition": "current", "fields": map[string]string{"path": "a.md"}}},
			"status": map[string]any{"complete": false, "next_cursor": "cursor-1"},
		}, nil, true
	}
	daemon.start()
	remote, _, _ := openTestRemoteSidecar(t, socketPath)
	remoteSearch := remote.GetSearch()
	if remoteSearch == nil {
		t.Fatal("a remote Sidecar answers no search")
	}
	remoteListener := newTestSearchListener()
	remoteHandle := remoteSearch.Search(&SidecarSearchQuery{Text: "budget", PageSize: 1}, remoteListener)
	if page := remoteListener.next(t); len(page.hits) != 1 || page.done {
		t.Fatalf("the remote page: %d hits done %v", len(page.hits), page.done)
	}
	remoteHandle.Cancel()
	remoteListener.requireNone(t, func() {})
	before := daemon.seenMethods()
	for _, method := range before {
		if method != sidecarControlMethodSearch {
			t.Errorf("the daemon was asked %s", method)
		}
		if strings.Contains(method, "cancel") {
			t.Errorf("a cancel reached the daemon: %s", method)
		}
	}
	if len(before) != 1 {
		t.Errorf("the daemon was asked %d searches for one full page", len(before))
	}
}

// GetSearch is nil unless the core's evs component is attached; with it, the core's search answers, here
// SEARCH_UNAVAILABLE since no host installs a search engine yet, and after Close it is nil again.
func TestNilWithoutEvs(t *testing.T) {
	world := newTestSidecarWorld(t)
	if bare := world.open(t); bare.GetSearch() != nil {
		t.Error("GetSearch without evs")
	}
	second := newTestSidecarWorld(t)
	settings := DefaultSidecarSettings()
	settings.Components.Add(SidecarComponentEvs)
	local := second.openWith(t, settings, &sidecarLocalOptions{componentFactories: map[string]sidecarComponentFactory{
		SidecarComponentEvs: testSearchEvsComponent,
	}})
	search := local.GetSearch()
	if search == nil || local.GetSearch() != search {
		t.Fatalf("GetSearch with evs: %v", search)
	}
	listener := newTestSearchListener()
	search.Search(&SidecarSearchQuery{Text: "anything"}, listener)
	if page := listener.next(t); !page.done || page.progress.Code != SidecarSearchCodeUnavailable || len(page.hits) != 0 {
		t.Fatalf("a search with no engine: %+v done %v", page.progress, page.done)
	}
	if status := search.GetStatus(nil); status != nil {
		t.Errorf("a status with no engine: %+v", status)
	}
	second.close(t, local)
	if local.GetSearch() != nil {
		t.Error("GetSearch after Close")
	}
	after := newTestSearchListener()
	search.Search(&SidecarSearchQuery{Text: "anything"}, after)
	if page := after.next(t); !page.done || page.progress.Code != SidecarSearchCodeUnavailable {
		t.Errorf("a search after Close: %+v", page.progress)
	}
}

// A test's evs component: the core's EvsComponent over an EvsClient that reaches nothing.
func testSearchEvsComponent(ctx context.Context, local *sidecar.Sidecar, settings *SidecarSettings) (sidecar.SidecarComponent, error) {
	client, err := sidecar.NewEvsClient(&sidecar.EvsClientArgs{
		NetworkId: sidecar.Id{0x71},
		EvsUrl:    "https://evs.sdktest.example",
		Transport: &testCountingTransport{},
		ServiceToken: func(ctx context.Context, resource string, scopes []string) (string, error) {
			return "", errors.New("a test's token refuses")
		},
		Sign: func(purpose string, signedType bridge.SignedType, body []byte, agentId *sidecar.Id) (*bridge.Signed, error) {
			return nil, errors.New("a test's signer refuses")
		},
		Identity: func() *sidecar.SidecarIdentity {
			return &sidecar.SidecarIdentity{NetworkId: sidecar.Id{0x71}, SidecarId: sidecar.Id{0x72}, ClientId: sidecar.Id{0x73}, Generation: 1}
		},
		Gkm:      testRefusingGkm{},
		Store:    testEmptySidecarStore{},
		Settings: sidecar.DefaultEvsSettings(),
	})
	if err != nil {
		return nil, err
	}
	return sidecar.NewEvsComponent(client), nil
}

// A remote-mode Sidecar's search streams through the daemon's control mode: the first call carries §16.10's arguments
// and asks for untrusted text, the next names the reply's cursor alone, the pages replace as each call's hits land,
// and the hits map their match and fields; GetStatus and SetPolicy forward, and DropCache and a background window are
// refused, the daemon running its own.
func TestRemoteModeStreams(t *testing.T) {
	socketPath := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, socketPath)
	repoId := sidecar.Id{0x5e, 0x01}
	// set once the test wants a daemon without the methods
	var missing atomic.Bool
	daemon.answer = func(request testRemoteDaemonRequest) (any, *sidecar.DaemonRpcError, bool) {
		if missing.Load() {
			return nil, nil, false
		}
		switch request.Method {
		case sidecarControlMethodSearch:
			params := &sidecarControlSearchParams{}
			if err := json.Unmarshal(request.Params, params); err != nil {
				return nil, &sidecar.DaemonRpcError{Code: sidecarControlCodeInvalidParams, Message: err.Error()}, true
			}
			if params.Cursor == "" {
				return map[string]any{
					"hits": []map[string]any{
						{"repo_id": repoId.String(), "version": 7, "time": "2027-01-15T08:00:00Z", "match": "confirmed", "by": "text", "partition": "current",
							"fields": map[string]string{"path": "plans/q1.md", "title": "Q1 plan"}, "snippet": "the q1 plan"},
						{"repo_id": repoId.String(), "version": 3, "time": "2026-06-01T08:00:00Z", "match": "confirmed", "by": "field", "partition": "all",
							"fields": map[string]string{"subject": "Re: plan", "from": "Dana"}},
					},
					"status": map[string]any{"complete": false, "next_cursor": "cursor-1"},
				}, nil, true
			}
			return map[string]any{
				"hits":   []map[string]any{{"repo_id": repoId.String(), "version": 2, "time": "2025-02-01T08:00:00Z", "match": "near", "correction": "plan", "partition": "older"}},
				"status": map[string]any{"complete": true},
			}, nil, true
		case sidecarControlMethodSearchStatus:
			return &SidecarSearchStatus{IndexOn: true, FormatSupported: true, Policy: "background", Remote: "off", AllCachedByteCount: 4096}, nil, true
		case sidecarControlMethodSearchPolicy:
			return map[string]any{}, nil, true
		}
		return nil, nil, false
	}
	daemon.start()
	remote, _, _ := openTestRemoteSidecar(t, socketPath)
	search := remote.GetSearch()
	if search == nil {
		t.Fatal("a remote Sidecar answers no search")
	}
	listener := newTestSearchListener()
	handle := search.Search(&SidecarSearchQuery{Text: "plan", RepoIds: testSearchIdList(repoId), Scope: SidecarSearchScopeAll, SinceMillis: 1_700_000_000_000}, listener)
	defer handle.Cancel()
	first := listener.next(t)
	if !slices.Equal(testSearchPaths(first.hits), []string{"plans/q1.md", ""}) || first.done {
		t.Fatalf("the first remote page: %v done %v", testSearchPaths(first.hits), first.done)
	}
	if hit := first.hits[0]; hit.State != SidecarSearchHitVerified || hit.Title != "Q1 plan" || hit.Version != 7 || hit.Partition != SidecarSearchPartitionLatest ||
		hit.TimeMillis != time.Date(2027, 1, 15, 8, 0, 0, 0, time.UTC).UnixMilli() || hit.Snippet != "the q1 plan" || hit.RepoId.Cmp(newId(repoId)) != 0 {
		t.Errorf("the verified hit: %+v", hit)
	}
	if hit := first.hits[1]; hit.State != SidecarSearchHitConfirmed || hit.Title != "Re: plan" || hit.From != "Dana" || hit.Partition != SidecarSearchPartitionAll {
		t.Errorf("the confirmed hit: %+v", hit)
	}
	last := listener.next(t)
	if len(last.hits) != 3 || !last.done || last.hits[2].State != SidecarSearchHitNear || last.hits[2].Correction != "plan" {
		t.Fatalf("the last remote page: %d hits done %v", len(last.hits), last.done)
	}
	searches := []*sidecarControlSearchParams{}
	for _, request := range daemon.seen() {
		if request.Method == sidecarControlMethodSearch {
			params := &sidecarControlSearchParams{}
			json.Unmarshal(request.Params, params)
			searches = append(searches, params)
		}
	}
	if len(searches) != 2 {
		t.Fatalf("the daemon took %d searches", len(searches))
	}
	if first := searches[0]; first.Query != "plan" || first.Scope != sidecarControlSearchScopeRepo || !slices.Equal(first.Repos, []string{"id:" + repoId.String()}) ||
		!first.More || first.Untrusted != sidecarControlSearchUntrustedInclude || first.Limit != 50 || first.WaitMs != 2000 ||
		first.Filters == nil || first.Filters.Since == "" || first.Cursor != "" {
		t.Errorf("the first call's arguments: %+v", first)
	}
	if next := searches[1]; next.Cursor != "cursor-1" || next.Query != "" || next.Untrusted != sidecarControlSearchUntrustedInclude {
		t.Errorf("the cursor call's arguments: %+v", next)
	}

	if status := search.GetStatus(newId(repoId)); status == nil || !status.IndexOn || status.Policy != "background" || status.AllCachedByteCount != 4096 {
		t.Errorf("the remote status: %+v", status)
	}
	policy, policyResults := testSidecarResultCallback()
	search.SetPolicy(newId(repoId), sidecar.SearchPolicyNever, policy)
	if result := <-policyResults; !result.Ok {
		t.Errorf("the remote policy: %+v", result)
	}
	for _, request := range daemon.seen() {
		if request.Method == sidecarControlMethodSearchPolicy {
			params := &sidecarControlSearchPolicyParams{}
			if json.Unmarshal(request.Params, params) != nil || params.Policy != sidecar.SearchPolicyNever || params.RepoId == nil || params.RepoId.Cmp(newId(repoId)) != 0 {
				t.Errorf("search.policy's params: %s", request.Params)
			}
		}
	}
	dropped, droppedResults := testSidecarResultCallback()
	search.DropCache(nil, dropped)
	if result := <-droppedResults; result.Ok || result.Code != SidecarSearchCodeRefused {
		t.Errorf("a remote DropCache: %+v", result)
	}
	window, windowResults := testSidecarResultCallback()
	search.RunBackgroundSync(1000, window)
	if result := <-windowResults; result.Ok || result.Code != SidecarSearchCodeRefused {
		t.Errorf("a remote background window: %+v", result)
	}

	// a daemon without the method reads unavailable
	missing.Store(true)
	unanswered := newTestSearchListener()
	search.Search(&SidecarSearchQuery{Text: "plan"}, unanswered)
	if page := unanswered.next(t); !page.done || page.progress.Code != SidecarSearchCodeUnavailable {
		t.Errorf("a daemon with no search method: %+v", page.progress)
	}
}

// An IdList of ids.
func testSearchIdList(ids ...sidecar.Id) *IdList {
	list := NewIdList()
	for _, id := range ids {
		list.Add(newId(id))
	}
	return list
}

// A query never reaches a message, a log or a crash report (I-19): a core error and a daemon refusal that each repeat
// the query answer their code's fixed text, and no file of the wrapper logs at all.
func TestQueryCanaryInNoLog(t *testing.T) {
	const canary = "query-canary-7f3a9c"
	backend := newTestSearchBackend()
	search := newTestSidecarSearch(t, backend, nil)
	listener := newTestSearchListener()
	handle := search.Search(&SidecarSearchQuery{Text: canary}, listener)
	defer handle.Cancel()
	source := backend.nextSource(t)
	source.step(t, &testSearchStep{err: fmt.Errorf("an engine that repeats %s: %w", canary, sidecar.ErrSearchBusy)})
	page := listener.next(t)
	if !page.done || page.progress.Code != SidecarSearchCodeBusy || strings.Contains(page.progress.Message, canary) {
		t.Fatalf("a core error: %+v", page.progress)
	}

	backend.searchErr = fmt.Errorf("a backend that repeats %s", canary)
	refused := newTestSearchListener()
	search.Search(&SidecarSearchQuery{Text: canary}, refused)
	if page := refused.next(t); page.progress.Code != SidecarSearchCodeFailed || strings.Contains(page.progress.Message, canary) {
		t.Fatalf("a backend refusal: %+v", page.progress)
	}

	socketPath := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, socketPath)
	daemon.answer = func(request testRemoteDaemonRequest) (any, *sidecar.DaemonRpcError, bool) {
		if request.Method == sidecarControlMethodSearch {
			return nil, &sidecar.DaemonRpcError{Code: sidecar.DaemonRpcFailed, Message: "a daemon that repeats " + canary}, true
		}
		return nil, nil, false
	}
	daemon.start()
	remote, _, _ := openTestRemoteSidecar(t, socketPath)
	remoteListener := newTestSearchListener()
	remote.GetSearch().Search(&SidecarSearchQuery{Text: canary}, remoteListener)
	if page := remoteListener.next(t); page.progress.Code != SidecarSearchCodeFailed || strings.Contains(page.progress.Message, canary) {
		t.Fatalf("a daemon refusal: %+v", page.progress)
	}
	for code, message := range sidecarSearchCodeMessages {
		if strings.Contains(message, canary) || message == "" {
			t.Errorf("%s's message: %q", code, message)
		}
	}

	// no file of the wrapper logs: none imports a logger or prints
	files, err := filepath.Glob("sidecar_search*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("the wrapper's files: %v %v", files, err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imported := range parsed.Imports {
			path := strings.Trim(imported.Path.Value, `"`)
			if path == "log" || path == "log/slog" || strings.HasSuffix(path, "/glog") || path == "fmt" {
				t.Errorf("%s imports %s", file, path)
			}
		}
		contents, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, call := range []string{"Infof(", "Warningf(", "Errorf(", "Println(", "Printf(", "gmLog("} {
			if strings.Contains(string(contents), call) {
				t.Errorf("%s calls %s", file, call)
			}
		}
	}
}

// The bound types cross as their json names say, and a status, a progress and an indexer map the core's field by field.
func TestSidecarSearchMapsTheCoresTypes(t *testing.T) {
	repoId := sidecar.Id{0x5e, 0x09}
	memberId := sidecar.Id{0x6d}
	at := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	hit := sidecarSearchHitOf(&sidecar.SearchHit{
		Repo: repoId, Path: "mail/inbox/42", Version: 42, Time: at, State: sidecar.SearchUnmatched, Partition: sidecar.SearchPartitionAll,
		Fields: []sidecar.SearchField{{Field: 2, Text: "Dana"}, {Field: 1, Text: "Invoice"}}, Snippet: "your invoice", By: "dana-laptop",
	})
	if hit.State != SidecarSearchHitUnmatched || hit.Partition != SidecarSearchPartitionAll || hit.Title != "Invoice" || hit.From != "Dana" ||
		hit.TimeMillis != at.UnixMilli() || hit.By != "dana-laptop" || hit.Version != 42 || hit.Path != "mail/inbox/42" {
		t.Errorf("the hit: %+v", hit)
	}
	record := sidecarSearchHitOf(&sidecar.SearchHit{Repo: repoId, State: sidecar.SearchVerified, Fields: []sidecar.SearchField{{Field: 4, Text: "Run card"}, {Field: 9, Text: "limiter-fix"}}})
	if record.Title != "Run card" || record.From != "limiter-fix" || record.State != SidecarSearchHitVerified {
		t.Errorf("a record's hit: %+v", record)
	}
	indexerId := sidecar.Id{0x1d}
	progress := sidecarSearchProgressOf(&sidecar.SearchProgress{Searched: 2, Total: 4, FetchBytes: 10, FetchedBytes: 5, Waiting: "metered", CoveredFrom: at,
		Remote: "asking", FallbackReason: "busy", RemoteBy: &indexerId}, func(sidecarId sidecar.Id) *SidecarIndexer {
		return sidecarIndexerOf(&sidecar.SearchIndexer{SidecarId: sidecarId, Name: "nas", MemberId: &memberId, Own: true, State: "online", ServedRepoIds: []sidecar.Id{repoId}})
	})
	if progress.Searched != 2 || progress.Total != 4 || progress.FetchByteCount != 10 || progress.FetchedByteCount != 5 || progress.Waiting != "metered" ||
		progress.CoveredFromMillis != at.UnixMilli() || progress.Remote != "asking" || progress.FallbackReason != "busy" ||
		progress.RemoteBy == nil || progress.RemoteBy.Name != "nas" || progress.RemoteBy.ServedRepoIds.Len() != 1 || progress.RemoteBy.MemberId.Cmp(newId(memberId)) != 0 {
		t.Errorf("the progress: %+v", progress)
	}
	status := sidecarSearchStatusOf(&sidecar.SearchStatus{IndexOn: true, FormatSupported: true, Policy: "wifi_charging", Remote: "own", LatestFrom: at, Uncovered: 3})
	if !status.IndexOn || status.Policy != "wifi_charging" || status.Remote != "own" || status.LatestFromMillis != at.UnixMilli() || status.AllCachedThroughMillis != 0 || status.Uncovered != 3 {
		t.Errorf("the status: %+v", status)
	}
	encoded, err := json.Marshal(&SidecarSearchQuery{Text: "x", RepoIds: testSearchIdList(repoId), Scope: "all", AllowMetered: true, PageSize: 10})
	if err != nil || !strings.Contains(string(encoded), `"repo_ids":["`) || !strings.Contains(string(encoded), `"allow_metered":true`) {
		t.Errorf("the query's json: %s %v", encoded, err)
	}
	decoded := &SidecarSearchQuery{}
	if err := json.Unmarshal(encoded, decoded); err != nil || decoded.RepoIds.Len() != 1 || decoded.Scope != "all" {
		t.Errorf("the query from json: %+v %v", decoded, err)
	}
	listJson, err := json.Marshal(NewSidecarSearchHitList())
	if err != nil || string(listJson) != "[]" {
		t.Errorf("an empty hit list: %s", listJson)
	}
}

// The index files the ios_extension leg compiles on sidecar at 85eac4d7, through the subprotocol's search RPC and the
// search cache's file class (question sdk-m10-01-ios-leg-links-index-code). §22.5's §18 row allows none; until the
// question's split lands, the check below fails on any index file beyond these, and logs these as pending.
var iosExtensionIndexFilesPending = map[string]bool{
	"evs_index_dictionary.go":   true,
	"evs_index_entry.go":        true,
	"evs_index_keyset.go":       true,
	"evs_index_lone.go":         true,
	"evs_index_normalize.go":    true,
	"evs_index_pad.go":          true,
	"evs_index_postings.go":     true,
	"evs_index_row.go":          true,
	"evs_index_settings.go":     true,
	"evs_index_shard.go":        true,
	"evs_index_shard_reader.go": true,
	"evs_index_slot.go":         true,
	"evs_index_tombstones.go":   true,
	"evs_index_trigram.go":      true,
	"evs_index_vbyte.go":        true,
	"protocol/evsindex.pb.go":   true,
	"protocol/search.pb.go":     true,
}

// The extension's leg links no index code (§22.5's §18 row): of the sidecar module, no root evs_index_ or search_ file
// and neither of protocol's evsindex and search descriptors, while the app's leg links them. The sdk's own search files
// carry !ios_extension; the root's files the leg compiles today are held to the pending set above.
func TestIosExtensionLinksNoIndexCode(t *testing.T) {
	moduleDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	isIndexFile := func(name string) bool {
		base := filepath.Base(name)
		return strings.HasPrefix(base, "search_") || strings.HasPrefix(base, "evs_index_") || base == "evsindex.pb.go" || base == "search.pb.go"
	}
	compiledIndexFiles := func(tags string) []string {
		findings, _ := sidecarIosExtensionFileFindings(t, "the "+tags+" leg", moduleDirectory, testingIosExtensionEnvironment(), []string{"-tags", tags, "."}, map[string]bool{})
		found := []string{}
		for _, finding := range findings {
			for _, field := range strings.Fields(finding) {
				if name := strings.TrimSuffix(strings.TrimPrefix(field, "module's "), ","); isIndexFile(name) {
					found = append(found, name)
				}
			}
		}
		return found
	}
	pending := []string{}
	for _, name := range compiledIndexFiles("sdk_mobile_bind,ios_extension") {
		if iosExtensionIndexFilesPending[name] {
			pending = append(pending, name)
			continue
		}
		t.Errorf("the ios_extension leg compiles index code beyond the pending set: %s", name)
	}
	if 0 < len(pending) {
		t.Logf("the ios_extension leg still compiles %d index files, pending question sdk-m10-01-ios-leg-links-index-code: %v", len(pending), pending)
	}
	appFiles := compiledIndexFiles("sdk_mobile_bind")
	if !slices.ContainsFunc(appFiles, func(name string) bool { return strings.HasPrefix(filepath.Base(name), "search_") }) {
		t.Fatalf("CONTROL FAILED: the app's leg compiles no search_ file, so the scan saw nothing: %v", appFiles)
	}
	// the sdk's own search files never build on the leg
	for _, file := range []string{"sidecar_search.go", "sidecar_search_types.go", "sidecar_search_remote.go", "sidecar_search_host.go", "sidecar_search_conditions.go"} {
		if !strings.HasPrefix(testFirstLine(t, file), "//go:build !ios_extension") {
			t.Errorf("%s does not carry !ios_extension first", file)
		}
	}
}

// The first line of file.
func testFirstLine(t *testing.T, file string) string {
	t.Helper()
	contents, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	line, _, _ := strings.Cut(string(contents), "\n")
	return line
}
