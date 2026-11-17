// An Android app's Java or Kotlin crash reached only logcat, and before
// Android 11 the system keeps no exit reason, so "send feedback with logs"
// could not say why the app closed (support inbox 8308, an Android 8.0
// phone). The app's handler now leaves the crash in its log directory, and
// the next start takes it into java-crash.txt, which the upload carries first.
package sdk

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A pending record as the app's handler writes it (android JavaCrashRecorder).
func testJavaCrashPendingRecord(timeMillis int64, thread string, stack string) string {
	return "urnetwork-java-crash 1\n" +
		"time_millis: " + strconv.FormatInt(timeMillis, 10) + "\n" +
		"app_version: 2026.11.17-1101053110\n" +
		"thread: " + thread + "\n" +
		"\n" +
		stack
}

// The record the app left at its crash is taken into java-crash.txt when a
// process next points its logs at the directory, and the pending file goes.
func TestJavaCrashPendingIsTakenWhenLogsStart(t *testing.T) {
	dir := t.TempDir()
	stack := "java.lang.IllegalStateException: test crash\n" +
		"\tat com.example.Test.run(Test.kt:10)\n" +
		"\tat java.lang.Thread.run(Thread.java:764)\n"
	pending := testJavaCrashPendingRecord(1700000000000, "main", stack)
	if err := os.WriteFile(filepath.Join(dir, "java-crash-pending.txt"), []byte(pending), 0600); err != nil {
		t.Fatal(err)
	}
	if err := SetLogDir(dir); err != nil {
		t.Fatal(err)
	}
	crash, err := os.ReadFile(filepath.Join(dir, "java-crash.txt"))
	if err != nil {
		t.Fatalf("no java crash file in the log directory: %v", err)
	}
	for _, want := range []string{
		"--- java crash at 2023-11-14T22:13:20Z\n",
		"app version: 2026.11.17-1101053110\n",
		"thread: main\n",
		"java.lang.IllegalStateException: test crash\n",
		"\tat com.example.Test.run(Test.kt:10)\n",
	} {
		if !strings.Contains(string(crash), want) {
			t.Fatalf("the java crash file lacks %q: %q", want, crash)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "java-crash-pending.txt")); !os.IsNotExist(err) {
		t.Fatalf("the pending record was left behind: %v", err)
	}
}

// A crash record carries no address, id or email: an exception message can
// name the address a connection failed to, and the record is for the code
// path, not the user.
func TestJavaCrashRecordLeavesOutAddressesIdsAndEmails(t *testing.T) {
	dir := t.TempDir()
	stack := "java.net.ConnectException: failed to connect to /203.0.113.7:443 from [2001:db8::1]\n" +
		"\tat com.example.Net.dial(Net.kt:20)\n" +
		"Caused by: java.lang.IllegalArgumentException: no client 01234567-89ab-cdef-0123-456789abcdef for someone@example.com\n" +
		"\t... 3 more\n"
	pending := testJavaCrashPendingRecord(1700000000000, "OkHttp 203.0.113.9", stack)
	if err := os.WriteFile(filepath.Join(dir, "java-crash-pending.txt"), []byte(pending), 0600); err != nil {
		t.Fatal(err)
	}
	if err := SetLogDir(dir); err != nil {
		t.Fatal(err)
	}
	crash, err := os.ReadFile(filepath.Join(dir, "java-crash.txt"))
	if err != nil {
		t.Fatalf("no java crash file in the log directory: %v", err)
	}
	for _, leak := range []string{"203.0.113.7", "203.0.113.9", "2001:db8::1", "01234567-89ab-cdef-0123-456789abcdef", "someone@example.com"} {
		if strings.Contains(string(crash), leak) {
			t.Fatalf("the java crash file keeps %q: %q", leak, crash)
		}
	}
	for _, want := range []string{"java.net.ConnectException: failed to connect to /<addr:", "\tat com.example.Net.dial(Net.kt:20)\n", "<email>"} {
		if !strings.Contains(string(crash), want) {
			t.Fatalf("the java crash file lacks %q: %q", want, crash)
		}
	}
}

