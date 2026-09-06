// In-process device auth mutations share LocalState's existing auth lock.
// Native owners must still join before replacing the LocalState object itself.
package sdk

import (
	"errors"
	"os"
)

// Invalidates delayed device writes without waiting for unrelated callbacks.
// A constructor claiming the same store is ordered by the same lock.
func (self *LocalState) closeDeviceAuthOwner(owner *deviceAuthPublicationGate) {
	if owner == nil {
		return
	}
	self.authStateLock.Lock()
	defer self.authStateLock.Unlock()
	if self.deviceAuthOwner == owner {
		self.deviceAuthOwner = nil
		self.deviceAuthGeneration += 1
	}
}

// An explicit device setter may update its own credential, never reclaim a
// store another constructor or login has already claimed.
func (self *LocalState) setOwnedClientJwt(byJwt string, instanceId *Id, owner *deviceAuthPublicationGate) (bool, error) {
	accepted := false
	err := self.updateAuthState(func(state *persistedLocalAuthState) (bool, error) {
		if owner == nil || self.deviceAuthOwner != owner || instanceId == nil {
			return false, nil
		}
		accepted = true
		instance := instanceId.String()
		if state.ByClientJwt == byJwt && state.InstanceId == instance {
			return false, nil
		}
		state.ByClientJwt = byJwt
		state.InstanceId = instance
		return true, nil
	})
	return accepted, err
}

// Destructive rejected-client cleanup is qualified by both in-process owner
// and durable credential/instance while holding the auth lock. A newer seed,
// explicit login, or client refresh therefore cannot be erased by this event.
func (self *LocalState) logoutRejectedClient(byJwt string, instanceId *Id, owner *deviceAuthPublicationGate) (bool, error) {
	self.authStateLock.Lock()
	defer self.authStateLock.Unlock()
	if owner == nil || self.deviceAuthOwner != owner || instanceId == nil || byJwt == "" {
		return false, nil
	}
	state, err := self.loadAuthStateLocked()
	if err != nil {
		return false, err
	}
	if state.ByClientJwt != byJwt || state.InstanceId != instanceId.String() {
		return false, nil
	}
	self.deviceAuthOwner = nil
	self.deviceAuthGeneration += 1
	return true, errors.Join(
		os.RemoveAll(self.localStorageDir),
		os.MkdirAll(self.localStorageDir, LocalStorageDirectoryPermissions),
	)
}
