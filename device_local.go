package sdk

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
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
	// scaled by the memory budget: deep sequence channels pin in-flight pool
	// buffers under sustained backpressure (see `SetMemoryLimit`)
	bufferSize := connect.MemoryScaledCount(256, 32)
	clientSettings := connect.DefaultClientSettingsWithBufferSize(bufferSize)
	// one transfer queue budget pair shared across all of the device's
	// clients (the provider client plus every window client): per-sequence
	// queues borrow above their floor from these pools, so the aggregate
	// queue memory stays flat as the window grows (scaled by the memory
	// budget). the provider client shares by settings copy; the window
	// client generator stamps the same pointers.
	clientSettings.SendBufferSettings.ResendQueueBudget =
		connect.NewTransferMemoryBudget(connect.MemoryScaledByteCount(6*1024*1024, 1024*1024))
	clientSettings.ReceiveBufferSettings.ReceiveQueueBudget =
		connect.NewTransferMemoryBudget(connect.MemoryScaledByteCount(8*1024*1024, 1536*1024))
	return &DeviceLocalSettings{
		// this works with the `SequenceBufferSize` to control packet loss during back pressure
		SendTimeout:        5 * time.Second,
		SequenceBufferSize: bufferSize,
		// ClientDrainTimeout: 30 * time.Second,

		NetContractStatusDuration: 10 * time.Second,
		NetContractStatusCount:    10,

		BlockActionWindowDuration: 300 * time.Second,
		BlockActionWindowMaxCount: 1024,

		ContractStatsEpoch: 1 * time.Second,

		NetworkPeersEpoch: 1 * time.Second,

		DefaultRouteLocal: true,
		// the ad/tracker blocker is opt-in; the apps expose the toggle
		DefaultBlockerEnabled:      false,
		DefaultCanShowRatingDialog: true,
		DefaultCanShowIntroFunnel:  true,

		DefaultProvideControlMode:       ProvideControlModeManual,
		DefaultProvideNetworkMode:       ProvideNetworkModeWiFi,
		DefaultCanRefer:                 false,
		DefaultAllowForeground:          false,
		DefaultOffline:                  true,
		DefaultVpnInterfaceWhileOffline: false,
		DefaultTunnelStarted:            false,

		// EXPERIMENT (temporary): default ON so the random 10.x tunnel address is
		// used without extra wiring. Set false to restore the 169.254/16 pool
		// allocator. See newDeviceLocalWithOverrides.
		UseExperimentalTunnelAddress: true,

		AllowProvider: true,
		Verbose:       true,

		ClientSettings: *clientSettings,
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

	// the time window and max count of retained block actions
	BlockActionWindowDuration time.Duration
	BlockActionWindowMaxCount int

	// the contract stats/details listeners emit at most once per epoch across
	// all window clients (a close event always emits)
	ContractStatsEpoch time.Duration

	// the network peers change listeners emit at most once per epoch
	NetworkPeersEpoch time.Duration

	DefaultRouteLocal          bool
	DefaultBlockerEnabled      bool
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

	// HostedIncompatible, when true, hard-guards the setters that must never
	// change on a hosted (platform-embedded) device: route local and the
	// provide settings, plus the identity/rpc setters that only make sense
	// for a locally-owned device. The guarded setters become no-ops; their
	// getters and change listeners keep working. This is defense in depth
	// alongside `DeviceLocalRpc.DisableHostedIncompatible`, which stops the
	// same operations at the rpc layer — either alone is sufficient, both
	// together mean nothing reachable can flip these on a hosted device.
	HostedIncompatible bool

	// UseExperimentalTunnelAddress, when set, assigns the TUN interface a random
	// 10.x.y.h (RFC1918, DHCP-shaped) address instead of drawing from connect's
	// 169.254/16 pool. 10.x is private, so the browser's mDNS obfuscation masks it
	// in WebRTC peer discovery, and randomizing avoids a fixed signature.
	// EXPERIMENT: defaults true for now (testing).
	UseExperimentalTunnelAddress bool

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

	// tunnelLocalAddress is the address the platform assigns to the TUN interface.
	// A random 10.x.y.h (RFC1918, DHCP-shaped) when settings.UseExperimentalTunnelAddress
	// is set (default on for now); otherwise reserved from connect's shared
	// local-address pool at construction (released in Close) so it never collides
	// with an IpMux-reserved address.
	tunnelLocalAddress netip.Addr

	// tunnelDnsSetting is the DNS config the platform applies to the TUN. Defaults
	// to plain DNS with no single-server override, so the platform applies the
	// default resolver list (9.9.9.9, 1.1.1.1) — plain (:53) is required for the
	// UpgradeMux to intercept, and no OS-level encrypted DNS is enabled.
	tunnelDnsSetting *TunnelDnsSetting

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

	// upgradeMux interposes on `remoteUserNatClient` (the exit/egress path) to
	// intercept and upgrade plaintext DNS (UDP/53) and HTTP (TCP/80). It is created and
	// torn down with `remoteUserNatClient`. When set, the send path runs through it (it
	// claims DNS/HTTP, else forwards to `remoteUserNatClient`) and the multi-client's
	// receive callback is the mux's `Receive`. nil => no interposition.
	upgradeMux         *connect.UpgradeMux
	upgradeMuxSettings *connect.UpgradeMuxSettings

	// sendRoute is an immutable snapshot of the routing fields read on the
	// per-packet send path (`remoteUserNatClient`, `routeLocal`, `provider`).
	// it is rebuilt under `stateLock` (via `updateSendRouteWithLock`) whenever
	// any of those change, and read lock-free by `sendPacket`, so the hot path
	// does not take `stateLock` once per packet just to read rarely-changing
	// configuration.
	sendRoute atomic.Pointer[deviceLocalSendRoute]

	remoteUserNatProviderLocalUserNat *connect.LocalUserNat
	remoteUserNatProvider             *connect.RemoteUserNatProvider

	// the ad/tracker blocker, shared by the upgrade mux (dns hostnames) and
	// the multi client (ips and reverse-index hostnames). a stable field:
	// the mux and multi client are torn down and rebuilt on every
	// destination change, and the blocker (with its enabled state) survives
	// the rebuilds and is re-wired into the fresh instances.
	blocker connect.Blocker

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
	// the last WindowStatus dispatched to listeners. the monitor fires an event
	// for transitions that do not change the derived status — notably a terminal
	// provider state for a client that was never added, whose delete is a no-op —
	// so the emit is gated on an actual change. an ungated emit re-sends an
	// identical snapshot, and on the remote device path each one also crosses the
	// rpc boundary
	lastWindowStatus *WindowStatus

	// insertion ordered, unique by override id
	blockActionOverrides []*BlockActionOverride
	// the recent routing decisions, newest last, gated by
	// `BlockActionWindowDuration`/`BlockActionWindowMaxCount`
	blockActions []*BlockAction
	// packet stats accumulated from closed clients. the live client's
	// stats are added on top
	packetStatsBase connect.PacketStats
	netBlockStats   BlockStats
	// contracts of the current client. the contracts die with the client
	contracts *deviceContractTracker

	// provider packet stats accumulated from closed provider user nats
	// (provide disabled). the live provider user nat's stats are added on top
	providerPacketStatsBase connect.PacketStats
	// contracts of the provider client, which lives as long as the device
	providerContracts *deviceContractTracker

	// packet counts on the fallback local route (no remote client)
	localFallbackEgressPacketCount  atomic.Int64
	localFallbackEgressByteCount    atomic.Int64
	localFallbackIngressPacketCount atomic.Int64
	localFallbackIngressByteCount   atomic.Int64

	blockActionSub        func()
	packetStatsSub        func()
	contractStatsEventSub func()

	providerPacketStatsSub        func()
	providerContractStatsEventSub func()

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
	blockerEnabledChangeListeners           *connect.CallbackList[BlockerEnabledChangeListener]
	connectLocationChangeListeners          *connect.CallbackList[ConnectLocationChangeListener]
	defaultLocationChangeListeners          *connect.CallbackList[DefaultLocationChangeListener]
	provideSecretKeysListeners              *connect.CallbackList[ProvideSecretKeysListener]
	tunnelChangeListeners                   *connect.CallbackList[TunnelChangeListener]
	contractStatusChangeListeners           *connect.CallbackList[ContractStatusChangeListener]
	windowStatusChangeListeners             *connect.CallbackList[WindowStatusChangeListener]
	jwtRefreshListeners                     *connect.CallbackList[JwtRefreshListener]
	authLogoutListeners                     *connect.CallbackList[AuthLogoutListener]

	blockActionWindowChangeListeners      *connect.CallbackList[BlockActionWindowChangeListener]
	blockStatsChangeListeners             *connect.CallbackList[BlockStatsChangeListener]
	blockActionOverridesChangeListeners   *connect.CallbackList[BlockActionOverridesChangeListener]
	packetStatsChangeListeners            *connect.CallbackList[PacketStatsChangeListener]
	egressContractStatsChangeListeners    *connect.CallbackList[ContractStatsChangeListener]
	egressContractDetailsChangeListeners  *connect.CallbackList[ContractDetailsChangeListener]
	ingressContractStatsChangeListeners   *connect.CallbackList[ContractStatsChangeListener]
	ingressContractDetailsChangeListeners *connect.CallbackList[ContractDetailsChangeListener]
	dnsResolverSettingsChangeListeners    *connect.CallbackList[DnsResolverSettingsChangeListener]
	networkPeersChangeListeners           *connect.CallbackList[NetworkPeersChangeListener]

	providerPacketStatsChangeListeners            *connect.CallbackList[PacketStatsChangeListener]
	providerEgressContractStatsChangeListeners    *connect.CallbackList[ContractStatsChangeListener]
	providerEgressContractDetailsChangeListeners  *connect.CallbackList[ContractDetailsChangeListener]
	providerIngressContractStatsChangeListeners   *connect.CallbackList[ContractStatsChangeListener]
	providerIngressContractDetailsChangeListeners *connect.CallbackList[ContractDetailsChangeListener]

	localUserNatSub func()

	clientSecurityPolicyGenerator   func(context.Context, *connect.SecurityPolicyStatsCollector) connect.SecurityPolicy
	providerSecurityPolicyGenerator func(context.Context, *connect.SecurityPolicyStatsCollector) connect.SecurityPolicy

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

	// the blocker outlives the mux/multi client rebuilds; seed the initial
	// enabled state from the settings. the persisted toggle is restored below,
	// and the device persists on set (see SetBlockerEnabled)
	blocker := connect.NewBlockerWithDefaults()
	blocker.SetEnabled(settings.DefaultBlockerEnabled)

	// EXPERIMENT: when UseExperimentalTunnelAddress is set (default on for now),
	// assign the TUN interface a 10.x.y.h address (minimum free /24, host
	// randomized in 2..254 like a DHCP lease) instead of reserving from connect's
	// 169.254/16 pool. 10.0.0.0/8 is RFC1918, which libwebrtc classifies as
	// private, so the browser's mDNS obfuscation rewrites the host candidate to
	// <hash>.local and the tunnel address does not leak in WebRTC peer discovery.
	// The randomized host avoids a fixed fingerprint; the /24 avoids a real local
	// subnet. Not from the pool -> not returned in Close.
	var tunnelLocalAddress netip.Addr
	if settings.UseExperimentalTunnelAddress {
		tunnelLocalAddress = connect.RandomLocalIpv4(connect.LocalIpv4Networks())
	} else {
		var ok bool
		tunnelLocalAddress, ok = connect.TakeLocalIpv4Address()
		if !ok {
			cancel()
			return nil, fmt.Errorf("no local tunnel address available")
		}
	}

	deviceLocal := &DeviceLocal{
		networkSpace: networkSpace,
		ctx:          ctx,
		cancel:       cancel,
		byJwt:        byJwt,
		// apiUrl:            apiUrl,
		deviceDescription:  deviceDescription,
		deviceSpec:         deviceSpec,
		appVersion:         appVersion,
		settings:           settings,
		log:                log,
		clientId:           clientId,
		instanceId:         instanceId.toConnectId(),
		tunnelLocalAddress: tunnelLocalAddress,
		tunnelDnsSetting:   DefaultTunnelDnsSetting(),
		clientStrategy:     clientStrategy,
		generatorFunc:      settings.GeneratorFunc,
		provider:           provider,
		// contractManager: contractManager,
		// routeManager: routeManager,
		stats:                                   newDeviceStats(),
		connectLocation:                         nil,
		defaultLocation:                         nil,
		remoteUserNatClient:                     nil,
		upgradeMux:                              nil,
		upgradeMuxSettings:                      connect.DefaultUpgradeMuxSettings(),
		remoteUserNatProviderLocalUserNat:       nil,
		remoteUserNatProvider:                   nil,
		blocker:                                 blocker,
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
		contracts:                               newDeviceContractTracker(),
		providerContracts:                       newDeviceContractTracker(),
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
		blockerEnabledChangeListeners:           connect.NewCallbackList[BlockerEnabledChangeListener](),
		connectLocationChangeListeners:          connect.NewCallbackList[ConnectLocationChangeListener](),
		defaultLocationChangeListeners:          connect.NewCallbackList[DefaultLocationChangeListener](),
		provideSecretKeysListeners:              connect.NewCallbackList[ProvideSecretKeysListener](),
		contractStatusChangeListeners:           connect.NewCallbackList[ContractStatusChangeListener](),
		tunnelChangeListeners:                   connect.NewCallbackList[TunnelChangeListener](),
		windowStatusChangeListeners:             connect.NewCallbackList[WindowStatusChangeListener](),
		jwtRefreshListeners:                     connect.NewCallbackList[JwtRefreshListener](),
		authLogoutListeners:                     connect.NewCallbackList[AuthLogoutListener](),
		blockActionWindowChangeListeners:        connect.NewCallbackList[BlockActionWindowChangeListener](),
		blockStatsChangeListeners:               connect.NewCallbackList[BlockStatsChangeListener](),
		blockActionOverridesChangeListeners:     connect.NewCallbackList[BlockActionOverridesChangeListener](),
		packetStatsChangeListeners:              connect.NewCallbackList[PacketStatsChangeListener](),
		egressContractStatsChangeListeners:      connect.NewCallbackList[ContractStatsChangeListener](),
		egressContractDetailsChangeListeners:    connect.NewCallbackList[ContractDetailsChangeListener](),
		ingressContractStatsChangeListeners:     connect.NewCallbackList[ContractStatsChangeListener](),
		ingressContractDetailsChangeListeners:   connect.NewCallbackList[ContractDetailsChangeListener](),
		dnsResolverSettingsChangeListeners:      connect.NewCallbackList[DnsResolverSettingsChangeListener](),
		networkPeersChangeListeners:             connect.NewCallbackList[NetworkPeersChangeListener](),

		providerPacketStatsChangeListeners:            connect.NewCallbackList[PacketStatsChangeListener](),
		providerEgressContractStatsChangeListeners:    connect.NewCallbackList[ContractStatsChangeListener](),
		providerEgressContractDetailsChangeListeners:  connect.NewCallbackList[ContractDetailsChangeListener](),
		providerIngressContractStatsChangeListeners:   connect.NewCallbackList[ContractStatsChangeListener](),
		providerIngressContractDetailsChangeListeners: connect.NewCallbackList[ContractDetailsChangeListener](),
	}
	// restore the persisted block action overrides and dns resolver settings
	if asyncLocalState := networkSpace.GetAsyncLocalState(); asyncLocalState != nil {
		localState := asyncLocalState.GetLocalState()
		if overrides := localState.GetBlockActionOverrides(); overrides != nil {
			deviceLocal.blockActionOverrides = overrides.getAll()
		}
		if dnsResolverSettings := localState.GetDnsResolverSettings(); dnsResolverSettings != nil {
			if upgradeMuxSettings := upgradeMuxSettingsWithDnsResolverSettings(deviceLocal.upgradeMuxSettings, dnsResolverSettings); upgradeMuxSettings != nil {
				deviceLocal.upgradeMuxSettings = upgradeMuxSettings
			}
		}
		// the blocker toggle persists on set (see SetBlockerEnabled); unset
		// reads false, matching the opt-in default
		blocker.SetEnabled(localState.GetBlockerEnabled())
	}

	// publish the initial send-route snapshot so `sendPacket` always has a
	// non-nil snapshot to read
	deviceLocal.updateSendRouteWithLock()
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
		// clear the local auth state, then propagate the logout to the app
		// (`AddAuthLogoutListener`) so the ui can return to the login flow
		func() error {
			err := logout()
			deviceLocal.authLogout()
			return err
		},
	)

	// set up with nil destination
	if provider != nil {
		localUserNatSub := provider.LocalUserNat().AddReceivePacketCallback(deviceLocal.localFallbackReceive)
		deviceLocal.localUserNatSub = localUserNatSub
		// the provider client lives as long as the device, so its contract
		// stats subscription does too
		deviceLocal.providerContractStatsEventSub = provider.Client().ContractManager().AddContractStatsCallback(deviceLocal.updateProviderContractStatsEvents)
		// the network peers are tracked by the provider client. Grab the
		// monitor channel synchronously here (before any peer update can be
		// delivered) so watchNetworkPeers never misses the first change.
		networkPeersNotify := provider.Client().PeerManager().PeersMonitor().NotifyChannel()
		go connect.HandleError(func() {
			deviceLocal.watchNetworkPeers(networkPeersNotify)
		})
	}

	// the trailing edge of the contract stats epoch gate: carries out the last
	// batch of a transfer, which lands inside the gate and would otherwise never
	// be emitted, and decays the bit rate of idle contracts
	go connect.HandleError(deviceLocal.runContractStatsFlush)

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

