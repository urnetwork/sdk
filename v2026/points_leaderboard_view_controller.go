//go:build !ios_extension

package sdk

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/emoji"
)

// PointsLeaderboardListener fires on every state change of the
// PointsLeaderboardViewController: a page appended or replaced, loading
// started or finished, the sort switched, an error, or the caller's own row.
// Read the state back through the getters.
type PointsLeaderboardListener interface {
	PointsLeaderboardChanged()
}

// PointsLeaderboardPageSize is the page the controller asks the server for.
const PointsLeaderboardPageSize = 50

// PointsLeaderboardViewController is the all-time points leaderboard
// (android/POINTSLEADERBOARD.md): the ranked networks in one sort order,
// paged from the server with a keyset cursor. It owns the sort, the pages
// and the paging state; the app renders `GetRows` in order and calls
// `LoadMore` when the list nears its end. It never sorts, ranks or pages on
// its own.
//
// Only networks that opted in are listed; a row's `NetworkName` is set only
// when that network's name is public, otherwise `Anonymous` is true and the
// app shows its localized "Anonymous". `EmojiTag` shows either way.
type PointsLeaderboardViewController struct {
	ctx    context.Context
	cancel context.CancelFunc

	device Device
	// api-only (NewPointsLeaderboardViewControllerWithApi): no device, the
	// same controller over the network space api. Exactly one of device / api.
	api *Api

	stateLock sync.Mutex

	sort string
	rows []*PointsLeaderboardRow
	// the cursor of the next page; empty once the end is reached
	nextCursor string
	endReached bool
	loading    bool
	// bumped by SetSort and Refresh so a response to a request from before
	// the change is dropped
	generation int
	started    bool

	me           *PointsLeaderboardMe
	totalRanked  int64
	latestEpoch  int64
	snapshotTime *Time
	errorMessage string

	listeners *connect.CallbackList[PointsLeaderboardListener]
}

func newPointsLeaderboardViewController(ctx context.Context, device Device) *PointsLeaderboardViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	return &PointsLeaderboardViewController{
		ctx:       cancelCtx,
		cancel:    cancel,
		device:    device,
		sort:      PointsLeaderboardSortPoints,
		listeners: connect.NewCallbackList[PointsLeaderboardListener](),
	}
}

// NewPointsLeaderboardViewControllerWithApi opens the leaderboard over an api
// with no device (a signed-in host, or a public page: the leaderboard needs no
// jwt, `GetMe` is then nil). The caller owns Close.
func NewPointsLeaderboardViewControllerWithApi(ctx context.Context, api *Api) *PointsLeaderboardViewController {
	vc := newPointsLeaderboardViewController(ctx, nil)
	vc.api = api
	return vc
}

func (self *PointsLeaderboardViewController) getApi() *Api {
	if self.api != nil {
		return self.api
	}
	return self.device.GetApi()
}

// Start fetches the first page of the current sort (once).
func (self *PointsLeaderboardViewController) Start() {
	self.stateLock.Lock()
	if self.started {
		self.stateLock.Unlock()
		return
	}
	self.started = true
	self.stateLock.Unlock()

	self.fetch("", true)
}

func (self *PointsLeaderboardViewController) Stop() {}

func (self *PointsLeaderboardViewController) Close() {
	deviceLog(self.device).Info("[plvc]close")

	self.cancel()
}

func (self *PointsLeaderboardViewController) AddPointsLeaderboardListener(listener PointsLeaderboardListener) Sub {
	callbackId := self.listeners.Add(listener)
	return newSub(func() {
		self.listeners.Remove(callbackId)
	})
}

func (self *PointsLeaderboardViewController) pointsLeaderboardChanged() {
	for _, listener := range self.listeners.Get() {
		connect.HandleError(func() {
			listener.PointsLeaderboardChanged()
		})
	}
}

// GetSort is the dimension the rows are sorted by, one of the
// PointsLeaderboardSort* values.
func (self *PointsLeaderboardViewController) GetSort() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.sort
}

// SetSort switches the dimension the list is sorted by. The rows are cleared
// and the first page of the new order is fetched. An unknown sort is
// ignored; the current sort is a no-op.
func (self *PointsLeaderboardViewController) SetSort(sort string) {
	if !IsPointsLeaderboardSort(sort) {
		return
	}

	self.stateLock.Lock()
	if sort == self.sort {
		self.stateLock.Unlock()
		return
	}
	self.sort = sort
	self.generation += 1
	self.rows = nil
	self.nextCursor = ""
	self.endReached = false
	self.loading = false
	self.errorMessage = ""
	started := self.started
	self.stateLock.Unlock()

	self.pointsLeaderboardChanged()
	if started {
		self.fetch("", true)
	}
}

