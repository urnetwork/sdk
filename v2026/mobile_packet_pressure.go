package sdk

import (
	"sync/atomic"

	"github.com/urnetwork/connect/v2026"
)

const (
	// Keep the measured 1-MiB aggregate gate. A 768-root experiment improved
	// H1 bulk traffic but reached 28.41 MiB while an H3 page was stalled, and a
	// 4096-root H1 diagnostic removed pressure drops without improving goodput.
	// The 24-MiB profile spends its headroom on the actual H1 bottleneck instead
	// of weakening the process-wide safety backstop.
	mobilePacketPressureMaxOutstandingByteCount uint64 = 512 * 2048
	// When provider work is disabled and the client is explicitly H1, reserve
	// another 1 MiB for ACK-only TCP packets. The small packet class makes that
	// space worth up to 4096 ordinary 256-byte ACK roots instead of 512 full-MTU
	// roots, without admitting data, SYN/FIN/RST, H3/Auto, or provider-on
	// traffic past the 1-MiB base gate. A measured 3-MiB arm eliminated ACK
	// drops but reduced bulk goodput and increased Pack handoff loss, so it is
	// intentionally not retained.
	mobilePacketPressureH1AckMaxOutstandingByteCount uint64 = 2 * 1024 * 1024
	// Snapshotting takes the eight small/full packet-class shard locks. Sample one in four
	// ingress calls while below the ceiling, then every call while overloaded
	// so traffic resumes promptly as ownership drains.
	mobilePacketPressureSampleEvery uint64 = 4
)

// mobilePacketPressureGate is created only for the <=24-MiB mobile profile.
// Consequently the server/connect and server/proxy packet paths pay neither
// an atomic operation nor a pool snapshot for this mobile overload policy.
type mobilePacketPressureGate struct {
	maxOutstandingBytes      uint64
	h1AckMaxOutstandingBytes uint64
	h1AckReserveEnabled      atomic.Bool
	sampleEvery              uint64
	calls                    atomic.Uint64
	cached                   atomic.Uint64
	sample                   func() uint64
}

func newMobilePacketPressureGate() *mobilePacketPressureGate {
	return &mobilePacketPressureGate{
		maxOutstandingBytes:      mobilePacketPressureMaxOutstandingByteCount,
		h1AckMaxOutstandingBytes: mobilePacketPressureH1AckMaxOutstandingByteCount,
		sampleEvery:              mobilePacketPressureSampleEvery,
		sample: func() uint64 {
			return uint64(connect.MessagePoolDeviceTunEgressOutstandingByteCount())
		},
	}
}

func newMobilePacketPressureGateForPlatform(
	memoryTargetByteCount ByteCount,
	mobile bool,
) *mobilePacketPressureGate {
	if !mobileLowMemoryPolicyEnabledForPlatform(memoryTargetByteCount, mobile) {
		return nil
	}
	return newMobilePacketPressureGate()
}

func (self *mobilePacketPressureGate) shouldDrop() bool {
	if self == nil || self.maxOutstandingBytes == 0 || self.sample == nil {
		return false
	}
	return self.outstanding() >= self.maxOutstandingBytes
}

// outstanding preserves the sparse low-pressure sampling policy shared by
// single-packet and batch admission. Once the base ceiling is reached every
// call resamples, so a drained overload reopens promptly.
func (self *mobilePacketPressureGate) outstanding() uint64 {
	sampleEvery := self.sampleEvery
	if sampleEvery == 0 {
		sampleEvery = 1
	}
	call := self.calls.Add(1)
	outstanding := self.cached.Load()
	if call == 1 || outstanding >= self.maxOutstandingBytes || call%sampleEvery == 0 {
		outstanding = self.sample()
		self.cached.Store(outstanding)
	}
	return outstanding
}

// admitPacket gives a single ACK-only TCP packet the same provider-off H1
// reserve used by native TUN batches. Classification runs only after the base
// gate is full, so ordinary traffic keeps the allocation-free cached fast path.
func (self *mobilePacketPressureGate) admitPacket(packet []byte) (bool, bool) {
	if self == nil || self.maxOutstandingBytes == 0 || self.sample == nil {
		return true, false
	}
	outstanding := self.outstanding()
	if outstanding < self.maxOutstandingBytes {
		return true, false
	}
	if self.h1AckReserveEnabled.Load() &&
		outstanding < self.h1AckMaxOutstandingBytes &&
		connect.IsTcpAckOnlyPacket(packet) {
		return true, true
	}
	return false, false
}

