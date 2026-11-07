// Persistent tier ratchet for peer identity keys.
//
// Verifying a peer's signed key-registration history is only half the defence.
// The other half is refusing to go back: if a client accepts "no signed
// evidence" whenever none is offered, an operator that wants to substitute a
// key does not forge anything, it simply withholds the history. Omission is
// cheaper and quieter than forgery, so the tier may only ratchet upward.
//
// See connect/DESIGNNOTES3 §4.
package sdk

import (
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"sync"

	"github.com/urnetwork/connect"
)

const peerClientKeyPinsFileName = ".peer_client_key_pins"

type peerClientKeyPinRecord struct {
	DomainDigest [32]byte                 `json:"domain_digest"`
	Signer       connect.ClientKeyAddress `json:"signer"`
	Generation   uint64                   `json:"generation"`
	PublicKey    [32]byte                 `json:"public_key"`
}

type peerClientKeyPinsStorage struct {
	// SignedHistorySeen latches once ANY peer has been verified against signed
	// evidence through this store. After that, a peer offering none at all is
	// a downgrade rather than a legacy peer.
	SignedHistorySeen bool                              `json:"signed_history_seen"`
	Pins              map[string]peerClientKeyPinRecord `json:"pins"`
}

// Concurrent device-scoped ratchet. Verification snapshots and commits share
// stateLock across managers; staged persistence precedes publication in memory.
// A failed load or commit refuses subsequent checked admission. The app-private
// directory has one external owner; unrelated store instances must not write it.
type localStatePeerClientKeyPinStore struct {
	path string

	stateLock sync.Mutex
	storage   peerClientKeyPinsStorage
	failure   error
	// Installed only by tests before concurrent operations begin.
	testingCommitStep func(string) error
}

var _ connect.AtomicPeerClientKeyPinStore = (*localStatePeerClientKeyPinStore)(nil)

// Loads before publication; an unreadable or corrupt existing file fails closed.
func newLocalStatePeerClientKeyPinStore(localStorageDir string) *localStatePeerClientKeyPinStore {
	store := &localStatePeerClientKeyPinStore{
		path: filepath.Join(localStorageDir, peerClientKeyPinsFileName),
		storage: peerClientKeyPinsStorage{
			SignedHistorySeen: false,
			Pins:              map[string]peerClientKeyPinRecord{},
		},
	}
	store.failure = store.load()
	return store
}

