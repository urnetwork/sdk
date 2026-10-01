// Package evm is the hand-rolled Subtensor EVM client the SDK uses to read
// and settle UR protocol entitlements directly against the STSettlementVault
// and STCoordinator contracts: ABI packing for the handful of calls involved,
// RLP + EIP-155/EIP-1559 transaction signing with the pure-Go secp256k1, and
// JSON-RPC over net/http. No go-ethereum: the app binaries stay small and the
// signing path has no dependency that could route a transaction anywhere but
// the chain endpoint the user configured.
package evm

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"golang.org/x/crypto/sha3"
)

// BPS is the share denominator used by the vault (shareBps / 10000).
const BPS = 10000

// Selectors of the contract entry points the SDK calls. Pinned by test to
// the values the sn repo's miner uses (claim = ce479a1b).
var (
	claimSelector           = selector("claim(uint256,uint256,bytes32,uint256,bytes32[])")
	entitlementSelector     = selector("entitlement(uint256,uint256)")
	leafClaimedSelector     = selector("leafClaimed(uint256,bytes32)")
	currentEpochSelector    = selector("currentEpoch()")
	epochStartBlockSelector = selector("epochStartBlock(uint256)")
	rootCommitmentsSelector = selector("rootCommitments(uint256,uint256)")
	errorStringSelector     = selector("Error(string)")
)

// Custom errors raised by STSettlementVault.claim, decoded for the user.
var vaultErrorNames = map[[4]byte]string{
	selector("InvalidTransition()"):    "InvalidTransition",
	selector("ClaimExpired()"):         "ClaimExpired",
	selector("InvalidConfiguration()"): "InvalidConfiguration",
	selector("InvalidProof()"):         "InvalidProof",
	selector("AlreadyClaimed()"):       "AlreadyClaimed",
	selector("Underfunded()"):          "Underfunded",
	selector("NothingToWithdraw()"):    "NothingToWithdraw",
	selector("InvalidEpoch()"):         "InvalidEpoch",
	selector("InvalidPolicy()"):        "InvalidPolicy",
}

// Keccak256 hashes the concatenation of data.
func Keccak256(data ...[]byte) [32]byte {
	h := sha3.NewLegacyKeccak256()
	for _, d := range data {
		h.Write(d)
	}
	var out [32]byte
	h.Sum(out[:0])
	return out
}

func selector(signature string) [4]byte {
	h := Keccak256([]byte(signature))
	var s [4]byte
	copy(s[:], h[:4])
	return s
}

// Selector returns the four-byte selector of a canonical function signature.
func Selector(signature string) [4]byte {
	return selector(signature)
}

var maxUint256 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))

// UintWord encodes v as one 32-byte big-endian ABI word.
func UintWord(v *big.Int) ([32]byte, error) {
	var w [32]byte
	if v == nil {
		return w, errors.New("nil uint256")
	}
	if v.Sign() < 0 || v.Cmp(maxUint256) > 0 {
		return w, fmt.Errorf("uint256 out of range: %s", v)
	}
	v.FillBytes(w[:])
	return w, nil
}

func uint64Word(v uint64) [32]byte {
	w, _ := UintWord(new(big.Int).SetUint64(v))
	return w
}

// PackClaim encodes claim(epoch, noId, coldkey, shareBps, proof).
func PackClaim(epoch, noId *big.Int, coldkey [32]byte, shareBps *big.Int, proof [][32]byte) ([]byte, error) {
	e, err := UintWord(epoch)
	if err != nil {
		return nil, fmt.Errorf("epoch: %w", err)
	}
	n, err := UintWord(noId)
	if err != nil {
		return nil, fmt.Errorf("noId: %w", err)
	}
	s, err := UintWord(shareBps)
	if err != nil {
		return nil, fmt.Errorf("shareBps: %w", err)
	}
	out := make([]byte, 0, 4+32*(6+len(proof)))
	out = append(out, claimSelector[:]...)
	out = append(out, e[:]...)
	out = append(out, n[:]...)
	out = append(out, coldkey[:]...)
	out = append(out, s[:]...)
	// offset of the dynamic proof array: after the five head words
	offset := uint64Word(5 * 32)
	out = append(out, offset[:]...)
	length := uint64Word(uint64(len(proof)))
	out = append(out, length[:]...)
	for _, node := range proof {
		out = append(out, node[:]...)
	}
	return out, nil
}

