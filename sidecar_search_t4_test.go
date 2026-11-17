//go:build sidecar_testhooks && !ios_extension && !js

package sdk

// T4's phone search cells (SEARCH-M10-38; FS-77's, FS-109's and FS-110's T4 forms; §18.4's targets and caps, D-SDK-25,
// owner decision 100): on the ios32 and Android surrogate profiles, under each profile's Go memory limit, a phone's
// search over sidecar's search world (SearchTestWorld, the sidecar_testhooks seam FS-77, FS-109 and FS-110 run on), its
// state on disk as a phone keeps it, the heap sampled at every record a search opens and every page:
//
//	buckets   FS-77's search from the buckets, held to the phone's Search.MemoryByteCount, one search's 4 MiB working
//	          set beside the 256 KiB of version-table pages (§18.4)
//	masked    FS-109's: a bucket of 20,000 nonmatching updates and an all row a newer version masks, the masked row
//	          never shown, the search held to the same bound
//	more      FS-110's "search more" of every all shard on Wi-Fi, reported against the bound rather than held, since its
//	          download's staging runs inside the search (SEARCH-M10-38's report)
//
// Each bound is FS-109's measure: the live heap after a forced collection, at each record and page, less its value at
// the search's start. The cell's own rows, which sample without collecting and so count garbage, are reported beside.
//
// The cells run on this host under the profiles' limits, and a surrogate device's run of the same cells is external; no
// figure here is a device's (owner decision 100). The app's SidecarSearch is not between the cell and the engine: no
// host installs a search engine on the local backend yet (SDK-M10-01's draft), so the cells drive the core's engine.
// The page Worker's cell is sdk/js's, in wasm under Node, and not here.

import (
	"context"
	"fmt"
	"runtime"
	"runtime/metrics"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest/t4cell"
)

// A fake clock for the search world: moved by the world's reads and nothing else.
type testSearchCellClock struct {
	stateLock sync.Mutex
	now       time.Time
	timers    []*testSearchCellTimer
}

type testSearchCellTimer struct {
	at       time.Time
	function func()
	stopped  bool
}

func newTestSearchCellClock() *testSearchCellClock {
	return &testSearchCellClock{now: time.Date(2026, time.March, 4, 5, 6, 7, 0, time.UTC)}
}

func (self *testSearchCellClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

func (self *testSearchCellClock) After(timeout time.Duration) <-chan time.Time {
	fired := make(chan time.Time, 1)
	self.AfterFunc(timeout, func() { fired <- self.Now() })
	return fired
}

func (self *testSearchCellClock) AfterFunc(timeout time.Duration, function func()) func() bool {
	self.stateLock.Lock()
	timer := &testSearchCellTimer{at: self.now.Add(timeout), function: function}
	if timeout <= 0 {
		self.stateLock.Unlock()
		go function()
		return func() bool { return false }
	}
	self.timers = append(self.timers, timer)
	self.stateLock.Unlock()
	return func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		stopped := !timer.stopped
		timer.stopped = true
		return stopped
	}
}

func (self *testSearchCellClock) Advance(timeout time.Duration) {
	self.stateLock.Lock()
	self.now = self.now.Add(timeout)
	due := []*testSearchCellTimer{}
	kept := []*testSearchCellTimer{}
	for _, timer := range self.timers {
		if !timer.stopped && !timer.at.After(self.now) {
			timer.stopped = true
			due = append(due, timer)
		} else if !timer.stopped {
			kept = append(kept, timer)
		}
	}
	self.timers = kept
	self.stateLock.Unlock()
	for _, timer := range due {
		go timer.function()
	}
}

