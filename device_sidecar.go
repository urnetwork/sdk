// The keyless device role in DeviceLocal (§18.6, D-SDK-1, D-SDK-13): deviceBridge, the seam device.Bridge
// (sidecar/device, D-SDK-20) satisfies, and the owner that holds one past legacy. Untagged, since the tunnel links it,
// so it names no root type and imports only sidecar/device and protocol/bridge of the sidecar module; the seam is
// unexported, so no connect or netip type reaches a binding.
//
//	off      DeviceLocalSettings.EnableBridge false, the default: no store is opened, every bridge call refuses, and a
//	         send costs one nil check
//	hosted   a HostedIncompatible device builds no bridge and refuses every bridge call (D-SDK-12), and so does a
//	         browser's (D-BRIDGE-11, device_sidecar_exit.go)
//	legacy   no sidecar role has enrolled for the device's network here: no device.Bridge, today's route
//	past     a device.Bridge, restored from the posture store while the device is built, so what was in force is in
//	         force before the first packet (DP1); Route runs on both send paths before the mux, so no bridged :53 or :80
//	         is claimed, and each mux holds its zone beside the blocker
//
// The posture moves only through setBridgePending, setBridgeState and applyBridgeDeltas, which SDK-M8-07's channel
// calls, and after each the owner holds the bridge while it is past legacy and closes it once this sidecar's own
// sign-out wipes it back. A bridge's change callback re-reads its routes and keeps its zone on the mux. Each hold also
// moves the bridge's hook onto the exit NAT, and the restored bridge's block and routes join the tunnel address's avoid
// list (device_sidecar_exit.go).
//
// Not here yet, and closed meanwhile: the bridge has no BridgeHost and no signer until SDK-M8-08's
// DeviceLocal.NewBridgeLink and delegations, so every link entry answers with a reject; the channel and the status
// mirror are SDK-M8-07's.

package sdk

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"runtime"
	"sync"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	sidecardevice "github.com/urnetwork/sidecar/device"
	"github.com/urnetwork/sidecar/protocol/bridge"
)

// §18.6's deviceBridge, with RouteBurst beside Route for the batch send path. SetState and ApplyDeltas carry the log
// that vouches for what they apply, as device.Posture takes it, while question sdk-m4-01-state-proof is open; §18.6
// writes them with the state and the deltas alone. Nil keeps today's behavior.
type deviceBridge interface {
	// A link, the reject client, the resolver, or nil for the default route. Lock- and allocation-free.
	Route(packet []byte) connect.UserNatClient
	// Routes a burst as Route routes each packet, calling send once per run one route takes, nil being the default,
	// keeping flow order; an all-default burst reaches send unchanged. Borrows the burst, and hands every packet to
	// exactly one send.
	RouteBurst(packets [][]byte, send func(client connect.UserNatClient, packets [][]byte))
	// The provider hook (§4's C5, §17.6), the same value on every call; BRIDGE-M4-21 installs it.
	EgressHook() connect.EgressHook
	// What the tun owner routes into the tun (§17.4, D-BRIDGE-12).
	Routes() []netip.Prefix
	// §12.9's private names of the state in force, nil before a state.
	LocalZone() connect.LocalZone
	// legacy -> pending, once the sidecar role has a sidecar_id.
	SetPending(pending *bridge.Pending) error
	// §17.8's state from this device's sidecar role, with the log that vouches for it; versions only rise.
	SetState(state *bridge.BridgeState, log *sidecardevice.PostureLog) error
	// Projection deltas between states, verified offline against the anchor (§8.9).
	ApplyDeltas(log *sidecardevice.PostureLog) error
	// What the bridge has in force and what it found.
	Status() sidecardevice.BridgeStatus
	// Calls callback after each publish; the returned function removes it.
	AddChangeCallback(callback func()) func()
}

// A deviceBridge its owner builds and ends: where its rejects and resolver replies enter the tun, and its close.
type deviceBridgeOwned interface {
	deviceBridge
	SetReceivePacket(receive connect.ReceivePacketFunction)
	Close()
}

var _ deviceBridgeOwned = (*sidecardevice.Bridge)(nil)

