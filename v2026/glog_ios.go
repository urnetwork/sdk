//go:build ios
// +build ios

package sdk

import "path/filepath"

func defaultLogDir(home string) string {
	return filepath.Join(home, "Library", "Application Support", "URnetwork")
}
