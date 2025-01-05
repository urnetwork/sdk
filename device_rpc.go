package sdk

import (
	"context"
	"time"
	"sync"
	"net"
	"net/netip"
	"net/rpc"
	"slices"
	"strconv"
	// "fmt"

	"golang.org/x/exp/maps"

	// "github.com/golang/glog"

	"github.com/urnetwork/connect"
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


type deviceRpcSettings struct {
	RpcConnectTimeout time.Duration
	RpcReconnectTimeout time.Duration
	// TODO randomize the ports
	Address *deviceRemoteAddress
	ResponseAddress *deviceRemoteAddress
}

func defaultDeviceRpcSettings() *deviceRpcSettings {
	return &deviceRpcSettings{
		RpcConnectTimeout: 10 * time.Second,
		RpcReconnectTimeout: 1 * time.Second,
		Address: requireRemoteAddress("127.0.0.1:12025"),
		ResponseAddress: requireRemoteAddress("127.0.0.1:12026"),
	}
}

// compile check that DeviceRemote conforms to Device and device
var _ Device = (*DeviceRemote)(nil)
var _ device = (*DeviceRemote)(nil)
type DeviceRemote struct {
	ctx context.Context
	cancel context.CancelFunc

	networkSpace *NetworkSpace
	byJwt string

	settings *deviceRpcSettings

	reconnectMonitor *connect.Monitor
	
	clientId connect.Id
	clientStrategy *connect.ClientStrategy

	stateLock sync.Mutex

	service *rpc.Client

	provideChangeListeners map[connect.Id]ProvideChangeListener
	providePausedChangeListeners map[connect.Id]ProvidePausedChangeListener
	offlineChangeListeners map[connect.Id]OfflineChangeListener
	connectChangeListeners map[connect.Id]ConnectChangeListener
	routeLocalChangeListeners map[connect.Id]RouteLocalChangeListener
	connectLocationChangeListeners map[connect.Id]ConnectLocationChangeListener
	provideSecretKeyListeners map[connect.Id]ProvideSecretKeysListener

	state deviceRemoteState
}

func NewDeviceRemoteWithDefaults(
	networkSpace *NetworkSpace,
	byJwt string,
) (*DeviceRemote, error) {
	return NewDeviceRemote(networkSpace, byJwt, defaultDeviceRpcSettings())
}

func NewDeviceRemote(
	networkSpace *NetworkSpace,
	byJwt string,
	settings *deviceRpcSettings,
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
		settings: settings,
		reconnectMonitor: connect.NewMonitor(),
		clientId: clientId,
		clientStrategy: networkSpace.clientStrategy,
	}

	// remote starts locked
	// only after the first attempt to connect to the local does it unlock
	device.stateLock.Lock()
	go device.run()
	return device, nil
}

