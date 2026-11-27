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
// The posture moves through setBridgePending, setBridgeState and applyBridgeDeltas, and on a VPN component through its
// own Forwarder (device_sidecar_forward.go), and after each the owner holds the bridge while it is past legacy and
// closes it once this sidecar's own sign-out wipes it back. A bridge's change callback re-reads its routes and keeps its
// zone on the mux. Each hold also moves the bridge's hook onto the exit NAT, and the restored bridge's block and routes
// join the tunnel address's avoid list (device_sidecar_exit.go).
//
// One role per network (owner decision 197, §18.6's Setup): the device client's network's is the primary, and each
// other network a forwarding hand-off names gets a role of its own, with its own store, posture and bridge. While any
// other role has a bridge the send route holds a router over every role's bridge in the order the networks joined:
// a packet goes to the first role whose routes hold its destination, so where two networks' blocks collide the
// first-joined network's wins (§17.4), and every other packet to the primary, whose egress rules decide it. Only the
// primary's hook goes on the exit NAT, so an app device hosts nothing outside its device client's network (D-SDK-3).
//
// Links: every role's bridge builds them through the device (DeviceLocal.NewBridgeLink, device_sidecar_host.go) on its
// network's forwarding client, signed by its Forwarder (the role's LinkSign), which the device binds as host once it
// is whole; a link asked for before that, or on a device with no forwarding role, is refused, so it fails closed.

package sdk

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"

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
	// Reads the clock again at once when the device wakes or its clock changes, never blocking (owner decision 789,
	// sidecar_clock_check.go): device.Bridge.CheckClock, and a router's asks each role's.
	CheckClock()
}

// A deviceBridge its owner builds and ends: where its rejects and resolver replies enter the tun, its close, and the
// posture's fork hold and decision, which a VPN component's Forwarder reads (sidecar.ForwarderSink).
type deviceBridgeOwned interface {
	deviceBridge
	SetReceivePacket(receive connect.ReceivePacketFunction)
	Close()
	SetFork(fork bool) error
	Decision() sidecardevice.PostureDecision
	// The posture's decision at the clock's now, where its overdue alarm is raised (device.Bridge.DecisionNow).
	DecisionNow() sidecardevice.PostureDecision
	SetLocalAddresses(addresses []netip.Addr)
	// Signs each live window client of the shares' links again, once a share's signer changed (§17.10).
	ResignShares(shareIds []connect.Id) error
}

var _ deviceBridgeOwned = (*sidecardevice.Bridge)(nil)

// Builds a device role's bridge over store, restoring its posture before it returns.
type deviceBridgeConstructor func(ctx context.Context, store sidecardevice.PostureStore, settings *sidecardevice.BridgeSettings) (deviceBridgeOwned, error)

// device.NewBridge with no BridgeHost, so no link is built.
func newDeviceBridge(ctx context.Context, store sidecardevice.PostureStore, settings *sidecardevice.BridgeSettings) (deviceBridgeOwned, error) {
	return newDeviceBridgeWithHost(ctx, store, nil, settings)
}

// device.NewBridge whose links host builds (the file's comment).
func newDeviceBridgeWithHost(ctx context.Context, store sidecardevice.PostureStore, host sidecardevice.BridgeHost, settings *sidecardevice.BridgeSettings) (deviceBridgeOwned, error) {
	bridge, err := sidecardevice.NewBridge(ctx, store, host, settings)
	if err != nil {
		return nil, err
	}
	return bridge, nil
}

// No device builds a role's links yet: it is not whole, or runs no forwarding role.
var errDeviceBridgeLinkUnbound = errors.New("no device builds this bridge's links yet")

// The BridgeHost every role's bridge is built with, before the device that builds its links exists: the device binds
// itself once it is whole (DeviceLocal.NewBridgeLink), and until then every build is refused, so a link fails closed.
// Safe for concurrent use.
type deviceBridgeLinkHost struct {
	bound atomic.Pointer[deviceBridgeLinkHostBinding]
}

// The host bound.
type deviceBridgeLinkHostBinding struct {
	host sidecardevice.BridgeHost
}

var _ sidecardevice.BridgeHost = (*deviceBridgeLinkHost)(nil)

// Binds the host every later build goes to.
func (self *deviceBridgeLinkHost) bind(host sidecardevice.BridgeHost) {
	self.bound.Store(&deviceBridgeLinkHostBinding{host: host})
}

