package subtensor

import (
	"errors"
	"fmt"
)

// A runtime's V14 metadata, as far as this package reads it: the type
// registry, each pallet's calls, events, errors, constants and storage types,
// and the extrinsic's signed extensions. Docs are skipped.

const (
	metadataMagic     = "meta"
	metadataVersion14 = 14
)

type typeDefKind uint8

// In the order of scale-info's TypeDef.
const (
	typeDefComposite typeDefKind = iota
	typeDefVariant
	typeDefSequence
	typeDefArray
	typeDefTuple
	typeDefPrimitive
	typeDefCompact
	typeDefBitSequence
)

type primitiveKind uint8

// In the order of scale-info's TypeDefPrimitive.
const (
	primitiveBool primitiveKind = iota
	primitiveChar
	primitiveStr
	primitiveU8
	primitiveU16
	primitiveU32
	primitiveU64
	primitiveU128
	primitiveU256
	primitiveI8
	primitiveI16
	primitiveI32
	primitiveI64
	primitiveI128
	primitiveI256
)

type metadataField struct {
	name string
	ty   uint32
}

type metadataVariant struct {
	name   string
	index  uint8
	fields []metadataField
}

type metadataTypeParam struct {
	name    string
	ty      uint32
	hasType bool
}

type metadataType struct {
	path   []string
	params []metadataTypeParam
	kind   typeDefKind

	fields    []metadataField   // composite
	variants  []metadataVariant // variant
	elem      uint32            // sequence, array, compact
	length    uint32            // array
	tuple     []uint32          // tuple
	primitive primitiveKind     // primitive
	store     uint32            // bit sequence store
}

func (self *metadataType) pathName() string {
	if len(self.path) == 0 {
		return ""
	}
	return self.path[len(self.path)-1]
}

func (self *metadataType) variantByName(name string) *metadataVariant {
	for i := range self.variants {
		if self.variants[i].name == name {
			return &self.variants[i]
		}
	}
	return nil
}

func (self *metadataType) variantByIndex(index uint8) *metadataVariant {
	for i := range self.variants {
		if self.variants[i].index == index {
			return &self.variants[i]
		}
	}
	return nil
}

func (self *metadataType) param(name string) (uint32, bool) {
	for _, param := range self.params {
		if param.name == name && param.hasType {
			return param.ty, true
		}
	}
	return 0, false
}

type metadataConstant struct {
	ty    uint32
	value []byte
}

type metadataStorageEntry struct {
	// the value's type, for a plain value and for a map's values
	valueType uint32
	// the hashers of a map; none for a plain value
	hashers []uint8
	keyType uint32
	// the runtime's value when the key is absent; empty for an optional entry
	optional bool
	fallback []byte
}

type metadataPallet struct {
	name      string
	index     uint8
	hasCalls  bool
	calls     uint32
	hasEvents bool
	events    uint32
	hasErrors bool
	errors    uint32
	constants map[string]metadataConstant
	storage   map[string]metadataStorageEntry
}

type metadataExtension struct {
	identifier string
	ty         uint32
	additional uint32
}

// Metadata is one runtime's parsed metadata.
type Metadata struct {
	types            map[uint32]*metadataType
	pallets          map[string]*metadataPallet
	palletsByIndex   map[uint8]*metadataPallet
	extrinsicType    uint32
	extrinsicVersion uint8
	extensions       []metadataExtension
}

// ParseMetadata reads the bytes of state_getMetadata. The whole input has to
// be one well-formed V14 metadata.
func ParseMetadata(raw []byte) (*Metadata, error) {
	r := newScaleReader(raw)
	magic, err := r.take(len(metadataMagic))
	if err != nil {
		return nil, err
	}
	if string(magic) != metadataMagic {
		return nil, errors.New("metadata: bad magic")
	}
	version, err := r.readU8()
	if err != nil {
		return nil, err
	}
	if version != metadataVersion14 {
		return nil, fmt.Errorf("metadata: version %d, this client reads %d", version, metadataVersion14)
	}
	m := &Metadata{
		types:          map[uint32]*metadataType{},
		pallets:        map[string]*metadataPallet{},
		palletsByIndex: map[uint8]*metadataPallet{},
	}
	if err := m.readTypes(r); err != nil {
		return nil, fmt.Errorf("metadata types: %w", err)
	}
	if err := m.readPallets(r); err != nil {
		return nil, fmt.Errorf("metadata pallets: %w", err)
	}
	if err := m.readExtrinsic(r); err != nil {
		return nil, fmt.Errorf("metadata extrinsic: %w", err)
	}
	// the runtime type
	if _, err := r.readCompact(); err != nil {
		return nil, err
	}
	if r.remaining() != 0 {
		return nil, errors.New("metadata: trailing bytes")
	}
	return m, nil
}

