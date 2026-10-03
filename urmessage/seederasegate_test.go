package urmessage

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"

	"github.com/urnetwork/connect/messagegroup"
	"github.com/urnetwork/connect/mls"
	"github.com/urnetwork/sdk"
)

// ══════════════════════════════════════════════════════════════════════════════════════════════
// EVERY PATH THAT DROPS THE DEVICE ERASES ITS X-WING SEED, AND NOT THE THREE THAT WERE FOUND
// ══════════════════════════════════════════════════════════════════════════════════════════════
//
// WHAT WAS WRONG, and it is three returns. S2-26 gave [Device] a field holding a private key and
// gave [Device.Close] an erase for it -- and the seed is LIVE before there is a Device to close.
// `deviceIdentity` mints it three statements before it hands it back, [NewDevice] holds it for the
// length of one engine construction, and on every error return in between the array was simply
// dropped. AT `sdk 5319536`, WHICH IS WHERE THESE LINE NUMBERS READ (they have moved since, by the
// length of the comments this repair wrote beside them):
//
//	device.go:343  NewDevice, the engine refused          return nil, fmt.Errorf("…the mls engine…")
//	device.go:415  deviceIdentity, the extension refused  return nil, nil, nil, nil, fmt.Errorf(…)
//	device.go:419  deviceIdentity, the store refused      return nil, nil, nil, nil, fmt.Errorf(…)
//
// This package's rule is that a field holding a private key with no erase on a drop path is a
// defect, and [Device.Close]'s own header states it -- *"a field holding key material that no path
// clears is what `connect/mls`'s erase gate refuses one package over."* Three paths cleared
// nothing. NO TEST WENT RED FOR ANY OF THEM:
// TestClosingADeviceErasesTheSeedAndNotOnlyTheField drives `Close` and nothing else, and the whole
// package answered `ok 6.096s` on the commit this file repairs.
//
// WHAT THE REPAIR IS, and it is deliberately not three erases. Each of the three live ranges gets
// ONE deferred erase, registered at the binding that makes the seed live and disarmed by a flag at
// the single exit that hands the seed on -- `writeRecord`'s own `committed := false` idiom, one
// file over, for the same reason it is there: a cleanup that must happen on every exit BUT one is
// a claim about the exits nobody has written yet, and a `defer` is the only form of that claim the
// language checks. Three explicit `zeroizeState(wrapSeed)` calls would have fixed exactly the
// three returns above and said nothing about the fourth.
//
// AND THAT IS WHY THIS GATE ASSERTS THE PROPERTY AND NOT THE THREE SITES. It reads the production
// source, finds every local that ALIASES the seed's array, and for each one requires that every
// `return` inside that local's own scope either HANDS THE SEED ON or is covered by a deferred
// erase registered between the binding and the return. A fourth error return added to either
// function tomorrow is therefore not silent: it is either inside the defer's reach, in which case
// it is already correct, or it is a drop with no cover and this gate names it. There is no list of
// three returns anywhere in this file, which is the point.
//
// WHAT THE GATE CANNOT SEE, said here rather than left to be discovered. It checks the SHAPE of
// the cover -- that a defer erasing that name is registered in front of the return -- and it
// cannot evaluate the flag that arms it, so `defer func() { if false { zeroizeState(seed) } }()`
// would satisfy it. That is what the runtime half below is for: it FORCES two of the drop returns
// with a store of its own and reads the actual octets of the actual array afterwards, so the shape
// is measured to fire and not only to exist. The two halves are joined by an assertion rather than
// by this paragraph: each runtime case names the binding site it drives and that name is held
// against [wrapSeedLiveBindings].
//
// THE ONE DROP THIS SUITE CANNOT DRIVE is `deviceIdentity`'s leaf-keys arm. `Encode` has exactly
// two refusals -- `AlgId != AlgIdXwing` and `len(DeviceXwingPub) != XwingPublicKeyLen`
// (`connect/mls/extension.go:672-678` at connect 96e6b461) -- and `deviceIdentity` writes the
// first as a constant and
// takes the second from `xwing.Public().Bytes()`, so NO VALUE A CALLER CONTROLS REACHES EITHER.
// Its third arm is a `marshalBytes` failure over an in-memory writer, which no case can provoke
// from outside the package either. The bridge to that return is therefore asserted rather than
// assumed: this gate requires a binding's drop returns to be covered by EXACTLY ONE defer, so the
// defer the runtime case below proves fires is the same one -- the only one -- standing in front
// of the arm no case can reach. That is a narrower claim than "it is tested" and it is the one the
// measurement supports.
//
// AND THE CENSUS FINDS FIVE DROPS, NOT THREE, which is worth saying because the finding named
// three. The other two are `NewDevice`'s own `return nil, err` when `deviceIdentity` refused, and
// the restore arm's return when the store refused for a reason other than absence: on both of
// those the seed is nil today, so the erase covering them is a no-op. They are covered anyway,
// because the cover is registered at the BINDING rather than at the returns somebody enumerated,
// and a structural claim that had to be re-argued per return is the claim this file exists to
// replace.

