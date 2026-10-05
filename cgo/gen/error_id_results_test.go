// An export whose string result is an error id answers "" on success, so it
// must never answer NULL when the call cannot run: the c++ wrapper takes NULL
// as "", which the apps read as saved or valid. It answered NULL for a space
// handle that did not resolve and for settings json that did not decode
// (urnet_network_space_set_vless_settings), so a save that never ran read as
// a save.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The error-id functions of the sdk surface, and how many exits each export
// has before the sdk call: one for the receiver handle and one for each json
// argument.
var testingErrorIdFunctions = []struct {
	receiver string
	method   string
	exits    int
}{
	{receiver: "NetworkSpace", method: "SetVlessSettings", exits: 2},
	{receiver: "NetworkSpace", method: "SetControlDohUrls", exits: 2},
	{method: "ValidateVlessSettings", exits: 1},
	{method: "ValidateControlDohUrl", exits: 0},
}

// The sdk symbol of an error-id function: `Receiver.Method`, or the bare name
// of a package function.
func testingErrorIdSymbol(receiver string, method string) string {
	if receiver == "" {
		return method
	}
	return receiver + "." + method
}

// Every exit where the call cannot run answers errorIdInternal, a recovered
// panic included; the sdk's own answer is passed through; and the c
// declaration is the char* it was.
func TestErrorIdExportsAnswerInternalErrorWhenTheCallCannotRun(t *testing.T) {
	g := testingPreferenceGenerator(t)
	g.emitType(testingPreferenceType(t, g, "NetworkSpace"))
	for _, name := range []string{"ValidateVlessSettings", "ValidateControlDohUrl"} {
		g.emitFunc(g.scope.Lookup(name).(*types.Func))
	}
	for _, expected := range testingErrorIdFunctions {
		symbol := testingErrorIdSymbol(expected.receiver, expected.method)
		item := testingPreferenceExport(t, g, expected.receiver, expected.method)
		if !item.sig.errorId {
			t.Errorf("%s is not mapped as an error id", symbol)
			continue
		}
		if !strings.HasPrefix(item.cDecl, "char* "+item.cName+"(") || item.sig.hasError {
			t.Errorf("%s changed its c declaration: %s", symbol, item.cDecl)
		}
		if strings.Contains(item.goCode, "return nil") {
			t.Errorf("%s still answers NULL, which reads as success:\n%s", symbol, item.goCode)
		}
		if exits := strings.Count(item.goCode, "\treturn cString(errorIdInternal)\n"); exits != expected.exits {
			t.Errorf("%s answers errorIdInternal at %d exits, want %d:\n%s", symbol, exits, expected.exits, item.goCode)
		}
		guard := fmt.Sprintf(`) (errorId *C.char) {
	defer func() {
		if r := recover(); r != nil {
			cgoPanicked(%q, r)
			errorId = cString(errorIdInternal)
		}
	}()
`, item.cName)
		if !strings.Contains(item.goCode, guard) || strings.Contains(item.goCode, "cgoGuard(") {
			t.Errorf("%s does not answer errorIdInternal for a recovered panic:\n%s", symbol, item.goCode)
		}
		if !strings.HasSuffix(item.goCode, "\treturn cString(string(r0))\n}\n") {
			t.Errorf("%s does not pass the sdk's answer through:\n%s", symbol, item.goCode)
		}
	}

	// a string that is not an error id keeps its zero result
	plain := testingPreferenceExport(t, g, "NetworkSpace", "ServiceUrl")
	if plain.sig.errorId || !strings.Contains(plain.goCode, "\t\treturn nil\n") ||
		strings.Contains(plain.goCode, "errorIdInternal") || !strings.Contains(plain.goCode, "\tdefer cgoGuard(") {
		t.Errorf("NetworkSpace.ServiceUrl is mapped as an error id:\n%s", plain.goCode)
	}
}

