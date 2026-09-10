package sdk

import (
	"os"
	"runtime"
	"strconv"
	"testing"
)

// TestMobileBuildMemoryProfileRate is exercised by
// build/check_mobile_runtime_policy with the same linker setting used by all
// Android and Apple gomobile artifacts. Ordinary `go test` runs skip it because
// their runtime policy is controlled by the Go tool, not the release Makefile.
func TestMobileBuildMemoryProfileRate(t *testing.T) {
	expectedText := os.Getenv("SDK_EXPECT_MEMORY_PROFILE_RATE")
	if expectedText == "" {
		t.Skip("mobile release linker policy is not active")
	}
	expected, err := strconv.Atoi(expectedText)
	if err != nil {
		t.Fatalf("parse SDK_EXPECT_MEMORY_PROFILE_RATE: %v", err)
	}
	if runtime.MemProfileRate != expected {
		t.Fatalf("runtime.MemProfileRate = %d, want %d", runtime.MemProfileRate, expected)
	}
	var reader mobileMemoryRuntimeReader
	var snapshot mobileMemoryRuntimeSnapshot
	reader.read(&snapshot)
	if snapshot.memoryProfileRateByteCount != int64(expected) {
		t.Fatalf(
			"sampler memory profile rate = %d, want %d",
			snapshot.memoryProfileRateByteCount,
			expected,
		)
	}
}
