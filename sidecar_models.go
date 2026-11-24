//go:build !ios_extension && !js

// The model catalog for the apps: canonical names and labels, local model pulls and the person's models key (DESIGN-
// COMPUTE §15.3, §6, §16.4). Created as a shell by CSURF-M11-01, with its type and its constructor; INFER's tasks add
// its methods here, Catalog, Recommended, Pull, Remove, Settings and SetSettings, each answering
// ErrSidecarComputeNotBuilt until it is built, never a nil, an empty success or a default, and none of them edits
// sidecar.go. The remote backend reaches the daemon through sidecarBackend.call, under method names that are INFER's. A
// close method, once one is added, runs at the Sidecar's Close.

package sdk

// The model catalog for the apps: canonical names and labels, local model pulls and the person's models key. Made once
// per Sidecar, at Models()'s first call; safe for concurrent use.
type SidecarModels struct {
	// the Sidecar's ctx and backends, which its methods call
	args *sidecarComputeArgs
}

// The wrapper over args. It runs under the Sidecar's stateLock, so it only builds.
func newSidecarModels(args *sidecarComputeArgs) *SidecarModels {
	return &SidecarModels{args: args}
}
