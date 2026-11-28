package sdk

// device_local_provider_connected.go -- the changes of a local device's
// GetProviderConnected, for the device rpc, which pushes them to the remotes
// that hold the provider state (DeviceRemoteProviderState).
//
// The provider is connected while its current transport generation has a
// registered route. A migration installs another generation and the close
// ends the provider, so the watch follows both the generation's own connect
// changes and the provider's installs.

import (
	"github.com/urnetwork/connect/v2026"
)

// Subscribes to every change of GetProviderConnected.
func (self *DeviceLocal) addProviderConnectedChangeCallback(callback func(providerConnected bool)) Sub {
	callbackId := self.providerConnectedChangeCallbacks.Add(callback)
	return newSub(func() {
		self.providerConnectedChangeCallbacks.Remove(callbackId)
	})
}

// Calls every subscriber with the new connected state.
func (self *DeviceLocal) providerConnectedChanged(providerConnected bool) {
	for _, callback := range self.providerConnectedChangeCallbacks.Get() {
		connect.HandleError(func() {
			callback(providerConnected)
		})
	}
}

// Emits each change of the provider's connected state. Each round subscribes to
// the provider's installs and then to the current generation's connect changes
// before it reads, so a change after the read wakes the next round instead of
// being lost. Every wake is an install, a close or a connect change, so the
// loop never spins.
func (self *DeviceLocal) watchProviderConnected(provider *deviceLocalProvider) {
	providerConnected := false
	for {
		platformTransport, closed, installNotify := provider.platformTransportNotify()
		var connectedNotify <-chan struct{}
		connected := false
		if !closed && platformTransport != nil {
			connectedNotify = platformTransport.ConnectedNotify()
			connected = platformTransport.IsConnected()
		}
		if connected != providerConnected {
			providerConnected = connected
			self.providerConnectedChanged(providerConnected)
		}
		select {
		case <-self.ctx.Done():
			return
		case <-installNotify:
		case <-connectedNotify:
		}
	}
}
