package urmessage

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// ══════════════════════════════════════════════════════════════════════════════════════════════
// THE CENSUS ENGINE THE KEY-MATERIAL GATES IN THIS PACKAGE SHARE
// ══════════════════════════════════════════════════════════════════════════════════════════════
//
// WHY THIS FILE EXISTS, AND IT IS THE SAME FINDING TWICE. This package now holds three dataflow
// censuses over three different kinds of key material -- the epoch keys, the device's X-Wing seed,
// and `pq_secret` -- and the first two were written by COPYING the predicate out of the one before
// it. The wrap-seed gate's own header says so in as many words: "its scar is copied here rather
// than relearned". A third hand-copy is a third chance to copy a scar WRONG, and a scar copied
// wrong is a gate that passes by asking a narrower question than the one it prints.
//
// SO THE PREDICATE IS ONE FUNCTION AND THE NETS ARE THREE. [censusBorneBy] takes the producer net
// as an argument instead of reading a package-level map, which is the whole of the change; every
// clause inside it, and every scar below, is the wrap-seed gate's verbatim.
//
// THE THREE SCARS, each of which was a gate defeated in this repository and none of which is
// hypothetical:
//
//   - A CENSUS MUST MATCH THE VALUE AND NOT THE FIELD NAME. A nested type that spelled the epoch
//     keys `wk`/`rk` walked past a gate keyed on strings.
//   - EVERY LANDING PLACE IS ASKED FOR AN EXPRESSION AND NEVER FOR AN *ast.Ident. `x = writeKey[:]`
//     was censused as NOTHING AT ALL -- not refused, absent -- because the sink clause read a bare
//     identifier and a slice expression is not one.
//   - EVERY BINDING FORM THE LANGUAGE HAS. `var leaked = writeKey` bound nothing while the
//     fixpoint read *ast.AssignStmt alone, so the taint stopped at the first `var`.
//
// AND ONE PROPERTY OF THE ENGINE ITSELF, which is why [censusHold] refuses an EMPTY census: a
// search that has stopped reading its own subject finds nothing, refuses nothing, and reports
// success. Every gate below is held both ways against a written-down disposition for the same
// reason -- a site with no entry is a value nobody weighed, and an entry with no site is the gate
// having gone blind.

// censusHold is the both-directions assertion every census in this package shares.
func censusHold(t *testing.T, what string, found map[string][]string,
	disposition map[string]string, why string) {

	t.Helper()
	if len(found) == 0 {
		t.Errorf("this gate found no %s at all in %d production files. An empty census passes every "+
			"refusal below by having nothing to refuse, which is how a search that has stopped "+
			"reading its own subject reports success.", what, len(stateTestProductionSources(t)))
	}
	for site := range found {
		reason, dispositioned := disposition[site]
		if !dispositioned {
			t.Errorf("%s %q has no entry in the disposition.\n%s", what, site, why)
			continue
		}
		t.Logf("    %s %s is allowed: %s", what, site, reason)
	}
	for site := range disposition {
		if _, ok := found[site]; !ok {
			t.Errorf("the disposition says %s %q is allowed and the census does not find it.\n%s",
				what, site, why)
		}
	}
}

