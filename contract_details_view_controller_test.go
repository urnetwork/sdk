package sdk

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// testing_contractDetailsDevice is a minimal Device for driving the view
// controller's recompute path: it embeds the Device interface (so it satisfies
// all 110 methods) and overrides only the contract-details getters the recompute
// reads. Any other method would panic if called -- the recompute-driven tests
// never reach them.
type testing_contractDetailsDevice struct {
	Device
	egress  *ContractDetailsList
	ingress *ContractDetailsList
}

func (self *testing_contractDetailsDevice) GetEgressContractDetails() *ContractDetailsList {
	return self.egress
}

func (self *testing_contractDetailsDevice) GetIngressContractDetails() *ContractDetailsList {
	return self.ingress
}

type testing_contractRowsListener struct {
	onChange func()
}

func (self *testing_contractRowsListener) ContractRowsChanged() {
	self.onChange()
}

func testing_peerContractDetails(
	contractId connect.Id,
	sourceId connect.Id,
	destinationId connect.Id,
	streamId connect.Id,
	used ByteCount,
	total ByteCount,
	bitRate int,
	status string,
) *ContractDetails {
	return &ContractDetails{
		ContractId:            newId(contractId),
		ContractUsedByteCount: used,
		ContractByteCount:     total,
		ContractBitRate:       bitRate,
		ContractTransferPath: fromConnect(connect.TransferPath{
			SourceId:      sourceId,
			DestinationId: destinationId,
			StreamId:      streamId,
		}),
		Status: status,
	}
}

func testing_rowForClient(rows *ContractPeerRowList, clientId string) *ContractPeerRow {
	for i := 0; i < rows.Len(); i += 1 {
		if row := rows.Get(i); row.ClientId == clientId {
			return row
		}
	}
	return nil
}

// TestContractPeerAggregatorDirectionPairing pins the peer resolution: a
// peer's send and receive contracts land in ONE row, resolved by direction
// (receive -> source, send -> destination), regardless of what this device's
// own end is. The send and receive paths here use DIFFERENT local ends
// (usSend, usReceive) -- as the provider feed can, where the self id differs
// from the client feed -- and the peer must still pair. The old
// own-client-id-dependent resolution split these into two rows.
func TestContractPeerAggregatorDirectionPairing(t *testing.T) {
	agg := newContractPeerAggregator(5 * time.Second)
	now := time.Now()

	usSend := connect.NewId()
	usReceive := connect.NewId()
	peer := connect.NewId()
	stream := connect.NewId()

	egress := NewContractDetailsList()
	egress.Add(testing_peerContractDetails(connect.NewId(), usSend, peer, stream, 1000, 10000, 100, ContractStatusOpen))
	ingress := NewContractDetailsList()
	ingress.Add(testing_peerContractDetails(connect.NewId(), peer, usReceive, stream, 500, 5000, 50, ContractStatusOpen))

	rows := agg.update(egress, ingress, now)
	if rows.Len() != 1 {
		t.Fatalf("expected the peer's send and receive in ONE row, got %d rows", rows.Len())
	}
	row := testing_rowForClient(rows, peer.String())
	if row == nil {
		t.Fatalf("expected a row keyed by the peer id")
	}
	if row.SendContracts.Len() != 1 || row.ReceiveContracts.Len() != 1 {
		t.Fatalf("expected the peer row to hold both directions, got send=%d receive=%d",
			row.SendContracts.Len(), row.ReceiveContracts.Len())
	}
}

