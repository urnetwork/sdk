package main

import (
	"runtime/debug"
	"sync"

	"github.com/urnetwork/glog/v2026"
)

// opaque handle registry
// every Go object exposed to the C side is stored here under a uint64 id,
// which keeps the object reachable until the C side calls urnet_release.
// ids are never reused so a stale handle can never resolve to a new object.

var handleRegistry = &struct {
	mutex  sync.RWMutex
	nextId uint64
	values map[uint64]any
}{
	values: map[uint64]any{},
}

func newHandle(value any) uint64 {
	if value == nil {
		return 0
	}
	handleRegistry.mutex.Lock()
	defer handleRegistry.mutex.Unlock()
	handleRegistry.nextId += 1
	id := handleRegistry.nextId
	handleRegistry.values[id] = value
	return id
}

func handleValue(id uint64) (any, bool) {
	handleRegistry.mutex.RLock()
	defer handleRegistry.mutex.RUnlock()
	value, ok := handleRegistry.values[id]
	return value, ok
}

func handleRelease(id uint64) bool {
	handleRegistry.mutex.Lock()
	defer handleRegistry.mutex.Unlock()
	_, ok := handleRegistry.values[id]
	if ok {
		delete(handleRegistry.values, id)
	}
	return ok
}

func handleCount() int64 {
	handleRegistry.mutex.RLock()
	defer handleRegistry.mutex.RUnlock()
	return int64(len(handleRegistry.values))
}

// resolveHandle looks up a handle and type-asserts it.
// a zero id resolves to the zero value (nil), which mirrors passing nil in Go.
func resolveHandle[T any](id uint64, name string) (T, bool) {
	var zero T
	if id == 0 {
		return zero, true
	}
	value, ok := handleValue(id)
	if !ok {
		glog.Errorf("[cgo]%s: unknown handle %d", name, id)
		return zero, false
	}
	typedValue, ok := value.(T)
	if !ok {
		glog.Errorf("[cgo]%s: handle %d has unexpected type %T", name, id, value)
		return zero, false
	}
	return typedValue, true
}

// cgoGuard must be deferred at the top of every exported function.
// a panic must never unwind into C, which would abort the host process.
func cgoGuard(name string) {
	if r := recover(); r != nil {
		glog.Errorf("[cgo]%s panicked: %v\n%s", name, r, string(debug.Stack()))
	}
}
