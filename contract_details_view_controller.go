package sdk

import (
	"context"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
)

// How long a peer row lingers (Closing = true) after its last contract is gone,
// so the UI can animate its remaining circles sliding off before the row is
// removed. A contract reopening for the peer before that cancels the removal.
const contractEjectWindow = 500 * time.Millisecond

// How often the controller re-checks eject deadlines while any are pending.
// Idle (no closing rows) it does not tick.
const contractDeadlineTick = 100 * time.Millisecond

// contractDetailsSettings holds the view controller's tunables.
type contractDetailsSettings struct {
	// RowsUpdateThrottle rate-limits how often the rows are recomputed and
	// published to the app. Contract stats change continuously (used bytes and
	// bit rates every stats epoch, across every open contract), but the app
	// animates smoothly between updates, so recomputing more than ~1/s just
	// burns CPU, RPC pulls, and full-list re-renders without looking any fresher.
	// Coalesced changes still recompute at most this often; pending eject
	// deadlines are always serviced promptly regardless (so closing rows animate
	// out on time).
	RowsUpdateThrottle time.Duration

	// ActivityWindow: a peer row counts as "active" -- and, while the list is at
	// the top, sorts above idle rows -- if any of its contracts moved bytes within
	// this window (judged against LastActivityMillis).
	ActivityWindow time.Duration

	// ResortCadence: how often the at-top ordering is re-evaluated, so a row ages
	// from active to idle on time even when no new contract data arrives. Only
	// re-emits when the order actually changes.
	ResortCadence time.Duration
}

func defaultContractDetailsSettings() *contractDetailsSettings {
	return &contractDetailsSettings{
		RowsUpdateThrottle: 1 * time.Second,
		ActivityWindow:     5 * time.Second,
		ResortCadence:      1 * time.Second,
	}
}

// ContractRowsListener fires whenever the contract rows change. The app re-reads
// GetContractRows (and PendingCount).
type ContractRowsListener interface {
	ContractRowsChanged()
}

// ContractEntry is one contract, un-aggregated: its own used/total byte counts
// and bit rate. Contracts are never paired -- the send and receive contracts of
// a peer are fundamentally many-to-many, so each is presented on its own.
type ContractEntry struct {
	ContractId     string
	UsedByteCount  ByteCount
	TotalByteCount ByteCount
	BitRate        int

	// HasStream is true when the contract carries a (non-zero) stream id in its
	// transfer path, i.e. it is a stream contract rather than a direct one. The
	// app renders stream contracts distinctly (a double concentric outer ring).
	HasStream bool
}

type ContractEntryList struct {
	exportedList[*ContractEntry]
}

func NewContractEntryList() *ContractEntryList {
	return &ContractEntryList{
		exportedList: *newExportedList[*ContractEntry](),
	}
}

// ContractPeerRow is one peer client's open contracts, as two independent
// stacks: contracts sending to the peer and contracts receiving from it, each
// newest first. No cross-direction aggregation or pairing is done; the only
// derived values are the per-direction bit rate sums for the stack headers.
type ContractPeerRow struct {
	ClientId string

	// newest first
	SendContracts    *ContractEntryList
	ReceiveContracts *ContractEntryList

	// SendByteCount / ReceiveByteCount are the cumulative bytes moved to / from
	// this peer in the CURRENT data run: they sum the actual bytes transferred
	// (per-contract deltas, so they survive renewals -- a contract closing and
	// another opening for the peer keeps counting) and reset to 0 once the peer
	// goes idle (no throughput for the activity window). This shows "how much did
	// this run move", which is far more useful at a glance than the instantaneous
	// rate it replaces.
	SendByteCount    ByteCount
	ReceiveByteCount ByteCount

	// LastActivityMillis is the unix-millis time this peer's contracts last moved
	// bytes (any contract in either stack had a positive bit rate). 0 if the peer
	// has not moved bytes since it appeared. The app uses it to float rows with
	// recent activity above idle ones; it is an absolute timestamp so the app can
	// judge freshness against its own clock (the view controller runs in-app, so
	// the clock is shared).
	LastActivityMillis int64

	// the peer's last contract closed and the row is being ejected: it is kept
	// briefly (with empty stacks) so the UI can animate the circles off, then
	// the row is removed
	Closing bool
}

