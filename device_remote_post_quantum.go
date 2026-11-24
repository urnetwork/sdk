package sdk

// device_remote_post_quantum.go -- the device's post-quantum notice
// (post_quantum_unavailable.go) across the device rpc. The device process
// decides it (device_local_post_quantum.go); a DeviceRemote mirrors it the
// way it mirrors the client limit status: a getter that reads through, the
// device's listener forwarded over the reverse rpc and re-emitted, and the
// last value kept for while the device process cannot be reached.
//
// Compatible with every device process of the current rpc version. The
// listener is added as a compatible listener and the getter allows a missing
// method, so a remote attached to a device process that predates the notice
// keeps its session and reads no notice, which is what such a device would
// decide. The notice only ever withholds "connecting" in favor of a choice,
// so no notice is the safe default. A remote that predates it never registers
// the listener, so the device process never pushes it one.

import (
	"github.com/urnetwork/connect"
)

// The notice on the wire. ConnectLocation crosses the rpc as its value form,
// as the connect location does.
//
//gomobile:noexport
type DeviceRemotePostQuantumUnavailable struct {
	// the notice's location; nil when there is no notice, since a notice
	// always names its location
	Location *DeviceRemoteConnectLocationValue
}

func newDeviceRemotePostQuantumUnavailable(postQuantumUnavailable *PostQuantumUnavailable) *DeviceRemotePostQuantumUnavailable {
	if postQuantumUnavailable == nil || postQuantumUnavailable.Location == nil {
		return &DeviceRemotePostQuantumUnavailable{}
	}
	return &DeviceRemotePostQuantumUnavailable{
		Location: newDeviceRemoteConnectLocationValue(postQuantumUnavailable.Location),
	}
}

// The notice, nil for none and for a nil value.
func (self *DeviceRemotePostQuantumUnavailable) toPostQuantumUnavailable() *PostQuantumUnavailable {
	if self == nil || self.Location == nil {
		return nil
	}
	return &PostQuantumUnavailable{
		Location: self.Location.toConnectLocation(),
	}
}

// GetPostQuantumUnavailable reads through to the local device, which decides
// the notice (on ios, the packet tunnel extension). With no service it
// answers the last notice the device answered or pushed. A device process too
// old to answer keeps its rpc session and reads no notice.
func (self *DeviceRemote) GetPostQuantumUnavailable() *PostQuantumUnavailable {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.service != nil {
		postQuantumUnavailable, err := rpcCallNoArgAllowMissingMethod[*DeviceRemotePostQuantumUnavailable](
			self.service,
			"DeviceLocalRpc.GetPostQuantumUnavailable",
			self.closeService,
		)
		if err == nil {
			self.lastPostQuantumUnavailable = postQuantumUnavailable.toPostQuantumUnavailable()
		}
	}
	return self.lastPostQuantumUnavailable
}

// The notice mirrors as the client limit status does: the local device's
// listener is forwarded over the reverse rpc and re-emitted here. Compatible,
// so an app attached to a device process without the listener keeps its
// session and reads no notice.
func (self *DeviceRemote) AddPostQuantumUnavailableChangeListener(listener PostQuantumUnavailableChangeListener) Sub {
	return addCompatibleListener(
		self,
		listener,
		self.postQuantumUnavailableChangeListeners,
		"DeviceLocalRpc.AddPostQuantumUnavailableChangeListener",
		"DeviceLocalRpc.RemovePostQuantumUnavailableChangeListener",
	)
}

// The pushed notice refreshes the kept value, so a getter called right after
// a disconnect answers the notice the device last published.
func (self *DeviceRemote) postQuantumUnavailableChanged(postQuantumUnavailable *PostQuantumUnavailable) {
	listenerList := func() []PostQuantumUnavailableChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.lastPostQuantumUnavailable = postQuantumUnavailable
		return listenerList(self.postQuantumUnavailableChangeListeners)
	}()
	for _, postQuantumUnavailableChangeListener := range listenerList {
		connect.HandleError(func() {
			postQuantumUnavailableChangeListener.PostQuantumUnavailableChanged(postQuantumUnavailable)
		})
	}
}

// PostQuantumUnavailableChanged re-emits a notice the device process pushed.
func (self *DeviceRemoteRpc) PostQuantumUnavailableChanged(postQuantumUnavailable *DeviceRemotePostQuantumUnavailable, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]PostQuantumUnavailableChanged")
	self.dispatch(func() {
		self.deviceRemote.postQuantumUnavailableChanged(postQuantumUnavailable.toPostQuantumUnavailable())
	})
	return nil
}

// AddPostQuantumUnavailableChangeListener forwards the device's post-quantum
// notice to the remote listener listenerId.
func (self *DeviceLocalRpc) AddPostQuantumUnavailableChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addPostQuantumUnavailableChangeListenerWithLock(listenerId)
	return nil
}

// addPostQuantumUnavailableChangeListenerWithLock subscribes to the device
// once, for the first remote listener.
func (self *DeviceLocalRpc) addPostQuantumUnavailableChangeListenerWithLock(listenerId connect.Id) {
	self.postQuantumUnavailableChangeListenerIds[listenerId] = true
	if self.postQuantumUnavailableChangeListenerSub == nil {
		self.postQuantumUnavailableChangeListenerSub = self.deviceLocal.AddPostQuantumUnavailableChangeListener(self)
	}
}

// removePostQuantumUnavailableChangeListenerWithLock unsubscribes from the
// device with the last remote listener.
func (self *DeviceLocalRpc) removePostQuantumUnavailableChangeListenerWithLock(listenerId connect.Id) {
	delete(self.postQuantumUnavailableChangeListenerIds, listenerId)
	if len(self.postQuantumUnavailableChangeListenerIds) == 0 && self.postQuantumUnavailableChangeListenerSub != nil {
		self.postQuantumUnavailableChangeListenerSub.Close()
		self.postQuantumUnavailableChangeListenerSub = nil
	}
}

// RemovePostQuantumUnavailableChangeListener stops forwarding to the remote
// listener listenerId.
func (self *DeviceLocalRpc) RemovePostQuantumUnavailableChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removePostQuantumUnavailableChangeListenerWithLock(listenerId)
	return nil
}

// PostQuantumUnavailableChanged is the device's
// PostQuantumUnavailableChangeListener.
func (self *DeviceLocalRpc) PostQuantumUnavailableChanged(postQuantumUnavailable *PostQuantumUnavailable) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.postQuantumUnavailableChangedWithLock(postQuantumUnavailable)
}

// postQuantumUnavailableChangedWithLock enqueues an async, coalescing reverse
// notification (see sendLoop), so the remote hears the latest notice.
func (self *DeviceLocalRpc) postQuantumUnavailableChangedWithLock(postQuantumUnavailable *PostQuantumUnavailable) {
	self.reverseNotify(
		"DeviceRemoteRpc.PostQuantumUnavailableChanged",
		newDeviceRemotePostQuantumUnavailable(postQuantumUnavailable),
	)
}

// GetPostQuantumUnavailable answers the device's post-quantum notice.
func (self *DeviceLocalRpc) GetPostQuantumUnavailable(_ RpcNoArg, postQuantumUnavailable **DeviceRemotePostQuantumUnavailable) error {
	*postQuantumUnavailable = newDeviceRemotePostQuantumUnavailable(self.deviceLocal.GetPostQuantumUnavailable())
	return nil
}
