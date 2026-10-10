package sdk

import (
	"encoding/json"
	"runtime"
	"runtime/metrics"
	"strings"
)

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
