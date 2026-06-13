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

type fixedWindowMonitor struct {
	clientIds []connect.Id
}

func newFixedWindowMonitor(clientIds []connect.Id) *fixedWindowMonitor {
	return &fixedWindowMonitor{
		clientIds: clientIds,
	}
}

func (self *fixedWindowMonitor) AddMonitorEventCallback(monitorEventCallback connect.MonitorEventFunction) func() {
	go connect.HandleError(func() {
		windowExpandEvent, providerEvents := self.Events()
		monitorEventCallback(windowExpandEvent, providerEvents, true)
	})
	return func() {}
}

func (self *fixedWindowMonitor) Events() (*connect.WindowExpandEvent, map[connect.Id]*connect.ProviderEvent) {
	windowExpandEvent := &connect.WindowExpandEvent{
		TargetSize:   len(self.clientIds),
		MinSatisfied: true,
	}
	providerEvents := map[connect.Id]*connect.ProviderEvent{}
	for _, clientId := range self.clientIds {
		providerEvents[clientId] = &connect.ProviderEvent{
			ClientId: clientId,
			State:    connect.ProviderStateAdded,
		}
	}
	return windowExpandEvent, providerEvents
}

func DefaultDeviceLocalSettings() *DeviceLocalSettings {
	bufferSize := 256
	return &DeviceLocalSettings{
		// this works with the `SequenceBufferSize` to control packet loss during back pressure
		SendTimeout:        5 * time.Second,
		SequenceBufferSize: bufferSize,
		// ClientDrainTimeout: 30 * time.Second,

		NetContractStatusDuration: 10 * time.Second,
		NetContractStatusCount:    10,

		DefaultRouteLocal:          true,
		DefaultCanShowRatingDialog: true,
		DefaultCanShowIntroFunnel:  true,

		DefaultProvideControlMode:       ProvideControlModeManual,
		DefaultProvideNetworkMode:       ProvideNetworkModeWiFi,
		DefaultCanRefer:                 false,
		DefaultAllowForeground:          false,
		DefaultOffline:                  true,
		DefaultVpnInterfaceWhileOffline: false,
		DefaultTunnelStarted:            false,

		AllowProvider: true,
		Verbose:       true,

		ClientSettings: *connect.DefaultClientSettingsWithBufferSize(bufferSize),
	}
}

// DeviceLocalSettings carries every device option, including what were
// previously constructor variant parameters. Construct with
// `DefaultDeviceLocalSettings` and override fields before passing to
// `NewDeviceLocal`.
//
// logger resolves the configured device logger
func (self *DeviceLocalSettings) logger() connect.Logger {
	if self.DisableLogging {
		return connect.NewNoopLogger()
	}
	if self.ClientSettings.Log != nil {
		return self.ClientSettings.Log
	}
	return connect.DefaultLogger()
}

//gomobile:noexport
type DeviceLocalSettings struct {
	// time to give up (drop) sending a packet to a destination
	SendTimeout time.Duration
	// ClientDrainTimeout time.Duration
	SequenceBufferSize int

	NetContractStatusDuration time.Duration
	NetContractStatusCount    int

	DefaultRouteLocal          bool
	DefaultCanShowRatingDialog bool
	DefaultCanShowIntroFunnel  bool

	DefaultProvideControlMode       ProvideControlMode
	DefaultProvideNetworkMode       ProvideNetworkMode
	DefaultCanRefer                 bool
	DefaultAllowForeground          bool
	DefaultOffline                  bool
	DefaultVpnInterfaceWhileOffline bool
	DefaultTunnelStarted            bool

	// options folded from the old constructor variants

	// AllowProvider creates the provider client and its local user nat.
	// The app constructors default this to true; the platform constructors
	// set false (the device is embedded inside the platform).
	AllowProvider bool
	// Verbose runs the security policy monitor when rpc is not enabled
	Verbose bool
	// GeneratorFunc, when set, builds the multi client generator instead of
	// the default api generator
	GeneratorFunc func(specs []*connect.ProviderSpec) connect.MultiClientGenerator
	// FIXME remove EnableRpc. Turn on RPC when RPC connections are set (receive net.Conn, send net.Conn)
	EnableRpc bool
	// KeyMaterial, when set, is applied to `ClientSettings` at construction
	KeyMaterial *DeviceLocalKeyMaterial
	// DisableLogging silences the device and all nested components and
	// clients, for hosts embedding many devices in one process.
	// It overrides `ClientSettings.Log`.
	DisableLogging bool

	connect.ClientSettings
}

// compile check that DeviceLocal conforms to Device, device, and ViewControllerManager
var _ Device = (*DeviceLocal)(nil)
var _ device = (*DeviceLocal)(nil)
var _ ViewControllerManager = (*DeviceLocal)(nil)

