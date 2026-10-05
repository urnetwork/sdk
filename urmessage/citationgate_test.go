package urmessage

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ══════════════════════════════════════════════════════════════════════════════════════════════
// EVERY TEST THIS REPOSITORY'S PRODUCTION PROSE CITES EXISTS, AND EXISTS EXACTLY ONCE
// ══════════════════════════════════════════════════════════════════════════════════════════════
//
// WHY THIS IS A GATE AND NOT A CONVENTION. This corpus writes its arguments in production comments
// and discharges them by naming the case that holds each one -- "an arm that forgot would be caught
// by the property rather than by this sentence: <Test...>". A name that resolves to nothing turns
// that discharge into decoration: the sentence still reads as though something holds it, and the
// next reader has no way to find out that nothing does without running the query. Ledger item 257
// recorded exactly that, found by hand: `publishCommitLocked`'s header cited
// TestAMemberRemovedByACommitCannotDeriveTheEpochThatCommitOpens, a name with one hit in either
// repository -- the comment citing it. Driven here at the commit that consumed ruling 51, the same
// query over the whole repository found FOUR more (an eph-wrap twin misnamed by two words, a
// ladder gate that had been renamed, a .def gate that had been renamed, and a stream-store
// exclusion named for a property rather than for the case that holds it) and TWO short forms that
// named a family rather than a case. All six are repaired in that commit; this is what stops the
// seventh.
//
// IT IS A STRUCTURAL PROPERTY AND THE INSTRUMENT IS STRUCTURAL, which is why ruling 46 does not
// apply to it. Ruling 46 refuses an AST reading that stands in for a RUNTIME question -- "does this
// refusal fire for every input it should" -- because each reading it adds is a fresh surface to
// route around. The question here is not about any run: it is *does this identifier name a
// declaration in this corpus*, a fact about the text, decided by reading the text. There is nothing
// behavioural underneath it to be approximated and nothing an indirection can hide, because a
// citation is prose and prose has no indirections.
//
// THE NARROWING IS ASSERTED IN BOTH DIRECTIONS AND NOT MERELY PRINTED. Two dispositions below
// carve names out of the refusal, and each is held both ways: an entry whose name this repository
// declares after all is a FAILURE (the carve-out has gone stale and is now excusing a name the
// plain rule would pass), and an entry no production comment cites any more is a FAILURE (the
// carve-out is excusing nothing and is the shape a disposition rots into). So a rename in connect,
// or the deletion of the one comment that needed an entry, reports here rather than going quiet.
//
// AND THE CONTROLS ARE INLINE AND FIRE FOR THEIR OWN REASONS. The absent literal is the very name
// item 257 found dangling -- it is not declared anywhere in this corpus and must resolve to zero,
// or this query is looking in the wrong place. The present literal is the case that holds the
// removal property one file away, and must resolve to exactly one. A build in which the walk found
// no files at all would answer zero for BOTH, which the second control refuses.
//
// AND ITS SUBJECT IS AN UNBRACKETED NAME, WHICH IS LEDGER RULING 57 AND USED NOT TO BE. For one
// commit the godoc gate below HANDED its bracketed `[Test...]` spellings to this rule, so that one
// namespace carried two rules: *names a declaration in the documented package* for most bracketed
// spellings and *names a declaration somewhere* for six handed ones. Ruling 57 closed that: a godoc
// bracket is a RENDERING contract -- `[X]` means "link to X" -- so it must name a declaration the
// documented package CONTAINS, and a test or test-helper name goes in backticks. Those six links are
// backticked and the hand-off arm is deleted, which costs this gate nothing: its net reads a name
// wherever it stands, backticks and all, so every one of them is still held here.

// citationDeclaredElsewhere is every cited name that lives in a SIBLING repository rather than in
// this one, with where it lives and why this file cannot simply look.
//
// WHY A TABLE AND NOT A WALK OF THE SIBLING. connect's own cross-repo gate reads ../../sdk, so the
// precedent for reaching across exists -- and the cost is what decides against it here: a walk of
// a sibling checkout makes THIS repository's suite depend on that checkout being present and at a
// compatible commit, so a developer with only `sdk` cloned gets a red suite about somebody else's
// file layout. A table costs one line per citation, needs no second checkout, and -- because it is
// held both ways below -- reports a rename over there as loudly as a walk would. What it cannot do
// is notice that the named test has been DELETED in the sibling while its name stays in this table;
// that residual is stated here rather than left to be found, and the reading that closes it is that
// sibling's own suite going red on the deletion.
//
// AND THE ARGUMENT IS STRICTLY STRONGER FOR msgrepo THAN FOR connect, which is worth saying now that
// an entry names it. `go.mod` replaces connect to ../connect, so connect's source is already a
// requirement of this module's build and the godoc gate below walks it for that reason. msgrepo
// appears in this module's go.mod nowhere at all -- it is the SERVER -- so a walk of it would add a
// checkout this build does not need for any other purpose.
var citationDeclaredElsewhere = map[string]string{
	"TestABodyNoRungCouldHoldCostsNeitherAnIndexNorAGeneration": "connect/messagegroup/mlsframe_test.go -- " +
		"the frame-size ladder is connect's and the cost it prices is read from this side",
	// THE FIRST msgrepo ENTRY IN THIS TABLE, and it is read rather than named: that case walks a
	// member holding read_key[1] from epoch 1 to epoch 3, asserts THREE round trips for three
	// epochs ("one per epoch"), that every page comes back Complete, and that the walk arrives
	// holding records 1..12. It is ledger item 246's own acceptance condition -- F0 is wrong if a
	// behind member cannot walk forward under the ceiling -- and it is cited from this side because
	// it is why liveprobe's one drain call site exists.
	"TestAMemberSeveralEpochsBehindWalksForwardOneEpochPerRoundTrip": "msgrepo/api/epochceiling_test.go -- " +
		"item 246's F0 ceiling paced end to end at the server: one epoch per round trip, every page " +
		"Complete, and the walk arrives at the present rather than short of it",
	"TestARemovalWhoseLeafIsRefilledInTheSameCommitIsStillNamedByTheStagedCommit": "connect/messagegroup/" +
		"engineremovewithextensions_test.go -- ledger ruling 51's derivation held one layer down, " +
		"on the seam's own PendingEpoch.RemovedLeaves",
	"TestAnyMemberCanStillSquatAnotherLeafsStreamIndex": "connect/messagegroup/m1w1repairs_test.go -- " +
		"the squat is a property of the server's index space, which connect owns",
	"TestClassBucketJoinIsConfinedToRecordGo": "connect/message/record_test.go -- the class bucket's " +
		"join is connect's, and this repository is the caller it is confined against",
	"TestTheCommittersOwnPathCanSwapItsLeafIdentityAndOnlyThePreCommitTreeStillNamesIt": "connect/mls/" +
		"staged_after_test.go -- the staged tree's own behaviour, below the seam",
	"TestTheSizeLadderCostOfTheInnerFrameIsMeasuredHere": "connect/messagegroup/mlsframe_test.go -- " +
		"the same ladder as the first entry, measured rather than reasoned",
}

// citationIsNotACase is a name the language reserves, cited as a mechanism rather than as a case.
//
// TestMain IS GO'S OWN ENTRY POINT and a package may declare at most one, so "exactly one
// declaration in this repository" is the wrong question about it: this repository has one and every
// sibling module may have its own, and a comment that says "TestMain installs X" is naming the
// hook and not a property somebody holds. It is carved out BY NAME and held both ways like the
// table above, so the day nothing cites it this entry reports rather than sitting here.
var citationIsNotACase = map[string]string{
	"TestMain": "go's own per-package entry point, named as a mechanism rather than as a case; " +
		"a repository has one per package and the count is meaningless",
}