func (self *mobilePacketPressureGate) setH1AckReserveEnabled(enabled bool) {
	if self != nil {
		self.h1AckReserveEnabled.Store(enabled)
	}
}

// admitOwnedPacketBatchBase returns the in-order prefix of an already-owned
// native TUN batch that fits below the packet-byte ceiling. The current pool
// snapshot includes this batch, so subtracting the exact small/full root bytes
// recovers pre-call pressure. Admitting a prefix avoids throwing away earlier
// TCP segments merely because later members crossed the ceiling.
func (self *mobilePacketPressureGate) admitOwnedPacketBatchBase(packets [][]byte) (int, uint64) {
	if len(packets) == 0 {
		return 0, 0
	}
	if self == nil || self.maxOutstandingBytes == 0 || self.sample == nil {
		return len(packets), 0
	}
	ownedBytes := uint64(0)
	for _, packet := range packets {
		ownedBytes += uint64(connect.MessagePoolPacketRootByteCount(packet))
	}
	sampleEvery := self.sampleEvery
	if sampleEvery == 0 {
		sampleEvery = 1
	}
	call := self.calls.Add(1)
	outstanding := self.cached.Load()
	if call != 1 && outstanding < self.maxOutstandingBytes &&
		call%sampleEvery != 0 && ownedBytes < self.maxOutstandingBytes-outstanding {
		return len(packets), 0
	}
	outstanding = self.sample()
	self.cached.Store(outstanding)
	preBatchOutstandingBytes := uint64(0)
	if ownedBytes < outstanding {
		preBatchOutstandingBytes = outstanding - ownedBytes
	}
	if self.maxOutstandingBytes <= preBatchOutstandingBytes {
		return 0, preBatchOutstandingBytes
	}
	usedBytes := preBatchOutstandingBytes
	admitted := 0
	for _, packet := range packets {
		rootBytes := uint64(connect.MessagePoolPacketRootByteCount(packet))
		if rootBytes != 0 && self.maxOutstandingBytes-usedBytes < rootBytes {
			break
		}
		usedBytes += rootBytes
		admitted += 1
	}
	return admitted, preBatchOutstandingBytes
}

// admitOwnedPacketBatch extends the ordinary ordered prefix with ACK-only TCP
// packets found anywhere in the otherwise-rejected suffix. It compacts those
// ACKs immediately after the prefix, preserving their relative order. An ACK
// can only overtake packets that this same admission decision rejects, so no
// two delivered packets are reordered. The returned second count is the
// number admitted from the H1-only reserve.
//
// The caller transfers ownership of every packet and permits this in-place
// compaction of the borrowed outer slice. Rejected owners remain in the suffix
// for the caller to return.
func (self *mobilePacketPressureGate) admitOwnedPacketBatch(packets [][]byte) (int, int) {
	admitted, preBatchOutstandingBytes := self.admitOwnedPacketBatchBase(packets)
	if self == nil || admitted == len(packets) ||
		!self.h1AckReserveEnabled.Load() ||
		self.h1AckMaxOutstandingBytes <= preBatchOutstandingBytes {
		return admitted, 0
	}
	usedBytes := preBatchOutstandingBytes
	for _, packet := range packets[:admitted] {
		usedBytes += uint64(connect.MessagePoolPacketRootByteCount(packet))
	}
	baseAdmitted := admitted
	for packetIndex := baseAdmitted; packetIndex < len(packets); packetIndex++ {
		if !connect.IsTcpAckOnlyPacket(packets[packetIndex]) {
			continue
		}
		rootBytes := uint64(connect.MessagePoolPacketRootByteCount(packets[packetIndex]))
		if rootBytes != 0 && self.h1AckMaxOutstandingBytes-usedBytes < rootBytes {
			continue
		}
		packets[admitted], packets[packetIndex] = packets[packetIndex], packets[admitted]
		usedBytes += rootBytes
		admitted++
	}
	return admitted, admitted - baseAdmitted
}
