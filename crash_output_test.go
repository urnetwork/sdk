// A Go crash of an app process left nothing in "send feedback with logs": the
// runtime prints a fatal only to stderr, which Android sends to logcat and
// Apple to the unified log (support inbox 8308, an Android 8.0 phone, whose
// exit reasons Android does not keep). The log directory now holds a copy.
package sdk

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const goCrashTestDirEnv = "URNETWORK_SDK_TEST_GO_CRASH_DIR"
const goCrashTestMessage = "go crash output test 5d1e9a"

// Not a test on its own: the process the crash test starts. It points glog at
// the directory the parent names, then dies of an unrecovered panic.
func TestGoCrashOutputHelperProcess(t *testing.T) {
	dir := os.Getenv(goCrashTestDirEnv)
	if dir == "" {
		t.Skip("run by TestGoCrashOutputLandsInTheLogDirectory")
	}
	if err := SetLogDir(dir); err != nil {
		t.Fatal(err)
	}
	go func() {
		panic(goCrashTestMessage)
	}()
	select {}
}

// A process whose log directory is set dies of a panic: the crash, with its
// message and stack, is in the directory's crash file, not only on stderr.
func TestGoCrashOutputLandsInTheLogDirectory(t *testing.T) {
	dir := t.TempDir()
	command := exec.Command(os.Args[0], "-test.run=^TestGoCrashOutputHelperProcess$", "-test.count=1")
	command.Env = append(os.Environ(), goCrashTestDirEnv+"="+dir)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("the helper process did not crash: %s", output)
	}
	if !strings.Contains(string(output), goCrashTestMessage) {
		t.Fatalf("the helper process crashed some other way: %s", output)
	}
	crash, err := os.ReadFile(filepath.Join(dir, "go-crash.txt"))
	if err != nil {
		t.Fatalf("no crash file in the log directory: %v", err)
	}
	if !strings.Contains(string(crash), "panic: "+goCrashTestMessage) || !strings.Contains(string(crash), "goroutine ") {
		t.Fatalf("the crash file lacks the panic and its stack: %q", crash)
	}
}

// The upload's inventory lists the crash file beside the glog files, and the
// upload takes it first, as it takes the exit diagnostics: newer logs must
// never crowd out the file that says why the app died.
func TestUploadLogsCarryTheGoCrashFile(t *testing.T) {
	dir := t.TempDir()
	glogName := "gojni.localhost.android.log.INFO.20261113-201455.18745"
	if err := os.WriteFile(filepath.Join(dir, glogName), []byte(strings.Repeat("x", 4096)), 0600); err != nil {
		t.Fatal(err)
	}
	crashPath := filepath.Join(dir, "go-crash.txt")
	if err := os.WriteFile(crashPath, []byte("panic: test\n\ngoroutine 1 [running]:\n"), 0600); err != nil {
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
		if logFileInfo.Name == "go-crash.txt" {
			crash = logFileInfo
		}
	}
	if crash == nil {
		t.Fatalf("the inventory left out the crash file: %+v", inventory.getAll())
	}
	if crash.Severity != "ERROR" {
		t.Fatalf("the crash file's severity is %q", crash.Severity)
	}

	// a cap that holds the crash file and not the newer glog file as well
	selected := selectUploadLogFiles(inventory.getAll(), crash.ByteCount+1024, 512)
	if len(selected) != 1 || selected[0].Name != "go-crash.txt" {
		t.Fatalf("the upload did not take the crash file first: %+v", selected)
	}
}

// A crash file over its bound when a process starts is emptied, so a crash
// loop cannot grow it without bound; one under it is kept whole.
func TestGoCrashFileIsEmptiedOverItsBound(t *testing.T) {
	settings := &goCrashSettings{MaxByteCount: 64}

	overDir := t.TempDir()
	overPath := filepath.Join(overDir, goCrashFileName)
	if err := os.WriteFile(overPath, []byte(strings.Repeat("y", 65)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := setGoCrashOutputDir(overDir, settings); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(overPath); err != nil || info.Size() != 0 {
		t.Fatalf("the crash file over its bound was not emptied: %v %v", info, err)
	}

	underDir := t.TempDir()
	underPath := filepath.Join(underDir, goCrashFileName)
	if err := os.WriteFile(underPath, []byte(strings.Repeat("z", 64)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := setGoCrashOutputDir(underDir, settings); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(underPath); err != nil || info.Size() != 64 {
		t.Fatalf("the crash file under its bound was changed: %v %v", info, err)
	}
}

// Every process opens its crash file when its logs start; an empty one, the
// usual case, is no part of an upload or an export.
func TestUploadLogsLeaveOutAnEmptyGoCrashFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, goCrashFileName), nil, 0600); err != nil {
		t.Fatal(err)
	}
	inventory := NewLogFileInfoList()
	if err := appendLogFilesIn(inventory, dir, "app"); err != nil {
		t.Fatal(err)
	}
	if 0 < len(inventory.getAll()) {
		t.Fatalf("the inventory listed an empty crash file: %+v", inventory.getAll()[0])
	}
}