// Only the constructor calls this, before any manager can obtain the store.
// Decode the same strict records as the bounded store without imposing its cap.
func (self *localStatePeerClientKeyPinStore) load() error {
	leaf, err := os.Lstat(self.path)
	if errors.Is(err, os.ErrNotExist) {
		parent, parentErr := os.Stat(filepath.Dir(self.path))
		if parentErr != nil || !parent.IsDir() {
			return errPeerPinStoreIO
		}
		return nil
	}
	if err != nil || !leaf.Mode().IsRegular() {
		return errPeerPinStoreIO
	}
	file, err := openBoundedPeerPinFile(self.path)
	if err != nil {
		return errPeerPinStoreIO
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(leaf, info) {
		return errPeerPinStoreIO
	}
	storage := peerClientKeyPinsStorage{Pins: map[string]peerClientKeyPinRecord{}}
	decoder := json.NewDecoder(file)
	if !pinJSONDelimiter(decoder, '{') {
		return errPeerPinStoreCorrupt
	}
	seen := uint8(0)
	for decoder.More() {
		field, err := decoder.Token()
		if err != nil {
			return errPeerPinStoreCorrupt
		}
		switch field {
		case "signed_history_seen":
			if seen&1 != 0 {
				return errPeerPinStoreCorrupt
			}
			seen |= 1
			value, err := decoder.Token()
			var ok bool
			storage.SignedHistorySeen, ok = value.(bool)
			if err != nil || !ok {
				return errPeerPinStoreCorrupt
			}
		case "pins":
			if seen&2 != 0 || !pinJSONDelimiter(decoder, '{') {
				return errPeerPinStoreCorrupt
			}
			seen |= 2
			for decoder.More() {
				value, err := decoder.Token()
				key, ok := value.(string)
				if err != nil || !ok {
					return errPeerPinStoreCorrupt
				}
				peer, err := connect.ParseId(key)
				if _, duplicate := storage.Pins[key]; err != nil || peer.String() != key || duplicate {
					return errPeerPinStoreCorrupt
				}
				pin, err := decodeBoundedPeerPin(decoder)
				if err != nil {
					return err
				}
				storage.Pins[key] = peerClientKeyPinRecord(pin)
			}
			if !pinJSONDelimiter(decoder, '}') {
				return errPeerPinStoreCorrupt
			}
		default:
			return errPeerPinStoreCorrupt
		}
	}
	if seen != 3 || !pinJSONDelimiter(decoder, '}') {
		return errPeerPinStoreCorrupt
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errPeerPinStoreCorrupt
	}
	// Older writers persisted the pin before the separate global latch.
	// Valid durable pins prove signed history even after a crash between them.
	storage.SignedHistorySeen = storage.SignedHistorySeen || len(storage.Pins) != 0
	self.storage = storage
	return nil
}

// Reads both pieces of verification evidence in one store generation.
func (self *localStatePeerClientKeyPinStore) ReadPeerClientKeyPinSnapshot(peer connect.Id) (connect.ClientKeyPinSnapshot, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.failure != nil {
		return connect.ClientKeyPinSnapshot{}, self.failure
	}
	return self.snapshotWithLock(peer), nil
}

// The absent pin is canonical zero state; the global latch is always compared.
func (self *localStatePeerClientKeyPinStore) snapshotWithLock(peer connect.Id) connect.ClientKeyPinSnapshot {
	record, found := self.storage.Pins[peer.String()]
	return connect.ClientKeyPinSnapshot{
		Pin: connect.ClientKeyPin(record), Pinned: found, SignedHistorySeen: self.storage.SignedHistorySeen,
	}
}

// A nil next pin admits read-only fallback only while its evidence is current.
// A changed snapshot returns false without mutating or poisoning the store.
// Non-nil pins must come from full-chain verification against expected and the
// caller's signer policy; only that path may authorize a forward signer change.
func (self *localStatePeerClientKeyPinStore) CompareAndSwapPeerClientKeyPin(peer connect.Id, expected connect.ClientKeyPinSnapshot, nextPin *connect.ClientKeyPin) (bool, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.failure != nil {
		return false, self.failure
	}
	if self.snapshotWithLock(peer) != expected {
		return false, nil
	}
	if nextPin == nil {
		return true, nil
	}
	if err := self.commitPeerClientKeyPinWithLock(peer, *nextPin, true); err != nil {
		return false, err
	}
	return true, nil
}

// Compatibility read; checked callers retain load and persistence errors.
func (self *localStatePeerClientKeyPinStore) GetPeerClientKeyPinChecked(peer connect.Id) (connect.ClientKeyPin, bool, error) {
	snapshot, err := self.ReadPeerClientKeyPinSnapshot(peer)
	return snapshot.Pin, snapshot.Pinned, err
}

// Legacy callers cannot observe failure; identity admission uses the atomic API.
func (self *localStatePeerClientKeyPinStore) GetPeerClientKeyPin(peer connect.Id) (connect.ClientKeyPin, bool) {
	pin, found, _ := self.GetPeerClientKeyPinChecked(peer)
	return pin, found
}

// Atomically retains the verified pin and global latch for checked callers.
func (self *localStatePeerClientKeyPinStore) CommitPeerClientKeyPin(peer connect.Id, pin connect.ClientKeyPin) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.failure != nil {
		return self.failure
	}
	return self.commitPeerClientKeyPinWithLock(peer, pin, false)
}

// Callers hold stateLock through validation, persistence and publication.
func (self *localStatePeerClientKeyPinStore) commitPeerClientKeyPinWithLock(peer connect.Id, pin connect.ClientKeyPin, allowSignerRotation bool) error {
	existing := self.snapshotWithLock(peer)
	if existing.Pinned && !peerClientKeyPinCanAdvance(existing.Pin, pin, allowSignerRotation) {
		return errPeerPinStoreRollback
	}
	candidate := peerClientKeyPinsStorage{SignedHistorySeen: true, Pins: maps.Clone(self.storage.Pins)}
	candidate.Pins[peer.String()] = peerClientKeyPinRecord(pin)
	return self.persistWithLock(candidate)
}

