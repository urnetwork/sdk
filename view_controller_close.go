//go:build !ios_extension

package sdk

// The concrete close entry points keep foreign-language bindings type-safe.
// ViewController is an interface, which the C++ binding represents as a callback
// bundle rather than as the original controller handle. Calling Close directly
// stops the controller but cannot remove it from the manager's ownership map.
// These methods preserve the concrete handle all the way back to Go, so repeated
// presentation suspend/resume cycles stay ownership- and memory-bounded.

func (self *viewControllerManager) CloseConnectViewController(vc *ConnectViewController) {
	if vc != nil {
		self.CloseViewController(vc)
	}
}

func (self *viewControllerManager) CloseContractViewController(vc *ContractViewController) {
	if vc != nil {
		self.CloseViewController(vc)
	}
}

func (self *viewControllerManager) CloseContractDetailsViewController(vc *ContractDetailsViewController) {
	if vc != nil {
		self.CloseViewController(vc)
	}
}

func (self *viewControllerManager) CloseBlockActionViewController(vc *BlockActionViewController) {
	if vc != nil {
		self.CloseViewController(vc)
	}
}

func (self *viewControllerManager) CloseLocationsViewController(vc *LocationsViewController) {
	if vc != nil {
		self.CloseViewController(vc)
	}
}

func (self *viewControllerManager) CloseProviderLocationsViewController(vc *ProviderLocationsViewController) {
	if vc != nil {
		self.CloseViewController(vc)
	}
}

func (self *viewControllerManager) ClosePeerViewController(vc *PeerViewController) {
	if vc != nil {
		self.CloseViewController(vc)
	}
}

func (self *viewControllerManager) CloseDevicesViewController(vc *DevicesViewController) {
	if vc != nil {
		self.CloseViewController(vc)
	}
}

func (self *viewControllerManager) ClosePostQuantumIdentityViewController(
	vc *PostQuantumIdentityViewController,
) {
	if vc != nil {
		self.CloseViewController(vc)
	}
}
