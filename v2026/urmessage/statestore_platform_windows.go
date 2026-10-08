//go:build windows

package urmessage

import (
	"fmt"
	"io"
	"syscall"
)

// The single-writer exclusion on Windows: CreateFile on the guard entry with dwShareMode = 0.
//
// THIS IS A SECOND COPY OF sdk's acquireStreamStoreExclusion AND THE DUPLICATION IS DELIBERATE.
// The one in `sdk` is unexported, and the alternative is a new EXPORTED symbol on package `sdk` --
// which is the package gomobile binds and cgo re-exports, so a helper added there is a change to
// the mobile and C surfaces for the sake of one package's convenience. `urmessage` is bound by
// neither: `grep -rn urmessage --include=*.go sdk/cgo sdk/js sdk/build` answers nothing. **Filed
// as S2-25: one directory exclusion for the whole module, in a package both stores may import.**
// Until it is ruled, the discipline is copied character for character rather than re-derived, and
// the two files are meant to be compared.
//
// A share mode of zero is the whole mechanism. It refuses a second open of the same entry from
// ANY opener -- another process, or a second OpenDurableStateStore inside this one -- and the hold
// is the kernel's, so it is released by the handle closing and by the process dying and by nothing
// else. There is no stale-lock heuristic here and there must never be one: nothing is written into
// the guard file and nothing is read out of it.

const (
	// ERROR_SHARING_VIOLATION and ERROR_LOCK_VIOLATION. These two, and only these two, mean
	// "somebody else holds it". Every other failure is a filesystem this store could not read.
	stateWindowsSharingViolation = syscall.Errno(32)
	stateWindowsLockViolation    = syscall.Errno(33)
)

type stateStoreExclusion struct {
	handle syscall.Handle
}

func (self *stateStoreExclusion) Close() error {
	return syscall.CloseHandle(self.handle)
}

func acquireStateStoreExclusion(dir string) (io.Closer, error) {
	path := stateStoreGuardPath(dir)
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("%w: the guard entry %s is not a path this platform can name: %v",
			ErrStateStoreState, path, err)
	}
	handle, err := syscall.CreateFile(
		name,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0, // dwShareMode = 0: no second opener of any kind, in this process or another.
		nil,
		syscall.OPEN_ALWAYS,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		if err == stateWindowsSharingViolation || err == stateWindowsLockViolation {
			return nil, fmt.Errorf(
				"%w: the state directory %s is held by another DurableStateStore; two stores over one directory are two devices writing one device's mls state, and the later writer's epoch overwrites the earlier's",
				ErrStateStoreLocked, dir)
		}
		return nil, fmt.Errorf("%w: the guard entry %s could not be opened: %v", ErrStateStoreState, path, err)
	}
	return &stateStoreExclusion{handle: handle}, nil
}

// syncStateDir is A NO-OP ON WINDOWS, and what that costs is stated rather than hidden.
//
// There is no FlushFileBuffers on a directory handle: NTFS exposes no such object to flush, and a
// handle opened with FILE_FLAG_BACKUP_SEMANTICS answers ERROR_ACCESS_DENIED to it. So on this
// platform the rename that publishes a value is durable when the filesystem's metadata journal
// says it is, and this code does not control when that is.
//
// WHAT IS STILL TRUE HERE, and it is the half that matters: the VALUE was fsync'd before the
// rename, so no crash can make a half-written value observable. What a crash can lose on Windows
// is the whole rename -- the previous value survives, or the new one does. It cannot lose half of
// either. That is the same guarantee the POSIX path gives about tearing, and a weaker one about
// how recent the surviving value is.
func syncStateDir(dir string) error {
	return nil
}
