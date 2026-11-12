package sdk

// Claims of every pool of a coldkey (sn CAPTURETHEFLAG.md §10): two or more
// accounts in one STClaimBatcher transaction (VERSION2-FINAL.md §10.2), one
// alone as before, a pool whose ledger could not be read reported without
// hiding the others, and no account in two transactions unless a mined batch
// provably did not attempt it.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/urnetwork/sdk/sn/evm"
	"github.com/urnetwork/sdk/sn/merkle"
)

const testCtfCumulative = int64(48_025_660)

// addCtfPool gives alice an account in operator 1's capture the flag pool,
// vault2 pool 2^32 + 1: account 1 of a two-account tree, with nothing paid.
// It returns the ledger's entry for it and its proof.
func (self *testV2Claim) addCtfPool(t *testing.T) (map[string]any, []merkle.LedgerNode) {
	t.Helper()
	root, total, proofs := buildTestLedgerTree([]testLedgerLeaf{{testColdkey(0x80), 96_051_320}, {self.alice, testCtfCumulative}})
	self.chain.roots[testCtfNoId] = []fakeVault2Root{{root, total, 2}}
	entry := testLedgerClaimEntry(testVault2, testCtfNoId, 1, 0, testCtfCumulative, proofs[1], 0)
	entry["kind"], entry["label"] = "ctf", "Capture the flag"
	return entry, proofs[1]
}

func (self *testV2Claim) pool1Entry() map[string]any {
	return testLedgerClaimEntry(testVault2, 1, 2, 1, testV2Cumulative, self.proofs[2], testV2Paid)
}

// the fixture with alice's pool and capture the flag accounts, and the batcher
// in the settings
func newTestV2BatchClaim(t *testing.T) (*testV2Claim, []merkle.LedgerNode) {
	t.Helper()
	f := newTestV2Claim(t)
	f.settings.ClaimBatcherAddress = testBatcher
	ctf, ctfProof := f.addCtfPool(t)
	f.answer(t, f.pool1Entry(), ctf)
	return f, ctfProof
}

