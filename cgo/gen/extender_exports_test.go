// The generated c abi is regenerated with the sdk surface it wraps. Every
// extender identifier the sdk exports is accounted for in the generated
// coverage report -- exported under its c name, or skipped with a reason --
// every extender constant has its define in the header, and the module
// definition and the report agree on what crosses.
//
// A surface added without `make generate` leaves the windows and linux apps
// unable to call it at all, and nothing else in the tree notices: the c files
// still build, and the sdk suite still passes.
package main

import (
	"go/constant"
	"go/types"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The extender surface by name: an extender, the gossip network it joins, or
// the alt service its carriers reach.
func testingExtenderIdentifier(name string) bool {
	for _, token := range []string{"Extender", "Gossip", "AltUrl"} {
		if strings.Contains(name, token) {
			return true
		}
	}
	return false
}

// The same, on the c side, where the names are snake case.
func testingExtenderCName(cName string) bool {
	for _, token := range []string{"extender", "gossip", "alt_url"} {
		if strings.Contains(cName, token) {
			return true
		}
	}
	return false
}

// The directory holding the generator, which every generated artifact is
// beside or under.
func testingGenDir(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not resolve test path")
	}
	return filepath.Dir(filename)
}

func testingGeneratedFile(t *testing.T, parts ...string) string {
	t.Helper()
	fileBytes, err := os.ReadFile(filepath.Join(append([]string{testingGenDir(t)}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	return string(fileBytes)
}

// The coverage report as two lookups: the c name of every exported identifier,
// and every identifier the report accounts for at all, exported or skipped.
func testingCoverageReport(t *testing.T) (map[string]string, map[string]bool) {
	t.Helper()
	exportedCNames := map[string]string{}
	accounted := map[string]bool{}
	exported := false
	for _, line := range strings.Split(testingGeneratedFile(t, "..", "coverage_report.txt"), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "== exported"):
			exported = true
			continue
		case strings.HasPrefix(line, "== skipped"):
			exported = false
			continue
		case line == "":
			continue
		}
		if exported {
			cName, identifier, ok := strings.Cut(line, " <- ")
			if !ok {
				continue
			}
			exportedCNames[identifier] = cName
			accounted[identifier] = true
			continue
		}
		if identifier, _, ok := strings.Cut(line, ": "); ok {
			accounted[identifier] = true
		}
	}
	if len(exportedCNames) == 0 || len(accounted) == 0 {
		t.Fatal("the coverage report parsed to nothing")
	}
	return exportedCNames, accounted
}

// The symbols of the windows module definition.
func testingDefSymbols(t *testing.T) map[string]bool {
	t.Helper()
	symbols := map[string]bool{}
	for _, line := range strings.Split(testingGeneratedFile(t, "..", "include", "urnetwork_sdk.def"), "\n") {
		if symbol := strings.TrimSpace(line); strings.HasPrefix(symbol, "urnet_") {
			symbols[symbol] = true
		}
	}
	if len(symbols) == 0 {
		t.Fatal("the module definition parsed to nothing")
	}
	return symbols
}

// Every extender identifier of the live sdk surface is in the generated
// coverage report, and every extender constant has its header define.
func TestExtenderSurfaceIsGenerated(t *testing.T) {
	g := testingPreferenceGenerator(t)
	_, accounted := testingCoverageReport(t)
	header := testingGeneratedFile(t, "..", "include", "urnetwork_sdk.h")

	requireAccounted := func(identifier string) {
		t.Helper()
		if !accounted[identifier] {
			t.Errorf(
				"%s is not in the generated coverage report; run `make generate` in cgo",
				identifier,
			)
		}
	}

	extenderIdentifiers := 0
	for _, name := range g.scope.Names() {
		object := g.scope.Lookup(name)
		if !object.Exported() {
			continue
		}
		switch object := object.(type) {
		case *types.Func:
			if !testingExtenderIdentifier(name) {
				continue
			}
			extenderIdentifiers += 1
			requireAccounted(name)
		case *types.Const:
			if !testingExtenderIdentifier(name) {
				continue
			}
			if object.Val().Kind() != constant.String {
				continue
			}
			extenderIdentifiers += 1
			define := "URNET_" + strings.ToUpper(snake(name))
			if !strings.Contains(header, "#define "+define+" ") {
				t.Errorf("%s has no %s define in the generated header", name, define)
			}
		case *types.TypeName:
			named, ok := types.Unalias(object.Type()).(*types.Named)
			if !ok {
				continue
			}
			// only a behavioral type crosses method by method; every other
			// one crosses whole -- as json, or as a c callback -- and is
			// named by the report as itself
			if !behavioralTypes[name] {
				if testingExtenderIdentifier(name) {
					extenderIdentifiers += 1
					requireAccounted(name)
				}
				continue
			}
			methodSet := types.NewMethodSet(types.NewPointer(named))
			for i := range methodSet.Len() {
				method := methodSet.At(i).Obj()
				if !method.Exported() {
					continue
				}
				// the whole surface of an extender type, and the extender part
				// of every other one
				if !testingExtenderIdentifier(name) && !testingExtenderIdentifier(method.Name()) {
					continue
				}
				extenderIdentifiers += 1
				requireAccounted(name + "." + method.Name())
			}
		}
	}
	// the sdk carries an extender surface at all, so a lookup that silently
	// found nothing is a failure rather than a pass
	if extenderIdentifiers < 20 {
		t.Fatalf("the sdk scope named %d extender identifiers, expected the whole surface", extenderIdentifiers)
	}
}

// The module definition and the coverage report are emitted from one pass, so
// they agree: every extender symbol windows links against is one the report
// names, and every extender c name the report exports is in the definition.
func TestExtenderDefAndCoverageReportAgree(t *testing.T) {
	exportedCNames, _ := testingCoverageReport(t)
	defSymbols := testingDefSymbols(t)
	report := testingGeneratedFile(t, "..", "coverage_report.txt")

	for symbol := range defSymbols {
		if !testingExtenderCName(symbol) {
			continue
		}
		// a manual export is named by the skipped line that defers to it
		if !strings.Contains(report, symbol) {
			t.Errorf("%s is exported but not in the coverage report", symbol)
		}
	}
	for identifier, cName := range exportedCNames {
		if !testingExtenderCName(cName) {
			continue
		}
		if !defSymbols[cName] {
			t.Errorf("%s (%s) is in the coverage report but not in the module definition", cName, identifier)
		}
	}
}
