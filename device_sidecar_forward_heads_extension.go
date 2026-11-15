//go:build !js && ios_extension

// The Apple network extensions' forwarding role reads no config head yet: the root's head reader replays push records
// through evs files the ios_extension leg leaves out by tag (DESIGN-DETAILS 4.2.3), so the leg builds none until those
// reads move onto it (question sdk-m8-04-forwarding-heads-on-the-leg). Every base names a head, base 1's included,
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
