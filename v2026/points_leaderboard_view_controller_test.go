//go:build !ios_extension

package sdk

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pointsLeaderboardTestServer serves a three-page leaderboard for "points"
// and a one-page leaderboard for "blocks", remembers every request and can
// answer a cursor with `restart`.
type pointsLeaderboardTestServer struct {
	lock                  sync.Mutex
	requests              []GetPointsLeaderboardArgs
	restart               map[string]bool
	fail                  bool
	epochMetricsAvailable bool
}

func (self *pointsLeaderboardTestServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/stats/points-leaderboard" {
			http.NotFound(w, r)
			return
		}
		var args GetPointsLeaderboardArgs
		if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		self.lock.Lock()
		self.requests = append(self.requests, args)
		fail := self.fail
		restart := self.restart[args.Cursor]
		epochMetricsAvailable := self.epochMetricsAvailable
		self.lock.Unlock()

		if fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		if restart {
			json.NewEncoder(w).Encode(map[string]any{"rows": []any{}, "restart": true, "total_ranked": 3})
			return
		}

		row := func(i int, name string, anonymous bool) map[string]any {
			return map[string]any{
				"network_id":         fmt.Sprintf("00000000-0000-0000-0000-%012d", i),
				"network_name":       name,
				"emoji_tag":          "🐬",
				"anonymous":          anonymous,
				"total_points":       float64(1000-i) * 1234.6,
				"blocks_with_points": 10 - i,
				"streak":             5 - i,
				"longest_streak":     9,
				"rank_points":        i,
				"rank_blocks":        i + 1,
				"rank_streak":        i + 2,
			}
		}
		result := map[string]any{
			"total_ranked":            3,
			"latest_epoch":            57,
			"snapshot_time":           "2026-09-03T00:00:00Z",
			"epoch_metrics_available": epochMetricsAvailable,
		}
		switch {
		case args.Sort == PointsLeaderboardSortPoints && args.Cursor == "":
			result["rows"] = []any{row(1, "alpha", false), row(2, "", true)}
			result["next_cursor"] = "c1"
			result["me"] = map[string]any{
				"network_id": "00000000-0000-0000-0000-000000000099", "network_name": "me", "anonymous": false,
				"total_points": 42.4, "rank_points": 37, "points_leaderboard_public": true,
			}
		case args.Sort == PointsLeaderboardSortPoints && args.Cursor == "c1":
			result["rows"] = []any{row(3, "gamma", false)}
			result["next_cursor"] = "c2"
		case args.Sort == PointsLeaderboardSortPoints && args.Cursor == "c2":
			result["rows"] = []any{}
		case args.Sort == PointsLeaderboardSortBlocks:
			result["rows"] = []any{row(7, "blocks-first", false)}
		case args.Sort == PointsLeaderboardSortStreak:
			// deliberately out of order: the view controller must sort them
			result["rows"] = []any{row(3, "three", false), row(1, "one", false), row(2, "two", false)}
		default:
			result["rows"] = []any{}
		}
		json.NewEncoder(w).Encode(result)
	})
}

func (self *pointsLeaderboardTestServer) requestCount() int {
	self.lock.Lock()
	defer self.lock.Unlock()
	return len(self.requests)
}

type pointsLeaderboardTestListener struct {
	changed chan struct{}
}

func (self *pointsLeaderboardTestListener) PointsLeaderboardChanged() {
	select {
	case self.changed <- struct{}{}:
	default:
	}
}

// waitFor polls the condition through the listener's change signal.
func waitForPointsLeaderboard(t *testing.T, listener *pointsLeaderboardTestListener, condition func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for !condition() {
		select {
		case <-listener.changed:
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			t.Fatal("timeout waiting for the leaderboard state")
		}
	}
}

func newPointsLeaderboardTest(t *testing.T) (*pointsLeaderboardTestServer, *PointsLeaderboardViewController, *pointsLeaderboardTestListener) {
	t.Helper()
	server := &pointsLeaderboardTestServer{restart: map[string]bool{}, epochMetricsAvailable: true}
	ctx, api := newTestApi(t, server.handler())
	vc := NewPointsLeaderboardViewControllerWithApi(ctx, api)
	t.Cleanup(vc.Close)
	listener := &pointsLeaderboardTestListener{changed: make(chan struct{}, 1)}
	sub := vc.AddPointsLeaderboardListener(listener)
	t.Cleanup(sub.Close)
	return server, vc, listener
}

