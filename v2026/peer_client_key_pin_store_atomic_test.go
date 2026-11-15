// Store admission binds verified evidence to one atomic peer/global snapshot.
// These tests force stale reads and persistence failures without timing guesses.
package sdk

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// The same admission contract applies to both shipping persistent stores.
type atomicPeerPinFixture struct {
	store connect.AtomicPeerClientKeyPinStore
	path  string
}

// Each fixture owns its directory and, for bounded stores, its existing budget.
func newAtomicPeerPinFixture(t *testing.T, kind string) *atomicPeerPinFixture {
	t.Helper()
	if kind == "legacy" {
		store := newLocalStatePeerClientKeyPinStore(t.TempDir())
		return &atomicPeerPinFixture{store: store, path: store.path}
	}
	state, budget := pinStoreTestOwner(t)
	store, err := newBoundedPeerClientKeyPinStore(state, nil, budget)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	return &atomicPeerPinFixture{store: store, path: store.path}
}

// Fault hooks are installed only between operations, before concurrent work.
func (self *atomicPeerPinFixture) commitStep(hook func(string) error) {
	switch store := self.store.(type) {
	case *localStatePeerClientKeyPinStore:
		store.testingCommitStep = hook
	case *boundedPeerClientKeyPinStore:
		store.testingCommitStep = hook
	}
}

// Inspects the retained ratchet after a failure has intentionally closed reads.
func (self *atomicPeerPinFixture) retainedSnapshot(peer connect.Id) connect.ClientKeyPinSnapshot {
	switch store := self.store.(type) {
	case *localStatePeerClientKeyPinStore:
		store.stateLock.Lock()
		defer store.stateLock.Unlock()
		return store.snapshotWithLock(peer)
	case *boundedPeerClientKeyPinStore:
		store.localState.authStateLock.Lock()
		defer store.localState.authStateLock.Unlock()
		store.stateLock.Lock()
		defer store.stateLock.Unlock()
		return store.snapshotWithLock(peer)
	default:
		panic("unknown atomic pin fixture")
	}
}

// Two resolvers share a store, but the second result was verified before the
// first committed. A numerically higher fork must not overwrite the winner.
func TestPeerPinStoresRejectStaleAdvancingFork(t *testing.T) {
	for _, kind := range []string{"legacy", "bounded"} {
		f := newAtomicPeerPinFixture(t, kind)
		peer, original := testBoundedPin(1)
		original.Generation = 0
		if err := f.store.CommitPeerClientKeyPin(peer, original); err != nil {
			t.Fatalf("%s seed: %v", kind, err)
		}
		first, err := f.store.ReadPeerClientKeyPinSnapshot(peer)
		if err != nil {
			t.Fatal(err)
		}
		stale, err := f.store.ReadPeerClientKeyPinSnapshot(peer)
		if err != nil {
			t.Fatal(err)
		}
		winner, fork := original, original
		winner.Generation, winner.PublicKey[0] = 1, 17
		fork.Generation, fork.PublicKey[0] = 2, 29
		if accepted, err := f.store.CompareAndSwapPeerClientKeyPin(peer, first, &winner); !accepted || err != nil {
			t.Fatalf("%s first commit: accepted=%t error=%v", kind, accepted, err)
		}
		before, err := os.ReadFile(f.path)
		if err != nil {
			t.Fatal(err)
		}
		if accepted, err := f.store.CompareAndSwapPeerClientKeyPin(peer, stale, &fork); accepted || err != nil {
			t.Fatalf("%s stale advancing fork: accepted=%t error=%v", kind, accepted, err)
		}
		after, err := os.ReadFile(f.path)
		current, readErr := f.store.ReadPeerClientKeyPinSnapshot(peer)
		if err != nil || readErr != nil || !bytes.Equal(before, after) || !current.Pinned || current.Pin != winner || !current.SignedHistorySeen {
			t.Fatalf("%s stale writer changed committed evidence: snapshot=%+v file=%v read=%v", kind, current, err, readErr)
		}
	}
}

