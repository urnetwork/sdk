// The sign-in fence of a network space's local state.
//
// A sign-out removes the account's state from the space: Logout, a rejected
// client's logout (a server-initiated sign-out) and conditional cleanup. The
// space itself outlives the sign-out, and a write already on its way can land
// after the removal. A connect blocked in a device rpc, which can hold for the
// rpc's full timeout, saves the connect location, the default location and,
// under Auto, the public provide mode when it returns; a device listener's
// mirror of one of them, or an app write after a device call, can run late too.
// The next sign-in reads them back as its own, and a saved connect location
// reconnects the next account, counted as already connected, past its
// balance gate.
//
// So every sign-out advances signInGeneration and marks the space signed out,
// and the next stored credential signs it in again. Removing the last stored
// credential without a logout (a failed sign-in's rollback, a rejected network
// credential) is a sign-out too: otherwise a sign-in that stored its network
// token and was rolled back would leave the space open with no account, and a
// late write of the previous one would reach the next. A deferred write captures
// its sign-in (localStateSignIn) before its device calls, and commits only while
// that sign-in is still current, checked in the same critical section as the
// write: a sign-out either comes first, and the write is dropped, or comes
// after, and removes the write with the rest of the account. The exported
// setters of the same records drop their writes while the space is signed out,
// which covers the apps' own writes without an app change. Credential renewal
// is not a sign-out and never drops a write. The fence is in-process: a new
// LocalState is signed in to whatever its store holds.
package sdk

// One sign-in of a network space, captured by a deferred account write. The
// zero value has no local state and drops every write.
type localStateSignIn struct {
	localState *LocalState
	generation uint64
	signedIn   bool
}

// Captures the current sign-in. One captured while the space is signed out is
// never current, not even after the next sign-in.
func (self *LocalState) currentSignIn() localStateSignIn {
	if self == nil {
		return localStateSignIn{}
	}
	self.authStateLock.Lock()
	defer self.authStateLock.Unlock()
	return localStateSignIn{
		localState: self,
		generation: self.signInGeneration,
		signedIn:   !self.signedOut,
	}
}

// Every removal of the account's state calls this before it removes anything,
// so the fence closes even when the removal then fails partway, and so does a
// commit that leaves no credential stored.
func (self *LocalState) signOutWithLock() {
	self.signInGeneration += 1
	self.signedOut = true
}

// A committed or confirmed auth state that holds a credential is a sign-in:
// writes for it commit again.
func (self *LocalState) signInStoredWithLock(state persistedLocalAuthState) {
	if state.ByJwt != "" || state.ByClientJwt != "" {
		self.signedOut = false
	}
}

// Runs a write of an exported setter, which carries no captured sign-in: it
// belongs to the current sign-in, and is dropped while there is none.
func (self *LocalState) writeSignedIn(writeWithLock func() error) error {
	self.authStateLock.Lock()
	defer self.authStateLock.Unlock()
	if self.signedOut {
		return nil
	}
	return writeWithLock()
}

// Runs writeWithLock only while this sign-in is current, and reports whether it
// ran. A dropped write is not an error: its sign-in has ended.
func (self localStateSignIn) write(writeWithLock func(*LocalState) error) (bool, error) {
	localState := self.localState
	if localState == nil {
		return false, nil
	}
	localState.authStateLock.Lock()
	defer localState.authStateLock.Unlock()
	if !self.signedIn || localState.signedOut || localState.signInGeneration != self.generation {
		return false, nil
	}
	return true, writeWithLock(localState)
}

// Saves the connect location for automatic reconnect, for this sign-in only.
func (self localStateSignIn) setConnectLocation(location *ConnectLocation) (bool, error) {
	return self.write(func(localState *LocalState) error {
		return localState.setLocationWithLock(localConnectLocationFileName, location)
	})
}

// Saves the default location, for this sign-in only.
func (self localStateSignIn) setDefaultLocation(location *ConnectLocation) (bool, error) {
	return self.write(func(localState *LocalState) error {
		return localState.setLocationWithLock(localDefaultLocationFileName, location)
	})
}

// Saves the provide mode, for this sign-in only.
func (self localStateSignIn) setProvideMode(provideMode ProvideMode) (bool, error) {
	return self.write(func(localState *LocalState) error {
		return localState.setProvideModeWithLock(provideMode)
	})
}
