package sdk

// A payer that can pay for one claim but not the batch of every account claims
// the largest account alone, and the others stay claimable (the owner,
// 2026-11-11): the device's gas key claims the accounts alone, the largest
// first, and an external wallet is given the largest account's claim alone with
// the others named still claimable. No account is in two transactions.

import (
	"context"
	"encoding/json"
	"math/big"
	"math/bits"
	"reflect"
	"strings"
	"testing"

	"github.com/urnetwork/sdk/sn/evm"
)

// the wallet that pays for an external claim, as a page names it
const testPayer = "0x00000000000000000000000000000000000000A1"

// A claim alone, at the fake's 11 gwei (1.1 × its 10 gwei gas price), and the
// least a claim alone uses once mined.
const (
	testClaimAloneGas  = int64(3_142_841)
	testClaimAloneUsed = int64(235_922)
	testWeiPerGas      = int64(11_000_000_000)
)

// The gas key holds enough for one claim alone (3,142,841 gas at 11 gwei) but
// not for the batch (about 3.5M gas): it claims capture the flag's 0.048 α, the
// largest, alone first, though the pool is first in the ledger. The chain then
// charges the gas that claim used, and what is left pays for no second claim:
// the pool's account is reported needs_gas with nothing sent for it, and stays
// claimable.
func TestSnLedgerClaimLargestAloneWhenTheGasKeyCannotPayTheBatch(t *testing.T) {
	f, _ := newTestV2BatchClaim(t)
	f.chain.chargeGas = true
	balance := int64(36_000_000_000_000_000)
	f.chain.balance = big.NewInt(balance)
	events := f.claim(t)
	sent := f.chain.sentTxs()
	if len(sent) != 1 || sent[0].to != f.chain.vault2 || len(events) != 3 {
		t.Fatalf("events %+v, %d sent", events, len(sent))
	}
	left := big.NewInt(balance - testClaimAloneUsed*testWeiPerGas)
	want := []testLedgerClaimEvent{
		{kind: "sent", noId: testCtfNoId, index: 1, txHash: events[0].txHash},
		{kind: "confirmed", noId: testCtfNoId, index: 1, txHash: events[0].txHash, amount: testCtfCumulative},
		{kind: "failed", noId: 1, index: 2, message: snNeedsGasMessage(big.NewInt(testClaimAloneGas*testWeiPerGas), left, snGasKeyInfo(f.key))},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events %+v, want %+v", events, want)
	}
	claim, err := decodeTestLedgerClaim(sent[0].data)
	if err != nil || claim.noId != testCtfNoId || sent[0].gas != uint64(testClaimAloneGas) {
		t.Fatalf("sent %+v, %v", sent[0], err)
	}
	if f.chain.paid(testCtfNoId, 1) != testCtfCumulative {
		t.Fatal("vault2 did not pay the capture the flag account")
	}
	f.requireClaimable(t)

	// with enough left after the first claim, the second is claimed too, in its
	// own transaction
	f, _ = newTestV2BatchClaim(t)
	f.chain.chargeGas = true
	f.chain.balance = big.NewInt(37_500_000_000_000_000)
	requireClaimedAlone(t, f, f.claim(t))
}

// testPayerResult builds the transactions for the payer with balance wei.
func testPayerResult(t *testing.T, f *testV2Claim, balance int64) *SnLedgerClaimTransactionsResult {
	t.Helper()
	f.chain.balance = big.NewInt(balance)
	sent := len(f.chain.sentTxs())
	result, err := SnLedgerClaimTransactionsForPayer(f.settings, f.aliceSs58, testPayer)
	if err != nil {
		t.Fatal(err)
	}
	if result.PayerAddress != strings.ToLower(testPayer) || result.PayerBalanceWei != big.NewInt(balance).String() || result.FeePerGasWei != "11000000000" {
		t.Fatalf("payer %+v", result)
	}
	if len(f.chain.sentTxs()) != sent {
		t.Fatal("building the transactions sent one")
	}
	return result
}

// testCost is gas at the fake's 11 gwei, decimal wei.
func testCost(gas int64) string {
	return new(big.Int).Mul(big.NewInt(gas), big.NewInt(testWeiPerGas)).String()
}

