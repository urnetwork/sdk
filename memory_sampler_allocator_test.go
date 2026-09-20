package sdk

import (
	"encoding/json"
	"testing"
)

func TestMobileAllocatorClassReaderDoesNotAllocate(t *testing.T) {
	reader := &mobileMemoryRuntimeReader{}
	var snapshot mobileMemoryRuntimeSnapshot
	reader.read(&snapshot)
	if allocations := testing.AllocsPerRun(1000, func() { reader.read(&snapshot) }); allocations != 0 {
		t.Fatalf("allocator-class reader allocations = %v, want zero", allocations)
	}
	if snapshot.heapAllocByteCount <= 0 || snapshot.stackInuseByteCount <= 0 ||
		snapshot.heapUnusedByteCount < 0 || snapshot.heapFreeByteCount < 0 {
		t.Fatalf("incomplete allocator classes: %+v", snapshot)
	}
	for i := 9; i < len(reader.samples); i++ {
		if reader.samples[i].Value.Kind() == 0 {
			t.Fatalf("unsupported allocator metric: %s", reader.samples[i].Name)
		}
	}
}

func TestMobileAllocatorClassesSurvivePrimitiveRingAndJSON(t *testing.T) {
	device := &DeviceLocal{memorySampler: &mobileMemorySampler{}}
	device.memorySampler.record(mobileMemorySample{
		GoHeapAllocByteCount:  101,
		GoHeapUnusedByteCount: 103,
		GoHeapFreeByteCount:   107,
		GoStackInuseByteCount: 109,
	})
	var decoded struct {
		Schema  int                `json:"schema"`
		Samples []map[string]int64 `json:"samples"`
	}
	if err := json.Unmarshal([]byte(device.TakeMemorySamplesJson()), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Schema != 13 || len(decoded.Samples) != 1 {
		t.Fatalf("unexpected allocator sample schema/length: %+v", decoded)
	}
	for key, want := range map[string]int64{
		"go_heap_alloc_bytes": 101, "go_heap_unused_bytes": 103,
		"go_heap_free_bytes": 107, "go_stack_inuse_bytes": 109,
	} {
		if got := decoded.Samples[0][key]; got != want {
			t.Fatalf("%s=%d, want%d", key, got, want)
		}
	}
}
