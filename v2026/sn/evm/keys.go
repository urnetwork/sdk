package evm

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"

	secp "github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// GeneratePrivateKey creates a fresh random secp256k1 key.
func GeneratePrivateKey() (*secp.PrivateKey, error) {
	return secp.GeneratePrivateKey()
}

// PrivateKeyFromBytes validates a 32-byte scalar in [1, N-1].
func PrivateKeyFromBytes(b []byte) (*secp.PrivateKey, error) {
	if len(b) != 32 {
		return nil, fmt.Errorf("private key must be 32 bytes, got %d", len(b))
	}
	v := new(big.Int).SetBytes(b)
	if v.Sign() == 0 || v.Cmp(secp.S256().Params().N) >= 0 {
		return nil, errors.New("private key scalar out of range")
	}
	return secp.PrivKeyFromBytes(b), nil
}

// PrivateKeyFromHex parses a 0x-optional hex secret.
func PrivateKeyFromHex(s string) (*secp.PrivateKey, error) {
	b, err := ParseHexBytes(s)
	if err != nil {
		return nil, err
	}
	return PrivateKeyFromBytes(b)
}

// PrivateKeyHex serializes the secret as 0x-prefixed hex (local state only).
func PrivateKeyHex(key *secp.PrivateKey) string {
	return "0x" + hex.EncodeToString(key.Serialize())
}

// AddressOf derives the EVM address of a private key.
func AddressOf(key *secp.PrivateKey) [20]byte {
	return AddressOfPublic(key.PubKey())
}

// AddressOfPublic derives the EVM address of a public key.
func AddressOfPublic(pub *secp.PublicKey) [20]byte {
	uncompressed := pub.SerializeUncompressed()
	h := Keccak256(uncompressed[1:])
	var addr [20]byte
	copy(addr[:], h[12:])
	return addr
}

// AddressHex renders an address with the EIP-55 mixed-case checksum.
func AddressHex(addr [20]byte) string {
	lower := hex.EncodeToString(addr[:])
	h := Keccak256([]byte(lower))
	out := make([]byte, len(lower))
	for i := 0; i < len(lower); i++ {
		c := lower[i]
		nibble := h[i/2]
		if i%2 == 0 {
			nibble >>= 4
		} else {
			nibble &= 0x0f
		}
		if c >= 'a' && c <= 'f' && nibble >= 8 {
			c = c - 'a' + 'A'
		}
		out[i] = c
	}
	return "0x" + string(out)
}

// ParseAddress parses a 20-byte 0x address (any case).
func ParseAddress(s string) ([20]byte, error) {
	var addr [20]byte
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "0x") && !strings.HasPrefix(s, "0X") {
		return addr, errors.New("address must start with 0x")
	}
	b, err := hex.DecodeString(s[2:])
	if err != nil {
		return addr, err
	}
	if len(b) != 20 {
		return addr, fmt.Errorf("address must be 20 bytes, got %d", len(b))
	}
	copy(addr[:], b)
	return addr, nil
}
