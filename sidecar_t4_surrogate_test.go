//go:build !ios_extension && !js

package sdk

// T4's requester surrogate (SDK-M10-03; §22.4's T4, FS-83's T4 form, D-SDK-25's remote targets, R-SDK-14; owner
// decision 100): a phone app host on the ios32 or the Android profile, its sidecar role booted and enrolled as the app
// does (testT4Role), each step held to the class's budget, whose core runs the search engine the surrogate installs,
// and which the test drives only through the bound SidecarSearch, as an app does. FS-83's app-host cases run on it
// (SEARCH-M10-39), at the later of M10's and M8's exits.
//
//	install    builds the engine from the enrolled core: its search state, conditions and foreground. Here it is a
//	           fake requester before fake indexers (testT4Requester), since remote search's requester is SEARCH's
//	           SearchRemoteClient, which no host composes on app hosts yet (CORE's wiring), and whose config state, a
//	           Team's verified log designating indexers, no test of the sdk's module can build. The fake reads the
//	           mode and the notice answers from the core's real search state, as SearchRemoteClient does; asks an
//	           indexer only for "search more", never for a keystroke; asks only own indexers in mode own and none in
//	           mode off; asks another member's only once its notice is allowed; remembers what each served in the
//	           search state; and reports DESIGN-DETAILS 7.2.4's states and fallback reasons. SEARCH-M10-39 installs
//	           the real requester through the same install, over an indexer of its own
//	indexers   fake indexers (testT4FakeIndexer): own or another member's, each reading some repos and answering them
//	           with hits verified and labeled with its device name, or down, when a request reaches it and no reply
//	           comes. Each records what reached it, the query included, which a designated indexer's holder sees by
//	           design (§18.4's notice)
//	directory  phones' directory "search more" (§16.11's phones row, §18.4's directory row): the team directory is one
//	           of the repos, asked of a designated indexer that reads it, and fetched for the fallback alone
//
// The surrogate devices' own runs of the same cells are external (owner decision 100), as are §18.4's remote targets,
// which §22.8's remote row measures with real relays.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest/t4cell"
)

// One request that reached a fake indexer.
type testT4Ask struct {
	query   string
	repoIds []sidecar.Id
}

// A fake indexer (the file's header). Safe for concurrent use.
type testT4FakeIndexer struct {
	sidecarId sidecar.Id
	name      string
	memberId  sidecar.Id
	member    string
	own       bool
	repoIds   []sidecar.Id

	stateLock sync.Mutex
	down      bool
	asked     []testT4Ask
}

// Whether the indexer reads repoId.
func (self *testT4FakeIndexer) reads(repoId sidecar.Id) bool {
	return slices.Contains(self.repoIds, repoId)
}

// Sets whether the indexer answers.
func (self *testT4FakeIndexer) setDown(down bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.down = down
}

// Takes a request: its hits, verified and labeled with the indexer's name, one a repo, or false when it is down.
func (self *testT4FakeIndexer) ask(query string, repoIds []sidecar.Id, at time.Time) ([]sidecar.SearchHit, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.asked = append(self.asked, testT4Ask{query: query, repoIds: slices.Clone(repoIds)})
	if self.down {
		return nil, false
	}
	hits := []sidecar.SearchHit{}
	for _, repoId := range repoIds {
		hits = append(hits, sidecar.SearchHit{
			Repo: repoId, Path: "remote/" + self.name + "/" + repoId.String() + ".eml", Version: 3, Time: at,
			Fields: []sidecar.SearchField{{Field: sidecarSearchFieldSubject, Text: "Harbor schedule"}}, Snippet: "the harbor ferry",
			State: sidecar.SearchVerified, Partition: sidecar.SearchPartitionAll, By: self.name,
		})
	}
	return hits, true
}

// What reached the indexer, in order.
func (self *testT4FakeIndexer) asks() []testT4Ask {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.asked)
}

// The device's own search, standing for its cache: each repo holds one latest document and one in all, a title each,
// that a query matches by containment. A search of all is "search more"'s download, its all hits possible matches.
type testT4LocalEngine struct {
	// the repos, in the scope's order, and each one's title
	repoIds []sidecar.Id
	titles  map[sidecar.Id]string
	at      time.Time
}

