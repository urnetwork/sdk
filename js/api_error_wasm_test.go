//go:build js

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"syscall/js"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
)

// Api promise rejections carry the kind fields the pages branch on, and the
// redeem classification answers "may have gone through" from the redeemed
// list (UPGRADE.md W5). Fake api calls only: no network, no timers beyond the
// promise-await guard.

func awaitApiPromise(t *testing.T, p js.Value) (js.Value, js.Value) {
	t.Helper()
	type settled struct {
		value    js.Value
		rejected js.Value
	}
	done := make(chan settled, 1)
	resolve := js.FuncOf(func(_ js.Value, a []js.Value) any { done <- settled{value: a[0], rejected: js.Undefined()}; return nil })
	reject := js.FuncOf(func(_ js.Value, a []js.Value) any { done <- settled{value: js.Undefined(), rejected: a[0]}; return nil })
	defer resolve.Release()
	defer reject.Release()
	p.Call("then", resolve, reject)
	select {
	case s := <-done:
		return s.value, s.rejected
	case <-time.After(2 * time.Second):
		t.Fatal("promise did not settle")
		return js.Undefined(), js.Undefined()
	}
}

type fakeTimeoutError struct{}

func (fakeTimeoutError) Error() string   { return "Client.Timeout exceeded while awaiting headers" }
func (fakeTimeoutError) Timeout() bool   { return true }
func (fakeTimeoutError) Temporary() bool { return true }

func rejectedApiPromise(err error) js.Value {
	return apiPromise(func(cb connect.ApiCallback[*sdk.RedeemBalanceCodeResult]) {
		go cb.Result(nil, err)
	})
}

func TestApiPromiseRejectsDeadlineAsTimeout(t *testing.T) {
	for _, err := range []error{
		context.DeadlineExceeded,
		fmt.Errorf("post: %w", context.DeadlineExceeded),
		fakeTimeoutError{},
	} {
		_, rejected := awaitApiPromise(t, rejectedApiPromise(err))
		if rejected.IsUndefined() {
			t.Fatalf("%v: resolved", err)
		}
		if !rejected.Get("isTimeout").Truthy() || rejected.Get("kind").String() != "timeout" {
			t.Fatalf("%v: rejected with isTimeout=%v kind=%v, want a timeout", err, rejected.Get("isTimeout"), rejected.Get("kind"))
		}
		if rejected.Get("message").String() != err.Error() {
			t.Fatalf("message %q, want %q", rejected.Get("message").String(), err.Error())
		}
	}
}

func TestApiPromiseRejectsOtherFailuresByKind(t *testing.T) {
	var syntaxErr error = &json.SyntaxError{Offset: 1}
	for _, c := range []struct {
		err    error
		kind   string
		status int
	}{
		{err: errors.New("http request failed"), kind: "network"},
		{err: &connect.HttpStatusError{StatusCode: 502, Status: "502 Bad Gateway"}, kind: "http", status: 502},
		{err: syntaxErr, kind: "parse"},
	} {
		_, rejected := awaitApiPromise(t, rejectedApiPromise(c.err))
		if rejected.Get("kind").String() != c.kind || rejected.Get("isTimeout").Truthy() || rejected.Get("status").Int() != c.status {
			t.Fatalf("%v: kind=%v isTimeout=%v status=%v, want %s/false/%d", c.err, rejected.Get("kind"), rejected.Get("isTimeout"), rejected.Get("status"), c.kind, c.status)
		}
	}
}

const testBalanceCodeSecret = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"

func redeemedList(secrets ...string) *sdk.RedeemedBalanceCodeList {
	list := sdk.NewRedeemedBalanceCodeList()
	for _, secret := range secrets {
		list.Add(&sdk.RedeemedBalanceCode{Secret: secret, BalanceByteCount: 1 << 30})
	}
	return list
}

func fakeRedeem(result *sdk.RedeemBalanceCodeResult, err error) func(*sdk.RedeemBalanceCodeArgs, sdk.RedeemBalanceCodeCallback) {
	return func(args *sdk.RedeemBalanceCodeArgs, callback sdk.RedeemBalanceCodeCallback) {
		go callback.Result(result, err)
	}
}

func fakeRedeemedCodes(result *sdk.GetNetworkRedeemedBalanceCodesResult, err error, calls *int) func(sdk.GetNetworkRedeemedBalanceCodesCallback) {
	return func(callback sdk.GetNetworkRedeemedBalanceCodesCallback) {
		*calls += 1
		go callback.Result(result, err)
	}
}

func TestRedeemBalanceCodeOutcome(t *testing.T) {
	credited := &sdk.RedeemBalanceCodeResult{TransferBalance: &sdk.RedeemBalanceCodeTransferBalance{BalanceByteCount: 1 << 30}}
	unknownCode := &sdk.RedeemBalanceCodeResult{Error: &sdk.RedeemBalanceCodeError{Message: "Unknown balance code."}}
	listed := &sdk.GetNetworkRedeemedBalanceCodesResult{BalanceCodes: redeemedList(testBalanceCodeSecret)}
	notListed := &sdk.GetNetworkRedeemedBalanceCodesResult{BalanceCodes: redeemedList("ZYXWVUTSRQPONMLKJIHGFEDCBA")}
	lost := errors.New("http request failed")

	for _, c := range []struct {
		name        string
		result      *sdk.RedeemBalanceCodeResult
		err         error
		list        *sdk.GetNetworkRedeemedBalanceCodesResult
		listErr     error
		want        string
		listFetched bool
	}{
		{name: "credited", result: credited, list: listed, want: sdk.BalanceCodeRedeemOutcomeRedeemed},
		// the answer was lost after the server committed: the code is in the list
		{name: "lost answer, committed", err: lost, list: listed, want: sdk.BalanceCodeRedeemOutcomeAlreadyRedeemed, listFetched: true},
		{name: "lost answer, not committed", err: lost, list: notListed, want: sdk.BalanceCodeRedeemOutcomeUnknown, listFetched: true},
		{name: "lost answer, list unreachable", err: lost, listErr: lost, want: sdk.BalanceCodeRedeemOutcomeUnknown, listFetched: true},
		// a retry of a code this network already redeemed answers "Unknown balance code."
		{name: "retry after commit", result: unknownCode, list: listed, want: sdk.BalanceCodeRedeemOutcomeAlreadyRedeemed, listFetched: true},
		{name: "invalid", result: unknownCode, list: notListed, want: sdk.BalanceCodeRedeemOutcomeInvalid, listFetched: true},
	} {
		listCalls := 0
		outcome := redeemBalanceCodeOutcome(testBalanceCodeSecret, fakeRedeem(c.result, c.err), fakeRedeemedCodes(c.list, c.listErr, &listCalls))
		if outcome.Outcome != c.want {
			t.Fatalf("%s: outcome %q, want %q", c.name, outcome.Outcome, c.want)
		}
		if (listCalls > 0) != c.listFetched {
			t.Fatalf("%s: redeemed list fetched %d times", c.name, listCalls)
		}
		if (c.err != nil) != (outcome.TransportError != nil) {
			t.Fatalf("%s: transport error %v", c.name, outcome.TransportError)
		}
	}
}