// the claimMany calldata of alice's two accounts
func (self *testV2Claim) batchData(t *testing.T, ctfProof []merkle.LedgerNode, gasPerClaim uint64) []byte {
	t.Helper()
	data, err := evm.PackClaimMany(self.chain.vault2, []*evm.LedgerClaim{
		{NoId: 1, RootIndex: 1, Index: 2, Coldkey: self.alice, Cumulative: big.NewInt(testV2Cumulative), Proof: self.proofs[2]},
		{NoId: uint64(testCtfNoId), RootIndex: 0, Index: 1, Coldkey: self.alice, Cumulative: big.NewInt(testCtfCumulative), Proof: ctfProof},
	}, gasPerClaim)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// The least gas limit at which the fake's batcher attempts both of alice's
// accounts: its overhead and the first claim, then the second's cap, 1/63 of it
// and the bookkeeping, at the 2,600,000 cap.
const testBatchLeastGas = uint64(60_000 + 195_000 + 2_600_000 + 2_600_000/63 + 20_000)

// requireBatchGas requires the limit sized within the tolerance of the least,
// with a fifth more.
func requireBatchGas(t *testing.T, gas uint64) {
	t.Helper()
	low, high := testBatchLeastGas+testBatchLeastGas/5, testBatchLeastGas+snLedgerBatchTolerance
	if gas < low || high+high/5 < gas {
		t.Fatalf("batch gas %d, want the least %d within %d, and a fifth", gas, testBatchLeastGas, snLedgerBatchTolerance)
	}
}

// One claim claims the pool and the capture the flag pool in one batcher
// transaction from the gas key: each account Sent with its hash, then
// Confirmed with what vault2's Claimed event paid it.
func TestSnLedgerClaimBatchesTwoPools(t *testing.T) {
	f, ctfProof := newTestV2BatchClaim(t)
	events := f.claim(t)
	sent := f.chain.sentTxs()
	if len(sent) != 1 || len(events) != 4 {
		t.Fatalf("events %+v, %d sent", events, len(sent))
	}
	tx := sent[0]
	if tx.to != testBatcherAddress || tx.from != evm.AddressOf(f.key) || tx.value.Sign() != 0 || tx.nonce != 5 || !bytes.Equal(tx.data, f.batchData(t, ctfProof, snLedgerClaimGasFloor)) {
		t.Fatalf("transaction %+v", tx)
	}
	requireBatchGas(t, tx.gas)
	hash := events[0].txHash
	want := []testLedgerClaimEvent{
		{kind: "sent", noId: 1, index: 2, txHash: hash},
		{kind: "sent", noId: testCtfNoId, index: 1, txHash: hash},
		{kind: "confirmed", noId: 1, index: 2, txHash: hash, amount: testV2Cumulative - testV2Paid},
		{kind: "confirmed", noId: testCtfNoId, index: 1, txHash: hash, amount: testCtfCumulative},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events %+v, want %+v", events, want)
	}
	if f.chain.paid(1, 2) != testV2Cumulative || f.chain.paid(testCtfNoId, 1) != testCtfCumulative {
		t.Fatal("vault2 did not pay both accounts")
	}
	// each claim's preflight and estimate from the gas key, and the batcher's code
	// checked before the batch is sized
	gasKey := strings.ToLower(evm.AddressHex(evm.AddressOf(f.key)))
	if calls := f.chain.claimCalls(); len(calls) != 4 || calls[0].from != gasKey || calls[1].method != "eth_estimateGas" || calls[3].method != "eth_estimateGas" {
		t.Fatalf("claim calls %+v", calls)
	}
	methods := f.chain.methods()
	if i := indexOf(methods, "eth_getCode"); i < 0 || indexOf(methods, "eth_sendRawTransaction") < i {
		t.Fatalf("methods %q", methods)
	}

	// an estimate above the floor caps each item at it, as sn's claim tools do
	f, ctfProof = newTestV2BatchClaim(t)
	f.chain.estimate = 2_700_000
	f.claim(t)
	if sent := f.chain.sentTxs(); len(sent) != 1 || !bytes.Equal(sent[0].data, f.batchData(t, ctfProof, 2_700_000)) {
		t.Fatalf("sent %+v", sent)
	}
}

func indexOf(values []string, value string) int {
	for i, v := range values {
		if v == value {
			return i
		}
	}
	return -1
}

// A single account claims alone, as before, whatever the batcher; the batcher
// is not even read.
func TestSnLedgerClaimOneAccountAloneWithABatcher(t *testing.T) {
	f := newTestV2Claim(t)
	f.settings.ClaimBatcherAddress = testBatcher
	events := f.claim(t)
	sent := f.chain.sentTxs()
	if len(sent) != 1 || sent[0].to != f.chain.vault2 || len(events) != 2 || events[1].kind != "confirmed" {
		t.Fatalf("events %+v, sent %+v", events, sent)
	}
	if indexOf(f.chain.methods(), "eth_getCode") != -1 {
		t.Fatal("a single claim read the batcher")
	}
}

// requireClaimedAlone requires each of alice's two accounts claimed by its own
// transaction to vault2, in turn, the largest first: capture the flag's 0.048 α
// before the pool's 0.023 α, though the pool is first in the ledger.
func requireClaimedAlone(t *testing.T, f *testV2Claim, events []testLedgerClaimEvent) {
	t.Helper()
	sent := f.chain.sentTxs()
	if len(sent) != 2 || sent[0].to != f.chain.vault2 || sent[1].to != f.chain.vault2 || len(events) != 4 {
		t.Fatalf("events %+v, %d sent", events, len(sent))
	}
	if events[0].kind != "sent" || events[0].noId != testCtfNoId || events[1].kind != "confirmed" || events[1].amount != testCtfCumulative ||
		events[2].kind != "sent" || events[2].noId != 1 || events[3].kind != "confirmed" || events[3].amount != testV2Cumulative-testV2Paid ||
		events[0].txHash == events[2].txHash {
		t.Fatalf("events %+v", events)
	}
}

// Without a batcher it can trust, or one the gas key can pay for, each account
// is claimed alone, as before: nothing was sent, so nothing is claimed twice.
func TestSnLedgerClaimBatchFallsBackToClaimsAlone(t *testing.T) {
	for _, c := range []struct {
		name   string
		mutate func(f *testV2Claim)
	}{
		{"no batcher", func(f *testV2Claim) { f.settings.ClaimBatcherAddress = "" }},
		{"a batcher address that does not parse", func(f *testV2Claim) { f.settings.ClaimBatcherAddress = "0xnot an address" }},
		{"the zero address", func(f *testV2Claim) { f.settings.ClaimBatcherAddress = "0x0000000000000000000000000000000000000000" }},
		{"no code at the batcher", func(f *testV2Claim) { f.chain.batcherCode = nil }},
		{"code that is not the release batcher's", func(f *testV2Claim) {
			f.chain.batcherCode = append(f.chain.batcherCode[:len(f.chain.batcherCode)-1:len(f.chain.batcherCode)-1], 0x34)
		}},
		{"the code cannot be read", func(f *testV2Claim) { f.chain.failing["eth_getCode"] = &evm.RpcError{Code: -32000, Message: "down"} }},
		{"no block gas limit", func(f *testV2Claim) { f.chain.blockGasLimit = 0 }},
		{"a batch that cannot be sized", func(f *testV2Claim) {
			f.chain.bySelector["d77e363f"] = fakeVault2Answer{err: &evm.RpcError{Code: -32000, Message: "out of gas"}}
		}},
		// 3,142,841 gas alone and about 3.5M for the batch, at 11 gwei
		{"a gas key that can pay one claim but not the batch", func(f *testV2Claim) { f.chain.balance = big.NewInt(36_000_000_000_000_000) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, _ := newTestV2BatchClaim(t)
			c.mutate(f)
			requireClaimedAlone(t, f, f.claim(t))
		})
	}
}

// A mined batch reports each account: what vault2 paid, why it refused one, and
// the accounts it did not attempt, which are then claimed alone.
func TestSnLedgerClaimBatchOutcomes(t *testing.T) {
	// a third party claims the capture the flag account between the preflight
	// and the batch: vault2 refuses it NothingToClaim, success for the account
	f, _ := newTestV2BatchClaim(t)
	f.chain.beforeMine = func() { f.chain.setClaimed(testCtfNoId, 1, testCtfCumulative) }
	events := f.claim(t)
	if len(f.chain.sentTxs()) != 1 || len(events) != 4 || events[2].kind != "confirmed" || events[2].noId != 1 ||
		events[3].kind != "failed" || events[3].noId != testCtfNoId || events[3].message != SnErrorCodeAlreadyClaimed+": NothingToClaim: another claim paid this account first" {
		t.Fatalf("paid first: %+v", events)
	}

	// third parties lengthen vault2's staking-hotkey list before the batch is
	// included: the first claim uses more than its estimate, the batcher cannot
	// cover the second, and it is claimed alone after
	f, _ = newTestV2BatchClaim(t)
	f.chain.mineExtraGas = 1_000_000
	events = f.claim(t)
	sent := f.chain.sentTxs()
	if len(sent) != 2 || sent[0].to != testBatcherAddress || sent[1].to != f.chain.vault2 || len(events) != 5 {
		t.Fatalf("not attempted: events %+v, sent %d", events, len(sent))
	}
	batchHash := events[0].txHash
	if events[1].txHash != batchHash || events[2].kind != "confirmed" || events[2].noId != 1 ||
		events[3].kind != "sent" || events[3].noId != testCtfNoId || events[3].txHash == batchHash ||
		events[4].kind != "confirmed" || events[4].noId != testCtfNoId || events[4].amount != testCtfCumulative {
		t.Fatalf("not attempted: %+v", events)
	}
	if f.chain.paid(testCtfNoId, 1) != testCtfCumulative {
		t.Fatal("the account the batch did not attempt was not claimed")
	}

	// the runtime refuses the first transfer once the batch is mined: the
	// refusal consumes its whole cap, the second is not attempted, and alone its
	// preflight is refused too; each reads as the runtime's transfer floor
	f, _ = newTestV2BatchClaim(t)
	f.chain.beforeMine = func() { f.chain.runtimeRefusal = "AmountTooLow" }
	events = f.claim(t)
	wantMessage := SnErrorCodeNotClaimable + ": AmountTooLow: the claim is below the runtime's transfer floor"
	if len(f.chain.sentTxs()) != 1 || len(events) != 4 || events[2].kind != "failed" || events[2].noId != 1 || events[2].message != wantMessage ||
		events[3].kind != "failed" || events[3].noId != testCtfNoId || events[3].message != wantMessage {
		t.Fatalf("runtime refusal: %+v", events)
	}
	f.requireClaimable(t)

	// vault2 refuses one account at its preflight: it is reported, and the other
	// is claimed alone
	f, _ = newTestV2BatchClaim(t)
	f.chain.beforeClaimCall = func() { f.chain.setClaimed(testCtfNoId, 1, testCtfCumulative) }
	events = f.claim(t)
	sent = f.chain.sentTxs()
	if len(sent) != 1 || sent[0].to != f.chain.vault2 || len(events) != 3 || events[0].kind != "failed" || events[0].noId != testCtfNoId ||
		!strings.HasPrefix(events[0].message, SnErrorCodeAlreadyClaimed+": ") || events[1].kind != "sent" || events[2].kind != "confirmed" {
		t.Fatalf("preflight refusal: %+v", events)
	}
}

// Once a batch is sent, its accounts are claimed by it alone: a batch whose
// send or receipt failed, that reverted, or whose receipt has no Claimed event
// fails each account and sends nothing more.
func TestSnLedgerClaimBatchIsNeverSentTwice(t *testing.T) {
	for _, c := range []struct {
		name    string
		mutate  func(f *testV2Claim)
		sent    int
		message string
	}{
		{"the send fails", func(f *testV2Claim) {
			f.chain.failing["eth_sendRawTransaction"] = &evm.RpcError{Code: -32000, Message: "nonce too low"}
		}, 0, "nonce too low"},
		{"the receipt cannot be read", func(f *testV2Claim) {
			f.chain.failing["eth_getTransactionReceipt"] = &evm.RpcError{Code: -32000, Message: "receipts down"}
		}, 1, SnErrorCodeClaimFailed + ": rpc error -32000: receipts down"},
		{"the batch reverts", func(f *testV2Claim) { f.chain.batchReverts = true }, 1, SnErrorCodeClaimFailed + ": the batch transaction reverted"},
		{"no Claimed event", func(f *testV2Claim) { f.chain.dropClaimedLog = true }, 1, SnErrorCodeClaimFailed + ": the batch transaction has no Claimed event for this account"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, _ := newTestV2BatchClaim(t)
			c.mutate(f)
			events := f.claim(t)
			if sent := f.chain.sentTxs(); len(sent) != c.sent || (0 < len(sent) && sent[0].to != testBatcherAddress) {
				t.Fatalf("sent %+v", sent)
			}
			var failed []testLedgerClaimEvent
			for _, event := range events {
				switch event.kind {
				case "failed":
					failed = append(failed, event)
				case "confirmed":
					t.Fatalf("confirmed %+v", event)
				}
			}
			if len(failed) != 2 || failed[0].noId != 1 || failed[1].noId != testCtfNoId || !strings.Contains(failed[0].message, c.message) || !strings.Contains(failed[1].message, c.message) {
				t.Fatalf("events %+v, want each failed with %q", events, c.message)
			}
		})
	}
}