type ContractPeerRowList struct {
	exportedList[*ContractPeerRow]
}

func NewContractPeerRowList() *ContractPeerRowList {
	return &ContractPeerRowList{
		exportedList: *newExportedList[*ContractPeerRow](),
	}
}

// ContractDetailsViewController is the shared source of one feed's per-contract
// rows for every app. It groups the feed's raw contracts by peer client into two
// per-direction stacks (send and receive), newest first; runs the closing
// lifecycle (a peer whose last contract is gone lingers as a Closing row for one
// eject window, then is removed); AND owns the display ordering so every app is
// identical: while the list is at the top it sorts rows with recent activity
// above idle ones (a stable partition); while scrolled away it freezes the
// membership and order so rows under the reader don't shift, reporting
// newly-arrived rows as a pending ("N new") count.
//
// It is single-feed: the client-traffic and provider-traffic lists are two
// instances of this same controller (OpenClientContractDetailsViewController /
// OpenProviderContractDetailsViewController) -- parallel state, identical logic.
// The app's only ordering responsibilities are to report its scroll position
// (SetAtTop) and render the ordered rows GetContractRows returns; the animation
// (closing/tetris, row moves) stays in the app as it is platform-specific.
//
// It deliberately does NOT aggregate, pair, or hold contracts: every contract is
// reported as itself, and a renewal is just one contract leaving and another
// arriving.
type ContractDetailsViewController struct {
	ctx    context.Context
	cancel context.CancelFunc

	device   Device
	provider bool // which feed: false = this device's client traffic, true = relayed provider traffic
	settings *contractDetailsSettings

	notify       chan struct{}
	resortNotify chan struct{}

	// at-top state, set by the app as it scrolls; drives ordering vs freeze
	atTop atomic.Bool

	// run-loop-only state (touched only by run()): the aggregator, its last output
	// (base rows: newest-first + closing lifecycle), the orderer, and the last
	// emitted order for change detection
	aggregator *contractPeerAggregator
	base       []*ContractPeerRow
	orderer    *contractRowOrderer
	lastOrder  []string

	// published state, read by the app getters: the FINAL ordered rows and the
	// pending ("N new") count
	stateLock sync.Mutex
	rows      *ContractPeerRowList
	pending   int

	rowsListeners *connect.CallbackList[ContractRowsListener]
	subs          []Sub
}

func newContractDetailsViewController(ctx context.Context, device Device, provider bool) *ContractDetailsViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	settings := defaultContractDetailsSettings()
	vc := &ContractDetailsViewController{
		ctx:           cancelCtx,
		cancel:        cancel,
		device:        device,
		provider:      provider,
		settings:      settings,
		notify:        make(chan struct{}, 1),
		resortNotify:  make(chan struct{}, 1),
		aggregator:    newContractPeerAggregator(settings.ActivityWindow),
		orderer:       newContractRowOrderer(settings),
		rows:          NewContractPeerRowList(),
		rowsListeners: connect.NewCallbackList[ContractRowsListener](),
	}
	// a fresh list starts at the top: order (don't freeze) until the app reports
	// otherwise. This is also the safe default for an app that never reports scroll
	// (it simply never freezes / shows a pending chip).
	vc.atTop.Store(true)
	return vc
}

func (self *ContractDetailsViewController) Start() {
	listener := &contractDetailsViewControllerListener{self}
	if self.provider {
		self.subs = append(self.subs,
			self.device.AddProviderEgressContractDetailsChangeListener(listener),
			self.device.AddProviderIngressContractDetailsChangeListener(listener),
		)
	} else {
		self.subs = append(self.subs,
			self.device.AddEgressContractDetailsChangeListener(listener),
			self.device.AddIngressContractDetailsChangeListener(listener),
		)
	}
	go self.run()
	self.scheduleUpdate()
}

func (self *ContractDetailsViewController) Stop() {}

