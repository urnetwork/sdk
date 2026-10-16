// The connect state a DeviceRemote shows its listeners: the connect location
// and whether connect is enabled.
//
// A local write (SetConnectLocation, Reconnect, SetDestination,
// RemoveDestination) is announced to the listeners from the written value,
// without waiting for the device to report it back. While the rpc is live the
// announcement follows the device applying the write, so a grid the view
// builds for it binds to the device's new window, never the outgoing one.
// While the rpc is down, or a sync is still in flight, the write is pending
// and announced at once; the device hears it from the next sync. So a connect
// tap shows the connecting state immediately.
//
// The device then reports the state it applied. A report that only repeats
// what the listeners were announced is that write's echo and is not announced
// again. A report that arrives while a local write is still pending was made
// before the device had the write (the replay of a sync that started before
// the tap, or a push still in flight), so it is dropped: the next sync applies
// the write over it, and announcing it would flip the view back until then.
// The pending write is kept as last known instead, so the getters answer with
// it while that sync is in flight.
//
// The state below is guarded by `DeviceRemote.stateLock`.
package sdk

import (
	"github.com/urnetwork/connect"
)

// bounds the echoes kept for a device that never reports some writes
// (a write that does not change the device's state is not reported)
const deviceRemoteConnectEchoLimit = 16

// the connect state announced for one local write
type deviceRemoteConnectAnnouncement struct {
	location       *ConnectLocation
	connectEnabled bool
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

// Reads the written connect state and expects the device to echo it. Must be
// called with stateLock.
func (self *DeviceRemote) announceConnectWriteWithLock(write *DeviceRemoteState) *deviceRemoteConnectAnnouncement {
	location, _ := deviceRemoteStateConnectLocation(write)
	connectEnabled, _ := deviceRemoteStateConnectEnabled(write)

	self.expectedConnectLocationEchoes = appendDeviceRemoteConnectEcho(
		self.expectedConnectLocationEchoes,
		cloneConnectLocation(location),
	)
	self.expectedConnectEnabledEchoes = appendDeviceRemoteConnectEcho(
		self.expectedConnectEnabledEchoes,
		connectEnabled,
	)

	return &deviceRemoteConnectAnnouncement{
		location:       location,
		connectEnabled: connectEnabled,
	}
}

// Keeps a pending write's announcement as last known, so the getters still
// answer with it while a sync that took the write is in flight. Must be called
// with stateLock.
func (self *DeviceRemote) recordPendingConnectWriteWithLock(announcement *deviceRemoteConnectAnnouncement) {
	self.lastKnownState.Location.Set(newDeviceRemoteConnectLocation(announcement.location))
	self.lastKnownState.Destination.Unset()
	self.lastKnownState.RemoveDestination.Unset()
	self.lastKnownState.ConnectEnabled.Set(announcement.connectEnabled)
}

func appendDeviceRemoteConnectEcho[T any](echoes []T, echo T) []T {
	echoes = append(echoes, echo)
	if deviceRemoteConnectEchoLimit < len(echoes) {
		echoes = echoes[len(echoes)-deviceRemoteConnectEchoLimit:]
	}
	return echoes
}

// Consumes the expected echoes through the first one equal to the report and
// returns true. A device coalesces its pushes (last value wins), so a report
// may skip earlier echoes. A report equal to no echo is a change of its own:
// the echoes are dropped and false is returned.
func consumeDeviceRemoteConnectEcho[T any](echoes []T, report T, equal func(T, T) bool) ([]T, bool) {
	for i, echo := range echoes {
		if equal(echo, report) {
			return echoes[i+1:], true
		}
	}
	return nil, false
}

// Delivers an announcement to the listeners. Never called with stateLock.
func (self *DeviceRemote) announceConnectState(announcement *deviceRemoteConnectAnnouncement) {
	connectLocationListeners, connectListeners := func() ([]ConnectLocationChangeListener, []ConnectChangeListener) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return listenerList(self.connectLocationChangeListeners), listenerList(self.connectChangeListeners)
	}()
	for _, connectLocationChangeListener := range connectLocationListeners {
		connect.HandleError(func() {
			connectLocationChangeListener.ConnectLocationChanged(cloneConnectLocation(announcement.location))
		})
	}
	for _, connectChangeListener := range connectListeners {
		connect.HandleError(func() {
			connectChangeListener.ConnectChanged(announcement.connectEnabled)
		})
	}
}

// The device's report of its connect location (reverse rpc).
func (self *DeviceRemote) connectLocationReported(deviceRemoteLocation *DeviceRemoteConnectLocation) {
	if deviceRemoteLocation == nil {
		deviceRemoteLocation = newDeviceRemoteConnectLocation(nil)
	}
	announce := func() bool {
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
		echoes, echo := consumeDeviceRemoteConnectEcho(
			self.expectedConnectLocationEchoes,
			deviceRemoteLocation.toConnectLocation(),
			connectLocationValuesEqual,
		)
		self.expectedConnectLocationEchoes = echoes
		return !echo
	}()
	if announce {
		self.connectLocationChanged(deviceRemoteLocation)
	}
}

// The device's report of whether connect is enabled (reverse rpc).
func (self *DeviceRemote) connectEnabledReported(connectEnabled bool) {
	announce := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if self.hasPendingConnectStateWithLock() {
			// made before the device had the pending write, which the next
			// sync applies over it
			return false
		}
		self.lastKnownState.ConnectEnabled.Set(connectEnabled)
		echoes, echo := consumeDeviceRemoteConnectEcho(
			self.expectedConnectEnabledEchoes,
			connectEnabled,
			func(a bool, b bool) bool { return a == b },
		)
		self.expectedConnectEnabledEchoes = echoes
		return !echo
	}()
	if announce {
		self.connectChanged(connectEnabled)
	}
}
