// The connect state a DeviceRemote shows its listeners: the connect location
// and whether connect is enabled.
//
// A local write (SetConnectLocation, Reconnect, SetDestination,
// RemoveDestination) is told to the listeners from the written value, without
// waiting for the device to report it back. While the rpc is live the write is
// told once the device applied it, so a grid the view builds for it binds to
// the device's new window, never the outgoing one. While the rpc is down, or a
// sync is still in flight, the write is pending and told at once; the device
// hears it from the next sync. So a connect tap shows the connecting state
// immediately.
//
// The device then reports its state. A report equal to what the listeners were
// last told changes nothing and is not told again; any other report is the
// device's own change and is told. A listener that registers seeds itself from
// the getters, and the device pushes nothing while the remote has no listener,
// so registering forgets what was told: the next report is told whatever it
// is. A report that arrives while a local write is still pending was made
// before the device had the write (the replay of a sync that started before
// the tap, or a push still in flight), so it is dropped: the next sync applies
// the write over it, and telling it would flip the view back until then. The
// pending write is kept as last known instead, also over the reply of a sync
// that was in flight when it was made, so the getters answer with it until a
// sync delivers it.
//
// One caller at a time delivers to the listeners, always the latest told
// state, so the listeners hear the changes in order and the latest last, even
// when writes and reports race on several goroutines. A caller that finds a
// delivery running leaves its newer state to that delivery, which also keeps a
// listener that writes from its callback from deadlocking. A delivery that
// ends abnormally (a listener exits its goroutine) still lets the next one run.
//
// The state below is guarded by `DeviceRemote.stateLock`.
package sdk

import (
	"github.com/urnetwork/connect"
)

// The connect state the listeners are told, with a sequence per value that
// advances on every change to tell and the sequence last delivered.
type deviceRemoteConnectTold struct {
	location          *ConnectLocation
	locationSet       bool
	locationSequence  uint64
	locationDelivered uint64

	connectEnabled          bool
	connectEnabledSet       bool
	connectEnabledSequence  uint64
	connectEnabledDelivered uint64

	delivering bool
}

// whether connect is enabled by the state's pending or last known write.
// The bool reports whether the state holds a write.
func deviceRemoteStateConnectEnabled(state *DeviceRemoteState) (bool, bool) {
	if state.RemoveDestination.IsSet {
		return false, true
	}
	if state.Location.IsSet {
		return state.Location.Value != nil && state.Location.Value.ConnectLocation != nil, true
	}
	if state.Destination.IsSet {
		// a spec-only custom destination may have no display location; the spec
		// list is the installed connection command
		return state.Destination.Value != nil && 0 < len(state.Destination.Value.Specs), true
	}
	return false, false
}

// must be called with stateLock
func (self *DeviceRemote) hasPendingConnectStateWithLock() bool {
	return self.state.RemoveDestination.IsSet ||
		self.state.Location.IsSet ||
		self.state.Destination.IsSet
}

// Keeps a pending write as last known, so the getters still answer with it
// while a sync that took the write is in flight. Must be called with stateLock.
func (self *DeviceRemote) recordPendingConnectWriteWithLock(write *DeviceRemoteState) {
	location, _ := deviceRemoteStateConnectLocation(write)
	connectEnabled, _ := deviceRemoteStateConnectEnabled(write)
	self.lastKnownState.Location.Set(newDeviceRemoteConnectLocation(location))
	self.lastKnownState.Destination.Unset()
	self.lastKnownState.RemoveDestination.Unset()
	self.lastKnownState.ConnectEnabled.Set(connectEnabled)
}

// A registering listener seeds itself from the getters, so the device's next
// report is told whatever was told before. Must be called with stateLock.
func (self *DeviceRemote) forgetToldConnectLocationWithLock() {
	self.connectTold.locationSet = false
}

// must be called with stateLock
func (self *DeviceRemote) forgetToldConnectEnabledWithLock() {
	self.connectTold.connectEnabledSet = false
}

