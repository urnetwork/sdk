// Exercise the SDK suite launcher with real Go target selection and synthetic
// standard-library modules. The fixture gate and npm command own no resources.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// Owns a complete disposable workspace so the launcher cannot run SDK tests.
type sdkTestScriptFixture struct {
	directory string
	env       []string
	tracePath string
}

// Installs the current launcher and records real Go test invocations. Child
// output stays captured, including intentionally failing fixture processes.
func newSdkTestScriptFixture(t *testing.T) *sdkTestScriptFixture {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("the SDK suite launcher needs a POSIX host with zsh")
	}
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Fatal(err)
	}
	goPath, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	launcher, err := os.ReadFile("../test.sh")
	if err != nil {
		t.Fatal(err)
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(base, "workspace with spaces")
	fixture := &sdkTestScriptFixture{
		directory: filepath.Join(workspace, "sdk"),
		tracePath: filepath.Join(workspace, "trace"),
	}
	fixture.write(t, "test.sh", string(launcher), 0o700)
	fixture.write(t, "../tests/network-intensive-suite-lock.sh", `#!/bin/sh
if [ "$1" = --verify-held ]; then
  [ "$2" = run-all ] && [ "$URNETWORK_NETWORK_TEST_LOCK_HELD" = 1 ]
  exit $?
fi
[ "$1" = run-all ] && [ "$2" = run-all-sdk ] && [ "$3" = -- ] || exit 70
shift 3
exec env URNETWORK_NETWORK_TEST_LOCK_HELD=1 "$@"
`, 0o700)
	fixture.write(t, "../bin/go", `#!/bin/sh
if [ "$1" = test ]; then
  if [ "$PWD" = "$SDK_TEST_FIXTURE_ROOT" ]; then module=.; else module="${PWD#"$SDK_TEST_FIXTURE_ROOT"/}"; fi
  printf 'go-test:%s\n' "$module" >>"$SDK_TEST_FIXTURE_TRACE"
fi
exec "$SDK_TEST_FIXTURE_GO" "$@"
`, 0o700)
	fixture.write(t, "../bin/npm", `#!/bin/sh
[ "$1" = test ] && [ "$PWD" = "$SDK_TEST_FIXTURE_ROOT/js" ] || exit 71
printf 'npm:js\n' >>"$SDK_TEST_FIXTURE_TRACE"
`, 0o700)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "URNETWORK_NETWORK_TEST_") || strings.HasPrefix(name, "SDK_TEST_FIXTURE_") {
			continue
		}
		switch name {
		case "PATH", "URNETWORK_ROOT", "GOWORK", "GOENV", "GOTOOLCHAIN":
			continue
		}
		fixture.env = append(fixture.env, entry)
	}
	fixture.env = append(fixture.env,
		"PATH="+filepath.Join(workspace, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
		"URNETWORK_ROOT="+workspace,
		"SDK_TEST_FIXTURE_ROOT="+fixture.directory,
		"SDK_TEST_FIXTURE_TRACE="+fixture.tracePath,
		"SDK_TEST_FIXTURE_GO="+goPath,
		"GOWORK=off", "GOENV=off", "GOTOOLCHAIN=local",
	)
	fixture.module(t, ".")
	fixture.write(t, "fixture_test.go", sdkTestScriptSource("fixture", ""), 0o600)
	fixture.write(t, "js/package.json", "{}\n", 0o600)
	return fixture
}

