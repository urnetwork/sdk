package sdk

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"runtime/metrics"
	"runtime/pprof"
	"strings"
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

// memoryClassNames are the Go runtime's memory classes. Their sum is
// /memory/classes/total:bytes, and goRuntimeBytes is that total minus the
// heap bytes already released to the operating system. Reporting the classes
// is the only way to say what fills the envelope: most of it is runtime
// structure (stacks, span and GC metadata, unreleased heap) that no SDK
// budget constant can guard.
var memoryClassNames = []string{
	"/memory/classes/total:bytes",
	"/memory/classes/heap/objects:bytes",
	"/memory/classes/heap/unused:bytes",
	"/memory/classes/heap/free:bytes",
	"/memory/classes/heap/released:bytes",
	"/memory/classes/heap/stacks:bytes",
	"/memory/classes/os-stacks:bytes",
	"/memory/classes/metadata/mspan/inuse:bytes",
	"/memory/classes/metadata/mspan/free:bytes",
	"/memory/classes/metadata/mcache/inuse:bytes",
	"/memory/classes/metadata/mcache/free:bytes",
	"/memory/classes/metadata/other:bytes",
	"/memory/classes/profiling/buckets:bytes",
	"/memory/classes/other:bytes",
	"/gc/heap/live:bytes",
	"/gc/heap/goal:bytes",
	"/gc/heap/objects:objects",
	"/sched/goroutines:goroutines",
}

// MemoryClassesJsonForDiag returns the Go runtime's memory classes as one
// JSON object, keyed by the class name with the leading path stripped. A
// debug-only seam for the physical rig; it reads metrics and allocates only
// the returned string.
func MemoryClassesJsonForDiag() string {
	samples := make([]metrics.Sample, len(memoryClassNames))
	for i, name := range memoryClassNames {
		samples[i].Name = name
	}
	metrics.Read(samples)
	out := map[string]uint64{}
	for i, sample := range samples {
		key := memoryClassNames[i]
		key = strings.TrimPrefix(key, "/memory/classes/")
		key = strings.TrimPrefix(key, "/")
		if index := strings.IndexByte(key, ':'); 0 <= index {
			key = key[:index]
		}
		key = strings.ReplaceAll(key, "/", "_")
		if sample.Value.Kind() == metrics.KindUint64 {
			out[key] = sample.Value.Uint64()
		}
	}
	out["goroutines_count"] = uint64(runtime.NumGoroutine())
	encoded, err := json.Marshal(out)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}
