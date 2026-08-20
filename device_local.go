package sdk

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
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
	// createTime approximates connected-since for the fixed destination: the
	// monitor is created when the fixed client connects (see
	// cachedWindowMonitor, one instance per remoteUserNatClient value)
	createTime time.Time
}

func newFixedWindowMonitor(clientIds []connect.Id) *fixedWindowMonitor {
	return &fixedWindowMonitor{
		clientIds:  clientIds,
		createTime: time.Now(),
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
			EventTime: self.createTime,
			ClientId:  clientId,
			State:     connect.ProviderStateAdded,
			// each fixed destination id is its own provider dot; there is no
			// separate window client id. Location stays nil (no discovery).
			EgressClientId: clientId,
		}
	}
	return windowExpandEvent, providerEvents
}

// defaultDeviceLocalMemoryTargetByteCount is the default per-device memory
// target (see DeviceLocalSettings.MemoryTargetByteCount). Hosts pass an
// explicit value where the device is created
// (NewDeviceLocalWithMemoryTarget); this default keeps a plain construction
// bounded.
const defaultDeviceLocalMemoryTargetByteCount = 20 * 1024 * 1024

// device memory target split, in parts of `deviceMemoryRatioParts`:
// dns 2 : client 14 : provider 4. The client share carries the in-flight
// (bandwidth-delay) window, so it takes the largest share — at the 20 MB
// reference target the provide-on client pair lands on the historically
// proven 6 MiB send / 8 MiB receive; dns needs only enough to keep parallel
// resolution above its demand caps. The provider share follows the provide
// state: while providing is off it backs the client pair instead of idling
// (see applyProvideMemorySharesWithLock), and when the device can never
// provide it folds statically (see deviceMemoryShares).
const (
	deviceMemoryRatioDns      = 2
	deviceMemoryRatioClient   = 14
	deviceMemoryRatioProvider = 4
	deviceMemoryRatioParts    = 20
)

// byteCountFraction returns floor(byteCount*numerator/denominator) without
// multiplying the full byte count first. Memory targets enter through a
// host-facing int64 API; multiplication-before-division can wrap even though
// every resulting share is no larger than the original target.
//
// All callers use 0 <= numerator <= denominator and small constant ratios.
func byteCountFraction(
	byteCount ByteCount,
	numerator ByteCount,
	denominator ByteCount,
) ByteCount {
	if byteCount <= 0 || numerator <= 0 {
		return 0
	}
	if denominator <= 0 || denominator < numerator {
		panic("invalid byte-count fraction")
	}
	return byteCount/denominator*numerator +
		byteCount%denominator*numerator/denominator
}

// deviceMemoryShares splits the device memory target into the dns, client,
// and provider shares, folding the provider share into the client share
// when the device cannot provide. A zero target returns zero shares (legacy
// process-budget scaling everywhere).
func deviceMemoryShares(settings *DeviceLocalSettings) (dnsByteCount ByteCount, clientByteCount ByteCount, providerByteCount ByteCount) {
	target := settings.MemoryTargetByteCount
	if target <= 0 {
		return 0, 0, 0
	}
	dnsByteCount = byteCountFraction(target, deviceMemoryRatioDns, deviceMemoryRatioParts)
	clientByteCount = byteCountFraction(target, deviceMemoryRatioClient, deviceMemoryRatioParts)
	providerByteCount = byteCountFraction(target, deviceMemoryRatioProvider, deviceMemoryRatioParts)
	if settings.HostedIncompatible || !settings.AllowProvider {
		clientByteCount += providerByteCount
		providerByteCount = 0
	}
	return
}

// deviceLocalSequenceBufferSize derives the sequence channel depth from the
// client share of the device memory target (one slot per 16 KiB — the depth
// pins in-flight pool buffers under sustained backpressure, outside the byte
// budgets), within the historical working range. A zero share keeps the
// process-budget scaled default.
func deviceLocalSequenceBufferSize(clientShareByteCount ByteCount) int {
	if clientShareByteCount <= 0 {
		return connect.MemoryScaledCount(256, 32)
	}
	// Clamp while the value is still int64. Converting an untrusted
	// host-supplied target before min() can wrap on 32-bit app targets.
	return max(32, int(min(ByteCount(256), clientShareByteCount/(16*1024))))
}

// deviceLocalTransferBudgets creates the device's shared transfer queue
// budget pair from the client share of the device memory target (3:4
// send:receive with working floors), or the legacy process-budget scaled
// sizes for a zero share. Per-sequence queues borrow above their floor
// from these pools, so the aggregate queue memory stays flat as the window
// grows.
func deviceLocalTransferBudgets(clientShareByteCount ByteCount) (resendQueueBudget *connect.TransferMemoryBudget, receiveQueueBudget *connect.TransferMemoryBudget) {
	resendQueueByteCount := connect.MemoryScaledByteCount(6*1024*1024, 1024*1024)
	receiveQueueByteCount := connect.MemoryScaledByteCount(8*1024*1024, 1536*1024)
	if 0 < clientShareByteCount {
		resendQueueByteCount = max(byteCountFraction(clientShareByteCount, 3, 7), 1024*1024)
		receiveQueueByteCount = max(byteCountFraction(clientShareByteCount, 4, 7), 1536*1024)
	}
	return connect.NewTransferMemoryBudget(resendQueueByteCount),
		connect.NewTransferMemoryBudget(receiveQueueByteCount)
}

// deviceLocalP2pReceiveBufferByteCount is the per-peer-connection SCTP receive
// buffer (and therefore the per-connection budget reservation) for the
// many-peer public/automatic mobile windows. It deliberately trades
// single-connection throughput for a quarter of the historical 512 KiB
// footprint so a modest dedicated budget admits several concurrent peer
// connections. Explicit fixed network-peer destinations use the larger
// measured window below. The 512 KiB default previously starved p2p when it
// was admitted against the shared transfer queue. See PACKETRESEARCH1 §17.
const deviceLocalP2pReceiveBufferByteCount = ByteCount(128 * 1024)
const deviceLocalP2pMinPeerConnectionCount = 8

// A user-selected network destination is fixed to one window client:
// ApiMultiClientGenerator.FixedDestinationSize returns one for its single
// ClientId spec, so the normal 6-quality + 2-speed auto-window maxima do not
// apply. Pion advertises this value as SCTP a_rwnd. Three controlled 50 ms runs
// put a 512 KiB window at 9.28-9.53 MiB/s, comfortably above the measured
// physical peer path (4.8-5.9 MiB/s), while using one quarter of the former
// 2 MiB per association.
//
// Reserve two associations so resident migration/reform can connect a
// replacement before closing the old stream. This gives the selected-peer
// WebRTC manager a predictable 1 MiB ceiling; browser DNS and TCP flows
// multiplex over that one client/association rather than consuming one P2P
// connection each.
const deviceLocalNetworkPeerP2pReceiveBufferByteCount = ByteCount(512 * 1024)
const deviceLocalNetworkPeerP2pConnectionCount = 2

func deviceLocalDestinationWebRtcSettings(
	settings *connect.WebRtcSettings,
	networkPeer bool,
) (receiveBufferByteCount ByteCount, memoryBudget *connect.TransferMemoryBudget) {
	receiveBufferByteCount = settings.ReceiveBufferSize
	memoryBudget = settings.MemoryBudget
	if !networkPeer {
		return
	}
	receiveBufferByteCount = max(
		receiveBufferByteCount,
		deviceLocalNetworkPeerP2pReceiveBufferByteCount,
	)
	memoryBudget = connect.NewTransferMemoryBudget(
		deviceLocalNetworkPeerP2pConnectionCount * receiveBufferByteCount,
	)
	return
}

// applyDeviceLocalDestinationWebRtcSettings configures one window client. A
// selected peer uses one destination-local pool for both the fallback and
// trusted-Network admission views: the peer is proactively marked before its
// first P2P offer, while sharing the pool keeps the hard ceiling at two 512 KiB
// associations instead of silently summing two pools.
func applyDeviceLocalDestinationWebRtcSettings(
	settings *connect.WebRtcSettings,
	networkPeer bool,
	networkPeerId *connect.Id,
	receiveBufferByteCount ByteCount,
	memoryBudget *connect.TransferMemoryBudget,
) {
	settings.ReceiveBufferSize = receiveBufferByteCount
	settings.MemoryBudget = memoryBudget
	if networkPeer {
		settings.NetworkPeerReceiveBufferSize = receiveBufferByteCount
		settings.NetworkPeerMemoryBudget = memoryBudget
		if networkPeerId != nil {
			settings.InitialNetworkPeerIds = []connect.Id{*networkPeerId}
		}
	}
}

// deviceLocalWebRtcBudget creates the DEDICATED p2p peer-connection admission
// budget for a memory share. p2p must not share the transfer receive-queue
// budget: an active download legitimately consumes the receive queue, so a
// shared budget refused every peer-connection setup exactly while traffic
// flowed — the moment p2p is needed — pinning the flow on the WAN relay
// forever (PACKETRESEARCH1 §17). A dedicated budget breaks that catch-22. It
// gates admission only; the SCTP buffer memory a formed connection uses is the
// same either way, so this adds no steady-state footprint beyond connections
// that actually establish. The original four-buffer floor was below the normal
// quality+speed window demand and was observed saturated indefinitely on both
// physical Android peers (thousands of refused retries). Eight buffers keep the
// provider-side floor aligned with connect's mobile peer-count floor; larger
// client shares still scale above it. A zero share keeps p2p unbudgeted
// (desktop/server).
func deviceLocalWebRtcBudget(shareByteCount ByteCount) *connect.TransferMemoryBudget {
	if shareByteCount <= 0 {
		return nil
	}
	return connect.NewTransferMemoryBudget(max(
		shareByteCount/8,
		deviceLocalP2pMinPeerConnectionCount*deviceLocalP2pReceiveBufferByteCount,
	))
}

