package sdk

// device_rpc_provider_state.go -- the provider state a device pushes to a
// browser-state remote (DeviceRemoteProviderState).
//
// A browser-state remote (deviceRpcSettings.BrowserStateOnly, every JavaScript
// remote) never waits on its websocket inside a getter, so its getters read
// only what the device synchronized to it. The rest of the device state
// reaches it through the sync reply and the listeners its app adds; the
// provider state needs more than that: the packet stats and the contract rows
// have no other cache, a pause or a client limit hold after the first sync
// reaches no getter without an app listener, and the provider's connected
// state has no listener at all. So the remote asks for the provider state on
// every sync (DeviceRemoteSyncRequest.ProviderStateListener), and the device
// answers with it in the sync reply and pushes it again after every change for
// as long as the connection lives, whatever listeners the app holds.
//
// The state crosses whole, so a push replaces the remote's copy: a contract
// that closed is gone from the next push rather than left behind, and the
// counts agree with the rows. A push reads the device when it is delivered,
// so a burst of changes coalesces into one push of the newest state.
//
// Within a connection the state only moves forward. The device reads the
// sync reply before the sync reverse sets its service, and pushes only once
// it is set, so every push reads the device after the reply did; the remote
// keeps the reply only until the first push (see DeviceRemote.run), because
// the reply crosses on the forward stream and a push on the reverse stream can
// overtake it.
//
// The contract rows fan out to the remote's provider contract details
// listeners from the pushes (DeviceRemote.providerStateChanged), and the
// device does not send them row by row to a connection that holds the
// provider state: a row sent alone could reach a listener before the state
// that holds it, and a listener such as ContractDetailsViewController reads
// the rows back from the getters.
//
// A device that predates the push ignores the request and answers no state.
// The remote then drops any state it held, and its provider getters read nil,
// as they did before the push existed. A native remote never asks: it reads
// through to the device.

import (
	"reflect"

	"github.com/urnetwork/connect"
)

// The provider state a device pushes to a remote that asked for it. Every
// field is read from the device's own getters when the push is made.
//
//gomobile:noexport
type DeviceRemoteProviderState struct {
	// the device has a provider (DeviceLocalSettings.AllowProvider). Without
	// one the provider stats and contract details read nil, as on the device
	Provider bool

	ProvideEnabled bool
	ProvidePaused  bool
	// a ProvideMode, spelled as its basic type for gobind (golang/go#71827)
	ProvideMode       int
	ProviderConnected bool
	ClientLimitStatus *ClientLimitStatus

	PacketStats            *PacketStatsRpc
	EgressContractStats    *ContractStats
	IngressContractStats   *ContractStats
	EgressContractDetails  []*ContractDetailsRpc
	IngressContractDetails []*ContractDetailsRpc
}

// The provider packet stats of the state, nil without a provider or without
// a state.
func (self *DeviceRemoteProviderState) packetStats() *PacketStats {
	if self == nil || !self.Provider {
		return nil
	}
	return self.PacketStats.toPacketStats(true)
}

// A copy of one direction's contract stats of the state, nil without a
// provider or without a state.
func (self *DeviceRemoteProviderState) contractStats(receive bool) *ContractStats {
	if self == nil || !self.Provider {
		return nil
	}
	contractStats := self.EgressContractStats
	if receive {
		contractStats = self.IngressContractStats
	}
	if contractStats == nil {
		return nil
	}
	copied := *contractStats
	return &copied
}

// One direction's contract rows of the state, nil without a provider or
// without a state.
func (self *DeviceRemoteProviderState) contractDetails(receive bool) *ContractDetailsList {
	if self == nil || !self.Provider {
		return nil
	}
	if receive {
		return toContractDetailsList(self.IngressContractDetails)
	}
	return toContractDetailsList(self.EgressContractDetails)
}

// Reports whether two lists hold the same rows. The device lists its contracts
// in map order, so rows are matched by contract id; a row without one never
// matches.
func contractDetailsRpcsEqual(a []*ContractDetailsRpc, b []*ContractDetailsRpc) bool {
	if len(a) != len(b) {
		return false
	}
	contractIdRows := map[connect.Id]*ContractDetailsRpc{}
	for _, row := range a {
		if row == nil || row.ContractId == nil {
			return false
		}
		contractIdRows[*row.ContractId] = row
	}
	for _, row := range b {
		if row == nil || row.ContractId == nil {
			return false
		}
		previous, ok := contractIdRows[*row.ContractId]
		if !ok || !reflect.DeepEqual(previous, row) {
			return false
		}
	}
	return true
}

// Turns every device change the provider state carries into one push of the
// whole state.
type providerStateForwarder struct {
	rpc *DeviceLocalRpc
}

// ProvideChangeListener
func (self *providerStateForwarder) ProvideChanged(provideEnabled bool) {
	self.rpc.providerStateChanged()
}

