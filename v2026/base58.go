// base58Encode/base58Decode implement Bitcoin/Solana modified base58, faithfully
// extracted from github.com/btcsuite/btcutil/base58 so the sdk module no longer
// pulls in the btcsuite/btcd-sized subtree that btcutil dragged in for these
// ~50 lines. The behavior is preserved exactly -- this encoding is on the
// Phantom/Solflare wallet-connect wire path (js bs58) and encodes nacl key
// material, so the byte<->character mapping and these contract quirks must not
// change:
//
//   - base58Decode returns an empty, non-nil slice (len 0) on any invalid input
//     character. It never returns an error and never returns nil; callers detect
//     invalid input via len(result) == 0 (see DecodeBase58). Do not change this
//     to return an error.
//   - leading '1' characters decode to leading zero bytes, and leading zero
//     bytes encode back to leading '1' characters -- base58 has no distinct zero
//     digit, so the count of leading zeros is carried separately.
//   - the alphabet is the Bitcoin alphabet (no 0, O, I, l), identical to the one
//     Solana wallets use for bs58.
//
// Derived from btcutil/base58 (base58.go, alphabet.go). Upstream notice:
//
//	Copyright (c) 2013-2015 The btcsuite developers
//	Use of this source code is governed by an ISC license.
//
//	Permission to use, copy, modify, and distribute this software for any
//	purpose with or without fee is hereby granted, provided that the above
//	copyright notice and this permission notice appear in all copies.
//
//	The software is provided "as is" and the author disclaims all warranties
//	with regard to this software.
package sdk

import (
	"math/big"
)

// base58Alphabet is the modified base58 alphabet used by Bitcoin and Solana.
const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// base58AlphabetIdx0 is the first alphabet character ('1'), which represents a
// leading zero byte.
const base58AlphabetIdx0 = '1'

// base58Table maps an ascii byte to its base58 digit value, or 255 when the
// byte is not a valid base58 character. Generated from base58Alphabet.
var base58Table = [256]byte{
	255, 255, 255, 255, 255, 255, 255, 255,
	255, 255, 255, 255, 255, 255, 255, 255,
	255, 255, 255, 255, 255, 255, 255, 255,
	255, 255, 255, 255, 255, 255, 255, 255,
	255, 255, 255, 255, 255, 255, 255, 255,
	255, 255, 255, 255, 255, 255, 255, 255,
	255, 0, 1, 2, 3, 4, 5, 6,
	7, 8, 255, 255, 255, 255, 255, 255,
	255, 9, 10, 11, 12, 13, 14, 15,
	16, 255, 17, 18, 19, 20, 21, 255,
	22, 23, 24, 25, 26, 27, 28, 29,
	30, 31, 32, 255, 255, 255, 255, 255,
	255, 33, 34, 35, 36, 37, 38, 39,
	40, 41, 42, 43, 255, 44, 45, 46,
	47, 48, 49, 50, 51, 52, 53, 54,
	55, 56, 57, 255, 255, 255, 255, 255,
	255, 255, 255, 255, 255, 255, 255, 255,
	255, 255, 255, 255, 255, 255, 255, 255,
	255, 255, 255, 255, 255, 255, 255, 255,
	255, 255, 255, 255, 255, 255, 255, 255,
	255, 255, 255, 255, 255, 255, 255, 255,
	255, 255, 255, 255, 255, 255, 255, 255,
	255, 255, 255, 255, 255, 255, 255, 255,
	255, 255, 255, 255, 255, 255, 255, 255,
	255, 255, 255, 255, 255, 255, 255, 255,
	255, 255, 255, 255, 255, 255, 255, 255,
	255, 255, 255, 255, 255, 255, 255, 255,
	255, 255, 255, 255, 255, 255, 255, 255,
	255, 255, 255, 255, 255, 255, 255, 255,
	255, 255, 255, 255, 255, 255, 255, 255,
	255, 255, 255, 255, 255, 255, 255, 255,
	255, 255, 255, 255, 255, 255, 255, 255,
}

var base58BigRadix = big.NewInt(58)
var base58BigZero = big.NewInt(0)

// base58Decode decodes a modified base58 string to a byte slice. On any invalid
// input character it returns an empty, non-nil slice (len 0) -- not an error and
// not nil (see the file header contract).
func base58Decode(b string) []byte {
	answer := big.NewInt(0)
	j := big.NewInt(1)

	scratch := new(big.Int)
	for i := len(b) - 1; i >= 0; i-- {
		tmp := base58Table[b[i]]
		if tmp == 255 {
			return []byte("")
		}
		scratch.SetInt64(int64(tmp))
		scratch.Mul(j, scratch)
		answer.Add(answer, scratch)
		j.Mul(j, base58BigRadix)
	}

	tmpval := answer.Bytes()

	var numZeros int
	for numZeros = 0; numZeros < len(b); numZeros++ {
		if b[numZeros] != base58AlphabetIdx0 {
			break
		}
	}
	flen := numZeros + len(tmpval)
	val := make([]byte, flen)
	copy(val[numZeros:], tmpval)

	return val
}

// base58Encode encodes a byte slice to a modified base58 string. Leading zero
// bytes encode to leading '1' characters.
func base58Encode(b []byte) string {
	x := new(big.Int)
	x.SetBytes(b)

	answer := make([]byte, 0, len(b)*136/100)
	for x.Cmp(base58BigZero) > 0 {
		mod := new(big.Int)
		x.DivMod(x, base58BigRadix, mod)
		answer = append(answer, base58Alphabet[mod.Int64()])
	}

	// leading zero bytes
	for _, i := range b {
		if i != 0 {
			break
		}
		answer = append(answer, base58AlphabetIdx0)
	}

	// reverse
	alen := len(answer)
	for i := 0; i < alen/2; i++ {
		answer[i], answer[alen-1-i] = answer[alen-1-i], answer[i]
	}

	return string(answer)
}