func DefaultDeviceLocalSettings() *DeviceLocalSettings {
	memoryTargetByteCount := ByteCount(defaultDeviceLocalMemoryTargetByteCount)
	// provisional sizing from the unfolded client share; device construction
	// re-derives from the final settings (target overrides, provider fold)
	clientShareByteCount := memoryTargetByteCount * deviceMemoryRatioClient / deviceMemoryRatioParts
	bufferSize := deviceLocalSequenceBufferSize(clientShareByteCount)
	clientSettings := connect.DefaultClientSettingsWithBufferSize(bufferSize)
	// A VPN device needs only the current physical/default-route addresses.
	// Enumerating every bridge, stale tunnel, AWDL, and VM interface makes
	// ICE check a local×remote candidate cross-product for every window
	// client, producing avoidable CPU bursts and setup pauses on macOS.
	clientSettings.WebRtcSettings.UseEgressOnlyIceInterfaces = true
	// one transfer queue budget pair shared across all of the device's
	// clients (the provider client plus every window client): the provider
	// client replaces it with its own pair from the provider share; the
	// window client generator stamps the same pointers. these are resized
	// from the final settings value at device construction (see
	// newDeviceLocalWithOverrides), so a caller override of
	// MemoryTargetByteCount after this constructor takes effect.
	resendQueueBudget, receiveQueueBudget := deviceLocalTransferBudgets(clientShareByteCount)
	clientSettings.SendBufferSettings.ResendQueueBudget = resendQueueBudget
	clientSettings.ReceiveBufferSettings.ReceiveQueueBudget = receiveQueueBudget
	// p2p peer connections admit against a DEDICATED budget (see
	// deviceLocalWebRtcBudget) with a phone-sized SCTP buffer, not the shared
	// receive queue that active transfer starves. Only on a memory-targeted
	// device (mobile); a zero share (desktop/server) keeps the connect
	// defaults (512 KiB buffer, unbudgeted).
	if 0 < clientShareByteCount {
		clientSettings.WebRtcSettings.ReceiveBufferSize = deviceLocalP2pReceiveBufferByteCount
		clientSettings.WebRtcSettings.MemoryBudget = deviceLocalWebRtcBudget(clientShareByteCount)
	}
	return &DeviceLocalSettings{
		MemoryTargetByteCount: memoryTargetByteCount,
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
		// Security-policy monitoring clones diagnostic maps and, for a
		// DeviceRemote, performs synchronous RPC. Keep it opt-in so an app
		// object never owns background polling.
		Verbose: false,

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

// ---- millisecond accessors for the time.Duration tunables ----------------
//
// Go always uses time.Duration; these exist purely as the Sdk translation,
// because gomobile cannot bind time.Duration and the fields were therefore
// silently unsettable from android/apple on a type apps do construct. The
// bound name drops the redundant "Duration"/reads as "<thing>Millis", matching
// BusyProbeBudgetMillis and friends elsewhere in the sdk.
//
// Millisecond granularity is not a narrowing in practice: every default here
// is whole seconds (5s, 10s, 300s, 1s, 1s). A sub-millisecond value set from
// Go still round-trips through the Go field untouched — only the bound view
// truncates.

func (self *DeviceLocalSettings) GetSendTimeoutMillis() int64 {
	return self.SendTimeout.Milliseconds()
}

func (self *DeviceLocalSettings) SetSendTimeoutMillis(millis int64) {
	self.SendTimeout = time.Duration(millis) * time.Millisecond
}

func (self *DeviceLocalSettings) GetNetContractStatusMillis() int64 {
	return self.NetContractStatusDuration.Milliseconds()
}

func (self *DeviceLocalSettings) SetNetContractStatusMillis(millis int64) {
	self.NetContractStatusDuration = time.Duration(millis) * time.Millisecond
}

func (self *DeviceLocalSettings) GetBlockActionWindowMillis() int64 {
	return self.BlockActionWindowDuration.Milliseconds()
}

func (self *DeviceLocalSettings) SetBlockActionWindowMillis(millis int64) {
	self.BlockActionWindowDuration = time.Duration(millis) * time.Millisecond
}

func (self *DeviceLocalSettings) GetContractStatsEpochMillis() int64 {
	return self.ContractStatsEpoch.Milliseconds()
}

func (self *DeviceLocalSettings) SetContractStatsEpochMillis(millis int64) {
	self.ContractStatsEpoch = time.Duration(millis) * time.Millisecond
}

func (self *DeviceLocalSettings) GetNetworkPeersEpochMillis() int64 {
	return self.NetworkPeersEpoch.Milliseconds()
}

func (self *DeviceLocalSettings) SetNetworkPeersEpochMillis(millis int64) {
	self.NetworkPeersEpoch = time.Duration(millis) * time.Millisecond
}

// DeviceLocalSettings is BOUND and app-facing, despite what a
// `//gomobile:noexport` on this type used to claim: gobind emits the class
// with ~50 working members, and Sdk exposes both defaultDeviceLocalSettings()
// and newDeviceLocal(..., settings). Apps construct and mutate it.
//
// What gobind does drop is a handful of individual fields — the embedded
// connect.ClientSettings, GeneratorFunc (a func value),
// MultiClientIdentityStore (a foreign interface) and the time.Duration
// tunables. Those carry their own field-level markers below. The durations
// are reachable through the *Millis accessor pairs at the end of this file,
// so an app can set them; the other three are Go-construction only.
type DeviceLocalSettings struct {
	// MemoryTargetByteCount is this device's memory target, split by ratio
	// (dns 2 : client 14 : provider 4, see deviceMemoryShares) among dns
	// resolution (a live byte budget on the device's resolvers), the client
	// transfer buffers (the shared queue budget pair + p2p peer connection
	// admission), and the provider path (the provider client's budget pair +
	// the egress nat flow caps). When the device cannot provide, the
	// provider share folds into the client share. Per device, so a
	// multi-device process (the cloud proxy) gives each instance independent
	// admission and sizing state; the message pools are the process-global complement
	// (SetMemoryLimit / SetMessagePoolMemoryTargets). 0 disables the
	// per-device target (legacy process-budget scaling). Hosts set this
	// explicitly where the device is created; the default keeps a plain
	// construction bounded.
	MemoryTargetByteCount ByteCount

	// time to give up (drop) sending a packet to a destination
	//
	//gomobile:noexport time.Duration — bound as GetSendTimeoutMillis/SetSendTimeoutMillis
	SendTimeout time.Duration
	// ClientDrainTimeout time.Duration
	SequenceBufferSize int

	//gomobile:noexport time.Duration — bound as GetNetContractStatusMillis/SetNetContractStatusMillis
	NetContractStatusDuration time.Duration
	NetContractStatusCount    int

	// the time window and max count of retained block actions
	//
	//gomobile:noexport time.Duration — bound as GetBlockActionWindowMillis/SetBlockActionWindowMillis
	BlockActionWindowDuration time.Duration
	BlockActionWindowMaxCount int

	// the contract stats/details listeners emit at most once per epoch across
	// all window clients (a close event always emits)
	//
	//gomobile:noexport time.Duration — bound as GetContractStatsEpochMillis/SetContractStatsEpochMillis
	ContractStatsEpoch time.Duration

	// the network peers change listeners emit at most once per epoch
	//
	//gomobile:noexport time.Duration — bound as GetNetworkPeersEpochMillis/SetNetworkPeersEpochMillis
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
	// Verbose opts into periodic, summarized security-policy diagnostics. It
	// is disabled by default because a DeviceRemote poll performs RPC and app
	// foreground/background polling belongs to view controllers.
	Verbose bool
	// GeneratorFunc, when set, builds the multi client generator instead of
	// the default api generator
	//
	//gomobile:noexport func value — gomobile cannot bind funcs (only interfaces).
	// Go/headless hosts only; apps get the default api generator.
	GeneratorFunc func(specs []*connect.ProviderSpec) connect.MultiClientGenerator
	// MultiClientIdentityStore, when set, persists the api generator's
	// window client identities so a process restart reuses them against the
	// same destinations — keeping provider-side NAT flows resumable
	// (PROXYDRAIN1.md §3.5). Only applies to the default api generator.
	//
	//gomobile:noexport connect.MultiClientIdentityStore is an interface from
	// another package, which gomobile does not bind. Go/headless hosts only.
	MultiClientIdentityStore connect.MultiClientIdentityStore
	// ProviderDialContextSettings, when set, is applied only to the exit NAT's
	// TCP and UDP sockets. Headless integration harnesses use it to bind each
	// provider to a distinct loopback source address while exercising the real
	// tunnel stack on one host. Ordinary applications leave it nil.
	//
	//gomobile:noexport Go-only network dial seam.
	ProviderDialContextSettings *connect.DialContextSettings
	// FIXME remove EnableRpc. Turn on RPC when RPC connections are set (receive net.Conn, send net.Conn)
	EnableRpc bool
	// KeyMaterial, when set, is applied to `ClientSettings` at construction
	KeyMaterial *DeviceLocalKeyMaterial
	// DisableLogging silences the device and all nested components and
	// clients, for hosts embedding many devices in one process.
	// It overrides `ClientSettings.Log`.
	DisableLogging bool

	// HostedIncompatible, when true, hard-guards the setters that must never
	// change on a hosted (platform-embedded) device: route local, provide and
	// transport settings, plus the identity/rpc setters that only make sense
	// for a locally-owned device. The guarded setters become no-ops; their
	// getters and change listeners keep working. Hosted transport is pinned to
	// H1. This is defense in depth alongside
	// `DeviceLocalRpc.DisableHostedIncompatible`, which stops the
	// same operations at the rpc layer — either alone is sufficient, both
	// together mean nothing reachable can flip these on a hosted device.
	HostedIncompatible bool

	// UseExperimentalTunnelAddress, when set, assigns the TUN interface a random
	// 10.x.y.h (RFC1918, DHCP-shaped) address instead of drawing from connect's
	// 169.254/16 pool. 10.x is private, so the browser's mDNS obfuscation masks it
	// in WebRTC peer discovery, and randomizing avoids a fixed signature.
	// EXPERIMENT: defaults true for now (testing).
	UseExperimentalTunnelAddress bool

	//gomobile:noexport connect.ClientSettings is a struct from another package,
	// which gomobile does not bind — the whole embedded block (and every
	// field promoted from it) is absent on android/apple. Apps configure the
	// client through the constructors and the setters on DeviceLocal instead.
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

	byJwt            string
	apiJwtRefreshSub Sub
	apiAuthLogoutSub Sub
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

	// tunnelDnsSetting is the DNS config the platform applies to the TUN. It
	// defaults to the URnetwork-owned plain-DNS identity: UpgradeMux claims :53
	// before that address is reached, and Android does not recognize it as a
	// public resolver to opportunistically encrypt around the mux.
	tunnelDnsSetting *TunnelDnsSetting

	clientStrategy *connect.ClientStrategy

	generatorFunc           func(specs []*connect.ProviderSpec) connect.MultiClientGenerator
	apiMultiClientGenerator *connect.ApiMultiClientGenerator
	provider                *deviceLocalProvider

	stats *DeviceStats

	deviceLocalRpcManager *deviceLocalRpcManager
	// current listener config, so SetRpcServer is a no-op (no rebind that would
	// drop live connections) when the same server is re-applied
	rpcHostPort      string
	rpcServerPem     string
	rpcClientCertPem string

	stateLock sync.Mutex
	closeOnce sync.Once
	// stateLockGoid atomic.Int64

	connectLocation *ConnectLocation // reconnects when launched
	defaultLocation *ConnectLocation // persisting the location after the client has disconnected
	// SetDestination is also used by app/extension state synchronization.
	// Keep the installed transport identity so an equivalent sync does not
	// close and recreate the entire mux + multi-client provider window.
	destinationInitialized      bool
	destinationSpecsFingerprint string

	performanceProfile *PerformanceProfile

	// when nil, packets get routed to the local user nat
	remoteUserNatClient connect.UserNatClient
	contractStatusSub   func()
	windowMonitorSub    func()

	// a stable windowMonitor instance per remoteUserNatClient for the
	// fixed/empty monitor types (see cachedWindowMonitor). Guarded by its own
	// leaf lock so windowMonitor() adds no lock-order edges.
	windowMonitorCacheLock   sync.Mutex
	windowMonitorCacheClient connect.UserNatClient
	windowMonitorCache       windowMonitor

	// upgradeMux interposes on `remoteUserNatClient` (the exit/egress path) to
	// intercept and upgrade plaintext DNS (UDP/53) and HTTP (TCP/80). It is created and
	// torn down with `remoteUserNatClient`. When set, the send path runs through it (it
	// claims DNS/HTTP, else forwards to `remoteUserNatClient`) and the multi-client's
	// receive callback is the mux's `Receive`. nil => no interposition.
	upgradeMux         *connect.UpgradeMux
	upgradeMuxSettings *connect.UpgradeMuxSettings

	// dnsMemoryTarget is the dns share of the device memory target: one live
	// byte budget shared by the device's resolver caches across mux rebuilds
	// (in-flight accounting carries over). stamped into the mux settings at
	// mux construction.
	dnsMemoryTarget *connect.MemoryTarget

	// dohServerScoresSeed is the per-DoH-server success ordering carried into
	// each mux build: loaded from local storage at construction (the last
	// session's experience) and refreshed from the live mux at teardown —
	// which also persists it for the next session. Guarded by stateLock.
	dohServerScoresSeed map[string]float64

	// performanceDegraded is the host's degraded-performance state (low power
	// mode, thermal throttling, constrained network), reported by the apps
	// via SetPerformanceDegraded; carried into each window build and
	// forwarded live so the liveness probe timings ease on a slow device.
	performanceDegraded atomic.Bool

	// windowIdentityStore, when the device owns its storage (not hosted, not
	// host-provided), persists the window client identities so a relaunch
	// that reconnects to the same destination reuses them (see
	// window_identity_store.go). The device stamps the connect-spec
	// fingerprint before each generator build. nil when unavailable.
	windowIdentityStore *localStateWindowIdentityStore

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
	provideControlMode       ProvideControlMode // auto, always, network, never
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
	// the platform flow-owner resolver for per-app pinning; re-applied to
	// every multi client the device builds
	flowOwnerLookup FlowOwnerLookup
	// Client and provider carrier policies are restored before either side
	// constructs a platform transport. They survive destination rebuilds and
	// are cloned at every public boundary.
	transportSettings         *TransportSettings
	providerTransportSettings *TransportSettings
	// the runtime reliability override, nil when none is set; re-applied to
	// every multi client the device builds. The override lives on the multi
	// client, which is rebuilt on every connect, so without this copy a
	// developer-menu experiment set before connecting -- or simply surviving
	// a reconnect -- would evaporate silently
	reliabilitySettings *connect.ReliabilitySettings
	// routingTier is the persisted RoutingTier dial (off/light/full, see
	// routing_tier.go), stored as a bare int for the same reason
	// SetRoutingTier takes one. Zero value is RoutingTierOff, so a device that
	// never restores a persisted tier (fresh install) gets legacy behavior.
	// Baked into settings at every window build (see SetDestination) and
	// pushed live via SetReliabilitySettings on SetRoutingTier
	routingTier int
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
	peerIdentitySub       func()

	providerPacketStatsSub        func()
	providerContractStatsEventSub func()

	receiveCallbacks            *connect.CallbackList[connect.ReceivePacketFunction]
	receivePacketsCallbacks     *connect.CallbackList[connect.ReceivePacketsFunction]
	receivePacketBatchCallbacks *connect.CallbackList[receivePacketBatchFunction]

	// probeSuiteState owns the in-app test suite. It pumps its own userspace
	// tun through this device, so probes take the same exits as real traffic
	// -- see probe_suite.go for why an ordinary http client cannot.
	probeSuiteState *probeSuite

	canShowRatingDialogChangeListeners       *connect.CallbackList[CanShowRatingDialogChangeListener]
	canPromptIntroFunnelChangeListeners      *connect.CallbackList[CanPromptIntroFunnelChangeListener]
	allowForegroundChangeListeners           *connect.CallbackList[AllowForegroundChangeListener]
	canReferChangeListeners                  *connect.CallbackList[CanReferChangeListener]
	provideModeChangeListeners               *connect.CallbackList[ProvideModeChangeListener]
	provideChangeListeners                   *connect.CallbackList[ProvideChangeListener]
	provideControlModeChangeListeners        *connect.CallbackList[ProvideControlModeChangeListener]
	performanceProfileChangeListeners        *connect.CallbackList[PerformanceProfileChangeListener]
	providerIdentityChangeListeners          *connect.CallbackList[ProviderIdentityChangeListener]
	connectedProviderLocationChangeListeners *connect.CallbackList[ConnectedProviderLocationChangeListener]
	providePausedChangeListeners             *connect.CallbackList[ProvidePausedChangeListener]
	provideNetworkModeChangeListeners        *connect.CallbackList[ProvideNetworkModeChangeListener]
	offlineChangeListeners                   *connect.CallbackList[OfflineChangeListener]
	vpnInterfaceWhileOfflineChangeListeners  *connect.CallbackList[VpnInterfaceWhileOfflineChangeListener]
	connectChangeListeners                   *connect.CallbackList[ConnectChangeListener]
	routeLocalChangeListeners                *connect.CallbackList[RouteLocalChangeListener]
	blockerEnabledChangeListeners            *connect.CallbackList[BlockerEnabledChangeListener]
	connectLocationChangeListeners           *connect.CallbackList[ConnectLocationChangeListener]
	defaultLocationChangeListeners           *connect.CallbackList[DefaultLocationChangeListener]
	provideSecretKeysListeners               *connect.CallbackList[ProvideSecretKeysListener]
	tunnelChangeListeners                    *connect.CallbackList[TunnelChangeListener]
	contractStatusChangeListeners            *connect.CallbackList[ContractStatusChangeListener]
	windowStatusChangeListeners              *connect.CallbackList[WindowStatusChangeListener]
	jwtRefreshListeners                      *connect.CallbackList[JwtRefreshListener]
	authLogoutListeners                      *connect.CallbackList[AuthLogoutListener]

	blockActionWindowChangeListeners         *connect.CallbackList[BlockActionWindowChangeListener]
	blockStatsChangeListeners                *connect.CallbackList[BlockStatsChangeListener]
	blockActionOverridesChangeListeners      *connect.CallbackList[BlockActionOverridesChangeListener]
	transportSettingsChangeListeners         *connect.CallbackList[TransportSettingsChangeListener]
	providerTransportSettingsChangeListeners *connect.CallbackList[ProviderTransportSettingsChangeListener]
	transportStatusChangeListeners           *connect.CallbackList[TransportStatusChangeListener]
	providerTransportStatusChangeListeners   *connect.CallbackList[ProviderTransportStatusChangeListener]
	packetStatsChangeListeners               *connect.CallbackList[PacketStatsChangeListener]
	egressContractStatsChangeListeners       *connect.CallbackList[ContractStatsChangeListener]
	egressContractDetailsChangeListeners     *connect.CallbackList[ContractDetailsChangeListener]
	ingressContractStatsChangeListeners      *connect.CallbackList[ContractStatsChangeListener]
	ingressContractDetailsChangeListeners    *connect.CallbackList[ContractDetailsChangeListener]
	dnsResolverSettingsChangeListeners       *connect.CallbackList[DnsResolverSettingsChangeListener]
	networkPeersChangeListeners              *connect.CallbackList[NetworkPeersChangeListener]

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

// NewDeviceLocalWithMemoryTarget creates a device with an explicit
// per-device memory target (see DeviceLocalSettings.MemoryTargetByteCount:
// split dns 2 : client 14 : provider 4 by ratio, with the provider share
// folded into the client share when the device cannot provide). This is the
// host-facing constructor for sizing a device's memory where it is created;
// keyMaterial may be nil.
func NewDeviceLocalWithMemoryTarget(
	networkSpace *NetworkSpace,
	byJwt string,
	deviceDescription string,
	deviceSpec string,
	appVersion string,
	instanceId *Id,
	enableRpc bool,
	keyMaterial *DeviceLocalKeyMaterial,
	memoryTargetByteCount int64,
) (*DeviceLocal, error) {
	settings := DefaultDeviceLocalSettings()
	settings.EnableRpc = enableRpc
	settings.KeyMaterial = keyMaterial
	settings.MemoryTargetByteCount = memoryTargetByteCount
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

//gomobile:noexport
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

//gomobile:noexport
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
//
//gomobile:noexport
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

	transportSettings := DefaultTransportSettings()
	providerTransportSettings := DefaultProviderTransportSettings()
	var localState *LocalState
	if asyncLocalState := networkSpace.GetAsyncLocalState(); asyncLocalState != nil {
		localState = asyncLocalState.GetLocalState()
		if !settings.HostedIncompatible {
			if persisted := localState.GetTransportSettings(); persisted != nil {
				transportSettings = normalizeTransportSettings(persisted, false)
			}
			if persisted := localState.GetProviderTransportSettings(); persisted != nil {
				providerTransportSettings = normalizeTransportSettings(persisted, true)
			}
		}
	}
	if settings.HostedIncompatible {
		transportSettings = hostedTransportSettings()
		providerTransportSettings = hostedTransportSettings()
	}

	// (re)size the device's shared transfer budget pair and p2p admission
	// from the final per-device memory shares: the settings constructor
	// sized them from its default, and the caller may have overridden
	// MemoryTargetByteCount (or disabled providing, folding the provider
	// share into the client share) since
	dnsShareByteCount, clientShareByteCount, providerShareByteCount := deviceMemoryShares(settings)
	resendQueueBudget, receiveQueueBudget := deviceLocalTransferBudgets(clientShareByteCount)
	settings.ClientSettings.SendBufferSettings.ResendQueueBudget = resendQueueBudget
	settings.ClientSettings.ReceiveBufferSettings.ReceiveQueueBudget = receiveQueueBudget
	// dedicated p2p admission budget + phone-sized SCTP buffer (see
	// deviceLocalWebRtcBudget / PACKETRESEARCH1 §17). Only on a
	// memory-targeted device; a zero share keeps the connect defaults.
	if 0 < clientShareByteCount {
		settings.ClientSettings.WebRtcSettings.ReceiveBufferSize = deviceLocalP2pReceiveBufferByteCount
		settings.ClientSettings.WebRtcSettings.MemoryBudget = deviceLocalWebRtcBudget(clientShareByteCount)
	}

	var provider *deviceLocalProvider
	if settings.AllowProvider {
		providerTransportMode, providerModePreferences := toConnectTransportPolicy(providerTransportSettings, true)
		provider = newDeviceLocalProviderWithOverrides(
			ctx,
			networkSpace,
			byJwt,
			appVersion,
			instanceId.toConnectId(),
			&settings.ClientSettings,
			clientId,
			// the provider share of the device memory target
			providerShareByteCount,
			providerTransportMode,
			providerModePreferences,
		)
	}

	// api := newBringYourApiWithContext(cancelCtx, clientStrategy, apiUrl)
	api := networkSpace.GetApi()

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
		// the dns share of the device memory target; one live budget for the
		// life of the device (see the field doc)
		dnsMemoryTarget:           connect.NewMemoryTarget(dnsShareByteCount),
		generatorFunc:             settings.GeneratorFunc,
		provider:                  provider,
		transportSettings:         cloneTransportSettings(transportSettings),
		providerTransportSettings: cloneTransportSettings(providerTransportSettings),
		// contractManager: contractManager,
		// routeManager: routeManager,
		stats:                                    newDeviceStats(),
		connectLocation:                          nil,
		defaultLocation:                          nil,
		remoteUserNatClient:                      nil,
		upgradeMux:                               nil,
		upgradeMuxSettings:                       connect.DefaultUpgradeMuxSettings(),
		remoteUserNatProviderLocalUserNat:        nil,
		remoteUserNatProvider:                    nil,
		blocker:                                  blocker,
		routeLocal:                               defaultRouteLocal,
		canShowRatingDialog:                      settings.DefaultCanShowRatingDialog,
		canPromptIntroFunnel:                     settings.DefaultCanShowIntroFunnel,
		canRefer:                                 settings.DefaultCanRefer,
		allowForeground:                          settings.DefaultAllowForeground,
		provideMode:                              ProvideModeNone,
		provideControlMode:                       defaultProvideControlMode,
		provideNetworkMode:                       settings.DefaultProvideNetworkMode,
		offline:                                  settings.DefaultOffline,
		vpnInterfaceWhileOffline:                 settings.DefaultVpnInterfaceWhileOffline,
		tunnelStarted:                            settings.DefaultTunnelStarted,
		orderedContractStatusUpdates:             []*contractStatusUpdate{},
		netContractStatus:                        &ContractStatus{},
		contracts:                                newDeviceContractTracker(),
		providerContracts:                        newDeviceContractTracker(),
		receiveCallbacks:                         connect.NewCallbackList[connect.ReceivePacketFunction](),
		receivePacketsCallbacks:                  connect.NewCallbackList[connect.ReceivePacketsFunction](),
		receivePacketBatchCallbacks:              connect.NewCallbackList[receivePacketBatchFunction](),
		probeSuiteState:                          &probeSuite{},
		canShowRatingDialogChangeListeners:       connect.NewCallbackList[CanShowRatingDialogChangeListener](),
		canPromptIntroFunnelChangeListeners:      connect.NewCallbackList[CanPromptIntroFunnelChangeListener](),
		allowForegroundChangeListeners:           connect.NewCallbackList[AllowForegroundChangeListener](),
		canReferChangeListeners:                  connect.NewCallbackList[CanReferChangeListener](),
		provideModeChangeListeners:               connect.NewCallbackList[ProvideModeChangeListener](),
		provideChangeListeners:                   connect.NewCallbackList[ProvideChangeListener](),
		provideControlModeChangeListeners:        connect.NewCallbackList[ProvideControlModeChangeListener](),
		performanceProfileChangeListeners:        connect.NewCallbackList[PerformanceProfileChangeListener](),
		providerIdentityChangeListeners:          connect.NewCallbackList[ProviderIdentityChangeListener](),
		connectedProviderLocationChangeListeners: connect.NewCallbackList[ConnectedProviderLocationChangeListener](),
		providePausedChangeListeners:             connect.NewCallbackList[ProvidePausedChangeListener](),
		provideNetworkModeChangeListeners:        connect.NewCallbackList[ProvideNetworkModeChangeListener](),
		offlineChangeListeners:                   connect.NewCallbackList[OfflineChangeListener](),
		vpnInterfaceWhileOfflineChangeListeners:  connect.NewCallbackList[VpnInterfaceWhileOfflineChangeListener](),
		connectChangeListeners:                   connect.NewCallbackList[ConnectChangeListener](),
		routeLocalChangeListeners:                connect.NewCallbackList[RouteLocalChangeListener](),
		blockerEnabledChangeListeners:            connect.NewCallbackList[BlockerEnabledChangeListener](),
		connectLocationChangeListeners:           connect.NewCallbackList[ConnectLocationChangeListener](),
		defaultLocationChangeListeners:           connect.NewCallbackList[DefaultLocationChangeListener](),
		provideSecretKeysListeners:               connect.NewCallbackList[ProvideSecretKeysListener](),
		contractStatusChangeListeners:            connect.NewCallbackList[ContractStatusChangeListener](),
		tunnelChangeListeners:                    connect.NewCallbackList[TunnelChangeListener](),
		windowStatusChangeListeners:              connect.NewCallbackList[WindowStatusChangeListener](),
		jwtRefreshListeners:                      connect.NewCallbackList[JwtRefreshListener](),
		authLogoutListeners:                      connect.NewCallbackList[AuthLogoutListener](),
		blockActionWindowChangeListeners:         connect.NewCallbackList[BlockActionWindowChangeListener](),
		blockStatsChangeListeners:                connect.NewCallbackList[BlockStatsChangeListener](),
		blockActionOverridesChangeListeners:      connect.NewCallbackList[BlockActionOverridesChangeListener](),
		transportSettingsChangeListeners:         connect.NewCallbackList[TransportSettingsChangeListener](),
		providerTransportSettingsChangeListeners: connect.NewCallbackList[ProviderTransportSettingsChangeListener](),
		transportStatusChangeListeners:           connect.NewCallbackList[TransportStatusChangeListener](),
		providerTransportStatusChangeListeners:   connect.NewCallbackList[ProviderTransportStatusChangeListener](),
		packetStatsChangeListeners:               connect.NewCallbackList[PacketStatsChangeListener](),
		egressContractStatsChangeListeners:       connect.NewCallbackList[ContractStatsChangeListener](),
		egressContractDetailsChangeListeners:     connect.NewCallbackList[ContractDetailsChangeListener](),
		ingressContractStatsChangeListeners:      connect.NewCallbackList[ContractStatsChangeListener](),
		ingressContractDetailsChangeListeners:    connect.NewCallbackList[ContractDetailsChangeListener](),
		dnsResolverSettingsChangeListeners:       connect.NewCallbackList[DnsResolverSettingsChangeListener](),
		networkPeersChangeListeners:              connect.NewCallbackList[NetworkPeersChangeListener](),

		providerPacketStatsChangeListeners:            connect.NewCallbackList[PacketStatsChangeListener](),
		providerEgressContractStatsChangeListeners:    connect.NewCallbackList[ContractStatsChangeListener](),
		providerEgressContractDetailsChangeListeners:  connect.NewCallbackList[ContractDetailsChangeListener](),
		providerIngressContractStatsChangeListeners:   connect.NewCallbackList[ContractStatsChangeListener](),
		providerIngressContractDetailsChangeListeners: connect.NewCallbackList[ContractDetailsChangeListener](),
	}
	// restore the persisted block action overrides and dns resolver settings
	if localState != nil {
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
		// the routing tier persists on set (see SetRoutingTier); unset reads
		// RoutingTierOff, matching its zero value. Restoring here (rather than
		// leaving the zero-initialized field) is what makes a tier chosen in a
		// PRIOR process survive into the very first window this process
		// builds, before SetRoutingTier is ever called again
		deviceLocal.routingTier = localState.GetRoutingTier()
		// seed the DoH fan-out order from the last session's per-server scores,
		// so the first lookups after launch pick the known-fastest server
		deviceLocal.dohServerScoresSeed = localState.getDohServerScores()
		// window identity persistence: a relaunch that reconnects to the same
		// destination reuses the last session's window client identities —
		// skipping an auth api round trip per window client during formation
		// (see window_identity_store.go). Only when no store was provided and
		// the device is locally owned (a hosted device's store comes from the
		// embedding host).
		if !settings.HostedIncompatible && settings.MultiClientIdentityStore == nil {
			deviceLocal.windowIdentityStore = newLocalStateWindowIdentityStore(localState, clientId)
			settings.MultiClientIdentityStore = deviceLocal.windowIdentityStore
		}
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

	// Api is the credential owner. DeviceLocal subscribes to apply rotations
	// to persistence and connect transports, while keeping its established
	// device-level listener contract for the applications.
	api.setLog(log)
	deviceLocal.apiJwtRefreshSub = api.AddJwtRefreshListener(
		jwtRefreshListenerFunc(deviceLocal.SetByJwt),
	)
	deviceLocal.apiAuthLogoutSub = api.AddAuthLogoutListener(
		authLogoutListenerFunc(func() {
			if err := logout(); err != nil {
				log.Errorf("failed to clear local auth state: %v", err)
			}
			deviceLocal.handleApiAuthLogout()
		}),
	)
	api.SetByJwt(byJwt)
	api.StartJwtRefresh()

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
	} else {
		newSecurityPolicyMonitor(ctx, deviceLocal, settings.Verbose)
	}

	// initial allocation: providing starts off (provide mode none), so the
	// provider share backs the client pair until SetProvideMode enables it
	deviceLocal.applyProvideMemorySharesWithLock(false)

	return deviceLocal, nil
}

//gomobile:noexport
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
// TUN. With no explicit server, DeviceLocal advertises the resolver's dedicated
// upgrade-mask address; plain DNS is required for the UpgradeMux to intercept and
// upgrade :53 traffic.
func (self *DeviceLocal) TunnelDnsSetting() *TunnelDnsSetting {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.tunnelDnsSetting
}

// SetTunnelDnsSetting overrides the platform DNS configuration. Each use case
// sets its own (apps use the default URnetwork-owned identity; server/proxy may
// differ). A non-empty Server narrows the tunnel to that single identity.
func (self *DeviceLocal) SetTunnelDnsSetting(setting *TunnelDnsSetting) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.tunnelDnsSetting = setting
}

// TunnelDnsAddressesIpv4 returns the plain-DNS IPv4 server IPs the platform should
// apply to the TUN interface (Android `addDnsServer`), sourced from the device at
// tunnel-build time like TunnelLocalAddress: the dns resolver settings'
// unencrypted local servers when set, otherwise DnsUpgradeMaskAddress. The mask
// must differ from TunnelLocalAddress: an OS treats its assigned interface
// address as a local host destination, so DNS sent there never reaches the TUN
// packet reader. Plain :53 keeps the UpgradeMux able to intercept and upgrade
// the query before the stand-in address is reached.
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
	dnsAddresses := tunnelDnsAddresses(resolver, self.tunnelDnsSetting, ipv6)
	if self.tunnelLocalAddress.IsValid() {
		filteredAddresses := make([]string, 0, len(dnsAddresses))
		for _, dnsAddress := range dnsAddresses {
			address, err := netip.ParseAddr(dnsAddress)
			if err == nil && address.Unmap() == self.tunnelLocalAddress.Unmap() {
				continue
			}
			filteredAddresses = append(filteredAddresses, dnsAddress)
		}
		dnsAddresses = filteredAddresses
	}
	// A persisted/custom mask or explicit resolver can accidentally equal the
	// assigned TUN address. Never emit an empty resolver list for that collision:
	// fall back to the separately owned, routable-through-TUN mask identity.
	if len(dnsAddresses) == 0 && self.tunnelDnsSetting != nil {
		dnsAddresses = defaultTunnelDnsServers(ipv6)
	}
	addresses := NewStringList()
	addresses.addAll(dnsAddresses...)
	return addresses
}

