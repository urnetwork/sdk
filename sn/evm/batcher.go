package evm

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/urnetwork/sdk/sn/merkle"
)

// The STClaimBatcher call and events a batched settlement v2 claim uses (the
// sn repository's evm/src/STClaimBatcher.sol, VERSION2-FINAL.md §10.2): one
// transaction calls vault2's claim for each item with a gas cap, inside
// try/catch, and reports each item's outcome. vault2 pays each leaf's coldkey
// whoever calls, so the batcher can redirect nothing. Pinned by test to the
// selectors and topics of the batcher's ABI.
var (
	claimManySelector = selector("claimMany(address,(uint256,uint256,(uint256,bytes32,uint256),(bytes32,uint256)[])[],uint256)")
	// ClaimPaid(item, amount): item `item` paid `amount` to its leaf's coldkey
	ClaimPaidTopic = Keccak256([]byte("ClaimPaid(uint256,uint256)"))
	// ClaimRefused(item, selector): item `item` reverted with revert data
	// starting `selector`, zero for a call that ran out of its gas, as one
	// does when the runtime refuses a precompile call
	ClaimRefusedTopic = Keccak256([]byte("ClaimRefused(uint256,bytes4)"))
	// ClaimsNotAttempted(item): items from `item` to the last were not attempted
	ClaimsNotAttemptedTopic = Keccak256([]byte("ClaimsNotAttempted(uint256)"))
)

// ClaimBatcherRuntimeHash is keccak256 of the release claim batcher's runtime
// code (sn sim-testnet/contracts_gen.go ClaimBatcherRuntimeBytecodeHash, and
// the code at the mainnet batcher). A batcher with any other code would refuse
// or misreport every batch.
var ClaimBatcherRuntimeHash = [32]byte{
	0x29, 0x64, 0xc2, 0x88, 0x2e, 0x0e, 0x4f, 0x85, 0xc2, 0x29, 0x55, 0x63, 0xda, 0x92, 0xea, 0xd2,
	0xfa, 0x86, 0x69, 0xd0, 0x04, 0x50, 0x59, 0xee, 0xfb, 0xef, 0xa1, 0x87, 0xff, 0xd2, 0x2c, 0x26,
}

// ClaimBatcherBookkeepingGas is the batcher's BOOKKEEPING_GAS: what an item
// needs beyond its cap for the batcher's own work. An item is attempted only
// while the gas left covers its cap, the 1/63 of it EIP-150 keeps, and this.
const ClaimBatcherBookkeepingGas uint64 = 20_000

// LedgerClaim is the arguments of one vault2 claim(noId, rootIndex, (index,
// coldkey, cumulative), proof): one item of a batch.
type LedgerClaim struct {
	NoId       uint64
	RootIndex  uint64
	Index      uint64
	Coldkey    [32]byte
	Cumulative *big.Int
	Proof      []merkle.LedgerNode
}

// PackClaimMany encodes STClaimBatcher.claimMany(vault, claims, gasPerClaim).
// Each item is the dynamic Claim tuple, whose encoding is a vault2 claim's
// arguments.
func PackClaimMany(vault [20]byte, claims []*LedgerClaim, gasPerClaim uint64) ([]byte, error) {
	tuples := make([][]byte, len(claims))
	for i, claim := range claims {
		data, err := PackLedgerClaim(claim.NoId, claim.RootIndex, claim.Index, claim.Coldkey, claim.Cumulative, claim.Proof)
		if err != nil {
			return nil, fmt.Errorf("claim %d: %w", i, err)
		}
		tuples[i] = data[4:]
	}
	out := append([]byte(nil), claimManySelector[:]...)
	var vaultWord [32]byte
	copy(vaultWord[12:], vault[:])
	// the head: the vault, the offset of the claims array after the three
	// head words, and the gas cap
	offset, gas := uint64Word(3*32), uint64Word(gasPerClaim)
	out = append(out, vaultWord[:]...)
	out = append(out, offset[:]...)
	out = append(out, gas[:]...)
	length := uint64Word(uint64(len(claims)))
	out = append(out, length[:]...)
	// each element's offset counts from the start of the element offsets
	next := uint64(32 * len(claims))
	for _, tuple := range tuples {
		word := uint64Word(next)
		out = append(out, word[:]...)
		next += uint64(len(tuple))
	}
	for _, tuple := range tuples {
		out = append(out, tuple...)
	}
	return out, nil
}

