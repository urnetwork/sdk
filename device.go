package sdk

import (
	"context"
	"fmt"

	// "net/netip"
	"sync"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/golang/glog"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// the device upgrades the api, including setting the client jwt
// closing the device does not close the api

// the app handling the packet transfer should instantiate `DeviceLocal`
// which has additional packet flow functions than the `Device` interface
// most users should just use the `Device` type which is compatible with
// running in multiple processes via RPC


type ProvideChangeListener interface {
	ProvideChanged(provideEnabled bool)
}

type ProvidePausedChangeListener interface {
	ProvidePausedChanged(providePaused bool)
}

type OfflineChangeListener interface {
	OfflineChanged(offline bool, vpnInterfaceWhileOffline bool)
}

type ConnectChangeListener interface {
	ConnectChanged(connectEnabled bool)
}

type RouteLocalChangeListener interface {
	RouteLocalChanged(routeLocal bool)
}

type ConnectLocationChangeListener interface {
	ConnectLocationChanged(location *ConnectLocation)
}

// FIXME rename to ProvideSecretKeysChangeListener
type ProvideSecretKeysListener interface {
	ProvideSecretKeysChanged(provideSecretKeyList *ProvideSecretKeyList)
}


// type IpProtocol = int
const (
	IpProtocolUnkown /*IpProtocol*/int = 0
	IpProtocolUdp /*IpProtocol*/int = 1
	IpProtocolTcp /*IpProtocol*/int = 2
)


// receive a packet into the local raw socket
type ReceivePacket interface {
	ReceivePacket(ipVersion int, ipProtocol /*IpProtocol*/int, packet []byte)
}

type TunnelChangeListener interface {
	TunnelChanged(tunnelStarted bool)
}

type ContractStatusChangeListener interface {
	ContractStatusChanged(contractStatus *ContractStatus)
}


type ContractStatus struct {
	InsufficientBalance bool
	NoPermission bool
	Premium bool
}


// every device must also support the unexported `device` interface
type Device interface {

	GetClientId() *Id
	GetInstanceId() *Id

	GetNetworkSpace() *NetworkSpace

	GetApi() *Api

	GetStats() *DeviceStats

	GetShouldShowRatingDialog() bool 

	GetCanShowRatingDialog() bool

	SetCanShowRatingDialog(canShowRatingDialog bool) 

	GetProvideWhileDisconnected() bool

	SetProvideWhileDisconnected(provideWhileDisconnected bool)

	GetCanRefer() bool

	SetCanRefer(canRefer bool)

	SetRouteLocal(routeLocal bool) 

	GetRouteLocal() bool

	LoadProvideSecretKeys(provideSecretKeyList *ProvideSecretKeyList)

	InitProvideSecretKeys()

	GetProvideEnabled() bool 

	GetConnectEnabled() bool 

	SetProvideMode(provideMode /*ProvideMode*/int) 

	GetProvideMode() /*ProvideMode*/int 

	SetProvidePaused(providePaused bool) 

	GetProvidePaused() bool 

	SetOffline(offline bool) 

	GetOffline() bool 

	SetVpnInterfaceWhileOffline(vpnInterfaceWhileOffline bool)

	GetVpnInterfaceWhileOffline() bool

	RemoveDestination()

	SetDestination(location *ConnectLocation, specs *ProviderSpecList, provideMode /*ProvideMode*/int)

	SetConnectLocation(location *ConnectLocation) 

	GetConnectLocation() *ConnectLocation 

	Shuffle()

	SetTunnelStarted(tunnelStarted bool)

	GetTunnelStarted() bool

	GetContractStatus() *ContractStatus

	Close()

	Cancel()

	GetDone() bool

	
	AddProvideChangeListener(listener ProvideChangeListener) Sub 

	AddProvidePausedChangeListener(listener ProvidePausedChangeListener) Sub 

	AddOfflineChangeListener(listener OfflineChangeListener) Sub 

	AddConnectChangeListener(listener ConnectChangeListener) Sub 

	AddRouteLocalChangeListener(listener RouteLocalChangeListener) Sub

	AddConnectLocationChangeListener(listener ConnectLocationChangeListener) Sub 

	AddProvideSecretKeysListener(listener ProvideSecretKeysListener) Sub 

	AddTunnelChangeListener(listener TunnelChangeListener) Sub

	AddContractStatusChangeListener(listener ContractStatusChangeListener) Sub

}

// unexported to gomobile
type device interface {
	// monitor for the current connection window
	// the client must get the window monitor each time the connection destination changes
	windowMonitor() windowMonitor
	egressSecurityPolicy() securityPolicy
	ingressSecurityPolicy() securityPolicy
}

type windowMonitor interface {
	AddMonitorEventCallback(monitorEventCallback connect.MonitorEventFunction) func()
	Events() (*connect.WindowExpandEvent, map[connect.Id]*connect.ProviderEvent)
}


type securityPolicy interface {
	Stats(reset bool) connect.SecurityPolicyStats
	// ResetStats()
}

type emptyWindowMonitor struct {
}

func (self *emptyWindowMonitor) AddMonitorEventCallback(monitorEventCallback connect.MonitorEventFunction) func() {
	return func() {}
}

func (self *emptyWindowMonitor) Events() (*connect.WindowExpandEvent, map[connect.Id]*connect.ProviderEvent) {
	return &connect.WindowExpandEvent{}, map[connect.Id]*connect.ProviderEvent{}
}


const defaultRouteLocal = true
const defaultCanShowRatingDialog = true
const defaultProvideWhileDisconnected = false
const defaultCanRefer = false
const defaultOffline = true
const defaultVpnInterfaceWhileOffline = false
const defaultTunnelStarted = false


type deviceLocalSettings struct {
	// time to give up (drop) sending a packet to a destination
	SendTimeout time.Duration
	// ClientDrainTimeout time.Duration

	NetContractStatusDuration time.Duration
	NetContractStatusCount int
}

func defaultDeviceLocalSettings() *deviceLocalSettings {
	return &deviceLocalSettings{
		SendTimeout: 4 * time.Second,
		// ClientDrainTimeout: 30 * time.Second,

		NetContractStatusDuration: 10 * time.Second,
		NetContractStatusCount: 10,
	}
}

// compile check that DeviceLocal conforms to Device, device, and ViewControllerManager
var _ Device = (*DeviceLocal)(nil)
var _ device = (*DeviceLocal)(nil)
var _ ViewControllerManager = (*DeviceLocal)(nil)
type DeviceLocal struct {
	networkSpace *NetworkSpace

	ctx    context.Context
	cancel context.CancelFunc

	byJwt string
	// platformUrl string
	// apiUrl      string

	deviceDescription string
	deviceSpec        string
	appVersion        string

	settings *deviceLocalSettings

	clientId   connect.Id
	instanceId connect.Id

	clientStrategy *connect.ClientStrategy
	// this is the client for provide
	client *connect.Client

	// contractManager *connect.ContractManager
	// routeManager *connect.RouteManager

	platformTransport *connect.PlatformTransport

	localUserNat *connect.LocalUserNat

	stats *DeviceStats

	deviceLocalRpcManager *deviceLocalRpcManager

	stateLock sync.Mutex

	connectLocation *ConnectLocation

	// when nil, packets get routed to the local user nat
	remoteUserNatClient connect.UserNatClient

	remoteUserNatProviderLocalUserNat *connect.LocalUserNat
	remoteUserNatProvider             *connect.RemoteUserNatProvider

	routeLocal          bool
	canShowRatingDialog bool
	canRefer            bool

	provideWhileDisconnected bool
	offline                  bool
	vpnInterfaceWhileOffline bool
	tunnelStarted bool

	orderedContractStatusUpdates []*contractStatusUpdate
	netContractStatus *ContractStatus

	receiveCallbacks *connect.CallbackList[connect.ReceivePacketFunction]

	provideChangeListeners         *connect.CallbackList[ProvideChangeListener]
	providePausedChangeListeners   *connect.CallbackList[ProvidePausedChangeListener]
	offlineChangeListeners         *connect.CallbackList[OfflineChangeListener]
	connectChangeListeners         *connect.CallbackList[ConnectChangeListener]
	routeLocalChangeListeners      *connect.CallbackList[RouteLocalChangeListener]
	connectLocationChangeListeners *connect.CallbackList[ConnectLocationChangeListener]
	provideSecretKeysListeners *connect.CallbackList[ProvideSecretKeysListener]
	tunnelChangeListeners *connect.CallbackList[TunnelChangeListener]
	contractStatusChangeListeners *connect.CallbackList[ContractStatusChangeListener]

	localUserNatUnsub func()

	viewControllerManager
}

func NewDeviceLocalWithDefaults(
	networkSpace *NetworkSpace,
	byJwt string,
	deviceDescription string,
	deviceSpec string,
	appVersion string,
	instanceId *Id,
	enableRpc bool,
) (*DeviceLocal, error) {
	return traceWithReturnError(
		func() (*DeviceLocal, error) {
			return newDeviceLocal(
				networkSpace,
				byJwt,
				deviceDescription,
				deviceSpec,
				appVersion,
				instanceId,
				enableRpc,
				defaultDeviceLocalSettings(),
			)
		},
	)
}

func newDeviceLocal(
	networkSpace *NetworkSpace,
	byJwt string,
	deviceDescription string,
	deviceSpec string,
	appVersion string,
	instanceId *Id,
	enableRpc bool,
	settings *deviceLocalSettings,
) (*DeviceLocal, error) {
	clientId, err := parseByJwtClientId(byJwt)
	if err != nil {
		return nil, err
	}
	return newDeviceLocalWithOverrides(
		networkSpace,
		byJwt,
		deviceDescription,
		deviceSpec,
		appVersion,
		instanceId,
		enableRpc,
		settings,
		clientId,
	)
}

func newDeviceLocalWithOverrides(
	networkSpace *NetworkSpace,
	byJwt string,
	deviceDescription string,
	deviceSpec string,
	appVersion string,
	instanceId *Id,
	enableRpc bool,
	settings *deviceLocalSettings,
	clientId connect.Id,
) (*DeviceLocal, error) {
	ctx, cancel := context.WithCancel(context.Background())
	// ctx, cancel := api.ctx, api.cancel
	apiUrl := networkSpace.apiUrl
	clientStrategy := networkSpace.clientStrategy

	clientOob := connect.NewApiOutOfBandControl(ctx, clientStrategy, byJwt, apiUrl)
	client := connect.NewClient(
		ctx,
		clientId,
		clientOob,
		// connect.DefaultClientSettingsNoNetworkEvents(),
		connect.DefaultClientSettings(),
	)

	// routeManager := connect.NewRouteManager(connectClient)
	// contractManager := connect.NewContractManagerWithDefaults(connectClient)
	// connectClient.Setup(routeManager, contractManager)
	// go connectClient.Run()

	auth := &connect.ClientAuth{
		ByJwt:      byJwt,
		InstanceId: instanceId.toConnectId(),
		AppVersion: Version,
	}
	platformTransport := connect.NewPlatformTransportWithDefaults(
		client.Ctx(),
		clientStrategy,
		client.RouteManager(),
		networkSpace.platformUrl,
		auth,
	)

	// go platformTransport.Run(connectClient.RouteManager())

	localUserNatSettings := connect.DefaultLocalUserNatSettings()
	// no ulimit for local traffic
	localUserNatSettings.UdpBufferSettings.UserLimit = 0
	localUserNatSettings.TcpBufferSettings.UserLimit = 0
	localUserNat := connect.NewLocalUserNat(client.Ctx(), clientId.String(), localUserNatSettings)

	// api := newBringYourApiWithContext(cancelCtx, clientStrategy, apiUrl)
	api := networkSpace.GetApi()
	api.SetByJwt(byJwt)

	deviceLocal := &DeviceLocal{
		networkSpace: networkSpace,
		ctx:          ctx,
		cancel:       cancel,
		byJwt:        byJwt,
		// apiUrl:            apiUrl,
		deviceDescription: deviceDescription,
		deviceSpec:        deviceSpec,
		appVersion:        appVersion,
		settings:          settings,
		clientId:          clientId,
		instanceId:        instanceId.toConnectId(),
		clientStrategy:    clientStrategy,
		client:            client,
		// contractManager: contractManager,
		// routeManager: routeManager,
		platformTransport:                 platformTransport,
		localUserNat:                      localUserNat,
		stats:                             newDeviceStats(),
		connectLocation:                   nil,
		remoteUserNatClient:               nil,
		remoteUserNatProviderLocalUserNat: nil,
		remoteUserNatProvider:             nil,
		routeLocal:                        defaultRouteLocal,
		canShowRatingDialog:               defaultCanShowRatingDialog,
		canRefer: defaultCanRefer,
		provideWhileDisconnected:          defaultProvideWhileDisconnected,
		offline:                           defaultOffline,
		vpnInterfaceWhileOffline:          defaultVpnInterfaceWhileOffline,
		tunnelStarted: defaultTunnelStarted,
		orderedContractStatusUpdates: []*contractStatusUpdate{},
		netContractStatus: nil,
		receiveCallbacks:                  connect.NewCallbackList[connect.ReceivePacketFunction](),
		provideChangeListeners:            connect.NewCallbackList[ProvideChangeListener](),
		providePausedChangeListeners:      connect.NewCallbackList[ProvidePausedChangeListener](),
		offlineChangeListeners:            connect.NewCallbackList[OfflineChangeListener](),
		connectChangeListeners:            connect.NewCallbackList[ConnectChangeListener](),
		routeLocalChangeListeners:         connect.NewCallbackList[RouteLocalChangeListener](),
		connectLocationChangeListeners:    connect.NewCallbackList[ConnectLocationChangeListener](),
		provideSecretKeysListeners:    connect.NewCallbackList[ProvideSecretKeysListener](),
		contractStatusChangeListeners: connect.NewCallbackList[ContractStatusChangeListener](),		
		tunnelChangeListeners: connect.NewCallbackList[TunnelChangeListener](),
	}
	deviceLocal.viewControllerManager = *newViewControllerManager(ctx, deviceLocal)

	// set up with nil destination
	localUserNatUnsub := localUserNat.AddReceivePacketCallback(deviceLocal.receive)
	deviceLocal.localUserNatUnsub = localUserNatUnsub

	if enableRpc {
		deviceLocal.deviceLocalRpcManager = newDeviceLocalRpcManagerWithDefaults(ctx, deviceLocal)
	}

	return deviceLocal, nil
}


type contractStatusUpdate struct {
	updateTime time.Time
	contractStatus *connect.ContractStatus
}

func (self *DeviceLocal) updateContractStatus(contractStatus *connect.ContractStatus) {
	event := false	
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		// track last n status updates and use all updates newer than M seconds
		now := time.Now()
		windowStartTime := now.Add(-self.settings.NetContractStatusDuration)
		i := max(
			0,
			len(self.orderedContractStatusUpdates) - (self.settings.NetContractStatusCount - 1),
		)
		for i < len(self.orderedContractStatusUpdates) && self.orderedContractStatusUpdates[i].updateTime.Before(windowStartTime) {
			i += 1
		}
		self.orderedContractStatusUpdates = self.orderedContractStatusUpdates[:i]
		update := &contractStatusUpdate{
			updateTime: now,
			contractStatus: contractStatus,
		}
		self.orderedContractStatusUpdates = append(self.orderedContractStatusUpdates, update)

		// summarize the update window
		netContractStatus := &ContractStatus{}
		for _, contractStatusUpdate := range self.orderedContractStatusUpdates {
			contractStatus := contractStatusUpdate.contractStatus
			if contractStatus.Error != nil {
				switch *contractStatus.Error {
				case protocol.ContractError_InsufficientBalance:
					netContractStatus.InsufficientBalance = true
				case protocol.ContractError_NoPermission:
					netContractStatus.NoPermission = true
				}
			}
			if contractStatus.Premium {
				netContractStatus.Premium = true
			}
		}

		if self.netContractStatus == nil || *self.netContractStatus != *netContractStatus {
			self.netContractStatus = netContractStatus
			event = true
		}
	}()
	if event {
		self.contractStatusChanged(self.GetContractStatus())
	}
}

