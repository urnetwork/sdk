// Package merkle implements the OpenZeppelin-compatible keccak256 Merkle
// tree shared by the UR subnet server (payout roots), the ops CLI, and the
// validator (effort roots). Roots built here are committed on-chain and the
// proofs are checked in Solidity with OpenZeppelin's MerkleProof.verify, so
// the scheme is byte-exact with OZ:
//
//   - Leaf hash: keccak256(bytes.concat(keccak256(payload))) — the OZ
//     StandardMerkleTree "double hash". A payload is the abi.encode of the
//     leaf tuple. Both UR leaf types are (bytes32, uint256) tuples, which
//     abi-encode to 64 bytes: the 32-byte key followed by the 32-byte
//     big-endian unsigned value. Payout leaves are (coldkey, shareBps);
//     effort leaves are (pathId, coverage).
//   - Internal node: keccak256(a ‖ b) with a and b in ascending
//     lexicographic byte order — the commutative "sorted pair" hash that
//     OZ MerkleProof.processProof folds during verification.
//
// # Canonical tree shape
//
// NewTree always builds one canonical shape so that independent
// implementations reconstruct bit-identical roots from the same leaf set:
//
//  1. Hash every leaf (a Leaf value is already the 32-byte leaf hash).
//  2. Sort the leaf hashes in ascending lexicographic byte order.
//  3. Build bottom-up, pairing adjacent nodes left to right. A trailing
//     unpaired node at any level is promoted unchanged to the next level.
//  4. The single remaining node is the root.
//
// Proofs produced from any tree shape verify under OZ MerkleProof.verify —
// on-chain verification is shape-blind, it only folds sorted pairs — but
// root determinism across implementations requires this exact
// construction. Note that OpenZeppelin's JS StandardMerkleTree lays odd
// levels out differently (a packed array where the trailing node pairs
// early instead of being promoted), so for some leaf counts (e.g. 5) it
// produces a different — equally verifiable — root from the same leaves.
// Every UR component must therefore build roots with this package's
// canonical shape (or a faithful port of it).
package merkle

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"
	"sort"

	"golang.org/x/crypto/sha3"
)

// Errors returned by tree construction and proof lookup.
var (
	// ErrEmptyTree is returned by NewTree when no leaves are supplied.
	ErrEmptyTree = errors.New("merkle: tree requires at least one leaf")
	// ErrDuplicateLeaf is returned by NewTree when two leaves hash
	// identically. The payout tree requires exactly one leaf per coldkey,
	// so duplicates always indicate a caller bug (and would make
	// Proof-by-leaf ambiguous).
	ErrDuplicateLeaf = errors.New("merkle: duplicate leaf")
	// ErrUnknownLeaf is returned by Proof when the leaf is not in the tree.
	ErrUnknownLeaf = errors.New("merkle: leaf not in tree")
	// ErrIndexOutOfRange is returned by ProofAt for an invalid leaf index.
	ErrIndexOutOfRange = errors.New("merkle: leaf index out of range")
)

// Leaf is a fully hashed Merkle leaf:
// keccak256(bytes.concat(keccak256(abi.encode(leaf tuple)))).
type Leaf [32]byte

// keccak256 hashes the concatenation of data with legacy Keccak-256 (the
// EVM's keccak256, not NIST SHA3-256).
func keccak256(data ...[]byte) [32]byte {
	h := sha3.NewLegacyKeccak256()
	for _, d := range data {
		h.Write(d)
	}
	var out [32]byte
	h.Sum(out[:0])
	return out
}

// hashPair returns keccak256(min(a,b) ‖ max(a,b)) — OZ's commutative
// sorted-pair hash (Hashes.commutativeKeccak256, folded by
// MerkleProof.processProof).
func hashPair(a, b [32]byte) [32]byte {
	if bytes.Compare(a[:], b[:]) > 0 {
		a, b = b, a
	}
	return keccak256(a[:], b[:])
}

// LeafFromPayload double-hashes an abi-encoded leaf payload:
// keccak256(bytes.concat(keccak256(payload))). Use this for leaf tuples
// other than the two built-in (bytes32, uint256) shapes.
func LeafFromPayload(payload []byte) Leaf {
	inner := keccak256(payload)
	return Leaf(keccak256(inner[:]))
}

// uint256Bytes abi-encodes v as a uint256 word. It panics if v is nil,
// negative, or does not fit in 256 bits, since such a value can never be a
// valid uint256 leaf field and silently truncating it would corrupt a
// payout commitment.
func uint256Bytes(v *big.Int) [32]byte {
	if v == nil {
		panic("merkle: nil uint256 value")
	}
	if v.Sign() < 0 {
		panic(fmt.Sprintf("merkle: negative value %s cannot be abi-encoded as uint256", v))
	}
	if v.BitLen() > 256 {
		panic(fmt.Sprintf("merkle: value %s overflows uint256", v))
	}
	var out [32]byte
	v.FillBytes(out[:])
	return out
}

// encodeWordUint256 is abi.encode(bytes32 key, uint256 value): 64 bytes,
// key word then big-endian zero-padded value word.
func encodeWordUint256(key [32]byte, value *big.Int) []byte {
	payload := make([]byte, 64)
	copy(payload[:32], key[:])
	word := uint256Bytes(value)
	copy(payload[32:], word[:])
	return payload
}

