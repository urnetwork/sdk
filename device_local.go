package sdk

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	// "net/netip"
	"sync"
	// "sync/atomic"
	"time"
	// "os"
	// "syscall"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

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
const defaultCanShowIntroFunnel = true

const defaultProvideControlMode = ProvideControlModeManual
const defaultProvideNetworkMode = ProvideNetworkModeWiFi
const defaultCanRefer = false
const defaultAllowForeground = false
const defaultOffline = true
const defaultVpnInterfaceWhileOffline = false
const defaultTunnelStarted = false

type deviceLocalSettings struct {
	// time to give up (drop) sending a packet to a destination
	SendTimeout time.Duration
	// ClientDrainTimeout time.Duration

	NetContractStatusDuration time.Duration
	NetContractStatusCount    int
}

func defaultDeviceLocalSettings() *deviceLocalSettings {
	return &deviceLocalSettings{
		SendTimeout: 4 * time.Second,
		// ClientDrainTimeout: 30 * time.Second,

		NetContractStatusDuration: 10 * time.Second,
		NetContractStatusCount:    10,
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
	// stateLockGoid atomic.Int64

	connectLocation *ConnectLocation // reconnects when launched
	defaultLocation *ConnectLocation // persisting the location after the client has disconnected

	// when nil, packets get routed to the local user nat
	remoteUserNatClient connect.UserNatClient
	contractStatusSub   func()
	windowMonitorSub    func()

	remoteUserNatProviderLocalUserNat *connect.LocalUserNat
	remoteUserNatProvider             *connect.RemoteUserNatProvider

	routeLocal           bool
	canShowRatingDialog  bool
	canPromptIntroFunnel bool
	canRefer             bool
	allowForeground      bool

	provideMode              ProvideMode
	provideControlMode       ProvideControlMode // auto, always, never
	provideNetworkMode       ProvideNetworkMode // wifi, cellular
	offline                  bool
	vpnInterfaceWhileOffline bool
	tunnelStarted            bool

	orderedContractStatusUpdates []*contractStatusUpdate
	netContractStatus            *ContractStatus

	receiveCallbacks *connect.CallbackList[connect.ReceivePacketFunction]

	provideChangeListeners            *connect.CallbackList[ProvideChangeListener]
	providePausedChangeListeners      *connect.CallbackList[ProvidePausedChangeListener]
	provideNetworkModeChangeListeners *connect.CallbackList[ProvideNetworkModeChangeListener]
	offlineChangeListeners            *connect.CallbackList[OfflineChangeListener]
	connectChangeListeners            *connect.CallbackList[ConnectChangeListener]
	routeLocalChangeListeners         *connect.CallbackList[RouteLocalChangeListener]
	connectLocationChangeListeners    *connect.CallbackList[ConnectLocationChangeListener]
	provideSecretKeysListeners        *connect.CallbackList[ProvideSecretKeysListener]
	tunnelChangeListeners             *connect.CallbackList[TunnelChangeListener]
	contractStatusChangeListeners     *connect.CallbackList[ContractStatusChangeListener]
	windowStatusChangeListeners       *connect.CallbackList[WindowStatusChangeListener]

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
		defaultLocation:                   nil,
		remoteUserNatClient:               nil,
		remoteUserNatProviderLocalUserNat: nil,
		remoteUserNatProvider:             nil,
		routeLocal:                        defaultRouteLocal,
		canShowRatingDialog:               defaultCanShowRatingDialog,
		canPromptIntroFunnel:              defaultCanShowIntroFunnel,
		canRefer:                          defaultCanRefer,
		allowForeground:                   defaultAllowForeground,
		provideMode:                       ProvideModeNone,
		provideControlMode:                defaultProvideControlMode,
		provideNetworkMode:                defaultProvideNetworkMode,
		offline:                           defaultOffline,
		vpnInterfaceWhileOffline:          defaultVpnInterfaceWhileOffline,
		tunnelStarted:                     defaultTunnelStarted,
		orderedContractStatusUpdates:      []*contractStatusUpdate{},
		netContractStatus:                 &ContractStatus{},
		receiveCallbacks:                  connect.NewCallbackList[connect.ReceivePacketFunction](),
		provideChangeListeners:            connect.NewCallbackList[ProvideChangeListener](),
		providePausedChangeListeners:      connect.NewCallbackList[ProvidePausedChangeListener](),
		provideNetworkModeChangeListeners: connect.NewCallbackList[ProvideNetworkModeChangeListener](),
		offlineChangeListeners:            connect.NewCallbackList[OfflineChangeListener](),
		connectChangeListeners:            connect.NewCallbackList[ConnectChangeListener](),
		routeLocalChangeListeners:         connect.NewCallbackList[RouteLocalChangeListener](),
		connectLocationChangeListeners:    connect.NewCallbackList[ConnectLocationChangeListener](),
		provideSecretKeysListeners:        connect.NewCallbackList[ProvideSecretKeysListener](),
		contractStatusChangeListeners:     connect.NewCallbackList[ContractStatusChangeListener](),
		tunnelChangeListeners:             connect.NewCallbackList[TunnelChangeListener](),
		windowStatusChangeListeners:       connect.NewCallbackList[WindowStatusChangeListener](),
	}
	deviceLocal.viewControllerManager = *newViewControllerManager(ctx, deviceLocal)

	// set up with nil destination
	localUserNatUnsub := localUserNat.AddReceivePacketCallback(deviceLocal.receive)
	deviceLocal.localUserNatUnsub = localUserNatUnsub

	if enableRpc {
		deviceLocal.deviceLocalRpcManager = newDeviceLocalRpcManagerWithDefaults(ctx, deviceLocal)
	} else {
		newSecurityPolicyMonitor(ctx, deviceLocal)
	}

	return deviceLocal, nil
}