func (self *ContractDetailsViewController) Close() {
	deviceLog(self.device).Info("[cdvc]close")

	self.cancel()
	for _, sub := range self.subs {
		if sub != nil {
			sub.Close()
		}
	}
	self.subs = nil
}

// scheduleUpdate coalesces a burst of listener callbacks (egress + ingress fire
// separately for one change) into a single recompute on the run loop.
func (self *ContractDetailsViewController) scheduleUpdate() {
	select {
	case self.notify <- struct{}{}:
	default:
		// an update is already queued; the coalesced recompute will pick up the
		// latest state
	}
}

// run recomputes coalesced changes at most once per RowsUpdateThrottle (contract
// stats change continuously and the app animates between updates, so a faster
// cadence just burns work), while still servicing pending eject deadlines
// promptly on the tick so closing rows animate out on time. Idle (no changes, no
// closing rows) it does nothing but a cheap tick check.
func (self *ContractDetailsViewController) run() {
	ticker := time.NewTicker(contractDeadlineTick)
	defer ticker.Stop()
	var lastRecompute time.Time
	var lastResort time.Time
	dirty := false
	resortRequested := false
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-self.notify:
			dirty = true
		case <-self.resortNotify:
			resortRequested = true
		case <-ticker.C:
		}

		now := time.Now()
		// rate-limit data-driven recomputes; a still-pending eject deadline
		// bypasses the throttle so the closing animation stays on time (closes
		// are per-peer and infrequent, so this does not defeat the throttle)
		throttled := !lastRecompute.IsZero() && now.Sub(lastRecompute) < self.settings.RowsUpdateThrottle
		switch {
		case (dirty && !throttled) || self.pendingDeadlines():
			// new contract data (or a closing deadline): refresh the base rows
			// (the aggregators, incl. RPC pulls) and re-order + notify
			self.recompute(now)
			lastRecompute = now
			lastResort = now
			dirty = false
			resortRequested = false
		case resortRequested || (!lastResort.IsZero() && self.settings.ResortCadence <= now.Sub(lastResort)):
			// no new data: re-order the cached base only (apply an at-top change,
			// or age active rows to idle), notifying only if the order changed
			self.applyOrder(now, false)
			lastResort = now
			resortRequested = false
		}
	}
}

func (self *ContractDetailsViewController) pendingDeadlines() bool {
	// the aggregator is run-loop-only state
	return self.aggregator.pending()
}

// recompute refreshes the base rows from the feed (the aggregator, including the
// device RPC pulls) and re-orders them. Called on new contract data.
func (self *ContractDetailsViewController) recompute(now time.Time) {
	// the peer end is resolved by direction (see peerClientIdFromDetails), so
	// this device's own client id is not needed here
	var egress, ingress *ContractDetailsList
	if self.provider {
		egress = self.device.GetProviderEgressContractDetails()
		ingress = self.device.GetProviderIngressContractDetails()
	} else {
		egress = self.device.GetEgressContractDetails()
		ingress = self.device.GetIngressContractDetails()
	}
	self.base = contractPeerRowsFromList(self.aggregator.update(egress, ingress, now))
	// data changed -> always notify
	self.applyOrder(now, true)
}

// applyOrder orders the cached base rows (active-above-idle while at the top,
// frozen membership+order while scrolled away) and publishes the result plus the
// pending count. It notifies listeners when force is set (new data, values
// changed) or when the ordered row sequence or the pending count changed -- so a
// bare resort tick with no reordering does not churn the UI.
func (self *ContractDetailsViewController) applyOrder(now time.Time, force bool) {
	ordered, pending := self.orderer.order(self.base, self.atTop.Load(), now)

	order := contractPeerRowIds(ordered)
	changed := force || !slices.Equal(order, self.lastOrder)
	self.lastOrder = order

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.pending != pending {
			changed = true
		}
		self.rows = contractPeerRowList(ordered)
		self.pending = pending
	}()

	if changed {
		self.rowsChanged()
	}
}

func (self *ContractDetailsViewController) GetContractRows() *ContractPeerRowList {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.rows
}

