// Packet batches amortize the Go/native boundary while retaining ordinary IP
// packet framing. Every packet exposed to a receive callback is borrowed for
// that callback only.
package sdk

// ReceivePackets accepts one borrowed packet burst from the tunnel. Callers
// must copy a packet before retaining it past the callback.
type ReceivePackets interface {
	ReceivePackets(packetBatch *PacketBatch)
}

// ReceivePacketBatch accepts one borrowed uint16 length-prefixed packet burst.
// The byte slice is valid only for the duration of the callback.
type ReceivePacketBatch interface {
	ReceivePacketBatch(packetBatchBytes []byte)
}

// The internal form keeps the packed callback out of connect's public API.
type receivePacketBatchFunction func(packetBatchBytes []byte)

// PacketBatch is a borrowed, read-only view over one packet burst. It is also
// the mobile-safe adapter for the SDK's internal [][]byte representation.
type PacketBatch struct {
	packets [][]byte
}

// Len reports the number of packets available to Get.
func (self *PacketBatch) Len() int {
	if self == nil {
		return 0
	}
	return len(self.packets)
}

// Get returns one borrowed packet or nil for an invalid index.
func (self *PacketBatch) Get(index int) []byte {
	if self == nil || index < 0 || len(self.packets) <= index {
		return nil
	}
	return self.packets[index]
}

// IpVersion returns 4, 6, or zero when the packet header is unavailable.
func (self *PacketBatch) IpVersion(index int) int {
	packet := self.Get(index)
	if len(packet) == 0 {
		return 0
	}
	switch packet[0] >> 4 {
	case 4:
		return 4
	case 6:
		return 6
	default:
		return 0
	}
}

// IpProtocol returns the packet's immediate IPv4 protocol or IPv6 next-header
// value mapped to the SDK's public protocol constants.
func (self *PacketBatch) IpProtocol(index int) IpProtocol {
	packet := self.Get(index)
	protocolNumber := byte(0)
	switch self.IpVersion(index) {
	case 4:
		if len(packet) <= 9 {
			return IpProtocolUnknown
		}
		protocolNumber = packet[9]
	case 6:
		if len(packet) <= 6 {
			return IpProtocolUnknown
		}
		protocolNumber = packet[6]
	default:
		return IpProtocolUnknown
	}
	switch protocolNumber {
	case 17:
		return IpProtocolUdp
	case 6:
		return IpProtocolTcp
	default:
		return IpProtocolUnknown
	}
}
