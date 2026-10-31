//go:build darwin || linux || android || ios

package sdk

import (
	"os"

	"golang.org/x/sys/unix"
)

func openBoundedPeerPinFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
