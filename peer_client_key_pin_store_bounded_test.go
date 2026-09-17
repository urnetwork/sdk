package sdk

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/connect"
)

func testBoundedPin(i int) (connect.Id, connect.ClientKeyPin) {
	var peer connect.Id
	peer[14], peer[15] = byte(i>>8), byte(i)
	pin := connect.ClientKeyPin{Generation: ^uint64(0) - 1}
	for i := range pin.DomainDigest {
		pin.DomainDigest[i], pin.PublicKey[i] = 255, 255
	}
	for i := range pin.Signer {
		pin.Signer[i] = 255
	}
	return peer, pin
}

func seedBoundedPins(t *testing.T, state *LocalState, count int) []byte {
	t.Helper()
	storage := peerClientKeyPinsStorage{SignedHistorySeen: count != 0, Pins: map[string]peerClientKeyPinRecord{}}
	for i := 0; i < count; i++ {
		peer, pin := testBoundedPin(i)
		storage.Pins[peer.String()] = peerClientKeyPinRecord{DomainDigest: pin.DomainDigest, Signer: pin.Signer, Generation: pin.Generation, PublicKey: pin.PublicKey}
	}
	encoded, err := json.Marshal(storage)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state.localStorageDir, peerClientKeyPinsFileName), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	return encoded
}

func pinStoreTestOwner(t *testing.T) (*LocalState, *connect.TransferMemoryBudget) {
	t.Helper()
	state := newLocalState(t.Context(), t.TempDir())
	t.Cleanup(state.Close)
	return state, connect.NewTransferMemoryBudget(peerPinStoreMemoryByteCount)
}

func TestBoundedPeerPinStoreLoadLimitsAndBudget(t *testing.T) {
	for _, kind := range []string{"empty", "exact256", "257", "oversized whitespace", "corrupt", "directory", "dangling", "live symlink", "unreadable", "missing parent"} {
		t.Run(kind, func(t *testing.T) {
			state, budget := pinStoreTestOwner(t)
			path := filepath.Join(state.localStorageDir, peerClientKeyPinsFileName)
			var want error
			switch kind {
			case "exact256":
				seedBoundedPins(t, state, 256)
			case "257":
				seedBoundedPins(t, state, 257)
				want = errPeerPinStoreCapacity
			case "oversized whitespace":
				encoded := seedBoundedPins(t, state, 1)
				encoded = append(encoded, bytes.Repeat([]byte(" "), peerPinStoreMaxFileByteCount)...)
				if err := os.WriteFile(path, encoded, 0600); err != nil {
					t.Fatal(err)
				}
				want = errPeerPinStoreOversize
			case "corrupt":
				if err := os.WriteFile(path, []byte(`{"pins":`), 0600); err != nil {
					t.Fatal(err)
				}
				want = errPeerPinStoreCorrupt
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
				want = errPeerPinStoreIO
			case "dangling":
				if err := os.Symlink(filepath.Join(state.localStorageDir, "missing"), path); err != nil {
					t.Fatal(err)
				}
				want = errPeerPinStoreIO
			case "live symlink":
				seedBoundedPins(t, state, 1)
				target := filepath.Join(state.localStorageDir, "pins-target")
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
				want = errPeerPinStoreIO
			case "unreadable":
				seedBoundedPins(t, state, 1)
				if err := os.Chmod(path, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(path, 0600) })
				if file, err := os.Open(path); err == nil {
					file.Close()
					t.Skip("privileged process can read mode-000 file")
				}
				want = errPeerPinStoreIO
			case "missing parent":
				state.localStorageDir = filepath.Join(state.localStorageDir, "missing")
				want = errPeerPinStoreIO
			}
			store, err := newBoundedPeerClientKeyPinStore(state, nil, budget)
			if !errors.Is(err, want) {
				t.Fatalf("error=%v want=%v", err, want)
			}
			if want == nil {
				if store == nil || budget.UsedByteCount() != peerPinStoreMemoryByteCount {
					t.Fatal("successful load lacks its exact claim")
				}
				if kind == "exact256" && store.Stats().PeerCount != 256 {
					t.Fatal("exact-cap file did not load")
				}
				store.Close()
				store.Close()
			} else if store != nil {
				t.Fatal("failed load returned an owner")
			}
			if s := budget.Stats(); s.UsedByteCount != 0 || s.ReservedByteCount != s.ReleasedByteCount {
				t.Fatalf("unbalanced load/close: %+v", s)
			}
		})
	}
	state, budget := pinStoreTestOwner(t)
	if !budget.TryReserve(peerPinStoreMemoryByteCount) {
		t.Fatal("fill")
	}
	defer budget.Release(peerPinStoreMemoryByteCount)
	before := budget.Stats()
	allocations := testing.AllocsPerRun(100, func() {
		store, err := newBoundedPeerClientKeyPinStore(state, nil, budget)
		if store != nil || !errors.Is(err, errPeerPinStoreBudget) {
			t.Fatal("full budget admitted a store")
		}
	})
	if allocations != 0 || budget.Stats() != before {
		t.Fatalf("refusal allocated/changed ownership: allocations=%g", allocations)
	}
}

