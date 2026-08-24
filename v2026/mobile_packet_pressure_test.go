package sdk

import (
	"testing"

	"github.com/urnetwork/connect/v2026"
)

func TestMobilePacketPressureGateSamplesSparselyAndRecoversPromptly(t *testing.T) {
	samples := []uint64{100, mobilePacketPressureMaxOutstandingCount, 100}
	sampleIndex := 0
	gate := &mobilePacketPressureGate{
		maxOutstanding: mobilePacketPressureMaxOutstandingCount,
		sampleEvery:    mobilePacketPressureSampleEvery,
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

func TestMobilePacketPressureGateIsMobileTwentyMiBOnly(t *testing.T) {
	if gate := newMobilePacketPressureGateForPlatform(
		mobileSteadyMemoryTargetByteCount,
		true,
	); gate == nil {
		t.Fatal("20-MiB mobile target did not create a pressure gate")
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
				t.Fatal("created a mobile pressure gate outside the 20-MiB mobile profile")
			}
		})
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
		maxOutstanding: mobilePacketPressureMaxOutstandingCount,
		sampleEvery:    1,
		sample:         func() uint64 { return 0 },
	}
	if allocations := testing.AllocsPerRun(100, func() {
		_ = gate.shouldDrop()
	}); allocations != 0 {
		t.Fatalf("pressure gate allocated %.0f objects, want 0", allocations)
	}
}

func TestDeviceLocalPacketPressureBatchRejectsAndReturnsOwnership(t *testing.T) {
	gate := &mobilePacketPressureGate{
		maxOutstanding: mobilePacketPressureMaxOutstandingCount,
		sampleEvery:    1,
		sample: func() uint64 {
			return mobilePacketPressureMaxOutstandingCount
		},
	}
	device := &DeviceLocal{mobilePacketPressure: gate}
	baseline := connect.MessagePoolPacketOutstandingCount()
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
	if got := device.mobilePacketPressureDropCount.Load(); got != int64(len(packets)) {
		t.Fatalf("pressure drop count = %d, want %d", got, len(packets))
	}
}

func TestDeviceLocalPacketPressureSingleRejectLeavesOwnershipWithCaller(t *testing.T) {
	device := &DeviceLocal{mobilePacketPressure: &mobilePacketPressureGate{
		maxOutstanding: 1,
		sampleEvery:    1,
		sample:         func() uint64 { return 1 },
	}}
	baseline := connect.MessagePoolPacketOutstandingCount()
	packet := connect.MessagePoolGet(connect.DefaultMtu)
	if device.sendPacket(packet) {
		t.Fatal("single packet passed a closed pressure gate")
	}
	if got := connect.MessagePoolPacketOutstandingCount(); got != baseline+1 {
		t.Fatalf("single-packet failure stole caller ownership: roots=%d want=%d", got, baseline+1)
	}
	connect.MessagePoolReturn(packet)
	if got := connect.MessagePoolPacketOutstandingCount(); got != baseline {
		t.Fatalf("single packet root after caller return = %d, want %d", got, baseline)
	}
	if got := device.mobilePacketPressureDropCount.Load(); got != 1 {
		t.Fatalf("single packet pressure drop count = %d, want 1", got)
	}
}
