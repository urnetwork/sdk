//go:build !windows

package sdk

import (
	"os"
)

// Off Windows a directory can be flushed, so no shipping platform selects
// this rename (peerPinDurableRenameFor). Only tests that stand in for Windows
// use it, as a plain rename.
func renamePeerPinFileWithoutDirectoryFlush(from string, to string) error {
	return os.Rename(from, to)
}