// See sidecar.SidecarSearchEngine: a request naming no repo searches every one.
func (self *testT4LocalEngine) Search(ctx context.Context, request *sidecar.SearchRequest) (*sidecar.SearchStream, error) {
	repoIds := request.Repos
	if len(repoIds) == 0 {
		repoIds = self.repoIds
	}
	return sidecar.NewSearchStream(&testT4Pages{pages: []testT4Page{{
		hits: self.hits(request, repoIds), progress: sidecar.SearchProgress{Searched: 1, Total: 1}, done: true,
	}}}), nil
}

// The device's own hits of repoIds for request.
func (self *testT4LocalEngine) hits(request *sidecar.SearchRequest, repoIds []sidecar.Id) []sidecar.SearchHit {
	hits := []sidecar.SearchHit{}
	query := strings.ToLower(request.Text)
	for _, repoId := range repoIds {
		title := self.titles[repoId]
		if query == "" || !strings.Contains(strings.ToLower(title), query) {
			continue
		}
		hits = append(hits, sidecar.SearchHit{
			Repo: repoId, Path: "local/" + repoId.String() + ".md", Version: 1, Time: self.at,
			Fields: []sidecar.SearchField{{Field: sidecarSearchFieldTitle, Text: title}},
			State:  sidecar.SearchConfirmed, Partition: sidecar.SearchPartitionLatest,
		})
		if request.Scope == sidecar.SearchAll {
			hits = append(hits, sidecar.SearchHit{
				Repo: repoId, Path: "local/" + repoId.String() + "-archive.md", Version: 1, Time: self.at.Add(-400 * 24 * time.Hour),
				State: sidecar.SearchPossible, Partition: sidecar.SearchPartitionAll,
			})
		}
	}
	return hits
}

// See sidecar.SidecarSearchEngine.
func (self *testT4LocalEngine) Status(repoId *sidecar.Id) (*sidecar.SearchStatus, error) {
	return &sidecar.SearchStatus{IndexOn: true, FormatSupported: true}, nil
}

// One page of a canned stream.
type testT4Page struct {
	hits     []sidecar.SearchHit
	progress sidecar.SearchProgress
	done     bool
}

// A stream of canned pages. Safe for concurrent use.
type testT4Pages struct {
	// every page after the first waits until this closes; nil waits for nothing
	gate <-chan struct{}

	stateLock sync.Mutex
	pages     []testT4Page
	given     int
	closed    bool
}

// See sidecar.SearchStreamSource.
func (self *testT4Pages) Next() ([]sidecar.SearchHit, sidecar.SearchProgress, bool, error) {
	wait := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.gate != nil && 0 < self.given
	}()
	if wait {
		<-self.gate
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.given += 1
	if self.closed {
		return nil, sidecar.SearchProgress{}, false, sidecar.ErrSidecarSearchUnavailable
	}
	if len(self.pages) == 0 {
		return nil, sidecar.SearchProgress{}, true, nil
	}
	page := self.pages[0]
	self.pages = self.pages[1:]
	return page.hits, page.progress, page.done, nil
}

// See sidecar.SearchStreamSource.
func (self *testT4Pages) Close() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.closed = true
}

// The surrogate's fake requester (the file's header): the SidecarSearchEngine the core runs, and its indexer lister.
// Safe for concurrent use.
type testT4Requester struct {
	state    *sidecar.SidecarSearchState
	local    sidecar.SidecarSearchEngine
	indexers []*testT4FakeIndexer
	// the repos this device reads with its index on, in the scope's order
	repoIds []sidecar.Id
	now     func() time.Time

	stateLock sync.Mutex
	// the next "search more"'s last page waits until this closes; nil holds none
	gate chan struct{}
}

// Holds the last page of the next "search more" until release, so its listener sees the asking state's page first.
// release is safe to call again.
func (self *testT4Requester) holdNextAnswer() (release func()) {
	gate := make(chan struct{})
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.gate = gate
	var once sync.Once
	return func() {
		once.Do(func() { close(gate) })
	}
}

// One repo's lot in a search (DESIGN-DETAILS 7.2.3's states, once the plan ran).
type testT4RepoLot struct {
	servedBy   *testT4FakeIndexer
	fallback   string
	notice     *testT4FakeIndexer
	hadIndexer bool
}

