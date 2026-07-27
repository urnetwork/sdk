//go:build !android && !ios

package sdk

// on unix-convention platforms stderr stays stderr, so binaries that emit
// data on stdout (sim-latency CSV/json, piped tools) stay clean
func redirectStderrForPlatform() {
}
