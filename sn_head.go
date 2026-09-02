package sdk

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk/sn/evm"
	"github.com/urnetwork/sdk/sn/protocol"
)

// Points history

// AccountEpoch is one finalized epoch of the network's points history.
type AccountEpoch struct {
	Epoch       int64   `json:"epoch"`
	StartMillis int64   `json:"start_millis"`
	EndMillis   int64   `json:"end_millis"`
	Points      float64 `json:"points"`
	// the network's share of the block in the payout artifact (0 = none)
	ShareBps int64 `json:"share_bps"`
	// leaderboard rank for the epoch when the server reports it
	Rank int64 `json:"rank,omitempty"`
}

type AccountEpochList struct {
	exportedList[*AccountEpoch]
}

func NewAccountEpochList() *AccountEpochList {
	return &AccountEpochList{
		exportedList: *newExportedList[*AccountEpoch](),
	}
}

type AccountEpochsResult struct {
	Epochs      *AccountEpochList `json:"epochs"`
	TotalPoints float64           `json:"total_points,omitempty"`
	Error       *SnError          `json:"error,omitempty"`
}

type AccountEpochsCallback connect.ApiCallback[*AccountEpochsResult]

// AccountEpochs reads the per-epoch points history (GET /account/epochs).
func (self *Api) AccountEpochs(callback AccountEpochsCallback) {
	go connect.HandleError(func() {
		connect.HttpGetWithRawFunction(
			self.ctx,
			self.getHttpGetRaw(),
			fmt.Sprintf("%s/account/epochs", self.apiUrl),
			self.GetByJwt(),
			&AccountEpochsResult{},
			callback,
		)
	})
}

// Head spot (Top 200)

// SnHeadResult is the network's head-miner eligibility (GET /sn/head).
type SnHeadResult struct {
	Eligible     bool    `json:"eligible"`
	Score        float64 `json:"score"`
	Floor        float64 `json:"floor"`
	RankEstimate int64   `json:"rank_estimate"`
	Cutoff       int64   `json:"cutoff"`
	// a hotkey is bound to this network's fleet
	Bound  bool   `json:"bound"`
	Hotkey string `json:"hotkey,omitempty"`
	Uid    int64  `json:"uid,omitempty"`
	Rank   int64  `json:"rank,omitempty"`
	Epoch  int64  `json:"epoch"`
	// "server" (estimate) or "chain" (validator consensus)
	Source string   `json:"source"`
	Error  *SnError `json:"error,omitempty"`
}

type snHeadResultJson struct {
	Eligible     bool            `json:"eligible"`
	Score        float64         `json:"score"`
	Floor        float64         `json:"floor"`
	RankEstimate int64           `json:"rank_estimate"`
	Cutoff       int64           `json:"cutoff"`
	Bound        json.RawMessage `json:"bound"`
	Hotkey       string          `json:"hotkey"`
	Uid          int64           `json:"uid"`
	Rank         int64           `json:"rank"`
	Epoch        int64           `json:"epoch"`
	Source       string          `json:"source"`
	Error        *SnError        `json:"error"`
}

// UnmarshalJSON accepts `bound` either as a bool or as an object
// {hotkey, uid} (the server's richer form).
func (self *SnHeadResult) UnmarshalJSON(b []byte) error {
	var j snHeadResultJson
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	*self = SnHeadResult{
		Eligible:     j.Eligible,
		Score:        j.Score,
		Floor:        j.Floor,
		RankEstimate: j.RankEstimate,
		Cutoff:       j.Cutoff,
		Hotkey:       j.Hotkey,
		Uid:          j.Uid,
		Rank:         j.Rank,
		Epoch:        j.Epoch,
		Source:       j.Source,
		Error:        j.Error,
	}
	if len(j.Bound) > 0 && string(j.Bound) != "null" {
		var flag bool
		if err := json.Unmarshal(j.Bound, &flag); err == nil {
			self.Bound = flag
		} else {
			var bound struct {
				Hotkey string `json:"hotkey"`
				Uid    int64  `json:"uid"`
				Rank   int64  `json:"rank"`
			}
			if err := json.Unmarshal(j.Bound, &bound); err != nil {
				return err
			}
			self.Bound = bound.Hotkey != "" || bound.Uid > 0
			if self.Hotkey == "" {
				self.Hotkey = bound.Hotkey
			}
			if self.Uid == 0 {
				self.Uid = bound.Uid
			}
			if self.Rank == 0 {
				self.Rank = bound.Rank
			}
		}
	}
	return nil
}

type SnHeadCallback connect.ApiCallback[*SnHeadResult]

// SnHead reads the head-spot eligibility (GET /sn/head).
func (self *Api) SnHead(callback SnHeadCallback) {
	go connect.HandleError(func() {
		connect.HttpGetWithRawFunction(
			self.ctx,
			self.getHttpGetRaw(),
			fmt.Sprintf("%s/sn/head", self.apiUrl),
			self.GetByJwt(),
			&SnHeadResult{},
			callback,
		)
	})
}

// Fleet binding

// snFlexBytes accepts hex strings ("0x…" or bare), a uuid string (client
// ids) or a json array of byte values.
type snFlexBytes []byte

