//go:build android || ios

package sdk

import (
	"os"
)

// on mobile the platform convention is for diagnostics to go to stdout
func redirectStderrForPlatform() {
	os.Stderr = os.Stdout
}
