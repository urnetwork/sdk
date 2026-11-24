//go:build !ios_extension && !js

// The recURd skill's agents for the apps: what they are doing, their inboxes and the agent graph (DESIGN-COMPUTE §15.3,
// §20). Created as a shell by CSURF-M11-01, with its type and its constructor; SKILL's tasks add its methods here,
// List, Send, Broadcast, Inbox, Graph and AddListener, each answering ErrSidecarComputeNotBuilt until it is built,
// never a nil, an empty success or a default, and none of them edits sidecar.go. The remote backend reaches the daemon
// through sidecarBackend.call, under method names that are SKILL's. A close method, once one is added, runs at the
// Sidecar's Close.

package sdk

// The recURd skill's agents for the apps: what they are doing, their inboxes and the agent graph. Made once per
// Sidecar, at Agents()'s first call; safe for concurrent use.
type SidecarAgents struct {
	// the Sidecar's ctx and backends, which its methods call
	args *sidecarComputeArgs
}

// The wrapper over args. It runs under the Sidecar's stateLock, so it only builds.
func newSidecarAgents(args *sidecarComputeArgs) *SidecarAgents {
	return &SidecarAgents{args: args}
}
