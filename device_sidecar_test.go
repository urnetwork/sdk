// The device role's seam in DeviceLocal (device_sidecar.go): the send paths with no bridge and with one, the restore
// before the first packet, the zone across a mux rebuild, the off and hosted refusals, and the owner's lifecycle. A
// real device.Bridge runs on sidecar's posture vectors (testdata/device/posture_vectors.json): a synthetic network's
// Pending, log, rows and states, which the posture verifies as it would a sidecar role's. A state's rules are not
// what its log signs, so each test puts rules of its own in the vectors' states.
package sdk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"golang.org/x/net/dns/dnsmessage"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	sidecardevice "github.com/urnetwork/sidecar/device"
	"github.com/urnetwork/sidecar/protocol/bridge"
)

// How long a test waits for what a real device does on another goroutine before it fails.
const testingDeviceRoleWatchdog = 5 * time.Second

// sidecar's posture vectors, decoded.
type testingPostureVectors struct {
	pending *bridge.Pending
	entries [][]byte
	rows    map[[sha256.Size]byte][]byte
	states  map[uint64]*bridge.BridgeState
}

// Reads the vectors from the sidecar module this one builds against.
func testingLoadPostureVectors(t *testing.T) *testingPostureVectors {
	t.Helper()
	command := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/urnetwork/sidecar")
	command.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("finding the sidecar module: %v", err)
	}
	encoded, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(output)), "testdata", "device", "posture_vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vectorsJson struct {
		Pending string            `json:"pending"`
		Entries []string          `json:"entries"`
		Rows    map[string]string `json:"rows"`
		States  map[string]string `json:"states"`
	}
	if err := json.Unmarshal(encoded, &vectorsJson); err != nil {
		t.Fatal(err)
	}
	decodeHex := func(text string) []byte {
		decoded, err := hex.DecodeString(text)
		if err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	vectors := &testingPostureVectors{
		pending: &bridge.Pending{},
		rows:    map[[sha256.Size]byte][]byte{},
		states:  map[uint64]*bridge.BridgeState{},
	}
	if err := proto.Unmarshal(decodeHex(vectorsJson.Pending), vectors.pending); err != nil {
		t.Fatal(err)
	}
	for _, entry := range vectorsJson.Entries {
		vectors.entries = append(vectors.entries, decodeHex(entry))
	}
	for rowsSha256, rows := range vectorsJson.Rows {
		vectors.rows[[sha256.Size]byte(decodeHex(rowsSha256))] = decodeHex(rows)
	}
	for n, stateText := range vectorsJson.States {
		pinN, err := strconv.ParseUint(n, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		state := &bridge.BridgeState{}
		if err := proto.Unmarshal(decodeHex(stateText), state); err != nil {
			t.Fatal(err)
		}
		vectors.states[pinN] = state
	}
	if len(vectors.entries) < 7 || vectors.states[3] == nil || vectors.states[6] == nil {
		t.Fatal("CONTROL FAILED: the posture vectors lack the script this file runs")
	}
	return vectors
}

// The vectors' network, every device's here.
func (self *testingPostureVectors) networkId() connect.Id {
	return connect.Id(self.pending.GetNetworkId())
}

// Entries from through, both included, with the rows a base among them names.
func (self *testingPostureVectors) log(from uint64, through uint64) *sidecardevice.PostureLog {
	return &sidecardevice.PostureLog{
		Entries: slices.Clone(self.entries[from-1 : through]),
		OpenRows: func(rowsSha256 []byte) (io.ReadCloser, error) {
			rows, ok := self.rows[[sha256.Size]byte(rowsSha256)]
			if !ok {
				return nil, errors.New("no such rows")
			}
			return io.NopCloser(bytes.NewReader(rows)), nil
		},
	}
}

// The vectors' state at n with rules in it, which the bridge must parse.
func (self *testingPostureVectors) state(t *testing.T, n uint64, rules *bridge.BridgeRules) *bridge.BridgeState {
	t.Helper()
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(rules)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sidecardevice.ParseBridgeRules(encoded, nil); err != nil {
		t.Fatalf("CONTROL FAILED: the test's rules do not parse: %v", err)
	}
	state := proto.Clone(self.states[n]).(*bridge.BridgeState)
	state.Rules = encoded
	return state
}

// A /16 block of 100.64.0.0/10 with its services and one unassigned /24: 100.64.0.53 is the resolver, 100.64.9.0/24
// rejects, and everything outside the block takes the default route. The vectors' names put laptop.net.example at
// 100.64.0.1.
func testingBridgeRules() *bridge.BridgeRules {
	return &bridge.BridgeRules{
		Block: &bridge.BridgeIpv4Prefix{Address: 0x64400000, Length: 16},
		Entries: []*bridge.BridgeRuleEntry{
			{
				Prefix: &bridge.BridgeIpv4Prefix{Address: 0x64400000, Length: 24},
				Kind:   bridge.BridgeRuleEntryKind_BRIDGE_RULE_ENTRY_KIND_SERVICE,
			},
			{
				Prefix: &bridge.BridgeIpv4Prefix{Address: 0x64400900, Length: 24},
				Kind:   bridge.BridgeRuleEntryKind_BRIDGE_RULE_ENTRY_KIND_UNASSIGNED,
			},
		},
	}
}

var (
	// the tun's own address in these packets
	testingDeviceRoleSource = netip.MustParseAddr("198.51.100.20")
	// the bridge's resolver, <block>.0.53
	testingDeviceRoleResolver = netip.MustParseAddr("100.64.0.53")
	// an address of the unassigned /24, which the bridge rejects
	testingDeviceRoleUnassigned = netip.MustParseAddr("100.64.9.9")
	// an address outside the block, the default route's
	testingDeviceRoleOutside = netip.MustParseAddr("192.0.2.1")
	// the vectors' private name and its address
	testingDeviceRoleName     = "laptop.net.example"
	testingDeviceRoleNameAddr = netip.MustParseAddr("100.64.0.1")
)

// An IPv4 UDP packet, or a TCP SYN, from the tun to destination:port carrying payload. A fresh slice, the test's own.
func testingDeviceRolePacket(t *testing.T, destination netip.Addr, ipProtocol layers.IPProtocol, port uint16, payload []byte) []byte {
	t.Helper()
	ip := &layers.IPv4{
		Version:  4,
		TTL:      64,
		SrcIP:    net.IP(testingDeviceRoleSource.AsSlice()),
		DstIP:    net.IP(destination.AsSlice()),
		Protocol: ipProtocol,
	}
	serializableLayers := []gopacket.SerializableLayer{ip}
	switch ipProtocol {
	case layers.IPProtocolUDP:
		udp := &layers.UDP{SrcPort: 43123, DstPort: layers.UDPPort(port)}
		if err := udp.SetNetworkLayerForChecksum(ip); err != nil {
			t.Fatal(err)
		}
		serializableLayers = append(serializableLayers, udp)
	case layers.IPProtocolTCP:
		tcp := &layers.TCP{SrcPort: 43124, DstPort: layers.TCPPort(port), SYN: true, Seq: 1000, Window: 65535}
		if err := tcp.SetNetworkLayerForChecksum(ip); err != nil {
			t.Fatal(err)
		}
		serializableLayers = append(serializableLayers, tcp)
	default:
		t.Fatalf("no packet of protocol %d here", ipProtocol)
	}
	serializableLayers = append(serializableLayers, gopacket.Payload(payload))
	buffer := gopacket.NewSerializeBuffer()
	options := gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}
	if err := gopacket.SerializeLayers(buffer, options, serializableLayers...); err != nil {
		t.Fatal(err)
	}
	return slices.Clone(buffer.Bytes())
}

