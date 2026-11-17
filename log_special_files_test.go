package sdk

// Tests of the files an upload carries beside the glog files (support inbox
// 8308). On windows and linux the app's (the gui's) Go crash file rode in the
// service's zip only under a glog name, so it never did, and the windows
// service's own logs (urnetworkd.log, and go-crash.log, its capture of a Go
// fatal from stderr) and the app's (urnetwork-app.log) sat in the directories
// an upload reads without ever being read. They are carried now, masked, and a
// log the host writes without bound is carried as its newest lines.

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// What a host log or a crash file can hold that must not leave the device as
// written. The addresses and the email are documentation values; the id and
// the user's folders are made up.
const testingUnmaskedValues = "dial 203.0.113.5:443 and [2001:db8::7]:443 for client 01234567-89ab-cdef-0123-456789abcdef " +
	"of someone@example.com in C:\\Users\\someone\\AppData\\Local\\URnetwork\\app\\logs and /home/someone/.local/share/urnetwork\n"

func checkTestingMasked(t *testing.T, entryName string, content string) {
	t.Helper()
	for _, leak := range []string{"203.0.113.5", "2001:db8::7", "01234567-89ab-cdef-0123-456789abcdef", "someone@example.com", "\\someone\\", "/someone/"} {
		if strings.Contains(content, leak) {
			t.Errorf("the upload has %q with %q unmasked: %q", entryName, leak, content)
		}
	}
	for _, want := range []string{"<addr:", "<id:", "<email>", "C:\\Users\\<user>\\AppData", "/home/<user>/.local"} {
		if !strings.Contains(content, want) {
			t.Errorf("the upload has %q without %q: %q", entryName, want, content)
		}
	}
}

// The app's (the gui's) Go crash file and the windows app's own log ride in
// the service's zip under the app's folder, masked; its glog files ride as
// they are, as before.
func TestUploadLogsCarryAnotherProcessesCrashFileAndHostLog(t *testing.T) {
	restoreTestingLogDir(t)

	if err := SetLogDir(t.TempDir()); err != nil {
		t.Fatalf("SetLogDir: %v", err)
	}
	appDir := t.TempDir()
	modTime := time.Now().Add(-time.Hour)
	glogName := "URnetwork.host.user.log.INFO.20260901-000000.111"
	glogPath := writeTestingUploadLogFile(t, appDir, glogName, "app line 203.0.113.9\n", modTime)
	crashPath := writeTestingUploadLogFile(t, appDir, "go-crash.txt", "panic: test\n\ngoroutine 1 [running]:\n"+testingUnmaskedValues, modTime)
	hostLogPath := writeTestingUploadLogFile(t, appDir, "urnetwork-app.log", "2026-11-17 02:16:42 [app] INF: started\n"+testingUnmaskedValues, modTime)
	files := openTestingUploadLogsFiles(t, glogPath, crashPath, hostLogPath)

	zipPath, err := zipUploadLogs(uploadLogsMaxByteCount, []*UploadLogsFile{
		{Source: "app", Name: glogName, FileDescriptor: testingFileDescriptor(files[0])},
		{Source: "app", Name: "go-crash.txt", FileDescriptor: testingFileDescriptor(files[1])},
		{Source: "app", Name: "urnetwork-app.log", FileDescriptor: testingFileDescriptor(files[2])},
	}, connect.DefaultLogger())
	if err != nil {
		t.Fatalf("zipUploadLogs = %v", err)
	}
	zipBytes, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	entryNameContents := readTestingZip(t, zipBytes)
	if content := entryNameContents["app/"+glogName]; content != "app line 203.0.113.9\n" {
		t.Errorf("the upload has %q for the app's glog file, want it as written (entries %v)", content, testingZipEntryNames(entryNameContents))
	}
	for _, name := range []string{"app/go-crash.txt", "app/urnetwork-app.log"} {
		content, ok := entryNameContents[name]
		if !ok {
			t.Fatalf("the upload left out %q (entries %v)", name, testingZipEntryNames(entryNameContents))
		}
		checkTestingMasked(t, name, content)
	}
	if !strings.Contains(entryNameContents["app/go-crash.txt"], "panic: test\n") {
		t.Errorf("the app's crash file lost its panic: %q", entryNameContents["app/go-crash.txt"])
	}
}

