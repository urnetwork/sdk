package evm

import "math/big"

// Minimal RLP encoder: only what transaction serialization needs.

func rlpBytes(b []byte) []byte {
	if len(b) == 1 && b[0] < 0x80 {
		return []byte{b[0]}
	}
	return append(rlpLength(len(b), 0x80), b...)
}

func rlpUint(v *big.Int) []byte {
	if v == nil || v.Sign() == 0 {
		return []byte{0x80}
	}
	return rlpBytes(v.Bytes())
}

func rlpUint64(v uint64) []byte {
	return rlpUint(new(big.Int).SetUint64(v))
}

func rlpList(items ...[]byte) []byte {
	var body []byte
	for _, item := range items {
		body = append(body, item...)
	}
	return append(rlpLength(len(body), 0xc0), body...)
}

func rlpLength(n int, offset byte) []byte {
	if n < 56 {
		return []byte{offset + byte(n)}
	}
	be := new(big.Int).SetInt64(int64(n)).Bytes()
	return append([]byte{offset + 55 + byte(len(be))}, be...)
}
