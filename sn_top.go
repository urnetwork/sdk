package sdk

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"

	"github.com/urnetwork/connect"
)

// Protocol leaderboard (server LEADERTOP.md §7): networks ranked the way the
// SN25 validator picks its Top 200 head fleets, from one snapshot per tempo.
// GET /sn/top is public, and with a jwt it adds the caller's own row; GET
// /sn/top/me is the caller's detail.
//
// The server sends nullable numbers as json null. Each one is a value with a
// Has<Field> flag here, since gomobile binds no optional numbers: when the flag
// is false the value is zero and means nothing. Nullable times are a nil
// *Time. Lists are never nil once decoded.

// The `filter` of GET /sn/top.
const (
	SnTopFilterAll       = "all"
	SnTopFilterUnclaimed = "unclaimed"
	SnTopFilterClaimed   = "claimed"
)

// The recommendation `label` of a row.
const (
	// the projection's measured precision isn't high enough to recommend yet
	SnTopLabelEstimate = "estimate"
	SnTopLabelLikely   = "likely"
	// within 20% of the Top 200 line either way
	SnTopLabelBorderline = "borderline"
	SnTopLabelUnlikely   = "unlikely"
	// the validator cannot measure the network, whatever its projection
	SnTopLabelNotMeasurable = "not_measurable"
)

// Why the validator cannot measure the caller's network (SnTopMe).
const (
	// the network is measurable
	SnTopNotMeasurableReasonNone = ""
	// its providers aren't in the validator's verify cohort
	SnTopNotMeasurableReasonNotInCohort = "not_in_cohort"
	// its providers share an egress address with another network
	SnTopNotMeasurableReasonSharedEgress = "shared_egress"
	// none of its devices provides
	SnTopNotMeasurableReasonNotProviding = "not_providing"
)

// snTopNullable decodes one json value that the server may send as null.
// encoding/json calls UnmarshalJSON for a literal null too, so a null and an
// absent key both leave it unset.
type snTopNullable[T any] struct {
	value T
	set   bool
}

func (self *snTopNullable[T]) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*self = snTopNullable[T]{}
		return nil
	}
	var value T
	if err := json.Unmarshal(b, &value); err != nil {
		return err
	}
	*self = snTopNullable[T]{value: value, set: true}
	return nil
}

// get resolves the value and its Has flag. The server's form is the value or
// null. The sdk's own json form (these types marshal as plain structs) carries
// the has_ flag beside the value, so a value the sdk encoded decodes back
// unchanged. Without the flag the value is zero.
func (self snTopNullable[T]) get(has *bool) (T, bool) {
	set := self.set
	if has != nil {
		set = *has
	}
	if !set {
		var zero T
		return zero, false
	}
	return self.value, true
}

// SnTopArgs selects one page of GET /sn/top. The zero value is the first page
// of the newest snapshot, every network, with the server's page size.
type SnTopArgs struct {
	// page size from 1 to 200; 0 asks for the server's default (50)
	Limit int `json:"limit,omitempty"`
	// a previous page's `Next`; empty for the first page
	Cursor string `json:"cursor,omitempty"`
	// one of the SnTopFilter* values; empty means SnTopFilterAll
	Filter string `json:"filter,omitempty"`
	// a snapshot `Id` to read the first page from; empty for the newest
	Snapshot string `json:"snapshot,omitempty"`
}

