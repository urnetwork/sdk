//go:build !ios_extension && !js

package sdk

// Compute() for the apps (sidecar_compute.go; BAL-M12-19; DESIGN-COMPUTE §15.3, §9.5): its JSON shapes, the local's
// and the daemon's alike, refusals included; a store purchase reported through the app's own Api with the network's
// credential, under purchase_report.go's contract; packs offered only where the person may buy and sales are open;
// the remote backend calling ur daemon's compute methods, the person's acts confirmed; and every exported signature
// one gomobile binds. Synthetic values only.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
)

// One call the daemon took.
type testComputeDaemonCall struct {
	method    string
	confirmed bool
	params    json.RawMessage
}

// A daemon answering ur's compute methods from replies, recording each call.
type testComputeDaemon struct {
	stateLock sync.Mutex
	calls     []testComputeDaemonCall
	replies   map[string]any
}

func newTestComputeDaemon(replies map[string]any) *testComputeDaemon {
	return &testComputeDaemon{replies: replies}
}

func (self *testComputeDaemon) sidecarId() *Id {
	return nil
}

func (self *testComputeDaemon) state() (string, string) {
	return "online", ""
}

func (self *testComputeDaemon) generation() int64 {
	return 1
}

func (self *testComputeDaemon) refresh(ctx context.Context) error {
	return nil
}

func (self *testComputeDaemon) pair(ctx context.Context, childJwt string) (*sidecarControlPairReply, error) {
	return nil, errors.New("this daemon pairs nothing")
}

func (self *testComputeDaemon) network() string {
	return "the paired network"
}

func (self *testComputeDaemon) call(ctx context.Context, method string, params any, confirmed bool, result any) error {
	paramsJson, err := json.Marshal(params)
	if err != nil {
		return err
	}
	self.stateLock.Lock()
	self.calls = append(self.calls, testComputeDaemonCall{method: method, confirmed: confirmed, params: paramsJson})
	reply, ok := self.replies[method]
	self.stateLock.Unlock()
	if !ok {
		return &sidecarControlError{Code: sidecarControlCodeMethodNotFound, Message: "method not found"}
	}
	replyJson, err := json.Marshal(reply)
	if err != nil {
		return err
	}
	return json.Unmarshal(replyJson, result)
}

func (self *testComputeDaemon) close() {
}

func (self *testComputeDaemon) taken() []testComputeDaemonCall {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.calls)
}

// A balance as the daemon and the core answer it.
func testComputeBalance(mayBuy bool, salesOpen bool) *sidecar.ComputeBalance {
	return &sidecar.ComputeBalance{
		AvailableNanoUsd: 4_500_000_000, HeldNanoUsd: 500_000_000, BalanceNanoUsd: 5_000_000_000,
		State: sidecar.ComputeAccountStateActive, Version: 7, MayBuy: mayBuy, SalesOpen: salesOpen,
		AtRiskEnclaveIds: []sidecar.Id{}, UpdateTime: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC),
	}
}