// The producers this gate starts at, and WHICH OF THEIR RESULTS is the seed. It is a position and
// not a name, which is the difference between this net and [wrapSeedProducerNames] next door: that
// one over-approximates on purpose, because a leak census may not miss a spelling; this one asks
// WHOSE ARRAY IS THIS, and the signature private key returned beside the seed by the same call is
// a different array with a different owner ([messagegroup.NewConnectMlsEngine] copies it and
// `connect/mls`'s own erase gates hold that copy).
//
//   - `Seed` is `xwing.Seed()`, the mint. It answers a COPY, which is what makes the array the
//     caller's to erase; a gate over a borrowed array would be asserting someone else's duty.
//   - `GetDeviceIdentity` is the store's read, whose fourth result is the seed as the disk last
//     held it. The record parts it answers are freshly decoded and referenced by nothing else,
//     which is what makes erasing them correct and is why [DeviceStore] says the read "is never
//     five nils" rather than "may alias".
//   - `deviceIdentity` is the package helper whose fourth result is that same value one call out.
var wrapSeedLiveProducerResult = map[string]int{
	"Seed":              0,
	"GetDeviceIdentity": 3,
	"deviceIdentity":    3,
}

// Every place in this package's production source where the seed becomes a LOCAL this function is
// then responsible for, as "<enclosing function>|<name> from <producer as written>" -> why.
//
// HELD BOTH WAYS, and the stale direction is the one that matters most here. Every clause below
// searches for returns "in scope of a binding"; if the binding is not found, nothing is in scope
// of it, no drop is reported and this gate passes by having looked at nothing. An entry with no
// site is exactly that state, and it is the only thing standing between this file and a silent
// green.
var wrapSeedLiveBindings = map[string]string{
	"NewDevice|wrapSeed from deviceIdentity": "the seed in the hands of the one road into " +
		"[Device], live from the moment `deviceIdentity` answers until the struct literal that " +
		"holds it. Everything in between -- today, one engine construction -- can fail.",
	"deviceIdentity|wrapSeed from store.GetDeviceIdentity": "the RESTORE arm. The store answers " +
		"an array freshly decoded off the disk and referenced by nothing else, so it is this " +
		"call's to erase; on the one drop below this binding the store refused and the array is " +
		"empty, which is why the erase there is a no-op TODAY and is registered anyway.",
	"deviceIdentity|wrapSeed from xwing.Seed": "the MINT arm, and the live range the finding was " +
		"about: the seed exists for two more statements -- the leaf keys encoding and the write -- " +
		"before it is handed back, and both of them can refuse.",
}

// THE FIRST NARROWING'S COMPLEMENT: every parameter and named result in this package that carries
// the seed, which this gate declines to call a binding.
//
// A PARAMETER IS SOMEBODY ELSE'S ARRAY AND ERASING IT WOULD BE A BUG, not a fix. `PutDeviceIdentity`
// receives the very array [NewDevice] is about to hold; a store that erased it on the way out would
// leave the device with 32 zero octets, which is a WELL FORMED X-Wing seed that decapsulates to a
// uniform-looking wrong secret -- the exact hazard [Device.Close] nils the field to avoid. The same
// holds for the record `parts` under it. So the narrowing is real and this is its census: held both
// ways, because a parameter that stops carrying the seed and an entry nobody deleted look identical
// from inside a green run.
//
// IT IS COMPUTED FROM [wrapSeedProducerNames], the neighbouring gate's net, rather than from a
// second list -- so a rename that makes one of these disappear is reported by both files at once.
var wrapSeedNotThisFunctionsToErase = map[string]string{
	"PutDeviceIdentity|parameter wrapSeed": "the store's write path. The array is the CALLER's and " +
		"is the one the device goes on to hold; the store copies its octets into the record it " +
		"frames and erases that copy ([DurableStateStore.writeRecord]'s deferred zeroizeState), " +
		"which is the erase that belongs to this function and is censused as a sink next door.",
	"writeRecord|parameter parts":       "the same array one call further in, now one of four record parts.",
	"encodeStateRecord|parameter parts": "and one call further still, at the framing.",
	"groupRecordOf|parameter parts": "THE GROUP RECORD'S DECODE, WHICH HOLDS NO SEED. It is in " +
		"this census only because the net above seeds any parameter called `parts`, and the ownership " +
		"answer is the same one: the six parts are the CALLER's, read off the disk by " +
		"[DurableStateStore.GroupRecords] and handed on to a [GroupRecord] that outlives this call.",
}

// THE SECOND NARROWING'S COMPLEMENT: every local bound from a value DERIVED from the seed rather
// than ALIASING it, which this gate does not follow.
//
// `private, err := messagegroup.XwingKeyGenFromSeed(self.wrapSeed)` is the whole of it today, and
// it is a limit with a name: `messagegroup.XwingPrivateKey` declares no erase, this package cannot
// reach inside one, and `connect/mls`'s erase gate excuses the type on the written ground that no
// production declaration holds one in a field -- which is a sentence
// TestNoFieldInThisPackageHoldsAnUnerasableXwingPrivateKey keeps true HERE and which the seed is
// held rather than the key in order not to break. So the expanded pair is a copy of this device's
// private key that lives until the collector takes it, on every [Device.DecapsulateToOwnLeaf], and
// nothing in this repository can clear it.
//
// THAT IS A REAL RESIDUAL AND IT IS WRITTEN DOWN HERE BECAUSE IT IS WHERE IT WOULD BE MISSED: a
// reader who has just watched this gate refuse three drops should not conclude the seed's every
// copy is erased. It is not. The fix is `connect`'s and is item 243's, not this step's.
var wrapSeedDerivedNotAliasedSites = map[string]string{
	"DecapsulateToOwnLeaf|private = messagegroup.XwingKeyGenFromSeed": "the seed expanded into " +
		"the key pair it is the seed OF. A derived value and not an alias: erasing `private` is " +
		"not something this package can do, and following it would make this gate demand an erase " +
		"that has nowhere to land.",
	"openWrapToOwnLeaf|private = messagegroup.XwingKeyGenFromSeed": "THE SAME EXPANSION, one " +
		"layer out, for the door that opens a device wrap rather than a bare encapsulation " +
		"(ledger item 243's receive leg). It is a second site and not a shared one because the " +
		"two calls answer different things -- a shared secret and a wrap's payload -- and the " +
		"residual is the same one, unchanged and still `connect`'s: `*messagegroup.XwingPrivateKey` " +
		"declares no erase, so the expanded copy of the seed inside it cannot be dropped from here.",
}

