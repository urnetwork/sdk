package sdk

import (
	"sync/atomic"

	"github.com/urnetwork/connect/v2026"
)

const (
	// Keep the measured 512-root aggregate gate. A 768-root experiment improved
	// H1 bulk traffic but reached 28.41 MiB while an H3 page was stalled, so the
	// 24-MiB profile spends its headroom on per-flow progress and GC pacing
	// instead of weakening the process-wide safety backstop.
	mobilePacketPressureMaxOutstandingCount uint64 = 512
	// Snapshotting takes the four packet-class shard locks. Sample one in four
	// ingress calls while below the ceiling, then every call while overloaded
	// so traffic resumes promptly as ownership drains.
	mobilePacketPressureSampleEvery uint64 = 4
)

// mobilePacketPressureGate is created only for the <=24-MiB mobile profile.
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
