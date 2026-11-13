// Provider readiness separates a connected carrier from processed identity
// registration. The production key manager owns retries and key generations.
package sdk

// The headless production readiness boundary requires both the live carrier
// and the current key's processed registration. Neither implies the other.
func (self *DeviceLocal) GetProviderReady() bool {
	self.stateLock.Lock()
	provider, closed := self.provider, self.closed
	self.stateLock.Unlock()
	if closed || provider == nil || !provider.IsConnected() {
		return false
	}
	client := provider.Client()
	return client != nil && client.ClientKeyManager() != nil && client.ClientKeyManager().Registered()
}

// Returns false for closed, absent or legacy clients. No device lock spans
// key-manager or transport work.
func (self *DeviceLocal) GetProviderClientKeyRegistered() bool {
	self.stateLock.Lock()
	provider, closed := self.provider, self.closed
	self.stateLock.Unlock()
	if closed || provider == nil {
		return false
	}
	client := provider.Client()
	return client != nil && client.ClientKeyManager() != nil && client.ClientKeyManager().Registered()
}
