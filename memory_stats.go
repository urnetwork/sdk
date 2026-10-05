package sdk

import (
	"math"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"sync"
	"sync/atomic"

	"github.com/urnetwork/connect"
)

// memory gauges for host telemetry, so footprint regressions show up in app
// metrics rather than as os memory kills. All values are sampled at call time.

type MemoryStats struct {
	// the gc live set
	HeapLiveByteCount int64
	// the gc heap goal; the steady state heap cycles up to this
	HeapGoalByteCount int64
	// memory counted against the Go soft limit: all runtime-managed mapped
	// memory less heap pages released to the OS. This is
	// runtime.MemStats.Sys - runtime.MemStats.HeapReleased, not process RSS;
	// binary mappings, C allocations, and kernel memory are excluded.
	TotalRuntimeByteCount int64
	// the soft memory limit (see `SetMemoryLimit`). MaxInt64 when unset
	MemoryLimitByteCount int64
	GoroutineCount       int
	// Latest/peak iOS TASK_VM_INFO.phys_footprint supplied by the extension
	// host. Zero on other platforms or before the host records a sample.
	PhysicalFootprintByteCount     int64
	PhysicalFootprintPeakByteCount int64
	PhysicalFootprintPressureCount int64
	// cumulative message pool counters. taken minus returned is the number
	// of pool buffers currently held by consumers
	PoolTakenCount    int64
	PoolReturnedCount int64
	PoolCreatedCount  int64
	// Returned buffers are no longer owned by packet work, but can remain
	// reachable on bounded free lists for reuse. Report that retention
	// separately from Taken-Returned.
	PoolRetainedCount                int64
	PoolRetainedByteCount            int64
	PoolCapacityByteCount            int64
	PacketPoolRetainedCount          int64
	PacketPoolRetainedByteCount      int64
	LargeObjectPoolRetainedCount     int64
	LargeObjectPoolRetainedByteCount int64
	// DeviceTunEgressOutstandingByteCount is the subset of packet-root
	// ownership admitted from the device TUN. Mobile pressure gating uses this
	// directional value so downloaded roots cannot crowd out TCP ACKs.
	DeviceTunEgressOutstandingByteCount int64
	// Automatic idle trimming runs at most once per quiet traffic epoch. These
	// counters make a footprint drop attributable without parsing host logs.
	IdleMemoryTrimCount                int64
	LastIdleMemoryTrimDroppedByteCount int64
	IdleMemoryTrimDeferredCount        int64
	IdleMemoryTrimBelowTargetCount     int64
	IdleMemoryTrimCooldownCount        int64
	LastIdleMemoryTrimBeforeByteCount  int64
	LastIdleMemoryTrimAfterByteCount   int64
	// Host platform trim levels (ReportMemoryTrimLevel): the latest level, how
	// many reports arrived, how many acted, and the bytes the latest trim pass
	// dropped.
	TrimLevelLast                 int64
	TrimLevelCount                int64
	TrimLevelActionCount          int64
	LastTrimLevelDroppedByteCount int64

	// Carrier reservations for one DeviceLocal owner, populated by
	// DeviceLocal.GetMemoryStats. Package-level GetMemoryStats leaves these
	// fields zero because there is no shared process admission budget.
	PlatformTransportBudgetTotalByteCount     int64
	PlatformTransportBudgetUsedByteCount      int64
	PlatformTransportBudgetUsedCount          int
	PlatformTransportBudgetPendingH1Count     int
	PlatformTransportBudgetPendingH1ByteCount int64

	// Allocator detail used to distinguish reachable objects from idle spans
	// retained after a burst. HeapIdleByteCount-HeapReleasedByteCount is heap
	// memory the runtime could return to the OS; HeapInuseByteCount-
	// HeapAllocByteCount is an upper bound on size-class fragmentation.
	HeapAllocByteCount         int64
	HeapSystemByteCount        int64
	HeapInuseByteCount         int64
	HeapIdleByteCount          int64
	HeapReleasedByteCount      int64
	HeapObjectCount            int64
	StackInuseByteCount        int64
	MSpanInuseByteCount        int64
	MCacheInuseByteCount       int64
	GCSystemByteCount          int64
	OtherSystemByteCount       int64
	ProfilingBucketByteCount   int64
	SystemByteCount            int64
	MemoryProfileRateByteCount int64

	// Cumulative allocation and GC counters let a host correlate traffic
	// phases with churn without retaining packet contents or destinations.
	TotalAllocatedByteCount int64
	MallocCount             int64
	FreeCount               int64
	GCCycleCount            int64
	ForcedGCCycleCount      int64
	GCPauseTotalNanoseconds int64
}