// SnTopSnapshot is one per-tempo computation of the board.
type SnTopSnapshot struct {
	// opaque; SnTopArgs.Snapshot reads a first page from it
	Id         string `json:"id"`
	TempoIndex int64  `json:"tempo_index"`
	// the chain block it was computed at
	Block      int64 `json:"block"`
	ComputedAt *Time `json:"computed_at"`
	// the settlement epoch whose prefix hashes it uses
	SettlementEpoch int64 `json:"settlement_epoch"`
	// networks ranked
	TotalRanked int `json:"total_ranked"`
	// head seats on SN25 (200)
	HeadSeats     int `json:"head_seats"`
	ClaimedFleets int `json:"claimed_fleets"`
	OpenSeats     int `json:"open_seats"`
	// the projected score of the 200th unit: the line every margin and label
	// is measured against
	ProjectedCutoff float64 `json:"projected_cutoff"`
	// the current floor among claimed fleets; unset when no fleet has claimed
	ClaimedCutoff    float64 `json:"claimed_cutoff"`
	HasClaimedCutoff bool    `json:"has_claimed_cutoff"`
	// fleet binding is open on mainnet; while it is closed the apps say that
	// claiming isn't open yet, and show no live claim button
	BindingOpen bool `json:"binding_open"`
	// the state of binding, in English
	BindingNote string `json:"binding_note"`
	// providers in the validator's verify cohort, and how many it holds
	MeasuredProviders int `json:"measured_providers"`
	CohortCapacity    int `json:"cohort_capacity"`
	// when the validator's latest head summary arrived; nil without one
	ValidatorSummaryAt *Time `json:"validator_summary_at"`
	// when on-chain incentive was last read; nil before the first read
	IncentiveAt *Time `json:"incentive_at"`
	// the measured precision of the projected top 200, 0 to 1; unset until
	// fleets bind
	PrecisionAt200    float64 `json:"precision_at_200"`
	HasPrecisionAt200 bool    `json:"has_precision_at_200"`
	// computed more than 3 tempos ago
	Stale bool `json:"stale"`
}

func (self *SnTopSnapshot) UnmarshalJSON(b []byte) error {
	// plain has the fields and none of the methods; the fields beside it
	// shadow its nullable ones
	type plain SnTopSnapshot
	var wire struct {
		plain
		ClaimedCutoff     snTopNullable[float64] `json:"claimed_cutoff"`
		HasClaimedCutoff  *bool                  `json:"has_claimed_cutoff"`
		PrecisionAt200    snTopNullable[float64] `json:"precision_at_200"`
		HasPrecisionAt200 *bool                  `json:"has_precision_at_200"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}
	*self = SnTopSnapshot(wire.plain)
	self.ClaimedCutoff, self.HasClaimedCutoff = wire.ClaimedCutoff.get(wire.HasClaimedCutoff)
	self.PrecisionAt200, self.HasPrecisionAt200 = wire.PrecisionAt200.get(wire.HasPrecisionAt200)
	return nil
}

// SnTopFleet is one fleet a network has claimed, bound on SN25 to a hotkey
// UID. The validator fields are unset until the validator's head summary
// covers the fleet, and the incentive until it is first read on chain.
type SnTopFleet struct {
	Uid          int64 `json:"uid"`
	Generation   int64 `json:"generation"`
	ValidToEpoch int64 `json:"valid_to_epoch"`
	// the board's replica of the validator's head score for the fleet
	ReplicaScore float64 `json:"replica_score"`
	// the validator's own head score
	ValidatorScore    float64 `json:"validator_score"`
	HasValidatorScore bool    `json:"has_validator_score"`
	// the fleet's 1-based rank among the fleets the validator ranked, and how
	// many it ranked ("#rank of count claimed")
	ValidatorRank      int  `json:"validator_rank"`
	HasValidatorRank   bool `json:"has_validator_rank"`
	ValidatorRankOf    int  `json:"validator_rank_of"`
	HasValidatorRankOf bool `json:"has_validator_rank_of"`
	// the validator selected the fleet into the Top 200
	Selected    bool `json:"selected"`
	HasSelected bool `json:"has_selected"`
	// the UID's on-chain incentive
	Incentive    float64 `json:"incentive"`
	HasIncentive bool    `json:"has_incentive"`
	// the incentive is stale: the validator's weights are its fallback row or
	// outside the activity cutoff
	IncentiveStale bool `json:"incentive_stale"`
}

func (self *SnTopFleet) UnmarshalJSON(b []byte) error {
	type plain SnTopFleet
	var wire struct {
		plain
		ValidatorScore     snTopNullable[float64] `json:"validator_score"`
		HasValidatorScore  *bool                  `json:"has_validator_score"`
		ValidatorRank      snTopNullable[int]     `json:"validator_rank"`
		HasValidatorRank   *bool                  `json:"has_validator_rank"`
		ValidatorRankOf    snTopNullable[int]     `json:"validator_rank_of"`
		HasValidatorRankOf *bool                  `json:"has_validator_rank_of"`
		Selected           snTopNullable[bool]    `json:"selected"`
		HasSelected        *bool                  `json:"has_selected"`
		Incentive          snTopNullable[float64] `json:"incentive"`
		HasIncentive       *bool                  `json:"has_incentive"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}
	*self = SnTopFleet(wire.plain)
	self.ValidatorScore, self.HasValidatorScore = wire.ValidatorScore.get(wire.HasValidatorScore)
	self.ValidatorRank, self.HasValidatorRank = wire.ValidatorRank.get(wire.HasValidatorRank)
	self.ValidatorRankOf, self.HasValidatorRankOf = wire.ValidatorRankOf.get(wire.HasValidatorRankOf)
	self.Selected, self.HasSelected = wire.Selected.get(wire.HasSelected)
	self.Incentive, self.HasIncentive = wire.Incentive.get(wire.HasIncentive)
	return nil
}

