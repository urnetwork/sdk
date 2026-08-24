package sdk

import (
	"context"
	"encoding/json"
	"math"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"sync"
	"time"

	"github.com/urnetwork/connect/v2026"
)

const (
	mobileMemorySampleInterval = 15 * time.Second
	mobileMemorySampleCapacity = 64
	mobileMemorySampleSchema   = 1
)

// mobileMemorySample contains primitives only. Recording it never constructs
// gomobile lists, exit objects, status objects, strings, or packet identities.
type mobileMemorySample struct {
	UnixMillis int64 `json:"unix_millis"`

	GoTotalByteCount      int64 `json:"go_total_bytes"`
	GoLiveByteCount       int64 `json:"go_live_bytes"`
	GoGoalByteCount       int64 `json:"go_goal_bytes"`
	GoLimitByteCount      int64 `json:"go_limit_bytes"`
	PhysicalByteCount     int64 `json:"physical_bytes"`
	PhysicalPeakByteCount int64 `json:"physical_peak_bytes"`
	PhysicalPressureCount int64 `json:"physical_pressure_signals"`
	GoroutineCount        int64 `json:"goroutines"`

	PoolOutstandingCount    int64 `json:"pool_outstanding"`
	PoolRetainedByteCount   int64 `json:"pool_retained_bytes"`
	PoolCapacityByteCount   int64 `json:"pool_capacity_bytes"`
	PacketPressureDropCount int64 `json:"packet_pressure_drops"`
	DeviceTrackedByteCount  int64 `json:"device_tracked_bytes"`

	QualityClientCount int64 `json:"quality_clients"`
	SpeedClientCount   int64 `json:"speed_clients"`
	FlowCount          int64 `json:"flows"`

	TransportBudgetUsedByteCount   int64 `json:"transport_budget_used_bytes"`
	TransportBudgetUsedCount       int64 `json:"transport_budget_used_count"`
	TransportBudgetPendingH1Count  int64 `json:"transport_budget_pending_h1"`
	IdleReclaimCount               int64 `json:"idle_reclaims"`
	ForcedGCCount                  int64 `json:"forced_gc"`
	GCCycleCount                   int64 `json:"gc_cycles"`
	TotalAllocatedByteCount        int64 `json:"total_allocated_bytes"`
	ProfilingBucketByteCount       int64 `json:"profiling_bucket_bytes"`
	MemoryProfileRateByteCount     int64 `json:"memory_profile_rate_bytes"`
	IdleReclaimDeferredCount       int64 `json:"idle_reclaim_deferred"`
	IdleReclaimBelowTargetCount    int64 `json:"idle_reclaim_below_target"`
	IdleReclaimCooldownCount       int64 `json:"idle_reclaim_cooldown"`
	LastIdleReclaimBeforeByteCount int64 `json:"last_idle_reclaim_before_bytes"`
	LastIdleReclaimAfterByteCount  int64 `json:"last_idle_reclaim_after_bytes"`
}

type mobileMemorySampleBatch struct {
	Schema  int                  `json:"schema"`
	Dropped int64                `json:"dropped"`
	Samples []mobileMemorySample `json:"samples"`
}

type mobileMemoryRuntimeSnapshot struct {
	totalByteCount                 int64
	liveByteCount                  int64
	goalByteCount                  int64
	limitByteCount                 int64
	physicalByteCount              int64
	physicalPeakByteCount          int64
	physicalPressureCount          int64
	goroutineCount                 int64
	poolOutstandingCount           int64
	poolRetainedByteCount          int64
	poolCapacityByteCount          int64
	transportBudgetUsedByteCount   int64
	transportBudgetUsedCount       int64
	transportBudgetPendingH1Count  int64
	idleReclaimCount               int64
	forcedGCCount                  int64
	gcCycleCount                   int64
	totalAllocatedByteCount        int64
	profilingBucketByteCount       int64
	memoryProfileRateByteCount     int64
	idleReclaimDeferredCount       int64
	idleReclaimBelowTargetCount    int64
	idleReclaimCooldownCount       int64
	lastIdleReclaimBeforeByteCount int64
	lastIdleReclaimAfterByteCount  int64
}

type mobileMemoryRuntimeReader struct {
	samples [9]metrics.Sample
}

func mobileMetricInt64(sample *metrics.Sample) int64 {
	if sample.Value.Kind() != metrics.KindUint64 {
		return 0
	}
	value := sample.Value.Uint64()
	if math.MaxInt64 < value {
		return math.MaxInt64
	}
	return int64(value)
}

