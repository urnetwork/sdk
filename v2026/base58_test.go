package sdk

import (
	"bytes"
	"testing"
)

// base58Golden are the standard Bitcoin/Solana base58 vectors plus the
// leading-zero cases. They pin the alphabet and leading-zero handling so a
// future edit cannot silently change the wallet-connect wire encoding.
var base58Golden = []struct {
	decoded []byte
	encoded string
}{
	{[]byte{}, ""},
	{[]byte{0x00}, "1"},
	{[]byte{0x00, 0x00}, "11"},
	{[]byte{0x61}, "2g"},
	{[]byte{0x62, 0x62, 0x62}, "a3gV"},
	{[]byte{0x51, 0x6b, 0x6f, 0xcd, 0x0f}, "ABnLTmg"},
	{[]byte{0x00, 0x00, 0x28, 0x7f, 0xb4, 0xcd}, "11233QC4"},
}

func TestBase58EncodeDecodeGolden(t *testing.T) {
	for _, g := range base58Golden {
		if got := base58Encode(g.decoded); got != g.encoded {
			t.Errorf("base58Encode(%x) = %q, want %q", g.decoded, got, g.encoded)
		}
		if got := base58Decode(g.encoded); !bytes.Equal(got, g.decoded) {
			t.Errorf("base58Decode(%q) = %x, want %x", g.encoded, got, g.decoded)
		}
	}
}

func TestBase58RoundTrip(t *testing.T) {
	cases := [][]byte{
		{},
		{0x00},
		{0x00, 0x00, 0x00},
		{0xff},
		{0xff, 0xff, 0xff, 0xff},
		{0x00, 0x01, 0x02, 0x03, 0xfe, 0xff},
		[]byte("the quick brown fox"),
	}
	for _, c := range cases {
		if got := base58Decode(base58Encode(c)); !bytes.Equal(got, c) {
			t.Errorf("round trip %x -> %q -> %x", c, base58Encode(c), got)
		}
	}
}

// TestBase58InvalidReturnsEmptyNonNil pins the contract quirk: any invalid
// character yields an empty, non-nil slice (len 0), never an error, never nil.
// DecodeBase58 relies on len(result) == 0 to detect bad input.
func TestBase58InvalidReturnsEmptyNonNil(t *testing.T) {
	// 0, O, I, l are deliberately not in the base58 alphabet
	for _, s := range []string{"0", "O", "I", "l", "hello world!", "abc+def"} {
		got := base58Decode(s)
		if got == nil {
			t.Errorf("base58Decode(%q) = nil, want empty non-nil slice", s)
		}
		if len(got) != 0 {
			t.Errorf("base58Decode(%q) = %x, want empty slice", s, got)
		}
	}
}
