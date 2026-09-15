package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regenerate the complete C surface in isolation and compare every artifact
// byte-for-byte. This keeps ordinary test runs read-only while making a missed
// `make generate` fail at the same generator boundary that owns the files.
func TestGeneratedArtifactsMatchCurrentSDKSurface(t *testing.T) {
	generatorDirectory := testingGenDir(t)
	cgoDirectory := filepath.Dir(generatorDirectory)
	g := testingPreferenceGenerator(t)
	outputDirectory := t.TempDir()
	t.Chdir(outputDirectory)
	if err := g.run(); err != nil {
		t.Fatal(err)
	}
	generatedDefinition, err := os.ReadFile(
		filepath.Join(outputDirectory, "include", "urnetwork_sdk.def"),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, symbol := range testingManualExportSymbols(t, cgoDirectory) {
		if !bytes.Contains(generatedDefinition, []byte("\n\t"+symbol+"\n")) {
			t.Errorf("fresh module definition omitted manual export %s", symbol)
		}
	}

	for _, path := range []string{
		"exports_gen.go",
		"exports_gen_unix.go",
		"callbacks.h",
		"callbacks.c",
		filepath.Join("include", "urnetwork_sdk.h"),
		filepath.Join("include", "urnetwork_sdk.def"),
		filepath.Join("include", "urnetwork_sdk.hpp"),
		"coverage_report.txt",
	} {
		tracked, err := os.ReadFile(filepath.Join(cgoDirectory, path))
		if err != nil {
			t.Fatal(err)
		}
		generated, err := os.ReadFile(filepath.Join(outputDirectory, path))
		if err != nil {
			t.Fatal(err)
		}
		if err := generatedArtifactMismatch(path, tracked, generated); err != nil {
			t.Error(err)
		}
	}
}

// Preserve the demonstrated failure mode: a coverage report can remain valid
// text while naming declarations that the SDK and generator no longer have.
func TestGeneratedArtifactCurrentnessRejectsStaleCoverage(t *testing.T) {
	coveragePath := filepath.Join(filepath.Dir(testingGenDir(t)), "coverage_report.txt")
	current, err := os.ReadFile(coveragePath)
	if err != nil {
		t.Fatal(err)
	}
	stale := bytes.Clone(current)
	stale = append(stale,
		[]byte("DialWebTransport: skipped: Go/JS WebTransport surface\n"+
			"WebTransportOptions: skipped: Go WebTransport configuration\n")...,
	)
	if err := generatedArtifactMismatch("coverage_report.txt", stale, current); err == nil {
		t.Fatal("stale generated coverage was accepted")
	}
	if err := generatedArtifactMismatch("coverage_report.txt", current, current); err != nil {
		t.Fatalf("identical generated coverage was rejected: %v", err)
	}
}

func generatedArtifactMismatch(path string, tracked, generated []byte) error {
	if bytes.Equal(tracked, generated) {
		return nil
	}
	return fmt.Errorf(
		"%s is stale: tracked sha256 %x, freshly generated sha256 %x; run `make generate` in cgo",
		path,
		sha256.Sum256(tracked),
		sha256.Sum256(generated),
	)
}

// Read generator inputs independently of manualExports so a broken source
// scan cannot make both the generation and its assertion silently empty.
func testingManualExportSymbols(t *testing.T, cgoDirectory string) []string {
	t.Helper()
	entries, err := os.ReadDir(cgoDirectory)
	if err != nil {
		t.Fatal(err)
	}
	var symbols []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasPrefix(name, "exports_gen") || name == "exports_core.go" {
			continue
		}
		source, err := os.ReadFile(filepath.Join(cgoDirectory, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(source), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[0] == "//export" {
				symbols = append(symbols, fields[1])
			}
		}
	}
	if len(symbols) == 0 {
		t.Fatal("manual export input scan found no symbols")
	}
	return symbols
}
