// The settings a DeviceRemote shows its listeners besides its connect state:
// the provide mode, the provide control mode, provide paused, offline (the
// offline listeners also hear the vpn interface while offline), the vpn
// interface while offline, route local and the blocker, and provide enabled,
// which differs as below. Each is told as the connect state is
// (device_remote_connect_state.go), one value at a time.
//
// A local write is told to the listeners from the written value, without
// waiting for the device to report it back: once the device applied it while
// the rpc is live, and at once while the write is pending for the next sync.
// While a write the provide mode follows is pending (its own, the control
// mode's, or a connect write's, which moves the auto control mode between
// public and network), the getters derive the provide mode from the pending
// writes, and every such write tells the derived mode, so the listeners hear
// what the getters answer. While the rpc is live the device applies a control
// mode's or a connect write's provide mapping itself and reports the mode.
//
// A live write counts as applied once the device's rpc returns success. A
// device that accepted a write without applying it would leave the told value
// stuck until a resync, since no report follows. The hosted-incompatible
// guards do accept without applying (DeviceLocalRpc.hostedIncompatibleRpcGuarded
// and DeviceLocal.hostedIncompatibleGuarded), but no write reaches them today:
// every remote to a hosted device sets DisableHostedIncompatible, which drops
// the write before it is sent or told (DeviceRemote.hostedIncompatibleGuarded).
//
// The device then reports each value. A report equal to what the listeners
// were last told is not told again; any other report is the device's own
// change and is told. A listener that registers seeds itself from the getters,
// and the device pushes nothing while the remote has no listener, so
// registering forgets what was told: the next report is told whatever it is. A
// report that arrives while a write of its value is pending was made before
// the device had the write, so it is dropped: the next sync applies the write
// over it. The pending write is kept as last known instead, also over the
// reply of a sync that was in flight when it was made and over a push of the
// provider state, so the getters answer with it until a sync delivers it.
//
// One caller at a time delivers a value to its listeners, always the latest
// told, so the listeners hear its changes in order and the latest last, even
// when writes and reports race on several goroutines. A caller that finds a
// delivery of the value running leaves its newer value to that delivery, which
// also keeps a listener that writes from its callback from deadlocking. A
// delivery that ends abnormally (a listener exits its goroutine) still lets the
// next one run.
//
// Provide enabled (the provide change listeners) has no setter: it follows the
// provide mode. A pending provide mode, control mode or destination write tells
// the provide enabled state the getters derive from the pending writes, a
// report made while a write the provide mode follows is pending is dropped, the
// getters answer from the pending writes first, and the pending state is kept
// as last known, as for the values above. Unlike them, every value is told,
// also one equal to the last, and registering forgets nothing: DeviceLocal
// reports provide enabled again on every provide mode change, and consumers
// with no provide mode listener refresh the provide mode from it (the desktop
// apps' status).
//
// Nothing here writes or derives the provide control mode: only
// SetProvideControlMode and the device's reports change what its listeners are
// told, and a provide mode tell never touches it (upgrading to Pro must never
// change the provide control mode, owner rule 2026-10-02).
//
// The state below is guarded by `DeviceRemote.stateLock`.
package sdk

import (
	"github.com/urnetwork/connect"
)

// One value as its listeners were told it, with a sequence that advances on
// every change to tell, the sequence last delivered, and whether a caller is
// delivering it.
type deviceRemoteToldValue[T comparable] struct {
	value T
	// a value was told since a listener last registered
	set        bool
	sequence   uint64
	delivered  uint64
	delivering bool
}

// Records the value to tell and returns whether a delivery is due. An equal
// value is not told again. Must be called with stateLock.
func (self *deviceRemoteToldValue[T]) tellWithLock(value T) bool {
	if self.set && self.value == value {
		return false
	}
	self.value = value
	self.set = true
	self.sequence += 1
	return true
}

