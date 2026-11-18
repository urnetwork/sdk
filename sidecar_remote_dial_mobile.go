//go:build !ios_extension && !js && ((ios && !macos) || android)

// Phones run the sidecar in the app's own process (§18.4): no `ur daemon` exists on iOS or Android, and the root links
// no DaemonClient there, so the remote backend has nothing to dial. GOOS=ios is a phone only without gomobile's macos
// tag: a macOS slice's binding step type-checks with GOOS=ios and macos, and is a desktop's, sidecar_remote_dial.go's
// (§18.10's tags; owner decision 493).

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
