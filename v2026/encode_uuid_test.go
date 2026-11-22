package sdk

import (
	"fmt"
	"math/rand/v2"
	"testing"
)

// The id text is fmt's canonical form for every id, byte for byte.
func TestEncodeUuidMatchesCanonicalFormat(t *testing.T) {
	reference := func(src [16]byte) string {
		return fmt.Sprintf("%x-%x-%x-%x-%x", src[0:4], src[4:6], src[6:8], src[8:10], src[10:16])
	}
	ids := [][16]byte{{}, {0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}}
	// deterministic so a failing id reproduces (CODESTYLE: tests may seed)
	rng := rand.New(rand.NewPCG(5, 9))
	for range 10000 {
		var id [16]byte
		for i := range id {
			id[i] = byte(rng.IntN(256))
		}
		ids = append(ids, id)
	}
	for _, id := range ids {
		if got, want := encodeUuid(id), reference(id); got != want {
			t.Fatalf("encodeUuid(%x) = %q, want %q", id, got, want)
		}
	}
}

// One id costs one allocation, its string.
func TestEncodeUuidAllocatesOnlyTheString(t *testing.T) {
	id := [16]byte{0x01, 0x9a, 0x2b, 0x3c, 0x4d, 0x5e, 0x6f, 0x70, 0x81, 0x92, 0xa3, 0xb4, 0xc5, 0xd6, 0xe7, 0xf8}
	var text string
	if allocs := testing.AllocsPerRun(1000, func() { text = encodeUuid(id) }); allocs != 1 {
		t.Fatalf("encodeUuid allocated %.1f times, want 1", allocs)
	}
	if text != "019a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8" {
		t.Fatalf("encodeUuid = %q", text)
	}
}

func BenchmarkEncodeUuid(b *testing.B) {
	id := [16]byte{0x01, 0x9a, 0x2b, 0x3c, 0x4d, 0x5e, 0x6f, 0x70, 0x81, 0x92, 0xa3, 0xb4, 0xc5, 0xd6, 0xe7, 0xf8}
	b.ReportAllocs()
	for b.Loop() {
		_ = encodeUuid(id)
	}
}