func TestEveryPathThatDropsTheDeviceErasesItsWrapSeed(t *testing.T) {
	bindings := map[string][]string{}
	parameters := map[string][]string{}
	derived := map[string][]string{}
	dropped := []string{}
	handedOn := 0
	fieldErasures := 0

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

		// THE CONTROL ON THE ERASE'S OWN SPELLING. Every clause below looks for a call to
		// `zeroizeState` over the seed; if this package renamed its one erase, the defers would
		// stop being found and every drop would be reported as uncovered -- which is loud -- but
		// the FIELD erase in [Device.Close] would go quiet in exactly the same way and nothing
		// would say so. This counts it, and the assertion is at the foot of the test.
		ast.Inspect(parsed, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || censusCalleeName(call.Fun) != "zeroizeState" {
				return true
			}
			for _, argument := range call.Args {
				if selector, field := argument.(*ast.SelectorExpr); field &&
					selector.Sel.Name == "wrapSeed" {
					fieldErasures += 1
				}
			}
			return true
		})

		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			where := function.Name.Name
			at := func(node ast.Node) string {
				return fmt.Sprintf("%s:%d", name, fileSet.Position(node.Pos()).Line)
			}

			// THE FIRST NARROWING'S CENSUS: parameters and named results carrying the seed, which
			// are NOT bindings because the array belongs to the caller.
			for _, fields := range []*ast.FieldList{function.Type.Params, function.Type.Results} {
				if fields == nil {
					continue
				}
				for _, field := range fields.List {
					for _, target := range field.Names {
						if !wrapSeedProducerNames[target.Name] {
							continue
						}
						site := where + "|parameter " + target.Name
						parameters[site] = append(parameters[site], at(target))
					}
				}
			}

			// THE BLOCKS, so that a binding's scope is the block it was declared in and not the
			// rest of the file. `deviceIdentity` declares TWO locals called `wrapSeed` -- the
			// restore arm's inside `if durable {…}` and the mint arm's at body level -- and a gate
			// that read positions alone would put the mint arm's returns in scope of the restore
			// arm's binding and demand an erase from a name that is not in scope there.
			//
			// AND THE FUNCTION LITERALS, because a `return` inside one is that literal's exit and
			// not this function's -- which matters exactly here, since the repair's cover IS a
			// function literal.
			blocks := []*ast.BlockStmt{}
			literals := [][2]token.Pos{}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				switch shape := node.(type) {
				case *ast.BlockStmt:
					blocks = append(blocks, shape)
				case *ast.FuncLit:
					literals = append(literals, [2]token.Pos{shape.Pos(), shape.End()})
				}
				return true
			})

			// THE BINDINGS, to a fixpoint over every binding form, so that `s := wrapSeed` and
			// `var s = wrapSeed` make `s` this function's to erase as surely as the producer call
			// did. Nothing in the tree today takes that second hop; the clause is here because the
			// epoch-key gate was defeated once by exactly the binding form its fixpoint did not
			// read, and relearning that costs more than copying it.
			type liveSeed struct {
				name  string
				site  string
				from  token.Pos
				until token.Pos
			}
			live := []liveSeed{}
			liveNames := map[string]bool{}
			bound := map[string]bool{}
			for spin := 0; spin < 8; spin += 1 {
				grew := false
				consider := func(node ast.Node, targets []ast.Expr, values []ast.Expr) {
					hold := func(target ast.Expr, from string) {
						identifier, bare := target.(*ast.Ident)
						if !bare || identifier.Name == "_" || identifier.Name == "err" {
							return
						}
						site := where + "|" + identifier.Name + " from " + from
						if bound[site] {
							return
						}
						bound[site] = true
						grew = true
						liveNames[identifier.Name] = true
						bindings[site] = append(bindings[site], at(node))
						live = append(live, liveSeed{
							name:  identifier.Name,
							site:  site,
							from:  node.Pos(),
							until: wrapSeedScopeEnd(blocks, node.Pos()),
						})
					}
					if len(values) == 1 {
						if index, producer := wrapSeedProducerResultIndex(values[0]); producer {
							if index < len(targets) {
								call := values[0].(*ast.CallExpr)
								hold(targets[index], censusExpr(call.Fun))
							}
							return
						}
					}
					if len(targets) != len(values) {
						return
					}
					for index := range values {
						if position, producer := wrapSeedProducerResultIndex(values[index]); producer {
							if position == 0 {
								call := values[index].(*ast.CallExpr)
								hold(targets[index], censusExpr(call.Fun))
							}
							continue
						}
						if wrapSeedAliasedBy(values[index], liveNames) {
							hold(targets[index], censusExpr(values[index]))
						}
					}
				}
				ast.Inspect(function.Body, func(node ast.Node) bool {
					switch shape := node.(type) {
					case *ast.AssignStmt:
						consider(shape, shape.Lhs, shape.Rhs)
					case *ast.ValueSpec:
						declared := []ast.Expr{}
						for _, target := range shape.Names {
							declared = append(declared, target)
						}
						consider(shape, declared, shape.Values)
					}
					return true
				})
				if !grew {
					break
				}
			}

			// THE SECOND NARROWING'S CENSUS, taken after the fixpoint and independently of it:
			// every local bound from a call that was HANDED the seed but is not a producer of it.
			// Those are derivations, this gate does not follow them, and every one is named.
			ast.Inspect(function.Body, func(node ast.Node) bool {
				var targets, values []ast.Expr
				switch shape := node.(type) {
				case *ast.AssignStmt:
					targets, values = shape.Lhs, shape.Rhs
				case *ast.ValueSpec:
					for _, target := range shape.Names {
						targets = append(targets, target)
					}
					values = shape.Values
				default:
					return true
				}
				for _, value := range values {
					call, isCall := value.(*ast.CallExpr)
					if !isCall {
						continue
					}
					if _, producer := wrapSeedProducerResultIndex(call); producer {
						continue
					}
					if wrapSeedAliasedBy(value, liveNames) {
						continue // a conversion or a parenthesis: an alias, not a derivation
					}
					carries := false
					for _, argument := range call.Args {
						if wrapSeedAliasedBy(argument, liveNames) {
							carries = true
						}
					}
					if !carries {
						continue
					}
					for _, target := range targets {
						identifier, bare := target.(*ast.Ident)
						if !bare || identifier.Name == "_" || identifier.Name == "err" {
							continue
						}
						site := where + "|" + identifier.Name + " = " + censusExpr(call.Fun)
						derived[site] = append(derived[site], at(node))
					}
				}
				return true
			})

			// ── THE PROPERTY ──────────────────────────────────────────────────────────────────
			for _, binding := range live {
				erasers := wrapSeedErasingDefers(function.Body, binding.name)
				covers := map[token.Pos]bool{}
				drops := 0
				ast.Inspect(function.Body, func(node ast.Node) bool {
					statement, ok := node.(*ast.ReturnStmt)
					if !ok {
						return true
					}
					if statement.Pos() <= binding.from || binding.until <= statement.Pos() {
						return true // before the seed was live, or out of its scope
					}
					for _, literal := range literals {
						if literal[0] < statement.Pos() && statement.Pos() < literal[1] {
							return true // a function literal's own exit, not this function's
						}
					}
					for _, result := range statement.Results {
						if wrapSeedMentions(result, binding.name) {
							handedOn += 1
							t.Logf("    %s HANDS THE SEED ON at %s -- not a drop",
								binding.site, at(statement))
							return true
						}
					}
					drops += 1
					dropped = append(dropped, binding.site+" at "+at(statement))
					covered := false
					for _, eraser := range erasers {
						if binding.from < eraser && eraser < statement.Pos() {
							covers[eraser] = true
							covered = true
						}
					}
					if !covered {
						t.Errorf("%s is DROPPED at %s and nothing erases it.\n"+
							"A return inside a seed-bearing local's own scope either hands the seed "+
							"on or is a private key going out of scope with its octets intact, and "+
							"this package's rule is that the second is a defect. The cover this gate "+
							"accepts is a `defer` that calls zeroizeState over that name, registered "+
							"between the binding and this return -- deliberately NOT an erase "+
							"written in front of one return, because that is a claim about one path "+
							"and the next path added would be silent. See "+
							"[Device.Close] for the same erase at the other end of the device's life.",
							binding.site, at(statement))
						return true
					}
					t.Logf("    %s is dropped at %s and a deferred erase covers it",
						binding.site, at(statement))
					return true
				})
				if 0 < drops && len(covers) != 1 {
					t.Errorf("%s has %d drop return(s) covered by %d distinct deferred erases, want "+
						"exactly one.\nTHE BRIDGE TO THE ARM NO CASE CAN DRIVE RESTS ON THIS: "+
						"TestForcingADropPathErasesTheSeedTheDeviceWasHanded forces one of these "+
						"returns and measures the octets afterwards, and that measurement carries to "+
						"the others only while they are covered by the SAME defer. Two covers means "+
						"one of them is unmeasured; none means the check above is reporting nothing.",
						binding.site, drops, len(covers))
				}
			}
		}
	}

	// ── THE COMPLEMENT, PRINTED ───────────────────────────────────────────────────────────────
	t.Logf("production sources read (%d)", len(sources))
	t.Logf("seed-bearing bindings found (%d):", len(bindings))
	for _, site := range epochKeySortedMap(bindings) {
		t.Logf("    %s  at %v", site, bindings[site])
	}
	t.Logf("DROP RETURNS found (%d) -- a return inside a binding's scope that does not hand the "+
		"seed on; every one is asserted covered above: %v", len(dropped), dropped)
	t.Logf("HANDED ON (%d) -- the returns that carry the seed out and are therefore not drops", handedOn)
	t.Logf("NOT THIS FUNCTION'S TO ERASE (%d) -- the parameters the first narrowing removed:", len(parameters))
	for _, site := range epochKeySortedMap(parameters) {
		t.Logf("    %s  at %v", site, parameters[site])
	}
	t.Logf("DERIVED AND NOT ALIASED (%d) -- the values the second narrowing removed:", len(derived))
	for _, site := range epochKeySortedMap(derived) {
		t.Logf("    %s  at %v", site, derived[site])
	}

	// ── AND ASSERTED, IN BOTH DIRECTIONS ──────────────────────────────────────────────────────
	censusHold(t, "seed-bearing binding", bindings, wrapSeedLiveBindings,
		"A BINDING IS WHERE THIS GATE'S WHOLE SEARCH BEGINS: every drop it refuses is a return "+
			"found IN SCOPE OF ONE. A site with no entry is a live range nobody weighed. An entry "+
			"with no site is worse -- it is this gate having gone blind, because nothing is in "+
			"scope of a binding that was not found, no drop is reported, and the refusal above "+
			"passes by having nothing to refuse.")
	censusHold(t, "not-this-function's-to-erase site", parameters, wrapSeedNotThisFunctionsToErase,
		"A PARAMETER CARRYING THE SEED IS THE CALLER'S ARRAY, and erasing it would hand the device "+
			"32 zero octets -- a well formed seed that decapsulates to a uniform-looking wrong "+
			"secret. That is why this gate declines to treat a parameter as a binding, and this is "+
			"the census of every site the decision removed. A site with no entry is a new parameter "+
			"carrying key material that nobody has decided the ownership of.")
	censusHold(t, "derived-not-aliased site", derived, wrapSeedDerivedNotAliasedSites,
		"A VALUE DERIVED FROM THE SEED IS NOT THE SEED'S ARRAY, and this gate follows aliases only. "+
			"Each site it removed is a copy of key material this package cannot erase, and naming "+
			"them is what stops this file being read as 'every copy of the seed is erased' -- which "+
			"is not true and is not what it measures.")

	if fieldErasures == 0 {
		t.Error("no production site in this package calls zeroizeState over a `.wrapSeed` field, so " +
			"[Device.Close]'s own erase is gone -- or this package's erase has been renamed, in " +
			"which case every `defer` this gate looks for has stopped being found and its covers " +
			"are being reported for a spelling nothing uses.")
	}
	if len(dropped) == 0 {
		t.Error("this gate found no drop return at all. Either every exit in scope of a seed " +
			"binding now hands the seed on -- which would be a real change worth reading the " +
			"dispositions above against -- or the scope arithmetic has stopped matching returns, " +
			"in which case the refusal is vacuous.")
	}
	if handedOn == 0 {
		t.Error("this gate found no return that hands the seed on, so every exit looks like a drop " +
			"and the distinction the refusal rests on is not being drawn.")
	}
}

