package sdk




// this implements a thick client `DeviceRemote` and
// the RPC needed to coordidate with a `DeviceLocal` running in a separate process.
// the client has the following behaviors:
// |  device  |  device    |  behavior 
//    remote     local
//    active     active/
//               reachable  
// |  true    |  false     |  The device remote queues up changes to sync
//                            to the device local, when active.
//                            Listeners work as expected on queued up state.
// |  false   |  true      |  The device remote blocks until an attempt is made
//                            to connect to the device local.
//                            This avoids rapidly changing transient state on startup.
// |  true    |  true       | The device local is the source of truth for state.
//                            The device remote synchronized its queued up state
//                            on first connect and clears its queued up state.
//                            All calls to the device remote are forwarded
//                            to the device local.

// this uses `net/rpc` for simplicity, and
// the rpc is blocking on a single goproutine per peer



type DeviceRemoteValue[T any] struct {
	Value T
	Set bool
}

func Set[T any](value T) DeviceRemoteValue[T] {
	return DeviceRemoteValue{
		Value: value,
		Set: true,
	}
}

func Unset[T any]() DeviceRemoteValue[T] {
	return DeviceRemoteValue{
		Set: false,
	}
}


type DeviceRemoteListenerList[T any] = map[connect.Id, T]

func (self DeviceRemoteListenerList[T]) Get() []T {
	n := len(self)
	orderedKeys := maps.Keys(self)
	slices.Sort(orderedKeys)
	listeners := make([]T, n, n)
	for i := 0; i < n; i += 1 {
		listeners[i] = self[orderedKeys[i]]
	}
	return listeners
}



type DeviceRemoteAddress struct {
	Ip net.IP
	Port int
	// TODO allow using messages to communicate
}



type DeviceRemoteState struct {
	canShowRatingDialog DeviceRemoteValue[bool] 
	provideWhileDisconnected DeviceRemoteValue[bool]
	canRefer DeviceRemoteValue[bool]
	routeLocal DeviceRemoteValue[bool]
	initProvideSecretKeys DeviceRemoteValue[bool]
	provideMode DeviceRemoteValue[ProvideMode]
	providePaused DeviceRemoteValue[bool]
	offline DeviceRemoteValue[bool]
	vpnInterfaceWhileOffline DeviceRemoteValue[bool]
	destination DeviceRemoteValue[*DeviceDestination]
	location DeviceRemoteValue[*ConnectLocation]
	shuffle DeviceRemoteValue[bool]
}

func (self *DeviceRemoteState) Unset() {
	self.canShowRatingDialog = Unset()
	self.provideWhileDisconnected = Unset()
	self.canRefer = Unset()
	self.routeLocal = Unset()
	self.initProvideSecretKeys = Unset()
	self.provideMode = Unset()
	self.providePaused = Unset()
	self.offline = Unset()
	self.vpnInterfaceWhileOffline = Unset()
	self.destination = Unset()
	self.location = Unset()
	self.shuffle = Unset()
}



type DeviceRemote struct {

	stateLock Lock

	byJwt string
	
	address *DeviceRemoteAddress


	clientId Id
	networkSpace *NetworkSpace


	provideChangeListeners DeviceRemoteListenerList[ProvideChangeListener]
	providePausedChangeListeners DeviceRemoteListenerList[ProvidePauseChangeListener]
	offlineChangeListeners DeviceRemoteListenerList[OfflineChangeListener]
	connectChangeListeners DeviceRemoteListenerList[ConnectChangeListener]
	routeLocalChangeListeners DeviceRemoteListenerList[RouteLocalChangeListener]
	connectLocationChangeListeners DeviceRemoteListenerList[ConnectLocationChangeListener]
	provideSecretKeyListeners DeviceRemoteListenerList[ProvideSecretKeysListener]
	receivePackets DeviceRemoteListenerList[ReceivePacket]

	DeviceRemoteState
}

func newDeviceRemote() *DeviceRemote {

	// remote starts locked
	// only after the first attempt to connect to the local does it unlock
	LOCK()
	go device.run()
	return device
}

