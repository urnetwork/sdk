// Package protocol carries the client half of the UR subnet fleet binding
// (WHITEPAPER §11.4): the domain-separated payload, its keccak256 digest and
// the Ed25519 client signature. A device only ever signs with its own client
// key, and checks the hotkey's signature (fleet_authorization.go) without ever
// holding the hotkey.
package protocol

import (
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"fmt"

	"golang.org/x/crypto/sha3"
)

const FleetBindingDomain = "urnetwork/fleet-binding/v1"

const FleetRevokeDomain = "urnetwork/fleet-revoke/v1"

// FleetBinding is the dual-signed consent that attaches a provider client to
// a head-miner fleet for a generation and epoch range.
type FleetBinding struct {
	ChainID        uint64
	Netuid         uint16
	Coordinator    [20]byte
	FleetID        [32]byte
	Hotkey         [32]byte
	ClientID       [16]byte
	ClientKey      [32]byte
	Generation     uint64
	ValidFromEpoch uint64
	ValidToEpoch   uint64
	CommitmentHash [32]byte
}

const FleetBindingPayloadSize = len(FleetBindingDomain) + 8 + 2 + 20 + 32 + 32 + 16 + 32 + 8 + 8 + 8 + 32

func keccak256(data []byte) [32]byte {
	h := sha3.NewLegacyKeccak256()
	h.Write(data)
	var out [32]byte
	h.Sum(out[:0])
	return out
}

// Validate rejects zero identities and an invalid validity window.
func (b FleetBinding) Validate() error {
	if b.ChainID == 0 || b.Netuid == 0 {
		return errors.New("binding chain_id/netuid is zero")
	}
	if b.Coordinator == ([20]byte{}) || b.FleetID == ([32]byte{}) || b.Hotkey == ([32]byte{}) || b.ClientID == ([16]byte{}) || b.ClientKey == ([32]byte{}) || b.CommitmentHash == ([32]byte{}) {
		return errors.New("binding contains a zero identity/hash")
	}
	if b.Generation == 0 || b.ValidToEpoch < b.ValidFromEpoch {
		return errors.New("binding generation/validity is invalid")
	}
	return nil
}

// Payload is the exact byte string both parties sign (through its digest).
func (b FleetBinding) Payload() ([]byte, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	out := make([]byte, 0, FleetBindingPayloadSize)
	out = append(out, FleetBindingDomain...)
	var u64 [8]byte
	binary.BigEndian.PutUint64(u64[:], b.ChainID)
	out = append(out, u64[:]...)
	var u16 [2]byte
	binary.BigEndian.PutUint16(u16[:], b.Netuid)
	out = append(out, u16[:]...)
	out = append(out, b.Coordinator[:]...)
	out = append(out, b.FleetID[:]...)
	out = append(out, b.Hotkey[:]...)
	out = append(out, b.ClientID[:]...)
	out = append(out, b.ClientKey[:]...)
	binary.BigEndian.PutUint64(u64[:], b.Generation)
	out = append(out, u64[:]...)
	binary.BigEndian.PutUint64(u64[:], b.ValidFromEpoch)
	out = append(out, u64[:]...)
	binary.BigEndian.PutUint64(u64[:], b.ValidToEpoch)
	out = append(out, u64[:]...)
	out = append(out, b.CommitmentHash[:]...)
	if len(out) != FleetBindingPayloadSize {
		return nil, fmt.Errorf("binding payload length %d, want %d", len(out), FleetBindingPayloadSize)
	}
	return out, nil
}

// Digest is keccak256(Payload).
func (b FleetBinding) Digest() ([32]byte, error) {
	payload, err := b.Payload()
	if err != nil {
		return [32]byte{}, err
	}
	return keccak256(payload), nil
}

// SignClient signs the digest with the client's Ed25519 key, which must be
// the key named in the binding.
func (b FleetBinding) SignClient(privateKey ed25519.PrivateKey) ([]byte, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid Ed25519 private key")
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	if string(publicKey) != string(b.ClientKey[:]) {
		return nil, errors.New("client private key does not match binding")
	}
	digest, err := b.Digest()
	if err != nil {
		return nil, err
	}
	return ed25519.Sign(privateKey, digest[:]), nil
}

// VerifyClient checks a client signature against the binding's client key.
func (b FleetBinding) VerifyClient(signature []byte) bool {
	digest, err := b.Digest()
	return err == nil && ed25519.Verify(ed25519.PublicKey(b.ClientKey[:]), digest[:], signature)
}

// FleetRevoke is the client's consent to end one binding generation at a future
// epoch. The coordinator resolves the client key from the generation being
// revoked, so the key is not part of the digest.
type FleetRevoke struct {
	ChainID        uint64
	Netuid         uint16
	Coordinator    [20]byte
	ClientID       [16]byte
	Generation     uint64
	EffectiveEpoch uint64
}

const FleetRevokePayloadSize = len(FleetRevokeDomain) + 8 + 2 + 20 + 16 + 8 + 8

// Validate rejects zero coordinates before any signature can be made.
func (self FleetRevoke) Validate() error {
	if self.ChainID == 0 || self.Netuid == 0 {
		return errors.New("fleet revoke chain_id/netuid is zero")
	}
	if self.Coordinator == ([20]byte{}) || self.ClientID == ([16]byte{}) {
		return errors.New("fleet revoke contains a zero identity")
	}
	if self.Generation == 0 || self.EffectiveEpoch == 0 {
		return errors.New("fleet revoke generation/effective epoch is zero")
	}
	return nil
}

// Payload is the exact byte string the digest covers.
func (self FleetRevoke) Payload() ([]byte, error) {
	if err := self.Validate(); err != nil {
		return nil, err
	}
	out := make([]byte, 0, FleetRevokePayloadSize)
	out = append(out, FleetRevokeDomain...)
	var u64 [8]byte
	binary.BigEndian.PutUint64(u64[:], self.ChainID)
	out = append(out, u64[:]...)
	var u16 [2]byte
	binary.BigEndian.PutUint16(u16[:], self.Netuid)
	out = append(out, u16[:]...)
	out = append(out, self.Coordinator[:]...)
	out = append(out, self.ClientID[:]...)
	binary.BigEndian.PutUint64(u64[:], self.Generation)
	out = append(out, u64[:]...)
	binary.BigEndian.PutUint64(u64[:], self.EffectiveEpoch)
	out = append(out, u64[:]...)
	if len(out) != FleetRevokePayloadSize {
		return nil, fmt.Errorf("fleet revoke payload length %d, want %d", len(out), FleetRevokePayloadSize)
	}
	return out, nil
}

// Digest is keccak256(Payload), the coordinator's fleetRevokeDigest.
func (self FleetRevoke) Digest() ([32]byte, error) {
	payload, err := self.Payload()
	if err != nil {
		return [32]byte{}, err
	}
	return keccak256(payload), nil
}

// SignClient signs the digest with the client's Ed25519 key.
func (self FleetRevoke) SignClient(privateKey ed25519.PrivateKey) ([]byte, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid Ed25519 private key")
	}
	digest, err := self.Digest()
	if err != nil {
		return nil, err
	}
	return ed25519.Sign(privateKey, digest[:]), nil
}

// VerifyClient checks a revocation against the key the binding record holds.
func (self FleetRevoke) VerifyClient(publicKey ed25519.PublicKey, signature []byte) bool {
	digest, err := self.Digest()
	return err == nil && len(publicKey) == ed25519.PublicKeySize && ed25519.Verify(publicKey, digest[:], signature)
}
