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
	"fmt"
	// "runtime/debug"

	"golang.org/x/exp/maps"

	"github.com/golang/glog"

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


type RemoteChangeListener interface {
	RemoteChanged(remoteConnected bool)
}


type deviceRpcSettings struct {
	RpcConnectTimeout time.Duration
	RpcReconnectTimeout time.Duration
	// TODO randomize the ports
	Address *DeviceRemoteAddress
	ResponseAddress *DeviceRemoteAddress
	InitialLockTimeout time.Duration
}

func defaultDeviceRpcSettings() *deviceRpcSettings {
	return &deviceRpcSettings{
		RpcConnectTimeout: 1 * time.Second,
		RpcReconnectTimeout: 1 * time.Second,
		Address: requireRemoteAddress("127.0.0.1:12025"),
		ResponseAddress: requireRemoteAddress("127.0.0.1:12026"),
		InitialLockTimeout: 1 * time.Second,
	}
}

// FIXME this needs to track the last known value from get or event
// FIXME get when not sync and not set should return last known value
// FIXME the default values should be the last known value

// compile check that DeviceRemote conforms to Device, device, and ViewControllerManager
var _ Device = (*DeviceRemote)(nil)
var _ device = (*DeviceRemote)(nil)
var _ ViewControllerManager = (*DeviceRemote)(nil)
type DeviceRemote struct {
	ctx context.Context
	cancel context.CancelFunc

	networkSpace *NetworkSpace
	byJwt string

	settings *deviceRpcSettings

	reconnectMonitor *connect.Monitor
	syncMonitor *connect.Monitor
	
	clientId connect.Id
	clientStrategy *connect.ClientStrategy

	remoteChangeListeners *connect.CallbackList[RemoteChangeListener]

	stateLock sync.Mutex

	service *rpc.Client

	provideChangeListeners map[connect.Id]ProvideChangeListener
	providePausedChangeListeners map[connect.Id]ProvidePausedChangeListener
	offlineChangeListeners map[connect.Id]OfflineChangeListener
	connectChangeListeners map[connect.Id]ConnectChangeListener
	routeLocalChangeListeners map[connect.Id]RouteLocalChangeListener
	connectLocationChangeListeners map[connect.Id]ConnectLocationChangeListener
	provideSecretKeysListeners map[connect.Id]ProvideSecretKeysListener
	windowMonitors map[connect.Id]*deviceRemoteWindowMonitor

	httpResponseChannels map[connect.Id]chan *DeviceRemoteHttpResponse

	state DeviceRemoteState

	viewControllerManager
}

func NewDeviceRemoteWithDefaults(
	networkSpace *NetworkSpace,
	byJwt string,
) (*DeviceRemote, error) {
	return newDeviceRemote(networkSpace, byJwt, defaultDeviceRpcSettings())
}

func newDeviceRemote(
	networkSpace *NetworkSpace,
	byJwt string,
	settings *deviceRpcSettings,
) (*DeviceRemote, error) {
	clientId, err := parseByJwtClientId(byJwt)
	if err != nil {
		return nil, err
	}
	api := networkSpace.GetApi()
	api.SetByJwt(byJwt)

	deviceRemote, err := newDeviceRemoteWithOverrides(
		networkSpace,
		byJwt,
		settings,
		clientId,
	)
	if err != nil {
		return nil, err
	}

	api.setHttpPostRaw(deviceRemote.httpPostRaw)
	api.setHttpGetRaw(deviceRemote.httpGetRaw)

	return deviceRemote, nil
}

func newDeviceRemoteWithOverrides(
	networkSpace *NetworkSpace,
	byJwt string,
	settings *deviceRpcSettings,
	clientId connect.Id,
) (*DeviceRemote, error) {
	ctx, cancel := context.WithCancel(context.Background())

	deviceRemote := &DeviceRemote{
		ctx: ctx,
		cancel: cancel,
		networkSpace: networkSpace,
		byJwt: byJwt,
		settings: settings,
		reconnectMonitor: connect.NewMonitor(),
		syncMonitor: connect.NewMonitor(),
		clientId: clientId,
		clientStrategy: networkSpace.clientStrategy,
		remoteChangeListeners: connect.NewCallbackList[RemoteChangeListener](),

		provideChangeListeners: map[connect.Id]ProvideChangeListener{},
		providePausedChangeListeners: map[connect.Id]ProvidePausedChangeListener{},
		offlineChangeListeners: map[connect.Id]OfflineChangeListener{},
		connectChangeListeners: map[connect.Id]ConnectChangeListener{},
		routeLocalChangeListeners: map[connect.Id]RouteLocalChangeListener{},
		connectLocationChangeListeners: map[connect.Id]ConnectLocationChangeListener{},
		provideSecretKeysListeners: map[connect.Id]ProvideSecretKeysListener{},
		windowMonitors: map[connect.Id]*deviceRemoteWindowMonitor{},
		httpResponseChannels: map[connect.Id]chan *DeviceRemoteHttpResponse{},
	}
	deviceRemote.viewControllerManager = *newViewControllerManager(ctx, deviceRemote)

	// remote starts locked
	// only after the first attempt to connect to the local does it unlock
	deviceRemote.stateLock.Lock()
	go deviceRemote.run()
	return deviceRemote, nil
}

