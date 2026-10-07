//go:build race

package sdk

// raceEnabled reports whether the test binary was built with -race. Under -race,
// heap-delta assertions that are otherwise tight (e.g. the DMCA idle-scan reclaim)
// are relaxed, because the allocator/GC behavior under the race detector makes the
// live-heap delta unreliable; the no-leak proof there is the return-to-baseline
// check after teardown.
const raceEnabled = true
