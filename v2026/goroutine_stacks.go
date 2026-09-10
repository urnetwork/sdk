// Go goroutine stack capture shared by the Android binding and host tests.
// The buffer grows until runtime.Stack confirms that one complete snapshot fit.
package sdk

import (
	"io"
	"os"
	"runtime"
)

// Initial capacity avoids repeated snapshots in ordinary mobile processes.
const initialGoroutineStackBufferByteCount = 64 * 1024

// Writes one textual snapshot of every Go goroutine to a caller-owned
// diagnostic path. The destination is private before stack data is written,
// including when it already existed with broader permissions.
func writeGoroutineStacks(path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}

	stackBytes := make([]byte, initialGoroutineStackBufferByteCount)
	for {
		stackByteCount := runtime.Stack(stackBytes, true)
		if stackByteCount < len(stackBytes) {
			writtenByteCount, writeErr := file.Write(stackBytes[:stackByteCount])
			if writeErr == nil && writtenByteCount != stackByteCount {
				writeErr = io.ErrShortWrite
			}
			closeErr := file.Close()
			if writeErr != nil {
				return writeErr
			}
			return closeErr
		}
		stackBytes = make([]byte, 2*len(stackBytes))
	}
}