// citationCorpus is every `Test...` function this repository declares and every citation of one in
// its production prose, read ONCE and answered to by the rule above.
//
// WHY IT IS A STRUCTURE AND NOT TWO LOCALS, WHICH IS THIS FILE'S HISTORY AND NO LONGER ITS REASON.
// For one commit the godoc gate below HANDED this corpus its bracketed `[Test...]` spellings and
// asserted the hand-off against these very maps, so the scan and the rule had to be reachable from
// two callers. Ledger ruling 57 deleted that arm -- a bracket must name a declaration the documented
// package contains, and a case name goes in backticks -- so there is one caller again. The structure
// stays because it is what makes `holding` below THE reading of this rule rather than a switch
// written out wherever the rule is needed, which is the defect this file shipped once.
//
// THE DEFECT THAT BOUGHT THAT HAND-OFF is recorded in godocReadDeclarations' header: the godoc
// gate used to read `_test.go` declarations into the tables it resolved production prose against,
// while the external half of the same seam excluded them. SIX of these names resolved down there
// by a rule that existed nowhere -- and `rotWorld`, a test type and no `Test...` spelling at all,
// resolved beside them, bracketed at urmessage/pqepoch.go:1147 with the same type correctly
// backticked two lines above it. The hand-off was the first repair; ruling 57 is the second, and it
// is the one that leaves a single rule in the bracket namespace.
type citationCorpus struct {
	// declared is every Test... function this repository declares, to the files declaring it.
	declared map[string][]string
	// cited is every Test... spelling a production comment names, to the sites naming it.
	cited map[string][]string
	// total and production are the .go files the walk saw and how many of them are not tests.
	total, production int
}

// citationHolding is which disposition of the rule below holds one spelling, or none of them.
type citationHolding int

const (
	citationHeldHere citationHolding = iota
	citationHeldElsewhere
	citationHeldNotACase
	citationUnheld
)

// holding is THE reading of this file's test-citation rule, and there is exactly one of it.
//
// THE RULE IS DECIDED HERE AND NOWHERE ELSE, rather than as a switch written out at each place that
// needs it, because the defect this file shipped once was two readings of one rule: the godoc gate's
// own walk and godocDeclarationsAt disagreed about `_test.go`, and the laxer of the two was the one
// answering this repository's own prose. A second copy of a decision is where that starts. Ruling 57
// left this method one caller and that is not a reason to inline it back into that caller.
func (self *citationCorpus) holding(name string) citationHolding {
	if _, carved := citationIsNotACase[name]; carved {
		return citationHeldNotACase
	}
	if _, carved := citationDeclaredElsewhere[name]; carved {
		return citationHeldElsewhere
	}
	if len(self.declared[name]) == 1 {
		return citationHeldHere
	}
	return citationUnheld
}

// citationNet is the spelling of a cited case: one net, stated once, for the one rule that reads it.
//
// IT WAS PACKAGE-LEVEL SO THE godoc GATE COULD ROUTE ON THIS VERY EXPRESSION, and ruling 57 deleted
// that arm. It stays here rather than moving into citationScan because the spelling of a cited case
// is this file's own vocabulary and a second copy of it -- a `strings.HasPrefix(name, "Test")`, say
// -- is how two nets come to disagree about which names anything holds.
var citationNet = regexp.MustCompile(`\bTest[A-Z][A-Za-z0-9_]*`)

