package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/sdk/v2026/sn/evm"
)

// What the main control does: the relay whenever it is claiming, and the device's
// own claim only when it is not, since that spends TAO the user has to hold.
func TestSnClaimActionPrefersTheRelay(t *testing.T) {
	for _, c := range []struct {
		name       string
		claimable  int64
		reason     string
		claimNow   bool
		selfClaim  bool
		needsGas   bool
		wantAction string
	}{
		{"nothing", 0, SnClaimReasonNothingPending, false, true, false, SnClaimActionNone},
		{"nothing, relay down", 0, SnClaimReasonRelayNotRunning, false, true, true, SnClaimActionNone},
		{"under the threshold, tappable", 5, SnClaimReasonUnderThreshold, true, true, false, SnClaimActionClaimNow},
		{"under the threshold, limit used", 5, SnClaimReasonUnderThreshold, false, true, false, SnClaimActionWait},
		{"queued", 5, SnClaimReasonQueued, false, true, false, SnClaimActionWait},
		{"queued, tappable to go first", 5, SnClaimReasonQueued, true, true, false, SnClaimActionClaimNow},
		{"cap reached", 5, SnClaimReasonCapReached, false, true, false, SnClaimActionWait},
		{"refused", 5, SnClaimReasonRefused, false, true, false, SnClaimActionWait},
		// the relay's stops fall back to the device's own claim
		{"relay not running", 5, SnClaimReasonRelayNotRunning, false, true, false, SnClaimActionSelfClaim},
		{"relay paused", 5, SnClaimReasonRelayPaused, false, true, false, SnClaimActionSelfClaim},
		{"relay paused, no gas", 5, SnClaimReasonRelayPaused, false, true, true, SnClaimActionTopUpGas},
		{"relay not running, no gas", 5, SnClaimReasonRelayNotRunning, false, true, true, SnClaimActionTopUpGas},
		// and with no way to self-claim there is only waiting
		{"relay down, no self-claim", 5, SnClaimReasonRelayNotRunning, false, false, false, SnClaimActionWait},
	} {
		account := &SnClaimRelayAccount{ClaimableRao: c.claimable, Reason: c.reason, ClaimNowAvailable: c.claimNow}
		if action := snClaimAction(account, c.selfClaim, c.needsGas); action != c.wantAction {
			t.Errorf("%s: action %s, want %s", c.name, action, c.wantAction)
		}
	}
	if action := snClaimAction(nil, true, false); action != SnClaimActionNone {
		t.Errorf("no account: action %s", action)
	}
}

func testClaimRelayAnswer(state string, accounts ...*SnClaimRelayAccount) *SnClaimRelayResult {
	list := NewSnClaimRelayAccountList()
	list.addAll(accounts...)
	return &SnClaimRelayResult{State: state, AutomaticThresholdTaoRao: 12_500_000, TransferFloorTaoRao: 100_000,
		AutomaticThresholdRao: 3_000_000_000, TransferFloorRao: 24_000_000, ClaimNowPeriodDays: 30, Accounts: list}
}

// The top level is the connected wallet's account, and each account has its own
// action. Only the connected wallet's can be self-claimed, since that is what
// SnLedgerClaim claims.
func TestSnClaimStatusBuildDrawsThePrimaryAccount(t *testing.T) {
	const connected, other = "connected", "other"
	build := func() *SnClaimStatusResult {
		return snClaimStatusBuild(testClaimRelayAnswer("running",
			&SnClaimRelayAccount{ColdkeySs58: other, ClaimableRao: 100_000_000_000, Reason: SnClaimReasonQueued},
			&SnClaimRelayAccount{ColdkeySs58: connected, ClaimableRao: 500_000_000, ValueTaoRao: 2_000_000, PendingRao: 7, Reason: SnClaimReasonUnderThreshold, ClaimNowAvailable: true},
		), connected, true, false)
	}
	result := build()
	if result.RelayState != "running" || result.ColdkeySs58 != connected || result.Action != SnClaimActionClaimNow || result.Reason != SnClaimReasonUnderThreshold {
		t.Fatalf("result %+v", result)
	}
	if result.ClaimableRao != 500_000_000 || result.ValueTaoRao != 2_000_000 || result.PendingRao != 7 || !result.ClaimNowAvailable {
		t.Fatalf("amounts %+v", result)
	}
	if !result.SelfClaimAvailable || result.SelfClaimNeedsGas {
		t.Fatalf("self claim %+v", result)
	}
	if result.AutomaticThresholdRao != 3_000_000_000 || result.TransferFloorRao != 24_000_000 || result.ClaimNowPeriodDays != 30 {
		t.Fatalf("policy %+v", result)
	}
	// the policy's own TAO figures, for "once it's worth about 0.0125 TAO"
	if result.AutomaticThresholdTaoRao != 12_500_000 || result.TransferFloorTaoRao != 100_000 {
		t.Fatalf("policy in TAO %+v", result)
	}
	accounts := result.Accounts.getAll()
	if len(accounts) != 2 || accounts[0].Connected || !accounts[1].Connected || accounts[0].Action != SnClaimActionWait || accounts[1].Action != SnClaimActionClaimNow {
		t.Fatalf("accounts %+v %+v", accounts[0], accounts[1])
	}

	// with the relay down the connected wallet's falls back and another coldkey
	// on the network has nothing the device can do
	result = snClaimStatusBuild(testClaimRelayAnswer(SnClaimRelayStateUnavailable,
		&SnClaimRelayAccount{ColdkeySs58: other, ClaimableRao: 100_000_000_000, Reason: SnClaimReasonRelayNotRunning},
		&SnClaimRelayAccount{ColdkeySs58: connected, ClaimableRao: 100_000_000_000, Reason: SnClaimReasonRelayNotRunning},
	), connected, true, true)
	accounts = result.Accounts.getAll()
	if accounts[0].Action != SnClaimActionWait || accounts[1].Action != SnClaimActionTopUpGas || result.Action != SnClaimActionTopUpGas || !result.SelfClaimNeedsGas {
		t.Fatalf("accounts %+v %+v, result %+v", accounts[0], accounts[1], result)
	}

	// without a wallet, the first with something to claim
	result = snClaimStatusBuild(testClaimRelayAnswer("running",
		&SnClaimRelayAccount{ColdkeySs58: connected, Reason: SnClaimReasonNothingPending},
		&SnClaimRelayAccount{ColdkeySs58: other, ClaimableRao: 5, Reason: SnClaimReasonQueued},
	), "", true, false)
	if result.ColdkeySs58 != other || result.Action != SnClaimActionWait || result.SelfClaimAvailable {
		t.Fatalf("result %+v", result)
	}

	// of the connected wallet's accounts, one with something to claim
	result = snClaimStatusBuild(testClaimRelayAnswer("running",
		&SnClaimRelayAccount{ColdkeySs58: other, ClaimableRao: 5, Reason: SnClaimReasonQueued},
		&SnClaimRelayAccount{ColdkeySs58: connected, NoId: 1, Reason: SnClaimReasonNothingPending},
		&SnClaimRelayAccount{ColdkeySs58: connected, NoId: 2, ClaimableRao: 7, Reason: SnClaimReasonQueued},
	), connected, true, false)
	if result.ColdkeySs58 != connected || result.ClaimableRao != 7 {
		t.Fatalf("result %+v", result)
	}

	// no accounts: nothing to do, and the connected wallet is still named
	result = snClaimStatusBuild(testClaimRelayAnswer("no_key"), connected, true, false)
	if result.RelayState != "no_key" || result.ColdkeySs58 != connected || result.Action != SnClaimActionNone || result.Reason != SnClaimReasonNothingPending || result.Accounts.Len() != 0 {
		t.Fatalf("result %+v", result)
	}
}

