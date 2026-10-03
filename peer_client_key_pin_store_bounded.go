package sdk

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/urnetwork/connect"
)

const (
	peerPinStoreMemoryByteCount  = 1024 * 1024
	peerPinStoreMaxPeerCount     = 256
	peerPinStoreMaxFileByteCount = 128 * 1024
)

var (
	errPeerPinStoreBudget     = errors.New("peer identity pin store memory budget is full")
	errPeerPinStoreCapacity   = errors.New("peer identity pin store capacity is full")
	errPeerPinStoreOversize   = errors.New("peer identity pin store exceeds 128 KiB")
	errPeerPinStoreCorrupt    = errors.New("peer identity pin store is invalid")
	errPeerPinStoreIO         = errors.New("peer identity pin store persistence failed")
	errPeerPinStoreClosed     = errors.New("peer identity pin store is closed")
	errPeerPinStoreSuperseded = errors.New("peer identity pin store owner is superseded")
	errPeerPinStoreRollback   = errors.New("peer identity pin store refused a generation rollback or conflict")
)

type boundedPeerPinEntry struct {
	peer connect.Id
	pin  connect.ClientKeyPin
}

// One fixed owner, not a growing map or a global serialization pool. The
// admission includes 29 KiB of entries, 129 KiB of owned input/output, and
// bounded JSON decoder/token/scanner scratch below the 1 MiB envelope.
type boundedPeerPinData struct {
	entries    [peerPinStoreMaxPeerCount]boundedPeerPinEntry
	buffer     [peerPinStoreMaxFileByteCount + 1]byte
	count      int
	signedSeen bool
}

type boundedPeerPinStats struct {
	PeerCount           int
	CapacityRefusals    uint64
	PersistenceFailures uint64
	RollbackRefusals    uint64
	StateFailures       uint64
}

type boundedPeerClientKeyPinStore struct {
	stateLock  sync.Mutex
	localState *LocalState
	owner      *deviceAuthPublicationGate
	generation uint64
	budget     *connect.TransferMemoryBudget
	path       string
	data       *boundedPeerPinData
	failure    error
	stats      boundedPeerPinStats
	// Installed only by tests before concurrent operations begin.
	testingCommitStep func(string) error
}

var _ connect.CheckedPeerClientKeyPinStore = (*boundedPeerClientKeyPinStore)(nil)

func newBoundedPeerClientKeyPinStore(state *LocalState, owner *deviceAuthPublicationGate, budget *connect.TransferMemoryBudget) (*boundedPeerClientKeyPinStore, error) {
	store, err := prepareBoundedPeerClientKeyPinStore(state, owner, budget)
	if err != nil {
		return nil, err
	}
	if err := store.activate(); err != nil {
		store.Close()
		return nil, err
	}
	return store, nil
}

// Device construction prepares before provider allocation, but publishes only
// after auth succeeds. Failed preparation must not supersede a serving store.
func prepareBoundedPeerClientKeyPinStore(state *LocalState, owner *deviceAuthPublicationGate, budget *connect.TransferMemoryBudget) (*boundedPeerClientKeyPinStore, error) {
	if budget == nil || !budget.TryReserve(peerPinStoreMemoryByteCount) {
		return nil, errPeerPinStoreBudget
	}
	if state == nil || state.localStorageDir == "" || len(state.localStorageDir) > 4096 {
		budget.Release(peerPinStoreMemoryByteCount)
		return nil, errPeerPinStoreIO
	}
	store := &boundedPeerClientKeyPinStore{
		localState: state, owner: owner, budget: budget,
		path: filepath.Join(state.localStorageDir, peerClientKeyPinsFileName),
		data: new(boundedPeerPinData),
	}
	state.authStateLock.Lock()
	err := store.loadWithLock()
	if err == nil {
		state.peerPinStoreGeneration++
		store.generation = state.peerPinStoreGeneration
	}
	state.authStateLock.Unlock()
	if err != nil {
		store.Close()
		return nil, err
	}
	return store, nil
}