func (self *DeviceLocal) AddContractStatusChangeListener(listener ContractStatusChangeListener) Sub {
	callbackId := self.contractStatusChangeListeners.Add(listener)
	return newSub(func() {
		self.contractStatusChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) contractStatusChanged(contractStatus *ContractStatus) {
	for _, contractStatusChangeListener := range self.contractStatusChangeListeners.Get() {
		connect.HandleError(func() {
			contractStatusChangeListener.ContractStatusChanged(contractStatus)
		})
	}
}


func (self *DeviceLocal) SetTunnelStarted(tunnelStarted bool) {
	event := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if self.tunnelStarted != tunnelStarted {
			self.tunnelStarted = tunnelStarted
			event = true
		}
	}()
	if event {
		self.tunnelChanged(self.GetTunnelStarted())
	}
}

func (self *DeviceLocal) GetTunnelStarted() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return self.tunnelStarted
}

func (self *DeviceLocal) AddTunnelChangeListener(listener TunnelChangeListener) Sub {
	callbackId := self.tunnelChangeListeners.Add(listener)
	return newSub(func() {
		self.tunnelChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) tunnelChanged(tunnelStarted bool) {
	for _, tunnelChangeListener := range self.tunnelChangeListeners.Get() {
		connect.HandleError(func() {
			tunnelChangeListener.TunnelChanged(tunnelStarted)
		})
	}
}


func (self *DeviceLocal) GetContractStatus() *ContractStatus {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return self.netContractStatus
}


func (self *DeviceLocal) GetClientId() *Id {
	return newId(self.clientId)
}

func (self *DeviceLocal) GetInstanceId() *Id {
	return newId(self.instanceId)
}

func (self *DeviceLocal) GetApi() *Api {
	return self.networkSpace.GetApi()
}

func (self *DeviceLocal) GetNetworkSpace() *NetworkSpace {
	return self.networkSpace
}

func (self *DeviceLocal) GetStats() *DeviceStats {
	return self.stats
}

func (self *DeviceLocal) GetShouldShowRatingDialog() bool {
	if !self.stats.GetUserSuccess() {
		return false
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.canShowRatingDialog
}

func (self *DeviceLocal) GetCanShowRatingDialog() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.canShowRatingDialog
}

func (self *DeviceLocal) SetCanShowRatingDialog(canShowRatingDialog bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.canShowRatingDialog = canShowRatingDialog
}

func (self *DeviceLocal) GetProvideWhileDisconnected() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.provideWhileDisconnected
}

func (self *DeviceLocal) SetProvideWhileDisconnected(provideWhileDisconnected bool) {

	changed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.provideWhileDisconnected != provideWhileDisconnected {
			changed = true
			self.provideWhileDisconnected = provideWhileDisconnected
		}
	}()

	if changed && !self.GetConnectEnabled() {
		if !self.GetProvideWhileDisconnected() {
			self.SetProvideMode(ProvideModeNone)
		} else {
			self.SetProvideMode(ProvideModePublic)
		}
	}

}