func (self *DeviceRemote) run() {


	for {
		service = XXX
		// FIXME RPC connect
		if timeout {
			// FIXME timeout
			continue
		}

		// FIXME send sync message

		err := func()(error) {
			if 0 < i {
				LOCK
			}
			defer UNLOCK
			

			syncRequest := &SyncRequest{
				// FIXME set all the current local state
				// FIXME send all the subscriptions

				ProvideChangeListeners: maps.Keys(provideChangeListeners),
				providePausedChangeListeners: maps.Keys(providePausedChangeListeners),
				offlineChangeListeners: maps.Keys(offlineChangeListeners),
				connectChangeListeners: maps.Keys(connectChangeListeners),
				routeLocalChangeListeners: maps.Keys(routeLocalChangeListeners),
				connectLocationChangeListeners: maps.Keys(connectLocationChangeListeners),
				provideSecretKeyListeners: maps.Keys(provideSecretKeyListeners),
				
				DeviceRemoteState: self.DeviceRemoteState,
			}

			// if canShowRatingDialog.Set {
			// 	syncRequest.CanShowRatingDialog: canShowRatingDialog.Value
			// }
			// if provideWhileDisconnected.Set {
			// 	syncRequest.ProvideWhileDisconnected = provideWhileDisconnected.Value
			// }
			// if canRefer.Set {
			// 	syncRequest.CanRefer = canRefer.Value
			// }
			// if routeLocal.Set {
			// 	syncRequest.RouteLocal = routeLocal.Value
			// }
			// if initProvideSecretKeys.Set {
			// 	syncRequst.initProvideSecretKeys = initProvideSecretKeys.Value
			// }
			// if provideMode.Set {
			// 	syncRequest.provideMode = provideMode.Value
			// }
			// if providePaused.Set {
			// 	syncRequest.providePaused = providePaused.Value
			// }
			// if offline.Set {
			// 	syncRequest.offline = offline.Value
			// }
			// if vpnInterfaceWhileOffline.Set {
			// 	syncRequest.vpnInterfaceWhileOffline = vpnInterfaceWhileOffline.Value
			// }
			// if destination.Set {
			// 	syncRequest.destination = destination.Value
			// }
			// if location.Set {
			// 	syncRequest.location = location.Value
			// }
			// if shuffle.Set {
			// 	syncRequest.shuffle = shuffle.Value
			// }


			syncResponse := service.Sync(syncRequest)
			// FIXME on success, unset all local state
			if syncRespinse.Success {
				canShowRatingDialog = Unset()
				provideWhileDisconnected = Unset()
				canRefer = Unset()
				routeLocal = Unset()
				initProvideSecretKeys = Unset()
				provideMode = Unset()
				providePaused = Unset()
				offline = Unset()
				vpnInterfaceWhileOffline = Unset()
				destination = Unset()
				location = Unset()
				shuffle = Unset()

				self.service = X
			}
		}()

		if err != nil {
			service.Disconnect()
			continue
		}

		// TODO Just use tcp keep alive

		// go func() {
		// 	// for {
		// 	// LOCK
		// 	// client.Ping()
		// 	// }
		// }()


		rpc := newDeviceRemoteRpc(self)
		server := rpc.NewServer()
		server.Register(rpc)
		server.ServeConn(conn)


	}
}





func (self *DeviceRemote) GetClientId() *Id {
	return self.clientId
}

func (self *DeviceRemote) GetApi() *BringYourApi {
	return self.api
}

func (self *DeviceRemote) GetNetworkSpace() *NetworkSpace {
	return self.networkSpace
}

func (self *DeviceRemote) GetStats() *DeviceStats {
	LOCK
	defer UNLOCK

	stats, success := func()(*DeviceStats, bool) {
		if service == nil {
			return nil, false
		}

		var stats *DeviceStats
		err := service.Call("DeviceLocalRpc.GetStats", nil, &stats)
		if err != nil {
			service.Close()
			return nil, false
		}
		return stats, true
	}()
	if success {
		return stats
	} else {
		return nil
	}
}

func (self *DeviceRemote) GetShouldShowRatingDialog() bool {
	LOCK
	defer UNLOCK

	shouldShowRatingDialog, success := func()(bool, bool) {
		if service == nil {
			return false, false
		}

		var shouldShowRatingDialog bool
		err := service.Call("DeviceLocalRpc.GetShouldShowRatingDialog", nil, &shouldShowRatingDialog)
		if err != nil {
			service.Close()
			return false, false
		}
		return shouldShowRatingDialog, true
	}()
	if success {
		return shouldShowRatingDialog
	} else {
		return false
	}
}

func (self *DeviceRemote) GetCanShowRatingDialog() bool {
	LOCK
	defer UNLOCK

	canShowRatingDialog, success := func()(bool, bool) {
		if service == nil {
			return false, false
		}

		var canShowRatingDialog bool
		err := service.Call("DeviceLocalRpc.GetCanShowRatingDialog", nil, &canShowRatingDialog)
		if err != nil {
			service.Close()
			return false, false
		}
		return canShowRatingDialog, true
	}()
	if success {
		return canShowRatingDialog
	} else {
		return false
	}
}

func (self *DeviceRemote) SetCanShowRatingDialog(canShowRatingDialog bool) {
	LOCK
	defer UNLOCK

	success := func()(bool) {
		if service == nil {
			return false
		}

		err := service.Call("DeviceLocalRpc.SetCanShowRatingDialog", canShowRatingDialog, nil)
		if err != nil {
			service.Close()
			return false
		}
		return true
	}
	if success {
		self.canShowRatingDialog = Unset()
	} else {
		self.canShowRatingDialog = Set(canShowRatingDialog)
	}
}

func (self *DeviceRemote) GetProvideWhileDisconnected() bool {
	LOCK
	defer UNLOCK

	provideWhileDisconnected, success := func()(bool, bool) {
		if service == nil {
			return false, false
		}

		var provideWhileDisconnected bool
		err := service.Call("DeviceLocalRpc.GetProvideWhileDisconnected", nil, &provideWhileDisconnected)
		if err != nil {
			service.Close()
			return false, false
		}
		return provideWhileDisconnected, true
	}()
	if success {
		return provideWhileDisconnected
	} else {
		return self.provideWhileDisconnected.Value
	}
}

func (self *DeviceRemote) SetProvideWhileDisconnected(provideWhileDisconnected bool) {
	LOCK
	defer UNLOCK

	success := func()(bool) {
		if service == nil {
			return false
		}

		err := service.Call("DeviceLocalRpc.SetProvideWhileDisconnected", provideWhileDisconnected, nil)
		if err != nil {
			service.Close()
			return false
		}
		return true
	}()
	if success {
		self.provideWhileDisconnected = Unset()
	} else {
		self.provideWhileDisconnected = Set(provideWhileDisconnected)
	}
}