func TestPointsLeaderboardPagesToTheEnd(t *testing.T) {
	server, vc, listener := newPointsLeaderboardTest(t)

	if vc.GetSort() != PointsLeaderboardSortPoints {
		t.Fatalf("default sort = %q", vc.GetSort())
	}
	vc.Start()
	waitForPointsLeaderboard(t, listener, func() bool { return vc.GetRowCount() == 2 && !vc.IsLoading() })

	rows := vc.GetRows()
	first := rows.Get(0)
	if first.DisplayName != "alpha" || first.Anonymous {
		t.Fatalf("first row display = %q anonymous=%v", first.DisplayName, first.Anonymous)
	}
	if first.TotalPointsText != "1,233,365" || first.RankPointsText != "#1" || first.RankStreakText != "#3" {
		t.Fatalf("formatted = %q %q %q", first.TotalPointsText, first.RankPointsText, first.RankStreakText)
	}
	second := rows.Get(1)
	if !second.Anonymous || second.DisplayName != "" || second.EmojiTag != "🐬" {
		t.Fatalf("anonymous row = %+v", second)
	}
	me := vc.GetMe()
	if me == nil || me.Row == nil || !me.PointsLeaderboardPublic || me.Row.RankPointsText != "#37" || me.Row.DisplayName != "me" {
		t.Fatalf("me = %+v", me)
	}
	if vc.GetTotalRanked() != 3 || vc.GetLatestEpoch() != 57 || vc.GetSnapshotTime() == nil {
		t.Fatalf("meta = %d %d %v", vc.GetTotalRanked(), vc.GetLatestEpoch(), vc.GetSnapshotTime())
	}
	if vc.IsEndReached() {
		t.Fatal("end reached after the first page")
	}

	// second page appends
	vc.LoadMore()
	waitForPointsLeaderboard(t, listener, func() bool { return vc.GetRowCount() == 3 && !vc.IsLoading() })
	if vc.GetRows().Get(2).DisplayName != "gamma" {
		t.Fatal("second page not appended in order")
	}
	// me is kept across pages that carry none
	if vc.GetMe() == nil {
		t.Fatal("me dropped on a later page")
	}

	// the empty last page ends the list; further LoadMore calls do nothing
	vc.LoadMore()
	waitForPointsLeaderboard(t, listener, func() bool { return vc.IsEndReached() && !vc.IsLoading() })
	before := server.requestCount()
	vc.LoadMore()
	vc.LoadMore()
	time.Sleep(50 * time.Millisecond)
	if server.requestCount() != before {
		t.Fatalf("LoadMore after the end made %d requests", server.requestCount()-before)
	}
	if vc.GetRowCount() != 3 {
		t.Fatalf("rows = %d", vc.GetRowCount())
	}
}

func TestPointsLeaderboardSetSortClearsAndReloads(t *testing.T) {
	server, vc, listener := newPointsLeaderboardTest(t)
	vc.Start()
	waitForPointsLeaderboard(t, listener, func() bool { return vc.GetRowCount() == 2 && !vc.IsLoading() })

	// an unknown sort and the current sort are no-ops
	before := server.requestCount()
	vc.SetSort("bogus")
	vc.SetSort(PointsLeaderboardSortPoints)
	time.Sleep(30 * time.Millisecond)
	if server.requestCount() != before || vc.GetRowCount() != 2 {
		t.Fatal("no-op sorts changed the state")
	}

	vc.SetSort(PointsLeaderboardSortBlocks)
	if vc.GetSort() != PointsLeaderboardSortBlocks {
		t.Fatal("sort not switched")
	}
	waitForPointsLeaderboard(t, listener, func() bool {
		return vc.GetRowCount() == 1 && !vc.IsLoading() && vc.GetSort() == PointsLeaderboardSortBlocks
	})
	if vc.GetRows().Get(0).DisplayName != "blocks-first" || !vc.IsEndReached() {
		t.Fatalf("blocks sort rows = %v end=%v", vc.GetRows().Get(0).DisplayName, vc.IsEndReached())
	}
}

func TestPointsLeaderboardRestartReloadsFromTheTop(t *testing.T) {
	server, vc, listener := newPointsLeaderboardTest(t)
	server.lock.Lock()
	server.restart["c1"] = true
	server.lock.Unlock()

	vc.Start()
	waitForPointsLeaderboard(t, listener, func() bool { return vc.GetRowCount() == 2 && !vc.IsLoading() })
	// the cursor's snapshot is gone: the controller reloads the first page
	// (replacing, not appending) and pages on from there
	vc.LoadMore()
	waitForPointsLeaderboard(t, listener, func() bool {
		return !vc.IsLoading() && server.requestCount() >= 3
	})
	if vc.GetRowCount() != 2 {
		t.Fatalf("rows after restart = %d", vc.GetRowCount())
	}
	server.lock.Lock()
	last := server.requests[len(server.requests)-1]
	server.lock.Unlock()
	if last.Cursor != "" {
		t.Fatalf("restart did not reload from the top: cursor %q", last.Cursor)
	}
	if vc.IsEndReached() {
		t.Fatal("restart ended the list")
	}
}