func (self *DeviceRemote) run() {
	// defer func() {
	// 	if r := recover(); r != nil {
	// 		glog.Errorf("[dr]unrecovered = %s", r)
	// 		debug.PrintStack()
	// 		panic(r)
	// 	}
	// }()

	initialLock := true
	intialLockEndTime := time.Now().Add(self.settings.InitialLockTimeout)
	for {
		handleCtx, handleCancel := context.WithCancel(self.ctx)

		notify := self.reconnectMonitor.NotifyChannel()
		func() {
			defer handleCancel()

			var responseListener net.Listener
			defer func() {
				if responseListener != nil {
					responseListener.Close()
				}
			}()

			synced := false
			func() {
				if initialLock {
					defer func() {
						if initialLock && intialLockEndTime.Before(time.Now()) {
							initialLock = false
							self.stateLock.Unlock()
						}
					}()
				} else {

					self.stateLock.Lock()
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
					// failure to connect here is normal if the local is not running
					// glog.Infof("[dr]sync connect err = %s", err)
					return
				}
				// FIXME
				// tls.Handshake()

				select {
				case <- handleCtx.Done():
					return
				default:
				}

				service := rpc.NewClient(conn)
				defer func() {
					if !synced {
						service.Close()
					}
				}()
				
				windowMonitorListenerIds := map[connect.Id][]connect.Id{}
				for windowId, windowMonitor := range self.windowMonitors {
					windowMonitorListenerIds[windowId] = maps.Keys(windowMonitor.listeners)
				}

				syncRequest := &DeviceRemoteSyncRequest{
					ProvideChangeListenerIds: maps.Keys(self.provideChangeListeners),
					ProvidePausedChangeListenerIds: maps.Keys(self.providePausedChangeListeners),
					OfflineChangeListenerIds: maps.Keys(self.offlineChangeListeners),
					ConnectChangeListenerIds: maps.Keys(self.connectChangeListeners),
					RouteLocalChangeListenerIds: maps.Keys(self.routeLocalChangeListeners),
					ConnectLocationChangeListenerIds: maps.Keys(self.connectLocationChangeListeners),
					ProvideSecretKeysListenerIds: maps.Keys(self.provideSecretKeysListeners),
					WindowMonitorEventListenerIds: windowMonitorListenerIds,
					State: self.state,
				}
				syncResponse, err := rpcCall[*DeviceRemoteSyncResponse](service, "DeviceLocalRpc.Sync", syncRequest)
				if err != nil {
					return
				}

				// trim the windows
				for windowId, windowMonitor := range self.windowMonitors {
					if !syncResponse.WindowIds[windowId] {
						delete(self.windowMonitors, windowId)
						clear(windowMonitor.listeners)
					} 
				}


				// FIXME use response cert to listen with TLS
				listenConfig := &net.ListenConfig{
					KeepAliveConfig: net.KeepAliveConfig{
						Enable: true,
					},
				}
				responseListener, err = listenConfig.Listen(handleCtx, "tcp", self.settings.ResponseAddress.HostPort())
				if err != nil {
					glog.Infof("[dr]sync reverse listen err = %s", err)
					return
				}
				

				glog.Info("[dr]start device remote rpc")
				deviceRemoteRpc := newDeviceRemoteRpc(handleCtx, self)
				server := rpc.NewServer()
				server.Register(deviceRemoteRpc)

				// defer deviceRemoteRpc.Close()
				// defer server.Close()

				go func() {
					defer func() {
						handleCancel()
						deviceRemoteRpc.Close()
					}()

					conn, err := responseListener.Accept()
					if err != nil {
						glog.Infof("[dr]sync reverse accept err = %s", err)
						return
					}
					func() {
						connCtx, connCancel := context.WithCancel(handleCtx)
						defer connCancel()
						go func() {
							defer conn.Close()
							select {
							case <- connCtx.Done():
							}
						}()
						server.ServeConn(conn)
						glog.Infof("[dr]sync reverse server done")
					}()

					// resync
				}()


				err = rpcCallVoid(service, "DeviceLocalRpc.SyncReverse", self.settings.ResponseAddress)
				if err != nil {
					return
				}


				glog.Infof("[dr]sync 1")

				self.state.Unset()
				self.service = service
				self.syncMonitor.NotifyAll()

				glog.Infof("[dr]sync 2")


				if initialLock {
					initialLock = false
					self.stateLock.Unlock()
				}

				synced = true
			}()

			if synced {
				self.remoteChanged(true)

				glog.Infof("[dr]sync done")
				select {
				case <- handleCtx.Done():
				}
				glog.Infof("[dr]handle done")

				func() {
					self.stateLock.Lock()
					defer self.stateLock.Unlock()
					self.service.Close()
					self.service = nil

					// close pending http responses
					for _, responseChannel := range self.httpResponseChannels {
						close(responseChannel)
					}
					clear(self.httpResponseChannels)
				}()

				self.remoteChanged(false)
			}


		}()

		select {
		case <- self.ctx.Done():
			return
		case <- time.After(self.settings.RpcReconnectTimeout):
		case <- notify:
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
func (self *DeviceRemote) Sync() {
	self.reconnectMonitor.NotifyAll()
}

func (self *DeviceRemote) waitForSync(timeout time.Duration) bool {
	
	synced, notify := func()(bool, chan struct{}) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.service != nil {
			return true, nil
		}
		return false, self.syncMonitor.NotifyChannel()
	}()
	if synced {
		return true
	} else if 0 <= timeout {
		select {
		case <- self.ctx.Done():
			return false
		case <- notify:
			return true
		case <- time.After(timeout):
			return false
		}
	} else {
		select {
		case <- self.ctx.Done():
			return false
		case <- notify:
			return true
		}
	}
}

func (self *DeviceRemote) getService() *rpc.Client {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.service
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

		stats, err := rpcCallNoArg[*DeviceStats](self.service, "DeviceLocalRpc.GetStats")
		if err != nil {
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

		shouldShowRatingDialog, err := rpcCallNoArg[bool](self.service, "DeviceLocalRpc.GetShouldShowRatingDialog")
		if err != nil {
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

		canShowRatingDialog, err := rpcCallNoArg[bool](self.service, "DeviceLocalRpc.GetCanShowRatingDialog")
		if err != nil {
			return false, false
		}
		return canShowRatingDialog, true
	}()
	if success {
		return canShowRatingDialog
	} else {
		return self.state.CanShowRatingDialog.Get(defaultCanShowRatingDialog)
	}
}

func (self *DeviceRemote) SetCanShowRatingDialog(canShowRatingDialog bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	success := func()(bool) {
		if self.service == nil {
			return false
		}

		err := rpcCallVoid(self.service, "DeviceLocalRpc.SetCanShowRatingDialog", canShowRatingDialog)
		if err != nil {
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

		provideWhileDisconnected, err := rpcCallNoArg[bool](self.service, "DeviceLocalRpc.GetProvideWhileDisconnected")
		if err != nil {
			return false, false
		}
		return provideWhileDisconnected, true
	}()
	if success {
		return provideWhileDisconnected
	} else {
		return self.state.ProvideWhileDisconnected.Get(defaultProvideWhileDisconnected)
	}
}

func (self *DeviceRemote) SetProvideWhileDisconnected(provideWhileDisconnected bool) {
	event := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		success := func()(bool) {
			if self.service == nil {
				return false
			}

			err := rpcCallVoid(self.service, "DeviceLocalRpc.SetProvideWhileDisconnected", provideWhileDisconnected)
			if err != nil {
				return false
			}
			return true
		}()
		if success {
			self.state.ProvideWhileDisconnected.Unset()
		} else {
			self.state.ProvideWhileDisconnected.Set(provideWhileDisconnected)
			event = true
		}
	}()
	if event {
		self.provideChanged(self.GetProvideEnabled())
	}
}

func (self *DeviceRemote) GetCanRefer() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	canRefer, success := func()(bool, bool) {
		if self.service == nil {
			return false, false
		}

		canRefer, err := rpcCallNoArg[bool](self.service, "DeviceLocalRpc.GetCanRefer")
		if err != nil {
			return false, false
		}
		return canRefer, true
	}()
	if success {
		return canRefer
	} else {	
		return self.state.CanRefer.Get(defaultCanRefer)
	}
}

func (self *DeviceRemote) SetCanRefer(canRefer bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	success := func()(bool) {
		if self.service == nil {
			return false
		}

		err := rpcCallVoid(self.service, "DeviceLocalRpc.SetCanRefer", canRefer)
		if err != nil {
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

			err := rpcCallVoid(self.service, "DeviceLocalRpc.SetRouteLocal", routeLocal)
			if err != nil {
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

		routeLocal, err := rpcCallNoArg[bool](self.service, "DeviceLocalRpc.GetRouteLocal")
		if err != nil {
			return false, false
		}
		return routeLocal, true
	}()
	if success {
		return routeLocal
	} else {
		return self.state.RouteLocal.Get(defaultRouteLocal)
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
		rpcCallVoid(deviceRemote.service, addServiceFunc, listenerId)
	}

	return newSub(func() {
		deviceRemote.stateLock.Lock()
		defer deviceRemote.stateLock.Unlock()

		delete(listeners, listenerId)
		if deviceRemote.service != nil {
			rpcCallVoid(deviceRemote.service, removeServiceFunc, listenerId)
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

func (self *DeviceRemote) AddRouteLocalChangeListener(listener RouteLocalChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.routeLocalChangeListeners,
		"DeviceLocalRpc.AddRouteLocalChangeListener",
		"DeviceLocalRpc.RemoveRouteLocalChangeListener",
	)
}

func (self *DeviceRemote) AddConnectLocationChangeListener(listener ConnectLocationChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.connectLocationChangeListeners,
		"DeviceLocalRpc.AddConnectLocationChangeListener",
		"DeviceLocalRpc.RemoveConnectLocationChangeListener",
	)
}

func (self *DeviceRemote) AddProvideSecretKeysListener(listener ProvideSecretKeysListener) Sub {
	return addListener(
		self,
		listener,
		self.provideSecretKeysListeners,
		"DeviceLocalRpc.AddProvideSecretKeysListener",
		"DeviceLocalRpc.RemoveProvideSecretKeysListener",
	)
}

func (self *DeviceRemote) LoadProvideSecretKeys(provideSecretKeyList *ProvideSecretKeyList) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	success := func()(bool) {
		if self.service == nil {
			return false
		}

		err := rpcCallVoid(self.service, "DeviceLocalRpc.LoadProvideSecretKeys", provideSecretKeyList)
		if err != nil {
			return false
		}
		return true
	}()
	if success {
		self.state.LoadProvideSecretKeys.Unset()
	} else {	
		self.state.LoadProvideSecretKeys.Set(provideSecretKeyList.getAll())
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

		err := rpcCallVoid(self.service, "DeviceLocalRpc.InitProvideSecretKeys", nil)
		if err != nil {
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

		provideEnabled, err := rpcCallNoArg[bool](self.service, "DeviceLocalRpc.GetProvideEnabled")
		if err != nil {
			return false, false
		}
		return provideEnabled, true
	}()
	if success {
		return provideEnabled
	} else {
		if self.state.ProvideWhileDisconnected.IsSet {
			return self.state.ProvideWhileDisconnected.Value
		} else {
			return false
		}
	}
}

func (self *DeviceRemote) GetConnectEnabled() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	connectEnabled, success := func()(bool, bool) {
		if self.service == nil {
			return false, false
		}

		connectEnabled, err := rpcCallNoArg[bool](self.service, "DeviceLocalRpc.GetConnectEnabled")
		if err != nil {
			return false, false
		}
		return connectEnabled, true
	}()
	if success {
		return connectEnabled
	} else {	
		return self.state.Location.IsSet || self.state.Destination.IsSet
	}
}

func (self *DeviceRemote) SetProvideMode(provideMode ProvideMode) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	success := func()(bool) {
		if self.service == nil {
			return false
		}

		err := rpcCallVoid(self.service, "DeviceLocalRpc.SetProvideMode", provideMode)
		if err != nil {
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

		provideMode, err := rpcCallNoArg[ProvideMode](self.service, "DeviceLocalRpc.GetProvideMode")
		if err != nil {
			var empty ProvideMode
			return empty, false
		}
		return provideMode, true
	}()
	if success {
		return provideMode
	} else {	
		return self.state.ProvideMode.Get(ProvideModeNone)
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

			err := rpcCallVoid(self.service, "DeviceLocalRpc.SetProvidePaused", providePaused)
			if err != nil {
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

		providePaused, err := rpcCallNoArg[bool](self.service, "DeviceLocalRpc.GetProvidePaused")
		if err != nil {
			return false, false
		}
		return providePaused, true
	}()
	if success {
		return providePaused
	} else {	
		return self.state.ProvidePaused.Get(false)
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

			err := rpcCallVoid(self.service, "DeviceLocalRpc.SetOffline", offline)
			if err != nil {
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

		offline, err := rpcCallNoArg[bool](self.service, "DeviceLocalRpc.GetOffline")
		if err != nil {
			return false, false
		}
		return offline, true
	}()
	if success {
		return offline
	} else {	
		return self.state.Offline.Get(defaultOffline)
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

			err := rpcCallVoid(self.service, "DeviceLocalRpc.SetVpnInterfaceWhileOffline", vpnInterfaceWhileOffline)
			if err != nil {
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

		vpnInterfaceWhileOffline, err := rpcCallNoArg[bool](self.service, "DeviceLocalRpc.GetVpnInterfaceWhileOffline")
		if err != nil {
			return false, false
		}
		return vpnInterfaceWhileOffline, true
	}()
	if success {
		return vpnInterfaceWhileOffline
	} else {	
		return self.state.VpnInterfaceWhileOffline.Get(defaultVpnInterfaceWhileOffline)
	}
}

func (self *DeviceRemote) RemoveDestination() {
	event := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		success := func()(bool) {
			if self.service == nil {
				return false
			}

			err := rpcCallVoid(self.service, "DeviceLocalRpc.RemoveDestination", nil)
			if err != nil {
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
	}()
	if event {
		self.connectChanged(self.GetConnectEnabled())
	}
}

func (self *DeviceRemote) SetDestination(location *ConnectLocation, specs *ProviderSpecList, provideMode ProvideMode) {
	event := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		destination := &DeviceRemoteDestination{
			Location: newDeviceRemoteConnectLocation(location),
			Specs: specs.getAll(),
			ProvideMode: provideMode,
		}

		success := func()(bool) {
			if self.service == nil {
				return false
			}

			err := rpcCallVoid(self.service, "DeviceLocalRpc.SetDestination", destination)
			if err != nil {
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
			event = true
		}
	}()
	if event {
		self.connectChanged(self.GetConnectEnabled())
	}
}

func (self *DeviceRemote) SetConnectLocation(location *ConnectLocation) {
	deviceRemoteLocation := newDeviceRemoteConnectLocation(location)
	event := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		success := func()(bool) {
			if self.service == nil {
				return false
			}

			err := rpcCallVoid(self.service, "DeviceLocalRpc.SetConnectLocation", deviceRemoteLocation)
			if err != nil {
				return false
			}
			return true
		}()
		if success {
			self.state.Location.Unset()
		} else {
			self.state.Location.Set(deviceRemoteLocation)
			self.state.RemoveDestination.Unset()
			self.state.Destination.Unset()
			event = true
		}
	}()
	if event {
		self.connectChanged(self.GetConnectEnabled())
	}
}

func (self *DeviceRemote) GetConnectLocation() *ConnectLocation {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	location, success := func()(*ConnectLocation, bool) {
		if self.service == nil {
			return nil, false
		}

		deviceRemoteLocation, err := rpcCallNoArg[*DeviceRemoteConnectLocation](self.service, "DeviceLocalRpc.GetConnectLocation")
		if err != nil {
			return nil, false
		}
		return deviceRemoteLocation.toConnectLocation(), true
	}()
	if success {
		return location
	} else {
		if self.state.Location.IsSet {
			return self.state.Location.Value.toConnectLocation()
		} else if self.state.Destination.IsSet {
			return self.state.Destination.Value.Location.toConnectLocation()
		} else {
			return nil
		}
	}
}

func (self *DeviceRemote) Shuffle() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	success := func()(bool) {
		if self.service == nil {
			return false
		}

		err := rpcCallVoid(self.service, "DeviceLocalRpc.Shuffle", nil)
		if err != nil {
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

func (self *DeviceRemote) Cancel() {
	self.cancel()
}

func (self *DeviceRemote) Close() {
	// self.stateLock.Lock()
	// defer self.stateLock.Unlock()

	self.cancel()

	// if self.service != nil {
	// 	self.service.Close()
	// 	self.service = nil
	// }
}

func (self *DeviceRemote) GetDone() bool {
	select {
	case <-self.ctx.Done():
		return true
	default:
		return false
	}
}


func (self *DeviceRemote) windowMonitor() windowMonitor {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	windowMonitor := newDeviceRemoteWindowMonitor(self)
	self.windowMonitors[windowMonitor.windowId] = windowMonitor
	return windowMonitor
}

func (self *DeviceRemote) windowMonitorAddMonitorEventCallback(windowMonitor *deviceRemoteWindowMonitor, monitorEventCallback connect.MonitorEventFunction) func() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	_, ok := self.windowMonitors[windowMonitor.windowId]
	if !ok {
		// the window is not longer active
		return func() {}
	}

	listenerId := connect.NewId()
	windowMonitor.listeners[listenerId] = monitorEventCallback

	if self.service != nil {
		func() {
			windowListenerId := &DeviceRemoteWindowListenerId{
				WindowId: windowMonitor.windowId,
				ListenerId: listenerId,
			}
			windowIds, err := rpcCall[map[connect.Id]bool](self.service, "DeviceLocalRpc.AddWindowMonitorEventListener", windowListenerId)
			if err != nil {
				return
			}

			// trim the windows
			for windowId, windowMonitor := range self.windowMonitors {
				if !windowIds[windowId] {
					delete(self.windowMonitors, windowId)
					clear(windowMonitor.listeners)
				} 
			}
		}()
	}

	return func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		delete(windowMonitor.listeners, listenerId)

		if self.service != nil {
			func() {
				windowListenerId := &DeviceRemoteWindowListenerId{
					WindowId: windowMonitor.windowId,
					ListenerId: listenerId,
				}
				err := rpcCallVoid(self.service, "DeviceLocalRpc.RemoveWindowMonitorEventListener", windowListenerId)
				if err != nil {
					return
				}
			}()
		}
	}
}

func (self *DeviceRemote) windowMonitorEvents(windowMonitor *deviceRemoteWindowMonitor) (*connect.WindowExpandEvent, map[connect.Id]*connect.ProviderEvent) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	_, ok := self.windowMonitors[windowMonitor.windowId]
	if !ok {
		// window no longer active
		return &connect.WindowExpandEvent{}, map[connect.Id]*connect.ProviderEvent{}
	}

	if self.service == nil {
		return &connect.WindowExpandEvent{}, map[connect.Id]*connect.ProviderEvent{}
	}

	event, err := rpcCallNoArg[*DeviceRemoteWindowMonitorEvent](self.service, "DeviceLocalRpc.WindowMonitorEvents")
	if err != nil {
		return &connect.WindowExpandEvent{}, map[connect.Id]*connect.ProviderEvent{}
	}
	if event == nil {
		return &connect.WindowExpandEvent{}, map[connect.Id]*connect.ProviderEvent{}
	}
	return event.WindowExpandEvent, event.ProviderEvents
}


// this object is locked under the DeviceRemote.stateLock
type deviceRemoteWindowMonitor struct {
	deviceRemote *DeviceRemote

	windowId connect.Id
	listeners map[connect.Id]connect.MonitorEventFunction
}

func newDeviceRemoteWindowMonitor(deviceRemote *DeviceRemote) *deviceRemoteWindowMonitor {
	windowId := connect.NewId()

	return &deviceRemoteWindowMonitor{
		deviceRemote: deviceRemote,
		windowId: windowId,
		listeners: map[connect.Id]connect.MonitorEventFunction{},
	}
}

// windowMonitor

func (self *deviceRemoteWindowMonitor) AddMonitorEventCallback(monitorEventCallback connect.MonitorEventFunction) func() {
	return self.deviceRemote.windowMonitorAddMonitorEventCallback(self, monitorEventCallback)
}

func (self *deviceRemoteWindowMonitor) Events() (*connect.WindowExpandEvent, map[connect.Id]*connect.ProviderEvent) {
	return self.deviceRemote.windowMonitorEvents(self)
}


// event dispatch

func listenerList[T any](listenerMap map[connect.Id]T) []T {
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
	listenerList := func()[]ProvideChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return listenerList(self.provideChangeListeners)
	}()
	glog.Infof("!!DISPATCH provideChanged")
	for i, provideChangeListener := range listenerList {
		glog.Infof("!!DISPATCH provideChanged [%d]", i)
		provideChangeListener.ProvideChanged(provideEnabled)
	}
}

func (self *DeviceRemote) providePausedChanged(providePaused bool) {
	listenerList := func()[]ProvidePausedChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return listenerList(self.providePausedChangeListeners)
	}()
	for _, providePausedChangeListener := range listenerList {
		providePausedChangeListener.ProvidePausedChanged(providePaused)
	}
}

func (self *DeviceRemote) offlineChanged(offline bool, vpnInterfaceWhileOffline bool) {
	listenerList := func()[]OfflineChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return listenerList(self.offlineChangeListeners)
	}()
	for _, offlineChangeListener := range listenerList {
		offlineChangeListener.OfflineChanged(offline, vpnInterfaceWhileOffline)
	}
}

func (self *DeviceRemote) connectChanged(connectEnabled bool) {
	listenerList := func()[]ConnectChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return listenerList(self.connectChangeListeners)
	}()
	for _, connectChangeListener := range listenerList {
		connectChangeListener.ConnectChanged(connectEnabled)
	}
}

func (self *DeviceRemote) routeLocalChanged(routeLocal bool) {
	listenerList := func()[]RouteLocalChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return listenerList(self.routeLocalChangeListeners)
	}()
	for _, routeLocalChangeListener := range listenerList {
		routeLocalChangeListener.RouteLocalChanged(routeLocal)
	}
}

func (self *DeviceRemote) connectLocationChanged(location *ConnectLocation) {
	listenerList := func()[]ConnectLocationChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return listenerList(self.connectLocationChangeListeners)
	}()
	for _, connectLocationChangeListener := range listenerList {
		connectLocationChangeListener.ConnectLocationChanged(location)
	}
}

func (self *DeviceRemote) provideSecretKeysChanged(provideSecretKeyList *ProvideSecretKeyList) {
	listenerList := func()[]ProvideSecretKeysListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return listenerList(self.provideSecretKeysListeners)
	}()
	for _, provideSecretKeyListener := range listenerList {
		provideSecretKeyListener.ProvideSecretKeysChanged(provideSecretKeyList)
	}
}