// A pool whose ledger the server could not read is reported with its pool id
// and index 0, and never hides the others; with nothing else to claim it is
// the reason, never "nothing to claim".
func TestSnLedgerClaimReportsUnavailablePools(t *testing.T) {
	f := newTestV2Claim(t)
	// the ledger answers these accounts, and names the capture the flag pool
	// unavailable
	unavailable := func(entries ...map[string]any) {
		accounts := []any{}
		for _, entry := range entries {
			accounts = append(accounts, entry)
		}
		f.ledger.answer(http.StatusOK, testLedgerJson(t, map[string]any{
			"coldkey":           "0x" + hex.EncodeToString(f.alice[:]),
			"accounts":          accounts,
			"unavailable_pools": []any{map[string]any{"vault": testVault2, "no_id": testCtfNoId, "kind": "ctf", "label": "Capture the flag"}},
		}))
	}
	unavailable(f.pool1Entry())
	wantMessage := SnErrorCodeLedgerUnavailable + ": the ledger of Capture the flag (pool 4294967297) could not be read"
	events := f.claim(t)
	if len(events) != 3 || events[0].kind != "failed" || events[0].noId != testCtfNoId || events[0].index != 0 || events[0].message != wantMessage ||
		events[1].kind != "sent" || events[2].kind != "confirmed" || events[2].noId != 1 {
		t.Fatalf("events %+v", events)
	}

	unavailable()
	events = f.claim(t)
	if len(events) != 1 || events[0].message != wantMessage {
		t.Fatalf("only unavailable: %+v", events)
	}
	if _, err := f.engine(t).unsignedTxs(context.Background()); err == nil || err.Error() != wantMessage {
		t.Fatalf("transactions: %v", err)
	}
	// beside a claimable account the transactions are built, and the page knows
	// the pool is unavailable from its read
	f.chain.setClaimed(1, 2, testV2Paid)
	unavailable(f.pool1Entry())
	if txs, err := f.engine(t).unsignedTxs(context.Background()); err != nil || txs.Len() != 1 {
		t.Fatalf("transactions %+v, %v", txs, err)
	}
}

