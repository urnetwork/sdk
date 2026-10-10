package sdk

// The log upload behind "send feedback with logs": one zip of glog files,
// posted to /log/{feedback_id}/upload, which the server stores only up to
// 100 MiB and rate limits to one upload per network per period.
//
// Either process of a platform can build it. Under a log root
// (SetLogDirForProcess) the zip holds every process's directory, each under its
// process name, so on ios the network extension's upload (the device's, while
// the tunnel is up) holds the app's logs, and the app's upload (Api.UploadLogs,
// while the extension is down) holds the extension's last logs. Under the
// legacy single directory (SetLogDir) it holds that directory's files under
// their bare names, as it always did.
//
// A process that logs outside the log root rides along as open files instead
// (UploadLogsFile, DeviceLocal.UploadLogsWithFiles): on windows and linux the
// app (gui) and the service (urnetworkd) are separate processes with separate
// log directories, and the service, which carries the upload, adds the app's
// files under the app's own folder of the one zip.
//
// The files are the newest that fit a cap below the server's, so the upload is
// never one the server drops for its size. Only glog files are read, never the
// symlinks glog keeps beside them or anything else in the directories.

import (
	"archive/zip"
	"cmp"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/urnetwork/connect"
)

// Bounds the log bytes one upload carries.
//
// The server keeps a feedback's log zip only when it is at most 100 MiB
// (server controller.LogFileMaxByteCount) and drops a larger one whole. The
// files under a log root are not bounded by that on their own: retention
// (clearOldLogs) keeps the 4 newest files of a process directory only when the
// process starts, glog starts a new 16 MiB file each time one fills, and on ios
// the app and the network extension each have a directory. Each file counts
// against this with its zip entry (uploadLogsEntryByteCount). Deflate adds at
// most a few bytes per 16 KiB block of bytes that do not compress, and the zip
// a few dozen for its end records, so this leaves room for those under the
// server's cap.
const uploadLogsMaxByteCount = int64(96 * 1024 * 1024)

// Bounds what one file adds to the zip beyond its bytes: its local and central
// directory headers, each with its name and modified time, and its data
// descriptor. Counting it against the cap keeps the zip under the cap whatever
// the number of files.
const uploadLogsEntryByteCount = int64(1024)

// Bounds the files of other processes one upload considers. A process's log
// directory holds a handful of glog files (retention keeps 4 at a start, and a
// long run adds one per 16 MiB), so this only stops a caller that hands over
// far more descriptors than any log directory holds.
const uploadLogsMaxOpenFileCount = 64

// Bounds the length of a folder name (UploadLogsFile.Source).
const uploadLogsMaxSourceLength = 32

// A log file of another process that an upload from this process carries in
// its zip as <Source>/<Name>, beside this process's own logs.
//
// On windows and linux the app (the gui) and the service (urnetworkd) are
// separate processes, each with its own log directory, and the service carries
// "send feedback with logs": it has a device when none is connected and a
// network path under the kill switch. The server keeps one zip per feedback
// and admits one upload per network per 5 minutes, so the app's logs can reach
// support only inside the service's zip.
//
// The file is handed over open, never by path. The service runs as LocalSystem
// or root and the app as the signed-in user, and the service answers any local
// user, so a path from the app would let a user have the service read files the
// user cannot. Instead the file is opened with the rights of the process that
// wrote it (on linux the gui opens it and passes the descriptor over the
// control socket, on windows the service opens it while impersonating the app's
// pipe client), and the upload reads it only through that descriptor. It takes
// only a regular file with a glog name, and never more than the planned size.
//
// The upload borrows the descriptor for the call (DeviceLocal.UploadLogsWithFiles):
// it reads a duplicate that it closes itself before the call returns, and
// FileDescriptor stays open and the caller's to close.
type UploadLogsFile struct {
	// the folder of the process that wrote the file, such as "app" or "gui":
	// lowercase letters, digits and dashes, starting with a letter
	Source string
	// the file's glog name, its entry in the folder
	Name string
	// the open file: a file descriptor on unix, a handle on windows
	FileDescriptor int64
}

type UploadLogsFileList struct {
	exportedList[*UploadLogsFile]
}

func NewUploadLogsFileList() *UploadLogsFileList {
	return &UploadLogsFileList{
		exportedList: *newExportedList[*UploadLogsFile](),
	}
}

// What one upload from this process sends.
type uploadLogsPlan struct {
	// the log files in the upload, newest first
	logFileInfos []*LogFileInfo
	// true when this process's files come from the per-process directories
	// under a log root and are zipped as <source>/<name>. Under the legacy
	// single directory they keep the bare names they were always zipped under.
	perProcess bool
	// the files of other processes in the plan (UploadLogsFile), each read
	// through its own duplicate of the caller's descriptor and zipped as
	// <source>/<name>, by the info that plans it. close closes them.
	logFileInfoOpenFiles map[*LogFileInfo]*uploadLogsOpenFile
}