// The answers' shapes: the daemon's JSON reaches the app as the core's types write it, a refusal api answered with a
// code reads {"refusal": {...}} with no error on both backends, any other failure is the app's error, and a Sidecar
// with no core answers an error from every call.
func TestSidecarComputeJsonShapes(t *testing.T) {
	balance := testComputeBalance(true, true)
	daemon := newTestComputeDaemon(map[string]any{sidecarControlMethodComputeBalance: balance})
	compute := newSidecarCompute(&sidecarComputeArgs{remote: daemon})
	balanceJson, err := compute.Balance()
	if err != nil {
		t.Fatal(err)
	}
	read := &sidecar.ComputeBalance{}
	if err := json.Unmarshal([]byte(balanceJson), read); err != nil {
		t.Fatal(err)
	}
	if read.AvailableNanoUsd != balance.AvailableNanoUsd || read.State != balance.State || !read.MayBuy || !read.UpdateTime.Equal(balance.UpdateTime) {
		t.Errorf("the balance reads %+v", read)
	}
	// the core's answer is the same JSON the daemon writes
	localJson, err := sidecarComputeJson(balance, nil)
	if err != nil {
		t.Fatal(err)
	}
	if localJson != balanceJson {
		t.Errorf("the core's JSON %s differs from the daemon's %s", localJson, balanceJson)
	}
	// a refusal with a code is an answer, with no error; another failure is an error
	refusalJson, err := sidecarComputeJson(nil, &sidecar.SidecarApiError{Route: "GET /compute/balance", Status: http.StatusForbidden, Code: "may_not_buy", RetryAfterMs: 1500})
	if err != nil || refusalJson != `{"refusal":{"code":"may_not_buy","retry_after_ms":1500}}` {
		t.Errorf("a refusal answers %s, %v", refusalJson, err)
	}
	failure := errors.New("the api did not answer")
	if _, err := sidecarComputeJson(nil, failure); !errors.Is(err, failure) {
		t.Errorf("a failure answers %v", err)
	}
	if _, err := sidecarComputeJson(nil, &sidecar.SidecarApiError{Route: "GET /compute/balance", Status: http.StatusBadGateway}); err == nil {
		t.Error("a refusal with no code answered no error")
	}

	// no core
	nothing := newSidecarCompute(&sidecarComputeArgs{})
	if _, err := nothing.Balance(); !errors.Is(err, errSidecarComputeNoCore) {
		t.Errorf("Balance on no core: %v", err)
	}
	if _, err := nothing.Spend(""); !errors.Is(err, errSidecarComputeNoCore) {
		t.Errorf("Spend on no core: %v", err)
	}
	if err := nothing.AutoRenew(`{"mode":"threshold","on":false}`); !errors.Is(err, errSidecarComputeNoCore) {
		t.Errorf("AutoRenew on no core: %v", err)
	}
	// a malformed request is refused before any backend
	if _, err := compute.Charges(`{"cursor":"a","card":"4242"}`); !errors.Is(err, errSidecarComputeRequest) {
		t.Errorf("a request with an unknown field: %v", err)
	}
	if _, err := compute.Spend(`{"by":"device"}`); !errors.Is(err, errSidecarComputeRequest) {
		t.Errorf("a spend view by no dimension: %v", err)
	}
	if err := compute.AutoRenew(`{"mode":"weekly","on":true}`); !errors.Is(err, errSidecarComputeRequest) {
		t.Errorf("an auto-renew mode §9.8 does not name: %v", err)
	}
	if len(daemon.taken()) != 1 {
		t.Errorf("a refused request reached the daemon: %d calls", len(daemon.taken()))
	}
}