// For an external signer two accounts are one batch transaction to the
// batcher, sized for any sender, with the accounts it claims; its mined
// receipt reads each account's outcome. One account is a claim alone, as before.
func TestSnLedgerClaimTransactionsBatchForExternalSigners(t *testing.T) {
	f, ctfProof := newTestV2BatchClaim(t)
	txs, err := SnLedgerClaimTransactionsFor(f.settings, f.aliceSs58)
	if err != nil || txs.Len() != 1 {
		t.Fatalf("transactions %+v, %v", txs, err)
	}
	tx := txs.Get(0)
	wantItems := testClaimItems(
		&SnLedgerClaimItem{NoId: 1, Index: 2, RootIndex: 1, Kind: SnLedgerPoolKindPool, Label: "Pool 1", AmountRao: testV2Cumulative - testV2Paid},
		&SnLedgerClaimItem{NoId: testCtfNoId, Index: 1, RootIndex: 0, Kind: SnLedgerPoolKindCtf, Label: "Capture the flag", AmountRao: testCtfCumulative},
	)
	if !tx.Batch || tx.To != testBatcher || tx.Vault != testVault2 || tx.NoId != 0 || tx.Index != 0 || tx.Value != "0" || tx.ChainId != SnChainIdTestnet ||
		tx.AmountRao != testV2Cumulative-testV2Paid+testCtfCumulative || tx.Data != "0x"+hex.EncodeToString(f.batchData(t, ctfProof, snLedgerClaimGasFloor)) ||
		!reflect.DeepEqual(tx.Items, wantItems) {
		t.Fatalf("transaction %+v", tx)
	}
	requireBatchGas(t, uint64(tx.Gas))
	// sized without a sender, which neither the batcher nor vault2 depends on
	for _, call := range f.chain.calls {
		if call.selector == "d77e363f" && call.from != "" {
			t.Fatalf("a batch call from %s", call.from)
		}
	}
	if len(f.chain.sentTxs()) != 0 {
		t.Fatal("building the transactions sent one")
	}
	raw, _ := json.Marshal(tx)
	if !strings.Contains(string(raw), `"to":"`+testBatcher+`"`) || !strings.Contains(string(raw), `"vault":"`+testVault2+`","batch":true,"items":[{"no_id":1,"index":2,"root_index":1,"kind":"pool","label":"Pool 1","amount_rao":23456789},{"no_id":4294967297,"index":1,"root_index":0,"kind":"ctf","label":"Capture the flag","amount_rao":48025660}]`) {
		t.Fatalf("json %s", raw)
	}

	// the reader's wallet sends it, and its receipt reads both confirmed
	data, _ := evm.ParseHexBytes(tx.Data)
	signed, _, err := (&evm.LegacyTx{Nonce: 0, GasPrice: big.NewInt(11_000_000_000), Gas: uint64(tx.Gas), To: testBatcherAddress, Value: big.NewInt(0), Data: data}).Signed(big.NewInt(SnChainIdTestnet), f.key)
	if err != nil {
		t.Fatal(err)
	}
	txHash, err := f.engine(t).client.SendRawTransaction(context.Background(), signed)
	if err != nil {
		t.Fatal(err)
	}
	f.chain.mutex.Lock()
	receipt, _ := json.Marshal(f.chain.receipts[txHash])
	f.chain.mutex.Unlock()
	outcomes, err := SnLedgerClaimOutcomesOf(tx, string(receipt))
	want := []*SnLedgerClaimOutcome{
		{NoId: 1, Index: 2, Status: SnLedgerClaimOutcomeConfirmed, AmountRao: testV2Cumulative - testV2Paid},
		{NoId: testCtfNoId, Index: 1, Status: SnLedgerClaimOutcomeConfirmed, AmountRao: testCtfCumulative},
	}
	if err != nil || !reflect.DeepEqual(outcomes.getAll(), want) {
		t.Fatalf("outcomes %+v, %v", outcomes, err)
	}

	// one account: a claim alone, as before, with no batcher read
	f, _ = newTestV2BatchClaim(t)
	f.answer(t, f.pool1Entry())
	if txs, err = SnLedgerClaimTransactionsFor(f.settings, f.aliceSs58); err != nil || txs.Len() != 1 || txs.Get(0).Batch || txs.Get(0).To != testVault2 || txs.Get(0).Gas != 2_600_000 {
		t.Fatalf("one account %+v, %v", txs, err)
	}
	if indexOf(f.chain.methods(), "eth_getCode") != -1 {
		t.Fatal("a single claim read the batcher")
	}

	// a batcher it cannot trust: a claim alone each
	f, _ = newTestV2BatchClaim(t)
	f.chain.batcherCode = nil
	if txs, err = SnLedgerClaimTransactionsFor(f.settings, f.aliceSs58); err != nil || txs.Len() != 2 || txs.Get(0).Batch || txs.Get(1).Batch || txs.Get(1).NoId != testCtfNoId {
		t.Fatalf("no batcher %+v, %v", txs, err)
	}
}