// tunnelDnsAddresses derives the plain-dns tunnel resolver ips of one address
// family with the context-free fallback.
func tunnelDnsAddresses(resolver *connect.DnsResolverSettings, tunnelDnsSetting *TunnelDnsSetting, ipv6 bool) []string {
	return tunnelDnsAddressesWithDefault(
		resolver,
		tunnelDnsSetting,
		defaultTunnelDnsServers(ipv6),
		ipv6,
	)
}

// tunnelDnsAddressesWithDefault derives the platform DNS addresses for one
// family. Explicit local DNS remains an actual resolver override. Otherwise an
// explicit TunnelDnsSetting server wins, then the resolver's
// DnsUpgradeMaskAddress, then defaultServers. The mask is the platform-facing
// stand-in for UpgradeMux, not an upstream resolver. Entries that do not parse
// as IPs are dropped so the platform never applies a bad address.
func tunnelDnsAddressesWithDefault(
	resolver *connect.DnsResolverSettings,
	tunnelDnsSetting *TunnelDnsSetting,
	defaultServers []string,
	ipv6 bool,
) []string {
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
		// Preserve the older explicit per-tunnel override.
		if server := strings.TrimSpace(tunnelDnsSetting.Server); server != "" {
			return family([]string{server})
		}
	}
	if tunnelDnsSetting != nil {
		if resolver != nil {
			if servers := family([]string{resolver.DnsUpgradeMaskAddress}); 0 < len(servers) {
				return servers
			}
		}
		return family(defaultServers)
	}
	return []string{}
}

