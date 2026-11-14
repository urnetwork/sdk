// Exercises the server's account epochs wire shape through the actual sdk
// transport: the bytes served beside each epoch's points and the open
// epoch's running figure (server#451).
package sdk

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// Reads GET /account/epochs once from a server answering with response.
func readTestAccountEpochs(t *testing.T, response string) *AccountEpochsResult {
	t.Helper()
	const bearerJwt = "synthetic-account-epochs-token"
	_, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.RequestURI != "/account/epochs" {
			t.Errorf("account epochs request = %s %s, want GET /account/epochs", r.Method, r.RequestURI)
		}
		requireRequestBearer(t, r, bearerJwt)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, response)
	}))
	api.SetByJwt(bearerJwt)
	type answer struct {
		result *AccountEpochsResult
		err    error
	}
	answers := make(chan answer, 1)
	api.AccountEpochs(connect.NewApiCallback(func(result *AccountEpochsResult, err error) {
		answers <- answer{result: result, err: err}
	}))
	select {
	case a := <-answers:
		if a.err != nil || a.result == nil {
			t.Fatalf("account epochs = %+v, %v", a.result, a.err)
		}
		return a.result
	case <-time.After(30 * time.Second):
		t.Fatal("no account epochs answer")
		return nil
	}
}

// Each epoch's served_byte_count, the open epoch and the open block, past
// 2^53 bytes so a lossy float decode would show.
func TestApiAccountEpochsDecodesServedBytesAndTheRunningFigures(t *testing.T) {
	const response = `{"epochs":[{"epoch":6,"start_millis":1794938940000,"end_millis":1795543740000,"points":12.5,"share_bps":40,"amount_rao":"700","served_byte_count":9007199254740993}],"current_epoch":{"epoch":7,"start_millis":1795543740000,"end_millis":1796148540000,"served_byte_count":1099511627776,"as_of_millis":1795636800000},"current_block":{"block_number":21,"start_millis":1794700800000,"end_millis":1795305600000,"served_byte_count":9007199254740995,"as_of_millis":1795636800000,"epoch":8}}`
	result := readTestAccountEpochs(t, response)
	if result.Epochs == nil || result.Epochs.Len() != 1 {
		t.Fatalf("epochs = %+v", result.Epochs)
	}
	epoch := result.Epochs.Get(0)
	want := AccountEpoch{Epoch: 6, StartMillis: 1794938940000, EndMillis: 1795543740000, Points: 12.5, ShareBps: 40, ServedByteCount: 9007199254740993}
	if *epoch != want {
		t.Fatalf("epoch = %+v, want %+v", *epoch, want)
	}
	wantCurrent := AccountCurrentEpoch{Epoch: 7, StartMillis: 1795543740000, EndMillis: 1796148540000, ServedByteCount: 1099511627776, AsOfMillis: 1795636800000}
	if result.CurrentEpoch == nil || *result.CurrentEpoch != wantCurrent {
		t.Fatalf("current epoch = %+v, want %+v", result.CurrentEpoch, wantCurrent)
	}
	wantBlock := AccountCurrentBlock{BlockNumber: 21, StartMillis: 1794700800000, EndMillis: 1795305600000, ServedByteCount: 9007199254740995, AsOfMillis: 1795636800000, Epoch: 8}
	if result.CurrentBlock == nil || *result.CurrentBlock != wantBlock {
		t.Fatalf("current block = %+v, want %+v", result.CurrentBlock, wantBlock)
	}
}

// A server that predates the fields: zero bytes and no running figures.
func TestApiAccountEpochsDecodesAServerWithoutServedBytes(t *testing.T) {
	const response = `{"epochs":[{"epoch":4,"start_millis":1793729340000,"end_millis":1794334140000,"points":3,"share_bps":0}]}`
	result := readTestAccountEpochs(t, response)
	if result.Epochs == nil || result.Epochs.Len() != 1 || result.Epochs.Get(0).ServedByteCount != 0 || result.Epochs.Get(0).Points != 3 {
		t.Fatalf("epochs = %+v", result.Epochs)
	}
	if result.CurrentEpoch != nil || result.CurrentBlock != nil {
		t.Fatalf("current epoch = %+v and block = %+v, want none", result.CurrentEpoch, result.CurrentBlock)
	}
}
