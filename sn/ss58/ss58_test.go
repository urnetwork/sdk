package ss58

import (
	"encoding/hex"
	"strings"
	"testing"
)

// Well-known Substrate dev account (Alice), generic prefix 42.
const (
	aliceAddress = "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY"
	alicePubkey  = "d43593c715fdd31c61141abd04a99fd6822c8558854ccde39a5684e7a56da27d"
)

func mustHex32(t *testing.T, s string) [32]byte {
	t.Helper()
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 32 {
		t.Fatalf("bad hex32 fixture %q: %v", s, err)
	}
	var out [32]byte
	copy(out[:], raw)
	return out
}

func TestDecodeAlice(t *testing.T) {
	pubkey, prefix, err := Decode(aliceAddress)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if prefix != BittensorPrefix {
		t.Fatalf("prefix: got %d, want %d", prefix, BittensorPrefix)
	}
	if got := hex.EncodeToString(pubkey[:]); got != alicePubkey {
		t.Fatalf("pubkey: got %s, want %s", got, alicePubkey)
	}
}

func TestEncodeAlice(t *testing.T) {
	address, err := Encode(mustHex32(t, alicePubkey), BittensorPrefix)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if address != aliceAddress {
		t.Fatalf("address: got %s, want %s", address, aliceAddress)
	}
}

func TestRoundTripPrefixes(t *testing.T) {
	pubkey := mustHex32(t, alicePubkey)
	for _, prefix := range []uint16{0, 2, 42, 63, 64, 255, 4096, 16383} {
		address, err := Encode(pubkey, prefix)
		if err != nil {
			t.Fatalf("encode prefix %d: %v", prefix, err)
		}
		gotPubkey, gotPrefix, err := Decode(address)
		if err != nil {
			t.Fatalf("decode prefix %d (%s): %v", prefix, address, err)
		}
		if gotPrefix != prefix {
			t.Errorf("prefix %d: round-tripped to %d", prefix, gotPrefix)
		}
		if gotPubkey != pubkey {
			t.Errorf("prefix %d: pubkey mismatch", prefix)
		}
	}
}

func TestDecodeRejectsCorruption(t *testing.T) {
	// flip one address character (avoid producing an invalid base58 char)
	corrupt := []byte(aliceAddress)
	if corrupt[10] == 'a' {
		corrupt[10] = 'b'
	} else {
		corrupt[10] = 'a'
	}
	if _, _, err := Decode(string(corrupt)); err == nil {
		t.Fatal("corrupted address decoded without error")
	}

	if _, _, err := Decode("5Grwva0"); err == nil { // '0' is not base58
		t.Fatal("invalid base58 accepted")
	}

	if _, _, err := Decode("2q"); err == nil {
		t.Fatal("short input accepted")
	}
}

func TestDecodeWithPrefix(t *testing.T) {
	if _, err := DecodeWithPrefix(aliceAddress, BittensorPrefix); err != nil {
		t.Fatalf("expected prefix accept: %v", err)
	}
	if _, err := DecodeWithPrefix(aliceAddress, 0); err == nil {
		t.Fatal("wrong prefix accepted")
	}
}

func TestEvmMirror(t *testing.T) {
	// Deterministic derivation: pubkey = blake2b-256("evm:" ‖ h160).
	// Runtime conformance (that Subtensor uses exactly this mapping) is
	// checked live under SP-1; this test pins our derivation.
	var h160 [20]byte
	for i := range h160 {
		h160[i] = byte(i + 1)
	}
	pubkey := EvmMirrorPubkey(h160)
	address, err := EvmMirrorAddress(h160, BittensorPrefix)
	if err != nil {
		t.Fatalf("mirror encode: %v", err)
	}
	decoded, prefix, err := Decode(address)
	if err != nil {
		t.Fatalf("mirror decode: %v", err)
	}
	if prefix != BittensorPrefix || decoded != pubkey {
		t.Fatal("mirror address does not round-trip to derived pubkey")
	}
	if strings.HasPrefix(address, "5") == false {
		t.Errorf("prefix-42 addresses start with 5, got %s", address)
	}

	// distinct inputs -> distinct mirrors
	var h160b [20]byte
	copy(h160b[:], h160[:])
	h160b[19] ^= 0xff
	if EvmMirrorPubkey(h160b) == pubkey {
		t.Fatal("mirror collision on distinct inputs")
	}
}