// The headers name the internal id once, from the one constant the generated
// go answers, and mark each error-id function where it is declared.
func TestErrorIdHeadersDefineTheInternalIdAndMarkTheFunctions(t *testing.T) {
	header := testingGeneratedFile(t, "..", "include", "urnetwork_sdk.h")
	hpp := testingGeneratedFile(t, "..", "include", "urnetwork_sdk.hpp")
	exports := testingGeneratedFile(t, "..", "exports_gen.go")
	report := testingGeneratedFile(t, "..", "coverage_report.txt")

	if errorIdInternal != "internal_error" {
		t.Fatalf("errorIdInternal is %q; the apps mirror \"internal_error\"", errorIdInternal)
	}
	for _, definition := range []struct {
		file string
		text string
		line string
	}{
		{file: "urnetwork_sdk.h", text: header, line: "#define URNET_ERROR_ID_INTERNAL \"internal_error\"\n"},
		{file: "urnetwork_sdk.hpp", text: hpp, line: "inline constexpr const char* ErrorIdInternal = URNET_ERROR_ID_INTERNAL;\n"},
		{file: "exports_gen.go", text: exports, line: "const errorIdInternal = \"internal_error\"\n"},
	} {
		if strings.Count(definition.text, definition.line) != 1 {
			t.Errorf("%s does not define the internal id once: %q", definition.file, definition.line)
		}
	}

	g := testingPreferenceGenerator(t)
	g.emitType(testingPreferenceType(t, g, "NetworkSpace"))
	for _, name := range []string{"ValidateVlessSettings", "ValidateControlDohUrl"} {
		g.emitFunc(g.scope.Lookup(name).(*types.Func))
	}
	for _, expected := range testingErrorIdFunctions {
		symbol := testingErrorIdSymbol(expected.receiver, expected.method)
		item := testingPreferenceExport(t, g, expected.receiver, expected.method)
		if !strings.Contains(header, errorIdHeaderDoc+"\n"+item.cDecl+"\n") {
			t.Errorf("the c header does not mark %s as an error id", item.cName)
		}
		if !strings.Contains(report, item.cName+" <- "+symbol+" (error id)\n") {
			t.Errorf("the coverage report does not mark %s as an error id", item.cName)
		}
	}
	plain := testingPreferenceExport(t, g, "NetworkSpace", "ServiceUrl")
	if strings.Contains(header, errorIdHeaderDoc+"\n"+plain.cDecl+"\n") {
		t.Errorf("the c header marks %s as an error id", plain.cName)
	}
}

// The generator knows an error id by the result's name, so the name has to be
// where the sdk declares one: every exported function whose doc says it
// answers an error id names its lone string result errorId, and nothing else
// carries the name. The set the apps call is pinned besides.
func TestErrorIdResultsAreNamedWhereTheSdkDeclaresThem(t *testing.T) {
	sdkDirectory := filepath.Join(testingGenDir(t), "..", "..")
	entries, err := os.ReadDir(sdkDirectory)
	if err != nil {
		t.Fatal(err)
	}
	fileSet := token.NewFileSet()
	namedSymbols := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, filepath.Join(sdkDirectory, name), nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || !function.Name.IsExported() {
				continue
			}
			receiver := ""
			if function.Recv != nil {
				receiverType := function.Recv.List[0].Type
				if star, ok := receiverType.(*ast.StarExpr); ok {
					receiverType = star.X
				}
				ident, ok := receiverType.(*ast.Ident)
				if !ok || !ident.IsExported() {
					continue
				}
				receiver = ident.Name
			}
			symbol := testingErrorIdSymbol(receiver, function.Name.Name)

			loneString := false
			resultName := ""
			if results := function.Type.Results; results != nil && len(results.List) == 1 {
				field := results.List[0]
				if ident, ok := field.Type.(*ast.Ident); ok && ident.Name == "string" && len(field.Names) <= 1 {
					loneString = true
				}
				if len(field.Names) == 1 {
					resultName = field.Names[0].Name
				}
			}
			if results := function.Type.Results; results != nil && !loneString {
				for _, field := range results.List {
					for _, fieldName := range field.Names {
						if fieldName.Name == "errorId" {
							t.Errorf("%s (%s) names a result errorId that is not its lone string result", symbol, name)
						}
					}
				}
			}
			if !loneString {
				continue
			}
			if resultName == "errorId" {
				namedSymbols[symbol] = true
			}
			if strings.Contains(strings.ToLower(function.Doc.Text()), "error id") && resultName != "errorId" {
				t.Errorf("%s (%s) answers an error id and does not name its result errorId", symbol, name)
			}
		}
	}
	for _, expected := range testingErrorIdFunctions {
		symbol := testingErrorIdSymbol(expected.receiver, expected.method)
		if !namedSymbols[symbol] {
			t.Errorf("%s does not name its result errorId", symbol)
		}
	}
	if len(namedSymbols) != len(testingErrorIdFunctions) {
		t.Errorf("the sdk names %d error-id results %v, the pinned set is %d", len(namedSymbols), namedSymbols, len(testingErrorIdFunctions))
	}
}