func TestBoundedPeerPinStoreConcurrentCapacityUpdateAndRollback(t *testing.T) {
	state, budget := pinStoreTestOwner(t)
	seedBoundedPins(t, state, 255)
	store, err := newBoundedPeerClientKeyPinStore(state, nil, budget)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var accepted atomic.Int32
	var workers sync.WaitGroup
	for i := 255; i < 271; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			peer, pin := testBoundedPin(i)
			err := store.CommitPeerClientKeyPin(peer, pin)
			if err == nil {
				accepted.Add(1)
			} else if !errors.Is(err, errPeerPinStoreCapacity) {
				t.Errorf("commit: %v", err)
			}
		}()
	}
	workers.Wait()
	if accepted.Load() != 1 || store.Stats().PeerCount != 256 || store.Stats().CapacityRefusals != 15 {
		t.Fatalf("last-slot admission: accepted=%d stats=%+v", accepted.Load(), store.Stats())
	}
	peer, pin := testBoundedPin(17)
	if err := store.CommitPeerClientKeyPin(peer, pin); err != nil {
		t.Fatalf("identical head refused: %v", err)
	}
	pin.Generation++
	if err := store.CommitPeerClientKeyPin(peer, pin); err != nil {
		t.Fatalf("existing peer could not ratchet at capacity: %v", err)
	}
	previous := pin
	previous.Generation--
	if err := store.CommitPeerClientKeyPin(peer, previous); !errors.Is(err, errPeerPinStoreRollback) {
		t.Fatalf("rollback=%v", err)
	}
	conflict := pin
	conflict.PublicKey[0]--
	if err := store.CommitPeerClientKeyPin(peer, conflict); !errors.Is(err, errPeerPinStoreRollback) {
		t.Fatalf("same-generation conflict=%v", err)
	}
	store.Close()
	restored, err := newBoundedPeerClientKeyPinStore(state, nil, budget)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if restored.Stats().PeerCount != 256 || !restored.SignedHistorySeen() {
		t.Fatal("pin+latch commit did not survive reload")
	}
	for i := 0; i < 255; i++ {
		key, want := testBoundedPin(i)
		if i == 17 {
			want = pin
		}
		got, ok, err := restored.GetPeerClientKeyPinChecked(key)
		if err != nil || !ok || got != want {
			t.Fatalf("replacement changed slot %d: %v %v", i, ok, err)
		}
	}
	info, err := os.Stat(restored.path)
	if err != nil || info.Size() > peerPinStoreMaxFileByteCount {
		t.Fatalf("unbounded output: %v %v", info, err)
	}
}

