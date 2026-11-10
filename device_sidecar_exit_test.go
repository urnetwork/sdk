// The device role's bridge beyond its seam (device_sidecar_exit.go): its hook on the exit NAT, at the NAT's
// construction and on a live one, never on the fallback NAT, so this device's own route-local flows pass while the
// posture is pending; provider flows under the sdk provider's table; the tunnel address avoiding the block and the
// approved prefixes; and the devices that build no bridge. Devices are built through NewDeviceLocal on sidecar's
// posture vectors (device_sidecar_test.go) and given a provider in the process: a client with no transport, as the
// provider rotation tests use, so nothing leaves the process, and an exit NAT whose dials a dialer records and refuses.
package sdk

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/gopacket/layers"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	sidecardevice "github.com/urnetwork/sidecar/device"
	"github.com/urnetwork/sidecar/protocol/bridge"
)

// One flow a hook decided: where it was going, and the verdict.
type testingExitDecision struct {
	to      netip.AddrPort
	verdict connect.EgressVerdict
}

// A hook over a bridge's own that records where it is installed and what it decides. Installing it binds the bridge's
// own hook too, so the bridge's revocations still reach every NAT it is on.
type testingExitHook struct {
	connect.EgressHook
	binder connect.EgressHookBinder

	stateLock sync.Mutex
	// the NATs it is installed on now, and every NAT it was ever installed on, in order
	boundClosers     []connect.EgressFlowCloser
	everBoundClosers []connect.EgressFlowCloser
	// one signal for each install and removal
	changed chan struct{}
	// each flow it decides, as it decides it
	decided chan testingExitDecision
}

var (
	_ connect.EgressHook       = (*testingExitHook)(nil)
	_ connect.EgressHookBinder = (*testingExitHook)(nil)
)

// connect.EgressHookBinder: binds the bridge's own hook, and records the NAT until it lets go.
func (self *testingExitHook) BindFlowCloser(closer connect.EgressFlowCloser) func() {
	unbind := self.binder.BindFlowCloser(closer)
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.boundClosers = append(self.boundClosers, closer)
		self.everBoundClosers = append(self.everBoundClosers, closer)
	}()
	self.signal()
	return func() {
		if unbind != nil {
			unbind()
		}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.boundClosers = slices.DeleteFunc(self.boundClosers, func(bound connect.EgressFlowCloser) bool {
				return bound == closer
			})
		}()
		self.signal()
	}
}

// connect.EgressHook: the bridge's own decision, recorded.
func (self *testingExitHook) ResolveEgress(flow *connect.EgressFlow) connect.EgressDecision {
	decision := self.EgressHook.ResolveEgress(flow)
	select {
	case self.decided <- testingExitDecision{to: flow.To, verdict: decision.Verdict}:
	default:
	}
	return decision
}

// Wakes awaitBound, never blocking: a signal already waiting carries this one.
func (self *testingExitHook) signal() {
	select {
	case self.changed <- struct{}{}:
	default:
	}
}

// The NATs the hook is installed on now.
func (self *testingExitHook) bound() []connect.EgressFlowCloser {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.boundClosers)
}

// Every NAT the hook was installed on, in order.
func (self *testingExitHook) everBound() []connect.EgressFlowCloser {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.everBoundClosers)
}

// Waits until the hook is installed on exactly closers, failing past the watchdog. A NAT lets go of its hook as its
// run ends, after its close returns.
func (self *testingExitHook) awaitBound(t *testing.T, closers ...connect.EgressFlowCloser) {
	t.Helper()
	deadline := time.After(testingDeviceRoleWatchdog)
	for {
		if slices.Equal(self.bound(), closers) {
			return
		}
		select {
		case <-self.changed:
		case <-deadline:
			t.Fatalf("the hook is on %d NATs, want %d", len(self.bound()), len(closers))
		}
	}
}

// The next count decisions, by where each flow was going, failing past the watchdog.
func (self *testingExitHook) decisions(t *testing.T, count int) map[netip.AddrPort]connect.EgressVerdict {
	t.Helper()
	deadline := time.After(testingDeviceRoleWatchdog)
	verdicts := map[netip.AddrPort]connect.EgressVerdict{}
	for len(verdicts) < count {
		select {
		case decision := <-self.decided:
			verdicts[decision.to] = decision.verdict
		case <-deadline:
			t.Fatalf("the hook decided %d flows, want %d", len(verdicts), count)
		}
	}
	return verdicts
}

// A device.Bridge whose hook is a testingExitHook over its own.
type testingExitBridge struct {
	*sidecardevice.Bridge
	hook *testingExitHook
}

