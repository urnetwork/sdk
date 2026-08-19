// Device receive-callback policy tests keep SDK-owned Connect subscribers in
// an explicit audit inventory.
package sdk

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestSdkClientReceiveRegistrationsAreAudited fails when a new SDK subscriber
// bypasses the nonblocking receive review in connect/CODESTYLE.md.
func TestSdkClientReceiveRegistrationsAreAudited(t *testing.T) {
	fileSet := token.NewFileSet()
	var registrations []string
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != "." && strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		path = filepath.ToSlash(path)
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "AddReceiveCallback" {
				return true
			}
			var callback bytes.Buffer
			if err := format.Node(&callback, fileSet, call.Args[0]); err != nil {
				t.Fatalf("format SDK receive callback: %v", err)
			}
			registrations = append(registrations, path+":"+callback.String())
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("parse SDK production Go: %v", err)
	}
	sort.Strings(registrations)
	expected := []string{"device_local_provider.go:provider.handleControlFrames"}
	if len(registrations) != len(expected) {
		t.Fatalf("SDK receive registrations = %v, want %v; audit the new boundary", registrations, expected)
	}
	for index := range expected {
		if registrations[index] != expected[index] {
			t.Fatalf("SDK receive registrations = %v, want %v; audit the changed boundary", registrations, expected)
		}
	}
}
