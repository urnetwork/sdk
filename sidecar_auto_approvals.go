//go:build !ios_extension && !js

// The person's automatic approvals for the apps: the mode, the network's limit and the day's digest (DESIGN-COMPUTE
// §15.3, §27). Created as a shell by CSURF-M11-01, with its type and its constructor; SAFE's tasks add its methods
// here, Settings, SetSettings and Digest, each answering ErrSidecarComputeNotBuilt until it is built, never a nil, an
// empty success or a default, and none of them edits sidecar.go. The remote backend reaches the daemon through
// sidecarBackend.call, under method names that are SAFE's. No area's sdk family names this file; the planner gives it
// to SAFE. A close method, once one is added, runs at the Sidecar's Close.

package sdk

// The person's automatic approvals for the apps: the mode, the network's limit and the day's digest. Made once per
// Sidecar, at AutoApprovals()'s first call; safe for concurrent use.
type SidecarAutoApprovals struct {
	// the Sidecar's ctx and backends, which its methods call
	args *sidecarComputeArgs
}

// The wrapper over args. It runs under the Sidecar's stateLock, so it only builds.
func newSidecarAutoApprovals(args *sidecarComputeArgs) *SidecarAutoApprovals {
	return &SidecarAutoApprovals{args: args}
}