func (self *DeviceLocal) GetCanRefer() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.canRefer
}

func (self *DeviceLocal) SetCanRefer(canRefer bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.canRefer = canRefer
}

func (self *DeviceLocal) SetRouteLocal(routeLocal bool) {
	set := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if self.routeLocal != routeLocal {
			self.routeLocal = routeLocal
			set = true
		}
	}()
	if set {
		self.routeLocalChanged(routeLocal)
	}
}

func (self *DeviceLocal) GetRouteLocal() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return self.routeLocal
}

func (self *DeviceLocal) windowMonitor() windowMonitor {
	switch v := self.remoteUserNatClient.(type) {
	case *connect.RemoteUserNatMultiClient:
		return v.Monitor()
	default:
		// return an empty window monitor to be consistent with the device remote behavior
		return &emptyWindowMonitor{}
	}
}


type deviceLocalEgressSecurityPolicy struct {
	deviceLocal *DeviceLocal
}

func newDeviceLocalEgressSecurityPolicy(deviceLocal *DeviceLocal) *deviceLocalEgressSecurityPolicy {
	return &deviceLocalEgressSecurityPolicy{
		deviceLocal: deviceLocal,
	}	
}

func (self *deviceLocalEgressSecurityPolicy) Stats(reset bool) connect.SecurityPolicyStats {
	return self.deviceLocal.egressSecurityPolicyStats(reset)
}