// An external wallet that pays for the batch is given it, as
// SnLedgerClaimTransactionsFor builds it. One that can pay for one claim alone
// (2.6M gas) but not the batch is given capture the flag's claim alone, the
// largest, and the pool's account is still claimable: no transaction claims it.
// Its outcome after the claim is mined reads confirmed, and the pool is unpaid.
func TestSnLedgerClaimTransactionsForPayerClaimsTheLargestAlone(t *testing.T) {
	f, _ := newTestV2BatchClaim(t)
	full := testPayerResult(t, f, 1_000_000_000_000_000_000)
	batches, err := SnLedgerClaimTransactionsFor(f.settings, f.aliceSs58)
	if err != nil || full.Transactions.Len() != 1 || !reflect.DeepEqual(full.Transactions.getAll(), batches.getAll()) || !full.Transactions.Get(0).Batch {
		t.Fatalf("a payer of the batch %+v, want %+v, %v", full.Transactions, batches, err)
	}
	batchCost := testCost(full.Transactions.Get(0).Gas)
	if full.StillClaimable.Len() != 0 || full.NeedsGas || full.MaxCostWei != batchCost || full.AllCostWei != batchCost {
		t.Fatalf("a payer of the batch %+v", full)
	}

	// 0.036 TAO: a claim alone at 2.6M gas is 0.0286 TAO, the batch about 0.0385
	f, _ = newTestV2BatchClaim(t)
	result := testPayerResult(t, f, 36_000_000_000_000_000)
	if result.Transactions.Len() != 1 || result.NeedsGas || result.MaxCostWei != testCost(2_600_000) || result.AllCostWei != batchCost {
		t.Fatalf("a payer of one claim %+v", result)
	}
	tx := result.Transactions.Get(0)
	wantItems := testClaimItems(&SnLedgerClaimItem{NoId: testCtfNoId, Index: 1, RootIndex: 0, Kind: SnLedgerPoolKindCtf, Label: "Capture the flag", AmountRao: testCtfCumulative})
	if tx.Batch || tx.To != testVault2 || tx.Vault != testVault2 || tx.NoId != testCtfNoId || tx.Index != 1 || tx.Gas != 2_600_000 || tx.AmountRao != testCtfCumulative ||
		!reflect.DeepEqual(tx.Items, wantItems) {
		t.Fatalf("the largest alone %+v", tx)
	}
	wantStill := []*SnLedgerClaimOutcome{{
		NoId:      1,
		Index:     2,
		Status:    SnLedgerClaimOutcomeStillClaimable,
		AmountRao: testV2Cumulative - testV2Paid,
		Message:   SnErrorCodeNeedsGas + ": claiming every account together needs up to " + snFormatTao(testBigWei(t, batchCost)) + " of gas and the wallet " + strings.ToLower(testPayer) + " holds 0.036000 TAO, so only the largest account is claimed now; claim again for this one",
	}}
	if !reflect.DeepEqual(result.StillClaimable.getAll(), wantStill) {
		t.Fatalf("still claimable %+v, want %+v", result.StillClaimable.getAll()[0], wantStill[0])
	}
	raw, _ := json.Marshal(result)
	if !strings.Contains(string(raw), `"still_claimable":[{"no_id":1,"index":2,"status":"still_claimable","amount_rao":23456789,"message":"needs_gas: `) ||
		!strings.Contains(string(raw), `"payer_address":"`+strings.ToLower(testPayer)+`","payer_balance_wei":"36000000000000000","fee_per_gas_wei":"11000000000","max_cost_wei":"28600000000000000","all_cost_wei":"`+batchCost+`","needs_gas":false}`) {
		t.Fatalf("json %s", raw)
	}

	// the wallet sends it and its receipt reads the account confirmed; the pool
	// is unpaid, still claimable
	data, _ := evm.ParseHexBytes(tx.Data)
	signed, _, err := (&evm.LegacyTx{Nonce: 0, GasPrice: big.NewInt(testWeiPerGas), Gas: uint64(tx.Gas), To: f.chain.vault2, Value: big.NewInt(0), Data: data}).Signed(big.NewInt(SnChainIdTestnet), f.key)
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
	if err != nil || !reflect.DeepEqual(outcomes.getAll(), []*SnLedgerClaimOutcome{{NoId: testCtfNoId, Index: 1, Status: SnLedgerClaimOutcomeConfirmed, AmountRao: testCtfCumulative}}) {
		t.Fatalf("outcomes %+v, %v", outcomes, err)
	}
	f.requireClaimable(t)

	// the next claim, with the pool's account alone left, is its claim alone
	result = testPayerResult(t, f, 36_000_000_000_000_000)
	if result.Transactions.Len() != 1 || result.Transactions.Get(0).NoId != 1 || result.StillClaimable.Len() != 0 || result.NeedsGas {
		t.Fatalf("the pool after %+v", result)
	}
}

