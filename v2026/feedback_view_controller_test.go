//go:build !ios_extension

package sdk

import (
	"context"
	"errors"
	"testing"
)

// Records every feedback controller callback in order. The fake request
// answers synchronously, so the order is final when SendFeedback returns.
type feedbackEventRecorder struct {
	events []string
}

func (self *feedbackEventRecorder) StateChanged(isSending bool) {
	if isSending {
		self.events = append(self.events, "sending:true")
	} else {
		self.events = append(self.events, "sending:false")
	}
}

func (self *feedbackEventRecorder) Message(message string) {
	self.events = append(self.events, "error:"+message)
}

func (self *feedbackEventRecorder) Success() {
	self.events = append(self.events, "success")
}

// Sends one feedback through a controller whose api answers err (nil = ok)
// without leaving the goroutine, and returns the callbacks it made.
func sendTestFeedback(t *testing.T, err error) []string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	vc := newFeedbackViewController(ctx, nil)
	defer vc.Close()
	vc.sendFeedbackRequest = func(args *FeedbackSendArgs, callback SendFeedbackCallback) {
		if args.Needs.Other != "the tunnel stops after an hour" || args.StarCount != 4 {
			t.Fatalf("request args %+v", args)
		}
		if err != nil {
			callback.Result(nil, err)
		} else {
			callback.Result(&FeedbackSendResult{}, nil)
		}
	}

	recorder := &feedbackEventRecorder{}
	vc.AddIsSendingFeedbackListener(recorder)
	vc.AddFeedbackSendErrorListener(recorder)
	vc.AddFeedbackSendSuccessListener(recorder)

	vc.SendFeedback("the tunnel stops after an hour", 4)
	return recorder.events
}

// The root cause: the api error was dropped and a failed send ended exactly
// like a successful one (sending true, then false), so a client could not
// tell them apart. A failed send must report the error before sending clears.
func TestFeedbackSendFailureReportsError(t *testing.T) {
	events := sendTestFeedback(t, errors.New("503 Service Unavailable"))
	want := []string{"sending:true", "error:503 Service Unavailable", "sending:false"}
	if !equalStrings(events, want) {
		t.Fatalf("events %v, want %v", events, want)
	}
}

func TestFeedbackSendSuccessReportsSuccess(t *testing.T) {
	events := sendTestFeedback(t, nil)
	want := []string{"sending:true", "success", "sending:false"}
	if !equalStrings(events, want) {
		t.Fatalf("events %v, want %v", events, want)
	}
}