func (self *DeviceRemote) run() {
	for i := 0; true; i += 1 {
		handleCtx, handleCancel := context.WithCancel(self.ctx)

		reconnect := self.reconnectMonitor.NotifyChannel()
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
			conn, err := dialer.DialContext(handleCtx, "tcp", self.settings.Address.HostPort())
			if err != nil {
				return
			}
			// FIXME
			// tls.Handshake()

			service := rpc.NewClient(conn)
			defer service.Close()
			
			syncRequest := &deviceRemoteSyncRequest{
				ProvideChangeListenerIds: maps.Keys(self.provideChangeListeners),
				ProvidePausedChangeListenerIds: maps.Keys(self.providePausedChangeListeners),
				OfflineChangeListenerIds: maps.Keys(self.offlineChangeListeners),
				ConnectChangeListenerIds: maps.Keys(self.connectChangeListeners),
				RouteLocalChangeListenerIds: maps.Keys(self.routeLocalChangeListeners),
				ConnectLocationChangeListenerIds: maps.Keys(self.connectLocationChangeListeners),
				ProvideSecretKeysListenerIds: maps.Keys(self.provideSecretKeyListeners),
				deviceRemoteState: self.state,
			}
			var syncResponse *deviceRemoteSyncResponse
			err = service.Call("deviceLocalRpc.Sync", syncRequest, &syncResponse)
			if err != nil {
				return
			}

			// FIXME use response cert to listen with TLS
			listenConfig := &net.ListenConfig{
				KeepAliveConfig: net.KeepAliveConfig{
					Enable: true,
				},
			}
			responseListener, err := listenConfig.Listen(handleCtx, "tcp", self.settings.ResponseAddress.HostPort())
			if err != nil {
				return
			}
			defer responseListener.Close()

			deviceRemoteRpc := newDeviceRemoteRpc(handleCtx, self)
			server := rpc.NewServer()
			server.Register(deviceRemoteRpc)

			defer deviceRemoteRpc.Close()
			// defer server.Close()

			go func() {
				defer handleCancel()

				// handle connections serially
				for {
					conn, err := responseListener.Accept()
					if err != nil {
						return
					}
					server.ServeConn(conn)
				}
			}()


			err = service.Call("deviceLocalRpc.ConnectResponse", self.settings.ResponseAddress, nil)
			if err != nil {
				return
			}


			func() {
				if  0 < i {
					self.stateLock.Lock()
					defer self.stateLock.Unlock()
				}
				self.state.Unset()
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
			case <- self.ctx.Done():
			case <- handleCtx.Done():
			}
		}()

		select {
		case <- self.ctx.Done():
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
	self.reconnectMonitor.NotifyAll()
}

func (self *DeviceRemote) GetClientId() *Id {
	return newId(self.clientId)
}

func (self *DeviceRemote) GetApi() *Api {
	return self.networkSpace.GetApi()
}

func (self *DeviceRemote) GetNetworkSpace() *NetworkSpace {
	return self.networkSpace
}

func (self *DeviceRemote) GetStats() *DeviceStats {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	stats, success := func()(*DeviceStats, bool) {
		if self.service == nil {
			return nil, false
		}

		var stats *DeviceStats
		err := self.service.Call("deviceLocalRpc.GetStats", nil, &stats)
		if err != nil {
			self.service.Close()
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
		if self.service == nil {
			return false, false
		}

		var shouldShowRatingDialog bool
		err := self.service.Call("deviceLocalRpc.GetShouldShowRatingDialog", nil, &shouldShowRatingDialog)
		if err != nil {
			self.service.Close()
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
		if self.service == nil {
			return false, false
		}

		var canShowRatingDialog bool
		err := self.service.Call("deviceLocalRpc.GetCanShowRatingDialog", nil, &canShowRatingDialog)
		if err != nil {
			self.service.Close()
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
		if self.service == nil {
			return false
		}

		err := self.service.Call("deviceLocalRpc.SetCanShowRatingDialog", canShowRatingDialog, nil)
		if err != nil {
			self.service.Close()
			return false
		}
		return true
	}()
	if success {
		self.state.CanShowRatingDialog.Unset()
	} else {
		self.state.CanShowRatingDialog.Set(canShowRatingDialog)
	}
}

func (self *DeviceRemote) GetProvideWhileDisconnected() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	provideWhileDisconnected, success := func()(bool, bool) {
		if self.service == nil {
			return false, false
		}

		var provideWhileDisconnected bool
		err := self.service.Call("deviceLocalRpc.GetProvideWhileDisconnected", nil, &provideWhileDisconnected)
		if err != nil {
			self.service.Close()
			return false, false
		}
		return provideWhileDisconnected, true
	}()
	if success {
		return provideWhileDisconnected
	} else {
		return self.state.ProvideWhileDisconnected.Value
	}
}

func (self *DeviceRemote) SetProvideWhileDisconnected(provideWhileDisconnected bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	success := func()(bool) {
		if self.service == nil {
			return false
		}

		err := self.service.Call("deviceLocalRpc.SetProvideWhileDisconnected", provideWhileDisconnected, nil)
		if err != nil {
			self.service.Close()
			return false
		}
		return true
	}()
	if success {
		self.state.ProvideWhileDisconnected.Unset()
	} else {
		self.state.ProvideWhileDisconnected.Set(provideWhileDisconnected)
	}
}

func (self *DeviceRemote) GetCanRefer() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	canRefer, success := func()(bool, bool) {
		if self.service == nil {
			return false, false
		}

		var canRefer bool
		err := self.service.Call("deviceLocalRpc.GetCanRefer", nil, &canRefer)
		if err != nil {
			self.service.Close()
			return false, false
		}
		return canRefer, true
	}()
	if success {
		return canRefer
	} else {	
		return self.state.CanRefer.Value
	}
}

func (self *DeviceRemote) SetCanRefer(canRefer bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	success := func()(bool) {
		if self.service == nil {
			return false
		}

		err := self.service.Call("deviceLocalRpc.SetProvideWhileDisconnected", canRefer, nil)
		if err != nil {
			self.service.Close()
			return false
		}
		return true
	}()
	if success {
		self.state.CanRefer.Unset()
	} else {
		self.state.CanRefer.Set(canRefer)
	}
}

func (self *DeviceRemote) SetRouteLocal(routeLocal bool) {
	event := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		success := func()(bool) {
			if self.service == nil {
				return false
			}

			err := self.service.Call("deviceLocalRpc.SetRouteLocal", routeLocal, nil)
			if err != nil {
				self.service.Close()
				return false
			}
			return true
		}()
		if success {
			self.state.RouteLocal.Unset()
		} else {
			self.state.RouteLocal.Set(routeLocal)
			event = true
		}
	}()
	if event {
		self.routeLocalChanged(self.GetRouteLocal())
	}
}

func (self *DeviceRemote) GetRouteLocal() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	routeLocal, success := func()(bool, bool) {
		if self.service == nil {
			return false, false
		}

		var routeLocal bool
		err := self.service.Call("deviceLocalRpc.GetRouteLocal", nil, &routeLocal)
		if err != nil {
			self.service.Close()
			return false, false
		}
		return routeLocal, true
	}()
	if success {
		return routeLocal
	} else {	
		return self.state.RouteLocal.Value
	}
}

func addListener[T any](
	deviceRemote *DeviceRemote,
	listener T,
	listeners map[connect.Id]T,
	addServiceFunc string,
	removeServiceFunc string,
) Sub {
	deviceRemote.stateLock.Lock()
	defer deviceRemote.stateLock.Unlock()

	listenerId := connect.NewId()
	listeners[listenerId] = listener
	if deviceRemote.service != nil {
		err := deviceRemote.service.Call(addServiceFunc, listenerId, nil)
		if err != nil {
			deviceRemote.service.Close()
		}
	}

	return newSub(func() {
		deviceRemote.stateLock.Lock()
		defer deviceRemote.stateLock.Unlock()

		delete(listeners, listenerId)
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
		if self.service == nil {
			return false
		}

		err := self.service.Call("deviceLocalRpc.LoadProvideSecretKeys", provideSecretKeyList, nil)
		if err != nil {
			self.service.Close()
			return false
		}
		return true
	}()
	if success {
		self.state.LoadProvideSecretKeys.Unset()
	} else {	
		self.state.LoadProvideSecretKeys.Set(provideSecretKeyList)
		self.state.InitProvideSecretKeys.Unset()
	}
}

func (self *DeviceRemote) InitProvideSecretKeys() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	success := func()(bool) {
		if self.service == nil {
			return false
		}

		err := self.service.Call("deviceLocalRpc.InitProvideSecretKeys", nil, nil)
		if err != nil {
			self.service.Close()
			return false
		}
		return true
	}()
	if success {
		self.state.InitProvideSecretKeys.Unset()
	} else {	
		self.state.InitProvideSecretKeys.Set(true)
		self.state.LoadProvideSecretKeys.Unset()
	}
}