// device.BridgeHost: the bound host's link, or a refusal while none is bound.
func (self *deviceBridgeLinkHost) NewBridgeLink(ctx context.Context, target connect.Id, subject connect.Id, shareAliases []netip.Prefix,
	sign func(windowClientId connect.Id, windowClientPublicKey []byte, notAfter time.Time) ([]byte, error),
) (connect.UserNatClient, error) {
	binding := self.bound.Load()
	if binding == nil || binding.host == nil {
		return nil, errors.Join(sidecardevice.ErrBridgeLinkRefused, errDeviceBridgeLinkUnbound)
	}
	return binding.host.NewBridgeLink(ctx, target, subject, shareAliases, sign)
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

// Whether a device built on settings hosts in its device client's network (DeviceLocalSettings.EnableBridgeHosting;
// BRIDGE-M8-01): only one that runs the device role, so never an off, hosted or browser's device.
func deviceBridgeHosting(settings *DeviceLocalSettings) bool {
	return deviceBridgeHostingFor(settings, runtime.GOOS)
}

// deviceBridgeHosting for a device of goos.
func deviceBridgeHostingFor(settings *DeviceLocalSettings, goos string) bool {
	return settings.EnableBridgeHosting && deviceBridgeRefusal(settings, goos) == nil
}

// The settings of the bridge the device client's network's forwarding role restores into the primary (usePrimaryStore),
// a copy of settings: the sdk host's table where the device hosts, since that role's forwarding Delegation names this
// device as host and senders link to it by that Delegation's host client key (§17.9's target row, D-BRIDGE-5); else
// the sdk provider's. The M4 seam's primary and every other network's role keep the sdk provider's, so an app device
// hosts nothing outside its device client's network and nothing before its forwarding is set up there.
func deviceBridgeHostingSettings(settings *sidecardevice.BridgeSettings, hosting bool) *sidecardevice.BridgeSettings {
	copied := *settings
	if hosting {
		copied.PolicyProfile = sidecardevice.BridgePolicyProfileSdkHost
	}
	return &copied
}

// The device role's bridge settings for a device's: §17's desktop values or a phone's, LinkCountMax per platform and
// the egress spec off unless EnableBridgeEgress (device_sidecar_egress.go); the sdk exit NAT's table until C16, so an
// app device hosts, serves and maps nothing (D-SDK-3) until its device client's network's forwarding role restores a
// host's (deviceBridgeHostingSettings). No signer and no binding fetch: each forwarding role's settings add its own
// (device_sidecar_host.go), so a bridge no role's settings built builds no link.
func deviceLocalBridgeSettings(settings *DeviceLocalSettings) *sidecardevice.BridgeSettings {
	return deviceLocalBridgeSettingsFor(settings, runtime.GOOS)
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
	// the other networks' roles, in the order they joined (the file's comment)
	others []*deviceLocalBridgeRole
	// the router the send route holds while another role has a bridge, nil otherwise; read by the bridges' callbacks
	router atomic.Pointer[deviceBridgeRouter]
	// the device's, set once by attach, each called with operationLock and none of the device's locks
	hold    func(bridge deviceBridge)
	changed func(bridge deviceBridge)
	receive connect.ReceivePacketFunction
	closed  bool
	// the order the networks with a forwarding role joined in (device_sidecar_forward.go), nil before forwarding runs
	joinOrder func() []connect.Id
	// the VPN component's forwarding roles, nil on a device given no forwarding key store or directory
	forwarding *deviceLocalForwardingHost
	// the tun owner's local addresses, which every bridge built later takes too
	localAddresses []netip.Addr
	// what every bridge builds its links through, which the device binds once it is whole
	linkHost *deviceBridgeLinkHost
	// whether the device hosts in its client's network once that network's forwarding role takes the primary
	// (deviceBridgeHosting)
	hosting bool
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
		linkHost:  &deviceBridgeLinkHost{},
		hosting:   deviceBridgeHosting(settings),
	}
	if self.newBridge == nil {
		linkHost := self.linkHost
		self.newBridge = func(ctx context.Context, store sidecardevice.PostureStore, settings *sidecardevice.BridgeSettings) (deviceBridgeOwned, error) {
			return newDeviceBridgeWithHost(ctx, store, linkHost, settings)
		}
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
	// the VPN component's forwarding roles (device_sidecar_forward.go), each network's posture restored from its
	// sealed store here too, before the first packet
	self.forwarding = newDeviceLocalForwardingHost(settings, networkId, self.settings, log)
	if self.forwarding != nil {
		if err := self.forwarding.restore(self); err != nil {
			return nil, err
		}
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
	}
	for _, role := range self.others {
		if role.bridge != nil {
			role.removeChangeCallback = self.listenRoleWithLock(role.bridge)
		}
	}
	if self.bridge != nil || 0 < len(self.others) {
		self.holdWithLock()
	}
}

// A new bridge over the store, restored before this returns; heard from at once if the device has attached.
func (self *deviceLocalBridgeOwner) buildWithLock() error {
	bridge, err := self.newBridge(self.ctx, self.store, self.settings)
	if err != nil {
		return err
	}
	if self.localAddresses != nil {
		bridge.SetLocalAddresses(self.localAddresses)
	}
	self.bridge = bridge
	if self.hold != nil {
		self.listenWithLock(bridge)
	}
	return nil
}

// Sends bridge's rejects and replies into the tun and hears each of its publishes, as the primary's.
func (self *deviceLocalBridgeOwner) listenWithLock(bridge deviceBridgeOwned) {
	self.removeChangeCallback = self.listenRoleWithLock(bridge)
}

// Sends bridge's rejects and replies into the tun and hears each of its publishes, answering what stops the hearing.
func (self *deviceLocalBridgeOwner) listenRoleWithLock(bridge deviceBridgeOwned) func() {
	bridge.SetReceivePacket(self.receive)
	return bridge.AddChangeCallback(func() {
		self.roleChanged(bridge)
	})
}

// A role's bridge published: while a router is held its table is read again and the device hears the router, else it
// hears the bridge, which it ignores unless it holds it.
func (self *deviceLocalBridgeOwner) roleChanged(bridge deviceBridgeOwned) {
	if router := self.router.Load(); router != nil {
		router.refresh()
		self.changed(router)
		return
	}
	self.changed(bridge)
}

// After anything that may move a posture: the device holds what is past legacy, the primary's bridge or a router over
// every role's, and a bridge back in legacy, after its sidecar's own sign-out or a restore that found nothing, is let
// go and then closed.
func (self *deviceLocalBridgeOwner) settleWithLock() {
	closing := []deviceBridgeOwned{}
	removing := []func(){}
	if bridge := self.bridge; bridge != nil && bridge.Status().State == sidecardevice.PostureLegacy {
		self.bridge = nil
		closing = append(closing, bridge)
		removing = append(removing, self.removeChangeCallback)
		self.removeChangeCallback = nil
	}
	for _, role := range self.others {
		if role.bridge != nil && role.bridge.Status().State == sidecardevice.PostureLegacy {
			closing = append(closing, role.bridge)
			removing = append(removing, role.removeChangeCallback)
			role.bridge, role.removeChangeCallback = nil, nil
		}
	}
	self.holdWithLock()
	for _, remove := range removing {
		if remove != nil {
			remove()
		}
	}
	for _, bridge := range closing {
		bridge.Close()
	}
}

// Hands the device what the send route holds now: nothing, the primary's bridge alone, or a router over every role
// with a bridge, kept the same while those bridges are the same, so a republish installs no hook again.
func (self *deviceLocalBridgeOwner) holdWithLock() {
	routerRoles := []deviceBridgeRouterRole{}
	for _, role := range self.others {
		if role.bridge != nil {
			routerRoles = append(routerRoles, deviceBridgeRouterRole{networkId: role.networkId, bridge: role.bridge})
		}
	}
	var held deviceBridge
	switch {
	case len(routerRoles) == 0:
		self.router.Store(nil)
		if self.bridge != nil {
			held = self.bridge
		}
	default:
		if self.bridge != nil {
			routerRoles = append(routerRoles, deviceBridgeRouterRole{networkId: self.networkId, bridge: self.bridge, primary: true})
		}
		slices.SortStableFunc(routerRoles, func(a deviceBridgeRouterRole, b deviceBridgeRouterRole) int {
			return self.joinIndex(a.networkId) - self.joinIndex(b.networkId)
		})
		router := self.router.Load()
		if router == nil || !router.sameRoles(routerRoles) {
			router = newDeviceBridgeRouter(routerRoles)
			self.router.Store(router)
		}
		router.refresh()
		held = router
	}
	if self.hold != nil {
		self.hold(held)
	}
}

// Where networkId joined among the roles, the primary first when the order names none.
func (self *deviceLocalBridgeOwner) joinIndex(networkId connect.Id) int {
	if self.joinOrder != nil {
		if index := slices.Index(self.joinOrder(), networkId); 0 <= index {
			return index + 1
		}
	}
	if networkId == self.networkId {
		return 0
	}
	return 1 << 20
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
	// the Forwarders first, outside the lock, since each calls into its role through it until it stops
	if self.forwarding != nil {
		self.forwarding.close()
	}
	self.operationLock.Lock()
	defer self.operationLock.Unlock()
	self.closed = true
	self.router.Store(nil)
	for _, role := range self.others {
		if role.removeChangeCallback != nil {
			role.removeChangeCallback()
			role.removeChangeCallback = nil
		}
		if role.bridge != nil {
			role.bridge.Close()
			role.bridge = nil
		}
	}
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
// Called by the device role's owner under its lock, after every call that may move a posture. The status listeners are
// told only of a change, the held bridge, and with it its zone, or the routes kept: a hold that changes nothing tells
// nobody, as a held bridge's own publishes tell them through bridgeChanged (QD-1437; CODESTYLE's notify on an actual
// change).
func (self *DeviceLocal) holdBridge(bridge deviceBridge) {
	var routes []netip.Prefix
	if bridge != nil {
		routes = bridge.Routes()
	}
	readNumber := self.bridgeReadNumber.Add(1)
	changed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return false
		}
		changed := false
		if self.bridge != bridge {
			self.bridge = bridge
			self.bridgeZone = nil
			if bridge != nil {
				self.bridgeZone = &deviceBridgeZone{bridge: bridge}
			}
			self.updateSendRouteWithLock()
			changed = true
		}
		previousRoutes := self.bridgeRoutes
		self.setBridgeRoutesWithLock(readNumber, routes)
		if !slices.Equal(previousRoutes, self.bridgeRoutes) {
			changed = true
		}
		self.applyBridgeZoneWithLock()
		return changed
	}()
	// told once the lock is let go
	if changed {
		self.notifyBridgeStatus()
	}
}

