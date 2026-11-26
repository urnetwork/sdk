//go:build js

// A browser runs no forwarding role (D-BRIDGE-11, device_sidecar_exit.go): the forwarding host's names on the js leg,
// which build none, so sdk.wasm links no sidecar root (plan-SDK-small-readings #7). The channel's calls refuse.

package sdk

import (
	"context"
	"net/http"

	"github.com/urnetwork/connect"

	sidecardevice "github.com/urnetwork/sidecar/device"
	"github.com/urnetwork/sidecar/protocol/bridge"
)

// No forwarding role on js.
type deviceLocalForwardingHost struct{}

// No tunables on js.
type deviceForwardingSettings struct{}

// What a forwarding host would be built over, kept for DeviceLocalSettings' shape.
type deviceForwardingDeps struct {
	ctx         context.Context
	apiUrl      string
	platformUrl string
	transport   http.RoundTripper
	hostClient  func() (clientId []byte, publicKey []byte)
}

// None on js.
func newDeviceLocalForwardingHost(settings *DeviceLocalSettings, networkId connect.Id, bridgeSettings *sidecardevice.BridgeSettings, log connect.Logger) *deviceLocalForwardingHost {
	return nil
}

func (self *deviceLocalForwardingHost) restore(owner *deviceLocalBridgeOwner) error {
	return nil
}

func (self *deviceLocalForwardingHost) close() {}

// No role's clock to check on js.
func (self *deviceLocalForwardingHost) checkClock() {}

func (self *deviceLocalForwardingHost) keys(networkId connect.Id) (*bridge.ForwardingKeys, error) {
	return nil, errDeviceBridgeBrowser
}

func (self *deviceLocalForwardingHost) handOff(handOff *bridge.ForwardingHandOff) error {
	return errDeviceBridgeBrowser
}

func (self *deviceLocalForwardingHost) witness(networkId connect.Id, pin *bridge.AuthPin, fork bool) error {
	return errDeviceBridgeBrowser
}

func (self *deviceLocalForwardingHost) status() []deviceForwardingStatus {
	return nil
}

// Nothing to start on js.
func (self *DeviceLocal) startForwarding() {}
