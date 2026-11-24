//go:build !ios_extension && !js

// The network's compute balance for the apps: its credits, purchases, auto-renew, budgets and where credits went
// (DESIGN-COMPUTE §15.3, §9). Created as a shell by CSURF-M11-01, with its type and its constructor; BAL's tasks add
// its methods here, Balance, Packs, Charges, Checkout, StorePurchase, Redeem, AutoRenew, RaiseBudget, Spend, Terms and
// AcceptTerms, each answering ErrSidecarComputeNotBuilt until it is built, never a nil, an empty success or a default,
// and none of them edits sidecar.go. The remote backend reaches the daemon through sidecarBackend.call, under method
// names that are BAL's. A close method, once one is added, runs at the Sidecar's Close.

package sdk

// The network's compute balance for the apps: its credits, purchases, auto-renew, budgets and where credits went. Made
// once per Sidecar, at Compute()'s first call; safe for concurrent use.
type SidecarCompute struct {
	// the Sidecar's ctx and backends, which its methods call
	args *sidecarComputeArgs
}

// The wrapper over args. It runs under the Sidecar's stateLock, so it only builds.
func newSidecarCompute(args *sidecarComputeArgs) *SidecarCompute {
	return &SidecarCompute{args: args}
}
