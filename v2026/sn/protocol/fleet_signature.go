//go:build !ios_extension && !js

package protocol

// The signature checks a device runs before it signs a binding with no person
// at the host. The build constraint keeps these and their libraries out of the
// tunnel extension, whose binary has a hard size ceiling, and out of the browser:
// on those platforms the app process runs the checks and the extension only signs.

import (
	"crypto/ed25519"
	"errors"

	"github.com/ChainSafe/go-schnorrkel"
)

// VerifyFleetHotkeyAuthorization parses a signed message and checks the
// signature of the coldkey it names. The caller still compares that coldkey
// with the one it pinned.
func VerifyFleetHotkeyAuthorization(message string, signature []byte) (*FleetHotkeyAuthorization, error) {
	authorization, err := ParseFleetHotkeyAuthorization(message)
	if err != nil {
		return nil, err
	}
	if !VerifyWalletSignature(authorization.Coldkey, message, signature) {
		return nil, errors.New("fleet hotkey authorization signature does not verify")
	}
	return authorization, nil
}

// sr25519Verify is the "substrate" signing context the chain's precompile, the
// wallets and btcli all use.
func sr25519Verify(publicKey [32]byte, message []byte, signature [64]byte) bool {
	public := &schnorrkel.PublicKey{}
	if err := public.Decode(publicKey); err != nil {
		return false
	}
	decoded := &schnorrkel.Signature{}
	if err := decoded.Decode(signature); err != nil {
		return false
	}
	ok, err := public.Verify(decoded, schnorrkel.NewSigningContext([]byte("substrate"), message))
	return err == nil && ok
}

// VerifyWalletSignature checks a wallet's signature over the exact text: sr25519
// or ed25519, over the bare text or wrapped in <Bytes>...</Bytes>, the forms the
// server's own wallet check takes (server model.VerifyBittensorSignature). The
// signature says which curve: schnorrkel sets the high bit of its last byte and
// an ed25519 signature never does.
func VerifyWalletSignature(publicKey [32]byte, message string, signature []byte) bool {
	if len(signature) != 64 {
		return false
	}
	candidates := [][]byte{[]byte("<Bytes>" + message + "</Bytes>"), []byte(message)}
	if signature[63]&0x80 == 0 {
		for _, candidate := range candidates {
			if ed25519.Verify(ed25519.PublicKey(publicKey[:]), candidate, signature) {
				return true
			}
		}
		return false
	}
	var fixed [64]byte
	copy(fixed[:], signature)
	for _, candidate := range candidates {
		if sr25519Verify(publicKey, candidate, fixed) {
			return true
		}
	}
	return false
}

// VerifyHotkey checks the fleet hotkey's sr25519 signature over the binding
// digest, as the coordinator's precompile does.
func (b FleetBinding) VerifyHotkey(signature []byte) bool {
	digest, err := b.Digest()
	if err != nil || len(signature) != 64 {
		return false
	}
	var fixed [64]byte
	copy(fixed[:], signature)
	return sr25519Verify(b.Hotkey, digest[:], fixed)
}

// FleetHotkeyPublicKey is the account of a fleet hotkey's 32-byte seed, derived the
// way subkey, btcli and wallets derive it. The seed is the key holder's, on one
// device. It is never a recovery phrase.
func FleetHotkeyPublicKey(seed [32]byte) ([32]byte, error) {
	secret, err := fleetHotkeySecret(seed)
	if err != nil {
		return [32]byte{}, err
	}
	public, err := secret.Public()
	if err != nil {
		return [32]byte{}, err
	}
	return public.Encode(), nil
}

// SignFleetBindingWithHotkey is the hotkey's signature over a binding's digest, the
// only thing a fleet hotkey signs besides its own commitment extrinsic.
func SignFleetBindingWithHotkey(seed [32]byte, binding FleetBinding) ([]byte, error) {
	secret, err := fleetHotkeySecret(seed)
	if err != nil {
		return nil, err
	}
	digest, err := binding.Digest()
	if err != nil {
		return nil, err
	}
	signature, err := secret.Sign(schnorrkel.NewSigningContext([]byte("substrate"), digest[:]))
	if err != nil {
		return nil, errors.New("the hotkey could not sign")
	}
	encoded := signature.Encode()
	return encoded[:], nil
}

// The library's errors stop here: none reaches a log with a seed in it.
func fleetHotkeySecret(seed [32]byte) (*schnorrkel.SecretKey, error) {
	if seed == ([32]byte{}) {
		return nil, errors.New("the fleet hotkey seed is empty")
	}
	mini, err := schnorrkel.NewMiniSecretKeyFromRaw(seed)
	if err != nil {
		return nil, errors.New("the fleet hotkey seed can't be used")
	}
	return mini.ExpandEd25519(), nil
}
