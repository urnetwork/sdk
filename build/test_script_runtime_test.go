// The real repository scripts must separate test-runtime and driver scratch.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Keeps the existing real-Go fixture, including its network gate and JS stubs.
// The recorder delegates to real Go; it does not implement the temp split.
func sdkRuntimeScriptFixture(t *testing.T, checkedPhase string) *sdkTestScriptFixture {
	t.Helper()
	fixture := newSdkTestScriptFixture(t)
	driver := filepath.Join(fixture.directory, "..", "compiler scratch")
	runtime := filepath.Join(fixture.directory, "..", "runtime scratch")
	for _, path := range []string{driver, runtime} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	var environment []string
	for _, entry := range fixture.env {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "TMPDIR", "GOTMPDIR", "URNETWORK_SDK_TEST_RUNTIME_DIR", "GOMAXPROCS":
			continue
		}
		environment = append(environment, entry)
	}
	fixture.env = append(environment,
		"GOTMPDIR="+driver, "TMPDIR="+runtime, "GOMAXPROCS=2",
		"URNETWORK_SDK_TEST_RUNTIME_DIR="+runtime,
		"SDK_TEST_FIXTURE_DRIVER_TEMP="+driver,
		"SDK_TEST_FIXTURE_RUNTIME_TEMP="+runtime,
		"SDK_TEST_FIXTURE_CHECKED_PHASE="+checkedPhase,
		"SDK_TEST_FIXTURE_DRIVER_CACHE="+os.Getenv("GOCACHE"),
		"SDK_TEST_FIXTURE_DRIVER_FLAGS="+os.Getenv("GOFLAGS"),
	)
	// Check the driver before the script's actual -exec boundary runs.
	fixture.write(t, "../bin/go", `#!/bin/sh
[ "$GOTMPDIR" = "$SDK_TEST_FIXTURE_DRIVER_TEMP" ] || exit 81
[ "$GOCACHE" = "$SDK_TEST_FIXTURE_DRIVER_CACHE" ] || exit 82
[ "$GOFLAGS" = "$SDK_TEST_FIXTURE_DRIVER_FLAGS" ] || exit 83
[ "$GOMAXPROCS" = 2 ] || exit 84
[ "$GOTOOLCHAIN" = local ] || exit 85
if [ "$1" = test ]; then
  if [ "$PWD" = "$SDK_TEST_FIXTURE_ROOT" ]; then module=.; else module=$(basename "$PWD"); fi
  printf 'go-test:%s\n' "$module" >>"$SDK_TEST_FIXTURE_TRACE"
elif [ "$1" = -C ] && [ "$2" = .. ] && [ "$3" = test ]; then
  printf 'go-test:js-companion\n' >>"$SDK_TEST_FIXTURE_TRACE"
fi
exec "$SDK_TEST_FIXTURE_GO" "$@"
`, 0700)
	fixture.write(t, "smoke_test.go", sdkRuntimeChildSource("TestSDKSmoke", "smoke"), 0600)
	fixture.write(t, "fixture_test.go", sdkRuntimeChildSource("TestFixture", "root"), 0600)
	fixture.module(t, "nested")
	fixture.write(t, "nested/fixture_test.go", sdkRuntimeChildSource("TestNested", "nested"), 0600)
	return fixture
}

// The same child assertion is compiled by unchanged and repaired launchers.
// Wrong placement itself fails independently of the test host's mount settings.
func sdkRuntimeChildSource(name, phase string) string {
	return fmt.Sprintf(`package fixture

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func %s(t *testing.T) {
	if os.Getenv("SDK_TEST_FIXTURE_CHECKED_PHASE") != %q {
		return
	}
	if os.Getenv("SDK_TEST_FIXTURE_CHECKED_PHASE") == "js-companion" && os.Getenv("UR_SUBPROTOCOL_WASM_TEST") != "1" {
		t.Fatal("JS companion lost its required WASM opt-in")
	}
	want, err := filepath.EvalSymlinks(os.Getenv("SDK_TEST_FIXTURE_RUNTIME_TEMP"))
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.MkdirTemp("", "runtime-mkdir-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	for _, path := range []string{t.TempDir(), directory} {
		actual, err := filepath.EvalSymlinks(path)
		if err != nil || !strings.HasPrefix(actual, want+string(os.PathSeparator)) {
			t.Fatalf("actual-script runtime escaped its owned directory: got %%q, want beneath %%q: %%v", actual, want, err)
		}
	}
	if os.Getenv("TMPDIR") != os.Getenv("GOTMPDIR") || runtime.GOMAXPROCS(0) != 2 {
		t.Fatal("actual-script runtime lost its temp or CPU contract")
	}
}
`, name, phase)
}

