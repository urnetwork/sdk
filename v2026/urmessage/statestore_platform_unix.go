// THE CONSTRAINT IS THE GOOS SET ON WHICH syscall.Flock IS DECLARED, spelled out as terms, and it
// is deliberately NOT the `unix` build term -- for the reason sdk/message_stream_exclusion_unix.go
// gives at length. go/build's `unix` term covers solaris and aix, where syscall.Flock is not
// declared, so constraining this file with it would turn those two platforms into a BUILD BREAK
// rather than letting them fall through to the fail-closed file that refuses on them. android is
// covered by linux and ios by darwin.

//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd

package urmessage

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

// The single-writer exclusion where syscall.Flock exists: LOCK_EX|LOCK_NB on the guard entry.
//
// THIS IS A SECOND COPY OF sdk's acquireStreamStoreExclusion; see the windows file's header for
// why the duplication was chosen over exporting a helper from package `sdk`, and for S2-25.
//
// A BSD flock is held by the OPEN FILE DESCRIPTION, not by the process, so two separate opens of
// the same entry conflict even inside one process. The kernel releases it when the descriptor
// closes and when the process dies, so there is no stale-lock heuristic here and there must never
// be one.

type stateStoreExclusion struct {
	file *os.File
}

func (self *stateStoreExclusion) Close() error {
	// The close releases the lock; unlocking first would leave a window in which the descriptor
	// is open and the lock is not held.
	return self.file.Close()
}

func acquireStateStoreExclusion(dir string) (io.Closer, error) {
	path := stateStoreGuardPath(dir)
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("%w: the guard entry %s could not be opened: %v", ErrStateStoreState, path, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return nil, fmt.Errorf(
				"%w: the state directory %s is held by another DurableStateStore; two stores over one directory are two devices writing one device's mls state, and the later writer's epoch overwrites the earlier's",
				ErrStateStoreLocked, dir)
		}
		return nil, fmt.Errorf("%w: the guard entry %s could not be locked: %v", ErrStateStoreState, path, err)
	}
	return &stateStoreExclusion{file: file}, nil
}

// syncStateDir fsyncs a directory, which is what makes a rename into it durable.
//
// It is the half of the write that Windows cannot perform; see that file's syncStateDir for what
// the absence costs there. Here the failure is RETURNED, because a rename this call could not
// flush is a value the store has reported durable and the filesystem has not.
func syncStateDir(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("%w: %s could not be opened to be flushed: %v", ErrStateStoreState, dir, err)
	}
	defer handle.Close()
	if err := handle.Sync(); err != nil {
		return fmt.Errorf("%w: %s could not be flushed, so a value this store named may not survive a crash: %v",
			ErrStateStoreState, dir, err)
	}
	return nil
}
