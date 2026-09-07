//go:build !ios_extension

package sdk

import (
	"context"
	"sync"

	"github.com/urnetwork/connect/v2026"
)

type IsSendingFeedbackListener interface {
	StateChanged(bool)
}

type FeedbackViewController struct {
	ctx    context.Context
	cancel context.CancelFunc
	device Device
	// api-only (NewFeedbackViewControllerWithApi): no device, the same
	// controller over the network space api. Exactly one of device / api.
	api *Api

	stateLock sync.Mutex

	isSendingFeedback bool

	isSendingFeedbackListeners *connect.CallbackList[IsSendingFeedbackListener]
}

func newFeedbackViewController(ctx context.Context, device Device) *FeedbackViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &FeedbackViewController{
		ctx:    cancelCtx,
		cancel: cancel,
		device: device,

		isSendingFeedbackListeners: connect.NewCallbackList[IsSendingFeedbackListener](),
	}
	return vc
}

// NewFeedbackViewControllerWithApi opens the feedback controller over an api
// with no device; the caller owns Close.
func NewFeedbackViewControllerWithApi(ctx context.Context, api *Api) *FeedbackViewController {
	vc := newFeedbackViewController(ctx, nil)
	vc.api = api
	return vc
}

func (vc *FeedbackViewController) getApi() *Api {
	if vc.api != nil {
		return vc.api
	}
	return vc.device.GetApi()
}

func (vc *FeedbackViewController) Start() {}

func (vc *FeedbackViewController) Stop() {}

func (vc *FeedbackViewController) Close() {
	deviceLog(vc.device).Info("[fbvc]close")

	vc.cancel()
}

func (vc *FeedbackViewController) AddIsSendingFeedbackListener(listener IsSendingFeedbackListener) Sub {
	callbackId := vc.isSendingFeedbackListeners.Add(listener)
	return newSub(func() {
		vc.isSendingFeedbackListeners.Remove(callbackId)
	})
}

func (vc *FeedbackViewController) isSendingFeedbackChanged(isSending bool) {
	for _, listener := range vc.isSendingFeedbackListeners.Get() {
		connect.HandleError(func() {
			listener.StateChanged(isSending)
		})
	}
}

func (vc *FeedbackViewController) setIsSendingFeedback(isSending bool) {
	func() {
		vc.stateLock.Lock()
		defer vc.stateLock.Unlock()
		vc.isSendingFeedback = isSending
	}()
	vc.isSendingFeedbackChanged(isSending)
}

func (vc *FeedbackViewController) SendFeedback(
	msg string,
	starCount int,
) {
	// check-and-set under the lock so concurrent callers don't both proceed
	enter := false
	func() {
		vc.stateLock.Lock()
		defer vc.stateLock.Unlock()
		if !vc.isSendingFeedback {
			vc.isSendingFeedback = true
			enter = true
		}
	}()
	if !enter {
		return
	}
	vc.isSendingFeedbackChanged(true)

	args := &FeedbackSendArgs{
		Needs: &FeedbackSendNeeds{
			Other: msg,
		},
		StarCount: starCount,
	}

	vc.getApi().SendFeedback(args, SendFeedbackCallback(connect.NewApiCallback[*FeedbackSendResult](
		func(result *FeedbackSendResult, err error) {

			vc.setIsSendingFeedback(false)

		},
	)))

}
