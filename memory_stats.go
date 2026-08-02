package sdk

import (
	"math"
	"runtime/debug"
	"runtime/metrics"

	"github.com/urnetwork/connect"
)

// memory gauges for host telemetry, so footprint regressions show up in app
// metrics rather than as os memory kills. All values are sampled at call time.

type MemoryStats struct {
	// the gc live set
	HeapLiveByteCount ByteCount
	// the gc heap goal; the steady state heap cycles up to this
	HeapGoalByteCount ByteCount
	// memory counted against the Go soft limit: all runtime-managed mapped
	// memory less heap pages released to the OS. This is
	// runtime.MemStats.Sys - runtime.MemStats.HeapReleased, not process RSS;
	// binary mappings, C allocations, and kernel memory are excluded.
	TotalRuntimeByteCount ByteCount
	// the soft memory limit (see `SetMemoryLimit`). MaxInt64 when unset
	MemoryLimitByteCount ByteCount
	GoroutineCount       int
	// cumulative message pool counters. taken minus returned is the number
	// of pool buffers currently held by consumers
	PoolTakenCount    int64
	PoolReturnedCount int64
	PoolCreatedCount  int64
}

func GetMemoryStats() *MemoryStats {
	samples := []metrics.Sample{
		{Name: "/gc/heap/live:bytes"},
		{Name: "/gc/heap/goal:bytes"},
		{Name: "/memory/classes/total:bytes"},
		{Name: "/memory/classes/heap/released:bytes"},
		{Name: "/sched/goroutines:goroutines"},
	}
	metrics.Read(samples)
	sampleInt64 := func(i int) int64 {
		if samples[i].Value.Kind() != metrics.KindUint64 {
			return 0
		}
		v := samples[i].Value.Uint64()
		if math.MaxInt64 < v {
			return math.MaxInt64
		}
		return int64(v)
	}

	taken, returned, created := connect.MessagePoolCounts()

	// a negative value reads the current limit without changing it
	memoryLimitByteCount := debug.SetMemoryLimit(-1)

	totalRuntimeByteCount := max(int64(0), sampleInt64(2)-sampleInt64(3))

	return &MemoryStats{
		HeapLiveByteCount:     sampleInt64(0),
		HeapGoalByteCount:     sampleInt64(1),
		TotalRuntimeByteCount: totalRuntimeByteCount,
		MemoryLimitByteCount:  memoryLimitByteCount,
		GoroutineCount:        int(sampleInt64(4)),
		PoolTakenCount:        int64(taken),
		PoolReturnedCount:     int64(returned),
		PoolCreatedCount:      int64(created),
	}
}

// runtimeTotalByteCount samples the memory counted against the Go soft
// memory limit (runtime total mapped minus released heap pages).
func runtimeTotalByteCount() int64 {
	samples := []metrics.Sample{
		{Name: "/memory/classes/total:bytes"},
		{Name: "/memory/classes/heap/released:bytes"},
	}
	metrics.Read(samples)
	if samples[0].Value.Kind() != metrics.KindUint64 ||
		samples[1].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	total := samples[0].Value.Uint64()
	released := samples[1].Value.Uint64()
	if total < released {
		return 0
	}
	v := total - released
	if math.MaxInt64 < v {
		return math.MaxInt64
	}
	return int64(v)
}
