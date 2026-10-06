package sdk

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/urnetwork/connect/v2026"
)

// local_state_extender.go — the persisted extender state (EXTENDER.md E1, D5,
// E7, F3).
//
// Five dot files, all following the shape of the other local-state files: the
// directory envelope, written by connect through the store adapter below, the
// latest reset the directory has applied, the gossip mode the user chose, the
// identity key, and the provider extender setting the user chose.
//
// The directory is a cache. Every read failure -- missing, unreadable, corrupt
// -- reads as no directory at all, and the client rediscovers, so nothing here
// can keep a launch from succeeding.

// The serialized extender directory (E1).
const extenderStoreFileName = ".extenders"

// The persisted gossip role override (D5).
const extenderGossipModeFileName = ".extender_gossip_mode"

// The latest reset the directory above has applied (E7).
const extenderResetIdFileName = ".extender_reset_id"

// The id of the latest reset the stored directory has applied, empty when it
// has applied none or the file cannot be read: a reset the values bring is
// then applied again, which costs a rediscovery and nothing else.
func (self *LocalState) getExtenderResetId() string {
	path := filepath.Join(self.localStorageDir, extenderResetIdFileName)
	resetIdBytes, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(resetIdBytes))
}

// Records the latest reset the stored directory has applied. Never written by
// a process whose extender store is read-only (K5), for the reason the store
// is not: the file sits beside the directory it describes, and only the
// process that writes the directory may say what it has applied.
func (self *LocalState) setExtenderResetId(resetId string) error {
	if GetExtenderStoreReadOnly() {
		return nil
	}
	path := filepath.Join(self.localStorageDir, extenderResetIdFileName)
	return os.WriteFile(path, []byte(resetId), LocalStorageFilePermissions)
}

// The stored directory envelope, or nil when there is none.
func (self *LocalState) getExtenders() ([]byte, error) {
	path := filepath.Join(self.localStorageDir, extenderStoreFileName)
	stateBytes, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return stateBytes, nil
}

// Replaces the stored directory envelope. Empty removes the file, so a cleared
// directory does not leave stale entries behind for the next launch.
func (self *LocalState) setExtenders(stateBytes []byte) error {
	path := filepath.Join(self.localStorageDir, extenderStoreFileName)
	if len(stateBytes) == 0 {
		os.Remove(path)
		return nil
	}
	return os.WriteFile(path, stateBytes, LocalStorageFilePermissions)
}

// The persisted gossip mode. Unset, unreadable or unknown all read as auto,
// which is the mode that lets the platform rule decide (D5).
func (self *LocalState) GetExtenderGossipMode() string {
	path := filepath.Join(self.localStorageDir, extenderGossipModeFileName)
	modeBytes, err := os.ReadFile(path)
	if err != nil {
		return ExtenderGossipModeAuto
	}
	return NormalExtenderGossipMode(string(modeBytes))
}

// Persists the gossip mode. An unknown value is stored as auto rather than
// refused, so a newer app's mode degrades to the platform rule here instead of
// leaving the previous mode in force.
func (self *LocalState) SetExtenderGossipMode(mode string) error {
	path := filepath.Join(self.localStorageDir, extenderGossipModeFileName)
	return os.WriteFile(
		path,
		[]byte(NormalExtenderGossipMode(mode)),
		LocalStorageFilePermissions,
	)
}

// The provider extender setting (F3, G1). A file that is not there is a
// provider that has never been asked, and the device default applies
// (DeviceLocalSettings.DefaultProvideExtender, on unless the embedder turned
// it off).
const provideExtenderFileName = ".provide_extender"

// The persisted provider extender setting. A space that stores none reads as
// on, the sdk default of F3. A device reads the stored value itself instead,
// so that its own default applies where there is none, which is what
// DeviceLocal.GetProvideExtender answers.
func (self *LocalState) GetProvideExtender() bool {
	if provideExtender, stored := self.getStoredProvideExtender(); stored {
		return provideExtender
	}
	return true
}

// The stored provider extender setting, and whether the space stores one at
// all.
//
// Read from its file once and cached after. There is one LocalState per space
// and every writer goes through SetProvideExtender, so the cache is the setting
// for the life of the process, shared by every device on the space (the miner
// swarm runs many), and the status, which reads it on every derivation, never
// touches the disk (N4).
func (self *LocalState) getStoredProvideExtender() (provideExtender bool, stored bool) {
	self.provideExtenderLock.Lock()
	defer self.provideExtenderLock.Unlock()
	if !self.provideExtenderLoaded {
		self.provideExtender, self.provideExtenderStored = self.readProvideExtender()
		self.provideExtenderLoaded = true
	}
	return self.provideExtender, self.provideExtenderStored
}

