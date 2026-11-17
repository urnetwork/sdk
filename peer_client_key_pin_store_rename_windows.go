//go:build windows

package sdk

import (
	"golang.org/x/sys/windows"
)

// Replaces `to` with the complete file at `from`, and returns only once the
// move is on disk (MOVEFILE_WRITE_THROUGH). This is how a rename is made
// durable on Windows: a directory handle there can never be flushed, since
// FlushFileBuffers needs GENERIC_WRITE, which a directory handle cannot have.
func renamePeerPinFileWithoutDirectoryFlush(from string, to string) error {
	fromPath, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	toPath, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(fromPath, toPath, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
