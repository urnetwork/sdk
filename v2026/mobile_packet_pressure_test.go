package sdk

import (
	"encoding/binary"
	"slices"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

func mobilePressureTcp4Packet(flags byte, payload []byte) []byte {
	packet := make([]byte, 40+len(payload))
	packet[0] = 0x45
	binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
	packet[8] = 64
	packet[9] = 6
	copy(packet[12:16], []byte{10, 0, 0, 2})
	copy(packet[16:20], []byte{203, 0, 113, 10})
	tcp := packet[20:]
	binary.BigEndian.PutUint16(tcp[0:2], 47001)
	binary.BigEndian.PutUint16(tcp[2:4], 443)
	tcp[12] = 5 << 4
	tcp[13] = flags
	binary.BigEndian.PutUint16(tcp[14:16], 65535)
	copy(tcp[20:], payload)
	return packet
}

func TestMobilePacketPressureGateSamplesSparselyAndRecoversPromptly(t *testing.T) {
	samples := []uint64{100, mobilePacketPressureMaxOutstandingByteCount, 100}
	sampleIndex := 0
	gate := &mobilePacketPressureGate{
		maxOutstandingBytes: mobilePacketPressureMaxOutstandingByteCount,
		sampleEvery:         mobilePacketPressureSampleEvery,
		sample: func() uint64 {
			value := samples[sampleIndex]
			sampleIndex += 1
			return value
		},
	}

	if gate.shouldDrop() {
		t.Fatal("first low-pressure sample rejected traffic")
	}
	if gate.shouldDrop() || gate.shouldDrop() {
		t.Fatal("cached low-pressure sample rejected traffic")
	}
	if !gate.shouldDrop() {
		t.Fatal("fourth call did not observe the pressure ceiling")
	}
	if gate.shouldDrop() {
		t.Fatal("overloaded gate did not resample and reopen promptly")
	}
	if sampleIndex != len(samples) {
		t.Fatalf("sample calls = %d, want %d", sampleIndex, len(samples))
	}
}

func TestMobilePacketPressureProductionGateIgnoresInboundPacketRoots(t *testing.T) {
	baseline := uint64(connect.MessagePoolDeviceTunEgressOutstandingByteCount())
	inbound := connect.MessagePoolGet(80)
	defer connect.MessagePoolReturn(inbound)

	gate := newMobilePacketPressureGate()
	gate.maxOutstandingBytes = baseline + 256
	gate.sampleEvery = 1
	if gate.shouldDrop() {
		t.Fatal("unclassified inbound packet root consumed device-egress pressure")
	}
	if !connect.MessagePoolMarkDeviceTunEgress(inbound) {
		t.Fatal("packet root could not be classified as device TUN egress")
	}
	if !gate.shouldDrop() {
		t.Fatal("device-egress packet root did not close the production pressure gate")
	}
	connect.MessagePoolReturn(inbound)
	inbound = nil
	if gate.shouldDrop() {
		t.Fatal("production pressure gate did not reopen after egress ownership drained")
	}
}

func TestMobilePacketPressureGateIsMobileTwentyFourMiBOnly(t *testing.T) {
	if mobilePacketPressureMaxOutstandingByteCount != 1024*1024 {
		t.Fatalf("packet pressure ceiling = %d, want H3-safe 1-MiB ceiling", mobilePacketPressureMaxOutstandingByteCount)
	}
	if mobilePacketPressureH1AckMaxOutstandingByteCount != 2*1024*1024 {
		t.Fatalf("H1 ACK pressure ceiling = %d, want 2-MiB provider-off ceiling", mobilePacketPressureH1AckMaxOutstandingByteCount)
	}
	if gate := newMobilePacketPressureGateForPlatform(
		mobileSteadyMemoryTargetByteCount,
		true,
	); gate == nil {
		t.Fatal("24-MiB mobile target did not create a pressure gate")
	}
	if gate := newMobilePacketPressureGateForPlatform(20*1024*1024, true); gate == nil {
		t.Fatal("tighter mobile target did not retain the pressure gate")
	}
	for _, testCase := range []struct {
		name   string
		target ByteCount
		mobile bool
	}{
		{name: "server", target: mobileSteadyMemoryTargetByteCount, mobile: false},
		{name: "larger mobile", target: 32 * 1024 * 1024, mobile: true},
		{name: "disabled mobile", target: 0, mobile: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if gate := newMobilePacketPressureGateForPlatform(
				testCase.target,
				testCase.mobile,
			); gate != nil {
				t.Fatal("created a mobile pressure gate outside the 24-MiB mobile profile")
			}
		})
	}
}