func (self *DeviceRemote) GetProvideEnabled() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	provideEnabled, success := func()(bool, bool) {
		if self.service == nil {
			return false, false
		}

		var provideEnabled bool
		err := self.service.Call("deviceLocalRpc.GetProvideEnabled", nil, &provideEnabled)
		if err != nil {
			self.service.Close()
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
		if self.service == nil {
			return false, false
		}

		var connectEnabled bool
		err := self.service.Call("deviceLocalRpc.GetConnectEnabled", nil, &connectEnabled)
		if err != nil {
			self.service.Close()
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
		if self.service == nil {
			return false
		}

		err := self.service.Call("deviceLocalRpc.SetProvideMode", provideMode, nil)
		if err != nil {
			self.service.Close()
			return false
		}
		return true
	}()
	if success {
		self.state.ProvideMode.Unset()
	} else {	
		self.state.ProvideMode.Set(provideMode)
	}
}

func (self *DeviceRemote) GetProvideMode() ProvideMode {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	provideMode, success := func()(ProvideMode, bool) {
		if self.service == nil {
			var empty ProvideMode
			return empty, false
		}

		var provideMode ProvideMode
		err := self.service.Call("deviceLocalRpc.GetProvideMode", nil, &provideMode)
		if err != nil {
			self.service.Close()
			var empty ProvideMode
			return empty, false
		}
		return provideMode, true
	}()
	if success {
		return provideMode
	} else {	
		return self.state.ProvideMode.Value
	}
}

func (self *DeviceRemote) SetProvidePaused(providePaused bool) {
	event := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		success := func()(bool) {
			if self.service == nil {
				return false
			}

			err := self.service.Call("deviceLocalRpc.SetProvidePaused", providePaused, nil)
			if err != nil {
				self.service.Close()
				return false
			}
			return true
		}()
		if success {
			self.state.ProvidePaused.Unset()
		} else {	
			self.state.ProvidePaused.Set(providePaused)
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
		if self.service == nil {
			return false, false
		}

		var providePaused bool
		err := self.service.Call("deviceLocalRpc.GetProvidePaused", nil, &providePaused)
		if err != nil {
			self.service.Close()
			return false, false
		}
		return providePaused, true
	}()
	if success {
		return providePaused
	} else {	
		return self.state.ProvidePaused.Value
	}
}

func (self *DeviceRemote) SetOffline(offline bool) {
	event := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		success := func()(bool) {
			if self.service == nil {
				return false
			}

			err := self.service.Call("deviceLocalRpc.SetOffline", offline, nil)
			if err != nil {
				self.service.Close()
				return false
			}
			return true
		}()
		if success {
			self.state.Offline.Unset()
		} else {	
			self.state.Offline.Set(offline)
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
		if self.service == nil {
			return false, false
		}

		var offline bool
		err := self.service.Call("deviceLocalRpc.GetOffline", nil, &offline)
		if err != nil {
			self.service.Close()
			return false, false
		}
		return offline, true
	}()
	if success {
		return offline
	} else {	
		return self.state.Offline.Value
	}
}

func (self *DeviceRemote) SetVpnInterfaceWhileOffline(vpnInterfaceWhileOffline bool) {
	event := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		success := func()(bool) {
			if self.service == nil {
				return false
			}

			err := self.service.Call("deviceLocalRpc.SetVpnInterfaceWhileOffline", vpnInterfaceWhileOffline, nil)
			if err != nil {
				self.service.Close()
				return false
			}
			return true
		}()
		if success {
			self.state.VpnInterfaceWhileOffline.Unset()
		} else {	
			self.state.VpnInterfaceWhileOffline.Set(vpnInterfaceWhileOffline)
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
		if self.service == nil {
			return false, false
		}

		var vpnInterfaceWhileOffline bool
		err := self.service.Call("deviceLocalRpc.GetVpnInterfaceWhileOffline", nil, &vpnInterfaceWhileOffline)
		if err != nil {
			self.service.Close()
			return false, false
		}
		return vpnInterfaceWhileOffline, true
	}()
	if success {
		return vpnInterfaceWhileOffline
	} else {	
		return self.state.VpnInterfaceWhileOffline.Value
	}
}

func (self *DeviceRemote) RemoveDestination() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	success := func()(bool) {
		if self.service == nil {
			return false
		}

		err := self.service.Call("deviceLocalRpc.RemoveDestination", nil, nil)
		if err != nil {
			self.service.Close()
			return false
		}
		return true
	}()
	if success {
		self.state.RemoveDestination.Unset()
	} else {	
		self.state.RemoveDestination.Set(true)
		self.state.Destination.Unset()
		self.state.Location.Unset()
	}
}

func (self *DeviceRemote) SetDestination(location *ConnectLocation, specs *ProviderSpecList, provideMode ProvideMode) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	destination := &deviceRemoteDestination{
		Location: location,
		Specs: specs,
		ProvideMode: provideMode,
	}

	success := func()(bool) {
		if self.service == nil {
			return false
		}

		err := self.service.Call("deviceLocalRpc.SetDestination", destination, nil)
		if err != nil {
			self.service.Close()
			return false
		}
		return true
	}()
	if success {
		self.state.Destination.Unset()
	} else {	
		self.state.Destination.Set(destination)
		self.state.RemoveDestination.Unset()
		self.state.Location.Unset()
	}
}