type DeviceLocal struct {
	networkSpace *NetworkSpace

	ctx    context.Context
	cancel context.CancelFunc

	byJwt        string
	tokenManager *deviceTokenManager
	// platformUrl string
	// apiUrl      string

	deviceDescription string
	deviceSpec        string
	appVersion        string

	settings *DeviceLocalSettings
	log      connect.Logger

	clientId   connect.Id
	instanceId connect.Id

	clientStrategy *connect.ClientStrategy

	generatorFunc func(specs []*connect.ProviderSpec) connect.MultiClientGenerator
	provider      *deviceLocalProvider

	stats *DeviceStats

	deviceLocalRpcManager *deviceLocalRpcManager
	// current listener config, so SetRpcServer is a no-op (no rebind that would
	// drop live connections) when the same server is re-applied
	rpcHostPort      string
	rpcServerPem     string
	rpcClientCertPem string

	stateLock sync.Mutex
	// stateLockGoid atomic.Int64

	connectLocation *ConnectLocation // reconnects when launched
	defaultLocation *ConnectLocation // persisting the location after the client has disconnected

	performanceProfile *PerformanceProfile

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

	canShowRatingDialogChangeListeners      *connect.CallbackList[CanShowRatingDialogChangeListener]
	canPromptIntroFunnelChangeListeners     *connect.CallbackList[CanPromptIntroFunnelChangeListener]
	allowForegroundChangeListeners          *connect.CallbackList[AllowForegroundChangeListener]
	canReferChangeListeners                 *connect.CallbackList[CanReferChangeListener]
	provideModeChangeListeners              *connect.CallbackList[ProvideModeChangeListener]
	provideChangeListeners                  *connect.CallbackList[ProvideChangeListener]
	provideControlModeChangeListeners       *connect.CallbackList[ProvideControlModeChangeListener]
	performanceProfileChangeListeners       *connect.CallbackList[PerformanceProfileChangeListener]
	providePausedChangeListeners            *connect.CallbackList[ProvidePausedChangeListener]
	provideNetworkModeChangeListeners       *connect.CallbackList[ProvideNetworkModeChangeListener]
	offlineChangeListeners                  *connect.CallbackList[OfflineChangeListener]
	vpnInterfaceWhileOfflineChangeListeners *connect.CallbackList[VpnInterfaceWhileOfflineChangeListener]
	connectChangeListeners                  *connect.CallbackList[ConnectChangeListener]
	routeLocalChangeListeners               *connect.CallbackList[RouteLocalChangeListener]
	connectLocationChangeListeners          *connect.CallbackList[ConnectLocationChangeListener]
	defaultLocationChangeListeners          *connect.CallbackList[DefaultLocationChangeListener]
	provideSecretKeysListeners              *connect.CallbackList[ProvideSecretKeysListener]
	tunnelChangeListeners                   *connect.CallbackList[TunnelChangeListener]
	contractStatusChangeListeners           *connect.CallbackList[ContractStatusChangeListener]
	windowStatusChangeListeners             *connect.CallbackList[WindowStatusChangeListener]
	jwtRefreshListeners                     *connect.CallbackList[JwtRefreshListener]

	localUserNatSub func()

	ingressSecurityPolicyGenerator func(*connect.SecurityPolicyStatsCollector) connect.SecurityPolicy
	egressSecurityPolicyGenerator  func(*connect.SecurityPolicyStatsCollector) connect.SecurityPolicy

	viewControllerManager
}

// FIXME remove enableRpc. Turn on RPC when RPC connections are set (receive net.Conn, send net.Conn)
func NewDeviceLocalWithDefaults(
	networkSpace *NetworkSpace,
	byJwt string,
	deviceDescription string,
	deviceSpec string,
	appVersion string,
	instanceId *Id,
	enableRpc bool,
) (*DeviceLocal, error) {
	settings := DefaultDeviceLocalSettings()
	settings.EnableRpc = enableRpc
	return NewDeviceLocal(
		networkSpace,
		byJwt,
		deviceDescription,
		deviceSpec,
		appVersion,
		instanceId,
		settings,
	)
}

func NewDeviceLocalWithKeyMaterial(
	networkSpace *NetworkSpace,
	byJwt string,
	deviceDescription string,
	deviceSpec string,
	appVersion string,
	instanceId *Id,
	enableRpc bool,
	keyMaterial *DeviceLocalKeyMaterial,
) (*DeviceLocal, error) {
	settings := DefaultDeviceLocalSettings()
	settings.EnableRpc = enableRpc
	settings.KeyMaterial = keyMaterial
	return NewDeviceLocal(
		networkSpace,
		byJwt,
		deviceDescription,
		deviceSpec,
		appVersion,
		instanceId,
		settings,
	)
}

// NewDeviceLocal creates a device with all options carried on `settings`
// (see `DeviceLocalSettings`).
//
//gomobile:noexport
func NewDeviceLocal(
	networkSpace *NetworkSpace,
	byJwt string,
	deviceDescription string,
	deviceSpec string,
	appVersion string,
	instanceId *Id,
	settings *DeviceLocalSettings,
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
				settings,
			)
		},
	)
}

// gomobile:ignore
func NewPlatformDeviceLocalWithDefaults(
	generatorFunc func(specs []*connect.ProviderSpec) connect.MultiClientGenerator,
	networkSpace *NetworkSpace,
	byJwt string,
	deviceDescription string,
	deviceSpec string,
	appVersion string,
	instanceId *Id,
) (*DeviceLocal, error) {
	return NewPlatformDeviceLocal(
		generatorFunc,
		networkSpace,
		byJwt,
		deviceDescription,
		deviceSpec,
		appVersion,
		instanceId,
		DefaultDeviceLocalSettings(),
	)
}

// gomobile:ignore
func NewPlatformDeviceLocalWithKeyMaterial(
	generatorFunc func(specs []*connect.ProviderSpec) connect.MultiClientGenerator,
	networkSpace *NetworkSpace,
	byJwt string,
	deviceDescription string,
	deviceSpec string,
	appVersion string,
	instanceId *Id,
	keyMaterial *DeviceLocalKeyMaterial,
) (*DeviceLocal, error) {
	settings := DefaultDeviceLocalSettings()
	settings.KeyMaterial = keyMaterial
	return NewPlatformDeviceLocal(
		generatorFunc,
		networkSpace,
		byJwt,
		deviceDescription,
		deviceSpec,
		appVersion,
		instanceId,
		settings,
	)
}

// a local device that does not use the default platform transport
// this device is typically embedded inside the platform
// gomobile:ignore
func NewPlatformDeviceLocal(
	generatorFunc func(specs []*connect.ProviderSpec) connect.MultiClientGenerator,
	networkSpace *NetworkSpace,
	byJwt string,
	deviceDescription string,
	deviceSpec string,
	appVersion string,
	instanceId *Id,
	settings *DeviceLocalSettings,
) (*DeviceLocal, error) {
	settings.AllowProvider = false
	settings.Verbose = false
	settings.GeneratorFunc = generatorFunc
	// FIXME change rpc to set connections. Embedded devices will set RPC connection when there is a control connection
	settings.EnableRpc = false
	return newDeviceLocal(
		networkSpace,
		byJwt,
		deviceDescription,
		deviceSpec,
		appVersion,
		instanceId,
		settings,
	)
}

