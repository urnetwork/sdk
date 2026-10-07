package sdk

// device_local_client_limit.go -- the client limit status of a local device
// (client_limit_status.go): the getter, the listener, and the watch that turns
// the provider's client limit hold into callbacks.
//
// The device's platform connection is its provider's (deviceLocalProvider),
// which every app build runs whether or not it provides, so the hold of the
// provider's transports is the device's hold. A device built without a
// provider has no platform connection to hold and always reads
// ClientLimitStatusNone.

import (
	"github.com/urnetwork/connect"
)

// GetClientLimitStatus is the client limit status of the device's platform
// connection. Never nil.
func (self *DeviceLocal) GetClientLimitStatus() *ClientLimitStatus {
	provider, closed := func() (*deviceLocalProvider, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.provider, self.closed
	}()
	if closed || provider == nil {
		return noneClientLimitStatus()
	}
	status, _ := provider.clientLimitStatus()
	return newClientLimitStatus(status)
}

// AddClientLimitStatusChangeListener subscribes to every change of the client
// limit status: a hold starting, and a hold ending or being reset.
func (self *DeviceLocal) AddClientLimitStatusChangeListener(listener ClientLimitStatusChangeListener) Sub {
	callbackId := self.clientLimitStatusChangeListeners.Add(listener)
	return newSub(func() {
		self.clientLimitStatusChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) clientLimitStatusChanged(status *ClientLimitStatus) {
	for _, listener := range self.clientLimitStatusChangeListeners.Get() {
		connect.HandleError(func() {
			listener.ClientLimitStatusChanged(cloneClientLimitStatus(status))
		})
	}
}

// watchClientLimitStatus emits each change of the provider's client limit
// hold. The hold notifies only on an actual change, so every wake carries a
// new status. update is armed by the constructor before this goroutine starts,
// so a hold that starts the instant the constructor returns still wakes it.
func (self *DeviceLocal) watchClientLimitStatus(provider *deviceLocalProvider, update chan struct{}) {
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-update:
		}
		var status connect.ClientLimitStatus
		status, update = provider.clientLimitStatus()
		// contain a panic to the emit: a failed emit must never end the watch
		connect.HandleError(func() {
			self.clientLimitStatusChanged(newClientLimitStatus(status))
		})
	}
}
