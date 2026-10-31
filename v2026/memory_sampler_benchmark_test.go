package sdk

import (
	"testing"
	"unsafe"
)

func BenchmarkMobileMemoryRuntimeReader(b *testing.B) {
	reader := &mobileMemoryRuntimeReader{}
	var snapshot mobileMemoryRuntimeSnapshot
	reader.read(&snapshot, nil)
	b.ReportAllocs()
	for b.Loop() {
		reader.read(&snapshot, nil)
	}
	b.ReportMetric(float64(unsafe.Sizeof(mobileMemorySample{}))*mobileMemorySampleCapacity, "ring-B")
	b.ReportMetric(float64(unsafe.Sizeof(mobileMemoryRuntimeReader{})), "reader-B")
}
