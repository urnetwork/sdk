//go:build !ios_extension && !js

// The network's enclaves for the apps (DESIGN-COMPUTE §15.3, §11). Created as a shell by CSURF-M11-01, with its type
// and its constructor; ENC's tasks add its methods here, List, Create, Stop, Start, Delete, Egress, Keeper,
// GrantMailbox, Held and Release, each answering ErrSidecarComputeNotBuilt until it is built, never a nil, an empty
// success or a default, and none of them edits sidecar.go. The remote backend reaches the daemon through
// sidecarBackend.call, under method names that are ENC's. A close method, once one is added, runs at the Sidecar's
// Close.

package sdk

// The network's enclaves for the apps. Made once per Sidecar, at Enclaves()'s first call; safe for concurrent use.
type SidecarEnclaves struct {
	// the Sidecar's ctx and backends, which its methods call
	args *sidecarComputeArgs
}

// The wrapper over args. It runs under the Sidecar's stateLock, so it only builds.
func newSidecarEnclaves(args *sidecarComputeArgs) *SidecarEnclaves {
	return &SidecarEnclaves{args: args}
}
