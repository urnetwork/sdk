// Neither this repository nor any library it ships depends on messaging, which lives in
// github.com/urnetwork/message. Two gates hold the boundary, and each sees what the other cannot:
//
//   - every module file names no messaging module, and every .go file, whatever its build
//     constraints, imports no messaging package. This reads files no build compiles.
//   - for every build this repository ships, the package closure `go list -deps` resolves holds no
//     messaging package. This reads what a file pulls in transitively.
//
// Until connect's own removal lands, connect/protocol still carries the messaging schema, so a
// core binary still links it; that package is generic transport and is not on the list.
package sdk

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Package path roots a core build must never contain, each with what it is. A path is under a root
// when it equals the root or continues it with a "/", so github.com/urnetwork/message-server is
// its own root and github.com/urnetwork/messagex is under none.
var messagingBoundaryRoots = map[string]string{
	"github.com/urnetwork/message":              "the messaging module",
	"github.com/urnetwork/message-server":       "messaging's server",
	"github.com/urnetwork/connect/message":      "connect's record layer, which moves to the messaging module",
	"github.com/urnetwork/connect/messagegroup": "connect's group sessions, which move to the messaging module",
	"github.com/urnetwork/connect/mls":          "connect's mls and its codec, which move to the messaging module",
	"github.com/urnetwork/sdk/urmessage":        "this repository's former messaging package",
}

// The root a package path falls under, if any.
func messagingBoundaryRootOf(path string) (string, bool) {
	for root := range messagingBoundaryRoots {
		if path == root || strings.HasPrefix(path, root+"/") {
			return root, true
		}
	}
	return "", false
}

// Directory names the repository walks do not enter: git's own objects, and javascript
// dependencies that hold no go.
var messagingBoundarySkippedDirectoryNames = map[string]string{
	".git":         "git's own objects",
	"node_modules": "javascript dependencies, which hold no go",
}

// One build this repository ships, as `go list -deps` resolves it.
type messagingBoundaryLeg struct {
	name string
	// the module directory, relative to the repository root
	moduleDirectory string
	// GOOS, GOARCH and CGO_ENABLED, as KEY=value
	environment []string
	tags        string
	test        bool
	patterns    []string
	// a package the closure must hold, so an empty or wrong closure cannot pass
	mustContain string
}

