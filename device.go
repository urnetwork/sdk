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


type IpProtocol = int
const IpProtocolUnkown = 0
const IpProtocolUdp = 1
const IpProtocolTcp = 2

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

	GetProvideWhileConnected() bool

	SetProvideWhileConnected(provideWhileConnected bool)

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


	// privacy block

	GetBlockStats() *BlockStats

	// includes applicable overrides
	GetRecentBlockActions(blockActionId) *BlockActionWindow // FIXME return list of blockActions since the given id

	// host can be *.H, **.H, or a regex s/H/
	OverrideBlockAction(host string, block bool)

	RemoveBlockActionOverride(host string)

	GetBlockEnabled() bool

	SetBlockEnabled(blockEnabled bool)

	GetBlockWhileDisconnected() bool

	SetBlockWhileDisconnected(blockWhileDisconnected bool)

	// FIXME block action override listener, include all block action overrides in window

	// FIXME block action listener


	// FIXME real time contract status




	// FIXME pubsub in api

	// FIXME api for contract status
	// receive: companion and non companion
	// send: companion and non companion
	// limit and current fill of each
	// this is a poll only api. No events since it changes so much
	// ordered by last used time
	// client id and location of client
	GetEgressContractStats() *ContractStats
	GetEgressContractDetails() *ContractDetailsList
	
	GetIngressContractStats() *ContractStats
	GetIngressContractDetails() *ContractDetailsList
	

	// FIXME contract details listener
	// FIXME contract stats listener



}


type BlockStats struct {

	// allowed count
	// blocked count

}

type BlockActionWindow struct {
	// host -> block action overrides
	// []*BlockAction


}

type BlockAction struct {
	Time time.Time
	Host string
	Block bool
	Override bool
	BlockOverride bool
}


type ContractStats struct {
	ContractUsedByteCount ByteCount
	ContractByteCount ByteCount
	ContractBitRate int

	CompanionContractUsedByteCount ByteCount
	CompanionContractByteCount ByteCount
	CompanionContractBitRate int

}


type ContractDetails struct {
	ContractId *Id
	ContractUsedByteCount ByteCount
	ContractByteCount ByteCount
	ContractBitRate int
	ContractClientId *Id

	CompanionContractId *Id
	CompanionContractUsedByteCount ByteCount
	CompanionContractByteCount ByteCount
	CompanionContractBitRate int
	CompanionContractClientId *Id

	Ipv4 string
	Ipv6 string
	Country string

}


// block action:  block action id;  status allow, deny, remove;  host name if status is allow or deny



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