// Writes only within the fixture workspace, including its sibling tool stubs.
func (self *sdkTestScriptFixture) write(t *testing.T, relative, data string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(self.directory, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}

// Gives modules synthetic import paths that stay valid when directories contain spaces.
func (self *sdkTestScriptFixture) module(t *testing.T, relative string) {
	t.Helper()
	name := strings.ReplaceAll(relative, " ", "_")
	if name == "." {
		name = "root"
	}
	self.write(t, filepath.Join(relative, "go.mod"), "module sdk-fixture.example/"+name+"\n\ngo 1.26\n", 0o600)
}

// Returns a tiny test whose execution is observable without runtime services.
func sdkTestScriptSource(packageName, constraint string) string {
	if constraint != "" {
		constraint = "//go:build " + constraint + "\n\n"
	}
	return constraint + "package " + packageName + "\n\nimport \"testing\"\n\nfunc TestFixture(t *testing.T) {}\n"
}

// Runs and joins every fixture command; no nested failure output reaches the suite log.
func (self *sdkTestScriptFixture) run(t *testing.T, args ...string) (string, []string, error) {
	t.Helper()
	command := exec.CommandContext(t.Context(), "zsh", append([]string{"./test.sh"}, args...)...)
	command.Dir = self.directory
	command.Env = self.env
	output, err := command.CombinedOutput()
	trace, readErr := os.ReadFile(self.tracePath)
	if readErr != nil {
		t.Fatalf("reading fixture trace: %v", readErr)
	}
	return string(output), strings.Split(strings.TrimSpace(string(trace)), "\n"), err
}

// A wasm-only module must not abort host testing before packaging and npm.
// Renamed modules, race tags, external tests, filename targets, and nested
// module boundaries make admission depend on Go semantics rather than names.
func TestSdkTestScriptHostModuleDiscovery(t *testing.T) {
	fixture := newSdkTestScriptFixture(t)
	for _, name := range []string{"build", "cgo", "js", "packaging", "raceonly", "renamed", "testonly", "untested", "zz-parent"} {
		fixture.module(t, name)
	}
	fixture.write(t, "build/fixture_test.go", sdkTestScriptSource("fixture", ""), 0o600)
	fixture.write(t, "cgo/gen/fixture_test.go", sdkTestScriptSource("fixture", ""), 0o600)
	fixture.write(t, "js/fixture_wasm_test.go", sdkTestScriptSource("fixture", "js"), 0o600)
	fixture.write(t, "packaging/fixture_test.go", sdkTestScriptSource("fixture_test", ""), 0o600)
	fixture.write(t, "raceonly/fixture_test.go", sdkTestScriptSource("fixture", "race"), 0o600)
	fixture.write(t, "renamed/fixture_wasm_test.go", sdkTestScriptSource("fixture", "js"), 0o600)
	fixture.write(t, "testonly/fixture_test.go", sdkTestScriptSource("fixture", ""), 0o600)
	fixture.write(t, "untested/fixture.go", "package fixture\n", 0o600)
	fixture.write(t, "untested/fixture_windows_test.go", sdkTestScriptSource("fixture", ""), 0o600)
	fixture.write(t, "zz-parent/fixture_wasm_test.go", sdkTestScriptSource("fixture", "js"), 0o600)
	fixture.module(t, "zz-parent/child")
	fixture.write(t, "zz-parent/child/fixture_test.go", sdkTestScriptSource("fixture", ""), 0o600)
	output, trace, err := fixture.run(t, "-count=1", "-run", "^TestFixture$")
	if err != nil {
		t.Fatalf("host module discovery aborted: %v\n%s", err, output)
	}
	expected := []string{"go-test:.", "go-test:build", "go-test:cgo", "go-test:packaging", "go-test:raceonly", "go-test:testonly", "npm:js"}
	if !slices.Equal(trace, expected) {
		t.Fatalf("module commands = %q, want %q", trace, expected)
	}
}

// Build constraints selected through either the CLI or GOFLAGS must govern
// both discovery and execution, including a js-named module with host tests.
func TestSdkTestScriptBuildTags(t *testing.T) {
	for _, options := range []struct {
		args     []string
		flags    string
		selected bool
	}{
		{args: []string{"-tags", "fixture_tag"}, selected: true},
		{args: []string{"-tags=fixture_tag"}, selected: true},
		{flags: "-tags=fixture_tag", selected: true},
		{args: []string{"-run", "-tags", "-tags", "fixture_tag"}, selected: true},
		{args: []string{"-tags=other_fixture_tag"}, flags: "-tags=fixture_tag", selected: false},
	} {
		fixture := newSdkTestScriptFixture(t)
		fixture.module(t, "js")
		fixture.write(t, "js/fixture_test.go", sdkTestScriptSource("fixture", "fixture_tag"), 0o600)
		fixture.module(t, "space module")
		fixture.write(t, "space module/fixture_test.go", sdkTestScriptSource("fixture", "fixture_tag"), 0o600)
		if options.flags != "" {
			fixture.env = append(fixture.env, "GOFLAGS="+os.Getenv("GOFLAGS")+" "+options.flags)
		}
		args := append([]string{"-count=1", "-run", "^TestFixture$"}, options.args...)
		output, trace, err := fixture.run(t, args...)
		if err != nil {
			t.Fatalf("tags args=%q flags=%q: %v\n%s", options.args, options.flags, err, output)
		}
		expected := []string{"go-test:.", "npm:js"}
		if options.selected {
			expected = []string{"go-test:.", "go-test:js", "go-test:space module", "npm:js"}
		}
		if !slices.Equal(trace, expected) {
			t.Fatalf("tags args=%q flags=%q: commands=%q, want %q", options.args, options.flags, trace, expected)
		}
	}
}

// Empty-module admission must not turn real package-loading errors into skips.
func TestSdkTestScriptDiscoveryErrorStopsSuite(t *testing.T) {
	fixture := newSdkTestScriptFixture(t)
	fixture.module(t, "broken")
	fixture.write(t, "broken/go.mod", "module\n", 0o600)
	fixture.write(t, "broken/fixture_test.go", sdkTestScriptSource("fixture", ""), 0o600)
	fixture.module(t, "packaging")
	fixture.write(t, "packaging/fixture_test.go", sdkTestScriptSource("fixture", ""), 0o600)
	output, trace, err := fixture.run(t, "-run", "^TestFixture$")
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 1 || !strings.Contains(output, "errors parsing go.mod") {
		t.Fatalf("malformed module result: %v\n%s", err, output)
	}
	if !slices.Equal(trace, []string{"go-test:."}) {
		t.Fatalf("discovery error continued the suite: %q", trace)
	}
}

// A failed host test remains terminal and is never retried or followed by npm.
func TestSdkTestScriptHostFailureStopsSuite(t *testing.T) {
	fixture := newSdkTestScriptFixture(t)
	fixture.module(t, "build")
	fixture.write(t, "build/fixture_test.go", fmt.Sprintf("package fixture\nimport \"testing\"\nfunc TestFixture(t *testing.T) { t.Fatal(%q) }\n", "synthetic fixture failure"), 0o600)
	fixture.module(t, "packaging")
	fixture.write(t, "packaging/fixture_test.go", sdkTestScriptSource("fixture", ""), 0o600)
	output, trace, err := fixture.run(t, "-count=1", "-run", "^TestFixture$")
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 1 || !strings.Contains(output, "synthetic fixture failure") {
		t.Fatalf("host failure result: %v\n%s", err, output)
	}
	if !slices.Equal(trace, []string{"go-test:.", "go-test:build"}) {
		t.Fatalf("host failure retried or continued the suite: %q", trace)
	}
}