// Publish auth first, then reread under its lock: a retiring generation may
// have committed another pin between initial preparation and publication.
func (self *boundedPeerClientKeyPinStore) activate() error {
	self.localState.authStateLock.Lock()
	defer self.localState.authStateLock.Unlock()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.data == nil {
		return errPeerPinStoreClosed
	}
	if self.failure != nil {
		return self.failure
	}
	if self.owner != nil && self.localState.deviceAuthOwner != self.owner {
		return errPeerPinStoreSuperseded
	}
	if self.localState.peerPinStorePublishedGeneration > self.generation {
		return errPeerPinStoreSuperseded
	}
	clear(self.data.entries[:])
	self.data.count, self.data.signedSeen = 0, false
	self.stats.PeerCount = 0
	if err := self.loadWithLock(); err != nil {
		return err
	}
	self.localState.peerPinStoreOwner = self
	self.localState.peerPinStorePublishedGeneration = self.generation
	return nil
}

func (self *boundedPeerClientKeyPinStore) loadWithLock() error {
	// The private application directory and its parent are trusted and owned
	// by one LocalState. Never follow an untrusted leaf or wait on a FIFO. The
	// mobile opener also uses O_NOFOLLOW|O_NONBLOCK against leaf replacement.
	leaf, err := os.Lstat(self.path)
	if errors.Is(err, os.ErrNotExist) {
		// Only a genuinely absent leaf in an existing directory is fresh.
		// A dangling link or missing parent must not erase an earlier ratchet.
		if _, leafErr := os.Lstat(self.path); !errors.Is(leafErr, os.ErrNotExist) {
			return errPeerPinStoreIO
		}
		parent, parentErr := os.Stat(filepath.Dir(self.path))
		if parentErr != nil || !parent.IsDir() {
			return errPeerPinStoreIO
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %v", errPeerPinStoreIO, err)
	}
	if !leaf.Mode().IsRegular() {
		return errPeerPinStoreIO
	}
	file, err := openBoundedPeerPinFile(self.path)
	if err != nil {
		return fmt.Errorf("%w: %v", errPeerPinStoreIO, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("%w: %v", errPeerPinStoreIO, err)
	}
	if !info.Mode().IsRegular() || !os.SameFile(leaf, info) {
		return errPeerPinStoreIO
	}
	if info.Size() > peerPinStoreMaxFileByteCount {
		return errPeerPinStoreOversize
	}
	n, err := io.ReadFull(file, self.data.buffer[:])
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return fmt.Errorf("%w: %v", errPeerPinStoreIO, err)
	}
	if n > peerPinStoreMaxFileByteCount {
		return errPeerPinStoreOversize
	}
	encoded, err := compactBoundedPeerPinJSON(self.data.buffer[:n])
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	if err := self.decodeWithLock(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errPeerPinStoreCorrupt
	}
	clear(self.data.buffer[:n])
	return nil
}

// A legal 128-KiB file may put nearly all its whitespace inside one byte
// array. Feeding that token to json.Decoder doubles its scratch on each load.
// Validate first (so removing whitespace cannot repair malformed JSON), then
// compact in the already admitted input owner. No second output allocation.
func compactBoundedPeerPinJSON(data []byte) ([]byte, error) {
	depth, quoted, escaped := 0, false, false
	for _, value := range data {
		if quoted {
			if escaped {
				escaped = false
			} else if value == '\\' {
				escaped = true
			} else if value == '"' {
				quoted = false
			}
		} else {
			switch value {
			case '"':
				quoted = true
			case '{', '[':
				depth++
				// Store -> pins -> pin -> 32-byte array is the deepest schema.
				if depth > 4 {
					return nil, errPeerPinStoreCorrupt
				}
			case '}', ']':
				depth--
			}
		}
	}
	if !json.Valid(data) {
		return nil, errPeerPinStoreCorrupt
	}
	write := 0
	quoted, escaped = false, false
	for _, value := range data {
		if quoted {
			if escaped {
				escaped = false
			} else if value == '\\' {
				escaped = true
			} else if value == '"' {
				quoted = false
			}
		} else if value == '"' {
			quoted = true
		} else if value == ' ' || value == '\t' || value == '\r' || value == '\n' {
			continue
		}
		data[write] = value
		write++
	}
	return data[:write], nil
}

func pinJSONDelimiter(decoder *json.Decoder, want json.Delim) bool {
	token, err := decoder.Token()
	return err == nil && token == want
}

func (self *boundedPeerClientKeyPinStore) decodeWithLock(decoder *json.Decoder) error {
	if !pinJSONDelimiter(decoder, '{') {
		return errPeerPinStoreCorrupt
	}
	seen := uint8(0)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return errPeerPinStoreCorrupt
		}
		switch token {
		case "signed_history_seen":
			if seen&1 != 0 {
				return errPeerPinStoreCorrupt
			}
			seen |= 1
			value, err := decoder.Token()
			seenValue, ok := value.(bool)
			if err != nil || !ok {
				return errPeerPinStoreCorrupt
			}
			self.data.signedSeen = seenValue
		case "pins":
			if seen&2 != 0 || !pinJSONDelimiter(decoder, '{') {
				return errPeerPinStoreCorrupt
			}
			seen |= 2
			for decoder.More() {
				// Refuse before decoding/retaining an additional record.
				if self.data.count == peerPinStoreMaxPeerCount {
					return errPeerPinStoreCapacity
				}
				token, err := decoder.Token()
				key, ok := token.(string)
				if err != nil || !ok || len(key) > 36 {
					return errPeerPinStoreCorrupt
				}
				peer, err := connect.ParseId(key)
				if err != nil || peer.String() != key || self.indexWithLock(peer) >= 0 {
					return errPeerPinStoreCorrupt
				}
				pin, err := decodeBoundedPeerPin(decoder)
				if err != nil {
					return err
				}
				self.data.entries[self.data.count] = boundedPeerPinEntry{peer: peer, pin: pin}
				self.data.count++
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
	self.stats.PeerCount = self.data.count
	return nil
}

func decodeBoundedPeerPin(decoder *json.Decoder) (connect.ClientKeyPin, error) {
	var pin connect.ClientKeyPin
	if !pinJSONDelimiter(decoder, '{') {
		return pin, errPeerPinStoreCorrupt
	}
	seen := uint8(0)
	for decoder.More() {
		field, err := decoder.Token()
		if err != nil {
			return pin, errPeerPinStoreCorrupt
		}
		var bit uint8
		var value any
		switch field {
		case "domain_digest":
			bit, value = 1, (*boundedPinBytes)(&pin.DomainDigest)
		case "signer":
			bit, value = 2, &pin.Signer
		case "generation":
			bit, value = 4, (*boundedPinGeneration)(&pin.Generation)
		case "public_key":
			bit, value = 8, (*boundedPinBytes)(&pin.PublicKey)
		default:
			return pin, errPeerPinStoreCorrupt
		}
		if seen&bit != 0 || decoder.Decode(value) != nil {
			return pin, errPeerPinStoreCorrupt
		}
		seen |= bit
	}
	if seen != 15 || !pinJSONDelimiter(decoder, '}') {
		return pin, errPeerPinStoreCorrupt
	}
	return pin, nil
}

// json's fixed-array decoder silently pads short arrays, drops extra entries,
// and accepts null. Identity material needs exactly 32 actual byte values.
// UnmarshalJSON borrows the decoder's already bounded token, without scratch.
type boundedPinBytes [32]byte

func (self *boundedPinBytes) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) < 2 || data[0] != '[' || data[len(data)-1] != ']' {
		return errPeerPinStoreCorrupt
	}
	data = data[1 : len(data)-1]
	for i := range self {
		field := data
		comma := bytes.IndexByte(data, ',')
		if i < len(self)-1 {
			if comma < 0 {
				return errPeerPinStoreCorrupt
			}
			field, data = data[:comma], data[comma+1:]
		} else if comma >= 0 {
			return errPeerPinStoreCorrupt
		}
		value, err := strconv.ParseUint(string(bytes.TrimSpace(field)), 10, 8)
		if err != nil {
			return errPeerPinStoreCorrupt
		}
		self[i] = byte(value)
	}
	return nil
}

