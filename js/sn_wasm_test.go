//go:build js

// The settlement schedule on the wasm claims result.
package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"syscall/js"
	"testing"

	"github.com/urnetwork/sdk"
)

// The claims result carries the epoch schedule to the site, and null while
// the coordinator's policy is unreadable.
func TestSnClaimsResultWasmSchedule(t *testing.T) {
	result := jsSnClaimsResult(&sdk.SnClaimsResult{
		Claims:       sdk.NewSnEpochClaimList(),
		CurrentEpoch: 9,
		BlockNumber:  1000,
		Schedule: &sdk.SnEpochSchedule{
			Epoch:                 9,
			EpochBlocks:           50_400,
			ClaimOpenOffsetBlocks: 14_400,
			ClaimTtlEpochs:        8,
			ClaimGraceEpochs:      1,
			EndBlock:              51_350,
			ClaimOpenBlock:        65_750,
			ExpiryBlock:           504_949,
			HeadBlock:             1000,
			HeadMillis:            1_791_244_800_000,
			EndMillis:             1_791_849_000_000,
			ClaimOpenMillis:       1_792_021_800_000,
			ExpiryMillis:          1_797_292_188_000,
		},
	})
	schedule := result.Get("Schedule")
	if schedule.IsNull() || schedule.IsUndefined() {
		t.Fatal("no schedule")
	}
	for name, want := range map[string]int64{
		"Epoch":                 9,
		"ClaimOpenOffsetBlocks": 14_400,
		"ClaimTtlEpochs":        8,
		"ClaimGraceEpochs":      1,
		"EndMillis":             1_791_849_000_000,
		"ClaimOpenMillis":       1_792_021_800_000,
		"ExpiryMillis":          1_797_292_188_000,
	} {
		if got := int64(schedule.Get(name).Float()); got != want {
			t.Fatalf("%s = %d, want %d", name, got, want)
		}
	}

	none := jsSnClaimsResult(&sdk.SnClaimsResult{Claims: sdk.NewSnEpochClaimList()})
	if !none.Get("Schedule").IsNull() {
		t.Fatalf("schedule without a policy: %v", none.Get("Schedule"))
	}
}

// CL-2: the site sees each v2 account's states, and an unavailable ledger as
// an error with no account.
func TestSnLedgerAccountsResultWasm(t *testing.T) {
	accounts := sdk.NewSnLedgerAccountList()
	accounts.Add(&sdk.SnLedgerAccount{
		Vault:        "0xabababababababababababababababababababab",
		NoId:         1,
		Index:        42,
		FinalizedRao: 123_456_789,
		ClaimableRao: 23_456_789,
		PaidRao:      100_000_000,
		Pending:      true,
		PendingRao:   130_000_000,
	})
	result := jsSnLedgerAccountsResult(&sdk.SnLedgerAccountsResult{Accounts: accounts, ColdkeySs58: "5F"})
	if result.Get("Accounts").Length() != 1 || result.Get("ColdkeySs58").String() != "5F" || !result.Get("Error").IsNull() {
		t.Fatalf("result %v", result)
	}
	account := result.Get("Accounts").Index(0)
	for name, want := range map[string]int64{
		"NoId":         1,
		"Index":        42,
		"FinalizedRao": 123_456_789,
		"ClaimableRao": 23_456_789,
		"PaidRao":      100_000_000,
		"PendingRao":   130_000_000,
	} {
		if got := int64(account.Get(name).Float()); got != want {
			t.Fatalf("%s = %d, want %d", name, got, want)
		}
	}
	if !account.Get("Pending").Bool() || account.Get("Vault").String() != "0xabababababababababababababababababababab" {
		t.Fatalf("account %v", account)
	}

	unavailable := jsSnLedgerAccountsResult(&sdk.SnLedgerAccountsResult{
		Accounts: sdk.NewSnLedgerAccountList(),
		Error:    &sdk.SnError{Code: sdk.SnErrorCodeLedgerUnavailable, Message: "http 500 fetching the ledger account"},
	})
	if unavailable.Get("Accounts").Length() != 0 || unavailable.Get("Error").Get("Code").String() != sdk.SnErrorCodeLedgerUnavailable {
		t.Fatalf("unavailable %v", unavailable)
	}
}

