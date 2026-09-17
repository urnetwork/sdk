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
	"os"
	"path/filepath"
	"sync"

	"github.com/urnetwork/connect/v2026"
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

// localStatePeerClientKeyPinStore is a `connect.PeerClientKeyPinStore` backed
// by one JSON file in the device's local storage.
//
// Held entirely in memory and rewritten whole on change. The set is bounded by
// the number of distinct providers a device has ever sealed to, which is small
// (the quality window is six), and a whole-file rewrite keeps the ratchet
// atomic: a partially written pin file that lost a peer's tier would be a
// silent downgrade, which is the exact failure this store exists to prevent.
type localStatePeerClientKeyPinStore struct {
	path string

	stateLock sync.Mutex
	storage   peerClientKeyPinsStorage
}

func newLocalStatePeerClientKeyPinStore(localStorageDir string) *localStatePeerClientKeyPinStore {
	store := &localStatePeerClientKeyPinStore{
		path: filepath.Join(localStorageDir, peerClientKeyPinsFileName),
		storage: peerClientKeyPinsStorage{
			SignedHistorySeen: false,
			Pins:              map[string]peerClientKeyPinRecord{},
		},
	}
	store.load()
	return store
}

// load reads the pin file. A missing or unreadable file yields an empty store,
// which is the correct failure direction only because a fresh install has no
// history to protect: it means a first run trusts on first use rather than
// refusing to connect. A CORRUPT file is treated the same way, which is a
// deliberate availability choice and the weakest point of this store.
func (self *localStatePeerClientKeyPinStore) load() {
	pinBytes, err := os.ReadFile(self.path)
	if err != nil {
		return
	}
	var storage peerClientKeyPinsStorage
	if err := json.Unmarshal(pinBytes, &storage); err != nil {
		return
	}
	if storage.Pins == nil {
		storage.Pins = map[string]peerClientKeyPinRecord{}
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.storage = storage
}

// flushWithLock rewrites the whole file. Callers hold stateLock.
func (self *localStatePeerClientKeyPinStore) flushWithLock() {
	pinBytes, err := json.Marshal(self.storage)
	if err != nil {
		return
	}
	os.WriteFile(self.path, pinBytes, LocalStorageFilePermissions)
}

func (self *localStatePeerClientKeyPinStore) GetPeerClientKeyPin(peerId connect.Id) (connect.ClientKeyPin, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	record, ok := self.storage.Pins[peerId.String()]
	if !ok {
		return connect.ClientKeyPin{}, false
	}
	return connect.ClientKeyPin{
		DomainDigest: record.DomainDigest,
		Signer:       record.Signer,
		Generation:   record.Generation,
		PublicKey:    record.PublicKey,
	}, true
}

func (self *localStatePeerClientKeyPinStore) SetPeerClientKeyPin(peerId connect.Id, pin connect.ClientKeyPin) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	existing, ok := self.storage.Pins[peerId.String()]
	if ok && pin.Generation < existing.Generation {
		// never ratchet down; the verifier already refuses a rewind, and this
		// is the belt to that braces
		return
	}
	self.storage.Pins[peerId.String()] = peerClientKeyPinRecord{
		DomainDigest: pin.DomainDigest,
		Signer:       pin.Signer,
		Generation:   pin.Generation,
		PublicKey:    pin.PublicKey,
	}
	self.flushWithLock()
}

func (self *localStatePeerClientKeyPinStore) SignedHistorySeen() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.storage.SignedHistorySeen
}

func (self *localStatePeerClientKeyPinStore) SetSignedHistorySeen() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.storage.SignedHistorySeen {
		return
	}
	self.storage.SignedHistorySeen = true
	self.flushWithLock()
}

// GetPeerClientKeyPinStore returns the device-scoped pin store. A device
// without its own local storage (a hosted device sharing a host's directory)
// gets nil, which disables the ratchet: a hosted device's operator already
// runs its client, so there is no separate party for the ratchet to protect it
// from, and sharing one store across tenants would leak which providers a
// tenant has sealed to.
func (self *LocalState) GetPeerClientKeyPinStore() connect.PeerClientKeyPinStore {
	if self.localStorageDir == "" {
		return nil
	}
	return newLocalStatePeerClientKeyPinStore(self.localStorageDir)
}
