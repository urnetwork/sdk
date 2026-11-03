//go:build js

// The claim relay for the site, which has no device: the sdk's SnClaimStatus and
// SnClaimNow over what the page read, through URnetworkSnClaimStatusOf and
// URnetworkSnClaimNow. ur.io only draws the answers (apple BTEASY.md: the sdk
// decides, the app draws). Inputs are built as a page passes them: the server's
// json, the wasm's ledger read in the page's camelCase, and plain js functions.
package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"syscall/js"
	"testing"

	"github.com/urnetwork/sdk"
)

// a coldkey of the sdk's test vectors
const testClaimColdkey = "5F3sa2TJAWMqDhXG6jhV4N8ko9SxwGy8TpaNS1repo5EYjQX"

const testClaimHash = "0x8a5f0c9e1d2b3a4f5e6d7c8b9a0f1e2d3c4b5a69788796a5b4c3d2e1f0a9b8c7"

func jsonValue(text string) js.Value {
	return js.Global().Get("JSON").Call("parse", text)
}

func jsonOf(t *testing.T, value js.Value) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(js.Global().Get("JSON").Call("stringify", value).String()), &out); err != nil {
		t.Fatalf("%v: %v", value, err)
	}
	return out
}

func snExport(t *testing.T, name string) js.Value {
	t.Helper()
	registerSnExports()
	fn := js.Global().Get(name)
	if fn.Type() != js.TypeFunction {
		t.Fatalf("%s is not exported", name)
	}
	return fn
}

// the status for one page input, as json
func claimStatusOf(t *testing.T, input string) map[string]any {
	t.Helper()
	return jsonOf(t, snExport(t, "URnetworkSnClaimStatusOf").Invoke(jsonValue(input)))
}

// GET /sn/claim-relay as the server sends it: the policy (0.0125 TAO, about 3
// SN25α at its price; a floor of 0.0001 TAO; 30 days) and one account
func testRelayAnswer(account string) string {
	return `{"state":"running","automatic_threshold_tao_rao":12500000,"transfer_floor_tao_rao":100000,` +
		`"automatic_threshold_rao":3000000000,"transfer_floor_rao":24000000,"claim_now_period_days":30,"accounts":[` + account + `]}`
}

const testUnderThreshold = `{"coldkey_ss58":"` + testClaimColdkey + `","no_id":1,"index":42,"claimable_rao":842000000,` +
	`"value_tao_rao":3556608,"pending_rao":0,"reason":"under_threshold","claim_now_available":true}`

// the wallet's v2 accounts as ur.io's fetchSnLedgerAccounts hands them on:
// camelCase, beside its own fields
const testLedgerRead = `{"configured":true,"canClaim":true,"settings":{"chainId":964},"error":null,"accounts":[` +
	`{"vault":"0x2222222222222222222222222222222222abcdef","noId":1,"index":42,"finalizedRao":2100000000,` +
	`"claimableRao":842000000,"paidRao":1258000000,"pending":true,"pendingRao":2200000000},` +
	`{"vault":"0x2222222222222222222222222222222222abcdef","noId":2,"index":7,"finalizedRao":5,` +
	`"claimableRao":0,"paidRao":5,"pending":false,"pendingRao":0}]}`

func wantFields(t *testing.T, name string, got map[string]any, want map[string]any) {
	t.Helper()
	for key, value := range want {
		if !reflect.DeepEqual(got[key], value) {
			t.Errorf("%s: %s = %#v, want %#v (status %v)", name, key, got[key], value, got)
		}
	}
}

func accountsOf(t *testing.T, status map[string]any) []map[string]any {
	t.Helper()
	list, ok := status["accounts"].([]any)
	if !ok {
		t.Fatalf("accounts %#v", status["accounts"])
	}
	out := []map[string]any{}
	for _, item := range list {
		out = append(out, item.(map[string]any))
	}
	return out
}