// deviceBridge: the recording hook, the same value on every call.
func (self *testingExitBridge) EgressHook() connect.EgressHook {
	return self.hook
}

// The bridges a device role built, in order, each a testingExitBridge, with settings adjusted by configure first.
type testingExitBridges struct {
	configure func(settings *sidecardevice.BridgeSettings)

	stateLock sync.Mutex
	built     []*testingExitBridge
}

// Builds the device role's bridges through this.
func (self *testingExitBridges) adjust(settings *DeviceLocalSettings) {
	settings.testingNewDeviceBridge = self.newBridge
}

// deviceBridgeConstructor.
func (self *testingExitBridges) newBridge(
	ctx context.Context,
	store sidecardevice.PostureStore,
	settings *sidecardevice.BridgeSettings,
) (deviceBridgeOwned, error) {
	if self.configure != nil {
		configured := *settings
		self.configure(&configured)
		settings = &configured
	}
	bridge, err := sidecardevice.NewBridge(ctx, store, nil, settings)
	if err != nil {
		return nil, err
	}
	hook := bridge.EgressHook()
	binder, ok := hook.(connect.EgressHookBinder)
	if !ok {
		bridge.Close()
		return nil, errors.New("CONTROL FAILED: the bridge's hook binds no NAT")
	}
	recorded := &testingExitBridge{
		Bridge: bridge,
		hook: &testingExitHook{
			EgressHook: hook,
			binder:     binder,
			changed:    make(chan struct{}, 64),
			decided:    make(chan testingExitDecision, 64),
		},
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.built = append(self.built, recorded)
	return recorded, nil
}

// The last bridge built.
func (self *testingExitBridges) last(t *testing.T) *testingExitBridge {
	t.Helper()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(self.built) == 0 {
		t.Fatal("the device role built no bridge")
	}
	return self.built[len(self.built)-1]
}

// Every bridge built.
func (self *testingExitBridges) all() []*testingExitBridge {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.built)
}

// A dialer for the exit NAT that records each address it is asked for and refuses it, so no flow leaves the process.
type testingExitDialer struct {
	stateLock sync.Mutex
	addresses []string
	dialed    chan struct{}
}

// A dialer that has dialed nothing.
func newTestingExitDialer() *testingExitDialer {
	return &testingExitDialer{dialed: make(chan struct{}, 64)}
}

// The dialer as DeviceLocalSettings.ProviderDialContextSettings, which the exit NAT's TCP and UDP sockets dial through.
func (self *testingExitDialer) settings() *connect.DialContextSettings {
	return &connect.DialContextSettings{
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.addresses = append(self.addresses, address)
			}()
			select {
			case self.dialed <- struct{}{}:
			default:
			}
			return nil, &net.OpError{Op: "dial", Net: network, Err: net.UnknownNetworkError("the exit nat test dials nothing")}
		},
	}
}

// What the exit NAT dialed, sorted.
func (self *testingExitDialer) all() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	addresses := slices.Clone(self.addresses)
	slices.Sort(addresses)
	return addresses
}

// Waits until the exit NAT has dialed each of addresses, failing past the watchdog.
func (self *testingExitDialer) await(t *testing.T, addresses ...netip.AddrPort) {
	t.Helper()
	deadline := time.After(testingDeviceRoleWatchdog)
	for {
		dialed := self.all()
		missing := slices.DeleteFunc(slices.Clone(addresses), func(address netip.AddrPort) bool {
			return slices.Contains(dialed, address.String())
		})
		if len(missing) == 0 {
			return
		}
		select {
		case <-self.dialed:
		case <-deadline:
			t.Fatalf("the exit NAT never dialed %v", missing)
		}
	}
}

