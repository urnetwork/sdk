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

// And the same property held against the REAL file rather than against a string written here: the
// loopback world is the only build-tag-gated file in this package today, it really does carry
// //export directives, and not one of them may reach the .def.
//
// The second half of this -- that the file HAS exports -- is what keeps the first half from
// passing vacuously if the harness is ever deleted or renamed.
func TestTheLoopbackHarnessIsNotInTheShippingLibrarysDef(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not resolve test path")
	}
	path := filepath.Join(filepath.Dir(filename), "..", "loopback_test_world.go")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the loopback harness is not where this test expects it: %v", err)
	}
	source := string(b)

	exports := exportDirective.FindAllStringSubmatch(source, -1)
	if len(exports) == 0 {
		t.Fatal("loopback_test_world.go declares no //export, so this case proves nothing")
	}
	for _, m := range exports {
		if !strings.HasPrefix(m[1], "urnet_message_loopback_") {
			t.Errorf("the harness exports %q, which is not under the urnet_message_loopback_ prefix "+
				"the shipping-library check in ctest/run.sh greps for", m[1])
		}
	}
	if inAnyShippedBuild(source) {
		t.Fatalf("the harness's %d exports would reach include/urnetwork_sdk.def, and they are in "+
			"no shipped library", len(exports))
	}

	// and the .def as it stands names none of them
	defBytes, err := os.ReadFile(filepath.Join(filepath.Dir(filename), "..", "include", "urnetwork_sdk.def"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range exports {
		if strings.Contains(string(defBytes), m[1]) {
			t.Errorf("include/urnetwork_sdk.def names the harness symbol %q", m[1])
		}
	}

	// THROUGH manualExports ITSELF, not only through inAnyShippedBuild. The generator runs from the
	// cgo module root, so this goes there and calls the real function. Without these lines,
	// deleting the `if !inAnyShippedBuild(...)` guard from manualExports leaves every case in this
	// file green -- which was true of this test until it grew them.
	t.Chdir(filepath.Join(filepath.Dir(filename), ".."))
	manual := manualExports(".")
	if len(manual) == 0 {
		t.Fatal("manualExports found nothing at all, so the two checks below prove nothing")
	}
	byName := map[string]bool{}
	for _, name := range manual {
		byName[name] = true
	}
	for _, m := range exports {
		if byName[m[1]] {
			t.Errorf("manualExports published the harness symbol %q, which no shipped library exports", m[1])
		}
	}
	// and the hand-written messaging surface, which DOES ship, is still found -- the control that
	// says the exclusion above is about the build tag rather than about the scan being broken
	if !byName["urnet_message_group_send"] {
		t.Errorf("manualExports did not find urnet_message_group_send among its %d names; on a CRLF "+
			"checkout that is the `\\r?$` in its scan pattern having been lost", len(manual))
	}
}

// EVERY HAND-WRITTEN EXPORT THAT SHIPS IS NAMED IN THE .def, AND THE MESSAGING SURFACE IS ALL OF IT.
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
// THE .def IS ONE PROMISE ABOUT THE SHIPPED LIBRARY AND THE HEADER IS THE OTHER, and until this
// case only the first was kept. A C or C++ consumer needs BOTH: the .def is what the import
// library is built from, so a name missing there is a LINK error, and the header is what the
// compiler reads, so a name missing there is a COMPILE error one step earlier.
//
// WRITTEN BECAUSE THE SECOND ONE HAPPENED. urnet_message_group_add_member_and_publish was added
// to exports_message.go and to the .def, and the patch that was meant to declare it in
// urnetwork_message.h built the explanatory comment and never appended the declaration under it.
// Nine lines landed, every one of them comment, and the commit message said the symbol was
// declared. Everything in this module passed: the exports were real, the .def named them, both
// suites were green. The Windows client found it with C3861, identifier not found, the first
// time it called the verb -- which is a compile in another repository and is no kind of gate.
//
// THE MATCH IS ON THE DECLARATION AND NOT ON THE NAME ANYWHERE IN THE FILE, which is the whole
// point: the name appeared SEVEN times in that header, all inside comments about it, and a
// strings.Contains gate would have passed the defect it was written for. What is required is the
// name followed by an open parenthesis, at the start of a line, which is what a declaration is
// and what a comment about one is not.
func TestTheMessagingHeaderDeclaresEveryMessagingExport(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not resolve test path")
	}
	root := filepath.Join(filepath.Dir(filename), "..")
	headerBytes, err := os.ReadFile(filepath.Join(root, "include", "urnetwork_message.h"))
	if err != nil {
		t.Fatal(err)
	}
	header := strings.ReplaceAll(string(headerBytes), "\r\n", "\n")

	// A DECLARATION, not a mention. `(?m)^[^/*].*\bNAME\(` -- the line must not open a comment
	// and the name must be followed by its parameter list.
	declares := func(name string) bool {
		return regexp.MustCompile(`(?m)^[^/* ].*\b` + regexp.QuoteMeta(name) + `\(`).MatchString(header)
	}

	b, err := os.ReadFile(filepath.Join(root, "exports_message.go"))
	if err != nil {
		t.Fatal(err)
	}
	found := exportDirective.FindAllStringSubmatch(string(b), -1)
	if len(found) == 0 {
		t.Fatal("exports_message.go declares no //export at all, so this case would pass vacuously")
	}
	missing := []string{}
	for _, m := range found {
		if !declares(m[1]) {
			missing = append(missing, m[1])
		}
	}

	// TWO CONTROLS, AND THE SECOND IS THE ONE THAT MATTERS. The first says the matcher can find a
	// declaration that is there. The second says it does NOT find a name that appears only in
	// prose -- without it, a matcher that answered true for every string in the file would pass
	// this whole case and would have passed the defect it exists for.
	if !declares("urnet_message_group_add_member") {
		t.Fatal("CONTROL FAILED: the matcher cannot find a declaration that is in the header")
	}
	if declares("urnet_message_this_name_is_in_no_declaration") {
		t.Fatal("CONTROL FAILED: the matcher answers yes for a name the header does not declare")
	}
	const prose = "A JOIN CODE IS NOT A SECRET"
	if strings.Contains(header, prose) && declares(prose) {
		t.Fatal("CONTROL FAILED: the matcher treats comment prose as a declaration")
	}

	if len(missing) != 0 {
		t.Fatalf("include/urnetwork_message.h does not DECLARE %d of the %d messaging exports, so a C consumer calling them does not compile: %v",
			len(missing), len(found), missing)
	}
	t.Logf("include/urnetwork_message.h declares all %d messaging exports", len(found))
}

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
	messaging := 0
	missing := []string{}
	for _, name := range manual {
		if strings.HasPrefix(name, "urnet_message_") {
			messaging += 1
		}
		if !strings.Contains(def, "\n\t"+name+"\n") {
			missing = append(missing, name)
		}
	}
	// the control that stops an empty scan passing: the messaging file declares its exports, and
	// the scan must find every one of them
	b, err := os.ReadFile("exports_message.go")
	if err != nil {
		t.Fatal(err)
	}
	declared := len(exportDirective.FindAllStringSubmatch(string(b), -1))
	if declared == 0 || messaging != declared {
		t.Fatalf("exports_message.go declares %d //export and manualExports found %d urnet_message_ names", declared, messaging)
	}
	if len(missing) != 0 {
		t.Fatalf("include/urnetwork_sdk.def does not name %d of the %d hand-written exports that ship, so an MSVC consumer linking through the import library cannot reach them: %v",
			len(missing), len(manual), missing)
	}
	t.Logf("include/urnetwork_sdk.def names all %d hand-written exports that ship, %d of them messaging", len(manual), messaging)
}