// A file of another process in a plan: the duplicate the upload reads, and
// what it was when planned.
type uploadLogsOpenFile struct {
	file     *os.File
	fileInfo os.FileInfo
}

// Picks the glog files an upload from this process sends: those of every
// process under the log root, else those in this process's log directory,
// with the open files of other processes (uploadLogsFiles). Symlinks and files
// glog did not name are never in it (logInventory, openUploadLogsFiles). The
// plan holds duplicates of the descriptors it took; close it when done.
func newUploadLogsPlan(maxByteCount int64, uploadLogsFiles []*UploadLogsFile, log connect.Logger) *uploadLogsPlan {
	perProcess := GetLogRoot() != ""
	inventory, _, sourceRoots := logInventory()

	ownSources := map[string]bool{}
	if perProcess {
		for source := range sourceRoots {
			ownSources[source] = true
		}
	}
	openLogFileInfos, logFileInfoOpenFiles := openUploadLogsFiles(uploadLogsFiles, ownSources, log)

	logFileInfos := append(inventory.getAll(), openLogFileInfos...)
	selectedLogFileInfos := selectUploadLogFiles(logFileInfos, maxByteCount, uploadLogsEntryByteCount)

	// the duplicates of files that did not fit are closed now
	for logFileInfo, openFile := range logFileInfoOpenFiles {
		if !slices.Contains(selectedLogFileInfos, logFileInfo) {
			openFile.file.Close()
			delete(logFileInfoOpenFiles, logFileInfo)
		}
	}

	return &uploadLogsPlan{
		logFileInfos:         selectedLogFileInfos,
		perProcess:           perProcess,
		logFileInfoOpenFiles: logFileInfoOpenFiles,
	}
}

// Takes the files of other processes an upload may carry, in the given order,
// and returns them planned (Path is empty: they are read only through their
// duplicates) with the duplicate opened for each.
//
// A file is left out, with a line in this process's log, when its folder is not
// a plain name or is the folder of a process under the log root (ownSources),
// when its name is not one glog writes or repeats one already taken, when its
// descriptor cannot be duplicated, or when it is not a regular file (a
// directory, a pipe, a socket, a device). One bad file never fails the upload:
// this process's own logs, which support reads first, still go.
func openUploadLogsFiles(uploadLogsFiles []*UploadLogsFile, ownSources map[string]bool, log connect.Logger) ([]*LogFileInfo, map[*LogFileInfo]*uploadLogsOpenFile) {
	logFileInfos := []*LogFileInfo{}
	logFileInfoOpenFiles := map[*LogFileInfo]*uploadLogsOpenFile{}
	zipNames := map[string]bool{}
	for _, uploadLogsFile := range uploadLogsFiles {
		if uploadLogsFile == nil {
			continue
		}
		if uploadLogsMaxOpenFileCount <= len(logFileInfos) {
			log.Infof("[log]upload leaves out the files of other processes past the first %d", uploadLogsMaxOpenFileCount)
			break
		}
		if !isUploadLogsSource(uploadLogsFile.Source) {
			log.Infof("[log]upload leaves out a file of folder %q: not a folder name", uploadLogsFile.Source)
			continue
		}
		if ownSources[uploadLogsFile.Source] {
			log.Infof("[log]upload leaves out a file of folder %q: the folder of a process under the log root", uploadLogsFile.Source)
			continue
		}
		if !isUploadLogsFileName(uploadLogsFile.Name) {
			log.Infof("[log]upload leaves out a file of folder %q: not a glog file name", uploadLogsFile.Source)
			continue
		}
		zipName := uploadLogsFile.Source + "/" + uploadLogsFile.Name
		if zipNames[zipName] {
			log.Infof("[log]upload leaves out a second %q", zipName)
			continue
		}
		file, err := openUploadLogsFileDuplicate(uploadLogsFile.FileDescriptor, zipName)
		if err != nil {
			log.Infof("[log]upload leaves out %q: %v", zipName, err)
			continue
		}
		fileInfo, err := file.Stat()
		if err != nil {
			file.Close()
			log.Infof("[log]upload leaves out %q: %v", zipName, err)
			continue
		}
		if !fileInfo.Mode().IsRegular() {
			file.Close()
			log.Infof("[log]upload leaves out %q: not a regular file (%v)", zipName, fileInfo.Mode().Type())
			continue
		}
		zipNames[zipName] = true
		logFileInfo := &LogFileInfo{
			Name:           uploadLogsFile.Name,
			Source:         uploadLogsFile.Source,
			Severity:       logSeverityOf(uploadLogsFile.Name),
			ByteCount:      fileInfo.Size(),
			ModifiedMillis: fileInfo.ModTime().UnixMilli(),
		}
		logFileInfos = append(logFileInfos, logFileInfo)
		logFileInfoOpenFiles[logFileInfo] = &uploadLogsOpenFile{
			file:     file,
			fileInfo: fileInfo,
		}
	}
	return logFileInfos, logFileInfoOpenFiles
}