// See sidecar.SidecarSearchEngine: the device's own search alone for a keystroke and in mode off; for "search more",
// each repo asked of an indexer the mode and the answers let answer it, the rest the device's download.
func (self *testT4Requester) Search(ctx context.Context, request *sidecar.SearchRequest) (*sidecar.SearchStream, error) {
	scope := request.Repos
	if len(scope) == 0 {
		scope = self.repoIds
	}
	if request.Scope != sidecar.SearchAll {
		// keystrokes never leave the device
		return self.local.Search(ctx, request)
	}
	mode := request.Remote
	lots := map[sidecar.Id]*testT4RepoLot{}
	byIndexer := map[*testT4FakeIndexer][]sidecar.Id{}
	for _, repoId := range scope {
		lot := self.plan(mode, repoId)
		lots[repoId] = lot
		if lot.servedBy != nil {
			byIndexer[lot.servedBy] = append(byIndexer[lot.servedBy], repoId)
		}
	}
	// page 0 goes to each target with its repos; a target that does not answer fails, its repos falling back offline
	remoteHits := []sidecar.SearchHit{}
	at := self.now()
	for _, indexer := range self.indexers {
		repoIds, ok := byIndexer[indexer]
		if !ok {
			continue
		}
		hits, answered := indexer.ask(request.Text, repoIds, at)
		if !answered {
			for _, repoId := range repoIds {
				lots[repoId].servedBy = nil
				lots[repoId].fallback = sidecar.SearchFallbackOffline
			}
			continue
		}
		for _, repoId := range repoIds {
			if err := self.state.NoteServed(indexer.sidecarId, repoId); err != nil {
				return nil, err
			}
		}
		remoteHits = append(remoteHits, hits...)
	}
	fallbacks := []sidecar.Id{}
	for _, repoId := range scope {
		if lots[repoId].servedBy == nil {
			fallbacks = append(fallbacks, repoId)
		}
	}
	local := self.localHitsOf(request, scope, fallbacks)
	asking := 0 < len(byIndexer)
	first := sidecar.SearchProgress{Searched: 1, Total: 2}
	if asking {
		first.Remote = sidecar.SearchRemoteStateAsking
	}
	last := self.progressOf(mode, scope, lots)
	last.Searched, last.Total = 2, 2
	gate := func() chan struct{} {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		gate := self.gate
		self.gate = nil
		return gate
	}()
	pages := &testT4Pages{pages: []testT4Page{
		{hits: local, progress: first},
		{hits: remoteHits, progress: last, done: true},
	}}
	if gate != nil {
		pages.gate = gate
	}
	return sidecar.NewSearchStream(pages), nil
}

// The device's own hits: a keystroke's search of every repo, and the download of the repos that fell back.
func (self *testT4Requester) localHitsOf(request *sidecar.SearchRequest, scope []sidecar.Id, fallbacks []sidecar.Id) []sidecar.SearchHit {
	engine, ok := self.local.(*testT4LocalEngine)
	if !ok {
		return nil
	}
	cached := *request
	cached.Scope = sidecar.SearchCached
	hits := engine.hits(&cached, scope)
	for _, hit := range engine.hits(request, fallbacks) {
		if hit.Partition == sidecar.SearchPartitionAll {
			hits = append(hits, hit)
		}
	}
	return hits
}

// One repo's plan (DESIGN-DETAILS 7.2.3's assign, for one indexer at a time): off in mode off; else the first own
// indexer that reads it; else, in mode on, another member's that reads it once its member's notice allowed it, the
// notice waiting otherwise; else none.
func (self *testT4Requester) plan(mode string, repoId sidecar.Id) *testT4RepoLot {
	lot := &testT4RepoLot{}
	if mode == sidecar.SearchRemoteOff {
		lot.fallback = sidecar.SearchFallbackOff
		return lot
	}
	for _, indexer := range self.indexers {
		if indexer.own && indexer.reads(repoId) {
			lot.servedBy, lot.hadIndexer = indexer, true
			return lot
		}
	}
	if mode == sidecar.SearchRemoteOn {
		for _, indexer := range self.indexers {
			if indexer.own || !indexer.reads(repoId) {
				continue
			}
			lot.hadIndexer = true
			allow, answered, err := self.state.Answer(indexer.memberId)
			if err == nil && answered && allow {
				lot.servedBy = indexer
				return lot
			}
			lot.fallback, lot.notice = sidecar.SearchFallbackNotice, indexer
			return lot
		}
	}
	lot.fallback = sidecar.SearchFallbackNone
	return lot
}

