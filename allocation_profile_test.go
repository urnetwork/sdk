package sdk

import (
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// Counts what each call of f allocates, as testing.AllocsPerRun does, but from
// the allocation profile and only under f's own frame, closures inside it
// included. testing.AllocsPerRun reads the process's malloc count, which in a
// whole-package run takes in whatever another goroutine allocates in the
// window, an earlier test's devices winding down among them, and a check of
// zero allocations then fails for objects that are not f's. As
// testing.AllocsPerRun does, it calls f once before counting, holds the
// process to one P while it counts, so a per-P cache a call draws from is the
// one the call before returned to, and answers the whole-number average over
// runs. f's allocations must be made within about 30 calls of f, as the
// profile records at most 32 frames of each allocation's stack.
func profiledAllocsPerRun(t *testing.T, runs int, f func()) float64 {
	t.Helper()
	frame := runtime.FuncForPC(reflect.ValueOf(f).Pointer()).Name()
	underFrame := func(name string) bool {
		return name == frame || strings.HasPrefix(name, frame+".")
	}
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	f()
	allocations := profiledAllocationsMatching(t, underFrame, false, func() {
		for range runs {
			f()
		}
	})
	return float64(allocations / int64(runs))
}

var testingAllocationSink []byte

// Makes one 64-byte object depth calls below its caller.
//
//go:noinline
func testingAllocateBelow(depth int) {
	if 0 < depth {
		testingAllocateBelow(depth - 1)
		return
	}
	testingAllocationSink = make([]byte, 64)
}

// A run's count is the run's own: a run that makes two objects twenty calls
// below it counts two, and still two while another goroutine allocates three
// in each run's window, where testing.AllocsPerRun's process-wide count takes
// the other goroutine's in as the run's.
func TestProfiledAllocsPerRunCountsOnlyTheRunsOwn(t *testing.T) {
	turns := make(chan struct{})
	turnDone := make(chan struct{})
	otherDone := make(chan struct{})
	go func() {
		defer close(otherDone)
		for range turns {
			for range 3 {
				testingAllocateBelow(0)
			}
			turnDone <- struct{}{}
		}
	}()
	defer func() {
		close(turns)
		<-otherDone
	}()
	handTurn := func() {
		turns <- struct{}{}
		<-turnDone
	}

	alone := profiledAllocsPerRun(t, 100, func() {
		testingAllocateBelow(20)
		testingAllocateBelow(20)
	})
	if alone != 2 {
		t.Fatalf("a run that makes two objects counted %.0f", alone)
	}
	besideOthers := profiledAllocsPerRun(t, 100, func() {
		testingAllocateBelow(20)
		testingAllocateBelow(20)
		handTurn()
	})
	if besideOthers != 2 {
		t.Fatalf("a run that makes two objects counted %.0f beside another goroutine's three", besideOthers)
	}
	processWide := testing.AllocsPerRun(100, func() {
		testingAllocateBelow(20)
		testingAllocateBelow(20)
		handTurn()
	})
	if processWide < 5 {
		t.Fatalf("the process-wide count of a run that makes two objects beside another goroutine's three was %.0f, want at least 5", processWide)
	}
}