func citationScan(t *testing.T, root string) *citationCorpus {
	// a Go test function's declaration, which has to name the same spelling citationNet finds.
	declaration := regexp.MustCompile(`(?m)^func\s+(Test[A-Z][A-Za-z0-9_]*)\s*\(`)
	citation := citationNet

	declared := map[string][]string{}
	cited := map[string][]string{}
	production, total := 0, 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			if name := info.Name(); name == ".git" || name == "testdata" || name == "vendor" || name == "build" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		source, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		total += 1
		rel := filepath.ToSlash(func() string {
			at, _ := filepath.Rel(root, path)
			return at
		}())
		for _, one := range declaration.FindAllStringSubmatch(string(source), -1) {
			declared[one[1]] = append(declared[one[1]], rel)
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		production += 1
		// THE CITATION IS IN A COMMENT AND NOWHERE ELSE. A production file cannot call a test, so
		// a Test... spelling outside a comment would be a different finding; reading only the
		// comment half keeps this gate's subject the PROSE, which is what rots.
		for at, line := range strings.Split(string(source), "\n") {
			cut := strings.Index(line, "//")
			if cut < 0 {
				continue
			}
			for _, name := range citation.FindAllString(line[cut:], -1) {
				cited[name] = append(cited[name], fmt.Sprintf("%s:%d", rel, at+1))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return &citationCorpus{declared: declared, cited: cited, total: total, production: production}
}

func TestEveryTestNameCitedInThisRepositorysProductionProseResolvesToOneDeclaration(t *testing.T) {
	root := moduleRoot(t)
	scan := citationScan(t, root)
	declared, cited := scan.declared, scan.cited
	total, production := scan.total, scan.production

	// ── THE CONTROLS, IN THE SAME QUERY AND OVER THE SAME MAPS ─────────────────────────────────
	const absent = "TestAMemberRemovedByACommitCannotDeriveTheEpochThatCommitOpens"
	const present = "TestThreeMembersRotateAcrossTwoEpochsAndAMemberRemovedByThatCommitCannotFollow"
	if found := len(declared[absent]); found != 0 {
		t.Fatalf("CONTROL FAILED: %s resolves to %d declaration(s). It is ledger item 257's own "+
			"dangling citation and this query is supposed to answer zero for it; a non-zero answer "+
			"means the net is matching something that is not a declaration", absent, found)
	}
	if found := len(declared[present]); found != 1 {
		t.Fatalf("CONTROL FAILED: %s resolves to %d declaration(s), want 1. Without this the zero "+
			"above is satisfied by a walk that read no files at all", present, found)
	}
	if production == 0 || total == production {
		t.Fatalf("CONTROL FAILED: the walk saw %d .go files of which %d are production; a run with "+
			"no production files or no test files is measuring nothing", total, production)
	}

	// ── THE DISPOSITIONS, HELD BOTH WAYS ───────────────────────────────────────────────────────
	for name, why := range citationDeclaredElsewhere {
		if here := declared[name]; 0 < len(here) {
			t.Errorf("%s is carved out as living in a sibling repository (%s) and THIS repository "+
				"declares it at %s. The entry is excusing a name the plain rule would pass; delete it",
				name, why, strings.Join(here, ", "))
		}
		if len(cited[name]) == 0 {
			t.Errorf("%s is carved out as living in a sibling repository (%s) and no production "+
				"comment cites it any more. An entry nothing needs is how a disposition rots; delete it",
				name, why)
		}
	}
	for name, why := range citationIsNotACase {
		if len(cited[name]) == 0 {
			t.Errorf("%s is carved out as not-a-case (%s) and no production comment names it any "+
				"more; delete the entry", name, why)
		}
	}

	// ── THE PROPERTY ───────────────────────────────────────────────────────────────────────────
	names := []string{}
	for name := range cited {
		names = append(names, name)
	}
	sort.Strings(names)
	here, elsewhere, exempt := 0, 0, 0
	for _, name := range names {
		// THE DISPOSITION IS DECIDED BY scan.holding AND NOWHERE ELSE, so this rule has exactly one
		// reading. It had two callers while the godoc gate handed its bracketed case names over;
		// ruling 57 left it one, and one reading is still the point.
		switch scan.holding(name) {
		case citationHeldNotACase:
			exempt += 1
			continue
		case citationHeldElsewhere:
			elsewhere += 1
			continue
		case citationHeldHere:
			here += 1
			continue
		}
		found := declared[name]
		if len(found) == 0 {
			// the nearest declared name, so a rename or a typo says so rather than making the
			// reader run the query by hand. It is a REPORT and not a suggestion: naming the wrong
			// case is worse than naming none, so what follows the colon has to be read before it
			// is written in.
			t.Errorf("%s is cited at %s and NOTHING in this repository declares it. A sentence that "+
				"names a case which does not exist reads as though something holds it and nothing "+
				"does. Either the case is in a sibling repository -- add it to "+
				"citationDeclaredElsewhere, having READ what it holds -- or the citation is stale "+
				"and the right name goes here.%s",
				name, strings.Join(cited[name], ", "), citationNearest(name, declared))
			continue
		}
		t.Errorf("%s is cited at %s and this repository declares it %d times (%s), so the citation "+
			"does not say which one", name, strings.Join(cited[name], ", "), len(found),
			strings.Join(found, ", "))
	}

	// ── THE COMPLEMENT, PRINTED BESIDE WHAT WAS ASSERTED ───────────────────────────────────────
	t.Logf("%d .go files walked, %d of them production; %d distinct Test... names declared here",
		total, production, len(declared))
	t.Logf("%d distinct names cited in production prose: %d declared here exactly once, %d carved "+
		"out to a sibling repository, %d carved out as not-a-case", len(names), here, elsewhere, exempt)
	if here == 0 {
		t.Errorf("no cited name resolves inside this repository at all, so the rule above is " +
			"vacuous and only the carve-outs are being exercised")
	}
}

// citationNearest is the declared name that shares the longest prefix with a dangling one, as a
// suffix for the failure above, or the empty string when nothing is close. It exists so a two-word
// rename says so in the failure rather than in a reader's next hour.
func citationNearest(name string, declared map[string][]string) string {
	best, longest := "", 0
	for candidate := range declared {
		shared := 0
		for shared < len(name) && shared < len(candidate) && name[shared] == candidate[shared] {
			shared += 1
		}
		if longest < shared {
			best, longest = candidate, shared
		}
	}
	if best == "" || longest < 12 {
		return ""
	}
	return fmt.Sprintf(" The declared name sharing the longest prefix (%d characters) is %s -- READ "+
		"WHAT IT HOLDS before writing it in.", longest, best)
}

// ══════════════════════════════════════════════════════════════════════════════════════════════
// EVERY GODOC LINK IN THIS REPOSITORY'S PRODUCTION PROSE NAMES A DECLARATION
// ══════════════════════════════════════════════════════════════════════════════════════════════
//
// THIS IS THE GATE ABOVE, ONE NAMESPACE OVER, AND LEDGER ITEM 259 FILED THE INSTANCE. Two
// production comments pointed at [Group.RemoveDevice] -- a METHOD that ruling 50 put in its own
// track after removal ships, and that nothing in this repository declares. The test-citation gate
// above could not see it: its subject is `Test...` spellings, and this is the same rot in the
// bracket namespace, where this corpus does most of its pointing. Driven at the commit that closed
// item 259's owed list, the same query over the whole module found SEVEN more, every one repaired
// by READING the target rather than by matching a name: a method renamed since the sentence was
// written ([Group.wrapTargetsLocked], which is wrapTargetsAtLocked), a FIELD PATH no godoc link
// can name ([Group.handle.Epoch]), four bare field or method names written inside their own
// package's prose ([Text], [Send], [RemovedKind], [WrapDarkKind], each of which resolves only when
// it is qualified), a LOCAL CLOSURE (`answerSecret`) and an unexported method of a sibling package
// (`installPqSecretOnLoop`) -- the last two un-bracketed, because no link can name either.
//
// RULING 46 DOES NOT REACH IT, FOR THE GATE ABOVE'S REASON AND NOT A NEW ONE. The question is not
// whether a refusal fires for every input it should; it is *does this identifier name a
// declaration*, which is a fact about the text and is decided by reading the text. There is
// nothing behavioural underneath it and prose has no indirections to hide behind.
//
// RESOLUTION IS PER PACKAGE, WHICH IS GODOC'S OWN RULE AND NOT A CONVENIENCE. An unqualified link
// names something in the package the comment lives in, so `[receive]` in liveprobe and
// `[Group.Receive]` here are two different questions, and one repository-wide table would answer
// both wrongly. The declarations are read off the AST rather than off a regex, so a method is a
// method; a struct FIELD counts, because this corpus links fields although godoc renders no link
// for one and a reader still has to be able to find them; and a build-tagged file's declarations
// count too, or a link in the windows file to the unix spelling would be a false red.
//
// A QUALIFIED LINK IS RESOLVED IN THE PACKAGE ITS HEAD NAMES, AND THIS PARAGRAPH IS THIS GATE'S
// OWN CORRECTION OF ITSELF. The first build of it CLASSIFIED such a link and called that a
// narrowing: `case one.imports[parts[0]]: elsewhere += 1`, counted and printed and never asked
// anything. That left 128 of 1,523 links -- and every one of the 8 heads -- outside the property
// the gate is named for, and the shape of the hole was measured rather than argued: planting
// `[messagegroup.ThisNameIsDeclaredNowhereAtAll]`,
// `[messagegroup.GroupSession.NoSuchMethodEither]` and `[sdk.NoSuchExportedThing]` in this
// package's production prose left the gate GREEN on all three, and three links in this module's
// prose were dangling on the day it was declared closed -- [messagegroup.GroupSession.PeekSender],
// which is the GroupHandle interface's method, and `InstallPqSecret` and `DeclarePqSecretRotated`
// written as though they were package-level names when both are methods on *GroupSession. A COUNT
// IS NOT A PROPERTY, which this corpus has now been taught by its own gate.
//
// SO THE HEAD IS TURNED INTO A DIRECTORY, AND THERE ARE EXACTLY THREE ROADS. (1) This module's own
// packages -- 14 of those 128 named them at that commit, and the walk was already holding their
// declarations, so a [sdk.…] link is now held exactly like a same-package one. (2) A module go.mod
// REPLACES with a directory: `replace github.com/urnetwork/connect => ../connect` makes that source
// a requirement of this module's own build -- without it nothing here COMPILES -- so resolving the
// links into connect costs this suite no dependency it did not already have, and a rename in connect
// reports here instead of going quiet. How many there are is DERIVED and printed in the complement
// rather than written here: two prose sites used to carry the number, they disagreed (94 against
// 111) and the larger exceeded what this gate measured for every replaced module together. That is
// the measured difference from
// citationDeclaredElsewhere above, whose targets are `_test.go` files in connect AND in msgrepo --
// which the build does not need, and msgrepo is not in this module's go.mod at all -- and which
// therefore stay a table. (3) GOROOT and the module cache, which this gate does not read and
// which godocLinkOutsideThisBuild disposes of by name, held both ways.
//
// AND THE PRECEDENCE IS UNCHANGED: a link whose first element is a TYPE declared here is a member
// link and is resolved here whatever else it looks like, so [mls.ErrNoOwner] and
// [Group.RemoveMember] can never be taken for one another; a head that is neither a local type nor
// an import of the file's own package is a failure and not a shrug.
//
// AND THE TABLES ARE BUILT FROM PRODUCTION FILES ONLY, WHICH IS THIS GATE'S SECOND CORRECTION OF
// ITSELF. The first build read every `.go` file of this module into them, `_test.go` included,
// while godocDeclarationsAt excluded exactly those on the other side of the seam, so the standard
// this gate claimed to apply on both sides was applied on one, and the laxer side was this
// repository's own. A bracketed name that named nothing but a test-file declaration RESOLVED.
// Measured rather than supposed, by rebuilding the tables from production files and diffing: SEVEN
// distinct spellings resolved for that reason and no other. Six were case names, which for one
// commit were HANDED to the gate above; ruling 57 ended that and they are BACKTICKED, for the reason
// the next paragraph gives. The seventh was
// `rotWorld`, a test type, bracketed at urmessage/pqepoch.go:1147 -- two lines below
// `rotWorld.admit` written correctly in BACKTICKS, which is what this gate's own failure message
// prescribes, so the corpus contradicted itself inside one paragraph and the gate could not see
// it. That link is now in backticks, and the pair of controls holding the class compares two
// UNEXPORTED types, one from a test file and one from a production file, so a no cannot be
// explained by a case rule.
//
// LEDGER RULING 57: ONE NAMESPACE, ONE RULE -- AND A BRACKET MUST NAME A DECLARATION THE DOCUMENTED
// PACKAGE CONTAINS. The repair above left this gate enforcing two rules under one spelling: *names a
// declaration in the documented package* for every link, except *names a declaration somewhere* for
// six `Test...` names it routed to the test-citation gate. A godoc bracket is a RENDERING contract
// -- `[X]` means "link to X" -- and a reader of the rendered doc meets a bracketed name that is not
// a link, which is worse than backticks. The routing bought nothing besides: the weaker property is
// already held by that gate, whose net reads an UNBRACKETED name perfectly well. So the six links
// are backticked, the arm is deleted, and a bracketed case name now reports here like any other
// spelling the package does not declare. Measured, with the control in the same query and its
// literals taken from the source: `go doc -all -u ./urmessage` prints 290 `func` and 42 `type`
// declarations at column zero, 99 of the funcs unexported and one of the types the `ladderKey` the
// control below names -- and ZERO of either whose name begins `Test`. That is the same fact that
// convicted `[rotWorld]`.
//
// AND THE CONTROLS ARE INLINE, WITH EVERY LITERAL TAKEN FROM THE SOURCE IT IS ABOUT.
// `Group.RemoveMember` is a member link this package declares and must resolve, or the resolver is
// blind to the whole class; `Group.RemoveDevice` is the exact spelling item 259 found dangling and
// must resolve to NOTHING; `StreamStore.SeedStreamIndex` is this module's ROOT package's and must
// resolve, against `NoSuchExportedThing` which must not; connect's `GroupSession.InstallPqSecret`
// and `GroupHandle.PeekSender` must resolve while `GroupSession.PeekSender` must not, which is the
// pair this pass's finding turned on; and a walk that saw no production file, or no test file, or
// that resolved nothing on either of the two new roads, would satisfy every count below with zero.

// godocLinkQuoted is every bracketed spelling URmessage's production comments carry ONLY inside a
// quoted or backticked span ([urmessageOwns] is the scope). A quotation is quoted text and not prose
// naming a declaration, so the net blanks quoted spans before it reads a comment, and this table is
// that narrowing written down. Five entries for log tags and a wire sketch in upstream's files
// (device_rpc.go, device_local.go and two more) went when the rule's scope became URmessage's own
// files: the rot check below named each as excusing nothing, which is what it is for.
//
// HELD BOTH WAYS, like citationDeclaredElsewhere. An entry that no quoted-only span carries any
// more is a carve-out excusing nothing, which is how a disposition rots, and it is deleted. A
// spelling that appears ONLY inside quotes and has no entry is a FAILURE, because the next one may
// be a doc link somebody buried in a string rather than another log tag.
var godocLinkQuoted = map[string]string{
	"messagegroup.GroupHandle.Commit": "urmessage/group.go -- a REAL link, written inside a " +
		"quotation of the sentence it is quoting; it is a sibling package's name either way",
}

// godocLinkOutsideThisBuild is every link whose head names a package this build takes from GOROOT
// or from the module cache rather than from a directory on this disk, with what it is.
//
// WHY THESE AND NOTHING ELSE NEEDS A ROW. Everything this module's go.mod REPLACES with a
// directory is resolved for real -- this module does not compile without those directories, so
// reading them costs nothing -- and so is every package of this module itself. What is left is the
// standard library and the module cache, whose layout is a Go installation's business: GOROOT may
// hold no sources at all on a stripped image, and asserting against a version-pinned cache entry
// would make this suite red on a dependency bump rather than on a defect. So those get a row, and
// the row is the record that somebody RESOLVED the name once.
//
// HELD BOTH WAYS, like godocLinkQuoted and citationDeclaredElsewhere. A spelling whose head this
// build cannot reach and which has NO row is a failure, because the next one may be an invented
// name; and a row no production comment carries any more is a failure, because that is the shape a
// disposition rots into. What it cannot do is notice that `errors.Is` was renamed in a future Go;
// that residual is stated here, and the reading that closes it is this module failing to build.
var godocLinkOutsideThisBuild = map[string]string{
	"errors.Is":                  "GOROOT/src/errors -- the standard library's sentinel comparison",
	"sha256.Size":                "GOROOT/src/crypto/sha256 -- the digest length, 32",
	"subtle.ConstantTimeCompare": "GOROOT/src/crypto/subtle -- the constant-time equality",
}

// godocPackage is one package directory's declarations as the AST reports them, which is what an
// unqualified link in that directory's prose has to resolve against.
type godocPackage struct {
	// top is every package-level name: funcs, types, consts and vars.
	top map[string]bool
	// types is the subset of top that names a TYPE, which is what makes `A.B` a member link
	// rather than another package's name.
	types map[string]bool
	// members is each declared type's methods and, for a struct or an interface, its fields or
	// its method set.
	members map[string]map[string]bool
	// imports is every package name a file of this directory imports, under its alias when it
	// has one, mapped to the IMPORT PATH that name stands for. It is the whole of what a
	// qualified link may name, and the PATH is what turns such a link's head into a directory
	// this gate can resolve the rest of the link in.
	imports map[string]string
}

func newGodocPackage() *godocPackage {
	return &godocPackage{top: map[string]bool{}, types: map[string]bool{},
		members: map[string]map[string]bool{}, imports: map[string]string{}}
}

func (self *godocPackage) member(typeName string, name string) {
	if self.members[typeName] == nil {
		self.members[typeName] = map[string]bool{}
	}
	self.members[typeName][name] = true
}

// godocTypeName is the type a receiver or an embedded field names, with the pointer and any type
// parameters taken off, or "" for a shape a link could not name anyway.
func godocTypeName(expr ast.Expr) string {
	switch typed := expr.(type) {
	case *ast.StarExpr:
		return godocTypeName(typed.X)
	case *ast.Ident:
		return typed.Name
	case *ast.IndexExpr:
		return godocTypeName(typed.X)
	case *ast.IndexListExpr:
		return godocTypeName(typed.X)
	}
	return ""
}

// godocReadDeclarations adds ONE file's declarations and imports to a package's tables.
//
// IT IS THE ONE READER FOR BOTH SIDES, which is the property that makes a link into
// `connect/messagegroup` answerable to the same standard as a link into this package. The walk
// below builds this module's directories with it and godocDeclarationsAt builds a replaced
// module's directory with it, so "a member of GroupSession" means the same thing on both sides of
// the seam.
//
// AND THE SENTENCE THAT USED TO FOLLOW -- *"no second, laxer reading exists for the other
// repository"* -- WAS FALSE, IN THIS DIRECTION. A reader is one function; what it is CALLED ON is
// the reading. The walk fed it every `.go` file including `_test.go`, while godocDeclarationsAt
// skipped `_test.go` and said in its own header that a link in this repository's production prose
// has no business naming one. So the laxer reading existed and it was the one answering THIS
// repository's prose: a bracketed name that named only a test-file declaration resolved. It was
// measured rather than supposed -- SEVEN distinct spellings resolved for that reason and no other,
// six of them case names the gate above already held, and the seventh `rotWorld`, bracketed at
// urmessage/pqepoch.go:1147 two lines below the same type written correctly in backticks. Both
// callers now apply the same suffix test; this file's controls assert the result on a pair of
// unexported types that differ only in which FILE declares them; and what the excluded half once
// held was first HANDED to the gate that owns it and then, under ruling 57, put in BACKTICKS --
// because a bracket is a link and a link must name a declaration the documented package contains.
func godocReadDeclarations(one *godocPackage, path string) error {
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}
	for _, spec := range file.Imports {
		importPath := strings.Trim(spec.Path.Value, `"`)
		name := importPath
		if at := strings.LastIndex(name, "/"); 0 <= at {
			name = name[at+1:]
		}
		if spec.Name != nil {
			name = spec.Name.Name
		}
		one.imports[name] = importPath
	}
	for _, decl := range file.Decls {
		switch typed := decl.(type) {
		case *ast.FuncDecl:
			if typed.Recv == nil || len(typed.Recv.List) == 0 {
				one.top[typed.Name.Name] = true
				continue
			}
			if name := godocTypeName(typed.Recv.List[0].Type); name != "" {
				one.member(name, typed.Name.Name)
			}
		case *ast.GenDecl:
			for _, spec := range typed.Specs {
				switch declared := spec.(type) {
				case *ast.TypeSpec:
					one.top[declared.Name.Name] = true
					one.types[declared.Name.Name] = true
					switch under := declared.Type.(type) {
					case *ast.StructType:
						for _, field := range under.Fields.List {
							for _, ident := range field.Names {
								one.member(declared.Name.Name, ident.Name)
							}
							if len(field.Names) == 0 {
								// an embedded field is named by its own type
								if name := godocTypeName(field.Type); name != "" {
									one.member(declared.Name.Name, name)
								}
							}
						}
					case *ast.InterfaceType:
						for _, method := range under.Methods.List {
							for _, ident := range method.Names {
								one.member(declared.Name.Name, ident.Name)
							}
						}
					}
				case *ast.ValueSpec:
					for _, ident := range declared.Names {
						one.top[ident.Name] = true
					}
				}
			}
		}
	}
	return nil
}

// godocModule is this module's own import path and every module its go.mod REPLACES with a
// directory, which together are the source tree a link's head can be resolved in.
//
// WHY go.mod AND NOT A CONSTANT. `replace github.com/urnetwork/connect => ../connect` is what
// makes connect's source a hard requirement of this module rather than a courtesy: without that
// directory this module does not COMPILE, so resolving a link into it costs this suite no
// dependency it did not already have. That is the measured difference from
// citationDeclaredElsewhere 300 lines up, whose targets are `_test.go` files in connect AND in
// msgrepo -- which the build does NOT need, and msgrepo is in this module's go.mod nowhere -- and
// which therefore stays a table. Reading the directive rather than writing the path down means a
// re-layout of the workspace moves this gate with it.
type godocModule struct {
	// path is this module's own import path, off go.mod's `module` line.
	path string
	// root is the directory that path names.
	root string
	// replaced is import-path prefix -> directory, for the filesystem replacements only. A
	// replacement by another MODULE (`=> other/mod v1.2.3`) names no directory here and is left
	// to the disposition table, like the standard library.
	replaced map[string]string
}

func godocReadModule(t *testing.T, root string) *godocModule {
	source, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("reading go.mod under %s: %v", root, err)
	}
	held := &godocModule{root: root, replaced: map[string]string{}}
	inBlock := false
	for _, raw := range strings.Split(string(source), "\n") {
		line := strings.TrimSpace(raw)
		if cut := strings.Index(line, "//"); 0 <= cut {
			line = strings.TrimSpace(line[:cut])
		}
		if rest, is := strings.CutPrefix(line, "module "); is {
			held.path = strings.TrimSpace(rest)
			continue
		}
		switch {
		case line == "replace (":
			inBlock = true
			continue
		case inBlock && line == ")":
			inBlock = false
			continue
		case strings.HasPrefix(line, "replace "):
			line = strings.TrimSpace(strings.TrimPrefix(line, "replace "))
		case inBlock:
			// a row of a replace block, already in `line`
		default:
			continue
		}
		parts := strings.Fields(line)
		arrow := -1
		for at, part := range parts {
			if part == "=>" {
				arrow = at
			}
		}
		// exactly one field on the right, and it has to look like a path rather than a version
		if arrow < 1 || arrow != len(parts)-2 {
			continue
		}
		target := parts[len(parts)-1]
		if !strings.HasPrefix(target, ".") && !filepath.IsAbs(target) {
			continue
		}
		held.replaced[parts[0]] = filepath.Clean(filepath.Join(root, filepath.FromSlash(target)))
	}
	if held.path == "" {
		t.Fatalf("no `module` line in %s", filepath.Join(root, "go.mod"))
	}
	return held
}

// dirOf is the directory an import path names inside this build's SOURCE, and whether it belongs
// to this module. It answers "" for anything the build takes from the module cache or from GOROOT,
// which is what godocLinkOutsideThisBuild disposes of.
func (self *godocModule) dirOf(importPath string) (string, bool) {
	if importPath == self.path {
		return self.root, true
	}
	if rest, is := strings.CutPrefix(importPath, self.path+"/"); is {
		return filepath.Join(self.root, filepath.FromSlash(rest)), true
	}
	for prefix, dir := range self.replaced {
		if importPath == prefix {
			return dir, false
		}
		if rest, is := strings.CutPrefix(importPath, prefix+"/"); is {
			return filepath.Join(dir, filepath.FromSlash(rest)), false
		}
	}
	return "", false
}

// godocDeclarationsAt builds one directory OUTSIDE this module from its production files, cached
// per directory because connect/mls alone is 140 files.
//
// `_test.go` IS EXCLUDED AND THAT IS THE LINE THIS GATE DRAWS, ON BOTH SIDES NOW. What the replace
// directive makes a requirement of this build is the other module's PRODUCTION source; its test
// files are its own business and a link in this repository's production prose has no business
// naming one. So a link to a sibling's test helper reports here, with the same sentence as any
// other name that does not resolve. For one commit that rule was drawn HERE and not in the walk
// over this module, which is the defect godocReadDeclarations' header records: the same sentence
// was true of connect and false of this repository, and it is the near side a reader trusts most.
func godocDeclarationsAt(t *testing.T, cache map[string]*godocPackage, dir string) *godocPackage {
	if held, found := cache[dir]; found {
		return held
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("a link resolves into %s and this build cannot read it: %v. That directory is a "+
			"replace target of this module's go.mod, so its absence is a broken checkout rather "+
			"than a stale link", dir, err)
	}
	one := newGodocPackage()
	files := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if err := godocReadDeclarations(one, filepath.Join(dir, name)); err != nil {
			t.Fatalf("%v", err)
		}
		files += 1
	}
	if files == 0 {
		t.Fatalf("CONTROL FAILED: %s holds no production .go file, so every link resolving into it "+
			"would be answered by an empty table", dir)
	}
	cache[dir] = one
	return one
}

