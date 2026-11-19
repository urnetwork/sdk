package subtensor

import (
	"encoding/binary"
	"errors"
)

// The calls this client can sign, and there is no other way to make a Call:
// each constructor builds one fixed call from typed arguments, with its pallet
// and call indices compiled in. The runtime's metadata has to agree with them
// (checkCall), so an endpoint that serves altered metadata can make a
// transaction refuse, never make it mean something else. The indices are set
// explicitly in Subtensor's source (`#[pallet::call_index]`, construct_runtime),
// and stay across upgrades.

const (
	palletSubtensorModule = "SubtensorModule"
	palletCommitments     = "Commitments"

	palletIndexSubtensorModule = 7
	palletIndexCommitments     = 18

	callIndexRemoveStakeLimit = 89
	callIndexRegisterLimit    = 134
	callIndexSetCommitment    = 0

	// FleetCommitmentTag starts the one field of a fleet commitment.
	FleetCommitmentTag = "urfleet1"

	// the Data enum's Raw40 (Raw<n> is variant n + 1)
	commitmentDataRaw40 = 41
)

type callField struct {
	name  string
	shape func(*Metadata, uint32) bool
}

// Call is one fixed runtime call with its arguments encoded.
type Call struct {
	pallet      string
	name        string
	palletIndex uint8
	callIndex   uint8
	fields      []callField
	args        []byte
}

func (self *Call) encode() []byte {
	out := make([]byte, 0, 2+len(self.args))
	out = append(out, self.palletIndex, self.callIndex)
	return append(out, self.args...)
}

// Name is "Pallet.call".
func (self *Call) Name() string {
	return self.pallet + "." + self.name
}

func shapeAccount(meta *Metadata, id uint32) bool {
	return meta.isBytes32(id)
}

func shapeU16(meta *Metadata, id uint32) bool {
	return meta.isPrimitive(id, primitiveU16)
}

func shapeU64(meta *Metadata, id uint32) bool {
	return meta.isPrimitive(id, primitiveU64)
}

func shapeBool(meta *Metadata, id uint32) bool {
	return meta.isPrimitive(id, primitiveBool)
}

// shapeFleetCommitmentInfo is CommitmentInfo { fields: BoundedVec<Data> } with
// Data::Raw40 a 40-byte array at the index this client writes.
func shapeFleetCommitmentInfo(meta *Metadata, id uint32) bool {
	info, err := meta.lookup(id)
	if err != nil || info.pathName() != "CommitmentInfo" {
		return false
	}
	sequence, err := meta.resolve(id)
	if err != nil || sequence.kind != typeDefSequence {
		return false
	}
	data, err := meta.resolve(sequence.elem)
	if err != nil || data.kind != typeDefVariant {
		return false
	}
	raw40 := data.variantByName("Raw40")
	if raw40 == nil || raw40.index != commitmentDataRaw40 || len(raw40.fields) != 1 {
		return false
	}
	bytes40, err := meta.resolve(raw40.fields[0].ty)
	return err == nil && bytes40.kind == typeDefArray && bytes40.length == 40 && meta.isPrimitive(bytes40.elem, primitiveU8)
}

// NewRemoveStakeLimit is SubtensorModule.remove_stake_limit(hotkey, netuid,
// amount_unstaked, limit_price, allow_partial = false): sell this much alpha
// for TAO to the signing coldkey, all of it or none, with no sale below the
// limit price in rao per alpha.
func NewRemoveStakeLimit(hotkey [32]byte, netuid uint16, amountRao uint64, limitPriceRao uint64) (*Call, error) {
	if hotkey == ([32]byte{}) || netuid == 0 || amountRao == 0 || limitPriceRao == 0 {
		return nil, errors.New("remove_stake_limit: the hotkey, subnet, amount and limit price are all required")
	}
	args := make([]byte, 0, 32+2+8+8+1)
	args = append(args, hotkey[:]...)
	args = binary.LittleEndian.AppendUint16(args, netuid)
	args = binary.LittleEndian.AppendUint64(args, amountRao)
	args = binary.LittleEndian.AppendUint64(args, limitPriceRao)
	args = append(args, 0)
	return &Call{
		pallet:      palletSubtensorModule,
		name:        "remove_stake_limit",
		palletIndex: palletIndexSubtensorModule,
		callIndex:   callIndexRemoveStakeLimit,
		fields: []callField{
			{name: "hotkey", shape: shapeAccount},
			{name: "netuid", shape: shapeU16},
			{name: "amount_unstaked", shape: shapeU64},
			{name: "limit_price", shape: shapeU64},
			{name: "allow_partial", shape: shapeBool},
		},
		args: args,
	}, nil
}

// NewRegisterLimit is SubtensorModule.register_limit(netuid, hotkey,
// limit_price): burn-register the hotkey on the subnet, signed by its coldkey,
// if the burn is at most the limit in rao.
func NewRegisterLimit(netuid uint16, hotkey [32]byte, limitRao uint64) (*Call, error) {
	if hotkey == ([32]byte{}) || netuid == 0 || limitRao == 0 {
		return nil, errors.New("register_limit: the subnet, hotkey and limit are all required")
	}
	args := make([]byte, 0, 2+32+8)
	args = binary.LittleEndian.AppendUint16(args, netuid)
	args = append(args, hotkey[:]...)
	args = binary.LittleEndian.AppendUint64(args, limitRao)
	return &Call{
		pallet:      palletSubtensorModule,
		name:        "register_limit",
		palletIndex: palletIndexSubtensorModule,
		callIndex:   callIndexRegisterLimit,
		fields: []callField{
			{name: "netuid", shape: shapeU16},
			{name: "hotkey", shape: shapeAccount},
			{name: "limit_price", shape: shapeU64},
		},
		args: args,
	}, nil
}

// NewSetFleetCommitment is Commitments.set_commitment(netuid, info) with the
// one thing a fleet commits: one Raw40 field, FleetCommitmentTag then the
// 32-byte hash. Signed by the hotkey.
func NewSetFleetCommitment(netuid uint16, hash [32]byte) (*Call, error) {
	if hash == ([32]byte{}) || netuid == 0 {
		return nil, errors.New("set_commitment: the subnet and hash are required")
	}
	args := make([]byte, 0, 2+2+len(FleetCommitmentTag)+32)
	args = binary.LittleEndian.AppendUint16(args, netuid)
	args = appendCompact(args, 1)
	args = append(args, commitmentDataRaw40)
	args = append(args, FleetCommitmentTag...)
	args = append(args, hash[:]...)
	return &Call{
		pallet:      palletCommitments,
		name:        "set_commitment",
		palletIndex: palletIndexCommitments,
		callIndex:   callIndexSetCommitment,
		fields: []callField{
			{name: "netuid", shape: shapeU16},
			{name: "info", shape: shapeFleetCommitmentInfo},
		},
		args: args,
	}, nil
}
