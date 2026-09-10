// Package ss58 implements SS58 address encoding/decoding for 32-byte
// account public keys (Substrate/Bittensor), plus the Subtensor EVM
// H160 -> AccountId32 mirror derivation.
//
// Format: base58( prefixBytes ‖ pubkey[32] ‖ checksum[2] ) where
// checksum = blake2b-512("SS58PRE" ‖ prefixBytes ‖ pubkey)[:2].
// Network prefix types 0..63 use one prefix byte; 64..16383 use the
// two-byte form. Bittensor uses the generic Substrate prefix 42.
//
// The EVM mirror is the account the Subtensor runtime debits/credits
// for an EVM H160: pubkey = blake2b-256("evm:" ‖ h160). It is one-way;
// no substrate private key exists for it. (Runtime semantics verified
// on testnet under SP-1.)
package ss58

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"

	"golang.org/x/crypto/blake2b"
)

const (
	// BittensorPrefix is the SS58 network type Bittensor uses (generic Substrate).
	BittensorPrefix uint16 = 42

	checksumPrefix = "SS58PRE"
)

var (
	ErrInvalidBase58   = errors.New("ss58: invalid base58")
	ErrInvalidLength   = errors.New("ss58: invalid decoded length")
	ErrInvalidChecksum = errors.New("ss58: invalid checksum")
	ErrInvalidPrefix   = errors.New("ss58: invalid network prefix")
)

// Encode renders a 32-byte account public key as an SS58 address with the
// given network prefix type (0..16383).
func Encode(pubkey [32]byte, prefix uint16) (string, error) {
	prefixBytes, err := prefixToBytes(prefix)
	if err != nil {
		return "", err
	}
	payload := append(prefixBytes, pubkey[:]...)
	sum := checksum(payload)
	return base58Encode(append(payload, sum[:2]...)), nil
}

// Decode parses an SS58 address, returning the 32-byte public key and the
// network prefix type. The checksum is verified.
func Decode(address string) (pubkey [32]byte, prefix uint16, err error) {
	raw, err := base58Decode(address)
	if err != nil {
		return pubkey, 0, err
	}
	// 1-byte prefix: 1 + 32 + 2 = 35; 2-byte prefix: 2 + 32 + 2 = 36
	var prefixLen int
	switch len(raw) {
	case 35:
		prefixLen = 1
	case 36:
		prefixLen = 2
	default:
		return pubkey, 0, fmt.Errorf("%w: %d bytes", ErrInvalidLength, len(raw))
	}
	payload := raw[:len(raw)-2]
	sum := checksum(payload)
	if !bytes.Equal(sum[:2], raw[len(raw)-2:]) {
		return pubkey, 0, ErrInvalidChecksum
	}
	prefix, err = prefixFromBytes(raw[:prefixLen])
	if err != nil {
		return pubkey, 0, err
	}
	copy(pubkey[:], payload[prefixLen:])
	return pubkey, prefix, nil
}

// DecodeWithPrefix decodes and additionally requires the given network prefix.
func DecodeWithPrefix(address string, requiredPrefix uint16) ([32]byte, error) {
	pubkey, prefix, err := Decode(address)
	if err != nil {
		return pubkey, err
	}
	if prefix != requiredPrefix {
		return pubkey, fmt.Errorf("%w: got %d, want %d", ErrInvalidPrefix, prefix, requiredPrefix)
	}
	return pubkey, nil
}

// EvmMirrorPubkey derives the AccountId32 the Subtensor runtime maps an EVM
// H160 address to: blake2b-256("evm:" ‖ h160).
func EvmMirrorPubkey(h160 [20]byte) [32]byte {
	return blake2b.Sum256(append([]byte("evm:"), h160[:]...))
}

// EvmMirrorAddress renders the mirror account of an EVM H160 as an SS58
// address under the given prefix (fund this address to fund the H160).
func EvmMirrorAddress(h160 [20]byte, prefix uint16) (string, error) {
	return Encode(EvmMirrorPubkey(h160), prefix)
}

func checksum(payload []byte) [64]byte {
	return blake2b.Sum512(append([]byte(checksumPrefix), payload...))
}

func prefixToBytes(prefix uint16) ([]byte, error) {
	switch {
	case prefix <= 63:
		return []byte{byte(prefix)}, nil
	case prefix <= 16383:
		// two-byte form per the SS58 registry
		return []byte{
			byte(prefix&0b0000_0000_1111_1100)>>2 | 0b0100_0000,
			byte(prefix>>8) | byte(prefix&0b0000_0000_0000_0011)<<6,
		}, nil
	default:
		return nil, fmt.Errorf("%w: %d", ErrInvalidPrefix, prefix)
	}
}

func prefixFromBytes(prefixBytes []byte) (uint16, error) {
	switch len(prefixBytes) {
	case 1:
		if prefixBytes[0] > 63 {
			return 0, ErrInvalidPrefix
		}
		return uint16(prefixBytes[0]), nil
	case 2:
		lower := uint16(prefixBytes[0]&0b0011_1111)<<2 | uint16(prefixBytes[1])>>6
		upper := uint16(prefixBytes[1]&0b0011_1111) << 8
		return lower | upper, nil
	default:
		return 0, ErrInvalidPrefix
	}
}

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

var base58Index = func() [256]int8 {
	var index [256]int8
	for i := range index {
		index[i] = -1
	}
	for i, c := range []byte(base58Alphabet) {
		index[c] = int8(i)
	}
	return index
}()

func base58Encode(input []byte) string {
	zeros := 0
	for zeros < len(input) && input[zeros] == 0 {
		zeros++
	}
	n := new(big.Int).SetBytes(input)
	radix := big.NewInt(58)
	mod := new(big.Int)
	// max output length ~ len(input) * 138/100 + 1
	out := make([]byte, 0, len(input)*138/100+1)
	for n.Sign() > 0 {
		n.DivMod(n, radix, mod)
		out = append(out, base58Alphabet[mod.Int64()])
	}
	for i := 0; i < zeros; i++ {
		out = append(out, base58Alphabet[0])
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}

func base58Decode(input string) ([]byte, error) {
	if len(input) == 0 {
		return nil, ErrInvalidBase58
	}
	zeros := 0
	for zeros < len(input) && input[zeros] == base58Alphabet[0] {
		zeros++
	}
	n := new(big.Int)
	radix := big.NewInt(58)
	for i := 0; i < len(input); i++ {
		digit := base58Index[input[i]]
		if digit < 0 {
			return nil, fmt.Errorf("%w: character %q", ErrInvalidBase58, input[i])
		}
		n.Mul(n, radix)
		n.Add(n, big.NewInt(int64(digit)))
	}
	decoded := n.Bytes()
	out := make([]byte, zeros+len(decoded))
	copy(out[zeros:], decoded)
	return out, nil
}