func newDeviceLocal(
	networkSpace *NetworkSpace,
	byJwt string,
	deviceDescription string,
	deviceSpec string,
	appVersion string,
	instanceId *Id,
	settings *DeviceLocalSettings,
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
	settings *DeviceLocalSettings,
	clientId connect.Id,
) (*DeviceLocal, error) {
	if settings.KeyMaterial != nil {
		applyDeviceLocalKeyMaterial(&settings.ClientSettings, settings.KeyMaterial)
	}

	// resolve the device logger. all nested components and clients follow it.
	log := settings.logger()
	settings.ClientSettings.Log = log

	ctx, cancel := context.WithCancel(context.Background())
	// ctx, cancel := api.ctx, api.cancel
	// apiUrl := networkSpace.apiUrl
	clientStrategy := networkSpace.clientStrategy

	var provider *deviceLocalProvider
	if settings.AllowProvider {
		provider = newDeviceLocalProviderWithOverrides(
			ctx,
			networkSpace,
			byJwt,
			appVersion,
			instanceId.toConnectId(),
			&settings.ClientSettings,
			clientId,
		)
	}

	// api := newBringYourApiWithContext(cancelCtx, clientStrategy, apiUrl)
	api := networkSpace.GetApi()
	api.SetByJwt(byJwt)

	defaultRouteLocal := settings.DefaultRouteLocal
	defaultProvideControlMode := settings.DefaultProvideControlMode
	if provider == nil {
		defaultRouteLocal = false
		defaultProvideControlMode = ProvideControlModeNever
	}

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
		log:               log,
		clientId:          clientId,
		instanceId:        instanceId.toConnectId(),
		clientStrategy:    clientStrategy,
		generatorFunc:     settings.GeneratorFunc,
		provider:          provider,
		// contractManager: contractManager,
		// routeManager: routeManager,
		stats:                                   newDeviceStats(),
		connectLocation:                         nil,
		defaultLocation:                         nil,
		remoteUserNatClient:                     nil,
		remoteUserNatProviderLocalUserNat:       nil,
		remoteUserNatProvider:                   nil,
		routeLocal:                              defaultRouteLocal,
		canShowRatingDialog:                     settings.DefaultCanShowRatingDialog,
		canPromptIntroFunnel:                    settings.DefaultCanShowIntroFunnel,
		canRefer:                                settings.DefaultCanRefer,
		allowForeground:                         settings.DefaultAllowForeground,
		provideMode:                             ProvideModeNone,
		provideControlMode:                      defaultProvideControlMode,
		provideNetworkMode:                      settings.DefaultProvideNetworkMode,
		offline:                                 settings.DefaultOffline,
		vpnInterfaceWhileOffline:                settings.DefaultVpnInterfaceWhileOffline,
		tunnelStarted:                           settings.DefaultTunnelStarted,
		orderedContractStatusUpdates:            []*contractStatusUpdate{},
		netContractStatus:                       &ContractStatus{},
		receiveCallbacks:                        connect.NewCallbackList[connect.ReceivePacketFunction](),
		canShowRatingDialogChangeListeners:      connect.NewCallbackList[CanShowRatingDialogChangeListener](),
		canPromptIntroFunnelChangeListeners:     connect.NewCallbackList[CanPromptIntroFunnelChangeListener](),
		allowForegroundChangeListeners:          connect.NewCallbackList[AllowForegroundChangeListener](),
		canReferChangeListeners:                 connect.NewCallbackList[CanReferChangeListener](),
		provideModeChangeListeners:              connect.NewCallbackList[ProvideModeChangeListener](),
		provideChangeListeners:                  connect.NewCallbackList[ProvideChangeListener](),
		provideControlModeChangeListeners:       connect.NewCallbackList[ProvideControlModeChangeListener](),
		performanceProfileChangeListeners:       connect.NewCallbackList[PerformanceProfileChangeListener](),
		providePausedChangeListeners:            connect.NewCallbackList[ProvidePausedChangeListener](),
		provideNetworkModeChangeListeners:       connect.NewCallbackList[ProvideNetworkModeChangeListener](),
		offlineChangeListeners:                  connect.NewCallbackList[OfflineChangeListener](),
		vpnInterfaceWhileOfflineChangeListeners: connect.NewCallbackList[VpnInterfaceWhileOfflineChangeListener](),
		connectChangeListeners:                  connect.NewCallbackList[ConnectChangeListener](),
		routeLocalChangeListeners:               connect.NewCallbackList[RouteLocalChangeListener](),
		connectLocationChangeListeners:          connect.NewCallbackList[ConnectLocationChangeListener](),
		defaultLocationChangeListeners:          connect.NewCallbackList[DefaultLocationChangeListener](),
		provideSecretKeysListeners:              connect.NewCallbackList[ProvideSecretKeysListener](),
		contractStatusChangeListeners:           connect.NewCallbackList[ContractStatusChangeListener](),
		tunnelChangeListeners:                   connect.NewCallbackList[TunnelChangeListener](),
		windowStatusChangeListeners:             connect.NewCallbackList[WindowStatusChangeListener](),
		jwtRefreshListeners:                     connect.NewCallbackList[JwtRefreshListener](),
	}
	deviceLocal.viewControllerManager = *newViewControllerManager(ctx, deviceLocal)

	var logout func() error
	if networkSpace.asyncLocalState != nil {
		logout = networkSpace.asyncLocalState.localState.Logout
	} else {
		// do nothing
		logout = func() error {
			return nil
		}
	}

	deviceLocal.tokenManager = newDeviceTokenManager(
		ctx,
		log,
		api,
		deviceLocal.SetByJwt,
		// TODO the logout event should be propagated to the user
		logout,
	)

	// set up with nil destination
	if provider != nil {
		localUserNatSub := provider.LocalUserNat().AddReceivePacketCallback(deviceLocal.receive)
		deviceLocal.localUserNatSub = localUserNatSub
	}

	if settings.EnableRpc {
		deviceLocal.deviceLocalRpcManager = newDeviceLocalRpcManagerWithDefaults(ctx, deviceLocal)
	} else if settings.Verbose {
		newSecurityPolicyMonitor(ctx, deviceLocal)
	}

	return deviceLocal, nil
}

