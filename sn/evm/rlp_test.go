package evm

import (
	"bytes"
	"encoding/hex"
	"math/big"
	"testing"
)

func TestRlpVectors(t *testing.T) {
	cases := []struct {
		name string
		got  []byte
		want string
	}{
		{"dog", rlpBytes([]byte("dog")), "83646f67"},
		{"cat,dog", rlpList(rlpBytes([]byte("cat")), rlpBytes([]byte("dog"))), "c88363617483646f67"},
		{"empty string", rlpBytes(nil), "80"},
		{"empty list", rlpList(), "c0"},
		{"zero", rlpUint(big.NewInt(0)), "80"},
		{"15", rlpUint(big.NewInt(15)), "0f"},
		{"1024", rlpUint(big.NewInt(1024)), "820400"},
		{"long string", rlpBytes(bytes.Repeat([]byte("a"), 56)), "b838" + hex.EncodeToString(bytes.Repeat([]byte("a"), 56))},
	}
	for _, tc := range cases {
		if got := hex.EncodeToString(tc.got); got != tc.want {
			t.Fatalf("%s: got %s want %s", tc.name, got, tc.want)
		}
	}
}
