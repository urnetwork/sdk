//go:build !ios_extension

package sdk

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

// pointsLeaderboardTestServer serves a three-page leaderboard for "points"
// and a one-page leaderboard for "blocks", remembers every request and can
// answer a cursor with `restart`.
type pointsLeaderboardTestServer struct {
	lock     sync.Mutex
	requests []GetPointsLeaderboardArgs
	restart  map[string]bool
	fail     bool
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
		result := map[string]any{"total_ranked": 3, "latest_epoch": 57, "snapshot_time": "2026-09-03T00:00:00Z"}
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
	server := &pointsLeaderboardTestServer{restart: map[string]bool{}}
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
