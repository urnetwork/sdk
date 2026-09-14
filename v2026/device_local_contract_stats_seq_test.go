package sdk

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// TestDeviceLocalContractStatsSequenceDiscard pins the consumer side of the
// `ContractStatsEvent.Sequence` contract: the producer assigns strictly
// increasing per-contract sequences (starting at 1), and the consumer must
// discard any event at or below the last consumed sequence for that contract
// id. In particular a stale `Open=true` delivered after the final close (the
// emit callbacks run outside the producer lock and can reorder) must not
// resurrect the contract. The sequence high-water mark is kept while the
// contract's tombstone is tracked and dropped when the tombstone is evicted,
// mirroring the producer's counter lifecycle and keeping the map bounded.
func TestDeviceLocalContractStatsSequenceDiscard(t *testing.T) {
	tracker := newDeviceContractTracker()
	// gate everything except the first batch and closes, like the device does
	epoch := 1 * time.Hour

	us := connect.NewId()
	peer := connect.NewId()
	contractId := connect.NewId()
	path := connect.TransferPath{SourceId: us, DestinationId: peer}

	open := &connect.ContractStatsEvent{
		ContractId:        contractId,
		Path:              path,
		TransferByteCount: 10000,
		UsedByteCount:     100,
		Open:              true,
		Sequence:          1,
	}
	egressStats, _, _, _ := tracker.update([]*connect.ContractStatsEvent{open}, epoch)
	if egressStats == nil {
		t.Fatalf("expected the first batch to emit")
	}
	if state := tracker.egressContracts[contractId]; state == nil || !state.open {
		t.Fatalf("expected the contract open, got %+v", state)
	}
	if seq := tracker.lastSeenSequences[contractId]; seq != 1 {
		t.Fatalf("expected sequence high-water 1, got %d", seq)
	}

	closeEvent := *open
	closeEvent.UsedByteCount = 200
	closeEvent.Open = false
	closeEvent.Sequence = 2
	egressStats, _, egressDetails, _ := tracker.update([]*connect.ContractStatsEvent{&closeEvent}, epoch)
	if egressStats == nil {
		t.Fatalf("expected the close to emit")
	}
	if egressDetails.Len() != 1 || egressDetails.Get(0).Status != ContractStatusClosed {
		t.Fatalf("expected the one closed tombstone, got %+v", egressDetails)
	}
	// the high-water mark advances with the close and is kept while the
	// tombstone is still tracked
	if seq := tracker.lastSeenSequences[contractId]; seq != 2 {
		t.Fatalf("expected sequence high-water 2, got %d", seq)
	}

	// a replayed open (seq 1) and a replayed close (seq 2) — reordered or
	// duplicated deliveries — must both be discarded: the contract stays
	// closed, and the replayed close must not force a listener dispatch
	staleOpen := *open
	staleClose := closeEvent
	egressStats, _, _, _ = tracker.update([]*connect.ContractStatsEvent{&staleOpen, &staleClose}, epoch)
	if egressStats != nil {
		t.Fatalf("expected the stale batch fully discarded (gated), got %+v", egressStats)
	}
	if state := tracker.egressContracts[contractId]; state == nil || state.open {
		t.Fatalf("expected the contract to stay closed, got %+v", state)
	}
	if seq := tracker.lastSeenSequences[contractId]; seq != 2 {
		t.Fatalf("expected sequence high-water pinned at 2, got %d", seq)
	}

	// the settle pass evicts the reported tombstone and drops the sequence
	// entry with it — the map is bounded by the tracked contracts
	tracker.flushPending(0)
	if _, ok := tracker.egressContracts[contractId]; ok {
		t.Fatalf("expected the tombstone evicted")
	}
	if _, ok := tracker.lastSeenSequences[contractId]; ok {
		t.Fatalf("expected the sequence entry dropped after the close was consumed")
	}
	if count := len(tracker.lastSeenSequences); count != 0 {
		t.Fatalf("expected an empty sequence map, got %d entries", count)
	}
}

// TestDeviceLocalContractStatsSequenceDiscardDevice runs the same reorder
// through the registered device consumer (`updateContractStatsEvents`): a
// stale open replayed after the final close must not resurrect the contract
// in the pushed stats or the getter surface, and must not fire the listeners.
func TestDeviceLocalContractStatsSequenceDiscardDevice(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}
	settings := DefaultDeviceLocalSettings()
	settings.AllowProvider = false
	settings.Verbose = false
	settings.DisableLogging = true
	// gate everything except closes
	settings.ContractStatsEpoch = 1 * time.Hour
	device, err := newDeviceLocalWithOverrides(networkSpace, byJwt, "", "", "", NewId(), settings, connect.NewId())
	if err != nil {
		t.Fatalf("device: %v", err)
	}
	defer device.Close()

	egressStatsListener := &testing_contractStatsListener{}
	egressStatsSub := device.AddEgressContractStatsChangeListener(egressStatsListener)
	defer egressStatsSub.Close()

	us := connect.NewId()
	peer := connect.NewId()
	contractId := connect.NewId()
	openEvent := &connect.ContractStatsEvent{
		ContractId:        contractId,
		Path:              connect.TransferPath{SourceId: us, DestinationId: peer},
		TransferByteCount: 10000,
		UsedByteCount:     100,
		Open:              true,
		Sequence:          1,
	}
	device.updateContractStatsEvents([]*connect.ContractStatsEvent{openEvent})
	if count := egressStatsListener.fireCount(); count != 1 {
		t.Fatalf("expected 1 fire, got %d", count)
	}

	closeEvent := *openEvent
	closeEvent.UsedByteCount = 200
	closeEvent.Open = false
	closeEvent.Sequence = 2
	device.updateContractStatsEvents([]*connect.ContractStatsEvent{&closeEvent})
	if count := egressStatsListener.fireCount(); count != 2 {
		t.Fatalf("expected the close to emit, got %d", count)
	}

	// the stale replayed open arrives after the close (the reorder): it must
	// be discarded — no listener fire, and the contract must not come back
	// open on the getter surface
	staleOpen := *openEvent
	device.updateContractStatsEvents([]*connect.ContractStatsEvent{&staleOpen})
	if count := egressStatsListener.fireCount(); count != 2 {
		t.Fatalf("expected the stale open discarded, got %d fires", count)
	}
	details := device.GetEgressContractDetails()
	for i := 0; i < details.Len(); i += 1 {
		if details.Get(i).Status == ContractStatusOpen {
			t.Fatalf("expected no open contract after the close, got %+v", details.Get(i))
		}
	}

	// after the tombstone settles out, the sequence entry is dropped
	func() {
		device.stateLock.Lock()
		defer device.stateLock.Unlock()
		device.contracts.flushPending(0)
		if _, ok := device.contracts.lastSeenSequences[contractId]; ok {
			t.Fatalf("expected the sequence entry dropped after the close")
		}
	}()
	if details := device.GetEgressContractDetails(); details.Len() != 0 {
		t.Fatalf("expected the contract evicted, got %+v", details)
	}
}