// Builds a device role's bridge over store, restoring its posture before it returns.
type deviceBridgeConstructor func(ctx context.Context, store sidecardevice.PostureStore, settings *sidecardevice.BridgeSettings) (deviceBridgeOwned, error)

// device.NewBridge with no BridgeHost, so no link is built (the file's comment).
func newDeviceBridge(ctx context.Context, store sidecardevice.PostureStore, settings *sidecardevice.BridgeSettings) (deviceBridgeOwned, error) {
	bridge, err := sidecardevice.NewBridge(ctx, store, nil, settings)
	if err != nil {
		return nil, err
	}
	return bridge, nil
}

// Why a device runs no device role, or refuses a bridge call.
var (
	errDeviceBridgeOff       = errors.New("the device role is off: DeviceLocalSettings.EnableBridge is false")
	errDeviceBridgeHosted    = errors.New("a hosted device runs no device role (D-SDK-12)")
	errDeviceBridgeNoNetwork = errors.New("the device role needs the network of the device's client, and its credential names none")
	errDeviceBridgeNetwork   = errors.New("a Pending for another network than the device's client")
	errDeviceBridgeLegacy    = errors.New("the device role is legacy: a state or a delta needs a Pending first")
	errDeviceBridgeClosed    = errors.New("the device is closed")
)

// The device role's bridge settings for a device's: §17's desktop values, a phone's on a low-memory device (§17.5),
// the sdk exit NAT's table until C16, so an app device hosts, serves and maps nothing (D-SDK-3), and on iOS no link
// until §22.9 measures one there (owner decision 100). No signer and no binding fetch until SDK-M8-08.
func deviceLocalBridgeSettings(settings *DeviceLocalSettings) *sidecardevice.BridgeSettings {
	var bridgeSettings *sidecardevice.BridgeSettings
	if settings.LowMemoryProfile {
		bridgeSettings = sidecardevice.MobileBridgeSettings()
	} else {
		bridgeSettings = sidecardevice.DefaultBridgeSettings()
	}
	bridgeSettings.PolicyProfile = sidecardevice.BridgePolicyProfileSdkProvider
	if runtime.GOOS == "ios" {
		bridgeSettings.LinkCountMax = 0
	}
	return bridgeSettings
}

// The zone a mux holds while a bridge is held: the bridge's LocalZone at each query, so a state entering or leaving
// enforcing reaches every mux at once, and no install can hold an older state's answer than the bridge has.
type deviceBridgeZone struct {
	bridge deviceBridge
}

var _ connect.LocalZone = (*deviceBridgeZone)(nil)

// connect.LocalZone: the bridge's answer, or not owned while it has no state's names.
func (self *deviceBridgeZone) Answer(host string, qtype uint16) ([]netip.Addr, uint32, bool) {
	zone := self.bridge.LocalZone()
	if zone == nil {
		return nil, 0, false
	}
	return zone.Answer(host, qtype)
}

// One DeviceLocal's device role (the file's comment). Its operationLock serializes what builds, feeds and closes the
// bridge, held across those calls out, and is taken before the device's stateLock, never after; the device calls in
// holding none of its own locks.
type deviceLocalBridgeOwner struct {
	ctx context.Context
	// why the device runs no device role; nil when it runs one
	refusal   error
	store     sidecardevice.PostureStore
	networkId connect.Id
	settings  *sidecardevice.BridgeSettings
	newBridge deviceBridgeConstructor
	log       connect.Logger

	operationLock sync.Mutex
	// past legacy, the bridge; nil in legacy, off and once closed
	bridge               deviceBridgeOwned
	removeChangeCallback func()
	// the device's, set once by attach, each called with operationLock and none of the device's locks
	hold    func(bridge deviceBridge)
	changed func(bridge deviceBridge)
	receive connect.ReceivePacketFunction
	closed  bool
}

