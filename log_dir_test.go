package sdk

import (
	"os"
	"path/filepath"
	"strings"
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

// TestSetLogDirForProcessScopesRetentionPerProcess is the reason per-process
// subdirectories exist. clearOldLogs keeps the 4 newest files in whatever
// directory it is handed, so two processes sharing one directory delete each
// other's history. Under a root, each process prunes only its own.
func TestSetLogDirForProcessScopesRetentionPerProcess(t *testing.T) {
	restoreTestingLogDir(t)

	root := t.TempDir()

	if err := SetLogDirForProcess(root, "extension"); err != nil {
		t.Fatalf("SetLogDirForProcess(root, \"extension\") = %v, want nil", err)
	}
	extensionDir := GetLogDir()
	if extensionDir != filepath.Join(root, "extension") {
		t.Fatalf("GetLogDir() = %q, want %q", extensionDir, filepath.Join(root, "extension"))
	}
	if got := GetLogRoot(); got != root {
		t.Fatalf("GetLogRoot() = %q, want %q", got, root)
	}

	// six files in the extension's directory: retention keeps the newest 4
	for i := 0; i < 6; i += 1 {
		writeTestingLogFile(t, extensionDir, "urnetwork.host.user.log.INFO.2026083"+string(rune('0'+i))+"-000000.100")
	}

	// the app's own history must survive the extension's pruning
	if err := SetLogDirForProcess(root, "app"); err != nil {
		t.Fatalf("SetLogDirForProcess(root, \"app\") = %v, want nil", err)
	}
	appDir := GetLogDir()
	writeTestingLogFile(t, appDir, "urnetwork.host.user.log.INFO.20260830-000000.200")
	// pointing glog at appDir already ran this pipeline's own housekeeping
	// (clearOldLogs plus an Infof), and glog opens its file in the newly
	// targeted directory on the next write -- so appDir may already hold a
	// file of its own. Snapshot the count now so the assertion below tests the
	// real intent, that pruning extensionDir must not touch appDir at all,
	// rather than a total that depends on that incidental write.
	appDirCountBeforePrune := countTestingLogFiles(t, appDir)

	// prune the extension's directory again, as its next launch would
	clearOldLogs(extensionDir)

	if got := countTestingLogFiles(t, extensionDir); got > 4 {
		t.Fatalf("extension dir kept %d log files, want at most 4", got)
	}
	if got := countTestingLogFiles(t, appDir); got != appDirCountBeforePrune {
		t.Fatalf("app dir has %d log files, want %d (unchanged) -- the extension's pruning deleted the app's history", got, appDirCountBeforePrune)
	}
}

// TestSetLogDirClearsTheRecordedRoot pins that GetLogRoot never names a root
// that does not contain the directory glog is writing to.
//
// A plain SetLogDir after a SetLogDirForProcess would otherwise leave the
// previous root recorded, and LogInventory enumerates that root: the export
// would list files from per-process directories this process had abandoned
// and miss the one it was actually writing.
func TestSetLogDirClearsTheRecordedRoot(t *testing.T) {
	restoreTestingLogDir(t)

	root := t.TempDir()
	if err := SetLogDirForProcess(root, "app"); err != nil {
		t.Fatalf("SetLogDirForProcess: %v", err)
	}
	if got := GetLogRoot(); got != root {
		t.Fatalf("GetLogRoot() = %q, want %q", got, root)
	}

	legacy := t.TempDir()
	if err := SetLogDir(legacy); err != nil {
		t.Fatalf("SetLogDir: %v", err)
	}

	if got := GetLogDir(); got != legacy {
		t.Fatalf("GetLogDir() = %q, want %q", got, legacy)
	}
	if got := GetLogRoot(); got != "" {
		t.Fatalf("GetLogRoot() = %q after a legacy SetLogDir, want \"\" -- it no longer contains GetLogDir()", got)
	}

	// the inventory must follow glog, not the abandoned root
	writeTestingLogFile(t, legacy, "urnetwork.host.user.log.INFO.20260830-101112.4242")
	inventory := LogInventory()
	found := false
	for i := 0; i < inventory.Len(); i += 1 {
		if filepath.Dir(inventory.Get(i).Path) == legacy {
			found = true
		}
	}
	if !found {
		t.Fatal("LogInventory did not list the directory glog is writing to")
	}
}

func writeTestingLogFile(t *testing.T, dir string, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("I0830 00:00:00.000000 1 x.go:1] test\n"), 0600); err != nil {
		t.Fatalf("WriteFile(%q): %v", name, err)
	}
}

func countTestingLogFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	n := 0
	for _, e := range entries {
		if strings.Contains(e.Name(), ".log.") {
			n += 1
		}
	}
	return n
}

// restoreTestingLogDir puts the process-global log destination back after the
// test. SetLogDir and SetLogDirForProcess point glog at a t.TempDir that is
// deleted on exit, so without this every later test in the package logs into a
// directory that no longer exists.
func restoreTestingLogDir(t *testing.T) {
	t.Helper()
	dir := GetLogDir()
	root := GetLogRoot()
	t.Cleanup(func() {
		currentLogDirMu.Lock()
		defer currentLogDirMu.Unlock()
		currentLogDir = dir
		currentLogRoot = root
		if dir != "" {
			glog.SetLogDir(dir)
		}
	})
}
