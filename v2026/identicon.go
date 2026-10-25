// The identicon renderer is excluded from the iOS network extension.
//
// This is the ONLY importer of goidenticons in the sdk, and nothing else here
// imports image/png or golang.org/x/image, so this one file drags the entire
// raster closure -- goidenticons, x/image/{draw,vector,math}, image/png,
// image/draw, compress/zlib, hash/adler32 -- into whatever binary links it.
// gomobile bind roots every exported symbol, so RenderIdenticonPng being
// exported is enough to keep all of that alive: the linker cannot drop it, and
// -s -w do not touch it.
//
// The extension is a packet tunnel with no screen. It never renders an
// identicon -- the raster is drawn by the app, from a key the extension
// supplies as bytes. Measured saving on an arm64 build with the symbol rooted
// versus not: 327,680 bytes.
//
// PublicIdentityKeyHash, ProviderIdentity and ProviderIdentityList stay in
// post_quantum_identity.go WITHOUT a tag -- device_local.go needs them in both
// builds, and they cost only sha256 and base32, which are linked anyway.

//go:build !ios_extension

package sdk

import (
	"github.com/urnetwork/goidenticons/v2026"
)

// RenderIdenticonPng renders the canonical identicon raster for `input` as an
// opaque size x size png. This is the one true identicon rendering for
// identity keys on every platform; apps clip the corners client-side.
// Renders the goidenticons v2 scheme (URnetwork brand palette).
func RenderIdenticonPng(input []byte, size int) ([]byte, error) {
	return goidenticons.RenderPngV2(input, size)
}
