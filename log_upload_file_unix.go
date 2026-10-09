//go:build darwin || linux || android || ios

package sdk

import (
	"fmt"
	"math"
	"os"

	"golang.org/x/sys/unix"
)

// Opens the upload's own duplicate of a caller's file descriptor
// (UploadLogsFile), close-on-exec, which the upload closes. The caller's
// descriptor is never wrapped: os.NewFile closes the descriptor it wraps, also
// from a finalizer, and the caller still owns and closes its own.
func openUploadLogsFileDuplicate(fileDescriptor int64, name string) (*os.File, error) {
	if fileDescriptor < 0 || math.MaxInt32 < fileDescriptor {
		return nil, fmt.Errorf("not a file descriptor: %d", fileDescriptor)
	}
	duplicateFileDescriptor, err := unix.FcntlInt(uintptr(fileDescriptor), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(duplicateFileDescriptor), name), nil
}