// Keeps the prior in-memory ratchet on error and refuses subsequent admission.
func (self *localStatePeerClientKeyPinStore) persistWithLock(candidate peerClientKeyPinsStorage) error {
	encoded, err := json.Marshal(candidate)
	if err == nil {
		err = persistPeerClientKeyPinsWithLock(self.path, encoded, self.testingCommitStep)
	}
	if err != nil {
		self.failure = errPeerPinStoreIO
		return self.failure
	}
	self.storage = candidate
	return nil
}

// Retains the legacy interface without permitting it to bypass atomic commits.
func (self *localStatePeerClientKeyPinStore) SetPeerClientKeyPin(peer connect.Id, pin connect.ClientKeyPin) {
	_ = self.CommitPeerClientKeyPin(peer, pin)
}

// Legacy observation; checked admission must read the complete snapshot.
func (self *localStatePeerClientKeyPinStore) SignedHistorySeen() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.storage.SignedHistorySeen
}

// Keeps latch-only compatibility writes durable before publication.
func (self *localStatePeerClientKeyPinStore) SetSignedHistorySeen() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.failure != nil || self.storage.SignedHistorySeen {
		return
	}
	candidate := self.storage
	candidate.SignedHistorySeen = true
	_ = self.persistWithLock(candidate)
}

// A conditional commit follows full chain/signer-policy verification against
// its exact snapshot. Unconditional callers cannot replace the pinned signer.
func peerClientKeyPinCanAdvance(previous, next connect.ClientKeyPin, allowSignerRotation bool) bool {
	signerAllowed := previous.Signer == next.Signer || allowSignerRotation && previous.Generation < next.Generation
	return previous.Generation <= next.Generation && previous.DomainDigest == next.DomainDigest &&
		signerAllowed && (previous.Generation != next.Generation || previous.PublicKey == next.PublicKey)
}

// Borrows encoded bytes while the caller holds its store lock. Rename publishes
// only a complete file. A directory-sync failure after rename has an uncertain
// durability outcome; callers keep their old memory and fail closed thereafter.
func persistPeerClientKeyPinsWithLock(path string, encoded []byte, testingCommitStep func(string) error) error {
	fail := func(step string) bool { return testingCommitStep != nil && testingCommitStep(step) != nil }
	if info, err := os.Lstat(path); (err == nil && !info.Mode().IsRegular()) ||
		(err != nil && !errors.Is(err, os.ErrNotExist)) {
		return errPeerPinStoreIO
	}
	if fail("create") {
		return errPeerPinStoreIO
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".peer_client_key_pins-*")
	if err != nil {
		return errPeerPinStoreIO
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	defer file.Close()
	if fail("write") {
		return errPeerPinStoreIO
	}
	if n, err := file.Write(encoded); err != nil || n != len(encoded) {
		return errPeerPinStoreIO
	}
	if fail("sync") || file.Sync() != nil || fail("close") || file.Close() != nil {
		return errPeerPinStoreIO
	}
	if fail("rename") || os.Rename(temporary, path) != nil {
		return errPeerPinStoreIO
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return errPeerPinStoreIO
	}
	defer directory.Close()
	if fail("directory sync") || directory.Sync() != nil {
		return errPeerPinStoreIO
	}
	return nil
}

// peerClientKeyPinStore returns the device-scoped pin store. A device
// without its own local storage (a hosted device sharing a host's directory)
// gets nil, which disables the ratchet: a hosted device's operator already
// runs its client, so there is no separate party for the ratchet to protect it
// from, and sharing one store across tenants would leak which providers a
// tenant has sealed to. This stays unexported because connect's store interface
// is an internal wiring seam that gomobile cannot bind.
func (self *LocalState) peerClientKeyPinStore() connect.PeerClientKeyPinStore {
	if self.localStorageDir == "" {
		return nil
	}
	return newLocalStatePeerClientKeyPinStore(self.localStorageDir)
}
