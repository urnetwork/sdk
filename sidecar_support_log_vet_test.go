//go:build !ios_extension && !js

package sdk

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"

	"github.com/urnetwork/sidecar"
)

// The support log's analyzer over this repository's files (DESIGN-COMPUTE §28.7), as sidecar's check/support_log_test.go
// holds the sidecar repository's: every write to the support log passes a code constant and, as its fields, direct
// calls of the constructors that code declares, read from the root's registry, so no string reaches the app's ring that
// the registry did not declare.
func TestSupportLogWritesHereAreDeclared(t *testing.T) {
	// the Go name of each code, as the generator spells it
	codes := map[string]sidecar.SupportLogCode{}
	for _, code := range sidecar.SupportLogCodes() {
		codes["SupportLogCode"+testSupportLogCamel(strings.ReplaceAll(code.String(), ".", "_"))] = code
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	writes := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fileSet := token.NewFileSet()
		syntax, err := parser.ParseFile(fileSet, filepath.Join(".", name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		checked := map[*ast.CallExpr]bool{}
		ast.Inspect(syntax, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			codeName := testSupportLogName(call.Args[0])
			if !strings.HasPrefix(codeName, "SupportLogCode") {
				return true
			}
			writes += 1
			at := fileSet.Position(call.Pos())
			code, declared := codes[codeName]
			if !declared {
				t.Errorf("%s: a support log write names %s, which the registry does not declare", at, codeName)
				return true
			}
			if call.Ellipsis.IsValid() {
				t.Errorf("%s: %s's write spreads its fields", at, codeName)
				return true
			}
			for _, argument := range call.Args[1:] {
				fieldCall, ok := argument.(*ast.CallExpr)
				constructor := ""
				if ok {
					constructor = testSupportLogName(fieldCall.Fun)
				}
				if !ok || !strings.HasPrefix(constructor, "SupportLogField") || !code.Declares(testSupportLogSnake(strings.TrimPrefix(constructor, "SupportLogField"))) {
					t.Errorf("%s: %s's write passes a field that is no constructor it declares", at, codeName)
					continue
				}
				checked[fieldCall] = true
			}
			return true
		})
		ast.Inspect(syntax, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.CallExpr:
				if name := testSupportLogName(typed.Fun); strings.HasPrefix(name, "SupportLogField") && !checked[typed] {
					t.Errorf("%s: %s builds a field away from a support log write", fileSet.Position(typed.Pos()), name)
				}
			case *ast.CompositeLit:
				if testSupportLogName(typed.Type) == "SupportLogField" {
					t.Errorf("%s: a SupportLogField literal", fileSet.Position(typed.Pos()))
				}
			}
			return true
		})
	}
	if writes == 0 {
		t.Fatal("found no support log write in this package, so the analyzer read nothing")
	}
}

// An identifier's or a selector's name.
func testSupportLogName(expression ast.Expr) string {
	switch typed := expression.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.SelectorExpr:
		return typed.Sel.Name
	}
	return ""
}

// support_log as SupportLog, as the registry's generator spells a name.
func testSupportLogCamel(name string) string {
	parts := strings.Split(name, "_")
	for i, part := range parts {
		if part != "" {
			parts[i] = strings.ToUpper(part[:1]) + part[1:]
		}
	}
	return strings.Join(parts, "")
}

// AutoSpawned as auto_spawned, the registry's spelling of a constructor's field.
func testSupportLogSnake(name string) string {
	snake := strings.Builder{}
	for i, r := range name {
		if unicode.IsUpper(r) {
			if 0 < i {
				snake.WriteByte('_')
			}
			r = unicode.ToLower(r)
		}
		snake.WriteRune(r)
	}
	return snake.String()
}
