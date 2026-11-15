// Package subtensor is the one place the sdk builds, signs and reads back a
// Subtensor (Substrate) transaction. It holds SCALE, runtime metadata, signed
// extensions and the chain reads a transaction needs, and nothing else.
//
// The only input to a signature is a Call from a constructor in calls.go, plus
// what comes from the chain: spec and transaction versions, the era's block and
// the nonce. No function takes call bytes or a payload to sign, so nothing a
// server or an endpoint sends can become what a key signs. What an endpoint can
// do is lie about those values, which only makes the transaction invalid.
//
// It is internal, and only the sdk files built for the full app bindings import
// it, so it stays out of the iOS network extension.
package subtensor

import (
	"encoding/binary"
	"errors"
	"math/bits"

	"golang.org/x/crypto/blake2b"
)

const (
	extrinsicVersionSigned = 0x84

	multiAddressId        = 0
	multiSignatureSr25519 = 1

	metadataHashModeDisabled = 0

	// EraPeriodBlocks is how long a signed transaction can land. It then can
	// never land, so a retry after that cannot double a conversion. About 13
	// minutes at 12 seconds a block.
	EraPeriodBlocks uint64 = 64

	// a signing payload longer than this is signed by its hash
	maxPlainSigningPayload = 256
)

// Signer holds an sr25519 key. Sign signs under the "substrate" context, over
// exactly the bytes it is given.
type Signer interface {
	PublicKey() [32]byte
	Sign(payload []byte) ([64]byte, error)
}

// SigningContext is what a transaction commits to besides its call.
type SigningContext struct {
	GenesisHash        [32]byte
	SpecVersion        uint32
	TransactionVersion uint32
	// the era's first block, a recent finalized one, and its hash
	BirthBlockNumber uint64
	BirthBlockHash   [32]byte
	Nonce            uint32
}

// EncodeMortalEra is sp_runtime's Era::mortal(period, current) encoding for a
// power-of-two period of at most 4096 blocks.
func EncodeMortalEra(period uint64, current uint64) ([2]byte, error) {
	if period < 4 || 4096 < period || period&(period-1) != 0 {
		return [2]byte{}, errors.New("era period must be a power of two from 4 to 4096")
	}
	phase := current % period
	encoded := min(max(uint16(bits.TrailingZeros64(period))-1, 1), 15) | uint16(phase)<<4
	var out [2]byte
	binary.LittleEndian.PutUint16(out[:], encoded)
	return out, nil
}

// Prepared is one call under one context for one signer, ready to sign.
type Prepared struct {
	signer     [32]byte
	call       []byte
	extra      []byte
	additional []byte
}

// Prepare checks the runtime's shape for the call and its extensions, and
// encodes everything but the signature. It refuses with RuntimeChangedError
// before anything can be signed.
func Prepare(meta *Metadata, context SigningContext, signer [32]byte, call *Call) (*Prepared, error) {
	era, err := EncodeMortalEra(EraPeriodBlocks, context.BirthBlockNumber)
	if err != nil {
		return nil, err
	}
	return prepareWithEra(meta, context, signer, call, era)
}

func prepareWithEra(meta *Metadata, context SigningContext, signer [32]byte, call *Call, era [2]byte) (*Prepared, error) {
	if meta == nil || call == nil {
		return nil, errors.New("prepare: no metadata or call")
	}
	if signer == ([32]byte{}) {
		return nil, errors.New("prepare: no signer")
	}
	plan, err := meta.signingPlan()
	if err != nil {
		return nil, err
	}
	if err := meta.checkCall(call); err != nil {
		return nil, err
	}
	prepared := &Prepared{signer: signer, call: call.encode()}
	for _, step := range plan {
		switch step {
		case extensionEmpty:
		case extensionSpecVersion:
			prepared.additional = binary.LittleEndian.AppendUint32(prepared.additional, context.SpecVersion)
		case extensionTransactionVersion:
			prepared.additional = binary.LittleEndian.AppendUint32(prepared.additional, context.TransactionVersion)
		case extensionGenesis:
			prepared.additional = append(prepared.additional, context.GenesisHash[:]...)
		case extensionMortality:
			prepared.extra = append(prepared.extra, era[:]...)
			prepared.additional = append(prepared.additional, context.BirthBlockHash[:]...)
		case extensionNonce:
			prepared.extra = appendCompact(prepared.extra, uint64(context.Nonce))
		case extensionTip:
			// no tip: this runtime gives a tip no priority
			prepared.extra = appendCompact(prepared.extra, 0)
		case extensionMetadataHash:
			prepared.extra = append(prepared.extra, metadataHashModeDisabled)
			// Option::None
			prepared.additional = append(prepared.additional, 0)
		}
	}
	return prepared, nil
}

// SigningPayload is what the key signs: the call, the extras and the implicit
// values, or their hash when that is longer than 256 bytes.
func (self *Prepared) SigningPayload() []byte {
	payload := make([]byte, 0, len(self.call)+len(self.extra)+len(self.additional))
	payload = append(payload, self.call...)
	payload = append(payload, self.extra...)
	payload = append(payload, self.additional...)
	if maxPlainSigningPayload < len(payload) {
		hash := blake2b.Sum256(payload)
		return hash[:]
	}
	return payload
}

func (self *Prepared) encode(signature [64]byte) []byte {
	body := make([]byte, 0, 1+33+65+len(self.extra)+len(self.call))
	body = append(body, extrinsicVersionSigned)
	body = append(body, multiAddressId)
	body = append(body, self.signer[:]...)
	body = append(body, multiSignatureSr25519)
	body = append(body, signature[:]...)
	body = append(body, self.extra...)
	body = append(body, self.call...)
	out := appendCompact(make([]byte, 0, len(body)+4), uint64(len(body)))
	return append(out, body...)
}

// UnsignedForFee is the transaction with an empty signature, the same length
// as the signed one, for a fee quote. The runtime does not check a signature
// to quote it, and it can never be accepted.
func (self *Prepared) UnsignedForFee() []byte {
	return self.encode([64]byte{})
}

// Signed is a transaction ready to send.
type Signed struct {
	// the extrinsic, with its length prefix
	Raw []byte
	// what the chain calls it: the blake2b-256 of Raw
	Hash [32]byte
}

// Sign signs the payload with the signer it was prepared for.
func (self *Prepared) Sign(signer Signer) (*Signed, error) {
	if signer == nil || signer.PublicKey() != self.signer {
		return nil, errors.New("sign: not the key this transaction was prepared for")
	}
	signature, err := signer.Sign(self.SigningPayload())
	if err != nil {
		return nil, err
	}
	raw := self.encode(signature)
	return &Signed{Raw: raw, Hash: blake2b.Sum256(raw)}, nil
}
