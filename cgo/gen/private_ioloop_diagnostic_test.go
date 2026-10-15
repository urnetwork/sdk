//go:build !windows

// Keep the acceptance-only native loop session out of the public C surface.
// Generate through the real SDK loader so platform and ownership classification
// cannot be replaced by a test-only declaration or a direct map assertion.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The private !windows session must not become empty JSON in common exports;
// ordinary loop handles and the already-published teardown observation remain.
func TestPrivateIoLoopDiagnosticSessionIsExcludedFromCAbi(t *testing.T) {
	g := testingPreferenceGenerator(t)
	for _, name := range []string{"IoLoopDiagnosticSession", "NewIoLoopDiagnosticSession"} {
		if g.scope.Lookup(name) == nil {
			t.Fatalf("private native diagnostic declaration is unavailable: %s", name)
		}
	}
	outputDirectory := t.TempDir()
	t.Chdir(outputDirectory)
	if err := g.run(); err != nil {
		t.Fatal(err)
	}
	contentsKVs := map[string]string{}
	for _, name := range []string{
		"exports_gen.go",
		"exports_gen_unix.go",
		"callbacks.h",
		"callbacks.c",
		filepath.Join("include", "urnetwork_sdk.h"),
		filepath.Join("include", "urnetwork_sdk.def"),
		filepath.Join("include", "urnetwork_sdk.hpp"),
	} {
		contents, err := os.ReadFile(filepath.Join(outputDirectory, name))
		if err != nil {
			t.Fatal(err)
		}
		contentsKVs[name] = string(contents)
		for _, forbidden := range []string{
			"IoLoopDiagnosticSession",
			"urnet_new_io_loop_diagnostic_session",
			"urnet_io_loop_diagnostic_session_",
		} {
			if strings.Contains(string(contents), forbidden) {
				t.Errorf("PRIVATE_IO_LOOP_DIAGNOSTIC_EXPORTED artifact=%s symbol=%s", name, forbidden)
			}
		}
	}
	coverage, err := os.ReadFile(filepath.Join(outputDirectory, "coverage_report.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"IoLoopDiagnosticSession", "NewIoLoopDiagnosticSession"} {
		want := name + ": skipped: private acceptance diagnostic (native mobile only)\n"
		if !strings.Contains(string(coverage), want) {
			t.Errorf("PRIVATE_IO_LOOP_DIAGNOSTIC_REASON_MISSING symbol=%s", name)
		}
	}
	for _, expected := range []struct {
		artifact string
		symbol   string
	}{
		{artifact: "exports_gen_unix.go", symbol: "//export urnet_new_io_loop\n"},
		{artifact: filepath.Join("include", "urnetwork_sdk.h"), symbol: "uint64_t urnet_new_io_loop("},
		{artifact: filepath.Join("include", "urnetwork_sdk.hpp"), symbol: "inline IoLoop newIoLoop("},
		{artifact: "exports_gen.go", symbol: "//export urnet_new_device_local\n"},
		{artifact: filepath.Join("include", "urnetwork_sdk.def"), symbol: "\n\turnet_new_device_local\n"},
		{artifact: "exports_gen.go", symbol: "//export urnet_device_local_begin_memory_teardown_observation\n"},
		{artifact: filepath.Join("include", "urnetwork_sdk.h"), symbol: "char* urnet_device_local_begin_memory_teardown_observation("},
	} {
		if !strings.Contains(contentsKVs[expected.artifact], expected.symbol) {
			t.Errorf("existing public binding changed: artifact=%s symbol=%s", expected.artifact, expected.symbol)
		}
	}
	if strings.Contains(contentsKVs["exports_gen.go"], "//export urnet_new_io_loop\n") ||
		strings.Contains(contentsKVs[filepath.Join("include", "urnetwork_sdk.def")], "\n\turnet_new_io_loop\n") {
		t.Error("ordinary native loop binding lost its existing unix-only boundary")
	}
}
