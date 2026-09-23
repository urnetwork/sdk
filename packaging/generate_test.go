// SPDX-License-Identifier: MPL-2.0
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var generatedBindingPaths = []string{
	"python/src/urnetwork/_raw.py",
	"java/src/main/java/io/ur/sdk/Raw.java",
	"csharp/Raw.g.cs",
	"rust/src/raw.rs",
	"ruby/lib/urnetwork/raw.rb",
}

// Generate from the real C ABI into a private tree, never over the checked-in
// bindings: otherwise a freshness check would silently repair its own failure.
// Like the credential fixtures, this changes root and must not run in parallel.
func generatedBindingsFixture(t *testing.T) string {
	t.Helper()
	sourceRoot := root
	header := read(path("cgo/include/urnetwork_sdk.h"))
	root = t.TempDir()
	t.Cleanup(func() { root = sourceRoot })
	write(path("cgo/include/urnetwork_sdk.h"), header)
	generateBindings()
	return sourceRoot
}

func TestGeneratedBindingsMatchCurrentABI(t *testing.T) {
	sourceRoot := generatedBindingsFixture(t)
	for _, name := range generatedBindingPaths {
		t.Run(name, func(t *testing.T) {
			actual, err := os.ReadFile(filepath.Join(sourceRoot, name))
			if err != nil {
				t.Fatalf("read checked-in binding: %v", err)
			}
			if !bytes.Equal(actual, read(path(name))) {
				t.Fatalf("%s is stale relative to cgo/include/urnetwork_sdk.h; run `go -C packaging run . generate` from the SDK root and commit the generated bindings", name)
			}
		})
	}
}

func TestGeneratedBindingsDoNotRewriteUnchangedSources(t *testing.T) {
	generatedBindingsFixture(t)
	before := make(map[string]os.FileInfo)
	contents := make(map[string][]byte)
	oldTime := time.Unix(1700000000, 0)
	for _, name := range generatedBindingPaths {
		must(os.Chtimes(path(name), oldTime, oldTime))
		info, err := os.Stat(path(name))
		must(err)
		before[name] = info
		contents[name] = read(path(name))
	}

	generateBindings()
	for _, name := range generatedBindingPaths {
		info, err := os.Stat(path(name))
		must(err)
		if !bytes.Equal(contents[name], read(path(name))) {
			t.Errorf("second generation changed %s", name)
		}
		if !os.SameFile(before[name], info) || !before[name].ModTime().Equal(info.ModTime()) {
			t.Errorf("second generation rewrote already-current %s", name)
		}
	}
}

func TestGeneratedBindingsPropagateABIAdditions(t *testing.T) {
	generatedBindingsFixture(t)
	before := make(map[string][]byte)
	for _, name := range generatedBindingPaths {
		before[name] = read(path(name))
	}

	// Reproduce the original stale-source cause: the C ABI gains an export,
	// so every language's previous generated source must cease to be current.
	const symbol = "urnet_generated_binding_freshness_probe"
	headerPath := path("cgo/include/urnetwork_sdk.h")
	write(headerPath, append(read(headerPath), []byte("\nvoid "+symbol+"(bool enabled);\n")...))
	generateBindings()
	for _, name := range generatedBindingPaths {
		after := read(path(name))
		if bytes.Equal(before[name], after) || !bytes.Contains(after, []byte(symbol)) {
			t.Errorf("%s did not detect the added C ABI export", name)
		}
	}
}