// ProvidePausedChangeListener
func (self *providerStateForwarder) ProvidePausedChanged(providePaused bool) {
	self.rpc.providerStateChanged()
}

// ProvideModeChangeListener
func (self *providerStateForwarder) ProvideModeChanged(provideMode ProvideMode) {
	self.rpc.providerStateChanged()
}

// ClientLimitStatusChangeListener
func (self *providerStateForwarder) ClientLimitStatusChanged(status *ClientLimitStatus) {
	self.rpc.providerStateChanged()
}

// PacketStatsChangeListener, for the provider packet stats
func (self *providerStateForwarder) PacketStatsChanged(packetStats *PacketStats) {
	self.rpc.providerStateChanged()
}

// ContractStatsChangeListener, for the provider contract stats
func (self *providerStateForwarder) ContractStatsChanged(contractStats *ContractStats) {
	self.rpc.providerStateChanged()
}

// The callback of DeviceLocal.addProviderConnectedChangeCallback.
func (self *providerStateForwarder) providerConnectedChanged(providerConnected bool) {
	self.rpc.providerStateChanged()
}

// Subscribes, once per connection, to every device change the provider state
// carries. It runs before the remote's own listeners are added, so the device
// calls it first for a change both hear, and the push of the state is queued
// ahead of the listener's notification unless that notification is still
// queued from an earlier change. The contract stats stand for the rows: the
// device reports both on every emit of its contracts, and the stats also on an
// emit with no rows left, which the per row details never report. Must be
// called with stateLock.
func (self *DeviceLocalRpc) addProviderStateListenerWithLock() {
	if self.providerStateSubs != nil {
		return
	}
	forwarder := &providerStateForwarder{
		rpc: self,
	}
	self.providerStateSubs = []Sub{
		self.deviceLocal.AddProvideChangeListener(forwarder),
		self.deviceLocal.AddProvidePausedChangeListener(forwarder),
		self.deviceLocal.AddProvideModeChangeListener(forwarder),
		self.deviceLocal.AddClientLimitStatusChangeListener(forwarder),
		self.deviceLocal.addProviderConnectedChangeCallback(forwarder.providerConnectedChanged),
		self.deviceLocal.AddProviderPacketStatsChangeListener(forwarder),
		self.deviceLocal.AddProviderEgressContractStatsChangeListener(forwarder),
		self.deviceLocal.AddProviderIngressContractStatsChangeListener(forwarder),
	}
}

// Ends the subscription with the connection. Must be called with stateLock.
func (self *DeviceLocalRpc) removeProviderStateListenerWithLock() {
	for _, sub := range self.providerStateSubs {
		sub.Close()
	}
	self.providerStateSubs = nil
}

// Queues one push of the provider state, coalesced with a push still queued
// (see sendLoop). The push reads the device when it is delivered, and only
// once the sync reverse has set the service, so it reads the device after the
// sync reply did. Never blocks.
func (self *DeviceLocalRpc) providerStateChanged() {
	const name = "DeviceRemoteRpc.ProviderStateChanged"
	self.enqueueReverse(name, func() {
		hasService := func() bool {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			return self.service != nil
		}()
		if hasService {
			self.reverseCall(name, self.providerState())
		}
	})
}

// Reads the provider state from the device's getters. Sync reads it holding
// stateLock, as it reads state(); the pushes read it holding no lock.
func (self *DeviceLocalRpc) providerState() *DeviceRemoteProviderState {
	deviceLocal := self.deviceLocal
	packetStats := deviceLocal.GetProviderPacketStats()
	providerState := &DeviceRemoteProviderState{
		Provider:          packetStats != nil,
		ProvideEnabled:    deviceLocal.GetProvideEnabled(),
		ProvidePaused:     deviceLocal.GetProvidePaused(),
		ProvideMode:       deviceLocal.GetProvideMode(),
		ProviderConnected: deviceLocal.GetProviderConnected(),
		ClientLimitStatus: deviceLocal.GetClientLimitStatus(),
		PacketStats:       newPacketStatsRpc(packetStats, true),
	}
	if providerState.Provider {
		providerState.EgressContractStats = deviceLocal.GetProviderEgressContractStats()
		providerState.IngressContractStats = deviceLocal.GetProviderIngressContractStats()
		providerState.EgressContractDetails = newContractDetailsListRpc(deviceLocal.GetProviderEgressContractDetails())
		providerState.IngressContractDetails = newContractDetailsListRpc(deviceLocal.GetProviderIngressContractDetails())
	}
	return providerState
}

// Answers DeviceRemote.GetProviderConnected for a remote that reads through.
func (self *DeviceLocalRpc) GetProviderConnected(_ RpcNoArg, providerConnected *bool) error {
	*providerConnected = self.deviceLocal.GetProviderConnected()
	return nil
}

