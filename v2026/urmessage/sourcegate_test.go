package urmessage

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------------------------
// gates over this package's own source
// ----------------------------------------------------------------------------------------------

// EVERY fsync IN THIS PACKAGE IS AT A SITE THIS SUITE NAMES, AND THE VALUE FLUSH IS COUNTED AFTER
// IT RETURNS.
//
// WHY A COUNTER WAS NOT ENOUGH, MEASURED RATHER THAN ARGUED. [DurableStateStore.flushes] was
// introduced with the claim that "the flush cannot be deleted without this number going to zero".
// It is false and it was falsified by deleting the flush:
//
//	was:  syncErr := temp.Sync()
//	made: var syncErr error // the Sync is gone and the counter is not
//	      self.flushes += 1
//
//	go test -count=1 -race ./urmessage        -> ok
//	cd cp3b && go test -count=1 -race ./...   -> ok
//
// The whole suite stayed GREEN with the value fsync deleted, because an UNCONDITIONAL increment
// counts the same whether the call above it is there or not. Moving the increment below the call
// does not repair that; nothing about the position of an unconditional statement makes it a count
// of work performed. THE COUNTER MEASURES THAT A WRITE PASSED THROUGH writeRecord. ONLY THIS GATE
// MEASURES THAT THE FLUSH IS STILL IN IT.
//
// It is sdk/message_stream_store_test.go's TestEveryForcedFlushInTheStoreIsCounted, one package
// over, and that file says why it exists there in the same terms: "a counter above a Sync counts
// the same whether the Sync is there or not, and that is how 'return from Reserve before the
// flush' survived a whole suite once". The discipline was copied into this package one layer deep
// -- the counter came and the gate did not -- and this is the missing layer.
//
// WHAT IT PERMITS AND WHY IT IS TWO SITES AND NOT ONE. The stream store has exactly one because
// it has one kind of flush. This package has two KINDS: the VALUE flush in writeRecord, which is
// what makes a record whole before it is named, and the DIRECTORY flush in syncStateDir, which is
// what makes the rename that names it durable. They are counted differently on purpose --
// [DurableStateStore.flushes] counts only the value flush, because the directory flush is a no-op
// on Windows by construction and a counter that read zero on one platform and one on another would
// measure the platform rather than the code. A THIRD site appearing anywhere in this package is
// this gate's business: it is either a flush nothing counts or a second discipline growing beside
// the first, and both are what this is here to refuse.
func TestEveryFsyncInThisPackageIsAtASiteThisSuiteNames(t *testing.T) {
	sites := map[string]int{}
	for _, name := range stateTestProductionSources(t) {
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
			function, ok := node.(*ast.FuncDecl)
			if !ok {
				return true
			}
			ast.Inspect(function.Body, func(inner ast.Node) bool {
				call, ok := inner.(*ast.CallExpr)
				if !ok {
					return true
				}
				if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "Sync" {
					sites[function.Name.Name] += 1
				}
				return true
			})
			return false
		})
	}
	t.Logf("Sync call sites in this package's production source, by enclosing function: %v", sites)
	if len(sites) != 2 || sites["writeRecord"] != 1 || sites["syncStateDir"] != 1 {
		t.Errorf("this package has %v Sync call sites; there must be exactly two -- one in writeRecord, which is the VALUE flush the flush counter counts, and one in syncStateDir, which is the DIRECTORY flush no counter can portably count. A counter above a Sync counts the same whether the Sync is there or not: deleting writeRecord's Sync and leaving `self.flushes += 1` where it is left this whole suite green, which is what this gate exists to stop",
			sites)
	}

	// and writeRecord counts AFTER the call returns, which is the half the counter owes back.
	const source = "statestore_durable.go"
	content, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read %s: %v", source, err)
	}
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, source, content, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", source, err)
	}
	counted := false
	sawSyncFirst := false
	ast.Inspect(parsed, func(node ast.Node) bool {
		function, ok := node.(*ast.FuncDecl)
		if !ok || function.Name.Name != "writeRecord" {
			return true
		}
		for _, statement := range function.Body.List {
			if stateTestStatementCallsSync(statement) {
				sawSyncFirst = true
			}
			if stateTestStatementAssigns(statement, "flushes") {
				counted = sawSyncFirst
			}
		}
		return false
	})
	if !counted {
		t.Error("writeRecord does not increment flushes after its Sync returns")
	}
}

// stateTestProductionSources is every non-test .go file in this package, WHATEVER GOOS IT IS
// CONSTRAINED TO: this gate reads source rather than compiling it, so the two platform files this
// build excludes are still read. That is the point -- syncStateDir is the unix file's on every
// machine, and a gate that only saw the files this GOOS compiles would answer "one site" on
// Windows and "two" on Linux and would be measuring the platform.
func stateTestProductionSources(t *testing.T) []string {
	t.Helper()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	sources := []string{}
	for _, name := range names {
		if !strings.HasSuffix(name, "_test.go") {
			sources = append(sources, name)
		}
	}
	if len(sources) == 0 {
		t.Fatal("this gate read no production source, so it is holding nothing")
	}
	// the control on the control: the three platform files are the ones a build constraint
	// hides, and a gate that silently stopped reading them would go green by blindness.
	for _, needed := range []string{
		"statestore_durable.go",
		"statestore_platform_unix.go",
		"statestore_platform_windows.go",
		"statestore_platform_other.go",
	} {
		found := false
		for _, name := range sources {
			if name == needed {
				found = true
			}
		}
		if !found {
			t.Fatalf("this gate did not read %s, so it is not holding the file it is about", needed)
		}
	}
	return sources
}

func stateTestStatementAssigns(statement ast.Stmt, field string) bool {
	found := false
	ast.Inspect(statement, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, target := range assign.Lhs {
			if selector, ok := target.(*ast.SelectorExpr); ok && selector.Sel.Name == field {
				found = true
			}
		}
		return true
	})
	return found
}

func stateTestStatementCallsSync(statement ast.Stmt) bool {
	found := false
	ast.Inspect(statement, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "Sync" {
			found = true
		}
		return true
	})
	return found
}
