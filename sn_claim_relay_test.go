package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk/sn/evm"
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
	ledgerStatus int
	ledgerBody   string
}

func (self *testClaimRelayService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	switch r.URL.Path {
	case "/sn/claim-relay":
		self.authorized = append(self.authorized, r.Header.Get("Authorization"))
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
		_, maxCost, err := snSignClaim(ctx, client, big.NewInt(945), txType, key, [20]byte{1}, []byte{1}, gas)
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
	for _, part := range []string{`"relay_state":"running"`, `"action":"claim_now"`, `"reason":"queued"`, `"claimable_rao":5`, `"claim_now_available":true`, `"self_claim_available":false`, `"accounts":[{`, `"connected":true`} {
		if !strings.Contains(raw, part) {
			t.Errorf("%s lacks %s", raw, part)
		}
	}
	empty := testClaimRelayJson(t, snClaimStatusBuild(nil, "", false, false))
	if !strings.Contains(empty, `"accounts":[]`) {
		t.Errorf("an empty list must marshal as []: %s", empty)
	}
}
