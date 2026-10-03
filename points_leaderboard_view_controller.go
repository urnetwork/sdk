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

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/emoji"
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
// Every ranked network is listed; a row's `NetworkName` is set only when the
// server sent one (a network that revealed its name); otherwise `Anonymous` is
// true and the app shows its localized "Anonymous". The caller's own list row
// is no exception: it is anonymous until the network opts in, and the app only
// highlights it. The caller's own name travels on the `me` row, for the
// own-stats card above the list. `EmojiTag` shows either way.
//
// Every method is safe for concurrent use. Listeners are called with the
// state lock released, so a listener may call back into the controller
// (LoadMore from a change callback is the expected pattern).
//
// One page is in flight at a time. The in-flight slot is claimed inside the
// same locked scope that checks it (Start, SetSort, LoadMore, Refresh), never
// after the check: a check-then-claim split let two LoadMore calls both pass
// the "not loading" test and request the same cursor twice, appending the
// page twice.
type PointsLeaderboardViewController struct {
	ctx    context.Context
	cancel context.CancelFunc

	device Device
	// api-only (NewPointsLeaderboardViewControllerWithApi): no device, the
	// same controller over the network space api. Exactly one of device / api.
	api *Api

	stateLock sync.Mutex

	sort string
	// the loaded window of the sort's total order, contiguous and in order,
	// keyed by row position so a prepended or appended page never repeats a
	// row (see mergePointsLeaderboardRows)
	rows []*PointsLeaderboardRow
	// the cursor of the next page; empty once the end is reached
	nextCursor string
	// the cursor of the page before the window; empty when the window starts
	// at the top
	prevCursor string
	endReached bool
	loading    bool
	// bumped by SetSort and Refresh so a response to a request from before
	// the change is dropped
	generation int
	started    bool

	me                    *PointsLeaderboardMe
	totalRanked           int64
	latestEpoch           int64
	epochMetricsKnown     bool
	epochMetricsAvailable bool
	snapshotTime          *Time
	errorMessage          string

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
	self.loading = true
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
	if sort != PointsLeaderboardSortPoints && self.epochMetricsKnown && !self.epochMetricsAvailable {
		self.stateLock.Unlock()
		// Some native adapters optimistically mirror a tapped sort before they
		// call us. Re-emit the authoritative state so they immediately return to
		// points instead of displaying an unavailable sort as selected.
		self.pointsLeaderboardChanged()
		return
	}
	if sort == self.sort {
		self.stateLock.Unlock()
		return
	}
	self.sort = sort
	self.generation += 1
	self.rows = nil
	self.nextCursor = ""
	self.prevCursor = ""
	self.endReached = false
	self.errorMessage = ""
	started := self.started
	// the new order's first page is claimed here; a stale page of the old
	// order is dropped by the generation
	self.loading = started
	self.stateLock.Unlock()

	if started {
		self.fetch("", true)
	} else {
		self.pointsLeaderboardChanged()
	}
}

// ReloadFromTop drops the loaded window and loads the first page of the
// current sort again: the way back from a seek (or a tab tap that scrolls to
// the top). Unlike Refresh the rows are cleared at once, so the list never
// shows a window that starts mid-list next to a top-of-list scroll position.
func (self *PointsLeaderboardViewController) ReloadFromTop() {
	self.seek(0)
}

// SeekToRank jumps the loaded window to the page holding the given 1-based
// position of the current sort's total order (the scroll indicator's rank).
// An in-flight page is cancelled, the rows are cleared, and the page at the
// rank lands as the new window; the app then pages backward with
// LoadMoreBefore and forward with LoadMore from there. The server clamps the
// rank to [1, total ranked]; a rank below 1 reloads from the top.
func (self *PointsLeaderboardViewController) SeekToRank(rank int) {
	if rank < 1 {
		rank = 0
	}
	self.seek(int64(rank))
}

func (self *PointsLeaderboardViewController) seek(rank int64) {
	self.stateLock.Lock()
	self.generation += 1
	self.rows = nil
	self.nextCursor = ""
	self.prevCursor = ""
	self.endReached = false
	self.errorMessage = ""
	self.started = true
	self.loading = true
	self.stateLock.Unlock()

	self.fetchPage(pointsLeaderboardRequest{seekRank: rank, mode: pointsLeaderboardReplace})
}

