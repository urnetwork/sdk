package merkle

// The ledger sum tree `urnetwork-ledger-sum-tree-v1` (VERSION2-FINAL.md §6.5)
// commits a pool's settlement v2 ledger in STSettlementVault2: account i is
// the leaf (coldkey_i, cumulative_i) at position i, and every internal node
// commits to both children's hashes and stated sums. This is the SDK's port
// of the hashing and the claim proof fold of the sn repository's
// merkle/ledger_sum_tree.go, the Go twin of evm/src/LedgerTree.sol;
// testdata/ledger_v2_vectors.json is the sn vector file, copied unchanged.
//
//	leafHash(c, v)        = keccak256(0x00 ‖ c ‖ uint256(v))
//	nodeHash(hL,sL,hR,sR) = keccak256(0x01 ‖ hL ‖ uint256(sL) ‖ hR ‖ uint256(sR))
//	d = 0 if n == 1, else ceil(log2 n); a claim proof is d siblings, bottom-up.

import (
	"errors"
	"math/big"
)

const (
	// LedgerSumTreeScheme names the tree construction.
	LedgerSumTreeScheme = "urnetwork-ledger-sum-tree-v1"
	// MaxLedgerLeafCount is the largest account count a tree can hold.
	MaxLedgerLeafCount = 1 << 29
)

// ErrLedgerLeafCount is returned for a tree of zero or more than 2^29 accounts.
var ErrLedgerLeafCount = errors.New("merkle: ledger tree needs 1 to 2^29 accounts")

// LedgerNode is a tree node: its hash and its stated sum. A claim proof is a
// list of sibling nodes, for levels 0 … d−1.
type LedgerNode struct {
	Hash [32]byte
	Sum  *big.Int
}

// LedgerLeafHash returns keccak256(0x00 ‖ coldkey ‖ uint256(value)). It
// panics if value is nil, negative, or wider than 256 bits.
func LedgerLeafHash(coldkey [32]byte, value *big.Int) [32]byte {
	v := uint256Bytes(value)
	return keccak256([]byte{0x00}, coldkey[:], v[:])
}

// LedgerNodeHash returns keccak256(0x01 ‖ hL ‖ uint256(sL) ‖ hR ‖ uint256(sR)).
// It panics if a sum is nil, negative, or wider than 256 bits.
func LedgerNodeHash(leftHash [32]byte, leftSum *big.Int, rightHash [32]byte, rightSum *big.Int) [32]byte {
	ls, rs := uint256Bytes(leftSum), uint256Bytes(rightSum)
	return keccak256([]byte{0x01}, leftHash[:], ls[:], rightHash[:], rs[:])
}

// LedgerTreeDepth returns d for a tree of leafCount accounts.
func LedgerTreeDepth(leafCount uint64) (int, error) {
	if leafCount == 0 || leafCount > MaxLedgerLeafCount {
		return 0, ErrLedgerLeafCount
	}
	d := 0
	for uint64(1)<<d < leafCount {
		d++
	}
	return d, nil
}

// isUint256 reports whether v can be a uint256 word.
func isUint256(v *big.Int) bool {
	return v != nil && v.Sign() >= 0 && v.BitLen() <= 256
}

// VerifyLedgerProof reports whether proof folds (coldkey, value) at index into
// root and total, exactly as LedgerTree.verifyLeaf does: index below
// leafCount, exactly d siblings, and the fold reproducing both the root and
// the total. Every input may be untrusted. An invalid leaf count gives false
// where LedgerTree.verifyLeaf reverts InvalidLeafCount, a sum that overflows
// 256 bits gives false where it reverts on the overflow, and a value or sum
// that is not a uint256 gives false.
func VerifyLedgerProof(root [32]byte, total *big.Int, leafCount uint64, index uint64, coldkey [32]byte, value *big.Int, proof []LedgerNode) bool {
	d, err := LedgerTreeDepth(leafCount)
	if err != nil || index >= leafCount || len(proof) != d || !isUint256(total) || !isUint256(value) {
		return false
	}
	h := LedgerLeafHash(coldkey, value)
	s := new(big.Int).Set(value)
	for k := 0; k < d; k++ {
		sibling := &proof[k]
		if !isUint256(sibling.Sum) {
			return false
		}
		if (index>>k)&1 == 1 {
			h = LedgerNodeHash(sibling.Hash, sibling.Sum, h, s)
		} else {
			h = LedgerNodeHash(h, s, sibling.Hash, sibling.Sum)
		}
		if s.Add(s, sibling.Sum).BitLen() > 256 {
			return false
		}
	}
	return h == root && s.Cmp(total) == 0
}