// wrapSeedProducerResultIndex answers WHICH result of a call is the seed, for the calls in
// [wrapSeedLiveProducerResult]. A position rather than a name, because `deviceIdentity` answers
// five values and only one of them is the array this gate is about.
func wrapSeedProducerResultIndex(expression ast.Expr) (int, bool) {
	call, ok := expression.(*ast.CallExpr)
	if !ok {
		return 0, false
	}
	index, producer := wrapSeedLiveProducerResult[censusCalleeName(call.Fun)]
	return index, producer
}

// wrapSeedAliasedBy answers whether an expression is the seed's own array under another spelling:
// the six the epoch-key gate's scar names, minus the ones that would make it a different array.
// `wrapSeed`, `(wrapSeed)`, `wrapSeed[:]`, `[]byte(wrapSeed)` and `any(x).([]byte)` all point at
// the same octets; `append([]byte(nil), wrapSeed...)` does not and is a derivation.
//
// A SELECTOR SPELLED `.wrapSeed` IS THE FIELD, whatever its receiver, which is how [Device.Close]
// and [Device.DecapsulateToOwnLeaf] spell it and is the only form the field is ever read in.
func wrapSeedAliasedBy(expression ast.Expr, live map[string]bool) bool {
	switch shape := expression.(type) {
	case *ast.Ident:
		return live[shape.Name]
	case *ast.SelectorExpr:
		return shape.Sel.Name == "wrapSeed"
	case *ast.ParenExpr:
		return wrapSeedAliasedBy(shape.X, live)
	case *ast.SliceExpr:
		return wrapSeedAliasedBy(shape.X, live)
	case *ast.TypeAssertExpr:
		return wrapSeedAliasedBy(shape.X, live)
	case *ast.CallExpr:
		// `[]byte(x)` is a conversion and not a call: the octets do not move.
		if _, conversion := shape.Fun.(*ast.ArrayType); conversion && len(shape.Args) == 1 {
			return wrapSeedAliasedBy(shape.Args[0], live)
		}
	}
	return false
}