// Records the value to tell even when it equals the last told, and returns
// that a delivery is due. Must be called with stateLock.
func (self *deviceRemoteToldValue[T]) tellAgainWithLock(value T) bool {
	self.value = value
	self.set = true
	self.sequence += 1
	return true
}

// Must be called with stateLock.
func (self *deviceRemoteToldValue[T]) forgetWithLock() {
	self.set = false
}

// Takes the device's report of the value and returns whether a delivery is
// due. A report made while a write of the value is pending is dropped. Must be
// called with stateLock.
func (self *deviceRemoteToldValue[T]) reportWithLock(value T, pending bool, lastKnown *deviceRemoteValue[T]) bool {
	if pending {
		// made before the device had the pending write, which the next sync
		// applies over it
		return false
	}
	lastKnown.Set(value)
	return self.tellWithLock(value)
}

// What the offline listeners hear.
type deviceRemoteOffline struct {
	offline                  bool
	vpnInterfaceWhileOffline bool
}

// The settings the listeners are told.
type deviceRemoteSettingsTold struct {
	provideMode              deviceRemoteToldValue[ProvideMode]
	provideControlMode       deviceRemoteToldValue[ProvideControlMode]
	providePaused            deviceRemoteToldValue[bool]
	offline                  deviceRemoteToldValue[deviceRemoteOffline]
	vpnInterfaceWhileOffline deviceRemoteToldValue[bool]
	routeLocal               deviceRemoteToldValue[bool]
	blockerEnabled           deviceRemoteToldValue[bool]
	// every value is told, see the file header
	provideEnabled deviceRemoteToldValue[bool]
}

// A registering listener seeds itself from the getters, so the device's next
// report of the value is told whatever was told before.
func forgetToldValue[T comparable](deviceRemote *DeviceRemote, told *deviceRemoteToldValue[T]) {
	deviceRemote.stateLock.Lock()
	defer deviceRemote.stateLock.Unlock()
	told.forgetWithLock()
}

// Delivers the told value to the listeners, unless another caller is
// delivering it: that caller delivers the latest value before it stops. Never
// called with stateLock.
func deliverToldValue[T comparable, L any](
	deviceRemote *DeviceRemote,
	told *deviceRemoteToldValue[T],
	listeners map[connect.Id]L,
	call func(L, T),
) {
	deviceRemote.stateLock.Lock()
	if told.delivering {
		deviceRemote.stateLock.Unlock()
		return
	}
	told.delivering = true
	locked := true
	defer func() {
		// also when a listener exits the goroutine (runtime.Goexit), which the
		// listeners' panic containment does not catch
		if !locked {
			deviceRemote.stateLock.Lock()
		}
		told.delivering = false
		deviceRemote.stateLock.Unlock()
	}()
	for told.delivered != told.sequence {
		value := told.value
		told.delivered = told.sequence
		orderedListeners := listenerList(listeners)
		locked = false
		deviceRemote.stateLock.Unlock()

		// listener panics are contained, so the delivery resumes here
		for _, listener := range orderedListeners {
			connect.HandleError(func() {
				call(listener, value)
			})
		}

		deviceRemote.stateLock.Lock()
		locked = true
	}
}

// Never called with stateLock.
func (self *DeviceRemote) deliverProvideMode() {
	deliverToldValue(self, &self.settingsTold.provideMode, self.provideModeChangeListeners, ProvideModeChangeListener.ProvideModeChanged)
}

// Never called with stateLock.
func (self *DeviceRemote) deliverProvideControlMode() {
	deliverToldValue(self, &self.settingsTold.provideControlMode, self.provideControlModeChangeListeners, ProvideControlModeChangeListener.ProvideControlModeChanged)
}

// Never called with stateLock.
func (self *DeviceRemote) deliverProvidePaused() {
	deliverToldValue(self, &self.settingsTold.providePaused, self.providePausedChangeListeners, ProvidePausedChangeListener.ProvidePausedChanged)
}

