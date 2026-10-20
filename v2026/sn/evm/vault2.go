package evm

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/urnetwork/sdk/v2026/sn/merkle"
)

// The STSettlementVault2 calls a settlement v2 claim makes (the sn
// repository's evm/src/interfaces/ISTSettlementVault2.sol), its Claimed
// event, and the staking precompile transfer the vault pays with. Pinned by
// test to the selectors of the vault's ABI.
var (
	ledgerClaimSelector   = selector("claim(uint256,uint256,(uint256,bytes32,uint256),(bytes32,uint256)[])")
	ledgerRootsSelector   = selector("roots(uint256,uint256)")
	ledgerClaimedSelector = selector("claimed(uint256,uint256)")
	escrowHotkeySelector  = selector("escrowHotkey()")
	transferStakeSelector = selector("transferStake(bytes32,bytes32,uint256,uint256,uint256)")
)

// LedgerClaimedTopic is topic 0 of STSettlementVault2's Claimed(noId,
// rootIndex, index, coldkey, cumulative, amount, caller), with noId, index
// and coldkey indexed.
var LedgerClaimedTopic = Keccak256([]byte("Claimed(uint256,uint256,uint256,bytes32,uint256,uint256,address)"))

// StakingPrecompile is the Subtensor staking precompile (IStaking at 0x805).
var StakingPrecompile = [20]byte{18: 0x08, 19: 0x05}

// PackLedgerClaim encodes STSettlementVault2.claim(noId, rootIndex,
// (index, coldkey, cumulative), proof). vault2 takes pool ids below 2^64 and
// at most 2^29 accounts; amounts are uint256.
func PackLedgerClaim(noId, rootIndex, index uint64, coldkey [32]byte, cumulative *big.Int, proof []merkle.LedgerNode) ([]byte, error) {
	c, err := UintWord(cumulative)
	if err != nil {
		return nil, fmt.Errorf("cumulative: %w", err)
	}
	n, r, i := uint64Word(noId), uint64Word(rootIndex), uint64Word(index)
	out := make([]byte, 0, 4+32*(7+2*len(proof)))
	out = append(out, ledgerClaimSelector[:]...)
	out = append(out, n[:]...)
	out = append(out, r[:]...)
	// the account tuple is static, so it is inline
	out = append(out, i[:]...)
	out = append(out, coldkey[:]...)
	out = append(out, c[:]...)
	// offset of the dynamic proof array: after the six head words
	offset := uint64Word(6 * 32)
	out = append(out, offset[:]...)
	length := uint64Word(uint64(len(proof)))
	out = append(out, length[:]...)
	for k, node := range proof {
		s, err := UintWord(node.Sum)
		if err != nil {
			return nil, fmt.Errorf("proof %d sum: %w", k, err)
		}
		out = append(out, node.Hash[:]...)
		out = append(out, s[:]...)
	}
	return out, nil
}

// PackLedgerRoots encodes STSettlementVault2.roots(noId, rootIndex).
func PackLedgerRoots(noId, rootIndex uint64) []byte {
	return packTwoUint64s(ledgerRootsSelector, noId, rootIndex)
}

// PackLedgerClaimed encodes STSettlementVault2.claimed(noId, index).
func PackLedgerClaimed(noId, index uint64) []byte {
	return packTwoUint64s(ledgerClaimedSelector, noId, index)
}

func packTwoUint64s(sel [4]byte, a, b uint64) []byte {
	aw, bw := uint64Word(a), uint64Word(b)
	out := make([]byte, 0, 4+64)
	out = append(out, sel[:]...)
	out = append(out, aw[:]...)
	return append(out, bw[:]...)
}

// PackEscrowHotkey encodes STSettlementVault2.escrowHotkey().
func PackEscrowHotkey() []byte {
	return append([]byte(nil), escrowHotkeySelector[:]...)
}

// PackTransferStake encodes the staking precompile's
// transferStake(destinationColdkey, hotkey, netuid, netuid, amount), a
// transfer within one subnet.
func PackTransferStake(destinationColdkey [32]byte, hotkey [32]byte, netuid uint64, amount uint64) []byte {
	n, a := uint64Word(netuid), uint64Word(amount)
	out := make([]byte, 0, 4+5*32)
	out = append(out, transferStakeSelector[:]...)
	out = append(out, destinationColdkey[:]...)
	out = append(out, hotkey[:]...)
	out = append(out, n[:]...)
	out = append(out, n[:]...)
	out = append(out, a[:]...)
	return out
}

// LedgerRoot mirrors ISTSettlementVault2.Root, a finalized root.
type LedgerRoot struct {
	Root       [32]byte
	Total      *big.Int
	LeafCount  uint64
	Epoch      uint64
	LedgerHash [32]byte
}

// DecodeLedgerRoot decodes the static Root tuple returned by roots().
func DecodeLedgerRoot(ret []byte) (*LedgerRoot, error) {
	if len(ret) < 5*32 {
		return nil, fmt.Errorf("roots return too short: %d bytes", len(ret))
	}
	r := &LedgerRoot{Total: new(big.Int).SetBytes(ret[32:64])}
	copy(r.Root[:], ret[0:32])
	copy(r.LedgerHash[:], ret[128:160])
	leafCount := new(big.Int).SetBytes(ret[64:96])
	epoch := new(big.Int).SetBytes(ret[96:128])
	if !leafCount.IsUint64() || !epoch.IsUint64() {
		return nil, errors.New("root leaf count or epoch overflows uint64")
	}
	r.LeafCount = leafCount.Uint64()
	r.Epoch = epoch.Uint64()
	return r, nil
}

// LedgerClaimed is a decoded Claimed event.
type LedgerClaimed struct {
	NoId       *big.Int
	RootIndex  *big.Int
	Index      *big.Int
	Coldkey    [32]byte
	Cumulative *big.Int
	Amount     *big.Int
	Caller     [20]byte
}

// DecodeLedgerClaimed decodes a Claimed log; false for any other log.
func DecodeLedgerClaimed(log *Log) (*LedgerClaimed, bool) {
	if len(log.Topics) != 4 || log.Topics[0] != LedgerClaimedTopic || len(log.Data) != 4*32 {
		return nil, false
	}
	event := &LedgerClaimed{
		NoId:       new(big.Int).SetBytes(log.Topics[1][:]),
		RootIndex:  new(big.Int).SetBytes(log.Data[0:32]),
		Index:      new(big.Int).SetBytes(log.Topics[2][:]),
		Coldkey:    log.Topics[3],
		Cumulative: new(big.Int).SetBytes(log.Data[32:64]),
		Amount:     new(big.Int).SetBytes(log.Data[64:96]),
	}
	copy(event.Caller[:], log.Data[96+12:128])
	return event, true
}