func testBigWei(t *testing.T, decimal string) *big.Int {
	t.Helper()
	wei, ok := new(big.Int).SetString(decimal, 10)
	if !ok {
		t.Fatalf("wei %q", decimal)
	}
	return wei
}

// A wallet that cannot pay even for one claim alone is given the largest
// account's claim alone with NeedsGas, the others still claimable; with one
// account, its claim with NeedsGas. Without a batcher the claims alone are
// summed, so a wallet that cannot pay for both is given the largest alone.
func TestSnLedgerClaimTransactionsForPayerNeedsGas(t *testing.T) {
	f, _ := newTestV2BatchClaim(t)
	result := testPayerResult(t, f, 10_000_000_000_000_000)
	if result.Transactions.Len() != 1 || result.Transactions.Get(0).NoId != testCtfNoId || !result.NeedsGas || result.MaxCostWei != testCost(2_600_000) ||
		result.StillClaimable.Len() != 1 || result.StillClaimable.Get(0).NoId != 1 || result.StillClaimable.Get(0).Status != SnLedgerClaimOutcomeStillClaimable {
		t.Fatalf("a payer of no claim %+v", result)
	}

	f = newTestV2Claim(t)
	f.settings.ClaimBatcherAddress = testBatcher
	result = testPayerResult(t, f, 10_000_000_000_000_000)
	if result.Transactions.Len() != 1 || result.Transactions.Get(0).NoId != 1 || !result.NeedsGas || result.StillClaimable.Len() != 0 ||
		result.MaxCostWei != testCost(2_600_000) || result.AllCostWei != testCost(2_600_000) {
		t.Fatalf("one account %+v", result)
	}

	for _, c := range []struct {
		name    string
		balance int64
		alone   bool
	}{
		{"both", 2*2_600_000*testWeiPerGas + 1, false},
		{"one", 2*2_600_000*testWeiPerGas - 1, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, _ := newTestV2BatchClaim(t)
			f.settings.ClaimBatcherAddress = ""
			result := testPayerResult(t, f, c.balance)
			switch {
			case !c.alone && (result.Transactions.Len() != 2 || result.StillClaimable.Len() != 0 || result.Transactions.Get(0).NoId != 1 || result.Transactions.Get(1).NoId != testCtfNoId):
				t.Fatalf("claims alone %+v", result.Transactions)
			case c.alone && (result.Transactions.Len() != 1 || result.Transactions.Get(0).NoId != testCtfNoId || result.StillClaimable.Len() != 1 || result.NeedsGas):
				t.Fatalf("the largest alone %+v", result)
			case result.AllCostWei != testCost(2*2_600_000):
				t.Fatalf("all cost %s", result.AllCostWei)
			}
		})
	}
}

