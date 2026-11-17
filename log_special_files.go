package sdk

// The files an upload and an export carry beside the glog files, and how.
//
// Beside a process's glog files sit files the process or its host writes that
// say why it ended or what the host did: the exit diagnostics
// (exit_diagnostics.go), the Go crash file (crash_output.go) and the java
// crash file (java_crash.go), and on windows the host's own logs, in the same
// directories its glog writes (windows Common/Log.cpp, Service/main.cpp):
// urnetworkd.log, the service's; urnetwork-app.log, the app's; and
// go-crash.log, the service's capture of a Go fatal from stderr. Until these
// were carried, the app's (the gui's) Go crash file rode in the service's zip
// only under a glog name, so it never did, and the windows hosts' logs never
// left the device at all (support inbox 8308). On windows and linux the hosts
// hand the app's files over under these names as well as under glog names
// (UploadLogsFile).
//
// Every file here but the exit diagnostics, which keep only what their writers
// chose to keep, leaves the device masked, in the upload and in the export
// alike, redacted or not: addresses and ids as the redacted export masks them,
// email addresses, and the user's own folder in a path (maskLogLine). So none
// of them adds what the glog files do not already carry. The glog files are
// carried as before. A file its writer does not bound (the windows hosts'
// logs, which never rotate) is carried as its newest whole lines within its
// MaxByteCount, and an empty one not at all: every process opens its Go crash
// file, and only a crash writes it.

import (
	"archive/zip"
	"bufio"
	"io"
	"os"
	"regexp"
)

// The windows hosts' own logs and crash capture (windows Common/Log.cpp,
// Service/main.cpp), beside their glog files.
const (
	windowsServiceLogFileName     = "urnetworkd.log"
	windowsAppLogFileName         = "urnetwork-app.log"
	windowsServiceGoCrashFileName = "go-crash.log"
)

// How an upload and an export carry one file that is not a glog file.
type logSpecialFile struct {
	// the severity the inventory gives the file
	Severity string
	// planned before the glog files: a small file, rarely written, that says
	// why a process ended
	First bool
	// masked whenever it leaves the device (maskLogLine)
	Masked bool
	// the most of the file carried, its newest whole lines. Non-positive
	// carries it whole.
	MaxByteCount int64
}

// The files beside the glog files that an upload and an export carry, by
// name. A new map on every call.
func defaultLogSpecialFiles() map[string]logSpecialFile {
	return map[string]logSpecialFile{
		exitDiagnosticsFileName: {Severity: exitDiagnosticsSeverity, First: true},
		goCrashFileName:         {Severity: goCrashSeverity, First: true, Masked: true},
		javaCrashFileName:       {Severity: javaCrashSeverity, First: true, Masked: true},
		// a host log of a few days is a few MiB (8386: 2 MiB in five days);
		// these are its newest days, a tenth of the upload's cap each
		windowsServiceLogFileName: {Severity: "INFO", Masked: true, MaxByteCount: 8 * 1024 * 1024},
		windowsAppLogFileName:     {Severity: "INFO", Masked: true, MaxByteCount: 8 * 1024 * 1024},
		// as the Go crash file's own bound (goCrashSettings)
		windowsServiceGoCrashFileName: {Severity: "ERROR", First: true, Masked: true, MaxByteCount: 1024 * 1024},
	}
}

// The special file name names, if it names one.
func logSpecialFileOf(name string) (logSpecialFile, bool) {
	specialFile, ok := defaultLogSpecialFiles()[name]
	return specialFile, ok
}

// How much of a file of byteCount bytes is carried.
func (self logSpecialFile) carriedByteCount(byteCount int64) int64 {
	if 0 < self.MaxByteCount && self.MaxByteCount < byteCount {
		return self.MaxByteCount
	}
	return byteCount
}

// An email address, which a log line or an exception message can carry and
// the redacted export does not mask.
var logEmailPattern = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`)

// The user's own folder in a path: its name is the user's account name.
var (
	// C:\Users\<name>\, or with forward slashes
	logWindowsUserFolderPattern = regexp.MustCompile(`(?i)\b([A-Z]:[\\/]Users[\\/])[^\\/:*?"<>|\r\n]+`)
	// /home/<name>/, /Users/<name>/ (macos)
	logUnixUserFolderPattern = regexp.MustCompile(`(/(?:home|Users)/)[^/\s"']+`)
)

// Masks one line of a file that leaves the device masked: addresses and ids
// as the redacted export masks them, email addresses, and the user's own
// folder in a path.
func (self *logRedactor) maskLogLine(line string) string {
	line = self.redactLine(line)
	line = logEmailPattern.ReplaceAllString(line, "<email>")
	line = logWindowsUserFolderPattern.ReplaceAllString(line, "${1}<user>")
	line = logUnixUserFolderPattern.ReplaceAllString(line, "${1}<user>")
	return line
}

// The part of a file of size bytes that is carried, carriedByteCount bytes: the
// whole file, or its last bytes from the first line that starts in them.
func logSpecialFileSection(file io.ReaderAt, size int64, carriedByteCount int64) (io.Reader, error) {
	if size <= carriedByteCount {
		return io.NewSectionReader(file, 0, size), nil
	}
	offset := size - carriedByteCount
	// the byte before the window says whether the window starts on a line
	previous := make([]byte, 1)
	if _, err := file.ReadAt(previous, offset-1); err != nil {
		return nil, err
	}
	reader := bufio.NewReader(io.NewSectionReader(file, offset, carriedByteCount))
	if previous[0] != '\n' {
		// drop the rest of the line the window starts inside
		if _, err := reader.ReadString('\n'); err != nil && err != io.EOF {
			return nil, err
		}
	}
	return reader, nil
}

// Writes one masked entry into an open zip: r's lines, each masked, while they
// fit in maxByteCount bytes, so that masking never grows an entry past what was
// planned for it.
func zipWriteMaskedEntry(zipWriter *zip.Writer, name string, r io.Reader, fi os.FileInfo, mask func(string) string, maxByteCount int64) error {
	header, err := zip.FileInfoHeader(fi)
	if err != nil {
		return err
	}
	header.Name = name
	header.Method = zip.Deflate
	w, err := zipWriter.CreateHeader(header)
	if err != nil {
		return err
	}
	reader := bufio.NewReader(r)
	byteCount := int64(0)
	for {
		line, readErr := reader.ReadString('\n')
		if line != "" {
			masked := mask(line)
			if maxByteCount < byteCount+int64(len(masked)) {
				return nil
			}
			if _, err := io.WriteString(w, masked); err != nil {
				return err
			}
			byteCount += int64(len(masked))
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}