// censusBorneBy is THE ONE QUESTION a census asks of an expression: which values from `producers`
// does it bear? It answers the tainted identifiers, the producer calls and the producer field
// reads anywhere inside the expression, spelled the way the source spells them, and the taint step
// and every sink clause run it.
//
// ONE PREDICATE ASKED IN EVERY POSITION IS THE POINT. The defect the epoch-key gate took at
// 4fde7ad was two positions asking a narrower question than the taint step did, so a value the
// walk was following landed somewhere the walk did not count.
//
// `producers` IS A PARAMETER AND USED TO BE A PACKAGE-LEVEL MAP. That is the only difference
// between this function and the one it was lifted from; it is what lets the three gates share the
// three scars instead of each copying them.
func censusBorneBy(expression ast.Expr, tainted map[string]bool, intoLiterals bool,
	local map[string]bool, producers map[string]bool) []string {

	if expression == nil {
		return nil
	}
	borne := []string{}
	seen := map[string]bool{}
	carry := func(spelled string) {
		if !seen[spelled] {
			seen[spelled] = true
			borne = append(borne, spelled)
		}
	}
	recurse := func(inner ast.Expr) {
		for _, spelled := range censusBorneBy(inner, tainted, intoLiterals, local, producers) {
			carry(spelled)
		}
	}
	ast.Inspect(expression, func(node ast.Node) bool {
		switch shape := node.(type) {
		case *ast.CallExpr:
			if censusIsBuiltinLen(shape, local) {
				return false
			}
			named := censusCalleeName(shape.Fun)
			if named != "" && producers[named] {
				carry(censusExpr(shape.Fun))
				// the callee is the producer's own name; only its arguments are values
				for _, argument := range shape.Args {
					recurse(argument)
				}
				return false
			}
		case *ast.SelectorExpr:
			if producers[shape.Sel.Name] {
				carry(censusExpr(shape))
			}
			recurse(shape.X)
			return false
		case *ast.KeyValueExpr:
			if _, named := shape.Key.(*ast.Ident); named {
				recurse(shape.Value)
				return false
			}
		case *ast.CompositeLit:
			if !intoLiterals {
				return false
			}
		case *ast.FuncLit:
			if !intoLiterals {
				return false
			}
		case *ast.Ident:
			if tainted[shape.Name] {
				carry(shape.Name)
			}
		}
		return true
	})
	sort.Strings(borne)
	return borne
}

// censusIsBuiltinLen answers whether this call is the builtin `len` and not some local of that
// name. A function that declared `len` would otherwise turn the one exclusion of [censusBorneBy]
// into a hole, from inside the source the gate inspects.
func censusIsBuiltinLen(call *ast.CallExpr, local map[string]bool) bool {
	identifier, bare := call.Fun.(*ast.Ident)
	return bare && identifier.Name == "len" && !local["len"] && len(call.Args) == 1
}

// censusCalleeName is the last name of a callee, bare or qualified: `deviceIdentity` and
// `xwing.Seed` both answer their own final name. A net that read selectors alone could not see a
// package-local helper called by its bare name.
func censusCalleeName(callee ast.Expr) string {
	switch shape := callee.(type) {
	case *ast.Ident:
		return shape.Name
	case *ast.SelectorExpr:
		return shape.Sel.Name
	case *ast.ParenExpr:
		return censusCalleeName(shape.X)
	}
	return ""
}

// censusRootName is the identifier an expression is rooted at, or "" for an expression with no
// single root -- a call, a literal, an arithmetic. It is what the producer-function seeding binds:
// `parts[3]` is rooted at `parts` and `leafKeys.ExtensionData` at `leafKeys`. A SELECTOR whose
// field is itself in the producer net has no root here, because the field read is a producer in
// its own right and seeding its receiver would taint the whole of `self`.
func censusRootName(expression ast.Expr, producers map[string]bool) string {
	switch shape := expression.(type) {
	case *ast.Ident:
		return shape.Name
	case *ast.ParenExpr:
		return censusRootName(shape.X, producers)
	case *ast.IndexExpr:
		return censusRootName(shape.X, producers)
	case *ast.SliceExpr:
		return censusRootName(shape.X, producers)
	case *ast.StarExpr:
		return censusRootName(shape.X, producers)
	case *ast.UnaryExpr:
		return censusRootName(shape.X, producers)
	case *ast.SelectorExpr:
		if producers[shape.Sel.Name] {
			return ""
		}
		return censusRootName(shape.X, producers)
	}
	return ""
}

// censusExpr spells an expression the way the source does, so a census entry reads as the code
// reads. It is [epochKeyExpr] with the INDEX kept -- `parts[3]` rather than `parts` -- because a
// store's read path spells a key as one part of a generic record, and a census that printed
// `parts` four times over would name the same site for four different values.
func censusExpr(expression ast.Expr) string {
	if index, ok := expression.(*ast.IndexExpr); ok {
		return censusExpr(index.X) + "[" + censusExpr(index.Index) + "]"
	}
	return epochKeyExpr(expression)
}

