package sdk

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The Protocol leaderboard bindings (server LEADERTOP.md §7): GET /sn/top and
// GET /sn/top/me against a local server.

const snTopTestSnapshotJson = `{
	"id": "snapshot-7", "tempo_index": 19034, "block": 6852240,
	"computed_at": "2026-10-29T15:12:00Z", "settlement_epoch": 88,
	"total_ranked": 1234, "head_seats": 200, "claimed_fleets": 3, "open_seats": 197,
	"projected_cutoff": 2.5, "claimed_cutoff": null,
	"binding_open": false, "binding_note": "Top 200 binding isn't open on mainnet yet",
	"measured_providers": 1450, "cohort_capacity": 1500,
	"validator_summary_at": null, "incentive_at": "2026-10-29T15:00:00Z",
	"precision_at_200": null, "stale": true
}`

// one claimed row whose fleet has a validator summary (a score of exactly 0,
// which is a value and not a null) and no incentive yet, and one anonymous
// row with no fleets that is the caller's
const snTopTestBoardJson = `{
	"snapshot": ` + snTopTestSnapshotJson + `,
	"rows": [
		{
			"position": 1, "name": "alpha", "tag": "🦊",
			"projected_score": 12.75, "raw_last_tempo": 13, "margin": 4.1, "trend": 2,
			"prefixes": 14, "shared_prefixes": 1, "presence": 0.98, "label": "estimate",
			"measurable": true, "verify_eligible": true, "claim_prompt": true, "is_me": false,
			"fleets": [
				{
					"uid": 42, "generation": 3, "valid_to_epoch": 99, "replica_score": 12.5,
					"validator_score": 0, "validator_rank": 1, "validator_rank_of": 3,
					"selected": false, "incentive": null, "incentive_stale": true
				},
				{
					"uid": 77, "generation": 1, "valid_to_epoch": 96, "replica_score": 0.25,
					"validator_score": null, "validator_rank": null, "validator_rank_of": null,
					"selected": null, "incentive": 0.0125, "incentive_stale": false
				}
			]
		},
		{
			"position": 2, "name": "", "tag": "",
			"projected_score": 2, "raw_last_tempo": 1.5, "margin": -0.2, "trend": -3,
			"prefixes": 3, "shared_prefixes": 2, "presence": 0.5, "label": "not_measurable",
			"measurable": false, "verify_eligible": true, "claim_prompt": false, "is_me": true,
			"fleets": []
		}
	],
	"me": {
		"position": 2, "name": "my-network", "tag": "",
		"projected_score": 2, "raw_last_tempo": 1.5, "margin": -0.2, "trend": -3,
		"prefixes": 3, "shared_prefixes": 2, "presence": 0.5, "label": "not_measurable",
		"measurable": false, "verify_eligible": true, "claim_prompt": false, "is_me": true,
		"fleets": []
	},
	"next": "c/2+x=y"
}`

const snTopTestMeJson = `{
	"snapshot": ` + snTopTestSnapshotJson + `,
	"me": {
		"row": {
			"position": 230, "name": "my-network", "tag": "🛰️",
			"projected_score": 2.2, "raw_last_tempo": 2.4, "margin": -0.12, "trend": 5,
			"prefixes": 4, "shared_prefixes": 2, "presence": 0.9, "label": "estimate",
			"measurable": true, "verify_eligible": true, "claim_prompt": false, "is_me": true,
			"fleets": []
		},
		"solo_score": 3.5, "distinct_prefixes": 4, "shared_prefixes": 2, "shared_with_units": 5,
		"not_measurable_reason": "",
		"history": [
			{"day": "2026-10-27", "rank": 260, "score": 1.9},
			{"day": "2026-10-28", "rank": 230, "score": 2.2}
		]
	}
}`

type snTopTestRequest struct {
	path          string
	query         url.Values
	authorization string
}