// Every build this repository ships. The root module on each desktop platform, the browser build,
// the c library on each desktop platform, the gomobile binding on each mobile target, and the two
// tool modules. A module that holds no package is disposed of in messagingBoundaryModulesWithoutABuild.
var messagingBoundaryLegs = []messagingBoundaryLeg{
	{name: "root linux/amd64", moduleDirectory: ".", environment: []string{"GOOS=linux", "GOARCH=amd64"}, test: true, patterns: []string{"./..."}, mustContain: "github.com/urnetwork/sdk"},
	{name: "root windows/amd64", moduleDirectory: ".", environment: []string{"GOOS=windows", "GOARCH=amd64"}, test: true, patterns: []string{"./..."}, mustContain: "github.com/urnetwork/sdk"},
	{name: "root darwin/arm64", moduleDirectory: ".", environment: []string{"GOOS=darwin", "GOARCH=arm64"}, test: true, patterns: []string{"./..."}, mustContain: "github.com/urnetwork/sdk"},
	{name: "js/wasm", moduleDirectory: "js", environment: []string{"GOOS=js", "GOARCH=wasm"}, test: true, patterns: []string{"./..."}, mustContain: "github.com/urnetwork/sdk"},
	{name: "cgo linux/amd64", moduleDirectory: "cgo", environment: []string{"GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=1"}, test: true, patterns: []string{"./..."}, mustContain: "github.com/urnetwork/sdk"},
	{name: "cgo windows/amd64", moduleDirectory: "cgo", environment: []string{"GOOS=windows", "GOARCH=amd64", "CGO_ENABLED=1"}, test: true, patterns: []string{"./..."}, mustContain: "github.com/urnetwork/sdk"},
	{name: "cgo darwin/arm64", moduleDirectory: "cgo", environment: []string{"GOOS=darwin", "GOARCH=arm64", "CGO_ENABLED=1"}, test: true, patterns: []string{"./..."}, mustContain: "github.com/urnetwork/sdk"},
	{name: "gomobile android/arm64", moduleDirectory: ".", environment: []string{"GOOS=android", "GOARCH=arm64", "CGO_ENABLED=1"}, tags: "sdk_mobile_bind", patterns: []string{"."}, mustContain: "github.com/urnetwork/sdk"},
	{name: "gomobile ios/arm64", moduleDirectory: ".", environment: []string{"GOOS=ios", "GOARCH=arm64", "CGO_ENABLED=1"}, tags: "sdk_mobile_bind", patterns: []string{"."}, mustContain: "github.com/urnetwork/sdk"},
	{name: "gomobile ios/arm64 extension", moduleDirectory: ".", environment: []string{"GOOS=ios", "GOARCH=arm64", "CGO_ENABLED=1"}, tags: "sdk_mobile_bind,ios_extension", patterns: []string{"."}, mustContain: "github.com/urnetwork/sdk"},
	{name: "gomobile macos/arm64", moduleDirectory: ".", environment: []string{"GOOS=darwin", "GOARCH=arm64", "CGO_ENABLED=1"}, tags: "sdk_mobile_bind", patterns: []string{"."}, mustContain: "github.com/urnetwork/sdk"},
	{name: "build tools linux/amd64", moduleDirectory: "build", environment: []string{"GOOS=linux", "GOARCH=amd64"}, test: true, patterns: []string{"./..."}, mustContain: "github.com/urnetwork/sdk"},
	{name: "packaging linux/amd64", moduleDirectory: "packaging", environment: []string{"GOOS=linux", "GOARCH=amd64"}, test: true, patterns: []string{"./..."}, mustContain: "github.com/urnetwork/sdk/packaging"},
}

// Modules that hold no package, so no build of theirs can link anything, with why.
var messagingBoundaryModulesWithoutABuild = map[string]string{
	"cgo/build": "an empty module that keeps the cgo library's build output out of the cgo module",
}