// func (self *DeviceLocal) lock() {
// 	goid := goid()
// 	lockGoid := self.stateLockGoid.Load()
// 	if goid == lockGoid {
// 		panic(fmt.Errorf("Recursive lock"))
// 	}
// 	self.stateLock.Lock()
// 	self.stateLockGoid.Store(goid)
// }

// func (self *DeviceLocal) unlock() {
// 	self.stateLockGoid.Store(0)
// 	self.stateLock.Unlock()
// }

// func (self *DeviceLocal) assertNotLockOwner() {
// 	goid := goid()
// 	lockGoid := self.stateLockGoid.Load()
// 	if goid == lockGoid {
// 		debug.PrintStack()
// 	}
// }

type contractStatusUpdate struct {
	updateTime     time.Time
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
			len(self.orderedContractStatusUpdates)-(self.settings.NetContractStatusCount-1),
		)
		for i < len(self.orderedContractStatusUpdates) && self.orderedContractStatusUpdates[i].updateTime.Before(windowStartTime) {
			i += 1
		}
		self.orderedContractStatusUpdates = self.orderedContractStatusUpdates[:i]
		update := &contractStatusUpdate{
			updateTime:     now,
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
					glog.Infof("[contract]error insufficent balance\n")
				case protocol.ContractError_NoPermission:
					netContractStatus.NoPermission = true
					glog.Infof("[contract]error no permission\n")
				}
			} else {
				// reset the error state
				netContractStatus.InsufficientBalance = false
				netContractStatus.NoPermission = false
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

/**
 * Prompt Intro tunnel
 */
func (self *DeviceLocal) GetCanPromptIntroFunnel() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.canPromptIntroFunnel
}

func (self *DeviceLocal) SetCanPromptIntroFunnel(canPrompt bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.canPromptIntroFunnel = canPrompt
}

/**
 * Get provide network mode.
 * for example, auto, always, never
 */
func (self *DeviceLocal) GetProvideControlMode() ProvideControlMode {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.provideControlMode
}

/**
 * Set provide network mode.
 * auto, always, never
 */