func (self *DeviceRemote) GetCanRefer() bool {
	LOCK
	defer UNLOCK

	canRefer, success := func()(bool, bool) {
		if service == nil {
			return false, false
		}

		var canRefer bool
		err := service.Call("DeviceLocalRpc.GetCanRefer", nil, &canRefer)
		if err != nil {
			service.Close()
			return false, false
		}
		return canRefer, true
	}()
	if success {
		return canRefer
	} else {	
		return self.canRefer.Value
	}
}

func (self *DeviceRemote) SetCanRefer(canRefer bool) {
	LOCK
	defer UNLOCK

	success := func()(bool) {
		if service == nil {
			return false
		}

		err := service.Call("DeviceLocalRpc.SetProvideWhileDisconnected", canRefer, nil)
		if err != nil {
			service.Close()
			return false
		}
		return true
	}()
	if success {
		self.canRefer = Unset()
	} else {
		self.canRefer = Set(canRefer)
	}
}

func (self *DeviceRemote) SetRouteLocal(routeLocal bool) {
	event := false
	func() {
		LOCK
		defer UNLOCK

		success := func()(bool) {
			if service == nil {
				return false
			}

			err := service.Call("DeviceLocalRpc.SetRouteLocal", routeLocal, nil)
			if err != nil {
				service.Close()
				return false
			}
			return true
		}()
		if success {
			self.routeLocal = Unset()
		} else {
			self.routeLocal = Set(routeLocal)
			event = true
		}
	}
	if event {
		routeLocalChanged(GetRouteLocal())
	}
}

func (self *DeviceRemote) GetRouteLocal() bool {
	LOCK
	defer UNLOCK

	routeLocal, success := func()(bool, bool) {
		if service == nil {
			return false, false
		}

		var routeLocal bool
		err := service.Call("DeviceLocalRpc.GetRouteLocal", nil, &routeLocal)
		if err != nil {
			service.Close()
			return false, false
		}
		return routeLocal, true
	}()
	if success {
		return routeLocal
	} else {	
		return self.routeLocal.Value
	}
}

func addListener[L any](deviceRemote, listener, listeners, addServiceFunc, removeServiceFunc) Sub {
	LOCK
	defer UNLOCK

	listenerId := NewId()
	listeners[listenerId] = listener
	if deviceRemote.service != nil {
		err := deviceRemote.service.Call(addServiceFunc, listenerId, nil)
		if err != nil {
			deviceRemote.service.Close()
		}
	}

	return Sub(func() {
		LOCK
		defer UNLOCK

		delete(self.provideChangeListeners, listenerId)
		if deviceRemote.service != nil {
			err := deviceRemote.service.Call(removeServiceFunc, listenerId, nil)
			if err != nil {
				deviceRemote.service.Close()
			}
		}
	})
}

func (self *DeviceRemote) AddProvideChangeListener(listener ProvideChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.provideChangeListeners,
		"DeviceLocalRpc.AddProvideChangeListener",
		"DeviceLocalRpc.RemoveProvideChangeListener",
	)
}

func (self *DeviceRemote) AddProvidePausedChangeListener(listener ProvidePausedChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.providePausedChangeListeners,
		"DeviceLocalRpc.AddProvidePausedChangeListener",
		"DeviceLocalRpc.RemoveProvidePausedChangeListener",
	)
}

func (self *DeviceRemote) AddOfflineChangeListener(listener OfflineChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.offlineChangeListeners,
		"DeviceLocalRpc.AddOfflineChangeListener",
		"DeviceLocalRpc.RemoveOfflineChangeListener",
	)
}

func (self *DeviceRemote) AddConnectChangeListener(listener ConnectChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.connectChangeListeners,
		"DeviceLocalRpc.AddConnectChangeListener",
		"DeviceLocalRpc.RemoveConnectChangeListener",
	)
}

func (self *BringYourDevice) AddRouteLocalChangeListener(listener RouteLocalChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.routeLocalChangeListeners,
		"DeviceLocalRpc.AddRouteLocalChangeListener",
		"DeviceLocalRpc.RemoveRouteLocalChangeListener",
	)
}

func (self *BringYourDevice) AddConnectLocationChangeListener(listener ConnectLocationChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.connectLocationChangeListeners,
		"DeviceLocalRpc.AddConnectLocationChangeListener",
		"DeviceLocalRpc.RemoveConnectLocationChangeListener",
	)
}

func (self *BringYourDevice) AddProvideSecretKeysListener(listener ProvideSecretKeysListener) Sub {
	return addListener(
		self,
		listener,
		self.provideSecretKeyListeners,
		"DeviceLocalRpc.AddProvideSecretKeysListener",
		"DeviceLocalRpc.RemoveProvideSecretKeysListener",
	)
}