// A held bridge's change callback, on the goroutine that published: re-reads its routes and puts its zone back on the
// mux. A bridge no longer held changes nothing.
func (self *DeviceLocal) bridgeChanged(bridge deviceBridge) {
	routes := bridge.Routes()
	readNumber := self.bridgeReadNumber.Add(1)
	held := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed || self.bridge != bridge {
			return false
		}
		self.setBridgeRoutesWithLock(readNumber, routes)
		self.applyBridgeZoneWithLock()
		return true
	}()
	if held {
		self.notifyBridgeStatus()
	}
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

// One network's role beside the device client's (the file's comment): its store and settings, which carry its own
// floor, and past legacy its bridge, which never takes the provider's hook.
type deviceLocalBridgeRole struct {
	networkId            connect.Id
	store                sidecardevice.PostureStore
	settings             *sidecardevice.BridgeSettings
	bridge               deviceBridgeOwned
	removeChangeCallback func()
}

// Replaces the primary's store and settings while it holds no bridge, so a VPN component's sealed store takes the
// device client's network's posture from its first Pending on (device_sidecar_forward.go), under the sdk host's table
// where the device hosts (deviceBridgeHostingSettings). False once a bridge stands, whose store and table stay.
func (self *deviceLocalBridgeOwner) usePrimaryStore(store sidecardevice.PostureStore, settings *sidecardevice.BridgeSettings) bool {
	self.operationLock.Lock()
	defer self.operationLock.Unlock()
	if self.refusal != nil || self.closed || self.bridge != nil {
		return false
	}
	self.store = store
	self.settings = deviceBridgeHostingSettings(settings, self.hosting)
	return true
}