// The last page's remote state, fallback reason and indexer (DESIGN-DETAILS 7.2.4's table).
func (self *testT4Requester) progressOf(mode string, scope []sidecar.Id, lots map[sidecar.Id]*testT4RepoLot) sidecar.SearchProgress {
	progress := sidecar.SearchProgress{}
	served := map[*testT4FakeIndexer]int{}
	servedCount, noticeCount, hadIndexer := 0, 0, false
	var notice *testT4FakeIndexer
	for _, repoId := range scope {
		lot := lots[repoId]
		hadIndexer = hadIndexer || lot.hadIndexer
		if lot.servedBy != nil {
			servedCount += 1
			served[lot.servedBy] += 1
			continue
		}
		if progress.FallbackReason == "" {
			progress.FallbackReason = lot.fallback
		}
		if lot.fallback == sidecar.SearchFallbackNotice {
			noticeCount += 1
			if notice == nil {
				notice = lot.notice
			}
		}
	}
	switch {
	case mode == sidecar.SearchRemoteOff:
		progress.Remote = sidecar.SearchRemoteStateOff
	case 0 < noticeCount && servedCount == 0:
		progress.Remote = sidecar.SearchRemoteStateNotice
	case servedCount == len(scope):
		progress.Remote = sidecar.SearchRemoteStateAnswered
	case 0 < servedCount:
		progress.Remote = sidecar.SearchRemoteStatePartial
	case hadIndexer:
		progress.Remote = sidecar.SearchRemoteStateFallback
	}
	most := 0
	for _, indexer := range self.indexers {
		if count := served[indexer]; most < count {
			sidecarId := indexer.sidecarId
			progress.RemoteBy, most = &sidecarId, count
		}
	}
	if progress.RemoteBy == nil && notice != nil {
		sidecarId := notice.sidecarId
		progress.RemoteBy = &sidecarId
	}
	return progress
}

// See sidecar.SidecarSearchEngine.
func (self *testT4Requester) Status(repoId *sidecar.Id) (*sidecar.SearchStatus, error) {
	return self.local.Status(repoId)
}

// See sidecar.SidecarSearchIndexerLister: every fake indexer, own first; one that is down unreachable, one that
// answered online.
func (self *testT4Requester) Indexers() []sidecar.SearchIndexer {
	indexers := []sidecar.SearchIndexer{}
	for _, indexer := range self.indexers {
		memberId := indexer.memberId
		listed := sidecar.SearchIndexer{SidecarId: indexer.sidecarId, Name: indexer.name, MemberId: &memberId, Member: indexer.member, Own: indexer.own, State: SidecarIndexerUnknown}
		func() {
			indexer.stateLock.Lock()
			defer indexer.stateLock.Unlock()
			switch {
			case indexer.down:
				listed.State = SidecarIndexerUnreachable
			case 0 < len(indexer.asked):
				listed.State = SidecarIndexerOnline
			}
		}()
		indexers = append(indexers, listed)
	}
	slices.SortStableFunc(indexers, func(a sidecar.SearchIndexer, b sidecar.SearchIndexer) int {
		switch {
		case a.Own && !b.Own:
			return -1
		case b.Own && !a.Own:
			return 1
		}
		return 0
	})
	return indexers
}

// The engine and the lister the core runs.
var (
	_ sidecar.SidecarSearchEngine        = (*testT4Requester)(nil)
	_ sidecar.SidecarSearchIndexerLister = (*testT4Requester)(nil)
	_ sidecar.SidecarSearchEngine        = (*testT4LocalEngine)(nil)
)

// FS-83's T4 world, in the small: this member's mailbox, the team record and the team directory; its own NAS, a search
// indexer that reads the mailbox and the directory; and another member's network-scope indexer, which reads the record
// and the directory, never the mailbox, which is owner-bound.
type testT4RemoteWorld struct {
	memberId    sidecar.Id
	otherId     sidecar.Id
	mailboxId   sidecar.Id
	recordId    sidecar.Id
	directoryId sidecar.Id
	nas         *testT4FakeIndexer
	laptop      *testT4FakeIndexer
	local       *testT4LocalEngine
	// the requester install built last
	requester *testT4Requester
}

