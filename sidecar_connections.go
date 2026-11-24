//go:build !ios_extension && !js

// The member's connected services for the apps, never a token (DESIGN-COMPUTE §15.3, §25). Created as a shell by
// CSURF-M11-01, with its type and its constructor; CONN's tasks add its methods here, Services, List, Connect,
// AddFeature, Disconnect, Answer and UseHere, each answering ErrSidecarComputeNotBuilt until it is built, never a nil,
// an empty success or a default, and none of them edits sidecar.go. The remote backend reaches the daemon through
// sidecarBackend.call, under method names that are CONN's. A close method, once one is added, runs at the Sidecar's
// Close.

package sdk

// The member's connected services for the apps, never a token. Made once per Sidecar, at Connections()'s first call;
// safe for concurrent use.
type SidecarConnections struct {
	// the Sidecar's ctx and backends, which its methods call
	args *sidecarComputeArgs
}

// The wrapper over args. It runs under the Sidecar's stateLock, so it only builds.
func newSidecarConnections(args *sidecarComputeArgs) *SidecarConnections {
	return &SidecarConnections{args: args}
}
