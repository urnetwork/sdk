package urmessage

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// ══════════════════════════════════════════════════════════════════════════════════════════════
// EVERY ERROR [Group.Receive] ANSWERS COMES OUT OF THE WALK'S OWN COMMIT
// ══════════════════════════════════════════════════════════════════════════════════════════════
//
// THE DEFECT THIS GATE IS SHAPED AGAINST, reproduced against a real server before it was repaired.
// [Group.commitWalkLocked] is the only place [Group.wrapDark] -- ruling 38's sticky diagnosis, the
// whole reason this build can say WHY a member went dark rather than answering an undiagnosable
// refusal -- is consulted on the read path. Five arms of [Group.Receive] called it as a STATEMENT
// and returned their own transport error, so everything it decided was discarded on every one of
// them:
//
//	1st Receive: urmessage: a device wrap addressed to this device did not open …   ErrWrapUnreadable
//	2nd Receive: urmessage: the message server refused this fetch: REASON_REJECTED  ErrFetchRefused
//	3rd Receive: the same.
//
// and that second arm is not incidental: a dark group's read_key is wrong, the server verifies
// req_auth before it reaches any AEAD, so `REASON_REJECTED` is the GUARANTEED arm for a dark group
// from its second fetch onwards. The diagnosis could never reach a caller again.
//
// WHY THE MUTATION TABLE OF THE COMMIT THAT INTRODUCED IT MISSED IT, which is the part worth
// keeping. That table's row M9 deleted the sticky field and watched three cases go red -- through
// the `urmessage` harness, which has NO FETCH. The gate's CLASS did not match the defect's SHAPE:
// the defect was in an arm of [Group.Receive] that throws the return value away, and no test in
// this package can reach an arm that only exists under a transport.
//
// SO THE REPAIR IS TWO THINGS AND THIS FILE IS THE SECOND. The first is in the code: the transport
// error is now a PARAMETER of [Group.commitWalkLocked], which the compiler will not let an arm
// skip. The second is this gate, which refuses the shape rather than the instance -- a sixth arm
// added tomorrow that answers AROUND the walk is red here even though it compiles.
//
// AND THE BEHAVIOUR ITSELF IS MEASURED ELSEWHERE, in two places, because a structural gate cannot
// see an ordering decision: TestTheWalksStickyRefusalsOutrankTheTransportsOwn below drives
// [Group.commitWalkLocked]'s precedence directly, and cp3b's
// TestADarkGroupNamesItsWrapOnEveryLaterReceiveAndNotTheServersRefusal drives the whole loop
// against a real server, which is the only place the refusal arm exists at all.

// The two-result returns [Group.Receive] makes BEFORE its walk exists, as
// "<first result> -> <second result>" -> why. HELD BOTH WAYS: a return with no entry is an arm
// answering without the walk, and an entry with no return is this gate describing code that has
// moved.
//
// THESE ARE THE ONLY ONES ALLOWED TO ANSWER ON THEIR OWN, and the reason is one sentence: there is
// no walk yet, so there is nothing to commit and nothing sticky to consult. Every return below the
// walk's construction is held against the walk instead, by [receiveAnswersThroughTheWalk].
var receiveAnswersBeforeTheWalk = map[string]string{
	"nil -> fmt.Errorf": "TWO sites: the closed-group refusal, which is the first statement after " +
		"the mutex; and this device's own sender_handle, which is one of the two values the walk " +
		"is CONSTRUCTED FROM. Neither has a walk to fold back, and the second cannot have one -- " +
		"`walk.own` is that handle.",
	"nil -> ErrNoMemberAdded": "a group with no session: a [Group] built by [Device.CreateGroup] " +
		"whose founder has not yet added anybody, which has no epoch keys to fetch under.",
	"nil -> err": "THREE sites, and all three are inputs the walk is BUILT FROM. " +
		"[Group.rebindLocked] is the server nonce this fetch's authenticator is bound to, " +
		"[Device.nonce] is that authenticator's own, and [Group.leavesLocked] is `walk.leaves`. A " +
		"walk constructed over any of those failures would be a walk over a page this device " +
		"could not have asked for.",
}

