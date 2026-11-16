package sdk

// A full peer identity pin store makes room for a new verified peer by evicting
// its least recently committed or admitted pin. It used to refuse the pin, and a
// required (post-quantum) session treats a refused pin as terminal, so a phone
// that had pinned 256 providers could open no new post-quantum session again
// (support inbox 8250, 8307, 8308; owner decision 2026-11-16). The store-wide
// signed-history latch survives eviction and a restart, so an evicted peer is a
// first contact that still needs signed evidence.

import (
	"os"
	"testing"

	"github.com/urnetwork/connect"
)

// Seeds peers 0..count-1, which the store keeps least recent first in that
// order (seedBoundedPins' keys sort by index), and opens the store.
func openSeededBoundedPinStore(t *testing.T, count int) (*LocalState, *connect.TransferMemoryBudget, *boundedPeerClientKeyPinStore) {
	t.Helper()
	state, budget := pinStoreTestOwner(t)
	seedBoundedPins(t, state, count)
	store, err := newBoundedPeerClientKeyPinStore(state, nil, budget)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	return state, budget, store
}

// Commits a new peer the way a required session admits one: a snapshot, then a
// compare-and-swap with the verified pin.
func admitBoundedPin(t *testing.T, store *boundedPeerClientKeyPinStore, i int) connect.ClientKeyPin {
	t.Helper()
	peer, pin := testBoundedPin(i)
	snapshot, err := store.ReadPeerClientKeyPinSnapshot(peer)
	if err != nil {
		t.Fatal(err)
	}
	if accepted, err := store.CompareAndSwapPeerClientKeyPin(peer, snapshot, &pin); !accepted || err != nil {
		t.Fatalf("peer %d not admitted: %t %v", i, accepted, err)
	}
	return pin
}

func requireBoundedPinned(t *testing.T, store *boundedPeerClientKeyPinStore, i int, want bool) {
	t.Helper()
	peer, _ := testBoundedPin(i)
	snapshot, err := store.ReadPeerClientKeyPinSnapshot(peer)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Pinned != want {
		t.Fatalf("peer %d pinned=%t, want %t", i, snapshot.Pinned, want)
	}
	// the latch holds for every peer, pinned or not: none may go unsigned
	if !snapshot.SignedHistorySeen {
		t.Fatalf("peer %d: the signed-history latch was lost", i)
	}
}

func TestBoundedPeerPinStoreFullStoreAdmitsNewPeerAndEvictsLeastRecent(t *testing.T) {
	state, budget, store := openSeededBoundedPinStore(t, peerPinStoreMaxPeerCount)

	pin := admitBoundedPin(t, store, peerPinStoreMaxPeerCount)
	if stats := store.Stats(); stats.PeerCount != peerPinStoreMaxPeerCount || stats.Evictions != 1 {
		t.Fatalf("stats after the first eviction: %+v", stats)
	}
	requireBoundedPinned(t, store, 0, false)
	for i := 1; i <= peerPinStoreMaxPeerCount; i++ {
		requireBoundedPinned(t, store, i, true)
	}
	peer, _ := testBoundedPin(peerPinStoreMaxPeerCount)
	if got, ok, err := store.GetPeerClientKeyPinChecked(peer); err != nil || !ok || got != pin {
		t.Fatalf("admitted pin: %v %v", ok, err)
	}

	// the eviction and the order are durable: after a restart the next new
	// peer evicts peer 1, the least recent then
	store.Close()
	restored, err := newBoundedPeerClientKeyPinStore(state, nil, budget)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if restored.Stats().PeerCount != peerPinStoreMaxPeerCount || !restored.SignedHistorySeen() {
		t.Fatalf("reload: %+v", restored.Stats())
	}
	requireBoundedPinned(t, restored, 0, false)
	requireBoundedPinned(t, restored, peerPinStoreMaxPeerCount, true)
	admitBoundedPin(t, restored, peerPinStoreMaxPeerCount+1)
	requireBoundedPinned(t, restored, 1, false)
	requireBoundedPinned(t, restored, 2, true)
	info, err := os.Stat(restored.path)
	if err != nil || peerPinStoreMaxFileByteCount < info.Size() {
		t.Fatalf("unbounded file: %v %v", info, err)
	}
}

