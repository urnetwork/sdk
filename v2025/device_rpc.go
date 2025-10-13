package sdk

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/rpc"
	"slices"
	"strconv"
	"sync"
	"time"

	// "runtime/debug"
	mathrand "math/rand"

	"golang.org/x/exp/maps"

	"github.com/golang/glog"

	"github.com/urnetwork/connect/v2025"
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
	RpcCallTimeout      time.Duration
	RpcConnectTimeout   time.Duration
	RpcReconnectTimeout time.Duration
	KeepAliveTimeout    time.Duration
	// max number of keep alive pings after first. Must be at least 1
	KeepAliveRetryCount int
	// TODO randomize the ports
	Address                *DeviceRemoteAddress
	ResponseHost           string
	ResponsePorts          []int
	ResponsePortProbeCount int
	InitialLockTimeout     time.Duration
}

func defaultDeviceRpcSettings() *deviceRpcSettings {
	responsePorts := []int{}
	for port := 12025; port < 12125; port += 1 {
		responsePorts = append(responsePorts, port)
	}
	return &deviceRpcSettings{
		RpcCallTimeout:         4 * time.Second,
		RpcConnectTimeout:      1 * time.Second,
		RpcReconnectTimeout:    1 * time.Second,
		KeepAliveTimeout:       1 * time.Second,
		KeepAliveRetryCount:    2,
		Address:                requireRemoteAddress("127.0.0.1:12025"),
		ResponseHost:           "127.0.0.1",
		ResponsePorts:          responsePorts,
		ResponsePortProbeCount: 10,
		InitialLockTimeout:     200 * time.Millisecond,
	}
}

func (self *deviceRpcSettings) RequireRandResponseAddress() *DeviceRemoteAddress {
	address, err := self.RandResponseAddress()
	if err != nil {
		panic(err)
	}
	return address
}

func (self *deviceRpcSettings) RandResponseAddress() (*DeviceRemoteAddress, error) {
	ip, err := netip.ParseAddr(self.ResponseHost)
	if err != nil {
		return nil, err
	}
	port := self.ResponsePorts[mathrand.Intn(len(self.ResponsePorts))]
	return &DeviceRemoteAddress{
		Ip:   ip,
		Port: port,
	}, nil
}

// compile check that DeviceRemote conforms to Device, device, and ViewControllerManager
var _ Device = (*DeviceRemote)(nil)
var _ device = (*DeviceRemote)(nil)
var _ ViewControllerManager = (*DeviceRemote)(nil)

type DeviceRemote struct {
	ctx    context.Context
	cancel context.CancelFunc

	networkSpace *NetworkSpace
	byJwt        string

	settings *deviceRpcSettings

	reconnectMonitor *connect.Monitor
	syncMonitor      *connect.Monitor

	clientId       connect.Id
	instanceId     connect.Id
	clientStrategy *connect.ClientStrategy

	remoteChangeListeners *connect.CallbackList[RemoteChangeListener]

	// egressSecurityPolicy *deviceRemoteEgressSecurityPolicy
	// ingressSecurityPolicy *deviceRemote

	stateLock sync.Mutex

	service *rpcClient

	provideChangeListeners            map[connect.Id]ProvideChangeListener
	providePausedChangeListeners      map[connect.Id]ProvidePausedChangeListener
	provideNetworkModeChangeListeners map[connect.Id]ProvideNetworkModeChangeListener
	offlineChangeListeners            map[connect.Id]OfflineChangeListener
	connectChangeListeners            map[connect.Id]ConnectChangeListener
	routeLocalChangeListeners         map[connect.Id]RouteLocalChangeListener
	connectLocationChangeListeners    map[connect.Id]ConnectLocationChangeListener
	provideSecretKeysListeners        map[connect.Id]ProvideSecretKeysListener
	windowMonitors                    map[connect.Id]*deviceRemoteWindowMonitor
	tunnelChangeListeners             map[connect.Id]TunnelChangeListener
	contractStatusChangeListeners     map[connect.Id]ContractStatusChangeListener
	windowStatusChangeListeners       map[connect.Id]WindowStatusChangeListener

	httpResponseChannels map[connect.Id]chan *DeviceRemoteHttpResponse

	state DeviceRemoteState
	// last observed values
	lastKnownState DeviceRemoteState

	viewControllerManager
}

func NewDeviceRemoteWithDefaults(
	networkSpace *NetworkSpace,
	byJwt string,
	instanceId *Id,
) (*DeviceRemote, error) {
	return newDeviceRemote(networkSpace, byJwt, instanceId, defaultDeviceRpcSettings())
}

func newDeviceRemote(
	networkSpace *NetworkSpace,
	byJwt string,
	instanceId *Id,
	settings *deviceRpcSettings,
) (*DeviceRemote, error) {
	clientId, err := parseByJwtClientId(byJwt)
	if err != nil {
		return nil, err
	}

	return newDeviceRemoteWithOverrides(
		networkSpace,
		byJwt,
		instanceId,
		settings,
		clientId,
	)
}

func newDeviceRemoteWithOverrides(
	networkSpace *NetworkSpace,
	byJwt string,
	instanceId *Id,
	settings *deviceRpcSettings,
	clientId connect.Id,
) (*DeviceRemote, error) {
	ctx, cancel := context.WithCancel(context.Background())

	api := networkSpace.GetApi()
	api.SetByJwt(byJwt)

	deviceRemote := &DeviceRemote{
		ctx:                   ctx,
		cancel:                cancel,
		networkSpace:          networkSpace,
		byJwt:                 byJwt,
		settings:              settings,
		reconnectMonitor:      connect.NewMonitor(),
		syncMonitor:           connect.NewMonitor(),
		clientId:              clientId,
		instanceId:            instanceId.toConnectId(),
		clientStrategy:        networkSpace.clientStrategy,
		remoteChangeListeners: connect.NewCallbackList[RemoteChangeListener](),

		provideChangeListeners:            map[connect.Id]ProvideChangeListener{},
		providePausedChangeListeners:      map[connect.Id]ProvidePausedChangeListener{},
		provideNetworkModeChangeListeners: map[connect.Id]ProvideNetworkModeChangeListener{},
		offlineChangeListeners:            map[connect.Id]OfflineChangeListener{},
		connectChangeListeners:            map[connect.Id]ConnectChangeListener{},
		routeLocalChangeListeners:         map[connect.Id]RouteLocalChangeListener{},
		connectLocationChangeListeners:    map[connect.Id]ConnectLocationChangeListener{},
		provideSecretKeysListeners:        map[connect.Id]ProvideSecretKeysListener{},
		windowMonitors:                    map[connect.Id]*deviceRemoteWindowMonitor{},
		tunnelChangeListeners:             map[connect.Id]TunnelChangeListener{},
		contractStatusChangeListeners:     map[connect.Id]ContractStatusChangeListener{},
		windowStatusChangeListeners:       map[connect.Id]WindowStatusChangeListener{},
		httpResponseChannels:              map[connect.Id]chan *DeviceRemoteHttpResponse{},
	}

	deviceRemote.viewControllerManager = *newViewControllerManager(ctx, deviceRemote)

	api.setHttpPostRaw(deviceRemote.httpPostRaw)
	api.setHttpGetRaw(deviceRemote.httpGetRaw)

	newSecurityPolicyMonitor(ctx, deviceRemote)

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
				case <-handleCtx.Done():
					return
				default:
				}

				// service := rpc.NewClient(conn)
				service := &rpcClientWithTimeout{
					ctx:         self.ctx,
					timeout:     self.settings.RpcCallTimeout,
					closeClient: conn.Close,
					client:      rpc.NewClient(conn),
				}
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
					ProvideChangeListenerIds:         maps.Keys(self.provideChangeListeners),
					ProvidePausedChangeListenerIds:   maps.Keys(self.providePausedChangeListeners),
					OfflineChangeListenerIds:         maps.Keys(self.offlineChangeListeners),
					ConnectChangeListenerIds:         maps.Keys(self.connectChangeListeners),
					RouteLocalChangeListenerIds:      maps.Keys(self.routeLocalChangeListeners),
					ConnectLocationChangeListenerIds: maps.Keys(self.connectLocationChangeListeners),
					ProvideSecretKeysListenerIds:     maps.Keys(self.provideSecretKeysListeners),
					TunnelChangeListenerIds:          maps.Keys(self.tunnelChangeListeners),
					ContractStatusChangeListenerIds:  maps.Keys(self.contractStatusChangeListeners),
					WindowStatusChangeListenerIds:    maps.Keys(self.windowStatusChangeListeners),
					WindowMonitorEventListenerIds:    windowMonitorListenerIds,
					State:                            self.state,
				}
				syncResponse, err := rpcCall[*DeviceRemoteSyncResponse](service, "DeviceLocalRpc.Sync", syncRequest, self.closeService)
				if err != nil {
					return
				}

				if syncResponse.Error != "" {
					glog.Infof("Sync error: %s", syncResponse.Error)
					return
				}

				// trim the windows
				// for windowId, windowMonitor := range self.windowMonitors {
				// 	if !syncResponse.WindowIds[windowId] {
				// 		delete(self.windowMonitors, windowId)
				// 		clear(windowMonitor.listeners)
				// 	}
				// }

				// FIXME use response cert to listen with TLS
				listenConfig := &net.ListenConfig{
					KeepAliveConfig: net.KeepAliveConfig{
						Enable:   true,
						Idle:     self.settings.KeepAliveTimeout / time.Duration(2*self.settings.KeepAliveRetryCount),
						Interval: self.settings.KeepAliveTimeout / time.Duration(2*self.settings.KeepAliveRetryCount),
						Count:    self.settings.KeepAliveRetryCount,
					},
				}
				var responseAddress *DeviceRemoteAddress
				for range self.settings.ResponsePortProbeCount {
					responseAddress = self.settings.RequireRandResponseAddress()
					responseListener, err = listenConfig.Listen(handleCtx, "tcp", responseAddress.HostPort())
					if err == nil {
						break
					}
				}
				if err != nil {
					glog.Infof("[dr]sync reverse listen err = %s", err)
					return
				}
				defer func() {
					if !synced {
						responseListener.Close()
					}
				}()

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
							case <-connCtx.Done():
							}
						}()
						server.ServeConn(conn)
						glog.Infof("[dr]sync reverse server done")
					}()

					// resync
				}()

				err = rpcCallVoid(service, "DeviceLocalRpc.SyncReverse", responseAddress, self.closeService)
				if err != nil {
					return
				}

				// because the local state changes always win,
				// the last known state can be copied from the local state changes
				// note if there were conflict rules, we would need to get the remote state here
				self.lastKnownState.Merge(&self.state)
				self.state.Unset()
				self.syncMonitor.NotifyAll()

				self.service = service

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
				case <-handleCtx.Done():
				}
				glog.Infof("[dr]handle done")

				func() {
					self.stateLock.Lock()
					defer self.stateLock.Unlock()

					self.closeService()
					responseListener.Close()

					// close pending http responses
					for _, responseChannel := range self.httpResponseChannels {
						close(responseChannel)
					}
					clear(self.httpResponseChannels)
				}()

				self.remoteChanged(false)
				self.tunnelChanged(false)
			}

		}()

		select {
		case <-self.ctx.Done():
			return
		case <-time.After(self.settings.RpcReconnectTimeout):
		case <-notify:
			// reconnect now
		}
	}
}

// must be called with state lock
func (self *DeviceRemote) closeService() {
	if self.service != nil {
		self.service.Close()
		self.service = nil
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

	synced, notify := func() (bool, chan struct{}) {
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
		case <-self.ctx.Done():
			return false
		case <-notify:
			return true
		case <-time.After(timeout):
			return false
		}
	} else {
		select {
		case <-self.ctx.Done():
			return false
		case <-notify:
			return true
		}
	}
}

func (self *DeviceRemote) getService() *rpcClient {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.service
}

func (self *DeviceRemote) GetClientId() *Id {
	return newId(self.clientId)
}

func (self *DeviceRemote) GetInstanceId() *Id {
	return newId(self.instanceId)
}

func (self *DeviceRemote) GetApi() *Api {
	return self.networkSpace.GetApi()
}

func (self *DeviceRemote) GetNetworkSpace() *NetworkSpace {
	return self.networkSpace
}