func TestEveryErrorReceiveAnswersComesOutOfTheWalksOwnCommit(t *testing.T) {
	sources := stateTestProductionSources(t)
	fileSet := token.NewFileSet()

	found := false
	before := map[string][]string{}
	after := []string{}
	discarded := []string{}

	for _, name := range sources {
		parsed, err := parser.ParseFile(fileSet, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		at := func(node ast.Node) string {
			return fmt.Sprintf("%s:%d", name, fileSet.Position(node.Pos()).Line)
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}

			// THE DISCARD CLAUSE, and it is over EVERY production function rather than over
			// [Group.Receive] alone. The original defect was a call whose return value was
			// dropped; the repair made that impossible to spell by giving the function a
			// parameter, and this is what refuses the shape coming back by any other route --
			// including in a function this gate has never heard of.
			ast.Inspect(function.Body, func(node ast.Node) bool {
				statement, isStatement := node.(*ast.ExprStmt)
				if !isStatement {
					return true
				}
				call, isCall := statement.X.(*ast.CallExpr)
				if !isCall || censusCalleeName(call.Fun) != "commitWalkLocked" {
					return true
				}
				discarded = append(discarded,
					fmt.Sprintf("%s in %s", at(call), function.Name.Name))
				return true
			})

			if function.Name.Name != "Receive" || !receiveIsGroupMethod(function) {
				continue
			}
			found = true

			// WHERE THE WALK BEGINS. Every return above this position answers without a walk
			// because there is not one; every return below it must answer through the walk.
			walkAt := token.NoPos
			ast.Inspect(function.Body, func(node ast.Node) bool {
				assign, ok := node.(*ast.AssignStmt)
				if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
					return true
				}
				target, bare := assign.Lhs[0].(*ast.Ident)
				if !bare || target.Name != "walk" {
					return true
				}
				if walkAt == token.NoPos {
					walkAt = assign.Pos()
				}
				return true
			})
			if walkAt == token.NoPos {
				t.Fatalf("%s: this gate found no `walk :=` in Group.Receive, so it cannot tell "+
					"the arms that have a walk from the ones that do not, and every clause below "+
					"would be measuring nothing", name)
			}

			ast.Inspect(function.Body, func(node ast.Node) bool {
				// A function literal inside Receive has its own returns and they are not
				// Receive's answers. `refreshReadKey` is one and it returns a single error.
				if _, isLiteral := node.(*ast.FuncLit); isLiteral {
					return false
				}
				statement, ok := node.(*ast.ReturnStmt)
				if !ok || len(statement.Results) != 2 {
					return true
				}
				answer := statement.Results[1]
				if statement.Pos() < walkAt {
					site := censusExpr(statement.Results[0]) + " -> " +
						receiveAnswerHead(answer)
					before[site] = append(before[site], at(statement))
					return true
				}
				after = append(after, at(statement)+" "+censusExpr(answer))
				call, isCall := answer.(*ast.CallExpr)
				if isCall && censusCalleeName(call.Fun) == "commitWalkLocked" {
					return true
				}
				t.Errorf("Group.Receive answers %q at %s, and that return is BELOW the walk's "+
					"own construction. Every error a walk-bearing arm answers has to come out of "+
					"[Group.commitWalkLocked], because that is the only place [Group.wrapDark] "+
					"and [Group.identityInUse] are consulted on the read path -- and a dark "+
					"group's every later fetch is REFUSED by the server, so this is the arm the "+
					"diagnosis has to survive. Measured: before the repair, a dark group's second "+
					"and every later Receive answered REASON_REJECTED and errors.Is("+
					"ErrWrapUnreadable) was false.", censusExpr(answer), at(statement))
				return true
			})
		}
	}

	if !found {
		t.Fatal("this gate did not find Group.Receive in any production source, so it is holding " +
			"nothing. A rename is a change this file has to be read against.")
	}

	// ── THE COMPLEMENT, PRINTED AND THEN ASSERTED ────────────────────────────────────────────
	t.Logf("production sources read (%d)", len(sources))
	t.Logf("returns BELOW the walk, every one of which must be the walk's own answer (%d):", len(after))
	for _, one := range after {
		t.Logf("    %s", one)
	}
	t.Logf("returns ABOVE the walk, which answer on their own and are held against a written-down "+
		"disposition (%d):", len(before))
	for _, site := range epochKeySortedMap(before) {
		t.Logf("    %s  at %v", site, before[site])
	}
	t.Logf("bare `commitWalkLocked(...)` statements in production, which is the discarded-return "+
		"shape itself: %v", discarded)

	if len(after) == 0 {
		t.Error("this gate found no two-result return below the walk at all. Group.Receive has " +
			"either been rewritten or this gate has stopped reading it, and either way every " +
			"refusal above passed by having nothing to refuse.")
	}
	if 0 < len(discarded) {
		t.Errorf("commitWalkLocked is called as a STATEMENT at %v, so whatever it decided is "+
			"discarded there. That is the original defect's exact shape: the sticky diagnosis "+
			"[Group.wrapDark] is computed and dropped, and the caller is told something else.",
			discarded)
	}
	censusHold(t, "Receive answer before the walk", before, receiveAnswersBeforeTheWalk,
		"A two-result return ABOVE the walk's construction answers without folding a walk back "+
			"into the group, which is correct only while there is no walk. A return with no entry "+
			"is an arm somebody added without deciding that; an entry with no return is this "+
			"disposition describing code that has moved, and the clause below it -- which is what "+
			"actually refuses the defect -- has to be re-read before the entry is deleted.")
}

// receiveIsGroupMethod is whether this declaration is a method on *Group, so that a `Receive` on
// some other type in this package is not mistaken for the one this gate is about.
func receiveIsGroupMethod(function *ast.FuncDecl) bool {
	if function.Recv == nil || len(function.Recv.List) != 1 {
		return false
	}
	pointer, ok := function.Recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	named, ok := pointer.X.(*ast.Ident)
	return ok && named.Name == "Group"
}

// receiveAnswerHead is how a return's error result is named in the census above: a call by its
// callee, anything else by its own spelling. It is what makes `fmt.Errorf(…)` one site rather than
// one site per message.
func receiveAnswerHead(answer ast.Expr) string {
	if call, ok := answer.(*ast.CallExpr); ok {
		return censusExpr(call.Fun)
	}
	return censusExpr(answer)
}