// The mocks' 4a: the relay claims it, under the threshold, and Claim now is
// tappable. The connected wallet's account leads, the device's own claim is the
// fallback, and the policy's figures come through for the screen's words.
func TestSnClaimStatusOfDrawsTheRelaysAnswer(t *testing.T) {
	status := claimStatusOf(t, `{"relay":`+testRelayAnswer(testUnderThreshold)+`,"connectedSs58":"`+testClaimColdkey+`","selfClaimConfigured":true}`)
	wantFields(t, "4a", status, map[string]any{
		"relay_state":                 "running",
		"coldkey_ss58":                testClaimColdkey,
		"action":                      sdk.SnClaimActionClaimNow,
		"reason":                      sdk.SnClaimReasonUnderThreshold,
		"claimable_rao":               float64(842_000_000),
		"value_tao_rao":               float64(3_556_608),
		"pending_rao":                 float64(0),
		"claim_now_available":         true,
		"self_claim_available":        true,
		"self_claim_needs_gas":        false,
		"automatic_threshold_rao":     float64(3_000_000_000),
		"transfer_floor_rao":          float64(24_000_000),
		"automatic_threshold_tao_rao": float64(12_500_000),
		"transfer_floor_tao_rao":      float64(100_000),
		"claim_now_period_days":       float64(30),
		"shows_tile":                  true,
		"shows_claimed":               false,
		"error":                       nil,
	})
	accounts := accountsOf(t, status)
	if len(accounts) != 1 || accounts[0]["connected"] != true || accounts[0]["action"] != sdk.SnClaimActionClaimNow {
		t.Fatalf("accounts %v", accounts)
	}

	// Go's field names and camelCase say the same
	pascal := `{"Relay":{"State":"running","AutomaticThresholdTaoRao":12500000,"TransferFloorTaoRao":100000,"AutomaticThresholdRao":3000000000,` +
		`"TransferFloorRao":24000000,"ClaimNowPeriodDays":30,"Accounts":[{"ColdkeySs58":"` + testClaimColdkey + `","NoId":1,"Index":42,` +
		`"ClaimableRao":842000000,"ValueTaoRao":3556608,"PendingRao":0,"Reason":"under_threshold","ClaimNowAvailable":true}]},` +
		`"ConnectedSs58":"` + testClaimColdkey + `","SelfClaimConfigured":true}`
	if again := claimStatusOf(t, pascal); !reflect.DeepEqual(again, status) {
		t.Fatalf("Go names:\n%v\nwant\n%v", again, status)
	}

	// the relay down (4e): the device's own claim, the site's, is the way for now
	notRunning := `{"coldkey_ss58":"` + testClaimColdkey + `","no_id":1,"index":42,"claimable_rao":842000000,"reason":"relay_not_running",` +
		`"claim_now_available":false,"claim_now_reason":"relay_not_running"}`
	status = claimStatusOf(t, `{"relay":`+testRelayAnswer(notRunning)+`,"connectedSs58":"`+testClaimColdkey+`","selfClaimConfigured":true}`)
	wantFields(t, "4e", status, map[string]any{"action": sdk.SnClaimActionSelfClaim, "self_claim_available": true, "claim_now_reason": sdk.SnClaimReasonRelayNotRunning, "shows_tile": true})
	// a page that cannot send its own claim only waits
	status = claimStatusOf(t, `{"relay":`+testRelayAnswer(notRunning)+`,"connectedSs58":"`+testClaimColdkey+`","selfClaimConfigured":false}`)
	wantFields(t, "4e, not configured", status, map[string]any{"action": sdk.SnClaimActionWait, "self_claim_available": false})
	// and so does a network with no wallet here
	status = claimStatusOf(t, `{"relay":`+testRelayAnswer(notRunning)+`,"connectedSs58":"","selfClaimConfigured":true}`)
	wantFields(t, "4e, no wallet", status, map[string]any{"action": sdk.SnClaimActionWait, "self_claim_available": false, "coldkey_ss58": testClaimColdkey, "shows_tile": true})

	// what the sdk sets is its own: a page's connected, action or link is redrawn
	injected := `{"coldkey_ss58":"5Other","no_id":1,"index":3,"claimable_rao":5,"reason":"queued","connected":true,"action":"self_claim","claim_tx_url":"javascript:alert(1)"}`
	status = claimStatusOf(t, `{"relay":`+testRelayAnswer(injected)+`,"connectedSs58":"`+testClaimColdkey+`","selfClaimConfigured":true}`)
	accounts = accountsOf(t, status)
	if accounts[0]["connected"] == true || accounts[0]["action"] != sdk.SnClaimActionWait || accounts[0]["claim_tx_url"] != nil {
		t.Fatalf("injected %v", accounts[0])
	}
}

