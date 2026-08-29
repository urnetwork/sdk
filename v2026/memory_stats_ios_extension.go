//go:build ios_extension

package sdk

import (
	"fmt"
	"sync/atomic"

	"github.com/urnetwork/glog/v2026"
)

var (
	extensionMemoryGoPeakByteCount atomic.Int64
)

func updateExtensionMemoryPeak(peak *atomic.Int64, value int64) int64 {
	value = max(0, value)
	for {
		current := peak.Load()
		if value <= current || peak.CompareAndSwap(current, value) {
			return max(current, value)
		}
	}
}

// RecordExtensionMemorySample returns a parseable, paired snapshot of Go's
// runtime accounting and the kernel's phys_footprint. It also writes the same
// line to the extension's existing diagnostic log, so the evidence survives
// process separation and is included in uploaded crash diagnostics.
//
// This API exists only in the reduced ios_extension binding.
func RecordExtensionMemorySample(event string, physicalFootprintByteCount int64) string {
	physicalPeakByteCount := recordMobilePhysicalFootprint(physicalFootprintByteCount)
	stats := GetMemoryStats()
	goPeakByteCount := updateExtensionMemoryPeak(
		&extensionMemoryGoPeakByteCount,
		stats.TotalRuntimeByteCount,
	)
	poolHeldCount := max(int64(0), stats.PoolTakenCount-stats.PoolReturnedCount)

	line := fmt.Sprintf(
		"[memory] event=%s go_total_bytes=%d go_heap_live_bytes=%d go_heap_goal_bytes=%d go_limit_bytes=%d phys_footprint_bytes=%d go_peak_bytes=%d phys_peak_bytes=%d goroutines=%d pool_held=%d",
		event,
		stats.TotalRuntimeByteCount,
		stats.HeapLiveByteCount,
		stats.HeapGoalByteCount,
		stats.MemoryLimitByteCount,
		max(int64(0), physicalFootprintByteCount),
		goPeakByteCount,
		physicalPeakByteCount,
		stats.GoroutineCount,
		poolHeldCount,
	)
	glog.Info(line)
	return line
}

// RecordExtensionPhysicalFootprint is the high-frequency, allocation-free
// counterpart to RecordExtensionMemorySample. The Network Extension may call
// this at 20--50 Hz; the bounded Go sampler pairs the latest/peak value with
// its next primitive snapshot. Use RecordExtensionMemorySample only for sparse
// named events because formatting and logging every kernel sample would itself
// create memory and I/O pressure.
func RecordExtensionPhysicalFootprint(physicalFootprintByteCount int64) {
	recordMobilePhysicalFootprint(physicalFootprintByteCount)
}

// SetExtensionMemoryPressureByteCount sets the phys_footprint high-water that
// starts a bounded quiet reclaim. The release default is 40 MiB, leaving a
// provisional 10-MiB margin below the historically documented extension
// boundary. Set zero to disable proactive physical-footprint triggering when a
// host has its own measured pressure controller.
func SetExtensionMemoryPressureByteCount(byteCount int64) {
	mobilePhysicalPressureByteCount.Store(max(int64(0), byteCount))
	mobilePhysicalPressureArmed.Store(false)
}