// A balance the relay's price puts under the floor would revert: the fallback is
// not offered for it. Without a price the claim's own preflight decides.
func TestSnClaimStatusSelfClaimRespectsTheFloor(t *testing.T) {
	under := &SnClaimRelayAccount{ColdkeySs58: "a", ClaimableRao: 23_999_999, Reason: SnClaimReasonRelayPaused}
	result := snClaimStatusBuild(testClaimRelayAnswer("griefing_stop", under), "a", true, false)
	if result.SelfClaimAvailable || result.Action != SnClaimActionWait {
		t.Fatalf("under the floor %+v", result)
	}
	at := &SnClaimRelayAccount{ColdkeySs58: "a", ClaimableRao: 24_000_000, Reason: SnClaimReasonRelayPaused}
	result = snClaimStatusBuild(testClaimRelayAnswer("griefing_stop", at), "a", true, false)
	if !result.SelfClaimAvailable || result.Action != SnClaimActionSelfClaim {
		t.Fatalf("at the floor %+v", result)
	}
	noPrice := testClaimRelayAnswer("not_running", &SnClaimRelayAccount{ColdkeySs58: "a", ClaimableRao: 1, Reason: SnClaimReasonRelayNotRunning})
	noPrice.TransferFloorRao = 0
	result = snClaimStatusBuild(noPrice, "a", true, false)
	if !result.SelfClaimAvailable || result.Action != SnClaimActionSelfClaim {
		t.Fatalf("without a price %+v", result)
	}
	// a device whose chain settings cannot send a claim has only waiting
	result = snClaimStatusBuild(testClaimRelayAnswer("not_running", &SnClaimRelayAccount{ColdkeySs58: "a", ClaimableRao: 100_000_000, Reason: SnClaimReasonRelayNotRunning}), "a", false, false)
	if result.SelfClaimAvailable || result.Action != SnClaimActionWait {
		t.Fatalf("not configured %+v", result)
	}
}

// testClaimRelayService answers the relay's two routes and the public ledger.
type testClaimRelayService struct {
	stateLock    sync.Mutex
	relayStatus  int
	relayBody    string
	nowStatus    int
	nowBody      string
	nowArgs      []string
	authorized   []string
	relayQueries []string
	ledgerStatus int
	ledgerBody   string
}

func (self *testClaimRelayService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	switch r.URL.Path {
	case "/sn/claim-relay":
		self.authorized = append(self.authorized, r.Header.Get("Authorization"))
		self.relayQueries = append(self.relayQueries, r.URL.RawQuery)
		w.WriteHeader(self.relayStatus)
		io.WriteString(w, self.relayBody)
	case "/sn/claim-relay/claim-now":
		body, _ := io.ReadAll(r.Body)
		self.authorized = append(self.authorized, r.Header.Get("Authorization"))
		self.nowArgs = append(self.nowArgs, r.Method+" "+string(body))
		w.WriteHeader(self.nowStatus)
		io.WriteString(w, self.nowBody)
	case "/sn/ledger/account":
		w.WriteHeader(self.ledgerStatus)
		io.WriteString(w, self.ledgerBody)
	default:
		http.NotFound(w, r)
	}
}

func testClaimRelayDevice(t *testing.T, service http.Handler, chain http.Handler, wallet string) *snDevice {
	t.Helper()
	operator := httptest.NewServer(testApiHandler(service))
	t.Cleanup(operator.Close)
	chainServer := httptest.NewServer(chain)
	t.Cleanup(chainServer.Close)
	space, localState := testingLogoutCauseSpace(t, operator.URL)
	settings := SnTestnetChainSettings()
	settings.SetRpcUrls(nil)
	settings.AddRpcUrl(chainServer.URL)
	settings.Vault2Address = "0x3333333333333333333333333333333333333333"
	if err := localState.SetSnChainSettings(settings); err != nil {
		t.Fatal(err)
	}
	ctx, api := newTestApiForURL(t, operator.URL)
	device := &snDevice{ctx: ctx, networkSpace: space, api: func() *Api { return api }, clientId: connect.NewId(), state: newDeviceSn()}
	if wallet != "" {
		device.setSnWallet(&SnWallet{ColdkeySs58: wallet})
	}
	return device
}