func TestBoundedPeerPinStorePersistenceFailureKeepsPinAndLatch(t *testing.T) {
	for _, phase := range []string{"create", "write", "sync", "close", "rename", "directory sync", "nonregular target"} {
		t.Run(phase, func(t *testing.T) {
			state, budget := pinStoreTestOwner(t)
			store, err := newBoundedPeerClientKeyPinStore(state, nil, budget)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			store.testingCommitStep = func(step string) error {
				if step == phase {
					return &os.PathError{Op: step, Path: "sensitive-local-path", Err: os.ErrPermission}
				}
				return nil
			}
			if phase == "nonregular target" {
				if err := os.Mkdir(store.path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			peer, pin := testBoundedPin(1)
			if err := store.CommitPeerClientKeyPin(peer, pin); err != errPeerPinStoreIO {
				t.Fatalf("commit leaked non-class error: %v", err)
			}
			if store.data.count != 0 || store.data.signedSeen || store.SignedHistorySeen() || store.Stats().PersistenceFailures != 1 {
				t.Fatal("failed durable commit changed pin/latch")
			}
			if _, _, err := store.GetPeerClientKeyPinChecked(peer); err != errPeerPinStoreIO {
				t.Fatal("failed store did not fail closed")
			}
			temporary, err := filepath.Glob(filepath.Join(state.localStorageDir, ".peer_client_key_pins-*"))
			if err != nil || len(temporary) != 0 {
				t.Fatalf("temporary ownership leaked: %v %v", temporary, err)
			}
		})
	}
}

func TestBoundedPeerPinStoreOwnerlessGenerationsCannotErasePins(t *testing.T) {
	state, _ := pinStoreTestOwner(t)
	root := connect.NewTransferMemoryBudget(2 * peerPinStoreMemoryByteCount)
	first, err := newBoundedPeerClientKeyPinStore(state, nil, root)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	// Prepare a stale snapshot while A still serves. A's later commit must be
	// reread at B's publication, and A must never write over B afterwards.
	second, err := prepareBoundedPeerClientKeyPinStore(state, nil, root)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	peerA, pinA := testBoundedPin(1)
	peerB, pinB := testBoundedPin(2)
	if err := first.CommitPeerClientKeyPin(peerA, pinA); err != nil {
		t.Fatal(err)
	}
	if err := second.activate(); err != nil {
		t.Fatal(err)
	}
	if err := second.CommitPeerClientKeyPin(peerB, pinB); err != nil {
		t.Fatal(err)
	}
	if err := first.CommitPeerClientKeyPin(peerA, pinA); err != errPeerPinStoreSuperseded {
		t.Fatalf("old anonymous owner could overwrite new pins: %v", err)
	}
	if _, _, err := first.GetPeerClientKeyPinChecked(peerA); err != errPeerPinStoreSuperseded {
		t.Fatalf("old anonymous owner could serve stale pins: %v", err)
	}
	if root.UsedByteCount() != 2*peerPinStoreMemoryByteCount {
		t.Fatal("overlapping generations lost their claims")
	}
	second.Close()
	if err := first.activate(); err != errPeerPinStoreSuperseded {
		t.Fatalf("closed successor resurrected retired owner: %v", err)
	}
	first.Close()
	restored, err := newBoundedPeerClientKeyPinStore(state, nil, root)
	if err != nil {
		t.Fatal(err)
	}
	for _, peer := range []connect.Id{peerA, peerB} {
		if _, ok, err := restored.GetPeerClientKeyPinChecked(peer); err != nil || !ok {
			t.Fatalf("durable pin disappeared: %v %v", ok, err)
		}
	}
	restored.Close()
	stats := root.Stats()
	if stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
		t.Fatal("generation claims did not balance")
	}
}

func TestBoundedPeerPinStoreFailedPreparationPreservesServingOwner(t *testing.T) {
	state, budget := pinStoreTestOwner(t)
	store, err := newBoundedPeerClientKeyPinStore(state, nil, budget)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	refused, err := newBoundedPeerClientKeyPinStore(state, nil, budget)
	if refused != nil || err != errPeerPinStoreBudget || state.peerPinStoreOwner != store {
		t.Fatal("refused generation displaced serving store")
	}
	peer, pin := testBoundedPin(1)
	if err := store.CommitPeerClientKeyPin(peer, pin); err != nil {
		t.Fatal(err)
	}
}

func TestBoundedPeerPinStoreLoadAllocationEnvelope(t *testing.T) {
	if runIsolatedLoadTest(t) {
		return
	}
	state, budget := pinStoreTestOwner(t)
	seedBoundedPins(t, state, 256)
	// Warm reflection metadata, then measure all constructor allocation, a
	// stricter upper bound than only simultaneously retained decoder roots.
	warm, err := newBoundedPeerClientKeyPinStore(state, nil, budget)
	if err != nil {
		t.Fatal(err)
	}
	warm.Close()
	for range 3 {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		store, err := newBoundedPeerClientKeyPinStore(state, nil, budget)
		runtime.ReadMemStats(&after)
		if err != nil {
			t.Fatal(err)
		}
		store.Close()
		allocated := after.TotalAlloc - before.TotalAlloc
		t.Logf("max-value 256-pin load allocated %d bytes within %d-byte claim", allocated, peerPinStoreMemoryByteCount)
		if allocated > peerPinStoreMemoryByteCount {
			t.Fatalf("fixed envelope is insufficient: allocated=%d", allocated)
		}
	}
}

func TestBoundedPeerPinStoreHostileInputAndSerializationEnvelope(t *testing.T) {
	if runIsolatedLoadTest(t) {
		return
	}
	for _, kind := range []string{"giant key", "nested", "padded valid pin", "invalid whitespace", "short bytes", "extra bytes", "null bytes", "null generation", "null latch"} {
		t.Run(kind, func(t *testing.T) {
			state, budget := pinStoreTestOwner(t)
			encoded := seedBoundedPins(t, state, 1)
			want := errPeerPinStoreCorrupt
			switch kind {
			case "giant key":
				encoded = []byte(`{"` + strings.Repeat("k", peerPinStoreMaxFileByteCount-8) + `":0}`)
			case "nested":
				encoded = []byte(`{"pins":` + strings.Repeat("[", 10000) + strings.Repeat("]", 10000) + `}`)
			case "padded valid pin":
				encoded = bytes.Replace(encoded, []byte(`"domain_digest":[`), append([]byte(`"domain_digest":[`), bytes.Repeat([]byte(" "), peerPinStoreMaxFileByteCount-len(encoded))...), 1)
				want = nil
			case "invalid whitespace":
				encoded = bytes.Replace(encoded, []byte(`"signed_history_seen":true`), []byte(`"signed_history_seen":t r u e`), 1)
			case "short bytes", "extra bytes", "null bytes":
				start := bytes.Index(encoded, []byte(`"domain_digest":`)) + len(`"domain_digest":`)
				end := start + bytes.IndexByte(encoded[start:], ']') + 1
				replace := []byte("[1]")
				if kind == "extra bytes" {
					replace = []byte("[" + strings.Repeat("1,", 32) + "1]")
				} else if kind == "null bytes" {
					replace = []byte("null")
				}
				encoded = append(append(append([]byte(nil), encoded[:start]...), replace...), encoded[end:]...)
			case "null generation":
				encoded = bytes.Replace(encoded, []byte(`18446744073709551614`), []byte(`null`), 1)
			case "null latch":
				encoded = bytes.Replace(encoded, []byte(`"signed_history_seen":true`), []byte(`"signed_history_seen":null`), 1)
			}
			if err := os.WriteFile(filepath.Join(state.localStorageDir, peerClientKeyPinsFileName), encoded, 0600); err != nil {
				t.Fatal(err)
			}
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			store, err := newBoundedPeerClientKeyPinStore(state, nil, budget)
			runtime.ReadMemStats(&after)
			store.Close()
			if !errors.Is(err, want) {
				t.Fatalf("load=%v want=%v", err, want)
			}
			allocated := after.TotalAlloc - before.TotalAlloc
			t.Logf("%s load allocated %d bytes", kind, allocated)
			if allocated > peerPinStoreMemoryByteCount {
				t.Fatalf("input exceeded fixed allocation envelope: %d", allocated)
			}
		})
	}
	state, budget := pinStoreTestOwner(t)
	seedBoundedPins(t, state, peerPinStoreMaxPeerCount)
	store, err := newBoundedPeerClientKeyPinStore(state, nil, budget)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	peer, pin := testBoundedPin(10)
	pin.Generation++
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err = store.CommitPeerClientKeyPin(peer, pin)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	// The already-retained fixed owner is ~164 KiB, and remains charged.
	if allocated+uint64(192*1024) > peerPinStoreMemoryByteCount {
		t.Fatalf("serialization exceeded owner+scratch claim: %d", allocated)
	}
	t.Logf("max-value full-store replacement serialization allocated %d bytes", allocated)
}