func (self *DeviceRemote) SetTunnelStarted(tunnelStarted bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	success := func() bool {
		if self.service == nil {
			return false
		}

		err := rpcCallVoid(self.service, "DeviceLocalRpc.SetTunnelStarted", tunnelStarted, self.closeService)
		if err != nil {
			return false
		}
		return true
	}()
	state := &self.state
	if success {
		state = &self.lastKnownState
	}
	state.TunnelStarted.Set(tunnelStarted)
}

func (self *DeviceRemote) GetTunnelStarted() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	tunnelStarted, success := func() (bool, bool) {
		if self.service == nil {
			return false, false
		}

		tunnelStarted, err := rpcCallNoArg[bool](self.service, "DeviceLocalRpc.GetTunnelStarted", self.closeService)
		if err != nil {
			return false, false
		}
		self.lastKnownState.TunnelStarted.Set(tunnelStarted)
		return tunnelStarted, true
	}()
	if success {
		return tunnelStarted
	} else {
		return self.service != nil && self.state.TunnelStarted.Get(
			self.lastKnownState.TunnelStarted.Get(defaultTunnelStarted),
		)
	}
}

func (self *DeviceRemote) GetContractStatus() *ContractStatus {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	contractStatus, success := func() (*ContractStatus, bool) {
		if self.service == nil {
			return nil, false
		}

		status, err := rpcCallNoArg[*DeviceRemoteContractStatus](self.service, "DeviceLocalRpc.GetContractStatus", self.closeService)
		if err != nil {
			return nil, false
		}
		contractStatus := status.ContractStatus
		self.lastKnownState.ContractStatus.Set(contractStatus)
		return contractStatus, true
	}()
	if success {
		return contractStatus
	} else {
		return self.state.ContractStatus.Get(
			self.lastKnownState.ContractStatus.Get(nil),
		)
	}
}

func (self *DeviceRemote) GetWindowStatus() *WindowStatus {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	windowStatus, success := func() (*WindowStatus, bool) {
		if self.service == nil {
			return nil, false
		}

		status, err := rpcCallNoArg[*DeviceRemoteWindowStatus](self.service, "DeviceLocalRpc.GetWindowStatus", self.closeService)
		if err != nil {
			return nil, false
		}
		windowStatus := status.WindowStatus
		self.lastKnownState.WindowStatus.Set(windowStatus)
		return windowStatus, true
	}()
	if success {
		return windowStatus
	} else {
		return self.state.WindowStatus.Get(
			self.lastKnownState.WindowStatus.Get(nil),
		)
	}
}

func (self *DeviceRemote) GetStats() *DeviceStats {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	stats, success := func() (*DeviceStats, bool) {
		if self.service == nil {
			return nil, false
		}

		stats, err := rpcCallNoArg[*DeviceStats](self.service, "DeviceLocalRpc.GetStats", self.closeService)
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

	shouldShowRatingDialog, success := func() (bool, bool) {
		if self.service == nil {
			return false, false
		}

		shouldShowRatingDialog, err := rpcCallNoArg[bool](self.service, "DeviceLocalRpc.GetShouldShowRatingDialog", self.closeService)
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

	canShowRatingDialog, success := func() (bool, bool) {
		if self.service == nil {
			return false, false
		}

		canShowRatingDialog, err := rpcCallNoArg[bool](self.service, "DeviceLocalRpc.GetCanShowRatingDialog", self.closeService)
		if err != nil {
			return false, false
		}
		self.lastKnownState.CanShowRatingDialog.Set(canShowRatingDialog)
		return canShowRatingDialog, true
	}()
	if success {
		return canShowRatingDialog
	} else {
		return self.state.CanShowRatingDialog.Get(
			self.lastKnownState.CanShowRatingDialog.Get(defaultCanShowRatingDialog),
		)
	}
}

func (self *DeviceRemote) SetCanShowRatingDialog(canShowRatingDialog bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	success := func() bool {
		if self.service == nil {
			return false
		}

		err := rpcCallVoid(self.service, "DeviceLocalRpc.SetCanShowRatingDialog", canShowRatingDialog, self.closeService)
		if err != nil {
			return false
		}
		return true
	}()
	state := &self.state
	if success {
		state = &self.lastKnownState
	}
	state.CanShowRatingDialog.Set(canShowRatingDialog)
}

func (self *DeviceRemote) GetProvideControlMode() ProvideControlMode {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	provideControlMode, success := func() (ProvideControlMode, bool) {
		if self.service == nil {
			return ProvideControlModeAuto, false
		}

		provideControlMode, err := rpcCallNoArg[ProvideControlMode](self.service, "DeviceLocalRpc.GetProvideControlMode", self.closeService)
		if err != nil {
			return ProvideControlModeAuto, false
		}
		self.lastKnownState.ProvideControlMode.Set(provideControlMode)
		return provideControlMode, true
	}()
	if success {
		return provideControlMode
	} else {
		return self.state.ProvideControlMode.Get(
			self.lastKnownState.ProvideControlMode.Get(defaultProvideControlMode),
		)
	}
}

func (self *DeviceRemote) SetProvideControlMode(mode ProvideControlMode) {
	event := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		success := func() bool {
			if self.service == nil {
				return false
			}

			err := rpcCallVoid(self.service, "DeviceLocalRpc.SetProvideControlMode", mode, self.closeService)
			if err != nil {
				return false
			}
			return true
		}()
		state := &self.state
		if success {
			state = &self.lastKnownState
		} else {
			event = true
		}
		state.ProvideControlMode.Set(mode)
	}()
	if event {
		self.provideChanged(self.GetProvideEnabled())
	}
}

func (self *DeviceRemote) GetCanRefer() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	canRefer, success := func() (bool, bool) {
		if self.service == nil {
			return false, false
		}

		canRefer, err := rpcCallNoArg[bool](self.service, "DeviceLocalRpc.GetCanRefer", self.closeService)
		if err != nil {
			return false, false
		}
		self.lastKnownState.CanRefer.Set(canRefer)
		return canRefer, true
	}()
	if success {
		return canRefer
	} else {
		return self.state.CanRefer.Get(
			self.lastKnownState.CanRefer.Get(defaultCanRefer),
		)
	}
}

func (self *DeviceRemote) SetCanRefer(canRefer bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	success := func() bool {
		if self.service == nil {
			return false
		}

		err := rpcCallVoid(self.service, "DeviceLocalRpc.SetCanRefer", canRefer, self.closeService)
		if err != nil {
			return false
		}
		return true
	}()
	state := &self.state
	if success {
		state = &self.lastKnownState
	}
	state.CanRefer.Set(canRefer)
}

func (self *DeviceRemote) GetAllowForeground() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	allowForeground, success := func() (bool, bool) {
		if self.service == nil {
			return false, false
		}

		allowForeground, err := rpcCallNoArg[bool](self.service, "DeviceLocalRpc.GetAllowForeground", self.closeService)
		if err != nil {
			return false, false
		}
		self.lastKnownState.CanRefer.Set(allowForeground)
		return allowForeground, true
	}()
	if success {
		return allowForeground
	} else {
		return self.state.AllowForeground.Get(
			self.lastKnownState.AllowForeground.Get(defaultAllowForeground),
		)
	}
}

func (self *DeviceRemote) SetAllowForeground(allowForeground bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	success := func() bool {
		if self.service == nil {
			return false
		}

		err := rpcCallVoid(self.service, "DeviceLocalRpc.SetAllowForeground", allowForeground, self.closeService)
		if err != nil {
			return false
		}
		return true
	}()
	state := &self.state
	if success {
		state = &self.lastKnownState
	}
	state.AllowForeground.Set(allowForeground)
}

func (self *DeviceRemote) SetRouteLocal(routeLocal bool) {
	event := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		success := func() bool {
			if self.service == nil {
				return false
			}

			err := rpcCallVoid(self.service, "DeviceLocalRpc.SetRouteLocal", routeLocal, self.closeService)
			if err != nil {
				return false
			}
			return true
		}()
		state := &self.state
		if success {
			state = &self.lastKnownState
		} else {
			event = true
		}
		state.RouteLocal.Set(routeLocal)
	}()
	if event {
		self.routeLocalChanged(self.GetRouteLocal())
	}
}

func (self *DeviceRemote) GetRouteLocal() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	routeLocal, success := func() (bool, bool) {
		if self.service == nil {
			return false, false
		}

		routeLocal, err := rpcCallNoArg[bool](self.service, "DeviceLocalRpc.GetRouteLocal", self.closeService)
		if err != nil {
			return false, false
		}
		self.lastKnownState.RouteLocal.Set(routeLocal)
		return routeLocal, true
	}()
	if success {
		return routeLocal
	} else {
		return self.state.RouteLocal.Get(
			self.lastKnownState.RouteLocal.Get(defaultRouteLocal),
		)
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
		rpcCallVoid(deviceRemote.service, addServiceFunc, listenerId, deviceRemote.closeService)
	}

	return newSub(func() {
		deviceRemote.stateLock.Lock()
		defer deviceRemote.stateLock.Unlock()

		delete(listeners, listenerId)
		if deviceRemote.service != nil {
			rpcCallVoid(deviceRemote.service, removeServiceFunc, listenerId, deviceRemote.closeService)
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

func (self *DeviceRemote) AddProvideNetworkModeChangeListener(listener ProvideNetworkModeChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.provideNetworkModeChangeListeners,
		"DeviceLocalRpc.AddProvideNetworkModeChangeListener",
		"DeviceLocalRpc.RemoveProvideNetworkModeChangeListener",
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

func (self *DeviceRemote) AddTunnelChangeListener(listener TunnelChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.tunnelChangeListeners,
		"DeviceLocalRpc.AddTunnelChangeListener",
		"DeviceLocalRpc.RemoveTunnelChangeListener",
	)
}

func (self *DeviceRemote) AddContractStatusChangeListener(listener ContractStatusChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.contractStatusChangeListeners,
		"DeviceLocalRpc.AddContractStatusChangeListener",
		"DeviceLocalRpc.RemoveContractStatusChangeListener",
	)
}

func (self *DeviceRemote) AddWindowStatusChangeListener(listener WindowStatusChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.windowStatusChangeListeners,
		"DeviceLocalRpc.AddWindowStatusChangeListener",
		"DeviceLocalRpc.RemoveWindowStatusChangeListener",
	)
}

func (self *DeviceRemote) LoadProvideSecretKeys(provideSecretKeyList *ProvideSecretKeyList) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	success := func() bool {
		if self.service == nil {
			return false
		}

		err := rpcCallVoid(self.service, "DeviceLocalRpc.LoadProvideSecretKeys", provideSecretKeyList.getAll(), self.closeService)
		if err != nil {
			return false
		}
		return true
	}()
	state := &self.state
	if success {
		state = &self.lastKnownState
	}
	state.LoadProvideSecretKeys.Set(provideSecretKeyList.getAll())
	state.InitProvideSecretKeys.Unset()
}

func (self *DeviceRemote) InitProvideSecretKeys() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	success := func() bool {
		if self.service == nil {
			return false
		}

		err := rpcCallNoArgVoid(self.service, "DeviceLocalRpc.InitProvideSecretKeys", self.closeService)
		if err != nil {
			return false
		}
		return true
	}()
	state := &self.state
	if success {
		state = &self.lastKnownState
	}
	state.InitProvideSecretKeys.Set(true)
	state.LoadProvideSecretKeys.Unset()
}

func (self *DeviceRemote) GetProvideEnabled() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	provideEnabled, success := func() (bool, bool) {
		if self.service == nil {
			return false, false
		}

		provideEnabled, err := rpcCallNoArg[bool](self.service, "DeviceLocalRpc.GetProvideEnabled", self.closeService)
		if err != nil {
			return false, false
		}
		self.lastKnownState.ProvideEnabled.Set(provideEnabled)
		return provideEnabled, true
	}()
	if success {
		return provideEnabled
	} else {
		return self.lastKnownState.ProvideEnabled.Get(false)
	}
}