// readMobileMemoryRuntimeSnapshot is the sampler's non-observing hot path. It
// uses runtime/metrics and primitive Connect snapshots only; unlike the public
// detailed MemoryStats getter it does not call runtime.ReadMemStats or create a
// gomobile-visible object.
func (self *mobileMemoryRuntimeReader) read(snapshot *mobileMemoryRuntimeSnapshot) {
	if self.samples[0].Name == "" {
		self.samples = [9]metrics.Sample{
			{Name: "/gc/heap/live:bytes"},
			{Name: "/gc/heap/goal:bytes"},
			{Name: "/memory/classes/total:bytes"},
			{Name: "/memory/classes/heap/released:bytes"},
			{Name: "/sched/goroutines:goroutines"},
			{Name: "/gc/cycles/forced:gc-cycles"},
			{Name: "/gc/cycles/total:gc-cycles"},
			{Name: "/gc/heap/allocs:bytes"},
			{Name: "/memory/classes/profiling/buckets:bytes"},
		}
	}
	metrics.Read(self.samples[:])
	poolStats := connect.GetMessagePoolAggregateStats()
	transportBudgetStats := connect.DefaultPlatformTransportBudget().Stats()
	*snapshot = mobileMemoryRuntimeSnapshot{
		totalByteCount: max(
			int64(0),
			mobileMetricInt64(&self.samples[2])-mobileMetricInt64(&self.samples[3]),
		),
		liveByteCount:                  mobileMetricInt64(&self.samples[0]),
		goalByteCount:                  mobileMetricInt64(&self.samples[1]),
		limitByteCount:                 debug.SetMemoryLimit(-1),
		physicalByteCount:              mobilePhysicalFootprintCurrent.Load(),
		physicalPeakByteCount:          mobilePhysicalFootprintPeak.Load(),
		physicalPressureCount:          mobilePhysicalPressureCount.Load(),
		goroutineCount:                 mobileMetricInt64(&self.samples[4]),
		poolOutstandingCount:           max(int64(0), int64(poolStats.Taken)-int64(poolStats.Returned)),
		poolRetainedByteCount:          int64(poolStats.RetainedByteCount),
		poolCapacityByteCount:          int64(poolStats.CapacityByteCount),
		transportBudgetUsedByteCount:   int64(transportBudgetStats.UsedByteCount),
		transportBudgetUsedCount:       int64(transportBudgetStats.UsedTransportCount),
		transportBudgetPendingH1Count:  int64(transportBudgetStats.PendingH1Count),
		idleReclaimCount:               mobileIdleMemoryTrimCount.Load(),
		forcedGCCount:                  mobileMetricInt64(&self.samples[5]),
		gcCycleCount:                   mobileMetricInt64(&self.samples[6]),
		totalAllocatedByteCount:        mobileMetricInt64(&self.samples[7]),
		profilingBucketByteCount:       mobileMetricInt64(&self.samples[8]),
		memoryProfileRateByteCount:     int64(runtime.MemProfileRate),
		idleReclaimDeferredCount:       mobileIdleMemoryTrimDeferred.Load(),
		idleReclaimBelowTargetCount:    mobileIdleMemoryTrimBelow.Load(),
		idleReclaimCooldownCount:       mobileIdleMemoryTrimCooldowns.Load(),
		lastIdleReclaimBeforeByteCount: mobileIdleMemoryTrimBefore.Load(),
		lastIdleReclaimAfterByteCount:  mobileIdleMemoryTrimAfter.Load(),
	}
}

type mobileMemorySampler struct {
	stateLock     sync.Mutex
	runtimeReader mobileMemoryRuntimeReader
	samples       [mobileMemorySampleCapacity]mobileMemorySample
	head          int
	count         int
	dropped       int64
}

func (self *mobileMemorySampler) record(sample mobileMemorySample) {
	self.stateLock.Lock()
	if self.count < len(self.samples) {
		tail := (self.head + self.count) % len(self.samples)
		self.samples[tail] = sample
		self.count += 1
	} else {
		self.samples[self.head] = sample
		self.head = (self.head + 1) % len(self.samples)
		self.dropped += 1
	}
	self.stateLock.Unlock()
}

func (self *mobileMemorySampler) take() mobileMemorySampleBatch {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	batch := mobileMemorySampleBatch{
		Schema:  mobileMemorySampleSchema,
		Dropped: self.dropped,
		Samples: make([]mobileMemorySample, self.count),
	}
	for i := range self.count {
		index := (self.head + i) % len(self.samples)
		batch.Samples[i] = self.samples[index]
		self.samples[index] = mobileMemorySample{}
	}
	self.head = 0
	self.count = 0
	self.dropped = 0
	return batch
}

func (self *mobileMemorySampler) start(
	ctx context.Context,
	sample func() mobileMemorySample,
) {
	self.record(sample())
	go func() {
		ticker := time.NewTicker(mobileMemorySampleInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				self.record(sample())
			}
		}
	}()
}