type SnTopFleetList struct {
	exportedList[*SnTopFleet]
}

func NewSnTopFleetList() *SnTopFleetList {
	return &SnTopFleetList{
		exportedList: *newExportedList[*SnTopFleet](),
	}
}

// SnTopRow is one network on the board, scored as one hypothetical fleet of
// its verify-eligible providers. No row carries a network id.
type SnTopRow struct {
	// the 1-based position in the snapshot's total order
	Position int `json:"position"`
	// the network name, only when the network shows its name on leaderboards;
	// "" when the row is anonymous
	Name string `json:"name"`
	// the emoji tag; "" when unset
	Tag            string  `json:"tag"`
	ProjectedScore float64 `json:"projected_score"`
	RawLastTempo   float64 `json:"raw_last_tempo"`
	// the score relative to the snapshot's ProjectedCutoff, as a fraction of
	// it: positive above the Top 200 line, negative below
	Margin float64 `json:"margin"`
	// positions moved since the previous tempo; positive is up
	Trend int `json:"trend"`
	// distinct /29 or /48 egress prefixes, and how many of them another
	// network or claimed fleet shares
	Prefixes       int `json:"prefixes"`
	SharedPrefixes int `json:"shared_prefixes"`
	// how present the providers were over the last tempo, 0 to 1
	Presence float64 `json:"presence"`
	// one of the SnTopLabel* values
	Label string `json:"label"`
	// the validator can measure the network (its providers are in the cohort)
	Measurable     bool `json:"measurable"`
	VerifyEligible bool `json:"verify_eligible"`
	// prompt the network to claim: measurable and position 200 or better
	ClaimPrompt bool `json:"claim_prompt"`
	// the caller's own network
	IsMe bool `json:"is_me"`
	// the network's claimed fleets; empty when it has not claimed. An
	// anonymous claimed row is shown by its fleets' UIDs.
	Fleets *SnTopFleetList `json:"fleets"`
}

func (self *SnTopRow) UnmarshalJSON(b []byte) error {
	type plain SnTopRow
	var wire plain
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}
	*self = SnTopRow(wire)
	if self.Fleets == nil {
		self.Fleets = NewSnTopFleetList()
	}
	return nil
}

type SnTopRowList struct {
	exportedList[*SnTopRow]
}

func NewSnTopRowList() *SnTopRowList {
	return &SnTopRowList{
		exportedList: *newExportedList[*SnTopRow](),
	}
}

// SnTopHistoryPoint is the caller network's projected rank and score on one
// day.
type SnTopHistoryPoint struct {
	// YYYY-MM-DD
	Day   string  `json:"day"`
	Rank  int     `json:"rank"`
	Score float64 `json:"score"`
}

type SnTopHistoryPointList struct {
	exportedList[*SnTopHistoryPoint]
}

func NewSnTopHistoryPointList() *SnTopHistoryPointList {
	return &SnTopHistoryPointList{
		exportedList: *newExportedList[*SnTopHistoryPoint](),
	}
}

