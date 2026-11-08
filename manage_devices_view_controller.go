//go:build !ios_extension

package sdk

// manage_devices_view_controller.go -- the network's devices, for a user who
// frees a slot of the concurrent client limit so this device can be added
// (client_limit_status.go), and to manage the devices any time.
//
// The list is every client that takes a slot of the limit: the network's
// devices (its top-level clients that are not hosted proxies,
// GET /network/clients) and its hosted proxies (GET /network/proxies, with their
// credentials dropped). Each says whether it is connected, when it last signed
// in, and whether it is this device. Removing one deactivates its client on the
// server (POST /network/remove-client): its credentials are revoked, its
// connection closes, and its slot is free at once.
//
// Authority: every request carries the network credential, the network's
// sign-in token, never a client token. The apps keep it beside the device's
// client token (api_network_credential.go), and a sign-in in progress passes
// its own. A client token can remove only its own client (server AUTHZ1), and a
// network that hands its client tokens to a third party (Embed) has no network
// credential in those installs. Without a network credential the controller
// sends nothing and reports ManageDevicesErrorSignInRequired, as it does when
// the server refuses the credential: the user signs in again to manage devices.
//
// After a removal frees a slot it adds this device again:
//   - opened from a device (Device OpenManageDevicesViewController): when the
//     platform holds the device off for the client limit, it ends the hold
//     (Device.RetryClientLimit) so the device dials again at once, and when the
//     hold's retry interval has not passed it retries once when it has;
//   - opened for a sign-in the limit refused
//     (NewSignInManageDevicesViewController): it calls the host's
//     ManageDevicesSignInRetry, which runs the sign-in's auth-client again.
//
// Safe for concurrent use. Listeners are called on a goroutine of the
// controller's, never with its lock held.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

const (
	// Keep these untyped so gomobile exports them on every app platform.

	// a device: the app on a phone, a computer, a provider
	ManagedDeviceKindDevice = "device"
	// a hosted proxy: a proxy the network runs from ur.io, the extension or
	// the api, which takes a slot while it is connected
	ManagedDeviceKindProxy = "proxy"
)

const (
	// Keep these untyped so gomobile exports them on every app platform.

	ManageDevicesErrorNone = ""
	// there is no network credential, or the server refused it: the user
	// signs in again to manage devices
	ManageDevicesErrorSignInRequired = "sign_in_required"
	// the list could not be loaded; Refresh tries again
	ManageDevicesErrorLoadFailed = "load_failed"
	// the removal failed and the device is still listed; RemoveDevice may
	// try again
	ManageDevicesErrorRemoveFailed = "remove_failed"
)

// ManagedDevice is one client that takes a slot of the network's concurrent
// client limit.
type ManagedDevice struct {
	ClientId *Id
	// ManagedDeviceKindDevice or ManagedDeviceKindProxy
	Kind string
	// the name the user gave the device, else its description; may be empty
	Name string
	// the system and model the app reported ("18.5 iPhone 16 Pro Max"); may
	// be empty
	Spec string
	// the device this controller was opened from; never removable here (sign
	// out instead)
	ThisDevice bool
	// connected now, so it takes a slot
	Connected bool
	// when it last signed in or renewed its credential, in unix epoch
	// milliseconds; 0 when unknown
	LastSeenTime int64
}

type ManagedDeviceList struct {
	exportedList[*ManagedDevice]
}

func NewManagedDeviceList() *ManagedDeviceList {
	return &ManagedDeviceList{exportedList: *newExportedList[*ManagedDevice]()}
}

// ManageDevicesSnapshot is one readout of the controller.
type ManageDevicesSnapshot struct {
	// this device first, then connected ones, then by the time last seen
	Devices *ManagedDeviceList
	// the list loaded at least once
	Loaded bool
	// a load is in flight
	Loading bool
	// the client a removal is in flight for; nil with none. One removal runs
	// at a time.
	RemovingClientId *Id
	// the last failure, a ManageDevicesError constant; ManageDevicesErrorNone
	// after a success
	ErrorId string
	// the removals that freed a slot and added this device again, so an app
	// can say it is connecting this device
	AddRetryCount int
}

// ManageDevicesListener receives every change of the snapshot.
type ManageDevicesListener interface {
	ManageDevicesChanged(snapshot *ManageDevicesSnapshot)
}

// ManageDevicesSignInRetry is the host's retry of a sign-in the client limit
// refused: called once after each removal that freed a slot, on a goroutine of
// the controller's, it runs the sign-in's auth-client again.
type ManageDevicesSignInRetry interface {
	RetrySignIn()
}

// What the controller needs of the device it was opened from, so a test can
// stand in for one.
type manageDevicesDevice interface {
	GetClientId() *Id
	GetApi() *Api
	GetClientLimitStatus() *ClientLimitStatus
	RetryClientLimitAfterChange() bool
}