// A DNS query for name's A record to destination:53, as id.
func testingDeviceRoleDnsQuery(t *testing.T, destination netip.Addr, id uint16, name string) []byte {
	t.Helper()
	dns := &layers.DNS{
		ID:        id,
		RD:        true,
		Questions: []layers.DNSQuestion{{Name: []byte(name), Type: layers.DNSTypeA, Class: layers.DNSClassIN}},
	}
	buffer := gopacket.NewSerializeBuffer()
	if err := dns.SerializeTo(buffer, gopacket.SerializeOptions{FixLengths: true}); err != nil {
		t.Fatal(err)
	}
	return testingDeviceRolePacket(t, destination, layers.IPProtocolUDP, 53, slices.Clone(buffer.Bytes()))
}

// What a DNS reply entering the tun says: where from, its id, and the A records it answers.
type testingDnsReply struct {
	source  netip.Addr
	id      uint16
	answers []netip.Addr
}

// The reply packet carries, or false for a packet that is no DNS reply.
func testingParseDnsReply(packet []byte) (testingDnsReply, bool) {
	path, payload, err := connect.ParseIpPathWithPayload(packet)
	if err != nil || path.Protocol != connect.IpProtocolUdp || path.SourcePort != 53 {
		return testingDnsReply{}, false
	}
	source, _ := netip.AddrFromSlice(path.SourceIp)
	var parser dnsmessage.Parser
	header, err := parser.Start(payload)
	if err != nil || !header.Response {
		return testingDnsReply{}, false
	}
	if err := parser.SkipAllQuestions(); err != nil {
		return testingDnsReply{}, false
	}
	reply := testingDnsReply{source: source.Unmap(), id: header.ID}
	for {
		answerHeader, err := parser.AnswerHeader()
		if err != nil {
			break
		}
		if answerHeader.Type != dnsmessage.TypeA {
			if err := parser.SkipAnswer(); err != nil {
				break
			}
			continue
		}
		answer, err := parser.AResource()
		if err != nil {
			break
		}
		reply.answers = append(reply.answers, netip.AddrFrom4(answer.A))
	}
	return reply, true
}

// A network space of the vectors' network, its login seeded as an app's, under a home of the test's own. Devices on
// it run the device role with no provider and windows with no candidates, so nothing leaves the process.
type testingDeviceRole struct {
	vectors    *testingPostureVectors
	space      *NetworkSpace
	home       string
	clientJwt  string
	instanceId *Id
}

func newTestingDeviceRole(t *testing.T) *testingDeviceRole {
	t.Helper()
	vectors := testingLoadPostureVectors(t)
	home := t.TempDir()
	space := newNetworkSpace(context.Background(), *NewNetworkSpaceKey("device-role.test", "test"),
		NetworkSpaceValues{ApiUrl: "http://127.0.0.1:1", PlatformUrl: "ws://127.0.0.1:1"}, home)
	t.Cleanup(space.close)
	api := space.GetApi()
	api.tokenManager.Close()
	testingAwaitAuthBoundary(t, api.tokenManager.done)
	networkId := vectors.networkId().String()
	localState := space.asyncLocalState.localState
	networkJwt := testingJwt(map[string]any{
		"user_id":      "00000000-0000-0000-0000-000000000003",
		"network_id":   networkId,
		"network_name": "device-role-test",
	})
	if err := localState.SetByJwt(networkJwt); err != nil {
		t.Fatal(err)
	}
	clientJwt, err := gojwt.NewWithClaims(gojwt.SigningMethodNone, gojwt.MapClaims{
		"client_id":  "00000000-0000-0000-0000-000000000001",
		"device_id":  "00000000-0000-0000-0000-000000000002",
		"network_id": networkId,
		"exp":        time.Now().Add(30 * 24 * time.Hour).Unix(),
	}).SignedString(gojwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	if err := localState.SetByClientJwt(clientJwt); err != nil {
		t.Fatal(err)
	}
	instanceId := localState.GetInstanceId()
	if instanceId == nil {
		t.Fatal("the seeded login made no instance")
	}
	return &testingDeviceRole{
		vectors:    vectors,
		space:      space,
		home:       home,
		clientJwt:  clientJwt,
		instanceId: instanceId,
	}
}

// A device on the space with the device role on, settings adjusted by adjust; joined at the test's end.
func (self *testingDeviceRole) device(t *testing.T, adjust func(settings *DeviceLocalSettings)) *DeviceLocal {
	t.Helper()
	device, err := self.tryDevice(adjust)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testingJoinDeviceRole(t, device)
	})
	return device
}