// TestContractPeerAggregatorStacks pins the un-aggregated shape: each peer's
// contracts are two independent newest-first stacks, never paired or summed
// (only the per-direction bit rates are summed for the stack headers).
func TestContractPeerAggregatorStacks(t *testing.T) {
	agg := newContractPeerAggregator(5 * time.Second)
	now := time.Now()

	us := connect.NewId()
	peer1 := connect.NewId()
	peer2 := connect.NewId()
	stream := connect.NewId()

	send1 := connect.NewId()
	send2 := connect.NewId()
	send3 := connect.NewId()
	receive1 := connect.NewId()

	egress := NewContractDetailsList()
	egress.Add(testing_peerContractDetails(send1, us, peer1, stream, 1000, 10000, 100, ContractStatusOpen))
	egress.Add(testing_peerContractDetails(send2, us, peer1, stream, 2000, 20000, 200, ContractStatusOpen))
	egress.Add(testing_peerContractDetails(send3, us, peer2, stream, 3000, 30000, 0, ContractStatusOpen))
	ingress := NewContractDetailsList()
	ingress.Add(testing_peerContractDetails(receive1, peer1, us, stream, 500, 5000, 50, ContractStatusOpen))

	rows := agg.update(egress, ingress, now)
	if rows.Len() != 2 {
		t.Fatalf("expected 2 peer rows, got %d", rows.Len())
	}

	row1 := testing_rowForClient(rows, peer1.String())
	if row1 == nil {
		t.Fatalf("expected a row for peer1")
	}
	if row1.SendContracts.Len() != 2 || row1.ReceiveContracts.Len() != 1 {
		t.Fatalf("unexpected peer1 stacks %d/%d", row1.SendContracts.Len(), row1.ReceiveContracts.Len())
	}
	// run totals accumulate the bytes moved this recompute (each contract's used
	// count, from a 0 baseline): send = 1000 + 2000, receive = 500
	if row1.SendByteCount != 3000 || row1.ReceiveByteCount != 500 {
		t.Fatalf("unexpected peer1 run totals %d/%d (want 3000/500)", row1.SendByteCount, row1.ReceiveByteCount)
	}
	entry := row1.ReceiveContracts.Get(0)
	if entry.ContractId != receive1.String() || entry.UsedByteCount != 500 || entry.TotalByteCount != 5000 {
		t.Fatalf("unexpected receive entry %+v", entry)
	}

	row2 := testing_rowForClient(rows, peer2.String())
	if row2 == nil || row2.SendContracts.Len() != 1 || row2.ReceiveContracts.Len() != 0 {
		t.Fatalf("unexpected peer2 row")
	}

	// a contract arriving later stacks newest first, and its peer row moves
	// nothing -- peer order is stable by first seen
	send4 := connect.NewId()
	egress.Add(testing_peerContractDetails(send4, us, peer1, stream, 0, 40000, 0, ContractStatusOpen))
	rows = agg.update(egress, ingress, now.Add(100*time.Millisecond))
	row1 = testing_rowForClient(rows, peer1.String())
	if row1.SendContracts.Len() != 3 {
		t.Fatalf("expected 3 send contracts, got %d", row1.SendContracts.Len())
	}
	if row1.SendContracts.Get(0).ContractId != send4.String() {
		t.Fatalf("expected the newest contract first, got %s", row1.SendContracts.Get(0).ContractId)
	}

	// a new peer appears above the existing rows
	peer3 := connect.NewId()
	send5 := connect.NewId()
	egress.Add(testing_peerContractDetails(send5, us, peer3, stream, 0, 1000, 0, ContractStatusOpen))
	rows = agg.update(egress, ingress, now.Add(200*time.Millisecond))
	if rows.Len() != 3 || rows.Get(0).ClientId != peer3.String() {
		t.Fatalf("expected the new peer first")
	}
}