func TestMobilePacketPressureH1ReserveCompactsAckOnlyRejectedSuffix(t *testing.T) {
	packetValues := [][]byte{
		{0},
		{1},
		mobilePressureTcp4Packet(0x10, nil),
		mobilePressureTcp4Packet(0x18, []byte("data-a")),
		mobilePressureTcp4Packet(0x10, nil),
		mobilePressureTcp4Packet(0x18, []byte("data-b")),
		mobilePressureTcp4Packet(0x10, nil),
	}
	newGate := func(enabled bool) *mobilePacketPressureGate {
		gate := &mobilePacketPressureGate{
			maxOutstandingBytes:      10 * 256,
			h1AckMaxOutstandingBytes: 14 * 256,
			sampleEvery:              1,
			// Seven roots belong to this batch, so the pre-batch pressure is 8.
			sample: func() uint64 { return 15 * 256 },
		}
		gate.setH1AckReserveEnabled(enabled)
		return gate
	}
	copyPackets := func() [][]byte {
		packets := make([][]byte, len(packetValues))
		for index, packet := range packetValues {
			packets[index] = connect.MessagePoolCopy(packet)
		}
		return packets
	}
	returnPackets := func(packets [][]byte) {
		for _, packet := range packets {
			connect.MessagePoolReturn(packet)
		}
	}

	disabledPackets := copyPackets()
	if admitted, ackAdmitted := newGate(false).admitOwnedPacketBatch(disabledPackets); admitted != 2 || ackAdmitted != 0 {
		t.Fatalf("disabled H1 ACK reserve admitted=(%d, %d), want (2, 0)", admitted, ackAdmitted)
	}
	returnPackets(disabledPackets)

	packets := copyPackets()
	wantAckPackets := [][]byte{packets[2], packets[4], packets[6]}
	if admitted, ackAdmitted := newGate(true).admitOwnedPacketBatch(packets); admitted != 5 || ackAdmitted != 3 {
		t.Fatalf("enabled H1 ACK reserve admitted=(%d, %d), want (5, 3)", admitted, ackAdmitted)
	}
	for ackIndex, want := range wantAckPackets {
		if got := packets[2+ackIndex]; !slices.Equal(got, want) {
			t.Fatalf("compacted ACK %d changed or reordered", ackIndex)
		}
	}
	for _, rejected := range packets[5:] {
		if connect.IsTcpAckOnlyPacket(rejected) {
			t.Fatal("an ACK-only packet remained rejected below the reserve limit")
		}
	}

	gate := newGate(true)
	if allocations := testing.AllocsPerRun(100, func() {
		gate.calls.Store(0)
		_, _ = gate.admitOwnedPacketBatch(packets)
	}); allocations != 0 {
		t.Fatalf("H1 ACK reserve allocated %.0f objects, want 0", allocations)
	}
	returnPackets(packets)
}

func TestMobilePacketPressurePerformanceModeRequiresH1ProviderOff(t *testing.T) {
	gate := &mobilePacketPressureGate{}
	device := &DeviceLocal{
		mobilePacketPressure: gate,
		transportSettings:    &TransportSettings{Mode: TransportModeH1},
		provideMode:          ProvideModeNone,
	}
	device.updateMobilePacketPerformanceModeWithLock()
	if !gate.h1AckReserveEnabled.Load() {
		t.Fatal("provider-off H1 did not enable ACK reserve")
	}
	device.transportSettings.Mode = TransportModeH3
	device.updateMobilePacketPerformanceModeWithLock()
	if gate.h1AckReserveEnabled.Load() {
		t.Fatal("H3 retained H1 ACK reserve")
	}
	device.transportSettings.Mode = TransportModeH1
	device.provideMode = ProvideModePublic
	device.updateMobilePacketPerformanceModeWithLock()
	if gate.h1AckReserveEnabled.Load() {
		t.Fatal("provider-on H1 retained provider-off ACK reserve")
	}
}

func TestMobilePacketPressureGateDisabledAndHotPathDoNotAllocate(t *testing.T) {
	var nilGate *mobilePacketPressureGate
	if nilGate.shouldDrop() {
		t.Fatal("nil gate rejected traffic")
	}
	if (&mobilePacketPressureGate{}).shouldDrop() {
		t.Fatal("disabled gate rejected traffic")
	}

	gate := &mobilePacketPressureGate{
		maxOutstandingBytes: mobilePacketPressureMaxOutstandingByteCount,
		sampleEvery:         1,
		sample:              func() uint64 { return 0 },
	}
	packets := make([][]byte, 8)
	for index := range packets {
		packets[index] = connect.MessagePoolCopy([]byte{byte(index)})
		defer connect.MessagePoolReturn(packets[index])
	}
	if allocations := testing.AllocsPerRun(100, func() {
		_ = gate.shouldDrop()
		_, _ = gate.admitOwnedPacketBatch(packets)
	}); allocations != 0 {
		t.Fatalf("pressure gate allocated %.0f objects, want 0", allocations)
	}
}

