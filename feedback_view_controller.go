package sdk

import (
	"context"
	"sync"

	"github.com/golang/glog"

	"github.com/urnetwork/connect"
)

type IsSendingFeedbackListener interface {
	StateChanged(bool)
}

type FeedbackViewController struct {
	ctx    context.Context
	cancel context.CancelFunc
	device Device

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

		isSendingFeedback:          false,
		isSendingFeedbackListeners: connect.NewCallbackList[IsSendingFeedbackListener](),
	}
	return vc
}

func (vc *FeedbackViewController) Start() {}

func (vc *FeedbackViewController) Stop() {}

func (vc *FeedbackViewController) Close() {
	glog.Info("[fbvc]close")

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

	if !vc.isSendingFeedback {

		vc.setIsSendingFeedback(true)

		args := &FeedbackSendArgs{
			Needs: &FeedbackSendNeeds{
				Other: msg,
			},
			StarCount: starCount,
		}

		vc.device.GetApi().SendFeedback(args, SendFeedbackCallback(connect.NewApiCallback[*FeedbackSendResult](
			func(result *FeedbackSendResult, err error) {

				vc.setIsSendingFeedback(false)

			},
		)))

	}

}
