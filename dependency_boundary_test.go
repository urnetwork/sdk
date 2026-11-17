// Neither this repository nor any library it ships depends on messaging, which lives in
// github.com/urnetwork/message. Two gates hold the boundary, and each sees what the other cannot:
//
//   - every module file names no messaging module, and every .go file, whatever its build
//     constraints, imports no messaging package. This reads files no build compiles.
//   - for every build this repository ships, the package closure `go list -deps` resolves holds no
//     messaging package. This reads what a file pulls in transitively. A module no leg builds is
//     disposed of by a property its own files hold: it holds no package, or its module file
//     requires nothing.
//
// Until connect's own removal lands, connect/protocol still carries the messaging schema, so a
// core binary still links it; that package is generic transport and is not on the list.
package sdk

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
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
// the c library on each desktop platform, the gomobile binding on every target build/Makefile binds,
// and the build tools module. A module no leg builds is disposed of below, by a property `go list` or
// its own module file holds.
//
// The gomobile rows are the Makefile's binds in its order: the Android bind, the Apple bind and the
// network extension's (QD-908). Each carries the bind's own tags and the tag gomobile adds for its
// platform (cmd/gomobile/env.go's platformTags at the golang.org/x/mobile build/go.mod pins): ios on
// the iPhone and simulator slices. Android's is android, which GOOS=android sets itself, so the
// Android rows carry the bind's tags alone.
//
// gomobile runs each macOS slice in two configurations, and the bind fails if either does (QD-997):
// its binding step type-checks the package with GOOS=ios and the bind's tags plus macos, and its
// archive compiles with GOOS=darwin and the bind's tags alone, a command-line -tags replacing the
// -tags=macos gomobile sets in GOFLAGS. A row's name says which it models. The macOS rows here are the
// compile's; the binding step's take rows of their own beside them, and until then sdk's
// TestSdkCompilesForEveryBoundTarget compiles that configuration, cgo off.
//
// TestTheMessagingBoundaryLegsListEveryBoundTarget holds the rows to the binds both ways.
var messagingBoundaryLegs = []messagingBoundaryLeg{
	{name: "root linux/amd64", moduleDirectory: ".", environment: []string{"GOOS=linux", "GOARCH=amd64"}, test: true, patterns: []string{"./..."}, mustContain: "github.com/urnetwork/sdk"},
	{name: "root windows/amd64", moduleDirectory: ".", environment: []string{"GOOS=windows", "GOARCH=amd64"}, test: true, patterns: []string{"./..."}, mustContain: "github.com/urnetwork/sdk"},
	{name: "root darwin/arm64", moduleDirectory: ".", environment: []string{"GOOS=darwin", "GOARCH=arm64"}, test: true, patterns: []string{"./..."}, mustContain: "github.com/urnetwork/sdk"},
	{name: "js/wasm", moduleDirectory: "js", environment: []string{"GOOS=js", "GOARCH=wasm", "CGO_ENABLED=0"}, test: true, patterns: []string{"./..."}, mustContain: "github.com/urnetwork/sdk"},
	{name: sidecarWasmLegName, moduleDirectory: "js", environment: []string{"GOOS=js", "GOARCH=wasm", "CGO_ENABLED=0"}, tags: "sidecar_http", patterns: []string{"./cmd/sidecarwasm"}, mustContain: sidecarWasmMainPath},
	{name: "cgo linux/amd64", moduleDirectory: "cgo", environment: []string{"GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=1"}, test: true, patterns: []string{"./..."}, mustContain: "github.com/urnetwork/sdk"},
	{name: "cgo windows/amd64", moduleDirectory: "cgo", environment: []string{"GOOS=windows", "GOARCH=amd64", "CGO_ENABLED=1"}, test: true, patterns: []string{"./..."}, mustContain: "github.com/urnetwork/sdk"},
	{name: "cgo darwin/arm64", moduleDirectory: "cgo", environment: []string{"GOOS=darwin", "GOARCH=arm64", "CGO_ENABLED=1"}, test: true, patterns: []string{"./..."}, mustContain: "github.com/urnetwork/sdk"},
	{name: "gomobile android/arm64", moduleDirectory: ".", environment: []string{"GOOS=android", "GOARCH=arm64", "CGO_ENABLED=1"}, tags: "sdk_mobile_bind", patterns: []string{"."}, mustContain: "github.com/urnetwork/sdk"},
	{name: "gomobile android/arm", moduleDirectory: ".", environment: []string{"GOOS=android", "GOARCH=arm", "CGO_ENABLED=1"}, tags: "sdk_mobile_bind", patterns: []string{"."}, mustContain: "github.com/urnetwork/sdk"},
	{name: "gomobile android/amd64", moduleDirectory: ".", environment: []string{"GOOS=android", "GOARCH=amd64", "CGO_ENABLED=1"}, tags: "sdk_mobile_bind", patterns: []string{"."}, mustContain: "github.com/urnetwork/sdk"},
	{name: "gomobile ios/arm64", moduleDirectory: ".", environment: []string{"GOOS=ios", "GOARCH=arm64", "CGO_ENABLED=1"}, tags: "sdk_mobile_bind,ios", patterns: []string{"."}, mustContain: "github.com/urnetwork/sdk"},
	{name: "gomobile iossimulator/arm64", moduleDirectory: ".", environment: []string{"GOOS=ios", "GOARCH=arm64", "CGO_ENABLED=1"}, tags: "sdk_mobile_bind,ios", patterns: []string{"."}, mustContain: "github.com/urnetwork/sdk"},
	{name: "gomobile macos/arm64 compile", moduleDirectory: ".", environment: []string{"GOOS=darwin", "GOARCH=arm64", "CGO_ENABLED=1"}, tags: "sdk_mobile_bind", patterns: []string{"."}, mustContain: "github.com/urnetwork/sdk"},
	{name: "gomobile macos/amd64 compile", moduleDirectory: ".", environment: []string{"GOOS=darwin", "GOARCH=amd64", "CGO_ENABLED=1"}, tags: "sdk_mobile_bind", patterns: []string{"."}, mustContain: "github.com/urnetwork/sdk"},
	{name: "gomobile ios/arm64 extension", moduleDirectory: ".", environment: []string{"GOOS=ios", "GOARCH=arm64", "CGO_ENABLED=1"}, tags: "sdk_mobile_bind,ios_extension,ios", patterns: []string{"."}, mustContain: "github.com/urnetwork/sdk"},
	{name: "gomobile iossimulator/arm64 extension", moduleDirectory: ".", environment: []string{"GOOS=ios", "GOARCH=arm64", "CGO_ENABLED=1"}, tags: "sdk_mobile_bind,ios_extension,ios", patterns: []string{"."}, mustContain: "github.com/urnetwork/sdk"},
	{name: "gomobile macos/arm64 extension compile", moduleDirectory: ".", environment: []string{"GOOS=darwin", "GOARCH=arm64", "CGO_ENABLED=1"}, tags: "sdk_mobile_bind,ios_extension", patterns: []string{"."}, mustContain: "github.com/urnetwork/sdk"},
	{name: "gomobile macos/amd64 extension compile", moduleDirectory: ".", environment: []string{"GOOS=darwin", "GOARCH=amd64", "CGO_ENABLED=1"}, tags: "sdk_mobile_bind,ios_extension", patterns: []string{"."}, mustContain: "github.com/urnetwork/sdk"},
	{name: "build tools linux/amd64", moduleDirectory: "build", environment: []string{"GOOS=linux", "GOARCH=amd64"}, test: true, patterns: []string{"./..."}, mustContain: "github.com/urnetwork/sdk"},
}