// TunnelLocalAddress returns the IPv4 address the platform assigns to the TUN
// interface. When settings.UseExperimentalTunnelAddress is set (default on for
// now) this is a random 10.x.y.h (RFC1918, DHCP-shaped) so it is private and the
// browser's mDNS obfuscation masks it in WebRTC peer discovery; otherwise it is
// drawn from connect's shared 169.254/16 allocator (no IpMux collision).
func (self *DeviceLocal) TunnelLocalAddress() string {
	return self.tunnelLocalAddress.String()
}

// TunnelDnsSetting returns the DNS configuration the platform should apply to the
// TUN (defaults to plain DNS, resolver list 9.9.9.9, 1.1.1.1). Plain DNS is
// required for the UpgradeMux to intercept and upgrade :53 traffic.
func (self *DeviceLocal) TunnelDnsSetting() *TunnelDnsSetting {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.tunnelDnsSetting
}

// SetTunnelDnsSetting overrides the platform DNS configuration. Each use case sets
// its own (the apps use the default plain-DNS resolver list; server/proxy may
// differ). A non-empty Server narrows the tunnel to that single resolver.
func (self *DeviceLocal) SetTunnelDnsSetting(setting *TunnelDnsSetting) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.tunnelDnsSetting = setting
}

// TunnelDnsAddressesIpv4 returns the plain-DNS IPv4 server IPs the platform should
// apply to the TUN interface (Android `addDnsServer`), sourced from the device at
// tunnel-build time like TunnelLocalAddress: the dns resolver settings' unencrypted
// local servers when set, otherwise the default tunnel dns setting (the default
// plain-DNS resolvers 9.9.9.9, 1.1.1.1). Plain :53 keeps the UpgradeMux able to
// intercept and upgrade queries.
func (self *DeviceLocal) TunnelDnsAddressesIpv4() *StringList {
	return self.tunnelDnsAddressList(false)
}

// TunnelDnsAddressesIpv6 is TunnelDnsAddressesIpv4 for IPv6. There is no default
// IPv6 tunnel dns, so this is empty unless the dns resolver settings set
// unencrypted local IPv6 servers.
func (self *DeviceLocal) TunnelDnsAddressesIpv6() *StringList {
	return self.tunnelDnsAddressList(true)
}

func (self *DeviceLocal) tunnelDnsAddressList(ipv6 bool) *StringList {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	var resolver *connect.DnsResolverSettings
	if self.upgradeMuxSettings != nil && self.upgradeMuxSettings.Dns != nil {
		resolver = self.upgradeMuxSettings.Dns.Resolver
	}
	addresses := NewStringList()
	addresses.addAll(tunnelDnsAddresses(resolver, self.tunnelDnsSetting, ipv6)...)
	return addresses
}

// tunnelDnsAddresses derives the plain-dns tunnel resolver ips of one address
// family: the resolver settings' unencrypted local dns servers when enabled and
// non-empty, otherwise the tunnel dns setting's single-server override when set,
// otherwise the default plain-dns resolver list (defaultTunnelDnsServersIpv4).
// entries that do not parse as an ip are dropped, so the platform never applies a
// bad address
func tunnelDnsAddresses(resolver *connect.DnsResolverSettings, tunnelDnsSetting *TunnelDnsSetting, ipv6 bool) []string {
	family := func(servers []string) []string {
		out := []string{}
		for _, server := range servers {
			server = strings.TrimSpace(server)
			// `Is4() != ipv6` keeps v4 when !ipv6 and v6 when ipv6
			if addr, err := netip.ParseAddr(server); err == nil && addr.Unmap().Is4() != ipv6 {
				out = append(out, server)
			}
		}
		return out
	}
	if resolver != nil && resolver.EnableLocalDns {
		// family-classify the union so a misfiled entry still lands correctly
		servers := family(append(append([]string{}, resolver.LocalDnsIpv4...), resolver.LocalDnsIpv6...))
		if 0 < len(servers) {
			return servers
		}
	}
	if tunnelDnsSetting != nil {
		// an explicit single-server override wins; otherwise (the default, empty
		// Server) apply the default plain-dns resolver list. plain :53 keeps the
		// UpgradeMux able to intercept and upgrade, and the default leads with a
		// resolver the OS does not auto-upgrade to encrypted DNS (see
		// defaultTunnelDnsServersIpv4)
		if server := strings.TrimSpace(tunnelDnsSetting.Server); server != "" {
			return family([]string{server})
		}
		return family(defaultTunnelDnsServers(ipv6))
	}
	return []string{}
}

