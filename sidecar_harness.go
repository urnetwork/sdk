//go:build !ios_extension && !js

// The Harness for the apps (DESIGN-COMPUTE §15.3, §7): a Harness on the Sidecar, whose output is a stream of typed
// events the apps render as components. Created as a shell by CSURF-M11-01, with its types and its constructor; HARN's
// tasks fill the constructor and add SidecarHarness's methods here, NewAgent, Prompt, Steer, FollowUp, Abort, SetModel,
// Close, Attach, Harnesses, Manage, Launch, Watch, Unmanage, AddListener, RespondUi, Runs and Continue, none of them
// editing sidecar.go. Until then NewHarness answers ErrSidecarComputeNotBuilt, never a nil Harness with no error. The
// remote backend reaches the daemon through sidecarBackend.call, under method names that are HARN's.

package sdk

// One Harness and its agents. Safe for concurrent use.
type SidecarHarness struct {
	// the Sidecar's ctx and backends, which its methods call
	args *sidecarComputeArgs
}

// A Harness's events, each one of §7.6's typed events as JSON.
type SidecarHarnessListener interface {
	OnEvent(eventJson string)
}

// A Harness on args with settingsJson's settings: ErrSidecarComputeNotBuilt until HARN builds it.
func newSidecarHarness(args *sidecarComputeArgs, settingsJson string) (*SidecarHarness, error) {
	return nil, ErrSidecarComputeNotBuilt
}