func (self *DeviceRemote) GetConnectEnabled() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	connectEnabled, success := func() (bool, bool) {
		if self.service == nil {
			return false, false
		}

		connectEnabled, err := rpcCallNoArg[bool](self.service, "DeviceLocalRpc.GetConnectEnabled", self.closeService)
		if err != nil {
			return false, false
		}
		self.lastKnownState.ConnectEnabled.Set(connectEnabled)
		return connectEnabled, true
	}()
	if success {
		return connectEnabled
	} else {
		if self.state.Location.IsSet {
			return self.state.Location.Value.ConnectLocation != nil
		} else if self.state.Destination.IsSet {
			return self.state.Destination.Value.Location.ConnectLocation != nil
		} else if self.state.RemoveDestination.IsSet {
			return false
		} else {
			return self.lastKnownState.ConnectEnabled.Get(false)
		}
	}
}

func (self *DeviceRemote) SetProvideMode(provideMode ProvideMode) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	success := func() bool {
		if self.service == nil {
			return false
		}

		err := rpcCallVoid(self.service, "DeviceLocalRpc.SetProvideMode", provideMode, self.closeService)
		if err != nil {
			return false
		}
		return true
	}()
	state := &self.state
	if success {
		state = &self.lastKnownState
	}
	state.ProvideMode.Set(provideMode)
}

func (self *DeviceRemote) GetProvideMode() ProvideMode {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	provideMode, success := func() (ProvideMode, bool) {
		if self.service == nil {
			var empty ProvideMode
			return empty, false
		}

		provideMode, err := rpcCallNoArg[ProvideMode](self.service, "DeviceLocalRpc.GetProvideMode", self.closeService)
		if err != nil {
			var empty ProvideMode
			return empty, false
		}
		self.lastKnownState.ProvideMode.Set(provideMode)
		return provideMode, true
	}()
	if success {
		return provideMode
	} else {
		return self.state.ProvideMode.Get(
			self.lastKnownState.ProvideMode.Get(ProvideModeNone),
		)
	}
}

func (self *DeviceRemote) SetProvidePaused(providePaused bool) {
	event := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		success := func() bool {
			if self.service == nil {
				return false
			}

			err := rpcCallVoid(self.service, "DeviceLocalRpc.SetProvidePaused", providePaused, self.closeService)
			if err != nil {
				return false
			}
			return true
		}()
		state := &self.state
		if success {
			state = &self.lastKnownState
		} else {
			event = true
		}
		state.ProvidePaused.Set(providePaused)
	}()
	if event {
		self.providePausedChanged(self.GetProvidePaused())
	}
}

func (self *DeviceRemote) GetProvidePaused() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	providePaused, success := func() (bool, bool) {
		if self.service == nil {
			return false, false
		}

		providePaused, err := rpcCallNoArg[bool](self.service, "DeviceLocalRpc.GetProvidePaused", self.closeService)
		if err != nil {
			return false, false
		}
		self.lastKnownState.ProvidePaused.Set(providePaused)
		return providePaused, true
	}()
	if success {
		return providePaused
	} else {
		return self.state.ProvidePaused.Get(
			self.lastKnownState.ProvidePaused.Get(false),
		)
	}
}

func (self *DeviceRemote) SetProvideNetworkMode(provideNetworkMode ProvideNetworkMode) {
	event := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		success := func() bool {
			if self.service == nil {
				return false
			}

			err := rpcCallVoid(self.service, "DeviceLocalRpc.SetProvideNetworkMode", provideNetworkMode, self.closeService)
			if err != nil {
				return false
			}
			return true
		}()
		state := &self.state
		if success {
			state = &self.lastKnownState
		} else {
			event = true
		}
		state.ProvideNetworkMode.Set(provideNetworkMode)
	}()
	if event {
		self.provideNetworkModeChanged(self.GetProvideNetworkMode())
	}
}

func (self *DeviceRemote) GetProvideNetworkMode() ProvideNetworkMode {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	provideNetworkMode, success := func() (ProvideNetworkMode, bool) {
		if self.service == nil {
			return ProvideNetworkModeWiFi, false
		}

		provideNetworkMode, err := rpcCallNoArg[ProvideNetworkMode](self.service, "DeviceLocalRpc.GetProvideNetworkMode", self.closeService)
		if err != nil {
			return ProvideNetworkModeWiFi, false
		}
		self.lastKnownState.ProvideNetworkMode.Set(provideNetworkMode)
		return provideNetworkMode, true
	}()
	if success {
		return provideNetworkMode
	} else {
		return self.state.ProvideNetworkMode.Get(
			self.lastKnownState.ProvideNetworkMode.Get(ProvideNetworkModeWiFi),
		)
	}
}

func (self *DeviceRemote) SetOffline(offline bool) {
	event := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		success := func() bool {
			if self.service == nil {
				return false
			}

			err := rpcCallVoid(self.service, "DeviceLocalRpc.SetOffline", offline, self.closeService)
			if err != nil {
				return false
			}
			return true
		}()
		state := &self.state
		if success {
			state = &self.lastKnownState
		} else {
			event = true
		}
		state.Offline.Set(offline)
	}()
	if event {
		self.offlineChanged(self.GetOffline(), self.GetVpnInterfaceWhileOffline())
	}
}

func (self *DeviceRemote) GetOffline() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	offline, success := func() (bool, bool) {
		if self.service == nil {
			return false, false
		}

		offline, err := rpcCallNoArg[bool](self.service, "DeviceLocalRpc.GetOffline", self.closeService)
		if err != nil {
			return false, false
		}
		self.lastKnownState.Offline.Set(offline)
		return offline, true
	}()
	if success {
		return offline
	} else {
		return self.state.Offline.Get(
			self.lastKnownState.Offline.Get(defaultOffline),
		)
	}
}

func (self *DeviceRemote) SetVpnInterfaceWhileOffline(vpnInterfaceWhileOffline bool) {
	event := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		success := func() bool {
			if self.service == nil {
				return false
			}

			err := rpcCallVoid(self.service, "DeviceLocalRpc.SetVpnInterfaceWhileOffline", vpnInterfaceWhileOffline, self.closeService)
			if err != nil {
				return false
			}
			return true
		}()
		state := &self.state
		if success {
			state = &self.lastKnownState
		} else {
			event = true
		}
		state.VpnInterfaceWhileOffline.Set(vpnInterfaceWhileOffline)
	}()
	if event {
		self.offlineChanged(self.GetOffline(), self.GetVpnInterfaceWhileOffline())
	}
}

func (self *DeviceRemote) GetVpnInterfaceWhileOffline() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	vpnInterfaceWhileOffline, success := func() (bool, bool) {
		if self.service == nil {
			return false, false
		}

		vpnInterfaceWhileOffline, err := rpcCallNoArg[bool](self.service, "DeviceLocalRpc.GetVpnInterfaceWhileOffline", self.closeService)
		if err != nil {
			return false, false
		}
		self.lastKnownState.VpnInterfaceWhileOffline.Set(vpnInterfaceWhileOffline)
		return vpnInterfaceWhileOffline, true
	}()
	if success {
		return vpnInterfaceWhileOffline
	} else {
		return self.state.VpnInterfaceWhileOffline.Get(
			self.lastKnownState.VpnInterfaceWhileOffline.Get(defaultVpnInterfaceWhileOffline),
		)
	}
}

func (self *DeviceRemote) RemoveDestination() {
	event := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		success := func() bool {
			if self.service == nil {
				return false
			}

			err := rpcCallNoArgVoid(self.service, "DeviceLocalRpc.RemoveDestination", self.closeService)
			if err != nil {
				return false
			}
			return true
		}()
		state := &self.state
		if success {
			state = &self.lastKnownState
		} else {
			event = true
		}
		state.RemoveDestination.Set(true)
		state.Destination.Unset()
		state.Location.Unset()
	}()
	if event {
		self.connectLocationChanged(newDeviceRemoteConnectLocation(self.GetConnectLocation()))
		self.connectChanged(self.GetConnectEnabled())
	}
}

func (self *DeviceRemote) SetDestination(location *ConnectLocation, specs *ProviderSpecList, provideMode ProvideMode) {
	event := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		destination := &DeviceRemoteDestination{
			Location:    newDeviceRemoteConnectLocation(location),
			Specs:       specs.getAll(),
			ProvideMode: provideMode,
		}

		success := func() bool {
			if self.service == nil {
				return false
			}

			err := rpcCallVoid(self.service, "DeviceLocalRpc.SetDestination", destination, self.closeService)
			if err != nil {
				return false
			}
			return true
		}()
		state := &self.state
		if success {
			state = &self.lastKnownState
		} else {
			event = true
		}
		state.Destination.Set(destination)
		state.RemoveDestination.Unset()
		state.Location.Unset()
	}()
	if event {
		self.connectLocationChanged(newDeviceRemoteConnectLocation(self.GetConnectLocation()))
		self.connectChanged(self.GetConnectEnabled())
		self.provideChanged(self.GetProvideEnabled())
	}
}

func (self *DeviceRemote) SetConnectLocation(location *ConnectLocation) {
	deviceRemoteLocation := newDeviceRemoteConnectLocation(location)
	event := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		success := func() bool {
			if self.service == nil {
				return false
			}

			err := rpcCallVoid(self.service, "DeviceLocalRpc.SetConnectLocation", deviceRemoteLocation, self.closeService)
			if err != nil {
				return false
			}
			return true
		}()
		state := &self.state
		if success {
			state = &self.lastKnownState
		} else {
			event = true
		}
		state.Location.Set(deviceRemoteLocation)
		state.RemoveDestination.Unset()
		state.Destination.Unset()
	}()
	if event {
		self.connectLocationChanged(newDeviceRemoteConnectLocation(self.GetConnectLocation()))
		self.connectChanged(self.GetConnectEnabled())
	}
}

func (self *DeviceRemote) GetConnectLocation() *ConnectLocation {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	location, success := func() (*ConnectLocation, bool) {
		if self.service == nil {
			return nil, false
		}

		deviceRemoteLocation, err := rpcCallNoArg[*DeviceRemoteConnectLocation](self.service, "DeviceLocalRpc.GetConnectLocation", self.closeService)
		if err != nil {
			return nil, false
		}
		self.lastKnownState.Location.Set(deviceRemoteLocation)
		return deviceRemoteLocation.toConnectLocation(), true
	}()
	if success {
		return location
	} else {
		if self.state.Location.IsSet {
			return self.state.Location.Value.toConnectLocation()
		} else if self.state.Destination.IsSet {
			return self.state.Destination.Value.Location.toConnectLocation()
		} else if self.lastKnownState.Location.IsSet {
			return self.lastKnownState.Location.Value.toConnectLocation()
		} else {
			return nil
		}
	}
}

func (self *DeviceRemote) GetDefaultLocation() *ConnectLocation {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	defaultLocation, success := func() (*ConnectLocation, bool) {
		if self.service == nil {
			return nil, false
		}

		defaultLocation, err := rpcCallNoArg[*DeviceRemoteConnectLocation](self.service, "DeviceLocalRpc.GetDefaultLocation", self.closeService)
		if err != nil {
			return nil, false
		}
		return defaultLocation.toConnectLocation(), true
	}()
	if success {
		return defaultLocation
	} else {
		if self.state.DefaultLocation.IsSet {
			return self.state.DefaultLocation.Value.ConnectLocation.toConnectLocation()
		}
		glog.Infof("No default location set, returning nil")
		return nil
	}
}

func (self *DeviceRemote) SetDefaultLocation(connectLocation *ConnectLocation) {

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	deviceRemoteLocation := newDeviceRemoteDefaultLocation(connectLocation)

	success := func() bool {
		if self.service == nil {
			return false
		}

		err := rpcCallVoid(self.service, "DeviceLocalRpc.SetDefaultLocation", deviceRemoteLocation, self.closeService)
		if err != nil {
			return false
		}
		return true
	}()

	state := &self.state

	if success {
		state = &self.lastKnownState
	}

	state.DefaultLocation.Set(deviceRemoteLocation)
}

func (self *DeviceRemote) Shuffle() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	success := func() bool {
		if self.service == nil {
			return false
		}

		err := rpcCallNoArgVoid(self.service, "DeviceLocalRpc.Shuffle", self.closeService)
		if err != nil {
			return false
		}
		return true
	}()
	state := &self.state
	if success {
		state = &self.lastKnownState
	}
	state.Shuffle.Set(true)
}

func (self *DeviceRemote) Cancel() {
	self.cancel()
}

