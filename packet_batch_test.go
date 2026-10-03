// Packet batch tests lock the mobile framing, callback, routing, and pool
// ownership contracts used by every native app adapter.
package sdk

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// A successful fake copies the borrowed observation and takes ownership of
// each pooled send buffer exactly like a production user NAT.
type packetBatchTestUserNat struct {
	capture        bool
	packetCount    int
	batchCallCount int
	batchSizes     []int
	packets        [][]byte
}

// Every valid packet is accepted and returned to its pool after observation.
func (self *packetBatchTestUserNat) SendPacket(
	source connect.TransferPath,
	provideMode protocol.ProvideMode,
	packet []byte,
	timeout time.Duration,
) bool {
	self.packetCount += 1
	if self.capture {
		self.packets = append(self.packets, bytes.Clone(packet))
	}
	connect.MessagePoolReturn(packet)
	return true
}

// A complete burst crosses the fake route once and transfers every packet.
func (self *packetBatchTestUserNat) SendPacketBatch(
	source connect.TransferPath,
	provideMode protocol.ProvideMode,
	packets [][]byte,
	timeout time.Duration,
) int {
	self.batchCallCount += 1
	if self.capture {
		self.batchSizes = append(self.batchSizes, len(packets))
	}
	for _, packet := range packets {
		self.packetCount += 1
		if self.capture {
			self.packets = append(self.packets, bytes.Clone(packet))
		}
		connect.MessagePoolReturn(packet)
	}
	return len(packets)
}

// No resources are held by the fake.
func (self *packetBatchTestUserNat) Close() {
}

// No provider ordering exists in the fake.
func (self *packetBatchTestUserNat) Shuffle() {
}

// The fake applies no packet security policy.
func (self *packetBatchTestUserNat) SecurityPolicyStats(reset bool) connect.SecurityPolicyStats {
	return connect.SecurityPolicyStats{}
}

// The fake has no local security policy to configure.
func (self *packetBatchTestUserNat) SetLocalSecurityBypass(localSecurityBypass bool) {
}

// One callback recorder verifies the exported mobile wrapper without retaining
// its borrowed PacketBatch object.
type packetBatchTestReceiver struct {
	callCount   int
	packetCount int
	versions    []int
}

// The packed recorder copies the borrowed buffer for later assertions.
type packetBatchBytesTestReceiver struct {
	callCount        int
	packetBatchBytes []byte
}

// The callback consumes the entire native representation in one call.
func (self *packetBatchBytesTestReceiver) ReceivePacketBatch(packetBatchBytes []byte) {
	self.callCount += 1
	self.packetBatchBytes = bytes.Clone(packetBatchBytes)
}

// Metadata and packet count are consumed synchronously.
func (self *packetBatchTestReceiver) ReceivePackets(packetBatch *PacketBatch) {
	self.callCount += 1
	self.packetCount += packetBatch.Len()
	for packetIndex := 0; packetIndex < packetBatch.Len(); packetIndex += 1 {
		self.versions = append(self.versions, packetBatch.IpVersion(packetIndex))
	}
}

// A compact batch fixture uses the exact uint16 framing consumed by the
// gomobile and cgo entry point.
func packetBatchTestEncode(packets [][]byte) []byte {
	byteCount := 0
	for _, packet := range packets {
		byteCount += 2 + len(packet)
	}
	packetBatchBytes := make([]byte, byteCount)
	offset := 0
	for _, packet := range packets {
		binary.BigEndian.PutUint16(
			packetBatchBytes[offset:offset+2],
			uint16(len(packet)),
		)
		offset += 2
		copy(packetBatchBytes[offset:offset+len(packet)], packet)
		offset += len(packet)
	}
	return packetBatchBytes
}

// A minimal device isolates the batch boundary from destination discovery.
func newPacketBatchTestDevice(userNat connect.UserNatClient) *DeviceLocal {
	device := &DeviceLocal{
		clientId:                    connect.NewId(),
		settings:                    DefaultDeviceLocalSettings(),
		stats:                       newDeviceStats(),
		receiveCallbacks:            connect.NewCallbackList[connect.ReceivePacketFunction](),
		receivePacketsCallbacks:     connect.NewCallbackList[connect.ReceivePacketsFunction](),
		receivePacketBatchCallbacks: connect.NewCallbackList[receivePacketBatchFunction](),
	}
	device.sendRoute.Store(&deviceLocalSendRoute{remoteUserNatClient: userNat})
	return device
}

// The exported view derives stable IP metadata and handles invalid indexes.
func TestPacketBatchMetadata(t *testing.T) {
	ipv4Packet := make([]byte, 20)
	ipv4Packet[0] = 0x45
	ipv4Packet[9] = 17
	ipv6Packet := make([]byte, 40)
	ipv6Packet[0] = 0x60
	ipv6Packet[6] = 6
	packetBatch := &PacketBatch{packets: [][]byte{ipv4Packet, ipv6Packet}}
	if packetBatch.Len() != 2 ||
		packetBatch.IpVersion(0) != 4 ||
		packetBatch.IpProtocol(0) != IpProtocolUdp ||
		packetBatch.IpVersion(1) != 6 ||
		packetBatch.IpProtocol(1) != IpProtocolTcp ||
		packetBatch.Get(2) != nil {
		t.Fatalf("unexpected packet batch metadata")
	}
}