// A local server answering one fixed body, recording each request.
func newSnTopTestApi(t *testing.T, status int, body string) (*Api, chan snTopTestRequest) {
	t.Helper()
	requests := make(chan snTopTestRequest, 8)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		requests <- snTopTestRequest{
			path:          r.URL.Path,
			query:         r.URL.Query(),
			authorization: r.Header.Get("Authorization"),
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	_, api := newTestApi(t, handler)
	return api, requests
}

func awaitSnTop(t *testing.T, api *Api, args *SnTopArgs) (*SnTopResult, error) {
	t.Helper()
	done := make(chan struct{})
	var result *SnTopResult
	var err error
	api.SnTop(args, connect.NewApiCallback[*SnTopResult](func(r *SnTopResult, e error) {
		result, err = r, e
		close(done)
	}))
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("SnTop did not complete")
	}
	return result, err
}

func awaitSnTopMe(t *testing.T, api *Api) (*SnTopMeResult, error) {
	t.Helper()
	done := make(chan struct{})
	var result *SnTopMeResult
	var err error
	api.SnTopMe(connect.NewApiCallback[*SnTopMeResult](func(r *SnTopMeResult, e error) {
		result, err = r, e
		close(done)
	}))
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("SnTopMe did not complete")
	}
	return result, err
}

func takeSnTopRequest(t *testing.T, requests chan snTopTestRequest) snTopTestRequest {
	t.Helper()
	select {
	case request := <-requests:
		return request
	default:
		t.Fatal("no request was sent")
		return snTopTestRequest{}
	}
}

// Only the arguments that are set go in the query, escaped, and nil or zero
// args ask for the server's first page.
func TestSnTopUrlSendsOnlyTheSetArguments(t *testing.T) {
	for _, c := range []struct {
		name string
		args *SnTopArgs
		want string
	}{
		{"nil", nil, "https://api.example/sn/top"},
		{"zero", &SnTopArgs{}, "https://api.example/sn/top"},
		{"no page size below 1", &SnTopArgs{Limit: -5}, "https://api.example/sn/top"},
		{"limit", &SnTopArgs{Limit: 200}, "https://api.example/sn/top?limit=200"},
		{
			"all",
			&SnTopArgs{Limit: 50, Cursor: "c/2+x=y&z", Filter: SnTopFilterUnclaimed, Snapshot: "snapshot 7"},
			"https://api.example/sn/top?cursor=c%2F2%2Bx%3Dy%26z&filter=unclaimed&limit=50&snapshot=snapshot+7",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			connect.AssertEqual(t, snTopUrl("https://api.example", c.args), c.want)
		})
	}
}

