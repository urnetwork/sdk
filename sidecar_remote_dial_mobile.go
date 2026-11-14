//go:build !ios_extension && !js && (ios || android)

// Phones run the sidecar in the app's own process (§18.4): no `ur daemon` exists on iOS or Android, and the root links
// no DaemonClient there, so the remote backend has nothing to dial.

package sdk

import (
	"errors"
)

// NewSidecarRemote on a leg with no daemon.
var errSidecarRemoteUnsupported = errors.New("a remote-mode sidecar needs ur daemon, which runs on desktops alone")

// Refuses: this leg has no control socket.
func sidecarDaemonControlDial(settings *SidecarSettings) (sidecarControlDial, error) {
	return nil, errSidecarRemoteUnsupported
}