// wrapSeedMentions answers whether a returned expression carries a name ANYWHERE inside it, which
// is the question "does the seed leave by this return" and is deliberately wider than
// [wrapSeedAliasedBy]: [NewDevice] hands the seed on inside a struct literal, so a predicate that
// only read root expressions would have called that return a drop and demanded an erase of the
// value the device is built out of.
//
// A FIELD NAME IN A LITERAL IS NOT A READ. `Device{wrapSeed: wrapSeed}` names a field on the left
// and reads a value on the right, and counting the key would make the spelling of a field enough
// to satisfy this gate.
func wrapSeedMentions(expression ast.Expr, name string) bool {
	found := false
	var walk func(node ast.Node)
	walk = func(node ast.Node) {
		if node == nil || found {
			return
		}
		ast.Inspect(node, func(inner ast.Node) bool {
			if found {
				return false
			}
			switch shape := inner.(type) {
			case *ast.KeyValueExpr:
				if _, named := shape.Key.(*ast.Ident); named {
					walk(shape.Value)
					return false
				}
			case *ast.Ident:
				if shape.Name == name {
					found = true
				}
			}
			return true
		})
	}
	walk(expression)
	return found
}

// wrapSeedScopeEnd is where the block a binding was declared in closes, which is where that name
// stops existing. See the comment at its one call site for the shadowing case it is there for.
func wrapSeedScopeEnd(blocks []*ast.BlockStmt, pos token.Pos) token.Pos {
	innermost := token.NoPos
	end := token.NoPos
	for _, block := range blocks {
		if block.Pos() <= pos && pos < block.End() {
			if innermost == token.NoPos || innermost < block.Pos() {
				innermost = block.Pos()
				end = block.End()
			}
		}
	}
	return end
}