// func (self *deviceLocalEgressSecurityPolicy) ResetStats() {
// 	self.deviceLocal.resetEgressSecurityPolicyStats()
// }


type deviceLocalIngressSecurityPolicy struct {
	deviceLocal *DeviceLocal
}

func newDeviceLocalIngressSecurityPolicy(deviceLocal *DeviceLocal) *deviceLocalIngressSecurityPolicy {
	return &deviceLocalIngressSecurityPolicy{
		deviceLocal: deviceLocal,
	}	
}

func (self *deviceLocalIngressSecurityPolicy) Stats(reset bool) connect.SecurityPolicyStats {
	return self.deviceLocal.ingressSecurityPolicyStats(reset)
}

// func (self *deviceLocalIngressSecurityPolicy) ResetStats() {
// 	self.deviceLocal.resetIngressSecurityPolicyStats()
// }


func (self *DeviceLocal) egressSecurityPolicy() securityPolicy {
	return &deviceLocalEgressSecurityPolicy{
		deviceLocal: self,
	}
}

func (self *DeviceLocal) ingressSecurityPolicy() securityPolicy {
	return &deviceLocalIngressSecurityPolicy{
 		deviceLocal: self,
	}
} 

func (self *DeviceLocal) egressSecurityPolicyStats(reset bool) connect.SecurityPolicyStats {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.remoteUserNatClient != nil {
		return self.remoteUserNatClient.SecurityPolicyStats(reset)
	} else {
		return connect.SecurityPolicyStats{}
	}
}