// LoadMore fetches the next page. It is a no-op while a page is loading and
// once the end is reached; after an error it retries the same page.
func (self *PointsLeaderboardViewController) LoadMore() {
	self.stateLock.Lock()
	if !self.started || self.loading || self.endReached {
		self.stateLock.Unlock()
		return
	}
	// claim the slot in the scope that checked it
	self.loading = true
	cursor := self.nextCursor
	self.stateLock.Unlock()

	if pointsLeaderboardTestBeforeFetch != nil {
		pointsLeaderboardTestBeforeFetch()
	}
	self.fetch(cursor, cursor == "")
}

// LoadMoreBefore fetches the page before the loaded window and prepends it.
// It is a no-op while a page is loading and when the window already starts
// at the top (HasMoreBefore is false); after an error it retries the same
// page.
func (self *PointsLeaderboardViewController) LoadMoreBefore() {
	self.stateLock.Lock()
	if !self.started || self.loading || self.prevCursor == "" {
		self.stateLock.Unlock()
		return
	}
	self.loading = true
	cursor := self.prevCursor
	self.stateLock.Unlock()

	if pointsLeaderboardTestBeforeFetch != nil {
		pointsLeaderboardTestBeforeFetch()
	}
	self.fetchPage(pointsLeaderboardRequest{cursor: cursor, mode: pointsLeaderboardPrepend})
}

// HasMoreBefore is true while there are ranks above the loaded window (the
// window does not start at rank 1), so LoadMoreBefore has a page to fetch.
func (self *PointsLeaderboardViewController) HasMoreBefore() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.prevCursor != ""
}

// HasMoreAfter is true while there are ranks below the loaded window, so
// LoadMore has a page to fetch. It is the negation of IsEndReached.
func (self *PointsLeaderboardViewController) HasMoreAfter() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return !self.endReached
}

// FirstLoadedPosition is the 1-based position (in the current sort's total
// order) of the first loaded row, 0 while no row is loaded.
func (self *PointsLeaderboardViewController) FirstLoadedPosition() int64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(self.rows) == 0 {
		return 0
	}
	return self.rows[0].Position
}

// LastLoadedPosition is the 1-based position of the last loaded row, 0 while
// no row is loaded.
func (self *PointsLeaderboardViewController) LastLoadedPosition() int64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(self.rows) == 0 {
		return 0
	}
	return self.rows[len(self.rows)-1].Position
}

// TotalRanked is GetTotalRanked under the name the indicator math reads.
func (self *PointsLeaderboardViewController) TotalRanked() int64 {
	return self.GetTotalRanked()
}

// pointsLeaderboardTestBeforeFetch is a test barrier between LoadMore's claim
// and its request, to prove a second LoadMore in that gap is refused.
var pointsLeaderboardTestBeforeFetch func()

// Refresh re-fetches the first page of the current sort. The rows stay in
// place until the new page lands, then it replaces them.
func (self *PointsLeaderboardViewController) Refresh() {
	self.stateLock.Lock()
	self.generation += 1
	self.loading = true
	self.errorMessage = ""
	self.started = true
	self.stateLock.Unlock()

	self.fetch("", true)
}

// pointsLeaderboardMode is what a landed page does to the loaded window.
type pointsLeaderboardMode int

const (
	// the page becomes the window
	pointsLeaderboardReplace pointsLeaderboardMode = iota
	// the page follows the window
	pointsLeaderboardAppend
	// the page precedes the window
	pointsLeaderboardPrepend
)

// pointsLeaderboardRequest is one page request: a cursor (either direction),
// or a seek rank, or neither for the first page.
type pointsLeaderboardRequest struct {
	cursor   string
	seekRank int64
	mode     pointsLeaderboardMode
}

// fetch requests one forward page: the first page (`replace`) or the page
// after `cursor`. The caller has already claimed the in-flight slot.
func (self *PointsLeaderboardViewController) fetch(cursor string, replace bool) {
	mode := pointsLeaderboardAppend
	if replace {
		mode = pointsLeaderboardReplace
	}
	self.fetchPage(pointsLeaderboardRequest{cursor: cursor, mode: mode})
}

// fetchPage requests one page. The caller has already claimed the in-flight
// slot (`loading`) under the lock; this only snapshots what the request needs
// and announces the loading state.
func (self *PointsLeaderboardViewController) fetchPage(request pointsLeaderboardRequest) {
	self.stateLock.Lock()
	self.errorMessage = ""
	generation := self.generation
	sort := self.sort
	self.stateLock.Unlock()

	self.pointsLeaderboardChanged()

	args := &GetPointsLeaderboardArgs{
		Sort:     sort,
		Cursor:   request.cursor,
		SeekRank: request.seekRank,
		Limit:    PointsLeaderboardPageSize,
	}
	self.getApi().GetPointsLeaderboard(args, GetPointsLeaderboardCallback(connect.NewApiCallback[*PointsLeaderboardResult](
		func(result *PointsLeaderboardResult, err error) {
			self.handlePage(generation, request, result, err)
		},
	)))
}

