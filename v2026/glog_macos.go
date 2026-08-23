//go:build darwin && !ios
// +build darwin,!ios

package sdk

import "path/filepath"

func defaultLogDir(home string) string {
	return filepath.Join(home, "Library", "Logs", "URnetwork")
}