type boundedPinGeneration uint64

func (self *boundedPinGeneration) UnmarshalJSON(data []byte) error {
	value, err := strconv.ParseUint(string(data), 10, 64)
	if err != nil {
		return errPeerPinStoreCorrupt
	}
	*self = boundedPinGeneration(value)
	return nil
}

func (self *boundedPeerClientKeyPinStore) indexWithLock(peer connect.Id) int {
	for i := 0; i < self.data.count; i++ {
		if self.data.entries[i].peer == peer {
			return i
		}
	}
	return -1
}

func (self *boundedPeerClientKeyPinStore) checkWithLock() error {
	if self.data == nil {
		return errPeerPinStoreClosed
	}
	if self.failure != nil {
		return self.failure
	}
	if self.localState.peerPinStoreOwner != self {
		return errPeerPinStoreSuperseded
	}
	if self.owner != nil && self.localState.deviceAuthOwner != self.owner {
		return errPeerPinStoreSuperseded
	}
	return nil
}

func (self *boundedPeerClientKeyPinStore) GetPeerClientKeyPinChecked(peer connect.Id) (connect.ClientKeyPin, bool, error) {
	self.localState.authStateLock.Lock()
	defer self.localState.authStateLock.Unlock()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if err := self.checkWithLock(); err != nil {
		self.stats.StateFailures++
		return connect.ClientKeyPin{}, false, err
	}
	if i := self.indexWithLock(peer); i >= 0 {
		return self.data.entries[i].pin, true, nil
	}
	return connect.ClientKeyPin{}, false, nil
}

