//go:build !ios_extension

package sdk

import (
	"context"
	"sync"

	"github.com/urnetwork/connect"
)

type ReferralCodeListener interface {
	ReferralCodeUpdated(string)
}

// A fetch ended without a code; the message is the api error. Start fetches
// again, so a client can show the error with a retry instead of loading forever.
type ReferralCodeFetchErrorListener interface {
	Message(string)
}

type ReferralCodeViewController struct {
	ctx    context.Context
	cancel context.CancelFunc

	stateLock sync.Mutex

	isFetching bool
	// the last successful fetch, for hosts that render the terms (total
	// referrals, the cap and the bonus) and not just the code
	result *GetNetworkReferralCodeResult

	device Device
	// api-only (NewReferralCodeViewControllerWithApi): no device, the same
	// controller over the network space api. Exactly one of device / api.
	api *Api
	// replaces the api call when set (tests drive the result without a network)
	referralCodeRequest func(callback GetNetworkReferralCodeCallback)

	referralCodeListeners           *connect.CallbackList[ReferralCodeListener]
	referralCodeFetchErrorListeners *connect.CallbackList[ReferralCodeFetchErrorListener]
}

// NewReferralCodeViewControllerWithApi opens the referral code controller over
// an api with no device; the caller owns Close.
func NewReferralCodeViewControllerWithApi(ctx context.Context, api *Api) *ReferralCodeViewController {
	vc := newReferralCodeViewController(ctx, nil)
	vc.api = api
	return vc
}

func (self *ReferralCodeViewController) getApi() *Api {
	if self.api != nil {
		return self.api
	}
	return self.device.GetApi()
}

// GetReferralCodeResult is the last fetched referral code result (code, total
// referrals and the referral terms), nil until the first fetch lands.
func (self *ReferralCodeViewController) GetReferralCodeResult() *GetNetworkReferralCodeResult {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.result
}

func (self *ReferralCodeViewController) setResult(result *GetNetworkReferralCodeResult) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.result = result
}

func newReferralCodeViewController(ctx context.Context, device Device) *ReferralCodeViewController {

	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &ReferralCodeViewController{
		ctx:    cancelCtx,
		cancel: cancel,
		device: device,

		referralCodeListeners:           connect.NewCallbackList[ReferralCodeListener](),
		referralCodeFetchErrorListeners: connect.NewCallbackList[ReferralCodeFetchErrorListener](),
	}
	return vc
}

func (self *ReferralCodeViewController) AddReferralCodeListener(listener ReferralCodeListener) Sub {
	callbackId := self.referralCodeListeners.Add(listener)
	return newSub(func() {
		self.referralCodeListeners.Remove(callbackId)
	})
}

func (self *ReferralCodeViewController) AddReferralCodeFetchErrorListener(listener ReferralCodeFetchErrorListener) Sub {
	callbackId := self.referralCodeFetchErrorListeners.Add(listener)
	return newSub(func() {
		self.referralCodeFetchErrorListeners.Remove(callbackId)
	})
}

func (self *ReferralCodeViewController) referralCodeFetchFailed(message string) {
	for _, listener := range self.referralCodeFetchErrorListeners.Get() {
		connect.HandleError(func() {
			listener.Message(message)
		})
	}
}

func (self *ReferralCodeViewController) referralCodeChanged(code string) {
	for _, listener := range self.referralCodeListeners.Get() {
		connect.HandleError(func() {
			listener.ReferralCodeUpdated(code)
		})
	}
}

func (self *ReferralCodeViewController) requestReferralCode(callback GetNetworkReferralCodeCallback) {
	if self.referralCodeRequest != nil {
		self.referralCodeRequest(callback)
		return
	}
	self.getApi().GetNetworkReferralCode(callback)
}

func (self *ReferralCodeViewController) Start() {
	go connect.HandleError(self.fetchNetworkReferralCode)
}

func (self *ReferralCodeViewController) Stop() {}

func (self *ReferralCodeViewController) Close() {
	deviceLog(self.device).Info("[rcvc]close")

	self.cancel()
}

func (self *ReferralCodeViewController) setIsFetching(isFetching bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.isFetching = isFetching
}

func (self *ReferralCodeViewController) fetchNetworkReferralCode() {
	// check-and-set under the lock so concurrent callers don't both proceed
	enter := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !self.isFetching {
			self.isFetching = true
			enter = true
		}
	}()
	if !enter {
		return
	}
	self.requestReferralCode(
		GetNetworkReferralCodeCallback(
			connect.NewApiCallback[*GetNetworkReferralCodeResult](
				func(result *GetNetworkReferralCodeResult, err error) {
					// fetching clears first, so an error listener may Start a retry
					if err != nil {
						self.setIsFetching(false)
						deviceLog(self.device).Infof("[rcvc]error fetching referral code: %s", err)
						self.referralCodeFetchFailed(err.Error())
						return
					}

					if result == nil || result.ReferralCode == "" {
						self.setIsFetching(false)
						self.referralCodeFetchFailed("no referral code")
						return
					}

					self.setResult(result)
					self.referralCodeChanged(result.ReferralCode)
					self.setIsFetching(false)
				},
			),
		),
	)
}