// godocHostsOf is every type of one package that declares a member by this name, in order. It is
// the whole of the failure message for the shape this pass found three of: a METHOD written as
// though it were a package-level name, which is what `[messagegroup.InstallPqSecret]` was.
func godocHostsOf(one *godocPackage, member string) string {
	hosts := []string{}
	for typeName, members := range one.members {
		if members[member] {
			hosts = append(hosts, typeName)
		}
	}
	if len(hosts) == 0 {
		return ""
	}
	sort.Strings(hosts)
	return fmt.Sprintf(" That package declares %s as a member of %s, so the link needs the type in "+
		"it -- READ which one before writing it in.", member, strings.Join(hosts, ", "))
}

// godocLinkNet finds a doc link in one comment: a bracketed identifier path of one to three
// elements, with the opening bracket NOT glued to an identifier, a `]` or a `)`.
//
// THE LEFT GUARD IS WHAT KEEPS AN INDEX OUT OF THE LINK NAMESPACE, and it is measured rather than
// assumed: without it `storage_root[n]`, `map[int]bool` and `self.peerHeads[ladder]` all read as
// links, and each answers a name nothing declares. There is no right-hand guard, deliberately:
// [PeerHead]s and [GapReason]s are ordinary links with a plural on them, and any character rule
// that removed the log tags would remove those two as well. What removes the log tags is the
// quoted-span blanking above, which is about where the text is rather than about what follows it.
var godocLinkNet = regexp.MustCompile(`(?:^|[^A-Za-z0-9_\]\)])\[([A-Za-z_][A-Za-z0-9_.]*)\]`)

