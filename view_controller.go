package sdk

import (
	"context"
	"sync"

	// "github.com/golang/glog"
)


type ViewController interface {
	Close()
	Start()
	Stop()
}


type ViewControllerManager struct {
	ctx context.Context
	cancel context.CancelFunc
	device Device

	stateLock sync.Mutex

	openedViewControllers map[ViewController]bool
}

func NewViewControllerManager(device Device) *ViewControllerManager {
	ctx, cancel := context.WithCancel(context.Background())

	return &ViewControllerManager{
		ctx: ctx,
		cancel: cancel,
		device: device,
		openedViewControllers:             map[ViewController]bool{},
	}
}

func (self *ViewControllerManager) openViewController(vc ViewController) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.openedViewControllers[vc] = true
}

func (self *ViewControllerManager) closeViewController(vc ViewController) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.openedViewControllers, vc)
}

func (self *ViewControllerManager) OpenLocationsViewController() *LocationsViewController {
	vm := newLocationsViewController(self.ctx, self.device)
	self.openViewController(vm)
	return vm
}

func (self *ViewControllerManager) OpenConnectViewController() *ConnectViewController {
	vm := newConnectViewController(self.ctx, self.device)
	self.openViewController(vm)
	return vm
}

func (self *ViewControllerManager) OpenWalletViewController() *WalletViewController {
	vc := newWalletViewController(self.ctx, self.device)
	self.openViewController(vc)
	return vc
}

func (self *ViewControllerManager) OpenProvideViewController() *ProvideViewController {
	vc := newProvideViewController(self.ctx, self.device)
	self.openViewController(vc)
	return vc
}

func (self *ViewControllerManager) OpenDevicesViewController() *DevicesViewController {
	vc := newDevicesViewController(self.ctx, self.device)
	self.openViewController(vc)
	return vc
}

func (self *ViewControllerManager) OpenAccountViewController() *AccountViewController {
	vc := newAccountViewController(self.ctx, self.device)
	self.openViewController(vc)
	return vc
}

func (self *ViewControllerManager) OpenFeedbackViewController() *FeedbackViewController {
	vc := newFeedbackViewController(self.ctx, self.device)
	self.openViewController(vc)
	return vc
}

func (self *ViewControllerManager) OpenNetworkUserViewController() *NetworkUserViewController {
	vc := newNetworkUserViewController(self.ctx, self.device)
	self.openViewController(vc)
	return vc
}

func (self *ViewControllerManager) OpenAccountPreferencesViewController() *AccountPreferencesViewController {
	vc := newAccountPreferencesViewController(self.ctx, self.device)
	self.openViewController(vc)
	return vc
}

func (self *ViewControllerManager) OpenReferralCodeViewController() *ReferralCodeViewController {
	vc := newReferralCodeViewController(self.ctx, self.device)
	self.openViewController(vc)
	return vc
}

func (self *ViewControllerManager) CloseViewController(vc ViewController) {
	vc.Close()
	self.closeViewController(vc)
}


func (self *ViewControllerManager) Close() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.cancel()

	for vc, _ := range self.openedViewControllers {
		vc.Close()
	}
	clear(self.openedViewControllers)
}