// Takes a push of the provider state.
func (self *DeviceRemoteRpc) ProviderStateChanged(providerState *DeviceRemoteProviderState, _ RpcVoid) error {
	self.deviceRemote.log.Infof("[drrpc]ProviderStateChanged")
	self.dispatch(func() {
		self.deviceRemote.providerStateChanged(providerState)
	})
	return nil
}

// Makes providerState the remote's copy, including the values the provide
// getters read from the last known state. Must be called with stateLock.
func (self *DeviceRemote) setProviderStateWithLock(providerState *DeviceRemoteProviderState) {
	self.lastProviderState = providerState
	self.lastKnownState.ProvideEnabled.Set(providerState.ProvideEnabled)
	self.lastKnownState.ProvidePaused.Set(providerState.ProvidePaused)
	self.lastKnownState.ProvideMode.Set(providerState.ProvideMode)
	self.lastProviderConnected = providerState.ProviderConnected
	if providerState.ClientLimitStatus != nil {
		self.lastClientLimitStatus = cloneClientLimitStatus(providerState.ClientLimitStatus)
	}
}

// Reports a queued write the next sync applies that can change the provide
// state: the provide or control mode, or a destination, which moves the auto
// control mode between public and network. While one is queued the provide
// getters derive the state from it rather than read the pushed state, which
// predates it. Must be called with stateLock.
func (self *DeviceRemote) provideQueuedWithLock() bool {
	return self.state.ProvideControlMode.IsSet ||
		self.state.ProvideMode.IsSet ||
		self.state.Location.IsSet ||
		self.state.Destination.IsSet ||
		self.state.RemoveDestination.IsSet
}

// Takes the provider state of a sync reply, after the reply's state replaced
// the last known state. A push of the same connection read the device after
// the reply did, so once one landed it stays, and is applied again over the
// provide values the reply's state replaced. A device that answers no state
// pushes none, so the last one is dropped rather than read on without a
// refresh. Must be called with stateLock.
func (self *DeviceRemote) syncProviderStateWithLock(providerState *DeviceRemoteProviderState) {
	switch {
	case providerState == nil:
		self.lastProviderState = nil
	case self.providerStatePushed && self.lastProviderState != nil:
		self.setProviderStateWithLock(self.lastProviderState)
	default:
		self.setProviderStateWithLock(providerState)
	}
}

// Takes a push, which read the device after the sync reply and every earlier
// push of the connection did, so it replaces the remote's copy whole. The
// contract rows then fan out to the provider contract details listeners as the
// device fans out an emit of its contracts: every row of a direction whose
// rows changed, and on the first push of a connection every row, which is the
// state a listener gets on each sync. The rows are in the copy before a
// listener hears of them, so a listener that reads the getters reads the rows
// it was told of.
func (self *DeviceRemote) providerStateChanged(providerState *DeviceRemoteProviderState) {
	if providerState == nil {
		return
	}
	var egressListeners []ContractDetailsChangeListener
	var ingressListeners []ContractDetailsChangeListener
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		previous := self.lastProviderState
		initial := !self.providerStatePushed || previous == nil
		self.providerStatePushed = true
		self.setProviderStateWithLock(providerState)
		if initial || !contractDetailsRpcsEqual(previous.EgressContractDetails, providerState.EgressContractDetails) {
			egressListeners = listenerList(self.providerEgressContractDetailsChangeListeners)
		}
		if initial || !contractDetailsRpcsEqual(previous.IngressContractDetails, providerState.IngressContractDetails) {
			ingressListeners = listenerList(self.providerIngressContractDetailsChangeListeners)
		}
	}()
	fanOut := func(listeners []ContractDetailsChangeListener, contractDetailsRpcs []*ContractDetailsRpc) {
		if len(listeners) == 0 {
			return
		}
		for _, contractDetailsRpc := range contractDetailsRpcs {
			if contractDetailsRpc == nil {
				continue
			}
			contractDetails := contractDetailsRpc.toContractDetails()
			for _, listener := range listeners {
				connect.HandleError(func() {
					listener.ContractDetailsChanged(contractDetails)
				})
			}
		}
	}
	fanOut(egressListeners, providerState.EgressContractDetails)
	fanOut(ingressListeners, providerState.IngressContractDetails)
}

// Reports whether the provider of the device process has a platform transport
// with a registered route (DeviceLocal.GetProviderConnected). It reads through
// to the device process. Without a service it reads the last value the device
// process answered or pushed -- which is how a browser-state remote reads it,
// from the provider state the device pushes -- and false before the first. A
// device process too old to answer keeps its rpc session and reads the last
// value.
func (self *DeviceRemote) GetProviderConnected() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.service != nil {
		providerConnected, err := rpcCallNoArgAllowMissingMethod[bool](
			self.service,
			"DeviceLocalRpc.GetProviderConnected",
			self.closeService,
		)
		if err == nil {
			self.lastProviderConnected = providerConnected
			return providerConnected
		}
	}
	return self.lastProviderConnected
}