func (self *BringYourDevice) LoadProvideSecretKeys(provideSecretKeyList *ProvideSecretKeyList) {
	LOCK
	defer UNLOCK

	success := func()(bool) {
		if service == nil {
			return false
		}

		err := service.Call("DeviceLocalRpc.LoadProvideSecretKeys", provideSecretKeyList, nil)
		if err != nil {
			service.Close()
			return false
		}
		return true
	}()
	if success {
		self.loadProvideSecretKeyList = Unset()
	} else {	
		self.loadProvideSecretKeyList = Set(provideSecretKeyList)
		self.initProvideSecretKeys = Unset()
	}
}

func (self *BringYourDevice) InitProvideSecretKeys() {
	LOCK
	defer UNLOCK

	success := func()(bool) {
		if service == nil {
			return false
		}

		err := service.Call("DeviceLocalRpc.InitProvideSecretKeys", nil, nil)
		if err != nil {
			service.Close()
			return false
		}
		return true
	}()
	if success {
		self.initProvideSecretKeys = Unset()
	} else {	
		self.initProvideSecretKeys = Set(true)
		self.loadProvideSecretKeyList = Unset()
	}
}

func (self *BringYourDevice) GetProvideEnabled() bool {
	LOCK
	defer UNLOCK

	provideEnabled, success := func()(bool, bool) {
		if service == nil {
			return false, false
		}

		var provideEnabled bool
		err := service.Call("DeviceLocalRpc.GetProvideEnabled", nil, &provideEnabled)
		if err != nil {
			service.Close()
			return false, false
		}
		return provideEnabled, true
	}()
	if success {
		return provideEnabled
	} else {	
		return false
	}
}

func (self *BringYourDevice) GetConnectEnabled() bool {
	LOCK
	defer UNLOCK

	connectEnabled, success := func()(bool, bool) {
		if service == nil {
			return false, false
		}

		var connectEnabled bool
		err := service.Call("DeviceLocalRpc.GetConnectEnabled", nil, &connectEnabled)
		if err != nil {
			service.Close()
			return false, false
		}
		return connectEnabled, true
	}()
	if success {
		return connectEnabled
	} else {	
		return false
	}
}

func (self *BringYourDevice) SetProvideMode(provideMode ProvideMode) {
	LOCK
	defer UNLOCK

	success := func()(bool) {
		if service == nil {
			return false
		}

		err := service.Call("DeviceLocalRpc.SetProvideMode", provideMode, nil)
		if err != nil {
			service.Close()
			return false
		}
		return true
	}()
	if success {
		self.provideMode = Unset()
	} else {	
		self.provideMode = Set(provideMode)
	}
}

func (self *BringYourDevice) GetProvideMode() ProvideMode {
	LOCK
	defer UNLOCK

	provideMode, success := func()(ProvideMode, bool) {
		if service == nil {
			return false, false
		}

		var provideMode ProvideMode
		err := service.Call("DeviceLocalRpc.GetProvideMode", nil, &provideMode)
		if err != nil {
			service.Close()
			return false, false
		}
		return provideMode, true
	}()
	if success {
		return provideMode
	} else {	
		return self.provideMode.Value
	}
}

func (self *BringYourDevice) SetProvidePaused(providePaused bool) {
	event := false
	func() {
		LOCK
		defer UNLOCK

		success := func()(bool) {
			if service == nil {
				return false
			}

			err := service.Call("DeviceLocalRpc.SetProvidePaused", providePaused, nil)
			if err != nil {
				service.Close()
				return false
			}
			return true
		}()
		if success {
			self.providePaused = Unset()
		} else {	
			self.providePaused = Set(providePaused)
			event = true
		}
	}()
	if event {
		self.providePausedChanged(self.GetProvidePaused())
	}
}

func (self *BringYourDevice) GetProvidePaused() bool {
	LOCK
	defer UNLOCK

	providePaused, success := func()(bool, bool) {
		if service == nil {
			return false, false
		}

		var providePaused bool
		err := service.Call("DeviceLocalRpc.GetProvidePaused", nil, &providePaused)
		if err != nil {
			service.Close()
			return false, false
		}
		return providePaused, true
	}()
	if success {
		return providePaused
	} else {	
		return self.providePaused.Value
	}
}

func (self *BringYourDevice) SetOffline(offline bool) {
	event := false
	func() {
		LOCK
		defer UNLOCK

		success := func()(bool) {
			if service == nil {
				return false
			}

			err := service.Call("DeviceLocalRpc.SetOffline", offline, nil)
			if err != nil {
				service.Close()
				return false
			}
			return true
		}()
		if success {
			self.offline = Unset()
		} else {	
			self.offline = Set(offline)
			event = true
		}
	}()
	if event {
		self.offlineChanged(self.GetOffline(), self.GetVpnInterfaceWhileOffline())
	}
}

func (self *BringYourDevice) GetOffline() bool {
	LOCK
	defer UNLOCK

	offline, success := func()(bool, bool) {
		if service == nil {
			return false, false
		}

		var offline bool
		err := service.Call("DeviceLocalRpc.GetOffline", nil, &offline)
		if err != nil {
			service.Close()
			return false, false
		}
		return offline, true
	}()
	if success {
		return offline
	} else {	
		return self.offline.Value
	}
}

