// The device channel (§18.6's Setup and Mirror; D-SDK-1, D-SDK-3, D-SDK-13, D-SDK-32, DP1, owner decision 197;
// SDK-M8-07): forwarding's three setup calls between the sidecar role and the app's VPN component, and the bridge
// status mirror.
//
//	keys       ForwardingKeys(networkId): the component's public halves (C0), nothing else
//	hand-off   ForwardingHandOff(bytes): the sidecar role's hand-off (S6 to C7), which the component checks itself
//	witness    ForwardingWitness(networkId, pin, fork): the app's witnessed pin, a hint the component's Forwarder
//	           follows to and judges itself
//	status     GetBridgeStatus and AddBridgeStatusChangeListener: the device role's state with each forwarding role's
//	           fields, read-only
//
// In process on Android (DeviceLocal's own calls), over the device RPC to the Apple extensions (DeviceRemote), and from
// ur daemon through a paired desktop app (sidecar_forward_setup.go relays to the daemon). Each RPC method is additive
// and listed in its method table, so an older peer answers it unknown and DeviceRpcVersion stays 3. No state,
// delegation or key crosses: the component compiles its own state, signs its own links, and its private keys never
// leave it, while the sidecar's never enter it (KeysStayPut). Nothing the channel carries can lower a posture: a
// witness is a hint, a status is read-only, and a hand-off the component did not verify is refused (DP1).
//
// GetBridgeRoutes and SetBridgeLocalAddresses are the tun owner's, bound on DeviceLocal in its own process (§18.15).
// Hosted devices refuse every call (D-SDK-12). Untagged: the tunnel links it. Safe for concurrent use.

package sdk

import (
	"errors"
	"net/netip"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"

	sidecardevice "github.com/urnetwork/sidecar/device"
	"github.com/urnetwork/sidecar/protocol/bridge"
)

// One network's forwarding as a status shows it (§18.6's Status, the mirror's forwarding fields), from the role's own
// state and its Forwarder's (device_sidecar_forward.go).
type deviceForwardingStatus struct {
	NetworkId connect.Id
	// The device client's network, the one role that hosts.
	Hosting bool
	Posture sidecardevice.PostureState
	// none, generated, delegated or active (4.2.6).
	KeySet string
	// F's token is past its exp: the posture holds pending until a new hand-off.
	Lapsed bool
	// F's exp, zero while none is kept.
	ForwardClientExpiresAt time.Time
	// The Forwarder's newest entry and compile, the bridge_alarm reasons standing, and its compile counts.
	TipN                uint64
	CompiledN           uint64
	Alarms              []string
	CompileCount        uint64
	CompileErrorCount   uint64
	CompileRefusedCount uint64
	// The newest head_alarm the Forwarder raised, zero for none.
	HeadAlarm deviceForwardHeadAlarm
}

// A head_alarm as a role's status shows it: its kind, when the Forwarder raised it, and the two pins' positions, this
// device's and the other's (§6.5's evidence, positions alone, I-13). Zero for none. Declared here, beside the status
// that carries it, so the js leg, which runs no Forwarder, builds the status too.
type deviceForwardHeadAlarm struct {
	Kind   string
	At     time.Time
	N      uint64
	OtherN uint64
}

// What forwarding's setup asks of the device that runs the VPN component: DeviceLocal in process, DeviceRemote over
// the device RPC. Go only.
type deviceForwardingChannel interface {
	// C0: the component's public halves for networkId.
	forwardingKeys(networkId connect.Id) (*bridge.ForwardingKeys, error)
	// C7: the hand-off, which the component checks itself.
	forwardingHandOff(handOff *bridge.ForwardingHandOff) error
	// The app's witnessed pin and fork for networkId.
	forwardingWitness(networkId connect.Id, pin *bridge.AuthPin, fork bool) error
}

var (
	_ deviceForwardingChannel = (*DeviceLocal)(nil)
	_ deviceForwardingChannel = (*DeviceRemote)(nil)
)