func TestDeviceLocalPacketPressureBatchRejectsAndReturnsOwnership(t *testing.T) {
	packetCount := 2
	gate := &mobilePacketPressureGate{
		maxOutstandingBytes: mobilePacketPressureMaxOutstandingByteCount,
		sampleEvery:         1,
		sample: func() uint64 {
			// The production snapshot includes this already-owned batch.
			return mobilePacketPressureMaxOutstandingByteCount + uint64(packetCount*2048)
		},
	}
	device := &DeviceLocal{mobilePacketPressure: gate}
	baseline := connect.MessagePoolPacketOutstandingCount()
	directionalBaseline := connect.MessagePoolDeviceTunEgressOutstandingByteCount()
	packets := [][]byte{
		connect.MessagePoolGet(connect.DefaultMtu),
		connect.MessagePoolGet(connect.DefaultMtu),
	}
	if got := connect.MessagePoolPacketOutstandingCount(); got != baseline+uint64(len(packets)) {
		t.Fatalf("packet roots before pressure rejection = %d, want %d", got, baseline+uint64(len(packets)))
	}
	if sent := device.sendPacketsNoCopy(packets); sent != 0 {
		t.Fatalf("pressure-rejected batch sent %d packets", sent)
	}
	if got := connect.MessagePoolPacketOutstandingCount(); got != baseline {
		t.Fatalf("packet roots after pressure rejection = %d, want %d", got, baseline)
	}
	if got := connect.MessagePoolDeviceTunEgressOutstandingByteCount(); got != directionalBaseline {
		t.Fatalf("device-egress roots after pressure rejection = %d, want %d", got, directionalBaseline)
	}
	if got := device.mobilePacketPressureDropCount.Load(); got != int64(len(packets)) {
		t.Fatalf("pressure drop count = %d, want %d", got, len(packets))
	}
}

func TestDeviceLocalPacketPressureAdmitsOrderedPrefix(t *testing.T) {
	const (
		packetCount   = 8
		availableRoot = 4
	)
	userNat := &packetBatchTestUserNat{capture: true}
	device := newPacketBatchTestDevice(userNat)
	device.mobilePacketPressure = &mobilePacketPressureGate{
		maxOutstandingBytes: mobilePacketPressureMaxOutstandingByteCount,
		sampleEvery:         1,
		sample: func() uint64 {
			preBatch := mobilePacketPressureMaxOutstandingByteCount - availableRoot*256
			return preBatch + packetCount*256
		},
	}
	baseline := connect.MessagePoolPacketOutstandingCount()
	packets := make([][]byte, packetCount)
	for packetIndex := range packets {
		packets[packetIndex] = connect.MessagePoolCopy([]byte{byte(packetIndex)})
	}

	if sent := device.sendPacketsNoCopy(packets); sent != availableRoot {
		t.Fatalf("pressure-limited batch sent %d packets, want %d", sent, availableRoot)
	}
	if userNat.batchCallCount != 1 || len(userNat.batchSizes) != 1 ||
		userNat.batchSizes[0] != availableRoot {
		t.Fatalf("route batches=%d sizes=%v, want one prefix of %d", userNat.batchCallCount, userNat.batchSizes, availableRoot)
	}
	for packetIndex, packet := range userNat.packets {
		if len(packet) != 1 || packet[0] != byte(packetIndex) {
			t.Fatalf("admitted packet %d = %v, want ordered prefix", packetIndex, packet)
		}
	}
	if got := device.mobilePacketPressureDropCount.Load(); got != packetCount-availableRoot {
		t.Fatalf("pressure drop count = %d, want %d", got, packetCount-availableRoot)
	}
	if got := connect.MessagePoolPacketOutstandingCount(); got != baseline {
		t.Fatalf("packet roots after partial admission = %d, want %d", got, baseline)
	}
}

