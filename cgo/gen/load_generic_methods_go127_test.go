//go:build go1.27

// The current toolchain must preserve mixed generic and ordinary methods in
// export format v5. This fixture does not raise the module's older Go floor.
package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"
)

// Compare with the same toolchain's checker rather than assuming a particular
// public method-index ordering, which go/types deliberately leaves unspecified.
func TestGeneratorPreservesGenericMethodMetadata(t *testing.T) {
	const source = `package sdk

type Box[T any] struct { Value T }
func (self *Box[T]) Zeta() T { return self.Value }
func (self *Box[T]) Map[U any](transform func(T) U) U { return transform(self.Value) }
func (self *Box[T]) Alpha() T { return self.Value }
func (self *Box[T]) Pair[V any](other V) (T, V) { return self.Value, other }
func (self *Box[T]) Middle() T { return self.Value }
`
	config := testingGeneratorImportFixture(t, map[string]string{
		"sdk/go.mod": "module " + sdkPath + "\n\ngo 1.27.0\n",
		"sdk/sdk.go": source,
	})
	g, err := loadWithConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, "fixture.go", source, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	typeConfig := &types.Config{GoVersion: "go1.27"}
	sourcePackage, err := typeConfig.Check(sdkPath, fileSet, []*ast.File{file}, nil)
	if err != nil {
		t.Fatal(err)
	}
	loadedObject, ok := g.scope.Lookup("Box").(*types.TypeName)
	if !ok {
		t.Fatal("exported generic Box type was lost")
	}
	loaded := types.Unalias(loadedObject.Type()).(*types.Named)
	expected := types.Unalias(sourcePackage.Scope().Lookup("Box").Type()).(*types.Named)
	if loaded.NumMethods() != expected.NumMethods() {
		t.Fatalf("method count = %d, want %d", loaded.NumMethods(), expected.NumMethods())
	}
	qualifier := func(pkg *types.Package) string { return pkg.Path() }
	for index := range expected.NumMethods() {
		actualMethod := loaded.Method(index)
		expectedMethod := expected.Method(index)
		actualSignature := actualMethod.Type().(*types.Signature)
		expectedSignature := expectedMethod.Type().(*types.Signature)
		if actualMethod.Name() != expectedMethod.Name() || actualSignature.TypeParams().Len() != expectedSignature.TypeParams().Len() || actualSignature.RecvTypeParams().Len() != expectedSignature.RecvTypeParams().Len() || types.TypeString(actualSignature, qualifier) != types.TypeString(expectedSignature, qualifier) {
			t.Fatalf("method %d differs: export %s, source %s", index, actualMethod, expectedMethod)
		}
	}
	actualInstance, err := types.Instantiate(nil, loaded, []types.Type{types.Typ[types.String]}, true)
	if err != nil {
		t.Fatal(err)
	}
	expectedInstance, err := types.Instantiate(nil, expected, []types.Type{types.Typ[types.String]}, true)
	if err != nil {
		t.Fatal(err)
	}
	actualSet := types.NewMethodSet(types.NewPointer(actualInstance))
	expectedSet := types.NewMethodSet(types.NewPointer(expectedInstance))
	if actualSet.Len() != expectedSet.Len() {
		t.Fatalf("instantiated method-set length = %d, want %d", actualSet.Len(), expectedSet.Len())
	}
	for index := range expectedSet.Len() {
		actualMethod := actualSet.At(index).Obj()
		expectedMethod := expectedSet.At(index).Obj()
		if actualMethod.Name() != expectedMethod.Name() || types.TypeString(actualMethod.Type(), qualifier) != types.TypeString(expectedMethod.Type(), qualifier) {
			t.Fatalf("instantiated method %d differs: export %s, source %s", index, actualMethod, expectedMethod)
		}
	}
}
