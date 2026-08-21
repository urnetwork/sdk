package sdk

import (
	"math"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"runtime/pprof"

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
	// Returned buffers are no longer owned by packet work, but can remain
	// reachable on bounded free lists for reuse. Report that retention
	// separately from Taken-Returned.
	PoolRetainedCount                int64
	PoolRetainedByteCount            ByteCount
	PoolCapacityByteCount            ByteCount
	PacketPoolRetainedCount          int64
	PacketPoolRetainedByteCount      ByteCount
	LargeObjectPoolRetainedCount     int64
	LargeObjectPoolRetainedByteCount ByteCount
	// Automatic idle trimming runs at most once per quiet traffic epoch. These
	// counters make a footprint drop attributable without parsing host logs.
	IdleMemoryTrimCount                int64
	LastIdleMemoryTrimDroppedByteCount ByteCount

	// Allocator detail used to distinguish reachable objects from idle spans
	// retained after a burst. HeapIdleByteCount-HeapReleasedByteCount is heap
	// memory the runtime could return to the OS; HeapInuseByteCount-
	// HeapAllocByteCount is an upper bound on size-class fragmentation.
	HeapAllocByteCount         ByteCount
	HeapSystemByteCount        ByteCount
	HeapInuseByteCount         ByteCount
	HeapIdleByteCount          ByteCount
	HeapReleasedByteCount      ByteCount
	HeapObjectCount            int64
	StackInuseByteCount        ByteCount
	MSpanInuseByteCount        ByteCount
	MCacheInuseByteCount       ByteCount
	GCSystemByteCount          ByteCount
	OtherSystemByteCount       ByteCount
	ProfilingBucketByteCount   ByteCount
	SystemByteCount            ByteCount
	MemoryProfileRateByteCount int64

	// Cumulative allocation and GC counters let a host correlate traffic
	// phases with churn without retaining packet contents or destinations.
	TotalAllocatedByteCount ByteCount
	MallocCount             int64
	FreeCount               int64
	GCCycleCount            int64
	ForcedGCCycleCount      int64
	GCPauseTotalNanoseconds int64
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

	poolStats := connect.GetMessagePoolAggregateStats()
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	uint64ToInt64 := func(value uint64) int64 {
		if math.MaxInt64 < value {
			return math.MaxInt64
		}
		return int64(value)
	}

	// a negative value reads the current limit without changing it
	memoryLimitByteCount := debug.SetMemoryLimit(-1)

	totalRuntimeByteCount := max(int64(0), sampleInt64(2)-sampleInt64(3))

	return &MemoryStats{
		HeapLiveByteCount:                sampleInt64(0),
		HeapGoalByteCount:                sampleInt64(1),
		TotalRuntimeByteCount:            totalRuntimeByteCount,
		MemoryLimitByteCount:             memoryLimitByteCount,
		GoroutineCount:                   int(sampleInt64(4)),
		PoolTakenCount:                   int64(poolStats.Taken),
		PoolReturnedCount:                int64(poolStats.Returned),
		PoolCreatedCount:                 int64(poolStats.Created),
		PoolRetainedCount:                int64(poolStats.RetainedCount),
		PoolRetainedByteCount:            int64(poolStats.RetainedByteCount),
		PoolCapacityByteCount:            int64(poolStats.CapacityByteCount),
		PacketPoolRetainedCount:          int64(poolStats.PacketRetainedCount),
		PacketPoolRetainedByteCount:      int64(poolStats.PacketRetainedByteCount),
		LargeObjectPoolRetainedCount:     int64(poolStats.LargeObjectRetainedCount),
		LargeObjectPoolRetainedByteCount: int64(poolStats.LargeObjectRetainedByteCount),
		IdleMemoryTrimCount:              mobileIdleMemoryTrimCount.Load(),
		LastIdleMemoryTrimDroppedByteCount: ByteCount(
			mobileIdleMemoryTrimDropped.Load(),
		),
		HeapAllocByteCount:         uint64ToInt64(mem.HeapAlloc),
		HeapSystemByteCount:        uint64ToInt64(mem.HeapSys),
		HeapInuseByteCount:         uint64ToInt64(mem.HeapInuse),
		HeapIdleByteCount:          uint64ToInt64(mem.HeapIdle),
		HeapReleasedByteCount:      uint64ToInt64(mem.HeapReleased),
		HeapObjectCount:            uint64ToInt64(mem.HeapObjects),
		StackInuseByteCount:        uint64ToInt64(mem.StackInuse),
		MSpanInuseByteCount:        uint64ToInt64(mem.MSpanInuse),
		MCacheInuseByteCount:       uint64ToInt64(mem.MCacheInuse),
		GCSystemByteCount:          uint64ToInt64(mem.GCSys),
		OtherSystemByteCount:       uint64ToInt64(mem.OtherSys),
		ProfilingBucketByteCount:   uint64ToInt64(mem.BuckHashSys),
		SystemByteCount:            uint64ToInt64(mem.Sys),
		MemoryProfileRateByteCount: int64(runtime.MemProfileRate),
		TotalAllocatedByteCount:    uint64ToInt64(mem.TotalAlloc),
		MallocCount:                uint64ToInt64(mem.Mallocs),
		FreeCount:                  uint64ToInt64(mem.Frees),
		GCCycleCount:               int64(mem.NumGC),
		ForcedGCCycleCount:         int64(mem.NumForcedGC),
		GCPauseTotalNanoseconds:    uint64ToInt64(mem.PauseTotalNs),
	}
}

// SetMemoryProfileRate controls Go heap-profile sampling. Set it exactly once,
// as early as possible in a diagnostic process; changing it after allocations
// makes profile weights inconsistent. Production hosts should retain Go's
// default because denser sampling consumes additional profiling metadata.
func SetMemoryProfileRate(byteCount int) {
	runtime.MemProfileRate = max(0, byteCount)
}

// WriteHeapProfile forces a complete collection and writes a private Go heap
// profile. The profile contains aggregate allocation stacks, not packet
// payloads, addresses, or destinations. Hosts should keep it in their private
// diagnostics directory: function names and allocation sizes are still
// implementation details. The resulting profile includes both the live
// (in-use) and cumulative allocation sample types understood by go tool pprof.
func WriteHeapProfile(path string) error {
	runtime.GC()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	writeErr := pprof.WriteHeapProfile(file)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
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
