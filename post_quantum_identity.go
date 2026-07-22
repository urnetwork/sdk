// post quantum identity: the exported surface for provider identity keys.
// A provider identity is a peer with an established, identity-verified e2e
// session (the session cipher completed its handshake and the peer proved
// possession of its long-lived identity key over the session channel).
// See `Device.GetProviderIdentities` / `Device.GetPublicIdentityKey`.
package sdk

import (
	"crypto/sha256"
	"encoding/base32"

	"github.com/urnetwork/goidenticons"
)

// RenderIdenticonPng renders the canonical identicon raster for `input` as an
// opaque size x size png. This is the one true identicon rendering for
// identity keys on every platform; apps clip the corners client-side.
func RenderIdenticonPng(input []byte, size int) ([]byte, error) {
	return goidenticons.RenderPng(input, size)
}

// publicIdentityKeyHashEncoding is unpadded uppercase base32 (RFC 4648)
var publicIdentityKeyHashEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// PublicIdentityKeyHash is THE canonical display hash for identity keys on
// every platform: the SHA256 of the key, encoded as unpadded uppercase
// base32 (RFC 4648). 52 characters for a 32-byte key.
func PublicIdentityKeyHash(publicKey []byte) string {
	sum := sha256.Sum256(publicKey)
	return publicIdentityKeyHashEncoding.EncodeToString(sum[:])
}

// ProviderIdentity is a provider with an established, identity-verified e2e
// session to this device. `PublicKey` is the provider's long-lived Ed25519
// public identity key.
type ProviderIdentity struct {
	ClientId  *Id
	PublicKey []byte
}

// GetPublicKeyHash returns the canonical display hash of the provider's
// public identity key (see `PublicIdentityKeyHash`), or "" when the key is
// missing.
func (self *ProviderIdentity) GetPublicKeyHash() string {
	if len(self.PublicKey) == 0 {
		return ""
	}
	return PublicIdentityKeyHash(self.PublicKey)
}

type ProviderIdentityList struct {
	exportedList[*ProviderIdentity]
}

func NewProviderIdentityList() *ProviderIdentityList {
	return &ProviderIdentityList{
		exportedList: *newExportedList[*ProviderIdentity](),
	}
}
