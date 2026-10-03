//go:build !ios_extension

package sdk

import (
	"net/http"
	"sync"
	"testing"
	"time"
)

// Collects the referral code controller's callbacks.
type referralCodeEventRecorder struct {
	stateLock sync.Mutex
	codes     []string
	errors    []string
	update    chan struct{}
}

func newReferralCodeEventRecorder() *referralCodeEventRecorder {
	return &referralCodeEventRecorder{update: make(chan struct{}, 16)}
}

func (self *referralCodeEventRecorder) ReferralCodeUpdated(code string) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.codes = append(self.codes, code)
	}()
	self.update <- struct{}{}
}

func (self *referralCodeEventRecorder) Message(message string) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.errors = append(self.errors, message)
	}()
	self.update <- struct{}{}
}

func (self *referralCodeEventRecorder) next(t *testing.T) (codes []string, errors []string) {
	t.Helper()
	select {
	case <-self.update:
	case <-time.After(10 * time.Second):
		t.Fatal("the fetch reported nothing")
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string{}, self.codes...), append([]string{}, self.errors...)
}

// A failed fetch reports an error instead of leaving the client loading, and
// Start after it fetches again and delivers the code.
func TestReferralCodeFetchFailureReportsErrorAndRetries(t *testing.T) {
	var stateLock sync.Mutex
	fail := true
	ctx, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/account/referral-code" {
			http.NotFound(w, r)
			return
		}
		stateLock.Lock()
		failNow := fail
		stateLock.Unlock()
		if failNow {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"referral_code": "TESTCODE", "total_referrals": 1}`))
	}))
	vc := NewReferralCodeViewControllerWithApi(ctx, api)
	t.Cleanup(vc.Close)

	recorder := newReferralCodeEventRecorder()
	vc.AddReferralCodeListener(recorder)
	vc.AddReferralCodeFetchErrorListener(recorder)

	vc.Start()
	codes, errors := recorder.next(t)
	if len(codes) != 0 || len(errors) != 1 {
		t.Fatalf("after a failed fetch: codes %v errors %v", codes, errors)
	}
	if vc.GetReferralCodeResult() != nil {
		t.Fatal("a failed fetch must not publish a result")
	}

	stateLock.Lock()
	fail = false
	stateLock.Unlock()
	vc.Start()
	codes, errors = recorder.next(t)
	if len(codes) != 1 || codes[0] != "TESTCODE" || len(errors) != 1 {
		t.Fatalf("after the retry: codes %v errors %v", codes, errors)
	}
}

// A reply without a code is a failure too: the listener would otherwise
// never fire.
func TestReferralCodeEmptyReplyReportsError(t *testing.T) {
	ctx, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	vc := NewReferralCodeViewControllerWithApi(ctx, api)
	t.Cleanup(vc.Close)

	recorder := newReferralCodeEventRecorder()
	vc.AddReferralCodeListener(recorder)
	vc.AddReferralCodeFetchErrorListener(recorder)

	vc.Start()
	codes, errors := recorder.next(t)
	if len(codes) != 0 || len(errors) != 1 {
		t.Fatalf("codes %v errors %v", codes, errors)
	}
}
