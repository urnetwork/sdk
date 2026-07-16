package sdk

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// How long a contract's slot (its transfer path) is held after the contract
// disappears, waiting to see if a renewal opens on the same path. Within this
// window a close-then-open renewal is presented as one atomic replace rather
// than a bouncy close (ring ejects) followed by a fresh open. If nothing
// reopens, the slot is dropped and the row begins ejecting.
const contractRenewalWindow = 750 * time.Millisecond

// How long a client row lingers (Closing = true) after its last contract is
// truly gone, so the UI can eject its circles before the row is removed.
const contractEjectWindow = 500 * time.Millisecond

// How often the controller re-checks renewal/eject deadlines while any are
// pending. Idle (no holds, no closing rows) it does not tick.
const contractDeadlineTick = 100 * time.Millisecond

// ContractRowsListener fires whenever the aggregated contract rows change (either
// list). The app re-reads GetClientContractRows / GetProviderContractRows.
type ContractRowsListener interface {
	ContractRowsChanged()
}

// ContractClientRow is one peer client's aggregated contract pair, ready to
// render: the summed usage of the client's contracts (egress = "contract",
// ingress = "companion"), a signature of the open contract ids (a change means a
// contract was replaced -> swap the circle), and a Closing flag (the client's
// last contract is gone and the row is ejecting).
type ContractClientRow struct {
	ClientId string

	// sorted-joined signatures of the client's active contract ids; a change
	// means a contract was replaced (swap the circle rather than resize it)
	ContractId          string
	CompanionContractId string

	ContractUsedByteCount ByteCount
	ContractByteCount     ByteCount
	ContractBitRate       int

	CompanionContractUsedByteCount ByteCount
	CompanionContractByteCount     ByteCount
	CompanionContractBitRate       int

	PairCount int

	// the client's last contract closed and the row is being ejected: kept
	// briefly so its circles slide off, then removed
	Closing bool
}

type ContractClientRowList struct {
	exportedList[*ContractClientRow]
}

func NewContractClientRowList() *ContractClientRowList {
	return &ContractClientRowList{
		exportedList: *newExportedList[*ContractClientRow](),
	}
}

// ContractDetailsViewController is the single, shared source of the aggregated
// contract-details rows for every app. It owns what each app used to do itself:
//   - coalescing the egress + ingress change streams into one settled recompute
//     (a single contract change fires both, so intermediate one-list-updated
//     states are never published -- this is what made the totals flicker);
//   - RENEWAL coalescing: holding a contract's transfer-path slot briefly so a
//     close-then-open renewal is one atomic replace instead of a bouncy gap;
//   - per-peer aggregation into ContractClientRow, skipping closed tombstones
//     (subtract the replaced) and summing open contracts (add the new);
//   - the closing lifecycle: a client whose last contract is gone lingers as a
//     Closing row for one eject window, then is removed; a contract reopening
//     before that cancels the removal.
//
// It serves both the client-traffic feed and the provider-traffic feed. Apps
// render the rows and animate; they no longer re-implement any of the above.
type ContractDetailsViewController struct {
	ctx    context.Context
	cancel context.CancelFunc

	device Device

	notify chan struct{}

	stateLock    sync.Mutex
	client       *contractRowAggregator
	provider     *contractRowAggregator
	clientRows   *ContractClientRowList
	providerRows *ContractClientRowList

	rowsListeners *connect.CallbackList[ContractRowsListener]
	subs          []Sub
}

func newContractDetailsViewController(ctx context.Context, device Device) *ContractDetailsViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &ContractDetailsViewController{
		ctx:           cancelCtx,
		cancel:        cancel,
		device:        device,
		notify:        make(chan struct{}, 1),
		client:        newContractRowAggregator(),
		provider:      newContractRowAggregator(),
		clientRows:    NewContractClientRowList(),
		providerRows:  NewContractClientRowList(),
		rowsListeners: connect.NewCallbackList[ContractRowsListener](),
	}
	return vc
}

func (self *ContractDetailsViewController) Start() {
	listener := &contractDetailsViewControllerListener{self}
	self.subs = append(self.subs,
		self.device.AddEgressContractDetailsChangeListener(listener),
		self.device.AddIngressContractDetailsChangeListener(listener),
		self.device.AddProviderEgressContractDetailsChangeListener(listener),
		self.device.AddProviderIngressContractDetailsChangeListener(listener),
	)
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

// run recomputes on each coalesced change, and on a low-frequency tick while any
// renewal hold or closing row still has a pending deadline (idle otherwise).
func (self *ContractDetailsViewController) run() {
	ticker := time.NewTicker(contractDeadlineTick)
	defer ticker.Stop()
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-self.notify:
			self.recompute()
		case <-ticker.C:
			if self.pendingDeadlines() {
				self.recompute()
			}
		}
	}
}

