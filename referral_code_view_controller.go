package sdk

import (
	"context"
	"sync"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/connect"
)

type ReferralCodeListener interface {
	ReferralCodeUpdated(string)
}

type ReferralCodeViewController struct {
	ctx    context.Context
	cancel context.CancelFunc

	stateLock sync.Mutex

	isFetching bool

	device Device

	referralCodeListeners *connect.CallbackList[ReferralCodeListener]
}

func newReferralCodeViewController(ctx context.Context, device Device) *ReferralCodeViewController {

	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &ReferralCodeViewController{
		ctx:    cancelCtx,
		cancel: cancel,
		device: device,

		isFetching: false,

		referralCodeListeners: connect.NewCallbackList[ReferralCodeListener](),
	}
	return vc
}

func (self *ReferralCodeViewController) AddReferralCodeListener(listener ReferralCodeListener) Sub {
	callbackId := self.referralCodeListeners.Add(listener)
	return newSub(func() {
		self.referralCodeListeners.Remove(callbackId)
	})
}

func (self *ReferralCodeViewController) referralCodeChanged(code string) {
	for _, listener := range self.referralCodeListeners.Get() {
		connect.HandleError(func() {
			listener.ReferralCodeUpdated(code)
		})
	}
}

func (self *ReferralCodeViewController) Start() {
	go self.fetchNetworkReferralCode()
}

func (self *ReferralCodeViewController) Stop() {}

func (self *ReferralCodeViewController) Close() {
	glog.Info("[rcvc]close")

	self.cancel()
}

func (self *ReferralCodeViewController) setIsFetching(isFetching bool) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.isFetching = isFetching
	}()
}

func (self *ReferralCodeViewController) fetchNetworkReferralCode() {

	if !self.isFetching {
		self.device.GetApi().GetNetworkReferralCode(
			GetNetworkReferralCodeCallback(
				connect.NewApiCallback[*GetNetworkReferralCodeResult](
					func(result *GetNetworkReferralCodeResult, err error) {
						if err != nil {
							self.setIsFetching(false)
							glog.Infof("[rcvc]error fetching referral code: %s", err)
							return
						}

						if result != nil && result.ReferralCode != "" {
							self.referralCodeChanged(result.ReferralCode)
						}

						self.setIsFetching(false)
					},
				),
			),
		)
	}
}