func newTestT4RemoteWorld() *testT4RemoteWorld {
	world := &testT4RemoteWorld{
		memberId: sidecar.NewId(), otherId: sidecar.NewId(),
		mailboxId: sidecar.NewId(), recordId: sidecar.NewId(), directoryId: sidecar.NewId(),
	}
	world.nas = &testT4FakeIndexer{sidecarId: sidecar.NewId(), name: "home-nas", memberId: world.memberId, member: "Robin", own: true,
		repoIds: []sidecar.Id{world.mailboxId, world.directoryId}}
	world.laptop = &testT4FakeIndexer{sidecarId: sidecar.NewId(), name: "dana-laptop", memberId: world.otherId, member: "Dana",
		repoIds: []sidecar.Id{world.recordId, world.directoryId}}
	world.local = &testT4LocalEngine{
		repoIds: []sidecar.Id{world.mailboxId, world.recordId, world.directoryId},
		at:      time.Date(2027, time.April, 2, 9, 0, 0, 0, time.UTC),
		titles: map[sidecar.Id]string{
			world.mailboxId: "Harbor tickets", world.recordId: "Harbor run card", world.directoryId: "Harbor worker",
		},
	}
	return world
}

// The world's requester on core, its search state the core's own.
func (self *testT4RemoteWorld) install(core *sidecar.Sidecar) (sidecar.SidecarSearchEngine, error) {
	state, err := core.SearchState()
	if err != nil {
		return nil, err
	}
	self.requester = &testT4Requester{
		state: state, local: self.local, indexers: []*testT4FakeIndexer{self.nas, self.laptop},
		repoIds: slices.Clone(self.local.repoIds), now: time.Now,
	}
	return self.requester, nil
}

// The requester surrogate: a phone's role on cell, enrolled, its core running what install builds.
type testT4Surrogate struct {
	t      *testing.T
	cell   *t4cell.Cell
	class  string
	role   *testT4Role
	search *SidecarSearch
}

// Boots and enrolls the surrogate's role on cell's profile under class, each step held to its sidecar budget, installs
// the engine install builds from the enrolled core, and reports the app in the foreground, as an app does at start.
func newTestT4Surrogate(t *testing.T, cell *t4cell.Cell, class string, install func(core *sidecar.Sidecar) (sidecar.SidecarSearchEngine, error)) *testT4Surrogate {
	t.Helper()
	role := newTestT4Role(t, class)
	budget := cell.Profile().SidecarBudgetByteCount
	if _, err := cell.Measure("requester surrogate boots ("+class+")", budget, t4cell.HoldsPeak, func() (any, error) {
		return role.boot()
	}); err != nil {
		t.Fatal(err)
	}
	role.awaitUnenrolled()
	if _, err := cell.Measure("requester surrogate enrolls ("+class+")", budget, t4cell.HoldsPeak, func() (any, error) {
		return nil, role.enroll()
	}); err != nil {
		t.Fatal(err)
	}
	engine, err := install(role.local.Core())
	if err != nil {
		t.Fatal(err)
	}
	uninstall, err := role.local.Core().InstallSearchEngine(engine)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(uninstall)
	search := role.local.GetSearch()
	if search == nil {
		t.Fatal("the surrogate's role has no search")
	}
	search.SetForeground(true)
	return &testT4Surrogate{t: t, cell: cell, class: class, role: role, search: search}
}

// Runs query through the bound SidecarSearch to its last page, measured on the cell as step and held to the class's
// sidecar budget: every page the listener got, in order.
func (self *testT4Surrogate) searchStep(step string, query *SidecarSearchQuery) []*testSearchPage {
	self.t.Helper()
	return self.searchStepWatching(step, query, nil)
}

// searchStep, onPage seeing each page as it arrives, on the test's goroutine; nil sees none.
func (self *testT4Surrogate) searchStepWatching(step string, query *SidecarSearchQuery, onPage func(page *testSearchPage)) []*testSearchPage {
	self.t.Helper()
	pages := []*testSearchPage{}
	if _, err := self.cell.Measure(step+" ("+self.class+")", self.cell.Profile().SidecarBudgetByteCount, t4cell.HoldsPeak, func() (any, error) {
		listener := newTestSearchListener()
		handle := self.search.Search(query, listener)
		defer handle.Cancel()
		for {
			page := listener.next(self.t)
			pages = append(pages, page)
			if onPage != nil {
				onPage(page)
			}
			if page.done {
				return nil, nil
			}
		}
	}); err != nil {
		self.t.Fatal(err)
	}
	return pages
}

