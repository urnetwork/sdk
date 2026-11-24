//go:build !ios_extension && !js

// The person's calendar links and events for the apps, never a link's secret (DESIGN-COMPUTE §15.3, §23). Created as a
// shell by CSURF-M11-01, with its type and its constructor; MAILCAL's tasks add its methods here, Links, SetLink,
// Connect, RemoveLink, List, Undo and Reconcile, each answering ErrSidecarComputeNotBuilt until it is built, never a
// nil, an empty success or a default, and none of them edits sidecar.go. The remote backend reaches the daemon through
// sidecarBackend.call, under method names that are MAILCAL's. No area's sdk family names this file; the planner gives
// it to MAILCAL. A close method, once one is added, runs at the Sidecar's Close.

package sdk

// The person's calendar links and events for the apps, never a link's secret. Made once per Sidecar, at Calendar()'s
// first call; safe for concurrent use.
type SidecarCalendar struct {
	// the Sidecar's ctx and backends, which its methods call
	args *sidecarComputeArgs
}

// The wrapper over args. It runs under the Sidecar's stateLock, so it only builds.
func newSidecarCalendar(args *sidecarComputeArgs) *SidecarCalendar {
	return &SidecarCalendar{args: args}
}