// Gives device a provider in the process: a client with no transport and a fallback NAT built from the same settings
// function the exit NAT is built from, as newDeviceLocalProvider builds its own, whose route-local flows dial through
// localDial, nil for the real dialer. Route local is on, as it is for a device with a provider. The device's close
// ends both.
func testingExitProvider(t *testing.T, device *DeviceLocal, localDial *connect.DialContextSettings) *deviceLocalProvider {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	clientSettings := connect.DefaultClientSettings()
	clientSettings.Log = connect.NewNoopLogger()
	clientSettings.EncryptionSettings.Mode = connect.EncryptionModeOff
	client := connect.NewClient(ctx, connect.NewId(), connect.NewNoContractClientOob(), clientSettings)
	localUserNat, err := connect.TryNewLocalUserNat(
		ctx,
		client.ClientId().String(),
		providerLocalUserNatSettings(0, connect.NewNoopLogger(), localDial),
	)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	provider := &deviceLocalProvider{ctx: ctx, cancel: cancel, client: client, localUserNat: localUserNat}
	// the provider's returns to sources no transport reaches give up at once
	device.newRemoteUserNatProviderForTest = func(
		client *connect.Client,
		localUserNat *connect.LocalUserNat,
		settings *connect.RemoteUserNatProviderSettings,
	) *connect.RemoteUserNatProvider {
		settings.WriteTimeout = time.Millisecond
		return connect.NewRemoteUserNatProvider(client, localUserNat, settings)
	}
	device.stateLock.Lock()
	defer device.stateLock.Unlock()
	device.provider = provider
	device.routeLocal = true
	device.updateSendRouteWithLock()
	return provider
}

// Sets device's provide mode as SetProvideMode applies it, saving no preference.
func testingExitProvide(device *DeviceLocal, provideMode ProvideMode) {
	device.stateLock.Lock()
	defer device.stateLock.Unlock()
	device.setProvideModeWithLock(provideMode)
}

// The exit NAT, nil for none, and the bridge whose hook the device says it holds.
func testingExitNat(device *DeviceLocal) (*connect.LocalUserNat, deviceBridge) {
	device.stateLock.Lock()
	defer device.stateLock.Unlock()
	return device.remoteUserNatProviderLocalUserNat, device.remoteUserNatProviderEgressBridge
}

// A Network source's seals: the contract class and the subject the platform sealed, a client id. No session key, as
// an Opportunistic provider's plaintext source has none.
func testingExitPeer(subjectClientId connect.Id, contractClass protocol.ContractClass) connect.Peer {
	return connect.Peer{
		ProvideMode:   protocol.ProvideMode_Network,
		ContractClass: contractClass,
		SourceSubject: subjectClientId,
	}
}