// wrapSeedErasingDefers is every `defer` in a function whose call, anywhere inside it, erases the
// named local. It matches the deferred closure the repair uses and a bare `defer zeroizeState(x)`
// equally, because both are claims about every exit below them; what it does NOT match is an erase
// written in front of one return, which is a claim about one path.
func wrapSeedErasingDefers(body *ast.BlockStmt, name string) []token.Pos {
	registered := []token.Pos{}
	live := map[string]bool{name: true}
	ast.Inspect(body, func(node ast.Node) bool {
		statement, ok := node.(*ast.DeferStmt)
		if !ok {
			return true
		}
		erases := false
		ast.Inspect(statement.Call, func(inner ast.Node) bool {
			call, isCall := inner.(*ast.CallExpr)
			if !isCall || censusCalleeName(call.Fun) != "zeroizeState" {
				return true
			}
			for _, argument := range call.Args {
				if wrapSeedAliasedBy(argument, live) {
					erases = true
				}
			}
			return true
		})
		if erases {
			registered = append(registered, statement.Pos())
		}
		return true
	})
	return registered
}

// ══════════════════════════════════════════════════════════════════════════════════════════════
// THE RUNTIME HALF: THE SHAPE ABOVE IS MEASURED TO FIRE
// ══════════════════════════════════════════════════════════════════════════════════════════════

// wrapSeedFaultStore is a [DeviceStore] whose two identity methods are this case's own, and it
// exists for one reason that no simpler shape gives: THE ARRAY.
//
// A case can force an error return easily enough. What it cannot ordinarily do is look at the
// seed's octets AFTER the call that dropped them has returned -- `xwing.Seed()` answers an array
// inside `deviceIdentity` and nothing outside can reach it. Both arms here hand this case an array
// it allocated or was handed by reference: `GetDeviceIdentity` answers one this case still holds,
// and `PutDeviceIdentity`'s fourth parameter ALIASES the caller's, which is precisely the aliasing
// [wrapSeedNotThisFunctionsToErase] says a store must not erase -- and is what makes the erase on
// the caller's side observable from here.
type wrapSeedFaultStore struct {
	DeviceStore
	get func() ([]byte, []byte, []byte, []byte, error)
	put func(signerPub []byte, signerPriv []byte, leafKeys []byte, wrapSeed []byte) error
}

func (self *wrapSeedFaultStore) GetDeviceIdentity() ([]byte, []byte, []byte, []byte, error) {
	return self.get()
}

func (self *wrapSeedFaultStore) PutDeviceIdentity(signerPub []byte, signerPriv []byte,
	leafKeys []byte, wrapSeed []byte) error {

	return self.put(signerPub, signerPriv, leafKeys, wrapSeed)
}

func wrapSeedAllZero(octets []byte) (int, bool) {
	for at, octet := range octets {
		if octet != 0 {
			return at, false
		}
	}
	return 0, true
}