func (self *DeviceRemote) windowMonitorEvent(
	windowIds map[connect.Id]bool,
	windowExpandEvent *connect.WindowExpandEvent,
	providerEvents map[connect.Id]*connect.ProviderEvent,
) {
	listenerLists := [][]connect.MonitorEventFunction{}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		
		for windowId, _ := range windowIds {
			windowMonitor := self.windowMonitors[windowId]
			listenerLists = append(listenerLists, listenerList(windowMonitor.listeners))
		}
	}()
	for _, listenerList := range listenerLists {
		for _, monitorEventCallback := range listenerList {
			monitorEventCallback(windowExpandEvent, providerEvents)
		}
	}
}


// safe to call on multiple goroutines
func (self *DeviceRemote) httpPostRaw(ctx context.Context, requestUrl string, requestBodyBytes []byte, byJwt string) ([]byte, error) {
	// if server is set, use remote
	// else use local

	requestCtx, requestCancel := context.WithCancel(ctx)
	defer requestCancel()
	go func() {
		defer requestCancel()
		select {
		case <- self.ctx.Done():
		case <- requestCtx.Done():
		}
	}()

	service := self.getService()

	if service != nil {
		httpRequestId := connect.NewId()
		httpRequest := &DeviceRemoteHttpRequest{
			RequestId: httpRequestId,
			RequestUrl: requestUrl,
			RequestBodyBytes: requestBodyBytes,
			ByJwt: byJwt,
		}

		httpResponseChannel := make(chan *DeviceRemoteHttpResponse)

		var err error
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			
			err = rpcCallVoid(service, "DeviceLocalRpc.HttpPostRaw", httpRequest)
			if err != nil {
				close(httpResponseChannel)
				return
			}

			self.httpResponseChannels[httpRequestId] = httpResponseChannel
		}()

		if err != nil {
			return nil, err
		}

		select {
		case httpResponse, ok := <- httpResponseChannel:
			if !ok {
				return nil, fmt.Errorf("Done")
			}
			return httpResponse.BodyBytes, httpResponse.toError()
		case <- requestCtx.Done():
			return nil, fmt.Errorf("Done")
		}
	} else {
		return connect.HttpPostWithStrategyRaw(
			requestCtx,
			self.clientStrategy,
			requestUrl,
			requestBodyBytes,
			byJwt,
		)
	}
}