// Why a device answers no forwarding call.
var (
	// A device built with no forwarding key store or directory, or one whose role is off.
	errDeviceForwardingNone = errors.New("this device runs no forwarding role")
	// The VPN component's process is too old to know the call, or the RPC is down.
	errDeviceForwardingUnreachable = errors.New("the VPN component does not answer forwarding's setup")
)

// The forwarding host, or why the device has none.
func (self *DeviceLocal) forwardingChannelHost(name string) (*deviceLocalForwardingHost, error) {
	if self.hostedIncompatibleGuarded(name) {
		return nil, errDeviceBridgeHosted
	}
	host := self.forwardingHost()
	if host == nil {
		if self.bridgeOwner != nil && self.bridgeOwner.refusal != nil {
			return nil, self.bridgeOwner.refusal
		}
		return nil, errDeviceForwardingNone
	}
	return host, nil
}

// See deviceForwardingChannel.
func (self *DeviceLocal) forwardingKeys(networkId connect.Id) (*bridge.ForwardingKeys, error) {
	host, err := self.forwardingChannelHost("forwardingKeys")
	if err != nil {
		return nil, err
	}
	return host.keys(networkId)
}

// See deviceForwardingChannel.
func (self *DeviceLocal) forwardingHandOff(handOff *bridge.ForwardingHandOff) error {
	host, err := self.forwardingChannelHost("forwardingHandOff")
	if err != nil {
		return err
	}
	return host.handOff(handOff)
}

// See deviceForwardingChannel.
func (self *DeviceLocal) forwardingWitness(networkId connect.Id, pin *bridge.AuthPin, fork bool) error {
	host, err := self.forwardingChannelHost("forwardingWitness")
	if err != nil {
		return err
	}
	return host.witness(networkId, pin, fork)
}

// ForwardingKeys' argument and answer on the device RPC: the network, and the encoded bridge.ForwardingKeys. RPC
// plumbing, named with the DeviceRemote prefix mobileexports keeps out of the bindings, as every RPC message type is.
type DeviceRemoteForwardingKeysArgs struct {
	NetworkId connect.Id
}

type DeviceRemoteForwardingKeys struct {
	Keys []byte
}

// ForwardingHandOff's argument: the encoded bridge.ForwardingHandOff, which holds F's token and is never logged (I-7).
type DeviceRemoteForwardingHandOff struct {
	HandOff []byte
}

// ForwardingWitness's argument: the network, the encoded bridge.AuthPin, and whether the sidecar role found a fork.
type DeviceRemoteForwardingWitness struct {
	NetworkId connect.Id
	Pin       []byte
	Fork      bool
}

