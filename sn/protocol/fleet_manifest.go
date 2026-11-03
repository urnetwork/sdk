package protocol

// The fleet manifest (sn protocol/fleet.go): the member set whose SHA-256 the
// fleet hotkey publishes as its commitment. This copy lets a key holder's device
// recompute what it publishes and signs from the manifest bytes alone, never
// from a hash the server names. The golden in sn protocol holds the two together.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const FleetManifestSchema = "urnetwork-fleet-manifest-v1"

// FleetMember is one independently keyed client in a fleet.
type FleetMember struct {
	ClientID  [16]byte
	ClientKey [32]byte
}

// FleetManifest is the immutable member set of one generation. Epoch validity
// lives in each binding, not here.
type FleetManifest struct {
	Schema      string
	ChainID     uint64
	Netuid      uint16
	Coordinator [20]byte
	FleetID     [32]byte
	Hotkey      [32]byte
	Generation  uint64
	Members     []FleetMember
}

type fleetManifestJson struct {
	Schema      string `json:"schema"`
	ChainID     uint64 `json:"chain_id"`
	Netuid      uint16 `json:"netuid"`
	Coordinator string `json:"coordinator"`
	FleetID     string `json:"fleet_id"`
	Hotkey      string `json:"hotkey"`
	Generation  uint64 `json:"generation"`
	Members     []struct {
		ClientID  string `json:"client_id"`
		ClientKey string `json:"client_key"`
	} `json:"members"`
}

// Validate requires a whole identity, at least one member, and no client id or
// client key used twice. So two proxy slots of one host, which share a key, can't
// both be members of one manifest.
func (self FleetManifest) Validate() error {
	if self.Schema != FleetManifestSchema {
		return fmt.Errorf("fleet manifest schema %q, want %q", self.Schema, FleetManifestSchema)
	}
	if self.ChainID == 0 || self.Netuid == 0 || self.Coordinator == ([20]byte{}) || self.FleetID == ([32]byte{}) || self.Hotkey == ([32]byte{}) || self.Generation == 0 {
		return errors.New("fleet manifest contains a zero chain/identity/generation")
	}
	if len(self.Members) == 0 {
		return errors.New("fleet manifest has no members")
	}
	ids := map[[16]byte]bool{}
	keys := map[[32]byte]bool{}
	for _, member := range self.Members {
		if member.ClientID == ([16]byte{}) || member.ClientKey == ([32]byte{}) {
			return errors.New("fleet manifest contains a zero member identity/key")
		}
		if ids[member.ClientID] || keys[member.ClientKey] {
			return errors.New("fleet manifest reuses a client identity/key")
		}
		ids[member.ClientID] = true
		keys[member.ClientKey] = true
	}
	return nil
}

// Canonical is the deterministic json the commitment hashes, members ordered by
// client id.
func (self FleetManifest) Canonical() ([]byte, error) {
	if err := self.Validate(); err != nil {
		return nil, err
	}
	members := append([]FleetMember(nil), self.Members...)
	sort.Slice(members, func(i, j int) bool {
		return string(members[i].ClientID[:]) < string(members[j].ClientID[:])
	})
	out := fleetManifestJson{
		Schema:      self.Schema,
		ChainID:     self.ChainID,
		Netuid:      self.Netuid,
		Coordinator: "0x" + hex.EncodeToString(self.Coordinator[:]),
		FleetID:     "0x" + hex.EncodeToString(self.FleetID[:]),
		Hotkey:      "0x" + hex.EncodeToString(self.Hotkey[:]),
		Generation:  self.Generation,
	}
	for _, member := range members {
		out.Members = append(out.Members, struct {
			ClientID  string `json:"client_id"`
			ClientKey string `json:"client_key"`
		}{ClientID: "0x" + hex.EncodeToString(member.ClientID[:]), ClientKey: "0x" + hex.EncodeToString(member.ClientKey[:])})
	}
	return json.Marshal(out)
}

// CommitmentHash is the SHA-256 the hotkey publishes.
func (self FleetManifest) CommitmentHash() ([32]byte, error) {
	canonical, err := self.Canonical()
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(canonical), nil
}

// Binding is the canonical binding of one member over an epoch window.
func (self FleetManifest) Binding(member FleetMember, validFromEpoch uint64, validToEpoch uint64) (FleetBinding, error) {
	hash, err := self.CommitmentHash()
	if err != nil {
		return FleetBinding{}, err
	}
	found := false
	for _, candidate := range self.Members {
		if candidate == member {
			found = true
			break
		}
	}
	if !found {
		return FleetBinding{}, errors.New("fleet binding member is not in the committed manifest")
	}
	binding := FleetBinding{
		ChainID:        self.ChainID,
		Netuid:         self.Netuid,
		Coordinator:    self.Coordinator,
		FleetID:        self.FleetID,
		Hotkey:         self.Hotkey,
		ClientID:       member.ClientID,
		ClientKey:      member.ClientKey,
		Generation:     self.Generation,
		ValidFromEpoch: validFromEpoch,
		ValidToEpoch:   validToEpoch,
		CommitmentHash: hash,
	}
	if err := binding.Validate(); err != nil {
		return FleetBinding{}, err
	}
	return binding, nil
}

func fleetManifestFixedHex(name string, value string, out []byte) error {
	raw, err := hex.DecodeString(strings.TrimPrefix(value, "0x"))
	if err != nil || len(raw) != len(out) || value != "0x"+hex.EncodeToString(raw) {
		return fmt.Errorf("%s must be %d-byte lowercase 0x hex", name, len(out))
	}
	copy(out, raw)
	return nil
}

// ParseFleetManifest accepts only the canonical bytes: unknown fields, other
// orderings and other hex spellings are refused, so what is hashed is what was
// read.
func ParseFleetManifest(raw []byte) (*FleetManifest, error) {
	var in fleetManifestJson
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&in); err != nil {
		return nil, fmt.Errorf("decode fleet manifest: %w", err)
	}
	if decoder.More() {
		return nil, errors.New("decode fleet manifest: multiple json values")
	}
	out := &FleetManifest{Schema: in.Schema, ChainID: in.ChainID, Netuid: in.Netuid, Generation: in.Generation}
	if err := fleetManifestFixedHex("coordinator", in.Coordinator, out.Coordinator[:]); err != nil {
		return nil, err
	}
	if err := fleetManifestFixedHex("fleet_id", in.FleetID, out.FleetID[:]); err != nil {
		return nil, err
	}
	if err := fleetManifestFixedHex("hotkey", in.Hotkey, out.Hotkey[:]); err != nil {
		return nil, err
	}
	for i, member := range in.Members {
		var parsed FleetMember
		if err := fleetManifestFixedHex(fmt.Sprintf("members[%d].client_id", i), member.ClientID, parsed.ClientID[:]); err != nil {
			return nil, err
		}
		if err := fleetManifestFixedHex(fmt.Sprintf("members[%d].client_key", i), member.ClientKey, parsed.ClientKey[:]); err != nil {
			return nil, err
		}
		out.Members = append(out.Members, parsed)
	}
	canonical, err := out.Canonical()
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(raw, canonical) {
		return nil, errors.New("fleet manifest is valid but not canonical (members/order/whitespace differ)")
	}
	return out, nil
}