// The upload's inventory lists the java crash file beside the glog files, and
// the upload takes it first, as it takes the exit diagnostics and the Go crash
// file: newer logs must never crowd out the file that says why the app died.
func TestUploadLogsCarryTheJavaCrashFile(t *testing.T) {
	dir := t.TempDir()
	glogName := "gojni.localhost.android.log.INFO.20261113-201455.18745"
	if err := os.WriteFile(filepath.Join(dir, glogName), []byte(strings.Repeat("x", 4096)), 0600); err != nil {
		t.Fatal(err)
	}
	crashPath := filepath.Join(dir, "java-crash.txt")
	if err := os.WriteFile(crashPath, []byte("--- java crash at 2023-11-14T22:13:20Z\nthread: main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// the crash file is the oldest file of its directory
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(crashPath, old, old); err != nil {
		t.Fatal(err)
	}

	inventory := NewLogFileInfoList()
	if err := appendLogFilesIn(inventory, dir, "app"); err != nil {
		t.Fatal(err)
	}
	var crash *LogFileInfo
	for _, logFileInfo := range inventory.getAll() {
		if logFileInfo.Name == "java-crash.txt" {
			crash = logFileInfo
		}
	}
	if crash == nil {
		t.Fatalf("the inventory left out the java crash file: %+v", inventory.getAll())
	}
	if crash.Severity != "ERROR" {
		t.Fatalf("the java crash file's severity is %q", crash.Severity)
	}
	// a cap that holds the crash file and not the newer glog file as well
	selected := selectUploadLogFiles(inventory.getAll(), crash.ByteCount+1024, 512)
	if len(selected) != 1 || selected[0].Name != "java-crash.txt" {
		t.Fatalf("the upload did not take the java crash file first: %+v", selected)
	}
}

// The file keeps its newest records within its bound, newest last, and a
// record larger than the bound by itself is cut to it at a line, keeping its
// head. Nothing is left in staging.
func TestJavaCrashFileKeepsTheNewestRecordsWithinItsBound(t *testing.T) {
	dir := t.TempDir()
	settings := &javaCrashSettings{MaxPendingByteCount: 64 * 1024, MaxByteCount: 400}
	stack := func(name string) string {
		return "java.lang.IllegalStateException: " + name + "\n\tat com.example.Test.run(Test.kt:10)\n"
	}
	for i, name := range []string{"first", "second", "third"} {
		pending := testJavaCrashPendingRecord(1700000000000+int64(i)*1000, "main", stack(name))
		if err := os.WriteFile(filepath.Join(dir, javaCrashPendingFileName), []byte(pending), 0600); err != nil {
			t.Fatal(err)
		}
		if err := takeJavaCrashPending(dir, settings); err != nil {
			t.Fatal(err)
		}
	}
	crash, err := os.ReadFile(filepath.Join(dir, javaCrashFileName))
	if err != nil {
		t.Fatal(err)
	}
	if settings.MaxByteCount < int64(len(crash)) {
		t.Fatalf("the java crash file is %d bytes, over its bound %d", len(crash), settings.MaxByteCount)
	}
	records := splitJavaCrashRecords(string(crash))
	if len(records) != 2 || !strings.Contains(records[0], ": second\n") || !strings.Contains(records[1], ": third\n") {
		t.Fatalf("the file did not keep the newest records, newest last: %q", crash)
	}

	// one record over the bound alone
	long := "java.lang.StackOverflowError\n" + strings.Repeat("\tat com.example.Deep.recurse(Deep.kt:7)\n", 40)
	if err := os.WriteFile(filepath.Join(dir, javaCrashPendingFileName), []byte(testJavaCrashPendingRecord(1700000009000, "main", long)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := takeJavaCrashPending(dir, settings); err != nil {
		t.Fatal(err)
	}
	crash, err = os.ReadFile(filepath.Join(dir, javaCrashFileName))
	if err != nil {
		t.Fatal(err)
	}
	if settings.MaxByteCount < int64(len(crash)) || !strings.HasPrefix(string(crash), javaCrashRecordPrefix) ||
		!strings.Contains(string(crash), "java.lang.StackOverflowError\n") || !strings.HasSuffix(string(crash), "\n") {
		t.Fatalf("the oversized record was not cut to its head: %d bytes %q", len(crash), crash)
	}
	staging, err := filepath.Glob(filepath.Join(dir, ".java-crash-*"))
	if err != nil || len(staging) != 0 {
		t.Fatalf("staging files were left behind: %v %v", staging, err)
	}
}

// A pending file the app's handler did not write is removed, so it is never
// read again, and the java crash file is left as it was.
func TestJavaCrashPendingThatIsNoRecordIsRemoved(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, javaCrashFileName), []byte(javaCrashRecordPrefix+"2023-11-14T22:13:20Z\nthread: main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, javaCrashPendingFileName), []byte("not a crash record\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := takeJavaCrashPending(dir, defaultJavaCrashSettings()); err == nil {
		t.Fatal("a pending file with no header was taken")
	}
	if _, err := os.Stat(filepath.Join(dir, javaCrashPendingFileName)); !os.IsNotExist(err) {
		t.Fatalf("the unreadable pending file was left behind: %v", err)
	}
	crash, err := os.ReadFile(filepath.Join(dir, javaCrashFileName))
	if err != nil || string(crash) != javaCrashRecordPrefix+"2023-11-14T22:13:20Z\nthread: main\n" {
		t.Fatalf("the java crash file changed: %q %v", crash, err)
	}
}