func TestPointsLeaderboardErrorThenRetry(t *testing.T) {
	server, vc, listener := newPointsLeaderboardTest(t)
	server.lock.Lock()
	server.fail = true
	server.lock.Unlock()

	vc.Start()
	waitForPointsLeaderboard(t, listener, func() bool { return !vc.IsLoading() && vc.GetErrorMessage() != "" })
	if vc.GetRowCount() != 0 || vc.IsEndReached() {
		t.Fatal("error page changed rows or ended the list")
	}

	server.lock.Lock()
	server.fail = false
	server.lock.Unlock()
	// LoadMore retries the same (first) page
	vc.LoadMore()
	waitForPointsLeaderboard(t, listener, func() bool { return vc.GetRowCount() == 2 && !vc.IsLoading() })
	if vc.GetErrorMessage() != "" {
		t.Fatal("error not cleared")
	}
}

func TestPointsLeaderboardRefreshReplaces(t *testing.T) {
	_, vc, listener := newPointsLeaderboardTest(t)
	vc.Start()
	waitForPointsLeaderboard(t, listener, func() bool { return vc.GetRowCount() == 2 && !vc.IsLoading() })
	vc.LoadMore()
	waitForPointsLeaderboard(t, listener, func() bool { return vc.GetRowCount() == 3 && !vc.IsLoading() })

	vc.Refresh()
	if vc.GetRowCount() != 3 {
		t.Fatal("refresh cleared the rows before the new page landed")
	}
	waitForPointsLeaderboard(t, listener, func() bool { return vc.GetRowCount() == 2 && !vc.IsLoading() })
	if vc.IsEndReached() {
		t.Fatal("refresh ended the list")
	}
}

func TestPointsLeaderboardMeJson(t *testing.T) {
	me := &PointsLeaderboardMe{}
	if err := json.Unmarshal([]byte(`{"network_id":"00000000-0000-0000-0000-000000000001","anonymous":true,"total_points":5,"points_leaderboard_public":true}`), me); err != nil {
		t.Fatal(err)
	}
	if me.Row == nil || !me.Row.Anonymous || me.Row.TotalPoints != 5 || !me.PointsLeaderboardPublic {
		t.Fatalf("me = %+v", me)
	}
	out, err := json.Marshal(me)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if back["points_leaderboard_public"] != true || back["anonymous"] != true {
		t.Fatalf("round trip = %s", out)
	}
}

func TestFormatPointsAndRank(t *testing.T) {
	cases := map[float64]string{0: "0", 999: "999", 1000: "1,000", 152829.4: "152,829", 1234567.6: "1,234,568", -1500: "-1,500"}
	for in, want := range cases {
		if got := FormatPoints(in); got != want {
			t.Fatalf("FormatPoints(%v) = %q, want %q", in, got, want)
		}
	}
	if FormatRank(0) != "-" || FormatRank(37) != "#37" {
		t.Fatal("FormatRank")
	}
}

func TestValidateEmojiTag(t *testing.T) {
	ok := ValidateEmojiTag(" 🐬🔥 ")
	if !ok.Ok || ok.Count != 2 || ok.Normalized != "🐬🔥" || ok.Reason != "" || ok.Message != "" {
		t.Fatalf("ok = %+v", ok)
	}
	promoted := ValidateEmojiTag("☺")
	if !promoted.Ok || promoted.Normalized != "☺️" {
		t.Fatalf("promoted = %+v", promoted)
	}
	for in, reason := range map[string]string{"": EmojiTagReasonEmpty, "🐬🐬🐬🐬🐬🐬🐬": EmojiTagReasonTooMany, "gg🐬": EmojiTagReasonNotEmoji, "1": EmojiTagReasonNotEmoji} {
		v := ValidateEmojiTag(in)
		if v.Ok || v.Reason != reason || v.Message == "" || v.Normalized != "" {
			t.Fatalf("ValidateEmojiTag(%q) = %+v, want reason %s", in, v, reason)
		}
	}
	if EmojiTagMaxCount != 6 {
		t.Fatal("cap")
	}
}

func TestSuggestEmojiTag(t *testing.T) {
	if EmojiTagSuggestMaxCount != 3 {
		t.Fatal("suggest cap")
	}
	lengths := map[int]bool{}
	for i := 0; i < 300; i++ {
		count := i % 5 // 0..4
		tag := SuggestEmojiTag(count)
		v := ValidateEmojiTag(tag)
		if !v.Ok || v.Normalized != tag {
			t.Fatalf("SuggestEmojiTag(%d) = %q: %+v", count, tag, v)
		}
		switch {
		case count == 0:
			if v.Count < 1 || EmojiTagSuggestMaxCount < v.Count {
				t.Fatalf("SuggestEmojiTag(0) = %q has %d emoji", tag, v.Count)
			}
			lengths[v.Count] = true
		case EmojiTagSuggestMaxCount < count:
			if v.Count != EmojiTagSuggestMaxCount {
				t.Fatalf("SuggestEmojiTag(%d) = %q has %d emoji", count, tag, v.Count)
			}
		default:
			if v.Count != count {
				t.Fatalf("SuggestEmojiTag(%d) = %q has %d emoji", count, tag, v.Count)
			}
		}
	}
	if len(lengths) != EmojiTagSuggestMaxCount {
		t.Fatalf("random lengths = %v", lengths)
	}
}

