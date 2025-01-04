package sdk

import (
	"net/netip"
	"net/rpc"

)

// On some platforms the device needs to run in a separate process,
// separate from the app. In this scenario we still want to code the app
// against the `Device` implementation and have it work as if the device was local.

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

// this uses `net/rpc` for simplicity, with tls auth, and
// the rpc is blocking on a single goproutine per peer


type DeviceRpcSettings struct {
	RpcConnectTimeout time.Duration
	RpcReconnectTimeout time.Duration
	// TODO randomize the ports
	Address *DeviceRemoteAddress
	ResponseAddress *DeviceRemoteAddress
}

func DefaultDeviceRpcSettings() *DeviceRpcSettings {
	return &DeviceRemoteSettings{
		RpcConnectTimeout: 10 * time.Second,
		RpcReconnectTimeout: 1 * time.Second, 
		Address: &DeviceRemoteAddress{
			"127.0.0.1",
			12025,
		},
		ResponseAddress: &DeviceRemoteAddress{
			"127.0.0.1",
			12026,
		}
	}
}


type DeviceRemote struct {
	ctx context.Context
	cancel context.CancelFunc

	networkSpace *NetworkSpace
	byJwt string

	settings *DeviceRpcSettings

	reconnectMonitor *connect.Monitor
	
	clientId connect.Id

	stateLock sync.Mutex

	service rpc.Client

	provideChangeListeners DeviceRemoteListenerList[ProvideChangeListener]
	providePausedChangeListeners DeviceRemoteListenerList[ProvidePauseChangeListener]
	offlineChangeListeners DeviceRemoteListenerList[OfflineChangeListener]
	connectChangeListeners DeviceRemoteListenerList[ConnectChangeListener]
	routeLocalChangeListeners DeviceRemoteListenerList[RouteLocalChangeListener]
	connectLocationChangeListeners DeviceRemoteListenerList[ConnectLocationChangeListener]
	provideSecretKeyListeners DeviceRemoteListenerList[ProvideSecretKeysListener]

	state DeviceRemoteState
}

func NewDeviceRemoteWithDefaults(
	networkSpace *NetworkSpace,
	byJwt string,
) (*DeviceRemote, error) {
	return NewDeviceRemote(networkSpace, byJwt, DefaultDeviceRpcSettings())
}

func NewDeviceRemote(
	networkSpace *NetworkSpace,
	byJwt string,
	settings *DeviceRpcSettings,
) (*DeviceRemote, error) {
	clientId, err := parseByJwtClientId(byJwt)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())

	device := &DeviceRemote{
		ctx: ctx,
		cancel: cancel,
		networkSpace: networkSpace,
		byJwt: byJwt,
		address: address,
		settings: settings,
		reconnectMonitor: connect.NewMonitor(),
		clientId: clientId,
	}

	// remote starts locked
	// only after the first attempt to connect to the local does it unlock
	device.stateLock.Lock()
	go device.run()
	return device, nil
}