// func (self *DeviceLocal) resetEgressSecurityPolicyStats() {
// 	self.stateLock.Lock()
// 	defer self.stateLock.Unlock()

// 	if self.remoteUserNatClient != nil {
// 		self.remoteUserNatClient.ResetSecurityPolicyStats()
// 	}
// }

func (self *DeviceLocal) ingressSecurityPolicyStats(reset bool) connect.SecurityPolicyStats {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.remoteUserNatProvider != nil {
		return self.remoteUserNatProvider.SecurityPolicyStats(reset)
	} else {
		return connect.SecurityPolicyStats{}
	}
}

// func (self *DeviceLocal) resetIngressSecurityPolicyStats() {
// 	self.stateLock.Lock()
// 	defer self.stateLock.Unlock()

// 	if self.remoteUserNatProvider != nil {
// 		self.remoteUserNatProvider.ResetSecurityPolicyStats()
// 	}
// }

func (self *DeviceLocal) AddProvideChangeListener(listener ProvideChangeListener) Sub {
	callbackId := self.provideChangeListeners.Add(listener)
	return newSub(func() {
		self.provideChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) AddProvidePausedChangeListener(listener ProvidePausedChangeListener) Sub {
	callbackId := self.providePausedChangeListeners.Add(listener)
	return newSub(func() {
		self.providePausedChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) AddOfflineChangeListener(listener OfflineChangeListener) Sub {
	callbackId := self.offlineChangeListeners.Add(listener)
	return newSub(func() {
		self.offlineChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) AddConnectChangeListener(listener ConnectChangeListener) Sub {
	callbackId := self.connectChangeListeners.Add(listener)
	return newSub(func() {
		self.connectChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) AddRouteLocalChangeListener(listener RouteLocalChangeListener) Sub {
	callbackId := self.routeLocalChangeListeners.Add(listener)
	return newSub(func() {
		self.routeLocalChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) AddConnectLocationChangeListener(listener ConnectLocationChangeListener) Sub {
	callbackId := self.connectLocationChangeListeners.Add(listener)
	return newSub(func() {
		self.connectLocationChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) AddProvideSecretKeysListener(listener ProvideSecretKeysListener) Sub {
	callbackId := self.provideSecretKeysListeners.Add(listener)
	return newSub(func() {
		self.provideSecretKeysListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) provideChanged(provideEnabled bool) {
	for _, listener := range self.provideChangeListeners.Get() {
		connect.HandleError(func() {
			listener.ProvideChanged(provideEnabled)
		})
	}
}

func (self *DeviceLocal) providePausedChanged(providePaused bool) {
	for _, listener := range self.providePausedChangeListeners.Get() {
		connect.HandleError(func() {
			listener.ProvidePausedChanged(providePaused)
		})
	}
}

func (self *DeviceLocal) offlineChanged(offline bool, vpnInterfaceWhileOffline bool) {
	for _, listener := range self.offlineChangeListeners.Get() {
		connect.HandleError(func() {
			listener.OfflineChanged(offline, vpnInterfaceWhileOffline)
		})
	}
}

func (self *DeviceLocal) connectChanged(connectEnabled bool) {
	for _, listener := range self.connectChangeListeners.Get() {
		connect.HandleError(func() {
			listener.ConnectChanged(connectEnabled)
		})
	}
}

func (self *DeviceLocal) routeLocalChanged(routeLocal bool) {
	for _, listener := range self.routeLocalChangeListeners.Get() {
		connect.HandleError(func() {
			listener.RouteLocalChanged(routeLocal)
		})
	}
}

func (self *DeviceLocal) connectLocationChanged(location *ConnectLocation) {
	for _, listener := range self.connectLocationChangeListeners.Get() {
		connect.HandleError(func() {
			listener.ConnectLocationChanged(location)
		})
	}
}

func (self *DeviceLocal) provideSecretKeysChanged(provideSecretKeyList *ProvideSecretKeyList) {
	for _, listener := range self.provideSecretKeysListeners.Get() {
		connect.HandleError(func() {
			listener.ProvideSecretKeysChanged(provideSecretKeyList)
		})
	}
}

// `ReceivePacketFunction`
func (self *DeviceLocal) receive(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte) {
	// deviceLog("GOT A PACKET %d", len(packet))
	for _, receiveCallback := range self.receiveCallbacks.Get() {
		receiveCallback(source, provideMode, ipPath, packet)
	}
}

func (self *DeviceLocal) GetProvideSecretKeys() *ProvideSecretKeyList {
	provideSecretKeys := self.client.ContractManager().GetProvideSecretKeys()
	provideSecretKeyList := NewProvideSecretKeyList()
	for provideMode, provideSecretKey := range provideSecretKeys {
		provideSecretKey := &ProvideSecretKey{
			ProvideMode:      /*ProvideMode*/int(provideMode),
			ProvideSecretKey: string(provideSecretKey),
		}
		provideSecretKeyList.Add(provideSecretKey)
	}
	return provideSecretKeyList
}

func (self *DeviceLocal) LoadProvideSecretKeys(provideSecretKeyList *ProvideSecretKeyList) {
	provideSecretKeys := map[protocol.ProvideMode][]byte{}
	for i := 0; i < provideSecretKeyList.Len(); i += 1 {
		provideSecretKey := provideSecretKeyList.Get(i)
		provideMode := protocol.ProvideMode(provideSecretKey.ProvideMode)
		provideSecretKeys[provideMode] = []byte(provideSecretKey.ProvideSecretKey)
	}
	self.client.ContractManager().LoadProvideSecretKeys(provideSecretKeys)

	self.provideSecretKeysChanged(self.GetProvideSecretKeys())
}

func (self *DeviceLocal) InitProvideSecretKeys() {
	self.client.ContractManager().InitProvideSecretKeys()

	self.provideSecretKeysChanged(self.GetProvideSecretKeys())
}

func (self *DeviceLocal) GetProvideEnabled() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return self.remoteUserNatProvider != nil
}

func (self *DeviceLocal) GetConnectEnabled() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return self.remoteUserNatClient != nil
}

func (self *DeviceLocal) SetProvideMode(provideMode /*ProvideMode*/int) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		// TODO create a new provider only client?

		provideModes := map[protocol.ProvideMode]bool{}
		if ProvideModePublic <= provideMode {
			provideModes[protocol.ProvideMode_Public] = true
		}
		if ProvideModeFriendsAndFamily <= provideMode {
			provideModes[protocol.ProvideMode_FriendsAndFamily] = true
		}
		if ProvideModeNetwork <= provideMode {
			provideModes[protocol.ProvideMode_Network] = true
		}
		self.client.ContractManager().SetProvideModesWithReturnTraffic(provideModes)

		// recreate the provider user nat
		if self.remoteUserNatProviderLocalUserNat != nil {
			self.remoteUserNatProviderLocalUserNat.Close()
			self.remoteUserNatProviderLocalUserNat = nil
		}
		if self.remoteUserNatProvider != nil {
			self.remoteUserNatProvider.Close()
			self.remoteUserNatProvider = nil
		}

		if ProvideModeNone < provideMode {
			self.remoteUserNatProviderLocalUserNat = connect.NewLocalUserNatWithDefaults(self.client.Ctx(), self.clientId.String())
			self.remoteUserNatProvider = connect.NewRemoteUserNatProviderWithDefaults(self.client, self.remoteUserNatProviderLocalUserNat)
		}
	}()
	self.provideChanged(self.GetProvideEnabled())
}

func (self *DeviceLocal) GetProvideMode() /*ProvideMode*/int {
	maxProvideMode := protocol.ProvideMode_None
	for provideMode, _ := range self.client.ContractManager().GetProvideModes() {
		maxProvideMode = max(maxProvideMode, provideMode)
	}
	return /*ProvideMode*/int(maxProvideMode)
}

func (self *DeviceLocal) SetProvidePaused(providePaused bool) {
	glog.Infof("[device]provide paused = %t\n", providePaused)

	if self.client.ContractManager().SetProvidePaused(providePaused) {
		self.providePausedChanged(self.GetProvidePaused())
	}
}

func (self *DeviceLocal) GetProvidePaused() bool {
	return self.client.ContractManager().IsProvidePaused()
}

func (self *DeviceLocal) SetOffline(offline bool) {
	glog.Infof("[device]offline = %t\n", offline)

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.offline = offline
	}()
	self.offlineChanged(self.GetOffline(), self.GetVpnInterfaceWhileOffline())
}

func (self *DeviceLocal) GetOffline() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.offline
}

func (self *DeviceLocal) SetVpnInterfaceWhileOffline(vpnInterfaceWhileOffline bool) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.vpnInterfaceWhileOffline = vpnInterfaceWhileOffline
	}()
	self.offlineChanged(self.GetOffline(), self.GetVpnInterfaceWhileOffline())
}

func (self *DeviceLocal) GetVpnInterfaceWhileOffline() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.vpnInterfaceWhileOffline
}

func (self *DeviceLocal) RemoveDestination() {
	self.SetDestination(nil, nil, ProvideModeNone)
}

func (self *DeviceLocal) SetDestination(location *ConnectLocation, specs *ProviderSpecList, provideMode /*ProvideMode*/int) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		self.connectLocation = location

		if self.remoteUserNatClient != nil {
			self.remoteUserNatClient.Close()
			self.remoteUserNatClient = nil
		}

		if specs != nil && 0 < specs.Len() {
			connectSpecs := []*connect.ProviderSpec{}
			for i := 0; i < specs.Len(); i += 1 {
				connectSpecs = append(connectSpecs, specs.Get(i).toConnectProviderSpec())
			}

			generator := connect.NewApiMultiClientGenerator(
				self.ctx,
				connectSpecs,
				self.clientStrategy,
				// exclude self
				[]connect.Id{self.clientId},
				self.networkSpace.apiUrl,
				self.byJwt,
				self.networkSpace.platformUrl,
				self.deviceDescription,
				self.deviceSpec,
				self.appVersion,
				// connect.DefaultClientSettingsNoNetworkEvents,
				connect.DefaultClientSettings,
				connect.DefaultApiMultiClientGeneratorSettings(),
			)
			remoteReceive := func(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte) {
				self.stats.UpdateRemoteReceive(/*ByteCount*/int64(len(packet)))
				self.receive(source, provideMode, ipPath, packet)
			}
			multi := connect.NewRemoteUserNatMultiClientWithDefaults(
				self.ctx,
				generator,
				remoteReceive,
				protocol.ProvideMode_Network,
			)
			multi.AddContractStatusCallback(self.updateContractStatus)
			self.remoteUserNatClient = multi
		}
		// else no specs, not an error
	}()
	self.connectLocationChanged(self.GetConnectLocation())
	connectEnabled := self.GetConnectEnabled()
	self.stats.UpdateConnect(connectEnabled)
	self.connectChanged(connectEnabled)
}

func (self *DeviceLocal) SetConnectLocation(location *ConnectLocation) {
	if location == nil {
		self.RemoveDestination()
	} else {
		specs := NewProviderSpecList()
		specs.Add(&ProviderSpec{
			LocationId:      location.ConnectLocationId.LocationId,
			LocationGroupId: location.ConnectLocationId.LocationGroupId,
			ClientId:        location.ConnectLocationId.ClientId,
			BestAvailable:   location.ConnectLocationId.BestAvailable,
		})

		self.SetDestination(location, specs, ProvideModePublic)
	}
}

func (self *DeviceLocal) GetConnectLocation() *ConnectLocation {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.connectLocation
}

func (self *DeviceLocal) Shuffle() {
	var remoteUserNatClient connect.UserNatClient
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		remoteUserNatClient = self.remoteUserNatClient
	}()

	if remoteUserNatClient != nil {
		remoteUserNatClient.Shuffle()
	}
}

func (self *DeviceLocal) SendPacket(packet []byte, n int32) bool {
	packetCopy := make([]byte, n)
	copy(packetCopy, packet[0:n])
	source := connect.SourceId(self.clientId)

	var remoteUserNatClient connect.UserNatClient
	var localUserNat *connect.LocalUserNat
	var routeLocal bool
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		remoteUserNatClient = self.remoteUserNatClient
		localUserNat = self.localUserNat
		routeLocal = self.routeLocal
	}()

	if remoteUserNatClient != nil {
		self.stats.UpdateRemoteSend(/*ByteCount*/int64(n))
		return remoteUserNatClient.SendPacket(
			source,
			protocol.ProvideMode_Network,
			packetCopy,
			self.settings.SendTimeout,
		)
	} else if routeLocal {
		// route locally
		return localUserNat.SendPacket(
			source,
			protocol.ProvideMode_Network,
			packetCopy,
			self.settings.SendTimeout,
		)
	} else {
		return false
	}
}

func (self *DeviceLocal) AddReceivePacket(receivePacket ReceivePacket) Sub {
	receive := func(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte) {
		var ipProtocol /*IpProtocol*/int
		switch ipPath.Protocol {
		case connect.IpProtocolUdp:
			ipProtocol = IpProtocolUdp
		case connect.IpProtocolTcp:
			ipProtocol = IpProtocolTcp
		default:
			ipProtocol = IpProtocolUnkown
		}

		receivePacket.ReceivePacket(ipPath.Version, ipProtocol, packet)
	}
	callbackId := self.receiveCallbacks.Add(receive)
	return newSub(func() {
		self.receiveCallbacks.Remove(callbackId)
	})
}

func (self *DeviceLocal) Cancel() {
	self.cancel()
}


func (self *DeviceLocal) Close() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.cancel()

	self.client.Cancel()

	if self.remoteUserNatClient != nil {
		self.remoteUserNatClient.Close()
		self.remoteUserNatClient = nil
	}
	// self.localUserNat.RemoveReceivePacketCallback(self.receive)
	self.localUserNatUnsub()
	if self.remoteUserNatProviderLocalUserNat != nil {
		self.remoteUserNatProviderLocalUserNat.Close()
		self.remoteUserNatProviderLocalUserNat = nil
	}
	if self.remoteUserNatProvider != nil {
		self.remoteUserNatProvider.Close()
		self.remoteUserNatProvider = nil
	}

	self.localUserNat.Close()

	if self.deviceLocalRpcManager != nil {
		self.deviceLocalRpcManager.Close()
	}

	api := self.networkSpace.GetApi()
	api.SetByJwt("")
}