// A stale valid extension is retried against refreshed evidence, not rejected
// permanently or committed using the verifier's earlier root of trust.
func TestPeerPinStoresAcceptExtensionAfterSnapshotRetry(t *testing.T) {
	for _, kind := range []string{"legacy", "bounded"} {
		f := newAtomicPeerPinFixture(t, kind)
		peer, first := testBoundedPin(2)
		first.Generation = 3
		if err := f.store.CommitPeerClientKeyPin(peer, first); err != nil {
			t.Fatal(err)
		}
		stale, err := f.store.ReadPeerClientKeyPinSnapshot(peer)
		if err != nil {
			t.Fatal(err)
		}
		second, third := first, first
		second.Generation, second.PublicKey[0] = 4, 31
		third.Generation, third.PublicKey[0] = 5, 47
		if accepted, err := f.store.CompareAndSwapPeerClientKeyPin(peer, stale, &second); !accepted || err != nil {
			t.Fatalf("%s second head: %t %v", kind, accepted, err)
		}
		if accepted, err := f.store.CompareAndSwapPeerClientKeyPin(peer, stale, &third); accepted || err != nil {
			t.Fatalf("%s stale extension did not request retry: %t %v", kind, accepted, err)
		}
		fresh, err := f.store.ReadPeerClientKeyPinSnapshot(peer)
		if err != nil || fresh.Pin != second {
			t.Fatalf("%s refreshed verification input: %+v %v", kind, fresh, err)
		}
		// Chain verification belongs to connect; only its revalidated result
		// may be submitted with this new snapshot.
		if accepted, err := f.store.CompareAndSwapPeerClientKeyPin(peer, fresh, &third); !accepted || err != nil {
			t.Fatalf("%s refreshed extension: %t %v", kind, accepted, err)
		}
		restored := newLocalStatePeerClientKeyPinStore(filepath.Dir(f.path))
		got, err := restored.ReadPeerClientKeyPinSnapshot(peer)
		if err != nil || !got.Pinned || got.Pin != third || !got.SignedHistorySeen {
			t.Fatalf("%s committed pin/latch did not reload: %+v %v", kind, got, err)
		}
		info, err := os.Stat(f.path)
		if err != nil || info.Mode().Perm() != LocalStorageFilePermissions {
			t.Fatalf("%s private file permissions: %v %v", kind, info, err)
		}
	}
}

// A different peer can change the global latch after unsigned fallback was
// verified. Nil-next admission must compare that latch and never write a file.
func TestPeerPinStoresReadOnlyAdmissionChecksGlobalLatch(t *testing.T) {
	for _, kind := range []string{"legacy", "bounded"} {
		f := newAtomicPeerPinFixture(t, kind)
		peer, _ := testBoundedPin(3)
		stale, err := f.store.ReadPeerClientKeyPinSnapshot(peer)
		if err != nil || stale != (connect.ClientKeyPinSnapshot{}) {
			t.Fatalf("%s fresh snapshot: %+v %v", kind, stale, err)
		}
		f.commitStep(func(string) error { t.Error("read-only admission attempted persistence"); return os.ErrPermission })
		if accepted, err := f.store.CompareAndSwapPeerClientKeyPin(peer, stale, nil); !accepted || err != nil {
			t.Fatalf("%s fresh read-only admission: %t %v", kind, accepted, err)
		}
		if _, err := os.Stat(f.path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s read-only admission created a file: %v", kind, err)
		}
		f.commitStep(nil)
		otherPeer, pin := testBoundedPin(4)
		if err := f.store.CommitPeerClientKeyPin(otherPeer, pin); err != nil {
			t.Fatal(err)
		}
		f.commitStep(func(string) error { t.Error("read-only retry attempted persistence"); return os.ErrPermission })
		if accepted, err := f.store.CompareAndSwapPeerClientKeyPin(peer, stale, nil); accepted || err != nil {
			t.Fatalf("%s stale unsigned decision crossed latch update: %t %v", kind, accepted, err)
		}
		fresh, err := f.store.ReadPeerClientKeyPinSnapshot(peer)
		if err != nil || fresh.Pinned || !fresh.SignedHistorySeen {
			t.Fatalf("%s latch was not part of refreshed evidence: %+v %v", kind, fresh, err)
		}
		// The caller now sees signed history and must apply its stricter
		// fallback policy; the store itself only linearizes the chosen policy.
		if accepted, err := f.store.CompareAndSwapPeerClientKeyPin(peer, fresh, nil); !accepted || err != nil {
			t.Fatalf("%s unchanged refreshed snapshot: %t %v", kind, accepted, err)
		}
	}
}