// SetAtTop reports whether the app's list is scrolled to the top. At the top the
// rows re-sort (active above idle); scrolled away the membership and order freeze
// and newly-arrived rows collect into the pending count.
func (self *ContractDetailsViewController) SetAtTop(atTop bool) {
	if self.atTop.Swap(atTop) != atTop {
		self.scheduleResort()
	}
}

// PendingCount is the number of newly-arrived rows not yet shown while the list
// is scrolled away from the top (0 while at the top) -- the count for the app's
// "N new" chip.
func (self *ContractDetailsViewController) PendingCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.pending
}

// scheduleResort wakes the run loop to re-order the cached base without a full
// (RPC) recompute -- used when the app's scroll position changes.
func (self *ContractDetailsViewController) scheduleResort() {
	select {
	case self.resortNotify <- struct{}{}:
	default:
	}
}

func (self *ContractDetailsViewController) AddContractRowsListener(listener ContractRowsListener) Sub {
	callbackId := self.rowsListeners.Add(listener)
	return newSub(func() {
		self.rowsListeners.Remove(callbackId)
	})
}

func (self *ContractDetailsViewController) rowsChanged() {
	for _, listener := range self.rowsListeners.Get() {
		connect.HandleError(func() {
			listener.ContractRowsChanged()
		})
	}
}

type contractDetailsViewControllerListener struct {
	vc *ContractDetailsViewController
}

func (self *contractDetailsViewControllerListener) ContractDetailsChanged(contractDetails *ContractDetails) {
	self.vc.scheduleUpdate()
}

// ---- per-feed ordering (at-top activity sort + scrolled-away freeze) ---------

// contractRowOrderer applies, for one feed, the at-top activity ordering and the
// scrolled-away freeze. While at the top it sorts rows with recent activity
// above idle ones -- a STABLE partition of the newest-first base, so two equally
// active rows never swap. While scrolled away it keeps the last shown order
// frozen (so rows under the reader don't shift) and reports newly-arrived rows
// as a pending count for the "N new" chip. State is touched only by the run loop.
type contractRowOrderer struct {
	settings *contractDetailsSettings
	// the client ids in shown order; frozen while scrolled away
	shownOrder []string
}

func newContractRowOrderer(settings *contractDetailsSettings) *contractRowOrderer {
	return &contractRowOrderer{settings: settings}
}

// order returns the base rows in display order plus the pending (not-yet-shown)
// count. atTop: re-sort active-above-idle and adopt as the shown order (pending
// 0). !atTop: resolve the frozen shown order to current rows, dropping any that
// closed, and count the leading run of new rows as pending.
func (self *contractRowOrderer) order(base []*ContractPeerRow, atTop bool, now time.Time) ([]*ContractPeerRow, int) {
	byId := make(map[string]*ContractPeerRow, len(base))
	for _, row := range base {
		byId[row.ClientId] = row
	}

	if atTop {
		// stable partition: active rows (in newest-first base order) then idle
		ordered := make([]*ContractPeerRow, 0, len(base))
		for _, row := range base {
			if self.isActive(row, now) {
				ordered = append(ordered, row)
			}
		}
		for _, row := range base {
			if !self.isActive(row, now) {
				ordered = append(ordered, row)
			}
		}
		self.shownOrder = contractPeerRowIds(ordered)
		return ordered, 0
	}

	// frozen: resolve the shown order to current rows, dropping any that closed
	shown := make(map[string]bool, len(self.shownOrder))
	resolved := make([]*ContractPeerRow, 0, len(self.shownOrder))
	for _, id := range self.shownOrder {
		if row, ok := byId[id]; ok {
			resolved = append(resolved, row)
			shown[id] = true
		}
	}
	if len(resolved) == 0 {
		// every frozen row has closed: fall back to the live rows so the list
		// can't get stuck empty behind the chip
		self.shownOrder = contractPeerRowIds(base)
		return base, 0
	}
	// pending = leading run of base rows not yet shown (base is newest-first, so
	// new rows are at the front)
	pending := 0
	for _, row := range base {
		if shown[row.ClientId] {
			break
		}
		pending += 1
	}
	return resolved, pending
}

