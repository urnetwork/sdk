//go:build android

// Android-only gomobile surface for bounded, caller-triggered failure evidence.
package sdk

// Writes every Go goroutine stack to a private file. The caller supplies a path
// in its private diagnostics directory and owns the resulting
// implementation-detail artifact.
func WriteGoroutineStacks(path string) error {
	return writeGoroutineStacks(path)
}