// A relay that cannot answer leaves the site's own claim working, with the
// connected wallet's balances from the public ledger; neither readable is
// unavailable, never zero.
func TestSnClaimStatusOfFallsBackToTheLedger(t *testing.T) {
	connected := `,"connectedSs58":"` + testClaimColdkey + `","selfClaimConfigured":true`
	status := claimStatusOf(t, `{"relay":null,"relayError":"Failed to fetch","ledger":`+testLedgerRead+connected+`}`)
	wantFields(t, "no answer", status, map[string]any{
		"relay_state":           sdk.SnClaimRelayStateUnavailable,
		"error":                 nil,
		"coldkey_ss58":          testClaimColdkey,
		"action":                sdk.SnClaimActionSelfClaim,
		"reason":                sdk.SnClaimReasonRelayNotRunning,
		"claimable_rao":         float64(842_000_000),
		"pending_rao":           float64(100_000_000),
		"claim_now_available":   false,
		"claim_now_reason":      sdk.SnClaimReasonRelayNotRunning,
		"claim_now_period_days": float64(0),
		"shows_tile":            true,
	})
	accounts := accountsOf(t, status)
	if len(accounts) != 2 || accounts[1]["reason"] != sdk.SnClaimReasonNothingPending || accounts[1]["action"] != sdk.SnClaimActionNone || accounts[1]["coldkey_ss58"] != testClaimColdkey {
		t.Fatalf("accounts %v", accounts)
	}

	// the server is up and cannot read the balances: its state reaches the
	// page, and the ledger's balances stand in
	relayError := `{"state":"cap_reached","accounts":[],"error":{"message":"The balances could not be read."}}`
	status = claimStatusOf(t, `{"relay":`+relayError+`,"ledger":`+testLedgerRead+connected+`}`)
	wantFields(t, "relay error", status, map[string]any{"relay_state": "cap_reached", "error": nil, "action": sdk.SnClaimActionSelfClaim})

	// an answer the sdk cannot read is no answer
	status = claimStatusOf(t, `{"relay":{"state":"running","accounts":"none"},"ledger":`+testLedgerRead+connected+`}`)
	wantFields(t, "unreadable answer", status, map[string]any{"relay_state": sdk.SnClaimRelayStateUnavailable, "error": nil, "reason": sdk.SnClaimReasonRelayNotRunning})

	// neither: unavailable, with the ledger's own code, never zero
	ledgerDown := `{"configured":true,"canClaim":true,"accounts":null,"error":{"code":"chain_rpc_unreachable","message":"rpc down"}}`
	status = claimStatusOf(t, `{"relay":`+relayError+`,"ledger":`+ledgerDown+connected+`}`)
	wantFields(t, "neither", status, map[string]any{
		"relay_state": "cap_reached", "action": sdk.SnClaimActionNone, "claimable_rao": float64(0), "shows_tile": false, "shows_claimed": false,
		"error": map[string]any{"code": sdk.SnErrorCodeChainRpcUnreachable, "message": "rpc down"},
	})
	if accounts := accountsOf(t, status); len(accounts) != 0 {
		t.Fatalf("accounts %v", accounts)
	}
	// a ledger not read, or read without a code, is the ledger unavailable
	for _, ledger := range []string{`null`, `{"accounts":null,"error":{"code":"","message":""}}`} {
		status = claimStatusOf(t, `{"relay":null,"ledger":`+ledger+connected+`}`)
		wantFields(t, "ledger "+ledger, status, map[string]any{
			"relay_state": sdk.SnClaimRelayStateUnavailable, "action": sdk.SnClaimActionNone, "shows_tile": false,
			"error": map[string]any{"code": sdk.SnErrorCodeLedgerUnavailable, "message": "the ledger could not be read"},
		})
	}

	// without a wallet there is no ledger to read: the relay's own words
	status = claimStatusOf(t, `{"relay":null,"relayError":"Failed to fetch","ledger":`+testLedgerRead+`}`)
	wantFields(t, "no wallet", status, map[string]any{
		"relay_state": sdk.SnClaimRelayStateUnavailable, "action": sdk.SnClaimActionNone, "shows_tile": false,
		"error": map[string]any{"code": sdk.SnErrorCodeLedgerUnavailable, "message": "Failed to fetch"},
	})
	status = claimStatusOf(t, `{"relay":`+relayError+`}`)
	wantFields(t, "no wallet, relay error", status, map[string]any{
		"relay_state": "cap_reached",
		"error":       map[string]any{"code": sdk.SnErrorCodeLedgerUnavailable, "message": "The balances could not be read."},
	})
	// nothing at all
	wantFields(t, "nothing", jsonOf(t, snExport(t, "URnetworkSnClaimStatusOf").Invoke()), map[string]any{
		"relay_state": sdk.SnClaimRelayStateUnavailable, "action": sdk.SnClaimActionNone,
		"error": map[string]any{"code": sdk.SnErrorCodeLedgerUnavailable, "message": "the claim relay did not answer"},
	})
}

