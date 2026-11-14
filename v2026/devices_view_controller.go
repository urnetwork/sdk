//go:build !ios_extension

package sdk

import (
	"context"
	"slices"

	"github.com/urnetwork/connect/v2026"
)

type NetworkClientsListener interface {
	NetworkClientsChanged(networkClients *NetworkClientInfoList)
}

type DevicesViewController struct {
	ctx    context.Context
	cancel context.CancelFunc

	device Device
	// an api-only controller (NewDevicesViewControllerWithApi) has no device:
	// the list needs nothing but the network space api. Exactly one of
	// device / api is set.
	api *Api

	networkClientsListeners *connect.CallbackList[NetworkClientsListener]
}

func newDevicesViewController(ctx context.Context, device Device) *DevicesViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &DevicesViewController{
		ctx:                     cancelCtx,
		cancel:                  cancel,
		device:                  device,
		networkClientsListeners: connect.NewCallbackList[NetworkClientsListener](),
	}
	return vc
}

// NewDevicesViewControllerWithApi opens the devices list over an api with no
// device (a host that holds a network member jwt but no device yet). Without a
// device there is no "this device" to float to the top: ClientId is nil.
func NewDevicesViewControllerWithApi(ctx context.Context, api *Api) *DevicesViewController {
	vc := newDevicesViewController(ctx, nil)
	vc.api = api
	return vc
}

func (self *DevicesViewController) getApi() *Api {
	if self.api != nil {
		return self.api
	}
	return self.device.GetApi()
}

func (self *DevicesViewController) ClientId() *Id {
	if self.device == nil {
		return nil
	}
	return self.device.GetClientId()
}

func (self *DevicesViewController) Start() {
	// FIXME

	// request clients
	self.getApi().GetNetworkClients(GetNetworkClientsCallback(connect.NewApiCallback[*NetworkClientsResult](
		func(result *NetworkClientsResult, err error) {
			if err == nil {
				self.networkClientsChanged(self.networkClientsFromResult(result))
			}
		},
	)))
}

// Converts a successful API response into the sorted, non-nil collection the
// devices view publishes. Older API builds encode an empty slice as JSON null.
func (self *DevicesViewController) networkClientsFromResult(result *NetworkClientsResult) *NetworkClientInfoList {
	networkClients := []*NetworkClientInfo{}
	if result != nil && result.Clients != nil {
		for i := 0; i < result.Clients.Len(); i += 1 {
			networkClients = append(networkClients, result.Clients.Get(i))
		}
	}
	slices.SortStableFunc(networkClients, self.cmpNetworkClientLayout)

	exportedNetworkClients := NewNetworkClientInfoList()
	exportedNetworkClients.addAll(networkClients...)
	return exportedNetworkClients
}

func (self *DevicesViewController) Stop() {
	// FIXME
}

func (self *DevicesViewController) AddNetworkClientsListener(listener NetworkClientsListener) Sub {
	callbackId := self.networkClientsListeners.Add(listener)
	return newSub(func() {
		self.networkClientsListeners.Remove(callbackId)
	})
}

// `NetworkClientsListener`
func (self *DevicesViewController) networkClientsChanged(networkClients *NetworkClientInfoList) {
	for _, listener := range self.networkClientsListeners.Get() {
		connect.HandleError(func() {
			listener.NetworkClientsChanged(networkClients)
		})
	}
}

func (self *DevicesViewController) Close() {
	deviceLog(self.device).Info("[dvc]close")

	self.cancel()
}

func (self *DevicesViewController) cmpNetworkClientLayout(a *NetworkClientInfo, b *NetworkClientInfo) int {
	if a == b {
		return 0
	}

	if clientId := self.ClientId(); clientId != nil {
		aSelf := a.ClientId != nil && *clientId == *a.ClientId
		bSelf := b.ClientId != nil && *clientId == *b.ClientId
		if aSelf != bSelf {
			if aSelf {
				return -1
			} else {
				return 1
			}
		}
	}

	if (a.Connections != nil && 0 < a.Connections.Len()) != (b.Connections != nil && 0 < b.Connections.Len()) {
		if a.Connections != nil && 0 < a.Connections.Len() {
			return -1
		} else {
			return 1
		}
	}

	return a.ClientId.Cmp(b.ClientId)
}