var (
	mobilePhysicalFootprintCurrent atomic.Int64
	mobilePhysicalFootprintPeak    atomic.Int64
	runtimeTotalMetricLock         sync.Mutex
	runtimeTotalMetricSamples      = [...]metrics.Sample{
		{Name: "/memory/classes/total:bytes"},
		{Name: "/memory/classes/heap/released:bytes"},
	}
)

func recordMobilePhysicalFootprint(byteCount int64) int64 {
	byteCount = max(int64(0), byteCount)
	mobilePhysicalFootprintCurrent.Store(byteCount)
	var peak int64
	for {
		current := mobilePhysicalFootprintPeak.Load()
		if byteCount <= current {
			peak = current
			break
		}
		if mobilePhysicalFootprintPeak.CompareAndSwap(current, byteCount) {
			peak = byteCount
			break
		}
	}
	noteMobilePhysicalFootprint(byteCount)
	return peak
}

func GetMemoryStats() *MemoryStats {
	stats := &MemoryStats{}
	readMemoryStats(stats)
	return stats
}

// GetMemoryStats includes this device's private carrier admission counters
// alongside the process runtime and allocator counters.
func (self *DeviceLocal) GetMemoryStats() *MemoryStats {
	stats := GetMemoryStats()
	if self != nil && self.platformTransportBudget != nil {
		budgetStats := self.platformTransportBudget.Stats()
		stats.PlatformTransportBudgetTotalByteCount = budgetStats.TotalByteCount
		stats.PlatformTransportBudgetUsedByteCount = budgetStats.UsedByteCount
		stats.PlatformTransportBudgetUsedCount = budgetStats.UsedTransportCount
		stats.PlatformTransportBudgetPendingH1Count = budgetStats.PendingH1Count
		stats.PlatformTransportBudgetPendingH1ByteCount = budgetStats.PendingH1ByteCount
	}
	return stats
}