// The device role of a device built on settings for the network networkIdText names, its posture restored from the
// network space's storage before this returns. A device that runs none, being off, hosted or of no network, gets an
// owner that refuses every call; one that runs one with no absolute storage path is refused, since a posture it could
// not keep would come back legacy after a restart.
func newDeviceLocalBridgeOwner(
	ctx context.Context,
	networkSpace *NetworkSpace,
	networkIdText string,
	settings *DeviceLocalSettings,
	log connect.Logger,
) (*deviceLocalBridgeOwner, error) {
	self := &deviceLocalBridgeOwner{
		ctx:       ctx,
		settings:  deviceLocalBridgeSettings(settings),
		newBridge: settings.testingNewDeviceBridge,
		log:       log,
	}
	if self.newBridge == nil {
		self.newBridge = newDeviceBridge
	}
	if refusal := deviceBridgeRefusal(settings, runtime.GOOS); refusal != nil {
		if refusal != errDeviceBridgeOff {
			log.Infof("[device]the device role is not run: %v\n", refusal)
		}
		self.refusal = refusal
		return self, nil
	}
	networkId, err := connect.ParseId(networkIdText)
	if err != nil {
		self.refusal = errDeviceBridgeNoNetwork
		return self, nil
	}
	self.networkId = networkId
	store, err := newSdkPostureStore(networkSpace.storagePath, networkId)
	if err != nil {
		return nil, err
	}
	self.store = store
	if err := self.restore(); err != nil {
		return nil, err
	}
	return self, nil
}

// Builds the bridge from the store, keeping it past legacy and closing it in legacy.
func (self *deviceLocalBridgeOwner) restore() error {
	self.operationLock.Lock()
	defer self.operationLock.Unlock()
	if err := self.buildWithLock(); err != nil {
		return err
	}
	self.settleWithLock()
	return nil
}

// The device's half: where the bridge is held and its change heard, and where its rejects and replies enter the tun.
// Called once, before the device is returned, so the restored bridge is in its first send route.
func (self *deviceLocalBridgeOwner) attach(
	hold func(bridge deviceBridge),
	changed func(bridge deviceBridge),
	receive connect.ReceivePacketFunction,
) {
	self.operationLock.Lock()
	defer self.operationLock.Unlock()
	self.hold = hold
	self.changed = changed
	self.receive = receive
	if self.bridge != nil {
		self.listenWithLock(self.bridge)
		self.hold(self.bridge)
	}
}

// A new bridge over the store, restored before this returns; heard from at once if the device has attached.
func (self *deviceLocalBridgeOwner) buildWithLock() error {
	bridge, err := self.newBridge(self.ctx, self.store, self.settings)
	if err != nil {
		return err
	}
	self.bridge = bridge
	if self.hold != nil {
		self.listenWithLock(bridge)
	}
	return nil
}

// Sends bridge's rejects and replies into the tun and hears each of its publishes.
func (self *deviceLocalBridgeOwner) listenWithLock(bridge deviceBridgeOwned) {
	bridge.SetReceivePacket(self.receive)
	changed := self.changed
	self.removeChangeCallback = bridge.AddChangeCallback(func() {
		changed(bridge)
	})
}

// After anything that may move the posture: the device holds the bridge past legacy, and in legacy, after this
// sidecar's own sign-out or a restore that found nothing, the bridge is dropped and closed.
func (self *deviceLocalBridgeOwner) settleWithLock() {
	bridge := self.bridge
	if bridge == nil {
		return
	}
	if bridge.Status().State != sidecardevice.PostureLegacy {
		if self.hold != nil {
			self.hold(bridge)
		}
		return
	}
	self.bridge = nil
	if self.hold != nil {
		self.hold(nil)
	}
	if self.removeChangeCallback != nil {
		self.removeChangeCallback()
		self.removeChangeCallback = nil
	}
	bridge.Close()
}

// legacy -> pending: builds the bridge in legacy, then hands it the Pending, which must name the device's network.
func (self *deviceLocalBridgeOwner) setPending(pending *bridge.Pending) error {
	if self.refusal != nil {
		return self.refusal
	}
	if !bytes.Equal(pending.GetNetworkId(), self.networkId[:]) {
		return errDeviceBridgeNetwork
	}
	self.operationLock.Lock()
	defer self.operationLock.Unlock()
	if self.closed {
		return errDeviceBridgeClosed
	}
	if self.bridge == nil {
		if err := self.buildWithLock(); err != nil {
			return err
		}
	}
	err := self.bridge.SetPending(pending)
	self.settleWithLock()
	return err
}

