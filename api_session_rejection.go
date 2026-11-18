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

// Removes the rejected network credential, and with removeRelatedClient the
// client credential of the same session, or any client credential with
// anyClient (a sign-out of this app's own session). A device that owns the
// API removes its own client credential, with the rest of its sign-in, when
// its logout listener runs (logoutRejectedClient checks that credential is
// still the stored one), so its API leaves it here.
func (self *LocalState) rejectNetworkCredential(expected string, removeRelatedClient bool, anyClient bool) error {
	return self.updateAuthState(func(state *persistedLocalAuthState) (bool, error) {
		if state.ByJwt != expected {
			return false, nil
		}
		state.ByJwt = ""
		if removeRelatedClient && (anyClient || sameTaggedSession(expected, state.ByClientJwt)) {
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

// The cause (confirmedRejectionCause, or "" for a sign-out this API made) is
// recorded only for the current target, before the listeners run.
//
// A logout keeps the device owner, as rejectByJwt does: the owning device's
// logout listener reads the rejected credential through it and then removes
// the device's sign-in, its client credential included, before it notifies
// the app.
func (self *Api) rejectNetworkCredential(target networkRenewalTarget, cause string) bool {
	return self.endNetworkCredential(target, cause, false)
}

// The sign-out of this app's own session from the Sessions screen (the
// session controller's revoke of its current session, once enforced): the
// whole sign-in ends, the client credential with the network one, whatever
// session the client credential carries, and the app gets its AuthLogout.
// A client credential of another session (one not yet renewed into the
// session after sessions turned on, or an older lineage) is not revoked with
// this one, so a rejection left that app signed in on it, with only
// AccountSignInRequired, after a confirmation that said "This app will be
// signed out".
func (self *Api) signOutNetworkCredential(target networkRenewalTarget) bool {
	return self.endNetworkCredential(target, "", true)
}

// rejectNetworkCredential, or with signOut signOutNetworkCredential.
func (self *Api) endNetworkCredential(target networkRenewalTarget, cause string, signOut bool) bool {
	self.authMutationLock.Lock()
	self.mutex.Lock()
	current := self.networkByJwt == target.byJwt && self.networkByJwtGeneration == target.generation && self.networkByJwtStore == target.store
	if !current {
		self.mutex.Unlock()
		self.authMutationLock.Unlock()
		return false
	}
	// this app's own sign-out ends the whole sign-in, also when the rejection
	// of another request comes before the revoke's answer
	signOut = signOut || self.selfRevokedWithLock(target.byJwt)
	logout := signOut || self.byJwt == target.byJwt || self.byJwt == "" || sameTaggedSession(self.byJwt, target.byJwt)
	deviceOwned := self.deviceAuthOwner != nil
	self.mutex.Unlock()
	var persistenceErr error
	if target.store != nil {
		persistenceErr = target.store.rejectNetworkCredential(target.byJwt, !deviceOwned, signOut)
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
	cause = self.trustedRejectionCauseWithLock(target.byJwt, cause)
	self.noteSignInRejectionWithLock(cause)
	var logoutGeneration uint64
	if logout {
		self.rejectedByJwt = self.byJwt
		self.authLogoutCause = cause
		self.byJwt = ""
		self.deviceAuthGeneration++
		self.authLogoutGeneration = self.deviceAuthGeneration
		logoutGeneration = self.authLogoutGeneration
	}
	self.mutex.Unlock()
	self.authMutationLock.Unlock()
	self.networkRenewer.credentialChanged()
	if logout {
		for _, listener := range self.authLogoutListeners.Get() {
			connect.HandleError(listener.AuthLogout)
		}
		self.authLogoutEvent(logoutGeneration, cause)
	} else {
		for _, listener := range self.accountSignInRequiredListeners.Get() {
			connect.HandleError(listener.AccountSignInRequired)
		}
	}
	return true
}