// The device, or why it could not be built, through the public constructor an app calls.
func (self *testingDeviceRole) tryDevice(adjust func(settings *DeviceLocalSettings)) (*DeviceLocal, error) {
	settings := DefaultDeviceLocalSettings()
	settings.DisableLogging = true
	settings.AllowProvider = false
	settings.EnableBridge = true
	settings.GeneratorFunc = func([]*connect.ProviderSpec) connect.MultiClientGenerator {
		return &testingDnsOwnerGenerator{}
	}
	if adjust != nil {
		adjust(settings)
	}
	return NewDeviceLocal(self.space, self.clientJwt, "device-role", "test", "0", self.instanceId, settings)
}

// Moves device to enforcing{doc} at the vectors' pin 3 with rules.
func (self *testingDeviceRole) enforce(t *testing.T, device *DeviceLocal, rules *bridge.BridgeRules) {
	t.Helper()
	if err := device.setBridgePending(self.vectors.pending); err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if err := device.setBridgeState(self.vectors.state(t, 3, rules), self.vectors.log(1, 3)); err != nil {
		t.Fatalf("state 3: %v", err)
	}
	status, err := device.bridgeStatus()
	if err != nil || status.State != sidecardevice.PostureEnforcingDocument {
		t.Fatalf("the device role is %v (%v), want enforcing_doc", status.State, err)
	}
}

// The network's posture directory under the space's storage.
func (self *testingDeviceRole) postureDirectory() string {
	return filepath.Join(self.home, sdkPostureDirectoryName, self.vectors.networkId().String())
}

// Every record of the posture directory, by name.
func (self *testingDeviceRole) postureRecords(t *testing.T) map[string][]byte {
	t.Helper()
	records := map[string][]byte{}
	entries, err := os.ReadDir(self.postureDirectory())
	if errors.Is(err, os.ErrNotExist) {
		return records
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		record, err := os.ReadFile(filepath.Join(self.postureDirectory(), entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		records[entry.Name()] = record
	}
	return records
}

// Closes device and waits for it, failing past the watchdog.
func testingJoinDeviceRole(t *testing.T, device *DeviceLocal) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testingDeviceRoleWatchdog)
	defer cancel()
	if err := device.CloseAndWait(ctx); err != nil {
		t.Errorf("the device did not join: %v", err)
	}
}

// Everything that enters device's tun, copied.
type testingTunCapture struct {
	stateLock sync.Mutex
	packets   [][]byte
	arrived   chan struct{}
}

func newTestingTunCapture(t *testing.T, device *DeviceLocal) *testingTunCapture {
	capture := &testingTunCapture{arrived: make(chan struct{}, 64)}
	unsubscribe := device.AddReceivePacketCallback(func(_ connect.TransferPath, _ protocol.ProvideMode, _ *connect.IpPath, packet []byte) {
		capture.stateLock.Lock()
		capture.packets = append(capture.packets, slices.Clone(packet))
		capture.stateLock.Unlock()
		select {
		case capture.arrived <- struct{}{}:
		default:
		}
	})
	t.Cleanup(unsubscribe)
	return capture
}

// The first DNS reply with id to enter the tun, failing past the watchdog.
func (self *testingTunCapture) dnsReply(t *testing.T, id uint16) testingDnsReply {
	t.Helper()
	deadline := time.After(testingDeviceRoleWatchdog)
	for {
		self.stateLock.Lock()
		for _, packet := range self.packets {
			if reply, ok := testingParseDnsReply(packet); ok && reply.id == id {
				self.stateLock.Unlock()
				return reply
			}
		}
		self.stateLock.Unlock()
		select {
		case <-self.arrived:
		case <-deadline:
			t.Fatalf("no DNS reply %d entered the tun", id)
		}
	}
}

// The packets that entered the tun so far.
func (self *testingTunCapture) snapshot() [][]byte {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.packets)
}

// A route that takes every packet without allocating or returning it, counting what it took and, when asked to,
// recording each packet's last byte in the order it took them.
type testingCountingRoute struct {
	stateLock   sync.Mutex
	keepOrder   bool
	packetCount int
	batchCount  int
	nativeCount int
	lastBytes   []byte
}

func (self *testingCountingRoute) take(packets ...[]byte) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.packetCount += len(packets)
	if self.keepOrder {
		for _, packet := range packets {
			self.lastBytes = append(self.lastBytes, packet[len(packet)-1])
		}
	}
}

// Takes the packet.
func (self *testingCountingRoute) SendPacket(source connect.TransferPath, provideMode protocol.ProvideMode, packet []byte, timeout time.Duration) bool {
	self.take(packet)
	return true
}

// Takes every packet.
func (self *testingCountingRoute) SendPacketBatch(source connect.TransferPath, provideMode protocol.ProvideMode, packets [][]byte, timeout time.Duration) int {
	self.stateLock.Lock()
	self.batchCount += 1
	self.stateLock.Unlock()
	self.take(packets...)
	return len(packets)
}

func (self *testingCountingRoute) Close() {}

func (self *testingCountingRoute) Shuffle() {}

func (self *testingCountingRoute) SecurityPolicyStats(reset bool) connect.SecurityPolicyStats {
	return connect.SecurityPolicyStats{}
}

func (self *testingCountingRoute) SetLocalSecurityBypass(localSecurityBypass bool) {}

// A minimal device whose today's route is route, and whose send route holds bridge, nil for none.
func testingDeviceRoleSendDevice(route connect.UserNatClient, bridge deviceBridge) *DeviceLocal {
	device := newPacketBatchTestDevice(route)
	device.sendRoute.Store(&deviceLocalSendRoute{remoteUserNatClient: route, bridge: bridge})
	return device
}

// With no bridge the send paths are today's: the packet reaches today's route, and the device role's check costs no
// allocation on either path.
func TestNilBridgeKeepsTodaysRoute(t *testing.T) {
	route := &testingCountingRoute{}
	device := testingDeviceRoleSendDevice(route, nil)
	packet := testingDeviceRolePacket(t, testingDeviceRoleUnassigned, layers.IPProtocolUDP, 9, []byte("x"))
	if !device.sendPacket(packet) {
		t.Fatal("today's route refused the packet")
	}
	packets := [][]byte{packet, packet}
	if device.sendPacketsWithPolicy(packets, false) != 2 || device.sendPacketsWithPolicy(packets, true) != 2 {
		t.Fatal("today's route did not take the bursts")
	}
	connect.AssertEqual(t, route.packetCount, 5)
	singleAllocs := testing.AllocsPerRun(1000, func() {
		device.sendPacket(packet)
	})
	burstAllocs := testing.AllocsPerRun(1000, func() {
		device.sendPacketsWithPolicy(packets, true)
	})
	if singleAllocs != 0 || burstAllocs != 0 {
		t.Fatalf("a send with no bridge allocates: %v a packet, %v a burst", singleAllocs, burstAllocs)
	}
}

