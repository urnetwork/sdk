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

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/sdk/v2026"
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

// A gift code's redeem answer carries its year of Pro (`pro`), and the
// outcome the pages read passes it through beside transfer_balance; a data
// code's outcome has none (GIFTOFPRIVACY.md 10.2).
func TestRedeemBalanceCodeOutcomeCarriesTheGiftYear(t *testing.T) {
	start := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC)
	gift := &sdk.RedeemBalanceCodeResult{
		TransferBalance: &sdk.RedeemBalanceCodeTransferBalance{
			StartTime: sdk.NewTimeUnixMilli(start.UnixMilli()),
			EndTime:   sdk.NewTimeUnixMilli(end.UnixMilli()),
		},
		Pro: &sdk.RedeemBalanceCodePro{
			StartTime: sdk.NewTimeUnixMilli(start.UnixMilli()),
			EndTime:   sdk.NewTimeUnixMilli(end.UnixMilli()),
			Queued:    true,
		},
	}
	data := &sdk.RedeemBalanceCodeResult{TransferBalance: &sdk.RedeemBalanceCodeTransferBalance{BalanceByteCount: 1 << 30}}
	unused := &sdk.GetNetworkRedeemedBalanceCodesResult{BalanceCodes: redeemedList()}

	listCalls := 0
	outcome := redeemBalanceCodeOutcome(testBalanceCodeSecret, fakeRedeem(gift, nil), fakeRedeemedCodes(unused, nil, &listCalls))
	if outcome.Outcome != sdk.BalanceCodeRedeemOutcomeRedeemed {
		t.Fatalf("outcome %q, want %q", outcome.Outcome, sdk.BalanceCodeRedeemOutcomeRedeemed)
	}
	if outcome.Pro != gift.Pro {
		t.Fatalf("pro = %+v, want the answer's", outcome.Pro)
	}
	// as the page reads it: the promise resolves with the outcome's json
	page := jsJson(outcome)
	pro := page.Get("pro")
	if pro.IsUndefined() || !pro.Get("queued").Truthy() {
		t.Fatalf("pro on the page = %v", pro)
	}
	for name, want := range map[string]time.Time{"start_time": start, "end_time": end} {
		got, err := time.Parse(time.RFC3339, pro.Get(name).String())
		if err != nil || !got.Equal(want) {
			t.Fatalf("pro.%s = %q, want %s", name, pro.Get(name).String(), want.Format(time.RFC3339))
		}
	}
	if page.Get("transfer_balance").IsUndefined() {
		t.Fatal("the gift outcome lost transfer_balance")
	}

	outcome = redeemBalanceCodeOutcome(testBalanceCodeSecret, fakeRedeem(data, nil), fakeRedeemedCodes(unused, nil, &listCalls))
	if outcome.Outcome != sdk.BalanceCodeRedeemOutcomeRedeemed || outcome.Pro != nil {
		t.Fatalf("data outcome %q with pro %+v", outcome.Outcome, outcome.Pro)
	}
	if !jsJson(outcome).Get("pro").IsUndefined() {
		t.Fatal("a data code's outcome carries pro on the page")
	}
	if listCalls != 0 {
		t.Fatalf("a credited redeem fetched the redeemed list %d times", listCalls)
	}
}

// A code typed as a gift card prints it (in groups, with dashes, in lower
// case, or with 0, 1 and 8 for O, I and B), whose answer was lost after the
// server committed, still reads "already redeemed" from the list. The redeem
// sends the code exactly as typed.
func TestRedeemBalanceCodeOutcomeTypedInGroups(t *testing.T) {
	listed := &sdk.GetNetworkRedeemedBalanceCodesResult{BalanceCodes: redeemedList(testBalanceCodeSecret)}
	lost := errors.New("http request failed")
	for _, typed := range []string{
		"ABCD EFGH IJKL MNOP QRST UVWX YZ",
		"abcd-efgh-ijkl-mnop-qrst-uvwx-yz",
		"ABCD EFGH 1JKL MN0P QRST UVWX YZ",
	} {
		sent := ""
		redeem := func(args *sdk.RedeemBalanceCodeArgs, callback sdk.RedeemBalanceCodeCallback) {
			sent = args.Secret
			go callback.Result(nil, lost)
		}
		listCalls := 0
		outcome := redeemBalanceCodeOutcome(typed, redeem, fakeRedeemedCodes(listed, nil, &listCalls))
		if outcome.Outcome != sdk.BalanceCodeRedeemOutcomeAlreadyRedeemed {
			t.Fatalf("%q: outcome %q, want %q", typed, outcome.Outcome, sdk.BalanceCodeRedeemOutcomeAlreadyRedeemed)
		}
		if sent != typed {
			t.Fatalf("sent %q, want the code as typed, %q", sent, typed)
		}
	}
}
