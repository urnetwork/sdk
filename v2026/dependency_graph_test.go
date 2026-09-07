package sdk

import (
	"bufio"
	"bytes"
	"fmt"
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
