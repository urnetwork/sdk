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
	mobileMemorySampleSchema   = 12
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

	PoolOutstandingCount                int64 `json:"pool_outstanding"`
	PacketPoolOutstandingByteCount      int64 `json:"packet_pool_outstanding_bytes"`
	DeviceTunEgressOutstandingByteCount int64 `json:"device_tun_egress_outstanding_bytes"`
	PoolRetainedByteCount               int64 `json:"pool_retained_bytes"`
	PacketPoolRetainedByteCount         int64 `json:"packet_pool_retained_bytes"`
	LargeObjectPoolRetainedByteCount    int64 `json:"large_object_pool_retained_bytes"`
	PoolCapacityByteCount               int64 `json:"pool_capacity_bytes"`
	PacketPressureDropCount             int64 `json:"packet_pressure_drops"`
	PacketPressureDropByteCount         int64 `json:"packet_pressure_drop_bytes"`
	PacketPressureH1AckAdmitCount       int64 `json:"packet_pressure_h1_ack_admits"`
	PacketPressureAckDropCount          int64 `json:"packet_pressure_ack_drops"`
	PacketPressureOtherDropCount        int64 `json:"packet_pressure_other_drops"`
	DeviceTrackedByteCount              int64 `json:"device_tracked_bytes"`
	ResendQueueUsedByteCount            int64 `json:"resend_queue_used_bytes"`
	ResendQueueCapacityByteCount        int64 `json:"resend_queue_capacity_bytes"`
	ReceiveQueueUsedByteCount           int64 `json:"receive_queue_used_bytes"`
	ReceiveQueueCapacityByteCount       int64 `json:"receive_queue_capacity_bytes"`
	PackQueueUsedByteCount              int64 `json:"pack_queue_used_bytes"`
	PackQueueCapacityByteCount          int64 `json:"pack_queue_capacity_bytes"`

	QualityClientCount int64 `json:"quality_clients"`
	SpeedClientCount   int64 `json:"speed_clients"`
	FlowCount          int64 `json:"flows"`

	PackHandoffDropCount                   int64 `json:"pack_handoff_drops"`
	PackHandoffDropByteCount               int64 `json:"pack_handoff_drop_bytes"`
	PackHandoffWaitCount                   int64 `json:"pack_handoff_waits"`
	PackHandoffWaitSuccess                 int64 `json:"pack_handoff_wait_successes"`
	PackHandoffMaxCount                    int64 `json:"pack_handoff_max_count"`
	PackHandoffMaxByteCount                int64 `json:"pack_handoff_max_bytes"`
	PackHandoffSaturationCount             int64 `json:"pack_handoff_saturations"`
	PackHandoffDepthGrowCount              int64 `json:"pack_handoff_depth_grows"`
	PackHandoffDeepenedFlows               int64 `json:"pack_handoff_deepened_flows"`
	PackHandoffAdaptiveMaxDepth            int64 `json:"pack_handoff_adaptive_max_depth"`
	PackHandoffAdaptiveMaxBytes            int64 `json:"pack_handoff_adaptive_max_bytes"`
	AckHandoffDropCount                    int64 `json:"ack_handoff_drops"`
	AckHandoffQueueFullCount               int64 `json:"ack_handoff_queue_full_drops"`
	AckHandoffMissCount                    int64 `json:"ack_handoff_misses"`
	AckHandoffWaitCount                    int64 `json:"ack_handoff_waits"`
	AckHandoffWaitSuccess                  int64 `json:"ack_handoff_wait_successes"`
	AckRouteWriteCount                     int64 `json:"ack_route_writes"`
	AckRoutePriorityWriteCount             int64 `json:"ack_route_priority_writes"`
	AckRouteWriteBlockedCount              int64 `json:"ack_route_write_blocks"`
	AckRouteWriteErrorCount                int64 `json:"ack_route_write_errors"`
	AckRouteWriteWaitNanos                 int64 `json:"ack_route_write_wait_nanos"`
	AckRouteWriteMaxWaitNanos              int64 `json:"ack_route_write_max_wait_nanos"`
	InitialWriteCount                      int64 `json:"initial_writes"`
	InitialFrameCount                      int64 `json:"initial_frames"`
	InitialMessageByteCount                int64 `json:"initial_message_bytes"`
	TimeoutResendWriteCount                int64 `json:"timeout_resend_writes"`
	AckPendingResendPreemptCount           int64 `json:"ack_pending_resend_preempts"`
	CarrierChangeWriteCount                int64 `json:"carrier_change_writes"`
	SelectiveGapWriteCount                 int64 `json:"selective_gap_writes"`
	AckTailProbeWriteCount                 int64 `json:"ack_tail_probe_writes"`
	CumulativeProbeWriteCount              int64 `json:"cumulative_probe_writes"`
	RecoveryWriteErrorCount                int64 `json:"recovery_write_errors"`
	PlatformH1ReceiveQueueDropCount        int64 `json:"platform_h1_receive_queue_drops"`
	PlatformH1ReceiveQueueDropByteCount    int64 `json:"platform_h1_receive_queue_drop_bytes"`
	PlatformH1ReceiveBackpressureCount     int64 `json:"platform_h1_receive_backpressure"`
	PlatformH1ReceiveBackpressureByteCount int64 `json:"platform_h1_receive_backpressure_bytes"`
	ProviderPackHandoffDropCount           int64 `json:"provider_pack_handoff_drops"`
	ProviderPackHandoffDropByteCount       int64 `json:"provider_pack_handoff_drop_bytes"`
	ProviderPackHandoffWaitCount           int64 `json:"provider_pack_handoff_waits"`
	ProviderPackHandoffWaitSuccess         int64 `json:"provider_pack_handoff_wait_successes"`
	ProviderPackHandoffMaxCount            int64 `json:"provider_pack_handoff_max_count"`
	ProviderPackHandoffMaxByteCount        int64 `json:"provider_pack_handoff_max_bytes"`
	ProviderAckRouteWriteCount             int64 `json:"provider_ack_route_writes"`
	ProviderAckRouteWriteBlockedCount      int64 `json:"provider_ack_route_write_blocks"`
	ProviderAckRouteWriteErrorCount        int64 `json:"provider_ack_route_write_errors"`
	ProviderAckRouteWriteWaitNanos         int64 `json:"provider_ack_route_write_wait_nanos"`
	ProviderAckRouteWriteMaxWaitNanos      int64 `json:"provider_ack_route_write_max_wait_nanos"`
	ProviderInitialWriteCount              int64 `json:"provider_initial_writes"`
	ProviderInitialFrameCount              int64 `json:"provider_initial_frames"`
	ProviderInitialMessageByteCount        int64 `json:"provider_initial_message_bytes"`
	ProviderTimeoutResendWriteCount        int64 `json:"provider_timeout_resend_writes"`
	ProviderAckPendingResendPreemptCount   int64 `json:"provider_ack_pending_resend_preempts"`
	ProviderCarrierChangeWriteCount        int64 `json:"provider_carrier_change_writes"`
	ProviderSelectiveGapWriteCount         int64 `json:"provider_selective_gap_writes"`
	ProviderAckTailProbeWriteCount         int64 `json:"provider_ack_tail_probe_writes"`
	ProviderCumulativeProbeWriteCount      int64 `json:"provider_cumulative_probe_writes"`
	ProviderRecoveryWriteErrorCount        int64 `json:"provider_recovery_write_errors"`

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
	totalByteCount                      int64
	liveByteCount                       int64
	goalByteCount                       int64
	limitByteCount                      int64
	physicalByteCount                   int64
	physicalPeakByteCount               int64
	physicalPressureCount               int64
	goroutineCount                      int64
	poolOutstandingCount                int64
	packetPoolOutstandingByteCount      int64
	deviceTunEgressOutstandingByteCount int64
	poolRetainedByteCount               int64
	packetPoolRetainedByteCount         int64
	largeObjectPoolRetainedByteCount    int64
	poolCapacityByteCount               int64
	transportBudgetUsedByteCount        int64
	transportBudgetUsedCount            int64
	transportBudgetPendingH1Count       int64
	idleReclaimCount                    int64
	forcedGCCount                       int64
	gcCycleCount                        int64
	totalAllocatedByteCount             int64
	profilingBucketByteCount            int64
	memoryProfileRateByteCount          int64
	idleReclaimDeferredCount            int64
	idleReclaimBelowTargetCount         int64
	idleReclaimCooldownCount            int64
	lastIdleReclaimBeforeByteCount      int64
	lastIdleReclaimAfterByteCount       int64
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
		packetPoolOutstandingByteCount: int64(connect.MessagePoolPacketOutstandingByteCount()),
		deviceTunEgressOutstandingByteCount: int64(
			poolStats.DeviceTunEgressOutstandingByteCount,
		),
		poolRetainedByteCount:       int64(poolStats.RetainedByteCount),
		packetPoolRetainedByteCount: int64(poolStats.PacketRetainedByteCount),
		largeObjectPoolRetainedByteCount: int64(
			poolStats.LargeObjectRetainedByteCount,
		),
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
	resendQueueUsedByteCount := connect.ByteCount(0)
	resendQueueCapacityByteCount := connect.ByteCount(0)
	receiveQueueUsedByteCount := connect.ByteCount(0)
	receiveQueueCapacityByteCount := connect.ByteCount(0)
	packQueueUsedByteCount := connect.ByteCount(0)
	packQueueCapacityByteCount := connect.ByteCount(0)
	if settings := self.settings.ClientSettings.SendBufferSettings; settings != nil && settings.ResendQueueBudget != nil {
		resendQueueUsedByteCount = settings.ResendQueueBudget.UsedByteCount()
		resendQueueCapacityByteCount = settings.ResendQueueBudget.TotalByteCount()
		trackedByteCount += resendQueueUsedByteCount
	}
	if settings := self.settings.ClientSettings.ReceiveBufferSettings; settings != nil {
		if settings.ReceiveQueueBudget != nil {
			receiveQueueUsedByteCount = settings.ReceiveQueueBudget.UsedByteCount()
			receiveQueueCapacityByteCount = settings.ReceiveQueueBudget.TotalByteCount()
			trackedByteCount += receiveQueueUsedByteCount
		}
		if settings.PackQueueBudget != nil {
			packQueueUsedByteCount = settings.PackQueueBudget.UsedByteCount()
			packQueueCapacityByteCount = settings.PackQueueBudget.TotalByteCount()
			trackedByteCount += packQueueUsedByteCount
		}
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
	var platformReceive connect.PlatformTransportReceiveStatsSnapshot
	if self.platformTransportReceiveStats != nil {
		platformReceive = self.platformTransportReceiveStats.Snapshot()
	}
	var providerReceive connect.ClientReceiveStatsSnapshot
	var providerRecovery connect.ClientSendRecoveryStatsSnapshot
	if provider != nil {
		if providerClient := provider.Client(); providerClient != nil {
			providerReceive = providerClient.ReceiveStats()
			providerRecovery = providerClient.SendRecoveryStats()
		}
	}
	return mobileMemorySample{
		UnixMillis:                             time.Now().UnixMilli(),
		GoTotalByteCount:                       runtimeSnapshot.totalByteCount,
		GoLiveByteCount:                        runtimeSnapshot.liveByteCount,
		GoGoalByteCount:                        runtimeSnapshot.goalByteCount,
		GoLimitByteCount:                       runtimeSnapshot.limitByteCount,
		PhysicalByteCount:                      runtimeSnapshot.physicalByteCount,
		PhysicalPeakByteCount:                  runtimeSnapshot.physicalPeakByteCount,
		PhysicalPressureCount:                  runtimeSnapshot.physicalPressureCount,
		GoroutineCount:                         runtimeSnapshot.goroutineCount,
		PoolOutstandingCount:                   runtimeSnapshot.poolOutstandingCount,
		PacketPoolOutstandingByteCount:         runtimeSnapshot.packetPoolOutstandingByteCount,
		DeviceTunEgressOutstandingByteCount:    runtimeSnapshot.deviceTunEgressOutstandingByteCount,
		PoolRetainedByteCount:                  runtimeSnapshot.poolRetainedByteCount,
		PacketPoolRetainedByteCount:            runtimeSnapshot.packetPoolRetainedByteCount,
		LargeObjectPoolRetainedByteCount:       runtimeSnapshot.largeObjectPoolRetainedByteCount,
		PoolCapacityByteCount:                  runtimeSnapshot.poolCapacityByteCount,
		PacketPressureDropCount:                self.mobilePacketPressureDropCount.Load(),
		PacketPressureDropByteCount:            self.mobilePacketPressureDropBytes.Load(),
		PacketPressureH1AckAdmitCount:          self.mobilePacketPressureAckAdmits.Load(),
		PacketPressureAckDropCount:             self.mobilePacketPressureAckDrops.Load(),
		PacketPressureOtherDropCount:           self.mobilePacketPressureOtherDrops.Load(),
		DeviceTrackedByteCount:                 int64(trackedByteCount),
		ResendQueueUsedByteCount:               int64(resendQueueUsedByteCount),
		ResendQueueCapacityByteCount:           int64(resendQueueCapacityByteCount),
		ReceiveQueueUsedByteCount:              int64(receiveQueueUsedByteCount),
		ReceiveQueueCapacityByteCount:          int64(receiveQueueCapacityByteCount),
		PackQueueUsedByteCount:                 int64(packQueueUsedByteCount),
		PackQueueCapacityByteCount:             int64(packQueueCapacityByteCount),
		QualityClientCount:                     int64(topology.QualityClientCount),
		SpeedClientCount:                       int64(topology.SpeedClientCount),
		FlowCount:                              int64(topology.FlowCount),
		PackHandoffDropCount:                   int64(topology.PackHandoffDropCount),
		PackHandoffDropByteCount:               int64(topology.PackHandoffDropByteCount),
		PackHandoffWaitCount:                   int64(topology.PackHandoffWaitCount),
		PackHandoffWaitSuccess:                 int64(topology.PackHandoffWaitSuccess),
		PackHandoffMaxCount:                    int64(topology.PackHandoffMaxCount),
		PackHandoffMaxByteCount:                int64(topology.PackHandoffMaxByteCount),
		PackHandoffSaturationCount:             int64(topology.PackHandoffSaturationCount),
		PackHandoffDepthGrowCount:              int64(topology.PackHandoffDepthGrowCount),
		PackHandoffDeepenedFlows:               int64(topology.PackHandoffDeepenedFlows),
		PackHandoffAdaptiveMaxDepth:            int64(topology.PackHandoffAdaptiveMaxDepth),
		PackHandoffAdaptiveMaxBytes:            int64(topology.PackHandoffAdaptiveMaxByteCount),
		AckHandoffDropCount:                    int64(topology.AckHandoffDropCount),
		AckHandoffQueueFullCount:               int64(topology.AckHandoffQueueFullCount),
		AckHandoffMissCount:                    int64(topology.AckHandoffMissCount),
		AckHandoffWaitCount:                    int64(topology.AckHandoffWaitCount),
		AckHandoffWaitSuccess:                  int64(topology.AckHandoffWaitSuccess),
		AckRouteWriteCount:                     int64(topology.AckRouteWriteCount),
		AckRoutePriorityWriteCount:             int64(topology.AckRoutePriorityWriteCount),
		AckRouteWriteBlockedCount:              int64(topology.AckRouteWriteBlockedCount),
		AckRouteWriteErrorCount:                int64(topology.AckRouteWriteErrorCount),
		AckRouteWriteWaitNanos:                 int64(topology.AckRouteWriteWaitNanos),
		AckRouteWriteMaxWaitNanos:              int64(topology.AckRouteWriteMaxWaitNanos),
		InitialWriteCount:                      int64(topology.InitialWriteCount),
		InitialFrameCount:                      int64(topology.InitialFrameCount),
		InitialMessageByteCount:                int64(topology.InitialMessageByteCount),
		TimeoutResendWriteCount:                int64(topology.TimeoutResendWriteCount),
		AckPendingResendPreemptCount:           int64(topology.AckPendingResendPreemptCount),
		CarrierChangeWriteCount:                int64(topology.CarrierChangeWriteCount),
		SelectiveGapWriteCount:                 int64(topology.SelectiveGapWriteCount),
		AckTailProbeWriteCount:                 int64(topology.AckTailProbeWriteCount),
		CumulativeProbeWriteCount:              int64(topology.CumulativeProbeWriteCount),
		RecoveryWriteErrorCount:                int64(topology.RecoveryWriteErrorCount),
		PlatformH1ReceiveQueueDropCount:        int64(platformReceive.H1.QueueDropMessageCount),
		PlatformH1ReceiveQueueDropByteCount:    int64(platformReceive.H1.QueueDropByteCount),
		PlatformH1ReceiveBackpressureCount:     int64(platformReceive.H1.QueueBackpressureMessageCount),
		PlatformH1ReceiveBackpressureByteCount: int64(platformReceive.H1.QueueBackpressureByteCount),
		ProviderPackHandoffDropCount:           int64(providerReceive.PackHandoffDropCount),
		ProviderPackHandoffDropByteCount:       int64(providerReceive.PackHandoffDropByteCount),
		ProviderPackHandoffWaitCount:           int64(providerReceive.PackHandoffWaitCount),
		ProviderPackHandoffWaitSuccess:         int64(providerReceive.PackHandoffWaitSuccess),
		ProviderPackHandoffMaxCount:            int64(providerReceive.PackHandoffMaxCount),
		ProviderPackHandoffMaxByteCount:        int64(providerReceive.PackHandoffMaxByteCount),
		ProviderAckRouteWriteCount:             int64(providerReceive.AckRouteWriteCount),
		ProviderAckRouteWriteBlockedCount:      int64(providerReceive.AckRouteWriteBlockedCount),
		ProviderAckRouteWriteErrorCount:        int64(providerReceive.AckRouteWriteErrorCount),
		ProviderAckRouteWriteWaitNanos:         int64(providerReceive.AckRouteWriteWaitDuration),
		ProviderAckRouteWriteMaxWaitNanos:      int64(providerReceive.AckRouteWriteMaxWait),
		ProviderInitialWriteCount:              int64(providerRecovery.InitialWriteCount),
		ProviderInitialFrameCount:              int64(providerRecovery.InitialFrameCount),
		ProviderInitialMessageByteCount:        int64(providerRecovery.InitialMessageByteCount),
		ProviderTimeoutResendWriteCount:        int64(providerRecovery.TimeoutResendWriteCount),
		ProviderAckPendingResendPreemptCount:   int64(providerRecovery.AckPendingResendPreemptCount),
		ProviderCarrierChangeWriteCount:        int64(providerRecovery.CarrierChangeWriteCount),
		ProviderSelectiveGapWriteCount:         int64(providerRecovery.SelectiveGapWriteCount),
		ProviderAckTailProbeWriteCount:         int64(providerRecovery.AckTailProbeWriteCount),
		ProviderCumulativeProbeWriteCount:      int64(providerRecovery.CumulativeProbeWriteCount),
		ProviderRecoveryWriteErrorCount:        int64(providerRecovery.RecoveryWriteErrorCount),
		TransportBudgetUsedByteCount:           runtimeSnapshot.transportBudgetUsedByteCount,
		TransportBudgetUsedCount:               runtimeSnapshot.transportBudgetUsedCount,
		TransportBudgetPendingH1Count:          runtimeSnapshot.transportBudgetPendingH1Count,
		IdleReclaimCount:                       runtimeSnapshot.idleReclaimCount,
		ForcedGCCount:                          runtimeSnapshot.forcedGCCount,
		GCCycleCount:                           runtimeSnapshot.gcCycleCount,
		TotalAllocatedByteCount:                runtimeSnapshot.totalAllocatedByteCount,
		ProfilingBucketByteCount:               runtimeSnapshot.profilingBucketByteCount,
		MemoryProfileRateByteCount:             runtimeSnapshot.memoryProfileRateByteCount,
		IdleReclaimDeferredCount:               runtimeSnapshot.idleReclaimDeferredCount,
		IdleReclaimBelowTargetCount:            runtimeSnapshot.idleReclaimBelowTargetCount,
		IdleReclaimCooldownCount:               runtimeSnapshot.idleReclaimCooldownCount,
		LastIdleReclaimBeforeByteCount:         runtimeSnapshot.lastIdleReclaimBeforeByteCount,
		LastIdleReclaimAfterByteCount:          runtimeSnapshot.lastIdleReclaimAfterByteCount,
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
		return `{"schema":12,"dropped":0,"samples":[]}`
	}
	return string(encoded)
}