// safe to call on multiple goroutines
func (self *DeviceRemote) httpGetRaw(ctx context.Context, requestUrl string, byJwt string) ([]byte, error) {
	// if server is set, use remote
	// else use local

	requestCtx, requestCancel := context.WithCancel(ctx)
	defer requestCancel()
	go func() {
		defer requestCancel()
		select {
		case <- self.ctx.Done():
		case <- requestCtx.Done():
		}
	}()

	service := self.getService()

	if service != nil {
		httpRequestId := connect.NewId()
		httpRequest := &DeviceRemoteHttpRequest{
			RequestId: httpRequestId,
			RequestUrl: requestUrl,
			ByJwt: byJwt,
		}

		httpResponseChannel := make(chan *DeviceRemoteHttpResponse)

		var err error
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			
			err = rpcCallVoid(service, "DeviceLocalRpc.HttpGetRaw", httpRequest)
			if err != nil {
				close(httpResponseChannel)
				return
			}

			self.httpResponseChannels[httpRequestId] = httpResponseChannel
		}()

		if err != nil {
			return nil, err
		}

		select {
		case httpResponse, ok := <- httpResponseChannel:
			if !ok {
				return nil, fmt.Errorf("Done")
			}
			return httpResponse.BodyBytes, httpResponse.toError()
		case <- requestCtx.Done():
			return nil, fmt.Errorf("Done")
		}
	} else {
		return connect.HttpGetWithStrategyRaw(
			requestCtx,
			self.clientStrategy,
			requestUrl,
			byJwt,
		)
	}
}

func (self *DeviceRemote) httpResponse(httpResponse *DeviceRemoteHttpResponse) {
	httpRequestId := httpResponse.RequestId

	var ok bool
	var httpResponseChannel chan *DeviceRemoteHttpResponse
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		httpResponseChannel, ok = self.httpResponseChannels[httpRequestId]
		if ok {
			delete(self.httpResponseChannels, httpRequestId)
		}
	}()
	if !ok {
		return
	}

	select {
	case httpResponseChannel <- httpResponse:
	default:
		// no one is still listening, drop the response	
	}
	close(httpResponseChannel)
}

func (self *DeviceRemote) AddRemoteChangeListener(listener RemoteChangeListener) Sub {
	listenerId := self.remoteChangeListeners.Add(listener)
	return newSub(func() {
		self.remoteChangeListeners.Remove(listenerId)
	})
}

func (self *DeviceRemote) remoteChanged(remoteConnected bool) {
	for _, listener := range self.remoteChangeListeners.Get() {
		listener.RemoteChanged(remoteConnected)
	}
}




// *important rpc note* gob encoding cannot encode fields that are not exported
// so our usual gomobile types that have private fields cannot be properly sent via rpc
// for rpc we redefine these gomobile types so that they can be gob encoded

// *important type note*
// all of the types below here should *not* be exported by gomobile
// we use a made-up annotation gomobile:noexport to try to document this
// however, the types must be exported for net.rpc to work
// this leads to some unfortunate gomobile warnings currently



//gomobile:noexport
type DeviceRemoteDestination struct {
	Location *DeviceRemoteConnectLocation
	Specs []*ProviderSpec
	ProvideMode ProvideMode
}

//gomobile:noexport
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

func (self *deviceRemoteValue[T]) Get(defaultValue T) T {
	if self.IsSet {
		return self.Value
	} else {
		return defaultValue
	}
}


//gomobile:noexport
type DeviceRemoteAddress struct {
	Ip netip.Addr
	Port int
}

func parseDeviceRemoteAddress(hostPort string) (*DeviceRemoteAddress, error) {
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
	return &DeviceRemoteAddress{
		Ip: ip,
		Port: port,
	}, nil
}

func requireRemoteAddress(hostPort string) *DeviceRemoteAddress {
	address, err := parseDeviceRemoteAddress(hostPort)
	if err != nil {
		panic(err)
	}
	return address
}

func (self *DeviceRemoteAddress) HostPort() string {
	return net.JoinHostPort(self.Ip.String(), strconv.Itoa(self.Port))
}


//gomobile:noexport
type DeviceRemoteOfflineChangeEvent struct {
	Offline bool
	VpnInterfaceWhileOffline bool
}

//gomobile:noexport
type DeviceRemoteConnectLocationChangeEvent struct {
	Location *DeviceRemoteConnectLocation
}


//gomobile:noexport
type DeviceRemoteState struct {
	CanShowRatingDialog deviceRemoteValue[bool] 
	ProvideWhileDisconnected deviceRemoteValue[bool]
	CanRefer deviceRemoteValue[bool]
	RouteLocal deviceRemoteValue[bool]
	InitProvideSecretKeys deviceRemoteValue[bool]
	LoadProvideSecretKeys deviceRemoteValue[[]*ProvideSecretKey]
	ProvideMode deviceRemoteValue[ProvideMode]
	ProvidePaused deviceRemoteValue[bool]
	Offline deviceRemoteValue[bool]
	VpnInterfaceWhileOffline deviceRemoteValue[bool]
	RemoveDestination deviceRemoteValue[bool]
	Destination deviceRemoteValue[*DeviceRemoteDestination]
	Location deviceRemoteValue[*DeviceRemoteConnectLocation]
	Shuffle deviceRemoteValue[bool]
}