// The last page.
func testT4LastPage(t *testing.T, pages []*testSearchPage) *testSearchPage {
	t.Helper()
	if len(pages) == 0 {
		t.Fatal("no page")
	}
	return pages[len(pages)-1]
}

// Whether some hit is verified by name.
func testT4VerifiedBy(hits []*SidecarSearchHit, name string) bool {
	return slices.ContainsFunc(hits, func(hit *SidecarSearchHit) bool {
		return hit.State == SidecarSearchHitVerified && hit.By == name
	})
}

// One answer of a result callback.
func testT4Result(t *testing.T, call func(callback SidecarResultCallback)) *SidecarResult {
	t.Helper()
	callback, results := testSidecarResultCallback()
	call(callback)
	select {
	case result := <-results:
		return result
	case <-time.After(testSidecarTimeout):
		t.Fatal("no result")
		return nil
	}
}

// The surrogate boots against a fake indexer on both phone profiles, within each class's budget at every step, and
// runs §18.4's remote rows through the bound API: GetIndexers lists the own indexer first; a keystroke's search asks
// no indexer; "search more" asks the own indexer for the mailbox and the directory, verified hits labeled by its
// name, and holds the team record behind another member's notice, nothing reaching that member's device; the notice
// allowed, the record goes there; refused, the mode reads own and that device is asked nothing more; in mode off no
// indexer is asked; with the own indexer down the mailbox falls back offline, never to the other member's device.
func TestT4SurrogateBootsAgainstAFakeIndexer(t *testing.T) {
	for _, c := range []struct {
		profile *t4cell.Profile
		class   string
	}{
		{profile: t4cell.Ios32Profile(), class: SidecarHostClassIos},
		{profile: t4cell.AndroidProfile(), class: SidecarHostClassAndroid},
	} {
		world := newTestT4RemoteWorld()
		cell := t4cell.NewCell(c.profile)
		surrogate := newTestT4Surrogate(t, cell, c.class, world.install)
		search := surrogate.search

		indexers := search.GetIndexers().getAll()
		if len(indexers) != 2 || !indexers[0].Own || indexers[0].Name != "home-nas" || indexers[1].Own || indexers[1].Member != "Dana" ||
			indexers[1].Acknowledged || indexers[1].MemberId.Cmp(newId(world.otherId)) != 0 {
			t.Fatalf("%s: the indexers %+v", c.class, indexers)
		}
		if status := search.GetStatus(nil); status == nil || status.Remote != SidecarSearchRemoteOn {
			t.Fatalf("%s: the mode at start: %+v", c.class, status)
		}

		// a keystroke's search stays on the device
		typed := testT4LastPage(t, surrogate.searchStep("a keystroke's search", &SidecarSearchQuery{Text: "harbor"}))
		if typed.progress.Remote != "" || len(world.nas.asks()) != 0 || len(world.laptop.asks()) != 0 || len(typed.hits) != 3 {
			t.Fatalf("%s: a cached search: remote %q, %d hits, asked %d and %d", c.class, typed.progress.Remote, len(typed.hits), len(world.nas.asks()), len(world.laptop.asks()))
		}

		// "search more": the own indexer for the mailbox and the directory, the record behind the notice; the page reads
		// asking until page 0 comes, which the requester holds until the listener has seen it
		release := world.requester.holdNextAnswer()
		t.Cleanup(release)
		sawAsking := false
		pages := surrogate.searchStepWatching("search more, a notice unanswered", &SidecarSearchQuery{Text: "harbor", Scope: SidecarSearchScopeAll}, func(page *testSearchPage) {
			if page.progress.Remote == SidecarSearchRemoteStateAsking && !page.done {
				sawAsking = true
				release()
			}
		})
		if !sawAsking {
			t.Errorf("%s: no page read asking before page 0: %d pages", c.class, len(pages))
		}
		more := testT4LastPage(t, pages)
		if more.progress.Remote != SidecarSearchRemoteStatePartial || more.progress.FallbackReason != SidecarSearchFallbackNotice ||
			more.progress.RemoteBy == nil || more.progress.RemoteBy.Name != "home-nas" || !testT4VerifiedBy(more.hits, "home-nas") {
			t.Fatalf("%s: search more: %+v by %+v", c.class, more.progress, more.progress.RemoteBy)
		}
		if asked := world.nas.asks(); len(asked) != 1 || !slices.Equal(asked[0].repoIds, []sidecar.Id{world.mailboxId, world.directoryId}) {
			t.Errorf("%s: the own indexer was asked %+v", c.class, asked)
		}
		if asked := world.laptop.asks(); len(asked) != 0 {
			t.Fatalf("%s: another member's device was asked before its notice: %+v", c.class, asked)
		}

		// the notice alone: the record waits on Dana's device, which the progress names
		noticed := testT4LastPage(t, surrogate.searchStep("search more of the record, the notice", &SidecarSearchQuery{
			Text: "harbor", Scope: SidecarSearchScopeAll, RepoIds: testSearchIdList(world.recordId),
		}))
		if noticed.progress.Remote != SidecarSearchRemoteStateNotice || noticed.progress.FallbackReason != SidecarSearchFallbackNotice ||
			noticed.progress.RemoteBy == nil || noticed.progress.RemoteBy.Name != "dana-laptop" || noticed.progress.RemoteBy.Member != "Dana" ||
			len(world.laptop.asks()) != 0 {
			t.Fatalf("%s: the notice: %+v by %+v, asked %d", c.class, noticed.progress, noticed.progress.RemoteBy, len(world.laptop.asks()))
		}

		// [Search]: the record goes to Dana's device
		if result := testT4Result(t, func(callback SidecarResultCallback) { search.AcknowledgeIndexer(newId(world.otherId), true, callback) }); !result.Ok {
			t.Fatalf("%s: allowing the notice: %+v", c.class, result)
		}
		if indexers := search.GetIndexers().getAll(); len(indexers) != 2 || !indexers[1].Acknowledged {
			t.Errorf("%s: the answered notice's indexer: %+v", c.class, indexers)
		}
		allowed := testT4LastPage(t, surrogate.searchStep("search more of the record, allowed", &SidecarSearchQuery{
			Text: "harbor", Scope: SidecarSearchScopeAll, RepoIds: testSearchIdList(world.recordId),
		}))
		if allowed.progress.Remote != SidecarSearchRemoteStateAnswered || !testT4VerifiedBy(allowed.hits, "dana-laptop") || len(world.laptop.asks()) != 1 {
			t.Fatalf("%s: allowed: %+v, asked %d", c.class, allowed.progress, len(world.laptop.asks()))
		}
		if indexers := search.GetIndexers().getAll(); len(indexers) != 2 || indexers[1].ServedRepoIds.Len() != 1 || indexers[1].State != SidecarIndexerOnline {
			t.Errorf("%s: what Dana's device served: %+v", c.class, indexers[1])
		}

		// [Only my devices]: own, and Dana's device is asked nothing more
		if result := testT4Result(t, func(callback SidecarResultCallback) { search.AcknowledgeIndexer(newId(world.otherId), false, callback) }); !result.Ok {
			t.Fatalf("%s: refusing the notice: %+v", c.class, result)
		}
		if status := search.GetStatus(nil); status == nil || status.Remote != SidecarSearchRemoteOwn {
			t.Fatalf("%s: the mode after the refusal: %+v", c.class, status)
		}
		own := testT4LastPage(t, surrogate.searchStep("search more in mode own", &SidecarSearchQuery{Text: "harbor", Scope: SidecarSearchScopeAll}))
		if own.progress.Remote != SidecarSearchRemoteStatePartial || own.progress.FallbackReason != SidecarSearchFallbackNone || len(world.laptop.asks()) != 1 {
			t.Fatalf("%s: mode own: %+v, Dana's device asked %d", c.class, own.progress, len(world.laptop.asks()))
		}

		// off: the fallback alone, no indexer asked
		if result := testT4Result(t, func(callback SidecarResultCallback) { search.SetRemote(SidecarSearchRemoteOff, callback) }); !result.Ok {
			t.Fatalf("%s: SetRemote off: %+v", c.class, result)
		}
		nasAsked := len(world.nas.asks())
		off := testT4LastPage(t, surrogate.searchStep("search more in mode off", &SidecarSearchQuery{Text: "harbor", Scope: SidecarSearchScopeAll}))
		if off.progress.Remote != SidecarSearchRemoteStateOff || off.progress.FallbackReason != SidecarSearchFallbackOff ||
			len(world.nas.asks()) != nasAsked || len(world.laptop.asks()) != 1 {
			t.Fatalf("%s: mode off: %+v", c.class, off.progress)
		}

		// the own indexer down: the mailbox falls back offline, to the download, never to Dana's device
		if result := testT4Result(t, func(callback SidecarResultCallback) { search.SetRemote(SidecarSearchRemoteOn, callback) }); !result.Ok {
			t.Fatalf("%s: SetRemote on: %+v", c.class, result)
		}
		world.nas.setDown(true)
		down := testT4LastPage(t, surrogate.searchStep("search more of the mailbox, its indexer down", &SidecarSearchQuery{
			Text: "harbor", Scope: SidecarSearchScopeAll, RepoIds: testSearchIdList(world.mailboxId),
		}))
		if down.progress.Remote != SidecarSearchRemoteStateFallback || down.progress.FallbackReason != SidecarSearchFallbackOffline ||
			len(world.laptop.asks()) != 1 || !slices.ContainsFunc(down.hits, func(hit *SidecarSearchHit) bool { return hit.Partition == SidecarSearchPartitionAll }) {
			t.Fatalf("%s: the own indexer down: %+v, %d hits", c.class, down.progress, len(down.hits))
		}
		if indexers := search.GetIndexers().getAll(); indexers[0].State != SidecarIndexerUnreachable {
			t.Errorf("%s: a down indexer reads %q", c.class, indexers[0].State)
		}

		if err := cell.CheckWithin(); err != nil {
			t.Error(err)
		}
		t.Log("\n" + cell.Table())
	}
}