func (self *DeviceRemote) run() {
	for {
		handleCtx, handleCancel := context.WithCancel(self.ctx)

		reconnect := reconnectMonitor.Context()
		func() {
			defer handleCancel()

			if i == 0 {
				defer self.stateLock.Unlock()
			}

			dialer := net.Dialer{
				Timeout: self.settings.RpcConnectTimeout,
				KeepAliveConfig: net.KeepAliveConfig{
					Enable: true,
				},
			}
			conn, err := dialer.DialContext(
				handleCtx,
				"tcp",
				net.JoinHostPort(self.address.Ip.String(), fmt.Sprintf("%d", self.address.Port)),
			)
			if err != nil {
				return
			}
			// FIXME
			// tls.Handshake()

			service := rpc.NewClient(conn)
			defer service.Close()
			
			syncRequest := &SyncRequest{
				ProvideChangeListeners: maps.Keys(provideChangeListeners),
				providePausedChangeListeners: maps.Keys(providePausedChangeListeners),
				offlineChangeListeners: maps.Keys(offlineChangeListeners),
				connectChangeListeners: maps.Keys(connectChangeListeners),
				routeLocalChangeListeners: maps.Keys(routeLocalChangeListeners),
				connectLocationChangeListeners: maps.Keys(connectLocationChangeListeners),
				provideSecretKeyListeners: maps.Keys(provideSecretKeyListeners),
				DeviceRemoteState: self.DeviceRemoteState,
			}
			var response *DeviceRemoteSyncResponse
			err := service.Sync(syncRequest, &SyncResponse)
			if err != nil {
				return
			}


			// FIXME use response cert to listen with TLS
			listenConfig := &ListenConfig{
				KeepAliveConfig: net.KeepAliveConfig{
					Enable: true,
				},
			}
			responseListener, err := listenConfig.Listen(handleCtx, "tcp", responseAddress)
			if err != nil {
				return
			}
			defer responseListener.Close()

			deviceRemoteRpc := newDeviceRemoteRpc(self)
			server := rpc.NewServer()
			server.Register(deviceRemoteRpc)

			defer deviceRemoteRpc.Close()
			defer server.Close()

			go func() {
				defer handleCancel()

				// handle connections serially
				for {
					conn, err := responseListener.Accept()
					if err != nil {
						return
					}
					server.ServerConn(conn)
				}
			}()


			err := service.ConnectResponse(self.responseAddress, nil)
			if err != nil {
				return
			}


			func() {
				if  0 < i {
					self.stateLock.Lock()
					defer self.stateLock.Unlock()
				}

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

				self.service = service
			}()

			defer func() {
				if  0 < i {
					self.stateLock.Lock()
					defer self.stateLock.Unlock()
				}

				if self.service == service {
					self.service = nil
				}
			}()

			select {
			case <- ctx.Done():
			case <- handleCtx.Done():
			}
		}()

		select {
		case <- ctx.Done():
			return
		case <- time.After(self.settings.RpcReconnectTimeout):
		case <- reconnect:
			// reconnect now
		}
	}
}

func (self *DeviceRemote) GetRpcPublicKey() string {
	// FIXME
	return ""
}

// force a connect attempt as soon as possible
// note this just speeds up connection but is not required,
// since the rpc connect will poll until connected
func (self *DeviceRemote) Connect() {
	reconnectMonitor.NotifyAll()
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
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

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
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

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
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

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
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

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
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

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
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

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
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	canRefer, success := func()(bool, bool) {
		if service == nil {
			return false, false
		}

		var canRefer bool
		err := service.Call("deviceLocalRpc.GetCanRefer", nil, &canRefer)
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
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	success := func()(bool) {
		if service == nil {
			return false
		}

		err := service.Call("deviceLocalRpc.SetProvideWhileDisconnected", canRefer, nil)
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
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		success := func()(bool) {
			if service == nil {
				return false
			}

			err := service.Call("deviceLocalRpc.SetRouteLocal", routeLocal, nil)
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
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	routeLocal, success := func()(bool, bool) {
		if service == nil {
			return false, false
		}

		var routeLocal bool
		err := service.Call("deviceLocalRpc.GetRouteLocal", nil, &routeLocal)
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
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	listenerId := NewId()
	listeners[listenerId] = listener
	if deviceRemote.service != nil {
		err := deviceRemote.service.Call(addServiceFunc, listenerId, nil)
		if err != nil {
			deviceRemote.service.Close()
		}
	}

	return Sub(func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

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
		"deviceLocalRpc.AddProvideChangeListener",
		"deviceLocalRpc.RemoveProvideChangeListener",
	)
}

func (self *DeviceRemote) AddProvidePausedChangeListener(listener ProvidePausedChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.providePausedChangeListeners,
		"deviceLocalRpc.AddProvidePausedChangeListener",
		"deviceLocalRpc.RemoveProvidePausedChangeListener",
	)
}

func (self *DeviceRemote) AddOfflineChangeListener(listener OfflineChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.offlineChangeListeners,
		"deviceLocalRpc.AddOfflineChangeListener",
		"deviceLocalRpc.RemoveOfflineChangeListener",
	)
}

func (self *DeviceRemote) AddConnectChangeListener(listener ConnectChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.connectChangeListeners,
		"deviceLocalRpc.AddConnectChangeListener",
		"deviceLocalRpc.RemoveConnectChangeListener",
	)
}

func (self *DeviceRemote) AddRouteLocalChangeListener(listener RouteLocalChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.routeLocalChangeListeners,
		"deviceLocalRpc.AddRouteLocalChangeListener",
		"deviceLocalRpc.RemoveRouteLocalChangeListener",
	)
}