func TestBoundedPeerPinStoreRecentlyUsedPeerKeepsItsRatchet(t *testing.T) {
	state, budget, store := openSeededBoundedPinStore(t, peerPinStoreMaxPeerCount)

	// peer 0 ratchets forward (a commit); peer 1 is admitted again against its
	// unchanged pin (a use). Both become more recent than peers 2..255.
	peer0, pin0 := testBoundedPin(0)
	pin0.Generation++
	if err := store.CommitPeerClientKeyPin(peer0, pin0); err != nil {
		t.Fatal(err)
	}
	peer1, _ := testBoundedPin(1)
	snapshot1, err := store.ReadPeerClientKeyPinSnapshot(peer1)
	if err != nil {
		t.Fatal(err)
	}
	if accepted, err := store.CompareAndSwapPeerClientKeyPin(peer1, snapshot1, nil); !accepted || err != nil {
		t.Fatalf("unchanged admission: %t %v", accepted, err)
	}

	admitBoundedPin(t, store, peerPinStoreMaxPeerCount)
	admitBoundedPin(t, store, peerPinStoreMaxPeerCount+1)
	requireBoundedPinned(t, store, 2, false)
	requireBoundedPinned(t, store, 3, false)
	requireBoundedPinned(t, store, 0, true)
	requireBoundedPinned(t, store, 1, true)
	if got, ok, err := store.GetPeerClientKeyPinChecked(peer0); err != nil || !ok || got != pin0 {
		t.Fatalf("peer 0 lost its ratchet: %v %v", ok, err)
	}
	// the kept ratchet still refuses a rollback
	previous := pin0
	previous.Generation--
	if err := store.CommitPeerClientKeyPin(peer0, previous); err != errPeerPinStoreRollback {
		t.Fatalf("rollback of a kept pin: %v", err)
	}

	// the refreshed order survives a restart: the next new peer evicts peer 4
	store.Close()
	restored, err := newBoundedPeerClientKeyPinStore(state, nil, budget)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	admitBoundedPin(t, restored, peerPinStoreMaxPeerCount+2)
	requireBoundedPinned(t, restored, 4, false)
	requireBoundedPinned(t, restored, 0, true)
	requireBoundedPinned(t, restored, 1, true)
	if got, ok, err := restored.GetPeerClientKeyPinChecked(peer0); err != nil || !ok || got != pin0 {
		t.Fatalf("peer 0's ratchet after reload: %v %v", ok, err)
	}
}

// A failed write while evicting changes nothing: the full store keeps every pin
// and the store fails closed, as for any persistence failure.
func TestBoundedPeerPinStoreFailedEvictionKeepsEveryPin(t *testing.T) {
	_, _, store := openSeededBoundedPinStore(t, peerPinStoreMaxPeerCount)
	store.testingCommitStep = func(step string) error {
		if step == "write" {
			return &os.PathError{Op: step, Path: "sensitive-local-path", Err: os.ErrPermission}
		}
		return nil
	}
	peer, pin := testBoundedPin(peerPinStoreMaxPeerCount)
	if err := store.CommitPeerClientKeyPin(peer, pin); err != errPeerPinStoreIO {
		t.Fatalf("commit: %v", err)
	}
	evicted, _ := testBoundedPin(0)
	if store.data.count != peerPinStoreMaxPeerCount || store.indexWithLock(evicted) != 0 || store.indexWithLock(peer) != -1 {
		t.Fatal("a failed eviction changed the pins")
	}
	if stats := store.Stats(); stats.Evictions != 0 || stats.PersistenceFailures != 1 {
		t.Fatalf("stats: %+v", stats)
	}
}