// Phones' directory "search more" (§16.11's phones row, §18.4's directory row): the phone keeps no directory buckets,
// and a "search more" of the directory asks a designated indexer that reads it, own first, the buckets and all fetched
// only for its fallback: with no indexer answering, the directory's download alone.
func TestT4SurrogateDirectorySearchMoreAsksAnIndexer(t *testing.T) {
	world := newTestT4RemoteWorld()
	cell := t4cell.NewCell(t4cell.Ios32Profile())
	surrogate := newTestT4Surrogate(t, cell, SidecarHostClassIos, world.install)
	if surrogate.role.local.hostClass.search.DirectoryBuckets {
		t.Fatal("a phone keeps the directory's buckets synced")
	}
	directory := testSearchIdList(world.directoryId)
	asked := testT4LastPage(t, surrogate.searchStep("the directory's search more", &SidecarSearchQuery{Text: "harbor", Scope: SidecarSearchScopeAll, RepoIds: directory}))
	if asked.progress.Remote != SidecarSearchRemoteStateAnswered || !testT4VerifiedBy(asked.hits, "home-nas") ||
		slices.ContainsFunc(asked.hits, func(hit *SidecarSearchHit) bool { return hit.Partition == SidecarSearchPartitionAll && hit.By == "" }) {
		t.Fatalf("the directory's search more: %+v", asked.progress)
	}
	world.nas.setDown(true)
	world.laptop.setDown(true)
	fallback := testT4LastPage(t, surrogate.searchStep("the directory's search more, no indexer answering", &SidecarSearchQuery{Text: "harbor", Scope: SidecarSearchScopeAll, RepoIds: directory}))
	if fallback.progress.FallbackReason != SidecarSearchFallbackOffline ||
		!slices.ContainsFunc(fallback.hits, func(hit *SidecarSearchHit) bool { return hit.Partition == SidecarSearchPartitionAll && hit.By == "" }) {
		t.Fatalf("the directory's fallback: %+v, %d hits", fallback.progress, len(fallback.hits))
	}
	if err := cell.CheckWithin(); err != nil {
		t.Error(err)
	}
}

// What the surrogate's own tests need of its pieces: a fake indexer that is down answers nothing but still takes the
// request, as a device whose reply never comes; the requester reports none of the mode's work for a cached search.
func TestT4SurrogateFakesHoldTheirRules(t *testing.T) {
	indexer := &testT4FakeIndexer{sidecarId: sidecar.NewId(), name: "nas", own: true}
	indexer.setDown(true)
	if hits, answered := indexer.ask("q", []sidecar.Id{sidecar.NewId()}, time.Now()); answered || hits != nil || len(indexer.asks()) != 1 {
		t.Errorf("a down indexer: %v %v %d", hits, answered, len(indexer.asks()))
	}
	pages := &testT4Pages{pages: []testT4Page{{done: true}}}
	pages.Close()
	if _, _, _, err := pages.Next(); !errors.Is(err, sidecar.ErrSidecarSearchUnavailable) {
		t.Errorf("a closed stream: %v", err)
	}
}
