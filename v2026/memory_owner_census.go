//go:build !ios

package sdk

import (
	"encoding/json"
	"os"
	"runtime"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// Diagnostic only, like WriteHeapProfile: not linked into the shipped iOS
// extension. No periodic sampler field or new lifecycle registration is added.
// The on-demand primitive read allocates nothing; file/JSON serialization is
// intentionally off the packet path and perturbs a diagnostic run, not a gate.
type deviceMemoryOwnerCensus struct {
	Api              connect.ApiOwnerCensus         `json:"network_space_api"`
	Client           connect.MultiClientOwnerCensus `json:"client_windows"`
	Provider         connect.TransferOwnerCensus    `json:"provider_transfer"`
	Dns              connect.DnsOwnerCensus         `json:"dns"`
	ProcessPools     connect.PoolOwnerCensus        `json:"process_transfer_pools"`
	ProcessClaims    connect.TransportClaimCensus   `json:"process_transport_claims"`
	BlockActions     int64                          `json:"block_actions"`
	BlockActionSlots int64                          `json:"block_action_slots"`
}

func (self *DeviceLocal) memoryOwnerCensus() deviceMemoryOwnerCensus {
	var out deviceMemoryOwnerCensus
	out.ProcessPools = connect.GetPoolOwnerCensus()
	out.ProcessClaims = connect.DefaultPlatformTransportBudget().MemoryOwnerCensus()
	if self == nil {
		return out
	}
	self.stateLock.Lock()
	remote, provider, mux := self.remoteUserNatClient, self.provider, self.upgradeMux
	strategy := self.clientStrategy
	out.BlockActions = int64(len(self.blockActions))
	out.BlockActionSlots = int64(cap(self.blockActions))
	self.stateLock.Unlock()
	out.Api = strategy.MemoryOwnerCensus()
	if multi, ok := remote.(*connect.RemoteUserNatMultiClient); ok {
		out.Client = multi.MemoryOwnerCensus()
	}
	if provider != nil {
		out.Provider = provider.Client().MemoryOwnerCensus()
	}
	out.Dns = mux.MemoryOwnerCensus()
	return out
}

type memoryOwnerRuntimeCensus struct {
	RuntimeBytes    uint64 `json:"runtime_bytes"`
	HeapObjectBytes uint64 `json:"heap_object_bytes"`
	HeapUnusedBytes uint64 `json:"heap_unused_bytes"`
	HeapFreeBytes   uint64 `json:"heap_free_bytes"`
	StackBytes      uint64 `json:"stack_bytes"`
	GcCycles        uint32 `json:"gc_cycles"`
	ForcedGcCycles  uint32 `json:"forced_gc_cycles"`
	Goroutines      int    `json:"goroutines"`
}

type memoryOwnerSizeClass struct {
	SizeBytes   uint32 `json:"size_bytes"`
	LiveObjects uint64 `json:"live_objects"`
}

type memoryOwnerCensusReport struct {
	Schema            int                      `json:"schema"`
	UnixMillis        int64                    `json:"unix_millis"`
	MemoryProfileRate int                      `json:"memory_profile_rate_bytes"`
	Before            memoryOwnerRuntimeCensus `json:"before"`
	Owners            deviceMemoryOwnerCensus  `json:"owners"`
	SizeClasses       [61]memoryOwnerSizeClass `json:"allocator_size_classes"`
	After             memoryOwnerRuntimeCensus `json:"after"`
}

func memoryOwnerRuntime(stats *runtime.MemStats) memoryOwnerRuntimeCensus {
	return memoryOwnerRuntimeCensus{
		RuntimeBytes: stats.Sys - stats.HeapReleased, HeapObjectBytes: stats.HeapAlloc,
		HeapUnusedBytes: stats.HeapInuse - stats.HeapAlloc,
		HeapFreeBytes:   stats.HeapIdle - stats.HeapReleased, StackBytes: stats.StackInuse,
		GcCycles: stats.NumGC, ForcedGcCycles: stats.NumForcedGC, Goroutines: runtime.NumGoroutine(),
	}
}

func (self *DeviceLocal) memoryOwnerCensusReport() memoryOwnerCensusReport {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	out := memoryOwnerCensusReport{
		Schema: 1, UnixMillis: time.Now().UnixMilli(), MemoryProfileRate: runtime.MemProfileRate,
		Before: memoryOwnerRuntime(&stats), Owners: self.memoryOwnerCensus(),
	}
	for i, size := range stats.BySize {
		out.SizeClasses[i] = memoryOwnerSizeClass{SizeBytes: size.Size, LiveObjects: size.Mallocs - size.Frees}
	}
	runtime.ReadMemStats(&stats)
	out.After = memoryOwnerRuntime(&stats)
	return out
}

// WriteMemoryOwnerCensus writes aggregate owner counts and allocator classes,
// never IDs, names, addresses, payloads, credentials or stack arguments. It does
// not force GC, shed caches, close connections or change profile sampling.
// Before/after bracket the owner read; JSON/file writing follows that read.
// Opening the output file precedes Before. Files are exclusive
// and private; callers must use a new filename for each diagnostic boundary.
// Counts and named backing capacities cannot assign span slack to an owner:
// correlate these with a separately private heap/goroutine profile and an
// owner-specific release test. Reservations are not physical-memory estimates.
func (self *DeviceLocal) WriteMemoryOwnerCensus(path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	report := self.memoryOwnerCensusReport()
	writeErr := json.NewEncoder(file).Encode(report)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}
