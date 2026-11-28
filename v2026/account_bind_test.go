package sdk

// The account features' mobile surface (the email change, Sign in with
// Microsoft and the payout wallet disconnect) binds whole through gobind.
// gobind skips a field whose type it cannot bind, and the Android build's
// mobileexports check rejects every skip it has no policy for, which fails
// the nightly's AAR step. MicrosoftSignInSettings.AttemptTimeout, a
// time.Duration, did that; the settings are now off the bound api.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// The files of the account features' bound api.
var accountBindFiles = []string{
	"api_email_change.go",
	"api_sn_wallet_disconnect.go",
	"oauth_microsoft.go",
}

// Whether gobind binds a field of this type: a basic or package type, a
// pointer to a package type, or []byte. Another package's type (time.Duration,
// time.Time, context.Context), a map, a channel, a func or another slice is
// skipped.
func accountBindableFieldType(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.Ident:
		return true
	case *ast.StarExpr:
		_, ok := e.X.(*ast.Ident)
		return ok
	case *ast.ArrayType:
		element, ok := e.Elt.(*ast.Ident)
		return e.Len == nil && ok && element.Name == "byte"
	default:
		return false
	}
}

func TestAccountBoundStructsHaveOnlyBindableFields(t *testing.T) {
	files := token.NewFileSet()
	for _, name := range accountBindFiles {
		file, err := parser.ParseFile(files, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			spec, ok := node.(*ast.TypeSpec)
			if !ok || !spec.Name.IsExported() {
				return true
			}
			structType, ok := spec.Type.(*ast.StructType)
			if !ok {
				return true
			}
			for _, field := range structType.Fields.List {
				for _, fieldName := range field.Names {
					if fieldName.IsExported() && !accountBindableFieldType(field.Type) {
						t.Errorf("%s: %s.%s has a type gobind skips", files.Position(field.Pos()), spec.Name.Name, fieldName.Name)
					}
				}
			}
			return true
		})
	}
}

// The rule itself: what it refuses is what gobind skips.
func TestAccountBindableFieldTypeRule(t *testing.T) {
	cases := map[string]bool{
		"string":         true,
		"int64":          true,
		"*SnWallet":      true,
		"[]byte":         true,
		"time.Duration":  false,
		"time.Time":      false,
		"[]string":       false,
		"map[string]int": false,
		"func()":         false,
		"*time.Time":     false,
	}
	for source, want := range cases {
		expr, err := parser.ParseExpr(source)
		if err != nil {
			t.Fatal(err)
		}
		if got := accountBindableFieldType(expr); got != want {
			t.Errorf("%s: bindable %t, want %t", source, got, want)
		}
	}
}
