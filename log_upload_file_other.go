//go:build !darwin && !linux && !android && !ios && !windows

package sdk

import (
	"fmt"
	"os"
)

// Platforms with no file descriptors to hand over (the browser) carry no other
// process's files: each is left out.
func openUploadLogsFileDuplicate(fileDescriptor int64, name string) (*os.File, error) {
	return nil, fmt.Errorf("no file descriptors on this platform")
}