// Tells the listeners a local write and returns whether a delivery is due.
// A rebuild (Reconnect) tells its location even when unchanged: it is a new
// connection, and the view builds a new grid for it. Must be called with
// stateLock.
func (self *DeviceRemote) tellConnectWriteWithLock(write *DeviceRemoteState, rebuild bool) bool {
	location, _ := deviceRemoteStateConnectLocation(write)
	connectEnabled, _ := deviceRemoteStateConnectEnabled(write)
	locationDue := self.tellConnectLocationWithLock(location, rebuild)
	connectEnabledDue := self.tellConnectEnabledWithLock(connectEnabled)
	return locationDue || connectEnabledDue
}

// must be called with stateLock
func (self *DeviceRemote) tellConnectLocationWithLock(location *ConnectLocation, always bool) bool {
	told := &self.connectTold
	if !always && told.locationSet && connectLocationValuesEqual(told.location, location) {
		return false
	}
	told.location = cloneConnectLocation(location)
	told.locationSet = true
	told.locationSequence += 1
	return true
}

// must be called with stateLock
func (self *DeviceRemote) tellConnectEnabledWithLock(connectEnabled bool) bool {
	told := &self.connectTold
	if told.connectEnabledSet && told.connectEnabled == connectEnabled {
		return false
	}
	told.connectEnabled = connectEnabled
	told.connectEnabledSet = true
	told.connectEnabledSequence += 1
	return true
}

// Delivers the told state to the listeners, unless another caller is
// delivering: that caller delivers the latest state before it stops. Never
// called with stateLock.
func (self *DeviceRemote) deliverConnectState() {
	self.stateLock.Lock()
	told := &self.connectTold
	if told.delivering {
		self.stateLock.Unlock()
		return
	}
	told.delivering = true
	locked := true
	defer func() {
		// also when a listener exits the goroutine (runtime.Goexit), which the
		// listeners' panic containment does not catch
		if !locked {
			self.stateLock.Lock()
		}
		told.delivering = false
		self.stateLock.Unlock()
	}()
	for {
		locationDue := told.locationDelivered != told.locationSequence
		connectEnabledDue := told.connectEnabledDelivered != told.connectEnabledSequence
		if !locationDue && !connectEnabledDue {
			return
		}
		location := cloneConnectLocation(told.location)
		connectEnabled := told.connectEnabled
		told.locationDelivered = told.locationSequence
		told.connectEnabledDelivered = told.connectEnabledSequence
		var connectLocationListeners []ConnectLocationChangeListener
		if locationDue {
			connectLocationListeners = listenerList(self.connectLocationChangeListeners)
		}
		var connectListeners []ConnectChangeListener
		if connectEnabledDue {
			connectListeners = listenerList(self.connectChangeListeners)
		}
		locked = false
		self.stateLock.Unlock()

		// listener panics are contained, so the delivery resumes here
		for _, connectLocationChangeListener := range connectLocationListeners {
			connect.HandleError(func() {
				connectLocationChangeListener.ConnectLocationChanged(cloneConnectLocation(location))
			})
		}
		for _, connectChangeListener := range connectListeners {
			connect.HandleError(func() {
				connectChangeListener.ConnectChanged(connectEnabled)
			})
		}

		self.stateLock.Lock()
		locked = true
	}
}

// The device's report of its connect location (reverse rpc).
func (self *DeviceRemote) connectLocationReported(deviceRemoteLocation *DeviceRemoteConnectLocation) {
	if deviceRemoteLocation == nil {
		deviceRemoteLocation = newDeviceRemoteConnectLocation(nil)
	}
	deliver := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if self.hasPendingConnectStateWithLock() {
			// made before the device had the pending write, which the next
			// sync applies over it
			return false
		}
		self.lastKnownState.Location.Set(deviceRemoteLocation)
		self.lastKnownState.Destination.Unset()
		self.lastKnownState.RemoveDestination.Unset()
		return self.tellConnectLocationWithLock(deviceRemoteLocation.toConnectLocation(), false)
	}()
	if deliver {
		self.deliverConnectState()
	}
}

// The device's report of whether connect is enabled (reverse rpc).
func (self *DeviceRemote) connectEnabledReported(connectEnabled bool) {
	deliver := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if self.hasPendingConnectStateWithLock() {
			// made before the device had the pending write, which the next
			// sync applies over it
			return false
		}
		self.lastKnownState.ConnectEnabled.Set(connectEnabled)
		return self.tellConnectEnabledWithLock(connectEnabled)
	}()
	if deliver {
		self.deliverConnectState()
	}
}
