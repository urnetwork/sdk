package sdk

import (
	"errors"
	"github.com/urnetwork/connect"
)

// An unrelated client stays connected when only its account credential fails.
type AccountSignInRequiredListener interface{ AccountSignInRequired() }

// A notice that is still to be dismissed, also one kept from before a restart
// (adoptAccountSignInRequiredWithLock), reaches a listener when it is added,
// so every app shows it again with no startup step of its own.
func (self *Api) AddAccountSignInRequiredListener(listener AccountSignInRequiredListener) Sub {
	id := self.accountSignInRequiredListeners.Add(listener)
	self.mutex.Lock()
	pending := self.accountSignInNoticePending
	self.mutex.Unlock()
	if pending {
		go connect.HandleError(listener.AccountSignInRequired)
	}
	return newSub(func() { self.accountSignInRequiredListeners.Remove(id) })
}

// Whether the account sign-in notice (AccountSignInRequired) is to be shown:
// the account's sign-in was rejected while this device stayed signed in, also
// before a restart, and the notice was not dismissed since. For an app that
// shows the notice after its listener was added (AddAccountSignInRequiredListener
// also reports it then); GetAccountSignInRequiredCause gives its cause.
func (self *Api) HasAccountSignInNotice() bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.accountSignInNoticePending
}

// Ends the account sign-in notice (AccountSignInRequired) the user dismissed,
// so it does not show again, after a restart either. The next sign-in or
// sign-out ends it too. The account still needs its sign-in: the Sessions
// screen keeps its sign-in required state.
func (self *Api) DismissAccountSignInNotice() {
	self.authMutationLock.Lock()
	defer self.authMutationLock.Unlock()
	self.mutex.Lock()
	store := self.accountSignInNoticeStore
	self.accountSignInNoticePending = false
	self.accountSignInNoticeStore = nil
	self.mutex.Unlock()
	if store != nil {
		if err := store.dismissAccountSignInNotice(); err != nil {
			self.logger().Infof("[api]the account sign-in notice was not dismissed in storage: %s\n", err)
		}
	}
}

// Takes up the account's sign-in requirement a LocalState kept from before a
// restart (the state the device starts from, or a network space's at its
// start, before any device): the Sessions screen's state is sign-in required
// again, with the cause, and the notice is pending unless it was dismissed.
// Nothing while the API holds the account's sign-in. Reports whether a notice
// is pending. Callers hold mutex (after newLoginWithLock, for a device); the
// caller notifies the listeners once it holds no lock.
func (self *Api) adoptAccountSignInRequiredWithLock(localState *LocalState, state persistedLocalAuthState) bool {
	if localState == nil || !state.accountSignInRequired() || self.networkByJwt != "" || isNetworkCredential(self.byJwt) {
		return false
	}
	self.noteSignInRejectionWithLock(state.AccountSignInRequiredCause)
	self.accountSignInNoticeStore = localState
	self.accountSignInNoticePending = !state.AccountSignInNoticeDismissed
	return self.accountSignInNoticePending
}

// A network space's start: its API takes up the requirement its LocalState
// kept (adoptAccountSignInRequiredWithLock), so it reports it from the start,
// before a device starts, or with none (a desktop app connects later). No
// listener is added yet; one added later hears a pending notice.
func (self *Api) adoptStoredAccountSignInRequired(localState *LocalState) {
	if localState == nil {
		return
	}
	self.authMutationLock.Lock()
	defer self.authMutationLock.Unlock()
	state, err := localState.loadAuthState()
	if err != nil {
		return
	}
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.adoptAccountSignInRequiredWithLock(localState, state)
}

// Tells the listeners of a notice adopted at a device's start.
func (self *Api) notifyAdoptedAccountSignInNotice() {
	for _, listener := range self.accountSignInRequiredListeners.Get() {
		connect.HandleError(listener.AccountSignInRequired)
	}
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
// still the stored one), so its API leaves it here. required keeps the
// account's sign-in requirement, with its cause, across restarts when the
// device stays signed in; false is a sign-out, which ends any.
func (self *LocalState) rejectNetworkCredential(expected string, removeRelatedClient bool, anyClient bool, required bool, cause string) error {
	return self.updateAuthState(func(state *persistedLocalAuthState) (bool, error) {
		if state.ByJwt != expected {
			return false, nil
		}
		state.ByJwt = ""
		state.setAccountSignInRequired(required, cause)
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
	// a rejection that keeps this device signed in is kept with the store, so
	// the account's sign-in requirement and its notice outlive a restart; a
	// sign-out ends any
	requiredCause := ""
	if !logout {
		requiredCause = self.trustedRejectionCauseWithLock(target.byJwt, cause)
	}
	self.mutex.Unlock()
	var persistenceErr error
	if target.store != nil {
		persistenceErr = target.store.rejectNetworkCredential(target.byJwt, !deviceOwned, signOut, !logout, requiredCause)
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
	self.accountSignInNoticePending = !logout
	self.accountSignInNoticeStore = nil
	if !logout {
		self.accountSignInNoticeStore = target.store
	}
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
