// Excluded from the iOS network extension alongside identicon.go, which is
// where RenderIdenticonPng lives. `go test -tags ios_extension` must still
// build, and it cannot if this test references a symbol that build drops.

//go:build !ios_extension

package sdk

import (
	"bytes"
	"testing"
)

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