func (self *DeviceRemote) SetConnectLocation(location *ConnectLocation) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	success := func()(bool) {
		if self.service == nil {
			return false
		}

		err := self.service.Call("deviceLocalRpc.SetConnectLocation", location, nil)
		if err != nil {
			self.service.Close()
			return false
		}
		return true
	}()
	if success {
		self.state.Location.Unset()
	} else {
		self.state.Location.Set(location)
		self.state.RemoveDestination.Unset()
		self.state.Destination.Unset()
	}
}

func (self *DeviceRemote) GetConnectLocation() *ConnectLocation {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	location, success := func()(*ConnectLocation, bool) {
		if self.service == nil {
			return nil, false
		}

		var location *ConnectLocation
		err := self.service.Call("deviceLocalRpc.GetConnectLocation", nil, &location)
		if err != nil {
			self.service.Close()
			return nil, false
		}
		return location, true
	}()
	if success {
		return location
	} else {
		return self.state.Location.Value
	}
}

func (self *DeviceRemote) Shuffle() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	success := func()(bool) {
		if self.service == nil {
			return false
		}

		err := self.service.Call("deviceLocalRpc.Shuffle", nil, nil)
		if err != nil {
			self.service.Close()
			return false
		}
		return true
	}()
	if success {
		self.state.Shuffle.Unset()
	} else {
		self.state.Shuffle.Set(true)
	}
}

func (self *DeviceRemote) Close() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.cancel()
}


func (self *DeviceRemote) windowMonitor() windowMonitor {
	// FIXME if connected, get windowId and lock window monitor to that
	// FIXME else queue up window window monitor
	return newDeviceRemoteWindowMonitor(self, windowId)
}

// FIXME queue up window monitors
func (self *DeviceRemote) windowMonitorAddMonitorEventCallback(monitorEventCallback connect.MonitorEventFunction) func() {
	// FIXME queue up to add
	// FIXME remote side tracks last used window monitor
	// FIXME if different, clear all listeners and reset
	// FIXME return window Id
	// FIXME track window Id on local side, and clear listeners of old window id
}

func (self *DeviceRemote) windowMonitorEvents() (*connect.WindowExpandEvent, map[connect.Id]*connect.ProviderEvent) {
	// FIXME if not connected, empty
}






// event dispatch

func listenerList[T any](lock sync.Mutex, listenerMap map[connect.Id]T) []T {
	lock.Lock()
	defer lock.Unlock()

	// consistent dispatch order
	n := len(listenerMap)
	orderedKeys := maps.Keys(listenerMap)
	slices.SortFunc(orderedKeys, func(a connect.Id, b connect.Id)(int) {
		return a.Cmp(b)
	})
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
	for _, connectChangeListener := range listenerList(self.stateLock, self.connectChangeListeners) {
		connectChangeListener.ConnectChanged(connectEnabled)
	}
}