// Adds networkId's role over store, restored before this returns: past legacy, its bridge is held at once. The device
// client's network is the primary's and takes no other role; a network already with one keeps it.
func (self *deviceLocalBridgeOwner) addRole(networkId connect.Id, store sidecardevice.PostureStore, settings *sidecardevice.BridgeSettings) error {
	if self.refusal != nil {
		return self.refusal
	}
	if networkId == self.networkId {
		return errDeviceBridgeNetwork
	}
	self.operationLock.Lock()
	defer self.operationLock.Unlock()
	if self.closed {
		return errDeviceBridgeClosed
	}
	if slices.ContainsFunc(self.others, func(role *deviceLocalBridgeRole) bool { return role.networkId == networkId }) {
		return nil
	}
	role := &deviceLocalBridgeRole{networkId: networkId, store: store, settings: settings}
	self.others = append(self.others, role)
	if err := self.buildRoleWithLock(role); err != nil {
		return err
	}
	self.settleWithLock()
	return nil
}

// Drops networkId's role, closing its bridge: its sidecar's own sign-out wiped it.
func (self *deviceLocalBridgeOwner) removeRole(networkId connect.Id) {
	self.operationLock.Lock()
	defer self.operationLock.Unlock()
	index := slices.IndexFunc(self.others, func(role *deviceLocalBridgeRole) bool { return role.networkId == networkId })
	if index < 0 {
		return
	}
	role := self.others[index]
	self.others = slices.Delete(self.others, index, index+1)
	self.holdWithLock()
	if role.removeChangeCallback != nil {
		role.removeChangeCallback()
	}
	if role.bridge != nil {
		role.bridge.Close()
	}
}

