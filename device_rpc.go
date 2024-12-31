





type DeviceRemoteAddress struct {
	Ip net.IP
	Port int
	// TODO allow using messages to communicate
}



type DeviceRemote struct {

	byJwt string
	
	address *DeviceRemoteAddress



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
		// FIXME RPC connect
		if timeout {
			// FIXME timeout
			continue
		}

		// FIXME send sync message

		func() {
			if 0 < i {
				LOCK
			}
			defer UNLOCK
			self.service = X

			service.Sync(&SyncRequest{
				// FIXME set all the current local state
				// FIXME send all the subscriptions
			})
			// FIXME on success, unset all local state
		}()





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

	if service != nil {
		return service.GetStats()
	} else {
		return nil
	}
}

func (self *DeviceRemote) GetShouldShowRatingDialog() bool {
	LOCK
	defer UNLOCK

	if service != nil {
		return service.GetShouldShowRatingDialog()
	} else {
		return false
	}
}

func (self *DeviceRemote) GetCanShowRatingDialog() bool {
	LOCK
	defer UNLOCK

	if service != nil {
		return self.service.GetCanShowRatingDialog()
	} else {
		return self.canShowRatingDialog
	}
}

func (self *DeviceRemote) SetCanShowRatingDialog(canShowRatingDialog bool) {
	LOCK
	defer UNLOCK

	if service != nil {
		self.service.SetCanShowRatingDialog(canShowRatingDialog)
		self.canShowRatingDialog = Unset()
	} else {
		self.canShowRatingDialog = Set(canShowRatingDialog)
	}
}

func (self *DeviceRemote) GetProvideWhileDisconnected() bool {
	LOCK
	defer UNLOCK

	if service != nil {
		return self.service.GetProvideWhileDisconnected()
	} else {
		return self.provideWhileDisconnected
	}
}

func (self *DeviceRemote) SetProvideWhileDisconnected(provideWhileDisconnected bool) {
	LOCK
	defer UNLOCK

	if service != nil {
		self.service.SetProvideWhileDisconnected(provideWhileDisconnected)
		self.provideWhileDisconnected = Unset()
	} else {
		self.provideWhileDisconnected = Set(provideWhileDisconnected)
	}
}

func (self *DeviceRemote) GetCanRefer() bool {
	LOCK
	defer UNLOCK

	if service != nil {
		return self.service.GetCanRefer()
	} else {	
		return self.canRefer
	}
}

func (self *DeviceRemote) SetCanRefer(canRefer bool) {
	LOCK
	defer UNLOCK

	if service != nil {
		self.service.SetProvideWhileDisconnected(provideWhileDisconnected)
		self.provideWhileDisconnected = Unset()
	} else {
		self.provideWhileDisconnected = Set(provideWhileDisconnected)
	}
}

func (self *DeviceRemote) SetRouteLocal(routeLocal bool) {
	LOCK
	defer UNLOCK

	if service != nil {
		self.service.SetRouteLocal(routeLocal)
		self.routeLocal = Unset()
	} else {
		self.routeLocal = Set(routeLocal)
	}
}

func (self *DeviceRemote) GetRouteLocal() bool {
	LOCK
	defer UNLOCK

	if service != nil {
		return self.service.GetRouteLocal()
	} else {	
		return self.routeLocal
	}
}

func (self *DeviceRemote) AddProvideChangeListener(listener ProvideChangeListener) Sub {
	LOCK
	defer UNLOCK

	listenerId := NewId()
	self.provideChangeListeners[listenerId] = listener
	if service != nil {
		return self.service.AddProvideChangeListener(listenerId)
	}

	return Sub(func() {
		LOCK
		defer UNLOCK

		delete(self.provideChangeListeners, listenerId)
	})
}

func (self *DeviceRemote) AddProvidePausedChangeListener(listener ProvidePausedChangeListener) Sub {
	LOCK
	defer UNLOCK

	listenerId := NewId()
	self.providePausedChangeListeners[listenerId] = listener
	if service != nil {
		return self.service.AddProvidePausedChangeListener(listenerId)
	}

	return Sub(func() {
		LOCK
		defer UNLOCK

		delete(self.providePausedChangeListeners, listenerId)
	})
}