// The device RPC's ForwardingKeys (the file's comment).
func (self *DeviceLocalRpc) ForwardingKeys(args *DeviceRemoteForwardingKeysArgs, reply **DeviceRemoteForwardingKeys) error {
	if args == nil {
		return errors.New("forwarding keys name a network")
	}
	keys, err := self.deviceLocal.forwardingKeys(args.NetworkId)
	if err != nil {
		return err
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(keys)
	if err != nil {
		return err
	}
	*reply = &DeviceRemoteForwardingKeys{Keys: encoded}
	return nil
}

// The device RPC's ForwardingHandOff (the file's comment). A refusal names no byte of the hand-off.
func (self *DeviceLocalRpc) ForwardingHandOff(args *DeviceRemoteForwardingHandOff, _ RpcVoid) error {
	if args == nil {
		return errors.New("a hand-off is needed")
	}
	handOff := &bridge.ForwardingHandOff{}
	if err := proto.Unmarshal(args.HandOff, handOff); err != nil {
		return errors.New("the hand-off does not decode")
	}
	return self.deviceLocal.forwardingHandOff(handOff)
}

// The device RPC's ForwardingWitness (the file's comment).
func (self *DeviceLocalRpc) ForwardingWitness(args *DeviceRemoteForwardingWitness, _ RpcVoid) error {
	if args == nil {
		return errors.New("a witness names a network")
	}
	pin := &bridge.AuthPin{}
	if err := proto.Unmarshal(args.Pin, pin); err != nil {
		return errors.New("the witnessed pin does not decode")
	}
	return self.deviceLocal.forwardingWitness(args.NetworkId, pin, args.Fork)
}

// One call to the VPN component, which an older peer answers unknown without the RPC closing.
func deviceRemoteForwardingCall[T any](self *DeviceRemote, name string, arg any) (T, error) {
	var reply T
	self.stateLock.Lock()
	service := self.service
	self.stateLock.Unlock()
	if service == nil {
		return reply, errDeviceForwardingUnreachable
	}
	service.log.Infof("[rpc]%s", name)
	err := service.Call(name, arg, &reply)
	if err != nil && rpcMissingMethodError(err) {
		return reply, errDeviceForwardingUnreachable
	}
	if err != nil {
		var serverError deviceRpcServerError
		if !errors.As(err, &serverError) {
			self.closeServiceInstance(service)
			return reply, errDeviceForwardingUnreachable
		}
	}
	return reply, err
}

// See deviceForwardingChannel.
func (self *DeviceRemote) forwardingKeys(networkId connect.Id) (*bridge.ForwardingKeys, error) {
	reply, err := deviceRemoteForwardingCall[*DeviceRemoteForwardingKeys](self, "DeviceLocalRpc.ForwardingKeys", &DeviceRemoteForwardingKeysArgs{NetworkId: networkId})
	if err != nil {
		return nil, err
	}
	if reply == nil {
		return nil, errDeviceForwardingUnreachable
	}
	keys := &bridge.ForwardingKeys{}
	if err := proto.Unmarshal(reply.Keys, keys); err != nil {
		return nil, errors.New("the component's keys do not decode")
	}
	return keys, nil
}

// See deviceForwardingChannel.
func (self *DeviceRemote) forwardingHandOff(handOff *bridge.ForwardingHandOff) error {
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(handOff)
	if err != nil {
		return err
	}
	_, err = deviceRemoteForwardingCall[RpcVoid](self, "DeviceLocalRpc.ForwardingHandOff", &DeviceRemoteForwardingHandOff{HandOff: encoded})
	return err
}

// See deviceForwardingChannel.
func (self *DeviceRemote) forwardingWitness(networkId connect.Id, pin *bridge.AuthPin, fork bool) error {
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(pin)
	if err != nil {
		return err
	}
	_, err = deviceRemoteForwardingCall[RpcVoid](self, "DeviceLocalRpc.ForwardingWitness", &DeviceRemoteForwardingWitness{NetworkId: networkId, Pin: encoded, Fork: fork})
	return err
}

// One forwarding role's fields in a status (DESIGN-DETAILS 4.2.6's states). json in the C ABI and js.
type BridgeNetworkStatus struct {
	NetworkId *Id `json:"network_id"`
	// The device client's network, the one role that hosts (D-SDK-3).
	Hosting bool `json:"hosting,omitempty"`
	// legacy, pending, enforcing_doc or enforcing_nodoc (§17.8).
	Posture string `json:"posture"`
	// none, generated, delegated or active.
	KeySet string `json:"key_set"`
	// F's token passed its exp: pending until the app's next setup.
	Lapsed bool `json:"lapsed,omitempty"`
	// F's exp, in unix milliseconds, 0 for none.
	ForwardClientExpiresMillis int64 `json:"forward_client_expires_millis,omitempty"`
	// The newest entry the component holds, and the pin of its newest compile.
	TipN      int64 `json:"tip_n,omitempty"`
	CompiledN int64 `json:"compiled_n,omitempty"`
	// The bridge_alarm reasons standing: compile_over_budget, compile_deferred, config_key_waiting,
	// forward_client_lapsed, narrowing_overdue.
	Alarms *StringList `json:"alarms,omitempty"`
	// Compiles handed to the posture, failed and refused by the admission rule since the component started.
	CompileCount        int64 `json:"compile_count,omitempty"`
	CompileErrorCount   int64 `json:"compile_error_count,omitempty"`
	CompileRefusedCount int64 `json:"compile_refused_count,omitempty"`
	// The newest head_alarm the component raised (fork, withheld, unverified), empty for none: when, in unix
	// milliseconds, and the positions of its two pins, the component's and the one it was shown (§6.5).
	HeadAlarm       string `json:"head_alarm,omitempty"`
	HeadAlarmMillis int64  `json:"head_alarm_millis,omitempty"`
	HeadAlarmN      int64  `json:"head_alarm_n,omitempty"`
	HeadAlarmOtherN int64  `json:"head_alarm_other_n,omitempty"`
}

// Each forwarding role's fields.
type BridgeNetworkStatusList struct {
	exportedList[*BridgeNetworkStatus]
}

// An empty list.
func NewBridgeNetworkStatusList() *BridgeNetworkStatusList {
	return &BridgeNetworkStatusList{
		exportedList: *newExportedList[*BridgeNetworkStatus](),
	}
}

// The device role's status as the mirror carries it (§18.6's Status): the device client's network's bridge, then each
// forwarding role. Names states, counts and this network's own block, never a key, a name or a client's address
// (I-13). json in the C ABI and js.
type BridgeStatus struct {
	// legacy, pending, enforcing_doc or enforcing_nodoc for the device client's network (§17.8).
	State       string `json:"state"`
	HasDocument bool   `json:"has_document,omitempty"`
	PinN        int64  `json:"pin_n,omitempty"`
	// The network's block, empty before a state names one.
	Block string `json:"block,omitempty"`
	// This host's overlay addresses.
	Addresses *StringList `json:"addresses,omitempty"`
	// The rules in force.
	EntryCount   int32 `json:"entry_count,omitempty"`
	MapCount     int32 `json:"map_count,omitempty"`
	ServedCount  int32 `json:"served_count,omitempty"`
	SubjectCount int32 `json:"subject_count,omitempty"`
	// The sender's links, open and building.
	LinkOpenCount     int32 `json:"link_open_count,omitempty"`
	LinkBuildingCount int32 `json:"link_building_count,omitempty"`
	// §12.9's private names in force.
	NameCount int32 `json:"name_count,omitempty"`
	// Each forwarding role, in the order the networks joined.
	Networks *BridgeNetworkStatusList `json:"networks,omitempty"`
}

// App-implemented: called on a Go thread with each status after a change.
type BridgeStatusChangeListener interface {
	BridgeStatusChanged(status *BridgeStatus)
}

// The bound status of a bridge's and the forwarding roles'.
func bridgeStatusOf(status sidecardevice.BridgeStatus, roles []deviceForwardingStatus) *BridgeStatus {
	bound := &BridgeStatus{
		State:             status.State.String(),
		HasDocument:       status.HasDocument,
		PinN:              int64(min(status.PinN, uint64(1<<63-1))),
		Addresses:         NewStringList(),
		EntryCount:        int32(status.EntryCount),
		MapCount:          int32(status.MapCount),
		ServedCount:       int32(status.ServedCount),
		SubjectCount:      int32(status.SubjectCount),
		LinkOpenCount:     int32(status.LinkOpenCount),
		LinkBuildingCount: int32(status.LinkBuildingCount),
		NameCount:         int32(status.NameCount),
		Networks:          NewBridgeNetworkStatusList(),
	}
	if status.Block.IsValid() {
		bound.Block = status.Block.String()
	}
	for _, address := range status.Addresses {
		bound.Addresses.Add(address.String())
	}
	for _, role := range roles {
		network := &BridgeNetworkStatus{
			NetworkId:           newId([16]byte(role.NetworkId)),
			Hosting:             role.Hosting,
			Posture:             role.Posture.String(),
			KeySet:              role.KeySet,
			Lapsed:              role.Lapsed,
			TipN:                int64(min(role.TipN, uint64(1<<63-1))),
			CompiledN:           int64(min(role.CompiledN, uint64(1<<63-1))),
			Alarms:              NewStringList(),
			CompileCount:        int64(min(role.CompileCount, uint64(1<<63-1))),
			CompileErrorCount:   int64(min(role.CompileErrorCount, uint64(1<<63-1))),
			CompileRefusedCount: int64(min(role.CompileRefusedCount, uint64(1<<63-1))),
		}
		if !role.ForwardClientExpiresAt.IsZero() {
			network.ForwardClientExpiresMillis = role.ForwardClientExpiresAt.UnixMilli()
		}
		if role.HeadAlarm.Kind != "" {
			network.HeadAlarm = role.HeadAlarm.Kind
			network.HeadAlarmMillis = role.HeadAlarm.At.UnixMilli()
			network.HeadAlarmN = int64(min(role.HeadAlarm.N, uint64(1<<63-1)))
			network.HeadAlarmOtherN = int64(min(role.HeadAlarm.OtherN, uint64(1<<63-1)))
		}
		network.Alarms.addAll(role.Alarms...)
		bound.Networks.Add(network)
	}
	return bound
}

// The device role's status (the file's comment), legacy's on a device that runs none.
func (self *DeviceLocal) GetBridgeStatus() *BridgeStatus {
	status, err := traceWithReturn(func() *BridgeStatus {
		bridgeStatus, err := self.bridgeStatus()
		if err != nil {
			bridgeStatus = sidecardevice.BridgeStatus{State: sidecardevice.PostureLegacy}
		}
		var roles []deviceForwardingStatus
		if host := self.forwardingHost(); host != nil {
			roles = host.status()
		}
		return bridgeStatusOf(bridgeStatus, roles)
	})
	if err != nil {
		return bridgeStatusOf(sidecardevice.BridgeStatus{State: sidecardevice.PostureLegacy}, nil)
	}
	return status
}

// Calls listener with the status after each change of the device role, on a Go thread, until the answer closes.
func (self *DeviceLocal) AddBridgeStatusChangeListener(listener BridgeStatusChangeListener) Sub {
	if listener == nil {
		return newSub(func() {})
	}
	callbackId := self.bridgeStatusListeners().Add(listener)
	return newSub(func() {
		self.bridgeStatusListeners().Remove(callbackId)
	})
}

// The status listeners, made at the first use.
func (self *DeviceLocal) bridgeStatusListeners() *connect.CallbackList[BridgeStatusChangeListener] {
	self.bridgeStatusListenersOnce.Do(func() {
		self.bridgeStatusChangeListeners = connect.NewCallbackList[BridgeStatusChangeListener]()
	})
	return self.bridgeStatusChangeListeners
}

// Tells the status listeners, on a goroutine of its own, so no listener runs under a lock of the device's or the
// owner's.
func (self *DeviceLocal) notifyBridgeStatus() {
	if settings := self.settings; settings != nil && settings.testingBridgeStatusNotify != nil {
		settings.testingBridgeStatusNotify()
	}
	listeners := self.bridgeStatusListeners().Get()
	if len(listeners) == 0 {
		return
	}
	go connect.HandleError(func() {
		status := self.GetBridgeStatus()
		for _, listener := range listeners {
			trace(func() {
				listener.BridgeStatusChanged(status)
			})
		}
	})
}

// The status as the device RPC carries it: plain fields and slices, since gob encodes no StringList or bound list,
// whose fields are unexported. RPC plumbing, named with the DeviceRemote prefix mobileexports keeps out of the
// bindings, as every RPC message type is.
//
//gomobile:noexport
type DeviceRemoteBridgeStatus struct {
	State             string
	HasDocument       bool
	PinN              int64
	Block             string
	Addresses         []string
	EntryCount        int32
	MapCount          int32
	ServedCount       int32
	SubjectCount      int32
	LinkOpenCount     int32
	LinkBuildingCount int32
	NameCount         int32
	Networks          []*DeviceRemoteBridgeNetworkStatus
}

// One forwarding role's fields as the device RPC carries them.
//
//gomobile:noexport
type DeviceRemoteBridgeNetworkStatus struct {
	NetworkId                  connect.Id
	Hosting                    bool
	Posture                    string
	KeySet                     string
	Lapsed                     bool
	ForwardClientExpiresMillis int64
	TipN                       int64
	CompiledN                  int64
	Alarms                     []string
	CompileCount               int64
	CompileErrorCount          int64
	CompileRefusedCount        int64
	HeadAlarm                  string
	HeadAlarmMillis            int64
	HeadAlarmN                 int64
	HeadAlarmOtherN            int64
}

// The RPC's copy of status, nil for nil.
func newDeviceRemoteBridgeStatus(status *BridgeStatus) *DeviceRemoteBridgeStatus {
	if status == nil {
		return nil
	}
	mirrored := &DeviceRemoteBridgeStatus{
		State:             status.State,
		HasDocument:       status.HasDocument,
		PinN:              status.PinN,
		Block:             status.Block,
		EntryCount:        status.EntryCount,
		MapCount:          status.MapCount,
		ServedCount:       status.ServedCount,
		SubjectCount:      status.SubjectCount,
		LinkOpenCount:     status.LinkOpenCount,
		LinkBuildingCount: status.LinkBuildingCount,
		NameCount:         status.NameCount,
	}
	if status.Addresses != nil {
		mirrored.Addresses = status.Addresses.getAll()
	}
	if status.Networks != nil {
		for _, network := range status.Networks.getAll() {
			if network == nil {
				continue
			}
			mirroredNetwork := &DeviceRemoteBridgeNetworkStatus{
				Hosting:                    network.Hosting,
				Posture:                    network.Posture,
				KeySet:                     network.KeySet,
				Lapsed:                     network.Lapsed,
				ForwardClientExpiresMillis: network.ForwardClientExpiresMillis,
				TipN:                       network.TipN,
				CompiledN:                  network.CompiledN,
				CompileCount:               network.CompileCount,
				CompileErrorCount:          network.CompileErrorCount,
				CompileRefusedCount:        network.CompileRefusedCount,
				HeadAlarm:                  network.HeadAlarm,
				HeadAlarmMillis:            network.HeadAlarmMillis,
				HeadAlarmN:                 network.HeadAlarmN,
				HeadAlarmOtherN:            network.HeadAlarmOtherN,
			}
			if network.NetworkId != nil {
				mirroredNetwork.NetworkId = network.NetworkId.toConnectId()
			}
			if network.Alarms != nil {
				mirroredNetwork.Alarms = network.Alarms.getAll()
			}
			mirrored.Networks = append(mirrored.Networks, mirroredNetwork)
		}
	}
	return mirrored
}

// The bound status the RPC's copy carries, legacy's for nil.
func (self *DeviceRemoteBridgeStatus) toBridgeStatus() *BridgeStatus {
	if self == nil {
		return bridgeStatusOf(sidecardevice.BridgeStatus{State: sidecardevice.PostureLegacy}, nil)
	}
	status := &BridgeStatus{
		State:             self.State,
		HasDocument:       self.HasDocument,
		PinN:              self.PinN,
		Block:             self.Block,
		Addresses:         stringListFromStrings(self.Addresses),
		EntryCount:        self.EntryCount,
		MapCount:          self.MapCount,
		ServedCount:       self.ServedCount,
		SubjectCount:      self.SubjectCount,
		LinkOpenCount:     self.LinkOpenCount,
		LinkBuildingCount: self.LinkBuildingCount,
		NameCount:         self.NameCount,
		Networks:          NewBridgeNetworkStatusList(),
	}
	for _, network := range self.Networks {
		if network == nil {
			continue
		}
		status.Networks.Add(&BridgeNetworkStatus{
			NetworkId:                  newId([16]byte(network.NetworkId)),
			Hosting:                    network.Hosting,
			Posture:                    network.Posture,
			KeySet:                     network.KeySet,
			Lapsed:                     network.Lapsed,
			ForwardClientExpiresMillis: network.ForwardClientExpiresMillis,
			TipN:                       network.TipN,
			CompiledN:                  network.CompiledN,
			Alarms:                     stringListFromStrings(network.Alarms),
			CompileCount:               network.CompileCount,
			CompileErrorCount:          network.CompileErrorCount,
			CompileRefusedCount:        network.CompileRefusedCount,
			HeadAlarm:                  network.HeadAlarm,
			HeadAlarmMillis:            network.HeadAlarmMillis,
			HeadAlarmN:                 network.HeadAlarmN,
			HeadAlarmOtherN:            network.HeadAlarmOtherN,
		})
	}
	return status
}

// The device RPC's GetBridgeStatus.
func (self *DeviceLocalRpc) GetBridgeStatus(_ RpcNoArg, status **DeviceRemoteBridgeStatus) error {
	*status = newDeviceRemoteBridgeStatus(self.deviceLocal.GetBridgeStatus())
	return nil
}

// The device RPC's AddBridgeStatusChangeListener: the remote's listener id, heard through BridgeStatusChanged. A sync
// re-adds every id the remote holds, since each connection's rpc starts with none, and closing the service removes
// them, so a closed connection's rpc stays subscribed to nothing.
func (self *DeviceLocalRpc) AddBridgeStatusChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addBridgeStatusChangeListenerWithLock(listenerId)
	return nil
}