// The ordering rule, key by key: each sort's own dimension first, then its
// two tie-breaks, every key descending, then the network id ascending.
func TestPointsLeaderboardOrder(t *testing.T) {
	key := func(points int64, blocks int64, streak int64, id string) *PointsLeaderboardKey {
		return &PointsLeaderboardKey{NanoPoints: points * PointsLeaderboardNanoPointsPerPoint, Blocks: blocks, Streak: streak, NetworkId: id}
	}
	cases := []struct {
		name   string
		sort   string
		ahead  *PointsLeaderboardKey
		behind *PointsLeaderboardKey
	}{
		{"points: more points first", PointsLeaderboardSortPoints, key(10, 1, 1, "b"), key(9, 9, 9, "a")},
		{"points: tie -> streak", PointsLeaderboardSortPoints, key(10, 1, 3, "b"), key(10, 9, 2, "a")},
		{"points: tie, tie -> blocks", PointsLeaderboardSortPoints, key(10, 4, 3, "b"), key(10, 3, 3, "a")},
		{"points: all tie -> id asc", PointsLeaderboardSortPoints, key(10, 4, 3, "a"), key(10, 4, 3, "b")},
		{"blocks: more blocks first", PointsLeaderboardSortBlocks, key(1, 5, 1, "b"), key(99, 4, 9, "a")},
		{"blocks: tie -> streak", PointsLeaderboardSortBlocks, key(1, 5, 2, "b"), key(99, 5, 1, "a")},
		{"blocks: tie, tie -> points", PointsLeaderboardSortBlocks, key(2, 5, 2, "b"), key(1, 5, 2, "a")},
		{"streak: longer streak first", PointsLeaderboardSortStreak, key(1, 1, 7, "b"), key(99, 9, 6, "a")},
		{"streak: tie -> blocks", PointsLeaderboardSortStreak, key(1, 3, 7, "b"), key(99, 2, 7, "a")},
		{"streak: tie, tie -> points", PointsLeaderboardSortStreak, key(2, 3, 7, "b"), key(1, 3, 7, "a")},
		{"unknown sort orders as points", "bogus", key(10, 1, 1, "b"), key(9, 9, 9, "a")},
	}
	for _, c := range cases {
		if ComparePointsLeaderboardKeys(c.sort, c.ahead, c.behind) >= 0 {
			t.Errorf("%s: expected ahead < behind", c.name)
		}
		if ComparePointsLeaderboardKeys(c.sort, c.behind, c.ahead) <= 0 {
			t.Errorf("%s: expected behind > ahead", c.name)
		}
	}
	// the values compare equal only when all three tie; the key order still separates the networks
	a, b := key(10, 4, 3, "a"), key(10, 4, 3, "b")
	if ComparePointsLeaderboardValues(PointsLeaderboardSortPoints, a, b) != 0 {
		t.Error("equal values must compare 0")
	}
	if ComparePointsLeaderboardKeys(PointsLeaderboardSortPoints, a, a) != 0 {
		t.Error("a key must compare 0 with itself")
	}
	// the dimensions per sort
	for sortBy, want := range map[string][3]string{
		PointsLeaderboardSortPoints: {PointsLeaderboardSortPoints, PointsLeaderboardSortStreak, PointsLeaderboardSortBlocks},
		PointsLeaderboardSortBlocks: {PointsLeaderboardSortBlocks, PointsLeaderboardSortStreak, PointsLeaderboardSortPoints},
		PointsLeaderboardSortStreak: {PointsLeaderboardSortStreak, PointsLeaderboardSortBlocks, PointsLeaderboardSortPoints},
	} {
		first, second, third := pointsLeaderboardDimensions(sortBy)
		if [3]string{first, second, third} != want {
			t.Errorf("%s: dimensions %v %v %v", sortBy, first, second, third)
		}
	}
	// a row's key recovers the exact nano points from the wire's float
	row := &PointsLeaderboardRow{TotalPoints: 1234.567891, BlocksWithPoints: 2, Streak: 1}
	if k := PointsLeaderboardKeyOf(row); k.NanoPoints != 1234567891 || k.Blocks != 2 || k.Streak != 1 {
		t.Errorf("key of row: %+v", k)
	}
}