// Builds role's bridge over its store, restored before this returns, and hears it once the device has attached.
func (self *deviceLocalBridgeOwner) buildRoleWithLock(role *deviceLocalBridgeRole) error {
	bridge, err := self.newBridge(self.ctx, role.store, role.settings)
	if err != nil {
		return err
	}
	if self.localAddresses != nil {
		bridge.SetLocalAddresses(self.localAddresses)
	}
	role.bridge = bridge
	if self.hold != nil {
		role.removeChangeCallback = self.listenRoleWithLock(bridge)
	}
	return nil
}

// Runs call on networkId's bridge, the primary's or another role's, building it in legacy when build, then settles.
// errDeviceBridgeLegacy for a role with no bridge that call may not build.
func (self *deviceLocalBridgeOwner) onRole(networkId connect.Id, build bool, call func(bridge deviceBridgeOwned) error) error {
	if self.refusal != nil {
		return self.refusal
	}
	self.operationLock.Lock()
	defer self.operationLock.Unlock()
	if self.closed {
		return errDeviceBridgeClosed
	}
	var bridge deviceBridgeOwned
	if networkId == self.networkId {
		if self.bridge == nil && build {
			if err := self.buildWithLock(); err != nil {
				return err
			}
		}
		bridge = self.bridge
	} else {
		index := slices.IndexFunc(self.others, func(role *deviceLocalBridgeRole) bool { return role.networkId == networkId })
		if index < 0 {
			return errDeviceBridgeNetwork
		}
		role := self.others[index]
		if role.bridge == nil && build {
			if err := self.buildRoleWithLock(role); err != nil {
				return err
			}
		}
		bridge = role.bridge
	}
	if bridge == nil {
		return errDeviceBridgeLegacy
	}
	err := call(bridge)
	self.settleWithLock()
	return err
}

// Runs read on networkId's bridge, the primary's or another role's, under the operation lock and with no settle: a
// read moves no posture between legacy and past it, and a settle holds the bridges again, which tells the status
// listeners, whose status reads a role's posture here, so with one listener each status asked for the next (QD-1437).
// A read whose decision publishes tells them through the bridge's own change callback. errDeviceBridgeLegacy for a role
// with no bridge.
func (self *deviceLocalBridgeOwner) readRole(networkId connect.Id, read func(bridge deviceBridgeOwned)) error {
	if self.refusal != nil {
		return self.refusal
	}
	self.operationLock.Lock()
	defer self.operationLock.Unlock()
	if self.closed {
		return errDeviceBridgeClosed
	}
	bridge := self.bridge
	if networkId != self.networkId {
		index := slices.IndexFunc(self.others, func(role *deviceLocalBridgeRole) bool { return role.networkId == networkId })
		if index < 0 {
			return errDeviceBridgeNetwork
		}
		bridge = self.others[index].bridge
	}
	if bridge == nil {
		return errDeviceBridgeLegacy
	}
	read(bridge)
	return nil
}

