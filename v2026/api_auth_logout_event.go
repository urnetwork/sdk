// Rejection events retain their owner and cause across delayed delivery. A
// callback-time read of the current credential or cause cannot identify an
// earlier rejection after a new login has been accepted on the same API.
package sdk

import "github.com/urnetwork/connect/v2026"

// The opaque generation names the rejection committed before dispatch. It
// contains no credential and is only compared for equality. The signed basic
// type crosses the native, Java and Objective-C bindings unchanged.
type AuthLogoutEventListener interface {
	AuthLogoutEvent(generation int64, cause string)
}

// Observes the same rejections as the legacy listener with immutable origin
// and cause. No listener runs while an API auth lock is held. Applications
// check IsAuthLogoutCurrent at their serialized destructive cleanup boundary.
func (self *Api) AddAuthLogoutEventListener(listener AuthLogoutEventListener) Sub {
	id := self.authLogoutEventListeners.Add(listener)
	return newSub(func() { self.authLogoutEventListeners.Remove(id) })
}

// A new credential, including an equal-byte login, or an explicit SetByJwt
// retires an earlier event. Ordinary device teardown retains it: disconnecting
// after a rejection must not prevent the application from following that
// rejection. This read does not serialize application state; the caller owns
// serialization of its credential installs and destructive cleanup.
func (self *Api) IsAuthLogoutCurrent(generation int64) bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return generation != 0 && self.authLogoutGeneration == uint64(generation)
}

// Both arguments were captured together at the rejection commit. In
// particular, never replace them with a read after obtaining this list.
func (self *Api) authLogoutEvent(generation uint64, cause string) {
	for _, listener := range self.authLogoutEventListeners.Get() {
		connect.HandleError(func() { listener.AuthLogoutEvent(int64(generation), cause) })
	}
}