func (self *PointsLeaderboardViewController) handlePage(
	generation int,
	request pointsLeaderboardRequest,
	result *PointsLeaderboardResult,
	err error,
) {
	cursor := request.cursor
	if self.ctx.Err() != nil {
		// closed while the page was in flight
		return
	}
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
		if cursor == "" && request.seekRank == 0 {
			// a restart on a fresh page: the server has nothing to page
			self.loading = false
			self.endReached = true
			self.stateLock.Unlock()

			self.pointsLeaderboardChanged()
			return
		}
		// the snapshot behind the cursor is gone: reload from the top (a
		// seek too: its positions belonged to the old snapshot)
		self.rows = nil
		self.nextCursor = ""
		self.prevCursor = ""
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
			formatPointsLeaderboardRow(row, result.EpochMetricsAvailable)
			page = append(page, row)
		}
	}
	switch request.mode {
	case pointsLeaderboardReplace:
		self.rows = page
		self.nextCursor = result.NextCursor
		self.prevCursor = result.PrevCursor
		self.endReached = result.NextCursor == "" || len(page) == 0
	case pointsLeaderboardAppend:
		self.rows = mergePointsLeaderboardRows(self.rows, page)
		self.nextCursor = result.NextCursor
		self.endReached = result.NextCursor == "" || len(page) == 0
	case pointsLeaderboardPrepend:
		self.rows = mergePointsLeaderboardRows(page, self.rows)
		self.prevCursor = result.PrevCursor
	}
	// the rows are always in the sort's order (see ComparePointsLeaderboardRows),
	// whatever order the pages arrived in
	sortPointsLeaderboardRows(self.sort, self.rows)
	self.loading = false
	self.errorMessage = ""
	if result.Me != nil {
		formatPointsLeaderboardRow(result.Me.Row, result.EpochMetricsAvailable)
	}
	if request.mode == pointsLeaderboardReplace || result.Me != nil {
		self.me = result.Me
	}
	self.totalRanked = result.TotalRanked
	self.latestEpoch = result.LatestEpoch
	self.epochMetricsKnown = true
	self.epochMetricsAvailable = result.EpochMetricsAvailable
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

// GetEpochMetricsAvailable reports whether blocks and streaks came from at
// least one legitimate finalized epoch. Total points remain available when
// this is false.
func (self *PointsLeaderboardViewController) GetEpochMetricsAvailable() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.epochMetricsAvailable
}

// GetSnapshotTime is when the ranks were computed, nil before the first page.
func (self *PointsLeaderboardViewController) GetSnapshotTime() *Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.snapshotTime
}

// formatPointsLeaderboardRow fills the preformatted text fields of a row.
func formatPointsLeaderboardRow(row *PointsLeaderboardRow, epochMetricsAvailable bool) {
	if row == nil {
		return
	}
	// the server omits the name of an anonymous list row (the caller's own
	// row included) and sends the caller's own name only on `me`, so the name
	// is shown whenever it was sent: the own-stats card names the network while
	// its list row reads Anonymous like everyone else's. Blanking the name on
	// `Anonymous` alone hid the name on `me`.
	row.DisplayName = row.NetworkName
	row.TotalPointsText = FormatPoints(row.TotalPoints)
	row.RankPointsText = FormatRank(row.RankPoints)
	if epochMetricsAvailable {
		row.BlocksWithPointsText = fmt.Sprintf("%d", row.BlocksWithPoints)
		row.StreakText = fmt.Sprintf("%d", row.Streak)
		row.LongestStreakText = fmt.Sprintf("%d", row.LongestStreak)
		row.RankBlocksText = FormatRank(row.RankBlocks)
		row.RankStreakText = FormatRank(row.RankStreak)
	} else {
		row.BlocksWithPointsText = "-"
		row.StreakText = "-"
		row.LongestStreakText = "-"
		row.RankBlocksText = "-"
		row.RankStreakText = "-"
	}
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

/**
 * Loaded window
 */

// mergePointsLeaderboardRows joins two runs of rows, `before` then `after`,
// dropping from `after` every row `before` already holds. Rows are keyed by
// their position in the sort's total order; a row without a position (an
// older server) falls back to its network id. The result is the loaded
// window: a page prepended or appended twice (a retried request, a page
// that overlaps the window's edge) never repeats a row.
func mergePointsLeaderboardRows(before []*PointsLeaderboardRow, after []*PointsLeaderboardRow) []*PointsLeaderboardRow {
	if len(before) == 0 {
		return after
	}
	if len(after) == 0 {
		return before
	}
	seen := map[string]bool{}
	for _, row := range before {
		seen[pointsLeaderboardRowKey(row)] = true
	}
	out := make([]*PointsLeaderboardRow, 0, len(before)+len(after))
	out = append(out, before...)
	for _, row := range after {
		key := pointsLeaderboardRowKey(row)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, row)
	}
	return out
}

