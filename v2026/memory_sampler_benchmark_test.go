package sdk

import (
	"testing"
	"unsafe"
)

func BenchmarkMobileMemoryRuntimeReader(b *testing.B) {
	reader := &mobileMemoryRuntimeReader{}
	var snapshot mobileMemoryRuntimeSnapshot
	reader.read(&snapshot)
	b.ReportAllocs()
	for b.Loop() {
		reader.read(&snapshot)
	}
	b.ReportMetric(float64(unsafe.Sizeof(mobileMemorySample{}))*mobileMemorySampleCapacity, "ring-B")
	b.ReportMetric(float64(unsafe.Sizeof(mobileMemoryRuntimeReader{})), "reader-B")
}
