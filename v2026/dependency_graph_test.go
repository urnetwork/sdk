package sdk

import (
	"bufio"
	"bytes"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

// TestSdkArtifactModulePionVersionsMatchRoot prevents the native and browser
// artifact builders from silently compiling an older WebRTC/ICE/SCTP graph
// than the main SDK. Replacements in a dependency module are not inherited,
// and every nested module has its own go.mod/go.sum release boundary.
func TestSdkArtifactModulePionVersionsMatchRoot(t *testing.T) {
	rootVersions := testingPionModuleVersions(t, "go.mod")
	for _, modulePath := range []string{
		"build/go.mod",
		"cgo/go.mod",
		"js/go.mod",
	} {
		artifactVersions := testingPionModuleVersions(t, modulePath)
		if diff := testingModuleVersionDiff(rootVersions, artifactVersions); diff != "" {
			t.Errorf("%s Pion dependency graph differs from the SDK root:\n%s", modulePath, diff)
		}
	}
}

func testingPionModuleVersions(t *testing.T, path string) map[string]string {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	versions := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 || !strings.HasPrefix(fields[0], "github.com/pion/") {
			continue
		}
		versions[fields[0]] = fields[1]
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(versions) == 0 {
		t.Fatalf("%s contains no Pion module versions", path)
	}
	return versions
}

func testingModuleVersionDiff(expected map[string]string, actual map[string]string) string {
	moduleSet := map[string]bool{}
	for module := range expected {
		moduleSet[module] = true
	}
	for module := range actual {
		moduleSet[module] = true
	}
	modules := make([]string, 0, len(moduleSet))
	for module := range moduleSet {
		modules = append(modules, module)
	}
	slices.Sort(modules)

	var differences strings.Builder
	for _, module := range modules {
		expectedVersion, expectedOk := expected[module]
		actualVersion, actualOk := actual[module]
		if expectedOk && actualOk && expectedVersion == actualVersion {
			continue
		}
		fmt.Fprintf(
			&differences,
			"%s: root=%q artifact=%q\n",
			module,
			expectedVersion,
			actualVersion,
		)
	}
	return differences.String()
}

// TestSdkArtifactModulePionClassIsTheResolvedGraphAndNotTheRequireBlocks closes the hole the
// review found in the guard above: ITS CLASS IS THE MODULES NAMED IN go.mod TEXT, and the graph
// the compiler resolves is larger.
//
// Measured 2026-09-12 with this toolchain: the root's require blocks name 16 Pion modules and
// `go list -m all` resolves 17. The seventeenth is github.com/pion/transport/v3 v3.1.1, which no
// require block in any of the four modules names -- it is an indirect requirement of a pion
// module rather than of this one -- so it is outside a guard that reads require blocks, and an
// artifact module compiling a different v3 than the root would not be a finding anywhere.
//
// THE CLASS HERE IS THE MODULE GRAPH, read off go.sum. That is the derivation available OFFLINE:
// the artifact modules cannot answer `go list -m all` in this checkout at all -- they say
// "updates to go.mod needed; to update it: go mod tidy" -- so a guard that shelled out to the
// resolver would be a guard that does not run. go.sum is a superset of the resolved selection
// (it carries every version the graph mentions, not only the selected one), and for these
// modules its PATH projection is exactly the resolved graph's: 17 paths in the root's go.sum,
// 17 in `go list -m all`, the same 17.
//
// AND THE GATE PRINTS ITS COMPLEMENT -- the graph modules no require block names -- rather than
// asserting it is non-empty, because an empty complement would be the reading that silently
// means "the guard covers everything" when it might instead mean the derivation broke.
func TestSdkArtifactModulePionClassIsTheResolvedGraphAndNotTheRequireBlocks(t *testing.T) {
	modules := []string{"go.mod", "build/go.mod", "cgo/go.mod", "js/go.mod"}

	rootGraph := testingPionGraphVersions(t, "go.sum")
	rootRequired := testingPionModuleVersions(t, "go.mod")

	// the complement: graph modules that NO require block in ANY of the four modules names.
	named := map[string]bool{}
	for _, modulePath := range modules {
		for module := range testingPionModuleVersions(t, modulePath) {
			named[module] = true
		}
	}
	complement := []string{}
	for module := range rootGraph {
		if !named[module] {
			complement = append(complement, module)
		}
	}
	slices.Sort(complement)
	t.Logf("Pion modules in the root's MODULE GRAPH: %d", len(rootGraph))
	t.Logf("Pion modules NAMED in the root's require blocks: %d", len(rootRequired))
	t.Logf("THE COMPLEMENT -- graph modules no require block in any of the four names (%d): %v",
		len(complement), complement)

	// every module's graph carries the same Pion module PATHS as the root's. A path in one and
	// not another is an artifact compiling a Pion graph the root does not have, or the reverse.
	rootPaths := slices.Sorted(maps.Keys(rootGraph))
	for _, modulePath := range modules[1:] {
		sumPath := strings.TrimSuffix(modulePath, ".mod") + ".sum"
		graph := testingPionGraphVersions(t, sumPath)
		paths := slices.Sorted(maps.Keys(graph))
		if !slices.Equal(rootPaths, paths) {
			t.Errorf("%s's Pion module graph is %v and the root's is %v", sumPath, paths, rootPaths)
			continue
		}
		// and for the COMPLEMENT specifically -- the modules the require-block guard above
		// cannot see -- the versions the graph offers must match too. This is the check
		// that was missing: without it pion/transport/v3 could differ between the root and
		// an artifact with nothing to say so.
		for _, module := range complement {
			if !slices.Equal(graph[module], rootGraph[module]) {
				t.Errorf("%s offers %s at %v and the root offers %v; this module is outside the require-block guard, so nothing else would have said so",
					sumPath, module, graph[module], rootGraph[module])
			}
		}
	}
}

// testingPionGraphVersions reads a go.sum and answers, per Pion module path, the sorted set of
// versions that appear in it -- the module GRAPH rather than the selection. The "/go.mod" suffix
// a go.sum puts on half its lines is stripped, so a module contributes one version and not two.
func testingPionGraphVersions(t *testing.T, path string) map[string][]string {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	versions := map[string]map[string]bool{}
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 || !strings.HasPrefix(fields[0], "github.com/pion/") {
			continue
		}
		version := strings.TrimSuffix(fields[1], "/go.mod")
		if versions[fields[0]] == nil {
			versions[fields[0]] = map[string]bool{}
		}
		versions[fields[0]][version] = true
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(versions) == 0 {
		t.Fatalf("%s contains no Pion module versions", path)
	}
	graph := map[string][]string{}
	for module, set := range versions {
		graph[module] = slices.Sorted(maps.Keys(set))
	}
	return graph
}
