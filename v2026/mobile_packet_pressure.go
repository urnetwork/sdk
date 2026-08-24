package sdk

import (
	"sync/atomic"

	"github.com/urnetwork/connect/v2026"
)

const (
	// Physical fast.com traces crossed 28 MiB at 1,343--2,488 packet roots,
	// while ordinary browsing stayed below the ceiling at 306. The 512-root
	// gate leaves burst room but stops an active download from multiplying
	// packet ownership across dozens of flows until iOS jetsam kills the
	// extension.
	mobilePacketPressureMaxOutstandingCount uint64 = 512
	// Snapshotting takes the four packet-class shard locks. Sample one in four
	// ingress calls while below the ceiling, then every call while overloaded
	// so traffic resumes promptly as ownership drains.
	mobilePacketPressureSampleEvery uint64 = 4
)

// mobilePacketPressureGate is created only for the <=20-MiB mobile profile.
// Consequently the server/connect and server/proxy packet paths pay neither
// an atomic operation nor a pool snapshot for this mobile overload policy.
type mobilePacketPressureGate struct {
	maxOutstanding uint64
	sampleEvery    uint64
	calls          atomic.Uint64
	cached         atomic.Uint64
	sample         func() uint64
}

func newMobilePacketPressureGate() *mobilePacketPressureGate {
	return &mobilePacketPressureGate{
		maxOutstanding: mobilePacketPressureMaxOutstandingCount,
		sampleEvery:    mobilePacketPressureSampleEvery,
		sample:         connect.MessagePoolPacketOutstandingCount,
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
	if self == nil || self.maxOutstanding == 0 || self.sample == nil {
		return false
	}
	sampleEvery := self.sampleEvery
	if sampleEvery == 0 {
		sampleEvery = 1
	}
	call := self.calls.Add(1)
	outstanding := self.cached.Load()
	if call == 1 || outstanding >= self.maxOutstanding || call%sampleEvery == 0 {
		outstanding = self.sample()
		self.cached.Store(outstanding)
	}
	return outstanding >= self.maxOutstanding
}