// gomobile:ignore
func (self *DeviceLocal) Ctx() context.Context {
	return self.ctx
}

// conforms to `device`
func (self *DeviceLocal) logger() connect.Logger {
	return self.log
}

// gomobile:ignore
func (self *DeviceLocal) SetIngressSecurityPolicyGenerator(g func(*connect.SecurityPolicyStatsCollector) connect.SecurityPolicy) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.ingressSecurityPolicyGenerator = g
}

// gomobile:ignore
func (self *DeviceLocal) SetEgressSecurityPolicyGenerator(g func(*connect.SecurityPolicyStatsCollector) connect.SecurityPolicy) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.egressSecurityPolicyGenerator = g
}

func (self *DeviceLocal) RefreshToken(attempt int) error {
	self.tokenManager.RefreshToken()
	return nil
}

func (self *DeviceLocal) SetPerformanceProfile(performanceProfile *PerformanceProfile) {
	var remoteUserNatClient connect.UserNatClient
	changed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		changed = !performanceProfilesEqual(self.performanceProfile, performanceProfile)
		self.performanceProfile = performanceProfile
		remoteUserNatClient = self.remoteUserNatClient
	}()
	if remoteUserNatClient != nil {
		switch v := remoteUserNatClient.(type) {
		case *connect.RemoteUserNatClient:
			if performanceProfile != nil {
				v.SetAllowDirect(performanceProfile.AllowDirect)
			} else {
				v.SetAllowDirect(false)
			}
		case *connect.RemoteUserNatMultiClient:
			v.SetPerformanceProfile(toConnectPerformanceProfile(performanceProfile))
		}
	}
	if changed {
		self.performanceProfileChanged(performanceProfile)
	}
}

func (self *DeviceLocal) GetPerformanceProfile() *PerformanceProfile {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return self.performanceProfile
}

func performanceProfilesEqual(a *PerformanceProfile, b *PerformanceProfile) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.WindowType != b.WindowType || a.AllowDirect != b.AllowDirect {
		return false
	}
	return windowSizeSettingsEqual(a.WindowSize, b.WindowSize)
}

func windowSizeSettingsEqual(a *WindowSizeSettings, b *WindowSizeSettings) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.WindowSizeMin == b.WindowSizeMin &&
		a.WindowSizeMinP2pOnly == b.WindowSizeMinP2pOnly &&
		a.WindowSizeMax == b.WindowSizeMax &&
		a.WindowSizeHardMax == b.WindowSizeHardMax &&
		a.WindowSizeReconnectScale == b.WindowSizeReconnectScale &&
		a.KeepHealthiestCount == b.KeepHealthiestCount &&
		a.Ulimit == b.Ulimit
}

func connectLocationsEqual(a *ConnectLocation, b *ConnectLocation) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equals(b)
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

func (self *DeviceLocal) providerClient() *connect.Client {
	if self.provider == nil {
		return nil
	}
	return self.provider.Client()
}

func (self *DeviceLocal) providerClientSnapshot() *connect.Client {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.providerClient()
}

func (self *DeviceLocal) SetByJwt(byJwt string) {
	self.GetApi().SetByJwt(byJwt)

	if self.networkSpace.asyncLocalState != nil {
		self.networkSpace.asyncLocalState.localState.SetByClientJwt(byJwt)
		self.networkSpace.asyncLocalState.localState.SetByJwt(byJwt)
	}

	if self.provider != nil {
		self.provider.SetByJwt(byJwt)
	}

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.byJwt = byJwt
	}()

	// fire listeners
	self.jwtRefreshed(byJwt)
}

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
					self.log.Infof("[contract]error insufficent balance\n")
				case protocol.ContractError_NoPermission:
					netContractStatus.NoPermission = true
					self.log.Infof("[contract]error no permission\n")
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
	changed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.canShowRatingDialog != canShowRatingDialog {
			self.canShowRatingDialog = canShowRatingDialog
			changed = true
		}
	}()
	if changed {
		self.canShowRatingDialogChanged(canShowRatingDialog)
	}
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
	changed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.canPromptIntroFunnel != canPrompt {
			self.canPromptIntroFunnel = canPrompt
			changed = true
		}
	}()
	if changed {
		self.canPromptIntroFunnelChanged(canPrompt)
	}
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
	provideChanged := false
	provideControlModeChanged := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.provideControlMode != provideControlMode {
			self.provideControlMode = provideControlMode
			provideControlModeChanged = true

			switch provideControlMode {
			case ProvideControlModeAuto:
				if self.remoteUserNatClient != nil {
					// if user is connected, start providing
					provideChanged = self.setProvideModeWithLock(ProvideModePublic)
				} else {
					// if user is not connected, stop providing
					provideChanged = self.setProvideModeWithLock(ProvideModeNone)
				}
			case ProvideControlModeAlways:
				provideChanged = self.setProvideModeWithLock(ProvideModePublic)
			default:
				provideChanged = self.setProvideModeWithLock(ProvideModeNone)
			}
		}
	}()

	if provideControlModeChanged {
		self.provideControlModeChanged(provideControlMode)
	}
	if provideChanged {
		self.provideModeChanged(self.GetProvideMode())
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
		self.log.Infof("Set provide network mode: %s", mode)
		self.provideNetworkModeChanged(mode)
	}
}

func (self *DeviceLocal) GetCanRefer() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.canRefer
}

func (self *DeviceLocal) SetCanRefer(canRefer bool) {
	changed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.canRefer != canRefer {
			self.canRefer = canRefer
			changed = true
		}
	}()
	if changed {
		self.canReferChanged(canRefer)
	}
}

func (self *DeviceLocal) GetAllowForeground() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.allowForeground
}

func (self *DeviceLocal) SetAllowForeground(allowForeground bool) {
	changed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.allowForeground != allowForeground {
			self.allowForeground = allowForeground
			changed = true
		}
	}()
	if changed {
		self.allowForegroundChanged(allowForeground)
	}
}

