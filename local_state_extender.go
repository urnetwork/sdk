package sdk

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/urnetwork/connect"
)

// local_state_extender.go — the persisted extender state (EXTENDER.md E1, D5,
// F3).
//
// Four dot files, all following the shape of the other local-state files: the
// directory envelope, written by connect through the store adapter below, the
// gossip mode the user chose, the identity key, and the provider extender
// opt-out.
//
// The directory is a cache. Every read failure -- missing, unreadable, corrupt
// -- reads as no directory at all, and the client rediscovers, so nothing here
// can keep a launch from succeeding.

// The serialized extender directory (E1).
const extenderStoreFileName = ".extenders"

// The persisted gossip role override (D5).
const extenderGossipModeFileName = ".extender_gossip_mode"

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

// The provider extender opt-out (F3, G1). Default on: a file that is not there
// is a provider that has never been asked, and the role is on by default.
const provideExtenderFileName = ".provide_extender"

// The persisted provider extender setting. Unset or unreadable reads as on,
// which is the default of F3; only an explicit off is stored as off.
func (self *LocalState) GetProvideExtender() bool {
	path := filepath.Join(self.localStorageDir, provideExtenderFileName)
	stateBytes, err := os.ReadFile(path)
	if err != nil {
		return true
	}
	return strings.TrimSpace(string(stateBytes)) != "false"
}

// Persists the provider extender setting.
func (self *LocalState) SetProvideExtender(provideExtender bool) error {
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