// The check prices gas at the higher of twice the base fee and the tip, what an
// EIP-1559 wallet offers by default, and 1.1 × eth_gasPrice. A payer that is not
// an EVM address is refused before anything is read, and a balance that cannot
// be read refuses the build rather than guessing.
func TestSnLedgerClaimTransactionsForPayerPricesAndRefusals(t *testing.T) {
	f, _ := newTestV2BatchClaim(t)
	f.chain.baseFee = big.NewInt(20_000_000_000)
	f.chain.balance = big.NewInt(1_000_000_000_000_000_000)
	result, err := SnLedgerClaimTransactionsForPayer(f.settings, f.aliceSs58, testPayer)
	if err != nil || result.FeePerGasWei != "42000000000" || result.MaxCostWei != new(big.Int).Mul(big.NewInt(result.Transactions.Get(0).Gas), big.NewInt(42_000_000_000)).String() {
		t.Fatalf("eip-1559 %+v, %v", result, err)
	}
	// a base fee under the gas price: 1.1 × the gas price
	f.chain.baseFee = big.NewInt(1_000_000_000)
	f.chain.tip = big.NewInt(1)
	if result, err = SnLedgerClaimTransactionsForPayer(f.settings, f.aliceSs58, testPayer); err != nil || result.FeePerGasWei != "11000000000" {
		t.Fatalf("legacy price %+v, %v", result, err)
	}

	for _, payer := range []string{"", "0x12", "a1" + strings.Repeat("0", 38), "0x" + strings.Repeat("g", 40)} {
		f, _ := newTestV2BatchClaim(t)
		if _, err := SnLedgerClaimTransactionsForPayer(f.settings, f.aliceSs58, payer); err == nil || !strings.HasPrefix(err.Error(), SnErrorCodeInvalidAddress+": ") {
			t.Fatalf("payer %q: %v", payer, err)
		}
		if requests := f.ledger.takeRequests(); len(requests) != 0 || len(f.chain.methods()) != 0 {
			t.Fatalf("payer %q read %q and %q", payer, requests, f.chain.methods())
		}
	}

	// a balance or a gas price that cannot be read refuses the build
	for _, method := range []string{"eth_getBalance", "eth_gasPrice"} {
		f, _ = newTestV2BatchClaim(t)
		f.chain.failing[method] = &evm.RpcError{Code: -32000, Message: "down"}
		if _, err := SnLedgerClaimTransactionsForPayer(f.settings, f.aliceSs58, testPayer); err == nil || err.Error() != SnErrorCodeClaimFailed+": down" {
			t.Fatalf("no %s: %v", method, err)
		}
	}
	// a block that cannot be read prices legacy only, as the device signs, and
	// leaves the batch unsized: the claims alone
	f, _ = newTestV2BatchClaim(t)
	f.chain.baseFee = big.NewInt(20_000_000_000)
	f.chain.failing["eth_getBlockByNumber"] = &evm.RpcError{Code: -32000, Message: "down"}
	if result, err = SnLedgerClaimTransactionsForPayer(f.settings, f.aliceSs58, testPayer); err != nil || result.FeePerGasWei != "11000000000" || result.Transactions.Len() != 2 {
		t.Fatalf("no block %+v, %v", result, err)
	}

	// nothing to claim: as SnLedgerClaimTransactionsFor
	f, _ = newTestV2BatchClaim(t)
	f.answer(t)
	if _, err := SnLedgerClaimTransactionsForPayer(f.settings, f.aliceSs58, testPayer); err == nil || err.Error() != snLedgerNothingToClaim {
		t.Fatalf("nothing to claim: %v", err)
	}
}

// newTestV2ThreeClaim is alice with three accounts, in the ledger's order pool
// 1's 0.023 α, pool 2's 0.03 α and capture the flag's 0.048 α, and the batcher
// in the settings. The largest first: capture the flag, pool 2, pool 1, so the
// two largest are not the ledger's order.
func newTestV2ThreeClaim(t *testing.T) *testV2Claim {
	t.Helper()
	f := newTestV2Claim(t)
	f.settings.ClaimBatcherAddress = testBatcher
	ctf, _ := f.addCtfPool(t)
	root, total, proofs := buildTestLedgerTree([]testLedgerLeaf{{f.alice, testThreeAmounts[2]}})
	f.chain.roots[2] = []fakeVault2Root{{root, total, 1}}
	f.answer(t, f.pool1Entry(), testLedgerClaimEntry(testVault2, 2, 0, 0, testThreeAmounts[2], proofs[0], 0), ctf)
	return f
}

// the three accounts' amounts by pool
var testThreeAmounts = map[int64]int64{1: testV2Cumulative - testV2Paid, 2: 30_000_000, testCtfNoId: testCtfCumulative}

// testTxPools is the pools of the accounts each transaction claims, as its
// items name them, after checking each transaction's calldata claims exactly
// those accounts.
func testTxPools(t *testing.T, f *testV2Claim, txs *SnLedgerUnsignedTxList) [][]int64 {
	t.Helper()
	var pools [][]int64
	for _, tx := range txs.getAll() {
		data, err := evm.ParseHexBytes(tx.Data)
		if err != nil {
			t.Fatal(err)
		}
		claims := [][]byte{data}
		if tx.Batch {
			if _, claims, _, err = decodeTestClaimMany(data); err != nil {
				t.Fatal(err)
			}
		}
		var txPools []int64
		amount := int64(0)
		for i, item := range tx.Items.getAll() {
			claim, err := decodeTestLedgerClaim(claims[i])
			if err != nil || claim.noId != item.NoId || claim.index != item.Index || item.AmountRao != testThreeAmounts[item.NoId] {
				t.Fatalf("item %d of %+v: %+v, %v", i, tx, claim, err)
			}
			txPools = append(txPools, item.NoId)
			amount += item.AmountRao
		}
		if len(claims) != tx.Items.Len() || tx.AmountRao != amount || tx.Batch != (2 <= len(txPools)) {
			t.Fatalf("transaction %+v", tx)
		}
		pools = append(pools, txPools)
	}
	return pools
}