// LoadMore fetches the next page. It is a no-op while a page is loading and
// once the end is reached; after an error it retries the same page.
func (self *PointsLeaderboardViewController) LoadMore() {
	self.stateLock.Lock()
	if !self.started || self.loading || self.endReached {
		self.stateLock.Unlock()
		return
	}
	cursor := self.nextCursor
	self.stateLock.Unlock()

	self.fetch(cursor, cursor == "")
}

// Refresh re-fetches the first page of the current sort. The rows stay in
// place until the new page lands, then it replaces them.
func (self *PointsLeaderboardViewController) Refresh() {
	self.stateLock.Lock()
	self.generation += 1
	self.loading = false
	self.errorMessage = ""
	self.started = true
	self.stateLock.Unlock()

	self.fetch("", true)
}

func (self *PointsLeaderboardViewController) fetch(cursor string, replace bool) {
	self.stateLock.Lock()
	self.loading = true
	self.errorMessage = ""
	generation := self.generation
	sort := self.sort
	self.stateLock.Unlock()

	self.pointsLeaderboardChanged()

	args := &GetPointsLeaderboardArgs{
		Sort:   sort,
		Cursor: cursor,
		Limit:  PointsLeaderboardPageSize,
	}
	self.getApi().GetPointsLeaderboard(args, GetPointsLeaderboardCallback(connect.NewApiCallback[*PointsLeaderboardResult](
		func(result *PointsLeaderboardResult, err error) {
			self.handlePage(generation, cursor, replace, result, err)
		},
	)))
}

func (self *PointsLeaderboardViewController) handlePage(
	generation int,
	cursor string,
	replace bool,
	result *PointsLeaderboardResult,
	err error,
) {
	self.stateLock.Lock()
	if generation != self.generation {
		// SetSort or Refresh happened while this page was in flight
		self.stateLock.Unlock()
		return
	}

	if err == nil && result == nil {
		err = errors.New("empty result")
	}
	if err == nil && result.Error != nil {
		err = errors.New(result.Error.Message)
	}
	if err != nil {
		self.loading = false
		self.errorMessage = err.Error()
		self.stateLock.Unlock()

		self.pointsLeaderboardChanged()
		return
	}

	if result.Restart {
		if cursor == "" {
			// a restart on a fresh page: the server has nothing to page
			self.loading = false
			self.endReached = true
			self.stateLock.Unlock()

			self.pointsLeaderboardChanged()
			return
		}
		// the snapshot behind the cursor is gone: reload from the top
		self.stateLock.Unlock()

		self.fetch("", true)
		return
	}

	page := []*PointsLeaderboardRow{}
	if result.Rows != nil {
		for _, row := range result.Rows.getAll() {
			if row == nil {
				continue
			}
			formatPointsLeaderboardRow(row)
			page = append(page, row)
		}
	}
	if replace {
		self.rows = page
	} else {
		self.rows = append(self.rows, page...)
	}
	// the rows are always in the sort's order (see ComparePointsLeaderboardRows),
	// whatever order the pages arrived in
	sortPointsLeaderboardRows(self.sort, self.rows)
	self.nextCursor = result.NextCursor
	self.endReached = result.NextCursor == "" || len(page) == 0
	self.loading = false
	self.errorMessage = ""
	if result.Me != nil {
		formatPointsLeaderboardRow(result.Me.Row)
	}
	if replace || result.Me != nil {
		self.me = result.Me
	}
	self.totalRanked = result.TotalRanked
	self.latestEpoch = result.LatestEpoch
	if result.SnapshotTime != nil {
		self.snapshotTime = result.SnapshotTime
	}
	self.stateLock.Unlock()

	self.pointsLeaderboardChanged()
}

// GetRows is every row fetched so far, in server order, with the preformatted
// text fields filled in.
func (self *PointsLeaderboardViewController) GetRows() *PointsLeaderboardRowList {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	list := NewPointsLeaderboardRowList()
	list.addAll(self.rows...)
	return list
}

func (self *PointsLeaderboardViewController) GetRowCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.rows)
}

// IsLoading is true while a page is in flight.
func (self *PointsLeaderboardViewController) IsLoading() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.loading
}

// IsEndReached is true once the last page of the current sort has landed;
// LoadMore then does nothing.
func (self *PointsLeaderboardViewController) IsEndReached() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.endReached
}

// GetMe is the caller's own row and opt-in state, nil when the api holds no
// jwt or before the first page lands. It is set whether or not the network
// opted in, so the header can always show its own stats.
func (self *PointsLeaderboardViewController) GetMe() *PointsLeaderboardMe {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.me
}

// GetErrorMessage is the last page's error, empty when the last page landed.
func (self *PointsLeaderboardViewController) GetErrorMessage() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.errorMessage
}