func (self *DeviceRemote) AddConnectLocationChangeListener(listener ConnectLocationChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.connectLocationChangeListeners,
		"deviceLocalRpc.AddConnectLocationChangeListener",
		"deviceLocalRpc.RemoveConnectLocationChangeListener",
	)
}

func (self *DeviceRemote) AddProvideSecretKeysListener(listener ProvideSecretKeysListener) Sub {
	return addListener(
		self,
		listener,
		self.provideSecretKeyListeners,
		"deviceLocalRpc.AddProvideSecretKeysListener",
		"deviceLocalRpc.RemoveProvideSecretKeysListener",
	)
}

func (self *DeviceRemote) LoadProvideSecretKeys(provideSecretKeyList *ProvideSecretKeyList) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	success := func()(bool) {
		if service == nil {
			return false
		}

		err := service.Call("deviceLocalRpc.LoadProvideSecretKeys", provideSecretKeyList, nil)
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

func (self *DeviceRemote) InitProvideSecretKeys() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	success := func()(bool) {
		if service == nil {
			return false
		}

		err := service.Call("deviceLocalRpc.InitProvideSecretKeys", nil, nil)
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

func (self *DeviceRemote) GetProvideEnabled() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	provideEnabled, success := func()(bool, bool) {
		if service == nil {
			return false, false
		}

		var provideEnabled bool
		err := service.Call("deviceLocalRpc.GetProvideEnabled", nil, &provideEnabled)
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

func (self *DeviceRemote) GetConnectEnabled() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	connectEnabled, success := func()(bool, bool) {
		if service == nil {
			return false, false
		}

		var connectEnabled bool
		err := service.Call("deviceLocalRpc.GetConnectEnabled", nil, &connectEnabled)
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

func (self *DeviceRemote) SetProvideMode(provideMode ProvideMode) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	success := func()(bool) {
		if service == nil {
			return false
		}

		err := service.Call("deviceLocalRpc.SetProvideMode", provideMode, nil)
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

func (self *DeviceRemote) GetProvideMode() ProvideMode {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	provideMode, success := func()(ProvideMode, bool) {
		if service == nil {
			return false, false
		}

		var provideMode ProvideMode
		err := service.Call("deviceLocalRpc.GetProvideMode", nil, &provideMode)
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

func (self *DeviceRemote) SetProvidePaused(providePaused bool) {
	event := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		success := func()(bool) {
			if service == nil {
				return false
			}

			err := service.Call("deviceLocalRpc.SetProvidePaused", providePaused, nil)
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

func (self *DeviceRemote) GetProvidePaused() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	providePaused, success := func()(bool, bool) {
		if service == nil {
			return false, false
		}

		var providePaused bool
		err := service.Call("deviceLocalRpc.GetProvidePaused", nil, &providePaused)
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

func (self *DeviceRemote) SetOffline(offline bool) {
	event := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		success := func()(bool) {
			if service == nil {
				return false
			}

			err := service.Call("deviceLocalRpc.SetOffline", offline, nil)
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

func (self *DeviceRemote) GetOffline() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	offline, success := func()(bool, bool) {
		if service == nil {
			return false, false
		}

		var offline bool
		err := service.Call("deviceLocalRpc.GetOffline", nil, &offline)
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

func (self *DeviceRemote) SetVpnInterfaceWhileOffline(vpnInterfaceWhileOffline bool) {
	event := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		success := func()(bool) {
			if service == nil {
				return false
			}

			err := service.Call("deviceLocalRpc.SetVpnInterfaceWhileOffline", vpnInterfaceWhileOffline, nil)
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

func (self *DeviceRemote) GetVpnInterfaceWhileOffline() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	vpnInterfaceWhileOffline, success := func()(bool, bool) {
		if service == nil {
			return false, false
		}

		var vpnInterfaceWhileOffline bool
		err := service.Call("deviceLocalRpc.GetVpnInterfaceWhileOffline", nil, &vpnInterfaceWhileOffline)
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

func (self *DeviceRemote) RemoveDestination() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	success := func()(bool) {
		if service == nil {
			return false
		}

		err := service.Call("deviceLocalRpc.RemoveDestination", nil, nil)
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

func (self *DeviceRemote) SetDestination(destination *DeviceDestination) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	success := func()(bool) {
		if service == nil {
			return false
		}

		err := service.Call("deviceLocalRpc.SetDestination", destination, nil)
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

func (self *DeviceRemote) SetConnectLocation(location *ConnectLocation) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	success := func()(bool) {
		if service == nil {
			return false
		}

		err := service.Call("deviceLocalRpc.SetConnectLocation", location, nil)
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

func (self *DeviceRemote) GetConnectLocation() *ConnectLocation {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	location, success := func()(*ConnectLocation, bool) {
		if service == nil {
			return nil, false
		}

		var location *ConnectLocation
		err := service.Call("deviceLocalRpc.GetConnectLocation", nil, &location)
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

func (self *DeviceRemote) Shuffle() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	success := func()(bool) {
		if service == nil {
			return false
		}

		err := service.Call("deviceLocalRpc.Shuffle", nil, nil)
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

func (self *DeviceRemote) SendPacket(packet []byte, n int32) bool {
	return false
}

func (self *DeviceRemote) AddReceivePacket(receivePacket ReceivePacket) Sub {
	return nil
}

func (self *DeviceRemote) Close() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.cancel()
}

// event dispatch

func [T any]listenerList(lock sync.Mutex, listenerMap DeviceRemoteListenerMap[T]) []T {
	lock.Lock()
	defer lock.Unlock()

	// consistent dispatch order
	n := len(listenerMap)
	orderedKeys := maps.Keys(listenerMap)
	slices.Sort(orderedKeys)
	listeners := make([]T, n, n)
	for i := 0; i < n; i += 1 {
		listeners[i] = listenerMap[orderedKeys[i]]
	}
	return listeners
}

func (self *DeviceRemote) provideChanged(provideEnabled bool) {
	for _, provideChangeListener := range listenerList(self.stateLock, self.provideChangeListeners) {
		provideChangeListener.ProvideChanged(provideEnabled)
	}
}

func (self *DeviceRemote) providePausedChanged(providePaused bool) {
	for _, providePausedChangeListener := range listenerList(self.stateLock, self.providePausedChangeListeners) {
		providePausedChangeListener.ProvidePausedChanged(providePaused)
	}
}

func (self *DeviceRemote) offlineChanged(offline bool, vpnInterfaceWhileOffline bool) {
	for _, offlineChangeListener := range listenerList(self.stateLock, self.offlineChangeListeners) {
		offlineChangeListener.OfflineChanged(offline, vpnInterfaceWhileOffline)
	}
}

func (self *DeviceRemote) connectChanged(connectEnabled bool) {
	for _, connectChangeListeners := range listenerList(self.stateLock, self.connectChangeListeners) {
		connectChangeListeners.ConnectChanged(connectEnabled)
	}
}

func (self *DeviceRemote) routeLocalChanged(routeLocal bool) {
	for _, routeLocalChangeListener := range listenerList(self.stateLock, self.routeLocalChangeListeners) {
		connectChangeListeners.RouteLocalChanged(routeLocal)
	}
}

func (self *DeviceRemote) connectLocationChanged(location *ConnectLocation) {
	for _, connectLocationChangeListener := range listenerList(self.stateLock, self.connectLocationChangeListeners) {
		connectLocationChangeListener.ConnectLocationChanged(location)
	}
}

func (self *DeviceRemote) provideSecretKeysChanged(provideSecretKeyList *ProvideSecretKeyList) {
	for _, provideSecretKeyListener := range listenerList(self.stateLock, self.provideSecretKeyListeners) {
		provideSecretKeyListener.ProvideSecretKeysChanged(provideSecretKeyList)
	}
}


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


type DeviceRemoteListenerMap[T any] = map[connect.Id, T]


type DeviceRemoteAddress struct {
	Ip netip.Addr
	Port int
}

type DeviceRemoteState struct {
	CanShowRatingDialog DeviceRemoteValue[bool] 
	ProvideWhileDisconnected DeviceRemoteValue[bool]
	CanRefer DeviceRemoteValue[bool]
	RouteLocal DeviceRemoteValue[bool]
	InitProvideSecretKeys DeviceRemoteValue[bool]
	ProvideMode DeviceRemoteValue[ProvideMode]
	ProvidePaused DeviceRemoteValue[bool]
	Offline DeviceRemoteValue[bool]
	VpnInterfaceWhileOffline DeviceRemoteValue[bool]
	Destination DeviceRemoteValue[*DeviceDestination]
	Location DeviceRemoteValue[*ConnectLocation]
	Shuffle DeviceRemoteValue[bool]
}

func (self *DeviceRemoteState) Unset() {
	self.CanShowRatingDialog = Unset()
	self.ProvideWhileDisconnected = Unset()
	self.CanRefer = Unset()
	self.RouteLocal = Unset()
	self.InitProvideSecretKeys = Unset()
	self.ProvideMode = Unset()
	self.ProvidePaused = Unset()
	self.Offline = Unset()
	self.VpnInterfaceWhileOffline = Unset()
	self.Destination = Unset()
	self.Location = Unset()
	self.Shuffle = Unset()
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

type DeviceRemoteSyncResponse struct {
	// FIXME response cert
}

type DeviceRemoteConnectBack struct {
	ResponseAddress DeviceRemoteAddress
}


// rpc are called on a single go routine


type deviceLocalRpc struct {
	ctx context.Context
	cancel context.CancelFunc

	deviceLocal *DeviceLocal

	provideChangeListenerIds map[connect.Id]bool
	providePausedChangeListenerIds map[connect.Id]bool
	offlineChangeListenerIds map[connect.Id]bool
	connectChangeListenerIds map[connect.Id]bool
	routeLocalChangeListenerIds map[connect.Id]bool
	connectLocationChangeListenerIds map[connect.Id]bool
	provideSecretKeysListenerIds map[connect.Id]bool

	provideChangeListenerSub Sub
	providePausedChangeListenerSub Sub
	offlineChangeListenerSub Sub
	connectChangeListenerSub Sub
	routeLocalChangeListenerSub Sub
	connectLocationChangeListenerSub Sub
	provideSecretKeysListenerSub Sub

	service *rpc.Client
}

func newDeviceLocalRpcWithDefaults(
	ctx context.Context,
	deviceLocal *DeviceLocal
) *deviceLocalRpc {
	return newDeviceLocalRpc(ctx, deviceLocal, DefaultDeviceRpcSettings())
}

func newDeviceLocalRpc(ctx context.Context, deviceLocal *DeviceLocal, settings *DeviceRpcSettings) *deviceLocalRpc {
	cancelCtx, cancel := context.WithCancel(ctx)

	deviceLocalRpc := &deviceLocalRpc{
		ctx: cancelCtx,
		cancel: cancel,
		deviceLocal: deviceLocal,
		settings: settings,
		provideChangeListenerIds: map[connect.Id]bool{},
		providePausedChangeListenerIds: map[connect.Id]bool{},
		offlineChangeListenerIds: map[connect.Id]bool{},
		connectChangeListenerIds: map[connect.Id]bool{},
		routeLocalChangeListenerIds: map[connect.Id]bool{},
		connectLocationChangeListenerIds: map[connect.Id]bool{},
		provideSecretKeysListenerIds: map[connect.Id]bool{},
	}

	go deviceLocalRpc.run()
	return deviceLocalRpc
}

func (self *deviceLocalRpc) run() {
	for {
		handleCtx, handleCancel := context.WithCancel(self.ctx)

		func() {
			defer handleCancel()

			listenConfig := &ListenConfig{
				KeepAliveConfig: net.KeepAliveConfig{
					Enable: true,
				},
			}
			listener, err := listenConfig.Listen(handleCtx, "tcp", address)
			if err != nil {
				return
			}

			defer listener.Close()

			server := rpc.NewServer()
			server.Register(self)

			defer server.Close()

			go func() {
				defer handleCancel()

				// handle connections serially
				for {
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					server.ServerConn(conn)
				}
			}()

			select {
			case <- handleCtx.Done():
			}
		}()

		select {
		case <- self.ctx.Done():
			return
		case <- time.After(self.settings.RpcReconnectTimeout):
		}
	}
}

func (self *deviceLocalRpc) Sync(syncRequest *DeviceRemoteSyncRequest, syncResponse **DeviceRemoteSyncResponse) error {

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


	// add listeners

	for provideChangeListenerId, _ := range self.provideChangeListenerIds {
		self.RemoveProvideChangeListener(provideChangeListenerId)
	}
	for _, provideChangeListenerId := range syncRequest.provideChangeListenerIds {
		self.AddProvideChangeListener(provideChangeListenerId)
	}

	for providePausedChangeListenerId, _ := range self.providePausedChangeListenerIds {
		self.RemoveProvidePausedChangeListener(providePausedChangeListenerId)
	}
	for _, providePausedChangeListenerId := range syncRequest.providePausedChangeListenerIds {
		self.AddProvidePausedChangeListener(providePausedChangeListenerId)
	}

	for offlineChangeListenerId, _ := range self.offlineChangeListenerIds {
		self.RemoveOfflineChangeListener(offlineChangeListenerId)
	}
	for _, offlineChangeListenerId := range syncRequest.offlineChangeListenerIds {
		self.AddOfflineChangeListener(offlineChangeListenerId)
	}

	for connectChangeListenerId, _ := range self.connectChangeListenerIds {
		self.RemoveConnectChangeListenerId(connectChangeListenerId)
	}
	for _, connectChangeListenerId := range syncRequest.connectChangeListenerIds {
		self.AddConnectChangeListener(connectChangeListenerId)
	}

	for routeLocalChangeListenerId, _ := range self.routeLocalChangeListenerIds {
		self.RemoveRouteLocalChangeListener(routeLocalChangeListenerId)
	}
	for _, routeLocalChangeListenerId := range syncRequest.routeLocalChangeListenerIds {
		self.AddRouteLocalChangeListener(routeLocalChangeListenerId)
	}

	for connectLocationChangeListenerId, _ := range self.connectLocationChangeListenerIds {
		self.RemoveConnectLocationChangeListener(connectLocationChangeListenerId)
	}
	for _, connectLocationChangeListenerId := range syncRequest.connectLocationChangeListenerIds {
		self.AddConnectLocationChangeListener(connectLocationChangeListenerId)
	}

	for provideSecretKeysListenerId, _ := range self.provideSecretKeysListenerIds {
		self.RemoveProvideSecretKeysListener(provideSecretKeysListenerId)
	}
	for _, provideSecretKeysListenerId := range syncRequest.provideSecretKeysListenerIds {
		self.AddProvideSecretKeysListener(provideSecretKeysListenerId)
	}


	// fire listeners with the current state
	
	if self.provideChangeListenerSub != nil {
		self.ProvideChanged(deviceLocal.GetProvideEnabled())
	}
	if self.providePausedChangeListenerSub != nil {
		self.ProvidePausedChanged(deviceLocal.GetProvidePaused())
	}
	if self.offlineChangeListenerSub != nil {
		self.OfflineChanged(deviceLocal.GetOffline())
	}
	if self.connectChangeListenerSub != nil {
		self.ConnectChanged(deviceLocal.GetConnectEnabled())
	}
	if self.routeLocalChangeListenerSub != nil {
		self.RouteLocalChanged(deviceLocal.GetRouteLocal())
	}
	if self.connectLocationChangeListenerSub != nil {
		self.ConnectLocationChanged(deviceLocal.GetConnectLocation())
	}
	if self.connectLocationChangeListenerSub != nil {
		self.ConnectLocationChanged(deviceLocal.GetConnectLocation())
	}
	if self.provideSecretKeysListenerSub != nil {
		self.ProvideSecretKeysChanged(deviceLocal.GetProvideSecretKeys())
	}

}

func (self *deviceLocalRpc) ConnectResponse(responseAddress *DeviceRemoteAddress, _ any) error {
	dialer := net.Dialer{
		Timeout: self.settings.RpcConnectTimeout,
		KeepAliveConfig: net.KeepAliveConfig{
			Enable: true,
		},
	}
	conn, err := dialer.DialContext(
		handleCtx,
		"tcp",
		net.JoinHostPort(responseAddress.Ip.String(), fmt.Sprintf("%d", responseAddress.Port)),
	)
	if err != nil {
		return
	}
	// FIXME
	// tls.Handshake()

	service := rpc.NewClient(conn)
}

func (self *deviceLocalRpc) GetStats(_ any, stats **DeviceStats) error {
	*stats = deviceLocal.GetStats()
	return nil
}

func (self *deviceLocalRpc) GetShouldShowRatingDialog(_ any, shouldShowRatingDialog *bool) error {
	*shouldShowRatingDialog = deviceLocal.GetShouldShowRatingDialog()
	return nil
}

func (self *deviceLocalRpc) GetCanShowRatingDialog(_ any, canShowRatingDialog *bool) error {
	*canShowRatingDialog = deviceLocal.GetCanShowRatingDialog()
	return nil
}

func (self *deviceLocalRpc) SetCanShowRatingDialog(canShowRatingDialog bool, _ any) error {
	deviceLocal.SetCanShowRatingDialog(canShowRatingDialog)
	return nil
} 

func (self *deviceLocalRpc) GetProvideWhileDisconnected(_ any, provideWhileDisconnected *bool) error {
	*provideWhileDisconnected = deviceLocal.GetProvideWhileDisconnected()
	return nil
}

func (self *deviceLocalRpc) SetProvideWhileDisconnected(provideWhileDisconnected bool, _ any) error {
	deviceLocal.SetProvideWhileDisconnected(provideWhileDisconnected)
	return nil
}

func (self *deviceLocalRpc) GetCanRefer(_ any, canRefer *bool) error {
	*canRefer = deviceLocal.GetCanRefer()
	return nil
}

func (self *deviceLocalRpc) SetCanRefer(canRefer bool, _ any) error {
	deviceLocal.SetCanRefer(canRefer)
	return nil
}

func (self *deviceLocalRpc) SetRouteLocal(routeLocal bool, _ any) error {
	deviceLocal.SetRouteLocal(routeLocal)
	return nil
}

func (self *deviceLocalRpc) GetRouteLocal(_ any, routeLocal *bool) error {
	*routeLocal = deviceLocal.GetRouteLocal()
	return nil
}

func (self *deviceLocalRpc) AddProvideChangeListener(listenerId Id, _ any) error {
	self.provideChangeListenerIds[listenerId] = true
	if self.provideChangeListenerSub == nil {
		self.provideChangeListenerSub = deviceLocal.AddProvideChangeListener(self)
	}
}

func (self *deviceLocalRpc) RemoveProvideChangeListener(listenerId Id, _ any) error {
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


func (self *deviceLocalRpc) AddProvidePausedChangeListener(listenerId Id, _ any) error {
	self.providePausedChangeListenerIds[listenerId] = true
	if self.providePausedChangeListenerSub == nil {
		self.providePausedChangeListenerSub = deviceLocal.AddProvidePausedChangeListener(self)
	}
}

func (self *deviceLocalRpc) RemoveProvidePausedChangeListener(listenerId Id, _ any) error {
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


func (self *deviceLocalRpc) AddOfflineChangeListener(listenerId Id, _ any) error {
	self.offlineChangeListenerIds[listenerId] = true
	if self.offlineChangeListenerSub == nil {
		self.offlineChangeListenerSub = deviceLocal.AddOfflineChangeListener(self)
	}
}

func (self *deviceLocalRpc) RemoveOfflineChangeListener(listenerId Id, _ any) error {
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


func (self *deviceLocalRpc) AddConnectChangeListener(listenerId Id, _ any) error {
	self.connectChangeListenerIds[listenerId] = true
	if self.connectChangeListenerSub == nil {
		self.connectChangeListenerSub = deviceLocal.AddConnectChangeListener(self)
	}
}

func (self *deviceLocalRpc) RemoveConnectChangeListener(listenerId Id, _ any) error {
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


func (self *deviceLocalRpc) AddRouteLocalChangeListener(listenerId Id, _ any) error {
	self.routeLocalChangeListenerIds[listenerId] = true
	if self.routeLocalChangeListenerSub == nil {
		self.routeLocalChangeListenerSub = deviceLocal.AddRouteLocalChangeListener(self)
	}
}

func (self *deviceLocalRpc) RemoveRouteLocalChangeListener(listenerId Id, _ any) error {
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



func (self *deviceLocalRpc) AddConnectLocationChangeListener(listenerId Id, _ any) error {
	self.connectLocationChangeIds[listenerId] = true
	if self.connectLocationChangeSub == nil {
		self.connectLocationChangeSub = deviceLocal.AddConnectLocationChangeListener(self)
	}
}

func (self *deviceLocalRpc) RemoveConnectLocationChangeListener(listenerId Id, _ any) error {
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


func (self *deviceLocalRpc) AddProvideSecretKeysListener(listenerId Id, _ any) error {
	self.provideSecretKeysListenerIds[listenerId] = true
	if self.provideSecretKeysListenerSub == nil {
		self.provideSecretKeysListenerSub = deviceLocal.AddProvideSecretKeysListener(self)
	}
}

func (self *deviceLocalRpc) RemoveProvideSecretKeysListener(listenerId Id, _ any) error {
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


func (self *deviceLocalRpc) LoadProvideSecretKeys(provideSecretKeyList *ProvideSecretKeyList, _ any) error {
	deviceLocal.LoadProvideSecretKeys(provideSecretKeyList)
	return nil
}

func (self *deviceLocalRpc) InitProvideSecretKeys(_ any, _ any) error {
	deviceLocal.InitProvideSecretKeys()
	return nil
}

func (self *deviceLocalRpc) GetProvideEnabled(_ any, provideEnabled *bool) error {
	*provideEnabled = deviceLocal.GetProvideEnabled()
	return nil
}

func (self *deviceLocalRpc) GetConnectEnabled(_ any, connectEnabled *bool) error {
	*connectEnabled = deviceLocal.GetConnectEnabled()
	return nil
}

func (self *deviceLocalRpc) SetProvideMode(provideMode ProvideMode, _ any) error {
	deviceLocal.SetProvideMod(provideMode)
	return nil
} 

func (self *deviceLocalRpc) GetProvideMode(_ any, provideMode *ProvideMode) error {
	*provideMode = deviceLocal.GetProvideMode()
	return nil
}

func (self *deviceLocalRpc) SetProvidePaused(providePaused bool, _ any) error {
	deviceLocal.SetProvidePaused(providePaused)
	return nil
} 

func (self *deviceLocalRpc) GetProvidePaused(_ any, providePaused *bool) error {
	*providePaused = deviceLocal.GetProvidePaused()
	return nil
}

func (self *deviceLocalRpc) SetOffline(offline bool, _ any) error {
	deviceLocal.SetOffline(offline)
	return nil
}

func (self *deviceLocalRpc) GetOffline(_ any, offline *bool) error {
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
	self.cancel()
	if self.service != nil {
		self.service.Close()
		self.service = nil
	}
}


type deviceRemoteRpc struct {
	ctx context.Context
	cancel context.CancelFunc
	deviceRemote *DeviceRemote
}

func newDeviceRemoteRpc(ctx context.Context, deviceRemote *DeviceRemote) *deviceRemoteRpc {
	cancelCtx, cancel := context.WithCancel(ctx)

	return &deviceRemoteRpc{
		ctx: cancelCtx,
		cancel: cancel,
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
	self.cancel()
}
