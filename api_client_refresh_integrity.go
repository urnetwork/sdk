package sdk

import "github.com/urnetwork/connect"

// ClientRefreshIntegrityListener observes a completed invalid refresh for the
// captured credential. It grants no identity or logout authority. A consumer
// may stop dependent work only after the notice atomically closes that owner.
//
//gomobile:noexport
type ClientRefreshIntegrityListener interface {
	ClientRefreshInvalid(notice *ClientRefreshIntegrityNotice)
}

// The immutable notice may become stale during delivery. No credential is
// exposed. Its optional action checks the original generation at the actual
// cancellation boundary; callbacks themselves never run under auth locks.
//
//gomobile:noexport
type ClientRefreshIntegrityNotice struct {
	api         *Api
	originalJwt string
	generation  uint64
}

// CloseApiIfCurrent requests cancellation without joining any worker. A newer
// equal-byte login invalidates this notice. Once canceled, this API's lifetime
// cannot be revived by installing another token.
//
//gomobile:noexport
func (self *ClientRefreshIntegrityNotice) CloseApiIfCurrent() bool {
	if self == nil || self.api == nil {
		return false
	}
	self.api.authMutationLock.Lock()
	defer self.api.authMutationLock.Unlock()
	self.api.mutex.Lock()
	defer self.api.mutex.Unlock()
	if self.api.ctx.Err() != nil || self.api.byJwt != self.originalJwt || self.api.deviceAuthGeneration != self.generation {
		return false
	}
	self.api.cancel()
	return true
}

// AddClientRefreshIntegrityListener does not replace the existing validation
// and publication gate. Callbacks run after auth locks have been released.
//
//gomobile:noexport
func (self *Api) AddClientRefreshIntegrityListener(listener ClientRefreshIntegrityListener) Sub {
	id := self.clientRefreshIntegrityListeners.Add(listener)
	return newSub(func() { self.clientRefreshIntegrityListeners.Remove(id) })
}

func (self *Api) reportClientRefreshIntegrity(originalJwt string, generation uint64) {
	current, currentGeneration := self.authCredentialSnapshot()
	if current != originalJwt || currentGeneration != generation {
		return
	}
	for _, listener := range self.clientRefreshIntegrityListeners.Get() {
		connect.HandleError(func() {
			listener.ClientRefreshInvalid(&ClientRefreshIntegrityNotice{api: self, originalJwt: originalJwt, generation: generation})
		})
	}
}
