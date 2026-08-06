// view controller for the post quantum identity ui.
// Surfaces the device's own public identity key (and its canonical display
// hash) and the providers with an established, identity-verified e2e session,
// re-emitting the device's provider identity changes to the ui.
// All methods are safe for concurrent use.
package sdk

import (
	"context"
	"sync"

	"github.com/urnetwork/connect/v2026"
)

type PostQuantumIdentityListener interface {
	ProviderIdentitiesChanged()
}

type PostQuantumIdentityViewController struct {
	ctx    context.Context
	cancel context.CancelFunc
	device Device

	stateLock sync.Mutex

	providerIdentitiesChangedSub Sub

	postQuantumIdentityListeners *connect.CallbackList[PostQuantumIdentityListener]
}

func newPostQuantumIdentityViewController(ctx context.Context, device Device) *PostQuantumIdentityViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &PostQuantumIdentityViewController{
		ctx:    cancelCtx,
		cancel: cancel,
		device: device,

		postQuantumIdentityListeners: connect.NewCallbackList[PostQuantumIdentityListener](),
	}
	return vc
}

// GetPublicIdentityKey returns the device provider client's long-lived public
// identity key, or nil when unavailable
func (self *PostQuantumIdentityViewController) GetPublicIdentityKey() []byte {
	return self.device.GetPublicIdentityKey()
}

// GetPublicIdentityKeyHash returns the canonical display hash of the device's
// public identity key (see `PublicIdentityKeyHash`), or "" when unavailable
func (self *PostQuantumIdentityViewController) GetPublicIdentityKeyHash() string {
	return self.device.GetPublicIdentityKeyHash()
}

// GetProviderIdentities returns the providers with an established,
// identity-verified e2e session. Empty (never nil) when disconnected
func (self *PostQuantumIdentityViewController) GetProviderIdentities() *ProviderIdentityList {
	return self.device.GetProviderIdentities()
}

// ProviderIdentityChangeListener. re-emitted to the ui listeners
func (self *PostQuantumIdentityViewController) ProviderIdentitiesChanged() {
	self.providerIdentitiesChanged()
}

func (self *PostQuantumIdentityViewController) providerIdentitiesChanged() {
	for _, listener := range self.postQuantumIdentityListeners.Get() {
		connect.HandleError(func() {
			listener.ProviderIdentitiesChanged()
		})
	}
}

func (self *PostQuantumIdentityViewController) AddPostQuantumIdentityListener(listener PostQuantumIdentityListener) Sub {
	callbackId := self.postQuantumIdentityListeners.Add(listener)
	return newSub(func() {
		self.postQuantumIdentityListeners.Remove(callbackId)
	})
}

func (self *PostQuantumIdentityViewController) Start() {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.providerIdentitiesChangedSub == nil {
			self.providerIdentitiesChangedSub = self.device.AddProviderIdentityChangeListener(self)
		}
	}()
	// seed the ui with the current state
	self.providerIdentitiesChanged()
}

func (self *PostQuantumIdentityViewController) Stop() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.providerIdentitiesChangedSub != nil {
		self.providerIdentitiesChangedSub.Close()
		self.providerIdentitiesChangedSub = nil
	}
}

func (self *PostQuantumIdentityViewController) Close() {
	deviceLog(self.device).Info("[pqivc]close")
	self.cancel()
	self.Stop()
}