func testClaimRelayJson(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The whole status on a device: the server's answer, the wallet and the gas key,
// and a chain that says whether the gas key can pay.
func TestSnClaimStatusOnADevice(t *testing.T) {
	_, aliceSs58 := testAliceSs58(t)
	relayJson := func(state string, reason string, tappable bool) string {
		return fmt.Sprintf(`{"state":%q,"automatic_threshold_tao_rao":12500000,"transfer_floor_tao_rao":100000,"automatic_threshold_rao":3000000000,"transfer_floor_rao":24000000,"claim_now_period_days":30,
			"accounts":[{"coldkey_ss58":%q,"no_id":1,"index":7,"claimable_rao":1000000000,"value_tao_rao":4224000,"pending_rao":5,"reason":%q,"claim_now_available":%t}]}`, state, aliceSs58, reason, tappable)
	}
	service := &testClaimRelayService{relayStatus: http.StatusOK, relayBody: relayJson("running", SnClaimReasonUnderThreshold, true), ledgerStatus: http.StatusInternalServerError, ledgerBody: "ledger down"}
	chain := &fakeChain{t: t, block: 1000, claimed: map[string]bool{}, receipts: map[string]bool{}}
	device := testClaimRelayDevice(t, service, chain, aliceSs58)
	ctx := context.Background()

	result := device.snClaimStatus(ctx)
	if result.Error != nil || result.RelayState != "running" || result.Action != SnClaimActionClaimNow || result.Reason != SnClaimReasonUnderThreshold || result.ColdkeySs58 != aliceSs58 {
		t.Fatalf("running: %+v (%+v)", result, result.Error)
	}
	if !result.SelfClaimAvailable || result.SelfClaimNeedsGas || result.ClaimableRao != 1_000_000_000 || result.ValueTaoRao != 4_224_000 || result.PendingRao != 5 {
		t.Fatalf("running: %+v", result)
	}

	// the relay stopped and the gas key has enough: the device's own claim
	service.relayBody = relayJson("griefing_stop", SnClaimReasonRelayPaused, false)
	result = device.snClaimStatus(ctx)
	if result.Action != SnClaimActionSelfClaim || !result.SelfClaimAvailable || result.SelfClaimNeedsGas {
		t.Fatalf("paused: %+v", result)
	}

	// and it does not: the limit at the node's price and 10%, 2.6M × 1.1 gwei
	short := new(big.Int).Sub(big.NewInt(2_860_000_000_000_000), big.NewInt(1))
	chain.balance = short
	result = device.snClaimStatus(ctx)
	if result.Action != SnClaimActionTopUpGas || !result.SelfClaimNeedsGas {
		t.Fatalf("no gas: %+v", result)
	}
	chain.balance = big.NewInt(2_860_000_000_000_000)
	if result = device.snClaimStatus(ctx); result.Action != SnClaimActionSelfClaim || result.SelfClaimNeedsGas {
		t.Fatalf("exactly enough gas: %+v", result)
	}
}

// The status prices the device's own claim as signing it does, so what it says
// about gas is what the claim would find.
func TestSnClaimMaxFeePerGasIsWhatSigningPays(t *testing.T) {
	chain := &fakeChain{t: t, block: 1000, claimed: map[string]bool{}, receipts: map[string]bool{}}
	server := httptest.NewServer(chain)
	defer server.Close()
	settings := SnTestnetChainSettings()
	settings.SetRpcUrls(nil)
	settings.AddRpcUrl(server.URL)
	client := settings.chainClient()
	key, err := evm.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, txType := range []string{SnTxTypeLegacy, SnTxTypeEip1559} {
		const gas = uint64(2_600_000)
		turn, err := newSnKeyTurns().take(ctx, snGasKeyId{chainId: 945, address: evm.AddressOf(key)})
		if err != nil {
			t.Fatal(err)
		}
		_, maxCost, err := snSignClaim(ctx, client, big.NewInt(945), txType, turn, key, [20]byte{1}, []byte{1}, gas)
		turn.release()
		if err != nil {
			t.Fatalf("%s: %v", txType, err)
		}
		feePerGas, err := snClaimMaxFeePerGas(ctx, client, txType)
		if err != nil {
			t.Fatalf("%s: %v", txType, err)
		}
		if want := new(big.Int).Mul(feePerGas, new(big.Int).SetUint64(gas)); want.Cmp(maxCost) != 0 {
			t.Fatalf("%s: the status says %s, signing %s", txType, want, maxCost)
		}
	}
}

// A server that cannot answer leaves the device's own claim working, with the
// balances from the public ledger, and an unreadable ledger reads as unavailable,
// never as zero.
func TestSnClaimStatusFallsBackToTheLedger(t *testing.T) {
	alice, aliceSs58 := testAliceSs58(t)
	ledgerBody := string(testLedgerJson(t, testLedgerDocument(alice)))
	service := &testClaimRelayService{relayStatus: http.StatusNotFound, relayBody: "no such route", ledgerStatus: http.StatusOK, ledgerBody: ledgerBody}
	chain := &fakeChain{t: t, block: 1000, claimed: map[string]bool{}, receipts: map[string]bool{}}
	device := testClaimRelayDevice(t, service, chain, aliceSs58)
	ctx := context.Background()

	result := device.snClaimStatus(ctx)
	if result.Error != nil || result.RelayState != SnClaimRelayStateUnavailable {
		t.Fatalf("no route: %+v (%+v)", result, result.Error)
	}
	want := testLedgerAccounts()
	if result.Accounts.Len() != len(want) {
		t.Fatalf("no route: %d accounts, want %d", result.Accounts.Len(), len(want))
	}
	total := int64(0)
	for index, account := range result.Accounts.getAll() {
		total += account.ClaimableRao
		if account.ClaimableRao != want[index].ClaimableRao || account.NoId != want[index].NoId || account.Index != want[index].Index {
			t.Fatalf("account %d: %+v, want %+v", index, account, want[index])
		}
		if account.ClaimNowAvailable {
			t.Fatalf("account %d offers Claim now without the relay", index)
		}
		switch {
		case 0 < account.ClaimableRao && (account.Reason != SnClaimReasonRelayNotRunning || account.Action != SnClaimActionSelfClaim):
			t.Fatalf("account %d: %+v", index, account)
		case account.ClaimableRao <= 0 && (account.Reason != SnClaimReasonNothingPending || account.Action != SnClaimActionNone):
			t.Fatalf("account %d: %+v", index, account)
		}
	}
	if total <= 0 {
		t.Fatal("the fixture has nothing claimable")
	}

	// the server is up and cannot read the balances: its state still reaches the
	// app, and the ledger's balances stand in
	service.relayStatus = http.StatusOK
	service.relayBody = `{"state":"cap_reached","accounts":[],"error":{"message":"The balances could not be read."}}`
	result = device.snClaimStatus(ctx)
	if result.Error != nil || result.RelayState != "cap_reached" || result.Accounts.Len() != len(want) {
		t.Fatalf("relay error: %+v (%+v)", result, result.Error)
	}

	// neither can be read: unavailable, never zero
	service.ledgerStatus = http.StatusInternalServerError
	service.ledgerBody = "ledger down"
	result = device.snClaimStatus(ctx)
	if result.Error == nil || result.Error.Code != SnErrorCodeLedgerUnavailable || result.RelayState != "cap_reached" || result.Action != SnClaimActionNone {
		t.Fatalf("nothing readable: %+v (%+v)", result, result.Error)
	}
	service.relayStatus = http.StatusInternalServerError
	service.relayBody = "down"
	result = device.snClaimStatus(ctx)
	if result.Error == nil || result.Error.Code != SnErrorCodeLedgerUnavailable || result.RelayState != SnClaimRelayStateUnavailable || result.Action != SnClaimActionNone {
		t.Fatalf("nothing answers: %+v (%+v)", result, result.Error)
	}
}

// Without a wallet the network's accounts still read, and with neither a wallet nor
// a relay there is nothing to say but that it could not be read.
func TestSnClaimStatusWithoutAWallet(t *testing.T) {
	_, aliceSs58 := testAliceSs58(t)
	service := &testClaimRelayService{relayStatus: http.StatusOK, ledgerStatus: http.StatusOK, ledgerBody: "{}",
		relayBody: fmt.Sprintf(`{"state":"running","accounts":[{"coldkey_ss58":%q,"no_id":1,"index":7,"claimable_rao":5,"reason":"queued"}]}`, aliceSs58)}
	chain := &fakeChain{t: t, block: 1000, claimed: map[string]bool{}, receipts: map[string]bool{}}
	device := testClaimRelayDevice(t, service, chain, "")
	result := device.snClaimStatus(context.Background())
	if result.Error != nil || result.ColdkeySs58 != aliceSs58 || result.Action != SnClaimActionWait || result.SelfClaimAvailable {
		t.Fatalf("result %+v", result)
	}
	service.relayStatus, service.relayBody = http.StatusBadGateway, "down"
	result = device.snClaimStatus(context.Background())
	if result.Error == nil || result.Error.Code != SnErrorCodeLedgerUnavailable || result.Action != SnClaimActionNone {
		t.Fatalf("result %+v", result)
	}
}

// A tap names the connected wallet unless it names another, and the server's limit
// reaches the app. Nothing is asked of a server for an address that is not one.
func TestSnClaimNow(t *testing.T) {
	_, aliceSs58 := testAliceSs58(t)
	service := &testClaimRelayService{relayStatus: http.StatusOK, relayBody: `{}`, nowStatus: http.StatusOK, nowBody: `{"requested":true,"next_at_millis":1700000000000}`}
	chain := &fakeChain{t: t, block: 1000, claimed: map[string]bool{}, receipts: map[string]bool{}}
	device := testClaimRelayDevice(t, service, chain, aliceSs58)
	ctx := context.Background()

	result := device.snClaimNow(ctx, "")
	if result.Error != nil || !result.Requested || result.NextAtMillis != 1_700_000_000_000 {
		t.Fatalf("taken: %+v", result)
	}
	if len(service.nowArgs) != 1 || !strings.HasPrefix(service.nowArgs[0], "POST ") || !strings.Contains(service.nowArgs[0], `"coldkey_ss58":"`+aliceSs58+`"`) {
		t.Fatalf("args %q", service.nowArgs)
	}

	service.nowBody = `{"requested":false,"reason":"limit","next_at_millis":1800000000000}`
	result = device.snClaimNow(ctx, aliceSs58)
	if result.Error != nil || result.Requested || result.Reason != SnClaimNowReasonLimit || result.NextAtMillis != 1_800_000_000_000 {
		t.Fatalf("limit: %+v", result)
	}

	service.nowBody = `{"requested":false,"error":{"message":"The balances could not be read."}}`
	if result = device.snClaimNow(ctx, ""); result.Error == nil || result.Error.Code != SnErrorCodeLedgerUnavailable || result.Requested {
		t.Fatalf("unavailable: %+v", result)
	}
	service.nowStatus, service.nowBody = http.StatusInternalServerError, "down"
	if result = device.snClaimNow(ctx, ""); result.Error == nil || result.Error.Code != SnErrorCodeServer || result.Requested {
		t.Fatalf("down: %+v", result)
	}

	asked := len(service.nowArgs)
	if result = device.snClaimNow(ctx, "not an address"); result.Error == nil || result.Error.Code != SnErrorCodeInvalidAddress {
		t.Fatalf("not an address: %+v", result)
	}
	noWallet := testClaimRelayDevice(t, service, chain, "")
	if result = noWallet.snClaimNow(ctx, ""); result.Error == nil || result.Error.Code != SnErrorCodeConnectWalletFirst {
		t.Fatalf("no wallet: %+v", result)
	}
	if len(service.nowArgs) != asked {
		t.Fatalf("asked the server %d more", len(service.nowArgs)-asked)
	}
}

// The status arrives through the callback an app registers, on both device kinds.
func TestSnClaimStatusCallback(t *testing.T) {
	_, aliceSs58 := testAliceSs58(t)
	service := &testClaimRelayService{relayStatus: http.StatusOK, nowStatus: http.StatusOK, nowBody: `{"requested":true}`,
		relayBody: fmt.Sprintf(`{"state":"running","accounts":[{"coldkey_ss58":%q,"no_id":1,"index":7,"claimable_rao":5,"reason":"queued"}]}`, aliceSs58)}
	chain := &fakeChain{t: t, block: 1000, claimed: map[string]bool{}, receipts: map[string]bool{}}
	device := testClaimRelayDevice(t, service, chain, aliceSs58)

	done := make(chan *SnClaimStatusResult, 1)
	device.SnClaimStatus(connect.NewApiCallback[*SnClaimStatusResult](func(result *SnClaimStatusResult, err error) {
		if err != nil {
			t.Error(err)
		}
		done <- result
	}))
	if result := <-done; result.Action != SnClaimActionWait || result.RelayState != "running" {
		t.Fatalf("result %+v", result)
	}
	tapped := make(chan *SnClaimNowResult, 1)
	device.SnClaimNow("", connect.NewApiCallback[*SnClaimNowResult](func(result *SnClaimNowResult, err error) {
		if err != nil {
			t.Error(err)
		}
		tapped <- result
	}))
	if result := <-tapped; !result.Requested {
		t.Fatalf("result %+v", result)
	}
}

// What crosses to the apps is json-shaped as the cgo layer sends it.
func TestSnClaimStatusJsonIsStable(t *testing.T) {
	result := snClaimStatusBuild(testClaimRelayAnswer("running", &SnClaimRelayAccount{ColdkeySs58: "a", ClaimableRao: 5, Reason: SnClaimReasonQueued, ClaimNowAvailable: true}), "a", true, false)
	raw := testClaimRelayJson(t, result)
	for _, part := range []string{`"relay_state":"running"`, `"action":"claim_now"`, `"reason":"queued"`, `"claimable_rao":5`, `"claim_now_available":true`, `"self_claim_available":false`, `"accounts":[{`, `"connected":true`, `"shows_tile":true`, `"shows_claimed":false`} {
		if !strings.Contains(raw, part) {
			t.Errorf("%s lacks %s", raw, part)
		}
	}
	empty := testClaimRelayJson(t, snClaimStatusBuild(nil, "", false, false))
	if !strings.Contains(empty, `"accounts":[]`) || !strings.Contains(empty, `"shows_tile":false`) {
		t.Errorf("an empty list must marshal as [], and no tile shows: %s", empty)
	}
	claimed := snClaimStatusBuild(testClaimRelayAnswer("running", &SnClaimRelayAccount{ColdkeySs58: "a", Reason: SnClaimReasonNothingPending, ClaimTxHash: testClaimTxHash, ClaimAmountRao: 842_000_000}), "", true, false)
	snClaimStatusLinkTransactions(claimed, SnTestnetChainSettings())
	raw = testClaimRelayJson(t, claimed)
	for _, part := range []string{`"shows_claimed":true`, `"claim_tx_hash":"` + testClaimTxHash + `"`, `"claim_tx_url":"https://evm-testnet.taostats.io/tx/` + testClaimTxHash + `"`, `"claim_amount_rao":842000000`} {
		if !strings.Contains(raw, part) {
			t.Errorf("%s lacks %s", raw, part)
		}
	}
}

const testClaimTxHash = "0x8a5f0c9e1d2b3a4f5e6d7c8b9a0f1e2d3c4b5a69788796a5b4c3d2e1f0a9b8c7"

// When a network with no wallet on this device shows the Settlement v2 tile: the
// status was read and its primary account has a claimable or pending balance, as
// Apple's Earnings first drew it, or shows the claimed state, which is reached from
// the tile. A device with its own wallet's ledger tile shows that one, so the sdk
// says nothing about it.
func TestSnClaimStatusShowsTile(t *testing.T) {
	for _, c := range []struct {
		name      string
		connected string
		accounts  []*SnClaimRelayAccount
		want      bool
	}{
		{"claimable", "", []*SnClaimRelayAccount{{ColdkeySs58: "a", ClaimableRao: 5, Reason: SnClaimReasonUnderThreshold}}, true},
		{"pending only", "", []*SnClaimRelayAccount{{ColdkeySs58: "a", PendingRao: 5, Reason: SnClaimReasonNothingPending}}, true},
		{"claimable and pending", "", []*SnClaimRelayAccount{{ColdkeySs58: "a", ClaimableRao: 5, PendingRao: 5, Reason: SnClaimReasonQueued}}, true},
		{"nothing", "", []*SnClaimRelayAccount{{ColdkeySs58: "a", Reason: SnClaimReasonNothingPending}}, false},
		{"no accounts", "", nil, false},
		// the relay down: the balance is still the network's, and URnetwork waits
		{"relay down", "", []*SnClaimRelayAccount{{ColdkeySs58: "a", ClaimableRao: 5, Reason: SnClaimReasonRelayNotRunning}}, true},
		// the top level decides, as it does on Apple: the connected wallet's account
		// first, and another of the network's with a balance does not
		{"the connected wallet's has nothing", "a", []*SnClaimRelayAccount{{ColdkeySs58: "b", ClaimableRao: 5, Reason: SnClaimReasonQueued}, {ColdkeySs58: "a", Reason: SnClaimReasonNothingPending}}, false},
		{"another of the network's", "", []*SnClaimRelayAccount{{ColdkeySs58: "a", Reason: SnClaimReasonNothingPending}, {ColdkeySs58: "b", ClaimableRao: 5, Reason: SnClaimReasonQueued}}, true},
		// URnetwork claimed it all and nothing is pending: the tile stays, with the
		// claimed state and its transaction
		{"claimed, nothing left", "", []*SnClaimRelayAccount{{ColdkeySs58: "a", Reason: SnClaimReasonNothingPending, ClaimTxHash: testClaimTxHash, ClaimAmountRao: 9}}, true},
		// a transaction the sdk cannot name is none, and no claimed state holds the tile
		{"claimed, a hash that is none", "", []*SnClaimRelayAccount{{ColdkeySs58: "a", Reason: SnClaimReasonNothingPending, ClaimTxHash: "0x1234", ClaimAmountRao: 9}}, false},
		// the primary's state decides: another account's claim holds no tile
		{"another account's claim", "a", []*SnClaimRelayAccount{{ColdkeySs58: "a", Reason: SnClaimReasonNothingPending}, {ColdkeySs58: "b", Reason: SnClaimReasonNothingPending, ClaimTxHash: testClaimTxHash}}, false},
		// past int64 a sum would wrap; a balance is still a balance
		{"huge", "", []*SnClaimRelayAccount{{ColdkeySs58: "a", ClaimableRao: math.MaxInt64, PendingRao: math.MaxInt64, Reason: SnClaimReasonQueued}}, true},
	} {
		result := snClaimStatusBuild(testClaimRelayAnswer("running", c.accounts...), c.connected, true, false)
		if result.ShowsTile != c.want {
			t.Errorf("%s: shows the tile %t, want %t (%+v)", c.name, result.ShowsTile, c.want, result)
		}
	}
	// a status that could not be read is unavailable, never zero, and no tile
	if result := snClaimStatusBuild(nil, "", true, false); result.ShowsTile || result.ShowsClaimed {
		t.Fatalf("unread %+v", result)
	}
}

// The claimed state: URnetwork's claim is the primary account's latest and nothing
// is claimable now, drawn with the transaction and what it paid. A balance since
// is the pending state again, with the transaction still named. A transaction the
// sdk cannot name is none.
func TestSnClaimStatusShowsClaimedWithTheTransaction(t *testing.T) {
	build := func(accounts ...*SnClaimRelayAccount) *SnClaimStatusResult {
		result := snClaimStatusBuild(testClaimRelayAnswer("running", accounts...), "", true, false)
		snClaimStatusLinkTransactions(result, SnTestnetChainSettings())
		return result
	}
	result := build(&SnClaimRelayAccount{ColdkeySs58: "a", Reason: SnClaimReasonNothingPending, ClaimNowReason: SnClaimReasonNothingPending, ClaimNowNextAtMillis: 1_800_000_000_000, ClaimTxHash: testClaimTxHash, ClaimAmountRao: 842_000_000})
	if !result.ShowsClaimed || result.ClaimTxHash != testClaimTxHash || result.ClaimAmountRao != 842_000_000 || result.ClaimTxUrl != "https://evm-testnet.taostats.io/tx/"+testClaimTxHash {
		t.Fatalf("claimed %+v", result)
	}
	// the tile stays, so the claimed state is reached from it
	if !result.ShowsTile {
		t.Fatalf("claimed, no tile %+v", result)
	}
	// "Claim now is available again" goes by the limit, as before
	if result.ClaimNowNextAtMillis != 1_800_000_000_000 || result.ClaimNowAvailable || result.Action != SnClaimActionNone {
		t.Fatalf("claimed %+v", result)
	}
	if account := result.Accounts.getAll()[0]; account.ClaimTxUrl != result.ClaimTxUrl || account.ClaimTxHash != testClaimTxHash {
		t.Fatalf("account %+v", account)
	}

	// a new root since: pending again, and the claim is still the latest
	result = build(&SnClaimRelayAccount{ColdkeySs58: "a", ClaimableRao: 5, Reason: SnClaimReasonUnderThreshold, ClaimNowAvailable: true, ClaimTxHash: testClaimTxHash, ClaimAmountRao: 842_000_000})
	if result.ShowsClaimed || result.ClaimTxHash != testClaimTxHash || result.Action != SnClaimActionClaimNow || !result.ShowsTile {
		t.Fatalf("after a new root %+v", result)
	}

	// nothing claimed by URnetwork, and nothing left: no claimed state, and no tile
	if result = build(&SnClaimRelayAccount{ColdkeySs58: "a", Reason: SnClaimReasonNothingPending}); result.ShowsClaimed || result.ShowsTile || result.ClaimTxHash != "" || result.ClaimTxUrl != "" || result.ClaimAmountRao != 0 {
		t.Fatalf("nothing claimed %+v", result)
	}

	// what is not a transaction hash is none
	for _, hash := range []string{"0x1234", testClaimTxHash + "00", "8a5f" + testClaimTxHash[2:] + "ab", "0x" + strings.Repeat("g", 64), "javascript:alert(1)", "0x" + strings.Repeat("a", 63) + "/"} {
		result = build(&SnClaimRelayAccount{ColdkeySs58: "a", Reason: SnClaimReasonNothingPending, ClaimTxHash: hash, ClaimAmountRao: 7})
		account := result.Accounts.getAll()[0]
		if result.ShowsClaimed || result.ShowsTile || result.ClaimTxHash != "" || result.ClaimTxUrl != "" || result.ClaimAmountRao != 0 || account.ClaimTxHash != "" || account.ClaimTxUrl != "" || account.ClaimAmountRao != 0 {
			t.Fatalf("hash %q: %+v %+v", hash, result, account)
		}
	}
	// a negative amount is none
	if result = build(&SnClaimRelayAccount{ColdkeySs58: "a", Reason: SnClaimReasonNothingPending, ClaimTxHash: testClaimTxHash, ClaimAmountRao: -1}); !result.ShowsClaimed || result.ClaimAmountRao != 0 {
		t.Fatalf("negative amount %+v", result)
	}

	// the primary is the one drawn: another account's claim is listed, not drawn
	result = build(
		&SnClaimRelayAccount{ColdkeySs58: "a", ClaimableRao: 5, Reason: SnClaimReasonQueued},
		&SnClaimRelayAccount{ColdkeySs58: "b", Reason: SnClaimReasonNothingPending, ClaimTxHash: testClaimTxHash, ClaimAmountRao: 9},
	)
	if result.ShowsClaimed || result.ClaimTxHash != "" || result.Accounts.getAll()[1].ClaimTxUrl != "https://evm-testnet.taostats.io/tx/"+testClaimTxHash {
		t.Fatalf("another account's claim %+v", result)
	}

	// no explorer in the chain settings: no link, the transaction still named
	result = snClaimStatusBuild(testClaimRelayAnswer("running", &SnClaimRelayAccount{ColdkeySs58: "a", ClaimTxHash: testClaimTxHash}), "", true, false)
	snClaimStatusLinkTransactions(result, &SnChainSettings{})
	if result.ClaimTxUrl != "" || result.ClaimTxHash != testClaimTxHash || !result.ShowsClaimed {
		t.Fatalf("no explorer %+v", result)
	}
	snClaimStatusLinkTransactions(result, nil)
	if result.ClaimTxUrl != "" {
		t.Fatalf("no settings %+v", result)
	}
}

// On a device: the server's answer names the transaction, and the device's own
// chain settings link it.
func TestSnClaimStatusOnADeviceLinksTheTransaction(t *testing.T) {
	_, aliceSs58 := testAliceSs58(t)
	service := &testClaimRelayService{relayStatus: http.StatusOK, ledgerStatus: http.StatusInternalServerError, ledgerBody: "ledger down",
		relayBody: fmt.Sprintf(`{"state":"running","automatic_threshold_tao_rao":12500000,"transfer_floor_tao_rao":100000,"automatic_threshold_rao":3000000000,"transfer_floor_rao":24000000,"claim_now_period_days":30,
			"accounts":[{"coldkey_ss58":%q,"no_id":1,"index":7,"claimable_rao":0,"value_tao_rao":0,"pending_rao":0,"reason":"nothing_pending","claim_now_available":false,"claim_now_reason":"nothing_pending","claim_now_next_at_millis":1800000000000,"claim_tx_hash":%q,"claim_amount_rao":842000000}]}`, aliceSs58, testClaimTxHash)}
	chain := &fakeChain{t: t, block: 1000, claimed: map[string]bool{}, receipts: map[string]bool{}}
	device := testClaimRelayDevice(t, service, chain, "")
	result := device.snClaimStatus(context.Background())
	// claimed in full, nothing pending: the claimed state, on a tile that stays
	if result.Error != nil || !result.ShowsClaimed || !result.ShowsTile || result.ColdkeySs58 != aliceSs58 {
		t.Fatalf("result %+v (%+v)", result, result.Error)
	}
	want := "https://evm-testnet.taostats.io/tx/" + testClaimTxHash
	if result.ClaimTxHash != testClaimTxHash || result.ClaimTxUrl != want || result.ClaimAmountRao != 842_000_000 || result.ClaimNowNextAtMillis != 1_800_000_000_000 {
		t.Fatalf("result %+v", result)
	}
	if account := result.Accounts.getAll()[0]; account.ClaimTxUrl != want || account.ClaimTxHash != testClaimTxHash || account.ClaimAmountRao != 842_000_000 {
		t.Fatalf("account %+v", account)
	}

	// a status that could not be read names nothing and shows no tile
	service.relayStatus, service.relayBody = http.StatusBadGateway, "down"
	if result = device.snClaimStatus(context.Background()); result.Error == nil || result.ShowsTile || result.ShowsClaimed || result.ClaimTxHash != "" || result.ClaimTxUrl != "" {
		t.Fatalf("unread %+v", result)
	}
}

// A device and a page with no device share one set of rules (snClaimStatusOf, and
// the web's SnClaimStatusOf in sn_claim_relay_js.go): the public ledger is read
// only for an answer without balances and with a wallet to read it for, and the
// gas only when the device's own claim could pay something.
func TestSnClaimStatusOfReadsOnlyWhatItNeeds(t *testing.T) {
	ledgerReads, gasReads := 0, 0
	ledger := func() (*SnClaimRelayAccountList, *SnLedgerUnavailablePoolList, *SnError) {
		ledgerReads++
		list := NewSnClaimRelayAccountList()
		list.Add(&SnClaimRelayAccount{ColdkeySs58: "a", ClaimableRao: 5, Reason: SnClaimReasonRelayNotRunning, ClaimNowReason: SnClaimReasonRelayNotRunning})
		return list, NewSnLedgerUnavailablePoolList(), nil
	}
	needsGas := func() bool {
		gasReads++
		return true
	}
	paused := func() *SnClaimRelayResult {
		return testClaimRelayAnswer("griefing_stop", &SnClaimRelayAccount{ColdkeySs58: "a", ClaimableRao: 100_000_000, Reason: SnClaimReasonRelayPaused})
	}

	// an answer with balances: no ledger read, and the gas, since the device's own
	// claim could pay
	result := snClaimStatusOf(paused(), nil, "a", true, ledger, needsGas, nil)
	if ledgerReads != 0 || gasReads != 1 || result.Action != SnClaimActionTopUpGas || !result.SelfClaimNeedsGas {
		t.Fatalf("answered: %d ledger, %d gas reads, %+v", ledgerReads, gasReads, result)
	}
	// a device that cannot send its own claim, or with nothing of the wallet's to
	// claim, never reads the gas
	snClaimStatusOf(paused(), nil, "a", false, ledger, needsGas, nil)
	snClaimStatusOf(paused(), nil, "b", true, ledger, needsGas, nil)
	if gasReads != 1 {
		t.Fatalf("%d gas reads", gasReads)
	}
	// no gas read at all, as on a page: never short
	if result = snClaimStatusOf(paused(), nil, "a", true, ledger, nil, nil); result.Action != SnClaimActionSelfClaim || result.SelfClaimNeedsGas {
		t.Fatalf("no gas read: %+v", result)
	}

	// no answer and no wallet: nothing to read the ledger for
	result = snClaimStatusOf(nil, errors.New("Failed to fetch"), "", true, ledger, needsGas, nil)
	if ledgerReads != 0 || result.Error == nil || result.Error.Code != SnErrorCodeLedgerUnavailable || result.Error.Message != "Failed to fetch" || result.RelayState != SnClaimRelayStateUnavailable {
		t.Fatalf("no wallet: %d ledger reads, %+v (%+v)", ledgerReads, result, result.Error)
	}
	// no answer, with a wallet: the ledger once, and its balances stand in
	result = snClaimStatusOf(nil, nil, "a", true, ledger, needsGas, nil)
	if ledgerReads != 1 || result.Error != nil || result.Action != SnClaimActionTopUpGas || result.RelayState != SnClaimRelayStateUnavailable {
		t.Fatalf("fallback: %d ledger reads, %+v (%+v)", ledgerReads, result, result.Error)
	}
	// nothing that reads the ledger: unavailable, never zero
	result = snClaimStatusOf(nil, nil, "a", true, nil, needsGas, nil)
	if result.Error == nil || result.Error.Message != "the claim relay did not answer" || result.Action != SnClaimActionNone || result.ShowsTile {
		t.Fatalf("no ledger: %+v (%+v)", result, result.Error)
	}
	// an answer that says it could not read the balances keeps its state
	failed := testClaimRelayAnswer("cap_reached")
	failed.Error = &SnClaimRelayError{Message: "The balances could not be read."}
	result = snClaimStatusOf(failed, nil, "", true, ledger, needsGas, nil)
	if result.Error == nil || result.Error.Message != "The balances could not be read." || result.RelayState != "cap_reached" {
		t.Fatalf("relay error: %+v (%+v)", result, result.Error)
	}
}

// The ledger's read as the relay's accounts: what a claim pays now, what a root
// not final yet adds, and no relay behind them. A read not made, or without
// accounts or a code, is the ledger unavailable, never zero.
func TestSnClaimLedgerRelayAccounts(t *testing.T) {
	ledgerAccounts := NewSnLedgerAccountList()
	ledgerAccounts.Add(&SnLedgerAccount{NoId: 1, Index: 42, FinalizedRao: 2_100_000_000, ClaimableRao: 842_000_000, Pending: true, PendingRao: 2_200_000_000})
	ledgerAccounts.Add(&SnLedgerAccount{NoId: 2, Index: 7, FinalizedRao: 5, PaidRao: 5})
	accounts, unavailable, snErr := snClaimLedgerRelayAccounts(&SnLedgerAccountsResult{Accounts: ledgerAccounts}, "a")
	if snErr != nil || accounts.Len() != 2 || unavailable.Len() != 0 {
		t.Fatalf("accounts %v, unavailable %v, %+v", accounts, unavailable, snErr)
	}
	first, second := accounts.getAll()[0], accounts.getAll()[1]
	if first.ColdkeySs58 != "a" || first.NoId != 1 || first.Index != 42 || first.ClaimableRao != 842_000_000 || first.PendingRao != 100_000_000 ||
		first.Reason != SnClaimReasonRelayNotRunning || first.ClaimNowReason != SnClaimReasonRelayNotRunning || first.ClaimNowAvailable {
		t.Fatalf("first %+v", first)
	}
	if second.ClaimableRao != 0 || second.PendingRao != 0 || second.Reason != SnClaimReasonNothingPending {
		t.Fatalf("second %+v", second)
	}
	for _, c := range []struct {
		name   string
		ledger *SnLedgerAccountsResult
		code   string
		text   string
	}{
		{"not read", nil, SnErrorCodeLedgerUnavailable, "the ledger could not be read"},
		{"no accounts", &SnLedgerAccountsResult{}, SnErrorCodeLedgerUnavailable, "the ledger could not be read"},
		{"no code", &SnLedgerAccountsResult{Error: &SnError{}}, SnErrorCodeLedgerUnavailable, "the ledger could not be read"},
		{"its own code", &SnLedgerAccountsResult{Accounts: NewSnLedgerAccountList(), Error: &SnError{Code: SnErrorCodeChainRpcUnreachable, Message: "rpc down"}}, SnErrorCodeChainRpcUnreachable, "rpc down"},
		// partial, without naming what it lacks: never drawn as whole
		{"partial without its pools", &SnLedgerAccountsResult{Accounts: NewSnLedgerAccountList(), Partial: true}, SnErrorCodeLedgerUnavailable, "the ledger answer is partial and names no unavailable pool"},
	} {
		if accounts, unavailable, snErr := snClaimLedgerRelayAccounts(c.ledger, "a"); accounts != nil || unavailable != nil || snErr == nil || snErr.Code != c.code || snErr.Message != c.text {
			t.Errorf("%s: %v, %v, %+v", c.name, accounts, unavailable, snErr)
		}
	}
}

// A tap through snClaimNowOf, as a page sends it: nothing is posted for an
// address that is not one or with no wallet to name, and a post with no answer is
// the server's failure.
func TestSnClaimNowOfPostsOnlyForAnAddress(t *testing.T) {
	_, aliceSs58 := testAliceSs58(t)
	posts := []string{}
	post := func(answer *SnClaimRelayClaimNowResult, err error) func(*SnClaimRelayClaimNowArgs) (*SnClaimRelayClaimNowResult, error) {
		return func(args *SnClaimRelayClaimNowArgs) (*SnClaimRelayClaimNowResult, error) {
			posts = append(posts, args.ColdkeySs58)
			return answer, err
		}
	}
	wallet := func(coldkey string, ok bool) func() (string, bool) {
		return func() (string, bool) { return coldkey, ok }
	}
	if result := snClaimNowOf("", wallet(" "+aliceSs58+" ", true), post(&SnClaimRelayClaimNowResult{Requested: true}, nil)); !result.Requested || len(posts) != 1 || posts[0] != aliceSs58 {
		t.Fatalf("taken: %+v, posts %q", result, posts)
	}
	if result := snClaimNowOf(aliceSs58, wallet("", false), post(nil, nil)); result.Error == nil || result.Error.Code != SnErrorCodeServer || result.Requested {
		t.Fatalf("no answer: %+v", result)
	}
	if result := snClaimNowOf(aliceSs58, wallet("", false), post(nil, errors.New("Service unavailable"))); result.Error == nil || result.Error.Code != SnErrorCodeServer || result.Error.Message != "Service unavailable" {
		t.Fatalf("failed: %+v", result)
	}
	posted := len(posts)
	if result := snClaimNowOf("", wallet("", false), post(&SnClaimRelayClaimNowResult{Requested: true}, nil)); result.Error == nil || result.Error.Code != SnErrorCodeConnectWalletFirst {
		t.Fatalf("no wallet: %+v", result)
	}
	// a wallet with no coldkey is no address
	if result := snClaimNowOf("", wallet(" ", true), post(&SnClaimRelayClaimNowResult{Requested: true}, nil)); result.Error == nil || result.Error.Code != SnErrorCodeInvalidAddress {
		t.Fatalf("empty wallet: %+v", result)
	}
	if result := snClaimNowOf("not an address", wallet(aliceSs58, true), post(&SnClaimRelayClaimNowResult{Requested: true}, nil)); result.Error == nil || result.Error.Code != SnErrorCodeInvalidAddress {
		t.Fatalf("not an address: %+v", result)
	}
	if len(posts) != posted {
		t.Fatalf("posted %d more", len(posts)-posted)
	}
}

// A coldkey's accounts in the operator's pool and its capture the flag pool: one
// row each, with its pool's kind and label, the relay's when it names them and
// else the pool id's, and a top level that is the coldkey's accounts together,
// so one action claims both.
func TestSnClaimStatusDrawsEachPoolOfAColdkey(t *testing.T) {
	const connected, other = "connected", "other"
	result := snClaimStatusBuild(testClaimRelayAnswer("running",
		&SnClaimRelayAccount{ColdkeySs58: other, NoId: 1, ClaimableRao: 100_000_000_000, Reason: SnClaimReasonQueued},
		&SnClaimRelayAccount{ColdkeySs58: connected, NoId: 1, ClaimableRao: 500_000_000, ValueTaoRao: 2_000_000, PendingRao: 7, Reason: SnClaimReasonQueued, ClaimNowReason: SnClaimNowReasonRequested},
		// a relay that names the kind and label
		&SnClaimRelayAccount{ColdkeySs58: connected, NoId: testCtfNoId, Kind: SnLedgerPoolKindCtf, Label: "Capture the flag", ClaimableRao: 48_025_660, ValueTaoRao: 200_000, PendingRao: 3, Reason: SnClaimReasonUnderThreshold, ClaimNowAvailable: true, ClaimNowNextAtMillis: 1_800_000_000_000},
	), connected, true, false)
	accounts := result.Accounts.getAll()
	if len(accounts) != 3 {
		t.Fatalf("accounts %+v", accounts)
	}
	if accounts[0].Kind != SnLedgerPoolKindPool || accounts[0].Label != "Pool 1" || accounts[1].Kind != SnLedgerPoolKindPool || accounts[1].Label != "Pool 1" ||
		accounts[2].Kind != SnLedgerPoolKindCtf || accounts[2].Label != "Capture the flag" {
		t.Fatalf("kinds and labels %+v %+v %+v", accounts[0], accounts[1], accounts[2])
	}
	// each row keeps its own action
	if accounts[1].Action != SnClaimActionWait || accounts[2].Action != SnClaimActionClaimNow {
		t.Fatalf("row actions %s %s", accounts[1].Action, accounts[2].Action)
	}
	// the top level: the connected coldkey's two accounts together, its reason the
	// lead's (the most to claim), and Claim now open since a tap claims both
	if result.ColdkeySs58 != connected || result.ClaimableRao != 548_025_660 || result.ValueTaoRao != 2_200_000 || result.PendingRao != 10 {
		t.Fatalf("sums %+v", result)
	}
	if result.Reason != SnClaimReasonQueued || !result.ClaimNowAvailable || result.ClaimNowReason != "" || result.ClaimNowNextAtMillis != 1_800_000_000_000 || result.Action != SnClaimActionClaimNow {
		t.Fatalf("top level %+v", result)
	}
	if !result.SelfClaimAvailable || result.Partial || result.UnavailablePools.Len() != 0 {
		t.Fatalf("self claim and partial %+v", result)
	}

	// a relay that names no kind: the capture the flag pool's id is ctf
	result = snClaimStatusBuild(testClaimRelayAnswer("running",
		&SnClaimRelayAccount{ColdkeySs58: connected, NoId: testCtfNoId, ClaimableRao: 5, Reason: SnClaimReasonUnderThreshold},
		// a kind the sdk does not know is a pool, whatever its id
		&SnClaimRelayAccount{ColdkeySs58: connected, NoId: testCtfNoId + 1, Kind: "head", Label: " Head ", Reason: SnClaimReasonNothingPending},
	), connected, true, false)
	accounts = result.Accounts.getAll()
	if accounts[0].Kind != SnLedgerPoolKindCtf || accounts[0].Label != "Capture the flag" || accounts[1].Kind != SnLedgerPoolKindPool || accounts[1].Label != "Head" {
		t.Fatalf("derived %+v %+v", accounts[0], accounts[1])
	}
	// the lead is the one with something to claim: its reason is the top level's
	if result.Reason != SnClaimReasonUnderThreshold || result.ClaimableRao != 5 || result.ClaimNowAvailable || result.Action != SnClaimActionWait {
		t.Fatalf("lead %+v", result)
	}

	// neither can be tapped: the lead's reason why not
	result = snClaimStatusBuild(testClaimRelayAnswer("running",
		&SnClaimRelayAccount{ColdkeySs58: connected, NoId: 1, ClaimableRao: 9, Reason: SnClaimReasonUnderThreshold, ClaimNowReason: SnClaimNowReasonLimit, ClaimNowNextAtMillis: 5},
		&SnClaimRelayAccount{ColdkeySs58: connected, NoId: testCtfNoId, ClaimableRao: 1, Reason: SnClaimReasonUnderThreshold, ClaimNowReason: SnClaimNowReasonUnderFloor},
	), connected, true, false)
	if result.ClaimNowAvailable || result.ClaimNowReason != SnClaimNowReasonLimit || result.ClaimNowNextAtMillis != 5 || result.ClaimableRao != 10 {
		t.Fatalf("not tappable %+v", result)
	}

	// with the relay stopped the device's own claim claims both: it is offered
	// when either clears the floor
	result = snClaimStatusBuild(testClaimRelayAnswer("griefing_stop",
		&SnClaimRelayAccount{ColdkeySs58: connected, NoId: 1, ClaimableRao: 23_999_999, Reason: SnClaimReasonRelayPaused},
		&SnClaimRelayAccount{ColdkeySs58: connected, NoId: testCtfNoId, ClaimableRao: 24_000_000, Reason: SnClaimReasonRelayPaused},
	), connected, true, true)
	if !result.SelfClaimAvailable || !result.SelfClaimNeedsGas || result.Action != SnClaimActionTopUpGas {
		t.Fatalf("self claim %+v", result)
	}
}

// URnetwork's batch claimed both of a coldkey's pools in one transaction: the
// claimed state names it with what it paid both, and only while neither pool has
// something to claim.
func TestSnClaimStatusShowsABatchClaimed(t *testing.T) {
	const otherTx = "0x1111111111111111111111111111111111111111111111111111111111111111"
	build := func(accounts ...*SnClaimRelayAccount) *SnClaimStatusResult {
		return snClaimStatusBuild(testClaimRelayAnswer("running", accounts...), "", true, false)
	}
	result := build(
		&SnClaimRelayAccount{ColdkeySs58: "a", NoId: 1, Reason: SnClaimReasonNothingPending, ClaimTxHash: testClaimTxHash, ClaimAmountRao: 842_000_000},
		&SnClaimRelayAccount{ColdkeySs58: "a", NoId: testCtfNoId, Reason: SnClaimReasonNothingPending, ClaimTxHash: testClaimTxHash, ClaimAmountRao: 48_025_660},
	)
	if !result.ShowsClaimed || !result.ShowsTile || result.ClaimTxHash != testClaimTxHash || result.ClaimAmountRao != 890_025_660 {
		t.Fatalf("batch claimed %+v", result)
	}
	// claimed apart: the lead's transaction and what it paid
	result = build(
		&SnClaimRelayAccount{ColdkeySs58: "a", NoId: 1, Reason: SnClaimReasonNothingPending, ClaimTxHash: testClaimTxHash, ClaimAmountRao: 842_000_000},
		&SnClaimRelayAccount{ColdkeySs58: "a", NoId: testCtfNoId, Reason: SnClaimReasonNothingPending, ClaimTxHash: otherTx, ClaimAmountRao: 48_025_660},
	)
	if !result.ShowsClaimed || result.ClaimTxHash != testClaimTxHash || result.ClaimAmountRao != 842_000_000 {
		t.Fatalf("claimed apart %+v", result)
	}
	// only the capture the flag pool was claimed by URnetwork: its claim leads
	result = build(
		&SnClaimRelayAccount{ColdkeySs58: "a", NoId: 1, Reason: SnClaimReasonNothingPending},
		&SnClaimRelayAccount{ColdkeySs58: "a", NoId: testCtfNoId, Reason: SnClaimReasonNothingPending, ClaimTxHash: otherTx, ClaimAmountRao: 48_025_660},
	)
	if !result.ShowsClaimed || result.ClaimTxHash != otherTx || result.ClaimAmountRao != 48_025_660 {
		t.Fatalf("one pool claimed %+v", result)
	}
	// one pool has something to claim since: pending again, not claimed
	result = build(
		&SnClaimRelayAccount{ColdkeySs58: "a", NoId: 1, Reason: SnClaimReasonNothingPending, ClaimTxHash: testClaimTxHash, ClaimAmountRao: 842_000_000},
		&SnClaimRelayAccount{ColdkeySs58: "a", NoId: testCtfNoId, ClaimableRao: 5, Reason: SnClaimReasonUnderThreshold, ClaimTxHash: testClaimTxHash, ClaimAmountRao: 48_025_660},
	)
	if result.ShowsClaimed || result.ClaimableRao != 5 || result.Reason != SnClaimReasonUnderThreshold || !result.ShowsTile {
		t.Fatalf("pending again %+v", result)
	}
}

// A pool whose accounts could not be read is unavailable, never zero, and never
// hides the others: the relay's unavailable pools reach the status, the sums
// leave them out and say so, and no claimed state is drawn while one is unknown.
func TestSnClaimStatusPartial(t *testing.T) {
	answer := testClaimRelayAnswer("running",
		&SnClaimRelayAccount{ColdkeySs58: "a", NoId: 1, Reason: SnClaimReasonNothingPending, ClaimTxHash: testClaimTxHash, ClaimAmountRao: 9},
	)
	answer.UnavailablePools = NewSnLedgerUnavailablePoolList()
	answer.UnavailablePools.Add(&SnLedgerUnavailablePool{Vault: testLedgerVault, NoId: testCtfNoId})
	result := snClaimStatusBuild(answer, "a", true, false)
	if !result.Partial || result.UnavailablePools.Len() != 1 || result.Accounts.Len() != 1 {
		t.Fatalf("partial %+v", result)
	}
	if pool := result.UnavailablePools.Get(0); pool.Kind != SnLedgerPoolKindCtf || pool.Label != "Capture the flag" || pool.NoId != testCtfNoId {
		t.Fatalf("unavailable pool %+v", pool)
	}
	if result.ShowsClaimed || !result.ShowsTile || result.ClaimTxHash != testClaimTxHash {
		t.Fatalf("partial claimed state %+v", result)
	}
	// nothing read but an unavailable pool: the tile shows it as unavailable
	answer = testClaimRelayAnswer("running")
	answer.UnavailablePools = NewSnLedgerUnavailablePoolList()
	answer.UnavailablePools.Add(&SnLedgerUnavailablePool{Vault: testLedgerVault, NoId: testCtfNoId, Kind: SnLedgerPoolKindCtf, Label: "Capture the flag"})
	if result = snClaimStatusBuild(answer, "", true, false); !result.Partial || !result.ShowsTile || result.Action != SnClaimActionNone {
		t.Fatalf("only unavailable %+v", result)
	}
	// a whole answer
	if result = snClaimStatusBuild(testClaimRelayAnswer("running"), "", true, false); result.Partial || result.UnavailablePools == nil || result.UnavailablePools.Len() != 0 {
		t.Fatalf("whole %+v", result)
	}

	// the ledger standing in for the relay: a partial read carries its unavailable
	// pools into the status
	ledgerAccounts := NewSnLedgerAccountList()
	ledgerAccounts.Add(&SnLedgerAccount{NoId: 1, Kind: SnLedgerPoolKindPool, Label: "Pool 1", Index: 42, FinalizedRao: 900, ClaimableRao: 900})
	unavailable := NewSnLedgerUnavailablePoolList()
	unavailable.Add(&SnLedgerUnavailablePool{Vault: testLedgerVault, NoId: testCtfNoId, Kind: SnLedgerPoolKindCtf, Label: "Capture the flag"})
	ledger := &SnLedgerAccountsResult{Accounts: ledgerAccounts, UnavailablePools: unavailable, Partial: true}
	result = snClaimStatusOf(nil, errors.New("Failed to fetch"), "a", true, func() (*SnClaimRelayAccountList, *SnLedgerUnavailablePoolList, *SnError) {
		return snClaimLedgerRelayAccounts(ledger, "a")
	}, nil, nil)
	if result.Error != nil || !result.Partial || result.UnavailablePools.Len() != 1 || result.ClaimableRao != 900 || result.Action != SnClaimActionSelfClaim {
		t.Fatalf("partial ledger %+v (%+v)", result, result.Error)
	}
	if account := result.Accounts.Get(0); account.Kind != SnLedgerPoolKindPool || account.Label != "Pool 1" {
		t.Fatalf("ledger row %+v", account)
	}
	// the json the apps read
	raw := testClaimRelayJson(t, result)
	for _, part := range []string{`"partial":true`, `"unavailable_pools":[{"vault":"` + testLedgerVault + `","no_id":4294967297,"kind":"ctf","label":"Capture the flag"}]`, `"kind":"pool","label":"Pool 1"`} {
		if !strings.Contains(raw, part) {
			t.Errorf("%s lacks %s", raw, part)
		}
	}
}

// The relay's json, as the server sends it, reads its kinds, labels and
// unavailable pools, and a relay that sends none reads as today.
func TestSnClaimRelayResultReadsPools(t *testing.T) {
	var answer SnClaimRelayResult
	if err := json.Unmarshal([]byte(`{"state":"running","accounts":[
		{"coldkey_ss58":"a","no_id":1,"kind":"pool","label":"Pool 1","index":7,"claimable_rao":5,"reason":"queued"},
		{"coldkey_ss58":"a","no_id":4294967297,"kind":"ctf","label":"Capture the flag","index":3,"claimable_rao":2,"reason":"queued"}],
		"unavailable_pools":[]}`), &answer); err != nil {
		t.Fatal(err)
	}
	result := snClaimStatusBuild(&answer, "a", true, false)
	if result.Accounts.Len() != 2 || result.Accounts.Get(1).Kind != SnLedgerPoolKindCtf || result.Accounts.Get(1).Index != 3 || result.Partial || result.ClaimableRao != 7 {
		t.Fatalf("read %+v", result)
	}
	answer = SnClaimRelayResult{}
	if err := json.Unmarshal([]byte(`{"state":"running","accounts":[{"coldkey_ss58":"a","no_id":1,"index":7,"claimable_rao":5,"reason":"queued"}]}`), &answer); err != nil {
		t.Fatal(err)
	}
	result = snClaimStatusBuild(&answer, "a", true, false)
	if account := result.Accounts.Get(0); account.Kind != SnLedgerPoolKindPool || account.Label != "Pool 1" || result.Partial {
		t.Fatalf("an old relay %+v %+v", result, account)
	}
}

// A device asks the relay with partial=true, so a pool the relay cannot read is
// named while the others are answered: each pool of the coldkey is a row with
// its kind and label, and the unavailable pool is drawn as unavailable.
func TestSnClaimStatusAsksTheRelayForEachPool(t *testing.T) {
	_, aliceSs58 := testAliceSs58(t)
	service := &testClaimRelayService{relayStatus: http.StatusOK, ledgerStatus: http.StatusInternalServerError, ledgerBody: "ledger down",
		relayBody: fmt.Sprintf(`{"state":"running","transfer_floor_rao":24000000,"accounts":[
			{"coldkey_ss58":%q,"no_id":1,"kind":"pool","label":"Pool 1","index":7,"claimable_rao":1000000000,"reason":"queued"}],
			"unavailable_pools":[{"vault":"0x2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b","no_id":4294967297,"kind":"ctf","label":"Capture the flag"}]}`, aliceSs58)}
	chain := &fakeChain{t: t, block: 1000, claimed: map[string]bool{}, receipts: map[string]bool{}}
	device := testClaimRelayDevice(t, service, chain, "")
	result := device.snClaimStatus(context.Background())
	if len(service.relayQueries) != 1 || service.relayQueries[0] != "partial=true" {
		t.Fatalf("relay queries %q", service.relayQueries)
	}
	if result.Error != nil || !result.Partial || result.UnavailablePools.Len() != 1 || result.UnavailablePools.Get(0).Kind != SnLedgerPoolKindCtf ||
		result.ClaimableRao != 1_000_000_000 || result.Accounts.Len() != 1 || result.Accounts.Get(0).Label != "Pool 1" {
		t.Fatalf("result %+v (%+v)", result, result.Error)
	}
}

// A relay answer with a null account is read past it, on a device whose own
// claim could pay, as on any other.
func TestSnClaimStatusOfSkipsANullAccount(t *testing.T) {
	relay := testClaimRelayAnswer("griefing_stop", nil, &SnClaimRelayAccount{ColdkeySs58: "a", ClaimableRao: 100_000_000, Reason: SnClaimReasonRelayPaused})
	gasReads := 0
	result := snClaimStatusOf(relay, nil, "a", true, nil, func() bool {
		gasReads++
		return false
	}, nil)
	if gasReads != 1 || result.Accounts.Len() != 1 || result.Action != SnClaimActionSelfClaim || result.ClaimableRao != 100_000_000 {
		t.Fatalf("gas reads %d, result %+v", gasReads, result)
	}
}