func (self *DeviceRemoteState) Unset() {
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


//gomobile:noexport
type DeviceRemoteSyncRequest struct {
	ProvideChangeListenerIds []connect.Id
	ProvidePausedChangeListenerIds []connect.Id
	OfflineChangeListenerIds []connect.Id
	ConnectChangeListenerIds []connect.Id
	RouteLocalChangeListenerIds []connect.Id
	ConnectLocationChangeListenerIds []connect.Id
	ProvideSecretKeysListenerIds []connect.Id
	WindowMonitorEventListenerIds map[connect.Id][]connect.Id
	State DeviceRemoteState
}


//gomobile:noexport
type DeviceRemoteSyncResponse struct {
	WindowIds map[connect.Id]bool
	// FIXME response cert
}


//gomobile:noexport
type DeviceRemoteWindowListenerId struct {
	WindowId connect.Id
	ListenerId connect.Id
}


//gomobile:noexport
type DeviceRemoteWindowMonitorEvent struct {
	WindowIds map[connect.Id]bool
	WindowExpandEvent *connect.WindowExpandEvent
	ProviderEvents map[connect.Id]*connect.ProviderEvent
}



//gomobile:noexport
type DeviceRemoteConnectLocation struct {
	ConnectLocation *DeviceRemoteConnectLocationValue
}

func newDeviceRemoteConnectLocation(connectLocation *ConnectLocation) *DeviceRemoteConnectLocation {
	deviceRemoteConnectLocation := &DeviceRemoteConnectLocation{}
	if connectLocation != nil {
		deviceRemoteConnectLocation.ConnectLocation = newDeviceRemoteConnectLocationValue(connectLocation)
	}
	return deviceRemoteConnectLocation
}

func (self *DeviceRemoteConnectLocation) toConnectLocation() *ConnectLocation {
	if self.ConnectLocation == nil {
		return nil
	}
	return self.ConnectLocation.toConnectLocation()
}

//gomobile:noexport
type DeviceRemoteConnectLocationValue struct {
	ConnectLocationId *DeviceRemoteConnectLocationId
	Name              string

	ProviderCount int32
	Promoted      bool
	MatchDistance int32

	LocationType LocationType

	City        string
	Region      string
	Country     string
	CountryCode string

	CityLocationId    *connect.Id
	RegionLocationId  *connect.Id
	CountryLocationId *connect.Id
}

func newDeviceRemoteConnectLocationValue(connectLocation *ConnectLocation) *DeviceRemoteConnectLocationValue {
	deviceRemoteConnectLocationValue := &DeviceRemoteConnectLocationValue{
		Name: connectLocation.Name,

		ProviderCount: connectLocation.ProviderCount,
		Promoted: connectLocation.Promoted,
		MatchDistance: connectLocation.MatchDistance,

		LocationType: connectLocation.LocationType,

		City: connectLocation.City,
		Region: connectLocation.Region,
		Country: connectLocation.Country,
		CountryCode: connectLocation.CountryCode,
	}
	if connectLocation.ConnectLocationId != nil {
		deviceRemoteConnectLocationValue.ConnectLocationId = newDeviceRemoteConnectLocationId(connectLocation.ConnectLocationId)
	}
	if connectLocation.CityLocationId != nil {
		id := connectLocation.CityLocationId.toConnectId()
		deviceRemoteConnectLocationValue.CityLocationId = &id
	}
	if connectLocation.RegionLocationId != nil {
		id := connectLocation.RegionLocationId.toConnectId()
		deviceRemoteConnectLocationValue.RegionLocationId = &id
	}
	if connectLocation.CountryLocationId != nil {
		id := connectLocation.CountryLocationId.toConnectId()
		deviceRemoteConnectLocationValue.CountryLocationId = &id
	}
	return deviceRemoteConnectLocationValue
}

func (self *DeviceRemoteConnectLocationValue) toConnectLocation() *ConnectLocation {
	connectLocation := &ConnectLocation{
		Name: self.Name,

		ProviderCount: self.ProviderCount,
		Promoted: self.Promoted,
		MatchDistance: self.MatchDistance,

		LocationType: self.LocationType,

		City: self.City,
		Region: self.Region,
		Country: self.Country,
		CountryCode: self.CountryCode,
	}
	if self.ConnectLocationId != nil {
		connectLocation.ConnectLocationId = self.ConnectLocationId.toConnectLocationId()
	}
	if self.CityLocationId != nil {
		connectLocation.CityLocationId = newId(*self.CityLocationId)
	}
	if self.RegionLocationId != nil {
		connectLocation.RegionLocationId = newId(*self.RegionLocationId)
	}
	if connectLocation.CountryLocationId != nil {
		connectLocation.CountryLocationId = newId(*self.CountryLocationId)
	}
	return connectLocation
}

//gomobile:noexport
type DeviceRemoteConnectLocationId struct {
	ClientId        *connect.Id
	LocationId      *connect.Id
	LocationGroupId *connect.Id
	BestAvailable   bool
}

func newDeviceRemoteConnectLocationId(connectLocationId *ConnectLocationId) *DeviceRemoteConnectLocationId {
	deviceRemoteConnectLocationId := &DeviceRemoteConnectLocationId{
		BestAvailable: connectLocationId.BestAvailable,
	}
	if connectLocationId.ClientId != nil {
		id := connectLocationId.ClientId.toConnectId()
		deviceRemoteConnectLocationId.ClientId = &id
	}
	if connectLocationId.LocationId != nil {
		id := connectLocationId.LocationId.toConnectId()
		deviceRemoteConnectLocationId.LocationId = &id
	}
	if connectLocationId.LocationGroupId != nil {
		id := connectLocationId.LocationGroupId.toConnectId()
		deviceRemoteConnectLocationId.LocationGroupId = &id
	}
	return deviceRemoteConnectLocationId
}

func (self *DeviceRemoteConnectLocationId) toConnectLocationId() *ConnectLocationId {
	connectLocationId := &ConnectLocationId{
		BestAvailable: self.BestAvailable,
	}
	if self.ClientId != nil {
		connectLocationId.ClientId = newId(*self.ClientId)
	}
	if self.LocationId != nil {
		connectLocationId.LocationId = newId(*self.LocationId)
	}
	if self.LocationGroupId != nil {
		connectLocationId.LocationGroupId =newId(*self.LocationGroupId)
	}
	return connectLocationId
}




// rpc wrappers

type RpcVoid = *any
type RpcNoArg = int

func rpcCallVoid(service *rpc.Client, name string, arg any) error {
	var void RpcVoid
	glog.Infof("[rpc]%s", name)
	err := service.Call(name, arg, &void)
	if err != nil {
		glog.Infof("[rpc]%s err = %s", name, err)
		service.Close()
	}
	return err
}

func rpcCallNoArgVoid(service *rpc.Client, name string) error {
	var noarg RpcNoArg
	var void RpcVoid
	glog.Infof("[rpc]%s", name)
	err := service.Call(name, noarg, &void)
	if err != nil {
		glog.Infof("[rpc]%s err = %s", name, err)
		service.Close()
	}
	return err
}

func rpcCallNoArg[T any](service *rpc.Client, name string) (T, error) {
	var noarg RpcNoArg
	var r T
	glog.Infof("[rpc]%s", name)
	err := service.Call(name, noarg, &r)
	if err != nil {
		glog.Infof("[rpc]%s err = %s", name, err)
		service.Close()
	}
	return r, err
}

func rpcCall[T any](service *rpc.Client, name string, arg any) (T, error) {
	var r T
	glog.Infof("[rpc]%s", name)
	err := service.Call(name, arg, &r)
	if err != nil {
		glog.Infof("[rpc]%s err = %s", name, err)
		service.Close()
	}
	return r, err
}


//gomobile:noexport
type DeviceRemoteHttpRequest struct {
	RequestId connect.Id
	RequestUrl string
	RequestBodyBytes []byte
	ByJwt string
}


//gomobile:noexport
type DeviceRemoteHttpResponse struct {
	RequestId connect.Id
	BodyBytes []byte
	Error *DeviceRemoteHttpResponseError
}

func newDeviceRemoteHttpResponse(requestId connect.Id, bodyBytes []byte, err error) *DeviceRemoteHttpResponse {
	httpResponse := &DeviceRemoteHttpResponse{
		RequestId: requestId,
		BodyBytes: bodyBytes,
	}
	if err != nil {
		httpResponse.Error = &DeviceRemoteHttpResponseError{
			Error: fmt.Sprintf("%v", err),
		}
	}
	return httpResponse
}

func (self *DeviceRemoteHttpResponse) toError() error {
	if self.Error != nil {
		return fmt.Errorf("%s", self.Error.Error)
	}
	return nil
}

//gomobile:noexport
type DeviceRemoteHttpResponseError struct {
	Error string
}


// rpc are called on a single go routine

//gomobile:noexport
type DeviceLocalRpc struct {
	ctx context.Context
	cancel context.CancelFunc

	deviceLocal *DeviceLocal
	settings *deviceRpcSettings

	stateLock sync.Mutex

	provideChangeListenerIds map[connect.Id]bool
	providePausedChangeListenerIds map[connect.Id]bool
	offlineChangeListenerIds map[connect.Id]bool
	connectChangeListenerIds map[connect.Id]bool
	routeLocalChangeListenerIds map[connect.Id]bool
	connectLocationChangeListenerIds map[connect.Id]bool
	provideSecretKeysListenerIds map[connect.Id]bool


	// window id -> listener id
	windowMonitorEventListenerIds map[connect.Id]map[connect.Id]bool
	// local window id -> window id
	localWindowIds map[connect.Id]connect.Id
	localWindowMonitor windowMonitor
	localWindowId connect.Id


	provideChangeListenerSub Sub
	providePausedChangeListenerSub Sub
	offlineChangeListenerSub Sub
	connectChangeListenerSub Sub
	routeLocalChangeListenerSub Sub
	connectLocationChangeListenerSub Sub
	provideSecretKeysListenerSub Sub
	windowMonitorEventListenerSub func()

	service *rpc.Client
}

func newDeviceLocalRpcWithDefaults(
	ctx context.Context,
	deviceLocal *DeviceLocal,
) *DeviceLocalRpc {
	return newDeviceLocalRpc(ctx, deviceLocal, defaultDeviceRpcSettings())
}

func newDeviceLocalRpc(
	ctx context.Context,
	deviceLocal *DeviceLocal,
	settings *deviceRpcSettings,
) *DeviceLocalRpc {
	cancelCtx, cancel := context.WithCancel(ctx)

	deviceLocalRpc := &DeviceLocalRpc{
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
		windowMonitorEventListenerIds: map[connect.Id]map[connect.Id]bool{},
		localWindowIds: map[connect.Id]connect.Id{},
	}

	go deviceLocalRpc.run()
	return deviceLocalRpc
}

func (self *DeviceLocalRpc) run() {
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
				glog.Infof("[dlrcp]listen err = %s", err)
				return
			}

			defer listener.Close()

			server := rpc.NewServer()
			server.Register(self)

			go func() {
				defer handleCancel()

				// handle connections serially
				for {
					select {
					case <- handleCtx.Done():
						return
					default:
					}

					conn, err := listener.Accept()
					if err != nil {
						glog.Infof("[dlrcp]listen accept err = %s", err)
						return
					}

					func() {
						connCtx, connCancel := context.WithCancel(handleCtx)
						defer connCancel()
						go func() {
							defer conn.Close()
							select {
							case <- connCtx.Done():
							}
						}()
						server.ServeConn(conn)
						glog.Infof("[dlrcp]server conn done")

						if self.service != nil {
							self.service.Close()
							self.service = nil
						}
					}()

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

func (self *DeviceLocalRpc) Sync(
	syncRequest *DeviceRemoteSyncRequest,
	syncResponse **DeviceRemoteSyncResponse,
) error {
	// defer func() {
	// 	if r := recover(); r != nil {
	// 		debug.PrintStack()
	// 		panic(r)
	// 	}
	// }()

	glog.Infof("s1")

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	// apply state adjustments

	state := syncRequest.State

	glog.Infof("s2")
	if state.CanShowRatingDialog.IsSet {
		self.deviceLocal.SetCanShowRatingDialog(state.CanShowRatingDialog.Value)
	}
	glog.Infof("s2")
	if state.ProvideWhileDisconnected.IsSet {
		self.deviceLocal.SetProvideWhileDisconnected(state.ProvideWhileDisconnected.Value)
	}
	glog.Infof("s3")
	if state.CanRefer.IsSet {
		self.deviceLocal.SetCanRefer(state.CanRefer.Value)
	}
	glog.Infof("s4")
	if state.RouteLocal.IsSet {
		self.deviceLocal.SetRouteLocal(state.RouteLocal.Value)
	}
	glog.Infof("s5")
	if state.InitProvideSecretKeys.IsSet {
		self.deviceLocal.InitProvideSecretKeys()
	}
	glog.Infof("s6")
	if state.LoadProvideSecretKeys.IsSet {
		provideSecretKeyList := NewProvideSecretKeyList()
		provideSecretKeyList.addAll(state.LoadProvideSecretKeys.Value...)
		self.deviceLocal.LoadProvideSecretKeys(provideSecretKeyList)
	}
	glog.Infof("s7")
	if state.ProvideMode.IsSet {
		self.deviceLocal.SetProvideMode(state.ProvideMode.Value)
	}
	glog.Infof("s8")
	if state.ProvidePaused.IsSet {
		self.deviceLocal.SetProvidePaused(state.ProvidePaused.Value)
	}
	glog.Infof("s9")
	if state.Offline.IsSet {
		self.deviceLocal.SetOffline(state.Offline.Value)
	}
	glog.Infof("s10")
	if state.VpnInterfaceWhileOffline.IsSet {
		self.deviceLocal.SetVpnInterfaceWhileOffline(state.VpnInterfaceWhileOffline.Value)
	}
	glog.Infof("s11")
	if state.RemoveDestination.IsSet {
		self.deviceLocal.RemoveDestination()
	}
	glog.Infof("s12")
	if state.Destination.IsSet {
		destination := state.Destination.Value
		providerSpecList := NewProviderSpecList()
		providerSpecList.addAll(destination.Specs...)
		self.deviceLocal.SetDestination(
			destination.Location.toConnectLocation(),
			providerSpecList,
			destination.ProvideMode,
		)
	}
	glog.Infof("s13")
	if state.Location.IsSet {
		self.deviceLocal.SetConnectLocation(state.Location.Value.toConnectLocation())
	}
	glog.Infof("s14")
	if state.Shuffle.IsSet {
		self.deviceLocal.Shuffle()
	}

	glog.Infof("s15")
	self.updateWindowMonitor(true)



	// add listeners
	glog.Infof("s16")
	for provideChangeListenerId, _ := range self.provideChangeListenerIds {
		self.removeProvideChangeListener(provideChangeListenerId)
	}
	glog.Infof("s16b")
	for _, provideChangeListenerId := range syncRequest.ProvideChangeListenerIds {
		self.addProvideChangeListener(provideChangeListenerId)
	}

	glog.Infof("s17")
	for providePausedChangeListenerId, _ := range self.providePausedChangeListenerIds {
		self.removeProvidePausedChangeListener(providePausedChangeListenerId)
	}
	for _, providePausedChangeListenerId := range syncRequest.ProvidePausedChangeListenerIds {
		self.addProvidePausedChangeListener(providePausedChangeListenerId)
	}

	glog.Infof("s18")
	for offlineChangeListenerId, _ := range self.offlineChangeListenerIds {
		self.removeOfflineChangeListener(offlineChangeListenerId)
	}
	for _, offlineChangeListenerId := range syncRequest.OfflineChangeListenerIds {
		self.addOfflineChangeListener(offlineChangeListenerId)
	}

	glog.Infof("s19")
	for connectChangeListenerId, _ := range self.connectChangeListenerIds {
		self.removeConnectChangeListener(connectChangeListenerId)
	}
	for _, connectChangeListenerId := range syncRequest.ConnectChangeListenerIds {
		self.addConnectChangeListener(connectChangeListenerId)
	}

	glog.Infof("s20")
	for routeLocalChangeListenerId, _ := range self.routeLocalChangeListenerIds {
		self.removeRouteLocalChangeListener(routeLocalChangeListenerId)
	}
	for _, routeLocalChangeListenerId := range syncRequest.RouteLocalChangeListenerIds {
		self.addRouteLocalChangeListener(routeLocalChangeListenerId)
	}

	glog.Infof("s21")
	for connectLocationChangeListenerId, _ := range self.connectLocationChangeListenerIds {
		self.removeConnectLocationChangeListener(connectLocationChangeListenerId)
	}
	for _, connectLocationChangeListenerId := range syncRequest.ConnectLocationChangeListenerIds {
		self.addConnectLocationChangeListener(connectLocationChangeListenerId)
	}

	glog.Infof("s22")
	for provideSecretKeysListenerId, _ := range self.provideSecretKeysListenerIds {
		self.removeProvideSecretKeysListener(provideSecretKeysListenerId)
	}
	for _, provideSecretKeysListenerId := range syncRequest.ProvideSecretKeysListenerIds {
		self.addProvideSecretKeysListener(provideSecretKeysListenerId)
	}

	glog.Infof("s23")
	for windowId, windowMonitorEventListenerIds := range self.windowMonitorEventListenerIds {
		for windowMonitorEventListenerId, _ := range windowMonitorEventListenerIds {
			windowListenerId := DeviceRemoteWindowListenerId{
				WindowId: windowId, 
				ListenerId: windowMonitorEventListenerId,
			}
			self.removeWindowMonitorEventListener(windowListenerId)
		}
	}
	for windowId, windowMonitorEventListenerIds := range syncRequest.WindowMonitorEventListenerIds {
		for _, windowMonitorEventListenerId := range windowMonitorEventListenerIds {
			windowListenerId := DeviceRemoteWindowListenerId{
				WindowId: windowId, 
				ListenerId: windowMonitorEventListenerId,
			}
			self.addWindowMonitorEventListener(windowListenerId)
		}
	}


 	glog.Infof("s33")
 	*syncResponse = &DeviceRemoteSyncResponse{
 		WindowIds: self.windowIds(),
 	}
 	glog.Infof("s34")
	return nil
}

func (self *DeviceLocalRpc) SyncReverse(responseAddress *DeviceRemoteAddress, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

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
	glog.Infof("[dlrpc]sync reverse")
	conn, err := dialer.DialContext(self.ctx, "tcp", responseAddress.HostPort())
	if err != nil {
		glog.Infof("[dlrpc]sync reverse err = %s", err)
		return err
	}
	// FIXME
	// tls.Handshake()

	select {
	case <- self.ctx.Done():
		return fmt.Errorf("Done")
	default:
	}

	glog.Infof("[dlrpc]sync reverse connected")

	self.service = rpc.NewClient(conn)



	// fire listeners with the current state
	
	glog.Infof("s24")
	if self.provideChangeListenerSub != nil {
		self.provideChanged(self.deviceLocal.GetProvideEnabled())
	}
	glog.Infof("s25")
	if self.providePausedChangeListenerSub != nil {
		self.providePausedChanged(self.deviceLocal.GetProvidePaused())
	}
	glog.Infof("s26")
	if self.offlineChangeListenerSub != nil {
		self.offlineChanged(self.deviceLocal.GetOffline(), self.deviceLocal.GetVpnInterfaceWhileOffline())
	}
	glog.Infof("s27")
	if self.connectChangeListenerSub != nil {
		self.connectChanged(self.deviceLocal.GetConnectEnabled())
	}
	glog.Infof("s28")
	if self.routeLocalChangeListenerSub != nil {
		self.routeLocalChanged(self.deviceLocal.GetRouteLocal())
	}
	glog.Infof("s29")
	if self.connectLocationChangeListenerSub != nil {
		self.connectLocationChanged(self.deviceLocal.GetConnectLocation())
	}
	glog.Infof("s31")
	if self.provideSecretKeysListenerSub != nil {
		self.provideSecretKeysChanged(self.deviceLocal.GetProvideSecretKeys())
	}
	glog.Infof("s32")
	if self.localWindowMonitor != nil && self.windowMonitorEventListenerSub != nil {
		self.windowMonitorEventCallback(self.localWindowMonitor.Events())
	}
 	

	return nil
}

func (self *DeviceLocalRpc) GetStats(_ RpcNoArg, stats **DeviceStats) error {
	*stats = self.deviceLocal.GetStats()
	return nil
}

func (self *DeviceLocalRpc) GetShouldShowRatingDialog(_ RpcNoArg, shouldShowRatingDialog *bool) error {
	*shouldShowRatingDialog = self.deviceLocal.GetShouldShowRatingDialog()
	return nil
}

func (self *DeviceLocalRpc) GetCanShowRatingDialog(_ RpcNoArg, canShowRatingDialog *bool) error {
	*canShowRatingDialog = self.deviceLocal.GetCanShowRatingDialog()
	return nil
}

func (self *DeviceLocalRpc) SetCanShowRatingDialog(canShowRatingDialog bool, _ RpcVoid) error {
	self.deviceLocal.SetCanShowRatingDialog(canShowRatingDialog)
	return nil
} 

func (self *DeviceLocalRpc) GetProvideWhileDisconnected(_ RpcNoArg, provideWhileDisconnected *bool) error {
	*provideWhileDisconnected = self.deviceLocal.GetProvideWhileDisconnected()
	return nil
}

func (self *DeviceLocalRpc) SetProvideWhileDisconnected(provideWhileDisconnected bool, _ RpcVoid) error {
	self.deviceLocal.SetProvideWhileDisconnected(provideWhileDisconnected)
	return nil
}

func (self *DeviceLocalRpc) GetCanRefer(_ RpcNoArg, canRefer *bool) error {
	*canRefer = self.deviceLocal.GetCanRefer()
	return nil
}

func (self *DeviceLocalRpc) SetCanRefer(canRefer bool, _ RpcVoid) error {
	self.deviceLocal.SetCanRefer(canRefer)
	return nil
}

func (self *DeviceLocalRpc) SetRouteLocal(routeLocal bool, _ RpcVoid) error {
	self.deviceLocal.SetRouteLocal(routeLocal)
	return nil
}

func (self *DeviceLocalRpc) GetRouteLocal(_ RpcNoArg, routeLocal *bool) error {
	*routeLocal = self.deviceLocal.GetRouteLocal()
	return nil
}


func (self *DeviceLocalRpc) AddProvideChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addProvideChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addProvideChangeListener(listenerId connect.Id) {
	self.provideChangeListenerIds[listenerId] = true
	if self.provideChangeListenerSub == nil {
		self.provideChangeListenerSub = self.deviceLocal.AddProvideChangeListener(self)
	}
}

func (self *DeviceLocalRpc) RemoveProvideChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeProvideChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeProvideChangeListener(listenerId connect.Id) {
	delete(self.provideChangeListenerIds, listenerId)
	if len(self.provideChangeListenerIds) == 0 && self.provideChangeListenerSub != nil {
		self.provideChangeListenerSub.Close()
		self.provideChangeListenerSub = nil
	}
}

// ProvideChangeListener
func (self *DeviceLocalRpc) ProvideChanged(provideEnabled bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.provideChanged(provideEnabled)
}

// must be called with stateLock
func (self *DeviceLocalRpc) provideChanged(provideEnabled bool) {
	if self.service != nil {
		rpcCallVoid(self.service, "DeviceRemoteRpc.ProvideChanged", provideEnabled)
	}
}


func (self *DeviceLocalRpc) AddProvidePausedChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addProvidePausedChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addProvidePausedChangeListener(listenerId connect.Id) error {
	self.providePausedChangeListenerIds[listenerId] = true
	if self.providePausedChangeListenerSub == nil {
		self.providePausedChangeListenerSub = self.deviceLocal.AddProvidePausedChangeListener(self)
	}
	return nil
}

func (self *DeviceLocalRpc) RemoveProvidePausedChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeProvidePausedChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeProvidePausedChangeListener(listenerId connect.Id) {
	delete(self.providePausedChangeListenerIds, listenerId)
	if len(self.providePausedChangeListenerIds) == 0 && self.providePausedChangeListenerSub != nil {
		self.providePausedChangeListenerSub.Close()
		self.providePausedChangeListenerSub = nil
	}
}

// ProvidePausedChangeListener
func (self *DeviceLocalRpc) ProvidePausedChanged(providePaused bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.providePausedChanged(providePaused)
}

// must be called with stateLock
func (self *DeviceLocalRpc) providePausedChanged(providePaused bool) {
	if self.service != nil {
		rpcCallVoid(self.service, "DeviceRemoteRpc.ProvidePausedChanged", providePaused)
	}
}


func (self *DeviceLocalRpc) AddOfflineChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addOfflineChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addOfflineChangeListener(listenerId connect.Id) {
	self.offlineChangeListenerIds[listenerId] = true
	if self.offlineChangeListenerSub == nil {
		self.offlineChangeListenerSub = self.deviceLocal.AddOfflineChangeListener(self)
	}
}

func (self *DeviceLocalRpc) RemoveOfflineChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeOfflineChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeOfflineChangeListener(listenerId connect.Id) {
	delete(self.offlineChangeListenerIds, listenerId)
	if len(self.offlineChangeListenerIds) == 0 && self.offlineChangeListenerSub != nil {
		self.offlineChangeListenerSub.Close()
		self.offlineChangeListenerSub = nil
	}
}

// OfflineChangeListener
func (self *DeviceLocalRpc) OfflineChanged(offline bool, vpnInterfaceWhileOffline bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.offlineChanged(offline, vpnInterfaceWhileOffline)
}

// must be called with stateLock
func (self *DeviceLocalRpc) offlineChanged(offline bool, vpnInterfaceWhileOffline bool) {
	if self.service != nil {
		event := &DeviceRemoteOfflineChangeEvent{
			Offline: offline,
			VpnInterfaceWhileOffline: vpnInterfaceWhileOffline,
		}
		rpcCallVoid(self.service, "DeviceRemoteRpc.OfflineChanged", event)
	}
}


func (self *DeviceLocalRpc) AddConnectChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addConnectChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addConnectChangeListener(listenerId connect.Id) {
	self.connectChangeListenerIds[listenerId] = true
	if self.connectChangeListenerSub == nil {
		self.connectChangeListenerSub = self.deviceLocal.AddConnectChangeListener(self)
	}
}

func (self *DeviceLocalRpc) RemoveConnectChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeConnectChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeConnectChangeListener(listenerId connect.Id) {
	delete(self.connectChangeListenerIds, listenerId)
	if len(self.connectChangeListenerIds) == 0 && self.connectChangeListenerSub != nil {
		self.connectChangeListenerSub.Close()
		self.connectChangeListenerSub = nil
	}
}

// ConnectChangeListener
func (self *DeviceLocalRpc) ConnectChanged(connectEnabled bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.connectChanged(connectEnabled)
}

// must be called with stateLock
func (self *DeviceLocalRpc) connectChanged(connectEnabled bool) {
	if self.service != nil {
		rpcCallVoid(self.service, "DeviceRemoteRpc.ConnectChanged", connectEnabled)
	}
}


func (self *DeviceLocalRpc) AddRouteLocalChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addRouteLocalChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addRouteLocalChangeListener(listenerId connect.Id) {
	self.routeLocalChangeListenerIds[listenerId] = true
	if self.routeLocalChangeListenerSub == nil {
		self.routeLocalChangeListenerSub = self.deviceLocal.AddRouteLocalChangeListener(self)
	}
}

func (self *DeviceLocalRpc) RemoveRouteLocalChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeRouteLocalChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeRouteLocalChangeListener(listenerId connect.Id) {
	delete(self.routeLocalChangeListenerIds, listenerId)
	if len(self.routeLocalChangeListenerIds) == 0 && self.routeLocalChangeListenerSub != nil {
		self.routeLocalChangeListenerSub.Close()
		self.routeLocalChangeListenerSub = nil
	}
}

// RouteLocalChangeListener
func (self *DeviceLocalRpc) RouteLocalChanged(routeLocal bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.routeLocalChanged(routeLocal)
}

// must be called with stateLock
func (self *DeviceLocalRpc) routeLocalChanged(routeLocal bool) {
	if self.service != nil {
		rpcCallVoid(self.service, "DeviceRemoteRpc.RouteLocalChanged", routeLocal)
	}
}


func (self *DeviceLocalRpc) AddConnectLocationChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addConnectLocationChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addConnectLocationChangeListener(listenerId connect.Id) {
	self.connectLocationChangeListenerIds[listenerId] = true
	if self.connectLocationChangeListenerSub == nil {
		self.connectLocationChangeListenerSub = self.deviceLocal.AddConnectLocationChangeListener(self)
	}
}

func (self *DeviceLocalRpc) RemoveConnectLocationChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeConnectLocationChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeConnectLocationChangeListener(listenerId connect.Id) {
	delete(self.connectLocationChangeListenerIds, listenerId)
	if len(self.connectLocationChangeListenerIds) == 0 && self.connectLocationChangeListenerSub != nil {
		self.connectLocationChangeListenerSub.Close()
		self.connectLocationChangeListenerSub = nil
	}
}

// ConnectLocationChangeListener
func (self *DeviceLocalRpc) ConnectLocationChanged(location *ConnectLocation) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.connectLocationChanged(location)
}

// must be called with stateLock
func (self *DeviceLocalRpc) connectLocationChanged(location *ConnectLocation) {
	if self.service != nil {
		event := &DeviceRemoteConnectLocationChangeEvent{
			Location: newDeviceRemoteConnectLocation(location),
		}
		rpcCallVoid(self.service, "DeviceRemoteRpc.ConnectLocationChanged", event)
	}
}


func (self *DeviceLocalRpc) AddProvideSecretKeysListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addProvideSecretKeysListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addProvideSecretKeysListener(listenerId connect.Id) {
	self.provideSecretKeysListenerIds[listenerId] = true
	if self.provideSecretKeysListenerSub == nil {
		self.provideSecretKeysListenerSub = self.deviceLocal.AddProvideSecretKeysListener(self)
	}
}

func (self *DeviceLocalRpc) RemoveProvideSecretKeysListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeProvideSecretKeysListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeProvideSecretKeysListener(listenerId connect.Id) {
	delete(self.provideSecretKeysListenerIds, listenerId)
	if len(self.provideSecretKeysListenerIds) == 0 && self.provideSecretKeysListenerSub != nil {
		self.provideSecretKeysListenerSub.Close()
		self.provideSecretKeysListenerSub = nil
	}
}

// ProvideSecretKeysListener
func (self *DeviceLocalRpc) ProvideSecretKeysChanged(provideSecretKeyList *ProvideSecretKeyList) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.provideSecretKeysChanged(provideSecretKeyList)
}

// must be called with stateLock
func (self *DeviceLocalRpc) provideSecretKeysChanged(provideSecretKeyList *ProvideSecretKeyList) {
	if self.service != nil {
		rpcCallVoid(self.service, "DeviceRemoteRpc.ProvideSecretKeysChanged", provideSecretKeyList.getAll())
	}
}


// must be called with stateLock
// `trim` is true if the remote client will trim the active window ids
func (self *DeviceLocalRpc) updateWindowMonitor(trim bool) {
	localWindowMonitor := self.deviceLocal.windowMonitor()
	if self.localWindowMonitor != localWindowMonitor {
		if self.windowMonitorEventListenerSub != nil {
			self.windowMonitorEventListenerSub()
			self.windowMonitorEventListenerSub = nil
		}
		if trim {
			clear(self.localWindowIds)
		}

		self.localWindowId = connect.NewId()
		self.localWindowMonitor = localWindowMonitor
	}
}

// must be called with the stateLock
func (self *DeviceLocalRpc) windowIds() map[connect.Id]bool {
	windowIds := map[connect.Id]bool{}
 	for windowId, _ := range self.windowMonitorEventListenerIds {
 		windowIds[windowId] = true
 	}
 	return windowIds
}

func (self *DeviceLocalRpc) AddWindowMonitorEventListener(windowListenerId DeviceRemoteWindowListenerId, windowIds *map[connect.Id]bool) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	*windowIds = self.addWindowMonitorEventListener(windowListenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addWindowMonitorEventListener(windowListenerId DeviceRemoteWindowListenerId) map[connect.Id]bool {
	self.updateWindowMonitor(true)

	localWindowId, ok := self.localWindowIds[windowListenerId.WindowId]
	if !ok {
		localWindowId = self.localWindowId
		self.localWindowIds[windowListenerId.WindowId] = localWindowId
	}

	if self.localWindowId == localWindowId {
		monitorEventListeners, ok := self.windowMonitorEventListenerIds[windowListenerId.WindowId]
		if !ok {
			monitorEventListeners = map[connect.Id]bool{}
			self.windowMonitorEventListenerIds[windowListenerId.WindowId] = monitorEventListeners
		}
		monitorEventListeners[windowListenerId.ListenerId] = true

		if self.windowMonitorEventListenerSub == nil {
			self.windowMonitorEventListenerSub = self.localWindowMonitor.AddMonitorEventCallback(self.WindowMonitorEventCallback)
		}
	}

	return self.windowIds()
}

func (self *DeviceLocalRpc) RemoveWindowMonitorEventListener(windowListenerId DeviceRemoteWindowListenerId, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeWindowMonitorEventListener(windowListenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeWindowMonitorEventListener(windowListenerId DeviceRemoteWindowListenerId) {
	localWindowId, ok := self.localWindowIds[windowListenerId.WindowId]
	if ok && self.localWindowId == localWindowId {
		monitorEventListeners, ok := self.windowMonitorEventListenerIds[windowListenerId.WindowId]
		if ok {
			delete(monitorEventListeners, windowListenerId.ListenerId)
			if len(monitorEventListeners) == 0 {
				delete(self.windowMonitorEventListenerIds, windowListenerId.WindowId)
			}

			if len(self.windowMonitorEventListenerIds) == 0 && self.windowMonitorEventListenerSub != nil {
				self.windowMonitorEventListenerSub()
				self.windowMonitorEventListenerSub = nil
			}
		}
	}
}

func (self *DeviceLocalRpc) WindowMonitorEvents(_ RpcNoArg, event **DeviceRemoteWindowMonitorEvent) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	*event = self.windowMonitorEvents()
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) windowMonitorEvents() *DeviceRemoteWindowMonitorEvent {
	if self.localWindowMonitor != nil {
		windowExpandEvent, providerEvents := self.localWindowMonitor.Events()

		return &DeviceRemoteWindowMonitorEvent{
			WindowIds: self.windowIds(),
			WindowExpandEvent: windowExpandEvent,
			ProviderEvents: providerEvents,	
		}
	}
	return nil
}

// connect.MonitorEventFunction
func (self *DeviceLocalRpc) WindowMonitorEventCallback(
	windowExpandEvent *connect.WindowExpandEvent,
	providerEvents map[connect.Id]*connect.ProviderEvent,
) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.windowMonitorEventCallback(windowExpandEvent, providerEvents)
}

// must be called with stateLock
func (self *DeviceLocalRpc) windowMonitorEventCallback(
	windowExpandEvent *connect.WindowExpandEvent,
	providerEvents map[connect.Id]*connect.ProviderEvent,
) {
	if self.service != nil {
		event := &DeviceRemoteWindowMonitorEvent{
			WindowIds: self.windowIds(),
			WindowExpandEvent: windowExpandEvent,
			ProviderEvents: providerEvents,	
		}

		rpcCallVoid(self.service, "DeviceRemoteRpc.WindowMonitorEventCallback", event)
	}
}


func (self *DeviceLocalRpc) LoadProvideSecretKeys(provideSecretKeyList *ProvideSecretKeyList, _ RpcVoid) error {
	self.deviceLocal.LoadProvideSecretKeys(provideSecretKeyList)
	return nil
}

func (self *DeviceLocalRpc) InitProvideSecretKeys(_ RpcNoArg, _ RpcVoid) error {
	self.deviceLocal.InitProvideSecretKeys()
	return nil
}

func (self *DeviceLocalRpc) GetProvideEnabled(_ RpcNoArg, provideEnabled *bool) error {
	*provideEnabled = self.deviceLocal.GetProvideEnabled()
	return nil
}

func (self *DeviceLocalRpc) GetConnectEnabled(_ RpcNoArg, connectEnabled *bool) error {
	*connectEnabled = self.deviceLocal.GetConnectEnabled()
	return nil
}

func (self *DeviceLocalRpc) SetProvideMode(provideMode ProvideMode, _ RpcVoid) error {
	self.deviceLocal.SetProvideMode(provideMode)
	return nil
} 

func (self *DeviceLocalRpc) GetProvideMode(_ RpcNoArg, provideMode *ProvideMode) error {
	*provideMode = self.deviceLocal.GetProvideMode()
	return nil
}

func (self *DeviceLocalRpc) SetProvidePaused(providePaused bool, _ RpcVoid) error {
	self.deviceLocal.SetProvidePaused(providePaused)
	return nil
} 

func (self *DeviceLocalRpc) GetProvidePaused(_ RpcNoArg, providePaused *bool) error {
	*providePaused = self.deviceLocal.GetProvidePaused()
	return nil
}

func (self *DeviceLocalRpc) SetOffline(offline bool, _ RpcVoid) error {
	glog.Infof("[dlrpc]set offline")
	self.deviceLocal.SetOffline(offline)
	return nil
}

func (self *DeviceLocalRpc) GetOffline(_ RpcNoArg, offline *bool) error {
	*offline = self.deviceLocal.GetOffline()
	return nil
}

func (self *DeviceLocalRpc) SetVpnInterfaceWhileOffline(vpnInterfaceWhileOffline bool, _ RpcVoid) error {
	self.deviceLocal.SetVpnInterfaceWhileOffline(vpnInterfaceWhileOffline)
	return nil
}

func (self *DeviceLocalRpc) GetVpnInterfaceWhileOffline(_ RpcNoArg, vpnInterfaceWhileOffline *bool) error {
	*vpnInterfaceWhileOffline = self.deviceLocal.GetVpnInterfaceWhileOffline()
	return nil
}

func (self *DeviceLocalRpc) RemoveDestination(_ RpcNoArg, _ RpcVoid) error {
	self.deviceLocal.RemoveDestination()
	return nil
}

func (self *DeviceLocalRpc) SetDestination(destination *DeviceRemoteDestination, _ RpcVoid) error {
	providerSpecList := NewProviderSpecList()
	providerSpecList.addAll(destination.Specs...)
	self.deviceLocal.SetDestination(
		destination.Location.toConnectLocation(),
		providerSpecList,
		destination.ProvideMode,
	)
	return nil
}

func (self *DeviceLocalRpc) SetConnectLocation(location *DeviceRemoteConnectLocation, _ RpcVoid) error {
	self.deviceLocal.SetConnectLocation(location.toConnectLocation())
	return nil
} 

func (self *DeviceLocalRpc) GetConnectLocation(_ RpcNoArg, location **DeviceRemoteConnectLocation) error {
	*location = newDeviceRemoteConnectLocation(self.deviceLocal.GetConnectLocation())
	return nil
}  

func (self *DeviceLocalRpc) Shuffle(_ RpcNoArg, _ RpcVoid) error {
	self.deviceLocal.Shuffle()
	return nil
}

func (self *DeviceLocalRpc) HttpPostRaw(httpRequest *DeviceRemoteHttpRequest, _ RpcVoid) error {
	go func() {
		bodyBytes, err := connect.HttpPostWithStrategyRaw(
			self.ctx,
			self.deviceLocal.clientStrategy,
			httpRequest.RequestUrl,
			httpRequest.RequestBodyBytes,
			httpRequest.ByJwt,
		)

		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			
			if self.service != nil {
				httpResponse := newDeviceRemoteHttpResponse(httpRequest.RequestId, bodyBytes, err)

				rpcCallVoid(self.service, "DeviceRemoteRpc.HttpResponse", httpResponse)
			}
		}()
	}()
	return nil
}

func (self *DeviceLocalRpc) HttpGetRaw(httpRequest *DeviceRemoteHttpRequest, _ RpcVoid) error {
	go func() {
		bodyBytes, err := connect.HttpGetWithStrategyRaw(
			self.ctx,
			self.deviceLocal.clientStrategy,
			httpRequest.RequestUrl,
			httpRequest.ByJwt,
		)

		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			
			if self.service != nil {
				httpResponse := newDeviceRemoteHttpResponse(httpRequest.RequestId, bodyBytes, err)

				rpcCallVoid(self.service, "DeviceRemoteRpc.HttpResponse", httpResponse)
			}
		}()
	}()
	return nil
}