// The mocks' 4d: URnetwork claimed it all, drawn with its transaction, linked to
// the explorer of the page's chain settings; without settings, named but not
// linked.
func TestSnClaimStatusOfLinksTheClaimedTransaction(t *testing.T) {
	claimed := `{"coldkey_ss58":"` + testClaimColdkey + `","no_id":1,"index":42,"claimable_rao":0,"reason":"nothing_pending","claim_now_available":false,` +
		`"claim_now_reason":"nothing_pending","claim_now_next_at_millis":1800000000000,"claim_tx_hash":"` + testClaimHash + `","claim_amount_rao":842000000}`
	settings := `,"settings":{"rpcUrls":["https://rpc.invalid"],"chainId":964,"explorerTxUrl":"https://explorer.invalid/tx/%s","raw":{}}`
	status := claimStatusOf(t, `{"relay":`+testRelayAnswer(claimed)+`,"connectedSs58":""`+settings+`}`)
	link := "https://explorer.invalid/tx/" + testClaimHash
	wantFields(t, "claimed", status, map[string]any{
		"shows_claimed": true, "shows_tile": true, "claim_tx_hash": testClaimHash, "claim_tx_url": link,
		"claim_amount_rao": float64(842_000_000), "claim_now_next_at_millis": float64(1_800_000_000_000), "action": sdk.SnClaimActionNone,
	})
	if account := accountsOf(t, status)[0]; account["claim_tx_url"] != link {
		t.Fatalf("account %v", account)
	}
	status = claimStatusOf(t, `{"relay":`+testRelayAnswer(claimed)+`}`)
	wantFields(t, "no settings", status, map[string]any{"shows_claimed": true, "claim_tx_hash": testClaimHash, "claim_tx_url": nil})

	// what is not a transaction is none: no claimed state, no tile
	forged := strings.ReplaceAll(claimed, testClaimHash, "javascript:alert(1)")
	status = claimStatusOf(t, `{"relay":`+testRelayAnswer(forged)+settings+`}`)
	wantFields(t, "forged", status, map[string]any{"shows_claimed": false, "shows_tile": false, "claim_tx_hash": nil, "claim_tx_url": nil, "claim_amount_rao": nil})
}

// claimNow taps Claim now as a page does, with post answering for the server,
// and answers the result with the calls post saw
func claimNow(t *testing.T, coldkey string, connected string, post func(coldkey string) js.Value) (map[string]any, []string) {
	t.Helper()
	calls := []string{}
	postFn := js.FuncOf(func(this js.Value, args []js.Value) any {
		coldkey := ""
		if 0 < len(args) {
			coldkey = args[0].String()
		}
		calls = append(calls, coldkey)
		return post(coldkey)
	})
	defer postFn.Release()
	value, rejected := awaitApiPromise(t, snExport(t, "URnetworkSnClaimNow").Invoke(coldkey, connected, postFn))
	if !rejected.IsUndefined() {
		t.Fatalf("rejected: %v", rejected)
	}
	return jsonOf(t, value), calls
}