// godocQuotedSpan is a double-quoted or backticked run inside a comment, blanked before the net
// reads it. See godocLinkQuoted.
var godocQuotedSpan = regexp.MustCompile("\"[^\"]*\"|`[^`]*`")

func godocBlankQuoted(line string) string {
	return godocQuotedSpan.ReplaceAllStringFunc(line, func(span string) string {
		return strings.Repeat(" ", len(span))
	})
}

// urmessageOwns is the SUBJECT of the doc-link rule below: the production files URmessage wrote.
//
// IT IS NARROWER THAN THE REPOSITORY SINCE THIS BRANCH MERGED UPSTREAM sdk (msgrepo ledger 277), and
// the reason is the rule's subject rather than its convenience. Upstream writes brackets as plain
// prose -- `[contract]`, `[multi]` in device_local.go and sdk.go -- and this house rule is ours to
// keep, not one to impose on files we did not write. DECLARATIONS are still read from every
// production file, so a link from URmessage's prose into upstream's code resolves exactly as it
// did; only the prose that is CHECKED is ours. The test prints both counts, so what the narrowing
// removed is on the page beside what it kept.
func urmessageOwns(rel string) bool {
	for _, dir := range []string{"urmessage/", "cp3b/", "livepeer/", "liveprobe/"} {
		if strings.HasPrefix(rel, dir) {
			return true
		}
	}
	base := rel[strings.LastIndex(rel, "/")+1:]
	return strings.HasPrefix(base, "message") || strings.Contains(base, "_message")
}