// A page that arrives out of the sort's order is put in order: the rows the
// app renders are always the comparator's sequence.
func TestPointsLeaderboardPagesAreOrdered(t *testing.T) {
	_, vc, listener := newPointsLeaderboardTest(t)
	vc.Start()
	waitForPointsLeaderboard(t, listener, func() bool { return vc.GetRowCount() == 2 && !vc.IsLoading() })
	// the streak page is served as rows 3, 1, 2 (streaks 2, 4, 3): expect 1, 2, 3
	vc.SetSort(PointsLeaderboardSortStreak)
	waitForPointsLeaderboard(t, listener, func() bool {
		return vc.GetSort() == PointsLeaderboardSortStreak && vc.GetRowCount() == 3 && !vc.IsLoading()
	})
	rows := vc.GetRows()
	got := []int64{}
	for i := 0; i < rows.Len(); i++ {
		got = append(got, rows.Get(i).Streak)
	}
	if fmt.Sprint(got) != "[4 3 2]" {
		t.Fatalf("rows out of order: streaks %v", got)
	}
}

// Two LoadMore calls racing for the same page: the first claims the in-flight
// slot in the scope that checked it, so the second, arriving while the first
// is between its claim and its request, is refused before it ever reaches
// the request. Before the claim moved into that scope both passed and the
// page was requested and appended twice.
func TestPointsLeaderboardLoadMoreClaimsTheSlotOnce(t *testing.T) {
	server, vc, listener := newPointsLeaderboardTest(t)
	vc.Start()
	waitForPointsLeaderboard(t, listener, func() bool { return !vc.IsLoading() && vc.GetRowCount() == 2 })
	firstPageRequests := server.requestCount()

	// the barrier counts every LoadMore that got past the check and parks
	// only the first one in the gap
	var passed int32
	inGap := make(chan struct{})
	release := make(chan struct{})
	pointsLeaderboardTestBeforeFetch = func() {
		if atomic.AddInt32(&passed, 1) == 1 {
			close(inGap)
			<-release
		}
	}
	defer func() { pointsLeaderboardTestBeforeFetch = nil }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		vc.LoadMore()
	}()
	<-inGap
	// lands while the first holds the slot but has not sent
	vc.LoadMore()
	if got := atomic.LoadInt32(&passed); got != 1 {
		t.Fatalf("a second LoadMore passed the in-flight check: %d fetches", got)
	}
	close(release)
	<-done

	waitForPointsLeaderboard(t, listener, func() bool { return !vc.IsLoading() && vc.GetRowCount() == 3 })
	if got := server.requestCount() - firstPageRequests; got != 1 {
		t.Fatalf("expected one request for the second page, got %d", got)
	}
}

// The caller's own name is shown on `me` (the own-stats card) even while the
// network is anonymous: the server sends `network_name` on `me` regardless of
// the switch. A list row without a name stays anonymous, the caller's own
// list row included.
func TestPointsLeaderboardOwnNameShownWhenAnonymous(t *testing.T) {
	me := &PointsLeaderboardRow{NetworkName: "wickymicky", Anonymous: true, EmojiTag: "🦓"}
	formatPointsLeaderboardRow(me, true)
	if me.DisplayName != "wickymicky" {
		t.Fatalf("own row display name = %q, want the name the server sent", me.DisplayName)
	}
	other := &PointsLeaderboardRow{Anonymous: true, EmojiTag: "🔥"}
	formatPointsLeaderboardRow(other, true)
	if other.DisplayName != "" {
		t.Fatalf("anonymous row without a name must have no display name, got %q", other.DisplayName)
	}
}

// Missing finalized epoch history must not look like a legitimate zero. The
// controller preserves total points/rank, masks every epoch-derived display,
// and refuses block/streak sort changes until a later available snapshot.
func TestPointsLeaderboardUnavailableEpochMetrics(t *testing.T) {
	server, vc, listener := newPointsLeaderboardTest(t)
	server.lock.Lock()
	server.epochMetricsAvailable = false
	server.lock.Unlock()

	vc.Start()
	waitForPointsLeaderboard(t, listener, func() bool { return vc.GetRowCount() == 2 && !vc.IsLoading() })
	if vc.GetEpochMetricsAvailable() {
		t.Fatal("missing finalized epochs reported as available")
	}
	row := vc.GetRows().Get(0)
	if row.TotalPointsText == "-" || row.RankPointsText == "-" {
		t.Fatalf("total points were masked: %+v", row)
	}
	if row.BlocksWithPointsText != "-" || row.StreakText != "-" || row.LongestStreakText != "-" || row.RankBlocksText != "-" || row.RankStreakText != "-" {
		t.Fatalf("epoch-derived values were presented as measured: %+v", row)
	}
	before := server.requestCount()
	vc.SetSort(PointsLeaderboardSortBlocks)
	vc.SetSort(PointsLeaderboardSortStreak)
	if vc.GetSort() != PointsLeaderboardSortPoints || server.requestCount() != before {
		t.Fatal("unavailable epoch sort changed state or issued a request")
	}

	server.lock.Lock()
	server.epochMetricsAvailable = true
	server.lock.Unlock()
	vc.Refresh()
	waitForPointsLeaderboard(t, listener, func() bool { return vc.GetEpochMetricsAvailable() && !vc.IsLoading() })
	vc.SetSort(PointsLeaderboardSortBlocks)
	waitForPointsLeaderboard(t, listener, func() bool {
		return vc.GetSort() == PointsLeaderboardSortBlocks && !vc.IsLoading()
	})
	if vc.GetRows().Get(0).BlocksWithPointsText == "-" {
		t.Fatal("legitimate epoch values stayed masked after recovery")
	}
}

