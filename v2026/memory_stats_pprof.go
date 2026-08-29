//go:build !ios

package sdk

import (
	"os"
	"runtime"
	"runtime/pprof"
)

// Heap profiling, kept OUT of the iOS builds.
//
// runtime/pprof is a debug affordance, and linking it costs compiled size in an
// artifact that has an explicit size budget. memory_stats.go carried no build
// constraint, so pprof went into every binding gomobile produces -- including
// URnetworkExtensionSdk, the slice that runs inside the iOS NetworkExtension.
// That slice is the tightest budget of the three in build/check_apple_size.sh
// and the one that sits closest to the extension's runtime memory limit.
//
// Nothing shipped calls this. Checked across every consumer of the SDK at the
// time of writing -- apple, linux, connect, windows, extension, web, and the
// cgo/js export surfaces -- and the only reference anywhere is an Android
// instrumented acceptance test (androidTest PhysicalLowbarSessionTest.kt).
// `!ios` keeps it for that, and for every desktop host, where no size gate
// applies; it removes it only where the budget is real.
//
// If iOS ever needs heap profiling, the honest way back is a second build tag
// (an `ios && urdebug` file, say) rather than deleting the constraint, so a
// debug build can have it without the shipped extension paying for it.

// WriteHeapProfile forces a complete collection and writes a private Go heap
// profile. The profile contains aggregate allocation stacks, not packet
// payloads, addresses, or destinations. Hosts should keep it in their private
// diagnostics directory: function names and allocation sizes are still
// implementation details. The resulting profile includes both the live
// (in-use) and cumulative allocation sample types understood by go tool pprof.
func WriteHeapProfile(path string) error {
	runtime.GC()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	writeErr := pprof.WriteHeapProfile(file)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}
