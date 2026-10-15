// Checked destination reads distinguish absence from observation failure.
// Atomic replacement keeps the last committed destination after interruption;
// authStateLock serializes this object's writes with conditional cleanup.
package sdk

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

const localConnectLocationFileName = ".connect_location"
const localDefaultLocationFileName = ".default_location"

// A nil result without error means a missing leaf in an accessible store.
// Unreadable, malformed, null and non-regular records are errors, not an
// instruction to disconnect, choose a default, or reset authentication.
func (self *LocalState) LoadConnectLocation() (*ConnectLocation, error) {
	self.authStateLock.Lock()
	defer self.authStateLock.Unlock()
	return self.loadLocationWithLock(localConnectLocationFileName)
}

// Uses the same absence/error contract for the separately saved default.
func (self *LocalState) LoadDefaultLocation() (*ConnectLocation, error) {
	self.authStateLock.Lock()
	defer self.authStateLock.Unlock()
	return self.loadLocationWithLock(localDefaultLocationFileName)
}

// Reads only; compatible nonempty objects and readable symlinks retain their
// existing meaning. No routing policy or current-account decision lives here.
func (self *LocalState) loadLocationWithLock(name string) (*ConnectLocation, error) {
	if self.ctx.Err() != nil {
		return nil, errors.New("location storage owner is closed")
	}
	path := filepath.Join(self.localStorageDir, name)
	info, err := os.Lstat(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("inspect saved location")
		}
		parent, parentErr := os.Stat(self.localStorageDir)
		if parentErr != nil || !parent.IsDir() {
			return nil, errors.New("saved location directory is unavailable")
		}
		return nil, nil
	}
	if info.Mode()&os.ModeSymlink != 0 {
		info, err = os.Stat(path)
		if err != nil {
			return nil, errors.New("resolve saved location")
		}
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("saved location is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("read saved location")
	}
	var location *ConnectLocation
	if err := json.Unmarshal(data, &location); err != nil || location == nil || location.ConnectLocationId == nil {
		return nil, errors.New("decode saved location")
	}
	return location, nil
}

// Uses the existing auth-envelope commit convention: private same-directory
// temporary file, complete write, fsync, close, rename, best-effort directory
// sync. The on-disk location schema and explicit nil-removal stay unchanged.
func (self *LocalState) setLocationWithLock(name string, location *ConnectLocation) error {
	var data []byte
	if location != nil {
		var err error
		data, err = json.Marshal(location)
		if err != nil {
			return errors.New("encode saved location")
		}
	}
	return self.writePreferenceBytesWithLock(name, data)
}

// Both callers supply fixed existing filenames. Nil means explicit removal;
// a complete nonnil byte record commits before live preference adoption.
func (self *LocalState) writePreferenceBytesWithLock(name string, data []byte) (returnErr error) {
	if self.ctx.Err() != nil {
		return errors.New("location storage owner is closed")
	}
	path := filepath.Join(self.localStorageDir, name)
	if data == nil {
		if err := os.Remove(path); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return localStorageStageError("remove saved location", err)
			}
			parent, parentErr := os.Stat(self.localStorageDir)
			if parentErr != nil || !parent.IsDir() {
				return errors.New("saved location directory is unavailable")
			}
		}
	} else {
		temp, err := os.CreateTemp(self.localStorageDir, name+".tmp-")
		if err != nil {
			return localStorageStageError("stage saved location", err)
		}
		tempPath := temp.Name()
		defer func() {
			_ = temp.Close()
			if returnErr != nil {
				_ = os.Remove(tempPath)
			}
		}()
		if err := temp.Chmod(LocalStorageFilePermissions); err != nil {
			return localStorageStageError("restrict saved location", err)
		}
		if _, err := temp.Write(data); err != nil {
			return localStorageStageError("write saved location", err)
		}
		if err := temp.Sync(); err != nil {
			return localStorageStageError("sync saved location", err)
		}
		if err := temp.Close(); err != nil {
			return localStorageStageError("close staged location", err)
		}
		beforeCommit := self.testingBeforePreferenceCommit
		if name == localConnectLocationFileName || name == localDefaultLocationFileName {
			beforeCommit = self.testingBeforeLocationCommit
		}
		if beforeCommit != nil {
			if err := beforeCommit(name); err != nil {
				return localStorageStageError("commit saved location", err)
			}
		}
		if err := os.Rename(tempPath, path); err != nil {
			return localStorageStageError("commit saved location", err)
		}
	}
	// Rename/removal has committed. Match the existing best-effort directory
	// sync convention rather than reporting a false uncommitted result.
	if runtime.GOOS != "windows" {
		if directory, err := os.Open(self.localStorageDir); err == nil {
			_ = directory.Sync()
			_ = directory.Close()
		}
	}
	return nil
}