// A state past legacy; in legacy there is nothing to apply it to.
func (self *deviceLocalBridgeOwner) setState(state *bridge.BridgeState, log *sidecardevice.PostureLog) error {
	return self.apply(func(bridge deviceBridge) error {
		return bridge.SetState(state, log)
	})
}

// Deltas past legacy; in legacy there is nothing to apply them to.
func (self *deviceLocalBridgeOwner) applyDeltas(log *sidecardevice.PostureLog) error {
	return self.apply(func(bridge deviceBridge) error {
		return bridge.ApplyDeltas(log)
	})
}

// Runs call on the bridge past legacy, then settles.
func (self *deviceLocalBridgeOwner) apply(call func(bridge deviceBridge) error) error {
	if self.refusal != nil {
		return self.refusal
	}
	self.operationLock.Lock()
	defer self.operationLock.Unlock()
	if self.closed {
		return errDeviceBridgeClosed
	}
	if self.bridge == nil {
		return errDeviceBridgeLegacy
	}
	err := call(self.bridge)
	self.settleWithLock()
	return err
}

// The bridge's status, or legacy's with no bridge.
func (self *deviceLocalBridgeOwner) status() (sidecardevice.BridgeStatus, error) {
	if self.refusal != nil {
		return sidecardevice.BridgeStatus{}, self.refusal
	}
	self.operationLock.Lock()
	defer self.operationLock.Unlock()
	if self.closed {
		return sidecardevice.BridgeStatus{}, errDeviceBridgeClosed
	}
	if self.bridge == nil {
		return sidecardevice.BridgeStatus{State: sidecardevice.PostureLegacy}, nil
	}
	return self.bridge.Status(), nil
}

// Ends the bridge and refuses every later call. The device has already let go of it. Waits for the bridge's run, so
// call it from the device's lifecycle worker, never from inside a callback.
func (self *deviceLocalBridgeOwner) close() {
	self.operationLock.Lock()
	defer self.operationLock.Unlock()
	self.closed = true
	if self.bridge == nil {
		return
	}
	if self.removeChangeCallback != nil {
		self.removeChangeCallback()
		self.removeChangeCallback = nil
	}
	self.bridge.Close()
	self.bridge = nil
}

// The device role's bridge splits the burst before the mux (sidecar §17.5's
// batches): in place, keeping flow order, each run reaching the route its
// bridge gives it, today's for nil. Takes every packet. A concrete
// device.Bridge is called directly, so the closure stays on the stack and a
// burst allocates nothing; another bridge, a test's, through the seam.
func (self *DeviceLocal) sendPacketsWithBridge(route *deviceLocalSendRoute, packets [][]byte, nativeIngress bool) int {
	if concreteBridge, ok := route.bridge.(*sidecardevice.Bridge); ok {
		sentPacketCount := 0
		concreteBridge.RouteBurst(packets, func(client connect.UserNatClient, runPackets [][]byte) {
			sentPacketCount += self.sendBridgeRun(route, client, runPackets, nativeIngress)
		})
		return sentPacketCount
	}
	sentPacketCount := 0
	route.bridge.RouteBurst(packets, func(client connect.UserNatClient, runPackets [][]byte) {
		sentPacketCount += self.sendBridgeRun(route, client, runPackets, nativeIngress)
	})
	return sentPacketCount
}

// Sends one run of a burst on the route the bridge gave it, today's for nil.
// Takes every packet: the route takes those it accepts, and a packet it
// refuses, as a building link does, is returned here. Returns how many were
// accepted.
func (self *DeviceLocal) sendBridgeRun(route *deviceLocalSendRoute, client connect.UserNatClient, packets [][]byte, nativeIngress bool) int {
	if client == nil {
		return self.sendPacketsOnRoute(route, packets, nativeIngress)
	}
	source := connect.SourceId(self.clientId)
	if nativeIngress {
		if nativeClient, ok := client.(connect.UserNatNativeBatchClient); ok {
			return nativeClient.SendNativePacketBatch(source, protocol.ProvideMode_Network, packets, self.settings.SendTimeout)
		}
	}
	if batchClient, ok := client.(connect.UserNatBatchClient); ok {
		return batchClient.SendPacketBatch(source, protocol.ProvideMode_Network, packets, self.settings.SendTimeout)
	}
	sentPacketCount := 0
	for _, packet := range packets {
		if client.SendPacket(source, protocol.ProvideMode_Network, packet, self.settings.SendTimeout) {
			sentPacketCount += 1
		} else {
			connect.MessagePoolReturn(packet)
		}
	}
	return sentPacketCount
}