// A folder name for the files of another process: a lowercase letter, then
// lowercase letters, digits and dashes. It is one path segment of the zip, so
// it can never climb out of the zip or into another process's folder by name.
func isUploadLogsSource(source string) bool {
	if source == "" || uploadLogsMaxSourceLength < len(source) {
		return false
	}
	for i, c := range source {
		switch {
		case 'a' <= c && c <= 'z':
		case 0 < i && '0' <= c && c <= '9':
		case 0 < i && c == '-':
		default:
			return false
		}
	}
	return true
}

// A name glog writes (<program>.<host>.<user>.log.<SEVERITY>.<time>.<pid>),
// usable as one zip entry: valid utf-8 of at most 255 bytes, with no path
// separator, no control character and no leading dot. The name comes from the
// process that wrote the file, so it is checked before it names an entry.
func isUploadLogsFileName(name string) bool {
	if name == "" || 255 < len(name) || !utf8.ValidString(name) {
		return false
	}
	if strings.HasPrefix(name, ".") || logSeverityOf(name) == "" {
		return false
	}
	for _, c := range name {
		if c < 0x20 || c == 0x7f || c == '/' || c == '\\' || c == ':' {
			return false
		}
	}
	return true
}

// Takes files newest first while their sizes, each plus entryByteCount, fit in
// maxByteCount. A file that does not fit is skipped rather than ending the
// selection, so the most recent logs of each process are the ones sent, and an
// older small file can still follow a large one that did not fit. Files written
// at the same time are taken in source and name order.
func selectUploadLogFiles(logFileInfos []*LogFileInfo, maxByteCount int64, entryByteCount int64) []*LogFileInfo {
	newestLogFileInfos := slices.Clone(logFileInfos)
	slices.SortStableFunc(newestLogFileInfos, func(a *LogFileInfo, b *LogFileInfo) int {
		if c := cmp.Compare(b.ModifiedMillis, a.ModifiedMillis); c != 0 {
			return c
		}
		if c := strings.Compare(a.Source, b.Source); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})

	selectedLogFileInfos := []*LogFileInfo{}
	selectedByteCount := int64(0)
	for _, logFileInfo := range newestLogFileInfos {
		if maxByteCount < selectedByteCount+logFileInfo.ByteCount+entryByteCount {
			continue
		}
		selectedLogFileInfos = append(selectedLogFileInfos, logFileInfo)
		selectedByteCount += logFileInfo.ByteCount + entryByteCount
	}
	return selectedLogFileInfos
}

// A file's entry in the zip.
func (self *uploadLogsPlan) zipName(logFileInfo *LogFileInfo) string {
	if _, ok := self.logFileInfoOpenFiles[logFileInfo]; ok || self.perProcess {
		return logFileInfo.Source + "/" + logFileInfo.Name
	}
	return logFileInfo.Name
}

// Writes the planned files as a zip to w, and returns how many it holds.
//
// Each file is copied only up to the size it was planned at: the live file of a
// running process keeps growing while it is zipped, and the plan is what keeps
// the upload under the cap. A file that is gone by now (a process pruned or
// rotated it) is left out. A file of another process is read through its
// duplicate, never opened by a path. Failing to write the zip is an error.
func (self *uploadLogsPlan) writeZip(w io.Writer, log connect.Logger) (int, error) {
	fileCount := 0
	zipWriter := zip.NewWriter(w)
	for _, logFileInfo := range self.logFileInfos {
		if openFile, ok := self.logFileInfoOpenFiles[logFileInfo]; ok {
			err := zipWriteEntry(
				zipWriter,
				self.zipName(logFileInfo),
				io.NewSectionReader(openFile.file, 0, logFileInfo.ByteCount),
				openFile.fileInfo,
				nil,
			)
			if err != nil {
				zipWriter.Close()
				return fileCount, err
			}
			fileCount += 1
			continue
		}

		logFile, err := os.Open(logFileInfo.Path)
		if err != nil {
			log.Infof("[log]upload leaves out %q: %v", self.zipName(logFileInfo), err)
			continue
		}
		logFileStat, err := logFile.Stat()
		if err != nil {
			logFile.Close()
			log.Infof("[log]upload leaves out %q: %v", self.zipName(logFileInfo), err)
			continue
		}
		err = zipWriteEntry(
			zipWriter,
			self.zipName(logFileInfo),
			io.LimitReader(logFile, logFileInfo.ByteCount),
			logFileStat,
			nil,
		)
		logFile.Close()
		if err != nil {
			zipWriter.Close()
			return fileCount, err
		}
		fileCount += 1
	}
	if err := zipWriter.Close(); err != nil {
		return fileCount, err
	}
	return fileCount, nil
}