func (self *BringYourDevice) SetVpnInterfaceWhileOffline(vpnInterfaceWhileOffline bool) {
	event := false
	func() {
		LOCK
		defer UNLOCK

		success := func()(bool) {
			if service == nil {
				return false
			}

			err := service.Call("DeviceLocalRpc.SetVpnInterfaceWhileOffline", vpnInterfaceWhileOffline, nil)
			if err != nil {
				service.Close()
				return false
			}
			return true
		}()
		if success {
			self.vpnInterfaceWhileOffline = Unset()
		} else {	
			self.vpnInterfaceWhileOffline = Set(vpnInterfaceWhileOffline)
			event = true
		}
	}()
	if event {
		self.offlineChanged(self.GetOffline(), self.GetVpnInterfaceWhileOffline())
	}
}

func (self *BringYourDevice) GetVpnInterfaceWhileOffline() bool {
	LOCK
	defer UNLOCK

	vpnInterfaceWhileOffline, success := func()(bool, bool) {
		if service == nil {
			return false, false
		}

		var vpnInterfaceWhileOffline bool
		err := service.Call("DeviceLocalRpc.GetVpnInterfaceWhileOffline", nil, &vpnInterfaceWhileOffline)
		if err != nil {
			service.Close()
			return false, false
		}
		return vpnInterfaceWhileOffline, true
	}()
	if success {
		return vpnInterfaceWhileOffline
	} else {	
		return self.vpnInterfaceWhileOffline.Value
	}
}

func (self *BringYourDevice) RemoveDestination() {
	LOCK
	defer UNLOCK

	success := func()(bool) {
		if service == nil {
			return false
		}

		err := service.Call("DeviceLocalRpc.RemoveDestination", nil, nil)
		if err != nil {
			service.Close()
			return false
		}
		return true
	}()
	if success {
		self.removeDestination = Unset()
	} else {	
		self.removeDestination = Set(true)
		self.destination = Unset()
		self.location = Unset()
	}
}

func (self *BringYourDevice) SetDestination(destination *DeviceDestination) {
	LOCK
	defer UNLOCK

	success := func()(bool) {
		if service == nil {
			return false
		}

		err := service.Call("DeviceLocalRpc.SetDestination", destination, nil)
		if err != nil {
			service.Close()
			return false
		}
		return true
	}()
	if success {
		self.destination = Unset()
	} else {	
		self.destination = Set(destination)
		self.removeDestination = Unset()
		self.location = Unset()
	}
}

func (self *BringYourDevice) SetConnectLocation(location *ConnectLocation) {
	LOCK
	defer UNLOCK

	success := func()(bool) {
		if service == nil {
			return false
		}

		err := service.Call("DeviceLocalRpc.SetConnectLocation", location, nil)
		if err != nil {
			service.Close()
			return false
		}
		return true
	}()
	if success {
		self.location = Unset()
	} else {
		self.location = Set(location)
		self.remoteDestination = Unset()
		self.destination = Unset()
	}
}

func (self *BringYourDevice) GetConnectLocation() *ConnectLocation {
	LOCK
	defer UNLOCK

	location, success := func()(*ConnectLocation, bool) {
		if service == nil {
			return nil, false
		}

		var location *ConnectLocation
		err := service.Call("DeviceLocalRpc.GetConnectLocation", nil, &location)
		if err != nil {
			service.Close()
			return nil, false
		}
		return location, true
	}()
	if success {
		return location
	} else {
		return self.location.Value
	}
}

func (self *BringYourDevice) Shuffle() {
	LOCK
	defer UNLOCK

	success := func()(bool) {
		if service == nil {
			return false
		}

		err := service.Call("DeviceLocalRpc.Shuffle", nil, nil)
		if err != nil {
			service.Close()
			return false
		}
		return true
	}()
	if success {
		self.shuffle = Unset()
	} else {
		self.shuffle = Set(true)
	}
}

func (self *BringYourDevice) SendPacket(packet []byte, n int32) bool {
	return false
}

func (self *BringYourDevice) AddReceivePacket(receivePacket ReceivePacket) Sub {
	return nil
}

func (self *BringYourDevice) Close() {
	LOCK
	defer UNLOCK

	self.cancel()
}


func (self *BringYourDevice) GetAuthPublicKey() string {
	// FIXME
}



type DeviceRemoteSyncRequest struct {
	ProvideChangeListenerIds []connect.Id
	providePausedChangeListenerIds []connect.Id
	offlineChangeListenerIds []connect.Id
	connectChangeListenerIds []connect.Id
	routeLocalChangeListenerIds []connect.Id
	connectLocationChangeListenerIds []connect.Id
	provideSecretKeyListenerIds []connect.Id
	DeviceRemoteState
}


// rpc are called on a single go routine


type deviceLocalRpc struct {
	deviceLocal *DeviceLocal

	// DeviceRemoteRpc
	service rpc.Client

	provideChangeListenerIds map[connect.Id]bool
	providePausedChangeListenerIds map[connect.Id]bool
	offlineChangeListenerIds map[connect.Id]bool
	connectChangeListenerIds map[connect.Id]bool
	routeLocalChangeListenerIds map[connect.Id]bool
	connectLocationChangeIds map[connect.Id]bool
	provideSecretKeysListenerIds map[connect.Id]bool
}

func newDeviceLocalRpc(deviceLocal *DeviceLocal, service rpc.Client) *deviceLocalRpc {

}

