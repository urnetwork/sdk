// A retry and a restarted client preserve the caller's original transfer intent.
package sdk

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
)

type walletTransferTestCallback struct{ done chan error }

func (self *walletTransferTestCallback) Result(result *WalletCircleTransferOutResult, err error) {
	self.done <- err
}

func TestWalletTransferRequestIdSurvivesRetriesAndSerialization(t *testing.T) {
	var stateLock sync.Mutex
	ids := []string{}
	_, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var args WalletCircleTransferOutArgs
		if err := json.NewDecoder(r.Body).Decode(&args); err != nil || args.RequestId == nil {
			t.Errorf("missing caller intent: %v", err)
			http.Error(w, "bad request", 400)
			return
		}
		func() { stateLock.Lock(); defer stateLock.Unlock(); ids = append(ids, args.RequestId.String()) }()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"request_id":%q,"challenge_id":"synthetic-challenge","challenge_status":"PENDING"}`, args.RequestId.String())
	}))
	args := NewWalletCircleTransferOutArgs("synthetic-destination", 1_000_001_000, true)
	stored, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	var restarted WalletCircleTransferOutArgs
	if err := json.Unmarshal(stored, &restarted); err != nil {
		t.Fatal(err)
	}
	other := NewWalletCircleTransferOutArgs("synthetic-destination", 1_000_001_000, true)
	for _, value := range []*WalletCircleTransferOutArgs{args, &restarted, other} {
		callback := &walletTransferTestCallback{done: make(chan error, 1)}
		api.WalletCircleTransferOut(value, callback)
		if err := <-callback.done; err != nil {
			t.Fatal(err)
		}
	}
	if len(ids) != 3 || ids[0] != ids[1] || ids[0] == ids[2] {
		t.Fatal("retry changed request id or collapsed another user intent", ids)
	}
}

func TestWalletTransferOldCallerRefusesBeforeHttp(t *testing.T) {
	var calls atomic.Int64
	_, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, `{}`) }))
	for _, args := range []*WalletCircleTransferOutArgs{nil, {ToAddress: "synthetic-destination", AmountUsdcNanoCents: 1000, Terms: true}} {
		callback := &walletTransferTestCallback{done: make(chan error, 1)}
		api.WalletCircleTransferOut(args, callback)
		if err := <-callback.done; !errors.Is(err, ErrWalletCircleTransferRequestId) {
			t.Fatal("missing intent id did not refuse", err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("older caller initiated a transfer challenge")
	}
}