// manageDevicesSettings tunes the controller.
type manageDevicesSettings struct {
	// how long after a removal whose retry the device refused the controller
	// retries again, and between those retries: just past the device's
	// ClientLimitRetryMinInterval (30 s by default), the least time between
	// two change retries, so a user who removes devices in a row is not left
	// in the hold. Zero or less does not retry again.
	RetryAddDelay time.Duration
	// how many times it retries again, while the hold lasts
	RetryAddAttempts int
	// bounds each request
	RequestTimeout time.Duration
	// the delay of RetryAddDelay; time.After, replaced only by tests
	after func(timeout time.Duration) <-chan time.Time
}

func defaultManageDevicesSettings() *manageDevicesSettings {
	return &manageDevicesSettings{
		RetryAddDelay:    31 * time.Second,
		RetryAddAttempts: 8,
		RequestTimeout:   30 * time.Second,
		after:            time.After,
	}
}

type ManageDevicesViewController struct {
	ctx    context.Context
	cancel context.CancelFunc

	api *Api
	// nil for a sign-in
	device manageDevicesDevice
	// the network credential of a sign-in in progress, and its retry; empty
	// when opened from a device, which uses the api's kept credential
	signInByJwt string
	signInRetry ManageDevicesSignInRetry

	settings  *manageDevicesSettings
	listeners *connect.CallbackList[ManageDevicesListener]

	stateLock        sync.Mutex
	devices          []*ManagedDevice
	loaded           bool
	loading          bool
	loadGeneration   uint64
	removingClientId *Id
	errorId          string
	addRetryCount    int
}

func newManageDevicesViewController(ctx context.Context, device Device) *ManageDevicesViewController {
	return newManageDevicesViewControllerWithDevice(ctx, device, defaultManageDevicesSettings())
}

func newManageDevicesViewControllerWithDevice(ctx context.Context, device manageDevicesDevice, settings *manageDevicesSettings) *ManageDevicesViewController {
	vc := newManageDevicesViewControllerWithApi(ctx, device.GetApi(), settings)
	vc.device = device
	return vc
}

func newManageDevicesViewControllerWithApi(ctx context.Context, api *Api, settings *manageDevicesSettings) *ManageDevicesViewController {
	cancelCtx, cancel := context.WithCancel(ctx)
	return &ManageDevicesViewController{
		ctx:       cancelCtx,
		cancel:    cancel,
		api:       api,
		settings:  settings,
		listeners: connect.NewCallbackList[ManageDevicesListener](),
	}
}

// NewSignInManageDevicesViewController opens the network's devices for a
// sign-in the client limit refused (AuthNetworkClientError.ClientLimitPrompt):
// there is no device yet, so no device is this one, and every request carries
// networkByJwt, the sign-in's network token. After a removal frees a slot it
// calls retry, which runs the sign-in's auth-client again; a nil retry only
// reloads the list. The caller closes it.
func NewSignInManageDevicesViewController(api *Api, networkByJwt string, retry ManageDevicesSignInRetry) *ManageDevicesViewController {
	vc := newManageDevicesViewControllerWithApi(api.ctx, api, defaultManageDevicesSettings())
	vc.signInByJwt = networkByJwt
	vc.signInRetry = retry
	return vc
}

// OpenManageDevicesViewController opens the network's devices on this api
// alone, for a signed-in app that holds no device (a desktop app's settings
// while it is disconnected): every request carries the network credential the
// api keeps, no device is this one, and a removal adds nothing, since there is
// no hold or sign-in to retry. Without a network credential it reports
// ManageDevicesErrorSignInRequired. The caller closes it; it ends with the api.
func (self *Api) OpenManageDevicesViewController() *ManageDevicesViewController {
	return newManageDevicesViewControllerWithApi(self.ctx, self, defaultManageDevicesSettings())
}

// Start loads the list.
func (self *ManageDevicesViewController) Start() {
	self.Refresh()
}

func (self *ManageDevicesViewController) Stop() {}

func (self *ManageDevicesViewController) Close() {
	self.cancel()
}

// GetCanManage reports whether the controller holds a network credential to
// list and remove devices with. Without one the user signs in again.
func (self *ManageDevicesViewController) GetCanManage() bool {
	return self.networkByJwt() != ""
}

func (self *ManageDevicesViewController) AddManageDevicesListener(listener ManageDevicesListener) Sub {
	callbackId := self.listeners.Add(listener)
	return newSub(func() {
		self.listeners.Remove(callbackId)
	})
}

// GetSnapshot is the current readout. Never nil.
func (self *ManageDevicesViewController) GetSnapshot() *ManageDevicesSnapshot {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.snapshotWithLock()
}