// SnTopMe is the caller network's standing on the board, with what is behind
// it. Counts only, never prefix hashes or addresses.
type SnTopMe struct {
	Row *SnTopRow `json:"row"`
	// the score if the network claimed alone today, with no other network or
	// fleet on its prefixes claiming
	SoloScore        float64 `json:"solo_score"`
	DistinctPrefixes int     `json:"distinct_prefixes"`
	SharedPrefixes   int     `json:"shared_prefixes"`
	// how many other networks and claimed fleets share its prefixes
	SharedWithUnits int `json:"shared_with_units"`
	// one of the SnTopNotMeasurableReason* values
	NotMeasurableReason string `json:"not_measurable_reason"`
	// daily, at most 90 days
	History *SnTopHistoryPointList `json:"history"`
}

func (self *SnTopMe) UnmarshalJSON(b []byte) error {
	type plain SnTopMe
	var wire plain
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}
	*self = SnTopMe(wire)
	if self.History == nil {
		self.History = NewSnTopHistoryPointList()
	}
	return nil
}

// SnTopResult is one page of the board (GET /sn/top).
type SnTopResult struct {
	Snapshot *SnTopSnapshot `json:"snapshot"`
	// the page's rows in position order, within the top 400 positions
	Rows *SnTopRowList `json:"rows"`
	// the caller's own row wherever it ranks; nil when the request was not
	// signed in or the network isn't ranked
	Me *SnTopRow `json:"me"`
	// the cursor of the next page (SnTopArgs.Cursor); "" on the last page
	Next  string    `json:"next"`
	Error *ApiError `json:"error,omitempty"`
}

func (self *SnTopResult) UnmarshalJSON(b []byte) error {
	type plain SnTopResult
	var wire plain
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}
	*self = SnTopResult(wire)
	if self.Rows == nil {
		self.Rows = NewSnTopRowList()
	}
	return nil
}

type SnTopCallback connect.ApiCallback[*SnTopResult]

// SnTop reads one page of the Protocol leaderboard (GET /sn/top). The board
// is public: signed out the request carries no credential, and signed in it
// adds `Me`. nil args read the first page of the newest snapshot.
func (self *Api) SnTop(args *SnTopArgs, callback SnTopCallback) {
	runAsyncApiRequest[*SnTopResult](callback, func(callback connect.ApiCallback[*SnTopResult]) {
		connect.HttpGetWithRawFunction(
			self.ctx,
			self.getHttpGetRaw(),
			snTopUrl(self.apiUrl, args),
			self.GetByJwt(),
			&SnTopResult{},
			callback,
		)
	})
}

// snTopUrl is the GET /sn/top url, with only the arguments that are set.
func snTopUrl(apiUrl string, args *SnTopArgs) string {
	query := url.Values{}
	if args != nil {
		if 0 < args.Limit {
			query.Set("limit", strconv.Itoa(args.Limit))
		}
		if args.Cursor != "" {
			query.Set("cursor", args.Cursor)
		}
		if args.Filter != "" {
			query.Set("filter", args.Filter)
		}
		if args.Snapshot != "" {
			query.Set("snapshot", args.Snapshot)
		}
	}
	requestUrl := fmt.Sprintf("%s/sn/top", apiUrl)
	if encoded := query.Encode(); encoded != "" {
		requestUrl += "?" + encoded
	}
	return requestUrl
}

// SnTopMeResult is the caller's detail (GET /sn/top/me).
type SnTopMeResult struct {
	Snapshot *SnTopSnapshot `json:"snapshot"`
	Me       *SnTopMe       `json:"me"`
	Error    *ApiError      `json:"error,omitempty"`
}

type SnTopMeCallback connect.ApiCallback[*SnTopMeResult]

// SnTopMe reads the caller network's detail on the Protocol leaderboard (GET
// /sn/top/me), from the same snapshot as the board. It needs the jwt.
func (self *Api) SnTopMe(callback SnTopMeCallback) {
	runAsyncApiRequest[*SnTopMeResult](callback, func(callback connect.ApiCallback[*SnTopMeResult]) {
		connect.HttpGetWithRawFunction(
			self.ctx,
			self.getHttpGetRaw(),
			fmt.Sprintf("%s/sn/top/me", self.apiUrl),
			self.GetByJwt(),
			&SnTopMeResult{},
			callback,
		)
	})
}