func (self *DeviceLocal) GetDone() bool {
	select {
	case <-self.ctx.Done():
		return true
	default:
		return false
	}
}

func parseByJwtClientId(byJwt string) (connect.Id, error) {
	claims := gojwt.MapClaims{}
	gojwt.NewParser().ParseUnverified(byJwt, claims)

	jwtClientId, ok := claims["client_id"]
	if !ok {
		return connect.Id{}, fmt.Errorf("byJwt does not contain claim client_id")
	}
	switch v := jwtClientId.(type) {
	case string:
		return connect.ParseId(v)
	default:
		return connect.Id{}, fmt.Errorf("byJwt have invalid type for client_id: %T", v)
	}
}

/*
type WindowEvents struct {
	windowExpandEvent *connect.WindowExpandEvent
	providerEvents    map[connect.Id]*connect.ProviderEvent
}

func newWindowEvents(
	windowExpandEvent *connect.WindowExpandEvent,
	providerEvents map[connect.Id]*connect.ProviderEvent,
) *WindowEvents {
	return &WindowEvents{
		windowExpandEvent: windowExpandEvent,
		providerEvents:    providerEvents,
	}
}

func (self *WindowEvents) CurrentSize() int {
	count := 0
	for _, providerEvent := range self.providerEvents {
		if providerEvent.State.IsActive() {
			count += 1
		}
	}
	return count
}

func (self *WindowEvents) TargetSize() int {
	return self.windowExpandEvent.TargetSize
}

func (self *WindowEvents) InEvaluationClientCount() int {
	count := 0
	for _, providerEvent := range self.providerEvents {
		if providerEvent.State == connect.ProviderStateInEvaluation {
			count += 1
		}
	}
	return count
}

func (self *WindowEvents) AddedClientCount() int {
	count := 0
	for _, providerEvent := range self.providerEvents {
		if providerEvent.State == connect.ProviderStateAdded {
			count += 1
		}
	}
	return count
}

func (self *WindowEvents) NotAddedClientCount() int {
	count := 0
	for _, providerEvent := range self.providerEvents {
		if providerEvent.State == connect.ProviderStateNotAdded {
			count += 1
		}
	}
	return count
}

func (self *WindowEvents) EvaluationFailedClientCount() int {
	count := 0
	for _, providerEvent := range self.providerEvents {
		if providerEvent.State == connect.ProviderStateEvaluationFailed {
			count += 1
		}
	}
	return count
}
*/