func (self *DeviceRemote) Close() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.cancel()

	// if self.service != nil {
	// 	self.service.Close()
	// 	self.service = nil
	// }

	api := self.networkSpace.GetApi()
	api.SetByJwt("")
	api.setHttpPostRaw(nil)
	api.setHttpGetRaw(nil)
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
				WindowId:   windowMonitor.windowId,
				ListenerId: listenerId,
			}
			windowIds, err := rpcCall[map[connect.Id]bool](self.service, "DeviceLocalRpc.AddWindowMonitorEventListener", windowListenerId, self.closeService)
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
					WindowId:   windowMonitor.windowId,
					ListenerId: listenerId,
				}
				err := rpcCallVoid(self.service, "DeviceLocalRpc.RemoveWindowMonitorEventListener", windowListenerId, self.closeService)
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

	if _, ok := self.windowMonitors[windowMonitor.windowId]; !ok {
		// window no longer active
		return &connect.WindowExpandEvent{}, map[connect.Id]*connect.ProviderEvent{}
	}

	if self.service == nil {
		return &connect.WindowExpandEvent{}, map[connect.Id]*connect.ProviderEvent{}
	}

	event, err := rpcCallNoArg[*DeviceRemoteWindowMonitorEvent](self.service, "DeviceLocalRpc.WindowMonitorEvents", self.closeService)
	if err != nil {
		return &connect.WindowExpandEvent{}, map[connect.Id]*connect.ProviderEvent{}
	}
	if event == nil {
		return &connect.WindowExpandEvent{}, map[connect.Id]*connect.ProviderEvent{}
	}

	/*
		// trim the windows
		for windowId, windowMonitor := range self.windowMonitors {
			if !event.WindowIds[windowId] {
				delete(self.windowMonitors, windowId)
				clear(windowMonitor.listeners)
			}
		}

		if _, ok := self.windowMonitors[windowMonitor.windowId]; !ok {
			// window no longer active
			return &connect.WindowExpandEvent{}, map[connect.Id]*connect.ProviderEvent{}
		}
	*/

	return event.WindowExpandEvent, event.ProviderEvents
}

// this object is locked under the DeviceRemote.stateLock
type deviceRemoteWindowMonitor struct {
	deviceRemote *DeviceRemote

	windowId  connect.Id
	listeners map[connect.Id]connect.MonitorEventFunction
}

func newDeviceRemoteWindowMonitor(deviceRemote *DeviceRemote) *deviceRemoteWindowMonitor {
	windowId := connect.NewId()

	return &deviceRemoteWindowMonitor{
		deviceRemote: deviceRemote,
		windowId:     windowId,
		listeners:    map[connect.Id]connect.MonitorEventFunction{},
	}
}

// windowMonitor

func (self *deviceRemoteWindowMonitor) AddMonitorEventCallback(monitorEventCallback connect.MonitorEventFunction) func() {
	return self.deviceRemote.windowMonitorAddMonitorEventCallback(self, monitorEventCallback)
}

func (self *deviceRemoteWindowMonitor) Events() (*connect.WindowExpandEvent, map[connect.Id]*connect.ProviderEvent) {
	return self.deviceRemote.windowMonitorEvents(self)
}

func (self *DeviceRemote) egressSecurityPolicyStats(reset bool) connect.SecurityPolicyStats {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	stats, success := func() (connect.SecurityPolicyStats, bool) {
		if self.service == nil {
			return connect.SecurityPolicyStats{}, false
		}

		stats, err := rpcCall[connect.SecurityPolicyStats](self.service, "DeviceLocalRpc.EgressSecurityPolicyStats", reset, self.closeService)
		if err != nil {
			return connect.SecurityPolicyStats{}, false
		}
		self.lastKnownState.EgressSecurityPolicyStats.Set(stats)
		return stats, true
	}()

	var out connect.SecurityPolicyStats
	if success {
		out = stats
		self.state.ResetEgressSecurityPolicyStats.Unset()
	} else if self.state.ResetEgressSecurityPolicyStats.IsSet {
		out = connect.SecurityPolicyStats{}
	} else {
		out = self.lastKnownState.EgressSecurityPolicyStats.Get(
			connect.SecurityPolicyStats{},
		)
	}

	if reset {
		state := &self.state
		if success {
			state = &self.lastKnownState
		}
		state.ResetEgressSecurityPolicyStats.Set(true)
		state.EgressSecurityPolicyStats.Unset()
	}

	return out
}

/*
func (self *DeviceRemote) resetEgressSecurityPolicyStats() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	success := func()(bool) {
		if self.service == nil {
			return false
		}

		err := rpcCallNoArgVoid(self.service, "DeviceLocalRpc.ResetEgressSecurityPolicyStats", self.closeService)
		if err != nil {
			return false
		}
		return true
	}()
	state := &self.state
	if success {
		state = &self.lastKnownState
	}
	state.ResetEgressSecurityPolicyStats.Set(true)
	state.EgressSecurityPolicyStats.Unset()
}
*/

func (self *DeviceRemote) ingressSecurityPolicyStats(reset bool) connect.SecurityPolicyStats {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	stats, success := func() (connect.SecurityPolicyStats, bool) {
		if self.service == nil {
			return connect.SecurityPolicyStats{}, false
		}

		stats, err := rpcCall[connect.SecurityPolicyStats](self.service, "DeviceLocalRpc.IngressSecurityPolicyStats", reset, self.closeService)
		if err != nil {
			return connect.SecurityPolicyStats{}, false
		}
		self.lastKnownState.IngressSecurityPolicyStats.Set(stats)
		return stats, true
	}()

	var out connect.SecurityPolicyStats
	if success {
		out = stats
		self.state.ResetIngressSecurityPolicyStats.Unset()
	} else if self.state.ResetIngressSecurityPolicyStats.IsSet {
		out = connect.SecurityPolicyStats{}
	} else {
		out = self.lastKnownState.IngressSecurityPolicyStats.Get(
			connect.SecurityPolicyStats{},
		)
	}

	if reset {
		state := &self.state
		if success {
			state = &self.lastKnownState
		}
		state.ResetIngressSecurityPolicyStats.Set(true)
		state.IngressSecurityPolicyStats.Unset()
	}

	return out
}

/*
func (self *DeviceRemote) resetIngressSecurityPolicyStats() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	success := func()(bool) {
		if self.service == nil {
			return false
		}

		err := rpcCallNoArgVoid(self.service, "DeviceLocalRpc.ResetIngressSecurityPolicyStats", self.closeService)
		if err != nil {
			return false
		}
		return true
	}()
	state := &self.state
	if success {
		state = &self.lastKnownState
	}
	state.ResetIngressSecurityPolicyStats.Set(true)
	state.IngressSecurityPolicyStats.Unset()
}
*/

type deviceRemoteEgressSecurityPolicy struct {
	deviceRemote *DeviceRemote
}

func newDeviceRemoteEgressSecurityPolicy(deviceRemote *DeviceRemote) *deviceRemoteEgressSecurityPolicy {
	return &deviceRemoteEgressSecurityPolicy{
		deviceRemote: deviceRemote,
	}
}

func (self *deviceRemoteEgressSecurityPolicy) Stats(reset bool) connect.SecurityPolicyStats {
	return self.deviceRemote.egressSecurityPolicyStats(reset)
}

// func (self *deviceRemoteEgressSecurityPolicy) ResetStats() {
// 	self.deviceRemote.resetEgressSecurityPolicyStats()
// }

type deviceRemoteIngressSecurityPolicy struct {
	deviceRemote *DeviceRemote
}

func newDeviceRemoteIngressSecurityPolicy(deviceRemote *DeviceRemote) *deviceRemoteIngressSecurityPolicy {
	return &deviceRemoteIngressSecurityPolicy{
		deviceRemote: deviceRemote,
	}
}

func (self *deviceRemoteIngressSecurityPolicy) Stats(reset bool) connect.SecurityPolicyStats {
	return self.deviceRemote.ingressSecurityPolicyStats(reset)
}

// func (self *deviceRemoteIngressSecurityPolicy) ResetStats() {
// 	self.deviceRemote.resetIngressSecurityPolicyStats()
// }

func (self *DeviceRemote) egressSecurityPolicy() securityPolicy {
	return &deviceRemoteEgressSecurityPolicy{
		deviceRemote: self,
	}
}

func (self *DeviceRemote) ingressSecurityPolicy() securityPolicy {
	return &deviceRemoteIngressSecurityPolicy{
		deviceRemote: self,
	}
}

// event dispatch

func listenerList[T any](listenerMap map[connect.Id]T) []T {
	// consistent dispatch order
	n := len(listenerMap)
	orderedKeys := maps.Keys(listenerMap)
	slices.SortFunc(orderedKeys, func(a connect.Id, b connect.Id) int {
		return a.Cmp(b)
	})
	listeners := make([]T, n, n)
	for i := 0; i < n; i += 1 {
		listeners[i] = listenerMap[orderedKeys[i]]
	}
	return listeners
}

func (self *DeviceRemote) provideChanged(provideEnabled bool) {
	listenerList := func() []ProvideChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.lastKnownState.ProvideEnabled.Set(provideEnabled)
		return listenerList(self.provideChangeListeners)
	}()
	for _, provideChangeListener := range listenerList {
		provideChangeListener.ProvideChanged(provideEnabled)
	}
}

func (self *DeviceRemote) providePausedChanged(providePaused bool) {
	listenerList := func() []ProvidePausedChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.lastKnownState.ProvidePaused.Set(providePaused)
		return listenerList(self.providePausedChangeListeners)
	}()
	for _, providePausedChangeListener := range listenerList {
		providePausedChangeListener.ProvidePausedChanged(providePaused)
	}
}

func (self *DeviceRemote) provideNetworkModeChanged(provideNetworkMode ProvideNetworkMode) {
	listenerList := func() []ProvideNetworkModeChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.lastKnownState.ProvideNetworkMode.Set(provideNetworkMode)
		return listenerList(self.provideNetworkModeChangeListeners)
	}()
	for _, provideNetworkModeChangeListener := range listenerList {
		provideNetworkModeChangeListener.ProvideNetworkModeChanged(provideNetworkMode)
	}
}

func (self *DeviceRemote) offlineChanged(offline bool, vpnInterfaceWhileOffline bool) {
	listenerList := func() []OfflineChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.lastKnownState.Offline.Set(offline)
		self.lastKnownState.VpnInterfaceWhileOffline.Set(vpnInterfaceWhileOffline)
		return listenerList(self.offlineChangeListeners)
	}()
	for _, offlineChangeListener := range listenerList {
		offlineChangeListener.OfflineChanged(offline, vpnInterfaceWhileOffline)
	}
}

func (self *DeviceRemote) connectChanged(connectEnabled bool) {
	listenerList := func() []ConnectChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.lastKnownState.ConnectEnabled.Set(connectEnabled)
		return listenerList(self.connectChangeListeners)
	}()
	for _, connectChangeListener := range listenerList {
		connectChangeListener.ConnectChanged(connectEnabled)
	}
}

func (self *DeviceRemote) routeLocalChanged(routeLocal bool) {
	listenerList := func() []RouteLocalChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.lastKnownState.RouteLocal.Set(routeLocal)
		return listenerList(self.routeLocalChangeListeners)
	}()
	for _, routeLocalChangeListener := range listenerList {
		routeLocalChangeListener.RouteLocalChanged(routeLocal)
	}
}

func (self *DeviceRemote) connectLocationChanged(deviceRemoteLocation *DeviceRemoteConnectLocation) {
	listenerList := func() []ConnectLocationChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.lastKnownState.Location.Set(deviceRemoteLocation)
		return listenerList(self.connectLocationChangeListeners)
	}()
	location := deviceRemoteLocation.toConnectLocation()
	for _, connectLocationChangeListener := range listenerList {
		connectLocationChangeListener.ConnectLocationChanged(location)
	}
}

func (self *DeviceRemote) provideSecretKeysChanged(provideSecretKeys []*ProvideSecretKey) {
	listenerList := func() []ProvideSecretKeysListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return listenerList(self.provideSecretKeysListeners)
	}()
	provideSecretKeyList := NewProvideSecretKeyList()
	provideSecretKeyList.addAll(provideSecretKeys...)
	for _, provideSecretKeyListener := range listenerList {
		provideSecretKeyListener.ProvideSecretKeysChanged(provideSecretKeyList)
	}
}

