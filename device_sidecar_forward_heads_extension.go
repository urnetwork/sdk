//go:build !js && ios_extension

// The Apple network extensions' forwarding role reads no config head yet: the root's head reader replays push records
// through evs files the ios_extension leg leaves out by tag (DESIGN-DETAILS 4.2.3), so the leg builds none until those
// reads move onto it (question sdk-m8-04-forwarding-heads-on-the-leg). A network past its first base then never
// compiles in these extensions: its posture stays as its last verified state or pending, and denials still apply, the
// closed direction (QD-621).

package sdk

import (
	"sync/atomic"

	"github.com/urnetwork/sidecar"
)

// None on this leg (the file's comment).
func deviceForwardConfigHeads(role *deviceLocalForwarding, client *sidecar.ForwardingClient, sidecarId sidecar.Id, configRepoId sidecar.Id, forwarderSettings *sidecar.ForwarderSettings, running *atomic.Pointer[sidecar.Forwarder]) (sidecar.ConfigHeadReader, error) {
	return nil, nil
}
