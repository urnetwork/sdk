package sdk

// device_remote_provide_share.go -- the share limits through the device rpc
// (provide_share.go). The app sets them on its remote; the device process,
// which runs the provider, applies and keeps them, and pushes its status.
//
// The limits follow the provider extender setting's pattern: a write while
// the device process cannot be reached is queued and replayed at the next
// sync, and a device process too old to have the setter drops the write
// rather than replaying it on every reconnect. The status follows the client
// limit status: the getter reads through and keeps the last value, and the
// listener is compatible, so an app attached to an older device process keeps
// its session and reads the getter.

import (
	"time"

	"github.com/urnetwork/connect"
)

func cloneProvideShareLimits(limits *ProvideShareLimits) *ProvideShareLimits {
	if limits == nil {
		return nil
	}
	clone := *limits
	return &clone
}

func cloneProvideShareStatus(status *ProvideShareStatus) *ProvideShareStatus {
	if status == nil {
		return nil
	}
	clone := *status
	return &clone
}

// The status of a device that shares nothing: no limits and no data this
// month.
func noneProvideShareStatus(now time.Time) *ProvideShareStatus {
	start, end := provideSharePeriod(now, time.UTC)
	return &ProvideShareStatus{
		PeriodStartMillis: start.UnixMilli(),
		PeriodEndMillis:   end.UnixMilli(),
	}
}

// GetProvideShareLimits reads the limits through to the local device. A device
// that cannot be reached, or one too old to answer, reads the value queued
// for it, else the last value read, else none.
func (self *DeviceRemote) GetProvideShareLimits() *ProvideShareLimits {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	// a pending write is the device's state once delivered
	if self.state.ProvideShareLimits.IsSet {
		return cloneProvideShareLimits(self.state.ProvideShareLimits.Value)
	}
	if self.service != nil {
		limits, err := rpcCallNoArgAllowMissingMethod[*ProvideShareLimits](
			self.service,
			"DeviceLocalRpc.GetProvideShareLimits",
			self.closeService,
		)
		if err == nil && limits != nil {
			self.lastKnownState.ProvideShareLimits.Set(cloneProvideShareLimits(limits))
			return limits
		}
	}
	if limits := self.lastKnownState.ProvideShareLimits.Get(nil); limits != nil {
		return cloneProvideShareLimits(limits)
	}
	return &ProvideShareLimits{}
}

// SetProvideShareLimits writes the limits through to the local device, which
// keeps them and applies them at once. While the device process cannot be
// reached the value is queued and replayed at the next sync; a device process
// too old to have the setter drops the write. A hosted device has no provider,
// so the setter is guarded there as SetProvideMode is.
func (self *DeviceRemote) SetProvideShareLimits(limits *ProvideShareLimits) {
	if self.hostedIncompatibleGuarded("SetProvideShareLimits") {
		return
	}
	normal := normalProvideShareLimits(limits)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	// a queued value is kept as last known too, as SetProvideExtender keeps it
	if self.service == nil {
		self.state.ProvideShareLimits.Set(cloneProvideShareLimits(&normal))
		self.lastKnownState.ProvideShareLimits.Set(cloneProvideShareLimits(&normal))
		return
	}
	err := rpcCallVoidAllowMissingMethod(
		self.service,
		"DeviceLocalRpc.SetProvideShareLimits",
		&normal,
		self.closeService,
	)
	switch {
	case err == nil:
		self.state.ProvideShareLimits.Unset()
		self.lastKnownState.ProvideShareLimits.Set(cloneProvideShareLimits(&normal))
	case rpcMissingMethodError(err):
		self.state.ProvideShareLimits.Unset()
		self.log.Infof("[dr]provide share limits dropped: the device process has no setter")
	default:
		self.state.ProvideShareLimits.Set(cloneProvideShareLimits(&normal))
		self.lastKnownState.ProvideShareLimits.Set(cloneProvideShareLimits(&normal))
	}
}

// GetProvideShareStatus reads the status through to the local device. With no
// service it answers the last status read or pushed, else none.
func (self *DeviceRemote) GetProvideShareStatus() *ProvideShareStatus {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.service != nil {
		status, err := rpcCallNoArgAllowMissingMethod[*ProvideShareStatus](
			self.service,
			"DeviceLocalRpc.GetProvideShareStatus",
			self.closeService,
		)
		if err == nil && status != nil {
			self.lastProvideShareStatus = cloneProvideShareStatus(status)
			return status
		}
	}
	if self.lastProvideShareStatus != nil {
		return cloneProvideShareStatus(self.lastProvideShareStatus)
	}
	return noneProvideShareStatus(time.Now())
}

