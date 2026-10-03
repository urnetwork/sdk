// SPDX-License-Identifier: MPL-2.0
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
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

// Stale or missing checked-in outputs must fail without being repaired, even
// when another ALL step keeps going after an earlier failed SDK test.
func TestGeneratedBindingCheckNeverMutatesSource(t *testing.T) {
	for _, state := range []string{"current", "stale", "missing", "added-export"} {
		t.Run(state, func(t *testing.T) {
			generatedBindingsFixture(t)
			changed := generatedBindingPaths[0]
			switch state {
			case "stale":
				write(path(changed), append(read(path(changed)), []byte("# stale fixture\n")...))
			case "missing":
				must(os.Remove(path(changed)))
			case "added-export":
				header := path("cgo/include/urnetwork_sdk.h")
				write(header, append(read(header), []byte("\nvoid urnet_readonly_freshness_probe(bool enabled);\n")...))
			}
			contents := map[string][]byte{}
			metadata := map[string]os.FileInfo{}
			paths := append([]string{"cgo/include/urnetwork_sdk.h"}, generatedBindingPaths...)
			for _, name := range paths {
				if state == "missing" && name == changed {
					continue
				}
				must(os.Chtimes(path(name), time.Unix(1700000000, 0), time.Unix(1700000000, 0)))
				info, err := os.Stat(path(name))
				must(err)
				contents[name], metadata[name] = read(path(name)), info
			}
			for range 2 {
				err := checkGeneratedBindings()
				if (err != nil) != (state != "current") {
					t.Fatalf("state=%s freshness result=%v", state, err)
				}
				if err != nil {
					if !strings.Contains(err.Error(), changed) || !strings.Contains(err.Error(), "commit the generated bindings") {
						t.Fatalf("missing source update guidance: %v", err)
					}
					if state == "added-export" {
						for _, name := range generatedBindingPaths {
							if !strings.Contains(err.Error(), name) {
								t.Errorf("new ABI export did not invalidate %s: %v", name, err)
							}
						}
					}
				}
			}
			for _, name := range paths {
				info, err := os.Stat(path(name))
				if state == "missing" && name == changed {
					if !os.IsNotExist(err) {
						t.Errorf("freshness check recreated missing %s", name)
					}
					continue
				}
				must(err)
				if !bytes.Equal(contents[name], read(path(name))) || !os.SameFile(metadata[name], info) ||
					!metadata[name].ModTime().Equal(info.ModTime()) || metadata[name].Mode() != info.Mode() {
					t.Errorf("freshness check rewrote %s", name)
				}
			}
		})
	}
}
