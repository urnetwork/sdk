package sdk

import (
	"context"
	"sync"
	// "github.com/urnetwork/glog"
)

type ViewController interface {
	Close()
	Start()
	Stop()
}

type ViewControllerManager interface {
	OpenLocationsViewController() *LocationsViewController

	OpenConnectViewController() *ConnectViewController

	OpenWalletViewController() *WalletViewController

	OpenProvideViewController() *ProvideViewController

	OpenDevicesViewController() *DevicesViewController

	OpenAccountViewController() *AccountViewController

	OpenFeedbackViewController() *FeedbackViewController

	OpenNetworkUserViewController() *NetworkUserViewController

	OpenAccountPreferencesViewController() *AccountPreferencesViewController

	OpenReferralCodeViewController() *ReferralCodeViewController

	CloseViewController(vc ViewController)

	Close()
}

// compile check that viewControllerManager conforms to ViewControllerManager
var _ ViewControllerManager = (*viewControllerManager)(nil)

type viewControllerManager struct {
	ctx    context.Context
	cancel context.CancelFunc
	device Device

	stateLock sync.Mutex

	openedViewControllers map[ViewController]bool
}

func newViewControllerManager(ctx context.Context, device Device) *viewControllerManager {
	cancelCtx, cancel := context.WithCancel(ctx)

	return &viewControllerManager{
		ctx:                   cancelCtx,
		cancel:                cancel,
		device:                device,
		openedViewControllers: map[ViewController]bool{},
	}
}

func (self *viewControllerManager) openViewController(vc ViewController) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.openedViewControllers[vc] = true
}

func (self *viewControllerManager) closeViewController(vc ViewController) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.openedViewControllers, vc)
}

func (self *viewControllerManager) OpenLocationsViewController() *LocationsViewController {
	vm := newLocationsViewController(self.ctx, self.device)
	self.openViewController(vm)
	return vm
}

func (self *viewControllerManager) OpenConnectViewController() *ConnectViewController {
	vm := newConnectViewController(self.ctx, self.device)
	self.openViewController(vm)
	return vm
}

func (self *viewControllerManager) OpenWalletViewController() *WalletViewController {
	vc := newWalletViewController(self.ctx, self.device)
	self.openViewController(vc)
	return vc
}

func (self *viewControllerManager) OpenProvideViewController() *ProvideViewController {
	vc := newProvideViewController(self.ctx, self.device)
	self.openViewController(vc)
	return vc
}

func (self *viewControllerManager) OpenDevicesViewController() *DevicesViewController {
	vc := newDevicesViewController(self.ctx, self.device)
	self.openViewController(vc)
	return vc
}

func (self *viewControllerManager) OpenAccountViewController() *AccountViewController {
	vc := newAccountViewController(self.ctx, self.device)
	self.openViewController(vc)
	return vc
}

func (self *viewControllerManager) OpenFeedbackViewController() *FeedbackViewController {
	vc := newFeedbackViewController(self.ctx, self.device)
	self.openViewController(vc)
	return vc
}

func (self *viewControllerManager) OpenNetworkUserViewController() *NetworkUserViewController {
	vc := newNetworkUserViewController(self.ctx, self.device)
	self.openViewController(vc)
	return vc
}

func (self *viewControllerManager) OpenAccountPreferencesViewController() *AccountPreferencesViewController {
	vc := newAccountPreferencesViewController(self.ctx, self.device)
	self.openViewController(vc)
	return vc
}

func (self *viewControllerManager) OpenReferralCodeViewController() *ReferralCodeViewController {
	vc := newReferralCodeViewController(self.ctx, self.device)
	self.openViewController(vc)
	return vc
}

func (self *viewControllerManager) CloseViewController(vc ViewController) {
	vc.Close()
	self.closeViewController(vc)
}

func (self *viewControllerManager) Close() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.cancel()

	for vc, _ := range self.openedViewControllers {
		vc.Close()
	}
	clear(self.openedViewControllers)
}
