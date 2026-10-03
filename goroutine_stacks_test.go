// Deterministic tests for the Android goroutine-stack writer's shared core.
package sdk

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Stays parked until its stack has been checked.
//
//go:noinline
func goroutineStackCaptureFixture(started chan<- struct{}, release <-chan struct{}) {
	close(started)
	<-release
}

// Pins both the all=true runtime snapshot and the private-file contract used by
// Android acceptance.
func TestWriteGoroutineStacksCapturesAllGoroutinesPrivately(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	go goroutineStackCaptureFixture(started, release)
	<-started
	t.Cleanup(func() { close(release) })

	path := filepath.Join(t.TempDir(), "goroutines.txt")
	if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatalf("write stale destination: %v", err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatalf("broaden stale destination permissions: %v", err)
	}

	if err := writeGoroutineStacks(path); err != nil {
		t.Fatalf("writeGoroutineStacks: %v", err)
	}
	stackBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read stacks: %v", err)
	}
	stacks := string(stackBytes)
	if !strings.HasPrefix(stacks, "goroutine ") {
		t.Fatalf("stack snapshot has no goroutine header: %q", stacks)
	}
	if !strings.Contains(stacks, "goroutineStackCaptureFixture") {
		t.Fatal("stack snapshot omitted the explicitly parked goroutine")
	}
	if strings.Contains(stacks, "stale") {
		t.Fatal("stack snapshot did not replace the old destination")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat stacks: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("stack mode = %04o, want 0600", got)
	}
}

// Pins failures instead of letting a gomobile caller believe evidence was
// retained.
func TestWriteGoroutineStacksReportsDestinationError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "goroutines.txt")
	err := writeGoroutineStacks(path)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("writeGoroutineStacks error = %v, want os.ErrNotExist", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed capture left a destination: %v", statErr)
	}
}