// FORCING A DROP PATH ERASES THE SEED, MEASURED ON THE ARRAY AND NOT ON THE FIELD.
//
// Two of the three drop returns the finding named are driven here; the third, `deviceIdentity`'s
// leaf-keys arm, cannot be reached from outside (see this file's header) and is carried by the
// one-defer assertion above rather than by a case pretending to drive it.
//
// EVERY CLAUSE HAS ITS CONTROL IN THE SAME CASE, and the control is not "did it fail" -- it is the
// SUCCESS path over the identical construction, asserting the seed is still THERE. Without it,
// every assertion below is satisfied by a build that erases the seed unconditionally, which would
// hand every device 32 zero octets and is a strictly worse defect than the one this file repairs.
func TestForcingADropPathErasesTheSeedTheDeviceWasHanded(t *testing.T) {
	for _, drives := range []string{
		"NewDevice|wrapSeed from deviceIdentity",
		"deviceIdentity|wrapSeed from xwing.Seed",
	} {
		if _, known := wrapSeedLiveBindings[drives]; !known {
			t.Fatalf("this case says it drives the binding %q and that is not a site "+
				"TestEveryPathThatDropsTheDeviceErasesItsWrapSeed holds. The two halves of this "+
				"file are joined by these names: the source half proves every drop of a binding is "+
				"covered by one defer, and this half proves that defer fires.", drives)
		}
	}

	// ── (1) NewDevice's ENGINE failure, over a store whose seed array this case still holds ───
	//
	// The leaf keys body is one octet of nothing, which `mls.ParseLeafKeysExtension` refuses
	// inside `messagegroup.NewConnectMlsEngine` -- the one failure of that constructor a caller
	// can reach through this package -- so the seed is live and the device is not built.
	seed := bytes.Repeat([]byte{0x5E}, messagegroup.XwingSeedSize)
	refused := &wrapSeedFaultStore{
		DeviceStore: openTestStore(t, t.TempDir()),
		get: func() ([]byte, []byte, []byte, []byte, error) {
			return testPub, testPriv, []byte{0x00}, seed, nil
		},
	}
	if _, err := wrapSeedFaultDevice(t, refused); !errors.Is(err, messagegroup.ErrEngineLeafKeys) {
		t.Fatalf("(1) NewDevice answered %v, want the engine's leaf keys refusal; this case is not "+
			"driving the return it says it drives", err)
	}
	if at, zero := wrapSeedAllZero(seed); !zero {
		t.Errorf("(1) octet %d of the seed survived NewDevice's engine failure. The device was not "+
			"built, so nothing will ever Close it, and the private half of the key this leaf "+
			"publishes is still in this process's heap with nothing left pointing at it.", at)
	}

	// the control, and it fires for its own reason: the identical construction that SUCCEEDS must
	// leave the seed intact, because the device it builds is the thing that holds it.
	leafKeys, kept := mintLeafKeys(t)
	accepted := &wrapSeedFaultStore{
		DeviceStore: openTestStore(t, t.TempDir()),
		get: func() ([]byte, []byte, []byte, []byte, error) {
			return testPub, testPriv, leafKeys, kept, nil
		},
	}
	device, err := wrapSeedFaultDevice(t, accepted)
	if err != nil {
		t.Fatalf("(1) the control: NewDevice over a store the engine accepts answered %v", err)
	}
	if _, zero := wrapSeedAllZero(kept); zero {
		t.Fatal("(1) the control: NewDevice erased the seed it KEPT, so the assertion above is " +
			"satisfied by a build that erases unconditionally -- which hands every device 32 zero " +
			"octets, a well formed X-Wing seed that decapsulates to a wrong secret")
	}
	if err := device.Close(); err != nil {
		t.Errorf("(1) the control: Close answered %v", err)
	}

	// ── (2) deviceIdentity's MINT arm, at the write the store refuses ─────────────────────────
	//
	// The seed here is `xwing.Seed()`'s own array, which no case can name -- so the store keeps
	// the alias its fourth parameter is handed, and that is the array read afterwards.
	crypto, err := mls.NewCryptoProvider(deviceCipherSuite)
	if err != nil {
		t.Fatalf("the mls crypto provider: %v", err)
	}
	disk := errors.New("this disk will not take an identity")
	handed := []byte(nil)
	minting := &wrapSeedFaultStore{
		DeviceStore: openTestStore(t, t.TempDir()),
		get: func() ([]byte, []byte, []byte, []byte, error) {
			return nil, nil, nil, nil, fmt.Errorf("%w: nothing written here yet", ErrNoDeviceIdentity)
		},
		put: func(signerPub []byte, signerPriv []byte, leafKeys []byte, wrapSeed []byte) error {
			handed = wrapSeed
			return disk
		},
	}
	if _, _, _, _, err := deviceIdentity(crypto, minting, rand.Reader); !errors.Is(err, disk) {
		t.Fatalf("(2) deviceIdentity answered %v, want the store's refusal; this case is not "+
			"driving the return it says it drives", err)
	}
	if len(handed) != messagegroup.XwingSeedSize {
		t.Fatalf("(2) the store was handed a %d octet seed, so the mint arm was not reached and "+
			"there is no array to read", len(handed))
	}
	if at, zero := wrapSeedAllZero(handed); !zero {
		t.Errorf("(2) octet %d of the seed survived deviceIdentity's failed write. It was minted in "+
			"this call, it never reached a Device, and nothing else references it.", at)
	}

	// the control: the same mint over a store that ACCEPTS the write must keep the seed, because
	// the array the store was handed is the array the device goes on to hold.
	written := []byte(nil)
	accepting := &wrapSeedFaultStore{
		DeviceStore: openTestStore(t, t.TempDir()),
		get: func() ([]byte, []byte, []byte, []byte, error) {
			return nil, nil, nil, nil, fmt.Errorf("%w: nothing written here yet", ErrNoDeviceIdentity)
		},
		put: func(signerPub []byte, signerPriv []byte, leafKeys []byte, wrapSeed []byte) error {
			written = wrapSeed
			return nil
		},
	}
	_, _, mintedKeys, mintedSeed, err := deviceIdentity(crypto, accepting, rand.Reader)
	if err != nil {
		t.Fatalf("(2) the control: deviceIdentity over a store that accepts answered %v", err)
	}
	if _, zero := wrapSeedAllZero(written); zero {
		t.Fatal("(2) the control: a successful mint erased the seed it handed back, so the " +
			"assertion above is satisfied by an unconditional erase")
	}
	if !bytes.Equal(written, mintedSeed) {
		t.Error("(2) the control: the array the store was handed is not the one deviceIdentity " +
			"answered, so the measurement above is reading a copy and not the seed")
	}

	// and the seed that came back is still the seed of the key that was published, which is what
	// says the erase discipline has not quietly swapped the array under the success path
	published, err := mls.ParseLeafKeysExtension(mintedKeys)
	if err != nil {
		t.Fatalf("(2) the control: parsing the minted leaf keys body: %v", err)
	}
	expanded, err := messagegroup.XwingKeyGenFromSeed(mintedSeed)
	if err != nil {
		t.Fatalf("(2) the control: expanding the minted seed: %v", err)
	}
	if !bytes.Equal(expanded.Public().Bytes(), published.DeviceXwingPub) {
		t.Error("(2) the control: the seed a successful mint answers no longer expands to the key " +
			"its leaf publishes")
	}
}

