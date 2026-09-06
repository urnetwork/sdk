package sdk

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLocalStateEquivalentPerformanceProfileSkipsFilesystemWrite(t *testing.T) {
	localState := newLocalState(context.Background(), t.TempDir())
	profile := &PerformanceProfile{
		WindowType: WindowTypeSpeed,
		WindowSize: &WindowSizeSettings{
			WindowSizeMin: 2,
			WindowSizeMax: 4,
		},
	}
	if err := localState.SetPerformanceProfile(profile); err != nil {
		t.Fatalf("initial profile write: %v", err)
	}
	path := filepath.Join(localState.localStorageDir, ".performance_profile")
	knownTime := time.Unix(123456789, 0)
	if err := os.Chtimes(path, knownTime, knownTime); err != nil {
		t.Fatalf("set known profile time: %v", err)
	}

	if err := localState.SetPerformanceProfile(clonePerformanceProfile(profile)); err != nil {
		t.Fatalf("identical profile write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat profile: %v", err)
	}
	if !info.ModTime().Equal(knownTime) {
		t.Fatalf("identical profile rewrote file: modtime = %s", info.ModTime())
	}
}

func TestLocalStateChangedPerformanceProfileWritesFilesystem(t *testing.T) {
	localState := newLocalState(context.Background(), t.TempDir())
	profile := &PerformanceProfile{WindowType: WindowTypeSpeed}
	if err := localState.SetPerformanceProfile(profile); err != nil {
		t.Fatalf("initial profile write: %v", err)
	}
	path := filepath.Join(localState.localStorageDir, ".performance_profile")
	knownTime := time.Unix(123456789, 0)
	if err := os.Chtimes(path, knownTime, knownTime); err != nil {
		t.Fatalf("set known profile time: %v", err)
	}

	changed := &PerformanceProfile{WindowType: WindowTypeQuality}
	if err := localState.SetPerformanceProfile(changed); err != nil {
		t.Fatalf("changed profile write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat profile: %v", err)
	}
	if info.ModTime().Equal(knownTime) {
		t.Fatalf("changed profile did not rewrite file")
	}
}
