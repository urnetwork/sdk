//go:build !ios_extension

package sdk

import (
	"context"
	"sync"

	"github.com/urnetwork/connect"
)

type IsSendingFeedbackListener interface {
	StateChanged(bool)
}

// The send failed; the message is the api error. The form keeps its text so
// the user can send again.
type FeedbackSendErrorListener interface {
	Message(string)
}

// The server accepted the feedback.
type FeedbackSendSuccessListener interface {
	Success()
}

type FeedbackViewController struct {
	ctx    context.Context
	cancel context.CancelFunc
	device Device
	// api-only (NewFeedbackViewControllerWithApi): no device, the same
	// controller over the network space api. Exactly one of device / api.
	api *Api
	// replaces the api call when set (tests drive the result without a network)
	sendFeedbackRequest func(args *FeedbackSendArgs, callback SendFeedbackCallback)

	stateLock sync.Mutex

	isSendingFeedback bool

	isSendingFeedbackListeners   *connect.CallbackList[IsSendingFeedbackListener]
	feedbackSendErrorListeners   *connect.CallbackList[FeedbackSendErrorListener]
	feedbackSendSuccessListeners *connect.CallbackList[FeedbackSendSuccessListener]
}

func newFeedbackViewController(ctx context.Context, device Device) *FeedbackViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &FeedbackViewController{
		ctx:    cancelCtx,
		cancel: cancel,
		device: device,

		isSendingFeedbackListeners:   connect.NewCallbackList[IsSendingFeedbackListener](),
		feedbackSendErrorListeners:   connect.NewCallbackList[FeedbackSendErrorListener](),
		feedbackSendSuccessListeners: connect.NewCallbackList[FeedbackSendSuccessListener](),
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

func (vc *FeedbackViewController) requestSendFeedback(args *FeedbackSendArgs, callback SendFeedbackCallback) {
	if vc.sendFeedbackRequest != nil {
		vc.sendFeedbackRequest(args, callback)
		return
	}
	vc.getApi().SendFeedback(args, callback)
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

// Each send ends with exactly one result, delivered before the sending state
// returns to false, so a listener on both sees the result first.
func (vc *FeedbackViewController) AddFeedbackSendErrorListener(listener FeedbackSendErrorListener) Sub {
	callbackId := vc.feedbackSendErrorListeners.Add(listener)
	return newSub(func() {
		vc.feedbackSendErrorListeners.Remove(callbackId)
	})
}

func (vc *FeedbackViewController) AddFeedbackSendSuccessListener(listener FeedbackSendSuccessListener) Sub {
	callbackId := vc.feedbackSendSuccessListeners.Add(listener)
	return newSub(func() {
		vc.feedbackSendSuccessListeners.Remove(callbackId)
	})
}

func (vc *FeedbackViewController) feedbackSendFailed(message string) {
	for _, listener := range vc.feedbackSendErrorListeners.Get() {
		connect.HandleError(func() {
			listener.Message(message)
		})
	}
}

func (vc *FeedbackViewController) feedbackSendSucceeded() {
	for _, listener := range vc.feedbackSendSuccessListeners.Get() {
		connect.HandleError(func() {
			listener.Success()
		})
	}
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

	vc.requestSendFeedback(args, SendFeedbackCallback(connect.NewApiCallback[*FeedbackSendResult](
		func(result *FeedbackSendResult, err error) {
			// the result goes out before the sending state clears
			if err != nil {
				deviceLog(vc.device).Infof("[fbvc]error sending feedback: %s", err)
				vc.feedbackSendFailed(err.Error())
			} else {
				vc.feedbackSendSucceeded()
			}
			vc.setIsSendingFeedback(false)
		},
	)))

}
