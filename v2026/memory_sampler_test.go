package sdk

import (
	"encoding/json"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

func TestMobileMemorySamplerIsBoundedOrderedAndDrainable(t *testing.T) {
	sampler := &mobileMemorySampler{}
	for i := 0; i < mobileMemorySampleCapacity+3; i += 1 {
		sampler.record(mobileMemorySample{UnixMillis: int64(i)})
	}
	batch := sampler.take()
	if got := len(batch.Samples); got != mobileMemorySampleCapacity {
		t.Fatalf("sample count = %d, want %d", got, mobileMemorySampleCapacity)
	}
	if batch.Dropped != 3 {
		t.Fatalf("dropped = %d, want 3", batch.Dropped)
	}
	if got := batch.Samples[0].UnixMillis; got != 3 {
		t.Fatalf("oldest retained sample = %d, want 3", got)
	}
	if got := batch.Samples[len(batch.Samples)-1].UnixMillis; got != mobileMemorySampleCapacity+2 {
		t.Fatalf("newest retained sample = %d, want %d", got, mobileMemorySampleCapacity+2)
	}
	empty := sampler.take()
	if len(empty.Samples) != 0 || empty.Dropped != 0 {
		t.Fatalf("second drain = %+v, want empty", empty)
	}
}

func TestMobileMemorySamplerRecordAllocatesNothing(t *testing.T) {
	sampler := &mobileMemorySampler{}
	sample := mobileMemorySample{GoTotalByteCount: 123}
	if allocations := testing.AllocsPerRun(1000, func() {
		sampler.record(sample)
	}); allocations != 0 {
		t.Fatalf("record allocations/run = %.2f, want 0", allocations)
	}
}

func TestTakeMemorySamplesJsonIsOneValidBatch(t *testing.T) {
	device := &DeviceLocal{memorySampler: &mobileMemorySampler{}}
	device.memorySampler.record(mobileMemorySample{
		GoTotalByteCount:                       42,
		PackHandoffSaturationCount:             7,
		PackHandoffDepthGrowCount:              3,
		PackHandoffDeepenedFlows:               2,
		PackHandoffAdaptiveMaxDepth:            96,
		PackHandoffAdaptiveMaxBytes:            192 * 1024,
		PlatformH1ReceiveQueueDropCount:        5,
		PlatformH1ReceiveQueueDropByteCount:    12 * 1024,
		PlatformH1ReceiveBackpressureCount:     7,
		PlatformH1ReceiveBackpressureByteCount: 24 * 1024,
		AckPendingResendPreemptCount:           11,
		ProviderPackHandoffDropCount:           13,
		ProviderPackHandoffDropByteCount:       17,
		ProviderPackHandoffWaitCount:           19,
		ProviderPackHandoffWaitSuccess:         23,
		ProviderPackHandoffMaxCount:            29,
		ProviderPackHandoffMaxByteCount:        31,
		ProviderAckRouteWriteCount:             37,
		ProviderAckRouteWriteBlockedCount:      41,
		ProviderAckRouteWriteErrorCount:        43,
		ProviderAckRouteWriteWaitNanos:         47,
		ProviderAckRouteWriteMaxWaitNanos:      53,
		ProviderInitialWriteCount:              59,
		ProviderInitialFrameCount:              61,
		ProviderInitialMessageByteCount:        67,
		ProviderTimeoutResendWriteCount:        71,
		ProviderAckPendingResendPreemptCount:   73,
		ProviderCarrierChangeWriteCount:        79,
		ProviderSelectiveGapWriteCount:         83,
		ProviderAckTailProbeWriteCount:         89,
		ProviderCumulativeProbeWriteCount:      97,
		ProviderRecoveryWriteErrorCount:        101,
	})
	var batch mobileMemorySampleBatch
	if err := json.Unmarshal([]byte(device.TakeMemorySamplesJson()), &batch); err != nil {
		t.Fatalf("decode sample batch: %v", err)
	}
	if batch.Schema != mobileMemorySampleSchema || len(batch.Samples) != 1 ||
		batch.Samples[0].GoTotalByteCount != 42 ||
		batch.Samples[0].PackHandoffSaturationCount != 7 ||
		batch.Samples[0].PackHandoffDepthGrowCount != 3 ||
		batch.Samples[0].PackHandoffDeepenedFlows != 2 ||
		batch.Samples[0].PackHandoffAdaptiveMaxDepth != 96 ||
		batch.Samples[0].PackHandoffAdaptiveMaxBytes != 192*1024 ||
		batch.Samples[0].PlatformH1ReceiveQueueDropCount != 5 ||
		batch.Samples[0].PlatformH1ReceiveQueueDropByteCount != 12*1024 ||
		batch.Samples[0].PlatformH1ReceiveBackpressureCount != 7 ||
		batch.Samples[0].PlatformH1ReceiveBackpressureByteCount != 24*1024 ||
		batch.Samples[0].AckPendingResendPreemptCount != 11 ||
		batch.Samples[0].ProviderPackHandoffDropCount != 13 ||
		batch.Samples[0].ProviderPackHandoffDropByteCount != 17 ||
		batch.Samples[0].ProviderPackHandoffWaitCount != 19 ||
		batch.Samples[0].ProviderPackHandoffWaitSuccess != 23 ||
		batch.Samples[0].ProviderPackHandoffMaxCount != 29 ||
		batch.Samples[0].ProviderPackHandoffMaxByteCount != 31 ||
		batch.Samples[0].ProviderAckRouteWriteCount != 37 ||
		batch.Samples[0].ProviderAckRouteWriteBlockedCount != 41 ||
		batch.Samples[0].ProviderAckRouteWriteErrorCount != 43 ||
		batch.Samples[0].ProviderAckRouteWriteWaitNanos != 47 ||
		batch.Samples[0].ProviderAckRouteWriteMaxWaitNanos != 53 ||
		batch.Samples[0].ProviderInitialWriteCount != 59 ||
		batch.Samples[0].ProviderInitialFrameCount != 61 ||
		batch.Samples[0].ProviderInitialMessageByteCount != 67 ||
		batch.Samples[0].ProviderTimeoutResendWriteCount != 71 ||
		batch.Samples[0].ProviderAckPendingResendPreemptCount != 73 ||
		batch.Samples[0].ProviderCarrierChangeWriteCount != 79 ||
		batch.Samples[0].ProviderSelectiveGapWriteCount != 83 ||
		batch.Samples[0].ProviderAckTailProbeWriteCount != 89 ||
		batch.Samples[0].ProviderCumulativeProbeWriteCount != 97 ||
		batch.Samples[0].ProviderRecoveryWriteErrorCount != 101 {
		t.Fatalf("sample batch = %+v", batch)
	}
}

func TestMobileDeviceMemorySampleHotPathDoesNotAllocate(t *testing.T) {
	settings := DefaultDeviceLocalSettings()
	resendQueueBudget := connect.NewTransferMemoryBudget(2048)
	if !resendQueueBudget.TryReserve(111) {
		t.Fatal("reserve test resend queue budget")
	}
	defer resendQueueBudget.Release(111)
	receiveQueueBudget := connect.NewTransferMemoryBudget(4096)
	if !receiveQueueBudget.TryReserve(222) {
		t.Fatal("reserve test receive queue budget")
	}
	defer receiveQueueBudget.Release(222)
	packQueueBudget := connect.NewTransferMemoryBudget(1024)
	if !packQueueBudget.TryReserve(321) {
		t.Fatal("reserve test pack queue budget")
	}
	defer packQueueBudget.Release(321)
	settings.ClientSettings.SendBufferSettings.ResendQueueBudget = resendQueueBudget
	settings.ClientSettings.ReceiveBufferSettings.ReceiveQueueBudget = receiveQueueBudget
	settings.ClientSettings.ReceiveBufferSettings.PackQueueBudget = packQueueBudget
	device := &DeviceLocal{
		settings:                      settings,
		dnsMemoryTarget:               connect.NewMemoryTarget(1024),
		memorySampler:                 &mobileMemorySampler{},
		platformTransportReceiveStats: &connect.PlatformTransportReceiveStats{},
	}
	device.mobilePacketPressureDropCount.Store(17)
	device.mobilePacketPressureDropBytes.Store(1700)
	device.mobilePacketPressureAckAdmits.Store(19)
	device.mobilePacketPressureAckDrops.Store(23)
	device.mobilePacketPressureOtherDrops.Store(29)
	sample := device.memorySample()
	if sample.PacketPressureDropCount != 17 ||
		sample.PacketPressureDropByteCount != 1700 ||
		sample.PacketPressureH1AckAdmitCount != 19 ||
		sample.PacketPressureAckDropCount != 23 ||
		sample.PacketPressureOtherDropCount != 29 {
		t.Fatalf("packet pressure sample = %+v", sample)
	}
	if sample.DeviceTrackedByteCount != 654 {
		t.Fatalf("tracked bytes = %d, want queue ownership 654", sample.DeviceTrackedByteCount)
	}
	if sample.ResendQueueUsedByteCount != 111 || sample.ResendQueueCapacityByteCount != 2048 {
		t.Fatalf(
			"resend queue sample = (%d/%d), want (111/2048)",
			sample.ResendQueueUsedByteCount,
			sample.ResendQueueCapacityByteCount,
		)
	}
	if sample.ReceiveQueueUsedByteCount != 222 || sample.ReceiveQueueCapacityByteCount != 4096 {
		t.Fatalf(
			"receive queue sample = (%d/%d), want (222/4096)",
			sample.ReceiveQueueUsedByteCount,
			sample.ReceiveQueueCapacityByteCount,
		)
	}
	if sample.PackQueueUsedByteCount != 321 || sample.PackQueueCapacityByteCount != 1024 {
		t.Fatalf(
			"pack queue sample = (%d/%d), want (321/1024)",
			sample.PackQueueUsedByteCount,
			sample.PackQueueCapacityByteCount,
		)
	}
	if allocations := testing.AllocsPerRun(25, func() {
		_ = device.memorySample()
	}); allocations != 0 {
		t.Fatalf("device memory sample allocations/run = %.2f, want 0", allocations)
	}
}

func TestTakeMemorySamplesJsonHandlesNilDevice(t *testing.T) {
	var device *DeviceLocal
	var batch mobileMemorySampleBatch
	if err := json.Unmarshal([]byte(device.TakeMemorySamplesJson()), &batch); err != nil {
		t.Fatalf("decode nil-device sample batch: %v", err)
	}
	if batch.Schema != mobileMemorySampleSchema || len(batch.Samples) != 0 || batch.Dropped != 0 {
		t.Fatalf("nil-device sample batch = %+v", batch)
	}
}
