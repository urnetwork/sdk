package merkle

// trail_leaf.go — the 9-field validator effort ("trail") leaf.
//
// The ST contract commits validator effort claims as a Merkle root over
// TrailLeaf preimages (STSubnet.trailLeafHash). Unlike the 2-field
// (pathId, coverage) sketch in the whitepaper, the committed leaf carries
// the FULL dispute material — the FINAL digest and both Ed25519 signatures —
// so an on-chain dispute is cryptographically decidable from the root alone
// (0x402 verifies each signature against finalDigest).
//
// This helper is the Go counterpart of STSubnet.sol:
//
//	function trailLeafHash(TrailLeaf memory leaf) public pure returns (bytes32) {
//	    return keccak256(bytes.concat(keccak256(abi.encode(
//	        leaf.index,        // uint256
//	        leaf.pathId,       // bytes32
//	        leaf.coverage,     // uint256
//	        leaf.serverKeyId,  // uint64 (abi.encode pads to a full word)
//	        leaf.finalDigest,  // bytes32
//	        leaf.serverSigR,   // bytes32
//	        leaf.serverSigS,   // bytes32
//	        leaf.vpkSigR,      // bytes32
//	        leaf.vpkSigS       // bytes32
//	    ))));
//	}
//
// abi.encode of this static 9-tuple is exactly 9 × 32 bytes: every field
// occupies one word; the uint64 serverKeyId is left-padded (big-endian) to
// 32 bytes like every other unsigned integer. The double keccak is the OZ
// StandardMerkleTree leaf hash used across the package.

import (
	"encoding/binary"
	"math/big"
)

// TrailLeaf hashes a validator effort leaf: the 9-word
// abi.encode(uint256 index, bytes32 pathId, uint256 coverage,
// uint64 serverKeyId, bytes32 finalDigest, bytes32 serverSigR,
// bytes32 serverSigS, bytes32 vpkSigR, bytes32 vpkSigS), double-hashed per
// the package scheme — byte-for-byte STSubnet.trailLeafHash.
//
// `index` is the leaf's 0-based position in the validator's deterministic
// leaf order (it binds the on-chain random sampling); uint64 covers every
// realizable tree size. `coverage` panics if nil, negative, or wider than
// 256 bits (like the other leaf builders).
func TrailLeaf(
	index uint64,
	pathId [32]byte,
	coverage *big.Int,
	serverKeyId uint64,
	finalDigest [32]byte,
	serverSigR [32]byte,
	serverSigS [32]byte,
	vpkSigR [32]byte,
	vpkSigS [32]byte,
) Leaf {
	payload := make([]byte, 9*32)
	binary.BigEndian.PutUint64(payload[24:32], index) // word 0: uint256 index
	copy(payload[32:64], pathId[:])                   // word 1: bytes32 pathId
	coverageWord := uint256Bytes(coverage)            // word 2: uint256 coverage
	copy(payload[64:96], coverageWord[:])
	binary.BigEndian.PutUint64(payload[96+24:128], serverKeyId) // word 3: uint64 -> uint256 word
	copy(payload[128:160], finalDigest[:])                      // word 4: bytes32 finalDigest
	copy(payload[160:192], serverSigR[:])                       // word 5
	copy(payload[192:224], serverSigS[:])                       // word 6
	copy(payload[224:256], vpkSigR[:])                          // word 7
	copy(payload[256:288], vpkSigS[:])                          // word 8
	return LeafFromPayload(payload)
}