// TestContractPeerAggregatorClosing pins the closing lifecycle: Closed
// tombstones never appear in the stacks; a peer whose last contract is gone
// lingers as a Closing row (empty stacks) for the eject window, is removed
// after it, and a reappearing contract cancels the removal.
func TestContractPeerAggregatorClosing(t *testing.T) {
	agg := newContractPeerAggregator(5 * time.Second)
	now := time.Now()

	us := connect.NewId()
	peer := connect.NewId()
	stream := connect.NewId()
	send1 := connect.NewId()

	egress := NewContractDetailsList()
	egress.Add(testing_peerContractDetails(send1, us, peer, stream, 1000, 10000, 0, ContractStatusOpen))
	rows := agg.update(egress, NewContractDetailsList(), now)
	if rows.Len() != 1 || rows.Get(0).Closing {
		t.Fatalf("expected 1 active row")
	}

	// the contract closes: its tombstone is not a stack entry, and the peer row
	// lingers as Closing
	closedEgress := NewContractDetailsList()
	closedEgress.Add(testing_peerContractDetails(send1, us, peer, stream, 5000, 10000, 0, ContractStatusClosed))
	rows = agg.update(closedEgress, NewContractDetailsList(), now.Add(100*time.Millisecond))
	if rows.Len() != 1 {
		t.Fatalf("expected the closing row, got %d", rows.Len())
	}
	row := rows.Get(0)
	if !row.Closing || row.SendContracts.Len() != 0 || row.ReceiveContracts.Len() != 0 {
		t.Fatalf("expected an empty closing row, got %+v", row)
	}

	// a new contract for the peer before the eject deadline cancels the removal
	send2 := connect.NewId()
	reopened := NewContractDetailsList()
	reopened.Add(testing_peerContractDetails(send2, us, peer, stream, 0, 10000, 0, ContractStatusOpen))
	rows = agg.update(reopened, NewContractDetailsList(), now.Add(200*time.Millisecond))
	if rows.Len() != 1 || rows.Get(0).Closing {
		t.Fatalf("expected the row active again")
	}
	if rows.Get(0).SendContracts.Len() != 1 || rows.Get(0).SendContracts.Get(0).ContractId != send2.String() {
		t.Fatalf("expected the reopened contract")
	}

	// the contract goes away and the eject window passes: the row is removed
	rows = agg.update(NewContractDetailsList(), NewContractDetailsList(), now.Add(300*time.Millisecond))
	if rows.Len() != 1 || !rows.Get(0).Closing {
		t.Fatalf("expected the closing row again")
	}
	rows = agg.update(NewContractDetailsList(), NewContractDetailsList(), now.Add(300*time.Millisecond+contractEjectWindow))
	if rows.Len() != 0 {
		t.Fatalf("expected the row removed, got %d", rows.Len())
	}
	if agg.pending() {
		t.Fatalf("expected no pending deadlines")
	}
}

// TestContractPeerAggregatorLastActivity pins the activity signal the app uses
// to float active rows above idle ones: a peer whose contracts are moving bytes
// (positive bit rate) reports LastActivityMillis at now; a peer that has never
// moved bytes reports 0; once a peer goes idle it keeps its last-active time (so
// the app ages it out on its own clock); and moving bytes again advances it.
func TestContractPeerAggregatorLastActivity(t *testing.T) {
	agg := newContractPeerAggregator(5 * time.Second)
	t0 := time.Now()

	us := connect.NewId()
	active := connect.NewId()
	idle := connect.NewId()
	stream := connect.NewId()
	activeContract := connect.NewId()
	idleContract := connect.NewId()

	mk := func(used int, bitRate int, peer connect.Id, contract connect.Id) *ContractDetails {
		return testing_peerContractDetails(contract, us, peer, stream, ByteCount(used), 10000, bitRate, ContractStatusOpen)
	}

	// t0: active peer moving bytes, idle peer with a contract but no movement
	e0 := NewContractDetailsList()
	e0.Add(mk(1000, 100, active, activeContract))
	e0.Add(mk(0, 0, idle, idleContract))
	rows := agg.update(e0, NewContractDetailsList(), t0)
	if r := testing_rowForClient(rows, active.String()); r == nil || r.LastActivityMillis != t0.UnixMilli() {
		t.Fatalf("expected active LastActivityMillis=%d, got %+v", t0.UnixMilli(), r)
	}
	if r := testing_rowForClient(rows, idle.String()); r == nil || r.LastActivityMillis != 0 {
		t.Fatalf("expected idle LastActivityMillis=0, got %+v", r)
	}

	// t1: the active peer goes idle (bit rate 0) -> it keeps its last-active time
	// t0, NOT the later recompute time
	t1 := t0.Add(5 * time.Second)
	e1 := NewContractDetailsList()
	e1.Add(mk(2000, 0, active, activeContract))
	e1.Add(mk(0, 0, idle, idleContract))
	rows = agg.update(e1, NewContractDetailsList(), t1)
	if r := testing_rowForClient(rows, active.String()); r == nil || r.LastActivityMillis != t0.UnixMilli() {
		t.Fatalf("expected idle-now peer to keep t0=%d, got %+v", t0.UnixMilli(), r)
	}

	// t2: it moves bytes again -> last-active advances
	t2 := t0.Add(10 * time.Second)
	e2 := NewContractDetailsList()
	e2.Add(mk(3000, 200, active, activeContract))
	e2.Add(mk(0, 0, idle, idleContract))
	rows = agg.update(e2, NewContractDetailsList(), t2)
	if r := testing_rowForClient(rows, active.String()); r == nil || r.LastActivityMillis != t2.UnixMilli() {
		t.Fatalf("expected re-active LastActivityMillis=%d, got %+v", t2.UnixMilli(), r)
	}
}

