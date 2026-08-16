package sdk

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/urnetwork/connect"
)

const (
	localAuthStateVersion   = 1
	localAuthStateFileName  = ".auth_state"
	localAuthStateMaxBytes  = 1024 * 1024
	legacyByJwtFileName     = ".by_jwt"
	legacyClientJwtFileName = ".by_client_jwt"
	legacyInstanceFileName  = ".instance_id"
)

// persistedLocalAuthState is the one authoritative transaction for the three
// values that define a restartable authenticated device. Generation is local
// monotonic metadata: readers do not infer freshness from file mtimes, and a
// future cross-process store can compare records without changing this shape.
type persistedLocalAuthState struct {
	Version     int    `json:"version"`
	Generation  uint64 `json:"generation"`
	ByJwt       string `json:"by_jwt"`
	ByClientJwt string `json:"by_client_jwt"`
	InstanceId  string `json:"instance_id"`
}

func emptyPersistedLocalAuthState() persistedLocalAuthState {
	return persistedLocalAuthState{Version: localAuthStateVersion}
}

func (state persistedLocalAuthState) validate() error {
	if state.Version != localAuthStateVersion {
		return fmt.Errorf("unsupported auth state version %d", state.Version)
	}
	if state.InstanceId != "" {
		if _, err := ParseId(state.InstanceId); err != nil {
			return fmt.Errorf("invalid auth state instance_id: %w", err)
		}
	}
	return nil
}

func (self *LocalState) authStatePath() string {
	return filepath.Join(self.localStorageDir, localAuthStateFileName)
}

func (self *LocalState) loadAuthState() (persistedLocalAuthState, error) {
	self.authStateLock.Lock()
	defer self.authStateLock.Unlock()
	return self.loadAuthStateLocked()
}

// loadAuthStateLocked prefers the atomic envelope. Legacy files are consulted
// only when the envelope is genuinely absent; a corrupt/unknown envelope must
// never fall back to older credentials that may describe a different device.
// Caller holds authStateLock.
func (self *LocalState) loadAuthStateLocked() (persistedLocalAuthState, error) {
	path := self.authStatePath()
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > localAuthStateMaxBytes {
			return persistedLocalAuthState{}, errors.New("auth state is not a bounded regular file")
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return persistedLocalAuthState{}, readErr
		}
		var state persistedLocalAuthState
		if decodeErr := json.Unmarshal(data, &state); decodeErr != nil {
			return persistedLocalAuthState{}, fmt.Errorf("decode auth state: %w", decodeErr)
		}
		if validateErr := state.validate(); validateErr != nil {
			return persistedLocalAuthState{}, validateErr
		}
		// Older builds used 0700 for every local-state file. Tighten an existing
		// envelope opportunistically; failure is reported because this file holds
		// bearer credentials.
		if chmodErr := os.Chmod(path, LocalStorageFilePermissions); chmodErr != nil {
			return persistedLocalAuthState{}, chmodErr
		}
		self.cleanupAuthStateTempsLocked()
		return state, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return persistedLocalAuthState{}, err
	}

	legacy, found, legacyErr := self.loadLegacyAuthStateLocked()
	if legacyErr != nil {
		return persistedLocalAuthState{}, legacyErr
	}
	if !found {
		return emptyPersistedLocalAuthState(), nil
	}

	// A temporarily read-only filesystem must not turn a valid legacy login
	// into a logout. Return the coherent legacy snapshot and retry migration on
	// the next mutation/read; once the envelope commits, legacy files are erased.
	if writeErr := self.writeAuthStateLocked(legacy); writeErr != nil {
		return legacy, nil
	}
	return legacy, nil
}

