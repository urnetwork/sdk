package urmessage

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ══════════════════════════════════════════════════════════════════════════════════════════════
// EVERY ARM THAT ENTERS AN EPOCH FILES AND PRUNES WHAT THAT COMMIT TOOK OUT OF THE GROUP
// ══════════════════════════════════════════════════════════════════════════════════════════════
//
// WHAT THIS HOLDS. Two functions move this group onto an epoch a commit opened:
// [Group.ingestCommitLocked], which follows somebody else's commit, and
// [Group.publishCommitLocked], which follows this device's own. Since ledger item 245 each owes the
// same two statements over the same vector before it re-tracks the ladders --
// [Group.noteDepartedLeavesLocked] so the departed leaf's records stay resolvable, and
// [Group.pruneRemovedLaddersLocked] so no receiver ratchet is re-installed at every future epoch
// for a leaf nobody occupies. The ingest arm has run them since 245; the publish arm owed nothing
// until [Group.RemoveMember] gave a committer removed leaves of its own, and gained them with it.
//
// WHY THIS GATE EXISTS, AND IT IS A MUTANT THAT SURVIVED. Ledger item 259 filed it: deleting
// `self.pruneRemovedLaddersLocked(pending.RemovedLeaves)` from [Group.publishCommitLocked] -- the
// prune half, alone -- leaves **both** modules green. Reproduced at `sdk da331d9`:
// `go test ./urmessage -run '.*'` ok 17.9s and `go test ./cp3b -run '.*'` ok 43.6s, with the line
// gone. The previous pass deleted BOTH new lines together, saw cp3b's
// TestTheAdminThatRemovedSomebodyStillReadsTheirHistoryAfterARestart go red, and attributed the
// failure to the pair; that case is the FILING's, and it is red for the filing alone.
//
// AND THE SURVIVING MUTANT IS FIRST A CLAIM ABOUT THE QUERY, so the query was re-asked three ways
// before this instrument was chosen. The behavioural route is EMPTY, and that is a theorem about
// this build rather than a failure to think of a case: [Group.notePeerHeadLocked] raises
// [Group.peerHeads] and [Group.peerHeadsAt] from the same stream index in the same call, and both
// [Group.trackLocked] and [Group.crossEpochLadderLocked] position a ladder at
// `max(head, leafStreamFloorLocked(leaf, epoch))` -- so the head a STALE row supplies is exactly
// the floor a PRUNED one falls back to, and a newcomer on the refilled leaf meets the same rung
// either way. Nothing deletes from [Group.peerHeadsAt] or [Group.persistedHeads], so the floor is
// never lost. What the prune buys is therefore a ratchet not rebuilt, at every epoch change, for a
// leaf no member stands at -- a fact about this group's state and not about any record's fate.
//
// WHICH IS WHY THE INGEST ARM'S OWN PIN IS A STATE ASSERTION AND NOT A BEHAVIOURAL ONE:
// [TestASurvivorDoesNotReTrackAReusedLeafAtTheRemovedMembersHead] reads `peerHeads` directly, and
// it goes red for the ingest arm's prune (measured: it does, naming the head it still holds). The
// publish arm cannot be pinned that way, because no test in this package can run a publishing verb
// to completion -- the submit goes through `*sdk.MessageTransport`, a concrete type with no stub --
// and cp3b, which can, is another module and cannot read an unexported field. So the strongest
// available instrument over the publish arm is the text, and this is it.
//
// RULING 46 IS NOT AVOIDED, IT IS ANSWERED. Ruling 46 refuses an AST reading that STANDS IN for a
// runtime question. The runtime question here has been measured and has no answer: there is no
// input that distinguishes the pruned build from the unpruned one, because the floor absorbs the
// difference. What is left is a fact about the text -- do both arms run both statements over one
// vector before the re-track -- and a fact about the text is what the text can be asked.
//
// THE ARM SET IS DERIVED AND NOT LISTED, which is what makes this more than a two-entry table: an
// arm is any production function of this package that calls [Group.crossEpochLadderLocked], so a
// THIRD arm added later lands here as a function with no pair rather than as a silent third copy.
// The complement is printed and asserted BOTH ways: a function that files or prunes without
// re-tracking is reported too, because such a site is one this gate's subject does not cover.
//
// AND THE CONTROLS ARE INLINE, EACH WITH ITS LITERAL TAKEN FROM THE SOURCE.
// `ingestCommitLocked` must be in the derived set -- it is the arm item 245 built, so a set without
// it is a walk that read nothing. `enterEpochLocked` must NOT be: it is the function that moves
// [Group.epoch] and persists, it runs immediately after the re-track on both arms, and a derivation
// that swept it in would be naming statement neighbourhoods rather than callers.
//
// WHAT WOULD GO RED: delete either statement from either arm; move either after the re-track (the
// order is [Group.crossEpochLadderLocked]'s own requirement -- it re-tracks every entry of
// `peerHeads` at the new epoch, so a head left behind is a ladder installed at the removed member's
// index for a leaf §7.7's next Add refills); pass the two statements different vectors; add a third
// caller of the re-track without the pair.