func resolved(answer string) func(string) js.Value {
	return func(string) js.Value {
		return js.Global().Get("Promise").Call("resolve", jsonValue(answer))
	}
}

// A tap names the connected wallet unless it names another; the server's answer,
// its limit and its failures reach the page in the sdk's codes. Nothing is asked
// of the server for an address that is not one.
func TestSnClaimNowAsksTheRelay(t *testing.T) {
	if !sdk.ValidateSs58(testClaimColdkey) {
		t.Fatal("CONTROL FAILED: the test coldkey is not an ss58 address")
	}
	result, calls := claimNow(t, "", testClaimColdkey, resolved(`{"requested":true,"next_at_millis":1700000000000}`))
	wantFields(t, "taken", result, map[string]any{"requested": true, "next_at_millis": float64(1_700_000_000_000), "error": nil})
	if len(calls) != 1 || calls[0] != testClaimColdkey {
		t.Fatalf("calls %q", calls)
	}

	result, calls = claimNow(t, " "+testClaimColdkey+" ", "", resolved(`{"requested":false,"reason":"limit","next_at_millis":1800000000000}`))
	wantFields(t, "limit", result, map[string]any{"requested": false, "reason": sdk.SnClaimNowReasonLimit, "next_at_millis": float64(1_800_000_000_000)})
	if len(calls) != 1 || calls[0] != testClaimColdkey {
		t.Fatalf("calls %q", calls)
	}

	result, _ = claimNow(t, testClaimColdkey, "", resolved(`{"requested":false,"error":{"message":"The balances could not be read."}}`))
	wantFields(t, "unavailable", result, map[string]any{"requested": false, "error": map[string]any{"code": sdk.SnErrorCodeLedgerUnavailable, "message": "The balances could not be read."}})

	// the page's client failed: the server's words, as a server error
	result, _ = claimNow(t, testClaimColdkey, "", func(string) js.Value {
		return js.Global().Get("Promise").Call("reject", js.Global().Get("Error").New("Service unavailable"))
	})
	wantFields(t, "rejected", result, map[string]any{"requested": false, "error": map[string]any{"code": sdk.SnErrorCodeServer, "message": "Service unavailable"}})
	// and so does a client that throws, or answers nothing the sdk can read
	result, _ = claimNow(t, testClaimColdkey, "", func(string) js.Value {
		return js.Global().Get("Promise").Call("resolve", js.Null())
	})
	if errorOf, ok := result["error"].(map[string]any); !ok || errorOf["code"] != sdk.SnErrorCodeServer || result["requested"] != false {
		t.Fatalf("no answer %v", result)
	}
	throws := js.Global().Get("Function").New("coldkey", "throw new Error('client down')")
	value, rejected := awaitApiPromise(t, snExport(t, "URnetworkSnClaimNow").Invoke(testClaimColdkey, "", throws))
	if !rejected.IsUndefined() {
		t.Fatalf("throws: rejected %v", rejected)
	}
	wantFields(t, "throws", jsonOf(t, value), map[string]any{"requested": false, "error": map[string]any{"code": sdk.SnErrorCodeServer, "message": "client down"}})

	// nothing asked for an address that is not one, or with no wallet to name
	result, calls = claimNow(t, "not an address", testClaimColdkey, resolved(`{"requested":true}`))
	if errorOf, ok := result["error"].(map[string]any); !ok || errorOf["code"] != sdk.SnErrorCodeInvalidAddress || len(calls) != 0 {
		t.Fatalf("not an address: %v, calls %q", result, calls)
	}
	result, calls = claimNow(t, "", " ", resolved(`{"requested":true}`))
	wantFields(t, "no wallet", result, map[string]any{"requested": false, "error": map[string]any{"code": sdk.SnErrorCodeConnectWalletFirst, "message": "connect a Bittensor wallet first"}})
	if len(calls) != 0 {
		t.Fatalf("no wallet: calls %q", calls)
	}
}
