//go:build js && wasm && sidecar_http

// The page Worker's search within §18.4's working set, in wasm under Node (SDK-M10-06; §22.4's T4, §18.4's targets row,
// D-SDK-25, FS-109's measure): the page Worker's search operations (sidecarworker), as the key-origin frame sends them,
// over a stand-in for the Sidecar's search calls that answers pages of hits as the engine does, measured on T4's page
// Worker profile (t4cell's MeasureSearch: the live heap after a forced collection at every pull and every reply, less
// its value at the search's start) and held to 8 MiB beside the version-table pages; and a deliberate per-search key
// collection, FS-109's 1,000,000 keys, fails the cell. test/sidecarwasm_budget.test.ts runs them.
//
// What this measures is the Worker's own share of a search, its page states, hit views and replies, on this leg. The
// engine's share in wasm, its frames, lists and masks over a real index, is not measured here: the search world the
// native cells run the engine on (SearchTestWorld) does not build on js/wasm, its device's state being on an OS
// backend, and no page Worker installs an engine yet (SDK-M10-04's draft).

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest/t4cell"

	"github.com/urnetwork/sdk/js/sidecarworker"
)

// FS-109's larger count, at which a page Worker that collects the buckets' keys per search passes its 8 MiB.
const testPageBudgetKeyCount = 1000000

// A Sidecar for a Worker that only searches: enrolled as nothing, and refusing to sign out.
type testPageBudgetSidecar struct{}

func (self *testPageBudgetSidecar) Identity() *sidecar.SidecarIdentity {
	return nil
}

func (self *testPageBudgetSidecar) Status() sidecar.SidecarStatus {
	return sidecar.SidecarStatus{}
}

func (self *testPageBudgetSidecar) Logout(ctx context.Context) error {
	return errors.New("a budget cell signs nothing out")
}

// A repo a budget cell never reads.
type testPageBudgetRepo struct{}

func (self *testPageBudgetRepo) List(ctx context.Context, args *sidecar.EvsReadArgs) (*sidecar.EvsListing, error) {
	return nil, errors.New("a budget cell reads no repo")
}

func (self *testPageBudgetRepo) ReadHead(ctx context.Context, args *sidecar.EvsReadArgs, out io.Writer) (*sidecar.EvsFileInfo, error) {
	return nil, errors.New("a budget cell reads no repo")
}

func (self *testPageBudgetRepo) History(ctx context.Context, args *sidecar.EvsReadArgs) (*sidecar.EvsHistory, error) {
	return nil, errors.New("a budget cell reads no repo")
}

// Records a budget cell never reads.
type testPageBudgetRecords struct{}

func (self *testPageBudgetRecords) Sync(ctx context.Context, logId sidecar.Id) error {
	return errors.New("a budget cell reads no record")
}

func (self *testPageBudgetRecords) Entries(logId sidecar.Id) []*sidecar.RecordReadEntry {
	return nil
}

// The Sidecar's search calls, standing in for the engine: each search answers pageCount pulls of hitCount hits, each
// with its display fields and a snippet, calling onPull at each; while planted, its first pull collects FS-109's
// 1,000,000 keys and holds them until the stream closes.
type testPageBudgetSearch struct {
	repoId    sidecar.Id
	pageCount int
	hitCount  int
	onPull    func()
	planted   bool
}

func (self *testPageBudgetSearch) Search(ctx context.Context, request *sidecar.SearchRequest) (*sidecar.SearchStream, error) {
	return sidecar.NewSearchStream(&testPageBudgetPulls{search: self, left: self.pageCount}), nil
}

func (self *testPageBudgetSearch) GetSearchStatus(repoId *sidecar.Id) (*sidecar.SearchStatus, error) {
	return nil, sidecar.ErrSidecarSearchUnavailable
}

func (self *testPageBudgetSearch) SetSearchPolicy(repoId *sidecar.Id, mode string) error {
	return nil
}

func (self *testPageBudgetSearch) SetSearchConditions(unmetered bool, charging bool, lowPower bool) {
}

// One search's pulls.
type testPageBudgetPulls struct {
	search *testPageBudgetSearch

	stateLock sync.Mutex
	left      int
	given     int
	keys      [][16]byte
}

func (self *testPageBudgetPulls) Next() ([]sidecar.SearchHit, sidecar.SearchProgress, bool, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.search.planted && self.keys == nil {
		// the bug FS-109 exists to catch: every bucket key collected per search
		self.keys = make([][16]byte, testPageBudgetKeyCount)
		for i := range self.keys {
			self.keys[i][0], self.keys[i][1], self.keys[i][2] = byte(i), byte(i>>8), byte(i>>16)
		}
	}
	if self.search.onPull != nil {
		self.search.onPull()
	}
	if self.left <= 0 {
		return nil, sidecar.SearchProgress{Searched: 1, Total: 1}, true, nil
	}
	self.left -= 1
	hits := make([]sidecar.SearchHit, 0, self.search.hitCount)
	at := time.Date(2027, time.May, 3, 10, 0, 0, 0, time.UTC)
	for range self.search.hitCount {
		self.given += 1
		hits = append(hits, sidecar.SearchHit{
			Repo: self.search.repoId, Path: "mail/inbox/" + strings.Repeat("m", 8) + string(rune('a'+self.given%26)), Version: uint64(self.given),
			Time: at.Add(-time.Duration(self.given) * time.Minute), State: sidecar.SearchConfirmed, Partition: sidecar.SearchPartitionLatest,
			Fields:  []sidecar.SearchField{{Field: 1, Text: "Harbor schedule " + strings.Repeat("x", 120)}, {Field: 2, Text: "Dana <dana@sdktest.example>"}},
			Snippet: "the harbor ferry " + strings.Repeat("y", 160),
		})
	}
	return hits, sidecar.SearchProgress{Searched: 1, Total: 1}, self.left == 0, nil
}