func (self *deviceLocalRpc) Sync(syncRequest *DeviceRemoteSyncRequest, _ any) error {


	for provideChangeListenerId, _ := range self.provideChangeListenerIds {
		self.RemoveProvideChangeListener(provideChangeListenerId)
	}
	for _, provideChangeListenerId := range syncRequest.provideChangeListenerIds {
		self.AddProvideChangeListener(provideChangeListenerId)
	}
	// FIXME add all listeners


	// apply state adjustments

	if syncRequest.canShowRatingDialog.Set {
		deviceLocal.SetCanShowRatingDialog(syncRequest.canShowRatingDialog.Value)
	}
	if syncRequest.provideWhileDisconnected.Set {
		deviceLocal.SetProvideWhileDisconnected(syncRequest.provideWhileDisconnected.Value)
	}
	if syncRequest.CanRefer.Set {
		deviceLocal.SetCanRefer(syncRequest.CanRefer.Value)
	}
	if syncRequest.RouteLocal.Set {
		deviceLocal.SetRouteLocal(syncRequest.RouteLocal.Value)
	}
	if syncRequest.initProvideSecretKeys.Set {
		deviceLocal.InitProvideSecretKeys()
	}
	if syncRequest.loadProvideSecretKeys.Set {
		deviceLocal.LoadProvideSecretKeys(syncRequest.loadProvideSecretKeys.Value)
	}
	if syncRequest.ProvideMode.Set {
		deviceLocal.SetProvideMode(syncRequest.ProvideMode.Value)
	}
	if syncRequest.ProvidePaused.Set {
		deviceLocal.SetProvidePaused(syncRequest.ProvidePaused.Value)
	}
	if syncRequest.Offline.Set {
		deviceLocal.SetOffline(syncRequest.Offline.Value)
	}
	if syncRequest.vpnInterfaceWhileOffline.Set {
		deviceLocal.SetVpnInterfaceWhileOffline(syncRequest.vpnInterfaceWhileOffline.Value)
	}
	if syncRequest.removeDestination.Set {
		deviceLocal.RemoveDestination()
	}
	if syncRequest.destination.Set {
		deviceLocal.SetDestination(syncRequest.destination.Value)
	}
	if syncRequest.location.Set {
		deviceLocal.SetLocation(syncRequest.location.Value)
	}
	if syncRequest.Shuffle.Set {
		deviceLocal.Shuffle()
	}


	
	if self.provideChangeListenerSub != nil {
		self.ProvideChanged(deviceLocal.GetProvideEnabled())
	}
	// FIXME fire all listeners

}

func (self *deviceLocalRpc) GetStats(_, stats **DeviceStats) error {
	*stats = deviceLocal.GetStats()
	return nil
}

func (self *deviceLocalRpc) GetShouldShowRatingDialog(_, shouldShowRatingDialog *bool) error {
	*shouldShowRatingDialog = deviceLocal.GetShouldShowRatingDialog()
	return nil
}

func (self *deviceLocalRpc) GetCanShowRatingDialog(_, canShowRatingDialog *bool) error {
	*canShowRatingDialog = deviceLocal.GetCanShowRatingDialog()
	return nil
}

func (self *deviceLocalRpc) SetCanShowRatingDialog(canShowRatingDialog bool, _) error {
	deviceLocal.SetCanShowRatingDialog(canShowRatingDialog)
	return nil
} 

func (self *deviceLocalRpc) GetProvideWhileDisconnected(_, provideWhileDisconnected *bool) error {
	*provideWhileDisconnected = deviceLocal.GetProvideWhileDisconnected()
	return nil
}

func (self *deviceLocalRpc) SetProvideWhileDisconnected(provideWhileDisconnected bool, _) error {
	deviceLocal.SetProvideWhileDisconnected(provideWhileDisconnected)
	return nil
}

func (self *deviceLocalRpc) GetCanRefer(_, canRefer *bool) error {
	*canRefer = deviceLocal.GetCanRefer()
	return nil
}

func (self *deviceLocalRpc) SetCanRefer(canRefer bool, _) error {
	deviceLocal.SetCanRefer(canRefer)
	return nil
}

func (self *deviceLocalRpc) SetRouteLocal(routeLocal bool, _) error {
	deviceLocal.SetRouteLocal(routeLocal)
	return nil
}

func (self *deviceLocalRpc) GetRouteLocal(_, routeLocal *bool) error {
	*routeLocal = deviceLocal.GetRouteLocal()
	return nil
}



func (self *deviceLocalRpc) AddProvideChangeListener(listenerId Id, _) error {
	self.provideChangeListenerIds[listenerId] = true
	if self.provideChangeListenerSub == nil {
		self.provideChangeListenerSub = deviceLocal.AddProvideChangeListener(self)
	}
}

func (self *deviceLocalRpc) RemoveProvideChangeListener(listenerId Id, _) error {
	delete(self.provideChangeListenerIds, listenerId)
	if len(self.provideChangeListenerIds) == 0 && self.provideChangeListenerSub != nil {
		self.provideChangeListenerSub.Close()
		self.provideChangeListenerSub = nil
	}
}

// ProvideChangeListener
func (self *deviceLocalRpc) ProvideChanged(provideEnabled bool) {
	err := self.service.ProvideChanged(provideEnabled, nil)
	if err != nil {
		self.service.Close()
	}
}


func (self *deviceLocalRpc) AddProvidePausedChangeListener(listenerId Id, _) error {
	self.providePausedChangeListenerIds[listenerId] = true
	if self.providePausedChangeListenerSub == nil {
		self.providePausedChangeListenerSub = deviceLocal.AddProvidePausedChangeListener(self)
	}
}

