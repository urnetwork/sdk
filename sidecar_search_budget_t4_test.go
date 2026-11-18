//go:build sidecar_testhooks && !ios_extension && !js

package sdk

// T4's phone cells with search on (SDK-M10-06; §22.4's T4 from M10, §18.4's targets and caps, D-SDK-25, FS-77, FS-83,
// FS-109, FS-110, FI-18, FI-23; owner decision 100): on the ios32 and Android profiles, a phone's sidecar role booted and
// enrolled as the app does (testT4Role), its core running the search world's engine (SearchTestWorld's device, the
// sidecar_testhooks seam FS-77, FS-109 and FS-110 run on), and every search going through the app's own calls, the
// bound SidecarSearch, between the cell and the engine:
//
//	searches   from the buckets, beside 20,000 nonmatching updates and an all row a newer version masks, which never
//	           shows
//	downloads  "search more" with no indexer designated (owner decision 177's default): every all shard fetched and
//	           searched, the archived match found
//	asks       "search more" with T4's requester surrogate (sidecar_t4_surrogate_test.go) before the world's engine: the
//	           own indexer answers the repo with verified hits, and the device downloads nothing
//	budget     each search's heap growth by FS-109's measure (t4cell's MeasureSearch: the live heap after a forced
//	           collection at every record the search opens and every page the app's listener gets, less its value at
//	           the search's start), held to §18.4's working set beside the version-table pages, which is the phone
//	           class's Search.MemoryByteCount; "search more"'s download is reported against it rather than held, since
//	           the download's staging runs inside the search (SEARCH-M10-38's report); and a deliberate per-search key
//	           collection, FS-109's 500,000 keys, fails the cell, so the cells catch the bug they exist for
//
// The T4 rows of FS-77, FS-83, FS-109, FS-110 and FS-125 are their owners' tests run through these cells; FS-125 is held
// with C19's window accounting (QD-1038). The cells run on this host under the profiles' limits and build for an
// Android surrogate as the bind builds the sdk (cmd/t4cell); a surrogate device's run is external, and no figure here
// is a device's (owner decision 100).

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest/t4cell"
)

// FS-109's count at which a build that collects the buckets' keys per search fails the cap.
const testT4PerSearchKeyCount = 500000

// One phone cell's search world on a booted, enrolled role: the world, its device, and where its records' samples go.
type testT4SearchCell struct {
	t     *testing.T
	cell  *t4cell.Cell
	class string
	role  *testT4Role
	world *sidecar.SearchTestWorld
	clock *testSearchCellClock
	// the device's conditions, which a step sets
	conditionsLock sync.Mutex
	conditions     sidecar.SearchConditions
	device         *sidecar.SearchTestDevice
	// the sample the running step takes at each record, nil between steps
	sample atomic.Pointer[func()]
	// a deliberate per-search key collection, made at a search's first record while planted
	planted   atomic.Bool
	plantLock sync.Mutex
	keys      [][16]byte
}

// A cell's world of updates on a role booted and enrolled on cell's profile under class, each step held to the class's
// sidecar budget, the world's device synced and its engine installed on the role's core.
func newTestT4SearchCell(t *testing.T, cell *t4cell.Cell, class string, updates int) *testT4SearchCell {
	t.Helper()
	ctx := context.Background()
	self := &testT4SearchCell{t: t, cell: cell, class: class, clock: newTestSearchCellClock(),
		conditions: sidecar.SearchConditions{Reported: true, Foreground: true}}
	self.role = newTestT4Role(t, class)
	budget := cell.Profile().SidecarBudgetByteCount
	if _, err := cell.Measure("phone search role boots ("+class+")", budget, t4cell.HoldsPeak, func() (any, error) {
		return self.role.boot()
	}); err != nil {
		t.Fatal(err)
	}
	self.role.awaitUnenrolled()
	if _, err := cell.Measure("phone search role enrolls ("+class+")", budget, t4cell.HoldsPeak, func() (any, error) {
		return nil, self.role.enroll()
	}); err != nil {
		t.Fatal(err)
	}
	// the phone class's search settings, as the role's host applied them
	if self.role.local.hostClass.search.SearchMemoryByteCount != cell.Profile().SearchByteCount() {
		t.Fatalf("%s: the class's Search.MemoryByteCount %d is not §18.4's working set beside the version pages, %d",
			class, self.role.local.hostClass.search.SearchMemoryByteCount, cell.Profile().SearchByteCount())
	}
	self.world = testSearchCellWorld(t, self.clock, updates)
	device, err := self.world.NewDevice(ctx, &sidecar.SearchTestDeviceArgs{
		Settings: testSearchCellSettings(), Conditions: self.currentConditions, StateDirectory: t.TempDir(),
		ReadByteCount: 32 * 1024 * 1024, OnRecord: self.onRecord,
	})
	if err != nil {
		t.Fatal(err)
	}
	self.device = device
	t.Cleanup(device.Close)
	if err := device.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	self.install(device.Engine())
	return self
}