// The windows service's own logs, in the directory its glog writes, ride in
// its zip, masked, and its capture of a Go fatal is planned first.
func TestUploadLogsCarryTheServicesOwnHostLogs(t *testing.T) {
	restoreTestingLogDir(t)

	serviceDir := t.TempDir()
	modTime := time.Now().Add(-time.Hour)
	glogName := "urnetworkd.exe.HOST.SYSTEM.log.INFO.20260901-000000.909"
	writeTestingUploadLogFile(t, serviceDir, glogName, "service line\n", modTime)
	writeTestingUploadLogFile(t, serviceDir, "urnetworkd.log", "2026-11-17 02:10:19 [urnetworkd] INF: tunnel: starting\n"+testingUnmaskedValues, modTime)
	// older than everything, as a crash capture is
	writeTestingUploadLogFile(t, serviceDir, "go-crash.log", "fatal error: test\n"+testingUnmaskedValues, modTime.Add(-time.Hour))
	if err := SetLogDir(serviceDir); err != nil {
		t.Fatalf("SetLogDir: %v", err)
	}

	inventory := UploadLogsInventory()
	if inventory.Len() == 0 || inventory.Get(0).Name != "go-crash.log" {
		names := []string{}
		for i := 0; i < inventory.Len(); i += 1 {
			names = append(names, inventory.Get(i).Name)
		}
		t.Fatalf("the upload does not plan the service's crash capture first: %v", names)
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
	if entryNameContents[glogName] != "service line\n" {
		t.Errorf("the upload has %q for the service's glog file (entries %v)", entryNameContents[glogName], testingZipEntryNames(entryNameContents))
	}
	for _, name := range []string{"urnetworkd.log", "go-crash.log"} {
		content, ok := entryNameContents[name]
		if !ok {
			t.Fatalf("the upload left out %q (entries %v)", name, testingZipEntryNames(entryNameContents))
		}
		checkTestingMasked(t, name, content)
	}
}

// A host log has no rotation, so the upload carries its newest whole lines
// within the host log bound: the last line, never the first, and no part of a
// line.
func TestUploadLogsCarryTheNewestLinesOfAHostLog(t *testing.T) {
	restoreTestingLogDir(t)

	serviceDir := t.TempDir()
	line := "2026-11-17 02:10:19 [urnetworkd] DBG: egress: unchanged (v4=4 v6=4)\n"
	content := "first line\n" + strings.Repeat(line, (9*1024*1024)/len(line)) + "last line\n"
	writeTestingUploadLogFile(t, serviceDir, "urnetworkd.log", content, time.Now().Add(-time.Hour))
	if err := SetLogDir(serviceDir); err != nil {
		t.Fatalf("SetLogDir: %v", err)
	}

	zipPath, err := zipUploadLogs(uploadLogsMaxByteCount, nil, connect.DefaultLogger())
	if err != nil {
		t.Fatalf("zipUploadLogs = %v", err)
	}
	zipBytes, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	carried, ok := readTestingZip(t, zipBytes)["urnetworkd.log"]
	if !ok {
		t.Fatal("the upload left out the service's log")
	}
	if 8*1024*1024 < len(carried) {
		t.Fatalf("the upload carries %d bytes of the service's log, over its 8 MiB bound", len(carried))
	}
	if !strings.HasSuffix(carried, "last line\n") || strings.Contains(carried, "first line") || !strings.HasPrefix(carried, line) {
		t.Fatalf("the upload does not carry the log's newest whole lines: starts %q, ends %q", carried[:min(len(carried), 80)], carried[max(0, len(carried)-80):])
	}
}

// The diagnostic export carries the same files, masked whether or not the
// export is redacted: a raw export keeps the glog files as written, and gains
// no unmasked host log.
func TestExportCarriesTheHostLogsMasked(t *testing.T) {
	restoreTestingLogDir(t)

	serviceDir := t.TempDir()
	modTime := time.Now().Add(-time.Hour)
	glogName := "urnetworkd.exe.HOST.SYSTEM.log.INFO.20260901-000000.909"
	writeTestingUploadLogFile(t, serviceDir, glogName, "service line 203.0.113.9\n", modTime)
	writeTestingUploadLogFile(t, serviceDir, "urnetworkd.log", testingUnmaskedValues, modTime)
	if err := SetLogDir(serviceDir); err != nil {
		t.Fatalf("SetLogDir: %v", err)
	}

	exportPath := filepath.Join(t.TempDir(), "export.zip")
	if _, err := ExportDiagnosticBundle(exportPath, NewExportOptions()); err != nil {
		t.Fatalf("ExportDiagnosticBundle = %v", err)
	}
	zipReader, err := zip.OpenReader(exportPath)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer zipReader.Close()
	entryNameContents := map[string]string{}
	for _, file := range zipReader.File {
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		var buffer bytes.Buffer
		if _, err := io.Copy(&buffer, reader); err != nil {
			t.Fatal(err)
		}
		reader.Close()
		entryNameContents[file.Name] = buffer.String()
	}
	var hostLog, glog string
	hostLogFound := false
	for name, content := range entryNameContents {
		switch filepath.Base(name) {
		case "urnetworkd.log":
			hostLog = content
			hostLogFound = true
		case glogName:
			glog = content
		}
	}
	if !hostLogFound {
		t.Fatalf("the export left out the service's log (entries %v)", testingZipEntryNames(entryNameContents))
	}
	checkTestingMasked(t, "urnetworkd.log", hostLog)
	if glog != "service line 203.0.113.9\n" {
		t.Errorf("the raw export changed a glog file: %q", glog)
	}
}

// The carried section of a bounded file starts on a line: the window's first
// line when the window starts on one, else the next.
func TestLogSpecialFileSectionStartsOnALine(t *testing.T) {
	content := "aaaa\nbbbb\ncccc\n"
	for _, c := range []struct {
		carriedByteCount int64
		want             string
	}{
		{carriedByteCount: 15, want: content},
		{carriedByteCount: 99, want: content},
		// starts on the line "bbbb"
		{carriedByteCount: 10, want: "bbbb\ncccc\n"},
		// starts inside "bbbb", so that line goes
		{carriedByteCount: 8, want: "cccc\n"},
		{carriedByteCount: 3, want: ""},
	} {
		section, err := logSpecialFileSection(strings.NewReader(content), int64(len(content)), c.carriedByteCount)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(section)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != c.want {
			t.Errorf("section of %d bytes = %q, want %q", c.carriedByteCount, got, c.want)
		}
	}
}

// Masking can lengthen a line (an address becomes a token), and a masked
// entry never grows past the bytes planned for it: the lines that would not
// fit are left out whole.
func TestMaskedEntryStaysWithinItsPlannedBytes(t *testing.T) {
	var buffer bytes.Buffer
	zipWriter := zip.NewWriter(&buffer)
	fi, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Repeat("1.2.3.4\n", 10)
	grow := func(line string) string { return strings.Replace(line, "1.2.3.4", "<addr:0123456789ab>", 1) }
	if err := zipWriteMaskedEntry(zipWriter, "masked.log", strings.NewReader(lines), fi, grow, int64(len(lines))); err != nil {
		t.Fatal(err)
	}
	if err := zipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	got := readTestingZip(t, buffer.Bytes())["masked.log"]
	if int64(len(lines)) < int64(len(got)) || !strings.HasSuffix(got, "\n") || got == "" {
		t.Fatalf("the masked entry is %d bytes for %d planned: %q", len(got), len(lines), got)
	}
}