// PayoutLeaf hashes a payout leaf: the tuple abi.encode(bytes32 coldkey,
// uint256 shareBps), double-hashed per the package scheme. It panics if
// shareBps is nil, negative, or wider than 256 bits.
func PayoutLeaf(coldkey [32]byte, shareBps *big.Int) Leaf {
	return LeafFromPayload(encodeWordUint256(coldkey, shareBps))
}

// EffortLeaf hashes an effort leaf: the tuple abi.encode(bytes32 pathId,
// uint256 coverage), double-hashed per the package scheme. It panics if
// coverage is nil, negative, or wider than 256 bits.
func EffortLeaf(pathId [32]byte, coverage *big.Int) Leaf {
	return LeafFromPayload(encodeWordUint256(pathId, coverage))
}

// Tree is an immutable Merkle tree over a set of leaf hashes, built in the
// canonical shape described in the package documentation.
type Tree struct {
	leaves    []Leaf       // original input order
	sortedPos []int        // sortedPos[i] = position of leaves[i] in levels[0]
	pos       map[Leaf]int // leaf hash -> position in levels[0]
	levels    [][][32]byte // levels[0] = ascending sorted leaf hashes; last level = [root]
}

// NewTree builds the canonical tree over leaves. The input order is
// irrelevant to the root (leaf hashes are sorted internally) but is
// remembered so ProofAt can address leaves by their original index. It
// returns ErrEmptyTree for an empty input and ErrDuplicateLeaf if any two
// leaves are identical.
func NewTree(leaves []Leaf) (*Tree, error) {
	n := len(leaves)
	if n == 0 {
		return nil, ErrEmptyTree
	}

	// Sort original indices by leaf hash, ascending lexicographic.
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool {
		return bytes.Compare(leaves[order[a]][:], leaves[order[b]][:]) < 0
	})

	t := &Tree{
		leaves:    append([]Leaf(nil), leaves...),
		sortedPos: make([]int, n),
		pos:       make(map[Leaf]int, n),
	}
	level0 := make([][32]byte, n)
	for sortedIdx, origIdx := range order {
		leaf := leaves[origIdx]
		if sortedIdx > 0 && level0[sortedIdx-1] == [32]byte(leaf) {
			return nil, fmt.Errorf("%w: %#x", ErrDuplicateLeaf, leaf[:])
		}
		level0[sortedIdx] = leaf
		t.sortedPos[origIdx] = sortedIdx
		t.pos[leaf] = sortedIdx
	}

	// Build bottom-up: pair adjacent nodes; promote a trailing odd node.
	t.levels = [][][32]byte{level0}
	for cur := level0; len(cur) > 1; {
		next := make([][32]byte, 0, (len(cur)+1)/2)
		for i := 0; i+1 < len(cur); i += 2 {
			next = append(next, hashPair(cur[i], cur[i+1]))
		}
		if len(cur)%2 == 1 {
			next = append(next, cur[len(cur)-1])
		}
		t.levels = append(t.levels, next)
		cur = next
	}
	return t, nil
}

// Len returns the number of leaves in the tree.
func (t *Tree) Len() int {
	return len(t.leaves)
}

// Root returns the Merkle root. For a single-leaf tree the root is the
// leaf hash itself.
func (t *Tree) Root() [32]byte {
	top := t.levels[len(t.levels)-1]
	return top[0]
}

// Proof returns the sibling path for leaf, ordered leaf-to-root, suitable
// for OZ MerkleProof.verify. A single-leaf tree yields an empty proof. It
// returns ErrUnknownLeaf if leaf is not in the tree.
func (t *Tree) Proof(leaf Leaf) ([][32]byte, error) {
	p, ok := t.pos[leaf]
	if !ok {
		return nil, ErrUnknownLeaf
	}
	return t.proofFromSortedPos(p), nil
}

// ProofAt returns the proof for the i-th leaf of the slice originally
// passed to NewTree (not the internal sorted order), so callers that built
// leaves in, say, DB row order can fetch each row's proof directly.
func (t *Tree) ProofAt(i int) ([][32]byte, error) {
	if i < 0 || i >= len(t.leaves) {
		return nil, fmt.Errorf("%w: %d (tree has %d leaves)", ErrIndexOutOfRange, i, len(t.leaves))
	}
	return t.proofFromSortedPos(t.sortedPos[i]), nil
}

// proofFromSortedPos walks the levels from the given level-0 position,
// collecting the sibling at each level. A promoted node has no sibling at
// that level, so it contributes nothing; positions always advance as p/2.
func (t *Tree) proofFromSortedPos(p int) [][32]byte {
	proof := make([][32]byte, 0, len(t.levels)-1)
	for _, level := range t.levels[:len(t.levels)-1] {
		sib := p ^ 1 // p+1 if p is even, p-1 if odd
		if sib < len(level) {
			proof = append(proof, level[sib])
		}
		p /= 2
	}
	return proof
}

// Verify reports whether proof authenticates leaf against root. It is the
// exact Go counterpart of OZ MerkleProof.verify: it folds the leaf with
// each proof element using the commutative sorted-pair keccak256 hash and
// compares the result to root. Verification is tree-shape-blind.
func Verify(root [32]byte, leaf Leaf, proof [][32]byte) bool {
	computed := [32]byte(leaf)
	for _, sibling := range proof {
		computed = hashPair(computed, sibling)
	}
	return computed == root
}
