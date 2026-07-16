package sdk

import (
	"github.com/urnetwork/connect/v2026"
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

type ProvideModeChangeListener interface {
	ProvideModeChanged(provideMode ProvideMode)
}

type ProvidePausedChangeListener interface {
	ProvidePausedChanged(providePaused bool)
}

type ProvideNetworkModeChangeListener interface {
	ProvideNetworkModeChanged(provideNetworkMode ProvideNetworkMode)
}

type ProvideControlModeChangeListener interface {
	ProvideControlModeChanged(provideControlMode ProvideControlMode)
}

type PerformanceProfileChangeListener interface {
	PerformanceProfileChanged(performanceProfile *PerformanceProfile)
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

type BlockerEnabledChangeListener interface {
	BlockerEnabledChanged(blockerEnabled bool)
}

type VpnInterfaceWhileOfflineChangeListener interface {
	VpnInterfaceWhileOfflineChanged(vpnInterfaceWhileOffline bool)
}

type ConnectLocationChangeListener interface {
	ConnectLocationChanged(location *ConnectLocation)
}

type DefaultLocationChangeListener interface {
	DefaultLocationChanged(location *ConnectLocation)
}

type CanShowRatingDialogChangeListener interface {
	CanShowRatingDialogChanged(canShowRatingDialog bool)
}

type CanPromptIntroFunnelChangeListener interface {
	CanPromptIntroFunnelChanged(canPromptIntroFunnel bool)
}

type AllowForegroundChangeListener interface {
	AllowForegroundChanged(allowForeground bool)
}

type CanReferChangeListener interface {
	CanReferChanged(canRefer bool)
}

// FIXME rename to ProvideSecretKeysChangeListener
type ProvideSecretKeysListener interface {
	ProvideSecretKeysChanged(provideSecretKeyList *ProvideSecretKeyList)
}

type BlockActionWindowChangeListener interface {
	BlockActionWindowChanged(blockActionWindow *BlockActionWindow)
}

type BlockActionOverridesChangeListener interface {
	BlockActionOverridesChanged(blockActionOverrides *BlockActionOverrideList)
}

type BlockStatsChangeListener interface {
	BlockStatsChanged(blockStats *BlockStats)
}

type ContractStatsChangeListener interface {
	ContractStatsChanged(contractStats *ContractStats)
}

type ContractDetailsChangeListener interface {
	ContractDetailsChanged(contractDetails *ContractDetails)
}

type DnsResolverSettingsChangeListener interface {
	DnsResolverSettingsChanged(dnsResolverSettings *DnsResolverSettings)
}

type PacketStatsChangeListener interface {
	PacketStatsChanged(packetStats *PacketStats)
}

type NetworkPeersChangeListener interface {
	NetworkPeersChanged(networkPeers *NetworkPeers)
}

// receive a packet into the local raw socket
type ReceivePacket interface {
	ReceivePacket(ipVersion int, ipProtocol IpProtocol, packet []byte)
}

type TunnelChangeListener interface {
	TunnelChanged(tunnelStarted bool)
}

type ContractStatusChangeListener interface {
	ContractStatusChanged(contractStatus *ContractStatus)
}

type WindowStatusChangeListener interface {
	WindowStatusChanged(windowStatus *WindowStatus)
}

type JwtRefreshListener interface {
	// JWT refresh is intentionally handled by each Device implementation.
	// DeviceRemote refreshes on the remote/app side, so this listener is not
	// bridged over DeviceLocal RPC like durable device-state listeners.
	// JwtRefreshed(jwt *ByJwt)
	JwtRefreshed(jwt string)
}

type AuthLogoutListener interface {
	// fired when the stored auth is no longer valid on the server (for
	// example the jwt's client was removed) and the local auth state has
	// been cleared. The app must treat this as a logout: drop the device
	// and return to the login flow.
	AuthLogout()
}

type IpProtocol = int

const (
	IpProtocolUnknown IpProtocol = 0
	IpProtocolUdp     IpProtocol = 1
	IpProtocolTcp     IpProtocol = 2
)

type ContractStatus struct {
	InsufficientBalance bool
	NoPermission        bool
	Premium             bool
}

type BlockStats struct {
	AllowedCount int
	BlockedCount int
}

type BlockActionWindow struct {
	BlockActions *BlockActionList
}

type BlockActionOverride struct {
	// this is set by the caller and must be ensured unique by the caller
	OverrideId *Id
	// hosts mean the traffic overlaps any of the hosts
	// a host can be 1. an exact hostname 2. a wildcard host name (*.H matches subdomains of H,
	// **.H matches H and subdomains) 3. an ipv4 or ipv6 subnet 4. an ipv4 or ipv6
	Hosts *StringList
	// on android these are package names
	AppIds        *StringList
	BlockOverride *BlockOverride
	RouteOverride *RouteOverride
}

type OverrideLocalAppIds struct {
	Included *StringList
	Excluded *StringList
}

type BlockOverride struct {
	Block bool
}

type RouteOverride struct {
	Local bool
}

// a routing decision, aggregated per cluster per event epoch.
// decisions are made on the cluster level (destinations that activity-associate
// share the decision), so one action carries all the ips and hosts in the cluster
type BlockAction struct {
	BlockActionId *Id
	Time          int64
	// all ips in the cluster
	Ips *StringList
	// all hosts observed for the cluster ips
	Hosts *StringList
	Block bool
	Local bool
	// the applied overrides, when an override determined the decision.
	// `OverrideId` is the deciding override (see `BlockActionOverride.OverrideId`);
	// the block override's id when the block and route decisions came from
	// different overrides. the override may have been removed since the decision
	OverrideId    *Id
	BlockOverride *BlockOverride
	RouteOverride *RouteOverride
	PacketCount   int
	ByteCount     ByteCount
}

// cumulative packet counts by route.
// remote is traffic egressed to providers. local is traffic routed
// to the local user nat (route local and security bypass).
// block counts traffic dropped by the security rules, split by direction:
// egress is blocked on the way out, ingress on the way in
type PacketStats struct {
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
}

type ContractStats struct {
	ContractUsedByteCount ByteCount
	ContractByteCount     ByteCount
	ContractBitRate       int

	CompanionContractUsedByteCount ByteCount
	CompanionContractByteCount     ByteCount
	CompanionContractBitRate       int
}

/*
	contract
	  |   ^

contract |   | companion contract
transfer |   | transfer
path     |   | path

	  ⌄   |
	companion contract
*/
// Contract lifecycle status carried on ContractDetails.Status.
//
// A contract is Open while active. When it closes it is reported ONE more time
// with Closed -- a one-shot tombstone -- so the UI can animate it leaving, then
// it is evicted. An atomic replacement (a renewed contract taking over the same
// transfer path) is not a separate close+open that would make the UI bounce:
// the incoming contract is Open with ReplacesContractId set to the contract it
// superseded, and the superseded contract is dropped without its own Closed
// tombstone (its close is implied by the replacement). A plain close is just a
// replacement by nothing (A -> nil); a plain open is a replacement of nothing
// (nil -> B).
const (
	ContractStatusOpen   = "open"
	ContractStatusClosed = "closed"
)

type ContractDetails struct {
	ContractId            *Id
	ContractUsedByteCount ByteCount
	ContractByteCount     ByteCount
	ContractBitRate       int
	ContractTransferPath  *TransferPath

	CompanionContractId            *Id
	CompanionContractUsedByteCount ByteCount
	CompanionContractByteCount     ByteCount
	CompanionContractBitRate       int
	CompanionContractTransferPath  *TransferPath

	// ContractStatusOpen or ContractStatusClosed. Closed appears once (a
	// tombstone) then the contract is evicted.
	Status string
	// set on an Open contract that atomically replaced another on the same
	// transfer path (a renewal): the id of the contract it superseded, so the UI
	// ejects the old and fades in the new as one transition. nil for a fresh open.
	ReplacesContractId *Id
}

type WindowStatus struct {
	TargetSize                    int
	MinSatisfied                  bool
	ProviderStateInEvaluation     int
	ProviderStateEvaluationFailed int
	ProviderStateNotAdded         int
	ProviderStateAdded            int
	ProviderStateRemoved          int
}

type NetworkPeers struct {
	Connected         *NetworkPeerList
	DisconnectedCount int
}

type NetworkPeer struct {
	ClientId       *Id
	ProvideEnabled bool
	Principal      string
	Roles          *StringList
	DeviceSpec     string
	DeviceName     string
}

// DnsResolverSettings is the gomobile-friendly surface for how the device resolves
// DNS — plain DNS and/or DoH, against local and/or remote servers. It mirrors the
// resolver configuration in the connect package; the string lists hold resolver IPs
// and DoH endpoint URLs. TLS/cert-pinning is applied internally and not exposed here.
type DnsResolverSettings struct {
	EnableRemoteDoh bool
	EnableLocalDoh  bool
	EnableRemoteDns bool
	EnableLocalDns  bool
	// EnableFallback races a handicapped resolver over the local host network
	// when the tunnel resolver is slow, bridging tunnel startup. The fallback
	// servers are derived as the host-side projection of the resolver settings
	// above. When false, DNS only ever resolves through the tunnel
	EnableFallback bool

	RemoteDohUrlsIpv4 *StringList
	RemoteDohUrlsIpv6 *StringList
	LocalDohUrlsIpv4  *StringList
	LocalDohUrlsIpv6  *StringList
	RemoteDnsIpv4     *StringList
	RemoteDnsIpv6     *StringList
	LocalDnsIpv4      *StringList
	LocalDnsIpv6      *StringList
}

// a well known regional dns server, associated to a country code.
// these are suggestions the ui can surface when connected to the region
type RegionalDnsServer struct {
	CountryCode string
	Name        string
	Ipv4        string
}

// GetRegionalDnsServers enumerates the well known regional dns servers
func GetRegionalDnsServers() *RegionalDnsServerList {
	servers := NewRegionalDnsServerList()
	for _, server := range connect.RegionalDnsServers() {
		servers.Add(&RegionalDnsServer{
			CountryCode: server.CountryCode,
			Name:        server.Name,
			Ipv4:        server.Ipv4,
		})
	}
	return servers
}

// HasRegionalDnsRecommendation reports whether the country has a recommended
// dns configuration (the strong-privacy defaults are known not to work there)
func HasRegionalDnsRecommendation(countryCode string) bool {
	return 0 < len(connect.RegionalDnsServerIps(countryCode))
}

// GetRecommendedDnsResolverSettings returns the recommended dns settings for a
// country (unencrypted remote dns only, using the region's known-working
// servers), or nil when there is no recommendation. shares the connect
// recommendation used by the server proxy config
func GetRecommendedDnsResolverSettings(countryCode string) *DnsResolverSettings {
	resolver := connect.RegionalDnsResolverSettings(countryCode)
	if resolver == nil {
		return nil
	}
	return dnsResolverSettingsFromConnect(resolver)
}

// GetDefaultDnsResolverSettings returns the default, most secure dns settings:
// encrypted DNS over HTTPS through the tunnel, with the host-side DoH fallback
// while the tunnel starts. This is what the device uses out of the box
func GetDefaultDnsResolverSettings() *DnsResolverSettings {
	muxSettings := connect.DefaultUpgradeMuxSettings()
	if muxSettings == nil || muxSettings.Dns == nil || muxSettings.Dns.Resolver == nil {
		return nil
	}
	settings := dnsResolverSettingsFromConnect(muxSettings.Dns.Resolver)
	settings.EnableFallback = muxSettings.Dns.Fallback != nil
	return settings
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

	AddCanShowRatingDialogChangeListener(listener CanShowRatingDialogChangeListener) Sub

	GetCanPromptIntroFunnel() bool

	SetCanPromptIntroFunnel(canPrompt bool)

	AddCanPromptIntroFunnelChangeListener(listener CanPromptIntroFunnelChangeListener) Sub

	GetAllowForeground() bool

	SetAllowForeground(allowForeground bool)

	AddAllowForegroundChangeListener(listener AllowForegroundChangeListener) Sub

	GetProvideControlMode() ProvideControlMode // auto, always, never

	SetProvideControlMode(mode ProvideControlMode)

	AddProvideControlModeChangeListener(listener ProvideControlModeChangeListener) Sub

	GetProvideNetworkMode() ProvideNetworkMode // wifi, cellular, etc.

	SetProvideNetworkMode(mode ProvideNetworkMode)

	AddProvideNetworkModeChangeListener(listener ProvideNetworkModeChangeListener) Sub

	GetCanRefer() bool

	SetCanRefer(canRefer bool)

	AddCanReferChangeListener(listener CanReferChangeListener) Sub

	SetRouteLocal(routeLocal bool)

	GetRouteLocal() bool

	SetBlockerEnabled(blockerEnabled bool)

	GetBlockerEnabled() bool

	LoadProvideSecretKeys(provideSecretKeyList *ProvideSecretKeyList)

	InitProvideSecretKeys()

	GetProvideEnabled() bool

	GetConnectEnabled() bool

	SetProvideMode(provideMode ProvideMode)

	GetProvideMode() ProvideMode

	AddProvideModeChangeListener(listener ProvideModeChangeListener) Sub

	SetProvidePaused(providePaused bool)

	GetProvidePaused() bool

	SetOffline(offline bool)

	GetOffline() bool

	SetVpnInterfaceWhileOffline(vpnInterfaceWhileOffline bool)

	GetVpnInterfaceWhileOffline() bool

	AddVpnInterfaceWhileOfflineChangeListener(listener VpnInterfaceWhileOfflineChangeListener) Sub

	RemoveDestination()

	SetDestination(location *ConnectLocation, specs *ProviderSpecList)

	SetConnectLocation(location *ConnectLocation)

	GetConnectLocation() *ConnectLocation

	SetDefaultLocation(location *ConnectLocation)

	GetDefaultLocation() *ConnectLocation

	AddDefaultLocationChangeListener(listener DefaultLocationChangeListener) Sub

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

	AddBlockerEnabledChangeListener(listener BlockerEnabledChangeListener) Sub

	AddConnectLocationChangeListener(listener ConnectLocationChangeListener) Sub

	AddProvideSecretKeysListener(listener ProvideSecretKeysListener) Sub

	AddTunnelChangeListener(listener TunnelChangeListener) Sub

	AddContractStatusChangeListener(listener ContractStatusChangeListener) Sub

	// privacy block

	GetBlockStats() *BlockStats

	// the recent routing decisions, one per cluster, gated by a time window
	GetBlockActions() *BlockActionWindow

	AddBlockActionOverride(override *BlockActionOverride)

	// removes the override with the same override id
	RemoveBlockActionOverride(overrideId *Id)

	SetBlockActionOverrides(overrides *BlockActionOverrideList)

	GetBlockActionOverrides() *BlockActionOverrideList

	// these are derived from the set BlockActionOverrides
	GetLocalOverrideAppIds() *OverrideLocalAppIds

	// rate limited window updates
	AddBlockActionWindowChangeListener(listener BlockActionWindowChangeListener) Sub
	// rate limited
	AddBlockStatsChangeListener(listener BlockStatsChangeListener) Sub
	// fires with the full list when the overrides change
	AddBlockActionOverridesChangeListener(listener BlockActionOverridesChangeListener) Sub

	// packet stats

	GetPacketStats() *PacketStats

	// rate limited
	AddPacketStatsChangeListener(listener PacketStatsChangeListener) Sub

	// contract stats

	GetEgressContractStats() *ContractStats
	GetEgressContractDetails() *ContractDetailsList

	GetIngressContractStats() *ContractStats
	GetIngressContractDetails() *ContractDetailsList

	// rate limited
	AddEgressContractStatsChangeListener(listener ContractStatsChangeListener) Sub
	// rate limited
	AddEgressContractDetailsChangeListener(listener ContractDetailsChangeListener) Sub

	// rate limited
	AddIngressContractStatsChangeListener(listener ContractStatsChangeListener) Sub
	// rate limited
	AddIngressContractDetailsChangeListener(listener ContractDetailsChangeListener) Sub

	// provider packet and contract stats. the mirror of the client stats above
	// for the traffic relayed for remote clients. devices without a provider
	// (`AllowProvider`) return nil from the getters and never fire the listeners

	GetProviderPacketStats() *PacketStats

	// rate limited
	AddProviderPacketStatsChangeListener(listener PacketStatsChangeListener) Sub

	GetProviderEgressContractStats() *ContractStats
	GetProviderEgressContractDetails() *ContractDetailsList

	GetProviderIngressContractStats() *ContractStats
	GetProviderIngressContractDetails() *ContractDetailsList

	// rate limited
	AddProviderEgressContractStatsChangeListener(listener ContractStatsChangeListener) Sub
	// rate limited
	AddProviderEgressContractDetailsChangeListener(listener ContractDetailsChangeListener) Sub

	// rate limited
	AddProviderIngressContractStatsChangeListener(listener ContractStatsChangeListener) Sub
	// rate limited
	AddProviderIngressContractDetailsChangeListener(listener ContractDetailsChangeListener) Sub

	SetDnsResolverSettings(dnsResolverSettings *DnsResolverSettings)

	GetDnsResolverSettings() *DnsResolverSettings

	AddDnsResolverSettingsChangeListener(listener DnsResolverSettingsChangeListener) Sub

	GetNetworkPeers() *NetworkPeers

	AddNetworkPeersChangeListener(listener NetworkPeersChangeListener) Sub

	AddWindowStatusChangeListener(listener WindowStatusChangeListener) Sub

	AddJwtRefreshListener(listener JwtRefreshListener) Sub

	AddAuthLogoutListener(listener AuthLogoutListener) Sub

	GetWindowStatus() *WindowStatus

	UploadLogs(feedbackId string, callback UploadLogsCallback) error

	RefreshToken(attempt int) error

	SetPerformanceProfile(performanceProfile *PerformanceProfile)

	GetPerformanceProfile() *PerformanceProfile

	AddPerformanceProfileChangeListener(listener PerformanceProfileChangeListener) Sub
}

// unexported to gomobile
type device interface {
	// monitor for the current connection window
	// the client must get the window monitor each time the connection destination changes
	windowMonitor() windowMonitor
	egressSecurityPolicy() securityPolicy
	ingressSecurityPolicy() securityPolicy
	logger() connect.Logger
}

// deviceLog returns the device's logger, for view controllers and other
// device-attached components, so that they are silenced with the device
// (see `DeviceLocalSettings.DisableLogging`).
func deviceLog(d Device) connect.Logger {
	if dd, ok := d.(device); ok {
		return dd.logger()
	}
	return connect.DefaultLogger()
}

type windowMonitor interface {
	AddMonitorEventCallback(monitorEventCallback connect.MonitorEventFunction) func()
	Events() (*connect.WindowExpandEvent, map[connect.Id]*connect.ProviderEvent)
}

type securityPolicy interface {
	Stats(reset bool) connect.SecurityPolicyStats
	// ResetStats()
}
