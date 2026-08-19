package main

import (
	"runtime/debug"
	"sync"

	"github.com/urnetwork/glog"
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
//
// The zero id is the abi's null handle and answers ok=false, so exported
// functions no-op cleanly instead of proceeding onto a nil receiver. It used
// to answer (nil, ok=true) "to mirror passing nil in Go", which meant every
// method called through a zero handle became a guarded nil-receiver panic:
// measured at ~570 "[cgo]urnet_connect_grid_get_* panicked" log lines per
// idle signed-in session from a host app polling stats through a zero grid
// handle. Zero is deliberately not logged here — it is a legal "no object"
// value, unlike an unknown (stale) id, which is a caller bug and stays loud.
//
// Argument-position handles keep their nil-object semantics: the generated
// bindings translate a zero argument to a nil Go value without calling
// resolveHandle (see gen/gen.go kindHandle), so e.g.
// urnet_network_space_manager_set_active_network_space(self, 0) still means
// SetActiveNetworkSpace(nil).
func resolveHandle[T any](id uint64, name string) (T, bool) {
	var zero T
	if id == 0 {
		return zero, false
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
