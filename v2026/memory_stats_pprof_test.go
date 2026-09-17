//go:build !ios

package sdk

// The test travels with the code: WriteHeapProfile is built only when `ios` is
// not set (see memory_stats_pprof.go), so its test carries the same constraint.
// The rest of memory_stats_test.go stays unconstrained -- those tests cover the
// gauges and the trim path, which every platform builds.

import (
	"os"
	"path/filepath"
	"testing"
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
