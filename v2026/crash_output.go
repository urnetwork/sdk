package sdk

// The Go crash file: what the Go runtime prints when this process dies of an
// unrecovered panic or a fatal error, in the process's log directory beside
// its glog files.
//
// The runtime writes a fatal only to stderr, and no app keeps stderr: Android
// sends it to logcat and Apple to the unified log, so a Go crash left nothing
// in "send feedback with logs" (support inbox 8308: "the app crashes and
// closes itself" on an Android 8.0 phone, whose exit reasons Android does not
// keep). Pointing glog at a directory also points the runtime's crash output
// there (debug.SetCrashOutput), in addition to stderr. The file is not a glog
// file, so glog's retention (clearOldLogs) never removes it; it is emptied
// instead when a process starts with it over its bound (goCrashSettings).
//
// It leaves the device only as the glog files do, inside "send feedback with
// logs" and the diagnostic export, planned before them so that newer logs
// never crowd it out of the upload's cap (selectUploadLogFiles).

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
)

// The file's name in a process's log directory. It is not a glog name, so
// retention never takes it for a log file; the inventory lists it by this
// name.
const goCrashFileName = "go-crash.txt"

// The severity the inventory gives the file: every record in it is a crash.
const goCrashSeverity = "ERROR"

// Bounds the crash file.
type goCrashSettings struct {
	// A file larger than this when a process points its crash output at it is
	// emptied first, so a crash loop cannot grow it without bound. The crash
	// that matters is the newest, and every one after the emptying is kept.
	// Non-positive never empties it.
	MaxByteCount int64
}

func defaultGoCrashSettings() *goCrashSettings {
	return &goCrashSettings{
		// a crash's stack dump is a few KiB per goroutine; this holds several
		// crashes of a busy process
		MaxByteCount: 1024 * 1024,
	}
}

// Points the Go runtime's crash output at dir's crash file, as well as at
// stderr. The runtime keeps its own duplicate of the descriptor, so the file
// is closed here. Only a regular file is written.
func setGoCrashOutputDir(dir string, settings *goCrashSettings) error {
	path := filepath.Join(dir, goCrashFileName)
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("the go crash file is not a regular file")
		}
		if 0 < settings.MaxByteCount && settings.MaxByteCount < info.Size() {
			if err := os.Truncate(path, 0); err != nil {
				return err
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, LocalStorageFilePermissions)
	if err != nil {
		return err
	}
	defer file.Close()
	return debug.SetCrashOutput(file, debug.CrashOptions{})
}