// The device's conditions now.
func (self *testT4SearchCell) currentConditions() sidecar.SearchConditions {
	self.conditionsLock.Lock()
	defer self.conditionsLock.Unlock()
	return self.conditions
}

// Sets the device's conditions.
func (self *testT4SearchCell) setConditions(conditions sidecar.SearchConditions) {
	self.conditionsLock.Lock()
	defer self.conditionsLock.Unlock()
	self.conditions = conditions
}

// At each record a search opens: the running step's sample, and while planted the per-search key collection.
func (self *testT4SearchCell) onRecord() {
	if self.planted.Load() {
		func() {
			self.plantLock.Lock()
			defer self.plantLock.Unlock()
			if self.keys == nil {
				self.keys = make([][16]byte, testT4PerSearchKeyCount)
				for i := range self.keys {
					self.keys[i][0], self.keys[i][1], self.keys[i][2] = byte(i), byte(i>>8), byte(i>>16)
				}
			}
		}()
	}
	if sample := self.sample.Load(); sample != nil {
		(*sample)()
	}
}

// Installs engine on the role's core in place of any before.
func (self *testT4SearchCell) install(engine sidecar.SidecarSearchEngine) {
	self.t.Helper()
	uninstall, err := self.role.local.Core().InstallSearchEngine(engine)
	if err != nil {
		self.t.Fatal(err)
	}
	self.t.Cleanup(uninstall)
}

// Runs query through the bound SidecarSearch to its last page as step, by FS-109's measure, held to budgetByteCount (0
// reports): a sample at every record the search opens and every page the listener gets. The pages, in order.
func (self *testT4SearchCell) searchStep(step string, budgetByteCount int64, query *SidecarSearchQuery) []*testSearchPage {
	self.t.Helper()
	search := self.role.local.GetSearch()
	if search == nil {
		self.t.Fatal("the role has no search")
	}
	pages := []*testSearchPage{}
	_, err := self.cell.MeasureSearch(step+" ("+self.class+")", budgetByteCount, func(sample func()) error {
		self.sample.Store(&sample)
		defer self.sample.Store(nil)
		listener := newTestSearchListener()
		handle := search.Search(query, listener)
		defer handle.Cancel()
		for {
			page := listener.next(self.t)
			sample()
			pages = append(pages, page)
			if page.done {
				return nil
			}
		}
	})
	// the plant, made for one search, goes with it
	self.plantLock.Lock()
	self.keys = nil
	self.plantLock.Unlock()
	if err != nil {
		self.t.Fatal(err)
	}
	return pages
}

// The last page's hits' paths.
func testT4SearchPaths(pages []*testSearchPage) []string {
	if len(pages) == 0 {
		return nil
	}
	return testSearchPaths(pages[len(pages)-1].hits)
}

