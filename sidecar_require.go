//go:build tools

// Holds sidecar in this module's graph until sdk/sidecar.go imports it for real (§4.7). go mod tidy
// reads every build tag, so this import keeps the require here and in the sdk/build, sdk/cgo and
// sdk/js modules that build this package, and no build sets the tag, so no leg links sidecar yet.
// Delete this file in the change that adds the first real import.
package sdk

import (
	_ "github.com/urnetwork/sidecar"
)