// testBatchReceipt is a mined batch's receipt as JSON-RPC gives it.
func testBatchReceipt(status any, logs ...map[string]any) string {
	receipt := map[string]any{"status": status, "logs": logs}
	raw, _ := json.Marshal(receipt)
	return string(raw)
}

func testBatcherLog(topic [32]byte, words ...string) map[string]any {
	return map[string]any{"address": testBatcher, "topics": []string{"0x" + hex.EncodeToString(topic[:])}, "data": "0x" + strings.Join(words, "")}
}

func testWord(v int64) string {
	return strings.TrimPrefix(testWordHex(big.NewInt(v)), "0x")
}

// What a mined claim transaction did for each of its accounts, read from its
// receipt as a page's wallet gives it: vault2's Claimed event confirms an
// account, and a batch's events say why one was not paid or that it was not
// attempted.
func TestSnLedgerClaimOutcomesOf(t *testing.T) {
	vault2, _ := evm.ParseAddress(testVault2)
	alice := testColdkey(1)
	claimed := func(noId int64, index int64, amount int64) map[string]any {
		chain := &fakeVault2{}
		c := &testLedgerClaimArgs{noId: noId, index: index, coldkey: alice, cumulative: big.NewInt(amount)}
		return chain.testClaimedLog(vault2, c, big.NewInt(amount), testBatcherAddress)
	}
	batch := &SnLedgerUnsignedTx{To: testBatcher, Vault: testVault2, Batch: true, Items: testClaimItems(
		&SnLedgerClaimItem{NoId: 1, Index: 2}, &SnLedgerClaimItem{NoId: testCtfNoId, Index: 1}, &SnLedgerClaimItem{NoId: 3, Index: 0},
	)}
	nothingToClaim := hex.EncodeToString(testSelectorData("NothingToClaim()")) + strings.Repeat("0", 56)
	read := func(tx *SnLedgerUnsignedTx, receipt string) []*SnLedgerClaimOutcome {
		t.Helper()
		outcomes, err := SnLedgerClaimOutcomesOf(tx, receipt)
		if err != nil {
			t.Fatalf("receipt %s: %v", receipt, err)
		}
		return outcomes.getAll()
	}

	outcomes := read(batch, testBatchReceipt("0x1",
		claimed(1, 2, 700),
		testBatcherLog(evm.ClaimPaidTopic, testWord(0), testWord(700)),
		testBatcherLog(evm.ClaimRefusedTopic, testWord(1), nothingToClaim),
		testBatcherLog(evm.ClaimsNotAttemptedTopic, testWord(2)),
	))
	want := []*SnLedgerClaimOutcome{
		{NoId: 1, Index: 2, Status: SnLedgerClaimOutcomeConfirmed, AmountRao: 700},
		{NoId: testCtfNoId, Index: 1, Status: SnLedgerClaimOutcomeFailed, Message: SnErrorCodeAlreadyClaimed + ": NothingToClaim: another claim paid this account first"},
		{NoId: 3, Index: 0, Status: SnLedgerClaimOutcomeNotAttempted},
	}
	if !reflect.DeepEqual(outcomes, want) {
		t.Fatalf("outcomes %+v", outcomes)
	}

	// the statuses ethers and viem give
	for _, status := range []any{1, true, "success", "0x01", "1", "true"} {
		outcomes = read(batch, testBatchReceipt(status, claimed(1, 2, 700), testBatcherLog(evm.ClaimPaidTopic, testWord(0), testWord(700)), testBatcherLog(evm.ClaimsNotAttemptedTopic, testWord(1))))
		if outcomes[0].Status != SnLedgerClaimOutcomeConfirmed || outcomes[1].Status != SnLedgerClaimOutcomeNotAttempted {
			t.Fatalf("status %v: %+v", status, outcomes)
		}
	}
	for _, status := range []any{0, false, "reverted", "0x0", "0", "false"} {
		for _, outcome := range read(batch, testBatchReceipt(status)) {
			if outcome.Status != SnLedgerClaimOutcomeFailed || outcome.Message != SnErrorCodeClaimFailed+": the batch transaction reverted" {
				t.Fatalf("status %v: %+v", status, outcome)
			}
		}
	}

	// a refusal without data ran out of its cap, as a transfer the runtime refuses
	outcomes = read(batch, testBatchReceipt("0x1", testBatcherLog(evm.ClaimRefusedTopic, testWord(0), strings.Repeat("0", 64)), testBatcherLog(evm.ClaimsNotAttemptedTopic, testWord(1))))
	if outcomes[0].Message != SnErrorCodeClaimFailed+": the staking precompile refused the transfer" {
		t.Fatalf("runtime refusal %+v", outcomes[0])
	}
	// batcher events that do not say why: a Claimed event still confirms
	outcomes = read(batch, testBatchReceipt("0x1", claimed(1, 2, 700), testBatcherLog(evm.ClaimsNotAttemptedTopic, testWord(2))))
	if outcomes[0].Status != SnLedgerClaimOutcomeConfirmed || outcomes[1].Status != SnLedgerClaimOutcomeFailed || !strings.Contains(outcomes[1].Message, "its batcher events do not say why") {
		t.Fatalf("unreadable events %+v", outcomes)
	}
	// another vault's Claimed event pays nothing
	other := claimed(1, 2, 700)
	other["address"] = "0x" + strings.Repeat("99", 20)
	if outcomes = read(batch, testBatchReceipt("0x1", other, testBatcherLog(evm.ClaimsNotAttemptedTopic, testWord(0)))); outcomes[0].Status != SnLedgerClaimOutcomeNotAttempted {
		t.Fatalf("another vault %+v", outcomes[0])
	}

	// a claim alone; one built before Vault named the vault reads it from To
	single := &SnLedgerUnsignedTx{NoId: 1, Index: 2, To: testVault2, Items: testClaimItems(&SnLedgerClaimItem{NoId: 1, Index: 2})}
	if outcomes = read(single, testBatchReceipt("0x1", claimed(1, 2, 9))); outcomes[0].Status != SnLedgerClaimOutcomeConfirmed || outcomes[0].AmountRao != 9 {
		t.Fatalf("alone %+v", outcomes[0])
	}
	if outcomes = read(single, testBatchReceipt("0x1")); outcomes[0].Message != SnErrorCodeClaimFailed+": the claim transaction has no Claimed event for this account" {
		t.Fatalf("alone without an event %+v", outcomes[0])
	}
	if outcomes = read(single, testBatchReceipt("0x0")); outcomes[0].Message != SnErrorCodeClaimFailed+": the claim transaction reverted" {
		t.Fatalf("alone reverted %+v", outcomes[0])
	}

	for name, c := range map[string]struct {
		tx      *SnLedgerUnsignedTx
		receipt string
	}{
		"no transaction":                   {nil, testBatchReceipt("0x1")},
		"no items":                         {&SnLedgerUnsignedTx{To: testVault2}, testBatchReceipt("0x1")},
		"not json":                         {single, "receipt"},
		"no status":                        {single, `{"logs":[]}`},
		"a status of 2":                    {single, testBatchReceipt(2)},
		"a status of words":                {single, testBatchReceipt("pending")},
		"a batcher that is not an address": {&SnLedgerUnsignedTx{To: "batcher", Vault: testVault2, Batch: true, Items: batch.Items}, testBatchReceipt("0x1")},
	} {
		if _, err := SnLedgerClaimOutcomesOf(c.tx, c.receipt); err == nil {
			t.Errorf("%s: read", name)
		}
	}
}