// pointsLeaderboardWindowServer serves a leaderboard of `total` networks at
// positions 1..total with the server's seek and two-way cursor rules
// (server/controller/points_leaderboard_controller.go pointsLeaderboardPager):
// cursors are "f:<position>" (the page after) and "b:<position>" (the page
// before). It remembers every request.
type pointsLeaderboardWindowServer struct {
	lock     sync.Mutex
	total    int64
	requests []GetPointsLeaderboardArgs
	// answer this cursor with `restart` once
	restartOnce string
}

func (self *pointsLeaderboardWindowServer) handler() http.Handler {
	row := func(pos int64) map[string]any {
		return map[string]any{
			"network_id":         fmt.Sprintf("00000000-0000-0000-0000-%012d", pos),
			"network_name":       fmt.Sprintf("net-%d", pos),
			"emoji_tag":          "🐬",
			"anonymous":          false,
			"total_points":       float64(self.total-pos+1) * 100,
			"blocks_with_points": 1,
			"streak":             1,
			"longest_streak":     1,
			"rank_points":        pos,
			"rank_blocks":        pos,
			"rank_streak":        pos,
			"position":           pos,
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var args GetPointsLeaderboardArgs
		if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		self.lock.Lock()
		self.requests = append(self.requests, args)
		restart := self.restartOnce != "" && self.restartOnce == args.Cursor
		if restart {
			self.restartOnce = ""
		}
		total := self.total
		self.lock.Unlock()

		result := map[string]any{"total_ranked": total, "latest_epoch": 3, "epoch_metrics_available": true}
		if restart {
			result["rows"] = []any{}
			result["restart"] = true
			json.NewEncoder(w).Encode(result)
			return
		}
		limit := int64(args.Limit)
		rows := []any{}
		var first, last int64
		switch {
		case strings.HasPrefix(args.Cursor, "b:"):
			var before int64
			fmt.Sscanf(args.Cursor, "b:%d", &before)
			first = before - limit
			if first < 1 {
				first = 1
			}
			last = before - 1
		default:
			var after int64
			if strings.HasPrefix(args.Cursor, "f:") {
				fmt.Sscanf(args.Cursor, "f:%d", &after)
			} else if args.SeekRank > 0 {
				seek := args.SeekRank
				if seek > total {
					seek = total
				}
				after = seek - 1
			}
			first = after + 1
			last = after + limit
			if last > total {
				last = total
			}
		}
		for pos := first; pos <= last; pos++ {
			rows = append(rows, row(pos))
		}
		result["rows"] = rows
		if len(rows) > 0 {
			if first > 1 {
				result["prev_cursor"] = fmt.Sprintf("b:%d", first)
			}
			if last < total {
				result["next_cursor"] = fmt.Sprintf("f:%d", last)
			}
		}
		json.NewEncoder(w).Encode(result)
	})
}

func newPointsLeaderboardWindowTest(t *testing.T, total int64) (*pointsLeaderboardWindowServer, *PointsLeaderboardViewController, *pointsLeaderboardTestListener) {
	t.Helper()
	server := &pointsLeaderboardWindowServer{total: total}
	ctx, api := newTestApi(t, server.handler())
	vc := NewPointsLeaderboardViewControllerWithApi(ctx, api)
	t.Cleanup(vc.Close)
	listener := &pointsLeaderboardTestListener{changed: make(chan struct{}, 1)}
	sub := vc.AddPointsLeaderboardListener(listener)
	t.Cleanup(sub.Close)
	return server, vc, listener
}

func pointsLeaderboardPositions(vc *PointsLeaderboardViewController) []int64 {
	out := []int64{}
	for _, row := range vc.GetRows().getAll() {
		out = append(out, row.Position)
	}
	return out
}

func assertPointsLeaderboardWindow(t *testing.T, vc *PointsLeaderboardViewController, first int64, last int64) {
	t.Helper()
	positions := pointsLeaderboardPositions(vc)
	want := []int64{}
	for pos := first; pos <= last; pos++ {
		want = append(want, pos)
	}
	if len(positions) != len(want) {
		t.Fatalf("window %v, want %d..%d", positions, first, last)
	}
	for i := range want {
		if positions[i] != want[i] {
			t.Fatalf("window %v, want %d..%d", positions, first, last)
		}
	}
	if vc.FirstLoadedPosition() != first || vc.LastLoadedPosition() != last {
		t.Fatalf("loaded positions %d..%d, want %d..%d", vc.FirstLoadedPosition(), vc.LastLoadedPosition(), first, last)
	}
}