// A store purchase goes through the app's own Api, with the network's credential, to the verify route of its store,
// as Pro's do; the answer says whether the status is terminal and what to do with the store: keep the proof and report
// again after the contract's wait while pending, finish an App Store transaction once terminal, acknowledge a Play
// subscription, and nothing for a Play consumable, which the server consumes; a transport failure is an error, never
// terminal.
func TestStorePurchaseUsesTheNetworkCredentialAndTheContract(t *testing.T) {
	type seen struct {
		path          string
		authorization string
		body          map[string]any
	}
	var stateLock sync.Mutex
	requests := []*seen{}
	status := ""
	api := newTestPaymentApi(t, func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		body := map[string]any{}
		json.Unmarshal(data, &body)
		stateLock.Lock()
		requests = append(requests, &seen{path: r.URL.Path, authorization: r.Header.Get("Authorization"), body: body})
		answer := status
		stateLock.Unlock()
		if answer == "" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": answer})
	})
	compute := newSidecarCompute(&sidecarComputeArgs{})
	report := func(answer string, market string, purchaseJson string) *sidecarComputeStorePurchaseAnswer {
		t.Helper()
		stateLock.Lock()
		status = answer
		stateLock.Unlock()
		reply, err := compute.StorePurchase(api, market, purchaseJson)
		if err != nil {
			t.Fatalf("%s %s: %v", market, answer, err)
		}
		read := &sidecarComputeStorePurchaseAnswer{}
		if err := json.Unmarshal([]byte(reply), read); err != nil {
			t.Fatal(err)
		}
		return read
	}

	apple := `{"signed_transaction":"test.jws.transaction"}`
	if answer := report(PurchaseReportStatusPending, sidecarComputeMarketAppStore, apple); answer.Terminal || answer.Finalize != SidecarComputeFinalizeKeep || answer.RetryAfterMs != 1_000 {
		t.Errorf("a pending App Store report: %+v", answer)
	}
	if answer := report(PurchaseReportStatusPending, sidecarComputeMarketAppStore, `{"signed_transaction":"test.jws.transaction","attempt":2}`); answer.RetryAfterMs != 30_000 {
		t.Errorf("a third pending report waits %d ms", answer.RetryAfterMs)
	}
	for _, terminal := range []string{PurchaseReportStatusCredited, PurchaseReportStatusAlreadyCredited, PurchaseReportStatusWrongNetwork, PurchaseReportStatusInvalid} {
		if answer := report(terminal, sidecarComputeMarketAppStore, apple); !answer.Terminal || answer.Finalize != SidecarComputeFinalizeFinish || answer.Status != terminal {
			t.Errorf("an App Store %s: %+v", terminal, answer)
		}
	}
	consumable := `{"product_id":"compute.credits.25","purchase_token":"test-play-token","consumable":true}`
	if answer := report(PurchaseReportStatusCredited, sidecarComputeMarketGooglePlay, consumable); !answer.Terminal || answer.Finalize != SidecarComputeFinalizeNone {
		t.Errorf("a Play consumable credited: %+v", answer)
	}
	subscription := `{"product_id":"compute.monthly.25","purchase_token":"test-play-token-2"}`
	if answer := report(PurchaseReportStatusCredited, sidecarComputeMarketGooglePlay, subscription); !answer.Terminal || answer.Finalize != SidecarComputeFinalizeAcknowledge {
		t.Errorf("a Play subscription credited: %+v", answer)
	}
	if answer := report(PurchaseReportStatusInvalid, sidecarComputeMarketGooglePlay, subscription); !answer.Terminal || answer.Finalize != SidecarComputeFinalizeNone {
		t.Errorf("a Play purchase that never verifies: %+v", answer)
	}
	if answer := report(PurchaseReportStatusPending, sidecarComputeMarketGooglePlay, subscription); answer.Terminal || answer.Finalize != SidecarComputeFinalizeKeep {
		t.Errorf("a pending Play purchase: %+v", answer)
	}
	// a transport failure is an error: keep the proof
	stateLock.Lock()
	status = ""
	stateLock.Unlock()
	if _, err := compute.StorePurchase(api, sidecarComputeMarketAppStore, apple); err == nil {
		t.Error("a failed report answered")
	}

	// every report went to its store's verify route with the network's credential, the proof in its body
	stateLock.Lock()
	defer stateLock.Unlock()
	if len(requests) != 11 {
		t.Fatalf("%d reports reached the api", len(requests))
	}
	for i, request := range requests {
		if request.authorization != "Bearer test-network-jwt" {
			t.Errorf("report %d carried %q", i, request.authorization)
		}
		switch {
		case request.path == "/subscription/verify-apple-transaction":
			if request.body["signed_transaction"] != "test.jws.transaction" {
				t.Errorf("report %d sent %v", i, request.body)
			}
		case request.path == "/subscription/verify-play-purchase":
			if request.body["purchase_token"] == "" || request.body["product_id"] == "" || request.body["consumable"] != nil {
				t.Errorf("report %d sent %v", i, request.body)
			}
		default:
			t.Errorf("report %d went to %s", i, request.path)
		}
	}

	// what is refused before anything is sent
	for label, refused := range map[string]func() error{
		"no Api": func() error {
			_, err := compute.StorePurchase(nil, sidecarComputeMarketAppStore, apple)
			return err
		},
		"Stripe": func() error {
			_, err := compute.StorePurchase(api, "stripe", apple)
			return err
		},
		"an App Store report with a Play token": func() error {
			_, err := compute.StorePurchase(api, sidecarComputeMarketAppStore, `{"signed_transaction":"x","purchase_token":"y"}`)
			return err
		},
		"a Play report with no product": func() error {
			_, err := compute.StorePurchase(api, sidecarComputeMarketGooglePlay, `{"purchase_token":"y"}`)
			return err
		},
		"an unknown field": func() error {
			_, err := compute.StorePurchase(api, sidecarComputeMarketAppStore, `{"signed_transaction":"x","receipt":"y"}`)
			return err
		},
	} {
		if err := refused(); !errors.Is(err, errSidecarComputeRequest) {
			t.Errorf("%s: %v", label, err)
		}
	}
	if len(requests) != 11 {
		t.Errorf("a refused report reached the api")
	}
}

