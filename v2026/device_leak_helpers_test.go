package sdk

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// captureGoroutineStacks returns a map of goroutine stack signature -> count. The
// signature is the ordered list of function frames (args, addresses, file:line,
// and the varying "in goroutine N" suffix stripped), so many goroutines running
// the same code collapse to one entry. Diffing two snapshots finds a leaked
// goroutine by its identity even when the raw goroutine count coincidentally
// matches the baseline.
func captureGoroutineStacks() map[string]int {
	var buf []byte
	for size := 1 << 20; ; size *= 2 {
		buf = make([]byte, size)
		if n := runtime.Stack(buf, true); n < size {
			buf = buf[:n]
			break
		}
	}
	sigs := map[string]int{}
	for _, block := range strings.Split(string(buf), "\n\n") {
		lines := strings.Split(strings.TrimSpace(block), "\n")
		if len(lines) < 2 {
			continue
		}
		var frames []string
		for _, ln := range lines[1:] { // skip the "goroutine N [state]:" header
			if strings.HasPrefix(ln, "\t") {
				continue // "\t/path/file.go:123 +0x1a" location line
			}
			if i := strings.Index(ln, " in goroutine "); i >= 0 {
				ln = ln[:i] // "created by X in goroutine N" -> "created by X"
			}
			// strip the argument list but keep pointer receivers:
			// "pkg.(*Type).method(0x1, 0x2)" -> "pkg.(*Type).method". The arg-list
			// paren is the last '(' and always follows the last '.'; the receiver's
			// "(*Type)" paren precedes it, so a created-by line without an arg list
			// is left intact.
			if i := strings.LastIndexByte(ln, '('); i >= 0 && strings.LastIndexByte(ln, '.') < i {
				ln = ln[:i]
			}
			frames = append(frames, strings.TrimSpace(ln))
		}
		sigs[strings.Join(frames, "\n")]++
	}
	return sigs
}

// isRuntimeSignature reports whether every frame is runtime-internal (GC workers,
// scavenger, finalizers, etc.), whose counts legitimately vary run to run and are
// not application leaks.
func isRuntimeSignature(sig string) bool {
	for _, f := range strings.Split(sig, "\n") {
		if f == "" {
			continue
		}
		if !strings.HasPrefix(f, "runtime.") && !strings.HasPrefix(f, "runtime/") &&
			!strings.HasPrefix(f, "created by runtime.") {
			return false
		}
	}
	return true
}

// reportGoroutineLeaks fails the test for every non-runtime goroutine signature
// whose count grew from before to after by more than tolerance, printing the top
// frame so a leak is attributable, not just a number.
func reportGoroutineLeaks(t *testing.T, before, after map[string]int, tolerance int) {
	t.Helper()
	var leaks []string
	for sig, n := range after {
		if n-before[sig] > tolerance && !isRuntimeSignature(sig) {
			top := sig
			if i := strings.IndexByte(sig, '\n'); i >= 0 {
				top = sig[:i]
			}
			leaks = append(leaks, fmt.Sprintf("+%d %s", n-before[sig], top))
		}
	}
	if len(leaks) > 0 {
		sort.Strings(leaks)
		t.Errorf("goroutine stacks grew from baseline (potential leak):\n  %s", strings.Join(leaks, "\n  "))
	}
}

// poolOutstanding returns the number of message-pool buffers currently held by
// consumers (taken - returned). It returns to a stable baseline when every taken
// buffer is eventually returned, so growth across a load-then-teardown cycle is a
// lost MessagePoolReturn — a leak the heap checks cannot see, because the GC
// quietly collects lost buffers while the pool's reuse rate silently degrades.
func poolOutstanding() int64 {
	taken, returned, _ := connect.MessagePoolCounts()
	return int64(taken) - int64(returned)
}

// reportPoolLeaks fails the test when outstanding pool buffers grew from the
// baseline by more than tolerance after a full teardown.
func reportPoolLeaks(t *testing.T, before int64, tolerance int64) {
	t.Helper()
	after := poolOutstanding()
	if after-before > tolerance {
		t.Errorf("message pool buffers not returned: outstanding %d -> %d (+%d, tol +%d)",
			before, after, after-before, tolerance)
	}
}

// waitTimeout waits for wg up to d. false means the group did not finish — the
// caller reports a deadlock/stuck-goroutine failure instead of hanging the test.
func waitTimeout(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// openFdCount returns the number of open file descriptors for this process, or -1
// if it can't be determined on this platform. A socket/file-descriptor leak (e.g.
// an egress socket not closed) moves this without moving heap or goroutine counts.
func openFdCount() int {
	for _, dir := range []string{"/proc/self/fd", "/dev/fd"} {
		f, err := os.Open(dir)
		if err != nil {
			continue
		}
		// Readdirnames lists entries without stat'ing each one; ReadDir would
		// Lstat every fd and fail when a socket fd races closed under it.
		names, err := f.Readdirnames(-1)
		f.Close()
		if err == nil {
			return len(names)
		}
	}
	return -1
}