// A world of the cells' documents: three latest documents, two matching; an all partition of 2,000 filler and one
// archived match; and, with updates, a previous bucket of that many nonmatching updates and the all row's newer,
// nonmatching version in the current bucket.
func testSearchCellWorld(t *testing.T, clock *testSearchCellClock, updates int) *sidecar.SearchTestWorld {
	t.Helper()
	ctx := context.Background()
	world, err := sidecar.NewSearchTestWorld(&sidecar.SearchTestWorldArgs{
		RepoId: sidecar.NewId(), RefId: sidecar.NewId(), Clock: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	writer := sidecar.NewId()
	now := clock.Now()
	old := now.Add(-300 * 24 * time.Hour)
	latest := []*sidecar.SearchTestDocument{
		{Path: "notes/harbor.md", Version: 1, Title: "Harbor lights", Text: "the evening boat", Time: now},
		{Path: "notes/timetable.md", Version: 1, Title: "Timetable", Text: "the harbor ferry leaves at noon", Time: now},
		{Path: "notes/lunch.md", Version: 1, Title: "Lunch", Text: "soup", Time: now},
	}
	if 0 < updates {
		latest = append(latest, &sidecar.SearchTestDocument{Path: "archive/old-note.md", Version: 2, Title: "Moved", Text: "nothing of it now", Time: now})
	}
	if err := world.PushDocuments(ctx, writer, now, latest); err != nil {
		t.Fatal(err)
	}
	archived := []*sidecar.SearchTestDocument{{Path: "archive/old-note.md", Version: 1, Title: "Harbor archive", Text: "an old note", Time: old}}
	filler := &sidecar.SearchTestBulk{Prefix: "archive/filler-", Count: 2000, Version: 1, Document: &sidecar.SearchTestDocument{
		Path: "archive/filler-template", Version: 1, Text: "5150 5151", Time: old,
	}}
	partition := &sidecar.SearchTestPartition{All: true, RolledThroughDay: sidecar.SearchTestBucketStartDay(now) - 60, ShardDocumentCount: 512}
	if err := world.WriteShards(ctx, writer, old, partition, archived, []*sidecar.SearchTestBulk{filler}); err != nil {
		t.Fatal(err)
	}
	if 0 < updates {
		previous := now.Add(-30 * 24 * time.Hour)
		bulk := &sidecar.SearchTestBulk{Prefix: "notes/update-", Count: updates, Version: 1, Document: &sidecar.SearchTestDocument{
			Path: "notes/update-template", Version: 1, Text: "1000 2000 3000", Time: previous,
		}}
		bucket := &sidecar.SearchTestPartition{StartDay: sidecar.SearchTestBucketStartDay(previous), ShardDocumentCount: 4096}
		if err := world.WriteShards(ctx, writer, previous, bucket, nil, []*sidecar.SearchTestBulk{bulk}); err != nil {
			t.Fatal(err)
		}
	}
	return world
}

// A phone's search settings, as sdk's phone host class sets them over the core's.
func testSearchCellSettings() *sidecar.SidecarSearchSettings {
	settings := sidecar.DefaultSidecarSettings().Search
	search := defaultSidecarHostClassSettings().Ios.search
	settings.AllPolicy = search.AllPolicy
	settings.MemoryByteCount = search.SearchMemoryByteCount
	settings.DaemonSearchCount = search.SearchCount
	settings.AllCacheByteCount = search.AllCacheMaxByteCount
	settings.UnusedAllAge = search.UnusedAllAge
	settings.DirectoryBuckets = search.DirectoryBuckets
	settings.SyncRoundCount = search.SyncRoundCount
	settings.RemoteServe = false
	return settings
}

// The live heap after a forced collection, as FS-109 samples it.
func testSearchCellLiveHeap() int64 {
	runtime.GC()
	samples := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(samples)
	return int64(samples[0].Value.Uint64())
}

// FS-109's sampler: the most the live heap grew over its start, at each sample.
type testSearchCellSampler struct {
	stateLock sync.Mutex
	active    bool
	start     int64
	peak      int64
	count     int
}

func (self *testSearchCellSampler) begin() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.active, self.start, self.peak, self.count = true, testSearchCellLiveHeap(), 0, 0
}

func (self *testSearchCellSampler) sample() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !self.active {
		return
	}
	self.count += 1
	self.peak = max(self.peak, testSearchCellLiveHeap()-self.start)
}