// Packs are offered only where Balance says the person may buy and sales are open (Q-BAL-13): otherwise none, with
// why, and the market's products are never asked for.
func TestPacksHiddenWhenSalesClosedOrMayNotBuy(t *testing.T) {
	stripePacks := &struct {
		Packs []*sidecar.ComputePack `json:"packs"`
	}{Packs: []*sidecar.ComputePack{{Market: sidecar.ComputeMarketStripe, Credits: 25}}}
	for _, c := range []struct {
		mayBuy    bool
		salesOpen bool
	}{{mayBuy: false, salesOpen: true}, {mayBuy: true, salesOpen: false}, {mayBuy: false, salesOpen: false}} {
		daemon := newTestComputeDaemon(map[string]any{
			sidecarControlMethodComputeBalance: testComputeBalance(c.mayBuy, c.salesOpen),
			sidecarControlMethodComputePacks:   stripePacks,
		})
		packsJson, err := newSidecarCompute(&sidecarComputeArgs{remote: daemon}).Packs("stripe")
		if err != nil {
			t.Fatal(err)
		}
		packs := &sidecarComputePacks{}
		if err := json.Unmarshal([]byte(packsJson), packs); err != nil {
			t.Fatal(err)
		}
		if len(packs.Packs) != 0 || packs.MayBuy != c.mayBuy || packs.SalesOpen != c.salesOpen || packs.Market != "stripe" {
			t.Errorf("may buy %v, sales open %v: %s", c.mayBuy, c.salesOpen, packsJson)
		}
		for _, call := range daemon.taken() {
			if call.method == sidecarControlMethodComputePacks {
				t.Errorf("may buy %v, sales open %v: the products were asked for", c.mayBuy, c.salesOpen)
			}
		}
	}
	daemon := newTestComputeDaemon(map[string]any{
		sidecarControlMethodComputeBalance: testComputeBalance(true, true),
		sidecarControlMethodComputePacks:   stripePacks,
	})
	packsJson, err := newSidecarCompute(&sidecarComputeArgs{remote: daemon}).Packs("stripe")
	if err != nil {
		t.Fatal(err)
	}
	packs := &sidecarComputePacks{}
	if err := json.Unmarshal([]byte(packsJson), packs); err != nil {
		t.Fatal(err)
	}
	if len(packs.Packs) != 1 || packs.Packs[0].Credits != 25 {
		t.Errorf("the open sale's packs: %s", packsJson)
	}
	// a balance api refused is the answer, with no products asked for
	refused := newTestComputeDaemon(map[string]any{
		sidecarControlMethodComputeBalance: &sidecarComputeRefusalReply{Refusal: &sidecarComputeRefusal{Code: "not_built"}},
	})
	if answer, err := newSidecarCompute(&sidecarComputeArgs{remote: refused}).Packs("app_store"); err != nil || answer != `{"refusal":{"code":"not_built"}}` {
		t.Errorf("a refused balance: %s, %v", answer, err)
	}
	if _, err := newSidecarCompute(&sidecarComputeArgs{remote: daemon}).Packs("bitcoin"); !errors.Is(err, errSidecarComputeRequest) {
		t.Errorf("a market that sells no compute credit: %v", err)
	}
}

