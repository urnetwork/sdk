//go:build !ios_extension

package sdk

import (
	"context"
	"errors"
	"testing"
)

// Collects the referral code controller's callbacks. The fake request answers
// synchronously and the test calls the fetch directly (Start runs it on a
// goroutine), so the callbacks are final when the fetch returns.
type referralCodeEventRecorder struct {
	codes  []string
	errors []string
}

func (self *referralCodeEventRecorder) ReferralCodeUpdated(code string) {
	self.codes = append(self.codes, code)
}

func (self *referralCodeEventRecorder) Message(message string) {
	self.errors = append(self.errors, message)
}

type referralCodeAnswer struct {
	result *GetNetworkReferralCodeResult
	err    error
}

func newTestReferralCodeViewController(t *testing.T, answer *referralCodeAnswer) (*ReferralCodeViewController, *referralCodeEventRecorder) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	vc := newReferralCodeViewController(ctx, nil)
	t.Cleanup(vc.Close)
	vc.referralCodeRequest = func(callback GetNetworkReferralCodeCallback) {
		callback.Result(answer.result, answer.err)
	}
	recorder := &referralCodeEventRecorder{}
	vc.AddReferralCodeListener(recorder)
	vc.AddReferralCodeFetchErrorListener(recorder)
	return vc, recorder
}

// The root cause: a failed fetch was only logged, so no listener ever fired
// and a client waiting for the code loaded forever. A failed fetch must
// report its error, and the next fetch must run and deliver the code.
func TestReferralCodeFetchFailureReportsErrorAndRetries(t *testing.T) {
	answer := &referralCodeAnswer{err: errors.New("503 Service Unavailable")}
	vc, recorder := newTestReferralCodeViewController(t, answer)

	vc.fetchNetworkReferralCode()
	if len(recorder.codes) != 0 || len(recorder.errors) != 1 || recorder.errors[0] != "503 Service Unavailable" {
		t.Fatalf("after a failed fetch: codes %v errors %v", recorder.codes, recorder.errors)
	}
	if vc.GetReferralCodeResult() != nil {
		t.Fatal("a failed fetch must not publish a result")
	}

	answer.result = &GetNetworkReferralCodeResult{ReferralCode: "TESTCODE", TotalReferrals: 1}
	answer.err = nil
	vc.fetchNetworkReferralCode()
	if len(recorder.codes) != 1 || recorder.codes[0] != "TESTCODE" || len(recorder.errors) != 1 {
		t.Fatalf("after the retry: codes %v errors %v", recorder.codes, recorder.errors)
	}
}

// A reply without a code fired no listener either.
func TestReferralCodeEmptyReplyReportsError(t *testing.T) {
	vc, recorder := newTestReferralCodeViewController(t, &referralCodeAnswer{result: &GetNetworkReferralCodeResult{}})

	vc.fetchNetworkReferralCode()
	if len(recorder.codes) != 0 || len(recorder.errors) != 1 {
		t.Fatalf("codes %v errors %v", recorder.codes, recorder.errors)
	}
}
