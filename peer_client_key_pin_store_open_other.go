//go:build !darwin && !linux && !android && !ios

package sdk

import "os"

// Desktop fallback: the caller verifies the leaf before and after opening.
// Its private directory must not be concurrently replaced by another process.
func openBoundedPeerPinFile(path string) (*os.File, error) {
	return os.Open(path)
}
