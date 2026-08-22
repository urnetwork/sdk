package sdk

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

func TestWriteHeapProfileCreatesPrivateProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heap.pprof")
	if err := WriteHeapProfile(path); err != nil {
		t.Fatalf("WriteHeapProfile: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat profile: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("heap profile is empty")
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("profile mode = %04o, want 0600", got)
	}
}

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

	const packetSize = 2048
	messages := make([][]byte, 4*mib/packetSize)
	for i := range messages {
		messages[i] = MessagePoolGetRaw(packetSize)
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