func (self *snFlexBytes) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		s = strings.TrimSpace(s)
		if len(s) == 36 && strings.Count(s, "-") == 4 {
			id, err := ParseId(s)
			if err != nil {
				return err
			}
			raw := id.toConnectId()
			*self = append([]byte(nil), raw[:]...)
			return nil
		}
		raw, err := evm.ParseHexBytes(s)
		if err != nil {
			return err
		}
		*self = raw
		return nil
	}
	var values []int
	if err := json.Unmarshal(b, &values); err != nil {
		return err
	}
	out := make([]byte, len(values))
	for i, v := range values {
		if v < 0 || v > 255 {
			return errors.New("byte array value out of range")
		}
		out[i] = byte(v)
	}
	*self = out
	return nil
}

// snFlexUint accepts a json number or a decimal/hex string.
type snFlexUint uint64

func (self *snFlexUint) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		v, err := evm.ParseUint256(s)
		if err != nil {
			return err
		}
		if !v.IsUint64() {
			return errors.New("value overflows uint64")
		}
		*self = snFlexUint(v.Uint64())
		return nil
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return err
	}
	*self = snFlexUint(v)
	return nil
}

type snFleetBindingJson struct {
	ChainId        snFlexUint  `json:"chain_id"`
	Netuid         snFlexUint  `json:"netuid"`
	Coordinator    snFlexBytes `json:"coordinator"`
	FleetId        snFlexBytes `json:"fleet_id"`
	Hotkey         snFlexBytes `json:"hotkey"`
	ClientId       snFlexBytes `json:"client_id"`
	ClientKey      snFlexBytes `json:"client_key"`
	Generation     snFlexUint  `json:"generation"`
	ValidFromEpoch snFlexUint  `json:"valid_from_epoch"`
	ValidToEpoch   snFlexUint  `json:"valid_to_epoch"`
	CommitmentHash snFlexBytes `json:"commitment_hash"`
}

func snParseFleetBinding(bindingJson string) (*protocol.FleetBinding, error) {
	var j snFleetBindingJson
	if err := json.Unmarshal([]byte(bindingJson), &j); err != nil {
		return nil, fmt.Errorf("binding json: %w", err)
	}
	if j.Netuid > 0xffff {
		return nil, errors.New("netuid out of range")
	}
	b := &protocol.FleetBinding{
		ChainID:        uint64(j.ChainId),
		Netuid:         uint16(j.Netuid),
		Generation:     uint64(j.Generation),
		ValidFromEpoch: uint64(j.ValidFromEpoch),
		ValidToEpoch:   uint64(j.ValidToEpoch),
	}
	fill := func(name string, dst []byte, src snFlexBytes) error {
		if len(src) != len(dst) {
			return fmt.Errorf("binding %s must be %d bytes, got %d", name, len(dst), len(src))
		}
		copy(dst, src)
		return nil
	}
	if err := fill("coordinator", b.Coordinator[:], j.Coordinator); err != nil {
		return nil, err
	}
	if err := fill("fleet_id", b.FleetID[:], j.FleetId); err != nil {
		return nil, err
	}
	if err := fill("hotkey", b.Hotkey[:], j.Hotkey); err != nil {
		return nil, err
	}
	if err := fill("client_id", b.ClientID[:], j.ClientId); err != nil {
		return nil, err
	}
	if err := fill("client_key", b.ClientKey[:], j.ClientKey); err != nil {
		return nil, err
	}
	if err := fill("commitment_hash", b.CommitmentHash[:], j.CommitmentHash); err != nil {
		return nil, err
	}
	if err := b.Validate(); err != nil {
		return nil, err
	}
	return b, nil
}

// SnFleetBindingDigest returns the keccak256 digest (0x hex) a binding is
// signed over, for hosts that verify or display it.
func SnFleetBindingDigest(bindingJson string) (string, error) {
	b, err := snParseFleetBinding(bindingJson)
	if err != nil {
		return "", err
	}
	digest, err := b.Digest()
	if err != nil {
		return "", err
	}
	return "0x" + hex.EncodeToString(digest[:]), nil
}

func (self *snDevice) snClientPrivateKey() (ed25519.PrivateKey, error) {
	if self.clientKeySeed == nil {
		return nil, errors.New("client key unavailable")
	}
	seed := self.clientKeySeed()
	if len(seed) != ed25519.SeedSize {
		return nil, errors.New("client key unavailable")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// GetSnClientKey returns this device's Ed25519 client public key (0x hex),
// the `client_key` the server puts in a fleet binding for this client.
func (self *snDevice) GetSnClientKey() string {
	key, err := self.snClientPrivateKey()
	if err != nil {
		return ""
	}
	return "0x" + hex.EncodeToString(key.Public().(ed25519.PublicKey))
}

// SignSnFleetBinding signs a fleet binding (json, fields per WHITEPAPER
// §11.4: chain_id, netuid, coordinator, fleet_id, hotkey, client_id,
// client_key, generation, valid_from_epoch, valid_to_epoch,
// commitment_hash) with this device's client key. The binding must name
// this client and its key. Returns the 64-byte signature as 0x hex.
func (self *snDevice) SignSnFleetBinding(bindingJson string) (string, error) {
	b, err := snParseFleetBinding(bindingJson)
	if err != nil {
		return "", err
	}
	if b.ClientID != [16]byte(self.clientId) {
		return "", errors.New("binding client_id is not this device")
	}
	key, err := self.snClientPrivateKey()
	if err != nil {
		return "", err
	}
	signature, err := b.SignClient(key)
	if err != nil {
		return "", err
	}
	return "0x" + hex.EncodeToString(signature), nil
}