func (self *DeviceRemote) AddOfflineChangeListener(listener OfflineChangeListener) Sub {
	LOCK
	defer UNLOCK

	listenerId := NewId()
	self.offlineChangeListeners[listenerId] = listener
	if service != nil {
		return self.service.AddOfflineChangeListener(listenerId)
	}

	return Sub(func() {
		LOCK
		defer UNLOCK

		delete(self.offlineChangeListeners, listenerId)
	})
}

func (self *DeviceRemote) AddConnectChangeListener(listener ConnectChangeListener) Sub {
	LOCK
	defer UNLOCK

	listenerId := NewId()
	self.connectChangeListeners[listenerId] = listener
	if service != nil {
		return self.service.AddConnectChangeListener(listenerId)
	}

	return Sub(func() {
		LOCK
		defer UNLOCK

		delete(self.connectChangeListeners, listenerId)
	})
}

func (self *BringYourDevice) AddRouteLocalChangeListener(listener RouteLocalChangeListener) Sub {
	LOCK
	defer UNLOCK

	listenerId := NewId()
	self.routeLocalChangeListeners[listenerId] = listener
	if service != nil {
		return self.service.AddRouteLocalChangeListener(listenerId)
	}

	return Sub(func() {
		LOCK
		defer UNLOCK

		delete(self.routeLocalChangeListeners, listenerId)
	})
}

func (self *BringYourDevice) AddConnectLocationChangeListener(listener ConnectLocationChangeListener) Sub {
	LOCK
	defer UNLOCK

	listenerId := NewId()
	self.connectLocationChangeListeners[listenerId] = listener
	if service != nil {
		return self.service.AddConnectLocationChangeListener(listenerId)
	}

	return Sub(func() {
		LOCK
		defer UNLOCK

		delete(self.connectLocationChangeListeners, listenerId)
	})
}

func (self *BringYourDevice) AddProvideSecretKeysListener(listener ProvideSecretKeysListener) Sub {
	LOCK
	defer UNLOCK

	listenerId := NewId()
	self.provideSecretKeyListeners[listenerId] = listener
	if service != nil {
		return self.service.AddProvideSecretKeysListener(listenerId)
	}

	return Sub(func() {
		LOCK
		defer UNLOCK

		delete(self.provideSecretKeyListeners, listenerId)
	})
}

func (self *BringYourDevice) LoadProvideSecretKeys(provideSecretKeyList *ProvideSecretKeyList) {
	LOCK
	defer UNLOCK

	if service != nil {
		self.service.LoadProvideSecretKeys(provideSecretKeyList)
		self.loadProvideSecretKeyList = Unset()
	} else {	
		self.loadProvideSecretKeyList = Set(provideSecretKeyList)
	}
}

func (self *BringYourDevice) InitProvideSecretKeys() {
	LOCK
	defer UNLOCK

	if service != nil {
		self.service.InitProvideSecretKeys()
		self.initProvideSecretKeys = Unset()
	} else {	
		self.initProvideSecretKeys = Set(true)
	}
}

func (self *BringYourDevice) GetProvideEnabled() bool {
	LOCK
	defer UNLOCK

	if service != nil {
		return self.service.GetProvideEnabled()
	} else {	
		return false
	}
}

func (self *BringYourDevice) GetConnectEnabled() bool {
	LOCK
	defer UNLOCK

	if service != nil {
		return self.service.GetProvideEnabled()
	} else {	
		return false
	}
}

func (self *BringYourDevice) SetProvideMode(provideMode ProvideMode) {
	LOCK
	defer UNLOCK

	if service != nil {
		self.service.SetProvideMode(provideMode)
		self.provideMode = Unset()
	} else {	
		self.provideMode = Set(provideMode)
	}
}