// testSendAsTheWallet sends tx as the reader's wallet does, signed with its
// gas limit, and returns its receipt as JSON-RPC gives it.
func testSendAsTheWallet(t *testing.T, f *testV2Claim, tx *SnLedgerUnsignedTx) string {
	t.Helper()
	data, err := evm.ParseHexBytes(tx.Data)
	if err != nil {
		t.Fatal(err)
	}
	to, err := evm.ParseAddress(tx.To)
	if err != nil {
		t.Fatal(err)
	}
	signed, _, err := (&evm.LegacyTx{Nonce: 0, GasPrice: big.NewInt(11_000_000_000), Gas: uint64(tx.Gas), To: to, Value: big.NewInt(0), Data: data}).Signed(big.NewInt(SnChainIdTestnet), f.key)
	if err != nil {
		t.Fatal(err)
	}
	txHash, err := f.engine(t).client.SendRawTransaction(context.Background(), signed)
	if err != nil {
		t.Fatal(err)
	}
	f.chain.mutex.Lock()
	defer f.chain.mutex.Unlock()
	receipt, err := json.Marshal(f.chain.receipts[txHash])
	if err != nil {
		t.Fatal(err)
	}
	return string(receipt)
}

// A host that sends the claims with its own wallet reads why an account was
// not paid from the chain, as the device's claim does, so it shows the same
// outcome: the runtime refusing a mined batch's transfer under its transfer
// floor reads not_claimable (Below the claim minimum), not claim_failed as the
// pure reader must read it; another refusal of the runtime reads claim_failed
// with its reason, and one the chain no longer gives claim_failed. A claim
// alone that reverted is called again: AmountTooLow is not_claimable there
// too, and NothingToClaim already_claimed.
func TestSnLedgerClaimOutcomesFor(t *testing.T) {
	floor := SnErrorCodeNotClaimable + ": AmountTooLow: the claim is below the runtime's transfer floor"
	simulation := fakeVault2Call{method: "eth_call", from: testVault2, to: "0x0000000000000000000000000000000000000805", selector: "17ce5f62"}
	for _, c := range []struct {
		name string
		// the runtime's refusal once the batch is mined, and when the outcomes
		// are read
		mined, read string
		want        string
	}{
		{"the transfer floor", "AmountTooLow", "AmountTooLow", floor},
		{"too many staking hotkeys", "TooManyStakingHotkeys", "TooManyStakingHotkeys", SnErrorCodeClaimFailed + ": TooManyStakingHotkeys"},
		{"a refusal the chain no longer gives", "AmountTooLow", "", SnErrorCodeClaimFailed + ": the claim reverted without a reason"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, _ := newTestV2BatchClaim(t)
			txs, err := SnLedgerClaimTransactionsFor(f.settings, f.aliceSs58)
			if err != nil || txs.Len() != 1 || !txs.Get(0).Batch {
				t.Fatalf("transactions %+v, %v", txs, err)
			}
			tx := txs.Get(0)
			// the runtime refuses the first transfer, which consumes its cap, and
			// the batch does not attempt the second
			f.chain.beforeMine = func() { f.chain.runtimeRefusal = c.mined }
			receipt := testSendAsTheWallet(t, f, tx)
			f.chain.runtimeRefusal = c.read

			pure, err := SnLedgerClaimOutcomesOf(tx, receipt)
			want := []*SnLedgerClaimOutcome{
				{NoId: 1, Index: 2, Status: SnLedgerClaimOutcomeFailed, Message: SnErrorCodeClaimFailed + ": the staking precompile refused the transfer"},
				{NoId: testCtfNoId, Index: 1, Status: SnLedgerClaimOutcomeNotAttempted},
			}
			if err != nil || !reflect.DeepEqual(pure.getAll(), want) {
				t.Fatalf("pure outcomes %+v, %v", pure, err)
			}
			outcomes, err := SnLedgerClaimOutcomesFor(f.settings, f.aliceSs58, tx, receipt)
			want[0].Message = c.want
			if err != nil || !reflect.DeepEqual(outcomes.getAll(), want) {
				t.Fatalf("outcomes %+v, %v", outcomes, err)
			}
			// the reason is vault2's transfer of the amount to alice on the escrow
			// hotkey, simulated
			if calls := f.chain.calls; calls[len(calls)-1] != simulation {
				t.Fatalf("last call %+v", calls[len(calls)-1])
			}
			f.requireClaimable(t)

			// the device's claim in the same case reads the same
			f, _ = newTestV2BatchClaim(t)
			f.chain.beforeMine = func() { f.chain.runtimeRefusal = c.mined }
			events := f.claimReadingAt(t, c.read)
			if len(events) < 3 || events[2].kind != "failed" || events[2].noId != 1 || events[2].message != c.want {
				t.Fatalf("the device's claim %+v, want %q", events, c.want)
			}
		})
	}

	// a claim alone that reverted is called again
	for _, c := range []struct {
		name   string
		revert func(f *testV2Claim)
		want   string
	}{
		{"under the transfer floor", func(f *testV2Claim) { f.chain.runtimeRefusal = "AmountTooLow" }, floor},
		{"paid first by another claim", func(f *testV2Claim) { f.chain.setClaimed(1, 2, testV2Cumulative) }, SnErrorCodeAlreadyClaimed + ": NothingToClaim: another claim paid this account first"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newTestV2Claim(t)
			txs, err := SnLedgerClaimTransactionsFor(f.settings, f.aliceSs58)
			if err != nil || txs.Len() != 1 || txs.Get(0).Batch {
				t.Fatalf("transactions %+v, %v", txs, err)
			}
			tx := txs.Get(0)
			f.chain.beforeMine = func() { c.revert(f) }
			receipt := testSendAsTheWallet(t, f, tx)
			if pure, err := SnLedgerClaimOutcomesOf(tx, receipt); err != nil || pure.Get(0).Message != SnErrorCodeClaimFailed+": the claim transaction reverted" {
				t.Fatalf("pure outcomes %+v, %v", pure, err)
			}
			outcomes, err := SnLedgerClaimOutcomesFor(f.settings, f.aliceSs58, tx, receipt)
			want := []*SnLedgerClaimOutcome{{NoId: 1, Index: 2, Status: SnLedgerClaimOutcomeFailed, Message: c.want}}
			if err != nil || !reflect.DeepEqual(outcomes.getAll(), want) {
				t.Fatalf("outcomes %+v, %v", outcomes, err)
			}
		})
	}

	// a confirmed claim reads no reason from the chain, and the inputs are
	// checked before the receipt is read
	f, _ := newTestV2BatchClaim(t)
	txs, err := SnLedgerClaimTransactionsFor(f.settings, f.aliceSs58)
	if err != nil {
		t.Fatal(err)
	}
	tx := txs.Get(0)
	receipt := testSendAsTheWallet(t, f, tx)
	calls := len(f.chain.methods())
	outcomes, err := SnLedgerClaimOutcomesFor(f.settings, f.aliceSs58, tx, receipt)
	if err != nil || outcomes.Len() != 2 || outcomes.Get(0).Status != SnLedgerClaimOutcomeConfirmed || outcomes.Get(1).Status != SnLedgerClaimOutcomeConfirmed || len(f.chain.methods()) != calls {
		t.Fatalf("confirmed %+v, %v, %d calls", outcomes, err, len(f.chain.methods())-calls)
	}
	otherVault := *tx
	otherVault.Vault = "0x" + strings.Repeat("99", 20)
	unconfigured := f.settings.Copy()
	unconfigured.Vault2Address = ""
	for name, read := range map[string]func() error{
		"another vault2": func() error {
			_, err := SnLedgerClaimOutcomesFor(f.settings, f.aliceSs58, &otherVault, receipt)
			return err
		},
		"a coldkey that is not one": func() error {
			_, err := SnLedgerClaimOutcomesFor(f.settings, "5Hq5", tx, receipt)
			return err
		},
		"no vault2 configured": func() error {
			_, err := SnLedgerClaimOutcomesFor(unconfigured, f.aliceSs58, tx, receipt)
			return err
		},
		"no transaction": func() error {
			_, err := SnLedgerClaimOutcomesFor(f.settings, f.aliceSs58, nil, receipt)
			return err
		},
		"no receipt": func() error {
			_, err := SnLedgerClaimOutcomesFor(f.settings, f.aliceSs58, tx, "")
			return err
		},
	} {
		if err := read(); err == nil {
			t.Errorf("%s: read", name)
		}
	}
	if len(f.chain.methods()) != calls {
		t.Fatal("a refused read called the chain")
	}
}