// APPS-SPEC "Vault2Address": the site's settings carry the pinned vault2 out
// of the defaults and back in, under any key casing, so a page can configure
// the v2 claim.
func TestSnChainSettingsWasmVault2Address(t *testing.T) {
	if got := jsSnChainSettings(sdk.DefaultSnChainSettings()).Get("Vault2Address").String(); got != "0xe4865eb1bebdf5c6a10c9d37f3ccdd59aee8f20c" {
		t.Fatalf("default vault2 %q", got)
	}
	vault2 := "0x2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b"
	for _, key := range []string{"Vault2Address", "vault2Address", "vault2_address"} {
		settings, err := parseSnChainSettings(js.ValueOf(map[string]any{key: vault2}))
		if err != nil || settings.Vault2Address != vault2 {
			t.Fatalf("%s: %+v, %v", key, settings, err)
		}
		if got := jsSnChainSettings(settings).Get("Vault2Address").String(); got != vault2 {
			t.Fatalf("%s round trip %q", key, got)
		}
	}
}

// VERSION2-LAUNCH.md §2.6, the sizing rule for v1 claims: the site sees each
// v1 claim transaction with the Go field names, ready for its wallet, with
// the gas limit to send it with.
func TestSnUnsignedTxsWasm(t *testing.T) {
	txs := sdk.NewSnUnsignedTxList()
	txs.Add(&sdk.SnUnsignedTx{Epoch: 7, ChainId: 964, To: "0x2222222222222222222222222222222222222222", Data: "0xce479a1b", Value: "0", AmountRao: 123_400_000, Gas: 2_700_000})
	out := jsSnUnsignedTxs(txs)
	if out.Length() != 1 {
		t.Fatalf("transactions %v", out)
	}
	tx := out.Index(0)
	for name, want := range map[string]int64{"Epoch": 7, "ChainId": 964, "AmountRao": 123_400_000, "Gas": 2_700_000} {
		if got := int64(tx.Get(name).Float()); got != want {
			t.Fatalf("%s = %d, want %d", name, got, want)
		}
	}
	for name, want := range map[string]string{"To": "0x2222222222222222222222222222222222222222", "Data": "0xce479a1b", "Value": "0"} {
		if got := tx.Get(name).String(); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	if empty := jsSnUnsignedTxs(sdk.NewSnUnsignedTxList()); empty.Length() != 0 {
		t.Fatalf("empty %v", empty)
	}
}

// CL-4, APPS-SPEC "SnLedgerClaimTransactionsFor": the site sees each v2 claim
// transaction with the Go field names, ready for its wallet, with the gas
// limit to send it with.
func TestSnLedgerUnsignedTxsWasm(t *testing.T) {
	txs := sdk.NewSnLedgerUnsignedTxList()
	txs.Add(&sdk.SnLedgerUnsignedTx{NoId: 1, Index: 42, RootIndex: 3, ChainId: 964, To: "0x2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b", Data: "0xb7a6b907", Value: "0", AmountRao: 23_456_789, Gas: 2_600_000})
	out := jsSnLedgerUnsignedTxs(txs)
	if out.Length() != 1 {
		t.Fatalf("transactions %v", out)
	}
	tx := out.Index(0)
	for name, want := range map[string]int64{"NoId": 1, "Index": 42, "RootIndex": 3, "ChainId": 964, "AmountRao": 23_456_789, "Gas": 2_600_000} {
		if got := int64(tx.Get(name).Float()); got != want {
			t.Fatalf("%s = %d, want %d", name, got, want)
		}
	}
	for name, want := range map[string]string{"To": "0x2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b", "Data": "0xb7a6b907", "Value": "0"} {
		if got := tx.Get(name).String(); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	if empty := jsSnLedgerUnsignedTxs(sdk.NewSnLedgerUnsignedTxList()); empty.Length() != 0 {
		t.Fatalf("empty %v", empty)
	}
}

const testCtfVault2 = "0x2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b"

const testCtfBatcher = "0x14174e7c5671ce1499aef9c68d74bebffa58c41a"

// Capture the flag: the site sees each v2 account's pool kind and label, and
// each pool whose ledger could not be read, which it shows as unavailable
// beside the others.
func TestSnLedgerAccountsResultWasmPools(t *testing.T) {
	accounts := sdk.NewSnLedgerAccountList()
	accounts.Add(&sdk.SnLedgerAccount{Vault: testCtfVault2, NoId: 1, Kind: sdk.SnLedgerPoolKindPool, Label: "Pool 1", Index: 6, FinalizedRao: 9, ClaimableRao: 9})
	unavailable := sdk.NewSnLedgerUnavailablePoolList()
	unavailable.Add(&sdk.SnLedgerUnavailablePool{Vault: testCtfVault2, NoId: 4294967297, Kind: sdk.SnLedgerPoolKindCtf, Label: "Capture the flag"})
	result := jsSnLedgerAccountsResult(&sdk.SnLedgerAccountsResult{Accounts: accounts, UnavailablePools: unavailable, Partial: true})
	account := result.Get("Accounts").Index(0)
	if account.Get("Kind").String() != "pool" || account.Get("Label").String() != "Pool 1" || !result.Get("Partial").Bool() || result.Get("UnavailablePools").Length() != 1 {
		t.Fatalf("result %v", jsonOf(t, result))
	}
	pool := result.Get("UnavailablePools").Index(0)
	if pool.Get("NoId").Float() != 4294967297 || pool.Get("Kind").String() != "ctf" || pool.Get("Label").String() != "Capture the flag" || pool.Get("Vault").String() != testCtfVault2 {
		t.Fatalf("unavailable %v", jsonOf(t, pool))
	}
	whole := jsSnLedgerAccountsResult(&sdk.SnLedgerAccountsResult{Accounts: accounts})
	if whole.Get("Partial").Bool() || whole.Get("UnavailablePools").Length() != 0 {
		t.Fatalf("whole %v", jsonOf(t, whole))
	}
}

// The claim batcher is in the site's settings, pinned on mainnet, and read back
// under any key casing.
func TestSnChainSettingsWasmClaimBatcherAddress(t *testing.T) {
	if got := jsSnChainSettings(sdk.DefaultSnChainSettings()).Get("ClaimBatcherAddress").String(); got != testCtfBatcher {
		t.Fatalf("default batcher %q", got)
	}
	batcher := "0xb400000000000000000000000000000000000002"
	for _, key := range []string{"ClaimBatcherAddress", "claimBatcherAddress", "claim_batcher_address"} {
		settings, err := parseSnChainSettings(js.ValueOf(map[string]any{key: batcher}))
		if err != nil || settings.ClaimBatcherAddress != batcher {
			t.Fatalf("%s: %+v, %v", key, settings, err)
		}
	}
}

// testCtfBatchTx is a batch of a pool's and a capture the flag pool's claims, as
// URnetworkSnLedgerClaimTransactionsFor answers it.
func testCtfBatchTx() js.Value {
	items := sdk.NewSnLedgerClaimItemList()
	items.Add(&sdk.SnLedgerClaimItem{NoId: 1, Index: 2, RootIndex: 1, Kind: sdk.SnLedgerPoolKindPool, Label: "Pool 1", AmountRao: 700})
	items.Add(&sdk.SnLedgerClaimItem{NoId: 4294967297, Index: 1, RootIndex: 0, Kind: sdk.SnLedgerPoolKindCtf, Label: "Capture the flag", AmountRao: 48_025_660})
	txs := sdk.NewSnLedgerUnsignedTxList()
	txs.Add(&sdk.SnLedgerUnsignedTx{ChainId: 964, To: testCtfBatcher, Data: "0xd77e363f", Value: "0", AmountRao: 48_026_360, Gas: 3_500_000, Vault: testCtfVault2, Batch: true, Items: items})
	return jsSnLedgerUnsignedTxs(txs).Index(0)
}

// A batch transaction reaches the site with the accounts it claims.
func TestSnLedgerUnsignedTxsWasmBatch(t *testing.T) {
	tx := testCtfBatchTx()
	if !tx.Get("Batch").Bool() || tx.Get("Vault").String() != testCtfVault2 || tx.Get("To").String() != testCtfBatcher || tx.Get("NoId").Float() != 0 || tx.Get("Items").Length() != 2 {
		t.Fatalf("batch %v", jsonOf(t, tx))
	}
	item := tx.Get("Items").Index(1)
	if item.Get("NoId").Float() != 4294967297 || item.Get("Kind").String() != "ctf" || item.Get("Label").String() != "Capture the flag" || item.Get("AmountRao").Float() != 48_025_660 || item.Get("RootIndex").Float() != 0 {
		t.Fatalf("item %v", jsonOf(t, item))
	}
}

func testWordHex(v uint64) string {
	return fmt.Sprintf("%064x", v)
}

// testClaimedLog is vault2's Claimed event for account (noId, index) paying
// amount, as a wallet's receipt lists it.
func testClaimedLog(noId uint64, index uint64, amount uint64) map[string]any {
	return map[string]any{
		"address": testCtfVault2,
		"topics": []any{
			"0xea86c1b3284a090510a2098141252236197ab9664d20cf7a6d43ea767453bc2b",
			"0x" + testWordHex(noId), "0x" + testWordHex(index), "0x" + strings.Repeat("01", 32),
		},
		"data": "0x" + testWordHex(0) + testWordHex(amount) + testWordHex(amount) + strings.Repeat("0", 24) + testCtfBatcher[2:],
	}
}

func testBatcherLog(topic string, words ...string) map[string]any {
	return map[string]any{"address": testCtfBatcher, "topics": []any{topic}, "data": "0x" + strings.Join(words, "")}
}

// What a mined batch did for each account, from the receipt as viem and ethers
// give it: viem's bigints never reach JSON.stringify.
func TestSnLedgerClaimOutcomesWasm(t *testing.T) {
	outcomes := snExport(t, "URnetworkSnLedgerClaimOutcomes")
	logs := []any{
		testClaimedLog(1, 2, 700),
		testBatcherLog("0xb0727183796e52c5bb00a257c1eb36d8e94dbee5d6d87e259287e3372ae39ace", testWordHex(0), testWordHex(700)),
		testBatcherLog("0xf869308b71c9a01aff5b8d5408027b38b9076abea371ee96b2c875d292caf693", testWordHex(1), "969bf728"+strings.Repeat("0", 56)),
	}
	viem := js.ValueOf(map[string]any{"status": "success", "logs": logs})
	viem.Set("gasUsed", js.Global().Get("BigInt").Invoke(3_000_000))
	viem.Get("logs").Index(0).Set("blockNumber", js.Global().Get("BigInt").Invoke(17))
	ethers := js.ValueOf(map[string]any{"status": 1, "logs": logs})
	for name, receipt := range map[string]js.Value{"viem": viem, "ethers": ethers} {
		value, rejected := awaitApiPromise(t, outcomes.Invoke(testCtfBatchTx(), receipt))
		if !rejected.IsUndefined() {
			t.Fatalf("%s: rejected %v", name, rejected)
		}
		var got []map[string]any
		if err := json.Unmarshal([]byte(js.Global().Get("JSON").Call("stringify", value).String()), &got); err != nil || len(got) != 2 {
			t.Fatalf("%s: outcomes %v, %v", name, got, err)
		}
		wantFields(t, name, got[0], map[string]any{"no_id": float64(1), "index": float64(2), "status": "confirmed", "amount_rao": float64(700)})
		wantFields(t, name, got[1], map[string]any{"no_id": float64(4294967297), "index": float64(1), "status": "failed", "message": "already_claimed: NothingToClaim: another claim paid this account first"})
	}
	// a tx in the page's camelCase reads too, and without a receipt it rejects
	camel := jsonValue(`{"noId":0,"to":"` + testCtfBatcher + `","vault":"` + testCtfVault2 + `","batch":true,"items":[{"noId":1,"index":2}]}`)
	if _, rejected := awaitApiPromise(t, outcomes.Invoke(camel, ethers)); !rejected.IsUndefined() {
		t.Fatalf("camelCase rejected %v", rejected)
	}
	for name, args := range map[string][]any{"no receipt": {testCtfBatchTx()}, "no transaction": {}, "a receipt without a status": {testCtfBatchTx(), js.ValueOf(map[string]any{"logs": []any{}})}} {
		if _, rejected := awaitApiPromise(t, outcomes.Invoke(args...)); rejected.IsUndefined() {
			t.Fatalf("%s: resolved", name)
		}
	}
}

// A client's capture the flag wins from the route's json, as an object or its
// text, and a refused answer as unavailable.
func TestSnCtfClientOfWasm(t *testing.T) {
	ctfClientOf := snExport(t, "URnetworkSnCtfClientOf")
	answer := `{"as_of":"2026-12-11T22:24:12Z","client_id":"c","epochs":[{"epoch":9,"drawn":true,"root_status":"pending",` +
		`"providers":{"candidate":true,"reason":null,"country":"is","unit_family":4,"presence":{"present":664,"required":336,"samples":672,"excused":0},"units_in_draw":2,` +
		`"won":true,"rank":1,"slot":{"alpha":"0.048025660","rao":48025660},"pot_share":{"alpha":"0.028815396","rao":28815396},"pending":false,` +
		`"coldkey":"0xff05d4de50cdc2b6d6aca7b376cfdb00f65c4243f6799c4eaca182e877faf619","coldkey_ss58":"5Hq5oMjMnNDPJyRUScRVkWoFxYRdNqkHsjMBCxhPtYAEcnTd"},` +
		`"extenders":{"candidate":false,"reason":"not_in_role","country":null,"unit_family":null,"presence":null,"units_in_draw":null,` +
		`"won":false,"rank":null,"slot":null,"pot_share":null,"pending":false,"coldkey":null,"coldkey_ss58":null}}]}`
	for name, input := range map[string]js.Value{"object": jsonValue(answer), "text": js.ValueOf(answer)} {
		result := jsonOf(t, ctfClientOf.Invoke(input))
		latest, ok := result["latest_win"].(map[string]any)
		if !ok || result["error"] != nil {
			t.Fatalf("%s: %v", name, result)
		}
		wantFields(t, name, latest, map[string]any{"epoch": float64(9), "role": "provider", "country": "is", "rank": float64(1), "amount_rao": float64(76_841_056), "root_status": "pending"})
	}
	result := jsonOf(t, ctfClientOf.Invoke(jsonValue(`{"epochs":[]}`)))
	if errorOf, ok := result["error"].(map[string]any); !ok || errorOf["code"] != sdk.SnErrorCodeCtfUnavailable || result["latest_win"] != nil {
		t.Fatalf("refused %v", result)
	}
	if result = jsonOf(t, ctfClientOf.Invoke()); result["error"] == nil {
		t.Fatalf("no answer %v", result)
	}
}

// A payer's claim transactions reach the site in the sdk's json, snake_case:
// the transactions, the accounts still claimable, and the payer's balance
// check. The export refuses a payer that is not an EVM address, and a missing
// argument, before anything is read.
func TestSnLedgerClaimTransactionsForPayerWasm(t *testing.T) {
	items := sdk.NewSnLedgerClaimItemList()
	items.Add(&sdk.SnLedgerClaimItem{NoId: 4294967297, Index: 1, RootIndex: 0, Kind: sdk.SnLedgerPoolKindCtf, Label: "Capture the flag", AmountRao: 48_025_660})
	txs := sdk.NewSnLedgerUnsignedTxList()
	txs.Add(&sdk.SnLedgerUnsignedTx{NoId: 4294967297, Index: 1, ChainId: 964, To: testCtfVault2, Data: "0xb7a6b907", Value: "0", AmountRao: 48_025_660, Gas: 2_600_000, Vault: testCtfVault2, Items: items})
	still := sdk.NewSnLedgerClaimOutcomeList()
	still.Add(&sdk.SnLedgerClaimOutcome{NoId: 1, Index: 2, Status: sdk.SnLedgerClaimOutcomeStillClaimable, AmountRao: 700, Message: "needs_gas: claim again for this one"})
	result := jsonOf(t, jsJson(&sdk.SnLedgerClaimTransactionsResult{
		Transactions: txs, StillClaimable: still, PayerAddress: "0x00000000000000000000000000000000000000a1",
		PayerBalanceWei: "36000000000000000", FeePerGasWei: "11000000000", MaxCostWei: "28600000000000000", AllCostWei: "38500000000000000",
	}))
	wantFields(t, "result", result, map[string]any{"payer_address": "0x00000000000000000000000000000000000000a1", "payer_balance_wei": "36000000000000000",
		"fee_per_gas_wei": "11000000000", "max_cost_wei": "28600000000000000", "all_cost_wei": "38500000000000000", "needs_gas": false})
	transactions, _ := result["transactions"].([]any)
	stillClaimable, _ := result["still_claimable"].([]any)
	if len(transactions) != 1 || len(stillClaimable) != 1 {
		t.Fatalf("result %v", result)
	}
	wantFields(t, "transaction", transactions[0].(map[string]any), map[string]any{"no_id": float64(4294967297), "to": testCtfVault2, "gas": float64(2_600_000), "batch": false})
	wantFields(t, "still claimable", stillClaimable[0].(map[string]any), map[string]any{"no_id": float64(1), "index": float64(2), "status": "still_claimable", "amount_rao": float64(700)})

	forPayer := snExport(t, "URnetworkSnLedgerClaimTransactionsForPayer")
	settings := js.Global().Call("URnetworkDefaultSnChainSettings")
	coldkey := "5Hq5oMjMnNDPJyRUScRVkWoFxYRdNqkHsjMBCxhPtYAEcnTd"
	for name, args := range map[string][]any{
		"no payer":                       {settings, coldkey},
		"a payer as a number":            {settings, coldkey, 161},
		"a payer that is not an address": {settings, coldkey, "0x12"},
	} {
		_, rejected := awaitApiPromise(t, forPayer.Invoke(args...))
		if rejected.IsUndefined() {
			t.Fatalf("%s: resolved", name)
		}
		if message := rejected.Get("message").String(); name == "a payer that is not an address" && !strings.HasPrefix(message, sdk.SnErrorCodeInvalidAddress+": ") {
			t.Fatalf("%s: %s", name, message)
		}
	}
}

// A network's wins read through the export with each win's client and claim
// state, and the unclaimed ones the line shows; a page's read with its status
// reads a 401 to the network credential as no wins, and a 401 to a client
// token, or from a page that does not say which it sent, as unavailable.
func TestSnCtfClientOfResponseWasm(t *testing.T) {
	ofResponse := snExport(t, "URnetworkSnCtfClientOfResponse")
	network := `{"as_of":"2026-12-11T22:24:12Z","client_id":null,"epochs":[],"wins":[` +
		`{"epoch":9,"role":"provider","country":"is","rank":1,"client_id":"7d1e9a40-1b2c-4d3e-8f56-a1b2c3d4e5f6",` +
		`"amount":{"alpha":"0.048025660","rao":48025660},"slot":{"alpha":"0.048025660","rao":48025660},"pot_share":{"alpha":"0.000000000","rao":0},` +
		`"pending":false,"root_status":"pending","coldkey_ss58":"5Hq5oMjMnNDPJyRUScRVkWoFxYRdNqkHsjMBCxhPtYAEcnTd","claimed":false},` +
		`{"epoch":8,"role":"provider","country":"is","rank":2,"client_id":"7d1e9a40-1b2c-4d3e-8f56-a1b2c3d4e5f6",` +
		`"amount":{"alpha":"0.048025660","rao":48025660},"slot":{"alpha":"0.048025660","rao":48025660},"pot_share":{"alpha":"0.000000000","rao":0},` +
		`"pending":false,"root_status":"finalized","coldkey_ss58":"5Hq5oMjMnNDPJyRUScRVkWoFxYRdNqkHsjMBCxhPtYAEcnTd","claimed":true}]}`
	for name, value := range map[string]js.Value{
		"of":                                  snExport(t, "URnetworkSnCtfClientOf").Invoke(jsonValue(network)),
		"of response, the network credential": ofResponse.Invoke(200, network, true),
		"of response, a client token":         ofResponse.Invoke(200, network, false),
		"of response, not saying":             ofResponse.Invoke(200, network),
	} {
		result := jsonOf(t, value)
		unclaimed, _ := result["unclaimed_wins"].([]any)
		wins, _ := result["wins"].([]any)
		if result["error"] != nil || result["client_id"] != "" || len(wins) != 2 || len(unclaimed) != 1 {
			t.Fatalf("%s: %v", name, result)
		}
		wantFields(t, name, unclaimed[0].(map[string]any), map[string]any{"epoch": float64(9), "client_id": "7d1e9a40-1b2c-4d3e-8f56-a1b2c3d4e5f6", "claim_state": "unclaimed"})
		wantFields(t, name, wins[1].(map[string]any), map[string]any{"epoch": float64(8), "claim_state": "claimed"})
	}
	unauthorized := jsonValue(`{"status":401,"code":"unauthorized"}`)
	result := jsonOf(t, ofResponse.Invoke(401, unauthorized, true))
	if wins, _ := result["unclaimed_wins"].([]any); result["error"] != nil || wins == nil || len(wins) != 0 {
		t.Fatalf("401 to the network credential %v", result)
	}
	for _, args := range [][]any{{401, unauthorized, false}, {401, unauthorized}, {401, unauthorized, "true"}, {401, unauthorized, 1}, {503, network, true}, {"200", network, true}, {}} {
		result := jsonOf(t, ofResponse.Invoke(args...))
		if errorOf, ok := result["error"].(map[string]any); !ok || errorOf["code"] != sdk.SnErrorCodeCtfUnavailable {
			t.Fatalf("%v: %v", args, result)
		}
	}
}

// What a mined batch did for each account, with the reasons read from the
// chain: a confirmed account and vault2's own refusal need no chain read, and
// the export refuses the settings' other vault2, a coldkey that is not one and
// missing arguments.
func TestSnLedgerClaimOutcomesForWasm(t *testing.T) {
	outcomesFor := snExport(t, "URnetworkSnLedgerClaimOutcomesFor")
	settings := js.ValueOf(map[string]any{"vault2_address": testCtfVault2})
	coldkey := "5Hq5oMjMnNDPJyRUScRVkWoFxYRdNqkHsjMBCxhPtYAEcnTd"
	receipt := js.ValueOf(map[string]any{"status": 1, "logs": []any{
		testClaimedLog(1, 2, 700),
		testBatcherLog("0xb0727183796e52c5bb00a257c1eb36d8e94dbee5d6d87e259287e3372ae39ace", testWordHex(0), testWordHex(700)),
		testBatcherLog("0xf869308b71c9a01aff5b8d5408027b38b9076abea371ee96b2c875d292caf693", testWordHex(1), "969bf728"+strings.Repeat("0", 56)),
	}})
	value, rejected := awaitApiPromise(t, outcomesFor.Invoke(settings, coldkey, testCtfBatchTx(), receipt))
	if !rejected.IsUndefined() {
		t.Fatalf("rejected %v", rejected)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(js.Global().Get("JSON").Call("stringify", value).String()), &got); err != nil || len(got) != 2 {
		t.Fatalf("outcomes %v, %v", got, err)
	}
	wantFields(t, "confirmed", got[0], map[string]any{"no_id": float64(1), "index": float64(2), "status": "confirmed", "amount_rao": float64(700)})
	wantFields(t, "refused", got[1], map[string]any{"no_id": float64(4294967297), "index": float64(1), "status": "failed", "message": "already_claimed: NothingToClaim: another claim paid this account first"})

	for name, args := range map[string][]any{
		"the settings' other vault2": {js.Global().Call("URnetworkDefaultSnChainSettings"), coldkey, testCtfBatchTx(), receipt},
		"a coldkey that is not one":  {settings, "5Hq5", testCtfBatchTx(), receipt},
		"no coldkey":                 {settings},
		"no transaction":             {settings, coldkey},
		"no receipt":                 {settings, coldkey, testCtfBatchTx()},
	} {
		if _, rejected := awaitApiPromise(t, outcomesFor.Invoke(args...)); rejected.IsUndefined() {
			t.Fatalf("%s: resolved", name)
		}
	}
}