// networkId's posture decision, legacy's with no bridge.
func (self *deviceLocalBridgeOwner) roleDecision(networkId connect.Id) sidecardevice.PostureDecision {
	decision := sidecardevice.PostureDecision{State: sidecardevice.PostureLegacy}
	self.readRole(networkId, func(bridge deviceBridgeOwned) {
		decision = bridge.Decision()
	})
	return decision
}

// networkId's posture decision read at the clock's now, legacy's with no bridge: what a Forwarder reads at each of the
// posture's alarm times, where roleDecision answers the decision last adopted.
func (self *deviceLocalBridgeOwner) roleDecisionNow(networkId connect.Id) sidecardevice.PostureDecision {
	decision := sidecardevice.PostureDecision{State: sidecardevice.PostureLegacy}
	self.readRole(networkId, func(bridge deviceBridgeOwned) {
		decision = bridge.DecisionNow()
	})
	return decision
}

// networkId's bridge status, legacy's with no bridge.
func (self *deviceLocalBridgeOwner) roleStatus(networkId connect.Id) sidecardevice.BridgeStatus {
	status := sidecardevice.BridgeStatus{State: sidecardevice.PostureLegacy}
	self.readRole(networkId, func(bridge deviceBridgeOwned) {
		status = bridge.Status()
	})
	return status
}

// One bridge the router holds, the primary marked.
type deviceBridgeRouterRole struct {
	networkId connect.Id
	bridge    deviceBridgeOwned
	primary   bool
}

// What the router routes by: each prefix with the role it goes to, the first-joined role's where two collide.
type deviceBridgeRouteTable struct {
	prefixes []netip.Prefix
	targets  []deviceBridge
	routes   []netip.Prefix
}

// The send route's view of several roles (the file's comment): Route and RouteBurst by destination, the primary's hook
// and status, every role's routes and names. The posture calls go to each role through the owner, never here.
type deviceBridgeRouter struct {
	roles   []deviceBridgeRouterRole
	primary deviceBridge
	table   atomic.Pointer[deviceBridgeRouteTable]
}

var _ deviceBridge = (*deviceBridgeRouter)(nil)

// A router over roles, in join order.
func newDeviceBridgeRouter(roles []deviceBridgeRouterRole) *deviceBridgeRouter {
	self := &deviceBridgeRouter{roles: slices.Clone(roles)}
	for _, role := range roles {
		if role.primary {
			self.primary = role.bridge
		}
	}
	self.table.Store(&deviceBridgeRouteTable{})
	return self
}

// Whether roles are this router's, in the same order.
func (self *deviceBridgeRouter) sameRoles(roles []deviceBridgeRouterRole) bool {
	return slices.Equal(self.roles, roles)
}

// Reads every role's routes again, in join order, keeping a later role's prefix only where no earlier one overlaps it.
func (self *deviceBridgeRouter) refresh() {
	table := &deviceBridgeRouteTable{}
	for _, role := range self.roles {
		for _, prefix := range role.bridge.Routes() {
			if slices.ContainsFunc(table.prefixes, prefix.Overlaps) {
				continue
			}
			table.prefixes = append(table.prefixes, prefix)
			table.targets = append(table.targets, role.bridge)
		}
	}
	table.routes = slices.Clone(table.prefixes)
	self.table.Store(table)
}

// The role packet goes to: the first whose routes hold its destination, else the primary, else none.
func (self *deviceBridgeRouter) target(packet []byte) deviceBridge {
	if destination, ok := deviceBridgePacketDestination(packet); ok {
		table := self.table.Load()
		for i, prefix := range table.prefixes {
			if prefix.Contains(destination) {
				return table.targets[i]
			}
		}
	}
	return self.primary
}

// A packet's destination address, read in place: IPv4's at 16, IPv6's at 24.
func deviceBridgePacketDestination(packet []byte) (netip.Addr, bool) {
	if len(packet) == 0 {
		return netip.Addr{}, false
	}
	switch packet[0] >> 4 {
	case 4:
		if len(packet) < 20 {
			return netip.Addr{}, false
		}
		return netip.AddrFrom4([4]byte(packet[16:20])), true
	case 6:
		if len(packet) < 40 {
			return netip.Addr{}, false
		}
		return netip.AddrFrom16([16]byte(packet[24:40])), true
	}
	return netip.Addr{}, false
}