func (self *Metadata) readTypes(r *scaleReader) error {
	count, err := r.readCount()
	if err != nil {
		return err
	}
	for range count {
		id, err := readTypeId(r)
		if err != nil {
			return err
		}
		ty, err := readType(r)
		if err != nil {
			return fmt.Errorf("type %d: %w", id, err)
		}
		self.types[id] = ty
	}
	return nil
}

func readTypeId(r *scaleReader) (uint32, error) {
	id, err := r.readCompact()
	if err != nil {
		return 0, err
	}
	if id > 1<<31 {
		return 0, errScaleOverflow
	}
	return uint32(id), nil
}

func readType(r *scaleReader) (*metadataType, error) {
	ty := &metadataType{}
	pathCount, err := r.readCount()
	if err != nil {
		return nil, err
	}
	for range pathCount {
		segment, err := r.readString()
		if err != nil {
			return nil, err
		}
		ty.path = append(ty.path, segment)
	}
	paramCount, err := r.readCount()
	if err != nil {
		return nil, err
	}
	for range paramCount {
		name, err := r.readString()
		if err != nil {
			return nil, err
		}
		param := metadataTypeParam{name: name}
		param.hasType, err = r.readOption()
		if err != nil {
			return nil, err
		}
		if param.hasType {
			param.ty, err = readTypeId(r)
			if err != nil {
				return nil, err
			}
		}
		ty.params = append(ty.params, param)
	}
	if err := readTypeDef(r, ty); err != nil {
		return nil, err
	}
	return ty, r.skipStrings()
}

func readTypeDef(r *scaleReader, ty *metadataType) error {
	kind, err := r.readU8()
	if err != nil {
		return err
	}
	ty.kind = typeDefKind(kind)
	switch ty.kind {
	case typeDefComposite:
		ty.fields, err = readFields(r)
		return err
	case typeDefVariant:
		count, err := r.readCount()
		if err != nil {
			return err
		}
		for range count {
			variant := metadataVariant{}
			variant.name, err = r.readString()
			if err != nil {
				return err
			}
			variant.fields, err = readFields(r)
			if err != nil {
				return err
			}
			variant.index, err = r.readU8()
			if err != nil {
				return err
			}
			if err := r.skipStrings(); err != nil {
				return err
			}
			ty.variants = append(ty.variants, variant)
		}
		return nil
	case typeDefSequence, typeDefCompact:
		ty.elem, err = readTypeId(r)
		return err
	case typeDefArray:
		raw, err := r.take(4)
		if err != nil {
			return err
		}
		ty.length = uint32(raw[0]) | uint32(raw[1])<<8 | uint32(raw[2])<<16 | uint32(raw[3])<<24
		ty.elem, err = readTypeId(r)
		return err
	case typeDefTuple:
		count, err := r.readCount()
		if err != nil {
			return err
		}
		for range count {
			id, err := readTypeId(r)
			if err != nil {
				return err
			}
			ty.tuple = append(ty.tuple, id)
		}
		return nil
	case typeDefPrimitive:
		primitive, err := r.readU8()
		if err != nil {
			return err
		}
		if primitive > uint8(primitiveI256) {
			return fmt.Errorf("primitive %d", primitive)
		}
		ty.primitive = primitiveKind(primitive)
		return nil
	case typeDefBitSequence:
		ty.store, err = readTypeId(r)
		if err != nil {
			return err
		}
		_, err = readTypeId(r)
		return err
	default:
		return fmt.Errorf("type def %d", kind)
	}
}