// The mandatory early smoke does not accept the root suite's forwarded flags.
func TestSdkTestScriptSmokeRuntimeTemp(t *testing.T) {
	fixture := sdkRuntimeScriptFixture(t, "smoke")
	if output, _, err := fixture.run(t, "-count=1"); err != nil {
		t.Fatalf("smoke runtime boundary: %v\n%s", err, output)
	}
}

// Root-module tests retain the original uncached complete package sweep.
func TestSdkTestScriptRootRuntimeTemp(t *testing.T) {
	fixture := sdkRuntimeScriptFixture(t, "root")
	if output, trace, err := fixture.run(t, "-count=1"); err != nil {
		t.Fatalf("root runtime boundary: %v\n%s", err, output)
	} else if !slices.Equal(trace, []string{"go-test:.", "go-test:.", "go-test:nested", "make:js", "npm:js"}) {
		t.Fatalf("runtime split changed the complete SDK sequence: %q", trace)
	}
}

// Future immediate modules keep the same runtime boundary as today's modules.
func TestSdkTestScriptNestedRuntimeTemp(t *testing.T) {
	fixture := sdkRuntimeScriptFixture(t, "nested")
	if output, _, err := fixture.run(t, "-count=1"); err != nil {
		t.Fatalf("nested runtime boundary: %v\n%s", err, output)
	}
}

// Execute the actual JS script; make/npm stand-ins own no native output.
// Its separately selected companion still runs in a real Go test binary.
func TestSdkJsScriptCompanionRuntimeTemp(t *testing.T) {
	fixture := sdkRuntimeScriptFixture(t, "js-companion")
	source, err := os.ReadFile("../js/test.sh")
	if err != nil {
		t.Fatal(err)
	}
	fixture.write(t, "js/test.sh", string(source), 0700)
	fixture.write(t, "../bin/make", `#!/bin/sh
[ "$1" = smoke ] && [ "$PWD" = "$SDK_TEST_FIXTURE_ROOT/js" ] || exit 71
printf 'make:js-smoke\n' >>"$SDK_TEST_FIXTURE_TRACE"
`, 0700)
	fixture.write(t, "companion_test.go", sdkRuntimeChildSource("TestSubprotocolWasmCompanionRoundTrip", "js-companion"), 0600)
	command := exec.CommandContext(t.Context(), "zsh", "./test.sh")
	command.Dir = filepath.Join(fixture.directory, "js")
	command.Env = fixture.env
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("JS companion runtime boundary: %v\n%s", err, output)
	}
	trace, err := os.ReadFile(fixture.tracePath)
	if err != nil || string(trace) != "make:js-smoke\nnpm:js\ngo-test:js-companion\n" {
		t.Fatalf("JS runtime split changed make/npm/companion sequence: %q: %v", trace, err)
	}
}

// With no runner-owned runtime, the longstanding standalone environment stays.
// The unfiltered root sweep repeats smoke after the separate mandatory smoke.
func TestSdkTestScriptStandaloneTempUnchanged(t *testing.T) {
	fixture := newSdkTestScriptFixture(t)
	for i := 0; i < len(fixture.env); i++ {
		if strings.HasPrefix(fixture.env[i], "URNETWORK_SDK_TEST_RUNTIME_DIR=") {
			fixture.env = append(fixture.env[:i], fixture.env[i+1:]...)
			i--
		}
	}
	if output, trace, err := fixture.run(t, "-count=1"); err != nil {
		t.Fatalf("standalone launcher changed: %v\n%s", err, output)
	} else if !slices.Equal(trace, []string{"go-test:.", "smoke:.", "go-test:.", "smoke:.", "make:js", "npm:js"}) {
		t.Fatalf("standalone commands changed: %q", trace)
	}
}

// Reject explicit bad input rather than silently returning to compiler storage.
func TestSdkTestScriptRejectsInvalidRuntime(t *testing.T) {
	fixture := newSdkTestScriptFixture(t)
	fixture.env = append(fixture.env, "URNETWORK_SDK_TEST_RUNTIME_DIR=relative-runtime")
	output, trace, err := fixture.run(t, "-count=1")
	if err == nil || len(trace) != 0 || !strings.Contains(output, "SDK test runtime must be") {
		t.Fatalf("invalid runtime reached a Go child: %v, %q\n%s", err, trace, output)
	}
}