func (self *DeviceRemote) tunnelChanged(tunnelStarted bool) {
	listenerList := func() []TunnelChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.lastKnownState.TunnelStarted.Set(tunnelStarted)
		return listenerList(self.tunnelChangeListeners)
	}()
	for _, tunnelChangeListener := range listenerList {
		tunnelChangeListener.TunnelChanged(tunnelStarted)
	}
}

func (self *DeviceRemote) contractStatusChanged(contractStatus *ContractStatus) {
	listenerList := func() []ContractStatusChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.lastKnownState.ContractStatus.Set(contractStatus)
		return listenerList(self.contractStatusChangeListeners)
	}()
	for _, contractStatusChangeListener := range listenerList {
		contractStatusChangeListener.ContractStatusChanged(contractStatus)
	}
}

func (self *DeviceRemote) windowStatusChanged(windowStatus *WindowStatus) {
	listenerList := func() []WindowStatusChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.lastKnownState.WindowStatus.Set(windowStatus)
		return listenerList(self.windowStatusChangeListeners)
	}()
	for _, windowStatusChangeListener := range listenerList {
		windowStatusChangeListener.WindowStatusChanged(windowStatus)
	}
}

func (self *DeviceRemote) windowMonitorEvent(
	windowIds map[connect.Id]bool,
	windowExpandEvent *connect.WindowExpandEvent,
	providerEvents map[connect.Id]*connect.ProviderEvent,
	reset bool,
) {
	listenerLists := [][]connect.MonitorEventFunction{}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		for windowId, _ := range windowIds {
			if windowMonitor, ok := self.windowMonitors[windowId]; ok {
				listenerLists = append(listenerLists, listenerList(windowMonitor.listeners))
			}
		}
	}()
	for _, listenerList := range listenerLists {
		for _, monitorEventCallback := range listenerList {
			monitorEventCallback(windowExpandEvent, providerEvents, reset)
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
		case <-self.ctx.Done():
		case <-requestCtx.Done():
		}
	}()

	service := self.getService()

	if service != nil {
		httpRequestId := connect.NewId()
		httpRequest := &DeviceRemoteHttpRequest{
			RequestId:        httpRequestId,
			RequestUrl:       requestUrl,
			RequestBodyBytes: requestBodyBytes,
			ByJwt:            byJwt,
		}

		httpResponseChannel := make(chan *DeviceRemoteHttpResponse)

		var err error
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()

			err = rpcCallVoid(service, "DeviceLocalRpc.HttpPostRaw", httpRequest, self.closeService)
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
		case httpResponse, ok := <-httpResponseChannel:
			if !ok {
				return nil, fmt.Errorf("Done")
			}
			return httpResponse.BodyBytes, httpResponse.toError()
		case <-requestCtx.Done():
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
		case <-self.ctx.Done():
		case <-requestCtx.Done():
		}
	}()

	service := self.getService()

	if service != nil {
		httpRequestId := connect.NewId()
		httpRequest := &DeviceRemoteHttpRequest{
			RequestId:  httpRequestId,
			RequestUrl: requestUrl,
			ByJwt:      byJwt,
		}

		httpResponseChannel := make(chan *DeviceRemoteHttpResponse)

		var err error
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()

			err = rpcCallVoid(service, "DeviceLocalRpc.HttpGetRaw", httpRequest, self.closeService)
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
		case httpResponse, ok := <-httpResponseChannel:
			if !ok {
				return nil, fmt.Errorf("Done")
			}
			return httpResponse.BodyBytes, httpResponse.toError()
		case <-requestCtx.Done():
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

func (self *DeviceRemote) GetRemoteConnected() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.service != nil
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

func (self *DeviceRemote) GetProviderEnabled() bool {
	// FIXME
	return true
}

func (self *DeviceRemote) SetProviderEnabled(providerEnabled bool) {
	// FIXME
}

func (self *DeviceRemote) AddProviderChangeListener(listener ProviderChangeListener) Sub {
	// FIXME
	return nil
}

func (self *DeviceRemote) GetBlockStats() *BlockStats {
	// FIXME
	return nil
}

func (self *DeviceRemote) GetBlockActions() *BlockActionWindow {
	// FIXME
	return nil
}

func (self *DeviceRemote) OverrideBlockAction(hostPattern string, block bool) {
	// FIXME
}

func (self *DeviceRemote) RemoveBlockActionOverride(hostPattern string) {
	// FIXME
}

func (self *DeviceRemote) SetBlockActionOverrideList(blockActionOverrides *BlockActionOverrideList) {
	// FIXME
}

func (self *DeviceRemote) GetBlockEnabled() bool {
	// FIXME
	return false
}

func (self *DeviceRemote) SetBlockEnabled(blockEnabled bool) {
	// FIXME
}

func (self *DeviceRemote) GetBlockWhileDisconnected() bool {
	// FIXME
	return false
}

func (self *DeviceRemote) SetBlockWhileDisconnected(blockWhileDisconnected bool) {
	// FIXME
}

func (self *DeviceRemote) AddBlockChangeListener(listener BlockChangeListener) Sub {
	// FIXME
	return nil
}

func (self *DeviceRemote) AddBlockActionWindowChangeListener(listener BlockActionWindowChangeListener) Sub {
	// FIXME
	return nil
}

func (self *DeviceRemote) AddBlockStatsChangeListener(listener BlockStatsChangeListener) Sub {
	// FIXME
	return nil
}

// contract stats

func (self *DeviceRemote) GetEgressContractStats() *ContractStats {
	// FIXME
	return nil
}

func (self *DeviceRemote) GetEgressContractDetails() *ContractDetailsList {
	// FIXME
	return nil
}

func (self *DeviceRemote) GetIngressContractStats() *ContractStats {
	// FIXME
	return nil
}

func (self *DeviceRemote) GetIngressContractDetails() *ContractDetailsList {
	// FIXME
	return nil
}

func (self *DeviceRemote) AddEgressContratStatsChangeListener(listener ContractStatsChangeListener) Sub {
	// FIXME
	return nil
}

func (self *DeviceRemote) AddEgressContractDetailsChangeListener(listener ContractDetailsChangeListener) Sub {
	// FIXME
	return nil
}

func (self *DeviceRemote) AddIngressContratStatsChangeListener(listener ContractStatsChangeListener) Sub {
	// FIXME
	return nil
}

func (self *DeviceRemote) AddIngressContractDetailsChangeListener(listener ContractDetailsChangeListener) Sub {
	// FIXME
	return nil
}

// *important rpc note* gob encoding cannot encode fields that are not exported
// so our usual gomobile types that have private fields cannot be properly sent via rpc
// for rpc we redefine these gomobile types so that they can be gob encoded

// *important type note*
// all of the types below here should *not* be exported by gomobile
// we use a made-up annotation gomobile:noexport to try to document this
// however, the types must be exported for net.rpc to work
// this leads to some unfortunate gomobile warnings currently

// *important* argument and return values from rpc fucntios CANNOT be nil
// this is a limitation in net.rpc

//gomobile:noexport
type DeviceRemoteDestination struct {
	Location    *DeviceRemoteConnectLocation
	Specs       []*ProviderSpec
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

func (self *deviceRemoteValue[T]) Merge(update deviceRemoteValue[T]) {
	if update.IsSet {
		self.Value = update.Value
		self.IsSet = true
	}
}

//gomobile:noexport
type DeviceRemoteAddress struct {
	Ip   netip.Addr
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
		Ip:   ip,
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
	Offline                  bool
	VpnInterfaceWhileOffline bool
}

//gomobile:noexport
type DeviceRemoteConnectLocationChangeEvent struct {
	Location *DeviceRemoteConnectLocation
}

//gomobile:noexport
type DeviceRemoteContractStatus struct {
	ContractStatus *ContractStatus
}

//gomobile:noexport
type DeviceRemoteWindowStatus struct {
	WindowStatus *WindowStatus
}

//gomobile:noexport
type DeviceRemoteState struct {
	// thick state + last known state

	CanShowRatingDialog      deviceRemoteValue[bool]
	ProvideControlMode       deviceRemoteValue[ProvideControlMode]
	CanRefer                 deviceRemoteValue[bool]
	AllowForeground          deviceRemoteValue[bool]
	RouteLocal               deviceRemoteValue[bool]
	InitProvideSecretKeys    deviceRemoteValue[bool]
	LoadProvideSecretKeys    deviceRemoteValue[[]*ProvideSecretKey]
	ProvideMode              deviceRemoteValue[ProvideMode]        // auto, always, never
	ProvideNetworkMode       deviceRemoteValue[ProvideNetworkMode] // wifi or cellular + wifi
	ProvidePaused            deviceRemoteValue[bool]
	Offline                  deviceRemoteValue[bool]
	VpnInterfaceWhileOffline deviceRemoteValue[bool]
	RemoveDestination        deviceRemoteValue[bool]
	Destination              deviceRemoteValue[*DeviceRemoteDestination]

	/**
	 * Location used to connect on init
	 * if a user connects to a location, and relaunches the app, it will reconnect to this location
	 */
	Location deviceRemoteValue[*DeviceRemoteConnectLocation]

	/**
	 * Default location used to persist location
	 * if a user selects a location, connects, then disconnects, this should be persisted
	 */
	DefaultLocation                 deviceRemoteValue[*DeviceRemoteConnectLocation]
	Shuffle                         deviceRemoteValue[bool]
	ResetEgressSecurityPolicyStats  deviceRemoteValue[bool]
	ResetIngressSecurityPolicyStats deviceRemoteValue[bool]
	TunnelStarted                   deviceRemoteValue[bool]

	// only last known state

	ConnectEnabled deviceRemoteValue[bool]
	ProvideEnabled deviceRemoteValue[bool]

	EgressSecurityPolicyStats  deviceRemoteValue[connect.SecurityPolicyStats]
	IngressSecurityPolicyStats deviceRemoteValue[connect.SecurityPolicyStats]

	ContractStatus deviceRemoteValue[*ContractStatus]
	WindowStatus   deviceRemoteValue[*WindowStatus]
}

func (self *DeviceRemoteState) Unset() {
	self.CanShowRatingDialog.Unset()
	self.ProvideControlMode.Unset()
	self.CanRefer.Unset()
	self.RouteLocal.Unset()
	self.InitProvideSecretKeys.Unset()
	self.LoadProvideSecretKeys.Unset()
	self.ProvideMode.Unset()
	self.ProvideNetworkMode.Unset()
	self.ProvidePaused.Unset()
	self.Offline.Unset()
	self.VpnInterfaceWhileOffline.Unset()
	self.RemoveDestination.Unset()
	self.Destination.Unset()
	self.Location.Unset()
	self.DefaultLocation.Unset()
	self.Shuffle.Unset()
	self.ResetEgressSecurityPolicyStats.Unset()
	self.ResetIngressSecurityPolicyStats.Unset()
	self.TunnelStarted.Unset()
	self.AllowForeground.Unset()

	self.ConnectEnabled.Unset()
	self.ProvideEnabled.Unset()
	self.EgressSecurityPolicyStats.Unset()
	self.IngressSecurityPolicyStats.Unset()
	self.ContractStatus.Unset()
	self.WindowStatus.Unset()
}

func (self *DeviceRemoteState) Merge(update *DeviceRemoteState) {
	self.CanShowRatingDialog.Merge(update.CanShowRatingDialog)
	self.ProvideControlMode.Merge(update.ProvideControlMode)
	self.AllowForeground.Merge(update.AllowForeground)
	self.CanRefer.Merge(update.CanRefer)
	self.RouteLocal.Merge(update.RouteLocal)
	self.InitProvideSecretKeys.Merge(update.InitProvideSecretKeys)
	self.LoadProvideSecretKeys.Merge(update.LoadProvideSecretKeys)
	self.ProvideMode.Merge(update.ProvideMode)
	self.ProvideNetworkMode.Merge(update.ProvideNetworkMode)
	self.ProvidePaused.Merge(update.ProvidePaused)
	self.Offline.Merge(update.Offline)
	self.VpnInterfaceWhileOffline.Merge(update.VpnInterfaceWhileOffline)
	self.RemoveDestination.Merge(update.RemoveDestination)
	self.Destination.Merge(update.Destination)
	self.Location.Merge(update.Location)
	self.Shuffle.Merge(update.Shuffle)
	self.ResetEgressSecurityPolicyStats.Merge(update.ResetEgressSecurityPolicyStats)
	self.ResetIngressSecurityPolicyStats.Merge(update.ResetIngressSecurityPolicyStats)
	self.TunnelStarted.Merge(update.TunnelStarted)

	self.ConnectEnabled.Merge(update.ConnectEnabled)
	self.ProvideEnabled.Merge(update.ProvideEnabled)
	self.EgressSecurityPolicyStats.Merge(update.EgressSecurityPolicyStats)
	self.IngressSecurityPolicyStats.Merge(update.IngressSecurityPolicyStats)
	self.ContractStatus.Merge(update.ContractStatus)
	self.WindowStatus.Merge(update.WindowStatus)
}

//gomobile:noexport
type DeviceRemoteSyncRequest struct {
	ProvideChangeListenerIds         []connect.Id
	ProvidePausedChangeListenerIds   []connect.Id
	OfflineChangeListenerIds         []connect.Id
	ConnectChangeListenerIds         []connect.Id
	RouteLocalChangeListenerIds      []connect.Id
	ConnectLocationChangeListenerIds []connect.Id
	ProvideSecretKeysListenerIds     []connect.Id
	TunnelChangeListenerIds          []connect.Id
	ContractStatusChangeListenerIds  []connect.Id
	WindowStatusChangeListenerIds    []connect.Id
	WindowMonitorEventListenerIds    map[connect.Id][]connect.Id
	State                            DeviceRemoteState
}

//gomobile:noexport
type DeviceRemoteSyncResponse struct {
	// WindowIds map[connect.Id]bool
	// FIXME response cert
	RpcPublicKey string
	Error        string
}

//gomobile:noexport
type DeviceRemoteWindowListenerId struct {
	WindowId   connect.Id
	ListenerId connect.Id
}

//gomobile:noexport
type DeviceRemoteWindowMonitorEvent struct {
	WindowIds         map[connect.Id]bool
	WindowExpandEvent *connect.WindowExpandEvent
	ProviderEvents    map[connect.Id]*connect.ProviderEvent
	Reset             bool
}

//gomobile:noexport
type DeviceRemoteConnectLocation struct {
	ConnectLocation *DeviceRemoteConnectLocationValue
}

//gomobile:noexport
// type DeviceRemoteDefaultLocation struct {
// 	DefaultLocation *DeviceRemoteConnectLocationValue
// }

func newDeviceRemoteConnectLocation(connectLocation *ConnectLocation) *DeviceRemoteConnectLocation {
	deviceRemoteConnectLocation := &DeviceRemoteConnectLocation{}
	if connectLocation != nil {
		deviceRemoteConnectLocation.ConnectLocation = newDeviceRemoteConnectLocationValue(connectLocation)
	}
	return deviceRemoteConnectLocation
}

func newDeviceRemoteDefaultLocation(connectLocation *ConnectLocation) *DeviceRemoteConnectLocation {
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
		Promoted:      connectLocation.Promoted,
		MatchDistance: connectLocation.MatchDistance,

		LocationType: connectLocation.LocationType,

		City:        connectLocation.City,
		Region:      connectLocation.Region,
		Country:     connectLocation.Country,
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
		Promoted:      self.Promoted,
		MatchDistance: self.MatchDistance,

		LocationType: self.LocationType,

		City:        self.City,
		Region:      self.Region,
		Country:     self.Country,
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
		connectLocationId.LocationGroupId = newId(*self.LocationGroupId)
	}
	return connectLocationId
}

// rpc wrappers

type rpcClient = rpcClientWithTimeout

// type rpcClient = rpc.Client

type rpcClientWithTimeout struct {
	ctx         context.Context
	timeout     time.Duration
	closeClient func() error
	client      *rpc.Client
}

func (self *rpcClientWithTimeout) Call(serviceMethod string, args any, reply any) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		defer cancel()
		select {
		case <-ctx.Done():
		case <-time.After(self.timeout):
			self.closeClient()
		}
	}()
	return self.client.Call(serviceMethod, args, reply)
}