// Never called with stateLock.
func (self *DeviceRemote) deliverOffline() {
	deliverToldValue(self, &self.settingsTold.offline, self.offlineChangeListeners, func(listener OfflineChangeListener, offline deviceRemoteOffline) {
		listener.OfflineChanged(offline.offline, offline.vpnInterfaceWhileOffline)
	})
}

// Never called with stateLock.
func (self *DeviceRemote) deliverVpnInterfaceWhileOffline() {
	deliverToldValue(self, &self.settingsTold.vpnInterfaceWhileOffline, self.vpnInterfaceWhileOfflineChangeListeners, VpnInterfaceWhileOfflineChangeListener.VpnInterfaceWhileOfflineChanged)
}

// Never called with stateLock.
func (self *DeviceRemote) deliverRouteLocal() {
	deliverToldValue(self, &self.settingsTold.routeLocal, self.routeLocalChangeListeners, RouteLocalChangeListener.RouteLocalChanged)
}

// Never called with stateLock.
func (self *DeviceRemote) deliverBlockerEnabled() {
	deliverToldValue(self, &self.settingsTold.blockerEnabled, self.blockerEnabledChangeListeners, BlockerEnabledChangeListener.BlockerEnabledChanged)
}

// Never called with stateLock.
func (self *DeviceRemote) deliverProvideEnabled() {
	deliverToldValue(self, &self.settingsTold.provideEnabled, self.provideChangeListeners, ProvideChangeListener.ProvideChanged)
}

// Tells the provide enabled state the getters derive from the pending writes,
// which include the caller's own, and keeps it as last known. Returns that a
// delivery is due. Must be called with stateLock.
func (self *DeviceRemote) tellPendingProvideEnabledWithLock() bool {
	provideEnabled := self.derivedProvideEnabledWithLock()
	self.lastKnownState.ProvideEnabled.Set(provideEnabled)
	return self.settingsTold.provideEnabled.tellAgainWithLock(provideEnabled)
}

// Tells the provide mode the getters derive while a write it follows is
// pending, and returns whether a delivery is due. Must be called with
// stateLock.
func (self *DeviceRemote) tellPendingProvideModeWithLock() bool {
	if !self.provideQueuedWithLock() {
		return false
	}
	return self.settingsTold.provideMode.tellWithLock(self.derivedProvideModeWithLock())
}

// What the offline listeners hear, from the pending or last known values. Must
// be called with stateLock.
func (self *DeviceRemote) offlineWithLock() deviceRemoteOffline {
	return deviceRemoteOffline{
		offline: self.state.Offline.Get(
			self.lastKnownState.Offline.Get(self.settings.DefaultOffline),
		),
		vpnInterfaceWhileOffline: self.state.VpnInterfaceWhileOffline.Get(
			self.lastKnownState.VpnInterfaceWhileOffline.Get(self.settings.DefaultVpnInterfaceWhileOffline),
		),
	}
}

// Keeps the pending setting writes as last known, so the getters still answer
// with them while a sync that took them is in flight. Must be called with
// stateLock.
func (self *DeviceRemote) recordPendingSettingWritesWithLock() {
	self.lastKnownState.ProvideMode.Merge(self.state.ProvideMode)
	self.lastKnownState.ProvideControlMode.Merge(self.state.ProvideControlMode)
	self.lastKnownState.ProvidePaused.Merge(self.state.ProvidePaused)
	self.lastKnownState.Offline.Merge(self.state.Offline)
	self.lastKnownState.VpnInterfaceWhileOffline.Merge(self.state.VpnInterfaceWhileOffline)
	self.lastKnownState.RouteLocal.Merge(self.state.RouteLocal)
	self.lastKnownState.BlockerEnabled.Merge(self.state.BlockerEnabled)
	if self.provideQueuedWithLock() {
		// provide enabled follows the pending writes
		self.lastKnownState.ProvideEnabled.Set(self.derivedProvideEnabledWithLock())
	}
}