// claimReadingAt runs one claim of the gas key, the runtime refusing with
// refusal by the time the device reads why a mined batch did not pay an
// account.
func (self *testV2Claim) claimReadingAt(t *testing.T, refusal string) []testLedgerClaimEvent {
	t.Helper()
	mined := self.chain.beforeMine
	self.chain.beforeMine = func() {
		if mined != nil {
			mined()
		}
		// the device reads the receipt, then simulates: the refusal by then
		self.chain.beforeReceipt = func() { self.chain.runtimeRefusal = refusal }
	}
	return self.claim(t)
}

// The batcher is in the settings' json beside vault2, pinned on mainnet to the
// release batcher and empty on testnet, overlaid by Merge and read from the
// network space's sn_chain values.
func TestSnChainSettingsClaimBatcherAddress(t *testing.T) {
	if DefaultSnChainSettings().ClaimBatcherAddress != "0x14174e7c5671ce1499aef9c68d74bebffa58c41a" || SnTestnetChainSettings().ClaimBatcherAddress != "" {
		t.Fatal("the mainnet batcher pin or the empty testnet default differs from the release")
	}
	settings := SnTestnetChainSettings()
	settings.ClaimBatcherAddress = testBatcher
	raw, err := json.Marshal(settings)
	if err != nil || !strings.Contains(string(raw), `"vault2_address":"","claim_batcher_address":"`+testBatcher+`"`) {
		t.Fatalf("json %s, %v", raw, err)
	}
	var back SnChainSettings
	if err := json.Unmarshal(raw, &back); err != nil || back.ClaimBatcherAddress != testBatcher {
		t.Fatalf("round trip %+v, %v", back, err)
	}
	if merged := DefaultSnChainSettings().Merge(settings); merged.ClaimBatcherAddress != testBatcher {
		t.Fatalf("merged %+v", merged)
	}
	if kept := DefaultSnChainSettings().Merge(NewSnChainSettings()); kept.ClaimBatcherAddress != "0x14174e7c5671ce1499aef9c68d74bebffa58c41a" {
		t.Fatal("an empty override cleared the batcher")
	}
	var values NetworkSpaceValues
	if err := json.Unmarshal([]byte(`{"sn_chain":{"claim_batcher_address":"`+testBatcher+`"}}`), &values); err != nil {
		t.Fatal(err)
	}
	device := &snDevice{networkSpace: &NetworkSpace{values: values}, state: newDeviceSn()}
	if got := device.GetSnChainSettings(); got.ClaimBatcherAddress != testBatcher {
		t.Fatalf("network space settings %+v", got)
	}
}
