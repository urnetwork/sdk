package sdk

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// On 2026-11-13 an iOS packet tunnel near its memory warning line called
// FreeMemory from the OS pressure callback up to 42 times in one second. Every
// call shed the tunnel's DNS answers, pooled DoH connections and affinity maps
// and forced a full collection, so a burst of warnings was a burst of cold
// lookups and stalls. A burst now releases once.
func TestFreeMemoryCoalescesAPressureBurst(t *testing.T) {
	func() {
		processMemoryRelease.stateLock.Lock()
		defer processMemoryRelease.stateLock.Unlock()
		processMemoryRelease.lastReleaseTime = time.Time{}
		processMemoryRelease.coalescedCount = 0
	}()
	var shedCount atomic.Int64
	unregister := connect.AddMemoryShedder(func() {
		shedCount.Add(1)
	})
	defer unregister()

	for range 42 {
		FreeMemory()
	}
	if got := shedCount.Load(); got != 1 {
		t.Fatalf("a burst of 42 pressure calls shed %d times, want 1", got)
	}
}

// One release per interval: the calls inside it are coalesced and counted
// into the next release.
func TestMemoryReleaseAdmitsOncePerInterval(t *testing.T) {
	release := newMemoryRelease(defaultFreeMemorySettings())
	t0 := time.Unix(1_000_000, 0)
	if _, admitted := release.admit(t0); !admitted {
		t.Fatal("the first call was not released")
	}
	for i := 1; i <= 41; i += 1 {
		if _, admitted := release.admit(t0.Add(time.Duration(i) * 20 * time.Millisecond)); admitted {
			t.Fatalf("call %d inside the interval was released", i)
		}
	}
	if _, admitted := release.admit(t0.Add(30*time.Second - time.Nanosecond)); admitted {
		t.Fatal("released before the interval ended")
	}
	coalescedCount, admitted := release.admit(t0.Add(30 * time.Second))
	if !admitted {
		t.Fatal("pressure that lasted the interval was not released again")
	}
	if coalescedCount != 42 {
		t.Fatalf("coalesced=%d, want 42", coalescedCount)
	}
}

// A non-positive interval releases on every call, as before.
func TestMemoryReleaseIntervalDisabled(t *testing.T) {
	settings := defaultFreeMemorySettings()
	settings.MinInterval = 0
	release := newMemoryRelease(settings)
	t0 := time.Unix(1_000_000, 0)
	for i := range 3 {
		if _, admitted := release.admit(t0); !admitted {
			t.Fatalf("call %d was not released with the interval off", i)
		}
	}
}