// readMemoryStats fills caller-owned storage. The production sampler keeps the
// destination and runtime/metrics descriptor array off the heap; the exported
// getter retains its convenient one-object API for occasional host reads.
func readMemoryStats(stats *MemoryStats) {
	samples := [...]metrics.Sample{
		{Name: "/gc/heap/live:bytes"},
		{Name: "/gc/heap/goal:bytes"},
		{Name: "/memory/classes/total:bytes"},
		{Name: "/memory/classes/heap/released:bytes"},
		{Name: "/sched/goroutines:goroutines"},
	}
	metrics.Read(samples[:])
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

	*stats = MemoryStats{
		HeapLiveByteCount:                sampleInt64(0),
		HeapGoalByteCount:                sampleInt64(1),
		TotalRuntimeByteCount:            totalRuntimeByteCount,
		MemoryLimitByteCount:             memoryLimitByteCount,
		GoroutineCount:                   int(sampleInt64(4)),
		PhysicalFootprintByteCount:       mobilePhysicalFootprintCurrent.Load(),
		PhysicalFootprintPeakByteCount:   mobilePhysicalFootprintPeak.Load(),
		PhysicalFootprintPressureCount:   mobilePhysicalPressureCount.Load(),
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
		DeviceTunEgressOutstandingByteCount: int64(
			poolStats.DeviceTunEgressOutstandingByteCount,
		),
		IdleMemoryTrimCount: mobileIdleMemoryTrimCount.Load(),
		LastIdleMemoryTrimDroppedByteCount: ByteCount(
			mobileIdleMemoryTrimDropped.Load(),
		),
		IdleMemoryTrimDeferredCount:       mobileIdleMemoryTrimDeferred.Load(),
		IdleMemoryTrimBelowTargetCount:    mobileIdleMemoryTrimBelow.Load(),
		IdleMemoryTrimCooldownCount:       mobileIdleMemoryTrimCooldowns.Load(),
		LastIdleMemoryTrimBeforeByteCount: mobileIdleMemoryTrimBefore.Load(),
		LastIdleMemoryTrimAfterByteCount:  mobileIdleMemoryTrimAfter.Load(),
		TrimLevelLast:                     mobileTrimLevelLast.Load(),
		TrimLevelCount:                    mobileTrimLevelCount.Load(),
		TrimLevelActionCount:              mobileTrimLevelActionCount.Load(),
		LastTrimLevelDroppedByteCount:     ByteCount(mobileTrimLevelDropped.Load()),
		HeapAllocByteCount:                uint64ToInt64(mem.HeapAlloc),
		HeapSystemByteCount:               uint64ToInt64(mem.HeapSys),
		HeapInuseByteCount:                uint64ToInt64(mem.HeapInuse),
		HeapIdleByteCount:                 uint64ToInt64(mem.HeapIdle),
		HeapReleasedByteCount:             uint64ToInt64(mem.HeapReleased),
		HeapObjectCount:                   uint64ToInt64(mem.HeapObjects),
		StackInuseByteCount:               uint64ToInt64(mem.StackInuse),
		MSpanInuseByteCount:               uint64ToInt64(mem.MSpanInuse),
		MCacheInuseByteCount:              uint64ToInt64(mem.MCacheInuse),
		GCSystemByteCount:                 uint64ToInt64(mem.GCSys),
		OtherSystemByteCount:              uint64ToInt64(mem.OtherSys),
		ProfilingBucketByteCount:          uint64ToInt64(mem.BuckHashSys),
		SystemByteCount:                   uint64ToInt64(mem.Sys),
		MemoryProfileRateByteCount:        int64(runtime.MemProfileRate),
		TotalAllocatedByteCount:           uint64ToInt64(mem.TotalAlloc),
		MallocCount:                       uint64ToInt64(mem.Mallocs),
		FreeCount:                         uint64ToInt64(mem.Frees),
		GCCycleCount:                      int64(mem.NumGC),
		ForcedGCCycleCount:                int64(mem.NumForcedGC),
		GCPauseTotalNanoseconds:           uint64ToInt64(mem.PauseTotalNs),
	}
}

// SetMemoryProfileRate controls Go heap-profile sampling for diagnostics. The
// release gomobile build starts with sampling disabled before runtime startup;
// a private diagnostic build can select a positive startup rate, or call this
// exactly once before the workload begins. Changing it after allocations makes
// profile weights inconsistent and cannot recover startup profiling buckets.
func SetMemoryProfileRate(byteCount int) {
	runtime.MemProfileRate = max(0, byteCount)
}

// WriteHeapProfile lives in memory_stats_pprof.go, behind `!ios`: linking
// runtime/pprof costs compiled size in the iOS extension slice, which has
// the tightest budget in build/check_apple_size.sh.

// runtimeTotalByteCount samples the memory counted against the Go soft
// memory limit (runtime total mapped minus released heap pages).
func runtimeTotalByteCount() int64 {
	runtimeTotalMetricLock.Lock()
	defer runtimeTotalMetricLock.Unlock()
	metrics.Read(runtimeTotalMetricSamples[:])
	if runtimeTotalMetricSamples[0].Value.Kind() != metrics.KindUint64 ||
		runtimeTotalMetricSamples[1].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	total := runtimeTotalMetricSamples[0].Value.Uint64()
	released := runtimeTotalMetricSamples[1].Value.Uint64()
	if total < released {
		return 0
	}
	v := total - released
	if math.MaxInt64 < v {
		return math.MaxInt64
	}
	return int64(v)
}
