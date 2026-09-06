// A paired auth observation and conditional cleanup use the existing API and
// LocalState owners. Native request/selection admission remains with callers.
package sdk

import (
	"errors"
	"os"
	"path/filepath"
)

// Comparable API facts are copied during a short mutex section, never while
// that mutex covers storage I/O, notifications, callbacks, or joins.
type localAuthApiSnapshot struct {
	byJwt         string
	rejectedByJwt string
	generation    uint64
	owner         *deviceAuthPublicationGate
}

// The result distinguishes completed cleanup from superseded/in-flight work.
// An error instead returns nil; after destructive work began, it may describe
// partial effects and must never be presented as a successful reset.
type LocalStateResetResult struct {
	reset       bool
	keyMaterial *DeviceLocalKeyMaterial
}

// Reports whether this operation completed the requested reset.
func (self *LocalStateResetResult) GetReset() bool {
	return self.reset
}

// Returns the material actually preserved by this completed reset, including
// a genuine nil identity. A later separate storage read is not this result.
func (self *LocalStateResetResult) GetDeviceLocalKeyMaterial() *DeviceLocalKeyMaterial {
	if self.keyMaterial == nil {
		return nil
	}
	return NewDeviceLocalKeyMaterial(
		self.keyMaterial.GetClientKeySeed(),
		self.keyMaterial.GetProvideTlsCertificatePem(),
		self.keyMaterial.GetProvideTlsPrivateKeyPem(),
	)
}

// Reads one envelope and its API/LocalState ownership under the existing lock
// order. A paired snapshot, unlike a disk-only snapshot, can qualify a reset.
func (self *NetworkSpace) GetAuthStateSnapshot() (*LocalAuthStateSnapshot, error) {
	if self.asyncLocalState == nil || self.api == nil {
		return nil, errors.New("network space has no stored auth pair")
	}
	api := self.api
	localState := self.asyncLocalState.localState
	api.authMutationLock.Lock()
	defer api.authMutationLock.Unlock()
	localState.authStateLock.Lock()
	defer localState.authStateLock.Unlock()
	if self.ctx.Err() != nil || api.ctx.Err() != nil {
		return nil, errors.New("network space auth owner is closed")
	}
	snapshot, err := localState.authStateSnapshotWithLock()
	if err != nil {
		return nil, err
	}
	snapshot.networkSpace = self
	snapshot.api = api
	snapshot.apiState = func() localAuthApiSnapshot {
		api.mutex.Lock()
		defer api.mutex.Unlock()
		return api.authStateSnapshotWithLock()
	}()
	apiState := snapshot.apiState
	// A pending device refresh/rejection or unmatched owner is not a stable
	// stale record. An unowned API may hold the stored admin, client, or none.
	snapshot.resetEligible = apiState.owner == snapshot.localOwner && apiState.rejectedByJwt == ""
	if apiState.owner != nil {
		snapshot.resetEligible = snapshot.resetEligible && apiState.byJwt == snapshot.state.ByClientJwt
	} else if apiState.byJwt != "" {
		snapshot.resetEligible = snapshot.resetEligible &&
			(apiState.byJwt == snapshot.state.ByJwt || apiState.byJwt == snapshot.state.ByClientJwt)
	}
	return snapshot, nil
}

// Returns the short in-memory API facts while mutex is held.
func (self *Api) authStateSnapshotWithLock() localAuthApiSnapshot {
	return localAuthApiSnapshot{
		byJwt:         self.byJwt,
		rejectedByJwt: self.rejectedByJwt,
		generation:    self.deviceAuthGeneration,
		owner:         self.deviceAuthOwner,
	}
}

