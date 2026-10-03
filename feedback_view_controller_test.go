//go:build !ios_extension

package sdk

import (
	"net/http"
	"sync"
	"testing"
	"time"
)

// Records the order of every feedback controller callback, so a test can
// assert that the send result arrives before the sending state clears.
type feedbackEventRecorder struct {
	stateLock sync.Mutex
	events    []string
	done      chan struct{}
}

func newFeedbackEventRecorder() *feedbackEventRecorder {
	return &feedbackEventRecorder{done: make(chan struct{})}
}

func (self *feedbackEventRecorder) add(event string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.events = append(self.events, event)
	if event == "sending:false" {
		close(self.done)
	}
}

func (self *feedbackEventRecorder) StateChanged(isSending bool) {
	if isSending {
		self.add("sending:true")
	} else {
		self.add("sending:false")
	}
}

func (self *feedbackEventRecorder) Message(message string) {
	self.add("error")
}

func (self *feedbackEventRecorder) Success() {
	self.add("success")
}

func (self *feedbackEventRecorder) wait(t *testing.T) []string {
	t.Helper()
	select {
	case <-self.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the send never finished")
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string{}, self.events...)
}

func sendTestFeedback(t *testing.T, status int) []string {
	t.Helper()
	ctx, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/feedback/send-feedback" {
			http.NotFound(w, r)
			return
		}
		if status != http.StatusOK {
			http.Error(w, "unavailable", status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	vc := NewFeedbackViewControllerWithApi(ctx, api)
	t.Cleanup(vc.Close)

	recorder := newFeedbackEventRecorder()
	vc.AddIsSendingFeedbackListener(recorder)
	vc.AddFeedbackSendErrorListener(recorder)
	vc.AddFeedbackSendSuccessListener(recorder)

	vc.SendFeedback("the tunnel stops after an hour", 4)
	return recorder.wait(t)
}

// A failed send reports the error before the sending state clears, and never
// reports success, so the form can keep the text instead of thanking the user.
func TestFeedbackSendFailureReportsError(t *testing.T) {
	events := sendTestFeedback(t, http.StatusServiceUnavailable)
	want := []string{"sending:true", "error", "sending:false"}
	if !equalStrings(events, want) {
		t.Fatalf("events %v, want %v", events, want)
	}
}

func TestFeedbackSendSuccessReportsSuccess(t *testing.T) {
	events := sendTestFeedback(t, http.StatusOK)
	want := []string{"sending:true", "success", "sending:false"}
	if !equalStrings(events, want) {
		t.Fatalf("events %v, want %v", events, want)
	}
}