// Signed in, the board carries the jwt and decodes every field: nulls leave
// the Has flags false, and a present 0 or false is a value.
func TestApiSnTopDecodesTheBoard(t *testing.T) {
	api, requests := newSnTopTestApi(t, http.StatusOK, snTopTestBoardJson)
	api.SetByJwt("board-jwt")

	result, err := awaitSnTop(t, api, &SnTopArgs{Limit: 2, Cursor: "c/1", Filter: SnTopFilterClaimed, Snapshot: "snapshot-7"})
	if err != nil {
		t.Fatal(err)
	}
	request := takeSnTopRequest(t, requests)
	connect.AssertEqual(t, request.path, "/sn/top")
	connect.AssertEqual(t, request.authorization, "Bearer board-jwt")
	wantQuery := url.Values{
		"limit":    {"2"},
		"cursor":   {"c/1"},
		"filter":   {"claimed"},
		"snapshot": {"snapshot-7"},
	}
	if !reflect.DeepEqual(request.query, wantQuery) {
		t.Fatalf("query = %v, want %v", request.query, wantQuery)
	}

	snapshot := result.Snapshot
	if snapshot == nil {
		t.Fatal("no snapshot")
	}
	connect.AssertEqual(t, snapshot.Id, "snapshot-7")
	connect.AssertEqual(t, snapshot.TempoIndex, int64(19034))
	connect.AssertEqual(t, snapshot.Block, int64(6852240))
	connect.AssertEqual(t, snapshot.ComputedAt.UnixMilli(), time.Date(2026, 10, 29, 15, 12, 0, 0, time.UTC).UnixMilli())
	connect.AssertEqual(t, snapshot.SettlementEpoch, int64(88))
	connect.AssertEqual(t, snapshot.TotalRanked, 1234)
	connect.AssertEqual(t, snapshot.HeadSeats, 200)
	connect.AssertEqual(t, snapshot.ClaimedFleets, 3)
	connect.AssertEqual(t, snapshot.OpenSeats, 197)
	connect.AssertEqual(t, snapshot.ProjectedCutoff, 2.5)
	connect.AssertEqual(t, snapshot.ClaimedCutoff, 0.0)
	connect.AssertEqual(t, snapshot.HasClaimedCutoff, false)
	connect.AssertEqual(t, snapshot.BindingOpen, false)
	connect.AssertEqual(t, snapshot.BindingNote, "Top 200 binding isn't open on mainnet yet")
	connect.AssertEqual(t, snapshot.MeasuredProviders, 1450)
	connect.AssertEqual(t, snapshot.CohortCapacity, 1500)
	if snapshot.ValidatorSummaryAt != nil {
		t.Fatalf("validator summary at = %v, want nil for null", snapshot.ValidatorSummaryAt)
	}
	connect.AssertEqual(t, snapshot.IncentiveAt.UnixMilli(), time.Date(2026, 10, 29, 15, 0, 0, 0, time.UTC).UnixMilli())
	connect.AssertEqual(t, snapshot.HasPrecisionAt200, false)
	connect.AssertEqual(t, snapshot.Stale, true)

	connect.AssertEqual(t, result.Rows.Len(), 2)
	first := result.Rows.Get(0)
	connect.AssertEqual(t, first.Position, 1)
	connect.AssertEqual(t, first.Name, "alpha")
	connect.AssertEqual(t, first.Tag, "🦊")
	connect.AssertEqual(t, first.ProjectedScore, 12.75)
	connect.AssertEqual(t, first.RawLastTempo, 13.0)
	connect.AssertEqual(t, first.Margin, 4.1)
	connect.AssertEqual(t, first.Trend, 2)
	connect.AssertEqual(t, first.Prefixes, 14)
	connect.AssertEqual(t, first.SharedPrefixes, 1)
	connect.AssertEqual(t, first.Presence, 0.98)
	connect.AssertEqual(t, first.Label, SnTopLabelEstimate)
	connect.AssertEqual(t, first.Measurable, true)
	connect.AssertEqual(t, first.VerifyEligible, true)
	connect.AssertEqual(t, first.ClaimPrompt, true)
	connect.AssertEqual(t, first.IsMe, false)
	connect.AssertEqual(t, first.Fleets.Len(), 2)

	summarized := first.Fleets.Get(0)
	connect.AssertEqual(t, summarized.Uid, int64(42))
	connect.AssertEqual(t, summarized.Generation, int64(3))
	connect.AssertEqual(t, summarized.ValidToEpoch, int64(99))
	connect.AssertEqual(t, summarized.ReplicaScore, 12.5)
	// 0 and false are values: the validator scored and did not select it
	connect.AssertEqual(t, summarized.ValidatorScore, 0.0)
	connect.AssertEqual(t, summarized.HasValidatorScore, true)
	connect.AssertEqual(t, summarized.ValidatorRank, 1)
	connect.AssertEqual(t, summarized.HasValidatorRank, true)
	connect.AssertEqual(t, summarized.ValidatorRankOf, 3)
	connect.AssertEqual(t, summarized.HasValidatorRankOf, true)
	connect.AssertEqual(t, summarized.Selected, false)
	connect.AssertEqual(t, summarized.HasSelected, true)
	connect.AssertEqual(t, summarized.Incentive, 0.0)
	connect.AssertEqual(t, summarized.HasIncentive, false)
	connect.AssertEqual(t, summarized.IncentiveStale, true)

	unsummarized := first.Fleets.Get(1)
	connect.AssertEqual(t, unsummarized.Uid, int64(77))
	connect.AssertEqual(t, unsummarized.HasValidatorScore, false)
	connect.AssertEqual(t, unsummarized.HasValidatorRank, false)
	connect.AssertEqual(t, unsummarized.HasValidatorRankOf, false)
	connect.AssertEqual(t, unsummarized.HasSelected, false)
	connect.AssertEqual(t, unsummarized.Incentive, 0.0125)
	connect.AssertEqual(t, unsummarized.HasIncentive, true)

	anonymous := result.Rows.Get(1)
	connect.AssertEqual(t, anonymous.Name, "")
	connect.AssertEqual(t, anonymous.Trend, -3)
	connect.AssertEqual(t, anonymous.Margin, -0.2)
	connect.AssertEqual(t, anonymous.Label, SnTopLabelNotMeasurable)
	connect.AssertEqual(t, anonymous.IsMe, true)
	connect.AssertEqual(t, anonymous.Fleets.Len(), 0)

	if result.Me == nil {
		t.Fatal("no me row")
	}
	connect.AssertEqual(t, result.Me.Position, 2)
	connect.AssertEqual(t, result.Me.Name, "my-network")
	connect.AssertEqual(t, result.Me.IsMe, true)
	connect.AssertEqual(t, result.Next, "c/2+x=y")
	if result.Error != nil {
		t.Fatalf("error = %+v, want none", result.Error)
	}
}