// TestContractPeerAggregatorCloseOneOfMany pins the stack-update contract: a
// Closed tombstone for one contract of a multi-contract peer drops that
// contract from the stack (the departure the view animates), leaving the
// others, and does not put the row into Closing while others are open. If
// closed contracts accumulate in the UI, the tombstone is not arriving as
// Closed (upstream close-lifecycle), not a bug here.
func TestContractPeerAggregatorCloseOneOfMany(t *testing.T) {
	agg := newContractPeerAggregator(5 * time.Second)
	now := time.Now()
	us := connect.NewId()
	peer := connect.NewId()
	stream := connect.NewId()
	a := connect.NewId()
	b := connect.NewId()

	egress := NewContractDetailsList()
	egress.Add(testing_peerContractDetails(a, us, peer, stream, 1000, 10000, 100, ContractStatusOpen))
	egress.Add(testing_peerContractDetails(b, us, peer, stream, 2000, 20000, 200, ContractStatusOpen))
	rows := agg.update(egress, NewContractDetailsList(), now)
	if row := testing_rowForClient(rows, peer.String()); row == nil || row.SendContracts.Len() != 2 {
		t.Fatalf("expected 2 open send contracts")
	}

	// a closes -> Closed tombstone; the row must now hold only b
	closing := NewContractDetailsList()
	closing.Add(testing_peerContractDetails(a, us, peer, stream, 9000, 10000, 0, ContractStatusClosed))
	closing.Add(testing_peerContractDetails(b, us, peer, stream, 3000, 20000, 200, ContractStatusOpen))
	rows = agg.update(closing, NewContractDetailsList(), now.Add(100*time.Millisecond))
	row := testing_rowForClient(rows, peer.String())
	if row == nil || row.Closing {
		t.Fatalf("row should remain active while b is open")
	}
	if row.SendContracts.Len() != 1 || row.SendContracts.Get(0).ContractId != b.String() {
		t.Fatalf("closed contract not dropped: stack has %d (want only b)", row.SendContracts.Len())
	}
}

// TestContractRowOrdererAtTop pins the at-top ordering: rows with activity within
// the window sort above idle rows, as a STABLE partition of the newest-first base
// (equally-active rows keep base order), and pending is 0 at the top.
func TestContractRowOrdererAtTop(t *testing.T) {
	orderer := newContractRowOrderer(defaultContractDetailsSettings())
	now := time.Now()
	ms := now.UnixMilli()

	// base is newest-first: A (idle, >5s), B (active), C (active)
	base := []*ContractPeerRow{
		{ClientId: "A", LastActivityMillis: ms - 10_000},
		{ClientId: "B", LastActivityMillis: ms - 1_000},
		{ClientId: "C", LastActivityMillis: ms - 2_000},
	}
	ordered, pending := orderer.order(base, true, now)
	if pending != 0 {
		t.Fatalf("expected pending 0 at top, got %d", pending)
	}
	if got := contractPeerRowIds(ordered); !slices.Equal(got, []string{"B", "C", "A"}) {
		t.Fatalf("expected active-first [B C A], got %v", got)
	}
}

// TestContractRowOrdererFreezeAndPending pins the scrolled-away freeze: the shown
// order stays frozen, a newly-arrived row is not merged but counted as pending,
// and returning to the top merges + re-sorts it.
func TestContractRowOrdererFreezeAndPending(t *testing.T) {
	orderer := newContractRowOrderer(defaultContractDetailsSettings())
	now := time.Now()
	ms := now.UnixMilli()

	base := []*ContractPeerRow{
		{ClientId: "B", LastActivityMillis: ms - 1_000},
		{ClientId: "C", LastActivityMillis: ms - 2_000},
	}
	orderer.order(base, true, now) // establish shown order [B C] at the top

	// scroll away; D arrives at the front (newest-first)
	base2 := []*ContractPeerRow{
		{ClientId: "D", LastActivityMillis: ms},
		{ClientId: "B", LastActivityMillis: ms - 1_000},
		{ClientId: "C", LastActivityMillis: ms - 2_000},
	}
	ordered, pending := orderer.order(base2, false, now)
	if got := contractPeerRowIds(ordered); !slices.Equal(got, []string{"B", "C"}) {
		t.Fatalf("expected frozen [B C], got %v", got)
	}
	if pending != 1 {
		t.Fatalf("expected pending 1 (D), got %d", pending)
	}

	// back to top: D merges and all re-sort (all active -> newest-first)
	ordered, pending = orderer.order(base2, true, now)
	if pending != 0 || !slices.Equal(contractPeerRowIds(ordered), []string{"D", "B", "C"}) {
		t.Fatalf("expected [D B C] pending 0 at top, got %v pending %d", contractPeerRowIds(ordered), pending)
	}
}