// GetTotalRanked is the number of networks ranked (opted in or not).
func (self *PointsLeaderboardViewController) GetTotalRanked() int64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.totalRanked
}

// GetLatestEpoch is the latest finalized epoch the snapshot counts.
func (self *PointsLeaderboardViewController) GetLatestEpoch() int64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.latestEpoch
}

// GetSnapshotTime is when the ranks were computed, nil before the first page.
func (self *PointsLeaderboardViewController) GetSnapshotTime() *Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.snapshotTime
}

// formatPointsLeaderboardRow fills the preformatted text fields of a row.
func formatPointsLeaderboardRow(row *PointsLeaderboardRow) {
	if row == nil {
		return
	}
	row.DisplayName = ""
	if !row.Anonymous {
		row.DisplayName = row.NetworkName
	}
	row.TotalPointsText = FormatPoints(row.TotalPoints)
	row.BlocksWithPointsText = fmt.Sprintf("%d", row.BlocksWithPoints)
	row.StreakText = fmt.Sprintf("%d", row.Streak)
	row.LongestStreakText = fmt.Sprintf("%d", row.LongestStreak)
	row.RankPointsText = FormatRank(row.RankPoints)
	row.RankBlocksText = FormatRank(row.RankBlocks)
	row.RankStreakText = FormatRank(row.RankStreak)
}

// FormatPoints renders points as a whole number with thousands separators
// ("152,829"); fractions round to the nearest point.
func FormatPoints(points float64) string {
	if math.IsNaN(points) || math.IsInf(points, 0) {
		return "0"
	}
	n := int64(math.Round(points))
	negative := n < 0
	if negative {
		n = -n
	}
	digits := fmt.Sprintf("%d", n)
	var out strings.Builder
	head := len(digits) % 3
	if 0 < head {
		out.WriteString(digits[:head])
	}
	for i := head; i < len(digits); i += 3 {
		if 0 < out.Len() {
			out.WriteByte(',')
		}
		out.WriteString(digits[i : i+3])
	}
	if negative {
		return "-" + out.String()
	}
	return out.String()
}

// FormatRank renders a rank as "#37"; an unranked network (rank 0) as "-".
func FormatRank(rank int64) string {
	if rank <= 0 {
		return "-"
	}
	return fmt.Sprintf("#%d", rank)
}

/**
 * Emoji tag
 */

// EmojiTagMaxCount is the most emoji a tag can hold.
const EmojiTagMaxCount = emoji.MaxTagEmoji

// EmojiTagSuggestMaxCount is the most emoji SuggestEmojiTag returns.
const EmojiTagSuggestMaxCount = emoji.SuggestMaxEmoji

// SuggestEmojiTag returns a random tag of count distinct emoji to prefill the
// emoji editor with, so a network gets a usable tag without typing: count is
// clamped to 1..EmojiTagSuggestMaxCount, and zero or less picks the length at
// random in that range. Every suggestion passes ValidateEmojiTag unchanged.
func SuggestEmojiTag(count int) string {
	return emoji.Suggest(count, nil)
}

// Why ValidateEmojiTag rejected a tag; the app localizes by reason.
const (
	EmojiTagReasonEmpty    = "empty"
	EmojiTagReasonTooMany  = "too_many"
	EmojiTagReasonNotEmoji = "not_emoji"
)

// EmojiTagValidation is the result of ValidateEmojiTag.
type EmojiTagValidation struct {
	Ok bool `json:"ok"`
	// the number of emoji in the tag (0 when rejected)
	Count int `json:"count"`
	// the tag to send to the server: NFC-normalized, text-default pictographs
	// promoted to emoji presentation (empty when rejected)
	Normalized string `json:"normalized"`
	// "" when ok, else one of the EmojiTagReason* values
	Reason string `json:"reason"`
	// an English fallback for the reason; localize by Reason
	Message string `json:"message"`
}

// ValidateEmojiTag checks an emoji tag exactly the way the server does
// (connect/emoji): one to six emoji and nothing else. Run it on every change
// of the editor so a non-emoji character is rejected before the request.
func ValidateEmojiTag(tag string) *EmojiTagValidation {
	normalized, count, err := emoji.ValidateTag(tag)
	validation := &EmojiTagValidation{}
	switch {
	case err == nil:
		validation.Ok = true
		validation.Count = count
		validation.Normalized = normalized
	case errors.Is(err, emoji.ErrEmpty):
		validation.Reason = EmojiTagReasonEmpty
		validation.Message = "Add at least one emoji."
	case errors.Is(err, emoji.ErrTooMany):
		validation.Reason = EmojiTagReasonTooMany
		validation.Message = fmt.Sprintf("Use at most %d emoji.", EmojiTagMaxCount)
	default:
		validation.Reason = EmojiTagReasonNotEmoji
		validation.Message = "Only emoji are allowed."
	}
	return validation
}

