package sdk

import (
	"runtime"
	"strings"
	"sync"
	"testing"
)

// Counts the timer objects (time.NewTimer, time.After, time.AfterFunc) that
// were allocated from inside the named function -- closures included -- while
// run executes. It reads the allocation profile at full sampling, so the count
// is exact for the allocations made while run is in progress.
//
// A loop that arms a new timer for every wait allocates a timer and its
// channel on every tick; one that resets a single timer allocates them once.
func timerAllocationsUnder(t *testing.T, function string, run func()) int64 {
	t.Helper()
	return profiledAllocationsUnder(t, function, true, run)
}

// Counts the objects allocated while run executes from inside any function
// whose name contains function, closures included.
func profiledAllocationsUnder(t *testing.T, function string, timersOnly bool, run func()) int64 {
	t.Helper()
	return profiledAllocationsMatching(t, func(frame string) bool {
		return strings.Contains(frame, function)
	}, timersOnly, run)
}

// Counts the objects allocated while run executes from inside a function owned
// answers true for. The allocation profile records at most 32 frames of each
// allocation's stack, so an allocation made more than about 30 calls below
// the owned function does not count.
func profiledAllocationsMatching(t *testing.T, owned func(frame string) bool, timersOnly bool, run func()) int64 {
	t.Helper()
	timerAllocationProfileLock.Lock()
	defer timerAllocationProfileLock.Unlock()

	previousRate := runtime.MemProfileRate
	runtime.MemProfileRate = 1
	defer func() {
		runtime.MemProfileRate = previousRate
	}()

	// the profile reports allocations as of the most recent completed cycle
	runtime.GC()
	runtime.GC()
	before := profiledAllocations(owned, timersOnly)
	run()
	runtime.GC()
	runtime.GC()
	return profiledAllocations(owned, timersOnly) - before
}

// The memory profile rate is process state.
var timerAllocationProfileLock sync.Mutex

func profiledAllocations(owned func(frame string) bool, timersOnly bool) int64 {
	n, _ := runtime.MemProfile(nil, true)
	records := make([]runtime.MemProfileRecord, n+256)
	n, ok := runtime.MemProfile(records, true)
	if !ok {
		n = len(records)
	}
	total := int64(0)
	for _, record := range records[:n] {
		timer := false
		ownedRecord := false
		frames := runtime.CallersFrames(record.Stack())
		for {
			frame, more := frames.Next()
			switch frame.Function {
			case "time.NewTimer", "time.After", "time.AfterFunc", "time.newTimer", "time.NewTicker":
				timer = true
			}
			if owned(frame.Function) {
				ownedRecord = true
			}
			if !more {
				break
			}
		}
		if ownedRecord && (timer || !timersOnly) {
			total += record.AllocObjects
		}
	}
	return total
}
