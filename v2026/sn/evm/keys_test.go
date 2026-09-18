package evm

import (
	"encoding/hex"
	"testing"
)

func TestAddressDerivationAndChecksum(t *testing.T) {
	// The EIP-155 example key.
	key, err := PrivateKeyFromHex("0x4646464646464646464646464646464646464646464646464646464646464646")
	if err != nil {
		t.Fatal(err)
	}
	addr := AddressOf(key)
	if got := AddressHex(addr); got != "0x9d8A62f656a8d1615C1294fd71e9CFb3E4855A4F" {
		t.Fatalf("address = %s", got)
	}
	if PrivateKeyHex(key) != "0x4646464646464646464646464646464646464646464646464646464646464646" {
		t.Fatal("hex round trip")
	}
	parsed, err := ParseAddress("0x9d8a62f656a8d1615c1294fd71e9cfb3e4855a4f")
	if err != nil || parsed != addr {
		t.Fatalf("ParseAddress: %v", err)
	}
	for _, bad := range []string{"", "9d8a62f656a8d1615c1294fd71e9cfb3e4855a4f", "0x12", "0xzz8a62f656a8d1615c1294fd71e9cfb3e4855a4f"} {
		if _, err := ParseAddress(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	// EIP-55 reference vectors.
	for _, want := range []string{
		"0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed",
		"0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359",
		"0xdbF03B407c01E7cD3CBea99509d93f8DDDC8C6FB",
		"0xD1220A0cf47c7B9Be7A2E6BA89F429762e7b9aDb",
	} {
		a, err := ParseAddress(want)
		if err != nil {
			t.Fatal(err)
		}
		if got := AddressHex(a); got != want {
			t.Fatalf("checksum %s != %s", got, want)
		}
	}
}

func TestPrivateKeyValidation(t *testing.T) {
	if _, err := PrivateKeyFromBytes(make([]byte, 32)); err == nil {
		t.Fatal("zero key accepted")
	}
	if _, err := PrivateKeyFromBytes(make([]byte, 31)); err == nil {
		t.Fatal("short key accepted")
	}
	n, _ := hex.DecodeString("fffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141")
	if _, err := PrivateKeyFromBytes(n); err == nil {
		t.Fatal("N accepted")
	}
	k, err := GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PrivateKeyFromBytes(k.Serialize()); err != nil {
		t.Fatal(err)
	}
}
