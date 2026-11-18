//go:build !ios_extension && !js

package sdk

// SidecarSearch (SDK-M10-01): pages that replace the last as hits land, More's next page from hits already ranked,
// GetSearch nil without evs, and the bound types mapping the core's; every app leg builds these. The cases that run a
// fake ur daemon, Cancel's, the remote stream's and the query canary's, are sidecar_search_daemon_test.go's, which
// phones' builds leave out. Every repo and query is synthetic, and every wait is on a channel.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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

// An IdList of ids.
func testSearchIdList(ids ...sidecar.Id) *IdList {
	list := NewIdList()
	for _, id := range ids {
		list.Add(newId(id))
	}
	return list
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

// The index files the ios_extension leg may compile: none (§22.5's §18 row; QD-988, question
// sdk-m10-01-ios-leg-links-index-code). sidecar took the subprotocol's search RPC behind a leg file
// (rpc_search_leg.go) and the search cache's file class out of the cache, so the seventeen files that stood here
// while the leg still compiled them are gone. It stays a set so that a file allowed again is a line a diff shows.
var iosExtensionIndexFilesAllowed = map[string]bool{}

// The extension's leg links no index code (§22.5's §18 row): of the sidecar module, no root evs_index_ or search_ file
// and neither of protocol's evsindex and search descriptors, while the app's leg links them. The sdk's own search files
// carry !ios_extension.
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
	for _, name := range compiledIndexFiles("sdk_mobile_bind,ios_extension") {
		if !iosExtensionIndexFilesAllowed[name] {
			t.Errorf("the ios_extension leg compiles index code: %s", name)
		}
	}
	appFiles := compiledIndexFiles("sdk_mobile_bind")
	if !slices.ContainsFunc(appFiles, func(name string) bool { return strings.HasPrefix(filepath.Base(name), "search_") }) {
		t.Fatalf("CONTROL FAILED: the app's leg compiles no search_ file, so the scan saw nothing: %v", appFiles)
	}
	// the scan reads the names the leg once compiled, so a leg compiling one again is seen
	for _, name := range []string{"evs_index_row.go", "protocol/evsindex.pb.go", "protocol/search.pb.go"} {
		if !slices.Contains(appFiles, name) {
			t.Fatalf("CONTROL FAILED: the app's leg compiles no %s, so the scan would not see it on the extension's: %v", name, appFiles)
		}
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