// The file holding the gomobile binds the gomobile rows answer to, from the repository root.
const messagingBoundaryMakefilePath = "build/Makefile"

// One row a slice of a platform gomobile binds gets: what its name adds for the configuration it models,
// the GOOS, and the tags beside the bind's own.
type messagingBoundaryBindPlatform struct {
	nameSuffix string
	goos       string
	tags       []string
}

// Each platform the binds name, with the rows a slice of it gets, as the table builds them (see
// messagingBoundaryLegs).
var messagingBoundaryBindPlatforms = map[string][]messagingBoundaryBindPlatform{
	"android":      {{goos: "android"}},
	"ios":          {{goos: "ios", tags: []string{"ios"}}},
	"iossimulator": {{goos: "ios", tags: []string{"ios"}}},
	"macos":        {{nameSuffix: " compile", goos: "darwin"}},
}

// The rows the Makefile's gomobile binds call for, each named for its target as the table names it,
// "gomobile <platform>/<arch>" with " extension" for the bind tagged ios_extension and then what its
// platform's row adds, and built as its bind builds it; or what in the Makefile cannot be read. A bind
// runs from `gomobile bind` to the package it binds, a recipe continuing every line between.
func messagingBoundaryBoundRows(makefile string) ([]messagingBoundaryLeg, error) {
	const boundPackage = `"github.com/urnetwork/sdk"`
	targetPattern := regexp.MustCompile(`-target\s+(\S+)`)
	tagsPattern := regexp.MustCompile(`-tags\s+(\S+)`)
	binds := strings.Split(makefile, "gomobile bind")[1:]
	if len(binds) == 0 {
		return nil, errors.New("no gomobile bind")
	}
	rows := []messagingBoundaryLeg{}
	for _, bind := range binds {
		flags, _, found := strings.Cut(bind, boundPackage)
		if !found {
			return nil, fmt.Errorf("a gomobile bind of no %s", boundPackage)
		}
		target := targetPattern.FindStringSubmatch(flags)
		if target == nil {
			return nil, fmt.Errorf("a gomobile bind with no -target: %q", flags)
		}
		bindTags := []string{}
		if tags := tagsPattern.FindStringSubmatch(flags); tags != nil {
			bindTags = strings.Split(tags[1], ",")
		}
		bindNameSuffix := ""
		if slices.Contains(bindTags, "ios_extension") {
			bindNameSuffix = " extension"
		}
		for _, platformArch := range strings.Split(target[1], ",") {
			platformName, arch, found := strings.Cut(platformArch, "/")
			platformRows, known := messagingBoundaryBindPlatforms[platformName]
			if !found || arch == "" || !known {
				return nil, fmt.Errorf("a bound target the gomobile rows cannot build: %q", platformArch)
			}
			for _, platform := range platformRows {
				rows = append(rows, messagingBoundaryLeg{
					name:            "gomobile " + platformArch + bindNameSuffix + platform.nameSuffix,
					moduleDirectory: ".",
					environment:     []string{"GOOS=" + platform.goos, "GOARCH=" + arch, "CGO_ENABLED=1"},
					tags:            strings.Join(slices.Concat(bindTags, platform.tags), ","),
					patterns:        []string{"."},
				})
			}
		}
	}
	return rows, nil
}

// Where the table and the binds differ: a bound target with no row of its name, a row building other
// than its bind does, and a gomobile row no bind names. Environments and tags compare as sets.
func messagingBoundaryBoundRowFindings(boundRows []messagingBoundaryLeg, legs []messagingBoundaryLeg) []string {
	asSet := func(values []string) []string {
		set := slices.Clone(values)
		slices.Sort(set)
		return slices.Compact(set)
	}
	tagSet := func(tags string) []string {
		if tags == "" {
			return []string{}
		}
		return asSet(strings.Split(tags, ","))
	}
	legsByName := map[string]messagingBoundaryLeg{}
	for _, leg := range legs {
		legsByName[leg.name] = leg
	}
	boundNames := map[string]bool{}
	findings := []string{}
	for _, bound := range boundRows {
		boundNames[bound.name] = true
		leg, found := legsByName[bound.name]
		if !found {
			findings = append(findings, fmt.Sprintf("%s binds a target no row is named for: %q", messagingBoundaryMakefilePath, bound.name))
			continue
		}
		if leg.moduleDirectory != bound.moduleDirectory || !slices.Equal(leg.patterns, bound.patterns) ||
			!slices.Equal(asSet(leg.environment), asSet(bound.environment)) || !slices.Equal(tagSet(leg.tags), tagSet(bound.tags)) {
			findings = append(findings, fmt.Sprintf("%s builds %v in %s with %v and tags %q, and its bind %v in %s with %v and tags %q",
				leg.name, leg.patterns, leg.moduleDirectory, asSet(leg.environment), strings.Join(tagSet(leg.tags), ","),
				bound.patterns, bound.moduleDirectory, asSet(bound.environment), strings.Join(tagSet(bound.tags), ",")))
		}
	}
	for _, leg := range legs {
		if strings.HasPrefix(leg.name, "gomobile ") && !boundNames[leg.name] {
			findings = append(findings, fmt.Sprintf("%s is a gomobile row for a target %s binds nowhere", leg.name, messagingBoundaryMakefilePath))
		}
	}
	sort.Strings(findings)
	return findings
}

// Modules that hold no package, so no build of theirs links anything, with why. Held by `go list`
// matching no package there: a .go file added to one fails here until the module gets a leg.
var messagingBoundaryModulesWithoutAPackage = map[string]string{
	"cgo/build": "an empty module that keeps the cgo library's build output out of the cgo module",
}

// Modules whose module file requires nothing, with why each has no leg. Such a module's closure is
// the standard library and its own packages, and the import scan reads every one of its files.
// Held by its module file having no require, replace or tool entry: one added fails here until
// the module gets its leg back.
var messagingBoundaryModulesRequiringNothing = map[string]string{
	"packaging": "the binding generator and the packaging tools. Its go.mod asks for go 1.26.7, " +
		"newer than the root module's go 1.26.5, so `go list` there needs that toolchain, and a leg " +
		"would make the root module's own tests need it too",
}

