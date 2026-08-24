package sdk

import (
	"encoding/json"
	"testing"

	"github.com/urnetwork/connect"
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
	device.memorySampler.record(mobileMemorySample{GoTotalByteCount: 42})
	var batch mobileMemorySampleBatch
	if err := json.Unmarshal([]byte(device.TakeMemorySamplesJson()), &batch); err != nil {
		t.Fatalf("decode sample batch: %v", err)
	}
	if batch.Schema != mobileMemorySampleSchema || len(batch.Samples) != 1 ||
		batch.Samples[0].GoTotalByteCount != 42 {
		t.Fatalf("sample batch = %+v", batch)
	}
}

func TestMobileDeviceMemorySampleHotPathDoesNotAllocate(t *testing.T) {
	settings := DefaultDeviceLocalSettings()
	device := &DeviceLocal{
		settings:        settings,
		dnsMemoryTarget: connect.NewMemoryTarget(1024),
		memorySampler:   &mobileMemorySampler{},
	}
	device.mobilePacketPressureDropCount.Store(17)
	if got := device.memorySample().PacketPressureDropCount; got != 17 {
		t.Fatalf("packet pressure drop count = %d, want 17", got)
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
