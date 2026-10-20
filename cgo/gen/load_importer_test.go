// Preserve the type contract while importing the current compiler's export
// data, and reject unavailable or incompatible metadata without source fallback.
package main

import (
	"bytes"
	"encoding/binary"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// Write only synthetic local modules; the owner pins both go processes to the
// same toolchain. These serial tests must not fetch dependencies from a proxy.
func testingGeneratorImportFixture(t *testing.T, sources map[string]string) packages.Config {
	t.Helper()
	t.Setenv("GOWORK", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	t.Setenv("GOPROXY", "off")
	fixtureDirectory := t.TempDir()
	for relativePath, source := range sources {
		filename := filepath.Join(fixtureDirectory, relativePath)
		if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return packages.Config{Dir: filepath.Join(fixtureDirectory, "sdk")}
}

// A diamond through separate replaced modules must retain canonical type
// identity, generic aliases, private embedded fields and named result metadata.
func TestGeneratorPreservesToolchainExportMetadata(t *testing.T) {
	config := testingGeneratorImportFixture(t, map[string]string{
		"sdk/go.mod": "module " + sdkPath + `

go 1.26.5

require (
	bindings.example/dependency v0.0.0
	bindings.example/bridge v0.0.0
)
replace bindings.example/dependency => ../dependency
replace bindings.example/bridge => ../bridge
`,
		"dependency/go.mod": "module bindings.example/dependency\n\ngo 1.26.5\n",
		"bridge/go.mod":     "module bindings.example/bridge\n\ngo 1.26.5\n\nrequire bindings.example/dependency v0.0.0\n",
		"dependency/dependency.go": `package dependency

type privateRecord[T any] struct { Value T }
type PublicRecord[T any] struct { privateRecord[T] }
type RecordAlias[T any] = PublicRecord[T]
func (self *PublicRecord[T]) Zeta() T { return self.Value }
func (self *PublicRecord[T]) Alpha() T { return self.Value }
func (self *PublicRecord[T]) Middle() T { return self.Value }
`,
		"bridge/bridge.go": `package bridge

import "bindings.example/dependency"

type RecordAlias[T any] = dependency.RecordAlias[T]
`,
		"sdk/sdk.go": `package sdk

import (
	"bindings.example/dependency"
	"bindings.example/bridge"
)

type State struct {
	Direct dependency.PublicRecord[string]
	Indirect bridge.RecordAlias[string]
}
type StateAlias[T any] = dependency.RecordAlias[T]
type Device interface { Value() string }
func (self *State) Value() string { return self.Direct.Middle() }
func SaveState(state *State) (errorId string) { return "" }
`,
	})
	config.ParseFile = func(fileSet *token.FileSet, filename string, source []byte) (*ast.File, error) {
		t.Errorf("export-metadata load parsed source %s", filepath.Base(filename))
		return parser.ParseFile(fileSet, filename, source, parser.AllErrors)
	}
	g, err := loadWithConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	stateObject, ok := g.scope.Lookup("State").(*types.TypeName)
	if !ok {
		t.Fatal("missing root State type")
	}
	state := types.Unalias(stateObject.Type()).(*types.Named)
	fields := state.Underlying().(*types.Struct)
	direct := types.Unalias(fields.Field(0).Type()).(*types.Named)
	indirect := types.Unalias(fields.Field(1).Type()).(*types.Named)
	if direct.Origin() != indirect.Origin() || !types.Identical(direct, indirect) || direct.Obj().Pkg() != indirect.Obj().Pkg() {
		t.Fatal("diamond imports lost canonical type or package identity")
	}
	if direct.Obj().Pkg().Path() != "bindings.example/dependency" || direct.TypeArgs().Len() != 1 || !types.Identical(direct.TypeArgs().At(0), types.Typ[types.String]) {
		t.Fatal("local module replacement changed the canonical generic type")
	}
	aliasObject, ok := g.scope.Lookup("StateAlias").(*types.TypeName)
	if !ok || !aliasObject.IsAlias() {
		t.Fatal("generic alias was lost")
	}
	alias, ok := aliasObject.Type().(*types.Alias)
	if !ok || alias.TypeParams().Len() != 1 {
		t.Fatal("generic alias parameter was lost")
	}
	instantiated, err := types.Instantiate(nil, alias, []types.Type{types.Typ[types.String]}, true)
	if err != nil || !types.Identical(instantiated, direct) {
		t.Fatalf("generic alias no longer names the shared instance: %v", err)
	}
	embedded := direct.Underlying().(*types.Struct).Field(0)
	privateRecord := types.Unalias(embedded.Type()).(*types.Named)
	if !embedded.Embedded() || embedded.Exported() || privateRecord.Obj().Name() != "privateRecord" || !types.Identical(privateRecord.Underlying().(*types.Struct).Field(0).Type(), types.Typ[types.String]) {
		t.Fatal("private embedded generic type or substitution was lost")
	}
	methodSet := types.NewMethodSet(types.NewPointer(direct))
	if methodSet.Len() != 3 {
		t.Fatalf("dependency method count = %d, want 3", methodSet.Len())
	}
	for _, name := range []string{"Zeta", "Alpha", "Middle"} {
		method := methodSet.Lookup(nil, name)
		if method == nil || !types.Identical(method.Obj().Type().(*types.Signature).Results().At(0).Type(), types.Typ[types.String]) {
			t.Fatalf("generic receiver substitution changed for %s", name)
		}
	}
	if g.deviceType == nil || !types.Implements(types.NewPointer(state), g.deviceType.Underlying().(*types.Interface)) {
		t.Fatal("root method set no longer implements Device")
	}
	save, ok := g.scope.Lookup("SaveState").(*types.Func)
	if !ok || !errorIdResult(save.Type().(*types.Signature)) || save.Type().(*types.Signature).Params().At(0).Name() != "state" {
		t.Fatal("named result or parameter metadata changed")
	}
}

// Missing metadata and unreadable files must be errors, never requests to parse
// source or partially construct the generator's package.
func TestGeneratorRejectsUnavailableExportMetadata(t *testing.T) {
	for _, exportFile := range []string{"", filepath.Join(t.TempDir(), "missing.a")} {
		pkg, err := importGeneratorPackage(&packages.Package{PkgPath: sdkPath, ExportFile: exportFile}, nil)
		if err == nil || pkg != nil {
			t.Fatalf("unavailable export data returned package=%v, error=%v", pkg, err)
		}
	}
}

// A canonical import path cannot identify two different export artifacts.
func TestGeneratorRejectsAmbiguousExportMetadata(t *testing.T) {
	root := &packages.Package{
		PkgPath: sdkPath,
		Imports: map[string]*packages.Package{
			"first":  {PkgPath: "bindings.example/dependency", ExportFile: "first.a"},
			"second": {PkgPath: "bindings.example/dependency", ExportFile: "second.a"},
		},
	}
	pkg, err := importGeneratorPackage(root, nil)
	if err == nil || pkg != nil || !strings.Contains(err.Error(), "conflicting export files") {
		t.Fatalf("ambiguous export metadata returned package=%v, error=%v", pkg, err)
	}
}

// Corrupt only a private copy of a real export archive's version word. The
// decoder panic must become an error; build-cache data remains untouched.
func TestGeneratorRejectsUnsupportedExportVersion(t *testing.T) {
	config := testingGeneratorImportFixture(t, map[string]string{
		"sdk/go.mod": "module " + sdkPath + "\n\ngo 1.26.5\n",
		"sdk/sdk.go": "package sdk\n\ntype State struct{}\n",
	})
	config.Mode = packages.NeedName | packages.NeedImports | packages.NeedDeps | packages.NeedExportFile
	loaded, err := packages.Load(&config, sdkPath)
	if err != nil || packages.PrintErrors(loaded) != 0 || len(loaded) != 1 {
		t.Fatalf("load private export fixture: %v", err)
	}
	data, err := os.ReadFile(loaded[0].ExportFile)
	if err != nil {
		t.Fatal(err)
	}
	marker := []byte("\n$$B\nu")
	markerOffset := bytes.Index(data, marker)
	versionOffset := markerOffset + len(marker)
	if markerOffset < 0 || len(data) < versionOffset+4 {
		t.Fatal("fixture export archive lacks the unified-format version word")
	}
	binary.LittleEndian.PutUint32(data[versionOffset:versionOffset+4], ^uint32(0))
	exportFile := filepath.Join(t.TempDir(), "unsupported.a")
	if err := os.WriteFile(exportFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	root := *loaded[0]
	root.ExportFile = exportFile
	pkg, err := importGeneratorPackage(&root, nil)
	if err == nil || pkg != nil || !strings.Contains(err.Error(), "export data version") || !strings.Contains(err.Error(), "greater than maximum supported version") {
		t.Fatalf("unsupported export version returned package=%v, error=%v", pkg, err)
	}
}