// Ordering
//
// The leaderboard is sortable by any dimension, and each sort has its own
// tie-break order (user decision, 2026-09-03):
//
//	points: (points, streak, blocks)
//	blocks: (blocks, streak, points)
//	streak: (streak, blocks, points)
//
// Every key is descending; when all three keys tie the network id (ascending)
// makes the order total, so two clients and the server always agree on the
// exact sequence. This is THE definition of the order: the view controller
// keeps its rows in it, and the server ranks and pages with it (the ranks
// `rank_*` are competition ranks on the same three-key tuple, so two networks
// share a rank only when all three keys tie).

// PointsLeaderboardNanoPointsPerPoint is the points unit on the wire: a row's
// `total_points` is nano points / 1e6, and the ordering compares the exact
// nano points.
const PointsLeaderboardNanoPointsPerPoint = 1_000_000

// PointsLeaderboardKey is the ordering key of one network.
type PointsLeaderboardKey struct {
	NanoPoints int64
	Blocks     int64
	Streak     int64
	NetworkId  string
}

// pointsLeaderboardDimensions returns the three dimensions of a sort in
// tie-break order: the sort's own dimension first. Unknown sorts order as
// "points".
func pointsLeaderboardDimensions(sort string) (first string, second string, third string) {
	switch sort {
	case PointsLeaderboardSortBlocks:
		return PointsLeaderboardSortBlocks, PointsLeaderboardSortStreak, PointsLeaderboardSortPoints
	case PointsLeaderboardSortStreak:
		return PointsLeaderboardSortStreak, PointsLeaderboardSortBlocks, PointsLeaderboardSortPoints
	default:
		return PointsLeaderboardSortPoints, PointsLeaderboardSortStreak, PointsLeaderboardSortBlocks
	}
}

func pointsLeaderboardKeyValue(key *PointsLeaderboardKey, dimension string) int64 {
	switch dimension {
	case PointsLeaderboardSortBlocks:
		return key.Blocks
	case PointsLeaderboardSortStreak:
		return key.Streak
	default:
		return key.NanoPoints
	}
}

// ComparePointsLeaderboardValues compares the three ranked values of two keys
// in the sort's order, every value descending: negative when a ranks ahead of
// b, positive when b ranks ahead, zero when all three values tie. Two networks
// share a competition rank exactly when this returns zero.
func ComparePointsLeaderboardValues(sort string, a *PointsLeaderboardKey, b *PointsLeaderboardKey) int {
	first, second, third := pointsLeaderboardDimensions(sort)
	for _, dimension := range []string{first, second, third} {
		va, vb := pointsLeaderboardKeyValue(a, dimension), pointsLeaderboardKeyValue(b, dimension)
		if va != vb {
			if vb < va {
				return -1
			}
			return 1
		}
	}
	return 0
}

// ComparePointsLeaderboardKeys is the total order: the values in the sort's
// order, then the network id ascending. It never returns zero for two
// different networks.
func ComparePointsLeaderboardKeys(sort string, a *PointsLeaderboardKey, b *PointsLeaderboardKey) int {
	if c := ComparePointsLeaderboardValues(sort, a, b); c != 0 {
		return c
	}
	switch {
	case a.NetworkId < b.NetworkId:
		return -1
	case b.NetworkId < a.NetworkId:
		return 1
	}
	return 0
}

// PointsLeaderboardKeyOf is a row's ordering key. `total_points` is nano
// points / 1e6 on the wire, so the exact nano points are recovered.
func PointsLeaderboardKeyOf(row *PointsLeaderboardRow) *PointsLeaderboardKey {
	key := &PointsLeaderboardKey{
		NanoPoints: int64(math.Round(row.TotalPoints * PointsLeaderboardNanoPointsPerPoint)),
		Blocks:     row.BlocksWithPoints,
		Streak:     row.Streak,
	}
	if row.NetworkId != nil {
		key.NetworkId = row.NetworkId.String()
	}
	return key
}

// ComparePointsLeaderboardRows orders two rows for a sort (see
// ComparePointsLeaderboardKeys).
func ComparePointsLeaderboardRows(sort string, a *PointsLeaderboardRow, b *PointsLeaderboardRow) int {
	return ComparePointsLeaderboardKeys(sort, PointsLeaderboardKeyOf(a), PointsLeaderboardKeyOf(b))
}

// sortPointsLeaderboardRows puts rows in the sort's order in place.
func sortPointsLeaderboardRows(sortBy string, rows []*PointsLeaderboardRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		return ComparePointsLeaderboardRows(sortBy, rows[i], rows[j]) < 0
	})
}