func (self *rpcClientWithTimeout) Close() error {
	return self.client.Close()
}

type RpcVoid = *any
type RpcNoArg = int

// func rpcWithTimeout(ctx context.Context, rpc func()(error), timeout time.Duration, close func()(error)) error {
// 	ctx, cancel := context.WithCancel(context.Background())
// 	defer cancel()
// 	go func() {
// 		defer cancel()
// 		select {
// 		case <- ctx.Done():
// 		case <- time.After(timeout):
// 			close()
// 		}
// 	}()
// 	return rpc()
// }

func rpcCallVoid(service *rpcClient, name string, arg any, cleanup func()) error {
	if arg == nil {
		panic("rpc cannot have nil args")
	}
	var void RpcVoid
	glog.Infof("[rpc]%s", name)
	err := service.Call(name, arg, &void)
	if err != nil {
		glog.Infof("[rpc]%s err = %s", name, err)
		cleanup()
	}
	return err
}

func rpcCallNoArgVoid(service *rpcClient, name string, cleanup func()) error {
	var noarg RpcNoArg
	var void RpcVoid
	glog.Infof("[rpc]%s", name)
	err := service.Call(name, noarg, &void)
	if err != nil {
		glog.Infof("[rpc]%s err = %s", name, err)
		cleanup()
	}
	return err
}

func rpcCallNoArg[T any](service *rpcClient, name string, cleanup func()) (T, error) {
	var noarg RpcNoArg
	var r T
	glog.Infof("[rpc]%s", name)
	err := service.Call(name, noarg, &r)
	if err != nil {
		glog.Infof("[rpc]%s err = %s", name, err)
		cleanup()
	}
	return r, err
}

func rpcCall[T any](service *rpcClient, name string, arg any, cleanup func()) (T, error) {
	if arg == nil {
		panic("rpc cannot have nil args")
	}
	var r T
	glog.Infof("[rpc]%s", name)
	err := service.Call(name, arg, &r)
	if err != nil {
		glog.Infof("[rpc]%s err = %s", name, err)
		cleanup()
	}
	return r, err
}

//gomobile:noexport
type DeviceRemoteHttpRequest struct {
	RequestId        connect.Id
	RequestUrl       string
	RequestBodyBytes []byte
	ByJwt            string
}

//gomobile:noexport
type DeviceRemoteHttpResponse struct {
	RequestId connect.Id
	BodyBytes []byte
	Error     *DeviceRemoteHttpResponseError
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

type deviceLocalRpcManager struct {
	ctx         context.Context
	cancel      context.CancelFunc
	deviceLocal *DeviceLocal
	settings    *deviceRpcSettings
}

func newDeviceLocalRpcManagerWithDefaults(
	ctx context.Context,
	deviceLocal *DeviceLocal,
) *deviceLocalRpcManager {
	return newDeviceLocalRpcManager(ctx, deviceLocal, defaultDeviceRpcSettings())
}

func newDeviceLocalRpcManager(
	ctx context.Context,
	deviceLocal *DeviceLocal,
	settings *deviceRpcSettings,
) *deviceLocalRpcManager {
	cancelCtx, cancel := context.WithCancel(ctx)

	deviceLocalRpcManager := &deviceLocalRpcManager{
		ctx:         cancelCtx,
		cancel:      cancel,
		deviceLocal: deviceLocal,
		settings:    settings,
	}

	go deviceLocalRpcManager.run()
	return deviceLocalRpcManager
}

func (self *deviceLocalRpcManager) run() {
	for {
		handleCtx, handleCancel := context.WithCancel(self.ctx)

		func() {
			defer handleCancel()

			listenConfig := &net.ListenConfig{
				KeepAliveConfig: net.KeepAliveConfig{
					Enable:   true,
					Idle:     self.settings.KeepAliveTimeout / time.Duration(2*self.settings.KeepAliveRetryCount),
					Interval: self.settings.KeepAliveTimeout / time.Duration(2*self.settings.KeepAliveRetryCount),
					Count:    self.settings.KeepAliveRetryCount,
				},
			}
			listener, err := listenConfig.Listen(handleCtx, "tcp", self.settings.Address.HostPort())
			if err != nil {
				glog.Infof("[dlrcp]listen err = %s", err)
				return
			}
			defer listener.Close()

			go func() {
				defer handleCancel()

				// handle connections serially
				for {
					select {
					case <-handleCtx.Done():
						return
					default:
					}

					conn, err := listener.Accept()
					if err != nil {
						glog.Infof("[dlrcp]listen accept err = %s", err)
						return
					}

					newDeviceLocalRpc(
						self.ctx,
						conn,
						self.deviceLocal,
						self.settings,
					)
				}
			}()

			select {
			case <-handleCtx.Done():
			}
		}()

		select {
		case <-self.ctx.Done():
			return
		case <-time.After(self.settings.RpcReconnectTimeout):
		}
	}
}

func (self *deviceLocalRpcManager) Close() {
	self.cancel()
}

// rpc are called on a single go routine

//gomobile:noexport
type DeviceLocalRpc struct {
	ctx    context.Context
	cancel context.CancelFunc

	conn                  net.Conn
	deviceLocal           *DeviceLocal
	egressSecurityPolicy  securityPolicy
	ingressSecurityPolicy securityPolicy
	settings              *deviceRpcSettings

	stateLock sync.Mutex

	provideChangeListenerIds            map[connect.Id]bool
	providePausedChangeListenerIds      map[connect.Id]bool
	provideNetworkModeChangeListenerIds map[connect.Id]bool
	offlineChangeListenerIds            map[connect.Id]bool
	connectChangeListenerIds            map[connect.Id]bool
	routeLocalChangeListenerIds         map[connect.Id]bool
	connectLocationChangeListenerIds    map[connect.Id]bool
	provideSecretKeysListenerIds        map[connect.Id]bool
	tunnelChangeListenerIds             map[connect.Id]bool
	contractStatusChangeListenerIds     map[connect.Id]bool
	windowStatusChangeListenerIds       map[connect.Id]bool

	// window id -> listener id
	windowMonitorEventListenerIds map[connect.Id]map[connect.Id]bool
	// local window id -> window id
	localWindowIds     map[connect.Id]connect.Id
	localWindowMonitor windowMonitor
	localWindowId      connect.Id

	provideChangeListenerSub            Sub
	providePausedChangeListenerSub      Sub
	offlineChangeListenerSub            Sub
	connectChangeListenerSub            Sub
	routeLocalChangeListenerSub         Sub
	connectLocationChangeListenerSub    Sub
	provideSecretKeysListenerSub        Sub
	provideNetworkModeChangeListenerSub Sub
	windowMonitorEventListenerSub       func()
	tunnelChangeListenerSub             Sub
	contractStatusChangeListenerSub     Sub
	windowStatusChangeListenerSub       Sub

	service *rpcClient
}

func newDeviceLocalRpc(
	ctx context.Context,
	conn net.Conn,
	deviceLocal *DeviceLocal,
	settings *deviceRpcSettings,
) *DeviceLocalRpc {
	cancelCtx, cancel := context.WithCancel(ctx)

	deviceLocalRpc := &DeviceLocalRpc{
		ctx:                                 cancelCtx,
		cancel:                              cancel,
		conn:                                conn,
		deviceLocal:                         deviceLocal,
		egressSecurityPolicy:                deviceLocal.egressSecurityPolicy(),
		ingressSecurityPolicy:               deviceLocal.ingressSecurityPolicy(),
		settings:                            settings,
		provideChangeListenerIds:            map[connect.Id]bool{},
		provideNetworkModeChangeListenerIds: map[connect.Id]bool{},
		providePausedChangeListenerIds:      map[connect.Id]bool{},
		offlineChangeListenerIds:            map[connect.Id]bool{},
		connectChangeListenerIds:            map[connect.Id]bool{},
		routeLocalChangeListenerIds:         map[connect.Id]bool{},
		connectLocationChangeListenerIds:    map[connect.Id]bool{},
		provideSecretKeysListenerIds:        map[connect.Id]bool{},
		windowMonitorEventListenerIds:       map[connect.Id]map[connect.Id]bool{},
		tunnelChangeListenerIds:             map[connect.Id]bool{},
		contractStatusChangeListenerIds:     map[connect.Id]bool{},
		windowStatusChangeListenerIds:       map[connect.Id]bool{},
		localWindowIds:                      map[connect.Id]connect.Id{},
	}

	go deviceLocalRpc.run()
	return deviceLocalRpc
}

func (self *DeviceLocalRpc) run() {
	defer self.cancel()
	go func() {
		defer self.conn.Close()
		select {
		case <-self.ctx.Done():
		}
	}()

	server := rpc.NewServer()
	server.Register(self)
	server.ServeConn(self.conn)
	glog.Infof("[dlrcp]server conn done")

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.closeService()
	}()
}