// PackEntitlement encodes entitlement(epoch, noId).
func PackEntitlement(epoch, noId *big.Int) ([]byte, error) {
	return packTwoUints(entitlementSelector, epoch, noId)
}

// PackRootCommitments encodes STCoordinator.rootCommitments(epoch, noId).
func PackRootCommitments(epoch, noId *big.Int) ([]byte, error) {
	return packTwoUints(rootCommitmentsSelector, epoch, noId)
}

func packTwoUints(sel [4]byte, a, b *big.Int) ([]byte, error) {
	aw, err := UintWord(a)
	if err != nil {
		return nil, err
	}
	bw, err := UintWord(b)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 4+64)
	out = append(out, sel[:]...)
	out = append(out, aw[:]...)
	out = append(out, bw[:]...)
	return out, nil
}

// PackLeafClaimed encodes leafClaimed(epoch, claimKey).
func PackLeafClaimed(epoch *big.Int, key [32]byte) ([]byte, error) {
	e, err := UintWord(epoch)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 4+64)
	out = append(out, leafClaimedSelector[:]...)
	out = append(out, e[:]...)
	out = append(out, key[:]...)
	return out, nil
}

// PackCurrentEpoch encodes STCoordinator.currentEpoch().
func PackCurrentEpoch() []byte {
	return append([]byte(nil), currentEpochSelector[:]...)
}

// PackEpochStartBlock encodes STCoordinator.epochStartBlock(epoch).
func PackEpochStartBlock(epoch *big.Int) ([]byte, error) {
	e, err := UintWord(epoch)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 4+32)
	out = append(out, epochStartBlockSelector[:]...)
	out = append(out, e[:]...)
	return out, nil
}

// ClaimKey is the vault's per-coldkey claim slot: keccak256(abi.encode(noId, coldkey)).
func ClaimKey(noId *big.Int, coldkey [32]byte) ([32]byte, error) {
	n, err := UintWord(noId)
	if err != nil {
		return [32]byte{}, err
	}
	return Keccak256(n[:], coldkey[:]), nil
}

// PayoutLeaf mirrors STSettlementVault.payoutLeaf:
// keccak256(bytes.concat(keccak256(abi.encode(coldkey, shareBps)))).
func PayoutLeaf(coldkey [32]byte, shareBps *big.Int) ([32]byte, error) {
	s, err := UintWord(shareBps)
	if err != nil {
		return [32]byte{}, err
	}
	inner := Keccak256(coldkey[:], s[:])
	return Keccak256(inner[:]), nil
}

// EpochStatus mirrors STSettlementVault.EpochStatus.
type EpochStatus uint8

const (
	StatusUnset EpochStatus = iota
	StatusFunded
	StatusFinalized
	StatusRootMissed
	StatusCarried
)

func (s EpochStatus) String() string {
	switch s {
	case StatusUnset:
		return "unset"
	case StatusFunded:
		return "funded"
	case StatusFinalized:
		return "finalized"
	case StatusRootMissed:
		return "root-missed"
	case StatusCarried:
		return "carried"
	default:
		return fmt.Sprintf("status-%d", uint8(s))
	}
}

// Entitlement mirrors the vault's Entitlement record.
type Entitlement struct {
	PayoutRoot   [32]byte
	ArtifactHash [32]byte
	Funded       *big.Int
	Total        *big.Int
	Claimed      *big.Int
	ExpiryBlock  uint64
	Status       EpochStatus
}

// DecodeEntitlement decodes the inline static tuple returned by entitlement().
func DecodeEntitlement(ret []byte) (*Entitlement, error) {
	if len(ret) < 7*32 {
		return nil, fmt.Errorf("entitlement return too short: %d bytes", len(ret))
	}
	e := &Entitlement{}
	copy(e.PayoutRoot[:], ret[0:32])
	copy(e.ArtifactHash[:], ret[32:64])
	e.Funded = new(big.Int).SetBytes(ret[64:96])
	e.Total = new(big.Int).SetBytes(ret[96:128])
	e.Claimed = new(big.Int).SetBytes(ret[128:160])
	expiry := new(big.Int).SetBytes(ret[160:192])
	if !expiry.IsUint64() {
		return nil, errors.New("entitlement expiry block overflows uint64")
	}
	e.ExpiryBlock = expiry.Uint64()
	status := new(big.Int).SetBytes(ret[192:224])
	if !status.IsUint64() || status.Uint64() > 255 {
		return nil, errors.New("entitlement status is not a uint8")
	}
	e.Status = EpochStatus(status.Uint64())
	return e, nil
}