// The device's report of its provide mode (reverse rpc). The mode follows a
// pending control mode or connect write as well as its own.
func (self *DeviceRemote) provideModeReported(provideMode ProvideMode) {
	deliver := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.settingsTold.provideMode.reportWithLock(
			provideMode,
			self.provideQueuedWithLock(),
			&self.lastKnownState.ProvideMode,
		)
	}()
	if deliver {
		self.deliverProvideMode()
	}
}

// The device's report of provide enabled (reverse rpc), which follows a pending
// control mode, provide mode or connect write as the provide mode does. Any
// other report is told, also one equal to the last (see the file header).
func (self *DeviceRemote) provideEnabledReported(provideEnabled bool) {
	deliver := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.provideQueuedWithLock() {
			// made before the device had the pending write, which the next
			// sync applies over it
			return false
		}
		self.lastKnownState.ProvideEnabled.Set(provideEnabled)
		return self.settingsTold.provideEnabled.tellAgainWithLock(provideEnabled)
	}()
	if deliver {
		self.deliverProvideEnabled()
	}
}

// The device's report of its provide control mode (reverse rpc).
func (self *DeviceRemote) provideControlModeReported(provideControlMode ProvideControlMode) {
	deliver := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.settingsTold.provideControlMode.reportWithLock(
			provideControlMode,
			self.state.ProvideControlMode.IsSet,
			&self.lastKnownState.ProvideControlMode,
		)
	}()
	if deliver {
		self.deliverProvideControlMode()
	}
}

// The device's report of provide paused (reverse rpc).
func (self *DeviceRemote) providePausedReported(providePaused bool) {
	deliver := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.settingsTold.providePaused.reportWithLock(
			providePaused,
			self.state.ProvidePaused.IsSet,
			&self.lastKnownState.ProvidePaused,
		)
	}()
	if deliver {
		self.deliverProvidePaused()
	}
}

// The device's report of offline and the vpn interface while offline (reverse
// rpc).
func (self *DeviceRemote) offlineReported(offline bool, vpnInterfaceWhileOffline bool) {
	deliver := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.state.Offline.IsSet || self.state.VpnInterfaceWhileOffline.IsSet {
			// made before the device had the pending write, which the next
			// sync applies over it
			return false
		}
		self.lastKnownState.Offline.Set(offline)
		self.lastKnownState.VpnInterfaceWhileOffline.Set(vpnInterfaceWhileOffline)
		return self.settingsTold.offline.tellWithLock(deviceRemoteOffline{
			offline:                  offline,
			vpnInterfaceWhileOffline: vpnInterfaceWhileOffline,
		})
	}()
	if deliver {
		self.deliverOffline()
	}
}

// The device's report of the vpn interface while offline (reverse rpc).
func (self *DeviceRemote) vpnInterfaceWhileOfflineReported(vpnInterfaceWhileOffline bool) {
	deliver := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.settingsTold.vpnInterfaceWhileOffline.reportWithLock(
			vpnInterfaceWhileOffline,
			self.state.VpnInterfaceWhileOffline.IsSet,
			&self.lastKnownState.VpnInterfaceWhileOffline,
		)
	}()
	if deliver {
		self.deliverVpnInterfaceWhileOffline()
	}
}

// The device's report of route local (reverse rpc).
func (self *DeviceRemote) routeLocalReported(routeLocal bool) {
	deliver := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.settingsTold.routeLocal.reportWithLock(
			routeLocal,
			self.state.RouteLocal.IsSet,
			&self.lastKnownState.RouteLocal,
		)
	}()
	if deliver {
		self.deliverRouteLocal()
	}
}

// The device's report of the blocker (reverse rpc).
func (self *DeviceRemote) blockerEnabledReported(blockerEnabled bool) {
	deliver := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.settingsTold.blockerEnabled.reportWithLock(
			blockerEnabled,
			self.state.BlockerEnabled.IsSet,
			&self.lastKnownState.BlockerEnabled,
		)
	}()
	if deliver {
		self.deliverBlockerEnabled()
	}
}