func (self *contractRowOrderer) isActive(row *ContractPeerRow, now time.Time) bool {
	if row.LastActivityMillis <= 0 {
		return false
	}
	return now.UnixMilli()-row.LastActivityMillis < self.settings.ActivityWindow.Milliseconds()
}

func contractPeerRowsFromList(list *ContractPeerRowList) []*ContractPeerRow {
	if list == nil {
		return nil
	}
	rows := make([]*ContractPeerRow, 0, list.Len())
	for i := 0; i < list.Len(); i += 1 {
		rows = append(rows, list.Get(i))
	}
	return rows
}

func contractPeerRowList(rows []*ContractPeerRow) *ContractPeerRowList {
	list := NewContractPeerRowList()
	list.addAll(rows...)
	return list
}

func contractPeerRowIds(rows []*ContractPeerRow) []string {
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = row.ClientId
	}
	return ids
}

// ---- per-feed grouping (client feed and provider feed each have one) ---------

type contractPeerAggregator struct {
	// a peer counts as idle -- and its run total resets -- once it has moved no
	// bytes for this long. Matches the orderer's ActivityWindow so the run total
	// and the active/idle sort agree on when a run ends.
	idleWindow time.Duration

	// peer clientId -> first-seen sequence; rows render newest peer first
	clientOrder map[string]int
	orderNext   int
	// contractId -> first-seen sequence; stacks render newest contract first
	arrival     map[string]int
	arrivalNext int
	// peers whose last contract is gone, lingering as Closing rows until removed
	closingDeadlines map[string]time.Time
	// peer clientId -> last time any of its contracts moved bytes (positive bit
	// rate). Kept across recomputes so an idle peer keeps its last-active time;
	// pruned when the peer is removed.
	lastActivity map[string]time.Time

	// contractId -> last seen used byte count, so a per-recompute byte delta can
	// be summed into the peer's run total (a single contract's used count is
	// monotonic; the delta is the bytes it moved since the last recompute).
	// Pruned when the contract closes.
	lastUsed map[string]ByteCount
	// peer clientId -> cumulative bytes moved in the current run, per direction.
	// Accumulates deltas while the peer is active, resets to 0 when it goes idle;
	// pruned when the peer is removed.
	runSend    map[string]ByteCount
	runReceive map[string]ByteCount
}

func newContractPeerAggregator(idleWindow time.Duration) *contractPeerAggregator {
	return &contractPeerAggregator{
		idleWindow:       idleWindow,
		clientOrder:      map[string]int{},
		arrival:          map[string]int{},
		closingDeadlines: map[string]time.Time{},
		lastActivity:     map[string]time.Time{},
		lastUsed:         map[string]ByteCount{},
		runSend:          map[string]ByteCount{},
		runReceive:       map[string]ByteCount{},
	}
}

func (self *contractPeerAggregator) pending() bool {
	return 0 < len(self.closingDeadlines)
}

// one peer's stacks as they are collected
type peerStacks struct {
	send    []*ContractEntry
	receive []*ContractEntry
}