// SetUpgradeMuxSettings sets how the interposed mux resolves DNS and upgrades HTTP.
// It takes effect when the remote client is next (re)created. nil disables the mux
// (direct pass-through to the exit). gomobile:ignore until a platform-friendly
// settings surface lands with the per-use-case defaults.
//
// gomobile:ignore
func (self *DeviceLocal) SetUpgradeMuxSettings(settings *connect.UpgradeMuxSettings) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.upgradeMuxSettings = settings
	// apply to the live mux immediately when non-nil (rebuilds its DohCache); nil takes
	// effect on the next client recreation, which then creates no mux
	if self.upgradeMux != nil && settings != nil {
		self.upgradeMux.SetSettings(settings)
	}
}

// SetClientSecurityPolicyGenerator sets the multi-client (the device's own traffic) security policy.
// gomobile:ignore
func (self *DeviceLocal) SetClientSecurityPolicyGenerator(g func(context.Context, *connect.SecurityPolicyStatsCollector) connect.SecurityPolicy) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.clientSecurityPolicyGenerator = g
}

// SetProviderSecurityPolicyGenerator sets the provider (egressing remote clients' traffic) security
// policy. Defaults to the reversed client policy (connect.DefaultProviderSecurityPolicyWithStats).
// gomobile:ignore
func (self *DeviceLocal) SetProviderSecurityPolicyGenerator(g func(context.Context, *connect.SecurityPolicyStatsCollector) connect.SecurityPolicy) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.providerSecurityPolicyGenerator = g
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
		// ORDER MATTERS. LocalState.SetByJwt clears the client jwt and the instance
		// id whenever the value changes -- which is ALWAYS true on a refresh -- so
		// calling SetByClientJwt first meant SetByJwt immediately wiped it, leaving
		// .by_client_jwt empty and the user logged out on the next cold launch.
		// Write the network jwt first, then the client jwt.
		self.networkSpace.asyncLocalState.localState.SetByJwt(byJwt)
		self.networkSpace.asyncLocalState.localState.SetByClientJwt(byJwt)
	}

	// snapshot self.provider under stateLock, synchronizing with Close()'s
	// self.provider = nil write, and store byJwt in the same critical section.
	// provider.SetByJwt runs on the snapshot outside the lock (it only sets the
	// platform transport auth, which has its own locking).
	var provider *deviceLocalProvider
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		provider = self.provider
		self.byJwt = byJwt
	}()

	if provider != nil {
		provider.SetByJwt(byJwt)
	}

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

		// track last n status updates and use all updates newer than M seconds.
		// i walks up to the first update to KEEP: start past the count overflow
		// (leaving room for the one appended below), then skip anything that
		// aged out of the duration window. keep [i:] — slicing [:i] instead
		// would retain exactly the updates that were meant to expire, which
		// latches a stale Premium/error state forever and grows the slice
		// without bound
		now := time.Now()
		windowStartTime := now.Add(-self.settings.NetContractStatusDuration)
		i := max(
			0,
			len(self.orderedContractStatusUpdates)-(self.settings.NetContractStatusCount-1),
		)
		for i < len(self.orderedContractStatusUpdates) && self.orderedContractStatusUpdates[i].updateTime.Before(windowStartTime) {
			i += 1
		}
		self.orderedContractStatusUpdates = self.orderedContractStatusUpdates[i:]
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
	if self.hostedIncompatibleGuarded("SetTunnelStarted") {
		return
	}
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
	if self.hostedIncompatibleGuarded("SetProvideControlMode") {
		return
	}
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
	if self.hostedIncompatibleGuarded("SetProvideNetworkMode") {
		return
	}
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

// hostedIncompatibleGuarded reports whether a setter that must not run on a
// hosted device should be skipped. It logs the skip for visibility.
func (self *DeviceLocal) hostedIncompatibleGuarded(name string) bool {
	if self.settings.HostedIncompatible {
		self.log.Infof("[device]hosted incompatible: %s ignored\n", name)
		return true
	}
	return false
}

func (self *DeviceLocal) SetRouteLocal(routeLocal bool) {
	if self.hostedIncompatibleGuarded("SetRouteLocal") {
		return
	}
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
			self.updateSendRouteWithLock()
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

func (self *DeviceLocal) SetBlockerEnabled(blockerEnabled bool) {
	set := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if self.blocker.Enabled() != blockerEnabled {
			// the blocker is shared with the live mux and multi client, so
			// this takes effect immediately, and it survives their rebuilds
			self.blocker.SetEnabled(blockerEnabled)
			set = true
		}
	}()
	if set {
		self.persistBlockerEnabled(blockerEnabled)
		self.blockerEnabledChanged(blockerEnabled)
	}
}

// persists the blocker toggle to local state, asynchronously.
// restored at device creation (see the constructor restore block)
func (self *DeviceLocal) persistBlockerEnabled(blockerEnabled bool) {
	if asyncLocalState := self.networkSpace.GetAsyncLocalState(); asyncLocalState != nil {
		asyncLocalState.serialAsync(func() error {
			return asyncLocalState.GetLocalState().SetBlockerEnabled(blockerEnabled)
		})
	}
}