// Hands device's provider packet from source as its client's receive callback would, under peer's seals.
func testingExitProviderFlow(t *testing.T, device *DeviceLocal, source connect.TransferPath, peer connect.Peer, packet []byte) {
	t.Helper()
	device.stateLock.Lock()
	provider := device.remoteUserNatProvider
	device.stateLock.Unlock()
	if provider == nil {
		t.Fatal("the device provides nothing")
	}
	frame, err := connect.ToFrame(&protocol.IpPacketToProvider{
		IpPacket: &protocol.IpPacket{PacketBytes: packet},
	}, connect.DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	provider.ClientReceive(source, []*protocol.Frame{frame}, peer)
	if !frame.Raw {
		connect.MessagePoolReturn(frame.MessageBytes)
	}
}

// The subject the exit rules give an exit: an untagged member device, by its sidecar, member and current client.
var (
	testingExitSubjectSidecarId = connect.Id(bytes.Repeat([]byte{0x31}, 16))
	testingExitSubjectMemberId  = connect.Id(bytes.Repeat([]byte{0x32}, 16))
	testingExitSubjectClientId  = connect.Id(bytes.Repeat([]byte{0x33}, 16))
	// a client no row binds: another client (§8.4)
	testingExitOtherClientId = connect.Id(bytes.Repeat([]byte{0x34}, 16))
)

// testingBridgeRules' block and services, with the subject's exit row of every protocol.
func testingExitRules() *bridge.BridgeRules {
	rules := testingBridgeRules()
	rules.Subjects = []*bridge.BridgeRuleSubject{{
		SidecarId: testingExitSubjectSidecarId[:],
		MemberId:  testingExitSubjectMemberId[:],
		ClientIds: [][]byte{testingExitSubjectClientId[:]},
		Exit:      []*bridge.BridgeRuleReach{{Protocol: 0}},
	}}
	return rules
}

// What the avoid list must hold: a block chosen in 10/8 (owner decision 132), the block it replaced, still draining,
// and a granted subnet route in 10/8.
var (
	testingExitBlock         = netip.MustParsePrefix("10.0.0.0/16")
	testingExitPreviousBlock = netip.MustParsePrefix("10.2.0.0/16")
	testingExitRoute         = netip.MustParsePrefix("10.1.0.0/16")
)

// The block, the previous block and the route, the route over one advertiser.
func testingExitAvoidRules() *bridge.BridgeRules {
	prefix := func(prefix netip.Prefix) *bridge.BridgeIpv4Prefix {
		return &bridge.BridgeIpv4Prefix{Address: testingExitIpv4Value(prefix.Addr()), Length: uint32(prefix.Bits())}
	}
	return &bridge.BridgeRules{
		Block:                   prefix(testingExitBlock),
		PreviousBlock:           prefix(testingExitPreviousBlock),
		PreviousBlockDrainUntil: time.Now().Add(time.Hour).UnixMilli(),
		Entries: []*bridge.BridgeRuleEntry{
			{
				Prefix: prefix(netip.PrefixFrom(testingExitBlock.Addr(), 24)),
				Kind:   bridge.BridgeRuleEntryKind_BRIDGE_RULE_ENTRY_KIND_SERVICE,
			},
			{
				Prefix: prefix(testingExitRoute),
				Kind:   bridge.BridgeRuleEntryKind_BRIDGE_RULE_ENTRY_KIND_LINK,
				Links: []*bridge.BridgeRuleLink{{
					TargetClientId:          bytes.Repeat([]byte{0x41}, 16),
					TargetSidecarId:         bytes.Repeat([]byte{0x42}, 16),
					TargetIdentityPublicKey: bytes.Repeat([]byte{0x43}, 32),
					RuleId:                  bytes.Repeat([]byte{0x44}, 32),
					Weight:                  1,
				}},
			},
		},
	}
}

// An IPv4 address as the rules' fixed32.
func testingExitIpv4Value(addr netip.Addr) uint32 {
	octets := addr.As4()
	return uint32(octets[0])<<24 | uint32(octets[1])<<16 | uint32(octets[2])<<8 | uint32(octets[3])
}

// Each avoid list a device chose its tunnel address from, and the address.
type testingExitAvoids struct {
	stateLock sync.Mutex
	avoids    [][]netip.Prefix
	addrs     []netip.Addr
}

// DeviceLocalSettings.testingRandomLocalIpv4: the real choice, recorded.
func (self *testingExitAvoids) choose(avoid []netip.Prefix) netip.Addr {
	addr := connect.RandomLocalIpv4(avoid)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.avoids = append(self.avoids, slices.Clone(avoid))
	self.addrs = append(self.addrs, addr)
	return addr
}

// The last avoid list and address.
func (self *testingExitAvoids) last(t *testing.T) ([]netip.Prefix, netip.Addr) {
	t.Helper()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(self.avoids) == 0 {
		t.Fatal("no tunnel address was chosen")
	}
	return self.avoids[len(self.avoids)-1], self.addrs[len(self.addrs)-1]
}

// A live exit NAT takes the hook when a bridge is first held, and gives it up at this sidecar's own sign-out; a publish
// in between installs nothing, since a second install would close every flow the hook decided; a NAT built while a
// bridge is held takes the hook as it is built; and the fallback NAT, which carries this device's own route-local
// flows, never holds one. The hook is the sdk provider's table.
func TestExitNatHookFollowsTheHeldBridge(t *testing.T) {
	role := newTestingDeviceRole(t)
	bridges := &testingExitBridges{}
	dialer := newTestingExitDialer()
	device := role.device(t, func(settings *DeviceLocalSettings) {
		bridges.adjust(settings)
		settings.ProviderDialContextSettings = dialer.settings()
	})
	provider := testingExitProvider(t, device, nil)
	fallback := connect.EgressFlowCloser(provider.LocalUserNat())

	// legacy: the exit NAT is built with no hook, today's
	testingExitProvide(device, ProvideModeNetwork)
	first, egressBridge := testingExitNat(device)
	if first == nil || egressBridge != nil {
		t.Fatalf("legacy: an exit NAT %v holding a bridge's hook %v", first != nil, egressBridge != nil)
	}

	// the Pending builds and holds a bridge, and the live NAT takes its hook
	if err := device.setBridgePending(role.vectors.pending); err != nil {
		t.Fatal(err)
	}
	held := bridges.last(t)
	held.hook.awaitBound(t, first)
	if _, egressBridge = testingExitNat(device); egressBridge != deviceBridge(held) {
		t.Fatal("the device does not record the held bridge's hook on its exit NAT")
	}
	if profile := held.Status().PolicyProfile; profile != sidecardevice.BridgePolicyProfileSdkProvider {
		t.Fatalf("the exit NAT's hook runs the %v table", profile)
	}

	// a state publishes and installs nothing
	if err := device.setBridgeState(role.vectors.state(t, 3, testingBridgeRules()), role.vectors.log(1, 3)); err != nil {
		t.Fatal(err)
	}
	if installs := held.hook.everBound(); len(installs) != 1 {
		t.Fatalf("a publish installed the hook again: %d installs", len(installs))
	}

	// providing off ends the NAT, which lets go of the hook; providing again builds a NAT that takes it as it is built,
	// with no hold in between
	testingExitProvide(device, ProvideModeNone)
	held.hook.awaitBound(t)
	testingExitProvide(device, ProvideModeNetwork)
	second, egressBridge := testingExitNat(device)
	if second == nil || second == first || egressBridge != deviceBridge(held) {
		t.Fatal("the next exit NAT was not built holding the held bridge's hook")
	}
	if bound := held.hook.bound(); !slices.Equal(bound, []connect.EgressFlowCloser{second}) {
		t.Fatalf("the next exit NAT was built hookless: the hook is on %d NATs", len(bound))
	}

	// through a revocation, a base with no document and a state on it the hook stays; this sidecar's own sign-out drops
	// the bridge, and the live NAT gives up its hook
	for _, step := range []func() error{
		func() error { return device.applyBridgeDeltas(role.vectors.log(4, 4)) },
		func() error { return device.applyBridgeDeltas(role.vectors.log(5, 5)) },
		func() error {
			return device.setBridgeState(role.vectors.state(t, 6, testingBridgeRules()), role.vectors.log(6, 6))
		},
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	if installs := held.hook.everBound(); len(installs) != 2 {
		t.Fatalf("the hook was installed %d times, want 2", len(installs))
	}
	if err := device.applyBridgeDeltas(role.vectors.log(7, 7)); err != nil {
		t.Fatal(err)
	}
	held.hook.awaitBound(t)
	if nat, egressBridge := testingExitNat(device); nat != second || egressBridge != nil {
		t.Fatal("after the sign-out the exit NAT still records a hook, or was replaced")
	}

	for _, built := range bridges.all() {
		if slices.Contains(built.hook.everBound(), fallback) {
			t.Fatal("the fallback NAT took a hook")
		}
	}
	testingJoinDeviceRole(t, device)
	if dialed := dialer.all(); len(dialed) != 0 {
		t.Fatalf("the exit NAT dialed %v", dialed)
	}
}

// A device restored past legacy builds its first exit NAT holding the bridge's hook, so no provider flow meets it
// hookless.
func TestRestoredBridgeHooksTheFirstExitNat(t *testing.T) {
	role := newTestingDeviceRole(t)
	enforcing := role.device(t, nil)
	role.enforce(t, enforcing, testingExitRules())
	testingJoinDeviceRole(t, enforcing)

	bridges := &testingExitBridges{}
	device := role.device(t, func(settings *DeviceLocalSettings) {
		bridges.adjust(settings)
		settings.ProviderDialContextSettings = newTestingExitDialer().settings()
	})
	held := bridges.last(t)
	if device.sendRoute.Load().bridge != deviceBridge(held) || held.Status().State != sidecardevice.PostureEnforcingDocument {
		t.Fatal("CONTROL FAILED: the device did not restore and hold an enforcing bridge")
	}
	provider := testingExitProvider(t, device, nil)
	if installs := held.hook.everBound(); len(installs) != 0 {
		t.Fatalf("the hook was installed on %d NATs before the device provided", len(installs))
	}
	testingExitProvide(device, ProvideModeNetwork)
	exitNat, egressBridge := testingExitNat(device)
	if exitNat == nil || egressBridge != deviceBridge(held) {
		t.Fatal("the first exit NAT does not hold the restored bridge's hook")
	}
	if bound := held.hook.bound(); !slices.Equal(bound, []connect.EgressFlowCloser{exitNat}) {
		t.Fatalf("the first exit NAT was built hookless: the hook is on %d NATs", len(bound))
	}
	if slices.Contains(held.hook.everBound(), connect.EgressFlowCloser(provider.LocalUserNat())) {
		t.Fatal("the fallback NAT took the hook")
	}
}

// While the posture is pending, which denies every Network flow on the exit NAT, this device's own route-local flow
// still reaches a loopback service through the fallback NAT, which holds no hook; the same packet from a provider
// source meets the hook on the exit NAT and is denied.
func TestRouteLocalSurvivesPending(t *testing.T) {
	listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		listener.Close()
	})
	service := listener.LocalAddr().(*net.UDPAddr).AddrPort()
	received := make(chan []byte, 4)
	go func() {
		buffer := make([]byte, 2048)
		for {
			n, _, err := listener.ReadFromUDPAddrPort(buffer)
			if err != nil {
				return
			}
			select {
			case received <- slices.Clone(buffer[:n]):
			default:
			}
		}
	}()

	role := newTestingDeviceRole(t)
	bridges := &testingExitBridges{}
	dialer := newTestingExitDialer()
	device := role.device(t, func(settings *DeviceLocalSettings) {
		bridges.adjust(settings)
		settings.ProviderDialContextSettings = dialer.settings()
	})
	provider := testingExitProvider(t, device, nil)
	// providing from legacy, so the Pending's hold puts the hook on a live exit NAT, beside the fallback NAT
	testingExitProvide(device, ProvideModeNetwork)
	if err := device.setBridgePending(role.vectors.pending); err != nil {
		t.Fatal(err)
	}
	status, err := device.bridgeStatus()
	if err != nil || status.State != sidecardevice.PosturePending {
		t.Fatalf("the device role is %v (%v), want pending", status.State, err)
	}
	held := bridges.last(t)
	exitNat, _ := testingExitNat(device)

	// the hold installed the hook before it returned, so the route-local flow is sent with the hook in place
	payload := []byte("route-local")
	packet := testingDeviceRolePacket(t, service.Addr(), layers.IPProtocolUDP, service.Port(), payload)
	if !device.SendPacket(packet, int32(len(packet))) {
		t.Fatal("the local route refused the packet")
	}
	select {
	case got := <-received:
		if !bytes.Equal(got, payload) {
			t.Fatalf("the service received %q", got)
		}
	case <-time.After(testingDeviceRoleWatchdog):
		t.Fatal("this device's own route-local flow did not pass while pending")
	}
	select {
	case decision := <-held.hook.decided:
		t.Fatalf("the hook decided this device's own flow to %v", decision.to)
	default:
	}
	held.hook.awaitBound(t, exitNat)
	if slices.Contains(held.hook.everBound(), connect.EgressFlowCloser(provider.LocalUserNat())) {
		t.Fatal("the fallback NAT took the hook")
	}

	testingExitProviderFlow(t, device, connect.SourceId(connect.NewId()),
		testingExitPeer(testingExitSubjectClientId, connect.ContractClassData), packet)
	if verdicts := held.hook.decisions(t, 1); verdicts[service] != connect.EgressVerdictDeny {
		t.Fatalf("a provider flow while pending: %v", verdicts)
	}
	testingJoinDeviceRole(t, device)
	if dialed := dialer.all(); len(dialed) != 0 {
		t.Fatalf("the exit NAT dialed %v", dialed)
	}
}