// One upstream batch produces one mobile callback while an independently
// registered singular observer still sees each packet exactly once.
func TestDeviceLocalReceivePacketsPreservesCallbackSemantics(t *testing.T) {
	device := newPacketBatchTestDevice(&packetBatchTestUserNat{})
	receiver := &packetBatchTestReceiver{}
	sub := device.AddReceivePackets(receiver)
	defer sub.Close()
	packedReceiver := &packetBatchBytesTestReceiver{}
	packedSub := device.AddReceivePacketBatch(packedReceiver)
	defer packedSub.Close()
	singularPacketCount := 0
	unsub := device.AddReceivePacketCallback(func(
		source connect.TransferPath,
		provideMode protocol.ProvideMode,
		ipPath *connect.IpPath,
		packet []byte,
	) {
		singularPacketCount += 1
	})
	defer unsub()
	ipv4Packet := make([]byte, 20)
	ipv4Packet[0] = 0x45
	ipv6Packet := make([]byte, 40)
	ipv6Packet[0] = 0x60
	device.receivePackets(
		connect.TransferPath{},
		protocol.ProvideMode_Network,
		nil,
		[][]byte{ipv4Packet, ipv6Packet},
	)
	if receiver.callCount != 1 || receiver.packetCount != 2 || singularPacketCount != 2 {
		t.Fatalf(
			"batch calls=%d batch packets=%d singular packets=%d, want 1/2/2",
			receiver.callCount,
			receiver.packetCount,
			singularPacketCount,
		)
	}
	if packedReceiver.callCount != 1 ||
		!bytes.Equal(packedReceiver.packetBatchBytes, packetBatchTestEncode([][]byte{ipv4Packet, ipv6Packet})) {
		t.Fatalf(
			"packed calls=%d bytes=%x",
			packedReceiver.callCount,
			packedReceiver.packetBatchBytes,
		)
	}
}

// A singular upstream callback still produces one valid packed burst.
func TestDeviceLocalReceivePacketProducesPackedBatch(t *testing.T) {
	device := newPacketBatchTestDevice(&packetBatchTestUserNat{})
	receiver := &packetBatchBytesTestReceiver{}
	sub := device.AddReceivePacketBatch(receiver)
	defer sub.Close()
	packet := make([]byte, 20)
	packet[0] = 0x45
	device.receive(
		connect.TransferPath{},
		protocol.ProvideMode_Network,
		nil,
		packet,
	)
	if receiver.callCount != 1 ||
		!bytes.Equal(receiver.packetBatchBytes, packetBatchTestEncode([][]byte{packet})) {
		t.Fatalf("packed calls=%d bytes=%x", receiver.callCount, receiver.packetBatchBytes)
	}
}

// Framing is fully validated before the route sees anything, and a valid burst
// retains packet order while balancing every pooled copy.
func TestDeviceLocalSendPacketBatchValidatesBeforeSending(t *testing.T) {
	userNat := &packetBatchTestUserNat{capture: true}
	device := newPacketBatchTestDevice(userNat)
	packets := [][]byte{[]byte("first"), []byte("second")}
	if sentPacketCount := device.SendPacketBatch(packetBatchTestEncode(packets)); sentPacketCount != 2 {
		t.Fatalf("sent packet count=%d, want 2", sentPacketCount)
	}
	if len(userNat.packets) != len(packets) ||
		!bytes.Equal(userNat.packets[0], packets[0]) ||
		!bytes.Equal(userNat.packets[1], packets[1]) {
		t.Fatalf("routed packets=%q, want %q", userNat.packets, packets)
	}
	if userNat.batchCallCount != 1 ||
		len(userNat.batchSizes) != 1 ||
		userNat.batchSizes[0] != len(packets) {
		t.Fatalf(
			"batch calls=%d sizes=%v, want 1/%d",
			userNat.batchCallCount,
			userNat.batchSizes,
			len(packets),
		)
	}
	malformed := packetBatchTestEncode([][]byte{[]byte("valid")})
	malformed = append(malformed, 0)
	if sentPacketCount := device.SendPacketBatch(malformed); sentPacketCount != 0 {
		t.Fatalf("malformed sent packet count=%d, want 0", sentPacketCount)
	}
	if len(userNat.packets) != len(packets) {
		t.Fatalf("malformed framing partially sent %d packets", len(userNat.packets)-len(packets))
	}
	if userNat.batchCallCount != 1 {
		t.Fatalf("malformed framing reached route: batch calls=%d, want 1", userNat.batchCallCount)
	}
}

// Singular calls provide the pre-migration boundary baseline.
func BenchmarkDeviceLocalSendPacketsSingular(benchmark *testing.B) {
	userNat := &packetBatchTestUserNat{}
	device := newPacketBatchTestDevice(userNat)
	packet := make([]byte, 1380)
	benchmark.SetBytes(int64(32 * len(packet)))
	benchmark.ReportAllocs()
	benchmark.ResetTimer()
	for range benchmark.N {
		for range 32 {
			device.SendPacket(packet, int32(len(packet)))
		}
		userNat.packetCount = 0
	}
}

// The packed call measures the native-boundary and shared-route replacement.
func BenchmarkDeviceLocalSendPacketBatch(benchmark *testing.B) {
	userNat := &packetBatchTestUserNat{}
	device := newPacketBatchTestDevice(userNat)
	packet := make([]byte, 1380)
	packets := make([][]byte, 32)
	for packetIndex := range packets {
		packets[packetIndex] = packet
	}
	packetBatchBytes := packetBatchTestEncode(packets)
	benchmark.SetBytes(int64(32 * len(packet)))
	benchmark.ReportAllocs()
	benchmark.ResetTimer()
	for range benchmark.N {
		device.SendPacketBatch(packetBatchBytes)
		userNat.packetCount = 0
	}
}