func (self *DeviceRemote) routeLocalChanged(routeLocal bool) {
	for _, routeLocalChangeListener := range listenerList(self.stateLock, self.routeLocalChangeListeners) {
		routeLocalChangeListener.RouteLocalChanged(routeLocal)
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


type deviceRemoteDestination struct {
	Location *ConnectLocation
	Specs *ProviderSpecList
	ProvideMode ProvideMode
}


type deviceRemoteValue[T any] struct {
	Value T
	IsSet bool
}

func (self *deviceRemoteValue[T]) Set(value T) {
	self.Value = value
	self.IsSet = true
}

func (self *deviceRemoteValue[T]) Unset() {
	var empty T
	self.Value = empty
	self.IsSet = false
}


type deviceRemoteAddress struct {
	Ip netip.Addr
	Port int
}

func parseDeviceRemoteAddress(hostPort string) (*deviceRemoteAddress, error) {
	host, portStr, err := net.SplitHostPort(hostPort)
	if err != nil {
		return nil, err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, err
	}
	return &deviceRemoteAddress{
		Ip: ip,
		Port: port,
	}, nil
}

func requireRemoteAddress(hostPort string) *deviceRemoteAddress {
	address, err := parseDeviceRemoteAddress(hostPort)
	if err != nil {
		panic(err)
	}
	return address
}

func (self *deviceRemoteAddress) HostPort() string {
	return net.JoinHostPort(self.Ip.String(), strconv.Itoa(self.Port))
}


type deviceRemoteOfflineChangeEvent struct {
	Offline bool
	VpnInterfaceWhileOffline bool
}


type deviceRemoteState struct {
	CanShowRatingDialog deviceRemoteValue[bool] 
	ProvideWhileDisconnected deviceRemoteValue[bool]
	CanRefer deviceRemoteValue[bool]
	RouteLocal deviceRemoteValue[bool]
	InitProvideSecretKeys deviceRemoteValue[bool]
	LoadProvideSecretKeys deviceRemoteValue[*ProvideSecretKeyList]
	ProvideMode deviceRemoteValue[ProvideMode]
	ProvidePaused deviceRemoteValue[bool]
	Offline deviceRemoteValue[bool]
	VpnInterfaceWhileOffline deviceRemoteValue[bool]
	RemoveDestination deviceRemoteValue[bool]
	Destination deviceRemoteValue[*deviceRemoteDestination]
	Location deviceRemoteValue[*ConnectLocation]
	Shuffle deviceRemoteValue[bool]
}

func (self *deviceRemoteState) Unset() {
	self.CanShowRatingDialog.Unset()
	self.ProvideWhileDisconnected.Unset()
	self.CanRefer.Unset()
	self.RouteLocal.Unset()
	self.InitProvideSecretKeys.Unset()
	self.LoadProvideSecretKeys.Unset()
	self.ProvideMode.Unset()
	self.ProvidePaused.Unset()
	self.Offline.Unset()
	self.VpnInterfaceWhileOffline.Unset()
	self.Destination.Unset()
	self.Location.Unset()
	self.Shuffle.Unset()
}

type deviceRemoteSyncRequest struct {
	ProvideChangeListenerIds []connect.Id
	ProvidePausedChangeListenerIds []connect.Id
	OfflineChangeListenerIds []connect.Id
	ConnectChangeListenerIds []connect.Id
	RouteLocalChangeListenerIds []connect.Id
	ConnectLocationChangeListenerIds []connect.Id
	ProvideSecretKeysListenerIds []connect.Id
	deviceRemoteState
}

type deviceRemoteSyncResponse struct {
	// FIXME response cert
}


// rpc are called on a single go routine


type deviceLocalRpc struct {
	ctx context.Context
	cancel context.CancelFunc

	deviceLocal *DeviceLocal
	settings *deviceRpcSettings

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
	deviceLocal *DeviceLocal,
) *deviceLocalRpc {
	return newDeviceLocalRpc(ctx, deviceLocal, defaultDeviceRpcSettings())
}

func newDeviceLocalRpc(
	ctx context.Context,
	deviceLocal *DeviceLocal,
	settings *deviceRpcSettings,
) *deviceLocalRpc {
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

			listenConfig := &net.ListenConfig{
				KeepAliveConfig: net.KeepAliveConfig{
					Enable: true,
				},
			}
			listener, err := listenConfig.Listen(handleCtx, "tcp", self.settings.Address.HostPort())
			if err != nil {
				return
			}

			defer listener.Close()

			server := rpc.NewServer()
			server.Register(self)

			// defer server.Close()

			go func() {
				defer handleCancel()

				// handle connections serially
				for {
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					server.ServeConn(conn)
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

func (self *deviceLocalRpc) Sync(
	syncRequest *deviceRemoteSyncRequest,
	syncResponse **deviceRemoteSyncResponse,
) error {

	// apply state adjustments

	if syncRequest.CanShowRatingDialog.IsSet {
		self.deviceLocal.SetCanShowRatingDialog(syncRequest.CanShowRatingDialog.Value)
	}
	if syncRequest.ProvideWhileDisconnected.IsSet {
		self.deviceLocal.SetProvideWhileDisconnected(syncRequest.ProvideWhileDisconnected.Value)
	}
	if syncRequest.CanRefer.IsSet {
		self.deviceLocal.SetCanRefer(syncRequest.CanRefer.Value)
	}
	if syncRequest.RouteLocal.IsSet {
		self.deviceLocal.SetRouteLocal(syncRequest.RouteLocal.Value)
	}
	if syncRequest.InitProvideSecretKeys.IsSet {
		self.deviceLocal.InitProvideSecretKeys()
	}
	if syncRequest.LoadProvideSecretKeys.IsSet {
		self.deviceLocal.LoadProvideSecretKeys(syncRequest.LoadProvideSecretKeys.Value)
	}
	if syncRequest.ProvideMode.IsSet {
		self.deviceLocal.SetProvideMode(syncRequest.ProvideMode.Value)
	}
	if syncRequest.ProvidePaused.IsSet {
		self.deviceLocal.SetProvidePaused(syncRequest.ProvidePaused.Value)
	}
	if syncRequest.Offline.IsSet {
		self.deviceLocal.SetOffline(syncRequest.Offline.Value)
	}
	if syncRequest.VpnInterfaceWhileOffline.IsSet {
		self.deviceLocal.SetVpnInterfaceWhileOffline(syncRequest.VpnInterfaceWhileOffline.Value)
	}
	if syncRequest.RemoveDestination.IsSet {
		self.deviceLocal.RemoveDestination()
	}
	if syncRequest.Destination.IsSet {
		destination := syncRequest.Destination.Value
		self.deviceLocal.SetDestination(
			destination.Location,
			destination.Specs,
			destination.ProvideMode,
		)
	}
	if syncRequest.Location.IsSet {
		self.deviceLocal.SetConnectLocation(syncRequest.Location.Value)
	}
	if syncRequest.Shuffle.IsSet {
		self.deviceLocal.Shuffle()
	}


	// add listeners

	for provideChangeListenerId, _ := range self.provideChangeListenerIds {
		self.RemoveProvideChangeListener(provideChangeListenerId, nil)
	}
	for _, provideChangeListenerId := range syncRequest.ProvideChangeListenerIds {
		self.AddProvideChangeListener(provideChangeListenerId, nil)
	}

	for providePausedChangeListenerId, _ := range self.providePausedChangeListenerIds {
		self.RemoveProvidePausedChangeListener(providePausedChangeListenerId, nil)
	}
	for _, providePausedChangeListenerId := range syncRequest.ProvidePausedChangeListenerIds {
		self.AddProvidePausedChangeListener(providePausedChangeListenerId, nil)
	}

	for offlineChangeListenerId, _ := range self.offlineChangeListenerIds {
		self.RemoveOfflineChangeListener(offlineChangeListenerId, nil)
	}
	for _, offlineChangeListenerId := range syncRequest.OfflineChangeListenerIds {
		self.AddOfflineChangeListener(offlineChangeListenerId, nil)
	}

	for connectChangeListenerId, _ := range self.connectChangeListenerIds {
		self.RemoveConnectChangeListener(connectChangeListenerId, nil)
	}
	for _, connectChangeListenerId := range syncRequest.ConnectChangeListenerIds {
		self.AddConnectChangeListener(connectChangeListenerId, nil)
	}

	for routeLocalChangeListenerId, _ := range self.routeLocalChangeListenerIds {
		self.RemoveRouteLocalChangeListener(routeLocalChangeListenerId, nil)
	}
	for _, routeLocalChangeListenerId := range syncRequest.RouteLocalChangeListenerIds {
		self.AddRouteLocalChangeListener(routeLocalChangeListenerId, nil)
	}

	for connectLocationChangeListenerId, _ := range self.connectLocationChangeListenerIds {
		self.RemoveConnectLocationChangeListener(connectLocationChangeListenerId, nil)
	}
	for _, connectLocationChangeListenerId := range syncRequest.ConnectLocationChangeListenerIds {
		self.AddConnectLocationChangeListener(connectLocationChangeListenerId, nil)
	}

	for provideSecretKeysListenerId, _ := range self.provideSecretKeysListenerIds {
		self.RemoveProvideSecretKeysListener(provideSecretKeysListenerId, nil)
	}
	for _, provideSecretKeysListenerId := range syncRequest.ProvideSecretKeysListenerIds {
		self.AddProvideSecretKeysListener(provideSecretKeysListenerId, nil)
	}


	// fire listeners with the current state
	
	if self.provideChangeListenerSub != nil {
		self.ProvideChanged(self.deviceLocal.GetProvideEnabled())
	}
	if self.providePausedChangeListenerSub != nil {
		self.ProvidePausedChanged(self.deviceLocal.GetProvidePaused())
	}
	if self.offlineChangeListenerSub != nil {
		self.OfflineChanged(self.deviceLocal.GetOffline(), self.deviceLocal.GetVpnInterfaceWhileOffline())
	}
	if self.connectChangeListenerSub != nil {
		self.ConnectChanged(self.deviceLocal.GetConnectEnabled())
	}
	if self.routeLocalChangeListenerSub != nil {
		self.RouteLocalChanged(self.deviceLocal.GetRouteLocal())
	}
	if self.connectLocationChangeListenerSub != nil {
		self.ConnectLocationChanged(self.deviceLocal.GetConnectLocation())
	}
	if self.connectLocationChangeListenerSub != nil {
		self.ConnectLocationChanged(self.deviceLocal.GetConnectLocation())
	}
	if self.provideSecretKeysListenerSub != nil {
		self.ProvideSecretKeysChanged(self.deviceLocal.GetProvideSecretKeys())
	}

	return nil
}

func (self *deviceLocalRpc) ConnectResponse(responseAddress *deviceRemoteAddress, _ any) error {
	if self.service != nil {
		self.service.Close()
		self.service = nil
	}

	dialer := net.Dialer{
		Timeout: self.settings.RpcConnectTimeout,
		KeepAliveConfig: net.KeepAliveConfig{
			Enable: true,
		},
	}
	conn, err := dialer.DialContext(self.ctx, "tcp", responseAddress.HostPort())
	if err != nil {
		return err
	}
	// FIXME
	// tls.Handshake()

	self.service = rpc.NewClient(conn)

	return nil
}

func (self *deviceLocalRpc) GetStats(_ any, stats **DeviceStats) error {
	*stats = self.deviceLocal.GetStats()
	return nil
}

func (self *deviceLocalRpc) GetShouldShowRatingDialog(_ any, shouldShowRatingDialog *bool) error {
	*shouldShowRatingDialog = self.deviceLocal.GetShouldShowRatingDialog()
	return nil
}

func (self *deviceLocalRpc) GetCanShowRatingDialog(_ any, canShowRatingDialog *bool) error {
	*canShowRatingDialog = self.deviceLocal.GetCanShowRatingDialog()
	return nil
}

func (self *deviceLocalRpc) SetCanShowRatingDialog(canShowRatingDialog bool, _ any) error {
	self.deviceLocal.SetCanShowRatingDialog(canShowRatingDialog)
	return nil
} 

func (self *deviceLocalRpc) GetProvideWhileDisconnected(_ any, provideWhileDisconnected *bool) error {
	*provideWhileDisconnected = self.deviceLocal.GetProvideWhileDisconnected()
	return nil
}

func (self *deviceLocalRpc) SetProvideWhileDisconnected(provideWhileDisconnected bool, _ any) error {
	self.deviceLocal.SetProvideWhileDisconnected(provideWhileDisconnected)
	return nil
}

func (self *deviceLocalRpc) GetCanRefer(_ any, canRefer *bool) error {
	*canRefer = self.deviceLocal.GetCanRefer()
	return nil
}

func (self *deviceLocalRpc) SetCanRefer(canRefer bool, _ any) error {
	self.deviceLocal.SetCanRefer(canRefer)
	return nil
}

func (self *deviceLocalRpc) SetRouteLocal(routeLocal bool, _ any) error {
	self.deviceLocal.SetRouteLocal(routeLocal)
	return nil
}

func (self *deviceLocalRpc) GetRouteLocal(_ any, routeLocal *bool) error {
	*routeLocal = self.deviceLocal.GetRouteLocal()
	return nil
}

func (self *deviceLocalRpc) AddProvideChangeListener(listenerId connect.Id, _ any) error {
	self.provideChangeListenerIds[listenerId] = true
	if self.provideChangeListenerSub == nil {
		self.provideChangeListenerSub = self.deviceLocal.AddProvideChangeListener(self)
	}
	return nil
}

func (self *deviceLocalRpc) RemoveProvideChangeListener(listenerId connect.Id, _ any) error {
	delete(self.provideChangeListenerIds, listenerId)
	if len(self.provideChangeListenerIds) == 0 && self.provideChangeListenerSub != nil {
		self.provideChangeListenerSub.Close()
		self.provideChangeListenerSub = nil
	}
	return nil
}

// ProvideChangeListener
func (self *deviceLocalRpc) ProvideChanged(provideEnabled bool) {
	err := self.service.Call("deviceRemoteRpc.ProvideChanged", provideEnabled, nil)
	if err != nil {
		self.service.Close()
	}
}


func (self *deviceLocalRpc) AddProvidePausedChangeListener(listenerId connect.Id, _ any) error {
	self.providePausedChangeListenerIds[listenerId] = true
	if self.providePausedChangeListenerSub == nil {
		self.providePausedChangeListenerSub = self.deviceLocal.AddProvidePausedChangeListener(self)
	}
	return nil
}

func (self *deviceLocalRpc) RemoveProvidePausedChangeListener(listenerId connect.Id, _ any) error {
	delete(self.providePausedChangeListenerIds, listenerId)
	if len(self.providePausedChangeListenerIds) == 0 && self.providePausedChangeListenerSub != nil {
		self.providePausedChangeListenerSub.Close()
		self.providePausedChangeListenerSub = nil
	}
	return nil
}

// ProvidePausedChangeListener
func (self *deviceLocalRpc) ProvidePausedChanged(providePaused bool) {
	err := self.service.Call("deviceRemoteRpc.ProvidePausedChanged", providePaused, nil)
	if err != nil {
		self.service.Close()
	}
}


func (self *deviceLocalRpc) AddOfflineChangeListener(listenerId connect.Id, _ any) error {
	self.offlineChangeListenerIds[listenerId] = true
	if self.offlineChangeListenerSub == nil {
		self.offlineChangeListenerSub = self.deviceLocal.AddOfflineChangeListener(self)
	}
	return nil
}

func (self *deviceLocalRpc) RemoveOfflineChangeListener(listenerId connect.Id, _ any) error {
	delete(self.offlineChangeListenerIds, listenerId)
	if len(self.offlineChangeListenerIds) == 0 && self.offlineChangeListenerSub != nil {
		self.offlineChangeListenerSub.Close()
		self.offlineChangeListenerSub = nil
	}
	return nil
}

// OfflineChangeListener
func (self *deviceLocalRpc) OfflineChanged(offline bool, vpnInterfaceWhileOffline bool) {
	event := &deviceRemoteOfflineChangeEvent{
		Offline: offline,
		VpnInterfaceWhileOffline: vpnInterfaceWhileOffline,
	}
	err := self.service.Call("deviceRemoteRpc.OfflineChanged", event, nil)
	if err != nil {
		self.service.Close()
	}
}


func (self *deviceLocalRpc) AddConnectChangeListener(listenerId connect.Id, _ any) error {
	self.connectChangeListenerIds[listenerId] = true
	if self.connectChangeListenerSub == nil {
		self.connectChangeListenerSub = self.deviceLocal.AddConnectChangeListener(self)
	}
	return nil
}

func (self *deviceLocalRpc) RemoveConnectChangeListener(listenerId connect.Id, _ any) error {
	delete(self.connectChangeListenerIds, listenerId)
	if len(self.connectChangeListenerIds) == 0 && self.connectChangeListenerSub != nil {
		self.connectChangeListenerSub.Close()
		self.connectChangeListenerSub = nil
	}
	return nil
}

// ConnectChangeListener
func (self *deviceLocalRpc) ConnectChanged(connectEnabled bool) {
	err := self.service.Call("deviceRemoteRpc.ConnectChanged", connectEnabled, nil)
	if err != nil {
		self.service.Close()
	}
}


func (self *deviceLocalRpc) AddRouteLocalChangeListener(listenerId connect.Id, _ any) error {
	self.routeLocalChangeListenerIds[listenerId] = true
	if self.routeLocalChangeListenerSub == nil {
		self.routeLocalChangeListenerSub = self.deviceLocal.AddRouteLocalChangeListener(self)
	}
	return nil
}

func (self *deviceLocalRpc) RemoveRouteLocalChangeListener(listenerId connect.Id, _ any) error {
	delete(self.routeLocalChangeListenerIds, listenerId)
	if len(self.routeLocalChangeListenerIds) == 0 && self.routeLocalChangeListenerSub != nil {
		self.routeLocalChangeListenerSub.Close()
		self.routeLocalChangeListenerSub = nil
	}
	return nil
}

// RouteLocalChangeListener
func (self *deviceLocalRpc) RouteLocalChanged(routeLocal bool) {
	err := self.service.Call("deviceRemoteRpc.RouteLocalChanged", routeLocal, nil)
	if err != nil {
		self.service.Close()
	}
}


func (self *deviceLocalRpc) AddConnectLocationChangeListener(listenerId connect.Id, _ any) error {
	self.connectLocationChangeListenerIds[listenerId] = true
	if self.connectLocationChangeListenerSub == nil {
		self.connectLocationChangeListenerSub = self.deviceLocal.AddConnectLocationChangeListener(self)
	}
	return nil
}

func (self *deviceLocalRpc) RemoveConnectLocationChangeListener(listenerId connect.Id, _ any) error {
	delete(self.connectLocationChangeListenerIds, listenerId)
	if len(self.connectLocationChangeListenerIds) == 0 && self.connectLocationChangeListenerSub != nil {
		self.connectLocationChangeListenerSub.Close()
		self.connectLocationChangeListenerSub = nil
	}
	return nil
}

// ConnectLocationChangeListener
func (self *deviceLocalRpc) ConnectLocationChanged(location *ConnectLocation) {
	err := self.service.Call("deviceRemoteRpc.ConnectLocationChanged", location, nil)
	if err != nil {
		self.service.Close()
	}
}


func (self *deviceLocalRpc) AddProvideSecretKeysListener(listenerId connect.Id, _ any) error {
	self.provideSecretKeysListenerIds[listenerId] = true
	if self.provideSecretKeysListenerSub == nil {
		self.provideSecretKeysListenerSub = self.deviceLocal.AddProvideSecretKeysListener(self)
	}
	return nil
}

func (self *deviceLocalRpc) RemoveProvideSecretKeysListener(listenerId connect.Id, _ any) error {
	delete(self.provideSecretKeysListenerIds, listenerId)
	if len(self.provideSecretKeysListenerIds) == 0 && self.provideSecretKeysListenerSub != nil {
		self.provideSecretKeysListenerSub.Close()
		self.provideSecretKeysListenerSub = nil
	}
	return nil
}

// ProvideSecretKeysListener
func (self *deviceLocalRpc) ProvideSecretKeysChanged(provideSecretKeyList *ProvideSecretKeyList) {
	err := self.service.Call("deviceRemoteRpc.ProvideSecretKeysChanged", provideSecretKeyList, nil)
	if err != nil {
		self.service.Close()
	}
}


func (self *deviceLocalRpc) LoadProvideSecretKeys(provideSecretKeyList *ProvideSecretKeyList, _ any) error {
	self.deviceLocal.LoadProvideSecretKeys(provideSecretKeyList)
	return nil
}

func (self *deviceLocalRpc) InitProvideSecretKeys(_ any, _ any) error {
	self.deviceLocal.InitProvideSecretKeys()
	return nil
}

func (self *deviceLocalRpc) GetProvideEnabled(_ any, provideEnabled *bool) error {
	*provideEnabled = self.deviceLocal.GetProvideEnabled()
	return nil
}

func (self *deviceLocalRpc) GetConnectEnabled(_ any, connectEnabled *bool) error {
	*connectEnabled = self.deviceLocal.GetConnectEnabled()
	return nil
}

func (self *deviceLocalRpc) SetProvideMode(provideMode ProvideMode, _ any) error {
	self.deviceLocal.SetProvideMode(provideMode)
	return nil
} 

func (self *deviceLocalRpc) GetProvideMode(_ any, provideMode *ProvideMode) error {
	*provideMode = self.deviceLocal.GetProvideMode()
	return nil
}

func (self *deviceLocalRpc) SetProvidePaused(providePaused bool, _ any) error {
	self.deviceLocal.SetProvidePaused(providePaused)
	return nil
} 

func (self *deviceLocalRpc) GetProvidePaused(_ any, providePaused *bool) error {
	*providePaused = self.deviceLocal.GetProvidePaused()
	return nil
}

func (self *deviceLocalRpc) SetOffline(offline bool, _ any) error {
	self.deviceLocal.SetOffline(offline)
	return nil
}

func (self *deviceLocalRpc) GetOffline(_ any, offline *bool) error {
	*offline = self.deviceLocal.GetOffline()
	return nil
}

func (self *deviceLocalRpc) SetVpnInterfaceWhileOffline(vpnInterfaceWhileOffline bool, _ any) error {
	self.deviceLocal.SetVpnInterfaceWhileOffline(vpnInterfaceWhileOffline)
	return nil
}

func (self *deviceLocalRpc) GetVpnInterfaceWhileOffline(_ any, vpnInterfaceWhileOffline *bool) error {
	*vpnInterfaceWhileOffline = self.deviceLocal.GetVpnInterfaceWhileOffline()
	return nil
}

func (self *deviceLocalRpc) RemoveDestination(_ any, _ any) error {
	self.deviceLocal.RemoveDestination()
	return nil
}

func (self *deviceLocalRpc) SetDestination(destination *deviceRemoteDestination, _ any) error {
	self.deviceLocal.SetDestination(
		destination.Location,
		destination.Specs,
		destination.ProvideMode,
	)
	return nil
}

func (self *deviceLocalRpc) SetConnectLocation(location *ConnectLocation, _ any) error {
	self.deviceLocal.SetConnectLocation(location)
	return nil
} 

func (self *deviceLocalRpc) GetConnectLocation(_ any, location **ConnectLocation) error {
	*location = self.deviceLocal.GetConnectLocation()
	return nil
}  

func (self *deviceLocalRpc) Shuffle(_ any, _ any) error {
	self.deviceLocal.Shuffle()
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
	self.deviceRemote.provideChanged(provideEnabled)
	return nil
}

func (self *deviceRemoteRpc) ProvidePausedChanged(providePaused bool, _ any) error {
	self.deviceRemote.providePausedChanged(providePaused)
	return nil
}

func (self *deviceRemoteRpc) OfflineChanged(event *deviceRemoteOfflineChangeEvent, _ any) error {
	self.deviceRemote.offlineChanged(event.Offline, event.VpnInterfaceWhileOffline)
	return nil
}

func (self *deviceRemoteRpc) ConnectChanged(connectEnabled bool, _ any) error {
	self.deviceRemote.connectChanged(connectEnabled)
	return nil
}

func (self *deviceRemoteRpc) RouteLocalChanged(routeLocal bool, _ any) error {
	self.deviceRemote.routeLocalChanged(routeLocal)
	return nil
}

func (self *deviceRemoteRpc) ConnectLocationChanged(location *ConnectLocation, _ any) error {
	self.deviceRemote.connectLocationChanged(location)
	return nil
}

func (self *deviceRemoteRpc) ProvideSecretKeysChanged(provideSecretKeyList *ProvideSecretKeyList, _ any) error {
	self.deviceRemote.provideSecretKeysChanged(provideSecretKeyList)
	return nil
}

func (self *deviceRemoteRpc) Close() {
	self.cancel()
}