// Every go.mod-shaped file under root, and what each names that is under a messaging root.
// go.mod-shaped is a file named go.mod or ending in .go.mod, the shape of an alternate modfile. The
// file is read by `go mod edit -json`, the go command's own parser.
func messagingBoundaryModuleFileFindings(t *testing.T, walkRoot string) (moduleFiles []string, findings []string) {
	err := filepath.Walk(walkRoot, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			if _, skipped := messagingBoundarySkippedDirectoryNames[info.Name()]; skipped && path != walkRoot {
				return filepath.SkipDir
			}
			return nil
		}
		if info.Name() != "go.mod" && !strings.HasSuffix(info.Name(), ".go.mod") {
			return nil
		}
		rel, _ := filepath.Rel(walkRoot, path)
		rel = filepath.ToSlash(rel)
		moduleFiles = append(moduleFiles, rel)
		output, err := exec.Command("go", "mod", "edit", "-json", path).Output()
		if err != nil {
			stderr := ""
			var exitError *exec.ExitError
			if errors.As(err, &exitError) {
				stderr = string(exitError.Stderr)
			}
			return fmt.Errorf("go mod edit -json %s: %w\n%s", rel, err, stderr)
		}
		var parsed struct {
			Module struct {
				Path string
			}
			Require []struct {
				Path string
			}
			Exclude []struct {
				Path string
			}
			Replace []struct {
				Old struct {
					Path string
				}
				New struct {
					Path string
				}
			}
			Tool []struct {
				Path string
			}
		}
		if err := json.Unmarshal(output, &parsed); err != nil {
			return fmt.Errorf("decoding go mod edit -json %s: %w", rel, err)
		}
		named := map[string]string{"module": parsed.Module.Path}
		for _, require := range parsed.Require {
			named["require "+require.Path] = require.Path
		}
		for _, exclude := range parsed.Exclude {
			named["exclude "+exclude.Path] = exclude.Path
		}
		for _, replace := range parsed.Replace {
			named["replace "+replace.Old.Path] = replace.Old.Path
			named["replace => "+replace.New.Path] = replace.New.Path
		}
		for _, tool := range parsed.Tool {
			named["tool "+tool.Path] = tool.Path
		}
		for entry, namedPath := range named {
			if boundaryRoot, under := messagingBoundaryRootOf(namedPath); under {
				findings = append(findings, fmt.Sprintf("%s: %s is under %s", rel, entry, boundaryRoot))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", walkRoot, err)
	}
	sort.Strings(moduleFiles)
	sort.Strings(findings)
	return moduleFiles, findings
}

// Every import of every .go file under root that is under a messaging root, read off the syntax so
// build constraints, aliases, blank and dot imports, and test files are all in scope.
func messagingBoundaryImportFindings(t *testing.T, walkRoot string) (goFileCount int, importCount int, findings []string) {
	fileSet := token.NewFileSet()
	err := filepath.Walk(walkRoot, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			if _, skipped := messagingBoundarySkippedDirectoryNames[info.Name()]; skipped && path != walkRoot {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		parsed, err := parser.ParseFile(fileSet, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(walkRoot, path)
		rel = filepath.ToSlash(rel)
		goFileCount += 1
		for _, spec := range parsed.Imports {
			importCount += 1
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if boundaryRoot, under := messagingBoundaryRootOf(importPath); under {
				position := fileSet.Position(spec.Pos())
				findings = append(findings, fmt.Sprintf("%s:%d:%d imports %s, under %s", rel, position.Line, position.Column, importPath, boundaryRoot))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", walkRoot, err)
	}
	sort.Strings(findings)
	return goFileCount, importCount, findings
}

// The packages one leg's `go list -deps` resolves, and those under a messaging root. GOFLAGS is
// cleared so the leg is exactly what it names, and GOWORK is off so the closure is this module's.
// Only stdout is read as the package list; stderr carries progress such as module downloads.
func messagingBoundaryLegPackages(t *testing.T, repositoryRoot string, leg messagingBoundaryLeg) (packages []string, findings []string) {
	arguments := []string{"list", "-deps"}
	if leg.test {
		arguments = append(arguments, "-test")
	}
	if leg.tags != "" {
		arguments = append(arguments, "-tags", leg.tags)
	}
	arguments = append(arguments, leg.patterns...)
	command := exec.Command("go", arguments...)
	command.Dir = filepath.Join(repositoryRoot, filepath.FromSlash(leg.moduleDirectory))
	command.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off")
	command.Env = append(command.Env, leg.environment...)
	output, err := command.Output()
	if err != nil {
		stderr := ""
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			stderr = string(exitError.Stderr)
		}
		t.Fatalf("%s: go %s in %s: %v\n%s", leg.name, strings.Join(arguments, " "), leg.moduleDirectory, err, stderr)
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.ReplaceAll(string(output), "\r\n", "\n"), "\n") {
		// a test variant prints as "path [path.test]"; the package is the path
		packagePath, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		if packagePath == "" || seen[packagePath] {
			continue
		}
		seen[packagePath] = true
		packages = append(packages, packagePath)
		if root, under := messagingBoundaryRootOf(packagePath); under {
			findings = append(findings, fmt.Sprintf("%s links %s, under %s", leg.name, packagePath, root))
		}
	}
	sort.Strings(packages)
	sort.Strings(findings)
	return packages, findings
}

// The matcher takes whole path elements: a root and anything beneath it, never a longer name that
// shares its prefix. Every root has a row here, and every row's root is on the list, both ways.
func TestMessagingBoundaryMatchesWholePathElements(t *testing.T) {
	under := map[string]string{
		"github.com/urnetwork/message":                    "github.com/urnetwork/message",
		"github.com/urnetwork/message/protocol":           "github.com/urnetwork/message",
		"github.com/urnetwork/message/sdk/urmessage":      "github.com/urnetwork/message",
		"github.com/urnetwork/message-server":             "github.com/urnetwork/message-server",
		"github.com/urnetwork/message-server/api":         "github.com/urnetwork/message-server",
		"github.com/urnetwork/connect/message":            "github.com/urnetwork/connect/message",
		"github.com/urnetwork/connect/messagegroup":       "github.com/urnetwork/connect/messagegroup",
		"github.com/urnetwork/connect/messagegroup/inner": "github.com/urnetwork/connect/messagegroup",
		"github.com/urnetwork/connect/mls":                "github.com/urnetwork/connect/mls",
		"github.com/urnetwork/connect/mls/syntax":         "github.com/urnetwork/connect/mls",
		"github.com/urnetwork/sdk/urmessage":              "github.com/urnetwork/sdk/urmessage",
	}
	outside := []string{
		"github.com/urnetwork/messagex",
		"github.com/urnetwork/message-serverx",
		"github.com/urnetwork/messages/protocol",
		"github.com/urnetwork/connect",
		"github.com/urnetwork/connect/protocol",
		"github.com/urnetwork/connect/messages",
		"github.com/urnetwork/connect/mlsx",
		"github.com/urnetwork/sdk",
		"github.com/urnetwork/sdk/urmessagex",
		"example.com/github.com/urnetwork/message",
	}
	exercised := map[string]bool{}
	for path, want := range under {
		root, found := messagingBoundaryRootOf(path)
		if !found || root != want {
			t.Errorf("%s: matched %q (%v), want %q", path, root, found, want)
		}
		exercised[want] = true
	}
	for _, path := range outside {
		if root, found := messagingBoundaryRootOf(path); found {
			t.Errorf("%s matched %s, and shares only a prefix with it", path, root)
		}
	}
	for root := range messagingBoundaryRoots {
		if !exercised[root] {
			t.Errorf("no row here exercises the root %s", root)
		}
	}
	for root := range exercised {
		if _, listed := messagingBoundaryRoots[root]; !listed {
			t.Errorf("a row expects the root %s, which is not on the list", root)
		}
	}
}

// The two syntactic scans find exactly what a fixture plants: a require, a replace and a tool
// entry in two module files, an aliased import behind a constraint no build passes, and a dot
// import in a test file; and nothing for paths that only share a prefix with a root.
func TestTheMessagingBoundaryScansFindWhatAFixturePlants(t *testing.T) {
	fixture := t.TempDir()
	fixtureFiles := map[string]string{
		"go.mod":       "module fixture.example/core\n\ngo 1.26\n\nrequire github.com/urnetwork/message v0.0.0\n\nreplace github.com/urnetwork/message-server => ../server\n",
		"alt.go.mod":   "module fixture.example/alt\n\ngo 1.26\n\ntool github.com/urnetwork/message/cmd/fixture\n",
		"never.go":     "//go:build urnet_fixture_never\n\npackage core\n\nimport group \"github.com/urnetwork/connect/messagegroup\"\n\nvar _ = group.Fixture\n",
		"core.go":      "package core\n\nimport (\n\t_ \"github.com/urnetwork/messagex\"\n\t_ \"github.com/urnetwork/connect/messages\"\n\t_ \"github.com/urnetwork/message-serverx\"\n)\n",
		"core_test.go": "package core\n\nimport . \"github.com/urnetwork/sdk/urmessage\"\n",
	}
	for name, content := range fixtureFiles {
		if err := os.WriteFile(filepath.Join(fixture, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	moduleFiles, moduleFindings := messagingBoundaryModuleFileFindings(t, fixture)
	if !slices.Equal(moduleFiles, []string{"alt.go.mod", "go.mod"}) {
		t.Errorf("module files found: %v, want alt.go.mod and go.mod", moduleFiles)
	}
	wantModuleFindings := []string{
		"alt.go.mod: tool github.com/urnetwork/message/cmd/fixture is under github.com/urnetwork/message",
		"go.mod: replace github.com/urnetwork/message-server is under github.com/urnetwork/message-server",
		"go.mod: require github.com/urnetwork/message is under github.com/urnetwork/message",
	}
	if !slices.Equal(moduleFindings, wantModuleFindings) {
		t.Errorf("module file findings:\n%s\nwant:\n%s", strings.Join(moduleFindings, "\n"), strings.Join(wantModuleFindings, "\n"))
	}
	goFileCount, importCount, importFindings := messagingBoundaryImportFindings(t, fixture)
	if goFileCount != 3 || importCount != 5 {
		t.Errorf("the import scan read %d .go files and %d imports, want 3 and 5", goFileCount, importCount)
	}
	wantImportFindings := []string{
		"core_test.go:3:8 imports github.com/urnetwork/sdk/urmessage, under github.com/urnetwork/sdk/urmessage",
		"never.go:5:8 imports github.com/urnetwork/connect/messagegroup, under github.com/urnetwork/connect/messagegroup",
	}
	if !slices.Equal(importFindings, wantImportFindings) {
		t.Errorf("import findings:\n%s\nwant:\n%s", strings.Join(importFindings, "\n"), strings.Join(wantImportFindings, "\n"))
	}
}

// No module file in this repository names a messaging module, and no .go file imports a messaging
// package, whatever its build constraints.
func TestNoModuleFileOrImportInThisRepositoryNamesMessaging(t *testing.T) {
	moduleFiles, moduleFindings := messagingBoundaryModuleFileFindings(t, ".")
	if !slices.Contains(moduleFiles, "go.mod") || !slices.Contains(moduleFiles, "cgo/go.mod") {
		t.Fatalf("CONTROL FAILED: the module files found are %v, which lacks this repository's own", moduleFiles)
	}
	for _, finding := range moduleFindings {
		t.Errorf("a module file names messaging: %s", finding)
	}
	goFileCount, importCount, importFindings := messagingBoundaryImportFindings(t, ".")
	if goFileCount == 0 || importCount == 0 {
		t.Fatalf("CONTROL FAILED: the import scan read %d .go files and %d imports", goFileCount, importCount)
	}
	for _, finding := range importFindings {
		t.Errorf("a .go file imports messaging: %s", finding)
	}
	skipped := []string{}
	for name, why := range messagingBoundarySkippedDirectoryNames {
		skipped = append(skipped, name+" ("+why+")")
	}
	sort.Strings(skipped)
	t.Logf("%d module files read: %v", len(moduleFiles), moduleFiles)
	t.Logf("%d .go files read, %d imports; directory names not entered: %v", goFileCount, importCount, skipped)
}

// No build this repository ships links a messaging package. Every module file is either a leg's
// module or disposed of as holding no package, both ways, so a module added later is either
// checked or written down.
func TestNoShippedBuildLinksAMessagingPackage(t *testing.T) {
	moduleFiles, _ := messagingBoundaryModuleFileFindings(t, ".")
	moduleDirectories := map[string]bool{}
	for _, moduleFile := range moduleFiles {
		if filepath.Base(moduleFile) != "go.mod" {
			t.Errorf("%s is an alternate module file, and no leg here builds with it; give it a leg", moduleFile)
			continue
		}
		directory := filepath.ToSlash(filepath.Dir(filepath.FromSlash(moduleFile)))
		moduleDirectories[directory] = true
	}
	covered := map[string]bool{}
	for _, leg := range messagingBoundaryLegs {
		covered[leg.moduleDirectory] = true
	}
	for directory, why := range messagingBoundaryModulesWithoutABuild {
		if covered[directory] {
			t.Errorf("%s is disposed of as holding no package (%s) and a leg builds it", directory, why)
		}
		covered[directory] = true
	}
	for directory := range moduleDirectories {
		if !covered[directory] {
			t.Errorf("the module in %s has no leg and no disposition", directory)
		}
	}
	for directory := range covered {
		if !moduleDirectories[directory] {
			t.Errorf("a leg or disposition names %s, which holds no go.mod", directory)
		}
	}

	for _, leg := range messagingBoundaryLegs {
		packages, findings := messagingBoundaryLegPackages(t, ".", leg)
		if !slices.Contains(packages, leg.mustContain) {
			t.Errorf("CONTROL FAILED: %s resolved %d packages and not %s", leg.name, len(packages), leg.mustContain)
		}
		for _, finding := range findings {
			t.Errorf("%s", finding)
		}
		t.Logf("%s: go list -deps (test=%v tags=%q %v) in %s: %d packages, %d under a messaging root",
			leg.name, leg.test, leg.tags, leg.environment, leg.moduleDirectory, len(packages), len(findings))
	}
}

// The leg runner applies each part of a leg: a fixture module plants a messaging import behind
// each of GOOS=js, the sdk_mobile_bind tag, cgo and a test file, and each leg configuration finds
// exactly its own. A runner that dropped a leg's environment, tags or -test would find the wrong
// set here.
func TestTheMessagingBoundaryLegsReadTheirOwnBuildConfiguration(t *testing.T) {
	fixture := t.TempDir()
	fixtureFiles := map[string]string{
		"core/go.mod":           "module fixture.example/core\n\ngo 1.26\n\nrequire github.com/urnetwork/message v0.0.0\n\nreplace github.com/urnetwork/message => ../message\n",
		"core/core.go":          "package core\n",
		"core/core_js.go":       "//go:build js\n\npackage core\n\nimport _ \"github.com/urnetwork/message/browser\"\n",
		"core/core_mobile.go":   "//go:build sdk_mobile_bind\n\npackage core\n\nimport _ \"github.com/urnetwork/message/mobile\"\n",
		"core/core_cgo.go":      "//go:build cgo\n\npackage core\n\nimport _ \"github.com/urnetwork/message/native\"\n",
		"core/core_test.go":     "package core\n\nimport _ \"github.com/urnetwork/message/testonly\"\n",
		"message/go.mod":        "module github.com/urnetwork/message\n\ngo 1.26\n",
		"message/browser/b.go":  "package browser\n",
		"message/mobile/m.go":   "package mobile\n",
		"message/native/n.go":   "package native\n",
		"message/testonly/t.go": "package testonly\n",
	}
	for name, content := range fixtureFiles {
		path := filepath.Join(fixture, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		leg  messagingBoundaryLeg
		want []string
	}{
		{
			leg:  messagingBoundaryLeg{name: "host", environment: []string{"GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0"}},
			want: []string{},
		},
		{
			leg:  messagingBoundaryLeg{name: "browser", environment: []string{"GOOS=js", "GOARCH=wasm", "CGO_ENABLED=0"}},
			want: []string{"github.com/urnetwork/message/browser"},
		},
		{
			leg:  messagingBoundaryLeg{name: "mobile", environment: []string{"GOOS=android", "GOARCH=arm64", "CGO_ENABLED=0"}, tags: "sdk_mobile_bind"},
			want: []string{"github.com/urnetwork/message/mobile"},
		},
		{
			leg:  messagingBoundaryLeg{name: "library", environment: []string{"GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=1"}},
			want: []string{"github.com/urnetwork/message/native"},
		},
		{
			leg:  messagingBoundaryLeg{name: "tests", environment: []string{"GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0"}, test: true},
			want: []string{"github.com/urnetwork/message/testonly"},
		},
	} {
		c.leg.moduleDirectory = "core"
		c.leg.patterns = []string{"."}
		packages, findings := messagingBoundaryLegPackages(t, fixture, c.leg)
		if !slices.Contains(packages, "fixture.example/core") {
			t.Errorf("%s: the closure %v lacks the fixture package itself", c.leg.name, packages)
		}
		found := []string{}
		for _, packagePath := range packages {
			if _, under := messagingBoundaryRootOf(packagePath); under {
				found = append(found, packagePath)
			}
		}
		if !slices.Equal(found, c.want) {
			t.Errorf("%s: found %v under a messaging root, want %v", c.leg.name, found, c.want)
		}
		if len(findings) != len(c.want) {
			t.Errorf("%s: %d findings for %d planted packages: %v", c.leg.name, len(findings), len(c.want), findings)
		}
	}
}
