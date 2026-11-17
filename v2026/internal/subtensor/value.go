package subtensor

import (
	"errors"
	"fmt"
	"math/big"
	"unicode/utf8"
)

// Decoding values by the metadata's type registry, for what the chain reports
// back: account data, constants and events. Every read on the way to signing
// is covered by the shape checks in shape.go; these only say what happened.
//
// A single-field composite decodes as its field, so AccountId32 is []byte and
// TaoBalance is uint64.

const (
	maxTypeDepth = 64
	// a limit on the work of one decode, whatever the type registry says
	maxDecodeSteps = 1 << 24
)

var errDecodeTooLarge = errors.New("metadata: value is too large to decode")

type valueFields struct {
	names  []string
	values []any
}

func (self *valueFields) get(name string) any {
	for i, candidate := range self.names {
		if candidate == name {
			return self.values[i]
		}
	}
	return nil
}

func (self *valueFields) at(index int) any {
	if index < 0 || len(self.values) <= index {
		return nil
	}
	return self.values[index]
}

type valueVariant struct {
	name   string
	index  uint8
	fields *valueFields
}

type valueDecoder struct {
	meta  *Metadata
	r     *scaleReader
	steps int
}

// decodeValue decodes one value of the type, which has to use all of raw.
func (self *Metadata) decodeValue(raw []byte, id uint32) (any, error) {
	decoder := &valueDecoder{meta: self, r: newScaleReader(raw)}
	value, err := decoder.decode(id, 0)
	if err != nil {
		return nil, err
	}
	if decoder.r.remaining() != 0 {
		return nil, errors.New("metadata: trailing bytes after a value")
	}
	return value, nil
}

func (self *valueDecoder) decode(id uint32, depth int) (any, error) {
	if depth > maxTypeDepth {
		return nil, errors.New("metadata: value nesting too deep")
	}
	self.steps += 1
	if maxDecodeSteps < self.steps {
		return nil, errDecodeTooLarge
	}
	ty, err := self.meta.lookup(id)
	if err != nil {
		return nil, err
	}
	switch ty.kind {
	case typeDefComposite:
		fields, err := self.decodeFields(ty.fields, depth)
		if err != nil {
			return nil, err
		}
		if len(fields.values) == 1 {
			return fields.values[0], nil
		}
		return fields, nil
	case typeDefVariant:
		tag, err := self.r.readU8()
		if err != nil {
			return nil, err
		}
		variant := ty.variantByIndex(tag)
		if variant == nil {
			return nil, fmt.Errorf("metadata: %s has no variant %d", ty.pathName(), tag)
		}
		fields, err := self.decodeFields(variant.fields, depth)
		if err != nil {
			return nil, err
		}
		return &valueVariant{name: variant.name, index: tag, fields: fields}, nil
	case typeDefSequence:
		if self.meta.isPrimitive(ty.elem, primitiveU8) {
			raw, err := self.r.readBytes()
			if err != nil {
				return nil, err
			}
			return append([]byte(nil), raw...), nil
		}
		count, err := self.r.readCount()
		if err != nil {
			return nil, err
		}
		return self.decodeList(ty.elem, count, depth)
	case typeDefArray:
		if self.meta.isPrimitive(ty.elem, primitiveU8) {
			raw, err := self.r.take(int(ty.length))
			if err != nil {
				return nil, err
			}
			return append([]byte(nil), raw...), nil
		}
		if self.r.remaining() < int(ty.length) {
			return nil, errScaleTruncated
		}
		return self.decodeList(ty.elem, int(ty.length), depth)
	case typeDefTuple:
		if len(ty.tuple) == 1 {
			return self.decode(ty.tuple[0], depth+1)
		}
		out := make([]any, len(ty.tuple))
		for i, elem := range ty.tuple {
			out[i], err = self.decode(elem, depth+1)
			if err != nil {
				return nil, err
			}
		}
		return out, nil
	case typeDefPrimitive:
		return self.decodePrimitive(ty.primitive)
	case typeDefCompact:
		value, err := self.r.readCompactBig()
		if err != nil {
			return nil, err
		}
		if value.IsUint64() {
			return value.Uint64(), nil
		}
		return value, nil
	case typeDefBitSequence:
		return nil, self.skipBitSequence(ty)
	default:
		return nil, fmt.Errorf("metadata: type def %d", ty.kind)
	}
}