func (self *DeviceLocal) SetProvideControlMode(provideControlMode ProvideControlMode) {
	provideModeChanged := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.provideControlMode != provideControlMode {
			self.provideControlMode = provideControlMode

			switch provideControlMode {
			case ProvideControlModeAuto:
				if self.remoteUserNatClient != nil {
					// if user is connected, start providing
					provideModeChanged = self.setProvideModeWithLock(ProvideModePublic)
				} else {
					// if user is not connected, stop providing
					provideModeChanged = self.setProvideModeWithLock(ProvideModeNone)
				}
			case ProvideControlModeAlways:
				provideModeChanged = self.setProvideModeWithLock(ProvideModePublic)
			default:
				provideModeChanged = self.setProvideModeWithLock(ProvideModeNone)
			}
		}
	}()

	if provideModeChanged {
		self.provideChanged(self.GetProvideEnabled())
	}
}

/**
 * Get provide network mode.
 * for example, wifi, cellular
 */
func (self *DeviceLocal) GetProvideNetworkMode() ProvideNetworkMode {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.provideNetworkMode
}

func (self *DeviceLocal) SetProvideNetworkMode(mode ProvideNetworkMode) {
	set := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if self.provideNetworkMode != mode {
			self.provideNetworkMode = mode
			set = true
		}
	}()
	if set {
		glog.Infof("Set provide network mode: %s", mode)
		self.provideNetworkModeChanged(mode)
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

func (self *DeviceLocal) GetAllowForeground() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.allowForeground
}