// A send with no bridge: the figure behind TestNilBridgeKeepsTodaysRoute's allocation bound.
func BenchmarkNilBridgeSendPacket(b *testing.B) {
	route := &testingCountingRoute{}
	device := testingDeviceRoleSendDevice(route, nil)
	packet := connect.MessagePoolCopy(make([]byte, 64))
	defer connect.MessagePoolReturn(packet)
	packet[0] = 0x45
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i += 1 {
		device.sendPacket(packet)
	}
}

// A burst through a real device.Bridge, every packet on today's route, allocates nothing.
func TestBridgeBurstAllocatesNothing(t *testing.T) {
	role := newTestingDeviceRole(t)
	store, err := newSdkPostureStore(t.TempDir(), role.vectors.networkId())
	if err != nil {
		t.Fatal(err)
	}
	bridge := testingEnforcingBridge(t, role.vectors, store)
	route := &testingCountingRoute{}
	device := testingDeviceRoleSendDevice(route, bridge)
	outside := testingDeviceRolePacket(t, testingDeviceRoleOutside, layers.IPProtocolUDP, 9, []byte("x"))
	packets := [][]byte{outside, outside, outside}
	if device.sendPacketsWithPolicy(packets, false) != 3 || route.packetCount != 3 {
		t.Fatal("the bridge's default route did not reach today's route")
	}
	allocs := testing.AllocsPerRun(1000, func() {
		device.sendPacketsWithPolicy(packets, true)
	})
	if allocs != 0 {
		t.Fatalf("a burst through the bridge allocates %v", allocs)
	}
}

