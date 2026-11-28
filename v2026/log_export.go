//go:build !ios_extension

package sdk

// A person's own logs, in the app: Settings > Export logs and View logs on
// android and apple.
//
// Export logs is the upload behind "send feedback with logs" (log_upload.go)
// written to a file instead of posted: the same plan, the newest glog files of
// every process under the log root that fit the upload's cap with the exit
// diagnostics and crash files first, and the same zip writer. The app hands
// the file to the share sheet. Like the upload, it holds the logs as they were
// written. The developer bundle (ExportDiagnosticBundle) is the one with a
// choice of files, a manifest and redaction.
//
// View logs reads the last lines of each process's newest log, the files an
// upload starts with.
//
// Both read in this process. On ios that is the app, which shares the log root
// with the network extension, so an export holds both processes' logs, as the
// app's own upload does (Api.UploadLogs); the app asks a running extension to
// flush its glog first. The extension never exports, so this is not in the
// ios_extension binding.

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/urnetwork/connect/v2026"
)

// Writes the zip an upload from this process sends (Api.UploadLogs, or
// Device.UploadLogs where the device runs in this process) to destPath, and
// says what it holds: the number of log files and the zip's size. It flushes
// this process's glog first, as the upload does.
//
// The zip is written beside destPath and renamed onto it, so destPath never
// holds a partial zip. With no log file to send nothing is written: FileCount
// is 0 and destPath is left as it was. The zip is built before this returns,
// reading up to the upload's cap from disk, so call it off the ui thread.
func ExportUploadLogs(destPath string) (*ExportResult, error) {
	return exportUploadLogs(destPath, uploadLogsMaxByteCount, connect.DefaultLogger())
}

func exportUploadLogs(destPath string, maxByteCount int64, log connect.Logger) (*ExportResult, error) {
	if destPath == "" {
		return nil, fmt.Errorf("no export path")
	}

	FlushGlog()

	plan := newUploadLogsPlan(maxByteCount, nil, log)
	defer plan.close()

	// a unique name beside destPath, so that two exports never write one file
	// and the rename onto destPath stays in one directory
	zipFile, err := os.CreateTemp(filepath.Dir(destPath), ".logs-*.zip")
	if err != nil {
		return nil, err
	}
	zipPath := zipFile.Name()

	fileCount, err := plan.writeZip(zipFile, log)
	if closeErr := zipFile.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(zipPath)
		return nil, err
	}
	result := &ExportResult{
		FileCount:      fileCount,
		MissingSources: NewStringList(),
	}
	if fileCount == 0 {
		// a zip of nothing is not a file to send
		os.Remove(zipPath)
		return result, nil
	}
	if err := os.Rename(zipPath, destPath); err != nil {
		os.Remove(zipPath)
		return nil, err
	}
	if info, err := os.Stat(destPath); err == nil {
		result.ByteCount = info.Size()
	}
	log.Infof("[log]export zipped %d of %d planned log files", fileCount, len(plan.logFileInfos))
	return result, nil
}

// The end of one log file, for a person to read in the app (LogTails).
type LogTail struct {
	// the process that wrote the file, its folder under the log root, such as
	// "app" or "extension"
	Source string
	// the file's glog name
	Name string
	// the file's last lines, oldest first, without their line ends, as valid
	// utf-8
	Lines *StringList
	// true when the file has lines before Lines
	Truncated bool
}

type LogTailList struct {
	exportedList[*LogTail]
}

func NewLogTailList() *LogTailList {
	return &LogTailList{
		exportedList: *newExportedList[*LogTail](),
	}
}

// Bounds what one tail reads.
type logTailSettings struct {
	// The most lines of one file in a tail: the end of a busy log, a few
	// minutes of it, enough to see what the app is doing now. Non-positive
	// keeps no lines.
	MaxLineCount int
	// The most bytes read from the end of one file. It bounds the read
	// whatever the length of the lines: glog writes lines of up to 15000
	// bytes. Non-positive reads nothing.
	MaxByteCount int64
}

func defaultLogTailSettings() *logTailSettings {
	return &logTailSettings{
		MaxLineCount: 500,
		MaxByteCount: 256 * 1024,
	}
}