// TestContractRowOrdererFreezeFallback pins the stuck-empty guard: if every frozen
// row closes while scrolled away, the orderer falls back to the live rows.
func TestContractRowOrdererFreezeFallback(t *testing.T) {
	orderer := newContractRowOrderer(defaultContractDetailsSettings())
	now := time.Now()

	orderer.order([]*ContractPeerRow{{ClientId: "B"}, {ClientId: "C"}}, true, now)

	base2 := []*ContractPeerRow{{ClientId: "E", LastActivityMillis: now.UnixMilli()}}
	ordered, pending := orderer.order(base2, false, now)
	if got := contractPeerRowIds(ordered); !slices.Equal(got, []string{"E"}) || pending != 0 {
		t.Fatalf("expected fallback [E] pending 0, got %v pending %d", got, pending)
	}
}

// TestContractPeerAggregatorHasStream pins the stream flag: a contract whose
// transfer path carries a non-zero stream id is marked HasStream (the app draws
// it as a double ring); a direct contract (zero stream id) is not.
func TestContractPeerAggregatorHasStream(t *testing.T) {
	agg := newContractPeerAggregator(5 * time.Second)
	now := time.Now()
	us := connect.NewId()
	peer := connect.NewId()
	stream := connect.NewId()

	streamContract := connect.NewId()
	directContract := connect.NewId()
	egress := NewContractDetailsList()
	egress.Add(testing_peerContractDetails(streamContract, us, peer, stream, 1000, 10000, 100, ContractStatusOpen))
	// a direct contract has the zero stream id
	egress.Add(testing_peerContractDetails(directContract, us, peer, connect.Id{}, 2000, 20000, 200, ContractStatusOpen))

	rows := agg.update(egress, NewContractDetailsList(), now)
	row := testing_rowForClient(rows, peer.String())
	if row == nil || row.SendContracts.Len() != 2 {
		t.Fatalf("expected the peer's 2 send contracts")
	}
	var streamEntry, directEntry *ContractEntry
	for i := 0; i < row.SendContracts.Len(); i += 1 {
		e := row.SendContracts.Get(i)
		switch e.ContractId {
		case streamContract.String():
			streamEntry = e
		case directContract.String():
			directEntry = e
		}
	}
	if streamEntry == nil || !streamEntry.HasStream {
		t.Fatalf("expected the stream contract to have HasStream=true, got %+v", streamEntry)
	}
	if directEntry == nil || directEntry.HasStream {
		t.Fatalf("expected the direct contract to have HasStream=false, got %+v", directEntry)
	}
}

// TestContractPeerAggregatorRunTotalAccumulates pins the basic run total: while a
// peer stays active, the row's Send/ReceiveByteCount climb with the actual bytes
// moved (the used-count deltas), not the instantaneous rate. For a single contract
// the run total equals its monotonic used count.
func TestContractPeerAggregatorRunTotalAccumulates(t *testing.T) {
	agg := newContractPeerAggregator(5 * time.Second)
	t0 := time.Now()
	us := connect.NewId()
	peer := connect.NewId()
	stream := connect.NewId()
	c := connect.NewId()

	mk := func(used ByteCount, bitRate int) *ContractDetailsList {
		e := NewContractDetailsList()
		e.Add(testing_peerContractDetails(c, us, peer, stream, used, 100000, bitRate, ContractStatusOpen))
		return e
	}

	// used climbs 1000 -> 3000 -> 6000 across active recomputes 1s apart
	steps := []struct {
		used ByteCount
		at   time.Duration
		want ByteCount
	}{
		{1000, 0, 1000},
		{3000, 1 * time.Second, 3000},
		{6000, 2 * time.Second, 6000},
	}
	for _, s := range steps {
		rows := agg.update(mk(s.used, 200), NewContractDetailsList(), t0.Add(s.at))
		row := testing_rowForClient(rows, peer.String())
		if row == nil || row.SendByteCount != s.want {
			t.Fatalf("used=%d: SendByteCount=%v, want %d", s.used, row, s.want)
		}
	}
}

