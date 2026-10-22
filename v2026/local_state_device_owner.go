// In-process device auth mutations share LocalState's existing auth lock.
// Native owners must still join before replacing the LocalState object itself.
package sdk

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
// store another constructor or login has already claimed. The caller holds
// authStateLock through the matching API publication.
func (self *LocalState) setOwnedClientJwtWithLock(byJwt string, instanceId *Id, owner *deviceAuthPublicationGate) (bool, error) {
	accepted := false
	err := self.updateAuthStateWithLock(func(state *persistedLocalAuthState) (bool, error) {
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
// The optional published credential belongs to a device whose API rejection
// remains locked and current: failed refresh persistence can leave it behind.
func (self *LocalState) logoutRejectedClient(byJwt string, instanceId *Id, owner *deviceAuthPublicationGate, publishedByJwt ...string) (bool, error) {
	self.authStateLock.Lock()
	defer self.authStateLock.Unlock()
	if owner == nil || self.deviceAuthOwner != owner || instanceId == nil || byJwt == "" {
		return false, nil
	}
	state, err := self.loadAuthStateLocked()
	if err != nil {
		return false, err
	}
	credentialMatches := state.ByClientJwt == byJwt
	if len(publishedByJwt) == 1 && publishedByJwt[0] != "" {
		credentialMatches = credentialMatches || state.ByClientJwt == publishedByJwt[0]
	}
	if !credentialMatches || state.InstanceId != instanceId.String() {
		return false, nil
	}
	self.deviceAuthOwner = nil
	self.deviceAuthGeneration += 1
	return true, self.removeAccountStateWithLock()
}
