package main

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// exportDirective is gen.go's own scan pattern, character for character. `\r?$` rather than `$`
// because a CRLF checkout is what core.autocrlf=true gives every Windows clone, and under `$`
// this matched nothing at all there -- which would make every case below pass vacuously.
var exportDirective = regexp.MustCompile(`(?m)^//export (\w+)[ \t]*\r?$`)

// THE .def IS A PROMISE ABOUT WHAT THE SHIPPED LIBRARY CONTAINS, AND manualExports READS RAW FILE
// BYTES.
//
// An MSVC import library is generated from include/urnetwork_sdk.def. A .def that names a symbol
// the dll does not export is a link error at the consumer, and manualExports has no compiler in
// it: it greps //export out of every hand-written .go file in the package directory, build tags
// and all. So a file that is compiled out of the shipping library would still put its exports in
// the .def unless something stops it. inAnyShippedBuild is that something and this is its gate.
func TestAFileNoShippedBuildCompilesContributesNoExportedSymbol(t *testing.T) {
	for _, c := range []struct {
		name   string
		source string
		want   bool
	}{
		{"no constraint at all", "package main\n\n//export urnet_x\nfunc urnet_x() {}\n", true},
		{"a tag nobody passes", "//go:build urnet_message_loopback\n\npackage main\n", false},
		{"a tag nobody passes, negated", "//go:build !urnet_message_loopback\n\npackage main\n", true},
		{"a goos that is not this one", "//go:build js\n\npackage main\n", true},
		{"not a goos", "//go:build !js\n\npackage main\n", true},
		{"unix", "//go:build unix\n\npackage main\n", true},
		{"an and of a real tag and a made up one", "//go:build unix && urnet_message_loopback\n\npackage main\n", false},
		{"an or of a real tag and a made up one", "//go:build unix || urnet_message_loopback\n\npackage main\n", true},
		{"crlf line endings", "//go:build urnet_message_loopback\r\n\r\npackage main\r\n", false},
		{"a comment that only looks like one", "// go:build urnet_message_loopback\n\npackage main\n", true},
		{
			"a constraint below the package clause is not a constraint",
			"package main\n\n//go:build urnet_message_loopback\n",
			true,
		},
		{"an unparseable constraint is not published", "//go:build && ||\n\npackage main\n", false},
	} {
		if got := inAnyShippedBuild(c.source); got != c.want {
			t.Errorf("%s: inAnyShippedBuild answered %v, want %v", c.name, got, c.want)
		}
	}
}

// The same property held through manualExports itself rather than through inAnyShippedBuild alone:
// without it, deleting the `if !inAnyShippedBuild(...)` guard from manualExports leaves every case
// in this file green.
//
// This case used to read the messaging ABI's loopback harness, the one build-tag-gated file this
// package had; that harness moved to github.com/urnetwork/message with the messaging ABI, and no
// file here is gated that way today. So the gated file is a fixture: a directory holding a file
// every shipped build compiles, one behind a tag no shipped build passes, both again with crlf
// line endings, and the two names the scan skips outright. The real directory then supplies the
// control: every //export exports_manual.go declares is found, so the exclusion is about the
// constraint and not about a scan that finds nothing.
func TestManualExportsPublishesOnlyWhatAShippedBuildCompiles(t *testing.T) {
	fixtureDirectory := t.TempDir()
	fixtureSources := map[string]string{
		"shipped.go":             "package main\n\n//export urnet_fixture_shipped\nfunc urnet_fixture_shipped() {}\n",
		"shipped_crlf.go":        "package main\r\n\r\n//export urnet_fixture_shipped_crlf\r\nfunc urnet_fixture_shipped_crlf() {}\r\n",
		"unshipped.go":           "//go:build urnet_fixture_unshipped\n\npackage main\n\n//export urnet_fixture_unshipped\nfunc urnet_fixture_unshipped() {}\n",
		"unshipped_crlf.go":      "//go:build urnet_fixture_unshipped\r\n\r\npackage main\r\n\r\n//export urnet_fixture_unshipped_crlf\r\nfunc urnet_fixture_unshipped_crlf() {}\r\n",
		"exports_gen_fixture.go": "package main\n\n//export urnet_fixture_generated\nfunc urnet_fixture_generated() {}\n",
		"exports_core.go":        "package main\n\n//export urnet_fixture_core\nfunc urnet_fixture_core() {}\n",
	}
	for name, source := range fixtureSources {
		if err := os.WriteFile(filepath.Join(fixtureDirectory, name), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fixturePublished := map[string]bool{}
	for _, name := range manualExports(fixtureDirectory) {
		fixturePublished[name] = true
	}
	fixtureWants := map[string]bool{
		"urnet_fixture_shipped":        true,
		"urnet_fixture_shipped_crlf":   true,
		"urnet_fixture_unshipped":      false,
		"urnet_fixture_unshipped_crlf": false,
		"urnet_fixture_generated":      false,
		"urnet_fixture_core":           false,
	}
	for name, want := range fixtureWants {
		if fixturePublished[name] != want {
			t.Errorf("manualExports over the fixture directory published %s: %v, want %v", name, fixturePublished[name], want)
		}
	}
	for name := range fixturePublished {
		if _, known := fixtureWants[name]; !known {
			t.Errorf("manualExports over the fixture directory published %s, which no fixture declares", name)
		}
	}

	// the real directory, from the cgo module root the way the generator runs
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not resolve test path")
	}
	t.Chdir(filepath.Join(filepath.Dir(filename), ".."))
	published := map[string]bool{}
	for _, name := range manualExports(".") {
		published[name] = true
	}
	manualSource, err := os.ReadFile("exports_manual.go")
	if err != nil {
		t.Fatal(err)
	}
	declared := exportDirective.FindAllStringSubmatch(string(manualSource), -1)
	if len(declared) == 0 {
		t.Fatal("exports_manual.go declares no //export, so the control below proves nothing")
	}
	for _, m := range declared {
		if !published[m[1]] {
			t.Errorf("manualExports did not find %s, which exports_manual.go declares; on a crlf "+
				"checkout that is the `\\r?$` in its scan pattern having been lost", m[1])
		}
	}

	// the complement: every .go file here the scan skipped, and the rule that skipped it
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	skipped := []string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		if strings.HasPrefix(name, "exports_gen") || name == "exports_core.go" {
			skipped = append(skipped, name+" (by name)")
			continue
		}
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if !inAnyShippedBuild(string(source)) {
			skipped = append(skipped, name+" (no shipped build compiles it)")
		}
	}
	t.Logf("manualExports published %d names from the cgo package; it skipped %d files: %v", len(published), len(skipped), skipped)
}