// Storage checks remain a second fence even with a matching snapshot.
func TestPeerPinStoresPreserveIdentityAndGenerationGuards(t *testing.T) {
	for _, kind := range []string{"legacy", "bounded"} {
		f := newAtomicPeerPinFixture(t, kind)
		peer, pin := testBoundedPin(5)
		pin.Generation = 9
		if err := f.store.CommitPeerClientKeyPin(peer, pin); err != nil {
			t.Fatal(err)
		}
		snapshot, err := f.store.ReadPeerClientKeyPinSnapshot(peer)
		if err != nil {
			t.Fatal(err)
		}
		for _, change := range []string{"generation", "domain", "signer", "equal-generation key"} {
			next := pin
			switch change {
			case "generation":
				next.Generation--
			case "domain":
				next.Generation++
				next.DomainDigest[0]--
			case "signer":
				next.Generation++
				next.Signer[0]--
			case "equal-generation key":
				next.PublicKey[0]--
			}
			if err := f.store.CommitPeerClientKeyPin(peer, next); !errors.Is(err, errPeerPinStoreRollback) {
				t.Fatalf("%s direct %s conflict: %v", kind, change, err)
			}
			if change == "signer" {
				// Only verified forward history may change the signer through
				// conditional admission; an equal-generation replacement may not.
				next.Generation = pin.Generation
			}
			if accepted, err := f.store.CompareAndSwapPeerClientKeyPin(peer, snapshot, &next); accepted || !errors.Is(err, errPeerPinStoreRollback) {
				t.Fatalf("%s %s conflict: %t %v", kind, change, accepted, err)
			}
			got, err := f.store.ReadPeerClientKeyPinSnapshot(peer)
			if err != nil || got != snapshot {
				t.Fatalf("%s %s refusal changed/poisoned the prior pin: %+v %v", kind, change, got, err)
			}
		}
	}
}