// loadLegacyAuthStateLocked snapshots the three files written by pre-envelope
// SDKs. It intentionally mirrors their tolerant read behavior: an invalid
// instance is treated as missing while usable JWTs remain available so the app
// can repair or log out the partial installation.
func (self *LocalState) loadLegacyAuthStateLocked() (persistedLocalAuthState, bool, error) {
	state := emptyPersistedLocalAuthState()
	state.Generation = 1
	found := false

	readLegacy := func(name string) ([]byte, error) {
		path := filepath.Join(self.localStorageDir, name)
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		found = true
		return data, nil
	}

	byJwt, err := readLegacy(legacyByJwtFileName)
	if err != nil {
		return persistedLocalAuthState{}, false, err
	}
	state.ByJwt = string(byJwt)
	clientJwt, err := readLegacy(legacyClientJwtFileName)
	if err != nil {
		return persistedLocalAuthState{}, false, err
	}
	state.ByClientJwt = string(clientJwt)
	instanceBytes, err := readLegacy(legacyInstanceFileName)
	if err != nil {
		return persistedLocalAuthState{}, false, err
	}
	if len(instanceBytes) != 0 {
		if instanceId, parseErr := connect.IdFromBytes(instanceBytes); parseErr == nil {
			state.InstanceId = newId(instanceId).String()
		}
	}
	return state, found, nil
}

func (self *LocalState) updateAuthState(
	mutate func(state *persistedLocalAuthState) (bool, error),
) error {
	self.authStateLock.Lock()
	defer self.authStateLock.Unlock()

	state, err := self.loadAuthStateLocked()
	if err != nil {
		return err
	}
	changed, err := mutate(&state)
	if err != nil || !changed {
		return err
	}
	state.Version = localAuthStateVersion
	state.Generation += 1
	return self.writeAuthStateLocked(state)
}

// writeAuthStateLocked performs the complete durable transaction: create in
// the destination directory, restrict permissions before writing secrets,
// fsync the bytes, atomically replace the authoritative name, then fsync the
// directory where supported. Caller holds authStateLock.
func (self *LocalState) writeAuthStateLocked(state persistedLocalAuthState) (returnErr error) {
	if err := state.validate(); err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if len(data) == 0 || localAuthStateMaxBytes < len(data) {
		return errors.New("auth state exceeds size limit")
	}

	temp, err := os.CreateTemp(self.localStorageDir, localAuthStateFileName+".tmp-")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() {
		_ = temp.Close()
		if returnErr != nil {
			_ = os.Remove(tempPath)
		}
	}()
	if err = temp.Chmod(LocalStorageFilePermissions); err != nil {
		return err
	}
	if _, err = temp.Write(data); err != nil {
		return err
	}
	if err = temp.Sync(); err != nil {
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tempPath, self.authStatePath()); err != nil {
		return err
	}

	// Windows' directory handles do not support File.Sync. MoveFileEx with
	// REPLACE_EXISTING is still the atomic replacement primitive there. On Unix
	// and Apple platforms, make a best-effort directory sync. The rename has
	// already committed at this point, so reporting a later directory-sync error
	// would tell the caller the transaction failed even though every subsequent
	// reader observes the new generation.
	if runtime.GOOS != "windows" {
		dir, openErr := os.Open(self.localStorageDir)
		if openErr == nil {
			_ = dir.Sync()
			_ = dir.Close()
		}
	}

	self.removeLegacyAuthStateLocked()
	self.cleanupAuthStateTempsLocked()
	return nil
}

func (self *LocalState) removeLegacyAuthStateLocked() {
	for _, name := range []string{
		legacyByJwtFileName,
		legacyClientJwtFileName,
		legacyInstanceFileName,
	} {
		_ = os.Remove(filepath.Join(self.localStorageDir, name))
	}
}

func (self *LocalState) cleanupAuthStateTempsLocked() {
	matches, _ := filepath.Glob(filepath.Join(self.localStorageDir, localAuthStateFileName+".tmp-*"))
	staleBefore := time.Now().Add(-time.Hour)
	for _, match := range matches {
		info, err := os.Lstat(match)
		if err == nil && info.Mode().IsRegular() && info.ModTime().Before(staleBefore) {
			_ = os.Remove(match)
		}
	}
}