// Provider flows on the exit NAT under the sdk provider's table (§17.6's sdk exit NAT), from a Network source with no
// session, as an Opportunistic provider's may be: with a document its subject keeps the public exit its row allows
// and loses LAN, loopback and overlay reach, a contract that sealed no class gets nothing, and another client, bound by
// no row, holds no rows; with no document the untagged subject keeps today's LAN and loopback while the block stays
// denied. A denied flow is never dialed.
func TestExitNatProviderFlowsUnderTheSdkTable(t *testing.T) {
	role := newTestingDeviceRole(t)
	bridges := &testingExitBridges{}
	dialer := newTestingExitDialer()
	device := role.device(t, func(settings *DeviceLocalSettings) {
		bridges.adjust(settings)
		settings.ProviderDialContextSettings = dialer.settings()
	})
	testingExitProvider(t, device, nil)
	role.enforce(t, device, testingExitRules())
	testingExitProvide(device, ProvideModeNetwork)
	held := bridges.last(t)
	exitNat, _ := testingExitNat(device)
	held.hook.awaitBound(t, exitNat)

	public := netip.MustParseAddr("203.0.113.10")
	lan := netip.MustParseAddr("192.168.20.5")
	loopback := netip.MustParseAddr("127.0.0.1")
	overlay := netip.MustParseAddr("100.64.0.7")
	source := connect.SourceId(connect.NewId())
	expect := func(name string, peer connect.Peer, port uint16, want map[netip.Addr]connect.EgressVerdict) {
		t.Helper()
		for addr := range want {
			testingExitProviderFlow(t, device, source, peer,
				testingDeviceRolePacket(t, addr, layers.IPProtocolUDP, port, []byte(name)))
		}
		verdicts := held.hook.decisions(t, len(want))
		for addr, verdict := range want {
			if got := verdicts[netip.AddrPortFrom(addr, port)]; got != verdict {
				t.Errorf("%s: to %v %v, want %v", name, addr, got, verdict)
			}
		}
	}
	subject := testingExitPeer(testingExitSubjectClientId, connect.ContractClassData)
	expect("a document", subject, 5353, map[netip.Addr]connect.EgressVerdict{
		public:   connect.EgressVerdictPass,
		lan:      connect.EgressVerdictDeny,
		loopback: connect.EgressVerdictDeny,
		overlay:  connect.EgressVerdictDeny,
	})
	expect("no class", testingExitPeer(testingExitSubjectClientId, connect.ContractClassUnset), 5354,
		map[netip.Addr]connect.EgressVerdict{public: connect.EgressVerdictDeny})
	expect("another client", testingExitPeer(testingExitOtherClientId, connect.ContractClassData), 5355,
		map[netip.Addr]connect.EgressVerdict{public: connect.EgressVerdictDeny})
	dialer.await(t, netip.AddrPortFrom(public, 5353))

	// a covered revocation of another sidecar, then a verified base with no document, which opens nothing between
	// states, and the state on it, which alone moves the role to the allow-all (DESIGN-DETAILS 4.3's OnlyJudgedOpens)
	for _, n := range []uint64{4, 5} {
		if err := device.applyBridgeDeltas(role.vectors.log(n, n)); err != nil {
			t.Fatal(err)
		}
	}
	if status, err := device.bridgeStatus(); err != nil || status.State != sidecardevice.PostureEnforcingDocument {
		t.Fatalf("the device role is %v (%v) on a base between states, want enforcing_doc", status.State, err)
	}
	if err := device.setBridgeState(role.vectors.state(t, 6, testingExitRules()), role.vectors.log(6, 6)); err != nil {
		t.Fatal(err)
	}
	if status, err := device.bridgeStatus(); err != nil || status.State != sidecardevice.PostureEnforcingNoDocument {
		t.Fatalf("the device role is %v (%v), want enforcing_nodoc", status.State, err)
	}
	expect("no document", subject, 5356, map[netip.Addr]connect.EgressVerdict{
		public:   connect.EgressVerdictPass,
		lan:      connect.EgressVerdictPass,
		loopback: connect.EgressVerdictPass,
		overlay:  connect.EgressVerdictDeny,
	})
	passed := []netip.AddrPort{
		netip.AddrPortFrom(public, 5353),
		netip.AddrPortFrom(public, 5356),
		netip.AddrPortFrom(lan, 5356),
		netip.AddrPortFrom(loopback, 5356),
	}
	dialer.await(t, passed...)

	testingJoinDeviceRole(t, device)
	want := []string{}
	for _, address := range passed {
		want = append(want, address.String())
	}
	slices.Sort(want)
	if dialed := dialer.all(); !slices.Equal(dialed, want) {
		t.Fatalf("the exit NAT dialed %v, want only what passed, %v", dialed, want)
	}
}