// Failed staging never exposes half a pin/latch transaction. Directory-sync
// failure is after atomic rename: disk is complete, but memory stays refused.
func TestPeerPinStoresAtomicPersistenceFailure(t *testing.T) {
	for _, kind := range []string{"legacy", "bounded"} {
		for _, seeded := range []bool{false, true} {
			for _, phase := range []string{"create", "write", "sync", "close", "rename", "directory sync"} {
				f := newAtomicPeerPinFixture(t, kind)
				peer, pin := testBoundedPin(6)
				pin.Generation = 10
				if seeded {
					if err := f.store.CommitPeerClientKeyPin(peer, pin); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(f.path, []byte(`{"signed_history_seen":false,"pins":{}}`), 0600); err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(f.path)
				if err != nil {
					t.Fatal(err)
				}
				snapshot, err := f.store.ReadPeerClientKeyPinSnapshot(peer)
				if err != nil {
					t.Fatal(err)
				}
				f.commitStep(func(step string) error {
					if step == phase {
						return os.ErrPermission
					}
					return nil
				})
				pin.Generation++
				pin.PublicKey[0] = 71
				if accepted, err := f.store.CompareAndSwapPeerClientKeyPin(peer, snapshot, &pin); accepted || err != errPeerPinStoreIO {
					t.Fatalf("%s seeded=%t phase=%s: accepted=%t error=%v", kind, seeded, phase, accepted, err)
				}
				if retained := f.retainedSnapshot(peer); retained != snapshot {
					t.Fatalf("%s seeded=%t phase=%s changed memory before durability: %+v", kind, seeded, phase, retained)
				}
				if _, err := f.store.ReadPeerClientKeyPinSnapshot(peer); err != errPeerPinStoreIO {
					t.Fatalf("%s phase=%s did not fail closed: %v", kind, phase, err)
				}
				if accepted, err := f.store.CompareAndSwapPeerClientKeyPin(peer, snapshot, nil); accepted || err != errPeerPinStoreIO {
					t.Fatalf("%s phase=%s admitted after failed commit: %t %v", kind, phase, accepted, err)
				}
				after, err := os.ReadFile(f.path)
				if err != nil {
					t.Fatal(err)
				}
				if phase != "directory sync" && !bytes.Equal(before, after) {
					t.Fatalf("%s phase=%s replaced the original file before rename", kind, phase)
				}
				reloaded := newLocalStatePeerClientKeyPinStore(filepath.Dir(f.path))
				got, err := reloaded.ReadPeerClientKeyPinSnapshot(peer)
				want := snapshot
				if phase == "directory sync" {
					want = connect.ClientKeyPinSnapshot{Pin: pin, Pinned: true, SignedHistorySeen: true}
				}
				if err != nil || got != want {
					t.Fatalf("%s phase=%s left partial persisted evidence: %+v %v", kind, phase, got, err)
				}
				staging, err := filepath.Glob(filepath.Join(filepath.Dir(f.path), ".peer_client_key_pins-*"))
				if err != nil || len(staging) != 0 {
					t.Fatalf("%s phase=%s leaked staging files: %v %v", kind, phase, staging, err)
				}
			}
		}
	}
}

// The old JSON layout remains readable. A durable pin repairs a global latch
// left false by a crash between the legacy writer's two separate file writes.
func TestPeerPinStoresLegacyFileCompatibility(t *testing.T) {
	state, budget := pinStoreTestOwner(t)
	peer, pin := testBoundedPin(7)
	storage := peerClientKeyPinsStorage{Pins: map[string]peerClientKeyPinRecord{peer.String(): peerClientKeyPinRecord(pin)}}
	encoded, err := json.Marshal(storage)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(state.localStorageDir, peerClientKeyPinsFileName)
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	legacy := newLocalStatePeerClientKeyPinStore(state.localStorageDir)
	snapshot, err := legacy.ReadPeerClientKeyPinSnapshot(peer)
	if err != nil || !snapshot.Pinned || snapshot.Pin != pin || !snapshot.SignedHistorySeen {
		t.Fatalf("legacy reader lost the pin's signed-history evidence: %+v %v", snapshot, err)
	}
	bounded, err := newBoundedPeerClientKeyPinStore(state, nil, budget)
	if err != nil {
		t.Fatal(err)
	}
	defer bounded.Close()
	got, err := bounded.ReadPeerClientKeyPinSnapshot(peer)
	if err != nil || !got.Pinned || got.Pin != pin || !got.SignedHistorySeen {
		t.Fatalf("bounded reader lost legacy pin/latch: %+v %v", got, err)
	}
	otherPeer, _ := testBoundedPin(11)
	for _, store := range []connect.AtomicPeerClientKeyPinStore{legacy, bounded} {
		unseen, err := store.ReadPeerClientKeyPinSnapshot(otherPeer)
		if err != nil || unseen.Pinned || !unseen.SignedHistorySeen {
			t.Fatalf("old split write erased the global ratchet for another peer: %+v %v", unseen, err)
		}
	}
	afterRead, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(encoded, afterRead) {
		t.Fatalf("load repair unexpectedly rewrote the file: %v", err)
	}
	if accepted, err := bounded.CompareAndSwapPeerClientKeyPin(peer, got, &pin); !accepted || err != nil {
		t.Fatalf("compatible pin/latch commit: %t %v", accepted, err)
	}
	reloaded := newLocalStatePeerClientKeyPinStore(state.localStorageDir)
	if durable, err := reloaded.ReadPeerClientKeyPinSnapshot(peer); err != nil || durable != got {
		t.Fatalf("repaired ratchet did not survive commit/reload: %+v %v", durable, err)
	}
}

// Load failures can no longer masquerade as an unpinned first installation.
func TestLegacyPeerPinStoreCheckedLoadFailsClosed(t *testing.T) {
	for _, encoded := range []string{`{"pins":`, `null`, `{}`, `{"signed_history_seen":null,"pins":{}}`, `{"signed_history_seen":false,"pins":null}`} {
		directory := t.TempDir()
		path := filepath.Join(directory, peerClientKeyPinsFileName)
		if err := os.WriteFile(path, []byte(encoded), 0600); err != nil {
			t.Fatal(err)
		}
		store := newLocalStatePeerClientKeyPinStore(directory)
		peer, pin := testBoundedPin(8)
		if _, err := store.ReadPeerClientKeyPinSnapshot(peer); !errors.Is(err, errPeerPinStoreCorrupt) {
			t.Fatalf("corrupt store became fresh: %v", err)
		}
		if accepted, err := store.CompareAndSwapPeerClientKeyPin(peer, connect.ClientKeyPinSnapshot{}, &pin); accepted || !errors.Is(err, errPeerPinStoreCorrupt) {
			t.Fatalf("corrupt store admitted a replacement: %t %v", accepted, err)
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != encoded {
			t.Fatalf("corrupt input was silently overwritten: %q %v", after, err)
		}
	}
}

// Conditional admission preserves exact capacity and permits read-only checks
// at capacity; a changed owner or closed owner can never admit old snapshots.
func TestBoundedPeerPinStoreAtomicCapacityAndLifecycle(t *testing.T) {
	state, _ := pinStoreTestOwner(t)
	seedBoundedPins(t, state, peerPinStoreMaxPeerCount)
	budget := connect.NewTransferMemoryBudget(2 * peerPinStoreMemoryByteCount)
	store, err := newBoundedPeerClientKeyPinStore(state, nil, budget)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	peer, pin := testBoundedPin(peerPinStoreMaxPeerCount)
	snapshot, err := store.ReadPeerClientKeyPinSnapshot(peer)
	if err != nil {
		t.Fatal(err)
	}
	if accepted, err := store.CompareAndSwapPeerClientKeyPin(peer, snapshot, nil); !accepted || err != nil {
		t.Fatalf("capacity blocked read-only admission: %t %v", accepted, err)
	}
	if accepted, err := store.CompareAndSwapPeerClientKeyPin(peer, snapshot, &pin); accepted || !errors.Is(err, errPeerPinStoreCapacity) {
		t.Fatalf("atomic admission exceeded capacity: %t %v", accepted, err)
	}
	peer, pin = testBoundedPin(9)
	snapshot, err = store.ReadPeerClientKeyPinSnapshot(peer)
	if err != nil {
		t.Fatal(err)
	}
	pin.Generation++
	if accepted, err := store.CompareAndSwapPeerClientKeyPin(peer, snapshot, &pin); !accepted || err != nil {
		t.Fatalf("existing pin could not advance at capacity: %t %v", accepted, err)
	}
	successor, err := newBoundedPeerClientKeyPinStore(state, nil, budget)
	if err != nil {
		t.Fatal(err)
	}
	defer successor.Close()
	if _, err := store.ReadPeerClientKeyPinSnapshot(peer); err != errPeerPinStoreSuperseded {
		t.Fatalf("superseded owner supplied verification evidence: %v", err)
	}
	if accepted, err := store.CompareAndSwapPeerClientKeyPin(peer, snapshot, nil); accepted || err != errPeerPinStoreSuperseded {
		t.Fatalf("superseded owner admitted a stale decision: %t %v", accepted, err)
	}
	successor.Close()
	if accepted, err := successor.CompareAndSwapPeerClientKeyPin(peer, snapshot, &pin); accepted || err != errPeerPinStoreClosed {
		t.Fatalf("closed owner admitted a commit: %t %v", accepted, err)
	}
	store.Close()
	if stats := budget.Stats(); stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
		t.Fatalf("atomic store lifecycle leaked budget: %+v", stats)
	}
}

// Auth ownership is a separate fence from the pin-store generation itself.
func TestBoundedPeerPinStoreAtomicAuthOwnerGuard(t *testing.T) {
	state, budget := pinStoreTestOwner(t)
	owner := &deviceAuthPublicationGate{}
	state.deviceAuthOwner = owner
	store, err := newBoundedPeerClientKeyPinStore(state, owner, budget)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	peer, pin := testBoundedPin(10)
	snapshot, err := store.ReadPeerClientKeyPinSnapshot(peer)
	if err != nil {
		t.Fatal(err)
	}
	state.authStateLock.Lock()
	state.deviceAuthOwner = &deviceAuthPublicationGate{}
	state.authStateLock.Unlock()
	if _, err := store.ReadPeerClientKeyPinSnapshot(peer); err != errPeerPinStoreSuperseded {
		t.Fatalf("old auth owner supplied evidence: %v", err)
	}
	if accepted, err := store.CompareAndSwapPeerClientKeyPin(peer, snapshot, &pin); accepted || err != errPeerPinStoreSuperseded {
		t.Fatalf("old auth owner committed a pin: %t %v", accepted, err)
	}
}