// A real bridge over store, at enforcing{doc} with testingBridgeRules; closed at the test's end.
func testingEnforcingBridge(t *testing.T, vectors *testingPostureVectors, store sidecardevice.PostureStore) deviceBridgeOwned {
	t.Helper()
	bridge, err := newDeviceBridge(context.Background(), store, deviceLocalBridgeSettings(DefaultDeviceLocalSettings()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bridge.Close)
	if err := bridge.SetPending(vectors.pending); err != nil {
		t.Fatal(err)
	}
	if err := bridge.SetState(vectors.state(t, 3, testingBridgeRules()), vectors.log(1, 3)); err != nil {
		t.Fatal(err)
	}
	return bridge
}

// EnableBridge off, the default, changes nothing: a device opens no posture, makes no directory, holds no bridge
// even over a stored enforcing posture, which it leaves as it is, and refuses every bridge call.
func TestEnableBridgeOffChangesNothing(t *testing.T) {
	if DefaultDeviceLocalSettings().EnableBridge {
		t.Fatal("the device role is on by default")
	}
	role := newTestingDeviceRole(t)
	fresh := role.device(t, func(settings *DeviceLocalSettings) {
		settings.EnableBridge = false
	})
	if _, err := os.Stat(filepath.Join(role.home, sdkPostureDirectoryName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a device with the role off made the posture directory: %v", err)
	}
	if err := fresh.setBridgePending(role.vectors.pending); !errors.Is(err, errDeviceBridgeOff) {
		t.Fatalf("a Pending reached a device with the role off: %v", err)
	}
	testingJoinDeviceRole(t, fresh)

	enforcing := role.device(t, nil)
	role.enforce(t, enforcing, testingBridgeRules())
	testingJoinDeviceRole(t, enforcing)
	stored := role.postureRecords(t)
	if len(stored) == 0 {
		t.Fatal("CONTROL FAILED: the enforcing device stored nothing")
	}

	off := role.device(t, func(settings *DeviceLocalSettings) {
		settings.EnableBridge = false
	})
	if off.sendRoute.Load().bridge != nil || off.bridgeOwner.store != nil {
		t.Fatal("a device with the role off restored a bridge")
	}
	for _, err := range []error{
		off.setBridgePending(role.vectors.pending),
		off.setBridgeState(role.vectors.state(t, 3, testingBridgeRules()), role.vectors.log(1, 3)),
		off.applyBridgeDeltas(role.vectors.log(4, 4)),
	} {
		if !errors.Is(err, errDeviceBridgeOff) {
			t.Errorf("a bridge call on a device with the role off gave %v", err)
		}
	}
	if _, err := off.bridgeStatus(); !errors.Is(err, errDeviceBridgeOff) {
		t.Errorf("status on a device with the role off gave %v", err)
	}
	// no destination: with no bridge a packet to the block finds no route, as today
	unassigned := testingDeviceRolePacket(t, testingDeviceRoleUnassigned, layers.IPProtocolUDP, 9, []byte("x"))
	if off.SendPacket(unassigned, int32(len(unassigned))) {
		t.Fatal("a packet to the block found a route on a device with the role off and no destination")
	}
	testingJoinDeviceRole(t, off)
	if !maps.EqualFunc(stored, role.postureRecords(t), bytes.Equal) {
		t.Fatal("a device with the role off changed the stored posture")
	}
}

// What was enforcing is in force before the device's first packet: a new device, through the real constructor,
// restores the stored posture, holds its bridge in the first send route, and its first packet to an unassigned block
// address is the bridge's reject, which enters the tun, while a packet outside the block finds no route.
func TestPostureRestoredBeforeFirstPacket(t *testing.T) {
	role := newTestingDeviceRole(t)
	first := role.device(t, nil)
	role.enforce(t, first, testingBridgeRules())
	testingJoinDeviceRole(t, first)

	second := role.device(t, nil)
	if second.sendRoute.Load().bridge == nil {
		t.Fatal("the restored bridge is not in the first send route")
	}
	status, err := second.bridgeStatus()
	if err != nil || status.State != sidecardevice.PostureEnforcingDocument || status.EntryCount != 2 {
		t.Fatalf("restored %v with %d entries (%v), want enforcing_doc with 2", status.State, status.EntryCount, err)
	}
	capture := newTestingTunCapture(t, second)
	unassigned := testingDeviceRolePacket(t, testingDeviceRoleUnassigned, layers.IPProtocolUDP, 9, []byte("first"))
	if !second.SendPacket(unassigned, int32(len(unassigned))) {
		t.Fatal("the restored bridge did not take the first packet to an unassigned block address")
	}
	outside := testingDeviceRolePacket(t, testingDeviceRoleOutside, layers.IPProtocolUDP, 9, []byte("second"))
	if second.SendPacket(outside, int32(len(outside))) {
		t.Fatal("a packet outside the block found a route on a device with no destination")
	}
	status, _ = second.bridgeStatus()
	if status.RejectSentCount != 1 {
		t.Fatalf("the bridge sent %d rejects, want 1", status.RejectSentCount)
	}
	rejects := capture.snapshot()
	if len(rejects) != 1 {
		t.Fatalf("%d packets entered the tun, want the reject", len(rejects))
	}
	// ICMP administratively prohibited (3/13) from the unassigned address back to the tun
	reject := rejects[0]
	headerByteCount := int(reject[0]&0x0F) * 4
	if len(reject) < headerByteCount+2 || reject[9] != 1 || reject[headerByteCount] != 3 || reject[headerByteCount+1] != 13 ||
		netip.AddrFrom4([4]byte(reject[12:16])) != testingDeviceRoleUnassigned ||
		netip.AddrFrom4([4]byte(reject[16:20])) != testingDeviceRoleSource {
		t.Fatalf("what entered the tun is no reject of the packet: % x", reject)
	}
}

// Route runs before the mux on all three send paths: a DNS query to the bridge's resolver is the bridge's to answer
// from the state's names, though a live mux claims every UDP/53, and TCP to port 80 of an unassigned address is the
// bridge's to reject; a query to the mux's stand-in still reaches the mux, which answers the same name from the zone
// the device put on it.
func TestRouteRunsBeforeTheMux(t *testing.T) {
	role := newTestingDeviceRole(t)
	device := role.device(t, nil)
	role.enforce(t, device, testingBridgeRules())
	device.SetUpgradeMuxSettings(testingDnsOwnerSettings())
	device.SetConnectLocation(testingStoredLocation("device-role-destination"))
	if !device.GetTunnelDnsInterceptorActive() {
		t.Fatal("CONTROL FAILED: no live mux claims DNS")
	}
	capture := newTestingTunCapture(t, device)
	answered := func() uint64 {
		status, err := device.bridgeStatus()
		if err != nil {
			t.Fatal(err)
		}
		return status.ResolverAnsweredCount
	}
	wantAnswer := func(reply testingDnsReply, source netip.Addr) {
		t.Helper()
		if reply.source != source || !slices.Equal(reply.answers, []netip.Addr{testingDeviceRoleNameAddr}) {
			t.Fatalf("reply %d from %v answered %v, want %v from %v", reply.id, reply.source, reply.answers, testingDeviceRoleNameAddr, source)
		}
	}

	// the singular path
	query := testingDeviceRoleDnsQuery(t, testingDeviceRoleResolver, 4101, testingDeviceRoleName)
	if !device.SendPacket(query, int32(len(query))) {
		t.Fatal("the singular path refused the query")
	}
	wantAnswer(capture.dnsReply(t, 4101), testingDeviceRoleResolver)
	connect.AssertEqual(t, answered(), uint64(1))
	// the generic burst path, which takes pooled originals
	query = testingDeviceRoleDnsQuery(t, testingDeviceRoleResolver, 4102, testingDeviceRoleName)
	connect.AssertEqual(t, device.SendPacketsNoCopy([][]byte{connect.MessagePoolCopy(query)}), 1)
	wantAnswer(capture.dnsReply(t, 4102), testingDeviceRoleResolver)
	connect.AssertEqual(t, answered(), uint64(2))
	// the native burst path
	query = testingDeviceRoleDnsQuery(t, testingDeviceRoleResolver, 4103, testingDeviceRoleName)
	connect.AssertEqual(t, device.SendPacketBatch(packetBatchTestEncode([][]byte{query})), int32(1))
	wantAnswer(capture.dnsReply(t, 4103), testingDeviceRoleResolver)
	connect.AssertEqual(t, answered(), uint64(3))

	// TCP to :80 of an unassigned address is the bridge's reject
	statusBefore, _ := device.bridgeStatus()
	http := testingDeviceRolePacket(t, testingDeviceRoleUnassigned, layers.IPProtocolTCP, 80, nil)
	if !device.SendPacket(http, int32(len(http))) {
		t.Fatal("the bridge did not take TCP to :80 of an unassigned address")
	}
	statusAfter, _ := device.bridgeStatus()
	if statusAfter.RejectSentCount+statusAfter.RejectUnsentCount != statusBefore.RejectSentCount+statusBefore.RejectUnsentCount+1 {
		t.Fatal("TCP to :80 of an unassigned address was not the bridge's reject")
	}

	// a query to the mux's stand-in is the mux's, answered from the zone the device put on it
	addresses := device.TunnelDnsAddressesIpv4()
	if addresses == nil || addresses.Len() == 0 {
		t.Fatal("CONTROL FAILED: the mux advertises no DNS address")
	}
	standIn := netip.MustParseAddr(addresses.Get(0))
	query = testingDeviceRoleDnsQuery(t, standIn, 4104, testingDeviceRoleName)
	if !device.SendPacket(query, int32(len(query))) {
		t.Fatal("the mux did not claim a query to its stand-in")
	}
	wantAnswer(capture.dnsReply(t, 4104), standIn)
	connect.AssertEqual(t, answered(), uint64(3))
}

// A destination change builds a new mux, which holds no zone until the device puts the bridge's back beside the
// blocker: after a rebuild the new mux still answers the state's names.
func TestZoneReappliedAfterMuxRebuild(t *testing.T) {
	role := newTestingDeviceRole(t)
	device := role.device(t, nil)
	device.SetUpgradeMuxSettings(testingDnsOwnerSettings())
	device.SetConnectLocation(testingStoredLocation("device-role-first"))
	// the state arrives while the first mux is live: the change callback puts the zone on it
	role.enforce(t, device, testingBridgeRules())
	capture := newTestingTunCapture(t, device)
	ask := func(id uint16) testingDnsReply {
		t.Helper()
		addresses := device.TunnelDnsAddressesIpv4()
		if addresses == nil || addresses.Len() == 0 {
			t.Fatal("CONTROL FAILED: the mux advertises no DNS address")
		}
		query := testingDeviceRoleDnsQuery(t, netip.MustParseAddr(addresses.Get(0)), id, testingDeviceRoleName)
		if !device.SendPacket(query, int32(len(query))) {
			t.Fatal("the mux did not claim the query")
		}
		return capture.dnsReply(t, id)
	}
	if reply := ask(4201); !slices.Equal(reply.answers, []netip.Addr{testingDeviceRoleNameAddr}) {
		t.Fatalf("the first mux answered %v", reply.answers)
	}
	device.stateLock.Lock()
	firstMux := device.upgradeMux
	device.stateLock.Unlock()
	device.Reconnect(testingStoredLocation("device-role-first"))
	device.stateLock.Lock()
	secondMux := device.upgradeMux
	device.stateLock.Unlock()
	if firstMux == nil || secondMux == nil || firstMux == secondMux {
		t.Fatal("CONTROL FAILED: the reconnect built no new mux")
	}
	if reply := ask(4202); !slices.Equal(reply.answers, []netip.Addr{testingDeviceRoleNameAddr}) {
		t.Fatalf("the rebuilt mux answered %v: the zone was not put back", reply.answers)
	}
}

// One run of a fake bridge's burst.
type testingBridgeRun struct {
	client connect.UserNatClient
	count  int
}

// A bridge that routes by a packet's first byte: 1 to link, 2 to building, anything else to the default. Its burst
// split sends each maximal run of one route, in order, which keeps flow order.
type testingSplitBridge struct {
	deviceBridge
	link     connect.UserNatClient
	building connect.UserNatClient
}

func (self *testingSplitBridge) Route(packet []byte) connect.UserNatClient {
	switch packet[0] {
	case 1:
		return self.link
	case 2:
		return self.building
	default:
		return nil
	}
}

func (self *testingSplitBridge) RouteBurst(packets [][]byte, send func(client connect.UserNatClient, packets [][]byte)) {
	start := 0
	for i := 1; i <= len(packets); i += 1 {
		if i == len(packets) || self.Route(packets[i]) != self.Route(packets[start]) {
			send(self.Route(packets[start]), packets[start:i])
			start = i
		}
	}
}

// A link that takes native bursts as well as generic ones, and returns what it takes.
type testingNativeLink struct {
	testingCountingRoute
}

func (self *testingNativeLink) SendNativePacketBatch(source connect.TransferPath, provideMode protocol.ProvideMode, packets [][]byte, timeout time.Duration) int {
	self.stateLock.Lock()
	self.nativeCount += 1
	self.stateLock.Unlock()
	self.take(packets...)
	for _, packet := range packets {
		connect.MessagePoolReturn(packet)
	}
	return len(packets)
}

func (self *testingNativeLink) SendPacketBatch(source connect.TransferPath, provideMode protocol.ProvideMode, packets [][]byte, timeout time.Duration) int {
	count := self.testingCountingRoute.SendPacketBatch(source, provideMode, packets, timeout)
	for _, packet := range packets {
		connect.MessagePoolReturn(packet)
	}
	return count
}

// A link still building: it refuses every packet, which stays its caller's, and takes no batch.
type testingBuildingLink struct {
	refusedCount atomic.Int64
}

func (self *testingBuildingLink) SendPacket(source connect.TransferPath, provideMode protocol.ProvideMode, packet []byte, timeout time.Duration) bool {
	self.refusedCount.Add(1)
	return false
}

func (self *testingBuildingLink) Close() {}

func (self *testingBuildingLink) Shuffle() {}

func (self *testingBuildingLink) SecurityPolicyStats(reset bool) connect.SecurityPolicyStats {
	return connect.SecurityPolicyStats{}
}

func (self *testingBuildingLink) SetLocalSecurityBypass(localSecurityBypass bool) {}

// Today's route on a batch: takes and returns every packet, recording each one's last byte.
type testingPooledDefault struct {
	testingCountingRoute
}

func (self *testingPooledDefault) SendPacketBatch(source connect.TransferPath, provideMode protocol.ProvideMode, packets [][]byte, timeout time.Duration) int {
	count := self.testingCountingRoute.SendPacketBatch(source, provideMode, packets, timeout)
	for _, packet := range packets {
		connect.MessagePoolReturn(packet)
	}
	return count
}

// A burst split by the bridge keeps each flow's order on every route and gives each packet one owner, on the native
// path and the generic one: the link takes its run as a native batch where it can, a building link's refused packets
// go back to the pool here, today's route takes the default runs, and the count is what the routes accepted. Through a
// real bridge, a mixed burst's default packets keep their order around a reject and a resolver query, and every pooled
// packet comes back.
func TestBurstSplitKeepsFlowOrderAndOwners(t *testing.T) {
	for _, nativeIngress := range []bool{false, true} {
		link := &testingNativeLink{testingCountingRoute: testingCountingRoute{keepOrder: true}}
		building := &testingBuildingLink{}
		today := &testingPooledDefault{testingCountingRoute: testingCountingRoute{keepOrder: true}}
		bridge := &testingSplitBridge{link: link, building: building}
		device := testingDeviceRoleSendDevice(today, bridge)
		baseline := connect.MessagePoolPacketOutstandingCount()
		// each packet's first byte is its route, and its last its place in the burst
		routeOrders := [][2]byte{{0, 10}, {1, 11}, {1, 12}, {2, 13}, {0, 14}, {0, 15}, {1, 16}, {2, 17}, {0, 18}}
		packets := [][]byte{}
		for _, routeOrder := range routeOrders {
			packet := connect.MessagePoolGet(32)
			packet[0] = routeOrder[0]
			packet[len(packet)-1] = routeOrder[1]
			packets = append(packets, packet)
		}
		sent := device.sendPacketsWithPolicy(packets, nativeIngress)
		connect.AssertEqual(t, sent, 7)
		connect.AssertEqual(t, building.refusedCount.Load(), int64(2))
		if got := connect.MessagePoolPacketOutstandingCount(); got != baseline {
			t.Fatalf("native=%v: %d packet roots outstanding after the burst, want %d", nativeIngress, got, baseline)
		}
		if !slices.Equal(today.lastBytes, []byte{10, 14, 15, 18}) || !slices.Equal(link.lastBytes, []byte{11, 12, 16}) {
			t.Fatalf("native=%v: today's route took %v and the link %v, out of their order", nativeIngress, today.lastBytes, link.lastBytes)
		}
		if nativeIngress != (link.nativeCount == 2) || nativeIngress == (link.batchCount == 2) {
			t.Fatalf("native=%v: the link took %d native and %d generic batches", nativeIngress, link.nativeCount, link.batchCount)
		}
	}

	role := newTestingDeviceRole(t)
	store, err := newSdkPostureStore(t.TempDir(), role.vectors.networkId())
	if err != nil {
		t.Fatal(err)
	}
	realBridge := testingEnforcingBridge(t, role.vectors, store)
	tunPackets := 0
	realBridge.SetReceivePacket(func(_ connect.TransferPath, _ protocol.ProvideMode, _ *connect.IpPath, packet []byte) {
		tunPackets += 1
	})
	for _, nativeIngress := range []bool{false, true} {
		today := &testingPooledDefault{testingCountingRoute: testingCountingRoute{keepOrder: true}}
		device := testingDeviceRoleSendDevice(today, realBridge)
		baseline := connect.MessagePoolPacketOutstandingCount()
		statusBefore := realBridge.Status()
		burst := [][]byte{
			testingDeviceRolePacket(t, testingDeviceRoleOutside, layers.IPProtocolUDP, 9, []byte{1}),
			testingDeviceRolePacket(t, testingDeviceRoleUnassigned, layers.IPProtocolUDP, 9, []byte{0}),
			testingDeviceRolePacket(t, testingDeviceRoleOutside, layers.IPProtocolUDP, 9, []byte{2}),
			testingDeviceRoleDnsQuery(t, testingDeviceRoleResolver, 4301, testingDeviceRoleName),
			testingDeviceRolePacket(t, testingDeviceRoleOutside, layers.IPProtocolUDP, 9, []byte{3}),
		}
		pooled := [][]byte{}
		for _, packet := range burst {
			pooled = append(pooled, connect.MessagePoolCopy(packet))
		}
		connect.AssertEqual(t, device.sendPacketsWithPolicy(pooled, nativeIngress), 5)
		if got := connect.MessagePoolPacketOutstandingCount(); got != baseline {
			t.Fatalf("native=%v: %d packet roots outstanding after a real bridge's burst, want %d", nativeIngress, got, baseline)
		}
		if !slices.Equal(today.lastBytes, []byte{1, 2, 3}) {
			t.Fatalf("native=%v: today's route took %v, want the default packets 1, 2, 3 in order", nativeIngress, today.lastBytes)
		}
		statusAfter := realBridge.Status()
		connect.AssertEqual(t, statusAfter.ResolverAnsweredCount, statusBefore.ResolverAnsweredCount+1)
		connect.AssertEqual(t, statusAfter.RejectSentCount+statusAfter.RejectUnsentCount, statusBefore.RejectSentCount+statusBefore.RejectUnsentCount+1)
	}
	connect.AssertEqual(t, tunPackets, 4)
}

// A hosted device builds no bridge, even over a stored enforcing posture, which it neither reads nor changes, and
// every bridge call on it is refused (D-SDK-12).
func TestHostedDeviceBuildsNoBridge(t *testing.T) {
	role := newTestingDeviceRole(t)
	enforcing := role.device(t, nil)
	role.enforce(t, enforcing, testingBridgeRules())
	testingJoinDeviceRole(t, enforcing)
	stored := role.postureRecords(t)

	hosted := role.device(t, func(settings *DeviceLocalSettings) {
		settings.HostedIncompatible = true
	})
	if hosted.sendRoute.Load().bridge != nil || hosted.bridgeOwner.store != nil {
		t.Fatal("a hosted device built a bridge or opened a posture")
	}
	for _, err := range []error{
		hosted.setBridgePending(role.vectors.pending),
		hosted.setBridgeState(role.vectors.state(t, 3, testingBridgeRules()), role.vectors.log(1, 3)),
		hosted.applyBridgeDeltas(role.vectors.log(4, 4)),
	} {
		if !errors.Is(err, errDeviceBridgeHosted) {
			t.Errorf("a bridge call on a hosted device gave %v", err)
		}
	}
	if _, err := hosted.bridgeStatus(); !errors.Is(err, errDeviceBridgeHosted) {
		t.Errorf("status on a hosted device gave %v", err)
	}
	testingJoinDeviceRole(t, hosted)
	if !maps.EqualFunc(stored, role.postureRecords(t), bytes.Equal) {
		t.Fatal("a hosted device changed the stored posture")
	}
}

// A bridge that records its close, over the real one.
type testingRecordedBridge struct {
	*sidecardevice.Bridge
	closed atomic.Bool
}

func (self *testingRecordedBridge) Close() {
	self.closed.Store(true)
	self.Bridge.Close()
}

// The bridge lives with the device's client: a device is built with a bridge restored from the store past legacy,
// that bridge ends when the device and its client close, and the next device, a new client on the same network,
// builds a new bridge restored to the same state. In legacy the restored bridge is closed at once, and a Pending
// builds the one that is held.
func TestBridgeRebuiltWithTheClient(t *testing.T) {
	role := newTestingDeviceRole(t)
	builtLock := sync.Mutex{}
	built := []*testingRecordedBridge{}
	record := func(settings *DeviceLocalSettings) {
		settings.testingNewDeviceBridge = func(ctx context.Context, store sidecardevice.PostureStore, bridgeSettings *sidecardevice.BridgeSettings) (deviceBridgeOwned, error) {
			bridge, err := sidecardevice.NewBridge(ctx, store, nil, bridgeSettings)
			if err != nil {
				return nil, err
			}
			recorded := &testingRecordedBridge{Bridge: bridge}
			builtLock.Lock()
			built = append(built, recorded)
			builtLock.Unlock()
			return recorded, nil
		}
	}
	builtCount := func() int {
		builtLock.Lock()
		defer builtLock.Unlock()
		return len(built)
	}

	first := role.device(t, record)
	// legacy: the restore's bridge closed at once, none held
	if builtCount() != 1 || !built[0].closed.Load() || first.sendRoute.Load().bridge != nil {
		t.Fatal("a legacy restore left a bridge open or held")
	}
	role.enforce(t, first, testingBridgeRules())
	if builtCount() != 2 || built[1].closed.Load() || first.sendRoute.Load().bridge != deviceBridge(built[1]) {
		t.Fatal("the Pending did not build the held bridge")
	}
	testingJoinDeviceRole(t, first)
	if !built[1].closed.Load() {
		t.Fatal("the bridge outlived its device's client")
	}
	if err := first.setBridgePending(role.vectors.pending); !errors.Is(err, errDeviceBridgeClosed) {
		t.Fatalf("a closed device took a Pending: %v", err)
	}

	second := role.device(t, record)
	if builtCount() != 3 || built[2].closed.Load() || second.sendRoute.Load().bridge != deviceBridge(built[2]) {
		t.Fatal("the next device did not build and hold a bridge of its own")
	}
	status, err := second.bridgeStatus()
	if err != nil || status.State != sidecardevice.PostureEnforcingDocument {
		t.Fatalf("the rebuilt bridge restored %v (%v)", status.State, err)
	}
	testingJoinDeviceRole(t, second)
	if !built[2].closed.Load() {
		t.Fatal("the rebuilt bridge outlived its device")
	}
}

// Only this sidecar's own sign-out takes the device role back to legacy: through the vectors' script the device stays
// held through a revocation, a verified base with no document, which opens nothing between states (DESIGN-DETAILS
// 4.3's OnlyJudgedOpens), and a state on that base, which moves it to the allow-all, and at its own SidecarRevoke the
// bridge is dropped from the send route and the mux, closed, and the posture wiped.
func TestOwnSignOutDropsTheBridge(t *testing.T) {
	role := newTestingDeviceRole(t)
	device := role.device(t, nil)
	device.SetUpgradeMuxSettings(testingDnsOwnerSettings())
	device.SetConnectLocation(testingStoredLocation("device-role-sign-out"))
	role.enforce(t, device, testingBridgeRules())
	for _, step := range []struct {
		name string
		call func() error
		want sidecardevice.PostureState
	}{
		{name: "a covered revocation", call: func() error { return device.applyBridgeDeltas(role.vectors.log(4, 4)) }, want: sidecardevice.PostureEnforcingDocument},
		{name: "a base with no document", call: func() error { return device.applyBridgeDeltas(role.vectors.log(5, 5)) }, want: sidecardevice.PostureEnforcingDocument},
		{name: "a state on it", call: func() error {
			return device.setBridgeState(role.vectors.state(t, 6, testingBridgeRules()), role.vectors.log(6, 6))
		}, want: sidecardevice.PostureEnforcingNoDocument},
	} {
		if err := step.call(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		status, _ := device.bridgeStatus()
		if status.State != step.want || device.sendRoute.Load().bridge == nil {
			t.Fatalf("after %s the device role is %v, held %v", step.name, status.State, device.sendRoute.Load().bridge != nil)
		}
	}
	if err := device.applyBridgeDeltas(role.vectors.log(7, 7)); err != nil {
		t.Fatalf("the sign-out: %v", err)
	}
	status, err := device.bridgeStatus()
	if err != nil || status.State != sidecardevice.PostureLegacy {
		t.Fatalf("after its own sign-out the device role is %v (%v)", status.State, err)
	}
	device.stateLock.Lock()
	held, zone := device.bridge, device.bridgeZone
	device.stateLock.Unlock()
	if held != nil || zone != nil || device.sendRoute.Load().bridge != nil {
		t.Fatal("the signed-out bridge is still held")
	}
	if err := device.applyBridgeDeltas(role.vectors.log(7, 7)); !errors.Is(err, errDeviceBridgeLegacy) {
		t.Fatalf("a delta reached a legacy device role: %v", err)
	}
	// the wipe leaves a legacy manifest alone, and the next device restores legacy, holding nothing
	if records := role.postureRecords(t); 1 < len(records) {
		t.Fatalf("the sign-out left %d records", len(records))
	}
	testingJoinDeviceRole(t, device)
	next := role.device(t, nil)
	status, err = next.bridgeStatus()
	if err != nil || status.State != sidecardevice.PostureLegacy || next.sendRoute.Load().bridge != nil {
		t.Fatalf("after the sign-out a new device restored %v (%v)", status.State, err)
	}
}

// A Pending for another network than the device's client builds nothing and stores nothing.
func TestPendingForAnotherNetworkRefused(t *testing.T) {
	role := newTestingDeviceRole(t)
	device := role.device(t, nil)
	other := proto.Clone(role.vectors.pending).(*bridge.Pending)
	other.NetworkId = bytes.Repeat([]byte{0x02}, 16)
	if err := device.setBridgePending(other); !errors.Is(err, errDeviceBridgeNetwork) {
		t.Fatalf("another network's Pending gave %v", err)
	}
	if err := device.setBridgePending(nil); !errors.Is(err, errDeviceBridgeNetwork) {
		t.Fatalf("no Pending gave %v", err)
	}
	if device.sendRoute.Load().bridge != nil || len(role.postureRecords(t)) != 0 {
		t.Fatal("a refused Pending built or stored a posture")
	}
}

// A device that runs the role with no storage to keep its posture in is refused, since its posture would come back
// legacy after a restart.
func TestBridgeWithoutStorageRefused(t *testing.T) {
	role := newTestingDeviceRole(t)
	space := newNetworkSpace(context.Background(), *NewNetworkSpaceKey("device-role-memory.test", "test"),
		NetworkSpaceValues{ApiUrl: "http://127.0.0.1:1", PlatformUrl: "ws://127.0.0.1:1"}, "")
	t.Cleanup(space.close)
	settings := DefaultDeviceLocalSettings()
	settings.DisableLogging = true
	settings.AllowProvider = false
	settings.EnableBridge = true
	device, err := newDeviceLocalWithOverrides(space, role.clientJwt, "device-role", "test", "0", NewId(), settings, connect.NewId())
	if err == nil {
		testingJoinDeviceRole(t, device)
		t.Fatal("a device ran the role with nowhere to keep its posture")
	}
}