// Closes the duplicates of the other processes' files. The caller's descriptors
// stay open.
func (self *uploadLogsPlan) close() {
	for logFileInfo, openFile := range self.logFileInfoOpenFiles {
		openFile.file.Close()
		delete(self.logFileInfoOpenFiles, logFileInfo)
	}
}

// Lists the log files an upload from this process (Api.UploadLogs, or
// Device.UploadLogs where the device runs in this process) would send now,
// newest first: the glog files of every process under the log root, else those
// in this process's log directory, the newest that together fit the upload's
// cap.
func UploadLogsInventory() *LogFileInfoList {
	inventory := NewLogFileInfoList()
	inventory.addAll(newUploadLogsPlan(uploadLogsMaxByteCount, nil, connect.DefaultLogger()).logFileInfos...)
	return inventory
}

// Zips what an upload from this process sends, with the files of other
// processes (uploadLogsFiles), into a new file in this process's log
// directory, and returns its path. The other processes' files are read before
// this returns, and their descriptors are left open.
//
// It flushes glog first: glog buffers its file writes and flushes them only
// every 30 seconds, so the newest lines, such as an app line written with
// LogAppInfo just before the user sent feedback, would otherwise be missing.
// It flushes this process only. A process whose logs share the root flushes its
// own before it asks this one to upload (DeviceRemote.UploadLogs), and a
// process that hands over its files flushes before it opens them.
func zipUploadLogs(maxByteCount int64, uploadLogsFiles []*UploadLogsFile, log connect.Logger) (string, error) {
	FlushGlog()

	logDir := GetLogDir()
	if logDir == "" {
		return "", fmt.Errorf("no log directory")
	}

	plan := newUploadLogsPlan(maxByteCount, uploadLogsFiles, log)
	defer plan.close()

	// a unique name, so that two uploads never write one file
	zipFile, err := os.CreateTemp(logDir, "logs-*.zip")
	if err != nil {
		return "", err
	}
	zipPath := zipFile.Name()

	fileCount, err := plan.writeZip(zipFile, log)
	if closeErr := zipFile.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(zipPath)
		return "", err
	}
	log.Infof("[log]upload zipped %d of %d planned log files (%d of other processes)", fileCount, len(plan.logFileInfos), len(plan.logFileInfoOpenFiles))
	return zipPath, nil
}

// Zips what an upload from this process sends, with the files of other
// processes (uploadLogsFiles), and posts it through api to the feedback with
// feedbackId. The zip is built before this returns, so the other processes'
// descriptors are no longer needed then. The post runs in the background,
// callback (when not nil) gets its result, and the zip is removed when the post
// is done.
func uploadLogs(api *Api, log connect.Logger, feedbackId string, uploadLogsFiles []*UploadLogsFile, callback UploadLogsCallback) error {
	zipPath, err := zipUploadLogs(uploadLogsMaxByteCount, uploadLogsFiles, log)
	if err != nil {
		log.Errorf("Failed to zip the log files: %v", err)
		return err
	}

	zipFile, err := os.Open(zipPath)
	if err != nil {
		os.Remove(zipPath)
		return err
	}

	zipStat, err := zipFile.Stat()
	if err != nil {
		zipFile.Close()
		os.Remove(zipPath)
		return err
	}
	log.Infof("Uploading log file %q (%d bytes)", zipPath, zipStat.Size())

	api.postLogsZip(feedbackId, zipFile, connect.NewApiCallback[*UploadLogsResult](func(res *UploadLogsResult, err error) {
		// Ensure resources are cleaned up after upload completes (success or error)
		zipFile.Close()
		os.Remove(zipPath)

		// Forward result to the original callback
		if callback != nil {
			callback.Result(res, err)
		}
	}))

	return nil
}

// Uploads the log files this process can read to the feedback with feedbackId,
// through this api: one zip of the glog files of every process under the log
// root, else of this process's log directory, newest first up to the upload's
// cap (UploadLogsInventory lists them).
//
// Device.UploadLogs does this in the process that runs the device. This is for
// a process that cannot ask one: on ios the app, while the network extension
// is not running or its device rpc is not connected. The app and the extension
// share the app group's log root, so the app's upload holds the extension's
// last logs as well as its own.
//
// The zip is built before this returns, reading up to the cap from disk, so
// call it off the ui thread. The post runs in the background, and callback,
// when not nil, gets its result.
func (self *Api) UploadLogs(feedbackId string, callback UploadLogsCallback) error {
	return uploadLogs(self, self.logger(), feedbackId, nil, callback)
}