// SetUpgradeMuxSettings sets how the interposed mux resolves DNS and upgrades HTTP.
// It takes effect when the remote client is next (re)created. nil disables the mux
// (direct pass-through to the exit). gomobile:noexport until a platform-friendly
// settings surface lands with the per-use-case defaults.
//
//gomobile:noexport
func (self *DeviceLocal) SetUpgradeMuxSettings(settings *connect.UpgradeMuxSettings) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if settings != nil && settings.Dns != nil {
		// the device's dns byte budget always rides the mux settings (the
		// caller does not own memory sizing), as does the learned DoH server
		// ordering
		settings.Dns.MemoryTarget = self.dnsMemoryTarget
		settings.Dns.ServerStatsSeed = self.dohServerScoresSeed
	}
	self.upgradeMuxSettings = settings
	// apply to the live mux immediately when non-nil (rebuilds its DohCache); nil takes
	// effect on the next client recreation, which then creates no mux
	if self.upgradeMux != nil && settings != nil {
		self.upgradeMux.SetSettings(settings)
	}
}

// DeviceLocalMemoryUsage is a point-in-time sample of the device's tracked
// memory accounting versus its target (see
// `DeviceLocalSettings.MemoryTargetByteCount`). Tracked usage covers the
// live budget accounting: in-flight dns resolution, the client transfer
// queue pair (including p2p peer connection reservations), and the provider
// client's pair. The egress nat's per-flow memory is bounded by flow-count
// caps rather than live byte accounting, so it is not included here — the
// memory target load test measures that remainder as process heap.
type DeviceLocalMemoryUsage struct {
	TargetByteCount          ByteCount
	DnsByteCount             ByteCount
	ClientSendByteCount      ByteCount
	ClientReceiveByteCount   ByteCount
	ProviderSendByteCount    ByteCount
	ProviderReceiveByteCount ByteCount
	TotalByteCount           ByteCount
}

// MemoryUsed samples the tracked memory accounting of this device's areas
func (self *DeviceLocal) MemoryUsed() *DeviceLocalMemoryUsage {
	usage := &DeviceLocalMemoryUsage{
		TargetByteCount: self.settings.MemoryTargetByteCount,
		DnsByteCount:    self.dnsMemoryTarget.Used(),
	}
	if sendBufferSettings := self.settings.ClientSettings.SendBufferSettings; sendBufferSettings != nil && sendBufferSettings.ResendQueueBudget != nil {
		usage.ClientSendByteCount = sendBufferSettings.ResendQueueBudget.UsedByteCount()
	}
	if receiveBufferSettings := self.settings.ClientSettings.ReceiveBufferSettings; receiveBufferSettings != nil && receiveBufferSettings.ReceiveQueueBudget != nil {
		usage.ClientReceiveByteCount = receiveBufferSettings.ReceiveQueueBudget.UsedByteCount()
	}
	if self.provider != nil {
		// a nil pair means the provider shares the device client budgets
		// (already counted above)
		if resendQueueBudget, receiveQueueBudget := self.provider.transferBudgets(); resendQueueBudget != nil {
			usage.ProviderSendByteCount = resendQueueBudget.UsedByteCount()
			usage.ProviderReceiveByteCount = receiveQueueBudget.UsedByteCount()
		}
	}
	usage.TotalByteCount = usage.DnsByteCount +
		usage.ClientSendByteCount + usage.ClientReceiveByteCount +
		usage.ProviderSendByteCount + usage.ProviderReceiveByteCount
	return usage
}

// SetClientSecurityPolicyGenerator sets the multi-client (the device's own traffic) security policy.
//
//gomobile:noexport
func (self *DeviceLocal) SetClientSecurityPolicyGenerator(g func(context.Context, *connect.SecurityPolicyStatsCollector) connect.SecurityPolicy) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.clientSecurityPolicyGenerator = g
}

// SetProviderSecurityPolicyGenerator sets the provider (egressing remote clients' traffic) security
// policy. Defaults to the reversed client policy (connect.DefaultProviderSecurityPolicyWithStats).
//
//gomobile:noexport
func (self *DeviceLocal) SetProviderSecurityPolicyGenerator(g func(context.Context, *connect.SecurityPolicyStatsCollector) connect.SecurityPolicy) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.providerSecurityPolicyGenerator = g
}

func (self *DeviceLocal) RefreshToken(attempt int) error {
	self.GetApi().RequestJwtRefresh()
	return nil
}

// hostedSafePerformanceProfile forces direct mode (`AllowDirect`) off on the
// way into a hosted device's stored state, so `GetPerformanceProfile` is
// truthful and the input profile is not mutated in place. Direct mode on a
// hosted device would leak that the device is hosted, and where it is hosted,
// via the host addresses in the direct connection setup. Defense in depth
// alongside the equivalent hard limit on the live multi client
// (`connect.MultiClientSettings.OverrideAllowDirect` = false).
func (self *DeviceLocal) hostedSafePerformanceProfile(performanceProfile *PerformanceProfile) *PerformanceProfile {
	if !self.settings.HostedIncompatible {
		return performanceProfile
	}
	if performanceProfile == nil || !performanceProfile.AllowDirect {
		return performanceProfile
	}
	limited := *performanceProfile
	limited.AllowDirect = false
	return &limited
}

func (self *DeviceLocal) SetPerformanceProfile(performanceProfile *PerformanceProfile) {
	if limited := self.hostedSafePerformanceProfile(performanceProfile); limited != performanceProfile {
		self.log.Infof("[device]hosted incompatible: AllowDirect forced off\n")
		performanceProfile = limited
	}
	// Own the stored value. App presentation layers commonly retain and mutate
	// their model objects; retaining the caller's pointer would let those
	// mutations silently change transport policy without change detection,
	// callbacks, or a corresponding multi-client update.
	performanceProfile = clonePerformanceProfile(performanceProfile)
	var remoteUserNatClient connect.UserNatClient
	storedChanged := false
	behaviorChanged := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		storedChanged = !performanceProfileValuesEqual(self.performanceProfile, performanceProfile)
		if storedChanged {
			behaviorChanged = !performanceProfilesEqual(self.performanceProfile, performanceProfile)
			self.performanceProfile = performanceProfile
			if behaviorChanged {
				remoteUserNatClient = self.remoteUserNatClient
			}
		}
	}()
	if !storedChanged {
		// Presentation layers reconstruct value objects when they resume.
		// An exactly equal value changes neither public state nor behavior.
		return
	}
	if remoteUserNatClient != nil {
		switch v := remoteUserNatClient.(type) {
		case *connect.RemoteUserNatClient:
			// pqe applies to the multi client window clients only; the
			// single client path carries just the direct-mode setting
			if performanceProfile != nil {
				v.SetAllowDirect(performanceProfile.AllowDirect)
			} else {
				v.SetAllowDirect(false)
			}
		case *connect.RemoteUserNatMultiClient:
			v.SetPerformanceProfile(toConnectPerformanceProfile(performanceProfile))
		}
	}
	// Preserve the public value contract even when this was only a
	// representation change (for example nil -> explicit auto). Listeners and
	// getters see what was set, while the live transport above remains
	// untouched unless installed behavior changed.
	self.performanceProfileChanged(performanceProfile)
}

func (self *DeviceLocal) GetPerformanceProfile() *PerformanceProfile {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return clonePerformanceProfile(self.performanceProfile)
}

// GetPublicIdentityKey returns a copy of the provider client's long-lived
// public identity key, or nil when the device has no provider
func (self *DeviceLocal) GetPublicIdentityKey() []byte {
	client := self.providerClientSnapshot()
	if client == nil {
		return nil
	}
	// `ClientKeyManager.PublicKey` returns a fresh copy
	return []byte(client.ClientKeyManager().PublicKey())
}

func (self *DeviceLocal) GetPublicIdentityKeyHash() string {
	publicKey := self.GetPublicIdentityKey()
	if len(publicKey) == 0 {
		return ""
	}
	return PublicIdentityKeyHash(publicKey)
}

// GetProviderIdentities returns the providers with an established,
// identity-verified e2e session. Empty (never nil) when disconnected
func (self *DeviceLocal) GetProviderIdentities() *ProviderIdentityList {
	var remoteUserNatClient connect.UserNatClient
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		remoteUserNatClient = self.remoteUserNatClient
	}()

	providerIdentities := NewProviderIdentityList()
	if multi, ok := remoteUserNatClient.(*connect.RemoteUserNatMultiClient); ok {
		for _, peerIdentity := range multi.PeerIdentities() {
			providerIdentities.Add(&ProviderIdentity{
				ClientId:  newId(peerIdentity.PeerId),
				PublicKey: bytes.Clone(peerIdentity.PublicKey),
			})
		}
	}
	return providerIdentities
}

// GetConnectedProviderLocations returns the currently connected
// (routing-eligible) window providers with their locations, sorted
// oldest-connected first. Empty (never nil) when disconnected
func (self *DeviceLocal) GetConnectedProviderLocations() *ConnectedProviderLocationList {
	monitor := self.windowMonitor()
	if monitor == nil {
		return NewConnectedProviderLocationList()
	}
	_, providerEvents := monitor.Events()
	return deriveConnectedProviderLocations(providerEvents)
}

func performanceProfilesEqual(a *PerformanceProfile, b *PerformanceProfile) bool {
	allowDirect := func(profile *PerformanceProfile) bool {
		return profile != nil && profile.AllowDirect
	}
	postQuantumEncryption := func(profile *PerformanceProfile) bool {
		return profile != nil && profile.PostQuantumEncryption
	}
	if allowDirect(a) != allowDirect(b) ||
		postQuantumEncryption(a) != postQuantumEncryption(b) {
		return false
	}

	windowType := func(profile *PerformanceProfile) WindowType {
		if profile == nil {
			return WindowTypeAuto
		}
		switch profile.WindowType {
		case WindowTypeQuality, WindowTypeSpeed:
			return profile.WindowType
		default:
			return WindowTypeAuto
		}
	}
	aWindowType := windowType(a)
	bWindowType := windowType(b)
	if aWindowType != bWindowType {
		return false
	}
	if aWindowType == WindowTypeAuto {
		// Auto mode ignores WindowSize; nil, unset, and explicit auto install
		// the same transport behavior.
		return true
	}
	return windowSizeSettingsEqual(a.WindowSize, b.WindowSize)
}

// performanceProfileValuesEqual compares the exact public value rather than
// installed transport behavior. It deliberately distinguishes nil, unset,
// and explicit auto so Set/Get and listener round trips preserve app state.
func performanceProfileValuesEqual(a *PerformanceProfile, b *PerformanceProfile) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.WindowType != b.WindowType ||
		a.AllowDirect != b.AllowDirect ||
		a.PostQuantumEncryption != b.PostQuantumEncryption {
		return false
	}
	if a.WindowSize == nil || b.WindowSize == nil {
		return a.WindowSize == nil && b.WindowSize == nil
	}
	return *a.WindowSize == *b.WindowSize
}

func clonePerformanceProfile(performanceProfile *PerformanceProfile) *PerformanceProfile {
	if performanceProfile == nil {
		return nil
	}
	cloned := *performanceProfile
	if performanceProfile.WindowSize != nil {
		windowSize := *performanceProfile.WindowSize
		cloned.WindowSize = &windowSize
	}
	return &cloned
}

func windowSizeSettingsEqual(a *WindowSizeSettings, b *WindowSizeSettings) bool {
	effective := func(settings *WindowSizeSettings) WindowSizeSettings {
		if settings != nil {
			return *settings
		}
		// Mirrors connect.DefaultWindowSizeSettings, which is what
		// toConnectWindowSize installs for an omitted fixed window.
		return WindowSizeSettings{
			WindowSizeMin:            1,
			WindowSizeMax:            1,
			WindowSizeHardMax:        4,
			WindowSizeReconnectScale: 1.0,
			KeepHealthiestCount:      1,
		}
	}
	aEffective := effective(a)
	bEffective := effective(b)
	return aEffective == bEffective
}

func connectLocationsEqual(a *ConnectLocation, b *ConnectLocation) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equals(b)
}

func connectLocationTransportEqual(a *ConnectLocation, b *ConnectLocation) bool {
	if !connectLocationsEqual(a, b) {
		return false
	}
	if a == nil {
		return true
	}
	// NetworkPeer changes admission, receive-buffer sizing, and the provider
	// mode used by this destination. The other descriptive location fields do
	// not affect the installed transport.
	return a.NetworkPeer == b.NetworkPeer
}

func idsEqual(a *Id, b *Id) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Cmp(b) == 0
}

func connectLocationValuesEqual(a *ConnectLocation, b *ConnectLocation) bool {
	if !connectLocationsEqual(a, b) {
		return false
	}
	if a == nil {
		return true
	}
	return a.Name == b.Name &&
		a.ProviderCount == b.ProviderCount &&
		a.Promoted == b.Promoted &&
		a.MatchDistance == b.MatchDistance &&
		a.LocationType == b.LocationType &&
		a.City == b.City &&
		a.Region == b.Region &&
		a.Country == b.Country &&
		a.CountryCode == b.CountryCode &&
		idsEqual(a.CityLocationId, b.CityLocationId) &&
		idsEqual(a.RegionLocationId, b.RegionLocationId) &&
		idsEqual(a.CountryLocationId, b.CountryLocationId) &&
		a.Stable == b.Stable &&
		a.StrongPrivacy == b.StrongPrivacy &&
		a.NetworkPeer == b.NetworkPeer
}

func cloneId(id *Id) *Id {
	if id == nil {
		return nil
	}
	cloned := *id
	return &cloned
}

func cloneConnectLocation(location *ConnectLocation) *ConnectLocation {
	if location == nil {
		return nil
	}
	cloned := *location
	if location.ConnectLocationId != nil {
		connectLocationId := *location.ConnectLocationId
		connectLocationId.ClientId = cloneId(location.ConnectLocationId.ClientId)
		connectLocationId.LocationId = cloneId(location.ConnectLocationId.LocationId)
		connectLocationId.LocationGroupId = cloneId(location.ConnectLocationId.LocationGroupId)
		cloned.ConnectLocationId = &connectLocationId
	}
	cloned.CityLocationId = cloneId(location.CityLocationId)
	cloned.RegionLocationId = cloneId(location.RegionLocationId)
	cloned.CountryLocationId = cloneId(location.CountryLocationId)
	return &cloned
}

func cloneProviderSpec(spec *ProviderSpec) *ProviderSpec {
	if spec == nil {
		return nil
	}
	cloned := *spec
	cloned.LocationId = cloneId(spec.LocationId)
	cloned.LocationGroupId = cloneId(spec.LocationGroupId)
	cloned.ClientId = cloneId(spec.ClientId)
	return &cloned
}

func cloneProviderSpecs(specs []*ProviderSpec) []*ProviderSpec {
	if specs == nil {
		return nil
	}
	cloned := make([]*ProviderSpec, 0, len(specs))
	for _, spec := range specs {
		cloned = append(cloned, cloneProviderSpec(spec))
	}
	return cloned
}