// Subscribes to the device once, for the first remote listener.
func (self *DeviceLocalRpc) addBridgeStatusChangeListenerWithLock(listenerId connect.Id) {
	if self.bridgeStatusChangeListenerIds == nil {
		self.bridgeStatusChangeListenerIds = map[connect.Id]bool{}
	}
	self.bridgeStatusChangeListenerIds[listenerId] = true
	if self.bridgeStatusChangeListenerSub == nil {
		self.bridgeStatusChangeListenerSub = self.deviceLocal.AddBridgeStatusChangeListener(self)
	}
}

// The device RPC's RemoveBridgeStatusChangeListener.
func (self *DeviceLocalRpc) RemoveBridgeStatusChangeListener(listenerId connect.Id, _ RpcVoid) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeBridgeStatusChangeListenerWithLock(listenerId)
	return nil
}

// Unsubscribes from the device with the last remote listener.
func (self *DeviceLocalRpc) removeBridgeStatusChangeListenerWithLock(listenerId connect.Id) {
	delete(self.bridgeStatusChangeListenerIds, listenerId)
	if len(self.bridgeStatusChangeListenerIds) == 0 && self.bridgeStatusChangeListenerSub != nil {
		self.bridgeStatusChangeListenerSub.Close()
		self.bridgeStatusChangeListenerSub = nil
	}
}