// Signed out, the board is requested with no credential, `me` and `next` are
// null, and the lists are empty rather than nil.
func TestApiSnTopSignedOutSendsNoCredential(t *testing.T) {
	api, requests := newSnTopTestApi(t, http.StatusOK, `{
		"snapshot": `+snTopTestSnapshotJson+`,
		"rows": [{"position": 1, "name": "", "tag": "", "label": "estimate"}],
		"me": null,
		"next": null
	}`)

	result, err := awaitSnTop(t, api, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := takeSnTopRequest(t, requests)
	connect.AssertEqual(t, request.path, "/sn/top")
	connect.AssertEqual(t, request.authorization, "")
	connect.AssertEqual(t, len(request.query), 0)

	if result.Me != nil {
		t.Fatalf("me = %+v, want nil", result.Me)
	}
	connect.AssertEqual(t, result.Next, "")
	connect.AssertEqual(t, result.Rows.Len(), 1)
	// a row the server sent without fleets has an empty list, not nil
	if result.Rows.Get(0).Fleets == nil {
		t.Fatal("fleets = nil, want an empty list")
	}
	connect.AssertEqual(t, result.Rows.Get(0).Fleets.Len(), 0)
}

// A body without rows still has a list to read.
func TestApiSnTopWithoutRowsHasAnEmptyList(t *testing.T) {
	api, _ := newSnTopTestApi(t, http.StatusOK, `{"snapshot": null, "rows": null, "me": null, "next": null}`)
	result, err := awaitSnTop(t, api, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Snapshot != nil {
		t.Fatalf("snapshot = %+v, want nil", result.Snapshot)
	}
	if result.Rows == nil {
		t.Fatal("rows = nil, want an empty list")
	}
	connect.AssertEqual(t, result.Rows.Len(), 0)
}

// A refusal reaches the callback as an error.
func TestApiSnTopReportsAnHttpError(t *testing.T) {
	api, _ := newSnTopTestApi(t, http.StatusBadRequest, "Bad cursor.")
	result, err := awaitSnTop(t, api, &SnTopArgs{Cursor: "stale"})
	if err == nil {
		t.Fatalf("err = nil, result = %+v; want the 400", result)
	}
	var statusError *connect.HttpStatusError
	if errors.As(err, &statusError) {
		connect.AssertEqual(t, statusError.StatusCode, http.StatusBadRequest)
	}
}

// The detail carries the jwt and decodes the row, the counts and the history.
func TestApiSnTopMeDecodesTheDetail(t *testing.T) {
	api, requests := newSnTopTestApi(t, http.StatusOK, snTopTestMeJson)
	api.SetByJwt("me-jwt")

	result, err := awaitSnTopMe(t, api)
	if err != nil {
		t.Fatal(err)
	}
	request := takeSnTopRequest(t, requests)
	connect.AssertEqual(t, request.path, "/sn/top/me")
	connect.AssertEqual(t, request.authorization, "Bearer me-jwt")
	connect.AssertEqual(t, len(request.query), 0)

	if result.Snapshot == nil || result.Snapshot.Id != "snapshot-7" {
		t.Fatalf("snapshot = %+v, want snapshot-7", result.Snapshot)
	}
	me := result.Me
	if me == nil || me.Row == nil {
		t.Fatalf("me = %+v, want the detail with its row", me)
	}
	connect.AssertEqual(t, me.Row.Position, 230)
	connect.AssertEqual(t, me.Row.Tag, "🛰️")
	connect.AssertEqual(t, me.Row.IsMe, true)
	connect.AssertEqual(t, me.SoloScore, 3.5)
	connect.AssertEqual(t, me.DistinctPrefixes, 4)
	connect.AssertEqual(t, me.SharedPrefixes, 2)
	connect.AssertEqual(t, me.SharedWithUnits, 5)
	connect.AssertEqual(t, me.NotMeasurableReason, SnTopNotMeasurableReasonNone)
	connect.AssertEqual(t, me.History.Len(), 2)
	connect.AssertEqual(t, *me.History.Get(0), SnTopHistoryPoint{Day: "2026-10-27", Rank: 260, Score: 1.9})
	connect.AssertEqual(t, *me.History.Get(1), SnTopHistoryPoint{Day: "2026-10-28", Rank: 230, Score: 2.2})
}

// A detail without history still has a list to read, and the reason decodes.
func TestApiSnTopMeWithoutHistoryHasAnEmptyList(t *testing.T) {
	api, _ := newSnTopTestApi(t, http.StatusOK, `{
		"snapshot": `+snTopTestSnapshotJson+`,
		"me": {"row": {"position": 900, "label": "not_measurable"}, "not_measurable_reason": "not_in_cohort"}
	}`)
	result, err := awaitSnTopMe(t, api)
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, result.Me.NotMeasurableReason, SnTopNotMeasurableReasonNotInCohort)
	if result.Me.History == nil {
		t.Fatal("history = nil, want an empty list")
	}
	connect.AssertEqual(t, result.Me.History.Len(), 0)
}

// The sdk's own json (what the c abi hands the desktop apps) is the plain
// struct: each nullable value beside its has_ flag. It decodes back to the
// same values, so a Has flag survives even where the value is 0.
func TestSnTopJsonRoundTrips(t *testing.T) {
	var board SnTopResult
	if err := json.Unmarshal([]byte(snTopTestBoardJson), &board); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(&board)
	if err != nil {
		t.Fatal(err)
	}
	var fields struct {
		Rows []struct {
			Fleets []map[string]any `json:"fleets"`
		} `json:"rows"`
		Snapshot map[string]any `json:"snapshot"`
	}
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, fields.Snapshot["has_claimed_cutoff"], false)
	connect.AssertEqual(t, fields.Rows[0].Fleets[0]["validator_score"], 0.0)
	connect.AssertEqual(t, fields.Rows[0].Fleets[0]["has_validator_score"], true)
	connect.AssertEqual(t, fields.Rows[0].Fleets[0]["has_incentive"], false)

	var decoded SnTopResult
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	reencoded, err := json.Marshal(&decoded)
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, string(reencoded), string(encoded))
	if !reflect.DeepEqual(snTopFleetValues(&decoded), snTopFleetValues(&board)) {
		t.Fatalf("fleets after a round trip = %+v, want %+v", snTopFleetValues(&decoded), snTopFleetValues(&board))
	}

	var me SnTopMeResult
	if err := json.Unmarshal([]byte(snTopTestMeJson), &me); err != nil {
		t.Fatal(err)
	}
	encodedMe, err := json.Marshal(&me)
	if err != nil {
		t.Fatal(err)
	}
	var decodedMe SnTopMeResult
	if err := json.Unmarshal(encodedMe, &decodedMe); err != nil {
		t.Fatal(err)
	}
	reencodedMe, err := json.Marshal(&decodedMe)
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, string(reencodedMe), string(encodedMe))
}

func snTopFleetValues(result *SnTopResult) []SnTopFleet {
	var fleets []SnTopFleet
	for i := range result.Rows.Len() {
		row := result.Rows.Get(i)
		for j := range row.Fleets.Len() {
			fleets = append(fleets, *row.Fleets.Get(j))
		}
	}
	return fleets
}