// Holds bridge, nil for none, in the send route, each mux's zone and the routes, from now on; nothing once closed.
// Called by the device role's owner under its lock.
func (self *DeviceLocal) holdBridge(bridge deviceBridge) {
	var routes []netip.Prefix
	if bridge != nil {
		routes = bridge.Routes()
	}
	readNumber := self.bridgeReadNumber.Add(1)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return
	}
	if self.bridge != bridge {
		self.bridge = bridge
		self.bridgeZone = nil
		if bridge != nil {
			self.bridgeZone = &deviceBridgeZone{bridge: bridge}
		}
		self.updateSendRouteWithLock()
	}
	self.setBridgeRoutesWithLock(readNumber, routes)
	self.applyBridgeZoneWithLock()
}

// A held bridge's change callback, on the goroutine that published: re-reads its routes and puts its zone back on the
// mux. A bridge no longer held changes nothing.
func (self *DeviceLocal) bridgeChanged(bridge deviceBridge) {
	routes := bridge.Routes()
	readNumber := self.bridgeReadNumber.Add(1)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed || self.bridge != bridge {
		return
	}
	self.setBridgeRoutesWithLock(readNumber, routes)
	self.applyBridgeZoneWithLock()
}

// Keeps routes unless a later read is already kept, since a publish's callback and the owner can read them in either
// order.
func (self *DeviceLocal) setBridgeRoutesWithLock(readNumber uint64, routes []netip.Prefix) {
	if readNumber < self.bridgeRoutesReadNumber {
		return
	}
	self.bridgeRoutesReadNumber = readNumber
	self.bridgeRoutes = routes
}

// Puts the held bridge's zone, or none, on the live mux, beside its blocker (§18.6's Lifecycle, C14). A mux rebuild
// puts it on the new mux in the same place, since a new mux holds no zone.
func (self *DeviceLocal) applyBridgeZoneWithLock() {
	if self.upgradeMux != nil {
		self.upgradeMux.SetLocalZone(self.bridgeZone)
	}
}

// The prefixes the held bridge exports for the tun owner, nil with none; SDK-M8-07 binds them.
func (self *DeviceLocal) bridgeRoutesSnapshot() []netip.Prefix {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.bridgeRoutes
}

// legacy -> pending (§18.6's Channel): the sidecar role's Pending, once it has a sidecar_id. Builds the bridge past
// legacy; SDK-M8-07's channel calls it. A hosted device refuses it.
func (self *DeviceLocal) setBridgePending(pending *bridge.Pending) error {
	if self.hostedIncompatibleGuarded("setBridgePending") {
		return errDeviceBridgeHosted
	}
	return self.bridgeOwner.setPending(pending)
}

// The sidecar role's verified-config state with the log that vouches for it (§18.6's Channel, §17.8). A hosted device
// refuses it.
func (self *DeviceLocal) setBridgeState(state *bridge.BridgeState, log *sidecardevice.PostureLog) error {
	if self.hostedIncompatibleGuarded("setBridgeState") {
		return errDeviceBridgeHosted
	}
	return self.bridgeOwner.setState(state, log)
}

// Projection deltas between states (§18.6's Channel, §8.9). A hosted device refuses them.
func (self *DeviceLocal) applyBridgeDeltas(log *sidecardevice.PostureLog) error {
	if self.hostedIncompatibleGuarded("applyBridgeDeltas") {
		return errDeviceBridgeHosted
	}
	return self.bridgeOwner.applyDeltas(log)
}

// The device role's status (§18.6's Status), legacy's with no bridge; SDK-M8-07 mirrors it.
func (self *DeviceLocal) bridgeStatus() (sidecardevice.BridgeStatus, error) {
	return self.bridgeOwner.status()
}