// deviceBridge: the target's route, nil for the default when no role takes it.
func (self *deviceBridgeRouter) Route(packet []byte) connect.UserNatClient {
	target := self.target(packet)
	if target == nil {
		return nil
	}
	return target.Route(packet)
}

// deviceBridge: each run of packets one role takes goes to that role's RouteBurst, in order; a run no role takes goes
// to send with the default route.
func (self *deviceBridgeRouter) RouteBurst(packets [][]byte, send func(client connect.UserNatClient, packets [][]byte)) {
	start := 0
	for start < len(packets) {
		target := self.target(packets[start])
		end := start + 1
		for end < len(packets) && self.target(packets[end]) == target {
			end += 1
		}
		if target == nil {
			send(nil, packets[start:end])
		} else {
			target.RouteBurst(packets[start:end], send)
		}
		start = end
	}
}

// deviceBridge: the primary's hook, nil with no primary, so only the device client's network hosts.
func (self *deviceBridgeRouter) EgressHook() connect.EgressHook {
	if self.primary == nil {
		return nil
	}
	return self.primary.EgressHook()
}

// deviceBridge: every role's routes, a later role's colliding prefixes left out.
func (self *deviceBridgeRouter) Routes() []netip.Prefix {
	return slices.Clone(self.table.Load().routes)
}

// deviceBridge: each role's names, the first-joined role's answer where two own a name.
func (self *deviceBridgeRouter) LocalZone() connect.LocalZone {
	return &deviceBridgeRouterZone{router: self}
}

// deviceBridge: the router takes no posture call; the owner gives each role its own.
func (self *deviceBridgeRouter) SetPending(pending *bridge.Pending) error {
	return errDeviceBridgeNetwork
}

func (self *deviceBridgeRouter) SetState(state *bridge.BridgeState, log *sidecardevice.PostureLog) error {
	return errDeviceBridgeNetwork
}

func (self *deviceBridgeRouter) ApplyDeltas(log *sidecardevice.PostureLog) error {
	return errDeviceBridgeNetwork
}

// deviceBridge: the primary's status, legacy's with none.
func (self *deviceBridgeRouter) Status() sidecardevice.BridgeStatus {
	if self.primary == nil {
		return sidecardevice.BridgeStatus{State: sidecardevice.PostureLegacy}
	}
	return self.primary.Status()
}

// deviceBridge: callback after any role's publish.
func (self *deviceBridgeRouter) AddChangeCallback(callback func()) func() {
	removes := []func(){}
	for _, role := range self.roles {
		removes = append(removes, role.bridge.AddChangeCallback(callback))
	}
	return func() {
		for _, remove := range removes {
			remove()
		}
	}
}

// The router's zone: each role's at each query, in join order.
type deviceBridgeRouterZone struct {
	router *deviceBridgeRouter
}

func (self *deviceBridgeRouterZone) Answer(host string, qtype uint16) ([]netip.Addr, uint32, bool) {
	for _, role := range self.router.roles {
		zone := role.bridge.LocalZone()
		if zone == nil {
			continue
		}
		if addresses, ttl, owned := zone.Answer(host, qtype); owned {
			return addresses, ttl, true
		}
	}
	return nil, 0, false
}

// The VPN component's forwarding host, nil on a device that runs none: SDK-M8-07's channel calls its keys, hand-off
// and witness, and SDK-M8-08's links its signer.
func (self *DeviceLocal) forwardingHost() *deviceLocalForwardingHost {
	if self.bridgeOwner == nil {
		return nil
	}
	return self.bridgeOwner.forwarding
}

// The tun owner's local addresses to every role's bridge, which re-reads its routes (DeviceLocal.SetBridgeLocalAddresses).
func (self *deviceLocalBridgeOwner) setLocalAddresses(addresses []netip.Addr) {
	if self.refusal != nil {
		return
	}
	self.operationLock.Lock()
	defer self.operationLock.Unlock()
	if self.closed {
		return
	}
	self.localAddresses = slices.Clone(addresses)
	if self.bridge != nil {
		self.bridge.SetLocalAddresses(addresses)
	}
	for _, role := range self.others {
		if role.bridge != nil {
			role.bridge.SetLocalAddresses(addresses)
		}
	}
	self.holdWithLock()
}
