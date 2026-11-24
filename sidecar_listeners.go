//go:build !ios_extension && !js

// The network's listeners for the apps, each ip:port (description) (DESIGN-COMPUTE §15.3, §26). Created as a shell by
// CSURF-M11-01, with its type and its constructor; LSTN's tasks add its methods here, List, Refresh, Label, Hide,
// HideDevice, Map, AddListener and Confirm, each answering ErrSidecarComputeNotBuilt until it is built, never a nil, an
// empty success or a default, and none of them edits sidecar.go. The remote backend reaches the daemon through
// sidecarBackend.call, under method names that are LSTN's. A close method, once one is added, runs at the Sidecar's
// Close.

package sdk

// The network's listeners for the apps, each ip:port (description). Made once per Sidecar, at Listeners()'s first call;
// safe for concurrent use.
type SidecarListeners struct {
	// the Sidecar's ctx and backends, which its methods call
	args *sidecarComputeArgs
}

// The wrapper over args. It runs under the Sidecar's stateLock, so it only builds.
func newSidecarListeners(args *sidecarComputeArgs) *SidecarListeners {
	return &SidecarListeners{args: args}
}