func waitForPointsLeaderboardIdle(t *testing.T, vc *PointsLeaderboardViewController, listener *pointsLeaderboardTestListener) {
	t.Helper()
	waitForPointsLeaderboard(t, listener, func() bool { return !vc.IsLoading() && vc.GetRowCount() > 0 })
}

func TestPointsLeaderboardSeekThenPageBothWays(t *testing.T) {
	_, vc, listener := newPointsLeaderboardWindowTest(t, 137)

	vc.SeekToRank(80)
	waitForPointsLeaderboardIdle(t, vc, listener)
	assertPointsLeaderboardWindow(t, vc, 80, 129)
	if !vc.HasMoreBefore() || !vc.HasMoreAfter() || vc.IsEndReached() {
		t.Fatal("a mid-list window has ranks on both sides")
	}
	if vc.TotalRanked() != 137 {
		t.Fatalf("total ranked %d", vc.TotalRanked())
	}

	vc.LoadMoreBefore()
	waitForPointsLeaderboard(t, listener, func() bool { return !vc.IsLoading() && vc.GetRowCount() > 50 })
	assertPointsLeaderboardWindow(t, vc, 30, 129)
	if !vc.HasMoreBefore() {
		t.Fatal("ranks 1..29 are still above the window")
	}

	vc.LoadMoreBefore()
	waitForPointsLeaderboard(t, listener, func() bool { return !vc.IsLoading() && vc.GetRowCount() > 100 })
	assertPointsLeaderboardWindow(t, vc, 1, 129)
	if vc.HasMoreBefore() {
		t.Fatal("the window starts at the top")
	}
	// a no-op at the top
	vc.LoadMoreBefore()
	if vc.IsLoading() {
		t.Fatal("LoadMoreBefore at the top must not load")
	}

	vc.LoadMore()
	waitForPointsLeaderboard(t, listener, func() bool { return !vc.IsLoading() && vc.GetRowCount() > 129 })
	assertPointsLeaderboardWindow(t, vc, 1, 137)
	if vc.HasMoreAfter() || !vc.IsEndReached() {
		t.Fatal("the window ends at the bottom")
	}
}

func TestPointsLeaderboardSeekClampsToTheEnd(t *testing.T) {
	server, vc, listener := newPointsLeaderboardWindowTest(t, 137)

	vc.SeekToRank(100000)
	waitForPointsLeaderboardIdle(t, vc, listener)
	assertPointsLeaderboardWindow(t, vc, 137, 137)
	if vc.HasMoreAfter() || !vc.HasMoreBefore() {
		t.Fatal("the last rank has ranks above and none below")
	}
	server.lock.Lock()
	seek := server.requests[0].SeekRank
	server.lock.Unlock()
	if seek != 100000 {
		t.Fatalf("seek_rank %d sent, want the app's rank (the server clamps)", seek)
	}

	// a rank below 1 is the top
	vc.SeekToRank(0)
	waitForPointsLeaderboard(t, listener, func() bool { return !vc.IsLoading() && vc.FirstLoadedPosition() == 1 })
	assertPointsLeaderboardWindow(t, vc, 1, 50)
	if vc.HasMoreBefore() {
		t.Fatal("the top has nothing before it")
	}
}

func TestPointsLeaderboardSeekReplacesTheWindow(t *testing.T) {
	_, vc, listener := newPointsLeaderboardWindowTest(t, 137)

	vc.Start()
	waitForPointsLeaderboardIdle(t, vc, listener)
	assertPointsLeaderboardWindow(t, vc, 1, 50)

	vc.SeekToRank(120)
	if vc.GetRowCount() != 0 {
		t.Fatal("a seek clears the rows at once")
	}
	waitForPointsLeaderboardIdle(t, vc, listener)
	assertPointsLeaderboardWindow(t, vc, 120, 137)
	if vc.HasMoreAfter() {
		t.Fatal("the window reaches the end")
	}

	vc.ReloadFromTop()
	if vc.GetRowCount() != 0 {
		t.Fatal("a reload from the top clears the rows at once")
	}
	waitForPointsLeaderboardIdle(t, vc, listener)
	assertPointsLeaderboardWindow(t, vc, 1, 50)
	if vc.HasMoreBefore() || !vc.HasMoreAfter() {
		t.Fatal("the first page")
	}
}

func TestPointsLeaderboardSeekRestartReloadsFromTheTop(t *testing.T) {
	server, vc, listener := newPointsLeaderboardWindowTest(t, 137)

	vc.SeekToRank(80)
	waitForPointsLeaderboardIdle(t, vc, listener)
	server.lock.Lock()
	server.restartOnce = "b:80"
	server.lock.Unlock()

	vc.LoadMoreBefore()
	waitForPointsLeaderboard(t, listener, func() bool { return !vc.IsLoading() && vc.FirstLoadedPosition() == 1 })
	assertPointsLeaderboardWindow(t, vc, 1, 50)
	if vc.HasMoreBefore() {
		t.Fatal("after a restart the window starts at the top")
	}
}