// Ends the sampling: the peak growth and how many samples were taken.
func (self *testSearchCellSampler) end() (int64, int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.active = false
	return self.peak, self.count
}

// The cells on one profile.
func testSearchCells(t *testing.T, profile *t4cell.Profile) {
	ctx := context.Background()
	cell := t4cell.NewCell(profile)
	settings := testSearchCellSettings()
	budget := settings.MemoryByteCount
	sampler := &testSearchCellSampler{}
	for _, updates := range []int{0, 20000} {
		clock := newTestSearchCellClock()
		world := testSearchCellWorld(t, clock, updates)
		conditions := sidecar.SearchConditions{Reported: true, Foreground: true}
		device, err := world.NewDevice(ctx, &sidecar.SearchTestDeviceArgs{
			Settings: settings, Conditions: func() sidecar.SearchConditions { return conditions }, StateDirectory: t.TempDir(),
			ReadByteCount: 32 * 1024 * 1024, OnRecord: func() {
				cell.Sample()
				sampler.sample()
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := device.Sync(ctx); err != nil {
			device.Close()
			t.Fatal(err)
		}
		step := "search from the buckets"
		if 0 < updates {
			step = fmt.Sprintf("search with a masked all row beside %d updates", updates)
		}
		var hits []sidecar.SearchHit
		sampler.begin()
		if _, err := cell.Measure(step+", reported", 0, t4cell.HoldsPeak, func() (any, error) {
			search, err := device.Search(ctx, &sidecar.SearchRequest{Text: "harbor", Scope: sidecar.SearchCached, Remote: "off"}, 64, func(page *sidecar.SearchTestPage) {
				cell.Sample()
				sampler.sample()
			})
			if err != nil {
				return nil, err
			}
			hits = search.Hits()
			return nil, nil
		}); err != nil {
			device.Close()
			t.Fatal(err)
		}
		if growth, count := sampler.end(); count == 0 || budget < growth {
			t.Errorf("%s %s: the live heap grew %d B over %d samples, past %d B", profile.Name, step, growth, count, budget)
		} else {
			t.Logf("%s %s: the live heap grew at most %d B over %d samples, within %d B", profile.Name, step, growth, count, budget)
		}
		for _, hit := range hits {
			if strings.HasPrefix(hit.Path, "archive/") {
				t.Errorf("%s: %s showed, which no bucket search finds or whose newer version masks it", step, hit.Path)
			}
		}
		if len(hits) != 2 {
			t.Errorf("%s: %d hits, want the two latest matches", step, len(hits))
		}
		if updates == 0 {
			conditions = sidecar.SearchConditions{Reported: true, Foreground: true, Unmetered: true}
			if _, err := cell.Measure("search more of every all shard, reported", 0, t4cell.HoldsPeak, func() (any, error) {
				search, err := device.Search(ctx, &sidecar.SearchRequest{Text: "harbor", Scope: sidecar.SearchAll, Remote: "off"}, 256, func(page *sidecar.SearchTestPage) {
					cell.Sample()
				})
				if err != nil {
					return nil, err
				}
				if got := len(search.Hits()); got != 3 {
					return nil, fmt.Errorf("search more found %d hits, want the latest two and the archived one", got)
				}
				return nil, nil
			}); err != nil {
				device.Close()
				t.Fatal(err)
			}
		}
		device.Close()
	}
	if err := cell.CheckWithin(); err != nil {
		t.Error(err)
	}
	t.Log("\n" + cell.Table())
}

// The ios32 surrogate's phone search cells (FS-77, FS-109, FS-110 on T4).
func TestIos32CellSearchesWithinItsWorkingSet(t *testing.T) {
	testSearchCells(t, t4cell.Ios32Profile())
}

// The Android surrogate's, whose search matches iOS's (owner decision 197).
func TestAndroidCellSearchesWithinItsWorkingSet(t *testing.T) {
	testSearchCells(t, t4cell.AndroidProfile())
}
