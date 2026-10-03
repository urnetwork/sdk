// Keep generated native build output outside the source module's package tree.
package buildcontract

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Mirror the checkout's output boundary in a standalone, dependency-free module.
// An absent marker remains absent so the regression reaches the original build
// failure instead of stopping at a fixture file read.
func testingCgoOutputFixture(t *testing.T) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"go.mod":                  "module output-boundary.example/cgo\n\ngo 1.26.5\n",
		"source.go":               "package fixture\n",
		"gen/source.go":           "package main\nfunc main() {}\n",
		"buildcontract/source.go": "package buildcontract\n",
		"tooling/source.go":       "package tooling\n",
	}
	for path, source := range files {
		path = filepath.Join(directory, path)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(directory, "build"), 0o700); err != nil {
		t.Fatal(err)
	}
	boundary, err := os.ReadFile(filepath.Join(filepath.Dir(cgoMakefile(t)), "build", "go.mod"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err == nil {
		if err := os.WriteFile(filepath.Join(directory, "build", "go.mod"), boundary, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return directory
}

// The retained diagnostic intentionally lacks the hand-written binding helpers.
func testingCgoIncompleteOutput(t *testing.T, directory string) {
	t.Helper()
	outputDirectory := filepath.Join(directory, "build", "currentness-diagnostic.fixture")
	if err := os.MkdirAll(outputDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outputDirectory, "exports_gen.go"), []byte(
		"package main\nfunc binding() { cgoGuard(); cJson(); cString(); cStringFree() }\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Compile every source package with the same wildcard and race mode as test.sh.
// Keep child failure signatures captured even when an assertion reports an error.
func testingCgoOutputPackages(t *testing.T, directory, workspaceFile string) {
	t.Helper()
	command := exec.Command("go", "test", "-race", "-count=1", "-run=^$", "./...")
	command.Dir = directory
	command.Env = testingCgoGoEnvironment(workspaceFile)
	if output, err := command.CombinedOutput(); err != nil {
		diagnostic := strings.NewReplacer(
			"FAIL", "child failure", "panic:", "child panic:",
			"WARNING: DATA RACE", "child race report",
		).Replace(strings.TrimSpace(string(output)))
		t.Fatalf("source wildcard build returned %v; incomplete output compiled=%t; child output=%q",
			err, bytes.Contains(output, []byte("undefined: cgoGuard")), diagnostic)
	}
	command = exec.Command("go", "list", "./...")
	command.Dir = directory
	command.Env = testingCgoGoEnvironment(workspaceFile)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("source package discovery returned %v", err)
	}
	want := []string{
		"output-boundary.example/cgo",
		"output-boundary.example/cgo/buildcontract",
		"output-boundary.example/cgo/gen",
		"output-boundary.example/cgo/tooling",
	}
	if got := strings.Fields(string(output)); !slices.Equal(got, want) {
		t.Fatalf("discovered source packages = %q, want %q", got, want)
	}
}

// Replace inherited workspace selection while retaining the race and CPU budget.
func testingCgoGoEnvironment(workspaceFile string) []string {
	environment := slices.DeleteFunc(os.Environ(), func(value string) bool {
		return strings.HasPrefix(value, "GOWORK=")
	})
	return append(environment, "GOWORK="+workspaceFile)
}

// Hold incomplete output present through package discovery and compilation;
// neither a retained diagnostic nor a partially written file is source code.
func TestCgoBuildOutputModuleBoundary(t *testing.T) {
	directory := testingCgoOutputFixture(t)
	testingCgoIncompleteOutput(t, directory)
	testingCgoOutputPackages(t, directory, "off")
	if err := os.WriteFile(filepath.Join(directory, "build", "partial.go"), []byte("package"), 0o600); err != nil {
		t.Fatal(err)
	}
	testingCgoOutputPackages(t, directory, "off")
}

// Workspace discovery retains the same output boundary and still admits a
// separate source module named build, as the SDK's mobile tools require.
func TestCgoBuildOutputWorkspaceModuleBoundary(t *testing.T) {
	directory := testingCgoOutputFixture(t)
	testingCgoIncompleteOutput(t, directory)
	// Go matches member paths against the physical working directory, including
	// macOS's /var and /private/var temporary-directory aliases.
	workspaceDirectory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	buildDirectory := filepath.Join(workspaceDirectory, "sdk", "build")
	if err := os.MkdirAll(buildDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	for path, source := range map[string]string{
		"go.mod":    "module output-boundary.example/sdk/build\n\ngo 1.26.5\n",
		"source.go": "package build\n",
	} {
		if err := os.WriteFile(filepath.Join(buildDirectory, path), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	workspaceFile := filepath.Join(workspaceDirectory, "go.work")
	workspace := "go 1.26.5\nuse (\n" + strconv.Quote(directory) + "\n" + strconv.Quote(buildDirectory) + "\n)\n"
	if err := os.WriteFile(workspaceFile, []byte(workspace), 0o600); err != nil {
		t.Fatal(err)
	}
	testingCgoOutputPackages(t, directory, workspaceFile)
	command := exec.Command("go", "test", "-race", "-count=1", "./...")
	command.Dir = buildDirectory
	command.Env = testingCgoGoEnvironment(workspaceFile)
	output, err := command.CombinedOutput()
	if err != nil || !bytes.Contains(output, []byte("output-boundary.example/sdk/build")) {
		t.Fatalf("workspace omitted or failed the source build module: %v", err)
	}
}

// Keep the boundary present throughout cleanup, including while rm executes.
// Empty output, unknown children, hidden files and child links share the rule.
func TestCgoBuildOutputCleanPreservesModuleBoundary(t *testing.T) {
	directory := testingCgoOutputFixture(t)
	testingCgoIncompleteOutput(t, directory)
	buildDirectory := filepath.Join(directory, "build")
	boundary, err := os.ReadFile(filepath.Join(buildDirectory, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".hidden", "unknown output.zip"} {
		if err := os.WriteFile(filepath.Join(buildDirectory, name), []byte("output"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	externalDirectory := t.TempDir()
	externalFile := filepath.Join(externalDirectory, "preserved")
	if err := os.WriteFile(externalFile, []byte("external"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(externalDirectory, filepath.Join(buildDirectory, "linked-output")); err != nil {
		t.Fatal(err)
	}
	binDirectory := filepath.Join(directory, "bin")
	if err := os.Mkdir(binDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(binDirectory, "rm"), `#!/bin/sh
set -eu
test -f build/go.mod
/bin/rm "$@"
test -f build/go.mod
`)
	t.Setenv("PATH", binDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
	for range 2 {
		if _, err := runMake(t, directory, "clean"); err != nil {
			t.Fatalf("clean removed the output boundary or failed: %v", err)
		}
		entries, err := os.ReadDir(buildDirectory)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != "go.mod" {
			t.Fatalf("clean retained unexpected output entries: %v", entries)
		}
		current, err := os.ReadFile(filepath.Join(buildDirectory, "go.mod"))
		if err != nil || !bytes.Equal(current, boundary) {
			t.Fatalf("clean changed the output boundary: %v", err)
		}
	}
	if contents, err := os.ReadFile(externalFile); err != nil || string(contents) != "external" {
		t.Fatalf("clean followed a child link: %v", err)
	}
	testingCgoIncompleteOutput(t, directory)
	testingCgoOutputPackages(t, directory, "off")
}

// A missing output root is already clean; unexpected root types are preserved.
func TestCgoBuildOutputCleanValidatesRoot(t *testing.T) {
	directory := testingCgoOutputFixture(t)
	buildDirectory := filepath.Join(directory, "build")
	if err := os.RemoveAll(buildDirectory); err != nil {
		t.Fatal(err)
	}
	if _, err := runMake(t, directory, "clean"); err != nil {
		t.Fatalf("clean rejected an absent output root: %v", err)
	}
	externalDirectory := t.TempDir()
	externalFile := filepath.Join(externalDirectory, "preserved")
	if err := os.WriteFile(externalFile, []byte("external"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(externalDirectory, buildDirectory); err != nil {
		t.Fatal(err)
	}
	output, err := runMake(t, directory, "clean")
	if err == nil || !bytes.Contains(output, []byte("symbolic cgo build directory")) {
		t.Fatalf("clean did not reject the symbolic output root: %v", err)
	}
	if contents, err := os.ReadFile(externalFile); err != nil || string(contents) != "external" {
		t.Fatalf("clean changed the symbolic root's target: %v", err)
	}
	if err := os.Remove(buildDirectory); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(buildDirectory, []byte("unexpected root file"), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err = runMake(t, directory, "clean")
	if err == nil || !bytes.Contains(output, []byte("cgo build path is not a directory")) {
		t.Fatalf("clean did not reject the non-directory output root: %v", err)
	}
	if contents, err := os.ReadFile(buildDirectory); err != nil || string(contents) != "unexpected root file" {
		t.Fatalf("clean changed the non-directory output root: %v", err)
	}
}