func (self *DeviceLocal) SetAllowForeground(allowForeground bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.allowForeground = allowForeground
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

func (self *DeviceLocal) AddProvideNetworkModeChangeListener(listener ProvideNetworkModeChangeListener) Sub {
	callbackId := self.provideNetworkModeChangeListeners.Add(listener)
	return newSub(func() {
		self.provideNetworkModeChangeListeners.Remove(callbackId)
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

func (self *DeviceLocal) AddContractStatusChangeListener(listener ContractStatusChangeListener) Sub {
	callbackId := self.contractStatusChangeListeners.Add(listener)
	return newSub(func() {
		self.contractStatusChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) AddTunnelChangeListener(listener TunnelChangeListener) Sub {
	callbackId := self.tunnelChangeListeners.Add(listener)
	return newSub(func() {
		self.tunnelChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) AddWindowStatusChangeListener(listener WindowStatusChangeListener) Sub {
	callbackId := self.windowStatusChangeListeners.Add(listener)
	return newSub(func() {
		self.windowStatusChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) provideChanged(provideEnabled bool) {
	// self.assertNotLockOwner()
	for _, listener := range self.provideChangeListeners.Get() {
		connect.HandleError(func() {
			listener.ProvideChanged(provideEnabled)
		})
	}
}

func (self *DeviceLocal) providePausedChanged(providePaused bool) {
	// self.assertNotLockOwner()
	for _, listener := range self.providePausedChangeListeners.Get() {
		connect.HandleError(func() {
			listener.ProvidePausedChanged(providePaused)
		})
	}
}

func (self *DeviceLocal) provideNetworkModeChanged(provideNetworkMode ProvideNetworkMode) {

	for _, listener := range self.provideNetworkModeChangeListeners.Get() {
		connect.HandleError(func() {
			listener.ProvideNetworkModeChanged(provideNetworkMode)
		})
	}
}

func (self *DeviceLocal) offlineChanged(offline bool, vpnInterfaceWhileOffline bool) {
	// self.assertNotLockOwner()
	for _, listener := range self.offlineChangeListeners.Get() {
		connect.HandleError(func() {
			listener.OfflineChanged(offline, vpnInterfaceWhileOffline)
		})
	}
}

func (self *DeviceLocal) connectChanged(connectEnabled bool) {
	// self.assertNotLockOwner()
	for _, listener := range self.connectChangeListeners.Get() {
		connect.HandleError(func() {
			listener.ConnectChanged(connectEnabled)
		})
	}
}

func (self *DeviceLocal) routeLocalChanged(routeLocal bool) {
	// self.assertNotLockOwner()
	for _, listener := range self.routeLocalChangeListeners.Get() {
		connect.HandleError(func() {
			listener.RouteLocalChanged(routeLocal)
		})
	}
}

func (self *DeviceLocal) connectLocationChanged(location *ConnectLocation) {
	// self.assertNotLockOwner()
	for _, listener := range self.connectLocationChangeListeners.Get() {
		connect.HandleError(func() {
			listener.ConnectLocationChanged(location)
		})
	}
}

func (self *DeviceLocal) provideSecretKeysChanged(provideSecretKeyList *ProvideSecretKeyList) {
	// self.assertNotLockOwner()
	for _, listener := range self.provideSecretKeysListeners.Get() {
		connect.HandleError(func() {
			listener.ProvideSecretKeysChanged(provideSecretKeyList)
		})
	}
}

func (self *DeviceLocal) contractStatusChanged(contractStatus *ContractStatus) {
	// self.assertNotLockOwner()
	for _, contractStatusChangeListener := range self.contractStatusChangeListeners.Get() {
		connect.HandleError(func() {
			contractStatusChangeListener.ContractStatusChanged(contractStatus)
		})
	}
}

func (self *DeviceLocal) tunnelChanged(tunnelStarted bool) {
	// self.assertNotLockOwner()
	for _, tunnelChangeListener := range self.tunnelChangeListeners.Get() {
		connect.HandleError(func() {
			tunnelChangeListener.TunnelChanged(tunnelStarted)
		})
	}
}

func (self *DeviceLocal) windowStatusChanged(windowStatus *WindowStatus) {
	// self.assertNotLockOwner()
	for _, listener := range self.windowStatusChangeListeners.Get() {
		connect.HandleError(func() {
			listener.WindowStatusChanged(windowStatus)
		})
	}
}

// `ReceivePacketFunction`
func (self *DeviceLocal) receive(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte) {
	// self.assertNotLockOwner()
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
			ProvideMode:      ProvideMode(provideMode),
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

func (self *DeviceLocal) SetProvideMode(provideMode ProvideMode) {
	glog.Infof("[device]provide = %d\n", provideMode)

	changed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		changed = self.setProvideModeWithLock(provideMode)
	}()
	if changed {
		self.provideChanged(self.GetProvideEnabled())
	}
}

func (self *DeviceLocal) setProvideModeWithLock(provideMode ProvideMode) (changed bool) {
	if self.provideMode != provideMode {
		self.provideMode = provideMode
		changed = true

		// TODO create a new provider only client?

		if ProvideModeNone < provideMode {
			// recreate the provider user nat only as needed
			// this avoid connection disruptions
			if self.remoteUserNatProviderLocalUserNat == nil {
				self.remoteUserNatProviderLocalUserNat = connect.NewLocalUserNatWithDefaults(self.client.Ctx(), self.clientId.String())
			}
			if self.remoteUserNatProvider == nil {
				self.remoteUserNatProvider = connect.NewRemoteUserNatProviderWithDefaults(self.client, self.remoteUserNatProviderLocalUserNat)
			}
		} else {
			// close
			if self.remoteUserNatProviderLocalUserNat != nil {
				self.remoteUserNatProviderLocalUserNat.Close()
				self.remoteUserNatProviderLocalUserNat = nil
			}
			if self.remoteUserNatProvider != nil {
				self.remoteUserNatProvider.Close()
				self.remoteUserNatProvider = nil
			}
		}

		provideModes := map[protocol.ProvideMode]bool{}
		switch provideMode {
		case ProvideModePublic:
			provideModes[protocol.ProvideMode_Public] = true
			provideModes[protocol.ProvideMode_FriendsAndFamily] = true
			provideModes[protocol.ProvideMode_Network] = true
		case ProvideModeFriendsAndFamily:
			provideModes[protocol.ProvideMode_FriendsAndFamily] = true
			provideModes[protocol.ProvideMode_Network] = true
		case ProvideModeNetwork:
			provideModes[protocol.ProvideMode_Network] = true
		}

		self.client.ContractManager().SetProvideModesWithReturnTraffic(provideModes)
	}
	return
}

func (self *DeviceLocal) GetProvideMode() ProvideMode {
	// maxProvideMode := protocol.ProvideMode_None
	// for provideMode, _ := range self.client.ContractManager().GetProvideModes() {
	// 	maxProvideMode = max(maxProvideMode, provideMode)
	// }
	// return ProvideMode(maxProvideMode)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.provideMode
}

func (self *DeviceLocal) SetProvidePaused(providePaused bool) {
	if self.client.ContractManager().SetProvidePaused(providePaused) {
		glog.Infof("[device]provide paused = %t\n", providePaused)
		self.providePausedChanged(self.GetProvidePaused())
	}
}

func (self *DeviceLocal) GetProvidePaused() bool {
	return self.client.ContractManager().IsProvidePaused()
}

func (self *DeviceLocal) SetOffline(offline bool) {
	changed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.offline != offline {
			self.offline = offline
			changed = true
		}
	}()
	if changed {
		glog.Infof("[device]offline = %t\n", offline)
		self.offlineChanged(self.GetOffline(), self.GetVpnInterfaceWhileOffline())
	}
}

func (self *DeviceLocal) GetOffline() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.offline
}

func (self *DeviceLocal) SetVpnInterfaceWhileOffline(vpnInterfaceWhileOffline bool) {
	changed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.vpnInterfaceWhileOffline != vpnInterfaceWhileOffline {
			self.vpnInterfaceWhileOffline = vpnInterfaceWhileOffline
			changed = true
		}
	}()
	if changed {
		self.offlineChanged(self.GetOffline(), self.GetVpnInterfaceWhileOffline())
	}
}

func (self *DeviceLocal) GetVpnInterfaceWhileOffline() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.vpnInterfaceWhileOffline
}

func (self *DeviceLocal) RemoveDestination() {
	self.SetDestination(nil, nil, ProvideModeNone)
}

func (self *DeviceLocal) SetDestination(location *ConnectLocation, specs *ProviderSpecList, provideMode ProvideMode) {
	provideModeChanged := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		self.connectLocation = location

		if self.contractStatusSub != nil {
			self.contractStatusSub()
			self.contractStatusSub = nil
		}
		if self.windowMonitorSub != nil {
			self.windowMonitorSub()
			self.windowMonitorSub = nil
		}
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
				&self.clientId,
				// connect.DefaultClientSettingsNoNetworkEvents,
				connect.DefaultClientSettings,
				connect.DefaultApiMultiClientGeneratorSettings(),
			)
			remoteReceive := func(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte) {
				self.stats.UpdateRemoteReceive(ByteCount(len(packet)))
				self.receive(source, provideMode, ipPath, packet)
			}
			multi := connect.NewRemoteUserNatMultiClientWithDefaults(
				self.ctx,
				generator,
				remoteReceive,
				protocol.ProvideMode_Public,
			)
			self.contractStatusSub = multi.AddContractStatusCallback(self.updateContractStatus)
			self.remoteUserNatClient = multi

			monitor := multi.Monitor()
			windowMonitorEvent := func(windowExpandEvent *connect.WindowExpandEvent, providerEvents map[connect.Id]*connect.ProviderEvent, reset bool) {
				self.windowStatusChanged(toWindowStatus(monitor))
			}
			self.windowMonitorSub = monitor.AddMonitorEventCallback(windowMonitorEvent)

			if self.provideControlMode == ProvideControlModeAuto {
				provideModeChanged = self.setProvideModeWithLock(ProvideModePublic)
			}
		} else {
			// else no specs, not an error
			if self.provideControlMode == ProvideControlModeAuto {
				provideModeChanged = self.setProvideModeWithLock(ProvideModeNone)
			}
		}
	}()

	self.connectLocationChanged(self.GetConnectLocation())
	connectEnabled := self.GetConnectEnabled()
	self.stats.UpdateConnect(connectEnabled)
	self.connectChanged(connectEnabled)
	self.windowStatusChanged(self.GetWindowStatus())

	if provideModeChanged {
		self.provideChanged(self.GetProvideEnabled())
	}
}

func (self *DeviceLocal) GetWindowStatus() *WindowStatus {
	var windowStatus *WindowStatus
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		switch v := self.remoteUserNatClient.(type) {
		case *connect.RemoteUserNatMultiClient:
			windowStatus = toWindowStatus(v.Monitor())
		default:
			windowStatus = &WindowStatus{}
		}
	}()
	return windowStatus
}

func toWindowStatus(monitor connect.MultiClientMonitor) *WindowStatus {
	windowExpandEvent, providerEvents := monitor.Events()
	windowStatus := &WindowStatus{
		TargetSize:   windowExpandEvent.TargetSize,
		MinSatisfied: windowExpandEvent.MinSatisfied,
	}
	for _, providerEvent := range providerEvents {
		switch providerEvent.State {
		case connect.ProviderStateInEvaluation:
			windowStatus.ProviderStateInEvaluation += 1
		case connect.ProviderStateEvaluationFailed:
			windowStatus.ProviderStateEvaluationFailed += 1
		case connect.ProviderStateNotAdded:
			windowStatus.ProviderStateNotAdded += 1
		case connect.ProviderStateAdded:
			windowStatus.ProviderStateAdded += 1
		case connect.ProviderStateRemoved:
			windowStatus.ProviderStateRemoved += 1
		}
	}
	return windowStatus
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

func (self *DeviceLocal) GetDefaultLocation() *ConnectLocation {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.defaultLocation
}

func (self *DeviceLocal) SetDefaultLocation(location *ConnectLocation) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.defaultLocation = location
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
	b := connect.MessagePoolCopy(packet[:n])
	success := self.sendPacket(b)
	if !success {
		MessagePoolReturn(b)
	}
	return success
}

func (self *DeviceLocal) SendPacketNoCopy(packet []byte, n int32) bool {
	return self.sendPacket(packet[:n])
}

func (self *DeviceLocal) sendPacket(packet []byte) bool {
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
		self.stats.UpdateRemoteSend(ByteCount(len(packet)))
		return remoteUserNatClient.SendPacket(
			source,
			protocol.ProvideMode_Network,
			packet,
			self.settings.SendTimeout,
		)
	} else if routeLocal {
		// route locally
		return localUserNat.SendPacket(
			source,
			protocol.ProvideMode_Network,
			packet,
			self.settings.SendTimeout,
		)
	} else {
		return false
	}
}

func (self *DeviceLocal) AddReceivePacket(receivePacket ReceivePacket) Sub {
	receive := func(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte) {
		var ipProtocol IpProtocol
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

	if self.contractStatusSub != nil {
		self.contractStatusSub()
		self.contractStatusSub = nil
	}
	if self.windowMonitorSub != nil {
		self.windowMonitorSub()
		self.windowMonitorSub = nil
	}
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

func (self *DeviceLocal) GetProviderEnabled() bool {
	// FIXME
	return true
}

func (self *DeviceLocal) SetProviderEnabled(providerEnabled bool) {
	// FIXME
}

func (self *DeviceLocal) AddProviderChangeListener(listener ProviderChangeListener) Sub {
	// FIXME
	return nil
}

func (self *DeviceLocal) GetBlockStats() *BlockStats {
	// FIXME
	return nil
}

func (self *DeviceLocal) GetBlockActions() *BlockActionWindow {
	// FIXME
	return nil
}

func (self *DeviceLocal) OverrideBlockAction(hostPattern string, block bool) {
	// FIXME
}

func (self *DeviceLocal) RemoveBlockActionOverride(hostPattern string) {
	// FIXME
}

func (self *DeviceLocal) SetBlockActionOverrideList(blockActionOverrides *BlockActionOverrideList) {
	// FIXME
}

func (self *DeviceLocal) GetBlockEnabled() bool {
	// FIXME
	return false
}

func (self *DeviceLocal) SetBlockEnabled(blockEnabled bool) {
	// FIXME
}

func (self *DeviceLocal) GetBlockWhileDisconnected() bool {
	// FIXME
	return false
}

func (self *DeviceLocal) SetBlockWhileDisconnected(blockWhileDisconnected bool) {
	// FIXME
}

func (self *DeviceLocal) AddBlockChangeListener(listener BlockChangeListener) Sub {
	// FIXME
	return nil
}

func (self *DeviceLocal) AddBlockActionWindowChangeListener(listener BlockActionWindowChangeListener) Sub {
	// FIXME
	return nil
}

func (self *DeviceLocal) AddBlockStatsChangeListener(listener BlockStatsChangeListener) Sub {
	// FIXME
	return nil
}

// contract stats

func (self *DeviceLocal) GetEgressContractStats() *ContractStats {
	// FIXME
	return nil
}

func (self *DeviceLocal) GetEgressContractDetails() *ContractDetailsList {
	// FIXME
	return nil
}

func (self *DeviceLocal) GetIngressContractStats() *ContractStats {
	// FIXME
	return nil
}

func (self *DeviceLocal) GetIngressContractDetails() *ContractDetailsList {
	// FIXME
	return nil
}

func (self *DeviceLocal) AddEgressContratStatsChangeListener(listener ContractStatsChangeListener) Sub {
	// FIXME
	return nil
}

func (self *DeviceLocal) AddEgressContractDetailsChangeListener(listener ContractDetailsChangeListener) Sub {
	// FIXME
	return nil
}

func (self *DeviceLocal) AddIngressContratStatsChangeListener(listener ContractStatsChangeListener) Sub {
	// FIXME
	return nil
}

func (self *DeviceLocal) AddIngressContractDetailsChangeListener(listener ContractDetailsChangeListener) Sub {
	// FIXME
	return nil
}

func (self *DeviceLocal) UploadLogs(feedbackId string, callback UploadLogsCallback) error {

	logDir := GetLogDir()

	files, err := os.ReadDir(logDir)
	if err != nil {
		glog.Errorf("Failed to read log directory %q: %v", logDir, err)
		return err
	}

	logPaths := []string{}
	for _, file := range files {
		name := file.Name()
		if !file.IsDir() &&
			(bytes.Contains([]byte(name), []byte(".log.INFO")) ||
				bytes.Contains([]byte(name), []byte(".log.WARNING")) ||
				bytes.Contains([]byte(name), []byte(".log.ERROR")) ||
				bytes.Contains([]byte(name), []byte(".log.FATAL"))) {
			fullPath := logDir + "/" + name
			logPaths = append(logPaths, fullPath)
		}
	}

	zipName := fmt.Sprintf("logs-%s.zip", time.Now().Format("20060102-150405"))
	zipPath := filepath.Join(logDir, zipName)

	if err := zipLogs(logPaths, zipPath); err != nil {
		return err
	}

	zipFile, err := os.Open(zipPath)
	if err != nil {
		return err
	}

	fileInfo, err := zipFile.Stat()
	if err != nil {
		zipFile.Close()
		return err
	}
	fileSize := fileInfo.Size()
	glog.Infof("Uploading log file %q (%d bytes)", zipPath, fileSize)

	self.GetApi().uploadLogs(feedbackId, zipFile, connect.NewApiCallback[*UploadLogsResult](func(res *UploadLogsResult, err error) {
		// Ensure resources are cleaned up after upload completes (success or error)
		zipFile.Close()
		os.Remove(zipPath)

		// Forward result to the original callback
		if callback != nil {
			callback.Result(res, err)
		}
	}))

	return nil
}

func zipLogs(
	logFiles []string,
	zipPath string,
) error {
	zipFile, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	defer zipFile.Close()

	zipWriter := zip.NewWriter(zipFile)
	defer zipWriter.Close()

	for _, path := range logFiles {
		f, err := os.Open(path)
		if err != nil {
			return err
		}

		fi, err := f.Stat()
		if err != nil {
			f.Close()
			return err
		}

		hdr, err := zip.FileInfoHeader(fi)
		if err != nil {
			f.Close()
			return err
		}
		hdr.Name = filepath.Base(path)
		hdr.Method = zip.Deflate

		w, err := zipWriter.CreateHeader(hdr)
		if err != nil {
			f.Close()
			return err
		}

		if _, err := io.Copy(w, f); err != nil {
			f.Close()
			return err
		}
		f.Close()
	}
	return nil
}
