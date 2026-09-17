package evm

import (
	"errors"
	"math/big"

	secp "github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// Signature is a recoverable secp256k1 signature over a 32-byte digest.
type Signature struct {
	R, S  *big.Int
	RecId byte // 0 or 1 (EIP-1559 yParity)
}

// Sign produces a canonical (low-S, RFC 6979 deterministic) signature.
func Sign(hash [32]byte, key *secp.PrivateKey) (*Signature, error) {
	if key == nil {
		return nil, errors.New("nil key")
	}
	compact := ecdsa.SignCompact(key, hash[:], false)
	if len(compact) != 65 || compact[0] < 27 || compact[0] > 30 {
		return nil, errors.New("unexpected compact signature")
	}
	return &Signature{
		R:     new(big.Int).SetBytes(compact[1:33]),
		S:     new(big.Int).SetBytes(compact[33:65]),
		RecId: compact[0] - 27,
	}, nil
}

// Recover returns the signer's address for a digest and signature.
func Recover(hash [32]byte, sig *Signature) ([20]byte, error) {
	var compact [65]byte
	compact[0] = 27 + sig.RecId
	sig.R.FillBytes(compact[1:33])
	sig.S.FillBytes(compact[33:65])
	pub, _, err := ecdsa.RecoverCompact(compact[:], hash[:])
	if err != nil {
		return [20]byte{}, err
	}
	return AddressOfPublic(pub), nil
}

// LegacyTx is an EIP-155 replay-protected legacy transaction.
type LegacyTx struct {
	Nonce    uint64
	GasPrice *big.Int
	Gas      uint64
	To       [20]byte
	Value    *big.Int
	Data     []byte
}

// SigningHash is keccak256(rlp(nonce, gasPrice, gas, to, value, data, chainId, 0, 0)).
func (t *LegacyTx) SigningHash(chainId *big.Int) [32]byte {
	return Keccak256(rlpList(
		rlpUint64(t.Nonce),
		rlpUint(t.GasPrice),
		rlpUint64(t.Gas),
		rlpBytes(t.To[:]),
		rlpUint(t.Value),
		rlpBytes(t.Data),
		rlpUint(chainId),
		rlpUint(nil),
		rlpUint(nil),
	))
}

// Encode renders the signed transaction; v = chainId*2 + 35 + recId.
func (t *LegacyTx) Encode(chainId *big.Int, sig *Signature) []byte {
	v := new(big.Int).Mul(chainId, big.NewInt(2))
	v.Add(v, big.NewInt(35+int64(sig.RecId)))
	return rlpList(
		rlpUint64(t.Nonce),
		rlpUint(t.GasPrice),
		rlpUint64(t.Gas),
		rlpBytes(t.To[:]),
		rlpUint(t.Value),
		rlpBytes(t.Data),
		rlpUint(v),
		rlpUint(sig.R),
		rlpUint(sig.S),
	)
}

// Signed signs and encodes the transaction; returns the raw bytes and hash.
func (t *LegacyTx) Signed(chainId *big.Int, key *secp.PrivateKey) ([]byte, [32]byte, error) {
	sig, err := Sign(t.SigningHash(chainId), key)
	if err != nil {
		return nil, [32]byte{}, err
	}
	raw := t.Encode(chainId, sig)
	return raw, Keccak256(raw), nil
}

// DynamicFeeTx is an EIP-1559 (type 2) transaction with an empty access list.
type DynamicFeeTx struct {
	Nonce                uint64
	MaxPriorityFeePerGas *big.Int
	MaxFeePerGas         *big.Int
	Gas                  uint64
	To                   [20]byte
	Value                *big.Int
	Data                 []byte
}

func (t *DynamicFeeTx) fields(chainId *big.Int) [][]byte {
	return [][]byte{
		rlpUint(chainId),
		rlpUint64(t.Nonce),
		rlpUint(t.MaxPriorityFeePerGas),
		rlpUint(t.MaxFeePerGas),
		rlpUint64(t.Gas),
		rlpBytes(t.To[:]),
		rlpUint(t.Value),
		rlpBytes(t.Data),
		rlpList(), // access list
	}
}

// SigningHash is keccak256(0x02 || rlp(chainId, nonce, tip, cap, gas, to, value, data, accessList)).
func (t *DynamicFeeTx) SigningHash(chainId *big.Int) [32]byte {
	payload := append([]byte{0x02}, rlpList(t.fields(chainId)...)...)
	return Keccak256(payload)
}

// Encode renders the signed typed transaction envelope.
func (t *DynamicFeeTx) Encode(chainId *big.Int, sig *Signature) []byte {
	items := append(t.fields(chainId),
		rlpUint64(uint64(sig.RecId)),
		rlpUint(sig.R),
		rlpUint(sig.S),
	)
	return append([]byte{0x02}, rlpList(items...)...)
}

// Signed signs and encodes the transaction; returns the raw bytes and hash.
func (t *DynamicFeeTx) Signed(chainId *big.Int, key *secp.PrivateKey) ([]byte, [32]byte, error) {
	sig, err := Sign(t.SigningHash(chainId), key)
	if err != nil {
		return nil, [32]byte{}, err
	}
	raw := t.Encode(chainId, sig)
	return raw, Keccak256(raw), nil
}