// RootCommitment mirrors STCoordinator.rootCommitments(epoch, noId).
type RootCommitment struct {
	PayoutRoot   [32]byte
	ArtifactHash [32]byte
	Committer    [20]byte
	CommitBlock  uint64
}

// DecodeRootCommitment decodes the four static words of rootCommitments().
func DecodeRootCommitment(ret []byte) (*RootCommitment, error) {
	if len(ret) < 4*32 {
		return nil, fmt.Errorf("rootCommitments return too short: %d bytes", len(ret))
	}
	c := &RootCommitment{}
	copy(c.PayoutRoot[:], ret[0:32])
	copy(c.ArtifactHash[:], ret[32:64])
	copy(c.Committer[:], ret[64+12:96])
	block := new(big.Int).SetBytes(ret[96:128])
	if !block.IsUint64() {
		return nil, errors.New("commit block overflows uint64")
	}
	c.CommitBlock = block.Uint64()
	return c, nil
}

// DecodeBool decodes a single bool word.
func DecodeBool(ret []byte) (bool, error) {
	if len(ret) < 32 {
		return false, fmt.Errorf("bool return too short: %d bytes", len(ret))
	}
	for _, b := range ret[:31] {
		if b != 0 {
			return false, errors.New("bool word has high bits set")
		}
	}
	return ret[31] != 0, nil
}

// DecodeUint decodes a single uint256 word.
func DecodeUint(ret []byte) (*big.Int, error) {
	if len(ret) < 32 {
		return nil, fmt.Errorf("uint return too short: %d bytes", len(ret))
	}
	return new(big.Int).SetBytes(ret[:32]), nil
}

// DecodeRevert renders revert data as a short human-readable reason: the
// string of Error(string), the name of a known vault custom error, or hex.
func DecodeRevert(data []byte) string {
	if len(data) == 0 {
		return "reverted"
	}
	if len(data) >= 4 {
		var sel [4]byte
		copy(sel[:], data[:4])
		if sel == errorStringSelector && len(data) >= 4+64 {
			body := data[4:]
			offset := new(big.Int).SetBytes(body[:32])
			if offset.IsInt64() && offset.Int64()+32 <= int64(len(body)) {
				start := offset.Int64()
				length := new(big.Int).SetBytes(body[start : start+32])
				if length.IsInt64() && start+32+length.Int64() <= int64(len(body)) {
					return string(body[start+32 : start+32+length.Int64()])
				}
			}
		}
		if name, ok := vaultErrorNames[sel]; ok {
			return name
		}
	}
	return "reverted: 0x" + hex.EncodeToString(data)
}

// ParseHexBytes parses 0x-prefixed (or bare) hex.
func ParseHexBytes(s string) ([]byte, error) {
	s = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(s), "0x"), "0X")
	if len(s)%2 == 1 {
		s = "0" + s
	}
	return hex.DecodeString(s)
}

// ParseHash32 parses a 32-byte hex value.
func ParseHash32(s string) ([32]byte, error) {
	var out [32]byte
	b, err := ParseHexBytes(s)
	if err != nil {
		return out, err
	}
	if len(b) != 32 {
		return out, fmt.Errorf("expected 32 bytes, got %d", len(b))
	}
	copy(out[:], b)
	return out, nil
}

// ParseUint256 parses a decimal or 0x-hex unsigned integer.
func ParseUint256(s string) (*big.Int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("empty number")
	}
	v := new(big.Int)
	var ok bool
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		_, ok = v.SetString(s[2:], 16)
	} else {
		_, ok = v.SetString(s, 10)
	}
	if !ok || v.Sign() < 0 || v.Cmp(maxUint256) > 0 {
		return nil, fmt.Errorf("invalid uint256 %q", s)
	}
	return v, nil
}