func (self *DeviceLocalRpc) Close() {
	// self.stateLock.Lock()
	// defer self.stateLock.Unlock()

	self.cancel()
	// if self.service != nil {
	// 	self.service.Close()
	// 	self.service = nil
	// }
}


// important all rpc functions here must dispatch on a new goroutine
// to avoid deadlocks since
// 1. we don't separate the state lock from the rpc lock
// 2. rpc is blocking and we serialize access to the client

//gomobile:noexport
type DeviceRemoteRpc struct {
	ctx context.Context
	cancel context.CancelFunc
	deviceRemote *DeviceRemote
}

func newDeviceRemoteRpc(ctx context.Context, deviceRemote *DeviceRemote) *DeviceRemoteRpc {
	cancelCtx, cancel := context.WithCancel(ctx)

	return &DeviceRemoteRpc{
		ctx: cancelCtx,
		cancel: cancel,
		deviceRemote: deviceRemote,
	}
}

func (self *DeviceRemoteRpc) ProvideChanged(provideEnabled bool, _ RpcVoid) error {
	glog.Infof("[drrpc]ProvideChanged provideEnabled=%t", provideEnabled)
	go self.deviceRemote.provideChanged(provideEnabled)
	return nil
}

func (self *DeviceRemoteRpc) ProvidePausedChanged(providePaused bool, _ RpcVoid) error {
	glog.Infof("[drrpc]ProvidePausedChanged providePaused=%t", providePaused)
	go self.deviceRemote.providePausedChanged(providePaused)
	return nil
}