// EVERY HAND-WRITTEN EXPORT THAT SHIPS IS NAMED IN THE .def.
//
// THIS CASE USED TO BE TestTheDefIsStaleWithRespectToTheHandWrittenMessagingSurface, AND IT PASSED
// ON THE DEFECT. It counted how many of the messaging exports the .def named and LOGGED "0 of 34;
// `make generate` is owed" -- so a .def an MSVC consumer could link none of the messaging surface
// through was a green run. Measured against the shipping library at sdk cfe3ce4, the gap was wider
// than the case knew: the DLL's cgo header declared 654 urnet_ exports and the .def named 609,
// and the 45 missing were the 34 messaging exports AND all 11 of exports_manual.go's byte-buffer
// exports. The .def now names all 654, and this is a gate rather than a log line.
//
// THE SET IS manualExports() ITSELF, run from the cgo module root the way the generator runs it, so
// a new //export in any shipped hand-written file that is not in the .def is red here, and the
// build-tag exclusion is the generator's own and not a second reading of it.
//
// WHAT IT DOES NOT HOLD: that the GENERATED exports and the .def agree. That is
// TestExportedSymbolCompatibilityBaseline's half, over its own list.
//
// The messaging exports this case also counted, and the case holding their header, moved to
// github.com/urnetwork/message with the messaging ABI. The control that stops an empty scan
// passing is now the byte-buffer surface in exports_manual.go.
func TestTheDefNamesEveryHandWrittenExportThatShips(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not resolve test path")
	}
	root := filepath.Join(filepath.Dir(filename), "..")
	defBytes, err := os.ReadFile(filepath.Join(root, "include", "urnetwork_sdk.def"))
	if err != nil {
		t.Fatal(err)
	}
	def := "\n" + strings.ReplaceAll(string(defBytes), "\r\n", "\n") + "\n"
	t.Chdir(root)
	manual := manualExports(".")
	published := map[string]bool{}
	missing := []string{}
	for _, name := range manual {
		published[name] = true
		if !strings.Contains(def, "\n\t"+name+"\n") {
			missing = append(missing, name)
		}
	}
	// the control that stops an empty scan passing: exports_manual.go declares its exports, and
	// the scan must find every one of them
	b, err := os.ReadFile("exports_manual.go")
	if err != nil {
		t.Fatal(err)
	}
	declared := exportDirective.FindAllStringSubmatch(string(b), -1)
	if len(declared) == 0 {
		t.Fatal("exports_manual.go declares no //export, so this case would pass vacuously")
	}
	for _, m := range declared {
		if !published[m[1]] {
			t.Fatalf("exports_manual.go declares %s and manualExports did not find it among its %d names", m[1], len(manual))
		}
	}
	if len(missing) != 0 {
		t.Fatalf("include/urnetwork_sdk.def does not name %d of the %d hand-written exports that ship, so an MSVC consumer linking through the import library cannot reach them: %v",
			len(missing), len(manual), missing)
	}
	t.Logf("include/urnetwork_sdk.def names all %d hand-written exports that ship, %d of them from exports_manual.go", len(manual), len(declared))
}
