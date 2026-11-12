package sdk

// The exit diagnostics file (exit_diagnostics.go) in "send feedback with logs":
// the upload, its inventory and the diagnostic export carry it beside the glog
// files, newer glog files never crowd it out of the upload's cap, and glog's
// retention never removes it. Support 8190: why the app's earlier processes
// ended is only in this file on Apple platforms.
//
// These name the file by its literal name, so they read the same against a
// tree without it.

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

const testingExitDiagnosticsFileName = "exit-diagnostics.txt"

// On ios the app writes the file into Logs/app, and the extension uploads
// while the tunnel is up: its upload, made from the same root, carries the
// app's file beside the glog files, and nothing else that sits beside it.
func TestUploadLogsCarryTheExitDiagnosticsFile(t *testing.T) {
	restoreTestingLogDir(t)

	root := t.TempDir()
	modTime := time.Now().Add(-time.Hour)
	appName := "urnetwork.host.user.log.INFO.20261110-000000.101"
	extensionName := "urnetwork.host.user.log.INFO.20261110-000000.202"
	exitLine := "2026-11-10T13:19:00Z..2026-11-10T13:19:00Z metrickit crash app=2026.11.3 build=4512 signal=SIGSEGV(11)\n"
	writeTestingUploadLogFile(t, filepath.Join(root, "app"), appName, "app line\n", modTime)
	writeTestingUploadLogFile(t, filepath.Join(root, "app"), testingExitDiagnosticsFileName, exitLine, modTime.Add(-24*time.Hour))
	// what is written beside the file is never uploaded: the record of what
	// it holds, and a write in progress
	writeTestingUploadLogFile(t, filepath.Join(root, "app"), "exit-diagnostics-state.json", "{}", modTime)
	writeTestingUploadLogFile(t, filepath.Join(root, "app"), testingExitDiagnosticsFileName+".tmp", "partial", modTime)
	extensionPath := writeTestingUploadLogFile(t, filepath.Join(root, "extension"), extensionName, "extension line\n", modTime)
	// a link under the file's name is never followed
	if err := os.Symlink(extensionPath, filepath.Join(root, "extension", testingExitDiagnosticsFileName)); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	if err := SetLogDirForProcess(root, "extension"); err != nil {
		t.Fatalf("SetLogDirForProcess: %v", err)
	}

	zipPath, err := zipUploadLogs(uploadLogsMaxByteCount, nil, connect.DefaultLogger())
	if err != nil {
		t.Fatalf("zipUploadLogs = %v", err)
	}
	zipBytes, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	entryNameContents := readTestingZip(t, zipBytes)

	exitEntryName := "app/" + testingExitDiagnosticsFileName
	if entryNameContents[exitEntryName] != exitLine {
		t.Errorf("the upload has %q = %q, want %q (entries %v)", exitEntryName, entryNameContents[exitEntryName], exitLine, testingZipEntryNames(entryNameContents))
	}
	for _, entryName := range []string{"app/" + appName, "extension/" + extensionName} {
		if _, ok := entryNameContents[entryName]; !ok {
			t.Errorf("the upload lacks %q (entries %v)", entryName, testingZipEntryNames(entryNameContents))
		}
	}
	for entryName := range entryNameContents {
		if strings.Contains(entryName, "state") || strings.HasSuffix(entryName, ".tmp") || entryName == "extension/"+testingExitDiagnosticsFileName {
			t.Errorf("the upload has %q, which is not a log file", entryName)
		}
	}

	// the inventories list what the upload sends, the file as informational
	for _, inventory := range []*LogFileInfoList{UploadLogsInventory(), LogInventory()} {
		logFileInfos := inventory.getAll()
		if sourceNames := testingUploadLogSourceNames(logFileInfos); !slices.Contains(sourceNames, exitEntryName) {
			t.Errorf("the inventory lacks %q: %v", exitEntryName, sourceNames)
		}
		for _, logFileInfo := range logFileInfos {
			if logFileInfo.Name == testingExitDiagnosticsFileName && logFileInfo.Severity != "INFO" {
				t.Errorf("the inventory gives %s/%s the severity %q, want INFO", logFileInfo.Source, logFileInfo.Name, logFileInfo.Severity)
			}
		}
	}
}

// The upload takes the newest glog files that fit its cap, and the app and
// the extension can fill it. The exit diagnostics file is small and rarely
// written, so it is the oldest file there: it is planned first, and newer
// glog files never crowd it out.
func TestSelectUploadLogFilesPlansTheExitDiagnosticsFileFirst(t *testing.T) {
	logFileInfos := []*LogFileInfo{
		testingUploadLogFileInfo("app", "urnetwork.h.u.log.INFO.a", 500, 5000),
		testingUploadLogFileInfo("extension", "urnetwork.h.u.log.INFO.b", 450, 4000),
		testingUploadLogFileInfo("app", testingExitDiagnosticsFileName, 40, 1),
		testingUploadLogFileInfo("extension", "urnetwork.h.u.log.INFO.c", 30, 3000),
	}

	selectedLogFileInfos := selectUploadLogFiles(logFileInfos, 1000, 0)

	wantSourceNames := []string{
		"app/" + testingExitDiagnosticsFileName,
		"app/urnetwork.h.u.log.INFO.a",
		"extension/urnetwork.h.u.log.INFO.b",
		// c (30) would make 1020
	}
	if sourceNames := testingUploadLogSourceNames(selectedLogFileInfos); !slices.Equal(sourceNames, wantSourceNames) {
		t.Fatalf("selected %v, want %v", sourceNames, wantSourceNames)
	}
}

// Retention keeps the 4 newest glog files of a process directory at each
// start (clearOldLogs). The exit diagnostics file is not a glog file: it
// stays, older than all of them.
func TestLogRetentionKeepsTheExitDiagnosticsFile(t *testing.T) {
	restoreTestingLogDir(t)

	root := t.TempDir()
	appDir := filepath.Join(root, "app")
	modTime := time.Now().Add(-time.Hour)
	exitPath := writeTestingUploadLogFile(t, appDir, testingExitDiagnosticsFileName, "a line\n", modTime.Add(-48*time.Hour))
	oldestName := "urnetwork.host.user.log.INFO.20261110-000000.1"
	for i := range 6 {
		name := "urnetwork.host.user.log.INFO.20261110-00000" + string(rune('0'+i)) + ".1"
		writeTestingUploadLogFile(t, appDir, name, "line\n", modTime.Add(time.Duration(i)*time.Minute))
	}

	if err := SetLogDirForProcess(root, "app"); err != nil {
		t.Fatalf("SetLogDirForProcess: %v", err)
	}

	if _, err := os.Stat(filepath.Join(appDir, oldestName)); err == nil {
		t.Fatalf("retention kept %s, so this start did not prune", oldestName)
	}
	if content, err := os.ReadFile(exitPath); err != nil || string(content) != "a line\n" {
		t.Fatalf("after the start the exit diagnostics file is %q (%v), want it kept", content, err)
	}
}