func (self *ContractDetailsViewController) pendingDeadlines() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.client.pending() || self.provider.pending()
}

func (self *ContractDetailsViewController) recompute() {
	now := time.Now()
	ownClientId := ""
	if id := self.device.GetClientId(); id != nil {
		ownClientId = id.String()
	}

	clientRows := self.client.update(
		self.device.GetEgressContractDetails(),
		self.device.GetIngressContractDetails(),
		ownClientId, now,
	)
	providerRows := self.provider.update(
		self.device.GetProviderEgressContractDetails(),
		self.device.GetProviderIngressContractDetails(),
		ownClientId, now,
	)

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.clientRows = clientRows
		self.providerRows = providerRows
	}()

	self.rowsChanged()
}

func (self *ContractDetailsViewController) GetClientContractRows() *ContractClientRowList {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.clientRows
}

func (self *ContractDetailsViewController) GetProviderContractRows() *ContractClientRowList {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.providerRows
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

// ---- per-feed aggregation (client feed and provider feed each have one) -------

// a contract slot held after its contract disappeared, waiting for a renewal on
// the same transfer path
type contractSlotHold struct {
	details  *ContractDetails
	deadline time.Time
}

type contractRowAggregator struct {
	clientOrder      map[string]int
	held             map[string]*contractSlotHold // key: transfer-path signature
	prevOpenPaths    map[string]bool
	openDetails      map[string]*ContractDetails // last open details per slot (hold snapshot)
	closingDeadlines map[string]time.Time        // clientId -> remove-at
	lastRows         map[string]*ContractClientRow
}

func newContractRowAggregator() *contractRowAggregator {
	return &contractRowAggregator{
		clientOrder:      map[string]int{},
		held:             map[string]*contractSlotHold{},
		prevOpenPaths:    map[string]bool{},
		openDetails:      map[string]*ContractDetails{},
		closingDeadlines: map[string]time.Time{},
		lastRows:         map[string]*ContractClientRow{},
	}
}

func (self *contractRowAggregator) pending() bool {
	return 0 < len(self.held) || 0 < len(self.closingDeadlines)
}

// rowAccumulator sums a client's contracts as they are merged in.
type rowAccumulator struct {
	row          *ContractClientRow
	contractIds  []string
	companionIds []string
}

func (self *contractRowAggregator) update(
	egress *ContractDetailsList,
	ingress *ContractDetailsList,
	ownClientId string,
	now time.Time,
) *ContractClientRowList {
	// 1. collect the OPEN contracts, keyed by transfer-path slot (skip closed
	//    tombstones -- those are contracts leaving; a renewal shows up as a new
	//    open contract on the same path)
	currentOpen := map[string]*ContractDetails{}
	collect := func(list *ContractDetailsList) {
		if list == nil {
			return
		}
		for i := 0; i < list.Len(); i += 1 {
			details := list.Get(i)
			if details == nil || details.Status == ContractStatusClosed {
				continue
			}
			currentOpen[contractSlotKey(details)] = details
		}
	}
	collect(egress)
	collect(ingress)

	// 2. renewal holds: a slot present last time but gone now is held for the
	//    renewal window; a slot present now cancels any hold on it; expired holds
	//    are dropped
	for path := range self.prevOpenPaths {
		if _, ok := currentOpen[path]; !ok {
			if _, held := self.held[path]; !held {
				if last := self.openDetails[path]; last != nil {
					self.held[path] = &contractSlotHold{details: last, deadline: now.Add(contractRenewalWindow)}
				}
			}
		}
	}
	for path := range currentOpen {
		delete(self.held, path)
	}
	for path, hold := range self.held {
		if !now.Before(hold.deadline) {
			delete(self.held, path)
		}
	}

	// 3. the active contract set = open now + still-held (renewal pending)
	active := map[string]*ContractDetails{}
	for path, details := range currentOpen {
		active[path] = details
	}
	for path, hold := range self.held {
		if _, ok := active[path]; !ok {
			active[path] = hold.details
		}
	}
	// remember the current open slots for the next diff, and the last-seen open
	// details per slot (the snapshot a hold is created from). Rebuilt fresh each
	// cycle so it stays bounded to the current open set rather than growing.
	self.prevOpenPaths = map[string]bool{}
	self.openDetails = map[string]*ContractDetails{}
	for path, details := range currentOpen {
		self.prevOpenPaths[path] = true
		self.openDetails[path] = details
	}

	// 4. aggregate the active set per peer client
	accs := map[string]*rowAccumulator{}
	order := []string{}
	for _, details := range active {
		clientId := peerClientIdFromDetails(details, ownClientId)
		acc, ok := accs[clientId]
		if !ok {
			acc = &rowAccumulator{row: &ContractClientRow{ClientId: clientId}}
			accs[clientId] = acc
			order = append(order, clientId)
		}
		acc.row.ContractUsedByteCount += details.ContractUsedByteCount
		acc.row.ContractByteCount += details.ContractByteCount
		acc.row.ContractBitRate += details.ContractBitRate
		acc.row.CompanionContractUsedByteCount += details.CompanionContractUsedByteCount
		acc.row.CompanionContractByteCount += details.CompanionContractByteCount
		acc.row.CompanionContractBitRate += details.CompanionContractBitRate
		acc.row.PairCount += 1
		if details.ContractId != nil {
			acc.contractIds = append(acc.contractIds, details.ContractId.String())
		}
		if details.CompanionContractId != nil {
			acc.companionIds = append(acc.companionIds, details.CompanionContractId.String())
		}
	}

	// stable order: first-seen clients keep their slot, new clients append
	for _, clientId := range order {
		if _, ok := self.clientOrder[clientId]; !ok {
			self.clientOrder[clientId] = len(self.clientOrder)
		}
	}

	activeRows := map[string]*ContractClientRow{}
	for clientId, acc := range accs {
		row := acc.row
		row.ContractId = joinSorted(acc.contractIds)
		row.CompanionContractId = joinSorted(acc.companionIds)
		activeRows[clientId] = row
		self.lastRows[clientId] = row
	}

	// 5. closing lifecycle: a client that had a row but has no active contracts
	//    now lingers (Closing) for the eject window; a reappearing client cancels
	//    its removal
	for clientId := range activeRows {
		delete(self.closingDeadlines, clientId)
	}
	for clientId := range self.closingDeadlines {
		if _, ok := activeRows[clientId]; ok {
			delete(self.closingDeadlines, clientId)
		}
	}
	for clientId := range self.lastRows {
		if _, ok := activeRows[clientId]; ok {
			continue
		}
		if _, closing := self.closingDeadlines[clientId]; !closing {
			self.closingDeadlines[clientId] = now.Add(contractEjectWindow)
		}
	}
	for clientId, deadline := range self.closingDeadlines {
		if !now.Before(deadline) {
			delete(self.closingDeadlines, clientId)
			delete(self.lastRows, clientId)
			delete(self.clientOrder, clientId)
		}
	}

	// 6. output: active rows + still-closing rows (their last snapshot, marked
	//    Closing), ordered by first-seen
	out := []*ContractClientRow{}
	for _, row := range activeRows {
		out = append(out, row)
	}
	for clientId := range self.closingDeadlines {
		if row, ok := self.lastRows[clientId]; ok {
			closingRow := *row
			closingRow.Closing = true
			out = append(out, &closingRow)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return self.clientOrder[out[i].ClientId] > self.clientOrder[out[j].ClientId]
	})

	rows := NewContractClientRowList()
	rows.addAll(out...)
	return rows
}

func contractSlotKey(details *ContractDetails) string {
	if details.ContractTransferPath != nil {
		p := details.ContractTransferPath
		return transferIdStr(p.SourceId) + ">" + transferIdStr(p.DestinationId) + "/" + transferIdStr(p.StreamId)
	}
	if details.ContractId != nil {
		return details.ContractId.String()
	}
	return ""
}

func transferIdStr(id *Id) string {
	if id == nil {
		return ""
	}
	return id.String()
}

// peerClientIdFromDetails resolves the peer end of the contract transfer path.
func peerClientIdFromDetails(details *ContractDetails, ownClientId string) string {
	if p := details.ContractTransferPath; p != nil {
		sourceId := transferIdStr(p.SourceId)
		destinationId := transferIdStr(p.DestinationId)
		if ownClientId != "" {
			if sourceId == ownClientId && destinationId != "" {
				return destinationId
			}
			if destinationId == ownClientId && sourceId != "" {
				return sourceId
			}
		}
		if destinationId != "" {
			return destinationId
		}
		if sourceId != "" {
			return sourceId
		}
	}
	if details.ContractId != nil {
		return details.ContractId.String()
	}
	return "unknown"
}

func joinSorted(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	sorted := make([]string, len(ids))
	copy(sorted, ids)
	sort.Strings(sorted)
	return strings.Join(sorted, ",")
}