func (self *BringYourDevice) GetProvideMode() ProvideMode {
	LOCK
	defer UNLOCK

	if service != nil {
		self.service.GetProvideMode()
	} else {	
		return self.provideMode
	}
}

func (self *BringYourDevice) SetProvidePaused(providePaused bool) {
	LOCK
	defer UNLOCK

	if service != nil {
		self.service.SetProvidePaused(providePaused)
		self.providePaused = Unset()
	} else {	
		self.providePaused = Set(providePaused)
	}
}

func (self *BringYourDevice) GetProvidePaused() bool {
	LOCK
	defer UNLOCK

	if service != nil {
		return self.service.GetProvidePaused()
	} else {	
		return self.providePaused
	}
}

func (self *BringYourDevice) SetOffline(offline bool) {
	LOCK
	defer UNLOCK

	if service != nil {
		self.service.SetOffline(offline)
		self.offline = Unset()
	} else {	
		self.offline = Set(offline)
	}
}

func (self *BringYourDevice) GetOffline() bool {
	LOCK
	defer UNLOCK

	if service != nil {
		return self.service.GetOffline()
	} else {	
		return self.offline
	}
}

func (self *BringYourDevice) SetVpnInterfaceWhileOffline(vpnInterfaceWhileOffline bool) {
	LOCK
	defer UNLOCK

	if service != nil {
		self.service.SetVpnInterfaceWhileOffline(vpnInterfaceWhileOffline)
		self.vpnInterfaceWhileOffline = Unset()
	} else {	
		self.vpnInterfaceWhileOffline = Set(vpnInterfaceWhileOffline)
	}
}

func (self *BringYourDevice) GetVpnInterfaceWhileOffline() bool {
	LOCK
	defer UNLOCK

	if service != nil {
		return self.service.GetVpnInterfaceWhileOffline()
	} else {	
		return self.vpnInterfaceWhileOffline
	}
}

func (self *BringYourDevice) RemoveDestination() {
	LOCK
	defer UNLOCK

	if service != nil {
		self.service.RemoveDestination()
		self.removeDestination = Unset()
	} else {	
		self.removeDestination = Set(true)
	}
}

func (self *BringYourDevice) SetDestination(destination *DeviceDestination) {
	LOCK
	defer UNLOCK

	if service != nil {
		err := self.service.SetDestination(destination)
		self.destination = Unset()
	} else {	
		self.destination = Set(destination)
	}
}

func (self *BringYourDevice) SetConnectLocation(location *ConnectLocation) {
	LOCK
	defer UNLOCK

	if service != nil {
		err := self.service.SetConnectLocation(location)
		self.location = Unset()
	} else {
		self.location = Set(location)
	}
}

func (self *BringYourDevice) GetConnectLocation() *ConnectLocation {
	LOCK
	defer UNLOCK

	if service != nil {
		return self.service.GetConnectLocation()
	} else {
		return self.location
	}
}

func (self *BringYourDevice) Shuffle() {
	LOCK
	defer UNLOCK

	if service != nil {
		self.service.Shuffle()
		self.shuffle = Unset()
	} else {
		self.shuffle = Set(true)
	}
}

func (self *BringYourDevice) SendPacket(packet []byte, n int32) bool {
	LOCK
	defer UNLOCK

	if service != nil {
		return self.service.SendPacket(packet, n)
	}
	// else drop
}

func (self *BringYourDevice) AddReceivePacket(receivePacket ReceivePacket) Sub {
	LOCK
	defer UNLOCK

	listenerId := NewId()
	self.receivePackets[listenerId] = receivePacket
	if service != nil {
		return self.service.AddReceivePacket(listenerId)
	}

	return Sub(func() {
		LOCK
		defer UNLOCK

		delete(self.receivePackets, listenerId)
	})
}

func (self *BringYourDevice) Close() {
	// FIXME close
}


func (self *BringYourDevice) GetAuthPublicKey() string {
	// FIXME
}






// attempt to connect in background
// when connected, run init sync (init keys, merge add/remove listeners, etc)
// 
// bi-directional grpc


// authentication is to sign the hello with the jwt