// removalBookkeepingPair is the two statements every arm owes, and the re-track they must precede.
const (
	removalBookkeepingFile  = "noteDepartedLeavesLocked"
	removalBookkeepingPrune = "pruneRemovedLaddersLocked"
	removalBookkeepingTrack = "crossEpochLadderLocked"
)

// removalBookkeepingCall is one call of one of the three, with where it stands in its function.
type removalBookkeepingCall struct {
	at     token.Pos
	vector string
	where  string
}

func TestBothArmsThatEnterAnEpochFileAndPruneWhatTheirOwnCommitRemoved(t *testing.T) {
	root := moduleRoot(t)
	dir := filepath.Join(root, "urmessage")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	// every call of the three, per production function of this package
	calls := map[string]map[string][]removalBookkeepingCall{}
	files, functions := 0, 0
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if parseErr != nil {
			t.Fatalf("parsing %s: %v", name, parseErr)
		}
		files += 1
		for _, decl := range file.Decls {
			function, isFunction := decl.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			functions += 1
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, isCall := node.(*ast.CallExpr)
				if !isCall {
					return true
				}
				selector, isSelector := call.Fun.(*ast.SelectorExpr)
				if !isSelector {
					return true
				}
				switch selector.Sel.Name {
				case removalBookkeepingFile, removalBookkeepingPrune, removalBookkeepingTrack:
				default:
					return true
				}
				vector := ""
				if 0 < len(call.Args) {
					vector = exprText(call.Args[0])
				}
				if calls[function.Name.Name] == nil {
					calls[function.Name.Name] = map[string][]removalBookkeepingCall{}
				}
				position := fset.Position(call.Pos())
				calls[function.Name.Name][selector.Sel.Name] = append(
					calls[function.Name.Name][selector.Sel.Name],
					removalBookkeepingCall{at: call.Pos(), vector: vector,
						where: filepath.Base(position.Filename) + ":" + strconv.Itoa(position.Line)})
				return true
			})
		}
	}

	// ── THE ARMS, DERIVED ──────────────────────────────────────────────────────────────────────
	arms := []string{}
	for function, byName := range calls {
		if 0 < len(byName[removalBookkeepingTrack]) {
			arms = append(arms, function)
		}
	}
	sort.Strings(arms)

	// ── THE CONTROLS, IN THE SAME QUERY AND OVER THE SAME MAP ──────────────────────────────────
	if files == 0 || functions == 0 {
		t.Fatalf("CONTROL FAILED: the walk read %d production file(s) and %d function(s); a run that "+
			"read nothing satisfies every assertion below", files, functions)
	}
	found := map[string]bool{}
	for _, one := range arms {
		found[one] = true
	}
	if !found["ingestCommitLocked"] {
		t.Fatalf("CONTROL FAILED: ingestCommitLocked is not in the derived arm set %v. It is the arm "+
			"ledger item 245 built and it calls the re-track; a set without it means the derivation "+
			"read nothing, and every assertion below would then be about the empty set", arms)
	}
	if found["enterEpochLocked"] {
		t.Fatalf("CONTROL FAILED: enterEpochLocked is in the derived arm set %v. It runs immediately "+
			"after the re-track on both arms and calls none of the three, so a derivation that swept "+
			"it in is naming statement neighbourhoods rather than callers", arms)
	}
	if len(arms) < 2 {
		t.Fatalf("CONTROL FAILED: %d arm(s) call the re-track (%v) and there are two -- the ingest "+
			"path and the publish path. One means the publish arm's call has gone, which is the "+
			"defect this gate exists for arriving as a vacuous pass", len(arms), arms)
	}

	// ── THE PROPERTY ───────────────────────────────────────────────────────────────────────────
	for _, arm := range arms {
		byName := calls[arm]
		track := byName[removalBookkeepingTrack]
		if len(track) != 1 {
			t.Errorf("%s calls %s %d time(s); this gate reads the ONE re-track each arm makes and "+
				"cannot order two", arm, removalBookkeepingTrack, len(track))
			continue
		}
		for _, owed := range []string{removalBookkeepingFile, removalBookkeepingPrune} {
			made := byName[owed]
			if len(made) == 0 {
				t.Errorf("%s enters an epoch (it calls %s at %s) and never calls %s. Both halves of "+
					"ledger item 245's bookkeeping are owed by every arm that enters an epoch: the "+
					"filing keeps the departed leaf's records resolvable after a restart, and the "+
					"prune stops a receiver ratchet being re-installed at every future epoch for a "+
					"leaf nobody occupies. Deleting the PRUNE alone leaves both modules green, which "+
					"is exactly why this is read here", arm, removalBookkeepingTrack,
					track[0].where, owed)
				continue
			}
			if 1 < len(made) {
				t.Errorf("%s calls %s %d times; one arm, one call, or the vector comparison below is "+
					"between two of several", arm, owed, len(made))
			}
			if track[0].at < made[0].at {
				t.Errorf("%s calls %s at %s, AFTER the re-track at %s. The order is the re-track's "+
					"own requirement: it re-tracks every entry of peerHeads at the new epoch, so a "+
					"head left behind is a ladder installed at the removed member's index for a leaf "+
					"the next Add refills", arm, owed, made[0].where, track[0].where)
			}
		}
		filing, prune := byName[removalBookkeepingFile], byName[removalBookkeepingPrune]
		if 0 < len(filing) && 0 < len(prune) && filing[0].vector != prune[0].vector {
			t.Errorf("%s files %s and prunes %s. The two are one read of one staged commit -- the "+
				"leaves this device stops wrapping to and the leaves it files as departed -- and two "+
				"expressions is the site where they become two values", arm,
				filing[0].vector, prune[0].vector)
		}
		if 0 < len(filing) && filing[0].vector == "" {
			t.Errorf("%s calls %s with no argument at all", arm, removalBookkeepingFile)
		}
	}

	// ── THE COMPLEMENT, PRINTED AND ASSERTED ───────────────────────────────────────────────────
	// a function that files or prunes and does NOT re-track is a site this gate's subject does not
	// cover, which is a finding and not a footnote: the two statements exist to be run where an
	// epoch is entered, and anywhere else they are bookkeeping nobody ordered.
	outside := []string{}
	for function, byName := range calls {
		if 0 < len(byName[removalBookkeepingTrack]) {
			continue
		}
		if 0 < len(byName[removalBookkeepingFile]) || 0 < len(byName[removalBookkeepingPrune]) {
			outside = append(outside, function)
		}
	}
	sort.Strings(outside)
	for _, function := range outside {
		t.Errorf("%s calls the departed-leaf filing or the ladder prune and does NOT call %s, so it "+
			"is a third site for a pair that belongs where an epoch is entered. Either it enters an "+
			"epoch and owes the re-track, or it is bookkeeping outside this gate's subject and this "+
			"gate has to be told which", function, removalBookkeepingTrack)
	}
	t.Logf("%d production file(s), %d function(s); the arms that enter an epoch are %v, each filing "+
		"and pruning its own commit's removed leaves before the re-track", files, functions, arms)
	for _, arm := range arms {
		byName := calls[arm]
		if len(byName[removalBookkeepingFile]) == 0 || len(byName[removalBookkeepingPrune]) == 0 {
			continue
		}
		t.Logf("  %s: %s(%s) at %s, %s(%s) at %s, then %s at %s", arm,
			removalBookkeepingFile, byName[removalBookkeepingFile][0].vector,
			byName[removalBookkeepingFile][0].where,
			removalBookkeepingPrune, byName[removalBookkeepingPrune][0].vector,
			byName[removalBookkeepingPrune][0].where,
			removalBookkeepingTrack, byName[removalBookkeepingTrack][0].where)
	}
	t.Logf("the complement -- functions that file or prune without entering an epoch: %v", outside)
}
