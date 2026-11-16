//go:build !ios_extension && !js && (darwin || linux)

// The free space a phone's all cap reads (§18.4's caps row; plan-SDK-small-readings #14): statfs on the state
// directory's volume, the bytes an unprivileged process may still write. GOOS ios and android build here, as darwin's
// and linux's.

package sdk

import (
	"syscall"
)

// The free bytes on dir's volume.
func sidecarFreeByteCount(dir string) (uint64, error) {
	stat := &syscall.Statfs_t{}
	if err := syscall.Statfs(dir, stat); err != nil {
		return 0, err
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}