// With three accounts, an external wallet that cannot pay for the batch of all
// three is given the batch of the two largest when it can pay for that (capture
// the flag's 0.048 α and pool 2's 0.03 α, though pool 1 is first in the
// ledger; the batch's items in the ledger's order), else the largest account's
// claim alone, and the others are still claimable, in the ledger's order; one
// that cannot pay even for that is given it with NeedsGas, and the others'
// message says nothing is claimed for them. Without a batcher, the claims alone
// of the most accounts it can pay for, the largest first.
func TestSnLedgerClaimTransactionsForPayerTheMostItCanPayFor(t *testing.T) {
	const (
		threeAlone = 3 * 2_600_000 * testWeiPerGas
		twoAlone   = 2 * 2_600_000 * testWeiPerGas
		oneAlone   = 2_600_000 * testWeiPerGas
	)
	for _, c := range []struct {
		name     string
		batcher  bool
		balance  int64
		txs      [][]int64
		still    []int64
		needsGas bool
		why      string
	}{
		// the batch of three is about 3.73M gas, of two about 3.5M, at 11 gwei
		{"all three", true, 1_000_000_000_000_000_000, [][]int64{{1, 2, testCtfNoId}}, nil, false, ""},
		{"two", true, 40_000_000_000_000_000, [][]int64{{2, testCtfNoId}}, []int64{1}, false, "so only the 2 largest accounts are claimed now; claim again for this one"},
		{"one", true, 30_000_000_000_000_000, [][]int64{{testCtfNoId}}, []int64{1, 2}, false, "so only the largest account is claimed now; claim again for this one"},
		{"none", true, 10_000_000_000_000_000, [][]int64{{testCtfNoId}}, []int64{1, 2}, true,
			"which is not enough even for the largest account's claim alone (up to 0.028600 TAO), so this account is not claimed now; add TAO to the wallet and claim again"},
		// every claim alone, in the ledger's order, as SnLedgerClaimTransactionsFor
		// builds them; fewer, the largest first
		{"all three alone", false, threeAlone, [][]int64{{1}, {2}, {testCtfNoId}}, nil, false, ""},
		{"two alone", false, threeAlone - 1, [][]int64{{testCtfNoId}, {2}}, []int64{1}, false, "so only the 2 largest accounts are claimed now; claim again for this one"},
		{"one alone", false, twoAlone - 1, [][]int64{{testCtfNoId}}, []int64{1, 2}, false, "so only the largest account is claimed now; claim again for this one"},
		{"none alone", false, oneAlone - 1, [][]int64{{testCtfNoId}}, []int64{1, 2}, true,
			"which is not enough even for the largest account's claim alone (up to 0.028600 TAO), so this account is not claimed now; add TAO to the wallet and claim again"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newTestV2ThreeClaim(t)
			if !c.batcher {
				f.settings.ClaimBatcherAddress = ""
			}
			all, err := SnLedgerClaimTransactionsFor(f.settings, f.aliceSs58)
			if err != nil {
				t.Fatal(err)
			}
			result := testPayerResult(t, f, c.balance)
			if pools := testTxPools(t, f, result.Transactions); !reflect.DeepEqual(pools, c.txs) {
				t.Fatalf("transactions of %v, want %v", pools, c.txs)
			}
			if len(c.still) == 0 && !reflect.DeepEqual(result.Transactions.getAll(), all.getAll()) {
				t.Fatalf("a payer of every account %+v, want %+v", result.Transactions, all)
			}
			for _, tx := range result.Transactions.getAll() {
				// the least at which the fake's batcher attempts every item: its
				// overhead, each item but the last, then the last's cap, 1/63 of it
				// and the bookkeeping; sized within the tolerance, with a fifth more
				least := uint64(60_000+195_000*(tx.Items.Len()-1)) + 2_600_000 + 2_600_000/63 + 20_000
				switch {
				case tx.Batch && (uint64(tx.Gas) < least+least/5 || (least+snLedgerBatchTolerance)*6/5 < uint64(tx.Gas)):
					t.Fatalf("a batch of %d with gas %d, want the least %d with a fifth more", tx.Items.Len(), tx.Gas, least)
				case !tx.Batch && tx.Gas != 2_600_000:
					t.Fatalf("a claim alone with gas %d", tx.Gas)
				}
			}
			maxCost := snLedgerTxsCost(result.Transactions.getAll(), big.NewInt(testWeiPerGas))
			allCost := snLedgerTxsCost(all.getAll(), big.NewInt(testWeiPerGas))
			if result.NeedsGas != c.needsGas || result.MaxCostWei != maxCost.String() || result.AllCostWei != allCost.String() || (maxCost.Cmp(big.NewInt(c.balance)) <= 0) == c.needsGas {
				t.Fatalf("costs %+v", result)
			}
			var still []int64
			for _, outcome := range result.StillClaimable.getAll() {
				still = append(still, outcome.NoId)
				wantMessage := SnErrorCodeNeedsGas + ": claiming every account together needs up to " + snFormatTao(allCost) + " of gas and the wallet " + strings.ToLower(testPayer) + " holds " + snFormatTao(big.NewInt(c.balance)) + ", " + c.why
				if outcome.Status != SnLedgerClaimOutcomeStillClaimable || outcome.AmountRao != testThreeAmounts[outcome.NoId] || outcome.Message != wantMessage {
					t.Fatalf("still claimable %+v, want the message %q", outcome, wantMessage)
				}
			}
			if !reflect.DeepEqual(still, c.still) {
				t.Fatalf("still claimable %v, want %v", still, c.still)
			}
		})
	}
}