func (self *deviceLocalRpc) RemoveProvidePausedChangeListener(listenerId Id, _) error {
	delete(self.providePausedChangeListenerIds, listenerId)
	if len(self.providePausedChangeListenerIds) == 0 && self.providePausedChangeListenerSub != nil {
		self.providePausedChangeListenerSub.Close()
		self.providePausedChangeListenerSub = nil
	}
}

// ProvidePausedChangeListener
func (self *deviceLocalRpc) ProvidePausedChanged(providePaused bool) {
	err := self.service.ProvidePausedChanged(providePaused, nil)
	if err != nil {
		self.service.Close()
	}
}


func (self *deviceLocalRpc) AddOfflineChangeListener(listenerId Id, _) error {
	self.offlineChangeListenerIds[listenerId] = true
	if self.offlineChangeListenerSub == nil {
		self.offlineChangeListenerSub = deviceLocal.AddOfflineChangeListener(self)
	}
}

func (self *deviceLocalRpc) RemoveOfflineChangeListener(listenerId Id, _) error {
	delete(self.offlineChangeListenerIds, listenerId)
	if len(self.offlineChangeListenerIds) == 0 && self.offlineChangeListenerSub != nil {
		self.offlineChangeListenerSub.Close()
		self.offlineChangeListenerSub = nil
	}
}

// OfflineChangeListener
func (self *deviceLocalRpc) OfflineChanged(offline bool, vpnInterfaceWhileOffline bool) {
	event := &DeviceRemoteOfflineChangeEvent{
		Offline: offline,
		VpnInterfaceWhileOffline: vpnInterfaceWhileOffline,
	}
	err := self.service.OfflineChanged(event, nil)
	if err != nil {
		self.service.Close()
	}
}


func (self *deviceLocalRpc) AddConnectChangeListener(listenerId Id, _) error {
	self.connectChangeListenerIds[listenerId] = true
	if self.connectChangeListenerSub == nil {
		self.connectChangeListenerSub = deviceLocal.AddConnectChangeListener(self)
	}
}

func (self *deviceLocalRpc) RemoveConnectChangeListener(listenerId Id, _) error {
	delete(self.connectChangeListenerIds, listenerId)
	if len(self.connectChangeListenerIds) == 0 && self.connectChangeListenerSub != nil {
		self.connectChangeListenerSub.Close()
		self.connectChangeListenerSub = nil
	}
}

// ConnectChangeListener
func (self *deviceLocalRpc) ConnectChanged(connectEnabled bool) {
	err := self.service.ConnectChanged(connectEnabled, nil)
	if err != nil {
		self.service.Close()
	}
}


func (self *deviceLocalRpc) AddRouteLocalChangeListener(listenerId Id, _) error {
	self.routeLocalChangeListenerIds[listenerId] = true
	if self.routeLocalChangeListenerSub == nil {
		self.routeLocalChangeListenerSub = deviceLocal.AddRouteLocalChangeListener(self)
	}
}

func (self *deviceLocalRpc) RemoveRouteLocalChangeListener(listenerId Id, _) error {
	delete(self.routeLocalChangeListenerIds, listenerId)
	if len(self.routeLocalChangeListenerIds) == 0 && self.routeLocalChangeListenerSub != nil {
		self.routeLocalChangeListenerSub.Close()
		self.routeLocalChangeListenerSub = nil
	}
}

// RouteLocalChangeListener
func (self *deviceLocalRpc) RouteLocalChanged(routeLocal bool) {
	err := self.service.RouteLocalChanged(routeLocal, nil)
	if err != nil {
		self.service.Close()
	}
}



func (self *deviceLocalRpc) AddConnectLocationChangeListener(listenerId Id, _) error {
	self.connectLocationChangeIds[listenerId] = true
	if self.connectLocationChangeSub == nil {
		self.connectLocationChangeSub = deviceLocal.AddConnectLocationChangeListener(self)
	}
}

func (self *deviceLocalRpc) RemoveConnectLocationChangeListener(listenerId Id, _) error {
	delete(self.connectLocationChangeIds, listenerId)
	if len(self.connectLocationChangeIds) == 0 && self.connectLocationChangeSub != nil {
		self.connectLocationChangeSub.Close()
		self.connectLocationChangeSub = nil
	}
}

// ConnectLocationChangeListener
func (self *deviceLocalRpc) ConnectLocationChanged(location *ConnectLocation) {
	err := self.service.ConnectLocationChanged(location, nil)
	if err != nil {
		self.service.Close()
	}
}


func (self *deviceLocalRpc) AddProvideSecretKeysListener(listenerId Id, _) error {
	self.provideSecretKeysListenerIds[listenerId] = true
	if self.provideSecretKeysListenerSub == nil {
		self.provideSecretKeysListenerSub = deviceLocal.AddProvideSecretKeysListener(self)
	}
}

func (self *deviceLocalRpc) RemoveProvideSecretKeysListener(listenerId Id, _) error {
	delete(self.provideSecretKeysListenerIds, listenerId)
	if len(self.provideSecretKeysListenerIds) == 0 && self.provideSecretKeysListenerSub != nil {
		self.provideSecretKeysListenerSub.Close()
		self.provideSecretKeysListenerSub = nil
	}
}

