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

// Whether the platform holds the provider client's current key: its processed
// registration where the device requires one
// (`ClientSettings.ClientKeyRegistrationRequired`), otherwise the platform's
// acknowledgment of the ClientKey control that published it (connect
// `ClientKeyManager.Published`). A device that requires no processed
// registration reads its key's standing here, since its registration never
// reads true. False for closed or absent providers. No device lock spans
// key-manager work.
func (self *DeviceLocal) GetProviderClientKeyPublished() bool {
	self.stateLock.Lock()
	provider, closed := self.provider, self.closed
	self.stateLock.Unlock()
	if closed || provider == nil {
		return false
	}
	client := provider.Client()
	return client != nil && client.ClientKeyManager() != nil && client.ClientKeyManager().Published()
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
