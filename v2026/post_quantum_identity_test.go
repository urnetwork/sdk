package sdk

import (
	"bytes"
	"strings"
	"testing"
)

// TestPublicIdentityKeyHash pins the canonical display hash for identity
// keys: SHA256 of the key, unpadded uppercase base32 (RFC 4648), 52 chars
// for a 32-byte key. Every platform renders exactly this string, so the
// value is a cross-platform golden constant.
func TestPublicIdentityKeyHash(t *testing.T) {
	input := make([]byte, 32)
	for i := range input {
		input[i] = byte(i)
	}

	// sha256(0x00..0x1f) = 630dcd2966c4336691125448bbb25b4ff412a49c732db2c8abc1b8581bd710dd
	// verified independently (python hashlib/base64.b32encode)
	const expected = "MMG42KLGYQZWNEISKRELXMS3J72BFJE4OMW3FSFLYG4FQG6XCDOQ"

	hash := PublicIdentityKeyHash(input)
	if hash != expected {
		t.Fatalf("golden hash mismatch: %s != %s", hash, expected)
	}
	if len(hash) != 52 {
		t.Fatalf("hash length = %d, expected 52", len(hash))
	}
	if strings.Contains(hash, "=") {
		t.Fatalf("hash must be unpadded: %s", hash)
	}
	if hash != strings.ToUpper(hash) {
		t.Fatalf("hash must be uppercase: %s", hash)
	}

	// deterministic
	if PublicIdentityKeyHash(input) != hash {
		t.Fatalf("hash must be deterministic")
	}
	// sensitive to the input
	other := make([]byte, 32)
	copy(other, input)
	other[0] ^= 0x01
	if PublicIdentityKeyHash(other) == hash {
		t.Fatalf("distinct keys must hash distinctly")
	}
	// any input length hashes to 52 chars (sha256 is fixed size)
	if len(PublicIdentityKeyHash([]byte{})) != 52 {
		t.Fatalf("empty input hash length must be 52")
	}
}

func TestRenderIdenticonPng(t *testing.T) {
	input := []byte("post quantum identity")

	png, err := RenderIdenticonPng(input, 64)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	if len(png) == 0 {
		t.Fatalf("render returned no bytes")
	}
	pngMagic := []byte("\x89PNG\r\n\x1a\n")
	if !bytes.HasPrefix(png, pngMagic) {
		t.Fatalf("render output missing png magic: %x", png[:min(len(png), 8)])
	}

	// deterministic across calls
	png2, err := RenderIdenticonPng(input, 64)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	if !bytes.Equal(png, png2) {
		t.Fatalf("render must be deterministic")
	}

	// invalid size errors, never panics
	if _, err := RenderIdenticonPng(input, 0); err == nil {
		t.Fatalf("size 0 must error")
	}
}

func TestProviderIdentityGetPublicKeyHash(t *testing.T) {
	publicKey := make([]byte, 32)
	for i := range publicKey {
		publicKey[i] = byte(i)
	}
	providerIdentity := &ProviderIdentity{
		ClientId:  NewId(),
		PublicKey: publicKey,
	}
	if hash := providerIdentity.GetPublicKeyHash(); hash != PublicIdentityKeyHash(publicKey) {
		t.Fatalf("provider identity hash mismatch: %s", hash)
	}

	// missing key degrades to ""
	missing := &ProviderIdentity{
		ClientId: NewId(),
	}
	if hash := missing.GetPublicKeyHash(); hash != "" {
		t.Fatalf("missing key hash = %q, expected \"\"", hash)
	}
}