func readFields(r *scaleReader) ([]metadataField, error) {
	count, err := r.readCount()
	if err != nil {
		return nil, err
	}
	fields := make([]metadataField, 0, count)
	for range count {
		field := metadataField{}
		hasName, err := r.readOption()
		if err != nil {
			return nil, err
		}
		if hasName {
			field.name, err = r.readString()
			if err != nil {
				return nil, err
			}
		}
		field.ty, err = readTypeId(r)
		if err != nil {
			return nil, err
		}
		hasTypeName, err := r.readOption()
		if err != nil {
			return nil, err
		}
		if hasTypeName {
			if _, err := r.readBytes(); err != nil {
				return nil, err
			}
		}
		if err := r.skipStrings(); err != nil {
			return nil, err
		}
		fields = append(fields, field)
	}
	return fields, nil
}

func readOptionalTypeId(r *scaleReader) (uint32, bool, error) {
	present, err := r.readOption()
	if err != nil || !present {
		return 0, false, err
	}
	id, err := readTypeId(r)
	return id, err == nil, err
}

func (self *Metadata) readPallets(r *scaleReader) error {
	count, err := r.readCount()
	if err != nil {
		return err
	}
	for range count {
		pallet := &metadataPallet{
			constants: map[string]metadataConstant{},
			storage:   map[string]metadataStorageEntry{},
		}
		pallet.name, err = r.readString()
		if err != nil {
			return err
		}
		if err := readPalletStorage(r, pallet); err != nil {
			return fmt.Errorf("%s storage: %w", pallet.name, err)
		}
		pallet.calls, pallet.hasCalls, err = readOptionalTypeId(r)
		if err != nil {
			return err
		}
		pallet.events, pallet.hasEvents, err = readOptionalTypeId(r)
		if err != nil {
			return err
		}
		constantCount, err := r.readCount()
		if err != nil {
			return err
		}
		for range constantCount {
			name, err := r.readString()
			if err != nil {
				return err
			}
			constant := metadataConstant{}
			constant.ty, err = readTypeId(r)
			if err != nil {
				return err
			}
			constant.value, err = r.readBytes()
			if err != nil {
				return err
			}
			if err := r.skipStrings(); err != nil {
				return err
			}
			pallet.constants[name] = constant
		}
		pallet.errors, pallet.hasErrors, err = readOptionalTypeId(r)
		if err != nil {
			return err
		}
		pallet.index, err = r.readU8()
		if err != nil {
			return err
		}
		if _, exists := self.pallets[pallet.name]; exists {
			return fmt.Errorf("pallet %s twice", pallet.name)
		}
		if _, exists := self.palletsByIndex[pallet.index]; exists {
			return fmt.Errorf("pallet index %d twice", pallet.index)
		}
		self.pallets[pallet.name] = pallet
		self.palletsByIndex[pallet.index] = pallet
	}
	return nil
}

func readPalletStorage(r *scaleReader, pallet *metadataPallet) error {
	present, err := r.readOption()
	if err != nil || !present {
		return err
	}
	// the prefix
	if _, err := r.readBytes(); err != nil {
		return err
	}
	count, err := r.readCount()
	if err != nil {
		return err
	}
	for range count {
		name, err := r.readString()
		if err != nil {
			return err
		}
		entry := metadataStorageEntry{}
		modifier, err := r.readU8()
		if err != nil {
			return err
		}
		entry.optional = modifier == 0
		form, err := r.readU8()
		if err != nil {
			return err
		}
		switch form {
		case 0:
			entry.valueType, err = readTypeId(r)
			if err != nil {
				return err
			}
		case 1:
			hasherCount, err := r.readCount()
			if err != nil {
				return err
			}
			for range hasherCount {
				hasher, err := r.readU8()
				if err != nil {
					return err
				}
				entry.hashers = append(entry.hashers, hasher)
			}
			entry.keyType, err = readTypeId(r)
			if err != nil {
				return err
			}
			entry.valueType, err = readTypeId(r)
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("storage form %d", form)
		}
		entry.fallback, err = r.readBytes()
		if err != nil {
			return err
		}
		if err := r.skipStrings(); err != nil {
			return err
		}
		pallet.storage[name] = entry
	}
	return nil
}