func (self *testPageBudgetPulls) Close() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.keys = nil
}

// A Worker that only searches, over search.
func testPageBudgetWorker(t *testing.T, search *testPageBudgetSearch) *sidecarworker.Worker {
	t.Helper()
	worker, err := sidecarworker.NewWorker(&sidecarworker.WorkerArgs{
		Sidecar: &testPageBudgetSidecar{},
		Repo: func(repoId sidecar.Id) (sidecarworker.WorkerRepo, error) {
			return &testPageBudgetRepo{}, nil
		},
		Records: func() (sidecarworker.WorkerRecords, error) {
			return &testPageBudgetRecords{}, nil
		},
		Search: func() (sidecarworker.WorkerSearch, error) {
			return search, nil
		},
		Settings: sidecarworker.DefaultWorkerSettings(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

// One page Worker search, from search to its last page through search_more, on cell as step held to budgetByteCount:
// a sample at every pull and every reply. The hits it gave, and its last reply.
func testPageBudgetSearchStep(t *testing.T, cell *t4cell.Cell, step string, budgetByteCount int64, search *testPageBudgetSearch) (int, *sidecarworker.Reply) {
	t.Helper()
	worker := testPageBudgetWorker(t, search)
	hitCount := 0
	var last *sidecarworker.Reply
	_, err := cell.MeasureSearch(step, budgetByteCount, func(sample func()) error {
		search.onPull = sample
		defer func() { search.onPull = nil }()
		request := `{"id":"s","op":"search","query":"harbor"}`
		for range search.pageCount + 2 {
			reply := &sidecarworker.Reply{}
			if err := json.Unmarshal(worker.HandleJson(context.Background(), []byte(request)), reply); err != nil {
				return err
			}
			sample()
			last = reply
			if !reply.Ok || reply.Search == nil {
				return errors.New("the search answered " + reply.Code)
			}
			hitCount += len(reply.Search.Hits)
			if reply.Search.Done {
				return nil
			}
			request = `{"id":"m","op":"search_more","search_id":"` + reply.Search.SearchId + `"}`
		}
		return errors.New("the search ran past its pages")
	})
	if err != nil {
		t.Fatal(err)
	}
	return hitCount, last
}

// A page Worker's search of ten pages of fifty hits stays within its 8 MiB working set beside the version-table
// pages, on T4's page Worker profile in wasm.
func TestPageWorkerSearchWithinItsWorkingSet(t *testing.T) {
	profile := t4cell.PageWorkerProfile()
	cell := t4cell.NewCell(profile)
	search := &testPageBudgetSearch{repoId: sidecar.NewId(), pageCount: 10, hitCount: 50}
	hitCount, last := testPageBudgetSearchStep(t, cell, "a page Worker search of ten pages", profile.SearchByteCount(), search)
	if hitCount != 500 || last == nil || !last.Search.Done {
		t.Errorf("the search gave %d hits, its last reply %+v", hitCount, last)
	}
	if err := cell.CheckWithin(); err != nil {
		t.Error(err)
	}
	t.Log("\n" + cell.Table())
}

// A deliberate per-search key collection fails the page Worker's cell: the same search with FS-109's 1,000,000 keys
// collected at its first pull and held to its end grows past 8 MiB, and CheckWithin names it.
func TestPageWorkerSearchBudgetCatchesAPerSearchKeyCollection(t *testing.T) {
	profile := t4cell.PageWorkerProfile()
	clean := t4cell.NewCell(profile)
	testPageBudgetSearchStep(t, clean, "a page Worker search", profile.SearchByteCount(), &testPageBudgetSearch{repoId: sidecar.NewId(), pageCount: 2, hitCount: 50})
	if err := clean.CheckWithin(); err != nil {
		t.Fatalf("CONTROL FAILED: the search without the plant: %v", err)
	}
	planted := t4cell.NewCell(profile)
	testPageBudgetSearchStep(t, planted, "a page Worker search collecting every key", profile.SearchByteCount(), &testPageBudgetSearch{
		repoId: sidecar.NewId(), pageCount: 2, hitCount: 50, planted: true,
	})
	err := planted.CheckWithin()
	if err == nil {
		t.Fatalf("a search holding %d collected keys stayed within %d B:\n%s", testPageBudgetKeyCount, profile.SearchByteCount(), planted.Table())
	}
	t.Logf("the planted search: %v", err)
}
