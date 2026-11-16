//go:build !ios_extension && !js && !darwin && !linux

// The free space a phone's all cap reads (§18.4's caps row): no phone class runs here, and a desktop class sets no cap,
// so nothing reads it; where a class with a cap does, the cap keeps no all.

package sdk

import (
	"errors"
)

// No statfs on this platform.
func sidecarFreeByteCount(dir string) (uint64, error) {
	return 0, errors.New("free space is not read on this platform")
}