func TestDeviceLocalPacketPressureH1AckReserveReturnsRejectedOwnership(t *testing.T) {
	userNat := &packetBatchTestUserNat{capture: true}
	device := newPacketBatchTestDevice(userNat)
	gate := &mobilePacketPressureGate{
		maxOutstandingBytes:      10 * 256,
		h1AckMaxOutstandingBytes: 14 * 256,
		sampleEvery:              1,
		sample:                   func() uint64 { return 15 * 256 },
	}
	gate.setH1AckReserveEnabled(true)
	device.mobilePacketPressure = gate
	packetValues := [][]byte{
		{0},
		{1},
		mobilePressureTcp4Packet(0x10, nil),
		mobilePressureTcp4Packet(0x18, []byte("data-a")),
		mobilePressureTcp4Packet(0x10, nil),
		mobilePressureTcp4Packet(0x18, []byte("data-b")),
		mobilePressureTcp4Packet(0x10, nil),
	}
	baseline := connect.MessagePoolPacketOutstandingCount()
	packets := make([][]byte, len(packetValues))
	for index, packet := range packetValues {
		packets[index] = connect.MessagePoolCopy(packet)
	}
	if sent := device.sendPacketsNoCopy(packets); sent != 5 {
		t.Fatalf("H1 ACK-priority batch sent %d packets, want 5", sent)
	}
	if got := device.mobilePacketPressureAckAdmits.Load(); got != 3 {
		t.Fatalf("H1 ACK reserve admissions = %d, want 3", got)
	}
	if got := device.mobilePacketPressureDropCount.Load(); got != 2 {
		t.Fatalf("pressure drops = %d, want 2", got)
	}
	if got := device.mobilePacketPressureAckDrops.Load(); got != 0 {
		t.Fatalf("ACK-only pressure drops = %d, want 0", got)
	}
	if got := device.mobilePacketPressureOtherDrops.Load(); got != 2 {
		t.Fatalf("other pressure drops = %d, want 2", got)
	}
	wantDropBytes := int64(len(packetValues[3]) + len(packetValues[5]))
	if got := device.mobilePacketPressureDropBytes.Load(); got != wantDropBytes {
		t.Fatalf("pressure drop bytes = %d, want %d", got, wantDropBytes)
	}
	if got := connect.MessagePoolPacketOutstandingCount(); got != baseline {
		t.Fatalf("packet roots after H1 ACK reserve = %d, want %d", got, baseline)
	}
}

func TestDeviceLocalPacketPressureSingleRejectLeavesOwnershipWithCaller(t *testing.T) {
	device := &DeviceLocal{mobilePacketPressure: &mobilePacketPressureGate{
		maxOutstandingBytes: 1,
		sampleEvery:         1,
		sample:              func() uint64 { return 1 },
	}}
	baseline := connect.MessagePoolPacketOutstandingCount()
	directionalBaseline := connect.MessagePoolDeviceTunEgressOutstandingByteCount()
	packet := connect.MessagePoolGet(connect.DefaultMtu)
	if device.sendPacket(packet) {
		t.Fatal("single packet passed a closed pressure gate")
	}
	if got := connect.MessagePoolPacketOutstandingCount(); got != baseline+1 {
		t.Fatalf("single-packet failure stole caller ownership: roots=%d want=%d", got, baseline+1)
	}
	if got := connect.MessagePoolDeviceTunEgressOutstandingByteCount(); got != directionalBaseline+2048 {
		t.Fatalf("failed single packet lost egress classification: bytes=%d", got)
	}
	connect.MessagePoolReturn(packet)
	if got := connect.MessagePoolPacketOutstandingCount(); got != baseline {
		t.Fatalf("single packet root after caller return = %d, want %d", got, baseline)
	}
	if got := connect.MessagePoolDeviceTunEgressOutstandingByteCount(); got != directionalBaseline {
		t.Fatalf("single packet egress bytes after caller return = %d, want %d", got, directionalBaseline)
	}
	if got := device.mobilePacketPressureDropCount.Load(); got != 1 {
		t.Fatalf("single packet pressure drop count = %d, want 1", got)
	}
}

func TestDeviceLocalPacketPressureSingleAckUsesH1Reserve(t *testing.T) {
	userNat := &packetBatchTestUserNat{capture: true}
	device := newPacketBatchTestDevice(userNat)
	gate := &mobilePacketPressureGate{
		maxOutstandingBytes:      10,
		h1AckMaxOutstandingBytes: 14,
		sampleEvery:              1,
		sample:                   func() uint64 { return 12 },
	}
	gate.setH1AckReserveEnabled(true)
	device.mobilePacketPressure = gate

	ack := mobilePressureTcp4Packet(0x10, nil)
	if !device.sendPacket(ack) {
		t.Fatal("single ACK-only packet did not use the H1 reserve")
	}
	if got := device.mobilePacketPressureAckAdmits.Load(); got != 1 {
		t.Fatalf("single ACK reserve admissions = %d, want 1", got)
	}
	if got := device.mobilePacketPressureDropCount.Load(); got != 0 {
		t.Fatalf("single ACK pressure drops = %d, want 0", got)
	}

	gate.setH1AckReserveEnabled(false)
	if device.sendPacket(mobilePressureTcp4Packet(0x10, nil)) {
		t.Fatal("single ACK-only packet bypassed the disabled H1 reserve")
	}
	if got := device.mobilePacketPressureAckDrops.Load(); got != 1 {
		t.Fatalf("single ACK pressure drops = %d, want 1", got)
	}
}