func sdkProviderSpecsFingerprint(specs []*ProviderSpec) string {
	connectSpecs := make([]*connect.ProviderSpec, 0, len(specs))
	for _, spec := range specs {
		if spec != nil {
			connectSpecs = append(connectSpecs, spec.toConnectProviderSpec())
		}
	}
	return providerSpecsFingerprint(connectSpecs)
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
		if err := self.networkSpace.asyncLocalState.localState.setRefreshedByJwt(
			byJwt,
			newId(self.instanceId),
		); err != nil {
			self.log.Errorf("failed to persist refreshed JWT: %v", err)
		}
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

func (self *DeviceLocal) handleApiAuthLogout() {
	var provider *deviceLocalProvider
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.byJwt = ""
		provider = self.provider
	}()
	if provider != nil {
		provider.SetByJwt("")
	}
	self.authLogout()
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
		}

		// enforce the control mode's provide mapping even when the control mode
		// value is unchanged: the provide mode may have been set independently
		// (e.g. a stale persisted mode applied at init), and the control mode
		// owns the provide mode. `setProvideModeWithLock` no-ops when already
		// consistent, so redundant calls are cheap.
		switch self.provideControlMode {
		case ProvideControlModeAuto:
			if self.remoteUserNatClient != nil {
				// if user is connected, provide publicly
				provideChanged = self.setProvideModeWithLock(ProvideModePublic)
			} else {
				// if not connected, keep providing to same-network peers so this
				// device stays reachable/discoverable as a peer even when idle
				provideChanged = self.setProvideModeWithLock(ProvideModeNetwork)
			}
		case ProvideControlModeAlways:
			provideChanged = self.setProvideModeWithLock(ProvideModePublic)
		case ProvideControlModeNetwork:
			// the private provider: always on, but only for same-network peers
			provideChanged = self.setProvideModeWithLock(ProvideModeNetwork)
		case ProvideControlModeManual:
			// manual: the explicitly set provide mode is the truth — the
			// control mode enforces nothing
		default:
			// never (and any unknown mode, conservatively): no providing
			provideChanged = self.setProvideModeWithLock(ProvideModeNone)
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
		return self.cachedWindowMonitor(self.remoteUserNatClient, func() windowMonitor {
			return newFixedWindowMonitor(v.DestinationIds())
		})
	case *connect.RemoteUserNatMultiClient:
		// the multi client's monitor is already a stable instance
		return v.Monitor()
	default:
		// an empty window monitor to be consistent with the device remote behavior
		return self.cachedWindowMonitor(self.remoteUserNatClient, func() windowMonitor {
			return &emptyWindowMonitor{}
		})
	}
}