func (self *DeviceLocal) SetRouteLocal(routeLocal bool) {
	set := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if self.routeLocal != routeLocal {
			self.routeLocal = routeLocal
			set = true

			if self.remoteUserNatClient != nil {
				self.remoteUserNatClient.SetLocalSecurityBypass(routeLocal)
			}
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
	case *connect.RemoteUserNatClient:
		return newFixedWindowMonitor(v.DestinationIds())
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

func (self *DeviceLocal) AddCanShowRatingDialogChangeListener(listener CanShowRatingDialogChangeListener) Sub {
	callbackId := self.canShowRatingDialogChangeListeners.Add(listener)
	return newSub(func() {
		self.canShowRatingDialogChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) AddCanPromptIntroFunnelChangeListener(listener CanPromptIntroFunnelChangeListener) Sub {
	callbackId := self.canPromptIntroFunnelChangeListeners.Add(listener)
	return newSub(func() {
		self.canPromptIntroFunnelChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) AddAllowForegroundChangeListener(listener AllowForegroundChangeListener) Sub {
	callbackId := self.allowForegroundChangeListeners.Add(listener)
	return newSub(func() {
		self.allowForegroundChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) AddCanReferChangeListener(listener CanReferChangeListener) Sub {
	callbackId := self.canReferChangeListeners.Add(listener)
	return newSub(func() {
		self.canReferChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) AddProvideModeChangeListener(listener ProvideModeChangeListener) Sub {
	callbackId := self.provideModeChangeListeners.Add(listener)
	return newSub(func() {
		self.provideModeChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) AddProvideControlModeChangeListener(listener ProvideControlModeChangeListener) Sub {
	callbackId := self.provideControlModeChangeListeners.Add(listener)
	return newSub(func() {
		self.provideControlModeChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) AddPerformanceProfileChangeListener(listener PerformanceProfileChangeListener) Sub {
	callbackId := self.performanceProfileChangeListeners.Add(listener)
	return newSub(func() {
		self.performanceProfileChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) AddJwtRefreshListener(listener JwtRefreshListener) Sub {
	callbackId := self.jwtRefreshListeners.Add(listener)
	return newSub(func() {
		self.jwtRefreshListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) jwtRefreshed(jwt string) {
	for _, listener := range self.jwtRefreshListeners.Get() {
		connect.HandleError(func() {
			listener.JwtRefreshed(jwt)
		})
	}
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

func (self *DeviceLocal) AddVpnInterfaceWhileOfflineChangeListener(listener VpnInterfaceWhileOfflineChangeListener) Sub {
	callbackId := self.vpnInterfaceWhileOfflineChangeListeners.Add(listener)
	return newSub(func() {
		self.vpnInterfaceWhileOfflineChangeListeners.Remove(callbackId)
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

func (self *DeviceLocal) AddDefaultLocationChangeListener(listener DefaultLocationChangeListener) Sub {
	callbackId := self.defaultLocationChangeListeners.Add(listener)
	return newSub(func() {
		self.defaultLocationChangeListeners.Remove(callbackId)
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

func (self *DeviceLocal) canShowRatingDialogChanged(canShowRatingDialog bool) {
	for _, listener := range self.canShowRatingDialogChangeListeners.Get() {
		connect.HandleError(func() {
			listener.CanShowRatingDialogChanged(canShowRatingDialog)
		})
	}
}

func (self *DeviceLocal) canPromptIntroFunnelChanged(canPromptIntroFunnel bool) {
	for _, listener := range self.canPromptIntroFunnelChangeListeners.Get() {
		connect.HandleError(func() {
			listener.CanPromptIntroFunnelChanged(canPromptIntroFunnel)
		})
	}
}

func (self *DeviceLocal) allowForegroundChanged(allowForeground bool) {
	for _, listener := range self.allowForegroundChangeListeners.Get() {
		connect.HandleError(func() {
			listener.AllowForegroundChanged(allowForeground)
		})
	}
}

func (self *DeviceLocal) canReferChanged(canRefer bool) {
	for _, listener := range self.canReferChangeListeners.Get() {
		connect.HandleError(func() {
			listener.CanReferChanged(canRefer)
		})
	}
}

func (self *DeviceLocal) provideModeChanged(provideMode ProvideMode) {
	for _, listener := range self.provideModeChangeListeners.Get() {
		connect.HandleError(func() {
			listener.ProvideModeChanged(provideMode)
		})
	}
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

func (self *DeviceLocal) provideControlModeChanged(provideControlMode ProvideControlMode) {
	for _, listener := range self.provideControlModeChangeListeners.Get() {
		connect.HandleError(func() {
			listener.ProvideControlModeChanged(provideControlMode)
		})
	}
}

func (self *DeviceLocal) performanceProfileChanged(performanceProfile *PerformanceProfile) {
	for _, listener := range self.performanceProfileChangeListeners.Get() {
		connect.HandleError(func() {
			listener.PerformanceProfileChanged(performanceProfile)
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

func (self *DeviceLocal) vpnInterfaceWhileOfflineChanged(vpnInterfaceWhileOffline bool) {
	for _, listener := range self.vpnInterfaceWhileOfflineChangeListeners.Get() {
		connect.HandleError(func() {
			listener.VpnInterfaceWhileOfflineChanged(vpnInterfaceWhileOffline)
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

func (self *DeviceLocal) defaultLocationChanged(location *ConnectLocation) {
	for _, listener := range self.defaultLocationChangeListeners.Get() {
		connect.HandleError(func() {
			listener.DefaultLocationChanged(location)
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
	provideSecretKeyList := NewProvideSecretKeyList()
	if client := self.providerClient(); client != nil {
		provideSecretKeys := client.ContractManager().GetProvideSecretKeys()
		for provideMode, provideSecretKey := range provideSecretKeys {
			provideSecretKeyList.Add(&ProvideSecretKey{
				ProvideMode:      ProvideMode(provideMode),
				ProvideSecretKey: string(provideSecretKey),
			})
		}
	}
	return provideSecretKeyList
}

func (self *DeviceLocal) LoadProvideSecretKeys(provideSecretKeyList *ProvideSecretKeyList) {
	if client := self.providerClient(); client != nil {
		provideSecretKeys := map[protocol.ProvideMode][]byte{}
		for i := 0; i < provideSecretKeyList.Len(); i += 1 {
			provideSecretKey := provideSecretKeyList.Get(i)
			provideMode := protocol.ProvideMode(provideSecretKey.ProvideMode)
			provideSecretKeys[provideMode] = []byte(provideSecretKey.ProvideSecretKey)
		}
		client.ContractManager().LoadProvideSecretKeys(provideSecretKeys)

		self.provideSecretKeysChanged(self.GetProvideSecretKeys())
	}
}

func (self *DeviceLocal) InitProvideSecretKeys() {
	if client := self.providerClient(); client != nil {
		client.ContractManager().InitProvideSecretKeys()

		self.provideSecretKeysChanged(self.GetProvideSecretKeys())
	}
}

// GetClientKeySeed returns the 32-byte Ed25519 seed for the provider
// client's long-lived identity key. Persist it in caller-owned local
// storage and pass it back with NewDeviceLocalWithKeyMaterial on the next
// process start so the client's published ClientKey stays stable. Returns
// nil when no provider client exists or key initialization failed.
func (self *DeviceLocal) GetClientKeySeed() []byte {
	client := self.providerClientSnapshot()
	if client == nil {
		return nil
	}
	keyManager := client.ClientKeyManager()
	if keyManager == nil {
		return nil
	}
	return bytes.Clone(keyManager.Seed())
}

// GetProvideTlsCertificatePem returns the PEM-encoded TLS server
// certificate chain that the provider client publishes via
// `EncryptedKey`. Concatenated PEM blocks, leaf first. Pair with
// `GetProvideTlsPrivateKeyPem` and pass back with
// NewDeviceLocalWithKeyMaterial to keep the cert commitment stable across
// restarts. Returns nil when no provider client exists or encryption is
// disabled.
func (self *DeviceLocal) GetProvideTlsCertificatePem() []byte {
	client := self.providerClientSnapshot()
	if client == nil {
		return nil
	}
	manager := client.EncryptionSessionManager()
	if manager == nil {
		return nil
	}
	return bytes.Clone(manager.ProvideTlsCertificatePem())
}

// GetProvideTlsPrivateKeyPem returns the PEM-encoded PKCS#8 private
// key matching the leaf of `GetProvideTlsCertificatePem()`. Returns
// nil when no provider client exists, encryption is disabled, or
// the cert was supplied with no exposed private key.
func (self *DeviceLocal) GetProvideTlsPrivateKeyPem() []byte {
	client := self.providerClientSnapshot()
	if client == nil {
		return nil
	}
	manager := client.EncryptionSessionManager()
	if manager == nil {
		return nil
	}
	return bytes.Clone(manager.ProvideTlsPrivateKeyPem())
}

// GetKeyMaterial returns the provider client's persisted identity
// material. Persist it in caller-owned local storage and pass it back to
// NewDeviceLocalWithKeyMaterial on the next process start.
func (self *DeviceLocal) GetKeyMaterial() *DeviceLocalKeyMaterial {
	return NewDeviceLocalKeyMaterial(
		self.GetClientKeySeed(),
		self.GetProvideTlsCertificatePem(),
		self.GetProvideTlsPrivateKeyPem(),
	)
}

// SetKeyMaterial applies provider-client identity material to this device and
// emits ProvideSecretKeysChanged so callers can persist the resulting local
// state through the existing provide-secret-keys listener path.
func (self *DeviceLocal) SetKeyMaterial(keyMaterial *DeviceLocalKeyMaterial) {
	if keyMaterial == nil || keyMaterial.IsEmpty() {
		return
	}

	client := func() *connect.Client {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		applyDeviceLocalKeyMaterial(&self.settings.ClientSettings, keyMaterial)
		return self.providerClient()
	}()

	if client != nil {
		if seed := keyMaterial.GetClientKeySeed(); 0 < len(seed) {
			keyManager := client.ClientKeyManager()
			if keyManager != nil {
				if err := keyManager.SetSeed(seed); err != nil {
					self.log.Errorf("[device]failed to set client key seed: %s\n", err)
				}
			}
		}

		certPem := keyMaterial.GetProvideTlsCertificatePem()
		privateKeyPem := keyMaterial.GetProvideTlsPrivateKeyPem()
		if 0 < len(certPem) && 0 < len(privateKeyPem) {
			encryptionManager := client.EncryptionSessionManager()
			if encryptionManager != nil {
				if err := encryptionManager.SetProvideTlsKeyMaterial(certPem, privateKeyPem); err != nil {
					self.log.Errorf("[device]failed to set provide TLS key material: %s\n", err)
				}
			}
		}
	}

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
	self.log.Infof("[device]provide = %d\n", provideMode)

	changed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		changed = self.setProvideModeWithLock(provideMode)
	}()
	if changed {
		self.provideModeChanged(provideMode)
		self.provideChanged(self.GetProvideEnabled())
	}
}

func (self *DeviceLocal) setProvideModeWithLock(provideMode ProvideMode) (changed bool) {
	if client := self.providerClient(); client != nil {
		if self.provideMode != provideMode {
			self.provideMode = provideMode
			changed = true

			if provideMode != ProvideModeNone {
				// recreate the provider user nat only as needed
				// this avoid connection disruptions
				if self.remoteUserNatProviderLocalUserNat == nil {
					localUserNatSettings := connect.DefaultLocalUserNatSettings()
					localUserNatSettings.Log = self.log
					self.remoteUserNatProviderLocalUserNat = connect.NewLocalUserNat(client.Ctx(), self.clientId.String(), localUserNatSettings)
				}
				if self.remoteUserNatProvider == nil {
					self.remoteUserNatProvider = connect.NewRemoteUserNatProviderWithDefaults(client, self.remoteUserNatProviderLocalUserNat)
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

			client.ContractManager().SetProvideModesWithReturnTraffic(provideModes)
		}
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
	if client := self.providerClient(); client != nil {
		if client.ContractManager().SetProvidePaused(providePaused) {
			self.log.Infof("[device]provide paused = %t\n", providePaused)
			self.providePausedChanged(self.GetProvidePaused())
		}
	}
}

func (self *DeviceLocal) GetProvidePaused() (providePaused bool) {
	if client := self.providerClient(); client != nil {
		providePaused = client.ContractManager().IsProvidePaused()
	}
	return
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
		self.log.Infof("[device]offline = %t\n", offline)
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
		self.vpnInterfaceWhileOfflineChanged(vpnInterfaceWhileOffline)
		self.offlineChanged(self.GetOffline(), self.GetVpnInterfaceWhileOffline())
	}
}

func (self *DeviceLocal) GetVpnInterfaceWhileOffline() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.vpnInterfaceWhileOffline
}

func (self *DeviceLocal) RemoveDestination() {
	self.SetDestination(nil, nil)
}

func (self *DeviceLocal) SetDestination(location *ConnectLocation, specs *ProviderSpecList) {
	provideChanged := false
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

			// specClientIds := []connect.Id{}
			// for _, spec := range connectSpecs {
			// 	if spec.ClientId != nil {
			// 		specClientIds = append(specClientIds, *spec.ClientId)
			// 	}
			// }
			// fixedDestinationSize := len(specClientIds) == len(connectSpecs)

			remoteReceive := func(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte) {
				// self.log.Infof("[trace]receive packet\n")
				self.stats.UpdateRemoteReceive(ByteCount(len(packet)))
				self.receive(source, provideMode, ipPath, packet)
			}

			// if fixedDestinationSize && self.providerClient() == nil {
			// 	// a minimal efficient setup to send to fixed client id destinations
			// 	// the client id can be reused because there is no provider

			// 	// FIXME support custom security policies

			// 	apiUrl := self.networkSpace.apiUrl
			// 	clientStrategy := self.networkSpace.clientStrategy

			// 	clientOob := connect.NewApiOutOfBandControl(self.ctx, clientStrategy, self.byJwt, apiUrl)
			// 	client := connect.NewClient(
			// 		self.ctx,
			// 		self.clientId,
			// 		clientOob,
			// 		connect.DefaultClientSettings(),
			// 	)

			// 	auth := &connect.ClientAuth{
			// 		ByJwt:      self.byJwt,
			// 		InstanceId: self.instanceId,
			// 		AppVersion: self.appVersion,
			// 	}
			// 	platformTransport := connect.NewPlatformTransportWithDefaults(
			// 		client.Ctx(),
			// 		clientStrategy,
			// 		client.RouteManager(),
			// 		self.networkSpace.platformUrl,
			// 		auth,
			// 	)

			// 	var destinations []connect.MultiHopId
			// 	for _, clientId := range specClientIds {
			// 		destinations = append(destinations, connect.RequireMultiHopId(clientId))
			// 	}
			// 	nat := connect.NewRemoteUserNatClientWithClose(
			// 		client,
			// 		remoteReceive,
			// 		destinations,
			// 		protocol.ProvideMode_Public,
			// 		func() {
			// 			platformTransport.Close()
			// 			client.Close()
			// 		},
			// 	)
			// 	self.remoteUserNatClient = nat
			// } else {
			var generator connect.MultiClientGenerator
			if self.generatorFunc != nil {
				generator = self.generatorFunc(connectSpecs)
			} else {
				generator = connect.NewApiMultiClientGenerator(
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
					func() *connect.ClientSettings {
						clientSettings := newDeviceClientSettings(
							connect.DefaultClientSettingsWithBufferSize(self.settings.SequenceBufferSize),
							self.networkSpace.apiUrl,
							self.clientStrategy,
						)
						clientSettings.Log = self.log
						return clientSettings
					},
					connect.DefaultApiMultiClientGeneratorSettings(),
				)
			}
			settings := connect.DefaultMultiClientSettings()
			settings.Log = self.log
			settings.DefaultPerformanceProfile = toConnectPerformanceProfile(self.performanceProfile)
			if self.ingressSecurityPolicyGenerator != nil {
				settings.IngressSecurityPolicyGenerator = self.ingressSecurityPolicyGenerator
			}
			if self.egressSecurityPolicyGenerator != nil {
				settings.EgressSecurityPolicyGenerator = self.egressSecurityPolicyGenerator
			}
			multi := connect.NewRemoteUserNatMultiClient(
				self.ctx,
				generator,
				remoteReceive,
				protocol.ProvideMode_Public,
				settings,
			)
			self.contractStatusSub = multi.AddContractStatusCallback(self.updateContractStatus)
			self.remoteUserNatClient = multi
			monitor := multi.Monitor()
			windowMonitorEvent := func(windowExpandEvent *connect.WindowExpandEvent, providerEvents map[connect.Id]*connect.ProviderEvent, reset bool) {
				self.windowStatusChanged(toWindowStatus(monitor))
			}
			self.windowMonitorSub = monitor.AddMonitorEventCallback(windowMonitorEvent)
			// }

			self.remoteUserNatClient.SetLocalSecurityBypass(self.routeLocal)

			if self.provideControlMode == ProvideControlModeAuto {
				provideChanged = self.setProvideModeWithLock(ProvideModePublic)
			}
		} else {
			// else no specs, not an error
			if self.provideControlMode == ProvideControlModeAuto {
				provideChanged = self.setProvideModeWithLock(ProvideModeNone)
			}
		}
	}()

	self.connectLocationChanged(self.GetConnectLocation())
	connectEnabled := self.GetConnectEnabled()
	self.stats.UpdateConnect(connectEnabled)
	self.connectChanged(connectEnabled)
	self.windowStatusChanged(self.GetWindowStatus())

	if provideChanged {
		self.provideModeChanged(self.GetProvideMode())
		self.provideChanged(self.GetProvideEnabled())
	}
}

func (self *DeviceLocal) GetWindowStatus() *WindowStatus {
	var windowStatus *WindowStatus
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		switch v := self.remoteUserNatClient.(type) {
		case *connect.RemoteUserNatClient:
			n := len(v.DestinationIds())
			windowStatus = &WindowStatus{
				TargetSize:         n,
				ProviderStateAdded: n,
				MinSatisfied:       true,
			}
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
		self.SetDestination(location, specs)
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
	changed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !connectLocationsEqual(self.defaultLocation, location) {
			self.defaultLocation = location
			changed = true
		}
	}()
	if changed {
		self.defaultLocationChanged(location)
	}
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
	var routeLocal bool
	var provider *deviceLocalProvider
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		remoteUserNatClient = self.remoteUserNatClient
		routeLocal = self.routeLocal
		provider = self.provider
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
		var localUserNat *connect.LocalUserNat
		if provider != nil {
			localUserNat = provider.LocalUserNat()
		}
		if localUserNat != nil {
			// route locally
			return localUserNat.SendPacket(
				source,
				protocol.ProvideMode_Network,
				packet,
				0,
			)
		} else {
			return false
		}
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

// gomobile:ignore
func (self *DeviceLocal) AddReceivePacketCallback(callback func(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte)) func() {
	callbackId := self.receiveCallbacks.Add(callback)
	return func() {
		self.receiveCallbacks.Remove(callbackId)
	}
}

func (self *DeviceLocal) Cancel() {
	self.cancel()
}

func (self *DeviceLocal) Close() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.cancel()

	if self.provider != nil {
		self.provider.Close()
		self.provider = nil
	}

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
	if self.localUserNatSub != nil {
		self.localUserNatSub()
		self.localUserNatSub = nil
	}
	if self.remoteUserNatProviderLocalUserNat != nil {
		self.remoteUserNatProviderLocalUserNat.Close()
		self.remoteUserNatProviderLocalUserNat = nil
	}
	if self.remoteUserNatProvider != nil {
		self.remoteUserNatProvider.Close()
		self.remoteUserNatProvider = nil
	}

	// self.localUserNat.Close()

	if self.deviceLocalRpcManager != nil {
		self.deviceLocalRpcManager.Close()
	}

	if self.tokenManager != nil {
		self.tokenManager.Close()
		self.tokenManager = nil
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

// SetRpcServer starts (or restarts) the rpc server listening on hostPort
// (e.g. "127.0.0.1:12042"), presenting the certificate/key in serverPem and,
// when clientCertPem is non-empty, requiring and pinning that client
// certificate for mTLS. An empty serverPem listens unencrypted. Apps call this
// after constructing the device with the per-session server key material and
// client certificate received from the remote.
func (self *DeviceLocal) SetRpcServer(serverPem string, clientCertPem string, hostPort string) error {
	address, err := parseDeviceRemoteAddress(hostPort)
	if err != nil {
		return err
	}

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	// idempotent: if the listener config is unchanged, do not rebind (which would
	// drop live connections and force the remote to resync). re-applying the same
	// server must be a no-op.
	if self.deviceLocalRpcManager != nil &&
		self.rpcHostPort == hostPort &&
		self.rpcServerPem == serverPem &&
		self.rpcClientCertPem == clientCertPem {
		return nil
	}

	self.log.Infof("[dlrpc]set rpc server %s (tls=%t mtls=%t)", address.HostPort(), len(serverPem) != 0, len(clientCertPem) != 0)

	settings := defaultDeviceRpcSettings()
	settings.Address = address
	listener := NewWebsocketDeviceRpcListener(address, serverPem, clientCertPem, settings)

	// closing the old manager synchronously releases the previous listener's
	// port before the new listener binds (which may be the same port)
	if self.deviceLocalRpcManager != nil {
		self.deviceLocalRpcManager.Close()
	}
	self.deviceLocalRpcManager = newDeviceLocalRpcManager(self.ctx, self, settings, listener)
	self.rpcHostPort = hostPort
	self.rpcServerPem = serverPem
	self.rpcClientCertPem = clientCertPem
	return nil
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

/*
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
*/

func (self *DeviceLocal) UploadLogs(feedbackId string, callback UploadLogsCallback) error {

	logDir := GetLogDir()

	files, err := os.ReadDir(logDir)
	if err != nil {
		self.log.Errorf("Failed to read log directory %q: %v", logDir, err)
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
	self.log.Infof("Uploading log file %q (%d bytes)", zipPath, fileSize)

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

func toConnectPerformanceProfile(performanceProfile *PerformanceProfile) *connect.PerformanceProfile {
	if performanceProfile == nil {
		return nil
	}
	var connectWindowType connect.WindowType
	switch performanceProfile.WindowType {
	case WindowTypeQuality:
		connectWindowType = connect.WindowTypeQuality
	case WindowTypeSpeed:
		connectWindowType = connect.WindowTypeSpeed
	default:
		connectWindowType = connect.WindowTypeQuality
	}
	p := &connect.PerformanceProfile{
		WindowType:  connectWindowType,
		WindowSize:  toConnectWindowSize(performanceProfile.WindowSize),
		AllowDirect: performanceProfile.AllowDirect,
	}
	return p
}

func toConnectWindowSize(windowSize *WindowSizeSettings) connect.WindowSizeSettings {
	if windowSize == nil {
		return connect.DefaultWindowSizeSettings()
	}
	fixedWindowSize := 0
	if windowSize.WindowSizeMin == windowSize.WindowSizeMax {
		// fixed window size is a special mode that enforces a tigher window than just setting min=max
		// for simplicity, enable fixed window size in this case
		fixedWindowSize = windowSize.WindowSizeMin
	}
	return connect.WindowSizeSettings{
		WindowSizeMin:            windowSize.WindowSizeMin,
		WindowSizeMinP2pOnly:     windowSize.WindowSizeMinP2pOnly,
		WindowSizeMax:            windowSize.WindowSizeMax,
		WindowSizeHardMax:        windowSize.WindowSizeHardMax,
		FixedWindowSize:          fixedWindowSize,
		WindowSizeReconnectScale: windowSize.WindowSizeReconnectScale,
		KeepHealthiestCount:      windowSize.KeepHealthiestCount,
		Ulimit:                   windowSize.Ulimit,
	}
}
