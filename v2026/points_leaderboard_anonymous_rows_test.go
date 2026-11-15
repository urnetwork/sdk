package sdk

import (
	"encoding/json"
	"fmt"
	"testing"
)

// Since 2026-10-29 the server sends `network_id` only on a named row and on
// the signed-in caller's own row (LEADERTOP Q12), and marks the caller's row
// with `is_me`. A row's `position` is its place in the server's total order,
// which breaks a three-key tie by the network id the client can no longer
// see on an anonymous row.

// tied rows: the same points, blocks and streak, so only the final tie-break
// orders them
func pointsLeaderboardTiedRow(t *testing.T, fields string) *PointsLeaderboardRow {
	t.Helper()
	body := `{"total_points": 10, "blocks_with_points": 4, "streak": 3,` +
		` "longest_streak": 5, "rank_points": 6, "rank_blocks": 6, "rank_streak": 6, ` + fields + `}`
	row := &PointsLeaderboardRow{}
	if err := json.Unmarshal([]byte(body), row); err != nil {
		t.Fatal(err)
	}
	return row
}

func pointsLeaderboardRowPositions(rows []*PointsLeaderboardRow) string {
	positions := []int64{}
	for _, row := range rows {
		positions = append(positions, row.Position)
	}
	return fmt.Sprint(positions)
}

// An anonymous row decodes without a network id, the caller's own row keeps
// its id and says is_me, and the key of either carries its position.
func TestPointsLeaderboardAnonymousRowDecodesWithoutNetworkId(t *testing.T) {
	anonymous := pointsLeaderboardTiedRow(t, `"anonymous": true, "emoji_tag": "🦊", "position": 7`)
	if anonymous.NetworkId != nil {
		t.Fatalf("anonymous network id = %v, want nil", anonymous.NetworkId)
	}
	if anonymous.IsMe {
		t.Fatal("anonymous row is_me = true, want false")
	}
	key := PointsLeaderboardKeyOf(anonymous)
	if key.NetworkId != "" || key.Position != 7 {
		t.Fatalf("key of the anonymous row = %+v, want no id and position 7", key)
	}

	mine := pointsLeaderboardTiedRow(t, `"network_id": "00000000-0000-0000-0000-00000000a001", "is_me": true, "anonymous": true, "position": 8`)
	if mine.NetworkId == nil || mine.NetworkId.String() != "00000000-0000-0000-0000-00000000a001" {
		t.Fatalf("caller's network id = %v", mine.NetworkId)
	}
	if !mine.IsMe {
		t.Fatal("caller's row is_me = false, want true")
	}

	// the sdk's own json keeps both, so the wasm and c abi rows carry is_me
	encoded, err := json.Marshal(mine)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["is_me"] != true || fields["network_id"] != "00000000-0000-0000-0000-00000000a001" {
		t.Fatalf("encoded row = %s", encoded)
	}
}

// A three-key tie between named and anonymous rows sorts in the server's
// order, by position. With the network id as the final tie-break an
// anonymous row's missing id sorted ahead of every tied named row, and two
// tied anonymous rows kept whatever order they arrived in.
func TestPointsLeaderboardTieFollowsTheServerPosition(t *testing.T) {
	// the server placed the named row at 5 (its id is below the two
	// anonymous networks' ids, which the client does not see)
	named := pointsLeaderboardTiedRow(t, `"network_id": "ffffffff-ffff-ffff-ffff-ffffffffffff", "anonymous": false, "network_name": "zed", "position": 5`)
	anonymous := pointsLeaderboardTiedRow(t, `"anonymous": true, "position": 6`)
	otherAnonymous := pointsLeaderboardTiedRow(t, `"anonymous": true, "position": 4`)

	for _, sortBy := range []string{PointsLeaderboardSortPoints, PointsLeaderboardSortBlocks, PointsLeaderboardSortStreak} {
		for _, rows := range [][]*PointsLeaderboardRow{
			{anonymous, named, otherAnonymous},
			{named, otherAnonymous, anonymous},
			{otherAnonymous, anonymous, named},
		} {
			sorted := append([]*PointsLeaderboardRow(nil), rows...)
			sortPointsLeaderboardRows(sortBy, sorted)
			if got := pointsLeaderboardRowPositions(sorted); got != "[4 5 6]" {
				t.Fatalf("%s: %s sorted to positions %s, want [4 5 6]", sortBy, pointsLeaderboardRowPositions(rows), got)
			}
		}
		if ComparePointsLeaderboardRows(sortBy, named, anonymous) >= 0 {
			t.Fatalf("%s: the named row at 5 must sort ahead of the anonymous row at 6", sortBy)
		}
		if ComparePointsLeaderboardRows(sortBy, otherAnonymous, named) >= 0 {
			t.Fatalf("%s: the anonymous row at 4 must sort ahead of the named row at 5", sortBy)
		}
		if ComparePointsLeaderboardRows(sortBy, anonymous, anonymous) != 0 {
			t.Fatalf("%s: a row must compare 0 with itself", sortBy)
		}
	}

	// values still come first: a row ahead on points is ahead whatever its
	// position says
	ahead := pointsLeaderboardTiedRow(t, `"anonymous": true, "position": 9`)
	ahead.TotalPoints = 11
	if ComparePointsLeaderboardRows(PointsLeaderboardSortPoints, ahead, named) >= 0 {
		t.Fatal("more points must sort ahead regardless of position")
	}

	// a key without a position (an older server's row) falls back to the
	// network id
	withoutPosition := &PointsLeaderboardKey{NanoPoints: 10, Blocks: 4, Streak: 3, NetworkId: "a"}
	withPosition := &PointsLeaderboardKey{NanoPoints: 10, Blocks: 4, Streak: 3, NetworkId: "b", Position: 1}
	if ComparePointsLeaderboardKeys(PointsLeaderboardSortPoints, withoutPosition, withPosition) >= 0 {
		t.Fatal("without both positions the network id must order the keys")
	}
}