// ══════════════════════════════════════════════════════════════════════════════════════════════
// THE MUTATION TABLE, MEASURED
// ══════════════════════════════════════════════════════════════════════════════════════════════
//
// SIX MUTANTS, AND THE TABLE'S JOB IS TO SHOW WHICH HALF OF THIS FILE CATCHES WHAT -- because a
// table where every row is killed by both halves proves nothing about either. N1 and N2 remove the
// cover; N3 adds a DROP PATH THAT DID NOT EXIST, which is the property this file claims and the
// one a list of three returns could never have made; N4 erases a COPY instead of the array, which
// is the `Close` scar restated at the other end of the device's life; N5 DISARMS the cover without
// changing its shape, and is the row that justifies the runtime half existing at all; N6 respells
// the producer, and is the row where the gate is meant to report its own blindness rather than a
// defect. A SURVIVING MUTANT IS FIRST A CLAIM ABOUT THE QUERY, so each row names the text it
// produced and WHICH test produced it.
//
//	N1  NewDevice's cover deleted -- the finding's first site restored
//	    source:  "NewDevice|wrapSeed from deviceIdentity is DROPPED at device.go:361 and nothing
//	             erases it", twice, plus "2 drop return(s) covered by 0 distinct deferred erases"
//	    runtime: "(1) octet 0 of the seed survived NewDevice's engine failure"
//	N2  the MINT arm's cover deleted -- the finding's other two sites, which share one defer
//	    source:  the same refusal at both of the mint arm's returns, plus the one-cover clause
//	    runtime: "(2) octet 0 of the seed survived deviceIdentity's failed write"
//	N3  A NEW DROP PATH: `if leafKeys == nil { return nil, ErrNoReserver }` inserted BETWEEN the
//	    binding and the cover, which is a return no line of this file names and no case drives
//	    source:  "NewDevice|wrapSeed from deviceIdentity is DROPPED at device.go:359 and nothing
//	             erases it"
//	    runtime: --- PASS, correctly: no case drives that return, and a suite of hand-written
//	             cases is exactly what would have let this through
//	N4  THE COVER ERASES A COPY: `zeroizeState(append([]byte(nil), wrapSeed...))`. A defer is still
//	    registered and still names the seed; what it clears is a fresh array
//	    source:  both of NewDevice's drops reported uncovered -- an `append` is a derivation and
//	             not an alias, which is the same distinction [wrapSeedDerivedNotAliasedSites] is
//	             the complement of
//	    runtime: "(1) octet 0 of the seed survived NewDevice's engine failure"
//	N5  THE COVER DISARMED: `held := true`. The shape is untouched -- a defer, over the right
//	    array, in front of every return -- and it never fires
//	    source:  --- PASS. THIS IS THE LIMIT THE HEADER STATES, measured rather than asserted:
//	             this gate checks the shape and cannot evaluate the flag
//	    runtime: "(1) octet 0 of the seed survived NewDevice's engine failure" -- which is the
//	             whole reason the runtime half is not redundant with the source half
//	N6  THE PRODUCER RESPELLED: `wrapSeed := append([]byte(nil), xwing.Seed()...)`, which is how a
//	    gate keyed on a producer is AVOIDED rather than attacked
//	    source:  "the disposition says seed-bearing binding "deviceIdentity|wrapSeed from
//	             xwing.Seed" is allowed and the census does not find it" -- the gate reporting that
//	             it has gone blind, which is the only correct answer here
//	    runtime: --- PASS, correctly: the copy IS the array the device holds and erases, so the
//	             code is still right and only the gate's grip on it has slipped
//
// Applied one at a time with a python edit that ASSERTS count == 1, and reverted by a byte copy of
// a snapshot taken before the run whose sha256 was verified after every single revert
// (`d683ccf661d12ed5e0ed48b313c065f218678cfbaa24ecf132e77f8db1faebc2` for device.go). No
// `git checkout --`, no stash. The clean tree was re-run after each row.
//
// WHAT NO ROW HERE DRIVES, and it is the honest gap rather than a row: `deviceIdentity`'s
// leaf-keys arm. No mutant above reaches it and no case can, for the reason the header gives;
// N2 is what covers it, because N2 removes the ONE defer that stands in front of both of the mint
// arm's returns and the one-cover assertion is what says there is only one.

// wrapSeedFaultDevice is [NewDevice] over a caller's state store, with the transport and reserver
// every case in this package needs and none of them is about.
func wrapSeedFaultDevice(t *testing.T, stateStore mls.StateStore) (*Device, error) {
	t.Helper()
	streamStore, err := sdk.OpenStreamStore(t.TempDir())
	if err != nil {
		t.Fatalf("sdk.OpenStreamStore: %v", err)
	}
	t.Cleanup(func() { streamStore.Close() })
	return NewDevice(DeviceConfig{
		Transport:  newSilentTransport(t),
		Reserver:   sdk.NewStreamIndexReserver(streamStore),
		StateStore: stateStore,
	})
}
