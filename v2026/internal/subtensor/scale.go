package subtensor

import (
	"encoding/binary"
	"errors"
	"math/big"
	"math/bits"
)

// The SCALE this package needs: little-endian integers, compact integers and
// length-prefixed vectors. Input comes from a chain endpoint, so every length
// is bounded by the bytes that are actually there and nothing here panics.

var errScaleTruncated = errors.New("scale: input ended early")

var errScaleOverflow = errors.New("scale: compact integer does not fit")

type scaleReader struct {
	data []byte
	pos  int
}

func newScaleReader(data []byte) *scaleReader {
	return &scaleReader{data: data}
}

func (self *scaleReader) remaining() int {
	return len(self.data) - self.pos
}

func (self *scaleReader) take(n int) ([]byte, error) {
	if n < 0 || self.remaining() < n {
		return nil, errScaleTruncated
	}
	out := self.data[self.pos : self.pos+n]
	self.pos += n
	return out, nil
}

func (self *scaleReader) readU8() (uint8, error) {
	b, err := self.take(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

// readCompactBig reads a compact integer of any width (at most 67 bytes).
func (self *scaleReader) readCompactBig() (*big.Int, error) {
	first, err := self.readU8()
	if err != nil {
		return nil, err
	}
	switch first & 0b11 {
	case 0b00:
		return big.NewInt(int64(first >> 2)), nil
	case 0b01:
		rest, err := self.take(1)
		if err != nil {
			return nil, err
		}
		return big.NewInt(int64(uint16(first)|uint16(rest[0])<<8) >> 2), nil
	case 0b10:
		rest, err := self.take(3)
		if err != nil {
			return nil, err
		}
		value := uint32(first) | uint32(rest[0])<<8 | uint32(rest[1])<<16 | uint32(rest[2])<<24
		return big.NewInt(int64(value >> 2)), nil
	default:
		raw, err := self.take(int(first>>2) + 4)
		if err != nil {
			return nil, err
		}
		reversed := make([]byte, len(raw))
		for i, b := range raw {
			reversed[len(raw)-1-i] = b
		}
		return new(big.Int).SetBytes(reversed), nil
	}
}

func (self *scaleReader) readCompact() (uint64, error) {
	value, err := self.readCompactBig()
	if err != nil {
		return 0, err
	}
	if !value.IsUint64() {
		return 0, errScaleOverflow
	}
	return value.Uint64(), nil
}

// readCount reads a vector length. Every vector this package walks holds
// elements of at least one byte, so the count cannot pass the bytes left.
func (self *scaleReader) readCount() (int, error) {
	count, err := self.readCompact()
	if err != nil {
		return 0, err
	}
	if count > uint64(self.remaining()) {
		return 0, errScaleTruncated
	}
	return int(count), nil
}

func (self *scaleReader) readBytes() ([]byte, error) {
	count, err := self.readCount()
	if err != nil {
		return nil, err
	}
	return self.take(count)
}

func (self *scaleReader) readString() (string, error) {
	b, err := self.readBytes()
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (self *scaleReader) readBool() (bool, error) {
	b, err := self.readU8()
	if err != nil {
		return false, err
	}
	switch b {
	case 0:
		return false, nil
	case 1:
		return true, nil
	default:
		return false, errors.New("scale: bad bool")
	}
}

func (self *scaleReader) skipStrings() error {
	count, err := self.readCount()
	if err != nil {
		return err
	}
	for range count {
		if _, err := self.readBytes(); err != nil {
			return err
		}
	}
	return nil
}

// readOption reads the Option tag.
func (self *scaleReader) readOption() (bool, error) {
	tag, err := self.readU8()
	if err != nil {
		return false, err
	}
	switch tag {
	case 0:
		return false, nil
	case 1:
		return true, nil
	default:
		return false, errors.New("scale: bad option tag")
	}
}

func appendCompact(out []byte, value uint64) []byte {
	switch {
	case value < 1<<6:
		return append(out, byte(value<<2))
	case value < 1<<14:
		return binary.LittleEndian.AppendUint16(out, uint16(value<<2)|0b01)
	case value < 1<<30:
		return binary.LittleEndian.AppendUint32(out, uint32(value<<2)|0b10)
	}
	byteCount := (bits.Len64(value) + 7) / 8
	out = append(out, byte((byteCount-4)<<2)|0b11)
	for range byteCount {
		out = append(out, byte(value))
		value >>= 8
	}
	return out
}
