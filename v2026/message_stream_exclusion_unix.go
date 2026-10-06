// THE CONSTRAINT IS THE GOOS SET ON WHICH syscall.Flock IS DECLARED, spelled out as terms, and it
// is deliberately NOT the `unix` build term.
//
// go/build's `unix` term covers aix, android, darwin, dragonfly, freebsd, hurd, illumos, ios,
// linux, netbsd, openbsd and solaris. syscall.Flock is NOT declared on solaris or on aix -- read
// off this toolchain's own syscall package, where Flock appears in syscall_illumos.go and in the
// zsyscall files for darwin, dragonfly, freebsd, linux, netbsd and openbsd, and in none for
// solaris or aix. Constraining this file with `unix` therefore does not make those two platforms
// fall through to the fail-closed file: it makes them a BUILD BREAK, `undefined: syscall.Flock`,
// on a platform the fail-closed file was written to refuse on -- and a build that does not
// compile never runs the gate that would have said so.
//
// android is covered by the linux term and ios by the darwin term, which is how the Go toolchain
// spells those two GOOS values' constraints. The complement of this list, plus windows, is the
// fallback file's constituency: solaris, aix, js, wasip1 and plan9.

//go:build (darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd) && !sdk_mobile_bind

package sdk

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

// The single-writer exclusion where syscall.Flock exists: LOCK_EX|LOCK_NB on the guard entry.
//
// A BSD flock is held by the OPEN FILE DESCRIPTION, not by the process, so two separate opens of
// the same entry conflict even inside one process -- which is the in-process half of the class
// this exclusion has to cover. The kernel releases it when the descriptor closes and when the
// process dies, so there is no stale-lock heuristic here and there must never be one: nothing is
// written into the guard file, nothing is read out of it, and no pid, timestamp or file age is
// consulted.

type streamStoreExclusion struct {
	file *os.File
}

func (self *streamStoreExclusion) Close() error {
	// The close releases the lock; unlocking first would leave a window in which the
	// descriptor is open and the lock is not held.
	return self.file.Close()
}

func acquireStreamStoreExclusion(dir string) (io.Closer, error) {
	path := streamStoreGuardPath(dir)
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf(
			"%w: the guard entry %s could not be opened: %v",
			ErrStreamStoreState,
			path,
			err,
		)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return nil, fmt.Errorf(
				"%w: the store directory %s is held by %s; two stores over one directory each read the same persisted high water and each allocate the same next index, which spec A section 5.6 calls a total break of both AEADs for that record",
				ErrStreamStoreLocked,
				dir,
				streamStoreExclusionHolder(dir),
			)
		}
		return nil, fmt.Errorf(
			"%w: the guard entry %s could not be locked: %v",
			ErrStreamStoreState,
			path,
			err,
		)
	}
	return &streamStoreExclusion{file: file}, nil
}
