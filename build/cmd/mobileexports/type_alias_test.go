// Keeps the gomobile surface clear of the type alias uses that gobind
// mishandles. Go 1.27 removed GODEBUG gotypesalias, so gobind always sees
// go/types aliases, and the pinned x/mobile bind generators still treat an
// alias differently from the type it names in three places
// (https://github.com/golang/go/issues/71827). The sdk spells the basic type in
// those places, which keeps the generated bindings the same whether or not a
// toolchain materializes aliases.
package main

import (
	"fmt"
	"go/types"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// gobind skips a const whose type is an alias, because its const generators
// require a *types.Basic. A struct field whose type aliases a bool or numeric
// type gets a Java equals that compares the primitive with null, and an
// interface method that takes or returns an alias of a basic type gets a Go
// proxy that names the alias unqualified; neither compiles. Each view is a
// package view that gomobile binds.
func TestMobileApiAvoidsAliasesGobindMishandles(t *testing.T) {
	views := []struct {
		goos string
		tags string
	}{
		{goos: "android", tags: "sdk_mobile_bind"},
		// build_apple's macos targets select the same files as ios
		{goos: "ios", tags: "sdk_mobile_bind"},
		{goos: "ios", tags: "sdk_mobile_bind,ios_extension"},
	}
	violationLines := map[string]bool{}
	for _, view := range views {
		sdkPackage := testingLoadMobileView(t, view.goos, view.tags)
		report := func(object types.Object, format string, arguments ...any) {
			position := sdkPackage.Fset.Position(object.Pos())
			location := fmt.Sprintf("%s:%d: ", filepath.Base(position.Filename), position.Line)
			violationLines[location+fmt.Sprintf(format, arguments...)] = true
		}
		// an alias of a basic type, and that basic type
		basicAlias := func(candidateType types.Type) (*types.Alias, *types.Basic, bool) {
			alias, ok := candidateType.(*types.Alias)
			if !ok {
				return nil, nil, false
			}
			basic, ok := types.Unalias(alias).(*types.Basic)
			return alias, basic, ok
		}
		scope := sdkPackage.Types.Scope()
		declaresAlias := false
		materializesAlias := false
		for _, name := range scope.Names() {
			object := scope.Lookup(name)
			if typeName, ok := object.(*types.TypeName); ok && typeName.IsAlias() {
				declaresAlias = true
				if _, ok := typeName.Type().(*types.Alias); ok {
					materializesAlias = true
				}
			}
			if !object.Exported() {
				continue
			}
			switch object := object.(type) {
			case *types.Const:
				if alias, ok := object.Type().(*types.Alias); ok {
					report(object, "const %s has alias type %s, which gobind skips; declare it %s",
						object.Name(), alias.Obj().Name(), types.Unalias(alias))
				}
			case *types.TypeName:
				named, ok := object.Type().(*types.Named)
				if !ok {
					continue
				}
				switch underlying := named.Underlying().(type) {
				case *types.Struct:
					for index := 0; index < underlying.NumFields(); index++ {
						field := underlying.Field(index)
						if !field.Exported() {
							continue
						}
						alias, basic, ok := basicAlias(field.Type())
						if ok && basic.Info()&(types.IsBoolean|types.IsNumeric) != 0 {
							report(field, "field %s.%s has alias type %s, whose Java equals would compare a primitive with null; declare it %s",
								object.Name(), field.Name(), alias.Obj().Name(), basic)
						}
					}
				case *types.Interface:
					for index := 0; index < underlying.NumMethods(); index++ {
						method := underlying.Method(index)
						if !method.Exported() {
							continue
						}
						signature := method.Type().(*types.Signature)
						for _, tuple := range []*types.Tuple{signature.Params(), signature.Results()} {
							for variableIndex := 0; variableIndex < tuple.Len(); variableIndex++ {
								if alias, basic, ok := basicAlias(tuple.At(variableIndex).Type()); ok {
									report(method, "method %s.%s uses alias type %s, which the generated Go proxy names unqualified; spell %s",
										object.Name(), method.Name(), alias.Obj().Name(), basic)
								}
							}
						}
					}
				}
			}
		}
		// GODEBUG gotypesalias=0 (go 1.26 and earlier) hides every alias from this check
		if declaresAlias && !materializesAlias {
			t.Fatalf("go/types resolved the %s %s view's type aliases away; run without GODEBUG gotypesalias=0", view.goos, view.tags)
		}
	}
	if 0 < len(violationLines) {
		lines := slices.Sorted(maps.Keys(violationLines))
		t.Errorf("%d mobile api declarations use type aliases that gobind mishandles:\n%s", len(lines), strings.Join(lines, "\n"))
	}
}
