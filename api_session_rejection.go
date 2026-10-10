package sdk

import (
	"errors"
	"github.com/urnetwork/connect"
)

// An unrelated client stays connected when only its account credential fails.
type AccountSignInRequiredListener interface{ AccountSignInRequired() }

func (self *Api) AddAccountSignInRequiredListener(listener AccountSignInRequiredListener) Sub {
	id := self.accountSignInRequiredListeners.Add(listener)
	return newSub(func() { self.accountSignInRequiredListeners.Remove(id) })
}

func sameTaggedSession(a, b string) bool {
	ac, aok := unverifiedCredentialClaims(a)
	bc, bok := unverifiedCredentialClaims(b)
	if !aok || !bok {
		return false
	}
	sid, _ := ac["session_id"].(string)
	other, _ := bc["session_id"].(string)
	network, _ := ac["network_id"].(string)
	otherNetwork, _ := bc["network_id"].(string)
	user, _ := ac["user_id"].(string)
	otherUser, _ := bc["user_id"].(string)
	return sid != "" && sid == other && network != "" && network == otherNetwork && user != "" && user == otherUser
}
func (self *Api) networkRequestTarget(sent string) (networkRenewalTarget, bool) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if sent == "" || self.networkByJwt != sent || !isNetworkCredential(sent) {
		return networkRenewalTarget{}, false
	}
	return networkRenewalTarget{byJwt: sent, generation: self.networkByJwtGeneration, store: self.networkByJwtStore}, true
}

func (self *LocalState) rejectNetworkCredential(expected string) error {
	return self.updateAuthState(func(state *persistedLocalAuthState) (bool, error) {
		if state.ByJwt != expected {
			return false, nil
		}
		state.ByJwt = ""
		if sameTaggedSession(expected, state.ByClientJwt) {
			state.ByClientJwt = ""
			state.InstanceId = ""
		}
		return true, nil
	})
}

// API-only callers may attach persistence without constructing a Device.
func (self *Api) SetNetworkCredentialStore(store *LocalState) {
	self.authMutationLock.Lock()
	self.mutex.Lock()
	self.networkByJwtStore = store
	self.networkCredentialChangedWithLock()
	self.mutex.Unlock()
	self.authMutationLock.Unlock()
	self.networkRenewer.credentialChanged()
}

var ErrCredentialPersistence = errors.New("credential rejection could not be persisted")

// Persistence failure is reported while the in-memory rejection still fences
// re-adoption. A subsequent login can explicitly install a different identity.
func (self *Api) GetCredentialPersistenceError() error {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.credentialPersistenceError
}
func (self *Api) rejectNetworkCredential(target networkRenewalTarget) bool {
	self.authMutationLock.Lock()
	self.mutex.Lock()
	current := self.networkByJwt == target.byJwt && self.networkByJwtGeneration == target.generation && self.networkByJwtStore == target.store
	if !current {
		self.mutex.Unlock()
		self.authMutationLock.Unlock()
		return false
	}
	logout := self.byJwt == target.byJwt || self.byJwt == "" || sameTaggedSession(self.byJwt, target.byJwt)
	self.mutex.Unlock()
	var persistenceErr error
	if target.store != nil {
		persistenceErr = target.store.rejectNetworkCredential(target.byJwt)
	}
	self.mutex.Lock()
	self.credentialPersistenceError = nil
	if persistenceErr != nil {
		self.credentialPersistenceError = ErrCredentialPersistence
	}
	self.networkByJwt = ""
	self.networkByJwtStore = nil
	self.networkByJwtRejected = target.byJwt
	self.networkCredentialChangedWithLock()
	if logout {
		self.rejectedByJwt = self.byJwt
		self.byJwt = ""
		self.deviceAuthGeneration++
		self.deviceAuthOwner = nil
	}
	self.mutex.Unlock()
	self.authMutationLock.Unlock()
	self.networkRenewer.credentialChanged()
	if logout {
		for _, listener := range self.authLogoutListeners.Get() {
			connect.HandleError(listener.AuthLogout)
		}
	} else {
		for _, listener := range self.accountSignInRequiredListeners.Get() {
			connect.HandleError(listener.AccountSignInRequired)
		}
	}
	return true
}