// BridgeStatusChangeListener: the status to the remote, an async coalescing reverse notification.
func (self *DeviceLocalRpc) BridgeStatusChanged(status *BridgeStatus) {
	self.reverseNotify("DeviceRemoteRpc.BridgeStatusChanged", newDeviceRemoteBridgeStatus(status))
}

// The device RPC's reverse BridgeStatusChanged, to the remote's listeners.
func (self *DeviceRemoteRpc) BridgeStatusChanged(status *DeviceRemoteBridgeStatus, _ RpcVoid) error {
	self.dispatch(func() {
		self.deviceRemote.bridgeStatusChanged(status.toBridgeStatus())
	})
	return nil
}

// The status the VPN component's process reports, legacy's while it cannot answer or is too old to know the call.
func (self *DeviceRemote) GetBridgeStatus() *BridgeStatus {
	status, err := deviceRemoteForwardingCall[*DeviceRemoteBridgeStatus](self, "DeviceLocalRpc.GetBridgeStatus", RpcNoArg(0))
	if err != nil {
		return bridgeStatusOf(sidecardevice.BridgeStatus{State: sidecardevice.PostureLegacy}, nil)
	}
	return status.toBridgeStatus()
}

// Calls listener with each status the VPN component's process sends, across reconnects, each sync re-adding it with
// the current status; an older process sends none.
func (self *DeviceRemote) AddBridgeStatusChangeListener(listener BridgeStatusChangeListener) Sub {
	self.stateLock.Lock()
	if self.bridgeStatusChangeListeners == nil {
		self.bridgeStatusChangeListeners = map[connect.Id]BridgeStatusChangeListener{}
	}
	self.stateLock.Unlock()
	return addCompatibleListener(
		self,
		listener,
		self.bridgeStatusChangeListeners,
		"DeviceLocalRpc.AddBridgeStatusChangeListener",
		"DeviceLocalRpc.RemoveBridgeStatusChangeListener",
	)
}