func (self *contractPeerAggregator) update(
	egress *ContractDetailsList,
	ingress *ContractDetailsList,
	now time.Time,
) *ContractPeerRowList {
	// 1. collect the OPEN contracts of each direction, grouped by peer client
	//    (skip Closed tombstones -- those are contracts leaving)
	stacks := map[string]*peerStacks{}
	openIds := map[string]bool{}
	newIds := []string{}
	collect := func(list *ContractDetailsList, receive bool) {
		if list == nil {
			return
		}
		for i := 0; i < list.Len(); i += 1 {
			details := list.Get(i)
			if details == nil || details.Status == ContractStatusClosed || details.ContractId == nil {
				continue
			}
			contractId := details.ContractId.String()
			openIds[contractId] = true
			if _, ok := self.arrival[contractId]; !ok {
				newIds = append(newIds, contractId)
			}
			clientId := peerClientIdFromDetails(details, receive)
			s, ok := stacks[clientId]
			if !ok {
				s = &peerStacks{}
				stacks[clientId] = s
			}
			entry := &ContractEntry{
				ContractId:     contractId,
				UsedByteCount:  details.ContractUsedByteCount,
				TotalByteCount: details.ContractByteCount,
				BitRate:        details.ContractBitRate,
				HasStream:      contractHasStream(details),
			}
			if receive {
				s.receive = append(s.receive, entry)
			} else {
				s.send = append(s.send, entry)
			}
		}
	}
	collect(egress, false)
	collect(ingress, true)

	// 2. assign arrival order to first-seen contracts. The feeds are built from
	//    map iteration, so contracts first seen in the same recompute are ordered
	//    by id for determinism; the order is stable from then on.
	sort.Strings(newIds)
	for _, contractId := range newIds {
		self.arrival[contractId] = self.arrivalNext
		self.arrivalNext += 1
	}
	// drop arrival order (and the used-byte delta baseline) for contracts no
	// longer open (ids never reopen)
	for contractId := range self.arrival {
		if !openIds[contractId] {
			delete(self.arrival, contractId)
		}
	}
	for contractId := range self.lastUsed {
		if !openIds[contractId] {
			delete(self.lastUsed, contractId)
		}
	}

	// 3. assign first-seen order to new peers (by id within one recompute, for
	//    determinism), so rows keep a stable newest-first order
	newPeers := []string{}
	for clientId := range stacks {
		if _, ok := self.clientOrder[clientId]; !ok {
			newPeers = append(newPeers, clientId)
		}
	}
	sort.Strings(newPeers)
	for _, clientId := range newPeers {
		self.clientOrder[clientId] = self.orderNext
		self.orderNext += 1
	}

	// 4. closing lifecycle: a peer that had a row but has no open contracts now
	//    lingers (Closing, empty stacks) for the eject window; a reappearing peer
	//    cancels its removal
	for clientId := range stacks {
		delete(self.closingDeadlines, clientId)
	}
	for clientId := range self.clientOrder {
		if _, ok := stacks[clientId]; ok {
			continue
		}
		if _, closing := self.closingDeadlines[clientId]; !closing {
			self.closingDeadlines[clientId] = now.Add(contractEjectWindow)
		}
	}
	for clientId, deadline := range self.closingDeadlines {
		if !now.Before(deadline) {
			delete(self.closingDeadlines, clientId)
			delete(self.clientOrder, clientId)
		}
	}
	// drop last-activity and the run total for peers that are fully gone (a
	// removed peer id never reopens; keep them while the peer lingers as a
	// Closing row)
	for clientId := range self.lastActivity {
		if _, ok := self.clientOrder[clientId]; !ok {
			delete(self.lastActivity, clientId)
		}
	}
	for clientId := range self.runSend {
		if _, ok := self.clientOrder[clientId]; !ok {
			delete(self.runSend, clientId)
			delete(self.runReceive, clientId)
		}
	}

	// 5. output: active rows (stacks newest first) + still-closing rows (empty
	//    stacks, marked Closing), ordered newest peer first
	out := []*ContractPeerRow{}
	for clientId, s := range stacks {
		row := &ContractPeerRow{
			ClientId:         clientId,
			SendContracts:    NewContractEntryList(),
			ReceiveContracts: NewContractEntryList(),
		}
		self.sortNewestFirst(s.send)
		self.sortNewestFirst(s.receive)
		row.SendContracts.addAll(s.send...)
		row.ReceiveContracts.addAll(s.receive...)

		// per direction: the instantaneous rate sum (only used to judge activity)
		// and the bytes moved since the last recompute (folded into the run total)
		sendBitRate, sendDelta := self.directionStep(s.send)
		receiveBitRate, receiveDelta := self.directionStep(s.receive)

		// any contract moving bytes marks the peer active now; an idle peer keeps
		// its last-active time so the app can age it out against its own clock
		if 0 < sendBitRate || 0 < receiveBitRate {
			self.lastActivity[clientId] = now
		}
		if t, ok := self.lastActivity[clientId]; ok {
			row.LastActivityMillis = t.UnixMilli()
		}

		// run total: once the peer has been idle for the window its run is over,
		// so reset to 0; otherwise accumulate the bytes moved this recompute. The
		// reset baseline (lastUsed) is advanced either way by directionStep, so a
		// resuming peer starts a fresh run from its next bytes, never double-counts.
		if self.isIdle(clientId, now) {
			self.runSend[clientId] = 0
			self.runReceive[clientId] = 0
		} else {
			self.runSend[clientId] += sendDelta
			self.runReceive[clientId] += receiveDelta
		}
		row.SendByteCount = self.runSend[clientId]
		row.ReceiveByteCount = self.runReceive[clientId]

		out = append(out, row)
	}
	for clientId := range self.closingDeadlines {
		row := &ContractPeerRow{
			ClientId:         clientId,
			SendContracts:    NewContractEntryList(),
			ReceiveContracts: NewContractEntryList(),
			Closing:          true,
		}
		if t, ok := self.lastActivity[clientId]; ok {
			row.LastActivityMillis = t.UnixMilli()
		}
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return self.clientOrder[out[i].ClientId] > self.clientOrder[out[j].ClientId]
	})

	rows := NewContractPeerRowList()
	rows.addAll(out...)
	return rows
}