// Returns the last lines of the newest INFO log file of each process under
// the log root, else of this process's log directory, one tail per process in
// source order. glog writes every line, whatever its severity, to the INFO
// file, so a process's newest INFO file holds its latest lines. It flushes
// this process's glog first. Empty when there is no log file.
//
// Reads from disk, so call it off the ui thread.
func LogTails() *LogTailList {
	FlushGlog()
	inventory, _, _ := logInventory()
	return logTailsOf(inventory.getAll(), defaultLogTailSettings(), connect.DefaultLogger())
}

// The tails of the newest INFO glog file of each source in logFileInfos. The
// files of log_special_files.go are left out: they are not glog files, and
// some are masked when they are sent. A file that cannot be read is left out,
// with a line in this process's log.
func logTailsOf(logFileInfos []*LogFileInfo, settings *logTailSettings, log connect.Logger) *LogTailList {
	sourceLogFileInfos := map[string]*LogFileInfo{}
	for _, logFileInfo := range logFileInfos {
		if _, special := logSpecialFileOf(logFileInfo.Name); special {
			continue
		}
		if logSeverityOf(logFileInfo.Name) != "INFO" {
			continue
		}
		newest, ok := sourceLogFileInfos[logFileInfo.Source]
		// glog names a file for the time it started, so of two files written at
		// the same time the later name is the newer
		if !ok || newest.ModifiedMillis < logFileInfo.ModifiedMillis ||
			newest.ModifiedMillis == logFileInfo.ModifiedMillis && newest.Name < logFileInfo.Name {
			sourceLogFileInfos[logFileInfo.Source] = logFileInfo
		}
	}

	// The last lines of the file at path, oldest first: at most
	// settings.MaxLineCount, from at most its last settings.MaxByteCount bytes,
	// and true when the file has lines before them. A line the read starts
	// inside is left out, so every line is whole, except that the last line of
	// a live file may still be being written. Bytes written after the file's
	// size is read are not read.
	readTail := func(path string) ([]string, bool, error) {
		file, err := os.Open(path)
		if err != nil {
			return nil, false, err
		}
		defer file.Close()
		fileInfo, err := file.Stat()
		if err != nil {
			return nil, false, err
		}
		size := fileInfo.Size()
		if settings.MaxLineCount <= 0 || settings.MaxByteCount <= 0 {
			return []string{}, 0 < size, nil
		}

		offset := max(0, size-settings.MaxByteCount)
		// one byte more, before the offset, says whether the read starts a line
		readOffset := max(0, offset-1)
		buffer := make([]byte, size-readOffset)
		n, err := file.ReadAt(buffer, readOffset)
		if err != nil && err != io.EOF {
			return nil, false, err
		}
		buffer = buffer[:n]

		truncated := 0 < offset
		if 0 < offset {
			lineEnd := bytes.IndexByte(buffer, '\n')
			if lineEnd < 0 {
				// the read is all one line that starts before it
				return []string{}, true, nil
			}
			buffer = buffer[lineEnd+1:]
		}

		text := strings.TrimSuffix(string(buffer), "\n")
		if text == "" {
			return []string{}, truncated, nil
		}
		lines := strings.Split(text, "\n")
		if settings.MaxLineCount < len(lines) {
			lines = lines[len(lines)-settings.MaxLineCount:]
			truncated = true
		}
		for i, line := range lines {
			// gomobile hands a string to java and swift as utf-8, and a log
			// line may hold any bytes
			lines[i] = strings.ToValidUTF8(strings.TrimSuffix(line, "\r"), "\uFFFD")
		}
		return lines, truncated, nil
	}

	sources := []string{}
	for source := range sourceLogFileInfos {
		sources = append(sources, source)
	}
	slices.Sort(sources)

	logTails := NewLogTailList()
	for _, source := range sources {
		logFileInfo := sourceLogFileInfos[source]
		lines, truncated, err := readTail(logFileInfo.Path)
		if err != nil {
			log.Infof("[log]tail leaves out %q: %v", source+"/"+logFileInfo.Name, err)
			continue
		}
		tailLines := NewStringList()
		tailLines.addAll(lines...)
		logTails.Add(&LogTail{
			Source:    source,
			Name:      logFileInfo.Name,
			Lines:     tailLines,
			Truncated: truncated,
		})
	}
	return logTails
}
