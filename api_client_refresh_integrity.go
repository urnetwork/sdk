package sdk

import "github.com/urnetwork/connect"

// ClientRefreshIntegrityListener observes a completed invalid refresh for the
// captured credential. It grants no identity or logout authority. Consumers may
// stop dependent work; originalJwt is private credential data, never a log value.
//
//gomobile:noexport
type ClientRefreshIntegrityListener interface {
	ClientRefreshInvalid(originalJwt string)
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
		connect.HandleError(func() { listener.ClientRefreshInvalid(originalJwt) })
	}
}