// cachedWindowMonitor returns a stable windowMonitor instance per
// remoteUserNatClient value, so callers that detect monitor changes by identity
// (DeviceLocalRpc.updateWindowMonitor) do not see a spurious change on every
// call for the fixed/empty monitor types.
func (self *DeviceLocal) cachedWindowMonitor(client connect.UserNatClient, create func() windowMonitor) windowMonitor {
	self.windowMonitorCacheLock.Lock()
	defer self.windowMonitorCacheLock.Unlock()
	if self.windowMonitorCacheClient != client || self.windowMonitorCache == nil {
		self.windowMonitorCache = create()
		self.windowMonitorCacheClient = client
	}
	return self.windowMonitorCache
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

func (self *DeviceLocal) AddProviderIdentityChangeListener(listener ProviderIdentityChangeListener) Sub {
	callbackId := self.providerIdentityChangeListeners.Add(listener)
	return newSub(func() {
		self.providerIdentityChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) AddConnectedProviderLocationChangeListener(listener ConnectedProviderLocationChangeListener) Sub {
	callbackId := self.connectedProviderLocationChangeListeners.Add(listener)
	return newSub(func() {
		self.connectedProviderLocationChangeListeners.Remove(callbackId)
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
			// Each callback receives an owned snapshot. One app listener must
			// not be able to mutate the device state or another listener's
			// observation through a shared gomobile pointer.
			listener.PerformanceProfileChanged(clonePerformanceProfile(performanceProfile))
		})
	}
}

func (self *DeviceLocal) providerIdentitiesChanged() {
	for _, listener := range self.providerIdentityChangeListeners.Get() {
		connect.HandleError(func() {
			listener.ProviderIdentitiesChanged()
		})
	}
}

func (self *DeviceLocal) connectedProviderLocationsChanged() {
	for _, listener := range self.connectedProviderLocationChangeListeners.Get() {
		connect.HandleError(func() {
			listener.ConnectedProviderLocationsChanged()
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
			listener.ConnectLocationChanged(cloneConnectLocation(location))
		})
	}
}

func (self *DeviceLocal) defaultLocationChanged(location *ConnectLocation) {
	for _, listener := range self.defaultLocationChangeListeners.Get() {
		connect.HandleError(func() {
			listener.DefaultLocationChanged(cloneConnectLocation(location))
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
	// These are final device-TUN adapters. They intentionally run inline so
	// Connect acknowledges a borrowed packet only after native injection has
	// accepted it, the narrow exception documented in connect/CODESTYLE.md.
	packetStorage := [1][]byte{packet}
	self.receivePacketBatch(packetStorage[:])
	for _, receivePacketsCallback := range self.receivePacketsCallbacks.Get() {
		receivePacketsCallback(source, provideMode, ipPath, packetStorage[:])
	}
	for _, receiveCallback := range self.receiveCallbacks.Get() {
		receiveCallback(source, provideMode, ipPath, packet)
	}
}

// A common remote burst crosses the app boundary once. Singular observers
// still receive every packet because callback subscriptions are independent.
func (self *DeviceLocal) receivePackets(
	source connect.TransferPath,
	provideMode protocol.ProvideMode,
	ipPath *connect.IpPath,
	packets [][]byte,
) {
	// Batched form of the synchronous final-injection boundary above.
	self.receivePacketBatch(packets)
	for _, receivePacketsCallback := range self.receivePacketsCallbacks.Get() {
		receivePacketsCallback(source, provideMode, ipPath, packets)
	}
	for _, receiveCallback := range self.receiveCallbacks.Get() {
		for _, packet := range packets {
			receiveCallback(source, provideMode, ipPath, packet)
		}
	}
}

const (
	devicePacketBatchMaxPacketCount = 64
	devicePacketBatchMaxByteCount   = 96 * 1024
)

// withEncodedPacketBatches validates the complete burst before emitting any
// borrowed buffers, then splits it by both packet count and encoded byte count.
// This keeps native-bridge copies bounded without dropping a valid MTU-sized
// packet merely because it arrived at the edge of a burst.
func withEncodedPacketBatches(packets [][]byte, callback func([]byte)) bool {
	for _, packet := range packets {
		if len(packet) == 0 || 65535 < len(packet) {
			return false
		}
	}
	if len(packets) == 0 {
		return false
	}

	for packetStart := 0; packetStart < len(packets); {
		packetEnd := packetStart
		packetBatchByteCount := 0
		for packetEnd < len(packets) && packetEnd-packetStart < devicePacketBatchMaxPacketCount {
			encodedByteCount := 2 + len(packets[packetEnd])
			if 0 < packetBatchByteCount && devicePacketBatchMaxByteCount < packetBatchByteCount+encodedByteCount {
				break
			}
			packetBatchByteCount += encodedByteCount
			packetEnd += 1
		}

		func() {
			packetBatchBytes := connect.MessagePoolGet(packetBatchByteCount)
			defer connect.MessagePoolReturn(packetBatchBytes)
			offset := 0
			for _, packet := range packets[packetStart:packetEnd] {
				binary.BigEndian.PutUint16(
					packetBatchBytes[offset:offset+2],
					uint16(len(packet)),
				)
				offset += 2
				copy(packetBatchBytes[offset:offset+len(packet)], packet)
				offset += len(packet)
			}
			callback(packetBatchBytes)
		}()
		packetStart = packetEnd
	}
	return true
}

// Native adapters receive one borrowed buffer rather than crossing the ABI
// once per packet. Framing matches SendPacketBatch in the reverse direction.
func (self *DeviceLocal) receivePacketBatch(packets [][]byte) {
	callbacks := self.receivePacketBatchCallbacks.Get()
	if len(callbacks) == 0 {
		return
	}
	withEncodedPacketBatches(packets, func(packetBatchBytes []byte) {
		for _, callback := range callbacks {
			callback(packetBatchBytes)
		}
	})
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

// providerLocalUserNatSettings builds the settings for the provide exit nat.
// Unlike the local-traffic nats (a single trusted source, no limits), the
// exit nat serves unbounded remote sources, so the per source and aggregate
// flow counts are bounded (lru evict of the idle-most flow) to put a hard
// ceiling on flow state, sockets, and goroutines under any remote behavior.
// Scaled by the memory budget (see `SetMemoryLimit`).
// applyProvideMemorySharesWithLock reallocates the transfer budget
// capacities between the client and provider pairs for the provide state:
// while providing is off, the provider share backs the client pair instead
// of idling, and the provider pair drops to its working floor. The dns
// share is unaffected. Applies live — queues admit against the new
// capacities immediately (a shrunken pool admits nothing new above its new
// total until it drains). Called under stateLock (and once single-threaded
// at construction with the initial off state).
func (self *DeviceLocal) applyProvideMemorySharesWithLock(provideActive bool) {
	_, clientShareByteCount, providerShareByteCount := deviceMemoryShares(self.settings)
	if clientShareByteCount <= 0 {
		// no target: legacy static sizing
		return
	}
	clientPairByteCount := clientShareByteCount
	providerPairByteCount := ByteCount(0)
	if provideActive {
		// half the provider share backs the provider client's pair; the
		// other half sizes the egress nat flow caps (rebuilt on provide-on)
		providerPairByteCount = providerShareByteCount / 2
	} else {
		clientPairByteCount += providerShareByteCount
	}
	if resendQueueBudget := self.settings.ClientSettings.SendBufferSettings.ResendQueueBudget; resendQueueBudget != nil {
		resendQueueBudget.SetTotalByteCount(max(byteCountFraction(clientPairByteCount, 3, 7), 1024*1024))
	}
	if receiveQueueBudget := self.settings.ClientSettings.ReceiveBufferSettings.ReceiveQueueBudget; receiveQueueBudget != nil {
		receiveQueueBudget.SetTotalByteCount(max(byteCountFraction(clientPairByteCount, 4, 7), 1536*1024))
	}
	if self.provider != nil {
		if resendQueueBudget, receiveQueueBudget := self.provider.transferBudgets(); resendQueueBudget != nil {
			// the floors keep the idle provider client's control sequences
			// working while its pool is reallocated
			resendQueueBudget.SetTotalByteCount(max(byteCountFraction(providerPairByteCount, 3, 7), 256*1024))
			receiveQueueBudget.SetTotalByteCount(max(byteCountFraction(providerPairByteCount, 4, 7), 384*1024))
		}
	}
}

func providerLocalUserNatSettings(
	memoryTargetByteCount ByteCount,
	log connect.Logger,
	dialContextSettings ...*connect.DialContextSettings,
) *connect.LocalUserNatSettings {
	localUserNatSettings := connect.DefaultProviderLocalUserNatSettingsWithMemoryTarget(memoryTargetByteCount)
	localUserNatSettings.Log = log
	if len(dialContextSettings) != 0 && dialContextSettings[0] != nil {
		// Both protocols must expose the same address identity. ICMP uses a
		// platform-specific packet backend and is not involved in /verify.
		localUserNatSettings.TcpBufferSettings.DialContextSettings = dialContextSettings[0]
		localUserNatSettings.UdpBufferSettings.DialContextSettings = dialContextSettings[0]
	}
	return localUserNatSettings
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

			// reallocate the provider share of the memory target between the
			// client and provider pairs for the new provide state
			self.applyProvideMemorySharesWithLock(provideMode != ProvideModeNone)

			if provideMode != ProvideModeNone {
				// recreate the provider user nat only as needed
				// this avoid connection disruptions
				if self.remoteUserNatProviderLocalUserNat == nil {
					_, _, providerShareByteCount := deviceMemoryShares(self.settings)
					localUserNatSettings := providerLocalUserNatSettings(
						providerShareByteCount,
						self.log,
						self.settings.ProviderDialContextSettings,
					)
					self.remoteUserNatProviderLocalUserNat = connect.NewLocalUserNat(client.Ctx(), self.clientId.String(), localUserNatSettings)
				}
				if self.remoteUserNatProvider == nil {
					// the provider egresses remote clients' traffic and runs its own security policy:
					// the connect default is the reversed client policy
					// (DefaultProviderSecurityPolicyWithStats), or an explicitly set provider policy
					_, _, providerShareByteCount := deviceMemoryShares(self.settings)
					providerSettings := connect.DefaultRemoteUserNatProviderSettingsWithMemoryTarget(providerShareByteCount)
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

// saveDohServerScoresWithLock captures a mux's learned per-DoH-server ordering into the
// in-session seed (so the next mux build starts from live experience) and persists it in the
// background for the next session's first lookups. Callers hold stateLock; the file write
// happens off the lock.
func (self *DeviceLocal) saveDohServerScoresWithLock(upgradeMux *connect.UpgradeMux) {
	scores := upgradeMux.DnsServerScores()
	if len(scores) == 0 {
		return
	}
	self.dohServerScoresSeed = scores
	if asyncLocalState := self.networkSpace.GetAsyncLocalState(); asyncLocalState != nil {
		localState := asyncLocalState.GetLocalState()
		go connect.HandleError(func() {
			localState.setDohServerScores(scores)
		})
	}
}

// NetworkChanged reacts to a host network path change (wifi<->cell, interface
// change): every platform transport closes its live connection and re-dials
// over the new path immediately — instead of discovering the dead socket via
// ping timeouts seconds later — and the mux drops its pooled DoH connections,
// treats the tunnel-DoH as unproven (short local fallback until it re-proves),
// and re-warms in the background. Apps call this from the OS path-update
// signal (NWPathMonitor / ConnectivityManager); it is cheap and safe to call
// on every update.
// SetPerformanceDegraded reports the host's degraded-performance state: low
// power mode, thermal throttling, or a weak/constrained network. While set,
// the window clients' liveness probe timings are scaled up (default 3x) so a
// device that legitimately answers slowly is not misdiagnosed as a dead peer
// — a false removal (flow resets + reconnect churn) costs more than slower
// dead-peer detection. The idle keepalive rest stretches by the same factor,
// so an idle tunnel wakes the radio less often while the host is trying to
// save power. Apps call this from the OS signals (iOS low power mode /
// thermal state / constrained path; android power save mode / thermal
// status); cheap and safe to call on every change.
func (self *DeviceLocal) SetPerformanceDegraded(degraded bool) {
	self.performanceDegraded.Store(degraded)
	self.stateLock.Lock()
	remoteUserNatClient := self.remoteUserNatClient
	self.stateLock.Unlock()
	if multi, ok := remoteUserNatClient.(*connect.RemoteUserNatMultiClient); ok {
		multi.SetPerformanceDegraded(degraded)
	}
}

func (self *DeviceLocal) NetworkChanged() {
	// kick every platform transport in the process (window clients + the
	// provider client); connections bound to the old path re-dial now
	connect.NetworkChanged()
	self.stateLock.Lock()
	upgradeMux := self.upgradeMux
	self.stateLock.Unlock()
	if upgradeMux != nil {
		upgradeMux.NetworkChanged()
	}
}

// GetFirstLoadTimelineJson returns the current connect's first-load timeline samples (dns
// query→answer, tcp/443 syn→synack and first payload byte for the first flows after connect)
// as json, for diagnostics. Empty when not connected. See connect.FirstLoadSample.
func (self *DeviceLocal) GetFirstLoadTimelineJson() string {
	self.stateLock.Lock()
	upgradeMux := self.upgradeMux
	self.stateLock.Unlock()
	if upgradeMux == nil {
		return ""
	}
	if samplesBytes, err := json.Marshal(upgradeMux.FirstLoadSamples()); err == nil {
		return string(samplesBytes)
	}
	return ""
}

func (self *DeviceLocal) SetDestination(location *ConnectLocation, specs *ProviderSpecList) {
	self.setDestination(location, specs, false)
}

// setDestination installs the destination. An unchanged destination normally
// keeps the live connection — the same transport, multi client and window —
// because it is re-applied implicitly all the time (the rpc replays pending
// state, and callers persist and restore it). `rebuild` is the explicit user
// action asking for a fresh connection anyway: it tears the transport down and
// builds a new one, so the same location gets a NEW multi client and a new set
// of peers. See `Device.Reconnect`.
func (self *DeviceLocal) setDestination(
	location *ConnectLocation,
	specs *ProviderSpecList,
	rebuild bool,
) {
	location = cloneConnectLocation(location)
	connectSpecs := []*connect.ProviderSpec{}
	if specs != nil {
		for i := 0; i < specs.Len(); i += 1 {
			if spec := specs.Get(i); spec != nil {
				connectSpecs = append(connectSpecs, spec.toConnectProviderSpec())
			}
		}
	}
	specsFingerprint := providerSpecsFingerprint(connectSpecs)

	provideChanged := false
	sameTransport := false
	locationChanged := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if !rebuild &&
			self.destinationInitialized &&
			self.destinationSpecsFingerprint == specsFingerprint &&
			connectLocationTransportEqual(self.connectLocation, location) {
			locationChanged = !connectLocationValuesEqual(self.connectLocation, location)
			self.connectLocation = location
			sameTransport = true
			return
		}

		self.destinationInitialized = true
		self.destinationSpecsFingerprint = specsFingerprint
		self.connectLocation = location

		if self.contractStatusSub != nil {
			self.contractStatusSub()
			self.contractStatusSub = nil
		}
		if self.windowMonitorSub != nil {
			self.windowMonitorSub()
			self.windowMonitorSub = nil
		}
		// the prior mux is closed here but its learned IP→hostname names are carried
		// into the new mux below (AdoptServerNames): server names outlive the physical
		// connection, so a reconnect/location change must not blank the reverse index —
		// and the block-action host feed — for flows the OS keeps open (or dials from its
		// long-TTL DNS cache) across the rebuild. Close() cancels the ctx but leaves the
		// reverse map intact for the adopt.
		var priorUpgradeMux *connect.UpgradeMux
		if self.upgradeMux != nil {
			priorUpgradeMux = self.upgradeMux
			// capture the mux's learned DoH server ordering before teardown: it seeds
			// the next mux this session and persists for the next session's first
			// lookups (see dohServerScoresSeed)
			self.saveDohServerScoresWithLock(priorUpgradeMux)
			self.upgradeMux.Close()
			self.upgradeMux = nil
		}
		self.closeRemoteUserNatClientWithLock()

		if 0 < len(connectSpecs) {
			// scope window identity persistence to this destination: identities
			// recorded under one connect's specs must never steer a connect to a
			// different destination (restored identities are dialed first)
			if self.windowIdentityStore != nil {
				self.windowIdentityStore.SetSpecsFingerprint(specsFingerprint)
			}

			remoteReceive := func(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte) {
				// self.log.Infof("[trace]receive packet\n")
				self.stats.UpdateRemoteReceive(ByteCount(len(packet)))
				self.receive(source, provideMode, ipPath, packet)
			}
			remoteReceivePackets := func(
				source connect.TransferPath,
				provideMode protocol.ProvideMode,
				ipPath *connect.IpPath,
				packets [][]byte,
			) {
				remoteReceiveByteCount := 0
				for _, packet := range packets {
					remoteReceiveByteCount += len(packet)
				}
				self.stats.UpdateRemoteReceive(ByteCount(remoteReceiveByteCount))
				self.receivePackets(source, provideMode, ipPath, packets)
			}
			networkPeerDestination := location != nil && location.NetworkPeer
			var networkPeerDestinationId *connect.Id
			if networkPeerDestination {
				for _, connectSpec := range connectSpecs {
					if connectSpec.ClientId != nil {
						peerId := *connectSpec.ClientId
						networkPeerDestinationId = &peerId
						break
					}
				}
			}
			webRtcReceiveBufferSize, webRtcMemoryBudget := deviceLocalDestinationWebRtcSettings(
				self.settings.WebRtcSettings,
				networkPeerDestination,
			)

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
				apiGeneratorSettings := connect.DefaultApiMultiClientGeneratorSettings()
				transportMode, modePreferences := toConnectTransportPolicy(self.transportSettings, false)
				apiGeneratorSettings.PlatformTransportMode = transportMode
				apiGeneratorSettings.PlatformTransportModePreferences = modePreferences
				apiGenerator := connect.NewApiMultiClientGenerator(
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
						// every window client's p2p admits against the ONE
						// dedicated device webRtc budget with the phone-sized
						// SCTP buffer — never the receive queue that active
						// transfer starves (PACKETRESEARCH1 §17)
						applyDeviceLocalDestinationWebRtcSettings(
							clientSettings.WebRtcSettings,
							networkPeerDestination,
							networkPeerDestinationId,
							webRtcReceiveBufferSize,
							webRtcMemoryBudget,
						)
						clientSettings.WebRtcSettings.UseEgressOnlyIceInterfaces =
							self.settings.WebRtcSettings.UseEgressOnlyIceInterfaces
						return clientSettings
					},
					apiGeneratorSettings,
				)
				// window identity persistence across a process restart, when the
				// embedding host provides a store (e.g. the proxy service,
				// PROXYDRAIN1.md §3.5)
				if self.settings.MultiClientIdentityStore != nil {
					apiGenerator.SetIdentityStore(self.settings.MultiClientIdentityStore)
				}
				self.apiMultiClientGenerator = apiGenerator
				generator = apiGenerator
			}
			settings := connect.DefaultMultiClientSettings()
			settings.Log = self.log
			settings.DefaultPerformanceProfile = toConnectPerformanceProfile(self.performanceProfile)
			// smart-routing tier (Phase 1): bake self.routingTier's knobs into
			// the constructed baseline so a fresh window -- the very first one
			// after a restart, or any later reconnect -- reflects the tier even
			// before SetRoutingTier is ever called again in this process. A
			// SetRoutingTier call while already connected pushes the same knobs
			// onto the CURRENT window immediately through SetReliabilitySettings
			// (see routing_tier.go); this is what the NEXT window is built
			// with. A developer-menu override set afterwards
			// (self.reliabilitySettings, re-applied below) supersedes this
			// baseline entirely, same as it already supersedes the six
			// always-on reliability fixes.
			applyRoutingTierToMultiClientSettings(settings, self.routingTier)
			// hosted hard limit: the hosted multi client must never allow
			// direct mode, superseding any performance profile and the
			// same-network force inside the multi client
			if self.settings.HostedIncompatible {
				overrideAllowDirect := false
				settings.OverrideAllowDirect = &overrideAllowDirect
			}
			if self.clientSecurityPolicyGenerator != nil {
				settings.SecurityPolicyGenerator = self.clientSecurityPolicyGenerator
			}
			// interpose the upgrade mux on the exit path: the multi-client delivers to
			// the mux's Receive (mux-addressed replies terminate on its internal stack,
			// the rest flow on to remoteReceive), and the send path runs through the mux
			// (it claims DNS/HTTP, else forwards to the multi-client).
			muxReceive := connect.ReceivePacketFunction(remoteReceive)
			muxReceivePackets := connect.ReceivePacketsFunction(remoteReceivePackets)
			var upgradeMux *connect.UpgradeMux
			if self.upgradeMuxSettings != nil {
				if self.upgradeMuxSettings.Dns != nil {
					// the device's dns byte budget bounds this mux's
					// resolvers (persistent across mux rebuilds)
					self.upgradeMuxSettings.Dns.MemoryTarget = self.dnsMemoryTarget
					// carry the learned DoH server ordering into the fresh resolvers
					self.upgradeMuxSettings.Dns.ServerStatsSeed = self.dohServerScoresSeed
				}
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
					// carry the prior mux's learned server names across the rebuild
					upgradeMux.AdoptServerNames(priorUpgradeMux)
					muxReceive = m.Receive
					muxReceivePackets = m.ReceivePackets
					m.AddPacketsReceiver(remoteReceivePackets)
				}
			}

			// A trusted same-network peer (the user tapped one of their own devices in
			// the peer list) egresses under ProvideMode_Network so the security
			// relationship is not downgraded to Public. This is explicit state from
			// the app (location.NetworkPeer), never inferred from the destination
			// shape — a fixed client id can also be a public exit.
			peerProvideMode := protocol.ProvideMode_Public
			if networkPeerDestination {
				peerProvideMode = protocol.ProvideMode_Network
			}
			multi := connect.NewRemoteUserNatMultiClient(
				self.ctx,
				generator,
				muxReceive,
				peerProvideMode,
				settings,
			)
			multi.SetReceivePacketsCallback(muxReceivePackets)
			// carry the host's degraded-performance state into the fresh window
			// (eases the liveness probe timings; see SetPerformanceDegraded)
			multi.SetPerformanceDegraded(self.performanceDegraded.Load())
			self.contractStatusSub = multi.AddContractStatusCallback(self.updateContractStatus)
			self.peerIdentitySub = multi.AddPeerIdentityChangeCallback(self.providerIdentitiesChanged)
			self.remoteUserNatClient = multi
			if upgradeMux != nil {
				upgradeMux.SetUpstreamBatchClient(multi)
				// the mux's DNS reverse index drives ServerName path affinity (point 4)
				multi.SetServerNameLookup(upgradeMux)
				// the mux blocks ad/tracker hostnames at the dns layer
				upgradeMux.SetBlocker(self.blocker)
				self.upgradeMux = upgradeMux
				// pre-warm the DoH connections in the background: the tunnel dials park
				// until the window can carry traffic and complete at tunnel-up, so the
				// first lookup rides an already-open connection (see UpgradeMux.WarmDns)
				upgradeMux.WarmDns()
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
				// expand-only events (nil providerEvents) cannot change the
				// connected provider set
				if reset || 0 < len(providerEvents) {
					self.connectedProviderLocationsChanged()
				}
			}
			self.windowMonitorSub = monitor.AddMonitorEventCallback(windowMonitorEvent)
			// }

			self.remoteUserNatClient.SetLocalSecurityBypass(self.routeLocal)

			// the multi client blocks ad/tracker ips and reverse-index
			// hostnames (the backstop under the mux's dns-layer blocking)
			multi.SetBlocker(self.blocker)

			multi.SetBlockActionOverrides(connectBlockActionOverrides(self.blockActionOverrides, self.settings.HostedIncompatible))
			// exclude the resolver endpoints from the override and association logic
			multi.SetBlockActionIgnoreHosts(dnsIgnoreHostValues(self.dnsResolverSettingsWithLock()))
			// re-apply the platform flow-owner resolver so per-app pinning
			// survives reconnects (SetFlowOwnerLookup stores it here for
			// exactly this moment)
			if self.flowOwnerLookup != nil {
				applyFlowOwnerLookup(multi, self.flowOwnerLookup)
			}
			// re-apply the runtime reliability override for the same reason:
			// the override lives on the multi client, and this is a fresh
			// one. Without this a dev override set before connecting never
			// takes effect, and one set while connected dies at the next
			// reconnect -- the "mechanism with no field-observable signal"
			// failure class (SetReliabilitySettings stores it here)
			if self.reliabilitySettings != nil {
				multi.SetReliabilitySettings(self.reliabilitySettings)
			}
			self.blockActionSub = multi.AddBlockActionCallback(self.updateBlockActions)
			self.packetStatsSub = multi.AddPacketStatsCallback(self.updatePacketStats)
			self.contractStatsEventSub = multi.AddContractStatsCallback(self.updateContractStatsEvents)

			if self.provideControlMode == ProvideControlModeAuto {
				provideChanged = self.setProvideModeWithLock(ProvideModePublic)
			}
		} else {
			// else no specs, not an error. Auto stops providing publicly on
			// disconnect but keeps Network provide, so the device stays a
			// reachable/discoverable peer.
			if self.provideControlMode == ProvideControlModeAuto {
				provideChanged = self.setProvideModeWithLock(ProvideModeNetwork)
			}
		}
		self.updateSendRouteWithLock()
	}()

	if sameTransport {
		if locationChanged {
			self.connectLocationChanged(location)
		}
		return
	}

	self.connectLocationChanged(self.GetConnectLocation())
	connectEnabled := self.GetConnectEnabled()
	self.stats.UpdateConnect(connectEnabled)
	self.connectChanged(connectEnabled)
	self.windowStatusChanged(self.GetWindowStatus())
	// the destination change replaced (or tore down) the multi client, so the
	// established provider identity set was reset. Fire once so consumers
	// re-read (and observe the empty set on disconnect)
	self.providerIdentitiesChanged()
	// same for the connected provider locations, which derive from the
	// replaced window monitor
	self.connectedProviderLocationsChanged()

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
		StallReason:  windowExpandEvent.Reason,
		Failed:       windowExpandEvent.Failed,
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
	self.setConnectLocation(location, false)
}

// Reconnect is `SetConnectLocation` for an explicit user action: it rebuilds
// the connection even when `location` is already the installed destination.
// See the `Device` interface.
func (self *DeviceLocal) Reconnect(location *ConnectLocation) {
	self.setConnectLocation(location, true)
}

func (self *DeviceLocal) setConnectLocation(location *ConnectLocation, rebuild bool) {
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
		self.setDestination(location, specs, rebuild)
	}
}

func (self *DeviceLocal) GetConnectLocation() *ConnectLocation {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return cloneConnectLocation(self.connectLocation)
}

func (self *DeviceLocal) GetDefaultLocation() *ConnectLocation {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return cloneConnectLocation(self.defaultLocation)
}

func (self *DeviceLocal) SetDefaultLocation(location *ConnectLocation) {
	location = cloneConnectLocation(location)
	changed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !connectLocationValuesEqual(self.defaultLocation, location) {
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

// RemoveConnectedProvider drops the provider from the connection window and
// excludes it from further discovery for the life of this connection. See the
// `Device` interface for the exclusion's lifetime.
func (self *DeviceLocal) RemoveConnectedProvider(clientId *Id) {
	if clientId == nil {
		return
	}

	var remoteUserNatClient connect.UserNatClient
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		remoteUserNatClient = self.remoteUserNatClient
	}()

	if multi, ok := remoteUserNatClient.(*connect.RemoteUserNatMultiClient); ok {
		if multi.RemoveProvider(clientId.toConnectId()) {
			// the window reaps the canceled client asynchronously; fire now so
			// the ui drops the row immediately rather than on the next event
			self.connectedProviderLocationsChanged()
		}
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

// Go embedders hand over a complete pooled burst. The device consumes every
// packet regardless of whether its selected route accepts it.
//
//gomobile:noexport
func (self *DeviceLocal) SendPacketsNoCopy(packets [][]byte) int {
	return self.sendPacketsNoCopy(packets)
}

// SendPacketBatch parses a compact sequence of uint16 length-prefixed packets
// and crosses the Go/native boundary once. Invalid framing is rejected before
// any packet is sent. The return value is the number accepted by the route.
func (self *DeviceLocal) SendPacketBatch(packetBatchBytes []byte) int32 {
	if devicePacketBatchMaxByteCount < len(packetBatchBytes) {
		return 0
	}
	var packetRanges [devicePacketBatchMaxPacketCount][2]int
	packetCount := 0
	offset := 0
	for offset < len(packetBatchBytes) {
		if len(packetBatchBytes)-offset < 2 || devicePacketBatchMaxPacketCount <= packetCount {
			return 0
		}
		packetByteCount := int(binary.BigEndian.Uint16(packetBatchBytes[offset : offset+2]))
		offset += 2
		if packetByteCount == 0 || len(packetBatchBytes)-offset < packetByteCount {
			return 0
		}
		packetRanges[packetCount] = [2]int{offset, offset + packetByteCount}
		packetCount += 1
		offset += packetByteCount
	}
	if packetCount == 0 {
		return 0
	}
	var packetStorage [devicePacketBatchMaxPacketCount][]byte
	packets := packetStorage[:packetCount]
	for packetIndex, packetRange := range packetRanges[:packetCount] {
		packets[packetIndex] = connect.MessagePoolCopy(
			packetBatchBytes[packetRange[0]:packetRange[1]],
		)
	}
	return int32(self.sendPacketsNoCopy(packets))
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

// The batch send owns every pooled packet on entry. Accepted packets transfer
// to the route; rejected packets are returned here. One immutable route and
// one stats update cover the whole burst.
func (self *DeviceLocal) sendPacketsNoCopy(packets [][]byte) int {
	route := self.sendRoute.Load()
	packetByteCount := 0
	for _, packet := range packets {
		packetByteCount += len(packet)
	}
	if route.upgradeMux != nil || route.remoteUserNatClient != nil {
		self.stats.UpdateRemoteSend(ByteCount(packetByteCount))
	}
	source := connect.SourceId(self.clientId)
	if route.upgradeMux != nil {
		return route.upgradeMux.SendPacketBatch(
			source,
			protocol.ProvideMode_Network,
			packets,
			self.settings.SendTimeout,
		)
	}
	if route.remoteUserNatClient != nil {
		if batchClient, ok := route.remoteUserNatClient.(connect.UserNatBatchClient); ok {
			return batchClient.SendPacketBatch(
				source,
				protocol.ProvideMode_Network,
				packets,
				self.settings.SendTimeout,
			)
		}
	}
	if route.routeLocal && route.provider != nil {
		if localUserNat := route.provider.LocalUserNat(); localUserNat != nil {
			sentPacketCount := localUserNat.SendPacketBatch(
				source,
				protocol.ProvideMode_Network,
				packets,
				self.settings.SendTimeout,
			)
			self.localFallbackEgressPacketCount.Add(int64(sentPacketCount))
			if sentPacketCount == len(packets) {
				self.localFallbackEgressByteCount.Add(int64(packetByteCount))
			}
			return sentPacketCount
		}
	}

	// Compatibility fallback for a custom UserNatClient that implements only
	// the original singular send contract. Production routes implement the
	// batch capability above.
	sentPacketCount := 0
	for _, packet := range packets {
		sent := false
		switch {
		case route.remoteUserNatClient != nil:
			sent = route.remoteUserNatClient.SendPacket(
				source,
				protocol.ProvideMode_Network,
				packet,
				self.settings.SendTimeout,
			)
		}
		if sent {
			sentPacketCount += 1
		} else {
			connect.MessagePoolReturn(packet)
		}
	}
	return sentPacketCount
}

// Registers one final native device-TUN injector. The callback is inline and
// borrowed; it may perform the synchronous final injection but must not wait
// on unrelated work or send back through the shared receive path.
func (self *DeviceLocal) AddReceivePacket(receivePacket ReceivePacket) Sub {
	receive := func(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte) {
		packetStorage := [1][]byte{packet}
		packetBatch := PacketBatch{packets: packetStorage[:]}
		ipVersion := packetBatch.IpVersion(0)
		ipProtocol := packetBatch.IpProtocol(0)
		if ipPath != nil {
			ipVersion = ipPath.Version
			switch ipPath.Protocol {
			case connect.IpProtocolUdp:
				ipProtocol = IpProtocolUdp
			case connect.IpProtocolTcp:
				ipProtocol = IpProtocolTcp
			default:
				ipProtocol = IpProtocolUnknown
			}
		}

		receivePacket.ReceivePacket(ipVersion, ipProtocol, packet)
	}
	callbackId := self.receiveCallbacks.Add(receive)
	return newSub(func() {
		self.receiveCallbacks.Remove(callbackId)
	})
}

// A mobile final-injection callback receives one borrowed packet object per
// upstream burst under the same narrow synchronous device-TUN contract.
func (self *DeviceLocal) AddReceivePackets(receivePackets ReceivePackets) Sub {
	receive := func(
		source connect.TransferPath,
		provideMode protocol.ProvideMode,
		ipPath *connect.IpPath,
		packets [][]byte,
	) {
		receivePackets.ReceivePackets(&PacketBatch{packets: packets})
	}
	callbackId := self.receivePacketsCallbacks.Add(receive)
	return newSub(func() {
		self.receivePacketsCallbacks.Remove(callbackId)
	})
}

// A native final-injection callback receives the whole borrowed burst in
// compact framing under the same narrow synchronous device-TUN contract.
func (self *DeviceLocal) AddReceivePacketBatch(receivePacketBatch ReceivePacketBatch) Sub {
	callbackId := self.receivePacketBatchCallbacks.Add(receivePacketBatch.ReceivePacketBatch)
	return newSub(func() {
		self.receivePacketBatchCallbacks.Remove(callbackId)
	})
}

//gomobile:noexport
func (self *DeviceLocal) AddReceivePacketCallback(callback func(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte)) func() {
	callbackId := self.receiveCallbacks.Add(callback)
	return func() {
		self.receiveCallbacks.Remove(callbackId)
	}
}

// Internal adapters avoid constructing a mobile PacketBatch wrapper.
//
//gomobile:noexport
func (self *DeviceLocal) AddReceivePacketsCallback(callback connect.ReceivePacketsFunction) func() {
	callbackId := self.receivePacketsCallbacks.Add(callback)
	return func() {
		self.receivePacketsCallbacks.Remove(callbackId)
	}
}

func (self *DeviceLocal) Cancel() {
	self.cancel()
}

func (self *DeviceLocal) Close() {
	self.closeOnce.Do(self.close)
}

func (self *DeviceLocal) close() {
	// Controllers can hold device listeners and window monitor callbacks.
	// Release them before taking the device state lock so their transitive
	// Close methods can safely call back into the device.
	self.viewControllerManager.Close()

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
		// persist the learned DoH server ordering for the next session
		self.saveDohServerScoresWithLock(self.upgradeMux)
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

// Sentinel for a jwt with NO client_id claim at all (a network member jwt).
// Callers that accept a network jwt (NewPlatformDeviceRemote) treat exactly
// this case as non-fatal; a PRESENT-but-malformed claim still fails loudly.
var errByJwtNoClientId = fmt.Errorf("byJwt does not contain claim client_id")

func parseByJwtClientId(byJwt string) (connect.Id, error) {
	claims := gojwt.MapClaims{}
	gojwt.NewParser().ParseUnverified(byJwt, claims)

	jwtClientId, ok := claims["client_id"]
	if !ok {
		return connect.Id{}, errByJwtNoClientId
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
	self.apiMultiClientGenerator = nil
	if self.blockActionSub != nil {
		self.blockActionSub()
		self.blockActionSub = nil
	}
	if self.peerIdentitySub != nil {
		self.peerIdentitySub()
		self.peerIdentitySub = nil
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

// SetTransportSettings applies the client carrier policy to future windows and
// make-before-break migrates every live built-in window. Custom generators do
// not expose a transport seam, so they receive the persisted policy only when
// their owner chooses to consume it.
func (self *DeviceLocal) SetTransportSettings(transportSettings *TransportSettings) {
	if self.hostedIncompatibleGuarded("SetTransportSettings") {
		return
	}
	transportSettings = normalizeTransportSettings(transportSettings, false)
	var apiGenerator *connect.ApiMultiClientGenerator
	changed := false
	self.stateLock.Lock()
	if !transportSettingsEqual(self.transportSettings, transportSettings, false) {
		self.transportSettings = cloneTransportSettings(transportSettings)
		apiGenerator = self.apiMultiClientGenerator
		changed = true
	}
	self.stateLock.Unlock()
	if !changed {
		return
	}
	if apiGenerator != nil {
		mode, preferences := toConnectTransportPolicy(transportSettings, false)
		apiGenerator.SetPlatformTransportPolicy(mode, preferences)
	}
	if asyncLocalState := self.networkSpace.GetAsyncLocalState(); asyncLocalState != nil {
		if err := asyncLocalState.GetLocalState().SetTransportSettings(transportSettings); err != nil {
			self.log.Errorf("failed to persist transport settings: %v", err)
		}
	}
	self.transportSettingsChanged(transportSettings)
	self.transportStatusChanged(self.GetTransportStatus())
}

func (self *DeviceLocal) GetTransportSettings() *TransportSettings {
	if self.settings.HostedIncompatible {
		return hostedTransportSettings()
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return cloneTransportSettings(self.transportSettings)
}

func (self *DeviceLocal) AddTransportSettingsChangeListener(listener TransportSettingsChangeListener) Sub {
	if self.transportSettingsChangeListeners == nil {
		self.transportSettingsChangeListeners = connect.NewCallbackList[TransportSettingsChangeListener]()
	}
	callbackId := self.transportSettingsChangeListeners.Add(listener)
	return newSub(func() {
		self.transportSettingsChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) transportSettingsChanged(transportSettings *TransportSettings) {
	if self.transportSettingsChangeListeners == nil {
		return
	}
	for _, listener := range self.transportSettingsChangeListeners.Get() {
		connect.HandleError(func() {
			listener.TransportSettingsChanged(cloneTransportSettings(transportSettings))
		})
	}
}

func (self *DeviceLocal) GetTransportStatus() *TransportStatus {
	return transportStatus(self.GetTransportSettings(), false)
}

func (self *DeviceLocal) AddTransportStatusChangeListener(listener TransportStatusChangeListener) Sub {
	if self.transportStatusChangeListeners == nil {
		self.transportStatusChangeListeners = connect.NewCallbackList[TransportStatusChangeListener]()
	}
	callbackId := self.transportStatusChangeListeners.Add(listener)
	return newSub(func() {
		self.transportStatusChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) transportStatusChanged(status *TransportStatus) {
	if self.transportStatusChangeListeners == nil {
		return
	}
	for _, listener := range self.transportStatusChangeListeners.Get() {
		connect.HandleError(func() {
			listener.TransportStatusChanged(cloneTransportStatus(status))
		})
	}
}

// SetProviderTransportSettings applies the provider carrier policy through the
// provider's make-before-break transport replacement.
func (self *DeviceLocal) SetProviderTransportSettings(transportSettings *TransportSettings) {
	if self.hostedIncompatibleGuarded("SetProviderTransportSettings") {
		return
	}
	transportSettings = normalizeTransportSettings(transportSettings, true)
	var provider *deviceLocalProvider
	changed := false
	self.stateLock.Lock()
	if !transportSettingsEqual(self.providerTransportSettings, transportSettings, true) {
		self.providerTransportSettings = cloneTransportSettings(transportSettings)
		provider = self.provider
		changed = true
	}
	self.stateLock.Unlock()
	if !changed {
		return
	}
	if provider != nil {
		mode, preferences := toConnectTransportPolicy(transportSettings, true)
		provider.SetTransportPolicy(mode, preferences)
	}
	if asyncLocalState := self.networkSpace.GetAsyncLocalState(); asyncLocalState != nil {
		if err := asyncLocalState.GetLocalState().SetProviderTransportSettings(transportSettings); err != nil {
			self.log.Errorf("failed to persist provider transport settings: %v", err)
		}
	}
	self.providerTransportSettingsChanged(transportSettings)
	self.providerTransportStatusChanged(self.GetProviderTransportStatus())
}

func (self *DeviceLocal) AddProviderTransportSettingsChangeListener(listener ProviderTransportSettingsChangeListener) Sub {
	if self.providerTransportSettingsChangeListeners == nil {
		self.providerTransportSettingsChangeListeners = connect.NewCallbackList[ProviderTransportSettingsChangeListener]()
	}
	callbackId := self.providerTransportSettingsChangeListeners.Add(listener)
	return newSub(func() {
		self.providerTransportSettingsChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) providerTransportSettingsChanged(transportSettings *TransportSettings) {
	if self.providerTransportSettingsChangeListeners == nil {
		return
	}
	for _, listener := range self.providerTransportSettingsChangeListeners.Get() {
		connect.HandleError(func() {
			listener.ProviderTransportSettingsChanged(cloneTransportSettings(transportSettings))
		})
	}
}

func (self *DeviceLocal) GetProviderTransportSettings() *TransportSettings {
	if self.settings.HostedIncompatible {
		return hostedTransportSettings()
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return cloneTransportSettings(self.providerTransportSettings)
}

func (self *DeviceLocal) GetProviderTransportStatus() *TransportStatus {
	return transportStatus(self.GetProviderTransportSettings(), true)
}

func (self *DeviceLocal) AddProviderTransportStatusChangeListener(listener ProviderTransportStatusChangeListener) Sub {
	if self.providerTransportStatusChangeListeners == nil {
		self.providerTransportStatusChangeListeners = connect.NewCallbackList[ProviderTransportStatusChangeListener]()
	}
	callbackId := self.providerTransportStatusChangeListeners.Add(listener)
	return newSub(func() {
		self.providerTransportStatusChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) providerTransportStatusChanged(status *TransportStatus) {
	if self.providerTransportStatusChangeListeners == nil {
		return
	}
	for _, listener := range self.providerTransportStatusChangeListeners.Get() {
		connect.HandleError(func() {
			listener.ProviderTransportStatusChanged(cloneTransportStatus(status))
		})
	}
}

func addConnectPacketStats(out *connect.PacketStats, add *connect.PacketStats) {
	if add == nil {
		return
	}
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
	transportStats := map[connect.TransportType]*connect.PacketStats{}
	for transportType, stats := range out.TransportStats {
		if stats == nil {
			continue
		}
		statsCopy := *stats
		statsCopy.TransportStats = nil
		transportStats[transportType] = &statsCopy
	}
	for transportType, stats := range add.TransportStats {
		if stats == nil {
			continue
		}
		combined := transportStats[transportType]
		if combined == nil {
			combined = &connect.PacketStats{}
			transportStats[transportType] = combined
		}
		combined.RemoteEgressPacketCount += stats.RemoteEgressPacketCount
		combined.RemoteEgressByteCount += stats.RemoteEgressByteCount
		combined.RemoteIngressPacketCount += stats.RemoteIngressPacketCount
		combined.RemoteIngressByteCount += stats.RemoteIngressByteCount
	}
	out.TransportStats = transportStats
}

// overrides with no hosts (app id only) are applied by the platform,
// not the packet path.
//
// hostedIncompatible forces every RouteOverride to remote (Local=false). A
// hosted (platform-embedded, e.g. cloud proxy) device must never route any
// traffic locally: local egress leaves the host's real network interface,
// reaching the datacenter LAN, loopback, and the metadata endpoint. A local
// route override is the one client-reachable way to reach the multi client's
// LocalUserNat, so it is neutralized here — the single translation point every
// override source (rpc, sync-replay, persisted state, config) passes through on
// its way to the live client. Block overrides are unaffected.
func connectBlockActionOverrides(overrides []*BlockActionOverride, hostedIncompatible bool) []*connect.BlockActionOverride {
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
			local := override.RouteOverride.Local && !hostedIncompatible
			connectOverride.RouteOverride = &connect.RouteOverride{
				Local: local,
				Pin:   override.RouteOverride.Pin,
			}
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

func packetStatsFlatFromConnect(packetStats *connect.PacketStats) *PacketStats {
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

func transportTypeFromConnect(transportType connect.TransportType) TransportType {
	switch transportType {
	case connect.TransportTypeH3:
		return TransportTypeH3
	case connect.TransportTypeH1:
		return TransportTypeH1
	case connect.TransportTypeH3Dns:
		return TransportTypeDns
	case connect.TransportTypeH3DnsPump:
		return TransportTypeDnsPump
	case connect.TransportTypeP2p:
		return TransportTypeP2p
	default:
		return TransportTypeUnknown
	}
}

func packetStatsFromConnect(packetStats *connect.PacketStats) *PacketStats {
	stats := packetStatsFlatFromConnect(packetStats)
	stats.TransportStats = NewTransportPacketStatsList()
	for _, connectTransportType := range connect.TransportTypes() {
		transportStats := packetStats.TransportStats[connectTransportType]
		if transportStats == nil {
			transportStats = &connect.PacketStats{}
		}
		stats.TransportStats.Add(&TransportPacketStats{
			TransportType: transportTypeFromConnect(connectTransportType),
			Stats:         packetStatsFlatFromConnect(transportStats),
		})
	}
	return stats
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
	// the exact ips/hosts an override matched, disjoint from ips/hosts above
	matchedIps := NewStringList()
	for _, ip := range blockAction.MatchedIps {
		matchedIps.Add(ip.String())
	}
	matchedHosts := NewStringList()
	matchedHosts.addAll(blockAction.MatchedHosts...)
	out := &BlockAction{
		BlockActionId: NewId(),
		Time:          blockAction.Time.UnixMilli(),
		Ips:           ips,
		Hosts:         hosts,
		MatchedIps:    matchedIps,
		MatchedHosts:  matchedHosts,
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
			out.RouteOverride = &RouteOverride{
				Local: override.RouteOverride.Local,
				Pin:   override.RouteOverride.Pin,
			}
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
		multi.SetBlockActionOverrides(connectBlockActionOverrides(self.blockActionOverrides, self.settings.HostedIncompatible))
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

// hostedSafeBlockActionOverride returns the override to store on this device. On a
// hosted (cloud proxy) device a local route override is neutralized to remote
// routing (Local=false), so neither the stored nor the applied state can ever
// route locally; block overrides are unaffected. Non-hosted devices store the
// override unchanged. This makes the stored state truthful; connectBlockActionOverrides
// applies the same strip as a backstop for paths that do not go through the setters
// (persisted-state load, multi client re-create).
func (self *DeviceLocal) hostedSafeBlockActionOverride(override *BlockActionOverride) *BlockActionOverride {
	if override == nil || !self.settings.HostedIncompatible {
		return override
	}
	if override.RouteOverride != nil && override.RouteOverride.Local {
		safe := *override
		// only the local half is unsafe on a hosted device; a pin is exit
		// placement and must survive the rewrite
		safe.RouteOverride = &RouteOverride{Local: false, Pin: override.RouteOverride.Pin}
		return &safe
	}
	return override
}

func (self *DeviceLocal) AddBlockActionOverride(override *BlockActionOverride) {
	if override == nil || override.OverrideId == nil {
		return
	}
	override = self.hostedSafeBlockActionOverride(override)
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
				override = self.hostedSafeBlockActionOverride(override)
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
		if override.RouteOverride.Pin && !override.RouteOverride.Local {
			// a pin rule holds the app to one exit INSIDE the tunnel; it is
			// not a tunnel-membership rule. Counting it here would flip the
			// vpn into allowlist mode and route ONLY the pinned apps -- the
			// exact opposite of what pinning means.
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

// GetPinnedAppIds lists the app ids held to one exit by pin rules -- the set
// the platform's FlowOwnerLookup implementation answers for.
func (self *DeviceLocal) GetPinnedAppIds() *StringList {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	pinned := NewStringList()
	seen := map[string]bool{}
	for _, override := range self.blockActionOverrides {
		if override.RouteOverride == nil || !override.RouteOverride.Pin || override.AppIds == nil {
			continue
		}
		if override.RouteOverride.Local {
			// a local-routed app bypasses the tunnel entirely; there is no
			// exit to pin it to, and reporting it pinned would put a
			// bypassing app in the platform's allow-list union
			continue
		}
		for _, appId := range override.AppIds.getAll() {
			if !seen[appId] {
				seen[appId] = true
				pinned.Add(appId)
			}
		}
	}
	return pinned
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
	usedByteCount     ByteCount
	transferByteCount ByteCount
	bitRate           int
	open              bool
	updateTime        time.Time
	// a closed contract is reported once as a Closed tombstone, then evicted on the
	// next emit; this flags that its one report has been made.
	closedReported bool
}

// the open contracts of one connect client (the multi client or the provider
// client) and the listener dispatch gate.
// all methods must be called with the device `stateLock`
type deviceContractTracker struct {
	// egress (send) and ingress (receive) contracts, keyed by contract id
	egressContracts  map[connect.Id]*deviceContractState
	ingressContracts map[connect.Id]*deviceContractState
	// the highest consumed `ContractStatsEvent.Sequence` per contract id.
	// the producer assigns strictly increasing sequences per contract id
	// (the send and receive entries of a contract share one counter), so an
	// event at or below the recorded high-water mark is a reordered or
	// replayed delivery and is discarded — most importantly a stale
	// `Open=true` arriving after the final close, which must not resurrect
	// the contract. an entry is dropped once the contract's last state entry
	// is evicted, mirroring the producer's counter lifecycle (the producer
	// resets only after the final `Open=false`, so any post-reset straggler
	// is discarded here as stale while the tombstone is still tracked)
	lastSeenSequences map[connect.Id]uint64
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
		egressContracts:   map[connect.Id]*deviceContractState{},
		ingressContracts:  map[connect.Id]*deviceContractState{},
		lastSeenSequences: map[connect.Id]uint64{},
	}
}

func (self *deviceContractTracker) clear() {
	clear(self.egressContracts)
	clear(self.ingressContracts)
	// the torn-down client's contract ids are single-use and never reappear;
	// keeping their high-water marks would only leak
	clear(self.lastSeenSequences)
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
		if 0 < event.Sequence {
			// the producer's per-contract sequence contract: discard an
			// event at or below the last consumed sequence — a reordered or
			// replayed delivery (callbacks run outside the producer lock and
			// may further reorder across an rpc boundary). in particular a
			// stale `Open=true` delivered after the final close must be
			// ignored instead of resurrecting the contract.
			// (emitted events never carry Sequence 0; 0 means an
			// unsequenced source and bypasses the filter)
			if event.Sequence <= self.lastSeenSequences[event.ContractId] {
				continue
			}
			self.lastSeenSequences[event.ContractId] = event.Sequence
		}
		contracts := self.egressContracts
		if event.Receive {
			contracts = self.ingressContracts
		}
		state, ok := contracts[event.ContractId]
		if !ok {
			state = &deviceContractState{
				contractId: event.ContractId,
				path:       event.Path,
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

// emit builds the reports and advances the contract lifecycle: a contract that
// closed is reported once as a Closed tombstone carrying its final byte counts,
// kept one more cycle so a coalesced getter read still sees it, then evicted on
// the next emit.
func (self *deviceContractTracker) emit(now time.Time) (egressStats *ContractStats, ingressStats *ContractStats, egressDetails *ContractDetailsList, ingressDetails *ContractDetailsList) {
	self.pendingEmit = false
	self.lastEmitTime = now

	// drop tombstones whose one Closed report was made on a previous cycle
	self.evictReportedClosed(self.egressContracts, self.ingressContracts)
	self.evictReportedClosed(self.ingressContracts, self.egressContracts)

	egressStats = self.stats(false)
	ingressStats = self.stats(true)
	egressDetails = self.details(false)
	ingressDetails = self.details(true)

	// flag the Closed tombstones just reported; keep the settle loop alive while
	// any tombstone remains so the next emit evicts it. Both directions must be
	// finalized -- `a() || b()` would short-circuit past the ingress tombstones.
	egressRemaining := self.finalizeReported(self.egressContracts)
	ingressRemaining := self.finalizeReported(self.ingressContracts)
	if egressRemaining || ingressRemaining {
		self.pendingEmit = true
	}
	return
}

// evictReportedClosed removes closed contracts whose Closed tombstone was already
// emitted on a previous cycle. once no entry remains for a contract id in either
// direction, its sequence high-water mark is dropped with it (mirroring the
// producer, whose per-contract counter is dropped when its last entry closes),
// keeping `lastSeenSequences` bounded by the tracked contracts.
func (self *deviceContractTracker) evictReportedClosed(contracts map[connect.Id]*deviceContractState, siblingContracts map[connect.Id]*deviceContractState) {
	for contractId, state := range contracts {
		if !state.open && state.closedReported {
			delete(contracts, contractId)
			if _, ok := siblingContracts[contractId]; !ok {
				delete(self.lastSeenSequences, contractId)
			}
		}
	}
}

// finalizeReported flags each just-reported Closed tombstone (evicted next
// emit). Returns whether any tombstone remains pending eviction.
func (self *deviceContractTracker) finalizeReported(contracts map[connect.Id]*deviceContractState) (remaining bool) {
	for _, state := range contracts {
		if !state.open {
			state.closedReported = true
			remaining = true
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

// one entry per contract of the direction. Contracts are never paired with the
// opposite direction -- send and receive contracts are many-to-many per peer.
func (self *deviceContractTracker) details(receive bool) *ContractDetailsList {
	own := self.egressContracts
	if receive {
		own = self.ingressContracts
	}
	details := NewContractDetailsList()
	for _, state := range own {
		status := ContractStatusOpen
		if !state.open {
			status = ContractStatusClosed
		}
		details.Add(&ContractDetails{
			ContractId:            newId(state.contractId),
			ContractUsedByteCount: state.usedByteCount,
			ContractByteCount:     state.transferByteCount,
			ContractBitRate:       state.bitRate,
			ContractTransferPath:  fromConnect(state.path),
			Status:                status,
		})
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
		EnableRemoteDoh:       resolver.EnableRemoteDoh,
		EnableLocalDoh:        resolver.EnableLocalDoh,
		EnableRemoteDns:       resolver.EnableRemoteDns,
		EnableLocalDns:        resolver.EnableLocalDns,
		DnsUpgradeMaskAddress: dnsUpgradeMaskAddress(resolver.DnsUpgradeMaskAddress),
		RemoteDohUrlsIpv4:     stringListOf(resolver.RemoteDohUrlsIpv4),
		RemoteDohUrlsIpv6:     stringListOf(resolver.RemoteDohUrlsIpv6),
		LocalDohUrlsIpv4:      stringListOf(resolver.LocalDohUrlsIpv4),
		LocalDohUrlsIpv6:      stringListOf(resolver.LocalDohUrlsIpv6),
		RemoteDnsIpv4:         stringListOf(resolver.RemoteDnsIpv4),
		RemoteDnsIpv6:         stringListOf(resolver.RemoteDnsIpv6),
		LocalDnsIpv4:          stringListOf(resolver.LocalDnsIpv4),
		LocalDnsIpv6:          stringListOf(resolver.LocalDnsIpv6),
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
		EnableRemoteDoh:       self.EnableRemoteDoh,
		EnableLocalDoh:        self.EnableLocalDoh,
		EnableRemoteDns:       self.EnableRemoteDns,
		EnableLocalDns:        self.EnableLocalDns,
		DnsUpgradeMaskAddress: dnsUpgradeMaskAddress(self.DnsUpgradeMaskAddress),
		RemoteDohUrlsIpv4:     stringsOf(self.RemoteDohUrlsIpv4),
		RemoteDohUrlsIpv6:     stringsOf(self.RemoteDohUrlsIpv6),
		LocalDohUrlsIpv4:      stringsOf(self.LocalDohUrlsIpv4),
		LocalDohUrlsIpv6:      stringsOf(self.LocalDohUrlsIpv6),
		RemoteDnsIpv4:         stringsOf(self.RemoteDnsIpv4),
		RemoteDnsIpv6:         stringsOf(self.RemoteDnsIpv6),
		LocalDnsIpv4:          stringsOf(self.LocalDnsIpv4),
		LocalDnsIpv6:          stringsOf(self.LocalDnsIpv6),
	}
}

// dnsUpgradeMaskAddress upgrades settings persisted by older SDKs, which did
// not carry the mask field. An empty value therefore means the safe default,
// not that the mux stand-in is disabled.
func dnsUpgradeMaskAddress(value string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return connect.DefaultDnsUpgradeMaskAddress
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
	// The caller owns this mutable gomobile settings object and may reuse it as
	// soon as this method returns. Snapshot it before handing it to the async
	// local-state writer; otherwise a UI edit immediately following Set can race
	// JSON serialization (and persist a torn combination of toggles/addresses).
	dnsResolverSettings = cloneDnsResolverSettings(dnsResolverSettings)
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

func cloneDnsResolverSettings(dnsResolverSettings *DnsResolverSettings) *DnsResolverSettings {
	if dnsResolverSettings == nil {
		return nil
	}
	cloned := dnsResolverSettingsFromConnect(dnsResolverSettings.toConnect())
	cloned.EnableFallback = dnsResolverSettings.EnableFallback
	return cloned
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
	add(dnsUpgradeMaskAddress(dnsResolverSettings.DnsUpgradeMaskAddress))
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
		// contain panics to the tick: a failed snapshot or emit must never kill
		// the watch loop — the device would silently stop receiving peer
		// updates for the life of the tunnel (the same failure class as the
		// server-side peer listener death fixed 2026-07-15)
		connect.HandleError(func() {
			self.networkPeersChanged(self.GetNetworkPeers())
		})
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
		// auto or unset: no fixed window type
		connectWindowType = connect.WindowTypeAuto
	}
	p := &connect.PerformanceProfile{
		WindowType:            connectWindowType,
		WindowSize:            toConnectWindowSize(performanceProfile.WindowSize),
		AllowDirect:           performanceProfile.AllowDirect,
		PostQuantumEncryption: performanceProfile.PostQuantumEncryption,
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
