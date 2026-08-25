package sdk

import (
	"testing"

	"github.com/urnetwork/connect"
)

func TestMemoryStatsAllocatorAccounting(t *testing.T) {
	stats := GetMemoryStats()
	if stats.HeapAllocByteCount <= 0 {
		t.Fatalf("HeapAllocByteCount = %d, want positive", stats.HeapAllocByteCount)
	}
	if stats.HeapInuseByteCount < stats.HeapAllocByteCount {
		t.Fatalf(
			"HeapInuseByteCount = %d, below HeapAllocByteCount = %d",
			stats.HeapInuseByteCount,
			stats.HeapAllocByteCount,
		)
	}
	if stats.MallocCount < stats.FreeCount {
		t.Fatalf("MallocCount = %d, below FreeCount = %d", stats.MallocCount, stats.FreeCount)
	}
	if stats.HeapObjectCount != stats.MallocCount-stats.FreeCount {
		t.Fatalf(
			"HeapObjectCount = %d, want mallocs-frees = %d",
			stats.HeapObjectCount,
			stats.MallocCount-stats.FreeCount,
		)
	}
}

func TestMobileMemoryRuntimeSnapshotUsesCallerStorageWithoutAllocation(t *testing.T) {
	var reader mobileMemoryRuntimeReader
	var snapshot mobileMemoryRuntimeSnapshot
	reader.read(&snapshot)
	if allocations := testing.AllocsPerRun(25, func() {
		reader.read(&snapshot)
	}); allocations != 0 {
		t.Fatalf("mobile runtime snapshot allocations/run = %.2f, want 0", allocations)
	}
}

func TestRuntimeTotalByteCountDoesNotAllocate(t *testing.T) {
	runtimeTotalByteCount()
	if allocations := testing.AllocsPerRun(25, func() {
		_ = runtimeTotalByteCount()
	}); allocations != 0 {
		t.Fatalf("runtimeTotalByteCount allocations/run = %.2f, want 0", allocations)
	}
}

func TestPhysicalFootprintRecorderTracksLatestAndPeak(t *testing.T) {
	previousCurrent := mobilePhysicalFootprintCurrent.Load()
	previousPeak := mobilePhysicalFootprintPeak.Load()
	previousPressureArmed := mobilePhysicalPressureArmed.Load()
	previousPressureCount := mobilePhysicalPressureCount.Load()
	t.Cleanup(func() {
		mobilePhysicalFootprintCurrent.Store(previousCurrent)
		mobilePhysicalFootprintPeak.Store(previousPeak)
		mobilePhysicalPressureArmed.Store(previousPressureArmed)
		mobilePhysicalPressureCount.Store(previousPressureCount)
	})
	mobilePhysicalFootprintCurrent.Store(0)
	mobilePhysicalFootprintPeak.Store(0)

	if peak := recordMobilePhysicalFootprint(100); peak != 100 {
		t.Fatalf("first peak = %d, want 100", peak)
	}
	if peak := recordMobilePhysicalFootprint(60); peak != 100 {
		t.Fatalf("lower-sample peak = %d, want 100", peak)
	}
	if current := mobilePhysicalFootprintCurrent.Load(); current != 60 {
		t.Fatalf("latest footprint = %d, want 60", current)
	}
	if peak := recordMobilePhysicalFootprint(-1); peak != 100 {
		t.Fatalf("negative-sample peak = %d, want 100", peak)
	}
	if current := mobilePhysicalFootprintCurrent.Load(); current != 0 {
		t.Fatalf("negative sample was not clamped: %d", current)
	}
	if allocations := testing.AllocsPerRun(1000, func() {
		recordMobilePhysicalFootprint(60)
	}); allocations != 0 {
		t.Fatalf("physical footprint recorder allocations/run = %.2f, want 0", allocations)
	}
}

func TestTrimMemoryDecaysPoolsWithoutShrinkingCapacity(t *testing.T) {
	const mib = int64(1024 * 1024)
	connect.ResizeMessagePools(4*mib, 2*mib)
	connect.ClearMessagePools()
	t.Cleanup(func() {
		connect.ClearMessagePools()
		connect.ResizeMessagePools(
			connect.InitialMessagePoolByteCount/2,
			connect.InitialMessagePoolByteCount/2,
		)
	})

	// Resize splits the 4-MiB packet budget into 1 MiB of 256-byte small
	// roots and 3 MiB of 2-KiB full roots. Fill both classes so this test still
	// exercises decay from the entire configured packet budget.
	const (
		smallPacketSize = 80
		fullPacketSize  = 2048
	)
	messages := make([][]byte, mib/256+3*mib/fullPacketSize)
	for i := int64(0); i < mib/256; i += 1 {
		messages[i] = MessagePoolGetRaw(smallPacketSize)
	}
	for i := mib / 256; i < int64(len(messages)); i += 1 {
		messages[i] = MessagePoolGetRaw(fullPacketSize)
	}
	for _, message := range messages {
		MessagePoolReturn(message)
	}
	before := GetMemoryStats()
	if before.PacketPoolRetainedByteCount != 4*mib {
		t.Fatalf(
			"packet pool retained %d bytes before trim, want %d",
			before.PacketPoolRetainedByteCount,
			4*mib,
		)
	}

	TrimMemory()
	after := GetMemoryStats()
	if after.PacketPoolRetainedByteCount != mib {
		t.Fatalf(
			"packet pool retained %d bytes after trim, want %d",
			after.PacketPoolRetainedByteCount,
			mib,
		)
	}
	if after.PoolCapacityByteCount != before.PoolCapacityByteCount {
		t.Fatalf(
			"pool capacity changed from %d to %d",
			before.PoolCapacityByteCount,
			after.PoolCapacityByteCount,
		)
	}
}

func TestAutomaticTrimSkipsForcedCollectionForTrivialRefill(t *testing.T) {
	const mib = int64(1024 * 1024)
	connect.ResizeMessagePools(4*mib, 2*mib)
	connect.ClearMessagePools()
	connect.WarmMessagePools()
	t.Cleanup(func() {
		connect.ClearMessagePools()
		connect.ResizeMessagePools(
			connect.InitialMessagePoolByteCount/2,
			connect.InitialMessagePoolByteCount/2,
		)
	})

	messages := make([][]byte, mib/2048+1)
	for i := range messages {
		messages[i] = MessagePoolGetRaw(2048)
	}
	for _, message := range messages {
		MessagePoolReturn(message)
	}
	before := GetMemoryStats()
	if got := trimMemory(false); got != 0 {
		t.Fatalf("automatic trivial trim reported %d bytes, want 0", got)
	}
	after := GetMemoryStats()
	if after.ForcedGCCycleCount != before.ForcedGCCycleCount {
		t.Fatalf(
			"forced GC count changed from %d to %d for a trivial refill",
			before.ForcedGCCycleCount,
			after.ForcedGCCycleCount,
		)
	}
	if after.PacketPoolRetainedByteCount != mib {
		t.Fatalf(
			"packet pool retained %d bytes after cheap prune, want %d",
			after.PacketPoolRetainedByteCount,
			mib,
		)
	}
}