// The block in force, the block it replaced and the routes the bridge routes join the local networks the tunnel
// address avoids, read while the device is built from the restored bridge: a block chosen in 10/8, where
// RandomLocalIpv4 draws, never holds the tunnel's own address. A live device's address never moves; the next device
// takes the change.
func TestTunnelAddressAvoidsTheBlock(t *testing.T) {
	if !testingExitBlock.Contains(connect.RandomLocalIpv4(nil)) {
		t.Fatal("CONTROL FAILED: with nothing to avoid, the tunnel address would not fall in the test's block")
	}
	role := newTestingDeviceRole(t)
	avoids := &testingExitAvoids{}
	// no local network wins the route, whatever this machine's interfaces are
	bridges := &testingExitBridges{configure: func(settings *sidecardevice.BridgeSettings) {
		settings.LocalIpv4Networks = func() []netip.Prefix { return []netip.Prefix{} }
	}}
	adjust := func(settings *DeviceLocalSettings) {
		bridges.adjust(settings)
		settings.UseExperimentalTunnelAddress = true
		settings.testingRandomLocalIpv4 = avoids.choose
	}

	first := role.device(t, adjust)
	firstAvoid, firstAddr := avoids.last(t)
	if first.tunnelLocalAddress != firstAddr {
		t.Fatalf("the device's tunnel address %v is not the one chosen, %v", first.tunnelLocalAddress, firstAddr)
	}
	for _, prefix := range []netip.Prefix{testingExitBlock, testingExitPreviousBlock, testingExitRoute} {
		if slices.Contains(firstAvoid, prefix) {
			t.Fatalf("a legacy device avoided %v", prefix)
		}
	}
	role.enforce(t, first, testingExitAvoidRules())
	if first.tunnelLocalAddress != firstAddr {
		t.Fatal("a state moved a live tunnel address")
	}
	if routes := bridges.last(t).Routes(); !slices.Contains(routes, testingExitRoute) {
		t.Fatalf("CONTROL FAILED: the bridge routes %v, without the granted route", routes)
	}
	testingJoinDeviceRole(t, first)

	second := role.device(t, adjust)
	secondAvoid, secondAddr := avoids.last(t)
	if second.tunnelLocalAddress != secondAddr {
		t.Fatalf("the device's tunnel address %v is not the one chosen, %v", second.tunnelLocalAddress, secondAddr)
	}
	for _, prefix := range []netip.Prefix{testingExitBlock, testingExitPreviousBlock, testingExitRoute} {
		if !slices.Contains(secondAvoid, prefix) {
			t.Errorf("the avoid list lacks %v", prefix)
		}
		if prefix.Contains(secondAddr) {
			t.Errorf("the tunnel address %v lies in %v", secondAddr, prefix)
		}
	}
	if !netip.MustParsePrefix("10.0.0.0/8").Contains(secondAddr) {
		t.Fatalf("the tunnel address %v left 10/8", secondAddr)
	}
}

