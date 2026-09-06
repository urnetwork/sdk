// Device-owned API credentials and HTTP bindings carry the originating device
// identity, so retiring an old device cannot clear a replacement's resources.
package sdk

import "github.com/urnetwork/connect"

// Captures the exact request credential and auth generation before HTTP work.
// Equal-byte explicit login still supersedes a response from an older request.
func (self *Api) authCredentialSnapshot() (string, uint64) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.byJwt, self.deviceAuthGeneration
}

// Reads device ownership independently of an in-progress credential rotation.
func (self *Api) deviceOwnsAuth(owner *deviceAuthPublicationGate) bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return owner != nil && self.deviceAuthOwner == owner
}

// Checks the exact API credential and device owner in one read.
func (self *Api) deviceOwnsByJwt(owner *deviceAuthPublicationGate, byJwt string) bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return owner != nil && self.deviceAuthOwner == owner && self.byJwt == byJwt
}

// An explicit device update cannot reclaim the API after a replacement or
// admin login. The constructor-only install method establishes that ownership.
func (self *Api) replaceDeviceByJwt(owner *deviceAuthPublicationGate, byJwt string) bool {
	self.authMutationLock.Lock()
	self.mutex.Lock()
	if owner == nil || self.deviceAuthOwner != owner {
		self.mutex.Unlock()
		self.authMutationLock.Unlock()
		return false
	}
	changed := self.byJwt != byJwt
	self.byJwt = byJwt
	self.rejectedByJwt = ""
	self.deviceAuthGeneration += 1
	tokenManager := self.tokenManager
	self.mutex.Unlock()
	self.authMutationLock.Unlock()
	if changed && tokenManager != nil {
		tokenManager.TokenChanged()
	}
	return true
}

// Captures the rejected credential only while its originating device still
// owns the API. Public listener signatures and wire contracts stay unchanged.
func (self *Api) deviceRejectedJwt(owner *deviceAuthPublicationGate) (string, bool) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if owner == nil || self.deviceAuthOwner != owner || self.byJwt != "" || self.rejectedByJwt == "" {
		return "", false
	}
	return self.rejectedByJwt, true
}

// Installs a device credential without confusing it with an explicit admin
// login through SetByJwt. Refresh preserves this owner; a new login revokes it.
func (self *Api) setDeviceByJwt(prepared *deviceAuthStartup, owner *deviceAuthPublicationGate, log connect.Logger) error {
	byJwt := prepared.byJwt
	changed := false
	err := self.publishDeviceOwner(prepared, owner, func() {
		changed = self.byJwt != byJwt
		self.byJwt = byJwt
		self.deviceAuthOwner = owner
		self.rejectedByJwt = ""
		self.log = log
	})
	if err == nil && changed && self.tokenManager != nil {
		self.tokenManager.TokenChanged()
	}
	return err
}

// Publishes a remote's credential and request bindings in the same critical
// section. An ordinary native remote preserves a host-provided streaming seam,
// but never inherits a stream bound to a retired remote.
func (self *Api) installDeviceRemote(
	prepared *deviceAuthStartup,
	owner *deviceAuthPublicationGate,
	httpPostRaw connect.HttpPostRawFunction,
	httpGetRaw connect.HttpGetRawFunction,
	httpPostStreamRaw connect.HttpPostStreamRawFunction,
	log connect.Logger,
) error {
	byJwt := prepared.byJwt
	changed := false
	err := self.publishDeviceOwner(prepared, owner, func() {
		changed = self.byJwt != byJwt
		self.byJwt = byJwt
		self.deviceAuthOwner = owner
		self.rejectedByJwt = ""
		self.log = log
		self.httpPostRaw = httpPostRaw
		self.httpPostRawOwner = owner
		self.httpGetRaw = httpGetRaw
		self.httpGetRawOwner = owner
		if httpPostStreamRaw != nil {
			self.httpPostStreamRaw = httpPostStreamRaw
			self.httpPostStreamRawOwner = owner
		} else if self.httpPostStreamRawOwner != nil {
			self.httpPostStreamRaw = nil
			self.httpPostStreamRawOwner = nil
		}
	})
	if err == nil && changed && self.tokenManager != nil {
		self.tokenManager.TokenChanged()
	}
	return err
}

// Revokes only resources still owned by this device. Equal JWT bytes do not
// make a replacement the same owner, and an explicit login keeps its new token.
func (self *Api) closeDeviceOwner(owner *deviceAuthPublicationGate) {
	if owner == nil {
		return
	}
	self.authMutationLock.Lock()
	self.mutex.Lock()
	changed := false
	if self.deviceAuthOwner == owner || self.httpPostRawOwner == owner ||
		self.httpGetRawOwner == owner || self.httpPostStreamRawOwner == owner {
		self.deviceAuthGeneration += 1
	}
	if self.deviceAuthOwner == owner {
		changed = self.byJwt != ""
		self.byJwt = ""
		self.deviceAuthOwner = nil
		self.rejectedByJwt = ""
	}
	if self.httpPostRawOwner == owner {
		self.httpPostRaw = nil
		self.httpPostRawOwner = nil
	}
	if self.httpGetRawOwner == owner {
		self.httpGetRaw = nil
		self.httpGetRawOwner = nil
	}
	if self.httpPostStreamRawOwner == owner {
		self.httpPostStreamRaw = nil
		self.httpPostStreamRawOwner = nil
	}
	tokenManager := self.tokenManager
	self.mutex.Unlock()
	self.authMutationLock.Unlock()
	if changed && tokenManager != nil {
		tokenManager.TokenChanged()
	}
}