// must be called with state lock
func (self *DeviceLocalRpc) closeService() {
	if self.service != nil {
		self.service.Close()
		self.service = nil
	}

	// remove listeners
	for provideChangeListenerId, _ := range self.provideChangeListenerIds {
		self.removeProvideChangeListener(provideChangeListenerId)
	}
	for providePausedChangeListenerId, _ := range self.providePausedChangeListenerIds {
		self.removeProvidePausedChangeListener(providePausedChangeListenerId)
	}
	for provideNetworkModeChangeListenerId, _ := range self.provideNetworkModeChangeListenerIds {
		self.removeProvideNetworkModeChangeListener(provideNetworkModeChangeListenerId)
	}
	for offlineChangeListenerId, _ := range self.offlineChangeListenerIds {
		self.removeOfflineChangeListener(offlineChangeListenerId)
	}
	for connectChangeListenerId, _ := range self.connectChangeListenerIds {
		self.removeConnectChangeListener(connectChangeListenerId)
	}
	for routeLocalChangeListenerId, _ := range self.routeLocalChangeListenerIds {
		self.removeRouteLocalChangeListener(routeLocalChangeListenerId)
	}
	for connectLocationChangeListenerId, _ := range self.connectLocationChangeListenerIds {
		self.removeConnectLocationChangeListener(connectLocationChangeListenerId)
	}
	for provideSecretKeysListenerId, _ := range self.provideSecretKeysListenerIds {
		self.removeProvideSecretKeysListener(provideSecretKeysListenerId)
	}
	for tunnelChangeListenerId, _ := range self.tunnelChangeListenerIds {
		self.removeTunnelChangeListener(tunnelChangeListenerId)
	}
	for contractStatusChangeListenerId, _ := range self.contractStatusChangeListenerIds {
		self.removeContractStatusChangeListener(contractStatusChangeListenerId)
	}
	for windowStatusChangeListenerId, _ := range self.windowStatusChangeListenerIds {
		self.removeWindowStatusChangeListener(windowStatusChangeListenerId)
	}
	for windowId, windowMonitorEventListenerIds := range self.windowMonitorEventListenerIds {
		for windowMonitorEventListenerId, _ := range windowMonitorEventListenerIds {
			windowListenerId := DeviceRemoteWindowListenerId{
				WindowId:   windowId,
				ListenerId: windowMonitorEventListenerId,
			}
			self.removeWindowMonitorEventListener(windowListenerId)
		}
	}
}

func (self *DeviceLocalRpc) Sync(
	syncRequest *DeviceRemoteSyncRequest,
	syncResponse **DeviceRemoteSyncResponse,
) error {
	/*
		defer func() {
			if r := recover(); r != nil {
				// debug.PrintStack()
				// panic(r)

				*syncResponse = &DeviceRemoteSyncResponse{
			 		// WindowIds: self.windowIds(),
			 		// RpcPublicKey: "test",
			 		Error: fmt.Sprintf("%v", r),
			 	}
			 	returnErr = r.(error)
			}
		}()
	*/

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.closeService()

	// apply state adjustments

	state := syncRequest.State

	if state.CanShowRatingDialog.IsSet {
		self.deviceLocal.SetCanShowRatingDialog(state.CanShowRatingDialog.Value)
	}
	if state.ProvideControlMode.IsSet {
		self.deviceLocal.SetProvideControlMode(state.ProvideControlMode.Value)
	}
	if state.AllowForeground.IsSet {
		self.deviceLocal.SetAllowForeground(state.AllowForeground.Value)
	}
	if state.CanRefer.IsSet {
		self.deviceLocal.SetCanRefer(state.CanRefer.Value)
	}
	if state.RouteLocal.IsSet {
		self.deviceLocal.SetRouteLocal(state.RouteLocal.Value)
	}
	if state.InitProvideSecretKeys.IsSet {
		self.deviceLocal.InitProvideSecretKeys()
	}
	if state.LoadProvideSecretKeys.IsSet {
		provideSecretKeyList := NewProvideSecretKeyList()
		provideSecretKeyList.addAll(state.LoadProvideSecretKeys.Value...)
		self.deviceLocal.LoadProvideSecretKeys(provideSecretKeyList)
	}
	if state.ProvideMode.IsSet {
		self.deviceLocal.SetProvideMode(state.ProvideMode.Value)
	}
	if state.ProvideNetworkMode.IsSet {
		self.deviceLocal.SetProvideNetworkMode(state.ProvideNetworkMode.Value)
	}
	if state.ProvidePaused.IsSet {
		self.deviceLocal.SetProvidePaused(state.ProvidePaused.Value)
	}
	if state.Offline.IsSet {
		self.deviceLocal.SetOffline(state.Offline.Value)
	}
	if state.VpnInterfaceWhileOffline.IsSet {
		self.deviceLocal.SetVpnInterfaceWhileOffline(state.VpnInterfaceWhileOffline.Value)
	}
	if state.RemoveDestination.IsSet {
		self.deviceLocal.RemoveDestination()
	}
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
	if state.Location.IsSet {
		self.deviceLocal.SetConnectLocation(state.Location.Value.toConnectLocation())
	}
	if state.Shuffle.IsSet {
		self.deviceLocal.Shuffle()
	}

	if state.ResetEgressSecurityPolicyStats.IsSet {
		self.egressSecurityPolicy.Stats(true)
	}
	if state.ResetIngressSecurityPolicyStats.IsSet {
		self.ingressSecurityPolicy.Stats(true)
	}

	if state.TunnelStarted.IsSet {
		self.deviceLocal.SetTunnelStarted(state.TunnelStarted.Value)
	}

	// add listeners
	for _, provideChangeListenerId := range syncRequest.ProvideChangeListenerIds {
		self.addProvideChangeListener(provideChangeListenerId)
	}
	for _, providePausedChangeListenerId := range syncRequest.ProvidePausedChangeListenerIds {
		self.addProvidePausedChangeListener(providePausedChangeListenerId)
	}
	for _, offlineChangeListenerId := range syncRequest.OfflineChangeListenerIds {
		self.addOfflineChangeListener(offlineChangeListenerId)
	}
	for _, connectChangeListenerId := range syncRequest.ConnectChangeListenerIds {
		self.addConnectChangeListener(connectChangeListenerId)
	}
	for _, routeLocalChangeListenerId := range syncRequest.RouteLocalChangeListenerIds {
		self.addRouteLocalChangeListener(routeLocalChangeListenerId)
	}
	for _, connectLocationChangeListenerId := range syncRequest.ConnectLocationChangeListenerIds {
		self.addConnectLocationChangeListener(connectLocationChangeListenerId)
	}
	for _, provideSecretKeysListenerId := range syncRequest.ProvideSecretKeysListenerIds {
		self.addProvideSecretKeysListener(provideSecretKeysListenerId)
	}
	for _, tunnelChangeListenerId := range syncRequest.TunnelChangeListenerIds {
		self.addTunnelChangeListener(tunnelChangeListenerId)
	}
	for _, contractStatusChangeListenerId := range syncRequest.ContractStatusChangeListenerIds {
		self.addContractStatusChangeListener(contractStatusChangeListenerId)
	}
	for _, windowStatusChangeListenerId := range syncRequest.WindowStatusChangeListenerIds {
		self.addWindowStatusChangeListener(windowStatusChangeListenerId)
	}
	for windowId, windowMonitorEventListenerIds := range syncRequest.WindowMonitorEventListenerIds {
		for _, windowMonitorEventListenerId := range windowMonitorEventListenerIds {
			windowListenerId := DeviceRemoteWindowListenerId{
				WindowId:   windowId,
				ListenerId: windowMonitorEventListenerId,
			}
			self.addWindowMonitorEventListener(windowListenerId)
		}
	}

	*syncResponse = &DeviceRemoteSyncResponse{
		// WindowIds: self.windowIds(),
		// RpcPublicKey: "test",
	}
	return nil
}

func (self *DeviceLocalRpc) SyncReverse(responseAddress *DeviceRemoteAddress, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

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
	case <-self.ctx.Done():
		return fmt.Errorf("Done")
	default:
	}

	glog.Infof("[dlrpc]sync reverse connected")

	// self.service = rpc.NewClient(conn)
	self.service = &rpcClientWithTimeout{
		ctx:         self.ctx,
		timeout:     self.settings.RpcCallTimeout,
		closeClient: conn.Close,
		client:      rpc.NewClient(conn),
	}

	// fire listeners with the current state

	if self.provideChangeListenerSub != nil {
		self.provideChanged(self.deviceLocal.GetProvideEnabled())
	}
	if self.providePausedChangeListenerSub != nil {
		self.providePausedChanged(self.deviceLocal.GetProvidePaused())
	}
	if self.offlineChangeListenerSub != nil {
		self.offlineChanged(self.deviceLocal.GetOffline(), self.deviceLocal.GetVpnInterfaceWhileOffline())
	}
	if self.connectChangeListenerSub != nil {
		self.connectChanged(self.deviceLocal.GetConnectEnabled())
	}
	if self.routeLocalChangeListenerSub != nil {
		self.routeLocalChanged(self.deviceLocal.GetRouteLocal())
	}
	if self.connectLocationChangeListenerSub != nil {
		self.connectLocationChanged(self.deviceLocal.GetConnectLocation())
	}
	if self.provideSecretKeysListenerSub != nil {
		self.provideSecretKeysChanged(self.deviceLocal.GetProvideSecretKeys())
	}
	if self.tunnelChangeListenerSub != nil {
		self.tunnelChanged(self.deviceLocal.GetTunnelStarted())
	}
	if self.contractStatusChangeListenerSub != nil {
		self.contractStatusChanged(self.deviceLocal.GetContractStatus())
	}
	if self.windowStatusChangeListenerSub != nil {
		self.windowStatusChanged(self.deviceLocal.GetWindowStatus())
	}
	if self.localWindowMonitor != nil && self.windowMonitorEventListenerSub != nil {
		windowExpandEvent, providerEvents := self.localWindowMonitor.Events()
		self.windowMonitorEventCallback(windowExpandEvent, providerEvents, true)
	}

	return nil
}

func (self *DeviceLocalRpc) SetTunnelStarted(tunnelStarted bool, _ RpcVoid) error {
	self.deviceLocal.SetTunnelStarted(tunnelStarted)
	return nil
}

func (self *DeviceLocalRpc) GetTunnelStarted(_ RpcNoArg, tunnelStarted *bool) error {
	*tunnelStarted = self.deviceLocal.GetTunnelStarted()
	return nil
}

func (self *DeviceLocalRpc) GetContractStatus(_ RpcNoArg, status **DeviceRemoteContractStatus) error {
	*status = &DeviceRemoteContractStatus{
		ContractStatus: self.deviceLocal.GetContractStatus(),
	}
	return nil
}

func (self *DeviceLocalRpc) GetWindowStatus(_ RpcNoArg, status **DeviceRemoteWindowStatus) error {
	*status = &DeviceRemoteWindowStatus{
		WindowStatus: self.deviceLocal.GetWindowStatus(),
	}
	return nil
}

func (self *DeviceLocalRpc) AddTunnelChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addTunnelChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addTunnelChangeListener(listenerId connect.Id) {
	self.tunnelChangeListenerIds[listenerId] = true
	if self.tunnelChangeListenerSub == nil {
		self.tunnelChangeListenerSub = self.deviceLocal.AddTunnelChangeListener(self)
	}
}

func (self *DeviceLocalRpc) RemoveTunnelChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeTunnelChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeTunnelChangeListener(listenerId connect.Id) {
	delete(self.tunnelChangeListenerIds, listenerId)
	if len(self.tunnelChangeListenerIds) == 0 && self.tunnelChangeListenerSub != nil {
		self.tunnelChangeListenerSub.Close()
		self.tunnelChangeListenerSub = nil
	}
}

func (self *DeviceLocalRpc) AddContractStatusChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addContractStatusChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addContractStatusChangeListener(listenerId connect.Id) {
	self.contractStatusChangeListenerIds[listenerId] = true
	if self.contractStatusChangeListenerSub == nil {
		self.contractStatusChangeListenerSub = self.deviceLocal.AddContractStatusChangeListener(self)
	}
}

