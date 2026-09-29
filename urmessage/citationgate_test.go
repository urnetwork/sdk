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

// citationDeclaredElsewhere is every cited name that lives in a SIBLING repository rather than in
// this one, with where it lives and why this file cannot simply look.
//
// WHY A TABLE AND NOT A WALK OF ../connect. connect's own cross-repo gate reads ../../sdk, so the
// precedent for reaching across exists -- and the cost is what decides against it here: a walk of
// a sibling checkout makes THIS repository's suite depend on that checkout being present and at a
// compatible commit, so a developer with only `sdk` cloned gets a red suite about somebody else's
// file layout. A table costs one line per citation, needs no second checkout, and -- because it is
// held both ways below -- reports a connect rename as loudly as a walk would. What it cannot do is
// notice that the named test has been DELETED in connect while its name stays in this table; that
// residual is stated here rather than left to be found, and the reading that closes it is connect's
// own suite going red on the deletion.
var citationDeclaredElsewhere = map[string]string{
	"TestABodyNoRungCouldHoldCostsNeitherAnIndexNorAGeneration": "connect/messagegroup/mlsframe_test.go -- " +
		"the frame-size ladder is connect's and the cost it prices is read from this side",
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

func TestEveryTestNameCitedInThisRepositorysProductionProseResolvesToOneDeclaration(t *testing.T) {
	root := moduleRoot(t)
	// a Go test function's declaration, and the citation net that has to find the same spelling.
	declaration := regexp.MustCompile(`(?m)^func\s+(Test[A-Z][A-Za-z0-9_]*)\s*\(`)
	citation := regexp.MustCompile(`\bTest[A-Z][A-Za-z0-9_]*`)

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
		if _, carved := citationIsNotACase[name]; carved {
			exempt += 1
			continue
		}
		if _, carved := citationDeclaredElsewhere[name]; carved {
			elsewhere += 1
			continue
		}
		found := declared[name]
		if len(found) == 1 {
			here += 1
			continue
		}
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
// THE NARROWING IS EVERY OTHER PACKAGE'S NAMES, AND IT IS ASSERTED RATHER THAN PRINTED. A link
// whose first element is an import of its own file's package is that repository's to declare --
// counted, printed, and not followed, for citationDeclaredElsewhere's reason one paragraph up. A
// link whose first element is a TYPE declared here is a member link and is resolved here whatever
// else it looks like, so [mls.ErrNoOwner] and [Group.RemoveMember] can never be taken for one
// another. A two-part link whose first element is NEITHER is a failure and not a shrug.
//
// AND THE CONTROLS ARE INLINE, WITH EVERY LITERAL TAKEN FROM THE SOURCE. `Group.RemoveMember` is a
// member link this package declares and must resolve, or the resolver is blind to the whole class;
// `Group.RemoveDevice` is the exact spelling item 259 found dangling and must resolve to NOTHING,
// or the resolver is answering yes to names that do not exist; and a walk that saw no production
// file, or no test file, would satisfy every count below with zero.

// godocLinkQuoted is every bracketed spelling this module's production comments carry ONLY inside a
// quoted or backticked span. A quotation is quoted text and not prose naming a declaration: five of
// the six below are log tags inside a format string in commented-out code, and the sixth is a real
// link written inside a quotation of another sentence. So the net blanks quoted spans before it
// reads a comment, and this table is that narrowing written down.
//
// HELD BOTH WAYS, like citationDeclaredElsewhere. An entry that no quoted-only span carries any
// more is a carve-out excusing nothing, which is how a disposition rots, and it is deleted. A
// spelling that appears ONLY inside quotes and has no entry is a FAILURE, because the next one may
// be a doc link somebody buried in a string rather than another log tag.
var godocLinkQuoted = map[string]string{
	"dlrpc": "device_rpc.go -- a log tag inside a commented-out Infof format string",
	"dr":    "device_rpc.go -- the same log tag, at two commented-out sites",
	"io":    "device_local_ioloop.go -- the same",
	"trace": "device_local.go -- the same",
	"streamTag": "device_rpc_transport.go -- a wire-format sketch, `[streamTag][payload...]`, " +
		"where the brackets are the format and not a link",
	"messagegroup.GroupHandle.Commit": "urmessage/group.go -- a REAL link, written inside a " +
		"quotation of the sentence it is quoting; it is a sibling package's name either way",
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
	// has one. It is the whole of what a qualified link may name.
	imports map[string]bool
}

func newGodocPackage() *godocPackage {
	return &godocPackage{top: map[string]bool{}, types: map[string]bool{},
		members: map[string]map[string]bool{}, imports: map[string]bool{}}
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
		dir := filepath.Dir(path)
		if packages[dir] == nil {
			packages[dir] = newGodocPackage()
		}
		one := packages[dir]
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return fmt.Errorf("parsing %s: %w", path, parseErr)
		}
		for _, spec := range file.Imports {
			name := strings.Trim(spec.Path.Value, `"`)
			if at := strings.LastIndex(name, "/"); 0 <= at {
				name = name[at+1:]
			}
			if spec.Name != nil {
				name = spec.Name.Name
			}
			one.imports[name] = true
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
		if !strings.HasSuffix(path, "_test.go") {
			production += 1
			productionOf[dir] = append(productionOf[dir], path)
		}
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

	// ── THE PROPERTY ───────────────────────────────────────────────────────────────────────────
	quotedOnly := map[string][]string{}
	unquoted := map[string]bool{}
	links, resolvedTop, resolvedMember, elsewhere := 0, 0, 0, 0
	dirs := []string{}
	for dir := range productionOf {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		one := packages[dir]
		for _, path := range productionOf[dir] {
			source, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatalf("reading %s: %v", path, readErr)
			}
			rel := filepath.ToSlash(func() string {
				at, _ := filepath.Rel(root, path)
				return at
			}())
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
					case len(parts) == 1 && one.imports[parts[0]]:
						elsewhere += 1
					case len(parts) == 1:
						t.Errorf("[%s] is written at %s and this package declares nothing by that "+
							"name and imports no such package. A bracketed name that resolves to "+
							"nothing reads as a pointer and points nowhere: qualify it "+
							"([Type.Field] for a field of this package's own struct), name the "+
							"package that does declare it, or -- if no link can name it, which is "+
							"true of a local closure and of a sibling package's unexported method "+
							"-- put it in backticks.", name, where)
					case one.types[parts[0]] && len(parts) == 2 && one.members[parts[0]][parts[1]]:
						resolvedMember += 1
					case one.types[parts[0]]:
						t.Errorf("[%s] is written at %s: %s is a type this package declares and it "+
							"has no member %s. READ the target before rewriting this -- a renamed "+
							"method and a field path godoc cannot link are the two shapes ledger "+
							"item 259's sweep found, and naming the wrong member is worse than "+
							"naming none.%s", name, where, parts[0], strings.Join(parts[1:], "."),
							godocNearestMember(one, parts[0], parts[1]))
					case one.imports[parts[0]]:
						elsewhere += 1
					default:
						t.Errorf("[%s] is written at %s and %s is neither a type this package "+
							"declares nor a package this file's own package imports, so there is "+
							"nothing for the rest of the link to hang off.", name, where, parts[0])
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

	// ── THE COMPLEMENT, PRINTED BESIDE WHAT WAS ASSERTED ───────────────────────────────────────
	t.Logf("%d .go files walked in %d package directories, %d of the files production", total,
		len(packages), production)
	t.Logf("%d godoc links in production prose: %d resolve to a package-level declaration of their "+
		"own package, %d to a method or a field of a type it declares, %d name another package and "+
		"are that repository's to declare", links, resolvedTop, resolvedMember, elsewhere)
	t.Logf("the quoted-span narrowing removed %d spelling(s) that appear nowhere in prose: %v",
		len(carved), carved)
	if resolvedMember == 0 || resolvedTop == 0 {
		t.Errorf("no link resolved to a %s declaration at all, so the rule above is vacuous",
			map[bool]string{true: "package-level", false: "member"}[resolvedTop == 0])
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
