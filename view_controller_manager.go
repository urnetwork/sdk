


type ViewControllerManager struct {

	openedViewControllers map[ViewController]bool
}



func newViewControllerManager(
) (*BringYourDevice, error) {
	

	byDevice := &BringYourDevice{
		openedViewControllers:             map[ViewController]bool{},
	}

}



func (self *BringYourDevice) openViewController(vc ViewController) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.openedViewControllers[vc] = true
}

func (self *BringYourDevice) closeViewController(vc ViewController) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.openedViewControllers, vc)
}

func (self *BringYourDevice) OpenLocationsViewController() *LocationsViewController {
	vm := newLocationsViewController(self.ctx, self)
	self.openViewController(vm)
	return vm
}

func (self *BringYourDevice) OpenConnectViewController() *ConnectViewController {
	vm := newConnectViewController(self.ctx, self)
	self.openViewController(vm)
	return vm
}

func (self *BringYourDevice) OpenWalletViewController() *WalletViewController {
	vc := newWalletViewController(self.ctx, self)
	self.openViewController(vc)
	return vc
}

func (self *BringYourDevice) OpenProvideViewController() *ProvideViewController {
	vc := newProvideViewController(self.ctx, self)
	self.openViewController(vc)
	return vc
}

func (self *BringYourDevice) OpenDevicesViewController() *DevicesViewController {
	vc := newDevicesViewController(self.ctx, self)
	self.openViewController(vc)
	return vc
}

func (self *BringYourDevice) OpenAccountViewController() *AccountViewController {
	vc := newAccountViewController(self.ctx, self)
	self.openViewController(vc)
	return vc
}

func (self *BringYourDevice) OpenFeedbackViewController() *FeedbackViewController {
	vc := newFeedbackViewController(self.ctx, self)
	self.openViewController(vc)
	return vc
}

func (self *BringYourDevice) OpenNetworkUserViewController() *NetworkUserViewController {
	vc := newNetworkUserViewController(self.ctx, self)
	self.openViewController(vc)
	return vc
}

func (self *BringYourDevice) OpenAccountPreferencesViewController() *AccountPreferencesViewController {
	vc := newAccountPreferencesViewController(self.ctx, self)
	self.openViewController(vc)
	return vc
}

func (self *BringYourDevice) OpenReferralCodeViewController() *ReferralCodeViewController {
	vc := newReferralCodeViewController(self.ctx, self)
	self.openViewController(vc)
	return vc
}

func (self *BringYourDevice) CloseViewController(vc ViewController) {
	vc.Close()
	self.closeViewController(vc)
}


func (self *BringYourDevice) Close() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	for vc, _ := range self.openedViewControllers {
		vc.Close()
	}
	clear(self.openedViewControllers)
}