// The status mirrors as the client limit status does: the local device's
// callback is forwarded over the reverse rpc and re-emitted here. Compatible,
// so an app attached to a device process that does not have the listener yet
// keeps its session and degrades to the getter.
func (self *DeviceRemote) AddProvideShareStatusChangeListener(listener ProvideShareStatusChangeListener) Sub {
	return addCompatibleListener(
		self,
		listener,
		self.provideShareStatusChangeListeners,
		"DeviceLocalRpc.AddProvideShareStatusChangeListener",
		"DeviceLocalRpc.RemoveProvideShareStatusChangeListener",
	)
}

// The pushed status refreshes the cached last value, so a getter called right
// after a disconnect answers what the device last published.
func (self *DeviceRemote) provideShareStatusChanged(status *ProvideShareStatus) {
	listenerList := func() []ProvideShareStatusChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if status != nil {
			self.lastProvideShareStatus = cloneProvideShareStatus(status)
		}
		return listenerList(self.provideShareStatusChangeListeners)
	}()
	// a listener is handed the published value, never nil
	published := cloneProvideShareStatus(status)
	if published == nil {
		published = noneProvideShareStatus(time.Now())
	}
	for _, provideShareStatusChangeListener := range listenerList {
		connect.HandleError(func() {
			provideShareStatusChangeListener.ProvideShareStatusChanged(cloneProvideShareStatus(published))
		})
	}
}

func (self *DeviceLocalRpc) GetProvideShareLimits(_ RpcNoArg, limits **ProvideShareLimits) error {
	*limits = self.deviceLocal.GetProvideShareLimits()
	return nil
}

func (self *DeviceLocalRpc) SetProvideShareLimits(limits *ProvideShareLimits, _ RpcVoid) error {
	if self.hostedIncompatibleRpcGuarded("SetProvideShareLimits") {
		return nil
	}
	self.deviceLocal.SetProvideShareLimits(limits)
	return nil
}

func (self *DeviceLocalRpc) GetProvideShareStatus(_ RpcNoArg, status **ProvideShareStatus) error {
	*status = self.deviceLocal.GetProvideShareStatus()
	return nil
}

// AddProvideShareStatusChangeListener forwards the device's provide share
// status to the remote listener listenerId.
func (self *DeviceLocalRpc) AddProvideShareStatusChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addProvideShareStatusChangeListenerWithLock(listenerId)
	return nil
}

// Subscribes to the device once, for the first remote listener.
func (self *DeviceLocalRpc) addProvideShareStatusChangeListenerWithLock(listenerId connect.Id) {
	self.provideShareStatusChangeListenerIds[listenerId] = true
	if self.provideShareStatusChangeListenerSub == nil {
		self.provideShareStatusChangeListenerSub = self.deviceLocal.AddProvideShareStatusChangeListener(self)
	}
}

// Unsubscribes from the device with the last remote listener.
func (self *DeviceLocalRpc) removeProvideShareStatusChangeListenerWithLock(listenerId connect.Id) {
	delete(self.provideShareStatusChangeListenerIds, listenerId)
	if len(self.provideShareStatusChangeListenerIds) == 0 && self.provideShareStatusChangeListenerSub != nil {
		self.provideShareStatusChangeListenerSub.Close()
		self.provideShareStatusChangeListenerSub = nil
	}
}

// RemoveProvideShareStatusChangeListener stops forwarding to the remote
// listener listenerId.
func (self *DeviceLocalRpc) RemoveProvideShareStatusChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeProvideShareStatusChangeListenerWithLock(listenerId)
	return nil
}

// ProvideShareStatusChanged is the device's ProvideShareStatusChangeListener.
func (self *DeviceLocalRpc) ProvideShareStatusChanged(status *ProvideShareStatus) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.provideShareStatusChangedWithLock(status)
}

// Enqueues an async, coalescing reverse notification (see sendLoop). Every
// field is a plain value, so the status crosses as it stands; gob cannot
// carry nil, so none is sent as a status of no sharing.
func (self *DeviceLocalRpc) provideShareStatusChangedWithLock(status *ProvideShareStatus) {
	if status == nil {
		status = noneProvideShareStatus(time.Now())
	}
	self.reverseNotify("DeviceRemoteRpc.ProvideShareStatusChanged", status)
}

// ProvideShareStatusChanged re-emits a status the device process pushed.
func (self *DeviceRemoteRpc) ProvideShareStatusChanged(status *ProvideShareStatus, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]ProvideShareStatusChanged")
	self.dispatch(func() {
		self.deviceRemote.provideShareStatusChanged(status)
	})
	return nil
}
