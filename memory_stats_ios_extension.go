//go:build ios_extension

package sdk

import (
	"fmt"
	"sync/atomic"

	"github.com/urnetwork/glog"
)

var (
	extensionMemoryGoPeakByteCount       atomic.Int64
	extensionMemoryPhysicalPeakByteCount atomic.Int64
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
	stats := GetMemoryStats()
	goPeakByteCount := updateExtensionMemoryPeak(
		&extensionMemoryGoPeakByteCount,
		stats.TotalRuntimeByteCount,
	)
	physicalPeakByteCount := updateExtensionMemoryPeak(
		&extensionMemoryPhysicalPeakByteCount,
		physicalFootprintByteCount,
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
