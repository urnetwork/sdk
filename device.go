package sdk

import (
	"github.com/urnetwork/connect"
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

type ProviderChangeListener interface {
	ProviderChanged(providerEnabled bool)
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

type BlockChangeListener interface {
	BlockChanged(blockEnabled bool)
}

type BlockActionWindowChangeListener interface {
	BlockActionWindowChanged(blockActionWindow *BlockActionWindow)
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

type IpProtocol = int

const (
	IpProtocolUnkown IpProtocol = 0
	IpProtocolUdp    IpProtocol = 1
	IpProtocolTcp    IpProtocol = 2
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
	BlockActionOverrides *BlockActionOverrideList
	BlockActions         *BlockActionList
}

type BlockActionOverride struct {
	Host          string
	BlockOverride bool
}

type BlockAction struct {
	Time          int64
	Host          string
	Block         bool
	Override      bool
	BlockOverride bool
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

	Ipv4    string
	Ipv6    string
	Country string
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

	GetProvideControlMode() ProvideControlMode

	GetAllowForeground() bool

	SetAllowForeground(allowForeground bool)

	SetProvideControlMode(mode ProvideControlMode)

	GetCanRefer() bool

	SetCanRefer(canRefer bool)

	SetRouteLocal(routeLocal bool)

	GetRouteLocal() bool

	LoadProvideSecretKeys(provideSecretKeyList *ProvideSecretKeyList)

	InitProvideSecretKeys()

	GetProvideEnabled() bool

	GetConnectEnabled() bool

	SetProvideMode(provideMode ProvideMode)

	GetProvideMode() ProvideMode

	SetProvidePaused(providePaused bool)

	GetProvidePaused() bool

	SetOffline(offline bool)

	GetOffline() bool

	SetVpnInterfaceWhileOffline(vpnInterfaceWhileOffline bool)

	GetVpnInterfaceWhileOffline() bool

	RemoveDestination()

	SetDestination(location *ConnectLocation, specs *ProviderSpecList, provideMode ProvideMode)

	SetConnectLocation(location *ConnectLocation)

	GetConnectLocation() *ConnectLocation

	SetDefaultLocation(location *ConnectLocation)

	GetDefaultLocation() *ConnectLocation

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

	GetProviderEnabled() bool

	SetProviderEnabled(providerEnabled bool)

	AddProviderChangeListener(listener ProviderChangeListener) Sub

	// privacy block

	GetBlockStats() *BlockStats

	// includes applicable overrides
	GetBlockActions() *BlockActionWindow

	// host can be *.H, **.H (includes H and any subdomain), or a regex s/H/
	OverrideBlockAction(hostPattern string, block bool)

	// exact match of the host pattern
	RemoveBlockActionOverride(hostPattern string)

	SetBlockActionOverrideList(blockActionOverrides *BlockActionOverrideList)

	GetBlockEnabled() bool

	SetBlockEnabled(blockEnabled bool)

	GetBlockWhileDisconnected() bool

	SetBlockWhileDisconnected(blockWhileDisconnected bool)

	AddBlockChangeListener(listener BlockChangeListener) Sub
	// rate limited window updates
	AddBlockActionWindowChangeListener(listener BlockActionWindowChangeListener) Sub
	// rate limited
	AddBlockStatsChangeListener(listener BlockStatsChangeListener) Sub

	// contract stats

	GetEgressContractStats() *ContractStats
	GetEgressContractDetails() *ContractDetailsList

	GetIngressContractStats() *ContractStats
	GetIngressContractDetails() *ContractDetailsList

	// rate limited
	AddEgressContratStatsChangeListener(listener ContractStatsChangeListener) Sub
	// rate limited
	AddEgressContractDetailsChangeListener(listener ContractDetailsChangeListener) Sub

	// rate limited
	AddIngressContratStatsChangeListener(listener ContractStatsChangeListener) Sub
	// rate limited
	AddIngressContractDetailsChangeListener(listener ContractDetailsChangeListener) Sub

	AddWindowStatusChangeListener(listener WindowStatusChangeListener) Sub
	GetWindowStatus() *WindowStatus
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
