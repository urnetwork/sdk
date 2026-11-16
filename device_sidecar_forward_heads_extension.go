//go:build !js && ios_extension

// The Apple network extensions' forwarding role reads no config head yet. The root's head reader compiles on the
// ios_extension leg since its push record reads moved onto it (QD-839), but with it the forwarding leg passes
// DESIGN-DETAILS 4.2.3's 6 MiB budget (7,009,216 B added, stripped), so the reader stays tagged off the leg until
// question ios-leg-head-reader-size-over-budget is answered. Every base names a head, base 1's included,
// so nothing compiles in these extensions: a role's posture stays pending after its hand-off, or as a restored state
// left it, and the denials it checks still apply, the closed direction (QD-621).

package sdk

import (
	"sync/atomic"

	"github.com/urnetwork/sidecar"
)

// None on this leg (the file's comment).
func deviceForwardConfigHeads(role *deviceLocalForwarding, client *sidecar.ForwardingClient, sidecarId sidecar.Id, configRepoId sidecar.Id, forwarderSettings *sidecar.ForwarderSettings, running *atomic.Pointer[sidecar.Forwarder]) (sidecar.ConfigHeadReader, error) {
	return nil, nil
}