func TestPointsLeaderboardStaleSeekPageIsDropped(t *testing.T) {
	_, vc, listener := newPointsLeaderboardWindowTest(t, 137)

	vc.SeekToRank(20)
	// a second seek before the first lands: the first page must never show
	vc.SeekToRank(100)
	waitForPointsLeaderboardIdle(t, vc, listener)
	assertPointsLeaderboardWindow(t, vc, 100, 137)
	// let any straggler land
	time.Sleep(100 * time.Millisecond)
	assertPointsLeaderboardWindow(t, vc, 100, 137)
}

func TestMergePointsLeaderboardRows(t *testing.T) {
	rows := func(positions ...int64) []*PointsLeaderboardRow {
		out := []*PointsLeaderboardRow{}
		for _, pos := range positions {
			out = append(out, &PointsLeaderboardRow{Position: pos, RankPoints: pos})
		}
		return out
	}
	positions := func(rows []*PointsLeaderboardRow) []int64 {
		out := []int64{}
		for _, row := range rows {
			out = append(out, row.Position)
		}
		return out
	}
	// an overlapping append keeps one of each
	merged := mergePointsLeaderboardRows(rows(1, 2, 3), rows(3, 4))
	if fmt.Sprint(positions(merged)) != "[1 2 3 4]" {
		t.Fatalf("append overlap: %v", positions(merged))
	}
	// an overlapping prepend too
	merged = mergePointsLeaderboardRows(rows(28, 29, 30, 31), rows(30, 31, 32))
	if fmt.Sprint(positions(merged)) != "[28 29 30 31 32]" {
		t.Fatalf("prepend overlap: %v", positions(merged))
	}
	// the same page twice adds nothing
	merged = mergePointsLeaderboardRows(rows(1, 2), rows(1, 2))
	if fmt.Sprint(positions(merged)) != "[1 2]" {
		t.Fatalf("repeat: %v", positions(merged))
	}
	// rows without positions (an older server) are keyed by network id
	a := &PointsLeaderboardRow{NetworkId: NewId()}
	b := &PointsLeaderboardRow{NetworkId: NewId()}
	merged = mergePointsLeaderboardRows([]*PointsLeaderboardRow{a}, []*PointsLeaderboardRow{a, b})
	if len(merged) != 2 {
		t.Fatalf("id keyed: %d rows", len(merged))
	}
}

func TestPointsLeaderboardScrollLabel(t *testing.T) {
	cases := []struct {
		rank, total int64
		wantRank    int64
		text        string
		tier        int
		percent     int
	}{
		{1, 0, 1, "#1", PointsLeaderboardTierUnknown, 0},
		{1, 1, 1, "#1", PointsLeaderboardTierTop1, 1},
		{1, 3, 1, "#1", PointsLeaderboardTierTop1, 1},
		{2, 3, 2, "#2", PointsLeaderboardTierTop50, 50},
		{3, 3, 3, "#3", PointsLeaderboardTierRest, 0},
		{1, 1000, 1, "#1", PointsLeaderboardTierTop1, 1},
		{10, 1000, 10, "#10", PointsLeaderboardTierTop1, 1},
		{11, 1000, 11, "#11", PointsLeaderboardTierTop5, 5},
		{50, 1000, 50, "#50", PointsLeaderboardTierTop5, 5},
		{51, 1000, 51, "#51", PointsLeaderboardTierTop10, 10},
		{100, 1000, 100, "#100", PointsLeaderboardTierTop10, 10},
		{101, 1000, 101, "#101", PointsLeaderboardTierTop25, 25},
		{250, 1000, 250, "#250", PointsLeaderboardTierTop25, 25},
		{251, 1000, 251, "#251", PointsLeaderboardTierTop50, 50},
		{500, 1000, 500, "#500", PointsLeaderboardTierTop50, 50},
		{501, 1000, 501, "#501", PointsLeaderboardTierRest, 0},
		{1000, 1000, 1000, "#1000", PointsLeaderboardTierRest, 0},
		// rounding up: 1% of 137 holds 2 ranks
		{2, 137, 2, "#2", PointsLeaderboardTierTop1, 1},
		{3, 137, 3, "#3", PointsLeaderboardTierTop5, 5},
		// clamped
		{0, 137, 1, "#1", PointsLeaderboardTierTop1, 1},
		{5000, 137, 137, "#137", PointsLeaderboardTierRest, 0},
	}
	for _, c := range cases {
		parts := PointsLeaderboardScrollLabel(c.rank, c.total)
		if parts.Rank != c.wantRank || parts.RankText != c.text || parts.Tier != c.tier || parts.TierPercent != c.percent || parts.Total != c.total {
			t.Fatalf("label(%d, %d) = %+v, want rank %d %q tier %d percent %d", c.rank, c.total, parts, c.wantRank, c.text, c.tier, c.percent)
		}
	}
}
