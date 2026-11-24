//go:build !ios_extension && !js

// The bundled SSH's terminal sessions for the apps (DESIGN-COMPUTE §15.3, §12.8): OpenSsh's session, which feeds a
// terminal component, and its listener. Created as a shell by CSURF-M11-01, with its types and its function; SSH's
// tasks fill openSidecarSsh and add SidecarSshSession's methods here (SSH-M14-07: Write, Resize and Close), none of
// them editing sidecar.go. No area's sdk family names this file; the planner gives it to SSH. Until then OpenSsh
// answers ErrSidecarComputeNotBuilt, never a nil session with no error.

package sdk

// One terminal session over the bundled SSH. Safe for concurrent use.
type SidecarSshSession struct {
	// the Sidecar's ctx and backends, which its methods call
	args *sidecarComputeArgs
}

// A session's output and its close, each as one JSON event, as the Harness's and local inference's listeners take
// theirs; SSH-M14-07 settles the events' fields.
type SidecarSshListener interface {
	OnEvent(eventJson string)
}

// A session to host as user, cols by rows, its output to listener: ErrSidecarComputeNotBuilt until SSH builds it.
func openSidecarSsh(args *sidecarComputeArgs, host string, user string, cols int, rows int, listener SidecarSshListener) (*SidecarSshSession, error) {
	return nil, ErrSidecarComputeNotBuilt
}