// DecodeClaimManyAttempted decodes claimMany's return, the number of items
// attempted.
func DecodeClaimManyAttempted(ret []byte) (uint64, error) {
	if len(ret) != 32 {
		return 0, fmt.Errorf("claimMany returned %d bytes", len(ret))
	}
	attempted := new(big.Int).SetBytes(ret)
	if !attempted.IsUint64() {
		return 0, errors.New("claimMany's attempted count overflows uint64")
	}
	return attempted.Uint64(), nil
}

// The outcomes a batcher event reports.
const (
	ClaimBatchPaid         = "paid"
	ClaimBatchRefused      = "refused"
	ClaimBatchNotAttempted = "not_attempted"
)

// ClaimBatchEvent is a decoded batcher event: Item paid Amount, Item refused
// with revert data starting Selector, or the items from Item on not attempted.
type ClaimBatchEvent struct {
	Outcome  string
	Item     uint64
	Amount   *big.Int
	Selector [4]byte
}

// DecodeClaimBatchEvent decodes a batcher log; false for any other log. The
// caller checks the log's address.
func DecodeClaimBatchEvent(log *Log) (*ClaimBatchEvent, bool) {
	if len(log.Topics) != 1 || len(log.Data) < 32 {
		return nil, false
	}
	item := new(big.Int).SetBytes(log.Data[0:32])
	if !item.IsUint64() {
		return nil, false
	}
	event := &ClaimBatchEvent{Item: item.Uint64()}
	switch {
	case log.Topics[0] == ClaimPaidTopic && len(log.Data) == 64:
		event.Outcome = ClaimBatchPaid
		event.Amount = new(big.Int).SetBytes(log.Data[32:64])
	case log.Topics[0] == ClaimRefusedTopic && len(log.Data) == 64:
		// bytes4 is left-aligned in its word
		event.Outcome = ClaimBatchRefused
		copy(event.Selector[:], log.Data[32:36])
	case log.Topics[0] == ClaimsNotAttemptedTopic && len(log.Data) == 32:
		event.Outcome = ClaimBatchNotAttempted
	default:
		return nil, false
	}
	return event, true
}

// ClaimBatchOutcomes reads the outcome of each of a batch's items from the
// batcher's events in its successful receipt: one paid or refused per
// attempted item, in order, then one not attempted for the rest. Logs of
// other addresses are skipped; any other batcher event refuses the receipt.
func ClaimBatchOutcomes(receipt *Receipt, batcher [20]byte, items int) ([]*ClaimBatchEvent, error) {
	outcomes := make([]*ClaimBatchEvent, 0, items)
	for _, log := range receipt.Logs {
		if log.Address != batcher {
			continue
		}
		event, ok := DecodeClaimBatchEvent(log)
		if !ok || len(outcomes) == items || event.Item != uint64(len(outcomes)) {
			return nil, fmt.Errorf("the batch receipt has a batcher event out of order for its %d items", items)
		}
		outcomes = append(outcomes, event)
		for event.Outcome == ClaimBatchNotAttempted && len(outcomes) < items {
			outcomes = append(outcomes, &ClaimBatchEvent{Outcome: ClaimBatchNotAttempted, Item: uint64(len(outcomes))})
		}
	}
	if len(outcomes) != items {
		return nil, fmt.Errorf("the batch receipt reports %d of its %d items", len(outcomes), items)
	}
	return outcomes, nil
}