func (self *boundedPeerClientKeyPinStore) GetPeerClientKeyPin(peer connect.Id) (connect.ClientKeyPin, bool) {
	pin, found, _ := self.GetPeerClientKeyPinChecked(peer)
	return pin, found
}

func (self *boundedPeerClientKeyPinStore) CommitPeerClientKeyPin(peer connect.Id, pin connect.ClientKeyPin) error {
	self.localState.authStateLock.Lock()
	defer self.localState.authStateLock.Unlock()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if err := self.checkWithLock(); err != nil {
		self.stats.StateFailures++
		return err
	}
	i := self.indexWithLock(peer)
	if i < 0 && self.data.count == peerPinStoreMaxPeerCount {
		self.stats.CapacityRefusals++
		return errPeerPinStoreCapacity
	}
	if i >= 0 {
		previous := self.data.entries[i].pin
		if pin.Generation < previous.Generation || pin.DomainDigest != previous.DomainDigest || pin.Signer != previous.Signer ||
			(pin.Generation == previous.Generation && pin.PublicKey != previous.PublicKey) {
			self.stats.RollbackRefusals++
			return errPeerPinStoreRollback
		}
	}
	if err := self.persistWithLock(peer, pin, i, true); err != nil {
		self.stats.PersistenceFailures++
		self.failure = errPeerPinStoreIO
		return errPeerPinStoreIO
	}
	if i < 0 {
		i = self.data.count
		self.data.count++
	}
	self.data.entries[i] = boundedPeerPinEntry{peer: peer, pin: pin}
	self.data.signedSeen = true
	self.stats.PeerCount = self.data.count
	return nil
}

func (self *boundedPeerClientKeyPinStore) SetPeerClientKeyPin(peer connect.Id, pin connect.ClientKeyPin) {
	_ = self.CommitPeerClientKeyPin(peer, pin)
}

func (self *boundedPeerClientKeyPinStore) SignedHistorySeen() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.data != nil && self.data.signedSeen
}

func (self *boundedPeerClientKeyPinStore) SetSignedHistorySeen() {
	self.localState.authStateLock.Lock()
	defer self.localState.authStateLock.Unlock()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.checkWithLock() != nil || self.data.signedSeen {
		return
	}
	if err := self.persistWithLock(connect.Id{}, connect.ClientKeyPin{}, -1, false); err != nil {
		self.stats.PersistenceFailures++
		self.failure = errPeerPinStoreIO
		return
	}
	self.data.signedSeen = true
}

func appendPinBytesJSON(dst []byte, value []byte) []byte {
	dst = append(dst, '[')
	for i, b := range value {
		if i != 0 {
			dst = append(dst, ',')
		}
		dst = strconv.AppendUint(dst, uint64(b), 10)
	}
	return append(dst, ']')
}

