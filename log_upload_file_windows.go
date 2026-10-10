//go:build windows

package sdk

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// Opens the upload's own duplicate of a caller's file handle (UploadLogsFile),
// not inheritable and with the same access, which the upload closes. The
// caller's handle is never wrapped: os.NewFile closes the handle it wraps,
// also from a finalizer, and the caller still owns and closes its own.
//
// The handle must be one of this process (the service opens the app's files
// itself, while impersonating the app), opened for synchronous reads.
func openUploadLogsFileDuplicate(fileDescriptor int64, name string) (*os.File, error) {
	if fileDescriptor <= 0 {
		return nil, fmt.Errorf("not a file handle: %d", fileDescriptor)
	}
	process := windows.CurrentProcess()
	var duplicateHandle windows.Handle
	err := windows.DuplicateHandle(
		process,
		windows.Handle(fileDescriptor),
		process,
		&duplicateHandle,
		0,
		false,
		windows.DUPLICATE_SAME_ACCESS,
	)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(duplicateHandle), name), nil
}