func (self *valueDecoder) decodeFields(fields []metadataField, depth int) (*valueFields, error) {
	out := &valueFields{}
	for _, field := range fields {
		value, err := self.decode(field.ty, depth+1)
		if err != nil {
			return nil, err
		}
		out.names = append(out.names, field.name)
		out.values = append(out.values, value)
	}
	return out, nil
}

func (self *valueDecoder) decodeList(elem uint32, count int, depth int) ([]any, error) {
	out := make([]any, 0, min(count, 1<<12))
	for range count {
		value, err := self.decode(elem, depth+1)
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, nil
}

func (self *valueDecoder) decodePrimitive(kind primitiveKind) (any, error) {
	width := 0
	switch kind {
	case primitiveBool:
		return self.r.readBool()
	case primitiveChar:
		raw, err := self.r.take(4)
		if err != nil {
			return nil, err
		}
		value := uint32(raw[0]) | uint32(raw[1])<<8 | uint32(raw[2])<<16 | uint32(raw[3])<<24
		if !utf8.ValidRune(rune(value)) {
			return nil, errors.New("metadata: bad char")
		}
		return string(rune(value)), nil
	case primitiveStr:
		return self.r.readString()
	case primitiveU8, primitiveI8:
		width = 1
	case primitiveU16, primitiveI16:
		width = 2
	case primitiveU32, primitiveI32:
		width = 4
	case primitiveU64, primitiveI64:
		width = 8
	case primitiveU128, primitiveI128:
		width = 16
	case primitiveU256, primitiveI256:
		width = 32
	default:
		return nil, fmt.Errorf("metadata: primitive %d", kind)
	}
	raw, err := self.r.take(width)
	if err != nil {
		return nil, err
	}
	reversed := make([]byte, width)
	for i, b := range raw {
		reversed[width-1-i] = b
	}
	value := new(big.Int).SetBytes(reversed)
	if value.IsUint64() {
		return value.Uint64(), nil
	}
	return value, nil
}

func (self *valueDecoder) skipBitSequence(ty *metadataType) error {
	store, err := self.meta.resolve(ty.store)
	if err != nil {
		return err
	}
	storeBytes := 0
	if store.kind == typeDefPrimitive {
		switch store.primitive {
		case primitiveU8:
			storeBytes = 1
		case primitiveU16:
			storeBytes = 2
		case primitiveU32:
			storeBytes = 4
		case primitiveU64:
			storeBytes = 8
		}
	}
	if storeBytes == 0 {
		return errors.New("metadata: bit sequence store")
	}
	bitCount, err := self.r.readCompact()
	if err != nil {
		return err
	}
	storeBits := uint64(storeBytes) * 8
	if uint64(self.r.remaining())*8 < bitCount {
		return errScaleTruncated
	}
	_, err = self.r.take(int((bitCount+storeBits-1)/storeBits) * storeBytes)
	return err
}

func valueUint64(value any) (uint64, bool) {
	number, ok := value.(uint64)
	return number, ok
}

func valueBytes32(value any) ([32]byte, bool) {
	raw, ok := value.([]byte)
	if !ok || len(raw) != 32 {
		return [32]byte{}, false
	}
	return [32]byte(raw), true
}

// constantUint64 reads an integer constant of a pallet.
func (self *Metadata) constantUint64(pallet string, name string) (uint64, error) {
	entry := self.pallets[pallet]
	if entry == nil {
		return 0, &RuntimeChangedError{Detail: fmt.Sprintf("pallet %s is gone", pallet)}
	}
	constant, ok := entry.constants[name]
	if !ok {
		return 0, &RuntimeChangedError{Detail: fmt.Sprintf("constant %s.%s is gone", pallet, name)}
	}
	value, err := self.decodeValue(constant.value, constant.ty)
	if err != nil {
		return 0, &RuntimeChangedError{Detail: fmt.Sprintf("constant %s.%s: %v", pallet, name, err)}
	}
	number, ok := valueUint64(value)
	if !ok {
		return 0, &RuntimeChangedError{Detail: fmt.Sprintf("constant %s.%s is not an integer", pallet, name)}
	}
	return number, nil
}

// ExistentialDeposit is what an account must keep to exist, in rao.
func (self *Metadata) ExistentialDeposit() (uint64, error) {
	return self.constantUint64("Balances", "ExistentialDeposit")
}

// MinStakeRao is the least TAO value a partial unstake may have, in rao: the
// runtime's DefaultMinStake, which is this constant.
func (self *Metadata) MinStakeRao() (uint64, error) {
	return self.constantUint64("SubtensorModule", "InitialMinStake")
}