// A proxy device, shaped as server/proxy builds every hosted device (newProxyDeviceLocalSettings: the low profile and
// HostedIncompatible), builds no bridge with the role asked for and an enforcing posture stored, so nothing of a block
// reaches its tunnel address either (D-BRIDGE-11, D-SDK-12).
func TestProxyDeviceBuildsNoBridge(t *testing.T) {
	role := newTestingDeviceRole(t)
	enforcing := role.device(t, nil)
	role.enforce(t, enforcing, testingExitAvoidRules())
	testingJoinDeviceRole(t, enforcing)

	avoids := &testingExitAvoids{}
	settings := DefaultLowMemoryDeviceLocalSettings()
	settings.DisableLogging = true
	settings.HostedIncompatible = true
	settings.EnableBridge = true
	// a hosted device provides nothing, and this keeps the test in the process
	settings.AllowProvider = false
	settings.GeneratorFunc = func([]*connect.ProviderSpec) connect.MultiClientGenerator {
		return &testingDnsOwnerGenerator{}
	}
	settings.UseExperimentalTunnelAddress = true
	settings.testingRandomLocalIpv4 = avoids.choose
	proxy, err := NewDeviceLocal(role.space, role.clientJwt, "device-role-proxy", "test", "0", role.instanceId, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testingJoinDeviceRole(t, proxy)
	})
	if proxy.sendRoute.Load().bridge != nil || proxy.bridgeOwner.store != nil || proxy.bridgeOwner.bridge != nil {
		t.Fatal("a proxy device built a bridge or opened a posture")
	}
	if !errors.Is(proxy.bridgeOwner.refusal, errDeviceBridgeHosted) {
		t.Fatalf("a proxy device's role was refused as %v", proxy.bridgeOwner.refusal)
	}
	avoid, _ := avoids.last(t)
	for _, prefix := range []netip.Prefix{testingExitBlock, testingExitPreviousBlock, testingExitRoute} {
		if slices.Contains(avoid, prefix) {
			t.Fatalf("a proxy device avoided %v of a bridge it does not run", prefix)
		}
	}
}