func pointsLeaderboardRowKey(row *PointsLeaderboardRow) string {
	if row.Position > 0 {
		return fmt.Sprintf("p:%d", row.Position)
	}
	if row.NetworkId != nil {
		return "n:" + row.NetworkId.String()
	}
	return "n:"
}

/**
 * Scroll indicator label
 */

// The tier of a rank among the ranked networks, for the scroll indicator's
// "#1,240 · Top 5%" label. The app maps the tier to its localized string;
// the sdk never renders the words.
const (
	// no ranking to place the rank in (nothing ranked yet)
	PointsLeaderboardTierUnknown = 0
	PointsLeaderboardTierTop1    = 1
	PointsLeaderboardTierTop5    = 2
	PointsLeaderboardTierTop10   = 3
	PointsLeaderboardTierTop25   = 4
	PointsLeaderboardTierTop50   = 5
	// the lower half
	PointsLeaderboardTierRest = 6
)

// pointsLeaderboardTierPercents are the tier thresholds, top-first: a rank is
// in a tier when it is within the tier's percent of the total, rounded up,
// so the leader is always in the top 1% and a list of two has a top half.
var pointsLeaderboardTierPercents = []struct {
	tier    int
	percent int
}{
	{PointsLeaderboardTierTop1, 1},
	{PointsLeaderboardTierTop5, 5},
	{PointsLeaderboardTierTop10, 10},
	{PointsLeaderboardTierTop25, 25},
	{PointsLeaderboardTierTop50, 50},
}

// PointsLeaderboardScrollLabelParts is the scroll indicator's label while it
// is dragged: the rank it points at (clamped to the ranking) and the tier
// the rank is in. `RankText` is the rank preformatted ("#1,240"); `Tier` is
// one of the PointsLeaderboardTier* values and `TierPercent` its percent
// (1, 5, 10, 25, 50; 0 for the rest and the unknown tier).
type PointsLeaderboardScrollLabelParts struct {
	Rank        int64  `json:"rank"`
	Total       int64  `json:"total"`
	RankText    string `json:"rank_text"`
	Tier        int    `json:"tier"`
	TierPercent int    `json:"tier_percent"`
}

// PointsLeaderboardScrollLabel is the label of the scroll indicator at a
// rank among `total` ranked networks. It is pure: the app calls it on every
// drag move with the rank the indicator's position maps to. The rank is
// clamped to [1, total]; with nothing ranked the tier is unknown.
func PointsLeaderboardScrollLabel(rank int64, total int64) *PointsLeaderboardScrollLabelParts {
	if total < 0 {
		total = 0
	}
	if rank < 1 {
		rank = 1
	}
	if 0 < total && total < rank {
		rank = total
	}
	parts := &PointsLeaderboardScrollLabelParts{
		Rank:     rank,
		Total:    total,
		RankText: FormatRank(rank),
		Tier:     PointsLeaderboardTierUnknown,
	}
	if total == 0 {
		return parts
	}
	parts.Tier = PointsLeaderboardTierRest
	for _, tier := range pointsLeaderboardTierPercents {
		// the tier holds ceil(total * percent / 100) ranks
		if rank <= (total*int64(tier.percent)+99)/100 {
			parts.Tier = tier.tier
			parts.TierPercent = tier.percent
			break
		}
	}
	return parts
}

// GetScrollLabel is PointsLeaderboardScrollLabel over the controller's total
// ranked count, for bindings without free functions.
func (self *PointsLeaderboardViewController) GetScrollLabel(rank int64) *PointsLeaderboardScrollLabelParts {
	return PointsLeaderboardScrollLabel(rank, self.GetTotalRanked())
}
