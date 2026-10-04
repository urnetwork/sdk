package cp3b

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// EVERY READ OF THE SERVER'S OWN ROWS NAMES ITS EPOCH CEILING.
//
// WHY THIS EXISTS, AND IT IS NOT A STYLE RULE. `store.FetchRequest.ReadEpoch` is ledger item 246's
// epoch ceiling, and `store/store.go` states both halves of the trap in its own words: a row is
// served only when `record.Epoch <= ReadEpoch`, and the field is REQUIRED because "0 cannot mean
// 'no ceiling' the way `ClassMask`'s 0 means 'every class' -- epoch 0 is a real epoch, the
// founding commit sits at it". So `&store.FetchRequest{GroupId: groupId}` is not an unbounded
// read. It is a read bounded at epoch ZERO, and in this package that is the founding commit and
// nothing else.
//
// WHAT IT COST BEFORE IT EXISTED, measured rather than imagined. SIX call sites in this package
// were written that way, all of them before the ceiling landed, and they stayed green:
//
//   - `assertServerCannotRead` -- the clause that makes "the server cannot read your messages" a
//     measurement -- searched a row set that contained no message. Its own control
//     (`len(result.Records) == 0` is a t.Fatal) never fired, because the founding commit IS at
//     epoch 0, so there was always exactly one row to search.
//   - `noIndexIsUsedTwice` asserted an absence over one row.
//   - `observer_test.go`'s row count and `sizeladder_test.go`'s row map read one row each.
//   - `streamIndicesOf` was the only one that FAILED, and only because it indexed `[len-1]` of an
//     empty slice -- which is the general shape: a silent narrowing is found by whatever reads a
//     value back, never by whatever asserts a nothing.
//
// FOUR OF THE SIX WERE ABSENCE ASSERTIONS. An absence asserted over a silently narrowed set is
// the exact failure this corpus has a standing rule about, and no rule in a document would have
// caught it -- the query looked correct, and its narrowing lived in a field it did not mention.
// So the rule is mechanical here: a read that does not NAME its ceiling is refused, whatever it
// would have answered.
//
// THE POSITIVE CONTROL IS INLINE AND IT IS A t.Fatal. A walk that finds no `store.FetchRequest`
// literal at all would report success by reading nothing, which is the same defect one level up.
func TestEveryReadOfTheServersRowsNamesItsEpochCeiling(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("listing this package's sources: %v", err)
	}
	sort.Strings(names)

	found := 0
	withCeiling := []string{}
	for _, name := range names {
		content, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		fileSet := token.NewFileSet()
		parsed, err := parser.ParseFile(fileSet, name, content, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			literal, ok := node.(*ast.CompositeLit)
			if !ok {
				return true
			}
			selector, ok := literal.Type.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "FetchRequest" {
				return true
			}
			pkg, ok := selector.X.(*ast.Ident)
			if !ok || pkg.Name != "store" {
				return true
			}
			found += 1
			at := fileSet.Position(literal.Pos())
			where := name + ":" + itoa(at.Line)

			named := false
			for _, element := range literal.Elts {
				pair, ok := element.(*ast.KeyValueExpr)
				if !ok {
					// a POSITIONAL literal names no field at all, so it cannot name this one
					// either, and it is refused for the same reason rather than skipped.
					continue
				}
				if key, ok := pair.Key.(*ast.Ident); ok && key.Name == "ReadEpoch" {
					named = true
				}
			}
			if !named {
				t.Errorf("%s builds a store.FetchRequest that does not name ReadEpoch, so it is "+
					"a read bounded at EPOCH ZERO and not an unbounded one. In this package that "+
					"is the founding commit and nothing else: every message, every wrap and every "+
					"marker is above it. Use [world.allRows], or name the ceiling here and say "+
					"which epoch it is and why.", where)
				return true
			}
			withCeiling = append(withCeiling, where)
			return true
		})
	}

	// THE CONTROL: this walk really does find these literals. Without it a rename of the store
	// package, a change of the type's name, or a parser that silently returned nothing would make
	// every clause above vacuous and this case would pass by looking at no code at all.
	if found == 0 {
		t.Fatalf("this walk found no store.FetchRequest literal in any of the %d source(s) in "+
			"this package, so it is not reading the code it is supposed to be reading and its "+
			"refusals above are vacuous: %v", len(names), names)
	}
	// CONDITIONAL, for the reason the clause above exists at all: "all naming their ceiling: []"
	// printed beside a failure that says one of them does not is a sentence the measurement
	// contradicts, and it is the line a reader skimming the output would take away.
	if !t.Failed() {
		t.Logf("%d store.FetchRequest literal(s) across %d source(s), all naming their ceiling: %v",
			found, len(names), withCeiling)
	}
}

// itoa avoids pulling strconv in for one call in one gate.
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := []byte{}
	for 0 < value {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