// censusImportNames is the identifiers a FILE qualifies a package by, so that `fmt.Errorf` and
// `messagegroup.XwingKeyGenFromSeed` are told apart from `temp.Write` by reading the file's
// imports rather than by guessing at a spelling. An aliased import answers its alias; anything
// else answers the last element of its path, which is what the language resolves the qualifier to.
func censusImportNames(parsed *ast.File) map[string]bool {
	names := map[string]bool{}
	for _, one := range parsed.Imports {
		if one.Name != nil {
			names[one.Name.Name] = true
			continue
		}
		path := strings.Trim(one.Path.Value, "\"")
		if at := strings.LastIndex(path, "/"); 0 <= at {
			path = path[at+1:]
		}
		names[path] = true
	}
	return names
}

// censusReseedingFunctions is every function this package DECLARES whose own parameters or named
// results carry a name in `producers` -- which is to say, every function a taint walk starts again
// inside rather than stopping at.
//
// IT IS READ OFF THE SIGNATURES AND NOT LISTED, because a list would be a second spelling of the
// producer net and would drift from it silently.
func censusReseedingFunctions(t *testing.T, producers map[string]bool) map[string]bool {
	t.Helper()
	reseeds := map[string]bool{}
	fileSet := token.NewFileSet()
	for _, name := range stateTestProductionSources(t) {
		parsed, err := parser.ParseFile(fileSet, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			for _, fields := range []*ast.FieldList{function.Type.Params, function.Type.Results} {
				if fields == nil {
					continue
				}
				for _, field := range fields.List {
					for _, target := range field.Names {
						if producers[target.Name] {
							reseeds[function.Name.Name] = true
						}
					}
				}
			}
		}
	}
	if len(reseeds) == 0 {
		t.Fatal("no production function in this package re-seeds this walk, so a horizon census " +
			"would call every call into this package's own store a stopping place")
	}
	return reseeds
}

// censusLocalNames is every name a function declares: its receiver, its parameters, its named
// results, and every `:=`, `var` and `range` binding in its body.
//
// TWO CLAUSES NEED IT AND THEY NEED IT FOR OPPOSITE REASONS. A bare name on the left of an
// assignment is a REBINDING when this function declared it and a SINK when it did not, because a
// name that outlives the call parks a key for the lifetime of whatever holds it. And the `len`
// exclusion asks this map whether the builtin has been SHADOWED, because a local named `len` would
// turn one clause of every gate off from inside the source it inspects.
func censusLocalNames(function *ast.FuncDecl) (map[string]bool, string) {
	local := map[string]bool{"_": true}
	receiver := ""
	declare := func(fields *ast.FieldList) {
		if fields == nil {
			return
		}
		for _, field := range fields.List {
			for _, target := range field.Names {
				local[target.Name] = true
			}
		}
	}
	declare(function.Recv)
	if function.Recv != nil && 0 < len(function.Recv.List) &&
		0 < len(function.Recv.List[0].Names) {
		receiver = function.Recv.List[0].Names[0].Name
	}
	declare(function.Type.Params)
	declare(function.Type.Results)
	ast.Inspect(function.Body, func(node ast.Node) bool {
		switch shape := node.(type) {
		case *ast.AssignStmt:
			if shape.Tok == token.DEFINE {
				for _, target := range shape.Lhs {
					if identifier, ok := target.(*ast.Ident); ok {
						local[identifier.Name] = true
					}
				}
			}
		case *ast.ValueSpec:
			for _, target := range shape.Names {
				local[target.Name] = true
			}
		case *ast.RangeStmt:
			if shape.Tok == token.DEFINE {
				for _, target := range []ast.Expr{shape.Key, shape.Value} {
					if identifier, ok := target.(*ast.Ident); ok {
						local[identifier.Name] = true
					}
				}
			}
		case *ast.FuncLit:
			declare(shape.Type.Params)
			declare(shape.Type.Results)
		}
		return true
	})
	return local, receiver
}

// censusResult is everything one run of [runCensus] saw, in the five shapes the gates assert
// against: where the value is BORN, where it LANDS and what it carries there, what the one
// narrowing REMOVED, where the walk STOPS KNOWING, and which tainted names reached nothing at all.
type censusResult struct {
	sources []string

	// "<enclosing function>|<site>" -> the file:line list, for each of the five censuses.
	producers   map[string][]string
	sinks       map[string][]string
	counted     map[string][]string
	accumulated map[string][]string

	// per sink site, the VALUE SPELLINGS that landed there. A disposition keyed on the site alone
	// excuses the site FOREVER, whatever turns up there later, so every gate holds this too.
	carried map[string]map[string]bool

	// tainted names that reached no sink in their own function, printed as the honest complement:
	// excluded is not the same as absent.
	unspent map[string][]string
}