// Tells the remote's listeners.
func (self *DeviceRemote) bridgeStatusChanged(status *BridgeStatus) {
	self.stateLock.Lock()
	listeners := []BridgeStatusChangeListener{}
	for _, listener := range self.bridgeStatusChangeListeners {
		listeners = append(listeners, listener)
	}
	self.stateLock.Unlock()
	for _, listener := range listeners {
		trace(func() {
			listener.BridgeStatusChanged(status)
		})
	}
}

// One prefix the tun owner routes into the tun (§17.4). json in the C ABI and js.
type BridgeRoute struct {
	// CIDR text.
	Prefix string `json:"prefix"`
}

// The prefixes to route into the tun.
type BridgeRouteList struct {
	exportedList[*BridgeRoute]
}

// An empty list.
func NewBridgeRouteList() *BridgeRouteList {
	return &BridgeRouteList{
		exportedList: *newExportedList[*BridgeRoute](),
	}
}

// The prefixes the tun owner routes into the tun (§18.6, D-BRIDGE-12): the held bridge's, or the router's over every
// forwarding role; per-peer /32s while SetBridgeLocalAddresses has given none; empty in legacy. Its listener is
// AddBridgeStatusChangeListener's.
func (self *DeviceLocal) GetBridgeRoutes() *BridgeRouteList {
	routes := NewBridgeRouteList()
	for _, prefix := range self.bridgeRoutesSnapshot() {
		routes.Add(&BridgeRoute{Prefix: prefix.String()})
	}
	return routes
}

