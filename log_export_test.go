//go:build !ios_extension

package sdk

// Tests of a person's own logs in the app (log_export.go): the export holds
// what the upload sends, and nothing is left half written; the tails are the
// last whole lines of each process's newest INFO log.

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"
)

// The file names in dir, sorted.
func testingDirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	return names
}

// The inventory of the process directories under root, as the export and the
// tails read it.
func testingLogFileInfos(t *testing.T, root string, sources ...string) []*LogFileInfo {
	t.Helper()
	inventory := NewLogFileInfoList()
	for _, source := range sources {
		if err := appendLogFilesIn(inventory, filepath.Join(root, source), source); err != nil {
			t.Fatalf("appendLogFilesIn(%q): %v", source, err)
		}
	}
	return inventory.getAll()
}

// The tail of source in logTails, or nil.
func testingLogTailOf(logTails *LogTailList, source string) *LogTail {
	for _, logTail := range logTails.getAll() {
		if logTail.Source == source {
			return logTail
		}
	}
	return nil
}

// Export logs sends on what "send feedback with logs" uploads: the zip has the
// files of every process under the log root under the upload's names, and an
// earlier export at the path is replaced whole.
func TestExportUploadLogsHoldWhatTheUploadSends(t *testing.T) {
	restoreTestingLogDir(t)

	root := t.TempDir()
	modTime := time.Now().Add(-time.Hour)
	appName := "urnetwork.host.user.log.INFO.20260901-000000.101"
	extensionName := "urnetwork.host.user.log.INFO.20260901-000000.202"
	extensionErrorName := "urnetwork.host.user.log.ERROR.20260901-000000.202"
	writeTestingUploadLogFile(t, filepath.Join(root, "app"), appName, "app line\n", modTime)
	writeTestingUploadLogFile(t, filepath.Join(root, "extension"), extensionName, "extension line\n", modTime)
	writeTestingUploadLogFile(t, filepath.Join(root, "extension"), extensionErrorName, "extension error line\n", modTime)
	// what else sits in the log directories is not sent
	writeTestingUploadLogFile(t, filepath.Join(root, "extension"), "notes.txt", "not a log\n", modTime)

	// this process is the app, the one that exports
	if err := SetLogDirForProcess(root, "app"); err != nil {
		t.Fatalf("SetLogDirForProcess: %v", err)
	}

	exportDir := t.TempDir()
	destPath := filepath.Join(exportDir, "urnetwork-logs.zip")
	if err := os.WriteFile(destPath, []byte("an earlier export"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	result, err := ExportUploadLogs(destPath)
	if err != nil {
		t.Fatalf("ExportUploadLogs = %v", err)
	}
	zipBytes, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	entryNameContents := readTestingZip(t, zipBytes)

	wantEntryNameContents := map[string]string{
		"app/" + appName:                  "app line\n",
		"extension/" + extensionName:      "extension line\n",
		"extension/" + extensionErrorName: "extension error line\n",
	}
	for entryName, wantContent := range wantEntryNameContents {
		if entryNameContents[entryName] != wantContent {
			t.Errorf("the export has %q = %q, want %q (entries %v)", entryName, entryNameContents[entryName], wantContent, testingZipEntryNames(entryNameContents))
		}
	}

	// the upload from this process holds the same files, under the same names
	uploadZipPath, err := zipUploadLogs(uploadLogsMaxByteCount, nil, connect.DefaultLogger())
	if err != nil {
		t.Fatalf("zipUploadLogs = %v", err)
	}
	uploadZipBytes, err := os.ReadFile(uploadZipPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	uploadEntryNames := testingZipEntryNames(readTestingZip(t, uploadZipBytes))
	if entryNames := testingZipEntryNames(entryNameContents); !slices.Equal(entryNames, uploadEntryNames) {
		t.Errorf("the export has %v, the upload %v", entryNames, uploadEntryNames)
	}

	if result.FileCount != len(entryNameContents) {
		t.Errorf("FileCount = %d, the zip has %d files", result.FileCount, len(entryNameContents))
	}
	if result.ByteCount != int64(len(zipBytes)) {
		t.Errorf("ByteCount = %d, the zip is %d bytes", result.ByteCount, len(zipBytes))
	}
	// the zip was written beside the path and renamed onto it
	if names := testingDirNames(t, exportDir); !slices.Equal(names, []string{"urnetwork-logs.zip"}) {
		t.Errorf("the export directory has %v, want only the export", names)
	}
}

// The newest lines of this process are still in glog's buffer when the person
// taps Export logs. The export flushes them first, as the upload does. glog
// also flushes on its own every 30 seconds, which could put one round's line on
// disk, but not both rounds' lines.
func TestExportUploadLogsFlushThisProcessFirst(t *testing.T) {
	restoreTestingLogDir(t)

	root := t.TempDir()
	if err := SetLogDirForProcess(root, "app"); err != nil {
		t.Fatalf("SetLogDirForProcess: %v", err)
	}
	destPath := filepath.Join(t.TempDir(), "urnetwork-logs.zip")

	for round := 0; round < 2; round += 1 {
		marker := "app line buffered before the export " + NewId().String()
		glog.Info(marker)

		if _, err := ExportUploadLogs(destPath); err != nil {
			t.Fatalf("ExportUploadLogs = %v", err)
		}
		zipBytes, err := os.ReadFile(destPath)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		exported := false
		for entryName, content := range readTestingZip(t, zipBytes) {
			if strings.HasPrefix(entryName, "app/") && strings.Contains(content, marker) {
				exported = true
			}
		}
		if !exported {
			t.Fatalf("round %d: the export lacks this process's buffered line", round)
		}
	}
}

// With no log file to send, there is nothing to share: no file is written, and
// an earlier export at the path is left as it was.
func TestExportUploadLogsWithNothingToSendWriteNothing(t *testing.T) {
	restoreTestingLogDir(t)

	root := t.TempDir()
	writeTestingUploadLogFile(t, filepath.Join(root, "extension"), "urnetwork.host.user.log.INFO.20260901-000000.202", "extension line\n", time.Now().Add(-time.Hour))
	if err := SetLogDirForProcess(root, "app"); err != nil {
		t.Fatalf("SetLogDirForProcess: %v", err)
	}

	exportDir := t.TempDir()
	destPath := filepath.Join(exportDir, "urnetwork-logs.zip")
	if err := os.WriteFile(destPath, []byte("an earlier export"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// a cap no file fits: the plan sends nothing
	result, err := exportUploadLogs(destPath, 0, connect.DefaultLogger())
	if err != nil {
		t.Fatalf("exportUploadLogs = %v", err)
	}
	if result.FileCount != 0 || result.ByteCount != 0 {
		t.Errorf("the export of nothing = %d files, %d bytes, want none", result.FileCount, result.ByteCount)
	}
	content, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(content) != "an earlier export" {
		t.Errorf("the path holds %q, want the earlier export as it was", content)
	}
	if names := testingDirNames(t, exportDir); !slices.Equal(names, []string{"urnetwork-logs.zip"}) {
		t.Errorf("the export directory has %v, want only the earlier export", names)
	}
}

// A path the export cannot write fails the export, and leaves no file behind.
func TestExportUploadLogsToAMissingDirectoryFail(t *testing.T) {
	restoreTestingLogDir(t)

	root := t.TempDir()
	writeTestingUploadLogFile(t, filepath.Join(root, "app"), "urnetwork.host.user.log.INFO.20260901-000000.101", "app line\n", time.Now().Add(-time.Hour))
	if err := SetLogDirForProcess(root, "app"); err != nil {
		t.Fatalf("SetLogDirForProcess: %v", err)
	}

	exportDir := t.TempDir()
	if _, err := ExportUploadLogs(filepath.Join(exportDir, "missing", "urnetwork-logs.zip")); err == nil {
		t.Errorf("ExportUploadLogs into a missing directory = nil, want an error")
	}
	if _, err := ExportUploadLogs(""); err == nil {
		t.Errorf("ExportUploadLogs(\"\") = nil, want an error")
	}
	if names := testingDirNames(t, exportDir); len(names) != 0 {
		t.Errorf("the export directory has %v, want nothing", names)
	}
}

// A process's latest lines are in its newest INFO file: glog writes every
// line there whatever its severity, and starts a new file when one fills. The
// severity files, the exit diagnostics and the older files are not tailed.
func TestLogTailsHoldTheNewestInfoFileOfEachProcess(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	appOldName := "urnetwork.host.user.log.INFO.20260901-000000.101"
	appNewName := "urnetwork.host.user.log.INFO.20260902-000000.102"
	extensionName := "urnetwork.host.user.log.INFO.20260902-000000.202"
	writeTestingUploadLogFile(t, filepath.Join(root, "app"), appOldName, "old line\n", now.Add(-2*time.Hour))
	writeTestingUploadLogFile(t, filepath.Join(root, "app"), appNewName, "line 1\nline 2\nline 3\nline 4\n", now.Add(-time.Hour))
	// written later than the INFO file, and not where the latest lines are
	writeTestingUploadLogFile(t, filepath.Join(root, "app"), "urnetwork.host.user.log.ERROR.20260902-000000.102", "error line\n", now)
	writeTestingUploadLogFile(t, filepath.Join(root, "app"), exitDiagnosticsFileName, "exit record\n", now)
	writeTestingUploadLogFile(t, filepath.Join(root, "extension"), extensionName, "extension line\n", now.Add(-time.Hour))

	settings := &logTailSettings{
		MaxLineCount: 3,
		MaxByteCount: 1024,
	}
	logTails := logTailsOf(testingLogFileInfos(t, root, "app", "extension"), settings, connect.DefaultLogger())

	type testingTail struct {
		source    string
		name      string
		lines     []string
		truncated bool
	}
	want := []testingTail{
		{source: "app", name: appNewName, lines: []string{"line 2", "line 3", "line 4"}, truncated: true},
		{source: "extension", name: extensionName, lines: []string{"extension line"}, truncated: false},
	}
	got := []testingTail{}
	for _, logTail := range logTails.getAll() {
		got = append(got, testingTail{
			source:    logTail.Source,
			name:      logTail.Name,
			lines:     logTail.Lines.getAll(),
			truncated: logTail.Truncated,
		})
	}
	if !slices.EqualFunc(got, want, func(a testingTail, b testingTail) bool {
		return a.source == b.source && a.name == b.name && slices.Equal(a.lines, b.lines) && a.truncated == b.truncated
	}) {
		t.Fatalf("tails = %+v, want %+v", got, want)
	}
}

// Of two files written at the same time, the later name is the newer: glog
// names a file for the time it started.
func TestLogTailsOfFilesWrittenAtOnceTakeTheLaterName(t *testing.T) {
	root := t.TempDir()
	modTime := time.Now().Add(-time.Hour)
	newName := "urnetwork.host.user.log.INFO.20260902-000000.102"
	writeTestingUploadLogFile(t, filepath.Join(root, "app"), newName, "new line\n", modTime)
	writeTestingUploadLogFile(t, filepath.Join(root, "app"), "urnetwork.host.user.log.INFO.20260901-000000.101", "old line\n", modTime)

	logTails := logTailsOf(testingLogFileInfos(t, root, "app"), defaultLogTailSettings(), connect.DefaultLogger())
	logTail := testingLogTailOf(logTails, "app")
	if logTail == nil || logTail.Name != newName || !slices.Equal(logTail.Lines.getAll(), []string{"new line"}) {
		t.Fatalf("tail = %+v, want the lines of %q", logTail, newName)
	}
}

// A tail reads only the end of a large file, and keeps only whole lines: a
// line the read starts inside is left out, and a read that starts just after a
// line end keeps the line it starts.
func TestLogTailsReadOnlyTheEndOfAFile(t *testing.T) {
	// 11 + 12 + 6 bytes
	content := "first line\nsecond line\nthird\n"
	type testingCase struct {
		maxByteCount  int64
		wantLines     []string
		wantTruncated bool
	}
	cases := []testingCase{
		// the whole file
		{maxByteCount: 29, wantLines: []string{"first line", "second line", "third"}, wantTruncated: false},
		{maxByteCount: 1024, wantLines: []string{"first line", "second line", "third"}, wantTruncated: false},
		// starts just after the first line's end
		{maxByteCount: 18, wantLines: []string{"second line", "third"}, wantTruncated: true},
		// starts inside the second line
		{maxByteCount: 17, wantLines: []string{"third"}, wantTruncated: true},
		// starts just after the second line's end
		{maxByteCount: 6, wantLines: []string{"third"}, wantTruncated: true},
		// starts inside the last line
		{maxByteCount: 3, wantLines: []string{}, wantTruncated: true},
		// reads nothing
		{maxByteCount: 0, wantLines: []string{}, wantTruncated: true},
	}
	root := t.TempDir()
	writeTestingUploadLogFile(t, filepath.Join(root, "app"), "urnetwork.host.user.log.INFO.20260901-000000.101", content, time.Now().Add(-time.Hour))
	logFileInfos := testingLogFileInfos(t, root, "app")
	for _, c := range cases {
		settings := &logTailSettings{
			MaxLineCount: 100,
			MaxByteCount: c.maxByteCount,
		}
		logTail := testingLogTailOf(logTailsOf(logFileInfos, settings, connect.DefaultLogger()), "app")
		if logTail == nil {
			t.Errorf("max %d bytes: no tail", c.maxByteCount)
			continue
		}
		if lines := logTail.Lines.getAll(); !slices.Equal(lines, c.wantLines) || logTail.Truncated != c.wantTruncated {
			t.Errorf("max %d bytes: lines %q truncated %t, want %q truncated %t", c.maxByteCount, lines, logTail.Truncated, c.wantLines, c.wantTruncated)
		}
	}
}

// The last line of a live file may not have its line end yet. It is kept.
func TestLogTailsKeepALastLineWithoutItsEnd(t *testing.T) {
	root := t.TempDir()
	writeTestingUploadLogFile(t, filepath.Join(root, "app"), "urnetwork.host.user.log.INFO.20260901-000000.101", "whole line\nline being written", time.Now().Add(-time.Hour))

	logTail := testingLogTailOf(logTailsOf(testingLogFileInfos(t, root, "app"), defaultLogTailSettings(), connect.DefaultLogger()), "app")
	if logTail == nil || !slices.Equal(logTail.Lines.getAll(), []string{"whole line", "line being written"}) || logTail.Truncated {
		t.Fatalf("tail = %+v, want both lines, not truncated", logTail)
	}
}

// gomobile hands each line to java and swift as utf-8, and a log line may hold
// any bytes. A tail's lines are valid utf-8 without their line ends, so a
// carriage return is dropped too.
func TestLogTailsMakeEveryLineValidUtf8(t *testing.T) {
	root := t.TempDir()
	writeTestingUploadLogFile(t, filepath.Join(root, "app"), "urnetwork.host.user.log.INFO.20260901-000000.101", "crlf line\r\nbytes \xff\xfe in a line\n", time.Now().Add(-time.Hour))

	logTail := testingLogTailOf(logTailsOf(testingLogFileInfos(t, root, "app"), defaultLogTailSettings(), connect.DefaultLogger()), "app")
	if logTail == nil {
		t.Fatal("no tail")
	}
	want := []string{"crlf line", "bytes � in a line"}
	if lines := logTail.Lines.getAll(); !slices.Equal(lines, want) {
		t.Fatalf("lines = %q, want %q", lines, want)
	}
}

// A file that is gone by the time it is read (a process pruned it) is left out,
// and the other processes' tails are still read.
func TestLogTailsLeaveOutAFileThatIsGone(t *testing.T) {
	root := t.TempDir()
	modTime := time.Now().Add(-time.Hour)
	appPath := writeTestingUploadLogFile(t, filepath.Join(root, "app"), "urnetwork.host.user.log.INFO.20260901-000000.101", "app line\n", modTime)
	writeTestingUploadLogFile(t, filepath.Join(root, "extension"), "urnetwork.host.user.log.INFO.20260901-000000.202", "extension line\n", modTime)
	logFileInfos := testingLogFileInfos(t, root, "app", "extension")
	if err := os.Remove(appPath); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	logTails := logTailsOf(logFileInfos, defaultLogTailSettings(), connect.DefaultLogger())
	if logTails.Len() != 1 || testingLogTailOf(logTails, "extension") == nil {
		t.Fatalf("tails = %+v, want only the extension's", logTails.getAll())
	}
}

// The public tails read every process under the log root, this one's newest
// lines included: they flush this process's glog first. glog also flushes on
// its own every 30 seconds, which could put one round's line on disk, but not
// both rounds' lines.
func TestLogTailsReadEveryProcessUnderTheLogRoot(t *testing.T) {
	restoreTestingLogDir(t)

	root := t.TempDir()
	extensionName := "urnetwork.host.user.log.INFO.20260901-000000.202"
	writeTestingUploadLogFile(t, filepath.Join(root, "extension"), extensionName, "extension line\n", time.Now().Add(-time.Hour))
	if err := SetLogDirForProcess(root, "app"); err != nil {
		t.Fatalf("SetLogDirForProcess: %v", err)
	}

	for round := 0; round < 2; round += 1 {
		marker := "app line buffered before the tail " + NewId().String()
		glog.Info(marker)

		logTails := LogTails()
		extensionTail := testingLogTailOf(logTails, "extension")
		if extensionTail == nil || extensionTail.Name != extensionName || !slices.Equal(extensionTail.Lines.getAll(), []string{"extension line"}) {
			t.Fatalf("round %d: the extension's tail = %+v, want its line", round, extensionTail)
		}
		appTail := testingLogTailOf(logTails, "app")
		if appTail == nil {
			t.Fatalf("round %d: no tail of this process", round)
		}
		appLines := appTail.Lines.getAll()
		if !slices.ContainsFunc(appLines, func(line string) bool { return strings.HasSuffix(line, marker) }) {
			t.Fatalf("round %d: this process's tail lacks its buffered line: %q", round, appLines)
		}
	}
}