// newTestV2ThreeGasKeyClaim is the three accounts claimed with the device's gas
// key holding balance, the chain charging the gas each transaction uses.
func newTestV2ThreeGasKeyClaim(t *testing.T, balance int64) *testV2Claim {
	t.Helper()
	f := newTestV2ThreeClaim(t)
	f.chain.chargeGas = true
	f.chain.balance = big.NewInt(balance)
	return f
}

// The device's gas key with three accounts: the batch of all three when it can
// pay for it; else the batch of the two largest when it can pay for that
// (capture the flag and pool 2, though pool 1 is first in the ledger), then
// pool 1 alone, needs_gas when what is left after the batch cannot pay for it;
// else the accounts alone, the largest first, each it cannot pay for needs_gas
// with nothing sent for it, still claimable.
func TestSnLedgerClaimTheMostTheGasKeyCanPayFor(t *testing.T) {
	gasKey := snGasKeyInfo(newTestV2Claim(t).key)
	aloneCost := big.NewInt(testClaimAloneGas * testWeiPerGas)
	needsGas := func(balance int64) string {
		return snNeedsGasMessage(aloneCost, big.NewInt(balance), gasKey)
	}

	// all three: one batch, in the ledger's order
	f := newTestV2ThreeGasKeyClaim(t, 1_000_000_000_000_000_000)
	events := f.claim(t)
	sent := f.chain.sentTxs()
	if len(sent) != 1 || sent[0].to != testBatcherAddress || len(events) != 6 {
		t.Fatalf("all three: events %+v, %d sent", events, len(sent))
	}
	hash := events[0].txHash
	want := []testLedgerClaimEvent{
		{kind: "sent", noId: 1, index: 2, txHash: hash},
		{kind: "sent", noId: 2, index: 0, txHash: hash},
		{kind: "sent", noId: testCtfNoId, index: 1, txHash: hash},
		{kind: "confirmed", noId: 1, index: 2, txHash: hash, amount: testThreeAmounts[1]},
		{kind: "confirmed", noId: 2, index: 0, txHash: hash, amount: testThreeAmounts[2]},
		{kind: "confirmed", noId: testCtfNoId, index: 1, txHash: hash, amount: testThreeAmounts[testCtfNoId]},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("all three: events %+v, want %+v", events, want)
	}

	// two: 0.039 TAO pays for the batch of two (about 3.5M gas at 11 gwei) but
	// not of three (about 3.73M); the batch uses 450,000 gas, and the 0.03405
	// TAO left does not pay for pool 1's claim alone (3,142,841 gas)
	balance := int64(39_000_000_000_000_000)
	f = newTestV2ThreeGasKeyClaim(t, balance)
	events = f.claim(t)
	sent = f.chain.sentTxs()
	if len(sent) != 1 || sent[0].to != testBatcherAddress || len(events) != 5 {
		t.Fatalf("two: events %+v, %d sent", events, len(sent))
	}
	_, claims, _, err := decodeTestClaimMany(sent[0].data)
	if err != nil || len(claims) != 2 {
		t.Fatalf("two: the batch %v", err)
	}
	for i, noId := range []int64{2, testCtfNoId} {
		if claim, err := decodeTestLedgerClaim(claims[i]); err != nil || claim.noId != noId {
			t.Fatalf("two: the batch's claim %d %+v, %v", i, claim, err)
		}
	}
	if gasCost := new(big.Int).Mul(new(big.Int).SetUint64(sent[0].gas), big.NewInt(testWeiPerGas)); big.NewInt(balance).Cmp(gasCost) < 0 {
		t.Fatalf("two: a batch of %d gas the gas key cannot pay for", sent[0].gas)
	}
	hash = events[0].txHash
	want = []testLedgerClaimEvent{
		{kind: "sent", noId: 2, index: 0, txHash: hash},
		{kind: "sent", noId: testCtfNoId, index: 1, txHash: hash},
		{kind: "confirmed", noId: 2, index: 0, txHash: hash, amount: testThreeAmounts[2]},
		{kind: "confirmed", noId: testCtfNoId, index: 1, txHash: hash, amount: testThreeAmounts[testCtfNoId]},
		{kind: "failed", noId: 1, index: 2, message: needsGas(balance - (60_000+2*195_000)*testWeiPerGas)},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("two: events %+v, want %+v", events, want)
	}
	f.requireClaimable(t)

	// one: 0.036 TAO pays for no batch, but for capture the flag's claim alone;
	// what is left then pays for no other claim
	balance = 36_000_000_000_000_000
	f = newTestV2ThreeGasKeyClaim(t, balance)
	events = f.claim(t)
	sent = f.chain.sentTxs()
	if len(sent) != 1 || sent[0].to != f.chain.vault2 || len(events) != 4 {
		t.Fatalf("one: events %+v, %d sent", events, len(sent))
	}
	left := balance - testClaimAloneUsed*testWeiPerGas
	want = []testLedgerClaimEvent{
		{kind: "sent", noId: testCtfNoId, index: 1, txHash: events[0].txHash},
		{kind: "confirmed", noId: testCtfNoId, index: 1, txHash: events[0].txHash, amount: testThreeAmounts[testCtfNoId]},
		{kind: "failed", noId: 2, index: 0, message: needsGas(left)},
		{kind: "failed", noId: 1, index: 2, message: needsGas(left)},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("one: events %+v, want %+v", events, want)
	}
	f.requireClaimable(t)

	// none: nothing sent, each account needs_gas, the largest first
	balance = 10_000_000_000_000_000
	f = newTestV2ThreeGasKeyClaim(t, balance)
	events = f.claim(t)
	want = []testLedgerClaimEvent{
		{kind: "failed", noId: testCtfNoId, index: 1, message: needsGas(balance)},
		{kind: "failed", noId: 2, index: 0, message: needsGas(balance)},
		{kind: "failed", noId: 1, index: 2, message: needsGas(balance)},
	}
	if !reflect.DeepEqual(events, want) || len(f.chain.sentTxs()) != 0 {
		t.Fatalf("none: events %+v, want %+v, %d sent", events, want, len(f.chain.sentTxs()))
	}
	f.requireClaimable(t)
}

// snLedgerMostThatFit finds the most accounts that fit by halving, never trying
// one account or all of them, and is 1 when no two fit.
func TestSnLedgerMostThatFit(t *testing.T) {
	for count := 1; count <= 40; count++ {
		for most := 1; most < count || most == 1; most++ {
			var tried []int
			n := snLedgerMostThatFit(count, func(n int) bool {
				tried = append(tried, n)
				return n <= most
			})
			if n != most {
				t.Fatalf("%d accounts, %d fit: %d", count, most, n)
			}
			for _, n := range tried {
				if n < 2 || count <= n {
					t.Fatalf("%d accounts tried %d", count, n)
				}
			}
			if limit := bits.Len(uint(count)); limit < len(tried) {
				t.Fatalf("%d accounts tried %d times: %v", count, len(tried), tried)
			}
		}
	}
}