// ProvideSecretKeysListener
func (self *deviceLocalRpc) ProvideSecretKeysChanged(provideSecretKeyList *ProvideSecretKeyList) {
	err := self.service.ProvideSecretKeysChanged(provideSecretKeyList, nil)
	if err != nil {
		self.service.Close()
	}
}


func (self *deviceLocalRpc) LoadProvideSecretKeys(provideSecretKeyList *ProvideSecretKeyList, _) error {
	deviceLocal.LoadProvideSecretKeys(provideSecretKeyList)
	return nil
}

func (self *deviceLocalRpc) InitProvideSecretKeys(_, _) error {
	deviceLocal.InitProvideSecretKeys()
	return nil
}

func (self *deviceLocalRpc) GetProvideEnabled(_, provideEnabled *bool) error {
	*provideEnabled = deviceLocal.GetProvideEnabled()
	return nil
}

func (self *deviceLocalRpc) GetConnectEnabled(_, connectEnabled *bool) error {
	*connectEnabled = deviceLocal.GetConnectEnabled()
	return nil
}

func (self *deviceLocalRpc) SetProvideMode(provideMode ProvideMode, _) error {
	deviceLocal.SetProvideMod(provideMode)
	return nil
} 

func (self *deviceLocalRpc) GetProvideMode(_, provideMode *ProvideMode) error {
	*provideMode = deviceLocal.GetProvideMode()
	return nil
}

func (self *deviceLocalRpc) SetProvidePaused(providePaused bool, _) error {
	deviceLocal.SetProvidePaused(providePaused)
	return nil
} 

func (self *deviceLocalRpc) GetProvidePaused(_, providePaused *bool) error {
	*providePaused = deviceLocal.GetProvidePaused()
	return nil
}

func (self *deviceLocalRpc) SetOffline(offline bool, _) error {
	deviceLocal.SetOffline(offline)
	return nil
}

func (self *deviceLocalRpc) GetOffline(_, offline *bool) error {
	*offline = deviceLocal.GetOffline()
	return nil
}

func (self *deviceLocalRpc) SetVpnInterfaceWhileOffline(vpnInterfaceWhileOffline bool, _ any) error {
	deviceLocal.SetVpnInterfaceWhileOffline(vpnInterfaceWhileOffline)
	return nil
}

func (self *deviceLocalRpc) GetVpnInterfaceWhileOffline(_ any, vpnInterfaceWhileOffline *bool) error {
	*vpnInterfaceWhileOffline = deviceLocal.GetVpnInterfaceWhileOffline()
	return nil
}

func (self *deviceLocalRpc) RemoveDestination(_ any, _ any) error {
	deviceLocal.RemoveDestination()
	return nil
}

func (self *deviceLocalRpc) SetDestination(destination *DeviceDestination, _ any) error {
	deviceLocal.SetDestination(destination)
	return nil
}

func (self *deviceLocalRpc) SetConnectLocation(location *ConnectLocation, _ any) error {
	deviceLocal.SetConnectLocation(location)
	return nil
} 

func (self *deviceLocalRpc) GetConnectLocation(_ any, location *ConnectLocation) error {
	*location = deviceLocal.GetConnectLocation()
	return nil
}  

func (self *deviceLocalRpc) Shuffle(_ any, _ any) error {
	deviceLocal.Shuffle()
	return nil
}

func (self *deviceLocalRpc) Close() {
	service.Close()
}




type deviceRemoteRpc struct {
	deviceRemote *DeviceRemote
}

func newDeviceRemoteRpc(deviceRemote *DeviceRemote) *deviceRemoteRpc {
	return &deviceRemoteRpc{
		deviceRemote: deviceRemote,
	}
}

func (self *deviceRemoteRpc) ProvideChanged(provideEnabled bool, _ any) error {
	deviceRemote.provideChanged(provideEnabled)
	return nil
}

func (self *deviceRemoteRpc) ProvidePausedChanged(providePaused bool, _ any) error {
	deviceRemote.providePausedChanged(providePaused)
	return nil
}

func (self *deviceRemoteRpc) OfflineChanged(event *DeviceRemoteOfflineChangeEvent, _ any) error {
	deviceRemote.offlineChanged(event.Offline, event.VpnInterfaceWhileOffline)
	return nil
}

func (self *deviceRemoteRpc) ConnectChanged(connectEnabled bool, _ any) error {
	deviceRemote.connectChanged(event.Offline, event.VpnInterfaceWhileOffline)
	return nil
}

func (self *deviceRemoteRpc) RouteLocalChanged(routeLocal bool, _ any) error {
	deviceRemote.routeLocalChanged(routeLocal)
	return nil
}

func (self *deviceRemoteRpc) ConnectLocationChanged(location *ConnectLocation, _ any) error {
	deviceRemote.connectLocationChanged(location)
	return nil
}

func (self *deviceRemoteRpc) ProvideSecretKeysChanged(provideSecretKeyList *ProvideSecretKeyList, _ any) error {
	deviceRemote.provideSecretKeysChanged(provideSecretKeyList)
	return nil
}

func (self *deviceRemoteRpc) Close() {

}










// attempt to connect in background
// when connected, run init sync (init keys, merge add/remove listeners, etc)
// 
// bi-directional grpc


// authentication is to sign the hello with the jwt