func (self *ManageDevicesViewController) snapshotWithLock() *ManageDevicesSnapshot {
	devices := NewManagedDeviceList()
	for _, device := range self.devices {
		copied := *device
		devices.Add(&copied)
	}
	return &ManageDevicesSnapshot{
		Devices:          devices,
		Loaded:           self.loaded,
		Loading:          self.loading,
		RemovingClientId: self.removingClientId,
		ErrorId:          self.errorId,
		AddRetryCount:    self.addRetryCount,
	}
}

func (self *ManageDevicesViewController) publish() {
	snapshot := self.GetSnapshot()
	for _, listener := range self.listeners.Get() {
		connect.HandleError(func() {
			listener.ManageDevicesChanged(snapshot)
		})
	}
}

// networkByJwt is the credential every request carries: the sign-in's, else the
// network credential the api keeps beside the device's client token.
func (self *ManageDevicesViewController) networkByJwt() string {
	if self.signInByJwt != "" {
		return self.signInByJwt
	}
	return self.api.networkCredential()
}

// Refresh loads the list again. A refresh replaces one in flight.
func (self *ManageDevicesViewController) Refresh() {
	networkByJwt := self.networkByJwt()
	generation, ok := func() (uint64, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.ctx.Err() != nil {
			return 0, false
		}
		self.loadGeneration += 1
		if networkByJwt == "" {
			self.loading = false
			self.errorId = ManageDevicesErrorSignInRequired
		} else {
			self.loading = true
		}
		return self.loadGeneration, true
	}()
	if !ok {
		return
	}
	self.publish()
	if networkByJwt == "" {
		return
	}
	go connect.HandleError(func() {
		devices, errorId := self.load(networkByJwt)
		applied := func() bool {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if generation != self.loadGeneration || self.ctx.Err() != nil {
				return false
			}
			self.loading = false
			self.errorId = errorId
			if errorId == ManageDevicesErrorNone {
				self.devices = devices
				self.loaded = true
			}
			return true
		}()
		if applied {
			self.publish()
		}
	})
}

// load reads the devices and the hosted proxies. A failed proxies read leaves
// the devices listed: an older server, or a network the read refused.
func (self *ManageDevicesViewController) load(networkByJwt string) ([]*ManagedDevice, string) {
	ctx, cancel := context.WithTimeout(self.ctx, self.settings.RequestTimeout)
	defer cancel()
	devicesResult, err := connect.HttpGetWithRawFunction(
		ctx,
		self.api.getHttpGetRaw(),
		fmt.Sprintf("%s/network/clients", self.api.apiUrl),
		networkByJwt,
		&NetworkClientsResult{},
		connect.NewNoopApiCallback[*NetworkClientsResult](),
	)
	if err != nil {
		return nil, manageDevicesErrorFor(err, ManageDevicesErrorLoadFailed)
	}
	proxiesResult, err := connect.HttpGetWithRawFunction(
		ctx,
		self.api.getHttpGetRaw(),
		fmt.Sprintf("%s/network/proxies", self.api.apiUrl),
		networkByJwt,
		&NetworkClientsResult{},
		connect.NewNoopApiCallback[*NetworkClientsResult](),
	)
	if err != nil {
		proxiesResult = nil
	}
	var thisClientId *Id
	if self.device != nil {
		thisClientId = self.device.GetClientId()
	}
	return managedDevicesFrom(devicesResult, proxiesResult, thisClientId), ManageDevicesErrorNone
}

// managedDevicesFrom merges the devices and the hosted proxies into the list
// the controller shows, with the proxies' credentials dropped: this device
// first, then connected ones, then the latest seen, then by client id.
func managedDevicesFrom(devicesResult *NetworkClientsResult, proxiesResult *NetworkClientsResult, thisClientId *Id) []*ManagedDevice {
	devices := []*ManagedDevice{}
	add := func(result *NetworkClientsResult, kind string) {
		if result == nil || result.Clients == nil {
			return
		}
		for i := 0; i < result.Clients.Len(); i += 1 {
			client := result.Clients.Get(i)
			if client == nil || client.ClientId == nil {
				continue
			}
			name := client.DeviceName
			if name == "" {
				name = client.DeviceDescription
			}
			var lastSeenTime int64
			if client.AuthTime != nil {
				lastSeenTime = client.AuthTime.UnixMilli()
			}
			devices = append(devices, &ManagedDevice{
				ClientId:     client.ClientId,
				Kind:         kind,
				Name:         name,
				Spec:         client.DeviceSpec,
				ThisDevice:   thisClientId != nil && *thisClientId == *client.ClientId,
				Connected:    client.Connections != nil && 0 < client.Connections.Len(),
				LastSeenTime: lastSeenTime,
			})
		}
	}
	add(devicesResult, ManagedDeviceKindDevice)
	add(proxiesResult, ManagedDeviceKindProxy)
	slices.SortStableFunc(devices, func(a *ManagedDevice, b *ManagedDevice) int {
		switch {
		case a.ThisDevice != b.ThisDevice:
			if a.ThisDevice {
				return -1
			}
			return 1
		case a.Connected != b.Connected:
			if a.Connected {
				return -1
			}
			return 1
		case a.LastSeenTime != b.LastSeenTime:
			if b.LastSeenTime < a.LastSeenTime {
				return -1
			}
			return 1
		default:
			return a.ClientId.Cmp(b.ClientId)
		}
	})
	return devices
}