func (self *contractPeerAggregator) sortNewestFirst(entries []*ContractEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		return self.arrival[entries[i].ContractId] > self.arrival[entries[j].ContractId]
	})
}

// directionStep sums a direction's instantaneous bit rate and the bytes it moved
// since the last recompute -- each contract's used-count delta -- advancing the
// per-contract baseline as it goes. A first-seen contract counts its whole used
// count (its baseline is 0), so a renewal (the old contract closing and a new one
// opening) continues the run rather than restarting it at the new contract's zero.
func (self *contractPeerAggregator) directionStep(entries []*ContractEntry) (bitRate int, delta ByteCount) {
	for _, entry := range entries {
		bitRate += entry.BitRate
		if prev := self.lastUsed[entry.ContractId]; entry.UsedByteCount > prev {
			delta += entry.UsedByteCount - prev
		}
		self.lastUsed[entry.ContractId] = entry.UsedByteCount
	}
	return
}

// isIdle reports whether the peer has moved no bytes for at least the idle window,
// so its current run has ended and the run total should reset. A peer with no
// recorded activity is idle. This mirrors the orderer's active/idle boundary, so
// the run total resets exactly as a row ages from active to idle.
func (self *contractPeerAggregator) isIdle(clientId string, now time.Time) bool {
	last, ok := self.lastActivity[clientId]
	if !ok || last.IsZero() {
		return true
	}
	return self.idleWindow <= now.Sub(last)
}

func transferIdStr(id *Id) string {
	if id == nil {
		return ""
	}
	return id.String()
}

// contractHasStream reports whether the contract is a stream contract: its
// transfer path carries a non-zero stream id (mirrors connect.TransferPath's
// IsStream; note the SDK path always has a non-nil StreamId that is the zero id
// for a direct contract).
func contractHasStream(details *ContractDetails) bool {
	p := details.ContractTransferPath
	return p != nil && p.StreamId != nil && p.StreamId.id != [16]byte{}
}

// peerClientIdFromDetails resolves the peer end of the contract transfer path
// by DIRECTION, so a peer's send and receive contracts group under the same
// peer client id (and land in one row). This device is always one end of its
// own contract: on a receive (ingress) contract the peer is the SOURCE (the
// data comes from the peer to this device); on a send (egress) contract the
// peer is the DESTINATION. Resolving by direction is deterministic and does
// not depend on knowing this device's own client id -- which the previous
// implementation did, and its direction-blind fallback (always destination)
// put a peer's send under the peer but its receive under this device, so send
// and receive never shared a row.
func peerClientIdFromDetails(details *ContractDetails, receive bool) string {
	if p := details.ContractTransferPath; p != nil {
		if receive {
			if sourceId := transferIdStr(p.SourceId); sourceId != "" {
				return sourceId
			}
		} else {
			if destinationId := transferIdStr(p.DestinationId); destinationId != "" {
				return destinationId
			}
		}
	}
	if details.ContractId != nil {
		return details.ContractId.String()
	}
	return "unknown"
}
