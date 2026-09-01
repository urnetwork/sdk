package sdk

import (
	"os"
	"testing"

	"github.com/urnetwork/glog"
)

// TestGetLogDirReturnsTheDirectoryGlogWritesTo pins the readback contract.
// glog.SetLogDir mutates only glog's internal logDirs/dirSet and never the
// log_dir flag, and SetLogDir stopped setting that flag in 9f41a00, so
// GetLogDir returned "" in every process -- including the one that had just
// called SetLogDir. That silently broke DeviceLocal.UploadLogs, which does
// os.ReadDir(GetLogDir()) and fails on the empty path before it can attach
// anything to the feedback.
func TestGetLogDirReturnsTheDirectoryGlogWritesTo(t *testing.T) {
	restoreTestingLogDir(t)

	dir := t.TempDir()

	if err := SetLogDir(dir); err != nil {
		t.Fatalf("SetLogDir(%q) = %v, want nil", dir, err)
	}

	if got := GetLogDir(); got != dir {
		t.Fatalf("GetLogDir() = %q, want %q", got, dir)
	}

	// the actual failure mode: what every caller does with the result
	entries, err := os.ReadDir(GetLogDir())
	if err != nil {
		t.Fatalf("os.ReadDir(GetLogDir()) = %v, want nil", err)
	}
	if len(entries) == 0 {
		t.Fatal("os.ReadDir(GetLogDir()) found no files, but glog wrote its INFO file there")
	}
}

// restoreTestingLogDir puts the process-global log destination back after the
// test. SetLogDir points glog at a t.TempDir that is deleted on exit, so
// without this every later test in the package logs into a directory that no
// longer exists.
func restoreTestingLogDir(t *testing.T) {
	t.Helper()
	dir := GetLogDir()
	t.Cleanup(func() {
		currentLogDirMu.Lock()
		defer currentLogDirMu.Unlock()
		currentLogDir = dir
		if dir != "" {
			glog.SetLogDir(dir)
		}
	})
}
