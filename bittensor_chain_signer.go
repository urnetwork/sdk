//go:build !ios_extension && !js

package sdk

// A key that signs transactions: the local key's phrase, or the raw seed of a
// key the phrase does not open, such as a fleet's hotkey. These sign exactly what
// they are given, unwrapped, so only the chain engine holds one and it signs only
// what subtensor.Prepare built. A message for a person's eyes goes through
// BittensorLocalKeySign, which can never make a transaction's signature.

import (
	"errors"

	"github.com/ChainSafe/go-schnorrkel"

	"github.com/urnetwork/sdk/internal/subtensor"
)

var errBittensorSignerInvalidKey = errors.New(BittensorLocalKeyErrorFailed + ": could not read the key")

type bittensorSigner struct {
	public [32]byte
	secret *schnorrkel.SecretKey
}

func (self *bittensorSigner) PublicKey() [32]byte {
	return self.public
}

func (self *bittensorSigner) Sign(payload []byte) ([64]byte, error) {
	transcript := schnorrkel.NewSigningContext([]byte(bittensorLocalKeySigningContext), payload)
	signature, err := self.secret.Sign(transcript)
	if err != nil {
		return [64]byte{}, errBittensorLocalKeySign
	}
	return signature.Encode(), nil
}

func newBittensorSigner(miniSecret *schnorrkel.MiniSecretKey) (*bittensorSigner, error) {
	publicKey := miniSecret.Public()
	if publicKey == nil {
		return nil, errBittensorSignerInvalidKey
	}
	return &bittensorSigner{public: publicKey.Encode(), secret: miniSecret.ExpandEd25519()}, nil
}

// bittensorSignerFromPhrase is the key a recovery phrase opens. Its errors are
// the local key's fixed text: none holds any part of a phrase.
func bittensorSignerFromPhrase(mnemonic string) (subtensor.Signer, error) {
	miniSecret, err := bittensorLocalKeyMiniSecret(mnemonic)
	if err != nil {
		return nil, err
	}
	return newBittensorSigner(miniSecret)
}

// bittensorSignerFromSeed is the key of a raw 32-byte sr25519 seed.
func bittensorSignerFromSeed(seed [32]byte) (subtensor.Signer, error) {
	miniSecret, err := schnorrkel.NewMiniSecretKeyFromRaw(seed)
	if err != nil {
		return nil, errBittensorSignerInvalidKey
	}
	return newBittensorSigner(miniSecret)
}