// On the remote backend each call is ur daemon's compute method, naming the paired network: the reads and the budget
// raise unconfirmed, a redemption and an auto-renew change, the person's own acts in the app's UI, its Confirm methods
// sent confirmed; a refusal reaches the app as the daemon's answer, or for Checkout and AutoRenew as an error naming
// its code alone.
func TestRemoteModeCallsTheDaemonMethods(t *testing.T) {
	daemon := newTestComputeDaemon(map[string]any{
		sidecarControlMethodComputeBalance:      testComputeBalance(true, true),
		sidecarControlMethodComputeCharges:      &sidecar.ComputeChargesView{Charges: []*sidecar.ComputeChargeViewRow{}, NextCursor: "next"},
		sidecarControlMethodComputeSpend:        &sidecar.ComputeSpendView{By: sidecar.ComputeSpendByModel},
		sidecarControlMethodComputeCheckout:     &sidecar.ComputeCheckout{Url: "https://checkout.example/c/test"},
		sidecarControlMethodComputeRedeem:       &sidecarComputeRefusalReply{Refusal: &sidecarComputeRefusal{Code: "code_unknown"}},
		sidecarControlMethodComputeAutoRenewSet: &sidecarComputeRefusalReply{Refusal: &sidecarComputeRefusal{Code: "card_required"}},
		sidecarControlMethodComputeBudgetRaise:  &sidecar.ComputeBudgetRaiseResult{Outcome: sidecar.ComputeBudgetRaisePending},
	})
	compute := newSidecarCompute(&sidecarComputeArgs{remote: daemon})
	if _, err := compute.Balance(); err != nil {
		t.Fatal(err)
	}
	if answer, err := compute.Charges(`{"cursor":"page-2"}`); err != nil || !json.Valid([]byte(answer)) {
		t.Fatalf("Charges: %s, %v", answer, err)
	}
	if answer, err := compute.Spend(`{"by":"model"}`); err != nil || !json.Valid([]byte(answer)) {
		t.Fatalf("Spend: %s, %v", answer, err)
	}
	if url, err := compute.Checkout(`{"pack_credits":25,"auto_renew":"threshold"}`); err != nil || url != "https://checkout.example/c/test" {
		t.Fatalf("Checkout: %s, %v", url, err)
	}
	if answer, err := compute.Redeem("TESTCODE0001"); err != nil || answer != `{"refusal":{"code":"code_unknown"}}` {
		t.Fatalf("Redeem: %s, %v", answer, err)
	}
	err := compute.AutoRenew(`{"mode":"threshold","on":true}`)
	if err == nil || err.Error() != "compute refused: card_required" {
		t.Fatalf("AutoRenew: %v", err)
	}
	if answer, err := compute.RaiseBudget(`{"agent_id":"` + sidecar.NewId().String() + `","period":"day","amount_nano_usd":10000000000,"current_nano_usd":5000000000}`); err != nil || !json.Valid([]byte(answer)) {
		t.Fatalf("RaiseBudget: %s, %v", answer, err)
	}

	calls := daemon.taken()
	want := []struct {
		method    string
		confirmed bool
		field     string
		value     any
	}{
		{method: sidecarControlMethodComputeBalance},
		{method: sidecarControlMethodComputeCharges, field: "cursor", value: "page-2"},
		{method: sidecarControlMethodComputeSpend, field: "by", value: "model"},
		{method: sidecarControlMethodComputeCheckout, field: "pack_credits", value: float64(25)},
		{method: sidecarControlMethodComputeRedeem, confirmed: true, field: "code", value: "TESTCODE0001"},
		{method: sidecarControlMethodComputeAutoRenewSet, confirmed: true, field: "mode", value: "threshold"},
		{method: sidecarControlMethodComputeBudgetRaise, field: "period", value: "day"},
	}
	if len(calls) != len(want) {
		t.Fatalf("%d calls, want %d", len(calls), len(want))
	}
	for i, w := range want {
		params := map[string]any{}
		if err := json.Unmarshal(calls[i].params, &params); err != nil {
			t.Fatal(err)
		}
		if calls[i].method != w.method || calls[i].confirmed != w.confirmed || params["network"] != "the paired network" {
			t.Errorf("call %d: %s confirmed %v, params %v", i, calls[i].method, calls[i].confirmed, params)
		}
		if w.field != "" && params[w.field] != w.value {
			t.Errorf("call %d's %s is %v", i, w.field, params[w.field])
		}
	}
}

// Every exported signature the file adds is one gomobile binds (sidecar_accessors_compute_test.go's check), the app's
// Api among them.
func TestSidecarComputeSignaturesAreGomobileSafe(t *testing.T) {
	findings := testComputeSignatureFindings(t, []string{"sidecar_compute.go"}, map[string]string{"Sidecar": "struct", "Api": "struct"})
	for _, finding := range findings {
		t.Error(finding)
	}
}