func (self *DeviceLocal) GetBlockerEnabled() bool {
	return self.blocker.Enabled()
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

func (self *DeviceLocal) AddAuthLogoutListener(listener AuthLogoutListener) Sub {
	callbackId := self.authLogoutListeners.Add(listener)
	return newSub(func() {
		self.authLogoutListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) authLogout() {
	for _, listener := range self.authLogoutListeners.Get() {
		connect.HandleError(func() {
			listener.AuthLogout()
		})
	}
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

func (self *DeviceLocal) AddBlockerEnabledChangeListener(listener BlockerEnabledChangeListener) Sub {
	callbackId := self.blockerEnabledChangeListeners.Add(listener)
	return newSub(func() {
		self.blockerEnabledChangeListeners.Remove(callbackId)
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

func (self *DeviceLocal) blockerEnabledChanged(blockerEnabled bool) {
	// self.assertNotLockOwner()
	for _, listener := range self.blockerEnabledChangeListeners.Get() {
		connect.HandleError(func() {
			listener.BlockerEnabledChanged(blockerEnabled)
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

// return traffic on the fallback local route (no remote client)
func (self *DeviceLocal) localFallbackReceive(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte) {
	self.localFallbackIngressPacketCount.Add(1)
	self.localFallbackIngressByteCount.Add(int64(len(packet)))
	self.receive(source, provideMode, ipPath, packet)
}

func (self *DeviceLocal) GetProvideSecretKeys() *ProvideSecretKeyList {
	provideSecretKeyList := NewProvideSecretKeyList()
	// snapshot reads self.provider under stateLock, synchronizing with Close()'s
	// write; the unlocked providerClient() would race a concurrent teardown
	if client := self.providerClientSnapshot(); client != nil {
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
	if client := self.providerClientSnapshot(); client != nil {
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
	if client := self.providerClientSnapshot(); client != nil {
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
	if self.hostedIncompatibleGuarded("SetKeyMaterial") {
		return
	}
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
	if self.hostedIncompatibleGuarded("SetProvideMode") {
		return
	}
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
					// the provider egresses remote clients' traffic and runs its own security policy:
					// the connect default is the reversed client policy
					// (DefaultProviderSecurityPolicyWithStats), or an explicitly set provider policy
					providerSettings := connect.DefaultRemoteUserNatProviderSettings()
					if self.providerSecurityPolicyGenerator != nil {
						providerSettings.SecurityPolicyGenerator = self.providerSecurityPolicyGenerator
					}
					self.remoteUserNatProvider = connect.NewRemoteUserNatProvider(client, self.remoteUserNatProviderLocalUserNat, providerSettings)
					self.providerPacketStatsSub = self.remoteUserNatProvider.AddPacketStatsCallback(self.updateProviderPacketStats)
				}
			} else {
				// close
				if self.remoteUserNatProviderLocalUserNat != nil {
					self.remoteUserNatProviderLocalUserNat.Close()
					self.remoteUserNatProviderLocalUserNat = nil
				}
				if self.providerPacketStatsSub != nil {
					self.providerPacketStatsSub()
					self.providerPacketStatsSub = nil
				}
				if self.remoteUserNatProvider != nil {
					// fold the final packet counters into the device accumulator
					addConnectPacketStats(&self.providerPacketStatsBase, self.remoteUserNatProvider.PacketStats())
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
	if self.hostedIncompatibleGuarded("SetProvidePaused") {
		return
	}
	if client := self.providerClientSnapshot(); client != nil {
		if client.ContractManager().SetProvidePaused(providePaused) {
			self.log.Infof("[device]provide paused = %t\n", providePaused)
			self.providePausedChanged(self.GetProvidePaused())
		}
	}
}

func (self *DeviceLocal) GetProvidePaused() (providePaused bool) {
	if client := self.providerClientSnapshot(); client != nil {
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
	if self.hostedIncompatibleGuarded("SetVpnInterfaceWhileOffline") {
		return
	}
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
		if self.upgradeMux != nil {
			self.upgradeMux.Close()
			self.upgradeMux = nil
		}
		self.closeRemoteUserNatClientWithLock()

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
						// share the device budgets so every window client's
						// queues draw from the same pools
						clientSettings.SendBufferSettings.ResendQueueBudget = self.settings.SendBufferSettings.ResendQueueBudget
						clientSettings.ReceiveBufferSettings.ReceiveQueueBudget = self.settings.ReceiveBufferSettings.ReceiveQueueBudget
						return clientSettings
					},
					connect.DefaultApiMultiClientGeneratorSettings(),
				)
			}
			settings := connect.DefaultMultiClientSettings()
			settings.Log = self.log
			settings.DefaultPerformanceProfile = toConnectPerformanceProfile(self.performanceProfile)
			if self.clientSecurityPolicyGenerator != nil {
				settings.SecurityPolicyGenerator = self.clientSecurityPolicyGenerator
			}
			// interpose the upgrade mux on the exit path: the multi-client delivers to
			// the mux's Receive (mux-addressed replies terminate on its internal stack,
			// the rest flow on to remoteReceive), and the send path runs through the mux
			// (it claims DNS/HTTP, else forwards to the multi-client).
			muxReceive := connect.ReceivePacketFunction(remoteReceive)
			var upgradeMux *connect.UpgradeMux
			if self.upgradeMuxSettings != nil {
				m, err := connect.NewUpgradeMux(
					self.ctx,
					connect.SourceId(self.clientId),
					protocol.ProvideMode_Network,
					self.settings.SendTimeout,
					remoteReceive,
					self.upgradeMuxSettings,
					self.log,
				)
				if err != nil {
					self.log.Infof("[device]upgrade mux unavailable, passing through: %s\n", err)
				} else {
					upgradeMux = m
					muxReceive = m.Receive
				}
			}

			multi := connect.NewRemoteUserNatMultiClient(
				self.ctx,
				generator,
				muxReceive,
				protocol.ProvideMode_Public,
				settings,
			)
			self.contractStatusSub = multi.AddContractStatusCallback(self.updateContractStatus)
			self.remoteUserNatClient = multi
			if upgradeMux != nil {
				upgradeMux.SetUpstream(multi.SendPacket)
				// the mux's DNS reverse index drives ServerName path affinity (point 4)
				multi.SetServerNameLookup(upgradeMux)
				// the mux blocks ad/tracker hostnames at the dns layer
				upgradeMux.SetBlocker(self.blocker)
				self.upgradeMux = upgradeMux
			}
			monitor := multi.Monitor()
			windowMonitorEvent := func(windowExpandEvent *connect.WindowExpandEvent, providerEvents map[connect.Id]*connect.ProviderEvent, reset bool) {
				windowStatus := toWindowStatus(monitor)
				changed := false
				func() {
					self.stateLock.Lock()
					defer self.stateLock.Unlock()
					if self.lastWindowStatus == nil || *self.lastWindowStatus != *windowStatus {
						self.lastWindowStatus = windowStatus
						changed = true
					}
				}()
				if changed {
					self.windowStatusChanged(windowStatus)
				}
			}
			self.windowMonitorSub = monitor.AddMonitorEventCallback(windowMonitorEvent)
			// }

			self.remoteUserNatClient.SetLocalSecurityBypass(self.routeLocal)

			// the multi client blocks ad/tracker ips and reverse-index
			// hostnames (the backstop under the mux's dns-layer blocking)
			multi.SetBlocker(self.blocker)

			multi.SetBlockActionOverrides(connectBlockActionOverrides(self.blockActionOverrides))
			// exclude the resolver endpoints from the override and association logic
			multi.SetBlockActionIgnoreHosts(dnsIgnoreHostValues(self.dnsResolverSettingsWithLock()))
			self.blockActionSub = multi.AddBlockActionCallback(self.updateBlockActions)
			self.packetStatsSub = multi.AddPacketStatsCallback(self.updatePacketStats)
			self.contractStatsEventSub = multi.AddContractStatsCallback(self.updateContractStatsEvents)

			if self.provideControlMode == ProvideControlModeAuto {
				provideChanged = self.setProvideModeWithLock(ProvideModePublic)
			}
		} else {
			// else no specs, not an error
			if self.provideControlMode == ProvideControlModeAuto {
				provideChanged = self.setProvideModeWithLock(ProvideModeNone)
			}
		}
		self.updateSendRouteWithLock()
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

// deviceLocalSendRoute is an immutable snapshot of the routing fields read on
// the per-packet send path. see `DeviceLocal.sendRoute`.
type deviceLocalSendRoute struct {
	remoteUserNatClient connect.UserNatClient
	upgradeMux          *connect.UpgradeMux
	routeLocal          bool
	provider            *deviceLocalProvider
}

// must be called with `stateLock`
func (self *DeviceLocal) updateSendRouteWithLock() {
	self.sendRoute.Store(&deviceLocalSendRoute{
		remoteUserNatClient: self.remoteUserNatClient,
		upgradeMux:          self.upgradeMux,
		routeLocal:          self.routeLocal,
		provider:            self.provider,
	})
}

func (self *DeviceLocal) sendPacket(packet []byte) bool {
	source := connect.SourceId(self.clientId)

	// read the routing snapshot lock-free; it is rebuilt under `stateLock`
	// whenever the routing fields change
	route := self.sendRoute.Load()

	if route.upgradeMux != nil {
		// the mux claims DNS/HTTP and forwards everything else to remoteUserNatClient
		self.stats.UpdateRemoteSend(ByteCount(len(packet)))
		return route.upgradeMux.SendPacket(
			source,
			protocol.ProvideMode_Network,
			packet,
			self.settings.SendTimeout,
		)
	} else if route.remoteUserNatClient != nil {
		self.stats.UpdateRemoteSend(ByteCount(len(packet)))
		return route.remoteUserNatClient.SendPacket(
			source,
			protocol.ProvideMode_Network,
			packet,
			self.settings.SendTimeout,
		)
	} else if route.routeLocal {
		var localUserNat *connect.LocalUserNat
		if route.provider != nil {
			localUserNat = route.provider.LocalUserNat()
		}
		if localUserNat != nil {
			// route locally. Use the same send timeout as the remote/mux paths:
			// LocalUserNat assumes a lossless, in-order source and implements no
			// retransmit, so a non-blocking (timeout 0) send that drops on a full
			// channel corrupts the flow's protocol state under backpressure. Blocking
			// up to SendTimeout applies backpressure to the caller instead of dropping.
			success := localUserNat.SendPacket(
				source,
				protocol.ProvideMode_Network,
				packet,
				self.settings.SendTimeout,
			)
			if success {
				self.localFallbackEgressPacketCount.Add(1)
				self.localFallbackEgressByteCount.Add(int64(len(packet)))
			}
			return success
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
			ipProtocol = IpProtocolUnknown
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

	// return the address to the pool only when it was drawn from it (i.e. the
	// experimental random 10.x address was not used); mirrors the allocation in
	// newDeviceLocalWithOverrides so a non-pool address never pollutes the free list.
	if !self.settings.UseExperimentalTunnelAddress {
		connect.ReturnLocalIpv4Address(self.tunnelLocalAddress)
	}

	if self.providerContractStatsEventSub != nil {
		self.providerContractStatsEventSub()
		self.providerContractStatsEventSub = nil
	}
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
	if self.upgradeMux != nil {
		self.upgradeMux.Close()
		self.upgradeMux = nil
	}
	self.closeRemoteUserNatClientWithLock()
	self.updateSendRouteWithLock()
	// self.localUserNat.RemoveReceivePacketCallback(self.receive)
	if self.localUserNatSub != nil {
		self.localUserNatSub()
		self.localUserNatSub = nil
	}
	if self.remoteUserNatProviderLocalUserNat != nil {
		self.remoteUserNatProviderLocalUserNat.Close()
		self.remoteUserNatProviderLocalUserNat = nil
	}
	if self.providerPacketStatsSub != nil {
		self.providerPacketStatsSub()
		self.providerPacketStatsSub = nil
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
	if self.hostedIncompatibleGuarded("SetRpcServer") {
		return nil
	}
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

// StartHostedRpc runs the rpc over a custom listener (rather than binding a
// localhost websocket server) and marks every rpc session as
// hosted-incompatible, so the remote cannot change route local or provide
// settings. deviceGeneration identifies this DeviceLocal instance; the host
// stamps a fresh value each time it recreates the device, so a DeviceRemote
// can detect the recreate across reconnects. Used by the platform proxy host,
// where the resident bridge feeds the listener.
func (self *DeviceLocal) StartHostedRpc(listener DeviceRpcListener, deviceGeneration string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	settings := defaultDeviceRpcSettings()
	settings.DisableLogging = self.settings.DisableLogging
	settings.ClientSettings.Log = self.settings.ClientSettings.Log
	settings.DisableHostedIncompatible = true
	settings.DeviceGeneration = deviceGeneration

	if self.deviceLocalRpcManager != nil {
		self.deviceLocalRpcManager.Close()
	}
	// see the companion convention note in device_rpc_transport.go
	self.deviceLocalRpcManager = newDeviceLocalRpcManager(self.ctx, self, settings, listener.(deviceRpcListener))
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

// privacy block

// must be called with `stateLock`. tears down the client event subscriptions
// and folds the client's final packet counters into the device accumulators
// before closing it. the contracts die with the client
func (self *DeviceLocal) closeRemoteUserNatClientWithLock() {
	if self.blockActionSub != nil {
		self.blockActionSub()
		self.blockActionSub = nil
	}
	if self.packetStatsSub != nil {
		self.packetStatsSub()
		self.packetStatsSub = nil
	}
	if self.contractStatsEventSub != nil {
		self.contractStatsEventSub()
		self.contractStatsEventSub = nil
	}
	if self.remoteUserNatClient != nil {
		if multi, ok := self.remoteUserNatClient.(*connect.RemoteUserNatMultiClient); ok {
			addConnectPacketStats(&self.packetStatsBase, multi.PacketStats())
		}
		self.remoteUserNatClient.Close()
		self.remoteUserNatClient = nil
	}
	self.contracts.clear()
}

func addConnectPacketStats(out *connect.PacketStats, add *connect.PacketStats) {
	out.RemoteEgressPacketCount += add.RemoteEgressPacketCount
	out.RemoteEgressByteCount += add.RemoteEgressByteCount
	out.RemoteIngressPacketCount += add.RemoteIngressPacketCount
	out.RemoteIngressByteCount += add.RemoteIngressByteCount
	out.LocalEgressPacketCount += add.LocalEgressPacketCount
	out.LocalEgressByteCount += add.LocalEgressByteCount
	out.LocalIngressPacketCount += add.LocalIngressPacketCount
	out.LocalIngressByteCount += add.LocalIngressByteCount
	out.BlockEgressPacketCount += add.BlockEgressPacketCount
	out.BlockEgressByteCount += add.BlockEgressByteCount
	out.BlockIngressPacketCount += add.BlockIngressPacketCount
	out.BlockIngressByteCount += add.BlockIngressByteCount
}

// overrides with no hosts (app id only) are applied by the platform,
// not the packet path
func connectBlockActionOverrides(overrides []*BlockActionOverride) []*connect.BlockActionOverride {
	connectOverrides := []*connect.BlockActionOverride{}
	for _, override := range overrides {
		if override.OverrideId == nil || override.Hosts == nil || override.Hosts.Len() == 0 {
			continue
		}
		connectOverride := &connect.BlockActionOverride{
			OverrideId: override.OverrideId.toConnectId(),
			Hosts:      override.Hosts.getAll(),
		}
		if override.BlockOverride != nil {
			connectOverride.BlockOverride = &connect.BlockOverride{Block: override.BlockOverride.Block}
		}
		if override.RouteOverride != nil {
			connectOverride.RouteOverride = &connect.RouteOverride{Local: override.RouteOverride.Local}
		}
		connectOverrides = append(connectOverrides, connectOverride)
	}
	return connectOverrides
}

// must be called with `stateLock`
func (self *DeviceLocal) combinedConnectPacketStatsWithLock() *connect.PacketStats {
	combined := self.packetStatsBase
	if multi, ok := self.remoteUserNatClient.(*connect.RemoteUserNatMultiClient); ok {
		addConnectPacketStats(&combined, multi.PacketStats())
	}
	return &combined
}

func packetStatsFromConnect(packetStats *connect.PacketStats) *PacketStats {
	return &PacketStats{
		RemoteEgressPacketCount:  packetStats.RemoteEgressPacketCount,
		RemoteEgressByteCount:    packetStats.RemoteEgressByteCount,
		RemoteIngressPacketCount: packetStats.RemoteIngressPacketCount,
		RemoteIngressByteCount:   packetStats.RemoteIngressByteCount,
		LocalEgressPacketCount:   packetStats.LocalEgressPacketCount,
		LocalEgressByteCount:     packetStats.LocalEgressByteCount,
		LocalIngressPacketCount:  packetStats.LocalIngressPacketCount,
		LocalIngressByteCount:    packetStats.LocalIngressByteCount,
		BlockEgressPacketCount:   packetStats.BlockEgressPacketCount,
		BlockEgressByteCount:     packetStats.BlockEgressByteCount,
		BlockIngressPacketCount:  packetStats.BlockIngressPacketCount,
		BlockIngressByteCount:    packetStats.BlockIngressByteCount,
	}
}

// the client route stats: the multi client counters plus the fallback local route
func (self *DeviceLocal) clientPacketStatsFromConnect(packetStats *connect.PacketStats) *PacketStats {
	stats := packetStatsFromConnect(packetStats)
	stats.LocalEgressPacketCount += self.localFallbackEgressPacketCount.Load()
	stats.LocalEgressByteCount += ByteCount(self.localFallbackEgressByteCount.Load())
	stats.LocalIngressPacketCount += self.localFallbackIngressPacketCount.Load()
	stats.LocalIngressByteCount += ByteCount(self.localFallbackIngressByteCount.Load())
	return stats
}

// blocked includes incident-class drops (martians/malformed)
func (self *DeviceLocal) blockStatsFromConnect(packetStats *connect.PacketStats) *BlockStats {
	return &BlockStats{
		AllowedCount: int(packetStats.RemoteEgressPacketCount + packetStats.LocalEgressPacketCount + self.localFallbackEgressPacketCount.Load()),
		BlockedCount: int(packetStats.BlockEgressPacketCount + packetStats.BlockIngressPacketCount),
	}
}

func (self *DeviceLocal) GetBlockStats() *BlockStats {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.blockStatsFromConnect(self.combinedConnectPacketStatsWithLock())
}

func (self *DeviceLocal) GetPacketStats() *PacketStats {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.clientPacketStatsFromConnect(self.combinedConnectPacketStatsWithLock())
}

// the packet stats epoch callback from the multi client
func (self *DeviceLocal) updatePacketStats(packetStats *connect.PacketStats) {
	var netPacketStats *PacketStats
	var netBlockStats *BlockStats
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		combined := self.packetStatsBase
		addConnectPacketStats(&combined, packetStats)
		netPacketStats = self.clientPacketStatsFromConnect(&combined)
		blockStats := self.blockStatsFromConnect(&combined)
		if *blockStats != self.netBlockStats {
			self.netBlockStats = *blockStats
			netBlockStats = blockStats
		}
	}()
	self.packetStatsChanged(netPacketStats)
	if netBlockStats != nil {
		self.blockStatsChanged(netBlockStats)
	}
}

// provider packet stats

// must be called with `stateLock`
func (self *DeviceLocal) combinedProviderConnectPacketStatsWithLock() *connect.PacketStats {
	combined := self.providerPacketStatsBase
	if self.remoteUserNatProvider != nil {
		addConnectPacketStats(&combined, self.remoteUserNatProvider.PacketStats())
	}
	return &combined
}

// devices with the provider disabled have no provider packet stats
func (self *DeviceLocal) GetProviderPacketStats() *PacketStats {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.provider == nil {
		return nil
	}
	return packetStatsFromConnect(self.combinedProviderConnectPacketStatsWithLock())
}

// the packet stats epoch callback from the provider user nat
func (self *DeviceLocal) updateProviderPacketStats(packetStats *connect.PacketStats) {
	var netPacketStats *PacketStats
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		combined := self.providerPacketStatsBase
		addConnectPacketStats(&combined, packetStats)
		netPacketStats = packetStatsFromConnect(&combined)
	}()
	self.providerPacketStatsChanged(netPacketStats)
}

func (self *DeviceLocal) AddProviderPacketStatsChangeListener(listener PacketStatsChangeListener) Sub {
	callbackId := self.providerPacketStatsChangeListeners.Add(listener)
	return newSub(func() {
		self.providerPacketStatsChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) providerPacketStatsChanged(packetStats *PacketStats) {
	for _, listener := range self.providerPacketStatsChangeListeners.Get() {
		connect.HandleError(func() {
			listener.PacketStatsChanged(packetStats)
		})
	}
}

// the block action epoch callback from the multi client
func (self *DeviceLocal) updateBlockActions(blockActions []*connect.BlockAction) {
	var window *BlockActionWindow
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		for _, blockAction := range blockActions {
			self.blockActions = append(self.blockActions, self.blockActionFromConnectWithLock(blockAction))
		}
		self.trimBlockActionsWithLock()
		window = self.blockActionWindowWithLock()
	}()
	self.blockActionWindowChanged(window)
}

// must be called with `stateLock`
func (self *DeviceLocal) blockActionFromConnectWithLock(blockAction *connect.BlockAction) *BlockAction {
	ips := NewStringList()
	for _, ip := range blockAction.Ips {
		ips.Add(ip.String())
	}
	hosts := NewStringList()
	hosts.addAll(blockAction.Hosts...)
	out := &BlockAction{
		BlockActionId: NewId(),
		Time:          blockAction.Time.UnixMilli(),
		Ips:           ips,
		Hosts:         hosts,
		Block:         blockAction.Block,
		Local:         blockAction.Local,
		PacketCount:   blockAction.PacketCount,
		ByteCount:     blockAction.ByteCount,
	}
	// resolve the applied overrides. when an override was removed since the
	// decision, reflect the decision itself
	if blockAction.BlockOverrideId != nil {
		out.OverrideId = newId(*blockAction.BlockOverrideId)
		out.BlockOverride = &BlockOverride{Block: blockAction.Block}
		if override := self.blockActionOverrideWithLock(*blockAction.BlockOverrideId); override != nil && override.BlockOverride != nil {
			out.BlockOverride = &BlockOverride{Block: override.BlockOverride.Block}
		}
	}
	if blockAction.RouteOverrideId != nil {
		// the block override's id wins when the decisions came from different overrides
		if out.OverrideId == nil {
			out.OverrideId = newId(*blockAction.RouteOverrideId)
		}
		out.RouteOverride = &RouteOverride{Local: blockAction.Local}
		if override := self.blockActionOverrideWithLock(*blockAction.RouteOverrideId); override != nil && override.RouteOverride != nil {
			out.RouteOverride = &RouteOverride{Local: override.RouteOverride.Local}
		}
	}
	return out
}

// must be called with `stateLock`
func (self *DeviceLocal) blockActionOverrideWithLock(overrideId connect.Id) *BlockActionOverride {
	for _, override := range self.blockActionOverrides {
		if override.OverrideId != nil && override.OverrideId.toConnectId() == overrideId {
			return override
		}
	}
	return nil
}

// must be called with `stateLock`
func (self *DeviceLocal) trimBlockActionsWithLock() {
	windowStartTime := time.Now().Add(-self.settings.BlockActionWindowDuration).UnixMilli()
	i := 0
	for i < len(self.blockActions) && self.blockActions[i].Time < windowStartTime {
		i += 1
	}
	if d := len(self.blockActions) - i - self.settings.BlockActionWindowMaxCount; 0 < d {
		i += d
	}
	if 0 < i {
		self.blockActions = append([]*BlockAction{}, self.blockActions[i:]...)
	}
}

// must be called with `stateLock`
func (self *DeviceLocal) blockActionWindowWithLock() *BlockActionWindow {
	blockActions := NewBlockActionList()
	blockActions.addAll(self.blockActions...)
	return &BlockActionWindow{
		BlockActions: blockActions,
	}
}

func (self *DeviceLocal) GetBlockActions() *BlockActionWindow {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.trimBlockActionsWithLock()
	return self.blockActionWindowWithLock()
}

// must be called with `stateLock`.
// applies the overrides to the live client
func (self *DeviceLocal) updateBlockActionOverridesWithLock() {
	if multi, ok := self.remoteUserNatClient.(*connect.RemoteUserNatMultiClient); ok {
		multi.SetBlockActionOverrides(connectBlockActionOverrides(self.blockActionOverrides))
	}
}

// must be called with `stateLock`
func (self *DeviceLocal) blockActionOverridesWithLock() *BlockActionOverrideList {
	overrides := NewBlockActionOverrideList()
	overrides.addAll(self.blockActionOverrides...)
	return overrides
}

// persists the overrides to local state, asynchronously
func (self *DeviceLocal) persistBlockActionOverrides(overrides *BlockActionOverrideList) {
	if asyncLocalState := self.networkSpace.GetAsyncLocalState(); asyncLocalState != nil {
		asyncLocalState.serialAsync(func() error {
			return asyncLocalState.GetLocalState().SetBlockActionOverrides(overrides)
		})
	}
}

func (self *DeviceLocal) AddBlockActionOverride(override *BlockActionOverride) {
	if override == nil || override.OverrideId == nil {
		return
	}
	var overrides *BlockActionOverrideList
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		// replace an existing override with the same id
		self.removeBlockActionOverrideWithLock(override.OverrideId)
		self.blockActionOverrides = append(self.blockActionOverrides, override)
		self.updateBlockActionOverridesWithLock()
		overrides = self.blockActionOverridesWithLock()
	}()
	self.persistBlockActionOverrides(overrides)
	self.blockActionOverridesChanged(overrides)
}

// must be called with `stateLock`
func (self *DeviceLocal) removeBlockActionOverrideWithLock(overrideId *Id) bool {
	for i, override := range self.blockActionOverrides {
		if override.OverrideId != nil && override.OverrideId.Cmp(overrideId) == 0 {
			self.blockActionOverrides = append(
				self.blockActionOverrides[:i:i],
				self.blockActionOverrides[i+1:]...,
			)
			return true
		}
	}
	return false
}

func (self *DeviceLocal) RemoveBlockActionOverride(overrideId *Id) {
	if overrideId == nil {
		return
	}
	var overrides *BlockActionOverrideList
	removed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		removed = self.removeBlockActionOverrideWithLock(overrideId)
		if removed {
			self.updateBlockActionOverridesWithLock()
			overrides = self.blockActionOverridesWithLock()
		}
	}()
	if removed {
		self.persistBlockActionOverrides(overrides)
		self.blockActionOverridesChanged(overrides)
	}
}

func (self *DeviceLocal) SetBlockActionOverrides(overrides *BlockActionOverrideList) {
	var netOverrides *BlockActionOverrideList
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.blockActionOverrides = []*BlockActionOverride{}
		if overrides != nil {
			for _, override := range overrides.getAll() {
				if override.OverrideId == nil {
					continue
				}
				self.removeBlockActionOverrideWithLock(override.OverrideId)
				self.blockActionOverrides = append(self.blockActionOverrides, override)
			}
		}
		self.updateBlockActionOverridesWithLock()
		netOverrides = self.blockActionOverridesWithLock()
	}()
	self.persistBlockActionOverrides(netOverrides)
	self.blockActionOverridesChanged(netOverrides)
}

func (self *DeviceLocal) GetBlockActionOverrides() *BlockActionOverrideList {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.blockActionOverridesWithLock()
}

// GetLocalOverrideAppIds derives the app include/exclude sets from the
// overrides with app ids. app rules are enforced by the platform's per-app
// tunnel routing (currently Android), not the packet path
func (self *DeviceLocal) GetLocalOverrideAppIds() *OverrideLocalAppIds {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	included := NewStringList()
	excluded := NewStringList()
	seen := map[string]bool{}
	for _, override := range self.blockActionOverrides {
		if override.RouteOverride == nil || override.AppIds == nil {
			continue
		}
		for _, appId := range override.AppIds.getAll() {
			if seen[appId] {
				continue
			}
			seen[appId] = true
			if override.RouteOverride.Local {
				included.Add(appId)
			} else {
				excluded.Add(appId)
			}
		}
	}
	return &OverrideLocalAppIds{
		Included: included,
		Excluded: excluded,
	}
}

func (self *DeviceLocal) AddBlockActionWindowChangeListener(listener BlockActionWindowChangeListener) Sub {
	callbackId := self.blockActionWindowChangeListeners.Add(listener)
	return newSub(func() {
		self.blockActionWindowChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) AddBlockStatsChangeListener(listener BlockStatsChangeListener) Sub {
	callbackId := self.blockStatsChangeListeners.Add(listener)
	return newSub(func() {
		self.blockStatsChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) AddBlockActionOverridesChangeListener(listener BlockActionOverridesChangeListener) Sub {
	callbackId := self.blockActionOverridesChangeListeners.Add(listener)
	return newSub(func() {
		self.blockActionOverridesChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) AddPacketStatsChangeListener(listener PacketStatsChangeListener) Sub {
	callbackId := self.packetStatsChangeListeners.Add(listener)
	return newSub(func() {
		self.packetStatsChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) blockActionWindowChanged(blockActionWindow *BlockActionWindow) {
	for _, listener := range self.blockActionWindowChangeListeners.Get() {
		connect.HandleError(func() {
			listener.BlockActionWindowChanged(blockActionWindow)
		})
	}
}

func (self *DeviceLocal) blockStatsChanged(blockStats *BlockStats) {
	for _, listener := range self.blockStatsChangeListeners.Get() {
		connect.HandleError(func() {
			listener.BlockStatsChanged(blockStats)
		})
	}
}

func (self *DeviceLocal) blockActionOverridesChanged(blockActionOverrides *BlockActionOverrideList) {
	for _, listener := range self.blockActionOverridesChangeListeners.Get() {
		connect.HandleError(func() {
			listener.BlockActionOverridesChanged(blockActionOverrides)
		})
	}
}

func (self *DeviceLocal) packetStatsChanged(packetStats *PacketStats) {
	for _, listener := range self.packetStatsChangeListeners.Get() {
		connect.HandleError(func() {
			listener.PacketStatsChanged(packetStats)
		})
	}
}

// contract stats

// the state of an open contract, updated on the contract stats epoch
type deviceContractState struct {
	contractId        connect.Id
	path              connect.TransferPath
	companion         bool
	usedByteCount     ByteCount
	transferByteCount ByteCount
	bitRate           int
	open              bool
	updateTime        time.Time
}

// the open contracts of one connect client (the multi client or the provider
// client) and the listener dispatch gate.
// all methods must be called with the device `stateLock`
type deviceContractTracker struct {
	// egress (send) and ingress (receive) contracts, keyed by contract id
	egressContracts  map[connect.Id]*deviceContractState
	ingressContracts map[connect.Id]*deviceContractState
	// gates the listener dispatch to the contract stats epoch
	lastEmitTime time.Time
	// set when an update was folded in but gated, so `flushPending` still
	// carries it out. the connect side only produces a batch while bytes are
	// moving, so the last batch of a transfer lands in the gate and no further
	// batch ever arrives to carry it — without the trailing flush the pushed
	// stats sit permanently below what the getters report
	pendingEmit bool
}

func newDeviceContractTracker() *deviceContractTracker {
	return &deviceContractTracker{
		egressContracts:  map[connect.Id]*deviceContractState{},
		ingressContracts: map[connect.Id]*deviceContractState{},
	}
}

func (self *deviceContractTracker) clear() {
	clear(self.egressContracts)
	clear(self.ingressContracts)
	// emit the cleared (zero) stats. otherwise the listeners keep reporting the
	// last pre-disconnect values while the getters, reading the now empty maps,
	// report zero — push and pull disagreeing until the next connect
	self.pendingEmit = true
}

// applies a contract stats event batch.
// each source client emits on its own epoch, so the listener dispatch is gated
// to at most once per `epoch` across all of them.
// a batch with a close event always dispatches, so the final report of a
// contract is never swallowed by the gate.
// returns nil stats when gated
func (self *deviceContractTracker) update(
	events []*connect.ContractStatsEvent,
	epoch time.Duration,
) (egressStats *ContractStats, ingressStats *ContractStats, egressDetails *ContractDetailsList, ingressDetails *ContractDetailsList) {
	now := time.Now()
	hasClose := false
	for _, event := range events {
		contracts := self.egressContracts
		if event.Receive {
			contracts = self.ingressContracts
		}
		state, ok := contracts[event.ContractId]
		if !ok {
			state = &deviceContractState{
				contractId: event.ContractId,
				path:       event.Path,
				companion:  event.Companion,
				updateTime: now,
			}
			contracts[event.ContractId] = state
		}
		elapsed := now.Sub(state.updateTime)
		state.usedByteCount = event.UsedByteCount
		state.transferByteCount = event.TransferByteCount
		state.open = event.Open
		if !event.Open {
			hasClose = true
		}
		if ok && 0 < elapsed {
			state.bitRate = int(8 * float64(event.UsedByteCountDelta) / elapsed.Seconds())
		}
		state.updateTime = now
	}
	if !hasClose && now.Sub(self.lastEmitTime) < epoch {
		// gated. fold the state in and mark it un-emitted; `flushPending` carries
		// it out once the epoch passes. this used to just drop it, on the premise
		// that "the next batch past the epoch emits it" — but batches only arrive
		// while bytes are moving, so the final batch of a transfer was stranded
		self.pendingEmit = true
		return
	}
	return self.emit(now)
}

// emit builds the reports, evicts closed contracts, and opens a new epoch.
func (self *deviceContractTracker) emit(now time.Time) (egressStats *ContractStats, ingressStats *ContractStats, egressDetails *ContractDetailsList, ingressDetails *ContractDetailsList) {
	self.pendingEmit = false
	self.lastEmitTime = now
	egressStats = self.stats(false)
	ingressStats = self.stats(true)
	egressDetails = self.details(false)
	ingressDetails = self.details(true)
	// evict closed contracts after the final report
	for contractId, state := range self.egressContracts {
		if !state.open {
			delete(self.egressContracts, contractId)
		}
	}
	for contractId, state := range self.ingressContracts {
		if !state.open {
			delete(self.ingressContracts, contractId)
		}
	}
	return
}

// decayBitRates zeroes the bit rate of contracts that have gone idle, reporting
// whether any changed. bitRate is a time derivative recomputed only when an
// event for the contract arrives, and the connect side only produces an event
// while the byte count moves — so when a transfer stops, no event is ever
// generated again and the contract would otherwise report its last rate (say
// "40 Mbps") forever, on both the listener and the getter paths.
func (self *deviceContractTracker) decayBitRates(now time.Time, epoch time.Duration) bool {
	decayed := false
	for _, contracts := range []map[connect.Id]*deviceContractState{self.egressContracts, self.ingressContracts} {
		for _, state := range contracts {
			if state.bitRate != 0 && epoch <= now.Sub(state.updateTime) {
				state.bitRate = 0
				decayed = true
			}
		}
	}
	return decayed
}

// flushPending carries out a gated update once the epoch has passed, and decays
// the bit rate of idle contracts. returns nil stats when there is nothing to
// report. it settles: after the last real batch is emitted, one further emit
// zeroes the idle bit rates and then nothing more fires.
func (self *deviceContractTracker) flushPending(epoch time.Duration) (egressStats *ContractStats, ingressStats *ContractStats, egressDetails *ContractDetailsList, ingressDetails *ContractDetailsList) {
	now := time.Now()
	if now.Sub(self.lastEmitTime) < epoch {
		return
	}
	decayed := self.decayBitRates(now, epoch)
	if !self.pendingEmit && !decayed {
		return
	}
	return self.emit(now)
}

// the contract stats epoch callback from the multi client
func (self *DeviceLocal) updateContractStatsEvents(events []*connect.ContractStatsEvent) {
	var egressStats *ContractStats
	var ingressStats *ContractStats
	var egressDetails *ContractDetailsList
	var ingressDetails *ContractDetailsList
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		egressStats, ingressStats, egressDetails, ingressDetails = self.contracts.update(events, self.settings.ContractStatsEpoch)
	}()
	if egressStats == nil {
		return
	}
	self.egressContractStatsChanged(egressStats)
	self.ingressContractStatsChanged(ingressStats)
	self.egressContractDetailsChanged(egressDetails)
	self.ingressContractDetailsChanged(ingressDetails)
}

// runContractStatsFlush carries out the trailing state of a gated contract stats
// update, and decays the bit rate of contracts that have gone idle.
//
// `deviceContractTracker.update` folds a gated batch into the tracker and emits
// nothing, on the assumption that a later batch will carry it. But the connect
// side only produces a batch while a contract's byte count is moving, so the
// final batch of a transfer lands inside the gate and no further batch ever
// arrives — the pushed stats then sit permanently below what the getters report,
// and a finished transfer keeps reporting its last bit rate. This loop is the
// trailing edge the gate needs.
func (self *DeviceLocal) runContractStatsFlush() {
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(self.settings.ContractStatsEpoch):
		}
		self.flushContractStats()
	}
}

func (self *DeviceLocal) flushContractStats() {
	var egressStats *ContractStats
	var ingressStats *ContractStats
	var egressDetails *ContractDetailsList
	var ingressDetails *ContractDetailsList
	var providerEgressStats *ContractStats
	var providerIngressStats *ContractStats
	var providerEgressDetails *ContractDetailsList
	var providerIngressDetails *ContractDetailsList
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		egressStats, ingressStats, egressDetails, ingressDetails =
			self.contracts.flushPending(self.settings.ContractStatsEpoch)
		providerEgressStats, providerIngressStats, providerEgressDetails, providerIngressDetails =
			self.providerContracts.flushPending(self.settings.ContractStatsEpoch)
	}()
	if egressStats != nil {
		self.egressContractStatsChanged(egressStats)
		self.ingressContractStatsChanged(ingressStats)
		self.egressContractDetailsChanged(egressDetails)
		self.ingressContractDetailsChanged(ingressDetails)
	}
	if providerEgressStats != nil {
		self.providerEgressContractStatsChanged(providerEgressStats)
		self.providerIngressContractStatsChanged(providerIngressStats)
		self.providerEgressContractDetailsChanged(providerEgressDetails)
		self.providerIngressContractDetailsChanged(providerIngressDetails)
	}
}

// the contract stats epoch callback from the provider client
func (self *DeviceLocal) updateProviderContractStatsEvents(events []*connect.ContractStatsEvent) {
	var egressStats *ContractStats
	var ingressStats *ContractStats
	var egressDetails *ContractDetailsList
	var ingressDetails *ContractDetailsList
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		egressStats, ingressStats, egressDetails, ingressDetails = self.providerContracts.update(events, self.settings.ContractStatsEpoch)
	}()
	if egressStats == nil {
		return
	}
	self.providerEgressContractStatsChanged(egressStats)
	self.providerIngressContractStatsChanged(ingressStats)
	self.providerEgressContractDetailsChanged(egressDetails)
	self.providerIngressContractDetailsChanged(ingressDetails)
}

// the direction's own contracts fill the contract fields, and the opposite
// direction (the return path with the same peers) fills the companion fields
func (self *deviceContractTracker) stats(receive bool) *ContractStats {
	own := self.egressContracts
	other := self.ingressContracts
	if receive {
		own, other = other, own
	}
	stats := &ContractStats{}
	for _, state := range own {
		stats.ContractUsedByteCount += state.usedByteCount
		stats.ContractByteCount += state.transferByteCount
		stats.ContractBitRate += state.bitRate
	}
	for _, state := range other {
		stats.CompanionContractUsedByteCount += state.usedByteCount
		stats.CompanionContractByteCount += state.transferByteCount
		stats.CompanionContractBitRate += state.bitRate
	}
	return stats
}

// pairs each contract with its companion in the opposite direction by the peer
// client id: an exact reverse path match wins, else the most recently updated
// contract with the same peer
func (self *deviceContractTracker) details(receive bool) *ContractDetailsList {
	own := self.egressContracts
	other := self.ingressContracts
	if receive {
		own, other = other, own
	}
	details := NewContractDetailsList()
	for _, state := range own {
		peerClientId := state.path.DestinationId
		if receive {
			peerClientId = state.path.SourceId
		}
		var companionState *deviceContractState
		reversePath := connect.TransferPath{
			SourceId:      state.path.DestinationId,
			DestinationId: state.path.SourceId,
			StreamId:      state.path.StreamId,
		}
		for _, otherState := range other {
			otherPeerClientId := otherState.path.SourceId
			if receive {
				otherPeerClientId = otherState.path.DestinationId
			}
			if otherState.path == reversePath {
				companionState = otherState
				break
			}
			if otherPeerClientId == peerClientId {
				if companionState == nil || companionState.updateTime.Before(otherState.updateTime) {
					companionState = otherState
				}
			}
		}
		contractDetails := &ContractDetails{
			ContractId:            newId(state.contractId),
			ContractUsedByteCount: state.usedByteCount,
			ContractByteCount:     state.transferByteCount,
			ContractBitRate:       state.bitRate,
			ContractTransferPath:  fromConnect(state.path),
		}
		if companionState != nil {
			contractDetails.CompanionContractId = newId(companionState.contractId)
			contractDetails.CompanionContractUsedByteCount = companionState.usedByteCount
			contractDetails.CompanionContractByteCount = companionState.transferByteCount
			contractDetails.CompanionContractBitRate = companionState.bitRate
			contractDetails.CompanionContractTransferPath = fromConnect(companionState.path)
		}
		details.Add(contractDetails)
	}
	return details
}

func (self *DeviceLocal) GetEgressContractStats() *ContractStats {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.contracts.stats(false)
}

func (self *DeviceLocal) GetEgressContractDetails() *ContractDetailsList {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.contracts.details(false)
}

func (self *DeviceLocal) GetIngressContractStats() *ContractStats {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.contracts.stats(true)
}

func (self *DeviceLocal) GetIngressContractDetails() *ContractDetailsList {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.contracts.details(true)
}

// devices with the provider disabled have no provider contracts

func (self *DeviceLocal) GetProviderEgressContractStats() *ContractStats {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.provider == nil {
		return nil
	}
	return self.providerContracts.stats(false)
}

func (self *DeviceLocal) GetProviderEgressContractDetails() *ContractDetailsList {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.provider == nil {
		return nil
	}
	return self.providerContracts.details(false)
}

func (self *DeviceLocal) GetProviderIngressContractStats() *ContractStats {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.provider == nil {
		return nil
	}
	return self.providerContracts.stats(true)
}

func (self *DeviceLocal) GetProviderIngressContractDetails() *ContractDetailsList {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.provider == nil {
		return nil
	}
	return self.providerContracts.details(true)
}

func (self *DeviceLocal) AddEgressContractStatsChangeListener(listener ContractStatsChangeListener) Sub {
	callbackId := self.egressContractStatsChangeListeners.Add(listener)
	return newSub(func() {
		self.egressContractStatsChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) AddEgressContractDetailsChangeListener(listener ContractDetailsChangeListener) Sub {
	callbackId := self.egressContractDetailsChangeListeners.Add(listener)
	return newSub(func() {
		self.egressContractDetailsChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) AddIngressContractStatsChangeListener(listener ContractStatsChangeListener) Sub {
	callbackId := self.ingressContractStatsChangeListeners.Add(listener)
	return newSub(func() {
		self.ingressContractStatsChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) AddIngressContractDetailsChangeListener(listener ContractDetailsChangeListener) Sub {
	callbackId := self.ingressContractDetailsChangeListeners.Add(listener)
	return newSub(func() {
		self.ingressContractDetailsChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) AddProviderEgressContractStatsChangeListener(listener ContractStatsChangeListener) Sub {
	callbackId := self.providerEgressContractStatsChangeListeners.Add(listener)
	return newSub(func() {
		self.providerEgressContractStatsChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) AddProviderEgressContractDetailsChangeListener(listener ContractDetailsChangeListener) Sub {
	callbackId := self.providerEgressContractDetailsChangeListeners.Add(listener)
	return newSub(func() {
		self.providerEgressContractDetailsChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) AddProviderIngressContractStatsChangeListener(listener ContractStatsChangeListener) Sub {
	callbackId := self.providerIngressContractStatsChangeListeners.Add(listener)
	return newSub(func() {
		self.providerIngressContractStatsChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) AddProviderIngressContractDetailsChangeListener(listener ContractDetailsChangeListener) Sub {
	callbackId := self.providerIngressContractDetailsChangeListeners.Add(listener)
	return newSub(func() {
		self.providerIngressContractDetailsChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) egressContractStatsChanged(contractStats *ContractStats) {
	for _, listener := range self.egressContractStatsChangeListeners.Get() {
		connect.HandleError(func() {
			listener.ContractStatsChanged(contractStats)
		})
	}
}

func (self *DeviceLocal) egressContractDetailsChanged(contractDetails *ContractDetailsList) {
	for _, listener := range self.egressContractDetailsChangeListeners.Get() {
		connect.HandleError(func() {
			for _, details := range contractDetails.getAll() {
				listener.ContractDetailsChanged(details)
			}
		})
	}
}

func (self *DeviceLocal) ingressContractStatsChanged(contractStats *ContractStats) {
	for _, listener := range self.ingressContractStatsChangeListeners.Get() {
		connect.HandleError(func() {
			listener.ContractStatsChanged(contractStats)
		})
	}
}

func (self *DeviceLocal) ingressContractDetailsChanged(contractDetails *ContractDetailsList) {
	for _, listener := range self.ingressContractDetailsChangeListeners.Get() {
		connect.HandleError(func() {
			for _, details := range contractDetails.getAll() {
				listener.ContractDetailsChanged(details)
			}
		})
	}
}

func (self *DeviceLocal) providerEgressContractStatsChanged(contractStats *ContractStats) {
	for _, listener := range self.providerEgressContractStatsChangeListeners.Get() {
		connect.HandleError(func() {
			listener.ContractStatsChanged(contractStats)
		})
	}
}

func (self *DeviceLocal) providerEgressContractDetailsChanged(contractDetails *ContractDetailsList) {
	for _, listener := range self.providerEgressContractDetailsChangeListeners.Get() {
		connect.HandleError(func() {
			for _, details := range contractDetails.getAll() {
				listener.ContractDetailsChanged(details)
			}
		})
	}
}

func (self *DeviceLocal) providerIngressContractStatsChanged(contractStats *ContractStats) {
	for _, listener := range self.providerIngressContractStatsChangeListeners.Get() {
		connect.HandleError(func() {
			listener.ContractStatsChanged(contractStats)
		})
	}
}

func (self *DeviceLocal) providerIngressContractDetailsChanged(contractDetails *ContractDetailsList) {
	for _, listener := range self.providerIngressContractDetailsChangeListeners.Get() {
		connect.HandleError(func() {
			for _, details := range contractDetails.getAll() {
				listener.ContractDetailsChanged(details)
			}
		})
	}
}

// dns

func dnsResolverSettingsFromConnect(resolver *connect.DnsResolverSettings) *DnsResolverSettings {
	stringListOf := func(values []string) *StringList {
		list := NewStringList()
		list.addAll(values...)
		return list
	}
	return &DnsResolverSettings{
		EnableRemoteDoh:   resolver.EnableRemoteDoh,
		EnableLocalDoh:    resolver.EnableLocalDoh,
		EnableRemoteDns:   resolver.EnableRemoteDns,
		EnableLocalDns:    resolver.EnableLocalDns,
		RemoteDohUrlsIpv4: stringListOf(resolver.RemoteDohUrlsIpv4),
		RemoteDohUrlsIpv6: stringListOf(resolver.RemoteDohUrlsIpv6),
		LocalDohUrlsIpv4:  stringListOf(resolver.LocalDohUrlsIpv4),
		LocalDohUrlsIpv6:  stringListOf(resolver.LocalDohUrlsIpv6),
		RemoteDnsIpv4:     stringListOf(resolver.RemoteDnsIpv4),
		RemoteDnsIpv6:     stringListOf(resolver.RemoteDnsIpv6),
		LocalDnsIpv4:      stringListOf(resolver.LocalDnsIpv4),
		LocalDnsIpv6:      stringListOf(resolver.LocalDnsIpv6),
	}
}

func (self *DnsResolverSettings) toConnect() *connect.DnsResolverSettings {
	stringsOf := func(list *StringList) []string {
		if list == nil {
			return nil
		}
		return list.getAll()
	}
	return &connect.DnsResolverSettings{
		EnableRemoteDoh:   self.EnableRemoteDoh,
		EnableLocalDoh:    self.EnableLocalDoh,
		EnableRemoteDns:   self.EnableRemoteDns,
		EnableLocalDns:    self.EnableLocalDns,
		RemoteDohUrlsIpv4: stringsOf(self.RemoteDohUrlsIpv4),
		RemoteDohUrlsIpv6: stringsOf(self.RemoteDohUrlsIpv6),
		LocalDohUrlsIpv4:  stringsOf(self.LocalDohUrlsIpv4),
		LocalDohUrlsIpv6:  stringsOf(self.LocalDohUrlsIpv6),
		RemoteDnsIpv4:     stringsOf(self.RemoteDnsIpv4),
		RemoteDnsIpv6:     stringsOf(self.RemoteDnsIpv6),
		LocalDnsIpv4:      stringsOf(self.LocalDnsIpv4),
		LocalDnsIpv6:      stringsOf(self.LocalDnsIpv6),
	}
}

// upgradeMuxSettingsWithDnsResolverSettings builds the next upgrade mux settings
// with the resolver and the derived fallback applied, copy on write.
// returns nil when the mux is disabled (base nil)
func upgradeMuxSettingsWithDnsResolverSettings(base *connect.UpgradeMuxSettings, dnsResolverSettings *DnsResolverSettings) *connect.UpgradeMuxSettings {
	if base == nil {
		return nil
	}
	nextSettings := *base
	var nextDns connect.DnsUpgradeSettings
	if nextSettings.Dns != nil {
		nextDns = *nextSettings.Dns
	} else {
		nextDns = *connect.DefaultUpgradeMuxSettings().Dns
	}
	resolver := dnsResolverSettings.toConnect()
	nextDns.Resolver = resolver
	if dnsResolverSettings.EnableFallback {
		nextDns.Fallback = hostFallbackDnsResolverSettings(resolver)
	} else {
		// nil disables the fallback (see `connect.DnsUpgradeSettings`)
		nextDns.Fallback = nil
	}
	nextSettings.Dns = &nextDns
	return &nextSettings
}

// hostFallbackDnsResolverSettings derives the fallback resolver, which bridges
// tunnel startup by resolving over the host network, as the host-side projection
// of the resolver: remote entries are host-dialed (remote doh urls become local
// doh urls, remote dns ips become local dns ips)
func hostFallbackDnsResolverSettings(resolver *connect.DnsResolverSettings) *connect.DnsResolverSettings {
	union := func(locals []string, remotes []string) []string {
		var out []string
		seen := map[string]bool{}
		for _, value := range append(append([]string{}, locals...), remotes...) {
			if !seen[value] {
				seen[value] = true
				out = append(out, value)
			}
		}
		return out
	}
	return &connect.DnsResolverSettings{
		EnableLocalDoh:   resolver.EnableLocalDoh || resolver.EnableRemoteDoh,
		EnableLocalDns:   resolver.EnableLocalDns || resolver.EnableRemoteDns,
		LocalDohUrlsIpv4: union(resolver.LocalDohUrlsIpv4, resolver.RemoteDohUrlsIpv4),
		LocalDohUrlsIpv6: union(resolver.LocalDohUrlsIpv6, resolver.RemoteDohUrlsIpv6),
		LocalDnsIpv4:     union(resolver.LocalDnsIpv4, resolver.RemoteDnsIpv4),
		LocalDnsIpv6:     union(resolver.LocalDnsIpv6, resolver.RemoteDnsIpv6),
	}
}

// SetDnsResolverSettings sets the mux tunnel resolver and the derived
// host-side fallback (used to bridge tunnel startup), persisted to local state.
// TLS/cert pinning is applied internally and is not part of this surface
func (self *DeviceLocal) SetDnsResolverSettings(dnsResolverSettings *DnsResolverSettings) {
	if dnsResolverSettings == nil {
		return
	}
	var upgradeMuxSettings *connect.UpgradeMuxSettings
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		upgradeMuxSettings = upgradeMuxSettingsWithDnsResolverSettings(self.upgradeMuxSettings, dnsResolverSettings)
		// exclude the resolver endpoints from the override and association logic
		self.updateBlockActionIgnoreHostsWithLock(dnsResolverSettings)
	}()
	if upgradeMuxSettings == nil {
		// the mux is disabled. there is no resolver to configure
		return
	}
	// applies to the live mux, if any
	self.SetUpgradeMuxSettings(upgradeMuxSettings)
	self.persistDnsResolverSettings(dnsResolverSettings)
	self.dnsResolverSettingsChanged(self.GetDnsResolverSettings())
}

// persists the dns resolver settings to local state, asynchronously
func (self *DeviceLocal) persistDnsResolverSettings(dnsResolverSettings *DnsResolverSettings) {
	if asyncLocalState := self.networkSpace.GetAsyncLocalState(); asyncLocalState != nil {
		asyncLocalState.serialAsync(func() error {
			return asyncLocalState.GetLocalState().SetDnsResolverSettings(dnsResolverSettings)
		})
	}
}

func (self *DeviceLocal) GetDnsResolverSettings() *DnsResolverSettings {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.dnsResolverSettingsWithLock()
}

// must be called with `stateLock`
func (self *DeviceLocal) dnsResolverSettingsWithLock() *DnsResolverSettings {
	if self.upgradeMuxSettings == nil || self.upgradeMuxSettings.Dns == nil || self.upgradeMuxSettings.Dns.Resolver == nil {
		return nil
	}
	dnsResolverSettings := dnsResolverSettingsFromConnect(self.upgradeMuxSettings.Dns.Resolver)
	dnsResolverSettings.EnableFallback = self.upgradeMuxSettings.Dns.Fallback != nil
	return dnsResolverSettings
}

// must be called with `stateLock`.
// applies the resolver endpoints to the live client's ignore list, so
// resolver traffic is never captured by user override rules or clustered
// with user traffic
func (self *DeviceLocal) updateBlockActionIgnoreHostsWithLock(dnsResolverSettings *DnsResolverSettings) {
	if multi, ok := self.remoteUserNatClient.(*connect.RemoteUserNatMultiClient); ok {
		multi.SetBlockActionIgnoreHosts(dnsIgnoreHostValues(dnsResolverSettings))
	}
}

// the host values (hostnames and ips) of the resolver endpoints,
// used to exclude resolver traffic from the override and association logic
func dnsIgnoreHostValues(dnsResolverSettings *DnsResolverSettings) []string {
	if dnsResolverSettings == nil {
		return nil
	}
	values := []string{}
	seen := map[string]bool{}
	add := func(value string) {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		values = append(values, value)
	}
	addAll := func(list *StringList) {
		if list == nil {
			return
		}
		for _, value := range list.getAll() {
			add(value)
		}
	}
	// doh urls contribute their host (a hostname or a literal ip)
	addUrlHosts := func(list *StringList) {
		if list == nil {
			return
		}
		for _, value := range list.getAll() {
			if u, err := url.Parse(strings.TrimSpace(value)); err == nil && u.Hostname() != "" {
				add(u.Hostname())
			} else {
				add(value)
			}
		}
	}
	addUrlHosts(dnsResolverSettings.RemoteDohUrlsIpv4)
	addUrlHosts(dnsResolverSettings.RemoteDohUrlsIpv6)
	addUrlHosts(dnsResolverSettings.LocalDohUrlsIpv4)
	addUrlHosts(dnsResolverSettings.LocalDohUrlsIpv6)
	addAll(dnsResolverSettings.RemoteDnsIpv4)
	addAll(dnsResolverSettings.RemoteDnsIpv6)
	addAll(dnsResolverSettings.LocalDnsIpv4)
	addAll(dnsResolverSettings.LocalDnsIpv6)
	return values
}

func (self *DeviceLocal) AddDnsResolverSettingsChangeListener(listener DnsResolverSettingsChangeListener) Sub {
	callbackId := self.dnsResolverSettingsChangeListeners.Add(listener)
	return newSub(func() {
		self.dnsResolverSettingsChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) dnsResolverSettingsChanged(dnsResolverSettings *DnsResolverSettings) {
	for _, listener := range self.dnsResolverSettingsChangeListeners.Get() {
		connect.HandleError(func() {
			listener.DnsResolverSettingsChanged(dnsResolverSettings)
		})
	}
}

// network peers

// devices with the provider disabled have no network peers
func (self *DeviceLocal) GetNetworkPeers() *NetworkPeers {
	client := self.providerClientSnapshot()
	if client == nil {
		return nil
	}
	connected, disconnectedCount := client.NetworkPeers()
	networkPeers := &NetworkPeers{
		Connected:         NewNetworkPeerList(),
		DisconnectedCount: disconnectedCount,
	}
	for _, peer := range connected {
		roles := NewStringList()
		roles.addAll(peer.Roles...)
		networkPeers.Connected.Add(&NetworkPeer{
			ClientId:       newId(peer.ClientId),
			ProvideEnabled: peer.ProvideEnabled,
			Principal:      peer.Principal,
			Roles:          roles,
			DeviceSpec:     peer.DeviceSpec,
			DeviceName:     peer.DeviceName,
		})
	}
	return networkPeers
}

func (self *DeviceLocal) AddNetworkPeersChangeListener(listener NetworkPeersChangeListener) Sub {
	callbackId := self.networkPeersChangeListeners.Add(listener)
	return newSub(func() {
		self.networkPeersChangeListeners.Remove(callbackId)
	})
}

// watchNetworkPeers fires the network peers change listeners when the
// provider client's peer state changes, at most once per epoch. `notify` is the
// monitor channel grabbed synchronously at construction (before any change can
// be injected), so the first change is never missed.
func (self *DeviceLocal) watchNetworkPeers(notify chan struct{}) {
	client := self.providerClientSnapshot()
	if client == nil {
		return
	}
	peersMonitor := client.PeerManager().PeersMonitor()
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-notify:
		}
		// coalesce changes within the epoch
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(self.settings.NetworkPeersEpoch):
		}
		// re-arm immediately before the snapshot, NOT before the coalescing
		// window. the emit below reads the complete current peer state, so every
		// change that lands during the window is already carried by it. arming
		// ahead of the window instead leaves that channel closed by those same
		// changes (`Monitor.NotifyAll` closes the live channel), so the next loop
		// iteration fires immediately and re-emits an identical snapshot one epoch
		// later — a duplicate emit that breaks the at-most-once-per-epoch contract.
		// arm before the read and never after: a change racing the snapshot then
		// triggers the next round rather than being lost.
		notify = peersMonitor.NotifyChannel()
		self.networkPeersChanged(self.GetNetworkPeers())
	}
}

func (self *DeviceLocal) networkPeersChanged(networkPeers *NetworkPeers) {
	for _, listener := range self.networkPeersChangeListeners.Get() {
		connect.HandleError(func() {
			listener.NetworkPeersChanged(networkPeers)
		})
	}
}

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