func TestEveryGodocLinkInThisRepositorysProductionProseNamesADeclaration(t *testing.T) {
	root := moduleRoot(t)
	packages := map[string]*godocPackage{}
	productionOf := map[string][]string{}
	total, production := 0, 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			if name := info.Name(); name == ".git" || name == "testdata" || name == "vendor" || name == "build" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		total += 1
		// `_test.go` IS EXCLUDED HERE, WHICH IS THE SAME SUFFIX TEST godocDeclarationsAt APPLIES,
		// and until this line existed the two halves of the seam disagreed. See
		// godocReadDeclarations' header for what the disagreement admitted.
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		production += 1
		dir := filepath.Dir(path)
		if packages[dir] == nil {
			packages[dir] = newGodocPackage()
		}
		if readErr := godocReadDeclarations(packages[dir], path); readErr != nil {
			return readErr
		}
		productionOf[dir] = append(productionOf[dir], path)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	// ── THE CONTROLS, IN THE SAME QUERY AND OVER THE SAME MAPS ─────────────────────────────────
	here := packages[filepath.Join(root, "urmessage")]
	if here == nil {
		t.Fatalf("CONTROL FAILED: the walk built no package for this directory at all")
	}
	if !here.members["Group"]["RemoveMember"] {
		t.Fatalf("CONTROL FAILED: this package declares (*Group).RemoveMember and the resolver does " +
			"not see it, so every member link below is being answered by a blind instrument")
	}
	if here.members["Group"]["RemoveDevice"] {
		t.Fatalf("CONTROL FAILED: the resolver says this package declares (*Group).RemoveDevice. " +
			"Ruling 50 put that verb in its own track and nothing here declares it -- it is the exact " +
			"spelling ledger item 259 found dangling -- so a yes here means the resolver answers yes " +
			"to names that do not exist")
	}
	if total == 0 || production == 0 || total == production {
		t.Fatalf("CONTROL FAILED: the walk saw %d .go files of which %d are production; a run with "+
			"no production files or no test files is measuring nothing", total, production)
	}
	// ── AND THE TABLES HOLD NO TEST-FILE DECLARATION, WHICH IS THIS PASS'S OWN FINDING ─────────
	//
	// BOTH LITERALS ARE COPIED FROM THE SOURCE AND THE PAIR FIRES FOR ITS OWN REASON. `rotWorld` is
	// declared `type rotWorld struct` at urmessage/pqrotation_test.go:56 and nowhere else, and
	// `go doc -all -u ./urmessage` prints no declaration of it -- so the documented package does
	// not contain it and the resolver must not either. `ladderKey` is declared at
	// urmessage/group.go:688 and is just as UNEXPORTED, so a no here cannot be explained by a case
	// rule: the difference between the two is which FILE declares them, which is the whole of what
	// this pass changed.
	if here.types["rotWorld"] {
		t.Fatalf("CONTROL FAILED: the resolver says this package declares the type rotWorld. It is " +
			"declared in urmessage/pqrotation_test.go and in no production file, so a yes here means " +
			"`_test.go` declarations are back in the tables that answer PRODUCTION prose -- which is " +
			"the defect godocReadDeclarations' header records, and it let [rotWorld] read as a link " +
			"for a commit")
	}
	if !here.types["ladderKey"] {
		t.Fatalf("CONTROL FAILED: this package declares the unexported type ladderKey at " +
			"urmessage/group.go:688 and the resolver does not see it, so the no above is satisfied " +
			"by a resolver that dropped unexported names rather than test-file ones")
	}

	// ── THE CROSS-MODULE CONTROLS, OVER THE READER THAT ANSWERS FOR THE OTHER SIDE ─────────────
	//
	// EVERY LITERAL HERE IS COPIED FROM connect's OWN SOURCE, and the pair is the one this pass's
	// finding turned on: PeekSender is declared on the GroupHandle INTERFACE and on the unexported
	// engine handle, and NOT on GroupSession -- so `[messagegroup.GroupSession.PeekSender]` has to
	// resolve to nothing while `[messagegroup.GroupHandle.PeekSender]` resolves. A build that
	// answered yes to the first is the build that let three links dangle for a commit.
	module := godocReadModule(t, root)
	external := map[string]*godocPackage{}
	if _, replaced := module.replaced["github.com/urnetwork/connect"]; !replaced {
		t.Fatalf("CONTROL FAILED: go.mod carries no directory replacement for "+
			"github.com/urnetwork/connect, so the resolver below has nowhere to resolve the links "+
			"this module's prose writes into it -- the complement prints how many. Read %s",
			filepath.Join(root, "go.mod"))
	}
	messagegroupDir, ownsIt := module.dirOf("github.com/urnetwork/connect/messagegroup")
	if ownsIt {
		t.Fatalf("CONTROL FAILED: the resolver thinks connect/messagegroup is a directory of THIS " +
			"module, so the two sides of the seam are not being told apart")
	}
	if messagegroupDir == "" {
		t.Fatalf("CONTROL FAILED: the resolver turns github.com/urnetwork/connect/messagegroup into " +
			"no directory although go.mod replaces that module with one. Every link into it would " +
			"then fall through to the out-of-build disposition, where a row excuses it and nothing " +
			"reads connect at all")
	}
	seam := godocDeclarationsAt(t, external, messagegroupDir)
	if !seam.members["GroupSession"]["InstallPqSecret"] {
		t.Fatalf("CONTROL FAILED: connect/messagegroup declares (*GroupSession).InstallPqSecret and "+
			"the reader of %s does not see it, so every link into that package below is being "+
			"answered by a blind instrument", messagegroupDir)
	}
	if !seam.members["GroupHandle"]["PeekSender"] {
		t.Fatalf("CONTROL FAILED: connect/messagegroup's GroupHandle interface declares PeekSender " +
			"and the reader does not see it, so the repaired spelling is passing for the wrong reason")
	}
	if seam.members["GroupSession"]["PeekSender"] {
		t.Fatalf("CONTROL FAILED: the reader says connect/messagegroup declares " +
			"(*GroupSession).PeekSender. That is the exact spelling this pass found dangling -- the " +
			"method is the GroupHandle interface's -- so a yes here means the resolver answers yes " +
			"to names that do not exist. If connect has since DELEGATED it onto the session, read " +
			"that delegation and then delete this control rather than the link")
	}
	// AND THIS MODULE'S OWN ROOT PACKAGE IS REACHABLE UNDER ITS IMPORT NAME, which is the 14 links
	// the previous build counted as "another repository's to declare" while holding their
	// declarations in this very map.
	rootDir, ownsRoot := module.dirOf(module.path)
	if !ownsRoot || packages[rootDir] == nil {
		t.Fatalf("CONTROL FAILED: the walk built no package for this module's own root %s", rootDir)
	}
	if !packages[rootDir].types["StreamStore"] || !packages[rootDir].members["StreamStore"]["SeedStreamIndex"] {
		t.Fatalf("CONTROL FAILED: this module's root package declares StreamStore and its " +
			"SeedStreamIndex method, and the resolver does not see them, so every [sdk.…] link " +
			"below would pass by being unresolvable rather than by resolving")
	}
	if packages[rootDir].top["NoSuchExportedThing"] {
		t.Fatalf("CONTROL FAILED: the resolver says this module's root package declares " +
			"NoSuchExportedThing, so it answers yes to names that do not exist")
	}

	// ── RULING 57's OWN CONTROL: A CASE NAME IS NOT IN THE DOCUMENTED PACKAGE ──────────────────
	//
	// THIS IS WHAT REPLACED THE HAND-OFF ARM, AND IT IS THE FACT THAT CONVICTED IT. A test function
	// is not part of the documented package -- `go doc -all -u ./urmessage` prints 290 funcs at
	// column zero and not one whose name begins `Test` -- so a bracketed case name names nothing
	// this package contains and now reports through the plain rule below like any other such name.
	// The pair fires for its own reason and both literals are copied from the source:
	// TestThreeMembersRotate... is declared `func Test...(` at urmessage/pqrotation_test.go:720 and
	// in no production file, while `isEpochWrapRecord` is an unexported package-level FUNC declared
	// at urmessage/group.go:2959, so a no on the first cannot be explained by a resolver that
	// stopped seeing unexported top-level names.
	const declaredOnlyInATestFile = "TestThreeMembersRotateAcrossTwoEpochsAndAMemberRemovedByThatCommitCannotFollow"
	if here.top[declaredOnlyInATestFile] {
		t.Fatalf("CONTROL FAILED: the resolver says this package declares %s. It is declared in "+
			"urmessage/pqrotation_test.go and in no production file, and `go doc -all -u ./urmessage` "+
			"documents no Test... declaration at all -- so a yes here means a bracketed case name "+
			"would RESOLVE, which is the reading ledger ruling 57 deleted", declaredOnlyInATestFile)
	}
	if !here.top["isEpochWrapRecord"] {
		t.Fatalf("CONTROL FAILED: this package declares the unexported package-level func " +
			"isEpochWrapRecord at urmessage/group.go:2959 and the resolver does not see it, so the " +
			"no above is satisfied by a resolver that dropped unexported top-level names rather " +
			"than test-file ones")
	}

	// ── THE PROPERTY ───────────────────────────────────────────────────────────────────────────
	quotedOnly := map[string][]string{}
	unquoted := map[string]bool{}
	links, resolvedTop, resolvedMember := 0, 0, 0
	resolvedPackage, resolvedOwnModule, resolvedReplaced, outsideThisBuild := 0, 0, 0, 0
	resolvedIntoModule := map[string]int{}
	needsRow := map[string][]string{}
	tookOutside := map[string]bool{}
	dirs := []string{}
	for dir := range productionOf {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	checkedFiles, upstreamFiles := 0, 0
	defer func() {
		t.Logf("the doc-link rule read %d production file(s) of URmessage's and skipped %d of upstream's "+
			"(declarations were read from all of them)", checkedFiles, upstreamFiles)
	}()
	for _, dir := range dirs {
		one := packages[dir]
		for _, path := range productionOf[dir] {
			rel := filepath.ToSlash(func() string {
				at, _ := filepath.Rel(root, path)
				return at
			}())
			if !urmessageOwns(rel) {
				upstreamFiles += 1
				continue
			}
			checkedFiles += 1
			source, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatalf("reading %s: %v", path, readErr)
			}
			for at, line := range strings.Split(string(source), "\n") {
				cut := strings.Index(line, "//")
				if cut < 0 {
					continue
				}
				comment := line[cut:]
				blanked := godocBlankQuoted(comment)
				inProse := map[string]bool{}
				for _, found := range godocLinkNet.FindAllStringSubmatch(blanked, -1) {
					inProse[found[1]] = true
					unquoted[found[1]] = true
				}
				for _, found := range godocLinkNet.FindAllStringSubmatch(comment, -1) {
					if !inProse[found[1]] {
						quotedOnly[found[1]] = append(quotedOnly[found[1]],
							fmt.Sprintf("%s:%d", rel, at+1))
					}
				}
				for name := range inProse {
					parts := strings.Split(name, ".")
					empty := false
					for _, part := range parts {
						if part == "" {
							empty = true
						}
					}
					if empty || 3 < len(parts) {
						// not an identifier path at all; the net cannot have meant it
						continue
					}
					links += 1
					where := fmt.Sprintf("%s:%d", rel, at+1)
					switch {
					case len(parts) == 1 && one.top[parts[0]]:
						resolvedTop += 1
					case len(parts) == 1 && one.imports[parts[0]] != "":
						// a bare package name names a package and nothing inside one; there is
						// no member to resolve and the import is the whole of the question.
						resolvedPackage += 1
					case len(parts) == 1:
						t.Errorf("[%s] is written at %s and this package declares nothing by that "+
							"name and imports no such package. A bracketed name that resolves to "+
							"nothing reads as a pointer and points nowhere: qualify it "+
							"([Type.Field] for a field of this package's own struct), name the "+
							"package that does declare it, or -- if no link can name it -- put it "+
							"in BACKTICKS. A local closure and a sibling package's unexported "+
							"method are two such names; a TEST FUNCTION or a test helper is a "+
							"third, and that is ledger ruling 57 -- `go doc -all -u` documents no "+
							"declaration of one, so the bracket renders as text rather than as a "+
							"link however well some other gate holds the name.", name, where)
					case one.types[parts[0]] && len(parts) == 2 && one.members[parts[0]][parts[1]]:
						resolvedMember += 1
					case one.types[parts[0]]:
						t.Errorf("[%s] is written at %s: %s is a type this package declares and it "+
							"has no member %s. READ the target before rewriting this -- a renamed "+
							"method and a field path godoc cannot link are the two shapes ledger "+
							"item 259's sweep found, and naming the wrong member is worse than "+
							"naming none.%s", name, where, parts[0], strings.Join(parts[1:], "."),
							godocNearestMember(one, parts[0], parts[1]))
					case one.imports[parts[0]] == "":
						t.Errorf("[%s] is written at %s and %s is neither a type this package "+
							"declares nor a package this file's own package imports, so there is "+
							"nothing for the rest of the link to hang off.", name, where, parts[0])
					default:
						// ── THE HEAD NAMES AN IMPORTED PACKAGE, SO THE REST IS RESOLVED IN IT ──
						//
						// THIS ARM USED TO BE `elsewhere += 1` AND THAT IS THE DEFECT THIS PASS
						// CLOSED. Classifying a link is not resolving it: 128 of 1,523 links took
						// this road and NOTHING was asked of any of them, so
						// `[messagegroup.InstallPqSecret]` (a METHOD),
						// `[messagegroup.GroupSession.PeekSender]` (the wrong TYPE) and any
						// `[sdk.InventedName]` passed in silence -- 14 of the 128 naming packages
						// whose declarations this very walk was already holding.
						dir, own := module.dirOf(one.imports[parts[0]])
						if dir == "" {
							// GOROOT or the module cache: the disposition table's population
							if _, disposed := godocLinkOutsideThisBuild[name]; !disposed {
								needsRow[name] = append(needsRow[name], where)
								break
							}
							outsideThisBuild += 1
							tookOutside[name] = true
							break
						}
						that := packages[dir]
						if that == nil {
							that = godocDeclarationsAt(t, external, dir)
						}
						resolved := false
						switch len(parts) {
						case 2:
							resolved = that.top[parts[1]]
						case 3:
							resolved = that.types[parts[1]] && that.members[parts[1]][parts[2]]
						}
						if !resolved {
							hint := godocHostsOf(that, parts[len(parts)-1])
							if len(parts) == 3 && !that.types[parts[1]] {
								hint = fmt.Sprintf(" %s is not a type that package declares.%s",
									parts[1], hint)
							}
							t.Errorf("[%s] is written at %s and package %s (%s) declares no such "+
								"%s. READ the target before rewriting this: a method written as "+
								"though it were a package-level name, and a member hung off the "+
								"wrong type of the same package, are the two shapes this gate's "+
								"own repair pass found.%s", name, where, parts[0],
								one.imports[parts[0]],
								map[bool]string{true: "package-level name", false: "member"}[len(parts) == 2],
								hint)
							break
						}
						if own {
							resolvedOwnModule += 1
							break
						}
						resolvedReplaced += 1
						// AND WHICH REPLACED MODULE ABSORBED IT IS TALLIED RATHER THAN WRITTEN
						// DOWN. Two prose sites used to carry a HARDCODED count of the links this
						// module writes into connect -- 94 in one and 111 in the other, which
						// contradicted each other and the larger of which exceeded this gate's own
						// measurement for EVERY replaced module together. A count nobody derives
						// rots the moment a link is added; this one is printed in the complement.
						for prefix := range module.replaced {
							importPath := one.imports[parts[0]]
							if importPath == prefix || strings.HasPrefix(importPath, prefix+"/") {
								resolvedIntoModule[prefix] += 1
							}
						}
					}
				}
			}
		}
	}

	// ── THE QUOTED-SPAN NARROWING, HELD BOTH WAYS ──────────────────────────────────────────────
	carved := []string{}
	for name, at := range quotedOnly {
		if unquoted[name] {
			// it is gated in prose somewhere else in the module; the quoted copy adds nothing
			continue
		}
		carved = append(carved, name)
		if _, disposed := godocLinkQuoted[name]; !disposed {
			t.Errorf("[%s] appears in this module's production comments ONLY inside a quoted or "+
				"backticked span (%s), and the net therefore never reads it. That is right for a log "+
				"tag and wrong for a doc link somebody buried in a string: read it, then either take "+
				"it out of the quotes so this gate can hold it, or add a row to godocLinkQuoted "+
				"saying why it is not prose.", name, strings.Join(at, ", "))
		}
	}
	sort.Strings(carved)
	for name, why := range godocLinkQuoted {
		if len(quotedOnly[name]) == 0 || unquoted[name] {
			t.Errorf("[%s] is carved out of the link net as quoted text (%s) and no production "+
				"comment carries it only inside quotes any more. An entry nothing needs is how a "+
				"disposition rots; delete it", name, why)
		}
	}

	// ── THE OUT-OF-BUILD NARROWING, HELD BOTH WAYS ─────────────────────────────────────────────
	rowless := []string{}
	for name := range needsRow {
		rowless = append(rowless, name)
	}
	sort.Strings(rowless)
	for _, name := range rowless {
		t.Errorf("[%s] is written at %s and its head names a package this build takes from GOROOT "+
			"or from the module cache, neither of which this gate reads. RESOLVE it by hand and add "+
			"a row to godocLinkOutsideThisBuild saying what it is -- the row is the record that "+
			"somebody looked, which is the whole of what this disposition buys.",
			name, strings.Join(needsRow[name], ", "))
	}
	for name, why := range godocLinkOutsideThisBuild {
		if !tookOutside[name] {
			t.Errorf("[%s] is carved out as living outside this build's source (%s) and no "+
				"production comment writes it any more. An entry nothing needs is how a disposition "+
				"rots; delete it", name, why)
		}
	}

	// ── THE COMPLEMENT, PRINTED BESIDE WHAT WAS ASSERTED ───────────────────────────────────────
	t.Logf("%d .go files walked, %d of them production; %d package directories built FROM "+
		"PRODUCTION FILES ONLY", total, production, len(packages))
	t.Logf("%d godoc links in production prose: %d to a package-level declaration of their own "+
		"package, %d to a member of a type it declares, %d to a declaration of another package OF "+
		"THIS MODULE, %d to a declaration of a module go.mod replaces with a directory, %d a bare "+
		"package name, %d disposed of as outside this build's source", links, resolvedTop,
		resolvedMember, resolvedOwnModule, resolvedReplaced, resolvedPackage, outsideThisBuild)
	t.Logf("the quoted-span narrowing removed %d spelling(s) that appear nowhere in prose: %v",
		len(carved), carved)
	t.Logf("%d replaced module(s) read for real: %v; %d external package directory(ies) built",
		len(module.replaced), module.replaced, len(external))
	// AND THE PER-MODULE TALLY, WHICH IS WHERE A PROSE SITE USED TO WRITE A NUMBER DOWN. It is the
	// breakdown of the replaced road above, so its own sum is asserted against that road below.
	intoModules := []string{}
	for prefix := range resolvedIntoModule {
		intoModules = append(intoModules, prefix)
	}
	sort.Strings(intoModules)
	intoTotal := 0
	for _, prefix := range intoModules {
		t.Logf("  %d of those resolve into %s", resolvedIntoModule[prefix], prefix)
		intoTotal += resolvedIntoModule[prefix]
	}
	if intoTotal != resolvedReplaced {
		t.Errorf("the per-module tally sums to %d and the replaced road counted %d: a link resolved "+
			"into a replaced directory whose module prefix this tally does not name, so the "+
			"breakdown printed above is not the breakdown of that road", intoTotal, resolvedReplaced)
	}
	if resolvedMember == 0 || resolvedTop == 0 {
		t.Errorf("no link resolved to a %s declaration at all, so the rule above is vacuous",
			map[bool]string{true: "package-level", false: "member"}[resolvedTop == 0])
	}
	// AND THE TWO NEW ROADS ARE ASSERTED NON-EMPTY, or a resolver that fell back to counting would
	// read as a pass. The own-module road is the one ledger item 259's sweep missed and the replaced
	// road is the one into connect; both are facts about this corpus today, so a zero is a broken
	// instrument and not a clean repository. Neither is written down as a number: the complement
	// above prints both, because the two numbers that WERE written down went stale and disagreed.
	if resolvedOwnModule == 0 || resolvedReplaced == 0 {
		t.Errorf("resolvedOwnModule %d and resolvedReplaced %d: this module's prose links into its "+
			"own root package and into connect/messagegroup, so a zero on either road means that "+
			"road resolved nothing and the arm above is classifying again rather than resolving",
			resolvedOwnModule, resolvedReplaced)
	}
	// AND THE SUM IS ASSERTED, so no road can be added that counts nothing and no link can be
	// counted twice. It is also what catches a road being DELETED without its links going with it:
	// ledger ruling 57 deleted the hand-off road out of this very switch, and this line is what
	// says the six links it used to dispose of are gone from the corpus rather than uncounted. A
	// link is now either resolved on one of the FIVE roads, disposed of by a row, or already
	// reported above -- and `links` is the only number the loop increments unconditionally.
	disposed := resolvedTop + resolvedMember + resolvedOwnModule + resolvedReplaced +
		resolvedPackage + outsideThisBuild
	if !t.Failed() && disposed != links {
		t.Errorf("%d links were counted and %d were disposed of: the difference is a link that took "+
			"a road this complement does not print, which is how a count stops being a property",
			links, disposed)
	}
}

// godocNearestMember is the member of one type that shares the longest prefix with a dangling one,
// as a suffix for the failure above. It is the same instrument as citationNearest and it is a
// REPORT rather than a suggestion, for the same reason: naming the wrong member is worse than
// naming none, so what follows the colon has to be read before it is written in.
func godocNearestMember(one *godocPackage, typeName string, member string) string {
	best, longest := "", 0
	for candidate := range one.members[typeName] {
		shared := 0
		for shared < len(member) && shared < len(candidate) && member[shared] == candidate[shared] {
			shared += 1
		}
		if longest < shared {
			best, longest = candidate, shared
		}
	}
	if best == "" || longest < 6 {
		return ""
	}
	return fmt.Sprintf(" The member of %s sharing the longest prefix (%d characters) is %s -- READ "+
		"WHAT IT DOES before writing it in.", typeName, longest, best)
}
