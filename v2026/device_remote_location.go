// Checked current-location observation for native startup decisions. The
// compatibility getter keeps its cache/fallback behavior; this path observes
// one live RPC generation and never writes cached or pending preferences.
package sdk

import "errors"

var (
	errDeviceRemoteLocationUnavailable = errors.New("device remote current location is unavailable")
	errDeviceRemoteLocationReadFailed  = errors.New("device remote current location request failed")
	errDeviceRemoteLocationInvalid     = errors.New("device remote current location response is invalid")
	errDeviceRemoteLocationSuperseded  = errors.New("device remote changed while reading current location")
)

// Returns the current location from the existing RPC operation, without a
// pending/last-known fallback. A successful nil means no current display
// location, not disconnected transport or absent durable preferences.
//
// Calls run outside the state lock. Only the same live service can admit the
// reply; this is an observation, not a lease across a later connect action.
// Browser-only remotes cannot perform synchronous websocket observations.
func (self *DeviceRemote) GetConnectLocationChecked() (*ConnectLocation, error) {
	service := func() *rpcClient {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed || (self.ctx != nil && self.ctx.Err() != nil) ||
			(self.settings != nil && self.settings.BrowserStateOnly) {
			return nil
		}
		if self.service == nil || (self.service.ctx != nil && self.service.ctx.Err() != nil) {
			return nil
		}
		return self.service
	}()
	if service == nil {
		return nil, errDeviceRemoteLocationUnavailable
	}

	reply, err := rpcCallNoArg[*DeviceRemoteConnectLocation](
		service,
		"DeviceLocalRpc.GetConnectLocation",
		func() { self.closeServiceInstance(service) },
	)
	if err != nil {
		// Do not export arbitrary peer/transport diagnostics to native UI.
		return nil, errDeviceRemoteLocationReadFailed
	}
	if reply == nil {
		return nil, errDeviceRemoteLocationInvalid
	}
	location := reply.toConnectLocation()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed || (self.ctx != nil && self.ctx.Err() != nil) || self.service != service ||
		(service.ctx != nil && service.ctx.Err() != nil) {
		return nil, errDeviceRemoteLocationSuperseded
	}
	return location, nil
}