func appendBoundedPeerPin(dst []byte, entry boundedPeerPinEntry) []byte {
	dst = append(dst, '"')
	dst = append(dst, entry.peer.String()...)
	dst = append(dst, `":{"domain_digest":`...)
	dst = appendPinBytesJSON(dst, entry.pin.DomainDigest[:])
	dst = append(dst, `,"signer":"0x`...)
	dst = hex.AppendEncode(dst, entry.pin.Signer[:])
	dst = append(dst, '"')
	dst = append(dst, `,"generation":`...)
	dst = strconv.AppendUint(dst, entry.pin.Generation, 10)
	dst = append(dst, `,"public_key":`...)
	dst = appendPinBytesJSON(dst, entry.pin.PublicKey[:])
	return append(dst, '}')
}

func (self *boundedPeerClientKeyPinStore) persistWithLock(peer connect.Id, pin connect.ClientKeyPin, replace int, hasPin bool) error {
	buffer := append(self.data.buffer[:0:peerPinStoreMaxFileByteCount], `{"signed_history_seen":true,"pins":{`...)
	count := self.data.count
	if hasPin && replace < 0 {
		count++
	}
	for i := 0; i < count; i++ {
		// A canonical entry is <512 bytes even with every byte 255 and a
		// maximum uint64 generation. Never allow append to grow this owner.
		if len(buffer)+512+3 > peerPinStoreMaxFileByteCount {
			return errPeerPinStoreOversize
		}
		if i != 0 {
			buffer = append(buffer, ',')
		}
		entry := boundedPeerPinEntry{peer: peer, pin: pin}
		if i < self.data.count && (!hasPin || i != replace) {
			entry = self.data.entries[i]
		}
		buffer = appendBoundedPeerPin(buffer, entry)
	}
	buffer = append(buffer, '}', '}')
	// An attacker-controlled leaf is never used as either input or a commit
	// target. Rename itself never follows the leaf; the app-private parent
	// directory must not be concurrently replaced outside this LocalState.
	if info, err := os.Lstat(self.path); (err == nil && !info.Mode().IsRegular()) ||
		(err != nil && !errors.Is(err, os.ErrNotExist)) {
		return errPeerPinStoreIO
	}
	if self.failCommitStep("create") {
		return errPeerPinStoreIO
	}
	file, err := os.CreateTemp(filepath.Dir(self.path), ".peer_client_key_pins-*")
	if err != nil {
		return fmt.Errorf("%w: %v", errPeerPinStoreIO, err)
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	defer file.Close()
	if self.failCommitStep("write") {
		return errPeerPinStoreIO
	}
	if n, err := file.Write(buffer); err != nil || n != len(buffer) {
		return errPeerPinStoreIO
	}
	if self.failCommitStep("sync") || file.Sync() != nil || self.failCommitStep("close") || file.Close() != nil {
		return errPeerPinStoreIO
	}
	if self.failCommitStep("rename") || os.Rename(temporary, self.path) != nil {
		return errPeerPinStoreIO
	}
	directory, err := os.Open(filepath.Dir(self.path))
	if err != nil {
		return errPeerPinStoreIO
	}
	defer directory.Close()
	if self.failCommitStep("directory sync") || directory.Sync() != nil {
		return errPeerPinStoreIO
	}
	clear(buffer)
	return nil
}

func (self *boundedPeerClientKeyPinStore) failCommitStep(step string) bool {
	return self.testingCommitStep != nil && self.testingCommitStep(step) != nil
}

func (self *boundedPeerClientKeyPinStore) Stats() boundedPeerPinStats {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.stats
}

func (self *boundedPeerClientKeyPinStore) Close() {
	if self == nil {
		return
	}
	self.localState.authStateLock.Lock()
	defer self.localState.authStateLock.Unlock()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.data == nil {
		return
	}
	self.data = nil
	self.stats.PeerCount = 0
	self.testingCommitStep = nil
	if self.localState.peerPinStoreOwner == self {
		self.localState.peerPinStoreOwner = nil
	}
	self.budget.Release(peerPinStoreMemoryByteCount)
}
