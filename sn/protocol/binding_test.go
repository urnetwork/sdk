package protocol

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type fixture struct {
	ChainID         uint64 `json:"chain_id"`
	Netuid          uint16 `json:"netuid"`
	Coordinator     string `json:"coordinator"`
	FleetID         string `json:"fleet_id"`
	Hotkey          string `json:"hotkey"`
	ClientID        string `json:"client_id"`
	ClientKey       string `json:"client_key"`
	Generation      uint64 `json:"generation"`
	ValidFromEpoch  uint64 `json:"valid_from_epoch"`
	ValidToEpoch    uint64 `json:"valid_to_epoch"`
	CommitmentHash  string `json:"commitment_hash"`
	Payload         string `json:"payload"`
	Digest          string `json:"digest"`
	ClientSignature string `json:"client_signature"`
}

func unhex(t *testing.T, s string, dst []byte) {
	t.Helper()
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil || len(b) != len(dst) {
		t.Fatalf("bad fixture hex %q", s)
	}
	copy(dst, b)
}

// The committed sn/protocol golden: payload, digest and the client signature
// from the seed 0x00..1f must all reproduce byte for byte.
func TestFleetBindingGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/fleet-binding-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	b := FleetBinding{ChainID: f.ChainID, Netuid: f.Netuid, Generation: f.Generation, ValidFromEpoch: f.ValidFromEpoch, ValidToEpoch: f.ValidToEpoch}
	unhex(t, f.Coordinator, b.Coordinator[:])
	unhex(t, f.FleetID, b.FleetID[:])
	unhex(t, f.Hotkey, b.Hotkey[:])
	unhex(t, f.ClientID, b.ClientID[:])
	unhex(t, f.ClientKey, b.ClientKey[:])
	unhex(t, f.CommitmentHash, b.CommitmentHash[:])

	payload, err := b.Payload()
	if err != nil {
		t.Fatal(err)
	}
	if got := "0x" + hex.EncodeToString(payload); got != f.Payload {
		t.Fatalf("payload differs from fixture")
	}
	digest, err := b.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if got := "0x" + hex.EncodeToString(digest[:]); got != f.Digest {
		t.Fatalf("digest %s != %s", got, f.Digest)
	}
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	sig, err := b.SignClient(priv)
	if err != nil {
		t.Fatal(err)
	}
	if got := "0x" + hex.EncodeToString(sig); got != f.ClientSignature {
		t.Fatalf("client signature %s != %s", got, f.ClientSignature)
	}
	if !b.VerifyClient(sig) {
		t.Fatal("VerifyClient rejected the golden signature")
	}
	sig[0] ^= 1
	if b.VerifyClient(sig) {
		t.Fatal("VerifyClient accepted a corrupted signature")
	}
	other := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	if _, err := b.SignClient(other); err == nil {
		t.Fatal("signed with a key that is not the binding's client key")
	}
	bad := b
	bad.Generation = 0
	if _, err := bad.Digest(); err == nil {
		t.Fatal("invalid binding digested")
	}
}
