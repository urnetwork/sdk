// The device role's posture store (§18.6's stored row, §17.8's persist row): the network process's own store, so one
// directory per network under the NetworkSpace's storage path, <storagePath>/.bridge_posture/<network_id>. That is
// beside LocalState's .by and never inside it, since a sign-out empties .by, and a store emptied by anything but this
// sidecar's own SidecarRevoke would reopen a LAN (DP1). It is never the sidecar role's StateDir either, which on iOS
// is the app's and out of the tunnel's reach (§18.4).
//
// The records are device.NewFilePostureStore's: temp, fsync, rename, so a kill leaves the old record or the new one,
// and each streams both ways, so a base's 4 MiB of rows never sits whole in the tunnel's 1 MiB growth (R-SDK-6). On
// top of it this makes the first write's directories durable in their parents, the network's directory and
// .bridge_posture, so a power cut after a commit cannot drop the posture with a directory entry.
//
// Absent means only that no record was ever written. A directory that exists and cannot be read gives a store whose
// every call fails as device.ErrPostureStoreFailed, which a posture restores as pending with an alarm, never legacy,
// so a damaged store denies rather than reopens.
//
// Safe for concurrent use.

package sdk

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/urnetwork/connect"

	sidecardevice "github.com/urnetwork/sidecar/device"
)

// The directory under a NetworkSpace's storage path holding each network's posture.
const sdkPostureDirectoryName = ".bridge_posture"

// One network's posture records.
type sdkPostureStore struct {
	// the parent of directoryPath, made with it at the first write
	postureRootPath string
	directoryPath   string
	// the file store, or one failing every call when the directory could not be opened
	store sidecardevice.PostureStore

	stateLock sync.Mutex
	// whether the first write has made the directories and their entries durable
	directoryMade bool
}

var _ sidecardevice.PostureStore = (*sdkPostureStore)(nil)

// The posture store of networkId under storagePath, an absolute directory. Makes nothing until the first write, so a
// device that never leaves legacy leaves no trace. A directory that exists and cannot be opened gives a store that
// fails every call.
func newSdkPostureStore(storagePath string, networkId connect.Id) (*sdkPostureStore, error) {
	if storagePath == "" || !filepath.IsAbs(storagePath) {
		return nil, errors.New("the device role keeps its posture under the network space's storage path, which is not an absolute directory")
	}
	postureRootPath := filepath.Join(storagePath, sdkPostureDirectoryName)
	directoryPath := filepath.Join(postureRootPath, networkId.String())
	store, err := sidecardevice.NewFilePostureStore(directoryPath)
	if err != nil {
		store = &failedPostureStore{err: err}
	}
	return &sdkPostureStore{
		postureRootPath: postureRootPath,
		directoryPath:   directoryPath,
		store:           store,
	}, nil
}

// Streams the record; device.ErrPostureStoreAbsent only when no such record exists.
func (self *sdkPostureStore) Open(name string) (io.ReadCloser, error) {
	return self.store.Open(name)
}

// Replaces the record whole and durably, streaming what write writes. The first write makes the directories first,
// each entry durable in its parent.
func (self *sdkPostureStore) Write(name string, write func(writer io.Writer) error) error {
	if err := self.makeDirectory(); err != nil {
		return err
	}
	return self.store.Write(name, write)
}

// Takes the record away; absent is not an error.
func (self *sdkPostureStore) Remove(name string) error {
	return self.store.Remove(name)
}

// The names of the records held, sorted.
func (self *sdkPostureStore) List() ([]string, error) {
	return self.store.List()
}

// Makes .bridge_posture and the network's directory, 0700, with each new entry fsynced in its parent, once.
func (self *sdkPostureStore) makeDirectory() error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.directoryMade {
		return nil
	}
	for _, path := range []string{self.postureRootPath, self.directoryPath} {
		err := os.Mkdir(path, LocalStorageDirectoryPermissions)
		if errors.Is(err, fs.ErrExist) {
			info, statErr := os.Stat(path)
			if statErr != nil {
				return sdkPostureStoreFault(statErr)
			}
			if !info.IsDir() {
				return fmt.Errorf("%w: the posture's path is not a directory", sidecardevice.ErrPostureStoreFailed)
			}
			continue
		}
		if err != nil {
			return sdkPostureStoreFault(err)
		}
		if err := syncSdkPostureDirectory(filepath.Dir(path)); err != nil {
			return err
		}
	}
	self.directoryMade = true
	return nil
}

// The directory under storagePath this store keeps its records in.
func (self *sdkPostureStore) directory() string {
	return self.directoryPath
}

// fsyncs a directory. Windows opens no directory for fsync, and NTFS journals the entry itself.
func syncSdkPostureDirectory(directoryPath string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(directoryPath)
	if err != nil {
		return sdkPostureStoreFault(err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return sdkPostureStoreFault(err)
	}
	return nil
}

// A filesystem error as a store failure with its bare cause, since an os error carries its path (I-13).
func sdkPostureStoreFault(err error) error {
	for cause := errors.Unwrap(err); cause != nil; cause = errors.Unwrap(err) {
		err = cause
	}
	return fmt.Errorf("%w: %v", sidecardevice.ErrPostureStoreFailed, err)
}

// A store whose directory could not be opened: every call fails, never absent, so the posture restores as pending.
type failedPostureStore struct {
	err error
}

var _ sidecardevice.PostureStore = (*failedPostureStore)(nil)

// Fails: the record may be there and cannot be read.
func (self *failedPostureStore) Open(name string) (io.ReadCloser, error) {
	return nil, self.fault()
}

// Fails, writing nothing.
func (self *failedPostureStore) Write(name string, write func(writer io.Writer) error) error {
	return self.fault()
}

// Fails, removing nothing.
func (self *failedPostureStore) Remove(name string) error {
	return self.fault()
}

// Fails: what the store holds cannot be read.
func (self *failedPostureStore) List() ([]string, error) {
	return nil, self.fault()
}

// The open's failure, never ErrPostureStoreAbsent.
func (self *failedPostureStore) fault() error {
	if errors.Is(self.err, sidecardevice.ErrPostureStoreFailed) && !errors.Is(self.err, sidecardevice.ErrPostureStoreAbsent) {
		return self.err
	}
	return fmt.Errorf("%w: the posture's directory could not be opened", sidecardevice.ErrPostureStoreFailed)
}