// Only an explicit true or false is a stored value. A missing, unreadable or
// corrupt file stores none, so the device default applies rather than a value
// nobody chose: a corrupt file never silently opts a provider out of a default
// that is on, nor in where the embedder's default is off.
func (self *LocalState) readProvideExtender() (provideExtender bool, stored bool) {
	path := filepath.Join(self.localStorageDir, provideExtenderFileName)
	stateBytes, err := os.ReadFile(path)
	if err != nil {
		return false, false
	}
	switch strings.TrimSpace(string(stateBytes)) {
	case "true":
		return true, true
	case "false":
		return false, true
	default:
		return false, false
	}
}

// Persists the provider extender setting. The cache takes the value whether or
// not the write succeeds, so the setting applies for the session either way,
// and the write error is returned for the caller to log, as the device does.
func (self *LocalState) SetProvideExtender(provideExtender bool) error {
	self.provideExtenderLock.Lock()
	defer self.provideExtenderLock.Unlock()
	self.provideExtender = provideExtender
	self.provideExtenderStored = true
	self.provideExtenderLoaded = true

	path := filepath.Join(self.localStorageDir, provideExtenderFileName)
	value := "false"
	if provideExtender {
		value = "true"
	}
	return os.WriteFile(path, []byte(value), LocalStorageFilePermissions)
}

// NormalExtenderGossipMode maps any input to one of the three modes (D5).
func NormalExtenderGossipMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case ExtenderGossipModeFeed:
		return ExtenderGossipModeFeed
	case ExtenderGossipModeMember:
		return ExtenderGossipModeMember
	default:
		return ExtenderGossipModeAuto
	}
}

// extenderStoreReadOnly is the process-wide read-only switch of K5. Two
// processes on ios share one app group directory and therefore one
// `.extenders` file: the packet tunnel extension, whose directory carries the
// dials that matter, and the app, which keeps its own directory for api dials.
// Both loading it is right -- the app starts warm -- but both writing it means
// two coalescing save loops overwriting each other's state, so the app process
// sets this and only the extension writes.
//
// Process-wide rather than per space or per manager because it is a property
// of the process, not of a space: the app builds its manager from the shared
// path and every space under it is the same read-only case. The default is
// read-write, so every other platform, and the extension itself, is unchanged.
var extenderStoreReadOnly atomic.Bool

// SetExtenderStoreReadOnly makes this process load the shared extender
// directory file but never write it (K5). The ios app process sets it before
// building its NetworkSpaceManager; nothing else should.
func SetExtenderStoreReadOnly(readOnly bool) {
	extenderStoreReadOnly.Store(readOnly)
}

// GetExtenderStoreReadOnly reports the switch above.
func GetExtenderStoreReadOnly() bool {
	return extenderStoreReadOnly.Load()
}

// localStateExtenderStore implements connect.ExtenderDirectoryStore over the
// dot file above, so a connect.ExtenderDirectory persists across restarts.
// Mirrors localStatePriorsStore's shape.
type localStateExtenderStore struct {
	localState *LocalState
}

func newLocalStateExtenderStore(localState *LocalState) *localStateExtenderStore {
	return &localStateExtenderStore{localState: localState}
}

func (self *localStateExtenderStore) Load() ([]byte, error) {
	return self.localState.getExtenders()
}

// The switch is read at each save rather than captured at construction, so a
// process that sets it after building a space still never writes -- and so a
// test can put it back without rebuilding anything.
func (self *localStateExtenderStore) Save(stateBytes []byte) error {
	if GetExtenderStoreReadOnly() {
		return nil
	}
	return self.localState.setExtenders(stateBytes)
}

// The persisted extender identity key (B1). Phase 6 reuses the same file as
// the provider's extender identity, so a provider that becomes an extender
// keeps the mesh peer id it already had.
const extenderKeyFileName = ".extender_key"

// The stored key document.
type extenderKeyState struct {
	SeedHex string `json:"seed_hex"`
}

// GetOrCreateExtenderKeySeed returns the persisted ed25519 seed, creating it on
// first use (B1). A file that is missing, unreadable or not a seed is replaced
// rather than failing: the identity is a convenience, and an install that
// cannot read its own key is better off with a new one than with none.
func (self *LocalState) GetOrCreateExtenderKeySeed() ([]byte, error) {
	path := filepath.Join(self.localStorageDir, extenderKeyFileName)
	if stateBytes, err := os.ReadFile(path); err == nil {
		keyState := &extenderKeyState{}
		if err := json.Unmarshal(stateBytes, keyState); err == nil {
			if seed, err := connect.ParseExtenderKeySeedHex(keyState.SeedHex); err == nil {
				return seed, nil
			}
		}
	}
	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		return nil, err
	}
	stateBytes, err := json.Marshal(&extenderKeyState{
		SeedHex: connect.ExtenderKeySeedHex(seed),
	})
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, stateBytes, LocalStorageFilePermissions); err != nil {
		return nil, err
	}
	return seed, nil
}