// manageDevicesErrorFor maps a failed request: a missing network credential,
// or one the server refused (401, 403), needs a new sign-in; any other failure
// is the given retryable one.
func manageDevicesErrorFor(err error, failed string) string {
	if errors.Is(err, ErrNetworkCredentialRequired) {
		return ManageDevicesErrorSignInRequired
	}
	var statusError *connect.HttpStatusError
	if errors.As(err, &statusError) &&
		(statusError.StatusCode == http.StatusUnauthorized || statusError.StatusCode == http.StatusForbidden) {
		return ManageDevicesErrorSignInRequired
	}
	return failed
}

// RemoveDevice removes a device or hosted proxy of the list. Its client is
// deactivated on the server, which revokes its credentials, closes its
// connection and frees its slot at once; then the controller adds this device
// again (see the file header) and reloads the list. A client the server no
// longer has reads as removed. This device is never removed here, and one
// removal runs at a time.
func (self *ManageDevicesViewController) RemoveDevice(clientId *Id) {
	if clientId == nil {
		return
	}
	if self.device != nil {
		if thisClientId := self.device.GetClientId(); thisClientId != nil && *thisClientId == *clientId {
			return
		}
	}
	networkByJwt := self.networkByJwt()
	start := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.ctx.Err() != nil || self.removingClientId != nil {
			return false
		}
		if networkByJwt == "" {
			self.errorId = ManageDevicesErrorSignInRequired
			return true
		}
		self.removingClientId = clientId
		return true
	}()
	if !start {
		return
	}
	self.publish()
	if networkByJwt == "" {
		return
	}
	go connect.HandleError(func() {
		ctx, cancel := context.WithTimeout(self.ctx, self.settings.RequestTimeout)
		defer cancel()
		// the network credential removes any client of the network; a client
		// token would remove only its own
		_, err := self.api.RemoveNetworkClientSyncWithContextAndJwt(
			ctx,
			&RemoveNetworkClientArgs{ClientId: clientId},
			networkByJwt,
		)
		// a refusal in the result is a client the network no longer has: gone
		// either way, which the reload confirms
		removed := err == nil
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.removingClientId = nil
			if removed {
				self.errorId = ManageDevicesErrorNone
				self.devices = slices.DeleteFunc(slices.Clone(self.devices), func(device *ManagedDevice) bool {
					return *device.ClientId == *clientId
				})
			} else {
				self.errorId = manageDevicesErrorFor(err, ManageDevicesErrorRemoveFailed)
			}
		}()
		self.publish()
		if removed && self.ctx.Err() == nil {
			self.retryAdd()
			self.Refresh()
		}
	})
}

// retryAdd adds this device again after a removal freed a slot: the device's
// hold ends at once, or the sign-in runs its auth-client again. The device's
// retry is the change retry (Device.RetryClientLimitAfterChange): the removal
// is a change the server confirmed, so the backoff of the retries refused
// before it does not hold this one back.
func (self *ManageDevicesViewController) retryAdd() {
	switch {
	case self.device != nil:
		retry := func() (held bool, retried bool) {
			status := self.device.GetClientLimitStatus()
			if status == nil || status.Status != ClientLimitStatusExceeded {
				// not held: nothing to retry
				return false, false
			}
			return true, self.device.RetryClientLimitAfterChange()
		}
		held, retried := retry()
		if !held {
			return
		}
		self.countAddRetry()
		if !retried && 0 < self.settings.RetryAddDelay {
			// inside the change retries' interval, from a removal just
			// before: again each RetryAddDelay while the hold lasts, so
			// removals in a row are not left waiting out the hold. The device
			// decides when a retry dials; one it refuses costs nothing.
			go connect.HandleError(func() {
				for range self.settings.RetryAddAttempts {
					select {
					case <-self.ctx.Done():
						return
					case <-self.settings.after(self.settings.RetryAddDelay):
					}
					if held, retried := retry(); !held || retried {
						return
					}
				}
			})
		}
	case self.signInRetry != nil:
		self.countAddRetry()
		connect.HandleError(self.signInRetry.RetrySignIn)
	}
}

func (self *ManageDevicesViewController) countAddRetry() {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.addRetryCount += 1
	}()
	self.publish()
}