func (self *DeviceLocalRpc) RemoveContractStatusChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeContractStatusChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeContractStatusChangeListener(listenerId connect.Id) {
	delete(self.contractStatusChangeListenerIds, listenerId)
	if len(self.contractStatusChangeListenerIds) == 0 && self.contractStatusChangeListenerSub != nil {
		self.contractStatusChangeListenerSub.Close()
		self.contractStatusChangeListenerSub = nil
	}
}

func (self *DeviceLocalRpc) AddWindowStatusChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addWindowStatusChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addWindowStatusChangeListener(listenerId connect.Id) {
	self.windowStatusChangeListenerIds[listenerId] = true
	if self.windowStatusChangeListenerSub == nil {
		self.windowStatusChangeListenerSub = self.deviceLocal.AddWindowStatusChangeListener(self)
	}
}

func (self *DeviceLocalRpc) RemoveWindowStatusChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeWindowStatusChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeWindowStatusChangeListener(listenerId connect.Id) {
	delete(self.windowStatusChangeListenerIds, listenerId)
	if len(self.windowStatusChangeListenerIds) == 0 && self.windowStatusChangeListenerSub != nil {
		self.windowStatusChangeListenerSub.Close()
		self.windowStatusChangeListenerSub = nil
	}
}

// TunnelChangeListener
func (self *DeviceLocalRpc) TunnelChanged(tunnelStarted bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.tunnelChanged(tunnelStarted)
}

// must be called with stateLock
func (self *DeviceLocalRpc) tunnelChanged(tunnelStarted bool) {
	if self.service != nil {
		rpcCallVoid(self.service, "DeviceRemoteRpc.TunnelChanged", tunnelStarted, self.closeService)
	}
}

// ContractStatusChangeListener
func (self *DeviceLocalRpc) ContractStatusChanged(contractStatus *ContractStatus) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.contractStatusChanged(contractStatus)
}

// must be called with stateLock
func (self *DeviceLocalRpc) contractStatusChanged(contractStatus *ContractStatus) {
	if self.service != nil {
		status := &DeviceRemoteContractStatus{
			ContractStatus: contractStatus,
		}
		rpcCallVoid(self.service, "DeviceRemoteRpc.ContractStatusChanged", status, self.closeService)
	}
}

// WindowStatusChangeListener
func (self *DeviceLocalRpc) WindowStatusChanged(windowStatus *WindowStatus) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.windowStatusChanged(windowStatus)
}

// must be called with stateLock
func (self *DeviceLocalRpc) windowStatusChanged(windowStatus *WindowStatus) {
	if self.service != nil {
		status := &DeviceRemoteWindowStatus{
			WindowStatus: windowStatus,
		}
		rpcCallVoid(self.service, "DeviceRemoteRpc.WindowStatusChanged", status, self.closeService)
	}
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

func (self *DeviceLocalRpc) GetProvideControlMode(_ RpcNoArg, mode *ProvideControlMode) error {
	*mode = self.deviceLocal.GetProvideControlMode()
	return nil
}

func (self *DeviceLocalRpc) SetProvideControlMode(mode ProvideControlMode, _ RpcVoid) error {
	self.deviceLocal.SetProvideControlMode(mode)
	return nil
}

func (self *DeviceLocalRpc) GetAllowForeground(_ RpcNoArg, allowForeground *bool) error {
	*allowForeground = self.deviceLocal.GetAllowForeground()
	return nil
}

func (self *DeviceLocalRpc) SetAllowForeground(allowForeground bool, _ RpcVoid) error {
	self.deviceLocal.SetAllowForeground(allowForeground)
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
		rpcCallVoid(self.service, "DeviceRemoteRpc.ProvideChanged", provideEnabled, self.closeService)
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
		rpcCallVoid(self.service, "DeviceRemoteRpc.ProvidePausedChanged", providePaused, self.closeService)
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
			Offline:                  offline,
			VpnInterfaceWhileOffline: vpnInterfaceWhileOffline,
		}
		rpcCallVoid(self.service, "DeviceRemoteRpc.OfflineChanged", event, self.closeService)
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
		rpcCallVoid(self.service, "DeviceRemoteRpc.ConnectChanged", connectEnabled, self.closeService)
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
		rpcCallVoid(self.service, "DeviceRemoteRpc.RouteLocalChanged", routeLocal, self.closeService)
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
		rpcCallVoid(self.service, "DeviceRemoteRpc.ConnectLocationChanged", event, self.closeService)
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
		rpcCallVoid(self.service, "DeviceRemoteRpc.ProvideSecretKeysChanged", provideSecretKeyList.getAll(), self.closeService)
	}
}

// must be called with stateLock
func (self *DeviceLocalRpc) updateWindowMonitor() {
	localWindowMonitor := self.deviceLocal.windowMonitor()
	if self.localWindowMonitor != localWindowMonitor {
		if self.windowMonitorEventListenerSub != nil {
			self.windowMonitorEventListenerSub()
			self.windowMonitorEventListenerSub = nil
		}
		clear(self.localWindowIds)

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
	self.updateWindowMonitor()

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
	self.updateWindowMonitor()

	if self.localWindowMonitor != nil {
		windowExpandEvent, providerEvents := self.localWindowMonitor.Events()

		return &DeviceRemoteWindowMonitorEvent{
			WindowIds:         self.windowIds(),
			WindowExpandEvent: windowExpandEvent,
			ProviderEvents:    providerEvents,
			Reset:             true,
		}
	}
	return nil
}

// connect.MonitorEventFunction
func (self *DeviceLocalRpc) WindowMonitorEventCallback(
	windowExpandEvent *connect.WindowExpandEvent,
	providerEvents map[connect.Id]*connect.ProviderEvent,
	reset bool,
) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.windowMonitorEventCallback(windowExpandEvent, providerEvents, reset)
}

// must be called with stateLock
func (self *DeviceLocalRpc) windowMonitorEventCallback(
	windowExpandEvent *connect.WindowExpandEvent,
	providerEvents map[connect.Id]*connect.ProviderEvent,
	reset bool,
) {
	if self.service != nil {
		event := &DeviceRemoteWindowMonitorEvent{
			WindowIds:         self.windowIds(),
			WindowExpandEvent: windowExpandEvent,
			ProviderEvents:    providerEvents,
			Reset:             reset,
		}

		rpcCallVoid(self.service, "DeviceRemoteRpc.WindowMonitorEventCallback", event, self.closeService)
	}
}

func (self *DeviceLocalRpc) EgressSecurityPolicyStats(reset bool, stats *connect.SecurityPolicyStats) error {
	*stats = self.egressSecurityPolicy.Stats(reset)
	return nil
}

// func (self *DeviceLocalRpc) ResetEgressSecurityPolicyStats(_ RpcNoArg, _ RpcVoid) error {
// 	self.egressSecurityPolicy.ResetStats()
// 	return nil
// }

func (self *DeviceLocalRpc) IngressSecurityPolicyStats(reset bool, stats *connect.SecurityPolicyStats) error {
	*stats = self.ingressSecurityPolicy.Stats(reset)
	return nil
}

// func (self *DeviceLocalRpc) ResetIngressSecurityPolicyStats(_ RpcNoArg, _ RpcVoid) error {
// 	self.ingressSecurityPolicy.ResetStats()
// 	return nil
// }

func (self *DeviceLocalRpc) LoadProvideSecretKeys(provideSecretKeys []*ProvideSecretKey, _ RpcVoid) error {
	provideSecretKeyList := NewProvideSecretKeyList()
	provideSecretKeyList.addAll(provideSecretKeys...)
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

// ConnectChangeListener
func (self *DeviceLocalRpc) ProvideNetworkModeChanged(provideNetworkMode ProvideNetworkMode) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.provideNetworkModeChanged(provideNetworkMode)
}

// must be called with stateLock
func (self *DeviceLocalRpc) provideNetworkModeChanged(provideNetworkMode ProvideNetworkMode) {
	if self.service != nil {
		rpcCallVoid(self.service, "DeviceRemoteRpc.ProvideNetworkModeChanged", provideNetworkMode, self.closeService)
	}
}

func (self *DeviceLocalRpc) SetProvideNetworkMode(provideNetworkMode ProvideNetworkMode, _ RpcVoid) error {
	self.deviceLocal.SetProvideNetworkMode(provideNetworkMode)
	return nil
}

func (self *DeviceLocalRpc) GetProvideNetworkMode(_ RpcNoArg, provideNetworkMode *ProvideNetworkMode) error {
	*provideNetworkMode = self.deviceLocal.GetProvideNetworkMode()
	return nil
}

func (self *DeviceLocalRpc) AddProvideNetworkModeChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addProvideNetworkModeChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addProvideNetworkModeChangeListener(listenerId connect.Id) {
	self.provideNetworkModeChangeListenerIds[listenerId] = true
	if self.provideNetworkModeChangeListenerSub == nil {
		self.provideNetworkModeChangeListenerSub = self.deviceLocal.AddProvideNetworkModeChangeListener(self)
	}
}

func (self *DeviceLocalRpc) RemoveProvideNetworkModeChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeProvideNetworkModeChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeProvideNetworkModeChangeListener(listenerId connect.Id) {
	delete(self.provideNetworkModeChangeListenerIds, listenerId)
	if len(self.provideNetworkModeChangeListenerIds) == 0 && self.provideNetworkModeChangeListenerSub != nil {
		self.provideNetworkModeChangeListenerSub.Close()
		self.provideNetworkModeChangeListenerSub = nil
	}
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

func (self *DeviceLocalRpc) SetDefaultLocation(location *DeviceRemoteConnectLocation, _ RpcVoid) error {
	self.deviceLocal.SetDefaultLocation(location.toConnectLocation())
	return nil
}

func (self *DeviceLocalRpc) GetDefaultLocation(_ RpcNoArg, location **DeviceRemoteConnectLocation) error {
	*location = newDeviceRemoteDefaultLocation(self.deviceLocal.GetDefaultLocation())
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

				rpcCallVoid(self.service, "DeviceRemoteRpc.HttpResponse", httpResponse, self.closeService)
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

				rpcCallVoid(self.service, "DeviceRemoteRpc.HttpResponse", httpResponse, self.closeService)
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
	ctx          context.Context
	cancel       context.CancelFunc
	deviceRemote *DeviceRemote
}

func newDeviceRemoteRpc(ctx context.Context, deviceRemote *DeviceRemote) *DeviceRemoteRpc {
	cancelCtx, cancel := context.WithCancel(ctx)

	return &DeviceRemoteRpc{
		ctx:          cancelCtx,
		cancel:       cancel,
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
	go self.deviceRemote.connectLocationChanged(event.Location)
	return nil
}

func (self *DeviceRemoteRpc) ProvideSecretKeysChanged(provideSecretKeys []*ProvideSecretKey, _ RpcVoid) error {
	glog.Infof("[drrpc]ProvideSecretKeysChanged")
	go self.deviceRemote.provideSecretKeysChanged(provideSecretKeys)
	return nil
}

func (self *DeviceRemoteRpc) WindowMonitorEventCallback(event *DeviceRemoteWindowMonitorEvent, _ RpcVoid) error {
	glog.Infof("[drrpc]WindowMonitorEventCallback")
	go self.deviceRemote.windowMonitorEvent(
		event.WindowIds,
		event.WindowExpandEvent,
		event.ProviderEvents,
		event.Reset,
	)
	return nil
}

func (self *DeviceRemoteRpc) TunnelChanged(tunnelStarted bool, _ RpcVoid) error {
	glog.Infof("[drrpc]TunnelChanged")
	go self.deviceRemote.tunnelChanged(tunnelStarted)
	return nil
}

func (self *DeviceRemoteRpc) ContractStatusChanged(status *DeviceRemoteContractStatus, _ RpcVoid) error {
	glog.Infof("[drrpc]ContractStatusChanged")
	go self.deviceRemote.contractStatusChanged(status.ContractStatus)
	return nil
}

func (self *DeviceRemoteRpc) WindowStatusChanged(status *DeviceRemoteWindowStatus, _ RpcVoid) error {
	glog.Infof("[drrpc]WindowStatusChanged")
	go self.deviceRemote.windowStatusChanged(status.WindowStatus)
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