// TestContractPeerAggregatorRunTotalSpansRenewal is the key case: a run is not a
// single contract. When one contract closes and another opens for the same peer
// (a renewal), the run total keeps counting across the boundary -- it does NOT
// restart at the new contract's zero. This is exactly what makes the cumulative
// total more useful than a per-contract number.
func TestContractPeerAggregatorRunTotalSpansRenewal(t *testing.T) {
	agg := newContractPeerAggregator(5 * time.Second)
	t0 := time.Now()
	us := connect.NewId()
	peer := connect.NewId()
	stream := connect.NewId()
	a := connect.NewId()
	b := connect.NewId()

	// contract a moves 0 -> 3000
	e0 := NewContractDetailsList()
	e0.Add(testing_peerContractDetails(a, us, peer, stream, 3000, 10000, 300, ContractStatusOpen))
	rows := agg.update(e0, NewContractDetailsList(), t0)
	if row := testing_rowForClient(rows, peer.String()); row == nil || row.SendByteCount != 3000 {
		t.Fatalf("after a: SendByteCount=%v, want 3000", row)
	}

	// a closes (tombstone) and b opens at 500 -> the run continues: 3000 + 500
	e1 := NewContractDetailsList()
	e1.Add(testing_peerContractDetails(a, us, peer, stream, 3000, 10000, 0, ContractStatusClosed))
	e1.Add(testing_peerContractDetails(b, us, peer, stream, 500, 10000, 200, ContractStatusOpen))
	rows = agg.update(e1, NewContractDetailsList(), t0.Add(1*time.Second))
	if row := testing_rowForClient(rows, peer.String()); row == nil || row.SendByteCount != 3500 {
		t.Fatalf("across renewal: SendByteCount=%v, want 3500 (3000+500)", row)
	}

	// b climbs to 2000 -> run is 3000 + 2000
	e2 := NewContractDetailsList()
	e2.Add(testing_peerContractDetails(b, us, peer, stream, 2000, 10000, 200, ContractStatusOpen))
	rows = agg.update(e2, NewContractDetailsList(), t0.Add(2*time.Second))
	if row := testing_rowForClient(rows, peer.String()); row == nil || row.SendByteCount != 5000 {
		t.Fatalf("after b climbs: SendByteCount=%v, want 5000 (3000+2000)", row)
	}
}

// TestContractPeerAggregatorRunTotalResetsOnIdle pins the idle reset: once a peer
// has moved no bytes for the idle window its run is over and the total resets to
// 0; when it moves bytes again a fresh run starts from those bytes (it does not
// resume the old total, and it does not double-count the pre-idle bytes).
func TestContractPeerAggregatorRunTotalResetsOnIdle(t *testing.T) {
	agg := newContractPeerAggregator(5 * time.Second)
	t0 := time.Now()
	us := connect.NewId()
	peer := connect.NewId()
	stream := connect.NewId()
	c := connect.NewId()

	mk := func(used ByteCount, bitRate int) *ContractDetailsList {
		e := NewContractDetailsList()
		e.Add(testing_peerContractDetails(c, us, peer, stream, used, 100000, bitRate, ContractStatusOpen))
		return e
	}

	// active: run climbs to 4000
	agg.update(mk(1000, 100), NewContractDetailsList(), t0)
	rows := agg.update(mk(4000, 300), NewContractDetailsList(), t0.Add(1*time.Second))
	if row := testing_rowForClient(rows, peer.String()); row == nil || row.SendByteCount != 4000 {
		t.Fatalf("active run: SendByteCount=%v, want 4000", row)
	}

	// idle: no throughput (bitRate 0) and >5s later -> run resets to 0
	rows = agg.update(mk(4000, 0), NewContractDetailsList(), t0.Add(8*time.Second))
	if row := testing_rowForClient(rows, peer.String()); row == nil || row.SendByteCount != 0 {
		t.Fatalf("after idle: SendByteCount=%v, want 0 (reset)", row)
	}

	// resume: bytes move again -> a NEW run counting only the post-idle bytes (500)
	rows = agg.update(mk(4500, 200), NewContractDetailsList(), t0.Add(9*time.Second))
	if row := testing_rowForClient(rows, peer.String()); row == nil || row.SendByteCount != 500 {
		t.Fatalf("resumed run: SendByteCount=%v, want 500 (4500-4000)", row)
	}
}

