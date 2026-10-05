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

// What one upload from this process sends.
type uploadLogsPlan struct {
	// the log files in the upload, newest first
	logFileInfos []*LogFileInfo
	// true when the files come from the per-process directories under a log
	// root and are zipped as <source>/<name>. Under the legacy single directory
	// they keep the bare names they were always zipped under.
	perProcess bool
}

// Picks the glog files an upload from this process sends: those of every
// process under the log root, else those in this process's log directory.
// Symlinks and files glog did not name are never in it (logInventory).
func newUploadLogsPlan(maxByteCount int64) *uploadLogsPlan {
	perProcess := GetLogRoot() != ""
	inventory, _, _ := logInventory()
	return &uploadLogsPlan{
		logFileInfos: selectUploadLogFiles(inventory.getAll(), maxByteCount, uploadLogsEntryByteCount),
		perProcess:   perProcess,
	}
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
	if self.perProcess {
		return logFileInfo.Source + "/" + logFileInfo.Name
	}
	return logFileInfo.Name
}

// Writes the planned files as a zip to w, and returns how many it holds.
//
// Each file is copied only up to the size it was planned at: the live file of a
// running process keeps growing while it is zipped, and the plan is what keeps
// the upload under the cap. A file that is gone by now (a process pruned or
// rotated it) is left out. Failing to write the zip is an error.
func (self *uploadLogsPlan) writeZip(w io.Writer, log connect.Logger) (int, error) {
	fileCount := 0
	zipWriter := zip.NewWriter(w)
	for _, logFileInfo := range self.logFileInfos {
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

// Lists the log files an upload from this process (Api.UploadLogs, or
// Device.UploadLogs where the device runs in this process) would send now,
// newest first: the glog files of every process under the log root, else those
// in this process's log directory, the newest that together fit the upload's
// cap.
func UploadLogsInventory() *LogFileInfoList {
	inventory := NewLogFileInfoList()
	inventory.addAll(newUploadLogsPlan(uploadLogsMaxByteCount).logFileInfos...)
	return inventory
}

// Zips what an upload from this process sends into a new file in this
// process's log directory, and returns its path.
//
// It flushes glog first: glog buffers its file writes and flushes them only
// every 30 seconds, so the newest lines, such as an app line written with
// LogAppInfo just before the user sent feedback, would otherwise be missing.
// It flushes this process only. A process whose logs share the root flushes its
// own before it asks this one to upload (DeviceRemote.UploadLogs).
func zipUploadLogs(maxByteCount int64, log connect.Logger) (string, error) {
	FlushGlog()

	logDir := GetLogDir()
	if logDir == "" {
		return "", fmt.Errorf("no log directory")
	}

	plan := newUploadLogsPlan(maxByteCount)

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
	log.Infof("[log]upload zipped %d of %d planned log files", fileCount, len(plan.logFileInfos))
	return zipPath, nil
}

// Zips what an upload from this process sends and posts it through api to the
// feedback with feedbackId. The zip is built before this returns. The post runs
// in the background, callback (when not nil) gets its result, and the zip is
// removed when the post is done.
func uploadLogs(api *Api, log connect.Logger, feedbackId string, callback UploadLogsCallback) error {
	zipPath, err := zipUploadLogs(uploadLogsMaxByteCount, log)
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
	return uploadLogs(self, self.logger(), feedbackId, callback)
}