// The tun owner's interface, gateway and resolver addresses from the platform's routes and DNS settings, on each path
// change (§18.6, §17.4), which every role's routes carve; nil or empty where enumeration is restricted, which keeps
// per-peer /32s. An address that does not parse is dropped.
func (self *DeviceLocal) SetBridgeLocalAddresses(addresses *StringList) {
	trace(func() {
		if self.hostedIncompatibleGuarded("SetBridgeLocalAddresses") || self.bridgeOwner == nil {
			return
		}
		parsed := []netip.Addr{}
		if addresses != nil {
			for _, address := range addresses.getAll() {
				if addr, err := netip.ParseAddr(address); err == nil {
					parsed = append(parsed, addr.Unmap())
				}
			}
		}
		self.bridgeOwner.setLocalAddresses(parsed)
	})
}

// Whether an app mints its device client acl_enforcing now (§18.6's Rules, AuthNetworkClientArgs.AclEnforcing): once
// the forwarding role of the device client's network has left legacy, so its resident's team hints reach a role that
// follows them; never before, which keeps an older or unset role's client legacy.
func (self *BridgeStatus) AclEnforcingMint() bool {
	if self == nil || self.Networks == nil {
		return false
	}
	for _, network := range self.Networks.getAll() {
		if network != nil && network.Hosting && network.Posture != sidecardevice.PostureLegacy.String() {
			return true
		}
	}
	return false
}