// TestContractPeerAggregatorRunTotalPerDirection pins that send and receive run
// totals are independent: bytes on one direction never leak into the other.
func TestContractPeerAggregatorRunTotalPerDirection(t *testing.T) {
	agg := newContractPeerAggregator(5 * time.Second)
	t0 := time.Now()
	us := connect.NewId()
	peer := connect.NewId()
	stream := connect.NewId()
	sendC := connect.NewId()
	recvC := connect.NewId()

	mk := func(sendUsed, recvUsed ByteCount, bitRate int) (*ContractDetailsList, *ContractDetailsList) {
		egress := NewContractDetailsList()
		egress.Add(testing_peerContractDetails(sendC, us, peer, stream, sendUsed, 100000, bitRate, ContractStatusOpen))
		ingress := NewContractDetailsList()
		ingress.Add(testing_peerContractDetails(recvC, peer, us, stream, recvUsed, 100000, bitRate, ContractStatusOpen))
		return egress, ingress
	}

	egress, ingress := mk(1000, 3000, 200)
	rows := agg.update(egress, ingress, t0)
	row := testing_rowForClient(rows, peer.String())
	if row == nil || row.SendByteCount != 1000 || row.ReceiveByteCount != 3000 {
		t.Fatalf("initial: send/recv = %v, want 1000/3000", row)
	}

	// send moves 1500 more; receive is unchanged -> only send climbs
	egress, ingress = mk(2500, 3000, 200)
	rows = agg.update(egress, ingress, t0.Add(1*time.Second))
	row = testing_rowForClient(rows, peer.String())
	if row == nil || row.SendByteCount != 2500 || row.ReceiveByteCount != 3000 {
		t.Fatalf("after send-only: send/recv = %d/%d, want 2500/3000", row.SendByteCount, row.ReceiveByteCount)
	}
}

// TestContractDetailsViewControllerRunTotalsAcrossEvents drives the view
// controller itself (not just the aggregator) across a sequence of contract
// events: each recompute publishes rows carrying the cumulative run total, and
// the rows listener fires so the app re-reads. This pins the VC's event -> rows
// path for the new byte-count fields. It calls recompute directly rather than
// through the throttled run loop, so the assertions are deterministic.
func TestContractDetailsViewControllerRunTotalsAcrossEvents(t *testing.T) {
	ctx := context.Background()
	us := connect.NewId()
	peer := connect.NewId()
	stream := connect.NewId()
	c := connect.NewId()

	dev := &testing_contractDetailsDevice{
		egress:  NewContractDetailsList(),
		ingress: NewContractDetailsList(),
	}
	vc := newContractDetailsViewController(ctx, dev, false)

	notified := 0
	vc.AddContractRowsListener(&testing_contractRowsListener{onChange: func() { notified += 1 }})

	// event 1: the peer's send contract is at 2000 used
	dev.egress = NewContractDetailsList()
	dev.egress.Add(testing_peerContractDetails(c, us, peer, stream, 2000, 100000, 200, ContractStatusOpen))
	vc.recompute(time.Now())
	if row := testing_rowForClient(vc.GetContractRows(), peer.String()); row == nil || row.SendByteCount != 2000 {
		t.Fatalf("event 1: SendByteCount=%v, want 2000", row)
	}

	// event 2: it climbs to 5000 -> the published run total climbs with it
	dev.egress = NewContractDetailsList()
	dev.egress.Add(testing_peerContractDetails(c, us, peer, stream, 5000, 100000, 200, ContractStatusOpen))
	vc.recompute(time.Now())
	if row := testing_rowForClient(vc.GetContractRows(), peer.String()); row == nil || row.SendByteCount != 5000 {
		t.Fatalf("event 2: SendByteCount=%v, want 5000", row)
	}

	if notified < 2 {
		t.Fatalf("expected the rows listener to fire on each event, got %d", notified)
	}
}
