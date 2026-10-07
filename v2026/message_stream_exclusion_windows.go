//go:build windows && !sdk_mobile_bind

package sdk

import (
	"fmt"
	"io"
	"syscall"
)

// The single-writer exclusion on Windows: CreateFile on the guard entry with dwShareMode = 0.
//
// A share mode of zero is the whole mechanism. It refuses a second open of the same entry from
// ANY opener -- another process, or a second OpenStreamStore inside this one -- and the hold is
// the kernel's, so it is released by the handle closing and by the process dying and by nothing
// else. There is no stale-lock heuristic here and there must never be one: nothing is written
// into the guard file, nothing is read out of it, and no pid, timestamp or file age is consulted.
//
// Measured on this machine with this toolchain: a second CreateFile with dwShareMode = 0 against
// a held entry answers ERROR_SHARING_VIOLATION, and after CloseHandle a third acquire succeeds.

const (
	// ERROR_SHARING_VIOLATION and ERROR_LOCK_VIOLATION. These two, and only these two, mean
	// "somebody else holds it". Every other failure is a filesystem the store could not read,
	// which is ErrStreamStoreState and not a lock.
	windowsSharingViolation = syscall.Errno(32)
	windowsLockViolation    = syscall.Errno(33)
)

type streamStoreExclusion struct {
	handle syscall.Handle
	path   string
}

func (self *streamStoreExclusion) Close() error {
	return syscall.CloseHandle(self.handle)
}

func acquireStreamStoreExclusion(dir string) (io.Closer, error) {
	path := streamStoreGuardPath(dir)
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf(
			"%w: the guard entry %s is not a path this platform can name: %v",
			ErrStreamStoreState,
			path,
			err,
		)
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
		if err == windowsSharingViolation || err == windowsLockViolation {
			return nil, fmt.Errorf(
				"%w: the store directory %s is held by %s; two stores over one directory each read the same persisted high water and each allocate the same next index, which spec A section 5.6 calls a total break of both AEADs for that record",
				ErrStreamStoreLocked,
				dir,
				streamStoreExclusionHolder(dir),
			)
		}
		return nil, fmt.Errorf(
			"%w: the guard entry %s could not be opened: %v",
			ErrStreamStoreState,
			path,
			err,
		)
	}
	return &streamStoreExclusion{handle: handle, path: path}, nil
}