func (self *Metadata) readExtrinsic(r *scaleReader) error {
	var err error
	self.extrinsicType, err = readTypeId(r)
	if err != nil {
		return err
	}
	self.extrinsicVersion, err = r.readU8()
	if err != nil {
		return err
	}
	count, err := r.readCount()
	if err != nil {
		return err
	}
	for range count {
		extension := metadataExtension{}
		extension.identifier, err = r.readString()
		if err != nil {
			return err
		}
		extension.ty, err = readTypeId(r)
		if err != nil {
			return err
		}
		extension.additional, err = readTypeId(r)
		if err != nil {
			return err
		}
		self.extensions = append(self.extensions, extension)
	}
	return nil
}

func (self *Metadata) lookup(id uint32) (*metadataType, error) {
	ty := self.types[id]
	if ty == nil {
		return nil, fmt.Errorf("metadata: no type %d", id)
	}
	return ty, nil
}

// resolve follows single-field composites and single-element tuples to the
// type that says what the bytes are: AccountId32 to [u8; 32], NetUid to u16.
func (self *Metadata) resolve(id uint32) (*metadataType, error) {
	ty, err := self.lookup(id)
	if err != nil {
		return nil, err
	}
	for range maxTypeDepth {
		switch {
		case ty.kind == typeDefComposite && len(ty.fields) == 1:
			ty, err = self.lookup(ty.fields[0].ty)
		case ty.kind == typeDefTuple && len(ty.tuple) == 1:
			ty, err = self.lookup(ty.tuple[0])
		default:
			return ty, nil
		}
		if err != nil {
			return nil, err
		}
	}
	return nil, errors.New("metadata: type nesting too deep")
}

// isZeroSized reports whether a type's encoding is empty.
func (self *Metadata) isZeroSized(id uint32) (bool, error) {
	return self.isZeroSizedAt(id, 0, map[uint32]bool{})
}

func (self *Metadata) isZeroSizedAt(id uint32, depth int, known map[uint32]bool) (bool, error) {
	if depth > maxTypeDepth {
		return false, errors.New("metadata: type nesting too deep")
	}
	if zero, ok := known[id]; ok {
		return zero, nil
	}
	ty, err := self.lookup(id)
	if err != nil {
		return false, err
	}
	var elems []uint32
	switch ty.kind {
	case typeDefComposite:
		for _, field := range ty.fields {
			elems = append(elems, field.ty)
		}
	case typeDefTuple:
		elems = ty.tuple
	case typeDefArray:
		if 0 < ty.length {
			elems = []uint32{ty.elem}
		}
	default:
		known[id] = false
		return false, nil
	}
	for _, elem := range elems {
		zero, err := self.isZeroSizedAt(elem, depth+1, known)
		if err != nil {
			return false, err
		}
		if !zero {
			known[id] = false
			return false, nil
		}
	}
	known[id] = true
	return true, nil
}

func (self *Metadata) isPrimitive(id uint32, kind primitiveKind) bool {
	ty, err := self.resolve(id)
	return err == nil && ty.kind == typeDefPrimitive && ty.primitive == kind
}

func (self *Metadata) isBytes32(id uint32) bool {
	ty, err := self.resolve(id)
	if err != nil || ty.kind != typeDefArray || ty.length != 32 {
		return false
	}
	return self.isPrimitive(ty.elem, primitiveU8)
}

// isCompactOf reports whether a type is the compact encoding of an integer.
func (self *Metadata) isCompactOf(id uint32, kind primitiveKind) bool {
	ty, err := self.resolve(id)
	if err != nil || ty.kind != typeDefCompact {
		return false
	}
	return self.isPrimitive(ty.elem, kind)
}

// isOptionOfBytes32 reports whether a type is Option<[u8; 32]>.
func (self *Metadata) isOptionOfBytes32(id uint32) bool {
	ty, err := self.lookup(id)
	if err != nil || ty.kind != typeDefVariant || ty.pathName() != "Option" {
		return false
	}
	none, some := ty.variantByIndex(0), ty.variantByIndex(1)
	if none == nil || some == nil || none.name != "None" || some.name != "Some" || len(none.fields) != 0 || len(some.fields) != 1 {
		return false
	}
	return self.isBytes32(some.fields[0].ty)
}
