package sdk

// The Java crash file: an Android app's Java and Kotlin crashes, in its log
// directory beside the glog files and the Go crash file (crash_output.go).
//
// Android sends an uncaught exception's stack only to logcat, and before
// Android 11 it keeps no exit reason either, so a crash left nothing in "send
// feedback with logs" (support inbox 8308: "the app crashes and closes
// itself" on an Android 8.0 phone). The app's default uncaught exception
// handler (android JavaCrashRecorder) writes the crash as it happens, as
// java-crash-pending.txt in the app's log directory: the time, the app
// version, the thread's name and the stack trace, and nothing of the account,
// the network or the settings. It runs no Go code, since a crash can come
// before the app has loaded the sdk, and then hands the exception on to the
// handler before it, so the system and Google Play still see the crash.
//
// When a process next points glog at the directory (setLogDirWithRoot), the
// pending record is taken into java-crash.txt: its addresses and ids are
// masked as the diagnostic export masks them (diagnostics_redact.go) and its
// email addresses replaced, it is appended after the earlier records, and the
// file keeps its newest records within a bound (javaCrashSettings). The file
// is written whole to a staging file and renamed into place, then the pending
// record is removed. The upload carries the file before the glog files, as it
// carries the exit diagnostics and the Go crash file (selectUploadLogFiles);
// it leaves the device only there and in the diagnostic export.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The file's name in a process's log directory. It is not a glog name, so
// retention never takes it for a log file; the inventory lists it by this
// name.
const javaCrashFileName = "java-crash.txt"

// The record the app's handler leaves at a crash, for the next start to take.
// Never uploaded: it is not yet masked.
const javaCrashPendingFileName = "java-crash-pending.txt"

// The severity the inventory gives the file: every record in it is a crash.
const javaCrashSeverity = "ERROR"

// The first line of a pending record, which names its format.
const javaCrashPendingHeader = "urnetwork-java-crash 1"

// The line that opens each record of the file, followed by the crash's time.
const javaCrashRecordPrefix = "--- java crash at "