func (self *DeviceLocal) memorySample() mobileMemorySample {
	var runtimeSnapshot mobileMemoryRuntimeSnapshot
	self.memorySampler.runtimeReader.read(&runtimeSnapshot)
	noteMobileRuntimeFootprint(runtimeSnapshot.totalByteCount)

	self.stateLock.Lock()
	remoteUserNatClient := self.remoteUserNatClient
	provider := self.provider
	self.stateLock.Unlock()

	trackedByteCount := self.dnsMemoryTarget.Used()
	if settings := self.settings.ClientSettings.SendBufferSettings; settings != nil && settings.ResendQueueBudget != nil {
		trackedByteCount += settings.ResendQueueBudget.UsedByteCount()
	}
	if settings := self.settings.ClientSettings.ReceiveBufferSettings; settings != nil && settings.ReceiveQueueBudget != nil {
		trackedByteCount += settings.ReceiveQueueBudget.UsedByteCount()
	}
	if provider != nil {
		sendBudget, receiveBudget := provider.transferBudgets()
		if sendBudget != nil {
			trackedByteCount += sendBudget.UsedByteCount()
		}
		if receiveBudget != nil {
			trackedByteCount += receiveBudget.UsedByteCount()
		}
	}

	var topology connect.MultiClientMemorySnapshot
	if multi, ok := remoteUserNatClient.(*connect.RemoteUserNatMultiClient); ok {
		topology = multi.MemorySnapshot()
	}
	return mobileMemorySample{
		UnixMillis:                     time.Now().UnixMilli(),
		GoTotalByteCount:               runtimeSnapshot.totalByteCount,
		GoLiveByteCount:                runtimeSnapshot.liveByteCount,
		GoGoalByteCount:                runtimeSnapshot.goalByteCount,
		GoLimitByteCount:               runtimeSnapshot.limitByteCount,
		PhysicalByteCount:              runtimeSnapshot.physicalByteCount,
		PhysicalPeakByteCount:          runtimeSnapshot.physicalPeakByteCount,
		PhysicalPressureCount:          runtimeSnapshot.physicalPressureCount,
		GoroutineCount:                 runtimeSnapshot.goroutineCount,
		PoolOutstandingCount:           runtimeSnapshot.poolOutstandingCount,
		PoolRetainedByteCount:          runtimeSnapshot.poolRetainedByteCount,
		PoolCapacityByteCount:          runtimeSnapshot.poolCapacityByteCount,
		PacketPressureDropCount:        self.mobilePacketPressureDropCount.Load(),
		DeviceTrackedByteCount:         int64(trackedByteCount),
		QualityClientCount:             int64(topology.QualityClientCount),
		SpeedClientCount:               int64(topology.SpeedClientCount),
		FlowCount:                      int64(topology.FlowCount),
		TransportBudgetUsedByteCount:   runtimeSnapshot.transportBudgetUsedByteCount,
		TransportBudgetUsedCount:       runtimeSnapshot.transportBudgetUsedCount,
		TransportBudgetPendingH1Count:  runtimeSnapshot.transportBudgetPendingH1Count,
		IdleReclaimCount:               runtimeSnapshot.idleReclaimCount,
		ForcedGCCount:                  runtimeSnapshot.forcedGCCount,
		GCCycleCount:                   runtimeSnapshot.gcCycleCount,
		TotalAllocatedByteCount:        runtimeSnapshot.totalAllocatedByteCount,
		ProfilingBucketByteCount:       runtimeSnapshot.profilingBucketByteCount,
		MemoryProfileRateByteCount:     runtimeSnapshot.memoryProfileRateByteCount,
		IdleReclaimDeferredCount:       runtimeSnapshot.idleReclaimDeferredCount,
		IdleReclaimBelowTargetCount:    runtimeSnapshot.idleReclaimBelowTargetCount,
		IdleReclaimCooldownCount:       runtimeSnapshot.idleReclaimCooldownCount,
		LastIdleReclaimBeforeByteCount: runtimeSnapshot.lastIdleReclaimBeforeByteCount,
		LastIdleReclaimAfterByteCount:  runtimeSnapshot.lastIdleReclaimAfterByteCount,
	}
}

// TakeMemorySamplesJson returns and clears the bounded production sampler as
// one JSON batch. The sampler itself records only primitives into a fixed ring;
// allocation and gomobile string conversion happen here, once per requested
// interval, instead of every second. An empty/non-mobile device returns a valid
// empty batch.
func (self *DeviceLocal) TakeMemorySamplesJson() string {
	batch := mobileMemorySampleBatch{Schema: mobileMemorySampleSchema, Samples: []mobileMemorySample{}}
	if self != nil && self.memorySampler != nil {
		batch = self.memorySampler.take()
	}
	encoded, err := json.Marshal(batch)
	if err != nil {
		return `{"schema":1,"dropped":0,"samples":[]}`
	}
	return string(encoded)
}