// Every go.mod-shaped file under root, the dependency entries each declares, and what each names
// that is under a messaging root. go.mod-shaped is a file named go.mod or ending in .go.mod, the
// shape of an alternate modfile. The file is read by `go mod edit -json`, the go command's own
// parser. A dependency entry is a require, replace or tool line, the entries that can put a
// package other than the standard library's and the module's own into its builds.
func messagingBoundaryModuleFileFindings(t *testing.T, walkRoot string) (moduleFiles []string, dependencies map[string][]string, findings []string) {
	dependencies = map[string][]string{}
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
		entries := []string{}
		for _, require := range parsed.Require {
			named["require "+require.Path] = require.Path
			entries = append(entries, "require "+require.Path)
		}
		for _, exclude := range parsed.Exclude {
			named["exclude "+exclude.Path] = exclude.Path
		}
		for _, replace := range parsed.Replace {
			named["replace "+replace.Old.Path] = replace.Old.Path
			named["replace => "+replace.New.Path] = replace.New.Path
			entries = append(entries, "replace "+replace.Old.Path+" => "+replace.New.Path)
		}
		for _, tool := range parsed.Tool {
			named["tool "+tool.Path] = tool.Path
			entries = append(entries, "tool "+tool.Path)
		}
		sort.Strings(entries)
		dependencies[rel] = entries
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
	return moduleFiles, dependencies, findings
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
// import in a test file; and nothing for paths that only share a prefix with a root. The module
// file scan lists each file's dependency entries, messaging or not, and none for a module file
// that requires nothing, which is the reading a disposition of a module as requiring nothing
// rests on.
func TestTheMessagingBoundaryScansFindWhatAFixturePlants(t *testing.T) {
	fixture := t.TempDir()
	fixtureFiles := map[string]string{
		"go.mod":       "module fixture.example/core\n\ngo 1.26\n\nrequire github.com/urnetwork/message v0.0.0\n\nrequire example.com/plain v1.0.0\n\nreplace github.com/urnetwork/message-server => ../server\n",
		"alt.go.mod":   "module fixture.example/alt\n\ngo 1.26\n\ntool github.com/urnetwork/message/cmd/fixture\n",
		"bare/go.mod":  "module fixture.example/bare\n\ngo 1.26\n\nexclude example.com/plain v0.9.0\n",
		"never.go":     "//go:build urnet_fixture_never\n\npackage core\n\nimport group \"github.com/urnetwork/connect/messagegroup\"\n\nvar _ = group.Fixture\n",
		"core.go":      "package core\n\nimport (\n\t_ \"github.com/urnetwork/messagex\"\n\t_ \"github.com/urnetwork/connect/messages\"\n\t_ \"github.com/urnetwork/message-serverx\"\n)\n",
		"core_test.go": "package core\n\nimport . \"github.com/urnetwork/sdk/urmessage\"\n",
	}
	for name, content := range fixtureFiles {
		fixturePath := filepath.Join(fixture, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(fixturePath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fixturePath, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	moduleFiles, dependencies, moduleFindings := messagingBoundaryModuleFileFindings(t, fixture)
	if !slices.Equal(moduleFiles, []string{"alt.go.mod", "bare/go.mod", "go.mod"}) {
		t.Errorf("module files found: %v, want alt.go.mod, bare/go.mod and go.mod", moduleFiles)
	}
	wantDependencies := map[string][]string{
		"alt.go.mod":  {"tool github.com/urnetwork/message/cmd/fixture"},
		"bare/go.mod": {},
		"go.mod": {
			"replace github.com/urnetwork/message-server => ../server",
			"require example.com/plain",
			"require github.com/urnetwork/message",
		},
	}
	if !maps.EqualFunc(dependencies, wantDependencies, slices.Equal[[]string]) {
		t.Errorf("dependency entries: %v, want %v", dependencies, wantDependencies)
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
	moduleFiles, _, moduleFindings := messagingBoundaryModuleFileFindings(t, ".")
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
// module or disposed of, once, both ways, so a module added later is either checked or written
// down. Each disposition is held by what it says: a module said to hold no package matches none
// under `go list`, and a module said to require nothing has no require, replace or tool entry.
func TestNoShippedBuildLinksAMessagingPackage(t *testing.T) {
	moduleFiles, dependencies, _ := messagingBoundaryModuleFileFindings(t, ".")
	moduleDirectories := map[string]bool{}
	for _, moduleFile := range moduleFiles {
		if filepath.Base(moduleFile) != "go.mod" {
			t.Errorf("%s is an alternate module file, and no leg here builds with it; give it a leg", moduleFile)
			continue
		}
		directory := filepath.ToSlash(filepath.Dir(filepath.FromSlash(moduleFile)))
		moduleDirectories[directory] = true
	}
	// module directory -> what covers it, which must be exactly one thing
	coveredBy := map[string][]string{}
	for _, leg := range messagingBoundaryLegs {
		if !slices.Contains(coveredBy[leg.moduleDirectory], "legs") {
			coveredBy[leg.moduleDirectory] = append(coveredBy[leg.moduleDirectory], "legs")
		}
	}
	for directory := range messagingBoundaryModulesWithoutAPackage {
		coveredBy[directory] = append(coveredBy[directory], "holds no package")
	}
	for directory := range messagingBoundaryModulesRequiringNothing {
		coveredBy[directory] = append(coveredBy[directory], "requires nothing")
	}
	for directory, by := range coveredBy {
		if len(by) != 1 {
			t.Errorf("%s is covered %d ways (%s); a module is a leg's or disposed of, once", directory, len(by), strings.Join(by, ", "))
		}
		if !moduleDirectories[directory] {
			t.Errorf("a leg or disposition names %s, which holds no go.mod", directory)
		}
	}
	for directory := range moduleDirectories {
		if len(coveredBy[directory]) == 0 {
			t.Errorf("the module in %s has no leg and no disposition", directory)
		}
	}

	// each disposition, held by what it says
	for directory, why := range messagingBoundaryModulesWithoutAPackage {
		packages, _ := messagingBoundaryLegPackages(t, ".", messagingBoundaryLeg{
			name:            directory + ", disposed of as holding no package",
			moduleDirectory: directory,
			patterns:        []string{"./..."},
		})
		if len(packages) != 0 {
			t.Errorf("%s is disposed of as holding no package (%s) and go list resolves %v there; give it a leg", directory, why, packages)
		}
		t.Logf("%s holds no package (%s): go list -deps ./... resolves %d packages", directory, why, len(packages))
	}
	for directory, why := range messagingBoundaryModulesRequiringNothing {
		entries := dependencies[path.Join(directory, "go.mod")]
		if len(entries) != 0 {
			t.Errorf("%s is disposed of as requiring nothing (%s) and its module file declares %v, so its closure is no "+
				"longer the standard library and its own packages; give it a leg", directory, why, entries)
		}
		t.Logf("%s requires nothing (%s): %d require, replace or tool entries", directory, why, len(entries))
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
	// A native caller must not add cgo-only dependencies to the shipped browser leg.
	t.Setenv("CGO_ENABLED", "1")
	browserIndex := slices.IndexFunc(messagingBoundaryLegs, func(leg messagingBoundaryLeg) bool {
		return leg.name == "js/wasm"
	})
	if browserIndex < 0 {
		t.Fatal("the shipped browser dependency leg is missing")
	}
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
			leg:  messagingBoundaryLegs[browserIndex],
			want: []string{"github.com/urnetwork/message/browser", "github.com/urnetwork/message/testonly"},
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

// Every target build/Makefile's gomobile binds name has its row, built as its bind builds it, and every
// gomobile row is a bound target's (QD-908), so a target added to a bind is read by every rule here,
// and by sidecar's make check through its mirror of this table, from then on.
func TestTheMessagingBoundaryLegsListEveryBoundTarget(t *testing.T) {
	makefile, err := os.ReadFile(filepath.FromSlash(messagingBoundaryMakefilePath))
	if err != nil {
		t.Fatal(err)
	}
	boundRows, err := messagingBoundaryBoundRows(string(makefile))
	if err != nil {
		t.Fatalf("%s: %v", messagingBoundaryMakefilePath, err)
	}
	// the three binds, each read: a reader that found fewer would pass a table that lists fewer
	for _, name := range []string{"gomobile android/arm64", "gomobile ios/arm64", "gomobile ios/arm64 extension"} {
		if !slices.ContainsFunc(boundRows, func(bound messagingBoundaryLeg) bool { return bound.name == name }) {
			t.Fatalf("CONTROL FAILED: %s's binds read as %d targets, without %s", messagingBoundaryMakefilePath, len(boundRows), name)
		}
	}
	for _, finding := range messagingBoundaryBoundRowFindings(boundRows, messagingBoundaryLegs) {
		t.Errorf("%s", finding)
	}
	t.Logf("%d bound targets, each a row", len(boundRows))
}

// The rule finds what a fixture plants: a bound target with no row, a row lacking its platform's tag, a
// macOS compile row carrying the macos its archive compiles without, and gomobile rows no bind names, one
// of them a macOS row whose name says no step; a table built as the binds are passes, and a bind the rows
// cannot build is refused rather than skipped.
func TestTheBoundTargetRuleFindsWhatAFixturePlants(t *testing.T) {
	makefile := "build_mobile:\n" +
		"\t\"$(RETRY)\" gomobile bind \\\n\t\t-target android/arm,ios/arm64,macos/amd64 -iosversion 16.0 \\\n" +
		"\t\t-tags sdk_mobile_bind \\\n\t\t-o \"$$BUILD_DIR/Sdk.xcframework\" \\\n\t\t\"github.com/urnetwork/sdk\"; \\\n" +
		"\t\"$(RETRY)\" gomobile bind \\\n\t\t-target iossimulator/arm64 \\\n" +
		"\t\t-tags sdk_mobile_bind,ios_extension \\\n\t\t\"github.com/urnetwork/sdk\"\n"
	boundRows, err := messagingBoundaryBoundRows(makefile)
	if err != nil {
		t.Fatal(err)
	}
	row := func(name string, goos string, goarch string, tags string) messagingBoundaryLeg {
		return messagingBoundaryLeg{
			name: name, moduleDirectory: ".", environment: []string{"GOOS=" + goos, "GOARCH=" + goarch, "CGO_ENABLED=1"},
			tags: tags, patterns: []string{"."},
		}
	}
	matching := []messagingBoundaryLeg{
		row("gomobile android/arm", "android", "arm", "sdk_mobile_bind"),
		row("gomobile ios/arm64", "ios", "arm64", "ios,sdk_mobile_bind"),
		row("gomobile macos/amd64 compile", "darwin", "amd64", "sdk_mobile_bind"),
		row("gomobile iossimulator/arm64 extension", "ios", "arm64", "sdk_mobile_bind,ios_extension,ios"),
		{name: "root linux/amd64", moduleDirectory: ".", environment: []string{"GOOS=linux", "GOARCH=amd64"}, patterns: []string{"./..."}},
	}
	if findings := messagingBoundaryBoundRowFindings(boundRows, matching); len(findings) != 0 {
		t.Errorf("a table built as the binds are: %v", findings)
	}

	planted := []messagingBoundaryLeg{
		row("gomobile ios/arm64", "ios", "arm64", "sdk_mobile_bind"),
		row("gomobile macos/amd64 compile", "darwin", "amd64", "sdk_mobile_bind,macos"),
		row("gomobile macos/amd64", "darwin", "amd64", "sdk_mobile_bind"),
		row("gomobile iossimulator/arm64 extension", "ios", "arm64", "sdk_mobile_bind,ios_extension,ios"),
		row("gomobile android/386", "android", "386", "sdk_mobile_bind"),
	}
	want := []string{
		"build/Makefile binds a target no row is named for: \"gomobile android/arm\"",
		"gomobile android/386 is a gomobile row for a target build/Makefile binds nowhere",
		"gomobile ios/arm64 builds [.] in . with [CGO_ENABLED=1 GOARCH=arm64 GOOS=ios] and tags \"sdk_mobile_bind\", and its bind [.] in . with [CGO_ENABLED=1 GOARCH=arm64 GOOS=ios] and tags \"ios,sdk_mobile_bind\"",
		"gomobile macos/amd64 compile builds [.] in . with [CGO_ENABLED=1 GOARCH=amd64 GOOS=darwin] and tags \"macos,sdk_mobile_bind\", and its bind [.] in . with [CGO_ENABLED=1 GOARCH=amd64 GOOS=darwin] and tags \"sdk_mobile_bind\"",
		"gomobile macos/amd64 is a gomobile row for a target build/Makefile binds nowhere",
	}
	if findings := messagingBoundaryBoundRowFindings(boundRows, planted); !slices.Equal(findings, want) {
		t.Errorf("findings:\n%s\nwant:\n%s", strings.Join(findings, "\n"), strings.Join(want, "\n"))
	}

	for _, unreadable := range []string{
		"\tgomobile bind -target maccatalyst/arm64 -tags sdk_mobile_bind \"github.com/urnetwork/sdk\"\n",
		"\tgomobile bind -target ios -tags sdk_mobile_bind \"github.com/urnetwork/sdk\"\n",
		"\tgomobile bind -tags sdk_mobile_bind \"github.com/urnetwork/sdk\"\n",
		"\tgomobile bind -target ios/arm64 -tags sdk_mobile_bind \"github.com/urnetwork/other\"\n",
		"build:\n\tgo build ./...\n",
	} {
		if rows, err := messagingBoundaryBoundRows(unreadable); err == nil {
			t.Errorf("%q read as %d rows, not refused", unreadable, len(rows))
		}
	}
}

// The page Worker's main, sidecar.wasm (sidecar §18.10, D-SDK-19), and its leg's name.
const (
	sidecarWasmLegName  = "sidecar.wasm js/wasm sidecar_http"
	sidecarWasmMainPath = "github.com/urnetwork/sdk/js/cmd/sidecarwasm"
)

// The page Worker's logic (SDK-M4-04), which the main links: a package of this module that names no sdk or connect
// package itself, which the rest of the closure rule holds, so it is the one other path under
// github.com/urnetwork/sdk the leg may hold.
const sidecarWasmWorkerPath = "github.com/urnetwork/sdk/js/sidecarworker"

// What sidecar.wasm's closure must not hold, each with why. A path is under a root as the messaging
// matcher reads it, whole path elements. The main links the sidecar root, protocol and the Worker's
// logic only: connect
// links gVisor's IP stack into every importer, so a connect-only js/wasm main is already 55.0 MB
// unstripped, and the leg's goal is 41,943,040 B (§18.10); the sdk and sdk.wasm's main link connect;
// and net/rpc and the template packages keep a binary's exported methods alive, which loses the
// linker's method pruning for the whole binary (sdk/build/mobile_method_pruning_test.go).
var sidecarWasmRefusedRoots = map[string]string{
	"github.com/urnetwork/sdk": "this repository's sdk, which links connect",
	"net/rpc":                  "net/rpc, which loses method pruning",
	"text/template":            "text/template, which loses method pruning",
	"html/template":            "html/template, which loses method pruning",
}

// Roots the leg links only through operator-core's Id, which sidecar's Id aliases on every leg (owner
// decision 251): a package outside these trees and operator-core that imports one of them is a finding.
var sidecarWasmConnectRoots = map[string]string{
	"github.com/urnetwork/connect": "connect, which links gVisor",
	"gvisor.dev/gvisor":            "gVisor's IP stack",
	"github.com/pion/sctp":         "the sctp fork in connect's tree",
}

// The one module whose packages may import a connect root on the leg.
const sidecarWasmConnectCarrier = "github.com/urnetwork/operator-core"

// The connect root a package path falls under, by whole path elements.
func sidecarWasmConnectRootOf(packagePath string) (string, bool) {
	for root := range sidecarWasmConnectRoots {
		if packagePath == root || strings.HasPrefix(packagePath, root+"/") {
			return root, true
		}
	}
	return "", false
}

// Each package of the leg's closure with the packages it imports, from go list.
func sidecarWasmLegImports(t *testing.T, repositoryRoot string, leg messagingBoundaryLeg) map[string][]string {
	t.Helper()
	arguments := []string{"list", "-deps", "-f", "{{.ImportPath}} {{join .Imports \" \"}}"}
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
	imports := map[string][]string{}
	for _, line := range strings.Split(strings.ReplaceAll(string(output), "\r\n", "\n"), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		imports[fields[0]] = fields[1:]
	}
	return imports
}

// Each import of a connect root by a first-party package that is neither under a connect root nor
// operator-core's, as findings: connect reaches the leg through operator-core's Id and no other way.
// Connect's own third-party dependencies, which import its forks among themselves, are its closure's.
func sidecarWasmConnectFindings(legName string, imports map[string][]string) []string {
	findings := []string{}
	for importer, imported := range imports {
		if !strings.HasPrefix(importer, "github.com/urnetwork/") && !strings.HasPrefix(importer, "github.com/urfoundation/") {
			continue
		}
		if _, under := sidecarWasmConnectRootOf(importer); under {
			continue
		}
		if importer == sidecarWasmConnectCarrier || strings.HasPrefix(importer, sidecarWasmConnectCarrier+"/") {
			continue
		}
		for _, importPath := range imported {
			if root, under := sidecarWasmConnectRootOf(importPath); under {
				findings = append(findings, fmt.Sprintf("%s: %s imports %s, under %s (%s), which only operator-core may bring",
					legName, importer, importPath, root, sidecarWasmConnectRoots[root]))
			}
		}
	}
	sort.Strings(findings)
	return findings
}

// Each package in a closure under a refused root, as findings. The main's own package and the Worker's
// logic are the paths under github.com/urnetwork/sdk it may hold.
func sidecarWasmClosureFindings(legName string, mainPath string, packages []string) []string {
	findings := []string{}
	for _, packagePath := range packages {
		if packagePath == mainPath || packagePath == sidecarWasmWorkerPath {
			continue
		}
		for root, why := range sidecarWasmRefusedRoots {
			if packagePath == root || strings.HasPrefix(packagePath, root+"/") {
				findings = append(findings, fmt.Sprintf("%s links %s, under %s (%s)", legName, packagePath, root, why))
			}
		}
	}
	sort.Strings(findings)
	return findings
}

// sidecar.wasm links no sdk, net/rpc or template package, and links connect, gVisor and the sctp fork
// only through operator-core's Id (owner decision 251), so a connect import in the main, or in anything
// it reaches but operator-core, fails here as well as in sidecar's make check.
func TestTheSidecarWasmLegLinksNoSdkAndConnectOnlyThroughOperatorCore(t *testing.T) {
	legIndex := slices.IndexFunc(messagingBoundaryLegs, func(leg messagingBoundaryLeg) bool {
		return leg.name == sidecarWasmLegName
	})
	if legIndex < 0 {
		t.Fatal("the sidecar.wasm leg is missing")
	}
	leg := messagingBoundaryLegs[legIndex]
	packages, _ := messagingBoundaryLegPackages(t, ".", leg)
	if !slices.Contains(packages, sidecarWasmMainPath) || !slices.Contains(packages, "github.com/urnetwork/sidecar") {
		t.Fatalf("CONTROL FAILED: %s resolved %d packages, without the main or the sidecar root", leg.name, len(packages))
	}
	for _, finding := range sidecarWasmClosureFindings(leg.name, sidecarWasmMainPath, packages) {
		t.Errorf("%s", finding)
	}
	for _, finding := range sidecarWasmConnectFindings(leg.name, sidecarWasmLegImports(t, ".", leg)) {
		t.Errorf("%s", finding)
	}
	t.Logf("%s: %d packages", leg.name, len(packages))
}

// The rule finds what a fixture plants behind the leg's tag: a connect import, a sdk import and
// net/rpc with the template packages it brings, and nothing for the same main built without the tag,
// for a package sharing only a prefix with a root, or for connect reached through operator-core.
func TestTheSidecarWasmRuleFindsWhatAFixturePlants(t *testing.T) {
	fixture := t.TempDir()
	fixtureFiles := map[string]string{
		"core/go.mod": "module github.com/urnetwork/sdk/js\n\ngo 1.26\n\n" +
			"require (\n\tgithub.com/urnetwork/connect v0.0.0\n\tgithub.com/urnetwork/connectx v0.0.0\n\tgithub.com/urnetwork/operator-core v0.0.0\n\tgithub.com/urnetwork/sdk v0.0.0\n)\n\n" +
			"replace github.com/urnetwork/connect => ../connect\n\nreplace github.com/urnetwork/connectx => ../connectx\n\n" +
			"replace github.com/urnetwork/operator-core => ../operator-core\n\nreplace github.com/urnetwork/sdk => ../sdk\n",
		"core/cmd/sidecarwasm/main.go":    "package main\n\nimport (\n\t_ \"github.com/urnetwork/connectx\"\n\t_ \"github.com/urnetwork/operator-core\"\n)\n\nfunc main() {}\n",
		"operator-core/go.mod":            "module github.com/urnetwork/operator-core\n\ngo 1.26\n\nrequire github.com/urnetwork/connect v0.0.0\n\nreplace github.com/urnetwork/connect => ../connect\n",
		"operator-core/operator.go":       "package operator\n\nimport _ \"github.com/urnetwork/connect\"\n",
		"connect/connect.go":              "package connect\n",
		"core/cmd/sidecarwasm/planted.go": "//go:build sidecar_http\n\npackage main\n\nimport (\n\t_ \"github.com/urnetwork/connect/protocol\"\n\t_ \"github.com/urnetwork/sdk\"\n\t_ \"net/rpc\"\n)\n",
		"connect/go.mod":                  "module github.com/urnetwork/connect\n\ngo 1.26\n",
		"connect/protocol/p.go":           "package protocol\n",
		"connectx/go.mod":                 "module github.com/urnetwork/connectx\n\ngo 1.26\n",
		"connectx/x.go":                   "package connectx\n",
		"sdk/go.mod":                      "module github.com/urnetwork/sdk\n\ngo 1.26\n",
		"sdk/sdk.go":                      "package sdk\n",
	}
	for name, content := range fixtureFiles {
		fixturePath := filepath.Join(fixture, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(fixturePath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fixturePath, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	leg := messagingBoundaryLeg{
		name:            "fixture",
		moduleDirectory: "core",
		environment:     []string{"GOOS=js", "GOARCH=wasm", "CGO_ENABLED=0"},
		tags:            "sidecar_http",
		patterns:        []string{"./cmd/sidecarwasm"},
	}
	packages, _ := messagingBoundaryLegPackages(t, fixture, leg)
	if !slices.Contains(packages, "github.com/urnetwork/connect") {
		t.Fatalf("CONTROL FAILED: the fixture reaches no connect through operator-core: %v", packages)
	}
	findings := sidecarWasmClosureFindings(leg.name, sidecarWasmMainPath, packages)
	findings = append(findings, sidecarWasmConnectFindings(leg.name, sidecarWasmLegImports(t, fixture, leg))...)
	// net/rpc's debug page brings both template packages with it; connect through operator-core is no finding
	want := []string{
		"fixture links github.com/urnetwork/sdk, under github.com/urnetwork/sdk (this repository's sdk, which links connect)",
		"fixture links html/template, under html/template (html/template, which loses method pruning)",
		"fixture links net/rpc, under net/rpc (net/rpc, which loses method pruning)",
		"fixture links text/template, under text/template (text/template, which loses method pruning)",
		"fixture links text/template/parse, under text/template (text/template, which loses method pruning)",
		"fixture: github.com/urnetwork/sdk/js/cmd/sidecarwasm imports github.com/urnetwork/connect/protocol, under github.com/urnetwork/connect (connect, which links gVisor), which only operator-core may bring",
	}
	if !slices.Equal(findings, want) {
		t.Errorf("findings:\n%s\nwant:\n%s", strings.Join(findings, "\n"), strings.Join(want, "\n"))
	}

	leg.tags = ""
	packages, _ = messagingBoundaryLegPackages(t, fixture, leg)
	if !slices.Contains(packages, "github.com/urnetwork/connectx") {
		t.Fatalf("CONTROL FAILED: the untagged fixture resolved %v", packages)
	}
	if findings := sidecarWasmClosureFindings(leg.name, sidecarWasmMainPath, packages); len(findings) != 0 {
		t.Errorf("without the tag nothing is planted, and the rule found %v", findings)
	}
	if findings := sidecarWasmConnectFindings(leg.name, sidecarWasmLegImports(t, fixture, leg)); len(findings) != 0 {
		t.Errorf("without the tag connect comes only through operator-core, and the rule found %v", findings)
	}
}

// The sidecar module (sidecar §18.10's Imports, D-SDK-20, R-SDK-4, DESIGN-DETAILS 4.2.3), file by file on every leg: in
// the root package a sidecar-role file, sidecar*.go, may import the sidecar root, protocol, protocol/bridge and device,
// and carries !ios_extension, since the tunnel runs forwarding alone; a forwarding file, device_sidecar*.go, imports
// device, protocol/bridge and the sidecar root, whose forwarding files it names, untagged, since the tunnel links it; no
// other file of a package this repository builds imports a sidecar package, the page Worker's main aside, which its own
// leg holds to the root and protocol (TestTheSidecarWasmLegLinksNoSdkAndConnectOnlyThroughOperatorCore). On the ios_extension leg a sidecar-role
// file is refused whatever it imports, and every sidecar root or protocol file the leg compiles is on the sidecar
// module's ios_extension leg list. A rule read from the files each leg compiles, so a file's build constraints are what
// the leg makes of them.
const sidecarImportModulePath = "github.com/urnetwork/sidecar"

// What a sidecar-role file and a forwarding file may import of the sidecar module.
var (
	sidecarImportRoleAllowed = []string{
		sidecarImportModulePath,
		sidecarImportModulePath + "/protocol",
		sidecarImportModulePath + "/protocol/bridge",
		sidecarImportModulePath + "/device",
	}
	sidecarImportDeviceAllowed = []string{
		sidecarImportModulePath,
		sidecarImportModulePath + "/device",
		sidecarImportModulePath + "/protocol/bridge",
	}
	// What a sidecar-role test file may import beside the role's: T4's cell harness, which imports only the standard
	// library, so it brings nothing of sidecartest's servers into this module (SDK-M8-15); and the forwarding compile's
	// fixture, which imports only the role's own packages and the rows fixture's protocol messages (BRIDGE-M8-03). No
	// other sidecartest package, and nothing a file that is no test may import.
	sidecarImportRoleTestAllowed = []string{
		sidecarImportModulePath + "/sidecartest/t4cell",
		sidecarImportModulePath + "/sidecartest/forwardcell",
	}
)

// The sidecar module's ios_extension leg list, by path from that module (DESIGN-DETAILS 4.2.3): the root and protocol
// files the iOS extension may link, which sidecar's make check holds on its own extension legs.
const sidecarIosExtensionLegListPath = "check/testdata/ios_extension_leg.txt"

// The list, read from the sidecar module the module in moduleDirectory builds against. A list this cannot read fails
// the test: a rule that read no list has not passed.
func sidecarIosExtensionLegList(t *testing.T, moduleDirectory string) map[string]bool {
	t.Helper()
	command := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", sidecarImportModulePath)
	command.Dir = moduleDirectory
	command.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off")
	output, err := command.Output()
	if err != nil {
		stderr := ""
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			stderr = string(exitError.Stderr)
		}
		t.Fatalf("finding the sidecar module from %s: %v\n%s", moduleDirectory, err, stderr)
	}
	listPath := filepath.Join(strings.TrimSpace(string(output)), filepath.FromSlash(sidecarIosExtensionLegListPath))
	data, err := os.ReadFile(listPath)
	if err != nil {
		t.Fatalf("the sidecar module's ios_extension leg list does not read, so nothing holds the extension to it: %v", err)
	}
	files := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			files[line] = true
		}
	}
	if len(files) == 0 {
		t.Fatalf("%s names no file", listPath)
	}
	return files
}

// Each sidecar root or protocol file a build compiles that the list does not name, and how many such files it
// compiled, read from go list -deps in directory with the build's environment and arguments, its patterns last.
func sidecarIosExtensionFileFindings(t *testing.T, name string, directory string, environment []string, arguments []string, list map[string]bool) ([]string, int) {
	t.Helper()
	command := exec.Command("go", slices.Concat([]string{"list", "-deps", "-json=ImportPath,GoFiles,CgoFiles"}, arguments)...)
	command.Dir = directory
	command.Env = environment
	output, err := command.Output()
	if err != nil {
		stderr := ""
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			stderr = string(exitError.Stderr)
		}
		t.Fatalf("%s: go list %s in %s: %v\n%s", name, strings.Join(arguments, " "), directory, err, stderr)
	}
	findings := []string{}
	compiled := 0
	decoder := json.NewDecoder(bytes.NewReader(output))
	for decoder.More() {
		var listed struct {
			ImportPath string
			GoFiles    []string
			CgoFiles   []string
		}
		if err := decoder.Decode(&listed); err != nil {
			t.Fatalf("%s: reading go list: %v", name, err)
		}
		prefix := ""
		switch listed.ImportPath {
		case sidecarImportModulePath:
		case sidecarImportModulePath + "/protocol":
			prefix = "protocol/"
		default:
			continue
		}
		for _, goFile := range slices.Concat(listed.GoFiles, listed.CgoFiles) {
			compiled += 1
			if !list[prefix+goFile] {
				findings = append(findings, fmt.Sprintf("%s compiles the sidecar module's %s%s, which its ios_extension leg list does not name", name, prefix, goFile))
			}
		}
	}
	sort.Strings(findings)
	return findings, compiled
}

// Whether a package path is this repository's, its external tests included.
func sidecarImportRepositoryPackage(packagePath string) bool {
	packagePath = strings.TrimSuffix(packagePath, "_test")
	return packagePath == "github.com/urnetwork/sdk" || strings.HasPrefix(packagePath, "github.com/urnetwork/sdk/")
}

// This repository's files a leg compiles, by path under repositoryRoot, each with its package's import path; test
// files only on a leg that builds tests.
func sidecarImportLegFiles(t *testing.T, repositoryRoot string, leg messagingBoundaryLeg) map[string]string {
	t.Helper()
	arguments := []string{"list", "-deps", "-json=ImportPath,Dir,GoFiles,CgoFiles,TestGoFiles,XTestGoFiles"}
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
	// go list names directories as the system resolves them, so both sides are resolved: a temp directory is often
	// under a link
	resolve := func(directory string) string {
		absolute, err := filepath.Abs(directory)
		if err != nil {
			t.Fatal(err)
		}
		resolved, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			t.Fatal(err)
		}
		return resolved
	}
	resolvedRoot := resolve(repositoryRoot)
	files := map[string]string{}
	decoder := json.NewDecoder(bytes.NewReader(output))
	for decoder.More() {
		var listed struct {
			ImportPath   string
			Dir          string
			GoFiles      []string
			CgoFiles     []string
			TestGoFiles  []string
			XTestGoFiles []string
		}
		if err := decoder.Decode(&listed); err != nil {
			t.Fatalf("%s: reading go list: %v", leg.name, err)
		}
		packagePath, _, _ := strings.Cut(listed.ImportPath, " ")
		if !sidecarImportRepositoryPackage(packagePath) || strings.HasSuffix(packagePath, ".test") {
			continue
		}
		names := slices.Concat(listed.GoFiles, listed.CgoFiles)
		if leg.test {
			names = slices.Concat(names, listed.TestGoFiles, listed.XTestGoFiles)
		}
		resolvedDir := resolve(listed.Dir)
		for _, name := range names {
			relative, err := filepath.Rel(resolvedRoot, filepath.Join(resolvedDir, name))
			if err != nil {
				t.Fatal(err)
			}
			files[filepath.ToSlash(relative)] = strings.TrimSuffix(packagePath, "_test")
		}
	}
	return files
}

// Each import of the sidecar module a leg's files make that the rule refuses, and how many it allowed of each kind of
// file, keyed role and device.
func sidecarImportFindings(t *testing.T, repositoryRoot string, leg messagingBoundaryLeg) (findings []string, allowedCounts map[string]int) {
	t.Helper()
	extension := strings.Contains(leg.tags, "ios_extension")
	allowedCounts = map[string]int{}
	fileSet := token.NewFileSet()
	files := sidecarImportLegFiles(t, repositoryRoot, leg)
	for relative, packagePath := range files {
		parsed, err := parser.ParseFile(fileSet, filepath.Join(repositoryRoot, filepath.FromSlash(relative)), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		name := path.Base(relative)
		kind := "other"
		allowed := []string{}
		if packagePath == "github.com/urnetwork/sdk" {
			switch {
			case strings.HasPrefix(name, "device_sidecar"):
				kind, allowed = "device", sidecarImportDeviceAllowed
			case strings.HasPrefix(name, "sidecar"):
				kind, allowed = "role", sidecarImportRoleAllowed
			}
		}
		for _, spec := range parsed.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if importPath != sidecarImportModulePath && !strings.HasPrefix(importPath, sidecarImportModulePath+"/") {
				continue
			}
			switch {
			case extension && kind == "role":
				findings = append(findings, fmt.Sprintf("%s: %s imports %s, and a sidecar-role file carries !ios_extension, since the iOS extension runs forwarding alone", leg.name, relative, importPath))
			case kind == "other":
				findings = append(findings, fmt.Sprintf("%s: %s imports %s, and only sidecar*.go and device_sidecar*.go of the root package import the sidecar module", leg.name, relative, importPath))
			case kind == "role" && strings.HasSuffix(name, "_test.go") && slices.Contains(sidecarImportRoleTestAllowed, importPath):
				allowedCounts["role test"] += 1
			case !slices.Contains(allowed, importPath):
				findings = append(findings, fmt.Sprintf("%s: %s imports %s, which a %s file may not", leg.name, relative, importPath, kind))
			default:
				allowedCounts[kind] += 1
			}
		}
	}
	if extension {
		moduleDirectory := filepath.Join(repositoryRoot, filepath.FromSlash(leg.moduleDirectory))
		arguments := []string{}
		if leg.tags != "" {
			arguments = append(arguments, "-tags", leg.tags)
		}
		environment := append(append(os.Environ(), "GOFLAGS=", "GOWORK=off"), leg.environment...)
		listFindings, _ := sidecarIosExtensionFileFindings(t, leg.name, moduleDirectory, environment, append(arguments, leg.patterns...),
			sidecarIosExtensionLegList(t, moduleDirectory))
		findings = append(findings, listFindings...)
	}
	sort.Strings(findings)
	return findings, allowedCounts
}

// Every leg but the page Worker's holds the rule, and the rule saw both kinds of file: a sidecar-role file importing
// the root somewhere, and the device files in the iOS extension.
func TestSdkImportsTheSidecarModuleByRole(t *testing.T) {
	roleSeen := false
	extensionDeviceSeen := false
	for _, leg := range messagingBoundaryLegs {
		if leg.name == sidecarWasmLegName {
			continue
		}
		findings, allowedCounts := sidecarImportFindings(t, ".", leg)
		for _, finding := range findings {
			t.Errorf("%s", finding)
		}
		roleSeen = roleSeen || 0 < allowedCounts["role"]
		if strings.Contains(leg.tags, "ios_extension") {
			extensionDeviceSeen = 0 < allowedCounts["device"] && allowedCounts["role"] == 0
		}
		t.Logf("%s: %d sidecar imports allowed (%v), %d refused", leg.name, allowedCounts["role"]+allowedCounts["device"], allowedCounts, len(findings))
	}
	if !roleSeen || !extensionDeviceSeen {
		t.Fatal("CONTROL FAILED: no leg compiled a sidecar-role import, or the extension no device file's, so the rule saw nothing")
	}
}

// The rule finds what a fixture plants and nothing else: on a host leg a forwarding file importing protocol, another
// file importing device, and a sidecar-role file importing mcp, while a forwarding file importing the root passes; on
// the extension leg also an untagged sidecar-role file, while the tagged one is left out, and a sidecar root file the
// list does not name, while a tagged one is left out; on a test leg also a test file importing sidecartest.
func TestTheSidecarImportRuleFindsWhatAFixturePlants(t *testing.T) {
	fixture := t.TempDir()
	fixtureFiles := map[string]string{
		"core/go.mod": "module github.com/urnetwork/sdk\n\ngo 1.26\n\nrequire github.com/urnetwork/sidecar v0.0.0\n\n" +
			"replace github.com/urnetwork/sidecar => ../sidecar\n",
		"core/sidecar_role.go":                         "//go:build !ios_extension\n\npackage sdk\n\nimport (\n\t_ \"github.com/urnetwork/sidecar\"\n\t_ \"github.com/urnetwork/sidecar/protocol\"\n)\n",
		"core/sidecar_untagged.go":                     "package sdk\n\nimport _ \"github.com/urnetwork/sidecar\"\n",
		"core/sidecar_mcp.go":                          "//go:build !ios_extension\n\npackage sdk\n\nimport _ \"github.com/urnetwork/sidecar/mcp\"\n",
		"core/device_sidecar.go":                       "package sdk\n\nimport (\n\t_ \"github.com/urnetwork/sidecar/device\"\n\t_ \"github.com/urnetwork/sidecar/protocol/bridge\"\n)\n",
		"core/device_sidecar_root.go":                  "package sdk\n\nimport _ \"github.com/urnetwork/sidecar\"\n",
		"core/device_sidecar_proto.go":                 "package sdk\n\nimport _ \"github.com/urnetwork/sidecar/protocol\"\n",
		"core/other.go":                                "package sdk\n\nimport _ \"github.com/urnetwork/sidecar/device\"\n",
		"core/core_test.go":                            "package sdk\n\nimport _ \"github.com/urnetwork/sidecar/sidecartest\"\n",
		"core/sidecar_cells_test.go":                   "//go:build !ios_extension\n\npackage sdk\n\nimport _ \"github.com/urnetwork/sidecar/sidecartest/t4cell\"\n",
		"core/sidecar_cells.go":                        "//go:build !ios_extension\n\npackage sdk\n\nimport _ \"github.com/urnetwork/sidecar/sidecartest/t4cell\"\n",
		"sidecar/go.mod":                               "module github.com/urnetwork/sidecar\n\ngo 1.26\n",
		"sidecar/sidecar.go":                           "package sidecar\n",
		"sidecar/planted.go":                           "package sidecar\n",
		"sidecar/tagged.go":                            "//go:build !ios_extension\n\npackage sidecar\n",
		"sidecar/check/testdata/ios_extension_leg.txt": "# the fixture's list\nprotocol/p.go\nsidecar.go\n",
		"sidecar/protocol/p.go":                        "package protocol\n",
		"sidecar/protocol/bridge/b.go":                 "package bridge\n",
		"sidecar/device/d.go":                          "package device\n",
		"sidecar/mcp/m.go":                             "package mcp\n",
		"sidecar/sidecartest/s.go":                     "package sidecartest\n",
		"sidecar/sidecartest/t4cell/c.go":              "package t4cell\n",
	}
	for name, content := range fixtureFiles {
		fixturePath := filepath.Join(fixture, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(fixturePath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fixturePath, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	host := []string{
		"host: core/device_sidecar_proto.go imports github.com/urnetwork/sidecar/protocol, which a device file may not",
		"host: core/other.go imports github.com/urnetwork/sidecar/device, and only sidecar*.go and device_sidecar*.go of the root package import the sidecar module",
		"host: core/sidecar_mcp.go imports github.com/urnetwork/sidecar/mcp, which a role file may not",
		"host: core/sidecar_cells.go imports github.com/urnetwork/sidecar/sidecartest/t4cell, which a role file may not",
	}
	for _, c := range []struct {
		leg  messagingBoundaryLeg
		want []string
	}{
		{
			leg:  messagingBoundaryLeg{name: "host", environment: []string{"GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0"}},
			want: host,
		},
		{
			leg: messagingBoundaryLeg{name: "extension", environment: []string{"GOOS=darwin", "GOARCH=arm64", "CGO_ENABLED=0"}, tags: "ios_extension"},
			want: []string{
				"extension: core/device_sidecar_proto.go imports github.com/urnetwork/sidecar/protocol, which a device file may not",
				"extension: core/other.go imports github.com/urnetwork/sidecar/device, and only sidecar*.go and device_sidecar*.go of the root package import the sidecar module",
				"extension: core/sidecar_untagged.go imports github.com/urnetwork/sidecar, and a sidecar-role file carries !ios_extension, since the iOS extension runs forwarding alone",
				"extension compiles the sidecar module's planted.go, which its ios_extension leg list does not name",
			},
		},
		{
			leg: messagingBoundaryLeg{name: "host", environment: []string{"GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0"}, test: true},
			want: slices.Concat(host, []string{
				"host: core/core_test.go imports github.com/urnetwork/sidecar/sidecartest, and only sidecar*.go and device_sidecar*.go of the root package import the sidecar module",
			}),
		},
	} {
		c.leg.moduleDirectory = "core"
		c.leg.patterns = []string{"."}
		findings, allowedCounts := sidecarImportFindings(t, fixture, c.leg)
		want := slices.Clone(c.want)
		sort.Strings(want)
		if !slices.Equal(findings, want) {
			t.Errorf("%s (test=%v): findings\n%s\nwant\n%s", c.leg.name, c.leg.test, strings.Join(findings, "\n"), strings.Join(want, "\n"))
		}
		if allowedCounts["device"] != 3 {
			t.Errorf("%s: %d device imports allowed, want 3", c.leg.name, allowedCounts["device"])
		}
		// the role test file's harness import, on the test leg alone
		if wantRoleTest := map[bool]int{false: 0, true: 1}[c.leg.test]; allowedCounts["role test"] != wantRoleTest {
			t.Errorf("%s (test=%v): %d role test imports allowed, want %d", c.leg.name, c.leg.test, allowedCounts["role test"], wantRoleTest)
		}
	}
}