// Which devices run the role: one asked for it, on any platform with raw sockets; never one off, hosted or a
// browser's, whose extension worker and pages have no raw sockets (D-BRIDGE-11), in that order of reasons.
func TestDeviceBridgeRefusals(t *testing.T) {
	cases := []struct {
		enable bool
		hosted bool
		goos   string
		want   error
	}{
		{enable: false, hosted: false, goos: "linux", want: errDeviceBridgeOff},
		{enable: false, hosted: true, goos: "js", want: errDeviceBridgeOff},
		{enable: true, hosted: true, goos: "js", want: errDeviceBridgeHosted},
		{enable: true, hosted: true, goos: "linux", want: errDeviceBridgeHosted},
		{enable: true, hosted: false, goos: "js", want: errDeviceBridgeBrowser},
		{enable: true, hosted: false, goos: "linux", want: nil},
		{enable: true, hosted: false, goos: "darwin", want: nil},
		{enable: true, hosted: false, goos: "windows", want: nil},
		{enable: true, hosted: false, goos: "android", want: nil},
		{enable: true, hosted: false, goos: "ios", want: nil},
	}
	for _, c := range cases {
		settings := DefaultDeviceLocalSettings()
		settings.EnableBridge = c.enable
		settings.HostedIncompatible = c.hosted
		if got := deviceBridgeRefusal(settings, c.goos); got != c.want {
			t.Errorf("enable=%v hosted=%v on %s: %v, want %v", c.enable, c.hosted, c.goos, got, c.want)
		}
	}
}
