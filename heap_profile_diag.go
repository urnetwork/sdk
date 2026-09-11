package sdk

import (
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"time"
)

// WriteHeapProfileForDiag writes a Go heap profile to path and returns a short
// description of what was written. It is a debug-only seam for the physical
// rig (connect/FLIGHTGATEFIX.md): nothing calls it unless a diagnostic command
// asks, and an ordinary build never reaches it.
//
// A forced collection runs first so the profile's inuse_space describes what
// survived collection rather than what was merely unswept. That collection
// perturbs the runtime, so the sample after a profile is not an unperturbed
// recovery point (connect/MEMSTEADY.md).
//
// The profile is empty unless the library was linked with a nonzero
// memprofilerate; the returned description carries the rate so a reader can
// tell an empty profile from an empty heap.
func WriteHeapProfileForDiag(path string) (string, error) {
	rate := runtime.MemProfileRate
	runtime.GC()
	// let finalizers and the sweep settle before the profile is taken
	time.Sleep(200 * time.Millisecond)
	runtime.GC()
	file, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	if err := pprof.WriteHeapProfile(file); err != nil {
		return "", err
	}
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return fmt.Sprintf(
		"path=%s bytes=%d memprofilerate=%d heap_inuse=%d heap_alloc=%d sys=%d goroutines=%d",
		path, info.Size(), rate, stats.HeapInuse, stats.HeapAlloc, stats.Sys, runtime.NumGoroutine(),
	), nil
}
