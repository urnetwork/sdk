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
