package sdk

import (
	"bufio"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/rpc"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	// "runtime/debug"

	"maps"

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

// DeviceRecreatedListener fires when the hosted device is recreated under the
// remote (detected as a change in the device generation across reconnects).
// The client should re-run its device setup (destination, listeners) since the
// new device instance starts from its persisted initial state.
type DeviceRecreatedListener interface {
	DeviceRecreated()
}

type deviceRpcSettings struct {
	RpcCallTimeout      time.Duration
	RpcConnectTimeout   time.Duration
	RpcReconnectTimeout time.Duration
	KeepAliveTimeout    time.Duration
	// max number of keep alive pings after first. Must be at least 1
	KeepAliveRetryCount int
	Address             *DeviceRemoteAddress
	InitialLockTimeout  time.Duration
	// size of the buffered channel that serializes delivery of rpc callbacks.
	// when full, callback delivery blocks as expected back pressure.
	CallbackBufferSize int
	// per-stream buffered frame counts for the rpc transport mux
	MuxSendBufferSize    int
	MuxReceiveBufferSize int
	// MuxMaxFrameBytes is a hard per-websocket-message limit, including the
	// one-byte stream tag. MuxMaxQueuedBytes applies independently to send and
	// receive queues so either direction always retains enough progress capacity
	// for one maximum-sized frame without cross-direction deadlock.
	MuxMaxFrameBytes  int64
	MuxMaxQueuedBytes int64
	// deadline for a single mux frame write before the connection is torn down.
	// decoupled from RpcCallTimeout so the rpc call timeout can be long (back
	// pressure during spin-up) without letting a stuck write hang that long.
	MuxWriteTimeout time.Duration

	// RpcVersion is the device rpc wire version a DeviceRemote puts on its
	// sync request. Always DeviceRpcVersion in production; per-settings rather
	// than a package global only so a test can stand up a remote from an
	// incompatible build without a second binary. The DeviceLocal always
	// enforces against the DeviceRpcVersion const.
	RpcVersion int

	// Maximum concurrent HTTP-over-RPC operations on the app side and in each
	// extension fetch/delivery stage. This bounds in-flight request/response
	// buffers and goroutines when the app is slow or suspended.
	HttpMaxConcurrent int
	// largest request or response body allowed through http-over-rpc. Values
	// <= 0 resolve to the bounded production default.
	HttpMaxBodyBytes int

	// DisableHostedIncompatible, when true, makes the DeviceLocalRpc noop the
	// setters that must never change on a hosted device (route local, provide
	// settings, transport settings, tunnel/vpn, identity/rpc); the corresponding
	// getters and change listeners keep working. Hosted transport is pinned to
	// H1. Set by the platform hosted rpc. This is the rpc layer of the same guard
	// `DeviceLocalSettings.HostedIncompatible` enforces
	// inside DeviceLocal.
	DisableHostedIncompatible bool

	// DeviceGeneration identifies the specific hosted DeviceLocal instance an
	// rpc serves. The host stamps a fresh value each time it (re)creates the
	// device (e.g. after an idle reap or egress-death recreate). The
	// DeviceRemote reads it from the sync response and fires
	// DeviceRecreatedListener when it changes across reconnects, so the client
	// can re-run its setup. Empty on non-hosted (localhost) rpc, where the
	// device is not recreated under the remote.
	DeviceGeneration string

	DeviceLocalSettings
}

// deviceRpcDefaultAddress is the default localhost rpc address, used by both
// the DeviceLocal listener and the DeviceRemote dialer when no explicit
// transport is set. A var so the test harness can point an entire test process
// at a per-process ephemeral port (see TestMain).
var deviceRpcDefaultAddress = "127.0.0.1:12025"

const (
	deviceRpcDefaultMaxFrameBytes    int64 = 3 * 1024 * 1024
	deviceRpcDefaultMaxQueuedBytes   int64 = 4 * 1024 * 1024
	deviceRpcDefaultHttpMaxBodyBytes       = int(connect.DefaultMaxHttpResponseBodyBytes)
)

func (self *deviceRpcSettings) maxFrameBytes() int64 {
	if self.MuxMaxFrameBytes <= 0 {
		return deviceRpcDefaultMaxFrameBytes
	}
	return self.MuxMaxFrameBytes
}

func (self *deviceRpcSettings) maxQueuedBytes() int64 {
	if self.MuxMaxQueuedBytes <= 0 {
		return deviceRpcDefaultMaxQueuedBytes
	}
	return self.MuxMaxQueuedBytes
}

func (self *deviceRpcSettings) httpMaxBodyBytes() int {
	if self.HttpMaxBodyBytes <= 0 {
		return deviceRpcDefaultHttpMaxBodyBytes
	}
	return self.HttpMaxBodyBytes
}

func (self *deviceRpcSettings) httpMaxConcurrent() int {
	if self.HttpMaxConcurrent <= 0 {
		return 2
	}
	return self.HttpMaxConcurrent
}

func defaultDeviceRpcSettings() *deviceRpcSettings {
	return &deviceRpcSettings{
		RpcCallTimeout:    60 * time.Second,
		RpcConnectTimeout: 30 * time.Second,
		// Full-jitter over one second previously averaged two attempts per
		// second but allowed millisecond retry clusters. A fixed half-second
		// pace preserves that work rate while bounding resume detection sooner.
		RpcReconnectTimeout: 500 * time.Millisecond,
		// 5s ping; the read side tears down only after KeepAliveTimeout *
		// (KeepAliveRetryCount+1) = 30s of silence, so normal iOS app suspension
		// (the app process is frozen while backgrounded) does not flap the rpc,
		// while a genuinely dead connection is still reaped.
		KeepAliveTimeout:     5 * time.Second,
		KeepAliveRetryCount:  5,
		Address:              requireRemoteAddress(deviceRpcDefaultAddress),
		InitialLockTimeout:   1 * time.Second,
		CallbackBufferSize:   64,
		MuxSendBufferSize:    32,
		MuxReceiveBufferSize: 32,
		MuxMaxFrameBytes:     deviceRpcDefaultMaxFrameBytes,
		MuxMaxQueuedBytes:    deviceRpcDefaultMaxQueuedBytes,
		MuxWriteTimeout:      15 * time.Second,
		HttpMaxConcurrent:    2,
		HttpMaxBodyBytes:     deviceRpcDefaultHttpMaxBodyBytes,
		RpcVersion:           DeviceRpcVersion,

		DeviceLocalSettings: *DefaultDeviceLocalSettings(),
	}
}

// compile check that DeviceRemote conforms to Device, device, and ViewControllerManager
var _ Device = (*DeviceRemote)(nil)
var _ device = (*DeviceRemote)(nil)
var _ ViewControllerManager = (*DeviceRemote)(nil)

type DeviceRemote struct {
	ctx       context.Context
	cancel    context.CancelFunc
	log       connect.Logger
	closeOnce sync.Once

	networkSpace     *NetworkSpace
	byJwt            string
	apiJwtRefreshSub Sub
	apiAuthLogoutSub Sub

	settings *deviceRpcSettings

	reconnectMonitor *connect.Monitor
	syncMonitor      *connect.Monitor
	// notified when the rpc transport (dialer) is swapped, to drop a live
	// connection and reconnect with the new dialer
	resetMonitor *connect.Monitor

	clientId       connect.Id
	instanceId     connect.Id
	clientStrategy *connect.ClientStrategy

	remoteChangeListeners    *connect.CallbackList[RemoteChangeListener]
	deviceRecreatedListeners *connect.CallbackList[DeviceRecreatedListener]

	// the device generation observed on the last successful sync; a change
	// signals the host recreated the device. Guarded by stateLock.
	deviceGeneration    string
	hasDeviceGeneration bool

	// the error from the last sync attempt that the local REJECTED
	// (DeviceRemoteSyncResponse.Error), empty after a sync succeeds. A
	// rejection is otherwise invisible to the app: the remote just stays
	// unsynced and reconnects paced, which looks the same as a local that is
	// not running. Guarded by stateLock.
	syncError string

	// egressSecurityPolicy *deviceRemoteEgressSecurityPolicy
	// ingressSecurityPolicy *deviceRemote

	stateLock sync.Mutex

	dialer deviceRpcDialer
	// current dialer config, so SetRpcServer is a no-op (no reset of a live
	// connection) when the same transport is re-applied
	rpcHostPort      string
	rpcClientPem     string
	rpcServerCertPem string
	service          *rpcClient

	canShowRatingDialogChangeListeners       map[connect.Id]CanShowRatingDialogChangeListener
	canPromptIntroFunnelChangeListeners      map[connect.Id]CanPromptIntroFunnelChangeListener
	allowForegroundChangeListeners           map[connect.Id]AllowForegroundChangeListener
	canReferChangeListeners                  map[connect.Id]CanReferChangeListener
	provideModeChangeListeners               map[connect.Id]ProvideModeChangeListener
	provideChangeListeners                   map[connect.Id]ProvideChangeListener
	provideControlModeChangeListeners        map[connect.Id]ProvideControlModeChangeListener
	performanceProfileChangeListeners        map[connect.Id]PerformanceProfileChangeListener
	providerIdentityChangeListeners          map[connect.Id]ProviderIdentityChangeListener
	providePausedChangeListeners             map[connect.Id]ProvidePausedChangeListener
	provideNetworkModeChangeListeners        map[connect.Id]ProvideNetworkModeChangeListener
	offlineChangeListeners                   map[connect.Id]OfflineChangeListener
	vpnInterfaceWhileOfflineChangeListeners  map[connect.Id]VpnInterfaceWhileOfflineChangeListener
	connectChangeListeners                   map[connect.Id]ConnectChangeListener
	routeLocalChangeListeners                map[connect.Id]RouteLocalChangeListener
	blockerEnabledChangeListeners            map[connect.Id]BlockerEnabledChangeListener
	connectLocationChangeListeners           map[connect.Id]ConnectLocationChangeListener
	defaultLocationChangeListeners           map[connect.Id]DefaultLocationChangeListener
	provideSecretKeysListeners               map[connect.Id]ProvideSecretKeysListener
	windowMonitors                           map[connect.Id]*deviceRemoteWindowMonitor
	tunnelChangeListeners                    map[connect.Id]TunnelChangeListener
	contractStatusChangeListeners            map[connect.Id]ContractStatusChangeListener
	windowStatusChangeListeners              map[connect.Id]WindowStatusChangeListener
	blockActionWindowChangeListeners         map[connect.Id]BlockActionWindowChangeListener
	blockStatsChangeListeners                map[connect.Id]BlockStatsChangeListener
	blockActionOverridesChangeListeners      map[connect.Id]BlockActionOverridesChangeListener
	transportSettingsChangeListeners         map[connect.Id]TransportSettingsChangeListener
	providerTransportSettingsChangeListeners map[connect.Id]ProviderTransportSettingsChangeListener
	transportStatusChangeListeners           map[connect.Id]TransportStatusChangeListener
	providerTransportStatusChangeListeners   map[connect.Id]ProviderTransportStatusChangeListener
	packetStatsChangeListeners               map[connect.Id]PacketStatsChangeListener
	egressContractStatsChangeListeners       map[connect.Id]ContractStatsChangeListener
	egressContractDetailsChangeListeners     map[connect.Id]ContractDetailsChangeListener
	ingressContractStatsChangeListeners      map[connect.Id]ContractStatsChangeListener
	ingressContractDetailsChangeListeners    map[connect.Id]ContractDetailsChangeListener
	dnsResolverSettingsChangeListeners       map[connect.Id]DnsResolverSettingsChangeListener
	networkPeersChangeListeners              map[connect.Id]NetworkPeersChangeListener

	providerPacketStatsChangeListeners            map[connect.Id]PacketStatsChangeListener
	providerEgressContractStatsChangeListeners    map[connect.Id]ContractStatsChangeListener
	providerEgressContractDetailsChangeListeners  map[connect.Id]ContractDetailsChangeListener
	providerIngressContractStatsChangeListeners   map[connect.Id]ContractStatsChangeListener
	providerIngressContractDetailsChangeListeners map[connect.Id]ContractDetailsChangeListener
	// jwtRefreshListeners               map[connect.Id]JwtRefreshListener
	jwtRefreshListeners *connect.CallbackList[JwtRefreshListener]
	authLogoutListeners *connect.CallbackList[AuthLogoutListener]

	// connectedProviderLocationChangeListeners is a plain local callback list,
	// not an rpc-mirrored listener map: the change signal rides the existing
	// bridged window-monitor events (see providerLocationsMonitor), so no
	// dedicated rpc listener methods exist for it
	connectedProviderLocationChangeListeners *connect.CallbackList[ConnectedProviderLocationChangeListener]

	httpResponseChannels map[connect.Id]chan *DeviceRemoteHttpResponse
	// Admission control is taken before an HTTP request is encoded onto the RPC
	// stream. This bounds the response-channel map and prevents the extension
	// from decoding more requests than it can actively service.
	httpSemOnce sync.Once
	httpSem     chan struct{}

	state DeviceRemoteState
	// last observed values
	lastKnownState DeviceRemoteState

	// last observed post quantum identity values. Read-only data (there are
	// no setters), so these are cached outside the settable
	// `DeviceRemoteState` sync. Guarded by `stateLock`
	lastPublicIdentityKey  []byte
	lastProviderIdentities []*ProviderIdentity

	// providerLocationsMonitor is a lazily created, internally subscribed
	// window monitor: registration is what makes windowMonitorEvents readable,
	// and the subscription fans bridged monitor events out to
	// connectedProviderLocationChangeListeners. Created on first use so an app
	// that never reads provider locations adds no window listener to the sync.
	// Guarded by `providerLocationsMonitorLock` (never take `stateLock` first)
	providerLocationsMonitorLock  sync.Mutex
	providerLocationsMonitor      windowMonitor
	providerLocationsMonitorUnsub func()
	// last readable derivation, retained while the rpc is down (freeze rather
	// than drain). Guarded by `stateLock`
	lastConnectedProviderLocations []*ConnectedProviderLocation

	viewControllerManager
}

// conforms to `device`
func (self *DeviceRemote) logger() connect.Logger {
	return self.log
}

func NewDeviceRemoteWithDefaults(
	networkSpace *NetworkSpace,
	byJwt string,
	instanceId *Id,
) (*DeviceRemote, error) {
	settings := defaultDeviceRpcSettings()
	dialer := NewWebsocketDeviceRpcDialer(settings.Address, "", "", settings)
	return newDeviceRemote(networkSpace, byJwt, instanceId, settings, dialer)
}

// NewPlatformDeviceRemote creates a DeviceRemote that reaches a hosted
// DeviceLocal by connecting directly to the proxy host that runs it, at
// wss://<proxyUrl>/device-rpc. This is the constructor a JS/wasm client uses
// (with the browser websocket dialer). proxyUrl may be a bare host, host:port,
// or ws/wss url; a bare host defaults to wss.
//
// Auth to the device-rpc endpoint is signedProxyId (the device's signed proxy
// id — an HMAC bearer token, model.SignProxyId), not a jwt. byJwt is the
// network member jwt used for the network space api; it also supplies the
// remote's displayed client id.
func NewPlatformDeviceRemote(
	networkSpace *NetworkSpace,
	byJwt string,
	proxyUrl string,
	signedProxyId string,
	instanceId *Id,
) (*DeviceRemote, error) {
	clientId, err := parseByJwtClientId(byJwt)
	if err != nil {
		if err != errByJwtNoClientId {
			return nil, err
		}
		// A NETWORK member jwt — the documented input here — carries no
		// client_id claim. On the platform path the client id is display-only
		// (rpc auth is signedProxyId, device pairing is instanceId), so use
		// the zero id rather than refusing to construct the remote.
		clientId = connect.Id{}
	}
	settings := defaultDeviceRpcSettings()
	// The platform constructor controls a server/proxy-hosted DeviceLocal. Keep
	// hosted-incompatible mutations out of the pending client state as well as
	// rejecting them on the server.
	settings.DisableHostedIncompatible = true
	dialer := NewPlatformDeviceRpcDialer(proxyUrl, signedProxyId, settings)
	return newDeviceRemoteWithOverrides(networkSpace, byJwt, instanceId, settings, clientId, dialer)
}

func newDeviceRemote(
	networkSpace *NetworkSpace,
	byJwt string,
	instanceId *Id,
	settings *deviceRpcSettings,
	dialer deviceRpcDialer,
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
		dialer,
	)
}

func newDeviceRemoteWithOverrides(
	networkSpace *NetworkSpace,
	byJwt string,
	instanceId *Id,
	settings *deviceRpcSettings,
	clientId connect.Id,
	dialer deviceRpcDialer,
) (*DeviceRemote, error) {
	ctx, cancel := context.WithCancel(context.Background())

	api := networkSpace.GetApi()

	deviceRemote := &DeviceRemote{
		ctx:                      ctx,
		cancel:                   cancel,
		networkSpace:             networkSpace,
		byJwt:                    byJwt,
		settings:                 settings,
		log:                      settings.logger(),
		reconnectMonitor:         connect.NewMonitor(),
		syncMonitor:              connect.NewMonitor(),
		resetMonitor:             connect.NewMonitor(),
		clientId:                 clientId,
		instanceId:               instanceId.toConnectId(),
		clientStrategy:           networkSpace.clientStrategy,
		dialer:                   dialer,
		remoteChangeListeners:    connect.NewCallbackList[RemoteChangeListener](),
		deviceRecreatedListeners: connect.NewCallbackList[DeviceRecreatedListener](),

		canShowRatingDialogChangeListeners:       map[connect.Id]CanShowRatingDialogChangeListener{},
		canPromptIntroFunnelChangeListeners:      map[connect.Id]CanPromptIntroFunnelChangeListener{},
		allowForegroundChangeListeners:           map[connect.Id]AllowForegroundChangeListener{},
		canReferChangeListeners:                  map[connect.Id]CanReferChangeListener{},
		provideModeChangeListeners:               map[connect.Id]ProvideModeChangeListener{},
		provideChangeListeners:                   map[connect.Id]ProvideChangeListener{},
		provideControlModeChangeListeners:        map[connect.Id]ProvideControlModeChangeListener{},
		performanceProfileChangeListeners:        map[connect.Id]PerformanceProfileChangeListener{},
		providerIdentityChangeListeners:          map[connect.Id]ProviderIdentityChangeListener{},
		providePausedChangeListeners:             map[connect.Id]ProvidePausedChangeListener{},
		provideNetworkModeChangeListeners:        map[connect.Id]ProvideNetworkModeChangeListener{},
		offlineChangeListeners:                   map[connect.Id]OfflineChangeListener{},
		vpnInterfaceWhileOfflineChangeListeners:  map[connect.Id]VpnInterfaceWhileOfflineChangeListener{},
		connectChangeListeners:                   map[connect.Id]ConnectChangeListener{},
		routeLocalChangeListeners:                map[connect.Id]RouteLocalChangeListener{},
		blockerEnabledChangeListeners:            map[connect.Id]BlockerEnabledChangeListener{},
		connectLocationChangeListeners:           map[connect.Id]ConnectLocationChangeListener{},
		defaultLocationChangeListeners:           map[connect.Id]DefaultLocationChangeListener{},
		provideSecretKeysListeners:               map[connect.Id]ProvideSecretKeysListener{},
		windowMonitors:                           map[connect.Id]*deviceRemoteWindowMonitor{},
		tunnelChangeListeners:                    map[connect.Id]TunnelChangeListener{},
		contractStatusChangeListeners:            map[connect.Id]ContractStatusChangeListener{},
		windowStatusChangeListeners:              map[connect.Id]WindowStatusChangeListener{},
		blockActionWindowChangeListeners:         map[connect.Id]BlockActionWindowChangeListener{},
		blockStatsChangeListeners:                map[connect.Id]BlockStatsChangeListener{},
		blockActionOverridesChangeListeners:      map[connect.Id]BlockActionOverridesChangeListener{},
		transportSettingsChangeListeners:         map[connect.Id]TransportSettingsChangeListener{},
		providerTransportSettingsChangeListeners: map[connect.Id]ProviderTransportSettingsChangeListener{},
		transportStatusChangeListeners:           map[connect.Id]TransportStatusChangeListener{},
		providerTransportStatusChangeListeners:   map[connect.Id]ProviderTransportStatusChangeListener{},
		packetStatsChangeListeners:               map[connect.Id]PacketStatsChangeListener{},
		egressContractStatsChangeListeners:       map[connect.Id]ContractStatsChangeListener{},
		egressContractDetailsChangeListeners:     map[connect.Id]ContractDetailsChangeListener{},
		ingressContractStatsChangeListeners:      map[connect.Id]ContractStatsChangeListener{},
		ingressContractDetailsChangeListeners:    map[connect.Id]ContractDetailsChangeListener{},
		dnsResolverSettingsChangeListeners:       map[connect.Id]DnsResolverSettingsChangeListener{},
		networkPeersChangeListeners:              map[connect.Id]NetworkPeersChangeListener{},
		jwtRefreshListeners:                      connect.NewCallbackList[JwtRefreshListener](),
		connectedProviderLocationChangeListeners: connect.NewCallbackList[ConnectedProviderLocationChangeListener](),
		authLogoutListeners:                      connect.NewCallbackList[AuthLogoutListener](),
		httpResponseChannels:                     map[connect.Id]chan *DeviceRemoteHttpResponse{},

		providerPacketStatsChangeListeners:            map[connect.Id]PacketStatsChangeListener{},
		providerEgressContractStatsChangeListeners:    map[connect.Id]ContractStatsChangeListener{},
		providerEgressContractDetailsChangeListeners:  map[connect.Id]ContractDetailsChangeListener{},
		providerIngressContractStatsChangeListeners:   map[connect.Id]ContractStatsChangeListener{},
		providerIngressContractDetailsChangeListeners: map[connect.Id]ContractDetailsChangeListener{},
	}

	deviceRemote.viewControllerManager = *newViewControllerManager(ctx, deviceRemote)

	var logout func() error
	if networkSpace.asyncLocalState != nil {
		logout = networkSpace.asyncLocalState.localState.Logout
	} else {
		// do nothing
		logout = func() error {
			return nil
		}
	}

	// Install the RPC HTTP functions before enabling the API-owned refresh
	// worker, so its immediate startup validation follows the remote path.
	api.setHttpPostRaw(deviceRemote.httpPostRaw)
	api.setHttpGetRaw(deviceRemote.httpGetRaw)
	api.setLog(deviceRemote.log)
	deviceRemote.apiJwtRefreshSub = api.AddJwtRefreshListener(
		jwtRefreshListenerFunc(deviceRemote.setByJwt),
	)
	deviceRemote.apiAuthLogoutSub = api.AddAuthLogoutListener(
		authLogoutListenerFunc(func() {
			if err := logout(); err != nil {
				deviceRemote.log.Errorf("failed to clear local auth state: %v", err)
			}
			deviceRemote.handleApiAuthLogout()
		}),
	)
	api.SetByJwt(byJwt)
	api.StartJwtRefresh()

	newSecurityPolicyMonitor(ctx, deviceRemote, settings.Verbose)

	// remote starts locked
	// only after the first attempt to connect to the local does it unlock
	deviceRemote.stateLock.Lock()
	go connect.HandleError(deviceRemote.run, cancel)
	return deviceRemote, nil
}

func (self *DeviceRemote) run() {
	// defer func() {
	// 	if r := recover(); r != nil {
	// 		self.log.Errorf("[dr]unrecovered = %s", r)
	// 		debug.PrintStack()
	// 		panic(r)
	// 	}
	// }()

	initialLock := true
	intialLockEndTime := time.Now().Add(self.settings.InitialLockTimeout)
	for {
		// Rate-limit local reconnects: ensure at least RpcReconnectTimeout
		// between attempt starts. Sync and transport replacement still wake the
		// loop immediately, so explicit recovery never waits for the pace.
		syncReconnect := connect.NewPacedReconnect(self.settings.RpcReconnectTimeout)
		handleCtx, handleCancel := context.WithCancel(self.ctx)

		notify := self.reconnectMonitor.NotifyChannel()
		resetNotify := self.resetMonitor.NotifyChannel()
		func() {
			defer handleCancel()

			var reverseConn net.Conn

			synced := false
			deviceRecreated := false
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

				forwardConn, rev, err := self.dialer.Dial(handleCtx)
				if err != nil {
					// failure to connect here is normal if the local is not running
					// self.log.Infof("[dr]sync connect err = %s", err)
					return
				}
				reverseConn = rev

				select {
				case <-handleCtx.Done():
					forwardConn.Close()
					return
				default:
				}

				service := &rpcClientWithTimeout{
					ctx:         self.ctx,
					log:         self.log,
					timeout:     self.settings.RpcCallTimeout,
					closeClient: forwardConn.Close,
					client:      rpc.NewClient(forwardConn),
				}
				// closing the forward rpc client tears down the whole mux,
				// including reverseConn
				defer func() {
					if !synced {
						service.Close()
					}
				}()

				windowMonitorListenerIds := map[connect.Id][]connect.Id{}
				for windowId, windowMonitor := range self.windowMonitors {
					if 0 < len(windowMonitor.listeners) {
						windowMonitorListenerIds[windowId] = slices.Collect(maps.Keys(windowMonitor.listeners))
					}
				}

				syncRequest := &DeviceRemoteSyncRequest{
					InstanceId: self.instanceId,
					RpcVersion: self.settings.RpcVersion,

					CanShowRatingDialogChangeListenerIds:      slices.Collect(maps.Keys(self.canShowRatingDialogChangeListeners)),
					CanPromptIntroFunnelChangeListenerIds:     slices.Collect(maps.Keys(self.canPromptIntroFunnelChangeListeners)),
					AllowForegroundChangeListenerIds:          slices.Collect(maps.Keys(self.allowForegroundChangeListeners)),
					CanReferChangeListenerIds:                 slices.Collect(maps.Keys(self.canReferChangeListeners)),
					ProvideModeChangeListenerIds:              slices.Collect(maps.Keys(self.provideModeChangeListeners)),
					ProvideChangeListenerIds:                  slices.Collect(maps.Keys(self.provideChangeListeners)),
					ProvideControlModeChangeListenerIds:       slices.Collect(maps.Keys(self.provideControlModeChangeListeners)),
					PerformanceProfileChangeListenerIds:       slices.Collect(maps.Keys(self.performanceProfileChangeListeners)),
					ProviderIdentityChangeListenerIds:         slices.Collect(maps.Keys(self.providerIdentityChangeListeners)),
					ProvidePausedChangeListenerIds:            slices.Collect(maps.Keys(self.providePausedChangeListeners)),
					ProvideNetworkModeChangeListenerIds:       slices.Collect(maps.Keys(self.provideNetworkModeChangeListeners)),
					OfflineChangeListenerIds:                  slices.Collect(maps.Keys(self.offlineChangeListeners)),
					VpnInterfaceWhileOfflineChangeListenerIds: slices.Collect(maps.Keys(self.vpnInterfaceWhileOfflineChangeListeners)),
					ConnectChangeListenerIds:                  slices.Collect(maps.Keys(self.connectChangeListeners)),
					RouteLocalChangeListenerIds:               slices.Collect(maps.Keys(self.routeLocalChangeListeners)),
					BlockerEnabledChangeListenerIds:           slices.Collect(maps.Keys(self.blockerEnabledChangeListeners)),
					ConnectLocationChangeListenerIds:          slices.Collect(maps.Keys(self.connectLocationChangeListeners)),
					DefaultLocationChangeListenerIds:          slices.Collect(maps.Keys(self.defaultLocationChangeListeners)),
					ProvideSecretKeysListenerIds:              slices.Collect(maps.Keys(self.provideSecretKeysListeners)),
					TunnelChangeListenerIds:                   slices.Collect(maps.Keys(self.tunnelChangeListeners)),
					ContractStatusChangeListenerIds:           slices.Collect(maps.Keys(self.contractStatusChangeListeners)),
					WindowStatusChangeListenerIds:             slices.Collect(maps.Keys(self.windowStatusChangeListeners)),
					BlockActionWindowChangeListenerIds:        slices.Collect(maps.Keys(self.blockActionWindowChangeListeners)),
					BlockStatsChangeListenerIds:               slices.Collect(maps.Keys(self.blockStatsChangeListeners)),
					BlockActionOverridesChangeListenerIds:     slices.Collect(maps.Keys(self.blockActionOverridesChangeListeners)),
					TransportSettingsChangeListenerIds: append(
						slices.Collect(maps.Keys(self.transportSettingsChangeListeners)),
						slices.Collect(maps.Keys(self.transportStatusChangeListeners))...,
					),
					ProviderTransportSettingsChangeListenerIds: append(
						slices.Collect(maps.Keys(self.providerTransportSettingsChangeListeners)),
						slices.Collect(maps.Keys(self.providerTransportStatusChangeListeners))...,
					),
					PacketStatsChangeListenerIds:            slices.Collect(maps.Keys(self.packetStatsChangeListeners)),
					EgressContractStatsChangeListenerIds:    slices.Collect(maps.Keys(self.egressContractStatsChangeListeners)),
					EgressContractDetailsChangeListenerIds:  slices.Collect(maps.Keys(self.egressContractDetailsChangeListeners)),
					IngressContractStatsChangeListenerIds:   slices.Collect(maps.Keys(self.ingressContractStatsChangeListeners)),
					IngressContractDetailsChangeListenerIds: slices.Collect(maps.Keys(self.ingressContractDetailsChangeListeners)),
					DnsResolverSettingsChangeListenerIds:    slices.Collect(maps.Keys(self.dnsResolverSettingsChangeListeners)),
					NetworkPeersChangeListenerIds:           slices.Collect(maps.Keys(self.networkPeersChangeListeners)),

					ProviderPacketStatsChangeListenerIds:            slices.Collect(maps.Keys(self.providerPacketStatsChangeListeners)),
					ProviderEgressContractStatsChangeListenerIds:    slices.Collect(maps.Keys(self.providerEgressContractStatsChangeListeners)),
					ProviderEgressContractDetailsChangeListenerIds:  slices.Collect(maps.Keys(self.providerEgressContractDetailsChangeListeners)),
					ProviderIngressContractStatsChangeListenerIds:   slices.Collect(maps.Keys(self.providerIngressContractStatsChangeListeners)),
					ProviderIngressContractDetailsChangeListenerIds: slices.Collect(maps.Keys(self.providerIngressContractDetailsChangeListeners)),
					WindowMonitorEventListenerIds:                   windowMonitorListenerIds,
					State:                                           self.state,
				}
				syncResponse, err := rpcCall[*DeviceRemoteSyncResponse](service, "DeviceLocalRpc.Sync", syncRequest, self.closeService)
				if err != nil {
					return
				}

				// stateLock is held for the whole body (taken above, or carried
				// in as initialLock), so syncError is assigned directly here.
				if syncResponse.Error != "" {
					self.log.Infof("Sync error: %s", syncResponse.Error)
					// keep the rejection readable by the app (GetSyncError).
					// The remote stays unsynced and reconnects paced, so
					// without this the only trace of a hard, non-recoverable
					// rejection (wrong device instance, incompatible rpc
					// version) is a log line.
					self.syncError = syncResponse.Error
					return
				}
				self.syncError = ""

				// trim the windows
				// for windowId, windowMonitor := range self.windowMonitors {
				// 	if !syncResponse.WindowIds[windowId] {
				// 		delete(self.windowMonitors, windowId)
				// 		clear(windowMonitor.listeners)
				// 	}
				// }

				self.log.Info("[dr]start device remote rpc")
				deviceRemoteRpc := newDeviceRemoteRpc(handleCtx, self)
				server := rpc.NewServer()
				server.Register(deviceRemoteRpc)

				go connect.HandleError(func() {
					defer func() {
						handleCancel()
						deviceRemoteRpc.Close()
					}()

					// reverseConn is closed on teardown, which unblocks ServeConn
					server.ServeConn(reverseConn)
					self.log.Infof("[dr]sync reverse server done")
				}, func() {
					handleCancel()
					deviceRemoteRpc.Close()
				})

				err = rpcCallNoArgVoid(service, "DeviceLocalRpc.SyncReverse", self.closeService)
				if err != nil {
					return
				}

				// because the local state changes always win,
				// the last known state can be copied from the local state changes
				// note if there were conflict rules, we would need to get the remote state here
				//
				// the reliability settings override is the exception to the
				// post-sync state clear: it is runtime-only state on the local
				// (nothing persists it across an extension restart), so it
				// stays queued after every sync and is re-applied on each
				// reconnect. Without this, an override set while connected
				// would silently vanish on the next extension restart.
				// See `DeviceRemote.SetReliabilitySettings`.
				//
				// only a real override (a non-nil value) is carried: a queued
				// RESET was just delivered by this sync and has no values to
				// re-apply, so it is cleared with the rest of the state. That
				// keeps the queue's nil unambiguous -- it is always "clear the
				// override", never "an all-zero override".
				reliabilitySettings := self.state.ReliabilitySettings
				self.lastKnownState = syncResponse.State
				self.state = DeviceRemoteState{}
				if reliabilitySettings.Value != nil {
					self.state.ReliabilitySettings = reliabilitySettings
				}
				// self.lastKnownState.Merge(&self.state)
				// self.state.Unset()
				self.syncMonitor.NotifyAll()

				self.service = service

				// detect a hosted device recreate: a change in the device
				// generation across syncs means the host built a new device
				// instance, so the client must re-run its setup. The first
				// sync establishes the baseline (not a recreate).
				if self.hasDeviceGeneration && self.deviceGeneration != syncResponse.DeviceGeneration {
					deviceRecreated = true
				}
				self.deviceGeneration = syncResponse.DeviceGeneration
				self.hasDeviceGeneration = true

				if initialLock {
					initialLock = false
					self.stateLock.Unlock()
				}

				synced = true
			}()

			if synced {
				self.remoteChanged(true)
				if deviceRecreated {
					self.deviceRecreated()
				}

				self.log.Infof("[dr]sync done")
				select {
				case <-handleCtx.Done():
				case <-resetNotify:
					// the dialer was swapped; drop this connection and
					// reconnect with the new transport
					self.log.Infof("[dr]rpc transport reset")
				}
				self.log.Infof("[dr]handle done")

				func() {
					self.stateLock.Lock()
					defer self.stateLock.Unlock()

					self.closeService()
					if reverseConn != nil {
						reverseConn.Close()
					}

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
		case <-syncReconnect.After():
		case <-notify:
			// reconnect now
		case <-resetNotify:
			// the dialer was swapped; reconnect now with the new transport
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

func (self *DeviceRemote) setByJwt(byJwt string) {
	self.log.Infof("DeviceLocal JWT refreshed")

	self.GetApi().SetByJwt(byJwt)

	if self.networkSpace.asyncLocalState != nil {
		if err := self.networkSpace.asyncLocalState.localState.setRefreshedByJwt(
			byJwt,
			newId(self.instanceId),
		); err != nil {
			self.log.Errorf("failed to persist refreshed JWT: %v", err)
		}
	}

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		self.byJwt = byJwt
	}()

	// fire listeners
	self.jwtRefreshed(byJwt)

	self.log.Infof("DeviceRemote onTokenRefreshSuccess complete, should have fired listeners")
}

func (self *DeviceRemote) handleApiAuthLogout() {
	self.stateLock.Lock()
	self.byJwt = ""
	self.stateLock.Unlock()
	self.authLogout()
}

func (self *DeviceRemote) RefreshToken(attempt int) error {
	self.log.Infof("DeviceRemote RefreshToken attempt %d", attempt)
	self.GetApi().RequestJwtRefresh()
	return nil
}

func (self *DeviceRemote) SetPerformanceProfile(performanceProfile *PerformanceProfile) {
	performanceProfile = clonePerformanceProfile(performanceProfile)
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if self.state.PerformanceProfile.IsSet {
			if performanceProfileValuesEqual(self.state.PerformanceProfile.Value, performanceProfile) {
				return
			}
		} else if self.lastKnownState.PerformanceProfile.IsSet &&
			performanceProfileValuesEqual(self.lastKnownState.PerformanceProfile.Value, performanceProfile) {
			// Reapplying the same app value after resume must not cross the
			// process boundary. Representation changes still cross once so the
			// DeviceLocal getter/listener contract preserves what the app set;
			// DeviceLocal suppresses transport work when behavior is equivalent.
			return
		}

		success := func() bool {
			if self.service == nil {
				return false
			}

			devicePerformanceProfile := &DevicePerformanceProfile{
				PerformanceProfile: performanceProfile,
			}
			err := rpcCallVoid(self.service, "DeviceLocalRpc.SetPerformanceProfile", devicePerformanceProfile, self.closeService)
			if err != nil {
				return false
			}
			return true
		}()
		if success {
			self.state.PerformanceProfile.Unset()
			self.lastKnownState.PerformanceProfile.Set(performanceProfile)
		} else {
			self.state.PerformanceProfile.Set(performanceProfile)
		}
	}()
}

func (self *DeviceRemote) GetPerformanceProfile() *PerformanceProfile {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	performanceProfile, success := func() (*PerformanceProfile, bool) {
		if self.service == nil {
			return nil, false
		}

		devicePerformanceProfile, err := rpcCallNoArg[*DevicePerformanceProfile](self.service, "DeviceLocalRpc.GetPerformanceProfile", self.closeService)
		if err != nil {
			return nil, false
		}
		performanceProfile := devicePerformanceProfile.PerformanceProfile
		self.lastKnownState.PerformanceProfile.Set(clonePerformanceProfile(performanceProfile))
		return clonePerformanceProfile(performanceProfile), true
	}()
	if success {
		return performanceProfile
	} else {
		performanceProfile := self.state.PerformanceProfile.Get(
			self.lastKnownState.PerformanceProfile.Get(nil),
		)
		return clonePerformanceProfile(performanceProfile)
	}
}

// post quantum identity

func (self *DeviceRemote) GetPublicIdentityKey() []byte {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	publicIdentityKey, success := func() ([]byte, bool) {
		if self.service == nil {
			return nil, false
		}

		devicePublicIdentityKey, err := rpcCallNoArg[*DevicePublicIdentityKey](self.service, "DeviceLocalRpc.GetPublicIdentityKey", self.closeService)
		if err != nil {
			return nil, false
		}
		publicIdentityKey := devicePublicIdentityKey.PublicKey
		self.lastPublicIdentityKey = publicIdentityKey
		return publicIdentityKey, true
	}()
	if success {
		return publicIdentityKey
	} else {
		// retain last-known state on disconnect, consistent with the other
		// getters (the identity key is long-lived). nil when never observed
		return self.lastPublicIdentityKey
	}
}

func (self *DeviceRemote) GetPublicIdentityKeyHash() string {
	publicIdentityKey := self.GetPublicIdentityKey()
	if len(publicIdentityKey) == 0 {
		return ""
	}
	return PublicIdentityKeyHash(publicIdentityKey)
}

func (self *DeviceRemote) GetProviderIdentities() *ProviderIdentityList {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	providerIdentities, success := func() (*ProviderIdentityList, bool) {
		if self.service == nil {
			return nil, false
		}

		deviceProviderIdentities, err := rpcCallNoArg[*DeviceProviderIdentities](self.service, "DeviceLocalRpc.GetProviderIdentities", self.closeService)
		if err != nil {
			return nil, false
		}
		providerIdentities := deviceProviderIdentities.toProviderIdentityList()
		self.lastProviderIdentities = providerIdentities.getAll()
		return providerIdentities, true
	}()
	if success {
		return providerIdentities
	} else {
		// retain last-known state on disconnect, consistent with the other
		// getters. empty (never nil) when never observed
		lastProviderIdentities := NewProviderIdentityList()
		lastProviderIdentities.addAll(self.lastProviderIdentities...)
		return lastProviderIdentities
	}
}

// force a connect attempt as soon as possible
// note this just speeds up connection but is not required,
// since the rpc connect will poll until connected
func (self *DeviceRemote) Sync() {
	self.reconnectMonitor.NotifyAll()
}

// SetRpcServer points the rpc transport at hostPort (e.g. "127.0.0.1:12042"),
// pinning the server certificate in serverCertPem and presenting clientPem
// (cert+key) as the client identity for mTLS. An empty serverCertPem uses an
// unencrypted connection. A live connection is dropped and reconnected with the
// new transport. Apps call this per vpn session with fresh key material.
func (self *DeviceRemote) SetRpcServer(clientPem string, serverCertPem string, hostPort string) error {
	address, err := parseDeviceRemoteAddress(hostPort)
	if err != nil {
		return err
	}

	changed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		// idempotent: if the transport config is unchanged, do not swap the
		// dialer or reset a live connection. re-applying the same server (e.g.
		// on every state change) must not tear down and resync the session.
		if self.dialer != nil &&
			self.rpcHostPort == hostPort &&
			self.rpcClientPem == clientPem &&
			self.rpcServerCertPem == serverCertPem {
			return false
		}

		self.dialer = NewWebsocketDeviceRpcDialer(address, clientPem, serverCertPem, self.settings)
		self.rpcHostPort = hostPort
		self.rpcClientPem = clientPem
		self.rpcServerCertPem = serverCertPem
		return true
	}()

	if changed {
		self.log.Infof("[dr]set rpc server %s (mtls=%t)", address.HostPort(), len(clientPem) != 0)
		self.resetMonitor.NotifyAll()
	}
	return nil
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
	if success {
		self.state.TunnelStarted.Unset()
		self.lastKnownState.TunnelStarted.Set(tunnelStarted)
	} else {
		self.state.TunnelStarted.Set(tunnelStarted)
	}
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
		// retain last-known state on disconnect, consistent with the other
		// getters (GetContractStatus, GetWindowStatus, ...).
		return self.state.TunnelStarted.Get(
			self.lastKnownState.TunnelStarted.Get(self.settings.DefaultTunnelStarted),
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

/**
 * Ratings dialog
 */

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
			self.lastKnownState.CanShowRatingDialog.Get(self.settings.DefaultCanShowRatingDialog),
		)
	}
}

func (self *DeviceRemote) SetCanShowRatingDialog(canShowRatingDialog bool) {
	event := false
	func() {
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
		if success {
			self.state.CanShowRatingDialog.Unset()
			self.lastKnownState.CanShowRatingDialog.Set(canShowRatingDialog)
		} else {
			event = true
			self.state.CanShowRatingDialog.Set(canShowRatingDialog)
		}
	}()
	if event {
		self.canShowRatingDialogChanged(self.GetCanShowRatingDialog())
	}
}

/**
 * Intro funnel prompt
 */

func (self *DeviceRemote) GetCanPromptIntroFunnel() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	canPromptIntroFunnel, success := func() (bool, bool) {
		if self.service == nil {
			return false, false
		}

		canPromptIntroFunnel, err := rpcCallNoArg[bool](self.service, "DeviceLocalRpc.GetCanPromptIntroFunnel", self.closeService)
		if err != nil {
			return false, false
		}
		self.lastKnownState.CanPromptIntroFunnel.Set(canPromptIntroFunnel)
		return canPromptIntroFunnel, true
	}()
	if success {
		return canPromptIntroFunnel
	} else {
		return self.state.CanPromptIntroFunnel.Get(
			self.lastKnownState.CanPromptIntroFunnel.Get(self.settings.DefaultCanShowIntroFunnel),
		)
	}
}

func (self *DeviceRemote) SetCanPromptIntroFunnel(canPromptIntroFunnel bool) {
	event := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		success := func() bool {
			if self.service == nil {
				return false
			}

			err := rpcCallVoid(self.service, "DeviceLocalRpc.SetCanPromptIntroFunnel", canPromptIntroFunnel, self.closeService)
			if err != nil {
				return false
			}
			return true
		}()
		if success {
			self.state.CanPromptIntroFunnel.Unset()
			self.lastKnownState.CanPromptIntroFunnel.Set(canPromptIntroFunnel)
		} else {
			event = true
			self.state.CanPromptIntroFunnel.Set(canPromptIntroFunnel)
		}
	}()
	if event {
		self.canPromptIntroFunnelChanged(self.GetCanPromptIntroFunnel())
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
			self.lastKnownState.CanRefer.Get(self.settings.DefaultCanRefer),
		)
	}
}

func (self *DeviceRemote) SetCanRefer(canRefer bool) {
	event := false
	func() {
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
		if success {
			self.state.CanRefer.Unset()
			self.lastKnownState.CanRefer.Set(canRefer)
		} else {
			event = true
			self.state.CanRefer.Set(canRefer)
		}
	}()
	if event {
		self.canReferChanged(self.GetCanRefer())
	}
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
		self.lastKnownState.AllowForeground.Set(allowForeground)
		return allowForeground, true
	}()
	if success {
		return allowForeground
	} else {
		return self.state.AllowForeground.Get(
			self.lastKnownState.AllowForeground.Get(self.settings.DefaultAllowForeground),
		)
	}
}

func (self *DeviceRemote) SetAllowForeground(allowForeground bool) {
	event := false
	func() {
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
		if success {
			self.state.AllowForeground.Unset()
			self.lastKnownState.AllowForeground.Set(allowForeground)
		} else {
			event = true
			self.state.AllowForeground.Set(allowForeground)
		}
	}()
	if event {
		self.allowForegroundChanged(self.GetAllowForeground())
	}
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
		if success {
			self.state.RouteLocal.Unset()
			self.lastKnownState.RouteLocal.Set(routeLocal)
		} else {
			event = true
			self.state.RouteLocal.Set(routeLocal)
		}
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
			self.lastKnownState.RouteLocal.Get(self.settings.DefaultRouteLocal),
		)
	}
}

func (self *DeviceRemote) SetBlockerEnabled(blockerEnabled bool) {
	event := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		success := func() bool {
			if self.service == nil {
				return false
			}

			err := rpcCallVoid(self.service, "DeviceLocalRpc.SetBlockerEnabled", blockerEnabled, self.closeService)
			if err != nil {
				return false
			}
			return true
		}()
		if success {
			self.state.BlockerEnabled.Unset()
			self.lastKnownState.BlockerEnabled.Set(blockerEnabled)
		} else {
			event = true
			self.state.BlockerEnabled.Set(blockerEnabled)
		}
	}()
	if event {
		self.blockerEnabledChanged(self.GetBlockerEnabled())
	}
}

func (self *DeviceRemote) GetBlockerEnabled() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	blockerEnabled, success := func() (bool, bool) {
		if self.service == nil {
			return false, false
		}

		blockerEnabled, err := rpcCallNoArg[bool](self.service, "DeviceLocalRpc.GetBlockerEnabled", self.closeService)
		if err != nil {
			return false, false
		}
		self.lastKnownState.BlockerEnabled.Set(blockerEnabled)
		return blockerEnabled, true
	}()
	if success {
		return blockerEnabled
	} else {
		return self.state.BlockerEnabled.Get(
			self.lastKnownState.BlockerEnabled.Get(self.settings.DefaultBlockerEnabled),
		)
	}
}

type listenerRpcCall func(*rpcClient, string, any, func()) error

func addListenerWithRpcCall[T any](
	deviceRemote *DeviceRemote,
	listener T,
	listeners map[connect.Id]T,
	addServiceFunc string,
	removeServiceFunc string,
	rpcCall listenerRpcCall,
) Sub {
	deviceRemote.stateLock.Lock()
	defer deviceRemote.stateLock.Unlock()

	listenerId := connect.NewId()
	listeners[listenerId] = listener
	if deviceRemote.service != nil {
		rpcCall(deviceRemote.service, addServiceFunc, listenerId, deviceRemote.closeService)
	}

	return newSub(func() {
		deviceRemote.stateLock.Lock()
		defer deviceRemote.stateLock.Unlock()

		delete(listeners, listenerId)
		if deviceRemote.service != nil {
			rpcCall(deviceRemote.service, removeServiceFunc, listenerId, deviceRemote.closeService)
		}
	})
}

func addListener[T any](
	deviceRemote *DeviceRemote,
	listener T,
	listeners map[connect.Id]T,
	addServiceFunc string,
	removeServiceFunc string,
) Sub {
	return addListenerWithRpcCall(
		deviceRemote,
		listener,
		listeners,
		addServiceFunc,
		removeServiceFunc,
		rpcCallVoid,
	)
}

// addCompatibleListener is for a listener added to an otherwise compatible
// RPC version. A newly updated app can still be attached to the old extension
// process during a rolling install; an unknown listener method must degrade to
// getter-only state until that process restarts, not tear down a healthy RPC
// session. Listener IDs are also carried by Sync, so a current peer retains
// the ordinary reconnect and level-replay behavior.
func addCompatibleListener[T any](
	deviceRemote *DeviceRemote,
	listener T,
	listeners map[connect.Id]T,
	addServiceFunc string,
	removeServiceFunc string,
) Sub {
	return addListenerWithRpcCall(
		deviceRemote,
		listener,
		listeners,
		addServiceFunc,
		removeServiceFunc,
		rpcCallVoidAllowMissingMethod,
	)
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

func (self *DeviceRemote) AddCanShowRatingDialogChangeListener(listener CanShowRatingDialogChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.canShowRatingDialogChangeListeners,
		"DeviceLocalRpc.AddCanShowRatingDialogChangeListener",
		"DeviceLocalRpc.RemoveCanShowRatingDialogChangeListener",
	)
}

func (self *DeviceRemote) AddCanPromptIntroFunnelChangeListener(listener CanPromptIntroFunnelChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.canPromptIntroFunnelChangeListeners,
		"DeviceLocalRpc.AddCanPromptIntroFunnelChangeListener",
		"DeviceLocalRpc.RemoveCanPromptIntroFunnelChangeListener",
	)
}

func (self *DeviceRemote) AddAllowForegroundChangeListener(listener AllowForegroundChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.allowForegroundChangeListeners,
		"DeviceLocalRpc.AddAllowForegroundChangeListener",
		"DeviceLocalRpc.RemoveAllowForegroundChangeListener",
	)
}

func (self *DeviceRemote) AddCanReferChangeListener(listener CanReferChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.canReferChangeListeners,
		"DeviceLocalRpc.AddCanReferChangeListener",
		"DeviceLocalRpc.RemoveCanReferChangeListener",
	)
}

func (self *DeviceRemote) AddProvideModeChangeListener(listener ProvideModeChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.provideModeChangeListeners,
		"DeviceLocalRpc.AddProvideModeChangeListener",
		"DeviceLocalRpc.RemoveProvideModeChangeListener",
	)
}

func (self *DeviceRemote) AddProvideControlModeChangeListener(listener ProvideControlModeChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.provideControlModeChangeListeners,
		"DeviceLocalRpc.AddProvideControlModeChangeListener",
		"DeviceLocalRpc.RemoveProvideControlModeChangeListener",
	)
}

func (self *DeviceRemote) AddPerformanceProfileChangeListener(listener PerformanceProfileChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.performanceProfileChangeListeners,
		"DeviceLocalRpc.AddPerformanceProfileChangeListener",
		"DeviceLocalRpc.RemovePerformanceProfileChangeListener",
	)
}

func (self *DeviceRemote) AddProviderIdentityChangeListener(listener ProviderIdentityChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.providerIdentityChangeListeners,
		"DeviceLocalRpc.AddProviderIdentityChangeListener",
		"DeviceLocalRpc.RemoveProviderIdentityChangeListener",
	)
}

// ensureConnectedProviderLocationsMonitor lazily creates and subscribes the
// internal window monitor. The subscription both registers the window (making
// its events readable over rpc) and drives the local change listeners from
// the bridged monitor events.
func (self *DeviceRemote) ensureConnectedProviderLocationsMonitor() windowMonitor {
	self.providerLocationsMonitorLock.Lock()
	defer self.providerLocationsMonitorLock.Unlock()

	if self.providerLocationsMonitor == nil {
		monitor := self.windowMonitor()
		// AddMonitorEventCallback takes stateLock internally; the lock order
		// providerLocationsMonitorLock -> stateLock is fixed
		unsub := monitor.AddMonitorEventCallback(func(
			windowExpandEvent *connect.WindowExpandEvent,
			providerEvents map[connect.Id]*connect.ProviderEvent,
			reset bool,
		) {
			// expand-only events (nil providerEvents) cannot change the
			// connected provider set
			if reset || 0 < len(providerEvents) {
				self.connectedProviderLocationsChanged()
			}
		})
		self.providerLocationsMonitor = monitor
		self.providerLocationsMonitorUnsub = unsub
	}
	return self.providerLocationsMonitor
}

// GetConnectedProviderLocations returns the currently connected
// (routing-eligible) window providers with their locations, sorted
// oldest-connected first. While the rpc is down, the last readable list is
// retained (freeze rather than drain); `GetRemoteConnected` is the
// availability signal. Empty (never nil) when never observed
func (self *DeviceRemote) GetConnectedProviderLocations() *ConnectedProviderLocationList {
	monitor := self.ensureConnectedProviderLocationsMonitor()

	var providerEvents map[connect.Id]*connect.ProviderEvent
	available := true
	if m, ok := monitor.(windowMonitorWithAvailability); ok {
		_, providerEvents, available = m.EventsWithAvailability()
	} else {
		_, providerEvents = monitor.Events()
	}

	if available {
		connectedProviderLocations := deriveConnectedProviderLocations(providerEvents)
		self.stateLock.Lock()
		self.lastConnectedProviderLocations = connectedProviderLocations.getAll()
		self.stateLock.Unlock()
		return connectedProviderLocations
	}

	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	lastConnectedProviderLocations := NewConnectedProviderLocationList()
	lastConnectedProviderLocations.addAll(self.lastConnectedProviderLocations...)
	return lastConnectedProviderLocations
}

func (self *DeviceRemote) AddConnectedProviderLocationChangeListener(listener ConnectedProviderLocationChangeListener) Sub {
	self.ensureConnectedProviderLocationsMonitor()
	callbackId := self.connectedProviderLocationChangeListeners.Add(listener)
	return newSub(func() {
		self.connectedProviderLocationChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceRemote) connectedProviderLocationsChanged() {
	for _, listener := range self.connectedProviderLocationChangeListeners.Get() {
		connect.HandleError(func() {
			listener.ConnectedProviderLocationsChanged()
		})
	}
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

func (self *DeviceRemote) AddVpnInterfaceWhileOfflineChangeListener(listener VpnInterfaceWhileOfflineChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.vpnInterfaceWhileOfflineChangeListeners,
		"DeviceLocalRpc.AddVpnInterfaceWhileOfflineChangeListener",
		"DeviceLocalRpc.RemoveVpnInterfaceWhileOfflineChangeListener",
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

func (self *DeviceRemote) AddBlockerEnabledChangeListener(listener BlockerEnabledChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.blockerEnabledChangeListeners,
		"DeviceLocalRpc.AddBlockerEnabledChangeListener",
		"DeviceLocalRpc.RemoveBlockerEnabledChangeListener",
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

func (self *DeviceRemote) AddDefaultLocationChangeListener(listener DefaultLocationChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.defaultLocationChangeListeners,
		"DeviceLocalRpc.AddDefaultLocationChangeListener",
		"DeviceLocalRpc.RemoveDefaultLocationChangeListener",
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

func (self *DeviceRemote) AddJwtRefreshListener(listener JwtRefreshListener) Sub {
	callbackId := self.jwtRefreshListeners.Add(listener)
	return newSub(func() {
		self.jwtRefreshListeners.Remove(callbackId)
	})
}

func (self *DeviceRemote) AddAuthLogoutListener(listener AuthLogoutListener) Sub {
	callbackId := self.authLogoutListeners.Add(listener)
	return newSub(func() {
		self.authLogoutListeners.Remove(callbackId)
	})
}

func (self *DeviceRemote) LoadProvideSecretKeys(provideSecretKeyList *ProvideSecretKeyList) {
	event := false
	func() {
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
		if success {
			self.state.LoadProvideSecretKeys.Unset()
			self.state.InitProvideSecretKeys.Unset()
			self.lastKnownState.LoadProvideSecretKeys.Set(provideSecretKeyList.getAll())
			self.lastKnownState.InitProvideSecretKeys.Unset()
		} else {
			self.state.LoadProvideSecretKeys.Set(provideSecretKeyList.getAll())
			self.state.InitProvideSecretKeys.Unset()
			event = true
		}
	}()
	if event {
		// the rpc is not connected, so nothing else will announce the loaded
		// keys — fire the local listeners with them (the ui's
		// has-network-provide-key signal depends on this). When the rpc IS
		// connected, DeviceLocal fires and the reverse push delivers instead.
		self.provideSecretKeysChanged(provideSecretKeyList.getAll())
	}
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
	if success {
		self.state.InitProvideSecretKeys.Unset()
		self.state.LoadProvideSecretKeys.Unset()
		self.lastKnownState.InitProvideSecretKeys.Set(true)
		self.lastKnownState.LoadProvideSecretKeys.Unset()
	} else {
		self.state.InitProvideSecretKeys.Set(true)
		self.state.LoadProvideSecretKeys.Unset()
	}
}

/**
 * Provide Control Mode
 */
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
			self.lastKnownState.ProvideControlMode.Get(self.settings.DefaultProvideControlMode),
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
		if success {
			self.state.ProvideControlMode.Unset()
			self.lastKnownState.ProvideControlMode.Set(mode)
		} else {
			event = true
			self.state.ProvideControlMode.Set(mode)
		}
	}()
	if event {
		self.provideControlModeChanged(self.GetProvideControlMode())
		self.provideModeChanged(self.GetProvideMode())
		self.provideChanged(self.GetProvideEnabled())
	}
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
		provideControlMode := self.state.ProvideControlMode.Get(
			self.lastKnownState.ProvideControlMode.Get(self.settings.DefaultProvideControlMode),
		)
		switch provideControlMode {
		case ProvideControlModeNever:
			return false
		case ProvideControlModeAlways:
			return true
		case ProvideControlModeNetwork:
			// the private provider is always on (peers only)
			return true
		case ProvideControlModeAuto:
			// auto always provides: public while connected, network
			// (peer-reachable) while idle — the provider exists either way
			return true
		case ProvideControlModeManual:
			// manual: a queued explicit provide mode is the truth — the
			// provider exists for any mode but none (per-case; bit set)
			if self.state.ProvideMode.IsSet {
				switch self.state.ProvideMode.Value {
				case ProvideModeNetwork, ProvideModeFriendsAndFamily, ProvideModePublic:
					return true
				default:
					return false
				}
			}
			return self.lastKnownState.ProvideEnabled.Get(false)
		default:
			return false
		}
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
			return self.state.Location.Value != nil &&
				self.state.Location.Value.ConnectLocation != nil
		} else if self.state.Destination.IsSet {
			// SetDestination supports a spec-only custom destination. Its
			// display location may legitimately be nil; the spec list is the
			// installed connection command.
			return self.state.Destination.Value != nil &&
				0 < len(self.state.Destination.Value.Specs)
		} else if self.state.RemoveDestination.IsSet {
			return false
		} else {
			return self.lastKnownState.ConnectEnabled.Get(false)
		}
	}
}

func (self *DeviceRemote) SetProvideMode(provideMode ProvideMode) {
	event := false
	func() {
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
		if success {
			self.state.ProvideMode.Unset()
			self.lastKnownState.ProvideMode.Set(provideMode)
		} else {
			event = true
			self.state.ProvideMode.Set(provideMode)
		}
	}()
	if event {
		self.provideModeChanged(self.GetProvideMode())
		self.provideChanged(self.GetProvideEnabled())
	}
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
		// derive the EFFECTIVE mode from the queued/last-known control mode,
		// mirroring DeviceLocal's control-mode mapping (and GetProvideEnabled
		// below): the raw queued provide mode can be stale relative to a
		// queued control mode (e.g. a persisted mode applied at init), which
		// left the ui reading the wrong mode until the rpc connected.
		// ProvideMode is a bit set: compare per-case, never with ranges.
		provideControlMode := self.state.ProvideControlMode.Get(
			self.lastKnownState.ProvideControlMode.Get(self.settings.DefaultProvideControlMode),
		)
		switch provideControlMode {
		case ProvideControlModeNever:
			return ProvideModeNone
		case ProvideControlModeAlways:
			return ProvideModePublic
		case ProvideControlModeNetwork:
			// the private provider is always on (peers only)
			return ProvideModeNetwork
		case ProvideControlModeAuto:
			// public while connected; network (peer-reachable) while idle
			connected := false
			if self.state.Location.IsSet {
				connected = self.state.Location.Value.ConnectLocation != nil
			} else if self.state.Destination.IsSet {
				connected = self.state.Destination.Value.Location.ConnectLocation != nil
			} else if self.state.RemoveDestination.IsSet {
				connected = false
			} else {
				connected = self.lastKnownState.ConnectEnabled.Get(false)
			}
			if connected {
				return ProvideModePublic
			}
			return ProvideModeNetwork
		default:
			// manual: the explicitly set mode is the truth
			return self.state.ProvideMode.Get(
				self.lastKnownState.ProvideMode.Get(ProvideModeNone),
			)
		}
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
		if success {
			self.state.ProvidePaused.Unset()
			self.lastKnownState.ProvidePaused.Set(providePaused)
		} else {
			event = true
			self.state.ProvidePaused.Set(providePaused)
		}
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
		if success {
			self.state.ProvideNetworkMode.Unset()
			self.lastKnownState.ProvideNetworkMode.Set(provideNetworkMode)
		} else {
			event = true
			self.state.ProvideNetworkMode.Set(provideNetworkMode)
		}
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
		if success {
			self.state.Offline.Unset()
			self.lastKnownState.Offline.Set(offline)
		} else {
			event = true
			self.state.Offline.Set(offline)
		}
	}()
	if event {
		self.vpnInterfaceWhileOfflineChanged(self.GetVpnInterfaceWhileOffline())
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
			self.lastKnownState.Offline.Get(self.settings.DefaultOffline),
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
		if success {
			self.state.VpnInterfaceWhileOffline.Unset()
			self.lastKnownState.VpnInterfaceWhileOffline.Set(vpnInterfaceWhileOffline)
		} else {
			event = true
			self.state.VpnInterfaceWhileOffline.Set(vpnInterfaceWhileOffline)
		}
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
			self.lastKnownState.VpnInterfaceWhileOffline.Get(self.settings.DefaultVpnInterfaceWhileOffline),
		)
	}
}

func (self *DeviceRemote) RemoveDestination() {
	event := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if self.state.RemoveDestination.IsSet ||
			(!self.state.Destination.IsSet &&
				!self.state.Location.IsSet &&
				self.lastKnownState.RemoveDestination.IsSet) {
			return
		}

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
		if success {
			self.state.RemoveDestination.Unset()
			self.state.Destination.Unset()
			self.state.Location.Unset()
			self.lastKnownState.RemoveDestination.Set(true)
			self.lastKnownState.Destination.Unset()
			self.lastKnownState.Location.Unset()
		} else {
			event = true
			self.state.RemoveDestination.Set(true)
			self.state.Destination.Unset()
			self.state.Location.Unset()
		}
	}()
	if event {
		self.connectLocationChanged(newDeviceRemoteConnectLocation(self.GetConnectLocation()))
		self.connectChanged(self.GetConnectEnabled())
		self.provideChanged(self.GetProvideEnabled())
	}
}

func (self *DeviceRemote) SetDestination(location *ConnectLocation, specs *ProviderSpecList) {
	var providerSpecs []*ProviderSpec
	if specs != nil {
		providerSpecs = cloneProviderSpecs(specs.getAll())
	}
	destination := &DeviceRemoteDestination{
		Location: newDeviceRemoteConnectLocation(location),
		Specs:    providerSpecs,
	}
	event := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if self.state.Destination.IsSet &&
			deviceRemoteDestinationsEqual(self.state.Destination.Value, destination) {
			return
		}
		if !self.state.Destination.IsSet &&
			!self.state.Location.IsSet &&
			!self.state.RemoveDestination.IsSet &&
			self.lastKnownState.Destination.IsSet &&
			deviceRemoteDestinationsEqual(self.lastKnownState.Destination.Value, destination) {
			return
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
		if success {
			self.state.Destination.Unset()
			self.state.RemoveDestination.Unset()
			self.state.Location.Unset()
			self.lastKnownState.Destination.Set(destination)
			self.lastKnownState.RemoveDestination.Unset()
			self.lastKnownState.Location.Unset()
		} else {
			event = true
			self.state.Destination.Set(destination)
			self.state.RemoveDestination.Unset()
			self.state.Location.Unset()
		}
	}()
	if event {
		self.connectLocationChanged(newDeviceRemoteConnectLocation(self.GetConnectLocation()))
		self.connectChanged(self.GetConnectEnabled())
		self.provideChanged(self.GetProvideEnabled())
	}
}

func deviceRemoteDestinationsEqual(a *DeviceRemoteDestination, b *DeviceRemoteDestination) bool {
	if a == nil || b == nil {
		return a == b
	}
	var aLocation *ConnectLocation
	if a.Location != nil {
		aLocation = a.Location.toConnectLocation()
	}
	var bLocation *ConnectLocation
	if b.Location != nil {
		bLocation = b.Location.toConnectLocation()
	}
	return connectLocationValuesEqual(aLocation, bLocation) &&
		sdkProviderSpecsFingerprint(a.Specs) == sdkProviderSpecsFingerprint(b.Specs)
}

// Reconnect is `SetConnectLocation` for an explicit user action: it rebuilds
// the connection even when `location` is already the installed destination.
// See the `Device` interface.
//
// It suppresses nothing — suppressing is the whole thing being asked for here —
// and falls back to the same pending state as `SetConnectLocation` when the rpc
// is down: with no reachable device there is no live connection to rebuild, so
// installing the location on the next sync is the right landing.
func (self *DeviceRemote) Reconnect(location *ConnectLocation) {
	self.setConnectLocation(location, true)
}

func (self *DeviceRemote) SetConnectLocation(location *ConnectLocation) {
	self.setConnectLocation(location, false)
}

func (self *DeviceRemote) setConnectLocation(location *ConnectLocation, rebuild bool) {
	deviceRemoteLocation := newDeviceRemoteConnectLocation(location)
	rpcMethod := "DeviceLocalRpc.SetConnectLocation"
	if rebuild {
		rpcMethod = "DeviceLocalRpc.Reconnect"
	}
	event := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		// Only a pending SetConnectLocation carries enough provenance to
		// suppress here. A last-known Location may have come from a custom
		// SetDestination whose spec list differs despite the same display
		// location; DeviceLocal performs the authoritative installed-
		// destination fingerprint check.
		if !rebuild &&
			self.state.Location.IsSet &&
			self.state.Location.Value != nil &&
			connectLocationValuesEqual(self.state.Location.Value.toConnectLocation(), location) {
			return
		}

		success := func() bool {
			if self.service == nil {
				return false
			}

			err := rpcCallVoid(self.service, rpcMethod, deviceRemoteLocation, self.closeService)
			if err != nil {
				return false
			}
			return true
		}()
		if success {
			self.state.Location.Unset()
			self.state.RemoveDestination.Unset()
			self.state.Destination.Unset()
			self.lastKnownState.Location.Set(deviceRemoteLocation)
			self.lastKnownState.RemoveDestination.Unset()
			self.lastKnownState.Destination.Unset()
		} else {
			event = true
			self.state.Location.Set(deviceRemoteLocation)
			self.state.RemoveDestination.Unset()
			self.state.Destination.Unset()
		}
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
		self.lastKnownState.Destination.Unset()
		self.lastKnownState.RemoveDestination.Unset()
		return deviceRemoteLocation.toConnectLocation(), true
	}()
	if success {
		return location
	} else {
		if location, known := deviceRemoteStateConnectLocation(&self.state); known {
			return location
		}
		if location, known := deviceRemoteStateConnectLocation(&self.lastKnownState); known {
			return location
		}
		return nil
	}
}

func deviceRemoteStateConnectLocation(state *DeviceRemoteState) (*ConnectLocation, bool) {
	if state.RemoveDestination.IsSet {
		return nil, true
	}
	if state.Location.IsSet {
		if state.Location.Value == nil {
			return nil, true
		}
		return state.Location.Value.toConnectLocation(), true
	}
	if state.Destination.IsSet {
		if state.Destination.Value == nil || state.Destination.Value.Location == nil {
			return nil, true
		}
		return state.Destination.Value.Location.toConnectLocation(), true
	}
	return nil, false
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
		self.lastKnownState.DefaultLocation.Set(defaultLocation)
		return defaultLocation.toConnectLocation(), true
	}()
	if success {
		return defaultLocation
	} else {
		if self.state.DefaultLocation.IsSet {
			if self.state.DefaultLocation.Value == nil {
				return nil
			}
			return self.state.DefaultLocation.Value.toConnectLocation()
		}
		if self.lastKnownState.DefaultLocation.IsSet {
			if self.lastKnownState.DefaultLocation.Value == nil {
				return nil
			}
			return self.lastKnownState.DefaultLocation.Value.toConnectLocation()
		}
		self.log.Infof("No default location set, returning nil")
		return nil
	}
}

func (self *DeviceRemote) SetDefaultLocation(connectLocation *ConnectLocation) {

	deviceRemoteLocation := newDeviceRemoteDefaultLocation(connectLocation)
	event := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if self.state.DefaultLocation.IsSet {
			if self.state.DefaultLocation.Value != nil {
				current := self.state.DefaultLocation.Value.toConnectLocation()
				if connectLocationValuesEqual(current, connectLocation) {
					return
				}
			}
		} else if self.lastKnownState.DefaultLocation.IsSet &&
			self.lastKnownState.DefaultLocation.Value != nil {
			current := self.lastKnownState.DefaultLocation.Value.toConnectLocation()
			if connectLocationValuesEqual(current, connectLocation) {
				return
			}
		}

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

		if success {
			self.state.DefaultLocation.Unset()
			self.lastKnownState.DefaultLocation.Set(deviceRemoteLocation)
		} else {
			event = true
			self.state.DefaultLocation.Set(deviceRemoteLocation)
		}
	}()
	if event {
		self.defaultLocationChanged(self.GetDefaultLocation())
	}
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
	if success {
		self.state.Shuffle.Unset()
		self.lastKnownState.Shuffle.Set(true)
	} else {
		self.state.Shuffle.Set(true)
	}
}

// RemoveConnectedProvider is an action on the hosted device (unlike the
// read-only provider-locations surface, which derives from the bridged window
// monitor), so it is a plain rpc call. It is dropped when the rpc is down —
// the exclusion only lives as long as the connection anyway. stateLock is held
// across the call like every other forward rpc: the closeService cleanup
// requires the lock.
func (self *DeviceRemote) RemoveConnectedProvider(clientId *Id) {
	if clientId == nil {
		return
	}

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.service == nil {
		return
	}
	rpcCallVoid(self.service, "DeviceLocalRpc.RemoveConnectedProvider", clientId.toConnectId(), self.closeService)
}

func (self *DeviceRemote) Cancel() {
	self.cancel()
}

func (self *DeviceRemote) Close() {
	self.closeOnce.Do(func() {
		// Close child controllers while the RPC service is still available so
		// their listener removals reach the hosted device. In particular,
		// ConnectViewController.Close now detaches its current window monitor.
		self.viewControllerManager.Close()
		func() {
			self.providerLocationsMonitorLock.Lock()
			defer self.providerLocationsMonitorLock.Unlock()
			if self.providerLocationsMonitorUnsub != nil {
				self.providerLocationsMonitorUnsub()
				self.providerLocationsMonitorUnsub = nil
				self.providerLocationsMonitor = nil
			}
		}()
		self.cancel()

		if self.apiJwtRefreshSub != nil {
			self.apiJwtRefreshSub.Close()
			self.apiJwtRefreshSub = nil
		}
		if self.apiAuthLogoutSub != nil {
			self.apiAuthLogoutSub.Close()
			self.apiAuthLogoutSub = nil
		}

		api := self.networkSpace.GetApi()
		api.SetByJwt("")
		api.setHttpPostRaw(nil)
		api.setHttpGetRaw(nil)
	})
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
	// Registration is deferred until the first callback is attached. A grid
	// can be canceled between requesting a monitor and subscribing to it; eagerly
	// retaining here would leave an empty historical window in every reconnect
	// sync request.
	return newDeviceRemoteWindowMonitor(self)
}

func (self *DeviceRemote) windowMonitorAddMonitorEventCallback(windowMonitor *deviceRemoteWindowMonitor, monitorEventCallback connect.MonitorEventFunction) func() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if existing, ok := self.windowMonitors[windowMonitor.windowId]; ok && existing != windowMonitor {
		// IDs are unique, but do not allow an obsolete object to replace a live
		// monitor if a malformed caller reuses one.
		return func() {}
	}
	self.windowMonitors[windowMonitor.windowId] = windowMonitor

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
		if len(windowMonitor.listeners) == 0 {
			delete(self.windowMonitors, windowMonitor.windowId)
		}

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

// the bool reports whether the monitor state was actually readable: false when
// the window is stale, the rpc service is down, the call fails, or the device
// has no local monitor. Consumers reconciling against the events (ConnectGrid)
// freeze on unavailable rather than treating the empty result as truth.
func (self *DeviceRemote) windowMonitorEvents(windowMonitor *deviceRemoteWindowMonitor) (*connect.WindowExpandEvent, map[connect.Id]*connect.ProviderEvent, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if _, ok := self.windowMonitors[windowMonitor.windowId]; !ok {
		// window no longer active
		return &connect.WindowExpandEvent{}, map[connect.Id]*connect.ProviderEvent{}, false
	}

	if self.service == nil {
		return &connect.WindowExpandEvent{}, map[connect.Id]*connect.ProviderEvent{}, false
	}

	event, err := rpcCallNoArg[*DeviceRemoteWindowMonitorEvent](self.service, "DeviceLocalRpc.WindowMonitorEvents", self.closeService)
	if err != nil {
		return &connect.WindowExpandEvent{}, map[connect.Id]*connect.ProviderEvent{}, false
	}
	if event == nil {
		return &connect.WindowExpandEvent{}, map[connect.Id]*connect.ProviderEvent{}, false
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
			return &connect.WindowExpandEvent{}, map[connect.Id]*connect.ProviderEvent{}, false
		}
	*/

	return event.WindowExpandEvent, event.ProviderEvents, true
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
	windowExpandEvent, providerEvents, _ := self.deviceRemote.windowMonitorEvents(self)
	return windowExpandEvent, providerEvents
}

// windowMonitorWithAvailability
func (self *deviceRemoteWindowMonitor) EventsWithAvailability() (*connect.WindowExpandEvent, map[connect.Id]*connect.ProviderEvent, bool) {
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
		if success {
			self.state.ResetEgressSecurityPolicyStats.Unset()
			self.state.EgressSecurityPolicyStats.Unset()
			self.lastKnownState.ResetEgressSecurityPolicyStats.Set(true)
			self.lastKnownState.EgressSecurityPolicyStats.Unset()
		} else {
			self.state.ResetEgressSecurityPolicyStats.Set(true)
			self.state.EgressSecurityPolicyStats.Unset()
		}
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
		if success {
			self.state.ResetIngressSecurityPolicyStats.Unset()
			self.state.IngressSecurityPolicyStats.Unset()
			self.lastKnownState.ResetIngressSecurityPolicyStats.Set(true)
			self.lastKnownState.IngressSecurityPolicyStats.Unset()
		} else {
			self.state.ResetIngressSecurityPolicyStats.Set(true)
			self.state.IngressSecurityPolicyStats.Unset()
		}
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
	orderedKeys := slices.Collect(maps.Keys(listenerMap))
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
		connect.HandleError(func() {
			provideChangeListener.ProvideChanged(provideEnabled)
		})
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
		connect.HandleError(func() {
			providePausedChangeListener.ProvidePausedChanged(providePaused)
		})
	}
}

func (self *DeviceRemote) canShowRatingDialogChanged(canShowRatingDialog bool) {
	listenerList := func() []CanShowRatingDialogChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.lastKnownState.CanShowRatingDialog.Set(canShowRatingDialog)
		return listenerList(self.canShowRatingDialogChangeListeners)
	}()
	for _, canShowRatingDialogChangeListener := range listenerList {
		connect.HandleError(func() {
			canShowRatingDialogChangeListener.CanShowRatingDialogChanged(canShowRatingDialog)
		})
	}
}

func (self *DeviceRemote) canPromptIntroFunnelChanged(canPromptIntroFunnel bool) {
	listenerList := func() []CanPromptIntroFunnelChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.lastKnownState.CanPromptIntroFunnel.Set(canPromptIntroFunnel)
		return listenerList(self.canPromptIntroFunnelChangeListeners)
	}()
	for _, canPromptIntroFunnelChangeListener := range listenerList {
		connect.HandleError(func() {
			canPromptIntroFunnelChangeListener.CanPromptIntroFunnelChanged(canPromptIntroFunnel)
		})
	}
}

func (self *DeviceRemote) allowForegroundChanged(allowForeground bool) {
	listenerList := func() []AllowForegroundChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.lastKnownState.AllowForeground.Set(allowForeground)
		return listenerList(self.allowForegroundChangeListeners)
	}()
	for _, allowForegroundChangeListener := range listenerList {
		connect.HandleError(func() {
			allowForegroundChangeListener.AllowForegroundChanged(allowForeground)
		})
	}
}

func (self *DeviceRemote) canReferChanged(canRefer bool) {
	listenerList := func() []CanReferChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.lastKnownState.CanRefer.Set(canRefer)
		return listenerList(self.canReferChangeListeners)
	}()
	for _, canReferChangeListener := range listenerList {
		connect.HandleError(func() {
			canReferChangeListener.CanReferChanged(canRefer)
		})
	}
}

func (self *DeviceRemote) provideModeChanged(provideMode ProvideMode) {
	listenerList := func() []ProvideModeChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.lastKnownState.ProvideMode.Set(provideMode)
		return listenerList(self.provideModeChangeListeners)
	}()
	for _, provideModeChangeListener := range listenerList {
		connect.HandleError(func() {
			provideModeChangeListener.ProvideModeChanged(provideMode)
		})
	}
}

func (self *DeviceRemote) provideControlModeChanged(provideControlMode ProvideControlMode) {
	listenerList := func() []ProvideControlModeChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.lastKnownState.ProvideControlMode.Set(provideControlMode)
		return listenerList(self.provideControlModeChangeListeners)
	}()
	for _, provideControlModeChangeListener := range listenerList {
		connect.HandleError(func() {
			provideControlModeChangeListener.ProvideControlModeChanged(provideControlMode)
		})
	}
}

func (self *DeviceRemote) providerIdentitiesChanged(providerIdentities *ProviderIdentityList) {
	listenerList := func() []ProviderIdentityChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if providerIdentities != nil {
			self.lastProviderIdentities = providerIdentities.getAll()
		} else {
			self.lastProviderIdentities = nil
		}
		return listenerList(self.providerIdentityChangeListeners)
	}()
	for _, providerIdentityChangeListener := range listenerList {
		connect.HandleError(func() {
			providerIdentityChangeListener.ProviderIdentitiesChanged()
		})
	}
}

func (self *DeviceRemote) performanceProfileChanged(performanceProfile *PerformanceProfile) {
	performanceProfile = clonePerformanceProfile(performanceProfile)
	listenerList := func() []PerformanceProfileChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.lastKnownState.PerformanceProfile.Set(performanceProfile)
		return listenerList(self.performanceProfileChangeListeners)
	}()
	for _, performanceProfileChangeListener := range listenerList {
		connect.HandleError(func() {
			performanceProfileChangeListener.PerformanceProfileChanged(
				clonePerformanceProfile(performanceProfile),
			)
		})
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
		connect.HandleError(func() {
			provideNetworkModeChangeListener.ProvideNetworkModeChanged(provideNetworkMode)
		})
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
		connect.HandleError(func() {
			offlineChangeListener.OfflineChanged(offline, vpnInterfaceWhileOffline)
		})
	}
}

func (self *DeviceRemote) vpnInterfaceWhileOfflineChanged(vpnInterfaceWhileOffline bool) {
	listenerList := func() []VpnInterfaceWhileOfflineChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.lastKnownState.VpnInterfaceWhileOffline.Set(vpnInterfaceWhileOffline)
		return listenerList(self.vpnInterfaceWhileOfflineChangeListeners)
	}()
	for _, vpnInterfaceWhileOfflineChangeListener := range listenerList {
		connect.HandleError(func() {
			vpnInterfaceWhileOfflineChangeListener.VpnInterfaceWhileOfflineChanged(vpnInterfaceWhileOffline)
		})
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
		connect.HandleError(func() {
			connectChangeListener.ConnectChanged(connectEnabled)
		})
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
		connect.HandleError(func() {
			routeLocalChangeListener.RouteLocalChanged(routeLocal)
		})
	}
}

func (self *DeviceRemote) blockerEnabledChanged(blockerEnabled bool) {
	listenerList := func() []BlockerEnabledChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.lastKnownState.BlockerEnabled.Set(blockerEnabled)
		return listenerList(self.blockerEnabledChangeListeners)
	}()
	for _, blockerEnabledChangeListener := range listenerList {
		connect.HandleError(func() {
			blockerEnabledChangeListener.BlockerEnabledChanged(blockerEnabled)
		})
	}
}

func (self *DeviceRemote) connectLocationChanged(deviceRemoteLocation *DeviceRemoteConnectLocation) {
	listenerList := func() []ConnectLocationChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.lastKnownState.Location.Set(deviceRemoteLocation)
		self.lastKnownState.Destination.Unset()
		self.lastKnownState.RemoveDestination.Unset()
		return listenerList(self.connectLocationChangeListeners)
	}()
	location := deviceRemoteLocation.toConnectLocation()
	for _, connectLocationChangeListener := range listenerList {
		connect.HandleError(func() {
			connectLocationChangeListener.ConnectLocationChanged(cloneConnectLocation(location))
		})
	}
}

func (self *DeviceRemote) defaultLocationChanged(location *ConnectLocation) {
	listenerList := func() []DefaultLocationChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.lastKnownState.DefaultLocation.Set(newDeviceRemoteDefaultLocation(location))
		return listenerList(self.defaultLocationChangeListeners)
	}()
	for _, defaultLocationChangeListener := range listenerList {
		connect.HandleError(func() {
			defaultLocationChangeListener.DefaultLocationChanged(cloneConnectLocation(location))
		})
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
		connect.HandleError(func() {
			provideSecretKeyListener.ProvideSecretKeysChanged(provideSecretKeyList)
		})
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
		connect.HandleError(func() {
			tunnelChangeListener.TunnelChanged(tunnelStarted)
		})
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
		connect.HandleError(func() {
			contractStatusChangeListener.ContractStatusChanged(contractStatus)
		})
	}
}

func (self *DeviceRemote) jwtRefreshed(jwt string) {
	for _, listener := range self.jwtRefreshListeners.Get() {
		connect.HandleError(func() {
			listener.JwtRefreshed(jwt)
		})
	}
}

func (self *DeviceRemote) authLogout() {
	for _, listener := range self.authLogoutListeners.Get() {
		connect.HandleError(func() {
			listener.AuthLogout()
		})
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
		connect.HandleError(func() {
			windowStatusChangeListener.WindowStatusChanged(windowStatus)
		})
	}
}

func (self *DeviceRemote) blockActionWindowChanged(blockActionWindow *BlockActionWindow) {
	listenerList := func() []BlockActionWindowChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return listenerList(self.blockActionWindowChangeListeners)
	}()
	for _, blockActionWindowChangeListener := range listenerList {
		connect.HandleError(func() {
			blockActionWindowChangeListener.BlockActionWindowChanged(blockActionWindow)
		})
	}
}

func (self *DeviceRemote) blockStatsChanged(blockStats *BlockStats) {
	listenerList := func() []BlockStatsChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return listenerList(self.blockStatsChangeListeners)
	}()
	for _, blockStatsChangeListener := range listenerList {
		connect.HandleError(func() {
			blockStatsChangeListener.BlockStatsChanged(blockStats)
		})
	}
}

func (self *DeviceRemote) blockActionOverridesChanged(overridesRpc []*BlockActionOverrideRpc) {
	listenerList := func() []BlockActionOverridesChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.lastKnownState.BlockActionOverrides.Set(overridesRpc)
		return listenerList(self.blockActionOverridesChangeListeners)
	}()
	blockActionOverrides := toBlockActionOverrideList(overridesRpc)
	for _, blockActionOverridesChangeListener := range listenerList {
		connect.HandleError(func() {
			blockActionOverridesChangeListener.BlockActionOverridesChanged(blockActionOverrides)
		})
	}
}

func (self *DeviceRemote) packetStatsChanged(packetStats *PacketStats) {
	listenerList := func() []PacketStatsChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return listenerList(self.packetStatsChangeListeners)
	}()
	for _, packetStatsChangeListener := range listenerList {
		connect.HandleError(func() {
			packetStatsChangeListener.PacketStatsChanged(packetStats)
		})
	}
}

func (self *DeviceRemote) egressContractStatsChanged(contractStats *ContractStats) {
	listenerList := func() []ContractStatsChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return listenerList(self.egressContractStatsChangeListeners)
	}()
	for _, contractStatsChangeListener := range listenerList {
		connect.HandleError(func() {
			contractStatsChangeListener.ContractStatsChanged(contractStats)
		})
	}
}

func (self *DeviceRemote) egressContractDetailsChanged(contractDetails *ContractDetails) {
	listenerList := func() []ContractDetailsChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return listenerList(self.egressContractDetailsChangeListeners)
	}()
	for _, contractDetailsChangeListener := range listenerList {
		connect.HandleError(func() {
			contractDetailsChangeListener.ContractDetailsChanged(contractDetails)
		})
	}
}

func (self *DeviceRemote) ingressContractStatsChanged(contractStats *ContractStats) {
	listenerList := func() []ContractStatsChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return listenerList(self.ingressContractStatsChangeListeners)
	}()
	for _, contractStatsChangeListener := range listenerList {
		connect.HandleError(func() {
			contractStatsChangeListener.ContractStatsChanged(contractStats)
		})
	}
}

func (self *DeviceRemote) ingressContractDetailsChanged(contractDetails *ContractDetails) {
	listenerList := func() []ContractDetailsChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return listenerList(self.ingressContractDetailsChangeListeners)
	}()
	for _, contractDetailsChangeListener := range listenerList {
		connect.HandleError(func() {
			contractDetailsChangeListener.ContractDetailsChanged(contractDetails)
		})
	}
}

func (self *DeviceRemote) providerPacketStatsChanged(packetStats *PacketStats) {
	listenerList := func() []PacketStatsChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return listenerList(self.providerPacketStatsChangeListeners)
	}()
	for _, packetStatsChangeListener := range listenerList {
		connect.HandleError(func() {
			packetStatsChangeListener.PacketStatsChanged(packetStats)
		})
	}
}

func (self *DeviceRemote) providerEgressContractStatsChanged(contractStats *ContractStats) {
	listenerList := func() []ContractStatsChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return listenerList(self.providerEgressContractStatsChangeListeners)
	}()
	for _, contractStatsChangeListener := range listenerList {
		connect.HandleError(func() {
			contractStatsChangeListener.ContractStatsChanged(contractStats)
		})
	}
}

func (self *DeviceRemote) providerEgressContractDetailsChanged(contractDetails *ContractDetails) {
	listenerList := func() []ContractDetailsChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return listenerList(self.providerEgressContractDetailsChangeListeners)
	}()
	for _, contractDetailsChangeListener := range listenerList {
		connect.HandleError(func() {
			contractDetailsChangeListener.ContractDetailsChanged(contractDetails)
		})
	}
}

func (self *DeviceRemote) providerIngressContractStatsChanged(contractStats *ContractStats) {
	listenerList := func() []ContractStatsChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return listenerList(self.providerIngressContractStatsChangeListeners)
	}()
	for _, contractStatsChangeListener := range listenerList {
		connect.HandleError(func() {
			contractStatsChangeListener.ContractStatsChanged(contractStats)
		})
	}
}

func (self *DeviceRemote) providerIngressContractDetailsChanged(contractDetails *ContractDetails) {
	listenerList := func() []ContractDetailsChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return listenerList(self.providerIngressContractDetailsChangeListeners)
	}()
	for _, contractDetailsChangeListener := range listenerList {
		connect.HandleError(func() {
			contractDetailsChangeListener.ContractDetailsChanged(contractDetails)
		})
	}
}

func (self *DeviceRemote) transportSettingsChanged(
	settingsRpc *TransportSettingsRpc,
	statusRpc *TransportStatusRpc,
) {
	transportSettings := settingsRpc.toTransportSettings(false)
	settingsRpc = newTransportSettingsRpc(transportSettings, false)
	settingsListeners, statusListeners, status := func() (
		[]TransportSettingsChangeListener,
		[]TransportStatusChangeListener,
		*TransportStatus,
	) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.lastKnownState.TransportSettings.Set(settingsRpc)
		var status *TransportStatus
		if statusRpc != nil {
			self.lastKnownState.TransportStatus.Set(statusRpc)
			status = statusRpc.toTransportStatus()
		}
		return listenerList(self.transportSettingsChangeListeners),
			listenerList(self.transportStatusChangeListeners), status
	}()
	for _, listener := range settingsListeners {
		connect.HandleError(func() {
			listener.TransportSettingsChanged(cloneTransportSettings(transportSettings))
		})
	}
	if status != nil {
		for _, listener := range statusListeners {
			connect.HandleError(func() {
				listener.TransportStatusChanged(cloneTransportStatus(status))
			})
		}
	}
}

func (self *DeviceRemote) providerTransportSettingsChanged(
	settingsRpc *TransportSettingsRpc,
	statusRpc *TransportStatusRpc,
) {
	transportSettings := settingsRpc.toTransportSettings(true)
	settingsRpc = newTransportSettingsRpc(transportSettings, true)
	settingsListeners, statusListeners, status := func() (
		[]ProviderTransportSettingsChangeListener,
		[]ProviderTransportStatusChangeListener,
		*TransportStatus,
	) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.lastKnownState.ProviderTransportSettings.Set(settingsRpc)
		var status *TransportStatus
		if statusRpc != nil {
			self.lastKnownState.ProviderTransportStatus.Set(statusRpc)
			status = statusRpc.toTransportStatus()
		}
		return listenerList(self.providerTransportSettingsChangeListeners),
			listenerList(self.providerTransportStatusChangeListeners), status
	}()
	for _, listener := range settingsListeners {
		connect.HandleError(func() {
			listener.ProviderTransportSettingsChanged(cloneTransportSettings(transportSettings))
		})
	}
	if status != nil {
		for _, listener := range statusListeners {
			connect.HandleError(func() {
				listener.ProviderTransportStatusChanged(cloneTransportStatus(status))
			})
		}
	}
}

func (self *DeviceRemote) dnsResolverSettingsChanged(settingsRpc *DnsResolverSettingsRpc) {
	listenerList := func() []DnsResolverSettingsChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.lastKnownState.DnsResolverSettings.Set(settingsRpc)
		return listenerList(self.dnsResolverSettingsChangeListeners)
	}()
	dnsResolverSettings := settingsRpc.toDnsResolverSettings()
	for _, dnsResolverSettingsChangeListener := range listenerList {
		connect.HandleError(func() {
			dnsResolverSettingsChangeListener.DnsResolverSettingsChanged(dnsResolverSettings)
		})
	}
}

func (self *DeviceRemote) networkPeersChanged(networkPeers *NetworkPeers) {
	listenerList := func() []NetworkPeersChangeListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return listenerList(self.networkPeersChangeListeners)
	}()
	for _, networkPeersChangeListener := range listenerList {
		connect.HandleError(func() {
			networkPeersChangeListener.NetworkPeersChanged(networkPeers)
		})
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
			connect.HandleError(func() {
				monitorEventCallback(windowExpandEvent, providerEvents, reset)
			})
		}
	}
}

// safe to call on multiple goroutines
func (self *DeviceRemote) httpPostRaw(ctx context.Context, requestUrl string, requestBodyBytes []byte, byJwt string) ([]byte, error) {
	if self.settings.httpMaxBodyBytes() < len(requestBodyBytes) {
		return nil, fmt.Errorf("device rpc http request exceeds %d-byte limit", self.settings.httpMaxBodyBytes())
	}
	releaseHttp, ok := self.acquireHttp(ctx)
	if !ok {
		return nil, fmt.Errorf("Done")
	}
	defer releaseHttp()

	// if server is set, use remote
	// else use local

	requestCtx, requestCancel := context.WithCancel(ctx)
	defer requestCancel()
	go connect.HandleError(func() {
		defer requestCancel()
		select {
		case <-self.ctx.Done():
		case <-requestCtx.Done():
		}
	}, requestCancel)

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
		defer self.removeHttpResponseChannel(httpRequestId, httpResponseChannel)

		var err error
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()

			// the capture above only chose the remote path. The run goroutine can
			// tear down and replace the service between the capture and here, and
			// a failed call on a stale client must not closeService the live one
			if self.service == nil {
				err = fmt.Errorf("rpc service is down")
				close(httpResponseChannel)
				return
			}
			err = rpcCallHttpVoid(self.service, "DeviceLocalRpc.HttpPostRaw", httpRequest, self.closeService)
			// Encoding is complete once the forward RPC returns; do not retain a
			// request body for the entire remote fetch/response lifetime.
			httpRequest.RequestBodyBytes = nil
			requestBodyBytes = nil
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
	releaseHttp, ok := self.acquireHttp(ctx)
	if !ok {
		return nil, fmt.Errorf("Done")
	}
	defer releaseHttp()

	// if server is set, use remote
	// else use local

	requestCtx, requestCancel := context.WithCancel(ctx)
	defer requestCancel()
	go connect.HandleError(func() {
		defer requestCancel()
		select {
		case <-self.ctx.Done():
		case <-requestCtx.Done():
		}
	}, requestCancel)

	service := self.getService()

	if service != nil {
		httpRequestId := connect.NewId()
		httpRequest := &DeviceRemoteHttpRequest{
			RequestId:  httpRequestId,
			RequestUrl: requestUrl,
			ByJwt:      byJwt,
		}

		httpResponseChannel := make(chan *DeviceRemoteHttpResponse)
		defer self.removeHttpResponseChannel(httpRequestId, httpResponseChannel)

		var err error
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()

			// the capture above only chose the remote path. The run goroutine can
			// tear down and replace the service between the capture and here, and
			// a failed call on a stale client must not closeService the live one
			if self.service == nil {
				err = fmt.Errorf("rpc service is down")
				close(httpResponseChannel)
				return
			}
			err = rpcCallHttpVoid(self.service, "DeviceLocalRpc.HttpGetRaw", httpRequest, self.closeService)
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

func (self *DeviceRemote) acquireHttp(ctx context.Context) (release func(), ok bool) {
	self.httpSemOnce.Do(func() {
		self.httpSem = make(chan struct{}, self.settings.httpMaxConcurrent())
	})
	select {
	case self.httpSem <- struct{}{}:
		return func() { <-self.httpSem }, true
	case <-ctx.Done():
		return func() {}, false
	case <-self.ctx.Done():
		return func() {}, false
	}
}

func (self *DeviceRemote) removeHttpResponseChannel(
	requestId connect.Id,
	responseChannel chan *DeviceRemoteHttpResponse,
) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if current, ok := self.httpResponseChannels[requestId]; ok && current == responseChannel {
		delete(self.httpResponseChannels, requestId)
	}
}

func (self *DeviceRemote) httpResponse(httpResponse *DeviceRemoteHttpResponse) {
	if self.settings.httpMaxBodyBytes() < len(httpResponse.BodyBytes) {
		httpResponse = newDeviceRemoteHttpResponseWithLimit(
			httpResponse.RequestId,
			nil,
			fmt.Errorf("device rpc http response exceeds %d-byte limit", self.settings.httpMaxBodyBytes()),
			self.settings.httpMaxBodyBytes(),
		)
	}
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

// GetSyncError returns the message from the last sync the local REJECTED, or
// "" when the last sync succeeded (or none has been rejected). Pair it with
// GetRemoteConnected: not-connected plus an empty sync error is the ordinary
// "local is not running / not reachable yet" case, while not-connected plus a
// non-empty sync error is a rejection that reconnecting will not fix — the
// remote is talking to the wrong device instance, or was built against an
// incompatible device rpc wire version (see DeviceRpcVersion). The two are
// distinguishable by prefix: "device rpc version mismatch: ..." vs "device
// instance mismatch: ...".
func (self *DeviceRemote) GetSyncError() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.syncError
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

func (self *DeviceRemote) AddDeviceRecreatedListener(listener DeviceRecreatedListener) Sub {
	listenerId := self.deviceRecreatedListeners.Add(listener)
	return newSub(func() {
		self.deviceRecreatedListeners.Remove(listenerId)
	})
}

func (self *DeviceRemote) deviceRecreated() {
	for _, listener := range self.deviceRecreatedListeners.Get() {
		listener.DeviceRecreated()
	}
}

// privacy block

func (self *DeviceRemote) GetBlockStats() *BlockStats {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	blockStats, success := func() (*BlockStats, bool) {
		if self.service == nil {
			return nil, false
		}

		stats, err := rpcCallNoArg[*DeviceRemoteBlockStats](self.service, "DeviceLocalRpc.GetBlockStats", self.closeService)
		if err != nil {
			return nil, false
		}
		return stats.BlockStats, true
	}()
	if success {
		return blockStats
	} else {
		return nil
	}
}

func (self *DeviceRemote) GetBlockActions() *BlockActionWindow {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	blockActionWindow, success := func() (*BlockActionWindow, bool) {
		if self.service == nil {
			return nil, false
		}

		window, err := rpcCallNoArg[*DeviceRemoteBlockActionWindow](self.service, "DeviceLocalRpc.GetBlockActions", self.closeService)
		if err != nil {
			return nil, false
		}
		return window.BlockActionWindow.toBlockActionWindow(), true
	}()
	if success {
		return blockActionWindow
	} else {
		return nil
	}
}

func (self *DeviceRemote) AddBlockActionOverride(override *BlockActionOverride) {
	// mirror the `DeviceLocal` guard
	if override == nil || override.OverrideId == nil {
		return
	}
	overrideRpc := newBlockActionOverrideRpc(override)
	var eventOverridesRpc []*BlockActionOverrideRpc
	event := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		// maintain the full overrides list locally, replacing an existing
		// override with the same id (see `DeviceLocal.AddBlockActionOverride`)
		overridesRpc := removeBlockActionOverrideRpc(
			self.state.BlockActionOverrides.Get(
				self.lastKnownState.BlockActionOverrides.Get(nil),
			),
			overrideRpc.OverrideId,
		)
		overridesRpc = append(overridesRpc, overrideRpc)

		success := func() bool {
			if self.service == nil {
				return false
			}

			err := rpcCallVoid(self.service, "DeviceLocalRpc.AddBlockActionOverride", overrideRpc, self.closeService)
			if err != nil {
				return false
			}
			return true
		}()
		if success {
			self.state.BlockActionOverrides.Unset()
			self.lastKnownState.BlockActionOverrides.Set(overridesRpc)
		} else {
			event = true
			eventOverridesRpc = overridesRpc
			self.state.BlockActionOverrides.Set(overridesRpc)
		}
	}()
	if event {
		self.blockActionOverridesChanged(eventOverridesRpc)
	}
}

func (self *DeviceRemote) RemoveBlockActionOverride(overrideId *Id) {
	// mirror the `DeviceLocal` guard
	if overrideId == nil {
		return
	}
	var eventOverridesRpc []*BlockActionOverrideRpc
	event := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		// maintain the full overrides list locally
		// (see `DeviceLocal.RemoveBlockActionOverride`)
		overridesRpc := removeBlockActionOverrideRpc(
			self.state.BlockActionOverrides.Get(
				self.lastKnownState.BlockActionOverrides.Get(nil),
			),
			overrideId.toConnectId(),
		)

		success := func() bool {
			if self.service == nil {
				return false
			}

			err := rpcCallVoid(self.service, "DeviceLocalRpc.RemoveBlockActionOverride", overrideId.toConnectId(), self.closeService)
			if err != nil {
				return false
			}
			return true
		}()
		if success {
			self.state.BlockActionOverrides.Unset()
			self.lastKnownState.BlockActionOverrides.Set(overridesRpc)
		} else {
			event = true
			eventOverridesRpc = overridesRpc
			self.state.BlockActionOverrides.Set(overridesRpc)
		}
	}()
	if event {
		self.blockActionOverridesChanged(eventOverridesRpc)
	}
}

func (self *DeviceRemote) SetBlockActionOverrides(overrides *BlockActionOverrideList) {
	overridesRpc := newBlockActionOverridesRpc(overrides)
	event := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		success := func() bool {
			if self.service == nil {
				return false
			}

			deviceOverrides := &DeviceRemoteBlockActionOverrides{
				BlockActionOverrides: overridesRpc,
			}
			err := rpcCallVoid(self.service, "DeviceLocalRpc.SetBlockActionOverrides", deviceOverrides, self.closeService)
			if err != nil {
				return false
			}
			return true
		}()
		if success {
			self.state.BlockActionOverrides.Unset()
			self.lastKnownState.BlockActionOverrides.Set(overridesRpc)
		} else {
			event = true
			self.state.BlockActionOverrides.Set(overridesRpc)
		}
	}()
	if event {
		self.blockActionOverridesChanged(overridesRpc)
	}
}

func (self *DeviceRemote) GetBlockActionOverrides() *BlockActionOverrideList {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	overridesRpc, success := func() ([]*BlockActionOverrideRpc, bool) {
		if self.service == nil {
			return nil, false
		}

		deviceOverrides, err := rpcCallNoArg[*DeviceRemoteBlockActionOverrides](self.service, "DeviceLocalRpc.GetBlockActionOverrides", self.closeService)
		if err != nil {
			return nil, false
		}
		overridesRpc := deviceOverrides.BlockActionOverrides
		self.lastKnownState.BlockActionOverrides.Set(overridesRpc)
		return overridesRpc, true
	}()
	if success {
		return toBlockActionOverrideList(overridesRpc)
	} else {
		return toBlockActionOverrideList(self.state.BlockActionOverrides.Get(
			self.lastKnownState.BlockActionOverrides.Get(nil),
		))
	}
}

func (self *DeviceRemote) GetLocalOverrideAppIds() *OverrideLocalAppIds {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	localOverrideAppIds, success := func() (*OverrideLocalAppIds, bool) {
		if self.service == nil {
			return nil, false
		}

		appIds, err := rpcCallNoArg[*DeviceRemoteLocalOverrideAppIds](self.service, "DeviceLocalRpc.GetLocalOverrideAppIds", self.closeService)
		if err != nil {
			return nil, false
		}
		return appIds.LocalOverrideAppIds.toOverrideLocalAppIds(), true
	}()
	if success {
		return localOverrideAppIds
	} else {
		return nil
	}
}

func (self *DeviceRemote) AddBlockActionWindowChangeListener(listener BlockActionWindowChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.blockActionWindowChangeListeners,
		"DeviceLocalRpc.AddBlockActionWindowChangeListener",
		"DeviceLocalRpc.RemoveBlockActionWindowChangeListener",
	)
}

func (self *DeviceRemote) AddBlockStatsChangeListener(listener BlockStatsChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.blockStatsChangeListeners,
		"DeviceLocalRpc.AddBlockStatsChangeListener",
		"DeviceLocalRpc.RemoveBlockStatsChangeListener",
	)
}

func (self *DeviceRemote) AddBlockActionOverridesChangeListener(listener BlockActionOverridesChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.blockActionOverridesChangeListeners,
		"DeviceLocalRpc.AddBlockActionOverridesChangeListener",
		"DeviceLocalRpc.RemoveBlockActionOverridesChangeListener",
	)
}

// packet stats

func (self *DeviceRemote) GetPacketStats() *PacketStats {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	packetStats, success := func() (*PacketStats, bool) {
		if self.service == nil {
			return nil, false
		}

		stats, err := rpcCallNoArg[*DeviceRemotePacketStats](self.service, "DeviceLocalRpc.GetPacketStats", self.closeService)
		if err != nil {
			return nil, false
		}
		return stats.PacketStats.toPacketStats(true), true
	}()
	if success {
		return packetStats
	} else {
		return nil
	}
}

func (self *DeviceRemote) AddPacketStatsChangeListener(listener PacketStatsChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.packetStatsChangeListeners,
		"DeviceLocalRpc.AddPacketStatsChangeListener",
		"DeviceLocalRpc.RemovePacketStatsChangeListener",
	)
}

// contract stats

func (self *DeviceRemote) GetEgressContractStats() *ContractStats {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	contractStats, success := func() (*ContractStats, bool) {
		if self.service == nil {
			return nil, false
		}

		stats, err := rpcCallNoArg[*DeviceRemoteContractStats](self.service, "DeviceLocalRpc.GetEgressContractStats", self.closeService)
		if err != nil {
			return nil, false
		}
		return stats.ContractStats, true
	}()
	if success {
		return contractStats
	} else {
		return nil
	}
}

func (self *DeviceRemote) GetEgressContractDetails() *ContractDetailsList {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	contractDetailsList, success := func() (*ContractDetailsList, bool) {
		if self.service == nil {
			return nil, false
		}

		details, err := rpcCallNoArg[*DeviceRemoteContractDetailsList](self.service, "DeviceLocalRpc.GetEgressContractDetails", self.closeService)
		if err != nil {
			return nil, false
		}
		return toContractDetailsList(details.ContractDetailsList), true
	}()
	if success {
		return contractDetailsList
	} else {
		return nil
	}
}

func (self *DeviceRemote) GetIngressContractStats() *ContractStats {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	contractStats, success := func() (*ContractStats, bool) {
		if self.service == nil {
			return nil, false
		}

		stats, err := rpcCallNoArg[*DeviceRemoteContractStats](self.service, "DeviceLocalRpc.GetIngressContractStats", self.closeService)
		if err != nil {
			return nil, false
		}
		return stats.ContractStats, true
	}()
	if success {
		return contractStats
	} else {
		return nil
	}
}

func (self *DeviceRemote) GetIngressContractDetails() *ContractDetailsList {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	contractDetailsList, success := func() (*ContractDetailsList, bool) {
		if self.service == nil {
			return nil, false
		}

		details, err := rpcCallNoArg[*DeviceRemoteContractDetailsList](self.service, "DeviceLocalRpc.GetIngressContractDetails", self.closeService)
		if err != nil {
			return nil, false
		}
		return toContractDetailsList(details.ContractDetailsList), true
	}()
	if success {
		return contractDetailsList
	} else {
		return nil
	}
}

func (self *DeviceRemote) AddEgressContractStatsChangeListener(listener ContractStatsChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.egressContractStatsChangeListeners,
		"DeviceLocalRpc.AddEgressContractStatsChangeListener",
		"DeviceLocalRpc.RemoveEgressContractStatsChangeListener",
	)
}

func (self *DeviceRemote) AddEgressContractDetailsChangeListener(listener ContractDetailsChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.egressContractDetailsChangeListeners,
		"DeviceLocalRpc.AddEgressContractDetailsChangeListener",
		"DeviceLocalRpc.RemoveEgressContractDetailsChangeListener",
	)
}

func (self *DeviceRemote) AddIngressContractStatsChangeListener(listener ContractStatsChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.ingressContractStatsChangeListeners,
		"DeviceLocalRpc.AddIngressContractStatsChangeListener",
		"DeviceLocalRpc.RemoveIngressContractStatsChangeListener",
	)
}

func (self *DeviceRemote) AddIngressContractDetailsChangeListener(listener ContractDetailsChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.ingressContractDetailsChangeListeners,
		"DeviceLocalRpc.AddIngressContractDetailsChangeListener",
		"DeviceLocalRpc.RemoveIngressContractDetailsChangeListener",
	)
}

// provider packet stats

func (self *DeviceRemote) GetProviderPacketStats() *PacketStats {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	packetStats, success := func() (*PacketStats, bool) {
		if self.service == nil {
			return nil, false
		}

		stats, err := rpcCallNoArg[*DeviceRemotePacketStats](self.service, "DeviceLocalRpc.GetProviderPacketStats", self.closeService)
		if err != nil {
			return nil, false
		}
		return stats.PacketStats.toPacketStats(true), true
	}()
	if success {
		return packetStats
	} else {
		return nil
	}
}

func (self *DeviceRemote) AddProviderPacketStatsChangeListener(listener PacketStatsChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.providerPacketStatsChangeListeners,
		"DeviceLocalRpc.AddProviderPacketStatsChangeListener",
		"DeviceLocalRpc.RemoveProviderPacketStatsChangeListener",
	)
}

// provider contract stats

func (self *DeviceRemote) GetProviderEgressContractStats() *ContractStats {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	contractStats, success := func() (*ContractStats, bool) {
		if self.service == nil {
			return nil, false
		}

		stats, err := rpcCallNoArg[*DeviceRemoteContractStats](self.service, "DeviceLocalRpc.GetProviderEgressContractStats", self.closeService)
		if err != nil {
			return nil, false
		}
		return stats.ContractStats, true
	}()
	if success {
		return contractStats
	} else {
		return nil
	}
}

func (self *DeviceRemote) GetProviderEgressContractDetails() *ContractDetailsList {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	contractDetailsList, success := func() (*ContractDetailsList, bool) {
		if self.service == nil {
			return nil, false
		}

		details, err := rpcCallNoArg[*DeviceRemoteContractDetailsList](self.service, "DeviceLocalRpc.GetProviderEgressContractDetails", self.closeService)
		if err != nil {
			return nil, false
		}
		if details.Nil {
			return nil, true
		}
		return toContractDetailsList(details.ContractDetailsList), true
	}()
	if success {
		return contractDetailsList
	} else {
		return nil
	}
}

func (self *DeviceRemote) GetProviderIngressContractStats() *ContractStats {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	contractStats, success := func() (*ContractStats, bool) {
		if self.service == nil {
			return nil, false
		}

		stats, err := rpcCallNoArg[*DeviceRemoteContractStats](self.service, "DeviceLocalRpc.GetProviderIngressContractStats", self.closeService)
		if err != nil {
			return nil, false
		}
		return stats.ContractStats, true
	}()
	if success {
		return contractStats
	} else {
		return nil
	}
}

func (self *DeviceRemote) GetProviderIngressContractDetails() *ContractDetailsList {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	contractDetailsList, success := func() (*ContractDetailsList, bool) {
		if self.service == nil {
			return nil, false
		}

		details, err := rpcCallNoArg[*DeviceRemoteContractDetailsList](self.service, "DeviceLocalRpc.GetProviderIngressContractDetails", self.closeService)
		if err != nil {
			return nil, false
		}
		if details.Nil {
			return nil, true
		}
		return toContractDetailsList(details.ContractDetailsList), true
	}()
	if success {
		return contractDetailsList
	} else {
		return nil
	}
}

func (self *DeviceRemote) AddProviderEgressContractStatsChangeListener(listener ContractStatsChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.providerEgressContractStatsChangeListeners,
		"DeviceLocalRpc.AddProviderEgressContractStatsChangeListener",
		"DeviceLocalRpc.RemoveProviderEgressContractStatsChangeListener",
	)
}

func (self *DeviceRemote) AddProviderEgressContractDetailsChangeListener(listener ContractDetailsChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.providerEgressContractDetailsChangeListeners,
		"DeviceLocalRpc.AddProviderEgressContractDetailsChangeListener",
		"DeviceLocalRpc.RemoveProviderEgressContractDetailsChangeListener",
	)
}

func (self *DeviceRemote) AddProviderIngressContractStatsChangeListener(listener ContractStatsChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.providerIngressContractStatsChangeListeners,
		"DeviceLocalRpc.AddProviderIngressContractStatsChangeListener",
		"DeviceLocalRpc.RemoveProviderIngressContractStatsChangeListener",
	)
}

func (self *DeviceRemote) AddProviderIngressContractDetailsChangeListener(listener ContractDetailsChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.providerIngressContractDetailsChangeListeners,
		"DeviceLocalRpc.AddProviderIngressContractDetailsChangeListener",
		"DeviceLocalRpc.RemoveProviderIngressContractDetailsChangeListener",
	)
}

// dns

func (self *DeviceRemote) SetDnsResolverSettings(dnsResolverSettings *DnsResolverSettings) {
	// mirror the `DeviceLocal` guard
	if dnsResolverSettings == nil {
		return
	}
	settingsRpc := newDnsResolverSettingsRpc(dnsResolverSettings)
	event := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		success := func() bool {
			if self.service == nil {
				return false
			}

			deviceSettings := &DeviceRemoteDnsResolverSettings{
				DnsResolverSettings: settingsRpc,
			}
			err := rpcCallVoid(self.service, "DeviceLocalRpc.SetDnsResolverSettings", deviceSettings, self.closeService)
			if err != nil {
				return false
			}
			return true
		}()
		if success {
			self.state.DnsResolverSettings.Unset()
			self.lastKnownState.DnsResolverSettings.Set(settingsRpc)
		} else {
			event = true
			self.state.DnsResolverSettings.Set(settingsRpc)
		}
	}()
	if event {
		self.dnsResolverSettingsChanged(settingsRpc)
	}
}

func (self *DeviceRemote) GetDnsResolverSettings() *DnsResolverSettings {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	settingsRpc, success := func() (*DnsResolverSettingsRpc, bool) {
		if self.service == nil {
			return nil, false
		}

		deviceSettings, err := rpcCallNoArg[*DeviceRemoteDnsResolverSettings](self.service, "DeviceLocalRpc.GetDnsResolverSettings", self.closeService)
		if err != nil {
			return nil, false
		}
		settingsRpc := deviceSettings.DnsResolverSettings
		self.lastKnownState.DnsResolverSettings.Set(settingsRpc)
		return settingsRpc, true
	}()
	if success {
		return settingsRpc.toDnsResolverSettings()
	} else {
		return self.state.DnsResolverSettings.Get(
			self.lastKnownState.DnsResolverSettings.Get(nil),
		).toDnsResolverSettings()
	}
}

func (self *DeviceRemote) AddDnsResolverSettingsChangeListener(listener DnsResolverSettingsChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.dnsResolverSettingsChangeListeners,
		"DeviceLocalRpc.AddDnsResolverSettingsChangeListener",
		"DeviceLocalRpc.RemoveDnsResolverSettingsChangeListener",
	)
}

// transport settings

func (self *DeviceRemote) setTransportSettings(transportSettings *TransportSettings, provider bool) {
	if self.settings.DisableHostedIncompatible {
		self.log.Infof("[dr]hosted incompatible: transport settings ignored")
		return
	}
	settingsRpc := newTransportSettingsRpc(transportSettings, provider)
	event := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		var currentRpc *TransportSettingsRpc
		if provider {
			currentRpc = self.state.ProviderTransportSettings.Get(
				self.lastKnownState.ProviderTransportSettings.Get(nil),
			)
		} else {
			currentRpc = self.state.TransportSettings.Get(
				self.lastKnownState.TransportSettings.Get(nil),
			)
		}
		changed := !transportSettingsEqual(
			currentRpc.toTransportSettings(provider),
			settingsRpc.toTransportSettings(provider),
			provider,
		)

		method := "DeviceLocalRpc.SetTransportSettings"
		if provider {
			method = "DeviceLocalRpc.SetProviderTransportSettings"
		}
		success := false
		if self.service != nil {
			deviceSettings := &DeviceRemoteTransportSettingsRpc{TransportSettings: settingsRpc}
			success = rpcCallVoid(self.service, method, deviceSettings, self.closeService) == nil
		}
		if provider {
			if success {
				self.state.ProviderTransportSettings.Unset()
				self.lastKnownState.ProviderTransportSettings.Set(settingsRpc)
			} else {
				self.state.ProviderTransportSettings.Set(settingsRpc)
				event = changed
			}
		} else if success {
			self.state.TransportSettings.Unset()
			self.lastKnownState.TransportSettings.Set(settingsRpc)
		} else {
			self.state.TransportSettings.Set(settingsRpc)
			event = changed
		}
	}()
	if event {
		if provider {
			self.providerTransportSettingsChanged(settingsRpc, nil)
		} else {
			self.transportSettingsChanged(settingsRpc, nil)
		}
	}
}

func (self *DeviceRemote) SetTransportSettings(transportSettings *TransportSettings) {
	self.setTransportSettings(transportSettings, false)
}

func (self *DeviceRemote) SetProviderTransportSettings(transportSettings *TransportSettings) {
	self.setTransportSettings(transportSettings, true)
}

func (self *DeviceRemote) getTransportSettings(provider bool) *TransportSettings {
	if self.settings.DisableHostedIncompatible {
		return hostedTransportSettings()
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	method := "DeviceLocalRpc.GetTransportSettings"
	if provider {
		method = "DeviceLocalRpc.GetProviderTransportSettings"
	}
	if self.service != nil {
		deviceSettings, err := rpcCallNoArg[*DeviceRemoteTransportSettingsRpc](self.service, method, self.closeService)
		if err == nil && deviceSettings != nil && deviceSettings.TransportSettings != nil {
			if provider {
				self.lastKnownState.ProviderTransportSettings.Set(deviceSettings.TransportSettings)
				if deviceSettings.TransportStatus != nil {
					self.lastKnownState.ProviderTransportStatus.Set(deviceSettings.TransportStatus)
				}
			} else {
				self.lastKnownState.TransportSettings.Set(deviceSettings.TransportSettings)
				if deviceSettings.TransportStatus != nil {
					self.lastKnownState.TransportStatus.Set(deviceSettings.TransportStatus)
				}
			}
			return deviceSettings.TransportSettings.toTransportSettings(provider)
		}
	}

	var settingsRpc *TransportSettingsRpc
	if provider {
		settingsRpc = self.state.ProviderTransportSettings.Get(
			self.lastKnownState.ProviderTransportSettings.Get(nil),
		)
	} else {
		settingsRpc = self.state.TransportSettings.Get(
			self.lastKnownState.TransportSettings.Get(nil),
		)
	}
	return settingsRpc.toTransportSettings(provider)
}

func (self *DeviceRemote) GetTransportSettings() *TransportSettings {
	return self.getTransportSettings(false)
}

func (self *DeviceRemote) AddTransportSettingsChangeListener(listener TransportSettingsChangeListener) Sub {
	return addCompatibleListener(
		self,
		listener,
		self.transportSettingsChangeListeners,
		"DeviceLocalRpc.AddTransportSettingsChangeListener",
		"DeviceLocalRpc.RemoveTransportSettingsChangeListener",
	)
}

func (self *DeviceRemote) GetTransportStatus() *TransportStatus {
	// The existing settings getter carries the paired runtime status on the
	// same RPC snapshot, keeping policy and eligibility from different moments
	// from being combined in the UI.
	self.GetTransportSettings()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.lastKnownState.TransportStatus.Get(nil).toTransportStatus()
}

func (self *DeviceRemote) AddTransportStatusChangeListener(listener TransportStatusChangeListener) Sub {
	// Status rides the paired transport-settings wire event. A distinct public
	// listener keeps runtime capability separate without adding a second RPC
	// subscription or duplicate event stream.
	return addCompatibleListener(
		self,
		listener,
		self.transportStatusChangeListeners,
		"DeviceLocalRpc.AddTransportSettingsChangeListener",
		"DeviceLocalRpc.RemoveTransportSettingsChangeListener",
	)
}

func (self *DeviceRemote) GetProviderTransportSettings() *TransportSettings {
	return self.getTransportSettings(true)
}

func (self *DeviceRemote) AddProviderTransportSettingsChangeListener(listener ProviderTransportSettingsChangeListener) Sub {
	return addCompatibleListener(
		self,
		listener,
		self.providerTransportSettingsChangeListeners,
		"DeviceLocalRpc.AddProviderTransportSettingsChangeListener",
		"DeviceLocalRpc.RemoveProviderTransportSettingsChangeListener",
	)
}

func (self *DeviceRemote) GetProviderTransportStatus() *TransportStatus {
	self.GetProviderTransportSettings()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.lastKnownState.ProviderTransportStatus.Get(nil).toTransportStatus()
}

func (self *DeviceRemote) AddProviderTransportStatusChangeListener(listener ProviderTransportStatusChangeListener) Sub {
	return addCompatibleListener(
		self,
		listener,
		self.providerTransportStatusChangeListeners,
		"DeviceLocalRpc.AddProviderTransportSettingsChangeListener",
		"DeviceLocalRpc.RemoveProviderTransportSettingsChangeListener",
	)
}

// network peers

func (self *DeviceRemote) GetNetworkPeers() *NetworkPeers {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	networkPeers, success := func() (*NetworkPeers, bool) {
		if self.service == nil {
			return nil, false
		}

		peers, err := rpcCallNoArg[*DeviceRemoteNetworkPeers](self.service, "DeviceLocalRpc.GetNetworkPeers", self.closeService)
		if err != nil {
			return nil, false
		}
		return peers.NetworkPeers.toNetworkPeers(), true
	}()
	if success {
		return networkPeers
	} else {
		return nil
	}
}

func (self *DeviceRemote) AddNetworkPeersChangeListener(listener NetworkPeersChangeListener) Sub {
	return addListener(
		self,
		listener,
		self.networkPeersChangeListeners,
		"DeviceLocalRpc.AddNetworkPeersChangeListener",
		"DeviceLocalRpc.RemoveNetworkPeersChangeListener",
	)
}

// reliability (see reliability_controls.go for the `DeviceLocal` side)
//
// The getters read through to the local device and report what it reports.
// Metrics and the two list readouts degrade to zeros and empty lists, which
// are honest answers ("nothing measured yet", "no exits") and keep a nil
// list from crossing gomobile. SETTINGS do not degrade: they are nil-able
// end to end, because settings is the read-modify-WRITE path and a
// synthesized zero struct written back is an all-off override (see
// `DeviceLocal.GetReliabilitySettings`). The actions are no-ops while the
// rpc is down, matching the `DeviceLocal` methods that no-op while there is
// no multi client to act on.

// GetReliabilitySettings reports the reliability behavior currently in
// effect on the local device: the runtime override when one is set and the
// shipped defaults otherwise.
//
// nil when there is no effective config to report -- the local has no multi
// client (tunnel down), or the rpc is down with nothing pending to describe.
// Zeros are NEVER synthesized here: on ios the DeviceRemote is created at
// login and outlives every tunnel session, so a getter that manufactured a
// zero struct while the tunnel was down would hand the developer screen's
// read-modify-write an all-off struct to write back. nil is the signal that
// the controls have nothing to act on.
//
// While the rpc is down a queued override IS reported, since that is a real
// struct the caller itself set and will be applied on the next sync -- a
// read-modify-write on it preserves the caller's intent. A queued RESET
// carries no values, so it falls through to the last known effective
// settings, and to nil when there are none.
func (self *DeviceRemote) GetReliabilitySettings() *ReliabilitySettings {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	settings, success := func() (*ReliabilitySettings, bool) {
		if self.service == nil {
			return nil, false
		}

		deviceSettings, err := rpcCallNoArg[*DeviceRemoteReliabilitySettingsRpc](self.service, "DeviceLocalRpc.GetReliabilitySettings", self.closeService)
		if err != nil {
			return nil, false
		}
		settings := deviceSettings.ReliabilitySettings
		// mirror the local's answer, nil included: once the rpc drops, the
		// fallback below must not resurrect settings the local has already
		// stopped reporting
		self.lastKnownState.ReliabilitySettings.Set(cloneReliabilitySettings(settings))
		return settings, true
	}()
	if success {
		// the local is reachable, so its answer is authoritative -- including
		// nil, which is "no multi client, nothing in force"
		return settings
	}
	// rpc down: report the queued override if one is pending, since that IS
	// what will be in force the moment the local comes back
	if override := self.state.ReliabilitySettings.Get(nil); override != nil {
		return cloneReliabilitySettings(override)
	}
	// otherwise nil, and deliberately NOT lastKnownState. The caller's whole
	// contract is "non-nil means these settings are in force right now" -- a
	// developer screen enables its controls on it and presents the values as
	// effective. Last session's settings are not in force while the local is
	// unreachable, and on ios the extension holding the local is stopped on
	// every disconnect, so serving them would leave the screen live-looking
	// and every action a silent no-op against a device that is not there.
	return nil
}

// SetReliabilitySettings overrides the reliability behavior at runtime, no
// reconnect. The override is runtime-only state on the local device --
// nothing persists it across an extension restart -- so unlike the other
// setters it stays queued in the sync state even after a successful call and
// is re-applied by every sync (rpc reconnect or extension restart), until
// `ResetReliabilitySettings` clears it.
func (self *DeviceRemote) SetReliabilitySettings(reliabilitySettings *ReliabilitySettings) {
	// a nil override is invalid (`DeviceLocal.SetReliabilitySettings` does
	// not accept nil); use `ResetReliabilitySettings` to clear the override
	if reliabilitySettings == nil {
		return
	}
	// own a copy so a caller mutating its struct after the call (the
	// read-modify-write ui pattern) cannot corrupt the queued sync state
	settings := cloneReliabilitySettings(reliabilitySettings)

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	success := func() bool {
		if self.service == nil {
			return false
		}

		deviceSettings := &DeviceRemoteReliabilitySettingsRpc{
			ReliabilitySettings: settings,
		}
		err := rpcCallVoid(self.service, "DeviceLocalRpc.SetReliabilitySettings", deviceSettings, self.closeService)
		if err != nil {
			return false
		}
		return true
	}()
	// queued on success AND failure: the sync-state re-apply is what keeps
	// the override alive across extension restarts
	self.state.ReliabilitySettings.Set(settings)
	if success {
		self.lastKnownState.ReliabilitySettings.Set(cloneReliabilitySettings(settings))
	}
}

// ResetReliabilitySettings clears any runtime override, restoring the
// shipped behavior. Clears the queued sync-state override so it is no longer
// re-applied on reconnect; while the rpc is down the reset itself is queued
// and applied on the next sync.
func (self *DeviceRemote) ResetReliabilitySettings() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	success := func() bool {
		if self.service == nil {
			return false
		}

		err := rpcCallNoArgVoid(self.service, "DeviceLocalRpc.ResetReliabilitySettings", self.closeService)
		if err != nil {
			return false
		}
		return true
	}()
	if success {
		// the local override is cleared and a fresh local starts without
		// one, so there is nothing left to re-apply on sync. The last known
		// effective settings are stale now (they may reflect the override);
		// drop them and let the next connected get repopulate.
		self.state.ReliabilitySettings.Unset()
		self.lastKnownState.ReliabilitySettings.Unset()
	} else {
		// queue the reset so the next sync clears any override the local
		// still holds
		self.state.ReliabilitySettings.Set(nil)
	}
}

// GetReliabilityMetrics reports what provider failures have cost since the
// last reset. Zeros while the rpc is down, matching `DeviceLocal` while
// disconnected -- a fresh extension starts from zero counters anyway.
func (self *DeviceRemote) GetReliabilityMetrics() *ReliabilityMetrics {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	metrics, success := func() (*ReliabilityMetrics, bool) {
		if self.service == nil {
			return nil, false
		}

		deviceMetrics, err := rpcCallNoArg[*DeviceRemoteReliabilityMetricsRpc](self.service, "DeviceLocalRpc.GetReliabilityMetrics", self.closeService)
		if err != nil {
			return nil, false
		}
		return deviceMetrics.ReliabilityMetrics, true
	}()
	if success && metrics != nil {
		return metrics
	}
	return &ReliabilityMetrics{}
}

// ResetReliabilityMetrics zeroes the counters. A dev action: no-op while the
// rpc is down rather than queued, so a reset can never zero a later run the
// user is not watching.
func (self *DeviceRemote) ResetReliabilityMetrics() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.service == nil {
		return
	}
	rpcCallNoArgVoid(self.service, "DeviceLocalRpc.ResetReliabilityMetrics", self.closeService)
}

// GetExits lists the current provider channels with the flow count pinned to
// each. Empty (never nil) while the rpc is down, matching `DeviceLocal`
// while disconnected.
func (self *DeviceRemote) GetExits() *ExitList {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	exits, success := func() (*ExitList, bool) {
		if self.service == nil {
			return nil, false
		}

		deviceExits, err := rpcCallNoArg[*DeviceRemoteExitListRpc](self.service, "DeviceLocalRpc.GetExits", self.closeService)
		if err != nil {
			return nil, false
		}
		return toExitList(deviceExits.Exits), true
	}()
	if success && exits != nil {
		return exits
	}
	return NewExitList()
}

// GetDestinationExits reports which exit currently carries each destination
// ip, aggregated over live flows. Empty (never nil) while the rpc is down.
func (self *DeviceRemote) GetDestinationExits() *DestinationExitList {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	destinationExits, success := func() (*DestinationExitList, bool) {
		if self.service == nil {
			return nil, false
		}

		deviceDestinationExits, err := rpcCallNoArg[*DeviceRemoteDestinationExitListRpc](self.service, "DeviceLocalRpc.GetDestinationExits", self.closeService)
		if err != nil {
			return nil, false
		}
		return toDestinationExitList(deviceDestinationExits.DestinationExits), true
	}()
	if success && destinationExits != nil {
		return destinationExits
	}
	return NewDestinationExitList()
}

// MigrateExit hands one exit's movable flows to live replacements now (the
// drain-style hand-off; see `DeviceLocal.MigrateExit`). The exit client id
// is the string form (`Exit.ClientId.IdStr`). Returns the number of flows
// moved, and -1 when nothing could be attempted -- an unparseable id, a down
// rpc, or no such exit in the window (the local sentinel). The count is what
// makes this reportable in a ui: "requested" and "migrated 12 flows" are
// different outcomes and the caller cannot otherwise tell them apart.
func (self *DeviceRemote) MigrateExit(exitClientId string) int32 {
	exitId, err := ParseId(exitClientId)
	if err != nil {
		return -1
	}

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.service == nil {
		return -1
	}
	migratedCount, err := rpcCall[int32](self.service, "DeviceLocalRpc.MigrateExit", exitId.toConnectId(), self.closeService)
	if err != nil {
		return -1
	}
	return migratedCount
}

// NOTE: the WP1 contract lists `ProbeExit(exitClientId string)` (one
// qualification pass for one exit). It is deliberately NOT bridged: neither
// `DeviceLocal` nor connect's `RemoteUserNatMultiClient` exposes a per-exit
// probe seam (the per-client pass, the window client enumeration, and the
// qualification table are all unexported in connect; the only exported seam
// is `ProbeAllExits`). Bridging it would have meant inventing semantics.
// Add `RemoteUserNatMultiClient.ProbeExit(clientId Id)` in connect, mirror
// it in reliability_controls.go, and bridge it here following the
// `MigrateExit` shape.

// ProbeAllExits fires a qualification probe pass at every exit in the
// windows now, instead of waiting for the background sweep. Non-blocking on
// the local side. Returns how many passes were scheduled, and 0 while the rpc
// is down -- the same "nothing was scheduled" value `DeviceLocal` reports
// while disconnected or while provider probing is off.
func (self *DeviceRemote) ProbeAllExits() int32 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.service == nil {
		return 0
	}
	probeCount, err := rpcCallNoArg[int32](self.service, "DeviceLocalRpc.ProbeAllExits", self.closeService)
	if err != nil {
		return 0
	}
	return probeCount
}

// DropExit kills a single exit, as if that provider had died, leaving the
// others working (fault injection; see `DeviceLocal.DropExit`). The exit
// client id is the string form (`Exit.ClientId.IdStr`). Returns false when
// the exit is no longer in the window, and for an unparseable id or a down
// rpc.
func (self *DeviceRemote) DropExit(exitClientId string) bool {
	exitId, err := ParseId(exitClientId)
	if err != nil {
		return false
	}

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.service == nil {
		return false
	}
	dropped, err := rpcCall[bool](self.service, "DeviceLocalRpc.DropExit", exitId.toConnectId(), self.closeService)
	if err != nil {
		return false
	}
	return dropped
}

// StallExit makes an exit swallow packets without acknowledging them and
// without erroring, so it is neither healthy nor detectably dead (fault
// injection; see `DeviceLocal.StallExit`). Returns false when the exit is no
// longer in the window, and for an unparseable id or a down rpc.
//
// Deliberately not recorded in the sync state: a stall is a live-session
// fault injection, and replaying it onto a fresh device after a reconnect
// would resurrect a fault the user never asked for twice.
func (self *DeviceRemote) StallExit(exitClientId string, stalled bool) bool {
	exitId, err := ParseId(exitClientId)
	if err != nil {
		return false
	}

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.service == nil {
		return false
	}
	stallExit := &DeviceRemoteStallExitRpc{
		ClientId: exitId.toConnectId(),
		Stalled:  stalled,
	}
	set, err := rpcCall[bool](self.service, "DeviceLocalRpc.StallExit", stallExit, self.closeService)
	if err != nil {
		return false
	}
	return set
}

// ShuffleExits replaces every exit at once. No-op while the rpc is down.
//
// This reaches the same connect call as `DeviceRemote.Shuffle` -- both end at
// `RemoteUserNatMultiClient.Shuffle` -- and both exist because they differ in
// the only way that matters for a fault injection control: what happens when
// the call FAILS.
//
// `Shuffle` is a queued action. A failed call sets `state.Shuffle`, and the
// next successful sync replays it. That is right for the connect-flow caller
// it was built for, where "shuffle once the device is reachable again" is the
// intent. It is wrong here. In Advanced Mode the user presses this while
// watching an exit table; if the service is down the press does nothing
// visible, and a queued replay would then swap every exit minutes later,
// under a user who has moved on -- the same "a fault the user did not ask for
// twice" that `StallExit` refuses. Fault injection is immediate or nothing.
//
// So this is deliberately non-queued, like the other six advanced-mode
// actions, and `Shuffle` is left exactly as it was for its existing callers.
func (self *DeviceRemote) ShuffleExits() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.service == nil {
		return
	}
	rpcCallNoArgVoid(self.service, "DeviceLocalRpc.ShuffleExits", self.closeService)
}

// StartProbeSuite begins a probe run and returns immediately. Returns false
// when a run is already in progress, and for a down rpc. A nil config means
// the local default (`GetDefaultProbeSuiteConfig`).
func (self *DeviceRemote) StartProbeSuite(config *ProbeSuiteConfig) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.service == nil {
		return false
	}
	// the wrapper exists so a nil config can cross: `rpcCall` panics on a
	// nil arg, and a nil `*ProbeSuiteConfig` is a meaningful value here
	probeSuiteConfig := &DeviceRemoteProbeSuiteConfigRpc{
		Config: config,
	}
	started, err := rpcCall[bool](self.service, "DeviceLocalRpc.StartProbeSuite", probeSuiteConfig, self.closeService)
	if err != nil {
		return false
	}
	return started
}

// StopProbeSuite cancels a run in progress. Results collected so far are
// kept, so `GetProbeResults` stays meaningful after a stop. No-op while the
// rpc is down.
func (self *DeviceRemote) StopProbeSuite() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.service == nil {
		return
	}
	rpcCallNoArgVoid(self.service, "DeviceLocalRpc.StopProbeSuite", self.closeService)
}

// ProbeSuiteRunning reports whether a run is in progress. This is polled by
// the ui, so a down rpc reports false (not running) rather than failing.
func (self *DeviceRemote) ProbeSuiteRunning() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.service == nil {
		return false
	}
	running, err := rpcCallNoArg[bool](self.service, "DeviceLocalRpc.ProbeSuiteRunning", self.closeService)
	if err != nil {
		return false
	}
	return running
}

// GetProbeResults reports what has completed so far. Safe to call during a
// run. Empty (never nil) while the rpc is down or before a run has started --
// matching `GetExits`, and load-bearing for the c++ wrapper, which unwraps a
// list getter into a `std::vector`.
func (self *DeviceRemote) GetProbeResults() *ProbeResultList {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	results, success := func() (*ProbeResultList, bool) {
		if self.service == nil {
			return nil, false
		}

		deviceProbeResults, err := rpcCallNoArg[*DeviceRemoteProbeResultListRpc](self.service, "DeviceLocalRpc.GetProbeResults", self.closeService)
		if err != nil {
			return nil, false
		}
		return toProbeResultList(deviceProbeResults.Results), true
	}()
	if success && results != nil {
		return results
	}
	return NewProbeResultList()
}

// SimulateNetworkChange fires the platform network-change path on demand --
// the storm drill. No-op while the rpc is down.
func (self *DeviceRemote) SimulateNetworkChange() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.service == nil {
		return
	}
	rpcCallNoArgVoid(self.service, "DeviceLocalRpc.SimulateNetworkChange", self.closeService)
}

func (self *DeviceRemote) UploadLogs(feedbackId string, callback UploadLogsCallback) error {
	logsUploaded := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		success := func() bool {
			if self.service == nil {
				return false
			}

			err := rpcCallVoid(self.service, "DeviceLocalRpc.UploadLogs", feedbackId, self.closeService)

			if err != nil {
				self.log.Infof("Failed to upload logs: %v", err)
				return false
			}
			self.log.Infof("Logs uploaded successfully")
			return true
		}()
		logsUploaded = success
	}()

	if !logsUploaded {
		self.log.Infof("Failed to upload logs")
		return fmt.Errorf("Failed to upload logs")
	}

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
//
//gomobile:noexport is the type-level equivalent of the gomobile:noexport
// marker used elsewhere in the package, and it stands in for it here: gobind
// does emit classes for these types and silently drops ~123 fields across
// them (connect.Id, []string, netip.Addr, time.Duration, deviceRemoteValue[T],
// slices of structs). None of that is a bug — nothing below is meant to reach
// an app — so the omissions are deliberate and are not marked field by field.

// *important* argument and return values from rpc fucntios CANNOT be nil
// this is a limitation in net.rpc

//gomobile:noexport
type DeviceRemoteDestination struct {
	Location *DeviceRemoteConnectLocation
	Specs    []*ProviderSpec
	// ProvideMode ProvideMode
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
type DevicePerformanceProfile struct {
	PerformanceProfile *PerformanceProfile
}

//gomobile:noexport
type DeviceRemoteState struct {
	// thick state + last known state

	CanShowRatingDialog      deviceRemoteValue[bool]
	CanPromptIntroFunnel     deviceRemoteValue[bool]
	ProvideControlMode       deviceRemoteValue[ProvideControlMode]
	CanRefer                 deviceRemoteValue[bool]
	AllowForeground          deviceRemoteValue[bool]
	RouteLocal               deviceRemoteValue[bool]
	BlockerEnabled           deviceRemoteValue[bool]
	InitProvideSecretKeys    deviceRemoteValue[bool]
	LoadProvideSecretKeys    deviceRemoteValue[[]*ProvideSecretKey]
	ProvideMode              deviceRemoteValue[ProvideMode]        // auto, always, never
	ProvideNetworkMode       deviceRemoteValue[ProvideNetworkMode] // wifi or cellular + wifi
	ProvidePaused            deviceRemoteValue[bool]
	Offline                  deviceRemoteValue[bool]
	VpnInterfaceWhileOffline deviceRemoteValue[bool]
	RemoveDestination        deviceRemoteValue[bool]
	Destination              deviceRemoteValue[*DeviceRemoteDestination]
	PerformanceProfile       deviceRemoteValue[*PerformanceProfile]

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
	// the full overrides list is one synced value.
	// add/remove/set on the remote all funnel through it
	BlockActionOverrides      deviceRemoteValue[[]*BlockActionOverrideRpc]
	DnsResolverSettings       deviceRemoteValue[*DnsResolverSettingsRpc]
	TransportSettings         deviceRemoteValue[*TransportSettingsRpc]
	ProviderTransportSettings deviceRemoteValue[*TransportSettingsRpc]
	TransportStatus           deviceRemoteValue[*TransportStatusRpc]
	ProviderTransportStatus   deviceRemoteValue[*TransportStatusRpc]
	// the runtime reliability settings override. Unlike the fields above this
	// is runtime-only state on the local (nothing persists it), so the remote
	// keeps it queued across syncs and re-applies it on every reconnect and
	// extension restart. See `DeviceRemote.SetReliabilitySettings`.
	//
	// a nil value never means "an all-zero override" -- zero settings are
	// never synthesized on this path. Its meaning depends on the direction:
	// in a sync REQUEST (the remote's queued state) IsSet with nil queues a
	// reset, clearing the override; in a sync RESPONSE (the local's current
	// state) nil means the local has no multi client, so nothing is in force.
	// Nothing copies response state into request state, which is what keeps
	// the two readings from colliding.
	ReliabilitySettings deviceRemoteValue[*ReliabilitySettings]

	// only last known state

	ConnectEnabled deviceRemoteValue[bool]
	ProvideEnabled deviceRemoteValue[bool]

	EgressSecurityPolicyStats  deviceRemoteValue[connect.SecurityPolicyStats]
	IngressSecurityPolicyStats deviceRemoteValue[connect.SecurityPolicyStats]

	ContractStatus deviceRemoteValue[*ContractStatus]
	WindowStatus   deviceRemoteValue[*WindowStatus]

	// RefreshToken deviceRemoteValue[int]
}

/*
func (self *DeviceRemoteState) Unset() {
	self.CanShowRatingDialog.Unset()
	self.CanPromptIntroFunnel.Unset()
	self.ProvideControlMode.Unset()
	self.CanRefer.Unset()
	self.RouteLocal.Unset()
	self.BlockerEnabled.Unset()
	self.InitProvideSecretKeys.Unset()
	self.LoadProvideSecretKeys.Unset()
	self.ProvideMode.Unset()
	self.ProvideNetworkMode.Unset()
	self.ProvidePaused.Unset()
	self.Offline.Unset()
	self.VpnInterfaceWhileOffline.Unset()
	self.RemoveDestination.Unset()
	self.Destination.Unset()
	self.PerformanceProfile.Unset()
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

	// self.RefreshToken.Unset()
}
*/

/*
func (self *DeviceRemoteState) Merge(update *DeviceRemoteState) {
	self.CanShowRatingDialog.Merge(update.CanShowRatingDialog)
	self.CanPromptIntroFunnel.Merge(update.CanPromptIntroFunnel)
	self.ProvideControlMode.Merge(update.ProvideControlMode)
	self.AllowForeground.Merge(update.AllowForeground)
	self.CanRefer.Merge(update.CanRefer)
	self.RouteLocal.Merge(update.RouteLocal)
	self.BlockerEnabled.Merge(update.BlockerEnabled)
	self.InitProvideSecretKeys.Merge(update.InitProvideSecretKeys)
	self.LoadProvideSecretKeys.Merge(update.LoadProvideSecretKeys)
	self.ProvideMode.Merge(update.ProvideMode)
	self.ProvideNetworkMode.Merge(update.ProvideNetworkMode)
	self.ProvidePaused.Merge(update.ProvidePaused)
	self.Offline.Merge(update.Offline)
	self.VpnInterfaceWhileOffline.Merge(update.VpnInterfaceWhileOffline)
	self.RemoveDestination.Merge(update.RemoveDestination)
	self.Destination.Merge(update.Destination)
	self.PerformanceProfile.Merge(update.PerformanceProfile)
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

	// self.RefreshToken.Merge(update.RefreshToken)
}
*/

// DeviceRpcVersion is the wire-compatibility version of the device rpc gob
// structs. It is NOT the release/SDK version (see `Version`) and must never be
// tied to it: on the hosted/web path the two halves deploy on completely
// independent schedules — the browser runs DeviceRemote out of a cached
// sdk.wasm while DeviceLocal runs server side and redeploys continuously — so
// gating on the release version would reject every remote after every server
// deploy.
//
// BUMP ONLY when the gob wire shape changes INCOMPATIBLY:
//   - a field is renamed (gob matches by name, so the renamed field silently
//     decodes as its zero value: a feature dies with no error anywhere)
//   - a field's type changes (gob errors at decode)
//   - a field's MEANING changes while its name and type stay the same (gob
//     cannot see this at all)
//
// Adding or removing an optional field is tolerated by gob and normally does
// NOT require a bump. A new field that represents required settable state does:
// otherwise a new remote can report the setting as accepted while an old local
// silently ignores it. Version 2 establishes the transport-settings contract.
const DeviceRpcVersion = 2

//gomobile:noexport
type DeviceRemoteSyncRequest struct {
	// InstanceId is the device instance the remote expects to reach. Pairing is
	// by address, and an address can be reused (a stale process still bound to
	// the port, another app): the local rejects a sync from a remote built for
	// a different instance instead of silently applying its state and serving
	// its listeners. Zero skips the check (older remote).
	InstanceId connect.Id

	// RpcVersion is the device rpc wire version the remote was built against
	// (DeviceRpcVersion). The remote and the local are separately deployed
	// artifacts on the hosted path, and gob fails quietly across an
	// incompatible struct change (a renamed field decodes as zero), so the
	// local rejects a mismatched remote outright instead of half-applying a
	// misread state. Zero skips the check (older remote).
	RpcVersion int

	CanShowRatingDialogChangeListenerIds       []connect.Id
	CanPromptIntroFunnelChangeListenerIds      []connect.Id
	AllowForegroundChangeListenerIds           []connect.Id
	CanReferChangeListenerIds                  []connect.Id
	ProvideModeChangeListenerIds               []connect.Id
	ProvideChangeListenerIds                   []connect.Id
	ProvideControlModeChangeListenerIds        []connect.Id
	PerformanceProfileChangeListenerIds        []connect.Id
	ProviderIdentityChangeListenerIds          []connect.Id
	ProvidePausedChangeListenerIds             []connect.Id
	ProvideNetworkModeChangeListenerIds        []connect.Id
	OfflineChangeListenerIds                   []connect.Id
	VpnInterfaceWhileOfflineChangeListenerIds  []connect.Id
	ConnectChangeListenerIds                   []connect.Id
	RouteLocalChangeListenerIds                []connect.Id
	BlockerEnabledChangeListenerIds            []connect.Id
	ConnectLocationChangeListenerIds           []connect.Id
	DefaultLocationChangeListenerIds           []connect.Id
	ProvideSecretKeysListenerIds               []connect.Id
	TunnelChangeListenerIds                    []connect.Id
	ContractStatusChangeListenerIds            []connect.Id
	WindowStatusChangeListenerIds              []connect.Id
	BlockActionWindowChangeListenerIds         []connect.Id
	BlockStatsChangeListenerIds                []connect.Id
	BlockActionOverridesChangeListenerIds      []connect.Id
	TransportSettingsChangeListenerIds         []connect.Id
	ProviderTransportSettingsChangeListenerIds []connect.Id
	PacketStatsChangeListenerIds               []connect.Id
	EgressContractStatsChangeListenerIds       []connect.Id
	EgressContractDetailsChangeListenerIds     []connect.Id
	IngressContractStatsChangeListenerIds      []connect.Id
	IngressContractDetailsChangeListenerIds    []connect.Id
	DnsResolverSettingsChangeListenerIds       []connect.Id
	NetworkPeersChangeListenerIds              []connect.Id
	WindowMonitorEventListenerIds              map[connect.Id][]connect.Id
	State                                      DeviceRemoteState

	ProviderPacketStatsChangeListenerIds            []connect.Id
	ProviderEgressContractStatsChangeListenerIds    []connect.Id
	ProviderEgressContractDetailsChangeListenerIds  []connect.Id
	ProviderIngressContractStatsChangeListenerIds   []connect.Id
	ProviderIngressContractDetailsChangeListenerIds []connect.Id
}

//gomobile:noexport
type DeviceRemoteSyncResponse struct {
	Error string
	State DeviceRemoteState
	// DeviceGeneration identifies the hosted DeviceLocal instance; a change
	// across reconnects means the host recreated the device (see
	// deviceRpcSettings.DeviceGeneration).
	DeviceGeneration string
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

	Stable        bool
	StrongPrivacy bool

	// a trusted same-network peer destination; drives the multi client's
	// ProvideMode_Network allow-direct force (see DeviceLocal.SetDestination)
	NetworkPeer bool
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

		Stable:        connectLocation.Stable,
		StrongPrivacy: connectLocation.StrongPrivacy,

		NetworkPeer: connectLocation.NetworkPeer,
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

		Stable:        self.Stable,
		StrongPrivacy: self.StrongPrivacy,

		NetworkPeer: self.NetworkPeer,
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
	if self.CountryLocationId != nil {
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

// flattened types for the privacy block, packet stats, contract stats,
// dns, and network peers surface. The gomobile exportedList-based types
// (StringList, BlockActionOverrideList, ...) and *Id have unexported fields
// that gob drops, so they cannot cross the rpc (see the rpc note above).
// converters exist in both directions (sdk <-> rpc).
// note gob conflates empty and nil slices, so converters normalize.

func stringsFromStringList(list *StringList) []string {
	if list == nil {
		return nil
	}
	return list.getAll()
}

// always returns a non-nil list
func stringListFromStrings(values []string) *StringList {
	list := NewStringList()
	list.addAll(values...)
	return list
}

// preserves nil
func stringListFromStringsOrNil(values []string) *StringList {
	if values == nil {
		return nil
	}
	return stringListFromStrings(values)
}

// copies so that the rpc value never aliases a caller-owned value
func copyBlockOverride(blockOverride *BlockOverride) *BlockOverride {
	if blockOverride == nil {
		return nil
	}
	out := *blockOverride
	return &out
}

func copyRouteOverride(routeOverride *RouteOverride) *RouteOverride {
	if routeOverride == nil {
		return nil
	}
	out := *routeOverride
	return &out
}

//gomobile:noexport
type BlockActionOverrideRpc struct {
	OverrideId    connect.Id
	Hosts         []string
	AppIds        []string
	BlockOverride *BlockOverride
	RouteOverride *RouteOverride
}

// the override must have a non-nil `OverrideId`
func newBlockActionOverrideRpc(override *BlockActionOverride) *BlockActionOverrideRpc {
	return &BlockActionOverrideRpc{
		OverrideId:    override.OverrideId.toConnectId(),
		Hosts:         stringsFromStringList(override.Hosts),
		AppIds:        stringsFromStringList(override.AppIds),
		BlockOverride: copyBlockOverride(override.BlockOverride),
		RouteOverride: copyRouteOverride(override.RouteOverride),
	}
}

func (self *BlockActionOverrideRpc) toBlockActionOverride() *BlockActionOverride {
	if self == nil {
		return nil
	}
	return &BlockActionOverride{
		OverrideId:    newId(self.OverrideId),
		Hosts:         stringListFromStringsOrNil(self.Hosts),
		AppIds:        stringListFromStringsOrNil(self.AppIds),
		BlockOverride: copyBlockOverride(self.BlockOverride),
		RouteOverride: copyRouteOverride(self.RouteOverride),
	}
}

// skips overrides without an override id and replaces earlier overrides with
// the same id, mirroring `DeviceLocal.SetBlockActionOverrides`.
// always returns a non-nil slice with non-nil elements
// (gob cannot encode nil slice elements)
func newBlockActionOverridesRpc(overrides *BlockActionOverrideList) []*BlockActionOverrideRpc {
	overridesRpc := []*BlockActionOverrideRpc{}
	if overrides != nil {
		for _, override := range overrides.getAll() {
			if override == nil || override.OverrideId == nil {
				continue
			}
			overrideRpc := newBlockActionOverrideRpc(override)
			overridesRpc = removeBlockActionOverrideRpc(overridesRpc, overrideRpc.OverrideId)
			overridesRpc = append(overridesRpc, overrideRpc)
		}
	}
	return overridesRpc
}

func toBlockActionOverrideList(overridesRpc []*BlockActionOverrideRpc) *BlockActionOverrideList {
	overrides := NewBlockActionOverrideList()
	for _, overrideRpc := range overridesRpc {
		if overrideRpc == nil {
			continue
		}
		overrides.Add(overrideRpc.toBlockActionOverride())
	}
	return overrides
}

// returns a copy with the override with `overrideId` removed.
// the copy keeps mutations from aliasing a list held by the remote state
func removeBlockActionOverrideRpc(overridesRpc []*BlockActionOverrideRpc, overrideId connect.Id) []*BlockActionOverrideRpc {
	out := []*BlockActionOverrideRpc{}
	for _, overrideRpc := range overridesRpc {
		if overrideRpc == nil || overrideRpc.OverrideId == overrideId {
			continue
		}
		out = append(out, overrideRpc)
	}
	return out
}

//gomobile:noexport
type BlockActionRpc struct {
	BlockActionId connect.Id
	Time          int64
	Ips           []string
	Hosts         []string
	MatchedIps    []string
	MatchedHosts  []string
	Block         bool
	Local         bool
	OverrideId    *connect.Id
	BlockOverride *BlockOverride
	RouteOverride *RouteOverride
	PacketCount   int
	ByteCount     ByteCount
}

func newBlockActionRpc(blockAction *BlockAction) *BlockActionRpc {
	if blockAction == nil {
		return nil
	}
	blockActionRpc := &BlockActionRpc{
		Time:          blockAction.Time,
		Ips:           stringsFromStringList(blockAction.Ips),
		Hosts:         stringsFromStringList(blockAction.Hosts),
		MatchedIps:    stringsFromStringList(blockAction.MatchedIps),
		MatchedHosts:  stringsFromStringList(blockAction.MatchedHosts),
		Block:         blockAction.Block,
		Local:         blockAction.Local,
		BlockOverride: copyBlockOverride(blockAction.BlockOverride),
		RouteOverride: copyRouteOverride(blockAction.RouteOverride),
		PacketCount:   blockAction.PacketCount,
		ByteCount:     blockAction.ByteCount,
	}
	if blockAction.BlockActionId != nil {
		blockActionRpc.BlockActionId = blockAction.BlockActionId.toConnectId()
	}
	if blockAction.OverrideId != nil {
		overrideId := blockAction.OverrideId.toConnectId()
		blockActionRpc.OverrideId = &overrideId
	}
	return blockActionRpc
}

func (self *BlockActionRpc) toBlockAction() *BlockAction {
	if self == nil {
		return nil
	}
	blockAction := &BlockAction{
		BlockActionId: newId(self.BlockActionId),
		Time:          self.Time,
		Ips:           stringListFromStrings(self.Ips),
		Hosts:         stringListFromStrings(self.Hosts),
		MatchedIps:    stringListFromStrings(self.MatchedIps),
		MatchedHosts:  stringListFromStrings(self.MatchedHosts),
		Block:         self.Block,
		Local:         self.Local,
		BlockOverride: copyBlockOverride(self.BlockOverride),
		RouteOverride: copyRouteOverride(self.RouteOverride),
		PacketCount:   self.PacketCount,
		ByteCount:     self.ByteCount,
	}
	if self.OverrideId != nil {
		blockAction.OverrideId = newId(*self.OverrideId)
	}
	return blockAction
}

//gomobile:noexport
type BlockActionWindowRpc struct {
	BlockActions []*BlockActionRpc
}

func newBlockActionWindowRpc(blockActionWindow *BlockActionWindow) *BlockActionWindowRpc {
	if blockActionWindow == nil {
		return nil
	}
	windowRpc := &BlockActionWindowRpc{}
	if blockActionWindow.BlockActions != nil {
		for _, blockAction := range blockActionWindow.BlockActions.getAll() {
			if blockAction == nil {
				continue
			}
			windowRpc.BlockActions = append(windowRpc.BlockActions, newBlockActionRpc(blockAction))
		}
	}
	return windowRpc
}

func (self *BlockActionWindowRpc) toBlockActionWindow() *BlockActionWindow {
	if self == nil {
		return nil
	}
	blockActions := NewBlockActionList()
	for _, blockActionRpc := range self.BlockActions {
		if blockActionRpc == nil {
			continue
		}
		blockActions.Add(blockActionRpc.toBlockAction())
	}
	return &BlockActionWindow{
		BlockActions: blockActions,
	}
}

//gomobile:noexport
type OverrideLocalAppIdsRpc struct {
	Included []string
	Excluded []string
}

func newOverrideLocalAppIdsRpc(overrideLocalAppIds *OverrideLocalAppIds) *OverrideLocalAppIdsRpc {
	if overrideLocalAppIds == nil {
		return nil
	}
	return &OverrideLocalAppIdsRpc{
		Included: stringsFromStringList(overrideLocalAppIds.Included),
		Excluded: stringsFromStringList(overrideLocalAppIds.Excluded),
	}
}

func (self *OverrideLocalAppIdsRpc) toOverrideLocalAppIds() *OverrideLocalAppIds {
	if self == nil {
		return nil
	}
	return &OverrideLocalAppIds{
		Included: stringListFromStrings(self.Included),
		Excluded: stringListFromStrings(self.Excluded),
	}
}

//gomobile:noexport
type DnsResolverSettingsRpc struct {
	EnableRemoteDoh       bool
	EnableLocalDoh        bool
	EnableRemoteDns       bool
	EnableLocalDns        bool
	EnableFallback        bool
	DnsUpgradeMaskAddress string

	RemoteDohUrlsIpv4 []string
	RemoteDohUrlsIpv6 []string
	LocalDohUrlsIpv4  []string
	LocalDohUrlsIpv6  []string
	RemoteDnsIpv4     []string
	RemoteDnsIpv6     []string
	LocalDnsIpv4      []string
	LocalDnsIpv6      []string
}

func newDnsResolverSettingsRpc(dnsResolverSettings *DnsResolverSettings) *DnsResolverSettingsRpc {
	if dnsResolverSettings == nil {
		return nil
	}
	return &DnsResolverSettingsRpc{
		EnableRemoteDoh:       dnsResolverSettings.EnableRemoteDoh,
		EnableLocalDoh:        dnsResolverSettings.EnableLocalDoh,
		EnableRemoteDns:       dnsResolverSettings.EnableRemoteDns,
		EnableLocalDns:        dnsResolverSettings.EnableLocalDns,
		EnableFallback:        dnsResolverSettings.EnableFallback,
		DnsUpgradeMaskAddress: dnsResolverSettings.DnsUpgradeMaskAddress,
		RemoteDohUrlsIpv4:     stringsFromStringList(dnsResolverSettings.RemoteDohUrlsIpv4),
		RemoteDohUrlsIpv6:     stringsFromStringList(dnsResolverSettings.RemoteDohUrlsIpv6),
		LocalDohUrlsIpv4:      stringsFromStringList(dnsResolverSettings.LocalDohUrlsIpv4),
		LocalDohUrlsIpv6:      stringsFromStringList(dnsResolverSettings.LocalDohUrlsIpv6),
		RemoteDnsIpv4:         stringsFromStringList(dnsResolverSettings.RemoteDnsIpv4),
		RemoteDnsIpv6:         stringsFromStringList(dnsResolverSettings.RemoteDnsIpv6),
		LocalDnsIpv4:          stringsFromStringList(dnsResolverSettings.LocalDnsIpv4),
		LocalDnsIpv6:          stringsFromStringList(dnsResolverSettings.LocalDnsIpv6),
	}
}

// the string lists are always non-nil, matching `DeviceLocal.GetDnsResolverSettings`
func (self *DnsResolverSettingsRpc) toDnsResolverSettings() *DnsResolverSettings {
	if self == nil {
		return nil
	}
	return &DnsResolverSettings{
		EnableRemoteDoh:       self.EnableRemoteDoh,
		EnableLocalDoh:        self.EnableLocalDoh,
		EnableRemoteDns:       self.EnableRemoteDns,
		EnableLocalDns:        self.EnableLocalDns,
		EnableFallback:        self.EnableFallback,
		DnsUpgradeMaskAddress: dnsUpgradeMaskAddress(self.DnsUpgradeMaskAddress),
		RemoteDohUrlsIpv4:     stringListFromStrings(self.RemoteDohUrlsIpv4),
		RemoteDohUrlsIpv6:     stringListFromStrings(self.RemoteDohUrlsIpv6),
		LocalDohUrlsIpv4:      stringListFromStrings(self.LocalDohUrlsIpv4),
		LocalDohUrlsIpv6:      stringListFromStrings(self.LocalDohUrlsIpv6),
		RemoteDnsIpv4:         stringListFromStrings(self.RemoteDnsIpv4),
		RemoteDnsIpv6:         stringListFromStrings(self.RemoteDnsIpv6),
		LocalDnsIpv4:          stringListFromStrings(self.LocalDnsIpv4),
		LocalDnsIpv6:          stringListFromStrings(self.LocalDnsIpv6),
	}
}

//gomobile:noexport
type DeviceRemoteTransferPath struct {
	SourceId      *connect.Id
	DestinationId *connect.Id
	StreamId      *connect.Id
}

func newDeviceRemoteTransferPath(transferPath *TransferPath) *DeviceRemoteTransferPath {
	if transferPath == nil {
		return nil
	}
	deviceRemoteTransferPath := &DeviceRemoteTransferPath{}
	if transferPath.SourceId != nil {
		id := transferPath.SourceId.toConnectId()
		deviceRemoteTransferPath.SourceId = &id
	}
	if transferPath.DestinationId != nil {
		id := transferPath.DestinationId.toConnectId()
		deviceRemoteTransferPath.DestinationId = &id
	}
	if transferPath.StreamId != nil {
		id := transferPath.StreamId.toConnectId()
		deviceRemoteTransferPath.StreamId = &id
	}
	return deviceRemoteTransferPath
}

func (self *DeviceRemoteTransferPath) toTransferPath() *TransferPath {
	if self == nil {
		return nil
	}
	transferPath := &TransferPath{}
	if self.SourceId != nil {
		transferPath.SourceId = newId(*self.SourceId)
	}
	if self.DestinationId != nil {
		transferPath.DestinationId = newId(*self.DestinationId)
	}
	if self.StreamId != nil {
		transferPath.StreamId = newId(*self.StreamId)
	}
	return transferPath
}

//gomobile:noexport
type ContractDetailsRpc struct {
	ContractId            *connect.Id
	ContractUsedByteCount ByteCount
	ContractByteCount     ByteCount
	ContractBitRate       int
	ContractTransferPath  *DeviceRemoteTransferPath

	Status string
}

func newContractDetailsRpc(contractDetails *ContractDetails) *ContractDetailsRpc {
	if contractDetails == nil {
		return nil
	}
	contractDetailsRpc := &ContractDetailsRpc{
		ContractUsedByteCount: contractDetails.ContractUsedByteCount,
		ContractByteCount:     contractDetails.ContractByteCount,
		ContractBitRate:       contractDetails.ContractBitRate,
		ContractTransferPath:  newDeviceRemoteTransferPath(contractDetails.ContractTransferPath),

		Status: contractDetails.Status,
	}
	if contractDetails.ContractId != nil {
		id := contractDetails.ContractId.toConnectId()
		contractDetailsRpc.ContractId = &id
	}
	return contractDetailsRpc
}

func (self *ContractDetailsRpc) toContractDetails() *ContractDetails {
	if self == nil {
		return nil
	}
	contractDetails := &ContractDetails{
		ContractUsedByteCount: self.ContractUsedByteCount,
		ContractByteCount:     self.ContractByteCount,
		ContractBitRate:       self.ContractBitRate,
		ContractTransferPath:  self.ContractTransferPath.toTransferPath(),

		Status: self.Status,
	}
	if self.ContractId != nil {
		contractDetails.ContractId = newId(*self.ContractId)
	}
	return contractDetails
}

// always returns a non-nil slice with non-nil elements
// (gob cannot encode nil slice elements)
func newContractDetailsListRpc(contractDetailsList *ContractDetailsList) []*ContractDetailsRpc {
	contractDetailsRpcs := []*ContractDetailsRpc{}
	if contractDetailsList != nil {
		for _, contractDetails := range contractDetailsList.getAll() {
			if contractDetails == nil {
				continue
			}
			contractDetailsRpcs = append(contractDetailsRpcs, newContractDetailsRpc(contractDetails))
		}
	}
	return contractDetailsRpcs
}

func toContractDetailsList(contractDetailsRpcs []*ContractDetailsRpc) *ContractDetailsList {
	contractDetailsList := NewContractDetailsList()
	for _, contractDetailsRpc := range contractDetailsRpcs {
		if contractDetailsRpc == nil {
			continue
		}
		contractDetailsList.Add(contractDetailsRpc.toContractDetails())
	}
	return contractDetailsList
}

//gomobile:noexport
type NetworkPeerRpc struct {
	ClientId       connect.Id
	ProvideEnabled bool
	Principal      string
	Roles          []string
	DeviceSpec     string
	DeviceName     string
}

func newNetworkPeerRpc(networkPeer *NetworkPeer) *NetworkPeerRpc {
	if networkPeer == nil {
		return nil
	}
	networkPeerRpc := &NetworkPeerRpc{
		ProvideEnabled: networkPeer.ProvideEnabled,
		Principal:      networkPeer.Principal,
		Roles:          stringsFromStringList(networkPeer.Roles),
		DeviceSpec:     networkPeer.DeviceSpec,
		DeviceName:     networkPeer.DeviceName,
	}
	if networkPeer.ClientId != nil {
		networkPeerRpc.ClientId = networkPeer.ClientId.toConnectId()
	}
	return networkPeerRpc
}

func (self *NetworkPeerRpc) toNetworkPeer() *NetworkPeer {
	if self == nil {
		return nil
	}
	return &NetworkPeer{
		ClientId:       newId(self.ClientId),
		ProvideEnabled: self.ProvideEnabled,
		Principal:      self.Principal,
		Roles:          stringListFromStrings(self.Roles),
		DeviceSpec:     self.DeviceSpec,
		DeviceName:     self.DeviceName,
	}
}

//gomobile:noexport
type NetworkPeersRpc struct {
	Connected         []*NetworkPeerRpc
	DisconnectedCount int
}

func newNetworkPeersRpc(networkPeers *NetworkPeers) *NetworkPeersRpc {
	if networkPeers == nil {
		return nil
	}
	networkPeersRpc := &NetworkPeersRpc{
		DisconnectedCount: networkPeers.DisconnectedCount,
	}
	if networkPeers.Connected != nil {
		for _, networkPeer := range networkPeers.Connected.getAll() {
			if networkPeer == nil {
				continue
			}
			networkPeersRpc.Connected = append(networkPeersRpc.Connected, newNetworkPeerRpc(networkPeer))
		}
	}
	return networkPeersRpc
}

func (self *NetworkPeersRpc) toNetworkPeers() *NetworkPeers {
	if self == nil {
		return nil
	}
	connected := NewNetworkPeerList()
	for _, networkPeerRpc := range self.Connected {
		if networkPeerRpc == nil {
			continue
		}
		connected.Add(networkPeerRpc.toNetworkPeer())
	}
	return &NetworkPeers{
		Connected:         connected,
		DisconnectedCount: self.DisconnectedCount,
	}
}

//gomobile:noexport
type ProviderIdentityRpc struct {
	ClientId  connect.Id
	PublicKey []byte
}

func newProviderIdentityRpc(providerIdentity *ProviderIdentity) *ProviderIdentityRpc {
	if providerIdentity == nil {
		return nil
	}
	providerIdentityRpc := &ProviderIdentityRpc{
		PublicKey: providerIdentity.PublicKey,
	}
	if providerIdentity.ClientId != nil {
		providerIdentityRpc.ClientId = providerIdentity.ClientId.toConnectId()
	}
	return providerIdentityRpc
}

func (self *ProviderIdentityRpc) toProviderIdentity() *ProviderIdentity {
	if self == nil {
		return nil
	}
	return &ProviderIdentity{
		ClientId:  newId(self.ClientId),
		PublicKey: self.PublicKey,
	}
}

// the rpc payload/event for the provider identities getter and change event

//gomobile:noexport
type DeviceProviderIdentities struct {
	ProviderIdentities []*ProviderIdentityRpc
}

func newDeviceProviderIdentities(providerIdentities *ProviderIdentityList) *DeviceProviderIdentities {
	deviceProviderIdentities := &DeviceProviderIdentities{}
	if providerIdentities != nil {
		for _, providerIdentity := range providerIdentities.getAll() {
			if providerIdentity == nil {
				continue
			}
			deviceProviderIdentities.ProviderIdentities = append(
				deviceProviderIdentities.ProviderIdentities,
				newProviderIdentityRpc(providerIdentity),
			)
		}
	}
	return deviceProviderIdentities
}

func (self *DeviceProviderIdentities) toProviderIdentityList() *ProviderIdentityList {
	providerIdentities := NewProviderIdentityList()
	if self != nil {
		for _, providerIdentityRpc := range self.ProviderIdentities {
			if providerIdentityRpc == nil {
				continue
			}
			providerIdentities.Add(providerIdentityRpc.toProviderIdentity())
		}
	}
	return providerIdentities
}

// nil-able reply/event wrappers (net/rpc rejects nil args and replies)

//gomobile:noexport
type DevicePublicIdentityKey struct {
	PublicKey []byte
}

//gomobile:noexport
type DeviceRemoteBlockStats struct {
	BlockStats *BlockStats
}

//gomobile:noexport
type DeviceRemoteBlockActionWindow struct {
	BlockActionWindow *BlockActionWindowRpc
}

//gomobile:noexport
type DeviceRemoteBlockActionOverrides struct {
	BlockActionOverrides []*BlockActionOverrideRpc
}

//gomobile:noexport
type DeviceRemoteLocalOverrideAppIds struct {
	LocalOverrideAppIds *OverrideLocalAppIdsRpc
}

//gomobile:noexport
type DeviceRemotePacketStats struct {
	PacketStats *PacketStatsRpc
}

//gomobile:noexport
type TransportPacketStatsRpc struct {
	TransportType TransportType
	Stats         *PacketStatsRpc
}

// PacketStatsRpc is the explicit gob mirror. TransportPacketStatsList keeps
// its gomobile-safe slice unexported, so embedding the public PacketStats
// directly would silently erase the carrier breakdown over RPC.
//
//gomobile:noexport
type PacketStatsRpc struct {
	RemoteEgressPacketCount  int64
	RemoteEgressByteCount    ByteCount
	RemoteIngressPacketCount int64
	RemoteIngressByteCount   ByteCount
	LocalEgressPacketCount   int64
	LocalEgressByteCount     ByteCount
	LocalIngressPacketCount  int64
	LocalIngressByteCount    ByteCount
	BlockEgressPacketCount   int64
	BlockEgressByteCount     ByteCount
	BlockIngressPacketCount  int64
	BlockIngressByteCount    ByteCount
	TransportStats           []*TransportPacketStatsRpc
}

func newPacketStatsRpc(stats *PacketStats, includeTransportStats bool) *PacketStatsRpc {
	if stats == nil {
		return nil
	}
	rpc := &PacketStatsRpc{
		RemoteEgressPacketCount:  stats.RemoteEgressPacketCount,
		RemoteEgressByteCount:    stats.RemoteEgressByteCount,
		RemoteIngressPacketCount: stats.RemoteIngressPacketCount,
		RemoteIngressByteCount:   stats.RemoteIngressByteCount,
		LocalEgressPacketCount:   stats.LocalEgressPacketCount,
		LocalEgressByteCount:     stats.LocalEgressByteCount,
		LocalIngressPacketCount:  stats.LocalIngressPacketCount,
		LocalIngressByteCount:    stats.LocalIngressByteCount,
		BlockEgressPacketCount:   stats.BlockEgressPacketCount,
		BlockEgressByteCount:     stats.BlockEgressByteCount,
		BlockIngressPacketCount:  stats.BlockIngressPacketCount,
		BlockIngressByteCount:    stats.BlockIngressByteCount,
	}
	if includeTransportStats && stats.TransportStats != nil {
		for _, transportStats := range stats.TransportStats.getAll() {
			if transportStats == nil {
				continue
			}
			rpc.TransportStats = append(rpc.TransportStats, &TransportPacketStatsRpc{
				TransportType: transportStats.TransportType,
				Stats:         newPacketStatsRpc(transportStats.Stats, false),
			})
		}
	}
	return rpc
}

func (self *PacketStatsRpc) toPacketStats(includeTransportStats bool) *PacketStats {
	if self == nil {
		return nil
	}
	stats := &PacketStats{
		RemoteEgressPacketCount:  self.RemoteEgressPacketCount,
		RemoteEgressByteCount:    self.RemoteEgressByteCount,
		RemoteIngressPacketCount: self.RemoteIngressPacketCount,
		RemoteIngressByteCount:   self.RemoteIngressByteCount,
		LocalEgressPacketCount:   self.LocalEgressPacketCount,
		LocalEgressByteCount:     self.LocalEgressByteCount,
		LocalIngressPacketCount:  self.LocalIngressPacketCount,
		LocalIngressByteCount:    self.LocalIngressByteCount,
		BlockEgressPacketCount:   self.BlockEgressPacketCount,
		BlockEgressByteCount:     self.BlockEgressByteCount,
		BlockIngressPacketCount:  self.BlockIngressPacketCount,
		BlockIngressByteCount:    self.BlockIngressByteCount,
	}
	if includeTransportStats {
		stats.TransportStats = NewTransportPacketStatsList()
		for _, transportStats := range self.TransportStats {
			if transportStats == nil {
				continue
			}
			stats.TransportStats.Add(&TransportPacketStats{
				TransportType: transportStats.TransportType,
				Stats:         transportStats.Stats.toPacketStats(false),
			})
		}
	}
	return stats
}

//gomobile:noexport
type DeviceRemoteContractStats struct {
	ContractStats *ContractStats
}

//gomobile:noexport
type DeviceRemoteContractDetails struct {
	ContractDetails *ContractDetailsRpc
}

//gomobile:noexport
type DeviceRemoteContractDetailsList struct {
	ContractDetailsList []*ContractDetailsRpc
	// gob cannot distinguish a nil list from an empty one; set by the
	// provider getters when the device has no provider
	Nil bool
}

//gomobile:noexport
type DeviceRemoteDnsResolverSettings struct {
	DnsResolverSettings *DnsResolverSettingsRpc
}

//gomobile:noexport
type TransportModePriorityRpc struct {
	Mode     TransportMode
	Priority int
}

//gomobile:noexport
type TransportSettingsRpc struct {
	Mode               TransportMode
	AutoModePriorities []*TransportModePriorityRpc
}

func newTransportSettingsRpc(settings *TransportSettings, provider bool) *TransportSettingsRpc {
	settings = normalizeTransportSettings(settings, provider)
	priorities := make([]*TransportModePriorityRpc, 0, settings.AutoModePriorities.Len())
	for _, priority := range settings.AutoModePriorities.getAll() {
		if priority != nil {
			priorities = append(priorities, &TransportModePriorityRpc{
				Mode:     priority.Mode,
				Priority: priority.Priority,
			})
		}
	}
	return &TransportSettingsRpc{
		Mode:               settings.Mode,
		AutoModePriorities: priorities,
	}
}

func (self *TransportSettingsRpc) toTransportSettings(provider bool) *TransportSettings {
	if self == nil {
		return normalizeTransportSettings(nil, provider)
	}
	priorities := NewTransportModePriorityList()
	for _, priority := range self.AutoModePriorities {
		if priority != nil {
			priorities.Add(&TransportModePriority{
				Mode:     priority.Mode,
				Priority: priority.Priority,
			})
		}
	}
	return normalizeTransportSettings(&TransportSettings{
		Mode:               self.Mode,
		AutoModePriorities: priorities,
	}, provider)
}

//gomobile:noexport
type DeviceRemoteTransportSettingsRpc struct {
	TransportSettings *TransportSettingsRpc
	TransportStatus   *TransportStatusRpc
}

//gomobile:noexport
type TransportStatusRpc struct {
	AutoDegraded      bool
	AutoEligibleModes []TransportMode
	AutoConstraint    string
}

func newTransportStatusRpc(status *TransportStatus) *TransportStatusRpc {
	if status == nil {
		return nil
	}
	eligibleModes := []TransportMode{}
	if status.AutoEligibleModes != nil {
		eligibleModes = append(eligibleModes, status.AutoEligibleModes.getAll()...)
	}
	return &TransportStatusRpc{
		AutoDegraded:      status.AutoDegraded,
		AutoEligibleModes: eligibleModes,
		AutoConstraint:    status.AutoConstraint,
	}
}

func (self *TransportStatusRpc) toTransportStatus() *TransportStatus {
	if self == nil {
		return nil
	}
	eligibleModes := NewStringList()
	eligibleModes.addAll(self.AutoEligibleModes...)
	return &TransportStatus{
		AutoDegraded:      self.AutoDegraded,
		AutoEligibleModes: eligibleModes,
		AutoConstraint:    self.AutoConstraint,
	}
}

//gomobile:noexport
type DeviceRemoteNetworkPeers struct {
	NetworkPeers *NetworkPeersRpc
}

// reliability rpc payloads
//
// `ReliabilitySettings` and `ReliabilityMetrics` are flat gomobile-safe
// structs (bools, int32/int64 millis, float64) with every field exported, so
// they cross the gob wire whole inside nil-able wrappers, mirroring the
// `DevicePerformanceProfile` convention. `Exit` and `DestinationExit` carry
// an `*Id`, whose uuid bytes are unexported and would be silently dropped by
// gob, so they cross via hand mirrors with `connect.Id` like every other rpc
// payload id (see device_rpc_reliability_test.go for the completeness
// guards, which follow the device_rpc_mirror_test.go convention).

func cloneReliabilitySettings(reliabilitySettings *ReliabilitySettings) *ReliabilitySettings {
	if reliabilitySettings == nil {
		return nil
	}
	out := *reliabilitySettings
	return &out
}

//gomobile:noexport
type DeviceRemoteReliabilitySettingsRpc struct {
	ReliabilitySettings *ReliabilitySettings
}

//gomobile:noexport
type DeviceRemoteReliabilityMetricsRpc struct {
	ReliabilityMetrics *ReliabilityMetrics
}

//gomobile:noexport
type ExitRpc struct {
	ClientId         connect.Id
	WindowType       string
	Warning          bool
	Quarantined      bool
	WarningCause     string
	Done             bool
	P2pOnly          bool
	FlowCount        int32
	DialFailureCount int32
	Tier             int32
	EffectiveTier    int32
	Proven           bool
	ProbeAgeSeconds  int64
}

func newExitRpc(exit *Exit) *ExitRpc {
	if exit == nil {
		return nil
	}
	exitRpc := &ExitRpc{
		WindowType:       exit.WindowType,
		Warning:          exit.Warning,
		Quarantined:      exit.Quarantined,
		WarningCause:     exit.WarningCause,
		Done:             exit.Done,
		P2pOnly:          exit.P2pOnly,
		FlowCount:        exit.FlowCount,
		DialFailureCount: exit.DialFailureCount,
		Tier:             exit.Tier,
		EffectiveTier:    exit.EffectiveTier,
		Proven:           exit.Proven,
		ProbeAgeSeconds:  exit.ProbeAgeSeconds,
	}
	if exit.ClientId != nil {
		exitRpc.ClientId = exit.ClientId.toConnectId()
	}
	return exitRpc
}

func (self *ExitRpc) toExit() *Exit {
	if self == nil {
		return nil
	}
	return &Exit{
		ClientId:         newId(self.ClientId),
		WindowType:       self.WindowType,
		Warning:          self.Warning,
		Quarantined:      self.Quarantined,
		WarningCause:     self.WarningCause,
		Done:             self.Done,
		P2pOnly:          self.P2pOnly,
		FlowCount:        self.FlowCount,
		DialFailureCount: self.DialFailureCount,
		Tier:             self.Tier,
		EffectiveTier:    self.EffectiveTier,
		Proven:           self.Proven,
		ProbeAgeSeconds:  self.ProbeAgeSeconds,
	}
}

// always returns a non-nil slice with non-nil elements
// (gob cannot encode nil slice elements)
func newExitListRpc(exits *ExitList) []*ExitRpc {
	exitsRpc := []*ExitRpc{}
	if exits != nil {
		for _, exit := range exits.getAll() {
			if exit == nil {
				continue
			}
			exitsRpc = append(exitsRpc, newExitRpc(exit))
		}
	}
	return exitsRpc
}

func toExitList(exitsRpc []*ExitRpc) *ExitList {
	exits := NewExitList()
	for _, exitRpc := range exitsRpc {
		if exitRpc == nil {
			continue
		}
		exits.Add(exitRpc.toExit())
	}
	return exits
}

//gomobile:noexport
type DeviceRemoteExitListRpc struct {
	Exits []*ExitRpc
}

//gomobile:noexport
type DestinationExitRpc struct {
	DestinationIp string
	ClientId      connect.Id
	FlowCount     int32
}

func newDestinationExitRpc(destinationExit *DestinationExit) *DestinationExitRpc {
	if destinationExit == nil {
		return nil
	}
	destinationExitRpc := &DestinationExitRpc{
		DestinationIp: destinationExit.DestinationIp,
		FlowCount:     destinationExit.FlowCount,
	}
	if destinationExit.ClientId != nil {
		destinationExitRpc.ClientId = destinationExit.ClientId.toConnectId()
	}
	return destinationExitRpc
}

func (self *DestinationExitRpc) toDestinationExit() *DestinationExit {
	if self == nil {
		return nil
	}
	return &DestinationExit{
		DestinationIp: self.DestinationIp,
		ClientId:      newId(self.ClientId),
		FlowCount:     self.FlowCount,
	}
}

// always returns a non-nil slice with non-nil elements
// (gob cannot encode nil slice elements)
func newDestinationExitListRpc(destinationExits *DestinationExitList) []*DestinationExitRpc {
	destinationExitsRpc := []*DestinationExitRpc{}
	if destinationExits != nil {
		for _, destinationExit := range destinationExits.getAll() {
			if destinationExit == nil {
				continue
			}
			destinationExitsRpc = append(destinationExitsRpc, newDestinationExitRpc(destinationExit))
		}
	}
	return destinationExitsRpc
}

func toDestinationExitList(destinationExitsRpc []*DestinationExitRpc) *DestinationExitList {
	destinationExits := NewDestinationExitList()
	for _, destinationExitRpc := range destinationExitsRpc {
		if destinationExitRpc == nil {
			continue
		}
		destinationExits.Add(destinationExitRpc.toDestinationExit())
	}
	return destinationExits
}

//gomobile:noexport
type DeviceRemoteDestinationExitListRpc struct {
	DestinationExits []*DestinationExitRpc
}

// StallExit is the only reliability control that takes more than one
// argument, and net/rpc takes exactly one, so the pair travels as a struct.
//
//gomobile:noexport
type DeviceRemoteStallExitRpc struct {
	ClientId connect.Id
	Stalled  bool
}

// `ProbeSuiteConfig` has only exported scalar fields, so it gobs as-is; the
// wrapper is here so that a nil config (meaning "use the local default") can
// cross, since `rpcCall` panics on a nil arg.
//
//gomobile:noexport
type DeviceRemoteProbeSuiteConfigRpc struct {
	Config *ProbeSuiteConfig
}

// The nil config is carried, not resolved, on this side. Normalization lives
// in `DeviceLocal.StartProbeSuite` so that the rpc path and the direct
// gomobile/cgo callers get identical treatment -- a guard here would have
// covered only the remote path, leaving
// `urnet_device_local_start_probe_suite(handle, NULL)` to panic a spawned
// goroutine inside the privileged service.

// always returns a non-nil slice with non-nil elements
// (gob cannot encode nil slice elements)
//
// `ProbeResult` needs no rpc mirror type: unlike `Exit`, every field is an
// exported scalar, so gob encodes it directly. Only the enclosing
// `ProbeResultList` needs unwrapping, because `exportedList` keeps its
// `values` slice unexported.
func newProbeResultListRpc(results *ProbeResultList) []*ProbeResult {
	resultsRpc := []*ProbeResult{}
	if results != nil {
		for _, result := range results.getAll() {
			if result == nil {
				continue
			}
			resultsRpc = append(resultsRpc, result)
		}
	}
	return resultsRpc
}

func toProbeResultList(resultsRpc []*ProbeResult) *ProbeResultList {
	results := NewProbeResultList()
	for _, resultRpc := range resultsRpc {
		if resultRpc == nil {
			continue
		}
		results.Add(resultRpc)
	}
	return results
}

//gomobile:noexport
type DeviceRemoteProbeResultListRpc struct {
	Results []*ProbeResult
}

// rpc wrappers

type rpcClient = rpcClientWithTimeout

// type rpcClient = rpc.Client

type rpcClientWithTimeout struct {
	ctx         context.Context
	log         connect.Logger
	timeout     time.Duration
	closeClient func() error
	client      *rpc.Client
}

func (self *rpcClientWithTimeout) Call(serviceMethod string, args any, reply any) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go connect.HandleError(func() {
		defer cancel()
		select {
		case <-ctx.Done():
		case <-time.After(self.timeout):
			self.closeClient()
		}
	}, cancel)
	return self.client.Call(serviceMethod, args, reply)
}

// notifyBlocking delivers a fire-and-forget reverse rpc and blocks until it
// completes. A slow app (e.g. its callback buffer is full) applies back pressure
// here rather than failing — there is no per-call timeout, so a transiently slow
// app never reports a failure that would tear down the reverse rpc and flap the
// tunnel. Liveness is the transport's job: a dead or suspended app is reaped by
// the keepalive (~KeepAliveTimeout*(KeepAliveRetryCount+1)), which breaks the
// connection and surfaces here as a real error. Returns an error only on a real
// transport failure (a dead reverse rpc is a dead tunnel).
func (self *rpcClientWithTimeout) notifyBlocking(serviceMethod string, args any) error {
	var void RpcVoid
	return self.client.Call(serviceMethod, args, &void)
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
	service.log.Infof("[rpc]%s", name)
	err := service.Call(name, arg, &void)
	if err != nil {
		service.log.Infof("[rpc]%s err = %s", name, err)
		cleanup()
	}
	return err
}

// rpcCallVoidAllowMissingMethod preserves a live connection when an older
// peer does not yet expose an optional method. net/rpc reports that condition
// as an application-level ServerError; every transport error and every other
// server error retains the ordinary teardown behavior.
func rpcCallVoidAllowMissingMethod(service *rpcClient, name string, arg any, cleanup func()) error {
	if arg == nil {
		panic("rpc cannot have nil args")
	}
	var void RpcVoid
	service.log.Infof("[rpc]%s", name)
	err := service.Call(name, arg, &void)
	if err != nil {
		service.log.Infof("[rpc]%s err = %s", name, err)
		if !rpcMissingMethodError(err) {
			cleanup()
		}
	}
	return err
}

func rpcMissingMethodError(err error) bool {
	var serverError rpc.ServerError
	return errors.As(err, &serverError) &&
		strings.HasPrefix(string(serverError), "rpc: can't find method ")
}

// rpcCallHttpVoid keeps application-level rejections (body/concurrency limits)
// scoped to the HTTP request. net/rpc reports those as rpc.ServerError; only a
// transport failure should tear down the otherwise healthy tunnel RPC session.
func rpcCallHttpVoid(service *rpcClient, name string, arg any, cleanup func()) error {
	if arg == nil {
		panic("rpc cannot have nil args")
	}
	var void RpcVoid
	service.log.Infof("[rpc]%s", name)
	err := service.Call(name, arg, &void)
	if err != nil {
		service.log.Infof("[rpc]%s err = %s", name, err)
		if httpRpcErrorRequiresCleanup(err) {
			cleanup()
		}
	}
	return err
}

func httpRpcErrorRequiresCleanup(err error) bool {
	var serverError rpc.ServerError
	return !errors.As(err, &serverError)
}

func rpcCallNoArgVoid(service *rpcClient, name string, cleanup func()) error {
	var noarg RpcNoArg
	var void RpcVoid
	service.log.Infof("[rpc]%s", name)
	err := service.Call(name, noarg, &void)
	if err != nil {
		service.log.Infof("[rpc]%s err = %s", name, err)
		cleanup()
	}
	return err
}

func rpcCallNoArg[T any](service *rpcClient, name string, cleanup func()) (T, error) {
	var noarg RpcNoArg
	var r T
	service.log.Infof("[rpc]%s", name)
	err := service.Call(name, noarg, &r)
	if err != nil {
		service.log.Infof("[rpc]%s err = %s", name, err)
		cleanup()
	}
	return r, err
}

func rpcCall[T any](service *rpcClient, name string, arg any, cleanup func()) (T, error) {
	if arg == nil {
		panic("rpc cannot have nil args")
	}
	var r T
	service.log.Infof("[rpc]%s", name)
	err := service.Call(name, arg, &r)
	if err != nil {
		service.log.Infof("[rpc]%s err = %s", name, err)
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
	return newDeviceRemoteHttpResponseWithLimit(
		requestId,
		bodyBytes,
		err,
		deviceRpcDefaultHttpMaxBodyBytes,
	)
}

func newDeviceRemoteHttpResponseWithLimit(requestId connect.Id, bodyBytes []byte, err error, maxBodyBytes int) *DeviceRemoteHttpResponse {
	if maxBodyBytes <= 0 {
		maxBodyBytes = deviceRpcDefaultHttpMaxBodyBytes
	}
	if maxBodyBytes < len(bodyBytes) {
		bodyBytes = nil
		if err == nil {
			err = fmt.Errorf("device rpc http response exceeds %d-byte limit", maxBodyBytes)
		}
	}
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
	listener    deviceRpcListener
}

func newDeviceLocalRpcManagerWithDefaults(
	ctx context.Context,
	deviceLocal *DeviceLocal,
) *deviceLocalRpcManager {
	settings := defaultDeviceRpcSettings()
	// follow the device logging config (see `DeviceLocalSettings.DisableLogging`)
	settings.DisableLogging = deviceLocal.settings.DisableLogging
	settings.ClientSettings.Log = deviceLocal.settings.ClientSettings.Log
	listener := NewWebsocketDeviceRpcListener(settings.Address, "", "", settings)
	return newDeviceLocalRpcManager(ctx, deviceLocal, settings, listener)
}

func newDeviceLocalRpcManager(
	ctx context.Context,
	deviceLocal *DeviceLocal,
	settings *deviceRpcSettings,
	listener deviceRpcListener,
) *deviceLocalRpcManager {
	cancelCtx, cancel := context.WithCancel(ctx)

	deviceLocalRpcManager := &deviceLocalRpcManager{
		ctx:         cancelCtx,
		cancel:      cancel,
		deviceLocal: deviceLocal,
		settings:    settings,
		listener:    listener,
	}

	go connect.HandleError(deviceLocalRpcManager.run, cancel)
	return deviceLocalRpcManager
}

func (self *deviceLocalRpcManager) run() {
	defer self.listener.Close()

	lastAcceptError := ""
	for {
		select {
		case <-self.ctx.Done():
			return
		default:
		}

		// Recreate the pace for every Accept. Reusing one deadline means every
		// error after the first interval receives an already-closed channel and
		// hot-spins forever.
		acceptReconnect := connect.NewPacedReconnect(self.settings.RpcReconnectTimeout)
		forwardConn, reverseConn, err := self.listener.Accept(self.ctx)
		if err != nil {
			select {
			case <-self.ctx.Done():
				return
			default:
			}
			acceptError := err.Error()
			if acceptError != lastAcceptError {
				self.deviceLocal.log.Infof("[dlrcp]accept err = %s", err)
				lastAcceptError = acceptError
			}
			select {
			case <-self.ctx.Done():
				return
			case <-acceptReconnect.After():
				continue
			}
		}
		lastAcceptError = ""

		// each connection manages its own lifecycle; the rpc closes its
		// connection when its context is cancelled
		newDeviceLocalRpc(
			self.ctx,
			forwardConn,
			reverseConn,
			self.deviceLocal,
			self.settings,
		)
	}
}

func (self *deviceLocalRpcManager) Close() {
	self.cancel()
	// close the listener synchronously so the port is released before a
	// replacement listener (e.g. from DeviceLocal.SetRpcServer) binds it
	self.listener.Close()
}

// rpc are called on a single go routine

//gomobile:noexport
type DeviceLocalRpc struct {
	ctx    context.Context
	cancel context.CancelFunc

	// conn is the forward stream on which DeviceLocalRpc methods are served;
	// reverseConn is the reverse stream used as the DeviceRemoteRpc client
	conn                  net.Conn
	reverseConn           net.Conn
	deviceLocal           *DeviceLocal
	egressSecurityPolicy  securityPolicy
	ingressSecurityPolicy securityPolicy
	settings              *deviceRpcSettings

	stateLock sync.Mutex

	canShowRatingDialogChangeListenerIds       map[connect.Id]bool
	canPromptIntroFunnelChangeListenerIds      map[connect.Id]bool
	allowForegroundChangeListenerIds           map[connect.Id]bool
	canReferChangeListenerIds                  map[connect.Id]bool
	provideModeChangeListenerIds               map[connect.Id]bool
	provideChangeListenerIds                   map[connect.Id]bool
	provideControlModeChangeListenerIds        map[connect.Id]bool
	performanceProfileChangeListenerIds        map[connect.Id]bool
	providerIdentityChangeListenerIds          map[connect.Id]bool
	providePausedChangeListenerIds             map[connect.Id]bool
	provideNetworkModeChangeListenerIds        map[connect.Id]bool
	offlineChangeListenerIds                   map[connect.Id]bool
	vpnInterfaceWhileOfflineChangeListenerIds  map[connect.Id]bool
	connectChangeListenerIds                   map[connect.Id]bool
	routeLocalChangeListenerIds                map[connect.Id]bool
	blockerEnabledChangeListenerIds            map[connect.Id]bool
	connectLocationChangeListenerIds           map[connect.Id]bool
	defaultLocationChangeListenerIds           map[connect.Id]bool
	provideSecretKeysListenerIds               map[connect.Id]bool
	tunnelChangeListenerIds                    map[connect.Id]bool
	contractStatusChangeListenerIds            map[connect.Id]bool
	windowStatusChangeListenerIds              map[connect.Id]bool
	blockActionWindowChangeListenerIds         map[connect.Id]bool
	blockStatsChangeListenerIds                map[connect.Id]bool
	blockActionOverridesChangeListenerIds      map[connect.Id]bool
	transportSettingsChangeListenerIds         map[connect.Id]bool
	providerTransportSettingsChangeListenerIds map[connect.Id]bool
	packetStatsChangeListenerIds               map[connect.Id]bool
	egressContractStatsChangeListenerIds       map[connect.Id]bool
	egressContractDetailsChangeListenerIds     map[connect.Id]bool
	ingressContractStatsChangeListenerIds      map[connect.Id]bool
	ingressContractDetailsChangeListenerIds    map[connect.Id]bool
	dnsResolverSettingsChangeListenerIds       map[connect.Id]bool
	networkPeersChangeListenerIds              map[connect.Id]bool

	providerPacketStatsChangeListenerIds            map[connect.Id]bool
	providerEgressContractStatsChangeListenerIds    map[connect.Id]bool
	providerEgressContractDetailsChangeListenerIds  map[connect.Id]bool
	providerIngressContractStatsChangeListenerIds   map[connect.Id]bool
	providerIngressContractDetailsChangeListenerIds map[connect.Id]bool

	// window id -> listener id
	windowMonitorEventListenerIds map[connect.Id]map[connect.Id]bool
	// local window id -> window id
	localWindowIds     map[connect.Id]connect.Id
	localWindowMonitor windowMonitor
	localWindowId      connect.Id

	canShowRatingDialogChangeListenerSub       Sub
	canPromptIntroFunnelChangeListenerSub      Sub
	allowForegroundChangeListenerSub           Sub
	canReferChangeListenerSub                  Sub
	provideModeChangeListenerSub               Sub
	provideChangeListenerSub                   Sub
	provideControlModeChangeListenerSub        Sub
	performanceProfileChangeListenerSub        Sub
	providerIdentityChangeListenerSub          Sub
	providePausedChangeListenerSub             Sub
	offlineChangeListenerSub                   Sub
	vpnInterfaceWhileOfflineChangeListenerSub  Sub
	connectChangeListenerSub                   Sub
	routeLocalChangeListenerSub                Sub
	blockerEnabledChangeListenerSub            Sub
	connectLocationChangeListenerSub           Sub
	defaultLocationChangeListenerSub           Sub
	provideSecretKeysListenerSub               Sub
	provideNetworkModeChangeListenerSub        Sub
	windowMonitorEventListenerSub              func()
	tunnelChangeListenerSub                    Sub
	contractStatusChangeListenerSub            Sub
	windowStatusChangeListenerSub              Sub
	blockActionWindowChangeListenerSub         Sub
	blockStatsChangeListenerSub                Sub
	blockActionOverridesChangeListenerSub      Sub
	transportSettingsChangeListenerSub         Sub
	providerTransportSettingsChangeListenerSub Sub
	packetStatsChangeListenerSub               Sub
	egressContractStatsChangeListenerSub       Sub
	egressContractDetailsChangeListenerSub     Sub
	ingressContractStatsChangeListenerSub      Sub
	ingressContractDetailsChangeListenerSub    Sub
	dnsResolverSettingsChangeListenerSub       Sub
	networkPeersChangeListenerSub              Sub

	providerPacketStatsChangeListenerSub            Sub
	providerEgressContractStatsChangeListenerSub    Sub
	providerEgressContractDetailsChangeListenerSub  Sub
	providerIngressContractStatsChangeListenerSub   Sub
	providerIngressContractDetailsChangeListenerSub Sub

	service *rpcClient

	// reverse notification delivery (see sendLoop). producers enqueue and
	// return; the blocking rpc runs on sendLoop, off stateLock, coalescing by
	// key so a slow or suspended app cannot build unbounded backlog.
	sendMu                 sync.Mutex
	sendPending            map[string]func()
	sendOrder              []string
	sendSignal             chan struct{}
	sendWindowMonitorEvent *DeviceRemoteWindowMonitorEvent

	// Separately bounds fetch and delivery stages so a slow/suspended app cannot
	// pile up request/response buffers (HttpPostRaw / HttpGetRaw).
	httpSem         chan struct{}
	httpDeliverySem chan struct{}
}

func newDeviceLocalRpc(
	ctx context.Context,
	conn net.Conn,
	reverseConn net.Conn,
	deviceLocal *DeviceLocal,
	settings *deviceRpcSettings,
) *DeviceLocalRpc {
	cancelCtx, cancel := context.WithCancel(ctx)

	deviceLocalRpc := &DeviceLocalRpc{
		ctx:                                        cancelCtx,
		cancel:                                     cancel,
		conn:                                       conn,
		reverseConn:                                reverseConn,
		deviceLocal:                                deviceLocal,
		egressSecurityPolicy:                       deviceLocal.egressSecurityPolicy(),
		ingressSecurityPolicy:                      deviceLocal.ingressSecurityPolicy(),
		settings:                                   settings,
		canShowRatingDialogChangeListenerIds:       map[connect.Id]bool{},
		canPromptIntroFunnelChangeListenerIds:      map[connect.Id]bool{},
		allowForegroundChangeListenerIds:           map[connect.Id]bool{},
		canReferChangeListenerIds:                  map[connect.Id]bool{},
		provideModeChangeListenerIds:               map[connect.Id]bool{},
		provideChangeListenerIds:                   map[connect.Id]bool{},
		provideControlModeChangeListenerIds:        map[connect.Id]bool{},
		performanceProfileChangeListenerIds:        map[connect.Id]bool{},
		providerIdentityChangeListenerIds:          map[connect.Id]bool{},
		provideNetworkModeChangeListenerIds:        map[connect.Id]bool{},
		providePausedChangeListenerIds:             map[connect.Id]bool{},
		offlineChangeListenerIds:                   map[connect.Id]bool{},
		vpnInterfaceWhileOfflineChangeListenerIds:  map[connect.Id]bool{},
		connectChangeListenerIds:                   map[connect.Id]bool{},
		routeLocalChangeListenerIds:                map[connect.Id]bool{},
		blockerEnabledChangeListenerIds:            map[connect.Id]bool{},
		connectLocationChangeListenerIds:           map[connect.Id]bool{},
		defaultLocationChangeListenerIds:           map[connect.Id]bool{},
		provideSecretKeysListenerIds:               map[connect.Id]bool{},
		windowMonitorEventListenerIds:              map[connect.Id]map[connect.Id]bool{},
		tunnelChangeListenerIds:                    map[connect.Id]bool{},
		contractStatusChangeListenerIds:            map[connect.Id]bool{},
		windowStatusChangeListenerIds:              map[connect.Id]bool{},
		blockActionWindowChangeListenerIds:         map[connect.Id]bool{},
		blockStatsChangeListenerIds:                map[connect.Id]bool{},
		blockActionOverridesChangeListenerIds:      map[connect.Id]bool{},
		transportSettingsChangeListenerIds:         map[connect.Id]bool{},
		providerTransportSettingsChangeListenerIds: map[connect.Id]bool{},
		packetStatsChangeListenerIds:               map[connect.Id]bool{},
		egressContractStatsChangeListenerIds:       map[connect.Id]bool{},
		egressContractDetailsChangeListenerIds:     map[connect.Id]bool{},
		ingressContractStatsChangeListenerIds:      map[connect.Id]bool{},
		ingressContractDetailsChangeListenerIds:    map[connect.Id]bool{},
		dnsResolverSettingsChangeListenerIds:       map[connect.Id]bool{},
		networkPeersChangeListenerIds:              map[connect.Id]bool{},
		localWindowIds:                             map[connect.Id]connect.Id{},
		sendPending:                                map[string]func(){},
		sendSignal:                                 make(chan struct{}, 1),

		providerPacketStatsChangeListenerIds:            map[connect.Id]bool{},
		providerEgressContractStatsChangeListenerIds:    map[connect.Id]bool{},
		providerEgressContractDetailsChangeListenerIds:  map[connect.Id]bool{},
		providerIngressContractStatsChangeListenerIds:   map[connect.Id]bool{},
		providerIngressContractDetailsChangeListenerIds: map[connect.Id]bool{},
	}
	deviceLocalRpc.httpSem = make(chan struct{}, settings.httpMaxConcurrent())
	deviceLocalRpc.httpDeliverySem = make(chan struct{}, settings.httpMaxConcurrent())

	go connect.HandleError(deviceLocalRpc.run, cancel)
	return deviceLocalRpc
}

// gobServerCodec is the standard net/rpc gob server codec (net/rpc keeps its own
// unexported). It lets run() serve forward rpc one request at a time via
// server.ServeRequest so handler panics can be recovered (see run()).
type gobServerCodec struct {
	rwc    net.Conn
	dec    *gob.Decoder
	enc    *gob.Encoder
	encBuf *bufio.Writer
	closed bool
}

func newGobServerCodec(conn net.Conn) *gobServerCodec {
	buf := bufio.NewWriter(conn)
	return &gobServerCodec{
		rwc:    conn,
		dec:    gob.NewDecoder(conn),
		enc:    gob.NewEncoder(buf),
		encBuf: buf,
	}
}

func (self *gobServerCodec) ReadRequestHeader(r *rpc.Request) error {
	return self.dec.Decode(r)
}

func (self *gobServerCodec) ReadRequestBody(body any) error {
	return self.dec.Decode(body)
}

func (self *gobServerCodec) WriteResponse(r *rpc.Response, body any) (err error) {
	if err = self.enc.Encode(r); err != nil {
		if self.encBuf.Flush() == nil {
			// couldn't encode the header; close to signal the broken connection
			self.Close()
		}
		return
	}
	if err = self.enc.Encode(body); err != nil {
		if self.encBuf.Flush() == nil {
			self.Close()
		}
		return
	}
	return self.encBuf.Flush()
}

func (self *gobServerCodec) Close() error {
	if self.closed {
		return nil
	}
	self.closed = true
	return self.rwc.Close()
}

func (self *DeviceLocalRpc) run() {
	defer self.cancel()
	go connect.HandleError(func() {
		defer self.conn.Close()
		select {
		case <-self.ctx.Done():
		}
	}, self.cancel)

	go connect.HandleError(self.sendLoop, self.cancel)

	server := rpc.NewServer()
	server.Register(self)
	// Serve forward rpc one request at a time (the methods are intended to run on
	// a single goroutine) wrapped in HandleError. net/rpc does NOT recover handler
	// panics — without this, a panic in any served method (or in the core code it
	// calls) crashes the whole network extension. ServeRequest runs the call
	// synchronously, so a panic surfaces here, is recovered, and serving continues
	// with the next request instead of taking down the process. The connection is
	// closed (and ServeRequest unblocked) on ctx cancel by the goroutine above.
	codec := newGobServerCodec(self.conn)
	for self.ctx.Err() == nil {
		var serveErr error
		connect.HandleError(func() {
			serveErr = server.ServeRequest(codec)
		})
		if serveErr != nil {
			// connection closed or unrecoverable codec error
			break
		}
	}
	self.deviceLocal.log.Infof("[dlrcp]server conn done")

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
	for canShowRatingDialogChangeListenerId := range self.canShowRatingDialogChangeListenerIds {
		self.removeCanShowRatingDialogChangeListener(canShowRatingDialogChangeListenerId)
	}
	for canPromptIntroFunnelChangeListenerId := range self.canPromptIntroFunnelChangeListenerIds {
		self.removeCanPromptIntroFunnelChangeListener(canPromptIntroFunnelChangeListenerId)
	}
	for allowForegroundChangeListenerId := range self.allowForegroundChangeListenerIds {
		self.removeAllowForegroundChangeListener(allowForegroundChangeListenerId)
	}
	for canReferChangeListenerId := range self.canReferChangeListenerIds {
		self.removeCanReferChangeListener(canReferChangeListenerId)
	}
	for provideModeChangeListenerId := range self.provideModeChangeListenerIds {
		self.removeProvideModeChangeListener(provideModeChangeListenerId)
	}
	for provideChangeListenerId, _ := range self.provideChangeListenerIds {
		self.removeProvideChangeListener(provideChangeListenerId)
	}
	for provideControlModeChangeListenerId, _ := range self.provideControlModeChangeListenerIds {
		self.removeProvideControlModeChangeListener(provideControlModeChangeListenerId)
	}
	for performanceProfileChangeListenerId, _ := range self.performanceProfileChangeListenerIds {
		self.removePerformanceProfileChangeListener(performanceProfileChangeListenerId)
	}
	for providerIdentityChangeListenerId, _ := range self.providerIdentityChangeListenerIds {
		self.removeProviderIdentityChangeListener(providerIdentityChangeListenerId)
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
	for vpnInterfaceWhileOfflineChangeListenerId := range self.vpnInterfaceWhileOfflineChangeListenerIds {
		self.removeVpnInterfaceWhileOfflineChangeListener(vpnInterfaceWhileOfflineChangeListenerId)
	}
	for connectChangeListenerId, _ := range self.connectChangeListenerIds {
		self.removeConnectChangeListener(connectChangeListenerId)
	}
	for routeLocalChangeListenerId, _ := range self.routeLocalChangeListenerIds {
		self.removeRouteLocalChangeListener(routeLocalChangeListenerId)
	}
	for blockerEnabledChangeListenerId, _ := range self.blockerEnabledChangeListenerIds {
		self.removeBlockerEnabledChangeListener(blockerEnabledChangeListenerId)
	}
	for connectLocationChangeListenerId, _ := range self.connectLocationChangeListenerIds {
		self.removeConnectLocationChangeListener(connectLocationChangeListenerId)
	}
	for defaultLocationChangeListenerId := range self.defaultLocationChangeListenerIds {
		self.removeDefaultLocationChangeListener(defaultLocationChangeListenerId)
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
	for blockActionWindowChangeListenerId, _ := range self.blockActionWindowChangeListenerIds {
		self.removeBlockActionWindowChangeListener(blockActionWindowChangeListenerId)
	}
	for blockStatsChangeListenerId, _ := range self.blockStatsChangeListenerIds {
		self.removeBlockStatsChangeListener(blockStatsChangeListenerId)
	}
	for blockActionOverridesChangeListenerId, _ := range self.blockActionOverridesChangeListenerIds {
		self.removeBlockActionOverridesChangeListener(blockActionOverridesChangeListenerId)
	}
	for transportSettingsChangeListenerId := range self.transportSettingsChangeListenerIds {
		self.removeTransportSettingsChangeListener(transportSettingsChangeListenerId)
	}
	for providerTransportSettingsChangeListenerId := range self.providerTransportSettingsChangeListenerIds {
		self.removeProviderTransportSettingsChangeListener(providerTransportSettingsChangeListenerId)
	}
	for packetStatsChangeListenerId, _ := range self.packetStatsChangeListenerIds {
		self.removePacketStatsChangeListener(packetStatsChangeListenerId)
	}
	for egressContractStatsChangeListenerId, _ := range self.egressContractStatsChangeListenerIds {
		self.removeEgressContractStatsChangeListener(egressContractStatsChangeListenerId)
	}
	for egressContractDetailsChangeListenerId, _ := range self.egressContractDetailsChangeListenerIds {
		self.removeEgressContractDetailsChangeListener(egressContractDetailsChangeListenerId)
	}
	for ingressContractStatsChangeListenerId, _ := range self.ingressContractStatsChangeListenerIds {
		self.removeIngressContractStatsChangeListener(ingressContractStatsChangeListenerId)
	}
	for ingressContractDetailsChangeListenerId, _ := range self.ingressContractDetailsChangeListenerIds {
		self.removeIngressContractDetailsChangeListener(ingressContractDetailsChangeListenerId)
	}
	for providerPacketStatsChangeListenerId, _ := range self.providerPacketStatsChangeListenerIds {
		self.removeProviderPacketStatsChangeListener(providerPacketStatsChangeListenerId)
	}
	for providerEgressContractStatsChangeListenerId, _ := range self.providerEgressContractStatsChangeListenerIds {
		self.removeProviderEgressContractStatsChangeListener(providerEgressContractStatsChangeListenerId)
	}
	for providerEgressContractDetailsChangeListenerId, _ := range self.providerEgressContractDetailsChangeListenerIds {
		self.removeProviderEgressContractDetailsChangeListener(providerEgressContractDetailsChangeListenerId)
	}
	for providerIngressContractStatsChangeListenerId, _ := range self.providerIngressContractStatsChangeListenerIds {
		self.removeProviderIngressContractStatsChangeListener(providerIngressContractStatsChangeListenerId)
	}
	for providerIngressContractDetailsChangeListenerId, _ := range self.providerIngressContractDetailsChangeListenerIds {
		self.removeProviderIngressContractDetailsChangeListener(providerIngressContractDetailsChangeListenerId)
	}
	for dnsResolverSettingsChangeListenerId, _ := range self.dnsResolverSettingsChangeListenerIds {
		self.removeDnsResolverSettingsChangeListener(dnsResolverSettingsChangeListenerId)
	}
	for networkPeersChangeListenerId, _ := range self.networkPeersChangeListenerIds {
		self.removeNetworkPeersChangeListener(networkPeersChangeListenerId)
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

// --- reverse notification delivery ---
//
// Reverse rpcs (DeviceRemoteRpc.*) are fire-and-forget calls into the app,
// produced by connect workers and core-device listener callbacks. Producers must
// not block, and the calls must not run while holding stateLock.
//
// State notifications (the *Changed calls and the window monitor event) are
// delivered by sendLoop: producers enqueue and return immediately, and the
// (blocking) rpc runs on sendLoop, off the producer path and off stateLock. They
// coalesce by key (last value wins) so a slow or suspended app cannot build
// unbounded backlog.
//
// Http responses are delivered directly on their own per-request goroutine (see
// HttpPostRaw / HttpGetRaw), not through sendLoop, so a slow state notification
// can never delay an http response and vice versa.
//
// Delivery blocks (see reverseCall / notifyBlocking): a slow app — e.g. one whose
// callback buffer is full — applies back pressure rather than failing, so a
// transiently slow app never tears down the reverse rpc and flaps the tunnel.
// (State coalescing means back pressure just sheds intermediate values.) The
// reverse rpc (== the tunnel) is torn down only on a real transport error; a dead
// or suspended app is reaped by the transport keepalive, which surfaces as such
// an error.

// enqueueReverse schedules op for delivery by sendLoop, coalescing by key so the
// latest op for a key replaces any prior pending op (last value wins). Never
// blocks and never touches stateLock.
func (self *DeviceLocalRpc) enqueueReverse(key string, op func()) {
	self.sendMu.Lock()
	if _, ok := self.sendPending[key]; !ok {
		self.sendOrder = append(self.sendOrder, key)
	}
	self.sendPending[key] = op
	self.sendMu.Unlock()

	select {
	case self.sendSignal <- struct{}{}:
	default:
	}
}

// reverseNotify enqueues a coalescing fire-and-forget reverse rpc keyed by the
// method name (last value wins). Correct for the *Changed notifications, which
// each carry the current value of one piece of state.
func (self *DeviceLocalRpc) reverseNotify(name string, arg any) {
	self.enqueueReverse(name, func() {
		self.reverseCall(name, arg)
	})
}

// sendLoop delivers enqueued reverse rpcs serially in fifo order. A delivery
// blocks until the app acks it (back pressure); while it does, further events
// coalesce in the queue rather than blocking producers.
func (self *DeviceLocalRpc) sendLoop() {
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-self.sendSignal:
		}
		for {
			self.sendMu.Lock()
			if len(self.sendOrder) == 0 {
				self.sendMu.Unlock()
				break
			}
			key := self.sendOrder[0]
			self.sendOrder = self.sendOrder[1:]
			op := self.sendPending[key]
			delete(self.sendPending, key)
			self.sendMu.Unlock()

			if op != nil {
				op()
			}

			select {
			case <-self.ctx.Done():
				return
			default:
			}
		}
	}
}

// reverseCall performs a fire-and-forget reverse rpc without holding stateLock
// across the (blocking) call. Delivery blocks (notifyBlocking): a slow app
// applies back pressure rather than failing, so a transiently slow app never
// flaps the tunnel. The service is torn down only on a real transport error — a
// dead or suspended app is reaped by the transport keepalive, which surfaces as
// an error here (a dead reverse rpc is a dead tunnel). Called from sendLoop
// (state notifications) and directly from the http handler goroutines (http
// responses).
func (self *DeviceLocalRpc) reverseCall(name string, arg any) {
	self.stateLock.Lock()
	service := self.service
	self.stateLock.Unlock()

	if service == nil {
		return
	}

	service.log.Infof("[rpc]%s", name)
	if err := service.notifyBlocking(name, arg); err != nil {
		service.log.Infof("[rpc]%s err = %s", name, err)
		self.stateLock.Lock()
		// only tear down if this is still the live service; a concurrent
		// closeService or SyncReverse may have already replaced it
		if self.service == service {
			self.closeService()
		}
		self.stateLock.Unlock()
	}
}

// enqueueWindowMonitorEvent coalesces window monitor events. Unlike the
// *Changed notifications these are not pure last-value: each carries provider
// events keyed by clientId plus a reset flag for a full snapshot. Coalescing
// therefore merges rather than replaces — provider events are last-value per
// clientId, matching how the monitor itself coalesces them — so no provider
// transition is dropped under back pressure. A reset replaces accumulated state.
func (self *DeviceLocalRpc) enqueueWindowMonitorEvent(event *DeviceRemoteWindowMonitorEvent) {
	const key = "DeviceRemoteRpc.WindowMonitorEventCallback"

	self.sendMu.Lock()
	if self.sendWindowMonitorEvent == nil || event.Reset {
		// own the maps outright so later in-place merges never mutate a map held
		// by the connect window monitor (or by an in-flight delivery being encoded)
		event.WindowIds = maps.Clone(event.WindowIds)
		event.ProviderEvents = maps.Clone(event.ProviderEvents)
		self.sendWindowMonitorEvent = event
	} else {
		merged := self.sendWindowMonitorEvent
		merged.WindowExpandEvent = event.WindowExpandEvent
		if merged.WindowIds == nil {
			merged.WindowIds = map[connect.Id]bool{}
		}
		for windowId := range event.WindowIds {
			merged.WindowIds[windowId] = true
		}
		if merged.ProviderEvents == nil {
			merged.ProviderEvents = map[connect.Id]*connect.ProviderEvent{}
		}
		for clientId, providerEvent := range event.ProviderEvents {
			merged.ProviderEvents[clientId] = providerEvent
		}
	}
	if _, ok := self.sendPending[key]; !ok {
		self.sendOrder = append(self.sendOrder, key)
	}
	self.sendPending[key] = func() {
		self.sendMu.Lock()
		merged := self.sendWindowMonitorEvent
		self.sendWindowMonitorEvent = nil
		self.sendMu.Unlock()
		if merged != nil {
			self.reverseCall(key, merged)
		}
	}
	self.sendMu.Unlock()

	select {
	case self.sendSignal <- struct{}{}:
	default:
	}
}

func (self *DeviceLocalRpc) state() DeviceRemoteState {
	state := DeviceRemoteState{}

	state.CanShowRatingDialog.Set(self.deviceLocal.GetCanShowRatingDialog())
	state.CanPromptIntroFunnel.Set(self.deviceLocal.GetCanPromptIntroFunnel())
	state.ProvideControlMode.Set(self.deviceLocal.GetProvideControlMode())
	state.CanRefer.Set(self.deviceLocal.GetCanRefer())
	state.AllowForeground.Set(self.deviceLocal.GetAllowForeground())
	state.RouteLocal.Set(self.deviceLocal.GetRouteLocal())
	state.BlockerEnabled.Set(self.deviceLocal.GetBlockerEnabled())
	state.LoadProvideSecretKeys.Set(self.deviceLocal.GetProvideSecretKeys().getAll())
	state.ProvideMode.Set(self.deviceLocal.GetProvideMode())
	state.ProvideNetworkMode.Set(self.deviceLocal.GetProvideNetworkMode())
	state.ProvidePaused.Set(self.deviceLocal.GetProvidePaused())
	state.Offline.Set(self.deviceLocal.GetOffline())
	state.VpnInterfaceWhileOffline.Set(self.deviceLocal.GetVpnInterfaceWhileOffline())
	state.PerformanceProfile.Set(self.deviceLocal.GetPerformanceProfile())
	state.Location.Set(newDeviceRemoteConnectLocation(self.deviceLocal.GetConnectLocation()))
	state.DefaultLocation.Set(newDeviceRemoteDefaultLocation(self.deviceLocal.GetDefaultLocation()))
	state.TunnelStarted.Set(self.deviceLocal.GetTunnelStarted())
	state.BlockActionOverrides.Set(newBlockActionOverridesRpc(self.deviceLocal.GetBlockActionOverrides()))
	state.DnsResolverSettings.Set(newDnsResolverSettingsRpc(self.deviceLocal.GetDnsResolverSettings()))
	state.TransportSettings.Set(newTransportSettingsRpc(self.deviceLocal.GetTransportSettings(), false))
	state.ProviderTransportSettings.Set(newTransportSettingsRpc(self.deviceLocal.GetProviderTransportSettings(), true))
	state.TransportStatus.Set(newTransportStatusRpc(self.deviceLocal.GetTransportStatus()))
	state.ProviderTransportStatus.Set(newTransportStatusRpc(self.deviceLocal.GetProviderTransportStatus()))
	// the effective reliability settings (the override when one is set, the
	// shipped defaults otherwise, and nil when the local has no multi
	// client); lands in the remote's last known state. This is RESPONSE
	// state: its nil says "nothing in force", not "reset the override" --
	// see the `DeviceRemoteState.ReliabilitySettings` note
	state.ReliabilitySettings.Set(self.deviceLocal.GetReliabilitySettings())

	state.ConnectEnabled.Set(self.deviceLocal.GetConnectEnabled())
	state.ProvideEnabled.Set(self.deviceLocal.GetProvideEnabled())
	state.EgressSecurityPolicyStats.Set(self.egressSecurityPolicy.Stats(false))
	state.IngressSecurityPolicyStats.Set(self.ingressSecurityPolicy.Stats(false))
	state.ContractStatus.Set(self.deviceLocal.GetContractStatus())
	state.WindowStatus.Set(self.deviceLocal.GetWindowStatus())

	// InitProvideSecretKeys, RemoveDestination, Destination, Shuffle,
	// ResetEgressSecurityPolicyStats, and ResetIngressSecurityPolicyStats are
	// one-shot commands applied during sync, not queryable local state, so they
	// stay unset. The current connect location is reported via Location.

	return state
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

	// reject a remote built against an incompatible rpc wire version before any
	// side effect — before applying its state below and before it can register
	// listeners. The remote and the local are separately deployed artifacts on
	// the hosted path, and gob fails QUIETLY across an incompatible struct
	// change (a renamed field decodes as its zero value, with no error
	// anywhere), so a mismatched remote must be rejected outright rather than
	// left to half-apply a state it misread. Same handling as the instance
	// mismatch below: the remote surfaces syncResponse.Error, stays unsynced,
	// and reconnects paced.
	if syncRequest.RpcVersion != 0 && syncRequest.RpcVersion != DeviceRpcVersion {
		self.deviceLocal.log.Infof(
			"[dlrpc]sync rejected: remote rpc version %d, local is %d",
			syncRequest.RpcVersion, DeviceRpcVersion,
		)
		*syncResponse = &DeviceRemoteSyncResponse{
			Error: fmt.Sprintf(
				"device rpc version mismatch: remote is %d, local is %d",
				syncRequest.RpcVersion, DeviceRpcVersion,
			),
		}
		return nil
	}

	// reject a remote built for a different device instance before any side
	// effect — before applying its state below and before it can register
	// listeners. The remote surfaces syncResponse.Error, stays unsynced, and
	// reconnects paced, so a foreign listener on a reused address is loud and
	// recoverable rather than a silent wrong-device pairing.
	if syncRequest.InstanceId != (connect.Id{}) && syncRequest.InstanceId != self.deviceLocal.instanceId {
		self.deviceLocal.log.Infof(
			"[dlrpc]sync rejected: remote expects instance %s, local is %s",
			syncRequest.InstanceId, self.deviceLocal.instanceId,
		)
		*syncResponse = &DeviceRemoteSyncResponse{
			Error: fmt.Sprintf(
				"device instance mismatch: remote expects %s, local is %s",
				syncRequest.InstanceId, self.deviceLocal.instanceId,
			),
		}
		return nil
	}

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.closeService()

	// apply state adjustments

	state := syncRequest.State

	// the hosted-incompatible fields (route local, provide settings, transport
	// settings, tunnel/vpn) are guarded: skipped here at the rpc layer, and
	// hard-guarded again inside DeviceLocal. The remote's getters/listeners still
	// see the real device state, so a hosted device keeps its platform-owned
	// values.
	hostedIncompatible := self.settings.DisableHostedIncompatible

	if state.CanShowRatingDialog.IsSet {
		self.deviceLocal.SetCanShowRatingDialog(state.CanShowRatingDialog.Value)
	}
	if state.CanPromptIntroFunnel.IsSet {
		self.deviceLocal.SetCanPromptIntroFunnel(state.CanPromptIntroFunnel.Value)
	}
	if state.AllowForeground.IsSet {
		self.deviceLocal.SetAllowForeground(state.AllowForeground.Value)
	}
	if state.CanRefer.IsSet {
		self.deviceLocal.SetCanRefer(state.CanRefer.Value)
	}
	if state.RouteLocal.IsSet && !hostedIncompatible {
		self.deviceLocal.SetRouteLocal(state.RouteLocal.Value)
	}
	if state.BlockerEnabled.IsSet {
		self.deviceLocal.SetBlockerEnabled(state.BlockerEnabled.Value)
	}
	if state.InitProvideSecretKeys.IsSet {
		self.deviceLocal.InitProvideSecretKeys()
	}
	if state.LoadProvideSecretKeys.IsSet {
		provideSecretKeyList := NewProvideSecretKeyList()
		provideSecretKeyList.addAll(state.LoadProvideSecretKeys.Value...)
		self.deviceLocal.LoadProvideSecretKeys(provideSecretKeyList)
	}
	if state.ProvideMode.IsSet && !hostedIncompatible {
		self.deviceLocal.SetProvideMode(state.ProvideMode.Value)
	}
	// ORDER MATTERS: the control mode applies AFTER the raw provide mode —
	// SetProvideControlMode enforces the control mode's provide mapping, so
	// the control mode owns the effective mode. Applying the (possibly stale,
	// persisted) raw mode after it silently overrode the mapping on every rpc
	// connect (the ios red-light / not-discoverable-at-startup bug).
	if state.ProvideControlMode.IsSet && !hostedIncompatible {
		self.deviceLocal.SetProvideControlMode(state.ProvideControlMode.Value)
	}
	if state.ProvideNetworkMode.IsSet && !hostedIncompatible {
		self.deviceLocal.SetProvideNetworkMode(state.ProvideNetworkMode.Value)
	}
	if state.ProvidePaused.IsSet && !hostedIncompatible {
		self.deviceLocal.SetProvidePaused(state.ProvidePaused.Value)
	}
	if state.Offline.IsSet {
		self.deviceLocal.SetOffline(state.Offline.Value)
	}
	if state.VpnInterfaceWhileOffline.IsSet && !hostedIncompatible {
		self.deviceLocal.SetVpnInterfaceWhileOffline(state.VpnInterfaceWhileOffline.Value)
	}
	// Apply carrier policy before destination state so a newly constructed
	// window starts on the requested modes instead of immediately migrating.
	if state.TransportSettings.IsSet && !hostedIncompatible {
		self.deviceLocal.SetTransportSettings(state.TransportSettings.Value.toTransportSettings(false))
	}
	if state.ProviderTransportSettings.IsSet && !hostedIncompatible {
		self.deviceLocal.SetProviderTransportSettings(state.ProviderTransportSettings.Value.toTransportSettings(true))
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
		)
	}
	if state.PerformanceProfile.IsSet {
		self.deviceLocal.SetPerformanceProfile(state.PerformanceProfile.Value)
	}
	if state.Location.IsSet {
		self.deviceLocal.SetConnectLocation(state.Location.Value.toConnectLocation())
	}
	if state.DefaultLocation.IsSet {
		self.deviceLocal.SetDefaultLocation(state.DefaultLocation.Value.toConnectLocation())
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

	if state.TunnelStarted.IsSet && !hostedIncompatible {
		self.deviceLocal.SetTunnelStarted(state.TunnelStarted.Value)
	}

	if state.BlockActionOverrides.IsSet {
		self.deviceLocal.SetBlockActionOverrides(toBlockActionOverrideList(state.BlockActionOverrides.Value))
	}
	if state.DnsResolverSettings.IsSet {
		self.deviceLocal.SetDnsResolverSettings(state.DnsResolverSettings.Value.toDnsResolverSettings())
	}
	if state.ReliabilitySettings.IsSet {
		// the runtime reliability override; a nil value queues a reset. The
		// remote keeps this queued across syncs (nothing persists it), so an
		// extension restart or rpc reconnect re-applies the override here --
		// see `DeviceRemote.SetReliabilitySettings`
		if reliabilitySettings := state.ReliabilitySettings.Value; reliabilitySettings != nil {
			self.deviceLocal.SetReliabilitySettings(reliabilitySettings)
		} else {
			self.deviceLocal.ResetReliabilitySettings()
		}
	}

	// if state.RefreshToken.IsSet {
	// 	self.deviceLocal.RefreshToken(state.RefreshToken.Value)
	// }

	// add listeners
	for _, canShowRatingDialogChangeListenerId := range syncRequest.CanShowRatingDialogChangeListenerIds {
		self.addCanShowRatingDialogChangeListener(canShowRatingDialogChangeListenerId)
	}
	for _, canPromptIntroFunnelChangeListenerId := range syncRequest.CanPromptIntroFunnelChangeListenerIds {
		self.addCanPromptIntroFunnelChangeListener(canPromptIntroFunnelChangeListenerId)
	}
	for _, allowForegroundChangeListenerId := range syncRequest.AllowForegroundChangeListenerIds {
		self.addAllowForegroundChangeListener(allowForegroundChangeListenerId)
	}
	for _, canReferChangeListenerId := range syncRequest.CanReferChangeListenerIds {
		self.addCanReferChangeListener(canReferChangeListenerId)
	}
	for _, provideModeChangeListenerId := range syncRequest.ProvideModeChangeListenerIds {
		self.addProvideModeChangeListener(provideModeChangeListenerId)
	}
	for _, provideChangeListenerId := range syncRequest.ProvideChangeListenerIds {
		self.addProvideChangeListener(provideChangeListenerId)
	}
	for _, provideControlModeChangeListenerId := range syncRequest.ProvideControlModeChangeListenerIds {
		self.addProvideControlModeChangeListener(provideControlModeChangeListenerId)
	}
	for _, performanceProfileChangeListenerId := range syncRequest.PerformanceProfileChangeListenerIds {
		self.addPerformanceProfileChangeListener(performanceProfileChangeListenerId)
	}
	for _, providerIdentityChangeListenerId := range syncRequest.ProviderIdentityChangeListenerIds {
		self.addProviderIdentityChangeListener(providerIdentityChangeListenerId)
	}
	for _, providePausedChangeListenerId := range syncRequest.ProvidePausedChangeListenerIds {
		self.addProvidePausedChangeListener(providePausedChangeListenerId)
	}
	for _, provideNetworkModeChangeListenerId := range syncRequest.ProvideNetworkModeChangeListenerIds {
		self.addProvideNetworkModeChangeListener(provideNetworkModeChangeListenerId)
	}
	for _, offlineChangeListenerId := range syncRequest.OfflineChangeListenerIds {
		self.addOfflineChangeListener(offlineChangeListenerId)
	}
	for _, vpnInterfaceWhileOfflineChangeListenerId := range syncRequest.VpnInterfaceWhileOfflineChangeListenerIds {
		self.addVpnInterfaceWhileOfflineChangeListener(vpnInterfaceWhileOfflineChangeListenerId)
	}
	for _, connectChangeListenerId := range syncRequest.ConnectChangeListenerIds {
		self.addConnectChangeListener(connectChangeListenerId)
	}
	for _, routeLocalChangeListenerId := range syncRequest.RouteLocalChangeListenerIds {
		self.addRouteLocalChangeListener(routeLocalChangeListenerId)
	}
	for _, blockerEnabledChangeListenerId := range syncRequest.BlockerEnabledChangeListenerIds {
		self.addBlockerEnabledChangeListener(blockerEnabledChangeListenerId)
	}
	for _, connectLocationChangeListenerId := range syncRequest.ConnectLocationChangeListenerIds {
		self.addConnectLocationChangeListener(connectLocationChangeListenerId)
	}
	for _, defaultLocationChangeListenerId := range syncRequest.DefaultLocationChangeListenerIds {
		self.addDefaultLocationChangeListener(defaultLocationChangeListenerId)
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
	for _, blockActionWindowChangeListenerId := range syncRequest.BlockActionWindowChangeListenerIds {
		self.addBlockActionWindowChangeListener(blockActionWindowChangeListenerId)
	}
	for _, blockStatsChangeListenerId := range syncRequest.BlockStatsChangeListenerIds {
		self.addBlockStatsChangeListener(blockStatsChangeListenerId)
	}
	for _, blockActionOverridesChangeListenerId := range syncRequest.BlockActionOverridesChangeListenerIds {
		self.addBlockActionOverridesChangeListener(blockActionOverridesChangeListenerId)
	}
	for _, transportSettingsChangeListenerId := range syncRequest.TransportSettingsChangeListenerIds {
		self.addTransportSettingsChangeListener(transportSettingsChangeListenerId)
	}
	for _, providerTransportSettingsChangeListenerId := range syncRequest.ProviderTransportSettingsChangeListenerIds {
		self.addProviderTransportSettingsChangeListener(providerTransportSettingsChangeListenerId)
	}
	for _, packetStatsChangeListenerId := range syncRequest.PacketStatsChangeListenerIds {
		self.addPacketStatsChangeListener(packetStatsChangeListenerId)
	}
	for _, egressContractStatsChangeListenerId := range syncRequest.EgressContractStatsChangeListenerIds {
		self.addEgressContractStatsChangeListener(egressContractStatsChangeListenerId)
	}
	for _, egressContractDetailsChangeListenerId := range syncRequest.EgressContractDetailsChangeListenerIds {
		self.addEgressContractDetailsChangeListener(egressContractDetailsChangeListenerId)
	}
	for _, ingressContractStatsChangeListenerId := range syncRequest.IngressContractStatsChangeListenerIds {
		self.addIngressContractStatsChangeListener(ingressContractStatsChangeListenerId)
	}
	for _, ingressContractDetailsChangeListenerId := range syncRequest.IngressContractDetailsChangeListenerIds {
		self.addIngressContractDetailsChangeListener(ingressContractDetailsChangeListenerId)
	}
	for _, providerPacketStatsChangeListenerId := range syncRequest.ProviderPacketStatsChangeListenerIds {
		self.addProviderPacketStatsChangeListener(providerPacketStatsChangeListenerId)
	}
	for _, providerEgressContractStatsChangeListenerId := range syncRequest.ProviderEgressContractStatsChangeListenerIds {
		self.addProviderEgressContractStatsChangeListener(providerEgressContractStatsChangeListenerId)
	}
	for _, providerEgressContractDetailsChangeListenerId := range syncRequest.ProviderEgressContractDetailsChangeListenerIds {
		self.addProviderEgressContractDetailsChangeListener(providerEgressContractDetailsChangeListenerId)
	}
	for _, providerIngressContractStatsChangeListenerId := range syncRequest.ProviderIngressContractStatsChangeListenerIds {
		self.addProviderIngressContractStatsChangeListener(providerIngressContractStatsChangeListenerId)
	}
	for _, providerIngressContractDetailsChangeListenerId := range syncRequest.ProviderIngressContractDetailsChangeListenerIds {
		self.addProviderIngressContractDetailsChangeListener(providerIngressContractDetailsChangeListenerId)
	}
	for _, dnsResolverSettingsChangeListenerId := range syncRequest.DnsResolverSettingsChangeListenerIds {
		self.addDnsResolverSettingsChangeListener(dnsResolverSettingsChangeListenerId)
	}
	for _, networkPeersChangeListenerId := range syncRequest.NetworkPeersChangeListenerIds {
		self.addNetworkPeersChangeListener(networkPeersChangeListenerId)
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
		State:            self.state(),
		DeviceGeneration: self.settings.DeviceGeneration,
	}
	return nil
}

func (self *DeviceLocalRpc) SyncReverse(_ RpcNoArg, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	select {
	case <-self.ctx.Done():
		return fmt.Errorf("Done")
	default:
	}

	self.deviceLocal.log.Infof("[dlrpc]sync reverse")

	// the reverse stream is already established by the listener; use it as the
	// DeviceRemoteRpc client rather than dialing back. reverse delivery blocks for
	// back pressure (see reverseCall / notifyBlocking), so the timeout/closeClient
	// fields are unused for this client.
	self.service = &rpcClientWithTimeout{
		ctx:         self.ctx,
		log:         self.settings.logger(),
		timeout:     self.settings.RpcCallTimeout,
		closeClient: self.reverseConn.Close,
		client:      rpc.NewClient(self.reverseConn),
	}

	// fire listeners with the current state

	if self.canShowRatingDialogChangeListenerSub != nil {
		self.canShowRatingDialogChanged(self.deviceLocal.GetCanShowRatingDialog())
	}
	if self.canPromptIntroFunnelChangeListenerSub != nil {
		self.canPromptIntroFunnelChanged(self.deviceLocal.GetCanPromptIntroFunnel())
	}
	if self.allowForegroundChangeListenerSub != nil {
		self.allowForegroundChanged(self.deviceLocal.GetAllowForeground())
	}
	if self.canReferChangeListenerSub != nil {
		self.canReferChanged(self.deviceLocal.GetCanRefer())
	}
	if self.provideModeChangeListenerSub != nil {
		self.provideModeChanged(self.deviceLocal.GetProvideMode())
	}
	if self.provideChangeListenerSub != nil {
		self.provideChanged(self.deviceLocal.GetProvideEnabled())
	}
	if self.provideControlModeChangeListenerSub != nil {
		self.provideControlModeChanged(self.deviceLocal.GetProvideControlMode())
	}
	if self.providePausedChangeListenerSub != nil {
		self.providePausedChanged(self.deviceLocal.GetProvidePaused())
	}
	if self.performanceProfileChangeListenerSub != nil {
		self.performanceProfileChanged(self.deviceLocal.GetPerformanceProfile())
	}
	if self.providerIdentityChangeListenerSub != nil {
		self.providerIdentitiesChanged(self.deviceLocal.GetProviderIdentities())
	}
	if self.provideNetworkModeChangeListenerSub != nil {
		self.provideNetworkModeChanged(self.deviceLocal.GetProvideNetworkMode())
	}
	if self.offlineChangeListenerSub != nil {
		self.offlineChanged(self.deviceLocal.GetOffline(), self.deviceLocal.GetVpnInterfaceWhileOffline())
	}
	if self.vpnInterfaceWhileOfflineChangeListenerSub != nil {
		self.vpnInterfaceWhileOfflineChanged(self.deviceLocal.GetVpnInterfaceWhileOffline())
	}
	if self.connectChangeListenerSub != nil {
		self.connectChanged(self.deviceLocal.GetConnectEnabled())
	}
	if self.routeLocalChangeListenerSub != nil {
		self.routeLocalChanged(self.deviceLocal.GetRouteLocal())
	}
	if self.blockerEnabledChangeListenerSub != nil {
		self.blockerEnabledChanged(self.deviceLocal.GetBlockerEnabled())
	}
	if self.connectLocationChangeListenerSub != nil {
		self.connectLocationChanged(self.deviceLocal.GetConnectLocation())
	}
	if self.defaultLocationChangeListenerSub != nil {
		self.defaultLocationChanged(self.deviceLocal.GetDefaultLocation())
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
	if self.blockActionWindowChangeListenerSub != nil {
		self.blockActionWindowChanged(self.deviceLocal.GetBlockActions())
	}
	if self.blockStatsChangeListenerSub != nil {
		self.blockStatsChanged(self.deviceLocal.GetBlockStats())
	}
	if self.blockActionOverridesChangeListenerSub != nil {
		self.blockActionOverridesChanged(self.deviceLocal.GetBlockActionOverrides())
	}
	if self.transportSettingsChangeListenerSub != nil {
		self.transportSettingsChanged(self.deviceLocal.GetTransportSettings())
	}
	if self.providerTransportSettingsChangeListenerSub != nil {
		self.providerTransportSettingsChanged(self.deviceLocal.GetProviderTransportSettings())
	}
	if self.packetStatsChangeListenerSub != nil {
		self.packetStatsChanged(self.deviceLocal.GetPacketStats())
	}
	if self.egressContractStatsChangeListenerSub != nil {
		self.egressContractStatsChanged(self.deviceLocal.GetEgressContractStats())
	}
	if self.egressContractDetailsChangeListenerSub != nil {
		// the listener receives one details item per call (see `DeviceLocal`)
		for _, contractDetails := range self.deviceLocal.GetEgressContractDetails().getAll() {
			self.egressContractDetailsChanged(contractDetails)
		}
	}
	if self.ingressContractStatsChangeListenerSub != nil {
		self.ingressContractStatsChanged(self.deviceLocal.GetIngressContractStats())
	}
	if self.ingressContractDetailsChangeListenerSub != nil {
		// the listener receives one details item per call (see `DeviceLocal`)
		for _, contractDetails := range self.deviceLocal.GetIngressContractDetails().getAll() {
			self.ingressContractDetailsChanged(contractDetails)
		}
	}
	// the provider getters are nil when the device has no provider; there is
	// no current state to fire
	if self.providerPacketStatsChangeListenerSub != nil {
		if packetStats := self.deviceLocal.GetProviderPacketStats(); packetStats != nil {
			self.providerPacketStatsChanged(packetStats)
		}
	}
	if self.providerEgressContractStatsChangeListenerSub != nil {
		if contractStats := self.deviceLocal.GetProviderEgressContractStats(); contractStats != nil {
			self.providerEgressContractStatsChanged(contractStats)
		}
	}
	if self.providerEgressContractDetailsChangeListenerSub != nil {
		if contractDetailsList := self.deviceLocal.GetProviderEgressContractDetails(); contractDetailsList != nil {
			// the listener receives one details item per call (see `DeviceLocal`)
			for _, contractDetails := range contractDetailsList.getAll() {
				self.providerEgressContractDetailsChanged(contractDetails)
			}
		}
	}
	if self.providerIngressContractStatsChangeListenerSub != nil {
		if contractStats := self.deviceLocal.GetProviderIngressContractStats(); contractStats != nil {
			self.providerIngressContractStatsChanged(contractStats)
		}
	}
	if self.providerIngressContractDetailsChangeListenerSub != nil {
		if contractDetailsList := self.deviceLocal.GetProviderIngressContractDetails(); contractDetailsList != nil {
			// the listener receives one details item per call (see `DeviceLocal`)
			for _, contractDetails := range contractDetailsList.getAll() {
				self.providerIngressContractDetailsChanged(contractDetails)
			}
		}
	}
	if self.dnsResolverSettingsChangeListenerSub != nil {
		self.dnsResolverSettingsChanged(self.deviceLocal.GetDnsResolverSettings())
	}
	if self.networkPeersChangeListenerSub != nil {
		self.networkPeersChanged(self.deviceLocal.GetNetworkPeers())
	}
	if self.localWindowMonitor != nil && self.windowMonitorEventListenerSub != nil {
		windowExpandEvent, providerEvents := self.localWindowMonitor.Events()
		self.windowMonitorEventCallback(windowExpandEvent, providerEvents, true)
	}

	return nil
}

// hostedIncompatibleRpcGuarded reports whether a hosted-incompatible rpc setter
// should be skipped. Getters and listeners are never guarded.
func (self *DeviceLocalRpc) hostedIncompatibleRpcGuarded(name string) bool {
	if self.settings.DisableHostedIncompatible {
		self.deviceLocal.log.Infof("[dlrpc]hosted incompatible: %s ignored\n", name)
		return true
	}
	return false
}

func (self *DeviceLocalRpc) SetTunnelStarted(tunnelStarted bool, _ RpcVoid) error {
	if self.hostedIncompatibleRpcGuarded("SetTunnelStarted") {
		return nil
	}
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

// must be called with stateLock
func (self *DeviceLocalRpc) removeWindowStatusChangeListener(listenerId connect.Id) {
	delete(self.windowStatusChangeListenerIds, listenerId)
	if len(self.windowStatusChangeListenerIds) == 0 && self.windowStatusChangeListenerSub != nil {
		self.windowStatusChangeListenerSub.Close()
		self.windowStatusChangeListenerSub = nil
	}
}

func (self *DeviceLocalRpc) RemoveWindowStatusChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeWindowStatusChangeListener(listenerId)
	return nil
}

// TunnelChangeListener
func (self *DeviceLocalRpc) TunnelChanged(tunnelStarted bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.tunnelChanged(tunnelStarted)
}

// enqueues an async, coalescing reverse notification (see sendLoop)
func (self *DeviceLocalRpc) tunnelChanged(tunnelStarted bool) {
	self.reverseNotify("DeviceRemoteRpc.TunnelChanged", tunnelStarted)
}

// ContractStatusChangeListener
func (self *DeviceLocalRpc) ContractStatusChanged(contractStatus *ContractStatus) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.contractStatusChanged(contractStatus)
}

// enqueues an async, coalescing reverse notification (see sendLoop)
func (self *DeviceLocalRpc) contractStatusChanged(contractStatus *ContractStatus) {
	status := &DeviceRemoteContractStatus{
		ContractStatus: contractStatus,
	}
	self.reverseNotify("DeviceRemoteRpc.ContractStatusChanged", status)
}

// WindowStatusChangeListener
func (self *DeviceLocalRpc) WindowStatusChanged(windowStatus *WindowStatus) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.windowStatusChanged(windowStatus)
}

// enqueues an async, coalescing reverse notification (see sendLoop)
func (self *DeviceLocalRpc) windowStatusChanged(windowStatus *WindowStatus) {
	status := &DeviceRemoteWindowStatus{
		WindowStatus: windowStatus,
	}
	self.reverseNotify("DeviceRemoteRpc.WindowStatusChanged", status)
}

// privacy block

func (self *DeviceLocalRpc) GetBlockStats(_ RpcNoArg, stats **DeviceRemoteBlockStats) error {
	*stats = &DeviceRemoteBlockStats{
		BlockStats: self.deviceLocal.GetBlockStats(),
	}
	return nil
}

func (self *DeviceLocalRpc) GetBlockActions(_ RpcNoArg, window **DeviceRemoteBlockActionWindow) error {
	*window = &DeviceRemoteBlockActionWindow{
		BlockActionWindow: newBlockActionWindowRpc(self.deviceLocal.GetBlockActions()),
	}
	return nil
}

func (self *DeviceLocalRpc) AddBlockActionOverride(overrideRpc *BlockActionOverrideRpc, _ RpcVoid) error {
	self.deviceLocal.AddBlockActionOverride(overrideRpc.toBlockActionOverride())
	return nil
}

func (self *DeviceLocalRpc) RemoveBlockActionOverride(overrideId connect.Id, _ RpcVoid) error {
	self.deviceLocal.RemoveBlockActionOverride(newId(overrideId))
	return nil
}

func (self *DeviceLocalRpc) SetBlockActionOverrides(deviceOverrides *DeviceRemoteBlockActionOverrides, _ RpcVoid) error {
	self.deviceLocal.SetBlockActionOverrides(toBlockActionOverrideList(deviceOverrides.BlockActionOverrides))
	return nil
}

func (self *DeviceLocalRpc) GetBlockActionOverrides(_ RpcNoArg, deviceOverrides **DeviceRemoteBlockActionOverrides) error {
	*deviceOverrides = &DeviceRemoteBlockActionOverrides{
		BlockActionOverrides: newBlockActionOverridesRpc(self.deviceLocal.GetBlockActionOverrides()),
	}
	return nil
}

func (self *DeviceLocalRpc) GetLocalOverrideAppIds(_ RpcNoArg, appIds **DeviceRemoteLocalOverrideAppIds) error {
	*appIds = &DeviceRemoteLocalOverrideAppIds{
		LocalOverrideAppIds: newOverrideLocalAppIdsRpc(self.deviceLocal.GetLocalOverrideAppIds()),
	}
	return nil
}

func (self *DeviceLocalRpc) AddBlockActionWindowChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addBlockActionWindowChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addBlockActionWindowChangeListener(listenerId connect.Id) {
	self.blockActionWindowChangeListenerIds[listenerId] = true
	if self.blockActionWindowChangeListenerSub == nil {
		self.blockActionWindowChangeListenerSub = self.deviceLocal.AddBlockActionWindowChangeListener(self)
	}
}

func (self *DeviceLocalRpc) RemoveBlockActionWindowChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeBlockActionWindowChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeBlockActionWindowChangeListener(listenerId connect.Id) {
	delete(self.blockActionWindowChangeListenerIds, listenerId)
	if len(self.blockActionWindowChangeListenerIds) == 0 && self.blockActionWindowChangeListenerSub != nil {
		self.blockActionWindowChangeListenerSub.Close()
		self.blockActionWindowChangeListenerSub = nil
	}
}

// BlockActionWindowChangeListener
func (self *DeviceLocalRpc) BlockActionWindowChanged(blockActionWindow *BlockActionWindow) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.blockActionWindowChanged(blockActionWindow)
}

// enqueues an async, coalescing reverse notification (see sendLoop).
// the payload is the full window snapshot, so last-value coalescing is safe
func (self *DeviceLocalRpc) blockActionWindowChanged(blockActionWindow *BlockActionWindow) {
	event := &DeviceRemoteBlockActionWindow{
		BlockActionWindow: newBlockActionWindowRpc(blockActionWindow),
	}
	self.reverseNotify("DeviceRemoteRpc.BlockActionWindowChanged", event)
}

func (self *DeviceLocalRpc) AddBlockStatsChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addBlockStatsChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addBlockStatsChangeListener(listenerId connect.Id) {
	self.blockStatsChangeListenerIds[listenerId] = true
	if self.blockStatsChangeListenerSub == nil {
		self.blockStatsChangeListenerSub = self.deviceLocal.AddBlockStatsChangeListener(self)
	}
}

func (self *DeviceLocalRpc) RemoveBlockStatsChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeBlockStatsChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeBlockStatsChangeListener(listenerId connect.Id) {
	delete(self.blockStatsChangeListenerIds, listenerId)
	if len(self.blockStatsChangeListenerIds) == 0 && self.blockStatsChangeListenerSub != nil {
		self.blockStatsChangeListenerSub.Close()
		self.blockStatsChangeListenerSub = nil
	}
}

// BlockStatsChangeListener
func (self *DeviceLocalRpc) BlockStatsChanged(blockStats *BlockStats) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.blockStatsChanged(blockStats)
}

// enqueues an async, coalescing reverse notification (see sendLoop)
func (self *DeviceLocalRpc) blockStatsChanged(blockStats *BlockStats) {
	stats := &DeviceRemoteBlockStats{
		BlockStats: blockStats,
	}
	self.reverseNotify("DeviceRemoteRpc.BlockStatsChanged", stats)
}

func (self *DeviceLocalRpc) AddBlockActionOverridesChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addBlockActionOverridesChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addBlockActionOverridesChangeListener(listenerId connect.Id) {
	self.blockActionOverridesChangeListenerIds[listenerId] = true
	if self.blockActionOverridesChangeListenerSub == nil {
		self.blockActionOverridesChangeListenerSub = self.deviceLocal.AddBlockActionOverridesChangeListener(self)
	}
}

func (self *DeviceLocalRpc) RemoveBlockActionOverridesChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeBlockActionOverridesChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeBlockActionOverridesChangeListener(listenerId connect.Id) {
	delete(self.blockActionOverridesChangeListenerIds, listenerId)
	if len(self.blockActionOverridesChangeListenerIds) == 0 && self.blockActionOverridesChangeListenerSub != nil {
		self.blockActionOverridesChangeListenerSub.Close()
		self.blockActionOverridesChangeListenerSub = nil
	}
}

// BlockActionOverridesChangeListener
func (self *DeviceLocalRpc) BlockActionOverridesChanged(blockActionOverrides *BlockActionOverrideList) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.blockActionOverridesChanged(blockActionOverrides)
}

// enqueues an async, coalescing reverse notification (see sendLoop).
// the payload is the full overrides list, so last-value coalescing is safe
func (self *DeviceLocalRpc) blockActionOverridesChanged(blockActionOverrides *BlockActionOverrideList) {
	event := &DeviceRemoteBlockActionOverrides{
		BlockActionOverrides: newBlockActionOverridesRpc(blockActionOverrides),
	}
	self.reverseNotify("DeviceRemoteRpc.BlockActionOverridesChanged", event)
}

// packet stats

func (self *DeviceLocalRpc) GetPacketStats(_ RpcNoArg, stats **DeviceRemotePacketStats) error {
	*stats = &DeviceRemotePacketStats{
		PacketStats: newPacketStatsRpc(self.deviceLocal.GetPacketStats(), true),
	}
	return nil
}

func (self *DeviceLocalRpc) AddPacketStatsChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addPacketStatsChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addPacketStatsChangeListener(listenerId connect.Id) {
	self.packetStatsChangeListenerIds[listenerId] = true
	if self.packetStatsChangeListenerSub == nil {
		self.packetStatsChangeListenerSub = self.deviceLocal.AddPacketStatsChangeListener(self)
	}
}

func (self *DeviceLocalRpc) RemovePacketStatsChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removePacketStatsChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removePacketStatsChangeListener(listenerId connect.Id) {
	delete(self.packetStatsChangeListenerIds, listenerId)
	if len(self.packetStatsChangeListenerIds) == 0 && self.packetStatsChangeListenerSub != nil {
		self.packetStatsChangeListenerSub.Close()
		self.packetStatsChangeListenerSub = nil
	}
}

// PacketStatsChangeListener
func (self *DeviceLocalRpc) PacketStatsChanged(packetStats *PacketStats) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.packetStatsChanged(packetStats)
}

// enqueues an async, coalescing reverse notification (see sendLoop)
func (self *DeviceLocalRpc) packetStatsChanged(packetStats *PacketStats) {
	stats := &DeviceRemotePacketStats{
		PacketStats: newPacketStatsRpc(packetStats, true),
	}
	self.reverseNotify("DeviceRemoteRpc.PacketStatsChanged", stats)
}

// contract stats

// the `ContractStatsChangeListener`, `ContractDetailsChangeListener` and
// `PacketStatsChangeListener` interfaces are shared by the egress and ingress
// directions and by the client and provider stats, so `DeviceLocalRpc` cannot
// implement them once per surface.
// these small adapters subscribe to `DeviceLocal` per surface and forward to
// a surface-specific reverse notification (`changed`, called with stateLock).

type contractStatsForwarder struct {
	rpc     *DeviceLocalRpc
	changed func(contractStats *ContractStats)
}

// ContractStatsChangeListener
func (self *contractStatsForwarder) ContractStatsChanged(contractStats *ContractStats) {
	self.rpc.stateLock.Lock()
	defer self.rpc.stateLock.Unlock()
	self.changed(contractStats)
}

type contractDetailsForwarder struct {
	rpc     *DeviceLocalRpc
	changed func(contractDetails *ContractDetails)
}

// ContractDetailsChangeListener
func (self *contractDetailsForwarder) ContractDetailsChanged(contractDetails *ContractDetails) {
	self.rpc.stateLock.Lock()
	defer self.rpc.stateLock.Unlock()
	self.changed(contractDetails)
}

type packetStatsForwarder struct {
	rpc     *DeviceLocalRpc
	changed func(packetStats *PacketStats)
}

// PacketStatsChangeListener
func (self *packetStatsForwarder) PacketStatsChanged(packetStats *PacketStats) {
	self.rpc.stateLock.Lock()
	defer self.rpc.stateLock.Unlock()
	self.changed(packetStats)
}

func (self *DeviceLocalRpc) GetEgressContractStats(_ RpcNoArg, stats **DeviceRemoteContractStats) error {
	*stats = &DeviceRemoteContractStats{
		ContractStats: self.deviceLocal.GetEgressContractStats(),
	}
	return nil
}

func (self *DeviceLocalRpc) GetEgressContractDetails(_ RpcNoArg, details **DeviceRemoteContractDetailsList) error {
	*details = &DeviceRemoteContractDetailsList{
		ContractDetailsList: newContractDetailsListRpc(self.deviceLocal.GetEgressContractDetails()),
	}
	return nil
}

func (self *DeviceLocalRpc) GetIngressContractStats(_ RpcNoArg, stats **DeviceRemoteContractStats) error {
	*stats = &DeviceRemoteContractStats{
		ContractStats: self.deviceLocal.GetIngressContractStats(),
	}
	return nil
}

func (self *DeviceLocalRpc) GetIngressContractDetails(_ RpcNoArg, details **DeviceRemoteContractDetailsList) error {
	*details = &DeviceRemoteContractDetailsList{
		ContractDetailsList: newContractDetailsListRpc(self.deviceLocal.GetIngressContractDetails()),
	}
	return nil
}

func (self *DeviceLocalRpc) AddEgressContractStatsChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addEgressContractStatsChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addEgressContractStatsChangeListener(listenerId connect.Id) {
	self.egressContractStatsChangeListenerIds[listenerId] = true
	if self.egressContractStatsChangeListenerSub == nil {
		self.egressContractStatsChangeListenerSub = self.deviceLocal.AddEgressContractStatsChangeListener(&contractStatsForwarder{rpc: self, changed: self.egressContractStatsChanged})
	}
}

func (self *DeviceLocalRpc) RemoveEgressContractStatsChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeEgressContractStatsChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeEgressContractStatsChangeListener(listenerId connect.Id) {
	delete(self.egressContractStatsChangeListenerIds, listenerId)
	if len(self.egressContractStatsChangeListenerIds) == 0 && self.egressContractStatsChangeListenerSub != nil {
		self.egressContractStatsChangeListenerSub.Close()
		self.egressContractStatsChangeListenerSub = nil
	}
}

// enqueues an async, coalescing reverse notification (see sendLoop)
func (self *DeviceLocalRpc) egressContractStatsChanged(contractStats *ContractStats) {
	stats := &DeviceRemoteContractStats{
		ContractStats: contractStats,
	}
	self.reverseNotify("DeviceRemoteRpc.EgressContractStatsChanged", stats)
}

func (self *DeviceLocalRpc) AddEgressContractDetailsChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addEgressContractDetailsChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addEgressContractDetailsChangeListener(listenerId connect.Id) {
	self.egressContractDetailsChangeListenerIds[listenerId] = true
	if self.egressContractDetailsChangeListenerSub == nil {
		self.egressContractDetailsChangeListenerSub = self.deviceLocal.AddEgressContractDetailsChangeListener(&contractDetailsForwarder{rpc: self, changed: self.egressContractDetailsChanged})
	}
}

func (self *DeviceLocalRpc) RemoveEgressContractDetailsChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeEgressContractDetailsChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeEgressContractDetailsChangeListener(listenerId connect.Id) {
	delete(self.egressContractDetailsChangeListenerIds, listenerId)
	if len(self.egressContractDetailsChangeListenerIds) == 0 && self.egressContractDetailsChangeListenerSub != nil {
		self.egressContractDetailsChangeListenerSub.Close()
		self.egressContractDetailsChangeListenerSub = nil
	}
}

// enqueues an async, coalescing reverse notification (see sendLoop).
// unlike the full-state notifications, the payload is a single details item,
// so coalesce per contract id (last value per contract) to not drop a
// contract's latest details under back pressure
func (self *DeviceLocalRpc) egressContractDetailsChanged(contractDetails *ContractDetails) {
	if contractDetails == nil {
		return
	}
	event := &DeviceRemoteContractDetails{
		ContractDetails: newContractDetailsRpc(contractDetails),
	}
	key := "DeviceRemoteRpc.EgressContractDetailsChanged"
	if contractDetails.ContractId != nil {
		key += "/" + contractDetails.ContractId.IdStr
	}
	self.enqueueReverse(key, func() {
		self.reverseCall("DeviceRemoteRpc.EgressContractDetailsChanged", event)
	})
}

func (self *DeviceLocalRpc) AddIngressContractStatsChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addIngressContractStatsChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addIngressContractStatsChangeListener(listenerId connect.Id) {
	self.ingressContractStatsChangeListenerIds[listenerId] = true
	if self.ingressContractStatsChangeListenerSub == nil {
		self.ingressContractStatsChangeListenerSub = self.deviceLocal.AddIngressContractStatsChangeListener(&contractStatsForwarder{rpc: self, changed: self.ingressContractStatsChanged})
	}
}

func (self *DeviceLocalRpc) RemoveIngressContractStatsChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeIngressContractStatsChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeIngressContractStatsChangeListener(listenerId connect.Id) {
	delete(self.ingressContractStatsChangeListenerIds, listenerId)
	if len(self.ingressContractStatsChangeListenerIds) == 0 && self.ingressContractStatsChangeListenerSub != nil {
		self.ingressContractStatsChangeListenerSub.Close()
		self.ingressContractStatsChangeListenerSub = nil
	}
}

// enqueues an async, coalescing reverse notification (see sendLoop)
func (self *DeviceLocalRpc) ingressContractStatsChanged(contractStats *ContractStats) {
	stats := &DeviceRemoteContractStats{
		ContractStats: contractStats,
	}
	self.reverseNotify("DeviceRemoteRpc.IngressContractStatsChanged", stats)
}

func (self *DeviceLocalRpc) AddIngressContractDetailsChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addIngressContractDetailsChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addIngressContractDetailsChangeListener(listenerId connect.Id) {
	self.ingressContractDetailsChangeListenerIds[listenerId] = true
	if self.ingressContractDetailsChangeListenerSub == nil {
		self.ingressContractDetailsChangeListenerSub = self.deviceLocal.AddIngressContractDetailsChangeListener(&contractDetailsForwarder{rpc: self, changed: self.ingressContractDetailsChanged})
	}
}

func (self *DeviceLocalRpc) RemoveIngressContractDetailsChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeIngressContractDetailsChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeIngressContractDetailsChangeListener(listenerId connect.Id) {
	delete(self.ingressContractDetailsChangeListenerIds, listenerId)
	if len(self.ingressContractDetailsChangeListenerIds) == 0 && self.ingressContractDetailsChangeListenerSub != nil {
		self.ingressContractDetailsChangeListenerSub.Close()
		self.ingressContractDetailsChangeListenerSub = nil
	}
}

// enqueues an async, coalescing reverse notification (see sendLoop).
// see the egress note about the per contract id coalescing
func (self *DeviceLocalRpc) ingressContractDetailsChanged(contractDetails *ContractDetails) {
	if contractDetails == nil {
		return
	}
	event := &DeviceRemoteContractDetails{
		ContractDetails: newContractDetailsRpc(contractDetails),
	}
	key := "DeviceRemoteRpc.IngressContractDetailsChanged"
	if contractDetails.ContractId != nil {
		key += "/" + contractDetails.ContractId.IdStr
	}
	self.enqueueReverse(key, func() {
		self.reverseCall("DeviceRemoteRpc.IngressContractDetailsChanged", event)
	})
}

// provider packet stats

func (self *DeviceLocalRpc) GetProviderPacketStats(_ RpcNoArg, stats **DeviceRemotePacketStats) error {
	*stats = &DeviceRemotePacketStats{
		PacketStats: newPacketStatsRpc(self.deviceLocal.GetProviderPacketStats(), true),
	}
	return nil
}

func (self *DeviceLocalRpc) AddProviderPacketStatsChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addProviderPacketStatsChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addProviderPacketStatsChangeListener(listenerId connect.Id) {
	self.providerPacketStatsChangeListenerIds[listenerId] = true
	if self.providerPacketStatsChangeListenerSub == nil {
		self.providerPacketStatsChangeListenerSub = self.deviceLocal.AddProviderPacketStatsChangeListener(&packetStatsForwarder{rpc: self, changed: self.providerPacketStatsChanged})
	}
}

func (self *DeviceLocalRpc) RemoveProviderPacketStatsChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeProviderPacketStatsChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeProviderPacketStatsChangeListener(listenerId connect.Id) {
	delete(self.providerPacketStatsChangeListenerIds, listenerId)
	if len(self.providerPacketStatsChangeListenerIds) == 0 && self.providerPacketStatsChangeListenerSub != nil {
		self.providerPacketStatsChangeListenerSub.Close()
		self.providerPacketStatsChangeListenerSub = nil
	}
}

// enqueues an async, coalescing reverse notification (see sendLoop)
func (self *DeviceLocalRpc) providerPacketStatsChanged(packetStats *PacketStats) {
	stats := &DeviceRemotePacketStats{
		PacketStats: newPacketStatsRpc(packetStats, true),
	}
	self.reverseNotify("DeviceRemoteRpc.ProviderPacketStatsChanged", stats)
}

// provider contract stats

func (self *DeviceLocalRpc) GetProviderEgressContractStats(_ RpcNoArg, stats **DeviceRemoteContractStats) error {
	*stats = &DeviceRemoteContractStats{
		ContractStats: self.deviceLocal.GetProviderEgressContractStats(),
	}
	return nil
}

func (self *DeviceLocalRpc) GetProviderEgressContractDetails(_ RpcNoArg, details **DeviceRemoteContractDetailsList) error {
	contractDetailsList := self.deviceLocal.GetProviderEgressContractDetails()
	*details = &DeviceRemoteContractDetailsList{
		ContractDetailsList: newContractDetailsListRpc(contractDetailsList),
		Nil:                 contractDetailsList == nil,
	}
	return nil
}

func (self *DeviceLocalRpc) GetProviderIngressContractStats(_ RpcNoArg, stats **DeviceRemoteContractStats) error {
	*stats = &DeviceRemoteContractStats{
		ContractStats: self.deviceLocal.GetProviderIngressContractStats(),
	}
	return nil
}

func (self *DeviceLocalRpc) GetProviderIngressContractDetails(_ RpcNoArg, details **DeviceRemoteContractDetailsList) error {
	contractDetailsList := self.deviceLocal.GetProviderIngressContractDetails()
	*details = &DeviceRemoteContractDetailsList{
		ContractDetailsList: newContractDetailsListRpc(contractDetailsList),
		Nil:                 contractDetailsList == nil,
	}
	return nil
}

func (self *DeviceLocalRpc) AddProviderEgressContractStatsChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addProviderEgressContractStatsChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addProviderEgressContractStatsChangeListener(listenerId connect.Id) {
	self.providerEgressContractStatsChangeListenerIds[listenerId] = true
	if self.providerEgressContractStatsChangeListenerSub == nil {
		self.providerEgressContractStatsChangeListenerSub = self.deviceLocal.AddProviderEgressContractStatsChangeListener(&contractStatsForwarder{rpc: self, changed: self.providerEgressContractStatsChanged})
	}
}

func (self *DeviceLocalRpc) RemoveProviderEgressContractStatsChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeProviderEgressContractStatsChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeProviderEgressContractStatsChangeListener(listenerId connect.Id) {
	delete(self.providerEgressContractStatsChangeListenerIds, listenerId)
	if len(self.providerEgressContractStatsChangeListenerIds) == 0 && self.providerEgressContractStatsChangeListenerSub != nil {
		self.providerEgressContractStatsChangeListenerSub.Close()
		self.providerEgressContractStatsChangeListenerSub = nil
	}
}

// enqueues an async, coalescing reverse notification (see sendLoop)
func (self *DeviceLocalRpc) providerEgressContractStatsChanged(contractStats *ContractStats) {
	stats := &DeviceRemoteContractStats{
		ContractStats: contractStats,
	}
	self.reverseNotify("DeviceRemoteRpc.ProviderEgressContractStatsChanged", stats)
}

func (self *DeviceLocalRpc) AddProviderEgressContractDetailsChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addProviderEgressContractDetailsChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addProviderEgressContractDetailsChangeListener(listenerId connect.Id) {
	self.providerEgressContractDetailsChangeListenerIds[listenerId] = true
	if self.providerEgressContractDetailsChangeListenerSub == nil {
		self.providerEgressContractDetailsChangeListenerSub = self.deviceLocal.AddProviderEgressContractDetailsChangeListener(&contractDetailsForwarder{rpc: self, changed: self.providerEgressContractDetailsChanged})
	}
}

func (self *DeviceLocalRpc) RemoveProviderEgressContractDetailsChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeProviderEgressContractDetailsChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeProviderEgressContractDetailsChangeListener(listenerId connect.Id) {
	delete(self.providerEgressContractDetailsChangeListenerIds, listenerId)
	if len(self.providerEgressContractDetailsChangeListenerIds) == 0 && self.providerEgressContractDetailsChangeListenerSub != nil {
		self.providerEgressContractDetailsChangeListenerSub.Close()
		self.providerEgressContractDetailsChangeListenerSub = nil
	}
}

// enqueues an async, coalescing reverse notification (see sendLoop).
// see the egress note about the per contract id coalescing
func (self *DeviceLocalRpc) providerEgressContractDetailsChanged(contractDetails *ContractDetails) {
	if contractDetails == nil {
		return
	}
	event := &DeviceRemoteContractDetails{
		ContractDetails: newContractDetailsRpc(contractDetails),
	}
	key := "DeviceRemoteRpc.ProviderEgressContractDetailsChanged"
	if contractDetails.ContractId != nil {
		key += "/" + contractDetails.ContractId.IdStr
	}
	self.enqueueReverse(key, func() {
		self.reverseCall("DeviceRemoteRpc.ProviderEgressContractDetailsChanged", event)
	})
}

func (self *DeviceLocalRpc) AddProviderIngressContractStatsChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addProviderIngressContractStatsChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addProviderIngressContractStatsChangeListener(listenerId connect.Id) {
	self.providerIngressContractStatsChangeListenerIds[listenerId] = true
	if self.providerIngressContractStatsChangeListenerSub == nil {
		self.providerIngressContractStatsChangeListenerSub = self.deviceLocal.AddProviderIngressContractStatsChangeListener(&contractStatsForwarder{rpc: self, changed: self.providerIngressContractStatsChanged})
	}
}

func (self *DeviceLocalRpc) RemoveProviderIngressContractStatsChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeProviderIngressContractStatsChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeProviderIngressContractStatsChangeListener(listenerId connect.Id) {
	delete(self.providerIngressContractStatsChangeListenerIds, listenerId)
	if len(self.providerIngressContractStatsChangeListenerIds) == 0 && self.providerIngressContractStatsChangeListenerSub != nil {
		self.providerIngressContractStatsChangeListenerSub.Close()
		self.providerIngressContractStatsChangeListenerSub = nil
	}
}

// enqueues an async, coalescing reverse notification (see sendLoop)
func (self *DeviceLocalRpc) providerIngressContractStatsChanged(contractStats *ContractStats) {
	stats := &DeviceRemoteContractStats{
		ContractStats: contractStats,
	}
	self.reverseNotify("DeviceRemoteRpc.ProviderIngressContractStatsChanged", stats)
}

func (self *DeviceLocalRpc) AddProviderIngressContractDetailsChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addProviderIngressContractDetailsChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addProviderIngressContractDetailsChangeListener(listenerId connect.Id) {
	self.providerIngressContractDetailsChangeListenerIds[listenerId] = true
	if self.providerIngressContractDetailsChangeListenerSub == nil {
		self.providerIngressContractDetailsChangeListenerSub = self.deviceLocal.AddProviderIngressContractDetailsChangeListener(&contractDetailsForwarder{rpc: self, changed: self.providerIngressContractDetailsChanged})
	}
}

func (self *DeviceLocalRpc) RemoveProviderIngressContractDetailsChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeProviderIngressContractDetailsChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeProviderIngressContractDetailsChangeListener(listenerId connect.Id) {
	delete(self.providerIngressContractDetailsChangeListenerIds, listenerId)
	if len(self.providerIngressContractDetailsChangeListenerIds) == 0 && self.providerIngressContractDetailsChangeListenerSub != nil {
		self.providerIngressContractDetailsChangeListenerSub.Close()
		self.providerIngressContractDetailsChangeListenerSub = nil
	}
}

// enqueues an async, coalescing reverse notification (see sendLoop).
// see the egress note about the per contract id coalescing
func (self *DeviceLocalRpc) providerIngressContractDetailsChanged(contractDetails *ContractDetails) {
	if contractDetails == nil {
		return
	}
	event := &DeviceRemoteContractDetails{
		ContractDetails: newContractDetailsRpc(contractDetails),
	}
	key := "DeviceRemoteRpc.ProviderIngressContractDetailsChanged"
	if contractDetails.ContractId != nil {
		key += "/" + contractDetails.ContractId.IdStr
	}
	self.enqueueReverse(key, func() {
		self.reverseCall("DeviceRemoteRpc.ProviderIngressContractDetailsChanged", event)
	})
}

// dns

func (self *DeviceLocalRpc) SetDnsResolverSettings(deviceSettings *DeviceRemoteDnsResolverSettings, _ RpcVoid) error {
	self.deviceLocal.SetDnsResolverSettings(deviceSettings.DnsResolverSettings.toDnsResolverSettings())
	return nil
}

func (self *DeviceLocalRpc) GetDnsResolverSettings(_ RpcNoArg, deviceSettings **DeviceRemoteDnsResolverSettings) error {
	*deviceSettings = &DeviceRemoteDnsResolverSettings{
		DnsResolverSettings: newDnsResolverSettingsRpc(self.deviceLocal.GetDnsResolverSettings()),
	}
	return nil
}

func (self *DeviceLocalRpc) AddDnsResolverSettingsChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addDnsResolverSettingsChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addDnsResolverSettingsChangeListener(listenerId connect.Id) {
	self.dnsResolverSettingsChangeListenerIds[listenerId] = true
	if self.dnsResolverSettingsChangeListenerSub == nil {
		self.dnsResolverSettingsChangeListenerSub = self.deviceLocal.AddDnsResolverSettingsChangeListener(self)
	}
}

func (self *DeviceLocalRpc) RemoveDnsResolverSettingsChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeDnsResolverSettingsChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeDnsResolverSettingsChangeListener(listenerId connect.Id) {
	delete(self.dnsResolverSettingsChangeListenerIds, listenerId)
	if len(self.dnsResolverSettingsChangeListenerIds) == 0 && self.dnsResolverSettingsChangeListenerSub != nil {
		self.dnsResolverSettingsChangeListenerSub.Close()
		self.dnsResolverSettingsChangeListenerSub = nil
	}
}

// DnsResolverSettingsChangeListener
func (self *DeviceLocalRpc) DnsResolverSettingsChanged(dnsResolverSettings *DnsResolverSettings) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.dnsResolverSettingsChanged(dnsResolverSettings)
}

// enqueues an async, coalescing reverse notification (see sendLoop)
func (self *DeviceLocalRpc) dnsResolverSettingsChanged(dnsResolverSettings *DnsResolverSettings) {
	event := &DeviceRemoteDnsResolverSettings{
		DnsResolverSettings: newDnsResolverSettingsRpc(dnsResolverSettings),
	}
	self.reverseNotify("DeviceRemoteRpc.DnsResolverSettingsChanged", event)
}

// network peers

func (self *DeviceLocalRpc) GetNetworkPeers(_ RpcNoArg, peers **DeviceRemoteNetworkPeers) error {
	*peers = &DeviceRemoteNetworkPeers{
		NetworkPeers: newNetworkPeersRpc(self.deviceLocal.GetNetworkPeers()),
	}
	return nil
}

func (self *DeviceLocalRpc) AddNetworkPeersChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addNetworkPeersChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addNetworkPeersChangeListener(listenerId connect.Id) {
	self.networkPeersChangeListenerIds[listenerId] = true
	if self.networkPeersChangeListenerSub == nil {
		self.networkPeersChangeListenerSub = self.deviceLocal.AddNetworkPeersChangeListener(self)
	}
}

func (self *DeviceLocalRpc) RemoveNetworkPeersChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeNetworkPeersChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeNetworkPeersChangeListener(listenerId connect.Id) {
	delete(self.networkPeersChangeListenerIds, listenerId)
	if len(self.networkPeersChangeListenerIds) == 0 && self.networkPeersChangeListenerSub != nil {
		self.networkPeersChangeListenerSub.Close()
		self.networkPeersChangeListenerSub = nil
	}
}

// NetworkPeersChangeListener
func (self *DeviceLocalRpc) NetworkPeersChanged(networkPeers *NetworkPeers) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.networkPeersChanged(networkPeers)
}

// enqueues an async, coalescing reverse notification (see sendLoop)
func (self *DeviceLocalRpc) networkPeersChanged(networkPeers *NetworkPeers) {
	event := &DeviceRemoteNetworkPeers{
		NetworkPeers: newNetworkPeersRpc(networkPeers),
	}
	self.reverseNotify("DeviceRemoteRpc.NetworkPeersChanged", event)
}

// transport settings

func (self *DeviceLocalRpc) AddTransportSettingsChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addTransportSettingsChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addTransportSettingsChangeListener(listenerId connect.Id) {
	self.transportSettingsChangeListenerIds[listenerId] = true
	if self.transportSettingsChangeListenerSub == nil {
		self.transportSettingsChangeListenerSub = self.deviceLocal.AddTransportSettingsChangeListener(self)
	}
}

func (self *DeviceLocalRpc) RemoveTransportSettingsChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeTransportSettingsChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeTransportSettingsChangeListener(listenerId connect.Id) {
	delete(self.transportSettingsChangeListenerIds, listenerId)
	if len(self.transportSettingsChangeListenerIds) == 0 && self.transportSettingsChangeListenerSub != nil {
		self.transportSettingsChangeListenerSub.Close()
		self.transportSettingsChangeListenerSub = nil
	}
}

// TransportSettingsChangeListener
func (self *DeviceLocalRpc) TransportSettingsChanged(transportSettings *TransportSettings) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.transportSettingsChanged(transportSettings)
}

// enqueues an async, coalescing reverse notification (see sendLoop)
func (self *DeviceLocalRpc) transportSettingsChanged(transportSettings *TransportSettings) {
	event := &DeviceRemoteTransportSettingsRpc{
		TransportSettings: newTransportSettingsRpc(transportSettings, false),
		TransportStatus:   newTransportStatusRpc(self.deviceLocal.GetTransportStatus()),
	}
	self.reverseNotify("DeviceRemoteRpc.TransportSettingsChanged", event)
}

func (self *DeviceLocalRpc) AddProviderTransportSettingsChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addProviderTransportSettingsChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addProviderTransportSettingsChangeListener(listenerId connect.Id) {
	self.providerTransportSettingsChangeListenerIds[listenerId] = true
	if self.providerTransportSettingsChangeListenerSub == nil {
		self.providerTransportSettingsChangeListenerSub = self.deviceLocal.AddProviderTransportSettingsChangeListener(self)
	}
}

func (self *DeviceLocalRpc) RemoveProviderTransportSettingsChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeProviderTransportSettingsChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeProviderTransportSettingsChangeListener(listenerId connect.Id) {
	delete(self.providerTransportSettingsChangeListenerIds, listenerId)
	if len(self.providerTransportSettingsChangeListenerIds) == 0 && self.providerTransportSettingsChangeListenerSub != nil {
		self.providerTransportSettingsChangeListenerSub.Close()
		self.providerTransportSettingsChangeListenerSub = nil
	}
}

// ProviderTransportSettingsChangeListener
func (self *DeviceLocalRpc) ProviderTransportSettingsChanged(transportSettings *TransportSettings) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.providerTransportSettingsChanged(transportSettings)
}

// enqueues an async, coalescing reverse notification (see sendLoop)
func (self *DeviceLocalRpc) providerTransportSettingsChanged(transportSettings *TransportSettings) {
	event := &DeviceRemoteTransportSettingsRpc{
		TransportSettings: newTransportSettingsRpc(transportSettings, true),
		TransportStatus:   newTransportStatusRpc(self.deviceLocal.GetProviderTransportStatus()),
	}
	self.reverseNotify("DeviceRemoteRpc.ProviderTransportSettingsChanged", event)
}

func (self *DeviceLocalRpc) GetTransportSettings(_ RpcNoArg, deviceSettings **DeviceRemoteTransportSettingsRpc) error {
	*deviceSettings = &DeviceRemoteTransportSettingsRpc{
		TransportSettings: newTransportSettingsRpc(self.deviceLocal.GetTransportSettings(), false),
		TransportStatus:   newTransportStatusRpc(self.deviceLocal.GetTransportStatus()),
	}
	return nil
}

func (self *DeviceLocalRpc) SetTransportSettings(deviceSettings *DeviceRemoteTransportSettingsRpc, _ RpcVoid) error {
	if self.hostedIncompatibleRpcGuarded("SetTransportSettings") {
		return nil
	}
	var settings *TransportSettingsRpc
	if deviceSettings != nil {
		settings = deviceSettings.TransportSettings
	}
	self.deviceLocal.SetTransportSettings(settings.toTransportSettings(false))
	return nil
}

func (self *DeviceLocalRpc) GetProviderTransportSettings(_ RpcNoArg, deviceSettings **DeviceRemoteTransportSettingsRpc) error {
	*deviceSettings = &DeviceRemoteTransportSettingsRpc{
		TransportSettings: newTransportSettingsRpc(self.deviceLocal.GetProviderTransportSettings(), true),
		TransportStatus:   newTransportStatusRpc(self.deviceLocal.GetProviderTransportStatus()),
	}
	return nil
}

func (self *DeviceLocalRpc) SetProviderTransportSettings(deviceSettings *DeviceRemoteTransportSettingsRpc, _ RpcVoid) error {
	if self.hostedIncompatibleRpcGuarded("SetProviderTransportSettings") {
		return nil
	}
	var settings *TransportSettingsRpc
	if deviceSettings != nil {
		settings = deviceSettings.TransportSettings
	}
	self.deviceLocal.SetProviderTransportSettings(settings.toTransportSettings(true))
	return nil
}

// reliability (see reliability_controls.go for the `DeviceLocal` side)

func (self *DeviceLocalRpc) GetReliabilitySettings(_ RpcNoArg, deviceSettings **DeviceRemoteReliabilitySettingsRpc) error {
	*deviceSettings = &DeviceRemoteReliabilitySettingsRpc{
		ReliabilitySettings: self.deviceLocal.GetReliabilitySettings(),
	}
	return nil
}

func (self *DeviceLocalRpc) SetReliabilitySettings(deviceSettings *DeviceRemoteReliabilitySettingsRpc, _ RpcVoid) error {
	if deviceSettings.ReliabilitySettings == nil {
		// a nil override is a reset; `DeviceLocal.SetReliabilitySettings`
		// does not accept nil
		self.deviceLocal.ResetReliabilitySettings()
		return nil
	}
	self.deviceLocal.SetReliabilitySettings(deviceSettings.ReliabilitySettings)
	return nil
}

func (self *DeviceLocalRpc) ResetReliabilitySettings(_ RpcNoArg, _ RpcVoid) error {
	self.deviceLocal.ResetReliabilitySettings()
	return nil
}

func (self *DeviceLocalRpc) GetReliabilityMetrics(_ RpcNoArg, deviceMetrics **DeviceRemoteReliabilityMetricsRpc) error {
	*deviceMetrics = &DeviceRemoteReliabilityMetricsRpc{
		ReliabilityMetrics: self.deviceLocal.GetReliabilityMetrics(),
	}
	return nil
}

func (self *DeviceLocalRpc) ResetReliabilityMetrics(_ RpcNoArg, _ RpcVoid) error {
	self.deviceLocal.ResetReliabilityMetrics()
	return nil
}

func (self *DeviceLocalRpc) GetExits(_ RpcNoArg, deviceExits **DeviceRemoteExitListRpc) error {
	*deviceExits = &DeviceRemoteExitListRpc{
		Exits: newExitListRpc(self.deviceLocal.GetExits()),
	}
	return nil
}

func (self *DeviceLocalRpc) GetDestinationExits(_ RpcNoArg, deviceDestinationExits **DeviceRemoteDestinationExitListRpc) error {
	*deviceDestinationExits = &DeviceRemoteDestinationExitListRpc{
		DestinationExits: newDestinationExitListRpc(self.deviceLocal.GetDestinationExits()),
	}
	return nil
}

func (self *DeviceLocalRpc) MigrateExit(exitClientId connect.Id, migratedCount *int32) error {
	*migratedCount = self.deviceLocal.MigrateExit(newId(exitClientId))
	return nil
}

func (self *DeviceLocalRpc) ProbeAllExits(_ RpcNoArg, probeCount *int32) error {
	*probeCount = self.deviceLocal.ProbeAllExits()
	return nil
}

func (self *DeviceLocalRpc) DropExit(exitClientId connect.Id, dropped *bool) error {
	*dropped = self.deviceLocal.DropExit(newId(exitClientId))
	return nil
}

func (self *DeviceLocalRpc) StallExit(stallExit *DeviceRemoteStallExitRpc, set *bool) error {
	*set = self.deviceLocal.StallExit(newId(stallExit.ClientId), stallExit.Stalled)
	return nil
}

func (self *DeviceLocalRpc) ShuffleExits(_ RpcNoArg, _ RpcVoid) error {
	self.deviceLocal.ShuffleExits()
	return nil
}

// the nil config is passed through rather than defaulted here:
// `DeviceLocal.StartProbeSuite` normalizes it (nil -> default, every field
// bounded), so the rpc path and the direct gomobile/cgo path cannot disagree
// about what an out-of-range or absent config means
func (self *DeviceLocalRpc) StartProbeSuite(probeSuiteConfig *DeviceRemoteProbeSuiteConfigRpc, started *bool) error {
	*started = self.deviceLocal.StartProbeSuite(probeSuiteConfig.Config)
	return nil
}

func (self *DeviceLocalRpc) StopProbeSuite(_ RpcNoArg, _ RpcVoid) error {
	self.deviceLocal.StopProbeSuite()
	return nil
}

func (self *DeviceLocalRpc) ProbeSuiteRunning(_ RpcNoArg, running *bool) error {
	*running = self.deviceLocal.ProbeSuiteRunning()
	return nil
}

func (self *DeviceLocalRpc) GetProbeResults(_ RpcNoArg, deviceProbeResults **DeviceRemoteProbeResultListRpc) error {
	*deviceProbeResults = &DeviceRemoteProbeResultListRpc{
		Results: newProbeResultListRpc(self.deviceLocal.GetProbeResults()),
	}
	return nil
}

func (self *DeviceLocalRpc) SimulateNetworkChange(_ RpcNoArg, _ RpcVoid) error {
	self.deviceLocal.SimulateNetworkChange()
	return nil
}

func (self *DeviceLocalRpc) GetStats(_ RpcNoArg, stats **DeviceStats) error {
	*stats = self.deviceLocal.GetStats()
	return nil
}

// func (self *DeviceLocalRpc) RefreshToken(attempt int) error {

// 	self.deviceLocal.log.Infof("[dlrpc]RefreshToken attempt=%d", attempt)

// 	return self.deviceLocal.RefreshToken(attempt)
// }

/**
 * Ratings Dialog
 */

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

func (self *DeviceLocalRpc) AddCanShowRatingDialogChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addCanShowRatingDialogChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addCanShowRatingDialogChangeListener(listenerId connect.Id) {
	self.canShowRatingDialogChangeListenerIds[listenerId] = true
	if self.canShowRatingDialogChangeListenerSub == nil {
		self.canShowRatingDialogChangeListenerSub = self.deviceLocal.AddCanShowRatingDialogChangeListener(self)
	}
}

func (self *DeviceLocalRpc) RemoveCanShowRatingDialogChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeCanShowRatingDialogChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeCanShowRatingDialogChangeListener(listenerId connect.Id) {
	delete(self.canShowRatingDialogChangeListenerIds, listenerId)
	if len(self.canShowRatingDialogChangeListenerIds) == 0 && self.canShowRatingDialogChangeListenerSub != nil {
		self.canShowRatingDialogChangeListenerSub.Close()
		self.canShowRatingDialogChangeListenerSub = nil
	}
}

// CanShowRatingDialogChangeListener
func (self *DeviceLocalRpc) CanShowRatingDialogChanged(canShowRatingDialog bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.canShowRatingDialogChanged(canShowRatingDialog)
}

// enqueues an async, coalescing reverse notification (see sendLoop)
func (self *DeviceLocalRpc) canShowRatingDialogChanged(canShowRatingDialog bool) {
	self.reverseNotify("DeviceRemoteRpc.CanShowRatingDialogChanged", canShowRatingDialog)
}

func (self *DeviceLocalRpc) UploadLogs(feedbackId string, _ RpcVoid) error {
	self.deviceLocal.UploadLogs(feedbackId, nil)
	return nil
}

/**
 * Intro Funnel Prompt
 */

func (self *DeviceLocalRpc) GetCanPromptIntroFunnel(_ RpcNoArg, canPromptIntroFunnel *bool) error {
	*canPromptIntroFunnel = self.deviceLocal.GetCanPromptIntroFunnel()
	return nil
}

func (self *DeviceLocalRpc) SetCanPromptIntroFunnel(canPromptIntroFunnel bool, _ RpcVoid) error {
	self.deviceLocal.SetCanPromptIntroFunnel(canPromptIntroFunnel)
	return nil
}

func (self *DeviceLocalRpc) AddCanPromptIntroFunnelChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addCanPromptIntroFunnelChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addCanPromptIntroFunnelChangeListener(listenerId connect.Id) {
	self.canPromptIntroFunnelChangeListenerIds[listenerId] = true
	if self.canPromptIntroFunnelChangeListenerSub == nil {
		self.canPromptIntroFunnelChangeListenerSub = self.deviceLocal.AddCanPromptIntroFunnelChangeListener(self)
	}
}

func (self *DeviceLocalRpc) RemoveCanPromptIntroFunnelChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeCanPromptIntroFunnelChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeCanPromptIntroFunnelChangeListener(listenerId connect.Id) {
	delete(self.canPromptIntroFunnelChangeListenerIds, listenerId)
	if len(self.canPromptIntroFunnelChangeListenerIds) == 0 && self.canPromptIntroFunnelChangeListenerSub != nil {
		self.canPromptIntroFunnelChangeListenerSub.Close()
		self.canPromptIntroFunnelChangeListenerSub = nil
	}
}

// CanPromptIntroFunnelChangeListener
func (self *DeviceLocalRpc) CanPromptIntroFunnelChanged(canPromptIntroFunnel bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.canPromptIntroFunnelChanged(canPromptIntroFunnel)
}

// enqueues an async, coalescing reverse notification (see sendLoop)
func (self *DeviceLocalRpc) canPromptIntroFunnelChanged(canPromptIntroFunnel bool) {
	self.reverseNotify("DeviceRemoteRpc.CanPromptIntroFunnelChanged", canPromptIntroFunnel)
}

/**
 * Provide Control Mode
 */

func (self *DeviceLocalRpc) GetProvideControlMode(_ RpcNoArg, mode *ProvideControlMode) error {
	*mode = self.deviceLocal.GetProvideControlMode()
	return nil
}

func (self *DeviceLocalRpc) SetProvideControlMode(mode ProvideControlMode, _ RpcVoid) error {
	if self.hostedIncompatibleRpcGuarded("SetProvideControlMode") {
		return nil
	}
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

func (self *DeviceLocalRpc) AddAllowForegroundChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addAllowForegroundChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addAllowForegroundChangeListener(listenerId connect.Id) {
	self.allowForegroundChangeListenerIds[listenerId] = true
	if self.allowForegroundChangeListenerSub == nil {
		self.allowForegroundChangeListenerSub = self.deviceLocal.AddAllowForegroundChangeListener(self)
	}
}

func (self *DeviceLocalRpc) RemoveAllowForegroundChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeAllowForegroundChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeAllowForegroundChangeListener(listenerId connect.Id) {
	delete(self.allowForegroundChangeListenerIds, listenerId)
	if len(self.allowForegroundChangeListenerIds) == 0 && self.allowForegroundChangeListenerSub != nil {
		self.allowForegroundChangeListenerSub.Close()
		self.allowForegroundChangeListenerSub = nil
	}
}

// AllowForegroundChangeListener
func (self *DeviceLocalRpc) AllowForegroundChanged(allowForeground bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.allowForegroundChanged(allowForeground)
}

// enqueues an async, coalescing reverse notification (see sendLoop)
func (self *DeviceLocalRpc) allowForegroundChanged(allowForeground bool) {
	self.reverseNotify("DeviceRemoteRpc.AllowForegroundChanged", allowForeground)
}

func (self *DeviceLocalRpc) GetCanRefer(_ RpcNoArg, canRefer *bool) error {
	*canRefer = self.deviceLocal.GetCanRefer()
	return nil
}

func (self *DeviceLocalRpc) SetCanRefer(canRefer bool, _ RpcVoid) error {
	self.deviceLocal.SetCanRefer(canRefer)
	return nil
}

func (self *DeviceLocalRpc) AddCanReferChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addCanReferChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addCanReferChangeListener(listenerId connect.Id) {
	self.canReferChangeListenerIds[listenerId] = true
	if self.canReferChangeListenerSub == nil {
		self.canReferChangeListenerSub = self.deviceLocal.AddCanReferChangeListener(self)
	}
}

func (self *DeviceLocalRpc) RemoveCanReferChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeCanReferChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeCanReferChangeListener(listenerId connect.Id) {
	delete(self.canReferChangeListenerIds, listenerId)
	if len(self.canReferChangeListenerIds) == 0 && self.canReferChangeListenerSub != nil {
		self.canReferChangeListenerSub.Close()
		self.canReferChangeListenerSub = nil
	}
}

// CanReferChangeListener
func (self *DeviceLocalRpc) CanReferChanged(canRefer bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.canReferChanged(canRefer)
}

// enqueues an async, coalescing reverse notification (see sendLoop)
func (self *DeviceLocalRpc) canReferChanged(canRefer bool) {
	self.reverseNotify("DeviceRemoteRpc.CanReferChanged", canRefer)
}

func (self *DeviceLocalRpc) SetRouteLocal(routeLocal bool, _ RpcVoid) error {
	if self.hostedIncompatibleRpcGuarded("SetRouteLocal") {
		return nil
	}
	self.deviceLocal.SetRouteLocal(routeLocal)
	return nil
}

func (self *DeviceLocalRpc) GetRouteLocal(_ RpcNoArg, routeLocal *bool) error {
	*routeLocal = self.deviceLocal.GetRouteLocal()
	return nil
}

func (self *DeviceLocalRpc) SetBlockerEnabled(blockerEnabled bool, _ RpcVoid) error {
	self.deviceLocal.SetBlockerEnabled(blockerEnabled)
	return nil
}

func (self *DeviceLocalRpc) GetBlockerEnabled(_ RpcNoArg, blockerEnabled *bool) error {
	*blockerEnabled = self.deviceLocal.GetBlockerEnabled()
	return nil
}

func (self *DeviceLocalRpc) SetPerformanceProfile(devicePerformanceProfile *DevicePerformanceProfile, _ RpcVoid) error {
	self.deviceLocal.SetPerformanceProfile(devicePerformanceProfile.PerformanceProfile)
	return nil
}

func (self *DeviceLocalRpc) GetPublicIdentityKey(_ RpcNoArg, devicePublicIdentityKey **DevicePublicIdentityKey) error {
	*devicePublicIdentityKey = &DevicePublicIdentityKey{
		PublicKey: self.deviceLocal.GetPublicIdentityKey(),
	}
	return nil
}

func (self *DeviceLocalRpc) GetProviderIdentities(_ RpcNoArg, deviceProviderIdentities **DeviceProviderIdentities) error {
	*deviceProviderIdentities = newDeviceProviderIdentities(self.deviceLocal.GetProviderIdentities())
	return nil
}

func (self *DeviceLocalRpc) GetPerformanceProfile(_ RpcNoArg, devicePerformanceProfile **DevicePerformanceProfile) error {
	*devicePerformanceProfile = &DevicePerformanceProfile{
		PerformanceProfile: self.deviceLocal.GetPerformanceProfile(),
	}
	return nil
}

func (self *DeviceLocalRpc) AddProvideModeChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addProvideModeChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addProvideModeChangeListener(listenerId connect.Id) {
	self.provideModeChangeListenerIds[listenerId] = true
	if self.provideModeChangeListenerSub == nil {
		self.provideModeChangeListenerSub = self.deviceLocal.AddProvideModeChangeListener(self)
	}
}

func (self *DeviceLocalRpc) RemoveProvideModeChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeProvideModeChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeProvideModeChangeListener(listenerId connect.Id) {
	delete(self.provideModeChangeListenerIds, listenerId)
	if len(self.provideModeChangeListenerIds) == 0 && self.provideModeChangeListenerSub != nil {
		self.provideModeChangeListenerSub.Close()
		self.provideModeChangeListenerSub = nil
	}
}

// ProvideModeChangeListener
func (self *DeviceLocalRpc) ProvideModeChanged(provideMode ProvideMode) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.provideModeChanged(provideMode)
}

// enqueues an async, coalescing reverse notification (see sendLoop)
func (self *DeviceLocalRpc) provideModeChanged(provideMode ProvideMode) {
	self.reverseNotify("DeviceRemoteRpc.ProvideModeChanged", provideMode)
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

// enqueues an async, coalescing reverse notification (see sendLoop)
func (self *DeviceLocalRpc) provideChanged(provideEnabled bool) {
	self.reverseNotify("DeviceRemoteRpc.ProvideChanged", provideEnabled)
}

func (self *DeviceLocalRpc) AddProvideControlModeChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addProvideControlModeChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addProvideControlModeChangeListener(listenerId connect.Id) {
	self.provideControlModeChangeListenerIds[listenerId] = true
	if self.provideControlModeChangeListenerSub == nil {
		self.provideControlModeChangeListenerSub = self.deviceLocal.AddProvideControlModeChangeListener(self)
	}
}

func (self *DeviceLocalRpc) RemoveProvideControlModeChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeProvideControlModeChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeProvideControlModeChangeListener(listenerId connect.Id) {
	delete(self.provideControlModeChangeListenerIds, listenerId)
	if len(self.provideControlModeChangeListenerIds) == 0 && self.provideControlModeChangeListenerSub != nil {
		self.provideControlModeChangeListenerSub.Close()
		self.provideControlModeChangeListenerSub = nil
	}
}

// ProvideControlModeChangeListener
func (self *DeviceLocalRpc) ProvideControlModeChanged(provideControlMode ProvideControlMode) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.provideControlModeChanged(provideControlMode)
}

// enqueues an async, coalescing reverse notification (see sendLoop)
func (self *DeviceLocalRpc) provideControlModeChanged(provideControlMode ProvideControlMode) {
	self.reverseNotify("DeviceRemoteRpc.ProvideControlModeChanged", provideControlMode)
}

func (self *DeviceLocalRpc) AddPerformanceProfileChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addPerformanceProfileChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addPerformanceProfileChangeListener(listenerId connect.Id) {
	self.performanceProfileChangeListenerIds[listenerId] = true
	if self.performanceProfileChangeListenerSub == nil {
		self.performanceProfileChangeListenerSub = self.deviceLocal.AddPerformanceProfileChangeListener(self)
	}
}

func (self *DeviceLocalRpc) RemovePerformanceProfileChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removePerformanceProfileChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removePerformanceProfileChangeListener(listenerId connect.Id) {
	delete(self.performanceProfileChangeListenerIds, listenerId)
	if len(self.performanceProfileChangeListenerIds) == 0 && self.performanceProfileChangeListenerSub != nil {
		self.performanceProfileChangeListenerSub.Close()
		self.performanceProfileChangeListenerSub = nil
	}
}

// PerformanceProfileChangeListener
func (self *DeviceLocalRpc) PerformanceProfileChanged(performanceProfile *PerformanceProfile) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.performanceProfileChanged(performanceProfile)
}

// enqueues an async, coalescing reverse notification (see sendLoop)
func (self *DeviceLocalRpc) performanceProfileChanged(performanceProfile *PerformanceProfile) {
	event := &DevicePerformanceProfile{
		PerformanceProfile: performanceProfile,
	}
	self.reverseNotify("DeviceRemoteRpc.PerformanceProfileChanged", event)
}

func (self *DeviceLocalRpc) AddProviderIdentityChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addProviderIdentityChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addProviderIdentityChangeListener(listenerId connect.Id) {
	self.providerIdentityChangeListenerIds[listenerId] = true
	if self.providerIdentityChangeListenerSub == nil {
		self.providerIdentityChangeListenerSub = self.deviceLocal.AddProviderIdentityChangeListener(self)
	}
}

func (self *DeviceLocalRpc) RemoveProviderIdentityChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeProviderIdentityChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeProviderIdentityChangeListener(listenerId connect.Id) {
	delete(self.providerIdentityChangeListenerIds, listenerId)
	if len(self.providerIdentityChangeListenerIds) == 0 && self.providerIdentityChangeListenerSub != nil {
		self.providerIdentityChangeListenerSub.Close()
		self.providerIdentityChangeListenerSub = nil
	}
}

// ProviderIdentityChangeListener
func (self *DeviceLocalRpc) ProviderIdentitiesChanged() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	// the change event carries no payload; the event sent to the remote
	// carries the current list (same order as `SyncReverse`)
	self.providerIdentitiesChanged(self.deviceLocal.GetProviderIdentities())
}

// enqueues an async, coalescing reverse notification (see sendLoop)
func (self *DeviceLocalRpc) providerIdentitiesChanged(providerIdentities *ProviderIdentityList) {
	event := newDeviceProviderIdentities(providerIdentities)
	self.reverseNotify("DeviceRemoteRpc.ProviderIdentitiesChanged", event)
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

// enqueues an async, coalescing reverse notification (see sendLoop)
func (self *DeviceLocalRpc) providePausedChanged(providePaused bool) {
	self.reverseNotify("DeviceRemoteRpc.ProvidePausedChanged", providePaused)
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

// enqueues an async, coalescing reverse notification (see sendLoop)
func (self *DeviceLocalRpc) offlineChanged(offline bool, vpnInterfaceWhileOffline bool) {
	event := &DeviceRemoteOfflineChangeEvent{
		Offline:                  offline,
		VpnInterfaceWhileOffline: vpnInterfaceWhileOffline,
	}
	self.reverseNotify("DeviceRemoteRpc.OfflineChanged", event)
}

func (self *DeviceLocalRpc) AddVpnInterfaceWhileOfflineChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addVpnInterfaceWhileOfflineChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addVpnInterfaceWhileOfflineChangeListener(listenerId connect.Id) {
	self.vpnInterfaceWhileOfflineChangeListenerIds[listenerId] = true
	if self.vpnInterfaceWhileOfflineChangeListenerSub == nil {
		self.vpnInterfaceWhileOfflineChangeListenerSub = self.deviceLocal.AddVpnInterfaceWhileOfflineChangeListener(self)
	}
}

func (self *DeviceLocalRpc) RemoveVpnInterfaceWhileOfflineChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeVpnInterfaceWhileOfflineChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeVpnInterfaceWhileOfflineChangeListener(listenerId connect.Id) {
	delete(self.vpnInterfaceWhileOfflineChangeListenerIds, listenerId)
	if len(self.vpnInterfaceWhileOfflineChangeListenerIds) == 0 && self.vpnInterfaceWhileOfflineChangeListenerSub != nil {
		self.vpnInterfaceWhileOfflineChangeListenerSub.Close()
		self.vpnInterfaceWhileOfflineChangeListenerSub = nil
	}
}

// VpnInterfaceWhileOfflineChangeListener
func (self *DeviceLocalRpc) VpnInterfaceWhileOfflineChanged(vpnInterfaceWhileOffline bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.vpnInterfaceWhileOfflineChanged(vpnInterfaceWhileOffline)
}

// enqueues an async, coalescing reverse notification (see sendLoop)
func (self *DeviceLocalRpc) vpnInterfaceWhileOfflineChanged(vpnInterfaceWhileOffline bool) {
	self.reverseNotify("DeviceRemoteRpc.VpnInterfaceWhileOfflineChanged", vpnInterfaceWhileOffline)
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

// enqueues an async, coalescing reverse notification (see sendLoop)
func (self *DeviceLocalRpc) connectChanged(connectEnabled bool) {
	self.reverseNotify("DeviceRemoteRpc.ConnectChanged", connectEnabled)
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

// enqueues an async, coalescing reverse notification (see sendLoop)
func (self *DeviceLocalRpc) routeLocalChanged(routeLocal bool) {
	self.reverseNotify("DeviceRemoteRpc.RouteLocalChanged", routeLocal)
}

func (self *DeviceLocalRpc) AddBlockerEnabledChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addBlockerEnabledChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addBlockerEnabledChangeListener(listenerId connect.Id) {
	self.blockerEnabledChangeListenerIds[listenerId] = true
	if self.blockerEnabledChangeListenerSub == nil {
		self.blockerEnabledChangeListenerSub = self.deviceLocal.AddBlockerEnabledChangeListener(self)
	}
}

func (self *DeviceLocalRpc) RemoveBlockerEnabledChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeBlockerEnabledChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeBlockerEnabledChangeListener(listenerId connect.Id) {
	delete(self.blockerEnabledChangeListenerIds, listenerId)
	if len(self.blockerEnabledChangeListenerIds) == 0 && self.blockerEnabledChangeListenerSub != nil {
		self.blockerEnabledChangeListenerSub.Close()
		self.blockerEnabledChangeListenerSub = nil
	}
}

// BlockerEnabledChangeListener
func (self *DeviceLocalRpc) BlockerEnabledChanged(blockerEnabled bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.blockerEnabledChanged(blockerEnabled)
}

// enqueues an async, coalescing reverse notification (see sendLoop)
func (self *DeviceLocalRpc) blockerEnabledChanged(blockerEnabled bool) {
	self.reverseNotify("DeviceRemoteRpc.BlockerEnabledChanged", blockerEnabled)
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
	// D10: a destination change replaces the window monitor (the multi client
	// is recreated). Re-bind the service-side subscription NOW rather than at
	// the next listener add or reconcile pull, so remote grids receive the new
	// window's reset snapshot instead of a gap — and so the generation tag
	// (windowMonitorCallbackFor) has a current generation to compare against.
	self.updateWindowMonitor()
	self.connectLocationChanged(location)
}

// enqueues an async, coalescing reverse notification (see sendLoop)
func (self *DeviceLocalRpc) connectLocationChanged(location *ConnectLocation) {
	event := &DeviceRemoteConnectLocationChangeEvent{
		Location: newDeviceRemoteConnectLocation(location),
	}
	self.reverseNotify("DeviceRemoteRpc.ConnectLocationChanged", event)
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

// enqueues an async, coalescing reverse notification (see sendLoop)
func (self *DeviceLocalRpc) provideSecretKeysChanged(provideSecretKeyList *ProvideSecretKeyList) {
	self.reverseNotify("DeviceRemoteRpc.ProvideSecretKeysChanged", provideSecretKeyList.getAll())
}

// must be called with stateLock
func (self *DeviceLocalRpc) updateWindowMonitor() {
	localWindowMonitor := self.deviceLocal.windowMonitor()
	if self.localWindowMonitor != localWindowMonitor {
		if self.windowMonitorEventListenerSub != nil {
			self.windowMonitorEventListenerSub()
			self.windowMonitorEventListenerSub = nil
		}

		self.localWindowId = connect.NewId()
		self.localWindowMonitor = localWindowMonitor

		// re-bind the registered remote windows to the new local window and
		// resubscribe, so remote listeners keep receiving events across a local
		// monitor change (the multi client is recreated on destination change).
		// without this the old subscription is dropped and nothing subscribes to
		// the new monitor until a listener add, leaving remote grids frozen. a
		// reset snapshot resyncs the remote grids to the new monitor's state.
		clear(self.localWindowIds)
		for windowId := range self.windowMonitorEventListenerIds {
			self.localWindowIds[windowId] = self.localWindowId
		}
		if 0 < len(self.windowMonitorEventListenerIds) {
			self.windowMonitorEventListenerSub = localWindowMonitor.AddMonitorEventCallback(self.windowMonitorCallbackFor(self.localWindowId))
			windowExpandEvent, providerEvents := localWindowMonitor.Events()
			self.windowMonitorEventCallback(windowExpandEvent, providerEvents, true)
		}
	}
}

// windowMonitorCallbackFor tags a monitor subscription with the window
// generation (localWindowId) it was created under — the D10 stale-monitor
// hardening. The monitor's callback workers are asynchronous, so an event
// from a monitor that has since been replaced (the multi client is recreated
// on every destination change) can arrive after the replacement; without the
// tag it would be forwarded to remote listeners stamped with the CURRENT
// window ids, exactly the stale-generation flash the D2 gate exists for. A
// stale event is dropped, and the subscription state is re-derived from the
// live monitor (updateWindowMonitor re-binds and pushes a reset snapshot) so
// remote grids resync to the new window instead of freezing.
func (self *DeviceLocalRpc) windowMonitorCallbackFor(generation connect.Id) connect.MonitorEventFunction {
	return func(windowExpandEvent *connect.WindowExpandEvent, providerEvents map[connect.Id]*connect.ProviderEvent, reset bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.localWindowId != generation {
			// an event from the outgoing monitor generation: drop it, and
			// re-derive from the live monitor (nil deviceLocal is a bare test
			// fixture, which has nothing to re-derive from)
			if self.deviceLocal != nil {
				self.updateWindowMonitor()
			}
			return
		}
		self.windowMonitorEventCallback(windowExpandEvent, providerEvents, reset)
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
			self.windowMonitorEventListenerSub = self.localWindowMonitor.AddMonitorEventCallback(self.windowMonitorCallbackFor(self.localWindowId))
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

// must be called with stateLock (reads windowIds). enqueues an async, merging
// reverse notification (see enqueueWindowMonitorEvent / sendLoop).
func (self *DeviceLocalRpc) windowMonitorEventCallback(
	windowExpandEvent *connect.WindowExpandEvent,
	providerEvents map[connect.Id]*connect.ProviderEvent,
	reset bool,
) {
	self.enqueueWindowMonitorEvent(&DeviceRemoteWindowMonitorEvent{
		WindowIds:         self.windowIds(),
		WindowExpandEvent: windowExpandEvent,
		ProviderEvents:    providerEvents,
		Reset:             reset,
	})
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
	if self.hostedIncompatibleRpcGuarded("SetProvideMode") {
		return nil
	}
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

// enqueues an async, coalescing reverse notification (see sendLoop)
func (self *DeviceLocalRpc) provideNetworkModeChanged(provideNetworkMode ProvideNetworkMode) {
	self.reverseNotify("DeviceRemoteRpc.ProvideNetworkModeChanged", provideNetworkMode)
}

func (self *DeviceLocalRpc) SetProvideNetworkMode(provideNetworkMode ProvideNetworkMode, _ RpcVoid) error {
	if self.hostedIncompatibleRpcGuarded("SetProvideNetworkMode") {
		return nil
	}
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
	if self.hostedIncompatibleRpcGuarded("SetProvidePaused") {
		return nil
	}
	self.deviceLocal.SetProvidePaused(providePaused)
	return nil
}

func (self *DeviceLocalRpc) GetProvidePaused(_ RpcNoArg, providePaused *bool) error {
	*providePaused = self.deviceLocal.GetProvidePaused()
	return nil
}

func (self *DeviceLocalRpc) SetOffline(offline bool, _ RpcVoid) error {
	self.deviceLocal.log.Infof("[dlrpc]set offline")
	self.deviceLocal.SetOffline(offline)
	return nil
}

func (self *DeviceLocalRpc) GetOffline(_ RpcNoArg, offline *bool) error {
	*offline = self.deviceLocal.GetOffline()
	return nil
}

func (self *DeviceLocalRpc) SetVpnInterfaceWhileOffline(vpnInterfaceWhileOffline bool, _ RpcVoid) error {
	if self.hostedIncompatibleRpcGuarded("SetVpnInterfaceWhileOffline") {
		return nil
	}
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
	)
	return nil
}

func (self *DeviceLocalRpc) SetConnectLocation(location *DeviceRemoteConnectLocation, _ RpcVoid) error {
	self.deviceLocal.SetConnectLocation(location.toConnectLocation())
	return nil
}

func (self *DeviceLocalRpc) Reconnect(location *DeviceRemoteConnectLocation, _ RpcVoid) error {
	self.deviceLocal.Reconnect(location.toConnectLocation())
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

func (self *DeviceLocalRpc) AddDefaultLocationChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addDefaultLocationChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) addDefaultLocationChangeListener(listenerId connect.Id) {
	self.defaultLocationChangeListenerIds[listenerId] = true
	if self.defaultLocationChangeListenerSub == nil {
		self.defaultLocationChangeListenerSub = self.deviceLocal.AddDefaultLocationChangeListener(self)
	}
}

func (self *DeviceLocalRpc) RemoveDefaultLocationChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeDefaultLocationChangeListener(listenerId)
	return nil
}

// must be called with stateLock
func (self *DeviceLocalRpc) removeDefaultLocationChangeListener(listenerId connect.Id) {
	delete(self.defaultLocationChangeListenerIds, listenerId)
	if len(self.defaultLocationChangeListenerIds) == 0 && self.defaultLocationChangeListenerSub != nil {
		self.defaultLocationChangeListenerSub.Close()
		self.defaultLocationChangeListenerSub = nil
	}
}

// DefaultLocationChangeListener
func (self *DeviceLocalRpc) DefaultLocationChanged(location *ConnectLocation) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.defaultLocationChanged(location)
}

// enqueues an async, coalescing reverse notification (see sendLoop)
func (self *DeviceLocalRpc) defaultLocationChanged(location *ConnectLocation) {
	event := newDeviceRemoteDefaultLocation(location)
	self.reverseNotify("DeviceRemoteRpc.DefaultLocationChanged", event)
}

func (self *DeviceLocalRpc) Shuffle(_ RpcNoArg, _ RpcVoid) error {
	self.deviceLocal.Shuffle()
	return nil
}

func (self *DeviceLocalRpc) RemoveConnectedProvider(clientId connect.Id, _ RpcVoid) error {
	self.deviceLocal.RemoveConnectedProvider(newId(clientId))
	return nil
}

// tryAcquireHttp admits work before a handler worker is created. It rejects
// excess calls instead of leaving decoded request bodies in an unbounded set
// of goroutines waiting on a semaphore. A current app applies matching
// admission control before encoding requests, so this is primarily defensive
// backpressure for older or incompatible peers.
func (self *DeviceLocalRpc) tryAcquireHttp() (release func(), ok bool) {
	select {
	case self.httpSem <- struct{}{}:
		return func() { <-self.httpSem }, true
	case <-self.ctx.Done():
		return func() {}, false
	default:
		return func() {}, false
	}
}

// acquireHttpDelivery transfers a completed response into a separately
// bounded delivery stage. A worker takes this before releasing its fetch slot,
// so an older peer cannot create an unbounded set of response goroutines while
// the app is suspended. Fetches can still continue after a response begins its
// blocking reverse RPC, avoiding a scheduler handoff race at the fetch limit.
func (self *DeviceLocalRpc) acquireHttpDelivery() (release func(), ok bool) {
	select {
	case self.httpDeliverySem <- struct{}{}:
		return func() { <-self.httpDeliverySem }, true
	case <-self.ctx.Done():
		return func() {}, false
	}
}

func (self *DeviceLocalRpc) HttpPostRaw(httpRequest *DeviceRemoteHttpRequest, _ RpcVoid) error {
	if self.settings.httpMaxBodyBytes() < len(httpRequest.RequestBodyBytes) {
		httpRequest.RequestBodyBytes = nil
		return fmt.Errorf("device rpc http request exceeds %d-byte limit", self.settings.httpMaxBodyBytes())
	}

	release, ok := self.tryAcquireHttp()
	if !ok {
		return fmt.Errorf("device rpc http concurrency limit reached")
	}

	go connect.HandleError(func() {
		fetchReleased := false
		defer func() {
			if !fetchReleased {
				release()
			}
		}()

		bodyBytes, err := connect.HttpPostWithStrategyRaw(
			self.ctx,
			self.deviceLocal.clientStrategy,
			httpRequest.RequestUrl,
			httpRequest.RequestBodyBytes,
			httpRequest.ByJwt,
		)
		httpRequest.RequestBodyBytes = nil

		httpResponse := newDeviceRemoteHttpResponseWithLimit(httpRequest.RequestId, bodyBytes, err, self.settings.httpMaxBodyBytes())
		bodyBytes = nil
		releaseDelivery, ok := self.acquireHttpDelivery()
		if !ok {
			return
		}
		release()
		fetchReleased = true
		defer releaseDelivery()
		// deliver directly on this per-request goroutine rather than via the
		// state-notification queue (sendLoop): http responses must not head-of-line
		// block — or be blocked by — coalescing state notifications, and they are
		// independent request/reply pairs that need no coalescing or ordering
		self.reverseCall("DeviceRemoteRpc.HttpResponse", httpResponse)
	})
	return nil
}

func (self *DeviceLocalRpc) HttpGetRaw(httpRequest *DeviceRemoteHttpRequest, _ RpcVoid) error {
	release, ok := self.tryAcquireHttp()
	if !ok {
		return fmt.Errorf("device rpc http concurrency limit reached")
	}

	go connect.HandleError(func() {
		fetchReleased := false
		defer func() {
			if !fetchReleased {
				release()
			}
		}()

		bodyBytes, err := connect.HttpGetWithStrategyRaw(
			self.ctx,
			self.deviceLocal.clientStrategy,
			httpRequest.RequestUrl,
			httpRequest.ByJwt,
		)

		httpResponse := newDeviceRemoteHttpResponseWithLimit(httpRequest.RequestId, bodyBytes, err, self.settings.httpMaxBodyBytes())
		bodyBytes = nil
		releaseDelivery, ok := self.acquireHttpDelivery()
		if !ok {
			return
		}
		release()
		fetchReleased = true
		defer releaseDelivery()
		// deliver directly on this per-request goroutine rather than via the
		// state-notification queue (sendLoop): http responses must not head-of-line
		// block — or be blocked by — coalescing state notifications, and they are
		// independent request/reply pairs that need no coalescing or ordering
		self.reverseCall("DeviceRemoteRpc.HttpResponse", httpResponse)
	})
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
	// callbacks are delivered serially by `run` to preserve event ordering
	callbacks chan func()
}

func newDeviceRemoteRpc(ctx context.Context, deviceRemote *DeviceRemote) *DeviceRemoteRpc {
	cancelCtx, cancel := context.WithCancel(ctx)

	deviceRemoteRpc := &DeviceRemoteRpc{
		ctx:          cancelCtx,
		cancel:       cancel,
		deviceRemote: deviceRemote,
		callbacks:    make(chan func(), deviceRemote.settings.CallbackBufferSize),
	}
	go connect.HandleError(deviceRemoteRpc.run)
	return deviceRemoteRpc
}

// run delivers callbacks serially in the order they were dispatched.
func (self *DeviceRemoteRpc) run() {
	defer self.cancel()
	for {
		select {
		case <-self.ctx.Done():
			return
		case callback := <-self.callbacks:
			connect.HandleError(callback)
		}
	}
}

// dispatch enqueues a callback for serial delivery by `run`.
// it blocks when the buffer is full, applying back pressure to the rpc stream.
func (self *DeviceRemoteRpc) dispatch(callback func()) {
	select {
	case <-self.ctx.Done():
	case self.callbacks <- callback:
	}
}

func (self *DeviceRemoteRpc) ProvideChanged(provideEnabled bool, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]ProvideChanged provideEnabled=%t", provideEnabled)
	self.dispatch(func() {
		self.deviceRemote.provideChanged(provideEnabled)
	})
	return nil
}

func (self *DeviceRemoteRpc) CanShowRatingDialogChanged(canShowRatingDialog bool, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]CanShowRatingDialogChanged canShowRatingDialog=%t", canShowRatingDialog)
	self.dispatch(func() {
		self.deviceRemote.canShowRatingDialogChanged(canShowRatingDialog)
	})
	return nil
}

func (self *DeviceRemoteRpc) CanPromptIntroFunnelChanged(canPromptIntroFunnel bool, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]CanPromptIntroFunnelChanged canPromptIntroFunnel=%t", canPromptIntroFunnel)
	self.dispatch(func() {
		self.deviceRemote.canPromptIntroFunnelChanged(canPromptIntroFunnel)
	})
	return nil
}

func (self *DeviceRemoteRpc) AllowForegroundChanged(allowForeground bool, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]AllowForegroundChanged allowForeground=%t", allowForeground)
	self.dispatch(func() {
		self.deviceRemote.allowForegroundChanged(allowForeground)
	})
	return nil
}

func (self *DeviceRemoteRpc) CanReferChanged(canRefer bool, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]CanReferChanged canRefer=%t", canRefer)
	self.dispatch(func() {
		self.deviceRemote.canReferChanged(canRefer)
	})
	return nil
}

func (self *DeviceRemoteRpc) ProvideModeChanged(provideMode ProvideMode, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]ProvideModeChanged provideMode=%d", provideMode)
	self.dispatch(func() {
		self.deviceRemote.provideModeChanged(provideMode)
	})
	return nil
}

func (self *DeviceRemoteRpc) ProvidePausedChanged(providePaused bool, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]ProvidePausedChanged providePaused=%t", providePaused)
	self.dispatch(func() {
		self.deviceRemote.providePausedChanged(providePaused)
	})
	return nil
}

func (self *DeviceRemoteRpc) ProvideControlModeChanged(provideControlMode ProvideControlMode, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]ProvideControlModeChanged provideControlMode=%s", provideControlMode)
	self.dispatch(func() {
		self.deviceRemote.provideControlModeChanged(provideControlMode)
	})
	return nil
}

func (self *DeviceRemoteRpc) ProvideNetworkModeChanged(provideNetworkMode ProvideNetworkMode, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]ProvideNetworkModeChanged provideNetworkMode=%s", provideNetworkMode)
	self.dispatch(func() {
		self.deviceRemote.provideNetworkModeChanged(provideNetworkMode)
	})
	return nil
}

func (self *DeviceRemoteRpc) PerformanceProfileChanged(event *DevicePerformanceProfile, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]PerformanceProfileChanged")
	self.dispatch(func() {
		self.deviceRemote.performanceProfileChanged(event.PerformanceProfile)
	})
	return nil
}

func (self *DeviceRemoteRpc) ProviderIdentitiesChanged(event *DeviceProviderIdentities, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]ProviderIdentitiesChanged")
	self.dispatch(func() {
		self.deviceRemote.providerIdentitiesChanged(event.toProviderIdentityList())
	})
	return nil
}

func (self *DeviceRemoteRpc) OfflineChanged(event *DeviceRemoteOfflineChangeEvent, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]OfflineChanged offline=%t vpnInterfaceWhileOffline=%t", event.Offline, event.VpnInterfaceWhileOffline)
	self.dispatch(func() {
		self.deviceRemote.offlineChanged(event.Offline, event.VpnInterfaceWhileOffline)
	})
	return nil
}

func (self *DeviceRemoteRpc) VpnInterfaceWhileOfflineChanged(vpnInterfaceWhileOffline bool, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]VpnInterfaceWhileOfflineChanged vpnInterfaceWhileOffline=%t", vpnInterfaceWhileOffline)
	self.dispatch(func() {
		self.deviceRemote.vpnInterfaceWhileOfflineChanged(vpnInterfaceWhileOffline)
	})
	return nil
}

func (self *DeviceRemoteRpc) ConnectChanged(connectEnabled bool, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]ConnectChanged connectEnabled=%t", connectEnabled)
	self.dispatch(func() {
		self.deviceRemote.connectChanged(connectEnabled)
	})
	return nil
}

func (self *DeviceRemoteRpc) RouteLocalChanged(routeLocal bool, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]RouteLocalChanged routeLocal=%t", routeLocal)
	self.dispatch(func() {
		self.deviceRemote.routeLocalChanged(routeLocal)
	})
	return nil
}

func (self *DeviceRemoteRpc) BlockerEnabledChanged(blockerEnabled bool, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]BlockerEnabledChanged blockerEnabled=%t", blockerEnabled)
	self.dispatch(func() {
		self.deviceRemote.blockerEnabledChanged(blockerEnabled)
	})
	return nil
}

func (self *DeviceRemoteRpc) ConnectLocationChanged(event *DeviceRemoteConnectLocationChangeEvent, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]ConnectLocationChanged")
	self.dispatch(func() {
		self.deviceRemote.connectLocationChanged(event.Location)
	})
	return nil
}

func (self *DeviceRemoteRpc) DefaultLocationChanged(event *DeviceRemoteConnectLocation, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]DefaultLocationChanged")
	self.dispatch(func() {
		self.deviceRemote.defaultLocationChanged(event.toConnectLocation())
	})
	return nil
}

func (self *DeviceRemoteRpc) ProvideSecretKeysChanged(provideSecretKeys []*ProvideSecretKey, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]ProvideSecretKeysChanged")
	self.dispatch(func() {
		self.deviceRemote.provideSecretKeysChanged(provideSecretKeys)
	})
	return nil
}

func (self *DeviceRemoteRpc) WindowMonitorEventCallback(event *DeviceRemoteWindowMonitorEvent, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]WindowMonitorEventCallback")
	self.dispatch(func() {
		self.deviceRemote.windowMonitorEvent(
			event.WindowIds,
			event.WindowExpandEvent,
			event.ProviderEvents,
			event.Reset,
		)
	})
	return nil
}

func (self *DeviceRemoteRpc) TunnelChanged(tunnelStarted bool, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]TunnelChanged")
	self.dispatch(func() {
		self.deviceRemote.tunnelChanged(tunnelStarted)
	})
	return nil
}

func (self *DeviceRemoteRpc) ContractStatusChanged(status *DeviceRemoteContractStatus, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]ContractStatusChanged")
	self.dispatch(func() {
		self.deviceRemote.contractStatusChanged(status.ContractStatus)
	})
	return nil
}

func (self *DeviceRemoteRpc) WindowStatusChanged(status *DeviceRemoteWindowStatus, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]WindowStatusChanged")
	self.dispatch(func() {
		self.deviceRemote.windowStatusChanged(status.WindowStatus)
	})
	return nil
}

func (self *DeviceRemoteRpc) BlockActionWindowChanged(event *DeviceRemoteBlockActionWindow, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]BlockActionWindowChanged")
	self.dispatch(func() {
		self.deviceRemote.blockActionWindowChanged(event.BlockActionWindow.toBlockActionWindow())
	})
	return nil
}

func (self *DeviceRemoteRpc) BlockStatsChanged(stats *DeviceRemoteBlockStats, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]BlockStatsChanged")
	self.dispatch(func() {
		self.deviceRemote.blockStatsChanged(stats.BlockStats)
	})
	return nil
}

func (self *DeviceRemoteRpc) BlockActionOverridesChanged(event *DeviceRemoteBlockActionOverrides, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]BlockActionOverridesChanged")
	self.dispatch(func() {
		self.deviceRemote.blockActionOverridesChanged(event.BlockActionOverrides)
	})
	return nil
}

func (self *DeviceRemoteRpc) PacketStatsChanged(stats *DeviceRemotePacketStats, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]PacketStatsChanged")
	self.dispatch(func() {
		self.deviceRemote.packetStatsChanged(stats.PacketStats.toPacketStats(true))
	})
	return nil
}

func (self *DeviceRemoteRpc) EgressContractStatsChanged(stats *DeviceRemoteContractStats, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]EgressContractStatsChanged")
	self.dispatch(func() {
		self.deviceRemote.egressContractStatsChanged(stats.ContractStats)
	})
	return nil
}

func (self *DeviceRemoteRpc) EgressContractDetailsChanged(event *DeviceRemoteContractDetails, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]EgressContractDetailsChanged")
	self.dispatch(func() {
		self.deviceRemote.egressContractDetailsChanged(event.ContractDetails.toContractDetails())
	})
	return nil
}

func (self *DeviceRemoteRpc) IngressContractStatsChanged(stats *DeviceRemoteContractStats, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]IngressContractStatsChanged")
	self.dispatch(func() {
		self.deviceRemote.ingressContractStatsChanged(stats.ContractStats)
	})
	return nil
}

func (self *DeviceRemoteRpc) IngressContractDetailsChanged(event *DeviceRemoteContractDetails, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]IngressContractDetailsChanged")
	self.dispatch(func() {
		self.deviceRemote.ingressContractDetailsChanged(event.ContractDetails.toContractDetails())
	})
	return nil
}

func (self *DeviceRemoteRpc) ProviderPacketStatsChanged(stats *DeviceRemotePacketStats, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]ProviderPacketStatsChanged")
	self.dispatch(func() {
		self.deviceRemote.providerPacketStatsChanged(stats.PacketStats.toPacketStats(true))
	})
	return nil
}

func (self *DeviceRemoteRpc) ProviderEgressContractStatsChanged(stats *DeviceRemoteContractStats, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]ProviderEgressContractStatsChanged")
	self.dispatch(func() {
		self.deviceRemote.providerEgressContractStatsChanged(stats.ContractStats)
	})
	return nil
}

func (self *DeviceRemoteRpc) ProviderEgressContractDetailsChanged(event *DeviceRemoteContractDetails, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]ProviderEgressContractDetailsChanged")
	self.dispatch(func() {
		self.deviceRemote.providerEgressContractDetailsChanged(event.ContractDetails.toContractDetails())
	})
	return nil
}

func (self *DeviceRemoteRpc) ProviderIngressContractStatsChanged(stats *DeviceRemoteContractStats, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]ProviderIngressContractStatsChanged")
	self.dispatch(func() {
		self.deviceRemote.providerIngressContractStatsChanged(stats.ContractStats)
	})
	return nil
}

func (self *DeviceRemoteRpc) ProviderIngressContractDetailsChanged(event *DeviceRemoteContractDetails, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]ProviderIngressContractDetailsChanged")
	self.dispatch(func() {
		self.deviceRemote.providerIngressContractDetailsChanged(event.ContractDetails.toContractDetails())
	})
	return nil
}

func (self *DeviceRemoteRpc) TransportSettingsChanged(event *DeviceRemoteTransportSettingsRpc, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]TransportSettingsChanged")
	self.dispatch(func() {
		self.deviceRemote.transportSettingsChanged(event.TransportSettings, event.TransportStatus)
	})
	return nil
}

func (self *DeviceRemoteRpc) ProviderTransportSettingsChanged(event *DeviceRemoteTransportSettingsRpc, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]ProviderTransportSettingsChanged")
	self.dispatch(func() {
		self.deviceRemote.providerTransportSettingsChanged(event.TransportSettings, event.TransportStatus)
	})
	return nil
}

func (self *DeviceRemoteRpc) DnsResolverSettingsChanged(event *DeviceRemoteDnsResolverSettings, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]DnsResolverSettingsChanged")
	self.dispatch(func() {
		self.deviceRemote.dnsResolverSettingsChanged(event.DnsResolverSettings)
	})
	return nil
}

func (self *DeviceRemoteRpc) NetworkPeersChanged(event *DeviceRemoteNetworkPeers, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]NetworkPeersChanged")
	self.dispatch(func() {
		self.deviceRemote.networkPeersChanged(event.NetworkPeers.toNetworkPeers())
	})
	return nil
}

func (self *DeviceRemoteRpc) HttpResponse(httpResponse *DeviceRemoteHttpResponse, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]HttpResponse")
	self.dispatch(func() {
		self.deviceRemote.httpResponse(httpResponse)
	})
	return nil
}

func (self *DeviceRemoteRpc) Close() {
	self.cancel()
}
