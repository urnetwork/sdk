// The generator needs exported type metadata, including private types embedded
// in that surface, without parsing the implementation of every dependency.
package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/tools/go/packages"
)

// Observe the real loader's dependency parsing rather than a duration threshold.
// Export data must retain the generic, alias, method and named-result details
// that drive classification and emission in the production generator.
func TestGeneratorLoadsExportDataWithoutDependencySyntax(t *testing.T) {
	t.Setenv("GOWORK", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	t.Setenv("GOPROXY", "off")
	fixtureDirectory := t.TempDir()
	sdkDirectory := filepath.Join(fixtureDirectory, "sdk")
	sources := map[string]string{
		"sdk/go.mod":        "module " + sdkPath + "\n\ngo 1.26.5\n\nrequire bindings.example/dependency v0.0.0\nreplace bindings.example/dependency => ../dependency\n",
		"dependency/go.mod": "module bindings.example/dependency\n\ngo 1.26.5\n",
		"sdk/sdk.go": `package sdk

import "bindings.example/dependency"

type State struct {
	Record dependency.PublicRecord[string]
	Alias dependency.RecordAlias
}
type StateAlias = State
type GenericState[T any] struct { Value T }
type Device interface { Value() string }
func (self *State) Value() string { return self.Record.Get() }
func SaveState(state *State) (errorId string) { return "" }
`,
		"dependency/dependency.go": `package dependency

type privateRecord[T any] struct { Value T }
type PublicRecord[T any] struct { privateRecord[T] }
type RecordAlias = PublicRecord[string]
func (self *PublicRecord[T]) Get() T { return self.Value }
` + "\nconst privateDescriptor = " + strings.Repeat("\"fixture\" + ", 512) + "\"end\"\n",
	}
	for relativePath, source := range sources {
		filename := filepath.Join(fixtureDirectory, relativePath)
		if err := os.MkdirAll(filepath.Dir(filename), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(source), 0644); err != nil {
			t.Fatal(err)
		}
	}

	var stateLock sync.Mutex
	dependencyParseCount := 0
	g, err := loadWithConfig(packages.Config{
		Dir: sdkDirectory,
		ParseFile: func(fileSet *token.FileSet, filename string, source []byte) (*ast.File, error) {
			if filepath.Base(filename) == "dependency.go" {
				func() {
					stateLock.Lock()
					defer stateLock.Unlock()
					dependencyParseCount++
				}()
			}
			return parser.ParseFile(fileSet, filename, source, parser.AllErrors|parser.ParseComments)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	parsedDependencies := func() int {
		stateLock.Lock()
		defer stateLock.Unlock()
		return dependencyParseCount
	}()
	if parsedDependencies != 0 {
		t.Fatalf("generator parsed dependency implementation %d times; exported type metadata must suffice", parsedDependencies)
	}
	if g.pkg.Path() != sdkPath || g.scope != g.pkg.Scope() || !types.Identical(g.errorType, types.Universe.Lookup("error").Type()) {
		t.Fatal("loaded package, scope or universe error type changed")
	}

	namedType := func(name string) *types.Named {
		t.Helper()
		object, ok := g.scope.Lookup(name).(*types.TypeName)
		if !ok {
			t.Fatalf("missing exported type %s", name)
		}
		named, ok := types.Unalias(object.Type()).(*types.Named)
		if !ok {
			t.Fatalf("exported type %s is not named", name)
		}
		return named
	}
	state := namedType("State")
	stateAlias, ok := g.scope.Lookup("StateAlias").(*types.TypeName)
	if !ok || !stateAlias.IsAlias() || !types.Identical(stateAlias.Type(), state) {
		t.Fatal("exported alias lost its identity")
	}
	generic := namedType("GenericState")
	if generic.TypeParams().Len() != 1 || !types.Identical(generic.Underlying().(*types.Struct).Field(0).Type(), generic.TypeParams().At(0)) {
		t.Fatal("exported generic parameter was lost")
	}
	stateFields := state.Underlying().(*types.Struct)
	record, ok := types.Unalias(stateFields.Field(0).Type()).(*types.Named)
	if !ok || record.TypeArgs().Len() != 1 || !types.Identical(record.TypeArgs().At(0), types.Typ[types.String]) {
		t.Fatal("dependency generic instance lost its string argument")
	}
	if !types.Identical(stateFields.Field(1).Type(), record) {
		t.Fatal("dependency alias lost its instantiated identity")
	}
	embedded := record.Underlying().(*types.Struct).Field(0)
	if !embedded.Embedded() || embedded.Exported() {
		t.Fatal("private embedded field was lost")
	}
	privateRecord, ok := types.Unalias(embedded.Type()).(*types.Named)
	if !ok || privateRecord.Obj().Name() != "privateRecord" || !types.Identical(privateRecord.Underlying().(*types.Struct).Field(0).Type(), types.Typ[types.String]) {
		t.Fatal("private embedded generic type was not instantiated")
	}
	getMethod := types.NewMethodSet(types.NewPointer(record)).Lookup(nil, "Get")
	if getMethod == nil || !types.Identical(getMethod.Obj().Type().(*types.Signature).Results().At(0).Type(), types.Typ[types.String]) {
		t.Fatal("dependency generic method set changed")
	}
	if g.deviceType == nil || !types.Implements(types.NewPointer(state), g.deviceType.Underlying().(*types.Interface)) {
		t.Fatal("root method set no longer implements the loaded Device interface")
	}
	save, ok := g.scope.Lookup("SaveState").(*types.Func)
	if !ok || !errorIdResult(save.Type().(*types.Signature)) {
		t.Fatal("named error-id result was lost")
	}
	saveSignature := save.Type().(*types.Signature)
	if saveSignature.Params().At(0).Name() != "state" || !types.Identical(saveSignature.Params().At(0).Type(), types.NewPointer(state)) {
		t.Fatal("exported parameter name or type changed")
	}
}
