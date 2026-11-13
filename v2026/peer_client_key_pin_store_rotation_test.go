// Persistent admission must agree with real signed-history signer authorization.
package sdk

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"github.com/urnetwork/connect/v2026"
	"golang.org/x/crypto/sha3"
)

// A build-authorized signer transition extends its pinned predecessor. Removing
// the new signer's trust rejects verification before either store is mutated.
func TestPeerPinStoresPersistVerifiedSignerRotation(t *testing.T) {
	peer := connect.NewId()
	firstKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x31}, ed25519.SeedSize))
	secondKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x32}, ed25519.SeedSize))
	first := connect.ClientKeyRegistration{
		Schema: "urnetwork-operator-client-key-registration-v1",
		Domain: connect.ClientKeyHistoryDomain{
			ChainID: 1, GenesisHash: [32]byte{1}, Netuid: 1,
			Coordinator: connect.ClientKeyAddress{1}, SettlementVault: connect.ClientKeyAddress{2},
			DeploymentIDHash: [32]byte{2}, PolicyHash: [32]byte{3}, NoID: 1,
		},
		ClientID: [16]byte(peer), NetworkID: [16]byte(connect.NewId()), Generation: 1, Present: true,
		EffectiveBoundary: connect.ClientKeyEffectiveBoundary{Epoch: 1, Block: 1, Hash: [32]byte{4}},
	}
	copy(first.PublicKey[:], firstKey.Public().(ed25519.PublicKey))
	sign := func(registration *connect.ClientKeyRegistration, seed byte) []byte {
		t.Helper()
		privateKey := secp256k1.PrivKeyFromBytes(bytes.Repeat([]byte{seed}, 32))
		hash := sha3.NewLegacyKeccak256()
		hash.Write(privateKey.PubKey().SerializeUncompressed()[1:])
		address := hash.Sum(nil)
		copy(registration.Signer[:], address[len(address)-len(registration.Signer):])
		digest, err := registration.Digest()
		if err != nil {
			t.Fatal(err)
		}
		compact := ecdsa.SignCompact(privateKey, digest[:], false)
		copy(registration.Signature[:64], compact[1:])
		registration.Signature[64] = compact[0] - 27
		encoded, err := json.Marshal(registration)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	encodedFirst := sign(&first, 0x21)
	second := first
	second.Generation++
	second.EffectiveBoundary.Block++
	second.EffectiveBoundary.Hash[0]++
	copy(second.PublicKey[:], secondKey.Public().(ed25519.PublicKey))
	var err error
	second.PreviousHash, err = first.ContentHash()
	if err != nil {
		t.Fatal(err)
	}
	encodedSecond := sign(&second, 0x22)
	domain, err := first.Domain.Digest()
	if err != nil {
		t.Fatal(err)
	}
	policy := connect.ClientKeyHistoryPolicy{
		MaxGenerations: 4,
		TrustedSigners: []connect.ClientKeyTrustedSigner{{DomainDigest: domain, Signer: first.Signer}},
	}
	_, initial, err := connect.VerifyClientKeyHistory(peer, [][]byte{encodedFirst}, policy)
	if err != nil {
		t.Fatalf("initial signed history: %v", err)
	}
	history := [][]byte{encodedFirst, encodedSecond}
	for _, kind := range []string{"legacy", "bounded"} {
		f := newAtomicPeerPinFixture(t, kind)
		if err := f.store.CommitPeerClientKeyPin(peer, initial); err != nil {
			t.Fatal(err)
		}
		snapshot, err := f.store.ReadPeerClientKeyPinSnapshot(peer)
		if err != nil {
			t.Fatal(err)
		}
		unauthorized := policy
		unauthorized.Pin = &snapshot.Pin
		if _, _, err := connect.VerifyClientKeyHistory(peer, history, unauthorized); !errors.Is(err, connect.ErrClientKeyRegistrationInvalid) {
			t.Fatalf("%s accepted the new signer without build authorization: %v", kind, err)
		}
		if unchanged, err := f.store.ReadPeerClientKeyPinSnapshot(peer); err != nil || unchanged != snapshot {
			t.Fatalf("%s failed verification changed stored authority: %+v %v", kind, unchanged, err)
		}
		authorized := connect.ClientKeyHistoryPolicy{
			Pin: &snapshot.Pin, MaxGenerations: 4,
			TrustedSigners: []connect.ClientKeyTrustedSigner{
				{DomainDigest: domain, Signer: first.Signer},
				{DomainDigest: domain, Signer: second.Signer},
			},
		}
		_, rotated, err := connect.VerifyClientKeyHistory(peer, history, authorized)
		if err != nil || rotated.Signer != second.Signer || rotated.Generation != 2 {
			t.Fatalf("%s authorized signer extension did not verify: %+v %v", kind, rotated, err)
		}
		if err := f.store.CommitPeerClientKeyPin(peer, rotated); !errors.Is(err, errPeerPinStoreRollback) {
			t.Fatalf("%s unconditional write changed a pinned signer: %v", kind, err)
		}
		f.store.SetPeerClientKeyPin(peer, rotated)
		if unchanged, err := f.store.ReadPeerClientKeyPinSnapshot(peer); err != nil || unchanged != snapshot {
			t.Fatalf("%s legacy setter changed a pinned signer: %+v %v", kind, unchanged, err)
		}
		if accepted, err := f.store.CompareAndSwapPeerClientKeyPin(peer, snapshot, &rotated); !accepted || err != nil {
			t.Fatalf("%s refused verified signer rotation: accepted=%t error=%v", kind, accepted, err)
		}
		if accepted, err := f.store.CompareAndSwapPeerClientKeyPin(peer, snapshot, &rotated); accepted || err != nil {
			t.Fatalf("%s stale signer snapshot was reused: %t %v", kind, accepted, err)
		}
		reloaded := newLocalStatePeerClientKeyPinStore(filepath.Dir(f.path))
		got, err := reloaded.ReadPeerClientKeyPinSnapshot(peer)
		if err != nil || !got.Pinned || got.Pin != rotated || !got.SignedHistorySeen {
			t.Fatalf("%s verified signer rotation did not persist: %+v %v", kind, got, err)
		}
	}
}