// runCensus is the taint walk the three key-material gates in this package share.
//
// WHAT IT IS AND WHAT IT IS NOT. It is a four-producer, fixpoint, expression-level walk over every
// production source in this package, and it decides NOTHING: it collects five censuses and hands
// them back, and every refusal is the caller's, held against that gate's own written-down
// disposition. A gate is its NET and its DISPOSITIONS; this function is the machinery under them.
//
// WHY IT IS ONE FUNCTION. The first two gates were written by copying the predicate out of the one
// before -- see [censusBorneBy] for the three scars that copy carries -- and the third would have
// been a third copy. Three copies of a walk is three chances to weaken one of them silently, and
// the wrap-seed gate's own mutation table is what proves this walk still kills what it killed:
// rows M5 (the spelling), M6 (the binding form) and M3/M9 (the narrowing, from both sides) were
// re-run against THIS function and still fail.
//
// THE EXAMPLES IN THE COMMENTS BELOW ARE THE SEED GATE'S, because that is the gate this body was
// lifted from and a generic example would say less. They are illustrations of the CLAUSE, never a
// statement that the seed's net is the only one.
func runCensus(t *testing.T, net map[string]bool) censusResult {
	t.Helper()
	producers := map[string][]string{}
	sinks := map[string][]string{}
	carried := map[string]map[string]bool{}
	counted := map[string][]string{}
	accumulated := map[string][]string{}
	unspent := map[string][]string{}

	// the functions the walk STARTS AGAIN inside, read off their own signatures rather than
	// listed: this is what tells the horizon census below that `self.writeRecord(…, wrapSeed)` is
	// not a place the walk stops.
	reseeds := censusReseedingFunctions(t, net)

	sources := stateTestProductionSources(t)
	for _, name := range sources {
		content, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		fileSet := token.NewFileSet()
		parsed, err := parser.ParseFile(fileSet, name, content, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		imported := censusImportNames(parsed)
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			where := function.Name.Name
			at := func(node ast.Node) string {
				return fmt.Sprintf("%s:%d", name, fileSet.Position(node.Pos()).Line)
			}

			// THE NAMES THIS FUNCTION DECLARED. Two clauses need them. A bare name on the left of
			// an assignment is a REBINDING when this function declared it and a SINK when it did
			// not, because a name that outlives the call parks a key for the lifetime of whatever
			// holds it. And the `len` exclusion below asks this map whether the builtin has been
			// shadowed, because a local named `len` would turn one clause of this gate off.
			local, receiver := censusLocalNames(function)
			bears := func(expression ast.Expr, tainted map[string]bool, intoLiterals bool) []string {
				return censusBorneBy(expression, tainted, intoLiterals, local, net)
			}

			tainted := map[string]bool{}

			// PRODUCER ONE: a PARAMETER or named result carrying a producer's name. This is the
			// store's write path and nothing else -- `PutDeviceIdentity(…, wrapSeed []byte)` --
			// and without it statestore_durable.go is outside this census entirely.
			seedParameters := func(fields *ast.FieldList) {
				if fields == nil {
					return
				}
				for _, field := range fields.List {
					for _, target := range field.Names {
						if !net[target.Name] {
							continue
						}
						site := where + "|parameter " + target.Name
						producers[site] = append(producers[site], at(target))
						tainted[target.Name] = true
					}
				}
			}
			seedParameters(function.Type.Params)
			seedParameters(function.Type.Results)

			// PRODUCER TWO: the body of a producer-named function. Inside `GetDeviceIdentity` the
			// seed is `parts[3]`, a generic record part off a generic record read, and no call or
			// field read in that function would taint anything at all. So each RESULT EXPRESSION
			// of a producer-named function seeds the taint at its own root name. Calls and
			// literals in result position seed nothing -- `fmt.Errorf(...)` is an error and
			// `mls.SignaturePrivateKey(priv)` already carries `priv` by the walk below -- and the
			// RECEIVER is never a root, so `self.wrapSeed` seeds the field read's clause and not
			// the whole of `self`.
			if net[where] {
				ast.Inspect(function.Body, func(node ast.Node) bool {
					statement, ok := node.(*ast.ReturnStmt)
					if !ok {
						return true
					}
					for _, result := range statement.Results {
						root := censusRootName(result, net)
						if root == "" || root == "_" || root == "err" || root == "nil" ||
							root == receiver || !local[root] {
							continue
						}
						site := where + "|result " + root
						producers[site] = append(producers[site], at(result))
						tainted[root] = true
					}
					return true
				})
			}

			// PRODUCER THREE: a CALL whose callee's final name is in the net, spelled bare
			// (`deviceIdentity(...)`) or qualified (`xwing.Seed()`, `store.GetDeviceIdentity()`).
			// The epoch-key gate's net reads selectors alone; the seed's own package helper is
			// called by its bare name, and a net that could not see it would have left
			// [NewDevice]'s struct literal -- the field this whole step added -- uncensused.
			callee := map[ast.Node]bool{}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				named := censusCalleeName(call.Fun)
				if named == "" || !net[named] {
					return true
				}
				callee[call.Fun] = true
				site := where + "|" + censusExpr(call.Fun)
				producers[site] = append(producers[site], at(call))
				return true
			})

			// PRODUCER FOUR: a bare FIELD READ, `self.wrapSeed`, which is how the one field this
			// step added is spelled everywhere it is used. Not counted twice when the same
			// selector was already spent as a callee.
			ast.Inspect(function.Body, func(node ast.Node) bool {
				selector, ok := node.(*ast.SelectorExpr)
				if !ok || callee[selector] || !net[selector.Sel.Name] {
					return true
				}
				site := where + "|" + censusExpr(selector)
				producers[site] = append(producers[site], at(selector)+" field")
				return true
			})

			// THE FIXPOINT, over every binding form the language has. It runs to a fixpoint rather
			// than once because a seed reaches its sink through as many hops as the source cares
			// to take. `var x = wrapSeed` and `for _, b := range wrapSeed` bind here exactly as
			// `x := wrapSeed` does: the epoch-key gate was defeated by the first of those three
			// spellings and its scar is copied here rather than relearned.
			for spin := 0; spin < 16; spin += 1 {
				grew := false
				bind := func(targets []ast.Expr, values []ast.Expr) {
					carries := false
					for _, value := range values {
						if 0 < len(bears(value, tainted, true)) {
							carries = true
						}
					}
					if !carries {
						return
					}
					for _, target := range targets {
						identifier, ok := target.(*ast.Ident)
						if !ok || identifier.Name == "_" || identifier.Name == "err" {
							continue
						}
						if !tainted[identifier.Name] {
							tainted[identifier.Name] = true
							grew = true
						}
					}
				}
				ast.Inspect(function.Body, func(node ast.Node) bool {
					switch shape := node.(type) {
					case *ast.AssignStmt:
						bind(shape.Lhs, shape.Rhs)
					case *ast.ValueSpec:
						declared := []ast.Expr{}
						for _, target := range shape.Names {
							declared = append(declared, target)
						}
						bind(declared, shape.Values)
					case *ast.RangeStmt:
						bind([]ast.Expr{shape.Key, shape.Value}, []ast.Expr{shape.X})
					}
					return true
				})
				if !grew {
					break
				}
			}

			// THE NARROWING'S CENSUS, taken BEFORE the sinks and independently of them: every
			// `len(...)` of a seed-bearing expression. [censusBorneBy] will decline to look
			// through these, so this is the only place they are seen, and they are ASSERTED
			// against a written-down list rather than dropped.
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || !censusIsBuiltinLen(call, local) {
					return true
				}
				for _, argument := range call.Args {
					borne := bears(argument, tainted, true)
					if len(borne) == 0 {
						continue
					}
					site := where + "|len " + censusExpr(argument)
					counted[site] = append(counted[site], at(call)+" "+fmt.Sprint(borne))
				}
				return true
			})

			// THE HORIZON CENSUS, taken with the narrowing's own census and for the same reason:
			// every call at which a seed-bearing value is handed to a METHOD ON A VALUE this walk
			// does not follow. It is where the census ends, and where it ends is asserted below
			// against each gate's own accumulator disposition rather than left to be inferred from the
			// absence of entries.
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || censusIsBuiltinLen(call, local) {
					return true
				}
				selector, method := call.Fun.(*ast.SelectorExpr)
				if !method {
					return true // a bare call has no receiver to accumulate into
				}
				root := censusRootName(selector.X, net)
				switch {
				case root == "":
					return true // a receiver with no single root -- a literal, a call
				case imported[root]:
					return true // a package-qualified call is not a method on a value
				case tainted[root]:
					return true // a receiver the walk already follows is a sink, not a horizon
				case reseeds[selector.Sel.Name]:
					return true // the walk starts again inside it; see [censusReseedingFunctions]
				}
				borne := []string{}
				for _, argument := range call.Args {
					borne = append(borne, bears(argument, tainted, false)...)
				}
				if len(borne) == 0 {
					return true
				}
				site := where + "|accumulate " + censusExpr(selector)
				accumulated[site] = append(accumulated[site], at(call)+" "+fmt.Sprint(borne))
				return true
			})

			if len(tainted) == 0 && len(producers) == 0 {
				continue
			}

			// THE SINKS. Every clause asks an EXPRESSION and not a node class, which is the
			// epoch-key gate's second scar: `x = wrapSeed[:]`, `(wrapSeed)`, `[]byte(wrapSeed)`
			// and `any(wrapSeed).([]byte)` are ONE site rather than four holes.
			landed := map[string]bool{}
			record := func(site string, node ast.Node, expression ast.Expr, borne []string) {
				sinks[site] = append(sinks[site], at(node)+" "+censusExpr(expression))
				if carried[site] == nil {
					carried[site] = map[string]bool{}
				}
				for _, spelled := range borne {
					carried[site][spelled] = true
					if tainted[spelled] {
						landed[spelled] = true
					}
				}
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				switch shape := node.(type) {
				case *ast.CompositeLit:
					spelled := censusExpr(shape.Type)
					for index, element := range shape.Elts {
						value := element
						field := fmt.Sprintf("element %d", index)
						if pair, ok := element.(*ast.KeyValueExpr); ok {
							value = pair.Value
							field = censusExpr(pair.Key)
						}
						borne := bears(value, tainted, false)
						if len(borne) == 0 {
							continue
						}
						record(fmt.Sprintf("%s|literal %s.%s", where, spelled, field),
							element, value, borne)
					}
				case *ast.CallExpr:
					// the builtin `len` is not a sink: it is the narrowing, and it has a census
					// of its own two clauses up.
					if censusIsBuiltinLen(shape, local) {
						return true
					}
					for _, argument := range shape.Args {
						borne := bears(argument, tainted, false)
						if len(borne) == 0 {
							continue
						}
						record(fmt.Sprintf("%s|call %s", where, censusExpr(shape.Fun)),
							shape, argument, borne)
					}
				case *ast.AssignStmt:
					for index, target := range shape.Lhs {
						if identifier, bare := target.(*ast.Ident); bare && local[identifier.Name] {
							continue
						}
						var source ast.Expr
						switch {
						case len(shape.Lhs) == len(shape.Rhs):
							source = shape.Rhs[index]
						case len(shape.Rhs) == 1:
							source = shape.Rhs[0]
						default:
							continue
						}
						borne := bears(source, tainted, false)
						if len(borne) == 0 {
							continue
						}
						record(fmt.Sprintf("%s|assign %s", where, censusExpr(target)),
							shape, source, borne)
					}
				case *ast.ReturnStmt:
					for _, result := range shape.Results {
						borne := bears(result, tainted, false)
						if len(borne) == 0 {
							continue
						}
						record(where+"|return", shape, result, borne)
					}
				case *ast.SendStmt:
					borne := bears(shape.Value, tainted, false)
					if 0 < len(borne) {
						record(fmt.Sprintf("%s|send %s", where, censusExpr(shape.Chan)),
							shape, shape.Value, borne)
					}
				}
				return true
			})
			for identifier := range tainted {
				if !landed[identifier] {
					unspent[where] = append(unspent[where], identifier)
				}
			}
		}
	}

	return censusResult{
		sources:     sources,
		producers:   producers,
		sinks:       sinks,
		carried:     carried,
		counted:     counted,
		accumulated: accumulated,
		unspent:     unspent,
	}
}