func (self *DeviceRemoteRpc) OfflineChanged(event *DeviceRemoteOfflineChangeEvent, _ RpcVoid) error {
	glog.Infof("[drrpc]OfflineChanged offline=%t vpnInterfaceWhileOffline=%t", event.Offline, event.VpnInterfaceWhileOffline)
	go self.deviceRemote.offlineChanged(event.Offline, event.VpnInterfaceWhileOffline)
	return nil
}

func (self *DeviceRemoteRpc) ConnectChanged(connectEnabled bool, _ RpcVoid) error {
	glog.Infof("[drrpc]ConnectChanged connectEnabled=%t", connectEnabled)
	go self.deviceRemote.connectChanged(connectEnabled)
	return nil
}

func (self *DeviceRemoteRpc) RouteLocalChanged(routeLocal bool, _ RpcVoid) error {
	glog.Infof("[drrpc]RouteLocalChanged routeLocal=%t", routeLocal)
	go self.deviceRemote.routeLocalChanged(routeLocal)
	return nil
}

func (self *DeviceRemoteRpc) ConnectLocationChanged(event *DeviceRemoteConnectLocationChangeEvent, _ RpcVoid) error {
	glog.Infof("[drrpc]ConnectLocationChanged")
	go self.deviceRemote.connectLocationChanged(event.Location.toConnectLocation())
	return nil
}

func (self *DeviceRemoteRpc) ProvideSecretKeysChanged(provideSecretKeys []*ProvideSecretKey, _ RpcVoid) error {
	glog.Infof("[drrpc]ProvideSecretKeysChanged")
	provideSecretKeyList := NewProvideSecretKeyList()
	provideSecretKeyList.addAll(provideSecretKeys...)
	go self.deviceRemote.provideSecretKeysChanged(provideSecretKeyList)
	return nil
}

func (self *DeviceRemoteRpc) WindowMonitorEventCallback(event *DeviceRemoteWindowMonitorEvent, _ RpcVoid) error {
	glog.Infof("[drrpc]WindowMonitorEventCallback")
	go self.deviceRemote.windowMonitorEvent(
		event.WindowIds,
		event.WindowExpandEvent,
		event.ProviderEvents,
	)
	return nil
}

func (self *DeviceRemoteRpc) HttpResponse(httpResponse *DeviceRemoteHttpResponse, _ RpcVoid) error {
	glog.Infof("[drrpc]HttpResponse")
	go self.deviceRemote.httpResponse(httpResponse)
	return nil
}

func (self *DeviceRemoteRpc) Close() {
	self.cancel()
}