// An email address, which an exception message can carry and the diagnostic
// export does not mask.
var javaCrashEmailPattern = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`)

// Bounds the java crash file.
type javaCrashSettings struct {
	// How much of a pending record is read; the rest of it is cut. The app's
	// handler cuts a stack trace to 64 KiB of text, so this holds any record
	// it writes.
	MaxPendingByteCount int64
	// The file keeps its newest records within this, dropping the oldest. A
	// record larger than this by itself is cut to it, keeping its head, where
	// the exception and the frames that threw it are.
	MaxByteCount int64
}

func defaultJavaCrashSettings() *javaCrashSettings {
	return &javaCrashSettings{
		MaxPendingByteCount: 128 * 1024,
		MaxByteCount:        256 * 1024,
	}
}

// Takes dir's pending java crash record, if there is one, into dir's java
// crash file, and removes it. A pending record that is not one the app's
// handler writes is removed and reported, so it is never taken again.
func takeJavaCrashPending(dir string, settings *javaCrashSettings) error {
	pendingPath := filepath.Join(dir, javaCrashPendingFileName)
	info, err := os.Lstat(pendingPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("the pending java crash record is not a regular file")
	}
	pending, err := readJavaCrashPending(pendingPath, settings.MaxPendingByteCount)
	if err != nil {
		return err
	}
	record, err := javaCrashRecord(pending)
	if err != nil {
		// a record that cannot be read now never can
		_ = os.Remove(pendingPath)
		return err
	}
	crashPath := filepath.Join(dir, javaCrashFileName)
	previous, err := os.ReadFile(crashPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := writeJavaCrashFile(dir, crashPath, appendJavaCrashRecord(string(previous), record, settings.MaxByteCount)); err != nil {
		return err
	}
	return os.Remove(pendingPath)
}

func readJavaCrashPending(path string, maxByteCount int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	pending, err := io.ReadAll(io.LimitReader(file, maxByteCount))
	if err != nil {
		return "", err
	}
	return string(pending), nil
}

// The file record for one pending record: the opening line with the crash's
// time, the app version and the thread's name, then the stack trace, with
// addresses, ids and email addresses masked.
func javaCrashRecord(pending string) (string, error) {
	reader := bufio.NewReader(strings.NewReader(pending))
	header, err := reader.ReadString('\n')
	if err != nil || strings.TrimRight(header, "\r\n") != javaCrashPendingHeader {
		return "", errors.New("the pending java crash record has no header")
	}
	fields := map[string]string{}
	for {
		line, err := reader.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		key, value, found := strings.Cut(line, ": ")
		if !found {
			return "", errors.New("the pending java crash record has a malformed field")
		}
		fields[key] = value
		if err != nil {
			break
		}
	}
	timeMillis, err := strconv.ParseInt(fields["time_millis"], 10, 64)
	if err != nil {
		return "", errors.New("the pending java crash record has no time")
	}
	stack, _ := io.ReadAll(reader)
	if len(stack) == 0 {
		return "", errors.New("the pending java crash record has no stack trace")
	}

	redactor, err := newLogRedactor()
	if err != nil {
		return "", err
	}
	mask := func(text string) string {
		return javaCrashEmailPattern.ReplaceAllString(redactor.redactLine(text), "<email>")
	}
	record := &strings.Builder{}
	record.WriteString(javaCrashRecordPrefix)
	record.WriteString(time.UnixMilli(timeMillis).UTC().Format(time.RFC3339Nano))
	record.WriteString("\n")
	record.WriteString("app version: " + mask(fields["app_version"]) + "\n")
	record.WriteString("thread: " + mask(fields["thread"]) + "\n")
	for _, line := range strings.SplitAfter(string(stack), "\n") {
		record.WriteString(mask(line))
	}
	if !strings.HasSuffix(record.String(), "\n") {
		record.WriteString("\n")
	}
	return record.String(), nil
}

// The records of a java crash file, oldest first, each from its opening line
// through the line before the next one opens. Text before the first opening
// line is no record and is dropped.
func splitJavaCrashRecords(text string) []string {
	records := []string{}
	start := -1
	offset := 0
	for _, line := range strings.SplitAfter(text, "\n") {
		if strings.HasPrefix(line, javaCrashRecordPrefix) {
			if 0 <= start {
				records = append(records, text[start:offset])
			}
			start = offset
		}
		offset += len(line)
	}
	if 0 <= start {
		records = append(records, text[start:])
	}
	return records
}

// The file's text after record is added: the earlier records, oldest first,
// then record, keeping the newest that fit in maxByteCount. Non-positive keeps
// every record.
func appendJavaCrashRecord(previous string, record string, maxByteCount int64) string {
	records := append(splitJavaCrashRecords(previous), record)
	if maxByteCount <= 0 {
		return strings.Join(records, "")
	}
	if maxByteCount < int64(len(record)) {
		// the newest alone, cut to the bound at a line end, keeping its head
		cut := record[:maxByteCount]
		if i := strings.LastIndex(cut, "\n"); 0 < i {
			cut = cut[:i+1]
		}
		return cut
	}
	byteCount := int64(0)
	first := len(records)
	for 0 < first && byteCount+int64(len(records[first-1])) <= maxByteCount {
		first -= 1
		byteCount += int64(len(records[first]))
	}
	return strings.Join(records[first:], "")
}

// Writes text as dir's java crash file: whole to a staging file, then renamed
// into place, so a reader sees the old file or the new one, never a part.
func writeJavaCrashFile(dir string, path string, text string) error {
	staging, err := os.CreateTemp(dir, ".java-crash-*")
	if err != nil {
		return err
	}
	stagingPath := staging.Name()
	defer os.Remove(stagingPath)
	if _, err := staging.WriteString(text); err != nil {
		staging.Close()
		return err
	}
	if err := staging.Sync(); err != nil {
		staging.Close()
		return err
	}
	if err := staging.Close(); err != nil {
		return err
	}
	if err := os.Chmod(stagingPath, LocalStorageFilePermissions); err != nil {
		return err
	}
	if err := os.Rename(stagingPath, path); err != nil {
		return err
	}
	// best effort: some platforms cannot flush a directory (Windows)
	if directory, err := os.Open(dir); err == nil {
		_ = directory.Sync()
		directory.Close()
	}
	return nil
}