// Resets only the originally observed pair and preserves keys inside that
// same storage scope. Callers must independently establish that this current
// state is stale; a coherent admin-only login can still own pending work.
func (self *NetworkSpace) ResetLocalStateIfCurrent(snapshot *LocalAuthStateSnapshot) (*LocalStateResetResult, error) {
	if !self.ownsAuthSnapshot(snapshot) {
		return nil, errors.New("auth snapshot belongs to a different owner pair")
	}
	api := self.api
	localState := self.asyncLocalState.localState
	var tokenManager *apiTokenManager
	result, err := func() (*LocalStateResetResult, error) {
		api.authMutationLock.Lock()
		defer api.authMutationLock.Unlock()
		localState.authStateLock.Lock()
		defer localState.authStateLock.Unlock()
		current, err := self.authSnapshotCurrentWithLock(snapshot)
		if err != nil {
			return nil, err
		}
		if !current {
			return &LocalStateResetResult{}, nil
		}
		keyPath := filepath.Join(localState.localStorageDir, ".device_local_key_material")
		if info, err := os.Lstat(keyPath); err == nil {
			// A readable symlink remains compatible for ordinary key loading,
			// but cleanup cannot preserve a target it does not own in place.
			if !info.Mode().IsRegular() {
				return nil, errors.New("preserved device key material is not a regular file")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("inspect preserved device key material")
		}
		material, err := localState.loadDeviceLocalKeyMaterialWithLock()
		if err != nil {
			return nil, err
		}
		entries, err := os.ReadDir(localState.localStorageDir)
		if err != nil {
			return nil, errors.New("read local state for conditional cleanup")
		}
		// Destructive work now owns this pair, including a later filesystem
		// failure. Retire publishers before deleting any durable state.
		func() {
			api.mutex.Lock()
			defer api.mutex.Unlock()
			if api.byJwt != "" {
				tokenManager = api.tokenManager
			}
			api.byJwt = ""
			api.rejectedByJwt = ""
			owner := api.deviceAuthOwner
			if owner != nil && api.httpPostRawOwner == owner {
				api.httpPostRaw = nil
				api.httpPostRawOwner = nil
			}
			if owner != nil && api.httpGetRawOwner == owner {
				api.httpGetRaw = nil
				api.httpGetRawOwner = nil
			}
			if owner != nil && api.httpPostStreamRawOwner == owner {
				api.httpPostStreamRaw = nil
				api.httpPostStreamRawOwner = nil
			}
			api.deviceAuthOwner = nil
			api.deviceAuthGeneration += 1
		}()
		resetErr := localState.resetPreservingKeysWithLock(entries)
		if localState.testingAfterPairedReset != nil {
			localState.testingAfterPairedReset()
		}
		if resetErr != nil {
			return nil, resetErr
		}
		return &LocalStateResetResult{reset: true, keyMaterial: material}, nil
	}()
	if tokenManager != nil {
		tokenManager.TokenChanged()
	}
	return result, err
}

// Only this exact paired source can authorize an observation-dependent action.
func (self *NetworkSpace) ownsAuthSnapshot(snapshot *LocalAuthStateSnapshot) bool {
	return self != nil && self.asyncLocalState != nil && self.api != nil && snapshot != nil &&
		snapshot.networkSpace == self && snapshot.api == self.api &&
		snapshot.localState == self.asyncLocalState.localState
}

// Called with API authMutationLock then LocalState authStateLock. The complete
// envelope detects durable changes; epochs also reject equality/ABA ownership.
func (self *NetworkSpace) authSnapshotCurrentWithLock(snapshot *LocalAuthStateSnapshot) (bool, error) {
	localState := self.asyncLocalState.localState
	api := self.api
	if self.ctx.Err() != nil || api.ctx.Err() != nil || localState.ctx.Err() != nil {
		return false, errors.New("network space auth owner is closed")
	}
	if !snapshot.resetEligible || localState.deviceAuthGeneration != snapshot.localGeneration ||
		localState.deviceAuthOwner != snapshot.localOwner {
		return false, nil
	}
	apiState := func() localAuthApiSnapshot {
		api.mutex.Lock()
		defer api.mutex.Unlock()
		return api.authStateSnapshotWithLock()
	}()
	if apiState != snapshot.apiState {
		return false, nil
	}
	state, err := localState.loadAuthStateLocked()
	if err != nil {
		return false, localStorageStageError("verify auth snapshot", err)
	}
	return state == snapshot.state, nil
}

// Removes only enumerated children, never the storage root or the exact
// checked identity file. Clear routing before auth, so interruption does not
// erase the account boundary while an old private-peer destination remains.
// Errors can leave partial effects; publishers stay retired in every case.
func (self *LocalState) resetPreservingKeysWithLock(entries []os.DirEntry) error {
	self.deviceAuthOwner = nil
	self.deviceAuthGeneration += 1
	var resetErr error
	remove := func(name string) {
		resetErr = errors.Join(resetErr, localStorageStageError("remove local state during conditional cleanup", os.RemoveAll(filepath.Join(self.localStorageDir, name))))
		if self.testingAfterPairedResetRemove != nil {
			self.testingAfterPairedResetRemove(name)
		}
	}
	for _, entry := range entries {
		if name := entry.Name(); name != ".device_local_key_material" && name != localAuthStateFileName {
			remove(name)
		}
	}
	if resetErr != nil {
		return resetErr
	}
	for _, entry := range entries {
		if entry.Name() == localAuthStateFileName {
			remove(entry.Name())
		}
	}
	return resetErr
}