// The cells on one profile: a phone that searches, downloads and asks, each within §18.4's working set but the
// download, which is reported.
func testT4SearchesDownloadsAndAsks(t *testing.T, profile *t4cell.Profile, class string) {
	cell := t4cell.NewCell(profile)
	searchBudget := profile.SearchByteCount()

	// searches: the buckets beside 20,000 nonmatching updates, the masked all row never shown
	masked := newTestT4SearchCell(t, cell, class, 20000)
	pages := masked.searchStep("searches the buckets beside a masked all row and 20,000 updates", searchBudget, &SidecarSearchQuery{Text: "harbor"})
	paths := testT4SearchPaths(pages)
	if slices.ContainsFunc(paths, func(path string) bool { return strings.HasPrefix(path, "archive/") }) || len(paths) != 2 {
		t.Errorf("%s: the buckets' search found %v, want the two latest matches and no masked row", class, paths)
	}
	if last := pages[len(pages)-1]; last.progress.Code != "" || last.progress.Remote != "" {
		t.Errorf("%s: the buckets' search: %+v", class, last.progress)
	}

	// downloads: search more with no indexer designated, every all shard fetched on Wi-Fi
	plain := newTestT4SearchCell(t, cell, class, 0)
	plain.setConditions(sidecar.SearchConditions{Reported: true, Foreground: true, Unmetered: true})
	more := plain.searchStep("downloads every all shard for search more", 0, &SidecarSearchQuery{Text: "harbor", Scope: SidecarSearchScopeAll})
	if paths := testT4SearchPaths(more); len(paths) != 3 || !slices.Contains(paths, "archive/old-note.md") {
		t.Errorf("%s: search more found %v, want the latest two and the archived one", class, paths)
	}
	if fetched := plain.device.Fetches(); len(fetched) == 0 {
		t.Errorf("%s: search more fetched nothing", class)
	}

	// asks: the surrogate's requester before the world's engine, the own indexer reading the world's repo
	state, err := plain.role.local.Core().SearchState()
	if err != nil {
		t.Fatal(err)
	}
	nas := &testT4FakeIndexer{sidecarId: sidecar.NewId(), name: "home-nas", memberId: sidecar.NewId(), own: true, repoIds: []sidecar.Id{plain.world.RepoId()}}
	plain.install(&testT4Requester{
		state: state, local: plain.device.Engine(), indexers: []*testT4FakeIndexer{nas},
		repoIds: []sidecar.Id{plain.world.RepoId()}, now: plain.clock.Now,
	})
	fetchedBefore := len(plain.device.Fetches())
	asked := plain.searchStep("asks its own indexer for search more", searchBudget, &SidecarSearchQuery{Text: "harbor", Scope: SidecarSearchScopeAll})
	last := asked[len(asked)-1]
	if last.progress.Remote != SidecarSearchRemoteStateAnswered || !testT4VerifiedBy(last.hits, "home-nas") || len(nas.asks()) != 1 {
		t.Errorf("%s: the ask: %+v, %d asks", class, last.progress, len(nas.asks()))
	}
	if fetched := plain.device.Fetches(); len(fetched) != fetchedBefore {
		t.Errorf("%s: a search the own indexer answered fetched %d objects", class, len(fetched)-fetchedBefore)
	}

	if err := cell.CheckWithin(); err != nil {
		t.Error(err)
	}
	t.Log("\n" + cell.Table())
}

// The ios32 surrogate's phone searches, downloads and asks within §18.4's working set.
func TestIos32CellSearchesDownloadsAndAsks(t *testing.T) {
	testT4SearchesDownloadsAndAsks(t, t4cell.Ios32Profile(), SidecarHostClassIos)
}

// The Android surrogate's, whose search matches iOS's (owner decision 197).
func TestAndroidCellSearchesDownloadsAndAsks(t *testing.T) {
	testT4SearchesDownloadsAndAsks(t, t4cell.AndroidProfile(), SidecarHostClassAndroid)
}

// A deliberate per-search key collection fails the cell (FS-109's last clause): the same bucket search, with FS-109's
// 500,000 keys collected at its first record and held to its end, grows past §18.4's working set and CheckWithin names
// it, while the search without the plant stays within.
func TestT4SearchBudgetCatchesAPerSearchKeyCollection(t *testing.T) {
	profile := t4cell.Ios32Profile()
	clean := t4cell.NewCell(profile)
	cell := newTestT4SearchCell(t, clean, SidecarHostClassIos, 20000)
	cell.searchStep("searches the buckets", profile.SearchByteCount(), &SidecarSearchQuery{Text: "harbor"})
	if err := clean.CheckWithin(); err != nil {
		t.Fatalf("CONTROL FAILED: the search without the plant: %v", err)
	}

	plantedCell := t4cell.NewCell(profile)
	cell.cell = plantedCell
	cell.planted.Store(true)
	cell.searchStep("searches the buckets, collecting every key", profile.SearchByteCount(), &SidecarSearchQuery{Text: "harbor"})
	cell.planted.Store(false)
	err := plantedCell.CheckWithin()
	if err == nil {
		t.Fatalf("a search holding %d collected keys stayed within %d B: %s", testT4PerSearchKeyCount, profile.SearchByteCount(), plantedCell.Table())
	}
	if !strings.Contains(err.Error(), "collecting every key") {
		t.Errorf("the finding names another step: %v", err)
	}
	t.Logf("the planted search: %v", err)
}
